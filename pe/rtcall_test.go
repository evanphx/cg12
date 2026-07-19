package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// A stack machine whose arithmetic is not built in but delegated to a runtime
// helper rt_binop(op, a, b) -- the common shape of a real interpreter, whose
// handlers call out to runtime routines (arithmetic, allocation, printing). The PE
// treats rt_binop as a black box: it cannot fold it, but it residualizes the call
// with the green opcode selector folded to a constant and the runtime operands
// carried through, and the dispatch that reached the handler is gone.
const toyRTInterp = `
void __cg12_merge_point(int pc, int sp);
long rt_binop(int op, long a, long b);

enum { OP_HALT, OP_PUSH, OP_INPUT, OP_RT, OP_LOAD, OP_STORE, OP_JMP, OP_JZ };

long interp(const unsigned char *code, const long *in) {
    long stack[64];
    long var[2];
    int sp = 0;
    int pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_PUSH:  stack[sp++] = code[pc+1];      pc += 2; break;
        case OP_INPUT: stack[sp++] = in[code[pc+1]];  pc += 2; break;
        case OP_RT: {
            long b = stack[--sp];
            stack[sp-1] = rt_binop(code[pc+1], stack[sp-1], b);
            pc += 2; break;
        }
        case OP_LOAD:  stack[sp++] = var[code[pc+1]];  pc += 2; break;
        case OP_STORE: var[code[pc+1]] = stack[--sp];  pc += 2; break;
        case OP_JMP:   pc = code[pc+1];                         break;
        case OP_JZ:    if (stack[--sp] == 0) pc = code[pc+1]; else pc += 2; break;
        case OP_HALT:  return stack[sp-1];
        }
    }
}
`

const (
	rtHALT = iota
	rtPUSH
	rtINPUT
	rtRT
	rtLOAD
	rtSTORE
	rtJMP
	rtJZ
)

// runtime-side operation selectors passed to rt_binop.
const (
	rbADD = iota
	rbSUB
	rbMUL
)

func rtBinop(op, a, b int64) int64 {
	switch op {
	case rbADD:
		return a + b
	case rbSUB:
		return a - b
	case rbMUL:
		return a * b
	}
	return 0
}

// refRunRT is an independent reference interpreter for the runtime-call toy.
func refRunRT(prog []byte, in []int64) int64 {
	var stack []int64
	var vars [2]int64
	pc := 0
	for {
		switch prog[pc] {
		case rtPUSH:
			stack = append(stack, int64(prog[pc+1]))
			pc += 2
		case rtINPUT:
			stack = append(stack, in[prog[pc+1]])
			pc += 2
		case rtRT:
			n := len(stack)
			b := stack[n-1]
			stack[n-2] = rtBinop(int64(prog[pc+1]), stack[n-2], b)
			stack, pc = stack[:n-1], pc+2
		case rtLOAD:
			stack = append(stack, vars[prog[pc+1]])
			pc += 2
		case rtSTORE:
			n := len(stack)
			vars[prog[pc+1]] = stack[n-1]
			stack, pc = stack[:n-1], pc+2
		case rtJMP:
			pc = int(prog[pc+1])
		case rtJZ:
			n := len(stack)
			v := stack[n-1]
			stack = stack[:n-1]
			if v == 0 {
				pc = int(prog[pc+1])
			} else {
				pc += 2
			}
		case rtHALT:
			return stack[len(stack)-1]
		}
	}
}

// A loop whose body calls a runtime helper specializes to a residual loop that
// calls the helper directly -- no dispatch -- with the opcode selector a constant.
func TestSpecializeRuntimeCalls(t *testing.T) {
	// product = 1; i = in0;
	// while (i != 0) { product = rt(MUL, product, i); i = rt(SUB, i, 1); }
	// return product     -- i.e. factorial(in0)
	loop, end := byte(8), byte(30)
	prog := []byte{
		rtPUSH, 1, rtSTORE, 0, // product = 1
		rtINPUT, 0, rtSTORE, 1, // i = in0
		rtLOAD, 1, rtJZ, end, // loop: if (i == 0) goto end
		rtLOAD, 0, rtLOAD, 1, rtRT, rbMUL, rtSTORE, 0, // product = rt(MUL, product, i)
		rtLOAD, 1, rtPUSH, 1, rtRT, rbSUB, rtSTORE, 1, // i = rt(SUB, i, 1)
		rtJMP, loop,
		rtLOAD, 0, rtHALT, // end: return product
	}

	m, err := cc.Compile("toy.c", toyRTInterp)
	require.NoError(t, err)
	spec, name, inputs, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Len(t, inputs, 1)
	require.Contains(t, residual, "call $rt_binop", "the runtime call survives")
	require.NotContains(t, residual, "switch", "but the dispatch is gone")

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec, interp.WithExtern("rt_binop",
		func(_ *interp.Machine, a []interp.Value) (interp.Value, error) {
			return interp.L(rtBinop(a[0].I64(), a[1].I64(), a[2].I64())), nil
		}))
	require.NoError(t, err)

	for _, in0 := range []int64{0, 1, 5, 8, 10} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, refRunRT(prog, []int64{in0}), v.I64(), "specialized(%d)", in0)
	}
}
