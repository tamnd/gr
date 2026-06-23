package tck

import (
	"os"
	"testing"
)

// TestConformanceStatement is a release gate (doc 25 §11.4):
// the embedded corpus must be 100% pass before a release ships.
// It is the machine-readable backing for the numbers in CONFORMANCE.md.
func TestConformanceStatement(t *testing.T) {
	root := os.DirFS("testdata/features")
	cfg := defaultCfg(t)
	rep := RunFS(t, root, cfg)

	if rep.Fail > 0 {
		t.Errorf("conformance gate: %d scenario(s) failed; fix before release", rep.Fail)
	}
	if rep.Total == 0 {
		t.Error("conformance gate: zero scenarios found in testdata/features")
	}

	// The embedded corpus must grow only forward: a reduction in scenario count
	// means someone deleted a test that should stay in.
	const minScenarios = 16
	if rep.Total < minScenarios {
		t.Errorf("conformance gate: only %d scenarios (need >= %d); do not remove scenarios", rep.Total, minScenarios)
	}

	t.Logf("conformance: pass=%d skip(unimpl)=%d skip(dev)=%d fail=%d total=%d",
		rep.Pass, rep.SkipUnimpl, rep.SkipDeviation, rep.Fail, rep.Total)
}
