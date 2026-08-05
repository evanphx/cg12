package gometa

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookupFindFuncTab is runtime.findfunc's index computation, transcribed from
// stdlib/src/runtime/symtab.go, so the tests below check the table against the
// code that reads it rather than against a restatement of how it is built.
//
// It returns the index the runtime would settle on: the bucket's index plus its
// subbucket delta, advanced by the forward scan.
func lookupFindFuncTab(table []byte, starts []uint64, minPC, maxPC, pc uint64) int {
	x := pc - minPC
	bucket := int(x / funcTabBucketSize)
	sub := int(x % funcTabBucketSize / findFuncSubbucketSize)
	record := table[bucket*findFuncBucketBytes:]
	index := int(binary.LittleEndian.Uint32(record)) + int(record[4+sub])
	entryoff := func(i int) uint64 {
		if i >= len(starts) {
			return maxPC
		}
		return starts[i]
	}
	for entryoff(index+1) <= pc {
		index++
	}
	return index
}

// containing is the answer the lookup must produce: the last function starting
// at or before pc.
func containing(starts []uint64, pc uint64) int {
	answer := 0
	for i, start := range starts {
		if start <= pc {
			answer = i
		}
	}
	return answer
}

// textObject is an object defining one text symbol per name at the given offset.
func textObject(names []string, offsets []uint64) *obj.Object {
	object := &obj.Object{}
	for i, name := range names {
		object.Syms = append(object.Syms, obj.Sym{
			Name:    name,
			Section: obj.SecText,
			Value:   offsets[i],
			Func:    true,
		})
	}
	return object
}

func functionsNamed(names []string) []FunctionInfo {
	functions := make([]FunctionInfo, len(names))
	for i, name := range names {
		functions[i] = FunctionInfo{Name: name}
	}
	return functions
}

// checkEveryPC is the property the whole table exists to have: for every PC in
// the module's text, the runtime's lookup lands on the function that contains
// it. It walks every subbucket boundary and every function entry, which is where
// an off-by-one in either direction shows up.
func checkEveryPC(t *testing.T, table []byte, starts []uint64, minPC, maxPC uint64) {
	t.Helper()
	probes := map[uint64]struct{}{}
	for pc := minPC; pc < maxPC; pc += findFuncSubbucketSize {
		probes[pc] = struct{}{}
	}
	for _, start := range starts {
		probes[start] = struct{}{}
		if start > minPC {
			probes[start-1] = struct{}{}
		}
		probes[start+1] = struct{}{}
	}
	probes[maxPC-1] = struct{}{}
	for pc := range probes {
		if pc < minPC || pc >= maxPC {
			continue
		}
		want := containing(starts, pc)
		got := lookupFindFuncTab(table, starts, minPC, maxPC, pc)
		require.Equalf(t, want, got, "pc %#x (offset %d)", pc, pc-minPC)
	}
}

// The table's whole purpose: a lookup near the end of the text must not walk the
// functab to get there. The bucket has to put the scan within one bucket's worth
// of functions of the answer.
func TestGoFindFuncTabStartsTheScanNextToTheAnswer(t *testing.T) {
	const count = 4000
	names := make([]string, count)
	starts := make([]uint64, count)
	for i := range names {
		names[i] = fmt.Sprintf("f%04d", i)
		starts[i] = 0x1000 + uint64(i)*48
	}
	end := starts[count-1] + 48
	names = append(names, "textend")
	starts = append(starts, end)

	object := textObject(names, starts)
	functions := functionsNamed(names[:count])
	buckets := findFuncBucketCount(object, functions, "textend")
	table := FindFuncTab(object, functions, "textend", buckets)

	// The last function: with a zero table this scan is `count` steps long.
	pc := starts[count-1]
	x := pc - starts[0]
	record := table[int(x/funcTabBucketSize)*findFuncBucketBytes:]
	sub := int(x % funcTabBucketSize / findFuncSubbucketSize)
	start := int(binary.LittleEndian.Uint32(record)) + int(record[4+sub])
	assert.Equal(t, count-1, lookupFindFuncTab(table, starts[:count], starts[0], end, pc))
	assert.LessOrEqual(t, count-1-start, funcTabBucketSize/findFuncSubbucketSize,
		"the scan should start within a bucket of the answer, not %d functions away", count-1-start)

	checkEveryPC(t, table, starts[:count], starts[0], end)
}

// Functions of uneven size, with gaps between them, are the ordinary case: a
// subbucket that no function starts in belongs to whichever function was still
// running when it began.
func TestGoFindFuncTabHandlesGapsAndUnevenFunctions(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "textend"}
	starts := []uint64{0x800, 0x820, 0x2000, 0x2004, 0x9000, 0xa000}

	object := textObject(names, starts)
	functions := functionsNamed(names[:5])
	buckets := findFuncBucketCount(object, functions, "textend")
	table := FindFuncTab(object, functions, "textend", buckets)

	checkEveryPC(t, table, starts[:5], starts[0], starts[5])
}

// More than 256 functions in one 4096-byte bucket overflows the byte-sized
// subbucket delta. Clamping it to 255 stays below the true index, so the lookup
// is still right and pays a few scan steps.
func TestGoFindFuncTabClampsAnOverfullBucket(t *testing.T) {
	const count = 1024
	names := make([]string, count)
	starts := make([]uint64, count)
	for i := range names {
		names[i] = fmt.Sprintf("f%04d", i)
		starts[i] = 0x1000 + uint64(i)*4
	}
	end := starts[count-1] + 4
	names = append(names, "textend")
	starts = append(starts, end)

	object := textObject(names, starts)
	functions := functionsNamed(names[:count])
	buckets := findFuncBucketCount(object, functions, "textend")
	table := FindFuncTab(object, functions, "textend", buckets)

	checkEveryPC(t, table, starts[:count], starts[0], end)
}

// The arm64 shape: the module's last functions live in the translated Plan 9
// sidecar, which is a different object, so this one cannot see where they are.
// Their buckets take the last known function's index -- a lower bound for every
// PC above it -- and the lookup's forward scan does the rest.
func TestGoFindFuncTabCoversFunctionsTheObjectDoesNotDefine(t *testing.T) {
	known := []string{"a", "b", "c"}
	knownStarts := []uint64{0x1000, 0x1400, 0x1800}
	object := textObject(known, knownStarts)

	functions := functionsNamed(append(append([]string{}, known...), "asm1", "asm2"))
	buckets := 4
	table := FindFuncTab(object, functions, "runtime_gocTextEnd", buckets)

	// At run time the sidecar lands above this object's text.
	starts := append(append([]uint64{}, knownStarts...), 0x2000, 0x2100)
	checkEveryPC(t, table, starts, starts[0], 0x2200)
}

// Without an offset for the first function there is no minpc to index from, so
// the table stays zero and every lookup scans from index 0 -- which is what cg12
// did for every image before this.
func TestGoFindFuncTabStaysZeroWithoutAKnownFirstFunction(t *testing.T) {
	object := textObject([]string{"b"}, []uint64{0x1000})
	table := FindFuncTab(object, functionsNamed([]string{"a", "b"}), "textend", 4)

	assert.Equal(t, make([]byte, 4*findFuncBucketBytes), table)
}

// If the object's offsets disagree with the order functab is written in, the
// table cannot be trusted to bound anything, so none of it is emitted.
func TestGoFindFuncTabStaysZeroWhenOffsetsAreOutOfOrder(t *testing.T) {
	object := textObject([]string{"a", "b"}, []uint64{0x1000, 0x800})
	table := FindFuncTab(object, functionsNamed([]string{"a", "b"}), "textend", 4)

	assert.Equal(t, make([]byte, 4*findFuncBucketBytes), table)
}

// One bucket record is a uint32 index and sixteen byte deltas, which is the 20
// bytes findFuncBucketCount has always sized the table in.
func TestGoFindFuncBucketRecordIsTwentyBytes(t *testing.T) {
	assert.Equal(t, 20, findFuncBucketBytes)
	assert.Equal(t, 16, findFuncSubbuckets)
	assert.Equal(t, 256, findFuncSubbucketSize)
}
