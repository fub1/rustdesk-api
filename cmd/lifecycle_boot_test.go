package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/global"
)

func TestLifecycleProcessHelper(t *testing.T) {
	mode := os.Getenv("RUSTDESK_LIFECYCLE_HELPER")
	if mode == "" {
		return
	}
	args := []string{"--config", os.Getenv("RUSTDESK_LIFECYCLE_CONFIG")}
	if mode == "maintenance" {
		args = append(args, "reset-admin-pwd", "lifecycle-test-password")
	}
	rootCmd.SetArgs(args)
	if mode == "maintenance" {
		if err := execute(context.Background()); err != nil {
			t.Fatal(err)
		}
		if global.DB != nil || global.Cache != nil || global.Redis != nil || global.LoginLimiter != nil {
			t.Fatal("maintenance left resources or unrelated workers")
		}
		if logFile != nil {
			if err := logFile.Close(); err != nil {
				t.Fatal(err)
			}
			logFile = nil
		}
		fmt.Println("MAINTENANCE_CLEAN")
		return
	}
	main()
}

func lifecycleConfig(t *testing.T, extra string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	resources, err := filepath.Abs("../resources")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	config := "lang: en\nlogger:\n  level: info\ngin:\n  mode: release\n  api-addr: 127.0.0.1:0\n  resources-path: " + filepath.ToSlash(resources) + "\ngorm:\n  type: sqlite\ncache:\n  type: memory\n" + extra
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestMaintenanceProcessClosesDatabaseWithoutServerWorkers(t *testing.T) {
	dir, config := lifecycleConfig(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleProcessHelper$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RUSTDESK_LIFECYCLE_HELPER=maintenance", "RUSTDESK_LIFECYCLE_CONFIG="+config)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "MAINTENANCE_CLEAN") || strings.Contains(string(output), "API SERVER START") {
		t.Fatalf("maintenance lifecycle failed: %v\n%s", err, output)
	}
}

func TestRealServerSIGTERMExitsSuccessfully(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM test is Unix; Windows runtime separately unverified")
	}
	dir, config := lifecycleConfig(t, "")
	outputPath := filepath.Join(dir, "process.log")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleProcessHelper$")
	cmd.Dir = dir
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Env = append(os.Environ(), "RUSTDESK_LIFECYCLE_HELPER=server", "RUSTDESK_LIFECYCLE_CONFIG="+config)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(outputPath)
		if strings.Contains(string(data), "API SERVER START") {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not become ready: %s", data)
		}
		select {
		case err := <-done:
			t.Fatalf("server exited before readiness: %v\n%s", err, data)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			data, _ := os.ReadFile(outputPath)
			t.Fatalf("signal shutdown failed: %v\n%s", err, data)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not stop after SIGTERM")
	}
}
