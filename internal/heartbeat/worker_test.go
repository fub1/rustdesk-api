package heartbeat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFailedBatchRetainsLatestConcurrentObservation(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	var accepted map[uint]Entry
	w, _ := New(func(ctx context.Context, batch map[uint]Entry) error {
		calls++
		if calls == 1 {
			close(entered)
			<-release
			return errors.New("temporary failure")
		}
		accepted = batch
		return nil
	}, Options{})
	w.Buffer(1, Entry{Time: 10, IP: "old"})
	done := make(chan error, 1)
	go func() { done <- w.Flush(ctx) }()
	<-entered
	w.Buffer(1, Entry{Time: 20, IP: "latest"})
	w.Buffer(2, Entry{Time: 15, IP: "second"})
	close(release)
	if err := <-done; err == nil || w.Pending() != 2 {
		t.Fatal("failed batch lost")
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if accepted[1].Time != 20 || accepted[1].IP != "latest" || w.Pending() != 0 {
		t.Fatal("older failed observation replaced latest")
	}
}

func TestConcurrentStopWaitsWithoutLosingFinalBatch(t *testing.T) {
	flushed := 0
	w, _ := New(func(context.Context, map[uint]Entry) error { flushed++; return nil }, Options{Interval: time.Hour})
	w.Start(context.Background())
	w.Buffer(1, Entry{Time: 1})
	var callers sync.WaitGroup
	for i := 0; i < 16; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if err := w.Stop(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	callers.Wait()
	if flushed != 1 || w.Pending() != 0 {
		t.Fatal("final batch lost or repeated")
	}
}

func TestStopWaitsForWorkerAndFlushesFinalBatch(t *testing.T) {
	ctx := context.Background()
	seen := map[uint]Entry{}
	w, _ := New(func(_ context.Context, batch map[uint]Entry) error {
		for id, e := range batch {
			seen[id] = e
		}
		return nil
	}, Options{Interval: time.Hour})
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Start(ctx) == nil {
		t.Fatal("duplicate worker start")
	}
	w.Buffer(1, Entry{Time: 20})
	w.Buffer(1, Entry{Time: 10})
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if seen[1].Time != 20 || w.Pending() != 0 {
		t.Fatal("last batch not flushed")
	}
	if w.Buffer(1, Entry{Time: 30}) == nil {
		t.Fatal("stopped worker accepted new telemetry")
	}
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodicFailureRetriesAndStopCancelsInFlightWrite(t *testing.T) {
	entered := make(chan struct{})
	observed := make(chan int, 2)
	w, _ := New(func(ctx context.Context, _ map[uint]Entry) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-ctx.Done()
		return ctx.Err()
	}, Options{Interval: time.Millisecond, OperationTimeout: time.Second, Observe: func(n int) { observed <- n }})
	w.Buffer(1, Entry{Time: 1})
	w.Start(context.Background())
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := w.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want final flush deadline: %v", err)
	}
	if w.Pending() != 1 {
		t.Fatal("shutdown failure lost pending data")
	}
	select {
	case n := <-observed:
		if n != 1 {
			t.Fatal("wrong pending count")
		}
	default:
		t.Fatal("failure not observed")
	}
}
