package amd64

import (
	"fmt"
	"math"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// xasmFloat is computation in XMM registers: the two-operand SSE arithmetic, the
// compare that materializes a boolean, the float/int conversions in both
// signednesses, and the bitcasts between the two register banks (which are
// XMM-side instructions -- movq -- even though only one end of each is a float).
type xasmFloat interface {
	// Floating point: two-operand arithmetic (dst = dst OP src), register move,
	// sign-flip negation, and a compare that sets the boolean result.
	binFP(op ir.Op, dbl bool, dst, src Reg)
	movFP(dbl bool, dst, src Reg)
	fnegFP(dbl bool, dst Reg)
	fcmpSet(cmp ir.Cmp, dbl bool, dst, a, b Reg)

	// Float/int conversions and int<->float bitcasts.
	cvtSS2SD(dst, src Reg)
	cvtSD2SS(dst, src Reg)
	cvtF2SI(w, srcD bool, dst, src Reg)
	cvtSI2F(w, dstD bool, dst, src Reg)

	// The 64-bit unsigned directions, which x86-64 has no single instruction for
	// (arm64 has fcvtzu/ucvtf): each is a compare-and-bias sequence pivoting on
	// 2^63, spelled out at its implementation. cvtUI642F rewrites its source in
	// place, so it must be handed a scratch register.
	cvtF2UI64(srcD bool, dst, src Reg)
	cvtUI642F(dstD bool, dst, src Reg)

	castG2F(dbl bool, dst, src Reg)
	castF2G(long bool, dst, src Reg)
}

func (b *mcXasm) binFP(op ir.Op, dbl bool, dst, src Reg) {
	dm, rm := dst.mreg(), src.mreg()
	switch {
	case op == ir.OAdd && dbl:
		b.m.emit(x64.Addsd(dm, rm))
	case op == ir.OAdd:
		b.m.emit(x64.Addss(dm, rm))
	case op == ir.OSub && dbl:
		b.m.emit(x64.Subsd(dm, rm))
	case op == ir.OSub:
		b.m.emit(x64.Subss(dm, rm))
	case op == ir.OMul && dbl:
		b.m.emit(x64.Mulsd(dm, rm))
	case op == ir.OMul:
		b.m.emit(x64.Mulss(dm, rm))
	case op == ir.ODiv && dbl:
		b.m.emit(x64.Divsd(dm, rm))
	case op == ir.ODiv:
		b.m.emit(x64.Divss(dm, rm))
	default:
		b.fail("amd64: unsupported float op %s", op)
	}
}
func (b *mcXasm) movFP(dbl bool, dst, src Reg) {
	if dbl {
		b.m.emit(x64.MovsdReg(dst.mreg(), src.mreg()))
	} else {
		b.m.emit(x64.MovssReg(dst.mreg(), src.mreg()))
	}
}
func (b *mcXasm) fnegFP(dbl bool, dst Reg) {
	if dbl {
		b.m.movImm(gpScratch0, int64(-9223372036854775808), true) // 0x8000000000000000
		b.m.emit(x64.MovqToXmm(true, fpScratch1.mreg(), gpScratch0.mreg()))
		b.m.emit(x64.Xorpd(dst.mreg(), fpScratch1.mreg()))
	} else {
		b.m.movImm(gpScratch0, 0x80000000, false)
		b.m.emit(x64.MovqToXmm(false, fpScratch1.mreg(), gpScratch0.mreg()))
		b.m.emit(x64.Xorps(dst.mreg(), fpScratch1.mreg()))
	}
}
func (b *mcXasm) fcmpSet(cmp ir.Cmp, dbl bool, dst, a, bb Reg) {
	dm := dst.mreg()
	ucomi := func(x, y Reg) {
		if dbl {
			b.m.emit(x64.Ucomisd(x.mreg(), y.mreg()))
		} else {
			b.m.emit(x64.Ucomiss(x.mreg(), y.mreg()))
		}
	}
	switch cmp {
	case ir.CmpFgt:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.A, dm))
	case ir.CmpFge:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.AE, dm))
	case ir.CmpFlt:
		ucomi(bb, a)
		b.m.emit(x64.Setcc(x64.A, dm))
	case ir.CmpFle:
		ucomi(bb, a)
		b.m.emit(x64.Setcc(x64.AE, dm))
	case ir.CmpFo:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.NP, dm))
	case ir.CmpFuo:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.P, dm))
	case ir.CmpFeq:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.NP, gpScratch0.mreg()))
		b.m.emit(x64.Setcc(x64.E, dm))
		b.m.emit(x64.AndReg(false, dm, gpScratch0.mreg()))
	case ir.CmpFne:
		ucomi(a, bb)
		b.m.emit(x64.Setcc(x64.P, gpScratch0.mreg()))
		b.m.emit(x64.Setcc(x64.NE, dm))
		b.m.emit(x64.OrReg(false, dm, gpScratch0.mreg()))
	default:
		b.fail("amd64: unsupported float compare %v", cmp)
	}
	b.m.emit(x64.MovzxByte(false, dm, dm))
}
func (b *mcXasm) cvtSS2SD(dst, src Reg) { b.m.emit(x64.Cvtss2sd(dst.mreg(), src.mreg())) }
func (b *mcXasm) cvtSD2SS(dst, src Reg) { b.m.emit(x64.Cvtsd2ss(dst.mreg(), src.mreg())) }
func (b *mcXasm) cvtF2SI(w, srcD bool, dst, src Reg) {
	if srcD {
		b.m.emit(x64.Cvttsd2si(w, dst.mreg(), src.mreg()))
	} else {
		b.m.emit(x64.Cvttss2si(w, dst.mreg(), src.mreg()))
	}
}
func (b *mcXasm) cvtSI2F(w, dstD bool, dst, src Reg) {
	if dstD {
		b.m.emit(x64.Cvtsi2sd(w, dst.mreg(), src.mreg()))
	} else {
		b.m.emit(x64.Cvtsi2ss(w, dst.mreg(), src.mreg()))
	}
}

// 2^63 as a single- and a double-precision bit pattern. It is the pivot both
// unsigned 64-bit conversions turn on, and it is exactly representable in either
// width (its mantissa is zero), which is what makes the bias steps below exact.
const (
	biasF32Bits = 0x5f000000
	biasF64Bits = 0x43e0000000000000
)

// cvtF2UI64 truncates a float towards zero into an unsigned 64-bit integer.
//
// x86-64 only has the signed truncation CVTTS{S,D}2SI (arm64 has a native
// fcvtzu). A source at or above 2^63 has no int64 to land in, so the instruction
// yields the "integer indefinite" value 0x8000000000000000 instead of the value
// asked for -- which is exactly the range an unsigned result cares about. The
// sequence therefore pivots on 2^63: below it the signed truncation is already
// the right answer, and at or above it the source is biased down by 2^63 before
// truncating and the 2^63 is put back into the integer result afterwards. Both
// halves are exact: a float at or above 2^63 has no fractional bits left, so
// subtracting 2^63 discards nothing, and the remainder is below 2^63 and so does
// fit an int64.
//
//	movabs r11, <2^63 as a float>
//	movq   xmm14, r11
//	ucomis{s,d} src, xmm14   ; CF is set when src < 2^63 -- and when src is NaN,
//	jb     direct            ; which wants the signed instruction's own answer
//	movs{s,d} xmm15, src     ; src can be a live value, so bias a copy of it
//	subs{s,d} xmm15, xmm14
//	cvtts{s,d}2si dst, xmm15 ; the remainder, which is in [0, 2^63)
//	movabs r11, 1<<63
//	add    dst, r11          ; put the subtracted 2^63 back
//	jmp    done
//	direct:
//	cvtts{s,d}2si dst, src
//	done:
//
// This clobbers R11 and both XMM scratch registers. dst is never R11 -- gpDst
// hands out either an allocated register or R10 -- so parking the two constants
// there cannot collide with the result being built.
func (b *mcXasm) cvtF2UI64(srcD bool, dst, src Reg) {
	direct, done := b.seqLabels(".f2u64")

	bias := int64(biasF32Bits)
	if srcD {
		bias = biasF64Bits
	}
	b.m.movImm(gpScratch1, bias, srcD)
	b.m.emit(x64.MovqToXmm(srcD, fpScratch0.mreg(), gpScratch1.mreg()))
	if srcD {
		b.m.emit(x64.Ucomisd(src.mreg(), fpScratch0.mreg()))
	} else {
		b.m.emit(x64.Ucomiss(src.mreg(), fpScratch0.mreg()))
	}
	b.m.prog.Jcc(x64.B, direct)

	if src != fpScratch1 {
		b.movFP(srcD, fpScratch1, src)
	}
	if srcD {
		b.m.emit(x64.Subsd(fpScratch1.mreg(), fpScratch0.mreg()))
	} else {
		b.m.emit(x64.Subss(fpScratch1.mreg(), fpScratch0.mreg()))
	}
	b.cvtF2SI(true, srcD, dst, fpScratch1)
	b.m.movImm(gpScratch1, math.MinInt64, true) // 1<<63, the bias as an integer
	b.m.emit(x64.AddReg(true, dst.mreg(), gpScratch1.mreg()))
	b.m.prog.Jmp(done)

	b.m.prog.Label(direct)
	b.cvtF2SI(true, srcD, dst, src)
	b.m.prog.Label(done)
}

// cvtUI642F converts an unsigned 64-bit integer to a float.
//
// CVTSI2S{S,D} is signed (arm64 has a native ucvtf), so a source with its top bit
// set converts to a *negative* float. A source that fits an int64 needs nothing
// special; otherwise the value is halved so that it does fit, converted, and
// doubled back:
//
//	test src, src
//	jns  direct
//	mov  r10, src
//	and  r10, 1        ; the bit the shift is about to drop
//	shr  src, 1
//	or   src, r10      ; keep the halved value odd when a bit was dropped
//	cvtsi2s{s,d} dst, src
//	adds{s,d} dst, dst ; doubling is exact, so no second rounding happens here
//	jmp  done
//	direct:
//	cvtsi2s{s,d} dst, src
//	done:
//
// The OR is what makes the halving round correctly rather than merely closely.
// Shifting right throws bit 0 away, and converting the halved value then rounds
// to nearest-even -- so a value sitting exactly halfway between two
// representable results would break the tie on the halved value, which is not
// where the tie is. Re-inserting the dropped bit keeps the halved value odd
// whenever anything was lost, which removes the false tie. Without it
// 2^63+1025 converts to 9223372036854775808 rather than the correctly rounded
// 9223372036854777856.
//
// src must be a scratch register, because the halving rewrites it in place. This
// also clobbers R10.
func (b *mcXasm) cvtUI642F(dstD bool, dst, src Reg) {
	direct, done := b.seqLabels(".u642f")

	b.m.emit(x64.TestReg(true, src.mreg(), src.mreg()))
	b.m.prog.Jcc(x64.NS, direct)

	b.m.emit(x64.MovReg(true, gpScratch0.mreg(), src.mreg()))
	b.m.emit(x64.AndImm(true, gpScratch0.mreg(), 1))
	b.m.emit(x64.ShrImm(true, src.mreg(), 1))
	b.m.emit(x64.OrReg(true, src.mreg(), gpScratch0.mreg()))
	b.cvtSI2F(true, dstD, dst, src)
	if dstD {
		b.m.emit(x64.Addsd(dst.mreg(), dst.mreg()))
	} else {
		b.m.emit(x64.Addss(dst.mreg(), dst.mreg()))
	}
	b.m.prog.Jmp(done)

	b.m.prog.Label(direct)
	b.cvtSI2F(true, dstD, dst, src)
	b.m.prog.Label(done)
}

// seqLabels names the two local labels the conversion sequences above need: the
// fast path that needs no adjustment, and the join point after the adjusted one.
// Program labels have to be unique within the function and there is no
// instruction counter to number them by, so the byte offset the sequence starts
// at does the naming: offsets only ever grow, and every sequence emits code, so
// no two can start at the same one. The leading dot keeps them out of the way of
// block labels, which are named by the IR.
func (b *mcXasm) seqLabels(kind string) (direct, done string) {
	tag := fmt.Sprintf("%s.%d", kind, b.m.prog.Len())
	return tag + ".direct", tag + ".done"
}
func (b *mcXasm) castG2F(dbl bool, dst, src Reg) {
	b.m.emit(x64.MovqToXmm(dbl, dst.mreg(), src.mreg()))
}
func (b *mcXasm) castF2G(long bool, dst, src Reg) {
	b.m.emit(x64.MovqFromXmm(long, dst.mreg(), src.mreg()))
}
