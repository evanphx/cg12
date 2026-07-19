// Package sem turns parsed Plan 9 assembly into ABI-independent machine
// semantics. It retains architectural operations and registers, but replaces
// FP, SP, and SB pseudo-addressing with arguments, results, locals, and symbols.
package sem

import "github.com/evanphx/cg12/plan9asm"

type ABIKind uint8

const (
	ABIDefault ABIKind = iota
	ABIInternal
	ABI0
)

type FuncKind uint8

const (
	Bindable FuncKind = iota
	Raw
)

type Unit struct {
	Funcs      []*Func
	Data       []*Data
	References []SymRef
}

type SymRef struct {
	Name   string
	Static bool
}

type Func struct {
	Sym               SymRef
	DeclABI           ABIKind
	Flags             []string
	FrameSize         int
	ArgsSize          int
	Kind              FuncKind
	Classification    string
	Signature         Signature
	Body              []Statement
	NoLocalPointers   bool
	signatureDeclared bool
}

type Signature struct {
	Params  []Slot
	Results []Slot
}

type Slot struct {
	Name   string
	Offset int
	Width  int
	Float  bool
	GCRef  bool
	Group  int
}

type Data struct {
	Sym        SymRef
	Size       int
	ReadOnly   bool
	NoPointers bool
	Thread     bool
	Duplicate  bool
	Items      []DataItem
}

type DataItem struct {
	Offset int
	Width  int
	Value  int64
	Bytes  []byte
	Symbol *SymRef
	Addend int64
}

type Statement interface {
	Position() plan9asm.Position
	semanticStatement()
}

type Label struct {
	Pos  plan9asm.Position
	Name string
}

func (s Label) Position() plan9asm.Position { return s.Pos }
func (Label) semanticStatement()            {}

type Align struct {
	Pos       plan9asm.Position
	Alignment int
}

func (s Align) Position() plan9asm.Position { return s.Pos }
func (Align) semanticStatement()            {}

type Directive struct {
	Pos  plan9asm.Position
	Name string
}

func (s Directive) Position() plan9asm.Position { return s.Pos }
func (Directive) semanticStatement()            {}

type Operation struct {
	Family  string
	Width   int
	Variant string
}

type Inst struct {
	Pos      plan9asm.Position
	Op       Operation
	Suffix   string
	Operands []Operand
}

func (s Inst) Position() plan9asm.Position { return s.Pos }
func (Inst) semanticStatement()            {}

type OperandKind uint8

const (
	InvalidOperand OperandKind = iota
	RegisterOperand
	ImmediateOperand
	FloatImmediateOperand
	MemoryOperand
	ArgumentOperand
	ResultOperand
	ArgumentAddressOperand
	ResultAddressOperand
	CallerFrameOperand
	CallerFrameAddressOperand
	LocalOperand
	LocalAddressOperand
	SymbolMemoryOperand
	SymbolAddressOperand
	RegisterAddressOperand
	LabelOperand
	FunctionOperand
	RegisterListOperand
	VectorOperand
	ShiftedOperand
	ExtendedOperand
)

type AddressMode uint8

const (
	AddressOffset AddressMode = iota
	AddressPreIndex
	AddressPostIndex
	AddressRegister
)

type Operand struct {
	Kind        OperandKind
	Register    string
	Registers   []string
	Immediate   int64
	Float       float64
	Base        string
	Index       string
	Offset      int64
	Mode        AddressMode
	Slot        int
	Width       int
	Signed      bool
	Symbol      SymRef
	Label       string
	Arrangement string
	Lane        int
	Shift       string
	Extend      string
}
