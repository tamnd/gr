package httpd

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tamnd/gr"
)

// apiError is one error in the response error array (doc 18 §9.3). The code is the
// Neo4j status code Bolt uses too, so error handling is consistent across the two
// transports (doc 18 §12).
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// mapError maps a library error to an HTTP status and a Neo4j status code (doc 18
// §12.5). It keys off the library's sentinel errors where they exist and falls back
// to a string match for the parser and runtime errors that are not yet exported
// sentinels. A client error (the request was bad) is a 4xx; a database error (the
// request was fine but execution failed) is a 5xx.
func mapError(err error) (int, apiError) {
	switch {
	case errors.Is(err, gr.ErrReadOnly):
		return http.StatusForbidden, apiError{
			Code:    "Neo.ClientError.Statement.AccessMode",
			Message: err.Error(),
		}
	case errors.Is(err, gr.ErrParam):
		return http.StatusBadRequest, apiError{
			Code:    "Neo.ClientError.Statement.ParameterMissing",
			Message: err.Error(),
		}
	case errors.Is(err, gr.ErrConflict):
		return http.StatusConflict, apiError{
			Code:    "Neo.TransientError.Transaction.Terminated",
			Message: err.Error(),
		}
	case errors.Is(err, gr.ErrOverloaded):
		// The in-flight gate is full (doc 18 §8.8). 503 with a transient code tells a
		// driver to back off and retry rather than treat the request as failed.
		return http.StatusServiceUnavailable, apiError{
			Code:    "Neo.TransientError.General.TransientError",
			Message: err.Error(),
		}
	case errors.Is(err, gr.ErrClosed):
		return http.StatusServiceUnavailable, apiError{
			Code:    "Neo.DatabaseError.General.UnknownError",
			Message: err.Error(),
		}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "parse") || strings.Contains(msg, "syntax") ||
		strings.Contains(msg, "unexpected") || strings.Contains(msg, "expected"):
		return http.StatusBadRequest, apiError{
			Code:    "Neo.ClientError.Statement.SyntaxError",
			Message: err.Error(),
		}
	case strings.Contains(msg, "constraint") || strings.Contains(msg, "integrity"):
		return http.StatusConflict, apiError{
			Code:    "Neo.ClientError.Schema.ConstraintValidationFailed",
			Message: err.Error(),
		}
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return http.StatusGatewayTimeout, apiError{
			Code:    "Neo.ClientError.Transaction.TransactionTimedOut",
			Message: err.Error(),
		}
	default:
		return http.StatusInternalServerError, apiError{
			Code:    "Neo.DatabaseError.Statement.ExecutionFailed",
			Message: err.Error(),
		}
	}
}
