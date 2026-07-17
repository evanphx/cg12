package arm64

import (
	"github.com/evanphx/cg12/ir"
)

func (e *emitter) parallelMove(pairs []movePairLoc) {
	(&sel{f: e.f, b: &textAsm{e: e}, spillBase: e.spillBase}).parallelMove(pairs)
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
		case size == 16:
			e.line("mov %s, %s", dst.reg.vec16Name(), src.reg.vec16Name()) // full 128-bit copy
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
