package cache

import (
	"sync"
	"testing"
)

func TestMemoryCloseConcurrentlyWaitsForWorker(t *testing.T) {
	cache := NewMemoryCache(0)
	var done sync.WaitGroup
	for i := 0; i < 32; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			if err := cache.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	done.Wait()
	select {
	case <-cache.done:
	default:
		t.Fatal("eviction worker still running")
	}
}
