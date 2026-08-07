package ir

// Instr is a single non-phi SSA instruction.
//
// Args holds the operands (0-2 for most modelled ops; 3 for OSel; calls carry
// their extra arguments inline). Keeping Args a slice rather than a fixed array
// trades a little density for a builder API that reads naturally and for uniform
// iteration in the passes.
type Instr struct {
	// Field order here is chosen for size, not for reading: Instr is stored by
	// value in Block.Instrs, so every byte of padding is paid 297,389 times for
	// a 168-line source. Eight-byte-aligned fields come first, then the Refs
	// (four-byte aligned but eight wide), then everything narrower. Interleaved,
	// the same fields cost 14 bytes of padding; in this order, none.
	//
	// Each field's meaning is documented where it used to be declared, below.
	Args []Ref // operands

	To Ref // result temporary, or R when the op has no result

	// Aux is an op-specific immediate. Each op that uses it reads exactly one
	// meaning, and they are mutually exclusive, but the field is untyped, so the
	// meanings are only kept apart by which op carries them -- which is why the two
	// that once shared OCall (recursion depth and stack bytes) were split out (see
	// Unroll). The meanings still here:
	//   - OBlit / aggregate copy: the number of bytes to copy.
	//   - OVaArg: the type id of the argument being fetched.
	//   - OCall (after ABI lowering): the outgoing stack-argument area size.
	//   - OPar / OArg (after ABI lowering): a stacked argument's byte offset.
	// The first two are set by the front end and survive serialization; the last
	// two are established during lowering and never reach the binary format.
	Aux int64

	// Amode carries a target's folded memory addressing mode on a load or store,
	// or 0 for a plain [base] or [base, #imm] access. It is set during lowering
	// (arm64's foldAddressing packs its extend option and scale here) and read by
	// the emitter; it never reaches the binary format, which only transports
	// pre-lowering IR.
	//
	// It has a field of its own rather than riding in Aux because a folded load is
	// still an OLoadl: the op does not change, so Aux on that op would mean "the
	// addressing mode" after the fold and "nothing" before it, on instructions a
	// pass cannot tell apart. That is the overload ir/instr.go's own contract warns
	// against, and the reason the fold was the one Aux meaning most able to be
	// misread by a pass that ran after it.
	//
	// Declared with the other narrow fields at the end of the struct.

	// Unroll is the recursion depth the inliner marks an OCall with while it
	// decides how far to unroll a cycle. It is transient: opt clears it when the
	// unroller is done, and nothing outside opt reads it.
	//
	// It has a field of its own because it used to live in Aux, which an OCall
	// also uses for its outgoing stack bytes. The two never met -- the backend's
	// ABI lowering rebuilds the call instruction, so a stale depth was overwritten
	// rather than misread -- but nothing said so, and what kept them apart was the
	// order the passes happen to run in plus one clearCallDepth call. Aux is an
	// untyped int64 with several meanings; that is exactly how a pass reordering
	// turns into a wrong stack frame.
	// Moved to InstrExtra: only an OCall carries a recursion depth, and only
	// while the unroller is running.

	// CallConv is the physical convention used by this OCall. Calls default to
	// the containing function's convention; an explicit override is represented
	// by CallConvSet. Keeping this on the call makes foreign and raw-runtime
	// boundaries local facts instead of properties inferred from the caller.
	//
	// Declared with the other single-byte fields at the top of the struct.

	// Extra holds the fields only a narrow set of opcodes ever uses. It is nil
	// for the great majority of instructions -- see [InstrExtra].
	//
	// Reach it through the accessors ([Instr.AggArgs()], [Instr.SetAggArgs] and
	// friends), not directly: they read through a nil Extra and allocate one
	// only on write, so a caller never has to know whether this instruction has
	// one.
	Extra *InstrExtra

	// RetAgg is the aggregate type of an OCall's result (the result temporary is
	// a pointer to it), or nil for a scalar result.
	RetAgg *AggType

	// RetValues means To and Defs are the scalar parts of RetAgg rather than To
	// being an address at which the aggregate result is reconstructed.
	//
	// Declared with the other single-byte fields at the top of the struct.

	// StackResult and StackResultOffset moved to InstrExtra: only arm64 sets
	// them, and only for a Go-ABI call with a stack-assigned aggregate result.

	// Pos is the source position this instruction was generated from, or the
	// zero SrcPos when unknown. Backends emit it as debug-line info.
	Pos SrcPos

	// Tail marks an OCall as a mandatory tail call: the call is in tail position
	// (the last instruction of its block, whose result the block immediately
	// returns) and the backend must emit a real tail call — reusing the frame —
	// or report an error. It is not a best-effort optimisation, so an author can
	// rely on it (e.g. for guaranteed tail-call elimination) and learn at compile
	// time when a target cannot honour it.
	//
	// Declared with the other single-byte fields at the top of the struct.

	// Volatile marks a load or store whose execution is itself observable: the
	// access must happen, exactly once, in the order written. It is what C's
	// `volatile` means, and it is a property of the access rather than of the op,
	// because the same OLoadw is ordinary in one place and a device register read
	// in another.
	//
	// A pass may not remove a volatile access (even for an unused result), fold
	// two into one, satisfy one from a previous value, or move one past another
	// access. Without this the optimizer is free to reason that reading the same
	// address twice must yield the same value -- which is exactly what an MMIO
	// register, a signal-handler flag, and another thread's variable all violate.
	//
	// Declared with the other single-byte fields at the top of the struct.

	// ClosureCall marks an indirect call whose callee receives a closure
	// environment in the architecture's dedicated closure register. ABIInternal
	// reserves an additional spill word for that register in the outgoing frame.
	//
	// Declared with the other single-byte fields at the top of the struct.
	// ClosureContext is the source-level closure object placed in the dedicated
	// register for a ClosureCall. Keeping it explicit lets the inliner replace a
	// callee's incoming closure register with the caller's ordinary SSA value.
	ClosureContext Ref

	// Blk names the target block of an OBlockAddr (the address of a label taken
	// with the &&label extension), whose result is that block's code address.
	Blk *Block

	// Inl records inline provenance: when the inliner splices a callee's body in,
	// each cloned instruction points at the InlineSite describing which function
	// it came from and where it was called. nil for ordinary (non-inlined) code.
	// Backends turn it into DWARF DW_TAG_inlined_subroutine records.
	Inl *InlineSite

	// Asm carries an OAsm's inline-assembly template and operand layout.
	Asm *AsmOp

	// Intrin carries an OIntrinsic's dispatch name (the intrinsic being invoked).
	// nil for every other op.
	Intrin *IntrinOp

	// Amode is four-byte aligned and sits here, next to Pos and the one-byte
	// fields, so it packs into what would otherwise be tail padding.
	Amode int32

	// The single-byte fields, last, so nothing eight-byte-aligned has to skip
	// past them. Scattered through the struct they cost 14 bytes of padding
	// between them; here they cost none. Each one's meaning is documented above,
	// where it used to be declared.
	Op          Op
	Cls         Cls // class of the result (and of integer operands, where relevant)
	Cmp         Cmp // predicate, valid only when Op == OCmp
	CallConv    CallConvention
	CallConvSet bool
	RetValues   bool
	Tail        bool
	Volatile    bool
	ClosureCall bool
}

// Uses returns every SSA value read by the instruction, including operands
// carried outside Args for ABI purposes.
func (in *Instr) Uses() []Ref {
	if in.ClosureContext.IsNone() {
		return in.Args
	}
	uses := make([]Ref, 0, len(in.Args)+1)
	uses = append(uses, in.Args...)
	uses = append(uses, in.ClosureContext)
	return uses
}

// AsmOperandKind classifies an inline-asm operand for template substitution.
type AsmOperandKind uint8

const (
	AsmRegOut   AsmOperandKind = iota // "=r": a register result (in To/Defs)
	AsmRegIn                          // "r":  a register input (in Args)
	AsmImm                            // "i":  an immediate constant (in Args)
	AsmMem                            // "m"/"=m": a memory reference by address (in Args)
	AsmRegInOut                       // "+r": a register result preloaded with an input (in Args)
)

// AsmOp describes a GNU inline-assembly statement lowered to an OAsm. Ops lists
// every operand in the GNU %N order (all outputs, then all inputs). A register
// output (AsmRegOut) or read-write register (AsmRegInOut) draws its result
// temporary from To (the first) then Defs; every operand other than a plain
// register output draws a value from Args in order -- a register input's value,
// an immediate's constant, a memory operand's address, or a read-write
// register's preload value.
type AsmOp struct {
	Template string           // the assembler template, verbatim between the quotes
	Ops      []AsmOperandKind // operand kinds in %N order
	// ExactClobbers says Clobbers is a complete register-use boundary. Semantic
	// assembly lowering can prove this; ordinary user inline assembly remains
	// conservative and clobbers the target's caller-saved set as before.
	ExactClobbers bool
	// Regs, parallel to Ops, holds a fixed physical-register constraint letter for
	// each operand ("a", "d", "S", ...) or "" when the allocator may choose. A
	// backend precolors the operand's temporary to the named register.
	Regs []string

	// Clobbers names the registers the template writes besides its outputs, as
	// written in the source ("x19", "rbx", "cc", "memory"). A value the allocator
	// is holding in one of them cannot survive the asm, so it must not be there.
	//
	// An OAsm already clobbers like a call, which covers the caller-saved set.
	// This is what the caller-saved assumption misses: GNU asm explicitly permits
	// clobbering a callee-saved register, and a value live across the asm is
	// exactly what the allocator puts in one.
	Clobbers []string
}

// AsmRegOuts returns the register-output result temporaries in %N order (To then
// Defs).
func (in *Instr) AsmRegOuts() []Ref {
	if in.Asm == nil || in.To.Kind != RefTemp {
		return nil
	}
	return append([]Ref{in.To}, in.Defs()...)
}

// InlineSite describes one level of inlining: a call to Callee at the source
// position Call was replaced by the callee's body. Parent is the enclosing
// InlineSite when this inline happened inside already-inlined code, forming a
// chain from the outermost inline down to this one.
type InlineSite struct {
	Callee string // name of the inlined function (the abstract origin)
	Call   SrcPos // the call-site position (where the inline happened)
	Parent *InlineSite
}

// TailCall reports whether block b is terminated by a tail call: its last
// instruction is a tail-marked call whose result the block immediately returns
// (or a void tail call followed by a bare return). It returns that call. A
// tail-marked call in any other position is invalid; ok is false there so a
// backend can reject it.
func TailCall(b *Block) (call *Instr, ok bool) {
	if len(b.Instrs) == 0 || b.Jmp.Kind != JmpRet {
		return nil, false
	}
	in := &b.Instrs[len(b.Instrs)-1]
	if in.Op != OCall || !in.Tail {
		return nil, false
	}
	if in.To.IsNone() {
		return in, b.Jmp.Arg.IsNone()
	}
	return in, b.Jmp.Arg == in.To
}

// HasTailCall reports whether the block contains a tail-marked call anywhere.
// A backend uses it to detect an ill-placed tail call (one HasTailCall finds but
// TailCall rejects) and error rather than silently miscompile.
func HasTailCall(b *Block) bool {
	for i := range b.Instrs {
		if b.Instrs[i].Op == OCall && b.Instrs[i].Tail {
			return true
		}
	}
	return false
}

// Arg returns operand i, or R if out of range.
func (in *Instr) Arg(i int) Ref {
	if i < 0 || i >= len(in.Args) {
		return R
	}
	return in.Args[i]
}

// Phi is an SSA phi node: at block entry it selects Args[k] when control arrived
// from Blocks[k]. Args and Blocks are parallel and equal length.
type Phi struct {
	Cls    Cls
	To     Ref
	Args   []Ref
	Blocks []*Block
}

// JmpKind is the terminator kind of a block.
type JmpKind uint8

const (
	JmpNone   JmpKind = iota // unterminated (under construction)
	JmpJmp                   // unconditional jump to To
	JmpJnz                   // if Arg != 0 goto To else To2
	JmpRet                   // return Arg (R for void)
	JmpHlt                   // trap / unreachable
	JmpBr                    // computed goto: branch to the address in Arg, reaching one of Targets
	JmpSwitch                // multiway branch: dispatch Arg to a matching Case, else To (default)
	JmpTable                 // indexed branch: go to Targets[Arg]; Arg is a bounds-checked 0-based index
)

// SwitchCase pairs a case's constant value with the block it dispatches to.
type SwitchCase struct {
	Val int64
	Blk *Block
}

// Jmp is a block terminator. Successor blocks are referenced directly so the
// CFG is navigable without a separate edge table.
type Jmp struct {
	Kind JmpKind
	Arg  Ref    // jnz condition or return value
	To   *Block // primary successor (jmp target, jnz true edge)
	To2  *Block // jnz false edge

	// Args lists additional values live at the terminator — the extra registers
	// of a multi-register aggregate return, beyond Arg.
	Args []Ref

	// Targets lists the possible successor blocks of a JmpBr (every label whose
	// address is taken), so the CFG and liveness see the indirect edges.
	Targets []*Block

	// Cases holds the value->block arms of a JmpSwitch; To is its default block.
	// Signed selects signed vs unsigned comparison when the switch is lowered.
	Cases  []SwitchCase
	Signed bool

	// Likely records a __builtin_expect hint on a JmpJnz: which edge the source
	// says is taken almost always. Static block-frequency estimation uses it to
	// bias the two edges (so the register allocator keeps hot-edge values in
	// registers and spills cold-edge ones). It is advisory and may be dropped by a
	// pass that rewrites the branch.
	Likely LikelyEdge
}

// LikelyEdge names the expected-taken edge of a conditional branch.
type LikelyEdge uint8

const (
	LikelyNone LikelyEdge = iota // no hint
	LikelyTo                     // the true (To) edge is taken almost always
	LikelyTo2                    // the false (To2) edge is taken almost always
)
