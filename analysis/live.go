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

	for changed := true; changed; {
		changed = false
		// Process near-exit blocks first so successors' live-in is fresh.
		for i := len(c.RPO) - 1; i >= 0; i-- {
			b := c.RPO[i]

			out := NewTempSet(nt)
			// Successors of a reachable block are reachable, hence in l.In.
			for _, s := range b.Succs() {
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
			l.Out[b] = out

			live := out.Copy()
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
				for _, d := range in.Defs {
					if d.IsTemp() {
						live.Remove(int(d.ID)) // extra call-defined registers
					}
				}
				for _, a := range in.Args {
					live.AddRef(a) // use
				}
			}
			for _, p := range b.Phis {
				if p.To.IsTemp() {
					live.Remove(int(p.To.ID)) // phi result defined at block top
				}
			}

			if !live.Equal(l.In[b]) {
				l.In[b] = live
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
