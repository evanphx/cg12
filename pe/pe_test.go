package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// A toy stack-machine interpreter with local variables and control flow, annotated
// for partial evaluation: `code` is the green program, `in` the runtime inputs, and
// __cg12_merge_point names the green loop variables (pc, sp).
const toyInterp = `
void __cg12_merge_point(int pc, int sp) { (void)pc; (void)sp; }

enum { OP_HALT, OP_PUSH, OP_INPUT, OP_ADD, OP_SUB, OP_MUL,
       OP_LOAD, OP_STORE, OP_JMP, OP_JZ };

long interp(const unsigned char *code, const long *in) {
    long stack[64];
    long var[2];
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
        case OP_LOAD:  stack[sp++] = var[code[pc+1]];    pc += 2; break;
        case OP_STORE: var[code[pc+1]] = stack[--sp];    pc += 2; break;
        case OP_JMP:   pc = code[pc+1];                           break;
        case OP_JZ:    if (stack[--sp] == 0) pc = code[pc+1]; else pc += 2; break;
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
	opLOAD
	opSTORE
	opJMP
	opJZ
)

// refRun is an independent reference interpreter for the toy bytecode, so the
// specialized function can be checked against the interpreter's actual semantics.
func refRun(prog []byte, in []int64) int64 {
	var stack []int64
	var vars [4]int64
	pc := 0
	for {
		switch prog[pc] {
		case opPUSH:
			stack = append(stack, int64(prog[pc+1]))
			pc += 2
		case opINPUT:
			stack = append(stack, in[prog[pc+1]])
			pc += 2
		case opADD:
			n := len(stack)
			stack[n-2] += stack[n-1]
			stack, pc = stack[:n-1], pc+1
		case opSUB:
			n := len(stack)
			stack[n-2] -= stack[n-1]
			stack, pc = stack[:n-1], pc+1
		case opMUL:
			n := len(stack)
			stack[n-2] *= stack[n-1]
			stack, pc = stack[:n-1], pc+1
		case opLOAD:
			stack = append(stack, vars[prog[pc+1]])
			pc += 2
		case opSTORE:
			n := len(stack)
			vars[prog[pc+1]] = stack[n-1]
			stack, pc = stack[:n-1], pc+2
		case opJMP:
			pc = int(prog[pc+1])
		case opJZ:
			n := len(stack)
			v := stack[n-1]
			stack = stack[:n-1]
			if v == 0 {
				pc = int(prog[pc+1])
			} else {
				pc += 2
			}
		case opHALT:
			return stack[len(stack)-1]
		}
	}
}

// specialize compiles the interpreter, specializes it against prog, optimizes the
// residual, and returns the runnable module + entry name + input order.
func specialize(t *testing.T, prog []byte) (*interp.Machine, string, []int64, string) {
	t.Helper()
	m, err := cc.Compile("toy.c", toyInterp)
	require.NoError(t, err, "compile interpreter")

	spec, name, inputs, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err, "specialize")
	residual := spec.String()

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err, "load residual")
	return mc, name, inputs, residual
}

// A program with no runtime input folds all the way to a constant: the whole
// interpreter, its dispatch, and the operand stack vanish.
func TestSpecializeConstantProgram(t *testing.T) {
	// PUSH 2, PUSH 3, ADD, PUSH 4, MUL, HALT   ->  (2+3)*4 = 20
	prog := []byte{opPUSH, 2, opPUSH, 3, opADD, opPUSH, 4, opMUL, opHALT}
	mc, name, inputs, residual := specialize(t, prog)
	t.Logf("residual:\n%s", residual)
	require.Empty(t, inputs)
	for _, gone := range []string{"switch", "call", "alloc"} {
		require.NotContains(t, residual, gone, "interpreter machinery should be gone")
	}
	v, err := mc.Call(name)
	require.NoError(t, err)
	require.Equal(t, int64(20), v.I64())
}

// A program that reads a runtime input specializes to a residual over that input.
func TestSpecializeProgramWithInput(t *testing.T) {
	// PUSH 2, PUSH 3, ADD, INPUT 0, MUL, HALT   ->  (2+3) * in[0]
	prog := []byte{opPUSH, 2, opPUSH, 3, opADD, opINPUT, 0, opMUL, opHALT}
	mc, name, inputs, residual := specialize(t, prog)
	t.Logf("residual:\n%s", residual)
	require.Len(t, inputs, 1)
	for _, in0 := range []int64{7, 10, -4} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 5*in0, v.I64(), "specialized(%d)", in0)
	}
}

// A program with a data-dependent loop specializes to a residual loop: the branch
// on the runtime counter residualizes, and the back-edge to the loop head becomes a
// residual back-edge, with the accumulator and counter carried as phis.
func TestSpecializeLoop(t *testing.T) {
	// sum = 0; i = in0; while (i != 0) { sum += i; i -= 1; } return sum
	//   -> sum of 1..in0.
	loop, end := byte(8), byte(28)
	prog := []byte{
		opPUSH, 0, opSTORE, 0, // var0 (sum) = 0
		opINPUT, 0, opSTORE, 1, // var1 (i) = in[0]
		opLOAD, 1, opJZ, end, // loop: if (i == 0) goto end
		opLOAD, 0, opLOAD, 1, opADD, opSTORE, 0, // sum += i
		opLOAD, 1, opPUSH, 1, opSUB, opSTORE, 1, // i -= 1
		opJMP, loop, // goto loop
		opLOAD, 0, opHALT, // end: return sum
	}
	mc, name, inputs, residual := specialize(t, prog)
	t.Logf("residual:\n%s", residual)
	require.Len(t, inputs, 1)

	for _, in0 := range []int64{0, 1, 5, 10, 100} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, refRun(prog, []int64{in0}), v.I64(), "specialized(%d)", in0)
	}
}
