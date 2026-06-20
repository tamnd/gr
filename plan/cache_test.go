package plan

import "testing"

func TestCacheGetPut(t *testing.T) {
	c := NewCache(4)
	k := Key{Text: "RETURN 1", Catalog: 0}
	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache should miss")
	}
	e := &Entry{Op: &Unit{}}
	c.Put(k, e)
	got, ok := c.Get(k)
	if !ok || got != e {
		t.Fatalf("expected the put entry back, got %v ok=%v", got, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("len = %d, want 1", c.Len())
	}
}

func TestCacheCatalogVersionKeys(t *testing.T) {
	c := NewCache(4)
	e0 := &Entry{Op: &Unit{}}
	e1 := &Entry{Op: &Unit{}}
	c.Put(Key{Text: "RETURN 1", Catalog: 0}, e0)
	c.Put(Key{Text: "RETURN 1", Catalog: 1}, e1)
	// Same text, different catalog version: two distinct entries.
	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2 (catalog version is part of the key)", c.Len())
	}
	if got, _ := c.Get(Key{Text: "RETURN 1", Catalog: 0}); got != e0 {
		t.Fatal("version 0 should resolve to its own entry")
	}
	if got, _ := c.Get(Key{Text: "RETURN 1", Catalog: 1}); got != e1 {
		t.Fatal("version 1 should resolve to its own entry")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := NewCache(2)
	a := Key{Text: "a"}
	b := Key{Text: "b"}
	d := Key{Text: "d"}
	c.Put(a, &Entry{Op: &Unit{}})
	c.Put(b, &Entry{Op: &Unit{}})
	// Touch a so b is the least-recently-used, then insert d to force an eviction.
	if _, ok := c.Get(a); !ok {
		t.Fatal("a should be present")
	}
	c.Put(d, &Entry{Op: &Unit{}})
	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.Len())
	}
	if _, ok := c.Get(b); ok {
		t.Fatal("b should have been evicted as least-recently-used")
	}
	if _, ok := c.Get(a); !ok {
		t.Fatal("a should have survived")
	}
	if _, ok := c.Get(d); !ok {
		t.Fatal("d should be present")
	}
}

func TestCacheReputRefreshes(t *testing.T) {
	c := NewCache(4)
	k := Key{Text: "x"}
	e1 := &Entry{Op: &Unit{}}
	e2 := &Entry{Op: &Unit{}}
	c.Put(k, e1)
	c.Put(k, e2)
	if c.Len() != 1 {
		t.Fatalf("len = %d, want 1 (re-put updates in place)", c.Len())
	}
	if got, _ := c.Get(k); got != e2 {
		t.Fatal("re-put should replace the entry")
	}
}

func TestNormalizeText(t *testing.T) {
	if got := NormalizeText("  RETURN 1  "); got != "RETURN 1" {
		t.Fatalf("NormalizeText trims outer whitespace, got %q", got)
	}
	// Inner whitespace is significant-preserving: it is left as written so a
	// string literal's spaces are never collapsed.
	if got := NormalizeText("RETURN 'a  b'"); got != "RETURN 'a  b'" {
		t.Fatalf("NormalizeText must not touch inner whitespace, got %q", got)
	}
}

func TestNewCacheDefault(t *testing.T) {
	if c := NewCache(0); c.max != DefaultCacheSize {
		t.Fatalf("zero size = %d, want default %d", c.max, DefaultCacheSize)
	}
	if c := NewCache(-5); c.max != DefaultCacheSize {
		t.Fatalf("negative size = %d, want default %d", c.max, DefaultCacheSize)
	}
}
