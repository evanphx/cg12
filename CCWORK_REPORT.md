# A nosplit frame budget for cg12

Branch `ccwork/nosplit-frame-budget`, from `main` (`5b085d2`).

_(run in progress — sections appended as each result lands)_

## What the budget is

`stackcheck` (new package) walks the nosplit call graph and computes, for each
function, the most stack a chain entered at it can consume before something
checks the stack again. `arm64/nosplit.go` supplies the facts: frame sizes from
the finished layout, call edges from the lowered IR, and the assembly the module
carries. The walk is deliberately Go's walk
(`cmd/link/internal/ld/stackcheck.go`): heights accumulate bottom-up, a
splittable callee ends a chain, an unresolved callee ends a chain, a cycle is
infinite. It runs in `compileToObjectWithBundle` before a byte of the object is
committed, and it returns an error.

### The limit is 920, not Go's 792

Go's linker uses `abi.StackNosplitBase` (800) minus 8 for AArch64's saved FP:
the familiar `nosplit stack over 792 byte limit`. Go cannot use the other 128
because its compiler lets a splittable function with a frame no larger than
`StackSmall` compare SP against `stackguard0` *without* subtracting its frame
first, spending that part of the reserve.

cg12 has no such shortcut. `arm64.(*mc).goStackPrologue` computes `SP - frame`
and compares that, unconditionally, for every managed non-nosplit frame. So a
cg12 guarded frame really does sit above `stack.lo + stackGuard`, and the whole
guard — `stackNosplit + stackSystem + StackSmall` = 800 + 0 + 128 — is available
below it. Minus the 8 for FP: **920**.

The extra 128 is load-bearing, not slack. See the next section.

## The first thing this found: the tree is already over the reserve

Running the budget on `main` as it stands — with the inliner's stopgap in place,
so nothing has been inlined into a nosplit caller — the goc runtime has **16
nosplit chains over 920 bytes**, deepest **1824**. Measured on
`goc/testdata/runtime_lock_osthread.go`, `goc -O`:

| height | chain |
|---:|---|
| 1824 | `callRet -> reflectcallmove -> bulkBarrierPreWrite -> typePointersOf -> typePointersOfUnchecked -> getGCMask -> getGCMaskOnDemand -> persistentalloc -> systemstack` |
| 1744 | `typedmemmove -> bulkBarrierPreWrite -> ...` (same tail) |
| 1744 | `typedslicecopy -> bulkBarrierPreWrite -> ...` |
| 1744 | `traceWriter.writeProcStatusForP -> writeProcStatus -> event -> ensure -> refill -> traceBuf.varint -> goc_memset` |
| 1728 | `memclrHasPointers -> bulkBarrierPreWrite -> ...` |
| 1520 | `bulkBarrierPreWriteSrcOnly -> ...`, `traceWriter.writeGoStatus -> ...` |
| 1360 | `debugCallCheck.func -> goc_storep -> atomicstorep -> atomicwb -> cg12CheckWriteBarrierPair -> cg12WriteBarrierWordIsRejected -> cg12WriteBarrierValueIsBad -> spanOf -> arenaIndex` |
| 1216 | `cgocallbackg -> reentersyscall -> goc_storep -> ...` |
| 1200 | `cgocall -> entersyscall -> reentersyscall -> goc_storep -> ...` |
| 1184 | `sigtrampgo -> badsignal -> dropm -> traceAcquire -> traceAcquireEnabled -> atomic.Bool.Store -> atomic.Uint8.Store -> atomic.Store8` |
| 1168 | `gorecover.func -> goc_storep -> ...` |
| **1104** | **`mcache.nextFree -> mcache.refill -> consistentHeapStats.acquire -> throw -> fatalthrow -> systemstack`** |
| 1056 | `releaseSudog -> goc_storep -> ...` |
| 992 | `runPerThreadSyscall -> fatal -> fatalthrow -> systemstack` |
| 960 | `cgoCheckTypedBlock -> cgoCheckBits -> isPinned -> goc_storep -> ...` |

Every function on these chains is `//go:nosplit` in the vendored runtime — the
markings were checked against `stdlib/src/runtime`, not assumed. The chains are
real in upstream Go too; Go's linker accepts them because gc's frames are
roughly a third the size. `runtime.mcache.nextFree`'s frame is **368 bytes**
here, which is exactly the number the stopgap's commit message records, so the
frame extraction is measuring the thing it claims to measure.

Two consequences worth stating plainly:

1. **The `runtime_lock_osthread` crash was one instance of a class, not an
   isolated defect.** The chain that killed it (`nextFree -> refill`, 976 bytes
   after inlining) is the twelfth-deepest of sixteen chains that are over the
   reserve *without* any inlining. It fired because the allocator path is
   entered constantly at every stack depth; the other fifteen have not fired yet.
2. **A budget calibrated at Go's 792 would reject this tree outright** — that is
   why the 920 derivation above matters, and it still is not enough.

The register grew when it met the configurations the capability matrix builds.
Heights are stable across *programs* — `runtime_lock_osthread`,
`runtime_gc_concurrent_mark`, `stdlib_compress_zlib_lzw` and
`stdlib_http_tls_client_server` agree to the byte — but not across
*configurations*:

| configuration | chains over 920 | deepest |
|---|---:|---:|
| `goc -O`, whole program | 21–23 | 1824 |
| `goc build-runtime -O` (7 pack roots) | 22–24 | 1824 |
| `goc` (no `-O`), whole program | 48–50 | 3024 |
| `goc build-runtime` (no `-O`, 7 pack roots) | 48–50 | 3024 |

**An unoptimized `goc` build is more than three times over the reserve.** Without
mem2reg every local keeps its frame slot, so every frame on every chain roughly
doubles: `runtime.bulkBarrierPreWrite` is 592 bytes with `-O` and 752 without,
and `runtime.doubleCheckTypePointersOfType` — which whole-program `-O` builds
drop as dead — is 1408 bytes on its own. That arm is what the default capability
matrix runs, and it is the worst case `arm64/nosplit_debt.go` records: the
register is the maximum over twenty configurations (seven pack roots × `-O` /
no-`-O`, plus six whole-program builds), 50 entries.

## What the budget rejects

- A chain over the reserve whose root is not in the register.
- A registered chain that grows past its recorded height.
- A cycle among nosplit functions — infinite height, reported as such.
- A nosplit frame with a dynamic stack allocation — unbounded, reported as such.

There is no warn mode and no truncation. `arm64.checkNoSplitBudget` returns an
error from `compileToObjectWithBundle` before any object bytes are produced, and
`TestNoSplitBudgetProducesNoObject` holds it to that.

The cases a naive walk gets wrong, and what this one does:

| case | treatment |
|---|---|
| recursion / mutual recursion among nosplit functions | height is `stackcheck.Infinite`; reported as `unbounded nosplit stack ... (cycle)` and rejected. A function *below* a cycle keeps its own height — the sentinel is not memoized through the back edge. |
| a cycle broken by a splittable function | not a cycle: the chain restarts at the guarded frame. |
| call through a function pointer / interface / closure | Go's rule by default (`cmd/link` assumes the target checks its own stack), and the count of such sites plus the set of **address-taken nosplit functions** is reported, so the assumption is visible rather than hidden. `GOC_NOSPLIT_INDIRECT=strict` resolves the edge against that set instead; it is an audit mode, not a gate — on goc's runtime, 53 nosplit functions call indirectly and 149 are address-taken, and the closure of that relation closes cycles no execution takes (`systemstack` → its argument → `atomicwb` → `systemstack`), so every chain measures infinite. |
| assembly | frames come from the `TEXT` directive and call edges from the `BL`/`CALL`/`B` operands, which `plan9asm` now records per function (`ARM64Function.Calls`). Assembly is not assumed to be a leaf: `runtime.systemstack`, `runtime.reflectcallmove` and the generated ABI0 wrappers all appear inside measured chains. |
| a nosplit function calling a splittable one | the chain ends there, which is how chains are meant to end. |
| a translated assembly function that is *not* `NOSPLIT` | ends the chain, and is counted in `Report.Unchecked` — 68 of them. This is a real hole: cg12's Plan 9 translator emits a bare `sub sp` prologue where Go's assembler inserts a split check, so those functions end a chain on a promise the toolchain does not keep. Treating them as nosplit instead is not the answer — `runtime·call1073741824` is a `WRAPPER` with a 1 GiB frame — but the gap is cg12's, it predates this branch, and it should be closed. |
| an undefined callee | ends the chain, on the same assumption Go's linker makes for a symbol outside the link, and is listed in `Report.External` (4 of them). |
