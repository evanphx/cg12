package arm64_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestDwarfDirectivesEmitted(t *testing.T) {
	asm, err := arm64.CompileModule(dwarfModule())
	require.NoError(t, err)
	assert.Contains(t, asm, `.file 1 "addmul.src"`)
	assert.Contains(t, asm, ".loc 1 10 3")
	assert.Contains(t, asm, ".loc 1 11 7")
}

func TestDwarfLineTableInObject(t *testing.T) {
	cc, ok := assembler()
	if !ok {
		t.Skip("no AArch64 assembler available")
	}
	objdump := "aarch64-linux-gnu-objdump"
	if _, err := exec.LookPath(objdump); err != nil {
		t.Skip("aarch64-linux-gnu-objdump not available")
	}

	asm, err := arm64.CompileModule(dwarfModule())
	require.NoError(t, err)

	dir := t.TempDir()
	asmPath := filepath.Join(dir, "out.s")
	objPath := filepath.Join(dir, "out.o")
	require.NoError(t, os.WriteFile(asmPath, []byte(asm), 0o644))

	out, err := exec.Command(cc, "-c", "-o", objPath, asmPath).CombinedOutput()
	require.NoErrorf(t, err, "assemble failed: %s", out)

	dump, err := exec.Command(objdump, "--dwarf=decodedline", objPath).CombinedOutput()
	require.NoError(t, err)
	table := string(dump)

	// The DWARF line program maps both source lines back to code.
	assert.Contains(t, table, "addmul.src")
	assert.Regexp(t, `addmul\.src\s+10\b`, table)
	assert.Regexp(t, `addmul\.src\s+11\b`, table)
}

func TestDwarfPositionsSurviveLowering(t *testing.T) {
	// A call's position must survive arm64 call lowering onto the emitted .loc.
	m := ir.NewModule()
	f := m.NewFunc("go", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	fi := m.File("call.src")
	e := f.Entry()
	e.At(ir.SrcPos{File: fi, Line: 42, Col: 9})
	e.Ret(e.Call(ir.ClsW, f.Sym("helper", 0), x))

	asm, err := arm64.CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, asm, ".loc 1 42 9")
	assert.Contains(t, asm, "\tbl helper\n")
	// the .loc precedes the call
	assert.Less(t, strings.Index(asm, ".loc 1 42 9"), strings.Index(asm, "bl helper"))
}
