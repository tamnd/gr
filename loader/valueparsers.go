package loader

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tamnd/gr/value"
)

// parseCSVField converts a raw CSV field string to a value.Value using the
// column's declared PropType (doc 19 §6.2). An empty field always returns
// (Null, false) so the caller writes a null cell. A parse failure returns an
// error and the caller rejects the row.
//
// For list columns the field is first split on the array delimiter and each
// element is parsed independently; the result is a value.List.
func parseCSVField(field string, pt PropType, isList bool, arrDelim rune) (value.Value, bool, error) {
	if field == "" {
		return value.Null, false, nil
	}
	if isList {
		parts := splitArrayField(field, arrDelim)
		elems := make([]value.Value, 0, len(parts))
		elemType := propTypeToValueType(pt)
		for _, p := range parts {
			if p == "" {
				elems = append(elems, value.Null)
				continue
			}
			v, err := parseSingleField(p, pt)
			if err != nil {
				return value.Null, false, err
			}
			_ = elemType
			elems = append(elems, v)
		}
		return value.List(elems...), true, nil
	}
	v, err := parseSingleField(field, pt)
	if err != nil {
		return value.Null, false, err
	}
	return v, true, nil
}

// parseSingleField converts one non-empty CSV token to a value.Value.
func parseSingleField(s string, pt PropType) (value.Value, error) {
	switch pt {
	case PropString:
		if !utf8.ValidString(s) {
			return value.Null, fmt.Errorf("non-UTF-8 string value")
		}
		return value.String(s), nil

	case PropInt:
		// Trim whitespace for robustness.
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return value.Null, fmt.Errorf("cannot parse %q as integer: %w", s, err)
		}
		return value.Int(n), nil

	case PropFloat:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return value.Null, fmt.Errorf("cannot parse %q as float: %w", s, err)
		}
		return value.Float(f), nil

	case PropBool:
		lower := strings.ToLower(strings.TrimSpace(s))
		switch lower {
		case "true", "1", "yes":
			return value.Bool(true), nil
		case "false", "0", "no":
			return value.Bool(false), nil
		default:
			return value.Null, fmt.Errorf("cannot parse %q as boolean", s)
		}

	case PropBytes:
		// Raw bytes columns hold the UTF-8 bytes of the field as-is.
		return value.Bytes([]byte(s)), nil

	// Temporal and spatial types are stored as strings for now; a later PR will
	// add proper parsing when the temporal value types land (doc 02 §4.5).
	case PropDate, PropDateTime, PropLocalDT, PropTime, PropLocalTime, PropDuration, PropPoint:
		return value.String(s), nil

	default:
		return value.String(s), nil
	}
}

// propTypeToValueType maps a header PropType to the storage value.Type that
// colseg will use for the segment plane. List columns map to TypeList.
func propTypeToValueType(pt PropType) value.Type {
	switch pt {
	case PropInt:
		return value.TypeInt
	case PropFloat:
		return value.TypeFloat
	case PropBool:
		return value.TypeBool
	case PropBytes:
		return value.TypeBytes
	case PropDate, PropDateTime, PropLocalDT, PropTime, PropLocalTime, PropDuration, PropPoint:
		return value.TypeString // stored as string until temporal types land
	default:
		return value.TypeString
	}
}
