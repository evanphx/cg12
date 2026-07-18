package interp_test

import (
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// A by-value aggregate flows through a return (mkpt), a by-value argument, and an
// aggregate parameter (sumpt): mkpt(3,4) builds a {w,w}, sumpt reads both fields.
func TestAggregateReturnParamArg(t *testing.T) {
	il := `type :pt = { w, w }

	function :pt $mkpt(w %x, w %y) {
	@start
		%r =l alloc4 8
		storew %x, %r
		%r4 =l add %r, 4
		storew %y, %r4
		ret %r
	}

	function w $sumpt(:pt %p) {
	@start
		%a =w loadw %p
		%p4 =l add %p, 4
		%b =w loadw %p4
		%s =w add %a, %b
		ret %s
	}

	export function w $test() {
	@start
		%p =:pt call $mkpt(w 3, w 4)
		%s =w call $sumpt(:pt %p)
		ret %s
	}`
	require.Equal(t, int64(7), run(t, il, "test").I64())
}

// A by-value aggregate argument is copied: the callee clobbering its parameter
// must not disturb the caller's original struct.
func TestAggregateArgIsCopied(t *testing.T) {
	il := `type :pt = { w, w }

	function $clobber(:pt %p) {
	@start
		storew 99, %p
		ret
	}

	export function w $test() {
	@start
		%p =l alloc4 8
		storew 5, %p
		call $clobber(:pt %p)
		%v =w loadw %p
		ret %v
	}`
	require.Equal(t, int64(5), run(t, il, "test").I64())
}

// A computed goto (the &&label extension): blockaddr mints a code address per
// label, a phi selects one, and br dispatches to it. The parser never emits these
// ops, so the function is built directly.
func TestComputedGoto(t *testing.T) {
	m := &ir.Module{}
	f := m.NewFunc("pick", ir.ClsW).Export()
	sel := f.Param("sel", ir.ClsW)

	entry := f.Entry()
	tA := f.NewBlock("tA")
	tB := f.NewBlock("tB")
	disp := f.NewBlock("disp")
	blkA := f.NewBlock("A")
	blkB := f.NewBlock("B")

	aA := entry.BlockAddr(blkA)
	aB := entry.BlockAddr(blkB)
	entry.Jnz(sel, tA, tB)

	tA.Goto(disp)
	tB.Goto(disp)

	addr := disp.Phi(ir.ClsL, ir.PhiEdge{From: tA, Val: aA}, ir.PhiEdge{From: tB, Val: aB})
	disp.BrIndirect(addr, blkA, blkB)

	blkA.Ret(f.Word(10))
	blkB.Ret(f.Word(20))

	mc, err := interp.New(m)
	require.NoError(t, err)

	v, err := mc.Call("pick", interp.W(1))
	require.NoError(t, err)
	require.Equal(t, int64(10), v.I64(), "sel!=0 dispatches to A")

	v, err = mc.Call("pick", interp.W(0))
	require.NoError(t, err)
	require.Equal(t, int64(20), v.I64(), "sel==0 dispatches to B")
}
