package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// Three integer variadic arguments summed from the register save area.
func TestObjVariadicIntSum(t *testing.T) {
	m := ir.NewModule()
	vsum := m.NewFunc("vsum", ir.ClsW)
	vsum.Variadic = true
	vsum.Param("n", ir.ClsW)
	ve := vsum.Entry()
	ap := ve.Alloc(8, 32)
	ve.VaStart(ap)
	acc := ve.VaArg(ir.ClsW, ap)
	acc = ve.Add(ir.ClsW, acc, ve.VaArg(ir.ClsW, ap))
	acc = ve.Add(ir.ClsW, acc, ve.VaArg(ir.ClsW, ap))
	ve.Ret(acc)

	f, e := entry(m)
	r := e.Call(ir.ClsW, f.Sym("vsum", 0), f.Word(3), f.Word(11), f.Word(22), f.Word(33))
	e.Ret(e.Sub(ir.ClsW, r, f.Word(66)))
	require.Equal(t, 0, runObj(t, m))
}

// Mixed integer and double variadic arguments exercise both the gp_offset and
// fp_offset paths of va_arg.
func TestObjVariadicMixed(t *testing.T) {
	m := ir.NewModule()
	vmix := m.NewFunc("vmix", ir.ClsW)
	vmix.Variadic = true
	vmix.Param("n", ir.ClsW)
	ve := vmix.Entry()
	ap := ve.Alloc(8, 32)
	ve.VaStart(ap)
	i := ve.VaArg(ir.ClsW, ap) // int vararg
	d := ve.VaArg(ir.ClsD, ap) // double vararg
	j := ve.VaArg(ir.ClsW, ap) // int vararg
	ve.Ret(ve.Add(ir.ClsW, ve.Add(ir.ClsW, i, j), ve.Stosi(ir.ClsW, d)))

	f, e := entry(m)
	// vmix(3, 10, 2.5, 30) -> 10 + 30 + int(2.5)=2 == 42.
	r := e.Call(ir.ClsW, f.Sym("vmix", 0), f.Word(3), f.Word(10), f.Double(2.5), f.Word(30))
	e.Ret(e.Sub(ir.ClsW, r, f.Word(42)))
	require.Equal(t, 0, runObj(t, m))
}

// Enough integer varargs to exhaust the 6 GP registers, so later ones come from
// the overflow (stack) area.
func TestObjVariadicOverflow(t *testing.T) {
	m := ir.NewModule()
	vsum := m.NewFunc("vsumn", ir.ClsW)
	vsum.Variadic = true
	vsum.Param("n", ir.ClsW)
	ve := vsum.Entry()
	ap := ve.Alloc(8, 32)
	ve.VaStart(ap)
	acc := ve.VaArg(ir.ClsW, ap)
	for i := 1; i < 8; i++ { // 8 varargs total; 5 fit in regs (n took rdi), 3 overflow
		acc = ve.Add(ir.ClsW, acc, ve.VaArg(ir.ClsW, ap))
	}
	ve.Ret(acc)

	f, e := entry(m)
	args := []ir.Ref{f.Word(8)}
	for i := 1; i <= 8; i++ {
		args = append(args, f.Word(int64(i)))
	}
	r := e.Call(ir.ClsW, f.Sym("vsumn", 0), args...) // sum 1..8 = 36
	e.Ret(e.Sub(ir.ClsW, r, f.Word(36)))
	require.Equal(t, 0, runObj(t, m))
}
