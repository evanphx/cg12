package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// An interpreter that returns an aggregate by value -- QuickJS's JS_CallInternal
// returns a JSValue. The residual returns the same aggregate: the returned struct's
// cells are copied into fresh residual storage and a pointer is returned, matching
// the ABI (the mirror of a by-value aggregate call argument).
const toyAggRetInterp = `
void __cg12_merge_point(int pc, int sp);
struct Val { long tag; long data; };

struct Val interp(const unsigned char *code, const long *in) {
    long stack[64];
    int sp = 0, pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case 1: stack[sp++] = in[code[pc+1]]; pc += 2; break;   /* INPUT */
        case 0: {                                               /* RET a Val */
            struct Val v;
            v.tag = 9;
            v.data = stack[sp-1];
            return v;
        }
        }
    }
}
`

func TestSpecializeAggregateReturn(t *testing.T) {
	// INPUT 0, RET  ->  { tag: 9, data: in0 }
	prog := []byte{1, 0, 0}
	m, err := cc.Compile("toy.c", toyAggRetInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	require.NoError(t, ir.VerifyModule(spec))
	residual := spec.String()
	t.Logf("residual:\n%s", residual)

	var rf *ir.Func
	for _, f := range spec.Funcs {
		if f.Name == name {
			rf = f
		}
	}
	require.NotNil(t, rf)
	require.NotNil(t, rf.RetAgg, "the residual returns an aggregate by value")
	require.NotContains(t, residual, "switch", "the dispatch folded away")

	// It survives the optimizer and the aggregate-return lowering it feeds.
	opt.Run(spec, opt.DefaultPipeline())
	require.NoError(t, ir.VerifyModule(spec))
}
