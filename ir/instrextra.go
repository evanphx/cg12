package ir

// InstrExtra holds the Instr fields that only a narrow set of opcodes uses.
//
// Instr is stored by value in Block.Instrs, so every field costs its width on
// every instruction in the program -- 297,389 of them for a 168-line source
// (opt/pass.go:393). These three slice headers are 72 of those bytes and are
// populated only by OCall and OAsm, so the overwhelming majority of
// instructions were carrying 72 bytes of nil.
//
// One shared struct rather than one per opcode: reaching a field is then a nil
// check rather than a type assertion, which matters because the optimiser is
// the hot phase and its passes walk every instruction of every block on every
// fixpoint round. An OAsm carrying the unused call fields wastes a little space
// on a rare instruction, which is the right side of that trade.
type InstrExtra struct {
	// AggArgs types by-value aggregate call arguments: for an OCall, AggArgs[k]
	// (when non-nil) is the aggregate type of the value argument Args[1+k],
	// which is a pointer to that aggregate. nil entries are scalar arguments.
	AggArgs []*AggType

	// ArgGroups describes aggregate call arguments that have already been
	// scalarized into consecutive entries of Args[1:]. Unlike AggArgs, whose
	// argument is an address, an ArgGroup carries the value's scalar SSA parts.
	ArgGroups []ValueGroup

	// Defs lists physical-register results an instruction produces beyond To.
	//
	// Two opcodes set it, which is why this struct is shared rather than a
	// call-only one: an OCall's extra registers of a multi-register aggregate
	// return (ir/build.go, Block.callAggregate), and an OAsm's register outputs
	// past the first (ir/build.go, Block.Asm -- see [Instr.AsmRegOuts], which
	// documents the order as To then Defs). They are defined at the instruction
	// for liveness and allocation.
	Defs []Ref

	// StackResult identifies a local aggregate slot that receives an ABIInternal
	// stack-assigned result, and StackResultOffset is the offset in the outgoing
	// call frame the emitter copies it from before releasing that frame.
	//
	// Only arm64 sets them, and only for a Go-ABI call whose result is
	// stack-assigned (arm64/lower.go); amd64 never touches them.
	StackResult       Ref
	StackResultOffset int64
}

// StackResult reports the local slot receiving a stack-assigned aggregate
// result, or R.
func (in *Instr) StackResult() Ref {
	if in.Extra == nil {
		return R
	}
	return in.Extra.StackResult
}

// StackResultOffset reports the outgoing-frame offset that result is copied
// from, or 0.
func (in *Instr) StackResultOffset() int64 {
	if in.Extra == nil {
		return 0
	}
	return in.Extra.StackResultOffset
}

// SetStackResult records the slot and the frame offset it is copied from.
func (in *Instr) SetStackResult(slot Ref, offset int64) {
	e := in.extra()
	e.StackResult, e.StackResultOffset = slot, offset
}

// extra returns this instruction's InstrExtra, allocating one if it has none.
// It is the write path; readers use the plain accessors, which tolerate nil.
func (in *Instr) extra() *InstrExtra {
	if in.Extra == nil {
		in.Extra = &InstrExtra{}
	}
	return in.Extra
}

// Clone returns a deep copy, or nil for a nil receiver.
//
// It exists because Instr is copied by value in several places -- opt/gcm.go
// stores instructions in a map and reinserts them elsewhere, opt/inline.go
// snapshots a callee's instruction slice -- and a plain struct copy would leave
// two instructions sharing one InstrExtra. Mutating either would then silently
// change the other.
func (e *InstrExtra) Clone() *InstrExtra {
	if e == nil {
		return nil
	}
	out := *e
	out.AggArgs = append([]*AggType(nil), e.AggArgs...)
	out.ArgGroups = append([]ValueGroup(nil), e.ArgGroups...)
	out.Defs = append([]Ref(nil), e.Defs...)
	return &out
}

// AggArgs reports the by-value aggregate types of a call's arguments, or nil.
func (in *Instr) AggArgs() []*AggType {
	if in.Extra == nil {
		return nil
	}
	return in.Extra.AggArgs
}

// SetAggArgs replaces the aggregate argument types.
func (in *Instr) SetAggArgs(types []*AggType) { in.extra().AggArgs = types }

// ArgGroups reports the scalarized aggregate call arguments, or nil.
func (in *Instr) ArgGroups() []ValueGroup {
	if in.Extra == nil {
		return nil
	}
	return in.Extra.ArgGroups
}

// SetArgGroups replaces the scalarized aggregate call arguments.
func (in *Instr) SetArgGroups(groups []ValueGroup) { in.extra().ArgGroups = groups }

// Defs reports the physical-register results this instruction produces beyond
// To, or nil. Set by OCall and OAsm; nil everywhere else.
func (in *Instr) Defs() []Ref {
	if in.Extra == nil {
		return nil
	}
	return in.Extra.Defs
}

// SetDefs replaces the extra register results.
func (in *Instr) SetDefs(defs []Ref) { in.extra().Defs = defs }

// AddDef appends one extra register result.
func (in *Instr) AddDef(def Ref) { e := in.extra(); e.Defs = append(e.Defs, def) }
