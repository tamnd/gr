package plan

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/bind"
)

// IndexLookup answers whether a property index is available as an access path,
// in the engine's SPI token space (doc 11 §6). SeekRewrite asks it once per
// candidate label and property; *engine.DiskEngine satisfies it through its
// HasNodeIndex method, which the compile path adapts.
type IndexLookup interface {
	// HasNodeIndex reports whether a single-property node index is declared on the
	// label and property keys.
	HasNodeIndex(label, prop uint32) bool
}

// SeekRewrite rewrites label scans that sit under an equality predicate into
// index seeks, where a declared property index serves the lookup (doc 11 §6). It
// runs after Plan, so it sees the canonical tree the binder, builder, and
// normalizer produced: predicate pushdown has already placed each property
// equality directly above the NodeScan for its variable, so a seekable shape is a
// filter chain bottoming in a labeled NodeScan whose variable an equality pins to
// a constant on an indexed property.
//
// The rewrite is meaning-preserving. It replaces only the NodeScan leaf with a
// NodeIndexSeek and leaves every filter in the chain on top untouched, including
// the equality that triggered it. The seek yields a superset of the rows the
// scan would have produced for that equality (the executor widens the probe to
// cover Cypher's cross-type numeric equality and falls back to a full label scan
// for a value it cannot key), and the retained filter chain trims that superset
// to exactly the original result. A tree with no index available, or no eligible
// equality, is returned unchanged.
func SeekRewrite(o Op, b *bind.Bound, ix IndexLookup) Op {
	if ix == nil || b == nil {
		return o
	}
	var rewrite func(Op) Op
	rewrite = func(n Op) Op {
		if seek, ok := trySeek(n, b, ix); ok {
			return seek
		}
		return mapChildren(n, rewrite)
	}
	return rewrite(o)
}

// trySeek attempts to rewrite a filter chain over a labeled NodeScan into the
// same filter chain over a NodeIndexSeek. It returns the rewritten chain and true
// when an eligible equality and a matching index are found, and false otherwise
// (leaving the caller to recurse structurally). The whole chain is returned, so
// the caller does not recurse into it again.
func trySeek(o Op, b *bind.Bound, ix IndexLookup) (Op, bool) {
	var preds []ast.Expr
	cur := o
	for {
		f, ok := cur.(*Filter)
		if !ok {
			break
		}
		preds = append(preds, f.Pred)
		cur = f.Input
	}
	scan, ok := cur.(*NodeScan)
	if !ok || len(scan.Labels) == 0 {
		return nil, false
	}
	for _, p := range preds {
		key, val, ok := eqOnVar(p, scan.Var)
		if !ok {
			continue
		}
		pref := b.PropKey(key)
		if !pref.Known {
			continue
		}
		for li, lab := range scan.Labels {
			if !lab.Known || !ix.HasNodeIndex(uint32(lab.Token), uint32(pref.Token)) {
				continue
			}
			rest := make([]bind.NameRef, 0, len(scan.Labels)-1)
			for j, other := range scan.Labels {
				if j != li {
					rest = append(rest, other)
				}
			}
			var res Op = &NodeIndexSeek{Var: scan.Var, Label: lab, Rest: rest, Prop: pref, Value: val}
			for i := len(preds) - 1; i >= 0; i-- {
				res = &Filter{Input: res, Pred: preds[i]}
			}
			return res, true
		}
	}
	return nil, false
}

// eqOnVar reports whether e is an equality between varName's property and a
// constant, returning the property key name and the constant-value expression.
// Either argument order is accepted (prop = const and const = prop).
func eqOnVar(e ast.Expr, varName string) (key string, val ast.Expr, ok bool) {
	bin, ok := e.(*ast.Binary)
	if !ok || bin.Op != ast.OpEq {
		return "", nil, false
	}
	if k, v, ok := propConstSide(bin.L, bin.R, varName); ok {
		return k, v, true
	}
	if k, v, ok := propConstSide(bin.R, bin.L, varName); ok {
		return k, v, true
	}
	return "", nil, false
}

// propConstSide reports whether prop is varName's property access and other is a
// constant, returning the property key and the constant expression.
func propConstSide(prop, other ast.Expr, varName string) (string, ast.Expr, bool) {
	p, ok := prop.(*ast.Property)
	if !ok {
		return "", nil, false
	}
	v, ok := p.Base.(*ast.Variable)
	if !ok || v.Name != varName {
		return "", nil, false
	}
	if !isConst(other) {
		return "", nil, false
	}
	return p.Key, other, true
}
