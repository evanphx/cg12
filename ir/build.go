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

// ParamGroup appends the scalar parts of one by-value aggregate parameter and
// records that they must be assigned atomically by the target ABI.
func (f *Func) ParamGroup(name string, aggregate *AggType, classes ...Cls) []Ref {
	start := len(f.Params)
	parts := make([]Ref, len(classes))
	for index, class := range classes {
		partName := fmt.Sprintf("%s.%d", name, index)
		parts[index] = f.Param(partName, class)
	}
	f.ParamGroups = append(f.ParamGroups, ValueGroup{
		Index: start,
		Count: len(parts),
		Type:  aggregate,
	})
	return parts
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

// InheritGCRef marks dst a managed reference when any of srcs is one, carrying
// the first managed source's type descriptor with it.
//
// It is for a transform that mints a temporary standing for values that already
// exist: a phi merging them, a select choosing between them, a clone of the
// instruction that defined one. Managed-ness is a property of the value, not of
// the temporary that happens to hold it, so such a temporary is managed exactly
// when what it stands for is. A new temporary starts unmarked, and an unmarked
// temporary holding a heap pointer is reported at no safepoint — the collector
// frees an object the program is still going to use.
func (f *Func) InheritGCRef(dst Ref, srcs ...Ref) Ref {
	if dst.Kind != RefTemp {
		return dst
	}
	for _, src := range srcs {
		if src.Kind != RefTemp {
			continue // a constant (a nil pointer) names no object
		}
		source := f.Temps[src.ID]
		if source == nil || !source.GCRef {
			continue
		}
		target := f.Temps[dst.ID]
		target.GCRef = true
		if target.GCType == 0 {
			target.GCType = source.GCType
		}
		return dst
	}
	return dst
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

// Aggregate creates an immutable frontend aggregate from scalar SSA parts.
func (f *Func) Aggregate(valueType *AggType, parts ...Ref) Ref {
	id := len(f.AggregateValues)
	f.AggregateValues = append(f.AggregateValues, AggregateValue{
		Type:  valueType,
		Parts: append([]Ref(nil), parts...),
	})
	return Ref{Kind: RefAggregate, ID: uint32(id)}
}

// Aggregate returns the aggregate value named by reference. It panics for a
// non-aggregate reference so accidental scalar use is caught close to the
// frontend that produced it.
func (f *Func) AggregateValue(reference Ref) AggregateValue {
	if reference.Kind != RefAggregate || int(reference.ID) >= len(f.AggregateValues) {
		panic("ir: reference is not an aggregate value")
	}
	return f.AggregateValues[reference.ID]
}

func (f *Func) internConst(c Const) Ref {
	k := constKey{c.Kind, c.Cls, c.Int, c.Flt, c.Sym, c.Thread}
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

// Pacia signs the pointer x with modifier y under pointer-authentication key A.
func (b *Block) Pacia(x, y Ref) Ref { return b.emit(OPacia, ClsL, x, y) }

// Binary arithmetic and bitwise operations.
func (b *Block) Add(cls Cls, x, y Ref) Ref { return b.emit(OAdd, cls, x, y) }
func (b *Block) Sub(cls Cls, x, y Ref) Ref { return b.emit(OSub, cls, x, y) }
func (b *Block) Mul(cls Cls, x, y Ref) Ref { return b.emit(OMul, cls, x, y) }

// UMulh and SMulh return the high 64 bits of the 128-bit product x*y (unsigned
// and signed respectively); 64-bit operands only. They exist for overflow tests.
func (b *Block) UMulh(x, y Ref) Ref { return b.emit(OUMulh, ClsL, x, y) }
func (b *Block) SMulh(x, y Ref) Ref { return b.emit(OSMulh, ClsL, x, y) }

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

// Select yields a when cond != 0, else b -- the branchless conditional (arm64
// csel, LLVM select). cond is an integer tested against zero; a and b share the
// result class cls.
func (b *Block) Select(cls Cls, cond, a, x Ref) Ref { return b.emit(OSel, cls, cond, a, x) }

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

// CallerPC returns the address to which the current function will return.
func (b *Block) CallerPC() Ref {
	return b.Intrinsic("getcallerpc", ClsL)
}

// CallerSP returns the caller's stack pointer at function entry.
func (b *Block) CallerSP() Ref {
	return b.Intrinsic("getcallersp", ClsL)
}

func (b *Block) Load(cls Cls, addr Ref) Ref {
	if addr.Kind == RefReg {
		value := b.emit(OGetReg, cls, addr) // read a machine register
		if cls == ClsP {
			b.fn.MarkGCRef(value)
		}
		return value
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
	value := b.emit(op, cls, addr)
	if cls == ClsP {
		b.fn.MarkGCRef(value)
	}
	return value
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

// LifeStart / LifeEnd bracket the live region of the stack slot at address addr (an
// OAlloc result), modelling LLVM's lifetime intrinsics. They read addr, define
// nothing, and emit no machine code.
func (b *Block) LifeStart(addr Ref) { b.lifetime(OLifeStart, addr) }
func (b *Block) LifeEnd(addr Ref)   { b.lifetime(OLifeEnd, addr) }
func (b *Block) lifetime(op Op, addr Ref) {
	b.Instrs = append(b.Instrs, Instr{Op: op, Args: []Ref{addr}, Pos: b.curPos})
}

// Intrinsic invokes the named intrinsic with the given operands and returns its
// result (class cls). For an intrinsic that yields no value use IntrinsicVoid.
func (b *Block) Intrinsic(name string, cls Cls, args ...Ref) Ref {
	res := b.fn.newTemp("", cls)
	b.Instrs = append(b.Instrs, Instr{
		Op: OIntrinsic, Cls: cls, To: res, Args: args,
		Intrin: &IntrinOp{Name: name}, Pos: b.curPos,
	})
	return res
}

// IntrinsicVoid invokes the named intrinsic for effect only.
func (b *Block) IntrinsicVoid(name string, args ...Ref) {
	b.Instrs = append(b.Instrs, Instr{
		Op: OIntrinsic, Args: args, Intrin: &IntrinOp{Name: name}, Pos: b.curPos,
	})
}

// StackSave captures the current stack pointer, and StackRestore sets it back to
// a saved value, reclaiming any VLAs allocated in between.
func (b *Block) StackSave() Ref      { return b.Intrinsic("stacksave", ClsP) }
func (b *Block) StackRestore(sp Ref) { b.IntrinsicVoid("stackrestore", sp) }

// atomicName is the registered name of an atomic operation at a given width: the
// class picks the ".w" (4-byte) or ".l" (8-byte) suffix. The width lives in the
// name, so it survives the textual IL even for a void store.
func atomicName(op string, cls Cls) string {
	if cls == ClsL || cls == ClsP {
		return "atomic." + op + ".l"
	}
	return "atomic." + op + ".w"
}

// AtomicLoad atomically reads a cls-width value from addr.
func (b *Block) AtomicLoad(cls Cls, addr Ref) Ref {
	return b.Intrinsic(atomicName("load", cls), cls, addr)
}

// AtomicStore atomically writes val (cls-width) to addr.
func (b *Block) AtomicStore(cls Cls, addr, val Ref) {
	b.IntrinsicVoid(atomicName("store", cls), addr, val)
}

// AtomicXchg atomically stores val to addr and returns the previous value.
func (b *Block) AtomicXchg(cls Cls, addr, val Ref) Ref {
	return b.Intrinsic(atomicName("xchg", cls), cls, addr, val)
}

// AtomicCAS atomically compares *addr against expected and, when equal, stores
// newVal; it returns the value that was in *addr (which equals expected exactly
// when the swap happened).
func (b *Block) AtomicCAS(cls Cls, addr, expected, newVal Ref) Ref {
	return b.Intrinsic(atomicName("cas", cls), cls, addr, expected, newVal)
}

// AtomicRMW atomically applies op (one of add, sub, and, or, xor) to *addr with
// val and returns the previous value.
func (b *Block) AtomicRMW(op string, cls Cls, addr, val Ref) Ref {
	return b.Intrinsic(atomicName(op, cls), cls, addr, val)
}

// AtomicFence emits a full memory barrier.
func (b *Block) AtomicFence() { b.IntrinsicVoid("atomic.fence") }

// HeapAlloc emits a typed heap-allocation candidate. The optimizer promotes
// it to a stack allocation when its result cannot escape and otherwise lowers
// it to allocator(typeDescriptor).
func (b *Block) HeapAlloc(allocator, typeDescriptor Ref, size, align int) Ref {
	return b.heapAlloc([]Ref{allocator, typeDescriptor, b.fn.Long(int64(size))}, size, align)
}

// HeapAllocConverted emits a heap-allocation candidate whose object, if it does
// have to leave the frame, can be built by a runtime conversion helper instead
// of by an allocator: converter(value) returns storage already holding value,
// and for a small enough value it returns a pointer into a static table and
// allocates nothing at all. runtime.convT64 and its siblings are the helpers
// this exists for.
//
// The candidate carries the allocator and type descriptor as well, because
// promoting it to the frame has to produce exactly what HeapAlloc's promotion
// produces; the helper is only consulted on the path where the object would
// otherwise have been allocated.
//
// Two rules bind the emitter and [opt.LowerHeapAllocations] together, and both
// are the emitter's to keep:
//
//   - The instruction immediately after the candidate must be the store that
//     initializes it. The lowering drops that store when it calls the helper,
//     because the helper's result already holds the value, and the storage it
//     returns for a small value is read-only.
//
//   - value must carry the same bytes the store writes, in the class the helper
//     takes its argument in. A widening or a bit reinterpretation between the
//     stored value and value is fine -- the store writes the low bytes of the
//     object and the helper is handed the whole register -- but they cannot be
//     two different values.
//
// If either rule is broken the lowering does not fire, and the candidate is
// lowered as an ordinary allocation, so a mistake here costs an allocation
// rather than correctness.
func (b *Block) HeapAllocConverted(allocator, typeDescriptor, converter, value Ref, size, align int) Ref {
	args := []Ref{allocator, typeDescriptor, b.fn.Long(int64(size)), converter, value}
	return b.heapAlloc(args, size, align)
}

// HeapAllocConvertedField is HeapAllocConverted for an object that has somewhere
// to go if it cannot be its own allocation: storage at offset bytes into the
// object container allocates, reserved for exactly this.
//
// It exists because one object is one placement, and a variadic `...any` call
// has two questions to answer, not one. The `[N]any` backing array escapes when
// the callee retains the *slice*; a boxed payload escapes when the callee
// retains an *element*. Packing both into one object -- which is what container
// still is for every payload with no conversion helper -- forces the array to
// the answer the payload got, and that is why `fmt.Sprintf("value=%d", n)` cost
// a heap allocation for an array gc has always kept in a frame.
//
// Emitting the payload separately lets the two be decided apart, and carrying
// the reserved field lets [opt.LowerHeapAllocations] undo the separation when
// it did not pay: with the container on the heap, N+1 objects cost N+1
// allocations where the combined one costs one. The pass folds the payload back
// to container+offset in that case, and the initializing store the contract
// above requires -- which HeapAllocConverted's helper path drops -- is kept,
// because there is no helper call to have written the value.
//
// The emitter owes one thing beyond HeapAllocConverted's two rules: container's
// allocated type must really have offset bytes of storage of this object's type
// there, since the fold makes the payload alias it.
func (b *Block) HeapAllocConvertedField(allocator, typeDescriptor, converter, value, container Ref, offset int64, size, align int) Ref {
	args := []Ref{allocator, typeDescriptor, b.fn.Long(int64(size)), converter, value, container, b.fn.Long(offset)}
	return b.heapAlloc(args, size, align)
}

func (b *Block) heapAlloc(args []Ref, size, align int) Ref {
	if size < 0 {
		panic("ir: negative heap allocation size")
	}
	if align != 4 && align != 8 && align != 16 {
		if align < 4 {
			align = 4
		} else {
			panic(fmt.Sprintf("ir: invalid heap allocation alignment %d", align))
		}
	}
	in := Instr{
		Op:   OHeapAlloc,
		Cls:  ClsP,
		Args: args,
		Aux:  int64(align),
		Pos:  b.curPos,
	}
	in.To = b.fn.newTemp("", ClsP)
	b.fn.MarkGCRef(in.To)
	b.Instrs = append(b.Instrs, in)
	return in.To
}

// Call invokes callee with args and returns the result (class retCls). For a
// void call use CallVoid.
func (b *Block) Call(retCls Cls, callee Ref, args ...Ref) Ref {
	return b.call(retCls, callee, false, CallConvPlatform, args...)
}

// CallWithConvention invokes callee using an explicit physical calling
// convention. It is intended for foreign calls and the small set of raw runtime
// entry points whose register contract differs from the containing function.
func (b *Block) CallWithConvention(retCls Cls, convention CallConvention, callee Ref, args ...Ref) Ref {
	return b.call(retCls, callee, true, convention, args...)
}

func (b *Block) call(retCls Cls, callee Ref, conventionSet bool, convention CallConvention, args ...Ref) Ref {
	in := Instr{
		Op:          OCall,
		Cls:         retCls,
		Args:        append([]Ref{callee}, args...),
		Pos:         b.curPos,
		CallConv:    convention,
		CallConvSet: conventionSet,
	}
	res := b.fn.newTemp("", retCls)
	in.To = res
	b.Instrs = append(b.Instrs, in)
	return res
}

// AsmSpec describes one inline-asm operand for the Asm builder. For an AsmRegOut
// operand Cls is the result class; for every other kind Ref is the operand's
// value (a register input's value, an immediate's constant, or a memory
// operand's address).
type AsmSpec struct {
	Kind AsmOperandKind
	Cls  Cls
	Ref  Ref
	Reg  string // fixed physical-register constraint letter, or "" for the allocator's choice
}

// Asm emits an inline-assembly statement (OAsm) from operand specs in %N order.
// A fresh result temporary is allocated for each register-output operand; those
// temporaries are returned in order (the first becomes To, the rest Defs).
func (b *Block) Asm(template string, specs []AsmSpec) []Ref {
	in := Instr{Op: OAsm, Pos: b.curPos, Asm: &AsmOp{Template: template}}
	var outs []Ref
	for _, s := range specs {
		in.Asm.Ops = append(in.Asm.Ops, s.Kind)
		in.Asm.Regs = append(in.Asm.Regs, s.Reg)
		if s.Kind == AsmRegOut || s.Kind == AsmRegInOut {
			t := b.fn.newTemp("", s.Cls)
			outs = append(outs, t)
			if in.To.Kind != RefTemp {
				in.Cls = s.Cls
				in.To = t
			} else {
				in.Defs = append(in.Defs, t)
			}
		}
		if s.Kind != AsmRegOut {
			in.Args = append(in.Args, s.Ref) // a read-write register also carries a preload value
		}
	}
	b.Instrs = append(b.Instrs, in)
	return outs
}

// CallAggregate invokes a function returning an aggregate already represented
// as scalar SSA parts. The result refs are defined together by the call.
func (b *Block) CallAggregate(aggregate *AggType, classes []Cls, callee Ref, args ...Ref) []Ref {
	return b.callAggregate(aggregate, classes, callee, false, CallConvPlatform, args...)
}

// CallAggregateWithConvention is CallAggregate with an explicit physical call
// convention.
func (b *Block) CallAggregateWithConvention(aggregate *AggType, classes []Cls, convention CallConvention, callee Ref, args ...Ref) []Ref {
	return b.callAggregate(aggregate, classes, callee, true, convention, args...)
}

func (b *Block) callAggregate(aggregate *AggType, classes []Cls, callee Ref, conventionSet bool, convention CallConvention, args ...Ref) []Ref {
	if len(classes) == 0 {
		panic("ir: aggregate call has no result parts")
	}
	instruction := Instr{
		Op:          OCall,
		Cls:         classes[0],
		Args:        append([]Ref{callee}, args...),
		RetAgg:      aggregate,
		RetValues:   true,
		Pos:         b.curPos,
		CallConv:    convention,
		CallConvSet: conventionSet,
	}
	results := make([]Ref, len(classes))
	for index, class := range classes {
		result := b.fn.newTemp("", class)
		results[index] = result
		if index == 0 {
			instruction.To = result
		} else {
			instruction.Defs = append(instruction.Defs, result)
		}
	}
	markAggregateResultGCRefs(b.fn, aggregate.Fields, results, new(int))
	b.Instrs = append(b.Instrs, instruction)
	return results
}

func markAggregateResultGCRefs(function *Func, fields []Field, results []Ref, resultIndex *int) {
	for _, field := range fields {
		count := field.count()
		for element := 0; element < count; element++ {
			if field.Type != nil {
				markAggregateResultGCRefs(function, field.Type.Fields, results, resultIndex)
				continue
			}
			if *resultIndex >= len(results) {
				return
			}
			if field.Pointer {
				function.MarkGCRef(results[*resultIndex])
			}
			*resultIndex = *resultIndex + 1
		}
	}
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
	b.callVoid(callee, false, CallConvPlatform, args...)

}

// CallVoidWithConvention is CallVoid with an explicit physical call
// convention.
func (b *Block) CallVoidWithConvention(convention CallConvention, callee Ref, args ...Ref) {
	b.callVoid(callee, true, convention, args...)
}

func (b *Block) callVoid(callee Ref, conventionSet bool, convention CallConvention, args ...Ref) {
	in := Instr{
		Op:          OCall,
		Cls:         ClsW,
		Args:        append([]Ref{callee}, args...),
		Pos:         b.curPos,
		CallConv:    convention,
		CallConvSet: conventionSet,
	}
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

// RetAggregate returns scalar SSA parts that together form the function's
// aggregate result.
func (b *Block) RetAggregate(parts ...Ref) {
	if len(parts) == 0 {
		panic("ir: aggregate return has no parts")
	}
	b.Jmp = Jmp{Kind: JmpRet, Arg: parts[0], Args: append([]Ref(nil), parts[1:]...)}
}

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
