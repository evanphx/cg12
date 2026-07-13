package arm64_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// buildObjAndRun compiles m straight to an ELF object with the machine-code
// emitter (no external assembler), links the .o with a C driver, runs it, and
// returns stdout and the exit code.
func buildObjAndRun(t *testing.T, m *ir.Module, cmain string) (string, int) {
	t.Helper()
	cc, ok := assembler()
	if !ok {
		t.Skip("no AArch64 toolchain available")
	}
	code, err := arm64.CompileObject(m)
	require.NoError(t, err)

	dir := t.TempDir()
	objPath := filepath.Join(dir, "unit.o")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, code, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(cmain), 0o644))

	out, err := exec.Command(cc, "-o", bin, objPath, cPath).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s", out)

	cmd := exec.Command(bin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = cmd.Run()
	ec := 0
	if ee, ok := err.(*exec.ExitError); ok {
		ec = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return stdout.String(), ec
}

func TestObjEmitAdd(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcadd", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, e.Mul(ir.ClsW, a, b), e.Sub(ir.ClsW, a, b)))

	_, code := buildObjAndRun(t, m, `
extern int mcadd(int,int);
int main(void){ return mcadd(6,4)==(6*4)+(6-4) ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitSumtoLoop(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcsumto", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	c := loop.Cmp(ir.CmpSge, ir.ClsW, i, n)
	loop.Jnz(c, exit, body)
	s2 := body.Add(ir.ClsW, s, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)

	_, code := buildObjAndRun(t, m, `
extern int mcsumto(int);
int main(void){ return mcsumto(10)==45 && mcsumto(100)==4950 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// Two functions in one object: caller() invokes helper() through a CALL26
// relocation resolved within the .o.
func TestObjEmitInternalCall(t *testing.T) {
	m := ir.NewModule()
	h := m.NewFunc("mchelper", ir.ClsW)
	x := h.Param("x", ir.ClsW)
	he := h.Entry()
	he.Ret(he.Mul(ir.ClsW, x, h.Word(3)))

	f := m.NewFunc("mccaller", ir.ClsW).Export()
	a := f.Param("a", ir.ClsW)
	fe := f.Entry()
	sym := f.Sym("mchelper", 0)
	fe.Ret(fe.Add(ir.ClsW, fe.Call(ir.ClsW, sym, a), f.Word(1)))

	_, code := buildObjAndRun(t, m, `
extern int mccaller(int);
int main(void){ return mccaller(14)==14*3+1 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitSubwordMemory(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcround", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.StoreSub(ir.SubB, x, p)
	b := e.LoadSub(ir.ClsW, ir.SubUB, p)
	e.StoreSub(ir.SubH, x, p)
	hh := e.LoadSub(ir.ClsW, ir.SubH, p)
	e.Ret(e.Add(ir.ClsW, b, hh))

	_, code := buildObjAndRun(t, m, `
extern int mcround(int);
int main(void){
  int xs[]={0x1234,-1,0x80,0x7fff,0x8000};
  for(int i=0;i<5;i++){ int x=xs[i];
    unsigned char b=(unsigned char)x; short h=(short)x;
    if(mcround(x)!=(int)b+(int)h) return 1; }
  return 0; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitFloat(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcfma", ir.ClsD).Export()
	a := f.Param("a", ir.ClsD)
	b := f.Param("b", ir.ClsD)
	c := f.Param("c", ir.ClsD)
	e := f.Entry()
	// (a*b + c), then convert to int and back to exercise conversions.
	prod := e.Add(ir.ClsD, e.Mul(ir.ClsD, a, b), c)
	e.Ret(prod)

	_, code := buildObjAndRun(t, m, `
#include <math.h>
extern double mcfma(double,double,double);
int main(void){ return fabs(mcfma(2.5,4.0,1.5) - 11.5) < 1e-9 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitFloatConvert(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcd2i2d", ir.ClsD).Export()
	a := f.Param("a", ir.ClsD)
	e := f.Entry()
	i := e.Stosi(ir.ClsL, a)   // double -> signed 64
	e.Ret(e.Sltof(ir.ClsD, i)) // signed 64 -> double (truncated)

	_, code := buildObjAndRun(t, m, `
#include <math.h>
extern double mcd2i2d(double);
int main(void){ return fabs(mcd2i2d(7.9) - 7.0) < 1e-9 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitIndirectCall(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mccallfp", ir.ClsW).Export()
	fp := f.Param("fp", ir.ClsL)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Call(ir.ClsW, fp, x))

	_, code := buildObjAndRun(t, m, `
int dbl(int x){ return x*2; }
extern int mccallfp(int(*)(int), int);
int main(void){ return mccallfp(dbl, 21)==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitTailCall(t *testing.T) {
	m := ir.NewModule()
	tgt := m.NewFunc("mctarget", ir.ClsW)
	x := tgt.Param("x", ir.ClsW)
	te := tgt.Entry()
	te.Ret(te.Add(ir.ClsW, x, tgt.Word(100)))

	f := m.NewFunc("mctail", ir.ClsW).Export()
	a := f.Param("a", ir.ClsW)
	fe := f.Entry()
	fe.TailCall(ir.ClsW, f.Sym("mctarget", 0), fe.Mul(ir.ClsW, a, f.Word(2)))

	_, code := buildObjAndRun(t, m, `
extern int mctail(int);
int main(void){ return mctail(21)==21*2+100 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// A function reads a module-level data table via a symbol address (adrp/add
// relocations) resolved within the object.
func TestObjEmitDataReference(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:  "mctable",
		Align: 4,
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{10, 20, 30, 40}}},
	})

	f := m.NewFunc("mcget", ir.ClsW).Export()
	i := f.Param("i", ir.ClsL)
	e := f.Entry()
	off := e.Mul(ir.ClsL, i, f.Long(4))
	e.Ret(e.Load(ir.ClsW, e.Add(ir.ClsL, f.Sym("mctable", 0), off)))

	_, code := buildObjAndRun(t, m, `
extern int mcget(long);
int main(void){ return mcget(0)==10 && mcget(2)==30 && mcget(3)==40 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// A sweep of every integer comparison predicate, packed into a bitmask, checks
// that each maps to the right condition code.
func TestObjEmitIntCompareSweep(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mccmp", ir.ClsL).Export()
	a := f.Param("a", ir.ClsL)
	b := f.Param("b", ir.ClsL)
	e := f.Entry()
	preds := []ir.Cmp{
		ir.CmpEq, ir.CmpNe, ir.CmpSlt, ir.CmpSle, ir.CmpSgt, ir.CmpSge,
		ir.CmpUlt, ir.CmpUle, ir.CmpUgt, ir.CmpUge,
	}
	acc := f.Long(0)
	for i, p := range preds {
		bit := e.Cmp(p, ir.ClsL, a, b) // 0/1 in a W
		shifted := e.Shl(ir.ClsL, e.Extuw(ir.ClsL, bit), f.Long(int64(i)))
		acc = e.Or(ir.ClsL, acc, shifted)
	}
	e.Ret(acc)

	_, code := buildObjAndRun(t, m, `
extern long mccmp(long,long);
static long expect(long a,long b){
  unsigned long ua=a, ub=b;
  return (a==b)|((a!=b)<<1)|((a<b)<<2)|((a<=b)<<3)|((a>b)<<4)|((a>=b)<<5)
    |((long)(ua<ub)<<6)|((long)(ua<=ub)<<7)|((long)(ua>ub)<<8)|((long)(ua>=ub)<<9);
}
int main(void){
  long xs[]={-5, 0, 3, 7, -1};
  for(int i=0;i<5;i++)for(int j=0;j<5;j++)
    if(mccmp(xs[i],xs[j])!=expect(xs[i],xs[j])) return 1;
  return 0; }`)
	require.Equal(t, 0, code)
}

// Signed/unsigned remainder, sub-word sign/zero extension, and the shift family.
func TestObjEmitRemAndExtend(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcmix", ir.ClsL).Export()
	a := f.Param("a", ir.ClsL)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	rem := e.Rem(ir.ClsL, a, f.Long(7))   // signed remainder
	urem := e.URem(ir.ClsL, a, f.Long(7)) // unsigned remainder
	sb := e.Extsb(ir.ClsL, b)             // sign-extend byte
	sh := e.Extsh(ir.ClsL, b)             // sign-extend halfword
	ub := e.Extub(ir.ClsL, b)             // zero-extend byte
	uh := e.Extuh(ir.ClsL, b)             // zero-extend halfword
	sum := e.Add(ir.ClsL, e.Add(ir.ClsL, rem, urem), e.Add(ir.ClsL, e.Add(ir.ClsL, sb, sh), e.Add(ir.ClsL, ub, uh)))
	e.Ret(e.Sar(ir.ClsL, e.Shl(ir.ClsL, sum, f.Long(1)), f.Long(1))) // shift roundtrip

	_, code := buildObjAndRun(t, m, `
#include <stdint.h>
extern long mcmix(long, int);
static long expect(long a, int b){
  long rem=a%7, urem=(long)((unsigned long)a%7ul);
  long sb=(int8_t)b, sh=(int16_t)b, ub=(uint8_t)b, uh=(uint16_t)b;
  long sum=rem+urem+sb+sh+ub+uh; return (sum<<1)>>1;
}
int main(void){
  long as[]={100,-100,55,-9}; int bs[]={0x7f,-1,0x80,0x1234};
  for(int i=0;i<4;i++)for(int j=0;j<4;j++)
    if(mcmix(as[i],bs[j])!=expect(as[i],bs[j])) return 1;
  return 0; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitFloatCompareSweep(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcfcmp", ir.ClsW).Export()
	a := f.Param("a", ir.ClsD)
	b := f.Param("b", ir.ClsD)
	e := f.Entry()
	preds := []ir.Cmp{ir.CmpFeq, ir.CmpFne, ir.CmpFlt, ir.CmpFle, ir.CmpFgt, ir.CmpFge}
	acc := f.Word(0)
	for i, p := range preds {
		bit := e.Cmp(p, ir.ClsD, a, b)
		acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, bit, f.Word(int64(i))))
	}
	e.Ret(acc)

	_, code := buildObjAndRun(t, m, `
extern int mcfcmp(double,double);
static int expect(double a,double b){
  return (a==b)|((a!=b)<<1)|((a<b)<<2)|((a<=b)<<3)|((a>b)<<4)|((a>=b)<<5);
}
int main(void){
  double xs[]={-1.5, 0.0, 2.25, 3.5};
  for(int i=0;i<4;i++)for(int j=0;j<4;j++)
    if(mcfcmp(xs[i],xs[j])!=expect(xs[i],xs[j])) return 1;
  return 0; }`)
	require.Equal(t, 0, code)
}

// Recursive fib keeps a value live across two calls, forcing a callee-saved
// register (saved/restored in the prologue and teardown).
func TestObjEmitFibRecursive(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcfib", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	rec := f.NewBlock("rec")
	base := f.NewBlock("base")
	start.Jnz(start.Cmp(ir.CmpSlt, ir.ClsW, n, f.Word(2)), base, rec)
	base.Ret(n)
	a := rec.Call(ir.ClsW, f.Sym("mcfib", 0), rec.Sub(ir.ClsW, n, f.Word(1)))
	b := rec.Call(ir.ClsW, f.Sym("mcfib", 0), rec.Sub(ir.ClsW, n, f.Word(2)))
	rec.Ret(rec.Add(ir.ClsW, a, b))

	_, code := buildObjAndRun(t, m, `
extern int mcfib(int);
int main(void){ return mcfib(10)==55 && mcfib(15)==610 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// A ten-argument call exercises stacked arguments (in the caller) and stacked
// parameters (in the callee).
func TestObjEmitStackArgs(t *testing.T) {
	m := ir.NewModule()
	// callee sums its 10 parameters.
	sum := m.NewFunc("mcsum10", ir.ClsW)
	var ps []ir.Ref
	for i := 0; i < 10; i++ {
		ps = append(ps, sum.Param(string(rune('a'+i)), ir.ClsW))
	}
	se := sum.Entry()
	acc := ps[0]
	for i := 1; i < 10; i++ {
		acc = se.Add(ir.ClsW, acc, ps[i])
	}
	se.Ret(acc)

	// caller passes 1..10 (two of them on the stack).
	f := m.NewFunc("mccall10", ir.ClsW).Export()
	fe := f.Entry()
	var args []ir.Ref
	for i := 1; i <= 10; i++ {
		args = append(args, f.Word(int64(i)))
	}
	fe.Ret(fe.Call(ir.ClsW, f.Sym("mcsum10", 0), args...))

	_, code := buildObjAndRun(t, m, `
extern int mccall10(void);
int main(void){ return mccall10()==55 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// An aggregate passed by value (a two-word struct arriving in a register) is
// reconstructed on the stack, loaded, and summed — end to end.
func TestObjEmitAggregateArg(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	f := m.NewFunc("mcsum2", ir.ClsW).Export()
	p := f.NewTemp("p", ir.ClsL)
	f.Temp(p).Agg = pair
	f.Params = append(f.Params, f.Temp(p))
	e := f.Entry()
	lo := e.Load(ir.ClsW, p)
	hi := e.Load(ir.ClsW, e.Add(ir.ClsL, p, f.Long(4)))
	e.Ret(e.Add(ir.ClsW, lo, hi))

	_, code := buildObjAndRun(t, m, `
struct pair { int a, b; };
extern int mcsum2(struct pair);
int main(void){ struct pair p={7,35}; return mcsum2(p)==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// A pointer stored in data (an ABS64 .rela.data relocation) points into another
// data symbol; the function dereferences it at runtime.
func TestObjEmitDataPointer(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{
			Name:  "mcvalues",
			Align: 4,
			Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{10, 20, 30, 40}}},
		},
		&ir.Data{
			Name:  "mcvptr",
			Align: 8,
			Items: []ir.DataItem{{Sym: "mcvalues", Off: 8}}, // &mcvalues[2]
		},
	)

	f := m.NewFunc("mcderef", ir.ClsW).Export()
	e := f.Entry()
	ptr := e.Load(ir.ClsL, f.Sym("mcvptr", 0)) // load the stored pointer
	e.Ret(e.Load(ir.ClsW, ptr))                // *ptr == mcvalues[2]

	_, code := buildObjAndRun(t, m, `
extern int mcderef(void);
int main(void){ return mcderef()==30 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestObjEmitVariadic(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mcsum", ir.ClsW).Export()
	f.Variadic = true
	f.Param("n", ir.ClsW)
	e := f.Entry()
	ap := e.Alloc(8, 32)
	e.VaStart(ap)
	// sum the first three variadic ints
	acc := e.VaArg(ir.ClsW, ap)
	acc = e.Add(ir.ClsW, acc, e.VaArg(ir.ClsW, ap))
	acc = e.Add(ir.ClsW, acc, e.VaArg(ir.ClsW, ap))
	e.Ret(acc)

	_, code := buildObjAndRun(t, m, `
extern int mcsum(int, ...);
int main(void){ return mcsum(3, 11, 22, 33)==66 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}
