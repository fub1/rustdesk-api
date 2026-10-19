package cache

import (
	"fmt"
	"reflect"
	"testing"
)

func TestSimpleCache(t *testing.T) {

	type st struct {
		A string
		B string
	}

	items := map[string]interface{}{}
	items["a"] = "b"
	items["b"] = "c"

	ab := &st{
		A: "a",
		B: "b",
	}
	items["ab"] = *ab

	a := items["a"]
	fmt.Println(a)

	b := items["b"]
	fmt.Println(b)

	ab.A = "aa"
	ab2 := st{}
	ab2 = (items["ab"]).(st)
	fmt.Println(ab2, reflect.TypeOf(ab2))

}

func TestFileCacheSet(t *testing.T) {
	fc := New(TypeFile).(*FileCache)
	fc.SetDir(t.TempDir())
	if err := fc.Set("key", "value", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var got string
	if err := fc.Get("key", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "value" {
		t.Fatalf("Get = %q, want value", got)
	}
}

func TestFileCacheGet(t *testing.T) {
	fc := New(TypeFile).(*FileCache)
	fc.SetDir(t.TempDir())
	if err := fc.Set("key", "updated", 300); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var got string
	if err := fc.Get("key", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "updated" {
		t.Fatalf("Get = %q, want updated", got)
	}
}

func TestRedisCacheSet(t *testing.T) {
	rc, key := redisForTest(t, NewRedis)
	if err := rc.Set(key, "value", 0); err != nil {
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

func TestRedisCacheGet(t *testing.T) {
	rc, key := redisForTest(t, NewRedis)
	if err := rc.Set(key, "updated", 300); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var got string
	if err := rc.Get(key, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "updated" {
		t.Fatalf("Get = %q, want updated", got)
	}
}
