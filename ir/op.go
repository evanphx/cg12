package ir

// Op identifies an instruction's operation. Unlike QBE, which defines a
// distinct opcode per operand class (addw/addl/...), we keep operations
// polymorphic over Cls: the class of the result comes from Instr.Cls and the
// class of the operands is recovered from the operands themselves. This keeps
// the opcode set small and the passes uniform.
type Op uint16

const (
	ONop Op = iota

	// Arithmetic (binary; result & integer operands share Instr.Cls).
	OAdd
	OSub
	OMul
	ODiv  // signed / float division
	OUDiv // unsigned division
	ORem  // signed remainder
	OURem // unsigned remainder

	// Bitwise / shifts (integer).
	OAnd
	OOr
	OXor
	OShl // logical left shift
	OShr // logical right shift
	OSar // arithmetic right shift

	// Unary.
	ONeg

	// Comparison. The predicate lives in Instr.Cmp; the operand class is read
	// from the operands, and Instr.Cls is the (integer) result class.
	OCmp

	// Memory stores. The op fixes the width; the value is Args[0], address
	// Args[1].
	OStoreb
	OStoreh
	OStorew
	OStorel
	OStores
	OStored
	OStoreq // 128-bit (quad) store

	// Memory loads. The op fixes width and signedness; Instr.Cls is the
	// destination class and Args[0] is the address.
	OLoadsb
	OLoadub
	OLoadsh
	OLoaduh
	OLoadsw
	OLoaduw
	OLoadl
	OLoads
	OLoadd
	OLoadq // 128-bit (quad) load

	// Stack allocation with the given alignment; result is a pointer (ClsL).
	// The byte size is Args[0] (usually a constant).
	OAlloc4
	OAlloc8
	OAlloc16

	// OAllocN allocates Args[0] bytes on the stack at run time (a VLA), moving
	// the stack pointer; the size must be a multiple of 16. The result is a
	// 16-aligned pointer, valid until the function returns.
	OAllocN

	// OStackSave captures the current stack pointer (result is a pointer);
	// OStackRestore sets the stack pointer to Args[0]. Together they bracket a
	// scope so its variable-length arrays are reclaimed on exit.
	OStackSave
	OStackRestore

	// Memory-to-memory copy of Aux bytes from Args[1] to Args[0].
	OBlit

	// Integer width conversions (result class Instr.Cls).
	OExtsb // sign-extend byte
	OExtub // zero-extend byte
	OExtsh // sign-extend halfword
	OExtuh // zero-extend halfword
	OExtsw // sign-extend word to long
	OExtuw // zero-extend word to long

	// Float width conversions.
	OExts   // single -> double
	OTruncd // double -> single

	// Float <-> integer conversions.
	OStosi // float -> signed integer
	OStoui // float -> unsigned integer
	OSltof // signed integer -> float
	OUltof // unsigned integer -> float

	// Bitwise reinterpretation between an integer and float of equal width.
	OCast

	// Plain copy (may re-type between equal-size classes).
	OCopy

	// Calls and arguments.
	OCall // Args[0] is the callee; Instr.Cls is the return class
	OArg  // an outgoing call argument (materialised before the OCall)
	_     // was OArgEnv, the outgoing environment argument: never referenced by
	//       anything, and its number is burned rather than reclaimed because an
	//       Op is written to the binary encoding as its number -- renumbering
	//       every op after it would decode old modules as different instructions.
	OPar    // an incoming parameter placeholder
	OParEnv // the incoming environment parameter

	// Variadic support.
	OVaStart
	OVaArg

	// OBlockAddr yields the code address of Instr.Blk (the &&label extension).
	OBlockAddr

	// Register variables: read and write a specific machine register directly.
	// Reading (OGetReg) yields the register's current value; writing (OSetReg)
	// sets it. The register operand is a RefReg naming the target register number,
	// making these ops architecture-specific — for low-level runtime code that
	// must touch the stack pointer, frame pointer, or other machine registers.
	OGetReg // result = register Args[0] (a RefReg)
	OSetReg // register Args[1] (a RefReg) = Args[0]

	// Thread-local storage. A thread-local has no address of its own: each thread
	// holds a copy, and how one is reached depends on the model the backend was
	// asked for. These ops exist so that reaching it is ordinary instruction
	// selection -- the register allocator supplies the registers, and for
	// general-dynamic it sees the call -- rather than something an addressing
	// helper improvises out of view.
	//
	// OThreadPtr is the running thread's pointer; OTLSOffset is a variable's
	// distance from it, read however the model says. An address is then just their
	// sum, an ordinary add.
	OThreadPtr // result = the thread pointer
	OTLSOffset // result = Args[0] (a thread-local symbol)'s offset from the thread pointer

	// OTLSIndexAddr yields the address of the descriptor naming Args[0] (a
	// thread-local symbol): which module owns it, and its offset within that
	// module. Handing that to __tls_get_addr yields the variable's address, which
	// is how one is reached whose storage may not exist yet -- a variable in a
	// library brought in by dlopen. That call is an ordinary one, so the register
	// allocator sees what it clobbers.
	OTLSIndexAddr

	// OSafepoint marks a point where the garbage collector may stop the thread:
	// the backend emits a stack map describing the managed references live here.
	// Calls are safepoints implicitly; this marks the ones that are not calls —
	// inlined-allocation fast paths, loop back-edge polls, and the like. It emits
	// no machine code.
	OSafepoint

	// ORotr rotates Args[0] right by Args[1] (a constant amount) within the width
	// of Instr.Cls. It is produced by a backend that recognizes the shift-or
	// rotate idiom (currently arm64) and has a single-instruction rotate.
	ORotr

	// OBic computes Args[0] AND NOT Args[1] (a bit clear). It is produced by a
	// backend that recognizes the and-not idiom (currently arm64) and has a
	// single-instruction form.
	OBic

	// OClz counts the leading zero bits of Args[0], at the width of Instr.Cls
	// (32 or 64). It backs __builtin_clz / __builtin_clzll.
	OClz

	// OAsm is a GNU inline-assembly statement carried through as an opaque,
	// architecture-specific instruction. Instr.Asm holds the template and operand
	// layout; Args are the input operands and To (when Asm.NumOut == 1) is the
	// single output. It clobbers like a call, so a backend that emits assembly
	// text passes the template through (substituting %-operands with the allocated
	// registers) while an object-emitting backend, having no assembler, rejects it.
	OAsm

	// OSel is a conditional select (arm64 csel, LLVM select, wasm select): its
	// result is Args[1] when Args[0] != 0, else Args[2]. It is the branchless form
	// of a c?a:b that would otherwise be a control-flow diamond and a phi. Three
	// value operands, one result of Instr.Cls.
	OSel

	numOps
)

// opInfo carries static metadata about an opcode.
type opInfo struct {
	name        string // IL mnemonic; empty when computed (OCmp)
	hasResult   bool   // defines Instr.To
	commutative bool   // operands may be swapped freely

	// private marks an op that a backend produces and consumes entirely within
	// its own lowering: it is not part of the target-independent IR a front end
	// builds or the text format transports. Such an op still has a name so the
	// printer can render a lowered function for debugging, but the text PARSER
	// refuses it -- a `.ssa` file naming one is not portable IR, it is one
	// backend's internal state written where another backend would read it and,
	// at best, reject it (arm64's `rotr` reaching amd64 is "unsupported op").
	private bool
}

var opTable = [numOps]opInfo{
	ONop: {name: "nop"},

	OAdd:  {name: "add", hasResult: true, commutative: true},
	OSub:  {name: "sub", hasResult: true},
	OMul:  {name: "mul", hasResult: true, commutative: true},
	ODiv:  {name: "div", hasResult: true},
	OUDiv: {name: "udiv", hasResult: true},
	ORem:  {name: "rem", hasResult: true},
	OURem: {name: "urem", hasResult: true},

	OAnd:  {name: "and", hasResult: true, commutative: true},
	OOr:   {name: "or", hasResult: true, commutative: true},
	OXor:  {name: "xor", hasResult: true, commutative: true},
	OShl:  {name: "shl", hasResult: true},
	OShr:  {name: "shr", hasResult: true},
	OSar:  {name: "sar", hasResult: true},
	ORotr: {name: "rotr", hasResult: true, private: true},
	OBic:  {name: "bic", hasResult: true, private: true},
	OClz:  {name: "clz", hasResult: true},

	ONeg: {name: "neg", hasResult: true},

	OCmp: {name: "", hasResult: true},

	OStoreb: {name: "storeb"},
	OStoreh: {name: "storeh"},
	OStorew: {name: "storew"},
	OStorel: {name: "storel"},
	OStores: {name: "stores"},
	OStored: {name: "stored"},
	OStoreq: {name: "storeq"},

	OLoadsb: {name: "loadsb", hasResult: true},
	OLoadub: {name: "loadub", hasResult: true},
	OLoadsh: {name: "loadsh", hasResult: true},
	OLoaduh: {name: "loaduh", hasResult: true},
	OLoadsw: {name: "loadsw", hasResult: true},
	OLoaduw: {name: "loaduw", hasResult: true},
	OLoadl:  {name: "loadl", hasResult: true},
	OLoads:  {name: "loads", hasResult: true},
	OLoadd:  {name: "loadd", hasResult: true},
	OLoadq:  {name: "loadq", hasResult: true},

	OAlloc4:       {name: "alloc4", hasResult: true},
	OAlloc8:       {name: "alloc8", hasResult: true},
	OAlloc16:      {name: "alloc16", hasResult: true},
	OAllocN:       {name: "allocn", hasResult: true},
	OStackSave:    {name: "stacksave", hasResult: true},
	OStackRestore: {name: "stackrestore"},

	OBlit: {name: "blit"},

	OExtsb: {name: "extsb", hasResult: true},
	OExtub: {name: "extub", hasResult: true},
	OExtsh: {name: "extsh", hasResult: true},
	OExtuh: {name: "extuh", hasResult: true},
	OExtsw: {name: "extsw", hasResult: true},
	OExtuw: {name: "extuw", hasResult: true},

	OExts:   {name: "exts", hasResult: true},
	OTruncd: {name: "truncd", hasResult: true},

	OStosi: {name: "stosi", hasResult: true},
	OStoui: {name: "stoui", hasResult: true},
	OSltof: {name: "sltof", hasResult: true},
	OUltof: {name: "ultof", hasResult: true},

	OCast: {name: "cast", hasResult: true},
	OCopy: {name: "copy", hasResult: true},

	OCall:   {name: "call", hasResult: true},
	OArg:    {name: "arg"},
	OPar:    {name: "par", hasResult: true},
	OParEnv: {name: "parenv", hasResult: true},

	OVaStart:   {name: "vastart"},
	OVaArg:     {name: "vaarg", hasResult: true},
	OBlockAddr: {name: "blockaddr", hasResult: true},

	OSafepoint: {name: "safept"},

	OThreadPtr:    {name: "threadptr", hasResult: true, private: true},
	OTLSOffset:    {name: "tlsoffset", hasResult: true, private: true},
	OTLSIndexAddr: {name: "tlsindexaddr", hasResult: true, private: true},

	OGetReg: {name: "getreg", hasResult: true},
	OSetReg: {name: "setreg"},

	OAsm: {name: "asm", hasResult: true},

	OSel: {name: "select", hasResult: true},
}

// Info returns the static metadata for an opcode.
func (o Op) Info() opInfo { return opTable[o] }

// opByName maps IL mnemonics back to opcodes (excluding OCmp, whose mnemonic is
// computed from a predicate and operand class).
var opByName = func() map[string]Op {
	m := make(map[string]Op, numOps)
	for o := Op(0); o < numOps; o++ {
		if n := opTable[o].name; n != "" && !opTable[o].private {
			m[n] = o // private ops are backend internals, not portable IL
		}
	}
	return m
}()

// OpFromString returns the opcode with the given IL mnemonic. Comparison
// mnemonics (which encode a predicate and operand class) are not covered here.
func OpFromString(name string) (Op, bool) {
	o, ok := opByName[name]
	return o, ok
}

// HasResult reports whether the op defines a result temporary.
func (o Op) HasResult() bool { return opTable[o].hasResult }

// IsCommutative reports whether the op's operands may be swapped.
func (o Op) IsCommutative() bool { return opTable[o].commutative }

// IsLoad reports whether the op reads memory.
func (o Op) IsLoad() bool { return o >= OLoadsb && o <= OLoadq }

// IsStore reports whether the op writes memory.
func (o Op) IsStore() bool { return o >= OStoreb && o <= OStoreq }

// IsAlloc reports whether the op allocates a stack slot.
func (o Op) IsAlloc() bool { return o >= OAlloc4 && o <= OAlloc16 }

// String returns the op's IL mnemonic (empty for OCmp, whose mnemonic is
// derived from its predicate and operand class at print time).
func (o Op) String() string { return opTable[o].name }

// Cmp is a comparison predicate carried by an OCmp instruction.
type Cmp uint8

const (
	// Integer predicates.
	CmpEq  Cmp = iota // ==
	CmpNe             // !=
	CmpSle            // signed <=
	CmpSlt            // signed <
	CmpSge            // signed >=
	CmpSgt            // signed >
	CmpUle            // unsigned <=
	CmpUlt            // unsigned <
	CmpUge            // unsigned >=
	CmpUgt            // unsigned >

	// Float predicates (operand class S or D).
	CmpFeq // ordered ==
	CmpFne // unordered != (true if either NaN)
	CmpFle // ordered <=
	CmpFlt // ordered <
	CmpFge // ordered >=
	CmpFgt // ordered >
	CmpFo  // ordered (neither operand NaN)
	CmpFuo // unordered (some operand NaN)

	numCmps
)

// IsFloat reports whether the predicate is a floating-point comparison.
func (c Cmp) IsFloat() bool { return c >= CmpFeq }

var cmpNames = [numCmps]string{
	CmpEq:  "eq",
	CmpNe:  "ne",
	CmpSle: "sle",
	CmpSlt: "slt",
	CmpSge: "sge",
	CmpSgt: "sgt",
	CmpUle: "ule",
	CmpUlt: "ult",
	CmpUge: "uge",
	CmpUgt: "ugt",
	CmpFeq: "eq",
	CmpFne: "ne",
	CmpFle: "le",
	CmpFlt: "lt",
	CmpFge: "ge",
	CmpFgt: "gt",
	CmpFo:  "o",
	CmpFuo: "uo",
}

// String returns the predicate mnemonic (without the leading 'c' or the operand
// class suffix that together form a full compare mnemonic like "csltw").
func (c Cmp) String() string { return cmpNames[c] }
