package engine

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/value"
)

// ConstraintError is the typed error a write raises when it would violate a
// declared constraint (doc 13 §12, doc 08 §4). It carries the constraint's name
// and the label, property, and offending value so the caller can report a clean
// diagnostic, and it aborts the transaction that hit it (doc 13 §16).
type ConstraintError struct {
	Constraint string
	Label      string
	Property   string
	Value      string
}

func (e *ConstraintError) Error() string {
	return fmt.Sprintf("gr/engine: uniqueness constraint %q violated: %s.%s already has value %s",
		e.Constraint, e.Label, e.Property, e.Value)
}

// uniqueKey is the comparison key for a uniqueness constraint: the value's type
// tag and its canonical text, so values of different types never collide (an
// integer 1 and a string \"1\" are distinct keys). Null values are exempt from
// uniqueness and are never keyed (doc 08 §4.1).
func uniqueKey(v value.Value) string {
	return strconv.Itoa(int(v.Type())) + ":" + v.String()
}

// autoConstraintName is the generated name of an unnamed constraint, derived from
// its kind, label, and property so a DROP CONSTRAINT can still address it and a
// repeat CREATE collides with itself (doc 08 §4.3).
func autoConstraintName(label, prop string) string {
	return "unique_" + label + "_" + prop
}

// CreateUniqueConstraint declares a node uniqueness constraint on label.prop in
// its own write transaction (doc 08 §6.1). It interns the label and property
// names, validates that the existing data already satisfies uniqueness (doc 08
// §6.4), records the constraint durably, and commits. It returns whether a
// constraint was added: false (with no error) when the constraint already exists
// and ifNotExists is set, true otherwise. Existing data that violates uniqueness
// fails the call with a [ConstraintError] and leaves the catalog unchanged.
func (e *DiskEngine) CreateUniqueConstraint(name, label, prop string, ifNotExists bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if name == "" {
		name = autoConstraintName(label, prop)
	}
	if _, exists := e.cat.ConstraintByName(name); exists {
		if ifNotExists {
			return false, nil
		}
		return false, catalog.ErrConstraintExists
	}
	// Validate the existing data before any mutation, against whatever tokens the
	// label and property already have. An un-interned label or property names no
	// stored data, so its constraint is vacuously satisfied.
	if lt, ok := e.cat.Lookup(catalog.KindLabel, label); ok {
		if pt, ok := e.cat.Lookup(catalog.KindPropKey, prop); ok {
			if err := e.checkUniqueData(name, lt, pt); err != nil {
				return false, err
			}
		}
	}
	lt, _, err := e.cat.Intern(catalog.KindLabel, label)
	if err != nil {
		return false, err
	}
	pt, _, err := e.cat.Intern(catalog.KindPropKey, prop)
	if err != nil {
		return false, err
	}
	if err := e.cat.AddConstraint(catalog.Constraint{
		Name:  name,
		Kind:  catalog.UniqueNode,
		Label: lt,
		Props: []uint32{pt},
	}); err != nil {
		return false, err
	}
	if _, err := e.commitPager(); err != nil {
		return false, err
	}
	return true, nil
}

// DropConstraint removes a constraint by name in its own write transaction. It
// returns whether a constraint was removed: false (no error) when none exists and
// ifExists is set, true otherwise; a plain drop of an absent constraint errors.
func (e *DiskEngine) DropConstraint(name string, ifExists bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.cat.ConstraintByName(name); !ok {
		if ifExists {
			return false, nil
		}
		return false, catalog.ErrNoSuchConstraint
	}
	if err := e.cat.DropConstraint(name); err != nil {
		return false, err
	}
	if _, err := e.commitPager(); err != nil {
		return false, err
	}
	return true, nil
}

// checkUniqueData scans the committed nodes carrying label and reports a
// [ConstraintError] if two of them share a non-null value for prop. It reads the
// base stores directly under the engine's held lock, so it sees the latest
// committed state (and, at commit time, the writer's own in-place writes). The
// tokens are catalog tokens.
func (e *DiskEngine) checkUniqueData(name string, label, prop uint32) error {
	seen := make(map[string]struct{})
	n := uint64(e.nodes.Count())
	for pos := uint64(0); pos < n; pos++ {
		if !e.nodes.Exists(pos) {
			continue
		}
		cats, err := e.nodes.Labels(pos)
		if err != nil {
			return err
		}
		if !slices.Contains(cats, label) {
			continue
		}
		v, ok, err := e.ncols.Get(prop, pos)
		if err != nil {
			return err
		}
		if !ok || v.IsNull() {
			continue
		}
		key := uniqueKey(v)
		if _, dup := seen[key]; dup {
			lname, _ := e.cat.Name(catalog.KindLabel, label)
			pname, _ := e.cat.Name(catalog.KindPropKey, prop)
			return &ConstraintError{Constraint: name, Label: lname, Property: pname, Value: v.String()}
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateUnique re-checks every declared uniqueness constraint against the
// writer's committed-plus-pending state, called from Commit before the pager is
// made durable. The whole-constraint rescan is the correctness-first form (doc 07
// §9): an incremental, index-backed check is the M4 refinement. With no declared
// constraints it is a single map lookup and returns immediately, so the common
// schemaless write pays nothing.
func (t *diskTx) validateUnique() error {
	for _, con := range t.e.cat.Constraints() {
		if con.Kind != catalog.UniqueNode || len(con.Props) != 1 {
			continue
		}
		if err := t.e.checkUniqueData(con.Name, con.Label, con.Props[0]); err != nil {
			return err
		}
	}
	return nil
}
