package bolt

import (
	"fmt"

	"github.com/tamnd/gr/pack"
	"github.com/tamnd/gr/value"
)

// The value/graph structure signatures (doc 18 §4.10, §6). These travel inside
// RECORD fields and parameter maps, distinct from the message signatures.
const (
	SigNode         = 0x4E
	SigRelationship = 0x52
	SigUnboundRel   = 0x72
	SigPath         = 0x50
)

// Node is a materialized graph node ready to serialize (doc 18 §6.1). The session
// fills it from the engine: the dense id, the labels resolved to names, the
// properties fetched late from the columnar segments, and the stable element id.
// The codec layer cannot produce this from a value.Value alone (which carries
// only the dense id), so materialization is the session's job and this struct is
// the hand-off.
type Node struct {
	ID        int64
	Labels    []string
	Props     map[string]any
	ElementID string
}

// Rel is a materialized relationship ready to serialize (doc 18 §6.2): the dense
// id, the endpoint dense ids, the type name, the properties, and the element ids.
type Rel struct {
	ID             int64
	StartID        int64
	EndID          int64
	Type           string
	Props          map[string]any
	ElementID      string
	StartElementID string
	EndElementID   string
}

// Materializer resolves a node or relationship dense id into its full form
// (doc 18 §6.10, ADR-22): the labels/type names, the properties, and the element
// ids. The session backs it with the engine; the codec calls it when a result
// value or a nested list/map element is a node or relationship handle.
type Materializer interface {
	MaterializeNode(id uint64) (Node, error)
	MaterializeRel(id uint64) (Rel, error)
}

// hasElementID reports whether the negotiated Bolt version carries element ids
// (doc 18 §6.11): 5.0 and up emit the longer node/relationship arity with the
// stable element id; 4.x emit the shorter legacy arity without it.
func hasElementID(v Version) bool { return v.Major >= 5 }

// BuildNode builds the PackStream Node structure for the negotiated version
// (doc 18 §6.1): 4 fields on 5.0+ (id, labels, props, element id), 3 on 4.x
// (no element id).
func BuildNode(n Node, ver Version) pack.Structure {
	labels := make([]any, len(n.Labels))
	for i, l := range n.Labels {
		labels[i] = l
	}
	fields := []any{n.ID, labels, mapOrEmpty(n.Props)}
	if hasElementID(ver) {
		fields = append(fields, n.ElementID)
	}
	return pack.Structure{Tag: SigNode, Fields: fields}
}

// BuildRel builds the PackStream Relationship structure for the negotiated
// version (doc 18 §6.2): 8 fields on 5.0+ (id, start, end, type, props, and the
// three element ids), 5 on 4.x.
func BuildRel(r Rel, ver Version) pack.Structure {
	fields := []any{r.ID, r.StartID, r.EndID, r.Type, mapOrEmpty(r.Props)}
	if hasElementID(ver) {
		fields = append(fields, r.ElementID, r.StartElementID, r.EndElementID)
	}
	return pack.Structure{Tag: SigRelationship, Fields: fields}
}

// BuildUnboundRel builds the PackStream UnboundRelationship structure for the
// negotiated version (doc 18 §6.3): 4 fields on 5.0+ (id, type, props, element
// id), 3 on 4.x. It appears only inside a Path, where the endpoints are carried
// by the path's node list and index walk.
func BuildUnboundRel(r Rel, ver Version) pack.Structure {
	fields := []any{r.ID, r.Type, mapOrEmpty(r.Props)}
	if hasElementID(ver) {
		fields = append(fields, r.ElementID)
	}
	return pack.Structure{Tag: SigUnboundRel, Fields: fields}
}

// EncodeValue converts a gr value.Value into the pack codec's plain-type form for
// a RECORD field or a returned scalar (doc 18 §6, §6.10). Scalars, lists, and
// maps map directly; a node or relationship handle is materialized through mat
// and built into its version-conditioned structure; a path is deduplicated into
// the nodes/relationships/indices form (doc 18 §6.4). Temporal and spatial values
// are not yet in value.Value and are added when those types land.
func EncodeValue(v value.Value, ver Version, mat Materializer) (any, error) {
	switch v.Type() {
	case value.TypeNull:
		return nil, nil
	case value.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case value.TypeInt:
		n, _ := v.AsInt()
		return n, nil
	case value.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case value.TypeString:
		s, _ := v.AsString()
		return s, nil
	case value.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case value.TypeList:
		xs, _ := v.AsList()
		out := make([]any, len(xs))
		for i, e := range xs {
			ev, err := EncodeValue(e, ver, mat)
			if err != nil {
				return nil, err
			}
			out[i] = ev
		}
		return out, nil
	case value.TypeMap:
		m, _ := v.AsMap()
		out := make(map[string]any, len(m))
		for k, e := range m {
			ev, err := EncodeValue(e, ver, mat)
			if err != nil {
				return nil, err
			}
			out[k] = ev
		}
		return out, nil
	case value.TypeNode:
		id, _ := v.AsNode()
		n, err := mat.MaterializeNode(id)
		if err != nil {
			return nil, err
		}
		return BuildNode(n, ver), nil
	case value.TypeRel:
		id, _ := v.AsRel()
		r, err := mat.MaterializeRel(id)
		if err != nil {
			return nil, err
		}
		return BuildRel(r, ver), nil
	case value.TypePath:
		return encodePath(v, ver, mat)
	default:
		return nil, fmt.Errorf("bolt: cannot serialize value of type %s", v.Type())
	}
}

// encodePath builds the PackStream Path structure (doc 18 §6.4) from a gr path
// value, deduplicating nodes and relationships by id and producing the index walk
// that reconstructs the alternating sequence. The relationship-index sign records
// the traversal direction: positive when the step goes along the stored
// start->end direction, negative when against it.
func encodePath(v value.Value, ver Version, mat Materializer) (any, error) {
	nodeVals := v.PathNodes()
	relVals := v.PathRels()
	if len(nodeVals) != len(relVals)+1 {
		return nil, fmt.Errorf("bolt: malformed path: %d nodes, %d relationships", len(nodeVals), len(relVals))
	}

	// Materialize the node sequence, deduplicating by id in order of first
	// appearance (doc 18 §6.4 field 0).
	var nodes []any
	nodeIndex := map[int64]int{}
	seqNodeID := make([]int64, len(nodeVals))
	for i, nv := range nodeVals {
		id, _ := nv.AsNode()
		mn, err := mat.MaterializeNode(id)
		if err != nil {
			return nil, err
		}
		seqNodeID[i] = mn.ID
		if _, seen := nodeIndex[mn.ID]; !seen {
			nodeIndex[mn.ID] = len(nodes)
			nodes = append(nodes, BuildNode(mn, ver))
		}
	}

	// Materialize the relationships, deduplicating by id, and build the index
	// list: a (signed rel index, node index) pair per step (doc 18 §6.4 fields
	// 1 and 2). The first node, nodes[0], is implicit and not in the index list.
	var rels []any
	relIndex := map[int64]int{}
	var indices []any
	for i, rv := range relVals {
		id, _ := rv.AsRel()
		mr, err := mat.MaterializeRel(id)
		if err != nil {
			return nil, err
		}
		pos, seen := relIndex[mr.ID]
		if !seen {
			pos = len(rels)
			relIndex[mr.ID] = pos
			rels = append(rels, BuildUnboundRel(mr, ver))
		}
		// The sign is the traversal direction at this step: + if we leave the
		// step's from-node along the stored start->end, - otherwise.
		signed := int64(pos + 1)
		if mr.StartID != seqNodeID[i] {
			signed = -signed
		}
		indices = append(indices, signed, int64(nodeIndex[seqNodeID[i+1]]))
	}

	if nodes == nil {
		nodes = []any{}
	}
	if rels == nil {
		rels = []any{}
	}
	if indices == nil {
		indices = []any{}
	}
	return pack.Structure{Tag: SigPath, Fields: []any{nodes, rels, indices}}, nil
}

// DecodeParam converts a decoded PackStream parameter value into a gr value.Value
// for binding into a query (doc 18 §6.9). Scalars, lists, and maps map directly.
// A Node or Relationship structure is reduced to its element id string, the
// convenience that lets WHERE elementId(n) = $node work whether the driver sends
// the element-id string or the node object. An unknown structure signature is a
// type error (the caller maps it to Neo.ClientError.Statement.TypeError);
// temporal and spatial structures are added when those value types land.
func DecodeParam(v any) (value.Value, error) {
	switch x := v.(type) {
	case nil:
		return value.Null, nil
	case bool:
		return value.Bool(x), nil
	case int64:
		return value.Int(x), nil
	case float64:
		return value.Float(x), nil
	case string:
		return value.String(x), nil
	case []byte:
		return value.Bytes(x), nil
	case []any:
		out := make([]value.Value, len(x))
		for i, e := range x {
			ev, err := DecodeParam(e)
			if err != nil {
				return value.Null, err
			}
			out[i] = ev
		}
		return value.List(out...), nil
	case map[string]any:
		out := make(map[string]value.Value, len(x))
		for k, e := range x {
			ev, err := DecodeParam(e)
			if err != nil {
				return value.Null, err
			}
			out[k] = ev
		}
		return value.Map(out), nil
	case pack.Structure:
		return decodeParamStruct(x)
	default:
		return value.Null, fmt.Errorf("bolt: cannot decode parameter of type %T", v)
	}
}

// decodeParamStruct reduces a Node or Relationship parameter structure to its
// element id string (doc 18 §6.9). The element id is the trailing string field of
// the 5.0+ arity; a structure without one (a 4.x-arity node, or a non-graph
// structure) is a type error here, since gr binds graph parameters by element id.
func decodeParamStruct(s pack.Structure) (value.Value, error) {
	switch s.Tag {
	case SigNode:
		if len(s.Fields) == 4 {
			if eid, ok := s.Fields[3].(string); ok {
				return value.String(eid), nil
			}
		}
		return value.Null, fmt.Errorf("bolt: node parameter has no element id to bind")
	case SigRelationship:
		if len(s.Fields) == 8 {
			if eid, ok := s.Fields[5].(string); ok {
				return value.String(eid), nil
			}
		}
		return value.Null, fmt.Errorf("bolt: relationship parameter has no element id to bind")
	default:
		return value.Null, fmt.Errorf("bolt: unsupported parameter structure signature 0x%02X", s.Tag)
	}
}

// mapOrEmpty returns m, or an empty map when m is nil, so a property map always
// encodes as a Dictionary rather than Null (doc 18 §6.1: a node's property field
// is always a map).
func mapOrEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
