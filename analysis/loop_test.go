package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func TestLoopDepthSimple(t *testing.T) {
	f, bl, _ := sumLoop()
	c := BuildCFG(f)
	d := c.LoopDepth(c.Dominators())

	assert.Equal(t, 0, d[bl["start"]])
	assert.Equal(t, 1, d[bl["loop"]])
	assert.Equal(t, 1, d[bl["body"]])
	assert.Equal(t, 0, d[bl["exit"]])
}

func TestLoopDepthNoLoops(t *testing.T) {
	f, bl := diamond()
	c := BuildCFG(f)
	d := c.LoopDepth(c.Dominators())
	for _, name := range []string{"start", "a", "b", "c"} {
		assert.Equal(t, 0, d[bl[name]], name)
	}
}

func TestLoopDepthNested(t *testing.T) {
	// outer: header ho, body with an inner loop hi.
	//   entry -> ho
	//   ho -> hi | done
	//   hi -> hi (self back edge) | ho (outer back edge)
	f := ir.NewModule().NewFuncVoid("nested")
	c := f.Param("c", ir.ClsW)
	entry := f.Entry()
	ho := f.NewBlock("ho")
	hi := f.NewBlock("hi")
	done := f.NewBlock("done")
	entry.Goto(ho)
	ho.Jnz(c, hi, done)
	hi.Jnz(c, hi, ho) // inner self-loop, plus back edge to outer header
	done.RetVoid()

	cfg := BuildCFG(f)
	d := cfg.LoopDepth(cfg.Dominators())
	assert.Equal(t, 0, d[entry])
	assert.Equal(t, 1, d[ho])
	assert.Equal(t, 2, d[hi], "inner header is inside both loops")
	assert.Equal(t, 0, d[done])
}
