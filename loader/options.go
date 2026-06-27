package loader

import "io"

// OnPolicy controls how the loader handles a class of bad-line problem
// (doc 19 §11.2).
type OnPolicy uint8

const (
	// Skip records the bad line and continues the load.
	Skip OnPolicy = iota
	// Fail aborts the load on the first occurrence of this problem.
	Fail
)

// NodeSource describes one node-set to load: a label (optional, may also come
// from a :LABEL column), a header (the first file, or a separate header file),
// and one or more data files (doc 19 §5.3).
type NodeSource struct {
	// Label is the prefix label (from --nodes=Label=...). It is merged with any
	// :LABEL column; when neither is present every node in the set has no label.
	Label string
	// IDSpace is the id space for the :ID column (from --nodes syntax or the
	// header). Empty means the global space.
	IDSpace string
	// IDProperty, if non-empty, stores the input :ID as a queryable property of
	// this name on each node (doc 19 §7.2), so post-load queries can read the
	// input id back and match on it. The :ID is otherwise consumed into gr's
	// internal element id and is not visible to a query. Declare an index on the
	// property separately (CREATE INDEX) when a point lookup on it must be fast.
	IDProperty string
	// Files is the list of CSV (or Parquet) files for this set. The first file
	// may be a header-only file (no data rows) when Header is empty; if Header
	// is non-empty it is the header string and all Files are data files.
	Files []string
	// Header is the raw header line, if supplied out-of-band. When empty the
	// first line of the first file is the header.
	Header string

	// r are the readers for each file; set by the caller for the streaming API.
	// When nil, the loader opens Files by path.
	readers []io.Reader
}

// RelSource describes one relationship-set (doc 19 §5.3).
type RelSource struct {
	Type       string // prefix relationship type (from --relationships=Type=...)
	StartSpace string // id space for :START_ID
	EndSpace   string // id space for :END_ID
	Files      []string
	Header     string
	readers    []io.Reader
}

// Options configures a load operation (doc 19 §13.2 and §11.2).
type Options struct {
	// Nodes is the ordered list of node sources.
	Nodes []NodeSource
	// Relationships is the ordered list of relationship sources.
	Relationships []RelSource

	// Delimiter is the CSV field separator. Zero defaults to ','.
	Delimiter rune
	// ArrayDelim is the separator for list-typed fields. Zero defaults to ';'.
	ArrayDelim rune

	// OnDuplicateID controls what happens when a node id is seen twice in the
	// same id space. Default is Fail.
	OnDuplicateID OnPolicy
	// OnMissingID controls what happens when a node row has no :ID value.
	// Default is Fail.
	OnMissingID OnPolicy
	// OnDangling controls what happens when a relationship endpoint does not
	// resolve. Default is Skip.
	OnDangling OnPolicy
	// BadTolerance is the maximum number of bad lines before the load aborts.
	// Zero means no limit.
	BadTolerance int

	// BadFile is the path to write the bad-line file. Empty means no file.
	BadFile string
	// BadWriter is an alternative to BadFile for the streaming API.
	BadWriter io.Writer

	// Workers is the number of worker goroutines. Zero defaults to 1 (serial).
	// Parallelism is a later milestone; this field is accepted and ignored until
	// then (doc 19 §8).
	Workers int

	// MaxPoolPages bounds the build pager's buffer pool in pages; 0 keeps the
	// pager's small built-in default. The four-pass build writes the whole output
	// file through this pool, so a default-sized pool against a multi-gigabyte
	// load evicts and re-faults the column and adjacency pages it is still filling.
	// Raise it to keep the pages being built resident and cut the build's eviction
	// churn.
	MaxPoolPages int
}

func (o *Options) delimiter() rune {
	if o.Delimiter != 0 {
		return o.Delimiter
	}
	return ','
}

func (o *Options) arrayDelim() rune {
	if o.ArrayDelim != 0 {
		return o.ArrayDelim
	}
	return ';'
}
