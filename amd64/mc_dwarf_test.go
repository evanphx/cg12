package amd64_test

import (
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"encoding/binary"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

func dwarfModule() *ir.Module {
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
	return m
}

// The object carries a real DWARF line table and a subprogram DIE, readable with
// Go's debug/dwarf.
func TestObjDwarf(t *testing.T) {
	code, err := amd64.CompileObject(dwarfModule())
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

// Each parameter has a .debug_loc location list resolving to a register or a
// frame slot over a non-empty PC range.
func TestObjDwarfParamLocations(t *testing.T) {
	code, err := amd64.CompileObject(dwarfModule())
	require.NoError(t, err)
	ef, err := elf.NewFile(bytes.NewReader(code))
	require.NoError(t, err)
	locSec := ef.Section(".debug_loc")
	require.NotNil(t, locSec)
	locBytes, err := locSec.Data()
	require.NoError(t, err)

	d, err := ef.DWARF()
	require.NoError(t, err)
	r := d.Reader()
	seen := map[string]bool{}
	for {
		e, err := r.Next()
		require.NoError(t, err)
		if e == nil {
			break
		}
		if e.Tag != dwarf.TagFormalParameter {
			continue
		}
		name := e.Val(dwarf.AttrName).(string)
		off, ok := e.Val(dwarf.AttrLocation).(int64)
		require.Truef(t, ok, "%s has a location-list offset", name)
		lo := binary.LittleEndian.Uint64(locBytes[off:])
		hi := binary.LittleEndian.Uint64(locBytes[off+8:])
		elen := binary.LittleEndian.Uint16(locBytes[off+16:])
		require.NotZerof(t, elen, "%s has a location expression", name)
		op := locBytes[off+18]
		require.Lessf(t, lo, hi, "%s has a non-empty PC range", name)
		// DW_OP_reg0..31 (0x50..0x6f), DW_OP_regx (0x90), or DW_OP_fbreg (0x91).
		require.Truef(t, (op >= 0x50 && op <= 0x6f) || op == 0x90 || op == 0x91,
			"%s location is a register or frame slot, got op %#x", name, op)
		seen[name] = true
	}
	require.True(t, seen["a"])
	require.True(t, seen["b"])
}

// compute(a) = dbl(a) + 1 with dbl inlined: compute's DWARF carries a
// DW_TAG_inlined_subroutine for dbl with a resolvable abstract origin and call
// site.
func TestObjDwarfInlined(t *testing.T) {
	m := ir.NewModule()
	callerF := m.File("caller.src")
	calleeF := m.File("callee.src")

	cal := m.NewFunc("dbl", ir.ClsW)
	x := cal.Param("x", ir.ClsW)
	ce := cal.Entry()
	ce.At(ir.SrcPos{File: calleeF, Line: 100, Col: 1})
	ce.Ret(ce.Add(ir.ClsW, x, x))

	f := m.NewFunc("compute", ir.ClsW).Export()
	a := f.Param("a", ir.ClsW)
	e := f.Entry()
	e.At(ir.SrcPos{File: callerF, Line: 10, Col: 3})
	r := e.Call(ir.ClsW, f.Sym("dbl", 0), a)
	e.At(ir.SrcPos{File: callerF, Line: 11, Col: 3})
	e.Ret(e.Add(ir.ClsW, r, f.Word(1)))

	require.True(t, opt.Inline(m), "dbl should inline into compute")

	code, err := amd64.CompileObject(m)
	require.NoError(t, err)
	ef, err := elf.NewFile(bytes.NewReader(code))
	require.NoError(t, err)
	d, err := ef.DWARF()
	require.NoError(t, err)

	rd := d.Reader()
	found := false
	for {
		e, err := rd.Next()
		require.NoError(t, err)
		if e == nil {
			break
		}
		if e.Tag != dwarf.TagInlinedSubroutine {
			continue
		}
		found = true
		require.EqualValues(t, 10, e.Val(dwarf.AttrCallLine))
		require.EqualValues(t, 1, e.Val(dwarf.AttrCallFile)) // caller.src

		off, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
		require.True(t, ok)
		ar := d.Reader()
		ar.Seek(off)
		ab, err := ar.Next()
		require.NoError(t, err)
		require.Equal(t, dwarf.TagSubprogram, ab.Tag)
		require.Equal(t, "dbl", ab.Val(dwarf.AttrName))
		require.EqualValues(t, 1, ab.Val(dwarf.AttrInline))
	}
	require.True(t, found, "an inlined_subroutine DIE is present")
}
