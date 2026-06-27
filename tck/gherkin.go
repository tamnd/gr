// Package tck implements the openCypher TCK runner for gr (doc 23 §2).
// It parses Gherkin .feature files, executes each scenario against a fresh
// in-memory gr database, and classifies the outcome as pass/fail/skip.
package tck

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
)

// Scenario is one TCK test case, parsed from a Gherkin .feature file.
type Scenario struct {
	Feature  string   // feature title
	Name     string   // scenario title
	Tags     []string // @-prefixed tags from the scenario or feature block
	Steps    []Step   // Given / And / When / Then steps
	Location string   // "file:line" for diagnostics
}

// Step is one Gherkin step in a scenario.
type Step struct {
	Keyword string // "Given", "When", "Then", "And", "But"
	Text    string // the prose text of the step
	// DocString holds the content of a triple-quote block under the step, used
	// for multi-line queries in "executing query" steps.
	DocString string
	// Table holds a Gherkin data table under the step, used for "the result
	// should be" steps and "having executed" steps with parameters.
	Table [][]string // [row][col], first row is header
}

// ParseFS reads all .feature files under root in the given file system and
// returns the collected scenarios.  It recurses into sub-directories.
func ParseFS(root fs.FS) ([]Scenario, error) {
	var all []Scenario
	err := fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".feature") {
			return nil
		}
		f, err := root.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		ss, err := ParseReader(path, f)
		if err != nil {
			return fmt.Errorf("tck: %s: %w", path, err)
		}
		all = append(all, ss...)
		return nil
	})
	return all, err
}

// ParseFile parses one .feature file and returns its scenarios.
func ParseFile(path string, r io.Reader) ([]Scenario, error) {
	return ParseReader(filepath.Base(path), r)
}

// ParseReader parses a Gherkin feature file from r, using file as the display
// name for Location fields.
func ParseReader(file string, r io.Reader) ([]Scenario, error) {
	p := &ghParser{file: file, sc: bufio.NewScanner(r)}
	return p.parse()
}

// ghParser is a minimal Gherkin parser tuned to the openCypher TCK dialect.
// It supports: Feature:, Background:, Scenario:, Scenario Outline:, tags (@),
// Given/When/Then/And/But steps, triple-quote doc-strings, and | table | rows.
// It does not support Examples: tables in Scenario Outlines (the TCK does not
// use them in a way we need to handle dynamically; those features are skipped).
type ghParser struct {
	file    string
	sc      *bufio.Scanner
	line    int
	peeked  string
	hasPeek bool

	featureName string
	featureTags []string
}

func (p *ghParser) nextLine() (string, bool) {
	if p.hasPeek {
		p.hasPeek = false
		return p.peeked, true
	}
	for p.sc.Scan() {
		p.line++
		raw := p.sc.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue // skip blank lines and comments
		}
		return trimmed, true
	}
	return "", false
}

func (p *ghParser) pushBack(line string) {
	p.peeked = line
	p.hasPeek = true
}

func (p *ghParser) parse() ([]Scenario, error) {
	var scenarios []Scenario
	var featureTags []string

	for {
		line, ok := p.nextLine()
		if !ok {
			break
		}
		switch {
		case strings.HasPrefix(line, "@"):
			featureTags = append(featureTags, parseTags(line)...)

		case strings.HasPrefix(line, "Feature:"):
			p.featureName = strings.TrimSpace(strings.TrimPrefix(line, "Feature:"))
			p.featureTags = featureTags
			featureTags = nil

		case strings.HasPrefix(line, "Background:"):
			// skip the background block body (we handle "an empty graph" inline)
			p.skipBlock()

		case strings.HasPrefix(line, "Scenario Outline:"), strings.HasPrefix(line, "Scenario:"):
			var scenTags []string
			scenTags = append(scenTags, featureTags...)
			scenTags = append(scenTags, p.featureTags...)
			name := ""
			if strings.HasPrefix(line, "Scenario Outline:") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "Scenario Outline:"))
			} else {
				name = strings.TrimSpace(strings.TrimPrefix(line, "Scenario:"))
			}
			loc := fmt.Sprintf("%s:%d", p.file, p.line)
			steps, tags, err := p.parseScenarioBody()
			if err != nil {
				return nil, err
			}
			scenTags = append(scenTags, tags...)
			scenarios = append(scenarios, Scenario{
				Feature:  p.featureName,
				Name:     name,
				Tags:     dedup(scenTags),
				Steps:    steps,
				Location: loc,
			})

		default:
			// Unknown block (e.g. Rule:, Examples: outside a scenario) — skip line.
		}
	}
	return scenarios, p.sc.Err()
}

// parseScenarioBody reads steps until the next Scenario/Feature/EOF/Examples boundary.
func (p *ghParser) parseScenarioBody() (steps []Step, tags []string, err error) {
	for {
		line, ok := p.nextLine()
		if !ok {
			break
		}
		switch {
		case strings.HasPrefix(line, "@"):
			// Tags before the next scenario — push back and stop.
			p.pushBack(line)
			return
		case strings.HasPrefix(line, "Scenario"), strings.HasPrefix(line, "Feature"),
			strings.HasPrefix(line, "Background"), strings.HasPrefix(line, "Rule"):
			p.pushBack(line)
			return
		case strings.HasPrefix(line, "Examples:"):
			// Outline examples table — skip it; the outline itself is a skip target.
			p.skipBlock()
			return
		default:
			kw, rest := splitKeyword(line)
			if kw == "" {
				// Not a step line; might be a continuation or unknown syntax.
				continue
			}
			step := Step{Keyword: kw, Text: rest}
			// Check for a doc-string or table immediately following.
			step, err = p.readStepBody(step)
			if err != nil {
				return nil, nil, err
			}
			steps = append(steps, step)
		}
	}
	return
}

// readStepBody reads an optional doc-string or table that may follow a step.
func (p *ghParser) readStepBody(step Step) (Step, error) {
	line, ok := p.nextLine()
	if !ok {
		return step, nil
	}
	switch {
	case strings.HasPrefix(line, `"""`):
		// Triple-quote doc-string.
		var sb strings.Builder
		for {
			l, ok2 := p.nextLine()
			if !ok2 {
				return step, fmt.Errorf("tck: unclosed doc-string in %s", p.file)
			}
			if strings.HasPrefix(l, `"""`) {
				break
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(l)
		}
		step.DocString = sb.String()

	case strings.HasPrefix(line, "|"):
		// Data table.
		var rows [][]string
		rows = append(rows, parseTableRow(line))
		for {
			l, ok2 := p.nextLine()
			if !ok2 {
				break
			}
			if !strings.HasPrefix(l, "|") {
				p.pushBack(l)
				break
			}
			rows = append(rows, parseTableRow(l))
		}
		step.Table = rows

	default:
		p.pushBack(line)
	}
	return step, nil
}

// skipBlock reads and discards lines until a line that looks like a new
// top-level keyword or a scenario/feature boundary.
func (p *ghParser) skipBlock() {
	for {
		line, ok := p.nextLine()
		if !ok {
			return
		}
		kw, _ := splitKeyword(line)
		if kw != "" && (kw == "Given" || kw == "When" || kw == "Then" || kw == "And" || kw == "But") {
			// Still inside the block.
			continue
		}
		if strings.HasPrefix(line, "Scenario") || strings.HasPrefix(line, "Feature") ||
			strings.HasPrefix(line, "Background") || strings.HasPrefix(line, "@") ||
			strings.HasPrefix(line, "Examples") || strings.HasPrefix(line, "Rule") {
			p.pushBack(line)
			return
		}
		// Anything else (step keywords, tables, doc-strings, blank lines) is
		// part of the block and gets discarded by looping again.
	}
}

// parseTags parses a line like "@wip @skip" into ["@wip", "@skip"].
func parseTags(line string) []string {
	var out []string
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "@") {
			out = append(out, f)
		}
	}
	return out
}

// splitKeyword splits a Gherkin step line into the keyword and the rest.
// Returns ("", "") if the line is not a recognized step keyword.
func splitKeyword(line string) (kw, rest string) {
	for _, k := range []string{"Given ", "When ", "Then ", "And ", "But ", "* "} {
		if strings.HasPrefix(line, k) {
			// Normalize "* " to "And".
			kw := strings.TrimSuffix(k, " ")
			if kw == "*" {
				kw = "And"
			}
			return kw, strings.TrimSpace(line[len(k):])
		}
	}
	return "", ""
}

// parseTableRow splits a Gherkin table row like "| a | b | c |" into cells.
func parseTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

// dedup removes duplicate strings, preserving order.
func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := ss[:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
