package loader

// CatalogBuilder accumulates the label, relationship-type, and property-key
// token dictionaries across all input files (doc 19 §4.1.3). It assigns a
// numeric token to each string on first sight, consistently across files, and
// maintains the insertion order that the determinism guarantee (doc 19 §4.8)
// requires: two runs over the same input always produce the same dictionaries
// and therefore the same on-disk token references.
//
// The builder also records the label-group assignment (doc 19 §4.1.1): every
// primary-label signature maps to a LabelGroup whose dense-id counter is
// maintained here and in the IDMapBuilder.
type CatalogBuilder struct {
	labels    map[string]int   // label string → label token
	labelOrd  []string         // insertion-order list (for determinism)
	relTypes  map[string]int   // rel-type string → type token
	relTypeOrd []string
	propKeys  map[string]int   // property-key string → key token
	propKeyOrd []string

	groups        []groupDesc       // groups[LabelGroup] = description
	groupByPrimary map[int]LabelGroup // primary label token → group id
}

// LabelGroup is the numeric id of a columnar node group (doc 19 §4.1.1).
// Nodes that share the same primary label land in the same group and get
// contiguous dense ids.
type LabelGroup int

// groupDesc holds the per-group metadata accumulated during pass 1.
type groupDesc struct {
	primaryToken int     // the primary label's token
	count        uint64  // nodes assigned so far (the dense-id high-water mark)
}

// newCatalogBuilder returns an empty CatalogBuilder ready to intern tokens.
func newCatalogBuilder() *CatalogBuilder {
	return &CatalogBuilder{
		labels:         make(map[string]int),
		relTypes:       make(map[string]int),
		propKeys:       make(map[string]int),
		groupByPrimary: make(map[int]LabelGroup),
	}
}

// LabelToken interns a label string, returning a consistent token.
func (c *CatalogBuilder) LabelToken(label string) int {
	if tok, ok := c.labels[label]; ok {
		return tok
	}
	tok := len(c.labelOrd)
	c.labels[label] = tok
	c.labelOrd = append(c.labelOrd, label)
	return tok
}

// RelTypeToken interns a relationship-type string.
func (c *CatalogBuilder) RelTypeToken(typ string) int {
	if tok, ok := c.relTypes[typ]; ok {
		return tok
	}
	tok := len(c.relTypeOrd)
	c.relTypes[typ] = tok
	c.relTypeOrd = append(c.relTypeOrd, typ)
	return tok
}

// PropKeyToken interns a property-key string.
func (c *CatalogBuilder) PropKeyToken(key string) int {
	if tok, ok := c.propKeys[key]; ok {
		return tok
	}
	tok := len(c.propKeyOrd)
	c.propKeys[key] = tok
	c.propKeyOrd = append(c.propKeyOrd, key)
	return tok
}

// GroupFor returns the LabelGroup for the given label set, creating a new one
// on first sight (doc 19 §4.1.1). The primary label is the first element of
// labels (the "prefix label" or the first declared label, per the grouping
// policy). The labels slice must be non-empty; a node with no labels lands in
// the unlabeled group whose primary is the empty string.
func (c *CatalogBuilder) GroupFor(labels []string) LabelGroup {
	primaryStr := ""
	if len(labels) > 0 {
		primaryStr = labels[0]
	}
	primary := c.LabelToken(primaryStr)
	if g, ok := c.groupByPrimary[primary]; ok {
		return g
	}
	g := LabelGroup(len(c.groups))
	c.groups = append(c.groups, groupDesc{primaryToken: primary})
	c.groupByPrimary[primary] = g
	return g
}

// NextDenseID returns the next dense id for the given group and increments
// the group's counter (doc 19 §4.1): dense ids are assigned in encounter order,
// gapless, per group.
func (c *CatalogBuilder) NextDenseID(g LabelGroup) uint64 {
	id := c.groups[g].count
	c.groups[g].count++
	return id
}

// GroupCount returns the number of nodes assigned to group g so far.
func (c *CatalogBuilder) GroupCount(g LabelGroup) uint64 {
	return c.groups[g].count
}

// Groups returns the number of label groups discovered so far.
func (c *CatalogBuilder) Groups() int { return len(c.groups) }

// LabelName returns the label string for a token.
func (c *CatalogBuilder) LabelName(tok int) string {
	if tok < 0 || tok >= len(c.labelOrd) {
		return ""
	}
	return c.labelOrd[tok]
}
