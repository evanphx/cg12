package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

// sumLoop builds an accumulating loop and returns its blocks and the key value
// refs, so liveness/dominance can be asserted precisely:
//
//	@start        jmp @loop
//	@loop         i = phi [0, start] [i2, body]
//	              s = phi [0, start] [s2, body]
//	              c = i >= n ; jnz c, @exit, @body
//	@body         s2 = s + i ; i2 = i + 1 ; jmp @loop
//	@exit         ret s
func sumLoop() (*ir.Func, map[string]*ir.Block, map[string]ir.Ref) {
	f := ir.NewModule().NewFunc("sum", ir.ClsW)
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")

	start.Goto(loop)

	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	c := loop.Cmp(ir.CmpSge, ir.ClsW, i, n)
	loop.Jnz(c, exit, body)

	s2 := body.Add(ir.ClsW, s, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2) // close i's back-edge
	loop.Phis[1].Add(body, s2) // close s's back-edge

	exit.Ret(s)

	blocks := map[string]*ir.Block{"start": start, "loop": loop, "body": body, "exit": exit}
	refs := map[string]ir.Ref{"n": n, "i": i, "s": s, "c": c, "s2": s2, "i2": i2}
	return f, blocks, refs
}

func TestLivenessSumLoop(t *testing.T) {
	f, bl, r := sumLoop()
	c := BuildCFG(f)
	l := c.Liveness()

	id := func(name string) int { return int(r[name].ID) }

	// Only the parameter n is live entering the function.
	assert.True(t, l.LiveIn(bl["start"]).Has(id("n")))
	assert.Equal(t, 1, l.LiveIn(bl["start"]).Len())

	// n is carried across the whole loop; the phi results are defined in @loop
	// and therefore are not live-in there.
	assert.True(t, l.LiveIn(bl["loop"]).Has(id("n")))
	assert.False(t, l.LiveIn(bl["loop"]).Has(id("i")))
	assert.False(t, l.LiveIn(bl["loop"]).Has(id("s")))

	// Live-out of @loop = {n, i, s}: i and s reach @body, s also reaches @exit.
	out := l.LiveOut(bl["loop"])
	assert.True(t, out.Has(id("n")))
	assert.True(t, out.Has(id("i")))
	assert.True(t, out.Has(id("s")))

	// @body uses i and s and produces the next iteration's values.
	assert.True(t, l.LiveIn(bl["body"]).Has(id("i")))
	assert.True(t, l.LiveIn(bl["body"]).Has(id("s")))
	assert.True(t, l.LiveIn(bl["body"]).Has(id("n")))

	// The updated values are live out of @body (they feed the phis); the old
	// phi values are dead after @body.
	bout := l.LiveOut(bl["body"])
	assert.True(t, bout.Has(id("i2")))
	assert.True(t, bout.Has(id("s2")))
	assert.False(t, bout.Has(id("i")))

	// @exit only needs the accumulator.
	assert.True(t, l.LiveIn(bl["exit"]).Has(id("s")))
	assert.Equal(t, 1, l.LiveIn(bl["exit"]).Len())
}

func TestLivenessMultiRegisterCallAndReturn(t *testing.T) {
	// A call that defines two registers (To + Defs), both live to a return that
	// uses them (Arg + Args). They are defined in the block, so not live-in.
	f := ir.NewModule().NewFuncVoid("m")
	t1 := f.NewTemp("t1", ir.ClsL)
	t2 := f.NewTemp("t2", ir.ClsL)
	e := f.Entry()
	e.Instrs = []ir.Instr{{
		Op:   ir.OCall,
		To:   t1,
		Defs: []ir.Ref{t2},
		Args: []ir.Ref{f.Sym("g", 0)},
	}}
	e.Jmp = ir.Jmp{Kind: ir.JmpRet, Arg: t1, Args: []ir.Ref{t2}}

	l := BuildCFG(f).Liveness()
	assert.False(t, l.LiveIn(e).Has(int(t1.ID)), "t1 is defined in the block")
	assert.False(t, l.LiveIn(e).Has(int(t2.ID)), "t2 is defined via Defs in the block")
}

func TestLivenessStraightLine(t *testing.T) {
	// %r = a + b ; ret %r  — a and b live-in, r dead after use.
	f := ir.NewModule().NewFunc("add", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	sum := e.Add(ir.ClsW, a, b)
	e.Ret(sum)

	l := BuildCFG(f).Liveness()
	in := l.LiveIn(e)
	assert.True(t, in.Has(int(a.ID)))
	assert.True(t, in.Has(int(b.ID)))
	assert.False(t, in.Has(int(sum.ID)))
	assert.Empty(t, l.LiveOut(e).Members())
}
