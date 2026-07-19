package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// A computed-goto interpreter -- the dispatch QuickJS and other fast interpreters
// use: goto *dispatch[code[pc]], threading the next opcode through a table of label
// addresses. cc funnels every such goto through one indirect branch; the engine
// resolves it because the opcode is green, so it reads the block address from the
// dispatch table and follows it, exactly as it did the switch.
const toyCGInterp = `
void __cg12_merge_point(int pc, int sp);

#define DISPATCH() do { __cg12_merge_point(pc, sp); goto *dispatch[code[pc]]; } while (0)

long interp(const unsigned char *code, const long *in) {
    static const void *dispatch[] = {
        &&op_halt, &&op_push, &&op_input, &&op_add,
        &&op_sub, &&op_load, &&op_store, &&op_jmp, &&op_jz,
    };
    long stack[64];
    long var[2];
    int sp = 0;
    int pc = 0;
    DISPATCH();
  op_push:  stack[sp++] = code[pc+1];       pc += 2; DISPATCH();
  op_input: stack[sp++] = in[code[pc+1]];   pc += 2; DISPATCH();
  op_add:   sp--; stack[sp-1] += stack[sp];  pc += 1; DISPATCH();
  op_sub:   sp--; stack[sp-1] -= stack[sp];  pc += 1; DISPATCH();
  op_load:  stack[sp++] = var[code[pc+1]];   pc += 2; DISPATCH();
  op_store: var[code[pc+1]] = stack[--sp];   pc += 2; DISPATCH();
  op_jmp:   pc = code[pc+1];                          DISPATCH();
  op_jz:    if (stack[--sp] == 0) pc = code[pc+1]; else pc += 2; DISPATCH();
  op_halt:  return stack[sp-1];
}
`

const (
	cgHALT = iota
	cgPUSH
	cgINPUT
	cgADD
	cgSUB
	cgLOAD
	cgSTORE
	cgJMP
	cgJZ
)

// A straight-line program specialized through the computed-goto dispatch folds to
// its arithmetic, the dispatch table and indirect branch gone.
func TestSpecializeComputedGoto(t *testing.T) {
	// PUSH 2, INPUT 0, ADD, HALT  ->  2 + in[0]
	prog := []byte{cgPUSH, 2, cgINPUT, 0, cgADD, cgHALT}
	m, err := cc.Compile("toy.c", toyCGInterp)
	require.NoError(t, err)
	spec, name, inputs, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	residual := spec.String()
	t.Logf("residual:\n%s", residual)
	require.Len(t, inputs, 1)
	require.NotContains(t, residual, "jmp *", "the computed goto is resolved away")

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100, -7} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 2+in0, v.I64(), "specialized(%d)", in0)
	}
}

// A loop program through the computed-goto dispatch specializes to a residual loop,
// just as it does through a switch: dispatch resolution and merge-point memoization
// compose.
func TestSpecializeComputedGotoLoop(t *testing.T) {
	// sum = 0; i = in0; while (i != 0) { sum += i; i -= 1; } return sum
	loop, end := byte(8), byte(28)
	prog := []byte{
		cgPUSH, 0, cgSTORE, 0,
		cgINPUT, 0, cgSTORE, 1,
		cgLOAD, 1, cgJZ, end,
		cgLOAD, 0, cgLOAD, 1, cgADD, cgSTORE, 0,
		cgLOAD, 1, cgPUSH, 1, cgSUB, cgSTORE, 1,
		cgJMP, loop,
		cgLOAD, 0, cgHALT,
	}
	m, err := cc.Compile("toy.c", toyCGInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)
	for _, in0 := range []int64{0, 1, 5, 10, 100} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, in0*(in0+1)/2, v.I64(), "specialized(%d)", in0)
	}
}
