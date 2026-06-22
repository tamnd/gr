package ast

import "github.com/tamnd/gr/value"

// PragmaCommand is a PRAGMA statement: it reads or sets one named configuration knob
// (doc 24 §3). Like a SchemaCommand or an AdminCommand it is a whole statement on its
// own, carried on ast.Query's Pragma field and routed to the configuration subsystem
// rather than the planner. A pragma reads a knob in the query form and changes it in the
// set form; it never reads or writes graph data.
//
// The query form `PRAGMA name` reads the knob's effective value and yields a one-row,
// one-column result (doc 24 §3.3). The set form `PRAGMA name = value` changes the knob
// (doc 24 §3.4): Set is true and Value carries the right-hand side. The call form
// `PRAGMA name(value)` invokes an action pragma parameterized by an argument (doc 24
// §3.7): Call is true and Value carries the argument, which is Null for the bare `name()`
// or `name` invocation of an argumentless action. The TEMP modifier
// (`PRAGMA name = value TEMP`) forces a persistent knob's set to apply to this connection
// only rather than persisting to the file (doc 24 §3.5); it is meaningless on a query
// form and on a session knob, which is session-only by construction.
type PragmaCommand struct {
	Pos
	Name  string
	Set   bool
	Call  bool
	Value value.Value
	Temp  bool
}
