package goc

import (
	"reflect"
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

func TestCompileRecordsExactGlobalPointerWords(t *testing.T) {
	m, err := Compile("globals.go", []byte(`package main
type node struct {
	next *node
	value int
	tail *node
}
var root node
var roots [2]*node
var pointer *node
var text string
var dynamic any
var values []int
`))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]int{
		"main.root":              {0, 2},
		"main.roots":             {0, 1},
		"main.pointer":           {0},
		"main.text":              {0},
		"main.text.descriptor":   {0},
		"main.dynamic":           {0},
		"main.values":            {0},
		"main.values.descriptor": {0},
	}
	for _, data := range m.Data {
		words, ok := want[data.Name]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(words, data.PointerWords) {
			t.Errorf("%s pointer words = %v, want %v", data.Name, data.PointerWords, words)
		}
		delete(want, data.Name)
	}
	for name := range want {
		t.Errorf("missing data definition %s", name)
	}
}

func TestRejectUnsupportedSelect(t *testing.T) {
	_, err := Compile("bad.go", []byte("package p\nfunc f() { select {} }"))
	if err == nil || !strings.Contains(err.Error(), "unsupported statement *ast.SelectStmt") {
		t.Fatalf("error = %v", err)
	}
}
