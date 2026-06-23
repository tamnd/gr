package gr

import "time"

// HealthReport is the structured health view of a running database (doc 20 §13.3): the engine
// state, whether it can serve, the key liveness gauges, and any active warnings. It is the
// human-and-script operator's at-a-glance health view, distinct from the orchestrator's
// boolean liveness and readiness probes: it carries why the engine is in its state and what an
// operator should look at, where the probes carry only the pass or fail an orchestrator acts
// on. The report is built from the same metric snapshot the dashboards read (doc 20 §7.3), so
// it never disagrees with the metrics, the surface-coherence doc 20 §19.2 requires.
type HealthReport struct {
	// State is "open" when the engine is serving, "stopped" once a fatal durability failure
	// (a WAL fsync error, doc 20 §13.5) has put it in the state a restart-and-recover replaces.
	// A closed handle reports "stopped" too, since it serves nothing.
	State string `json:"state"`
	// Ready reports whether the engine answers a shallow read right now (doc 20 §13.2), the
	// same probe /readyz uses; it is false for a closed or fatally stopped engine.
	Ready bool `json:"ready"`
	// InflightQueries is the count of queries executing right now, summed across kinds, the
	// load signal an operator reads against the admission limit (doc 20 §13.3).
	InflightQueries int64 `json:"inflight_queries"`
	// OpenTransactions is the count of open transactions, summed across read and write, the
	// signal of a leaked or long-held transaction (doc 20 §13.3).
	OpenTransactions int64 `json:"open_transactions"`
	// OpenSessions is the count of open sessions on the served surfaces, zero for a purely
	// embedded database.
	OpenSessions int64 `json:"open_sessions"`
	// Commits is the count of write transactions committed durably since open, the
	// fsync-amortization numerator (doc 20 §5).
	Commits uint64 `json:"commits"`
	// Checkpoints is the count of checkpoints run since open, summed across triggers; with
	// LastCheckpoint it tells an operator whether the checkpointer is keeping up (doc 20 §13.3).
	Checkpoints uint64 `json:"checkpoints"`
	// LastCheckpoint is the wall-clock time of the last successful checkpoint, zero if none has
	// run yet (doc 20 §13.3). It is omitted from the JSON when zero.
	LastCheckpoint time.Time `json:"last_checkpoint,omitempty"`
	// WALFsyncErrors is the count of WAL fsyncs that returned an error, any nonzero value the
	// durability alarm that puts the engine in the stopped state (doc 20 §13.5).
	WALFsyncErrors uint64 `json:"wal_fsync_errors"`
	// Warnings is the list of active health concerns the report surfaces, the "what to look at"
	// an operator reads before the metrics (doc 20 §13.3). It is empty for a healthy engine.
	Warnings []string `json:"warnings"`
}

// Health builds the structured health report (doc 20 §13.3) from the metric snapshot and a
// shallow engine probe. It is the shared source the served /healthz/detail endpoint and the
// CLI .health command both render, so the two never disagree, and it is safe to call on a
// closed database, which reports a stopped, not-ready engine.
//
// The report honestly carries only what the engine measures today: the state, the liveness
// gauges, the checkpoint and commit progress, and the fsync-error alarm. The doc 20 §13.3
// fields that wait on substrate that does not exist yet (the WAL backlog, the dirty-page
// count, the oldest-snapshot age, the file-integrity status, and the recovery progress) are
// omitted until the subsystems that produce them land, the same discipline the deferred
// metrics carry.
func (db *DB) Health() HealthReport {
	if db.eng == nil {
		return HealthReport{State: "stopped", Ready: false, Warnings: []string{"engine is closed"}}
	}
	snap := db.Metrics()

	rep := HealthReport{
		InflightQueries:  sumGauge(snap, "gr_query_inflight"),
		OpenTransactions: sumGauge(snap, "gr_transactions_open"),
		OpenSessions:     sumGauge(snap, "gr_sessions_open"),
		Commits:          snap.Counter("gr_commits_total", nil),
		Checkpoints:      sumCounter(snap, "gr_checkpoint_total"),
		WALFsyncErrors:   snap.Counter("gr_wal_fsync_errors_total", nil),
	}
	if ts := snap.Gauge("gr_checkpoint_last_timestamp_seconds", nil); ts > 0 {
		rep.LastCheckpoint = time.Unix(ts, 0).UTC()
	}

	// Readiness is derived from the snapshot, not from a probe that takes the engine lock: a
	// health report must answer even while a write transaction holds that lock for its whole
	// life (doc 20 §22.6), so it reads only the lock-free metrics the same way db.Metrics does,
	// never the engine lock, which would deadlock against a held writer. A WAL fsync error is
	// the durability alarm that stops the engine (doc 20 §13.5); absent it, an open engine is
	// serving and ready.
	if rep.WALFsyncErrors > 0 {
		rep.State = "stopped"
		rep.Ready = false
		rep.Warnings = append(rep.Warnings, "wal fsync errors: durability lost, restart and recover")
	} else {
		rep.State = "open"
		rep.Ready = true
	}

	// A write workload with no checkpoint yet means the WAL is carrying every commit, which
	// lengthens recovery; it is worth flagging once commits have happened but no fold has run.
	if rep.LastCheckpoint.IsZero() && rep.Commits > 0 {
		rep.Warnings = append(rep.Warnings, "no checkpoint has run yet: recovery would replay the whole WAL")
	}

	return rep
}

// sumGauge totals a gauge across every label set in the snapshot (doc 20 §7.3), so a
// per-kind or per-mode gauge like gr_query_inflight reads as one number for the health report.
func sumGauge(snap MetricsSnapshot, name string) int64 {
	var total int64
	for _, m := range snap.Metrics() {
		if m.Name == name && m.Type == MetricGauge {
			total += m.Gauge
		}
	}
	return total
}

// sumCounter totals a counter across every label set in the snapshot, the counter analogue of
// sumGauge for a per-trigger counter like gr_checkpoint_total.
func sumCounter(snap MetricsSnapshot, name string) uint64 {
	var total uint64
	for _, m := range snap.Metrics() {
		if m.Name == name && m.Type == MetricCounter {
			total += m.Counter
		}
	}
	return total
}
