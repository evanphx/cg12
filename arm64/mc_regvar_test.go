package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// A register variable reads the live value of a machine register: reading x0 at
// entry yields the incoming argument.
func TestRegVarReadRegister(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("readx0", ir.ClsL).Export()
	e := f.Entry()
	x0 := f.RegVar("x0", int(arm64.X0))
	e.Ret(e.Load(ir.ClsL, x0)) // return whatever is in register x0

	_, code := buildObjAndRun(t, m, `
extern long readx0(long);
int main(void){ return readx0(0x12345678L) == 0x12345678L ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// Writing then reading a register round-trips the value. x18 (the platform
// register) is never touched by the allocator, so the round-trip is exact.
func TestRegVarWriteReadRoundTrip(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("rt", ir.ClsL).Export()
	e := f.Entry()
	x0 := f.RegVar("x0", int(arm64.X0))
	x18 := f.RegVar("x18", int(arm64.X18))
	v := e.Load(ir.ClsL, x0) // v = incoming arg (x0)
	e.Store(v, x18)          // x18 = v
	e.Ret(e.Load(ir.ClsL, x18))

	_, code := buildObjAndRun(t, m, `
extern long rt(long);
int main(void){ return rt(0xABCDEF012L) == 0xABCDEF012L ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// Reading the stack pointer via a register variable yields the real sp.
func TestRegVarStackPointer(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("getsp", ir.ClsL).Export()
	e := f.Entry()
	sp := f.RegVar("sp", int(arm64.SP))
	e.Ret(e.Load(ir.ClsL, sp))

	_, code := buildObjAndRun(t, m, `
extern long getsp(void);
int main(void){
  long local;
  long sp = getsp();
  long d = sp - (long)&local;
  if (d < 0) d = -d;
  return d < 65536 ? 0 : 1; // the returned sp is a real, nearby stack address
}`)
	require.Equal(t, 0, code)
}

// The stack pointer can be written too — the primitive low-level runtime code
// (a stack switch) needs. This reserves scratch below sp, uses it, and restores.
func TestRegVarWriteStackPointer(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("spscratch", ir.ClsW).Export()
	e := f.Entry()
	sp := f.RegVar("sp", int(arm64.SP))
	s0 := e.Load(ir.ClsL, sp)                 // s0 = sp
	lowered := e.Sub(ir.ClsL, s0, f.Long(64)) // 64 bytes below sp
	e.Store(lowered, sp)                      // sp = s0 - 64  (reserve scratch)
	e.Store(f.Word(0xBEEF), lowered)          // use the reserved space
	v := e.Load(ir.ClsW, lowered)
	e.Store(s0, sp) // restore sp
	e.Ret(v)

	_, code := buildObjAndRun(t, m, `
extern int spscratch(void);
int main(void){ return spscratch() == 0xBEEF ? 0 : 1; }`)
	require.Equal(t, 0, code)
}
