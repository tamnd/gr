// Package blockcache is gr's property-block cache: the decoded-column-segment cache
// of doc 14 §4. It caches the decoded value vector of a hot column segment keyed by
// (column id, segment ordinal), so a re-scan or re-point-read of that segment is
// served without re-reading the segment's pages and re-running its codec (doc 15).
//
// It is a decoded cache, one altitude above the page cache (doc 14 §1.2): the page
// cache holds the segment's compressed bytes, this holds the decoded form, so a hit
// here skips both the page fetch and the decode. A cache is never a source of truth
// (doc 14 §1.4): an entry carries the base version it was built at, and a read under
// a different version misses and falls through to the authority, so the cache may be
// cleared, smaller than asked, or cold after a crash and the database still returns
// the same answers.
//
// A segment can be resident in two forms (doc 14 §4.3): decoded-resident (the value
// vector, a direct array index on read, but larger in memory) or compressed-resident
// (the segment's compressed bytes, decoded on read, smaller in memory). Under memory
// pressure an entry cools in stages (doc 14 §4.5, §4.6): a cold decoded entry is
// downgraded to compressed-resident (freeing the decoded blow-up), a cold compressed
// entry is downgraded to a zone-map stub (freeing the bytes but keeping the min/max
// so a predicated scan can still skip the segment without a page touch), and a cold
// stub is finally evicted. The replacement is CLOCK with a reference bit, which is
// scan-resistant and cheap per access (doc 14 §4.5, §9).
//
// The delta overlay is not part of this cache (doc 14 §4.7): the cached vector is the
// base, and the caller applies the recent writes on read, so a write to a position in
// the segment's range does not invalidate the cached base. The base entry is
// invalidated only when a checkpoint folds the delta into a new base segment, which
// the caller signals by reading at the new version (a version miss) or by calling
// Invalidate.
package blockcache

import (
	"container/list"
	"sync"
)

// Key identifies a cached segment: the column within its label/type group and the
// segment ordinal (which position range). The same property name on two labels is
// two columns (doc 14 §4.2), so the column id disambiguates them.
type Key struct {
	Column  uint32
	Segment uint32
}

// residency is how much of a segment an entry currently holds.
type residency uint8

const (
	// stub holds only the zone map and version, no values: what a fully cooled or
	// evicted-but-skip-useful entry leaves behind (doc 14 §4.6).
	stub residency = iota
	// compressed holds the segment's compressed bytes, decoded on read (doc 14 §4.3).
	compressed
	// decoded holds the fully decoded value vector, a direct index on read (doc 14 §4.3).
	decoded
)

// entry is one cached segment. The zone map and version live at every residency; the
// decoded vector and compressed bytes are present only at their residency.
type entry struct {
	key     Key
	res     residency
	version uint64
	vec     any    // decoded value vector, set when res == decoded
	vecSize int    // accounted bytes of vec
	comp    []byte // compressed bytes, set when res >= compressed
	zoneMin []byte
	zoneMax []byte
	ref     bool // CLOCK reference bit: set on access, cleared by the sweep
	pins    int  // an entry with pins > 0 is never cooled or evicted
}

// size is the bytes this entry counts against the budget: the decoded vector plus the
// compressed bytes it currently holds. A stub counts as zero, so the cache can keep
// far more zone-map stubs than value vectors (doc 14 §4.6).
func (e *entry) size() int {
	n := len(e.comp)
	if e.res == decoded {
		n += e.vecSize
	}
	return n
}

// Cache is the property-block cache. It is bounded by a byte budget over the value
// memory (decoded vectors plus compressed bytes); zone-map stubs are tiny and not
// counted, matching doc 14 §4.6's resident skip index. It is safe for concurrent use.
type Cache struct {
	mu       sync.Mutex
	maxBytes int
	curBytes int
	entries  map[Key]*list.Element
	ring     *list.List    // CLOCK ring of *entry
	hand     *list.Element // the next element the sweep will examine
}

// New creates a property-block cache bounded to maxBytes of value memory. A maxBytes
// of zero or less disables value caching: entries cool to zone-map stubs immediately,
// which is still useful for segment skipping (doc 14 §4.6).
func New(maxBytes int) *Cache {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &Cache{
		maxBytes: maxBytes,
		entries:  make(map[Key]*list.Element),
		ring:     list.New(),
	}
}

// GetDecoded returns the decoded value vector for a key when the entry is
// decoded-resident and its version matches, marking it referenced. A version mismatch
// is a miss and drops the stale entry, so a checkpoint that produced a new base is not
// served an old vector.
func (c *Cache) GetDecoded(k Key, version uint64) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.live(k, version)
	if !ok || e.res != decoded {
		return nil, false
	}
	e.ref = true
	return e.vec, true
}

// GetCompressed returns the resident compressed bytes for a key when the entry holds
// them (compressed-resident or decoded-resident that kept its bytes) and its version
// matches, marking it referenced.
func (c *Cache) GetCompressed(k Key, version uint64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.live(k, version)
	if !ok || e.comp == nil {
		return nil, false
	}
	e.ref = true
	return e.comp, true
}

// ZoneMap returns the cached min and max for a key at any residency (including a
// stub) when the version matches, the cheapest useful hit: a predicated scan can skip
// the segment without a page touch (doc 14 §4.6). It does not mark the entry
// referenced, since a skip is not a use of the values.
func (c *Cache) ZoneMap(k Key, version uint64) (min, max []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.live(k, version)
	if !ok {
		return nil, nil, false
	}
	return e.zoneMin, e.zoneMax, true
}

// live returns the entry for a key if present and version-current, dropping it on a
// version mismatch. The caller holds the lock.
func (c *Cache) live(k Key, version uint64) (*entry, bool) {
	el, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if e.version != version {
		c.remove(el)
		return nil, false
	}
	return e, true
}

// PutDecoded caches a segment decoded-resident, keeping its compressed bytes too so a
// later downgrade can fall back to them without a page fetch. vecSize is the caller's
// estimate of the decoded vector's bytes, which the budget accounts. Re-putting a key
// replaces its entry.
func (c *Cache) PutDecoded(k Key, version uint64, vec any, vecSize int, comp, zoneMin, zoneMax []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.upsert(k)
	e.version = version
	e.res = decoded
	e.vec = vec
	e.vecSize = vecSize
	e.comp = comp
	e.zoneMin = zoneMin
	e.zoneMax = zoneMax
	e.ref = true
	c.recount()
	c.evictToBudget()
}

// PutCompressed caches a segment compressed-resident: the bytes are decoded on each
// read, cheaper than a page miss because the bytes are resident (doc 14 §4.3).
func (c *Cache) PutCompressed(k Key, version uint64, comp, zoneMin, zoneMax []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.upsert(k)
	e.version = version
	e.res = compressed
	e.vec = nil
	e.vecSize = 0
	e.comp = comp
	e.zoneMin = zoneMin
	e.zoneMax = zoneMax
	e.ref = true
	c.recount()
	c.evictToBudget()
}

// upsert returns the entry for a key, creating it (and ringing it) if new. The caller
// holds the lock and fills in the fields.
func (c *Cache) upsert(k Key) *entry {
	if el, ok := c.entries[k]; ok {
		return el.Value.(*entry)
	}
	e := &entry{key: k}
	el := c.ring.PushBack(e)
	c.entries[k] = el
	return e
}

// Pin protects an entry from cooling and eviction while a reader holds its decoded
// vector. It is a no-op for an absent key. Every Pin must be matched by an Unpin.
func (c *Cache) Pin(k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		el.Value.(*entry).pins++
	}
}

// Unpin releases a pin taken by Pin.
func (c *Cache) Unpin(k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		if e := el.Value.(*entry); e.pins > 0 {
			e.pins--
		}
	}
}

// Invalidate drops a key entirely, the explicit form a checkpoint uses when it folds
// the delta into a new base segment and the cached base is no longer authoritative
// (doc 14 §4.7, §13.5). A version-based miss handles the common case; this is the
// eager path.
func (c *Cache) Invalidate(k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		c.remove(el)
	}
}

// recount recomputes curBytes from the entries. It is O(n) and called on a put, which
// is already an O(n)-amortized event under eviction; keeping a running sum is a later
// refinement if puts ever dominate.
func (c *Cache) recount() {
	total := 0
	for _, el := range c.entries {
		total += el.Value.(*entry).size()
	}
	c.curBytes = total
}

// evictToBudget sweeps the CLOCK ring until the value memory fits the budget. Each
// sweep step cools or evicts one cold, unpinned entry; a referenced entry gets its
// bit cleared and a reprieve. Cooling is staged (doc 14 §4.5): decoded downgrades to
// compressed, compressed downgrades to a stub, a stub is evicted. The sweep gives up
// after a bounded number of steps with no progress, so an all-pinned ring cannot spin.
func (c *Cache) evictToBudget() {
	if c.curBytes <= c.maxBytes {
		return
	}
	// A full sweep that frees nothing means every value-holding entry is pinned, so
	// cap the work at a few laps of the ring rather than looping forever.
	maxSteps := c.ring.Len() * 3
	for step := 0; c.curBytes > c.maxBytes && step < maxSteps; step++ {
		el := c.advance()
		if el == nil {
			return
		}
		e := el.Value.(*entry)
		if e.pins > 0 {
			continue
		}
		if e.ref {
			e.ref = false
			continue
		}
		c.cool(el)
	}
}

// cool drops one residency level of a cold entry, reclaiming its bytes: decoded ->
// compressed -> stub -> evicted.
func (c *Cache) cool(el *list.Element) {
	e := el.Value.(*entry)
	switch e.res {
	case decoded:
		c.curBytes -= e.vecSize
		e.vec = nil
		e.vecSize = 0
		e.res = compressed
	case compressed:
		c.curBytes -= len(e.comp)
		e.comp = nil
		e.res = stub
	default: // stub
		c.remove(el)
	}
}

// advance returns the element under the CLOCK hand and moves the hand on, wrapping at
// the ring's end. It returns nil only for an empty ring.
func (c *Cache) advance() *list.Element {
	if c.ring.Len() == 0 {
		return nil
	}
	if c.hand == nil {
		c.hand = c.ring.Front()
	}
	el := c.hand
	c.hand = c.hand.Next() // nil at the back, re-seeded to Front next call
	return el
}

// remove deletes an entry from the ring and the index, keeping the hand valid.
func (c *Cache) remove(el *list.Element) {
	e := el.Value.(*entry)
	c.curBytes -= e.size()
	if c.hand == el {
		c.hand = el.Next()
	}
	c.ring.Remove(el)
	delete(c.entries, e.key)
}

// Len returns the number of entries at any residency, including zone-map stubs.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.Len()
}

// Bytes returns the value memory currently held, the quantity the budget bounds.
func (c *Cache) Bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes
}
