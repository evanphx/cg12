package difftest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/evanphx/cg12/wasm"
	"github.com/stretchr/testify/require"
)

// wasmtimeBin locates a wasmtime binary or skips.
func wasmtimeBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("wasmtime"); err == nil {
		return p
	}
	t.Skip("wasmtime not available")
	return ""
}

// TestWASMCorpus runs a subset of QBE's own test programs on WebAssembly. Each
// compiles to wasm through the same pipeline as the arm64 corpus test; the ones
// with a cleanly checkable result are executed on wasmtime. The programs use
// class-l (i64) pointers throughout, so this also exercises the address-wrapping
// that lets parsed QBE IL run on a 32-bit target with no pointer inference.
//
// Two driver shapes are handled: `int a; test(); return !(a==V)` (test writes an
// external global, checked by loading it) and `return !(test()==V)` (test
// returns the value directly).
func TestWASMCorpus(t *testing.T) {
	wt := wasmtimeBin(t)
	cases := []struct {
		file   string
		global string // external global test() writes, or "" if test() returns the value
		expect string
	}{
		{"collatz.ssa", "a", "178"},
		{"double.ssa", "a", "55"},
		{"eucl.ssa", "a", "1"},
		{"loop.ssa", "a", "5050"},
		{"max.ssa", "a", "200"},
		{"prime.ssa", "a", "104743"},
		{"euclc.ssa", "", "1"},
		{"fixarg.ssa", "", "1"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "qbe", c.file))
			require.NoError(t, err)
			m, err := parse.Parse(string(src))
			require.NoErrorf(t, err, "parse %s", c.file)

			invoke := "test"
			if c.global != "" {
				// Synthesize an entry that runs test() and yields the global it
				// wrote, so a single wasmtime invocation observes the result.
				w := m.NewFunc("__wasmmain", ir.ClsW).Export()
				e := w.Entry()
				e.CallVoid(w.Sym("test", 0))
				e.Ret(e.Load(ir.ClsW, w.Sym(c.global, 0)))
				invoke = "__wasmmain"
			}

			code, err := wasm.CompileModule(m)
			require.NoErrorf(t, err, "compile %s", c.file)

			path := filepath.Join(t.TempDir(), "m.wat")
			require.NoError(t, os.WriteFile(path, []byte(code), 0o644))

			cmd := exec.Command(wt, "run", "--invoke", invoke, path)
			var out strings.Builder
			cmd.Stdout = &out
			require.NoErrorf(t, cmd.Run(), "wasmtime %s\n%s", c.file, code)
			require.Equal(t, c.expect, strings.TrimSpace(out.String()), "result of %s", c.file)
		})
	}
}
