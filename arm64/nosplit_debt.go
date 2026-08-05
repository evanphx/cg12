package arm64

// noSplitDebt is the register of nosplit chains that were already over the
// reserve when the budget was introduced.
//
// The budget was written on the premise that cg12's nosplit chains fit and that
// a check would keep them fitting. They do not fit. Running the walk on the tree
// as it stood -- with the inliner's stopgap in place, so nothing at all had been
// inlined into a nosplit caller -- found 23 nosplit functions whose deepest
// chain exceeds the 920-byte reserve, the worst by a factor of two.
//
// Every function on those chains is `//go:nosplit` in the vendored runtime; the
// markings were checked against stdlib/src/runtime rather than assumed. The
// chains exist in upstream Go too, and Go's linker accepts them because gc's
// frames are roughly a third the size of cg12's. runtime.mcache.nextFree, the
// function whose inlined frame produced "fatal error: runtime: split stack
// overflow" in runtime_lock_osthread, is on this list at 1104 bytes *without*
// any inlining: the crash that motivated the budget was one instance of a
// standing condition, not an isolated defect.
//
// So the budget had a choice between rejecting every build of the tree it was
// written for and recording what it found. It records it, here, by name and by
// size, and rejects anything worse:
//
//   - a chain over the reserve whose root is not on this list fails the build;
//   - a chain on this list that grows past its recorded height fails the build;
//   - the heights here are never spendable headroom. stackcheck.Report.Headroom
//     measures against the reserve alone, so a pass that wants to grow a frame
//     -- inlining into a nosplit caller is the one this was built for -- is told
//     it has none of these bytes to spend. Every entry below is frozen at the
//     size it was found at.
//
// That is a ratchet on existing debt, and it is not the same thing as the budget
// passing. These chains can still overflow the reserve at runtime, on the same
// terms nextFree did: a chain overflows when it is entered from a guarded frame
// that only just cleared its own check, which is why the deepest of them have
// not been seen to fire while the allocator path, entered constantly at every
// stack depth, did.
//
// Removing an entry means the chain now fits. Adding one means a new chain does
// not, and should be justified in the same breath.
//
// Heights are in bytes, measured with `goc -O` on linux/arm64 at
// ccwork/nosplit-frame-budget, and are identical across
// goc/testdata/runtime_lock_osthread.go, runtime_gc_concurrent_mark.go,
// stdlib_compress_zlib_lzw.go and stdlib_http_tls_client_server.go -- the whole
// register lives in the runtime, so it does not vary with the program. Regenerate
// with:
//
//	GOC_DEBUG_NOSPLIT=heights GOC_NOSPLIT_LIMIT=100000000 goc -O -o /dev/null prog.go
var noSplitDebt = map[string]int{
	// reflectcall's argument-copy path, through the write barrier and on into
	// the on-demand GC mask allocator.
	".Lruntime_asm_arm64_callRet":        1824,
	"runtime_reflectcallmove_abi0":       1760,
	"runtime_reflectcallmove":            1744,
	"runtime_typedmemmove":               1744,
	"runtime_typedslicecopy":             1744,
	"runtime_memclrHasPointers":          1728,
	"runtime_bulkBarrierPreWrite":        1648,
	"runtime_bulkBarrierPreWriteSrcOnly": 1520,
	"runtime_mspan_typePointersOf":       1056,

	// The execution tracer's status writers, which reach memset through the
	// buffer refill.
	"runtime_traceWriter_writeProcStatusForP": 1744,
	"runtime_traceWriter_writeGoStatus":       1520,
	"runtime_traceWriter_writeProcStatus":     1376,

	// Paths that reach cg12's own write-barrier verification helpers
	// (cg12CheckWriteBarrierPair and below), which upstream Go does not have.
	"runtime_debugCallCheck_func_49_14": 1360,
	"runtime_cgocallbackg":              1216,
	"runtime_cgocall":                   1200,
	"runtime_gorecover_func_1122_14":    1168,
	"runtime_releaseSudog":              1056,
	"runtime_cgoCheckTypedBlock":        960,

	// The allocator fast path, and the fatal-error tail it can reach. This is
	// the chain runtime_lock_osthread died on.
	"runtime_mcache_nextFree": 1104,

	// Signal delivery on a thread with no m, and the two syscall entry wrappers.
	"runtime_sigtrampgo":          1184,
	"syscall_Syscall6":            1120,
	"syscall_Syscall":             1088,
	"runtime_runPerThreadSyscall": 992,
}
