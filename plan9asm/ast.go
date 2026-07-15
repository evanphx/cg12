// Package plan9asm parses the assembly syntax accepted by the Go toolchain.
package plan9asm

// File is a parsed Plan 9 assembly source file.
type File struct {
	Statements []Statement
}

// Position identifies a statement's one-based source line.
type Position struct {
	Line int
}

// Statement is one source-level assembly statement.
type Statement interface {
	Position() Position
	statement()
}

type statementPosition struct {
	Pos Position
}

func (p statementPosition) Position() Position {
	return p.Pos
}

// Preprocessor is a C-preprocessor line such as #include or #ifdef.
type Preprocessor struct {
	statementPosition
	Text string
}

func (*Preprocessor) statement() {}

// Text declares the start of a function.
type Text struct {
	statementPosition
	Symbol Symbol
	Flags  []string
	Frame  string
	Args   string
}

func (*Text) statement() {}

// Label declares a branch target.
type Label struct {
	statementPosition
	Name string
}

func (*Label) statement() {}

// Directive is a non-TEXT assembler directive such as GLOBL or PCALIGN.
type Directive struct {
	statementPosition
	Name     string
	Operands []Operand
}

func (*Directive) statement() {}

// Instruction is an assembly instruction. Suffix holds the Go assembler's
// optional addressing suffix, such as P or W in STP.P and LDP.W.
type Instruction struct {
	statementPosition
	Opcode   string
	Suffix   string
	Operands []Operand
}

func (*Instruction) statement() {}

// OperandKind describes the syntactic form of an instruction operand.
type OperandKind uint8

const (
	OperandRaw OperandKind = iota
	OperandRegister
	OperandImmediate
	OperandMemory
	OperandRegisterPair
	OperandShiftedRegister
	OperandSymbol
)

// Operand is a parsed assembly operand. Text always retains the exact trimmed
// spelling. The remaining fields describe the selected Kind.
type Operand struct {
	Kind        OperandKind
	Text        string
	Register    string
	Immediate   string
	Base        string
	Offset      string
	Registers   []string
	Shift       string
	ShiftAmount string
	Symbol      Symbol
}

// Symbol is a Plan 9 symbol reference. ABI is empty unless the source names an
// ABI selector such as <ABIInternal>. Static records the <> file-local suffix.
type Symbol struct {
	Raw    string
	Name   string
	ABI    string
	Static bool
}
