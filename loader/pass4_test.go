package loader

import (
	"io"
	"strings"
	"testing"

	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/idmap"
	"github.com/tamnd/gr/node"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/rel"
	"github.com/tamnd/gr/stats"
	"github.com/tamnd/gr/store"
)

// runPass14 runs all four passes and returns the pager for inspection.
// The caller must call p.Close().
func runPass14(t *testing.T, nodeCSV, relCSV string, relType string) *pager.Pager {
	t.Helper()
	opts := Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(nodeCSV)}},
		},
	}
	if relCSV != "" {
		opts.Relationships = []RelSource{
			{Type: relType, readers: []io.Reader{strings.NewReader(relCSV)}},
		}
	}
	l := New(opts)
	fs := memFS()
	if err := l.Pass4FinalizeAll(fs, "test.gr"); err != nil {
		t.Fatalf("Pass4FinalizeAll: %v", err)
	}
	p, err := pager.Open(fs, "test.gr", pager.Options{})
	if err != nil {
		t.Fatalf("reopen pager: %v", err)
	}
	return p
}

func TestPass4CatalogIntern(t *testing.T) {
	// Two Person nodes; catalog should have Person label and name propKey.
	p := runPass14(t, ":ID(p),name:string,:LABEL\np1,Alice,Person\np2,Bob,Person\n", "", "")
	defer p.Close()

	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("open sections: %v", err)
	}
	cat, err := catalog.Open(p, secs)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}

	tok, ok := cat.Lookup(catalog.KindLabel, "Person")
	if !ok {
		t.Error("Person label not interned")
	} else if tok != 0 {
		t.Errorf("Person token: got %d, want 0", tok)
	}

	_, ok = cat.Lookup(catalog.KindPropKey, "name")
	if !ok {
		t.Error("name propkey not interned")
	}
}

func TestPass4NodeRecords(t *testing.T) {
	// Two Person nodes; node store should have two records.
	p := runPass14(t, ":ID(p),:LABEL\np1,Person\np2,Person\n", "", "")
	defer p.Close()

	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("open sections: %v", err)
	}
	ns, err := node.Open(p, secs)
	if err != nil {
		t.Fatalf("open node store: %v", err)
	}
	if ns.Count() != 2 {
		t.Errorf("node count: got %d, want 2", ns.Count())
	}
	if !ns.Exists(0) {
		t.Error("node 0 not live")
	}
	if !ns.Exists(1) {
		t.Error("node 1 not live")
	}
}

func TestPass4NodeLabels(t *testing.T) {
	// Person node should carry the Person label token.
	p := runPass14(t, ":ID(p),:LABEL\np1,Person\n", "", "")
	defer p.Close()

	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("open sections: %v", err)
	}
	cat, err := catalog.Open(p, secs)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	ns, err := node.Open(p, secs)
	if err != nil {
		t.Fatalf("open node store: %v", err)
	}

	personTok, ok := cat.Lookup(catalog.KindLabel, "Person")
	if !ok {
		t.Fatal("Person not interned")
	}
	has, err := ns.HasLabel(0, personTok)
	if err != nil {
		t.Fatalf("HasLabel: %v", err)
	}
	if !has {
		t.Error("node 0 should have Person label")
	}
}

func TestPass4IDMapNodes(t *testing.T) {
	// Three nodes; id-map should have 3 KindNode entries.
	p := runPass14(t, ":ID(p),:LABEL\np1,Person\np2,Person\np3,Person\n", "", "")
	defer p.Close()

	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("open sections: %v", err)
	}
	im, err := idmap.Open(p, secs)
	if err != nil {
		t.Fatalf("open idmap: %v", err)
	}

	if im.Allocated(idmap.KindNode) != 3 {
		t.Errorf("KindNode allocated: got %d, want 3", im.Allocated(idmap.KindNode))
	}
}

func TestPass4RelRecords(t *testing.T) {
	// Two nodes, one KNOWS rel.
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\n"
	relCSV := ":START_ID(p),:END_ID(p)\np1,p2\n"
	p := runPass14(t, nodeCSV, relCSV, "KNOWS")
	defer p.Close()

	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("open sections: %v", err)
	}
	rs, err := rel.Open(p, secs)
	if err != nil {
		t.Fatalf("open rel store: %v", err)
	}
	if rs.Count() != 1 {
		t.Errorf("rel count: got %d, want 1", rs.Count())
	}
}

func TestPass4Stats(t *testing.T) {
	// Two Person nodes and one KNOWS rel; check stats.
	nodeCSV := ":ID(p),:LABEL\np1,Person\np2,Person\n"
	relCSV := ":START_ID(p),:END_ID(p)\np1,p2\n"
	p := runPass14(t, nodeCSV, relCSV, "KNOWS")
	defer p.Close()

	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("open sections: %v", err)
	}
	cat, err := catalog.Open(p, secs)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	st, err := stats.Open(p, secs)
	if err != nil {
		t.Fatalf("open stats: %v", err)
	}

	personTok, _ := cat.Lookup(catalog.KindLabel, "Person")
	cnt, err := st.LabelCount(personTok)
	if err != nil {
		t.Fatalf("LabelCount: %v", err)
	}
	if cnt != 2 {
		t.Errorf("Person count: got %d, want 2", cnt)
	}

	knowsTok, _ := cat.Lookup(catalog.KindRelType, "KNOWS")
	rcnt, err := st.RelTypeCount(knowsTok)
	if err != nil {
		t.Fatalf("RelTypeCount: %v", err)
	}
	if rcnt != 1 {
		t.Errorf("KNOWS count: got %d, want 1", rcnt)
	}
}

func TestPass4TwoGroups(t *testing.T) {
	// Person and Movie in separate sources; check two node groups.
	opts := Options{
		Nodes: []NodeSource{
			{readers: []io.Reader{strings.NewReader(":ID(p),:LABEL\np1,Person\np2,Person\n")}},
			{readers: []io.Reader{strings.NewReader(":ID(m),:LABEL\nm1,Movie\n")}},
		},
	}
	l := New(opts)
	fs := memFS()
	if err := l.Pass4FinalizeAll(fs, "test.gr"); err != nil {
		t.Fatalf("Pass4FinalizeAll: %v", err)
	}

	p, err := pager.Open(fs, "test.gr", pager.Options{})
	if err != nil {
		t.Fatalf("reopen pager: %v", err)
	}
	defer p.Close()

	secs, err := store.OpenSections(p)
	if err != nil {
		t.Fatalf("open sections: %v", err)
	}
	ns, err := node.Open(p, secs)
	if err != nil {
		t.Fatalf("open node store: %v", err)
	}
	// 3 total nodes: Person 0,1 (group 0) + Movie 2 (group 1).
	if ns.Count() != 3 {
		t.Errorf("node count: got %d, want 3", ns.Count())
	}
}
