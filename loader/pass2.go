package loader

import (
	"fmt"

	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/vfs"
)

// pass2BuildNodeColumns scans each node file a second time. For every row it
// looks up the node's (group, denseID) from the id-map (built in pass 1) and
// appends each property's value to that group's column builders (doc 19 §4.2).
//
// Rows that were rejected in pass 1 (missing id, dup id) are silently skipped
// here because they have no entry in the id-map. Gaps in the dense-id space
// (from skipped rows) appear as absent cells, which is correct.
//
// After scanning all files the method flushes the final partial segment of each
// column builder.
func (l *Loader) pass2BuildNodeColumns(fb *fileBuilder) error {
	arrDelim := l.opts.arrayDelim()

	for si, src := range l.opts.Nodes {
		if err := l.pass2Source(si, src, fb, arrDelim); err != nil {
			return err
		}
	}
	// Flush all remaining partial segments.
	for g, builders := range fb.groupBuilders {
		for _, b := range builders {
			if err := b.Flush(); err != nil {
				return fmt.Errorf("loader: flush group %d column: %w", g, err)
			}
		}
	}
	return nil
}

// pass2Source runs pass 2 over a single NodeSource.
func (l *Loader) pass2Source(srcIdx int, src NodeSource, fb *fileBuilder, arrDelim rune) error {
	// Streaming sources (readers set) are exhausted after pass 1. Use the
	// buffered rows and the cached header instead of re-opening.
	if len(src.readers) > 0 {
		hdr := l.hdrBuf[srcIdx]
		propCols := l.propColDescs(hdr)
		return l.pass2Buffered(srcIdx, hdr, src, propCols, fb, arrDelim)
	}

	hdr, files, err := l.resolveNodeSource(srcIdx, src)
	if err != nil {
		return err
	}
	defer func() {
		for _, f := range files {
			f.close()
		}
	}()

	propCols := l.propColDescs(hdr)
	for _, nf := range files {
		if err := l.pass2File(nf.csv, nf.name, hdr, src, propCols, fb, arrDelim); err != nil {
			return err
		}
	}
	return nil
}

// pass2Buffered replays the rows buffered during pass 1 for a streaming source.
func (l *Loader) pass2Buffered(
	srcIdx int,
	hdr *NodeHeader, src NodeSource,
	propCols []propColDesc,
	fb *fileBuilder,
	arrDelim rune,
) error {
	buf := l.rowBuf[srcIdx]
	for _, rec := range buf {
		fields := rec.fields
		if len(fields) != len(hdr.Cols) {
			continue
		}
		extID := fields[hdr.IDCol]
		if extID == "" {
			continue
		}
		space := hdr.Cols[hdr.IDCol].IDSpace
		if space == "" {
			space = src.IDSpace
		}
		entry, ok := l.idmap.Get(space, extID)
		if !ok {
			continue
		}
		builders := fb.ensureBuilders(entry.Group, propCols)
		for i, pc := range propCols {
			raw := fields[pc.colIdx]
			val, present, _ := parseCSVField(raw, pc.pt, pc.isList, arrDelim)
			cell := colseg.Cell{Present: present, Value: val}
			if berr := builders[i].Append(entry.DenseID, cell); berr != nil {
				return fmt.Errorf("loader: buffered src %d pos %d col %d: %w", srcIdx, entry.DenseID, i, berr)
			}
		}
	}
	return nil
}

// pass2File processes one data file during pass 2.
func (l *Loader) pass2File(
	csv *csvReader, fileName string,
	hdr *NodeHeader, src NodeSource,
	propCols []propColDesc,
	fb *fileBuilder,
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
			// Row width mismatch — already rejected in pass 1; skip.
			continue
		}

		// Look up the id-map entry to find the group and dense position.
		extID := fields[hdr.IDCol]
		if extID == "" {
			continue
		}
		space := hdr.Cols[hdr.IDCol].IDSpace
		if space == "" {
			space = src.IDSpace
		}
		entry, ok := l.idmap.Get(space, extID)
		if !ok {
			// Row was rejected in pass 1.
			continue
		}

		g := entry.Group
		pos := entry.DenseID

		// Get or create the column builders for this group.
		builders := fb.ensureBuilders(g, propCols)

		// Append each property value to its column builder.
		for i, pc := range propCols {
			raw := fields[pc.colIdx]
			val, present, perr := parseCSVField(raw, pc.pt, pc.isList, arrDelim)
			if perr != nil {
				// Bad value: write absent and continue (already counted in pass 1).
				if berr := builders[i].Append(pos, colseg.Cell{Present: false}); berr != nil {
					return fmt.Errorf("loader: %s: group %d col %d: %w", fileName, g, i, berr)
				}
				continue
			}
			cell := colseg.Cell{Present: present, Value: val}
			if berr := builders[i].Append(pos, cell); berr != nil {
				return fmt.Errorf("loader: %s: group %d col %d: %w", fileName, g, i, berr)
			}
		}
	}
	return nil
}

// propColDescs builds the list of property column descriptors for a header.
// Only columns with Role == RoleProperty and a non-empty name are included.
func (l *Loader) propColDescs(hdr *NodeHeader) []propColDesc {
	var out []propColDesc
	for i, cd := range hdr.Cols {
		if cd.Role != RoleProperty || cd.Name == "" {
			continue
		}
		kt := l.catalog.PropKeyToken(cd.Name)
		vt := propTypeToValueType(cd.PropType)
		if cd.IsList {
			vt = propTypeToValueType(cd.PropType) // element type; the List wrapper is at runtime
		}
		out = append(out, propColDesc{
			colIdx:   i,
			keyToken: kt,
			vtype:    vt,
			pt:       cd.PropType,
			isList:   cd.IsList,
		})
	}
	return out
}

// Pass2BuildNodeColumns is exported for testing. In production use, Run calls
// it in sequence after Pass1ScanNodes.
func (l *Loader) Pass2BuildNodeColumns(fsys vfs.VFS, outputPath string) (*fileBuilder, error) {
	fb, err := openFileBuilder(fsys, outputPath, l.catalog.Groups())
	if err != nil {
		return nil, err
	}
	fb.groupBuilders = make([][](*colBuilder), l.catalog.Groups())
	if err := l.pass2BuildNodeColumns(fb); err != nil {
		_ = fb.Close()
		return nil, err
	}
	return fb, nil
}
