package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestMemorySet(t *testing.T) {
	mc := NewMemoryCache(0)
	t.Cleanup(mc.stopEviction)
	if err := mc.Set("key", "value", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
}

func TestMemoryGet(t *testing.T) {
	mc := NewMemoryCache(0)
	t.Cleanup(mc.stopEviction)
	if err := mc.Set("key", "value", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var got string
	if err := mc.Get("key", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "value" {
		t.Fatalf("Get = %q, want value", got)
	}
}

func TestMemorySetExpGet(t *testing.T) {
	mc := NewMemoryCache(0)
	t.Cleanup(mc.stopEviction)
	if err := mc.Set("short", "expires", 1); err != nil {
		t.Fatalf("Set short: %v", err)
	}
	if err := mc.Set("long", "retained", 30); err != nil {
		t.Fatalf("Set long: %v", err)
	}
	var got string
	if err := mc.Get("short", &got); err != nil || got != "expires" {
		t.Fatalf("Get before expiry = %q, %v", got, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := mc.Get("short", &got); err != nil || got != "" {
		t.Fatalf("Get expired value = %q, %v; want empty", got, err)
	}
	if err := mc.Get("long", &got); err != nil || got != "retained" {
		t.Fatalf("Get retained value = %q, %v", got, err)
	}
}

func TestMemoryLru(t *testing.T) {
	mc := NewMemoryCache(18)
	t.Cleanup(mc.stopEviction)
	if err := mc.Set("1", "1111", 30); err != nil {
		t.Fatal(err)
	}
	if err := mc.Set("2", "2222", 30); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := mc.Get("1", &got); err != nil || got != "1111" {
		t.Fatalf("Get recent key = %q, %v", got, err)
	}
	if err := mc.Set("3", "三", 30); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"1": "1111", "2": "", "3": "三"} {
		if err := mc.Get(key, &got); err != nil || got != want {
			t.Errorf("Get(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
}

func BenchmarkMemorySet(b *testing.B) {
	mc := NewMemoryCache(0)
	b.Cleanup(mc.stopEviction)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("key%d", i)
		value := fmt.Sprintf("value%d", i)
		if err := mc.Set(key, value, 1000); err != nil {
			b.Fatal(err)
		}
	}
}
