package amd64

import (
	"fmt"
	"math"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/obj"
)

// TLSModel names a way of reaching a thread-local variable.
//
// The models differ only in where the variable's offset from the thread pointer
// comes from, and they are ordered here by how much the code has to do to find
// out: baked in, read from a slot, or asked for at run time. A model that assumes
// less works in more places and costs more, so the cheapest one the image can
// actually use is the one to pick.
type TLSModel uint8

const (
	// TLSLocalExec bakes the offset from the thread pointer into the code. Only
	// the main executable can do this: its block sits at a fixed place in every
	// thread, so the offset is a link-time constant.
	TLSLocalExec TLSModel = iota

	// TLSInitialExec reads the offset from a GOT slot the loader fills, so the
	// variable may live in a shared library. It still assumes the library is there
	// from the start -- a library brought in later by dlopen may find no room left
	// in the static block, which is what general-dynamic exists for.
	TLSInitialExec

	// TLSGeneralDynamic asks __tls_get_addr for the address, which finds or
	// allocates this thread's block for the owning module. It works for any
	// thread-local, including one in a library brought in by dlopen, at the cost of
	// a call.
	//
	// It is not implemented on amd64 yet, and asking for it is an error rather than
	// a silent fallback -- see materializeThreadSym for why and for what it waits
	// on.
	TLSGeneralDynamic
)

// String names the model, for diagnostics.
func (t TLSModel) String() string {
	switch t {
	case TLSLocalExec:
		return "local-exec"
	case TLSInitialExec:
		return "initial-exec"
	case TLSGeneralDynamic:
		return "general-dynamic"
	}
	return fmt.Sprintf("TLSModel(%d)", uint8(t))
}

// materializeThreadSym computes the address of thread-local sym+off into d, by
// the sequence the selected model calls for.
//
// Every model starts the same way, with the thread pointer out of %fs:0. That
// self-load is the x86-64 way of reading the thread pointer: the thread control
// block begins with a pointer to itself, so the value stored at offset 0 of the
// %fs segment is the segment base. There is no "read the segment base" instruction
// a user-mode program may use (RDFSBASE needs a CPU feature and a kernel that
// enables it), which is why an ordinary load through the segment is the idiom.
//
// One register is enough for the whole access, in every model implemented here,
// which is why this is an addressing helper and not an instruction-selection
// pattern the way it is on arm64. That is a property of the machine and not a
// shortcut: x86-64 arithmetic takes a memory operand, so the offset never needs a
// register of its own. Only general-dynamic breaks that, because it ends in a
// call -- see below.
func (m *mc) materializeThreadSym(d Reg, sym string, off int64) {
	switch m.tlsModel {
	case TLSLocalExec:
		// The offset is a link-time constant, so it is the instruction's immediate
		// and any offset within the variable folds into the relocation's addend.
		m.emit(x64.MovFSZero(d.mreg()))         // d = thread pointer
		m.emit(x64.AddImm32(true, d.mreg(), 0)) // d += tpoff(sym) + off
		m.recordReloc(m.prog.Len()-4, sym, obj.R_X86_64_TPOFF32, off)

	case TLSInitialExec:
		// The offset lives in a GOT slot instead, so the add reads memory. The
		// relocation addend is -4 and not off: it corrects for the four
		// displacement bytes that sit between the fixup and the end of the
		// instruction RIP is measured from, and the slot is per-symbol, holding
		// tpoff(sym) with no room for an offset into the variable.
		m.emit(x64.MovFSZero(d.mreg()))   // d = thread pointer
		m.emit(x64.AddGotTPOff(d.mreg())) // d += [rip + sym@gottpoff]
		m.recordReloc(m.prog.Len()-4, sym, obj.R_X86_64_GOTTPOFF, -4)
		if off != 0 {
			// An offset into the variable is therefore a third instruction.
			if off < math.MinInt32 || off > math.MaxInt32 {
				m.fail(fmt.Errorf("amd64: offset %d into thread-local %q does not fit a 32-bit immediate", off, sym))
				return
			}
			m.emit(x64.AddImm32(true, d.mreg(), int32(off)))
		}

	default:
		// General-dynamic is `lea rdi, [rip + sym@tlsgd]` then `call
		// __tls_get_addr@PLT`, and it is blocked on two independent things.
		//
		// The call is the smaller one: it clobbers every caller-saved register, so
		// it cannot be improvised from an addressing helper like this one, which
		// runs after register allocation and has no way to spill around itself.
		// That wants the arm64 treatment -- a lowering pass turning the access into
		// an ordinary OTLSIndexAddr + OCall the allocator can see (arm64/lower.go's
		// lowerTLS).
		//
		// The blocker is in obj/, which has no x86-64 general-dynamic support at
		// all: no R_X86_64_TLSGD (19), DTPMOD64 (16) or DTPOFF64 (17) constants;
		// isTLSGdReloc (obj/elf.go:95) recognizes only the AArch64 types, so a
		// TLSGD relocation would not even mark its symbol STT_TLS in a relocatable
		// object; and the descriptor pair (obj/dynamic.go:895-899) is emitted with
		// R_AARCH64_TLS_DTPMOD64/DTPREL64 hardcoded. Emitting the sequence before
		// that lands would produce an object that links and then reads another
		// thread's memory, so this fails loudly instead.
		m.fail(fmt.Errorf("amd64: TLS model %s is unsupported (thread-local %q): "+
			"obj/ has no x86-64 general-dynamic relocations (R_X86_64_TLSGD/DTPMOD64/DTPOFF64), "+
			"so the sequence cannot be linked; use TLSInitialExec", m.tlsModel, sym))
	}
}
