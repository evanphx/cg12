package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// A select lowers to csel (integer) / fcsel (float). These compile the op and run
// the result through a C driver, so the encodings are exercised end to end.
func TestSelectCsel(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("seli", ir.ClsL).Export()
	c, a, b := f.Param("c", ir.ClsL), f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsL, c, a, b))

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	main := `extern long seli(long,long,long);
int main(){
  if (seli(1,7,9)   != 7)   return 1;
  if (seli(0,7,9)   != 9)   return 2;
  if (seli(5,100,-1)!= 100) return 3;
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// The stacksave/stackrestore intrinsics lower to SP moves (the OIntrinsic
// dispatch replacing the former OStackSave/OStackRestore ops). This brackets a
// runtime stack allocation with them and checks the value survives and the stack
// pointer is left where it began, end to end.
func TestStackIntrinsics(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("stk", ir.ClsL).Export()
	e := f.Entry()
	sp := e.StackSave()
	p := e.AllocN(f.Long(16)) // a 16-byte dynamic allocation
	e.Store(f.Long(42), p)
	v := e.Load(ir.ClsL, p)
	e.StackRestore(sp) // reclaim it
	e.Ret(v)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	main := `extern long stk(void);
int main(){ return stk()==42 ? 0 : 1; }`
	require.Equal(t, 0, runObject(t, data, main))
}

// A select whose condition is a comparison fuses to `cmp; csel <cond>`. This
// exercises the fused encoding end to end across a signed, unsigned, and
// equality predicate, checking the condition mapping is right in each direction.
func TestSelectFusedCompare(t *testing.T) {
	m := ir.NewModule()
	// r = (a < b) ? a : b   (signed) -- returns the min
	mn := m.NewFunc("fmin", ir.ClsL).Export()
	{
		a, b := mn.Param("a", ir.ClsL), mn.Param("b", ir.ClsL)
		e := mn.Entry()
		e.Ret(e.Select(ir.ClsL, e.Cmp(ir.CmpSlt, ir.ClsL, a, b), a, b))
	}
	// r = (a == b) ? a : b
	eq := m.NewFunc("feq", ir.ClsL).Export()
	{
		a, b := eq.Param("a", ir.ClsL), eq.Param("b", ir.ClsL)
		e := eq.Entry()
		e.Ret(e.Select(ir.ClsL, e.Cmp(ir.CmpEq, ir.ClsL, a, b), eq.ConstInt(ir.ClsL, 1), b))
	}

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	main := `extern long fmin(long,long);
extern long feq(long,long);
int main(){
  if (fmin(3,9)  != 3) return 1;
  if (fmin(9,3)  != 3) return 2;
  if (fmin(-5,2) != -5) return 3;
  if (feq(4,4)   != 1) return 4;  /* equal -> first arm (1) */
  if (feq(4,5)   != 5) return 5;  /* unequal -> second arm (b) */
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// A mask-test condition `(x & mask) ? a : b` fuses to `tst; csel ne`.
func TestSelectFusedMask(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("fmask", ir.ClsL).Export()
	x, a, b := f.Param("x", ir.ClsL), f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsL, e.And(ir.ClsL, x, f.ConstInt(ir.ClsL, 8)), a, b))

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	main := `extern long fmask(long,long,long);
int main(){
  if (fmask(8, 7, 9)  != 7) return 1;  /* bit set   -> a */
  if (fmask(15,7, 9)  != 7) return 2;  /* bit set   -> a */
  if (fmask(4, 7, 9)  != 9) return 3;  /* bit clear -> b */
  if (fmask(0, 7, 9)  != 9) return 4;  /* bit clear -> b */
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// A float-armed select on an integer comparison fuses to `cmp; fcsel <cond>`,
// and an integer-armed select on a float comparison to `fcmp; csel <cond>` --
// the compare's operand class and the select's result class are independent.
func TestSelectFusedClassMixes(t *testing.T) {
	m := ir.NewModule()
	// double result, integer condition: (a < b) ? x : y
	fi := m.NewFunc("selfi", ir.ClsD).Export()
	{
		a, b := fi.Param("a", ir.ClsL), fi.Param("b", ir.ClsL)
		x, y := fi.Param("x", ir.ClsD), fi.Param("y", ir.ClsD)
		e := fi.Entry()
		e.Ret(e.Select(ir.ClsD, e.Cmp(ir.CmpSlt, ir.ClsL, a, b), x, y))
	}
	// integer result, float condition: (x < y) ? a : b
	iflt := m.NewFunc("selif", ir.ClsL).Export()
	{
		x, y := iflt.Param("x", ir.ClsD), iflt.Param("y", ir.ClsD)
		a, b := iflt.Param("a", ir.ClsL), iflt.Param("b", ir.ClsL)
		e := iflt.Entry()
		e.Ret(e.Select(ir.ClsL, e.Cmp(ir.CmpFlt, ir.ClsL, x, y), a, b))
	}

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	main := `extern double selfi(long,long,double,double);
extern long   selif(double,double,long,long);
int main(){
  if (selfi(1,2,1.5,2.5) != 1.5) return 1;
  if (selfi(2,1,1.5,2.5) != 2.5) return 2;
  if (selif(1.0,2.0,7,9) != 7)   return 3;
  if (selif(2.0,1.0,7,9) != 9)   return 4;
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// A constant-zero arm selects the zero register; this checks `c ? a : 0` and
// `c ? 0 : b` both compute correctly through the xzr path.
func TestSelectFusedZeroArm(t *testing.T) {
	m := ir.NewModule()
	hi := m.NewFunc("relu", ir.ClsL).Export() // (a > 0) ? a : 0
	{
		a := hi.Param("a", ir.ClsL)
		e := hi.Entry()
		e.Ret(e.Select(ir.ClsL, e.Cmp(ir.CmpSgt, ir.ClsL, a, hi.ConstInt(ir.ClsL, 0)), a, hi.ConstInt(ir.ClsL, 0)))
	}

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	main := `extern long relu(long);
int main(){
  if (relu(5)  != 5) return 1;
  if (relu(-5) != 0) return 2;
  if (relu(0)  != 0) return 3;
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// Under enough register pressure to spill the select's operands, the fused
// `cmp; csel` must still compute correctly -- every operand materialization
// between the compare and the csel is flags-preserving, so the flags survive.
func TestSelectFusedUnderSpill(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("spillsel", ir.ClsL).Export()
	// 12 parameters, all summed after the select, so they are all live across
	// it and the allocator must spill -- including the select's own operands.
	ps := make([]ir.Ref, 12)
	names := []string{"a", "b", "c", "d", "e", "g", "h", "i", "j", "k", "l", "n"}
	for i := range ps {
		ps[i] = f.Param(names[i], ir.ClsL)
	}
	e := f.Entry()
	sel := e.Select(ir.ClsL, e.Cmp(ir.CmpSlt, ir.ClsL, ps[0], ps[1]), ps[2], ps[3])
	total := sel
	for _, p := range ps {
		total = e.Add(ir.ClsL, total, p)
	}
	e.Ret(total)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	// sum(1..12) = 78; select is (1<2)?3:4 = 3; total = 78 + 3 = 81.
	main := `extern long spillsel(long,long,long,long,long,long,long,long,long,long,long,long);
int main(){
  long r = spillsel(1,2,3,4,5,6,7,8,9,10,11,12);
  return r == 81 ? 0 : 1;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// A conjoined branch condition `if (a<b && c<d)` fuses to cmp; ccmp; b.cond.
// This runs the full truth table end to end so the ccmp nzcv and condition are
// exercised in every combination.
func TestConjoinedAndBranch(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("band", ir.ClsL).Export()
	a, b, c, d := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL), f.Param("c", ir.ClsL), f.Param("d", ir.ClsL)
	e := f.Entry()
	yes, no := f.NewBlock("yes"), f.NewBlock("no")
	c1 := e.Cmp(ir.CmpSlt, ir.ClsL, a, b)
	c2 := e.Cmp(ir.CmpSlt, ir.ClsL, c, d)
	e.Jnz(e.And(ir.ClsL, c1, c2), yes, no)
	yes.Ret(f.ConstInt(ir.ClsL, 1))
	no.Ret(f.ConstInt(ir.ClsL, 0))

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	// band returns 1 iff a<b AND c<d.
	main := `extern long band(long,long,long,long);
int main(){
  if (band(1,2,3,4) != 1) return 1;  /* T&&T */
  if (band(2,1,3,4) != 0) return 2;  /* F&&T */
  if (band(1,2,4,3) != 0) return 3;  /* T&&F */
  if (band(2,1,4,3) != 0) return 4;  /* F&&F */
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// A disjoined branch `if (a>b || c==5)` fuses to cmp; ccmp; b.cond, with the
// ccmp using the inverted first condition and a pass-forward nzcv.
func TestConjoinedOrBranch(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("bor", ir.ClsL).Export()
	a, b, c := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL), f.Param("c", ir.ClsL)
	e := f.Entry()
	yes, no := f.NewBlock("yes"), f.NewBlock("no")
	c1 := e.Cmp(ir.CmpSgt, ir.ClsL, a, b)
	c2 := e.Cmp(ir.CmpEq, ir.ClsL, c, f.ConstInt(ir.ClsL, 5))
	e.Jnz(e.Or(ir.ClsL, c1, c2), yes, no)
	yes.Ret(f.ConstInt(ir.ClsL, 1))
	no.Ret(f.ConstInt(ir.ClsL, 0))

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	// bor returns 1 iff a>b OR c==5.
	main := `extern long bor(long,long,long);
int main(){
  if (bor(9,1,0) != 1) return 1;  /* T||F */
  if (bor(1,9,5) != 1) return 2;  /* F||T */
  if (bor(9,1,5) != 1) return 3;  /* T||T */
  if (bor(1,9,3) != 0) return 4;  /* F||F */
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// Mixed predicates, unsigned, and a register second operand exercise the
// condition mapping and the register ccmp form.
func TestConjoinedMixedPredicates(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("bmix", ir.ClsL).Export()
	x, y, z := f.Param("x", ir.ClsL), f.Param("y", ir.ClsL), f.Param("z", ir.ClsL)
	e := f.Entry()
	yes, no := f.NewBlock("yes"), f.NewBlock("no")
	c1 := e.Cmp(ir.CmpUlt, ir.ClsL, x, y) // unsigned <
	c2 := e.Cmp(ir.CmpNe, ir.ClsL, z, x)  // register operand
	e.Jnz(e.And(ir.ClsL, c1, c2), yes, no)
	yes.Ret(f.ConstInt(ir.ClsL, 1))
	no.Ret(f.ConstInt(ir.ClsL, 0))

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	main := `extern long bmix(unsigned long,unsigned long,unsigned long);
int main(){
  if (bmix(1,2,9) != 1) return 1;  /* 1<2 && 9!=1 */
  if (bmix(2,1,9) != 0) return 2;  /* !(2<1) */
  if (bmix(1,2,1) != 0) return 3;  /* 1<2 && !(1!=1) */
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

// Reading and writing a global through the folded symbol addressing must be
// correct end to end -- the linker patches the LDSTn_ABS_LO12_NC relocation into
// the access's scaled-offset field.
func TestSymbolAddressFoldE2E(t *testing.T) {
	m := ir.NewModule()
	m.NoteSymAlign("counter", 8) // 8-aligned long -> folds
	rd := m.NewFunc("readg", ir.ClsL).Export()
	e := rd.Entry()
	e.Ret(e.Load(ir.ClsL, rd.Sym("counter", 0)))
	st := m.NewFuncVoid("setg").Export()
	v := st.Param("v", ir.ClsL)
	e2 := st.Entry()
	e2.Store(v, st.Sym("counter", 0))
	e2.RetVoid()

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	main := `extern long readg(void); extern void setg(long);
long counter = 12345;
int main(){
  if (readg() != 12345) return 1;   /* initial value through the fold */
  setg(99999);                        /* store through the fold */
  if (readg() != 99999) return 2;
  if (counter != 99999) return 3;
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}

func TestSelectFcsel(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("seld", ir.ClsD).Export()
	c := f.Param("c", ir.ClsL)
	a, b := f.Param("a", ir.ClsD), f.Param("b", ir.ClsD)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsD, c, a, b))

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	main := `extern double seld(long,double,double);
int main(){
  if (seld(1,1.5,2.5) != 1.5) return 1;
  if (seld(0,1.5,2.5) != 2.5) return 2;
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}
