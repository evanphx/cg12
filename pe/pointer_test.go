package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// A pointer-walking interpreter -- the QuickJS style. pc and sp are pointers, not
// integer indices: the opcode is read with *pc++, values are pushed with *sp++ and
// popped with *--sp. The engine tracks each as a green address into the program and
// the stack, so the green state is their byte offsets, and specialization proceeds
// as it does for the indexed style.
const toyPtrInterp = `
void __cg12_merge_point(const unsigned char *pc, long *sp);

long interp(const unsigned char *code, const long *in) {
    long stack[64];
    long var[2];
    long *sp = stack;
    const unsigned char *pc = code;
    for (;;) {
        __cg12_merge_point(pc, sp);
        unsigned char op = *pc++;
        switch (op) {
        case 1: *sp++ = *pc++; break;                     /* PUSH  */
        case 2: *sp++ = in[*pc++]; break;                 /* INPUT */
        case 3: { long b = *--sp; sp[-1] += b; break; }   /* ADD   */
        case 4: { long b = *--sp; sp[-1] -= b; break; }   /* SUB   */
        case 5: *sp++ = var[*pc++]; break;                /* LOAD  */
        case 6: var[*pc++] = *--sp; break;                /* STORE */
        case 7: pc = code + *pc; break;                   /* JMP   */
        case 8: { long c = *--sp; unsigned char t = *pc++; if (c == 0) pc = code + t; } break; /* JZ */
        case 0: return sp[-1];                            /* HALT  */
        }
    }
}
`

const (
	pHALT  = 0
	pPUSH  = 1
	pINPUT = 2
	pADD   = 3
	pSUB   = 4
	pLOAD  = 5
	pSTORE = 6
	pJMP   = 7
	pJZ    = 8
)

func TestSpecializePointerWalk(t *testing.T) {
	// PUSH 2, INPUT 0, ADD, HALT  ->  2 + in[0]
	prog := []byte{pPUSH, 2, pINPUT, 0, pADD, pHALT}
	m, err := cc.Compile("toy.c", toyPtrInterp)
	require.NoError(t, err)
	spec, name, inputs, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)
	t.Logf("residual:\n%s", spec.String())
	require.Len(t, inputs, 1)

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100, -7} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 2+in0, v.I64(), "specialized(%d)", in0)
	}
}

func TestSpecializePointerWalkLoop(t *testing.T) {
	// sum = 0; i = in0; while (i != 0) { sum += i; i -= 1; } return sum
	loop, end := byte(8), byte(28)
	prog := []byte{
		pPUSH, 0, pSTORE, 0,
		pINPUT, 0, pSTORE, 1,
		pLOAD, 1, pJZ, end,
		pLOAD, 0, pLOAD, 1, pADD, pSTORE, 0,
		pLOAD, 1, pPUSH, 1, pSUB, pSTORE, 1,
		pJMP, loop,
		pLOAD, 0, pHALT,
	}
	m, err := cc.Compile("toy.c", toyPtrInterp)
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
