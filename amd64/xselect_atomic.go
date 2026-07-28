package amd64

import "github.com/evanphx/cg12/ir"

// selectAtomic selects the atomic and synchronization operations through the
// xasmAtomic half of the builder.
//
// It claims nothing yet: Phase 2 Track A, agent A1 of AMD64_PARITY_PLAN.md owns
// this file and xasm_atomic.go, and fills both in together. The registration in
// xselect_registry.go is already in place, so that work is a write to these two
// files and no edit to any shared one.
func selectAtomic(s *xsel, in *ir.Instr) bool {
	return false
}
