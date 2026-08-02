package goc_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The escape walk's rule under test in this file: copying a value into freshly
// allocated storage that may be in the heap publishes everything reachable from
// that value, so the value's own storage cannot stay in the frame.
//
// TestFrameEscapeAudit is the corpus-wide statement of the same property. These
// are the reduced cases, kept because the audit's cost is two and a half
// minutes and because one of them -- the interface-typed result -- is a shape
// the audit structurally cannot see.

// frameEscapesInFunction returns the publications opt.FrameEscapes reports for
// one function, rendered.
func frameEscapesInFunction(module *ir.Module, suffix string) []string {
	var reported []string
	for _, escape := range opt.FrameEscapes(module) {
		if strings.HasSuffix(escape.Func, suffix) {
			reported = append(reported, escape.String())
		}
	}
	return reported
}

// TestValueBoxedIntoAnInterfaceEscapes covers the runtime.KeepAlive(values)
// shape. observe does not let its parameter escape -- its only use is a
// println the walk treats as benign -- and that is beside the point: the
// caller copies the slice header, backing-array pointer and all, into a
// runtime.newobject payload before observe is entered.
//
// notBoxed is the control. The same slice, without the conversion, must stay
// in the frame; a fix that simply made every slice escape would fail here.
func TestValueBoxedIntoAnInterfaceEscapes(t *testing.T) {
	module, err := goc.CompileExecutable("interface_box.go", []byte(`
package main

import "runtime"

var sink int

func observe(value any) {
	if sink < 0 {
		println(value)
	}
}

func boxed() {
	values := make([]int, 0, 4)
	values = append(values, 1)
	observe(values)
	sink += len(values)
}

func notBoxed() {
	values := make([]int, 0, 4)
	values = append(values, 1)
	sink += len(values)
}

func main() {
	runtime.GC()
	boxed()
	notBoxed()
}
`))
	require.NoError(t, err)

	assert.Empty(t, frameEscapesInFunction(module, "main.boxed"),
		"the boxed slice's backing array is still in the frame, and its address reaches the heap payload")

	notBoxed := functionWithSuffix(t, module, "main.notBoxed")
	assert.False(t, callsSymbol(notBoxed, "runtime.newobject"),
		"a slice that is never converted to an interface must keep its frame backing array")
	assert.True(t, containsInstruction(notBoxed, func(instruction ir.Instr) bool {
		return instruction.Op.IsAlloc()
	}), "the control function allocates nothing in its frame, so it is not testing anything")
}

// TestCompositeLiteralAddressCarriesItsElementsToTheHeap covers
// bigmod.Nat.Mul's return x.Mod(&Nat{limbs: T}, m). &box{...} makes storage of
// its own; nonEscapingAddress puts that storage in the heap when the address is
// a call argument, and the element goes wherever the storage goes.
//
// selected is the control: there the address is consumed by a field selection,
// nonEscapingAddress keeps the literal in the frame, and the element must stay
// with it.
func TestCompositeLiteralAddressCarriesItsElementsToTheHeap(t *testing.T) {
	module, err := goc.CompileExecutable("literal_address.go", []byte(`
package main

import "runtime"

type box struct {
	limbs []int
}

var sink int

func read(value *box, extra int) int {
	return len(value.limbs) + extra
}

func passedToACall() {
	limbs := make([]int, 0, 8)
	limbs = append(limbs, 3)
	sink += read(&box{limbs: limbs}, 1)
}

func selected() {
	limbs := make([]int, 0, 8)
	limbs = append(limbs, 3)
	sink += len((&box{limbs: limbs}).limbs)
}

func main() {
	runtime.GC()
	passedToACall()
	selected()
}
`))
	require.NoError(t, err)

	assert.Empty(t, frameEscapesInFunction(module, "main.passedToACall"),
		"the literal went to the heap and its element stayed in the frame, which is the pair that must not happen")

	selected := functionWithSuffix(t, module, "main.selected")
	assert.False(t, callsSymbol(selected, "runtime.newobject"),
		"a literal address nonEscapingAddress keeps in the frame must not drag its element to the heap")
}

// TestParameterLeakingToAnInterfaceResultEscapes covers the summary-walk hole:
// toAny's parameter reaches only its own result, which is what
// parameterLeaksOnlyToResult is for, but the result is an interface, so the
// value is in a heap payload the moment toAny returns.
//
// This one is asserted on where the allocation landed rather than on
// opt.FrameEscapes, because the publishing store is inside toAny, where the
// address arrives as a parameter rather than as one of toAny's own frame
// allocations. FrameEscapes is a per-function may-analysis and structurally
// cannot report it -- which is why the corpus audit passing is not evidence
// about this shape.
func TestParameterLeakingToAnInterfaceResultEscapes(t *testing.T) {
	module, err := goc.CompileExecutable("interface_result.go", []byte(`
package main

import "runtime"

var sink int

func toAny(value []int) any {
	return value
}

func passThrough(value []int) []int {
	return value
}

func leaksToAnInterfaceResult() {
	values := make([]int, 0, 8)
	values = append(values, 5)
	held := toAny(values)
	if held != nil {
		sink++
	}
	sink += len(values)
}

func leaksToASliceResult() {
	values := make([]int, 0, 8)
	values = append(values, 5)
	held := passThrough(values)
	sink += len(held)
}

func main() {
	runtime.GC()
	leaksToAnInterfaceResult()
	leaksToASliceResult()
}
`))
	require.NoError(t, err)

	boxed := functionWithSuffix(t, module, "main.leaksToAnInterfaceResult")
	assert.True(t, callsSymbol(boxed, "runtime.newobject"),
		"a value that leaks only to an interface-typed result is in the heap when the callee returns")

	plain := functionWithSuffix(t, module, "main.leaksToASliceResult")
	assert.False(t, callsSymbol(plain, "runtime.newobject"),
		"leaking only to a slice-typed result copies no storage, so the backing array stays in the frame")
}
