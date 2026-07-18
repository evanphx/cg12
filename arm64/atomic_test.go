package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// TestAtomicsE2E compiles every atomic intrinsic to arm64 (LDAR/STLR for
// load/store, an LDAXR/STLXR retry loop for the read-modify-writes, DMB for the
// fence) and runs the result through a C driver, so the encodings, the loop, and
// the memory ordering are exercised end to end. Each exported function performs
// one atomic operation and returns the value the driver checks.
func TestAtomicsE2E(t *testing.T) {
	m := ir.NewModule()

	// rmwL builds `long $name(long *p, long v) { ret atomic.<op>(p, v) }`.
	rmwL := func(name, op string) {
		f := m.NewFunc(name, ir.ClsL).Export()
		p, v := f.Param("p", ir.ClsL), f.Param("v", ir.ClsL)
		f.Entry().Ret(f.Entry().AtomicRMW(op, ir.ClsL, p, v))
	}
	rmwL("aadd", "add")
	rmwL("asub", "sub")
	rmwL("aand", "and")
	rmwL("aor", "or")
	rmwL("axor", "xor")

	// long axchg(long *p, long v): swap, return the previous value.
	{
		f := m.NewFunc("axchg", ir.ClsL).Export()
		p, v := f.Param("p", ir.ClsL), f.Param("v", ir.ClsL)
		f.Entry().Ret(f.Entry().AtomicXchg(ir.ClsL, p, v))
	}
	// long acas(long *p, long exp, long newv): CAS, return the value seen.
	{
		f := m.NewFunc("acas", ir.ClsL).Export()
		p := f.Param("p", ir.ClsL)
		exp, nw := f.Param("e", ir.ClsL), f.Param("n", ir.ClsL)
		f.Entry().Ret(f.Entry().AtomicCAS(ir.ClsL, p, exp, nw))
	}
	// int aaddw(int *p, int v): a 32-bit fetch-add, exercising the W-width loop.
	{
		f := m.NewFunc("aaddw", ir.ClsW).Export()
		p, v := f.Param("p", ir.ClsL), f.Param("v", ir.ClsW)
		f.Entry().Ret(f.Entry().AtomicRMW("add", ir.ClsW, p, v))
	}
	// int aloadw(int *p): acquire load.
	{
		f := m.NewFunc("aloadw", ir.ClsW).Export()
		p := f.Param("p", ir.ClsL)
		f.Entry().Ret(f.Entry().AtomicLoad(ir.ClsW, p))
	}
	// void astorew(int *p, int v): release store.
	{
		f := m.NewFuncVoid("astorew").Export()
		p, v := f.Param("p", ir.ClsL), f.Param("v", ir.ClsW)
		e := f.Entry()
		e.AtomicStore(ir.ClsW, p, v)
		e.RetVoid()
	}
	// void afence(): a full barrier.
	{
		f := m.NewFuncVoid("afence").Export()
		e := f.Entry()
		e.AtomicFence()
		e.RetVoid()
	}

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	main := `extern long aadd(long*, long);
extern long asub(long*, long);
extern long aand(long*, long);
extern long aor(long*, long);
extern long axor(long*, long);
extern long axchg(long*, long);
extern long acas(long*, long, long);
extern int  aaddw(int*, int);
extern int  aloadw(int*);
extern void astorew(int*, int);
extern void afence(void);

int main(void){
  long x = 10;
  if (axchg(&x, 20) != 10) return 1;   // returns old, x now 20
  if (x != 20)            return 2;
  if (aadd(&x, 5) != 20)  return 3;    // fetch-add returns old, x now 25
  if (x != 25)            return 4;
  if (asub(&x, 10) != 25) return 5;    // x now 15
  if (x != 15)            return 6;
  if (aand(&x, 12) != 15) return 7;    // 15 & 12 = 12
  if (x != 12)            return 8;
  if (aor(&x, 1) != 12)   return 9;    // 12 | 1 = 13
  if (x != 13)            return 10;
  if (axor(&x, 6) != 13)  return 11;   // 13 ^ 6 = 11
  if (x != 11)            return 12;

  if (acas(&x, 11, 99) != 11) return 13; // success, returns old 11
  if (x != 99)                return 14;
  if (acas(&x, 11, 7) != 99)  return 15; // mismatch, returns 99, no store
  if (x != 99)                return 16;

  int y = 42;
  if (aaddw(&y, 8) != 42) return 17;   // returns old, y now 50
  if (y != 50)            return 18;
  if (aloadw(&y) != 50)   return 19;
  astorew(&y, 77);
  if (y != 77)            return 20;

  afence();
  return 0;
}`
	require.Equal(t, 0, runObject(t, data, main))
}
