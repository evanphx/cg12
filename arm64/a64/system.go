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
