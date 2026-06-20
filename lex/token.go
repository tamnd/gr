// Package lex is the Cypher lexer: it scans query text into a token stream for
// the parser (spec 2060 doc 10 §2, doc 09 §2). It is the first stage of the M2
// read path and has no dependency on the storage engine — it turns characters
// into tokens, nothing more. Keywords are matched case-folded, identifiers stay
// case-sensitive, whitespace and comments are skipped, and every token carries
// its source position so later stages can point at the exact character that was
// wrong.
package lex

// Kind tags what a token is. Keywords, operators, and punctuation each get a
// distinct Kind so the parser switches on the Kind rather than re-comparing
// text; literals and identifiers carry their lexeme in the token's Text.
type Kind uint8

const (
	// EOF marks the end of the token stream.
	EOF Kind = iota

	// Ident is a variable, label, type, property-key, or function name. Its Text
	// is the (case-sensitive) name, with backticks already stripped.
	Ident
	// Int is an integer literal; Text is the raw lexeme (decimal, 0x.., or 0o..).
	Int
	// Float is a floating-point literal; Text is the raw lexeme.
	Float
	// String is a quoted string literal; Text is the decoded value with escapes
	// already resolved.
	String
	// Param is a parameter reference ($name or $0); Text is the name without the
	// leading dollar sign.
	Param

	// Arithmetic and string operators.
	Plus    // +
	Minus   // -
	Star    // *  (multiply, and the variable-length marker in patterns)
	Slash   // /
	Percent // %
	Caret   // ^  (power)

	// Comparison operators.
	Eq // =
	Ne // <>
	Lt // <
	Le // <=
	Gt // >
	Ge // >=

	// Punctuation.
	Lparen   // (
	Rparen   // )
	Lbracket // [
	Rbracket // ]
	Lbrace   // {
	Rbrace   // }
	Comma    // ,
	Colon    // :
	Dot      // .
	DotDot   // ..  (the variable-length range separator)
	Pipe     // |
	Semi     // ;

	// Keywords (matched case-insensitively). They occupy a contiguous range so
	// IsKeyword can range-test a Kind in O(1); the bounds are recorded just below.
	Match
	Optional
	Where
	With
	Return
	Unwind
	Order
	By
	Skip
	Limit
	Distinct
	As
	Union
	All
	And
	Or
	Xor
	Not
	In
	Is
	Starts
	Ends
	Contains
	Null
	True
	False
	Case
	When
	Then
	Else
	End
	Asc
	Desc
	Create
	Merge
	Set
	Delete
	Detach
	Remove
	On
	Foreach
)

// kwStart and kwEnd bound the contiguous keyword Kind range for IsKeyword.
const (
	kwStart = Match
	kwEnd   = Foreach
)

// keywords maps the case-folded keyword text to its Kind. Identifiers are looked
// up here after scanning; a hit becomes a keyword token, a miss stays Ident.
var keywords = map[string]Kind{
	"MATCH":    Match,
	"OPTIONAL": Optional,
	"WHERE":    Where,
	"WITH":     With,
	"RETURN":   Return,
	"UNWIND":   Unwind,
	"ORDER":    Order,
	"BY":       By,
	"SKIP":     Skip,
	"LIMIT":    Limit,
	"DISTINCT": Distinct,
	"AS":       As,
	"UNION":    Union,
	"ALL":      All,
	"AND":      And,
	"OR":       Or,
	"XOR":      Xor,
	"NOT":      Not,
	"IN":       In,
	"IS":       Is,
	"STARTS":   Starts,
	"ENDS":     Ends,
	"CONTAINS": Contains,
	"NULL":     Null,
	"TRUE":     True,
	"FALSE":    False,
	"CASE":     Case,
	"WHEN":     When,
	"THEN":     Then,
	"ELSE":     Else,
	"END":      End,
	"ASC":      Asc,
	"DESC":     Desc,
	"CREATE":   Create,
	"MERGE":    Merge,
	"SET":      Set,
	"DELETE":   Delete,
	"DETACH":   Detach,
	"REMOVE":   Remove,
	"ON":       On,
	"FOREACH":  Foreach,
}

// IsKeyword reports whether a Kind is one of the reserved keywords.
func (k Kind) IsKeyword() bool { return k >= kwStart && k <= kwEnd }

// names maps each Kind to a human-readable label for error messages. Keyword
// labels are upper-cased so a message reads "expected RETURN, found CREATE".
var names = map[Kind]string{
	EOF: "end of input", Ident: "identifier", Int: "integer", Float: "float",
	String: "string", Param: "parameter",
	Plus: "'+'", Minus: "'-'", Star: "'*'", Slash: "'/'", Percent: "'%'", Caret: "'^'",
	Eq: "'='", Ne: "'<>'", Lt: "'<'", Le: "'<='", Gt: "'>'", Ge: "'>='",
	Lparen: "'('", Rparen: "')'", Lbracket: "'['", Rbracket: "']'",
	Lbrace: "'{'", Rbrace: "'}'", Comma: "','", Colon: "':'", Dot: "'.'",
	DotDot: "'..'", Pipe: "'|'", Semi: "';'",
}

// String returns a human-readable label for the Kind, used in error messages.
func (k Kind) String() string {
	if s, ok := names[k]; ok {
		return s
	}
	if k.IsKeyword() {
		for s, kk := range keywords {
			if kk == k {
				return s
			}
		}
	}
	return "unknown"
}

// Token is one lexical unit: its Kind, the lexeme Text it carries, and the
// 1-based Line and Col where it begins. Pos is the 0-based byte offset, kept for
// callers that want a raw cursor.
type Token struct {
	Kind Kind
	Text string
	Pos  int
	Line int
	Col  int
}
