package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// A toy stack-machine interpreter, annotated for partial evaluation: `code` is the
// green program, `in` the runtime inputs, and __cg12_merge_point names the green
// loop variables (pc, sp).
const toyInterp = `
void __cg12_merge_point(int pc, int sp) { (void)pc; (void)sp; }

enum { OP_HALT, OP_PUSH, OP_INPUT, OP_ADD, OP_SUB, OP_MUL };

long interp(const unsigned char *code, const long *in) {
    long stack[64];
    int sp = 0;
    int pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_PUSH:  stack[sp++] = code[pc+1];         pc += 2; break;
        case OP_INPUT: stack[sp++] = in[code[pc+1]];     pc += 2; break;
        case OP_ADD:   sp--; stack[sp-1] += stack[sp];   pc += 1; break;
        case OP_SUB:   sp--; stack[sp-1] -= stack[sp];   pc += 1; break;
        case OP_MUL:   sp--; stack[sp-1] *= stack[sp];   pc += 1; break;
        case OP_HALT:  return stack[sp-1];
        }
    }
}
`

const (
	opHALT = iota
	opPUSH
	opINPUT
	opADD
	opSUB
	opMUL
)

// specialize compiles the toy interpreter, specializes it against prog, optimizes
// the residual, and returns the runnable module + entry name + input order.
func specialize(t *testing.T, prog []byte) (*interp.Machine, string, []int64) {
	t.Helper()
	m, err := cc.Compile("toy.c", toyInterp)
	require.NoError(t, err, "compile interpreter")

	spec, name, inputs, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err, "specialize")

	// The interpreter has dissolved: no dispatch, no operand-stack memory, no call
	// to the loop marker -- only the arithmetic the program describes remains.
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	for _, gone := range []string{"switch", "load", "store", "call", "alloc"} {
		require.NotContains(t, residual, gone, "interpreter machinery should be gone")
	}

	opt.Run(spec, opt.DefaultPipeline())

	mc, err := interp.New(spec)
	require.NoError(t, err, "load residual")
	return mc, name, inputs
}

// A program with no runtime input folds all the way to a constant: the whole
// interpreter, its dispatch, and the operand stack vanish.
func TestSpecializeConstantProgram(t *testing.T) {
	// PUSH 2, PUSH 3, ADD, PUSH 4, MUL, HALT   ->  (2+3)*4 = 20
	prog := []byte{opPUSH, 2, opPUSH, 3, opADD, opPUSH, 4, opMUL, opHALT}
	mc, name, inputs := specialize(t, prog)
	require.Empty(t, inputs, "no runtime inputs")

	v, err := mc.Call(name)
	require.NoError(t, err)
	require.Equal(t, int64(20), v.I64())
}

// A program that reads a runtime input specializes to a residual over that input:
// the interpreter is gone, leaving just the arithmetic the program describes.
func TestSpecializeProgramWithInput(t *testing.T) {
	// PUSH 2, PUSH 3, ADD, INPUT 0, MUL, HALT   ->  (2+3) * in[0] = 5 * in0
	prog := []byte{opPUSH, 2, opPUSH, 3, opADD, opINPUT, 0, opMUL, opHALT}
	mc, name, inputs := specialize(t, prog)
	require.Len(t, inputs, 1, "one runtime input")

	for _, in0 := range []int64{7, 10, -4} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 5*in0, v.I64(), "specialized(%d)", in0)
	}
}
