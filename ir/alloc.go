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
	// Reason names the use that escaped the candidate, in the analysis's own
	// words, or is empty for a frame placement -- there is no single use to name
	// for one.
	//
	// It is empty unless the escape diagnostic is on. The strings are built by
	// the mark loop as it marks, so what is printed is what decided; nothing
	// re-derives them afterwards. Off, the map they come from is never allocated
	// and no string is ever formatted. See opt.EscapeDiagLevel.
	Reason string
	// Use is where that use is written, when the marking instruction carried a
	// position. Like Reason it is empty unless the diagnostic is on.
	Use SrcPos
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
	// Allocator names the runtime allocator this object would have been passed
	// to had the front end sent it to the heap -- and, when Placement is
	// AllocOnHeap, the one it was passed to.
	//
	// It is empty at a site whose heap form is not an allocator call at all: the
	// string-conversion buffer's alternative is passing nil and letting
	// runtime.stringtoslicebyte allocate internally, which is not a placement
	// decision anyone can see from outside. A frame record with no allocator
	// cannot be paired with the heap record it would become, so it is not a
	// census site; see opt.AllocationCensusOptions.
	Allocator string
	// Type is the type-descriptor symbol, when the site had one. It is the
	// symbol the descriptor would be interned under, computed without emitting
	// it, so that recording a frame placement adds no data to the module.
	Type string
	// Pos is the source position of the decision.
	Pos SrcPos
	// Rule names the predicate in the AST walk that answered, and Use is the
	// position of the use that answered it -- for a heap placement, the
	// publication the walk found; for a frame placement, nothing.
	//
	// Chain is the sequence of questions between the object and that use, from
	// the use outwards: the summary walk into a callee's parameter, the callee's
	// own local, and so on. It is what makes "it escapes" actionable, and it is
	// only filled at diagnostic level 2.
	//
	// All three are empty unless the escape diagnostic is on. They are captured
	// by the walk at the moment it answers rather than recomputed, so a
	// diagnostic that disagrees with the placement beside it is impossible.
	// See opt.EscapeDiagLevel.
	Rule  string
	Use   SrcPos
	Chain []string
}
