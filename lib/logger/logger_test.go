package logger

import (
	log "github.com/sirupsen/logrus"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedLoggerDoesNotReplaceGlobalAndClosesFile(t *testing.T) {
	previous := log.StandardLogger().Out
	path := filepath.Join(t.TempDir(), "owned.log")
	logger, closer, err := Open(&Config{Path: path, Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	if closer == nil || log.StandardLogger().Out != previous {
		t.Fatal("logger ownership not isolated")
	}
	logger.Info("lifecycle message")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	file := closer.(*os.File)
	if _, err := file.Write([]byte("after close")); err == nil {
		t.Fatal("log file remained open")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatal("log not written")
	}
}

func TestOpenFailureReturnsError(t *testing.T) {
	if _, _, err := Open(&Config{Path: filepath.Join(t.TempDir(), "missing", "app.log")}); err == nil {
		t.Fatal("missing log directory accepted")
	}
}
