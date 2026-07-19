package pe_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// countInsns counts machine instructions in a disassembly (tab-indented lines that
// are not directives), so a specialized program's size can be compared to the
// interpreter's.
func countInsns(asm string) int {
	n := 0
	for _, line := range strings.Split(asm, "\n") {
		if !strings.HasPrefix(line, "\t") {
			continue // a label or blank line
		}
		if op := strings.Fields(line)[0]; op == "loc" {
			continue // a debug-location directive, not an instruction
		}
		n++
	}
	return n
}

// The whole point of the first Futamura projection: specializing the interpreter
// against a program yields native code for that program. Here the interpreter and
// the specialized loop both go all the way to ARM64, and the specialized program is
// a tight native loop -- no dispatch, dramatically smaller than the interpreter.
func TestSpecializeToNative(t *testing.T) {
	src, err := cc.Compile("toy.c", toyInterp)
	require.NoError(t, err)

	// sum = 0; i = in0; while (i != 0) { sum += i; i -= 1; } return sum
	loop, end := byte(8), byte(28)
	prog := []byte{
		opPUSH, 0, opSTORE, 0,
		opINPUT, 0, opSTORE, 1,
		opLOAD, 1, opJZ, end,
		opLOAD, 0, opLOAD, 1, opADD, opSTORE, 0,
		opLOAD, 1, opPUSH, 1, opSUB, opSTORE, 1,
		opJMP, loop,
		opLOAD, 0, opHALT,
	}

	// The interpreter, compiled to native as-is (the general dispatch loop).
	opt.OptimizeModule(src)
	interpObj, err := arm64.CompileToObject(src)
	require.NoError(t, err)
	interpAsm := arm64.Disassemble(interpObj)

	// The interpreter specialized against this program, then compiled to native.
	spec, name, _, err := pe.Specialize(mustCompile(t), "interp", prog)
	require.NoError(t, err)
	opt.Run(spec, opt.DefaultPipeline())
	rename(spec, name, "sum1toN")
	specObj, err := arm64.CompileToObject(spec)
	require.NoError(t, err)
	specAsm := arm64.Disassemble(specObj)
	t.Logf("specialized program, as native ARM64:\n%s", specAsm)

	ni, ns := countInsns(interpAsm), countInsns(specAsm)
	t.Logf("interpreter = %d instructions; specialized program = %d", ni, ns)

	require.Less(t, ns, 20, "the specialized loop is tight")
	require.Less(t, ns*4, ni, "and far smaller than the interpreter it came from")
	// It is a loop -- a backward branch -- with no dispatch left: no call, no
	// jump-table, no compare against a dozen opcodes.
	require.NotContains(t, specAsm, "bl ", "no call to a handler or the marker")
	require.NotContains(t, specAsm, "br ", "no computed-goto dispatch")
}

func mustCompile(t *testing.T) *ir.Module {
	t.Helper()
	m, err := cc.Compile("toy.c", toyInterp)
	require.NoError(t, err)
	return m
}

// rename gives the residual function a clean symbol name for the object.
func rename(m *ir.Module, from, to string) {
	for _, f := range m.Funcs {
		if f.Name == from {
			f.Name = to
		}
	}
}
