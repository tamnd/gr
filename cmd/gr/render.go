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
		return "(:#" + strconv.FormatUint(x.ID, 10) + ")"
	case gr.Relationship:
		return "[#" + strconv.FormatUint(x.ID, 10) + "]"
	case gr.Path:
		parts := make([]string, len(x.Elements))
		for i, e := range x.Elements {
			parts[i] = renderText(e, null)
		}
		return strings.Join(parts, "")
	default:
		return null
	}
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
		return `{"_id":` + strconv.FormatUint(x.ID, 10) + `}`
	case gr.Relationship:
		return `{"_id":` + strconv.FormatUint(x.ID, 10) + `}`
	case gr.Path:
		parts := make([]string, len(x.Elements))
		for i, e := range x.Elements {
			parts[i] = renderJSON(e)
		}
		return `{"_path":[` + strings.Join(parts, ",") + "]}"
	default:
		return "null"
	}
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
