package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// The QuickJS shape: a handler passes stack values to a runtime routine by value
// (rt_add takes two struct Vals and returns one) rather than pulling out scalar
// fields. The engine residualizes the aggregate call -- it builds each argument
// struct in fresh residual storage, passes it by value, and reads the result
// struct's fields -- so the dispatch and stack vanish and the by-value call remains.
const toyAggCallInterp = `
void __cg12_merge_point(int pc, int sp);
struct Val { long tag; long data; };
struct Val rt_add(struct Val a, struct Val b) {
    struct Val r; r.tag = 0; r.data = a.data + b.data; return r;
}
enum { OP_HALT, OP_PUSH, OP_INPUT, OP_ADD };

long interp(const unsigned char *code, const long *in) {
    struct Val stack[64];
    int sp = 0;
    int pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_PUSH:  stack[sp].tag = 0; stack[sp].data = code[pc+1];    sp++; pc += 2; break;
        case OP_INPUT: stack[sp].tag = 1; stack[sp].data = in[code[pc+1]]; sp++; pc += 2; break;
        case OP_ADD: {
            struct Val b = stack[--sp];
            struct Val a = stack[sp-1];
            stack[sp-1] = rt_add(a, b);
            pc += 1; break;
        }
        case OP_HALT: return stack[sp-1].data;
        }
    }
}
`

func TestSpecializeAggregateCall(t *testing.T) {
	// PUSH 2, INPUT 0, ADD, HALT  ->  rt_add({0,2}, {1,in0}).data == 2 + in0
	prog := []byte{agPUSH, 2, agINPUT, 0, agADD, agHALT}
	m, err := cc.Compile("toy.c", toyAggCallInterp)
	require.NoError(t, err)
	spec, name, inputs, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Len(t, inputs, 1)
	require.Contains(t, residual, "call $rt_add", "the by-value call remains")
	require.NotContains(t, residual, "switch", "the dispatch is gone")

	opt.Run(spec, opt.DefaultPipeline())

	// Run the residual alongside rt_add (which it calls) by placing it in the same
	// module: the aggregate ABI needs rt_add's real body, not a scalar extern.
	m.Funcs = append(m.Funcs, spec.Funcs...)
	mc, err := interp.New(m)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100, -7} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 2+in0, v.I64(), "specialized(%d)", in0)
	}
}
