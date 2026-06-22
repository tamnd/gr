package bolt

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/tamnd/gr/pack"
)

// encodeStruct marshals a structure to its PackStream bytes, the framed message
// body DecodeRequest reads.
func encodeStruct(t *testing.T, s pack.Structure) []byte {
	t.Helper()
	b, err := pack.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestDecodeRunSpecTrace decodes the RUN body from doc 18 §3.4/§5.6 and checks
// the parsed fields: query "RETURN 1", empty params, empty extra.
func TestDecodeRunSpecTrace(t *testing.T) {
	body := hex(t, "B3 10 88 52 45 54 55 52 4E 20 31 A0 A0")
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	run, ok := req.(Run)
	if !ok {
		t.Fatalf("decoded %T, want Run", req)
	}
	if run.Query != "RETURN 1" {
		t.Errorf("query %q, want RETURN 1", run.Query)
	}
	if len(run.Params) != 0 || len(run.Extra) != 0 {
		t.Errorf("params %v extra %v, want empty", run.Params, run.Extra)
	}
	if run.Signature() != SigRun {
		t.Errorf("signature 0x%02X, want 0x10", run.Signature())
	}
}

// TestDecodeRequestTypes decodes one of each message and checks its concrete Go
// type and signature.
func TestDecodeRequestTypes(t *testing.T) {
	cases := []struct {
		name string
		s    pack.Structure
		want Request
	}{
		{"hello", pack.Structure{Tag: SigHello, Fields: []any{map[string]any{"user_agent": "gr-test/1.0"}}}, Hello{Extra: map[string]any{"user_agent": "gr-test/1.0"}}},
		{"logon", pack.Structure{Tag: SigLogon, Fields: []any{map[string]any{"scheme": "none"}}}, Logon{Auth: map[string]any{"scheme": "none"}}},
		{"logoff", pack.Structure{Tag: SigLogoff, Fields: []any{}}, Logoff{}},
		{"goodbye", pack.Structure{Tag: SigGoodbye, Fields: []any{}}, Goodbye{}},
		{"reset", pack.Structure{Tag: SigReset, Fields: []any{}}, Reset{}},
		{"begin", pack.Structure{Tag: SigBegin, Fields: []any{map[string]any{"mode": "r"}}}, Begin{Extra: map[string]any{"mode": "r"}}},
		{"commit", pack.Structure{Tag: SigCommit, Fields: []any{}}, Commit{}},
		{"rollback", pack.Structure{Tag: SigRollback, Fields: []any{}}, Rollback{}},
		{"pull", pack.Structure{Tag: SigPull, Fields: []any{map[string]any{"n": int64(1000)}}}, Pull{Extra: map[string]any{"n": int64(1000)}}},
		{"discard", pack.Structure{Tag: SigDiscard, Fields: []any{map[string]any{"n": int64(-1)}}}, Discard{Extra: map[string]any{"n": int64(-1)}}},
		{"telemetry", pack.Structure{Tag: SigTelemetry, Fields: []any{int64(2)}}, Telemetry{API: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := DecodeRequest(encodeStruct(t, tc.s))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(req, tc.want) {
				t.Errorf("decoded %#v, want %#v", req, tc.want)
			}
		})
	}
}

// TestDecodeRoute decodes a ROUTE with its three fields.
func TestDecodeRoute(t *testing.T) {
	s := pack.Structure{Tag: SigRoute, Fields: []any{
		map[string]any{"address": "localhost:7687"},
		[]any{"bm:1", "bm:2"},
		map[string]any{"db": "neo4j"},
	}}
	req, err := DecodeRequest(encodeStruct(t, s))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	route, ok := req.(Route)
	if !ok {
		t.Fatalf("decoded %T, want Route", req)
	}
	if !reflect.DeepEqual(route.Bookmarks, []string{"bm:1", "bm:2"}) {
		t.Errorf("bookmarks %v", route.Bookmarks)
	}
	if route.Routing["address"] != "localhost:7687" || route.Extra["db"] != "neo4j" {
		t.Errorf("routing %v extra %v", route.Routing, route.Extra)
	}
}

// TestPullDiscardAccessors checks the streaming-control accessors default to -1
// when a key is absent and read the value when present (doc 18 §5.7, §5.8).
func TestPullDiscardAccessors(t *testing.T) {
	if p := (Pull{Extra: map[string]any{}}); p.N() != -1 || p.Qid() != -1 {
		t.Errorf("empty pull N=%d Qid=%d, want -1 -1", p.N(), p.Qid())
	}
	p := Pull{Extra: map[string]any{"n": int64(500), "qid": int64(3)}}
	if p.N() != 500 || p.Qid() != 3 {
		t.Errorf("pull N=%d Qid=%d, want 500 3", p.N(), p.Qid())
	}
	d := Discard{Extra: map[string]any{"n": int64(-1)}}
	if d.N() != -1 || d.Qid() != -1 {
		t.Errorf("discard N=%d Qid=%d", d.N(), d.Qid())
	}
}

// TestDecodeNullMapField confirms a null where a map is expected decodes to an
// empty map, so a driver sending null params is accepted (doc 18 §5.6).
func TestDecodeNullMapField(t *testing.T) {
	s := pack.Structure{Tag: SigRun, Fields: []any{"RETURN 1", nil, nil}}
	req, err := DecodeRequest(encodeStruct(t, s))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	run := req.(Run)
	if run.Params == nil || len(run.Params) != 0 || run.Extra == nil || len(run.Extra) != 0 {
		t.Errorf("null fields not coerced to empty maps: %#v", run)
	}
}

// TestDecodeErrors confirms malformed messages are loud errors (doc 18 §5.2,
// §13.4): unknown signature, wrong field count, wrong field type, a non-structure
// body.
func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"unknown-signature", encodeStruct(t, pack.Structure{Tag: 0x99, Fields: []any{}})},
		{"run-wrong-arity", encodeStruct(t, pack.Structure{Tag: SigRun, Fields: []any{"q"}})},
		{"run-bad-query-type", encodeStruct(t, pack.Structure{Tag: SigRun, Fields: []any{int64(1), map[string]any{}, map[string]any{}}})},
		{"hello-not-map", encodeStruct(t, pack.Structure{Tag: SigHello, Fields: []any{"not a map"}})},
		{"telemetry-not-int", encodeStruct(t, pack.Structure{Tag: SigTelemetry, Fields: []any{"x"}})},
		{"reset-extra-field", encodeStruct(t, pack.Structure{Tag: SigReset, Fields: []any{int64(1)}})},
		{"not-a-structure", func() []byte { b, _ := pack.Marshal("hello"); return b }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeRequest(tc.body); err == nil {
				t.Errorf("%s decoded without error", tc.name)
			}
		})
	}
}

// TestReplyBuilders checks the SUCCESS/RECORD/IGNORED/FAILURE structures carry
// the right signature and fields, and round-trip through the codec (doc 18 §5).
func TestReplyBuilders(t *testing.T) {
	success := Success(map[string]any{"fields": []any{"n"}})
	if success.Tag != SigSuccess || len(success.Fields) != 1 {
		t.Errorf("success %#v", success)
	}
	if Success(nil).Fields[0] == nil {
		t.Error("nil success metadata should encode as an empty map")
	}

	record := Record([]any{int64(1)})
	if record.Tag != SigRecord {
		t.Errorf("record tag 0x%02X", record.Tag)
	}

	ignored := Ignored()
	if ignored.Tag != SigIgnored || len(ignored.Fields) != 0 {
		t.Errorf("ignored %#v", ignored)
	}

	failure := Failure("Neo.ClientError.Statement.SyntaxError", "boom")
	meta := failure.Fields[0].(map[string]any)
	if failure.Tag != SigFailure || meta["code"] != "Neo.ClientError.Statement.SyntaxError" || meta["message"] != "boom" {
		t.Errorf("failure %#v", failure)
	}

	// Each reply must round-trip: encode then decode to an equal structure.
	for _, s := range []pack.Structure{success, record, ignored, failure} {
		b, err := pack.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, err := pack.Unmarshal(b)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(got, s) {
			t.Errorf("round trip %#v != %#v", got, s)
		}
	}
}

// TestRecordSpecTrace checks a RECORD carrying the single value 1 encodes to the
// structure shape from doc 18 §5.9 (B1 71 then a 1-element list 91 01).
func TestRecordSpecTrace(t *testing.T) {
	b, err := pack.Marshal(Record([]any{int64(1)}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := hex(t, "B1 71 91 01")
	if !bytes.Equal(b, want) {
		t.Errorf("record % X, want % X", b, want)
	}
}
