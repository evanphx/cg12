package arm64

import (
	"fmt"
	"testing"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lifetimeColoringFixture builds a function with three same-size allocations whose
// lifetime markers make exactly one pair overlap:
//
//	first  and second  are both live in @overlap, so they interfere.
//	third            is live only in @third, so it interferes with neither.
//
// Greedy coloring therefore produces two groups, and *which* group `third` joins
// depends entirely on the order the three were indexed in. All three are the same
// size, so the size-descending sort.SliceStable that drives the coloring leaves
// that order untouched. Indexing them by ranging a map made the answer -- and with
// it the emitted frame offsets -- a function of Go's map iteration seed.
func lifetimeColoringFixture(t *testing.T) (*ir.Func, []*ir.Instr) {
	t.Helper()

	module := ir.NewModule()
	f := module.NewFuncVoid("three_locals")
	entry := f.Entry()
	overlap := f.NewBlock("overlap")
	third := f.NewBlock("third")
	done := f.NewBlock("done")

	first := entry.Alloc(8, 8)
	second := entry.Alloc(8, 8)
	other := entry.Alloc(8, 8)
	entry.Goto(overlap)

	overlap.LifeStart(first)
	overlap.LifeStart(second)
	overlap.Store(f.Long(1), first)
	overlap.Store(f.Long(2), second)
	overlap.LifeEnd(second)
	overlap.LifeEnd(first)
	overlap.Goto(third)

	third.LifeStart(other)
	third.Store(f.Long(3), other)
	third.LifeEnd(other)
	third.Goto(done)

	done.RetVoid()

	allocs := map[uint32]*ir.Instr{}
	for k := range entry.Instrs {
		in := &entry.Instrs[k]
		if in.Op.IsAlloc() {
			allocs[in.To.ID] = in
		}
	}
	ordered := []*ir.Instr{allocs[first.ID], allocs[second.ID], allocs[other.ID]}
	for i, in := range ordered {
		require.NotNil(t, in, "allocation %d not found", i)
	}
	return f, ordered
}

// groupSignature renders one allocaGroups result as a comparable string, naming
// each allocation by its position in program order rather than by pointer.
func groupSignature(allocs []*ir.Instr, groups map[*ir.Instr]*ir.Instr) string {
	position := map[*ir.Instr]int{}
	for i, in := range allocs {
		position[in] = i
	}
	out := ""
	for i, in := range allocs {
		rep, shared := groups[in]
		if !shared {
			out += fmt.Sprintf("%d:private ", i)
			continue
		}
		out += fmt.Sprintf("%d:%d ", i, position[rep])
	}
	return out
}

// The negative control for the coloring's determinism. Ranging the alloca map made
// the tie-break Go's per-range map seed, so repeated runs over one unchanged
// function disagreed -- which is what this loop catches. Go randomizes small-map
// iteration order per range statement, so with three keys a handful of iterations
// is already conclusive; sixty-four leaves no room at all.
func TestAllocaColoringIsDeterministic(t *testing.T) {
	f, allocs := lifetimeColoringFixture(t)
	cfg := analysis.BuildCFG(f)

	first := groupSignature(allocs, allocaGroups(f, cfg))
	for run := 0; run < 64; run++ {
		got := groupSignature(allocs, allocaGroups(f, cfg))
		require.Equal(t, first, got, "run %d disagreed with the first run about slot sharing", run)
	}

	// And the order it settles on is program order: the earliest allocation is its
	// group's representative, so the third one shares with the first rather than
	// with the second. Stating the answer, not just its stability, is what keeps a
	// future reordering from being invisible.
	assert.Equal(t, "0:0 1:1 2:0 ", first)
}

// The frame offsets the coloring feeds are what actually reaches the object file,
// so pin those too: two runs of the compiler over identical input must place every
// allocation at the same offset and produce the same frame size.
func TestAllocaFrameOffsetsAreDeterministic(t *testing.T) {
	layoutOf := func() string {
		f, allocs := lifetimeColoringFixture(t)
		conventions := newCalleeConventions([]*ir.Func{f})
		ir.LowerPointers(f, ptrCls)
		require.NoError(t, lower(f, conventions, TLSLocalExec))
		allocation, err := regAlloc(f)
		require.NoError(t, err)
		// insertCallerSaves rebuilds the instruction slices, so re-find the
		// allocations by temp id before reading the layout keyed on them.
		byTemp := map[uint32]*ir.Instr{}
		for _, block := range f.Blocks {
			for k := range block.Instrs {
				in := &block.Instrs[k]
				if in.Op.IsAlloc() {
					byTemp[in.To.ID] = in
				}
			}
		}
		layout := computeFrame(f, allocation, conventions)
		out := fmt.Sprintf("frame=%d", layout.frame)
		for i, in := range allocs {
			out += fmt.Sprintf(" %d@%d", i, layout.allocOff[byTemp[in.To.ID]])
		}
		return out
	}

	want := layoutOf()
	for run := 0; run < 64; run++ {
		require.Equal(t, want, layoutOf(), "run %d laid the frame out differently", run)
	}
	// Two 8-byte slots, not three: the third allocation reuses the first one's.
	assert.Equal(t, "frame=32 0@16 1@24 2@16", want)
}
