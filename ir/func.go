package ir

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

	HasRet   bool     // whether the function returns a value
	Retty    Cls      // return class when HasRet and RetAgg == nil
	RetAgg   *AggType // non-nil when returning an aggregate by value
	Variadic bool     // accepts variadic arguments (a trailing "..." in the IL)

	Params []*Temp
	Blocks []*Block
	Start  *Block

	Temps  []*Temp
	Consts []Const

	nameSeq  int
	constIdx map[constKey]int
}

type constKey struct {
	kind ConstKind
	cls  Cls
	i    int64
	f    float64
	sym  string
}

// Module is a translation unit: a set of functions, aggregate types, and data.
type Module struct {
	Funcs []*Func
	Types []*AggType
	Data  []*Data
	Files []string // source-file table indexed (1-based) by SrcPos.File
}

// Data is a global data definition (initialised or zeroed memory).
type Data struct {
	Name    string
	Linkage Linkage
	Align   int
	Items   []DataItem
}

// DataItem is one initialiser field of a Data definition.
type DataItem struct {
	Sub  SubCls // element type
	Zero int    // when > 0, this item is Zero bytes of zero-fill
	Ints []int64
	Flts []float64
	Sym  string // when set, the item is the address of Sym
	Off  int64  // offset added to Sym
	Str  string // when set, a raw byte string
}

// ClassOf returns the class of a value reference within this function.
func (f *Func) ClassOf(r Ref) Cls {
	switch r.Kind {
	case RefTemp:
		return f.Temps[r.ID].Cls
	case RefConst:
		return f.Consts[r.ID].Cls
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
