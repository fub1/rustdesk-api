package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func redisForTest(t testing.TB, construct func(*redis.Options) *RedisCache) (*RedisCache, string) {
	t.Helper()
	addr := os.Getenv("RUSTDESK_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RUSTDESK_TEST_REDIS_REQUIRED") == "1" {
			t.Fatal("RUSTDESK_TEST_REDIS_REQUIRED=1 requires RUSTDESK_TEST_REDIS_ADDR")
		}
		t.Skip("Redis integration test: set RUSTDESK_TEST_REDIS_ADDR to opt in (required in CI)")
	}
	rc := construct(&redis.Options{
		Addr:         addr,
		Password:     os.Getenv("RUSTDESK_TEST_REDIS_PASSWORD"),
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		MaxRetries:   -1,
	})
	t.Cleanup(func() {
		if err := rc.rdb.Close(); err != nil {
			t.Errorf("close Redis connection: %v", err)
		}
	})
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rc.rdb.Ping(pingCtx).Err(); err != nil {
		t.Fatalf("Redis at configured address is unavailable: %v", err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("generate isolated Redis key: %v", err)
	}
	key := "rustdesk-api-test:" + hex.EncodeToString(random[:])
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rc.rdb.Del(cleanupCtx, key).Err(); err != nil {
			t.Errorf("remove test's Redis key: %v", err)
		}
	})
	return rc, key
}

func TestRedisSet(t *testing.T) {
	rc, key := redisForTest(t, RedisCacheInit)
	if err := rc.Set(key, "value", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	ttl, err := rc.rdb.TTL(context.Background(), key).Result()
	if err != nil || ttl <= 0 {
		t.Fatalf("TTL = %v, error = %v; want positive default TTL", ttl, err)
	}
}

func TestRedisGet(t *testing.T) {
	rc, key := redisForTest(t, RedisCacheInit)
	if err := rc.Set(key, "value", 300); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var got string
	if err := rc.Get(key, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "value" {
		t.Fatalf("Get = %q, want value", got)
	}
}

func TestRedisGetJson(t *testing.T) {
	rc, key := redisForTest(t, RedisCacheInit)
	want := &r{A: "a", B: "b", R: &rr{AA: "aa", BB: "bb"}}
	if err := rc.Set(key, want, 300); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := &r{}
	if err := rc.Get(key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %#v, want %#v", got, want)
	}
}

func BenchmarkRSet(b *testing.B) {
	rc, key := redisForTest(b, RedisCacheInit)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := rc.Set(key, "value", 1000); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRGet(b *testing.B) {
	rc, key := redisForTest(b, RedisCacheInit)
	if err := rc.Set(key, "value", 1000); err != nil {
		b.Fatal(err)
	}
	var got string
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := rc.Get(key, &got); err != nil {
			b.Fatal(err)
		}
	}
}
