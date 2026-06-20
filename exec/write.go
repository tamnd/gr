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
