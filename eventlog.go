package gr

import (
	"context"
	"log/slog"
	"time"
)

// LevelTrace is the most detailed severity, below slog's Debug (doc 20 §11.1): per-operation
// tracing for deep debugging, off in production. The other levels are slog's own Debug, Info,
// Warn, and Error, so an embedder configures gr's event log the way it configures its own.
const LevelTrace = slog.Level(-8)

// The event taxonomy (doc 20 §11.3). Every structured-event entry carries one of these as
// its event field, so a log pipeline filters by event type and an operator knows the
// vocabulary. The set is additive: a new event is a new constant with its fields documented
// here, the way a new metric is additive, so the taxonomy grows without breaking a pipeline
// keyed off the existing values.
const (
	EventOpen                = "open"                 // database opened
	EventClose               = "close"                // database closed
	EventRecoveryStart       = "recovery_start"       // crash recovery began on open
	EventRecoveryComplete    = "recovery_complete"    // recovery finished, durable prefix restored
	EventCheckpointStart     = "checkpoint_start"     // checkpoint began
	EventCheckpointComplete  = "checkpoint_complete"  // checkpoint finished
	EventGCRun               = "gc_run"               // version GC pass
	EventCompaction          = "compaction"           // storage compaction
	EventConfigChange        = "config_change"        // a PRAGMA or config setting changed
	EventQuerySlow           = "query_slow"           // a query exceeded the slow threshold
	EventQueryError          = "query_error"          // a query failed
	EventConstraintViolation = "constraint_violation" // a write violated a constraint
	EventConflict            = "conflict"             // a write-write conflict was detected
	EventSpill               = "spill"                // an operator spilled to disk
	EventFsyncError          = "fsync_error"          // an fsync failed, the fatal event
	EventIntegrityError      = "integrity_error"      // an integrity check found corruption
	EventBackupStart         = "backup_start"         // a backup operation began
	EventBackupComplete      = "backup_complete"      // a backup operation finished
	EventReplication         = "replication_event"    // a replication or WAL-shipping event
	EventAuthFailure         = "auth_failure"         // an authentication attempt failed
	EventOverload            = "overload"             // the server shed or queued load
)

// EventLog records the structured operational-event stream that sits below the query log
// (doc 20 §11): startup, shutdown, checkpoints, recovery, errors, configuration changes, the
// events an operator reads to understand what the engine did and when. It writes through an
// slog.Logger, so the log lands wherever the embedder or the server points slog and gr does
// not impose a logging stack.
//
// A nil *EventLog is disabled and records nothing, the embedded-friendly default when no
// logger is configured. The severity threshold is the slog handler's, which an embedder sets
// and (with a slog.LevelVar) raises or lowers at runtime without a restart (doc 20 §11.1).
type EventLog struct {
	logger *slog.Logger
	now    func() time.Time // clock for the ts field; nil uses time.Now
}

// NewEventLog builds an event log that writes through logger (doc 20 §11.2). A nil logger
// returns nil, a disabled log, so a caller always calls the record methods without first
// checking whether a log is configured.
func NewEventLog(logger *slog.Logger) *EventLog {
	if logger == nil {
		return nil
	}
	return &EventLog{logger: logger}
}

// clock returns the current time for the ts field, falling back to time.Now when no clock
// seam is set.
func (l *EventLog) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Event emits one taxonomy event at the given severity with the common fields and the
// event-specific attrs (doc 20 §11.2): ts (the event time in RFC 3339 UTC), event (the type),
// the slog level, msg (the slog message), and the attrs. A nil log is a no-op, and the slog
// handler drops an entry below its threshold, so an event below the configured level costs
// only the call. This is the general path; the typed helpers below wrap it for the common
// events so a call site cannot misname an event or omit a documented field.
func (l *EventLog) Event(level slog.Level, event, msg string, attrs ...slog.Attr) {
	if l == nil {
		return
	}
	if !l.logger.Enabled(context.Background(), level) {
		return
	}
	all := make([]slog.Attr, 0, len(attrs)+2)
	all = append(all,
		slog.String("ts", l.clock().UTC().Format(time.RFC3339Nano)),
		slog.String("event", event),
	)
	all = append(all, attrs...)
	l.logger.LogAttrs(context.Background(), level, msg, all...)
}

// Open records that a database opened (doc 20 §11.3): the path, the file format version, the
// page size, and whether the open recovered a WAL after a crash.
func (l *EventLog) Open(path string, formatVersion uint32, pageSize uint32, recovered bool) {
	l.Event(slog.LevelInfo, EventOpen, "database opened",
		slog.String("path", path),
		slog.Uint64("format_version", uint64(formatVersion)),
		slog.Uint64("page_size", uint64(pageSize)),
		slog.Bool("recovered", recovered),
	)
}

// Close records that a database closed (doc 20 §11.3): the path and whether the close was
// clean (a normal shutdown, not a close that left work undone).
func (l *EventLog) Close(path string, clean bool) {
	l.Event(slog.LevelInfo, EventClose, "database closed",
		slog.String("path", path),
		slog.Bool("clean", clean),
	)
}

// RecoveryComplete records that crash recovery finished (doc 20 §11.3): how many transactions
// were replayed, the last applied LSN, and how long recovery took.
func (l *EventLog) RecoveryComplete(transactionsReplayed int, lastLSN uint64, d time.Duration) {
	l.Event(slog.LevelInfo, EventRecoveryComplete, "recovery complete",
		slog.Int("transactions_replayed", transactionsReplayed),
		slog.Uint64("last_lsn", lastLSN),
		slog.Float64("duration_ms", float64(d)/float64(time.Millisecond)),
	)
}

// CheckpointComplete records that a checkpoint finished (doc 20 §11.3): how many pages it
// wrote, how much delta it folded, and how long it took.
func (l *EventLog) CheckpointComplete(pagesWritten int, deltaFolded int, d time.Duration) {
	l.Event(slog.LevelInfo, EventCheckpointComplete, "checkpoint complete",
		slog.Int("pages_written", pagesWritten),
		slog.Int("delta_folded", deltaFolded),
		slog.Float64("duration_ms", float64(d)/float64(time.Millisecond)),
	)
}

// ConfigChange records that a setting changed (doc 20 §11.3): the setting, its old and new
// values, and who changed it, the audit trail for a runtime reconfiguration.
func (l *EventLog) ConfigChange(setting, oldValue, newValue, who string) {
	l.Event(slog.LevelInfo, EventConfigChange, "configuration changed",
		slog.String("setting", setting),
		slog.String("old", oldValue),
		slog.String("new", newValue),
		slog.String("who", who),
	)
}

// QuerySlow records that a query crossed the slow threshold (doc 20 §11.3): the query id
// that correlates it to the full query-log entry, the statement kind, how long it ran, and
// the threshold it crossed. It is a warn so a log-based alert can watch the slow tail. The
// full record (the cypher, the parameters, the row count) is in the query log; this is the
// lighter signal an operator alerts on.
func (l *EventLog) QuerySlow(queryID, kind string, d, threshold time.Duration) {
	l.Event(slog.LevelWarn, EventQuerySlow, "slow query",
		slog.String("query_id", queryID),
		slog.String("kind", kind),
		slog.Float64("duration_ms", float64(d)/float64(time.Millisecond)),
		slog.Float64("threshold_ms", float64(threshold)/float64(time.Millisecond)),
	)
}

// QueryError records that a query failed (doc 20 §11.3): the query id, the kind, the
// query-log status (error, timeout, killed), and the error text. It is an error so a log
// pipeline alerts on it. The full record is in the query log; this is the lighter signal.
func (l *EventLog) QueryError(queryID, kind, status, errText string) {
	l.Event(slog.LevelError, EventQueryError, "query failed",
		slog.String("query_id", queryID),
		slog.String("kind", kind),
		slog.String("status", status),
		slog.String("error", errText),
	)
}

// AuthFailure records a failed authentication attempt (doc 20 §11.3): the user, the client
// address, and the reason, at warn so a log-based alert can watch a rising rate of failures.
func (l *EventLog) AuthFailure(user, client, reason string) {
	l.Event(slog.LevelWarn, EventAuthFailure, "authentication failed",
		slog.String("user", user),
		slog.String("client", client),
		slog.String("reason", reason),
	)
}

// Overload records that the server shed or queued load (doc 20 §11.3): the in-flight and
// queued counts and the action taken, at warn so an operator sees the server protecting
// itself before it falls over.
func (l *EventLog) Overload(inflight, queued int, action string) {
	l.Event(slog.LevelWarn, EventOverload, "server overloaded",
		slog.Int("inflight", inflight),
		slog.Int("queued", queued),
		slog.String("action", action),
	)
}
