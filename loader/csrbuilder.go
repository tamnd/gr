package loader

import "sort"

// csrDir is the direction of one CSR slot.
type csrDir uint8

const (
	csrFwd csrDir = 0 // out-edges: offset array indexed by source node
	csrBwd csrDir = 1 // in-edges: offset array indexed by destination node
)

// csrKey identifies one (relType token, direction) pair in the per-type CSR map.
type csrKey struct {
	relType uint32
	dir     csrDir
}

// csrBuilder builds one direction's CSR by counting-sort (doc 19 §4.3).
// The three sub-steps — count, prefix-sum, scatter — are driven by the caller;
// sort-within-runs is called once after all scatter passes are done.
//
// After PrefixSum the deg slice becomes the offset array. After Scatter+Sort the
// builder holds the final CSR: off[i] is the start of node i's neighbor run,
// off[i+1] its end, nbr[off[i]:off[i+1]] is the sorted neighbor list, and
// eid[off[i]:off[i+1]] is the parallel edge-id list.
type csrBuilder struct {
	deg []uint64 // count sub-step: out/in-degree of each node; after PrefixSum: offset array
	nbr []uint64 // neighbor dense ids (allocated after PrefixSum, filled in Scatter)
	eid []uint64 // edge dense ids parallel to nbr
	cur []uint64 // per-node write cursor during scatter (initialized from deg after prefix-sum)
}

// newCSRBuilder returns a builder for a source/destination space of size n.
func newCSRBuilder(n uint64) *csrBuilder {
	return &csrBuilder{deg: make([]uint64, n)}
}

// Count increments the degree of node src by one (called for each edge).
func (b *csrBuilder) Count(src uint64) {
	b.deg[src]++
}

// PrefixSum turns the degree array into the offset array in place and allocates
// the neighbor and edge-id arrays. After this call, offset[n] == edge count.
func (b *csrBuilder) PrefixSum() {
	n := uint64(len(b.deg))
	// Compute running total; grow deg by one to hold the terminating offset.
	var total uint64
	for i := range n {
		deg := b.deg[i]
		b.deg[i] = total
		total += deg
	}
	b.deg = append(b.deg, total) // terminating offset == edge count
	b.nbr = make([]uint64, total)
	b.eid = make([]uint64, total)
	// Initialize write cursors from the offsets (before they are advanced by Scatter).
	b.cur = make([]uint64, n)
	copy(b.cur, b.deg[:n])
}

// Scatter places neighbor dst and edge id eid into the next free slot of node
// src's run. Must be called after PrefixSum and before SortWithinRuns.
func (b *csrBuilder) Scatter(src, dst, eid uint64) {
	pos := b.cur[src]
	b.cur[src]++
	b.nbr[pos] = dst
	b.eid[pos] = eid
}

// SortWithinRuns sorts each node's neighbor run by neighbor dense id, keeping
// the eid array aligned. After this call the CSR is in the format the engine
// expects: each run is sorted ascending by neighbor id (doc 19 §4.3, step 4).
func (b *csrBuilder) SortWithinRuns() {
	n := len(b.deg) - 1 // len(deg) == nodeCount+1 after PrefixSum
	for i := range n {
		lo, hi := b.deg[i], b.deg[i+1]
		if hi-lo < 2 {
			continue // 0 or 1 neighbor: already sorted
		}
		sortRun(b.nbr[lo:hi], b.eid[lo:hi])
	}
}

// Offsets returns the offset array (len == nodeCount+1).
func (b *csrBuilder) Offsets() []uint64 { return b.deg }

// Neighbors returns the neighbor dense-id array.
func (b *csrBuilder) Neighbors() []uint64 { return b.nbr }

// Edges returns the edge dense-id array parallel to Neighbors.
func (b *csrBuilder) Edges() []uint64 { return b.eid }

// EdgeCount returns the total number of edges stored.
func (b *csrBuilder) EdgeCount() uint64 {
	if len(b.deg) == 0 {
		return 0
	}
	return b.deg[len(b.deg)-1]
}

// sortRun sorts two parallel slices by the values in nbr, keeping eid aligned.
// Uses sort.Slice which is fine for the typically small runs; within a run,
// edges are a small fraction of the total and the sort is cache-local.
func sortRun(nbr, eid []uint64) {
	type pair struct{ n, e uint64 }
	pairs := make([]pair, len(nbr))
	for i := range nbr {
		pairs[i] = pair{nbr[i], eid[i]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].n < pairs[j].n })
	for i, p := range pairs {
		nbr[i] = p.n
		eid[i] = p.e
	}
}
