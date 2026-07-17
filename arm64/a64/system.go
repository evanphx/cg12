package a64

// System and synchronization instructions: the ones an operating-system kernel,
// a threading runtime, or an atomic sequence is written out of. None of these
// were here, so a template using any of them met an assembler with no word for
// it -- which for the exclusives meant cg12 could not express a compare-and-swap
// loop at all, the thing #126's atomics will be built from.

// Svc encodes SVC #imm16, the supervisor call that enters the kernel. On Linux
// this is how a system call is made once its number and arguments are in place.
func Svc(imm16 uint16) uint32 { return 0xd4000001 | uint32(imm16)<<5 }

// BarrierOption is the shareability/access domain of a memory barrier, the CRm
// field of DMB/DSB. The common ones are the full-system and inner-shareable
// domains; a value outside 0..15 is not encodable.
type BarrierOption uint32

const (
	BarrierSY    BarrierOption = 0xf // full system, loads and stores
	BarrierISH   BarrierOption = 0xb // inner shareable
	BarrierISHST BarrierOption = 0xa // inner shareable, stores only
	BarrierLD    BarrierOption = 0xd // full system, loads
	BarrierST    BarrierOption = 0xe // full system, stores
)

// Dmb encodes DMB <option>, a data memory barrier: it orders memory accesses
// before it against those after it, without stalling the pipeline for
// non-memory work. This is the fence an acquire/release or a seq_cst access
// lowers to.
func Dmb(o BarrierOption) uint32 { return 0xd50330bf | field(uint32(o), 4, "barrier")<<8 }

// Dsb encodes DSB <option>, a data synchronization barrier: stronger than DMB,
// it also waits for the ordered accesses to complete before any later
// instruction runs at all.
func Dsb(o BarrierOption) uint32 { return 0xd503309f | field(uint32(o), 4, "barrier")<<8 }

// Isb encodes ISB, an instruction synchronization barrier: it flushes the
// pipeline so instructions after it are re-fetched, which is required after
// writing a system register that changes how later instructions execute.
func Isb() uint32 { return 0xd50330df | 0xf<<8 } // ISB SY

// The exclusive-access pair. LDXR reads and tags the address as monitored;
// STXR writes only if nothing touched it since, reporting success in a register.
// A loop of the two, retried while STXR reports failure, is the entire basis of
// lock-free atomics on this architecture. The load-acquire / store-release
// variants (LDAXR/STLXR) add the ordering a seq_cst atomic needs, in the same
// instruction rather than a separate barrier.

// exclusive builds one of the four forms. size 2 is a 32-bit access, 3 is
// 64-bit; o0 selects the acquire/release ordering; l selects load vs store.
func exclusive(size, l, o0 uint32, rs, rt2, rn, rt Reg) uint32 {
	return size<<30 | 0x8<<24 | l<<22 | field(uint32(rs), 5, "reg")<<16 | o0<<15 |
		field(uint32(rt2), 5, "reg")<<10 | r(rn)<<5 | r(rt)
}

// Ldxr encodes LDXR <Wt|Xt>, [<Xn>]: an exclusive load that begins a monitored
// sequence.
func Ldxr(w64 bool, rt, rn Reg) uint32 { return exclusive(exSize(w64), 1, 0, 31, 31, rn, rt) }

// Ldaxr encodes LDAXR <Wt|Xt>, [<Xn>]: an exclusive load with acquire ordering.
func Ldaxr(w64 bool, rt, rn Reg) uint32 { return exclusive(exSize(w64), 1, 1, 31, 31, rn, rt) }

// Stxr encodes STXR <Ws>, <Wt|Xt>, [<Xn>]: an exclusive store. Ws receives 0 on
// success and 1 if the monitor was lost, so the caller retries on non-zero.
func Stxr(w64 bool, rs, rt, rn Reg) uint32 { return exclusive(exSize(w64), 0, 0, rs, 31, rn, rt) }

// Stlxr encodes STLXR <Ws>, <Wt|Xt>, [<Xn>]: an exclusive store with release
// ordering.
func Stlxr(w64 bool, rs, rt, rn Reg) uint32 { return exclusive(exSize(w64), 0, 1, rs, 31, rn, rt) }

func exSize(w64 bool) uint32 {
	if w64 {
		return 3
	}
	return 2
}

// System-register access. A system register is named by five small fields
// (op0/op1, CRn, CRm, op2); MRS reads one into a general register, MSR writes
// one from it. The named-register set below is the user-accessible common ones
// -- the floating-point control and status words, the condition flags, the
// counters and cache-geometry registers a runtime reads -- rather than the whole
// architectural space, which runs to hundreds.

// SysReg identifies a system register by its encoding fields. o0 is op0-2, the
// two-bit value that actually lands in the instruction.
type SysReg struct {
	o0, op1, crn, crm, op2 uint32
	name                   string
}

// Mrs encodes MRS <Xt>, <sysreg> -- read a system register.
func Mrs(rt Reg, s SysReg) uint32 {
	return 0xd5300000 | s.o0<<19 | s.op1<<16 | s.crn<<12 | s.crm<<8 | s.op2<<5 | r(rt)
}

// Msr encodes MSR <sysreg>, <Xt> -- write a system register.
func Msr(s SysReg, rt Reg) uint32 {
	return 0xd5100000 | s.o0<<19 | s.op1<<16 | s.crn<<12 | s.crm<<8 | s.op2<<5 | r(rt)
}

// SysRegs are the system registers this assembler names, keyed by their lower-case
// mnemonic. Fields are (o0, op1, CRn, CRm, op2); o0 is op0-2 and is 1 for every
// register here (op0 == 3, the user/system range).
var SysRegs = map[string]SysReg{
	"tpidr_el0":   {1, 3, 13, 0, 2, "tpidr_el0"},   // thread pointer (EL0)
	"tpidrro_el0": {1, 3, 13, 0, 3, "tpidrro_el0"}, // read-only thread pointer
	"fpcr":        {1, 3, 4, 4, 0, "fpcr"},         // FP control
	"fpsr":        {1, 3, 4, 4, 1, "fpsr"},         // FP status
	"nzcv":        {1, 3, 4, 2, 0, "nzcv"},         // condition flags
	"ctr_el0":     {1, 3, 0, 0, 1, "ctr_el0"},      // cache type
	"dczid_el0":   {1, 3, 0, 0, 7, "dczid_el0"},    // DC ZVA block size
	"cntvct_el0":  {1, 3, 14, 0, 2, "cntvct_el0"},  // virtual counter
	"cntfrq_el0":  {1, 3, 14, 0, 0, "cntfrq_el0"},  // counter frequency
	"midr_el1":    {1, 0, 0, 0, 0, "midr_el1"},     // main ID (EL1)
	"mpidr_el1":   {1, 0, 0, 0, 5, "mpidr_el1"},    // multiprocessor affinity
}

// sysRegByEncoding is the inverse of SysRegs, for the disassembler.
var sysRegByEncoding = func() map[uint32]string {
	m := map[uint32]string{}
	for _, s := range SysRegs {
		m[s.o0<<19|s.op1<<16|s.crn<<12|s.crm<<8|s.op2<<5] = s.name
	}
	return m
}()
