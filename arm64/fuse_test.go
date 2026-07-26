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

// An unconditional jump to the block laid out next is elided -- the block simply
// falls through instead of branching to its immediate successor.
func TestFallThroughElision(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("fallthru", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	cont := f.NewBlock("cont") // laid out immediately after entry
	e.Goto(cont)               // ...so this jump should vanish
	cont.Ret(cont.Add(ir.ClsL, a, b))

	text := disasmModule(t, m)
	require.NotContains(t, text, "\tb\t", "a jump to the next block should be elided")
	require.Contains(t, text, "ret")
}

// A load or store straight from a global folds the symbol's low bits into the
// access (adrp; ldr [reg, #:lo12:sym]) rather than materializing the whole
// address with a separate add.
func TestSymbolAddressFoldsIntoAccess(t *testing.T) {
	m := ir.NewModule()
	m.NoteSymAlign("myglobal", 8) // an 8-byte-aligned global permits the fold
	f := m.NewFunc("getg", ir.ClsL).Export()
	e := f.Entry()
	e.Ret(e.Load(ir.ClsL, f.Sym("myglobal", 0)))

	text := disasmModule(t, m)
	require.Contains(t, text, "adrp", "the page address is still an adrp")
	require.Contains(t, text, "[x17, #:lo12:myglobal]", "the low bits fold into the load's offset")
	require.NotContains(t, text, "add x17", "no separate address add")
}

// An under-aligned global (aligned less than the access width) does NOT fold --
// the scaled offset could not represent its low bits and the linker would reject
// it -- so the full address is materialized with a separate add.
func TestUnderAlignedSymbolDoesNotFold(t *testing.T) {
	m := ir.NewModule()
	m.NoteSymAlign("packed", 4) // 4-aligned, but an 8-byte load needs 8
	f := m.NewFunc("getp", ir.ClsL).Export()
	e := f.Entry()
	e.Ret(e.Load(ir.ClsL, f.Sym("packed", 0)))

	text := disasmModule(t, m)
	require.Contains(t, text, "add x17", "an under-aligned symbol keeps the address add")
	require.NotContains(t, text, "[x17, #:lo12:", "and does not fold into the access")
}

// A comparison that feeds only a conditional select is emitted as `cmp; csel
// <cond>`, selecting on the flags directly, rather than materializing the boolean
// (cset) and re-testing it against zero (cmp #0; csel ne).
func TestCompareSelectFusion(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("selcmp", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsL, e.Cmp(ir.CmpSlt, ir.ClsL, a, b), a, b))

	text := disasmModule(t, m)
	require.Contains(t, text, "csel", "the select should be a csel")
	require.Contains(t, text, ", lt", "the csel should carry the compare's condition")
	require.NotContains(t, text, "cset", "the boolean should not be materialized")
	require.Equal(t, 1, strings.Count(text, "\tcmp"), "exactly one compare, and no cmp #0 re-test")
}

// A mask-test condition `(x & mask) ? a : b` fuses to `tst; csel ne`, with no
// materialized AND result.
func TestMaskSelectFusion(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("seltst", ir.ClsL).Export()
	x, a, b := f.Param("x", ir.ClsL), f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsL, e.And(ir.ClsL, x, f.ConstInt(ir.ClsL, 8)), a, b))

	text := disasmModule(t, m)
	require.Contains(t, text, "tst", "the mask test should be a flags-only tst")
	require.Contains(t, text, "csel", "the select should be a csel")
	require.Contains(t, text, ", ne", "the csel should select on the mask being non-zero")
	require.NotContains(t, text, "cset")
}

// A constant-zero arm selects the zero register, so `c ? a : 0` needs no
// materializing mov.
func TestSelectZeroArmUsesZeroReg(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("selz", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsL, e.Cmp(ir.CmpSgt, ir.ClsL, a, b), a, f.ConstInt(ir.ClsL, 0)))

	text := disasmModule(t, m)
	require.Contains(t, text, "xzr", "a constant-zero arm should use the zero register")
	require.Contains(t, text, "csel")
}

// The fusion applies only when the compare feeds the select alone; a compare
// whose boolean has another use must still be materialized.
func TestCompareSelectNoFusionWhenReused(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("selcmp2", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	c := e.Cmp(ir.CmpSlt, ir.ClsL, a, b)
	r := e.Select(ir.ClsL, c, a, b)
	e.Ret(e.Add(ir.ClsL, r, c)) // second use of the boolean

	text := disasmModule(t, m)
	require.Contains(t, text, "cset", "a reused comparison result must still be materialized")
}

// A conjoined `if (cmp1 && cmp2)` branch fuses to cmp; ccmp; b.cond -- no branch
// between the compares and no materialized booleans.
func TestConjoinedAndFuses(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("cand", ir.ClsL).Export()
	a, b, c, d := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL), f.Param("c", ir.ClsL), f.Param("d", ir.ClsL)
	e := f.Entry()
	yes, no := f.NewBlock("yes"), f.NewBlock("no")
	c1 := e.Cmp(ir.CmpSlt, ir.ClsL, a, b)
	c2 := e.Cmp(ir.CmpSlt, ir.ClsL, c, d)
	e.Jnz(e.And(ir.ClsL, c1, c2), yes, no)
	yes.Ret(f.ConstInt(ir.ClsL, 1))
	no.Ret(f.ConstInt(ir.ClsL, 0))

	text := disasmModule(t, m)
	require.Contains(t, text, "ccmp", "the second compare should be a ccmp")
	require.Equal(t, 1, strings.Count(text, "\tcmp"), "one plain cmp, the other folded into the ccmp")
	require.Contains(t, text, "b.lt", "the branch tests the second predicate")
	require.NotContains(t, text, "cset", "no boolean should be materialized")
}

// A conjoined condition whose AND result is used elsewhere must not fuse.
func TestConjoinedNoFusionWhenReused(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("cand2", ir.ClsL).Export()
	a, b, c, d := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL), f.Param("c", ir.ClsL), f.Param("d", ir.ClsL)
	e := f.Entry()
	yes, no := f.NewBlock("yes"), f.NewBlock("no")
	and := e.And(ir.ClsL, e.Cmp(ir.CmpSlt, ir.ClsL, a, b), e.Cmp(ir.CmpSlt, ir.ClsL, c, d))
	e.Jnz(and, yes, no)
	yes.Ret(and) // second use of the AND result
	no.Ret(f.ConstInt(ir.ClsL, 0))

	text := disasmModule(t, m)
	require.NotContains(t, text, "ccmp", "a reused conjoined result must not fuse to ccmp")
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
