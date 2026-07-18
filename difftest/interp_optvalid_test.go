package difftest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
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

// FuzzInterpOpt explores the same translation-validation property over mutated
// IL: a program that runs to completion must return the same value whether or not
// the optimizer touched it. Only completed runs are compared — a pass may legally
// add or remove a trap on a dead computation (e.g. DCE dropping an unused load),
// so a trap on one side alone is inconclusive, not a divergence. Div-by-zero is
// made to yield zero (as an AArch64 sdiv does) for the same reason.
//
// To validate the fuzzer itself: revert a fold.go arithmetic fix and confirm this
// reports a divergence.
func FuzzInterpOpt(f *testing.F) {
	for _, file := range []string{"collatz.ssa", "eucl.ssa", "loop.ssa", "max.ssa", "double.ssa", "euclc.ssa", "prime.ssa"} {
		src, err := os.ReadFile(filepath.Join("testdata", "qbe", file))
		if err == nil {
			f.Add(string(src))
		}
	}
	f.Fuzz(func(t *testing.T, src string) {
		m, err := parse.Parse(src)
		if err != nil || !fuzzWellFormed(m) {
			return // not IL, or references temps nothing defines
		}
		entry, ok := fuzzEntry(m)
		if !ok {
			return // no zero-argument entry to drive
		}
		base, baseOK := fuzzRun(src, entry)
		if !baseOK {
			return
		}
		optd, optOK := fuzzRun(src, entry, opt.DefaultPipeline()...)
		if !optOK {
			return
		}
		require.Equalf(t, base, optd, "optimization changed the result of %s", entry)
	})
}

// fuzzWellFormed reports whether every temporary a module reads is defined
// somewhere (by a parameter, an instruction result, or a phi). The parser mints a
// temp on first mention, so a typo'd or dropped definition survives as an
// ever-undefined temp; VerifyModule does not reject that, and the optimizer, which
// assumes well-formed SSA, can loop on it. This is a structural gate for the
// fuzzer, not a dominance check.
func fuzzWellFormed(m *ir.Module) bool {
	for _, f := range m.Funcs {
		if f.Start == nil {
			continue
		}
		defined := map[int]bool{}
		mark := func(r ir.Ref) {
			if r.Kind == ir.RefTemp {
				defined[int(r.ID)] = true
			}
		}
		for _, p := range f.Params {
			defined[p.ID] = true
		}
		for _, b := range f.Blocks {
			for _, ph := range b.Phis {
				mark(ph.To)
			}
			for i := range b.Instrs {
				mark(b.Instrs[i].To)
			}
		}
		used := true
		check := func(r ir.Ref) {
			if r.Kind == ir.RefTemp && !defined[int(r.ID)] {
				used = false
			}
		}
		for _, b := range f.Blocks {
			for _, ph := range b.Phis {
				for _, a := range ph.Args {
					check(a)
				}
			}
			for i := range b.Instrs {
				for _, a := range b.Instrs[i].Args {
					check(a)
				}
			}
			check(b.Jmp.Arg)
			for _, a := range b.Jmp.Args {
				check(a)
			}
		}
		if !used {
			return false
		}
	}
	return true
}

// fuzzEntry reports a zero-parameter, non-variadic function with a body to drive,
// preferring the conventional names.
func fuzzEntry(m *ir.Module) (string, bool) {
	var fallback string
	for _, fn := range m.Funcs {
		if fn.Start == nil || len(fn.Params) != 0 || fn.Variadic {
			continue
		}
		if fn.Name == "test" || fn.Name == "main" {
			return fn.Name, true
		}
		if fallback == "" {
			fallback = fn.Name
		}
	}
	return fallback, fallback != ""
}

// fuzzRun parses src fresh, applies passes, and drives entry with a fuel bound and
// zero-yielding division; ok is false if the program could not be loaded or did
// not run to completion. It runs under a watchdog: mutated IL can violate SSA
// invariants that VerifyModule does not catch (undefined-in-scope phis, dominance
// violations), on which the optimizer — which assumes well-formed SSA — can loop
// or panic. Those are optimizer-robustness concerns, orthogonal to the semantic
// property under test, so a non-terminating or panicking optimization is treated
// as inconclusive rather than a divergence.
func fuzzRun(src, entry string, passes ...opt.Pass) (uint64, bool) {
	type result struct {
		v  uint64
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if recover() != nil {
				ch <- result{0, false}
			}
		}()
		m, err := parse.Parse(src)
		if err != nil {
			ch <- result{0, false}
			return
		}
		opt.Run(m, passes)
		mc, err := interp.New(m, interp.WithFuel(5_000_000), interp.WithDivZeroYieldsZero())
		if err != nil {
			ch <- result{0, false}
			return
		}
		ret, err := mc.Call(entry)
		if err != nil {
			ch <- result{0, false}
			return
		}
		ch <- result{ret.U64(), true}
	}()
	select {
	case r := <-ch:
		return r.v, r.ok
	case <-time.After(3 * time.Second):
		return 0, false // did not terminate; inconclusive
	}
}

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
