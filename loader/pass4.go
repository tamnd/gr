package loader

import (
	"fmt"

	"github.com/tamnd/gr/adj"
	"github.com/tamnd/gr/catalog"
	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/column"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/idmap"
	"github.com/tamnd/gr/node"
	"github.com/tamnd/gr/rel"
	"github.com/tamnd/gr/stats"
	"github.com/tamnd/gr/store"
	"github.com/tamnd/gr/value"
	"github.com/tamnd/gr/vfs"
)

// pass4Finalize writes all remaining on-disk structures after passes 1-3:
// the section directory, catalog, id-map, node records, rel property columns,
// rel records, the CSR adjacency, empty naive column stores, statistics, and
// a final pager commit.
func (l *Loader) pass4Finalize(fb *fileBuilder) error {
	p := fb.p

	secs, err := store.CreateSections(p)
	if err != nil {
		return fmt.Errorf("loader: create sections: %w", err)
	}

	cat, err := catalog.Create(p, secs)
	if err != nil {
		return fmt.Errorf("loader: create catalog: %w", err)
	}
	if err := l.pass4InternTokens(cat); err != nil {
		return err
	}

	im, err := idmap.Create(p, secs)
	if err != nil {
		return fmt.Errorf("loader: create idmap: %w", err)
	}

	ns, err := node.Create(p, secs)
	if err != nil {
		return fmt.Errorf("loader: create node store: %w", err)
	}

	rs, err := rel.Create(p, secs)
	if err != nil {
		return fmt.Errorf("loader: create rel store: %w", err)
	}

	st, err := stats.Create(p, secs)
	if err != nil {
		return fmt.Errorf("loader: create stats: %w", err)
	}

	if err := l.pass4NodeRecords(fb, ns, im, cat, st); err != nil {
		return err
	}

	// Record the node segmented-column store that was built in pass 2.
	if err := secs.Set(store.SecNodeColSeg, fb.nodeStore.DirHead(), uint64(fb.nodeStore.DirCount())); err != nil {
		return fmt.Errorf("loader: record node colseg section: %w", err)
	}

	relColStore, err := colsegstore.CreateStore(p)
	if err != nil {
		return fmt.Errorf("loader: create rel colseg store: %w", err)
	}
	if err := l.pass4RelColumns(fb, cat, relColStore); err != nil {
		return err
	}
	if err := secs.Set(store.SecRelColSeg, relColStore.DirHead(), uint64(relColStore.DirCount())); err != nil {
		return fmt.Errorf("loader: record rel colseg section: %w", err)
	}

	if err := l.pass4RelRecords(fb, rs, im, cat, st); err != nil {
		return err
	}

	if err := l.pass4WriteCSR(fb, secs); err != nil {
		return err
	}

	// Create empty naive column stores so the engine can open the file.
	if _, err := column.Create(p, secs, store.SecNodeCols); err != nil {
		return fmt.Errorf("loader: create node column delta: %w", err)
	}
	if _, err := column.Create(p, secs, store.SecRelCols); err != nil {
		return fmt.Errorf("loader: create rel column delta: %w", err)
	}

	if err := p.Commit(); err != nil {
		return fmt.Errorf("loader: commit: %w", err)
	}
	return nil
}

// pass4InternTokens interns all tokens from the loader's catalog builder into
// the engine catalog in the same insertion order so token values are identical.
func (l *Loader) pass4InternTokens(cat *catalog.Catalog) error {
	for _, name := range l.catalog.LabelNames() {
		if _, _, err := cat.Intern(catalog.KindLabel, name); err != nil {
			return fmt.Errorf("loader: intern label %q: %w", name, err)
		}
	}
	for _, name := range l.catalog.RelTypeNames() {
		if _, _, err := cat.Intern(catalog.KindRelType, name); err != nil {
			return fmt.Errorf("loader: intern rel type %q: %w", name, err)
		}
	}
	for _, name := range l.catalog.PropKeyNames() {
		if _, _, err := cat.Intern(catalog.KindPropKey, name); err != nil {
			return fmt.Errorf("loader: intern prop key %q: %w", name, err)
		}
	}
	return nil
}

// pass4NodeRecords scans node files once more (or replays buffered rows) to
// build the per-node label sets, then writes node records and id-map entries in
// global position order. Global position = groupBase[g] + localDenseID, so
// records are written in ascending global position if sources are processed in
// group-index order.
//
// For simplicity, this implementation writes each node with its full label set
// derived from the source's prefix label and its :LABEL column field.
func (l *Loader) pass4NodeRecords(
	fb *fileBuilder,
	ns *node.Store,
	im *idmap.Map,
	cat *catalog.Catalog,
	st *stats.Stats,
) error {
	// labelBuf[globalPos] = sorted catalog label tokens for that node.
	// We accumulate them in a single pass ordered by globalPos.
	labelBuf := make([][]uint32, fb.totalNodes)

	arrDelim := l.opts.arrayDelim()
	for si, src := range l.opts.Nodes {
		if err := l.pass4CollectLabels(si, src, fb, cat, arrDelim, labelBuf); err != nil {
			return err
		}
	}

	// Write node records in globalPos 0..totalNodes-1 order.
	for gpos := uint64(0); gpos < fb.totalNodes; gpos++ {
		labels := labelBuf[gpos]
		pos, err := ns.Create(labels)
		if err != nil {
			return fmt.Errorf("loader: create node record at %d: %w", gpos, err)
		}
		if pos != gpos {
			return fmt.Errorf("loader: node record pos mismatch: got %d, want %d", pos, gpos)
		}
		_, _, err = im.Alloc(idmap.KindNode)
		if err != nil {
			return fmt.Errorf("loader: alloc node element id at %d: %w", gpos, err)
		}
		for _, tok := range labels {
			if err := st.AddLabel(tok, +1); err != nil {
				return fmt.Errorf("loader: stats.AddLabel %d: %w", tok, err)
			}
		}
	}
	return nil
}

// pass4CollectLabels reads one NodeSource and fills labelBuf at the global
// positions of each accepted node.
func (l *Loader) pass4CollectLabels(
	srcIdx int,
	src NodeSource,
	fb *fileBuilder,
	cat *catalog.Catalog,
	arrDelim rune,
	labelBuf [][]uint32,
) error {
	if len(src.readers) > 0 {
		return l.pass4CollectLabelsBuffered(srcIdx, src, fb, cat, arrDelim, labelBuf)
	}
	return l.pass4CollectLabelsFiles(srcIdx, src, fb, cat, arrDelim, labelBuf)
}

func (l *Loader) pass4CollectLabelsBuffered(
	srcIdx int,
	src NodeSource,
	fb *fileBuilder,
	cat *catalog.Catalog,
	arrDelim rune,
	labelBuf [][]uint32,
) error {
	hdr := l.hdrBuf[srcIdx]
	for _, rec := range l.rowBuf[srcIdx] {
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
		labelNames := l.labelsFor(fields, hdr, src.Label, arrDelim)
		gpos := fb.GlobalPos(entry.Group, entry.DenseID)
		labelBuf[gpos] = labelTokens(cat, labelNames)
	}
	return nil
}

func (l *Loader) pass4CollectLabelsFiles(
	srcIdx int,
	src NodeSource,
	fb *fileBuilder,
	cat *catalog.Catalog,
	arrDelim rune,
	labelBuf [][]uint32,
) error {
	hdr, files, err := l.resolveNodeSource(srcIdx, src)
	if err != nil {
		return err
	}
	defer func() {
		for _, f := range files {
			f.close()
		}
	}()
	for _, nf := range files {
		if err := l.pass4CollectLabelsFile(nf.csv, nf.name, hdr, src, fb, cat, arrDelim, labelBuf); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loader) pass4CollectLabelsFile(
	csv *csvReader, fileName string,
	hdr *NodeHeader, src NodeSource,
	fb *fileBuilder,
	cat *catalog.Catalog,
	arrDelim rune,
	labelBuf [][]uint32,
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
		labelNames := l.labelsFor(fields, hdr, src.Label, arrDelim)
		gpos := fb.GlobalPos(entry.Group, entry.DenseID)
		labelBuf[gpos] = labelTokens(cat, labelNames)
	}
	return nil
}

// labelTokens looks up catalog token values for the given label names and
// returns them as a sorted []uint32 for the node record store.
func labelTokens(cat *catalog.Catalog, names []string) []uint32 {
	out := make([]uint32, 0, len(names))
	for _, n := range names {
		tok, ok := cat.Lookup(catalog.KindLabel, n)
		if ok {
			out = append(out, tok)
		}
	}
	// The node store expects labels sorted by token value.
	sortU32(out)
	return out
}

func sortU32(a []uint32) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// relPropColDesc describes one relationship property column for the rel colseg store.
type relPropColDesc struct {
	colIdx   int
	keyToken uint32
	vtype    value.Type
	pt       PropType
	isList   bool
}

// pass4RelColumns scans relationship files once more and writes relationship
// property columns into relColStore. Uses the same buffered-replay strategy as
// pass 2 for streaming sources.
func (l *Loader) pass4RelColumns(
	fb *fileBuilder,
	cat *catalog.Catalog,
	relColStore *colsegstore.Store,
) error {
	// One colBuilder per unique property key (shared across rel types).
	relKeyBuilders := make(map[uint32]*colBuilder)

	for si, src := range l.opts.Relationships {
		hdr := l.relHdrBuf[si]
		if hdr == nil {
			continue
		}
		propCols := l.relPropColDescs(hdr, cat)
		if len(propCols) == 0 {
			continue
		}

		if len(src.readers) > 0 {
			// Streaming: already buffered in relRowBuf — handled in the combined
			// replay loop below where we need edge-id order anyway. Skip here.
			continue
		}

		// File source: re-open and scan.
		_, files, err := l.resolveRelSource(si, src)
		if err != nil {
			return err
		}
		defer func() {
			for _, f := range files {
				f.close()
			}
		}()
		for _, nf := range files {
			if err := l.pass4RelColumnsFile(nf.csv, nf.name, hdr, src, fb, propCols, relColStore, relKeyBuilders); err != nil {
				return err
			}
		}
	}

	// Replay buffered rows from streaming rel sources. All streaming rel sources
	// share l.relRowBuf in edge-id order; we match rows to their source by
	// the per-source header from relHdrBuf.
	//
	// The relRowBuf mixes rows from multiple sources; we match by trying all
	// headers since field counts may differ. The name field in each record
	// tells us which source it came from, but matching by field-count is
	// sufficient for the property-column write (same headers, different data).
	//
	// For correctness: streaming row records carry their globalEdgeID implicitly
	// by position in relRowBuf. Here we track a per-type edge counter to
	// determine the edge's global position (same order as scatter pass).
	edgeCntRel := make(map[uint32]uint64)
	arrDelim := l.opts.arrayDelim()
	for _, rec := range l.relRowBuf {
		// Find the source and header for this row.
		var matchHdr *RelHeader
		var matchSrc RelSource
		var matchSrcIdx int
		for si, src := range l.opts.Relationships {
			if len(src.readers) == 0 {
				continue
			}
			hdr := l.relHdrBuf[si]
			if hdr == nil || len(rec.fields) != len(hdr.Cols) {
				continue
			}
			matchHdr = hdr
			matchSrc = src
			matchSrcIdx = si
			break
		}
		if matchHdr == nil {
			continue
		}
		propCols := l.relPropColDescs(matchHdr, cat)
		if len(propCols) == 0 {
			continue
		}

		// Determine edge position.
		startSpace := matchHdr.Cols[matchHdr.StartCol].IDSpace
		endSpace := matchHdr.Cols[matchHdr.EndCol].IDSpace
		su, okS := l.idmap.Get(startSpace, rec.fields[matchHdr.StartCol])
		ev, okE := l.idmap.Get(endSpace, rec.fields[matchHdr.EndCol])
		if !okS || !okE {
			continue
		}
		rt := uint32(l.relTypeFor(rec.fields, matchHdr, matchSrc))
		if int(rt) < 0 {
			continue
		}

		// Edge position = sequential counter per type.
		edgePos := edgeCntRel[rt]
		edgeCntRel[rt]++
		_ = edgePos // currently unused: rel property columns use edge position as key
		_ = su
		_ = ev
		_ = matchSrcIdx

		// Write property values for this edge at edgePos.
		builders := l.relKeyBuildersFor(propCols, relColStore, relKeyBuilders)
		for i, pc := range propCols {
			raw := rec.fields[pc.colIdx]
			val, present, _ := parseCSVField(raw, pc.pt, pc.isList, arrDelim)
			cell := colsegCell(present, val)
			if err := builders[i].Append(edgePos, cell); err != nil {
				return fmt.Errorf("loader: rel col edge %d key %d: %w", edgePos, pc.keyToken, err)
			}
		}
	}

	// Flush all rel column builders.
	for key, b := range relKeyBuilders {
		if err := b.Flush(); err != nil {
			return fmt.Errorf("loader: flush rel col key %d: %w", key, err)
		}
	}
	return nil
}

func (l *Loader) pass4RelColumnsFile(
	csv *csvReader, fileName string,
	hdr *RelHeader, src RelSource,
	fb *fileBuilder,
	propCols []relPropColDesc,
	relColStore *colsegstore.Store,
	relKeyBuilders map[uint32]*colBuilder,
) error {
	arrDelim := l.opts.arrayDelim()
	edgeCnt := make(map[uint32]uint64)
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
			continue
		}
		startSpace := hdr.Cols[hdr.StartCol].IDSpace
		endSpace := hdr.Cols[hdr.EndCol].IDSpace
		_, okS := l.idmap.Get(startSpace, fields[hdr.StartCol])
		_, okE := l.idmap.Get(endSpace, fields[hdr.EndCol])
		if !okS || !okE {
			continue
		}
		rt := uint32(l.relTypeFor(fields, hdr, src))
		if int(rt) < 0 {
			continue
		}
		edgePos := edgeCnt[rt]
		edgeCnt[rt]++

		builders := l.relKeyBuildersFor(propCols, relColStore, relKeyBuilders)
		for i, pc := range propCols {
			raw := fields[pc.colIdx]
			val, present, _ := parseCSVField(raw, pc.pt, pc.isList, arrDelim)
			cell := colsegCell(present, val)
			if err := builders[i].Append(edgePos, cell); err != nil {
				return fmt.Errorf("loader: rel col edge %d key %d: %w", edgePos, pc.keyToken, err)
			}
		}
	}
	return nil
}

func (l *Loader) relKeyBuildersFor(
	propCols []relPropColDesc,
	relColStore *colsegstore.Store,
	relKeyBuilders map[uint32]*colBuilder,
) []*colBuilder {
	bs := make([]*colBuilder, len(propCols))
	for i, pc := range propCols {
		b, ok := relKeyBuilders[pc.keyToken]
		if !ok {
			b = newColBuilder(relColStore, pc.keyToken, pc.vtype, 0)
			relKeyBuilders[pc.keyToken] = b
		}
		bs[i] = b
	}
	return bs
}

// relPropColDescs builds property column descriptors for a relationship header.
func (l *Loader) relPropColDescs(hdr *RelHeader, cat *catalog.Catalog) []relPropColDesc {
	var out []relPropColDesc
	for i, cd := range hdr.Cols {
		if cd.Role != RoleProperty || cd.Name == "" {
			continue
		}
		tok, ok := cat.Lookup(catalog.KindPropKey, cd.Name)
		if !ok {
			continue
		}
		vt := propTypeToValueType(cd.PropType)
		out = append(out, relPropColDesc{
			colIdx:   i,
			keyToken: tok,
			vtype:    vt,
			pt:       cd.PropType,
			isList:   cd.IsList,
		})
	}
	return out
}

// colsegCell builds a colseg.Cell from a present/value pair.
func colsegCell(present bool, val value.Value) colseg.Cell {
	return colseg.Cell{Present: present, Value: val}
}

// pass4RelRecords replays buffered rel rows and file-source rel rows (in
// edge-id order per type) to write rel records and id-map entries.
func (l *Loader) pass4RelRecords(
	fb *fileBuilder,
	rs *rel.Store,
	im *idmap.Map,
	cat *catalog.Catalog,
	st *stats.Stats,
) error {
	// Re-scan file sources in the same order as the scatter pass.
	for si, src := range l.opts.Relationships {
		if len(src.readers) > 0 {
			continue // streaming sources handled via relRowBuf below
		}
		hdr := l.relHdrBuf[si]
		if hdr == nil {
			continue
		}
		_, files, err := l.resolveRelSource(si, src)
		if err != nil {
			return err
		}
		defer func() {
			for _, f := range files {
				f.close()
			}
		}()
		for _, nf := range files {
			if err := l.pass4RelRecordsFile(nf.csv, nf.name, hdr, src, fb, rs, im, st); err != nil {
				return err
			}
		}
	}

	// Replay streaming source rows from relRowBuf.
	for _, rec := range l.relRowBuf {
		var matchHdr *RelHeader
		var matchSrc RelSource
		for si, src := range l.opts.Relationships {
			if len(src.readers) == 0 {
				continue
			}
			hdr := l.relHdrBuf[si]
			if hdr == nil || len(rec.fields) != len(hdr.Cols) {
				continue
			}
			matchHdr = hdr
			matchSrc = src
			break
		}
		if matchHdr == nil {
			continue
		}
		if err := l.pass4RelRecordsRow(rec.fields, rec.name, rec.lineno, matchHdr, matchSrc, fb, rs, im, st); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loader) pass4RelRecordsFile(
	csv *csvReader, fileName string,
	hdr *RelHeader, src RelSource,
	fb *fileBuilder,
	rs *rel.Store,
	im *idmap.Map,
	st *stats.Stats,
) error {
	for {
		ok, err := csv.Next()
		if err != nil {
			return fmt.Errorf("loader: %s line %d: %w", fileName, csv.LineNo(), err)
		}
		if !ok {
			break
		}
		if err := l.pass4RelRecordsRow(csv.Fields(), fileName, csv.LineNo(), hdr, src, fb, rs, im, st); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loader) pass4RelRecordsRow(
	fields []string, fileName string, lineno int,
	hdr *RelHeader, src RelSource,
	fb *fileBuilder,
	rs *rel.Store,
	im *idmap.Map,
	st *stats.Stats,
) error {
	if len(fields) != len(hdr.Cols) {
		return nil
	}
	startSpace := hdr.Cols[hdr.StartCol].IDSpace
	endSpace := hdr.Cols[hdr.EndCol].IDSpace
	su, okS := l.idmap.Get(startSpace, fields[hdr.StartCol])
	ev, okE := l.idmap.Get(endSpace, fields[hdr.EndCol])
	if !okS || !okE {
		return nil
	}
	relType := l.relTypeFor(fields, hdr, src)
	if relType < 0 {
		return nil
	}
	rt := uint32(relType)
	srcGPos := fb.GlobalPos(su.Group, su.DenseID)
	dstGPos := fb.GlobalPos(ev.Group, ev.DenseID)

	if _, err := rs.Create(rt, srcGPos, dstGPos); err != nil {
		return fmt.Errorf("loader: %s line %d create rel: %w", fileName, lineno, err)
	}
	if _, _, err := im.Alloc(idmap.KindRel); err != nil {
		return fmt.Errorf("loader: %s line %d alloc rel eid: %w", fileName, lineno, err)
	}
	return st.AddRelType(rt, +1)
}

// pass4WriteCSR transfers the in-memory CSR arrays (built in pass 3) into the
// adjacency store via adj.BuildAdj + WriteSlot.
func (l *Loader) pass4WriteCSR(fb *fileBuilder, secs *store.Sections) error {
	a, err := adj.BuildAdj(fb.p, secs)
	if err != nil {
		return fmt.Errorf("loader: build adj: %w", err)
	}
	for key, b := range fb.relCSR {
		d := adj.Dir(key.dir) // csrFwd=0 ↔ adj.Out=0; csrBwd=1 ↔ adj.In=1
		if err := a.WriteSlot(key.relType, d, b.Offsets(), b.Neighbors(), b.Edges()); err != nil {
			return fmt.Errorf("loader: write CSR slot relType=%d dir=%d: %w", key.relType, key.dir, err)
		}
	}
	return nil
}

// Pass4Finalize is exported for testing.
func (l *Loader) Pass4Finalize(fb *fileBuilder) error {
	return l.pass4Finalize(fb)
}

// Pass4FinalizeAll runs all four passes and finalizes the output file.
func (l *Loader) Pass4FinalizeAll(fsys vfs.VFS, outputPath string) error {
	if err := l.Pass1ScanNodes(); err != nil {
		return err
	}
	fb, err := l.Pass2BuildNodeColumns(fsys, outputPath)
	if err != nil {
		return err
	}
	defer fb.Close()
	if err := l.Pass3BuildCSR(fb); err != nil {
		return err
	}
	return l.pass4Finalize(fb)
}
