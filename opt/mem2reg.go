package opt

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// varBase returns the source-variable base of an alloc temp's name, stripping a
// trailing ".addr"/".ptr" suffix that front ends use for a variable's storage
// slot (so a phi for the variable's value reads as the variable, not its slot).
func varBase(name string) string {
	for _, suf := range []string{".addr", ".ptr"} {
		if strings.HasSuffix(name, suf) {
			return name[:len(name)-len(suf)]
		}
	}
	return name
}

// Mem2Reg promotes stack slots that never escape and are accessed only through
// full-width loads and stores into SSA temporaries, inserting phi nodes at the
// iterated dominance frontier and renaming loads/stores away. This is the
// classic Cytron et al. SSA construction, run over the alloc-backed variables.
//
// A function with a computed goto is left alone -- see hasComputedGoto.
func hasComputedGoto(f *ir.Func) bool {
	for _, b := range f.Blocks {
		if b.Jmp.Kind == ir.JmpBr {
			return true
		}
	}
	return false
}

func Mem2Reg(f *ir.Func) bool {
	// Promoting a variable that lives across a computed goto is a performance loss,
	// not a correctness problem. lower.CoalescePhis resolves the phis it puts at the
	// dispatch handlers without the O(handlers^2) copy explosion a naive SSA
	// destruction would hit -- but only by coalescing each such variable into one
	// temporary whose live range spans the whole dispatch. Linear-scan allocation
	// then spills it across every handler, and the result runs slower than leaving
	// it in memory (measured ~33% slower on QuickJS's JS_CallInternal, which also
	// takes ~5x longer to compile). So an interpreter stays in memory form; the rest
	// of the module, which has no such spanning ranges, promotes normally.
	if hasComputedGoto(f) {
		return false
	}
	vars, varOf := findPromotable(f)
	if len(vars) == 0 {
		return false
	}

	cfg := analysis.BuildCFG(f)
	dom := cfg.Dominators()
	df := cfg.DominanceFrontier(dom)

	// A phi merging a promoted variable inherits that variable's name, so the SSA
	// reads like the source. uniq keeps names distinct within the function. The
	// alloc and load temps mem2reg is about to remove are excluded, so a variable's
	// name is free for its phi/value rather than pushed to a ".N" suffix.
	removed := map[uint32]bool{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp && isVarAlloc(in.To.ID, varOf) {
				removed[in.To.ID] = true
			} else if loadVar(in, varOf) >= 0 {
				removed[in.To.ID] = true
			}
		}
	}
	used := map[string]bool{}
	for _, t := range f.Temps {
		if !removed[uint32(t.ID)] {
			used[t.Name] = true
		}
	}
	uniq := func(base string) string {
		if base == "" {
			return ""
		}
		name := base
		for i := 1; used[name]; i++ {
			name = fmt.Sprintf("%s.%d", base, i)
		}
		used[name] = true
		return name
	}

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
			p := &ir.Phi{Cls: v.cls, To: f.NewTemp(uniq(varBase(v.name)), v.cls)}
			b.Phis = append(b.Phis, p)
			if phiOf[b] == nil {
				phiOf[b] = map[int]*ir.Phi{}
			}
			phiOf[b][vi] = p
		}
	}

	// nameVal names an anonymous (compiler-generated) value after the variable it
	// is stored into, so a single-assignment local like `int sum = a+b` reads as
	// %sum rather than a generic temp. Values that already carry a name (params,
	// phis, other variables' reads) are left alone.
	nameVal := func(v ir.Ref, vi int) {
		if v.Kind != ir.RefTemp {
			return
		}
		if t := f.Temps[v.ID]; isDefaultName(t.Name) {
			t.Name = uniq(varBase(vars[vi].name))
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
	renameBlock(f, cfg.RPO[0], init, vars, varOf, phiOf, children, sub, nameVal)

	applySubst(f, sub)
	return true
}

// promotable describes one alloc-backed variable being promoted.
type promotable struct {
	cls  ir.Cls
	name string // the alloc temp's name, so phis can inherit it (readability)
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
				cls, full := fullLoadClass(in)
				if full && !in.Volatile && in.Args[0].Kind == ir.RefTemp && isAlloc[in.Args[0].ID] {
					note(in.Args[0].ID, cls)
				} else {
					// Sub-word, volatile, or otherwise unpromotable: a volatile local
					// has to keep its storage, since an access to a register is no
					// access at all.
					escape(in.Args[0])
				}
			case in.Op.IsStore():
				escape(in.Args[0]) // storing the pointer value escapes it
				cls, full := fullStoreClass(in)
				if full && !in.Volatile && in.Args[1].Kind == ir.RefTemp && isAlloc[in.Args[1].ID] {
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
			vars = append(vars, promotable{cls: class[id], name: f.Temps[id].Name})
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
	nameVal func(ir.Ref, int),
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
			vi := addrVar(&in, varOf)
			nameVal(in.Args[0], vi)
			curDef[vi] = in.Args[0]
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
		renameBlock(f, c, child, vars, varOf, phiOf, children, sub, nameVal)
	}
}

// isDefaultName reports whether s is a builder-generated temp name (t followed by
// digits), i.e. an anonymous value with no source-level name yet.
func isDefaultName(s string) bool {
	if len(s) < 2 || s[0] != 't' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
