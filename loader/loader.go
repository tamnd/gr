package loader

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// LoadStats accumulates counters during a load for the post-load report
// (doc 19 §11.6).
type LoadStats struct {
	Nodes        uint64 // nodes accepted and assigned a dense id
	DupNodes     uint64 // node rows rejected for duplicate id (under Skip policy)
	BadNodes     uint64 // node rows rejected for other reasons (missing id, parse)
	Rels         uint64 // relationships accepted
	DanglingRels uint64 // relationship rows rejected for dangling endpoint
	BadLines     uint64 // total rejected lines across all files
}

// Loader orchestrates the four-pass bulk load pipeline (doc 19 §4).
//
// A Loader is created with [New], configured once, and run with [Loader.Run].
// It is not safe for concurrent use.
type Loader struct {
	opts    Options
	catalog *CatalogBuilder
	idmap   *IDMapBuilder
	bad     *BadLineSink
	stats   LoadStats
}

// New returns a Loader for a fresh .gr file, configured by opts. The actual
// output file is not opened here; it is created in Run. This allows the caller
// to inspect the Options before committing to disk.
//
// The four-pass pipeline (doc 19 §3.2):
//
//  1. pass1ScanNodes — scan node files, assign dense ids, build the id-map.
//  2. pass2BuildNodeColumns — scan node files again, build columnar segments.
//  3. pass3BuildCSR — scan relationship files, build the forward+backward CSR.
//  4. pass4PropsAndIndexes — scan relationship files again, build rel columns + indexes.
//
// Passes 2–4 are not yet implemented in this PR; they require the pager and
// column-segment infrastructure to be wired in. Pass 1 is self-contained.
func New(opts Options) *Loader {
	var badWriter io.Writer
	if opts.BadWriter != nil {
		badWriter = opts.BadWriter
	} else if opts.BadFile != "" {
		f, err := os.Create(opts.BadFile)
		if err == nil {
			badWriter = f
		}
	}
	return &Loader{
		opts:    opts,
		catalog: newCatalogBuilder(),
		idmap:   newIDMapBuilder(),
		bad:     newBadLineSink(badWriter),
	}
}

// Pass1ScanNodes scans all node files and builds the id-map and label-group
// assignment (doc 19 §4.1). After it returns, the Loader's IDMap and Catalog
// are populated and the per-group node counts are final.
//
// This is exported for testing; production use goes through Run.
func (l *Loader) Pass1ScanNodes() error {
	return l.pass1ScanNodes()
}

// IDMap returns the loader's in-flight external-id map, populated by pass 1.
func (l *Loader) IDMap() *IDMapBuilder { return l.idmap }

// Catalog returns the catalog builder populated across all passes.
func (l *Loader) Catalog() *CatalogBuilder { return l.catalog }

// Stats returns the accumulated load statistics.
func (l *Loader) Stats() LoadStats { return l.stats }

// Bad returns the bad-line sink.
func (l *Loader) Bad() *BadLineSink { return l.bad }

// nodeFile is one data-file worth of work: a csvReader already positioned at
// the first data row, paired with a name for error messages and an optional
// closer (for OS-opened files).
type nodeFile struct {
	csv    *csvReader
	name   string
	closer io.Closer
}

func (nf *nodeFile) close() {
	if nf.closer != nil {
		_ = nf.closer.Close()
	}
}

// pass1ScanNodes implements pass 1: scan every node file, parse each row's
// :ID and :LABEL fields, validate, and assign a dense id within the node's
// label group (doc 19 §4.1).
func (l *Loader) pass1ScanNodes() error {
	for si, src := range l.opts.Nodes {
		if err := l.pass1Source(si, src); err != nil {
			return err
		}
	}
	return nil
}

// pass1Source runs pass 1 over a single NodeSource.
func (l *Loader) pass1Source(srcIdx int, src NodeSource) error {
	// Resolve the header and the data files.
	hdr, files, err := l.resolveNodeSource(srcIdx, src)
	if err != nil {
		return err
	}
	defer func() {
		for _, f := range files {
			f.close()
		}
	}()

	// Register property keys with the catalog.
	for _, cd := range hdr.Cols {
		if cd.Role == RoleProperty && cd.Name != "" {
			l.catalog.PropKeyToken(cd.Name)
		}
	}
	if src.Label != "" {
		l.catalog.LabelToken(src.Label)
	}

	arrDelim := l.opts.arrayDelim()
	for _, nf := range files {
		if err := l.pass1File(nf.csv, nf.name, hdr, src, arrDelim); err != nil {
			return err
		}
	}
	return nil
}

// pass1File processes one data file (already open as a csvReader) during pass 1.
func (l *Loader) pass1File(
	csv *csvReader, fileName string,
	hdr *NodeHeader, src NodeSource,
	arrDelim rune,
) error {
	for {
		ok, err := csv.Next()
		if err != nil {
			return fmt.Errorf("loader: %s line %d: %w", fileName, csv.LineNo(), err)
		}
		if !ok {
			break
		}
		fields := csv.Fields()
		if len(fields) != len(hdr.Cols) {
			detail := fmt.Sprintf("expected %d fields, got %d", len(hdr.Cols), len(fields))
			l.rejectNode(fileName, csv.LineNo(), CodeParse, detail, joinFields(fields))
			if l.opts.OnMissingID == Fail {
				return fmt.Errorf("loader: %s line %d: %s", fileName, csv.LineNo(), detail)
			}
			continue
		}

		// Extract the external id.
		extID := fields[hdr.IDCol]
		if extID == "" {
			l.rejectNode(fileName, csv.LineNo(), CodeMissingID, "empty :ID field", joinFields(fields))
			if l.opts.OnMissingID == Fail {
				return errMissingID(csv.LineNo())
			}
			continue
		}

		// Determine the id space: from the header :ID(space) or the source IDSpace.
		space := hdr.Cols[hdr.IDCol].IDSpace
		if space == "" {
			space = src.IDSpace
		}

		// Check for duplicate.
		if l.idmap.Has(space, extID) {
			l.rejectNode(fileName, csv.LineNo(), CodeDupID,
				fmt.Sprintf("id %q already seen in space %q", extID, space),
				joinFields(fields))
			l.stats.DupNodes++
			if l.opts.OnDuplicateID == Fail {
				return errDuplicateID(space, extID, csv.LineNo())
			}
			continue
		}

		// Collect labels and assign a dense id.
		labels := l.labelsFor(fields, hdr, src.Label, arrDelim)
		group := l.catalog.GroupFor(labels)
		dense := l.catalog.NextDenseID(group)

		l.idmap.Put(space, extID, IDMapEntry{Group: group, DenseID: dense})
		l.stats.Nodes++
	}
	return nil
}

// labelsFor returns the combined label list for a node row.
func (l *Loader) labelsFor(fields []string, hdr *NodeHeader, prefix string, arrDelim rune) []string {
	var labels []string
	if prefix != "" {
		labels = append(labels, prefix)
	}
	if hdr.LblCol >= 0 && hdr.LblCol < len(fields) {
		raw := fields[hdr.LblCol]
		for _, lbl := range splitArrayField(raw, arrDelim) {
			lbl = strings.TrimSpace(lbl)
			if lbl == "" {
				continue
			}
			lbl = strings.TrimPrefix(lbl, ":")
			if lbl != "" && lbl != prefix {
				labels = append(labels, lbl)
			}
		}
	}
	for _, lbl := range labels {
		l.catalog.LabelToken(lbl)
	}
	return labels
}

// rejectNode writes one rejection to the bad-line sink and increments counters.
func (l *Loader) rejectNode(file string, lineno int, code RejectCode, detail, rawRow string) {
	l.bad.Reject(Reject{
		SourceFile: file,
		LineNo:     lineno,
		Code:       code,
		Detail:     detail,
		RawRow:     rawRow,
	})
	l.stats.BadLines++
}

// resolveNodeSource opens a NodeSource and returns the parsed header and a list
// of nodeFile values, each positioned at the first data row.
//
// For the streaming API (src.readers set), the first reader is consumed for the
// header (when src.Header is empty) and the rest are data readers; when there
// is a single reader it contains both header and data, so we return it as a
// data reader after consuming its header line.
//
// For the file-path API (src.Files set), we open files by path. When there is
// only one file, we reopen it and skip the header line.
func (l *Loader) resolveNodeSource(srcIdx int, src NodeSource) (*NodeHeader, []nodeFile, error) {
	delim := l.opts.delimiter()
	arrDelim := l.opts.arrayDelim()

	var hdrFields []string
	var dataFiles []nodeFile
	var err error

	if len(src.readers) > 0 {
		// Streaming API.
		if src.Header != "" {
			hdrFields, err = parseHeaderLine(src.Header, delim)
			if err != nil {
				return nil, nil, err
			}
			for i, r := range src.readers {
				dataFiles = append(dataFiles, nodeFile{
					csv:  newCSVReader(r, delim, arrDelim),
					name: fmt.Sprintf("<reader %d>", i+1),
				})
			}
		} else {
			// Read header from first reader; use its tail plus remaining readers as data.
			r0csv := newCSVReader(src.readers[0], delim, arrDelim)
			hdrFields, err = nextCSVRecord(r0csv, "<reader 1>")
			if err != nil {
				return nil, nil, err
			}
			// r0csv is now positioned at the first data row — use it as a data file.
			dataFiles = append(dataFiles, nodeFile{csv: r0csv, name: "<reader 1>"})
			for i, r := range src.readers[1:] {
				dataFiles = append(dataFiles, nodeFile{
					csv:  newCSVReader(r, delim, arrDelim),
					name: fmt.Sprintf("<reader %d>", i+2),
				})
			}
		}
	} else if len(src.Files) > 0 {
		// File-path API.
		if src.Header != "" {
			hdrFields, err = parseHeaderLine(src.Header, delim)
			if err != nil {
				return nil, nil, err
			}
			for _, path := range src.Files {
				f, ferr := os.Open(path)
				if ferr != nil {
					return nil, nil, ferr
				}
				dataFiles = append(dataFiles, nodeFile{
					csv:    newCSVReader(f, delim, arrDelim),
					name:   path,
					closer: f,
				})
			}
		} else {
			// First file contains the header; remaining files (or the same file) are data.
			f0, ferr := os.Open(src.Files[0])
			if ferr != nil {
				return nil, nil, ferr
			}
			r0csv := newCSVReader(f0, delim, arrDelim)
			hdrFields, err = nextCSVRecord(r0csv, src.Files[0])
			if ferr != nil {
				_ = f0.Close()
				return nil, nil, err
			}
			if len(src.Files) == 1 {
				// Same file: r0csv is already past the header; use it as data.
				dataFiles = append(dataFiles, nodeFile{csv: r0csv, name: src.Files[0], closer: f0})
			} else {
				// Separate data files; close the header file.
				_ = f0.Close()
				for _, path := range src.Files[1:] {
					f, ferr2 := os.Open(path)
					if ferr2 != nil {
						return nil, nil, ferr2
					}
					dataFiles = append(dataFiles, nodeFile{
						csv:    newCSVReader(f, delim, arrDelim),
						name:   path,
						closer: f,
					})
				}
			}
		}
	} else {
		return nil, nil, fmt.Errorf("loader: node source %d has no files or readers", srcIdx)
	}

	hdr, err := parseNodeHeader(hdrFields, src.Label)
	if err != nil {
		return nil, nil, fmt.Errorf("loader: node source %d header: %w", srcIdx, err)
	}
	return hdr, dataFiles, nil
}

// nextCSVRecord reads one record from csv and returns the fields, or an error.
func nextCSVRecord(csv *csvReader, name string) ([]string, error) {
	ok, err := csv.Next()
	if err != nil {
		return nil, fmt.Errorf("loader: %s: %w", name, err)
	}
	if !ok {
		return nil, fmt.Errorf("loader: %s: empty file", name)
	}
	fields := make([]string, len(csv.Fields()))
	copy(fields, csv.Fields())
	return fields, nil
}

// parseHeaderLine parses a raw header string into fields.
func parseHeaderLine(line string, delim rune) ([]string, error) {
	r := newCSVReader(strings.NewReader(line+"\n"), delim, 0)
	ok, err := r.Next()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("loader: empty header line")
	}
	fields := make([]string, len(r.Fields()))
	copy(fields, r.Fields())
	return fields, nil
}
