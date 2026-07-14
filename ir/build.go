package ir

import "fmt"

// This file is the library-first construction API: the intended way to build IR
// programmatically. Every result-producing helper returns the Ref naming its
// result, so expressions compose directly:
//
//	f := m.NewFunc("add", ir.ClsW)
//	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
//	e := f.Entry()
//	e.Ret(e.Add(ir.ClsW, a, b))

// NewModule returns an empty module.
func NewModule() *Module { return &Module{} }

// NewFunc creates a value-returning function with the given return class.
func (m *Module) NewFunc(name string, retty Cls) *Func {
	f := &Func{mod: m, Name: name, HasRet: true, Retty: retty}
	m.Funcs = append(m.Funcs, f)
	return f
}

// NewFuncVoid creates a function that returns no value.
func (m *Module) NewFuncVoid(name string) *Func {
	f := &Func{mod: m, Name: name}
	m.Funcs = append(m.Funcs, f)
	return f
}

// AddType registers an aggregate type and returns a ref to it (for calls and
// typed allocations).
func (m *Module) AddType(t *AggType) Ref {
	id := len(m.Types)
	m.Types = append(m.Types, t)
	return typeRef(id)
}

// Export marks a function as externally visible and returns it for chaining.
func (f *Func) Export() *Func { f.Linkage.Export = true; return f }

// Param appends a parameter of the given class and returns its temporary.
func (f *Func) Param(name string, cls Cls) Ref {
	r := f.newTemp(name, cls)
	f.Params = append(f.Params, f.Temps[r.ID])
	return r
}

// ParamRef appends a managed-reference (GC root) parameter: a pointer-class
// value flagged as a garbage-collector root.
func (f *Func) ParamRef(name string) Ref {
	r := f.Param(name, ClsP)
	f.Temps[r.ID].GCRef = true
	return r
}

// MarkGCRef flags a temporary as a managed heap reference (a GC root), so its
// location is reported in stack maps. It is a no-op for non-temporary refs.
func (f *Func) MarkGCRef(r Ref) Ref {
	if r.Kind == RefTemp {
		f.Temps[r.ID].GCRef = true
	}
	return r
}

// MarkGCRefType flags a temporary as a managed reference and tags it with a type
// descriptor, carried into the stack map for the runtime to interpret.
func (f *Func) MarkGCRefType(r Ref, typeID uint32) Ref {
	if r.Kind == RefTemp {
		t := f.Temps[r.ID]
		t.GCRef = true
		t.GCType = typeID
	}
	return r
}

// NewBlock creates a fresh basic block. The first block created becomes the
// function entry.
func (f *Func) NewBlock(name string) *Block {
	if name == "" {
		f.nameSeq++
		name = fmt.Sprintf("b%d", f.nameSeq)
	}
	b := &Block{fn: f, Name: name, ID: len(f.Blocks)}
	f.Blocks = append(f.Blocks, b)
	if f.Start == nil {
		f.Start = b
	}
	return b
}

// Entry returns the function's entry block, creating one if none exists.
func (f *Func) Entry() *Block {
	if f.Start == nil {
		return f.NewBlock("start")
	}
	return f.Start
}

func (f *Func) newTemp(name string, cls Cls) Ref {
	if name == "" {
		f.nameSeq++
		name = fmt.Sprintf("t%d", f.nameSeq)
	}
	t := &Temp{ID: len(f.Temps), Name: name, Cls: cls, Slot: -1, Reg: NoReg}
	f.Temps = append(f.Temps, t)
	return tempRef(t.ID)
}

// NewTemp creates a fresh temporary of the given class and returns its ref. It
// is the exported entry point backends use to introduce temporaries (copies,
// spill reloads, pre-coloured ABI registers) during lowering.
func (f *Func) NewTemp(name string, cls Cls) Ref { return f.newTemp(name, cls) }

func (f *Func) internConst(c Const) Ref {
	k := constKey{c.Kind, c.Cls, c.Int, c.Flt, c.Sym}
	if f.constIdx == nil {
		f.constIdx = map[constKey]int{}
	}
	if id, ok := f.constIdx[k]; ok {
		return constRef(id)
	}
	id := len(f.Consts)
	f.Consts = append(f.Consts, c)
	f.constIdx[k] = id
	return constRef(id)
}

// ConstInt returns an integer constant of the given class.
func (f *Func) ConstInt(cls Cls, v int64) Ref {
	return f.internConst(Const{Kind: ConstInt, Cls: cls, Int: v})
}

// Word and Long are shorthands for integer constants of class W and L.
func (f *Func) Word(v int64) Ref { return f.ConstInt(ClsW, v) }
func (f *Func) Long(v int64) Ref { return f.ConstInt(ClsL, v) }

// Single and Double return floating-point constants.
func (f *Func) Single(v float64) Ref {
	return f.internConst(Const{Kind: ConstFloat, Cls: ClsS, Flt: v})
}
func (f *Func) Double(v float64) Ref {
	return f.internConst(Const{Kind: ConstFloat, Cls: ClsD, Flt: v})
}

// Sym returns the address of a global symbol plus an offset (pointer class).
func (f *Func) Sym(name string, off int64) Ref {
	return f.internConst(Const{Kind: ConstSym, Cls: ClsP, Sym: name, Int: off})
}

// SymClass is Sym with an explicit class. Text IL types every pointer as l, so
// the parser records a symbol at the class its context expects (ClsL in QBE
// programs) rather than the fluent API's abstract pointer class.
func (f *Func) SymClass(name string, off int64, cls Cls) Ref {
	return f.internConst(Const{Kind: ConstSym, Cls: cls, Sym: name, Int: off})
}

// ThreadSym returns the address of a thread-local symbol: each thread sees its
// own instance, addressed through the platform's TLS ABI. Used for per-thread GC
// state such as a poll flag or an allocation buffer.
func (f *Func) ThreadSym(name string) Ref {
	return f.internConst(Const{Kind: ConstSym, Cls: ClsP, Sym: name, Thread: true})
}

// --- instruction emission -------------------------------------------------

// At sets the source position stamped onto instructions emitted next, driving
// debug-info emission. It returns the block for chaining, e.g.
// b.At(pos).Add(...). The position persists until changed.
func (b *Block) At(pos SrcPos) *Block {
	b.curPos = pos
	return b
}

func (b *Block) emit(op Op, cls Cls, args ...Ref) Ref {
	in := Instr{Op: op, Cls: cls, Args: args, Pos: b.curPos}
	var res Ref
	if op.HasResult() {
		res = b.fn.newTemp("", cls)
		in.To = res
	}
	b.Instrs = append(b.Instrs, in)
	return res
}

// Binary arithmetic and bitwise operations.
func (b *Block) Add(cls Cls, x, y Ref) Ref  { return b.emit(OAdd, cls, x, y) }
func (b *Block) Sub(cls Cls, x, y Ref) Ref  { return b.emit(OSub, cls, x, y) }
func (b *Block) Mul(cls Cls, x, y Ref) Ref  { return b.emit(OMul, cls, x, y) }
func (b *Block) Div(cls Cls, x, y Ref) Ref  { return b.emit(ODiv, cls, x, y) }
func (b *Block) UDiv(cls Cls, x, y Ref) Ref { return b.emit(OUDiv, cls, x, y) }
func (b *Block) Rem(cls Cls, x, y Ref) Ref  { return b.emit(ORem, cls, x, y) }
func (b *Block) URem(cls Cls, x, y Ref) Ref { return b.emit(OURem, cls, x, y) }
func (b *Block) And(cls Cls, x, y Ref) Ref  { return b.emit(OAnd, cls, x, y) }
func (b *Block) Or(cls Cls, x, y Ref) Ref   { return b.emit(OOr, cls, x, y) }
func (b *Block) Xor(cls Cls, x, y Ref) Ref  { return b.emit(OXor, cls, x, y) }
func (b *Block) Shl(cls Cls, x, y Ref) Ref  { return b.emit(OShl, cls, x, y) }
func (b *Block) Shr(cls Cls, x, y Ref) Ref  { return b.emit(OShr, cls, x, y) }
func (b *Block) Sar(cls Cls, x, y Ref) Ref  { return b.emit(OSar, cls, x, y) }

// Neg negates an integer or float value.
func (b *Block) Neg(cls Cls, x Ref) Ref { return b.emit(ONeg, cls, x) }

// Clz counts the leading zero bits of x at cls's width (32 or 64).
func (b *Block) Clz(cls Cls, x Ref) Ref { return b.emit(OClz, cls, x) }

// Cmp emits a comparison with the given predicate; the result has class resCls
// (an integer class) and the operands are compared at their own class.
func (b *Block) Cmp(pred Cmp, resCls Cls, x, y Ref) Ref {
	res := b.fn.newTemp("", resCls)
	b.Instrs = append(b.Instrs, Instr{Op: OCmp, Cls: resCls, To: res, Args: []Ref{x, y}, Cmp: pred})
	return res
}

// Copy returns a copy of x, optionally re-typed to cls.
func (b *Block) Copy(cls Cls, x Ref) Ref { return b.emit(OCopy, cls, x) }

// Cast reinterprets the bits of x as class cls (integer <-> float, equal width).
func (b *Block) Cast(cls Cls, x Ref) Ref { return b.emit(OCast, cls, x) }

// Integer width conversions (result class cls).
func (b *Block) Extsb(cls Cls, x Ref) Ref { return b.emit(OExtsb, cls, x) } // sign-extend byte
func (b *Block) Extub(cls Cls, x Ref) Ref { return b.emit(OExtub, cls, x) } // zero-extend byte
func (b *Block) Extsh(cls Cls, x Ref) Ref { return b.emit(OExtsh, cls, x) } // sign-extend halfword
func (b *Block) Extuh(cls Cls, x Ref) Ref { return b.emit(OExtuh, cls, x) } // zero-extend halfword
func (b *Block) Extsw(cls Cls, x Ref) Ref { return b.emit(OExtsw, cls, x) } // sign-extend word
func (b *Block) Extuw(cls Cls, x Ref) Ref { return b.emit(OExtuw, cls, x) } // zero-extend word

// Float width conversions.
func (b *Block) Exts(x Ref) Ref   { return b.emit(OExts, ClsD, x) }   // single -> double
func (b *Block) Truncd(x Ref) Ref { return b.emit(OTruncd, ClsS, x) } // double -> single

// Float/integer conversions (result class cls).
func (b *Block) Stosi(cls Cls, x Ref) Ref { return b.emit(OStosi, cls, x) } // float -> signed int
func (b *Block) Stoui(cls Cls, x Ref) Ref { return b.emit(OStoui, cls, x) } // float -> unsigned int
func (b *Block) Sltof(cls Cls, x Ref) Ref { return b.emit(OSltof, cls, x) } // signed int -> float
func (b *Block) Ultof(cls Cls, x Ref) Ref { return b.emit(OUltof, cls, x) } // unsigned int -> float

// Load reads a full-width value of class cls from address addr.
// RegVar declares a variable bound to a specific machine register: a Load of it
// reads that register and a Store to it writes it. reg is the backend's physical
// register number, so a RegVar makes the IR architecture-specific — it is for
// low-level runtime code (a stack switch, a trampoline) that must touch the
// stack pointer, frame pointer, or other registers directly. Binding a register
// the allocator uses is the author's responsibility; the non-allocatable
// registers (sp, fp, lr, the platform register) are always safe.
func (f *Func) RegVar(name string, reg int) Ref {
	return Ref{Kind: RefReg, ID: uint32(reg)}
}

func (b *Block) Load(cls Cls, addr Ref) Ref {
	if addr.Kind == RefReg {
		return b.emit(OGetReg, cls, addr) // read a machine register
	}
	var op Op
	switch cls {
	case ClsW:
		op = OLoaduw
	case ClsL, ClsP:
		op = OLoadl // a pointer loads at its canonical width; LowerPointers narrows it
	case ClsS:
		op = OLoads
	case ClsD:
		op = OLoadd
	case ClsQ:
		op = OLoadq
	}
	return b.emit(op, cls, addr)
}

// LoadSub reads a sub-word value from addr, extended into class cls per the
// sub-class's signedness.
func (b *Block) LoadSub(cls Cls, sub SubCls, addr Ref) Ref {
	var op Op
	switch sub {
	case SubB:
		op = OLoadsb
	case SubUB:
		op = OLoadub
	case SubH:
		op = OLoadsh
	case SubUH:
		op = OLoaduh
	case SubW:
		op = OLoadsw
	case SubL:
		op = OLoadl
	case SubS:
		op = OLoads
	case SubD:
		op = OLoadd
	}
	return b.emit(op, cls, addr)
}

// Store writes val (its class fixes the width) to address addr.
func (b *Block) Store(val, addr Ref) {
	if addr.Kind == RefReg {
		b.emit(OSetReg, b.fn.ClassOf(val), val, addr) // write a machine register
		return
	}
	cls := b.fn.ClassOf(val)
	var op Op
	switch cls {
	case ClsW:
		op = OStorew
	case ClsL, ClsP:
		op = OStorel // a pointer stores at its canonical width; LowerPointers narrows it
	case ClsS:
		op = OStores
	case ClsD:
		op = OStored
	case ClsQ:
		op = OStoreq
	}
	b.emit(op, cls, val, addr)
}

// StoreSub writes the low bytes of val to addr at the given sub-word width.
func (b *Block) StoreSub(sub SubCls, val, addr Ref) {
	var op Op
	switch sub {
	case SubB, SubUB:
		op = OStoreb
	case SubH, SubUH:
		op = OStoreh
	case SubW:
		op = OStorew
	case SubL:
		op = OStorel
	case SubS:
		op = OStores
	case SubD:
		op = OStored
	}
	b.emit(op, b.fn.ClassOf(val), val, addr)
}

// Alloc reserves size bytes on the stack at the given alignment (4, 8, or 16)
// and returns the address (pointer class ClsP).
func (b *Block) Alloc(align, size int) Ref {
	var op Op
	switch align {
	case 4:
		op = OAlloc4
	case 8:
		op = OAlloc8
	case 16:
		op = OAlloc16
	default:
		panic(fmt.Sprintf("ir: invalid alloc alignment %d", align))
	}
	return b.emit(op, ClsP, b.fn.Long(int64(size)))
}

// AllocN allocates size bytes on the stack at run time (a variable-length
// array). size must already be rounded up to a multiple of 16 so the stack
// pointer stays aligned. The result is a 16-aligned pointer.
func (b *Block) AllocN(size Ref) Ref {
	return b.emit(OAllocN, ClsP, size)
}

// Call invokes callee with args and returns the result (class retCls). For a
// void call use CallVoid.
func (b *Block) Call(retCls Cls, callee Ref, args ...Ref) Ref {
	in := Instr{Op: OCall, Cls: retCls, Args: append([]Ref{callee}, args...), Pos: b.curPos}
	res := b.fn.newTemp("", retCls)
	in.To = res
	b.Instrs = append(b.Instrs, in)
	return res
}

// Safepoint marks a garbage-collector safepoint that is not a call (an inlined
// allocation fast path, a loop back-edge poll, ...). The backend emits a stack
// map of the managed references live here, but no machine code.
func (b *Block) Safepoint() { b.emit(OSafepoint, ClsW) }

// VaStart initialises a variadic argument list at address ap.
func (b *Block) VaStart(ap Ref) { b.emit(OVaStart, ClsW, ap) }

// VaArg fetches the next variadic argument of class cls from the list at ap.
func (b *Block) VaArg(cls Cls, ap Ref) Ref { return b.emit(OVaArg, cls, ap) }

// CallVoid invokes callee for effect only.
func (b *Block) CallVoid(callee Ref, args ...Ref) {
	in := Instr{Op: OCall, Cls: ClsW, Args: append([]Ref{callee}, args...), Pos: b.curPos}
	b.Instrs = append(b.Instrs, in)
}

// TailCall makes callee a mandatory tail call returning class retCls: the call
// is marked Tail and the block returns its result. The backend must emit a real
// tail call or error (see [Instr].Tail). It terminates the block.
func (b *Block) TailCall(retCls Cls, callee Ref, args ...Ref) {
	r := b.Call(retCls, callee, args...)
	b.Instrs[len(b.Instrs)-1].Tail = true
	b.Ret(r)
}

// TailCallVoid is [Block.TailCall] for a callee returning nothing.
func (b *Block) TailCallVoid(callee Ref, args ...Ref) {
	b.CallVoid(callee, args...)
	b.Instrs[len(b.Instrs)-1].Tail = true
	b.RetVoid()
}

// --- terminators ----------------------------------------------------------

// Ret returns v from the function.
func (b *Block) Ret(v Ref) { b.Jmp = Jmp{Kind: JmpRet, Arg: v} }

// RetVoid returns with no value.
func (b *Block) RetVoid() { b.Jmp = Jmp{Kind: JmpRet} }

// Goto adds an unconditional jump to target.
func (b *Block) Goto(target *Block) { b.Jmp = Jmp{Kind: JmpJmp, To: target} }

// Jnz branches to ifTrue when cond is non-zero, otherwise to ifFalse.
func (b *Block) Jnz(cond Ref, ifTrue, ifFalse *Block) {
	b.Jmp = Jmp{Kind: JmpJnz, Arg: cond, To: ifTrue, To2: ifFalse}
}

// Hlt terminates the block as unreachable.
func (b *Block) Hlt() { b.Jmp = Jmp{Kind: JmpHlt} }

// BlockAddr yields the code address of target (the &&label GNU extension), for
// use as an indirect branch destination.
func (b *Block) BlockAddr(target *Block) Ref {
	res := b.fn.newTemp("", ClsP)
	b.Instrs = append(b.Instrs, Instr{Op: OBlockAddr, Cls: ClsP, To: res, Blk: target, Pos: b.curPos})
	return res
}

// BrIndirect terminates the block with a computed goto to the address in addr;
// targets lists every block the branch may reach (all address-taken labels).
func (b *Block) BrIndirect(addr Ref, targets ...*Block) {
	b.Jmp = Jmp{Kind: JmpBr, Arg: addr, Targets: targets}
}

// Switch terminates the block with a multiway branch: control goes to the arm
// whose value equals val, or to deflt when none matches. signed selects signed
// vs unsigned comparison for the ordered lowering. A backend or lowering pass
// turns this into an if-chain, a binary search, or a jump table.
func (b *Block) Switch(val Ref, deflt *Block, signed bool, cases []SwitchCase) {
	b.Jmp = Jmp{Kind: JmpSwitch, Arg: val, To: deflt, Signed: signed, Cases: cases}
}

// --- phis -----------------------------------------------------------------

// PhiEdge pairs an incoming value with the predecessor it arrives from.
type PhiEdge struct {
	From *Block
	Val  Ref
}

// Phi adds a phi node selecting among the given predecessor edges and returns
// its result. Edges may also be appended later with (*Phi).Add for loops whose
// back-edge value is not yet available.
func (b *Block) Phi(cls Cls, edges ...PhiEdge) Ref {
	p := &Phi{Cls: cls}
	res := b.fn.newTemp("", cls)
	p.To = res
	for _, e := range edges {
		p.Args = append(p.Args, e.Val)
		p.Blocks = append(p.Blocks, e.From)
	}
	b.Phis = append(b.Phis, p)
	return res
}

// Add appends an incoming edge to a phi (used to close loop back-edges).
func (p *Phi) Add(from *Block, val Ref) {
	p.Args = append(p.Args, val)
	p.Blocks = append(p.Blocks, from)
}
