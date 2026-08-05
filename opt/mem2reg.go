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
// A function with a computed goto (an interpreter) is promoted like any other.
// The frontend threads the dispatch -- each `goto *p` is its own indirect branch
// to every label -- so the CFG is a mesh, and a promoted loop-carried variable
// gets a phi at each handler. lower.CoalescePhis unifies each such variable across
// the whole mesh into one temporary, so the loop state becomes a register every
// handler reads and writes in place. A mesh value that genuinely interferes with the
// phi result cannot coalesce; lower.DestructSSA resolves that phi through memory
// rather than a copy the indirect branch has nowhere to put. This lets the
// interpreter's hot state live in registers rather than memory.
func Mem2Reg(f *ir.Func) bool {
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

	// Per-variable live-in sets, for pruned SSA. Placing a phi only where a variable
	// is live-in matters most for an irreducible computed-goto mesh (an interpreter's
	// threaded dispatch): its dominance frontier is the whole mesh, so unpruned
	// (minimal) SSA sprouts a phi for every promoted handler-local at every handler,
	// and the rename chains them into one live range spanning the entire mesh. Hundreds
	// of such spurious mesh-wide ranges then swamp the register allocator -- each is
	// live across every call, so they oversubscribe the callee-saved registers and push
	// the genuinely hot loop-carried state (pc, sp, cfp) into caller-saved registers
	// that must be saved and restored around every dispatched call. Liveness (a load is
	// a use, a store a def) is a standard backward fixpoint over the CFG.
	liveIn := variableLiveIn(f, cfg, varOf)

	// Diagnostic (env-gated, read-only): report each marked variable's lifetime
	// region before renaming drops the markers. See dumpLifeRegions.
	dumpLifeRegions(f, cfg, vars, varOf)

	// Insert phi placeholders at the iterated dominance frontier of each variable's
	// defining blocks, pruned to blocks where the variable is actually live-in.
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
		// In reverse post-order, not in the frontier set's iteration order. The
		// frontier is a map keyed by block pointer, and the body below calls
		// f.NewTemp, so ranging it would number the phi temporaries differently on
		// every compile -- and temporary ids reach register allocation and slot
		// assignment. Which block gets a phi is unaffected either way.
		frontier := analysis.IteratedFrontier(df, defs)
		for _, b := range cfg.RPO {
			if !frontier[b] {
				continue
			}
			if !liveIn[b][vi] {
				continue // dead here; a phi would only create a spurious live range
			}
			p := &ir.Phi{Cls: v.cls, To: f.NewTemp(uniq(varBase(v.name)), v.cls)}
			// A phi for a slot that held a managed pointer holds one too. The slot's
			// own marking is what the frame map would have described had the slot
			// survived, so carrying it onto the phi is what keeps promotion from
			// changing which values the collector can find. See the promotable.managed
			// comment for why the class alone cannot answer this.
			if v.managed {
				f.MarkGCRefType(p.To, v.gcType)
			}
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

	// managed records that the slot held a garbage-collected pointer, so the phis
	// that replace it must be reported as GC roots.
	//
	// The access class cannot answer this on its own. A frontend that types managed
	// pointers as [ir.ClsM] is served by ir.LowerPointers, which marks every ClsM
	// temporary a GC reference on the way into the backend. goc does not: it types
	// every pointer ClsP and marks the managed ones itself, so a ClsP phi minted
	// here would reach the backend unmarked, and a ClsP that is genuinely not
	// managed (a frame address, a C pointer) must stay unmarked. The alloc temp's
	// own flag is the frontend's answer for this slot, and it is exactly what the
	// safepoint map reports while the slot exists.
	managed bool
	gcType  uint32
}

// sameSlotClass reports whether two access classes on one stack slot are compatible
// for promotion. ClsP (the abstract pointer class) is the target's word register
// class -- ClsL on the pointer-width backends -- so a slot touched as both ClsP and
// ClsL is one word and promotable; ClsW stays distinct (it is genuinely narrower).
func sameSlotClass(a, b ir.Cls) bool {
	if a == b {
		return true
	}
	return (a == ir.ClsL && b == ir.ClsP) || (a == ir.ClsP && b == ir.ClsL)
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
		if classSet[id] && !sameSlotClass(class[id], cls) {
			ok[id] = false // inconsistent access width/class
		}
		// Keep the pointer class when a slot is touched as both ClsP and ClsL, so the
		// promoted value stays a pointer -- LowerPointers resolves its width and the GC
		// still sees a managed reference. (A pointer local read as a plain long, common
		// from pointer/integer-punning macros, would otherwise defeat promotion.)
		if !classSet[id] || cls == ir.ClsP {
			class[id] = cls
		}
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
			case in.Op.IsLifetime():
				// a lifetime marker names the alloca to bound its live range, not to
				// leak its address — it must not count as an escape.
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
		for _, argument := range b.Jmp.Args {
			escape(argument)
		}
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
			slot := f.Temps[id]
			vars = append(vars, promotable{
				cls:     class[id],
				name:    slot.Name,
				managed: slot.GCRef || class[id].IsManaged(),
				gcType:  slot.GCType,
			})
		}
	}
	return vars, varOf
}

// markManagedDef reports a value that becomes a promoted managed variable's
// reaching definition as a garbage-collected reference.
//
// While the slot existed, the safepoint map described the slot, and every value
// stored into it was described for as long as the program could still read it
// back. Promotion replaces those loads with the stored value itself, which
// extends the value's live range to the last load -- across calls, and so across
// stack growth and collection. If the value's own temporary is not marked, the
// pointer is invisible exactly where the slot used to be visible: it is not
// adjusted when the stack is copied, and it does not keep its referent alive.
// The phis promotion mints are marked for the same reason; a definition that
// reaches a load without passing through a phi needs it just as much.
func markManagedDef(f *ir.Func, value ir.Ref, variable promotable) {
	if !variable.managed || value.Kind != ir.RefTemp {
		return
	}
	if temp := f.Temps[value.ID]; temp == nil || temp.GCRef {
		// Already described, and by whoever defined the value: that type
		// descriptor is at least as precise as the slot's.
		return
	}
	f.MarkGCRefType(value, variable.gcType)
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
		case in.Op.IsLifetime() && in.Args[0].Kind == ir.RefTemp && isVarAlloc(in.Args[0].ID, varOf):
			// A lifetime marker on a promoted local is dropped: its bounding effect
			// was already consumed by variableLiveIn (which stops the variable being
			// live-in past its scope), and the alloca it names no longer exists, so a
			// surviving marker would reference a dropped temp. Markers on allocas that
			// stayed in memory fall through to default and are kept for the frame
			// allocator's slot coloring.
		case in.Op.IsStore() && addrVar(&in, varOf) >= 0:
			vi := addrVar(&in, varOf)
			nameVal(in.Args[0], vi)
			markManagedDef(f, in.Args[0], vars[vi])
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

// variableLiveIn computes, for each block, the set of promoted-variable indices that
// are live-in (a use is reachable before any redefinition). It is the classic backward
// liveness dataflow specialized to the variables being promoted: a load of a variable
// is a use, a store a def. Within a block, only a variable's first touch matters -- a
// leading load is upward-exposed (live-in), a leading store kills any incoming value.
func variableLiveIn(f *ir.Func, cfg *analysis.CFG, varOf map[uint32]int) map[*ir.Block]map[int]bool {
	upExposed := map[*ir.Block]map[int]bool{}
	killed := map[*ir.Block]map[int]bool{}
	for _, b := range f.Blocks {
		ue, kl, seen := map[int]bool{}, map[int]bool{}, map[int]bool{}
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if vi := loadVar(in, varOf); vi >= 0 {
				if !seen[vi] {
					ue[vi], seen[vi] = true, true
				}
			} else if vi := addrVar(in, varOf); vi >= 0 {
				if !seen[vi] {
					kl[vi], seen[vi] = true, true
				}
			} else if in.Op == ir.OLifeStart && in.Args[0].Kind == ir.RefTemp {
				// lifetime.start marks the slot as fresh (indeterminate) above this
				// point: nothing above reads its value, so as a first touch it kills any
				// incoming liveness. That is what stops a handler-local from being live-in
				// at every mesh block and chained into one whole-function range. (Only the
				// start bounds the top of the range; lifetime.end is used by the frame
				// allocator, not here -- treating end as a kill can wrongly prune a phi a
				// still-live value needs.)
				if vi, ok := varOf[in.Args[0].ID]; ok && !seen[vi] {
					kl[vi], seen[vi] = true, true
				}
			}
		}
		upExposed[b], killed[b] = ue, kl
	}

	liveIn := map[*ir.Block]map[int]bool{}
	liveOut := map[*ir.Block]map[int]bool{}
	for _, b := range f.Blocks {
		liveIn[b], liveOut[b] = map[int]bool{}, map[int]bool{}
	}
	// Iterate to a fixpoint, visiting blocks in reverse RPO (a post-order) so live
	// information flows backward quickly.
	for changed := true; changed; {
		changed = false
		for i := len(cfg.RPO) - 1; i >= 0; i-- {
			b := cfg.RPO[i]
			out := liveOut[b]
			for _, s := range b.Succs() {
				if s == nil {
					continue
				}
				for vi := range liveIn[s] {
					if !out[vi] {
						out[vi], changed = true, true
					}
				}
			}
			in := liveIn[b]
			for vi := range upExposed[b] {
				if !in[vi] {
					in[vi], changed = true, true
				}
			}
			kl := killed[b]
			for vi := range out {
				if !kl[vi] && !in[vi] {
					in[vi], changed = true, true
				}
			}
		}
	}
	return liveIn
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
