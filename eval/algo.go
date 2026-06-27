package eval

// This file is gr's native graph-algorithm surface: a small set of scalar
// functions that read the whole snapshot through the Env's read transaction,
// run a classic whole-graph algorithm in memory, and return the per-node result
// as a list of {id, <metric>} maps. A query consumes one with UNWIND:
//
//	UNWIND algo_pagerank('id', 0.85, 100) AS row
//	RETURN row.id AS id, row.score AS score ORDER BY id
//
// The functions exist because the six LDBC Graphalytics algorithms (BFS,
// PageRank, weakly connected components, community detection by label
// propagation, local clustering coefficient, single-source shortest paths) are
// iterative and whole-graph: they are not expressible as a read-only pattern
// match in any tractable form, so an engine runs them as a procedure. gr has no
// CALL surface, but a scalar function already receives the Env and so can reach
// the whole graph; returning a list of maps lets UNWIND project the rows with no
// new planner or executor machinery.
//
// Every function takes the name of the property that holds the node's external
// id as its first argument (the loader's NodeSource.IDProperty, usually "id"),
// because the algorithms report and seed by that id, not gr's internal element
// id. The id is read back and returned as an integer when it parses as one, so
// the result matches an engine that returns n.id directly; a non-numeric id
// falls back to the node's dense scan index, the same rule the reference uses.
//
// The semantics match the graph-bench Graphalytics reference exactly (the
// determinism choices that let two engines validate against one another):
// directed BFS and SSSP, weakly connected components labeled by smallest member
// id, label propagation with a smallest-label tie-break over a fixed round
// count, directed local clustering, and PageRank with uniform seed, damping over
// in-neighbor contributions, and dangling mass redistributed each iteration.

import (
	"fmt"
	"strconv"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// pageRankTol is the convergence floor for algo_pagerank: the iteration stops
// when the largest per-node change in a round falls below it. It is fixed rather
// than an argument because scientific-notation literals are awkward to write in
// a query and the value is the LDBC standard.
const pageRankTol = 1e-12

// algoGraph is a dense in-memory view of the whole snapshot, built once per
// algorithm call. out and in hold dense neighbor indices, one entry per
// relationship (a multiset), so the degree-weighted algorithms see parallel
// edges and self-loops exactly as the graph holds them, matching the reference.
type algoGraph struct {
	ids   []string       // external id (the IDProperty value) per dense index
	out   [][]int        // out-neighbor dense indices, per relationship
	in    [][]int        // in-neighbor dense indices, per relationship
	index map[string]int // external id to dense index, for source lookup
}

// loadAlgoGraph materializes the snapshot reachable through env into a dense
// graph keyed on the named id property. It scans every node once to assign dense
// indices and read the id, then walks each node's adjacency in both directions.
func loadAlgoGraph(env *Env, idProp string) (*algoGraph, error) {
	if env == nil || env.Tx == nil {
		return nil, fmt.Errorf("eval: graph algorithm needs a read transaction")
	}
	idTok, ok := env.Tx.Lookup(catalog.KindPropKey, idProp)
	if !ok {
		return nil, fmt.Errorf("eval: graph algorithm: unknown id property %q", idProp)
	}

	var nodes []engine.NodeID
	dense := map[engine.NodeID]int{}
	if err := env.Tx.ScanLabel(0, func(id engine.NodeID) error {
		dense[id] = len(nodes)
		nodes = append(nodes, id)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("eval: graph algorithm: scan nodes: %w", err)
	}

	n := len(nodes)
	g := &algoGraph{
		ids:   make([]string, n),
		out:   make([][]int, n),
		in:    make([][]int, n),
		index: make(map[string]int, n),
	}
	for i, id := range nodes {
		v, err := env.Tx.NodeProperty(id, idTok)
		if err != nil {
			return nil, fmt.Errorf("eval: graph algorithm: read id: %w", err)
		}
		s, _ := v.AsString()
		g.ids[i] = s
		g.index[s] = i
	}
	for i, id := range nodes {
		if err := env.Tx.Expand(id, 0, engine.Outgoing, func(nb engine.Neighbor) error {
			g.out[i] = append(g.out[i], dense[nb.Node])
			return nil
		}); err != nil {
			return nil, fmt.Errorf("eval: graph algorithm: expand out: %w", err)
		}
		if err := env.Tx.Expand(id, 0, engine.Incoming, func(nb engine.Neighbor) error {
			g.in[i] = append(g.in[i], dense[nb.Node])
			return nil
		}); err != nil {
			return nil, fmt.Errorf("eval: graph algorithm: expand in: %w", err)
		}
	}
	return g, nil
}

// numericID parses a node's id to an int64, falling back to its dense index when
// the id is not numeric, so a label is always a number. The synthetic datasets
// these algorithms run against emit dense numeric ids, so the fallback is dead
// weight there and only guards a hand-built graph.
func (g *algoGraph) numericID(i int) int64 {
	if v, err := strconv.ParseInt(g.ids[i], 10, 64); err == nil {
		return v
	}
	return int64(i)
}

// row builds one result map with the node's id (as a number) and one named
// metric value.
func (g *algoGraph) row(i int, metric string, v value.Value) value.Value {
	return value.Map(map[string]value.Value{
		"id":   value.Int(g.numericID(i)),
		metric: v,
	})
}

// --- argument helpers ---

func algoString(args []value.Value, i int, fn string) (string, error) {
	if i >= len(args) {
		return "", fmt.Errorf("eval: %s: missing argument %d", fn, i+1)
	}
	s, ok := args[i].AsString()
	if !ok {
		return "", fmt.Errorf("eval: %s: argument %d must be a string", fn, i+1)
	}
	return s, nil
}

func algoFloat(args []value.Value, i int, fn string) (float64, error) {
	if i >= len(args) {
		return 0, fmt.Errorf("eval: %s: missing argument %d", fn, i+1)
	}
	if f, ok := args[i].AsFloat(); ok {
		return f, nil
	}
	if n, ok := args[i].AsInt(); ok {
		return float64(n), nil
	}
	return 0, fmt.Errorf("eval: %s: argument %d must be a number", fn, i+1)
}

func algoInt(args []value.Value, i int, fn string) (int, error) {
	if i >= len(args) {
		return 0, fmt.Errorf("eval: %s: missing argument %d", fn, i+1)
	}
	if n, ok := args[i].AsInt(); ok {
		return int(n), nil
	}
	if f, ok := args[i].AsFloat(); ok {
		return int(f), nil
	}
	return 0, fmt.Errorf("eval: %s: argument %d must be an integer", fn, i+1)
}

// --- the algorithms ---

// fnAlgoBFS is algo_bfs(idProp, source): the breadth-first level (edge distance)
// of every node reachable from the source on the directed graph, the seed at
// level zero, unreachable nodes omitted.
func fnAlgoBFS(args []value.Value, env *Env) (value.Value, error) {
	idProp, err := algoString(args, 0, "algo_bfs")
	if err != nil {
		return value.Null, err
	}
	src, err := algoString(args, 1, "algo_bfs")
	if err != nil {
		return value.Null, err
	}
	g, err := loadAlgoGraph(env, idProp)
	if err != nil {
		return value.Null, err
	}
	dist, ok := g.bfs(src)
	if !ok {
		return value.List(), nil
	}
	var rows []value.Value
	for i, d := range dist {
		if d >= 0 {
			rows = append(rows, g.row(i, "level", value.Int(d)))
		}
	}
	return value.List(rows...), nil
}

// fnAlgoSSSP is algo_sssp(idProp, source): the single-source shortest-path
// distance from the source to every reachable node, as a float. The graph is
// unweighted, so the distance is the BFS level; it is its own function because an
// engine reaches it through a weighted-path procedure measured separately.
func fnAlgoSSSP(args []value.Value, env *Env) (value.Value, error) {
	idProp, err := algoString(args, 0, "algo_sssp")
	if err != nil {
		return value.Null, err
	}
	src, err := algoString(args, 1, "algo_sssp")
	if err != nil {
		return value.Null, err
	}
	g, err := loadAlgoGraph(env, idProp)
	if err != nil {
		return value.Null, err
	}
	dist, ok := g.bfs(src)
	if !ok {
		return value.List(), nil
	}
	var rows []value.Value
	for i, d := range dist {
		if d >= 0 {
			rows = append(rows, g.row(i, "distance", value.Float(float64(d))))
		}
	}
	return value.List(rows...), nil
}

// bfs returns the directed breadth-first distance to every node from the node
// with external id src, or -1 for an unreached node. ok is false when src is not
// in the graph.
func (g *algoGraph) bfs(src string) ([]int64, bool) {
	s, ok := g.index[src]
	if !ok {
		return nil, false
	}
	dist := make([]int64, len(g.ids))
	for i := range dist {
		dist[i] = -1
	}
	dist[s] = 0
	queue := []int{s}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, v := range g.out[u] {
			if dist[v] == -1 {
				dist[v] = dist[u] + 1
				queue = append(queue, v)
			}
		}
	}
	return dist, true
}

// fnAlgoWCC is algo_wcc(idProp): every node's weakly connected component, the
// component labeled by its smallest member id, edges followed in both directions.
func fnAlgoWCC(args []value.Value, env *Env) (value.Value, error) {
	idProp, err := algoString(args, 0, "algo_wcc")
	if err != nil {
		return value.Null, err
	}
	g, err := loadAlgoGraph(env, idProp)
	if err != nil {
		return value.Null, err
	}
	n := len(g.ids)
	comp := make([]int, n)
	for i := range comp {
		comp[i] = -1
	}
	label := 0
	for s := 0; s < n; s++ {
		if comp[s] != -1 {
			continue
		}
		comp[s] = label
		queue := []int{s}
		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			for _, v := range g.out[u] {
				if comp[v] == -1 {
					comp[v] = label
					queue = append(queue, v)
				}
			}
			for _, v := range g.in[u] {
				if comp[v] == -1 {
					comp[v] = label
					queue = append(queue, v)
				}
			}
		}
		label++
	}
	min := make([]int64, label)
	for i := range min {
		min[i] = -1
	}
	for i := 0; i < n; i++ {
		id := g.numericID(i)
		c := comp[i]
		if min[c] == -1 || id < min[c] {
			min[c] = id
		}
	}
	rows := make([]value.Value, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, g.row(i, "component", value.Int(min[comp[i]])))
	}
	return value.List(rows...), nil
}

// fnAlgoCDLP is algo_cdlp(idProp, rounds): every node's community after a fixed
// number of synchronous label-propagation rounds, ties broken by smallest label.
// Labels start as each node's own numeric id; a round adopts the most frequent
// label among neighbors in both directions, a bidirectional pair counting once
// per direction.
func fnAlgoCDLP(args []value.Value, env *Env) (value.Value, error) {
	idProp, err := algoString(args, 0, "algo_cdlp")
	if err != nil {
		return value.Null, err
	}
	rounds, err := algoInt(args, 1, "algo_cdlp")
	if err != nil {
		return value.Null, err
	}
	g, err := loadAlgoGraph(env, idProp)
	if err != nil {
		return value.Null, err
	}
	n := len(g.ids)
	labels := make([]int64, n)
	for i := 0; i < n; i++ {
		labels[i] = g.numericID(i)
	}
	next := make([]int64, n)
	for r := 0; r < rounds; r++ {
		for v := 0; v < n; v++ {
			counts := map[int64]int{}
			for _, u := range g.out[v] {
				counts[labels[u]]++
			}
			for _, u := range g.in[v] {
				counts[labels[u]]++
			}
			if len(counts) == 0 {
				next[v] = labels[v]
				continue
			}
			var best int64
			bestCount := -1
			for lab, c := range counts {
				if c > bestCount || (c == bestCount && lab < best) {
					best = lab
					bestCount = c
				}
			}
			next[v] = best
		}
		labels, next = next, labels
	}
	rows := make([]value.Value, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, g.row(i, "community", value.Int(labels[i])))
	}
	return value.List(rows...), nil
}

// fnAlgoLCC is algo_lcc(idProp): every node's local clustering coefficient. For a
// node whose neighbor set (undirected union, excluding itself) has size d, the
// coefficient is the number of directed edges between two neighbors over d*(d-1),
// and zero when d < 2.
func fnAlgoLCC(args []value.Value, env *Env) (value.Value, error) {
	idProp, err := algoString(args, 0, "algo_lcc")
	if err != nil {
		return value.Null, err
	}
	g, err := loadAlgoGraph(env, idProp)
	if err != nil {
		return value.Null, err
	}
	n := len(g.ids)
	nbr := make([]map[int]struct{}, n)
	for v := 0; v < n; v++ {
		set := map[int]struct{}{}
		for _, u := range g.out[v] {
			if u != v {
				set[u] = struct{}{}
			}
		}
		for _, u := range g.in[v] {
			if u != v {
				set[u] = struct{}{}
			}
		}
		nbr[v] = set
	}
	rows := make([]value.Value, 0, n)
	for v := 0; v < n; v++ {
		d := len(nbr[v])
		if d < 2 {
			rows = append(rows, g.row(v, "coefficient", value.Float(0)))
			continue
		}
		var links int64
		for u := range nbr[v] {
			for _, w := range g.out[u] {
				if w == u {
					continue
				}
				if _, ok := nbr[v][w]; ok {
					links++
				}
			}
		}
		coeff := float64(links) / (float64(d) * float64(d-1))
		rows = append(rows, g.row(v, "coefficient", value.Float(coeff)))
	}
	return value.List(rows...), nil
}

// fnAlgoPageRank is algo_pagerank(idProp, damping, maxIter): every node's
// PageRank with a uniform 1/N seed, damping over in-neighbor contributions, and
// dangling mass redistributed uniformly each iteration. It stops at the
// tolerance floor (the largest per-node change falls below pageRankTol) or the
// iteration cap, whichever comes first.
func fnAlgoPageRank(args []value.Value, env *Env) (value.Value, error) {
	idProp, err := algoString(args, 0, "algo_pagerank")
	if err != nil {
		return value.Null, err
	}
	damping, err := algoFloat(args, 1, "algo_pagerank")
	if err != nil {
		return value.Null, err
	}
	maxIter, err := algoInt(args, 2, "algo_pagerank")
	if err != nil {
		return value.Null, err
	}
	g, err := loadAlgoGraph(env, idProp)
	if err != nil {
		return value.Null, err
	}
	n := len(g.ids)
	if n == 0 {
		return value.List(), nil
	}
	pr := make([]float64, n)
	next := make([]float64, n)
	base := 1.0 / float64(n)
	for i := range pr {
		pr[i] = base
	}
	outdeg := make([]int, n)
	for i := range g.out {
		outdeg[i] = len(g.out[i])
	}
	for iter := 0; iter < maxIter; iter++ {
		var dangling float64
		for i := 0; i < n; i++ {
			if outdeg[i] == 0 {
				dangling += pr[i]
			}
		}
		danglingShare := damping * dangling / float64(n)
		teleport := (1.0 - damping) / float64(n)
		for v := 0; v < n; v++ {
			var sum float64
			for _, u := range g.in[v] {
				sum += pr[u] / float64(outdeg[u])
			}
			next[v] = teleport + danglingShare + damping*sum
		}
		var delta float64
		for i := 0; i < n; i++ {
			d := next[i] - pr[i]
			if d < 0 {
				d = -d
			}
			if d > delta {
				delta = d
			}
		}
		pr, next = next, pr
		if delta < pageRankTol {
			break
		}
	}
	rows := make([]value.Value, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, g.row(i, "score", value.Float(pr[i])))
	}
	return value.List(rows...), nil
}
