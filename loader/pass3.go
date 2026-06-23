package loader

import (
	"fmt"

	"github.com/tamnd/gr/vfs"
)

// pass3BuildCSR builds the forward and backward CSR for every relationship type
// by counting-sort (doc 19 §4.3). The four sub-steps run in sequence:
//
//  1. Count: scan all relationship files, tally out/in-degrees per node.
//  2. Prefix-sum: turn degree arrays into offset arrays; allocate neighbor/edge arrays.
//  3. Scatter: scan relationship files again, fill pre-counted slots.
//  4. Sort: sort each node's neighbor run by neighbor dense id.
//
// After this call, fb.relCSR holds the final CSR for each (relType, direction).
// The per-relationship dense edge ids are assigned in scatter order (= input file
// order for a serial load), stored in fb.relCSR[key].Edges(), and used by pass 4
// to address relationship property columns.
func (l *Loader) pass3BuildCSR(fb *fileBuilder) error {
	// Sub-step 1: count out/in-degrees for each (source, type) and (dest, type).
	if err := l.pass3Count(fb); err != nil {
		return err
	}

	// Sub-step 2: prefix-sum degree arrays into offset arrays.
	l.pass3PrefixSum(fb)

	// Sub-step 3: scatter neighbors and assign edge ids.
	if err := l.pass3Scatter(fb); err != nil {
		return err
	}

	// Sub-step 4: sort each node's neighbor run by neighbor dense id.
	for _, b := range fb.relCSR {
		b.SortWithinRuns()
	}
	return nil
}

// pass3Count scans all relationship sources once and increments per-node
// out/in-degree counters for each (type, direction) pair.
func (l *Loader) pass3Count(fb *fileBuilder) error {
	for si, src := range l.opts.Relationships {
		if err := l.pass3CountSource(si, src, fb); err != nil {
			return err
		}
	}
	return nil
}

// pass3CountSource counts degrees for one RelSource.
func (l *Loader) pass3CountSource(srcIdx int, src RelSource, fb *fileBuilder) error {
	hdr, files, err := l.resolveRelSource(srcIdx, src)
	if err != nil {
		return err
	}
	defer func() {
		for _, f := range files {
			f.close()
		}
	}()

	// Cache header for streaming sources (scatter pass re-uses it).
	l.relHdrBuf[srcIdx] = hdr

	arrDelim := l.opts.arrayDelim()
	for _, nf := range files {
		if err := l.pass3CountFile(nf.csv, nf.name, hdr, src, fb, arrDelim, true); err != nil {
			return err
		}
	}
	return nil
}

// pass3CountFile processes one file during the count sub-step.
// When buffer is true, accepted rows are appended to l.relRowBuf[...].
func (l *Loader) pass3CountFile(
	csv *csvReader, fileName string,
	hdr *RelHeader, src RelSource,
	fb *fileBuilder,
	arrDelim rune,
	doBuffer bool,
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
			continue // width mismatch; reject
		}

		startExtID := fields[hdr.StartCol]
		endExtID := fields[hdr.EndCol]

		// Resolve endpoints.
		startSpace := hdr.Cols[hdr.StartCol].IDSpace
		endSpace := hdr.Cols[hdr.EndCol].IDSpace

		su, okS := l.idmap.Get(startSpace, startExtID)
		ev, okE := l.idmap.Get(endSpace, endExtID)
		if !okS || !okE {
			l.stats.DanglingRels++
			l.stats.BadLines++
			if l.opts.OnDangling == Fail {
				return fmt.Errorf("loader: %s line %d: dangling relationship endpoint", fileName, csv.LineNo())
			}
			continue
		}

		// Determine the relationship type.
		relType := l.relTypeFor(fields, hdr, src)
		if relType < 0 {
			continue // empty type; skip
		}
		rt := uint32(relType)

		fb.ensureCSR(rt, csrFwd).Count(fb.GlobalPos(su.Group, su.DenseID))
		fb.ensureCSR(rt, csrBwd).Count(fb.GlobalPos(ev.Group, ev.DenseID))

		l.stats.Rels++

		if doBuffer {
			// For streaming sources: buffer accepted rows (same pattern as pass 1).
			l.bufRelRow(0, rowRecord{
				fields: copyFields(fields),
				lineno: csv.LineNo(),
				name:   fileName,
			})
		}
	}
	return nil
}

// pass3PrefixSum converts all degree arrays to offset arrays and allocates
// the neighbor and edge-id arrays.
func (l *Loader) pass3PrefixSum(fb *fileBuilder) {
	for _, b := range fb.relCSR {
		b.PrefixSum()
	}
}

// pass3Scatter scans relationship files again and fills pre-counted slots.
func (l *Loader) pass3Scatter(fb *fileBuilder) error {
	// Reset Rels counter; it was incremented in count, re-counted in scatter.
	l.stats.Rels = 0
	// Reset edge id counters for scatter-order assignment.
	for k := range fb.edgeCnt {
		delete(fb.edgeCnt, k)
	}

	for si, src := range l.opts.Relationships {
		if err := l.pass3ScatterSource(si, src, fb); err != nil {
			return err
		}
	}
	return nil
}

// pass3ScatterSource scatters neighbors for one RelSource.
func (l *Loader) pass3ScatterSource(srcIdx int, src RelSource, fb *fileBuilder) error {
	if len(src.readers) > 0 {
		// Streaming source: replay from the row buffer.
		return l.pass3ScatterBuffered(srcIdx, src, fb)
	}

	hdr := l.relHdrBuf[srcIdx]
	if hdr == nil {
		// Shouldn't happen, but guard anyway.
		return fmt.Errorf("loader: rel source %d has no cached header", srcIdx)
	}
	_, files, err := l.resolveRelSource(srcIdx, src)
	if err != nil {
		return err
	}
	defer func() {
		for _, f := range files {
			f.close()
		}
	}()

	for _, nf := range files {
		if err := l.pass3ScatterFile(nf.csv, nf.name, hdr, src, fb); err != nil {
			return err
		}
	}
	return nil
}

// pass3ScatterBuffered replays buffered rows for a streaming source.
func (l *Loader) pass3ScatterBuffered(srcIdx int, src RelSource, fb *fileBuilder) error {
	hdr := l.relHdrBuf[srcIdx]
	for _, rec := range l.relRowBuf {
		if err := l.pass3ScatterRow(rec.fields, rec.name, rec.lineno, hdr, src, fb); err != nil {
			return err
		}
	}
	return nil
}

// pass3ScatterFile scatters one file's rows.
func (l *Loader) pass3ScatterFile(
	csv *csvReader, fileName string,
	hdr *RelHeader, src RelSource,
	fb *fileBuilder,
) error {
	for {
		ok, err := csv.Next()
		if err != nil {
			return fmt.Errorf("loader: %s line %d: %w", fileName, csv.LineNo(), err)
		}
		if !ok {
			break
		}
		if err := l.pass3ScatterRow(csv.Fields(), fileName, csv.LineNo(), hdr, src, fb); err != nil {
			return err
		}
	}
	return nil
}

// pass3ScatterRow places one edge into the pre-counted CSR slots.
func (l *Loader) pass3ScatterRow(
	fields []string, fileName string, lineno int,
	hdr *RelHeader, src RelSource,
	fb *fileBuilder,
) error {
	if len(fields) != len(hdr.Cols) {
		return nil
	}
	startExtID := fields[hdr.StartCol]
	endExtID := fields[hdr.EndCol]
	startSpace := hdr.Cols[hdr.StartCol].IDSpace
	endSpace := hdr.Cols[hdr.EndCol].IDSpace

	su, okS := l.idmap.Get(startSpace, startExtID)
	ev, okE := l.idmap.Get(endSpace, endExtID)
	if !okS || !okE {
		return nil // dangling; already counted
	}

	relType := l.relTypeFor(fields, hdr, src)
	if relType < 0 {
		return nil
	}
	rt := uint32(relType)

	eid := fb.nextEdgeID(rt)

	srcGPos := fb.GlobalPos(su.Group, su.DenseID)
	dstGPos := fb.GlobalPos(ev.Group, ev.DenseID)

	fwd := fb.relCSR[csrKey{rt, csrFwd}]
	bwd := fb.relCSR[csrKey{rt, csrBwd}]
	if fwd != nil {
		fwd.Scatter(srcGPos, dstGPos, eid)
	}
	if bwd != nil {
		bwd.Scatter(dstGPos, srcGPos, eid)
	}
	l.stats.Rels++
	return nil
}

// relTypeFor returns the catalog token for the relationship type of this row.
// Returns -1 when no type can be determined (empty type, no prefix, no :TYPE col).
func (l *Loader) relTypeFor(fields []string, hdr *RelHeader, src RelSource) int {
	typeStr := src.Type
	if hdr.TypeCol >= 0 && hdr.TypeCol < len(fields) {
		col := fields[hdr.TypeCol]
		if col != "" {
			typeStr = col
		}
	}
	if typeStr == "" {
		return -1
	}
	return l.catalog.RelTypeToken(typeStr)
}

// Pass3BuildCSR is exported for testing.
func (l *Loader) Pass3BuildCSR(fb *fileBuilder) error {
	return l.pass3BuildCSR(fb)
}

// Pass3BuildCSRFull runs pass 1+2+3 (or just pass 1+3 if called with an
// existing fb) — convenience for tests that want to check the CSR directly
// without going through Pass2.
func (l *Loader) Pass3BuildCSRFull(fsys vfs.VFS, outputPath string) (*fileBuilder, error) {
	if err := l.Pass1ScanNodes(); err != nil {
		return nil, err
	}
	fb, err := l.Pass2BuildNodeColumns(fsys, outputPath)
	if err != nil {
		return nil, err
	}
	if err := l.Pass3BuildCSR(fb); err != nil {
		_ = fb.Close()
		return nil, err
	}
	return fb, nil
}

// --- streaming API row buffer for relationship sources ---

// relRowBuf holds accepted rows for all streaming relationship sources combined.
// Unlike node sources (where each source has its own buffer at rowBuf[srcIdx]),
// relationship sources across the whole load share a single ordered list so the
// scatter pass replays them in the same order the count pass saw them.
// (Multi-source ordering is deterministic because sources are processed in
// Options.Relationships slice order, and within each source in file/line order.)

// bufRelRow appends a relationship row to the combined buffer.
// srcIdx is not used yet but is reserved for future per-source buffering.
func (l *Loader) bufRelRow(_ int, rec rowRecord) {
	l.relRowBuf = append(l.relRowBuf, rec)
}

// copyFields deep-copies a field slice so the csvReader can overwrite its
// internal buffer on the next Next() call.
func copyFields(src []string) []string {
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}
