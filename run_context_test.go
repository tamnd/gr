package gr

import (
	"context"
	"errors"
	"testing"
)

// TestRunContextCanceled confirms the database-level Run honours its context at
// entry: a context already cancelled returns its error without touching the engine.
func TestRunContextCanceled(t *testing.T) {
	db := openMem(t, "rc1.gr")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.Run(ctx, "MATCH (n) RETURN n", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run on a cancelled context err = %v, want context.Canceled", err)
	}
}

// TestRunParamsGoValues confirms Run takes plain Go values as parameters and maps
// them to Cypher values, the Params surface the spec gives the entry points (doc 16
// §9). It writes with a Go string and an int and reads them back.
func TestRunParamsGoValues(t *testing.T) {
	db := openMem(t, "rc2.gr")
	ctx := context.Background()

	if _, err := db.Run(ctx, "CREATE (:Person {name:$n, age:$a})", Params{"n": "Ada", "a": 36}); err != nil {
		t.Fatalf("create with params: %v", err)
	}

	res, err := db.Run(ctx, "MATCH (p:Person {name:$n}) RETURN p.age AS age", Params{"n": "Ada"})
	if err != nil {
		t.Fatalf("match with params: %v", err)
	}
	defer func() { _ = res.Close() }()

	rec, err := Single(res)
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	if age, err := rec.GetInt("age"); err != nil || age != 36 {
		t.Fatalf("age = %d, %v, want 36", age, err)
	}
}

// TestRunParamUnrepresentable confirms a parameter the value model cannot represent
// fails before the statement runs, naming the offending parameter through ErrParam.
func TestRunParamUnrepresentable(t *testing.T) {
	db := openMem(t, "rc3.gr")
	if _, err := db.Run(context.Background(), "RETURN $x", Params{"x": make(chan int)}); !errors.Is(err, ErrParam) {
		t.Fatalf("Run with an unrepresentable param err = %v, want ErrParam", err)
	}
}
