package difftest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// observable is what a program's execution reveals to the outside world: its
// return value, its intrinsic output, and whether it trapped. Memory images are
// deliberately excluded — DCE and dead-store elimination legitimately change dead
// state without changing behavior.
type observable struct {
	ret     uint64
	out     string
	trapped bool
}

// optProgram runs one program through the interpreter after applying a given set
// of optimization passes, and reports its observable. Re-parsing per call keeps
// each run independent, so a mutating pass cannot leak into the next.
type optProgram struct {
	name string
	src  string
	run  func(t *testing.T, src string, passes []opt.Pass) observable
}

// TestInterpOptEquivalence is the payoff of an independent interpreter: it is a
// translation validator for the optimizer. For each program, the unoptimized
// observable is the oracle, and every single pass in isolation, every growing
// prefix of the pipeline, and the full pipeline must reproduce it exactly. A
// divergence localizes the offending pass — which the compile-and-run harness,
// reporting only "optimized differs", cannot do.
func TestInterpOptEquivalence(t *testing.T) {
	for _, p := range optPrograms(t) {
		t.Run(p.name, func(t *testing.T) {
			base := p.run(t, p.src, nil)

			require.Equal(t, base, p.run(t, p.src, opt.DefaultPipeline()), "full pipeline diverged")

			for _, pass := range opt.DefaultPipeline() {
				require.Equalf(t, base, p.run(t, p.src, []opt.Pass{pass}),
					"pass %q diverged in isolation", pass.Name())
			}

			pipeline := opt.DefaultPipeline()
			for i := 1; i < len(pipeline); i++ {
				require.Equalf(t, base, p.run(t, p.src, pipeline[:i]),
					"pipeline prefix of %d passes diverged", i)
			}
		})
	}
}

func optPrograms(t *testing.T) []optProgram {
	t.Helper()
	var progs []optProgram

	// Inline programs whose arithmetic the optimizer can fold or reassociate at
	// compile time — the constant paths the QBE corpus does not exercise.
	for _, c := range []struct{ name, src string }{
		{"constfold", `export function w $go() {
			@start
			%a =w add 20, 22
			%b =w mul %a, 5
			ret %b
		}`},
		{"constcmp", `export function w $go() {
			@start
			%a =w add 97, 3
			%lt =w csltw %a, 200
			%eq =w ceqw %a, 100
			%r =w add %lt, %eq
			ret %r
		}`},
		{"foldloop", `export function l $go() {
			@start
			%n =l copy 10
			jmp @loop
			@loop
			%i =l phi @start 0, @loop %i1
			%s =l phi @start 0, @loop %s1
			%s1 =l add %s, %i
			%i1 =l add %i, 1
			%done =w csgel %i1, %n
			jnz %done, @exit, @loop
			@exit
			ret %s1
		}`},
	} {
		progs = append(progs, optProgram{name: c.name, src: c.src, run: runReturnProgram("go")})
	}

	// $main programs: build argv the QBE harness way and observe stdout.
	for _, file := range []string{"mandel.ssa", "echo.ssa", "puts10.ssa"} {
		progs = append(progs, optProgram{name: file, src: readQBE(t, file), run: runMainProgram})
	}

	// simple test() programs: observe the value the driver reads (a global the
	// function writes, or its return value).
	for _, c := range []struct{ file, global string }{
		{"collatz.ssa", "a"},
		{"double.ssa", "a"},
		{"eucl.ssa", "a"},
		{"loop.ssa", "a"},
		{"max.ssa", "a"},
		{"prime.ssa", "a"},
		{"euclc.ssa", ""},
		{"fixarg.ssa", ""},
	} {
		global := c.global
		progs = append(progs, optProgram{
			name: c.file,
			src:  readQBE(t, c.file),
			run: func(t *testing.T, src string, passes []opt.Pass) observable {
				return runTestProgram(t, src, passes, global)
			},
		})
	}
	return progs
}

func readQBE(t *testing.T, file string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "qbe", file))
	require.NoError(t, err)
	return string(src)
}

func optLoad(t *testing.T, src string, passes []opt.Pass, opts ...interp.Option) (*interp.Machine, int) {
	t.Helper()
	m, err := parse.Parse(src)
	require.NoError(t, err)
	opt.Run(m, passes)
	mc, err := interp.New(m, append([]interp.Option{interp.WithFuel(1_000_000_000)}, opts...)...)
	require.NoError(t, err, "interp load after opt")
	return mc, mainParamCount(m)
}

func runMainProgram(t *testing.T, src string, passes []opt.Pass) observable {
	var buf bytes.Buffer
	mc, params := optLoad(t, src, passes, interp.WithStdout(&buf))

	// main is either main() or main(argc, argv), matching the QBE harness's
	// invocation with args "a b c".
	var call []interp.Value
	if params == 2 {
		args := []string{"prog", "a", "b", "c"}
		argvArr := mc.Alloc(len(args) * 8)
		for i, s := range args {
			require.NoError(t, mc.Store(argvArr+uint64(i*8), 8, mc.NewCString(s)))
		}
		call = []interp.Value{interp.W(int32(len(args))), interp.Ptr(argvArr)}
	}

	ret, err := mc.Call("main", call...)
	return finish(buf.String(), ret, err)
}

func runReturnProgram(fn string) func(*testing.T, string, []opt.Pass) observable {
	return func(t *testing.T, src string, passes []opt.Pass) observable {
		mc, _ := optLoad(t, src, passes)
		ret, err := mc.Call(fn)
		return finish("", ret, err)
	}
}

func runTestProgram(t *testing.T, src string, passes []opt.Pass, global string) observable {
	mc, _ := optLoad(t, src, passes)
	if global != "" {
		mc.DefineExtern(global, 8, 8)
	}
	ret, err := mc.Call("test")
	obs := finish("", ret, err)
	if global != "" && !obs.trapped {
		addr, _ := mc.GlobalAddr(global)
		b, rerr := mc.ReadBytes(addr, 8)
		require.NoError(t, rerr)
		var u uint64
		for i, by := range b {
			u |= uint64(by) << (8 * i)
		}
		obs.ret = u
	}
	return obs
}

// A fuzzer over mutated IL was tried here (interp baseline vs interp-after-
// pipeline) and deliberately removed: raw-text mutation produces programs with
// undefined behavior — zero-size allocations that alias, out-of-bounds and
// type-punned loads, uninitialized reads — on which the interpreter's concrete
// memory and the optimizer's alias analysis legitimately disagree without either
// being wrong. Distinguishing a real miscompile from a UB divergence needs a UB
// model the interpreter does not have, so the fuzzer reported false positives. It
// served its purpose first: it found that mem2reg could loop on IR VerifyModule
// accepted, which is now fixed in ir/verify.go. TestInterpOptEquivalence remains
// the sound, curated translation validator.

func finish(out string, ret interp.Value, err error) observable {
	if err != nil {
		var exit *interp.ExitTrap
		if errors.As(err, &exit) {
			return observable{out: out, ret: uint64(exit.Code)}
		}
		return observable{out: out, trapped: true}
	}
	return observable{out: out, ret: ret.U64()}
}
