package service

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func commandTestServer(t *testing.T, listener net.Listener, serve func(net.Conn)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		serve(conn)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("command test server did not stop")
		}
	})
}

func commandTestListener(t *testing.T) *net.TCPListener {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func TestSendCmdContextFallsBackBeforeSending(t *testing.T) {
	listener := commandTestListener(t)
	commands := make(chan string, 1)
	commandTestServer(t, listener, func(conn net.Conn) {
		buf := make([]byte, 128)
		n, _ := conn.Read(buf)
		commands <- string(buf[:n])
		_, _ = io.WriteString(conn, "command-result")
	})
	var service ServerCmdService
	res, err := service.SendCmdContext(context.Background(), listener.Addr().(*net.TCPAddr).Port, "h", "argument")
	if err != nil || res != "command-result" {
		t.Fatalf("command response: %q, %v", res, err)
	}
	if command := <-commands; command != "h argument" {
		t.Fatalf("wire command changed: %q", command)
	}
}

func TestSendCmdContextPreservesEmptySuccessResponse(t *testing.T) {
	listener := commandTestListener(t)
	commands := make(chan string, 1)
	commandTestServer(t, listener, func(conn net.Conn) {
		buffer := make([]byte, 128)
		n, _ := conn.Read(buffer)
		commands <- string(buffer[:n])
	})
	var service ServerCmdService
	response, err := service.SendCmdContext(context.Background(), listener.Addr().(*net.TCPAddr).Port, "rs", "configuration")
	if err != nil || response != "" {
		t.Fatalf("normal empty EOF must preserve server write-command success: response=%q error=%v", response, err)
	}
	if command := <-commands; command != "rs configuration" {
		t.Fatalf("wire command changed: %q", command)
	}
}

func TestSendCmdContextCancelsConnectedRead(t *testing.T) {
	listener := commandTestListener(t)
	accepted := make(chan struct{})
	commandTestServer(t, listener, func(conn net.Conn) {
		buf := make([]byte, 128)
		_, _ = conn.Read(buf)
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	var service ServerCmdService
	go func() {
		_, err := service.SendCmdContext(ctx, listener.Addr().(*net.TCPAddr).Port, "h", "")
		result <- err
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("command not received")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled connected read did not return")
	}
}

func TestSendCmdContextBoundsUnresponsiveServer(t *testing.T) {
	listener := commandTestListener(t)
	commandTestServer(t, listener, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	var service ServerCmdService
	start := time.Now()
	_, err := service.SendCmdContext(ctx, listener.Addr().(*net.TCPAddr).Port, "h", "")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("unbounded command: %v after %v", err, time.Since(start))
	}
	var timeout net.Error
	if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &timeout) || !timeout.Timeout()) {
		t.Fatalf("deadline cause lost: %v", err)
	}
}

func TestSendCmdContextDoesNotReplayAfterWrite(t *testing.T) {
	v6, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = v6.Close() })
	port := v6.Addr().(*net.TCPAddr).Port
	v4, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Skipf("independent IPv4 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = v4.Close() })
	commands := make(chan string, 1)
	commandTestServer(t, v6, func(conn net.Conn) {
		buf := make([]byte, 128)
		n, _ := conn.Read(buf)
		commands <- string(buf[:n])
		_ = conn.(*net.TCPConn).SetLinger(0)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var service ServerCmdService
	_, err = service.SendCmdContext(ctx, port, "write-command", "option")
	if err == nil {
		t.Fatal("response failure reported as success")
	}
	if command := <-commands; !strings.HasPrefix(command, "write-command ") {
		t.Fatalf("command not sent: %q", command)
	}
	_ = v4.SetDeadline(time.Now().Add(50 * time.Millisecond))
	if conn, err := v4.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("command was replayed on IPv4 after it was written on IPv6")
	}
}
