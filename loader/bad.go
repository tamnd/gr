package loader

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// RejectCode is the stable machine code for a bad-line rejection (doc 19 §11.4).
type RejectCode string

const (
	CodeParse     RejectCode = "parse"      // type parse failure
	CodeMissingID RejectCode = "missing_id" // :ID field absent or empty
	CodeDupID     RejectCode = "dup_id"     // duplicate node id in a space
	CodeDangling  RejectCode = "dangling"   // relationship with unresolved endpoint
	CodeUnique    RejectCode = "unique"     // uniqueness constraint violation
	CodeExistence RejectCode = "existence"  // existence constraint violation
)

// Reject is one entry in the bad-line file (doc 19 §11.4).
type Reject struct {
	SourceFile string
	LineNo     int
	Code       RejectCode
	Detail     string
	RawRow     string
}

// BadLineSink collects rejected input rows (doc 19 §11.4). It writes one CSV
// record per reject to an underlying writer (the bad-line file) and keeps a
// running count per reason code so the post-load report can tally them.
//
// The bad-line file format (normative, doc 19 §11.4):
//
//	source_file, line_no, reason_code, reason_detail, raw_row
//
// It is always written in CSV so tooling can parse it reliably. The sink is
// nil-safe: a nil sink silently drops rejects (useful in tests that do not care
// about the file, and in the --dry-run path).
type BadLineSink struct {
	w      *csv.Writer
	counts map[RejectCode]int
	total  int
}

// newBadLineSink wraps w as a bad-line sink, writing the header line immediately.
// Pass nil to create a sink that counts but does not write.
func newBadLineSink(w io.Writer) *BadLineSink {
	s := &BadLineSink{counts: make(map[RejectCode]int)}
	if w != nil {
		s.w = csv.NewWriter(w)
		_ = s.w.Write([]string{"source_file", "line_no", "reason_code", "reason_detail", "raw_row"})
	}
	return s
}

// Reject records one bad line. It writes to the bad-line file if a writer was
// given and always increments the counters.
func (s *BadLineSink) Reject(r Reject) {
	if s == nil {
		return
	}
	s.counts[r.Code]++
	s.total++
	if s.w != nil {
		_ = s.w.Write([]string{
			r.SourceFile,
			fmt.Sprintf("%d", r.LineNo),
			string(r.Code),
			r.Detail,
			r.RawRow,
		})
	}
}

// Flush finalizes any buffered writes to the underlying writer.
func (s *BadLineSink) Flush() {
	if s == nil || s.w == nil {
		return
	}
	s.w.Flush()
}

// Total returns the total number of rejected lines so far.
func (s *BadLineSink) Total() int {
	if s == nil {
		return 0
	}
	return s.total
}

// Count returns the number of rejects for one code.
func (s *BadLineSink) Count(code RejectCode) int {
	if s == nil {
		return 0
	}
	return s.counts[code]
}

// joinFields joins a field slice back into a raw CSV row for the raw_row column.
// This is a best-effort reconstruction, not a faithful round-trip of the original
// bytes — for the bad-line file, human readability matters more than byte identity.
func joinFields(fields []string) string {
	var sb strings.Builder
	for i, f := range fields {
		if i > 0 {
			sb.WriteByte(',')
		}
		if strings.ContainsAny(f, ",\"\n\r") {
			sb.WriteByte('"')
			sb.WriteString(strings.ReplaceAll(f, `"`, `""`))
			sb.WriteByte('"')
		} else {
			sb.WriteString(f)
		}
	}
	return sb.String()
}
