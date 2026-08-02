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
}
