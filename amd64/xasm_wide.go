package amd64

// xasmWide is 128-bit memory traffic: the OLoadq/OStoreq pair, which is entirely
// unhandled today, encoded with MOVDQU/MOVDQA (or MOVUPS where the operand is
// float-typed). Landing it is what allows the 128-bit skip at foldaddr.go:41-45 to
// be removed.
//
// Empty until Phase 2 Track A, agent A4 of AMD64_PARITY_PLAN.md fills it in; A4
// also owns xselect_wide.go. It is deliberately its own file so that adding the
// family is a write here plus one line in xasm.go, and nothing else.
type xasmWide interface {
}
