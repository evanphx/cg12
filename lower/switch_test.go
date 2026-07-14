package lower

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reachableCases returns the set of case blocks reachable from f's entry, so a
// lowering can be checked to preserve every arm.
func reachableCases(f *ir.Func) map[*ir.Block]bool {
	seen := map[*ir.Block]bool{}
	var walk func(b *ir.Block)
	walk = func(b *ir.Block) {
		if b == nil || seen[b] {
			return
		}
		seen[b] = true
		for _, s := range b.Succs() {
			walk(s)
		}
	}
	walk(f.Start)
	return seen
}

// buildSwitch makes a function whose entry switches k over the given values,
// each arm and the default returning a distinct constant.
func buildSwitch(signed bool, vals ...int64) (*ir.Func, []*ir.Block, *ir.Block) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	k := f.Param("k", ir.ClsW)
	dflt := f.NewBlock("dflt")
	dflt.Ret(f.ConstInt(ir.ClsW, -1))
	var arms []*ir.Block
	var cases []ir.SwitchCase
	for i, v := range vals {
		blk := f.NewBlock("")
		blk.Ret(f.ConstInt(ir.ClsW, int64(i)))
		arms = append(arms, blk)
		cases = append(cases, ir.SwitchCase{Val: v, Blk: blk})
	}
	f.Entry().Switch(k, dflt, signed, cases)
	return f, arms, dflt
}

func TestSwitchesRemovesTerminatorAndKeepsArms(t *testing.T) {
	// Enough cases to force the binary-search path.
	f, arms, dflt := buildSwitch(true, 1, 2, 3, 5, 8, 13, 21, 34)

	require.True(t, Switches(f))

	for _, b := range f.Blocks {
		assert.NotEqual(t, ir.JmpSwitch, b.Jmp.Kind, "no JmpSwitch remains after lowering")
	}
	reach := reachableCases(f)
	for i, a := range arms {
		assert.Truef(t, reach[a], "case arm %d still reachable", i)
	}
	assert.True(t, reach[dflt], "default still reachable")
}

func TestSwitchesLinearForFew(t *testing.T) {
	// A handful of cases lowers to a straight equality chain (no ordered compare).
	f, _, _ := buildSwitch(true, 10, 20)
	require.True(t, Switches(f))

	for _, b := range f.Blocks {
		for i := range b.Instrs {
			assert.NotEqual(t, ir.CmpSle, b.Instrs[i].Cmp, "small switch uses equality, not ordered compares")
			assert.NotEqual(t, ir.CmpUle, b.Instrs[i].Cmp)
		}
	}
}

func TestSwitchesNoop(t *testing.T) {
	// A function with no switch is left unchanged.
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	f.Entry().Ret(f.Param("a", ir.ClsW))
	assert.False(t, Switches(f))
}
