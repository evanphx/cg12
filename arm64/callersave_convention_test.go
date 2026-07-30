package arm64

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a call destroys is decided by the callee, and nothing else. These tests
// cover the one direction that used to be silently wrong: an unmanaged AAPCS64
// body calling a Go ABIInternal callee.
//
// The failure had no symptom to look for. Colouring prefers X19-X28 for a value
// live across a call, because under AAPCS64 the callee gives them back; an
// ABIInternal callee gives nothing back. The caller-save pass asked the
// *enclosing* function which registers a call destroys, got AAPCS64's answer, and
// left the value sitting in X19 across a call that overwrote it. Every emitted
// instruction is individually valid, so the only way to catch it is to look at
// what the allocator and the caller-save pass agreed on.
//
// This host cannot execute AArch64 code (buildAndRun skips without a cross
// toolchain), so these tests pin the register assignment and the save/restore
// pair in the IR rather than a program's exit status.

// crossConventionCase is one lowered-and-allocated fixture: an unmanaged AAPCS64
// function that computes a value, calls a function of the given convention, and
// uses the value afterwards.
type crossConventionCase struct {
	caller     *ir.Func
	liveAcross ir.Ref
	call       *ir.Instr
	block      *ir.Block
	assembly   []string // the emitted machine code, disassembled, one instruction per line
}

func buildCrossConventionCase(t *testing.T, calleeConvention ir.CallConvention) crossConventionCase {
	t.Helper()

	module := ir.NewModule()
	callee := module.NewFuncVoid("callee")
	callee.CallConv = calleeConvention

	// An ordinary platform-ABI function: neither Go's convention nor a managed
	// frame, which is what goc emits for every non-closure function.
	caller := module.NewFunc("aapcs_caller", ir.ClsL)
	first := caller.Param("a", ir.ClsL)
	second := caller.Param("b", ir.ClsL)
	entry := caller.Entry()
	liveAcross := entry.Add(ir.ClsL, first, second)
	entry.CallVoid(caller.Sym("callee", 0))
	entry.Ret(entry.Add(ir.ClsL, liveAcross, caller.Long(1)))

	conventions := newCalleeConventions(module.Funcs)
	ir.LowerPointers(caller, ptrCls)
	require.NoError(t, lower(caller, conventions, TLSLocalExec))
	allocation, err := regAlloc(caller)
	require.NoError(t, err)
	machine, err := emitMachine(caller, allocation, conventions, nil, TLSLocalExec)
	require.NoError(t, err)

	got := crossConventionCase{caller: caller, liveAcross: liveAcross}
	for offset := 0; offset+4 <= len(machine.code); offset += 4 {
		got.assembly = append(got.assembly, a64.Disasm(binary.LittleEndian.Uint32(machine.code[offset:])))
	}
	for _, block := range caller.Blocks {
		for index := range block.Instrs {
			if block.Instrs[index].Op == ir.OCall {
				require.Nil(t, got.call, "the fixture must contain exactly one call")
				got.call = &block.Instrs[index]
				got.block = block
			}
		}
	}
	require.NotNil(t, got.call, "the fixture must contain exactly one call")
	require.Equal(t, calleeConvention, got.call.CallConv,
		"the call must have been lowered against the callee's convention")
	return got
}

// savedAroundCall reports whether the value is stored before the call and
// reloaded after it, which is the whole of what insertCallerSaves does.
func (c crossConventionCase) savedAroundCall() (spilledBefore, reloadedAfter bool) {
	seenCall := false
	for index := range c.block.Instrs {
		in := &c.block.Instrs[index]
		switch {
		case in == c.call:
			seenCall = true
		case in.Op == ir.OSpill && !seenCall && len(in.Args) == 1 && in.Args[0] == c.liveAcross:
			spilledBefore = true
		case in.Op == ir.OReload && seenCall && in.To == c.liveAcross:
			reloadedAfter = true
		}
	}
	return spilledBefore, reloadedAfter
}

// accessesOf counts the emitted instructions that name one register with the given
// mnemonic, e.g. how many times x19 is stored.
func (c crossConventionCase) accessesOf(mnemonic string, r Reg) int {
	prefix := mnemonic + " " + r.xName() + ","
	count := 0
	for _, line := range c.assembly {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

// The negative control. Before the clobber set was keyed on the callee this
// asserted nothing was saved, and the value rode through the ABIInternal call in
// X19.
func TestGoInternalCalleeClobbersTheCallersCalleeSavedRegisters(t *testing.T) {
	c := buildCrossConventionCase(t, ir.CallConvGoInternal)

	held := Reg(c.caller.Temps[c.liveAcross.ID].Reg)
	require.NotEqual(t, Reg(ir.NoReg), held,
		"the fixture only proves anything while the value is register-resident")
	require.True(t, calleeSavedReg(held),
		"colouring is expected to park a call-crossing value in an AAPCS64 callee-saved register (got %s)",
		held.xName())

	spilledBefore, reloadedAfter := c.savedAroundCall()
	assert.True(t, spilledBefore,
		"%s does not survive an ABIInternal call, so the value must be stored before it", held.xName())
	assert.True(t, reloadedAfter,
		"%s does not survive an ABIInternal call, so the value must be reloaded after it", held.xName())

	// And it reaches the machine code: two stores and two loads of x19, the
	// prologue/epilogue pair that preserves it for *our* caller, plus the pair that
	// carries it across the call. This host has no AArch64 toolchain, so the code is
	// assembled and read back, never run.
	assert.Equal(t, 2, c.accessesOf("str", held),
		"expected the prologue save and the pre-call save:\n%s", strings.Join(c.assembly, "\n"))
	assert.Equal(t, 2, c.accessesOf("ldr", held),
		"expected the post-call reload and the epilogue restore:\n%s", strings.Join(c.assembly, "\n"))
}

// The same fixture against an AAPCS64 callee must save nothing: the callee really
// does preserve X19, and wrapping it anyway would be a pointless store and load
// on the hot path. Without this the test above would also pass a fix that simply
// declared every register volatile.
func TestPlatformCalleePreservesTheCallersCalleeSavedRegisters(t *testing.T) {
	c := buildCrossConventionCase(t, ir.CallConvPlatform)

	held := Reg(c.caller.Temps[c.liveAcross.ID].Reg)
	require.True(t, calleeSavedReg(held),
		"expected a callee-saved register, got %s", held.xName())

	spilledBefore, reloadedAfter := c.savedAroundCall()
	assert.False(t, spilledBefore, "an AAPCS64 callee preserves %s; nothing to save", held.xName())
	assert.False(t, reloadedAfter, "an AAPCS64 callee preserves %s; nothing to reload", held.xName())

	assert.Equal(t, 1, c.accessesOf("str", held),
		"only the prologue save belongs here:\n%s", strings.Join(c.assembly, "\n"))
	assert.Equal(t, 1, c.accessesOf("ldr", held),
		"only the epilogue restore belongs here:\n%s", strings.Join(c.assembly, "\n"))
}

// callerClobbered's contract, stated directly. The rows that matter are the
// callee-saved ones: they flip with the callee's convention and with nothing else.
func TestCallerClobberedFollowsTheCallee(t *testing.T) {
	platformCall := &ir.Instr{Op: ir.OCall, CallConv: ir.CallConvPlatform, CallConvSet: true}
	goCall := &ir.Instr{Op: ir.OCall, CallConv: ir.CallConvGoInternal, CallConvSet: true}

	module := ir.NewModule()
	platformBody := module.NewFuncVoid("platform_body")
	goBody := module.NewFuncVoid("go_body")
	goBody.CallConv = ir.CallConvGoInternal
	managedBody := module.NewFuncVoid("managed_body")
	managedBody.ManagedFrame = true

	for _, body := range []*ir.Func{platformBody, goBody} {
		assert.False(t, callerClobbered(body, platformCall, ir.ClsL, X19),
			"%s: an AAPCS64 callee preserves x19", body.Name)
		assert.True(t, callerClobbered(body, goCall, ir.ClsL, X19),
			"%s: an ABIInternal callee preserves nothing", body.Name)
		assert.True(t, callerClobbered(body, platformCall, ir.ClsL, X9),
			"%s: x9 is caller-saved under either convention", body.Name)
		assert.True(t, callerClobbered(body, goCall, ir.ClsL, X9), body.Name)
		assert.False(t, callerClobbered(body, platformCall, ir.ClsD, vReg(8)),
			"%s: AAPCS64 preserves the low 64 bits of v8", body.Name)
		assert.True(t, callerClobbered(body, goCall, ir.ClsD, vReg(8)),
			"%s: ABIInternal preserves no part of v8", body.Name)
	}

	// A managed frame can enter the runtime's stack-growth path, which follows
	// Go's volatile-register contract whatever the callee promises.
	assert.True(t, callerClobbered(managedBody, platformCall, ir.ClsL, X19),
		"a managed frame keeps no register across a call")
}

// A call that has not been through lowering carries no convention, and resolves
// exactly as calleeConventions.forCall resolves it: the platform ABI.
func TestLoweredCallConventionDefaultsToPlatform(t *testing.T) {
	assert.Equal(t, ir.CallConvPlatform, loweredCallConvention(&ir.Instr{Op: ir.OCall}))
	assert.Equal(t, ir.CallConvPlatform,
		loweredCallConvention(&ir.Instr{Op: ir.OCall, CallConv: ir.CallConvGoInternal}),
		"an unstamped convention field is not a resolution")
	assert.Equal(t, ir.CallConvGoInternal,
		loweredCallConvention(&ir.Instr{Op: ir.OCall, CallConv: ir.CallConvGoInternal, CallConvSet: true}))
}
