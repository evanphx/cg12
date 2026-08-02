package ir

// AllocPlacement names where an allocation the compiler decided about ended up.
type AllocPlacement string

const (
	// AllocInFrame is an allocation the escape decision left in the calling
	// function's frame.
	AllocInFrame AllocPlacement = "frame"
	// AllocOnHeap is an allocation the escape decision sent to the allocator.
	AllocOnHeap AllocPlacement = "heap"
)

// AllocDecision records one placement decision an optimization pass made about
// a typed allocation candidate, so a census can report it afterwards.
//
// It exists because the decision is not recoverable from the finished IR. A
// candidate sent to the heap keeps its type descriptor as an argument of the
// allocator call, but a candidate promoted into the frame becomes a bare
// OAlloc{4,8,16} carrying only a byte size -- indistinguishable from the
// thousands of ordinary local-variable slots in the same function, and with the
// type gone. Anything that wants to say "this object moved from the frame to the
// heap" therefore has to be told at the moment the pass decides, not asked
// afterwards.
//
// Nothing in the compiler reads these records; they do not affect code
// generation, and a module with none behaves identically to a module with all of
// them. They are diagnostic output that happens to ride on the module because
// that is what a caller of the whole pipeline gets back.
type AllocDecision struct {
	// Func is the IR function the allocation happens in, after inlining -- so a
	// candidate spliced into three callers is three decisions, which is right:
	// each copy is decided separately and can land differently.
	Func string
	// Pos is the source position of the allocating instruction, when the front
	// end recorded one.
	Pos SrcPos
	// Allocator names the allocator the candidate would have called, which is
	// what a heap placement does call.
	Allocator string
	// Type is the type-descriptor symbol of the allocated object. For a type
	// with no name of its own the symbol carries the size, as in "24_byte".
	Type string
	// Placement is where it landed.
	Placement AllocPlacement
	// BlockedByLoop marks a heap placement the escape analysis was willing to
	// promote and the loop rule sent to the allocator anyway, because the
	// allocation is inside a loop body and one frame slot cannot hold one object
	// per iteration. It is the only case where Placement is not what the escape
	// analysis decided, so it is recorded rather than inferred.
	BlockedByLoop bool
}

// PlacedAlloc records one allocation the *front end* placed itself, without
// routing it through the neutral OHeapAlloc candidate form -- the sites where a
// source-level escape analysis decided, and where the IR pass therefore never
// gets a say.
//
// It is diagnostic only, and for exactly one purpose: comparing the two
// analyses. An allocation the front end committed to a frame is an ordinary
// OAlloc that nothing distinguishes from a local variable's slot, and one it
// committed to the heap is an allocator call that nothing distinguishes from a
// candidate the IR pass lowered. Without a record made at the moment of the
// decision, neither can be found again, and "would the IR pass have agreed" is
// a question with no way to ask it. See opt.ShadowPlacement.
//
// Nothing in the compiler reads these records and a module with none compiles
// to the same code.
type PlacedAlloc struct {
	// Site names the front-end decision site, so a disagreement can be
	// attributed to the predicate that made it.
	Site string
	// Placement is where the front end put the object.
	Placement AllocPlacement
	// Type is the type-descriptor symbol, when the site had one.
	Type string
	// Pos is the source position of the decision.
	Pos SrcPos
}
