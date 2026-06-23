package loader

import (
	"io"
	"strings"
	"testing"
)

// FuzzImportCSV verifies that the pass-1 CSV node scanner never panics on
// arbitrary header+data input (doc 23 §7.5). Clean parse errors or bad-line
// rejections are correct behaviour; only a panic or hang is a failure.
func FuzzImportCSV(f *testing.F) {
	// Seed corpus: well-formed and deliberately malformed CSV inputs.
	seeds := []string{
		// Minimal valid node file.
		":ID\n1\n2\n3\n",
		// With a label column.
		":ID,:LABEL\nn1,Person\nn2,Movie\n",
		// With typed properties.
		":ID(person),name:string,age:int,:LABEL\np1,Alice,30,Person\np2,Bob,25,Person\n",
		// Array property.
		":ID,tags:string[]\nn1,a;b;c\n",
		// Empty data (header only).
		":ID,:LABEL\n",
		// No :ID column — every row is a bad line.
		"name:string\nAlice\n",
		// Completely empty.
		"",
		// Only a newline.
		"\n",
		// Header with BOM.
		"\xef\xbb\xbf:ID\n1\n",
		// Truncated mid-row.
		":ID,name:string\n1,",
		// Quoted fields.
		`:ID,name:string` + "\n" + `1,"Alice ""A"" Smith"` + "\n",
		// Very long line.
		":ID\n" + strings.Repeat("x", 65536) + "\n",
		// Null bytes.
		":ID\n\x001\n",
		// Wrong delimiter.
		":ID|name:string\n1|Alice\n",
		// Duplicate column names.
		":ID,:ID\n1,2\n",
		// ID space in header.
		":ID(space),name:string\ns1,Alice\n",
		// Numeric ids.
		":ID\n0\n-1\n9999999999999999999\n",
		// LABEL from prefix, no :LABEL column.
		":ID\np1\np2\n",
		// Unicode in values.
		":ID,name:string\n1,中文\n2,café\n",
	}

	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, csvData string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Pass1ScanNodes panicked on input (len=%d): %v", len(csvData), r)
			}
		}()

		l := New(Options{
			Nodes: []NodeSource{
				{readers: []io.Reader{strings.NewReader(csvData)}},
			},
			OnDuplicateID: Skip,
			OnMissingID:   Skip,
			BadTolerance:  1000,
		})
		// Pass1ScanNodes may return an error for malformed input — that is correct.
		_ = l.Pass1ScanNodes()
	})
}
