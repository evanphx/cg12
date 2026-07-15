package arm64

import (
	"fmt"
	"math"
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// colorAlloc assigns physical registers by graph colouring (Chaitin-Briggs
// optimistic colouring). It replaces the linear-scan assignment: each temporary
// becomes a node in an interference graph, nodes are removed lowest-degree first
// onto a stack, then popped and given a register a colored neighbour does not use;
// a node with no colour left is spilled. Because a spilled temp needs no register
// at all -- the emitter reloads it into a reserved scratch register at each use --
// spilling never invalidates the graph, so there is no rewrite-and-recolour
// iteration.
//
// Register-class constraints (integer vs float, call-crossing must be callee-saved,
// inline-asm clobbers, ABI pre-colouring) are modelled as a per-node set of
// forbidden colours rather than extra interference edges, so degrees stay realistic.
//
// The binding is written in place on ir.Temp (Reg / Slot); the returned allocation
// carries only spillBytes (its liveness substrate is filled by the caller).
func colorAlloc(f *ir.Func, cfg *analysis.CFG, live *analysis.Liveness, freq *analysis.Freq) (*allocation, error) {
	g := newColorGraph(f)
	g.freq = freq
	g.remat = rematRules(f)
	g.build(cfg, live)
	// A rematerialisable value is cheap to "spill" -- one ALU op per use, no memory --
	// so make it a preferred spill victim.
	for t := range f.Temps {
		if _, ok := g.remat[t]; ok {
			g.cost[t] *= 0.5
		}
	}
	alloc, err := g.assign()
	if alloc != nil {
		alloc.remat = g.remat
		coalesceSpillSlots(f, cfg, live, alloc)
	}
	return alloc, err
}

// coalesceSpillSlots re-lays the spill area so ordinary spilled temps with disjoint
// live ranges share a slot, shrinking the frame (assign gives each its own slot). GC
// references keep private slots: their whole-life stack home is part of the safepoint
// stack map, and keeping them separate keeps that mapping trivially sound.
func coalesceSpillSlots(f *ir.Func, cfg *analysis.CFG, live *analysis.Liveness, alloc *allocation) {
	var gc, ord []int
	for t := range f.Temps {
		tt := f.Temps[t]
		if tt == nil || tt.Reg != ir.NoReg || tt.Slot < 0 {
			continue // in a register, or rematerialised (Slot == -1)
		}
		if _, remat := alloc.remat[t]; remat {
			continue
		}
		if tt.GCRef {
			gc = append(gc, t)
		} else {
			ord = append(ord, t)
		}
	}
	sizeOf := func(t int) int {
		if f.Temps[t].Cls == ir.ClsQ {
			return 16
		}
		return 8
	}
	rep := slotGroups(f, cfg, live, ord, sizeOf)

	alloc.spillBytes = 0
	slotOf := map[int]int{}
	place := func(t int) {
		key := t
		if rep != nil {
			if r, ok := rep[t]; ok {
				key = r
			}
		}
		if s, ok := slotOf[key]; ok {
			f.Temps[t].Slot = s
			return
		}
		sz := sizeOf(key)
		if alloc.spillBytes%sz != 0 {
			alloc.spillBytes += sz - alloc.spillBytes%sz
		}
		slotOf[key] = alloc.spillBytes
		f.Temps[t].Slot = alloc.spillBytes
		alloc.spillBytes += sz
	}
	sort.Ints(gc)
	sort.Ints(ord)
	for _, t := range gc {
		place(t) // each GC ref its own slot (key == t)
	}
	for _, t := range ord {
		place(t) // ordinary spills coalesced via rep
	}
}

type colorGraph struct {
	f     *ir.Func
	freq  *analysis.Freq    // per-block execution frequency, for the spill cost model
	remat map[int]rematRule // temps recomputable at each use instead of reloaded

	adj       []map[int]bool // temp id -> interfering temp ids (nodes only; same class)
	forb      []map[Reg]bool // temp id -> physical registers it may not use
	node      []bool         // temp id -> is a colourable node (used, not pre-coloured)
	gc        []bool         // temp id -> must be spilled (GC ref live across a safepoint)
	mv        [][]int        // temp id -> temps it is copied to/from (coalescing bias)
	cost      []float64      // temp id -> spill cost (references weighted by block frequency)
	crossFreq []float64      // temp id -> summed frequency of the calls it is live across
}

func newColorGraph(f *ir.Func) *colorGraph {
	n := len(f.Temps)
	g := &colorGraph{
		f:         f,
		adj:       make([]map[int]bool, n),
		forb:      make([]map[Reg]bool, n),
		node:      make([]bool, n),
		gc:        make([]bool, n),
		mv:        make([][]int, n),
		cost:      make([]float64, n),
		crossFreq: make([]float64, n),
	}
	for i, t := range f.Temps {
		// A pre-coloured (Fixed) temp already holds its ABI register; it is not a
		// colourable node but constrains its neighbours (handled in addEdge).
		g.node[i] = t != nil && !t.Fixed
	}
	return g
}

func (g *colorGraph) forbid(t int, r Reg) {
	if g.forb[t] == nil {
		g.forb[t] = map[Reg]bool{}
	}
	g.forb[t][r] = true
}

// addEdge records that temps a and b are simultaneously live. Temps of different
// register classes never share registers, so no edge is needed. If either end is a
// pre-coloured temp, its fixed register simply becomes a forbidden colour for the
// other; otherwise a real interference edge joins the two nodes.
func (g *colorGraph) addEdge(a, b int) {
	if a == b {
		return
	}
	ta, tb := g.f.Temps[a], g.f.Temps[b]
	if ta.Cls.IsFloat() != tb.Cls.IsFloat() {
		return
	}
	switch {
	case ta.Fixed && tb.Fixed:
		return // two pinned registers; a genuine ABI clash surfaces at colouring
	case ta.Fixed:
		g.forbid(b, Reg(ta.Reg))
	case tb.Fixed:
		g.forbid(a, Reg(tb.Reg))
	default:
		if g.adj[a] == nil {
			g.adj[a] = map[int]bool{}
		}
		if g.adj[b] == nil {
			g.adj[b] = map[int]bool{}
		}
		g.adj[a][b] = true
		g.adj[b][a] = true
	}
}

// build constructs the interference graph and the forbidden-colour sets by a
// backward walk of each block seeded from its live-out set (Appel, Modern Compiler
// Implementation, ch. 11).
func (g *colorGraph) build(cfg *analysis.CFG, live *analysis.Liveness) {
	f := g.f
	for _, b := range cfg.RPO {
		// A reference's spill cost is its execution frequency: reloading in a hot loop
		// is expensive, in a rarely-hit switch case nearly free. Unlike loop depth, this
		// distinguishes a cold handler from the dispatch loop it sits inside. Floor it so
		// even ice-cold (or unreachable-but-emitted) references stay allocatable.
		weight := math.Max(g.freq.Of(b), 1e-4)
		liveSet := live.LiveOut(b).Copy()
		// The terminator's operands are used at the block's end, so they are live
		// there and interfere with anything defined among the trailing instructions
		// (a phi copy DestructSSA appended before a computed goto must not land on the
		// register holding the branch target).
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
			uses := instrUses(in)
			for _, d := range defs {
				g.cost[d] += weight
			}
			for _, u := range uses {
				g.cost[u] += weight
			}

			// A move's destination must not interfere with its source, so the copy
			// can later coalesce; drop the source from the live set first, and record
			// the pair so colouring biases them toward a shared register.
			if mdst, msrc, ok := moveEnds(in); ok {
				liveSet.Remove(msrc)
				g.mv[mdst] = append(g.mv[mdst], msrc)
				g.mv[msrc] = append(g.mv[msrc], mdst)
			}

			// A value live across a call prefers a callee-saved register (no save/restore)
			// but may use a caller-saved one, which insertCallerSaves then saves and
			// restores around each crossed call. Accumulate the frequency of the crossed
			// calls so colouring can weigh that cost. Exception: a 128-bit vector has NO
			// register that survives a call -- AAPCS64 preserves only the low 64 bits of
			// V8-V15 -- so it must stay out of every register across a call (hard forbid).
			if in.Op == ir.OCall {
				for _, t := range liveSet.Members() {
					g.crossFreq[t] += weight
					if f.Temps[t].Cls == ir.ClsQ {
						for r := V0; r <= vLast; r++ {
							g.forbid(t, r)
						}
					}
				}
			}
			// An asm's clobber list names registers its template writes. Every value
			// touching the asm must avoid them: not just values live across it, but its
			// own inputs (read by the template alongside the clobbering writes) and its
			// output (which must not land on a register the template also overwrites).
			if in.Op == ir.OAsm {
				regs := asmClobberRegs(in)
				forbidAll := func(t int) {
					for _, r := range regs {
						g.forbid(t, r)
					}
				}
				for _, t := range liveSet.Members() {
					forbidAll(t)
				}
				for _, u := range uses {
					forbidAll(u)
				}
				for _, d := range defs {
					forbidAll(d)
				}
			}
			if in.Op == ir.OCall || in.Op == ir.OSafepoint {
				for _, t := range liveSet.Members() {
					if f.Temps[t].GCRef {
						g.gc[t] = true
					}
				}
			}

			// def(I) interferes with everything live immediately after I.
			for _, d := range defs {
				liveSet.Add(d)
			}
			for _, d := range defs {
				for _, l := range liveSet.Members() {
					g.addEdge(d, l)
				}
			}
			// Some instructions read an operand after they have written their result,
			// so the result register must differ from the operands (a normal op may
			// reuse them). Inline asm's inputs are early-clobber against its outputs,
			// and an atomic read-modify-write loop re-reads its address and value on
			// every iteration after loading the old value into the result.
			if in.Op == ir.OAsm || atomicHoldsResult(in) {
				for _, d := range defs {
					for _, u := range uses {
						g.addEdge(d, u)
					}
				}
			}

			for _, d := range defs {
				liveSet.Remove(d)
			}
			for _, u := range uses {
				liveSet.Add(u)
			}
		}
	}
}

// assign removes nodes lowest-degree first, then colours them in reverse, spilling
// any node no colour fits.
func (g *colorGraph) assign() (*allocation, error) {
	f := g.f
	alloc := &allocation{}
	slotOf := map[int]int{}
	spill := func(t int) {
		tt := f.Temps[t]
		tt.Reg = ir.NoReg
		// A rematerialisable value needs no slot: the emitter recomputes it at each use.
		if _, ok := g.remat[t]; ok {
			tt.Slot = -1
			return
		}
		if _, ok := slotOf[t]; !ok {
			sz := 8
			if tt.Cls == ir.ClsQ {
				sz = 16
			}
			if alloc.spillBytes%sz != 0 {
				alloc.spillBytes += sz - alloc.spillBytes%sz
			}
			slotOf[t] = alloc.spillBytes
			alloc.spillBytes += sz
		}
		tt.Slot = slotOf[t]
	}

	removed := make([]bool, len(f.Temps))
	degree := make([]int, len(f.Temps))
	for t := range f.Temps {
		removed[t] = !g.node[t]
	}
	// A managed reference live across a safepoint is kept in a stack slot for its
	// whole life so the GC finds it at a fixed frame offset.
	for t := range f.Temps {
		if g.node[t] && g.gc[t] {
			spill(t)
			removed[t] = true
		}
	}
	// Pre-assign callee-saved registers to the hottest call-crossing values, costliest
	// first. Such a value is live across many calls -- a caller-saved register would
	// force a save/restore around each -- and it interferes with almost everything (a
	// whole interpreter dispatch mesh), so the graph colourer removes it, and thus
	// colours it, in an order set by when its degree falls below availK, not by cost.
	// A hotter value (the interpreter's cfp) is then coloured only after a colder one
	// (a cost-1 argument temp) has taken the last callee-saved register, and spills to
	// a caller-saved one that is save/restored at every dispatched call. Greedily giving
	// each the first callee-saved register no higher-priority interfering neighbour has
	// taken fixes the priority directly -- as gcc's IRA allocates high-priority allocnos
	// first -- keeping pc/cfp/ec/sp in callee-saved registers across the whole mesh. A
	// pre-coloured value is removed from the graph; its register becomes a forbidden
	// colour for every interfering neighbour, exactly like an ABI-fixed temp.
	type crossCand struct {
		t    int
		cost float64
	}
	var cands []crossCand
	for t := range f.Temps {
		if removed[t] || !g.node[t] || f.Temps[t].Cls.IsFloat() {
			continue
		}
		// preferCallee: the save/restore bill of a caller-saved register (crossFreq*2)
		// outweighs the single prologue save/restore of a callee-saved one (cost ~2).
		if g.crossFreq[t] > 0 && g.crossFreq[t]*2 >= 2.0 && g.cost[t] >= 1.0 {
			cands = append(cands, crossCand{t, g.cost[t]})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].cost > cands[j].cost })
	for _, c := range cands {
		pick := Reg(ir.NoReg)
		for _, r := range intAllocOrder {
			if calleeSavedReg(r) && !g.forb[c.t][r] {
				pick = r
				break
			}
		}
		if pick == Reg(ir.NoReg) {
			continue // no callee-saved register free; leave this one to the colourer
		}
		f.Temps[c.t].Reg = int(pick)
		removed[c.t] = true
		for w := range g.adj[c.t] {
			if !removed[w] {
				g.forbid(w, pick)
			}
		}
	}

	// availK is how many colours a node could actually take: its class pool minus
	// the registers forbidden to it. A node with fewer interfering neighbours than
	// that (degree < availK) is guaranteed a colour.
	availK := make([]int, len(f.Temps))
	for t := range f.Temps {
		if !g.node[t] {
			continue
		}
		pool := intAllocOrder
		if f.Temps[t].Cls.IsFloat() {
			pool = floatAllocOrder
		}
		for _, r := range pool {
			if !g.forb[t][r] {
				availK[t]++
			}
		}
	}

	active := 0
	for t := range f.Temps {
		if removed[t] {
			continue
		}
		active++
		for w := range g.adj[t] {
			if !removed[w] {
				degree[t]++
			}
		}
	}

	// Simplify: remove a node guaranteed to colour (degree < availK) and push it; it
	// is coloured last, when it will still find a colour. When only high-degree nodes
	// remain, remove the cheapest to spill -- lowest spill-cost per neighbour freed --
	// as an optimistic potential spill: it is coloured first and only actually spills
	// if no colour is left.
	//
	// Since colouring is the reverse of removal, removing a guaranteed-colourable node
	// first colours it last, so it takes whatever register is left; keeping a node in
	// the graph until last colours it first, with the widest choice. That choice of
	// order matters most for a value live across a call: it prefers a callee-saved
	// register (no save/restore around every crossed call), and those are scarce.
	//
	// A warm (non-trivially used) call-crossing value is therefore kept for last, so it
	// is coloured first and wins a callee-saved register -- this is what lets an
	// interpreter's hot loop-carried state (pc, cfp, ec) stay in callee-saved registers
	// across the whole dispatch mesh instead of being save/restored at every handler
	// call. Left to min-degree, the smallest-degree such value (often the hottest, e.g.
	// pc) becomes colourable first and is pushed to the bottom of the stack, coloured
	// last, into a caller-saved register. Among several warm crossing values the costlier
	// is kept for last (coloured first). Everything else keeps the cold-first, then
	// least-constrained (min-degree) order -- so pressure-free code, and pointer walks
	// whose values do not cross calls, keep the incidental register coincidence that
	// folds an advance into a pre/post-indexed writeback. Reordering low nodes never
	// changes which node spills (degree < availK holds for any order among them), only
	// the register each is given.
	coldRank := func(t int) int {
		if g.cost[t] < 1.0 {
			return 0
		}
		return 1
	}
	warmCross := func(t int) bool { return g.crossFreq[t] > 0 && g.cost[t] >= 1.0 }
	// moreRemovable reports whether a should be removed before b (thus coloured after).
	moreRemovable := func(a, b int) bool {
		wa, wb := warmCross(a), warmCross(b)
		if wa != wb {
			return !wa // keep warm-crossing values in the graph until last
		}
		if wa { // both warm-crossing: remove the cheaper first, colour the costlier first
			return g.cost[a] < g.cost[b]
		}
		if coldRank(a) != coldRank(b) {
			return coldRank(a) < coldRank(b) // cold first
		}
		return degree[a] < degree[b] // then least constrained
	}
	stack := make([]int, 0, active)
	for active > 0 {
		low, spillCand := -1, -1
		for t := range f.Temps {
			if !g.node[t] || removed[t] {
				continue
			}
			if degree[t] < availK[t] {
				if low < 0 || moreRemovable(t, low) {
					low = t
				}
			}
			if spillCand < 0 || spillRatio(g.cost[t], degree[t]) < spillRatio(g.cost[spillCand], degree[spillCand]) {
				spillCand = t
			}
		}
		best := low
		if best < 0 {
			best = spillCand
		}
		removed[best] = true
		stack = append(stack, best)
		active--
		for w := range g.adj[best] {
			if !removed[w] {
				degree[w]--
			}
		}
	}

	// Select: pop the stack, giving each node a register no colored neighbour uses.
	for i := len(stack) - 1; i >= 0; i-- {
		t := stack[i]
		used := map[Reg]bool{}
		for w := range g.adj[t] {
			if r := f.Temps[w].Reg; r != ir.NoReg {
				used[Reg(r)] = true
			}
		}
		// A value live across a call weighs callee-saved (one prologue save/restore,
		// cost ~2) against caller-saved (a save+restore around each crossed call, cost
		// crossFreq×2). preferCallee holds when the caller-saved bill is the larger, so
		// the value tries callee-saved registers first; otherwise it takes caller-saved
		// freely (cheap when the crossed calls are cold, the interpreter's case).
		float := f.Temps[t].Cls.IsFloat()
		crossing := g.crossFreq[t] > 0
		preferCallee := !f.GoABI && crossing && g.crossFreq[t]*2 >= 2.0
		pool := intAllocOrder
		switch {
		case preferCallee && float:
			pool = floatAllocOrderCalleeFirst
		case preferCallee:
			pool = intAllocOrderCalleeFirst
		case float:
			pool = floatAllocOrder
		}
		// Coalesce by biasing: prefer a register a move partner already holds so the copy
		// drops. For a value whose crossings are hot, never bias onto a caller-saved
		// register -- that would trade one dropped move for a save/restore at every
		// crossed call.
		prefer := map[Reg]bool{}
		for _, p := range g.mv[t] {
			if r := f.Temps[p].Reg; r != ir.NoReg {
				if preferCallee && !calleeSavedFor(f.GoABI, Reg(r)) {
					continue
				}
				prefer[Reg(r)] = true
			}
		}
		usable := func(r Reg) bool { return !used[r] && !g.forb[t][r] }
		picked := Reg(ir.NoReg)
		for _, r := range pool {
			if prefer[r] && usable(r) {
				picked = r
				break
			}
		}
		if picked == Reg(ir.NoReg) {
			for _, r := range pool {
				if usable(r) {
					picked = r
					break
				}
			}
		}
		// If the only register a hot-crossing value could get is caller-saved, wrapping
		// it costs crossFreq×2; spilling costs its reference weight. Take the cheaper.
		if picked != Reg(ir.NoReg) && preferCallee && !calleeSavedFor(f.GoABI, picked) &&
			g.crossFreq[t]*2 >= g.cost[t] {
			picked = Reg(ir.NoReg)
		}
		if picked == Reg(ir.NoReg) {
			spill(t)
		} else {
			f.Temps[t].Reg = int(picked)
		}
	}

	if err := g.verifyPrecolor(); err != nil {
		return nil, err
	}
	return alloc, nil
}

// spillRatio ranks spill candidates: spill cost per interfering neighbour freed. A
// low ratio -- a cheap, rarely-used value that unblocks many others -- is the best
// thing to spill. A degree of zero cannot arise here (such a node is always a
// simplify candidate) but is guarded so the ratio stays finite.
func spillRatio(cost float64, degree int) float64 {
	if degree < 1 {
		degree = 1
	}
	return cost / float64(degree)
}

// verifyPrecolor guards the ABI: a pre-coloured temp must keep its register. (It is
// never a node, so this only catches a genuinely over-constrained ABI where the
// lowering pinned two simultaneously-live values to one register.)
func (g *colorGraph) verifyPrecolor() error {
	for i, t := range g.f.Temps {
		if t != nil && t.Fixed && t.Reg == ir.NoReg {
			return fmt.Errorf("arm64: could not honour pre-coloured register for temp %%%s", g.f.Temps[i].Name)
		}
	}
	return nil
}

// instrDefs returns the temporaries an instruction defines (its result plus any
// extra call-defined registers).
func instrDefs(in *ir.Instr) []int {
	var d []int
	if in.To.Kind == ir.RefTemp {
		d = append(d, int(in.To.ID))
	}
	for _, x := range in.Defs {
		if x.Kind == ir.RefTemp {
			d = append(d, int(x.ID))
		}
	}
	return d
}

// instrUses returns the temporaries an instruction reads. A lifetime marker names
// an alloca to bound its slot's live region, not to read the address value, so it
// reads nothing — counting its operand would spuriously extend the alloca's live
// range in the allocator's interference and caller-save analyses.
func instrUses(in *ir.Instr) []int {
	if in.Op.IsLifetime() {
		return nil
	}
	var u []int
	for _, a := range in.Args {
		if a.Kind == ir.RefTemp {
			u = append(u, int(a.ID))
		}
	}
	return u
}

// moveEnds reports whether an instruction is a register-to-register move
// (OCopy/OPar/OArg with a single temp source and a temp destination) and returns
// its destination and source temps.
func moveEnds(in *ir.Instr) (dst, src int, ok bool) {
	switch in.Op {
	case ir.OCopy, ir.OPar, ir.OArg:
		if in.To.Kind == ir.RefTemp && len(in.Args) == 1 && in.Args[0].Kind == ir.RefTemp {
			return int(in.To.ID), int(in.Args[0].ID), true
		}
	}
	return 0, 0, false
}
