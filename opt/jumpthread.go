package opt

import (
	"fmt"
	"os"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// Jump threading removes a redundant re-branch: when a merge block B ends in a
// conditional jump whose condition is a constant along one predecessor P's edge
// -- the shape mem2reg leaves for `a && b` and fast-path sentinels, a phi of
// booleans re-tested by a jnz -- P is routed straight to the resolved target,
// bypassing B's phi and branch. gcc does this; cg12 without it materializes the
// boolean and branches again (mini_and's `f`: ~20 insns vs gcc's ~10).
//
// It is done by DUPLICATION, not edge redirection: B is cloned into a fresh
// single-predecessor block B' (phis substituted by their P-edge values, the now
// constant instructions folded away), B' jumps directly to the target, and P is
// pointed at B'. Because B' has exactly one predecessor and one successor it is
// the split of edge P->B, so any value B reads that is defined outside B is still
// dominated at B' (its def dominates P, hence B'). This file handles only the
// subset where nothing defined in B is used outside B, which needs no SSA
// reconstruction; the general case (Stage 3) reconstructs.
//
// SimplifyCFG, which runs next in the clean fixpoint, absorbs B' into P (P now
// jumps to it unconditionally) and collapses B once a predecessor is removed.

// jtMaxBlockLen bounds the size of a merge block we will duplicate: threading a
// large block trades a branch for a lot of copied code.
const jtMaxBlockLen = 8

// jtState bounds how much threading one function receives across the whole
// pipeline, so the clean fixpoint that contains the pass always terminates even
// if reconstruction were to keep exposing new candidates.
type jtState struct {
	origInstrs  int // function size when first seen (see jtSize)
	origBlocks  int // block count when first seen
	grownInstrs int // net size added by threading so far
	threads     int // transforms applied so far
}

// jtSize measures a function the way threading grows it, which funcSize does not.
//
// funcSize counts b.Instrs, and phis live in a separate slice, so it cannot see
// the thing this pass actually adds: threading a block into one predecessor
// leaves the join needing a phi entry for every value the block defined. The
// growth budget was therefore reading zero on every round while the function ran
// away -- Lua's luaV_execute reached 8.3x its instruction count and 27x its
// emitted size with the cap never once firing.
//
// A phi is weighted by its argument count because that is what it costs: leaving
// SSA turns one phi into one copy per predecessor, so a phi in a block with two
// hundred predecessors is two hundred instructions, not one. That weighting is
// the whole reason a dispatch mesh is where this pass gets expensive.
func jtSize(f *ir.Func) int {
	n := 0
	for _, b := range f.Blocks {
		n += len(b.Instrs)
		for _, p := range b.Phis {
			n += len(p.Args)
		}
	}
	return n
}

// JumpThreadPass returns the jump-threading pass. Its per-function budget state
// is captured once and shared across every pipeline position that reuses this
// pass instance (the clean fixpoint is built once), so a function's budget is
// measured against its original size, matching InlinePass.
func JumpThreadPass() Pass {
	state := map[*ir.Func]*jtState{}
	return FuncPass("jumpthread", func(f *ir.Func) bool {
		return jumpThread(f, state)
	})
}

// threadCand is one threading opportunity: predecessor pred of merge block block
// reaches it on an edge along which block's jnz condition is the constant that
// selects target. env maps each temp defined in block (a phi's result, or an
// instruction folded to a constant along this edge) to its value on this edge.
type threadCand struct {
	block  *ir.Block
	pred   *ir.Block
	target *ir.Block
	env    map[uint32]ir.Ref
}

func jumpThread(f *ir.Func, state map[*ir.Func]*jtState) bool {
	st := state[f]
	if st == nil {
		st = &jtState{origInstrs: jtSize(f), origBlocks: len(f.Blocks)}
		state[f] = st
	}
	// From the block count when the function was FIRST seen, not the current one.
	// Threading adds blocks, so sizing the cap on len(f.Blocks) let each transform
	// raise the ceiling on the next: on luaV_execute the cap climbed 237 -> 281 as
	// the pass consumed it.
	threadCap := 16
	if c := st.origBlocks / 4; c > threadCap {
		threadCap = c
	}
	growthCap := st.origInstrs / 5
	if os.Getenv("CG12_JT_STATS") == f.Name {
		fmt.Fprintf(os.Stderr, "jt %s: enter blocks=%d size=%d origInstrs=%d threads=%d/%d grown=%d/%d\n",
			f.Name, len(f.Blocks), jtSize(f), st.origInstrs, st.threads, threadCap, st.grownInstrs, growthCap)
	}
	check := os.Getenv("CG12_JT_CHECK") != ""

	changed := false
	for perRound := 0; perRound < 32; perRound++ {
		if st.threads >= threadCap || st.grownInstrs > growthCap {
			break
		}
		cfg := analysis.BuildCFG(f)
		dom := cfg.Dominators()
		cand, ok := findThreadCandidate(f, cfg, dom)
		// Last use of both: what follows threads the candidate and prunes, neither
		// of which asks the graph for a number. A deferred release would instead
		// hold every round's numbering until the pass returned.
		cfg.Release()
		dom.Release()
		if !ok {
			break
		}
		before := jtSize(f)
		threadOne(f, cand)
		// Threading can cut the last edge into a region, orphaning blocks whose
		// edges into surviving phi blocks would otherwise leave the phi predecessor
		// sets inconsistent until the following SimplifyCFG runs. Drop them now so
		// every transform leaves well-formed SSA.
		pruneUnreachable(f)
		st.grownInstrs += jtSize(f) - before
		st.threads++
		changed = true
		if check {
			if err := ir.Verify(f); err != nil {
				panic(fmt.Sprintf("jumpthread: %s: threading %s->%s broke SSA: %v",
					f.Name, cand.pred.Name, cand.block.Name, err))
			}
			if err := analysis.VerifyDominance(f); err != nil {
				panic(fmt.Sprintf("jumpthread: %s: threading %s->%s broke dominance: %v",
					f.Name, cand.pred.Name, cand.block.Name, err))
			}
		}
	}
	return changed
}

// findThreadCandidate returns the first threading opportunity in f, or ok=false.
func findThreadCandidate(f *ir.Func, cfg *analysis.CFG, dom *analysis.DomTree) (threadCand, bool) {
	// The dominance frontier is a property of the CFG, so it is the same for every
	// candidate this scan considers -- but it is not cheap, and most scans never
	// need it, since a candidate whose definitions do not escape is judged without
	// it. Compute it at most once, and only if something asks.
	var df map[*ir.Block]analysis.BlockSet
	frontier := func() map[*ir.Block]analysis.BlockSet {
		if df == nil {
			df = cfg.DominanceFrontier(dom)
		}
		return df
	}
	for _, b := range f.Blocks {
		if b == f.Start || !cfg.Reachable(b) {
			continue
		}
		if b.Jmp.Kind != ir.JmpJnz || b.Jmp.To == b.Jmp.To2 {
			continue // SimplifyCFG folds a coincident-target jnz
		}
		if b.Jmp.To == nil || b.Jmp.To2 == nil || b.Jmp.To == b || b.Jmp.To2 == b {
			continue
		}
		if len(b.Preds) < 2 || len(b.Phis) == 0 || len(b.Instrs) > jtMaxBlockLen {
			continue
		}
		if isLoopHeader(b, dom) || !jtDuplicable(b) {
			continue
		}
		for _, p := range b.Preds {
			if p == b || p.Jmp.Kind == ir.JmpBr {
				continue // JmpBr's real target is a runtime address; skip
			}
			if dom.Dominates(b, p) {
				continue // a back edge into a loop; do not thread it
			}
			env := edgeEnv(f, b, p)
			cond := resolveRef(env, b.Jmp.Arg)
			c, ok := constInt(f, cond)
			if !ok {
				continue
			}
			target := b.Jmp.To
			if c == 0 {
				target = b.Jmp.To2
			}
			if target == nil || target == b {
				continue
			}
			cand := threadCand{block: b, pred: p, target: target, env: env}
			if !threadProfitable(f, frontier, cand) {
				continue
			}
			return cand, true
		}
	}
	return threadCand{}, false
}

// jtPhiBudget is the most a single thread may add in phi arguments before it is
// judged not worth doing.
//
// The unit is what jtSize measures and what leaving SSA charges: one phi argument
// becomes one copy. A thread saves a conditional branch and whatever it folds --
// a handful of instructions -- so paying dozens of copies for it is a bad trade
// however the branch was going to behave.
//
// The exact figure barely matters, which is the useful thing to know about it.
// Sweeping 8, 24, 48, 96 and 256 moves sqlite3VdbeExec not at all (21 spills,
// weight 13, at every one) and the emitted text of sqlite and lua by under a
// tenth of a percent. The population is bimodal: a thread in a dispatch mesh
// costs hundreds of phi arguments and a useful one costs a few, so anything in
// this range separates them. 48 sits in the middle of the gap rather than at a
// tuned edge, and nothing here is balanced on it.
const jtPhiBudget = 48

// threadProfitable reports whether a candidate is worth its SSA repair.
//
// Threading clones a block onto one edge, which gives every value the block
// defines and that is used elsewhere a second reaching definition. Repairing that
// puts a phi at each block in the iterated dominance frontier of the clone, and a
// phi in a block with n predecessors is n copies once SSA is left. In an
// interpreter's dispatch mesh those frontier blocks have hundreds of
// predecessors, so one escaping value can cost hundreds of instructions to save
// one branch -- which is how sqlite3VdbeExec ended up with seven .jt clones among
// its ten hottest spills, each a duplicate of a value that was already losing its
// register.
//
// The common case is free to judge: a block whose definitions do not escape needs
// no reconstruction at all (the && merge, and most of what this pass fires on),
// so the frontier is never computed for it. The frontier is only priced when
// there is something to repair.
func threadProfitable(f *ir.Func, frontierOf func() map[*ir.Block]analysis.BlockSet, c threadCand) bool {
	esc := escapingDefs(f, c.block)
	if len(esc) == 0 {
		return true
	}
	// The clone's frontier is not known before the clone exists; the block's own
	// is a lower bound on it, and a sound one to price against -- bp sits on an
	// edge into b, so anything b's definitions reach, the clone's do too.
	reach := analysis.IteratedFrontier(frontierOf(), analysis.BlockSet{c.block: true})
	args := 0
	for x := range reach {
		args += len(x.Preds)
		if len(esc)*args > jtPhiBudget {
			return false // already over; no need to finish counting
		}
	}
	return len(esc)*args <= jtPhiBudget
}

// isLoopHeader reports whether any predecessor of b is dominated by b (a back
// edge), which makes b a loop header. Threading such a block is left to a later,
// SCC-guarded stage.
func isLoopHeader(b *ir.Block, dom *analysis.DomTree) bool {
	for _, p := range b.Preds {
		if dom.Dominates(b, p) {
			return true
		}
	}
	return false
}

// jtDuplicable reports whether b's body may be copied onto a threaded edge: no
// side effect (a store/call/volatile would run on a path that did not have it),
// no stack allocation (duplicating it would mint a second slot with a different
// address), and -- conservatively for this stage -- no load.
func jtDuplicable(b *ir.Block) bool {
	for i := range b.Instrs {
		in := &b.Instrs[i]
		if hasSideEffect(in) || in.Op.IsAlloc() || in.Op.IsLoad() {
			return false
		}
	}
	return true
}

// escapingDefs returns the temps defined in b (a phi result, an instruction
// result, or a multi-def register) that are used outside b -- in another block's
// instruction or terminator, or in any phi argument. These are the values SSA
// reconstruction must repair after b is cloned, because the clone introduces a
// second reaching definition of each. Uses of a b-def within b's own body are
// fine: the clone carries them.
func escapingDefs(f *ir.Func, b *ir.Block) []ir.Ref {
	defs := map[uint32]bool{}
	for _, ph := range b.Phis {
		if ph.To.Kind == ir.RefTemp {
			defs[ph.To.ID] = true
		}
	}
	for i := range b.Instrs {
		in := &b.Instrs[i]
		if in.To.Kind == ir.RefTemp {
			defs[in.To.ID] = true
		}
		for _, d := range in.Defs() {
			if d.Kind == ir.RefTemp {
				defs[d.ID] = true
			}
		}
	}
	if len(defs) == 0 {
		return nil
	}
	escaped := map[uint32]bool{}
	note := func(r ir.Ref) {
		if r.Kind == ir.RefTemp && defs[r.ID] {
			escaped[r.ID] = true
		}
	}
	for _, bb := range f.Blocks {
		for _, ph := range bb.Phis {
			for _, a := range ph.Args {
				note(a)
			}
		}
		if bb == b {
			continue // instruction and terminator uses within b are cloned
		}
		for i := range bb.Instrs {
			for _, a := range bb.Instrs[i].Args {
				note(a)
			}
		}
		note(bb.Jmp.Arg)
		for _, a := range bb.Jmp.Args {
			note(a)
		}
	}
	if len(escaped) == 0 {
		return nil
	}
	// Return in a deterministic order (by ascending id) so a run is reproducible.
	var out []ir.Ref
	for _, ph := range b.Phis {
		if ph.To.Kind == ir.RefTemp && escaped[ph.To.ID] {
			out = append(out, ph.To)
		}
	}
	for i := range b.Instrs {
		in := &b.Instrs[i]
		if in.To.Kind == ir.RefTemp && escaped[in.To.ID] {
			out = append(out, in.To)
		}
		for _, d := range in.Defs() {
			if d.Kind == ir.RefTemp && escaped[d.ID] {
				out = append(out, d)
			}
		}
	}
	return out
}

// edgeEnv computes the value of each temp defined in b as it stands when control
// arrives from p: a phi result takes its p-edge argument, and each instruction is
// constant-folded with those values substituted (reusing foldInstr, so the
// evaluation matches the constant folder exactly). A temp maps to a constant when
// it folds to one, to an alias when foldInstr simplifies it to another value.
func edgeEnv(f *ir.Func, b, p *ir.Block) map[uint32]ir.Ref {
	env := map[uint32]ir.Ref{}
	for _, ph := range b.Phis {
		if ph.To.Kind != ir.RefTemp {
			continue
		}
		for k, pb := range ph.Blocks {
			if pb == p {
				env[ph.To.ID] = ph.Args[k]
				break
			}
		}
	}
	for i := range b.Instrs {
		in := &b.Instrs[i]
		if in.To.Kind != ir.RefTemp {
			continue
		}
		tmp := *in
		tmp.Args = make([]ir.Ref, len(in.Args))
		for k, a := range in.Args {
			tmp.Args[k] = resolveRef(env, a)
		}
		// nil def map: this edge-specialised fold only needs constant folding, not
		// the boolean-def analysis (isBoolean treats a missing def as non-boolean).
		if r, ok := foldInstr(f, nil, &tmp); ok {
			env[in.To.ID] = r
		}
	}
	return env
}

// resolveRef replaces a temp by its edge value if env has one, else returns it
// unchanged. It is a single lookup (not transitive); edgeEnv builds env in
// definition order so a later fold already sees resolved earlier values.
func resolveRef(env map[uint32]ir.Ref, r ir.Ref) ir.Ref {
	if r.Kind == ir.RefTemp {
		if v, ok := env[r.ID]; ok {
			return v
		}
	}
	return r
}

// threadOne applies one candidate: clone block into a fresh single-predecessor
// block wired to target, redirect pred there, and repair the phis of block and
// target so each names exactly its control-flow predecessors.
func threadOne(f *ir.Func, c threadCand) {
	b, p, target, env := c.block, c.pred, c.target, c.env

	bp := f.NewBlock("")

	// Every non-folded temp defined in b needs a fresh clone temp; folded ones are
	// constants (or aliases) on this edge and are never emitted. Assign them all
	// first so a reference to any of them resolves, regardless of order.
	clone := map[uint32]ir.Ref{}
	for i := range b.Instrs {
		in := &b.Instrs[i]
		if in.To.Kind != ir.RefTemp {
			continue
		}
		if _, folded := env[in.To.ID]; folded {
			continue
		}
		t := f.Temp(in.To)
		clone[in.To.ID] = f.InheritGCRef(f.NewTemp(t.Name+".jt", t.Cls), in.To)
	}

	// resolve rewrites a reference from b's namespace into b's clone: a non-folded
	// b-def becomes its clone temp; a folded or phi-substituted temp is resolved to
	// its edge value, following through the clone map when that value is itself a
	// b-def (foldInstr can alias one instruction to another). Constants and values
	// defined outside b (which dominate bp) pass through unchanged.
	var resolve func(r ir.Ref) ir.Ref
	resolve = func(r ir.Ref) ir.Ref {
		if r.Kind != ir.RefTemp {
			return r
		}
		if v, ok := clone[r.ID]; ok {
			return v
		}
		if v, ok := env[r.ID]; ok {
			if v.Kind == ir.RefTemp && v.ID == r.ID {
				return v // defensive: never loop on a self-referential alias
			}
			return resolve(v)
		}
		return r
	}

	for i := range b.Instrs {
		in := &b.Instrs[i]
		if in.To.Kind == ir.RefTemp {
			if _, folded := env[in.To.ID]; folded {
				continue // its value is known on this edge; no need to recompute it
			}
		}
		bp.Instrs = append(bp.Instrs, cloneInstr(f, in, resolve, identityBlock))
	}
	bp.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: target}

	// Route p at the clone instead of b, on every edge that named b.
	replaceJmpTarget(&p.Jmp, b, bp)

	// b loses p as a predecessor: drop p's argument from each of b's phis.
	for _, ph := range b.Phis {
		for k := range ph.Blocks {
			if ph.Blocks[k] == p {
				ph.Args = append(ph.Args[:k], ph.Args[k+1:]...)
				ph.Blocks = append(ph.Blocks[:k], ph.Blocks[k+1:]...)
				break
			}
		}
	}

	// target gains bp as a predecessor carrying the same value b would have on the
	// b->target edge, resolved into bp's namespace.
	for _, ph := range target.Phis {
		for k := range ph.Blocks {
			if ph.Blocks[k] == b {
				ph.Args = append(ph.Args, resolve(ph.Args[k]))
				ph.Blocks = append(ph.Blocks, bp)
				break
			}
		}
	}

	// Any value defined in b and used outside it now has two reaching definitions
	// -- its original in b and its clone's value out of bp -- so SSA must be
	// reconstructed for those. (Nothing escaping is the common case, e.g. the &&
	// merge, and this returns immediately.)
	esc := escapingDefs(f, b)
	if len(esc) == 0 {
		return
	}
	vars := make([]reconVar, len(esc))
	for i, t := range esc {
		vars[i] = reconVar{t: t, tv: resolve(t), cls: f.Temp(t).Cls}
	}
	reconstructThreaded(f, b, bp, vars)
}

// reconVar is one value that escaped the threaded block: t is its definition in b,
// tv is the value that reaches out of the clone bp (t's clone temp, or a constant
// or outer value it folded to). The two are the definitions SSA reconstruction
// merges.
type reconVar struct {
	t   ir.Ref
	tv  ir.Ref
	cls ir.Cls
}

// reconstructThreaded repairs SSA after b was cloned into bp, for the escaped
// values in vars. It places phis at the iterated dominance frontier of the two
// definition sites {b, bp} (pruned to where the value is live, so a threaded mesh
// block does not sprout phis across the whole mesh), then renames every outside
// use to its reaching definition by a dominator-tree walk -- the same Cytron
// construction Mem2Reg runs, specialized to two def sites per value.
func reconstructThreaded(f *ir.Func, b, bp *ir.Block, vars []reconVar) {
	cfg := analysis.BuildCFG(f)
	dom := cfg.Dominators()
	df := cfg.DominanceFrontier(dom)
	live := tempLiveIn(f, cfg, vars)

	frontier := analysis.IteratedFrontier(df, analysis.BlockSet{b: true, bp: true})
	// The frontier in reverse post-order rather than in its own iteration order: it
	// is a map keyed by block pointer, and the loop below calls f.NewTemp, so
	// ranging it would number the phi temporaries differently on every compile.
	// Which block gets a phi is the same either way. Mem2Reg does the same thing
	// for the same reason.
	orderedFrontier := make([]*ir.Block, 0, len(frontier))
	for _, block := range cfg.RPO {
		if frontier[block] {
			orderedFrontier = append(orderedFrontier, block)
		}
	}
	phiOf := map[*ir.Block]map[int]*ir.Phi{}
	for vi := range vars {
		for _, x := range orderedFrontier {
			if !live[x][vi] {
				continue // dead here; a phi would only make a spurious live range
			}
			source := vars[vi].t
			p := &ir.Phi{Cls: vars[vi].cls, To: f.InheritGCRef(
				f.NewTemp(f.Temp(source).Name+".jt", vars[vi].cls), source)}
			x.Phis = append(x.Phis, p)
			if phiOf[x] == nil {
				phiOf[x] = map[int]*ir.Phi{}
			}
			phiOf[x][vi] = p
		}
	}

	idOf := map[uint32]int{}
	for vi, v := range vars {
		idOf[v.t.ID] = vi
	}
	children := domChildren(cfg, dom)
	reconRename(f, cfg.RPO[0], map[int]ir.Ref{}, b, bp, vars, idOf, phiOf, children)
}

// reconRename is the reaching-definition walk for reconstructThreaded, mirroring
// renameBlock. cur maps a value index to its reaching definition entering blk.
func reconRename(
	f *ir.Func,
	blk *ir.Block,
	cur map[int]ir.Ref,
	b, bp *ir.Block,
	vars []reconVar,
	idOf map[uint32]int,
	phiOf map[*ir.Block]map[int]*ir.Phi,
	children map[*ir.Block][]*ir.Block,
) {
	placed := phiOf[blk]
	for vi, p := range placed {
		cur[vi] = p.To
	}

	// Rewrite uses of an escaped value in this block to its reaching definition.
	// Uses inside b (which defines the originals) and bp (which holds the clones)
	// are already correct and left alone.
	if blk != b && blk != bp {
		for i := range blk.Instrs {
			in := &blk.Instrs[i]
			for k, a := range in.Args {
				if vi, ok := escIndex(idOf, a); ok {
					in.Args[k] = mustReach(cur, vi, f, blk)
				}
			}
		}
		if vi, ok := escIndex(idOf, blk.Jmp.Arg); ok {
			blk.Jmp.Arg = mustReach(cur, vi, f, blk)
		}
		for k, a := range blk.Jmp.Args {
			if vi, ok := escIndex(idOf, a); ok {
				blk.Jmp.Args[k] = mustReach(cur, vi, f, blk)
			}
		}
	}

	// The two definition sites establish their values at block exit.
	if blk == b {
		for vi := range vars {
			cur[vi] = vars[vi].t
		}
	} else if blk == bp {
		for vi := range vars {
			cur[vi] = vars[vi].tv
		}
	}

	seen := map[*ir.Block]bool{}
	for _, s := range blk.Succs() {
		if s == nil || seen[s] {
			continue
		}
		seen[s] = true
		for vi, p := range phiOf[s] {
			r, ok := cur[vi]
			if !ok {
				r = zeroConst(f, vars[vi].cls) // a path reaching the phi but never a use
			}
			p.Args = append(p.Args, r)
			p.Blocks = append(p.Blocks, blk)
		}
		// A pre-existing phi in s whose argument from this predecessor is an escaped
		// value is an edge use, rewritten in this predecessor's context.
		for _, q := range s.Phis {
			if placedPhi(phiOf, s, q) {
				continue
			}
			for k := range q.Blocks {
				if q.Blocks[k] == blk {
					if vi, ok := escIndex(idOf, q.Args[k]); ok {
						q.Args[k] = mustReach(cur, vi, f, blk)
					}
				}
			}
		}
	}

	for _, c := range children[blk] {
		reconRename(f, c, copyReachingMap(cur), b, bp, vars, idOf, phiOf, children)
	}
}

// escIndex returns the escaped-value index a reference names, if it names one.
func escIndex(idOf map[uint32]int, r ir.Ref) (int, bool) {
	if r.Kind != ir.RefTemp {
		return 0, false
	}
	vi, ok := idOf[r.ID]
	return vi, ok
}

// mustReach returns the reaching definition of value vi, or panics: an escaped
// value's use is always dominated by one of its two definition sites, so an
// absent reaching definition is a bug in the pass, not a silent miscompile.
func mustReach(cur map[int]ir.Ref, vi int, f *ir.Func, blk *ir.Block) ir.Ref {
	if r, ok := cur[vi]; ok {
		return r
	}
	panic(fmt.Sprintf("jumpthread: %s: block %q uses an escaped value with no reaching definition",
		f.Name, blk.Name))
}

// placedPhi reports whether q is one of the phis reconstruction placed in s (so
// the pre-existing-phi rewrite skips it).
func placedPhi(phiOf map[*ir.Block]map[int]*ir.Phi, s *ir.Block, q *ir.Phi) bool {
	for _, p := range phiOf[s] {
		if p == q {
			return true
		}
	}
	return false
}

// tempLiveIn computes, per block, which of the escaped values are live-in. It is
// standard SSA liveness (a phi argument is a use on the predecessor edge), used to
// prune phi placement to where a value is actually needed.
func tempLiveIn(f *ir.Func, cfg *analysis.CFG, vars []reconVar) map[*ir.Block]map[int]bool {
	idOf := map[uint32]int{}
	for vi, v := range vars {
		idOf[v.t.ID] = vi
	}

	upExposed := map[*ir.Block]map[int]bool{}
	defined := map[*ir.Block]map[int]bool{}
	for _, bb := range f.Blocks {
		ue, dfd := map[int]bool{}, map[int]bool{}
		expose := func(r ir.Ref) {
			if vi, ok := escIndex(idOf, r); ok && !dfd[vi] {
				ue[vi] = true
			}
		}
		for _, ph := range bb.Phis {
			if vi, ok := escIndex(idOf, ph.To); ok {
				dfd[vi] = true
			}
		}
		for i := range bb.Instrs {
			in := &bb.Instrs[i]
			for _, a := range in.Args {
				expose(a)
			}
			if vi, ok := escIndex(idOf, in.To); ok {
				dfd[vi] = true
			}
			for _, d := range in.Defs() {
				if vi, ok := escIndex(idOf, d); ok {
					dfd[vi] = true
				}
			}
		}
		expose(bb.Jmp.Arg)
		for _, a := range bb.Jmp.Args {
			expose(a)
		}
		upExposed[bb], defined[bb] = ue, dfd
	}

	liveIn := map[*ir.Block]map[int]bool{}
	liveOut := map[*ir.Block]map[int]bool{}
	for _, bb := range f.Blocks {
		liveIn[bb], liveOut[bb] = map[int]bool{}, map[int]bool{}
	}
	for changed := true; changed; {
		changed = false
		for i := len(cfg.RPO) - 1; i >= 0; i-- {
			bb := cfg.RPO[i]
			out := liveOut[bb]
			for _, s := range bb.Succs() {
				if s == nil {
					continue
				}
				for _, ph := range s.Phis {
					for k, pb := range ph.Blocks {
						if pb == bb {
							if vi, ok := escIndex(idOf, ph.Args[k]); ok && !out[vi] {
								out[vi], changed = true, true
							}
						}
					}
				}
				for vi := range liveIn[s] {
					if !out[vi] {
						out[vi], changed = true, true
					}
				}
			}
			in := liveIn[bb]
			for vi := range upExposed[bb] {
				if !in[vi] {
					in[vi], changed = true, true
				}
			}
			for vi := range out {
				if !defined[bb][vi] && !in[vi] {
					in[vi], changed = true, true
				}
			}
		}
	}
	return liveIn
}

// pruneUnreachable removes blocks not reachable from the entry and drops any phi
// arguments that named them, keeping phi predecessor sets consistent with the CFG
// after a transform orphans a block. It mirrors SimplifyCFG's reachability sweep
// but is applied eagerly so the threading pass never hands on malformed SSA.
func pruneUnreachable(f *ir.Func) {
	if f.Start == nil {
		return
	}
	reach := map[*ir.Block]bool{f.Start: true}
	stack := []*ir.Block{f.Start}
	for len(stack) > 0 {
		b := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, s := range b.Succs() {
			if s != nil && !reach[s] {
				reach[s] = true
				stack = append(stack, s)
			}
		}
	}
	if len(reach) == len(f.Blocks) {
		return
	}
	kept := f.Blocks[:0]
	for _, b := range f.Blocks {
		if reach[b] {
			kept = append(kept, b)
		}
	}
	f.Blocks = kept
	for _, b := range f.Blocks {
		for _, ph := range b.Phis {
			na, nb := ph.Args[:0], ph.Blocks[:0]
			for k, pb := range ph.Blocks {
				if reach[pb] {
					na = append(na, ph.Args[k])
					nb = append(nb, pb)
				}
			}
			ph.Args, ph.Blocks = na, nb
		}
	}
}

// identityBlock is the block remapper for cloneInstr: threading copies a single
// block, so any block reference (an OBlockAddr target) keeps pointing where it
// did.
func identityBlock(b *ir.Block) *ir.Block { return b }

// replaceJmpTarget repoints every edge of a terminator that named from at to. The
// pred is never a JmpBr (findThreadCandidate excludes those), so Targets here are
// only a JmpTable's.
func replaceJmpTarget(j *ir.Jmp, from, to *ir.Block) {
	if j.To == from {
		j.To = to
	}
	if j.To2 == from {
		j.To2 = to
	}
	for k := range j.Targets {
		if j.Targets[k] == from {
			j.Targets[k] = to
		}
	}
	for k := range j.Cases {
		if j.Cases[k].Blk == from {
			j.Cases[k].Blk = to
		}
	}
}

// copyReachingMap gives a dominator-tree child its own reaching definitions.
//
// This stays a map where mem2reg's equivalent became a slice, because here the
// difference between "no definition reaches this value" and "the definition is
// the zero ref" is load-bearing: reconRename falls back to a zero constant for
// the first (and mustReach panics on it), so the presence of the key is the
// answer, not the value stored under it.
func copyReachingMap(m map[int]ir.Ref) map[int]ir.Ref {
	out := make(map[int]ir.Ref, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
