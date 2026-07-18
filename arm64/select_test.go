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
