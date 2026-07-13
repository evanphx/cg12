package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStackGrowthGuard shows a Go-style stack-growth guard: the strategy emits a
// prologue check against a per-thread stack limit that calls a runtime routine
// when the frame would overflow. The IR is an ordinary function; the guard is
// entirely the strategy's doing.
func TestStackGrowthGuard(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("work", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, n, f.Word(1)))

	opts := arm64.Options{GC: arm64.StackGrowth{
		LimitSym: "stack_limit", MorestackSym: "morestack", ThreadLocal: true,
	}}
	data, err := arm64.CompileObjectWith(m, opts)
	require.NoError(t, err)

	code := runObject(t, data, `
#include <stdint.h>
_Thread_local uint64_t stack_limit = 0;   // a low limit: the guard passes
int g_grows = 0;
// The runtime routine: a real one reallocates and copies the stack; this stub
// records the call, is handed the frame size, and lowers the limit so the
// prologue's re-check proceeds.
void morestack(uint64_t framesize){ (void)framesize; g_grows++; stack_limit = 0; }

extern int work(int);
int main(void){
  if (work(10) != 11) return 1;     // computes correctly...
  if (g_grows != 0)   return 2;     // ...without growing (limit is low)

  stack_limit = ~(uint64_t)0;       // limit above the stack pointer: must grow
  if (work(20) != 21) return 3;     // grows once, re-checks, then proceeds
  if (g_grows != 1)   return 4;     // the guard fired exactly once
  return 0;
}`)
	require.Equal(t, 0, code)
}

// The growth call carries an argument pointer-map: the growing function's
// managed-reference parameters are described (sp-relative, in the guard's save
// area) so a copying runtime can fix up pointer arguments before the frame
// exists.
func TestStackGrowthArgPointerMap(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("grow", ir.ClsL).Export()
	obj := f.ParamRef("obj") // a managed-reference pointer argument
	f.MarkGCRefType(obj, gcHeap)
	f.Entry().Ret(obj)

	opts := arm64.Options{GC: arm64.StackGrowth{
		LimitSym: "stack_limit", MorestackSym: "morestack", ThreadLocal: true,
	}}
	data, err := arm64.CompileObjectWith(m, opts)
	require.NoError(t, err)

	points := parseStackMap(t, data)
	require.Len(t, points, 1, "the growth safepoint")
	require.Len(t, points[0], 1, "obj is a pointer argument")
	assert.Equal(t, byte(2), points[0][0].kind, "sp-relative (in the guard save area)")
	assert.Equal(t, int32(16), points[0][0].val, "just past the saved x29/x30")
	assert.Equal(t, uint32(gcHeap), points[0][0].typ, "carries its type")
}

// The guard preserves floating-point argument registers across the runtime call,
// not just the integer ones.
func TestStackGrowthGuardPreservesFloatArgs(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("fid", ir.ClsD).Export()
	x := f.Param("x", ir.ClsD)
	f.Entry().Ret(x) // return the double argument unchanged

	opts := arm64.Options{GC: arm64.StackGrowth{
		LimitSym: "stack_limit", MorestackSym: "morestack", ThreadLocal: true,
	}}
	data, err := arm64.CompileObjectWith(m, opts)
	require.NoError(t, err)

	code := runObject(t, data, `
#include <stdint.h>
#include <math.h>
_Thread_local uint64_t stack_limit = ~(uint64_t)0; // force a grow every call
int g_grows = 0;
void morestack(uint64_t fs){ (void)fs; g_grows++; stack_limit = 0; }
extern double fid(double);
int main(void){
  // If v0 were clobbered by morestack, the returned value would be garbage.
  if (fabs(fid(3.5) - 3.5) > 1e-9) return 1;
  if (g_grows != 1) return 2;
  return 0;
}`)
	require.Equal(t, 0, code)
}

// The guard computes SP - frame correctly for a frame larger than a 12-bit
// immediate (so it never silently checks the wrong amount).
func TestStackGrowthGuardLargeFrame(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("bigframe", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	buf := e.Alloc(8, 6000) // a ~6 KiB frame, well past 4095
	e.Store(n, buf)
	e.Ret(e.Load(ir.ClsW, buf))

	opts := arm64.Options{GC: arm64.StackGrowth{
		LimitSym: "stack_limit", MorestackSym: "morestack", ThreadLocal: true,
	}}
	data, err := arm64.CompileObjectWith(m, opts)
	require.NoError(t, err)

	code := runObject(t, data, `
#include <stdint.h>
_Thread_local uint64_t stack_limit = 0;
int g_grows = 0;
void morestack(uint64_t fs){ (void)fs; g_grows++; stack_limit = 0; }
extern int bigframe(int);
int main(void){
  if (bigframe(7) != 7) return 1;   // no grow, correct
  if (g_grows != 0)     return 2;
  stack_limit = ~(uint64_t)0;
  if (bigframe(9) != 9) return 3;   // grows, still correct
  if (g_grows != 1)     return 4;
  return 0;
}`)
	require.Equal(t, 0, code)
}
