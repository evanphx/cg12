package analysis

import "github.com/evanphx/cg12/ir"

// Liveness holds live-in and live-out temp sets for every reachable block.
//
// Phi semantics follow standard SSA liveness: a phi's result is considered
// defined at the top of its block (so it is not live-in), while each phi operand
// is a use on the corresponding predecessor edge (so it is live-out of that
// predecessor, not live-in of the phi's block).
type Liveness struct {
	In  map[*ir.Block]*TempSet
	Out map[*ir.Block]*TempSet
}

// Liveness computes block-level liveness by backward dataflow to a fixpoint.
func (c *CFG) Liveness() *Liveness {
	nt := len(c.Fn.Temps)
	l := &Liveness{
		In:  make(map[*ir.Block]*TempSet, len(c.RPO)),
		Out: make(map[*ir.Block]*TempSet, len(c.RPO)),
	}
	for _, b := range c.RPO {
		l.In[b] = NewTempSet(nt)
		l.Out[b] = NewTempSet(nt)
	}

	// The loop below allocated two sets and a successor slice for every block of
	// every round, and threw all three away again -- on a -O compile of the SQLite
	// amalgamation, NewTempSet and Copy together were 200 MB, most of it here.
	// None of it is necessary, because this dataflow only ever grows a set:
	//
	// Each round recomputed a block's live-out from scratch and replaced the
	// stored one. But live-in sets only gain elements, so the recomputed live-out
	// always contained the stored one -- accumulating into the stored set gives
	// the same answer without a new one. The same holds one level up: the new
	// live-in always contains the old, so unioning it in and asking whether that
	// changed anything is the same test as comparing them and replacing, which is
	// what lets the working set be one scratch buffer reused across every block.
	live := NewTempSet(nt)
	for changed := true; changed; {
		changed = false
		// Process near-exit blocks first so successors' live-in is fresh.
		for i := len(c.RPO) - 1; i >= 0; i-- {
			b := c.RPO[i]

			out := l.Out[b]
			// Successors of a reachable block are reachable, hence in l.In.
			for k, count := 0, succCount(b); k < count; k++ {
				s := succAt(b, k)
				if s == nil {
					continue
				}
				out.Union(l.In[s])
				// Operands of successor phis flow out along this edge.
				for _, p := range s.Phis {
					for k, pb := range p.Blocks {
						if pb == b {
							out.AddRef(p.Args[k])
						}
					}
				}
			}

			live.SetTo(out)
			live.AddRef(b.Jmp.Arg) // terminator use
			for _, a := range b.Jmp.Args {
				live.AddRef(a) // extra live-out registers (multi-register return)
			}
			for k := len(b.Instrs) - 1; k >= 0; k-- {
				in := &b.Instrs[k]
				// A lifetime marker names an alloca address but does not use its value —
				// it only bounds the slot's live range for mem2reg and the frame
				// allocator. Counting it as a use would extend the address temp's live
				// range, the opposite of the marker's purpose, so skip it entirely.
				if in.Op.IsLifetime() {
					continue
				}
				if in.To.IsTemp() {
					live.Remove(int(in.To.ID)) // def kills
				}
				for _, d := range in.Defs() {
					if d.IsTemp() {
						live.Remove(int(d.ID)) // extra call-defined registers
					}
				}
				for _, a := range in.Uses() {
					live.AddRef(a) // use
				}
			}
			for _, p := range b.Phis {
				if p.To.IsTemp() {
					live.Remove(int(p.To.ID)) // phi result defined at block top
				}
			}

			if l.In[b].Union(live) {
				changed = true
			}
		}
	}
	return l
}

// LiveIn returns the set of temporaries live on entry to b.
func (l *Liveness) LiveIn(b *ir.Block) *TempSet { return l.In[b] }

// LiveOut returns the set of temporaries live on exit from b.
func (l *Liveness) LiveOut(b *ir.Block) *TempSet { return l.Out[b] }
