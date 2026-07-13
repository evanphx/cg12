package arm64

import "testing"

func TestAsciiEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hi", "hi"},
		{"a\nb", `a\nb`},             // newline must not break the .ascii line
		{"say \"hi\"", `say \"hi\"`}, // quotes escaped
		{"c:\\x", `c:\\x`},           // backslash escaped
		{"tab\there", `tab\there`},   // tab escaped
		{"a\x00b", `a\000b`},         // NUL as fixed 3-digit octal
		{"\xff\x01", `\377\001`},     // arbitrary bytes (e.g. a quad constant)
	}
	for _, c := range cases {
		if got := asciiEscape(c.in); got != c.want {
			t.Errorf("asciiEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
