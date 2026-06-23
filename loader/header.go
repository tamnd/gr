package loader

import (
	"fmt"
	"strings"
)

// ColRole is the role a column plays in the input (doc 19 §6.5).
type ColRole uint8

const (
	RoleProperty ColRole = iota // a typed property column (name:type)
	RoleID                      // :ID (node files)
	RoleLabel                   // :LABEL (node files)
	RoleStartID                 // :START_ID (relationship files)
	RoleEndID                   // :END_ID (relationship files)
	RoleType                    // :TYPE (relationship files)
	RoleIgnore                  // :IGNORE
)

// PropType is the CSV type annotation from the header grammar (doc 19 §6.2).
// It maps onto the engine's value type system; the loader uses it to drive
// per-field parsing and to pick a column codec.
type PropType uint8

const (
	PropString    PropType = iota // default for untyped columns
	PropInt                       // int, short, byte, long -> Integer
	PropFloat                     // float, double -> Float
	PropBool                      // boolean -> Boolean
	PropDate                      // date -> Date
	PropDateTime                  // datetime -> DateTime (zoned)
	PropLocalDT                   // localdatetime -> LocalDateTime
	PropTime                      // time -> Time (zoned)
	PropLocalTime                 // localtime -> LocalTime
	PropDuration                  // duration -> Duration
	PropPoint                     // point -> Point
	PropBytes                     // byte[] -> Bytes
)

// ColDesc is the parsed description of one column in a node or relationship
// file header (doc 19 §6.1). The header grammar is:
//
//	field ::= name? (':' type)? ('[]')?       // property column
//	       |  ':ID'   ('(' id-space ')')?  (':' type)?
//	       |  ':LABEL'
//	       |  ':START_ID' ('(' id-space ')')?
//	       |  ':END_ID'   ('(' id-space ')')?
//	       |  ':TYPE'
//	       |  ':IGNORE'
type ColDesc struct {
	Role     ColRole
	Name     string   // property key name; empty for special columns
	IDSpace  string   // id space for :ID / :START_ID / :END_ID; empty = global
	PropType PropType // type for property columns and typed :ID
	IsList   bool     // true for name:type[]
}

// NodeHeader is the parsed header for a node file. It owns a ColDesc per
// input column and tracks which column carries :ID and :LABEL.
type NodeHeader struct {
	Cols        []ColDesc
	IDCol       int    // index of the :ID column; -1 if absent
	LblCol      int    // index of the :LABEL column; -1 if absent
	PrefixLabel string // from --nodes=Label=... (set by the caller)
}

// RelHeader is the parsed header for a relationship file.
type RelHeader struct {
	Cols       []ColDesc
	StartCol   int    // index of :START_ID
	EndCol     int    // index of :END_ID
	TypeCol    int    // index of :TYPE; -1 if absent
	PrefixType string // from --relationships=Type=... (set by the caller)
}

// parseNodeHeader parses a slice of raw header field strings into a NodeHeader
// (doc 19 §6.1, §6.5). It returns an error if the header violates the
// cardinality rules (exactly one :ID, at most one :LABEL, no :START_ID etc).
func parseNodeHeader(fields []string, prefixLabel string) (*NodeHeader, error) {
	h := &NodeHeader{
		Cols:        make([]ColDesc, len(fields)),
		IDCol:       -1,
		LblCol:      -1,
		PrefixLabel: prefixLabel,
	}
	seen := map[string]bool{}
	for i, f := range fields {
		f = strings.TrimSpace(f)
		cd, err := parseColDesc(f)
		if err != nil {
			return nil, fmt.Errorf("loader: header column %d %q: %w", i+1, f, err)
		}
		switch cd.Role {
		case RoleID:
			if h.IDCol >= 0 {
				return nil, fmt.Errorf("loader: header has more than one :ID column")
			}
			h.IDCol = i
		case RoleLabel:
			if h.LblCol >= 0 {
				return nil, fmt.Errorf("loader: header has more than one :LABEL column")
			}
			h.LblCol = i
		case RoleStartID, RoleEndID, RoleType:
			return nil, fmt.Errorf("loader: %s column is not allowed in a node header", specialName(cd.Role))
		case RoleProperty:
			if cd.Name != "" {
				if seen[cd.Name] {
					return nil, fmt.Errorf("loader: duplicate property key %q in header", cd.Name)
				}
				seen[cd.Name] = true
			}
		}
		h.Cols[i] = cd
	}
	if h.IDCol < 0 {
		return nil, fmt.Errorf("loader: node header missing required :ID column")
	}
	return h, nil
}

// parseRelHeader parses a slice of raw header field strings into a RelHeader
// (doc 19 §6.1, §6.5). It returns an error if required columns are absent or
// cardinality rules are violated.
func parseRelHeader(fields []string, prefixType string) (*RelHeader, error) {
	h := &RelHeader{
		Cols:       make([]ColDesc, len(fields)),
		StartCol:   -1,
		EndCol:     -1,
		TypeCol:    -1,
		PrefixType: prefixType,
	}
	seen := map[string]bool{}
	for i, f := range fields {
		f = strings.TrimSpace(f)
		cd, err := parseColDesc(f)
		if err != nil {
			return nil, fmt.Errorf("loader: header column %d %q: %w", i+1, f, err)
		}
		switch cd.Role {
		case RoleID, RoleLabel:
			return nil, fmt.Errorf("loader: %s column is not allowed in a relationship header", specialName(cd.Role))
		case RoleStartID:
			if h.StartCol >= 0 {
				return nil, fmt.Errorf("loader: header has more than one :START_ID column")
			}
			h.StartCol = i
		case RoleEndID:
			if h.EndCol >= 0 {
				return nil, fmt.Errorf("loader: header has more than one :END_ID column")
			}
			h.EndCol = i
		case RoleType:
			if h.TypeCol >= 0 {
				return nil, fmt.Errorf("loader: header has more than one :TYPE column")
			}
			h.TypeCol = i
		case RoleProperty:
			if cd.Name != "" {
				if seen[cd.Name] {
					return nil, fmt.Errorf("loader: duplicate property key %q in header", cd.Name)
				}
				seen[cd.Name] = true
			}
		}
		h.Cols[i] = cd
	}
	if h.StartCol < 0 {
		return nil, fmt.Errorf("loader: relationship header missing required :START_ID column")
	}
	if h.EndCol < 0 {
		return nil, fmt.Errorf("loader: relationship header missing required :END_ID column")
	}
	if h.TypeCol < 0 && prefixType == "" {
		return nil, fmt.Errorf("loader: relationship header has no :TYPE column and no prefix type was given")
	}
	return h, nil
}

// parseColDesc parses one header field token into a ColDesc.
func parseColDesc(f string) (ColDesc, error) {
	// Special columns start with ':'.
	if strings.HasPrefix(f, ":") {
		return parseSpecialCol(f)
	}
	// A structural column may carry an optional leading name, the way
	// neo4j-admin import writes `id:ID` or `start:START_ID`. When the token
	// after the first colon is a structural marker, the part before it is the
	// column's name (kept for readability; the loader stores the input id as a
	// property only when IDProperty is set, never from this name). When it is a
	// property type instead, the whole token is a property column.
	if i := strings.IndexByte(f, ':'); i > 0 {
		if cd, err := parseSpecialCol(f[i:]); err == nil {
			cd.Name = f[:i]
			return cd, nil
		}
	}
	// Property column: name[:type]['[]'?]
	return parsePropCol(f)
}

// parseSpecialCol parses :ID, :LABEL, :START_ID, :END_ID, :TYPE, :IGNORE and
// their optional (id-space) and :type suffixes.
func parseSpecialCol(f string) (ColDesc, error) {
	upper := strings.ToUpper(f)
	switch {
	case upper == ":LABEL":
		return ColDesc{Role: RoleLabel}, nil
	case upper == ":TYPE":
		return ColDesc{Role: RoleType}, nil
	case upper == ":IGNORE":
		return ColDesc{Role: RoleIgnore}, nil

	case strings.HasPrefix(upper, ":ID"):
		return parseIDLike(f, RoleID)
	case strings.HasPrefix(upper, ":START_ID"):
		return parseIDLike(f, RoleStartID)
	case strings.HasPrefix(upper, ":END_ID"):
		return parseIDLike(f, RoleEndID)
	}
	return ColDesc{}, fmt.Errorf("unknown special column %q", f)
}

// parseIDLike parses :ID[(space)][:type], :START_ID[(space)], :END_ID[(space)].
func parseIDLike(f string, role ColRole) (ColDesc, error) {
	cd := ColDesc{Role: role, PropType: PropString}

	// Determine the base token length based on role.
	var base string
	switch role {
	case RoleID:
		base = ":ID"
	case RoleStartID:
		base = ":START_ID"
	case RoleEndID:
		base = ":END_ID"
	}
	// Everything after the base token (case-insensitive prefix already matched by caller).
	rest := f[len(base):]

	// Optional (id-space).
	if strings.HasPrefix(rest, "(") {
		end := strings.Index(rest, ")")
		if end < 0 {
			return ColDesc{}, fmt.Errorf("unclosed '(' in %q", f)
		}
		cd.IDSpace = rest[1:end]
		rest = rest[end+1:]
	}

	// Optional :type (only meaningful for :ID in the spec, but we accept it
	// on :START_ID/:END_ID too so the type can drive parsing).
	if strings.HasPrefix(rest, ":") {
		pt, err := parsePropType(rest[1:])
		if err != nil {
			return ColDesc{}, fmt.Errorf("in %q: %w", f, err)
		}
		cd.PropType = pt
	} else if rest != "" {
		return ColDesc{}, fmt.Errorf("unexpected suffix %q in %q", rest, f)
	}
	return cd, nil
}

// parsePropCol parses a property column of the form name[:type]['[]'?].
func parsePropCol(f string) (ColDesc, error) {
	cd := ColDesc{Role: RoleProperty, PropType: PropString}

	// Split on ':' to find an optional type suffix.
	colonIdx := strings.LastIndex(f, ":")
	if colonIdx < 0 {
		// No type — plain name, defaults to string.
		cd.Name = f
		return cd, nil
	}

	name := f[:colonIdx]
	typePart := f[colonIdx+1:]

	// The type part may end with [] to mark a list column.
	if strings.HasSuffix(typePart, "[]") {
		cd.IsList = true
		typePart = typePart[:len(typePart)-2]
	}

	pt, err := parsePropType(typePart)
	if err != nil {
		return ColDesc{}, err
	}
	cd.Name = name
	cd.PropType = pt
	return cd, nil
}

// parsePropType maps a type keyword string (case-insensitive) to a PropType.
func parsePropType(s string) (PropType, error) {
	switch strings.ToLower(s) {
	case "string", "char":
		return PropString, nil
	case "int", "short", "byte", "long":
		return PropInt, nil
	case "float", "double":
		return PropFloat, nil
	case "boolean":
		return PropBool, nil
	case "date":
		return PropDate, nil
	case "datetime":
		return PropDateTime, nil
	case "localdatetime":
		return PropLocalDT, nil
	case "time":
		return PropTime, nil
	case "localtime":
		return PropLocalTime, nil
	case "duration":
		return PropDuration, nil
	case "point":
		return PropPoint, nil
	default:
		return PropString, fmt.Errorf("unknown property type %q", s)
	}
}

func specialName(r ColRole) string {
	switch r {
	case RoleID:
		return ":ID"
	case RoleLabel:
		return ":LABEL"
	case RoleStartID:
		return ":START_ID"
	case RoleEndID:
		return ":END_ID"
	case RoleType:
		return ":TYPE"
	default:
		return "unknown"
	}
}
