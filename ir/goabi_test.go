package ir

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// words returns the recorded pointer-word offsets of one allocation, sorted, so
// a test can assert on them without depending on map order.
func words(f *Func, allocation Ref) []int {
	offsets := make([]int, 0, len(f.StackPointerWords[allocation.ID]))
	for offset := range f.StackPointerWords[allocation.ID] {
		offsets = append(offsets, offset)
	}
	sort.Ints(offsets)
	return offsets
}

func TestInferStackPointerWordsRecordsManagedStoresIntoAnAllocation(t *testing.T) {
	f := NewModule().NewFuncVoid("store")
	pointer := f.MarkGCRef(f.Param("p", ClsL))
	entry := f.Entry()
	slot := entry.Alloc(8, 64)
	entry.Store(pointer, slot)
	entry.Store(pointer, entry.Add(ClsL, slot, f.Long(24)))
	entry.RetVoid()

	InferStackPointerWords(f)

	assert.Equal(t, []int{0, 24}, words(f, slot))
}

func TestInferStackPointerWordsFollowsChainedConstantOffsets(t *testing.T) {
	f := NewModule().NewFuncVoid("chain")
	pointer := f.MarkGCRef(f.Param("p", ClsL))
	entry := f.Entry()
	slot := entry.Alloc(8, 64)
	near := entry.Add(ClsL, slot, f.Long(16))
	far := entry.Add(ClsL, near, f.Long(24))
	entry.Store(pointer, far)
	entry.RetVoid()

	InferStackPointerWords(f)

	assert.Equal(t, []int{40}, words(f, slot))
}

func TestInferStackPointerWordsIgnoresOffsetsThatAreNotWholeWords(t *testing.T) {
	f := NewModule().NewFuncVoid("unaligned")
	pointer := f.MarkGCRef(f.Param("p", ClsL))
	entry := f.Entry()
	slot := entry.Alloc(8, 64)
	entry.Store(pointer, entry.Add(ClsL, slot, f.Long(4)))
	entry.RetVoid()

	InferStackPointerWords(f)

	// A pointer cannot straddle words, so an offset the collector could not name
	// as a root is not recorded at all.
	assert.Empty(t, words(f, slot))
}

func TestInferStackPointerWordsIgnoresRuntimeComputedAddresses(t *testing.T) {
	f := NewModule().NewFuncVoid("dynamic")
	pointer := f.MarkGCRef(f.Param("p", ClsL))
	index := f.Param("i", ClsL)
	entry := f.Entry()
	slot := entry.Alloc(8, 64)
	entry.Store(pointer, entry.Add(ClsL, slot, index))
	// A load also breaks the chain: the address stored through is whatever was in
	// memory, not a known offset into this frame.
	entry.Store(pointer, entry.Load(ClsL, slot))
	entry.RetVoid()

	InferStackPointerWords(f)

	assert.Empty(t, words(f, slot))
}

func TestInferStackPointerWordsTreatsAnAllocationResultAsManaged(t *testing.T) {
	f := NewModule().NewFuncVoid("interior")
	entry := f.Entry()
	outer := entry.Alloc(8, 64)
	inner := entry.Alloc(8, 16)
	// The stored value is an interior pointer into a frame the collector may move,
	// so it is a root even though nothing marked it GCRef.
	entry.Store(inner, entry.Add(ClsL, outer, f.Long(8)))
	entry.RetVoid()

	InferStackPointerWords(f)

	assert.Equal(t, []int{8}, words(f, outer))
	assert.False(t, f.Temp(inner).GCRef, "the pass must not invent GCRef flags")
}

func TestInferStackPointerWordsIgnoresUnmanagedValues(t *testing.T) {
	f := NewModule().NewFuncVoid("plain")
	plain := f.Param("n", ClsL)
	entry := f.Entry()
	slot := entry.Alloc(8, 64)
	entry.Store(plain, slot)
	entry.RetVoid()

	InferStackPointerWords(f)

	assert.Empty(t, words(f, slot))
}

// A raw pointer-class value is not a managed reference: goc gives ClsP to
// unsafe.Pointer-shaped values the collector must not follow. This moved here
// from arm64's unit tests along with the pass.
func TestInferStackPointerWordsDoesNotTreatRawPointerClassAsGCRoot(t *testing.T) {
	f := NewModule().NewFunc("raw_pointer", ClsP)
	f.CallConv = CallConvGoInternal
	f.ManagedFrame = true
	raw := f.Param("raw", ClsP)
	entry := f.Entry()
	slot := entry.Alloc(8, 16)
	entry.Store(raw, slot)
	entry.CallVoid(f.Sym("observe", 0))
	entry.Ret(raw)

	InferStackPointerWords(f)

	assert.False(t, f.Temp(raw).GCRef)
	assert.Empty(t, words(f, slot))
}

func TestInferStackPointerWordsAddsToAnExistingMap(t *testing.T) {
	f := NewModule().NewFuncVoid("merge")
	pointer := f.MarkGCRef(f.Param("p", ClsL))
	entry := f.Entry()
	slot := entry.Alloc(8, 64)
	entry.Store(pointer, entry.Add(ClsL, slot, f.Long(8)))
	entry.RetVoid()

	// The frontend records some words itself, so the pass must add rather than
	// replace.
	f.StackPointerWords = map[uint32]map[int]bool{slot.ID: {0: true}, 4242: {16: true}}

	InferStackPointerWords(f)

	assert.Equal(t, []int{0, 8}, words(f, slot))
	assert.Equal(t, map[int]bool{16: true}, f.StackPointerWords[4242])
}

func TestInferStackPointerWordsIsIdempotent(t *testing.T) {
	build := func() (*Func, Ref) {
		f := NewModule().NewFuncVoid("idem")
		pointer := f.MarkGCRef(f.Param("p", ClsL))
		entry := f.Entry()
		slot := entry.Alloc(8, 64)
		entry.Store(pointer, entry.Add(ClsL, slot, f.Long(16)))
		entry.RetVoid()
		return f, slot
	}

	once, slot := build()
	InferStackPointerWords(once)
	twice, _ := build()
	InferStackPointerWords(twice)
	InferStackPointerWords(twice)

	assert.Equal(t, once.StackPointerWords, twice.StackPointerWords)
	assert.Equal(t, []int{16}, words(twice, slot))
}

func TestFlattenAggregateReportsPartsInLayoutOrder(t *testing.T) {
	aggregate := &AggType{Name: "mixed", Fields: []Field{
		{Sub: SubB},
		{Sub: SubL, Pointer: true},
		{Sub: SubS},
		{Type: &AggType{Name: "inner", Fields: []Field{
			{Sub: SubUH},
			{Sub: SubL, Pointer: true},
		}}},
	}}

	parts, ok := FlattenAggregate(aggregate)
	require.True(t, ok)
	assert.Equal(t, []AggPart{
		{Sub: SubB, Offset: 0},
		{Sub: SubL, Offset: 8, Pointer: true},
		{Sub: SubS, Offset: 16},
		{Sub: SubUH, Offset: 24},
		{Sub: SubL, Offset: 32, Pointer: true},
	}, parts)
}

func TestFlattenAggregateRefusesShapesWithNoFlatForm(t *testing.T) {
	pointer := Field{Sub: SubL, Pointer: true}
	cases := map[string]*AggType{
		"nil":          nil,
		"opaque":       {Name: "o", Opaque: true, Size: 8},
		"union":        {Name: "u", Union: true, Cases: [][]Field{{pointer}}},
		"inline array": {Name: "a", Fields: []Field{{Sub: SubL, Count: 2}}},
		"nested array": {Name: "n", Fields: []Field{{Type: &AggType{Name: "i", Fields: []Field{{Sub: SubL, Count: 3}}}}}},
	}
	for name, aggregate := range cases {
		t.Run(name, func(t *testing.T) {
			parts, ok := FlattenAggregate(aggregate)
			assert.False(t, ok)
			assert.Nil(t, parts)
		})
	}
}

// The opaque/union check applies to the top-level aggregate only: a nested one is
// walked through its Fields, which an opaque or union type does not use, so it
// contributes no parts and the flattening still succeeds. Pinned because it is
// the behaviour the arm64 Go ABI has always had, not because it is desirable --
// see the note on FlattenAggregate.
func TestFlattenAggregateAcceptsNestedOpaqueAndUnionAsEmpty(t *testing.T) {
	pointer := Field{Sub: SubL, Pointer: true}
	cases := map[string]*AggType{
		"nested union":  {Name: "nu", Fields: []Field{{Type: &AggType{Name: "u2", Union: true, Cases: [][]Field{{pointer}}}}}},
		"nested opaque": {Name: "no", Fields: []Field{{Type: &AggType{Name: "o2", Opaque: true, Size: 8}}}},
	}
	for name, aggregate := range cases {
		t.Run(name, func(t *testing.T) {
			parts, ok := FlattenAggregate(aggregate)
			assert.True(t, ok)
			assert.Empty(t, parts)
		})
	}
}

// FlattenAggregate places fields at their natural alignment whether or not the
// aggregate is packed, unlike AggType.Leaves. Pinned for the same reason.
func TestFlattenAggregateIgnoresPackedLayout(t *testing.T) {
	aggregate := &AggType{Name: "packed", Packed: true, Fields: []Field{
		{Sub: SubB},
		{Sub: SubL, Pointer: true},
	}}

	parts, ok := FlattenAggregate(aggregate)
	require.True(t, ok)
	assert.Equal(t, 8, parts[1].Offset)

	leaves, _ := aggregate.Leaves()
	assert.Equal(t, 1, leaves[1].Off, "Leaves does honour Packed, so the two disagree")
}

func TestFlattenAggregateAcceptsCountOneAndZero(t *testing.T) {
	// Count 0 and 1 both mean a single element; only Count > 1 is an array.
	aggregate := &AggType{Name: "counts", Fields: []Field{
		{Sub: SubL, Count: 0},
		{Sub: SubL, Count: 1, Pointer: true},
	}}
	parts, ok := FlattenAggregate(aggregate)
	require.True(t, ok)
	assert.Equal(t, []AggPart{
		{Sub: SubL, Offset: 0},
		{Sub: SubL, Offset: 8, Pointer: true},
	}, parts)
}

func TestAggregatePointerOffsetsWalksArraysAndNesting(t *testing.T) {
	aggregate := &AggType{Name: "outer", Fields: []Field{
		{Sub: SubW},
		{Sub: SubL, Pointer: true, Count: 3},
		{Type: &AggType{Name: "inner", Fields: []Field{
			{Sub: SubL, Pointer: true},
			{Sub: SubL},
		}}, Count: 2},
	}}

	// Unlike FlattenAggregate this describes an aggregate passed in memory, so an
	// inline array is walked rather than refused.
	assert.Equal(t, []int{8, 16, 24, 32, 48}, AggregatePointerOffsets(aggregate))
}

func TestAggregatePointerOffsetsSkipsOverlappingAndUnknownShapes(t *testing.T) {
	pointer := Field{Sub: SubL, Pointer: true}
	assert.Nil(t, AggregatePointerOffsets(nil))
	assert.Nil(t, AggregatePointerOffsets(&AggType{Name: "o", Opaque: true, Size: 16}))
	assert.Nil(t, AggregatePointerOffsets(&AggType{Name: "u", Union: true, Cases: [][]Field{{pointer}}}))
	// A nested union contributes nothing, but the fields after it still get their
	// correct offsets: the union's size is known even though its contents are not.
	nested := &AggType{Name: "n", Fields: []Field{
		{Type: &AggType{Name: "u2", Union: true, Cases: [][]Field{{pointer}, {pointer, pointer}}}},
		pointer,
	}}
	assert.Equal(t, []int{16}, AggregatePointerOffsets(nested))
}

func TestMarkAggregatePointerWordsRecordsWholeWordPointerFields(t *testing.T) {
	f := NewModule().NewFuncVoid("mark")
	slot := Ref{Kind: RefTemp, ID: 3}

	// A pointer field that does not land on a word boundary cannot be named as a
	// root, so it is dropped.
	f.MarkAggregatePointerWords(slot, &AggType{Name: "straddling", Fields: []Field{
		{Sub: SubB},
		{Sub: SubB},
		{Sub: SubB, Pointer: true, Count: 6},
		{Sub: SubL, Pointer: true},
	}})
	assert.Equal(t, map[int]bool(nil), f.StackPointerWords[slot.ID])

	f.MarkAggregatePointerWords(slot, &AggType{Name: "aligned", Fields: []Field{
		{Sub: SubL},
		{Sub: SubL, Pointer: true},
	}})
	assert.Equal(t, map[int]bool{8: true}, f.StackPointerWords[slot.ID])
}

func TestMarkAggregatePointerWordsIgnoresUnflattenableTypes(t *testing.T) {
	f := NewModule().NewFuncVoid("mark_union")
	slot := Ref{Kind: RefTemp, ID: 1}
	f.MarkAggregatePointerWords(slot, &AggType{
		Name:  "u",
		Union: true,
		Cases: [][]Field{{{Sub: SubL, Pointer: true}}},
	})
	assert.Nil(t, f.StackPointerWords)
}

func TestAllocAggregateChoosesTheOpFromAlignment(t *testing.T) {
	cases := []struct {
		align int
		want  Op
	}{
		{1, OAlloc4},
		{4, OAlloc4},
		{8, OAlloc8},
		{16, OAlloc16},
	}
	for _, testCase := range cases {
		f := NewModule().NewFuncVoid("alloc")
		aggregate := &AggType{Name: "a", Align: testCase.align, Fields: []Field{{Sub: SubB}}}
		var out []Instr
		slot := f.AllocAggregate(aggregate, &out)

		require.Len(t, out, 1)
		assert.Equal(t, testCase.want, out[0].Op)
		assert.Equal(t, slot, out[0].To)
		assert.False(t, f.Temp(slot).GCRef, "a fixed stack needs no GC metadata")
		assert.Nil(t, f.StackPointerWords)
	}
}

func TestAllocAggregateTracksTheSlotUnderAManagedFrame(t *testing.T) {
	f := NewModule().NewFuncVoid("alloc_managed")
	f.CallConv = CallConvGoInternal
	f.ManagedFrame = true
	aggregate := &AggType{Name: "pair", Fields: []Field{
		{Sub: SubL, Pointer: true},
		{Sub: SubL},
		{Sub: SubL, Pointer: true},
	}}

	var out []Instr
	slot := f.AllocAggregate(aggregate, &out)

	require.Len(t, out, 1)
	assert.True(t, f.Temp(slot).GCRef, "a copied stack must relocate the interior pointer")
	assert.Equal(t, map[int]bool{0: true, 16: true}, f.StackPointerWords[slot.ID])
}

func TestAllocAggregateGivesAnEmptyTypeOneByte(t *testing.T) {
	f := NewModule().NewFuncVoid("alloc_empty")
	var out []Instr
	f.AllocAggregate(&AggType{Name: "empty"}, &out)
	require.Len(t, out, 1)
	require.Len(t, out[0].Args, 1)
	assert.Equal(t, int64(1), f.Consts[out[0].Args[0].ID].Int)
}

func TestValueGroupAtFindsTheGroupCoveringAnIndex(t *testing.T) {
	groups := []ValueGroup{
		{Index: 0, Count: 2, Type: &AggType{Name: "a"}},
		{Index: 3, Count: 3, Type: &AggType{Name: "b"}},
	}

	group, ok := ValueGroupAt(groups, 3)
	require.True(t, ok)
	assert.Equal(t, 3, group.Count)

	_, ok = ValueGroupAt(groups, 1)
	assert.False(t, ok, "only a group's first index identifies it")

	_, ok = ValueGroupAt(nil, 0)
	assert.False(t, ok)
}

func TestSubClassOpTablesCoverEverySubClass(t *testing.T) {
	all := []SubCls{SubB, SubUB, SubH, SubUH, SubW, SubL, SubS, SubD, SubQ}
	for _, sub := range all {
		load := LoadOpForSub(sub)
		store := StoreOpForSub(sub)
		assert.True(t, load.IsLoad(), "%v does not map to a load", sub)
		assert.True(t, store.IsStore(), "%v does not map to a store", sub)
	}

	// Loads keep signedness; stores do not distinguish it.
	assert.Equal(t, OLoadsb, LoadOpForSub(SubB))
	assert.Equal(t, OLoadub, LoadOpForSub(SubUB))
	assert.Equal(t, StoreOpForSub(SubB), StoreOpForSub(SubUB))
	assert.Equal(t, StoreOpForSub(SubH), StoreOpForSub(SubUH))

	assert.Panics(t, func() { LoadOpForSub(SubCls(200)) })
	assert.Panics(t, func() { StoreOpForSub(SubCls(200)) })
}

func TestFieldSizeAlignReportsNestedAggregateLayout(t *testing.T) {
	scalar := Field{Sub: SubW}
	size, align := scalar.SizeAlign()
	assert.Equal(t, 4, size)
	assert.Equal(t, 4, align)

	nested := Field{Type: &AggType{Name: "n", Fields: []Field{{Sub: SubB}, {Sub: SubL}}}}
	size, align = nested.SizeAlign()
	assert.Equal(t, 16, size)
	assert.Equal(t, 8, align)

	// It reports one element, not the whole array: the caller multiplies.
	array := Field{Sub: SubL, Count: 4}
	size, align = array.SizeAlign()
	assert.Equal(t, 8, size)
	assert.Equal(t, 8, align)
}
