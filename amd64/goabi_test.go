package amd64

import (
	"fmt"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the pure argument-frame computation in goabi.go. Everything
// under test is a function of an *ir.Func or an *ir.Instr, so nothing here builds
// a module, lowers, or emits: an off-by-one in this layer propagates into every
// managed frame, and it is far cheaper to pin the arithmetic directly than to
// read it back out of machine code.

// goFunc returns a Go-ABIInternal, managed-frame function.
func goFunc(name string) *ir.Func {
	f := ir.NewModule().NewFuncVoid(name)
	f.CallConv = ir.CallConvGoInternal
	f.ManagedFrame = true
	return f
}

// platformFunc returns a managed-frame function that still uses the platform
// (System V) argument convention -- the shape goc emits for the runtime's
// C-shaped helpers.
func platformFunc(name string) *ir.Func {
	f := ir.NewModule().NewFuncVoid(name)
	f.ManagedFrame = true
	return f
}

// aggregateParam adds a by-value aggregate parameter, mirroring what the parser
// does for ":type %p".
func aggregateParam(f *ir.Func, name string, aggregate *ir.AggType) {
	ref := f.NewTemp(name, ir.ClsL)
	temp := f.Temp(ref)
	temp.Agg = aggregate
	f.Params = append(f.Params, temp)
}

// goSliceType is the canonical three-word Go value with a pointer in word 0.
func goSliceType() *ir.AggType {
	return &ir.AggType{Name: "slice", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true},
		{Sub: ir.SubL},
		{Sub: ir.SubL},
	}}
}

// goCallInstr builds a bare ABIInternal call instruction over the given
// arguments. Only the fields the sizing code reads are set.
func goCallInstr(f *ir.Func, args ...ir.Ref) *ir.Instr {
	return &ir.Instr{
		Op:   ir.OCall,
		Args: append([]ir.Ref{f.Sym("callee", 0)}, args...),
	}
}

// spillRegs is the register sequence of a spill list, which is what most of the
// assignment assertions are really about.
func spillRegs(spills []goRegisterSpill) []Reg {
	regs := make([]Reg, len(spills))
	for index, spill := range spills {
		regs[index] = spill.reg
	}
	return regs
}

// spillOffsets is the home-slot offset sequence of a spill list.
func spillOffsets(spills []goRegisterSpill) []int {
	offsets := make([]int, len(spills))
	for index, spill := range spills {
		offsets[index] = spill.offset
	}
	return offsets
}

// ---------------------------------------------------------------------------
// Register assignment across the two banks
// ---------------------------------------------------------------------------

// Integer and float arguments consume independent banks, so an interleaved
// signature must not let one bank's consumption advance the other's.
func TestGoArgumentFrameAssignsIntegerAndFloatBanksIndependently(t *testing.T) {
	f := goFunc("interleaved")
	f.Param("a", ir.ClsL)
	f.Param("b", ir.ClsD)
	f.Param("c", ir.ClsL)
	f.Param("d", ir.ClsD)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RAX, XMM(0), RBX, XMM(1)}, spillRegs(frame.spills))
	require.Equal(t, []int{0, 8, 16, 24}, spillOffsets(frame.spills))
	require.Equal(t, []bool{false, true, false, true}, []bool{
		frame.spills[0].float, frame.spills[1].float, frame.spills[2].float, frame.spills[3].float,
	})
	require.Equal(t, 32, frame.size)
	require.Empty(t, frame.pointerWords)
}

// The integer bank is Go's own nine registers. The tenth integer argument moves
// to the stack, and the nine register arguments' home slots start above it.
func TestGoArgumentFrameExhaustsTheIntegerBankAtNine(t *testing.T) {
	f := goFunc("ten_words")
	for index := 0; index < 10; index++ {
		f.Param(fmt.Sprintf("w%d", index), ir.ClsL)
	}

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11}, spillRegs(frame.spills),
		"the nine ABIInternal integer argument registers, in Go's order")
	require.Equal(t, []int{8, 16, 24, 32, 40, 48, 56, 64, 72}, spillOffsets(frame.spills),
		"the tenth argument occupies [0,8), so the home slots start at 8")
	require.Equal(t, 80, frame.size)
}

// The float bank is capped at thirteen. Exhausting it is only legal when Go's own
// fifteen-register table would have run out too -- here the aggregate needs three
// registers and neither table has three left -- otherwise the signature is
// refused (see TestGoArgumentFrameRefusesSignatureExceedingTheFloatCap).
func TestGoArgumentFrameExhaustsTheFloatBankAtThirteen(t *testing.T) {
	triple := &ir.AggType{Name: "triple", Fields: []ir.Field{
		{Sub: ir.SubD}, {Sub: ir.SubD}, {Sub: ir.SubD},
	}}
	f := goFunc("thirteen_doubles_then_triple")
	for index := 0; index < 13; index++ {
		f.Param(fmt.Sprintf("d%d", index), ir.ClsD)
	}
	aggregateParam(f, "rest", triple)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Len(t, frame.spills, 13, "only the thirteen scalars are register-assigned")
	require.Equal(t, XMM(0), frame.spills[0].reg)
	require.Equal(t, XMM(12), frame.spills[12].reg, "goArgFP stops at X12")
	require.Equal(t, 24, frame.spills[0].offset, "the stacked aggregate occupies [0,24)")
	require.Equal(t, 128, frame.size)
}

// ---------------------------------------------------------------------------
// Stacked-argument packing
// ---------------------------------------------------------------------------

// ABIInternal packs stacked arguments at their natural alignment. Two words and
// a long occupy 16 bytes, not 24.
func TestGoArgumentFramePacksStackedArgumentsAtNaturalAlignment(t *testing.T) {
	f := goFunc("packed_stack")
	for index := 0; index < 9; index++ {
		f.Param(fmt.Sprintf("w%d", index), ir.ClsL)
	}
	f.Param("s0", ir.ClsW)
	f.Param("s1", ir.ClsW)
	f.Param("s2", ir.ClsL)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, 16, frame.spills[0].offset,
		"the two words pack into [0,8) and the long follows at [8,16)")
	require.Equal(t, 88, frame.size)
}

// System V does not pack: every stacked scalar takes a whole eightbyte. The
// contrast is what makes the packing above a real rule rather than an accident.
func TestPlatformArgumentFrameGivesEachStackedScalarAWholeEightbyte(t *testing.T) {
	f := platformFunc("sysv_stack")
	for index := 0; index < 6; index++ {
		f.Param(fmt.Sprintf("w%d", index), ir.ClsL)
	}
	f.Param("s0", ir.ClsW)
	f.Param("s1", ir.ClsW)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RDI, RSI, RDX, RCX, R8, R9}, spillRegs(frame.spills),
		"the System V integer argument registers, not the Go ones")
	require.Equal(t, 16, frame.spills[0].offset,
		"the two stacked words take 8 bytes each under System V")
	require.Equal(t, 64, frame.size)
}

// ---------------------------------------------------------------------------
// Aggregates
// ---------------------------------------------------------------------------

// A Go aggregate is decomposed by field: each field gets its own register, and a
// mixed struct straddles both banks.
func TestGoArgumentFrameSplitsAggregateFieldsAcrossBanks(t *testing.T) {
	mixed := &ir.AggType{Name: "mixed", Fields: []ir.Field{{Sub: ir.SubL}, {Sub: ir.SubD}}}
	f := goFunc("mixed_aggregate")
	aggregateParam(f, "value", mixed)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RAX, XMM(0)}, spillRegs(frame.spills))
	require.Equal(t, []int{0, 8}, spillOffsets(frame.spills))
	require.False(t, frame.spills[0].float)
	require.True(t, frame.spills[1].float)
	require.Equal(t, 16, frame.size)
}

// A three-word slice fits entirely in registers and is homed as one group, with
// its pointer word named only in the entry map.
func TestGoArgumentFrameHomesFullyRegisterAggregate(t *testing.T) {
	f := goFunc("consume_slice")
	f.ParamGroup("value", goSliceType(), ir.ClsP, ir.ClsL, ir.ClsL)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RAX, RBX, RCX}, spillRegs(frame.spills))
	require.Equal(t, []int{0, 8, 16}, spillOffsets(frame.spills))
	require.True(t, frame.spills[0].pointer)
	require.False(t, frame.spills[1].pointer)
	require.Equal(t, 24, frame.size)
	require.Equal(t, []int{0}, frame.pointerWords)
	require.Empty(t, frame.incomingPointerWords,
		"a home slot is written only by the prologue, so it is not a root for the whole call")
}

// With seven integer arguments ahead of it the slice's three words no longer fit,
// so the whole value moves to the stack -- it is never split. Its pointer word is
// caller-written and therefore in both maps.
func TestGoArgumentFrameMovesWholeAggregateToStackWhenItNoLongerFits(t *testing.T) {
	f := goFunc("consume_slice_after_seven")
	for index := 0; index < 7; index++ {
		f.Param(fmt.Sprintf("w%d", index), ir.ClsL)
	}
	f.ParamGroup("value", goSliceType(), ir.ClsP, ir.ClsL, ir.ClsL)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Len(t, frame.spills, 7, "only the seven scalars are register-assigned")
	require.Equal(t, 24, frame.spills[0].offset, "the stacked slice occupies [0,24)")
	require.Equal(t, 80, frame.size)
	require.Equal(t, []int{0}, frame.incomingPointerWords)
	require.Equal(t, []int{0}, frame.pointerWords)
}

// The boundary itself: six integer arguments leave exactly three registers, so
// the slice still fits and takes the last three.
func TestGoArgumentFrameKeepsAggregateInRegistersAtTheExactBoundary(t *testing.T) {
	f := goFunc("consume_slice_after_six")
	for index := 0; index < 6; index++ {
		f.Param(fmt.Sprintf("w%d", index), ir.ClsL)
	}
	f.ParamGroup("value", goSliceType(), ir.ClsP, ir.ClsL, ir.ClsL)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11}, spillRegs(frame.spills))
	require.Equal(t, 48, frame.spills[6].offset, "the aggregate group follows the six scalars")
	require.True(t, frame.spills[6].pointer)
	require.Equal(t, 72, frame.size)
	require.Equal(t, []int{6}, frame.pointerWords)
	require.Empty(t, frame.incomingPointerWords)
}

// A zero-sized aggregate consumes no register and no home slot.
func TestGoArgumentFrameDoesNotHomeEmptyAggregate(t *testing.T) {
	f := goFunc("consume_empty")
	aggregateParam(f, "value", &ir.AggType{Name: "empty"})

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Empty(t, frame.spills)
	require.Zero(t, frame.size)
}

// An aggregate with no register-assignable flat form -- here a non-trivial inline
// array -- travels as memory. Its pointer words still have to reach the
// collector, which is why pointer discovery uses ir.AggregatePointerOffsets and
// not the flattening.
func TestGoArgumentFrameDiscoversPointersInUnflattenableStackedAggregate(t *testing.T) {
	array := &ir.AggType{Name: "ptrarray", Fields: []ir.Field{
		{Sub: ir.SubL, Pointer: true, Count: 3},
	}}
	f := goFunc("consume_pointer_array")
	aggregateParam(f, "value", array)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Empty(t, frame.spills, "an unflattenable aggregate is never register-assigned")
	require.Equal(t, []int{0, 1, 2}, frame.incomingPointerWords)
	require.Equal(t, []int{0, 1, 2}, frame.pointerWords)
	require.Equal(t, 24, frame.size)
}

// ---------------------------------------------------------------------------
// The two pointer maps
// ---------------------------------------------------------------------------

// A register pointer argument's home slot is written only on the path that calls
// morestack, so it belongs to the entry map alone; a stack-passed pointer is
// caller-written and belongs to both.
func TestGoArgumentFrameSeparatesIncomingArgumentsFromRegisterHomes(t *testing.T) {
	f := goFunc("consume_ten_pointers")
	f.ParamRef("registerPointer")
	for index := 0; index < 8; index++ {
		f.Param(fmt.Sprintf("w%d", index), ir.ClsL)
	}
	f.ParamRef("stackPointer")

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []int{0}, frame.incomingPointerWords, "only the stack-passed argument is caller-written")
	require.Equal(t, []int{0, 1}, frame.pointerWords, "the entry map adds the first register's home slot")
	require.Equal(t, RAX, frame.spills[0].reg)
	require.True(t, frame.spills[0].pointer)
	require.Equal(t, 8, frame.spills[0].offset)
	require.Equal(t, 80, frame.size)
}

// ---------------------------------------------------------------------------
// The closure context slot
// ---------------------------------------------------------------------------

// The ABIInternal closure pointer arrives in RDX and is live at entry, so
// morestack must be able to find it: it gets a home slot and a pointer word.
func TestGoArgumentFrameHomesTheClosureContext(t *testing.T) {
	f := goFunc("closure_body")
	f.HasClosureContext = true

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{regClosure}, spillRegs(frame.spills))
	require.Equal(t, RDX, regClosure, "Go's amd64 closure register")
	require.Equal(t, 0, frame.spills[0].offset)
	require.True(t, frame.spills[0].pointer)
	require.Equal(t, 8, frame.size)
	require.Equal(t, []int{0}, frame.pointerWords)
}

// ---------------------------------------------------------------------------
// System V managed frames
// ---------------------------------------------------------------------------

// A managed-frame function with platform-ABI arguments still needs home slots,
// but they must describe System V's registers, not Go's.
func TestPlatformArgumentFrameHomesSystemVRegisters(t *testing.T) {
	f := platformFunc("managed_helper")
	f.ParamRef("root")
	f.Param("n", ir.ClsL)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RDI, RSI}, spillRegs(frame.spills))
	require.True(t, frame.spills[0].pointer)
	require.False(t, frame.spills[1].pointer)
	require.Equal(t, 16, frame.size)
	require.Equal(t, []int{0}, frame.pointerWords)
}

// A MEMORY-class System V result reserves the first integer argument register for
// the caller's buffer pointer, ahead of the arguments -- exactly as lowerParams
// does -- and that pointer needs a home slot of its own.
func TestPlatformArgumentFrameReservesTheStructReturnPointer(t *testing.T) {
	big := &ir.AggType{Name: "big", Fields: []ir.Field{
		{Sub: ir.SubL}, {Sub: ir.SubL}, {Sub: ir.SubL}, {Sub: ir.SubL},
	}}
	f := platformFunc("returns_big")
	f.RetAgg = big
	f.Param("n", ir.ClsL)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RDI, RSI}, spillRegs(frame.spills),
		"RDI carries the sret pointer, so the first real argument lands in RSI")
	require.True(t, frame.spills[0].pointer)
	require.Equal(t, 16, frame.size)
}

// A System V register-class aggregate is classified by eightbyte, not by field:
// a {w,w} pair travels in one integer register even though it has two fields.
func TestPlatformArgumentFrameClassifiesAggregatesByEightbyte(t *testing.T) {
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	f := platformFunc("sysv_pair")
	aggregateParam(f, "value", pair)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, []Reg{RDI}, spillRegs(frame.spills), "one eightbyte, one register")
	require.Equal(t, 8, frame.size)
}

// ---------------------------------------------------------------------------
// Loud refusals
// ---------------------------------------------------------------------------

// Every amd64 spill slot is eight bytes, so a 128-bit home slot would overrun its
// neighbour. The refusal must name the parameter rather than truncate the value.
func TestGoArgumentFrameRefusesQuadScalarParameter(t *testing.T) {
	f := goFunc("takes_quad")
	f.Param("wide", ir.ClsQ)

	_, err := goArgumentFrameFor(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"takes_quad"`, "the error must name the offending function")
	assert.Contains(t, err.Error(), `"wide"`, "the error must name the offending parameter")
	assert.Contains(t, err.Error(), "128-bit")
	assert.Contains(t, err.Error(), "8 bytes")
}

// The same refusal applies to a quad hiding inside an aggregate, where a silent
// truncation would be even harder to see.
func TestGoArgumentFrameRefusesQuadAggregateField(t *testing.T) {
	withQuad := &ir.AggType{Name: "withquad", Fields: []ir.Field{
		{Sub: ir.SubL},
		{Sub: ir.SubQ},
	}}
	f := goFunc("takes_quad_field")
	aggregateParam(f, "value", withQuad)

	_, err := goArgumentFrameFor(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"takes_quad_field"`)
	assert.Contains(t, err.Error(), `"value"`)
	assert.Contains(t, err.Error(), "field 1")
	assert.Contains(t, err.Error(), "128-bit")
}

// The System V managed path refuses a quad too: its home slots are the same
// eight-byte slots.
func TestPlatformArgumentFrameRefusesQuadScalarParameter(t *testing.T) {
	f := platformFunc("sysv_takes_quad")
	f.Param("wide", ir.ClsQ)

	_, err := goArgumentFrameFor(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"wide"`)
	assert.Contains(t, err.Error(), "128-bit")
}

// A quad inside an aggregate that cannot flatten is *not* refused: it travels as
// opaque memory and never reaches a register or a home slot. Refusing it would
// reject working code.
func TestGoArgumentFrameAcceptsQuadInsideUnflattenableAggregate(t *testing.T) {
	array := &ir.AggType{Name: "quadarray", Fields: []ir.Field{{Sub: ir.SubQ, Count: 2}}}
	f := goFunc("takes_quad_array")
	aggregateParam(f, "value", array)

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Empty(t, frame.spills)
	require.Equal(t, 32, frame.size)
}

// cg12 caps ABIInternal at thirteen float argument registers because X13/X14 are
// the emitter's float scratch pair and X15 is Go's zero register. A signature
// that Go would have given a fourteenth must be refused by name: passing it on
// the stack would be self-consistent between cg12 functions and wrong against the
// runtime's abi.RegArgs, which is sized by Go's FloatArgRegs = 15.
func TestGoArgumentFrameRefusesSignatureExceedingTheFloatCap(t *testing.T) {
	f := goFunc("fourteen_doubles")
	for index := 0; index < 14; index++ {
		f.Param(fmt.Sprintf("d%d", index), ir.ClsD)
	}

	_, err := goArgumentFrameFor(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"fourteen_doubles"`, "the error must name the offending function")
	assert.Contains(t, err.Error(), "14 float argument registers")
	assert.Contains(t, err.Error(), "X0..X12")
	assert.Contains(t, err.Error(), "abi.RegArgs")
	assert.True(t, goFloatArgRegSpill(14), "the predicate this refusal is derived from")
	assert.False(t, goFloatArgRegSpill(13))
}

// Thirteen is still fine -- the cap is a refusal, not a narrowing.
func TestGoArgumentFrameAcceptsThirteenFloatArguments(t *testing.T) {
	f := goFunc("thirteen_doubles")
	for index := 0; index < 13; index++ {
		f.Param(fmt.Sprintf("d%d", index), ir.ClsD)
	}

	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Len(t, frame.spills, 13)
	require.Equal(t, XMM(12), frame.spills[12].reg)
}

// The refusal also fires on a result, whose registers are a separate bank and so
// are checked separately.
func TestGoArgumentFrameRefusesResultExceedingTheFloatCap(t *testing.T) {
	fields := make([]ir.Field, 14)
	for index := range fields {
		fields[index] = ir.Field{Sub: ir.SubD}
	}
	f := goFunc("returns_fourteen_doubles")
	f.RetAgg = &ir.AggType{Name: "fourteen", Fields: fields}

	_, err := goArgumentFrameFor(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "result of function")
	assert.Contains(t, err.Error(), `"returns_fourteen_doubles"`)
	assert.Contains(t, err.Error(), "14 float argument registers")
}

// A call site is checked the same way, so a refusal cannot be dodged by putting
// the signature on the caller's side.
func TestGoCallStackBytesRefusesCallExceedingTheFloatCap(t *testing.T) {
	f := goFunc("caller")
	args := make([]ir.Ref, 14)
	for index := range args {
		args[index] = f.NewTemp(fmt.Sprintf("d%d", index), ir.ClsD)
	}

	_, err := goCallStackBytes(f, goCallInstr(f, args...), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `call in function "caller"`)
	assert.Contains(t, err.Error(), "14 float argument registers")
}

// System V classifies aggregates by eightbyte; the Go walk decomposes by field.
// Feeding a System V assigner to the Go walk would produce a plausible but wrong
// assignment, so it is a programming error rather than a silent one.
func TestAssignGoAggregateRejectsASystemVAssigner(t *testing.T) {
	assigner := newArgAssigner(false)
	defer func() {
		recovered := recover()
		require.NotNil(t, recovered, "assignGoAggregate must not accept a System V assigner")
		assert.Contains(t, fmt.Sprint(recovered), "System V assigner")
		assert.Contains(t, fmt.Sprint(recovered), "eightbyte")
	}()
	assignGoAggregate(&assigner, goSliceType())
}

// ---------------------------------------------------------------------------
// stackLinkBytes = 0
// ---------------------------------------------------------------------------

// amd64 reserves no frame-chain link: the CALL instruction pushes the return
// address itself, so the eight bytes arm64 reserves by hand (goStackLinkSize)
// already exist below the argument frame base. Every offset here is measured
// from the argument frame base with no link term, and reintroducing one would
// double-count eight bytes in every frame and at every call site.
func TestArgumentFrameReservesNoFrameChainLink(t *testing.T) {
	require.Zero(t, conventionABI(ir.CallConvGoInternal).stackLinkBytes,
		"the ABIInternal descriptor must not reserve a link")
	require.Zero(t, conventionABI(ir.CallConvPlatform).stackLinkBytes,
		"neither must the platform descriptor")

	// The first home slot of a function with no stack arguments is at 0, not 8.
	callee := goFunc("one_word")
	callee.Param("a", ir.ClsL)
	frame, err := goArgumentFrameFor(callee)
	require.NoError(t, err)
	require.Equal(t, 0, frame.spills[0].offset, "no link precedes the first home slot")
	require.Equal(t, 8, frame.size, "one home slot, and nothing else")

	// A stack-passed pointer argument is word 0 of the argument map, not word 1.
	stacked := goFunc("ten_pointers")
	for index := 0; index < 9; index++ {
		stacked.Param(fmt.Sprintf("w%d", index), ir.ClsL)
	}
	stacked.ParamRef("stackPointer")
	stackedFrame, err := goArgumentFrameFor(stacked)
	require.NoError(t, err)
	require.Equal(t, []int{0}, stackedFrame.incomingPointerWords,
		"the first stacked argument is word 0; a link would push it to word 1")

	// Two register arguments need 16 bytes of home slots at a call site. With a
	// link term it would round to 32.
	caller := goFunc("caller")
	first := caller.NewTemp("a", ir.ClsL)
	second := caller.NewTemp("b", ir.ClsL)
	bytes, err := goCallStackBytes(caller, goCallInstr(caller, first, second), 0)
	require.NoError(t, err)
	require.Equal(t, 16, bytes, "16 bytes of home slots, not 8 + 16 rounded to 32")
}

// The two biases B2 needs to turn a frame offset into an address are the return
// address the CALL pushed, and that plus the saved RBP.
func TestArgumentFrameBasesMatchTheAmd64Prologue(t *testing.T) {
	require.Equal(t, 8, goArgumentFrameFromEntrySP, "the CALL pushed an 8-byte return address")
	require.Equal(t, 16, goArgumentFrameFromFramePointer, "push %rbp adds another 8")
}

// ---------------------------------------------------------------------------
// Call-area sizing
// ---------------------------------------------------------------------------

// Every register argument gets a home slot in the caller's outgoing area, so the
// callee's prologue has somewhere to spill it before calling morestack.
func TestGoCallStackBytesReservesHomeSlotsForRegisterArguments(t *testing.T) {
	f := goFunc("caller")
	args := make([]ir.Ref, 3)
	for index := range args {
		args[index] = f.NewTemp(fmt.Sprintf("a%d", index), ir.ClsL)
	}

	bytes, err := goCallStackBytes(f, goCallInstr(f, args...), 0)
	require.NoError(t, err)
	require.Equal(t, 32, bytes, "three 8-byte home slots, rounded to the 16-byte stack alignment")
}

// Stack arguments sit at the bottom of the area and the home slots follow them.
func TestGoCallStackBytesPlacesHomeSlotsAboveStackArguments(t *testing.T) {
	f := goFunc("caller")
	args := make([]ir.Ref, 10)
	for index := range args {
		args[index] = f.NewTemp(fmt.Sprintf("a%d", index), ir.ClsL)
	}

	bytes, err := goCallStackBytes(f, goCallInstr(f, args...), 0)
	require.NoError(t, err)
	require.Equal(t, 80, bytes, "8 bytes of stack argument plus nine 8-byte home slots")
}

// A closure call also passes the context pointer, which needs its own slot.
func TestGoCallStackBytesAddsTheClosureContextSlot(t *testing.T) {
	f := goFunc("caller")
	first := f.NewTemp("a", ir.ClsL)
	second := f.NewTemp("b", ir.ClsL)

	plain := goCallInstr(f, first, second)
	plainBytes, err := goCallStackBytes(f, plain, 0)
	require.NoError(t, err)
	require.Equal(t, 16, plainBytes)

	closure := goCallInstr(f, first, second)
	closure.ClosureCall = true
	closureBytes, err := goCallStackBytes(f, closure, 0)
	require.NoError(t, err)
	require.Equal(t, 32, closureBytes, "16 bytes of home slots plus the 8-byte context slot, rounded to 16")
}

// A call with nothing to pass reserves nothing.
func TestGoCallStackBytesIsZeroForAnEmptyCall(t *testing.T) {
	f := goFunc("caller")
	bytes, err := goCallStackBytes(f, goCallInstr(f), 0)
	require.NoError(t, err)
	require.Zero(t, bytes)
}

// The caller's stack-assigned results sit below the home slots, so a non-zero
// resultEnd pushes them up.
func TestGoCallStackBytesPlacesHomeSlotsAboveStackResults(t *testing.T) {
	f := goFunc("caller")
	arg := f.NewTemp("a", ir.ClsL)

	bytes, err := goCallStackBytes(f, goCallInstr(f, arg), 24)
	require.NoError(t, err)
	require.Equal(t, 32, bytes, "24 bytes of stack results plus one home slot, rounded to 16")
}

// A managed System V call site reserves home slots too, using System V's
// register count.
func TestPlatformCallStackBytesReservesHomeSlotsForSystemVRegisters(t *testing.T) {
	f := platformFunc("caller")
	args := make([]ir.Ref, 8)
	for index := range args {
		args[index] = f.NewTemp(fmt.Sprintf("a%d", index), ir.ClsL)
	}

	bytes, err := platformCallStackBytes(f, goCallInstr(f, args...))
	require.NoError(t, err)
	require.Equal(t, 64, bytes,
		"two 8-byte stack arguments below six 8-byte home slots")
}

// ---------------------------------------------------------------------------
// goRegisterSpills
// ---------------------------------------------------------------------------

// goRegisterSpills is the B1 -> B2 contract: the prologue asks only for the
// spills, and gets exactly the argument frame's.
func TestGoRegisterSpillsMatchTheArgumentFrame(t *testing.T) {
	f := goFunc("consume_slice")
	f.ParamGroup("value", goSliceType(), ir.ClsP, ir.ClsL, ir.ClsL)

	spills, err := goRegisterSpills(f)
	require.NoError(t, err)
	frame, err := goArgumentFrameFor(f)
	require.NoError(t, err)
	require.Equal(t, frame.spills, spills)
}

// A refusal reaches the prologue as an error rather than as a short spill list.
func TestGoRegisterSpillsPropagatesRefusals(t *testing.T) {
	f := goFunc("takes_quad")
	f.Param("wide", ir.ClsQ)

	spills, err := goRegisterSpills(f)
	require.Error(t, err)
	require.Nil(t, spills)
	assert.True(t, strings.Contains(err.Error(), "128-bit"))
}
