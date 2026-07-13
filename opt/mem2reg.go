package opt

import (
	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// Mem2Reg promotes stack slots that never escape and are accessed only through
// full-width loads and stores into SSA temporaries, inserting phi nodes at the
// iterated dominance frontier and renaming loads/stores away. This is the
// classic Cytron et al. SSA construction, run over the alloc-backed variables.
func Mem2Reg(f *ir.Func) bool {
	vars, varOf := findPromotable(f)
	if len(vars) == 0 {
		return false
	}

	cfg := analysis.BuildCFG(f)
	dom := cfg.Dominators()
	df := cfg.DominanceFrontier(dom)

	// Insert phi placeholders at the iterated dominance frontier of each
	// variable's defining blocks.
	phiOf := map[*ir.Block]map[int]*ir.Phi{}
	for vi, v := range vars {
		defs := analysis.BlockSet{}
		for _, b := range cfg.RPO {
			for k := range b.Instrs {
				in := &b.Instrs[k]
				if in.Op.IsStore() && addrVar(in, varOf) == vi {
					defs[b] = true
				}
			}
		}
		for b := range analysis.IteratedFrontier(df, defs) {
			p := &ir.Phi{Cls: v.cls, To: f.NewTemp("", v.cls)}
			b.Phis = append(b.Phis, p)
			if phiOf[b] == nil {
				phiOf[b] = map[int]*ir.Phi{}
			}
			phiOf[b][vi] = p
		}
	}

	// Rename: walk the dominator tree, tracking each variable's reaching
	// definition, dropping loads/stores/allocs and filling successor phis.
	children := domChildren(cfg, dom)
	sub := subst{}
	init := map[int]ir.Ref{}
	for vi, v := range vars {
		init[vi] = zeroConst(f, v.cls)
	}
	renameBlock(f, cfg.RPO[0], init, vars, varOf, phiOf, children, sub)

	applySubst(f, sub)
	return true
}

// promotable describes one alloc-backed variable being promoted.
type promotable struct {
	cls ir.Cls
}

// findPromotable identifies alloc slots that can be promoted and returns them
// with a map from the alloc's result-temp id to the variable index.
func findPromotable(f *ir.Func) ([]promotable, map[uint32]int) {
	// Candidate alloc temp ids and their (consistent) access class.
	class := map[uint32]ir.Cls{}
	classSet := map[uint32]bool{}
	ok := map[uint32]bool{}
	isAlloc := map[uint32]bool{}

	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp {
				ok[in.To.ID] = true
				isAlloc[in.To.ID] = true
			}
		}
	}

	note := func(id uint32, cls ir.Cls) {
		if classSet[id] && class[id] != cls {
			ok[id] = false // inconsistent access width/class
		}
		class[id] = cls
		classSet[id] = true
	}
	escape := func(r ir.Ref) {
		if r.Kind == ir.RefTemp && isAlloc[r.ID] {
			ok[r.ID] = false
		}
	}

	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				escape(a)
			}
		}
		for k := range b.Instrs {
			in := &b.Instrs[k]
			switch {
			case in.Op.IsAlloc():
				// its result is a candidate; its size operand is not a temp
			case in.Op.IsLoad():
				if cls, full := fullLoadClass(in); full && in.Args[0].Kind == ir.RefTemp && isAlloc[in.Args[0].ID] {
					note(in.Args[0].ID, cls)
				} else {
					escape(in.Args[0]) // sub-word or otherwise unpromotable use
				}
			case in.Op.IsStore():
				escape(in.Args[0]) // storing the pointer value escapes it
				if cls, full := fullStoreClass(in); full && in.Args[1].Kind == ir.RefTemp && isAlloc[in.Args[1].ID] {
					note(in.Args[1].ID, cls)
				} else {
					escape(in.Args[1])
				}
			default:
				for _, a := range in.Args {
					escape(a)
				}
			}
		}
		escape(b.Jmp.Arg)
	}

	var vars []promotable
	varOf := map[uint32]int{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if !in.Op.IsAlloc() || in.To.Kind != ir.RefTemp {
				continue
			}
			id := in.To.ID
			if !ok[id] || !classSet[id] {
				continue // escaped, or never actually accessed
			}
			if _, done := varOf[id]; done {
				continue
			}
			varOf[id] = len(vars)
			vars = append(vars, promotable{cls: class[id]})
		}
	}
	return vars, varOf
}

func renameBlock(
	f *ir.Func,
	b *ir.Block,
	curDef map[int]ir.Ref,
	vars []promotable,
	varOf map[uint32]int,
	phiOf map[*ir.Block]map[int]*ir.Phi,
	children map[*ir.Block][]*ir.Block,
	sub subst,
) {
	// Phis defined here become the reaching definition for their variable.
	for vi, p := range phiOf[b] {
		curDef[vi] = p.To
	}

	out := b.Instrs[:0]
	for _, in := range b.Instrs {
		switch {
		case in.Op.IsAlloc() && in.To.Kind == ir.RefTemp && isVarAlloc(in.To.ID, varOf):
			// drop the allocation
		case in.Op.IsStore() && addrVar(&in, varOf) >= 0:
			curDef[addrVar(&in, varOf)] = in.Args[0]
		case in.Op.IsLoad() && loadVar(&in, varOf) >= 0:
			sub[in.To.ID] = curDef[loadVar(&in, varOf)]
		default:
			out = append(out, in)
		}
	}
	b.Instrs = out

	// Fill successor phi operands from this block's reaching definitions.
	seen := map[*ir.Block]bool{}
	for _, s := range b.Succs() {
		if s == nil || seen[s] {
			continue
		}
		seen[s] = true
		for vi, p := range phiOf[s] {
			p.Args = append(p.Args, curDef[vi])
			p.Blocks = append(p.Blocks, b)
		}
	}

	for _, c := range children[b] {
		child := copyDefs(curDef)
		renameBlock(f, c, child, vars, varOf, phiOf, children, sub)
	}
}

// domChildren builds the dominator-tree children lists.
func domChildren(cfg *analysis.CFG, dom *analysis.DomTree) map[*ir.Block][]*ir.Block {
	children := map[*ir.Block][]*ir.Block{}
	for _, b := range cfg.RPO {
		if id := dom.Idom[b]; id != nil {
			children[id] = append(children[id], b)
		}
	}
	return children
}

func copyDefs(m map[int]ir.Ref) map[int]ir.Ref {
	out := make(map[int]ir.Ref, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isVarAlloc(id uint32, varOf map[uint32]int) bool {
	_, ok := varOf[id]
	return ok
}

// addrVar returns the variable index a store writes to, or -1.
func addrVar(in *ir.Instr, varOf map[uint32]int) int {
	if in.Op.IsStore() && in.Args[1].Kind == ir.RefTemp {
		if vi, ok := varOf[in.Args[1].ID]; ok {
			return vi
		}
	}
	return -1
}

// loadVar returns the variable index a load reads from, or -1.
func loadVar(in *ir.Instr, varOf map[uint32]int) int {
	if in.Op.IsLoad() && in.Args[0].Kind == ir.RefTemp {
		if vi, ok := varOf[in.Args[0].ID]; ok {
			return vi
		}
	}
	return -1
}

func zeroConst(f *ir.Func, cls ir.Cls) ir.Ref {
	switch cls {
	case ir.ClsS:
		return f.Single(0)
	case ir.ClsD:
		return f.Double(0)
	default:
		return f.ConstInt(cls, 0)
	}
}

// fullLoadClass reports the register class of a full-width load, or ok=false for
// sub-word loads that cannot back a promoted variable.
func fullLoadClass(in *ir.Instr) (ir.Cls, bool) {
	switch in.Op {
	case ir.OLoaduw, ir.OLoadsw, ir.OLoadl, ir.OLoads, ir.OLoadd:
		return in.Cls, true
	}
	return 0, false
}

func fullStoreClass(in *ir.Instr) (ir.Cls, bool) {
	switch in.Op {
	case ir.OStorew, ir.OStorel, ir.OStores, ir.OStored:
		return in.Cls, true
	}
	return 0, false
}
