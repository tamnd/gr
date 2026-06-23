package tck

import (
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Deviation is one entry in the deviations registry (doc 23 §2.5).
// It documents an intentional divergence from the TCK expectation.
type Deviation struct {
	ID          string // e.g. "DEV-0001"
	Scenario    string // exact scenario name from the feature file
	Feature     string // feature file (partial match)
	GrBehavior  string
	TCKExpect   string
	Rationale   string
	Introduced  string
	Tracking    string
}

// Deviations is the loaded deviations registry.
type Deviations struct {
	entries []Deviation
	byScen  map[string]*Deviation // keyed by scenario name
}

// LoadDeviations reads the deviations.yaml file from the given file system and
// returns the parsed registry.  The format is the simple YAML-like format
// defined in doc 23 §2.5.  If the file does not exist, an empty registry is
// returned without error.
func LoadDeviations(fsys fs.FS, path string) (*Deviations, error) {
	f, err := fsys.Open(path)
	if err != nil {
		// Missing deviations file is not an error — just no deviations.
		return &Deviations{byScen: map[string]*Deviation{}}, nil
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("tck: reading %s: %w", path, err)
	}
	return parseDeviationsYAML(string(b))
}

// Lookup returns the Deviation for the given scenario name, or nil if none.
func (d *Deviations) Lookup(scenarioName string) *Deviation {
	if d == nil {
		return nil
	}
	return d.byScen[scenarioName]
}

// parseDeviationsYAML parses the simple block-YAML list used in deviations.yaml.
// Each entry begins with "- id: " and the following indented lines are fields.
// This is a bespoke parser, not a full YAML parser.
func parseDeviationsYAML(src string) (*Deviations, error) {
	d := &Deviations{byScen: map[string]*Deviation{}}
	lines := strings.Split(src, "\n")
	var cur *Deviation
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "- id:") {
			if cur != nil {
				d.entries = append(d.entries, *cur)
				d.byScen[cur.Scenario] = &d.entries[len(d.entries)-1]
			}
			cur = &Deviation{ID: strings.TrimSpace(strings.TrimPrefix(line, "- id:"))}
			continue
		}
		if cur == nil {
			continue
		}
		kv := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		switch key {
		case "scenario":
			cur.Scenario = val
		case "feature":
			cur.Feature = val
		case "gr_behavior":
			cur.GrBehavior = val
		case "tck_expectation":
			cur.TCKExpect = val
		case "rationale":
			cur.Rationale = val
		case "introduced":
			cur.Introduced = val
		case "tracking":
			cur.Tracking = val
		}
	}
	if cur != nil {
		d.entries = append(d.entries, *cur)
		d.byScen[cur.Scenario] = &d.entries[len(d.entries)-1]
	}
	return d, nil
}
