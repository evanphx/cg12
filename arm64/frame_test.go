package arm64

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// calleeSavedRegisters is every register AAPCS64 obliges a callee to preserve,
// read out of the predicates the backend itself uses rather than restated here.
func calleeSavedRegisters() []Reg {
	var all []Reg
	for r := X0; r <= vLast; r++ {
		if calleeSavedReg(r) {
			all = append(all, r)
		}
	}
	return all
}

// The save order has to be total over the callee-saved set, because computeFrame
// builds the prologue's save list by *filtering* it. A register missing from the
// order is a register the prologue does not save while the epilogue's offsets
// still assume the shorter list -- the caller gets a corrupted register back and
// nothing reports it.
func TestCalleeSaveOrderIsTotal(t *testing.T) {
	module := ir.NewModule()

	platform := module.NewFuncVoid("platform")
	managed := module.NewFuncVoid("managed")
	managed.ManagedFrame = true
	goInternal := module.NewFuncVoid("go_internal")
	goInternal.CallConv = ir.CallConvGoInternal

	for _, f := range []*ir.Func{platform, managed, goInternal} {
		order := calleeSaveOrder(f)

		seen := map[Reg]bool{}
		for _, r := range order {
			require.False(t, seen[r], "%s: %s appears twice in the save order", f.Name, r.xName())
			seen[r] = true
			require.True(t, calleeSavedFor(f.UsesGoInternalCallConvention(), r),
				"%s: %s is not a register this convention preserves", f.Name, r.xName())
		}

		for _, r := range calleeSavedRegisters() {
			if !calleeSavedFor(f.UsesGoInternalCallConvention(), r) {
				continue
			}
			assert.True(t, seen[r], "%s: %s must be preserved but is not in the save order", f.Name, r.xName())
		}

		assert.Equal(t, order, calleeSaveOrder(f), "%s: the save order must not vary between calls", f.Name)
	}

	// Go ABIInternal preserves nothing at all, so there is nothing to order.
	assert.Empty(t, calleeSaveOrder(goInternal))
}

// The registers the sweep exists for. None of them is in any allocation order --
// X27 is a reserved scratch register, and X26/X28 are the Go runtime's closure
// context and g register, which only a platform-ABI function may allocate -- yet
// AAPCS64 obliges the callee to preserve all three, and an inline-asm clobber list
// can name any of them.
func TestCalleeSaveOrderCoversRegistersOutsideTheAllocationOrders(t *testing.T) {
	module := ir.NewModule()
	platform := module.NewFuncVoid("platform")
	managed := module.NewFuncVoid("managed")
	managed.ManagedFrame = true

	inAllocationOrders := func(f *ir.Func, want Reg) bool {
		for _, r := range intAllocOrderFor(f) {
			if r == want {
				return true
			}
		}
		for _, r := range floatAllocOrder {
			if r == want {
				return true
			}
		}
		return false
	}

	require.False(t, inAllocationOrders(platform, X27), "X27 is reserved as a scratch register")
	require.False(t, inAllocationOrders(managed, X26), "a managed frame reserves X26 for the Go runtime")
	require.False(t, inAllocationOrders(managed, X28), "a managed frame reserves X28 for the Go runtime")

	assert.Contains(t, calleeSaveOrder(platform), X27)
	assert.Contains(t, calleeSaveOrder(managed), X26)
	assert.Contains(t, calleeSaveOrder(managed), X27)
	assert.Contains(t, calleeSaveOrder(managed), X28)
}

// The allocation orders still lead, so a register the allocator hands out keeps
// the save position a reader of intAllocOrderFor expects, and adjacent integer
// saves still pair into an stp.
func TestCalleeSaveOrderLeadsWithTheAllocationOrders(t *testing.T) {
	module := ir.NewModule()
	platform := module.NewFuncVoid("platform")

	order := calleeSaveOrder(platform)
	require.Greater(t, len(order), 8)
	assert.Equal(t, []Reg{X19, X20, X21, X22, X23, X24, X25, X26, X28}, order[:9],
		"the integer allocation order comes first, X26/X28 last within it")
	assert.Equal(t, X27, order[len(order)-1],
		"the swept leftover lands at the end, where it cannot move an existing slot")
}

// The negative control for the sweep. An inline-asm template that declares it
// writes x27 makes this function responsible for x27 exactly as allocating it
// would: the ABI promise is to the caller, which does not care where the write
// came from. asmClobberRegs answers for the template, not for the allocator, so
// x27 reaches the used set while being absent from every allocation order; the
// old save list dropped it there and the prologue silently did not save it.
func TestInlineAsmClobberOutsideTheAllocationOrderIsSaved(t *testing.T) {
	module := ir.NewModule()
	f := module.NewFunc("asm_clobbers_scratch", ir.ClsL)
	entry := f.Entry()
	results := entry.Asm("mov %x0, #7", []ir.AsmSpec{{Kind: ir.AsmRegOut, Cls: ir.ClsL}})
	require.Len(t, results, 1)
	asm := &entry.Instrs[len(entry.Instrs)-1]
	require.Equal(t, ir.OAsm, asm.Op)
	asm.Asm.Clobbers = []string{"x27"}
	entry.Ret(results[0])

	require.Equal(t, []Reg{X27}, asmClobberRegs(asm), "the fixture must declare the clobber")

	conventions := newCalleeConventions(module.Funcs)
	ir.LowerPointers(f, ptrCls)
	require.NoError(t, lower(f, conventions, TLSLocalExec))
	allocation, err := regAlloc(f)
	require.NoError(t, err)

	layout := computeFrame(f, allocation, conventions)
	assert.Contains(t, layout.calleeSaved, X27,
		"a callee-saved register an inline asm declares it writes must be saved by the prologue")
}
