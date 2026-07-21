package amd64

import (
	"fmt"
	"math"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// colorAlloc assigns physical registers by graph colouring (Chaitin-Briggs
// optimistic colouring), replacing linear scan. Each temporary is a node in an
// interference graph; nodes are removed lowest-degree first onto a stack, then
// popped and given a register a coloured neighbour does not use; a node with no
// colour left is spilled. Because a spilled temp needs no register at all -- the
// emitter reloads it into a reserved scratch register at each use -- spilling never
// invalidates the graph, so there is no rewrite-and-recolour iteration.
//
// Register-class constraints (integer vs XMM, call-crossing must be callee-saved,
// inline-asm clobbers, ABI pre-colouring) are modelled as a per-node set of
// forbidden colours rather than extra interference edges, so degrees stay
// realistic. The spill cost is each reference weighted by its block's execution
// frequency, so a value used in a cold path is a cheaper spill victim than one in
// a hot loop -- the win over linear scan's start-order eviction.
//
// The binding is written in place on ir.Temp (Reg / Slot); the returned allocation
// carries only spillBytes (its liveness substrate is filled by the caller).
func colorAlloc(f *ir.Func, cfg *analysis.CFG, live *analysis.Liveness, freq *analysis.Freq) (*allocation, error) {
	g := newColorGraph(f)
	g.freq = freq
	g.remat = rematRules(f)
	g.build(cfg, live)
	// Remat recomputes a value at each use rather than reloading it, so a spilled
	// rematerialisable temp is never worse than a spilled ordinary one (an lea/mov
	// immediate instead of a memory load). But it is NOT made a preferred spill
	// victim: biasing its cost down spilled hot alloca addresses that are used as
	// memory bases every loop iteration, turning a kept register into a recomputed
	// lea per use -- a large regression. So remat only helps values the frequency
	// cost model already chose to spill; it never causes an extra spill.
	alloc, err := g.assign()
	if alloc != nil {
		alloc.remat = g.remat
	}
	return alloc, err
}

type colorGraph struct {
	f    *ir.Func
	freq *analysis.Freq // per-block execution frequency, for the spill cost model

	adj       []map[int]bool    // temp id -> interfering temp ids (nodes only; same class)
	forb      []map[Reg]bool    // temp id -> physical registers it may not use
	node      []bool            // temp id -> is a colourable node (used, not pre-coloured)
	gc        []bool            // temp id -> must be spilled (GC ref live across a safepoint)
	mv        [][]int           // temp id -> temps it is copied to/from (coalescing bias)
	cost      []float64         // temp id -> spill cost (references weighted by block frequency)
	crossFreq []float64         // temp id -> summed frequency of the calls it is live across
	remat     map[int]rematRule // temps recomputable at each use instead of reloaded
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
		// A pre-coloured (Fixed) temp already holds its ABI/fixed register (a
		// division's RDX:RAX, a shift's CL, an argument register); it is not a
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
		// is expensive, in a rarely-hit switch case nearly free. Floor it so even
		// ice-cold (or unreachable-but-emitted) references stay allocatable.
		weight := math.Max(g.freq.Of(b), 1e-4)
		liveSet := live.LiveOut(b).Copy()
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

			// A value live across a call prefers a callee-saved register (no
			// save/restore) but may use a caller-saved one, which insertCallerSaves then
			// saves and restores around each crossed call. Accumulate the frequency of
			// the crossed calls so colouring can weigh that cost against callee-saved and
			// against spilling.
			if in.Op == ir.OCall {
				for _, t := range liveSet.Members() {
					g.crossFreq[t] += weight
				}
			}
			// An asm's clobber list names registers its template writes. Every value
			// touching the asm must avoid them: values live across it, its own inputs
			// (read alongside the clobbering writes), and its output.
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
			// Inline asm's inputs are early-clobber against its outputs: the result
			// register must differ from every operand register.
			if in.Op == ir.OAsm {
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
			slotOf[t] = alloc.spillBytes
			alloc.spillBytes += 8
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
	// availK is how many colours a node could actually take: its class pool minus
	// the registers forbidden to it. A node with fewer interfering neighbours than
	// that (degree < availK) is guaranteed a colour.
	availK := make([]int, len(f.Temps))
	for t := range f.Temps {
		if !g.node[t] {
			continue
		}
		for _, r := range g.pool(t) {
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
	stack := make([]int, 0, active)
	for active > 0 {
		low, spillCand := -1, -1
		for t := range f.Temps {
			if !g.node[t] || removed[t] {
				continue
			}
			if degree[t] < availK[t] {
				if low < 0 || degree[t] < degree[low] {
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

	// Select: pop the stack, giving each node a register no coloured neighbour uses,
	// biased toward a move partner's register so the copy can drop (coalescing).
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
		crossing := g.crossFreq[t] > 0
		preferCallee := crossing && g.crossFreq[t]*2 >= 2.0
		pool := g.pool(t)
		if preferCallee {
			pool = intAllocOrderCalleeFirst
			if f.Temps[t].Cls.IsFloat() {
				pool = floatAllocOrderCalleeFirst
			}
		}
		// Coalesce by biasing: prefer a register a move partner already holds. For a
		// value whose crossings are hot, never bias onto a caller-saved register --
		// that would trade one dropped move for a save/restore at every crossed call.
		prefer := map[Reg]bool{}
		for _, p := range g.mv[t] {
			if r := f.Temps[p].Reg; r != ir.NoReg {
				if preferCallee && !calleeSavedReg(Reg(r)) {
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
		if picked != Reg(ir.NoReg) && preferCallee && !calleeSavedReg(picked) &&
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

// pool is the register allocation order for temp t's class.
func (g *colorGraph) pool(t int) []Reg {
	if g.f.Temps[t].Cls.IsFloat() {
		return floatAllocOrder
	}
	return intAllocOrder
}

// spillRatio ranks spill candidates: spill cost per interfering neighbour freed. A
// low ratio -- a cheap, rarely-used value that unblocks many others -- is the best
// thing to spill.
func spillRatio(cost float64, degree int) float64 {
	if degree < 1 {
		degree = 1
	}
	return cost / float64(degree)
}

// verifyPrecolor guards the ABI: a pre-coloured temp must keep its register.
func (g *colorGraph) verifyPrecolor() error {
	for i, t := range g.f.Temps {
		if t != nil && t.Fixed && t.Reg == ir.NoReg {
			return fmt.Errorf("amd64: could not honour pre-coloured register for temp %%%s", g.f.Temps[i].Name)
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

// instrUses returns the temporaries an instruction reads.
func instrUses(in *ir.Instr) []int {
	var u []int
	for _, a := range in.Args {
		if a.Kind == ir.RefTemp {
			u = append(u, int(a.ID))
		}
	}
	return u
}

// moveEnds reports whether an instruction is a register-to-register move and
// returns its destination and source temps.
func moveEnds(in *ir.Instr) (dst, src int, ok bool) {
	switch in.Op {
	case ir.OCopy, ir.OPar, ir.OArg:
		if in.To.Kind == ir.RefTemp && len(in.Args) == 1 && in.Args[0].Kind == ir.RefTemp {
			return int(in.To.ID), int(in.Args[0].ID), true
		}
	}
	return 0, 0, false
}
