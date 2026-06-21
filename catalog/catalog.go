// Package catalog is gr's token dictionary: it interns the strings that label
// graph elements — node labels, relationship types, and property keys — into
// small dense integer tokens (spec 2060 doc 09 §3, doc 25 §4 deliverable 12).
// The storage engine stores tokens, never strings, so records stay fixed-width
// and cache-dense; the catalog is the one place that maps a token back to its
// name and a name forward to its token.
//
// All three dictionaries persist into a single append-only Log rooted at the
// header's CatalogRoot. Each entry is one tagged record — a kind byte then the
// length-prefixed name — appended in assignment order. Tokens are dense and
// monotonic per kind: the first label interned is label token 0, the second is
// 1, and so on. On open the Log is replayed front to back, which reconstructs
// the exact same token assignment, so tokens are stable across reopens.
//
// Interning a name mutates only in-memory maps plus an append to the Log; the
// change becomes durable when the enclosing transaction commits, inheriting the
// substrate's durability. The catalog is append-only — names are never removed
// or renumbered — so a token, once handed out, is valid for the life of the
// database.
package catalog

import (
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/pager"
	"github.com/tamnd/gr/store"
)

// Kind identifies which dictionary a token belongs to. The values are part of
// the on-disk record tag and must not change.
type Kind uint8

const (
	// KindLabel is the node-label dictionary.
	KindLabel Kind = 0
	// KindRelType is the relationship-type dictionary.
	KindRelType Kind = 1
	// KindPropKey is the property-key dictionary.
	KindPropKey Kind = 2
)

// dict is one name<->token dictionary.
type dict struct {
	names []string          // token -> name
	ids   map[string]uint32 // name -> token
}

func newDict() dict { return dict{ids: make(map[string]uint32)} }

func (d *dict) lookup(name string) (uint32, bool) {
	t, ok := d.ids[name]
	return t, ok
}

func (d *dict) name(t uint32) (string, bool) {
	if int(t) >= len(d.names) {
		return "", false
	}
	return d.names[t], true
}

// add appends a name without touching the log (used during replay and intern).
func (d *dict) add(name string) uint32 {
	t := uint32(len(d.names))
	d.names = append(d.names, name)
	d.ids[name] = t
	return t
}

// Catalog is the in-memory token dictionaries backed by a durable Log. Its
// persistent coordinates (the Log head and length) live in the section directory
// under store.SecCatalog, which the catalog keeps current as names are interned,
// so a reopen finds the exact committed extent to replay.
//
// The same Log also carries the schema records (doc 08 §8.1): a constraint add
// or drop is one more tagged record appended in order, so the constraints a file
// declares are rebuilt by the same replay that rebuilds the token dictionaries.
type Catalog struct {
	p     *pager.Pager
	secs  *store.Sections
	log   *store.Log
	dicts [3]dict

	cons    map[string]Constraint // name -> live constraint
	conSeq  []string              // constraint names in add order, for a stable listing
	idx     map[string]Index      // name -> live index
	idxSeq  []string              // index names in add order, for a stable listing
	schemaN uint64                // count of schema records ever appended (monotonic)
}

func newCatalog(p *pager.Pager, secs *store.Sections, log *store.Log) *Catalog {
	c := &Catalog{p: p, secs: secs, log: log, cons: make(map[string]Constraint), idx: make(map[string]Index)}
	for i := range c.dicts {
		c.dicts[i] = newDict()
	}
	return c
}

// Create initializes a fresh catalog, allocating its backing Log and recording
// its coordinates in the section directory and the header (durable at the next
// commit).
func Create(p *pager.Pager, secs *store.Sections) (*Catalog, error) {
	log, err := store.CreateLog(p, format.PageTypeCatalog)
	if err != nil {
		return nil, err
	}
	p.SetCatalogRoot(log.Head())
	if err := secs.Set(store.SecCatalog, log.Head(), 0); err != nil {
		return nil, err
	}
	return newCatalog(p, secs, log), nil
}

// Open reopens the catalog from the section directory, replaying its Log to
// rebuild the dictionaries.
func Open(p *pager.Pager, secs *store.Sections) (*Catalog, error) {
	head, length, err := secs.Get(store.SecCatalog)
	if err != nil {
		return nil, err
	}
	log, err := store.OpenLog(p, head, int(length))
	if err != nil {
		return nil, err
	}
	c := newCatalog(p, secs, log)
	if err := c.replay(); err != nil {
		return nil, err
	}
	return c, nil
}

// replay reads the whole Log front to back, re-adding each entry to its
// dictionary in the original order so tokens match their first assignment.
func (c *Catalog) replay() error {
	buf := make([]byte, c.log.Len())
	if err := c.log.Read(0, len(buf), buf); err != nil {
		return err
	}
	for len(buf) > 0 {
		kind := Kind(buf[0])
		buf = buf[1:]
		switch kind {
		case KindLabel, KindRelType, KindPropKey:
			name, n, err := format.String(buf)
			if err != nil {
				return err
			}
			buf = buf[n:]
			c.dicts[kind].add(name)
		case KindConstraintAdd:
			con, n, err := decodeConstraint(buf)
			if err != nil {
				return err
			}
			buf = buf[n:]
			c.applyConstraintAdd(con)
		case KindConstraintDrop:
			name, n, err := format.String(buf)
			if err != nil {
				return err
			}
			buf = buf[n:]
			c.applyConstraintDrop(name)
		case KindIndexAdd:
			ix, n, err := decodeIndex(buf)
			if err != nil {
				return err
			}
			buf = buf[n:]
			c.applyIndexAdd(ix)
		case KindIndexDrop:
			name, n, err := format.String(buf)
			if err != nil {
				return err
			}
			buf = buf[n:]
			c.applyIndexDrop(name)
		default:
			return errBadCatalogRecord
		}
	}
	return nil
}

// Intern returns the token for name in the given dictionary, assigning and
// durably appending a new token if the name is not yet known. The bool reports
// whether a new token was assigned.
func (c *Catalog) Intern(kind Kind, name string) (uint32, bool, error) {
	d := &c.dicts[kind]
	if t, ok := d.lookup(name); ok {
		return t, false, nil
	}
	rec := append([]byte{byte(kind)}, format.AppendString(nil, name)...)
	if _, err := c.log.Append(rec); err != nil {
		return 0, false, err
	}
	// Keep the section directory's recorded extent current so a reopen replays
	// exactly this much of the Log (durable when the transaction commits).
	if err := c.secs.Set(store.SecCatalog, c.log.Head(), uint64(c.log.Len())); err != nil {
		return 0, false, err
	}
	return d.add(name), true, nil
}

// Lookup returns the token for an already-interned name.
func (c *Catalog) Lookup(kind Kind, name string) (uint32, bool) {
	return c.dicts[kind].lookup(name)
}

// Name returns the name a token maps to.
func (c *Catalog) Name(kind Kind, token uint32) (string, bool) {
	return c.dicts[kind].name(token)
}

// Count returns how many tokens a dictionary holds.
func (c *Catalog) Count(kind Kind) int { return len(c.dicts[kind].names) }

// LogHead and LogLen expose the backing Log's persistent coordinates so the
// engine can record them in its section metadata.
func (c *Catalog) LogHead() format.PageID { return c.log.Head() }
func (c *Catalog) LogLen() int            { return c.log.Len() }
