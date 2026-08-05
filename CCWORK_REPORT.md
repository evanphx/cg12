# A nosplit frame budget for cg12

Branch `ccwork/nosplit-frame-budget`, from `main` (`5b085d2`).

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

## The restriction, lifted

`opt.InlineIntoNoSplitCallers` is the last pass in `DefaultPipeline`. For each
nosplit caller it:

1. asks the backend for that caller's headroom — how many bytes its frame can
   grow before the deepest chain running through it reaches 920, counting the
   frames *above* it as well as below;
2. takes a snapshot (`ir.CloneFunc`, through the binary unit format);
3. runs the ordinary inliner into it;
4. runs `opt.Optimize` on the result, so the measurement sees the code the
   backend will see rather than the code the splice left behind;
5. lays the frame out through the backend (`arm64.measureFunction` — the same
   `computeFrame` the emitter calls, from the same register allocation) and
   compares;
6. restores the snapshot verbatim if the frame grew past the allowance.

Nothing is estimated. The 16-byte `noSplitInlineSafetyBytes` held back from each
allowance is one AArch64 slot pair, not a fudge factor for a guess.

Cost: the backend measures only the nosplit functions (a splittable frame never
enters a chain, so it goes into the graph as a name with no frame), which is
about 600 of 15000 in a runtime module. Measured on
`runtime_lock_osthread.go`: **18.8 s → 21.8 s**, +16% wall on a small program.

### What it bought

On `goc -O goc/testdata/runtime_lock_osthread.go` (`GOC_DEBUG_NOSPLIT=inline`):

| outcome | callers |
|---|---:|
| inlined, frame measured, kept | **106** |
| inlined, frame measured, **reverted** | 1 |
| no headroom at all | 201 |

The one revert is `runtime.debugCallWrap1.func`: measured +96 bytes against an
88-byte allowance, restored from the snapshot. That single line is the
difference between a bound and a guess.

Most accepted callers got *smaller*, not larger — inlining removed the call and
with it the caller's outgoing-argument area:
`internal/runtime/atomic.Pointer.Load` 32 → 16, `Pointer.StoreNoWB` 48 → 16,
`runtime.addGSyscallNoP` 48 → 16. The ones that grew stayed inside their
measured allowance: `runtime.initsig` 80 → 208 of 424 allowed,
`runtime.notetsleep_internal` 80 → 176 of 312, `runtime.sysMemStat_add`
288 → 304 of 24 — that last one exactly at its limit and accepted for it.

### What it did not buy, and this is the finding

**The allocator fast paths get nothing, and the budget is right to refuse them.**

    runtime.mcache.nextFree      headroom -184   (chain 1104 of 920)
    runtime.mcache.refill        headroom -184
    runtime.acquirem             headroom  <0
    runtime.releasem             headroom  <0

`nextFree`'s nosplit chain is 1104 bytes with *nothing* inlined into it. The
crash this whole exercise started from — `nextFree`'s frame going 384 → 656 and
the run reaching 976 — was not the inliner creating an overflowing chain. It was
the inliner adding 272 bytes to a chain that was already 184 bytes over. The
stopgap fixed the symptom and hid the condition.

The part of the allocator path that does have room got its inlining back:
`runtime.nextFreeFast` (headroom 888), `runtime.mspan.nextFreeIndex` (168, frame
128 → 112), `runtime.spanQueue.refill` (536, frame 128 → 96).

Getting `nextFree` back means bringing its chain under 920 first, which is a
codegen-size problem (`nextFree` 368 + `refill` 352 + `consistentHeapStats.acquire`
144 + `throw` 128 + `fatalthrow` 96 + `systemstack` 16), not an inliner problem.
Upstream Go does not have this chain at all: gc inlines `nextFree` *into*
`mallocgc`, which is splittable, so the frame is spent under a guard. That —
inlining nextFree into its caller rather than inlining into nextFree — is the
change that would actually recover the motivating case.

## Guards

Every one of these ran on this branch's tree, watched to completion.

| guard | required | result |
|---|---|---|
| capability matrix, default arm | 368/368 | **368 subtests PASS**, `make test-goc-status` `ok` |
| capability matrix, `-O` arm | 368/368 | **368 subtests PASS**, `make test-goc-status-opt` `ok` |
| `runtime_lock_osthread` crash loop | ≥400 runs, 0 crashes | **400 runs, 0 non-zero exits** |
| GC reducer (`runtime_gc_promoted_local_root`) | 0/20 at `GOGC=10` | **20 runs, 0 failures** |
| GC reducer | 0/20 at default `GOGC` | **20 runs, 0 failures** |
| flate (`placement_bench/flate`) | 0/250 | **250 runs, 0 failures** |
| the four corpus audits | pass | `TestAllocationCensus` `TestEscapeShadowPlacement` `TestFrameEscapeAudit` `TestLoopAliasAudit` — **all PASS** |
| `TestParallelBackendIsByteIdenticalToSerial` | pass | **PASS** |
| determinism, byte-identical | pass | see below |

Determinism was checked at whole-artifact level, twice over and against a serial
backend, since the new pass runs a second frame layout and could have introduced
an order dependence:

    a4a749c0…  runtime pack, run 1
    a4a749c0…  runtime pack, run 2
    a4a749c0…  runtime pack, GOC_BACKEND_WORKERS=1
    2d93d9b4…  runtime_lock_osthread executable, run 1
    2d93d9b4…  runtime_lock_osthread executable, run 2
    2d93d9b4…  runtime_lock_osthread executable, GOC_BACKEND_WORKERS=1

Unit tests on every package this branch touches: `opt`, `ir`, `stackcheck`,
`plan9asm`, `plan9asm/sem`, `arm64`, `analysis`, `link`, `obj` — all `ok`.
(`go test ./goc/...` and `make test-unit` were left to the gate job, as
instructed; `go test ./goc -run TestNoSplitBudget` was run and passes.)

## `make bench-perf`, before and after

Both arms were run on this box. The first attempt of each was worthless — the
other two ccwork jobs on the machine had it at load average 188 on 64 cores, and
the suite's own null arm (goc timed against goc) spread by up to 104%. Both arms
were then re-run in a quiet window (load average under 4), which is what is
reported here.

**The control is unchanged.** `control/spin-fixed-work` is the fixed integer
loop compiled by both compilers and appears in all eleven programs, so it is
eleven independent readings of the same quantity:

| tree | control mean | s.d. | committed control |
|---|---:|---:|---:|
| `main` 5b085d2 | **0.9256** | 0.0041 | 0.9260 |
| branch | **0.9244** | 0.0075 | 0.9260 |

Across all 42 rows, `main` → branch: **median +0.32%, mean +0.73%**. Thirty-nine
of the 42 move by less than 8%.

### Both trees fail `make bench-perf`, and neither failure is a ratio

`main` at 5b085d2 fails it too, on a quiet box, for the same reason: rows whose
own one-repetition spread exceeds the suite's 15% ceiling, which is the suite
refusing to gate a row it cannot measure. No row on either tree failed a
tolerance band against the committed baseline.

| tree | rows over the noise ceiling |
|---|---|
| `main` | `chase/l1-resident` (24.0%, null 10.2%), `chase/pointer-node` (19.5%, null 31.1%), `gc/pointer-write` (26.9%, null 2.3%) |
| branch | `chase/l1-resident` (32.2%, null 31.1%), `regexp/replace` (17.5%, null 1.4%), `gc/live-heap-churn` (17.0%, null 3.5%), `gc/pointer-write` (25.8%, null 4.3%) |

So the honest statement is: **`make bench-perf` did not pass on this branch, and
it does not pass on `main` either.** It is not a gate this box can currently
satisfy. What it does say, through its ratios, is that goc's speed relative to
the host toolchain is where it was.

### The one row that looked like a regression, re-measured

The second branch run put `gcpress gc/pointer-write` at 11.72 against `main`'s
8.54, a 37% move — and it is a write-barrier workload, which is exactly where
this branch inlined into nosplit functions
(`internal/runtime/atomic.storePointer` 48 → 80 bytes,
`runtime.maybeTraceablePtr.set` 64 → 112). So it was re-measured rather than
explained away. A third branch run, same quiet box, puts it at **7.79 — below
`main`**. The row's own spread is 26–27% on both trees; it is the row the suite
refuses to gate.

| row | `main` | branch run 2 | branch run 3 |
|---|---:|---:|---:|
| `gc/pointer-write` | 8.5427 | 11.7236 | **7.7878** |
| `gc/live-heap-churn` | 4.5740 | 4.7328 | 4.5260 |
| `chase/l1-resident` | 1.1382 | 1.0813 | 0.9755 |

Run 3's own outliers are the four `sortmap` rows, *including its control row*
(0.9273 → 1.0180), which nothing in this branch can affect — a transient on the
box during that block, and all four failed the noise ceiling. Across three quiet
runs the control mean is 0.9256 (`main`), 0.9244 and 0.9325 (branch), against
the committed 0.9260, and the median row change from `main` is +0.32% and
+0.22%.

**No measurable performance change.** That is the expected result: the inlining
this branch re-enabled is inside nosplit runtime functions, and the deep
allocator paths — the ones on these workloads' hot paths — got none of it,
because their chains are over the reserve.

## The error, in full

`goc -O` on a three-link nosplit chain of 512-byte frames
(`goc/nosplitbudget_test.go` holds this shape as a test):

    goc: nosplit frame budget: nosplit stack overflow: main_middle -> main_deepest -> goc_memset
      1168 bytes of nosplit frames against a 920-byte limit, 248 over
           576  main_middle
           576  main_deepest
            16  goc_memset
      shrink a frame on this chain, or let one of these functions keep its stack-growth check

Exit status 1, no object written. The chain, each frame in it, the total, the
limit, how far over, and the two things a person can do about it.

## Verdict

**The budget fires on a constructed overflow** — `goc` exits 1 with the message
above, and `TestNoSplitBudgetRejectsAnOverflowingChain` /
`TestNoSplitBudgetProducesNoObject` hold it to rejecting rather than warning.
It also fired on the tree it was written for, which was not expected: 23 chains
over the reserve under `-O` and 50 without it, the deepest at 3024 bytes against
920, with `runtime.mcache.nextFree` — the function that crashed
`runtime_lock_osthread` — already 184 bytes over before anything is inlined into
it.

**Inlining into nosplit callers is re-enabled**, bounded by measured frames
rather than by a proxy: `opt.InlineIntoNoSplitCallers` inlines, cleans up,
measures the frame the backend will lay out, and restores the caller verbatim if
it grew past its chain's headroom.

**What it bought**: 106 nosplit callers got their inlining back on
`runtime_lock_osthread`, one was measured and reverted, and 201 have no headroom
at all. The allocator fast paths are in the third group and stay there — which
is the right answer and the reason the stopgap looked like it worked. On the
performance suite the change is not measurable: control 0.9244/0.9325 against
`main`'s 0.9256 and the committed 0.9260, median row change +0.2 to +0.3%.


## What this leaves behind

Three things this branch found and did not fix, in the order they matter:

1. **cg12's runtime nosplit chains are over the reserve, by up to 3.3×.** Fifty
   of them, recorded in `arm64/nosplit_debt.go`. They are latent stack
   overflows on the same terms `nextFree` was: a chain overflows when it is
   entered from a guarded frame that only just cleared its own check. The
   register makes the debt visible and stops it growing; it does not repay it.
   The largest single contributors are `runtime.doubleCheckTypePointersOfType`
   (1408 bytes), `runtime.bulkBarrierPreWrite` (592 with `-O`, 752 without) and
   `runtime.mspan.typePointersOfUnchecked` (416/592).
2. **cg12's Plan 9 assembly translator emits no stack-growth check at all**, for
   any translated function, where Go's assembler inserts one for every non-NOSPLIT
   `TEXT`. Sixty-eight functions end a nosplit chain here on a promise the
   toolchain does not keep; the budget counts them (`Report.Unchecked`) rather
   than pretending. `runtime·call1073741824` is a `WRAPPER` with a 1 GiB frame
   and no check.
3. **The motivating case needs the other inlining.** Getting the allocator fast
   path its inlining back means inlining `nextFree` *into* `mallocgc`, which is
   splittable — which is what gc does, and why upstream Go has no such chain —
   rather than inlining into `nextFree`. cg12 marks `nextFree`/`refill`/
   `nextFreeFast`/`nextFreeIndex` nosplit precisely because it cannot do that
   (`goc.runtimeImplicitNoSplit`), and that decision is what creates the 864
   bytes of chain before `throw` is even reached.

## Scope

arm64 only, which is where the Go runtime is. The walk in `stackcheck` is
arch-neutral — it takes the limit and the call-push size as configuration — but
only `arm64` supplies it facts. amd64 does not need it yet: a Go-runtime module
does not reach that backend at all (`goc -target amd64` on a Go program fails in
the frontend, on `main` and on this branch alike), and a platform-ABI module has
no stack-growth check for a function to be exempt from, so
`newNoSplitFrameBudget` returns no budget and the pass and the check are both
no-ops. `cmd/cc` and `cmd/cg12` are unaffected, measured, not assumed.

Branch: **`ccwork/nosplit-frame-budget`**, from `main` (`5b085d2`).
