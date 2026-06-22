package gr

import (
	"errors"
	"testing"
	"time"

	"github.com/tamnd/gr/bolt"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// boltDB opens a fresh in-memory database for the Bolt adapter tests.
func boltDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:.gr", Options{VFS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// runBolt runs one statement through a fresh adapter transaction, drains the
// cursor into rows, and commits, the way an auto-commit Bolt RUN does.
func runBolt(t *testing.T, h bolt.Handler, query string, params map[string]value.Value) ([][]value.Value, bolt.Summary, bolt.Tx) {
	t.Helper()
	tx, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cur, err := tx.Run(query, params)
	if err != nil {
		t.Fatalf("run %q: %v", query, err)
	}
	var rows [][]value.Value
	for {
		row, ok, err := cur.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		rows = append(rows, row)
	}
	summary := cur.Summary()
	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return rows, summary, tx
}

// TestBoltAdapterWriteThenRead runs a write and a read through the adapter and
// confirms the write commits and the read sees it (doc 18 §5.6).
func TestBoltAdapterWriteThenRead(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()

	_, summary, tx := runBolt(t, h, "CREATE (n:Person {name: 'Ada', age: 36}) RETURN n", nil)
	if summary.Type != "w" {
		t.Errorf("create query type %q, want w", summary.Type)
	}
	if summary.Stats["nodes-created"] != int64(1) {
		t.Errorf("nodes-created %v, want 1", summary.Stats["nodes-created"])
	}
	bm, err := tx.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if bm == "" {
		t.Error("commit returned an empty bookmark")
	}

	rows, summary, tx := runBolt(t, h, "MATCH (n:Person) RETURN n.name AS name, n.age AS age", nil)
	if summary.Type != "r" {
		t.Errorf("match query type %q, want r", summary.Type)
	}
	if len(rows) != 1 {
		t.Fatalf("matched %d rows, want 1", len(rows))
	}
	name, _ := rows[0][0].AsString()
	age, _ := rows[0][1].AsInt()
	if name != "Ada" || age != 36 {
		t.Errorf("row = (%q, %d), want (Ada, 36)", name, age)
	}
	tx.Commit()
}

// TestBoltAdapterMaterializeNode confirms a returned node handle materializes to
// its labels, properties, and element id (doc 18 §6.10).
func TestBoltAdapterMaterializeNode(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, _, tx := runBolt(t, h, "CREATE (:City {name: 'Oslo'})", nil)
	tx.Commit()

	tx, err := h.Begin(map[string]any{"mode": "r"}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	cur, err := tx.Run("MATCH (n:City) RETURN n", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("next: ok=%v err=%v", ok, err)
	}
	if row[0].Type() != value.TypeNode {
		t.Fatalf("column type %s, want a node handle", row[0].Type())
	}
	id, _ := row[0].AsNode()
	node, err := tx.Materializer().MaterializeNode(id)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(node.Labels) != 1 || node.Labels[0] != "City" {
		t.Errorf("labels %v, want [City]", node.Labels)
	}
	if node.Props["name"] != "Oslo" {
		t.Errorf("props %v, want name=Oslo", node.Props)
	}
	if node.ElementID == "" {
		t.Error("node has no element id")
	}
	cur.Close()
}

// TestBoltAdapterMaterializeRel confirms a returned relationship handle
// materializes to its type, endpoints, and element ids (doc 18 §6.10).
func TestBoltAdapterMaterializeRel(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, _, tx := runBolt(t, h, "CREATE (a:P {n:'a'})-[:KNOWS {since: 2020}]->(b:P {n:'b'})", nil)
	tx.Commit()

	tx, err := h.Begin(map[string]any{"mode": "r"}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	cur, err := tx.Run("MATCH ()-[r:KNOWS]->() RETURN r", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("next: ok=%v err=%v", ok, err)
	}
	if row[0].Type() != value.TypeRel {
		t.Fatalf("column type %s, want a relationship handle", row[0].Type())
	}
	id, _ := row[0].AsRel()
	rel, err := tx.Materializer().MaterializeRel(id)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if rel.Type != "KNOWS" {
		t.Errorf("type %q, want KNOWS", rel.Type)
	}
	if rel.Props["since"] != int64(2020) {
		t.Errorf("props %v, want since=2020", rel.Props)
	}
	if rel.StartElementID == "" || rel.EndElementID == "" || rel.StartElementID == rel.EndElementID {
		t.Errorf("endpoint element ids start=%q end=%q", rel.StartElementID, rel.EndElementID)
	}
	cur.Close()
}

// TestBoltAdapterParams confirms a parameter binds into a query (doc 18 §6.9).
func TestBoltAdapterParams(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, _, tx := runBolt(t, h, "CREATE (:N {v: 7})", nil)
	tx.Commit()

	rows, _, tx := runBolt(t, h, "MATCH (n:N) WHERE n.v = $want RETURN n.v",
		map[string]value.Value{"want": value.Int(7)})
	if len(rows) != 1 {
		t.Fatalf("matched %d rows with the parameter, want 1", len(rows))
	}
	got, _ := rows[0][0].AsInt()
	if got != 7 {
		t.Errorf("n.v = %d, want 7", got)
	}
	tx.Commit()
}

// TestBoltAdapterRollback confirms a rolled-back write is not visible (doc 18
// §5.10).
func TestBoltAdapterRollback(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	tx, err := h.Begin(map[string]any{"mode": "w"}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cur, err := tx.Run("CREATE (:Temp)", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	cur.Close()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	rows, _, tx2 := runBolt(t, h, "MATCH (n:Temp) RETURN n", nil)
	if len(rows) != 0 {
		t.Errorf("rolled-back node is visible: %d rows", len(rows))
	}
	tx2.Commit()
}

// TestBoltAdapterSchema confirms a schema statement runs and auto-commits through
// the adapter, reporting an "s" summary type (doc 18 §5.6).
func TestBoltAdapterSchema(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	_, summary, tx := runBolt(t, h, "CREATE INDEX FOR (n:Person) ON (n.name)", nil)
	if summary.Type != "s" {
		t.Errorf("schema query type %q, want s", summary.Type)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	idx, err := db.Indexes()
	if err != nil {
		t.Fatalf("indexes: %v", err)
	}
	if len(idx) == 0 {
		t.Error("schema statement created no index")
	}
}

// TestBoltAdapterSyntaxError confirms a bad query surfaces as a client status
// error (doc 18 §12).
func TestBoltAdapterSyntaxError(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler()
	tx, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	_, err = tx.Run("THIS IS NOT CYPHER", nil)
	if err == nil {
		t.Fatal("a bad query returned no error")
	}
	var se *bolt.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a bolt.StatusError", err)
	}
	if se.Code == "" || se.Code[:3] != "Neo" {
		t.Errorf("status code %q, want a Neo client code", se.Code)
	}
}

// TestBoltAdapterAuth confirms authentication enforcement (doc 18 §10).
func TestBoltAdapterAuth(t *testing.T) {
	db := boltDB(t)
	if err := db.CreateUser("alice", "secret", RoleReader); err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := db.BoltHandler(WithBoltAuth())

	auth, err := h.Authenticate("basic", "alice", "secret")
	if err != nil {
		t.Errorf("valid credentials rejected: %v", err)
	}
	if len(auth.Roles) != 1 || auth.Roles[0] != RoleReader {
		t.Errorf("authenticated roles %v, want [%s]", auth.Roles, RoleReader)
	}
	if _, err := h.Authenticate("basic", "alice", "wrong"); err == nil {
		t.Error("wrong password accepted")
	}
	if _, err := h.Authenticate("none", "", ""); err == nil {
		t.Error("none scheme accepted while auth is required")
	}

	// With auth off the same none scheme is accepted.
	if _, err := db.BoltHandler().Authenticate("none", "", ""); err != nil {
		t.Errorf("none scheme rejected while auth is off: %v", err)
	}
}

// TestBoltAdapterAuthz confirms per-statement role authorization on the Bolt path (doc
// 18 §10.6): a reader may read but not write, an editor may write, and an account with
// no role may run nothing, the same role model the HTTP surface enforces.
func TestBoltAdapterAuthz(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler(WithBoltAuthFunc(func(scheme, principal, credentials string) ([]string, error) {
		switch principal {
		case "reader":
			return []string{RoleReader}, nil
		case "editor":
			return []string{RoleEditor}, nil
		default:
			return nil, nil // authenticated but role-less
		}
	}))

	run := func(principal, query string) error {
		auth, err := h.Authenticate("basic", principal, "pw")
		if err != nil {
			t.Fatalf("authenticate %s: %v", principal, err)
		}
		tx, err := h.Begin(map[string]any{}, auth)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		cur, err := tx.Run(query, nil)
		if err != nil {
			return err
		}
		return cur.Close()
	}

	forbidden := func(err error) bool {
		var se *bolt.StatusError
		return errors.As(err, &se) && se.Code == "Neo.ClientError.Security.Forbidden"
	}

	if err := run("reader", "RETURN 1"); err != nil {
		t.Errorf("reader read rejected: %v", err)
	}
	if err := run("reader", "CREATE (:T)"); !forbidden(err) {
		t.Errorf("reader write error = %v, want Forbidden", err)
	}
	if err := run("editor", "CREATE (:T)"); err != nil {
		t.Errorf("editor write rejected: %v", err)
	}
	if err := run("nobody", "RETURN 1"); !forbidden(err) {
		t.Errorf("role-less read error = %v, want Forbidden", err)
	}
}

// TestBoltAdapterAuthOffNoAuthz confirms that with authentication off there is no
// authorization, so an anonymous connection runs any statement (doc 18 §11.4).
func TestBoltAdapterAuthOffNoAuthz(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler() // auth off
	auth, _ := h.Authenticate("none", "", "")
	tx, err := h.Begin(map[string]any{}, auth)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	cur, err := tx.Run("CREATE (:T)", nil)
	if err != nil {
		t.Fatalf("anonymous write rejected with auth off: %v", err)
	}
	cur.Close()
}

// TestBoltAdapterAuthFunc confirms WithBoltAuthFunc routes authentication through the
// given verifier, so the Bolt and HTTP transports can share one auth provider (doc 18
// §10.4). The verifier sees the scheme, principal, and credentials, and its result
// decides the connection.
func TestBoltAdapterAuthFunc(t *testing.T) {
	db := boltDB(t)
	var sawScheme, sawPrincipal, sawCreds string
	h := db.BoltHandler(WithBoltAuthFunc(func(scheme, principal, credentials string) ([]string, error) {
		sawScheme, sawPrincipal, sawCreds = scheme, principal, credentials
		if principal == "ada" && credentials == "letmein" {
			return []string{RoleEditor}, nil
		}
		return nil, errors.New("denied")
	}))

	auth, err := h.Authenticate("basic", "ada", "letmein")
	if err != nil {
		t.Errorf("verifier-approved credentials rejected: %v", err)
	}
	if len(auth.Roles) != 1 || auth.Roles[0] != RoleEditor {
		t.Errorf("verifier roles %v, want [%s]", auth.Roles, RoleEditor)
	}
	if sawScheme != "basic" || sawPrincipal != "ada" || sawCreds != "letmein" {
		t.Errorf("verifier saw (%q,%q,%q), want (basic,ada,letmein)", sawScheme, sawPrincipal, sawCreds)
	}
	if _, err := h.Authenticate("basic", "ada", "nope"); err == nil {
		t.Error("verifier-denied credentials accepted")
	}
}

// TestBoltAdapterAdmission confirms the in-flight gate sheds a query as a transient when
// the gate is full and admits it again once the holding cursor closes (doc 18 §8.9).
func TestBoltAdapterAdmission(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler(WithBoltAdmission(NewAdmission(1, 10*time.Millisecond)))

	// Open and hold a cursor, claiming the only slot.
	tx1, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	cur1, err := tx1.Run("RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// A second query finds the gate full and sheds as a retryable transient.
	tx2, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	_, err = tx2.Run("RETURN 2 AS n", nil)
	var se *bolt.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("full-gate run err = %v, want StatusError", err)
	}
	if se.Code != "Neo.TransientError.General.TransientError" {
		t.Errorf("full-gate code = %q, want transient", se.Code)
	}

	// Closing the first cursor frees the slot, so the query is admitted on retry.
	if err := cur1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	tx1.Commit()
	cur2, err := tx2.Run("RETURN 2 AS n", nil)
	if err != nil {
		t.Fatalf("run 2 after slot freed: %v", err)
	}
	cur2.Close()
	tx2.Commit()
}

// TestBoltAdapterQueryMaxTime confirms the wall-clock cap times a query out on the Bolt
// path. A sub-nanosecond cap means the deadline has already passed when the engine checks
// its context, so Run reports a timed out transaction as a Bolt status error.
func TestBoltAdapterQueryMaxTime(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler(WithBoltQueryMaxTime(time.Nanosecond))
	tx, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = tx.Run("RETURN 1 AS n", nil)
	var se *bolt.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("capped run err = %v, want StatusError", err)
	}
	if se.Code != "Neo.ClientError.Transaction.TransactionTimedOut" {
		t.Errorf("capped run code = %q, want TransactionTimedOut", se.Code)
	}
}

// TestBoltAdapterRateLimit confirms the per-principal rate limit throttles a Bolt query
// once the connection's principal has spent its burst. With a tiny rate and a burst of
// one, the first query runs and the second is refused as a retryable transient.
func TestBoltAdapterRateLimit(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler(WithBoltRateLimiter(NewRateLimiter(0.001, 1)))

	tx1, err := h.Begin(map[string]any{}, bolt.Auth{Principal: "alice"})
	if err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	cur1, err := tx1.Run("RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	cur1.Close()
	tx1.Commit()

	// The same principal's next query is throttled.
	tx2, err := h.Begin(map[string]any{}, bolt.Auth{Principal: "alice"})
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	_, err = tx2.Run("RETURN 2 AS n", nil)
	var se *bolt.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("throttled run err = %v, want StatusError", err)
	}
	if se.Code != "Neo.TransientError.General.TransientError" {
		t.Errorf("throttled code = %q, want transient", se.Code)
	}

	// A different principal has its own bucket and is not throttled.
	tx3, err := h.Begin(map[string]any{}, bolt.Auth{Principal: "bob"})
	if err != nil {
		t.Fatalf("begin 3: %v", err)
	}
	cur3, err := tx3.Run("RETURN 3 AS n", nil)
	if err != nil {
		t.Fatalf("bob's run throttled by alice's spend: %v", err)
	}
	cur3.Close()
	tx3.Commit()
}

// TestBoltAdapterQueryMaxTimeAdmits confirms a generous cap does not interfere with a
// normal query, and that the cursor closes cleanly with the cap's cancel wired in.
func TestBoltAdapterQueryMaxTimeAdmits(t *testing.T) {
	db := boltDB(t)
	h := db.BoltHandler(WithBoltQueryMaxTime(time.Minute))
	tx, err := h.Begin(map[string]any{}, bolt.Auth{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cur, err := tx.Run("RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("run under generous cap: %v", err)
	}
	if _, ok, err := cur.Next(); err != nil || !ok {
		t.Fatalf("next = (%v, %v), want a row", ok, err)
	}
	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
