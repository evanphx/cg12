package amd64

import (
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
)

// xasmAtomic is the atomic and synchronization surface: the atomic load/store
// pair, exchange, compare-and-swap, the locked read-modify-write arithmetic and
// bitwise forms, and the fence.
//
// x86-64 makes this simpler than arm64 rather than harder. arm64 states every
// read-modify-write as an LDAXR/STLXR retry loop, so its lowering has to find a
// register for the observed value, another for the computed value and a third for
// the store status, all distinct from the address and the operand -- five live
// registers for a fetch-and-add, which is what arm64/select.go's atomicResult and
// freeScratch machinery exists to arrange. Here the LOCK prefix makes one
// instruction do the whole thing, so nothing of that kind is needed.
//
// # Memory ordering
//
// This is the part worth reading, because getting it wrong produces code that
// works on one core and corrupts under contention.
//
// x86-64 is TSO. The only reordering the hardware performs on ordinary aligned
// accesses is store-then-load: a load may be satisfied from the store buffer
// before an earlier store becomes visible. Load-load, load-store and store-store
// order are all already guaranteed. So:
//
//   - An acquire load is a plain MOV. No barrier, because the orderings acquire
//     forbids (later loads and stores floating above it) are ones the hardware
//     never performs.
//   - A release store is a plain MOV, for the mirror-image reason.
//   - A *sequentially consistent* store is not, and this is the one place a
//     barrier is unavoidable: SC requires that a store and a subsequent load not
//     be reordered, which is precisely the reordering TSO allows. It needs either
//     MOV followed by MFENCE, or a locked instruction that both stores and drains
//     the store buffer. XCHG is exactly that in one instruction, and is what
//     atomicStore emits.
//   - Every LOCK-prefixed read-modify-write is already a full barrier, so none of
//     them needs a fence on either side.
//
// The IR's atomic intrinsics carry no memory-order operand -- ir/intrinsic.go
// encodes only the operation and the width, and cc/atomic.go discards C's order
// argument -- so the backend cannot know that a given atomic.store was only ever
// meant to be a release. It must therefore assume the strongest order any source
// could have asked for, which is why the store side pays for a barrier it would
// often not need. (Go's own amd64 runtime makes the same choice for the same
// reason: XCHG for Store, plain MOV for StoreRel, because Go's IR *does*
// distinguish them.) Adding an order to the intrinsic is the way to recover the
// cheaper store; see the report accompanying this work.
type xasmAtomic interface {
	// atomicLoad emits the load half of the pair: a plain MOV, zero-extending a
	// byte or halfword, which is already an acquire load on this architecture.
	atomicLoad(addr ir.Ref, bytes int, dst Reg)

	// atomicStore emits the store half: XCHG, whose implicit lock supplies the
	// store-then-load ordering a sequentially consistent store needs. val is
	// clobbered -- it receives the value being overwritten, which is discarded.
	atomicStore(addr ir.Ref, bytes int, val Reg)

	// atomicXchg emits XCHG for its value rather than for its barrier: val receives
	// the previous contents of the location.
	atomicXchg(addr ir.Ref, bytes int, val Reg)

	// atomicXadd emits LOCK XADD, the one fetch-and-op x86-64 has a single
	// instruction for. val receives the previous contents of the location.
	atomicXadd(addr ir.Ref, bytes int, val Reg)

	// atomicALU emits a locked memory-destination ALU instruction: an atomic
	// read-modify-write that yields no previous value, for the void form of an
	// intrinsic whose result nothing asked for.
	atomicALU(op string, addr ir.Ref, bytes int, val Reg)

	// atomicFetchALU emits the LOCK CMPXCHG retry loop that gives and/or/xor the
	// previous value they have no single-instruction form for. The result lands in
	// RAX.
	atomicFetchALU(op string, addr ir.Ref, bytes int, val Reg)

	// atomicCAS emits LOCK CMPXCHG with the comparand already in RAX, where the
	// previous value is also left.
	atomicCAS(addr ir.Ref, bytes int, replacement Reg)

	// atomicFence emits MFENCE.
	atomicFence()

	// atomicZeroExtend widens a byte or halfword previous value to a full word, and
	// does nothing at the wider widths.
	atomicZeroExtend(bytes int, r Reg)
}

// atomicLoopValue is the register the CMPXCHG retry loop computes the replacement
// value in. RCX is held out of allocation (reg.go's intAllocOrder) for the
// fixed-register instructions, the same standing arrangement divGP relies on for
// RDX:RAX and clzGP for RCX/RDX, so nothing the allocator produced can be sitting
// in it. The loop's other fixed register is RAX, which CMPXCHG's encoding chooses
// for us.
const atomicLoopValue = RCX

func (b *mcXasm) atomicLoad(addr ir.Ref, bytes int, dst Reg) {
	mem, fixup := b.m.memAddr(addr, b.m.gpScratch1)
	d := dst.mreg()
	// A narrow atomic load zero-extends, matching arm64's LDARB/LDARH and so
	// giving the intrinsic one result across backends. The 32-bit form needs no
	// extension of its own: writing a 32-bit register zeroes the upper half.
	switch bytes {
	case 1:
		b.m.emit(x64.MovzxLoadByte(false, d, mem))
	case 2:
		b.m.emit(x64.MovzxLoadWord(false, d, mem))
	case 4:
		b.m.emit(x64.Load(false, d, mem))
	default:
		b.m.emit(x64.Load(true, d, mem))
	}
	fixup()
}

func (b *mcXasm) atomicStore(addr ir.Ref, bytes int, val Reg) {
	// XCHG rather than MOV: see the ordering note on xasmAtomic. The exchanged-out
	// value lands in val and is simply not read.
	b.xchgMem(addr, bytes, val)
}

func (b *mcXasm) atomicXchg(addr ir.Ref, bytes int, val Reg) {
	b.xchgMem(addr, bytes, val)
}

func (b *mcXasm) xchgMem(addr ir.Ref, bytes int, val Reg) {
	mem, fixup := b.m.memAddr(addr, b.m.gpScratch1)
	b.m.emit(x64.Xchg(bytes*8, mem, val.mreg()))
	fixup()
}

func (b *mcXasm) atomicXadd(addr ir.Ref, bytes int, val Reg) {
	mem, fixup := b.m.memAddr(addr, b.m.gpScratch1)
	b.m.emit(x64.LockXadd(bytes*8, mem, val.mreg()))
	fixup()
}

func (b *mcXasm) atomicALU(op string, addr ir.Ref, bytes int, val Reg) {
	mem, fixup := b.m.memAddr(addr, b.m.gpScratch1)
	wbits := bytes * 8
	switch op {
	case "add":
		b.m.emit(x64.LockAdd(wbits, mem, val.mreg()))
	case "sub":
		b.m.emit(x64.LockSub(wbits, mem, val.mreg()))
	case "and":
		b.m.emit(x64.LockAnd(wbits, mem, val.mreg()))
	case "or":
		b.m.emit(x64.LockOr(wbits, mem, val.mreg()))
	case "xor":
		b.m.emit(x64.LockXor(wbits, mem, val.mreg()))
	default:
		b.fail("amd64: %q has no locked memory-destination form", op)
		return
	}
	fixup()
}

// atomicFetchALU emits the compare-and-swap retry loop:
//
//	mov      eax, [addr]        ; a plain read; the cmpxchg is the ordered access
//	retry:
//	mov      ecx, eax
//	and      ecx, val           ; or/xor likewise
//	lock cmpxchg [addr], ecx    ; ZF=1 and [addr]=ecx, or eax=[addr] and ZF=0
//	jne      retry
//	                            ; eax holds the previous value
//
// A loop is needed because x86-64 has no fetch-and-and: LOCK AND writes memory and
// flags and discards what was there, so the previous value the intrinsic yields can
// only come from a CMPXCHG that reports it. The loop is nevertheless bounded in
// practice in a way arm64's LL/SC loop is not -- CMPXCHG fails only on a genuine
// concurrent modification, not on an unrelated cache event.
//
// The initial load needs no ordering of its own: it is a hint, not an observation
// the result depends on. Everything the result rests on comes from the CMPXCHG that
// succeeded, and that instruction is a full barrier. A stale initial value costs one
// extra iteration and nothing else.
//
// On failure CMPXCHG has already reloaded RAX with what it found, so the branch
// goes back to recomputing the replacement rather than to the load.
func (b *mcXasm) atomicFetchALU(op string, addr ir.Ref, bytes int, val Reg) {
	mem, fixup := b.m.memAddr(addr, b.m.gpScratch1)
	observed := RAX.mreg()
	replacement := atomicLoopValue.mreg()
	// The width the ALU step computes at. A byte or halfword operation computes in
	// 32 bits and lets the narrow CMPXCHG take the low bytes it needs, so only the
	// 64-bit case needs REX.W.
	wide := bytes == 8

	switch bytes {
	case 1:
		b.m.emit(x64.MovzxLoadByte(false, observed, mem))
	case 2:
		b.m.emit(x64.MovzxLoadWord(false, observed, mem))
	case 4:
		b.m.emit(x64.Load(false, observed, mem))
	default:
		b.m.emit(x64.Load(true, observed, mem))
	}
	fixup()

	// The loop is closed with a computed displacement rather than a label. The
	// branch is entirely inside one instruction's expansion, so no other code can
	// name its target, and x64.Program never relaxes an encoding after the fact --
	// the offsets it hands out stay put -- which makes the arithmetic reliable and
	// spares the emitter a label-naming scheme for something invisible outside
	// these four instructions.
	retry := b.m.prog.Len()
	b.m.emit(x64.MovReg(wide, replacement, observed))
	switch op {
	case "and":
		b.m.emit(x64.AndReg(wide, replacement, val.mreg()))
	case "or":
		b.m.emit(x64.OrReg(wide, replacement, val.mreg()))
	case "xor":
		b.m.emit(x64.XorReg(wide, replacement, val.mreg()))
	default:
		b.fail("amd64: %q is not a compare-and-swap loop operation", op)
		return
	}
	b.m.emit(x64.LockCmpxchg(bytes*8, mem, replacement))
	fixup()

	// Jcc is a 6-byte rel32 form and its displacement is measured from the
	// instruction's end.
	const jccLen = 6
	back := int32(retry - (b.m.prog.Len() + jccLen))
	b.m.emit(x64.Jcc(x64.NE, back))
}

func (b *mcXasm) atomicCAS(addr ir.Ref, bytes int, replacement Reg) {
	mem, fixup := b.m.memAddr(addr, b.m.gpScratch1)
	b.m.emit(x64.LockCmpxchg(bytes*8, mem, replacement.mreg()))
	fixup()
}

func (b *mcXasm) atomicFence() {
	b.m.emit(x64.Mfence())
}

func (b *mcXasm) atomicZeroExtend(bytes int, r Reg) {
	switch bytes {
	case 1:
		b.m.emit(x64.MovzxByte(false, r.mreg(), r.mreg()))
	case 2:
		b.m.emit(x64.MovzxWord(false, r.mreg(), r.mreg()))
	}
}
