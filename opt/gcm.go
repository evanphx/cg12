package opt

import (
	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// GCM performs Cliff Click's Global Code Motion: pure computations are unpinned
// and rescheduled to the block on the dominator path between their earliest
// legal position (dominated by all inputs) and latest legal position (the
// lowest common ancestor of all uses) that has the smallest loop-nesting depth.
// This hoists loop-invariant code out of loops and sinks code toward its uses.
//
// Only side-effect-free instructions move; phis, loads, stores, calls, allocs,
// and terminators stay pinned, so memory ordering is preserved.
func GCM(f *ir.Func) bool {
	cfg := analysis.BuildCFG(f)
	if len(cfg.RPO) == 0 {
		return false
	}
	dom := cfg.Dominators()
	idom := dom.Idom
	domDepth := computeDomDepth(cfg, dom)
	loopDepth := cfg.LoopDepth(dom)

	sched := &gcmScheduler{
		f: f, cfg: cfg, idom: idom, domDepth: domDepth, loopDepth: loopDepth,
		movInstr: map[uint32]ir.Instr{}, origBlock: map[uint32]*ir.Block{},
		fixedBlock: map[uint32]*ir.Block{}, early: map[uint32]*ir.Block{},
		selected: map[uint32]*ir.Block{}, uses: map[uint32][]useSite{},
	}
	return sched.run()
}

type useSite struct {
	fixed  *ir.Block // block of a pinned/phi/terminator use
	userID uint32    // id of a movable user
	isUser bool
}

type gcmScheduler struct {
	f         *ir.Func
	cfg       *analysis.CFG
	idom      map[*ir.Block]*ir.Block
	domDepth  map[*ir.Block]int
	loopDepth map[*ir.Block]int

	movInstr   map[uint32]ir.Instr
	origBlock  map[uint32]*ir.Block
	fixedBlock map[uint32]*ir.Block
	movIDs     []uint32

	early    map[uint32]*ir.Block
	selected map[uint32]*ir.Block
	uses     map[uint32][]useSite
}

func (s *gcmScheduler) isMov(id uint32) bool {
	_, ok := s.movInstr[id]
	return ok
}

func (s *gcmScheduler) run() bool {
	s.classify()
	if len(s.movIDs) == 0 {
		return false
	}
	s.unpin()
	for _, id := range s.movIDs {
		s.scheduleEarly(id)
	}
	s.collectUses()
	for _, id := range s.movIDs {
		s.scheduleLate(id)
	}
	return s.place()
}

// classify records where every value is defined and which are movable.
func (s *gcmScheduler) classify() {
	for _, p := range s.f.Params {
		s.fixedBlock[uint32(p.ID)] = s.f.Start
	}
	for _, b := range s.cfg.RPO {
		for _, phi := range b.Phis {
			s.fixedBlock[phi.To.ID] = b
		}
		for _, in := range b.Instrs {
			if in.To.Kind != ir.RefTemp {
				continue
			}
			if movable(&in) {
				s.movInstr[in.To.ID] = in
				s.origBlock[in.To.ID] = b
				s.movIDs = append(s.movIDs, in.To.ID)
			} else {
				s.fixedBlock[in.To.ID] = b
			}
		}
	}
}

// unpin removes all movable instructions from their blocks.
func (s *gcmScheduler) unpin() {
	for _, b := range s.cfg.RPO {
		out := b.Instrs[:0]
		for _, in := range b.Instrs {
			if in.To.Kind == ir.RefTemp && movable(&in) {
				continue
			}
			out = append(out, in)
		}
		b.Instrs = out
	}
}

func (s *gcmScheduler) defBlock(r ir.Ref) *ir.Block {
	switch r.Kind {
	case ir.RefConst:
		return s.f.Start
	case ir.RefTemp:
		if b, ok := s.fixedBlock[r.ID]; ok {
			return b
		}
		if s.isMov(r.ID) {
			return s.scheduleEarly(r.ID)
		}
	}
	return s.f.Start
}

// scheduleEarly places a value in the deepest (in the dominator tree) block
// among its inputs' definition blocks: the earliest legal position.
func (s *gcmScheduler) scheduleEarly(id uint32) *ir.Block {
	if e, ok := s.early[id]; ok {
		return e
	}
	s.early[id] = s.f.Start // guard against cycles (none in SSA)
	e := s.f.Start
	for _, a := range s.movInstr[id].Args {
		if db := s.defBlock(a); s.domDepth[db] > s.domDepth[e] {
			e = db
		}
	}
	s.early[id] = e
	return e
}

// collectUses builds, for every movable value, the list of places it is used.
func (s *gcmScheduler) collectUses() {
	addFixed := func(id uint32, b *ir.Block) {
		if s.isMov(id) {
			s.uses[id] = append(s.uses[id], useSite{fixed: b})
		}
	}
	for _, b := range s.cfg.RPO {
		for _, phi := range b.Phis {
			for k, a := range phi.Args {
				if a.Kind == ir.RefTemp {
					addFixed(a.ID, phi.Blocks[k]) // phi operand used on its edge
				}
			}
		}
		for _, in := range b.Instrs { // pinned instructions only (movables removed)
			for _, a := range in.Args {
				if a.Kind == ir.RefTemp {
					addFixed(a.ID, b)
				}
			}
		}
		if b.Jmp.Arg.Kind == ir.RefTemp {
			addFixed(b.Jmp.Arg.ID, b)
		}
	}
	for _, uid := range s.movIDs {
		for _, a := range s.movInstr[uid].Args {
			if a.Kind == ir.RefTemp && s.isMov(a.ID) {
				s.uses[a.ID] = append(s.uses[a.ID], useSite{userID: uid, isUser: true})
			}
		}
	}
}

// scheduleLate picks the final block: on the dominator path from the earliest
// position down to the latest (LCA of uses), the block with the least loop
// depth, preferring the latest such block.
func (s *gcmScheduler) scheduleLate(id uint32) *ir.Block {
	if sel, ok := s.selected[id]; ok {
		return sel
	}
	s.selected[id] = s.early[id] // cycle guard

	var lca *ir.Block
	for _, u := range s.uses[id] {
		ub := u.fixed
		if u.isUser {
			ub = s.scheduleLate(u.userID)
		}
		lca = s.lca(lca, ub)
	}

	e := s.early[id]
	best := lca
	if lca == nil {
		best = e // dead value: park at its earliest position (DCE will remove it)
	} else {
		for cur := lca; cur != e; {
			cur = s.idom[cur]
			if cur == nil {
				break
			}
			if s.loopDepth[cur] < s.loopDepth[best] {
				best = cur
			}
			if cur == e {
				break
			}
		}
	}
	s.selected[id] = best
	return best
}

// place reinserts each movable instruction into its selected block, dropping
// dead ones, and reports whether anything actually moved.
func (s *gcmScheduler) place() bool {
	toPlace := map[*ir.Block][]ir.Instr{}
	moved := false
	for _, id := range s.movIDs {
		if len(s.uses[id]) == 0 {
			moved = true // dead: dropped
			continue
		}
		blk := s.selected[id]
		toPlace[blk] = append(toPlace[blk], s.movInstr[id])
		if blk != s.origBlock[id] {
			moved = true
		}
	}
	for blk, instrs := range toPlace {
		blk.Instrs = scheduleBlock(blk.Instrs, instrs)
	}
	return moved
}

// lca returns the lowest common ancestor of two blocks in the dominator tree.
func (s *gcmScheduler) lca(a, b *ir.Block) *ir.Block {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	for s.domDepth[a] > s.domDepth[b] {
		a = s.idom[a]
	}
	for s.domDepth[b] > s.domDepth[a] {
		b = s.idom[b]
	}
	for a != b {
		a = s.idom[a]
		b = s.idom[b]
	}
	return a
}

func computeDomDepth(cfg *analysis.CFG, dom *analysis.DomTree) map[*ir.Block]int {
	depth := map[*ir.Block]int{}
	for _, b := range cfg.RPO { // RPO visits a dominator before the blocks it dominates
		if id := dom.Idom[b]; id != nil {
			depth[b] = depth[id] + 1
		}
	}
	return depth
}

// scheduleBlock merges pinned and freshly-placed movable instructions into one
// data-dependency-respecting order, preserving the relative order of the pinned
// instructions (so memory operations keep their sequence).
func scheduleBlock(pinned, movable []ir.Instr) []ir.Instr {
	items := make([]ir.Instr, 0, len(pinned)+len(movable))
	items = append(items, pinned...)
	items = append(items, movable...)
	n := len(items)

	defIndex := map[uint32]int{}
	for i, it := range items {
		if it.To.Kind == ir.RefTemp {
			defIndex[it.To.ID] = i
		}
	}
	adj := make([][]int, n)
	indeg := make([]int, n)
	addEdge := func(from, to int) {
		adj[from] = append(adj[from], to)
		indeg[to]++
	}
	for i, it := range items {
		for _, a := range it.Args {
			if a.Kind == ir.RefTemp {
				if j, ok := defIndex[a.ID]; ok && j != i {
					addEdge(j, i)
				}
			}
		}
	}
	for k := 0; k+1 < len(pinned); k++ { // keep pinned instructions in order
		addEdge(k, k+1)
	}

	result := make([]ir.Instr, 0, n)
	placed := make([]bool, n)
	for len(result) < n {
		pick := -1
		for i := 0; i < n; i++ {
			if !placed[i] && indeg[i] == 0 {
				pick = i
				break
			}
		}
		if pick == -1 { // unreachable in acyclic SSA; emit the rest defensively
			for i := 0; i < n; i++ {
				if !placed[i] {
					result = append(result, items[i])
					placed[i] = true
				}
			}
			break
		}
		placed[pick] = true
		result = append(result, items[pick])
		for _, to := range adj[pick] {
			indeg[to]--
		}
	}
	return result
}
