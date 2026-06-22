package main

import (
	"errors"
	"strings"

	"github.com/tamnd/gr"
)

// Exit codes (doc 17 §10.1). The set is stable so a script can branch on the kind of
// failure; new categories take new codes rather than reusing one.
const (
	exitOK         = 0
	exitGeneric    = 1
	exitUsage      = 2
	exitOpen       = 3
	exitFormat     = 4
	exitSyntax     = 5
	exitData       = 6
	exitTimeout    = 7
	exitRuntime    = 8
	exitConflict   = 9
	exitIO         = 10
	exitReadOnly   = 11
	exitPermission = 12
	exitInterrupt  = 130
)

// classify maps an error from the library to a CLI exit code (doc 17 §10.2). It keys
// off the library's sentinel errors where they exist and falls back to a string match
// for the parser and runtime errors that are not yet exported sentinels, then to the
// generic runtime code.
func classify(err error) int {
	if err == nil {
		return exitOK
	}
	switch {
	case errors.Is(err, gr.ErrReadOnly):
		return exitReadOnly
	case errors.Is(err, gr.ErrParam):
		return exitSyntax
	case errors.Is(err, gr.ErrConflict):
		return exitConflict
	case errors.Is(err, gr.ErrClosed):
		return exitGeneric
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "parse") || strings.Contains(msg, "syntax") ||
		strings.Contains(msg, "unexpected") || strings.Contains(msg, "expected"):
		return exitSyntax
	case strings.Contains(msg, "constraint") || strings.Contains(msg, "integrity"):
		return exitData
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return exitTimeout
	case strings.Contains(msg, "context canceled"):
		return exitInterrupt
	default:
		return exitRuntime
	}
}

// worst returns the more severe of two exit codes (doc 17 §10.2). A non-interactive
// multi-statement run reports the most severe code among its statements, so a single
// failure dominates a string of successes.
func worst(a, b int) int {
	if a == exitOK {
		return b
	}
	if b == exitOK {
		return a
	}
	if b > a {
		return b
	}
	return a
}
