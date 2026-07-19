package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// An interpreter whose operand stack holds a two-word tagged value -- a miniature
// JSValue -- rather than a bare long. Each element is a struct with a tag and a
// data field; handlers read and write the fields. The engine tracks the struct's
// cells on the 8-byte grain and carries a whole element (both words) across merge
// points, so the stack of structs specializes just like a stack of scalars.
const toyAggInterp = `
void __cg12_merge_point(int pc, int sp);
struct Val { long tag; long data; };
enum { OP_HALT, OP_PUSH, OP_INPUT, OP_ADD };

long interp(const unsigned char *code, const long *in) {
    struct Val stack[64];
    int sp = 0;
    int pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case OP_PUSH:  stack[sp].tag = 0; stack[sp].data = code[pc+1];   sp++; pc += 2; break;
        case OP_INPUT: stack[sp].tag = 1; stack[sp].data = in[code[pc+1]]; sp++; pc += 2; break;
        case OP_ADD: {
            long a = stack[sp-2].data, b = stack[sp-1].data;
            sp--;
            stack[sp-1].tag = 0;
            stack[sp-1].data = a + b;
            pc += 1; break;
        }
        case OP_HALT: return stack[sp-1].data;
        }
    }
}
`

const (
	agHALT = iota
	agPUSH
	agINPUT
	agADD
)

func TestSpecializeStructStack(t *testing.T) {
	// PUSH 2, INPUT 0, ADD, HALT  ->  2 + in[0]
	prog := []byte{agPUSH, 2, agINPUT, 0, agADD, agHALT}
	m, err := cc.Compile("toy.c", toyAggInterp)
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

// Several struct values live on the stack at once, so the intermediate merge points
// carry multiple two-word elements -- exercising the element stride.
func TestSpecializeStructStackDeep(t *testing.T) {
	// PUSH 2, PUSH 3, PUSH 4, ADD, ADD, INPUT 0, ADD, HALT  ->  2 + 3 + 4 + in[0]
	prog := []byte{
		agPUSH, 2, agPUSH, 3, agPUSH, 4,
		agADD, agADD,
		agINPUT, 0, agADD, agHALT,
	}
	m, err := cc.Compile("toy.c", toyAggInterp)
	require.NoError(t, err)
	spec, name, _, err := pe.Specialize(m, "interp", prog)
	require.NoError(t, err)

	opt.Run(spec, opt.DefaultPipeline())
	mc, err := interp.New(spec)
	require.NoError(t, err)
	for _, in0 := range []int64{5, 100} {
		v, err := mc.Call(name, interp.L(in0))
		require.NoError(t, err)
		require.Equal(t, 9+in0, v.I64(), "specialized(%d)", in0)
	}
}
