package amd64

// xasmBits is the bit-manipulation surface: OClz (BSR, or LZCNT where it is
// available), the high half of a widening multiply for OUMulh/OSMulh (the
// one-operand MUL/IMUL forms), population count, the rotates, the double-precision
// shifts SHLD/SHRD, and byte swapping.
//
// Empty until Phase 2 Track A, agent A2 of AMD64_PARITY_PLAN.md fills it in; A2
// also owns xselect_bits.go. It is deliberately its own file so that adding the
// family is a write here plus one line in xasm.go, and nothing else.
type xasmBits interface {
}
