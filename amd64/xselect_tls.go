package amd64

import "github.com/evanphx/cg12/ir"

// selectTLS selects thread-local storage addressing, and claims nothing.
//
// The TLS models are implemented -- Options.TLSModel, with the local-exec and
// initial-exec sequences in tls.go -- but on x86-64 neither of them is an
// instruction to select: both are "load the thread pointer, add the variable's
// offset to it", one register from start to finish, which the operand
// materialization path emits directly. xasm_tls.go explains why the machine allows
// that and arm64's does not.
//
// It stays in the probe chain, empty, because the ops that would need it are real:
// general-dynamic ends in a call to __tls_get_addr, which has to be an ir.OCall
// the register allocator can see, which makes the descriptor address an
// ir.OTLSIndexAddr selected here (compare arm64/select.go's OThreadPtr /
// OTLSOffset / OTLSIndexAddr cases). Nothing emits those ops on amd64 yet, and
// nothing should until obj/ can link an x86-64 TLSGD relocation -- tls.go's
// materializeThreadSym lists what is missing. Claiming them now would be
// unreachable code standing in for the part that is actually absent.
func selectTLS(s *xsel, in *ir.Instr) bool {
	return false
}
