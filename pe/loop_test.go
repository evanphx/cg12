package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// A setup loop with a runtime trip count -- the shape QuickJS uses to clear var_buf
// before its interpreter loop. The induction variable starts green (0) but the bound
// n is runtime, so the loop cannot be unrolled; the engine residualizes it as a real
// loop whose carried cells (acc, i) become header phis, and the interpreter that
// follows specializes on top of the loop's result.
const toyLoopSum = `
void __cg12_merge_point(int pc, int sp);
long interp(const unsigned char *code, const long *in, long n) {
    long acc = 0;
    for (long i = 0; i < n; i++) acc += i;   /* residual loop: sum 0..n-1 */
    long stack[64];
    int sp = 0, pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case 1: stack[sp++] = acc + in[code[pc+1]]; pc += 2; break;
        case 0: return stack[sp-1];
        }
    }
}
`

func TestSpecializeResidualLoopSum(t *testing.T) {
	// GET 0, HALT  ->  (sum 0..n-1) + in0 == n(n-1)/2 + in0
	prog := []byte{1, 0, 0}
	m, err := cc.Compile("toy.c", toyLoopSum)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Contains(t, residual, "phi", "the loop survives as a residual loop with phis")
	require.NotContains(t, residual, "switch", "the interpreter dispatch is gone")

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)

	var rf *ir.Func
	for _, f := range spec.Funcs {
		if f.Name == name {
			rf = f
		}
	}
	call := func(n, in0 int64) int64 {
		args := make([]interp.Value, len(rf.Params))
		for i, p := range rf.Params {
			if p.Name == "n" {
				args[i] = interp.L(n)
			} else {
				args[i] = interp.L(in0)
			}
		}
		v, err := mc.Call(name, args...)
		require.NoError(t, err)
		return v.I64()
	}
	for _, n := range []int64{0, 1, 2, 5, 10, 100} {
		want := n * (n - 1) / 2
		require.Equal(t, want+7, call(n, 7), "n=%d", n)
	}
}

// A while-loop whose induction variable starts runtime (i = n) counting down, with a
// runtime-seeded accumulator -- so every loop-carried cell is runtime from the first
// iteration and the residual loop needs no generalization of a green value.
const toyLoopCountdown = `
void __cg12_merge_point(int pc, int sp);
long interp(const unsigned char *code, const long *in, long n) {
    long acc = 0;
    long i = n;
    while (i > 0) { acc += i; i--; }   /* residual loop: sum n..1 */
    long stack[64];
    int sp = 0, pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case 1: stack[sp++] = acc + in[code[pc+1]]; pc += 2; break;
        case 0: return stack[sp-1];
        }
    }
}
`

func TestSpecializeResidualLoopCountdown(t *testing.T) {
	// GET 0, HALT  ->  (sum n..1) + in0 == n(n+1)/2 + in0
	prog := []byte{1, 0, 0}
	m, err := cc.Compile("toy.c", toyLoopCountdown)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	t.Logf("residual:\n%s", spec.String())

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)

	var rf *ir.Func
	for _, f := range spec.Funcs {
		if f.Name == name {
			rf = f
		}
	}
	call := func(n, in0 int64) int64 {
		args := make([]interp.Value, len(rf.Params))
		for i, p := range rf.Params {
			if p.Name == "n" {
				args[i] = interp.L(n)
			} else {
				args[i] = interp.L(in0)
			}
		}
		v, err := mc.Call(name, args...)
		require.NoError(t, err)
		return v.I64()
	}
	for _, n := range []int64{0, 1, 2, 5, 10, 100} {
		want := n * (n + 1) / 2
		require.Equal(t, want-3, call(n, -3), "n=%d", n)
	}
}
