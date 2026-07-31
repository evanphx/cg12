package amd64

// xasm is the amd64 instruction builder: an x86-idiomatic assembler surface whose
// operations mirror the x64 encoders, implemented by mcXasm. It keeps the
// instruction selection that drives it (xsel) separate from the encoding, the
// same way asmb does on arm64.
//
// x86 is a two-operand, move-heavy machine, so the fundamental primitive is
// move(dst, src) over the shared loc abstraction; the arithmetic ops take
// registers the selector has already placed operands into.
//
// The surface is composed rather than written out: one sub-interface per concern,
// each declared in its own file directly above the mcXasm methods implementing
// it. That split is what keeps the builder growable. A new family of machine
// operations arrives as a new xasm_<family>.go carrying both its sub-interface
// and its implementation, so adding one costs at most a single line here --
// nothing else in this file, and no other file, has to move. This declaration is
// only a table of contents.
type xasm interface {
	xasmCore  // operand locations, moves, spill stores, failure
	xasmInt   // integer arithmetic, shifts, compares, extensions, divide
	xasmImm   // the same arithmetic and compares with a constant operand; CMOVcc
	xasmFloat // SSE arithmetic and compares, float/int conversions, bitcasts
	xasmMem   // loads and stores
	xasmFlow  // branches, the jump table, calls, ret
	xasmStack // stack pointer, VLA allocation, frame teardown

	// Empty today; Phase 2 of AMD64_PARITY_PLAN.md fills each one in. Each file
	// names the operations that land there and the plan item that owns it.
	xasmAtomic
	xasmBits
	xasmWide
	xasmTLS
}

// --- machine-code backend --------------------------------------------------

// mcXasm implements xasm against an mc, encoding each operation into its program.
// Its methods are not gathered here: each lives in the file that declares the
// sub-interface it satisfies, so a family's declaration and its encoding stay
// within one screen of each other.
type mcXasm struct{ m *mc }
