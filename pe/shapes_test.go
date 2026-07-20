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

func runInterp(t *testing.T, m *ir.Module, spec *ir.Module, name string, in0 int64) int64 {
	t.Helper()
	m.Funcs = append(m.Funcs, spec.Funcs...)
	mc, err := interp.New(m)
	require.NoError(t, err)
	var rf *ir.Func
	for _, f := range spec.Funcs {
		if f.Name == name {
			rf = f
		}
	}
	args := make([]interp.Value, len(rf.Params))
	for i, p := range rf.Params {
		if p.Cls == ir.ClsP {
			args[i] = interp.Ptr(0)
		} else {
			args[i] = interp.L(in0)
		}
	}
	v, err := mc.Call(name, args...)
	require.NoError(t, err)
	return v.I64()
}

// Nested runtime branches that re-converge -- a numeric fast/slow path like
// QuickJS's OP_neg (int vs float vs slow, all joining before the next opcode). A
// sub-branch inside a diamond arm reaches the outer join; both its arms must merge
// so no residual block is left without a terminator.
const toyNestedBranch = `
extern const unsigned char *__cg12_code(void);
void __cg12_merge_point(const unsigned char *pc, long *sp);
long rt_slow(long x) { return x * 10; }

long interp(const long *in) {
    long stack[64];
    long *sp = stack;
    const unsigned char *pc = __cg12_code();
    for (;;) {
        __cg12_merge_point(pc, sp);
        unsigned char op = *pc++;
        switch (op) {
        case 1: {
            long v = in[*pc++];
            long r;
            if (v > 0) {
                if (v > 100) r = 100; else r = v;
            } else {
                r = rt_slow(v);
            }
            *sp++ = r;
            break;
        }
        case 0: return sp[-1];
        }
    }
}
`

func TestSpecializeNestedReconvergingBranches(t *testing.T) {
	prog := []byte{1, 0, 0} // CLASSIFY 0, HALT
	m, err := cc.Compile("toy.c", toyNestedBranch)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	require.NoError(t, ir.VerifyModule(spec)) // no block left without a terminator

	opt.Run(spec, opt.DefaultPipeline())
	require.NoError(t, ir.VerifyModule(spec))
	require.Equal(t, int64(5), runInterp(t, m, spec, name, 5)) // 0<5<=100 -> 5
	m2, _ := cc.Compile("toy.c", toyNestedBranch)
	spec2, _, _, _ := pe.Specialize(m2, "interp", prog)
	opt.Run(spec2, opt.DefaultPipeline())
	require.Equal(t, int64(100), runInterp(t, m2, spec2, name, 150)) // v>100 -> 100
	m3, _ := cc.Compile("toy.c", toyNestedBranch)
	spec3, _, _, _ := pe.Specialize(m3, "interp", prog)
	opt.Run(spec3, opt.DefaultPipeline())
	require.Equal(t, int64(-30), runInterp(t, m3, spec3, name, -3)) // v<=0 -> rt_slow = v*10
}

// A jump that skips a local's alloca -- QuickJS shares a slow path via `goto
// add_loc_slow`, which lands past the `JSValue ops[2]` declaration, so on that path
// the alloca instruction never runs. The storage exists regardless; the engine must
// materialize the region on demand rather than see an undefined temporary.
const toyGotoPastAlloca = `
extern const unsigned char *__cg12_code(void);
void __cg12_merge_point(const unsigned char *pc, long *sp);
long rt_read(const long *p) { return p[0] + p[1]; }

long interp(const long *in) {
    long stack[64];
    long *sp = stack;
    const unsigned char *pc = __cg12_code();
    for (;;) {
        __cg12_merge_point(pc, sp);
        unsigned char op = *pc++;
        switch (op) {
        case 1: {
            long v = in[*pc++];
            if (v >= 0) goto slow;   /* jumps past the tmp alloca below */
            v = v - 1;               /* fall-through path also enters the block */
            {
                long tmp[2];
            slow:                    /* reached by fall-through (alloca runs) and by goto (skipped) */
                tmp[0] = v; tmp[1] = v;
                *sp++ = rt_read(tmp);
            }
            break;
        }
        case 0: return sp[-1];
        }
    }
}
`

func TestSpecializeJumpPastAlloca(t *testing.T) {
	prog := []byte{1, 0, 0} // OP 0, HALT
	run := func(in0 int64) int64 {
		m, err := cc.Compile("toy.c", toyGotoPastAlloca)
		require.NoError(t, err)
		spec, name, _, err := pe.Specialize(m, "interp", prog)
		require.NoError(t, err)
		require.NoError(t, ir.VerifyModule(spec))
		opt.Run(spec, opt.DefaultPipeline())
		return runInterp(t, m, spec, name, in0)
	}
	require.Equal(t, int64(10), run(5))  // v>=0: goto slow (alloca skipped) -> rt_read{5,5}=10
	require.Equal(t, int64(-8), run(-3)) // v<0: v-1=-4, alloca runs -> rt_read{-4,-4}=-8
}
