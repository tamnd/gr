package eval

import (
	"fmt"
	"math"

	"github.com/tamnd/gr/value"
)

// add implements the + operator, which is overloaded (doc 02 §5.2): numeric
// addition, string concatenation (coercing the other operand to its string
// form), and list concatenation. A null operand yields null.
func add(l, r value.Value) (value.Value, error) {
	if l.IsNull() || r.IsNull() {
		return value.Null, nil
	}
	lt, rt := l.Type(), r.Type()
	switch {
	case lt == value.TypeList || rt == value.TypeList:
		return concatList(l, r), nil
	case lt == value.TypeString || rt == value.TypeString:
		return value.String(stringForm(l) + stringForm(r)), nil
	case isNumber(lt) && isNumber(rt):
		return arith(l, r, '+')
	default:
		return value.Null, fmt.Errorf("eval: cannot add %s and %s", lt, rt)
	}
}

// arith implements numeric -, *, /, %, and the numeric case of +. Integer
// operands stay integers (with division and modulo by zero an error); a float
// operand widens the result to float, where division by zero follows IEEE-754
// (±Inf, NaN).
func arith(l, r value.Value, op byte) (value.Value, error) {
	if l.IsNull() || r.IsNull() {
		return value.Null, nil
	}
	if !isNumber(l.Type()) || !isNumber(r.Type()) {
		return value.Null, fmt.Errorf("eval: arithmetic requires numbers, got %s and %s", l.Type(), r.Type())
	}
	if l.Type() == value.TypeInt && r.Type() == value.TypeInt {
		li, _ := l.AsInt()
		ri, _ := r.AsInt()
		switch op {
		case '+':
			return value.Int(li + ri), nil
		case '-':
			return value.Int(li - ri), nil
		case '*':
			return value.Int(li * ri), nil
		case '/':
			if ri == 0 {
				return value.Null, fmt.Errorf("eval: integer division by zero")
			}
			return value.Int(li / ri), nil
		case '%':
			if ri == 0 {
				return value.Null, fmt.Errorf("eval: integer modulo by zero")
			}
			return value.Int(li % ri), nil
		}
	}
	lf, _ := l.AsFloat()
	rf, _ := r.AsFloat()
	switch op {
	case '+':
		return value.Float(lf + rf), nil
	case '-':
		return value.Float(lf - rf), nil
	case '*':
		return value.Float(lf * rf), nil
	case '/':
		return value.Float(lf / rf), nil
	case '%':
		return value.Float(math.Mod(lf, rf)), nil
	}
	return value.Null, fmt.Errorf("eval: unknown arithmetic operator")
}

// power implements ^, which always yields a float (doc 02 §5.2).
func power(l, r value.Value) (value.Value, error) {
	if l.IsNull() || r.IsNull() {
		return value.Null, nil
	}
	if !isNumber(l.Type()) || !isNumber(r.Type()) {
		return value.Null, fmt.Errorf("eval: ^ requires numbers, got %s and %s", l.Type(), r.Type())
	}
	lf, _ := l.AsFloat()
	rf, _ := r.AsFloat()
	return value.Float(math.Pow(lf, rf)), nil
}

// concatList joins two values into a list: list operands contribute their
// elements, a scalar operand contributes itself as one element.
func concatList(l, r value.Value) value.Value {
	var out []value.Value
	if ll, ok := l.AsList(); ok {
		out = append(out, ll...)
	} else {
		out = append(out, l)
	}
	if rl, ok := r.AsList(); ok {
		out = append(out, rl...)
	} else {
		out = append(out, r)
	}
	return value.List(out...)
}

// stringForm renders a value for string concatenation: a string contributes its
// raw text, every other scalar its display form.
func stringForm(v value.Value) string {
	if s, ok := v.AsString(); ok {
		return s
	}
	return v.String()
}
