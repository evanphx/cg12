package opt

import (
	"os"
	"sync/atomic"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// LoopCarriedCandidatesEscape turns on stage 1 of ESCAPE_IR_PLAN.md: an
// OHeapAlloc candidate inside a loop is not promoted to a frame slot when a
// value derived from it can outlive one iteration.
//
// A frame slot is allocated once per *frame*, not once per iteration, so
// promoting such a candidate puts every iteration on one object. cg12
// miscompiles that today; goc/testdata/spike/loop_alias_forms.go is the
// reduction, and RUNTIME_PLAN.md section 5.9 records the same hazard for the
// per-iteration loop variable, which it works around by never making that cell
// a candidate.
//
// It is a knob rather than the default because this is a design spike and the
// rule's cost has not been priced across the corpus. It is set from
// GOC_ESCAPE_LOOP once, at first use. A knob like this must not survive into a
// landed change: f397b28 removed the last one for exactly that reason.
var LoopCarriedCandidatesEscape = os.Getenv("GOC_ESCAPE_LOOP") == "1"

// loopRuleEscapes counts candidates the loop rule refused to promote. It is how
// the rule's cost is priced: a rule that never fires costs nothing and proves
// nothing, so the number has to be visible either way.
var loopRuleEscapes atomic.Int64

// LoopRuleEscapes reports how many candidates escapeLoopCarriedCandidates has
// escaped since the last ResetLoopRuleEscapes.
func LoopRuleEscapes() int64 { return loopRuleEscapes.Load() }

// ResetLoopRuleEscapes zeroes the counter LoopRuleEscapes reads.
func ResetLoopRuleEscapes() { loopRuleEscapes.Store(0) }

// escapeLoopCarriedCandidates escapes every candidate in bases that a value can
// carry out of the iteration it was allocated in.
//
// Two ways that happens, and the pass has already computed the maps both need:
//
//   - a temporary holding the candidate's address is live out of a latch, so
//     the next iteration observes the previous iteration's object;
//   - the address is stored into a frame slot whose own allocation is outside
//     the loop, which is the same thing one indirection later. This is the
//     shape of `for … { c := &cell{}; p = q; q = c }`: c's slot is inside the
//     loop and q's is not.
func escapeLoopCarriedCandidates(
	function *ir.Func,
	bases map[uint32]uint32,
	slotBases map[localSlot]uint32,
	escaped map[uint32]bool,
) {
	if len(bases) == 0 {
		return
	}
	cfg := analysis.BuildCFG(function)
	forest := cfg.LoopForest(cfg.Dominators())
	if len(forest.Loops) == 0 {
		return
	}

	// Which loop each candidate was allocated in, and which loop each frame
	// allocation was made in. A candidate allocated outside every loop cannot be
	// loop-carried, and a destination slot allocated inside the same loop is
	// re-made each iteration.
	allocLoop := make(map[uint32]*analysis.Loop)
	for _, block := range function.Blocks {
		loop := forest.In[block]
		if loop == nil {
			continue
		}
		for _, instruction := range block.Instrs {
			if instruction.To.Kind != ir.RefTemp {
				continue
			}
			if instruction.Op == ir.OHeapAlloc || isFrameAllocation(instruction) {
				allocLoop[instruction.To.ID] = loop
			}
		}
	}

	liveness := cfg.Liveness()
	for temp, base := range bases {
		loop := allocLoop[base]
		if loop == nil || escaped[base] {
			continue
		}
		for _, latch := range loop.Latches {
			out := liveness.Out[latch]
			if out != nil && out.Has(int(temp)) {
				escaped[base] = true
				loopRuleEscapes.Add(1)
				break
			}
		}
	}
	for slot, base := range slotBases {
		loop := allocLoop[base]
		if loop == nil || escaped[base] {
			continue
		}
		// slot.base is a locKey, not a temp id, so ask which loop the slot's
		// own storage was allocated in through the same map keyed by the
		// allocation temp the key names.
		if destination, ok := slotAllocLoop(slot, allocLoop); !ok || !loopContains(loop, destination) {
			escaped[base] = true
			loopRuleEscapes.Add(1)
		}
	}
}

// slotAllocLoop reports the loop a frame slot's own allocation sits in. The
// slot key carries the allocation's temporary id, so the answer is a lookup;
// ok is false when the slot's base is not one of this function's allocations,
// which is the conservative case.
func slotAllocLoop(slot localSlot, allocLoop map[uint32]*analysis.Loop) (*analysis.Loop, bool) {
	id, ok := slot.base.allocTemp()
	if !ok {
		return nil, false
	}
	return allocLoop[id], true
}

// loopContains reports whether inner is outer or is nested inside it.
func loopContains(outer, inner *analysis.Loop) bool {
	for current := inner; current != nil; current = current.Parent {
		if current == outer {
			return true
		}
	}
	return false
}
