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

// PrologueEmitter is an optional capability of a strategy: it emits code at the
// very start of every function, before the frame is set up. It is used for a
// stack-growth guard (Go-style growable stacks) — a check that branches to a
// runtime routine when the frame would overflow the current stack.
type PrologueEmitter interface {
	EmitPrologue(cx *PrologueContext)
}

// PrologueContext is the emission surface for EmitPrologue: raw instructions,
// scratch registers, symbol/runtime access, labels and branches, the frame size,
// and the retry label (the function's first instruction, to re-check after the
// stack has grown).
type PrologueContext struct {
	mc    *mc
	retry string
}

func (c *PrologueContext) FrameSize() int                { return c.mc.frame }
func (c *PrologueContext) Emit(word uint32)              { c.mc.emit(word) }
func (c *PrologueContext) Scratch() (a64.Reg, a64.Reg)   { return a64.Reg(16), a64.Reg(17) }
func (c *PrologueContext) MovImm(reg a64.Reg, v int64)   { c.mc.movImm(reg, v, true) }
func (c *PrologueContext) Label(name string)             { c.mc.prog.Label(name) }
func (c *PrologueContext) B(label string)                { c.mc.prog.B(label) }
func (c *PrologueContext) Bcond(cond a64.Cond, l string) { c.mc.prog.Bcond(cond, l) }
func (c *PrologueContext) RetryLabel() string            { return c.retry }

// Sym / TLSym materialize a (thread-local) symbol address into reg.
func (c *PrologueContext) Sym(reg a64.Reg, name string) {
	c.mc.materializeSym(reg, ir.Const{Kind: ir.ConstSym, Sym: name})
}
func (c *PrologueContext) TLSym(reg a64.Reg, name string) {
	c.mc.materializeSym(reg, ir.Const{Kind: ir.ConstSym, Sym: name, Thread: true})
}

// Call emits a call to a runtime symbol (bl + relocation).
func (c *PrologueContext) Call(name string) {
	c.mc.reloc(sanitize(name), obj.R_AARCH64_CALL26)
	c.mc.emit(a64.Bl(0))
}

// SubFrame computes reg = SP - frame for this function's frame size, handling
// frames too large for a single 12-bit immediate (so the guard never checks the
// wrong amount and silently fails to grow).
func (c *PrologueContext) SubFrame(reg a64.Reg) {
	c.Emit(a64.AddImm(true, reg, a64.SP, 0)) // reg = SP
	switch f := c.mc.frame; {
	case f == 0:
	case f <= 4095:
		c.Emit(a64.SubImm(true, reg, reg, uint32(f)))
	case f%4096 == 0 && f>>12 <= 4095:
		c.Emit(a64.SubImmLSL12(true, reg, reg, uint32(f>>12)))
	default:
		c.MovImm(a64.Reg(15), int64(f)) // x15 is a reserved scratch, free at entry
		c.Emit(a64.SubReg(true, reg, reg, a64.Reg(15)))
	}
}

// liveArgGP / liveArgFP are the registers holding live incoming values at entry:
// the argument registers the parameters consume (all eight for a variadic
// function, whose registers are not yet spilled), plus the indirect-result
// register x8 for a large aggregate return.
func (c *PrologueContext) liveArgGP() []a64.Reg {
	ngrn, _, _ := computeNamedCounts(c.mc.f)
	if c.mc.f.Variadic {
		ngrn = 8
	}
	var regs []a64.Reg
	for i := 0; i < ngrn; i++ {
		regs = append(regs, a64.Reg(i))
	}
	if a := c.mc.f.RetAgg; a != nil && classifyAgg(a).kind == aggMemory {
		regs = append(regs, a64.Reg(8))
	}
	return regs
}

func (c *PrologueContext) liveArgFP() []a64.Reg {
	_, nsrn, _ := computeNamedCounts(c.mc.f)
	if c.mc.f.Variadic {
		nsrn = 8
	}
	var regs []a64.Reg
	for j := 0; j < nsrn; j++ {
		regs = append(regs, a64.Reg(j))
	}
	return regs
}

// PushCallerState reserves stack space and saves fp/lr and every live incoming
// argument register — general-purpose and floating-point — so a runtime call
// made in the prologue, before the frame exists, preserves them all. Pass the
// returned size to PopCallerState. (A copying runtime relocates this saved area
// like any other frame, using the stack map.)
func (c *PrologueContext) PushCallerState() int {
	gp, fp := c.liveArgGP(), c.liveArgFP()
	size := roundUp(16+8*(len(gp)+len(fp)), 16)
	c.Emit(a64.SubImm(true, a64.SP, a64.SP, uint32(size)))
	c.Emit(a64.StrImm(true, a64.Reg(29), a64.SP, 0))
	c.Emit(a64.StrImm(true, a64.Reg(30), a64.SP, 8))
	off := uint32(16)
	for _, r := range gp {
		c.Emit(a64.StrImm(true, r, a64.SP, off))
		off += 8
	}
	for _, r := range fp {
		c.Emit(a64.StrFP(true, r, a64.SP, off))
		off += 8
	}
	return size
}

// PopCallerState restores what PushCallerState saved and releases the space.
func (c *PrologueContext) PopCallerState(size int) {
	gp, fp := c.liveArgGP(), c.liveArgFP()
	c.Emit(a64.LdrImm(true, a64.Reg(29), a64.SP, 0))
	c.Emit(a64.LdrImm(true, a64.Reg(30), a64.SP, 8))
	off := uint32(16)
	for _, r := range gp {
		c.Emit(a64.LdrImm(true, r, a64.SP, off))
		off += 8
	}
	for _, r := range fp {
		c.Emit(a64.LdrFP(true, r, a64.SP, off))
		off += 8
	}
	c.Emit(a64.AddImm(true, a64.SP, a64.SP, uint32(size)))
}

// StackGrowth is a strategy for Go-style growable stacks. Each function's
// prologue compares the space it needs against a per-thread stack limit; if the
// frame would overflow, it calls a runtime routine that reallocates the stack,
// copies the live frames over (using the typed stack maps to fix up pointers),
// and returns, whereupon the prologue re-checks and proceeds. It is also a GC
// strategy: safepoints still record stack maps (needed for the copy), but it
// emits no poll code of its own.
type StackGrowth struct {
	LimitSym     string // holds the current stack limit (a low address)
	MorestackSym string // the runtime routine that grows and copies the stack
	ThreadLocal  bool   // LimitSym is per-thread
}

// EmitSafepoint records nothing extra — the emitter already records the stack
// map; StackGrowth needs no poll code.
func (s StackGrowth) EmitSafepoint(*GCContext) {}

func (s StackGrowth) EmitPrologue(cx *PrologueContext) {
	limit, cur := cx.Scratch() // x16, x17

	if s.ThreadLocal {
		cx.TLSym(limit, s.LimitSym)
	} else {
		cx.Sym(limit, s.LimitSym)
	}
	cx.Emit(a64.LdrImm(true, limit, limit, 0)) // limit = *LimitSym
	cx.SubFrame(cur)                           // cur = SP - frame (any frame size)
	cx.Emit(a64.CmpReg(true, cur, limit))
	cx.Bcond(a64.HI, "__cg12_stack_ok") // SP-frame > limit: enough space

	// Grow path: preserve every live argument register (GP and FP) plus fp/lr
	// across the runtime call, pass the frame size, then re-check from the top.
	size := cx.PushCallerState()
	cx.MovImm(a64.Reg(0), int64(cx.FrameSize())) // frame size -> x0 (runtime arg)
	cx.Call(s.MorestackSym)
	cx.PopCallerState(size)
	cx.B(cx.RetryLabel())

	cx.Label("__cg12_stack_ok")
}

// GCRoot is a managed reference live at a safepoint, either in a register or at
// a byte offset from the frame pointer (x29). With the default force-to-stack
// policy, roots at safepoints are always frame slots. Type is the value's
// runtime type descriptor (0 if untyped).
type GCRoot struct {
	InReg  bool
	Reg    uint8
	Offset int32
	Type   uint32
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
			roots = append(roots, GCRoot{InReg: true, Reg: uint8(mreg(Reg(t.Reg))), Type: t.GCType})
		} else {
			roots = append(roots, GCRoot{Offset: int32(m.spillBase + t.Slot), Type: t.GCType})
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
