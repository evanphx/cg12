package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// diamond builds:  start -> {a, b} -> c
func diamond() (*ir.Func, map[string]*ir.Block) {
	f := NewModuleFunc()
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	c := f.NewBlock("c")
	start.Jnz(cond, a, b)
	a.Goto(c)
	b.Goto(c)
	c.RetVoid()
	return f, map[string]*ir.Block{"start": start, "a": a, "b": b, "c": c}
}

// NewModuleFunc is a tiny helper so tests don't repeat module plumbing.
func NewModuleFunc() *ir.Func { return ir.NewModule().NewFuncVoid("t") }

func TestFillPredsAndRPO(t *testing.T) {
	f, bl := diamond()
	c := BuildCFG(f)

	assert.Empty(t, bl["start"].Preds)
	assert.Equal(t, []*ir.Block{bl["start"]}, bl["a"].Preds)
	assert.Equal(t, []*ir.Block{bl["start"]}, bl["b"].Preds)
	assert.ElementsMatch(t, []*ir.Block{bl["a"], bl["b"]}, bl["c"].Preds)

	require.Len(t, c.RPO, 4)
	assert.Same(t, bl["start"], c.RPO[0], "entry is first in RPO")
	assert.Same(t, bl["c"], c.RPO[3], "join is last in RPO")
	// A predecessor precedes its non-back-edge successors in RPO.
	assert.Less(t, c.Num(bl["a"]), c.Num(bl["c"]))
	assert.Less(t, c.Num(bl["b"]), c.Num(bl["c"]))
}

func TestPredsDedupOnDoubleEdge(t *testing.T) {
	// jnz with identical targets must list the predecessor only once.
	f := NewModuleFunc()
	cond := f.Param("c", ir.ClsW)
	start := f.Entry()
	target := f.NewBlock("t")
	start.Jnz(cond, target, target)
	target.RetVoid()

	BuildCFG(f)
	assert.Equal(t, []*ir.Block{start}, target.Preds)
}

func TestUnreachableBlock(t *testing.T) {
	f := NewModuleFunc()
	start := f.Entry()
	dead := f.NewBlock("dead")
	start.RetVoid()
	dead.RetVoid()

	c := BuildCFG(f)
	assert.True(t, c.Reachable(start))
	assert.False(t, c.Reachable(dead))
	assert.Equal(t, -1, c.Num(dead))
	assert.Len(t, c.RPO, 1)
}

func TestRPORebuildsPredsIdempotent(t *testing.T) {
	f, _ := diamond()
	BuildCFG(f)
	c2 := BuildCFG(f) // second run must not duplicate preds
	assert.Len(t, c2.RPO, 4)
	for _, b := range f.Blocks {
		seen := map[*ir.Block]bool{}
		for _, p := range b.Preds {
			assert.False(t, seen[p], "duplicate predecessor after rebuild")
			seen[p] = true
		}
	}
}

func TestEmptyFunctionCFG(t *testing.T) {
	f := ir.NewModule().NewFuncVoid("empty") // no blocks
	c := BuildCFG(f)
	assert.Empty(t, c.RPO)
	d := c.Dominators()
	assert.Empty(t, d.Idom)
}
