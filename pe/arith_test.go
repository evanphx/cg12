package pe_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/pe"
	"github.com/stretchr/testify/require"
)

// Division and remainder -- JS arithmetic uses them (a/b, a%b), and the residual
// must emit the div/rem it cannot fold rather than fail on the opcode.
const toyDivRem = `
void __cg12_merge_point(int pc, int sp);
long interp(const unsigned char *code, const long *in) {
    long stack[64];
    int sp = 0, pc = 0;
    for (;;) {
        __cg12_merge_point(pc, sp);
        switch (code[pc]) {
        case 1: stack[sp++] = in[code[pc+1]]; pc += 2; break;              /* INPUT */
        case 2: { long b = stack[--sp]; stack[sp-1] /= b; pc += 1; break; } /* DIV   */
        case 3: { long b = stack[--sp]; stack[sp-1] %= b; pc += 1; break; } /* MOD   */
        case 0: return stack[sp-1];
        }
    }
}
`

func TestSpecializeDivRem(t *testing.T) {
	m, err := cc.Compile("toy.c", toyDivRem)
	require.NoError(t, err)

	specialize := func(prog []byte) (*interp.Machine, string) {
		mm, _ := cc.Compile("toy.c", toyDivRem)
		spec, name, _, err := pe.Specialize(mm, "interp", prog)
		require.NoError(t, err)
		opt.Run(spec, opt.DefaultPipeline())
		mc, err := interp.New(spec)
		require.NoError(t, err)
		return mc, name
	}
	_ = m

	// INPUT 0, INPUT 1, DIV, HALT  ->  in0 / in1
	divMC, divName := specialize([]byte{1, 0, 1, 1, 2, 0})
	// INPUT 0, INPUT 1, MOD, HALT  ->  in0 % in1
	modMC, modName := specialize([]byte{1, 0, 1, 1, 3, 0})

	for _, tc := range []struct{ a, b int64 }{{20, 3}, {100, 7}, {-17, 5}} {
		v, err := divMC.Call(divName, interp.L(tc.a), interp.L(tc.b))
		require.NoError(t, err)
		require.Equal(t, tc.a/tc.b, v.I64(), "div %d/%d", tc.a, tc.b)
		v, err = modMC.Call(modName, interp.L(tc.a), interp.L(tc.b))
		require.NoError(t, err)
		require.Equal(t, tc.a%tc.b, v.I64(), "mod %d%%%d", tc.a, tc.b)
	}
}
