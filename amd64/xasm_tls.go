package amd64

// xasmTLS is thread-local storage addressing. Only local-exec exists today, and it
// is hardcoded rather than selected; this family is where the addressing sequence
// for each TLS model goes once Options.TLSModel exists (mirroring arm64/mc.go:36-56)
// -- initial-exec through GOTTPOFF and general-dynamic through TLSGD alongside it.
//
// Empty until Phase 2 Track A, agent A5 of AMD64_PARITY_PLAN.md fills it in; A5
// also owns xselect_tls.go, plus the general-dynamic support in obj/ (Track F,
// F2). It is deliberately its own file so that adding the family is a write here
// plus one line in xasm.go, and nothing else.
type xasmTLS interface {
}
