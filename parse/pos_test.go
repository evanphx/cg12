package parse_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSrcPosRoundTrip(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("go", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	fi := m.File("prog.src")
	e := f.Entry()
	e.At(ir.SrcPos{File: fi, Line: 10, Col: 3})
	s := e.Add(ir.ClsW, a, b)
	e.At(ir.SrcPos{File: fi, Line: 11, Col: 5})
	e.Ret(e.Mul(ir.ClsW, s, a))

	text := m.String()
	assert.Contains(t, text, `dbgfile "prog.src"`)
	assert.Contains(t, text, "dbgloc 10 3")
	assert.Contains(t, text, "dbgloc 11 5")

	m2, err := parse.Parse(text)
	require.NoError(t, err)
	instrs := m2.Funcs[0].Start.Instrs

	p0 := instrs[0].Pos
	assert.Equal(t, "prog.src", m2.FileName(p0.File))
	assert.Equal(t, uint32(10), p0.Line)
	assert.Equal(t, uint32(3), p0.Col)

	p1 := instrs[1].Pos
	assert.Equal(t, uint32(11), p1.Line)
	assert.Equal(t, uint32(5), p1.Col)
}
