package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func TestCopyChainElimination(t *testing.T) {
	f := ir.NewModule().NewFunc("c", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	e := f.Entry()
	c1 := e.Copy(ir.ClsW, a)
	c2 := e.Copy(ir.ClsW, c1)
	e.Ret(c2)

	assert.True(t, Copy(f))
	assert.Empty(t, e.Instrs, "copies removed")
	assert.Equal(t, a, e.Jmp.Arg, "use rewritten to the original source")
}

func TestCopyNoneReturnsFalse(t *testing.T) {
	f := ir.NewModule().NewFunc("c", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))
	assert.False(t, Copy(f))
}

func TestCopyRewritesInsideInstrs(t *testing.T) {
	// %c = copy a ; %r = add %c, b ; ret %r  ->  %r = add a, b
	f := ir.NewModule().NewFunc("c", ir.ClsW)
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	c := e.Copy(ir.ClsW, a)
	e.Ret(e.Add(ir.ClsW, c, b))

	Copy(f)
	add := e.Instrs[0]
	assert.Equal(t, ir.OAdd, add.Op)
	assert.Equal(t, a, add.Args[0], "copy source substituted into the add")
	assert.Equal(t, b, add.Args[1])
}
