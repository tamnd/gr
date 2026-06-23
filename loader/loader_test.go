package loader

import (
	"io"
	"strings"
	"testing"
)

// --- CSV reader tests ---

func TestCSVReaderBasic(t *testing.T) {
	input := "a,b,c\n1,2,3\n4,5,6\n"
	r := newCSVReader(strings.NewReader(input), ',', ';')

	expect := [][]string{{"a", "b", "c"}, {"1", "2", "3"}, {"4", "5", "6"}}
	for i, want := range expect {
		ok, err := r.Next()
		if !ok || err != nil {
			t.Fatalf("row %d: ok=%v err=%v", i, ok, err)
		}
		if got := r.Fields(); !fieldsEq(got, want) {
			t.Errorf("row %d: got %v, want %v", i, got, want)
		}
	}
	ok, _ := r.Next()
	if ok {
		t.Error("expected EOF after 3 rows")
	}
}

func TestCSVReaderQuotedField(t *testing.T) {
	input := `"hello, world","line` + "\n" + `2","plain"`
	r := newCSVReader(strings.NewReader(input+"\n"), ',', ';')
	ok, err := r.Next()
	if !ok || err != nil {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	fields := r.Fields()
	if len(fields) != 3 {
		t.Fatalf("expected 3 fields, got %d: %v", len(fields), fields)
	}
	if fields[0] != "hello, world" {
		t.Errorf("field 0: %q", fields[0])
	}
	if !strings.Contains(fields[1], "line") {
		t.Errorf("field 1 should contain newline: %q", fields[1])
	}
	if fields[2] != "plain" {
		t.Errorf("field 2: %q", fields[2])
	}
}

func TestCSVReaderBOM(t *testing.T) {
	// BOM before the first field must be stripped.
	bom := "\xEF\xBB\xBF"
	input := bom + "id,name\n1,Ada\n"
	r := newCSVReader(strings.NewReader(input), ',', ';')
	ok, err := r.Next()
	if !ok || err != nil {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if r.Fields()[0] != "id" {
		t.Errorf("BOM not stripped; got %q", r.Fields()[0])
	}
}

func TestCSVReaderCRLF(t *testing.T) {
	input := "a,b\r\n1,2\r\n"
	r := newCSVReader(strings.NewReader(input), ',', ';')
	ok, _ := r.Next() // header
	if !ok {
		t.Fatal("expected header row")
	}
	ok, _ = r.Next()
	if !ok {
		t.Fatal("expected data row")
	}
	if !fieldsEq(r.Fields(), []string{"1", "2"}) {
		t.Errorf("got %v", r.Fields())
	}
}

func TestCSVReaderDoubleQuote(t *testing.T) {
	input := `"say ""hello""",world` + "\n"
	r := newCSVReader(strings.NewReader(input), ',', ';')
	ok, err := r.Next()
	if !ok || err != nil {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if r.Fields()[0] != `say "hello"` {
		t.Errorf("escaped quote: got %q", r.Fields()[0])
	}
}

// --- Header grammar tests ---

func TestParseNodeHeaderBasic(t *testing.T) {
	fields := []string{":ID(person)", "name:string", "born:int", ":LABEL"}
	hdr, err := parseNodeHeader(fields, "")
	if err != nil {
		t.Fatalf("parseNodeHeader: %v", err)
	}
	if hdr.IDCol != 0 {
		t.Errorf("IDCol: got %d, want 0", hdr.IDCol)
	}
	if hdr.LblCol != 3 {
		t.Errorf("LblCol: got %d, want 3", hdr.LblCol)
	}
	if hdr.Cols[0].IDSpace != "person" {
		t.Errorf("IDSpace: got %q, want %q", hdr.Cols[0].IDSpace, "person")
	}
	if hdr.Cols[1].PropType != PropString {
		t.Errorf("name col: got %v, want PropString", hdr.Cols[1].PropType)
	}
	if hdr.Cols[2].PropType != PropInt {
		t.Errorf("born col: got %v, want PropInt", hdr.Cols[2].PropType)
	}
}

func TestParseNodeHeaderNamedID(t *testing.T) {
	// neo4j-admin import writes the id column with a leading name (`id:ID`); the
	// loader accepts it and treats the column as the id, not a property. A named
	// id with an id space and a type suffix parses too.
	fields := []string{"id:ID", "name:string", ":LABEL"}
	hdr, err := parseNodeHeader(fields, "")
	if err != nil {
		t.Fatalf("parseNodeHeader: %v", err)
	}
	if hdr.IDCol != 0 {
		t.Errorf("IDCol: got %d, want 0", hdr.IDCol)
	}
	if hdr.Cols[0].Role != RoleID {
		t.Errorf("id col role: got %v, want RoleID", hdr.Cols[0].Role)
	}
	if hdr.Cols[0].Name != "id" {
		t.Errorf("id col name: got %q, want %q", hdr.Cols[0].Name, "id")
	}

	fields = []string{"key:ID(person):long", ":LABEL"}
	hdr, err = parseNodeHeader(fields, "")
	if err != nil {
		t.Fatalf("parseNodeHeader named id with space and type: %v", err)
	}
	if hdr.Cols[0].Role != RoleID || hdr.Cols[0].Name != "key" {
		t.Errorf("named id: role=%v name=%q", hdr.Cols[0].Role, hdr.Cols[0].Name)
	}
	if hdr.Cols[0].IDSpace != "person" {
		t.Errorf("named id space: got %q, want %q", hdr.Cols[0].IDSpace, "person")
	}
}

func TestParseRelHeaderNamedEndpoints(t *testing.T) {
	// A relationship header may name its endpoint columns the same way.
	fields := []string{"src:START_ID", "dst:END_ID", ":TYPE"}
	hdr, err := parseRelHeader(fields, "")
	if err != nil {
		t.Fatalf("parseRelHeader: %v", err)
	}
	if hdr.StartCol != 0 || hdr.EndCol != 1 {
		t.Errorf("cols: start=%d end=%d", hdr.StartCol, hdr.EndCol)
	}
	if hdr.Cols[0].Role != RoleStartID || hdr.Cols[1].Role != RoleEndID {
		t.Errorf("roles: start=%v end=%v", hdr.Cols[0].Role, hdr.Cols[1].Role)
	}
}

func TestParseNodeHeaderMissingID(t *testing.T) {
	_, err := parseNodeHeader([]string{"name:string", ":LABEL"}, "")
	if err == nil {
		t.Error("expected error for missing :ID column")
	}
}

func TestParseNodeHeaderDuplicateProperty(t *testing.T) {
	_, err := parseNodeHeader([]string{":ID", "name:string", "name:int"}, "")
	if err == nil {
		t.Error("expected error for duplicate property key 'name'")
	}
}

func TestParseRelHeaderBasic(t *testing.T) {
	fields := []string{":START_ID(person)", ":END_ID(person)", "since:int", ":TYPE"}
	hdr, err := parseRelHeader(fields, "")
	if err != nil {
		t.Fatalf("parseRelHeader: %v", err)
	}
	if hdr.StartCol != 0 || hdr.EndCol != 1 || hdr.TypeCol != 3 {
		t.Errorf("cols: start=%d end=%d type=%d", hdr.StartCol, hdr.EndCol, hdr.TypeCol)
	}
	if hdr.Cols[0].IDSpace != "person" {
		t.Errorf("start id space: %q", hdr.Cols[0].IDSpace)
	}
}

func TestParseRelHeaderMissingStart(t *testing.T) {
	_, err := parseRelHeader([]string{":END_ID", ":TYPE"}, "")
	if err == nil {
		t.Error("expected error for missing :START_ID")
	}
}

func TestParseRelHeaderNoTypeAndNoPrefix(t *testing.T) {
	_, err := parseRelHeader([]string{":START_ID", ":END_ID"}, "")
	if err == nil {
		t.Error("expected error: no :TYPE column and no prefix type")
	}
}

func TestParseRelHeaderPrefixType(t *testing.T) {
	// When a prefix type is given, :TYPE column is not required.
	_, err := parseRelHeader([]string{":START_ID", ":END_ID", "weight:float"}, "KNOWS")
	if err != nil {
		t.Errorf("unexpected error with prefix type: %v", err)
	}
}

func TestParseColDescIgnore(t *testing.T) {
	cd, err := parseColDesc(":IGNORE")
	if err != nil {
		t.Fatal(err)
	}
	if cd.Role != RoleIgnore {
		t.Errorf("got %v, want RoleIgnore", cd.Role)
	}
}

func TestParseColDescListType(t *testing.T) {
	cd, err := parseColDesc("emails:string[]")
	if err != nil {
		t.Fatal(err)
	}
	if cd.Role != RoleProperty {
		t.Errorf("got %v, want RoleProperty", cd.Role)
	}
	if cd.PropType != PropString {
		t.Errorf("type: got %v, want PropString", cd.PropType)
	}
	if !cd.IsList {
		t.Error("expected IsList")
	}
}

// --- Pass 1 tests ---

func TestPass1BasicNodeScan(t *testing.T) {
	// Three nodes, two labels, simple case.
	nodeCSV := ":ID(person),name:string,:LABEL\np1,Ada,Person\np2,Bob,Person\np3,Cy,Person\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if l.Stats().Nodes != 3 {
		t.Errorf("nodes: got %d, want 3", l.Stats().Nodes)
	}
	// All in the same group (same primary label).
	idmap := l.IDMap()
	for _, id := range []string{"p1", "p2", "p3"} {
		ent, ok := idmap.Get("person", id)
		if !ok {
			t.Errorf("id %q not found", id)
			continue
		}
		if ent.Group != 0 {
			t.Errorf("id %q: group %d, want 0", id, ent.Group)
		}
	}
	// Dense ids should be 0, 1, 2 in order.
	ent0, _ := idmap.Get("person", "p1")
	ent1, _ := idmap.Get("person", "p2")
	ent2, _ := idmap.Get("person", "p3")
	if ent0.DenseID != 0 || ent1.DenseID != 1 || ent2.DenseID != 2 {
		t.Errorf("dense ids: %d %d %d, want 0 1 2", ent0.DenseID, ent1.DenseID, ent2.DenseID)
	}
}

func TestPass1TwoGroups(t *testing.T) {
	// Two label groups: Person and Movie.
	nodeCSV := ":ID,:LABEL\nn1,Person\nn2,Movie\nn3,Person\nn4,Movie\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if l.Stats().Nodes != 4 {
		t.Errorf("nodes: got %d, want 4", l.Stats().Nodes)
	}
	idmap := l.IDMap()

	e1, _ := idmap.Get("", "n1") // Person
	e2, _ := idmap.Get("", "n2") // Movie
	e3, _ := idmap.Get("", "n3") // Person
	e4, _ := idmap.Get("", "n4") // Movie

	// n1 and n3 are both Person => same group, dense ids 0 and 1.
	if e1.Group != e3.Group {
		t.Errorf("n1 and n3 should be in the same group")
	}
	// n2 and n4 are both Movie => same group, dense ids 0 and 1.
	if e2.Group != e4.Group {
		t.Errorf("n2 and n4 should be in the same group")
	}
	if e1.Group == e2.Group {
		t.Errorf("Person and Movie should be in different groups")
	}
	if e1.DenseID != 0 || e3.DenseID != 1 {
		t.Errorf("Person dense ids: n1=%d n3=%d, want 0 1", e1.DenseID, e3.DenseID)
	}
	if e2.DenseID != 0 || e4.DenseID != 1 {
		t.Errorf("Movie dense ids: n2=%d n4=%d, want 0 1", e2.DenseID, e4.DenseID)
	}
}

func TestPass1DuplicateIDFail(t *testing.T) {
	nodeCSV := ":ID\np1\np1\n" // p1 appears twice
	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
		OnDuplicateID: Fail,
	})
	err := l.Pass1ScanNodes()
	if err == nil {
		t.Error("expected error on duplicate id with Fail policy")
	}
}

func TestPass1DuplicateIDSkip(t *testing.T) {
	nodeCSV := ":ID\np1\np1\np2\n"
	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
		OnDuplicateID: Skip,
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if l.Stats().Nodes != 2 { // p1 (once) + p2
		t.Errorf("nodes: got %d, want 2", l.Stats().Nodes)
	}
	if l.Stats().DupNodes != 1 {
		t.Errorf("dup nodes: got %d, want 1", l.Stats().DupNodes)
	}
}

func TestPass1MissingIDFail(t *testing.T) {
	nodeCSV := ":ID,name\n,Ada\n"
	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
		OnMissingID: Fail,
	})
	if err := l.Pass1ScanNodes(); err == nil {
		t.Error("expected error on missing id with Fail policy")
	}
}

func TestPass1MissingIDSkip(t *testing.T) {
	nodeCSV := ":ID,name\n,Ada\np1,Bob\n"
	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
		OnMissingID: Skip,
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if l.Stats().Nodes != 1 {
		t.Errorf("nodes: got %d, want 1", l.Stats().Nodes)
	}
}

func TestPass1IDSpaces(t *testing.T) {
	// Two node sources with different id spaces; the same external "1" in each.
	personCSV := ":ID(person),:LABEL\n1,Person\n2,Person\n"
	movieCSV := ":ID(movie),:LABEL\n1,Movie\n2,Movie\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(personCSV)}},
			{readers: []io.Reader{strings.NewReader(movieCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if l.Stats().Nodes != 4 {
		t.Errorf("nodes: got %d, want 4", l.Stats().Nodes)
	}
	idmap := l.IDMap()
	pe1, ok1 := idmap.Get("person", "1")
	me1, ok2 := idmap.Get("movie", "1")
	if !ok1 || !ok2 {
		t.Fatal("expected both (person,1) and (movie,1)")
	}
	// They must be in different groups (different labels).
	if pe1.Group == me1.Group {
		t.Errorf("(person,1) and (movie,1) should be in different groups")
	}
}

func TestPass1PrefixLabel(t *testing.T) {
	// When Label is set on the source and there is no :LABEL column, all nodes
	// in the source land in that label's group.
	nodeCSV := ":ID\np1\np2\n"
	l := New(Options{
		Nodes: []NodeSource{
			{Label: "Person", readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	idmap := l.IDMap()
	e, ok := idmap.Get("", "p1")
	if !ok {
		t.Fatal("p1 not found")
	}
	cat := l.Catalog()
	if cat.LabelName(cat.groups[e.Group].primaryToken) != "Person" {
		t.Errorf("group primary label: got %q", cat.LabelName(cat.groups[e.Group].primaryToken))
	}
}

func TestPass1MultipleNodeSources(t *testing.T) {
	// Two sources, each contributing a different label group.
	personCSV := ":ID(person),:LABEL\np1,Person\np2,Person\n"
	movieCSV := ":ID(movie),:LABEL\nm1,Movie\n"
	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(personCSV)}},
			{readers: []io.Reader{strings.NewReader(movieCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if l.Stats().Nodes != 3 {
		t.Errorf("nodes: got %d, want 3", l.Stats().Nodes)
	}
}

func TestBadLineSink(t *testing.T) {
	var buf strings.Builder
	sink := newBadLineSink(&buf)
	sink.Reject(Reject{SourceFile: "f.csv", LineNo: 3, Code: CodeMissingID, Detail: "empty", RawRow: ",,,"})
	sink.Reject(Reject{SourceFile: "f.csv", LineNo: 5, Code: CodeDupID, Detail: "dup", RawRow: "1,x"})
	sink.Flush()

	if sink.Total() != 2 {
		t.Errorf("total: got %d, want 2", sink.Total())
	}
	if sink.Count(CodeMissingID) != 1 {
		t.Errorf("missing_id count: got %d, want 1", sink.Count(CodeMissingID))
	}
	content := buf.String()
	if !strings.Contains(content, "missing_id") {
		t.Errorf("bad-line file missing 'missing_id': %s", content)
	}
	if !strings.Contains(content, "dup_id") {
		t.Errorf("bad-line file missing 'dup_id': %s", content)
	}
}

func TestSplitArrayField(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"English", []string{"English"}},
		{"English;French;Italian", []string{"English", "French", "Italian"}},
	}
	for _, tc := range cases {
		got := splitArrayField(tc.input, ';')
		if !fieldsEq(got, tc.want) {
			t.Errorf("splitArrayField(%q): got %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- helpers ---

func fieldsEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
