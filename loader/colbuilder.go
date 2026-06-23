package loader

import (
	"github.com/tamnd/gr/colseg"
	"github.com/tamnd/gr/colsegstore"
	"github.com/tamnd/gr/value"
)

// segmentSize is the number of cells per column segment written during the bulk
// load. Smaller means more segments (more directory entries); larger means fewer
// but bigger blobs. 4096 balances segment overhead vs decode cost.
const segmentSize = 4096

// colBuilder buffers colseg.Cell values for one property column and flushes
// completed segments to a colsegstore.Store (doc 19 §4.2).
//
// Cells must be appended in ascending dense-position order. Gaps (positions
// not present in the input) are filled with absent cells so the column is a
// contiguous run from firstPos to the highest written position.
type colBuilder struct {
	store    *colsegstore.Store
	key      uint32     // property-key token
	vtype    value.Type // column's declared value type
	cells    []colseg.Cell
	firstPos uint64 // dense position of cells[0]
	nextPos  uint64 // next position to be written (cursor)
}

// newColBuilder returns a colBuilder for a column that starts at startPos.
func newColBuilder(store *colsegstore.Store, key uint32, vtype value.Type, startPos uint64) *colBuilder {
	return &colBuilder{
		store:    store,
		key:      key,
		vtype:    vtype,
		cells:    make([]colseg.Cell, 0, segmentSize),
		firstPos: startPos,
		nextPos:  startPos,
	}
}

// Append adds one cell at the given dense position. Positions between nextPos
// and pos (exclusive) are filled with absent cells. After the append the buffer
// is flushed if it has reached segmentSize.
func (b *colBuilder) Append(pos uint64, cell colseg.Cell) error {
	// Fill any gap with absent cells.
	for b.nextPos < pos {
		b.cells = append(b.cells, colseg.Cell{Present: false})
		b.nextPos++
		if len(b.cells) >= segmentSize {
			if err := b.flush(); err != nil {
				return err
			}
		}
	}
	b.cells = append(b.cells, cell)
	b.nextPos++
	if len(b.cells) >= segmentSize {
		return b.flush()
	}
	return nil
}

// Flush writes any buffered cells as a final segment. Must be called after all
// rows have been appended.
func (b *colBuilder) Flush() error {
	if len(b.cells) == 0 {
		return nil
	}
	return b.flush()
}

func (b *colBuilder) flush() error {
	err := b.store.Append(b.key, b.firstPos, b.vtype, b.cells)
	if err != nil {
		return err
	}
	b.firstPos = b.nextPos
	b.cells = b.cells[:0]
	return nil
}
