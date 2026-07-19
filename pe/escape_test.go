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

// A runtime pointer -- like QuickJS's JSContext -- handed by value to a runtime
// routine, not indexed as an array. The engine must not treat it as a green input
// array (which would synthesize scalars and then fail when the pointer itself
// escapes); it becomes a residual pointer parameter that the call receives.
const toyEscapeInterp = `
void __cg12_merge_point(int pc, int sp);
long rt_op(void *ctx, long a, long b);
enum { OP_HALT, OP_PUSH, OP_INPUT, OP_ADD };

long interp(void *ctx, const unsigned char *code, const long *in) {
    long stack[64];
    int sp = 0;
    int pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_PUSH:  stack[sp++] = code[pc+1];    pc += 2; break;
        case OP_INPUT: stack[sp++] = in[code[pc+1]]; pc += 2; break;
        case OP_ADD: {
            long b = stack[--sp];
            stack[sp-1] = rt_op(ctx, stack[sp-1], b);  /* ctx escapes into the call */
            pc += 1; break;
        }
        case OP_HALT: return stack[sp-1];
        }
    }
}
`

func TestSpecializeEscapingPointerParam(t *testing.T) {
	// PUSH 2, INPUT 0, ADD, HALT  ->  rt_op(ctx, 2, in0) == 2 + in0
	prog := []byte{1, 2, 2, 0, 3, 0}
	m, err := cc.Compile("toy.c", toyEscapeInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Contains(t, residual, "call $rt_op", "the runtime call survives with ctx")
	require.Contains(t, residual, "p %ctx", "ctx is a residual pointer parameter")

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec, interp.WithExtern("rt_op",
		func(_ *interp.Machine, a []interp.Value) (interp.Value, error) {
			return interp.L(a[1].I64() + a[2].I64()), nil // rt_op(ctx, x, y) = x + y
		}))
	require.NoError(t, err)

	var rf *ir.Func
	for _, f := range spec.Funcs {
		if f.Name == name {
			rf = f
		}
	}
	for _, in0 := range []int64{5, 100, -7} {
		var args []interp.Value
		for _, p := range rf.Params {
			if p.Cls == ir.ClsP {
				args = append(args, interp.Ptr(0)) // a dummy ctx; rt_op ignores it
			} else {
				args = append(args, interp.L(in0))
			}
		}
		v, err := mc.Call(name, args...)
		require.NoError(t, err)
		require.Equal(t, 2+in0, v.I64(), "specialized(%d)", in0)
	}
}
