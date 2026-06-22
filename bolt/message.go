package bolt

import (
	"fmt"

	"github.com/tamnd/gr/pack"
)

// The Bolt message signatures (doc 18 §4.10). A message is a PackStream
// structure whose signature byte is its message type and whose fields are its
// arguments. The same byte 0x54 is both TELEMETRY (a top-level message) and Time
// (a value inside a record); they never share a parsing context, so the
// collision is unambiguous (doc 18 §4.10).
const (
	SigHello     = 0x01
	SigGoodbye   = 0x02
	SigReset     = 0x0F
	SigRun       = 0x10
	SigBegin     = 0x11
	SigCommit    = 0x12
	SigRollback  = 0x13
	SigDiscard   = 0x2F
	SigPull      = 0x3F
	SigRoute     = 0x66
	SigLogon     = 0x6A
	SigLogoff    = 0x6B
	SigTelemetry = 0x54

	SigSuccess = 0x70
	SigRecord  = 0x71
	SigIgnored = 0x7E
	SigFailure = 0x7F
)

// Request is a decoded client message (doc 18 §5). The session loop type-switches
// over the concrete types to drive the state machine.
type Request interface {
	// Signature returns the message's structure signature (doc 18 §4.10), for
	// logging and the state-machine validity table.
	Signature() byte
}

// Hello initializes a connection and, on Bolt < 5.1, authenticates (doc 18 §5.4).
type Hello struct{ Extra map[string]any }

// Logon authenticates a connection on Bolt 5.1+ (doc 18 §5.5).
type Logon struct{ Auth map[string]any }

// Logoff de-authenticates a READY connection on Bolt 5.1+ (doc 18 §5.5).
type Logoff struct{}

// Goodbye ends the connection (doc 18 §5).
type Goodbye struct{}

// Reset interrupts the current work and returns the connection to READY
// (doc 18 §5.2).
type Reset struct{}

// Run submits a Cypher query (doc 18 §5.6).
type Run struct {
	Query  string
	Params map[string]any
	Extra  map[string]any
}

// Begin opens an explicit transaction (doc 18 §5.10).
type Begin struct{ Extra map[string]any }

// Commit commits the open explicit transaction (doc 18 §5.10).
type Commit struct{}

// Rollback rolls back the open explicit transaction (doc 18 §5.10).
type Rollback struct{}

// Pull requests result rows from the active stream (doc 18 §5.7).
type Pull struct{ Extra map[string]any }

// Discard drains the active stream without transferring rows (doc 18 §5.8).
type Discard struct{ Extra map[string]any }

// Route asks for the routing table (doc 18 §5.14).
type Route struct {
	Routing   map[string]any
	Bookmarks []string
	Extra     map[string]any
}

// Telemetry reports a driver API usage hint on Bolt 5.4+ (doc 18 §5.16).
type Telemetry struct{ API int64 }

func (Hello) Signature() byte     { return SigHello }
func (Logon) Signature() byte     { return SigLogon }
func (Logoff) Signature() byte    { return SigLogoff }
func (Goodbye) Signature() byte   { return SigGoodbye }
func (Reset) Signature() byte     { return SigReset }
func (Run) Signature() byte       { return SigRun }
func (Begin) Signature() byte     { return SigBegin }
func (Commit) Signature() byte    { return SigCommit }
func (Rollback) Signature() byte  { return SigRollback }
func (Pull) Signature() byte      { return SigPull }
func (Discard) Signature() byte   { return SigDiscard }
func (Route) Signature() byte     { return SigRoute }
func (Telemetry) Signature() byte { return SigTelemetry }

// N returns a PULL's record count (doc 18 §5.7): -1 ("all remaining") when the
// key is absent.
func (p Pull) N() int64 { return intOr(p.Extra, "n", -1) }

// Qid returns a PULL's target stream id (doc 18 §5.7): -1 ("the most recent
// RUN's stream") when the key is absent.
func (p Pull) Qid() int64 { return intOr(p.Extra, "qid", -1) }

// N returns a DISCARD's record count (doc 18 §5.8): -1 ("all remaining") when
// the key is absent.
func (d Discard) N() int64 { return intOr(d.Extra, "n", -1) }

// Qid returns a DISCARD's target stream id (doc 18 §5.8): -1 ("the most recent
// RUN's stream") when the key is absent.
func (d Discard) Qid() int64 { return intOr(d.Extra, "qid", -1) }

// intOr reads an integer key from an extra map, returning def when the key is
// absent or not an integer.
func intOr(m map[string]any, key string, def int64) int64 {
	if v, ok := m[key]; ok {
		if n, ok := v.(int64); ok {
			return n
		}
	}
	return def
}

// DecodeRequest decodes a framed message body into a typed Request (doc 18 §5).
// The body is one PackStream structure whose signature names the message; an
// unknown signature, a wrong field count, or a wrong field type is a protocol
// error the session maps to Neo.ClientError.Request.Invalid.
func DecodeRequest(body []byte) (Request, error) {
	v, err := pack.Unmarshal(body)
	if err != nil {
		return nil, err
	}
	s, ok := v.(pack.Structure)
	if !ok {
		return nil, fmt.Errorf("bolt: message body is %T, want a structure", v)
	}
	switch s.Tag {
	case SigHello:
		m, err := oneMap(s, "HELLO")
		if err != nil {
			return nil, err
		}
		return Hello{Extra: m}, nil
	case SigLogon:
		m, err := oneMap(s, "LOGON")
		if err != nil {
			return nil, err
		}
		return Logon{Auth: m}, nil
	case SigLogoff:
		return Logoff{}, wantFields(s, 0, "LOGOFF")
	case SigGoodbye:
		return Goodbye{}, wantFields(s, 0, "GOODBYE")
	case SigReset:
		return Reset{}, wantFields(s, 0, "RESET")
	case SigRun:
		if err := wantFields(s, 3, "RUN"); err != nil {
			return nil, err
		}
		query, err := asString(s.Fields[0], "RUN query")
		if err != nil {
			return nil, err
		}
		params, err := asMap(s.Fields[1], "RUN parameters")
		if err != nil {
			return nil, err
		}
		extra, err := asMap(s.Fields[2], "RUN extra")
		if err != nil {
			return nil, err
		}
		return Run{Query: query, Params: params, Extra: extra}, nil
	case SigBegin:
		m, err := oneMap(s, "BEGIN")
		if err != nil {
			return nil, err
		}
		return Begin{Extra: m}, nil
	case SigCommit:
		return Commit{}, wantFields(s, 0, "COMMIT")
	case SigRollback:
		return Rollback{}, wantFields(s, 0, "ROLLBACK")
	case SigPull:
		m, err := oneMap(s, "PULL")
		if err != nil {
			return nil, err
		}
		return Pull{Extra: m}, nil
	case SigDiscard:
		m, err := oneMap(s, "DISCARD")
		if err != nil {
			return nil, err
		}
		return Discard{Extra: m}, nil
	case SigRoute:
		if err := wantFields(s, 3, "ROUTE"); err != nil {
			return nil, err
		}
		routing, err := asMap(s.Fields[0], "ROUTE routing")
		if err != nil {
			return nil, err
		}
		bookmarks, err := asStringList(s.Fields[1], "ROUTE bookmarks")
		if err != nil {
			return nil, err
		}
		extra, err := asMap(s.Fields[2], "ROUTE extra")
		if err != nil {
			return nil, err
		}
		return Route{Routing: routing, Bookmarks: bookmarks, Extra: extra}, nil
	case SigTelemetry:
		if err := wantFields(s, 1, "TELEMETRY"); err != nil {
			return nil, err
		}
		api, err := asInt(s.Fields[0], "TELEMETRY api")
		if err != nil {
			return nil, err
		}
		return Telemetry{API: api}, nil
	default:
		return nil, fmt.Errorf("bolt: unknown message signature 0x%02X", s.Tag)
	}
}

// The reply builders. Each returns a pack.Structure the session encodes with
// pack.Marshal and writes with a ChunkWriter (doc 18 §5).

// Success builds a SUCCESS reply carrying the metadata map (doc 18 §5.4, §5.6,
// §5.7). A nil map is encoded as an empty map, the LOGON/COMMIT success shape.
func Success(meta map[string]any) pack.Structure {
	if meta == nil {
		meta = map[string]any{}
	}
	return pack.Structure{Tag: SigSuccess, Fields: []any{meta}}
}

// Record builds a RECORD reply carrying one result row, a list of values (doc 18
// §5.9). The values are already in the pack codec's plain-type form (the graph
// value mapping, doc 18 §6, is a separate layer).
func Record(values []any) pack.Structure {
	if values == nil {
		values = []any{}
	}
	return pack.Structure{Tag: SigRecord, Fields: []any{values}}
}

// Ignored builds an IGNORED reply, sent for any request received while the
// connection is in the FAILED state (doc 18 §5.2).
func Ignored() pack.Structure {
	return pack.Structure{Tag: SigIgnored, Fields: []any{}}
}

// Failure builds a FAILURE reply carrying a Neo4j status code and a human
// message (doc 18 §5, §12). The richer GQLSTATUS diagnostic record (doc 18 §12.4)
// is added by the session on 5.6+ connections.
func Failure(code, message string) pack.Structure {
	return pack.Structure{Tag: SigFailure, Fields: []any{map[string]any{
		"code":    code,
		"message": message,
	}}}
}

// FailureMeta builds a FAILURE reply from a prebuilt metadata map, for the
// session to attach a diagnostic record or other 5.6+ fields (doc 18 §12.4).
func FailureMeta(meta map[string]any) pack.Structure {
	if meta == nil {
		meta = map[string]any{}
	}
	return pack.Structure{Tag: SigFailure, Fields: []any{meta}}
}

// oneMap validates a one-field message whose field is a Dictionary (HELLO, LOGON,
// BEGIN, PULL, DISCARD) and returns it.
func oneMap(s pack.Structure, name string) (map[string]any, error) {
	if err := wantFields(s, 1, name); err != nil {
		return nil, err
	}
	return asMap(s.Fields[0], name+" extra")
}

// wantFields checks a structure's field count.
func wantFields(s pack.Structure, n int, name string) error {
	if len(s.Fields) != n {
		return fmt.Errorf("bolt: %s has %d fields, want %d", name, len(s.Fields), n)
	}
	return nil
}

// asString coerces a decoded field to a string.
func asString(v any, what string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("bolt: %s is %T, want a string", what, v)
	}
	return s, nil
}

// asInt coerces a decoded field to an int64 (PackStream decodes every integer
// width to int64).
func asInt(v any, what string) (int64, error) {
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("bolt: %s is %T, want an integer", what, v)
	}
	return n, nil
}

// asMap coerces a decoded field to a Dictionary. A Null field (a driver sending
// null for an optional map) is treated as an empty map.
func asMap(v any, what string) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("bolt: %s is %T, want a map", what, v)
	}
	return m, nil
}

// asStringList coerces a decoded field to a list of strings. A Null field is
// treated as an empty list.
func asStringList(v any, what string) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	xs, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("bolt: %s is %T, want a list", what, v)
	}
	out := make([]string, len(xs))
	for i, e := range xs {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("bolt: %s[%d] is %T, want a string", what, i, e)
		}
		out[i] = s
	}
	return out, nil
}
