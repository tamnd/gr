package exec

import (
	"fmt"

	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
	"github.com/tamnd/gr/value"
)

// SideEffects counts the graph mutations a write query performed, the normative
// openCypher statistics a write returns (doc 13 §3.4). The executor's write
// operators increment these as they run; a read query leaves them all zero. SET to
// the same value still counts a property set; removing an absent property does not.
type SideEffects struct {
	NodesCreated       int
	NodesDeleted       int
	RelsCreated        int
	RelsDeleted        int
	PropertiesSet      int
	LabelsAdded        int
	LabelsRemoved      int
	IndexesAdded       int
	IndexesRemoved     int
	ConstraintsAdded   int
	ConstraintsRemoved int
}

// ContainsUpdates reports whether any mutation occurred, the openCypher
// "contains updates" flag.
func (s SideEffects) ContainsUpdates() bool {
	return s.NodesCreated > 0 || s.NodesDeleted > 0 || s.RelsCreated > 0 ||
		s.RelsDeleted > 0 || s.PropertiesSet > 0 || s.LabelsAdded > 0 ||
		s.LabelsRemoved > 0 || s.IndexesAdded > 0 || s.IndexesRemoved > 0 ||
		s.ConstraintsAdded > 0 || s.ConstraintsRemoved > 0
}

// createOp executes a CREATE clause (doc 13 §5). For each input row it creates the
// clause's new nodes and relationships through the write SPI, evaluates and sets
// their properties, binds each new element into the row, and yields the augmented
// row. Nodes are created before relationships so every endpoint exists when its
// relationship is built; a relationship endpoint that is bound but not a node (an
// unmatched OPTIONAL variable) is an error.
type createOp struct {
	spec  *plan.Create
	input operator
	ctx   *Ctx
}

func (o *createOp) open(ctx *Ctx) error {
	o.ctx = ctx
	return o.input.open(ctx)
}

func (o *createOp) next() (eval.Row, bool, error) {
	in, ok, err := o.input.next()
	if err != nil || !ok {
		return nil, false, err
	}
	row := cloneRow(in)
	for _, nc := range o.spec.Nodes {
		if err := o.createNode(nc, row); err != nil {
			return nil, false, err
		}
	}
	for _, rc := range o.spec.Rels {
		if err := o.createRel(rc, row); err != nil {
			return nil, false, err
		}
	}
	return row, true, nil
}

func (o *createOp) createNode(nc plan.NodeCreate, row eval.Row) error {
	labels := knownTokens(nc.Labels)
	id, err := o.ctx.Tx.CreateNode(labels)
	if err != nil {
		return err
	}
	row[nc.Var] = value.Node(uint64(id))
	o.ctx.Effects.NodesCreated++
	o.ctx.Effects.LabelsAdded += len(labels)
	return o.setProps(nc.Props, row, func(key engine.Token, v value.Value) error {
		return o.ctx.Tx.SetNodeProperty(id, key, v)
	})
}

func (o *createOp) createRel(rc plan.RelCreate, row eval.Row) error {
	src, ok := row[rc.From].AsNode()
	if !ok {
		return fmt.Errorf("exec: CREATE relationship source %q is not a node", rc.From)
	}
	dst, ok := row[rc.To].AsNode()
	if !ok {
		return fmt.Errorf("exec: CREATE relationship target %q is not a node", rc.To)
	}
	if !rc.Type.Known {
		return fmt.Errorf("exec: CREATE relationship type is unresolved")
	}
	id, err := o.ctx.Tx.CreateRel(engine.NodeID(src), engine.NodeID(dst), rc.Type.Token)
	if err != nil {
		return err
	}
	row[rc.Var] = value.Rel(uint64(id))
	o.ctx.Effects.RelsCreated++
	return o.setProps(rc.Props, row, func(key engine.Token, v value.Value) error {
		return o.ctx.Tx.SetRelProperty(id, key, v)
	})
}

// setProps evaluates each property expression against the current row and applies
// it through set. A value that evaluates to null leaves the property unset and is
// not counted (doc 13 §5.4).
func (o *createOp) setProps(props []plan.PropSet, row eval.Row, set func(engine.Token, value.Value) error) error {
	for _, p := range props {
		v, err := eval.Eval(p.Expr, o.ctx.env(row))
		if err != nil {
			return err
		}
		if v.IsNull() {
			continue
		}
		if !p.Key.Known {
			return fmt.Errorf("exec: CREATE property key is unresolved")
		}
		if err := set(p.Key.Token, v); err != nil {
			return err
		}
		o.ctx.Effects.PropertiesSet++
	}
	return nil
}

func (o *createOp) close() error { return o.input.close() }

// setOp executes a SET clause (doc 13 §6). For each input row it applies every
// update item to the bound element in order and passes the row on unchanged (SET
// binds nothing new). A property assignment to a non-null value always counts a
// property set, even when the value is unchanged (doc 13 §6.22); a property
// assigned null is removed and counts only when the property was present, the
// same rule REMOVE follows. A label is added only when the node does not already
// carry it, so only net additions count (doc 13 §6.7).
type setOp struct {
	spec  *plan.Set
	input operator
	ctx   *Ctx
}

func (o *setOp) open(ctx *Ctx) error {
	o.ctx = ctx
	return o.input.open(ctx)
}

func (o *setOp) next() (eval.Row, bool, error) {
	in, ok, err := o.input.next()
	if err != nil || !ok {
		return nil, false, err
	}
	for _, it := range o.spec.Items {
		switch it.Kind {
		case plan.SetItemProp:
			if err := o.setProp(it, in); err != nil {
				return nil, false, err
			}
		case plan.SetItemLabels:
			if err := o.setLabels(it, in); err != nil {
				return nil, false, err
			}
		case plan.SetItemMerge:
			if err := o.setMap(it, in, false); err != nil {
				return nil, false, err
			}
		case plan.SetItemReplace:
			if err := o.setMap(it, in, true); err != nil {
				return nil, false, err
			}
		}
	}
	return in, true, nil
}

func (o *setOp) setProp(it plan.SetItem, row eval.Row) error {
	v, err := eval.Eval(it.Expr, o.ctx.env(row))
	if err != nil {
		return err
	}
	if v.IsNull() {
		return removeElementProp(o.ctx, row[it.Var], it.Key)
	}
	if !it.Key.Known {
		return fmt.Errorf("exec: SET property key is unresolved")
	}
	return setElementProp(o.ctx, row[it.Var], it.Key.Token, v)
}

func (o *setOp) setLabels(it plan.SetItem, row eval.Row) error {
	id, ok := row[it.Var].AsNode()
	if !ok {
		return fmt.Errorf("exec: SET label target %q is not a node", it.Var)
	}
	for _, l := range it.Labels {
		if !l.Known {
			return fmt.Errorf("exec: SET label is unresolved")
		}
		has, err := o.ctx.Tx.HasLabel(engine.NodeID(id), l.Token)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if err := o.ctx.Tx.AddLabel(engine.NodeID(id), l.Token); err != nil {
			return err
		}
		o.ctx.Effects.LabelsAdded++
	}
	return nil
}

// setMap applies a map-form SET: n += m (merge) or n = m (replace). Each of the
// map's keys is set on the target, with a key set to null removing that property
// (the null-is-removal rule single SET follows, doc 13 §6.4). For replace, every
// property the target currently carries whose key is not in the map is removed
// first, so the target ends with exactly the map's properties (doc 13 §6.5). The
// right-hand side may be a literal or parameter map, or another bound element
// whose properties are read and copied (doc 13 §6.15). A null right-hand side
// clears the target on replace and is a no-op on merge.
func (o *setOp) setMap(it plan.SetItem, row eval.Row, replace bool) error {
	v, err := eval.Eval(it.Expr, o.ctx.env(row))
	if err != nil {
		return err
	}
	target := row[it.Var]
	if !isNode(target) && !isRel(target) {
		return fmt.Errorf("exec: SET of properties applies only to a node or relationship")
	}
	if v.IsNull() {
		if replace {
			return o.clearProps(target)
		}
		return nil
	}
	pairs, err := o.mapPairs(v)
	if err != nil {
		return err
	}
	if replace {
		if err := o.removeAbsent(target, pairs); err != nil {
			return err
		}
	}
	for _, p := range pairs {
		if p.val.IsNull() {
			if err := removeElementProp(o.ctx, target, bind.NameRef{Known: true, Token: p.key}); err != nil {
				return err
			}
			continue
		}
		if err := setElementProp(o.ctx, target, p.key, p.val); err != nil {
			return err
		}
	}
	return nil
}

// propPair is one property entry to apply in a map-form SET, the key already
// resolved to its token.
type propPair struct {
	key engine.Token
	val value.Value
}

// mapPairs turns a map-form SET right-hand side into the property entries to
// apply. A map value interns each of its keys in this transaction (the keys are
// only known now, doc 13 §6.4); an element value reads the source element's
// current properties under the writer's snapshot (read-your-writes, doc 13 §6.15).
func (o *setOp) mapPairs(v value.Value) ([]propPair, error) {
	if m, ok := v.AsMap(); ok {
		pairs := make([]propPair, 0, len(m))
		for name, val := range m {
			tok, err := o.ctx.Tx.InternPropKey(name)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, propPair{key: tok, val: val})
		}
		return pairs, nil
	}
	if isNode(v) || isRel(v) {
		return o.elementProps(v)
	}
	return nil, fmt.Errorf("exec: SET of properties requires a map or a node or relationship")
}

// elementProps reads every property a source element carries, as the entries to
// copy onto the target (doc 13 §6.15). The source keys already exist as tokens,
// so no interning is needed.
func (o *setOp) elementProps(v value.Value) ([]propPair, error) {
	keys, err := o.currentKeys(v)
	if err != nil {
		return nil, err
	}
	pairs := make([]propPair, 0, len(keys))
	for _, k := range keys {
		val, err := o.readProp(v, k)
		if err != nil {
			return nil, err
		}
		if val.IsNull() {
			continue
		}
		pairs = append(pairs, propPair{key: k, val: val})
	}
	return pairs, nil
}

// removeAbsent removes every property the target currently carries whose key is
// not among the replacement entries, the first half of map-replace (doc 13 §6.5).
func (o *setOp) removeAbsent(target value.Value, pairs []propPair) error {
	keep := make(map[engine.Token]struct{}, len(pairs))
	for _, p := range pairs {
		keep[p.key] = struct{}{}
	}
	cur, err := o.currentKeys(target)
	if err != nil {
		return err
	}
	for _, k := range cur {
		if _, ok := keep[k]; ok {
			continue
		}
		if err := removeElementProp(o.ctx, target, bind.NameRef{Known: true, Token: k}); err != nil {
			return err
		}
	}
	return nil
}

// clearProps removes every property the target carries, the effect of SET n = null.
func (o *setOp) clearProps(target value.Value) error {
	cur, err := o.currentKeys(target)
	if err != nil {
		return err
	}
	for _, k := range cur {
		if err := removeElementProp(o.ctx, target, bind.NameRef{Known: true, Token: k}); err != nil {
			return err
		}
	}
	return nil
}

// currentKeys returns the property keys the node or relationship currently carries.
func (o *setOp) currentKeys(elem value.Value) ([]engine.Token, error) {
	if id, ok := elem.AsNode(); ok {
		return o.ctx.Tx.NodePropertyKeys(engine.NodeID(id))
	}
	id, _ := elem.AsRel()
	return o.ctx.Tx.RelPropertyKeys(engine.RelID(id))
}

// readProp reads one property of the node or relationship bound to an element value.
func (o *setOp) readProp(elem value.Value, key engine.Token) (value.Value, error) {
	if id, ok := elem.AsNode(); ok {
		return o.ctx.Tx.NodeProperty(engine.NodeID(id), key)
	}
	id, _ := elem.AsRel()
	return o.ctx.Tx.RelProperty(engine.RelID(id), key)
}

func (o *setOp) close() error { return o.input.close() }

// removeOp executes a REMOVE clause (doc 13 §7). For each input row it removes
// every item's property or labels from the bound element and passes the row on
// unchanged. A property removal counts only when the property was present (doc 13
// §7.11), folded into PropertiesSet as Neo4j does; a label removal counts only
// when the node carried it (doc 13 §7.3). An unknown label or key names nothing
// in the catalog, so it is a no-op that counts nothing.
type removeOp struct {
	spec  *plan.Remove
	input operator
	ctx   *Ctx
}

func (o *removeOp) open(ctx *Ctx) error {
	o.ctx = ctx
	return o.input.open(ctx)
}

func (o *removeOp) next() (eval.Row, bool, error) {
	in, ok, err := o.input.next()
	if err != nil || !ok {
		return nil, false, err
	}
	for _, it := range o.spec.Items {
		if len(it.Labels) > 0 {
			if err := o.removeLabels(it, in); err != nil {
				return nil, false, err
			}
			continue
		}
		if err := removeElementProp(o.ctx, in[it.Var], it.Key); err != nil {
			return nil, false, err
		}
	}
	return in, true, nil
}

func (o *removeOp) removeLabels(it plan.RemoveItem, row eval.Row) error {
	id, ok := row[it.Var].AsNode()
	if !ok {
		return fmt.Errorf("exec: REMOVE label target %q is not a node", it.Var)
	}
	for _, l := range it.Labels {
		if !l.Known {
			continue // an unknown label names no node: nothing to remove
		}
		has, err := o.ctx.Tx.HasLabel(engine.NodeID(id), l.Token)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		if err := o.ctx.Tx.RemoveLabel(engine.NodeID(id), l.Token); err != nil {
			return err
		}
		o.ctx.Effects.LabelsRemoved++
	}
	return nil
}

func (o *removeOp) close() error { return o.input.close() }

// deleteOp executes a DELETE or DETACH DELETE clause (doc 13 §9). Within one row
// it removes relationships before nodes so a plain DELETE that lists both an edge
// and its endpoint does not trip the no-dangling check (doc 13 §9.12). A target
// that evaluates to null deletes nothing. Deleting an element already gone is a
// no-op and is not counted again (doc 13 §9.6). DETACH removes a node's
// relationships first, then the node (doc 13 §9.5).
//
// The operator buffers its whole input before applying any delete. This is the
// Eager barrier doc 13 §9.9 calls for: a lazy scan that kept pulling rows while
// the delete tombstoned nodes would advance onto a deleted node and fault, so the
// read must finish before the first delete. The planner will grow a general Eager
// pass for the write path later (doc 11 §10); until then the barrier lives here.
type deleteOp struct {
	spec    *plan.Delete
	input   operator
	ctx     *Ctx
	buf     []eval.Row
	pos     int
	applied bool
}

func (o *deleteOp) open(ctx *Ctx) error {
	o.ctx = ctx
	o.buf = nil
	o.pos = 0
	o.applied = false
	return o.input.open(ctx)
}

func (o *deleteOp) next() (eval.Row, bool, error) {
	if !o.applied {
		if err := o.drainAndDelete(); err != nil {
			return nil, false, err
		}
		o.applied = true
	}
	if o.pos >= len(o.buf) {
		return nil, false, nil
	}
	row := o.buf[o.pos]
	o.pos++
	return row, true, nil
}

// drainAndDelete reads the entire input into the buffer, then applies the deletes
// row by row. Reading first is the Eager barrier (see the type comment).
func (o *deleteOp) drainAndDelete() error {
	for {
		in, ok, err := o.input.next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		o.buf = append(o.buf, in)
	}
	for _, row := range o.buf {
		if err := o.deleteRow(row); err != nil {
			return err
		}
	}
	return nil
}

// deleteRow evaluates one row's targets and removes them, relationships before
// nodes (doc 13 §9.12).
func (o *deleteOp) deleteRow(row eval.Row) error {
	var nodes, rels []uint64
	for _, t := range o.spec.Targets {
		v, err := eval.Eval(t, o.ctx.env(row))
		if err != nil {
			return err
		}
		if v.IsNull() {
			continue
		}
		if id, ok := v.AsRel(); ok {
			rels = append(rels, id)
			continue
		}
		if id, ok := v.AsNode(); ok {
			nodes = append(nodes, id)
			continue
		}
		return fmt.Errorf("exec: DELETE target is not a node or relationship")
	}
	for _, id := range rels {
		if err := o.deleteRel(id); err != nil {
			return err
		}
	}
	for _, id := range nodes {
		if err := o.deleteNode(id); err != nil {
			return err
		}
	}
	return nil
}

// deleteRel removes a relationship, skipping one already gone so a re-delete is a
// harmless no-op (doc 13 §9.6).
func (o *deleteOp) deleteRel(id uint64) error {
	live, err := o.ctx.Tx.RelExists(engine.RelID(id))
	if err != nil {
		return err
	}
	if !live {
		return nil
	}
	if err := o.ctx.Tx.DeleteRel(engine.RelID(id)); err != nil {
		return err
	}
	o.ctx.Effects.RelsDeleted++
	return nil
}

// deleteNode removes a node. A plain DELETE leaves the engine's no-dangling check
// to refuse a still-attached node; DETACH first removes every incident
// relationship (doc 13 §9.5), then the node. A node already gone is a no-op (doc
// 13 §9.6).
func (o *deleteOp) deleteNode(id uint64) error {
	live, err := o.ctx.Tx.NodeExists(engine.NodeID(id))
	if err != nil {
		return err
	}
	if !live {
		return nil
	}
	if o.spec.Detach {
		if err := o.detach(id); err != nil {
			return err
		}
	}
	if err := o.ctx.Tx.DeleteNode(engine.NodeID(id)); err != nil {
		return err
	}
	o.ctx.Effects.NodesDeleted++
	return nil
}

// detach removes every relationship incident to a node, both directions and all
// types, before the node itself is deleted (doc 13 §9.5). The incident edges are
// collected first, then deleted; deleteRel skips any seen twice (a self-loop
// reached from both ends) or removed earlier.
func (o *deleteOp) detach(id uint64) error {
	var inc []uint64
	err := o.ctx.Tx.Expand(engine.NodeID(id), 0, engine.Both, func(nb engine.Neighbor) error {
		inc = append(inc, uint64(nb.Rel))
		return nil
	})
	if err != nil {
		return err
	}
	for _, rid := range inc {
		if err := o.deleteRel(rid); err != nil {
			return err
		}
	}
	return nil
}

func (o *deleteOp) close() error { return o.input.close() }

// setElementProp sets a non-null property on the node or relationship bound to an
// element value and counts it. SET to the same value still counts (doc 13 §6.22).
func setElementProp(ctx *Ctx, elem value.Value, key engine.Token, v value.Value) error {
	switch {
	case isNode(elem):
		id, _ := elem.AsNode()
		if err := ctx.Tx.SetNodeProperty(engine.NodeID(id), key, v); err != nil {
			return err
		}
	case isRel(elem):
		id, _ := elem.AsRel()
		if err := ctx.Tx.SetRelProperty(engine.RelID(id), key, v); err != nil {
			return err
		}
	default:
		return fmt.Errorf("exec: SET property target is not a node or relationship")
	}
	ctx.Effects.PropertiesSet++
	return nil
}

// removeElementProp removes a property from the node or relationship bound to an
// element value, counting only when the property was actually present (doc 13
// §7.11). An unknown key names no stored property, so it is a counted-as-nothing
// no-op. Removal is a SET to null through the same SPI.
func removeElementProp(ctx *Ctx, elem value.Value, key bind.NameRef) error {
	if !key.Known {
		return nil
	}
	switch {
	case isNode(elem):
		id, _ := elem.AsNode()
		cur, err := ctx.Tx.NodeProperty(engine.NodeID(id), key.Token)
		if err != nil {
			return err
		}
		if cur.IsNull() {
			return nil
		}
		if err := ctx.Tx.SetNodeProperty(engine.NodeID(id), key.Token, value.Null); err != nil {
			return err
		}
	case isRel(elem):
		id, _ := elem.AsRel()
		cur, err := ctx.Tx.RelProperty(engine.RelID(id), key.Token)
		if err != nil {
			return err
		}
		if cur.IsNull() {
			return nil
		}
		if err := ctx.Tx.SetRelProperty(engine.RelID(id), key.Token, value.Null); err != nil {
			return err
		}
	default:
		return fmt.Errorf("exec: REMOVE property target is not a node or relationship")
	}
	ctx.Effects.PropertiesSet++
	return nil
}

func isNode(v value.Value) bool { _, ok := v.AsNode(); return ok }
func isRel(v value.Value) bool  { _, ok := v.AsRel(); return ok }

// knownTokens returns the resolved tokens of a name set, skipping any that are
// unresolved. CREATE interns its names before binding, so in practice all are
// known; the guard keeps a stray sentinel from creating a zero (wildcard) token.
func knownTokens(refs []bind.NameRef) []engine.Token {
	out := make([]engine.Token, 0, len(refs))
	for _, r := range refs {
		if r.Known {
			out = append(out, r.Token)
		}
	}
	return out
}
