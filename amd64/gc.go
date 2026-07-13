package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// GCStrategy is a pluggable garbage-collector policy that emits GC support code
// late — during machine-code emission — so that neither the language frontend
// nor ordinary instruction selection needs to know about the GC. A nil strategy
// emits nothing, leaving safepoints as code-free stack-map markers.
type GCStrategy interface {
	// EmitSafepoint emits code for one safepoint (e.g. a cooperative poll).
	EmitSafepoint(cx *GCContext)
}

// PrologueEmitter is an optional capability of a strategy: it emits code at the
// very start of every function, before the frame is set up — used for a
// stack-growth guard (Go-style growable stacks).
type PrologueEmitter interface {
	EmitPrologue(cx *PrologueContext)
}

// GCRoot is a managed reference live at a safepoint, either in a register or at a
// byte offset from the frame pointer (rbp). With the default force-to-stack
// policy, safepoint roots are always frame slots. Type is the value's runtime
// type descriptor (0 if untyped).
type GCRoot struct {
	InReg  bool
	Reg    uint8
	Offset int32
	Type   uint32
}

// GCContext is the emission surface handed to a GCStrategy at a safepoint: raw
// instruction bytes, scratch registers free at the safepoint, symbol
// materialization and runtime calls, unique labels, and the live roots.
type GCContext struct {
	mc    *mc
	roots []GCRoot
}

// Emit appends raw x86-64 instruction bytes (build them with the x64 package).
func (c *GCContext) Emit(b []byte) { c.mc.emit(b) }

// Scratch returns two caller-clobberable registers (r10/r11) free at a safepoint.
func (c *GCContext) Scratch() (Reg, Reg) { return gpScratch0, gpScratch1 }

// Sym / TLSym materialize a (thread-local) symbol address into reg.
func (c *GCContext) Sym(reg Reg, name string)   { c.mc.materializeSym(reg, sanitize(name), 0, false) }
func (c *GCContext) TLSym(reg Reg, name string) { c.mc.materializeSym(reg, sanitize(name), 0, true) }

// Call emits a call to a runtime symbol (call + PLT32 relocation).
func (c *GCContext) Call(name string) {
	c.mc.emit(x64.CallRel(0))
	c.mc.recordReloc(c.mc.prog.Len()-4, sanitize(name), obj.R_X86_64_PLT32, -4)
}

// Label emits a unique label and returns its name (for skip branches).
func (c *GCContext) Label() string {
	c.mc.gcSeq++
	name := fmt.Sprintf("__cg12_gc%d", c.mc.gcSeq)
	return name
}

// Jcc / Bind emit a conditional jump to, and the definition of, a label.
func (c *GCContext) Jcc(cond x64.Cond, label string) { c.mc.prog.Jcc(cond, label) }
func (c *GCContext) Bind(label string)               { c.mc.prog.Label(label) }

// Roots returns the managed references live at this safepoint.
func (c *GCContext) Roots() []GCRoot { return c.roots }

// gcRoots builds the public root list for a safepoint instruction.
func (m *mc) gcRoots(in *ir.Instr) []GCRoot {
	ids := m.alloc.safeRoots[in]
	roots := make([]GCRoot, 0, len(ids))
	for _, id := range ids {
		t := m.f.Temps[id]
		if t.Reg != ir.NoReg {
			roots = append(roots, GCRoot{InReg: true, Reg: uint8(Reg(t.Reg).mreg()), Type: t.GCType})
		} else {
			roots = append(roots, GCRoot{Offset: int32(-(m.spillBase + 8 + t.Slot)), Type: t.GCType})
		}
	}
	return roots
}

// PrologueContext is the emission surface for EmitPrologue: raw instructions,
// scratch registers, symbol/runtime access, labels, branches, the frame size, and
// the retry label (the function's first instruction, to re-check after the stack
// has grown).
type PrologueContext struct {
	mc    *mc
	retry string
}

func (c *PrologueContext) FrameSize() int              { return c.mc.frame }
func (c *PrologueContext) Emit(b []byte)               { c.mc.emit(b) }
func (c *PrologueContext) Scratch() (Reg, Reg)         { return gpScratch0, gpScratch1 }
func (c *PrologueContext) MovImm(reg Reg, v int64)     { c.mc.movImm(reg, v, true) }
func (c *PrologueContext) Label(name string)           { c.mc.prog.Label(name) }
func (c *PrologueContext) Jmp(label string)            { c.mc.prog.Jmp(label) }
func (c *PrologueContext) Jcc(cond x64.Cond, l string) { c.mc.prog.Jcc(cond, l) }
func (c *PrologueContext) RetryLabel() string          { return c.retry }

func (c *PrologueContext) Sym(reg Reg, name string) {
	c.mc.materializeSym(reg, sanitize(name), 0, false)
}
func (c *PrologueContext) TLSym(reg Reg, name string) {
	c.mc.materializeSym(reg, sanitize(name), 0, true)
}

// Call emits a call to a runtime symbol (call + PLT32 relocation).
func (c *PrologueContext) Call(name string) {
	c.mc.emit(x64.CallRel(0))
	c.mc.recordReloc(c.mc.prog.Len()-4, sanitize(name), obj.R_X86_64_PLT32, -4)
}

// SubFrame computes reg = RSP - frame for this function's frame size.
func (c *PrologueContext) SubFrame(reg Reg) {
	c.Emit(x64.MovReg(true, reg.mreg(), RSP.mreg()))
	if c.mc.frame != 0 {
		c.Emit(x64.SubImm(true, reg.mreg(), int32(c.mc.frame)))
	}
}

// liveArgGP / liveArgFP are the argument registers holding live incoming values
// at entry: the ones the parameters (and a MEMORY-return sret pointer) consume —
// all of them for a variadic function, whose registers are not yet spilled.
func (c *PrologueContext) liveArgGP() []Reg {
	ngp, _, _ := c.mc.namedCounts()
	if c.mc.f.RetAgg != nil && classifyAgg(c.mc.f.RetAgg).memory {
		ngp++ // the sret pointer occupies rdi
	}
	if c.mc.f.Variadic {
		ngp = len(argGP)
	}
	if ngp > len(argGP) {
		ngp = len(argGP)
	}
	return append([]Reg(nil), argGP[:ngp]...)
}

func (c *PrologueContext) liveArgFP() []Reg {
	_, nfp, _ := c.mc.namedCounts()
	if c.mc.f.Variadic {
		nfp = len(argFP)
	}
	return append([]Reg(nil), argFP[:nfp]...)
}

// PushCallerState reserves stack space and saves every live incoming argument
// register (GP and FP) so a runtime call made in the prologue, before the frame
// exists, preserves them. The return address is already on the stack and rbp is
// callee-saved, so neither needs saving. Pass the returned size to
// PopCallerState. (A copying runtime relocates this saved area like any frame.)
func (c *PrologueContext) PushCallerState() int {
	gp, fp := c.liveArgGP(), c.liveArgFP()
	// Entry rsp ≡ 8 (mod 16); keep the runtime call 16-aligned by sizing so rsp
	// becomes ≡ 0.
	size := roundUp(8*(len(gp)+len(fp)), 16) + 8
	c.Emit(x64.SubImm(true, RSP.mreg(), int32(size)))
	off := int32(0)
	for _, r := range gp {
		c.Emit(x64.Store(64, r.mreg(), x64.At(RSP.mreg(), off)))
		off += 8
	}
	for _, r := range fp {
		c.Emit(x64.MovsdStore(r.mreg(), x64.At(RSP.mreg(), off)))
		off += 8
	}
	return size
}

// PopCallerState restores what PushCallerState saved and releases the space.
func (c *PrologueContext) PopCallerState(size int) {
	gp, fp := c.liveArgGP(), c.liveArgFP()
	off := int32(0)
	for _, r := range gp {
		c.Emit(x64.Load(true, r.mreg(), x64.At(RSP.mreg(), off)))
		off += 8
	}
	for _, r := range fp {
		c.Emit(x64.MovsdLoad(r.mreg(), x64.At(RSP.mreg(), off)))
		off += 8
	}
	c.Emit(x64.AddImm(true, RSP.mreg(), int32(size)))
}

// RecordArgRoots records a growth safepoint at the current PC (the morestack
// return address) whose roots are the managed-reference parameters, located in
// the guard's save area relative to the stack pointer. It must follow
// PushCallerState (whose layout it mirrors), letting a copying runtime fix up a
// growing function's pointer arguments — saved, but not yet in a frame.
func (c *PrologueContext) RecordArgRoots() {
	off := map[Reg]int{}
	pos := 0
	for _, r := range c.liveArgGP() {
		off[r] = pos
		pos += 8
	}
	for _, r := range c.liveArgFP() {
		off[r] = pos
		pos += 8
	}

	var roots []rootLoc
	for i := range c.mc.f.Start.Instrs {
		in := &c.mc.f.Start.Instrs[i]
		if in.Op != ir.OPar || in.To.Kind != ir.RefTemp || len(in.Args) == 0 {
			continue
		}
		t := c.mc.f.Temps[in.To.ID]
		if !t.GCRef || in.Args[0].Kind != ir.RefTemp {
			continue
		}
		reg := Reg(c.mc.f.Temps[in.Args[0].ID].Reg)
		if o, ok := off[reg]; ok {
			roots = append(roots, rootLoc{kind: rootSP, val: int32(o), typ: t.GCType})
		}
	}
	if len(roots) > 0 {
		c.mc.safepoints = append(c.mc.safepoints, safepoint{pc: uint64(c.mc.prog.Len()), roots: roots})
	}
}

// PollStrategy emits a cooperative-polling safepoint: load a byte from FlagSym
// and, if nonzero, call StubSym (the runtime's safepoint handler, which returns
// to the following instruction). The stack map is recorded at that return point.
type PollStrategy struct {
	FlagSym     string // a byte flag the runtime sets to request a collection
	StubSym     string // the runtime entry point called when the flag is set
	ThreadLocal bool   // FlagSym is a per-thread flag (addressed via TLS)
}

func (p PollStrategy) EmitSafepoint(cx *GCContext) {
	s, _ := cx.Scratch() // r10
	if p.ThreadLocal {
		cx.TLSym(s, p.FlagSym)
	} else {
		cx.Sym(s, p.FlagSym)
	}
	cx.Emit(x64.MovzxLoadByte(false, s.mreg(), x64.At(s.mreg(), 0))) // r10d = flag byte
	cx.Emit(x64.TestReg(false, s.mreg(), s.mreg()))
	skip := cx.Label()
	cx.Jcc(x64.E, skip) // clear: skip the call
	cx.Call(p.StubSym)
	cx.Bind(skip)
}

// StackGrowth is a strategy for Go-style growable stacks. Each function's
// prologue compares the space it needs against a stack limit; if the frame would
// overflow, it calls a runtime routine that reallocates the stack, copies the
// live frames over (using the typed stack maps to fix up pointers), and returns,
// whereupon the prologue re-checks and proceeds. It is also a GC strategy:
// safepoints still record stack maps (needed for the copy), but it emits no poll.
type StackGrowth struct {
	LimitSym     string // holds the current stack limit (a low address)
	MorestackSym string // the runtime routine that grows and copies the stack
	ThreadLocal  bool   // LimitSym is per-thread
}

func (s StackGrowth) EmitSafepoint(*GCContext) {}

func (s StackGrowth) EmitPrologue(cx *PrologueContext) {
	limit, cur := cx.Scratch() // r10, r11
	if s.ThreadLocal {
		cx.TLSym(limit, s.LimitSym)
	} else {
		cx.Sym(limit, s.LimitSym)
	}
	cx.Emit(x64.Load(true, limit.mreg(), x64.At(limit.mreg(), 0))) // limit = *LimitSym
	cx.SubFrame(cur)                                               // cur = rsp - frame
	cx.Emit(x64.CmpReg(true, cur.mreg(), limit.mreg()))
	cx.Jcc(x64.A, "__cg12_stack_ok") // rsp-frame > limit (unsigned): enough space

	// Grow path: preserve every live argument register across the runtime call,
	// pass the frame size in rdi, then re-check from the top.
	size := cx.PushCallerState()
	cx.MovImm(argGP[0], int64(cx.FrameSize())) // frame size -> rdi (runtime arg)
	cx.Call(s.MorestackSym)
	cx.RecordArgRoots() // describe the saved pointer arguments to the runtime
	cx.PopCallerState(size)
	cx.Jmp(cx.RetryLabel())

	cx.Label("__cg12_stack_ok")
}
