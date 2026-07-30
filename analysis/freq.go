package analysis

import "github.com/evanphx/cg12/ir"

// Static branch-probability and block-frequency estimation, in the style of GCC's
// predict.c (Wu & Larus, "Static Branch Frequency and Program Profile Analysis").
// The register allocator's cost model multiplies each reference by its block's
// frequency, so a value used in a rarely-taken switch case costs far less to spill
// than one used every loop iteration -- the distinction loop depth alone cannot make.

const (
	probHlt      = 0.001  // a branch to a trap/noreturn block is almost never taken
	probBackEdge = 0.9    // a loop back edge is taken ~90% of the time
	probLoopStay = 0.9    // staying in a loop is far likelier than exiting
	probExit     = 0.1    // an arm that soon returns/traps (an error path) is unlikely
	probExpect   = 0.9    // a __builtin_expect-hinted edge is taken almost always
	maxTrip      = 1000.0 // cap a loop's estimated trip count
	minFreq      = 1e-9   // reachable blocks never underflow to zero
	maxFreq      = 1e12   // keeps float64 exact (< 2^53) even for deep nests
)

// Freq holds estimated execution frequencies: the entry block is 1.0, a block that
// runs ten times as often is 10.0. Edge probabilities are stored parallel to
// Block.Succs() (not keyed by target) so a jnz double edge, duplicate switch cases,
// and repeated computed-goto targets all sum correctly.
type Freq struct {
	Block map[*ir.Block]float64
	Edge  map[*ir.Block][]float64
}

// Of returns b's frequency, or 0 for an unreachable block (absent from the CFG).
func (fr *Freq) Of(b *ir.Block) float64 { return fr.Block[b] }

// Prob returns the probability of the i-th successor edge of b (parallel to Succs()).
func (fr *Freq) Prob(b *ir.Block, i int) float64 {
	e := fr.Edge[b]
	if i < 0 || i >= len(e) {
		return 0
	}
	return e[i]
}

// Frequency estimates per-block execution frequencies for the reachable CFG.
func (c *CFG) Frequency(dom *DomTree) *Freq {
	lf := c.LoopForest(dom)
	fr := &Freq{Block: map[*ir.Block]float64{}, Edge: map[*ir.Block][]float64{}}
	for _, b := range c.RPO {
		fr.Edge[b] = edgeProbs(b, lf, dom)
	}
	probEdge := func(p, b *ir.Block) float64 {
		e, s := fr.Edge[p], 0.0
		for i, succ := range p.Succs() {
			if succ == b && i < len(e) {
				s += e[i]
			}
		}
		return s
	}

	// Pass A: each loop's trip multiplier, innermost first. Within a loop, compute
	// header-relative frequencies (header = 1) over a DAG with the loop's and inner
	// loops' back edges removed, folding each inner loop's multiplier in at its
	// header. The cyclic probability -- the chance of returning to the header via a
	// back edge in one trip -- gives the multiplier 1/(1-cp).
	mult := map[*Loop]float64{}
	for _, l := range lf.Loops {
		local := map[*ir.Block]float64{l.Header: 1.0}
		for _, b := range c.RPO {
			if b == l.Header || !l.Body[b] {
				continue
			}
			s := 0.0
			for _, p := range b.Preds {
				if c.Num(p) < 0 || !l.Body[p] || c.Num(p) >= c.Num(b) {
					continue // unreachable, entering, or a (back/retreating) edge
				}
				s += local[p] * probEdge(p, b)
			}
			if il := lf.ByHeader[b]; il != nil && il != l {
				s *= mult[il] // collapse the inner loop into its header
			}
			local[b] = s
		}
		// Cyclic probability: the chance an iteration stays in the loop rather than
		// leaving it, i.e. 1 minus the per-iteration probability of taking an edge out
		// of the body. Summing the local frequency that leaves counts every exit, not
		// only a back edge to the header -- so a computed-goto interpreter's dispatch
		// mesh, which continues by branching to any handler (never back to a single
		// header), is recognized as the hot loop it is instead of a run-once region.
		// For an ordinary single-exit loop this equals the old header-return sum.
		//
		// Summed in reverse postorder rather than over local's map iteration:
		// floating-point addition is not associative, so a map order would give a
		// different cyclic probability -- and thus a different multiplier, block
		// frequency and spill choice -- on each run of the same compiler.
		exit := 0.0
		for _, b := range c.RPO {
			lb, inLoop := local[b]
			if !inLoop {
				continue
			}
			for i, s := range b.Succs() {
				if s != nil && !l.Body[s] {
					exit += lb * fr.Edge[b][i]
				}
			}
		}
		cp := 1 - exit
		if cp > 1-1/maxTrip {
			cp = 1 - 1/maxTrip
		}
		if cp < 0 {
			cp = 0
		}
		mult[l] = 1 / (1 - cp)
	}

	// Pass B: one RPO pass over the whole function with back/retreating edges removed
	// (so it reads only already-computed predecessors), scaling each loop header by
	// its multiplier. A header thus carries the whole loop's iteration count, and the
	// frequency flowing out to the body and past the exit is scaled with it.
	for i, b := range c.RPO {
		s := 1.0
		if i != 0 {
			s = 0.0
			for _, p := range b.Preds {
				if c.Num(p) < 0 || c.Num(p) >= c.Num(b) {
					continue // unreachable pred, or a back/retreating edge (dropped)
				}
				s += fr.Block[p] * probEdge(p, b)
			}
		}
		if l := lf.ByHeader[b]; l != nil {
			s *= mult[l]
		}
		if s < minFreq {
			s = minFreq
		}
		if s > maxFreq {
			s = maxFreq
		}
		fr.Block[b] = s
	}

	redistributeMesh(c, fr, probEdge)
	return fr
}

// meshIters is how many times the mesh flow is relaxed. A computed-goto dispatch
// connects every handler in a single step, so the visit *distribution* mixes in a
// handful of iterations; the magnitude is preserved separately, so full convergence
// (which would take ~the trip count) is unnecessary.
const meshIters = 50

// redistributeMesh corrects the within-region visit distribution of every irreducible
// cyclic region reached through a computed goto -- an interpreter's dispatch mesh. The
// two passes above propagate frequency only along a spanning DAG (retreating edges
// dropped), so a handler the dispatch reaches by `goto *pc` lands at near-entry
// frequency while another gets the loop's whole multiplier: within the mesh the
// distribution is meaningless even though its total magnitude is about right. Relax the
// real flow over the region's own edges (the computed-goto back edges included), seeded
// by the frequency entering from outside, then rescale to the total mass the passes
// already assigned -- so only the distribution changes, not the region's overall
// weight. Ordinary reducible loops carry no computed goto and are left untouched.
func redistributeMesh(c *CFG, fr *Freq, probEdge func(p, b *ir.Block) float64) {
	for _, comp := range c.SCCs() {
		if len(comp) < 2 {
			continue
		}
		mesh := false
		for _, b := range comp {
			if b.Jmp.Kind == ir.JmpBr {
				mesh = true
				break
			}
		}
		if !mesh {
			continue
		}
		in := make(map[*ir.Block]bool, len(comp))
		for _, b := range comp {
			in[b] = true
		}
		// External inflow per block (fixed), and the mass to preserve.
		inflow := make(map[*ir.Block]float64, len(comp))
		mass := 0.0
		for _, b := range comp {
			mass += fr.Block[b]
			for _, p := range b.Preds {
				if c.Num(p) >= 0 && !in[p] {
					inflow[b] += fr.Block[p] * probEdge(p, b)
				}
			}
		}
		// Relax flow[b] = inflow[b] + sum over internal predecessors flow[p]*prob(p->b),
		// pushing along successor edges so the computed goto's back edges are honoured.
		flow := make(map[*ir.Block]float64, len(comp))
		for _, b := range comp {
			flow[b] = inflow[b]
		}
		for iter := 0; iter < meshIters; iter++ {
			next := make(map[*ir.Block]float64, len(comp))
			for b, v := range inflow {
				next[b] = v
			}
			for _, b := range comp {
				fb := flow[b]
				if fb == 0 {
					continue
				}
				for i, s := range b.Succs() {
					if s != nil && in[s] {
						next[s] += fb * fr.Prob(b, i)
					}
				}
			}
			flow = next
		}
		// Over the component, not over the flow map: as with the cyclic probability
		// above, summing floats in map order makes the scale differ between runs.
		total := 0.0
		for _, b := range comp {
			total += flow[b]
		}
		if total <= 0 {
			continue
		}
		scale := mass / total
		for _, b := range comp {
			s := flow[b] * scale
			if s < minFreq {
				s = minFreq
			}
			if s > maxFreq {
				s = maxFreq
			}
			fr.Block[b] = s
		}
	}
}

// edgeProbs assigns probabilities to b's out-edges, parallel to b.Succs().
func edgeProbs(b *ir.Block, lf *LoopForest, dom *DomTree) []float64 {
	succs := b.Succs()
	switch len(succs) {
	case 0:
		return nil
	case 1:
		return []float64{1.0}
	}
	if b.Jmp.Kind == ir.JmpJnz {
		return jnzProbs(b, succs, lf, dom)
	}
	return multiwayProbs(b, succs, lf, dom)
}

// jnzProbs handles a two-way conditional branch with succs [true, false], applying
// GCC-style heuristics in priority order.
func jnzProbs(b *ir.Block, succs []*ir.Block, lf *LoopForest, dom *DomTree) []float64 {
	skew := func(i int, p float64) []float64 { // succs[i] taken with probability p
		if i == 0 {
			return []float64{p, 1 - p}
		}
		return []float64{1 - p, p}
	}
	hlt := func(i int) bool { return succs[i] != nil && succs[i].Jmp.Kind == ir.JmpHlt }
	back := func(i int) bool { return succs[i] != nil && dom.Dominates(succs[i], b) }
	exit := func(i int) bool { return exitsSoon(succs[i]) }
	switch {
	case hlt(0) != hlt(1): // trap/noreturn arm is almost never taken
		return skew(pick(hlt), probHlt)
	case back(0) != back(1): // the back edge is the likely arm
		return skew(pick(back), probBackEdge)
	}
	// An explicit __builtin_expect (Ruby's RB_LIKELY/RB_UNLIKELY) overrides the
	// softer heuristics below: the source states which edge is taken.
	switch b.Jmp.Likely {
	case ir.LikelyTo:
		return skew(0, probExpect)
	case ir.LikelyTo2:
		return skew(1, probExpect)
	}
	if l := lf.In[b]; l != nil { // loop exit: staying is likely
		out0, out1 := !l.Body[succs[0]], !l.Body[succs[1]]
		if out0 != out1 {
			stay := 1
			if out1 {
				stay = 0
			}
			return skew(stay, probLoopStay)
		}
	}
	if exit(0) != exit(1) { // an arm that soon returns (an error/early-exit path) is unlikely
		return skew(pick(exit), probExit)
	}
	return []float64{0.5, 0.5}
}

// exitsSoon reports whether b leads to a function return or trap along an
// unconditional chain, without a branch or loop in between -- the shape of an error
// or early-exit arm (a guard failing into `goto err; ... return throw(...)`). Such an
// arm is treated as unlikely so it does not dilute the frequency of the path that
// continues into the function's real work.
func exitsSoon(b *ir.Block) bool {
	for i := 0; i < 6 && b != nil; i++ {
		switch b.Jmp.Kind {
		case ir.JmpRet, ir.JmpHlt:
			return true
		case ir.JmpJmp:
			b = b.Jmp.To
		default:
			return false // a branch: the path is no longer a straight run to an exit
		}
	}
	return false
}

// pick returns the index (0 or 1) for which the predicate holds.
func pick(f func(int) bool) int {
	if f(0) {
		return 0
	}
	return 1
}

// multiwayProbs handles switch/computed-goto/jump-table edges by weight: a trap arm
// is negligible, a loop-exit arm is downweighted, everything else is equal. This
// makes each of a switch's N in-loop cases about 1/N of the header -- exactly what
// turns a rarely-hit opcode handler cold.
func multiwayProbs(b *ir.Block, succs []*ir.Block, lf *LoopForest, dom *DomTree) []float64 {
	l := lf.In[b]
	w := make([]float64, len(succs))
	total := 0.0
	for i, s := range succs {
		switch {
		case s == nil:
			w[i] = 0
		case s.Jmp.Kind == ir.JmpHlt:
			w[i] = probHlt
		case l != nil && !l.Body[s] && !dom.Dominates(s, b):
			w[i] = 1 - probLoopStay // leaving the loop is unlikely
		default:
			w[i] = 1
		}
		total += w[i]
	}
	if total == 0 {
		total = 1
	}
	for i := range w {
		w[i] /= total
	}
	return w
}
