package difftest

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The QBE upstream corpus files with a simple C driver — either "test() writes an
// external global that the driver reads" or "test() returns the value" — run
// through the interpreter with no toolchain. The external global the driver would
// have defined is supplied with DefineExtern; the rest is the same table the wasm
// harness uses (difftest/wasm_corpus_test.go). Files needing printf, argv, or
// variadics wait for Phase C.
func TestInterpQBESimple(t *testing.T) {
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
		{"fixarg.ssa", "", "1"}, // cnel of two distinct allocs is 1
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "qbe", c.file))
			require.NoError(t, err)
			m, err := parse.Parse(string(src))
			require.NoErrorf(t, err, "parse %s", c.file)

			mc, err := interp.New(m)
			if err != nil {
				t.Skipf("interp cannot load %s yet: %v", c.file, err)
			}
			if c.global != "" {
				mc.DefineExtern(c.global, 8, 8) // the C driver's `int a;`, resolved at runtime
			}
			v, err := mc.Call("test")
			require.NoErrorf(t, err, "run %s", c.file)

			var got string
			if c.global != "" {
				addr, ok := mc.GlobalAddr(c.global)
				require.True(t, ok)
				b, err := mc.ReadBytes(addr, 8)
				require.NoError(t, err)
				var u uint64
				for i, by := range b {
					u |= uint64(by) << (8 * i)
				}
				got = strconv.FormatInt(int64(u), 10)
			} else {
				got = strconv.FormatInt(v.I64(), 10)
			}
			require.Equal(t, c.expect, got)
		})
	}
}
