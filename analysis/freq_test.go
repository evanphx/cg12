package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func freqOf(f *ir.Func) (*CFG, *Freq) {
	c := BuildCFG(f)
	return c, c.Frequency(c.Dominators())
}

func TestFrequencyDiamond(t *testing.T) {
	f, bl := diamond()
	_, fr := freqOf(f)
	assert.InDelta(t, 1.0, fr.Of(bl["start"]), 1e-9)
	assert.InDelta(t, 0.5, fr.Of(bl["a"]), 1e-9)
	assert.InDelta(t, 0.5, fr.Of(bl["b"]), 1e-9)
	assert.InDelta(t, 1.0, fr.Of(bl["c"]), 1e-9)
}

// A __builtin_expect hint biases an otherwise-even diamond toward its likely arm.
func TestFrequencyExpect(t *testing.T) {
	f := NewModuleFunc()
	c := f.Param("c", ir.ClsW)
	entry := f.Entry()
	hot, cold, join := f.NewBlock("hot"), f.NewBlock("cold"), f.NewBlock("join")
	entry.Jnz(c, hot, cold)
	entry.Jmp.Likely = ir.LikelyTo // the source says the To (hot) arm is taken
	hot.Goto(join)
	cold.Goto(join)
	join.RetVoid()

	_, fr := freqOf(f)
	assert.InDelta(t, 0.9, fr.Of(hot), 1e-9)
	assert.InDelta(t, 0.1, fr.Of(cold), 1e-9)
}

func TestFrequencySumLoop(t *testing.T) {
	f, bl, _ := sumLoop()
	_, fr := freqOf(f)
	// loop stays with p=0.9 → trip count 10; body runs 9×, exit once.
	assert.InDelta(t, 1.0, fr.Of(bl["start"]), 1e-9)
	assert.InDelta(t, 10.0, fr.Of(bl["loop"]), 1e-9)
	assert.InDelta(t, 9.0, fr.Of(bl["body"]), 1e-9)
	assert.InDelta(t, 1.0, fr.Of(bl["exit"]), 1e-9)
}

// A doubly-nested loop: the inner header runs outer_trips × inner_trips = 100×.
func TestFrequencyNestedLoop(t *testing.T) {
	f := NewModuleFunc()
	c := f.Param("c", ir.ClsW)
	entry := f.Entry()
	oh := f.NewBlock("oh") // outer header, unconditionally enters the inner region
	ih := f.NewBlock("ih") // inner header
	ib := f.NewBlock("ib") // inner body / latch
	ox := f.NewBlock("ox") // outer latch
	done := f.NewBlock("done")
	entry.Goto(oh)
	oh.Goto(ih)
	ih.Jnz(c, ox, ib) // exit inner to ox, else stay in ib
	ib.Goto(ih)
	ox.Jnz(c, done, oh) // loop outer back to oh, else done
	done.RetVoid()

	_, fr := freqOf(f)
	assert.InDelta(t, 10.0, fr.Of(oh), 1e-6)
	assert.InDelta(t, 100.0, fr.Of(ih), 1e-6)
	assert.Greater(t, fr.Of(ih), fr.Of(oh))
}

// The interpreter shape: a dispatch loop whose header switches to N handlers, each
// jumping back. Each handler is ~1/N as frequent as the header — the distinction
// that makes a rarely-hit opcode's call cheap to caller-save. This is the
// load-bearing test for the whole cost-model effort.
func TestFrequencyDispatchSwitch(t *testing.T) {
	const n = 10
	f := NewModuleFunc()
	sel := f.Param("op", ir.ClsW)
	entry := f.Entry()
	h := f.NewBlock("h")
	exit := f.NewBlock("exit")
	entry.Goto(h)
	handlers := make([]*ir.Block, n)
	cases := make([]ir.SwitchCase, n)
	for i := range handlers {
		hi := f.NewBlock("h")
		hi.Goto(h) // handler jumps back to the dispatch header
		handlers[i] = hi
		cases[i] = ir.SwitchCase{Val: int64(i), Blk: hi}
	}
	h.Switch(sel, exit, true, cases)
	exit.RetVoid()

	_, fr := freqOf(f)
	header := fr.Of(h)
	assert.Greater(t, header, 50.0, "dispatch header is hot")
	for _, hi := range handlers {
		assert.Less(t, fr.Of(hi), header*0.15, "each handler is ~1/N of the header (cold)")
		assert.Greater(t, fr.Of(hi), 0.0)
	}
}

// A branch straight to a trap is treated as almost never taken.
func TestFrequencyBranchToHalt(t *testing.T) {
	f := NewModuleFunc()
	c := f.Param("c", ir.ClsW)
	entry := f.Entry()
	trap := f.NewBlock("trap")
	ok := f.NewBlock("ok")
	entry.Jnz(c, trap, ok) // true → trap
	trap.Hlt()
	ok.RetVoid()

	_, fr := freqOf(f)
	assert.InDelta(t, 0.001, fr.Of(trap), 1e-9)
	assert.InDelta(t, 0.999, fr.Of(ok), 1e-9)
}

// A jnz whose two arms are the same block is a genuine double edge: the target's
// frequency equals the predecessor's (both edges sum in).
func TestFrequencyDoubleEdge(t *testing.T) {
	f := NewModuleFunc()
	c := f.Param("c", ir.ClsW)
	entry := f.Entry()
	t2 := f.NewBlock("t")
	entry.Jnz(c, t2, t2)
	t2.RetVoid()
	_, fr := freqOf(f)
	assert.InDelta(t, 1.0, fr.Of(t2), 1e-9)
}

// Every reachable block's edge probabilities sum to 1.
func TestFrequencyEdgeProbsSumToOne(t *testing.T) {
	for _, mk := range []func() *ir.Func{
		func() *ir.Func { f, _ := diamond(); return f },
		func() *ir.Func { f, _, _ := sumLoop(); return f },
	} {
		c, fr := freqOf(mk())
		for _, b := range c.RPO {
			if len(b.Succs()) == 0 {
				continue
			}
			sum := 0.0
			for i := range b.Succs() {
				sum += fr.Prob(b, i)
			}
			assert.InDelta(t, 1.0, sum, 1e-9)
		}
	}
}

// An irreducible CFG (two blocks entering each other's middle via computed goto)
// must not diverge or panic; every frequency is finite and non-negative.
func TestFrequencyIrreducibleTerminates(t *testing.T) {
	f := NewModuleFunc()
	c := f.Param("c", ir.ClsW)
	entry := f.Entry()
	x := f.NewBlock("x")
	y := f.NewBlock("y")
	done := f.NewBlock("done")
	entry.Jnz(c, x, y) // enter the cycle at either x or y — no single header
	x.Jnz(c, y, done)
	y.Jnz(c, x, done)
	done.RetVoid()

	c2, fr := freqOf(f)
	for _, b := range c2.RPO {
		v := fr.Of(b)
		assert.False(t, v < 0)
		assert.False(t, v > maxFreq*2, "finite")
	}
}

func TestFrequencyUnreachableIsZero(t *testing.T) {
	f := NewModuleFunc()
	entry := f.Entry()
	dead := f.NewBlock("dead")
	entry.RetVoid()
	dead.RetVoid() // never reached
	_, fr := freqOf(f)
	assert.Equal(t, 0.0, fr.Of(dead))
}
