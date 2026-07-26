package opt

import (
	"os"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// If-conversion turns a short branch-and-merge -- a c?a:b diamond, or the
// triangle an `if` without an else leaves -- into a straight run of speculated
// instructions feeding an OSel, which the arm64 backend fuses to `cmp; csel`
// (see fuseCmpSel). Removing the branch shrinks the function (the point, for
// inline density) and drops an unpredictable branch.
//
// It is deliberately conservative. csel evaluates BOTH arms, so it is a win only
// when the arms are cheap and side-effect-free; converting everything regresses
// size (a naive earlier attempt grew 107 of 126 functions). Two guards enforce
// that: a default-deny speculation allowlist (an arm may hold only pure,
// non-faulting instructions) and a size-primary profitability gate.

// Profitability bounds. k is the total speculated instructions across both arms;
// p is the phis at the join (one csel each). The gate admits p==1 with k<=3, or a
// pure phi-diamond (p==2, k==0) -- past that the added csels plus the
// unconditionally-run arm work stop paying for the removed branch. Starting
// points, tuned by measurement.
const (
	ifConvMaxSpecInstrs = 3
	ifConvMaxSelects    = 2
	// A branch head this much hotter than function entry is left as a branch: a
	// csel evaluates both arms every iteration, so converting a hot, usually
	// well-predicted branch is a measured interpreter slowdown. Cold-code
	// conversions -- the ones that shrink helpers for inlining -- run rarely, so
	// their extra arm work costs nothing while the size win stands.
	ifConvHotFreq = 25.0
)

// speculatable reports whether an op may be executed unconditionally -- it
// neither faults nor has a side effect, so hoisting it out of its arm and
// discarding the result on the not-taken path changes nothing observable. The
// list is closed: loads/stores (may fault), calls, division and remainder (trap
// on zero), allocations, atomics, inline asm, register and safepoint ops, and
// anything else default to non-speculatable.
func speculatable(op ir.Op) bool {
	switch op {
	case ir.ONop,
		ir.OAdd, ir.OSub, ir.OMul, ir.OUMulh, ir.OSMulh,
		ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar,
		ir.ONeg, ir.OClz, ir.OCmp, ir.OSel, ir.OCopy,
		ir.ORotr, ir.OBic,
		ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw,
		ir.OExts, ir.OTruncd, ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof,
		ir.OCast:
		return true
	}
	return false
}

// IfConvert converts qualifying diamonds and triangles in f to selects, leaving
// the drained arm blocks unreachable for the following SimplifyCFG/DCE to
// collect. It reports whether it changed anything. CG12_NO_IFCONVERT disables it
// for A/B measurement.
func IfConvert(f *ir.Func) bool {
	if os.Getenv("CG12_NO_IFCONVERT") != "" {
		return false
	}
	cfg := analysis.BuildCFG(f)
	if len(cfg.RPO) == 0 {
		return false
	}
	// Block frequencies gate out hot branches (see ifConvHotFreq). Computed once
	// on the entry CFG: conversion only drains blocks and repoints heads, so the
	// original heads' estimates stay valid.
	freq := cfg.Frequency(cfg.Dominators())
	changed := false
	// Convert one region per round, recomputing predecessors each time: a
	// conversion drains arm blocks and repoints their head, so stale edges must
	// not be reused. A converted inner diamond turns its enclosing arm into a
	// straight speculatable run, so an outer diamond becomes convertible on a
	// later round. Bounded by the block count (each round removes a branch).
	for range f.Blocks {
		preds := predMap(f)
		did := false
		for _, b := range f.Blocks {
			if convertRegion(f, b, preds, freq) {
				did, changed = true, true
				break
			}
		}
		if !did {
			break
		}
	}
	return changed
}

// predMap returns each block's predecessors, derived from the terminators.
func predMap(f *ir.Func) map[*ir.Block][]*ir.Block {
	preds := map[*ir.Block][]*ir.Block{}
	for _, b := range f.Blocks {
		for _, s := range b.Succs() {
			if s != nil {
				preds[s] = append(preds[s], b)
			}
		}
	}
	return preds
}

// classifyArm inspects a successor X of the branch head H. It returns either a
// speculatable arm block (single-predecessor H, no phis, a plain jump onward,
// every instruction speculatable) together with the join it reaches, or -- when
// X is not exclusively H's arm -- treats the edge as going straight to the join
// X itself (the direct side of a triangle). ok is false only when X is H's
// exclusive arm but cannot be speculated, which makes the whole region
// non-convertible.
func classifyArm(H, X *ir.Block, preds map[*ir.Block][]*ir.Block) (arm, join *ir.Block, ok bool) {
	if p := preds[X]; len(p) == 1 && p[0] == H {
		if len(X.Phis) == 0 && X.Jmp.Kind == ir.JmpJmp && X.Jmp.To != nil && allSpeculatable(X) {
			return X, X.Jmp.To, true
		}
		return nil, nil, false
	}
	return nil, X, true
}

// allSpeculatable reports whether every instruction in b may be speculated.
func allSpeculatable(b *ir.Block) bool {
	for i := range b.Instrs {
		if !speculatable(b.Instrs[i].Op) {
			return false
		}
	}
	return true
}

// convertRegion converts the diamond or triangle headed by H, if one qualifies.
func convertRegion(f *ir.Func, H *ir.Block, preds map[*ir.Block][]*ir.Block, freq *analysis.Freq) bool {
	if H.Jmp.Kind != ir.JmpJnz {
		return false
	}
	if freq.Of(H) > ifConvHotFreq { // hot branch: keep it a branch (see ifConvHotFreq)
		return false
	}
	tb, fb := H.Jmp.To, H.Jmp.To2
	if tb == nil || fb == nil || tb == fb {
		return false
	}
	tArm, tJoin, tok := classifyArm(H, tb, preds)
	fArm, fJoin, fok := classifyArm(H, fb, preds)
	if !tok || !fok {
		return false
	}
	if tArm == nil && fArm == nil { // no speculatable arm on either side
		return false
	}
	J := tJoin
	if fJoin != J || J == H || J == tArm || J == fArm || len(J.Phis) == 0 {
		return false
	}

	k := countInstrs(tArm) + countInstrs(fArm)
	p := len(J.Phis)
	if !profitable(k, p) {
		return false
	}

	// The join predecessor carrying each side's value: the arm block if the side
	// is an arm, otherwise H itself (the direct edge of a triangle).
	truePred, falsePred := H, H
	if tArm != nil {
		truePred = tArm
	}
	if fArm != nil {
		falsePred = fArm
	}

	// Validate every phi has both incoming edges before mutating anything.
	for _, phi := range J.Phis {
		if indexOf(phi.Blocks, truePred) < 0 || indexOf(phi.Blocks, falsePred) < 0 {
			return false
		}
	}

	cond := H.Jmp.Arg
	// Hoist both arms into H first, so the values the selects choose between are
	// defined before the selects that reference them.
	if tArm != nil {
		H.Instrs = append(H.Instrs, tArm.Instrs...)
	}
	if fArm != nil {
		H.Instrs = append(H.Instrs, fArm.Instrs...)
	}
	for _, phi := range J.Phis {
		ti := indexOf(phi.Blocks, truePred)
		fi := indexOf(phi.Blocks, falsePred)
		tVal, fVal := phi.Args[ti], phi.Args[fi] // OSel yields tVal when cond != 0
		sel := f.NewTemp("", phi.Cls)
		H.Instrs = append(H.Instrs, ir.Instr{Op: ir.OSel, Cls: phi.Cls, To: sel, Args: []ir.Ref{cond, tVal, fVal}})
		// Replace both side edges with a single edge from H carrying the select.
		na, nb := phi.Args[:0], phi.Blocks[:0]
		for i, blk := range phi.Blocks {
			if blk == truePred || blk == falsePred {
				continue
			}
			na = append(na, phi.Args[i])
			nb = append(nb, blk)
		}
		phi.Args = append(na, sel)
		phi.Blocks = append(nb, H)
	}
	H.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: J}
	if tArm != nil {
		drainBlock(tArm)
	}
	if fArm != nil {
		drainBlock(fArm)
	}
	return true
}

// profitable is the size-primary gate over the speculated-instruction count k
// and the select count p.
func profitable(k, p int) bool {
	if p < 1 || p > ifConvMaxSelects || k > ifConvMaxSpecInstrs {
		return false
	}
	if p == ifConvMaxSelects && k > 0 { // two selects only pay for a pure phi-merge
		return false
	}
	return true
}

// countInstrs counts a (possibly nil) arm's non-nop instructions.
func countInstrs(b *ir.Block) int {
	if b == nil {
		return 0
	}
	n := 0
	for i := range b.Instrs {
		if b.Instrs[i].Op != ir.ONop {
			n++
		}
	}
	return n
}

// drainBlock severs a hoisted arm: its instructions now live in the head, so it
// is left empty and unreachable (no successors) for SimplifyCFG to delete.
func drainBlock(b *ir.Block) {
	b.Instrs = nil
	b.Phis = nil
	b.Jmp = ir.Jmp{Kind: ir.JmpHlt}
}

// indexOf returns the position of blk in bs, or -1.
func indexOf(bs []*ir.Block, blk *ir.Block) int {
	for i, b := range bs {
		if b == blk {
			return i
		}
	}
	return -1
}
