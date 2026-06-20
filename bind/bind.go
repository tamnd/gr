// Package bind is the Cypher binder: the semantic-analysis and name-resolution
// stage that turns the parser's purely syntactic tree ([ast]) into a resolved,
// annotated tree the planner can trust (spec 2060 doc 10 §4, §5). It answers the
// two questions parsing cannot: is this query meaningful (every variable bound
// before use, scopes respected, aggregations well-formed), and what do its names
// refer to (labels, relationship types, and property keys resolved to catalog
// tokens).
//
// The binder reaches the catalog only through the [Catalog] seam — a narrow
// name→token resolver — never the catalog's storage internals (doc 08 §5,
// §7). Resolution is snapshot-scoped at the caller: the seam wraps the
// transaction's catalog, so a query resolves against the schema as of its
// transaction and a concurrent schema change is not seen mid-compilation.
//
// Unknown names follow the schema-optional model (doc 08 §5.3). In the lenient
// default an unknown label resolves to "matches nothing" and an unknown property
// to "always null", so a query against a not-yet-populated schema still runs and
// returns empties; in strict mode an unknown name is an error that catches
// typos. Either way the binder records the resolution; it does not silently drop
// names.
package bind

import (
	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/engine"
)

// Catalog is the binder's view of the catalog: a name→token resolver for the
// three dictionaries, scoped to the caller's transaction. It is the only seam
// between the binder and the storage engine; the binder carries tokens, the
// engine owns the dictionaries. *engine.DiskEngine satisfies it through
// [NewEngineCatalog].
type Catalog interface {
	// LabelToken resolves a node-label name, reporting false if it is unknown.
	LabelToken(name string) (engine.Token, bool)
	// RelTypeToken resolves a relationship-type name, reporting false if unknown.
	RelTypeToken(name string) (engine.Token, bool)
	// PropKeyToken resolves a property-key name, reporting false if unknown.
	PropKeyToken(name string) (engine.Token, bool)
}

// NameRef is one resolved name. Known reports whether the catalog held the name;
// Token is meaningful only when Known is true. An unresolved ref is the
// schema-optional sentinel: a label that matches nothing, a property that reads
// null. The binder produces these only in lenient mode; strict mode turns an
// unknown name into an error before a NameRef is recorded.
type NameRef struct {
	Token engine.Token
	Known bool
}

// Bound is the resolved, annotated query: the original tree plus the catalog
// resolutions the planner needs. The tree is unchanged (the binder annotates
// rather than rewrites), so a node or relationship pattern is looked up by
// pointer through the accessors below. Columns names the final result columns,
// in order, for the library API and the executor.
type Bound struct {
	Query   *ast.Query
	Columns []string

	nodeLabels map[*ast.NodePattern][]NameRef
	relTypes   map[*ast.RelPattern][]NameRef
	propKeys   map[string]NameRef
}

// NodeLabels returns the resolved label set of a node pattern, one NameRef per
// written label, in source order. An empty slice means the pattern carried no
// labels and so matches every node (the SPI wildcard).
func (b *Bound) NodeLabels(np *ast.NodePattern) []NameRef { return b.nodeLabels[np] }

// RelTypes returns the resolved type set of a relationship pattern, one NameRef
// per written type. An empty slice means the pattern constrained no type and so
// matches every relationship.
func (b *Bound) RelTypes(rp *ast.RelPattern) []NameRef { return b.relTypes[rp] }

// PropKey returns the resolution of a property-key name encountered anywhere in
// the query. The zero NameRef (Known false) is returned for a name the query
// never referenced as well as for one resolved leniently to null; callers that
// need the difference should consult the bound tree, not this map.
func (b *Bound) PropKey(name string) NameRef { return b.propKeys[name] }

// Error is a semantic or name-resolution failure, positioned at the offending
// construct so the message can point at the source.
type Error struct {
	Msg  string
	Line int
	Col  int
}

func (e *Error) Error() string {
	return "bind: " + e.Msg + " at line " + itoa(e.Line) + ":" + itoa(e.Col)
}

// varKind is what a variable binds to, tracked so a pattern that reuses a name
// with a conflicting role is caught and so node and relationship roles survive a
// WITH that carries the variable on by name.
type varKind uint8

const (
	vkNode varKind = iota
	vkRel
	vkPath
	vkValue // an UNWIND element, or any projected expression
)

// scope is the set of variables in scope at a point in the clause pipeline,
// keyed by name to their kind.
type scope map[string]varKind

// binder holds the resolution state for one Bind call.
type binder struct {
	cat    Catalog
	strict bool
	out    *Bound
}

// Bind analyzes and resolves a parsed query against the catalog, returning the
// annotated tree or the first semantic/resolution error. strict selects the
// schema-optional mode: false (the default) resolves unknown names leniently,
// true rejects them. The query tree is not modified.
func Bind(q *ast.Query, cat Catalog, strict bool) (*Bound, error) {
	bd := &binder{
		cat:    cat,
		strict: strict,
		out: &Bound{
			Query:      q,
			nodeLabels: map[*ast.NodePattern][]NameRef{},
			relTypes:   map[*ast.RelPattern][]NameRef{},
			propKeys:   map[string]NameRef{},
		},
	}
	cols, err := bd.single(q.First)
	if err != nil {
		return nil, err
	}
	bd.out.Columns = cols
	for _, tail := range q.Rest {
		tcols, err := bd.single(tail.Query)
		if err != nil {
			return nil, err
		}
		if err := unionColumnsMatch(cols, tcols, tail.Pos); err != nil {
			return nil, err
		}
	}
	return bd.out, nil
}

// single binds one UNION arm: a clause sequence ending in RETURN. Each arm has
// its own fresh scope (UNION combines result rows, not variable scopes). It
// returns the arm's final column names for the UNION compatibility check.
func (bd *binder) single(sq *ast.SingleQuery) ([]string, error) {
	sc := scope{}
	var cols []string
	for i, c := range sq.Clauses {
		last := i == len(sq.Clauses)-1
		switch cl := c.(type) {
		case *ast.Match:
			if err := bd.match(cl, sc); err != nil {
				return nil, err
			}
		case *ast.Unwind:
			if err := bd.unwind(cl, sc); err != nil {
				return nil, err
			}
		case *ast.With:
			ns, err := bd.projection(&cl.Projection, sc, false)
			if err != nil {
				return nil, err
			}
			if cl.Where != nil {
				if err := bd.checkExpr(cl.Where, ns, false); err != nil {
					return nil, err
				}
			}
			sc = ns
		case *ast.Return:
			if !last {
				return nil, &Error{"RETURN must be the final clause", cl.Line, cl.Col}
			}
			ns, err := bd.projection(&cl.Projection, sc, true)
			if err != nil {
				return nil, err
			}
			cols = projectionColumns(&cl.Projection, sc)
			sc = ns
		default:
			return nil, &Error{"unsupported clause in the read path", 0, 0}
		}
	}
	if cols == nil {
		return nil, &Error{"a query must end with RETURN", sq.Line, sq.Col}
	}
	return cols, nil
}

// match binds a MATCH (or OPTIONAL MATCH) clause in two passes: pass one binds
// every pattern variable and resolves every label, type, and property key so
// that pass two can check the property-map and WHERE expressions against the
// complete post-pattern scope (a property map may reference any variable the
// MATCH binds, regardless of left-to-right order).
func (bd *binder) match(m *ast.Match, sc scope) error {
	for _, pp := range m.Patterns {
		if err := bd.bindPath(pp, sc); err != nil {
			return err
		}
	}
	for _, pp := range m.Patterns {
		if err := bd.checkPathExprs(pp, sc); err != nil {
			return err
		}
	}
	if m.Where != nil {
		if err := bd.checkExpr(m.Where, sc, false); err != nil {
			return err
		}
	}
	return nil
}

// bindPath performs pass one over a path pattern: it binds the optional path
// variable and every node and relationship variable, and resolves the label,
// type, and property-key names.
func (bd *binder) bindPath(pp *ast.PathPattern, sc scope) error {
	if pp.Var != "" {
		// A named path materializes from its bound element variables in order
		// (plan.BindPath). A variable-length step binds a relationship list, not a
		// single relationship, and does not bind its intermediate nodes, so the
		// element sequence is incomplete; reject it until the variable-length
		// expand records the full walk (deliverable 8b follow-on).
		for _, step := range pp.Chain {
			if step.Rel.VarLen != nil {
				return &Error{"named path over a variable-length relationship is not yet supported", pp.Line, pp.Col}
			}
		}
		if err := bindVar(sc, pp.Var, vkPath, pp.Pos); err != nil {
			return err
		}
	}
	if err := bd.bindNode(pp.Start, sc); err != nil {
		return err
	}
	for _, step := range pp.Chain {
		if err := bd.bindRel(step.Rel, sc); err != nil {
			return err
		}
		if err := bd.bindNode(step.Node, sc); err != nil {
			return err
		}
	}
	return nil
}

func (bd *binder) bindNode(np *ast.NodePattern, sc scope) error {
	refs := make([]NameRef, 0, len(np.Labels))
	for _, name := range np.Labels {
		ref, err := bd.resolveLabel(name, np.Pos)
		if err != nil {
			return err
		}
		refs = append(refs, ref)
	}
	bd.out.nodeLabels[np] = refs
	if err := bd.resolvePropKeys(np.Properties, np.Pos); err != nil {
		return err
	}
	if np.Var != "" {
		return bindVar(sc, np.Var, vkNode, np.Pos)
	}
	return nil
}

func (bd *binder) bindRel(rp *ast.RelPattern, sc scope) error {
	refs := make([]NameRef, 0, len(rp.Types))
	for _, name := range rp.Types {
		ref, err := bd.resolveRelType(name, rp.Pos)
		if err != nil {
			return err
		}
		refs = append(refs, ref)
	}
	bd.out.relTypes[rp] = refs
	if err := bd.resolvePropKeys(rp.Properties, rp.Pos); err != nil {
		return err
	}
	if rp.VarLen != nil {
		if err := checkVarLength(rp.VarLen, rp.Pos); err != nil {
			return err
		}
	}
	if rp.Var != "" {
		return bindVar(sc, rp.Var, vkRel, rp.Pos)
	}
	return nil
}

// checkPathExprs performs pass two: the value expressions of every property-map
// constraint are checked against the (now complete) scope.
func (bd *binder) checkPathExprs(pp *ast.PathPattern, sc scope) error {
	if err := bd.checkProps(pp.Start.Properties, sc); err != nil {
		return err
	}
	for _, step := range pp.Chain {
		if err := bd.checkProps(step.Rel.Properties, sc); err != nil {
			return err
		}
		if err := bd.checkProps(step.Node.Properties, sc); err != nil {
			return err
		}
	}
	return nil
}

func (bd *binder) checkProps(props []ast.PropEntry, sc scope) error {
	for _, pe := range props {
		if err := bd.checkExpr(pe.Value, sc, false); err != nil {
			return err
		}
	}
	return nil
}

// unwind binds an UNWIND clause: the list expression is checked against the
// current scope, then the element variable enters scope as a value.
func (bd *binder) unwind(u *ast.Unwind, sc scope) error {
	if err := bd.checkExpr(u.Expr, sc, false); err != nil {
		return err
	}
	return bindVar(sc, u.Var, vkValue, u.Pos)
}

// projection binds a WITH or RETURN projection and returns the scope it
// produces. isReturn loosens the rules RETURN may relax: a WITH item that is not
// a bare variable must be aliased (its output needs a name to carry forward),
// while a RETURN item may stay anonymous. Each item's expression is checked
// against the input scope with aggregates permitted; ORDER BY, SKIP, and LIMIT
// are checked against the output scope.
func (bd *binder) projection(p *ast.Projection, sc scope, isReturn bool) (scope, error) {
	if p.Star {
		// WITH * / RETURN * carries the whole input scope onward unchanged, then
		// adds any explicitly listed extra items.
		ns := scope{}
		for k, v := range sc {
			ns[k] = v
		}
		if err := bd.projectItems(p, sc, ns, isReturn); err != nil {
			return nil, err
		}
		return bd.projectTail(p, sc, ns)
	}
	ns := scope{}
	if err := bd.projectItems(p, sc, ns, isReturn); err != nil {
		return nil, err
	}
	return bd.projectTail(p, sc, ns)
}

// projectItems checks each projected expression and records its output variable
// in ns, preserving a carried variable's kind so a node or relationship survives
// the projection by name.
func (bd *binder) projectItems(p *ast.Projection, in, ns scope, isReturn bool) error {
	for _, it := range p.Items {
		if err := bd.checkExpr(it.Expr, in, true); err != nil {
			return err
		}
		name := it.Alias
		if name == "" {
			if v, ok := it.Expr.(*ast.Variable); ok {
				name = v.Name
			} else if !isReturn {
				return &Error{"a WITH item that is not a variable must have an AS alias", exprPos(it.Expr).Line, exprPos(it.Expr).Col}
			}
		}
		if name != "" {
			ns[name] = projectedKind(it.Expr, in)
		}
	}
	return nil
}

// projectTail checks the ORDER BY / SKIP / LIMIT tail. ORDER BY may reference
// both the projection's output and its input (so RETURN p.name AS n ORDER BY
// p.age is accepted); SKIP and LIMIT are checked but reference no variables in
// practice.
func (bd *binder) projectTail(p *ast.Projection, in, ns scope) (scope, error) {
	order := scope{}
	for k, v := range in {
		order[k] = v
	}
	for k, v := range ns {
		order[k] = v
	}
	for _, s := range p.OrderBy {
		if err := bd.checkExpr(s.Expr, order, true); err != nil {
			return nil, err
		}
	}
	if p.Skip != nil {
		if err := bd.checkExpr(p.Skip, ns, false); err != nil {
			return nil, err
		}
	}
	if p.Limit != nil {
		if err := bd.checkExpr(p.Limit, ns, false); err != nil {
			return nil, err
		}
	}
	return ns, nil
}

// projectedKind is the kind a projected item carries into the next scope: a bare
// or aliased variable keeps its source kind (a node stays a node), everything
// else becomes a value.
func projectedKind(e ast.Expr, in scope) varKind {
	if v, ok := e.(*ast.Variable); ok {
		if k, ok := in[v.Name]; ok {
			return k
		}
	}
	return vkValue
}

// bindVar adds a variable to the scope, or accepts a reference to one already
// bound. Re-using a name with a different role (a node name used for a
// relationship) is a semantic error.
func bindVar(sc scope, name string, kind varKind, pos ast.Pos) error {
	if existing, ok := sc[name]; ok {
		if existing != kind {
			return &Error{"variable " + name + " is already bound with a different role", pos.Line, pos.Col}
		}
		return nil
	}
	sc[name] = kind
	return nil
}

// checkVarLength validates a variable-length range: a stated lower bound may not
// exceed a stated upper bound. Omitted bounds (-1) default later in planning.
func checkVarLength(v *ast.VarLength, pos ast.Pos) error {
	if v.Min >= 0 && v.Max >= 0 && v.Min > v.Max {
		return &Error{"variable-length lower bound exceeds upper bound", pos.Line, pos.Col}
	}
	return nil
}

// --- name resolution against the catalog seam ---

func (bd *binder) resolveLabel(name string, pos ast.Pos) (NameRef, error) {
	if t, ok := bd.cat.LabelToken(name); ok {
		return NameRef{Token: t, Known: true}, nil
	}
	if bd.strict {
		return NameRef{}, &Error{"unknown label " + name, pos.Line, pos.Col}
	}
	return NameRef{}, nil
}

func (bd *binder) resolveRelType(name string, pos ast.Pos) (NameRef, error) {
	if t, ok := bd.cat.RelTypeToken(name); ok {
		return NameRef{Token: t, Known: true}, nil
	}
	if bd.strict {
		return NameRef{}, &Error{"unknown relationship type " + name, pos.Line, pos.Col}
	}
	return NameRef{}, nil
}

// resolvePropKey resolves and records a property-key name. An unknown key is an
// error in strict mode and the always-null sentinel otherwise; the resolution
// is memoized in the Bound's property map.
func (bd *binder) resolvePropKey(name string, pos ast.Pos) error {
	if _, seen := bd.out.propKeys[name]; seen {
		return nil
	}
	if t, ok := bd.cat.PropKeyToken(name); ok {
		bd.out.propKeys[name] = NameRef{Token: t, Known: true}
		return nil
	}
	if bd.strict {
		return &Error{"unknown property key " + name, pos.Line, pos.Col}
	}
	bd.out.propKeys[name] = NameRef{}
	return nil
}

func (bd *binder) resolvePropKeys(props []ast.PropEntry, pos ast.Pos) error {
	for _, pe := range props {
		if err := bd.resolvePropKey(pe.Key, pos); err != nil {
			return err
		}
	}
	return nil
}

// --- the engine adapter ---

// TokenResolver is the slice of the storage engine the binder's catalog adapter
// needs: name lookup by dictionary kind. *engine.DiskEngine satisfies it.
type TokenResolver interface {
	Lookup(kind catalog.Kind, name string) (engine.Token, bool)
}

// engineCatalog adapts a TokenResolver to the Catalog seam, mapping each
// dictionary to its catalog.Kind.
type engineCatalog struct{ r TokenResolver }

// NewEngineCatalog wraps a storage engine as the binder's Catalog seam.
func NewEngineCatalog(r TokenResolver) Catalog { return engineCatalog{r} }

func (e engineCatalog) LabelToken(name string) (engine.Token, bool) {
	return e.r.Lookup(catalog.KindLabel, name)
}

func (e engineCatalog) RelTypeToken(name string) (engine.Token, bool) {
	return e.r.Lookup(catalog.KindRelType, name)
}

func (e engineCatalog) PropKeyToken(name string) (engine.Token, bool) {
	return e.r.Lookup(catalog.KindPropKey, name)
}

// itoa formats a non-negative int without importing fmt, matching the lexer and
// parser error helpers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
