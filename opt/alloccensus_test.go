package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// typeSymbol is a type descriptor named the way goc interns them: a readable
// mangling of the Go type, then a digest of the type's full definition.
func typeSymbol(readable string) string {
	return ".goc.runtime.type." + readable + ".0123456789abcdef"
}

func censusKeys(module *ir.Module) []string {
	census := AllocationCensus(module)
	keys := make([]string, 0, len(census))
	for _, allocation := range census {
		keys = append(keys, allocation.Key())
	}
	return keys
}

func TestAllocationCensusReportsAPromotedCandidateAsFrame(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("local", ir.ClsL)
	block := function.Entry()
	object := block.At(ir.SrcPos{File: module.File("a.go"), Line: 7, Col: 3}).
		HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym(typeSymbol("main_T"), 0), 8, 8)
	block.Store(function.Long(42), object)
	block.Ret(block.Load(ir.ClsL, object))

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, []string{
		"a.go:7:3\tlocal\truntime.newobject\tmain_T\tframe",
	}, censusKeys(module))
}

// A candidate lowered to a call and a call the front end emitted itself are the
// same instruction afterwards, and the census must render them the same way:
// the object is on the heap either way, and which pass decided is not a property
// a baseline should churn on.
func TestAllocationCensusReportsAnEscapingCandidateAsHeap(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("escape", ir.ClsP)
	block := function.Entry()
	object := block.At(ir.SrcPos{File: module.File("a.go"), Line: 9, Col: 12}).
		HeapAlloc(function.Sym("runtime.newobject", 0), function.Sym(typeSymbol("main_T"), 0), 8, 8)
	block.Ret(object)

	require.True(t, LowerHeapAllocations(module))
	assert.Equal(t, []string{
		"a.go:9:12\tescape\truntime.newobject\tmain_T\theap",
	}, censusKeys(module))
}

func TestAllocationCensusReportsAFrontEndAllocatorCall(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("direct", ir.ClsP)
	block := function.Entry()
	object := block.At(ir.SrcPos{File: module.File("a.go"), Line: 4, Col: 2}).
		Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym(typeSymbol("main_T"), 0))
	block.Ret(object)

	assert.Equal(t, []string{
		"a.go:4:2\tdirect\truntime.newobject\tmain_T\theap",
	}, censusKeys(module))
}

func TestAllocationCensusReportsMakeAllocators(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("makes", ir.ClsW)
	block := function.Entry()
	file := module.File("a.go")
	block.At(ir.SrcPos{File: file, Line: 1, Col: 1}).
		Call(ir.ClsP, function.Sym("runtime.makeslice", 0), function.Sym(typeSymbol("int"), 0), function.Long(2), function.Long(2))
	block.At(ir.SrcPos{File: file, Line: 2, Col: 1}).
		Call(ir.ClsP, function.Sym("runtime.makechan", 0), function.Sym(typeSymbol("chan_int"), 0), function.Long(0))
	block.At(ir.SrcPos{File: file, Line: 3, Col: 1}).
		Call(ir.ClsP, function.Sym("runtime.makemap_small", 0))
	block.Ret(function.Word(0))

	assert.Equal(t, []string{
		"a.go:1:1\tmakes\truntime.makeslice\tint\theap",
		"a.go:2:1\tmakes\truntime.makechan\tchan_int\theap",
		"a.go:3:1\tmakes\truntime.makemap_small\t-\theap",
	}, censusKeys(module))
}

// growslice reallocates storage that already exists. No escape decision picks
// it, so it is not a placement and does not belong in a placement census.
func TestAllocationCensusIgnoresGrowslice(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("grow", ir.ClsP)
	block := function.Entry()
	object := block.Call(ir.ClsP, function.Sym("runtime.growslice", 0), function.Sym(typeSymbol("int"), 0), function.Long(1))
	block.Ret(object)

	assert.Empty(t, censusKeys(module))
}

// Ordinary frame slots are not decisions and outnumber everything else in the
// census by two orders of magnitude; including them would drown it.
func TestAllocationCensusIgnoresOrdinaryFrameSlots(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("slot", ir.ClsL)
	block := function.Entry()
	slot := block.Alloc(8, 8)
	block.Store(function.Long(1), slot)
	block.Ret(block.Load(ir.ClsL, slot))

	assert.Empty(t, censusKeys(module))
}

func TestAllocationCensusIsSortedAndDeduplicated(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("twice", ir.ClsW)
	block := function.Entry()
	file := module.File("a.go")
	descriptor := function.Sym(typeSymbol("main_T"), 0)
	block.At(ir.SrcPos{File: file, Line: 20, Col: 1}).Call(ir.ClsP, function.Sym("runtime.newobject", 0), descriptor)
	block.At(ir.SrcPos{File: file, Line: 3, Col: 1}).Call(ir.ClsP, function.Sym("runtime.newobject", 0), descriptor)
	// The same site again: one loop body, two paths through it, one record.
	block.At(ir.SrcPos{File: file, Line: 3, Col: 1}).Call(ir.ClsP, function.Sym("runtime.newobject", 0), descriptor)
	block.Ret(function.Word(0))

	assert.Equal(t, []string{
		"a.go:20:1\ttwice\truntime.newobject\tmain_T\theap",
		"a.go:3:1\ttwice\truntime.newobject\tmain_T\theap",
	}, censusKeys(module))
}

// A site the front end gave no position still has to appear: the census is
// keyed on the site, and dropping the ones it cannot name would quietly exclude
// a third of the corpus.
func TestAllocationCensusKeepsPositionlessSites(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("nopos", ir.ClsP)
	block := function.Entry()
	block.Ret(block.Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym(typeSymbol("main_T"), 0)))

	assert.Equal(t, []string{
		"?\tnopos\truntime.newobject\tmain_T\theap",
	}, censusKeys(module))
}

// The site is everything but the placement, so a move rewrites one field of one
// line instead of removing a line and adding another. The test that reads the
// baseline depends on this.
func TestAllocationSiteExcludesPlacement(t *testing.T) {
	frame := Allocation{Func: "f", Allocator: "runtime.newobject", Type: "main_T", Placement: ir.AllocInFrame}
	heap := frame
	heap.Placement = ir.AllocOnHeap

	assert.Equal(t, frame.Site(), heap.Site())
	assert.Equal(t, frame.Site()+"\tframe", frame.Key())
	assert.Equal(t, heap.Site()+"\theap", heap.Key())
}

func TestAllocationTypeNameStripsPrefixAndDigest(t *testing.T) {
	assert.Equal(t, "main_T", AllocationTypeName(".goc.runtime.type.main_T.0123456789abcdef"))
	assert.Equal(t, "map_string_int", AllocationTypeName(".goc.type.map_string_int.fedcba9876543210"))
	assert.Equal(t, "-", AllocationTypeName(""))
	// Not one of goc's interned descriptors: leave it alone rather than guess.
	assert.Equal(t, "type.int", AllocationTypeName("type.int"))
}

// censusKeysWith is censusKeys with the counting rules chosen explicitly.
func censusKeysWith(module *ir.Module, options AllocationCensusOptions) []string {
	census := AllocationCensusWith(module, options)
	keys := make([]string, 0, len(census))
	for _, allocation := range census {
		keys = append(keys, allocation.Key())
	}
	return keys
}

// The whole point of recording a front-end frame slot is that the record names
// the same site the heap record of the same decision would name. If the two
// identities differ by so much as the allocator column, an object moving from
// the frame to the heap still reads as one site vanishing and another appearing,
// which is the reporting defect this option exists to fix.
func TestAllocationCensusPairsAFrontEndFrameSlotWithItsHeapForm(t *testing.T) {
	position := func(module *ir.Module) ir.SrcPos {
		return ir.SrcPos{File: module.File("a.go"), Line: 11, Col: 6}
	}
	options := AllocationCensusOptions{IncludeFrontEndFrameSlots: true}

	framed := ir.NewModule()
	function := framed.NewFunc("literal", ir.ClsP)
	block := function.Entry()
	slot := block.At(position(framed)).Alloc(8, 8)
	function.PlacedAllocs = map[uint32]ir.PlacedAlloc{slot.ID: {
		Site:      "composite-literal",
		Placement: ir.AllocInFrame,
		Allocator: "runtime.newobject",
		Type:      typeSymbol("main_T"),
	}}
	block.Ret(slot)

	heaped := ir.NewModule()
	function = heaped.NewFunc("literal", ir.ClsP)
	block = function.Entry()
	object := block.At(position(heaped)).
		Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym(typeSymbol("main_T"), 0))
	function.PlacedAllocs = map[uint32]ir.PlacedAlloc{object.ID: {
		Site:      "composite-literal",
		Placement: ir.AllocOnHeap,
		Allocator: "runtime.newobject",
		Type:      typeSymbol("main_T"),
	}}
	block.Ret(object)

	before := censusKeysWith(framed, options)
	after := censusKeysWith(heaped, options)
	require.Equal(t, []string{"a.go:11:6\tliteral\truntime.newobject\tmain_T\tframe"}, before)
	require.Equal(t, []string{"a.go:11:6\tliteral\truntime.newobject\tmain_T\theap"}, after)

	beforeSite := AllocationCensusWith(framed, options)[0].Site()
	afterSite := AllocationCensusWith(heaped, options)[0].Site()
	assert.Equal(t, beforeSite, afterSite,
		"a front-end frame slot and the heap allocation the same decision makes when it\n"+
			"goes the other way must be one site, or the move cannot be reported as one")
}

// Without the option the frame half of that pair is not recorded at all, which
// is the state in which a frame-to-heap move arrives as a site appearing.
func TestAllocationCensusOmitsFrontEndFrameSlotsByDefault(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("literal", ir.ClsP)
	block := function.Entry()
	slot := block.At(ir.SrcPos{File: module.File("a.go"), Line: 11, Col: 6}).Alloc(8, 8)
	function.PlacedAllocs = map[uint32]ir.PlacedAlloc{slot.ID: {
		Site:      "composite-literal",
		Placement: ir.AllocInFrame,
		Allocator: "runtime.newobject",
		Type:      typeSymbol("main_T"),
	}}
	block.Ret(slot)

	assert.Empty(t, censusKeys(module))
	assert.Equal(t,
		[]string{"a.go:11:6\tliteral\truntime.newobject\tmain_T\tframe"},
		censusKeysWith(module, AllocationCensusOptions{IncludeFrontEndFrameSlots: true}))
}

// A front-end frame placement whose heap form is not an allocator call has
// nothing to pair with -- the string-conversion buffer's alternative is a nil
// argument and an allocation inside runtime.stringtoslicebyte, which is not a
// census site on either side. Recording it would add a line that can only ever
// vanish, never move.
func TestAllocationCensusOmitsAFrontEndFrameSlotWithNoAllocator(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("convert", ir.ClsP)
	block := function.Entry()
	buffer := block.At(ir.SrcPos{File: module.File("a.go"), Line: 4, Col: 2}).Alloc(8, 32)
	function.PlacedAllocs = map[uint32]ir.PlacedAlloc{buffer.ID: {
		Site:      "string-conversion-buffer",
		Placement: ir.AllocInFrame,
	}}
	block.Ret(buffer)

	assert.Empty(t, censusKeysWith(module, AllocationCensusOptions{IncludeFrontEndFrameSlots: true}))
}

// A front-end heap placement is an allocator call and is read out of the IR, so
// the option must not record it a second time under a different spelling.
func TestAllocationCensusDoesNotDoubleCountAFrontEndHeapPlacement(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("escapes", ir.ClsP)
	block := function.Entry()
	object := block.At(ir.SrcPos{File: module.File("a.go"), Line: 8, Col: 9}).
		Call(ir.ClsP, function.Sym("runtime.newobject", 0), function.Sym(typeSymbol("main_T"), 0))
	function.PlacedAllocs = map[uint32]ir.PlacedAlloc{object.ID: {
		Site:      "escaping-typed",
		Placement: ir.AllocOnHeap,
		Allocator: "runtime.newobject",
		Type:      typeSymbol("main_T"),
	}}
	block.Ret(object)

	assert.Equal(t,
		[]string{"a.go:8:9\tescapes\truntime.newobject\tmain_T\theap"},
		censusKeysWith(module, AllocationCensusOptions{IncludeFrontEndFrameSlots: true}))
}
