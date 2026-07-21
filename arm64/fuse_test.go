package arm64

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// A comparison that feeds only the block's own conditional branch is emitted as
// `cmp; b.cond`, branching on the flags directly, rather than materializing the
// boolean with cset and testing it with cbnz.
func TestCompareBranchFusion(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("cmpbr", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	lt := f.NewBlock("lt")
	ge := f.NewBlock("ge")
	e.Jnz(e.Cmp(ir.CmpSlt, ir.ClsL, a, b), lt, ge)
	lt.Ret(f.ConstInt(ir.ClsL, 1))
	ge.Ret(f.ConstInt(ir.ClsL, 0))

	text := disasmModule(t, m)
	require.Contains(t, text, "b.lt", "the compare should branch on flags")
	require.NotContains(t, text, "cset", "the boolean should not be materialized")
	require.NotContains(t, text, "cbnz", "the branch should not test a materialized boolean")
}

// When the comparison's boolean has another use, it must still be materialized
// (the fusion applies only when the branch is the sole consumer).
func TestCompareBranchNoFusionWhenReused(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("cmpbr2", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	c := e.Cmp(ir.CmpSlt, ir.ClsL, a, b)
	lt := f.NewBlock("lt")
	ge := f.NewBlock("ge")
	e.Jnz(c, lt, ge)
	lt.Ret(c) // second use of the boolean
	ge.Ret(c)

	text := disasmModule(t, m)
	require.True(t, strings.Contains(text, "cset"),
		"a reused comparison result must still be materialized")
}
