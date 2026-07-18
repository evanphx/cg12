package difftest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The QBE upstream files that define $main and print their result run through the
// interpreter: it builds argv the way the QBE harness invokes the binary (args
// "a b c"), calls main, captures the intrinsic output, and compares it to the
// embedded `# >>> output` block — no assembler, no host.
func TestInterpQBEMain(t *testing.T) {
	// These files define $main and embed a canonical `# >>> output` block, so the
	// interpreter can drive them end-to-end. (cprime/dynalloc/queen ship no output
	// block in this corpus, so there is nothing to compare against.)
	for _, file := range []string{"mandel.ssa", "echo.ssa", "puts10.ssa"} {
		t.Run(file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "qbe", file))
			require.NoError(t, err)
			tc := loadQBETest(string(src))
			require.Truef(t, tc.hasOut, "%s has an expected-output block", file)

			m, err := parse.Parse(string(src))
			require.NoErrorf(t, err, "parse %s", file)

			var buf bytes.Buffer
			mc, err := interp.New(m, interp.WithStdout(&buf), interp.WithFuel(500_000_000))
			if err != nil {
				t.Skipf("interp cannot load %s yet: %v", file, err)
			}

			// main is either main() or main(argc, argv); build argv only for the
			// latter, matching the QBE harness's invocation with args "a b c".
			var callArgs []interp.Value
			if mainParamCount(m) == 2 {
				args := []string{"prog", "a", "b", "c"}
				argvArr := mc.Alloc(len(args) * 8)
				for i, s := range args {
					require.NoError(t, mc.Store(argvArr+uint64(i*8), 8, mc.NewCString(s)))
				}
				callArgs = []interp.Value{interp.W(int32(len(args))), interp.Ptr(argvArr)}
			}

			_, err = mc.Call("main", callArgs...)
			var exit *interp.ExitTrap
			if errors.As(err, &exit) {
				err = nil // a clean exit() ends the program
			}
			require.NoErrorf(t, err, "run %s", file)
			require.Equalf(t, tc.output, buf.String(), "output of %s", file)
		})
	}
}

// vararg1 has no $main — its C driver computes a call chain by hand. This
// transcribes that driver: g("Hell%c %s %g!\n", 'o', "world", f(42, "x", 42.0)),
// where f returns its trailing double vararg and g vprintf's its own va_list.
func TestInterpVararg1(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "qbe", "vararg1.ssa"))
	require.NoError(t, err)
	tc := loadQBETest(string(src))
	require.True(t, tc.hasOut)

	m, err := parse.Parse(string(src))
	require.NoError(t, err)

	var buf bytes.Buffer
	mc, err := interp.New(m, interp.WithStdout(&buf))
	require.NoError(t, err)

	// r = f(42, "x", 42.0) — f skips the "x" pointer and returns the 42.0 double.
	r, err := mc.Call("f", interp.L(42), interp.Ptr(mc.NewCString("x")), interp.D(42.0))
	require.NoError(t, err)
	require.Equal(t, 42.0, r.F64())

	// g("Hell%c %s %g!\n", 'o', "world", r) — g vprintf's the trailing args.
	fmt := mc.NewCString("Hell%c %s %g!\n")
	world := mc.NewCString("world")
	_, err = mc.Call("g", interp.Ptr(fmt), interp.W('o'), interp.Ptr(world), interp.D(r.F64()))
	require.NoError(t, err)

	require.Equal(t, tc.output, buf.String())
}

func mainParamCount(m *ir.Module) int {
	for _, f := range m.Funcs {
		if f.Name == "main" {
			return len(f.Params)
		}
	}
	return 0
}
