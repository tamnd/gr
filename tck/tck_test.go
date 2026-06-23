package tck

import (
	"os"
	"testing"
	"testing/fstest"
)

// TestTCKEmbedded runs the small embedded TCK scenario corpus from
// testdata/features/.  These scenarios are maintained by this repository
// and always run in CI, independent of the openCypher TCK download.
func TestTCKEmbedded(t *testing.T) {
	root := os.DirFS("testdata/features")
	cfg := defaultCfg(t)
	rep := RunFS(t, root, cfg)
	if !rep.IsClean() {
		t.Errorf("TCK embedded: %d failure(s)", rep.Fail)
	}
	t.Logf("TCK embedded: pass=%d skip(unimpl)=%d skip(dev)=%d fail=%d total=%d",
		rep.Pass, rep.SkipUnimpl, rep.SkipDeviation, rep.Fail, rep.Total)
}

// TestTCKGherkinParse verifies the Gherkin parser on all test feature files.
func TestTCKGherkinParse(t *testing.T) {
	root := os.DirFS("testdata/features")
	scenarios, err := ParseFS(root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(scenarios) == 0 {
		t.Error("parsed zero scenarios from testdata/features")
	}
	t.Logf("parsed %d scenarios", len(scenarios))
	for _, s := range scenarios {
		if s.Name == "" {
			t.Errorf("scenario at %s has empty name", s.Location)
		}
		if len(s.Steps) == 0 {
			t.Errorf("scenario %q at %s has no steps", s.Name, s.Location)
		}
	}
}

// TestTCKRunOneScenario exercises RunT on a hand-built Scenario, verifying
// that the runner calls t.Error on failure and t.Skip on skip.
func TestTCKRunOneScenario(t *testing.T) {
	s := Scenario{
		Feature:  "Test",
		Name:     "Return a constant",
		Location: "inline:1",
		Steps: []Step{
			{Keyword: "Given", Text: "an empty graph"},
			{Keyword: "When", Text: "executing query:", DocString: "RETURN 42 AS n"},
			{Keyword: "Then", Text: "the result should be:", Table: [][]string{
				{"n"},
				{"42"},
			}},
		},
	}
	out := Run(s, RunnerConfig{})
	if out.Kind != OutcomePass {
		t.Errorf("expected pass, got %s", out)
	}
}

// TestTCKRunExpectsError verifies that the runner correctly classifies a
// scenario that expects a SyntaxError.
func TestTCKRunExpectsError(t *testing.T) {
	s := Scenario{
		Feature:  "Errors",
		Name:     "Syntax error",
		Location: "inline:1",
		Steps: []Step{
			{Keyword: "Given", Text: "an empty graph"},
			{Keyword: "When", Text: "executing query:", DocString: "MATCH (n"},
			{Keyword: "Then", Text: "a SyntaxError should be raised at compile time: InvalidSyntax"},
		},
	}
	out := Run(s, RunnerConfig{})
	if out.Kind != OutcomePass {
		t.Errorf("expected pass on expected-error scenario, got %s", out)
	}
}

// TestTCKRunFail verifies that the runner returns OutcomeFail when a result is wrong.
func TestTCKRunFail(t *testing.T) {
	s := Scenario{
		Feature:  "Test",
		Name:     "Wrong result",
		Location: "inline:1",
		Steps: []Step{
			{Keyword: "Given", Text: "an empty graph"},
			{Keyword: "When", Text: "executing query:", DocString: "RETURN 1 AS n"},
			{Keyword: "Then", Text: "the result should be:", Table: [][]string{
				{"n"},
				{"999"}, // wrong expected value
			}},
		},
	}
	out := Run(s, RunnerConfig{})
	if out.Kind != OutcomeFail {
		t.Errorf("expected fail, got %s", out)
	}
}

// TestTCKRunSkipByTag verifies that a scenario tagged with a skip tag is skipped.
func TestTCKRunSkipByTag(t *testing.T) {
	s := Scenario{
		Feature:  "Test",
		Name:     "Skip me",
		Tags:     []string{"@skip"},
		Location: "inline:1",
		Steps: []Step{
			{Keyword: "Given", Text: "an empty graph"},
			{Keyword: "When", Text: "executing query:", DocString: "RETURN 1 AS n"},
			{Keyword: "Then", Text: "the result should be empty"},
		},
	}
	cfg := RunnerConfig{SkipTags: []string{"@skip"}}
	out := Run(s, cfg)
	if out.Kind != OutcomeSkipUnimplemented {
		t.Errorf("expected skip, got %s", out)
	}
}

// TestTCKRunDeviation verifies that a scenario in the deviations registry is skipped.
func TestTCKRunDeviation(t *testing.T) {
	devYAML := `- id: DEV-TEST
  scenario: My Scenario
  feature: test
  gr_behavior: does something different
  tck_expectation: something else
  rationale: test only
  introduced: v0.0.0
  tracking: none
`
	devs, err := parseDeviationsYAML(devYAML)
	if err != nil {
		t.Fatalf("parseDeviationsYAML: %v", err)
	}
	s := Scenario{
		Feature:  "Test",
		Name:     "My Scenario",
		Location: "inline:1",
		Steps: []Step{
			{Keyword: "Given", Text: "an empty graph"},
		},
	}
	cfg := RunnerConfig{Deviations: devs}
	out := Run(s, cfg)
	if out.Kind != OutcomeSkipDeviation {
		t.Errorf("expected deviation skip, got %s", out)
	}
}

// TestTCKGherkinParseInMemory parses an in-memory .feature file and checks
// that the scenarios, steps, tables, and doc-strings are parsed correctly.
func TestTCKGherkinParseInMemory(t *testing.T) {
	src := `Feature: My Feature

  @wip
  Scenario: With docstring
    Given an empty graph
    When executing query:
      """
      MATCH (n) RETURN n
      """
    Then the result should be empty

  Scenario: With table
    Given an empty graph
    When executing query:
      """
      RETURN 1 AS n
      """
    Then the result should be:
      | n |
      | 1 |
`
	fsys := fstest.MapFS{
		"test.feature": &fstest.MapFile{Data: []byte(src)},
	}
	scenarios, err := ParseFS(fsys)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("got %d scenarios, want 2", len(scenarios))
	}

	s0 := scenarios[0]
	if s0.Name != "With docstring" {
		t.Errorf("scenario 0 name: got %q, want %q", s0.Name, "With docstring")
	}
	if !contains(s0.Tags, "@wip") {
		t.Errorf("scenario 0 tags: %v should contain @wip", s0.Tags)
	}
	// When step should have a doc-string.
	var whenStep *Step
	for i := range s0.Steps {
		if s0.Steps[i].Keyword == "When" {
			whenStep = &s0.Steps[i]
			break
		}
	}
	if whenStep == nil || whenStep.DocString == "" {
		t.Error("When step should have a DocString")
	}

	s1 := scenarios[1]
	if s1.Name != "With table" {
		t.Errorf("scenario 1 name: got %q, want %q", s1.Name, "With table")
	}
	// Then step should have a table.
	var thenStep *Step
	for i := range s1.Steps {
		if s1.Steps[i].Keyword == "Then" {
			thenStep = &s1.Steps[i]
			break
		}
	}
	if thenStep == nil || len(thenStep.Table) == 0 {
		t.Error("Then step should have a Table")
	}
}

// defaultCfg returns a RunnerConfig with the TCK skip tags used in CI.
func defaultCfg(t *testing.T) RunnerConfig {
	t.Helper()
	devs, err := LoadDeviations(os.DirFS("."), "deviations.yaml")
	if err != nil {
		t.Logf("no deviations.yaml: %v", err)
		devs = &Deviations{byScen: map[string]*Deviation{}}
	}
	return RunnerConfig{
		Deviations: devs,
		SkipTags: []string{
			"@skip",
			"@wip",
			"@NegativeTests",
			"@ExpectError",
			"@ignore",
		},
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
