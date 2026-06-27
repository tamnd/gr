package engine

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/value"
)

// ConstraintError is the typed error a write raises when it would violate a
// declared constraint (doc 13 §12, doc 08 §4). It carries the constraint's kind
// and name and the label and property so the caller can report a clean diagnostic,
// and it aborts the transaction that hit it (doc 13 §16). Value holds the
// offending value for a uniqueness violation; it is empty for an existence
// violation, where the problem is the absence of a value. Want and Got hold the
// declared and actual type names for a property-type violation.
type ConstraintError struct {
	Kind       catalog.ConstraintKind
	Constraint string
	Label      string
	Property   string
	Value      string
	Want       string
	Got        string
}

func (e *ConstraintError) Error() string {
	switch e.Kind {
	case catalog.ExistsNode:
		return fmt.Sprintf("gr/engine: existence constraint %q violated: %s.%s is missing",
			e.Constraint, e.Label, e.Property)
	case catalog.TypedNode:
		return fmt.Sprintf("gr/engine: type constraint %q violated: %s.%s requires %s but has %s",
			e.Constraint, e.Label, e.Property, e.Want, e.Got)
	default:
		return fmt.Sprintf("gr/engine: uniqueness constraint %q violated: %s.%s already has value %s",
			e.Constraint, e.Label, e.Property, e.Value)
	}
}

// ConstraintObserver receives one report per constraint check the commit path runs (doc 20 §6.4),
// so a higher layer can count enforcement without the engine importing a metric registry. kind is
// the constraint class (unique, exists, or type) and ok is whether the check passed. A violation
// reports ok=false for the offending constraint and then aborts, so the failing check is counted
// before the transaction unwinds.
type ConstraintObserver interface {
	ConstraintCheck(kind string, ok bool)
}

// SetConstraintObserver installs the observer the commit path reports constraint checks to (doc 20
// §6.4). It is set once at Open and not meant to change under concurrent commits.
func (e *DiskEngine) SetConstraintObserver(o ConstraintObserver) { e.conObs = o }

// reportConstraint reports one constraint check to the observer if one is set (doc 20 §6.4). It is
// nil-safe so the validate functions call it unconditionally and a database with no observer pays
// nothing.
func (e *DiskEngine) reportConstraint(kind string, ok bool) {
	if e.conObs != nil {
		e.conObs.ConstraintCheck(kind, ok)
	}
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
// repeat CREATE collides with itself (doc 08 §4.3). The kind prefix keeps a
// uniqueness and an existence constraint on the same label and property from
// colliding on one generated name.
func autoConstraintName(prefix, label, prop string) string {
	return prefix + "_" + label + "_" + prop
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
	e.p.BeginWrite()
	defer e.p.EndWrite()
	if name == "" {
		name = autoConstraintName("unique", label, prop)
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

// CreateExistenceConstraint declares a node existence constraint on label.prop in
// its own write transaction (doc 08 §6.1). It mirrors CreateUniqueConstraint: it
// interns the label and property names, validates that the existing data already
// satisfies existence (every label-node carries a non-null value for the property,
// doc 08 §6.4), records the constraint durably, and commits. It returns whether a
// constraint was added: false (no error) when the constraint already exists and
// ifNotExists is set, true otherwise. Existing data that violates existence fails
// the call with a [ConstraintError] and leaves the catalog unchanged.
func (e *DiskEngine) CreateExistenceConstraint(name, label, prop string, ifNotExists bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.p.BeginWrite()
	defer e.p.EndWrite()
	if name == "" {
		name = autoConstraintName("exists", label, prop)
	}
	if _, exists := e.cat.ConstraintByName(name); exists {
		if ifNotExists {
			return false, nil
		}
		return false, catalog.ErrConstraintExists
	}
	// Validate the existing data first, against whatever tokens the label and
	// property already have. An un-interned label names no stored node, so the
	// constraint is vacuously satisfied; an un-interned property, with a stored
	// label, means no label-node carries the property, so a non-empty label group
	// violates existence. checkExistenceData handles both by reading the column,
	// which returns absent for an un-interned key.
	if lt, ok := e.cat.Lookup(catalog.KindLabel, label); ok {
		pt, pok := e.cat.Lookup(catalog.KindPropKey, prop)
		if err := e.checkExistenceData(name, prop, lt, pt, pok); err != nil {
			return false, err
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
		Kind:  catalog.ExistsNode,
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

// CreateTypeConstraint declares a node property-type constraint on label.prop in
// its own write transaction (doc 08 §6.1). It mirrors CreateExistenceConstraint: it
// interns the label and property names, validates that the existing data already
// satisfies the type (every present, non-null value for the property on a label-node
// is of the declared type, doc 08 §4.1), records the constraint durably, and
// commits. It returns whether a constraint was added: false (no error) when the
// constraint already exists and ifNotExists is set, true otherwise. Existing data
// that violates the type fails the call with a [ConstraintError] and leaves the
// catalog unchanged.
func (e *DiskEngine) CreateTypeConstraint(name, label, prop string, vt value.Type, ifNotExists bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.p.BeginWrite()
	defer e.p.EndWrite()
	if name == "" {
		name = autoConstraintName("type", label, prop)
	}
	if _, exists := e.cat.ConstraintByName(name); exists {
		if ifNotExists {
			return false, nil
		}
		return false, catalog.ErrConstraintExists
	}
	// Validate the existing data first. An un-interned label or property names no
	// stored value of the property, so the constraint is vacuously satisfied: a type
	// constraint only restricts values that are present, unlike existence.
	if lt, ok := e.cat.Lookup(catalog.KindLabel, label); ok {
		if pt, ok := e.cat.Lookup(catalog.KindPropKey, prop); ok {
			if err := e.checkTypeData(name, lt, pt, vt); err != nil {
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
		Name:      name,
		Kind:      catalog.TypedNode,
		Label:     lt,
		Props:     []uint32{pt},
		ValueType: uint8(vt),
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
	e.p.BeginWrite()
	defer e.p.EndWrite()
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
		v, ok, err := e.baseNodeProp(prop, pos)
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
		err := t.e.checkUniqueData(con.Name, con.Label, con.Props[0])
		t.e.reportConstraint("unique", err == nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// checkExistenceData scans the committed nodes carrying label and reports a
// [ConstraintError] if any of them lacks a present, non-null value for prop. It
// reads the base stores directly under the engine's held lock, so it sees the
// latest committed state (and, at commit time, the writer's own in-place writes).
// propInterned says whether the property key exists in the catalog: when it does
// not, no node carries the property, so any node in the label group violates the
// constraint, and the column is not read (catalog token 0 is a valid key, so a
// blind read of an un-interned token would alias the wrong column). propName names
// the property for the diagnostic, since an un-interned token has no catalog name.
// The tokens are catalog tokens.
func (e *DiskEngine) checkExistenceData(name, propName string, label, prop uint32, propInterned bool) error {
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
		present := false
		if propInterned {
			v, ok, err := e.baseNodeProp(prop, pos)
			if err != nil {
				return err
			}
			present = ok && !v.IsNull()
		}
		if !present {
			lname, _ := e.cat.Name(catalog.KindLabel, label)
			return &ConstraintError{Kind: catalog.ExistsNode, Constraint: name, Label: lname, Property: propName}
		}
	}
	return nil
}

// validateExistence re-checks every declared existence constraint against the
// writer's committed-plus-pending state, called from Commit alongside
// validateUnique. Like the uniqueness check it is a correctness-first whole-group
// rescan (doc 07 §9); an incremental check is the M4 refinement. A declared
// constraint's property is always interned, so propInterned is true here.
func (t *diskTx) validateExistence() error {
	for _, con := range t.e.cat.Constraints() {
		if con.Kind != catalog.ExistsNode || len(con.Props) != 1 {
			continue
		}
		pname, _ := t.e.cat.Name(catalog.KindPropKey, con.Props[0])
		err := t.e.checkExistenceData(con.Name, pname, con.Label, con.Props[0], true)
		t.e.reportConstraint("exists", err == nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// checkTypeData scans the committed nodes carrying label and reports a
// [ConstraintError] if any present, non-null value for prop is not of the declared
// type vt. It reads the base stores directly under the engine's held lock, so it
// sees the latest committed state (and, at commit time, the writer's own in-place
// writes). A type constraint restricts only values that are present: an absent or
// null property is allowed, the same exemption uniqueness grants and the opposite of
// existence. The tokens are catalog tokens.
func (e *DiskEngine) checkTypeData(name string, label, prop uint32, vt value.Type) error {
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
		v, ok, err := e.baseNodeProp(prop, pos)
		if err != nil {
			return err
		}
		if !ok || v.IsNull() {
			continue
		}
		if v.Type() != vt {
			lname, _ := e.cat.Name(catalog.KindLabel, label)
			pname, _ := e.cat.Name(catalog.KindPropKey, prop)
			return &ConstraintError{
				Kind: catalog.TypedNode, Constraint: name, Label: lname, Property: pname,
				Want: vt.String(), Got: v.Type().String(),
			}
		}
	}
	return nil
}

// validateType re-checks every declared property-type constraint against the
// writer's committed-plus-pending state, called from Commit alongside validateUnique
// and validateExistence. Like the other checks it is a correctness-first whole-group
// rescan (doc 07 §9); an incremental check is the M4 refinement.
func (t *diskTx) validateType() error {
	for _, con := range t.e.cat.Constraints() {
		if con.Kind != catalog.TypedNode || len(con.Props) != 1 {
			continue
		}
		err := t.e.checkTypeData(con.Name, con.Label, con.Props[0], value.Type(con.ValueType))
		t.e.reportConstraint("type", err == nil)
		if err != nil {
			return err
		}
	}
	return nil
}
