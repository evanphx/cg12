package goc

import (
	"strings"
	"testing"
)

func TestCompileCoreGo(t *testing.T) {
	m, err := Compile("sum.go", []byte(`package main
func sum(n int64) int64 { s := int64(0); for i := int64(1); i <= n; i++ { if i == 3 { continue }; s += i }; return s }
func main() { if sum(5) != 12 { for { break } } }
`))
	if err != nil {
		t.Fatal(err)
	}
	s := m.String()
	for _, want := range []string{"function l $main.sum", "$main", "jnz", "add"} {
		if !strings.Contains(s, want) {
			t.Errorf("IR missing %q:\n%s", want, s)
		}
	}
}

func TestRejectUnsupportedType(t *testing.T) {
	_, err := Compile("bad.go", []byte("package p\nfunc f(s string) {}"))
	if err == nil || !strings.Contains(err.Error(), "unsupported parameter type string") {
		t.Fatalf("error = %v", err)
	}
}
