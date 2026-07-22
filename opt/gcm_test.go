package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func blockHasOp(b *ir.Block, op ir.Op) bool {
	for _, in := range b.Instrs {
		if in.Op == op {
			return true
		}
	}
	return false
}

// buildInvariantLoop returns a loop that computes a*b (loop-invariant) and
// accumulates (a*b + i) into s.
func buildInvariantLoop() (*ir.Func, map[string]*ir.Block) {
	f := ir.NewModule().NewFunc("acc", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	n := f.Param("n", ir.ClsW)
	entry := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")

	entry.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)

	inv := body.Mul(ir.ClsW, a, b) // loop-invariant
	t := body.Add(ir.ClsW, s, inv)
	s2 := body.Add(ir.ClsW, t, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)

	return f, map[string]*ir.Block{"entry": entry, "loop": loop, "body": body, "exit": exit}
}

func TestGCMHoistsLoopInvariant(t *testing.T) {
	f, bl := buildInvariantLoop()
	assert.True(t, blockHasOp(bl["body"], ir.OMul), "precondition: mul starts in the loop body")

	assert.True(t, GCM(f))
	assert.True(t, blockHasOp(bl["entry"], ir.OMul), "invariant mul hoisted to the pre-header")
	assert.False(t, blockHasOp(bl["body"], ir.OMul), "invariant mul left the loop body")
	assert.False(t, blockHasOp(bl["loop"], ir.OMul), "invariant mul not in the loop header either")
}

func TestGCMNoMotionStraightLine(t *testing.T) {
	f := ir.NewModule().NewFunc("s", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b)) // nowhere better to put it
	assert.False(t, GCM(f))
}

func TestGCMNoMovable(t *testing.T) {
	// Only memory ops: nothing to schedule.
	f := ir.NewModule().NewFuncVoid("m")
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(1), p)
	e.RetVoid()
	assert.False(t, GCM(f))
}

func TestGCMPreservesFixedRegisterMoveAtCallSite(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("closure_call")
	condition := f.Param("condition", ir.ClsW)
	closure := f.Param("closure", ir.ClsP)
	entry := f.Entry()
	invoke := f.NewBlock("invoke")
	done := f.NewBlock("done")
	entry.Jnz(condition, invoke, done)

	context := invoke.Copy(ir.ClsP, closure)
	contextTemporary := f.Temp(context)
	contextTemporary.Fixed = true
	contextTemporary.Reg = 26
	invoke.CallVoid(f.Sym("call", 0))
	call := &invoke.Instrs[1]
	call.ClosureCall = true
	call.ClosureContext = context
	invoke.Goto(done)
	done.RetVoid()

	GCM(f)
	assert.False(t, blockHasOp(entry, ir.OCopy))
	assert.True(t, blockHasOp(invoke, ir.OCopy), "fixed register setup must remain beside its call")
}

func TestGCMEmptyFunction(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("empty") // no blocks
	assert.False(t, GCM(f))
}

func TestGCMDropsDeadValue(t *testing.T) {
	f := ir.NewModule().NewFunc("d", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Add(ir.ClsW, a, b) // dead: never used
	e.Ret(a)

	assert.True(t, GCM(f), "dropping a dead movable counts as a change")
	assert.Equal(t, 0, countOp(f, ir.OAdd), "dead value removed")
}

func TestGCMPinnedLoadAndBranchedUses(t *testing.T) {
	// A movable value used at two different dominator depths, alongside a pinned
	// load (non-eligible result) — exercises LCA and the fixed-def path.
	f := ir.NewModule().NewFunc("g", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	c := f.Param("c", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	entry := f.Entry()
	A := f.NewBlock("A")
	B := f.NewBlock("B")
	C := f.NewBlock("C")
	end := f.NewBlock("end")

	k := entry.Add(ir.ClsW, a, b) // movable
	lv := entry.Load(ir.ClsW, p)  // pinned, has a result
	entry.Jnz(c, A, B)
	A.Jnz(c, C, B)
	C.Ret(C.Add(ir.ClsW, k, lv)) // deep use of k
	B.Goto(end)
	end.Ret(end.Add(ir.ClsW, k, b)) // shallower use of k

	GCM(f)
	assert.Equal(t, 3, countOp(f, ir.OAdd), "k plus the two using adds remain")
	assert.Equal(t, 1, countOp(f, ir.OLoaduw), "pinned load untouched")
}

func TestGCMLCADifferentDepths(t *testing.T) {
	// k is used at two different dominator depths, forcing the LCA to climb.
	f := ir.NewModule().NewFuncVoid("g")
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	c := f.Param("c", ir.ClsW)
	p := f.Param("p", ir.ClsL)
	entry := f.Entry()
	L1 := f.NewBlock("L1")
	L2 := f.NewBlock("L2")
	X := f.NewBlock("X")

	k := entry.Add(ir.ClsW, a, b)
	entry.Jnz(c, L1, X)
	L1.Jnz(k, L2, X) // shallow use of k (depth 1)
	L2.Store(k, p)   // deep use of k (depth 2)
	L2.Goto(X)
	X.RetVoid()

	assert.True(t, GCM(f), "k sinks out of the entry toward its uses")
	assert.Equal(t, 1, countOp(f, ir.OAdd))
	assert.False(t, blockHasOp(entry, ir.OAdd), "k no longer in the entry")
}

func TestGCMPreservesStoreOrderAndDeps(t *testing.T) {
	// Two computations feed two stores; GCM must keep stores ordered and each
	// computed value before its store.
	f := ir.NewModule().NewFuncVoid("m")
	p := f.Param("p", ir.ClsL)
	q := f.Param("q", ir.ClsL)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	x := e.Add(ir.ClsW, a, b)
	e.Store(x, p)
	y := e.Mul(ir.ClsW, a, b)
	e.Store(y, q)
	e.RetVoid()

	GCM(f)
	// Verify every value is defined before use within the block.
	defined := map[uint32]bool{
		uint32(a.ID): true, uint32(b.ID): true,
		uint32(p.ID): true, uint32(q.ID): true,
	}
	for _, in := range e.Instrs {
		for _, arg := range in.Args {
			if arg.Kind == ir.RefTemp {
				assert.Truef(t, defined[arg.ID], "operand used before def: %v", arg)
			}
		}
		if in.To.Kind == ir.RefTemp {
			defined[in.To.ID] = true
		}
	}
	// Store order preserved: store to p precedes store to q.
	var order []uint32
	for _, in := range e.Instrs {
		if in.Op.IsStore() {
			order = append(order, in.Args[1].ID)
		}
	}
	assert.Equal(t, []uint32{uint32(p.ID), uint32(q.ID)}, order)
}
