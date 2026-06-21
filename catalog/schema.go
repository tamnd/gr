package catalog

import (
	"errors"

	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/store"
)

// errBadCatalogRecord guards replay against a record tag the catalog does not
// know: a file written by a newer format reaching an older reader, or corruption.
var errBadCatalogRecord = errors.New("gr/catalog: unknown catalog record")

// ErrConstraintExists is returned by AddConstraint when a constraint of the same
// name is already declared, so a plain CREATE CONSTRAINT (no IF NOT EXISTS) fails
// loudly rather than silently shadowing the existing one.
var ErrConstraintExists = errors.New("gr/catalog: constraint already exists")

// ErrNoSuchConstraint is returned by DropConstraint when no constraint of the
// given name is declared, so a plain DROP CONSTRAINT (no IF EXISTS) fails.
var ErrNoSuchConstraint = errors.New("gr/catalog: no such constraint")

// The schema-record tags continue the dictionary tags (0,1,2) in the one catalog
// Log. They are part of the on-disk format and must not change (doc 08 §8.1).
const (
	// KindConstraintAdd records a constraint declaration.
	KindConstraintAdd Kind = 3
	// KindConstraintDrop records a constraint removal, a tombstone the replay
	// applies over an earlier add so the append-only Log can express a drop.
	KindConstraintDrop Kind = 4
)

// ConstraintKind classifies a constraint. The value is part of the on-disk
// constraint record and must not change.
type ConstraintKind uint8

const (
	// UniqueNode is a node uniqueness constraint: among the nodes carrying the
	// label, the property tuple is unique (nulls are exempt, doc 08 §4.1).
	UniqueNode ConstraintKind = 0
	// ExistsNode is a node existence constraint: every node carrying the label
	// must hold a present, non-null value for the property (doc 08 §4.1).
	ExistsNode ConstraintKind = 1
)

// Constraint is one schema constraint, recorded against a label and a property
// tuple. Props holds the constrained property-key tokens in declaration order;
// it has one entry for a single-property constraint and several for a composite
// one (doc 07 §5). Label and Props are catalog tokens, not names.
type Constraint struct {
	Name  string
	Kind  ConstraintKind
	Label uint32
	Props []uint32
}

// encodeConstraint appends a constraint's body (everything after the kind tag).
func encodeConstraint(dst []byte, c Constraint) []byte {
	dst = format.AppendString(dst, c.Name)
	dst = format.AppendUvarint(dst, uint64(c.Kind))
	dst = format.AppendUvarint(dst, uint64(c.Label))
	dst = format.AppendUvarint(dst, uint64(len(c.Props)))
	for _, p := range c.Props {
		dst = format.AppendUvarint(dst, uint64(p))
	}
	return dst
}

// decodeConstraint reads a constraint body and returns it with the bytes consumed.
func decodeConstraint(b []byte) (Constraint, int, error) {
	var c Constraint
	name, n, err := format.String(b)
	if err != nil {
		return c, 0, err
	}
	c.Name = name
	off := n
	kind, n, err := format.Uvarint(b[off:])
	if err != nil {
		return c, 0, err
	}
	c.Kind = ConstraintKind(kind)
	off += n
	label, n, err := format.Uvarint(b[off:])
	if err != nil {
		return c, 0, err
	}
	c.Label = uint32(label)
	off += n
	count, n, err := format.Uvarint(b[off:])
	if err != nil {
		return c, 0, err
	}
	off += n
	c.Props = make([]uint32, count)
	for i := range c.Props {
		p, n, err := format.Uvarint(b[off:])
		if err != nil {
			return c, 0, err
		}
		c.Props[i] = uint32(p)
		off += n
	}
	return c, off, nil
}

// applyConstraintAdd records a constraint in memory and counts the schema op. It
// is the shared body of replay and AddConstraint.
func (c *Catalog) applyConstraintAdd(con Constraint) {
	if _, ok := c.cons[con.Name]; !ok {
		c.conSeq = append(c.conSeq, con.Name)
	}
	c.cons[con.Name] = con
	c.schemaN++
}

// applyConstraintDrop removes a constraint in memory and counts the schema op.
func (c *Catalog) applyConstraintDrop(name string) {
	if _, ok := c.cons[name]; ok {
		delete(c.cons, name)
		for i, n := range c.conSeq {
			if n == name {
				c.conSeq = append(c.conSeq[:i], c.conSeq[i+1:]...)
				break
			}
		}
	}
	c.schemaN++
}

// AddConstraint declares a constraint, appending its record to the Log and
// recording it in memory. The append becomes durable when the enclosing
// transaction commits (it is rolled back with the rest on abort). It errors if a
// constraint of the same name already exists.
func (c *Catalog) AddConstraint(con Constraint) error {
	if _, ok := c.cons[con.Name]; ok {
		return ErrConstraintExists
	}
	rec := encodeConstraint([]byte{byte(KindConstraintAdd)}, con)
	if err := c.appendSchema(rec); err != nil {
		return err
	}
	c.applyConstraintAdd(con)
	return nil
}

// DropConstraint removes a constraint, appending a tombstone to the Log and
// removing it from memory. It errors if no constraint of the name is declared.
func (c *Catalog) DropConstraint(name string) error {
	if _, ok := c.cons[name]; !ok {
		return ErrNoSuchConstraint
	}
	rec := format.AppendString([]byte{byte(KindConstraintDrop)}, name)
	if err := c.appendSchema(rec); err != nil {
		return err
	}
	c.applyConstraintDrop(name)
	return nil
}

// appendSchema appends a schema record to the Log and keeps the section
// directory's recorded extent current, mirroring Intern's durability handling.
func (c *Catalog) appendSchema(rec []byte) error {
	if _, err := c.log.Append(rec); err != nil {
		return err
	}
	return c.secs.Set(store.SecCatalog, c.log.Head(), uint64(c.log.Len()))
}

// Constraints returns the declared constraints in add order. The slice is a fresh
// copy the caller may keep; the Props slices inside are shared (read-only).
func (c *Catalog) Constraints() []Constraint {
	out := make([]Constraint, 0, len(c.conSeq))
	for _, name := range c.conSeq {
		out = append(out, c.cons[name])
	}
	return out
}

// ConstraintByName returns the constraint of the given name, if declared.
func (c *Catalog) ConstraintByName(name string) (Constraint, bool) {
	con, ok := c.cons[name]
	return con, ok
}

// SchemaOps returns how many schema records the catalog has ever applied, a
// monotonic counter the engine folds into its catalog version so a constraint
// add or drop invalidates plans bound against the old schema (doc 14 §8.4).
func (c *Catalog) SchemaOps() uint64 { return c.schemaN }
