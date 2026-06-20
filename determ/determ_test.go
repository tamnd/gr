package determ

import "testing"

func TestFakeClockAdvances(t *testing.T) {
	c := NewFakeClock(1000)
	if c.Now() != 1000 {
		t.Fatalf("Now=%d", c.Now())
	}
	c.Advance(500)
	if c.Now() != 1500 {
		t.Fatalf("after advance Now=%d", c.Now())
	}
}

func TestPRNGDeterministic(t *testing.T) {
	a := NewPRNG(42)
	b := NewPRNG(42)
	for i := 0; i < 1000; i++ {
		if a.Uint64() != b.Uint64() {
			t.Fatalf("PRNG diverged at %d for the same seed", i)
		}
	}
}

func TestPRNGDifferentSeeds(t *testing.T) {
	a := NewPRNG(1)
	b := NewPRNG(2)
	same := true
	for i := 0; i < 10; i++ {
		if a.Uint64() != b.Uint64() {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seeds produced identical streams")
	}
}

func TestPRNGIntnRange(t *testing.T) {
	p := NewPRNG(7)
	for i := 0; i < 1000; i++ {
		n := p.Intn(10)
		if n < 0 || n >= 10 {
			t.Fatalf("Intn(10)=%d out of range", n)
		}
	}
}
