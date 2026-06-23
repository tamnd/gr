package tck

import (
	"errors"

	"github.com/tamnd/gr/bind"
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/parse"
)

// ErrorCategory is the TCK error category name (doc 23 §2.4).
type ErrorCategory string

const (
	CatSyntax               ErrorCategory = "SyntaxError"
	CatSemantic             ErrorCategory = "SemanticError"
	CatType                 ErrorCategory = "TypeError"
	CatArgument             ErrorCategory = "ArgumentError"
	CatEntityNotFound       ErrorCategory = "EntityNotFound"
	CatConstraintValidation ErrorCategory = "ConstraintValidationFailed"
	CatArithmetic           ErrorCategory = "ArithmeticError"
	CatUnknown              ErrorCategory = "Error"
)

// classifyError maps a gr error to a TCK error category.
// This is the taxonomy bridge described in doc 23 §2.4.
func classifyError(err error) ErrorCategory {
	if err == nil {
		return ""
	}

	// Parse errors (syntax).
	var parseErr *parse.Error
	if errors.As(err, &parseErr) {
		return CatSyntax
	}

	// Semantic / name-resolution errors from the binder.
	var bindErr *bind.Error
	if errors.As(err, &bindErr) {
		return CatSemantic
	}

	// Constraint violations.
	var cErr *engine.ConstraintError
	if errors.As(err, &cErr) {
		return CatConstraintValidation
	}

	// Fall through: unknown category.
	return CatUnknown
}

// stepExpectsError reports whether the step text describes a "an error should
// be raised" step in the TCK.
func stepExpectsError(text string) bool {
	return text == "a SyntaxError should be raised at compile time: InvalidArgumentType" ||
		containsAny(text, []string{
			"should be raised at",
			"should fail with",
			"an error should",
			"a SyntaxError",
			"a SemanticError",
			"a TypeError",
		})
}

// parseExpectedCategory extracts the expected TCK error category from a
// "Then" step text like "a SyntaxError should be raised at compile time: ...".
func parseExpectedCategory(text string) ErrorCategory {
	prefixes := []struct {
		prefix string
		cat    ErrorCategory
	}{
		{"a SyntaxError", CatSyntax},
		{"a SemanticError", CatSemantic},
		{"a TypeError", CatType},
		{"a ArgumentError", CatArgument},
		{"an ArgumentError", CatArgument},
		{"a ConstraintValidationFailed", CatConstraintValidation},
		{"an EntityNotFound", CatEntityNotFound},
		{"a ArithmeticError", CatArithmetic},
		{"an ArithmeticError", CatArithmetic},
	}
	for _, p := range prefixes {
		if containsPrefix(text, p.prefix) {
			return p.cat
		}
	}
	return CatUnknown
}

func containsPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
