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

// A QuickJS-shaped interpreter, annotated to be specializable. Its program does not
// arrive as a parameter -- like JS_CallInternal, whose bytecode lives in a runtime
// object -- so pc is seeded from the __cg12_code() marker, and the frame-layout count
// var_count comes from __cg12_green(0) so it is static (the operand stack pointer
// stays green). The local-init loop then has a static bound and unrolls, while the
// dispatch loop specializes as usual.
const toyMarkerInterp = `
extern const unsigned char *__cg12_code(void);
extern int __cg12_green(int id);
void __cg12_merge_point(const unsigned char *pc, long *sp);

long interp(const long *in) {
    long var[8];
    long stack[64];
    long *sp = stack;
    const unsigned char *pc = __cg12_code();
    int var_count = __cg12_green(0);
    for (int i = 0; i < var_count; i++) var[i] = 0;   /* static bound -> unrolled */
    for (;;) {
        __cg12_merge_point(pc, sp);
        unsigned char op = *pc++;
        switch (op) {
        case 1: *sp++ = *pc++;          break;   /* PUSH imm    */
        case 2: *sp++ = in[*pc++];      break;   /* INPUT       */
        case 3: var[*pc++] = *--sp;     break;   /* STORE local */
        case 4: *sp++ = var[*pc++];     break;   /* LOAD local  */
        case 5: { long b = *--sp; sp[-1] += b; break; } /* ADD  */
        case 0: return sp[-1];                   /* HALT        */
        }
    }
}
`

func TestSpecializeWithMarkers(t *testing.T) {
	// PUSH 5, STORE 0, INPUT 0, LOAD 0, ADD, HALT  ->  var0=5; 5 + in0
	prog := []byte{1, 5, 3, 0, 2, 0, 4, 0, 5, 0}
	meta := []int64{2} // var_count = 2

	m, err := cc.Compile("toy.c", toyMarkerInterp)
	require.NoError(t, err)
	spec, name, inputs, err := pe.SpecializeWithMeta(m, "interp", prog, meta)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Len(t, inputs, 1)
	require.NotContains(t, residual, "switch", "the dispatch folded away")
	require.NotContains(t, residual, "call $__cg12", "the markers are compile-time, not calls")

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100, -7} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 5+in0, v.I64(), "specialized(%d)", in0)
	}
}

// A handler that walks the operand stack with a pointer comparison (`p < sp`) --
// QuickJS's epilogue frees the frame exactly this way. The two operands are green
// addresses in the same region, so the comparison folds and the walk unrolls over
// the statically-known stack depth; without the fold it would force the green stack
// pointers into the runtime and escape.
const toyStackWalkInterp = `
extern const unsigned char *__cg12_code(void);
void __cg12_merge_point(const unsigned char *pc, long *sp);

long interp(const long *in) {
    long stack[64];
    long *sp = stack;
    const unsigned char *pc = __cg12_code();
    for (;;) {
        __cg12_merge_point(pc, sp);
        unsigned char op = *pc++;
        switch (op) {
        case 1: *sp++ = *pc++;     break;   /* PUSH imm */
        case 2: *sp++ = in[*pc++]; break;   /* INPUT    */
        case 6: {                           /* SUMALL: replace the stack with its sum */
            long acc = 0;
            for (long *p = stack; p < sp; p++) acc += *p;
            sp = stack;
            *sp++ = acc;
            break;
        }
        case 0: return sp[-1];              /* HALT */
        }
    }
}
`

// A green program pointer handed to a runtime routine (QuickJS stores pc as cur_pc
// for a backtrace). Its bytes stay static -- reads still fold -- but the pointer
// itself becomes a residual "code" parameter the caller sets to the real bytecode
// buffer, rather than escaping.
const toyCodePtrInterp = `
extern const unsigned char *__cg12_code(void);
void __cg12_merge_point(const unsigned char *pc, long *sp);
long rt_at(const unsigned char *p, long x) { (void)p; return x; }

long interp(const long *in) {
    long stack[64];
    long *sp = stack;
    const unsigned char *pc = __cg12_code();
    for (;;) {
        __cg12_merge_point(pc, sp);
        unsigned char op = *pc++;
        switch (op) {
        case 1: *sp++ = rt_at(pc, in[*pc]); pc++; break;  /* pc (a code ptr) escapes */
        case 0: return sp[-1];
        }
    }
}
`

func TestSpecializeEscapingCodePointer(t *testing.T) {
	// AT 0, HALT  ->  rt_at(pc, in0) == in0
	prog := []byte{1, 0, 0}
	m, err := cc.Compile("toy.c", toyCodePtrInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Contains(t, residual, "p %code", "the code pointer became a residual parameter")
	require.Contains(t, residual, "call $rt_at")

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
	for _, in0 := range []int64{5, 100, -7} {
		args := make([]interp.Value, len(rf.Params))
		for i, p := range rf.Params {
			if p.Cls == ir.ClsP {
				args[i] = interp.Ptr(0) // a dummy code base; rt_at ignores it
			} else {
				args[i] = interp.L(in0)
			}
		}
		v, err := mc.Call(name, args...)
		require.NoError(t, err)
		require.Equal(t, in0, v.I64(), "specialized(%d)", in0)
	}
}

func TestSpecializeGreenPointerCompare(t *testing.T) {
	// PUSH 3, PUSH 4, INPUT 0, SUMALL, HALT  ->  3 + 4 + in0
	prog := []byte{1, 3, 1, 4, 2, 0, 6, 0}
	m, err := cc.Compile("toy.c", toyStackWalkInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.NotContains(t, residual, "switch", "the dispatch folded away")

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100, -7} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 7+in0, v.I64(), "specialized(%d)", in0)
	}
}
