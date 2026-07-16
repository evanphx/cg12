package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImmediateAndRotateCodegen checks that constant operands fold into
// immediate instruction forms and that the shift-or rotate idiom becomes a
// single ROR — and that the folded code still computes the right answer.
func TestImmediateAndRotateCodegen(t *testing.T) {
	build := func() *ir.Module {
		m := ir.NewModule()
		f := m.NewFunc("f", ir.ClsW).Export()
		x := f.Param("x", ir.ClsW)
		e := f.Entry()
		// A rotate-right-by-7 idiom: (x >> 7) | (x << 25).
		rot := e.Or(ir.ClsW, e.Shr(ir.ClsW, x, f.Word(7)), e.Shl(ir.ClsW, x, f.Word(25)))
		s := e.Add(ir.ClsW, rot, f.Word(100)) // add immediate
		s = e.And(ir.ClsW, s, f.Word(0xff))   // logical immediate
		e.Ret(s)
		return m
	}

	asm := disasmModule(t, build())
	assert.Contains(t, asm, "\tror ", "the shift-or idiom folds to a rotate")
	assert.NotContains(t, asm, "\tlsl ", "the rotate's shifts are gone")
	assert.Contains(t, asm, ", #100", "add folds a 12-bit immediate")
	assert.Contains(t, asm, ", #0xff", "and folds a logical immediate")
	assert.NotContains(t, asm, "#0x64", "the add constant is not materialized into a register")

	// It still computes ((rotr(x,7) + 100) & 0xff). For x=0x80: rotr=1, +100=101.
	_, code := buildAndRun(t, build(), `
extern unsigned f(unsigned);
int main(void){ return f(0x80) == 101 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// TestIndexedAddressingCodegen checks that a scaled array index (base +
// sext(i)*4) folds into an AArch64 register-offset load, and that the folded
// code loads the right element.
func TestIndexedAddressingCodegen(t *testing.T) {
	build := func() *ir.Module {
		m := ir.NewModule()
		f := m.NewFunc("elem", ir.ClsW).Export()
		a := f.Param("a", ir.ClsL) // int *a
		i := f.Param("i", ir.ClsW) // int i
		e := f.Entry()
		// a[i]: addr = a + sext(i) * 4; load the signed word there.
		off := e.Shl(ir.ClsL, e.Extsw(ir.ClsL, i), f.Long(2))
		e.Ret(e.LoadSub(ir.ClsW, ir.SubW, e.Add(ir.ClsL, a, off)))
		return m
	}

	asm := disasmModule(t, build())
	assert.Contains(t, asm, "sxtw #2]", "the scaled int index folds into a register offset")
	assert.NotContains(t, asm, "\tmul ", "the scale is a fold, not a multiply")

	// a[2] of {10,20,30,40,50} is 30.
	_, code := buildAndRun(t, build(), `
extern int elem(const int *, int);
int main(void){ int a[5] = {10,20,30,40,50}; return elem(a, 2) == 30 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// TestBinaryUnitCompilesAndRuns caches a module to bytes, reloads it, and
// compiles the reloaded unit — proving a cached unit is a complete, usable
// module (the point of the format: skip the front end on a cache hit).
func TestBinaryUnitCompilesAndRuns(t *testing.T) {
	build := func() *ir.Module {
		m := ir.NewModule()
		f := m.NewFunc("f", ir.ClsW).Export()
		a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
		e := f.Entry()
		// ((a + b) * a) - (b / 2)
		s := e.Add(ir.ClsW, a, b)
		p := e.Mul(ir.ClsW, s, a)
		e.Ret(e.Sub(ir.ClsW, p, e.Div(ir.ClsW, b, f.Word(2))))
		return m
	}

	// Compilation mutates the module, so encode the pristine one first.
	data, err := build().MarshalBinary()
	require.NoError(t, err)

	decoded, err := ir.DecodeModule(data)
	require.NoError(t, err)

	// Two fresh decodes compile to identical code (deterministic + complete).
	m1, _ := ir.DecodeModule(data)
	m2, _ := ir.DecodeModule(data)
	assert.Equal(t, disasmModule(t, m1), disasmModule(t, m2))

	// The decoded unit runs correctly: f(3,4) = (7*3) - (4/2) = 19.
	_, code := buildAndRun(t, decoded, `
extern int f(int, int);
int main(void){ return f(3, 4) == 19 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}
