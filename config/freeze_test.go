package config_test

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tamnd/gr/config"
)

func Registry() []config.Knob { return config.Registry() }

// TestFreezeGuard is the 1.0 config-surface freeze gate (doc 25 §11.4).
// It computes a deterministic digest of every (canonical-name, tier) pair in
// the registry and fails if the digest does not match the committed value.
//
// Adding a knob is additive and allowed: the new row extends the digest and the
// committed value here must be updated deliberately, with a comment explaining
// the addition.
//
// Renaming a canonical name or changing a knob's tier is a breaking change
// under the 1.0 contract and requires a major-version bump (doc 16 §24.1).
// The test makes that cost visible: the digest changes, the test fails, and
// no release can go out without a reviewer noticing and either updating the
// committed value with a documented reason or reverting the rename/retier.
func TestFreezeGuard(t *testing.T) {
	// committedDigest is the SHA-256 of the sorted "name:tier" lines as of the
	// 1.0 freeze. Update this exactly once when adding a new knob; never update
	// it for a rename or tier change without a major-version bump.
	const committedDigest = "7e121971d488228a684e42433a6ceec5df9fca1e6b99933ba1ae8f3827c27876"

	knobs := Registry()
	lines := make([]string, len(knobs))
	for i, k := range knobs {
		lines[i] = fmt.Sprintf("%s:%s", k.Name, k.Tier.String())
	}
	sort.Strings(lines)
	joined := strings.Join(lines, "\n")

	sum := sha256.Sum256([]byte(joined))
	got := fmt.Sprintf("%x", sum)
	if got != committedDigest {
		t.Logf("registry lines:\n%s", joined)
		t.Errorf("config freeze digest mismatch\ngot:  %s\nwant: %s\n\n"+
			"If you ADDED a knob (allowed, additive), update committedDigest to %s.\n"+
			"If you RENAMED or RETIERED a knob, revert — that is a breaking change\n"+
			"under the 1.0 contract and requires a major-version bump (doc 16 §24.1).",
			got, committedDigest, got)
	}
}

// TestRegistryNoDuplicateNames ensures the registry has no two knobs with the
// same canonical name; a duplicate would mean two conflicting definitions.
func TestRegistryNoDuplicateNames(t *testing.T) {
	seen := make(map[string]bool)
	for _, k := range Registry() {
		if seen[k.Name] {
			t.Errorf("duplicate knob name %q in registry", k.Name)
		}
		seen[k.Name] = true
	}
}

// TestRegistryPRAGMAMatchesName verifies that every knob's PRAGMA field equals
// its canonical Name (doc 24 §4.1: "the canonical name verbatim, case-insensitive").
func TestRegistryPRAGMAMatchesName(t *testing.T) {
	for _, k := range Registry() {
		if k.PRAGMA == "" {
			continue // read-only introspection knobs may have no PRAGMA form
		}
		if k.PRAGMA != k.Name {
			t.Errorf("knob %q: PRAGMA %q != canonical name", k.Name, k.PRAGMA)
		}
	}
}

// TestRegistryRequiredFields checks that every knob has a non-empty Name,
// KnobType, and Description.
func TestRegistryRequiredFields(t *testing.T) {
	for _, k := range Registry() {
		if k.Name == "" {
			t.Error("knob with empty Name in registry")
		}
		if k.KnobType == "" {
			t.Errorf("knob %q: empty KnobType", k.Name)
		}
		if k.Description == "" {
			t.Errorf("knob %q: empty Description", k.Name)
		}
	}
}
