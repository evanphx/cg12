package goc_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// pointerWidth is the size of the load every reader of an abi.Type's GCData
// performs. It is not the size of the mask.
const pointerWidth = 8

// The runtime never reads an abi.Type's GC pointer mask a byte at a time.
// runtime.readUintptr loads a whole uintptr, and every reader goes through it:
// typePointersOfType takes the first word as its mask, typePointers.next and
// fastForward take later words at eight-byte offsets, and heapSetType reads it
// to write a new object's heap bitmap.
//
// A mask emitted at its exact significant length is therefore read together with
// whatever symbol the linker placed after it, and every 1 bit in those bytes is
// a phantom pointer word at an offset outside the object. A one-byte mask for a
// one-pointer type read as 0x0800000000000001 in a real image -- bit 59, word
// offset 472 -- and growslice's bulkBarrierPreWriteSrcOnly duly buffered word 59
// of an eight-byte source array as a pointer. See RUNTIME_PLAN.md section 26.
//
// The host toolchain rounds the same way for the same reason;
// cmd/compile/internal/reflectdata/reflect.go's dgcptrmask says "Runtime wants
// ptrmasks padded to a multiple of uintptr in size".
//
// The types below are chosen to span the interesting mask lengths: one pointer
// word, a mixture, none at all, and an array long enough to need more than one
// mask byte.
func TestTypeGCMasksArePaddedToAPointerWord(t *testing.T) {
	module, err := goc.CompileExecutable("masks.go", []byte(`
package main

type box struct{ value int }

type vertex struct {
	value int
	label string
	edges []*vertex
}

type wide struct {
	slots [24]*box
}

type scalars struct {
	a int
	b float64
}

var sink any

func main() {
	sink = new(box)
	sink = new(vertex)
	sink = new(wide)
	sink = new(scalars)
	sink = make([]*vertex, 2)
	sink = new([1]*box)
	sink = make(map[string]*box)
}
`))
	require.NoError(t, err)

	masks := 0
	maskNames := ""
	for _, datum := range module.Data {
		if !strings.HasSuffix(datum.Name, ".gcdata") {
			continue
		}
		masks++
		maskNames += datum.Name + " "
		length := gcMaskByteLength(t, datum)
		require.NotZerof(t, length, "the mask %q is empty, so readUintptr reads only foreign bytes", datum.Name)
		require.Zerof(t, length%pointerWidth,
			"the mask %q is %d bytes, so readUintptr reads %d bytes of the next symbol as pointer bits",
			datum.Name, length, pointerWidth-length%pointerWidth)
		require.Equalf(t, pointerWidth, datum.Align,
			"the mask %q is not pointer-aligned, so readUintptr loads it unaligned", datum.Name)
	}
	require.GreaterOrEqual(t, masks, 4, "the program should emit a mask for each of its named types")
	require.Contains(t, maskNames, "vertex", "the program's own pointer-bearing struct should have a mask")
}

// gcMaskByteLength returns how many bytes a GC mask datum occupies. Every mask
// is emitted as a single run of unsigned bytes, so its length is the length of
// that run.
func gcMaskByteLength(t *testing.T, datum *ir.Data) int {
	t.Helper()
	length := 0
	for _, item := range datum.Items {
		require.Equalf(t, ir.SubUB, item.Sub, "the mask %q holds an item that is not a byte", datum.Name)
		require.Emptyf(t, item.Sym, "the mask %q holds a symbol reference", datum.Name)
		length += len(item.Ints)
		length += int(item.Zero)
	}
	return length
}
