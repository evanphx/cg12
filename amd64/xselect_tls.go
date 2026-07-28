package amd64

import "github.com/evanphx/cg12/ir"

// selectTLS selects thread-local storage addressing through the xasmTLS half of
// the builder, one sequence per TLS model.
//
// It claims nothing yet: Phase 2 Track A, agent A5 of AMD64_PARITY_PLAN.md owns
// this file and xasm_tls.go, and fills both in together. The registration in
// xselect_registry.go is already in place, so that work is a write to these two
// files and no edit to any shared one.
func selectTLS(s *xsel, in *ir.Instr) bool {
	return false
}
