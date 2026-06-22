package plan

import (
	"container/list"
	"strings"
	"sync"

	"github.com/tamnd/gr/bind"
)

// DefaultCacheSize is the plan cache's default capacity in distinct query shapes.
// A serving workload runs tens to low thousands of distinct shapes (doc 14 §8.8),
// so a few hundred entries holds the hot set with room to spare.
const DefaultCacheSize = 256

// Entry is a compiled query: the bound query and the logical plan built from it.
// The executor needs both — the plan to run, and the bound query to build the
// property-key resolver ([exec.ResolverFromBound]) — so the cache stores the pair.
// An Entry is immutable once cached: the plan is read (never mutated) when the
// executor compiles it, so one Entry serves concurrent executions safely.
type Entry struct {
	Bound *bind.Bound
	Op    Op
	// Stats is the cardinality basis the plan was costed against, so a cache hit can
	// tell whether the data has drifted far enough to re-plan (doc 11 §7). It is the
	// zero snapshot for a structurally planned query, which never drifts.
	Stats StatsSnapshot
}

// Key identifies a compiled query in the cache: the (normalized) query text and
// the catalog version it was bound against. The catalog version is part of the
// key so a schema change — which can change the optimal plan and can change which
// names a bound query resolved — misses the entries bound against the old catalog
// (doc 14 §8.2, §8.4), the conservative-and-correct invalidation M2 ships.
type Key struct {
	Text    string
	Catalog uint64
}

// Cache is the in-memory, per-open plan cache (doc 14 §8; doc 25 §5.2 item 10).
// It amortizes compilation: a repeated query shape reuses its plan instead of
// re-parsing, re-binding, and re-planning, so the per-execution cost falls to
// binding parameters and running the plan.
//
// M2 lands the seam and a basic LRU keyed by the parameterized query: parameters
// are placeholders in the text, so a shape's plan is reused across all parameter
// values, the point of a parameterized query. M4's adaptive policy — multiple
// plans per shape keyed by parameter selectivity (doc 11 §9.3) — lives behind
// this same Get/Put interface, as does any richer normalization (doc 10 §9.1);
// M2 keys by the trimmed text, the safe identity normalization (collapsing inner
// whitespace would corrupt string literals).
//
// The cache is not persisted: it starts empty on each open and warms as the hot
// shapes run (doc 14 §8.7). It is safe for concurrent use.
type Cache struct {
	mu      sync.Mutex
	max     int
	ll      *list.List // most-recently-used at the front
	entries map[Key]*list.Element

	// OnEvict, if set, is called once for each plan the cache drops, with the reason it left
	// (doc 20 §3.2). The cache drives only the capacity reason, the LRU eviction under size
	// pressure; it is the seam db.Metrics counts gr_plan_cache_evictions_total on. It is called
	// while the cache lock is held, so it must not re-enter the cache, only record.
	OnEvict func(reason string)
}

// cacheNode is the value stored in each list element: its key (so eviction can
// delete the map entry) and the compiled query.
type cacheNode struct {
	key   Key
	entry *Entry
}

// NewCache creates a plan cache holding at most max entries; a max of zero or
// less uses [DefaultCacheSize]. When full, the least-recently-used entry is
// evicted, which only costs that shape's next execution a re-plan (doc 14 §8.8).
func NewCache(max int) *Cache {
	if max <= 0 {
		max = DefaultCacheSize
	}
	return &Cache{
		max:     max,
		ll:      list.New(),
		entries: make(map[Key]*list.Element),
	}
}

// NormalizeText canonicalizes query text into the cache key's text component.
// M2's normalization is the identity-safe one: outer whitespace is trimmed (it
// can never fall inside a string literal), and the text is otherwise used as
// written, because token-level normalization (stripping insignificant inner
// whitespace and comments, doc 10 §9.1) needs the lexer and is a later
// refinement.
func NormalizeText(text string) string { return strings.TrimSpace(text) }

// Get returns the cached entry for a key, marking it most-recently-used, or
// reports false on a miss.
func (c *Cache) Get(k Key) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cacheNode).entry, true
}

// Put inserts (or refreshes) the entry for a key, evicting the least-recently-used
// entry if the cache is over capacity. Re-putting an existing key updates its
// entry and marks it most-recently-used.
func (c *Cache) Put(k Key, e *Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		el.Value.(*cacheNode).entry = e
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheNode{key: k, entry: e})
	c.entries[k] = el
	if c.ll.Len() > c.max {
		c.evict()
	}
}

// evict removes the least-recently-used entry (the back of the list).
func (c *Cache) evict() {
	el := c.ll.Back()
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.entries, el.Value.(*cacheNode).key)
	if c.OnEvict != nil {
		c.OnEvict("capacity")
	}
}

// Len returns the number of cached entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Cap returns the cache's capacity in distinct query shapes, the resolved maximum that
// NewCache fixed (the configured size, or DefaultCacheSize when none was given). It backs
// the read-only plan_cache_size pragma (doc 24 §3.3).
func (c *Cache) Cap() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max
}
