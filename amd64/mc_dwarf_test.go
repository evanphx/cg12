package amd64_test

import (
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// The object carries a real DWARF line table and a subprogram DIE, readable with
// Go's debug/dwarf.
func TestObjDwarf(t *testing.T) {
	m := ir.NewModule()
	file := m.File("add.src")
	f := m.NewFunc("addmul", ir.ClsW).Export()
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	e.At(ir.SrcPos{File: file, Line: 10, Col: 1})
	s := e.Add(ir.ClsW, a, b)
	e.At(ir.SrcPos{File: file, Line: 11, Col: 1})
	e.Ret(e.Mul(ir.ClsW, s, a))

	code, err := amd64.CompileObject(m)
	require.NoError(t, err)

	ef, err := elf.NewFile(bytes.NewReader(code))
	require.NoError(t, err)
	d, err := ef.DWARF()
	require.NoError(t, err)

	// The line table maps lines 10 and 11 to increasing addresses.
	r := d.Reader()
	cu, err := r.Next()
	require.NoError(t, err)
	require.Equal(t, dwarf.TagCompileUnit, cu.Tag)
	lr, err := d.LineReader(cu)
	require.NoError(t, err)
	addrOf := map[int]uint64{}
	var le dwarf.LineEntry
	for {
		if err := lr.Next(&le); err != nil {
			break
		}
		if _, seen := addrOf[le.Line]; !seen {
			addrOf[le.Line] = le.Address
		}
	}
	require.Contains(t, addrOf, 10)
	require.Contains(t, addrOf, 11)
	require.Less(t, addrOf[10], addrOf[11])

	// A DW_TAG_subprogram named "addmul" with two formal parameters exists.
	var subName string
	var nparams int
	for {
		ent, err := r.Next()
		if err != nil || ent == nil {
			break
		}
		switch ent.Tag {
		case dwarf.TagSubprogram:
			if n, ok := ent.Val(dwarf.AttrName).(string); ok {
				subName = n
			}
		case dwarf.TagFormalParameter:
			nparams++
		}
	}
	require.Equal(t, "addmul", subName)
	require.Equal(t, 2, nparams)
}
