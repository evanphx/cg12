package wasm

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// structurer restructures an arbitrary (reducible) CFG into WebAssembly's
// block/loop/if control constructs, following the dominator-tree recursive
// translation of Ramsey's "Beyond Relooper". Every reachable block is emitted
// exactly once:
//
//   - a loop header (target of a back edge) is wrapped in a `loop`; branching to
//     it emits `br $L<id>` (a continue);
//   - a merge node (>=2 forward predecessors) is wrapped in a `block` placed at
//     its immediate dominator; branching to it emits `br $B<id>` (a break);
//   - any other block has a single forward predecessor and is emitted inline at
//     that predecessor's branch.
//
// Because every construct carries a unique name and the wrapping is anchored at
// the target's immediate dominator, every emitted `br` is guaranteed in scope.
type structurer struct {
	e       *emitter
	cfg     *analysis.CFG
	dom     *analysis.DomTree
	isMerge map[*ir.Block]bool
	isLoop  map[*ir.Block]bool
	// mergeKids maps a block to its dominator-tree children that are merge
	// nodes, sorted by ascending reverse-postorder index.
	mergeKids map[*ir.Block][]*ir.Block
}

// structure analyses f and emits its whole body as structured control flow.
func (e *emitter) structure() error {
	f := e.f
	cfg := analysis.BuildCFG(f)
	dom := cfg.Dominators()

	s := &structurer{
		e:         e,
		cfg:       cfg,
		dom:       dom,
		isMerge:   map[*ir.Block]bool{},
		isLoop:    map[*ir.Block]bool{},
		mergeKids: map[*ir.Block][]*ir.Block{},
	}
	if err := s.classify(); err != nil {
		return err
	}
	s.doTree(f.Start)
	return e.err
}

// backEdge reports whether the edge a->b is a back edge, i.e. b dominates a.
func (s *structurer) backEdge(a, b *ir.Block) bool { return s.dom.Dominates(b, a) }

// classify computes the merge/loop-header sets and the per-dominator list of
// merge children, and rejects irreducible control flow (which the dominator
// translation cannot handle without node splitting).
func (s *structurer) classify() error {
	for _, b := range s.cfg.RPO {
		forward := 0
		for _, p := range b.Preds {
			if !s.cfg.Reachable(p) {
				continue
			}
			if s.backEdge(p, b) {
				s.isLoop[b] = true
			} else {
				forward++
			}
		}
		if forward >= 2 {
			s.isMerge[b] = true
		}
	}

	// Reject irreducibility: a retreating edge whose target does not dominate
	// its source means the CFG is not reducible.
	for _, a := range s.cfg.RPO {
		for _, b := range a.Succs() {
			if b == nil || !s.cfg.Reachable(b) {
				continue
			}
			if s.cfg.Num(b) <= s.cfg.Num(a) && !s.dom.Dominates(b, a) {
				return fmt.Errorf("wasm: irreducible control flow at block %s", b.Name)
			}
		}
	}

	// Group merge nodes under their immediate dominator, ordered by RPO.
	for _, b := range s.cfg.RPO {
		if !s.isMerge[b] {
			continue
		}
		d := s.dom.Idom[b]
		s.mergeKids[d] = append(s.mergeKids[d], b)
	}
	for d, kids := range s.mergeKids {
		sort.Slice(kids, func(i, j int) bool {
			return s.cfg.Num(kids[i]) < s.cfg.Num(kids[j])
		})
		s.mergeKids[d] = kids
	}
	return nil
}

// label returns a per-block identifier that is unique among reachable blocks.
// It uses the reverse-postorder index rather than Block.ID, which the text
// parser leaves colliding (its forward-referenced blocks share id 0) and which
// would otherwise produce two constructs with the same name.
func (s *structurer) label(b *ir.Block) int { return s.cfg.Num(b) }

// doTree emits x and, if x is a loop header, wraps its code in a loop.
func (s *structurer) doTree(x *ir.Block) {
	if s.isLoop[x] {
		s.e.line("loop $L%d", s.label(x))
		s.e.depth++
		s.codeForX(x)
		s.e.depth--
		s.e.line("end")
		return
	}
	s.codeForX(x)
}

// codeForX emits x's straight-line body and terminator, wrapping a `block` for
// each of x's merge children so forward branches to them are legal.
func (s *structurer) codeForX(x *ir.Block) {
	s.nodeWithin(x, s.mergeKids[x])
}

// nodeWithin peels the highest-RPO merge child, wraps the remaining code in a
// block whose label reaches it, then emits that child's subtree afterward.
func (s *structurer) nodeWithin(x *ir.Block, kids []*ir.Block) {
	if len(kids) == 0 {
		s.emitBody(x)
		s.emitTerminator(x)
		return
	}
	last := len(kids) - 1
	y := kids[last]
	s.e.line("block $B%d", s.label(y))
	s.e.depth++
	s.nodeWithin(x, kids[:last])
	s.e.depth--
	s.e.line("end")
	s.doTree(y)
}

// emitBody emits x's non-phi instructions. A terminating tail call is emitted by
// emitTerminator instead (as return_call), so it is skipped here.
func (s *structurer) emitBody(x *ir.Block) {
	n := len(x.Instrs)
	if _, ok := ir.TailCall(x); ok {
		n--
	}
	for i := 0; i < n; i++ {
		s.e.instr(&x.Instrs[i])
	}
}

// emitTerminator emits x's control transfer.
func (s *structurer) emitTerminator(x *ir.Block) {
	switch x.Jmp.Kind {
	case ir.JmpRet:
		if call, ok := ir.TailCall(x); ok {
			s.e.emitTailCall(call)
			return
		}
		s.e.emitReturn(x.Jmp.Arg)
	case ir.JmpHlt:
		s.e.line("unreachable")
	case ir.JmpJmp:
		s.doBranch(x, x.Jmp.To)
	case ir.JmpJnz:
		s.e.push(x.Jmp.Arg)
		s.e.line("if")
		s.e.depth++
		s.doBranch(x, x.Jmp.To)
		s.e.depth--
		s.e.line("else")
		s.e.depth++
		s.doBranch(x, x.Jmp.To2)
		s.e.depth--
		s.e.line("end")
	case ir.JmpNone:
		// Unterminated block under construction: nothing to emit.
	default:
		s.e.fail("wasm: unhandled terminator %d", x.Jmp.Kind)
	}
}

// doBranch realises the edge from -> to: first the phi copies for this edge,
// then either a br to the target's construct or an inline emission of a
// singly-reached target.
func (s *structurer) doBranch(from, to *ir.Block) {
	s.emitPhiMoves(from, to)
	switch {
	case s.backEdge(from, to):
		s.e.line("br $L%d", s.label(to))
	case s.isMerge[to]:
		s.e.line("br $B%d", s.label(to))
	default:
		s.doTree(to)
	}
}

// emitPhiMoves assigns, in parallel, every phi of `to` the value it selects for
// the edge from `from`. Pushing all sources before writing any destination
// gives correct parallel-copy semantics even for swaps and cycles.
func (s *structurer) emitPhiMoves(from, to *ir.Block) {
	var dsts []ir.Ref
	for _, phi := range to.Phis {
		src, ok := phiArg(phi, from)
		if !ok {
			continue
		}
		s.e.push(src)
		dsts = append(dsts, phi.To)
	}
	for i := len(dsts) - 1; i >= 0; i-- {
		s.e.line("local.set %s", tempName(s.e.f.Temps[dsts[i].ID]))
	}
}

// phiArg returns the phi's incoming value for predecessor `from`.
func phiArg(phi *ir.Phi, from *ir.Block) (ir.Ref, bool) {
	for k, pb := range phi.Blocks {
		if pb == from {
			return phi.Args[k], true
		}
	}
	return ir.R, false
}
