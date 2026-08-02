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
