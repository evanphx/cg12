package analysis

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// VerifyDominance checks the SSA dominance property that ir.Verify does not:
// every use of a temporary is dominated by that temporary's definition. This is
// the invariant a jump-threading or block-duplication pass most easily breaks --
// a use reachable along a new path on which its def never runs -- and which the
// single-def / all-uses-defined / phi-edge checks in ir.Verify all pass through
// silently, because each of those holds locally while the reaching relation is
// globally wrong. A phi argument counts as a use at the *exit* of the predecessor
// it arrives from, not in the phi's own block.
//
// It runs only on pre-lowering SSA; after lowering a temp is reassigned and phis
// are gone, so dominance is not the property to check. Callers gate on
// f.LoweredFor() == "" as ir.Verify does, but VerifyDominance also returns nil
// for a lowered function so it is safe to call unconditionally.
func VerifyDominance(f *ir.Func) error {
	if f.LoweredFor() != "" {
		return nil
	}
	cfg := BuildCFG(f)
	dom := cfg.Dominators()

	// Where each temp is defined: its block and an index ordering defs within a
	// block. Phi and parameter defs sort before every instruction (index -1);
	// instruction i's results (To and Defs) are defined at index i.
	type site struct {
		blk *ir.Block
		idx int
	}
	defOf := make(map[uint32]site)
	record := func(r ir.Ref, blk *ir.Block, idx int) {
		if r.Kind == ir.RefTemp {
			defOf[r.ID] = site{blk, idx}
		}
	}
	if f.Start != nil {
		for _, p := range f.Params {
			record(p.Ref(), f.Start, -1)
		}
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			record(p.To, b, -1)
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op.IsAlloc() && f.Start != nil {
				// A fixed-size stack allocation reserves a frame slot for the whole
				// function; its address is a frame-relative constant the backend
				// rematerializes at each use, so it is available everywhere regardless
				// of where the op sits. C code that declares a local inside one arm of
				// a setjmp/EC_TAG state machine and reads it after a restart into
				// another arm relies on exactly this. (OAllocN, a VLA, is dynamic and
				// excluded by IsAlloc, so it still requires dominance.)
				record(in.To, f.Start, -1)
			} else {
				record(in.To, b, i)
			}
			for _, d := range in.Defs {
				record(d, b, i)
			}
		}
	}

	// A use at (useBlk, useIdx) is dominated by its def when the def's block
	// dominates useBlk, or they share a block and the def is earlier. useIdx for a
	// terminator or a phi-edge use is len(Instrs) -- past every instruction -- so
	// any same-block def precedes it.
	check := func(where, what string, r ir.Ref, useBlk *ir.Block, useIdx int) error {
		if r.Kind != ir.RefTemp {
			return nil
		}
		if !cfg.Reachable(useBlk) {
			return nil // an unreachable use never executes
		}
		d, ok := defOf[r.ID]
		if !ok {
			return nil // undefined: ir.Verify's verifySSA reports this
		}
		if d.blk == useBlk {
			if d.idx < useIdx {
				return nil
			}
		} else if dom.Dominates(d.blk, useBlk) {
			return nil
		}
		return fmt.Errorf("ir: %s: %s: %s uses %%%d, whose definition does not dominate it",
			f.Name, where, what, r.ID)
	}

	for _, b := range f.Blocks {
		if !cfg.Reachable(b) {
			continue
		}
		end := len(b.Instrs)
		for _, p := range b.Phis {
			for k, a := range p.Args {
				pred := p.Blocks[k]
				if pred == nil {
					continue
				}
				// The argument is evaluated on the edge, i.e. at the exit of pred.
				if err := check(b.Name, "phi", a, pred, len(pred.Instrs)); err != nil {
					return err
				}
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			for _, a := range in.Args {
				if err := check(b.Name, in.Op.String(), a, b, i); err != nil {
					return err
				}
			}
		}
		if err := check(b.Name, "terminator", b.Jmp.Arg, b, end); err != nil {
			return err
		}
		for _, a := range b.Jmp.Args {
			if err := check(b.Name, "terminator", a, b, end); err != nil {
				return err
			}
		}
	}
	return nil
}

// VerifyDominanceModule runs VerifyDominance on every function in a module.
func VerifyDominanceModule(m *ir.Module) error {
	for _, f := range m.Funcs {
		if err := VerifyDominance(f); err != nil {
			return err
		}
	}
	return nil
}
