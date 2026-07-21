package arm64

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// insertCallerSaves saves and restores every value that is live across a call while
// held in a caller-saved register: an OSpill before the call sequence stores it, an
// OReload after the call loads it back into the same register. Colouring already
// decided which call-crossing values sit in caller-saved registers (weighing the
// save/restore cost against callee-saved and against spilling); this pass is the
// mechanical correctness step that mirrors GCC's caller-save.cc. A value crossing
// several calls shares one slot; slots extend the allocation's spill area, so the
// frame layout needs no change.
func insertCallerSaves(f *ir.Func, cfg *analysis.CFG, live *analysis.Liveness, alloc *allocation) error {
	// Pass 1: for each (non-tail) call, the caller-saved-register values live across
	// it -- live afterward and not defined by the call.
	saves := map[*ir.Instr][]int{}
	for _, b := range cfg.RPO {
		liveSet := live.LiveOut(b).Copy()
		// The terminator's operands are used at the block's end, so they are live after
		// the last instruction even when they die at the terminator (not in LiveOut).
		if b.Jmp.Arg.Kind == ir.RefTemp {
			liveSet.Add(int(b.Jmp.Arg.ID))
		}
		for _, a := range b.Jmp.Args {
			if a.Kind == ir.RefTemp {
				liveSet.Add(int(a.ID))
			}
		}
		for k := len(b.Instrs) - 1; k >= 0; k-- {
			in := &b.Instrs[k]
			defs := instrDefs(in)
			if in.Op == ir.OCall && !in.Tail {
				def := map[int]bool{}
				for _, d := range defs {
					def[d] = true
				}
				var sv []int
				for _, t := range liveSet.Members() {
					if def[t] {
						continue
					}
					r := f.Temps[t].Reg
					if r == ir.NoReg || !callerClobbered(f.Temps[t].Cls, Reg(r)) {
						continue
					}
					// A GC ref live across a safepoint is pre-spilled whole-life
					// (gcalloc), so it must never be register-resident here -- a stack map
					// pointing at a register while the value transits a save slot is a hole.
					if f.Temps[t].GCRef {
						return fmt.Errorf("arm64: GC ref %%%s held in caller-saved %s across a call",
							f.Temps[t].Name, Reg(r).xName())
					}
					sv = append(sv, t)
				}
				if len(sv) > 0 {
					sort.Ints(sv) // deterministic order
					saves[in] = sv
				}
			}
			for _, d := range defs {
				liveSet.Remove(d)
			}
			for _, u := range instrUses(in) {
				liveSet.Add(u)
			}
		}
	}
	if len(saves) == 0 {
		return nil
	}

	// One slot per saved temp, reused across every call it crosses, appended to the
	// spill area (computeFrame reserves spillBytes, so the frame just grows).
	slotOf := map[int]int{}
	slot := func(t int) int {
		if s, ok := slotOf[t]; ok {
			return s
		}
		sz := 8
		if f.Temps[t].Cls == ir.ClsQ {
			sz = 16
		}
		if alloc.spillBytes%sz != 0 {
			alloc.spillBytes += sz - alloc.spillBytes%sz
		}
		slotOf[t] = alloc.spillBytes
		alloc.spillBytes += sz
		return slotOf[t]
	}

	// Pass 2: wrap each call sequence -- saves before the first OArg of its contiguous
	// OArg run, restores after the OCall -- so the emitter still sees OArg...OCall with
	// no gaps.
	for _, b := range cfg.RPO {
		out := make([]ir.Instr, 0, len(b.Instrs))
		k := 0
		for k < len(b.Instrs) {
			if op := b.Instrs[k].Op; op != ir.OArg && op != ir.OCall {
				out = append(out, b.Instrs[k])
				k++
				continue
			}
			start := k
			for k < len(b.Instrs) && b.Instrs[k].Op == ir.OArg {
				k++
			}
			if k >= len(b.Instrs) || b.Instrs[k].Op != ir.OCall {
				for idx := start; idx < k; idx++ {
					out = append(out, b.Instrs[idx])
				}
				continue
			}
			sv := saves[&b.Instrs[k]]
			for _, t := range sv {
				out = append(out, ir.Instr{Op: ir.OSpill, Cls: f.Temps[t].Cls, Args: []ir.Ref{f.Temps[t].Ref()}, Aux: int64(slot(t))})
			}
			for idx := start; idx <= k; idx++ {
				out = append(out, b.Instrs[idx])
			}
			for _, t := range sv {
				out = append(out, ir.Instr{Op: ir.OReload, Cls: f.Temps[t].Cls, To: f.Temps[t].Ref(), Aux: int64(slot(t))})
			}
			k++
		}
		b.Instrs = out
	}
	return nil
}

// callerClobbered reports whether a call clobbers register r when it holds a value of
// class cls: any caller-saved X register; any V register whose relevant bits a call
// does not preserve -- the low 64 bits of V8..V15 are callee-saved, but a 128-bit
// value has no surviving register at all (AAPCS64 preserves only the low half).
func callerClobbered(cls ir.Cls, r Reg) bool {
	if cls.IsFloat() {
		if !(r >= V0 && r <= vLast) {
			return false
		}
		if cls == ir.ClsQ {
			return true // whole vector clobbered; no V register preserves it
		}
		return !calleeSavedFloat[r]
	}
	return isCallerSaved(r)
}
