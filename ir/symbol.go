package ir

import "strings"

// LinkerSymbol maps a semantic symbol name to its linker spelling: every
// character that is not an ASCII letter, digit, or underscore becomes an
// underscore, and an empty result becomes "anon". It is the single canonical
// mangling shared by the backend (which spells object symbols) and the
// frontend-agnostic passes (dead-function elimination canonicalizes names to
// match, e.g., a "runtime.foo" definition with a "runtime_foo" reference), so
// the two never drift.
func LinkerSymbol(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "anon"
	}
	return b.String()
}
