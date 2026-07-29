package arm64

import (
	"fmt"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A call's calling convention is a property of the callee, never of the function
// the call happens to sit in. goc marks method-value wrappers, function literals,
// and funcvalue adapters CallConvGoInternal, and those functions make ordinary
// unmarked direct calls out to plain CallConvPlatform functions; resolving such a
// call from the enclosing function lowers it as ABIInternal against an AAPCS64
// callee. The tests below pin each of the four resolution paths.

// callLowering is what one lowered call site looks like, at the level of detail
// the ABI decides: which registers the arguments went to, which went to the
// stack and at what offset, the convention stamped on the call, and the
// outgoing-argument size.
type callLowering struct {
	argRegs      []Reg
	stackOffsets []int64
	conv         ir.CallConvention
	convSet      bool
	stackBytes   int64
}

// lowerOneCall lowers function and returns its single call site's ABI shape.
func lowerOneCall(t *testing.T, function *ir.Func) callLowering {
	t.Helper()
	ir.LowerPointers(function, ptrCls)
	require.NoError(t, lower(function, moduleConventions(function), TLSLocalExec))

	got := callLowering{}
	calls := 0
	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			switch instruction.Op {
			case ir.OArg:
				if instruction.To.IsNone() {
					got.stackOffsets = append(got.stackOffsets, instruction.Aux)
					continue
				}
				temporary := function.Temp(instruction.To)
				require.True(t, temporary.Fixed, "an OArg destination must be a pinned ABI register")
				got.argRegs = append(got.argRegs, Reg(temporary.Reg))
			case ir.OCall:
				calls++
				got.conv, got.convSet, got.stackBytes = instruction.CallConv, instruction.CallConvSet, instruction.Aux
			}
		}
	}
	require.Equal(t, 1, calls, "the fixture must contain exactly one call")
	return got
}

// nineArgumentCaller builds a caller with the given convention that makes a
// single unmarked direct call, passing nine integers to callee.
func nineArgumentCaller(name string, callerConv ir.CallConvention, callee *ir.Func) *ir.Func {
	module := callee.Module()
	caller := module.NewFuncVoid(name)
	caller.CallConv = callerConv
	entry := caller.Entry()
	arguments := make([]ir.Ref, 9)
	for index := range arguments {
		arguments[index] = caller.Long(int64(index + 1))
	}
	entry.Call(ir.ClsW, caller.Sym(callee.Name, 0), arguments...)
	entry.RetVoid()
	return caller
}

// The regression this whole file exists for. Nine integer arguments is the
// smallest case where AAPCS64 and ABIInternal disagree about registers: AAPCS64
// runs out after x7 and puts the ninth on the stack, ABIInternal continues into
// x8. Under the old enclosing-function fallback the ABIInternal caller passed
// the ninth argument in x8 while the platform-ABI callee read it from the stack.
func TestUnmarkedCallResolvesCalleeConventionNotCallerConvention(t *testing.T) {
	platformModule := ir.NewModule()
	platformCallee := platformModule.NewFuncVoid("plain_callee")
	platformCallee.CallConv = ir.CallConvPlatform
	fromPlatform := lowerOneCall(t, nineArgumentCaller("platform_caller", ir.CallConvPlatform, platformCallee))

	goModule := ir.NewModule()
	goCallee := goModule.NewFuncVoid("plain_callee")
	goCallee.CallConv = ir.CallConvPlatform
	fromGoInternal := lowerOneCall(t, nineArgumentCaller("go_internal_caller", ir.CallConvGoInternal, goCallee))

	assert.Equal(t, []Reg{X0, X1, X2, X3, X4, X5, X6, X7}, fromPlatform.argRegs,
		"AAPCS64 passes eight integers in x0..x7")
	assert.Equal(t, []int64{0}, fromPlatform.stackOffsets,
		"AAPCS64 puts the ninth integer on the stack")

	assert.Equal(t, fromPlatform.argRegs, fromGoInternal.argRegs,
		"the callee is platform ABI, so the caller's own convention must not move an argument into x8")
	assert.Equal(t, fromPlatform.stackOffsets, fromGoInternal.stackOffsets,
		"the ninth argument belongs on the stack, where the platform-ABI callee reads it")
	assert.Equal(t, ir.CallConvPlatform, fromGoInternal.conv,
		"the lowered call must be stamped with the callee's convention")
	assert.True(t, fromGoInternal.convSet)
	assert.Equal(t, fromPlatform.stackBytes, fromGoInternal.stackBytes,
		"both callers reserve the same outgoing-argument area")
}

// Rule 1: an explicit convention on the call instruction wins over everything.
// This is how every closure and indirect call is marked, and it is the only way
// an ABIInternal function is ever reached.
func TestExplicitCallConventionOverridesCalleeLookup(t *testing.T) {
	module := ir.NewModule()
	callee := module.NewFuncVoid("platform_callee")
	callee.CallConv = ir.CallConvPlatform

	caller := module.NewFuncVoid("marked_caller")
	entry := caller.Entry()
	arguments := make([]ir.Ref, 9)
	for index := range arguments {
		arguments[index] = caller.Long(int64(index + 1))
	}
	entry.CallVoidWithConvention(ir.CallConvGoInternal, caller.Sym("platform_callee", 0), arguments...)
	entry.RetVoid()

	got := lowerOneCall(t, caller)
	assert.Equal(t, ir.CallConvGoInternal, got.conv)
	assert.Equal(t, []Reg{X0, X1, X2, X3, X4, X5, X6, X7, X8}, got.argRegs,
		"the explicit ABIInternal marking must win, ninth argument in x8")
	assert.Empty(t, got.stackOffsets)
}

// Rule 2: an unmarked call naming a symbol this object defines takes that
// callee's own convention.
func TestUnmarkedCallToDefinedGoInternalCalleeResolvesGoInternal(t *testing.T) {
	module := ir.NewModule()
	callee := module.NewFuncVoid("go_internal_callee")
	callee.CallConv = ir.CallConvGoInternal

	got := lowerOneCall(t, nineArgumentCaller("platform_caller", ir.CallConvPlatform, callee))
	assert.Equal(t, ir.CallConvGoInternal, got.conv,
		"the callee is ABIInternal, so a platform caller must still use ABIInternal at the call")
	assert.Equal(t, []Reg{X0, X1, X2, X3, X4, X5, X6, X7, X8}, got.argRegs)
	assert.Empty(t, got.stackOffsets)
}

// Rule 3: a symbol this object does not define is platform ABI. ABIInternal only
// ever exists inside a module cg12 compiled, so an external C helper or another
// object's symbol cannot be reached with it.
func TestUnmarkedCallToUndefinedSymbolResolvesPlatform(t *testing.T) {
	module := ir.NewModule()
	caller := module.NewFuncVoid("go_internal_caller")
	caller.CallConv = ir.CallConvGoInternal
	entry := caller.Entry()
	arguments := make([]ir.Ref, 9)
	for index := range arguments {
		arguments[index] = caller.Long(int64(index + 1))
	}
	entry.Call(ir.ClsW, caller.Sym("c_helper_defined_elsewhere", 0), arguments...)
	entry.RetVoid()

	got := lowerOneCall(t, caller)
	assert.Equal(t, ir.CallConvPlatform, got.conv)
	assert.Equal(t, []Reg{X0, X1, X2, X3, X4, X5, X6, X7}, got.argRegs)
	assert.Equal(t, []int64{0}, got.stackOffsets)
}

// Registers are not the only thing the two conventions disagree about, so the
// register-overlap argument does not make the rest safe either. Eighteen 4-byte
// arguments overflow both banks: AAPCS64 spills ten of them and gives each a
// whole 8-byte slot, with no frame link ahead of the area; ABIInternal spills two
// and packs them at their natural 4-byte size, behind an 8-byte frame-chain link.
// The caller is ABIInternal in both cases, so the layout is decided purely by the
// callee.
func TestStackedArgumentPackingFollowsCalleeConvention(t *testing.T) {
	build := func(calleeConv ir.CallConvention) callLowering {
		module := ir.NewModule()
		callee := module.NewFuncVoid("callee")
		callee.CallConv = calleeConv
		caller := module.NewFuncVoid("go_internal_caller")
		caller.CallConv = ir.CallConvGoInternal
		entry := caller.Entry()
		arguments := make([]ir.Ref, 18)
		for index := range arguments {
			arguments[index] = caller.Word(int64(index + 1))
		}
		entry.Call(ir.ClsW, caller.Sym("callee", 0), arguments...)
		entry.RetVoid()
		return lowerOneCall(t, caller)
	}

	platform := build(ir.CallConvPlatform)
	require.Equal(t, ir.CallConvPlatform, platform.conv)
	assert.Equal(t, []int64{0, 8, 16, 24, 32, 40, 48, 56, 64, 72}, platform.stackOffsets,
		"AAPCS64 gives each stacked scalar a whole 8-byte slot")
	assert.Equal(t, 0, stackLinkBytesFor(false),
		"AAPCS64 reserves no frame-chain link ahead of the stacked arguments")

	goInternal := build(ir.CallConvGoInternal)
	require.Equal(t, ir.CallConvGoInternal, goInternal.conv)
	assert.Equal(t, []int64{0, 4}, goInternal.stackOffsets,
		"ABIInternal packs stacked arguments at their natural size")
	assert.Equal(t, goStackLinkSize, stackLinkBytesFor(true),
		"ABIInternal threads the caller's frame pointer through a link ahead of them")

	assert.NotEqual(t, platform.stackOffsets, goInternal.stackOffsets,
		"the two layouts must actually differ, or this test proves nothing")
}

// The emitter must resolve every call exactly as the lowering did. It reads the
// convention through the same index, so a whole object's calls agree end to end;
// were they to disagree the argument setup and the call sequence would use
// different ABIs for one call.
func TestEmitterResolvesCallConventionAsLoweringDid(t *testing.T) {
	module := ir.NewModule()
	module.NewFuncVoid("platform_callee")
	goCallee := module.NewFuncVoid("go_internal_callee")
	goCallee.CallConv = ir.CallConvGoInternal

	caller := module.NewFuncVoid("mixed_caller")
	caller.CallConv = ir.CallConvGoInternal
	entry := caller.Entry()
	arguments := make([]ir.Ref, 9)
	for index := range arguments {
		arguments[index] = caller.Long(int64(index + 1))
	}
	entry.Call(ir.ClsW, caller.Sym("platform_callee", 0), arguments...)
	entry.Call(ir.ClsW, caller.Sym("go_internal_callee", 0), arguments...)
	entry.Call(ir.ClsW, caller.Sym("undefined_helper", 0), arguments...)
	entry.RetVoid()

	conventions := newCalleeConventions(module.Funcs)
	ir.LowerPointers(caller, ptrCls)
	require.NoError(t, lower(caller, conventions, TLSLocalExec))

	allocation, err := regAlloc(caller)
	require.NoError(t, err)
	machine, err := emitMachine(caller, allocation, conventions, nil, TLSLocalExec)
	require.NoError(t, err)
	require.NotNil(t, machine)

	want := []ir.CallConvention{ir.CallConvPlatform, ir.CallConvGoInternal, ir.CallConvPlatform}
	index := 0
	for _, block := range caller.Blocks {
		for i := range block.Instrs {
			instruction := &block.Instrs[i]
			if instruction.Op != ir.OCall {
				continue
			}
			require.Less(t, index, len(want))
			assert.Equal(t, want[index], instruction.CallConv,
				fmt.Sprintf("call %d was lowered against the wrong convention", index))
			assert.Equal(t, want[index] == ir.CallConvGoInternal,
				machine.m.conventions.goInternalCall(caller, instruction),
				fmt.Sprintf("the emitter disagrees with the lowering about call %d", index))
			index++
		}
	}
	assert.Equal(t, len(want), index)
}

// forCall's own contract, independent of any lowering.
func TestCalleeConventionsForCall(t *testing.T) {
	module := ir.NewModule()
	platformCallee := module.NewFuncVoid("platform_callee")
	goCallee := module.NewFuncVoid("go_callee")
	goCallee.CallConv = ir.CallConvGoInternal

	caller := module.NewFuncVoid("caller")
	caller.CallConv = ir.CallConvGoInternal
	entry := caller.Entry()
	entry.CallVoid(caller.Sym("platform_callee", 0))
	entry.CallVoid(caller.Sym("go_callee", 0))
	entry.CallVoid(caller.Sym("elsewhere", 0))
	entry.CallVoidWithConvention(ir.CallConvGoInternal, caller.Sym("platform_callee", 0))
	target := entry.Load(ir.ClsL, caller.Sym("funcptr", 0))
	entry.CallVoid(target)
	entry.RetVoid()

	conventions := newCalleeConventions(module.Funcs)
	assert.Equal(t, ir.CallConvPlatform, conventions[platformCallee.Name])
	assert.Equal(t, ir.CallConvGoInternal, conventions[goCallee.Name])

	want := []ir.CallConvention{
		ir.CallConvPlatform,   // defined here, platform ABI
		ir.CallConvGoInternal, // defined here, ABIInternal
		ir.CallConvPlatform,   // not defined here
		ir.CallConvGoInternal, // explicitly marked, overriding the callee lookup
		ir.CallConvPlatform,   // indirect and unmarked
	}
	index := 0
	for i := range entry.Instrs {
		instruction := &entry.Instrs[i]
		if instruction.Op != ir.OCall {
			continue
		}
		require.Less(t, index, len(want))
		assert.Equal(t, want[index], conventions.forCall(caller, instruction), "call %d", index)
		index++
	}
	require.Equal(t, len(want), index)

	// A nil index defines nothing, so every unmarked call is platform ABI; an
	// explicitly marked one is still honoured.
	var empty calleeConventions
	assert.Equal(t, ir.CallConvPlatform, empty.forCall(caller, &entry.Instrs[1]))
	assert.Equal(t, ir.CallConvGoInternal, empty.forCall(caller, &entry.Instrs[3]))
}
