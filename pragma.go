package gr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tamnd/gr/ast"
	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/value"
)

// The PRAGMA mechanism (doc 24 §3): the in-band configuration channel. A PRAGMA reads a
// knob's effective value in the query form (PRAGMA name) or changes it in the set form
// (PRAGMA name = value), the same idiom from the library, the CLI, and the server (doc 24
// §3.1). The knob set this release exposes is the session-runtime knobs that take effect
// live per statement, the create-time and read-only computed knobs that report engine
// state, and the pragma_list discovery surface. Persistent-runtime knobs that write
// through to the file (synchronous, the checkpoint thresholds) arrive with the catalog
// metadata persistence in a later slice; this slice deliberately exposes no persistent
// knob rather than half-persist one.

// ErrPragmaCommand is returned by Query, Exec, a transaction's Run/Exec, and compile when
// the statement is a PRAGMA. A PRAGMA reads or sets configuration outside the operator
// pipeline and auto-commits its own change, so it runs through the database-level Run, not
// a cache-backed read, a graph write, or a managed transaction (doc 24 §3).
var ErrPragmaCommand = errors.New("gr: PRAGMA statements run through Run, not Query, Exec, or a transaction")

// ErrExplainPragma is returned by EXPLAIN of a PRAGMA. A PRAGMA changes or reports
// configuration outside the operator pipeline, so it has no plan to render (doc 24 §3).
var ErrExplainPragma = errors.New("gr: cannot EXPLAIN a PRAGMA")

// ErrProfilePragma is returned by PROFILE of a PRAGMA. A PRAGMA runs outside the
// operator pipeline, so it has no execution to instrument (doc 24 §3, doc 20 §9).
var ErrProfilePragma = errors.New("gr: cannot PROFILE a PRAGMA")

// ErrUnknownPragma is returned when a PRAGMA names a knob the engine does not know (doc 24
// §24.4). An unknown name is a loud error rather than a silent no-op, so a typo
// (PRAGMA synchronus) is caught rather than swallowed (doc 24 §3.8).
var ErrUnknownPragma = errors.New("gr: unknown pragma")

// ErrNotSettable is returned by a set form against a read-only computed pragma (doc 24
// §24.4): page_count and the like report the engine's state and have no value to set.
var ErrNotSettable = errors.New("gr: pragma is read-only and cannot be set")

// ErrConfigConflict is returned by a set form against a create-time knob on an existing
// database (doc 24 §24.3): page_size and the other create-time settings are baked into the
// file's geometry and cannot change without a dump and reload (doc 24 §5).
var ErrConfigConflict = errors.New("gr: setting is fixed at create time and cannot be changed on an existing database")

// ErrConfigType is returned when a PRAGMA value cannot coerce to the knob's type (doc 24
// §24.4): a string for an integer knob, a non-boolean word for a boolean knob.
var ErrConfigType = errors.New("gr: pragma value has the wrong type")

// ErrConfigRange is returned when a PRAGMA value coerces to the right type but falls
// outside the knob's valid range (doc 24 §24.4): a negative byte budget, for example.
var ErrConfigRange = errors.New("gr: pragma value is out of range")

// pragmaTier classifies how a knob may be set (doc 24 §2.5): a session knob lives in the
// open connection's memory and a set changes it live; a create-time knob is baked into the
// file and a set on an existing file conflicts; a read-only computed pragma reports engine
// state and has no set form.
type pragmaTier uint8

const (
	tierSession pragmaTier = iota
	tierCreate
	tierReadOnly
	tierAction
)

// String names the tier for the pragma_list discovery surface (doc 24 §23.2).
func (t pragmaTier) String() string {
	switch t {
	case tierSession:
		return "session"
	case tierCreate:
		return "create-time"
	case tierAction:
		return "action"
	default:
		return "read-only"
	}
}

// pragmaDesc describes one knob: its tier, its value type (for discovery and error
// messages), a getter that reads the live effective value, and a setter that applies a new
// value live. set is nil for a knob with no set form (a create-time or read-only knob); a
// set against it reports ErrConfigConflict for a create-time knob and ErrNotSettable for a
// read-only one (doc 24 §24.4). act is non-nil for an action pragma (doc 24 §3.7): it runs
// the action with the call-form argument (or a null argument for the bare invocation) and
// returns the action's result. An action pragma has no get or set; a value pragma has no
// act.
type pragmaDesc struct {
	tier pragmaTier
	typ  string
	get  func(*DB) value.Value
	set  func(*DB, value.Value) error
	act  func(*DB, value.Value) (*Result, error)
}

// pragmas is the knob registry (doc 24 §3.8): one canonical lower-snake-case name to its
// descriptor. The name is the catalogue's column-1 canonical name (doc 24 §4.1), the same
// spelling the library option, CLI flag, and server-config key derive from.
var pragmas = map[string]pragmaDesc{
	"lazy_properties": {
		tier: tierSession, typ: "bool",
		get: func(db *DB) value.Value { return value.Bool(db.lazyDefault()) },
		set: func(db *DB, v value.Value) error {
			b, err := pragmaBool("lazy_properties", v)
			if err != nil {
				return err
			}
			db.cfgMu.Lock()
			db.lazyProps = b
			db.cfgMu.Unlock()
			return nil
		},
	},
	"mem_budget": {
		tier: tierSession, typ: "int",
		get: func(db *DB) value.Value { return value.Int(db.memBudgetVal()) },
		set: func(db *DB, v value.Value) error {
			n, err := pragmaInt("mem_budget", v)
			if err != nil {
				return err
			}
			if n < 0 {
				return fmt.Errorf("%w: mem_budget must be >= 0, got %d", ErrConfigRange, n)
			}
			db.cfgMu.Lock()
			db.memBudget = n
			db.cfgMu.Unlock()
			return nil
		},
	},
	"max_retries": {
		tier: tierSession, typ: "int",
		get: func(db *DB) value.Value { return value.Int(int64(db.retries())) },
		set: func(db *DB, v value.Value) error {
			n, err := pragmaInt("max_retries", v)
			if err != nil {
				return err
			}
			if n < 0 {
				return fmt.Errorf("%w: max_retries must be >= 0, got %d", ErrConfigRange, n)
			}
			db.cfgMu.Lock()
			db.maxRetries = int(n)
			db.cfgMu.Unlock()
			return nil
		},
	},
	"replan_drift_factor": {
		tier: tierSession, typ: "float",
		get: func(db *DB) value.Value { return value.Float(db.drift()) },
		set: func(db *DB, v value.Value) error {
			f, err := pragmaFloat("replan_drift_factor", v)
			if err != nil {
				return err
			}
			if f < 0 {
				return fmt.Errorf("%w: replan_drift_factor must be >= 0, got %v", ErrConfigRange, f)
			}
			db.cfgMu.Lock()
			db.driftFactor = f
			db.cfgMu.Unlock()
			return nil
		},
	},
	"page_size": {
		tier: tierCreate, typ: "int",
		get: func(db *DB) value.Value { return value.Int(int64(db.PageSize())) },
	},
	"read_only": {
		tier: tierReadOnly, typ: "bool",
		get: func(db *DB) value.Value { return value.Bool(db.readOnly) },
	},
	"page_count": {
		tier: tierReadOnly, typ: "int",
		get: func(db *DB) value.Value {
			info, err := db.Info()
			if err != nil {
				return value.Null
			}
			return value.Int(int64(info.PageCount))
		},
	},
	"plan_cache_size": {
		tier: tierReadOnly, typ: "int",
		get: func(db *DB) value.Value { return value.Int(int64(db.cache.Cap())) },
	},
	"wal_checkpoint": {
		tier: tierAction, typ: "action",
		act: (*DB).walCheckpoint,
	},
	"tracing_detail": {
		tier: tierSession, typ: "string",
		get: func(db *DB) value.Value { return value.String(db.tracingDetailVal()) },
		set: func(db *DB, v value.Value) error {
			s, err := pragmaString("tracing_detail", v)
			if err != nil {
				return err
			}
			if s != "phase" && s != "operator" {
				return fmt.Errorf("%w: tracing_detail must be 'phase' or 'operator', got %q", ErrConfigRange, s)
			}
			db.cfgMu.Lock()
			db.tracingDetail = s
			db.cfgMu.Unlock()
			return nil
		},
	},
}

// walCheckpoint runs a WAL checkpoint now (doc 24 §3.7): it flushes every committed frame
// into the main file and truncates the WAL, the on-demand counterpart to the automatic
// triggers (doc 05 §6). The optional mode argument names the SQLite checkpoint mode; gr has
// one checkpoint primitive (full flush plus WAL truncation), which is the TRUNCATE mode and
// also satisfies FULL (every frame is checkpointed), so those two modes run it and the
// finer-grained PASSIVE (non-blocking partial) and RESTART modes are rejected rather than
// silently treated as a truncating checkpoint they are not. It returns a one-row result
// naming the mode that ran.
func (db *DB) walCheckpoint(arg value.Value) (*Result, error) {
	if db.readOnly {
		return nil, fmt.Errorf("%w: wal_checkpoint", ErrReadOnly)
	}
	mode := "TRUNCATE"
	if !arg.IsNull() {
		s, ok := arg.AsString()
		if !ok {
			return nil, fmt.Errorf("%w: wal_checkpoint mode must be a word (PASSIVE/FULL/RESTART/TRUNCATE)", ErrConfigType)
		}
		mode = strings.ToUpper(s)
	}
	switch mode {
	case "TRUNCATE", "FULL":
		// gr's checkpoint flushes all committed frames and truncates the WAL, which is a
		// superset of FULL (all frames checkpointed) and exactly TRUNCATE (WAL reset).
	case "PASSIVE", "RESTART":
		return nil, fmt.Errorf("%w: wal_checkpoint mode %s is not implemented; gr runs a full, WAL-truncating checkpoint (use TRUNCATE)", ErrConfigRange, mode)
	default:
		return nil, fmt.Errorf("%w: unknown wal_checkpoint mode %q", ErrConfigRange, mode)
	}
	// Capture the engine's cumulative per-checkpoint counts before the fold so the difference
	// after it is exactly this checkpoint's work, the figures the checkpoint_complete event
	// carries (doc 20 §11.3). The counts are lock-free atomics the checkpoint bumps under the
	// engine lock, so reading them on either side of the call is safe and never takes the lock.
	pagesBefore := db.eng.CheckpointPagesWrittenTotal()
	foldedBefore := db.eng.CheckpointDeltaFoldedTotal()
	start := time.Now()
	if err := db.eng.Checkpoint(); err != nil {
		return nil, err
	}
	done := time.Now()
	// This PRAGMA is the manual trigger (doc 20 §5.4); the timer and wal_threshold triggers
	// record their own when the automatic checkpoint scheduler lands.
	db.metrics.recordCheckpoint("manual", done.Sub(start), done.Unix())
	// Emit the checkpoint_complete event with the work this checkpoint did, the operational
	// narrative an operator reads for checkpoint cadence and durations (doc 20 §11.3). The
	// checkpoint_start event waits on the WAL-backlog substrate its wal_backlog_bytes field
	// needs, so only the completion event fires today.
	db.events.CheckpointComplete(
		int(db.eng.CheckpointPagesWrittenTotal()-pagesBefore),
		int(db.eng.CheckpointDeltaFoldedTotal()-foldedBefore),
		done.Sub(start),
	)
	return &Result{cols: []string{"wal_checkpoint"}, buf: []eval.Row{{"wal_checkpoint": value.String(strings.ToLower(mode))}}}, nil
}

// execPragma runs a PRAGMA against the configuration subsystem and returns its result: the
// effective value for the query form, an empty result for a successful set form, or the
// discovery rows for pragma_list (doc 24 §3.3, §3.4, §23.2). An unknown name is a loud
// error (doc 24 §3.8). A PRAGMA touches no graph data, so it runs outside the read/write
// operator pipeline, the same as a schema or administrative statement.
func (db *DB) execPragma(cmd *ast.PragmaCommand) (*Result, error) {
	if db.eng == nil {
		return nil, ErrClosed
	}
	// pragma_list is the discovery surface, not a settable or callable knob (doc 24 §23.2).
	if cmd.Name == "pragma_list" {
		if cmd.Set || cmd.Call {
			return nil, fmt.Errorf("%w: pragma_list", ErrNotSettable)
		}
		return db.pragmaListResult(), nil
	}
	p, ok := pragmas[cmd.Name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownPragma, cmd.Name)
	}
	// An action pragma (doc 24 §3.7) runs on the call form PRAGMA name(arg) or the bare
	// query form PRAGMA name; it is invoked, never set. The bare form carries a null
	// argument, which the action treats as its default.
	if p.act != nil {
		if cmd.Set {
			return nil, fmt.Errorf("%w: %s is an action pragma and is invoked, not set", ErrNotSettable, cmd.Name)
		}
		arg := value.Null
		if cmd.Call {
			arg = cmd.Value
		}
		return p.act(db, arg)
	}
	// A value pragma has no call form; the parenthesis is a misuse.
	if cmd.Call {
		return nil, fmt.Errorf("%w: %s is not an action pragma and takes no call form", ErrNotSettable, cmd.Name)
	}
	if cmd.Set {
		if p.set == nil {
			// A create-time knob conflicts; a read-only computed knob is simply not
			// settable (doc 24 §24.4).
			if p.tier == tierCreate {
				return nil, fmt.Errorf("%w: %s", ErrConfigConflict, cmd.Name)
			}
			return nil, fmt.Errorf("%w: %s", ErrNotSettable, cmd.Name)
		}
		// Capture the value before the change so the config_change event carries the old and
		// new values (doc 20 §11.3), the audit trail for a runtime reconfiguration. The read is
		// the same lock-free getter the query form uses, so it adds no lock to the set path.
		old := p.get(db)
		if err := p.set(db, cmd.Value); err != nil {
			return nil, err
		}
		// A setting changed, so record it: the setting, its old and new rendered values, and
		// the principal. A library call is "embedded"; the served path threads its user once the
		// pragma surface carries a principal.
		db.events.ConfigChange(cmd.Name, old.String(), p.get(db).String(), "embedded")
		return emptyResult(), nil
	}
	return &Result{cols: []string{cmd.Name}, buf: []eval.Row{{cmd.Name: p.get(db)}}}, nil
}

// pragmaListResult builds the PRAGMA pragma_list discovery result (doc 24 §23.2): one row
// per known pragma with its name, tier, and type, sorted by name so the listing is stable.
// pragma_list itself is listed too, so the discovery surface is self-describing.
func (db *DB) pragmaListResult() *Result {
	names := make([]string, 0, len(pragmas)+1)
	for name := range pragmas {
		names = append(names, name)
	}
	names = append(names, "pragma_list")
	sort.Strings(names)
	buf := make([]eval.Row, 0, len(names))
	for _, name := range names {
		tier, typ := "read-only", "list"
		if p, ok := pragmas[name]; ok {
			tier, typ = p.tier.String(), p.typ
		}
		buf = append(buf, eval.Row{
			"name": value.String(name),
			"tier": value.String(tier),
			"type": value.String(typ),
		})
	}
	return &Result{cols: []string{"name", "tier", "type"}, buf: buf}
}

// lazyDefault returns the connection's property-materialization default under the config
// lock, so a concurrent PRAGMA set does not race the read (doc 24 §3.4).
func (db *DB) lazyDefault() bool {
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	return db.lazyProps
}

// memBudgetVal returns the per-operator memory budget under the config lock.
func (db *DB) memBudgetVal() int64 {
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	return db.memBudget
}

// retries returns the conflict-retry bound under the config lock.
func (db *DB) retries() int {
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	return db.maxRetries
}

// tracingDetailVal returns the tracing verbosity level under the config lock.
func (db *DB) tracingDetailVal() string {
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	if db.tracingDetail == "" {
		return "phase"
	}
	return db.tracingDetail
}

// drift returns the re-plan drift factor under the config lock.
func (db *DB) drift() float64 {
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	return db.driftFactor
}

// pragmaBool coerces a PRAGMA value to a boolean, accepting a bool directly and an integer
// 0/1 as a convenience (doc 24 §24.4); anything else is a type error naming the knob.
func pragmaBool(name string, v value.Value) (bool, error) {
	if b, ok := v.AsBool(); ok {
		return b, nil
	}
	if n, ok := v.AsInt(); ok {
		switch n {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
	}
	return false, fmt.Errorf("%w: %s expects a boolean (on/off, true/false)", ErrConfigType, name)
}

// pragmaInt coerces a PRAGMA value to an integer (doc 24 §24.4); a non-integer is a type
// error naming the knob.
func pragmaInt(name string, v value.Value) (int64, error) {
	if n, ok := v.AsInt(); ok {
		return n, nil
	}
	return 0, fmt.Errorf("%w: %s expects an integer", ErrConfigType, name)
}

// pragmaFloat coerces a PRAGMA value to a float, widening an integer (doc 24 §24.4); a
// non-numeric value is a type error naming the knob.
func pragmaFloat(name string, v value.Value) (float64, error) {
	if f, ok := v.AsFloat(); ok {
		return f, nil
	}
	return 0, fmt.Errorf("%w: %s expects a number", ErrConfigType, name)
}

func pragmaString(name string, v value.Value) (string, error) {
	if s, ok := v.AsString(); ok {
		return s, nil
	}
	return "", fmt.Errorf("%w: %s expects a string", ErrConfigType, name)
}
