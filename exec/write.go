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
