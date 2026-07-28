package x64

// Atomic memory operations.
//
// Every atomicity guarantee x86-64 offers on a read-modify-write comes from one
// bit of encoding: the LOCK prefix, F0. Without it CMPXCHG, XADD and the ALU
// forms below are ordinary non-atomic instructions that read, compute and write
// back with a window in between -- code that is indistinguishable from the atomic
// version on one core and corrupts memory the moment two cores race. So the
// prefix is not an option a caller may forget here: it is baked into the name of
// every function that needs it.
//
// XCHG is the one exception, and it is the opposite kind of exception: with a
// memory operand the processor's locking protocol is applied whether or not the
// prefix is present (SDM Vol. 2, XCHG), so [Xchg] deliberately does not emit F0.
// The instruction is atomic and a full barrier regardless, and the prefix would
// only add a byte.
//
// The other structural gap this file fills is the memory-*destination* ALU form.
// x64.go's AddMem/AndMem/... are the reg <- reg OP mem direction (the spill-slot
// fold); a locked read-modify-write needs mem <- mem OP reg, which is the other
// opcode of the same pair. Both directions exist for every ALU op in the ISA;
// only one of them was reachable from Go until now.

// --- prefixes --------------------------------------------------------------

// lockPrefix is the LOCK prefix. It makes the instruction it precedes an atomic
// read-modify-write of its memory operand, and a full barrier.
const lockPrefix = 0xf0

// opsizePrefix selects the 16-bit operand size for an instruction whose default
// is 32.
const opsizePrefix = 0x66

// atomicPrefixes returns the legacy prefix bytes for an operation on a wbits-wide
// memory operand: the operand-size override when the width is 16, and LOCK when
// the operation must be atomic.
//
// The order matters only for byte-identity with other assemblers, not for
// decoding -- the legacy prefix groups may appear in any order, and LOCK (group 1)
// and 66 (group 3) are different groups. llvm-mc emits 66 F0, so that is the order
// used, which keeps the encoder tests comparing like with like. Both prefixes
// precede REX, which op_rm appends after them.
func atomicPrefixes(wbits int, lock bool) []byte {
	var pfx []byte
	if wbits == 16 {
		pfx = append(pfx, opsizePrefix)
	}
	if lock {
		pfx = append(pfx, lockPrefix)
	}
	return pfx
}

// memRMW encodes an instruction whose r/m operand is the memory being modified and
// whose reg operand is a register, at an 8/16/32/64-bit operand size. The byte
// width is where the REX-forcing rule bites: without REX, the reg field of a
// byte-operand instruction names AH/CH/DH/BH rather than SPL/BPL/SIL/DIL, so a
// source in RSP..RDI needs an otherwise-empty REX to be addressable at all.
func memRMW(opcode []byte, wbits int, lock bool, m Mem, src Reg) []byte {
	force8 := wbits == 8 && byteForce(src)
	return op_rm(nil, atomicPrefixes(wbits, lock), wbits == 64, opcode, src, m, force8)
}

// --- compare-and-swap ------------------------------------------------------

// LockCmpxchg encodes LOCK CMPXCHG [mem], src (0F B0 /r for a byte operand, 0F B1
// /r otherwise).
//
// The comparand is implicit: the accumulator (AL/AX/EAX/RAX) is compared against
// the memory operand. On a match, src is stored and ZF is set; on a mismatch, the
// value found in memory is loaded into the accumulator and ZF is cleared. So the
// accumulator holds the previous value either way, which is exactly what the
// atomic.cas intrinsic yields -- and the mismatch case reloading it is what makes
// the retry loop for a fetch-and-op work without a second load.
func LockCmpxchg(wbits int, m Mem, src Reg) []byte {
	opcode := byte(0xb1)
	if wbits == 8 {
		opcode = 0xb0
	}
	return memRMW([]byte{0x0f, opcode}, wbits, true, m, src)
}

// --- exchange-and-add ------------------------------------------------------

// LockXadd encodes LOCK XADD [mem], src (0F C0 /r for a byte operand, 0F C1 /r
// otherwise): memory and the register are added, the sum is stored to memory, and
// the register receives the value memory held before the add.
//
// The register operand is therefore written, not just read: a caller must hand it
// a register whose old contents are dead.
func LockXadd(wbits int, m Mem, src Reg) []byte {
	opcode := byte(0xc1)
	if wbits == 8 {
		opcode = 0xc0
	}
	return memRMW([]byte{0x0f, opcode}, wbits, true, m, src)
}

// --- exchange --------------------------------------------------------------

// Xchg encodes XCHG [mem], src (86 /r for a byte operand, 87 /r otherwise):
// memory and the register swap contents, so the register receives the previous
// value.
//
// No LOCK prefix is emitted, and that is not an omission. A memory-operand XCHG is
// always atomic and always a full barrier; the prefix is redundant. That property
// is also what makes this the cheapest sequentially consistent *store* on x86-64 --
// one instruction that both writes and drains the store buffer.
//
// Like LockXadd, the register operand is written as well as read.
func Xchg(wbits int, m Mem, src Reg) []byte {
	opcode := byte(0x87)
	if wbits == 8 {
		opcode = 0x86
	}
	return memRMW([]byte{opcode}, wbits, false, m, src)
}

// --- locked memory-destination ALU -----------------------------------------

// lockedALU encodes LOCK <op> [mem], src: the memory-destination direction of the
// classic ALU encoding, which is the opcode aluOp already records (0x01 for ADD,
// 0x09 OR, 0x21 AND, 0x29 SUB, 0x31 XOR) with the r/m operand in memory. The byte
// form is that opcode minus one throughout -- x86 pairs every ALU opcode as
// (byte, word-or-wider) and 0x01/0x00 for ADD is the same relationship as
// 0x31/0x30 for XOR.
//
// These discard the previous value; they only write memory and flags. A caller
// that needs the previous value wants LockXadd (for add) or a LockCmpxchg loop.
func lockedALU(o aluOp, wbits int, m Mem, src Reg) []byte {
	opcode := o.rmReg
	if wbits == 8 {
		opcode--
	}
	return memRMW([]byte{opcode}, wbits, true, m, src)
}

// LockAdd/LockSub/LockAnd/LockOr/LockXor encode "LOCK op [mem], src", an atomic
// read-modify-write of memory that yields no previous value.
func LockAdd(wbits int, m Mem, src Reg) []byte { return lockedALU(opAdd, wbits, m, src) }
func LockSub(wbits int, m Mem, src Reg) []byte { return lockedALU(opSub, wbits, m, src) }
func LockAnd(wbits int, m Mem, src Reg) []byte { return lockedALU(opAnd, wbits, m, src) }
func LockOr(wbits int, m Mem, src Reg) []byte  { return lockedALU(opOr, wbits, m, src) }
func LockXor(wbits int, m Mem, src Reg) []byte { return lockedALU(opXor, wbits, m, src) }

// --- fences ----------------------------------------------------------------

// Mfence encodes MFENCE (0F AE F0): a full barrier, ordering every load and store
// before it against every load and store after it.
//
// It is the only fence a sequentially consistent barrier can be built from on
// x86-64. SFENCE orders stores against stores and LFENCE loads against loads, and
// under TSO both of those orderings already hold for ordinary accesses -- neither
// supplies the store-then-load ordering, which is the single reordering the
// hardware actually performs and therefore the only one a fence has to forbid.
func Mfence() []byte { return []byte{0x0f, 0xae, 0xf0} }
