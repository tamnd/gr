// Package httpd is the HTTP/JSON transport over a gr database (spec 2060 doc 18 §9):
// the driverless, curl-able surface that maps each request to a library Run and
// streams the result back as JSON. It is the lower-fidelity companion to the Bolt
// transport; graph values serialize to JSON objects rather than native driver types.
//
// The handler is built over a *gr.DB and is a plain http.Handler, so it mounts into
// any server and is exercised with httptest in the tests without binding a port.
package httpd

import (
	"encoding/base64"
	"math"
	"strconv"

	"github.com/tamnd/gr"
)

// safeIntMax is the largest integer a JSON number carries without losing precision,
// 2^53 - 1 (doc 18 §9.4). An integer outside the safe range is emitted in the tagged
// string form so a JavaScript client, whose number is a double, deserializes it
// losslessly.
const safeIntMax = 1<<53 - 1

// toJSON maps a gr value to a value the standard JSON encoder renders per the doc 18
// §9.4 type table. With intAsString every integer is emitted in the tagged string
// form (the ?integerEncoding=string request option, the safe default for JavaScript
// clients); otherwise only integers outside the safe range are tagged.
func toJSON(v gr.Value, intAsString bool) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		return x
	case int64:
		return intJSON(x, intAsString)
	case float64:
		// NaN and the infinities have no JSON number form; emit null rather than
		// produce invalid JSON the standard encoder would reject.
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return x
	case string:
		return x
	case []byte:
		return map[string]any{"$type": "Base64", "_value": base64.StdEncoding.EncodeToString(x)}
	case []gr.Value:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toJSON(e, intAsString)
		}
		return out
	case map[string]gr.Value:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = toJSON(e, intAsString)
		}
		return out
	case gr.Node:
		return nodeJSON(x)
	case gr.Relationship:
		return relJSON(x)
	case gr.Path:
		return pathJSON(x, intAsString)
	default:
		return nil
	}
}

// intJSON renders an integer either as a plain JSON number or, when it is outside the
// JavaScript-safe range or string encoding was requested, as the tagged string form
// {"$type":"Integer","_value":"..."} (doc 18 §9.4).
func intJSON(x int64, intAsString bool) any {
	if intAsString || x > safeIntMax || x < -safeIntMax {
		return map[string]any{"$type": "Integer", "_value": strconv.FormatInt(x, 10)}
	}
	return x
}

// nodeJSON renders a node (doc 18 §9.4). The library value model carries only the
// node id today; labels and properties await the lazily-fetched graph-object surface
// (doc 16 §10.6), so they are not emitted yet rather than emitted empty and wrong.
func nodeJSON(n gr.Node) map[string]any {
	return map[string]any{"elementId": strconv.FormatUint(n.ID, 10)}
}

// relJSON renders a relationship (doc 18 §9.4). Like a node it carries only the id
// today; the type and endpoints await the lazily-fetched graph-object surface.
func relJSON(r gr.Relationship) map[string]any {
	return map[string]any{"elementId": strconv.FormatUint(r.ID, 10)}
}

// pathJSON renders a path as a start node plus alternating relationship/end-node
// segments (doc 18 §9.4). The path elements alternate node, relationship, node, so
// each segment pairs the relationship with the node that follows it.
func pathJSON(p gr.Path, intAsString bool) map[string]any {
	if len(p.Elements) == 0 {
		return map[string]any{"start": nil, "segments": []any{}}
	}
	out := map[string]any{"start": toJSON(p.Elements[0], intAsString)}
	var segs []any
	for i := 1; i+1 < len(p.Elements); i += 2 {
		segs = append(segs, map[string]any{
			"relationship": toJSON(p.Elements[i], intAsString),
			"end":          toJSON(p.Elements[i+1], intAsString),
		})
	}
	if segs == nil {
		segs = []any{}
	}
	out["segments"] = segs
	return out
}

// counters builds the update-counter object for a response (doc 18 §9.3, §5.11). It
// always carries containsUpdates and adds each non-zero named counter, so a read
// reports just containsUpdates:false and a write reports exactly what it changed.
func counters(s gr.Summary) map[string]any {
	c := map[string]any{"containsUpdates": hasUpdates(s)}
	add := func(name string, n int) {
		if n != 0 {
			c[name] = n
		}
	}
	add("nodesCreated", s.NodesCreated)
	add("nodesDeleted", s.NodesDeleted)
	add("relationshipsCreated", s.RelationshipsCreated)
	add("relationshipsDeleted", s.RelationshipsDeleted)
	add("propertiesSet", s.PropertiesSet)
	add("labelsAdded", s.LabelsAdded)
	add("labelsRemoved", s.LabelsRemoved)
	add("indexesAdded", s.IndexesAdded)
	add("indexesRemoved", s.IndexesRemoved)
	add("constraintsAdded", s.ConstraintsAdded)
	add("constraintsRemoved", s.ConstraintsRemoved)
	return c
}

// hasUpdates reports whether a summary records any graph or schema mutation.
func hasUpdates(s gr.Summary) bool {
	return s.NodesCreated|s.NodesDeleted|s.RelationshipsCreated|s.RelationshipsDeleted|
		s.PropertiesSet|s.LabelsAdded|s.LabelsRemoved|s.IndexesAdded|s.IndexesRemoved|
		s.ConstraintsAdded|s.ConstraintsRemoved != 0
}

// queryType classifies a statement for the response (doc 18 §9.3): "s" for a schema
// command, "rw" for a write that also returned rows, "w" for a write that returned
// none, and "r" for a read.
func queryType(s gr.Summary, returnedRows bool) string {
	if s.IndexesAdded|s.IndexesRemoved|s.ConstraintsAdded|s.ConstraintsRemoved != 0 {
		return "s"
	}
	write := s.NodesCreated|s.NodesDeleted|s.RelationshipsCreated|s.RelationshipsDeleted|
		s.PropertiesSet|s.LabelsAdded|s.LabelsRemoved != 0
	switch {
	case write && returnedRows:
		return "rw"
	case write:
		return "w"
	default:
		return "r"
	}
}
