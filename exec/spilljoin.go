package exec

import (
	"fmt"
	"math"
	"sort"

	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// Spill-to-disk for an over-budget hash join (spec 2060 doc 12 §9, doc 25 §7.2
// deliverable 8). The in-memory join (setop.go) buffers the whole build side in a
// map, which is fine until the build side is larger than memory. When a memory
// budget is armed (Ctx.MemBudget > 0) and a temp area is configured
// (Ctx.TempFile != nil), the join switches to a recursive grace hash join: it
// partitions both inputs by a hash of the join key into temp files, then joins one
// partition pair at a time, so only one partition's build side is resident at once.
// A partition whose build side is still over budget is re-partitioned with a
// deeper hash seed, up to a depth cap past which it is joined in memory regardless
// (the cap guarantees termination on a heavily skewed key; the in-memory fallback
// keeps the answer correct even there).
//
// This changes only how the join spends memory, never its answer. The grace path
// produces exactly the rows the in-memory probe would, in the same per-left-row
// match order, so the M2/M3 result oracle stays green (M4 changes performance, not
// answers). Spilling is off by default: with the zero budget every read uses
// today, the join stays entirely in memory on the original fast path.
//
// A cartesian product (an empty join key) cannot be partitioned by key, so it is
// never spilled; the planner only emits a keyless join for a disconnected pattern,
// whose build side is small in practice.

const (
	// gracePartitions is the fan-out per partitioning pass: how many temp-file
	// buckets each input is split into. A larger fan-out makes each partition
	// smaller (fewer recursion passes) at the cost of more open temp files.
	gracePartitions = 16
	// graceMaxDepth caps the recursion. Past it a partition is joined in memory
	// even if over budget, which guarantees termination when a single key (or a
	// hash-colliding set) dominates and cannot be split further.
	graceMaxDepth = 4
)

// spillPart is one input's rows for one partition, length-prefixed records in a
// temp file. It tracks the exact bytes written so the reader can pull the whole
// file back without relying on the VFS reporting EOF (the in-memory VFS zero-fills
// a short read rather than returning io.EOF). discard closes and removes the file
// and is safe to call more than once.
type spillPart struct {
	file    vfs.File
	discard func() error
	off     int64
	rows    int
	done    bool
}

// write appends one row as a uvarint length prefix followed by the encoded row.
func (p *spillPart) write(row eval.Row) error {
	body := appendRow(nil, row)
	rec := format.AppendUvarint(nil, uint64(len(body)))
	rec = append(rec, body...)
	if _, err := p.file.WriteAt(rec, p.off); err != nil {
		return err
	}
	p.off += int64(len(rec))
	p.rows++
	return nil
}

// readAll reads the whole partition back into a row slice. Each partition is
// bounded to about the memory budget by the partitioning, so holding one resident
// is the grace join's memory ceiling.
func (p *spillPart) readAll() ([]eval.Row, error) {
	if p.off == 0 {
		return nil, nil
	}
	buf := make([]byte, p.off)
	if _, err := p.file.ReadAt(buf, 0); err != nil {
		return nil, err
	}
	var rows []eval.Row
	for len(buf) > 0 {
		n, hn, err := format.Uvarint(buf)
		if err != nil {
			return nil, err
		}
		buf = buf[hn:]
		if uint64(len(buf)) < n {
			return nil, fmt.Errorf("exec: truncated spill record")
		}
		row, _, err := decodeRow(buf[:n])
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
		buf = buf[n:]
	}
	return rows, nil
}

// bytes is the partition's on-disk size, the figure the grace join compares to the
// budget when deciding whether to re-partition.
func (p *spillPart) bytes() int64 { return p.off }

// discardOnce closes and removes the file, at most once.
func (p *spillPart) discardOnce() error {
	if p.done {
		return nil
	}
	p.done = true
	return p.discard()
}

// partPair is the two inputs' rows for one partition: left is the probe side,
// right is the build side, depth is how many partitioning passes produced it.
type partPair struct {
	left  *spillPart
	right *spillPart
	depth int
}

// graceJoin is the spilling form of joinOp: it owns the partition temp files and
// iterates them one pair at a time, building a hash table from the build (right)
// side and probing it with the left side, exactly as the in-memory join does per
// partition.
type graceJoin struct {
	ctx *Ctx
	on  []string

	level0 []*partPair  // the depth-0 partitions the build phase fills
	all    []*spillPart // every part ever created, for cleanup
	queue  []*partPair  // partitions still to join (children are pushed here)

	// current partition probe state, mirroring joinOp's in-memory probe loop.
	table   map[string][]eval.Row
	probe   []eval.Row
	ppos    int
	cur     eval.Row
	matches []eval.Row
	mpos    int
}

// newGraceJoin creates the depth-0 partition pairs the build phase fills.
func newGraceJoin(ctx *Ctx, on []string) (*graceJoin, error) {
	g := &graceJoin{ctx: ctx, on: on}
	g.level0 = make([]*partPair, gracePartitions)
	for i := range g.level0 {
		pair, err := g.newPair(0)
		if err != nil {
			g.cleanup()
			return nil, err
		}
		g.level0[i] = pair
	}
	return g, nil
}

// newPart opens a fresh temp file and tracks it for cleanup.
func (g *graceJoin) newPart() (*spillPart, error) {
	f, discard, err := g.ctx.TempFile()
	if err != nil {
		return nil, err
	}
	p := &spillPart{file: f, discard: discard}
	g.all = append(g.all, p)
	return p, nil
}

// newPair opens a left and a right part for one partition at the given depth.
func (g *graceJoin) newPair(depth int) (*partPair, error) {
	l, err := g.newPart()
	if err != nil {
		return nil, err
	}
	r, err := g.newPart()
	if err != nil {
		return nil, err
	}
	return &partPair{left: l, right: r, depth: depth}, nil
}

// partitionRight routes one build-side row to its depth-0 partition.
func (g *graceJoin) partitionRight(row eval.Row) error {
	return g.level0[partID(rowKey(row, g.on), 0)].right.write(row)
}

// partitionLeft routes one probe-side row to its depth-0 partition.
func (g *graceJoin) partitionLeft(row eval.Row) error {
	return g.level0[partID(rowKey(row, g.on), 0)].left.write(row)
}

// start arms iteration once both inputs are fully partitioned.
func (g *graceJoin) start() { g.queue = g.level0 }

// next yields the join's rows, the same rows and order the in-memory probe would.
func (g *graceJoin) next() (eval.Row, bool, error) {
	for {
		for g.mpos < len(g.matches) {
			r := g.matches[g.mpos]
			g.mpos++
			return mergeRows(g.cur, r), true, nil
		}
		if g.ppos < len(g.probe) {
			g.cur = g.probe[g.ppos]
			g.ppos++
			g.matches = g.table[rowKey(g.cur, g.on)]
			g.mpos = 0
			continue
		}
		ok, err := g.loadNext()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
	}
}

// loadNext advances to the next joinable partition. A partition whose build side
// is still over budget (and not at the depth cap) is re-partitioned with a deeper
// seed and its children take its place in the queue; otherwise its build side is
// loaded into the hash table and its probe side into the row slice.
func (g *graceJoin) loadNext() (bool, error) {
	for len(g.queue) > 0 {
		pr := g.queue[0]
		g.queue = g.queue[1:]
		if pr.right.bytes() > g.ctx.MemBudget && pr.depth < graceMaxDepth {
			children, err := g.repartition(pr)
			if err != nil {
				return false, err
			}
			_ = pr.left.discardOnce()
			_ = pr.right.discardOnce()
			g.queue = append(children, g.queue...)
			continue
		}
		rights, err := pr.right.readAll()
		if err != nil {
			return false, err
		}
		table := make(map[string][]eval.Row, len(rights))
		for _, r := range rights {
			k := rowKey(r, g.on)
			table[k] = append(table[k], r)
		}
		probe, err := pr.left.readAll()
		if err != nil {
			return false, err
		}
		_ = pr.left.discardOnce()
		_ = pr.right.discardOnce()
		g.table, g.probe, g.ppos = table, probe, 0
		g.matches, g.mpos = nil, 0
		return true, nil
	}
	return false, nil
}

// repartition splits a pair both sides into gracePartitions children one depth
// deeper, redistributing rows by the deeper-seeded hash so a key that collided at
// this depth can land in a different child next time.
func (g *graceJoin) repartition(pr *partPair) ([]*partPair, error) {
	depth := pr.depth + 1
	children := make([]*partPair, gracePartitions)
	for i := range children {
		pair, err := g.newPair(depth)
		if err != nil {
			return nil, err
		}
		children[i] = pair
	}
	rights, err := pr.right.readAll()
	if err != nil {
		return nil, err
	}
	for _, row := range rights {
		id := partID(rowKey(row, g.on), depth)
		if err := children[id].right.write(row); err != nil {
			return nil, err
		}
	}
	lefts, err := pr.left.readAll()
	if err != nil {
		return nil, err
	}
	for _, row := range lefts {
		id := partID(rowKey(row, g.on), depth)
		if err := children[id].left.write(row); err != nil {
			return nil, err
		}
	}
	return children, nil
}

// cleanup discards every temp file the join opened. It is idempotent per file, so
// joinOp.close can call it whether or not iteration ran to completion.
func (g *graceJoin) cleanup() {
	for _, p := range g.all {
		_ = p.discardOnce()
	}
}

// partID hashes a join key to a partition with a depth-varying seed (FNV-1a, the
// depth folded into the offset basis), so re-partitioning a colliding partition
// redistributes its keys.
func partID(key string, depth int) int {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset) ^ uint64(depth)*prime
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime
	}
	return int(h % gracePartitions)
}

// rowSize estimates a row's resident byte cost, the figure joinOp accumulates to
// decide when the in-memory build side has grown past the budget. It is an
// estimate, not the exact encoded size, which is all the spill decision needs.
func rowSize(row eval.Row) int64 {
	n := int64(8)
	for k, v := range row {
		n += int64(len(k)) + valueSize(v)
	}
	return n
}

// valueSize estimates one value's resident byte cost.
func valueSize(v value.Value) int64 {
	switch v.Type() {
	case value.TypeString:
		s, _ := v.AsString()
		return int64(len(s)) + 1
	case value.TypeBytes:
		b, _ := v.AsBytes()
		return int64(len(b)) + 1
	case value.TypeList:
		l, _ := v.AsList()
		n := int64(1)
		for _, e := range l {
			n += valueSize(e)
		}
		return n
	case value.TypePath:
		l, _ := v.AsPath()
		n := int64(1)
		for _, e := range l {
			n += valueSize(e)
		}
		return n
	case value.TypeMap:
		m, _ := v.AsMap()
		n := int64(1)
		for k, e := range m {
			n += int64(len(k)) + valueSize(e)
		}
		return n
	default:
		return 9
	}
}

// appendRow encodes a row: a uvarint key count, then each key and value, keys in
// sorted order so the encoding is deterministic.
func appendRow(dst []byte, row eval.Row) []byte {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dst = format.AppendUvarint(dst, uint64(len(keys)))
	for _, k := range keys {
		dst = format.AppendString(dst, k)
		dst = appendValue(dst, row[k])
	}
	return dst
}

// decodeRow is the inverse of appendRow.
func decodeRow(b []byte) (eval.Row, int, error) {
	n, hn, err := format.Uvarint(b)
	if err != nil {
		return nil, 0, err
	}
	off := hn
	row := make(eval.Row, n)
	for i := uint64(0); i < n; i++ {
		k, kn, err := format.String(b[off:])
		if err != nil {
			return nil, 0, err
		}
		off += kn
		v, vn, err := decodeValue(b[off:])
		if err != nil {
			return nil, 0, err
		}
		off += vn
		row[k] = v
	}
	return row, off, nil
}

// appendValue encodes one value. It covers every value type, including the
// runtime-only Node, Rel, and Path handles that format.AppendValue (which only
// handles stored property types) leaves out, since a spilled join row carries
// whatever the pattern bound, entities included.
func appendValue(dst []byte, v value.Value) []byte {
	dst = append(dst, byte(v.Type()))
	switch v.Type() {
	case value.TypeNull:
		// tag only
	case value.TypeBool:
		bv, _ := v.AsBool()
		if bv {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
	case value.TypeInt:
		iv, _ := v.AsInt()
		dst = format.AppendVarint(dst, iv)
	case value.TypeFloat:
		fv, _ := v.AsFloat()
		var buf [8]byte
		format.PutU64(buf[:], math.Float64bits(fv))
		dst = append(dst, buf[:]...)
	case value.TypeString:
		sv, _ := v.AsString()
		dst = format.AppendString(dst, sv)
	case value.TypeBytes:
		bv, _ := v.AsBytes()
		dst = format.AppendUvarint(dst, uint64(len(bv)))
		dst = append(dst, bv...)
	case value.TypeList:
		lv, _ := v.AsList()
		dst = format.AppendUvarint(dst, uint64(len(lv)))
		for _, e := range lv {
			dst = appendValue(dst, e)
		}
	case value.TypeMap:
		mv, _ := v.AsMap()
		keys := make([]string, 0, len(mv))
		for k := range mv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = format.AppendUvarint(dst, uint64(len(mv)))
		for _, k := range keys {
			dst = format.AppendString(dst, k)
			dst = appendValue(dst, mv[k])
		}
	case value.TypeNode:
		id, _ := v.AsNode()
		dst = format.AppendUvarint(dst, id)
	case value.TypeRel:
		id, _ := v.AsRel()
		dst = format.AppendUvarint(dst, id)
	case value.TypePath:
		pv, _ := v.AsPath()
		dst = format.AppendUvarint(dst, uint64(len(pv)))
		for _, e := range pv {
			dst = appendValue(dst, e)
		}
	}
	return dst
}

// decodeValue is the inverse of appendValue.
func decodeValue(b []byte) (value.Value, int, error) {
	if len(b) < 1 {
		return value.Null, 0, fmt.Errorf("exec: short spill value")
	}
	t := value.Type(b[0])
	off := 1
	switch t {
	case value.TypeNull:
		return value.Null, off, nil
	case value.TypeBool:
		if len(b) < off+1 {
			return value.Null, 0, fmt.Errorf("exec: short spill bool")
		}
		return value.Bool(b[off] != 0), off + 1, nil
	case value.TypeInt:
		iv, n, err := format.Varint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		return value.Int(iv), off + n, nil
	case value.TypeFloat:
		if len(b) < off+8 {
			return value.Null, 0, fmt.Errorf("exec: short spill float")
		}
		return value.Float(math.Float64frombits(format.U64(b[off:]))), off + 8, nil
	case value.TypeString:
		s, n, err := format.String(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		return value.String(s), off + n, nil
	case value.TypeBytes:
		ln, hn, err := format.Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		off += hn
		if uint64(len(b[off:])) < ln {
			return value.Null, 0, fmt.Errorf("exec: short spill bytes")
		}
		return value.Bytes(b[off : off+int(ln)]), off + int(ln), nil
	case value.TypeList:
		ln, hn, err := format.Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		off += hn
		elems := make([]value.Value, 0, ln)
		for i := uint64(0); i < ln; i++ {
			e, n, err := decodeValue(b[off:])
			if err != nil {
				return value.Null, 0, err
			}
			elems = append(elems, e)
			off += n
		}
		return value.List(elems...), off, nil
	case value.TypeMap:
		ln, hn, err := format.Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		off += hn
		m := make(map[string]value.Value, ln)
		for i := uint64(0); i < ln; i++ {
			k, n, err := format.String(b[off:])
			if err != nil {
				return value.Null, 0, err
			}
			off += n
			e, n2, err := decodeValue(b[off:])
			if err != nil {
				return value.Null, 0, err
			}
			off += n2
			m[k] = e
		}
		return value.Map(m), off, nil
	case value.TypeNode:
		id, n, err := format.Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		return value.Node(id), off + n, nil
	case value.TypeRel:
		id, n, err := format.Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		return value.Rel(id), off + n, nil
	case value.TypePath:
		ln, hn, err := format.Uvarint(b[off:])
		if err != nil {
			return value.Null, 0, err
		}
		off += hn
		elems := make([]value.Value, 0, ln)
		for i := uint64(0); i < ln; i++ {
			e, n, err := decodeValue(b[off:])
			if err != nil {
				return value.Null, 0, err
			}
			elems = append(elems, e)
			off += n
		}
		return value.Path(elems...), off, nil
	default:
		return value.Null, 0, fmt.Errorf("exec: bad spill value tag %d", t)
	}
}
