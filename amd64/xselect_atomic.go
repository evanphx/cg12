package amd64

import (
	"strings"

	"github.com/evanphx/cg12/ir"
)

// selectAtomic selects the atomic and synchronization operations through the
// xasmAtomic half of the builder.
//
// The 22 atomic intrinsics all arrive as ir.OIntrinsic, which selectCore also
// handles, so this family does override selectCore -- but only for names starting
// "atomic.", none of which selectCore recognizes: it would send every one of them
// to its "unsupported intrinsic" failure. An atomic name this file does not
// recognize is handed back to the chain deliberately, so it still reaches that
// failure and reports itself rather than being silently dropped.
func selectAtomic(s *xsel, in *ir.Instr) bool {
	if in.Op != ir.OIntrinsic || in.Intrin == nil {
		return false
	}
	base, bytes, ok := atomicNameParts(in.Intrin.Name)
	if !ok {
		return false
	}

	switch base {
	case "fence":
		s.b.atomicFence()
		return true

	case "load":
		// A plain MOV is already an acquire load on x86-64; see xasm_atomic.go.
		dst, commit := s.gpDst(in.To)
		s.b.atomicLoad(in.Arg(0), bytes, dst)
		commit()
		return true

	case "store":
		// The value is staged in scratch rather than used where the allocator put it:
		// atomicStore is an XCHG, which writes its register operand with the value it
		// displaced, and that register may still hold a live temp.
		s.gpInto(gpScratch0, in.Arg(1))
		s.b.atomicStore(in.Arg(0), bytes, gpScratch0)
		return true

	case "cas":
		s.atomicCAS(in, bytes)
		return true

	case "xchg", "add", "sub", "and", "or", "xor":
		s.atomicRMW(in, base, bytes)
		return true
	}
	return false
}

// atomicCAS selects a compare-and-swap.
//
// CMPXCHG's operands land where they need to be almost for free. The comparand is
// the accumulator, and RAX is held out of allocation (reg.go's intAllocOrder) so
// nothing has to be saved to use it -- the register constraint that on arm64 needs
// a scratch-pressure search costs one move here. The replacement value is read and
// not written, so it can be used from wherever it already is. And the accumulator
// holds the previous value afterwards whether the swap happened or not, which is
// exactly what the intrinsic yields: no branch, no second read, and the failure
// path falls out of the success path rather than being a case of its own.
func (s *xsel) atomicCAS(in *ir.Instr, bytes int) {
	replacement := s.gpValue(in.Arg(2), gpScratch0)
	s.gpInto(RAX, in.Arg(1))
	s.b.atomicCAS(in.Arg(0), bytes, replacement)
	s.atomicPrevious(in, bytes, RAX)
}

// atomicRMW selects an exchange or a fetch-and-op. Each yields the value the
// location held beforehand, and which instruction sequence produces that value is
// the only thing that varies:
//
//   - xchg: XCHG, whose register operand receives the previous value.
//   - add:  LOCK XADD, likewise.
//   - sub:  LOCK XADD of the negated operand. There is no locked subtract-and-fetch,
//     and there does not need to be -- x - v is x + (-v), and the negation happens
//     once, outside the atomic access, on a private copy of the operand.
//   - and/or/xor: a LOCK CMPXCHG retry loop, because the locked ALU forms report no
//     previous value.
//
// When the instruction names no result temp there is nothing to report and all of
// them collapse to a single locked memory-destination instruction. That form is
// reachable from the textual IL (ir/intrinsic.go's registration is what fixes the
// operation and width in the name precisely so a void atomic round-trips) and is
// why the memory-destination ALU encoders exist in x64 at all.
func (s *xsel) atomicRMW(in *ir.Instr, op string, bytes int) {
	if in.To.Kind != ir.RefTemp {
		if op == "xchg" {
			// A void exchange is a store: the displaced value is simply dropped.
			s.gpInto(gpScratch0, in.Arg(1))
			s.b.atomicXchg(in.Arg(0), bytes, gpScratch0)
			return
		}
		value := s.gpValue(in.Arg(1), gpScratch0)
		s.b.atomicALU(op, in.Arg(0), bytes, value)
		return
	}

	switch op {
	case "and", "or", "xor":
		// The operand is only read here, so it can stay where the allocator put it.
		// The loop's own registers are RAX and RCX, neither of which the allocator
		// hands out, so they cannot collide with it.
		value := s.gpValue(in.Arg(1), gpScratch0)
		s.b.atomicFetchALU(op, in.Arg(0), bytes, value)
		s.atomicPrevious(in, bytes, RAX)
		return
	}

	// XCHG and XADD both write the register they are given, so the operand has to be
	// copied into scratch first: writing the previous value over the allocator's
	// register for the operand would corrupt a temp that is still live. Negating for
	// sub then costs nothing, since the copy is already private.
	s.gpInto(gpScratch0, in.Arg(1))
	if op == "sub" {
		s.b.negGP(bytes == 8, gpScratch0)
	}
	if op == "xchg" {
		s.b.atomicXchg(in.Arg(0), bytes, gpScratch0)
	} else {
		s.b.atomicXadd(in.Arg(0), bytes, gpScratch0)
	}
	s.atomicPrevious(in, bytes, gpScratch0)
}

// atomicPrevious commits an atomic's previous-value result, held in `from`, to the
// result temp's home.
//
// A byte or halfword operation leaves only the low bytes of `from` defined -- XADD
// and CMPXCHG at those widths write nothing above them -- so the value is
// zero-extended first, which is also what makes a narrow atomic on this backend
// yield the same word arm64's LDAXRB/LDAXRH produce.
func (s *xsel) atomicPrevious(in *ir.Instr, bytes int, from Reg) {
	if in.To.Kind != ir.RefTemp {
		return
	}
	s.b.atomicZeroExtend(bytes, from)
	size := s.f.Temps[in.To.ID].Cls.Size()
	dst, commit := s.gpDst(in.To)
	s.b.move(regLoc(dst, size, false), regLoc(from, size, false))
	commit()
}

// atomicNameParts splits an atomic intrinsic name into its base operation and the
// access width in bytes the name's suffix encodes: "atomic.add.l" -> ("add", 8).
// "atomic.fence" has no width and reports 0. Anything that is not an atomic
// intrinsic reports ok=false, so the probe chain moves on.
func atomicNameParts(name string) (base string, bytes int, ok bool) {
	rest, isAtomic := strings.CutPrefix(name, "atomic.")
	if !isAtomic {
		return "", 0, false
	}
	if rest == "fence" {
		return "fence", 0, true
	}
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		return "", 0, false
	}
	bytes, ok = atomicWidthBytes(rest[dot+1:])
	if !ok {
		return "", 0, false
	}
	return rest[:dot], bytes, true
}

// atomicWidthBytes maps an atomic intrinsic's width suffix to its access width.
func atomicWidthBytes(suffix string) (int, bool) {
	switch suffix {
	case "b":
		return 1, true
	case "h":
		return 2, true
	case "w":
		return 4, true
	case "l":
		return 8, true
	}
	return 0, false
}
