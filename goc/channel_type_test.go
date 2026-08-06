package goc_test

import (
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// runtime.makechan reads the channel element descriptor to decide how to
// allocate the buffer:
//
//	case !elem.Pointers():
//	    c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
//	    c.buf = add(unsafe.Pointer(c), hchanSize)
//	default:
//	    c = new(hchan)
//	    c.buf = mallocgc(mem, elem, true)
//
// so an element descriptor whose PtrBytes is zero does not merely lose a write
// barrier: it puts the whole buffer inside a no-scan allocation, where the mark
// phase never sees the elements at all. chansend, chanrecv and sendDirect then
// hand the same descriptor to typedmemmove and bulkBarrierPreWriteSrcOnly, which
// are no-ops on a pointer-free type.
//
// goc emitted a hand-rolled 48-byte stub for that descriptor with only Size_,
// Align_, FieldAlign_ and Kind_ filled in. This test reads the emitted datum
// instead of the running program because the runtime consequence is two layers
// away from the mistake, and because a program that loses a buffered element
// only fails once the sweeper has reclaimed it.
func TestChannelElementDescriptorCarriesPointerMetadata(t *testing.T) {
	t.Parallel()
	module, err := goc.Compile("channels.go", []byte(`
package main

type box struct{ value int }

type pair struct {
	name string
	box  *box
}

func makeAll() (chan *box, chan string, chan pair, chan int) {
	return make(chan *box, 4), make(chan string, 4), make(chan pair, 4), make(chan int, 4)
}
`))
	require.NoError(t, err)

	data := make(map[string]*ir.Data, len(module.Data))
	for _, datum := range module.Data {
		data[datum.Name] = datum
	}

	var channelDescriptors []*ir.Data
	for _, datum := range module.Data {
		if isChannelDescriptor(datum) {
			channelDescriptors = append(channelDescriptors, datum)
		}
	}
	require.Len(t, channelDescriptors, 4, "one abi.ChanType per distinct channel element type")

	pointerful := 0
	for _, descriptor := range channelDescriptors {
		elementName := channelDescriptorElement(t, descriptor)
		element := data[elementName]
		require.NotNilf(t, element, "the element descriptor %q named by %q is not emitted", elementName, descriptor.Name)

		size, pointerBytes := abiTypeSizeAndPtrBytes(t, element)
		require.NotZerof(t, size, "the element descriptor %q has size 0", elementName)
		if pointerBytes == 0 {
			// chan int: pointer-free, and makechan's inline no-scan buffer is
			// the correct allocation for it.
			continue
		}
		pointerful++
		require.LessOrEqualf(t, pointerBytes, size,
			"the element descriptor %q claims more pointer bytes than it has bytes", elementName)
		require.NotEmptyf(t, abiTypeGCData(t, element),
			"the element descriptor %q has pointers but no GCData symbol, so heapSetType writes no bitmap", elementName)
	}
	require.Equal(t, 3, pointerful,
		"chan *box, chan string and chan pair all contain pointers; only chan int does not")
}

// Every channel element descriptor must be the same datum every other allocation
// site uses for that type, so that one type has one description of its pointers.
// A separate stub emitted only for channels is how the metadata drifted in the
// first place.
func TestChannelElementDescriptorIsTheSharedRuntimeTypeDescriptor(t *testing.T) {
	t.Parallel()
	module, err := goc.Compile("shared.go", []byte(`
package main

type box struct{ value int }

func viaChannel() chan *box { return make(chan *box, 2) }

func viaSlice() []*box { return make([]*box, 2) }
`))
	require.NoError(t, err)

	data := make(map[string]*ir.Data, len(module.Data))
	for _, datum := range module.Data {
		data[datum.Name] = datum
	}

	var elementNames []string
	for _, datum := range module.Data {
		if isChannelDescriptor(datum) {
			elementNames = append(elementNames, channelDescriptorElement(t, datum))
		}
	}
	require.Len(t, elementNames, 1)

	element := data[elementNames[0]]
	require.NotNil(t, element)

	// make([]*box, 2) passes the same *box descriptor to runtime.makeslice, so
	// exactly one datum in the module can describe *box. Two would mean the
	// channel path is emitting its own again.
	matching := 0
	_, elementPtrBytes := abiTypeSizeAndPtrBytes(t, element)
	for _, datum := range module.Data {
		if datum == element {
			matching++
			continue
		}
		if len(datum.Items) != len(element.Items) {
			continue
		}
		size, pointerBytes := abiTypeSizeAndPtrBytesIfPossible(datum)
		if size == 8 && pointerBytes == elementPtrBytes && abiTypeGCData(t, datum) != "" {
			matching++
		}
	}
	require.Equal(t, 1, matching,
		"*box is described by more than one abi.Type datum, so a channel and a slice can disagree about its pointers")
}

// isChannelDescriptor recognises the abi.ChanType layout goc emits: 48 zero
// bytes for the embedded abi.Type, the element pointer, and the direction.
func isChannelDescriptor(datum *ir.Data) bool {
	if len(datum.Items) != 3 {
		return false
	}
	if datum.Items[0].Zero != 48 {
		return false
	}
	if datum.Items[1].Sym == "" {
		return false
	}
	return len(datum.Items[2].Ints) == 1
}

func channelDescriptorElement(t *testing.T, datum *ir.Data) string {
	t.Helper()
	require.True(t, isChannelDescriptor(datum), "%q is not a channel descriptor", datum.Name)
	return datum.Items[1].Sym
}

// abiTypeSizeAndPtrBytes reads Size_ and PtrBytes out of an emitted abi.Type.
// goc writes them as the first item, two 8-byte words.
func abiTypeSizeAndPtrBytes(t *testing.T, datum *ir.Data) (int64, int64) {
	t.Helper()
	require.NotEmptyf(t, datum.Items, "%q has no items", datum.Name)
	require.Lenf(t, datum.Items[0].Ints, 2,
		"%q does not begin with Size_ and PtrBytes; it is not a complete abi.Type", datum.Name)
	return datum.Items[0].Ints[0], datum.Items[0].Ints[1]
}

func abiTypeSizeAndPtrBytesIfPossible(datum *ir.Data) (int64, int64) {
	if len(datum.Items) == 0 || len(datum.Items[0].Ints) != 2 {
		return 0, 0
	}
	return datum.Items[0].Ints[0], datum.Items[0].Ints[1]
}

// abiTypeGCData returns the name of the pointer-bitmap symbol an abi.Type points
// at, or "" when it has none. heapSetType reads it to stamp the bitmap for every
// allocation of the type, including a channel buffer's whole element array.
func abiTypeGCData(t *testing.T, datum *ir.Data) string {
	t.Helper()
	for _, item := range datum.Items {
		if item.Sym != "" {
			return item.Sym
		}
	}
	return ""
}
