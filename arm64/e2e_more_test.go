package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

func TestE2ESubwordMemory(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("roundtrip", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.StoreSub(ir.SubB, x, p)
	b := e.LoadSub(ir.ClsW, ir.SubUB, p) // unsigned byte
	e.StoreSub(ir.SubH, x, p)
	h := e.LoadSub(ir.ClsW, ir.SubH, p) // signed halfword
	e.Ret(e.Add(ir.ClsW, b, h))

	_, code := buildAndRun(t, m, `
extern int roundtrip(int);
int main(void){
  int xs[]={0x1234, -1, 0x80, 0x7fff, 0x8000};
  for(int i=0;i<5;i++){ int x=xs[i];
    unsigned char b=(unsigned char)x; short h=(short)x;
    if(roundtrip(x) != (int)b+(int)h) return 1; }
  return 0;
}`)
	require.Equal(t, 0, code)
}

func TestE2EIndirectCall(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("callfp", ir.ClsW).Export()
	fp := f.Param("fp", ir.ClsL)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Call(ir.ClsW, fp, x))

	_, code := buildAndRun(t, m, `
int dbl(int x){ return x*2; }
extern int callfp(int(*)(int), int);
int main(void){ return callfp(dbl, 21)==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2ELogicalShiftRight(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("lsr4", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Shr(ir.ClsW, x, f.Word(4))) // logical

	_, code := buildAndRun(t, m, `
#include <stdint.h>
extern int lsr4(unsigned);
int main(void){ unsigned x=0xffffffffu; return (unsigned)lsr4(x)==(x>>4) ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2EDataGlobal(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:    "answer",
		Linkage: ir.Linkage{Export: true},
		Align:   8,
		Items:   []ir.DataItem{{Sub: ir.SubL, Ints: []int64{42}}},
	})
	f := m.NewFunc("getanswer", ir.ClsL).Export()
	e := f.Entry()
	e.Ret(e.Load(ir.ClsL, f.Sym("answer", 0)))

	_, code := buildAndRun(t, m, `
extern long getanswer(void);
extern long answer;
int main(void){ return getanswer()==42 && answer==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2ENegAndCompareChain(t *testing.T) {
	// Covers neg plus several compare predicates in one runnable function.
	m := ir.NewModule()
	f := m.NewFunc("cmps", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	r := e.Cmp(ir.CmpSlt, ir.ClsW, a, b)
	r = e.Add(ir.ClsW, r, e.Cmp(ir.CmpSle, ir.ClsW, a, b))
	r = e.Add(ir.ClsW, r, e.Cmp(ir.CmpSgt, ir.ClsW, a, b))
	r = e.Add(ir.ClsW, r, e.Cmp(ir.CmpSge, ir.ClsW, a, b))
	r = e.Add(ir.ClsW, r, e.Cmp(ir.CmpEq, ir.ClsW, a, b))
	r = e.Add(ir.ClsW, r, e.Cmp(ir.CmpNe, ir.ClsW, a, b))
	r = e.Add(ir.ClsW, r, e.Neg(ir.ClsW, a))
	e.Ret(r)

	_, code := buildAndRun(t, m, `
extern int cmps(int,int);
static int c(int a,int b){ return (a<b)+(a<=b)+(a>b)+(a>=b)+(a==b)+(a!=b)+(-a); }
int main(void){
  int p[][2]={{1,2},{2,1},{3,3},{-4,5}};
  for(int i=0;i<4;i++) if(cmps(p[i][0],p[i][1])!=c(p[i][0],p[i][1])) return 1;
  return 0;
}`)
	require.Equal(t, 0, code)
}
