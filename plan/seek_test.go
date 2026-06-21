package plan

import "testing"

// fakeIndex is a fixed set of declared (label, prop) indexes, the planner's
// IndexLookup seam.
type fakeIndex map[[2]uint32]bool

func (f fakeIndex) HasNodeIndex(label, prop uint32) bool { return f[[2]uint32{label, prop}] }

// withIndex declares an index on the given catalog tokens.
func withIndex(pairs ...[2]uint32) fakeIndex {
	ix := fakeIndex{}
	for _, p := range pairs {
		ix[p] = true
	}
	return ix
}

func TestSeekRewriteEquality(t *testing.T) {
	b := bound(t, "MATCH (p:Person) WHERE p.name = 'x' RETURN p")
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{1, 1}))
	eq(t, "seek", String(op), `Project p
  Filter p.name = "x"
    NodeIndexSeek p:#1(#1 = "x")
`)
}

// TestSeekRewritePropertyMap confirms a pattern property map, which the binder
// lowers to an equality filter, also drives the seek.
func TestSeekRewritePropertyMap(t *testing.T) {
	b := bound(t, "MATCH (p:Person {name: 'x'}) RETURN p")
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{1, 1}))
	eq(t, "seek", String(op), `Project p
  Filter p.name = "x"
    NodeIndexSeek p:#1(#1 = "x")
`)
}

// TestSeekRewriteParam confirms a parameter value is a constant the seek accepts.
func TestSeekRewriteParam(t *testing.T) {
	b := bound(t, "MATCH (p:Person) WHERE p.name = $n RETURN p")
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{1, 1}))
	eq(t, "seek", String(op), `Project p
  Filter p.name = $n
    NodeIndexSeek p:#1(#1 = $n)
`)
}

// TestSeekRewriteNoIndex leaves the scan in place when no index is declared.
func TestSeekRewriteNoIndex(t *testing.T) {
	b := bound(t, "MATCH (p:Person) WHERE p.name = 'x' RETURN p")
	want := String(Plan(b))
	op := SeekRewrite(Plan(b), b, withIndex())
	eq(t, "unchanged", String(op), want)
}

// TestSeekRewriteWrongProp leaves the scan in place when the index is on a
// different property than the one the equality pins.
func TestSeekRewriteWrongProp(t *testing.T) {
	b := bound(t, "MATCH (p:Person) WHERE p.name = 'x' RETURN p")
	want := String(Plan(b))
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{1, 2})) // index on Person.age
	eq(t, "unchanged", String(op), want)
}

// TestSeekRewriteRangeNotPinned leaves a range predicate alone: only equality is
// an index access path here.
func TestSeekRewriteRangeNotPinned(t *testing.T) {
	b := bound(t, "MATCH (p:Person) WHERE p.age > 30 RETURN p")
	want := String(Plan(b))
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{1, 2}))
	eq(t, "unchanged", String(op), want)
}

// TestSeekRewriteResidualLabel keeps a second required label as a residual on the
// seek, seeking on the indexed label.
func TestSeekRewriteResidualLabel(t *testing.T) {
	b := bound(t, "MATCH (p:Person:Movie) WHERE p.name = 'x' RETURN p")
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{2, 1})) // index on Movie.name
	eq(t, "seek", String(op), `Project p
  Filter p.name = "x"
    NodeIndexSeek p:#2&#1(#1 = "x")
`)
}

// TestSeekRewriteUnlabeled leaves an unlabeled scan alone: an index is declared
// per label, so there is nothing to seek on.
func TestSeekRewriteUnlabeled(t *testing.T) {
	b := bound(t, "MATCH (p) WHERE p.name = 'x' RETURN p")
	want := String(Plan(b))
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{1, 1}))
	eq(t, "unchanged", String(op), want)
}

// TestSeekRewriteBehindExpand rewrites the scan that an equality pins even when it
// sits below an expand, since pushdown places the equality directly above it.
func TestSeekRewriteBehindExpand(t *testing.T) {
	b := bound(t, "MATCH (p:Person)-[:KNOWS]->(f) WHERE p.name = 'x' RETURN f")
	op := SeekRewrite(Plan(b), b, withIndex([2]uint32{1, 1}))
	eq(t, "seek", String(op), `Project f
  Expand p -[@r0:#1]-> f
    Filter p.name = "x"
      NodeIndexSeek p:#1(#1 = "x")
`)
}
