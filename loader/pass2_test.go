package loader

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// memFS returns a fresh in-memory VFS for use in bulk-load tests that need a
// pager but must not touch the filesystem.
func memFS() vfs.VFS { return vfs.NewMem() }

// --- value parser tests ---

func TestParseCSVFieldString(t *testing.T) {
	v, ok, err := parseCSVField("hello", PropString, false, ';')
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	s, _ := v.AsString()
	if s != "hello" {
		t.Errorf("got %q, want %q", s, "hello")
	}
}

func TestParseCSVFieldEmpty(t *testing.T) {
	_, ok, err := parseCSVField("", PropString, false, ';')
	if err != nil || ok {
		t.Fatalf("empty field: err=%v ok=%v (want ok=false)", err, ok)
	}
}

func TestParseCSVFieldInt(t *testing.T) {
	v, ok, err := parseCSVField("42", PropInt, false, ';')
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	n, _ := v.AsInt()
	if n != 42 {
		t.Errorf("got %d, want 42", n)
	}
}

func TestParseCSVFieldFloat(t *testing.T) {
	v, ok, err := parseCSVField("3.14", PropFloat, false, ';')
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	f, _ := v.AsFloat()
	if f < 3.13 || f > 3.15 {
		t.Errorf("got %f, want ~3.14", f)
	}
}

func TestParseCSVFieldBoolTrue(t *testing.T) {
	for _, s := range []string{"true", "True", "TRUE", "1", "yes"} {
		v, ok, err := parseCSVField(s, PropBool, false, ';')
		if err != nil || !ok {
			t.Errorf("%q: err=%v ok=%v", s, err, ok)
			continue
		}
		b, _ := v.AsBool()
		if !b {
			t.Errorf("%q: got false, want true", s)
		}
	}
}

func TestParseCSVFieldBoolFalse(t *testing.T) {
	for _, s := range []string{"false", "False", "FALSE", "0", "no"} {
		v, ok, err := parseCSVField(s, PropBool, false, ';')
		if err != nil || !ok {
			t.Errorf("%q: err=%v ok=%v", s, err, ok)
			continue
		}
		b, _ := v.AsBool()
		if b {
			t.Errorf("%q: got true, want false", s)
		}
	}
}

func TestParseCSVFieldIntBadInput(t *testing.T) {
	_, _, err := parseCSVField("not-a-number", PropInt, false, ';')
	if err == nil {
		t.Error("expected error parsing non-integer as int")
	}
}

func TestParseCSVFieldList(t *testing.T) {
	v, ok, err := parseCSVField("English;French;Italian", PropString, true, ';')
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	if v.Type() != value.TypeList {
		t.Fatalf("type: got %v, want LIST", v.Type())
	}
	elems, _ := v.AsList()
	if len(elems) != 3 {
		t.Fatalf("list len: got %d, want 3", len(elems))
	}
	s, _ := elems[0].AsString()
	if s != "English" {
		t.Errorf("elem 0: got %q, want English", s)
	}
}

// --- pass 2 tests ---

func TestPass2SingleGroupProps(t *testing.T) {
	// Three Person nodes with name and age.
	nodeCSV := ":ID(p),name:string,age:int,:LABEL\np1,Ada,30,Person\np2,Bob,25,Person\np3,Cy,42,Person\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	fb, err := l.Pass2BuildNodeColumns(memFS(), "test.gr")
	if err != nil {
		t.Fatalf("pass2: %v", err)
	}
	defer fb.Close()

	// Group 0 (Person): check name and age columns.
	s := fb.nodeStore
	nameTok := uint32(l.catalog.PropKeyToken("name"))
	ageTok := uint32(l.catalog.PropKeyToken("age"))

	// Dense position 0 = Ada, 1 = Bob, 2 = Cy (single group → globalPos == denseID).
	checkString := func(pos uint64, want string) {
		t.Helper()
		v, ok, err := s.Get(nameTok, pos)
		if err != nil || !ok {
			t.Errorf("pos %d name: err=%v ok=%v", pos, err, ok)
			return
		}
		got, _ := v.AsString()
		if got != want {
			t.Errorf("pos %d name: got %q, want %q", pos, got, want)
		}
	}
	checkInt := func(pos uint64, want int64) {
		t.Helper()
		v, ok, err := s.Get(ageTok, pos)
		if err != nil || !ok {
			t.Errorf("pos %d age: err=%v ok=%v", pos, err, ok)
			return
		}
		got, _ := v.AsInt()
		if got != want {
			t.Errorf("pos %d age: got %d, want %d", pos, got, want)
		}
	}

	checkString(0, "Ada")
	checkString(1, "Bob")
	checkString(2, "Cy")
	checkInt(0, 30)
	checkInt(1, 25)
	checkInt(2, 42)
}

func TestPass2TwoGroups(t *testing.T) {
	// Person and Movie in separate sources.
	personCSV := ":ID(p),name:string,:LABEL\np1,Alice,Person\np2,Bob,Person\n"
	movieCSV := ":ID(m),title:string,:LABEL\nm1,Matrix,Movie\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(personCSV)}},
			{readers: []io.Reader{strings.NewReader(movieCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	fb, err := l.Pass2BuildNodeColumns(memFS(), "test.gr")
	if err != nil {
		t.Fatalf("pass2: %v", err)
	}
	defer fb.Close()

	cat := l.Catalog()
	// Person is first-seen primary label => group 0; Movie => group 1.
	// Global positions: Person 0..1, Movie starts at groupBase[1]=2.
	nameTok := uint32(cat.PropKeyToken("name"))
	titleTok := uint32(cat.PropKeyToken("title"))

	s := fb.nodeStore

	v0, ok0, err0 := s.Get(nameTok, 0) // Alice at globalPos 0
	if err0 != nil || !ok0 {
		t.Fatalf("group 0 name[0]: err=%v ok=%v", err0, ok0)
	}
	if n, _ := v0.AsString(); n != "Alice" {
		t.Errorf("group 0 name[0]: got %q, want Alice", n)
	}

	movieBase := fb.groupBase[1] // global start of Movie group
	v1, ok1, err1 := s.Get(titleTok, movieBase)
	if err1 != nil || !ok1 {
		t.Fatalf("group 1 title[0]: err=%v ok=%v", err1, ok1)
	}
	if n, _ := v1.AsString(); n != "Matrix" {
		t.Errorf("group 1 title[0]: got %q, want Matrix", n)
	}
}

func TestPass2GapFillAbsent(t *testing.T) {
	// Three nodes but p2 has an empty name => absent cell at pos 1.
	nodeCSV := ":ID(p),name:string,:LABEL\np1,Ada,Person\np2,,Person\np3,Cy,Person\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	fb, err := l.Pass2BuildNodeColumns(memFS(), "test.gr")
	if err != nil {
		t.Fatalf("pass2: %v", err)
	}
	defer fb.Close()

	s := fb.nodeStore
	nameTok := uint32(l.catalog.PropKeyToken("name"))

	// pos 0 and 2 should be present; pos 1 absent (single group → globalPos == denseID).
	_, ok0, _ := s.Get(nameTok, 0)
	_, ok1, _ := s.Get(nameTok, 1)
	_, ok2, _ := s.Get(nameTok, 2)

	if !ok0 {
		t.Error("pos 0: want present, got absent")
	}
	if ok1 {
		t.Error("pos 1: want absent, got present")
	}
	if !ok2 {
		t.Error("pos 2: want present, got absent")
	}
}

func TestPass2SkippedRowHasNoColumn(t *testing.T) {
	// p1 appears twice; second is skipped; only 2 nodes reach the id-map.
	nodeCSV := ":ID(p),name:string\np1,Ada\np1,dup\np2,Bob\n"

	l := New(Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
		OnDuplicateID: Skip,
	})
	if err := l.Pass1ScanNodes(); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	fb, err := l.Pass2BuildNodeColumns(memFS(), "test.gr")
	if err != nil {
		t.Fatalf("pass2: %v", err)
	}
	defer fb.Close()

	s := fb.nodeStore
	nameTok := uint32(l.catalog.PropKeyToken("name"))

	// Two accepted nodes: p1=Ada at pos 0, p2=Bob at pos 1 (single group → globalPos == denseID).
	v0, ok0, _ := s.Get(nameTok, 0)
	v1, ok1, _ := s.Get(nameTok, 1)

	if !ok0 {
		t.Error("pos 0: want present")
	} else if s0, _ := v0.AsString(); s0 != "Ada" {
		t.Errorf("pos 0: got %q, want Ada", s0)
	}
	if !ok1 {
		t.Error("pos 1: want present")
	} else if s1, _ := v1.AsString(); s1 != "Bob" {
		t.Errorf("pos 1: got %q, want Bob", s1)
	}
}

// TestColBuilderFlush verifies that colBuilder.Flush writes the final partial
// segment and the cells are readable back via the store.
func TestColBuilderFlush(t *testing.T) {
	fs := memFS()
	p, err := openTestPager(t, fs)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	store, err := colsegstore.CreateStore(p)
	if err != nil {
		t.Fatal(err)
	}

	const key = uint32(0)
	b := newColBuilder(store, key, value.TypeString, 0)

	for i := range 5 {
		err := b.Append(uint64(i), colseg.Cell{Present: true, Value: value.String("v" + strconv.Itoa(i))})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := b.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	for i := range 5 {
		v, ok, err := store.Get(key, uint64(i))
		if err != nil || !ok {
			t.Errorf("pos %d: err=%v ok=%v", i, err, ok)
			continue
		}
		s, _ := v.AsString()
		want := "v" + strconv.Itoa(i)
		if s != want {
			t.Errorf("pos %d: got %q, want %q", i, s, want)
		}
	}
}

// openTestPager opens an in-memory pager for unit tests.
func openTestPager(t *testing.T, fs vfs.VFS) (*pager.Pager, error) {
	t.Helper()
	return pager.Open(fs, "test.gr", pager.Options{})
}
