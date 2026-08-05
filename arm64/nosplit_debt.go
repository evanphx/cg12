package arm64

// noSplitDebt is the register of nosplit chains that were already over the
// reserve when the budget was introduced.
//
// The budget was written on the premise that cg12's nosplit chains fit and that
// a check would keep them fitting. They do not fit. Running the walk on the tree
// as it stood -- with the inliner's stopgap in place, so nothing at all had been
// inlined into a nosplit caller -- found the 50 chains below over the 920-byte
// reserve, the deepest at 3024 bytes, more than three times the reserve.
//
// Every function on those chains is `//go:nosplit` in the vendored runtime; the
// markings were checked against stdlib/src/runtime rather than assumed. The
// chains exist in upstream Go too, and Go's linker accepts them because gc's
// frames are a fraction of cg12's. runtime.mcache.nextFree, the function whose
// inlined frame produced "fatal error: runtime: split stack overflow" in
// runtime_lock_osthread, is on this list at 1216 bytes *without* any inlining:
// the crash that motivated the budget was one instance of a standing condition,
// not an isolated defect.
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
// that only just cleared its own check, which is why the deepest of them have not
// been seen to fire while the allocator path, entered constantly at every stack
// depth, did.
//
// Removing an entry means the chain now fits. Adding one means a new chain does
// not, and should be justified in the same breath.
//
// Heights are in bytes, measured on linux/arm64 at ccwork/nosplit-frame-budget,
// as the maximum over twenty configurations: `goc build-runtime` for each of the
// seven capability-matrix pack roots with and without -O, and whole-program
// builds of runtime_lock_osthread, runtime_gc_concurrent_mark,
// stdlib_http_tls_client_server and stdlib_compress_zlib_lzw, again with and
// without -O.
//
// The configuration matters more than the program does. Every optimized build
// agrees on 22-24 over-limit chains; every unoptimized one has 48-50, because
// without mem2reg each local keeps a frame slot. A `goc` build with no -O is the
// worst case here and is most of what this register describes.
//
// Regenerate an entry with:
//
//	GOC_DEBUG_NOSPLIT=heights GOC_NOSPLIT_LIMIT=100000000 goc -o /dev/null prog.go
var noSplitDebt = map[string]int{
	"runtime_doubleCheckTypePointersOfType":   3024,
	".Lruntime_asm_arm64_callRet":             2608,
	"runtime_reflectcallmove_abi0":            2544,
	"runtime_reflectcallmove":                 2528,
	"runtime_typedslicecopy":                  2512,
	"runtime_typedmemmove":                    2496,
	"runtime_typedmemclr":                     2480,
	"runtime_memclrHasPointers":               2464,
	"runtime_bulkBarrierPreWrite":             2368,
	"runtime_bulkBarrierPreWriteSrcOnly":      2176,
	"runtime_traceWriter_writeProcStatusForP": 1904,
	"runtime_traceWriter_writeGoStatus":       1648,
	"runtime_mspan_typePointersOf":            1616,
	"runtime_debugCallCheck_func_49_14":       1536,
	"runtime_traceWriter_writeProcStatus":     1488,
	"runtime_gorecover_func_1122_14":          1456,
	"runtime_cgocallbackg":                    1424,
	"runtime_cgocall":                         1408,
	"runtime_sigtrampgo":                      1408,
	"runtime_releaseSudog":                    1360,
	"runtime_cgoCheckSliceCopy":               1344,
	"runtime_cgoCheckMemmove2":                1328,
	"syscall_Syscall6":                        1264,
	"runtime_mspan_typePointersOfUnchecked":   1248,
	"syscall_Syscall":                         1248,
	"runtime_mcache_nextFree":                 1216,
	"runtime_cgoCheckTypedBlock":              1184,
	"runtime_acquireSudog":                    1120,
	"runtime_cgoCheckBits":                    1072,
	"runtime_debugCallWrap1_func_222_8":       1072,
	"runtime_sigfwdgo":                        1072,
	"runtime_badsignal":                       1056,
	"runtime_entersyscall":                    1056,
	"runtime_fatalpanic":                      1056,
	"runtime_adjustSignalStack2":              1040,
	"runtime_entersyscallblock":               1040,
	"runtime_fatalthrow_func_1452_14":         1024,
	"runtime_gcBgMarkWorker_func_1847_15":     1024,
	"runtime_runPerThreadSyscall":             1008,
	"runtime_reentersyscall":                  992,
	"runtime_traceAdvance_func_508_15":        992,
	"runtime_cgoCheckPtrWrite":                976,
	"runtime_traceWriter_event":               976,
	"runtime__panic_nextFrame_func_1003_14":   960,
	"runtime_typeBitsBulkBarrier":             960,
	"runtime_dropm":                           944,
	"runtime_libpreinit":                      944,
	"runtime_adjustSignalStack":               928,
	"runtime_crash":                           928,
	"runtime_debugCallWrap_func_164_8":        928,
}
