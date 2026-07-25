package opt_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func countCalls(m *ir.Module) int {
	n := 0
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				if b.Instrs[i].Op == ir.OCall {
					n++
				}
			}
		}
	}
	return n
}

func hasFunc(m *ir.Module, name string) bool {
	for _, f := range m.Funcs {
		if f.Name == name {
			return true
		}
	}
	return false
}

// addHelper defines add3(x) = x + 3.
func addHelper(m *ir.Module) {
	f := m.NewFunc("add3", ir.ClsW)
	x := f.Param("x", ir.ClsW)
	f.Entry().Ret(f.Entry().Add(ir.ClsW, x, f.Word(3)))
}

// maxHelper defines maxi(a, b) with a branch and a phi-carried return.
func maxHelper(m *ir.Module) {
	f := m.NewFunc("maxi", ir.ClsW)
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	ra, rb := f.NewBlock("ra"), f.NewBlock("rb")
	e.Jnz(e.Cmp(ir.CmpSgt, ir.ClsW, a, b), ra, rb)
	ra.Ret(a)
	rb.Ret(b)
}

func TestInlineStraightLineAndControlFlow(t *testing.T) {
	m := ir.NewModule()
	maxHelper(m)
	addHelper(m)
	mn := m.NewFunc("go", ir.ClsW).Export()
	p := mn.Param("p", ir.ClsW)
	e := mn.Entry()
	t1 := e.Call(ir.ClsW, mn.Sym("add3", 0), p)                // p + 3
	e.Ret(e.Call(ir.ClsW, mn.Sym("maxi", 0), t1, mn.Word(10))) // max(p+3, 10)

	require.Equal(t, 2, countCalls(m))
	opt.OptimizeModule(m)

	assert.Equal(t, 0, countCalls(m), "both calls inlined")
	assert.False(t, hasFunc(m, "add3"), "dead helper removed")
	assert.False(t, hasFunc(m, "maxi"), "dead helper removed")

	// go(p) = max(p+3, 10)
	code := runModule(t, m, `
extern int go(int);
int main(void){ return (go(5) == 10 && go(20) == 23) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestInlineRecordsProvenance(t *testing.T) {
	m := ir.NewModule()
	addHelper(m) // add3(x) = x + 3
	f := m.NewFunc("go", ir.ClsW).Export()
	p := f.Param("p", ir.ClsW)
	fi := m.File("go.src")
	e := f.Entry()
	e.At(ir.SrcPos{File: fi, Line: 7, Col: 2}) // stamps the call
	e.Ret(e.Call(ir.ClsW, f.Sym("add3", 0), p))

	require.True(t, opt.Inline(m))

	// The inlined Add (from add3) records where it came from and where it was
	// called, with no parent (a single, top-level inline).
	var site *ir.InlineSite
	for _, fn := range m.Funcs {
		if fn.Name != "go" {
			continue
		}
		for _, b := range fn.Blocks {
			for i := range b.Instrs {
				if in := &b.Instrs[i]; in.Op == ir.OAdd && in.Inl != nil {
					site = in.Inl
				}
			}
		}
	}
	require.NotNil(t, site, "inlined instruction carries provenance")
	assert.Equal(t, "add3", site.Callee)
	assert.Equal(t, uint32(fi), site.Call.File)
	assert.EqualValues(t, 7, site.Call.Line)
	assert.Nil(t, site.Parent, "a top-level inline has no parent")
}

func TestInlineRecursiveNotInlined(t *testing.T) {
	// A self-recursive callee is never inlined (it would not terminate).
	m := ir.NewModule()
	fac := m.NewFunc("fac", ir.ClsW)
	n := fac.Param("n", ir.ClsW)
	e := fac.Entry()
	rec, base := fac.NewBlock("rec"), fac.NewBlock("base")
	e.Jnz(e.Cmp(ir.CmpSle, ir.ClsW, n, fac.Word(1)), base, rec)
	base.Ret(fac.Word(1))
	sub := rec.Call(ir.ClsW, fac.Sym("fac", 0), rec.Sub(ir.ClsW, n, fac.Word(1)))
	rec.Ret(rec.Mul(ir.ClsW, n, sub))

	mn := m.NewFunc("go", ir.ClsW).Export()
	p := mn.Param("p", ir.ClsW)
	mn.Entry().Ret(mn.Entry().Call(ir.ClsW, mn.Sym("fac", 0), p))

	opt.OptimizeModule(m)
	assert.True(t, countCalls(m) >= 1, "recursive callee not inlined")
	assert.True(t, hasFunc(m, "fac"), "still-called function kept")

	code := runModule(t, m, `
extern int go(int);
int main(void){ return go(5) == 120 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestInlineSkipsIndirectAndOversized(t *testing.T) {
	// Indirect call: the callee is a computed value, not a symbol.
	m := ir.NewModule()
	addHelper(m)
	mn := m.NewFunc("go", ir.ClsW).Export()
	fp := mn.Param("fp", ir.ClsP)
	x := mn.Param("x", ir.ClsW)
	mn.Entry().Ret(mn.Entry().Call(ir.ClsW, fp, x))
	opt.OptimizeModule(m)
	assert.Equal(t, 1, countCalls(m), "indirect call not inlined")

	// Oversized callee: a body past the single-site budget stays a call, even with
	// only one caller (its size outweighs relocating it).
	m2 := ir.NewModule()
	big := m2.NewFunc("big", ir.ClsW)
	bx := big.Param("x", ir.ClsW)
	be := big.Entry()
	acc := bx
	for i := 0; i < 400; i++ {
		acc = be.Add(ir.ClsW, acc, big.Word(int64(i)))
	}
	be.Ret(acc)
	caller := m2.NewFunc("go", ir.ClsW).Export()
	cp := caller.Param("p", ir.ClsW)
	caller.Entry().Ret(caller.Entry().Call(ir.ClsW, caller.Sym("big", 0), cp))
	opt.OptimizeModule(m2)
	assert.Equal(t, 1, countCalls(m2), "oversized callee not inlined")
}

func TestInlineHonorsNoInline(t *testing.T) {
	// A small callee inlines by default; marking it NoInline (from noinline/cold)
	// keeps it a call, so a cold slow-path stays out of the hot inlined body.
	build := func(noinline bool) *ir.Module {
		m := ir.NewModule()
		h := m.NewFunc("h", ir.ClsW)
		hx := h.Param("x", ir.ClsW)
		h.Entry().Ret(h.Entry().Add(ir.ClsW, hx, h.Word(1)))
		h.NoInline = noinline
		mn := m.NewFunc("go", ir.ClsW).Export()
		p := mn.Param("p", ir.ClsW)
		mn.Entry().Ret(mn.Entry().Call(ir.ClsW, mn.Sym("h", 0), p))
		return m
	}

	m1 := build(false)
	opt.OptimizeModule(m1)
	assert.Equal(t, 0, countCalls(m1), "a small callee inlines by default")

	m2 := build(true)
	opt.OptimizeModule(m2)
	assert.Equal(t, 1, countCalls(m2), "a NoInline callee stays a call")
}

func TestInlineCostInlineNoEviction(t *testing.T) {
	// A large CostInline callee (the cost model's pick) is inlined cap-free, and it must
	// NOT consume the budgeted headroom the small helper needs -- both end up inlined.
	// This is the fix for a big forced inline evicting an interpreter's small hot helpers.
	m := ir.NewModule()
	h := m.NewFunc("h", ir.ClsW) // small helper, inlines by default
	hx := h.Param("x", ir.ClsW)
	h.Entry().Ret(h.Entry().Add(ir.ClsW, hx, h.Word(1)))
	big := m.NewFunc("big", ir.ClsW) // large, past the size budget, but cost-model-chosen
	bx := big.Param("x", ir.ClsW)
	be := big.Entry()
	acc := bx
	for i := 0; i < 400; i++ {
		acc = be.Add(ir.ClsW, acc, big.Word(int64(i)))
	}
	be.Ret(acc)
	big.CostInline = true
	c := m.NewFunc("go", ir.ClsW).Export()
	cp := c.Param("p", ir.ClsW)
	t1 := c.Entry().Call(ir.ClsW, c.Sym("big", 0), cp)
	c.Entry().Ret(c.Entry().Call(ir.ClsW, c.Sym("h", 0), t1))

	opt.OptimizeModule(m)
	assert.Equal(t, 0, countCalls(m), "both the large CostInline callee and the small helper are inlined")
}

func TestCostInlineSelectsDispatchHelpers(t *testing.T) {
	// The cost model folds a medium static helper (past the ordinary size budget, so it
	// would otherwise stay a call) into a computed-goto dispatch loop -- an interpreter's
	// fast-path helper into its threaded core. An identically sized helper in ORDINARY code
	// is left alone: the dispatch gate is what distinguishes the interpreter's hot loop.
	medium := func(m *ir.Module, name string) {
		f := m.NewFunc(name, ir.ClsW)
		x := f.Param("x", ir.ClsW)
		e := f.Entry()
		acc := x
		for i := 0; i < 40; i++ { // ~40 instrs: past inlineOnceBudget (24), under maxCostInlineSize (70)
			acc = e.Add(ir.ClsW, acc, f.Word(int64(i)))
		}
		e.Ret(acc)
	}
	build := func() *ir.Module {
		m := ir.NewModule()
		medium(m, "hot")  // called from the dispatch loop -> selected
		medium(m, "cold") // called from ordinary code -> not selected

		// interp: a computed-goto dispatch loop, padded so its 30% growth budget admits hot.
		interp := m.NewFunc("interp", ir.ClsW).Export()
		pc := interp.Param("pc", ir.ClsP)
		x := interp.Param("x", ir.ClsW)
		e := interp.Entry()
		acc := x
		for i := 0; i < 150; i++ {
			acc = e.Add(ir.ClsW, acc, interp.Word(int64(i)))
		}
		h1, h2 := interp.NewBlock("h1"), interp.NewBlock("h2")
		e.BrIndirect(pc, h1, h2) // JmpBr: the dispatch
		h1.Ret(h1.Call(ir.ClsW, interp.Sym("hot", 0), acc))
		h2.Ret(acc)

		// plain: ordinary (non-dispatch) code calling the identical cold helper.
		plain := m.NewFunc("plain", ir.ClsW).Export()
		p := plain.Param("p", ir.ClsW)
		plain.Entry().Ret(plain.Entry().Call(ir.ClsW, plain.Sym("cold", 0), p))
		return m
	}

	m := build()
	opt.OptimizeModule(m)
	assert.False(t, hasFunc(m, "hot"), "medium helper folded into the dispatch loop")
	assert.True(t, hasFunc(m, "cold"), "identical helper in ordinary code left a call")

	// CG12_NO_COSTINLINE turns the selector off: hot stays a call, like cold.
	t.Setenv("CG12_NO_COSTINLINE", "1")
	m2 := build()
	opt.OptimizeModule(m2)
	assert.True(t, hasFunc(m2, "hot"), "selector disabled: dispatch helper stays a call")
	assert.True(t, hasFunc(m2, "cold"), "selector disabled: ordinary helper stays a call")
}

func TestInlineAggregateReturn(t *testing.T) {
	// An aggregate-returning callee inlines: spliceCall materializes the struct
	// copy-at-return the ABI would emit, so the result is correct and the call is gone.
	build := func() *ir.Module {
		m := ir.NewModule()
		pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
		m.AddType(pair)
		// mk(x) returns {x+1, x*2}
		mk := m.NewFuncVoid("mk")
		mk.HasRet = true
		mk.RetAgg = pair
		mx := mk.Param("x", ir.ClsW)
		me := mk.Entry()
		buf := me.Alloc(16, 8)
		me.Store(me.Add(ir.ClsW, mx, mk.Word(1)), buf)
		me.Store(me.Mul(ir.ClsW, mx, mk.Word(2)), me.Add(ir.ClsL, buf, mk.Long(4)))
		me.Ret(buf)
		// go(x) = mk(x).lo + mk(x).hi = (x+1) + (x*2) = 3x+1
		g := m.NewFunc("go", ir.ClsW).Export()
		gx := g.Param("x", ir.ClsW)
		ge := g.Entry()
		r := ge.Call(ir.ClsL, g.Sym("mk", 0), gx)
		ge.Instrs[len(ge.Instrs)-1].RetAgg = pair
		lo := ge.Load(ir.ClsW, r)
		hi := ge.Load(ir.ClsW, ge.Add(ir.ClsL, r, g.Long(4)))
		ge.Ret(ge.Add(ir.ClsW, lo, hi))
		return m
	}

	m := build()
	opt.OptimizeModule(m)
	assert.Equal(t, 0, countCalls(m), "the aggregate-returning call is inlined")
	assert.False(t, hasFunc(m, "mk"), "the dead callee is removed")

	code := runModule(t, m, `
extern int go(int);
int main(void){ return (go(5) == 16 && go(10) == 31) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestInlineNoInlineBeatsForceInline(t *testing.T) {
	// NoInline takes precedence if a function somehow carries both marks.
	m := ir.NewModule()
	h := m.NewFunc("h", ir.ClsW)
	hx := h.Param("x", ir.ClsW)
	h.Entry().Ret(h.Entry().Add(ir.ClsW, hx, h.Word(1)))
	h.ForceInline = true
	h.NoInline = true
	mn := m.NewFunc("go", ir.ClsW).Export()
	p := mn.Param("p", ir.ClsW)
	mn.Entry().Ret(mn.Entry().Call(ir.ClsW, mn.Sym("h", 0), p))

	opt.OptimizeModule(m)
	assert.Equal(t, 1, countCalls(m), "NoInline wins over ForceInline")
}

func TestDeadFuncElim(t *testing.T) {
	m := ir.NewModule()
	// used: called by exported main. dead: never referenced. keep: exported.
	used := m.NewFunc("used", ir.ClsW)
	ux := used.Param("x", ir.ClsW)
	used.Entry().Ret(used.Entry().Add(ir.ClsW, ux, used.Word(1)))
	dead := m.NewFunc("dead", ir.ClsW)
	dx := dead.Param("x", ir.ClsW)
	dead.Entry().Ret(dx)
	keep := m.NewFunc("keep", ir.ClsW).Export()
	kx := keep.Param("x", ir.ClsW)
	keep.Entry().Ret(kx)
	mn := m.NewFunc("main2", ir.ClsW).Export()
	mp := mn.Param("p", ir.ClsW)
	// Reference `used` indirectly (as a function pointer) so inlining can't erase
	// the reference and DeadFuncElim must keep it.
	mn.Entry().Ret(mn.Entry().Call(ir.ClsW, mn.Sym("used", 0), mp))

	changed := opt.DeadFuncElim(m)
	assert.True(t, changed)
	assert.False(t, hasFunc(m, "dead"), "unreferenced non-exported function removed")
	assert.True(t, hasFunc(m, "keep"), "exported function kept")
	assert.True(t, hasFunc(m, "used"), "referenced function kept")
}

func TestInlineConstantKinds(t *testing.T) {
	// A callee whose constant pool holds a symbol, a double, and a single, so
	// inlining clones all three constant kinds.
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name: "tbl", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{100}}},
	})
	h := m.NewFunc("h", ir.ClsW)
	x := h.Param("x", ir.ClsW)
	e := h.Entry()
	g := e.Load(ir.ClsW, h.Sym("tbl", 0)) // 100, a ConstSym
	s := e.Add(ir.ClsW, x, g)
	df := e.Mul(ir.ClsD, e.Sltof(ir.ClsD, e.Extsw(ir.ClsL, s)), h.Double(1.0)) // ClsD const
	sf := e.Add(ir.ClsS, e.Truncd(df), h.Single(0.0))                          // ClsS const
	e.Ret(e.Stosi(ir.ClsW, sf))

	mn := m.NewFunc("go", ir.ClsW).Export()
	p := mn.Param("p", ir.ClsW)
	mn.Entry().Ret(mn.Entry().Call(ir.ClsW, mn.Sym("h", 0), p))

	opt.OptimizeModule(m)
	assert.Equal(t, 0, countCalls(m), "callee inlined, all constant kinds cloned")
	code := runModule(t, m, `
extern int go(int);
int main(void){ return go(5) == 105 ? 0 : 1; }`) // (5+100) round-tripped through float
	assert.Equal(t, 0, code)
}

func TestInlinePreservesThreadLocalSymbol(t *testing.T) {
	// A callee that reads a thread-local through a ThreadSym constant (an
	// interpreter's per-thread state, reached via an inlined accessor). When it is
	// inlined, the cloned symbol must stay thread-local, or its reference is emitted
	// with a non-TLS relocation and the link fails against the real TLS definition.
	m := ir.NewModule()
	h := m.NewFunc("readtls", ir.ClsL)
	e := h.Entry()
	e.Ret(e.Load(ir.ClsL, h.ThreadSym("tlsvar")))

	mn := m.NewFunc("go", ir.ClsL).Export()
	mn.Entry().Ret(mn.Entry().Call(ir.ClsL, mn.Sym("readtls", 0)))

	opt.OptimizeModule(m)
	require.Equal(t, 0, countCalls(m), "callee inlined")

	var found, thread bool
	for _, f := range m.Funcs {
		if f.Name != "go" {
			continue
		}
		for i := range f.Consts {
			if c := f.Consts[i]; c.Kind == ir.ConstSym && c.Sym == "tlsvar" {
				found, thread = true, c.Thread
			}
		}
	}
	require.True(t, found, "the thread-local symbol survived inlining")
	assert.True(t, thread, "and stayed thread-local")
}

func TestInlineVoidCall(t *testing.T) {
	// A void callee: inlining leaves no result phi.
	m := ir.NewModule()
	st := m.NewFuncVoid("store1")
	p, v := st.Param("p", ir.ClsP), st.Param("v", ir.ClsW)
	se := st.Entry()
	se.Store(se.Add(ir.ClsW, v, st.Word(1)), p)
	se.RetVoid()

	mn := m.NewFunc("go", ir.ClsW).Export()
	x := mn.Param("x", ir.ClsW)
	me := mn.Entry()
	buf := me.Alloc(4, 4)
	me.CallVoid(mn.Sym("store1", 0), buf, x)
	me.Ret(me.Load(ir.ClsW, buf))

	opt.OptimizeModule(m)
	assert.Equal(t, 0, countCalls(m), "void call inlined")
	code := runModule(t, m, `
extern int go(int);
int main(void){ return go(41) == 42 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestInlinePreservesVolatile(t *testing.T) {
	// A callee with a volatile store: cloning it into the caller must keep the
	// store volatile, or the observable write can be reordered or eliminated once
	// it is inlined. cloneInstr used to drop the flag.
	m := ir.NewModule()
	st := m.NewFuncVoid("vstore")
	p, v := st.Param("p", ir.ClsP), st.Param("v", ir.ClsW)
	se := st.Entry()
	se.Store(v, p)
	se.Instrs[len(se.Instrs)-1].Volatile = true // mark the store volatile
	se.RetVoid()

	mn := m.NewFunc("go", ir.ClsW).Export()
	x := mn.Param("x", ir.ClsW)
	me := mn.Entry()
	buf := me.Alloc(4, 4)
	me.CallVoid(mn.Sym("vstore", 0), buf, x)
	me.Ret(me.Load(ir.ClsW, buf))

	require.True(t, opt.Inline(m))

	var found, vol bool
	for _, f := range m.Funcs {
		if f.Name != "go" {
			continue
		}
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				if in := &b.Instrs[i]; in.Op == ir.OStorew {
					found, vol = true, in.Volatile
				}
			}
		}
	}
	require.True(t, found, "inlined store present in caller")
	assert.True(t, vol, "inlined store stayed volatile")
}

func TestUnrollMutualRecursion(t *testing.T) {
	// iseven <-> isodd is a two-function recursion cycle. Bounded unrolling
	// expands the cycle a fixed number of levels and leaves a residual recursive
	// call, so the program still terminates and stays correct.
	m := ir.NewModule()

	parity := func(name, other string, base int64) {
		f := m.NewFunc(name, ir.ClsW)
		n := f.Param("n", ir.ClsW)
		e := f.Entry()
		ret, rec := f.NewBlock("ret"), f.NewBlock("rec")
		e.Jnz(e.Cmp(ir.CmpEq, ir.ClsW, n, f.Word(0)), ret, rec)
		ret.Ret(f.Word(base))
		rec.Ret(rec.Call(ir.ClsW, f.Sym(other, 0), rec.Sub(ir.ClsW, n, f.Word(1))))
	}
	parity("iseven", "isodd", 1)
	parity("isodd", "iseven", 0)

	mn := m.NewFunc("test", ir.ClsW).Export()
	p := mn.Param("p", ir.ClsW)
	mn.Entry().Ret(mn.Entry().Call(ir.ClsW, mn.Sym("iseven", 0), p))

	opt.OptimizeModule(m)

	// Unrolling stays bounded (a residual recursive call always remains) and
	// preserves semantics; termination is implied by the run completing.
	assert.LessOrEqual(t, countCalls(m), 30, "unrolling bounded, not runaway")
	assert.GreaterOrEqual(t, countCalls(m), 1, "recursion not fully eliminated")

	code := runModule(t, m, `
extern int test(int);
int main(void){ return (test(4) == 1 && test(5) == 0 && test(7) == 0) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestUnrollDirectRecursion(t *testing.T) {
	// fac calls itself; bounded unrolling expands the recursion in place (more
	// multiplies) while leaving a residual call so it still terminates.
	m := ir.NewModule()
	fac := m.NewFunc("fac", ir.ClsW).Export()
	n := fac.Param("n", ir.ClsW)
	e := fac.Entry()
	rec, base := fac.NewBlock("rec"), fac.NewBlock("base")
	e.Jnz(e.Cmp(ir.CmpSle, ir.ClsW, n, fac.Word(1)), base, rec)
	base.Ret(fac.Word(1))
	sub := rec.Call(ir.ClsW, fac.Sym("fac", 0), rec.Sub(ir.ClsW, n, fac.Word(1)))
	rec.Ret(rec.Mul(ir.ClsW, n, sub))

	opt.OptimizeModule(m)

	muls := 0
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				if b.Instrs[i].Op == ir.OMul {
					muls++
				}
			}
		}
	}
	assert.Greater(t, muls, 1, "recursion unrolled in place")
	assert.GreaterOrEqual(t, countCalls(m), 1, "residual recursive call kept")

	code := runModule(t, m, `
extern int fac(int);
int main(void){ return (fac(5) == 120 && fac(6) == 720 && fac(1) == 1) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}
