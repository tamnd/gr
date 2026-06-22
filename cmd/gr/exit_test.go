package main

import (
	"errors"
	"testing"

	"github.com/tamnd/gr"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{gr.ErrReadOnly, exitReadOnly},
		{gr.ErrParam, exitSyntax},
		{gr.ErrConflict, exitConflict},
		{errors.New("syntax error near RETURN"), exitSyntax},
		{errors.New("unexpected token"), exitSyntax},
		{errors.New("constraint violation"), exitData},
		{errors.New("context deadline exceeded"), exitTimeout},
		{errors.New("context canceled"), exitInterrupt},
		{errors.New("something else"), exitRuntime},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestWorst(t *testing.T) {
	if worst(exitOK, exitSyntax) != exitSyntax {
		t.Error("worst(OK, syntax) should be syntax")
	}
	if worst(exitSyntax, exitOK) != exitSyntax {
		t.Error("worst(syntax, OK) should be syntax")
	}
	if worst(exitData, exitConflict) != exitConflict {
		t.Error("worst(data, conflict) should be the larger code")
	}
	if worst(exitOK, exitOK) != exitOK {
		t.Error("worst(OK, OK) should be OK")
	}
}
