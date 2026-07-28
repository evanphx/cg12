package amd64

// xasmTLS is thread-local storage addressing, and it is empty on purpose.
//
// Phase 2 Track A, agent A5 of AMD64_PARITY_PLAN.md added the TLS models --
// Options.TLSModel, with the local-exec and initial-exec sequences in tls.go --
// and they turned out to need no builder surface. Reaching a thread-local on
// x86-64 is not an instruction a selector picks; it is how a symbol operand
// becomes a register, so it lives on the operand-materialization path
// (mc.materializeSym -> mc.materializeThreadSym) instead of behind an xasm method.
//
// That is a property of the machine rather than a shortcut. arm64 needs
// threadPtr/tlsOffset/tlsIndexAddr on its builder because its arithmetic is
// register-to-register: the thread pointer and the variable's offset occupy two
// registers at once, so the register allocator has to supply them, so the access
// has to become ordinary instructions before allocation runs. x86-64 arithmetic
// takes a memory operand, so both implemented models are "thread pointer into the
// destination, then add the offset to it" -- one register from start to finish,
// which an addressing helper can improvise after allocation.
//
// What would fill this file is general-dynamic, which ends in a call to
// __tls_get_addr and so cannot be improvised: the call clobbers the caller-saved
// registers, and only the allocator can spill across it. That needs the arm64
// shape -- a lowering pass emitting ir.OTLSIndexAddr and ir.OCall, whose selection
// lands here and in xselect_tls.go. It is in turn blocked on x86-64
// general-dynamic relocations in obj/; tls.go's materializeThreadSym records
// exactly what is missing, and asking for the model today is a loud error rather
// than wrong code.
type xasmTLS interface {
}
