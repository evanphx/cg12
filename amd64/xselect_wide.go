package amd64

import "github.com/evanphx/cg12/ir"

// selectWide selects the 128-bit memory operations (ir.OLoadq/ir.OStoreq) through
// the xasmWide half of the builder.
//
// It claims nothing yet: Phase 2 Track A, agent A4 of AMD64_PARITY_PLAN.md owns
// this file and xasm_wide.go, and fills both in together. The registration in
// xselect_registry.go is already in place, so that work is a write to these two
// files and no edit to any shared one.
func selectWide(s *xsel, in *ir.Instr) bool {
	return false
}
