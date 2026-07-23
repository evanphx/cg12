package arm64

import (
	"fmt"
	"sort"
	"strings"

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
					if r == ir.NoReg || !callerClobbered(f, f.Temps[t].Cls, Reg(r)) {
						continue
					}
					// A GC ref live across a safepoint is pre-spilled whole-life
					// (gcalloc), so it must never be register-resident here -- a stack map
					// pointing at a register while the value transits a save slot is a hole.
					if f.Temps[t].GCRef {
						_, rematerialized := alloc.remat[t]
						return fmt.Errorf("arm64: GC ref %%%s held in caller-saved %s across a call (managed=%v go-internal=%v fixed=%v slot=%d remat=%v)",
							f.Temps[t].Name, Reg(r).xName(), f.UsesManagedFrame(), f.UsesGoInternalCallConvention(),
							f.Temps[t].Fixed, f.Temps[t].Slot, rematerialized)
					}
					sv = append(sv, t)
				}
				if len(sv) > 0 {
					sort.Ints(sv) // deterministic order
					if f.UsesManagedFrame() || f.UsesGoInternalCallConvention() {
						names := make([]string, 0, len(sv))
						for _, t := range sv {
							names = append(names, fmt.Sprintf("%%%s/%s", f.Temps[t].Name, Reg(f.Temps[t].Reg).xName()))
						}
						return fmt.Errorf("arm64: %s keeps caller-saved values live across a managed/go-internal call: %s", f.Name, strings.Join(names, ", "))
					}
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

	// Coalesce the save slots: two saved values whose live ranges never overlap can
	// share one slot. In an interpreter the saved values are overwhelmingly per-handler
	// and mutually disjoint, so without this each takes its own slot and the save area
	// dominates the frame.
	sizeOf := func(t int) int {
		if f.Temps[t].Cls == ir.ClsQ {
			return 16
		}
		return 8
	}
	savedSet := map[int]bool{}
	for _, sv := range saves {
		for _, t := range sv {
			savedSet[t] = true
		}
	}
	var savedList []int
	for t := range savedSet {
		savedList = append(savedList, t)
	}
	groupRep := slotGroups(f, cfg, live, savedList, sizeOf)

	// One slot per group, keyed by representative, appended to the spill area.
	slotOf := map[int]int{}
	slot := func(t int) int {
		rep := t
		if groupRep != nil {
			rep = groupRep[t]
		}
		if s, ok := slotOf[rep]; ok {
			return s
		}
		sz := sizeOf(rep)
		if alloc.spillBytes%sz != 0 {
			alloc.spillBytes += sz - alloc.spillBytes%sz
		}
		slotOf[rep] = alloc.spillBytes
		alloc.spillBytes += sz
		return slotOf[rep]
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
			// Emit the saves and reloads in slot order, so two landing in adjacent
			// slots are consecutive and the emitter can pair them into an stp/ldp.
			// Order within a run is free: the saves are independent stores before the
			// call, the reloads independent loads after it.
			svo := append([]int(nil), sv...)
			sort.Slice(svo, func(i, j int) bool { return slot(svo[i]) < slot(svo[j]) })
			for _, t := range svo {
				out = append(out, ir.Instr{Op: ir.OSpill, Cls: f.Temps[t].Cls, Args: []ir.Ref{f.Temps[t].Ref()}, Aux: int64(slot(t))})
			}
			for idx := start; idx <= k; idx++ {
				out = append(out, b.Instrs[idx])
			}
			for _, t := range svo {
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
func callerClobbered(function *ir.Func, cls ir.Cls, r Reg) bool {
	if function.UsesManagedFrame() || function.UsesGoInternalCallConvention() {
		return r >= X0 && r <= vLast
	}
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
