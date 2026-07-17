package arm64_test

import (
	"bytes"
	"debug/dwarf"
	dwarfelf "debug/elf"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestObjEmitDwarfLineTable checks that the object path emits a valid DWARF line
// program — parsed here with Go's read-only debug/dwarf, no external tools.
func TestObjEmitDwarfLineTable(t *testing.T) {
	data, err := arm64.CompileObject(dwarfModule())
	require.NoError(t, err)

	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	require.NotNil(t, ef.Section(".debug_line"), "has .debug_line")
	require.NotNil(t, ef.Section(".debug_info"), "has .debug_info")
	require.NotNil(t, ef.Section(".debug_abbrev"), "has .debug_abbrev")

	d, err := ef.DWARF()
	require.NoError(t, err)

	cu, err := d.Reader().Next()
	require.NoError(t, err)
	require.NotNil(t, cu)
	assert.Equal(t, dwarf.TagCompileUnit, cu.Tag)
	assert.Equal(t, "addmul.src", cu.Val(dwarf.AttrName))

	lr, err := d.LineReader(cu)
	require.NoError(t, err)
	require.NotNil(t, lr)

	lineAddr := map[int]uint64{}
	var le dwarf.LineEntry
	for {
		err := lr.Next(&le)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if le.EndSequence {
			continue
		}
		require.NotNil(t, le.File)
		assert.Equal(t, "addmul.src", filepath.Base(le.File.Name))
		lineAddr[le.Line] = le.Address
	}

	// Both source lines are mapped, and line 10's code precedes line 11's — the
	// advance_pc deltas are applied even before the set_address relocation, so the
	// addresses are the real .text offsets.
	require.Contains(t, lineAddr, 10)
	require.Contains(t, lineAddr, 11)
	assert.Less(t, lineAddr[10], lineAddr[11])
}

// TestObjEmitDwarfSubprograms checks the compilation unit carries a subprogram
// DIE for the function, with its parameters and their types.
func TestObjEmitDwarfSubprograms(t *testing.T) {
	data, err := arm64.CompileObject(dwarfModule())
	require.NoError(t, err)
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	d, err := ef.DWARF()
	require.NoError(t, err)

	r := d.Reader()
	var subs, params []string
	subType := map[string]string{} // subprogram -> return type name
	paramType := map[string]string{}
	for {
		e, err := r.Next()
		require.NoError(t, err)
		if e == nil {
			break
		}
		switch e.Tag {
		case dwarf.TagSubprogram:
			name, _ := e.Val(dwarf.AttrName).(string)
			subs = append(subs, name)
			assert.NotNil(t, e.Val(dwarf.AttrLowpc), "%s has low_pc", name)
			assert.Equal(t, true, e.Val(dwarf.AttrExternal), "%s is external", name)
			if off, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
				if ty, err := d.Type(off); err == nil {
					subType[name] = ty.Common().Name
				}
			}
		case dwarf.TagFormalParameter:
			name, _ := e.Val(dwarf.AttrName).(string)
			params = append(params, name)
			if off, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
				if ty, err := d.Type(off); err == nil {
					paramType[name] = ty.Common().Name
				}
			}
		}
	}

	assert.Contains(t, subs, "addmul")
	assert.Equal(t, "int", subType["addmul"], "addmul returns int")
	assert.Contains(t, params, "a")
	assert.Contains(t, params, "b")
	assert.Equal(t, "int", paramType["a"])
	assert.Equal(t, "int", paramType["b"])
}

// TestObjEmitDwarfVariedSignatures exercises a double-returning function, a void
// function, and long/double/int base types across several subprograms.
func TestObjEmitDwarfVariedSignatures(t *testing.T) {
	m := ir.NewModule()
	fi := m.File("t.src")

	f := m.NewFunc("compute", ir.ClsD).Export()
	x := f.Param("x", ir.ClsD)
	n := f.Param("n", ir.ClsL)
	e := f.Entry()
	e.At(ir.SrcPos{File: fi, Line: 5, Col: 1})
	e.Ret(e.Add(ir.ClsD, x, e.Sltof(ir.ClsD, n)))

	g := m.NewFuncVoid("noret")
	g.Param("a", ir.ClsW)
	g.Entry().RetVoid()

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	d, err := ef.DWARF()
	require.NoError(t, err)

	r := d.Reader()
	subRet := map[string]string{}
	paramType := map[string]string{}
	seenSub := map[string]bool{}
	for {
		en, err := r.Next()
		require.NoError(t, err)
		if en == nil {
			break
		}
		switch en.Tag {
		case dwarf.TagSubprogram:
			name, _ := en.Val(dwarf.AttrName).(string)
			seenSub[name] = true
			if off, ok := en.Val(dwarf.AttrType).(dwarf.Offset); ok {
				if ty, err := d.Type(off); err == nil {
					subRet[name] = ty.Common().Name
				}
			}
		case dwarf.TagFormalParameter:
			name, _ := en.Val(dwarf.AttrName).(string)
			if off, ok := en.Val(dwarf.AttrType).(dwarf.Offset); ok {
				if ty, err := d.Type(off); err == nil {
					paramType[name] = ty.Common().Name
				}
			}
		}
	}

	assert.True(t, seenSub["compute"])
	assert.True(t, seenSub["noret"])
	assert.Equal(t, "double", subRet["compute"])
	assert.Empty(t, subRet["noret"], "void function has no return type")
	assert.Equal(t, "double", paramType["x"])
	assert.Equal(t, "long", paramType["n"])
	assert.Equal(t, "int", paramType["a"])
}

// TestObjEmitDwarfParamLocations checks each parameter carries a DW_AT_location
// pointing at a valid .debug_loc list (a register or frame-relative expression
// over a live PC range).
func TestObjEmitDwarfParamLocations(t *testing.T) {
	data, err := arm64.CompileObject(dwarfModule()) // addmul(a, b)
	require.NoError(t, err)

	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	locSec := ef.Section(".debug_loc")
	require.NotNil(t, locSec, "has .debug_loc")
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
		require.True(t, ok, "%s has a location-list offset", name)

		lo := binary.LittleEndian.Uint64(locBytes[off:])
		hi := binary.LittleEndian.Uint64(locBytes[off+8:])
		elen := binary.LittleEndian.Uint16(locBytes[off+16:])
		require.NotZero(t, elen, "%s has a location expression", name)
		op := locBytes[off+18]
		assert.Less(t, lo, hi, "%s has a non-empty PC range", name)
		// DW_OP_reg0..31 (0x50..0x6f), DW_OP_regx (0x90), or DW_OP_fbreg (0x91).
		assert.True(t, (op >= 0x50 && op <= 0x6f) || op == 0x90 || op == 0x91,
			"%s location is a register or frame slot, got op %#x", name, op)
		seen[name] = true
	}
	assert.True(t, seen["a"])
	assert.True(t, seen["b"])
}

// inlinedModule builds compute(a) = dbl(a) + 1 and inlines dbl, so compute's
// body contains an inlined region of dbl called at caller.src:10.
func inlinedModule(t *testing.T) *ir.Module {
	t.Helper()
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
	return m
}

// TestObjEmitDwarfInlined checks the inlined region of dbl surfaces as a
// DW_TAG_inlined_subroutine with a resolvable abstract origin and call site.
func TestObjEmitDwarfInlined(t *testing.T) {
	data, err := arm64.CompileObject(inlinedModule(t))
	require.NoError(t, err)
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	d, err := ef.DWARF()
	require.NoError(t, err)

	r := d.Reader()
	found := false
	for {
		e, err := r.Next()
		require.NoError(t, err)
		if e == nil {
			break
		}
		if e.Tag != dwarf.TagInlinedSubroutine {
			continue
		}
		found = true
		assert.EqualValues(t, 10, e.Val(dwarf.AttrCallLine), "call line")
		assert.EqualValues(t, 1, e.Val(dwarf.AttrCallFile), "call file (caller.src)")

		off, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
		require.True(t, ok, "abstract_origin is a DIE offset")
		ar := d.Reader()
		ar.Seek(off)
		ab, err := ar.Next()
		require.NoError(t, err)
		require.NotNil(t, ab)
		assert.Equal(t, dwarf.TagSubprogram, ab.Tag)
		assert.Equal(t, "dbl", ab.Val(dwarf.AttrName), "abstract origin is dbl")
		assert.EqualValues(t, 1, ab.Val(dwarf.AttrInline), "marked DW_AT_inline")
	}
	assert.True(t, found, "an inlined_subroutine DIE is present")
}

// TestObjEmitDwarfNestedInline builds top -> mid -> leaf and fully inlines the
// chain, so top's code carries a leaf inline nested inside a mid inline. The
// call-site file of each proves the inline context was rebased correctly: mid is
// called from top.src, but the nested leaf is called from mid.src.
func TestObjEmitDwarfNestedInline(t *testing.T) {
	m := ir.NewModule()
	topF := m.File("top.src")   // 1
	midF := m.File("mid.src")   // 2
	leafF := m.File("leaf.src") // 3

	leaf := m.NewFunc("leaf", ir.ClsW)
	lx := leaf.Param("x", ir.ClsW)
	le := leaf.Entry()
	le.At(ir.SrcPos{File: leafF, Line: 40, Col: 1})
	le.Ret(le.Mul(ir.ClsW, lx, leaf.Word(2)))

	mid := m.NewFunc("mid", ir.ClsW)
	my := mid.Param("y", ir.ClsW)
	me := mid.Entry()
	me.At(ir.SrcPos{File: midF, Line: 30, Col: 3})
	mr := me.Call(ir.ClsW, mid.Sym("leaf", 0), my)
	me.Ret(me.Add(ir.ClsW, mr, mid.Word(1)))

	top := m.NewFunc("top", ir.ClsW).Export()
	tz := top.Param("z", ir.ClsW)
	te := top.Entry()
	te.At(ir.SrcPos{File: topF, Line: 20, Col: 3})
	tr := te.Call(ir.ClsW, top.Sym("mid", 0), tz)
	te.Ret(te.Add(ir.ClsW, tr, top.Word(1)))

	for opt.Inline(m) { // inline until the chain is fully flattened
	}

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	d, err := ef.DWARF()
	require.NoError(t, err)

	r := d.Reader()
	callFile := map[string]int64{}
	callLine := map[string]int64{}
	for {
		e, err := r.Next()
		require.NoError(t, err)
		if e == nil {
			break
		}
		if e.Tag != dwarf.TagInlinedSubroutine {
			continue
		}
		off := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
		ar := d.Reader()
		ar.Seek(off)
		ab, _ := ar.Next()
		name := ab.Val(dwarf.AttrName).(string)
		callFile[name], _ = e.Val(dwarf.AttrCallFile).(int64)
		callLine[name], _ = e.Val(dwarf.AttrCallLine).(int64)
	}

	require.Contains(t, callFile, "mid")
	require.Contains(t, callFile, "leaf")
	assert.EqualValues(t, 1, callFile["mid"], "mid inlined at top.src")
	assert.EqualValues(t, 20, callLine["mid"])
	assert.EqualValues(t, 2, callFile["leaf"], "nested leaf inlined at mid.src")
	assert.EqualValues(t, 30, callLine["leaf"])
}

// TestObjEmitDwarfLinks confirms a real linker (gcc/ld) accepts and relocates the
// object's DWARF: after linking, objdump's decoded line table shows both lines.
func TestObjEmitDwarfLinks(t *testing.T) {
	cc := assembler(t)
	objdump := testenv.Tool(t, "aarch64-linux-gnu-objdump", "objdump")

	data, err := arm64.CompileObject(dwarfModule())
	require.NoError(t, err)

	dir := t.TempDir()
	objPath := filepath.Join(dir, "addmul.o")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, data, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(`
extern int addmul(int,int);
int main(void){ return addmul(3,4)==(3+4)*3 ? 0 : 1; }`), 0o644))

	out, err := exec.Command(cc, "-g", "-o", bin, objPath, cPath).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s", out)

	dump, err := exec.Command(objdump, "--dwarf=decodedline", bin).CombinedOutput()
	require.NoErrorf(t, err, "objdump failed: %s", dump)
	table := string(dump)
	assert.Contains(t, table, "addmul.src")
	assert.Regexp(t, `addmul\.src\s+10\b`, table)
	assert.Regexp(t, `addmul\.src\s+11\b`, table)
}

// dwarfModule builds addmul(a,b) = (a+b)*a with source positions on the two
// arithmetic instructions.
func dwarfModule() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("addmul", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	fi := m.File("addmul.src")
	e := f.Entry()
	e.At(ir.SrcPos{File: fi, Line: 10, Col: 3})
	s := e.Add(ir.ClsW, a, b)
	e.At(ir.SrcPos{File: fi, Line: 11, Col: 7})
	e.Ret(e.Mul(ir.ClsW, s, a))
	return m
}

// A call's position must survive arm64 call lowering, which replaces the call
// with a different instruction: the line table has to end up pointing at the
// branch that the call became. The relocation says exactly where that branch is,
// so this pins the recorded address to it rather than settling for "the position
// is mentioned somewhere".
func TestObjEmitDwarfCallPositionSurvivesLowering(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("go", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	fi := m.File("call.src")
	e := f.Entry()
	e.At(ir.SrcPos{File: fi, Line: 42, Col: 9})
	e.Ret(e.Call(ir.ClsW, f.Sym("helper", 0), x))

	o, err := arm64.CompileToObject(m)
	require.NoError(t, err)

	// Where the call ended up: the only relocation naming helper.
	var callAt uint64
	found := false
	for _, r := range o.Relocs {
		if r.Sym == "helper" && r.Type == obj.R_AARCH64_CALL26 {
			require.False(t, found, "expected a single call to helper")
			callAt, found = r.Offset, true
		}
	}
	require.True(t, found, "the call should survive as a bl:\n%s", arm64.Disassemble(o))

	// Lowering a call emits the argument moves ahead of the branch, and they carry
	// its position too, so line 42's code starts before the bl and runs through it.
	// What matters is the question a debugger asks: stopped here, what line is this?
	require.Equal(t, 42, dwarfLineAt(t, o, callAt), "stopped at the call, the line is 42")
}

// dwarfLineAt reads the object's DWARF line program and reports the source line
// covering addr -- the entry with the greatest address not past it, which is what
// a debugger resolves a program counter with.
func dwarfLineAt(t *testing.T, o *obj.Object, addr uint64) int {
	t.Helper()
	data, err := o.MarshalELF()
	require.NoError(t, err)
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	d, err := ef.DWARF()
	require.NoError(t, err)
	cu, err := d.Reader().Next()
	require.NoError(t, err)
	lr, err := d.LineReader(cu)
	require.NoError(t, err)

	line, best := 0, uint64(0)
	var le dwarf.LineEntry
	for {
		err := lr.Next(&le)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if le.EndSequence || le.Address > addr {
			continue
		}
		if le.Address >= best {
			line, best = le.Line, le.Address
		}
	}
	require.NotZero(t, line, "no line entry covers %#x", addr)
	return line
}
