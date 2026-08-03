package goc_test

import (
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Which runtime helper each interface conversion is compiled into.
//
// TestAllocationCounts measures what the conversions cost, which is the number
// anybody actually cares about, but it cannot say *why* a row is zero: a payload
// that stayed in the frame and a payload built by runtime.convT64 both cost
// nothing, and only one of them is this fast path working. A silent fall back to
// runtime.newobject on the escaping path -- which is what happens if the store
// stops sitting immediately after the candidate, the shape ir.Block's
// HeapAllocConverted requires -- would show up here as a helper that stopped
// being called, before it showed up anywhere as a number.
func TestInterfaceConversionsCallTheRuntimeHelpers(t *testing.T) {
	module, err := goc.Compile("iface_conversions.go", []byte(`
package main

import "runtime"

var sink any

//go:noinline
func keep(value any) { sink = value }

func escapingInt(value int) any { return value }

func escapingBool(value bool) any { return value }

func escapingInt32(value int32) any { return value }

func escapingInt16(value int16) any { return value }

func escapingFloat64(value float64) any { return value }

func escapingString(value string) any { return value }

func escapingPointer(value *int) any { return value }

func localInt(value int) bool {
	var boxed any = value
	other, ok := boxed.(int)
	return ok && other == value
}

//go:noinline
func countAny(values ...any) int { return len(values) }

func framedVariadic(value int) int { return countAny(value, value) }

func main() {
	runtime.GC()
	keep(escapingInt(7))
	keep(escapingBool(true))
	keep(escapingInt32(7))
	keep(escapingInt16(7))
	keep(escapingFloat64(7))
	keep(escapingString("seven"))
	keep(escapingPointer(nil))
	keep(localInt(7))
	keep(framedVariadic(7))
}
`))
	require.NoError(t, err)

	for _, expected := range []struct {
		function string
		helper   string
		why      string
	}{
		{"main.escapingInt", "runtime.convT64", "an int is eight bytes and pointer-free"},
		{"main.escapingBool", "runtime.convT64", "a bool goes through convT64 zero-extended: the runtime has no convT8"},
		{"main.escapingInt32", "runtime.convT32", "four bytes, four-byte aligned"},
		{"main.escapingInt16", "runtime.convT16", "two bytes, two-byte aligned"},
		{"main.escapingFloat64", "runtime.convT64", "eight bytes of float bits, handed over as an integer"},
	} {
		function := functionWithSuffix(t, module, expected.function)
		assert.True(t, callsSymbol(function, expected.helper),
			"%s should build its payload with %s (%s)", expected.function, expected.helper, expected.why)
		assert.False(t, callsSymbol(function, "runtime.newobject"),
			"%s allocated a payload a conversion helper was going to build", expected.function)
	}

	// A string payload is two words, so it is not in a register to be handed to
	// a helper, and goc does not wire gc's convTstring: it allocates for every
	// value but the empty one, which is no better than the allocator already
	// being called here.
	escapingString := functionWithSuffix(t, module, "main.escapingString")
	assert.True(t, callsSymbol(escapingString, "runtime.newobject"),
		"a string payload has no register-shaped helper, so it is still allocated")

	// A pointer is its own payload. It must not start calling a helper: there is
	// no storage to build, and handing a pointer to convT64 would box the
	// address rather than use it.
	escapingPointer := functionWithSuffix(t, module, "main.escapingPointer")
	for _, helper := range []string{"runtime.convT16", "runtime.convT32", "runtime.convT64", "runtime.newobject"} {
		assert.False(t, callsSymbol(escapingPointer, helper),
			"a pointer-shaped value goes in the interface word itself, with no box at all")
	}

	// A `...any` call packs the backing array and the boxed payloads into one
	// object, and that object is promoted to the frame. Neither an allocator nor
	// a helper is called: the frame is cheaper than both, and a payload the
	// caller was handed storage for is written into that storage rather than
	// built again somewhere else. This is the path adaptValueToInterface takes
	// when it is given a payload, and it is not the fast path's to take.
	framedVariadic := functionWithSuffix(t, module, "main.framedVariadic")
	for _, helper := range []string{"runtime.convT16", "runtime.convT32", "runtime.convT64", "runtime.newobject"} {
		assert.False(t, callsSymbol(framedVariadic, helper),
			"a `...any` object that stays in the frame should not be built by a call to %s", helper)
	}

	// goc's escape analysis does not frame this one, though nothing outside
	// localInt can observe the payload: the box is passed to the type assertion
	// through storage the analysis gives up on. That is a pessimism this change
	// does not fix -- it makes it free. The row is here so that if the analysis
	// ever does frame it, the reader of the diff is told which of the two
	// happened.
	localInt := functionWithSuffix(t, module, "main.localInt")
	assert.True(t, callsSymbol(localInt, "runtime.convT64"),
		"a payload goc's analysis will not frame is at least built without allocating")
	assert.False(t, callsSymbol(localInt, "runtime.newobject"),
		"the payload goc will not frame is no longer an allocation either")
}
