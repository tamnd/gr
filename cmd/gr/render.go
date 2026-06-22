package main

import (
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/gr"
)

// renderText renders a value as its flat text form for the fixed-width and delimited
// modes (doc 17 §5.10): the null string for null, decimal for integers, the shortest
// round-tripping decimal for floats, the string itself for strings, 0x-hex for bytes,
// a bracketed list, a braced map, and the compact graph forms for node/relationship/
// path. It is the single value-to-text mapping every text-shaped mode shares.
func renderText(v gr.Value, null string) string {
	switch x := v.(type) {
	case nil:
		return null
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	case []byte:
		return "0x" + hex.EncodeToString(x)
	case []gr.Value:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = renderText(e, null)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]gr.Value:
		return renderMapText(x, null)
	case gr.Node:
		return renderNodeText(x, null)
	case gr.Relationship:
		return renderRelText(x, null)
	case gr.Path:
		return renderPathText(x, null)
	default:
		return null
	}
}

// renderNodeText renders a node in the compact pattern form (doc 17 §5.7):
// (:Label1:Label2 {key: value, ...}), labels then properties, each part omitted when
// empty so an unlabeled propertyless node is (). The element id is not shown in the
// text form; it surfaces only in the JSON form, which is the round-trippable one.
func renderNodeText(n gr.Node, null string) string {
	var b strings.Builder
	b.WriteByte('(')
	labels := n.Labels()
	for _, l := range labels {
		b.WriteByte(':')
		b.WriteString(l)
	}
	if props := n.Props(); len(props) > 0 {
		if len(labels) > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(renderMapText(props, null))
	}
	b.WriteByte(')')
	return b.String()
}

// renderRelText renders a relationship in the compact pattern form (doc 17 §5.7):
// [:TYPE {key: value, ...}], its single type then its properties.
func renderRelText(r gr.Relationship, null string) string {
	var b strings.Builder
	b.WriteString("[:")
	b.WriteString(r.Type())
	if props := r.Props(); len(props) > 0 {
		b.WriteByte(' ')
		b.WriteString(renderMapText(props, null))
	}
	b.WriteByte(']')
	return b.String()
}

// renderPathText renders a path as its nodes and relationships zipped into a pattern
// (doc 17 §5.7): (n0)-[r0]-(n1)-[r1]-(n2). Direction is not encoded; the relationship
// endpoints carry it, and the JSON form is where a consumer reads them.
func renderPathText(p gr.Path, null string) string {
	nodes := p.Nodes()
	if len(nodes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(renderNodeText(nodes[0], null))
	for i, r := range p.Relationships() {
		b.WriteString("-")
		b.WriteString(renderRelText(r, null))
		b.WriteString("-")
		if i+1 < len(nodes) {
			b.WriteString(renderNodeText(nodes[i+1], null))
		}
	}
	return b.String()
}

// renderMapText renders a map with its keys in sorted order so the text form is
// deterministic across runs (a Go map iterates in random order).
func renderMapText(m map[string]gr.Value, null string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + renderText(m[k], null)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// renderQuote renders a value as a Cypher literal (doc 17 §5.6): strings single
// quoted with escapes, numbers bare, lists bracketed, maps braced, null as the
// keyword null. The output is re-parseable as Cypher, the basis of the quote mode and
// the logical dump.
func renderQuote(v gr.Value) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return quoteCypherString(x)
	case []byte:
		return "'0x" + hex.EncodeToString(x) + "'"
	case []gr.Value:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = renderQuote(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]gr.Value:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + renderQuote(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return renderText(v, "null")
	}
}

// quoteCypherString wraps a string in single quotes, escaping the backslash and the
// single quote and the usual control characters, so the result is a valid Cypher
// string literal.
func quoteCypherString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '\'':
			b.WriteString("\\'")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// renderJSON renders a value as compact JSON (doc 17 §5.4, §5.10): integers and
// floats as numbers, strings as JSON strings, booleans as JSON booleans, null as the
// JSON null, lists as arrays, maps as objects, and the graph and bytes types as the
// underscore-keyed wrapper objects that survive a round-trip. JSON has a real null, so
// it always uses it regardless of the configured null string.
func renderJSON(v gr.Value) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return jsonString(x)
	case []byte:
		return `{"_bytes":` + jsonString(hex.EncodeToString(x)) + `}`
	case []gr.Value:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = renderJSON(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]gr.Value:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = jsonString(k) + ":" + renderJSON(x[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case gr.Node:
		return renderNodeJSON(x)
	case gr.Relationship:
		return renderRelJSON(x)
	case gr.Path:
		return renderPathJSON(x)
	default:
		return "null"
	}
}

// renderNodeJSON renders a node as a JSON object (doc 17 §5.4): the element id under
// "_id", the labels under "_labels", and the properties inlined at the top level, so
// a node reads like a plain record with its structural fields underscore-prefixed out
// of the way of property names.
func renderNodeJSON(n gr.Node) string {
	parts := []string{
		`"_id":` + jsonString(n.ElementId()),
		`"_labels":` + jsonStringList(n.Labels()),
	}
	parts = append(parts, propEntriesJSON(n.Props())...)
	return "{" + strings.Join(parts, ",") + "}"
}

// renderRelJSON renders a relationship as a JSON object (doc 17 §5.4): the element id
// under "_id", the type under "_type", the endpoint element ids under "_start" and
// "_end", and the properties inlined at the top level.
func renderRelJSON(r gr.Relationship) string {
	parts := []string{
		`"_id":` + jsonString(r.ElementId()),
		`"_type":` + jsonString(r.Type()),
		`"_start":` + jsonString(r.StartElementId()),
		`"_end":` + jsonString(r.EndElementId()),
	}
	parts = append(parts, propEntriesJSON(r.Props())...)
	return "{" + strings.Join(parts, ",") + "}"
}

// renderPathJSON renders a path as its nodes and relationships in traversal order
// (doc 17 §5.4): {"_nodes":[...],"_rels":[...]}.
func renderPathJSON(p gr.Path) string {
	nodes := p.Nodes()
	nparts := make([]string, len(nodes))
	for i, n := range nodes {
		nparts[i] = renderNodeJSON(n)
	}
	rels := p.Relationships()
	rparts := make([]string, len(rels))
	for i, r := range rels {
		rparts[i] = renderRelJSON(r)
	}
	return `{"_nodes":[` + strings.Join(nparts, ",") + `],"_rels":[` + strings.Join(rparts, ",") + "]}"
}

// propEntriesJSON renders a property map as JSON object members, keys sorted so the
// output is deterministic, ready to join with the structural members of a node or
// relationship object.
func propEntriesJSON(props map[string]gr.Value) []string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = jsonString(k) + ":" + renderJSON(props[k])
	}
	return parts
}

// jsonStringList renders a slice of strings as a JSON array of strings.
func jsonStringList(ss []string) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = jsonString(s)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// jsonString encodes a string as a JSON string literal with the mandatory escapes.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		default:
			if r < 0x20 {
				b.WriteString("\\u")
				const hexd = "0123456789abcdef"
				b.WriteByte('0')
				b.WriteByte('0')
				b.WriteByte(hexd[(r>>4)&0xf])
				b.WriteByte(hexd[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// isNumeric reports whether a value renders as a right-aligned number in the
// fixed-width modes (doc 17 §5.2). Only integers and floats are right-aligned.
func isNumeric(v gr.Value) bool {
	switch v.(type) {
	case int64, float64:
		return true
	}
	return false
}
