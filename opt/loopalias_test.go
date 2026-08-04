package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopShape builds the CFG every test below shares: an entry that falls into a
// header, a body, a latch back to the header, and an exit. It returns the four
// blocks so a test can put its allocation and its store where it means to.
//
// The shape matters more than it looks. The rule is stated in terms of a natural
// loop's body -- the blocks that can reach a latch -- and the difference between
// "inside the loop" and "reached from inside the loop but on the way out" is one
// of the two false positives the audit had to be taught about.
func loopShape(function *ir.Func) (entry, header, body, latch, exit *ir.Block) {
	entry = function.NewBlock("entry")
	header = function.NewBlock("header")
	body = function.NewBlock("body")
	latch = function.NewBlock("latch")
	exit = function.NewBlock("exit")
	entry.Goto(header)
	header.Jnz(function.Word(1), body, exit)
	body.Goto(latch)
	latch.Goto(header)
	exit.RetVoid()
	return entry, header, body, latch, exit
}

// The defect itself, in the shape goc emits before mem2reg: an object allocated
// by the loop body, whose address is stored into the slot of a variable declared
// outside the loop. The next iteration allocates nothing new, so that slot names
// this iteration's object for ever after.
func TestLoopAliasesReportsAnAddressParkedInAnOuterSlot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("carry")
	entry, _, body, _, _ := loopShape(function)
	outer := entry.Alloc(8, 8)
	object := body.Alloc(8, 16)
	body.Store(object, outer)

	aliases := LoopAliases(module)
	require.Len(t, aliases, 1)
	assert.Equal(t, "carry", aliases[0].Func)
	assert.Equal(t, LoopAliasParked, aliases[0].Kind)
	assert.Equal(t, LoopAliasIntoOuterSlot, aliases[0].Destination)
}

// The same address reached the same way goc reaches it: through the loop-body
// slot of the variable that names the object. It is the shape every one of the
// four miscompiled reduction forms has, and it is the reason this cannot be a
// syntactic check on the store -- the value stored is a load, and only the frame
// may-analysis knows what that load produces.
func TestLoopAliasesFollowsAnAddressThroughALoopBodySlot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("carry")
	entry, _, body, _, _ := loopShape(function)
	outer := entry.Alloc(8, 8)
	object := body.Alloc(8, 16)
	named := body.Alloc(8, 8)
	body.Store(object, named)
	body.Store(body.Load(ir.ClsP, named), outer)

	aliases := LoopAliases(module)
	require.Len(t, aliases, 1)
	assert.Equal(t, LoopAliasParked, aliases[0].Kind)
}

// The SSA form of the same defect, which is what the parked store becomes once
// mem2reg has promoted the slot away: a header phi taking the address in along
// the back edge. Nothing in the corpus reaches this arm -- the audit runs on
// unoptimized IR, where goc's variables are still slots -- so it is asserted
// here or nowhere.
func TestLoopAliasesReportsAnAddressCarriedAcrossTheBackEdge(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("carry")
	entry, header, body, latch, _ := loopShape(function)
	header.Phi(ir.ClsP, ir.PhiEdge{From: entry, Val: function.Long(0)})
	object := body.Alloc(8, 16)
	header.Phis[0].Add(latch, object)

	aliases := LoopAliases(module)
	require.Len(t, aliases, 1)
	assert.Equal(t, LoopAliasCarried, aliases[0].Kind)
	assert.Equal(t, LoopAliasIntoBackEdge, aliases[0].Destination)
}

// The negative the audit exists to keep: a loop-body allocation whose address
// goes no further than another slot the loop body allocates. That slot is a
// variable the source re-declares on every trip, so the next iteration
// overwrites it before reading it, and the sharing is unobservable. This is
// testdata/loop_alias_frame_local.go's shape, and a rule that fired here would
// heap every scratch buffer in every loop in the corpus.
func TestLoopAliasesAllowsAnAddressKeptInsideTheIteration(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("scratch")
	_, _, body, _, _ := loopShape(function)
	object := body.Alloc(8, 16)
	body.Store(object, body.Alloc(8, 8))

	assert.Empty(t, LoopAliases(module))
}

// An allocation made outside the loop and parked in a slot outside it is not a
// per-iteration question at all: there is one object, which is what the source
// says too.
func TestLoopAliasesAllowsAnOuterAllocation(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("hoisted")
	entry, _, body, _, _ := loopShape(function)
	object := entry.Alloc(8, 16)
	outer := entry.Alloc(8, 8)
	body.Store(object, outer)

	assert.Empty(t, LoopAliases(module))
}

// A store on the way out of the loop. The block is reached from inside an
// iteration but cannot reach a latch, so no further iteration follows it and the
// address it parks is the last iteration's -- which one slot represents
// faithfully. runtime.getGCMaskOnDemand does exactly this, parking a loop-body
// local's address in a closure environment on the path that returns, and reading
// it as loop-carried was the audit's first false positive.
func TestLoopAliasesAllowsAStoreOnTheWayOutOfTheLoop(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("leaving")
	entry, _, body, latch, _ := loopShape(function)
	leave := function.NewBlock("leave")
	body.Jnz(function.Word(1), latch, leave)
	leave.RetVoid()

	outer := entry.Alloc(8, 8)
	object := body.Alloc(8, 16)
	leave.Store(object, outer)

	assert.Empty(t, LoopAliases(module))
}

// A loop-body address stored into a global has left the frame altogether. That
// is FrameEscapes' finding and a worse one; reporting it here as well would put
// one defect in two baselines.
func TestLoopAliasesLeavesAPublicationToFrameEscapes(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("publish")
	_, _, body, _, _ := loopShape(function)
	body.Store(body.Alloc(8, 16), function.Sym("keepalive.root", 0))

	assert.Empty(t, LoopAliases(module))
	require.Len(t, FrameEscapes(module), 1)
}

// A function with no loop at all is the common case and must cost nothing but
// the CFG.
func TestLoopAliasesIgnoresAFunctionWithNoLoop(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("straight")
	block := function.Entry()
	slot := block.Alloc(8, 8)
	block.Store(block.Alloc(8, 16), slot)
	block.RetVoid()

	assert.Empty(t, LoopAliases(module))
}

// The key is built from source positions alone, so that an unrelated change
// which emits one more instruction -- renumbering every temporary after it --
// does not rewrite the baseline.
func TestLoopAliasKeyIsMadeOfPositionsOnly(t *testing.T) {
	alias := LoopAlias{
		Func:        "carry",
		Kind:        LoopAliasParked,
		Destination: LoopAliasIntoOuterSlot,
		Pos:         ir.SrcPos{File: 1, Line: 7, Col: 3},
		File:        "loop.go",
		CrossPos:    ir.SrcPos{File: 1, Line: 9, Col: 4},
		CrossFile:   "loop.go",
		Alloc:       "%t42",
		Slot:        "%t7",
	}
	assert.NotContains(t, alias.Key(), "%t42")
	assert.NotContains(t, alias.Key(), "%t7")
	assert.Contains(t, alias.Key(), "loop.go:7:3")
	assert.Contains(t, alias.Key(), "loop.go:9:4")
	assert.Contains(t, alias.String(), "%t42")
}
