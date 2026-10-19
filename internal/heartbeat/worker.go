// Package heartbeat batches latest observed telemetry and retains failed writes.
package heartbeat

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Entry struct {
	Time int64
	IP   string
}
type Flush func(context.Context, map[uint]Entry) error
type Options struct {
	Interval, OperationTimeout time.Duration
	Observe                    func(pending int)
}
type Worker struct {
	mu              sync.Mutex
	pending         map[uint]Entry
	flush           Flush
	options         Options
	gate            chan struct{}
	started, closed bool
	cancel          context.CancelFunc
	done            chan struct{}
}

func New(flush Flush, options Options) (*Worker, error) {
	if flush == nil || options.Interval < 0 || options.OperationTimeout < 0 {
		return nil, errors.New("invalid heartbeat dependencies")
	}
	if options.Interval == 0 {
		options.Interval = 10 * time.Second
	}
	if options.OperationTimeout == 0 {
		options.OperationTimeout = 5 * time.Second
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Worker{flush: flush, options: options, pending: map[uint]Entry{}, gate: gate}, nil
}

func (w *Worker) Buffer(id uint, entry Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("heartbeat worker stopped")
	}
	if id == 0 {
		return errors.New("invalid heartbeat peer")
	}
	old, ok := w.pending[id]
	if !ok || entry.Time >= old.Time {
		w.pending[id] = entry
	}
	return nil
}

func (w *Worker) Start(parent context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.closed {
		return errors.New("heartbeat worker already started or closed")
	}
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.done = make(chan struct{})
	w.started = true
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.options.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				work, finish := context.WithTimeout(ctx, w.options.OperationTimeout)
				_ = w.Flush(work)
				finish()
			}
		}
	}()
	return nil
}

func (w *Worker) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.gate:
	}
	defer func() { w.gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	batch := w.pending
	w.pending = map[uint]Entry{}
	w.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	err := w.flush(ctx, batch)
	if err != nil {
		w.mu.Lock()
		for id, entry := range batch {
			if newer, ok := w.pending[id]; !ok || entry.Time > newer.Time {
				w.pending[id] = entry
			}
		}
		pending := len(w.pending)
		w.mu.Unlock()
		if w.options.Observe != nil {
			w.options.Observe(pending)
		}
	}
	return err
}

func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.closed = true
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	work, finish := context.WithTimeout(ctx, w.options.OperationTimeout)
	defer finish()
	return w.Flush(work)
}

func (w *Worker) Pending() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.pending) }
