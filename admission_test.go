package gr

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdmissionDisabled(t *testing.T) {
	if a := NewAdmission(0, 0); a != nil {
		t.Fatalf("NewAdmission(0) = %v, want nil (disabled)", a)
	}
	var a *Admission // nil gate
	release, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("nil gate Acquire: %v", err)
	}
	if release == nil {
		t.Fatal("nil gate returned no release")
	}
	release() // must not panic
	if a.InFlight() != 0 {
		t.Errorf("nil gate InFlight = %d, want 0", a.InFlight())
	}
	if a.Shed() != 0 {
		t.Errorf("nil gate Shed = %d, want 0", a.Shed())
	}
}

func TestAdmissionAcquireRelease(t *testing.T) {
	a := NewAdmission(2, 10*time.Millisecond)
	r1, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	r2, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if a.InFlight() != 2 {
		t.Errorf("InFlight = %d, want 2", a.InFlight())
	}
	// The gate is full: a third acquire sheds after the queue wait.
	if _, err := a.Acquire(context.Background()); !errors.Is(err, ErrOverloaded) {
		t.Errorf("full-gate acquire err = %v, want ErrOverloaded", err)
	}
	if a.Shed() != 1 {
		t.Errorf("Shed = %d, want 1 after one shed", a.Shed())
	}
	// Releasing a slot lets the next acquire through.
	r1()
	if a.InFlight() != 1 {
		t.Errorf("InFlight after release = %d, want 1", a.InFlight())
	}
	r3, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	r2()
	r3()
	if a.InFlight() != 0 {
		t.Errorf("InFlight after all released = %d, want 0", a.InFlight())
	}
}

func TestAdmissionContextCancel(t *testing.T) {
	a := NewAdmission(1, time.Hour) // long wait, so only the context ends the wait
	r, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer r()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled acquire err = %v, want context.Canceled", err)
	}
}

func TestAdmissionDefaultWait(t *testing.T) {
	// A zero queue wait uses the default rather than shedding instantly, so a brief
	// contention does not fail a query that a freed slot would have served.
	a := NewAdmission(1, 0)
	r, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		r2, err := a.Acquire(context.Background())
		if err == nil {
			r2()
		}
		done <- err
	}()
	// Free the slot well within the default wait; the queued acquire should succeed.
	time.Sleep(10 * time.Millisecond)
	r()
	if err := <-done; err != nil {
		t.Errorf("queued acquire within default wait err = %v, want nil", err)
	}
}
