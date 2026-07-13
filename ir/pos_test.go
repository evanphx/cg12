package ir

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSrcPosValid(t *testing.T) {
	assert.False(t, SrcPos{}.Valid())
	assert.False(t, SrcPos{File: 1}.Valid()) // no line
	assert.False(t, SrcPos{Line: 5}.Valid()) // no file
	assert.True(t, SrcPos{File: 1, Line: 5}.Valid())
}

func TestModuleFileTable(t *testing.T) {
	m := NewModule()
	a := m.File("a.src")
	b := m.File("b.src")
	assert.Equal(t, uint32(1), a)
	assert.Equal(t, uint32(2), b)
	assert.Equal(t, a, m.File("a.src")) // interned, not duplicated
	assert.Len(t, m.Files, 2)

	assert.Equal(t, "a.src", m.FileName(1))
	assert.Equal(t, "b.src", m.FileName(2))
	assert.Equal(t, "", m.FileName(0))
	assert.Equal(t, "", m.FileName(99))
}

func TestAtStampsPositions(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("g", ClsW)
	x := f.Param("x", ClsW)
	e := f.Entry()
	pos := SrcPos{File: m.File("g.src"), Line: 3, Col: 1}
	e.At(pos)
	r := e.Add(ClsW, x, f.Word(1))
	e.Ret(r)

	assert.Equal(t, pos, e.Instrs[0].Pos, "At stamps emitted instructions")
}
