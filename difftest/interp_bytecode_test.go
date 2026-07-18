package difftest

import (
	"bytes"
	"errors"
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

// The QBE $main programs -- mandel (floats + printf + loops), echo (argv), puts10
// -- run through both engines end to end; the bytecode output must match both the
// tree-walker and the embedded expected block.
func TestBytecodeMatchesTreeWalkerMain(t *testing.T) {
	for _, file := range []string{"mandel.ssa", "echo.ssa", "puts10.ssa"} {
		t.Run(file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "qbe", file))
			require.NoError(t, err)
			tc := loadQBETest(string(src))

			tw := runMainEngine(t, string(src), false)
			bc := runMainEngine(t, string(src), true)
			require.Equal(t, tw, bc, "tree-walker vs bytecode stdout for %s", file)
			require.Equal(t, tc.output, bc, "output of %s", file)
		})
	}
}

// runMainEngine drives $main through one engine and returns its captured stdout.
func runMainEngine(t *testing.T, src string, bytecode bool) string {
	t.Helper()
	m, err := parse.Parse(src)
	require.NoError(t, err)

	var buf bytes.Buffer
	opts := []interp.Option{interp.WithStdout(&buf), interp.WithFuel(500_000_000)}
	if bytecode {
		opts = append(opts, interp.WithBytecode())
	}
	mc, err := interp.New(m, opts...)
	require.NoError(t, err)

	var call []interp.Value
	if mainParamCount(m) == 2 {
		args := []string{"prog", "a", "b", "c"}
		argvArr := mc.Alloc(len(args) * 8)
		for i, s := range args {
			require.NoError(t, mc.Store(argvArr+uint64(i*8), 8, mc.NewCString(s)))
		}
		call = []interp.Value{interp.W(int32(len(args))), interp.Ptr(argvArr)}
	}
	_, err = mc.Call("main", call...)
	var exit *interp.ExitTrap
	if errors.As(err, &exit) {
		err = nil
	}
	require.NoError(t, err)
	return buf.String()
}

// BenchmarkEngines contrasts the two interpreters on mandel (compute-heavy). The
// bytecode VM's pre-compiled instruction stream should outrun the tree-walk.
func BenchmarkEngines(b *testing.B) {
	src, err := os.ReadFile(filepath.Join("testdata", "qbe", "mandel.ssa"))
	require.NoError(b, err)
	run := func(bytecode bool) {
		m, _ := parse.Parse(string(src))
		opts := []interp.Option{interp.WithFuel(500_000_000)}
		if bytecode {
			opts = append(opts, interp.WithBytecode())
		}
		mc, _ := interp.New(m, opts...)
		mc.Call("main")
	}
	b.Run("treewalk", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			run(false)
		}
	})
	b.Run("bytecode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			run(true)
		}
	})
}
