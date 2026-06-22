package main

import (
	"fmt"
	"strings"
)

// config is the resolved invocation: the database path, the open mode, the output
// settings, and the one-shot or script work to run (doc 17 §2). It is built from the
// command line by parseArgs and consumed by run.
type config struct {
	dbPath   string   // database path, ":memory:", or "" for the transient default
	cyphers  []string // --cypher/-c statements, run in order
	file     string   // --file/-f script path
	trailing string   // trailing-argument one-shot statement

	mode      string
	headers   bool
	headerSet bool // whether --headers was given (so the mode default can apply)
	separator string
	null      string
	output    string

	readonly    bool
	create      bool
	noCreate    bool
	batch       bool
	interactive bool
	timer       bool
	echo        bool
	bail        bool
	quiet       bool

	showVersion bool
	showHelp    bool
}

// defaultConfig is the configuration before any flag is applied. The mode is left
// empty so run can pick table for a TTY and csv for a pipe (doc 17 §5.1, §9.1).
func defaultConfig() config {
	return config{create: true, separator: ",", null: ""}
}

// parseArgs parses the command line into a config (doc 17 §2.2). It accepts the
// --flag=value and --flag value spellings and the short forms, treats the first
// non-flag token as the database path and a second as the trailing one-shot
// statement, and reports an unknown flag or a missing argument as an error (exit 2).
func parseArgs(args []string) (config, error) {
	cfg := defaultConfig()
	var positional []string
	i := 0
	next := func(flag string) (string, error) {
		i++
		if i >= len(args) {
			return "", fmt.Errorf("flag %s needs an argument", flag)
		}
		return args[i], nil
	}
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, inlineVal, hasInline := splitFlag(arg)
		val := func(flag string) (string, error) {
			if hasInline {
				return inlineVal, nil
			}
			return next(flag)
		}
		switch name {
		case "--help", "-h":
			cfg.showHelp = true
		case "--version", "-V":
			cfg.showVersion = true
		case "--readonly", "-r":
			cfg.readonly = true
		case "--create":
			cfg.create = true
		case "--no-create":
			cfg.noCreate = true
			cfg.create = false
		case "--batch":
			cfg.batch = true
		case "--interactive", "-i":
			cfg.interactive = true
		case "--echo":
			cfg.echo = true
		case "--bail":
			cfg.bail = true
		case "--quiet", "-q":
			cfg.quiet = true
		case "--mode":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			if !validMode(v) {
				return cfg, fmt.Errorf("unknown output mode %q", v)
			}
			cfg.mode = v
		case "--header", "--headers":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			on, err := parseOnOff(v)
			if err != nil {
				return cfg, err
			}
			cfg.headers, cfg.headerSet = on, true
		case "--separator":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			cfg.separator = v
		case "--nullvalue":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			cfg.null = v
		case "--output", "-o":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			cfg.output = v
		case "--timer":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			on, err := parseOnOff(v)
			if err != nil {
				return cfg, err
			}
			cfg.timer = on
		case "--cypher", "-c":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			cfg.cyphers = append(cfg.cyphers, v)
		case "--file", "-f":
			v, err := val(name)
			if err != nil {
				return cfg, err
			}
			cfg.file = v
		default:
			return cfg, fmt.Errorf("unknown flag %q%s", name, suggestFlag(name))
		}
	}
	if len(positional) > 0 {
		cfg.dbPath = positional[0]
	}
	if len(positional) > 1 {
		cfg.trailing = positional[1]
	}
	if len(positional) > 2 {
		return cfg, fmt.Errorf("too many arguments: %v", positional[2:])
	}
	return cfg, nil
}

// splitFlag splits --flag=value into its name and value; for a flag with no = it
// returns hasInline=false so the parser reads the next argument when one is needed.
func splitFlag(arg string) (name, value string, hasInline bool) {
	if eq := strings.IndexByte(arg, '='); eq >= 0 {
		return arg[:eq], arg[eq+1:], true
	}
	return arg, "", false
}

// parseOnOff reads an on/off flag value (doc 17 §2.2).
func parseOnOff(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("expected on or off, got %q", v)
	}
}

// suggestFlag offers a near-miss suggestion for an unknown flag (doc 17 §2.2).
func suggestFlag(name string) string {
	known := []string{
		"--help", "--version", "--readonly", "--create", "--no-create",
		"--mode", "--headers", "--separator", "--nullvalue", "--output",
		"--timer", "--echo", "--bail", "--quiet", "--batch", "--interactive",
		"--cypher", "--file",
	}
	for _, k := range known {
		if near(name, k) {
			return fmt.Sprintf(" (did you mean %s?)", k)
		}
	}
	return ""
}

// near reports whether two flag names are within an edit distance of one, the
// threshold for a "did you mean" suggestion.
func near(a, b string) bool {
	if a == b {
		return true
	}
	return editDistance(a, b) == 1
}

// editDistance is the Levenshtein distance between two short strings.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
