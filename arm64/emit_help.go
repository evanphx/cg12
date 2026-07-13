package arm64

import (
	"strings"

	"github.com/evanphx/cg12/ir"
)

type movePairLoc struct{ dst, src loc }

func sameLoc(a, b loc) bool {
	if a.reg == b.reg && !a.mem && !b.mem && !a.imm && !b.imm {
		return true
	}
	if a.mem && b.mem && a.slot == b.slot {
		return true
	}
	return false
}

// srcReadsDst reports whether reading src touches the destination location dst.
func srcReadsDst(src, dst loc) bool {
	if src.imm {
		return false
	}
	if !src.mem && !dst.mem {
		return src.reg == dst.reg
	}
	if src.mem && dst.mem {
		return src.slot == dst.slot
	}
	return false
}

// parallelMove emits a set of simultaneous moves, ordering them so no source is
// clobbered before it is read and breaking register cycles with scratch2.
func (e *emitter) parallelMove(pairs []movePairLoc) {
	var work []movePairLoc
	for _, p := range pairs {
		if !sameLoc(p.dst, p.src) {
			work = append(work, p)
		}
	}
	for len(work) > 0 {
		idx := -1
		for i, p := range work {
			blocked := false
			for j, q := range work {
				if i != j && srcReadsDst(q.src, p.dst) {
					blocked = true
					break
				}
			}
			if !blocked {
				idx = i
				break
			}
		}
		if idx >= 0 {
			e.emitMoveLoc(work[idx].dst, work[idx].src)
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		// Cyclic: rescue a register destination into scratch2 and reroute.
		ci := -1
		for i, p := range work {
			if !p.dst.mem {
				ci = i
				break
			}
		}
		if ci < 0 {
			e.fail("arm64: unexpected memory cycle in parallel move")
			return
		}
		saved := work[ci].dst
		rescue := scratch2
		if saved.reg.IsFloat() {
			rescue = fscratch0
		}
		tmp := loc{reg: rescue, size: saved.size}
		e.emitMoveLoc(tmp, saved)
		for i := range work {
			if srcReadsDst(work[i].src, saved) && !work[i].src.mem {
				work[i].src = tmp
			}
		}
	}
}

// emitMoveLoc emits a single move between two locations.
func (e *emitter) emitMoveLoc(dst, src loc) {
	size := dst.size
	switch {
	case !dst.mem:
		switch {
		case src.imm:
			if dst.reg.IsFloat() {
				// The immediate holds the value's bit pattern; move it via GPR.
				e.movImm(scratch0, src.val, size)
				e.line("fmov %s, %s", dst.reg.Name(size), scratch0.Name(size))
			} else {
				e.movImm(dst.reg, src.val, size)
			}
		case src.mem:
			e.line("ldr %s, [x29, #%d]", dst.reg.Name(size), e.spillBase+src.slot)
		case dst.reg.IsFloat():
			e.line("fmov %s, %s", dst.reg.Name(size), src.reg.Name(size))
		default:
			e.line("mov %s, %s", dst.reg.Name(size), src.reg.Name(size))
		}
	default: // dst is a spill slot
		switch {
		case src.imm:
			e.movImm(scratch0, src.val, size)
			e.line("str %s, [x29, #%d]", scratch0.Name(size), e.spillBase+dst.slot)
		case src.mem:
			e.line("ldr %s, [x29, #%d]", scratch0.Name(size), e.spillBase+src.slot)
			e.line("str %s, [x29, #%d]", scratch0.Name(size), e.spillBase+dst.slot)
		default:
			e.line("str %s, [x29, #%d]", src.reg.Name(size), e.spillBase+dst.slot)
		}
	}
}

// condCode maps an integer comparison predicate to an AArch64 condition.
func condCode(c ir.Cmp) (string, bool) {
	switch c {
	case ir.CmpEq:
		return "eq", true
	case ir.CmpNe:
		return "ne", true
	case ir.CmpSlt:
		return "lt", true
	case ir.CmpSle:
		return "le", true
	case ir.CmpSgt:
		return "gt", true
	case ir.CmpSge:
		return "ge", true
	case ir.CmpUlt:
		return "lo", true
	case ir.CmpUle:
		return "ls", true
	case ir.CmpUgt:
		return "hi", true
	case ir.CmpUge:
		return "hs", true
	}
	return "", false
}

// fpCondCode maps a floating-point comparison predicate to an AArch64 condition
// interpreting the flags set by fcmp. The ordered predicates are false when an
// operand is NaN.
func fpCondCode(c ir.Cmp) (string, bool) {
	switch c {
	case ir.CmpFeq:
		return "eq", true
	case ir.CmpFne:
		return "ne", true
	case ir.CmpFlt:
		return "mi", true
	case ir.CmpFle:
		return "ls", true
	case ir.CmpFgt:
		return "gt", true
	case ir.CmpFge:
		return "ge", true
	case ir.CmpFo:
		return "vc", true // no NaN operands
	case ir.CmpFuo:
		return "vs", true // some NaN operand
	}
	return "", false
}

// loadInfo returns the load mnemonic and the destination-register width to name.
func loadInfo(op ir.Op, cls ir.Cls) (string, int) {
	switch op {
	case ir.OLoadub:
		return "ldrb", 4
	case ir.OLoaduh:
		return "ldrh", 4
	case ir.OLoaduw:
		return "ldr", 4
	case ir.OLoadsb:
		return "ldrsb", cls.Size()
	case ir.OLoadsh:
		return "ldrsh", cls.Size()
	case ir.OLoadsw:
		return "ldrsw", 8
	case ir.OLoadl:
		return "ldr", 8
	case ir.OLoads:
		return "ldr", 4
	case ir.OLoadd:
		return "ldr", 8
	case ir.OLoadq:
		return "ldr", 16
	}
	return "", 0
}

// storeInfo returns the store mnemonic and the value-register width to name.
func storeInfo(op ir.Op) (string, int) {
	switch op {
	case ir.OStoreb:
		return "strb", 4
	case ir.OStoreh:
		return "strh", 4
	case ir.OStorew:
		return "str", 4
	case ir.OStorel:
		return "str", 8
	case ir.OStores:
		return "str", 4
	case ir.OStored:
		return "str", 8
	case ir.OStoreq:
		return "str", 16
	}
	return "", 0
}

// allocShape returns the alignment and byte size of a stack allocation.
func allocShape(f *ir.Func, in *ir.Instr) (align, size int) {
	switch in.Op {
	case ir.OAlloc4:
		align = 4
	case ir.OAlloc8:
		align = 8
	default:
		align = 16
	}
	size = align
	if a := in.Arg(0); a.Kind == ir.RefConst {
		if c := f.Consts[a.ID]; c.Kind == ir.ConstInt && c.Int > 0 {
			size = int(c.Int)
		}
	}
	return align, roundUp(size, align)
}

func roundUp(n, a int) int {
	if a <= 0 {
		return n
	}
	return ((n + a - 1) / a) * a
}

// sanitize turns an IR name into a valid assembler label component.
func sanitize(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "anon"
	}
	return sb.String()
}
