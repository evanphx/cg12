package ir

// Instr is a single non-phi SSA instruction.
//
// Args holds the operands (0-2 for most modelled ops; 3 for OSel; calls carry
// their extra arguments inline). Keeping Args a slice rather than a fixed array
// trades a little density for a builder API that reads naturally and for uniform
// iteration in the passes.
type Instr struct {
	Op   Op
	Cls  Cls   // class of the result (and of integer operands, where relevant)
	To   Ref   // result temporary, or R when the op has no result
	Args []Ref // operands
	Cmp  Cmp   // predicate, valid only when Op == OCmp

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
	Amode int32

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
	Unroll int32

	// AggArgs types by-value aggregate call arguments: for an OCall, AggArgs[k]
	// (when non-nil) is the aggregate type of the value argument Args[1+k],
	// which is a pointer to that aggregate. nil entries are scalar arguments.
	AggArgs []*AggType

	// Defs lists physical-register results an OCall produces beyond To — the
	// extra registers of a multi-register aggregate return. They are defined at
	// the call for liveness/allocation.
	Defs []Ref

	// RetAgg is the aggregate type of an OCall's result (the result temporary is
	// a pointer to it), or nil for a scalar result.
	RetAgg *AggType

	// Pos is the source position this instruction was generated from, or the
	// zero SrcPos when unknown. Backends emit it as debug-line info.
	Pos SrcPos

	// Tail marks an OCall as a mandatory tail call: the call is in tail position
	// (the last instruction of its block, whose result the block immediately
	// returns) and the backend must emit a real tail call — reusing the frame —
	// or report an error. It is not a best-effort optimisation, so an author can
	// rely on it (e.g. for guaranteed tail-call elimination) and learn at compile
	// time when a target cannot honour it.
	Tail bool

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
	Volatile bool

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
	return append([]Ref{in.To}, in.Defs...)
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
}
