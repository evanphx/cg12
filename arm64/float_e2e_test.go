package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

func TestE2EFloatArith(t *testing.T) {
	// single: (a + b) * a
	m := ir.NewModule()
	f := m.NewFunc("farith", ir.ClsS).Export()
	a := f.Param("a", ir.ClsS)
	b := f.Param("b", ir.ClsS)
	e := f.Entry()
	e.Ret(e.Mul(ir.ClsS, e.Add(ir.ClsS, a, b), a))

	_, code := buildAndRun(t, m, `
extern float farith(float,float);
int main(void){
  float r = farith(1.5f, 2.5f); // (1.5+2.5)*1.5 = 6.0
  return r == 6.0f ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

func TestE2EDoubleArith(t *testing.T) {
	// double: (a - b) / c
	m := ir.NewModule()
	f := m.NewFunc("darith", ir.ClsD).Export()
	a := f.Param("a", ir.ClsD)
	b := f.Param("b", ir.ClsD)
	c := f.Param("c", ir.ClsD)
	e := f.Entry()
	e.Ret(e.Div(ir.ClsD, e.Sub(ir.ClsD, a, b), c))

	_, code := buildAndRun(t, m, `
extern double darith(double,double,double);
int main(void){
  double r = darith(10.0, 4.0, 3.0); // (10-4)/3 = 2.0
  return r == 2.0 ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

func TestE2EFloatConstant(t *testing.T) {
	// return x + 0.5 (double constant materialised via fmov)
	m := ir.NewModule()
	f := m.NewFunc("addhalf", ir.ClsD).Export()
	x := f.Param("x", ir.ClsD)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsD, x, f.Double(0.5)))

	_, code := buildAndRun(t, m, `
extern double addhalf(double);
int main(void){ return addhalf(2.25) == 2.75 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2EMixedIntFloatArgs(t *testing.T) {
	// Integer and float arguments occupy independent register banks.
	m := ir.NewModule()
	f := m.NewFunc("mixed", ir.ClsD).Export()
	i := f.Param("i", ir.ClsW)
	d := f.Param("d", ir.ClsD)
	j := f.Param("j", ir.ClsW)
	e := f.Entry()
	// (i + j) as double, times d
	sum := e.Add(ir.ClsW, i, j)
	sumd := e.Sltof(ir.ClsD, e.Extsw(ir.ClsL, sum)) // widen then int->double
	e.Ret(e.Mul(ir.ClsD, sumd, d))

	_, code := buildAndRun(t, m, `
extern double mixed(int, double, int);
int main(void){
  double r = mixed(3, 2.5, 5); // (3+5)*2.5 = 20.0
  return r == 20.0 ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

func TestE2EFloatCompare(t *testing.T) {
	// Returns 1 when a < b (ordered), else 0.
	m := ir.NewModule()
	f := m.NewFunc("lt", ir.ClsW).Export()
	a := f.Param("a", ir.ClsD)
	b := f.Param("b", ir.ClsD)
	e := f.Entry()
	e.Ret(e.Cmp(ir.CmpFlt, ir.ClsW, a, b))

	_, code := buildAndRun(t, m, `
extern int lt(double,double);
int main(void){
  return (lt(1.0,2.0)==1 && lt(2.0,1.0)==0 && lt(1.0,1.0)==0) ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

func TestE2EConversions(t *testing.T) {
	// f2i: (long)x ; i2f: (double)n ; narrow: (float)d
	m := ir.NewModule()
	f2i := m.NewFunc("f2i", ir.ClsL).Export()
	x := f2i.Param("x", ir.ClsD)
	f2i.Entry().Ret(f2i.Entry().Stosi(ir.ClsL, x))

	i2f := m.NewFunc("i2f", ir.ClsD).Export()
	n := i2f.Param("n", ir.ClsL)
	i2f.Entry().Ret(i2f.Entry().Sltof(ir.ClsD, n))

	narrow := m.NewFunc("narrow", ir.ClsS).Export()
	d := narrow.Param("d", ir.ClsD)
	narrow.Entry().Ret(narrow.Entry().Truncd(d))

	widen := m.NewFunc("widen", ir.ClsD).Export()
	s := widen.Param("s", ir.ClsS)
	widen.Entry().Ret(widen.Entry().Exts(s))

	_, code := buildAndRun(t, m, `
extern long f2i(double); extern double i2f(long);
extern float narrow(double); extern double widen(float);
int main(void){
  if (f2i(3.9) != 3) return 1;         // truncation toward zero
  if (i2f(7) != 7.0) return 1;
  if (narrow(1.5) != 1.5f) return 1;
  if (widen(2.5f) != 2.5) return 1;
  return 0;
}`)
	require.Equal(t, 0, code)
}

func TestE2EFloatLoopSum(t *testing.T) {
	// Sum an array of n doubles passed by pointer.
	m := ir.NewModule()
	f := m.NewFunc("dsum", ir.ClsD).Export()
	p := f.Param("p", ir.ClsL)
	n := f.Param("n", ir.ClsW)
	entry := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")

	entry.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
	s := loop.Phi(ir.ClsD, ir.PhiEdge{From: entry, Val: f.Double(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)

	off := body.Mul(ir.ClsL, body.Extsw(ir.ClsL, i), f.Long(8))
	elem := body.Load(ir.ClsD, body.Add(ir.ClsL, p, off))
	s2 := body.Add(ir.ClsD, s, elem)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)

	_, code := buildAndRun(t, m, `
extern double dsum(double*, int);
int main(void){
  double a[4] = {1.5, 2.5, 3.0, 4.0};
  return dsum(a, 4) == 11.0 ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}
