package analysis

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func TestTempSetBasics(t *testing.T) {
	s := NewTempSet(200)
	assert.True(t, s.Add(5))
	assert.False(t, s.Add(5), "re-adding reports no change")
	assert.True(t, s.Add(130)) // spans into the second word
	assert.True(t, s.Has(5))
	assert.True(t, s.Has(130))
	assert.False(t, s.Has(6))
	assert.Equal(t, 2, s.Len())
	assert.Equal(t, []int{5, 130}, s.Members())

	s.Remove(5)
	assert.False(t, s.Has(5))
	assert.Equal(t, 1, s.Len())
}

func TestTempSetAddRef(t *testing.T) {
	s := NewTempSet(64)
	assert.True(t, s.AddRef(ir.Ref{Kind: ir.RefTemp, ID: 3}))
	assert.False(t, s.AddRef(ir.Ref{Kind: ir.RefConst, ID: 3}), "non-temp refs ignored")
	assert.False(t, s.AddRef(ir.R))
	assert.True(t, s.Has(3))
	assert.Equal(t, 1, s.Len())
}

func TestTempSetUnion(t *testing.T) {
	a := NewTempSet(64)
	a.Add(1)
	a.Add(2)
	b := NewTempSet(64)
	b.Add(2)
	b.Add(3)

	assert.True(t, a.Union(b), "union that adds elements reports change")
	assert.Equal(t, []int{1, 2, 3}, a.Members())
	assert.False(t, a.Union(b), "union of a subset reports no change")
}

func TestTempSetCopyAndEqual(t *testing.T) {
	a := NewTempSet(64)
	a.Add(7)
	b := a.Copy()
	assert.True(t, a.Equal(b))

	b.Add(9)
	assert.False(t, a.Equal(b), "copy is independent of the original")
	assert.False(t, a.Has(9))

	// Differently sized sets are unequal.
	assert.False(t, a.Equal(NewTempSet(200)))
}
