package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadElimStoreForwarding(t *testing.T) {
	// store 5, p ; r = load p ; ret r  ->  ret 5
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(5), p)
	e.Ret(e.Load(ir.ClsW, p))

	assert.True(t, LoadElim(f))
	assert.Equal(t, 0, countOp(f, ir.OLoaduw), "load replaced by the stored value")
	v, ok := constInt(f, e.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(5), v)
}

func TestLoadElimLoadToLoad(t *testing.T) {
	// a = load p ; b = load p ; ret a+b  ->  one load
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	a := e.Load(ir.ClsW, p)
	b := e.Load(ir.ClsW, p)
	e.Ret(e.Add(ir.ClsW, a, b))

	assert.True(t, LoadElim(f))
	assert.Equal(t, 1, countOp(f, ir.OLoaduw))
}

func TestLoadElimLastStoreWins(t *testing.T) {
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(1), p)
	e.Store(f.Word(2), p)
	e.Ret(e.Load(ir.ClsW, p))

	LoadElim(f)
	v, ok := constInt(f, e.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(2), v, "later store forwarded")
}

func TestLoadElimCallClobbers(t *testing.T) {
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(5), p)
	e.CallVoid(f.Sym("g", 0))
	e.Ret(e.Load(ir.ClsW, p))

	assert.False(t, LoadElim(f), "a call may write memory, so the load stays")
	assert.Equal(t, 1, countOp(f, ir.OLoaduw))
}

func TestLoadElimMayAliasBlocksForwarding(t *testing.T) {
	// Two unknown pointers may alias, so a store through q invalidates p.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	q := f.Param("q", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(1), p)
	e.Store(f.Word(2), q)
	e.Ret(e.Load(ir.ClsW, p))

	assert.False(t, LoadElim(f), "may-aliasing store prevents forwarding")
	assert.Equal(t, 1, countOp(f, ir.OLoaduw))
}

func TestLoadElimDistinctLocalsForward(t *testing.T) {
	// Two distinct non-escaping allocs: a store to one doesn't clobber the other.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	e := f.Entry()
	a := e.Alloc(4, 4)
	b := e.Alloc(4, 4)
	e.Store(f.Word(1), a)
	e.Store(f.Word(2), b) // does not alias a
	e.Ret(e.Load(ir.ClsW, a))

	assert.True(t, LoadElim(f))
	v, ok := constInt(f, e.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(1), v)
}

func TestLoadElimDisjointOffsetsInSameAlloc(t *testing.T) {
	// Writes at offset 0 and offset 4 of the same alloc don't clobber each other.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	e := f.Entry()
	a := e.Alloc(8, 8)
	e.Store(f.Word(1), a)
	hi := e.Add(ir.ClsL, a, f.Long(4))
	e.Store(f.Word(2), hi)
	e.Ret(e.Load(ir.ClsW, a))

	assert.True(t, LoadElim(f))
	v, ok := constInt(f, e.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(1), v)
}

func TestLoadElimExtendedBlock(t *testing.T) {
	// Availability flows into a single-predecessor successor.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	entry := f.Entry()
	next := f.NewBlock("next")
	entry.Store(f.Word(9), p)
	entry.Goto(next)
	next.Ret(next.Load(ir.ClsW, p))

	assert.True(t, LoadElim(f))
	v, ok := constInt(f, next.Jmp.Arg)
	require.True(t, ok)
	assert.Equal(t, int64(9), v)
}

func TestLoadElimMergeResets(t *testing.T) {
	// A block with two predecessors does not inherit availability.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	c := f.Param("c", ir.ClsW)
	entry := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	join := f.NewBlock("join")
	entry.Jnz(c, a, b)
	a.Store(f.Word(1), p)
	a.Goto(join)
	b.Store(f.Word(2), p)
	b.Goto(join)
	join.Ret(join.Load(ir.ClsW, p)) // must not forward either store

	assert.False(t, LoadElim(f))
	assert.Equal(t, 1, countOp(f, ir.OLoaduw))
}

func TestLoadElimSubwordStoreNotForwarded(t *testing.T) {
	// A byte store cannot forward its value to a full-word load.
	f := ir.NewModule().NewFunc("m", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.StoreSub(ir.SubB, x, p)
	e.Ret(e.Load(ir.ClsW, p))

	assert.False(t, LoadElim(f))
}

func TestAccessWidthAndCanonicalLoad(t *testing.T) {
	width := map[ir.Op]int{
		ir.OLoadsb: 1, ir.OLoadub: 1, ir.OStoreb: 1,
		ir.OLoadsh: 2, ir.OLoaduh: 2, ir.OStoreh: 2,
		ir.OLoadsw: 4, ir.OLoaduw: 4, ir.OLoads: 4, ir.OStorew: 4, ir.OStores: 4,
		ir.OLoadl: 8, ir.OLoadd: 8, ir.OStorel: 8, ir.OStored: 8,
	}
	for op, w := range width {
		assert.Equalf(t, w, accessWidth(op), op.String())
	}
	assert.Equal(t, 0, accessWidth(ir.OAdd), "non-memory op has no access width")

	canon := map[ir.Op]ir.Op{
		ir.OStorew: ir.OLoaduw, ir.OStorel: ir.OLoadl,
		ir.OStores: ir.OLoads, ir.OStored: ir.OLoadd,
	}
	for st, ld := range canon {
		lop, ok := canonicalLoadOp(st)
		assert.Truef(t, ok, st.String())
		assert.Equal(t, ld, lop)
	}
	for _, st := range []ir.Op{ir.OStoreb, ir.OStoreh} {
		_, ok := canonicalLoadOp(st)
		assert.Falsef(t, ok, "%s has no canonical load", st.String())
	}
}
