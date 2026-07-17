package lower

import "github.com/evanphx/cg12/ir"

// HoistAllocas moves every fixed-size stack allocation to the entry block. A
// local's OAlloc is emitted where the C declaration sits, but C control flow
// (a switch case label or a goto) can jump into the enclosing block past that
// point — SQLite's VDBE interpreter does exactly this. The alloca's address is a
// constant frame offset, so evaluating it at entry (which dominates every use)
// is always correct and avoids reading an uninitialised slot.
//
// Without it the IR is not even valid SSA: the alloca does not dominate its use,
// so the temporary holding the address is read on a path that never defined it.
// Every backend emits a real instruction for the alloca -- an add from sp, a lea
// from rbp -- so the register simply holds whatever it held, and the first store
// through it goes wherever that pointed.
//
// This lived in the arm64 backend alone, so amd64 compiled the shape above into
// a program that segfaults where gcc returns 42. Nothing about the problem is
// architectural: it is C's rules about where a declaration may be jumped over
// meeting SSA's rule about dominance, and neither mentions a machine.
func HoistAllocas(f *ir.Func) {
	var allocas []ir.Instr
	for _, b := range f.Blocks {
		kept := b.Instrs[:0]
		for _, in := range b.Instrs {
			// A variable-size alloca (a VLA) depends on a runtime value and cannot be
			// hoisted; only constant-size reservations move.
			if in.Op.IsAlloc() && in.Arg(0).Kind == ir.RefConst {
				allocas = append(allocas, in)
			} else {
				kept = append(kept, in)
			}
		}
		b.Instrs = kept
	}
	if len(allocas) == 0 {
		return
	}
	start := f.Start
	// Keep the allocas after any leading incoming-parameter placeholders.
	i := 0
	for i < len(start.Instrs) && (start.Instrs[i].Op == ir.OPar || start.Instrs[i].Op == ir.OParEnv) {
		i++
	}
	merged := make([]ir.Instr, 0, len(start.Instrs)+len(allocas))
	merged = append(merged, start.Instrs[:i]...)
	merged = append(merged, allocas...)
	merged = append(merged, start.Instrs[i:]...)
	start.Instrs = merged
}
