package opt_test

import (
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

func countRet(f *ir.Func) int {
	n := 0
	for _, b := range f.Blocks {
		if b.Jmp.Kind == ir.JmpRet {
			n++
		}
	}
	return n
}

// Two return blocks reached from distinct predecessors merge into one, and each
// path still returns its own value (through the merged block's phi).
func TestTailMergeReturns(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("pick", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	ba, bb, r1, r2 := f.NewBlock("A"), f.NewBlock("B"), f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(a, ba, bb)
	ba.Goto(r1)
	bb.Goto(r2)
	r1.Ret(a)
	r2.Ret(b)

	require.True(t, opt.TailMerge(f), "the two return blocks should merge")
	opt.SimplifyCFG(f)
	require.Equal(t, 1, countRet(f), "one shared return block")

	mc, err := interp.New(m)
	require.NoError(t, err)
	got, _ := mc.Call("pick", interp.L(5), interp.L(9)) // a!=0 -> returns a
	require.Equal(t, int64(5), got.I64())
	got, _ = mc.Call("pick", interp.L(0), interp.L(9)) // a==0 -> returns b
	require.Equal(t, int64(9), got.I64())
}

// Void returns merge with no phi, and the function still runs.
func TestTailMergeVoidReturns(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFuncVoid("v").Export()
	a := f.Param("a", ir.ClsL)
	e := f.Entry()
	ba, bb, r1, r2 := f.NewBlock("A"), f.NewBlock("B"), f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(a, ba, bb)
	ba.Goto(r1)
	bb.Goto(r2)
	r1.RetVoid()
	r2.RetVoid()

	require.True(t, opt.TailMerge(f))
	opt.SimplifyCFG(f)
	require.Equal(t, 1, countRet(f), "one shared void return")

	mc, err := interp.New(m)
	require.NoError(t, err)
	_, err = mc.Call("v", interp.L(1))
	require.NoError(t, err)
	_, err = mc.Call("v", interp.L(0))
	require.NoError(t, err)
}

// A return block a predecessor reaches together with another (if/else returns)
// is left alone: a phi cannot carry two values on one predecessor's edges.
func TestTailMergeSkipsConflict(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("iff", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	r1, r2 := f.NewBlock("r1"), f.NewBlock("r2")
	e.Jnz(a, r1, r2) // one predecessor reaches both returns
	r1.Ret(a)
	r2.Ret(b)

	require.False(t, opt.TailMerge(f), "conflicting returns must not merge")
	require.Equal(t, 2, countRet(f))
}
