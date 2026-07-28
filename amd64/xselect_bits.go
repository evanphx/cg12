package amd64

import "github.com/evanphx/cg12/ir"

// selectBits selects the bit-manipulation operations (count-leading-zeros, the
// high half of a widening multiply, popcount, rotates, double shifts, byte swap)
// through the xasmBits half of the builder.
//
// It claims nothing yet: Phase 2 Track A, agent A2 of AMD64_PARITY_PLAN.md owns
// this file and xasm_bits.go, and fills both in together. The registration in
// xselect_registry.go is already in place, so that work is a write to these two
// files and no edit to any shared one.
func selectBits(s *xsel, in *ir.Instr) bool {
	return false
}
