package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEscapeNonEscaping(t *testing.T) {
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	a := e.Alloc(4, 4)
	e.Store(x, a)
	e.Ret(e.Load(ir.ClsW, a))

	ai := newAliasInfo(f)
	assert.False(t, ai.escaped[a.ID], "alloc used only for local load/store does not escape")
}

func TestEscapeViaCall(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("m")
	e := f.Entry()
	a := e.Alloc(8, 8)
	e.CallVoid(f.Sym("g", 0), a) // address passed to a call
	e.RetVoid()

	ai := newAliasInfo(f)
	assert.True(t, ai.escaped[a.ID])
}

func TestEscapeViaStoredPointer(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("m")
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	a := e.Alloc(8, 8)
	e.Store(a, p) // the alloc's address is written to memory
	e.RetVoid()

	ai := newAliasInfo(f)
	assert.True(t, ai.escaped[a.ID])
}

func TestLocOfStripsConstOffset(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("m")
	e := f.Entry()
	a := e.Alloc(16, 16)
	p := e.Add(ir.ClsL, a, f.Long(8))
	ai := newAliasInfo(f)

	loc := ai.locOf(p, 4)
	assert.Equal(t, int64(8), loc.offset)
	assert.Equal(t, cLocal, loc.class)
	base := ai.locOf(a, 4)
	assert.Equal(t, base.key, loc.key, "derived pointer shares the alloc base")
}

func TestMustAlias(t *testing.T) {
	l := func(key string, off int64, w int) locInfo {
		return locInfo{key: key, class: cLocal, offset: off, offKnown: true, width: w}
	}
	assert.True(t, mustAlias(l("a:1", 0, 4), l("a:1", 0, 4)))
	assert.False(t, mustAlias(l("a:1", 0, 4), l("a:1", 4, 4)), "different offset")
	assert.False(t, mustAlias(l("a:1", 0, 8), l("a:1", 0, 4)), "different width")
	assert.False(t, mustAlias(l("a:1", 0, 4), l("a:2", 0, 4)), "different base")

	unknown := locInfo{key: "?", class: cUnknown, offKnown: false, width: 4}
	assert.False(t, mustAlias(unknown, unknown), "unresolved base never must-aliases")
}

func TestMayAliasAndDistinct(t *testing.T) {
	mk := func(key string, class int, off int64, w int) locInfo {
		return locInfo{key: key, class: class, offset: off, offKnown: true, width: w}
	}
	localA := mk("a:1", cLocal, 0, 4)
	localA2 := mk("a:1", cLocal, 4, 4)
	localB := mk("a:2", cLocal, 0, 4)
	escA := mk("e:1", cEscaped, 0, 8)
	escB := mk("e:2", cEscaped, 0, 8)
	glob1 := mk("g:x", cGlobal, 0, 8)
	glob2 := mk("g:y", cGlobal, 0, 8)
	unkP := mk("t:5", cUnknown, 0, 8)
	unkQ := mk("t:6", cUnknown, 0, 8)

	// Same base: decided by offset overlap.
	assert.True(t, mayAlias(localA, mk("a:1", cLocal, 0, 4)))
	assert.False(t, mayAlias(localA, localA2), "disjoint offsets in same base")
	assert.True(t, mayAlias(mk("a:1", cLocal, 0, 8), mk("a:1", cLocal, 4, 8)), "overlapping offsets")

	// Distinct regions never alias.
	assert.False(t, mayAlias(localA, localB), "two distinct non-escaped allocs")
	assert.False(t, mayAlias(localA, unkP), "unknown can't reach a non-escaped local")
	assert.False(t, mayAlias(escA, escB), "two distinct allocations")
	assert.False(t, mayAlias(escA, glob1), "an allocation is never a global")
	assert.False(t, mayAlias(glob1, glob2), "two distinct globals")

	// Possible aliases must be reported.
	assert.True(t, mayAlias(escA, unkP), "unknown may point into an escaped alloc")
	assert.True(t, mayAlias(glob1, unkP), "unknown may point at a global")
	assert.True(t, mayAlias(unkP, unkQ), "two unknown pointers may alias")
}

func TestLocOfVariants(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("m")
	e := f.Entry()
	a := e.Alloc(16, 32)
	// const-first add (commutative), and subtraction.
	p1 := e.Add(ir.ClsL, f.Long(8), a)
	p2 := e.Sub(ir.ClsL, a, f.Long(4))
	ai := newAliasInfo(f)

	l1 := ai.locOf(p1, 4)
	assert.Equal(t, cLocal, l1.class, "const-first pointer arithmetic tracked")
	assert.Equal(t, int64(8), l1.offset)

	l2 := ai.locOf(p2, 4)
	assert.Equal(t, int64(-4), l2.offset, "subtraction lowers the offset")
	assert.Equal(t, cLocal, l2.class)

	// A raw integer address is opaque; the absent ref is unknown.
	li := ai.locOf(f.Long(4096), 4)
	assert.Equal(t, cUnknown, li.class)
	ln := ai.locOf(ir.R, 4)
	assert.Equal(t, "?", ln.key)
}

func TestLocOfNonConstOffsetEscapes(t *testing.T) {
	// Indexing an alloc by a non-constant makes it escape (we can no longer
	// bound the accessed offset), so it is classed as escaped, not local.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	i := f.Param("i", ir.ClsL)
	e := f.Entry()
	a := e.Alloc(16, 16)
	p := e.Add(ir.ClsL, a, i) // variable offset
	e.Ret(e.Load(ir.ClsW, p))

	ai := newAliasInfo(f)
	assert.True(t, ai.escaped[a.ID], "variable indexing escapes the alloc")
	assert.Equal(t, cEscaped, ai.locOf(a, 4).class)
	// The derived pointer is opaque (unknown base, unknown offset).
	assert.Equal(t, cUnknown, ai.locOf(p, 4).class)
}

func TestTrackedAndMayAliasUnknownOffset(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("m")
	ai := newAliasInfo(f)
	assert.False(t, ai.tracked(f.Word(1)), "a constant is not an alloc-derived pointer")

	// Same base with an unknown offset must be treated as a possible alias.
	a := locInfo{key: "a:1", class: cLocal, offKnown: false, width: 4}
	b := locInfo{key: "a:1", class: cLocal, offset: 100, offKnown: true, width: 4}
	assert.True(t, mayAlias(a, b))
}

func TestLocOfGlobalOffset(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("m")
	e := f.Entry()
	_ = e
	ai := newAliasInfo(f)
	loc := ai.locOf(f.Sym("g", 16), 8)
	assert.Equal(t, cGlobal, loc.class)
	assert.Equal(t, int64(16), loc.offset)
	require.Equal(t, "g:g", loc.key)
}
