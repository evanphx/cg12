package ir

import "fmt"

// NoReg is the sentinel for a temporary that has not been assigned a physical
// register.
const NoReg = -1

// Temp is an SSA temporary: a value produced once and used any number of times.
// Analyses and the backend annotate it in place (liveness, spill slot, assigned
// register); those backend fields hold their sentinels until a pass fills them.
type Temp struct {
	ID   int
	Name string
	Cls  Cls

	Slot  int  // stack slot index once spilled, or -1
	Reg   int  // physical register id once allocated, or NoReg
	Fixed bool // Reg is a hard constraint (a pre-coloured ABI register)

	// Agg is set on a parameter temporary that is a by-value aggregate: the
	// temporary is a pointer (class L) to the aggregate, and Agg names its type.
	Agg *AggType

	// GCRef marks the temporary as a managed heap reference — a garbage-collector
	// root. It is independent of Cls (a GC reference is pointer-sized) and
	// survives pointer lowering, so backends can report the value's location in
	// stack maps at each safepoint.
	GCRef bool

	// GCType is an optional type descriptor for a GCRef root, carried into the
	// stack map so the runtime knows how to process the pointer — which fields to
	// scan, whether it may point into the stack (for a copying stack), and so on.
	// Zero means an untyped reference. Its meaning is defined by the runtime.
	GCType uint32
}

// ValueGroup records a run of scalar SSA values that together form one
// aggregate value. Index is relative to the surrounding parameter or argument
// list; Count is the number of recursively flattened scalar parts.
//
// Keeping the parts as ordinary temporaries lets the existing scalar optimizer
// and register allocator operate unchanged, while Type preserves the atomic
// ABI grouping required for values such as Go slices.
type ValueGroup struct {
	Index int
	Count int
	Type  *AggType
}

// AggregateValue is a frontend-level immutable aggregate composed of scalar
// SSA refs. Aggregate refs never receive registers or spill slots themselves;
// calls, returns, loads, and stores consume their Parts explicitly.
type AggregateValue struct {
	Type  *AggType
	Parts []Ref
}

// ConstKind distinguishes the flavours of constant.
type ConstKind uint8

const (
	ConstInt   ConstKind = iota // integer bit pattern in Int
	ConstFloat                  // floating value in Flt (class S or D)
	ConstSym                    // address of Sym plus Int offset
)

// Const is an immediate operand. Constants are deduplicated within a Func.
type Const struct {
	Kind   ConstKind
	Cls    Cls
	Int    int64   // integer value, or symbol offset
	Flt    float64 // floating value
	Sym    string  // symbol name for ConstSym
	Thread bool    // ConstSym: the symbol is thread-local (addressed via the TLS ABI)
}

// Linkage describes how a function or datum is exposed to the linker.
type Linkage struct {
	Export  bool
	Thread  bool
	Section string
	SecArgs string
}

// Block is a basic block: some phis, a straight-line body, and one terminator.
type Block struct {
	fn   *Func
	Name string
	ID   int // position in Func.Blocks; becomes the RPO index after ordering
	// SecondaryEntry keeps a block that is entered through metadata rather than
	// an ordinary CFG edge. Go panic recovery, for example, resumes at a
	// runtime.deferreturn call recorded in the function metadata.
	SecondaryEntry bool

	Phis   []*Phi
	Instrs []Instr
	Jmp    Jmp

	Preds []*Block // filled by the CFG pass

	// Sym, when non-empty, is a local object symbol the backend defines at this
	// block's code address. It is set when the block's address is taken in static
	// data (the &&label extension used as an initializer), so the data can hold a
	// relocation to the block.
	Sym string

	Pos    SrcPos // source position of the block (e.g. its label), if known
	curPos SrcPos // builder state: position stamped onto newly emitted instructions
}

// Succs returns the block's successors, derived from its terminator.
func (b *Block) Succs() []*Block {
	switch b.Jmp.Kind {
	case JmpJmp:
		if b.Jmp.To != nil {
			return []*Block{b.Jmp.To}
		}
	case JmpJnz:
		return []*Block{b.Jmp.To, b.Jmp.To2}
	case JmpBr, JmpTable:
		return b.Jmp.Targets
	case JmpSwitch:
		succs := make([]*Block, 0, len(b.Jmp.Cases)+1)
		succs = append(succs, b.Jmp.To) // default
		for _, c := range b.Jmp.Cases {
			succs = append(succs, c.Blk)
		}
		return succs
	}
	return nil
}

// Func is a single function body in SSA form.
type Func struct {
	mod     *Module
	Name    string
	Linkage Linkage

	HasRet bool     // whether the function returns a value
	Retty  Cls      // return class when HasRet and RetAgg == nil
	RetAgg *AggType // non-nil when returning an aggregate by value
	// RetValues means an aggregate return is already represented by scalar SSA
	// parts: Jmp.Arg is the first part and Jmp.Args contains the remaining parts.
	// Without it, the historical representation uses Jmp.Arg as an address.
	RetValues bool
	Variadic  bool // accepts variadic arguments (a trailing "..." in the IL)

	// CallConv controls the physical argument, result, and callee-save rules.
	// The zero value is AAPCS64 so ordinary cg12 functions interoperate with C
	// and with other cg12 front ends by default.
	CallConv CallConvention
	// ManagedFrame enables Go runtime stack growth, precise stack metadata, and
	// the runtime's frame-chain layout independently of the call convention.
	ManagedFrame bool
	// GoABI is retained as a source-compatibility bridge for existing IR
	// producers. New code should set CallConv and ManagedFrame separately. When
	// true it selects both GoInternal and a managed Go frame.
	GoABI   bool
	NoSplit bool // omit the Go stack-growth check at function entry
	// SystemStack marks a //go:systemstack function. Such functions must only run
	// on g0 or gsignal: their stack check uses g.stackguard1 and reports an
	// attempted call from an ordinary goroutine through runtime.morestackc.
	SystemStack bool
	// HasClosureContext reports that ABIInternal supplies this function's
	// closure environment in the architecture's dedicated closure register.
	HasClosureContext bool

	// StackPointerWords records pointer-bearing words within OAlloc results.
	// The outer key is the allocation result temporary ID; inner keys are byte
	// offsets from that allocation.
	StackPointerWords map[uint32]map[int]bool

	// ForceInline is set when the source marked the function
	// __attribute__((always_inline)) -- the inliner then inlines every call to it
	// regardless of its size budget (an interpreter's hot fast-path helpers rely on
	// this to fold into the dispatch loop, as they do under gcc/clang).
	ForceInline bool

	// CostInline marks a function the inliner's cost model (or an experiment) chose to
	// inline into its hot callers even though it exceeds the size budget -- distinct from
	// ForceInline (source always_inline) so honoring it never changes how always_inline
	// is treated. A caller receiving a CostInline splice inlines its whole always_inline
	// subtree cap-free too, so the big inline does not evict the caller's small hot helpers.
	CostInline bool

	// NoInline is set when the source marked the function __attribute__((noinline))
	// or ((cold)) -- the inliner never inlines a call to it, so a cold slow-path helper
	// stays a call and out of the hot inlined body (gcc's hot/cold split; Ruby annotates
	// its slow paths this way). It takes precedence over ForceInline.
	NoInline bool

	Params []*Temp
	// ParamGroups groups runs in Params into by-value aggregate parameters.
	// Parameters outside a group are ordinary scalar values.
	ParamGroups []ValueGroup
	Blocks      []*Block
	Start       *Block

	Temps           []*Temp
	Consts          []Const
	AggregateValues []AggregateValue

	// lowered names the target this function has been lowered for, or "" if it
	// has not been. See MarkLowered.
	lowered string

	nameSeq  int
	constIdx map[constKey]int
}

// CallConvention names a physical function-call contract. It deliberately
// does not describe stack ownership, garbage collection, or unwinding; those
// are properties of a function's frame rather than of its argument registers.
type CallConvention uint8

const (
	CallConvAAPCS64 CallConvention = iota
	CallConvGoInternal
)

// UsesGoInternalCallConvention reports the function's physical call ABI.
func (f *Func) UsesGoInternalCallConvention() bool {
	return f.CallConv == CallConvGoInternal || f.GoABI
}

// UsesManagedFrame reports whether the Go runtime owns this function's stack
// growth and frame metadata.
func (f *Func) UsesManagedFrame() bool {
	return f.ManagedFrame || f.GoABI
}

// MarkLowered records that f has been lowered for the named target, and reports
// an error if it was already lowered for a different one.
//
// Lowering rewrites a function in place and pins the target's physical registers
// into its temporaries. Nothing said so, and nothing stopped a second backend
// reading the result: compiling one module for arm64 and then for amd64 produced
// an amd64 program built around AArch64's register assignment. It did not fail --
// it returned the wrong answer, from an image that linked and ran.
//
// Lowering again for the SAME target is allowed, because it works: the passes are
// idempotent on already-lowered IR (verified on a function with phis, a loop,
// by-value aggregates and calls, lowered three times over). Compiling one module
// into two images of the same architecture is a reasonable thing to do, and
// refusing it would buy nothing.
//
// This is a marker, not a type. A distinct type for lowered IR would catch the
// mistake at compile time rather than here; that is the better answer and a much
// larger change (every pass and emitter signature). This costs one field and
// turns silence into a diagnostic.
func (f *Func) MarkLowered(target string) error {
	if f.lowered != "" && f.lowered != target {
		return fmt.Errorf("ir: %s: already lowered for %s, cannot lower for %s: "+
			"lowering pins the target's registers into the function, so the second "+
			"target would emit code built around the first one's", f.Name, f.lowered, target)
	}
	f.lowered = target
	return nil
}

// LoweredFor returns the target this function was lowered for, or "".
func (f *Func) LoweredFor() string { return f.lowered }

type constKey struct {
	kind   ConstKind
	cls    Cls
	i      int64
	f      float64
	sym    string
	thread bool // a thread-local symbol is a distinct constant from a plain one
}

// Module is a translation unit: a set of functions, aggregate types, data, and
// source assembly selected by the front end.
type Module struct {
	Funcs    []*Func
	Types    []*AggType
	Data     []*Data
	Aliases  []*Alias
	Files    []string // source-file table indexed (1-based) by SrcPos.File
	Assembly []AssemblyFile

	// SymAlign records the guaranteed alignment (bytes) of a data symbol, keyed by
	// its unmangled name -- from a definition or from the type of a reference. A
	// backend folding a symbol's low bits into a scaled load/store offset consults
	// it: that fold is only sound when the symbol is aligned to the access width.
	SymAlign map[string]int
}

// AssemblyFile is a package assembly source retained for parsing and target
// translation after IR compilation. Source uses the Go toolchain's Plan 9
// assembly syntax.
type AssemblyFile struct {
	PackagePath  string
	Path         string
	Source       string
	Defines      map[string]int64
	Includes     map[string]string
	FloatInputs  map[string][]int
	FloatOutputs map[string][]int
	Signatures   map[string]AsmSignature
}

// AsmSignature describes the source-level values consumed and produced by a
// Plan 9 assembly function. Offsets identify the ABI0 FP names used in the
// source; lowering binds the slots to ordinary IR parameters and results rather
// than re-emitting those stack offsets.
type AsmSignature struct {
	Params  []AsmSlot
	Results []AsmSlot
}

type AsmSlot struct {
	Name   string
	Offset int
	Cls    Cls
	Width  int
	GCRef  bool
	Group  int
}

// Module returns the module this function belongs to (nil if standalone).
func (f *Func) Module() *Module { return f.mod }

// NoteSymAlign records align as symbol name's guaranteed alignment, keeping the
// smallest seen so a wider-typed reference never overstates it.
func (m *Module) NoteSymAlign(name string, align int) {
	if align <= 0 {
		return
	}
	if m.SymAlign == nil {
		m.SymAlign = map[string]int{}
	}
	if cur, ok := m.SymAlign[name]; !ok || align < cur {
		m.SymAlign[name] = align
	}
}

// Alias is a second name for a symbol already defined in this module, as
// __attribute__((alias("target"))) requests. The backend emits it as a symbol at
// the target's own location, so the two names resolve to the same code or data.
type Alias struct {
	Name   string
	Target string
	Export bool // external linkage: visible to the linker
	Func   bool // aliases a function (STT_FUNC) rather than data (STT_OBJECT)
}

// Data is a global data definition (initialised or zeroed memory).
type Data struct {
	Name         string
	Linkage      Linkage
	Align        int
	Items        []DataItem
	PointerWords []int // pointer-bearing word indices relative to this datum
}

// HoldsAddress reports whether any of the definition's items is the address of a
// symbol, and so needs a relocation.
//
// It decides where read-only data goes. A const object that holds only numbers
// is finished the moment it is written, and belongs in .rodata. One that holds
// an address is not: in a position-independent image the address depends on
// where the loader put things, so something has to write it after .rodata has
// already been mapped unwritable. Such a datum goes to .data.rel.ro instead --
// writable for the loader, read-only before the program runs.
func (d *Data) HoldsAddress() bool {
	for _, it := range d.Items {
		if it.Sym != "" {
			return true
		}
	}
	return false
}

// DataItem is one initialiser field of a Data definition.
type DataItem struct {
	Sub        SubCls // element type
	Zero       int    // when > 0, this item is Zero bytes of zero-fill
	Ints       []int64
	Flts       []float64
	Sym        string // when set, the item refers to Sym
	RelativeTo string // when set with Sym, encode Sym - RelativeTo
	Off        int64  // offset added to Sym
	Str        string // when set, a raw byte string
}

// ClassOf returns the class of a value reference within this function.
func (f *Func) ClassOf(r Ref) Cls {
	switch r.Kind {
	case RefTemp:
		return f.Temps[r.ID].Cls
	case RefConst:
		return f.Consts[r.ID].Cls
	case RefAggregate:
		panic("ir: aggregate value has no scalar class")
	}
	return ClsW
}

// Temp returns the temporary a ref names, or nil if it is not a temp ref.
func (f *Func) Temp(r Ref) *Temp {
	if r.Kind == RefTemp {
		return f.Temps[r.ID]
	}
	return nil
}
