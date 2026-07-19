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

// A local built during setup whose ADDRESS is handed to a runtime routine -- like
// QuickJS constructing a JSValue on the stack and passing &val. It is not a green
// input; the engine backs it with residual storage, flushes the cells it set (3, 4)
// into that storage, and passes the pointer, so rt_sum reads 7 through it.
const toyLocalEscapeInterp = `
void __cg12_merge_point(int pc, int sp);
struct Ctx { long a; long b; };
long rt_sum(struct Ctx *c) { return c->a + c->b; }
enum { OP_HALT, OP_GET };

long interp(const unsigned char *code, const long *in) {
    struct Ctx ctx;
    ctx.a = 3;
    ctx.b = 4;
    long base = rt_sum(&ctx);   /* &ctx escapes into a runtime call during setup */
    long stack[64];
    int sp = 0, pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_GET:  stack[sp++] = base + in[code[pc+1]]; pc += 2; break;
        case OP_HALT: return stack[sp-1];
        }
    }
}
`

func TestSpecializeEscapingLocalAddress(t *testing.T) {
	// GET 0, HALT  ->  rt_sum({3,4}) + in0 == 7 + in0
	prog := []byte{1, 0, 0}
	m, err := cc.Compile("toy.c", toyLocalEscapeInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Contains(t, residual, "call $rt_sum", "the setup call through &ctx remains")

	opt.Run(spec, opt.DefaultPipeline())
	// rt_sum reads the struct, so it needs its real body, not a scalar extern.
	m.Funcs = append(m.Funcs, spec.Funcs...)
	mc, err := interp.New(m)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100, -7} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 7+in0, v.I64(), "specialized(%d)", in0)
	}
}

// A local built inside a HANDLER (not setup) whose address escapes into a runtime
// call -- QuickJS's common `JSValue v = ...; f(&v)`. It is not persistent VM state
// (so not carried across merges), and its buffer lives in the specialized handler's
// own blocks: two MAKE ops at different positions become two handler copies, each
// needing its own buffer, so this also checks the per-copy buffer scoping.
const toyHandlerLocalInterp = `
void __cg12_merge_point(int pc, int sp);
struct Val { long tag; long data; };
long rt_use(struct Val *v) { return v->tag + v->data; }
enum { OP_HALT, OP_MAKE, OP_ADD };

long interp(const unsigned char *code, const long *in) {
    long stack[64];
    int sp = 0, pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_MAKE: {
            struct Val v;
            v.tag = 7;
            v.data = in[code[pc+1]];
            stack[sp++] = rt_use(&v);   /* &v (a handler local) escapes */
            pc += 2; break;
        }
        case OP_ADD: { long b = stack[--sp]; stack[sp-1] += b; pc += 1; break; }
        case OP_HALT: return stack[sp-1];
        }
    }
}
`

func TestSpecializeEscapingHandlerLocal(t *testing.T) {
	// MAKE 0, MAKE 1, ADD, HALT  ->  (7+in0) + (7+in1) == 14 + in0 + in1
	prog := []byte{1, 0, 1, 1, 2, 0}
	m, err := cc.Compile("toy.c", toyHandlerLocalInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Contains(t, residual, "call $rt_use", "the call through &v remains")

	opt.Run(spec, opt.DefaultPipeline())
	m.Funcs = append(m.Funcs, spec.Funcs...)
	mc, err := interp.New(m)
	require.NoError(t, err)

	var rf *ir.Func
	for _, f := range spec.Funcs {
		if f.Name == name {
			rf = f
		}
	}
	for _, tc := range []struct{ in0, in1 int64 }{{5, 9}, {100, -4}, {-7, 8}} {
		args := make([]interp.Value, len(rf.Params))
		for i, p := range rf.Params {
			if p.Name == "in0" {
				args[i] = interp.L(tc.in0)
			} else {
				args[i] = interp.L(tc.in1)
			}
		}
		v, err := mc.Call(name, args...)
		require.NoError(t, err)
		require.Equal(t, 14+tc.in0+tc.in1, v.I64(), "in0=%d in1=%d", tc.in0, tc.in1)
	}
}

// A by-value aggregate argument that is itself another call's aggregate RESULT --
// rt_inc(rt_mk(x)). rt_mk's result is an opaque residual struct pointer, not a
// struct whose cells the engine tracks, so the aggregate-argument copy must read it
// back with residual loads (aggCell's vResid path) to hand rt_inc its own copy.
const toyAggResultInterp = `
void __cg12_merge_point(int pc, int sp);
struct Val { long tag; long data; };
struct Val rt_mk(long x)          { struct Val v; v.tag = 0; v.data = x; return v; }
struct Val rt_inc(struct Val a)   { struct Val r; r.tag = a.tag; r.data = a.data + 1; return r; }
enum { OP_HALT, OP_MKINC };

long interp(const unsigned char *code, const long *in) {
    struct Val stack[64];
    int sp = 0, pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_MKINC: stack[sp++] = rt_inc(rt_mk(in[code[pc+1]])); pc += 2; break;
        case OP_HALT:  return stack[sp-1].data;
        }
    }
}
`

func TestSpecializeAggregateArgFromResult(t *testing.T) {
	// MKINC 0, HALT  ->  rt_inc(rt_mk(in0)).data == in0 + 1
	prog := []byte{1, 0, 0}
	m, err := cc.Compile("toy.c", toyAggResultInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Contains(t, residual, "call $rt_mk", "the inner call remains")
	require.Contains(t, residual, "call $rt_inc", "the outer by-value call remains")

	opt.Run(spec, opt.DefaultPipeline())
	m.Funcs = append(m.Funcs, spec.Funcs...)
	mc, err := interp.New(m)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100, -7} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, in0+1, v.I64(), "specialized(%d)", in0)
	}
}
