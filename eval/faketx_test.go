package eval

import (
	"github.com/tamnd/gr/engine"
	"github.com/tamnd/gr/value"
)

// propKey identifies one property: an element id and a key token.
type propKey struct {
	id  uint64
	key engine.Token
}

// fakeTx is a minimal engine.Tx for the evaluator's property-read tests. Only
// NodeProperty and RelProperty carry data; the rest satisfy the interface and
// are unused here (the executor exercises the full SPI against a real engine).
type fakeTx struct {
	nodeProps  map[propKey]value.Value
	relProps   map[propKey]value.Value
	nodeLabels map[uint64][]engine.Token
	relTypes   map[uint64]engine.Token
}

func (f *fakeTx) NodeProperty(id engine.NodeID, key engine.Token) (value.Value, error) {
	return f.nodeProps[propKey{uint64(id), key}], nil
}

func (f *fakeTx) RelProperty(id engine.RelID, key engine.Token) (value.Value, error) {
	return f.relProps[propKey{uint64(id), key}], nil
}

func (f *fakeTx) RelType(id engine.RelID) (engine.Token, error) {
	return f.relTypes[uint64(id)], nil
}

func (f *fakeTx) NodePropertyKeys(id engine.NodeID) ([]engine.Token, error) {
	var out []engine.Token
	for pk := range f.nodeProps {
		if pk.id == uint64(id) {
			out = append(out, pk.key)
		}
	}
	return out, nil
}

func (f *fakeTx) RelPropertyKeys(id engine.RelID) ([]engine.Token, error) {
	var out []engine.Token
	for pk := range f.relProps {
		if pk.id == uint64(id) {
			out = append(out, pk.key)
		}
	}
	return out, nil
}

func (f *fakeTx) NodeExists(engine.NodeID) (bool, error) { return false, nil }
func (f *fakeTx) NodeLabels(id engine.NodeID) ([]engine.Token, error) {
	return f.nodeLabels[uint64(id)], nil
}
func (f *fakeTx) HasLabel(engine.NodeID, engine.Token) (bool, error)      { return false, nil }
func (f *fakeTx) ScanLabel(engine.Token, func(engine.NodeID) error) error { return nil }
func (f *fakeTx) Expand(engine.NodeID, engine.Token, engine.Direction, func(engine.Neighbor) error) error {
	return nil
}
func (f *fakeTx) Degree(engine.NodeID, engine.Token, engine.Direction) (int64, error) { return 0, nil }
func (f *fakeTx) CreateNode([]engine.Token) (engine.NodeID, error)                    { return 0, nil }
func (f *fakeTx) DeleteNode(engine.NodeID) error                                      { return nil }
func (f *fakeTx) CreateRel(engine.NodeID, engine.NodeID, engine.Token) (engine.RelID, error) {
	return 0, nil
}
func (f *fakeTx) DeleteRel(engine.RelID) error                                   { return nil }
func (f *fakeTx) SetNodeProperty(engine.NodeID, engine.Token, value.Value) error { return nil }
func (f *fakeTx) SetRelProperty(engine.RelID, engine.Token, value.Value) error   { return nil }
func (f *fakeTx) AddLabel(engine.NodeID, engine.Token) error                     { return nil }
func (f *fakeTx) RemoveLabel(engine.NodeID, engine.Token) error                  { return nil }
func (f *fakeTx) Commit() error                                                  { return nil }
func (f *fakeTx) Abort() error                                                   { return nil }
