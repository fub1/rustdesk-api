// Package lifecycle owns a listener, a standard HTTP server and bounded workers.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

type Worker interface {
	Start(context.Context) error
	Stop(context.Context) error
}
type Options struct {
	ShutdownTimeout time.Duration
	Listen          func(string, string) (net.Listener, error)
	OnReady         func(net.Addr)
}

const DefaultShutdownTimeout = 15 * time.Second

// A deadline can close connections and cancel requests, but Go cannot forcibly
// stop an uncooperative handler/worker. The command must then exit without
// concurrently resetting dependencies that such work can still be using.
var ErrIncompleteShutdown = errors.New("shutdown did not drain all work")

func Run(ctx context.Context, addr string, handler http.Handler, workers []Worker, options Options) error {
	if ctx == nil || handler == nil || options.ShutdownTimeout < 0 {
		return errors.New("invalid server lifecycle configuration")
	}
	if options.ShutdownTimeout == 0 {
		options.ShutdownTimeout = DefaultShutdownTimeout
	}
	if options.Listen == nil {
		options.Listen = net.Listen
	}
	if ctx.Err() != nil {
		return nil
	}
	listener, err := options.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	requests, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	started := make([]Worker, 0, len(workers))
	stop := func(shutdown context.Context) error {
		cancel()
		var result error
		for i := len(started) - 1; i >= 0; i-- {
			e := started[i].Stop(shutdown)
			if errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) {
				e = errors.Join(ErrIncompleteShutdown, e)
			}
			result = errors.Join(result, e)
		}
		return result
	}
	for _, worker := range workers {
		if worker == nil {
			err = errors.New("nil lifecycle worker")
		} else {
			err = worker.Start(lifetime)
		}
		if err != nil {
			_ = listener.Close()
			shutdown, done := context.WithTimeout(context.Background(), options.ShutdownTimeout)
			defer done()
			return errors.Join(fmt.Errorf("start worker: %w", err), stop(shutdown))
		}
		started = append(started, worker)
	}
	if ctx.Err() != nil {
		_ = listener.Close()
		shutdown, done := context.WithTimeout(context.Background(), options.ShutdownTimeout)
		defer done()
		return stop(shutdown)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return requests }}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	if options.OnReady != nil {
		options.OnReady(listener.Addr())
	}
	select {
	case <-ctx.Done():
	case err = <-served:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		shutdown, done := context.WithTimeout(context.Background(), options.ShutdownTimeout)
		defer done()
		drain, finishDrain := context.WithTimeout(shutdown, options.ShutdownTimeout*2/3)
		closeErr := server.Shutdown(drain)
		finishDrain()
		if closeErr != nil {
			cancelRequests()
			closeErr = errors.Join(ErrIncompleteShutdown, closeErr, server.Close())
		}
		return errors.Join(err, closeErr, stop(shutdown))
	}
	shutdown, done := context.WithTimeout(context.Background(), options.ShutdownTimeout)
	defer done()
	drain, finishDrain := context.WithTimeout(shutdown, options.ShutdownTimeout*2/3)
	closeErr := server.Shutdown(drain)
	finishDrain()
	if closeErr != nil {
		cancelRequests()
		closeErr = errors.Join(ErrIncompleteShutdown, closeErr, server.Close())
	}
	serveErr := <-served
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(closeErr, serveErr, stop(shutdown))
}
