package ast

import "strings"

// Print renders an expression to a compact, source-like string. It is the
// canonical expression printer used for diagnostics, implicit column names, and
// golden plan trees; it is a readable label, not a round-trippable serializer
// (parentheses are added only where a reader needs them, around binary operands).
func Print(e Expr) string {
	switch x := e.(type) {
	case *Literal:
		return x.Value.String()
	case *Param:
		return "$" + x.Name
	case *Variable:
		return x.Name
	case *Property:
		return Print(x.Base) + "." + x.Key
	case *Index:
		return Print(x.Base) + "[" + Print(x.Index) + "]"
	case *Slice:
		return Print(x.Base) + "[" + optPrint(x.Lo) + ".." + optPrint(x.Hi) + "]"
	case *Unary:
		if x.Op == OpNot {
			return "NOT " + paren(x.X)
		}
		return "-" + paren(x.X)
	case *Binary:
		return paren(x.L) + " " + x.Op.String() + " " + paren(x.R)
	case *IsNull:
		if x.Negate {
			return Print(x.X) + " IS NOT NULL"
		}
		return Print(x.X) + " IS NULL"
	case *ListLit:
		return "[" + joinExprs(x.Elems) + "]"
	case *MapLit:
		return "{" + joinEntries(x.Entries) + "}"
	case *FunctionCall:
		if x.Star {
			return x.Name + "(*)"
		}
		pre := ""
		if x.Distinct {
			pre = "DISTINCT "
		}
		return x.Name + "(" + pre + joinExprs(x.Args) + ")"
	case *Case:
		return printCase(x)
	default:
		return "?"
	}
}

// paren prints an expression, wrapping a binary or unary operand in parentheses
// so the operator structure reads unambiguously.
func paren(e Expr) string {
	switch e.(type) {
	case *Binary, *Unary:
		return "(" + Print(e) + ")"
	default:
		return Print(e)
	}
}

func optPrint(e Expr) string {
	if e == nil {
		return ""
	}
	return Print(e)
}

func joinExprs(es []Expr) string {
	var b strings.Builder
	for i, e := range es {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Print(e))
	}
	return b.String()
}

func joinEntries(es []PropEntry) string {
	var b strings.Builder
	for i, e := range es {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.Key)
		b.WriteString(": ")
		b.WriteString(Print(e.Value))
	}
	return b.String()
}

func printCase(c *Case) string {
	var b strings.Builder
	b.WriteString("CASE")
	if c.Subject != nil {
		b.WriteString(" ")
		b.WriteString(Print(c.Subject))
	}
	for _, w := range c.Whens {
		b.WriteString(" WHEN ")
		b.WriteString(Print(w.When))
		b.WriteString(" THEN ")
		b.WriteString(Print(w.Then))
	}
	if c.Else != nil {
		b.WriteString(" ELSE ")
		b.WriteString(Print(c.Else))
	}
	b.WriteString(" END")
	return b.String()
}
