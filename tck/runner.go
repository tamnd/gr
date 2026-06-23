package tck

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	gr "github.com/tamnd/gr"
	"github.com/tamnd/gr/vfs"
)

// OutcomeKind classifies a scenario's result.
type OutcomeKind int

const (
	OutcomePass OutcomeKind = iota
	OutcomeFail
	OutcomeSkipUnimplemented // feature not yet built
	OutcomeSkipDeviation     // intentional deviation from TCK expectation
)

// Outcome is the result of running one scenario.
type Outcome struct {
	Kind    OutcomeKind
	Reason  string // for skip: the skip reason; for fail: the failure message
	At      string // step or location where the failure occurred
}

func (o Outcome) String() string {
	switch o.Kind {
	case OutcomePass:
		return "PASS"
	case OutcomeFail:
		return "FAIL: " + o.Reason
	case OutcomeSkipUnimplemented:
		return "SKIP(unimplemented:" + o.Reason + ")"
	case OutcomeSkipDeviation:
		return "SKIP(deviation:" + o.Reason + ")"
	}
	return "UNKNOWN"
}

// RunnerConfig controls how the TCK runner executes scenarios.
type RunnerConfig struct {
	// Deviations is the loaded deviations registry.  Nil means no deviations.
	Deviations *Deviations
	// SkipTags lists @tags that cause a scenario to be skipped as unimplemented.
	// Typical entries: "@skip", "@NegativeTests", "@wip".
	SkipTags []string
}

// Run executes one TCK scenario and returns its outcome.
// It opens a fresh in-memory gr database for each scenario.
func Run(s Scenario, cfg RunnerConfig) Outcome {
	// Check for deviation skip.
	if cfg.Deviations != nil {
		if dev := cfg.Deviations.Lookup(s.Name); dev != nil {
			return Outcome{Kind: OutcomeSkipDeviation, Reason: dev.ID + ": " + dev.Rationale}
		}
	}

	// Check for tag-based skip.
	for _, tag := range s.Tags {
		for _, skip := range cfg.SkipTags {
			if tag == skip {
				return Outcome{Kind: OutcomeSkipUnimplemented, Reason: "tag:" + tag}
			}
		}
	}

	db, err := gr.Open("tck.gr", gr.Options{VFS: vfs.NewMem()})
	if err != nil {
		return Outcome{Kind: OutcomeFail, Reason: "open: " + err.Error()}
	}
	defer func() { _ = db.Close() }()

	return runSteps(db, s.Steps)
}

// RunT runs one TCK scenario under a *testing.T, calling t.Error on failure.
func RunT(t *testing.T, s Scenario, cfg RunnerConfig) Outcome {
	t.Helper()
	out := Run(s, cfg)
	switch out.Kind {
	case OutcomeFail:
		msg := fmt.Sprintf("TCK FAIL [%s] %s\n  %s", s.Feature, s.Name, out.Reason)
		if out.At != "" {
			msg += "\n  at: " + out.At
		}
		t.Error(msg)
	case OutcomeSkipUnimplemented, OutcomeSkipDeviation:
		t.Skip(out.String())
	}
	return out
}

// RunFS runs all .feature files found under root, registering each scenario
// as a sub-test via t.Run.
func RunFS(t *testing.T, root fs.FS, cfg RunnerConfig) *Report {
	t.Helper()
	scenarios, err := ParseFS(root)
	if err != nil {
		t.Fatalf("tck: parse: %v", err)
	}

	rep := &Report{}
	for _, s := range scenarios {
		s := s
		t.Run(s.Feature+"/"+s.Name, func(t *testing.T) {
			t.Helper()
			out := RunT(t, s, cfg)
			rep.record(s, out)
		})
	}
	return rep
}

// runSteps executes the steps of one scenario against db.
func runSteps(db *gr.DB, steps []Step) Outcome {
	var lastResult *gr.Result
	var lastErr error

	for _, step := range steps {
		kind := classifyStep(step)
		switch kind {
		case stepGivenEmptyGraph, stepGivenAnyGraph:
			// Fresh db is already empty; nothing to do.

		case stepGivenExecuted:
			// A "Given having executed" step: run the setup query but don't assert a result.
			query := queryFromStep(step)
			if query == "" {
				return Outcome{Kind: OutcomeSkipUnimplemented,
					Reason: "unrecognized Given step: " + step.Text, At: step.Text}
			}
			_, err := db.Exec(query, nil)
			if err != nil {
				return Outcome{Kind: OutcomeFail,
					Reason: "Given step failed: " + err.Error(), At: step.Text}
			}
			lastErr = nil
			lastResult = nil

		case stepWhenExecuting:
			query := queryFromStep(step)
			if query == "" {
				return Outcome{Kind: OutcomeSkipUnimplemented,
					Reason: "unrecognized When step: " + step.Text, At: step.Text}
			}
			res, err := db.Run(context.Background(), query, nil)
			lastErr = err
			lastResult = res

		case stepThenResultInOrder, stepThenResultAnyOrder:
			ord := AnyOrder
			if kind == stepThenResultInOrder {
				ord = InOrder
			}
			if lastErr != nil {
				return Outcome{Kind: OutcomeFail,
					Reason: "expected result but got error: " + lastErr.Error(), At: step.Text}
			}
			if lastResult == nil {
				return Outcome{Kind: OutcomeFail,
					Reason: "expected result but no query was run", At: step.Text}
			}
			if diff := compareResultTable(lastResult, step.Table, ord); diff != "" {
				return Outcome{Kind: OutcomeFail, Reason: diff, At: step.Text}
			}
			lastResult = nil

		case stepThenNoResult:
			if lastErr != nil {
				return Outcome{Kind: OutcomeFail,
					Reason: "expected no result but got error: " + lastErr.Error(), At: step.Text}
			}
			if lastResult != nil {
				if diff := compareEmptyResult(lastResult); diff != "" {
					return Outcome{Kind: OutcomeFail, Reason: diff, At: step.Text}
				}
				lastResult = nil
			}

		case stepThenError:
			if lastErr == nil {
				return Outcome{Kind: OutcomeFail,
					Reason: "expected an error but query succeeded", At: step.Text}
			}
			expectedCat := parseExpectedCategory(step.Text)
			if expectedCat != CatUnknown {
				gotCat := classifyError(lastErr)
				if gotCat != expectedCat {
					return Outcome{Kind: OutcomeFail,
						Reason: fmt.Sprintf("error category: got %s, want %s: %v",
							gotCat, expectedCat, lastErr), At: step.Text}
				}
			}
			lastErr = nil

		case stepThenSideEffects:
			// Side effects (node/rel counts) — skip for now.
			// A future PR will wire in Summary stats comparison.

		case stepUnknown:
			return Outcome{Kind: OutcomeSkipUnimplemented,
				Reason: "unrecognized step: " + step.Text, At: step.Text}
		}
	}
	return Outcome{Kind: OutcomePass}
}

// stepKind classifies a parsed step into a semantic role.
type stepKind int

const (
	stepUnknown stepKind = iota
	stepGivenEmptyGraph
	stepGivenAnyGraph
	stepGivenExecuted
	stepWhenExecuting
	stepThenResultInOrder
	stepThenResultAnyOrder
	stepThenNoResult
	stepThenError
	stepThenSideEffects
)

// classifyStep maps a Gherkin step to a semantic role.
func classifyStep(s Step) stepKind {
	t := strings.ToLower(strings.TrimSpace(s.Text))
	switch {
	case t == "an empty graph":
		return stepGivenEmptyGraph
	case t == "any graph":
		return stepGivenAnyGraph
	case strings.HasPrefix(t, "having executed:"),
		strings.HasPrefix(t, "having executed query:"):
		return stepGivenExecuted
	case strings.HasPrefix(t, "executing query:"),
		strings.HasPrefix(t, "running query:"),
		t == "executing query" && s.DocString != "":
		return stepWhenExecuting
	case t == "the result should be, in order:":
		return stepThenResultInOrder
	case t == "the result should be:":
		return stepThenResultAnyOrder
	case t == "the result should be empty":
		return stepThenNoResult
	case strings.HasPrefix(t, "a syntaxerror should be raised"),
		strings.HasPrefix(t, "a semanticerror should be raised"),
		strings.HasPrefix(t, "a typeerror should be raised"),
		strings.HasPrefix(t, "a constraintvalidationfailed should be raised"),
		strings.HasPrefix(t, "an arithmeticerror should be raised"),
		strings.HasPrefix(t, "an argumenterror should be raised"),
		strings.HasPrefix(t, "a error should be raised"),
		strings.HasPrefix(t, "an error should be raised"):
		return stepThenError
	case strings.Contains(t, "side effects"):
		return stepThenSideEffects
	case strings.HasPrefix(t, "no side effects"):
		return stepThenSideEffects
	}
	return stepUnknown
}

// queryFromStep extracts the Cypher query from a step, checking DocString first.
func queryFromStep(step Step) string {
	if step.DocString != "" {
		return strings.TrimSpace(step.DocString)
	}
	// Try stripping the prefix from the step text.
	t := step.Text
	for _, prefix := range []string{
		"having executed: ", "having executed query: ",
		"executing query: ", "running query: ",
	} {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimSpace(t[len(prefix):])
		}
	}
	return ""
}
