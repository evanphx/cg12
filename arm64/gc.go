package arm64

import (
	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// GCStrategy is a pluggable garbage-collector policy that emits GC support code
// late — during machine-code emission — so that neither the language frontend
// nor the backend's ordinary instruction selection needs to know about the GC.
// A nil strategy emits nothing, leaving safepoints as code-free stack-map markers.
//
// The emitter invokes the strategy at each garbage-collection point it exposes;
// today that is the explicit safepoint (OSafepoint). The strategy emits through
// the GCContext, and the emitter records the stack-map entry at the resulting PC.
type GCStrategy interface {
	// EmitSafepoint emits code for one safepoint (e.g. a cooperative poll).
	EmitSafepoint(cx *GCContext)
}

// GCRoot is a managed reference live at a safepoint, either in a register or at
// a byte offset from the frame pointer (x29). With the default force-to-stack
// policy, roots at safepoints are always frame slots.
type GCRoot struct {
	InReg  bool
	Reg    uint8
	Offset int32
}

// GCContext is the emission surface handed to a GCStrategy. It exposes raw
// instruction emission, scratch registers that are free at a safepoint, symbol
// materialization and runtime calls, and the live roots.
type GCContext struct {
	mc    *mc
	roots []GCRoot
}

// Emit appends a raw AArch64 instruction word.
func (c *GCContext) Emit(word uint32) { c.mc.emit(word) }

// Scratch returns two caller-clobberable registers (x16/x17) that are free at a
// safepoint — nothing live is held in them between instructions.
func (c *GCContext) Scratch() (a64.Reg, a64.Reg) { return a64.Reg(16), a64.Reg(17) }

// Sym materializes the address of a symbol into reg (adrp/add + relocations).
func (c *GCContext) Sym(reg a64.Reg, name string) {
	c.mc.materializeSym(reg, ir.Const{Kind: ir.ConstSym, Sym: name})
}

// TLSym materializes the address of a thread-local symbol into reg (the TLS ABI
// sequence). Use it for per-thread GC state — a poll flag, an allocation buffer.
func (c *GCContext) TLSym(reg a64.Reg, name string) {
	c.mc.materializeSym(reg, ir.Const{Kind: ir.ConstSym, Sym: name, Thread: true})
}

// Call emits a call to a runtime symbol (bl + relocation). Any state the runtime
// needs is its own concern; this just transfers control.
func (c *GCContext) Call(name string) {
	c.mc.reloc(sanitize(name), obj.R_AARCH64_CALL26)
	c.mc.emit(a64.Bl(0))
}

// Roots returns the managed references live at this safepoint.
func (c *GCContext) Roots() []GCRoot { return c.roots }

// gcRoots builds the public root list for a safepoint instruction.
func (m *mc) gcRoots(in *ir.Instr) []GCRoot {
	ids := m.alloc.safeRoots[in]
	roots := make([]GCRoot, 0, len(ids))
	for _, id := range ids {
		t := m.f.Temps[id]
		if t.Reg != ir.NoReg {
			roots = append(roots, GCRoot{InReg: true, Reg: uint8(mreg(Reg(t.Reg)))})
		} else {
			roots = append(roots, GCRoot{Offset: int32(m.spillBase + t.Slot)})
		}
	}
	return roots
}

// PollStrategy emits a cooperative-polling safepoint: load a byte from FlagSym
// and, if it is nonzero, call StubSym (the runtime's safepoint handler, which
// returns to the following instruction). The stack map is recorded at that
// return point, so a collection entered through the poll can scan this frame.
type PollStrategy struct {
	FlagSym     string // a byte flag the runtime sets to request a collection
	StubSym     string // the runtime entry point called when the flag is set
	ThreadLocal bool   // FlagSym is a per-thread flag (addressed via TLS)
}

func (p PollStrategy) EmitSafepoint(cx *GCContext) {
	s, _ := cx.Scratch() // x16
	if p.ThreadLocal {
		cx.TLSym(s, p.FlagSym) // &flag (thread-local) -> x16
	} else {
		cx.Sym(s, p.FlagSym) // &flag -> x16
	}
	cx.Emit(a64.LdrbImm(s, s, 0)) // ldrb w16, [x16]
	cx.Emit(a64.Cbz(false, s, 8)) // cbz w16, +8  (skip the call when clear)
	cx.Call(p.StubSym)            // bl stub
}
