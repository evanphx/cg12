package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// twoJoinDiamonds builds a function whose promoted variables need a phi in more
// than one block: two diamonds in series, with a store in every arm and a load at
// each join. The iterated dominance frontier of the store blocks is therefore
// {join1, join2}, both live-in for both variables, so Mem2Reg creates four phis --
// and the order it creates them in decides which temporary id each one gets.
//
// Two variables rather than one because the outer loop over variables is already
// ordered; it is the inner walk of the frontier that was not.
func twoJoinDiamonds() *ir.Func {
	function := ir.NewModule().NewFunc("m", ir.ClsW)
	condition := function.Param("c", ir.ClsW)
	entry := function.Entry()
	left := function.NewBlock("left")
	right := function.NewBlock("right")
	joinOne := function.NewBlock("join1")
	third := function.NewBlock("third")
	fourth := function.NewBlock("fourth")
	joinTwo := function.NewBlock("join2")

	first := entry.Alloc(4, 4)
	second := entry.Alloc(4, 4)
	entry.Store(function.Word(1), first)
	entry.Store(function.Word(2), second)
	entry.Jnz(condition, left, right)

	left.Store(function.Word(3), first)
	left.Store(function.Word(4), second)
	left.Goto(joinOne)
	right.Store(function.Word(5), first)
	right.Store(function.Word(6), second)
	right.Goto(joinOne)

	joinOneFirst := joinOne.Load(ir.ClsW, first)
	joinOneSecond := joinOne.Load(ir.ClsW, second)
	joinOneSum := joinOne.Add(ir.ClsW, joinOneFirst, joinOneSecond)
	joinOne.Jnz(joinOneSum, third, fourth)

	third.Store(function.Word(7), first)
	third.Store(function.Word(8), second)
	third.Goto(joinTwo)
	fourth.Store(function.Word(9), first)
	fourth.Store(function.Word(10), second)
	fourth.Goto(joinTwo)

	joinTwoFirst := joinTwo.Load(ir.ClsW, first)
	joinTwoSecond := joinTwo.Load(ir.ClsW, second)
	joinTwo.Ret(joinTwo.Add(ir.ClsW, joinTwoFirst, joinTwoSecond))
	return function
}

// TestMem2RegPlacesPhisInTheSameOrderEveryTime is the optimizer's half of the
// reproducibility property TestCompilingTheSameSourceTwiceGivesTheSameModule
// checks for the goc front end.
//
// It works because Go randomizes each `range` over a map independently, so
// promoting two identically built functions in one process draws different
// traversal orders exactly as two processes would. The order that mattered is the
// walk of the iterated dominance frontier: that is a map keyed by block pointer,
// and placing a phi calls f.NewTemp, so ranging it numbered the phi temporaries
// differently on every compile -- and temporary ids reach register allocation and
// slot assignment. RUNTIME_PLAN.md section 5.10 records this, and that at the time
// it could only reach cg12cc: every goc module was over opt's function budget and
// took BoundedPipeline, which does not run Mem2Reg at all. The budget is gone
// (opt.ModulePipeline), so this property now guards goc builds too.
//
// The repeats are what makes this a test rather than a coin flip: with four phis
// over a two-block frontier, one comparison passes by chance often enough that a
// single pair proves nothing.
func TestMem2RegPlacesPhisInTheSameOrderEveryTime(t *testing.T) {
	first := twoJoinDiamonds()
	require.True(t, Mem2Reg(first))
	reference := first.String()

	for attempt := 0; attempt < 20; attempt++ {
		again := twoJoinDiamonds()
		require.True(t, Mem2Reg(again))
		require.Equal(t, reference, again.String(),
			"promoting the same function twice numbered its phis differently (attempt %d)", attempt)
	}
}
