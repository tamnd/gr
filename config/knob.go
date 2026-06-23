// Package config is the canonical knob registry for gr (doc 24).
// It defines every tunable in one place: its name, tier, four surface
// spellings, type, default, and description. Other packages query this
// registry; the freeze test below keeps the 1.0 surface stable.
package config

// Tier classifies when and how a knob may be changed (doc 24 §2.1).
type Tier uint8

const (
	// TierCreate means the knob is baked into the file header at creation
	// and cannot change without a dump/reload (doc 24 §5).
	TierCreate Tier = iota + 1

	// TierPersistent means the knob is stored in the file's catalog and
	// remembered across reopens; a PRAGMA set writes it durably (doc 24 §6).
	TierPersistent

	// TierSession means the knob lives only in the open connection's memory
	// and resets on every Open (doc 24 §7).
	TierSession

	// TierReadOnly means the knob is a computed, read-only introspection
	// value; it has a PRAGMA get form but no set form (doc 24 §23).
	TierReadOnly
)

func (t Tier) String() string {
	switch t {
	case TierCreate:
		return "create-time"
	case TierPersistent:
		return "persistent"
	case TierSession:
		return "session"
	case TierReadOnly:
		return "read-only"
	default:
		return "unknown"
	}
}

// Knob is one entry in the canonical registry (doc 24 §4).
// Each field maps to one of the four surface spellings or a metadata column
// from the catalogue tables in doc 24 §8–20.
type Knob struct {
	// Name is the canonical lower_snake_case name (doc 24 §4.1).
	// This is the value the freeze guard checksums; renaming it is a
	// breaking change under the 1.0 contract.
	Name string

	// PRAGMA is the PRAGMA keyword form, always equal to Name (doc 24 §4.1).
	PRAGMA string

	// LibOption is the Go functional-option name, e.g. "WithSynchronous"
	// (doc 24 §4.1).
	LibOption string

	// CLIFlag is the kebab-case CLI flag, e.g. "--synchronous" (doc 24 §4.1).
	CLIFlag string

	// ConfigKey is the dotted server-config key, e.g. "durability.synchronous"
	// (doc 24 §4.1, §22).
	ConfigKey string

	// Tier classifies when the knob may change (doc 24 §2.1, §5–7).
	Tier Tier

	// KnobType is a human-readable type description for the discovery surface
	// (doc 24 §23.2). Examples: "bytes", "enum", "bool", "int", "duration_ms".
	KnobType string

	// Default is the built-in default as a string (doc 24 §1 — safe-by-default
	// principle). Displayed by PRAGMA discovery.
	Default string

	// Description is one sentence, operator-facing (doc 24 §8–20 table column).
	Description string
}

// Registry returns the complete list of knobs in their canonical order.
// This list is the 1.0 config surface; the freeze test below
// asserts that no existing knob's Name or Tier changes across releases.
func Registry() []Knob {
	return registry
}

// registry is the single source of truth for every gr knob (doc 24 §8–20).
// Sections are kept in the same order as the catalogue: pager/geometry (8),
// encryption (8), buffer pool/cache (8), durability (9), checkpoint (10),
// MVCC/transactions (11), planner (12), executor (13), caches (14),
// compression (15), indexing (16), server (18), logging/metrics/tracing (19).
var registry = []Knob{

	// --- Pager and storage geometry (doc 24 §8) ---

	{
		Name: "page_size", PRAGMA: "page_size",
		LibOption: "WithPageSize", CLIFlag: "--page-size", ConfigKey: "storage.page_size",
		Tier: TierCreate, KnobType: "bytes", Default: "4096",
		Description: "Page geometry in bytes; every page boundary depends on it.",
	},
	{
		Name: "checksum", PRAGMA: "checksum",
		LibOption: "WithChecksum", CLIFlag: "--checksum", ConfigKey: "storage.checksum",
		Tier: TierCreate, KnobType: "enum", Default: "xxh3",
		Description: "Per-page and header checksum algorithm (none/crc32c/xxh3).",
	},
	{
		Name: "encoding", PRAGMA: "encoding",
		LibOption: "WithEncoding", CLIFlag: "--encoding", ConfigKey: "storage.encoding",
		Tier: TierCreate, KnobType: "enum", Default: "utf8",
		Description: "Text encoding for stored strings; utf8 only in v1.",
	},
	{
		Name: "segment_size", PRAGMA: "segment_size",
		LibOption: "WithSegmentSize", CLIFlag: "--segment-size", ConfigKey: "storage.segment_size",
		Tier: TierCreate, KnobType: "int", Default: "4096",
		Description: "Positions per column/CSR segment; the on-disk segmentation is fixed by it.",
	},
	{
		Name: "case_sensitive", PRAGMA: "case_sensitive",
		LibOption: "WithCaseSensitive", CLIFlag: "--case-sensitive", ConfigKey: "storage.case_sensitive",
		Tier: TierCreate, KnobType: "bool", Default: "false",
		Description: "Label/type/property-key case sensitivity of the catalog token dictionary.",
	},
	{
		Name: "application_id", PRAGMA: "application_id",
		LibOption: "WithApplicationID", CLIFlag: "--application-id", ConfigKey: "storage.application_id",
		Tier: TierCreate, KnobType: "int", Default: "0",
		Description: "App-defined integer tag in the file header; set once at creation.",
	},
	{
		Name: "embedded_zone_db", PRAGMA: "embedded_zone_db",
		LibOption: "WithEmbeddedZoneDB", CLIFlag: "--embedded-zone-db", ConfigKey: "storage.embedded_zone_db",
		Tier: TierCreate, KnobType: "string", Default: "",
		Description: "IANA time-zone snapshot version for reproducible temporal arithmetic.",
	},
	{
		Name: "compression_baseline", PRAGMA: "compression_baseline",
		LibOption: "WithCompressionBaseline", CLIFlag: "--compression-baseline", ConfigKey: "storage.compression_baseline",
		Tier: TierCreate, KnobType: "enum", Default: "zstd",
		Description: "Default codec policy baked at creation; the floor for new segments.",
	},
	{
		Name: "database_uuid", PRAGMA: "database_uuid",
		LibOption: "", CLIFlag: "", ConfigKey: "",
		Tier: TierReadOnly, KnobType: "uuid", Default: "",
		Description: "Per-database UUID minted at creation; distinguishes a file from its copies.",
	},

	// --- Encryption (doc 24 §8) ---

	{
		Name: "encryption", PRAGMA: "encryption",
		LibOption: "WithEncryption", CLIFlag: "--encryption", ConfigKey: "storage.encryption",
		Tier: TierCreate, KnobType: "bool", Default: "off",
		Description: "At-rest encryption envelope; every page is enveloped when on.",
	},
	{
		Name: "cipher", PRAGMA: "cipher",
		LibOption: "WithCipher", CLIFlag: "--cipher", ConfigKey: "storage.cipher",
		Tier: TierCreate, KnobType: "enum", Default: "aes-256-gcm",
		Description: "AEAD cipher for at-rest encryption (aes-256-gcm/chacha20-poly1305).",
	},

	// --- Buffer pool and cache sizing (doc 24 §8) ---

	{
		Name: "cache_size", PRAGMA: "cache_size",
		LibOption: "WithCacheSize", CLIFlag: "--cache-size", ConfigKey: "cache.size",
		Tier: TierSession, KnobType: "bytes", Default: "auto",
		Description: "Total buffer-pool and cache budget; auto-sized to a fraction of RAM.",
	},
	{
		Name: "page_cache_size", PRAGMA: "page_cache_size",
		LibOption: "WithPageCacheSize", CLIFlag: "--page-cache-size", ConfigKey: "cache.page_cache_size",
		Tier: TierSession, KnobType: "bytes", Default: "derived",
		Description: "Fixed page-cache slice inside cache_size.",
	},
	{
		Name: "vfs", PRAGMA: "vfs",
		LibOption: "WithVFS", CLIFlag: "--vfs", ConfigKey: "storage.vfs",
		Tier: TierSession, KnobType: "enum", Default: "osfs",
		Description: "I/O backend (osfs/mmap/uring).",
	},
	{
		Name: "direct_io", PRAGMA: "direct_io",
		LibOption: "WithDirectIO", CLIFlag: "--direct-io", ConfigKey: "storage.direct_io",
		Tier: TierSession, KnobType: "bool", Default: "off",
		Description: "O_DIRECT, bypassing the OS page cache; for dedicated hosts with large cache.",
	},
	{
		Name: "mmap_size", PRAGMA: "mmap_size",
		LibOption: "WithMmapSize", CLIFlag: "--mmap-size", ConfigKey: "storage.mmap_size",
		Tier: TierSession, KnobType: "bytes", Default: "0",
		Description: "Maximum bytes to memory-map for read when vfs=mmap; 0 disables.",
	},

	// --- WAL and durability (doc 24 §9) ---

	{
		Name: "synchronous", PRAGMA: "synchronous",
		LibOption: "WithSynchronous", CLIFlag: "--synchronous", ConfigKey: "durability.synchronous",
		Tier: TierPersistent, KnobType: "enum", Default: "FULL",
		Description: "fsync discipline (OFF/NORMAL/FULL/EXTRA); the loss-window/speed dial.",
	},
	{
		Name: "journal_mode", PRAGMA: "journal_mode",
		LibOption: "WithJournalMode", CLIFlag: "--journal-mode", ConfigKey: "durability.journal_mode",
		Tier: TierPersistent, KnobType: "enum", Default: "WAL",
		Description: "Journaling mode (WAL/DELETE/TRUNCATE/PERSIST/MEMORY/OFF).",
	},
	{
		Name: "full_page_writes", PRAGMA: "full_page_writes",
		LibOption: "WithFullPageWrites", CLIFlag: "--full-page-writes", ConfigKey: "durability.full_page_writes",
		Tier: TierPersistent, KnobType: "bool", Default: "on",
		Description: "Log full page images on first change per checkpoint cycle for torn-write protection.",
	},
	{
		Name: "wal_full_image_always", PRAGMA: "wal_full_image_always",
		LibOption: "WithWALFullImageAlways", CLIFlag: "--wal-full-image-always", ConfigKey: "durability.wal_full_image_always",
		Tier: TierPersistent, KnobType: "bool", Default: "off",
		Description: "Force a full page image on every WAL write; maximum torn-write safety.",
	},
	{
		Name: "commit_linger_us", PRAGMA: "commit_linger_us",
		LibOption: "WithCommitLinger", CLIFlag: "--commit-linger-us", ConfigKey: "durability.commit_linger_us",
		Tier: TierPersistent, KnobType: "duration_us", Default: "adaptive",
		Description: "Group-commit window in microseconds; amortizes fsync across concurrent committers.",
	},

	// --- Checkpoint and space reclamation (doc 24 §10) ---

	{
		Name: "wal_autocheckpoint", PRAGMA: "wal_autocheckpoint",
		LibOption: "WithWALAutoCheckpoint", CLIFlag: "--wal-autocheckpoint", ConfigKey: "checkpoint.wal_autocheckpoint",
		Tier: TierPersistent, KnobType: "int", Default: "1000",
		Description: "WAL pages that trigger an automatic passive checkpoint.",
	},
	{
		Name: "wal_size_limit", PRAGMA: "wal_size_limit",
		LibOption: "WithWALSizeLimit", CLIFlag: "--wal-size-limit", ConfigKey: "checkpoint.wal_size_limit",
		Tier: TierPersistent, KnobType: "bytes", Default: "0",
		Description: "Maximum WAL file size before a blocking checkpoint is forced; 0 = no limit.",
	},
	{
		Name: "checkpoint_interval_s", PRAGMA: "checkpoint_interval_s",
		LibOption: "WithCheckpointInterval", CLIFlag: "--checkpoint-interval-s", ConfigKey: "checkpoint.checkpoint_interval_s",
		Tier: TierPersistent, KnobType: "int", Default: "0",
		Description: "Background checkpoint interval in seconds; 0 = passive only.",
	},
	{
		Name: "checkpoint_delta_threshold", PRAGMA: "checkpoint_delta_threshold",
		LibOption: "WithCheckpointDeltaThreshold", CLIFlag: "--checkpoint-delta-threshold", ConfigKey: "checkpoint.checkpoint_delta_threshold",
		Tier: TierPersistent, KnobType: "int", Default: "512",
		Description: "Delta-store entries that trigger a segment fold/checkpoint.",
	},
	{
		Name: "auto_vacuum", PRAGMA: "auto_vacuum",
		LibOption: "WithAutoVacuum", CLIFlag: "--auto-vacuum", ConfigKey: "checkpoint.auto_vacuum",
		Tier: TierPersistent, KnobType: "enum", Default: "NONE",
		Description: "Automatic freelist compaction mode (NONE/FULL/INCREMENTAL).",
	},
	{
		Name: "auto_compaction", PRAGMA: "auto_compaction",
		LibOption: "WithAutoCompaction", CLIFlag: "--auto-compaction", ConfigKey: "checkpoint.auto_compaction",
		Tier: TierPersistent, KnobType: "bool", Default: "on",
		Description: "Automatic segment compaction when sparsity exceeds the threshold.",
	},
	{
		Name: "compaction_sparsity_threshold", PRAGMA: "compaction_sparsity_threshold",
		LibOption: "WithCompactionSparsityThreshold", CLIFlag: "--compaction-sparsity-threshold", ConfigKey: "checkpoint.compaction_sparsity_threshold",
		Tier: TierPersistent, KnobType: "float", Default: "0.5",
		Description: "Live-row fraction below which a segment is compacted; 0.5 = 50% live.",
	},
	{
		Name: "inline_threshold", PRAGMA: "inline_threshold",
		LibOption: "WithInlineThreshold", CLIFlag: "--inline-threshold", ConfigKey: "storage.inline_threshold",
		Tier: TierPersistent, KnobType: "bytes", Default: "256",
		Description: "Maximum property bytes stored inline; larger values spill to overflow pages.",
	},

	// --- MVCC and transactions (doc 24 §11) ---

	{
		Name: "concurrency_model", PRAGMA: "concurrency_model",
		LibOption: "WithConcurrencyModel", CLIFlag: "--concurrency-model", ConfigKey: "mvcc.concurrency_model",
		Tier: TierPersistent, KnobType: "enum", Default: "single-writer",
		Description: "Concurrency model (single-writer/concurrent-writers); default is single-writer-first.",
	},
	{
		Name: "busy_timeout_ms", PRAGMA: "busy_timeout_ms",
		LibOption: "WithBusyTimeout", CLIFlag: "--busy-timeout-ms", ConfigKey: "mvcc.busy_timeout_ms",
		Tier: TierSession, KnobType: "duration_ms", Default: "0",
		Description: "How long to wait for a write lock before returning a busy error; 0 = no wait.",
	},
	{
		Name: "isolation", PRAGMA: "isolation",
		LibOption: "WithIsolation", CLIFlag: "--isolation", ConfigKey: "mvcc.isolation",
		Tier: TierSession, KnobType: "enum", Default: "snapshot",
		Description: "Transaction isolation level (snapshot/serializable).",
	},
	{
		Name: "access_mode", PRAGMA: "access_mode",
		LibOption: "WithReadOnly", CLIFlag: "--read-only", ConfigKey: "mvcc.access_mode",
		Tier: TierSession, KnobType: "enum", Default: "read-write",
		Description: "Access mode for the connection (read-write/read-only).",
	},
	{
		Name: "max_retries", PRAGMA: "max_retries",
		LibOption: "WithMaxRetries", CLIFlag: "--max-retries", ConfigKey: "mvcc.max_retries",
		Tier: TierSession, KnobType: "int", Default: "3",
		Description: "Conflict-retry bound for Update/ExecuteWrite closures.",
	},
	{
		Name: "max_txn_size", PRAGMA: "max_txn_size",
		LibOption: "WithMaxTxnSize", CLIFlag: "--max-txn-size", ConfigKey: "mvcc.max_txn_size",
		Tier: TierSession, KnobType: "int", Default: "0",
		Description: "Maximum write operations per transaction; 0 = no limit.",
	},

	// --- Planner (doc 24 §12) ---

	{
		Name: "statistics_target", PRAGMA: "statistics_target",
		LibOption: "WithStatisticsTarget", CLIFlag: "--statistics-target", ConfigKey: "planner.statistics_target",
		Tier: TierPersistent, KnobType: "int", Default: "100",
		Description: "Sample size per label/type for histogram statistics collection.",
	},
	{
		Name: "statistics_refresh_policy", PRAGMA: "statistics_refresh_policy",
		LibOption: "WithStatisticsRefreshPolicy", CLIFlag: "--statistics-refresh-policy", ConfigKey: "planner.statistics_refresh_policy",
		Tier: TierPersistent, KnobType: "enum", Default: "auto",
		Description: "When to refresh statistics (auto/manual/never).",
	},
	{
		Name: "plan_cache", PRAGMA: "plan_cache",
		LibOption: "WithPlanCache", CLIFlag: "--plan-cache", ConfigKey: "planner.plan_cache",
		Tier: TierSession, KnobType: "bool", Default: "on",
		Description: "Enable the compiled-plan cache; off for benchmarking or debugging.",
	},
	{
		Name: "adaptive_replanning", PRAGMA: "adaptive_replanning",
		LibOption: "WithAdaptiveReplanning", CLIFlag: "--adaptive-replanning", ConfigKey: "planner.adaptive_replanning",
		Tier: TierSession, KnobType: "bool", Default: "on",
		Description: "Re-cost a cached plan when statistics drift past a factor threshold.",
	},
	{
		Name: "auto_parameterize", PRAGMA: "auto_parameterize",
		LibOption: "WithAutoParameterize", CLIFlag: "--auto-parameterize", ConfigKey: "planner.auto_parameterize",
		Tier: TierSession, KnobType: "bool", Default: "on",
		Description: "Auto-extract literals as parameters to widen plan-cache sharing.",
	},

	// --- Executor (doc 24 §13) ---

	{
		Name: "query_timeout_ms", PRAGMA: "query_timeout_ms",
		LibOption: "WithStatementTimeout", CLIFlag: "--query-timeout-ms", ConfigKey: "executor.query_timeout_ms",
		Tier: TierSession, KnobType: "duration_ms", Default: "0",
		Description: "Default deadline for queries lacking a context deadline; 0 = no timeout.",
	},
	{
		Name: "max_query_memory", PRAGMA: "max_query_memory",
		LibOption: "WithQueryMemory", CLIFlag: "--max-query-memory", ConfigKey: "executor.max_query_memory",
		Tier: TierSession, KnobType: "bytes", Default: "0",
		Description: "Per-query memory budget; 0 = no limit.",
	},
	{
		Name: "spill_enabled", PRAGMA: "spill_enabled",
		LibOption: "WithSpillEnabled", CLIFlag: "--spill-enabled", ConfigKey: "executor.spill_enabled",
		Tier: TierSession, KnobType: "bool", Default: "on",
		Description: "Allow operators to spill to disk when the memory budget is exceeded.",
	},
	{
		Name: "morsel_size", PRAGMA: "morsel_size",
		LibOption: "WithMorselSize", CLIFlag: "--morsel-size", ConfigKey: "executor.morsel_size",
		Tier: TierSession, KnobType: "int", Default: "1024",
		Description: "Row batch size for morsel-parallel operators.",
	},
	{
		Name: "parallelism", PRAGMA: "parallelism",
		LibOption: "WithParallelism", CLIFlag: "--parallelism", ConfigKey: "executor.parallelism",
		Tier: TierSession, KnobType: "int", Default: "0",
		Description: "Degree of intra-query parallelism; 0 = auto from GOMAXPROCS.",
	},

	// --- Caches (doc 24 §14) ---

	{
		Name: "default_cache_size", PRAGMA: "default_cache_size",
		LibOption: "WithDefaultCacheSize", CLIFlag: "--default-cache-size", ConfigKey: "cache.default_cache_size",
		Tier: TierPersistent, KnobType: "bytes", Default: "auto",
		Description: "The cache_size a new opener defaults to when no open-time option names one.",
	},
	{
		Name: "cache_auto_fraction", PRAGMA: "cache_auto_fraction",
		LibOption: "WithCacheAutoFraction", CLIFlag: "--cache-auto-fraction", ConfigKey: "cache.cache_auto_fraction",
		Tier: TierPersistent, KnobType: "float", Default: "0.25",
		Description: "Fraction of available RAM auto-allocated to cache_size when not explicit.",
	},
	{
		Name: "readahead", PRAGMA: "readahead",
		LibOption: "WithReadahead", CLIFlag: "--readahead", ConfigKey: "cache.readahead",
		Tier: TierSession, KnobType: "bool", Default: "on",
		Description: "Sequential-scan readahead for column and page reads.",
	},
	{
		Name: "cache_disabled", PRAGMA: "cache_disabled",
		LibOption: "WithCacheDisabled", CLIFlag: "--cache-disabled", ConfigKey: "cache.cache_disabled",
		Tier: TierSession, KnobType: "bool", Default: "off",
		Description: "Disable all graph caches; for testing or strict memory bounds.",
	},

	// --- Compression and encoding (doc 24 §15) ---

	{
		Name: "compression", PRAGMA: "compression",
		LibOption: "WithCompression", CLIFlag: "--compression", ConfigKey: "storage.compression",
		Tier: TierPersistent, KnobType: "enum", Default: "zstd",
		Description: "Default codec for new column segments (none/lz4/zstd/snappy).",
	},
	{
		Name: "cold_compression", PRAGMA: "cold_compression",
		LibOption: "WithColdCompression", CLIFlag: "--cold-compression", ConfigKey: "storage.cold_compression",
		Tier: TierPersistent, KnobType: "enum", Default: "zstd-high",
		Description: "Codec applied during compaction of cold/infrequently-accessed segments.",
	},
}
