package lifecycle

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type worker struct {
	start func(context.Context) error
	stop  func(context.Context) error
}

func (w worker) Start(ctx context.Context) error { return w.start(ctx) }
func (w worker) Stop(ctx context.Context) error  { return w.stop(ctx) }

func TestListenFailureDoesNotStartWorkers(t *testing.T) {
	called := false
	err := Run(context.Background(), "127.0.0.1:invalid", http.NotFoundHandler(), []Worker{worker{start: func(context.Context) error { called = true; return nil }}}, Options{})
	if err == nil || called {
		t.Fatal("listen failure started a worker")
	}
}

func TestWorkerFailureClosesListenerAndUnwindsStartedWorkers(t *testing.T) {
	var listener net.Listener
	stopped := false
	ready := false
	first := worker{start: func(context.Context) error { return nil }, stop: func(context.Context) error { stopped = true; return nil }}
	sentinel := errors.New("worker failed")
	second := worker{start: func(context.Context) error { return sentinel }}
	err := Run(context.Background(), "127.0.0.1:0", http.NotFoundHandler(), []Worker{first, second}, Options{Listen: func(network, address string) (net.Listener, error) {
		var e error
		listener, e = net.Listen(network, address)
		return listener, e
	}, OnReady: func(net.Addr) { ready = true }})
	if !errors.Is(err, sentinel) || !stopped || ready {
		t.Fatal("worker failure did not unwind without readiness")
	}
	if conn, e := net.DialTimeout("tcp", listener.Addr().String(), time.Second); e == nil {
		conn.Close()
		t.Fatal("listener left open")
	}
}

func TestShutdownDrainsRequestBeforeStoppingWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := make(chan string, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	response := make(chan error, 1)
	finished := make(chan error, 1)
	var handled atomic.Bool
	w := worker{start: func(context.Context) error { return nil }, stop: func(context.Context) error {
		if !handled.Load() {
			return errors.New("stopped before handler drained")
		}
		return nil
	}}
	go func() {
		finished <- Run(ctx, "127.0.0.1:0", http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				t.Error("graceful stop canceled active request")
			}
			handled.Store(true)
			out.Write([]byte("complete"))
		}), []Worker{w}, Options{OnReady: func(a net.Addr) { address <- a.String() }, ShutdownTimeout: time.Second})
	}()
	endpoint := <-address
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://" + endpoint)
		if err == nil {
			data, e := io.ReadAll(resp.Body)
			resp.Body.Close()
			err = e
			if string(data) != "complete" {
				err = errors.New("incomplete response")
			}
		}
		response <- err
	}()
	<-entered
	cancel()
	select {
	case <-finished:
		t.Fatal("server returned before active request drained")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestShutdownTimeoutCancelsActiveRequestsAndReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := make(chan string, 1)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- Run(ctx, "127.0.0.1:0", http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(canceled) }), nil, Options{ShutdownTimeout: 50 * time.Millisecond, OnReady: func(a net.Addr) { address <- a.String() }})
	}()
	endpoint := <-address
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		client := &http.Client{Timeout: time.Second}
		resp, err := client.Get("http://" + endpoint)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrIncompleteShutdown) {
			t.Fatalf("want bounded deadline, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unbounded shutdown")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("active request context not canceled")
	}
	<-requestDone
}
