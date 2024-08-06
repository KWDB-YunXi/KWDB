// Code generated by "stringer -type=Token scanner.go"; DO NOT EDIT.

package lang

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[ILLEGAL-0]
	_ = x[ERROR-1]
	_ = x[EOF-2]
	_ = x[IDENT-3]
	_ = x[STRING-4]
	_ = x[NUMBER-5]
	_ = x[WHITESPACE-6]
	_ = x[COMMENT-7]
	_ = x[LPAREN-8]
	_ = x[RPAREN-9]
	_ = x[LBRACKET-10]
	_ = x[RBRACKET-11]
	_ = x[LBRACE-12]
	_ = x[RBRACE-13]
	_ = x[DOLLAR-14]
	_ = x[COLON-15]
	_ = x[ASTERISK-16]
	_ = x[EQUALS-17]
	_ = x[ARROW-18]
	_ = x[AMPERSAND-19]
	_ = x[COMMA-20]
	_ = x[CARET-21]
	_ = x[ELLIPSES-22]
	_ = x[PIPE-23]
}

const _Token_name = "ILLEGALERROREOFIDENTSTRINGNUMBERWHITESPACECOMMENTLPARENRPARENLBRACKETRBRACKETLBRACERBRACEDOLLARCOLONASTERISKEQUALSARROWAMPERSANDCOMMACARETELLIPSESPIPE"

var _Token_index = [...]uint8{0, 7, 12, 15, 20, 26, 32, 42, 49, 55, 61, 69, 77, 83, 89, 95, 100, 108, 114, 119, 128, 133, 138, 146, 150}

func (i Token) String() string {
	if i < 0 || i >= Token(len(_Token_index)-1) {
		return "Token(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _Token_name[_Token_index[i]:_Token_index[i+1]]
}
