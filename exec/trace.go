package exec

import (
	"fmt"
	"strings"

	"github.com/tamnd/gr/eval"
	"github.com/tamnd/gr/plan"
)

// OpSpan is the handle to one per-operator trace span (doc 20 §12.2, the detailed tracing
// level). End closes the span carrying the row count the operator produced; the tracer records
// the close time as the operator's wall-clock duration. A nil OpSpan is safe to pass End, so
// the traceOp shim need not guard it.
type OpSpan interface {
	End(rows int)
}

// OpTracer emits per-operator trace spans as children of the gr.execute phase span (doc 20
// §12.2). It is the seam exec uses to call back into gr's tracing without importing it, the
// same pattern GraphObserver uses for graph-operator metrics. A nil OpTracer disables operator
// spans; the default when tracing_detail is "phase" or when no tracer is configured.
type OpTracer interface {
	// StartOp starts a span for the operator whose type name is kind (e.g. "NodeScan",
	// "Expand"). The returned span's End is called when the operator closes, so the span
	// covers open-through-close, the operator's full lifetime.
	StartOp(kind string) OpSpan
}

// wrapTrace inserts the tracing shim around op when an OpTracer is set. With a nil tracer
// it returns op unchanged, the uninstrumented path, so an ordinary run pays nothing.
func wrapTrace(tracer OpTracer, o plan.Op, op operator) operator {
	if tracer == nil {
		return op
	}
	return &traceOp{inner: op, tracer: tracer, kind: opKind(o)}
}

// opKind returns the operator's type name without package prefix or pointer sigil: the
// string the OpTracer's StartOp receives, e.g. "NodeScan", "Expand", "Filter".
func opKind(o plan.Op) string {
	s := fmt.Sprintf("%T", o)
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// traceOp is the per-operator tracing shim (doc 20 §12.2): it forwards open, next, and
// close to the operator it wraps; open starts the span and close ends it with the row
// count, so the span covers the operator's full lifetime and carries its output cardinality.
// The shim changes no result and reshapes no plan; remove every shim and the run is
// identical, which is what lets the trace measure the same execution EXPLAIN describes.
type traceOp struct {
	inner  operator
	tracer OpTracer
	kind   string
	span   OpSpan
	rows   int
}

func (o *traceOp) open(ctx *Ctx) error {
	o.span = o.tracer.StartOp(o.kind)
	return o.inner.open(ctx)
}

func (o *traceOp) close() error {
	err := o.inner.close()
	if o.span != nil {
		o.span.End(o.rows)
	}
	return err
}

func (o *traceOp) next() (eval.Row, bool, error) {
	row, ok, err := o.inner.next()
	if ok {
		o.rows++
	}
	return row, ok, err
}
