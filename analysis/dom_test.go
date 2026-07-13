package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func TestDominatorsDiamond(t *testing.T) {
	f, bl := diamond()
	d := BuildCFG(f).Dominators()

	assert.Nil(t, d.Idom[bl["start"]])
	assert.Same(t, bl["start"], d.Idom[bl["a"]])
	assert.Same(t, bl["start"], d.Idom[bl["b"]])
	// The join is dominated by start, not by either arm.
	assert.Same(t, bl["start"], d.Idom[bl["c"]])

	assert.True(t, d.Dominates(bl["start"], bl["c"]))
	assert.True(t, d.Dominates(bl["c"], bl["c"]), "a block dominates itself")
	assert.False(t, d.Dominates(bl["a"], bl["c"]))
	assert.False(t, d.StrictlyDominates(bl["c"], bl["c"]))
	assert.True(t, d.StrictlyDominates(bl["start"], bl["c"]))
}

func TestDominatorsLoop(t *testing.T) {
	f, bl, _ := sumLoop()
	d := BuildCFG(f).Dominators()

	assert.Nil(t, d.Idom[bl["start"]])
	assert.Same(t, bl["start"], d.Idom[bl["loop"]])
	assert.Same(t, bl["loop"], d.Idom[bl["body"]])
	assert.Same(t, bl["loop"], d.Idom[bl["exit"]])

	assert.True(t, d.Dominates(bl["loop"], bl["body"]))
	assert.True(t, d.Dominates(bl["loop"], bl["exit"]))
	assert.False(t, d.Dominates(bl["body"], bl["exit"]), "sibling loop blocks don't dominate")
	assert.True(t, d.Dominates(bl["start"], bl["exit"]))
}

func TestDominatorsSingleBlock(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("one")
	e := f.Entry()
	e.RetVoid()
	d := BuildCFG(f).Dominators()
	assert.Nil(t, d.Idom[e])
	assert.True(t, d.Dominates(e, e))
}
