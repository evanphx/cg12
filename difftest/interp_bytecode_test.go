package difftest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The register bytecode VM (interp.WithBytecode) is a second, independent
// execution strategy: it compiles each function to bytecode over a rolling
// register window, performing SSA destruction and register allocation that the
// tree-walker does not. It must agree with the tree-walker on every program.
//
// This runs the scalar+memory QBE-simple corpus through both engines and compares
// what the C driver would observe (a global the function writes, or its return
// value). No toolchain.
func TestBytecodeMatchesTreeWalkerQBE(t *testing.T) {
	cases := []struct {
		file   string
		global string // external global test() writes, or "" if test() returns the value
	}{
		{"collatz.ssa", "a"},
		{"double.ssa", "a"},
		{"eucl.ssa", "a"},
		{"loop.ssa", "a"},
		{"max.ssa", "a"},
		{"prime.ssa", "a"},
		{"euclc.ssa", ""},
		{"fixarg.ssa", ""},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "qbe", c.file))
			require.NoError(t, err)

			tw := runQBEEngine(t, string(src), c.global, false)
			bc := runQBEEngine(t, string(src), c.global, true)
			require.Equal(t, tw, bc, "tree-walker vs bytecode observable for %s", c.file)
		})
	}
}

// runQBEEngine drives test() through one engine and returns the observable value:
// the global the driver reads, or the return value.
func runQBEEngine(t *testing.T, src, global string, bytecode bool) uint64 {
	t.Helper()
	m, err := parse.Parse(src)
	require.NoError(t, err)

	var opts []interp.Option
	if bytecode {
		opts = append(opts, interp.WithBytecode())
	}
	mc, err := interp.New(m, opts...)
	require.NoError(t, err)
	if global != "" {
		mc.DefineExtern(global, 8, 8)
	}

	ret, err := mc.Call("test")
	require.NoError(t, err)
	if global == "" {
		return ret.U64()
	}
	addr, ok := mc.GlobalAddr(global)
	require.True(t, ok)
	b, err := mc.ReadBytes(addr, 8)
	require.NoError(t, err)
	var u uint64
	for i, by := range b {
		u |= uint64(by) << (8 * i)
	}
	return u
}
