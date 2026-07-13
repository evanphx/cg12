package parse

import (
	"fmt"
	"strconv"
	"strings"
)

type tokKind int

const (
	tEOF tokKind = iota
	tPunct
	tWord   // bare identifier / keyword / opcode / class letter
	tTemp   // %name
	tGlobal // $name
	tLabel  // @name
	tType   // :name
	tInt
	tFloat
	tStr
)

type token struct {
	kind   tokKind
	s      string  // text (word/name/punct/string)
	i      int64   // integer value
	f      float64 // float value
	single bool    // tFloat: single precision (s_) vs double (d_)
	line   int
}

func (t token) String() string {
	switch t.kind {
	case tEOF:
		return "end of input"
	case tTemp:
		return "%" + t.s
	case tGlobal:
		return "$" + t.s
	case tLabel:
		return "@" + t.s
	case tType:
		return ":" + t.s
	case tInt:
		return strconv.FormatInt(t.i, 10)
	case tFloat:
		return "float"
	case tStr:
		return strconv.Quote(t.s)
	default:
		return t.s
	}
}

type lexer struct {
	src  string
	pos  int
	line int
}

func newLexer(src string) *lexer { return &lexer{src: src, line: 1} }

func isIdent(c byte) bool {
	return c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func (l *lexer) errf(format string, a ...any) error {
	return fmt.Errorf("line %d: %s", l.line, fmt.Sprintf(format, a...))
}

// next returns the next token.
func (l *lexer) next() (token, error) {
	l.skip()
	if l.pos >= len(l.src) {
		return token{kind: tEOF, line: l.line}, nil
	}
	c := l.src[l.pos]
	start := l.line
	switch {
	case c == '%' || c == '$' || c == '@' || c == ':':
		return l.sigil(c)
	case c == '"':
		return l.str()
	case c == '{' || c == '}' || c == '(' || c == ')' || c == ',' || c == '=' || c == '+':
		l.pos++
		return token{kind: tPunct, s: string(c), line: start}, nil
	case c == '.':
		if strings.HasPrefix(l.src[l.pos:], "...") {
			l.pos += 3
			return token{kind: tPunct, s: "...", line: start}, nil
		}
		return token{}, l.errf("unexpected '.'")
	case c == '-' || (c >= '0' && c <= '9'):
		return l.number()
	case isIdent(c):
		return l.word()
	}
	return token{}, l.errf("unexpected character %q", string(c))
}

// skip consumes whitespace and comments.
func (l *lexer) skip() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\n':
			l.line++
			l.pos++
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		default:
			return
		}
	}
}

func (l *lexer) sigil(c byte) (token, error) {
	line := l.line
	l.pos++ // consume sigil
	name := l.readIdent()
	if name == "" {
		return token{}, l.errf("expected a name after %q", string(c))
	}
	kind := map[byte]tokKind{'%': tTemp, '$': tGlobal, '@': tLabel, ':': tType}[c]
	return token{kind: kind, s: name, line: line}, nil
}

func (l *lexer) readIdent() string {
	start := l.pos
	for l.pos < len(l.src) && isIdent(l.src[l.pos]) {
		l.pos++
	}
	return l.src[start:l.pos]
}

func (l *lexer) str() (token, error) {
	line := l.line
	l.pos++ // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '"' {
			l.pos++
			return token{kind: tStr, s: sb.String(), line: line}, nil
		}
		if c == '\\' && l.pos+1 < len(l.src) {
			sb.WriteByte(c)
			sb.WriteByte(l.src[l.pos+1])
			l.pos += 2
			continue
		}
		if c == '\n' {
			l.line++
		}
		sb.WriteByte(c)
		l.pos++
	}
	return token{}, l.errf("unterminated string")
}

func (l *lexer) number() (token, error) {
	line := l.line
	start := l.pos
	if l.src[l.pos] == '-' {
		l.pos++
	}
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == 'x' || c == 'X' {
			l.pos++
			continue
		}
		break
	}
	text := l.src[start:l.pos]
	v, err := strconv.ParseInt(text, 0, 64)
	if err != nil {
		// Fall back to unsigned (e.g. 0xffffffffffffffff).
		if u, uerr := strconv.ParseUint(text, 0, 64); uerr == nil {
			return token{kind: tInt, i: int64(u), line: line}, nil
		}
		return token{}, l.errf("bad integer %q", text)
	}
	return token{kind: tInt, i: v, line: line}, nil
}

func (l *lexer) word() (token, error) {
	line := l.line
	w := l.readIdent()
	// Float literals are written s_<float> or d_<float>.
	if len(w) >= 2 && (w[0] == 's' || w[0] == 'd') && w[1] == '_' {
		single := w[0] == 's'
		rest := l.readFloatRest(w[2:])
		f, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return token{}, l.errf("bad float %q", w[:2]+rest)
		}
		return token{kind: tFloat, f: f, single: single, line: line}, nil
	}
	return token{kind: tWord, s: w, line: line}, nil
}

// readFloatRest extends a float body (already-read prefix) with any trailing
// sign, exponent, or fractional characters that readIdent could not include.
func (l *lexer) readFloatRest(prefix string) string {
	var sb strings.Builder
	sb.WriteString(prefix)
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E' {
			sb.WriteByte(c)
			l.pos++
			continue
		}
		break
	}
	return sb.String()
}
