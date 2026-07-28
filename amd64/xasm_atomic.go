package amd64

// xasmAtomic is the atomic and synchronization surface: the atomic load/store
// pair, exchange, compare-and-swap, the locked read-modify-write arithmetic and
// bitwise forms, and the fences. `grep -c atomic amd64/*.go` is 0 today, which is
// also why nothing in Go's runtime can run on this backend yet.
//
// Empty until Phase 2 Track A, agent A1 of AMD64_PARITY_PLAN.md fills it in;
// A1 also owns xselect_atomic.go and the memory-destination ALU forms in x64 that
// every locked RMW needs. It is deliberately its own file so that adding the
// family is a write here plus one line in xasm.go, and nothing else.
//
// x86-64 makes this simpler than arm64 rather than harder: each operation is a
// single LOCK-prefixed instruction, so there is no LL/SC retry loop to build, and
// RAX -- which CMPXCHG fixes as the comparand -- is already held out of
// allocation.
type xasmAtomic interface {
}
