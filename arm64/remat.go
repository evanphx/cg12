package arm64

import "github.com/evanphx/cg12/ir"

// Rematerialization (GCC's lra-remat): a value whose definition is a cheap, pure,
// operand-free computation is recomputed at each use instead of spilled and reloaded,
// and needs no stack slot. This attacks the register allocator's costliest spills --
// an interpreter keeps a stack-slot address for every local alive across its whole
// dispatch loop; recomputing `x29 + offset` at each use is cheaper than a load and
// removes the value from the pressure entirely.

type rematKind uint8

const (
	rematNone   rematKind = iota
	rematAlloca           // add r, x29, #allocOff  -- a stack-slot address
	rematConst            // movz/movk               -- a small integer constant
	rematSym              // adrp + add              -- a (non-thread) symbol address
)

type rematRule struct {
	kind rematKind
	in   *ir.Instr // rematAlloca: the OAlloc, for its frame offset
	c    ir.Const  // rematConst / rematSym
}

// rematRules finds every temp that can be recomputed at any use: defined exactly once
// by an OAlloc, or by a copy of a small integer constant (≤2 movz/movk) or a
// non-thread symbol address. A GC ref (its stack-map location must be a real slot), a
// pre-coloured temp, or a temp passed to a call (its value flows through a parallel
// move, which cannot recompute) is never rematerialized.
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
			// Only instructions that resolve their operands through the selection
			// driver's src/reload path can consume a rematerialised value (it is rebuilt
			// there). An operand of a call, parameter, blit, or by-value aggregate flows
			// through a parallel move / memcpy / frame-address path that reads a spill
			// slot directly and cannot recompute, so those operands are not remat-safe.
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
				case c.Kind == ir.ConstInt && movImmChunks(c.Int) <= 2:
					rules[t] = rematRule{kind: rematConst, c: c}
				case c.Kind == ir.ConstSym && !c.Thread && c.Int >= 0 && c.Int <= 4095:
					rules[t] = rematRule{kind: rematSym, c: c}
				}
			}
		}
	}
	return rules
}

// refreshRematAllocas re-points each rematerialised alloca-address rule at the current
// OAlloc instruction defining its temp, after a pass (insertCallerSaves) has rebuilt
// block instruction slices and moved those instructions.
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

// srcResolvesOperands reports whether the emitter resolves in's temp operands through
// the selection driver's src path (which rebuilds a rematerialised value), rather than
// a parallel move, memcpy, or frame-address path that reads a spill slot directly.
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

// movImmChunks is how many movz/movk instructions materialising val takes: the number
// of 16-bit halfwords differing from the cheaper fill (0 for movz, 0xffff for movn).
func movImmChunks(val int64) int {
	u := uint64(val)
	zeros, ones := 0, 0
	for i := 0; i < 4; i++ {
		switch (u >> uint(16*i)) & 0xffff {
		case 0:
			zeros++
		case 0xffff:
			ones++
		}
	}
	n := 4 - zeros
	if 4-ones < n {
		n = 4 - ones
	}
	if n == 0 {
		n = 1 // all-zero / all-ones still needs one instruction
	}
	return n
}
