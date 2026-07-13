package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func setOf(bs BlockSet) map[*ir.Block]bool { return bs }

func TestDominanceFrontierDiamond(t *testing.T) {
	f, bl := diamond()
	c := BuildCFG(f)
	df := c.DominanceFrontier(c.Dominators())

	// Both arms have the join as their frontier; start and join have none.
	assert.Equal(t, map[*ir.Block]bool{bl["c"]: true}, setOf(df[bl["a"]]))
	assert.Equal(t, map[*ir.Block]bool{bl["c"]: true}, setOf(df[bl["b"]]))
	assert.Empty(t, df[bl["start"]])
	assert.Empty(t, df[bl["c"]])
}

func TestDominanceFrontierLoop(t *testing.T) {
	f, bl, _ := sumLoop()
	c := BuildCFG(f)
	df := c.DominanceFrontier(c.Dominators())

	// The loop header sits in its own frontier (back edge) and in the body's.
	assert.True(t, df[bl["body"]][bl["loop"]])
	assert.True(t, df[bl["loop"]][bl["loop"]])
}

func TestIteratedFrontier(t *testing.T) {
	f, bl := diamond()
	c := BuildCFG(f)
	df := c.DominanceFrontier(c.Dominators())

	got := IteratedFrontier(df, BlockSet{bl["a"]: true})
	assert.Equal(t, BlockSet{bl["c"]: true}, got)

	got = IteratedFrontier(df, BlockSet{bl["a"]: true, bl["b"]: true})
	assert.Equal(t, BlockSet{bl["c"]: true}, got)

	// A def set whose blocks have empty frontiers yields nothing.
	assert.Empty(t, IteratedFrontier(df, BlockSet{bl["c"]: true}))
}
