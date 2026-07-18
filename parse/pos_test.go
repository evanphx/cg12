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
	assert.Contains(t, text, `loc "prog.src" 10 3`) // file printed when it changes
	assert.Contains(t, text, "loc 11 5")            // just line/column when the file is the same

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

// The compact loc directive: a file-setting form, line-only forms that keep the
// file, and the older dbgfile/dbgloc still accepted for back-compat.
func TestLocSyntax(t *testing.T) {
	src := `function w $f() {
@s
	loc "a.c" 10 3
	%x =w add 1, 2
	loc 11
	%y =w add %x, 1
	dbgfile "b.c"
	dbgloc 20 4
	%z =w add %y, 1
	ret %z
}
`
	m, err := parse.Parse(src)
	require.NoError(t, err)
	in := m.Funcs[0].Start.Instrs

	assert.Equal(t, "a.c", m.FileName(in[0].Pos.File))
	assert.Equal(t, [2]uint32{10, 3}, [2]uint32{in[0].Pos.Line, in[0].Pos.Col})
	// loc 11 keeps the file and resets the column to 0.
	assert.Equal(t, "a.c", m.FileName(in[1].Pos.File))
	assert.Equal(t, [2]uint32{11, 0}, [2]uint32{in[1].Pos.Line, in[1].Pos.Col})
	// dbgfile/dbgloc still work.
	assert.Equal(t, "b.c", m.FileName(in[2].Pos.File))
	assert.Equal(t, [2]uint32{20, 4}, [2]uint32{in[2].Pos.Line, in[2].Pos.Col})
}
