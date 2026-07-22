package amd64

import "github.com/evanphx/cg12/ir"

// Rematerialization (GCC's lra-remat): a value whose definition is a cheap, pure,
// operand-free computation is recomputed at each use instead of spilled and
// reloaded, and needs no stack slot. On x86 the recomputation is a single
// instruction -- an immediate mov, a RIP-relative lea to a symbol, or an lea of a
// frame slot address -- so a spilled such value is always cheaper recomputed. It
// attacks the register allocator's costliest spills: an alloca address kept alive
// across a whole function is `lea rbp+off` at each use rather than a store/reload.

type rematKind uint8

const (
	rematNone   rematKind = iota
	rematAlloca           // lea r, [rbp - allocOff]  -- a stack-slot address
	rematConst            // mov r, imm               -- an integer constant
	rematSym              // lea r, [rip + sym]       -- a (non-thread) symbol address
)

type rematRule struct {
	kind rematKind
	in   *ir.Instr // rematAlloca: the OAlloc, for its frame offset
	c    ir.Const  // rematConst / rematSym
}

// rematRules finds every temp that can be recomputed at any use: defined exactly
// once by an OAlloc, or by a copy of an integer constant or a non-thread symbol
// address. A GC ref (its stack-map location must be a real slot), a pre-coloured
// temp, or a temp passed to a call / aggregate path (its value flows through a
// parallel move or memcpy that reads a spill slot directly, which cannot
// recompute) is never rematerialized.
func rematRules(f *ir.Func) map[int]rematRule {
	n := len(f.Temps)
	defCount := make([]int, n)
	defInstr := make([]*ir.Instr, n)
	unsafe := make([]bool, n)
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			for _, d := range instrDefs(in) {
				defCount[d]++
				defInstr[d] = in
			}
			if !srcResolvesOperands(in) {
				for _, a := range in.Uses() {
					if a.Kind == ir.RefTemp {
						unsafe[a.ID] = true
					}
				}
			}
		}
		if b.Jmp.Arg.Kind == ir.RefTemp {
			unsafe[b.Jmp.Arg.ID] = true
		}
		for _, a := range b.Jmp.Args {
			if a.Kind == ir.RefTemp {
				unsafe[a.ID] = true
			}
		}
	}

	rules := map[int]rematRule{}
	for t := 0; t < n; t++ {
		tt := f.Temps[t]
		if tt == nil || tt.Fixed || tt.GCRef || unsafe[t] || defCount[t] != 1 {
			continue
		}
		in := defInstr[t]
		switch in.Op {
		case ir.OAlloc4, ir.OAlloc8, ir.OAlloc16:
			rules[t] = rematRule{kind: rematAlloca, in: in}
		case ir.OCopy:
			if len(in.Args) == 1 && in.Args[0].Kind == ir.RefConst {
				c := f.Consts[in.Args[0].ID]
				switch {
				case c.Kind == ir.ConstInt:
					rules[t] = rematRule{kind: rematConst, c: c}
				case c.Kind == ir.ConstSym && !c.Thread:
					rules[t] = rematRule{kind: rematSym, c: c}
				}
			}
		}
	}
	return rules
}

// refreshRematAllocas re-points each rematerialised alloca-address rule at the
// current OAlloc instruction defining its temp, after a pass (insertCallerSaves)
// has rebuilt block instruction slices and moved those instructions.
func refreshRematAllocas(f *ir.Func, remat map[int]rematRule) {
	if remat == nil {
		return
	}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.To.Kind != ir.RefTemp {
				continue
			}
			if rule, ok := remat[int(in.To.ID)]; ok && rule.kind == rematAlloca {
				rule.in = in
				remat[int(in.To.ID)] = rule
			}
		}
	}
}

// srcResolvesOperands reports whether the emitter resolves in's temp operands
// through the refLoc/move path (which rebuilds a rematerialised value), rather
// than a parallel move, memcpy, or frame-address path that reads a spill slot
// directly.
func srcResolvesOperands(in *ir.Instr) bool {
	if in.AggArgs != nil || in.RetAgg != nil {
		return false
	}
	switch in.Op {
	case ir.OArg, ir.OPar, ir.OCall, ir.OBlit, ir.OVaStart, ir.OVaArg:
		return false
	}
	return true
}
