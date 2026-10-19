package cache

import (
	"reflect"
	"testing"
)

func fileForTest(t testing.TB) *FileCache {
	t.Helper()
	fc := NewFileCache()
	fc.SetDir(t.TempDir())
	return fc
}

func TestFileSet(t *testing.T) {
	fc := fileForTest(t)
	if err := fc.Set("key", "value", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
}

func TestFileGet(t *testing.T) {
	fc := fileForTest(t)
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

func TestFileSetGet(t *testing.T) {
	fc := fileForTest(t)
	if err := fc.Set("key", "before", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := fc.Set("key", "after", 300); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	var got string
	if err := fc.Get("key", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "after" {
		t.Fatalf("Get = %q, want after", got)
	}
}

func TestFileGetJson(t *testing.T) {
	fc := fileForTest(t)
	want := &r{A: "a", B: "b"}
	if err := fc.Set("key", want, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := &r{}
	if err := fc.Get("key", got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %#v, want %#v", got, want)
	}
}

func TestFileSetGetJson(t *testing.T) {
	fc := fileForTest(t)
	want := &r{A: "a", B: "b", R: &rr{AA: "aa", BB: "bb"}}
	if err := fc.Set("key", want, 300); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := &r{}
	if err := fc.Get("key", got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %#v, want %#v", got, want)
	}
}

func BenchmarkSet(b *testing.B) {
	fc := fileForTest(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fc.Set("key", "value", 1000); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	fc := fileForTest(b)
	if err := fc.Set("key", "value", 1000); err != nil {
		b.Fatal(err)
	}
	var got string
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fc.Get("key", &got); err != nil {
			b.Fatal(err)
		}
	}
}
