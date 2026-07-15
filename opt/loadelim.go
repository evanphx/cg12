package opt

import (
	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// LoadElim removes redundant memory loads and forwards stored values to later
// loads of the same location. It tracks available memory values while scanning
// each block, seeding a block from its predecessor when that predecessor is the
// only way in (an extended basic block), and using alias analysis to decide
// which stores and calls invalidate which cached values.
func LoadElim(f *ir.Func) bool {
	ai := newAliasInfo(f)
	cfg := analysis.BuildCFG(f)

	exit := map[*ir.Block]availMem{}
	sub := subst{}
	changed := false

	for _, b := range cfg.RPO {
		var avail availMem
		if len(b.Preds) == 1 {
			if pe, ok := exit[b.Preds[0]]; ok {
				avail = pe.clone()
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			switch {
			case in.Op.IsLoad():
				loc := ai.locOf(in.Arg(0), accessWidth(in.Op))
				if v, ok := avail.lookup(in.Op, loc); ok {
					sub[in.To.ID] = v
					b.Instrs[i] = ir.Instr{Op: ir.ONop}
					changed = true
				} else {
					avail = avail.record(loc, in.To, in.Op)
				}
			case in.Op.IsStore():
				loc := ai.locOf(in.Arg(1), accessWidth(in.Op))
				avail = avail.invalidate(loc)
				if lop, ok := canonicalLoadOp(in.Op); ok {
					avail = avail.record(loc, in.Arg(0), lop)
				}
			case in.Op == ir.OCall || in.Op == ir.OBlit || in.Op == ir.OVaStart || in.Op == ir.OAsm:
				avail = nil // an unknown memory effect clears everything
			}
		}
		exit[b] = avail
	}

	if changed {
		applySubst(f, sub)
		removeNops(f)
	}
	return changed
}

// availEntry records that location loc holds value, readable with load op.
type availEntry struct {
	loc   locInfo
	value ir.Ref
	op    ir.Op
}

type availMem []availEntry

func (m availMem) clone() availMem {
	out := make(availMem, len(m))
	copy(out, m)
	return out
}

// lookup finds a cached value readable by a load with op at loc.
func (m availMem) lookup(op ir.Op, loc locInfo) (ir.Ref, bool) {
	for _, e := range m {
		if e.op == op && mustAlias(e.loc, loc) {
			return e.value, true
		}
	}
	return ir.R, false
}

// record adds a value known to be at loc, readable with op.
func (m availMem) record(loc locInfo, value ir.Ref, op ir.Op) availMem {
	return append(m, availEntry{loc: loc, value: value, op: op})
}

// invalidate drops every cached value that might overlap a write to loc.
func (m availMem) invalidate(loc locInfo) availMem {
	out := m[:0]
	for _, e := range m {
		if !mayAlias(e.loc, loc) {
			out = append(out, e)
		}
	}
	return out
}

// accessWidth returns the number of bytes a load or store touches.
func accessWidth(op ir.Op) int {
	switch op {
	case ir.OLoadsb, ir.OLoadub, ir.OStoreb:
		return 1
	case ir.OLoadsh, ir.OLoaduh, ir.OStoreh:
		return 2
	case ir.OLoadsw, ir.OLoaduw, ir.OLoads, ir.OStorew, ir.OStores:
		return 4
	case ir.OLoadl, ir.OLoadd, ir.OStorel, ir.OStored:
		return 8
	}
	return 0
}

// canonicalLoadOp returns the load op that reads a value back exactly as a store
// wrote it. Sub-word stores have no such op (the original wider value cannot be
// recovered), so ok is false.
func canonicalLoadOp(storeOp ir.Op) (ir.Op, bool) {
	switch storeOp {
	case ir.OStorew:
		return ir.OLoaduw, true
	case ir.OStorel:
		return ir.OLoadl, true
	case ir.OStores:
		return ir.OLoads, true
	case ir.OStored:
		return ir.OLoadd, true
	}
	return ir.ONop, false
}
