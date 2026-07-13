package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// buildGCMoveIR builds, in cg12 IR, the pointer-fixup engine of a stack-copy
// runtime: it reads its own frame pointer (a register variable), follows the
// chain to the caller's frame and its return address, looks that PC up in
// __cg12_stackmaps, and for each interior stack root relocates the pointee and
// rewrites the (spilled) pointer — the same algorithm the C proof-of-concept ran,
// now expressed entirely in the IR. It bumps the C global g_relocated per root.
func buildGCMoveIR(m *ir.Module) {
	f := m.NewFunc("gc_move", ir.ClsW)
	fpReg := f.RegVar("fp", int(arm64.X29))
	smap := f.Sym("__cg12_stackmaps", 0)
	four := f.Long(4)
	twelve := f.Long(12)

	entry := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	next := f.NewBlock("next")
	found := f.NewBlock("found")
	roots := f.NewBlock("roots")
	rbody := f.NewBlock("rbody")
	reloc := f.NewBlock("reloc")
	rnext := f.NewBlock("rnext")
	done := f.NewBlock("done")

	// entry: locate the caller's frame and its safepoint PC, and the map table.
	myFP := entry.Load(ir.ClsL, fpReg)                                // gc_move's x29
	callerFP := entry.Load(ir.ClsL, myFP)                             // [x29] = caller fp
	retPC := entry.Load(ir.ClsL, entry.Add(ir.ClsL, myFP, f.Long(8))) // [x29+8]
	count := entry.Load(ir.ClsW, entry.Add(ir.ClsL, smap, f.Long(8))) // header.count
	entry.Goto(loop)

	// loop over safepoint entries.
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
	p := loop.Phi(ir.ClsL, ir.PhiEdge{From: entry, Val: entry.Add(ir.ClsL, smap, twelve)})
	loop.Jnz(loop.Cmp(ir.CmpUge, ir.ClsW, i, count), done, body)

	pc := body.Load(ir.ClsL, p)
	nroots := body.Load(ir.ClsW, body.Add(ir.ClsL, p, f.Long(8)))
	r0 := body.Add(ir.ClsL, p, twelve) // first root of this entry
	body.Jnz(body.Cmp(ir.CmpEq, ir.ClsL, pc, retPC), found, next)

	// next entry: p += 12 + nroots*12.
	skip := next.Mul(ir.ClsL, next.Extuw(ir.ClsL, next.Add(ir.ClsW, nroots, f.Word(1))), twelve)
	iNext := next.Add(ir.ClsW, i, f.Word(1))
	pNext := next.Add(ir.ClsL, p, skip)
	next.Goto(loop)
	loop.Phis[0].Add(next, iNext)
	loop.Phis[1].Add(next, pNext)

	found.Goto(roots)
	j := roots.Phi(ir.ClsW, ir.PhiEdge{From: found, Val: f.Word(0)})
	r := roots.Phi(ir.ClsL, ir.PhiEdge{From: found, Val: r0})
	roots.Jnz(roots.Cmp(ir.CmpUge, ir.ClsW, j, nroots), done, rbody)

	kind := rbody.LoadSub(ir.ClsW, ir.SubUB, r)
	typ := rbody.Load(ir.ClsW, rbody.Add(ir.ClsL, r, f.Long(8)))
	isStack := rbody.And(ir.ClsW, rbody.Cmp(ir.CmpEq, ir.ClsW, kind, f.Word(1)),
		rbody.Cmp(ir.CmpEq, ir.ClsW, typ, f.Word(int64(gcStack))))
	rbody.Jnz(isStack, reloc, rnext)

	// relocate: read the interior pointer, copy the pointee +1000, rewrite the slot.
	off := reloc.LoadSub(ir.ClsL, ir.SubW, reloc.Add(ir.ClsL, r, four)) // signed frame offset
	slot := reloc.Add(ir.ClsL, callerFP, off)
	oldp := reloc.Load(ir.ClsL, slot)
	newp := reloc.Call(ir.ClsL, f.Sym("malloc", 0), four)
	reloc.Store(reloc.Add(ir.ClsW, reloc.Load(ir.ClsW, oldp), f.Word(1000)), newp)
	reloc.Store(newp, slot)
	g := f.Sym("g_relocated", 0)
	reloc.Store(reloc.Add(ir.ClsW, reloc.Load(ir.ClsW, g), f.Word(1)), g)
	reloc.Goto(rnext)

	jNext := rnext.Add(ir.ClsW, j, f.Word(1))
	rNext := rnext.Add(ir.ClsL, r, twelve)
	rnext.Goto(roots)
	roots.Phis[0].Add(rnext, jNext)
	roots.Phis[1].Add(rnext, rNext)

	done.Ret(f.Word(0))
}

// TestMorestackFixupEngineInIR runs the IR-written stack-copy fixup engine: a
// mutator holds an interior stack pointer across a call to gc_move (itself cg12
// IR), which relocates it; the mutator then uses the updated pointer.
func TestMorestackFixupEngineInIR(t *testing.T) {
	m := ir.NewModule()
	buildGCMoveIR(m)

	f := m.NewFunc("run", ir.ClsW).Export()
	seed := f.Param("seed", ir.ClsW)
	e := f.Entry()
	cell := e.Alloc(4, 4)
	f.MarkGCRefType(cell, gcStack)
	e.Store(seed, cell)
	e.CallVoid(f.Sym("gc_move", 0)) // the IR fixup engine, at a safepoint
	e.Ret(e.Load(ir.ClsW, cell))

	_, code := buildObjAndRun(t, m, `
extern int run(int);
int g_relocated = 0;
int main(void){
  if (run(5) != 1005) return 1;  // interior pointer relocated by the IR gc_move
  if (g_relocated != 1) return 2;
  return 0;
}`)
	require.Equal(t, 0, code)
}

// buildGrowIR builds grow(cur_sp), the copying half of a Go-style morestack, in
// cg12 IR: it allocates a larger stack, memcpy's the frames [cur_sp, base) onto
// it, and precisely relocates the saved frame-pointer chain (every frame's saved
// x29 that points back into the old stack is adjusted by the move delta). It
// returns the delta; the caller switches to the new stack by adding it to sp/x29.
func buildGrowIR(m *ir.Module) {
	f := m.NewFunc("grow", ir.ClsL).Export()
	curSP := f.Param("cur_sp", ir.ClsL)
	fpReg := f.RegVar("fp", int(arm64.X29))
	base0 := f.Sym("g_stack_base", 0)

	entry := f.Entry()
	fwalk := f.NewBlock("fwalk")
	fbody := f.NewBlock("fbody")
	fixdo := f.NewBlock("fixdo")
	fadv := f.NewBlock("fadv")
	fdone := f.NewBlock("fdone")

	base := entry.Load(ir.ClsL, base0)      // top of the current stack
	size := entry.Sub(ir.ClsL, base, curSP) // bytes in use
	region := entry.Call(ir.ClsL, f.Sym("malloc", 0), entry.Add(ir.ClsL, size, f.Long(1<<16)))
	newtop := entry.And(ir.ClsL, entry.Add(ir.ClsL, region, entry.Add(ir.ClsL, size, f.Long(1<<16))), f.Long(-16))
	newsp := entry.Sub(ir.ClsL, newtop, size)
	delta := entry.Sub(ir.ClsL, newsp, curSP)
	entry.Call(ir.ClsL, f.Sym("memcpy", 0), newsp, curSP, size) // copy the frames
	entry.Goto(fwalk)

	// Walk the frame-pointer chain; child holds the frame below the one we fix.
	child := fwalk.Phi(ir.ClsL, ir.PhiEdge{From: entry, Val: entry.Load(ir.ClsL, fpReg)})
	fp := fwalk.Load(ir.ClsL, child) // the caller frame this child links to
	inRange := fwalk.And(ir.ClsW, fwalk.Cmp(ir.CmpUge, ir.ClsL, fp, curSP), fwalk.Cmp(ir.CmpUlt, ir.ClsL, fp, base))
	fwalk.Jnz(inRange, fbody, fdone)

	savedFP := fbody.Load(ir.ClsL, fp) // fp's own saved frame pointer
	savedIn := fbody.And(ir.ClsW, fbody.Cmp(ir.CmpUge, ir.ClsL, savedFP, curSP), fbody.Cmp(ir.CmpUlt, ir.ClsL, savedFP, base))
	fbody.Jnz(savedIn, fixdo, fadv)

	// Adjust this frame's saved fp, in the copy, by the move delta.
	fixdo.Store(fixdo.Add(ir.ClsL, savedFP, delta), fixdo.Add(ir.ClsL, fp, delta))
	fixdo.Goto(fadv)

	fadv.Goto(fwalk)
	fwalk.Phis[0].Add(fadv, fp)

	// The old fp-chain is now fully walked, so poison the old stack: anything that
	// still reads it (an unrelocated pointer) reads garbage instead of stale-but-
	// valid data, making the relocation's correctness a hard requirement.
	fdone.Call(ir.ClsL, f.Sym("memset", 0), curSP, f.Word(0xEE), size)
	fdone.Store(newtop, base0)                 // publish the new stack top
	fdone.Store(f.Word(1), f.Sym("g_grew", 0)) // record that a growth happened
	fdone.Ret(delta)
}

// TestStackGrowthEndToEnd is the full mechanism: a mutator running on a small
// heap stack (via the IR trampoline) grows onto a larger one and continues. mid
// triggers the growth and switches sp/x29 to the new stack; outer, suspended one
// frame up, resumes on the relocated copy — reading its own local proves its
// frame was moved and its frame-pointer link relocated correctly.
func TestStackGrowthEndToEnd(t *testing.T) {
	m := ir.NewModule()
	buildGrowIR(m)

	// mid(): grow the stack, switch this frame onto it, return a marker.
	mid := m.NewFunc("mid", ir.ClsW)
	sp := mid.RegVar("sp", int(arm64.SP))
	fp := mid.RegVar("fp", int(arm64.X29))
	me := mid.Entry()
	d := me.Call(ir.ClsL, mid.Sym("grow", 0), me.Load(ir.ClsL, sp)) // grow(cur_sp) -> delta
	me.Store(me.Add(ir.ClsL, me.Load(ir.ClsL, sp), d), sp)          // sp  += delta
	me.Store(me.Add(ir.ClsL, me.Load(ir.ClsL, fp), d), fp)          // x29 += delta (now on new stack)
	me.Ret(mid.Word(7))

	// outer(seed): keep a local live across the growth, then use it afterward.
	outer := m.NewFunc("outer", ir.ClsW)
	seed := outer.Param("seed", ir.ClsW)
	oe := outer.Entry()
	lo := oe.Add(ir.ClsW, seed, outer.Word(100)) // lives across the call to mid
	r := oe.Call(ir.ClsW, outer.Sym("mid", 0))
	oe.Ret(oe.Add(ir.ClsW, lo, r)) // (seed+100) + 7, if outer's frame survived the move

	// run_on_stack(top, seed): set the stack top, switch to it, run outer, switch back.
	ros := m.NewFunc("run_on_stack", ir.ClsW).Export()
	top := ros.Param("top", ir.ClsL)
	sd := ros.Param("seed", ir.ClsW)
	rsp := ros.RegVar("sp", int(arm64.SP))
	re := ros.Entry()
	re.Store(top, ros.Sym("g_stack_base", 0)) // this stack's top
	saved := re.Load(ir.ClsL, rsp)
	re.Store(top, rsp) // switch to the heap stack
	result := re.Call(ir.ClsW, ros.Sym("outer", 0), sd)
	re.Store(saved, rsp) // switch back to the C stack
	re.Ret(result)

	_, code := buildObjAndRun(t, m, `
#include <stdlib.h>
#include <stdint.h>
long g_stack_base = 0;
int  g_grew = 0;
extern int run_on_stack(long top, int seed);
int main(void){
  size_t sz = 1<<14;                       // a small initial stack
  char *region = malloc(sz);
  long top = ((long)(region + sz)) & ~15L;
  int r = run_on_stack(top, 5);
  if (r != 105 + 7) return 1;              // outer's frame survived the move
  if (g_grew != 1)  return 2;              // ...and the stack actually grew
  return 0;
}`)
	require.Equal(t, 0, code)
}

// TestStackSwitchInIR is the foundational primitive for a growable-stack runtime,
// written entirely in cg12 IR with register variables: run_on_stack saves sp,
// switches it to a caller-provided region, runs a function there, and switches
// back. mutator returns the sp it observed, which must lie inside that region.
func TestStackSwitchInIR(t *testing.T) {
	m := ir.NewModule()

	mut := m.NewFunc("mutator", ir.ClsL)
	me := mut.Entry()
	me.Ret(me.Load(ir.ClsL, mut.RegVar("sp", int(arm64.SP)))) // report our sp

	ros := m.NewFunc("run_on_stack", ir.ClsL).Export()
	top := ros.Param("top", ir.ClsL)
	re := ros.Entry()
	sp := ros.RegVar("sp", int(arm64.SP))
	saved := re.Load(ir.ClsL, sp)                // save the current stack pointer
	re.Store(top, sp)                            // switch to the new stack
	r := re.Call(ir.ClsL, ros.Sym("mutator", 0)) // run on it
	re.Store(saved, sp)                          // switch back
	re.Ret(r)

	_, code := buildObjAndRun(t, m, `
#include <stdlib.h>
#include <stdint.h>
extern long run_on_stack(long top);
int main(void){
  size_t sz = 1<<16;
  char *region = malloc(sz);
  long top = ((long)(region + sz)) & ~15L; // 16-byte aligned top
  long sp_seen = run_on_stack(top);
  // mutator ran on the provided stack: its sp is inside the region, below top.
  return (sp_seen <= top && sp_seen > (long)region) ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}
