# Profiling goc's compile, and the cheap wins

# A nosplit frame budget for cg12

# Optimiser pipeline research — fixpoint vs. ordered

Branch `ccwork/nosplit-frame-budget`, from `main` (`5b085d2`).

Branch `ccwork/compile-time-profiling`, off `main` (5b085d2).

## What the budget is

Branch `ccwork/optimiser-pipeline-research`, cut from `main` (5b085d2).
Deliverable: `OPTIMISER_PIPELINE.md`. No compiler behaviour changed in the
committed tree; measurement scaffolding was applied as an uncommitted patch and
removed (see "Scaffolding" at the end).

`stackcheck` (new package) walks the nosplit call graph and computes, for each
function, the most stack a chain entered at it can consume before something
checks the stack again. `arm64/nosplit.go` supplies the facts: frame sizes from
the finished layout, call edges from the lowered IR, and the assembly the module
carries. The walk is deliberately Go's walk
(`cmd/link/internal/ld/stackcheck.go`): heights accumulate bottom-up, a
splittable callee ends a chain, an unresolved callee ends a chain, a cycle is
infinite. It runs in `compileToObjectWithBundle` before a byte of the object is
committed, and it returns an error.

Host: aarch64 Linux, 64 cores, 250 GiB RAM, go1.26.1. All compiles are
`goc -O` whole-program builds (the module carries the stdlib closure).

### The limit is 920, not Go's 792

_(run in progress — sections appended as each result lands)_

Go's linker uses `abi.StackNosplitBase` (800) minus 8 for AArch64's saved FP:
the familiar `nosplit stack over 792 byte limit`. Go cannot use the other 128
because its compiler lets a splittable function with a frame no larger than
`StackSmall` compare SP against `stackguard0` *without* subtracting its frame
first, spending that part of the reserve.

Two programs are profiled throughout:

cg12 has no such shortcut. `arm64.(*mc).goStackPrologue` computes `SP - frame`
and compares that, unconditionally, for every managed non-nosplit frame. So a
cg12 guarded frame really does sit above `stack.lo + stackGuard`, and the whole
guard — `stackNosplit + stackSystem + StackSmall` = 800 + 0 + 128 — is available
below it. Minus the 8 for FP: **920**.

## 1. What the three compilers actually do — read, not recalled

The extra 128 is load-bearing, not slack. See the next section.

- **small** — `goc/testdata/fmt_sprintf.go`, 10 lines, 5101 functions after the
  stdlib closure. This is the program the wave-9 gate measured at 6.24x wall.
- **http** — `goc/testdata/stdlib_http_tls_client_server.go`, the corpus's worst
  case: 4.23 GiB peak RSS at `GOMAXPROCS=1`, 380.5 CPU-seconds.

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

## 1. What the profiler says. CPU, small program

| configuration | chains over 920 | deepest |
|---|---:|---:|
| `goc -O`, whole program | 21–23 | 1824 |
| `goc build-runtime -O` (7 pack roots) | 22–24 | 1824 |
| `goc` (no `-O`), whole program | 48–50 | 3024 |
| `goc build-runtime` (no `-O`, 7 pack roots) | 48–50 | 3024 |

`goc -O -cpuprofile ... -o out goc/testdata/fmt_sprintf.go`, default GOMAXPROCS.

**An unoptimized `goc` build is more than three times over the reserve.** Without
mem2reg every local keeps its frame slot, so every frame on every chain roughly
doubles: `runtime.bulkBarrierPreWrite` is 592 bytes with `-O` and 752 without,
and `runtime.doubleCheckTypePointersOfType` — which whole-program `-O` builds
drop as dead — is 1408 bytes on its own. That arm is what the default capability
matrix runs, and it is the worst case `arm64/nosplit_debt.go` records: the
register is the maximum over twenty configurations (seven pack roots × `-O` /
no-`-O`, plus six whole-program builds), 50 entries.

    Duration: 43.93s, Total samples = 78.69s (179.11%)

## What the budget rejects

The 179% is the first finding: **the compile is not CPU-bound on the compiler.**
It is 44 s of wall clock costing 79 s of CPU, and the difference is the garbage
collector running on other cores.

- A chain over the reserve whose root is not in the register.
- A registered chain that grows past its recorded height.
- A cycle among nosplit functions — infinite height, reported as such.
- A nosplit frame with a dynamic stack allocation — unbounded, reported as such.

Top by cumulative cost, as fractions of the 78.69 s of samples:

There is no warn mode and no truncation. `arm64.checkNoSplitBudget` returns an
error from `compileToObjectWithBundle` before any object bytes are produced, and
`TestNoSplitBudgetProducesNoObject` holds it to that.

| | cum | what it is |
|---|---:|---|
| `main.main` | **52.78%** | everything the compile does on the main goroutine |
| ├ `opt.OptimizeModule` | **46.60%** | the optimiser |
| ├ `arm64.compileFunction` | 12.99% | the backend (runs on other goroutines, hence the overlap) |
| └ `goc.compile` (front end) | 5.25% | parse + type-check + IR generation |
| `runtime.gcBgMarkWorker` | **30.69%** | **the garbage collector, all of it concurrent** |

The cases a naive walk gets wrong, and what this one does:

Inside the optimiser, by cumulative share of total samples:

| pass / analysis | cum |

All three are **fixed ordered pass lists**. None re-converges a whole pipeline.
Each contains bounded, *local* iteration, in a different place.

### LLVM — fixed sequence, one bounded repeat, plus per-function memoisation

Evidence is the installed LLVM 18.1.3's own pipeline printer, so it is this
box's actual `-O2`, not a description of it:

    opt-18 -passes='default<O2>' -print-pipeline-passes -disable-output empty.ll

One comma-separated sequence. Pass-name frequencies in it:

| pass | times it appears in `-O2` |

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
| **`analysis.BuildCFG`** | **11.25%** |
| `opt.GVN` | 7.59% |
| `opt.LoadElim` | 7.45% |
| `opt.newAliasInfo` (called by LoadElim + DeadAlloc) | 6.70% |
| `opt.SimplifyCFG` | 6.30% |
| `opt.DCE` | 5.88% |
| `opt.jumpThread` | 5.43% |
| `opt.Fold` | 4.33% |
| `opt.DeadAlloc` | 3.84% |

| `instcombine` | 8 |
| `simplifycfg` | 8 |
| `sroa` | 3 |
| `jump-threading` | 2 |

and the runtime services those passes call:

That is the black-magic pipeline in its purest form: the cleanup passes are
*written down* at eight chosen points rather than looped over.

| | cum |
|---|---:|
| `runtime.mallocgc` | 9.45% |
| `maps.(*table).grow` / `rehash` | 6.88% |
| `runtime.mapassign_fast64ptr` | 8.49% |

- **No `repeat<N>` anywhere.** LLVM *has* a `repeat<N>` pipeline adaptor —
  `opt-18 -passes='repeat<3>(instcombine)'` parses and runs — and the default
  pipelines do not use it: `grep -c 'repeat<'` over the printed `-O1`, `-O2` and
  `-O3` pipelines is **0, 0, 0**.
- **The one iteration is `cgscc(devirt<4>(inline,...))`** — `DevirtSCCRepeatedPass`,
  which repeats the CGSCC body only when a pass turned an indirect call into a
  direct one, at most 4 times (`devirt<4>` is printed at both `-O2` and `-O3`).
  Its doc comment in `llvm/Analysis/CGSCCPassManager.h` states the reason for the
  bound outright: *"This repetition has the potential to be very large however, as
  each one might refine a single call site. As a consequence, in practice we use
  an upper bound on the number of repetitions to limit things."*
- **Re-visits are memoised per function.** The CGSCC walk can revisit an SCC as
  the call graph changes, so the function simplification pipeline nested inside it
  is added with `/*NoRerun=*/true` (`PassBuilderPipelines.cpp`, in
  `buildInlinerPipeline`), backed by `ShouldNotRunFunctionPassesAnalysis`, whose
  comment is: *"This is used to prevent running an expensive function pass
  (manager) on a function multiple times if SCC mutations cause a function to be
  visited multiple times and the function is not modified by other SCC passes."*
  In the printed pipeline this is the `function<eager-inv;no-rerun>(...)` wrapper.
- `instcombine<max-iterations=1;...>` — even InstCombine, the classic worklist
  peephole, is *capped*, and at `-O2` runs one iteration per invocation.

`BuildCFG` is the single largest identifiable cost in the compiler, ahead of
every actual optimisation pass, and it is called by ten different places:
`SimplifyCFG` (37.9% of BuildCFG's time — it builds three CFGs per call),
`jumpThread` (26.3%), `GVN` (15.8%), `LoadElim` (14.5%), and the rest.

So LLVM's answer is: fixed order; iterate only where a *specific* interprocedural
event (devirtualisation) can unlock more; bound that iteration at 4; and never
re-run a function's simplification if the function has not changed.

## 2. What the profiler says. Heap, small program

### GCC — a straight list, repetition written out by hand

Same compile, `-memprofile`. **11.76 GB allocated** in total to compile a
ten-line program, against a **382 MB** heap retained at the end.

Cumulative allocation (`alloc_space`), top sites:

| site | share of 11.76 GB |
|---|---:|
| **`analysis.BuildCFG`** | **33.55%** |
| ` ├ computeRPO` (the `visited`/`num` maps, `post`, the closure) | 32.47% |
| ` └ fillPreds` | 0.05% |
| `opt.newAliasInfo` | 9.22% |
| `opt.useCounts.func1` | 6.77% |
| `opt.defMap` | 5.79% |
| `opt.GVN.func1` | 4.98% |
| `opt.availMem.record` + `.clone` | 6.58% |
| `analysis.(*CFG).Dominators` | 3.85% |
| `opt.domChildren` | 2.53% |
| `analysis.(*CFG).Succs` | 2.34% |

**One third of every byte the compiler allocates is spent rebuilding control-flow
graphs**, and the GC that then has to collect them is 30.7% of the CPU. These are
the same finding seen from two sides.

Retained at the end (`inuse_space`, after a forced GC) is 382 MB and is almost
entirely the IR itself — `ir.(*Func).newTemp` 17.3%, `opt.scheduleBlock` 15.7%,
`ir.(*Func).NewBlock` 11.9%, `opt.spliceCall` (the inliner's clones) 8.5%,
`ir.(*Block).emit` 5.8%. Nothing is holding a second copy of the module.

## 3. What the profiler says. The `clean` fixpoint's real shape

The wave-9 gate's finding — skipping the inliner removes 85% of the cost, and
mem2reg's 28% is entirely indirect — reproduces here, and the profile explains
the mechanism. `opt.OptimizeModule` is 46.6% of small's CPU and 54.7% of http's,
and `opt.fixpoint.Run` is essentially all of it (52.91% of http's total).

Counting the visits directly (temporary instrumentation in `funcPass.Run` and
`fixpoint.Run`, small program):

    clean            13 calls, 50 rounds
    inline-fixpoint   2 calls, 10 rounds

| pass | visits | of which changed anything | |
|---|---:|---:|---:|
| fold | 252,550 | 5,384 | **2.13%** |
| simplifycfg | 257,601 | 9,858 | 3.83% |
| dce | 261,700 | 4,467 | 1.71% |
| copy | 252,550 | 3,814 | 1.51% |
| jumpthread | 252,550 | 3,737 | 1.48% |
| gvn | 252,550 | 3,060 | 1.21% |
| loadelim | 252,550 | 2,478 | 0.98% |
| deadalloc | 252,550 | **417** | **0.17%** |

`gcc/passes.def` (GCC 13 branch, fetched): 540 lines, **360 `NEXT_PASS` entries,
no loop of any kind**. Repetition is expressed by writing the pass again;
`gen-pass-instances.awk` numbers the copies, which is why the local
`gcc -O2 -fdump-passes` shows `tree-ccp1 … tree-ccp5`.

252,550 = 50 rounds × 5051 functions. **Between 96.2% and 99.8% of every pass's
work was re-proving a fixpoint it had already reached.** That is the cost the
inliner buys: not that inlining is expensive, but that each round of it sends
seven cleanup passes back over the whole module to find the few hundred functions
it touched.

From this box's `gcc (Ubuntu 13.3.0) -O2 -fdump-passes` (265 passes enabled):

---

## 4. What changed

Two fixes, plus the heap-profiling flag. Commit `d583aec`.

### 4a. `-memprofile` / `-memprofile-peak` (`cmd/goc/profile.go`)

`-cpuprofile` existed; heap profiling did not. `-memprofile file` writes a heap
profile of the compile (not the link, not the compiled program) after a forced
GC, so `inuse_space` is what the compile *retains* and `alloc_space` is its whole
allocation history. `-memprofile-peak` instead samples `runtime.ReadMemStats`
every 50 ms and rewrites the profile whenever the heap reaches a materially new
high, so `inuse_space` is what was resident at the high-water mark. The two
answer different questions and the second is the one peak RSS follows.

### 4b. `analysis.BuildCFG` stops allocating its way through the module

- the recursive DFS closure (which escaped to the heap on every call) is now an
  explicit stack;
- the separate `visited map[*ir.Block]bool` is gone — `c.num` doubles as it,
  holding a sentinel until the reverse pass overwrites it with the real index;
- `c.num` is presized to `len(f.Blocks)` instead of rehashing on the way up;
- the successor walk reads through new `ir.Block.SuccCount` / `SuccAt` instead of
  `Succs()`, which allocated a fresh one- or two-element slice per block per call
  for a jump or a conditional branch, and a fresh slice for a switch;
- `fillPreds` drops its per-block `seen` map: only the block currently being
  walked appends to any predecessor list, so a duplicate edge is exactly a repeat
  of the last entry in the target's list.

The reverse-postorder sequence is unchanged, which matters — RPO order is visible
to every pass downstream.

### 4c. Per-function change tracking in the pipeline (`opt/pass.go`)

This is the candidate the brief names, priced above and implemented.

`Run(m, pipeline)` now carries a `changeLog`: a version counter per function,
bumped whenever a pass changes it, and a record per (pass, function) of the
version at which that pass last ran and reported no change. A pass whose record
matches the function's current version is skipped.

Soundness: every pass built by `FuncPass` is a deterministic function of the one
function it is handed. The only module state any of them reads is
`ir.Module.SymAttrs` (LoadElim and DeadAlloc, through `isAtomicPointerStore`),
which the front end writes once and no pass touches. So a pass that reported no
change on `f`, with no change to `f` since, cannot change it now.

The inliner takes part as a *producer*: `spliceCall` clones a snapshot of the
callee and mutates the caller only, so `inlineModule` reports each caller it
spliced into and nothing else. That is what collapses the 13 × 5051 re-scans —
the `clean` fixpoint the two inline stages re-enter now starts from the few
hundred callers that actually moved. Any other `ModulePass` that reports a change
(`deadfunc`, `unroll`) is assumed to have changed everything, which is the
conservative direction.

The public `Pass` interface is unchanged; `Run(m)` on a pass or a fixpoint still
means "over everything", so the `pe` and `difftest` callers and the pass unit
tests behave exactly as before.

---

## 5. What it bought — single programs, idle box

`main` = `5b085d2`, the tree this branch is cut from, with the full pipeline on.
Two repetitions per arm agreeing to within 0.5%; the table gives the mean.

### small — `goc/testdata/fmt_sprintf.go`, default GOMAXPROCS

| arm | wall | user CPU | peak RSS |
|---|---:|---:|---:|
| `main` 5b085d2 | 41.63s | 82.07s | 952 MB |
| + BuildCFG fix | 36.60s | 71.86s | 932 MB |
| + change tracking | **15.87s** | **39.89s** | **921 MB** |
| | **2.62x faster** | **2.06x less CPU** | −3.3% |

### http — `goc/testdata/stdlib_http_tls_client_server.go`, the corpus's worst case

| arm | | wall | user CPU | peak RSS |
|---|---|---:|---:|---:|
| default GOMAXPROCS | `main` | 257.98s | 410.94s | 3.386 GiB |
| | this branch | **76.73s** | **153.09s** | **3.080 GiB** |
| | | 3.36x | 2.68x | −9.1% |
| `GOMAXPROCS=1` | `main` | 351.78s | 346.99s | 4.207 GiB |
| | this branch | **125.44s** | **121.64s** | **4.160 GiB** |
| | | 2.80x | 2.85x | **−1.1%** |

**Read the last row carefully. The CPU work is down by a factor of three and the
peak RSS has barely moved.** That is not a disappointment, it is what the heap
profile predicted, and §7 says why.

### The profiles, re-taken on the new binary (small program)

Total CPU samples fell 78.69s → 41.45s. In absolute seconds:

| | before | after | |
|---|---:|---:|---:|
| `analysis.BuildCFG` | 8.85s | 1.38s | **−84%** |
| `opt.newAliasInfo` | 5.27s | 0.86s | −84% |
| `opt.GVN` | 5.97s | 0.93s | −84% |
| `opt.LoadElim` | 5.86s | 0.82s | −86% |
| `opt.SimplifyCFG` | 4.96s | 0.83s | −83% |
| `opt.DCE` | 4.63s | 0.82s | −82% |
| `opt.jumpThread` | 4.27s | 1.61s | −62% |
| `opt.Fold` | 3.41s | 0.60s | −82% |
| `opt.inlineModule` | 1.38s | 1.27s | −8% |
| **`opt.OptimizeModule`, all of it** | **36.67s** | **9.42s** | **−74%** |
| `runtime.gcBgMarkWorker` | 24.15s | 15.09s | −37.5% |
| `arm64.compileFunction` (untouched) | 10.22s | 10.85s | +6% |
| `goc.compile`, the front end (untouched) | 4.13s | 3.83s | −7% |

Total bytes allocated:

| | before | after | |
|---|---:|---:|---:|
| small | 11.76 GB | **4.26 GB** | −64% |
| http | 69.18 GB | **17.75 GB** | −74% |
| of which `BuildCFG`, http | 24.36 GB (35.2%) | **2.45 GB** (13.8%) | −90% |

## 6. What it bought — the corpus, 406 programs, `-O`, `GOMAXPROCS=1`

Every program in `goc/testdata` compiled on its own at `GOMAXPROCS=1` (so
CPU-seconds are work done, not contention), 32 compiles concurrently — the same
harness shape the wave-9 gate used, so the numbers are comparable to its
4,733.9 → 21,157.7 figure.

Three arms, all on this box in one sitting. The denominator is
`GOC_OPT_PIPELINE=bounded`, which the wave-9 gate measured as reproducing the
pre-wave `main` to 0.5% — it is what `goc -O` meant before the pipeline was
turned on.

| arm | CPU-seconds | **multiplier** | wall (32-wide) | largest peak RSS | programs > 3 GiB |
|---|---:|---:|---:|---:|---:|
| `bounded` (the pre-pipeline floor) | 5,087.2 | 1.000x | 5,906.7s | 3.049 GiB | 1 |
| `main` 5b085d2, full pipeline | 21,838.0 | **4.293x** | 21,881.4s | 4.231 GiB | 6 |
| **this branch** | **9,590.2** | **1.885x** | 9,609.1s | 4.401 GiB | 6 |

**The corpus-wide multiplier goes 4.293x → 1.885x. 56.1% of the full pipeline's
CPU cost is gone**, and what is left is under twice the un-optimised floor.

The win is uniform, not concentrated. Per-program CPU ratio (new/old): median
**0.457**, p90 0.483, range 0.342–0.499. Every one of the 406 programs got
between 2.00x and 2.92x cheaper.

**All 406 output images are byte-identical between the two arms.** That is the
strongest correctness statement available for a change of this kind, and it is
the whole corpus, not a sample.

### Peak RSS: essentially unchanged, and honestly so

| | `main` 5b085d2 | this branch |
|---|---:|---:|
| per-program RSS ratio, median | — | **0.953** |
| p90 | — | 1.010 |
| range | — | 0.870 – 1.052 |
| programs whose peak went **up** | — | 56 of 406 |
| summed peak RSS over the corpus | 389.7 GiB | 373.8 GiB (−4.1%) |
| largest single compile | 4.231 GiB | 4.401 GiB |
| programs over 3 GiB | 6 | 6 |

The isolated, uncontended measurement of the worst program (§5) has it going
4.207 → 4.160 GiB; the 32-wide sweep has the same program going 4.231 → 4.401
GiB. Both are within the run-to-run spread of a GC-paced heap. **The right
statement is that peak RSS did not move.** `compileRuntimeCapabilityPeakBytes`
at 5 GiB still stands and this branch gives no grounds to lower it. §7 is why.

---

## 7. The memory finding, and why this branch does not fix it

The brief asks the heap profile to look for whole-module IR retained when it
could be freed per function, and for the pass pipeline holding several copies of
anything. **Neither is what is there.**

At the http program's high-water mark (`-memprofile-peak`, 1.29 GB of sampled
live heap), every top site is the module itself:

| site | share of live heap at peak |
|---|---:|
| `goc.(*gen).globalArray` | 20.2% |
| `opt.spliceCall` — the inliner's cloned bodies | 15.3% |
| `ir.(*Func).NewBlock` | 11.2% |
| `ir.(*Func).newTemp` | 10.6% |
| `ir.(*Block).emit` | 10.2% |
| `opt.rewriteHeapAllocations` | 9.6% |
| `go/types.(*Checker).recordTypeAndValue` | 1.7% |

And what the compile *retains* at the end, after a forced GC, is the same set:
382 MB for the small program, **1,465 MB** for http. There is no second copy of
the module, no analysis structure outliving its pass, and the front end's
type-checker tables are 1.7% — not the problem.

So peak RSS is, to within noise:

    peak RSS  ≈  (live IR module)  ×  (1 + GOGC/100)  +  overhead

3.2–3.6 GiB of RSS against 1.4 GiB of live IR is exactly what `GOGC=100` buys.
Every byte this branch stopped allocating was **short-lived garbage that was
never resident at the high-water mark** — which is why the CPU fell by a factor
of two and the peak did not move. The two are simply not the same quantity here.

The levers that would move peak RSS are (a) making the module smaller, i.e.
inlining less, and (b) capping the heap goal. Both are decisions, not cleanups;
they are in §9.

---

## 8. Guards

| guard | required | result |

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
| corpus output, all 406 programs, `-O` | — | **406/406 byte-identical to `main` 5b085d2** |
| four corpus audits (allocation census, frame-escape, loop-alias, escape shadow), **check** mode | pass | `ok github.com/evanphx/cg12/goc 188.812s`, exit 0 |
| `TestParallelBackendIsByteIdenticalToSerial` | pass | PASS at workers = 3, 8, 64, 256 |
| capability matrix, **default** arm | 368/368 | `ok ... 102.180s`, exit 0 — **368 subtests PASS, 0 FAIL** (367 `PASS`, 1 `EXPECTED FAILURE`, 0 `KNOWN GAP NOW PASSES`) |
| capability matrix, **`-O`** arm | 368/368 | `ok ... 141.168s`, exit 0 — **368 subtests PASS, 0 FAIL**, same 367 + 1 split |
| `runtime_gc_type_mask_padding.go` `-O`, default `GOGC`, `GOMAXPROCS=3` | 0/20 | **0 / 20 failed** |
| same, `GOGC=10` | 0/20 | **0 / 20 failed** |
| `placement_bench/flate` `-O`, default `GOGC` | 0/250 | **0 / 250 failed** |
| `placement_bench/flate` `-O`, `GOGC=10` | 0/250 | **0 / 250 failed** |
| `placement_bench/p256` `-O`, `GOGC=10` | 0/100 | **0 / 100 failed** |

| capability matrix, default arm | 368/368 | **368 subtests PASS**, `make test-goc-status` `ok` |
| capability matrix, `-O` arm | 368/368 | **368 subtests PASS**, `make test-goc-status-opt` `ok` |
| `runtime_lock_osthread` crash loop | ≥400 runs, 0 crashes | **400 runs, 0 non-zero exits** |
| GC reducer (`runtime_gc_promoted_local_root`) | 0/20 at `GOGC=10` | **20 runs, 0 failures** |
| GC reducer | 0/20 at default `GOGC` | **20 runs, 0 failures** |
| flate (`placement_bench/flate`) | 0/250 | **250 runs, 0 failures** |
| the four corpus audits | pass | `TestAllocationCensus` `TestEscapeShadowPlacement` `TestFrameEscapeAudit` `TestLoopAliasAudit` — **all PASS** |
| `TestParallelBackendIsByteIdenticalToSerial` | pass | **PASS** |
| determinism, byte-identical | pass | see below |

620 crash-loop runs, zero non-zero exits. Both bench programs `panic` on a wrong
answer (`"signature did not verify"` in p256, a decompression mismatch in flate),
so these are correctness loops, not only crash loops.

Determinism was checked at whole-artifact level, twice over and against a serial
backend, since the new pass runs a second frame layout and could have introduced
an order dependence:

The matrix also prices the wave from the cheapest angle: its `-O` arm is now
**1.39x** the default arm's wall clock (141.17s against 102.18s) where the
wave-9 gate measured **2.21x** (311.92s against 141.03s) on the same box.
| `scripts/determinism-check.sh -corpus` (406 × 4 rounds, unoptimised) | byte-identical | **reproducible=406 varying=0 failed=0**, exit 0 |
| `scripts/determinism-check.sh -corpus -O` (406 × 4 rounds — **the arm this branch changes**) | byte-identical | **reproducible=406 varying=0 failed=0**, exit 0, and 0 layout-only residues |

    a4a749c0…  runtime pack, run 1
    a4a749c0…  runtime pack, run 2
    a4a749c0…  runtime pack, GOC_BACKEND_WORKERS=1
    2d93d9b4…  runtime_lock_osthread executable, run 1
    2d93d9b4…  runtime_lock_osthread executable, run 2
    2d93d9b4…  runtime_lock_osthread executable, GOC_BACKEND_WORKERS=1

---

Unit tests on every package this branch touches: `opt`, `ir`, `stackcheck`,
`plan9asm`, `plan9asm/sem`, `arm64`, `analysis`, `link`, `obj` — all `ok`.
(`go test ./goc/...` and `make test-unit` were left to the gate job, as
instructed; `go test ./goc -run TestNoSplitBudget` was run and passes.)

## 9. Found, and deliberately not done

## `make bench-perf`, before and after

Everything below is real and measured on the *new* binary. None of it was
attempted, and the reasons differ.

| pass | instances at `-O2` |
|---|---:|
| `dce` | 8 (`dce1…dce6`, `cddce1…`) |
| `fre` | 5 |
| `dse` | 5 |
| `copyprop` | 5 |
| `ccp` | 5 |
| `forwprop` | 4 |

And where GCC does iterate, it is again *inside* one pass and explicitly flagged:
`NEXT_PASS (pass_fre, true /* may_iterate */)` early in the pipeline versus
`NEXT_PASS (pass_fre, false /* may_iterate */)` late — the same pass, permitted
to iterate at one point and forbidden at another.

### Go `cmd/compile` — a Go array, and a constraint list instead of a loop

`$GOROOT/src/cmd/compile/internal/ssa/compile.go:457`, `var passes = [...]pass{…}`
— 59 entries, run by `Compile(f *Func)` as `for _, p := range passes { … }`
(compile.go:439). One traversal, no outer loop, no convergence test.

Repetition is again spelled out: `deadcode` appears 8 times under 8 different
names (`early deadcode`, `pre-opt deadcode`, `opt deadcode`, `gcse deadcode`,
`generic deadcode`, `lowered deadcode for cse`, `lowered deadcode`, `late
deadcode`), the rewrite driver `opt` 3 times (`opt`, `middle opt`, `late opt`),
`cse` 3 times, `fuse` and `copyelim` and `nilcheckelim` twice each.

The *reasons* are checked, not just commented: `var passOrder = [...]constraint{…}`
(compile.go:526) is a machine-verified list of "a must come before b" pairs with
the black magic written next to each — *"prove relies on common-subexpression
elimination for maximum benefits"*, *"deadcode after prove to eliminate all new
dead blocks"*, *"cse substantially improves nilcheckelim efficacy"*.

Individual passes do iterate. `applyRewrite` (ssa/rewrite.go:41) — the engine
behind `opt`/`late opt`/`lower` — says *"repeat rewrites until we find no more
rewrites"*, and bounds itself: `itersLimit := f.NumBlocks()` (min 20), after which
it turns on cycle detection and can `f.Fatalf("rewrite cycle detected")`. The
comment records the empirical distribution: *"As of Sep 2021, 90% of rewrites
complete in 4 iterations or fewer and the maximum value encountered during
make.bash is 12."* That iteration is **per function and per pass**, not per module.

`-d=ssa/<phase>/<flag>[=value]` (compile.go:~380) turns individual phases
`on`/`off`, or asks for `time`, `mem`, `stats`, `dump`; phases marked
`required: true` refuse to be disabled. So the list is fixed but individually
addressable — which is what makes a fixed list debuggable.

## 2. cg12's fixpoint, measured

`opt.DefaultPipeline` is **already an ordered 16-entry list**; what re-converges
are two nested constructs — `clean` (8 local passes) and `inline-fixpoint`
(inline + clean). Both converge at **module** granularity: a round is a full
traversal of all 5101 functions by each member pass, repeated until no function
anywhere changed.

Instrumented compile, `goc -O -o out goc/testdata/fmt_sprintf.go`, arm64,
otherwise idle box. 42.8 s wall / 82.1 s user, of which the optimiser is
**36.28 s** in 421 pass invocations.

**Rounds.** `clean` is entered 13 times and takes **1 to 7 rounds** (50 rounds
total = 400 whole-module traversals). The two `inline-fixpoint`s take **7 rounds**
and **3 rounds**.

**What later rounds do.** Round 1 of the first `clean` changes 10288
function-instances; round 4 changes 15; rounds 5, 6 change 4 and 3; round 7
changes 0. That shape repeats everywhere: the tail rounds touch single-digit
numbers of functions out of 5101.

**What later rounds cost.** A round costs the same whether it changes 8625
functions or none, because the cost *is* the traversal:

| `clean` instance #5 | funcs changed | round cost |
|---|---:|---:|
| round 1 | 92 | 662 ms |
| round 2 | 4 | 650 ms |
| round 3 | 4 | 664 ms |
| round 4 | **0** | 658 ms |

- rounds ≥ 2 of `clean`: **22.25 s of the 33.20 s** `clean` spends (67 %).
- the 13 rounds that changed **nothing at all** — pure convergence proof —
  cost **8.16 s, 22.5 % of the entire optimiser**.
- the inliner itself is cheap: 1.34 s over 10 invocations. `inline-fixpoint#1`'s
  7 rounds are expensive only because each drags a whole `clean` fixpoint behind it.

**What later rounds change** (module instruction count, second traced run):

| fixpoint | r1 | r2 | r3 | r4 | r5 | r6 | r7 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `clean#1` | −50,247 | −7,506 | −576 | −35 | −9 | −4 | 0 |
| `inline-fixpoint#1` | **+334,222** | +19,639 | +3,001 | +661 | +65 | +25 | 0 |
| `clean#2` | −101,735 | −8,067 | −1,046 | −16 | −1 | 0 | |

Rounds 4-7 of the first inline fixpoint inline **751 instructions of the 357,613
that fixpoint adds — 0.21% — for 8.26 s of its 22.31 s (37%)**.

## 3. The arms — same passes, different convergence granularity

`goc -O -o out goc/testdata/fmt_sprintf.go`, 3 reps each, means; reps agree to
1.5%. `full` is `main`'s behaviour.

| arm | wall (s) | user (s) | output |
|---|---:|---:|---|
| `full` (`DefaultPipeline`) | **42.58** | 82.13 | 13,932,592 B |
| `ordered` (every fixpoint → one traversal) | **14.61** | 38.56 | +0.29% |
| `ordered2` (hand-ordered, `clean` twice at chosen points) | **16.91** | 42.04 | −0.09% |
| `perfunc` (convergence inside the per-function loop) | **20.24** | 49.33 | **byte-identical to `full`** |
| `bounded` (fold/copy/dce) | 6.84 | 20.70 | −23.8% |

Both arms were run on this box. The first attempt of each was worthless — the
other two ccwork jobs on the machine had it at load average 188 on 64 cores, and
the suite's own null arm (goc timed against goc) spread by up to 104%. Both arms
were then re-run in a quiet window (load average under 4), which is what is
reported here.

### Contained, but out of scope for "cheap fixes"

**The control is unchanged.** `control/spin-fixed-work` is the fixed integer
loop compiled by both compilers and appears in all eleven programs, so it is
eleven independent readings of the same quantity:

**`perfunc` is the result.** Every member of `clean` is a pure per-function
transform (`JumpThreadPass` keeps only `map[*ir.Func]*jtState`), so converging
one function before moving to the next gives the same program — measured, not
argued: identical sha256 and size, at **2.10x less wall time and 1.66x less
CPU**. Half of cg12's fixpoint cost is not the fixpoint; it is testing
convergence at module granularity when every transform is per-function.

| tree | control mean | s.d. | committed control |
|---|---:|---:|---:|
| `main` 5b085d2 | **0.9256** | 0.0041 | 0.9260 |
| branch | **0.9244** | 0.0075 | 0.9260 |

1. **The arm64 backend is now the largest compiler-side cost.**
   `arm64.compileFunction` was 12.99% of the small compile's CPU and is now
   **26.18%** (10.85s of 41.45s) — it did not get slower, everything around it
   got faster. `regAlloc` is 16.94% and `colorAlloc` 11.12% of the total. Nobody
   has profiled the backend; this job did not either.

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

### The identity holds on four programs

2. **`analysis.BuildCFG` is still 13.8% of all bytes allocated** (2.45 GB on the
   http compile) even after the rewrite, because the `num` map, the RPO slice,
   the postorder slice and the DFS stack are still allocated per call and it is
   still called once per block per pass per round. The fix is to cache the CFG on
   the function and invalidate it on mutation — which means touching every IR
   mutation site, and is a design decision, not a cleanup.

**The budget fires on a constructed overflow** — `goc` exits 1 with the message
above, and `TestNoSplitBudgetRejectsAnOverflowingChain` /
`TestNoSplitBudgetProducesNoObject` hold it to rejecting rather than warning.
It also fired on the tree it was written for, which was not expected: 23 chains
over the reserve under `-O` and 50 without it, the deepest at 3024 bytes against
920, with `runtime.mcache.nextFree` — the function that crashed
`runtime_lock_osthread` — already 184 bytes over before anything is inlined into
it.

`full` vs `perfunc`, whole-program `-O` builds, binaries compared byte for byte:

**Inlining into nosplit callers is re-enabled**, bounded by measured frames
rather than by a proxy: `opt.InlineIntoNoSplitCallers` inlines, cleans up,
measures the frame the backend will lay out, and restores the caller verbatim if
it grew past its chain's headroom.

3. **`jumpThread` is now the most expensive single optimisation pass** (1.61s,
   3.88%) and is the one that fell least (−62% against −82% to −86% for the
   rest). It builds a CFG twice per call (`jumpthread.go:83` and `:423`) and
   `SimplifyCFG` builds three per call (`cfg.go:197, 226, 229`). Both look
   reducible by inspection; neither was touched, because "looks reducible by
   inspection" is how a correct pass becomes an incorrect one.

**What it bought**: 106 nosplit callers got their inlining back on
`runtime_lock_osthread`, one was measured and reverted, and 201 have no headroom
at all. The allocator fast paths are in the third group and stay there — which
is the right answer and the reason the stopgap looked like it worked. On the
performance suite the change is not measurable: control 0.9244/0.9325 against
`main`'s 0.9256 and the committed 0.9260, median row change +0.2 to +0.3%.

| program | `full` wall/user | `perfunc` wall/user | binaries |
|---|---:|---:|---|
| `fmt_sprintf.go` | 41.59 / 82.41 | 20.21 / 48.48 | **identical** |
| `placement_bench/interp` | 42.32 / 80.71 | 21.05 / 48.62 | **identical** |
| `placement_bench/json` | 51.58 / 99.99 | 25.41 / 60.83 | **identical** |
| `placement_bench/flate` | 46.80 / 88.98 | 22.12 / 52.60 | **identical** |


4. **`inlineModule` rebuilds the call graph, the SCC condensation and the
   call-site counts on every fixpoint round** — 10 full rebuilds on the small
   program. It is 3.06% of CPU now (1.27s, up from 1.75% because everything else
   shrank). Maintaining them incrementally is possible; it interacts with the
   growth-cap bookkeeping and is not a five-line change.

## What this leaves behind

Adding a 3-round cap on the two inline fixpoints (`perfunc3`) gives 17.92 /
18.02 / 22.45 / 19.49 s wall — 2.3-2.4x `full` — for binaries 0.006-0.011%
*smaller* than `full`'s.

5. **The per-function analyses still allocate fresh maps per call**:
   `newAliasInfo` (4.9% of bytes), `useCounts` (3.6%), `defMap` (2.7%),
   `availMem` (2.3%), `Dominators` (3.6%), `domChildren` (2.2%), `predMap`
   (3.3%). A scratch arena reused across passes on one function would take most
   of the remaining garbage out. That is a refactor across a dozen files.

## 4. Code quality — `make bench-perf` under `ordered`

6. **`ir.Block.SuccCount`/`SuccAt` were added for the CFG walk only.** Ten other
   call sites still use `Succs()` and still allocate. Converting the hot ones is
   mechanical; it was left because the CFG walk was where the bytes were.

`GOC_OPT_PIPELINE=ordered make bench-perf`, 42 rows, 9 interleaved repetitions,
547 s. The committed baseline is the full-pipeline reference (`bb66b35`, re-cut
on an idle box), and the gated number is a goc/host ratio formed inside one
repetition, so it is comparable across runs by construction.

### Needs a decision, not a patch

**All 42 rows `within tolerance`.** Largest movements (lower ratio = better):

7. **Peak RSS is set by the module's live size times the GC's heap goal, and
   nothing in this job's remit moves either.** §7 has the profile. The two levers
   are inlining less (a code-quality trade, and the wave exists precisely to
   inline) and capping the heap goal — `debug.SetGCPercent` or, better,
   `debug.SetMemoryLimit`, which would let a compile bound its own RSS instead of
   `compileRuntimeCapabilityWorkers()` shrinking every machine's worker pool by
   dividing `MemAvailable` by 5 GiB. That is a policy question (what limit, set
   by whom, on which machines) and it trades back some of the CPU this job won,
   since the GC is already 36% of the remaining samples. It should be decided,
   not slipped in.

| row | fixpoint | ordered | change | resolved | tol |
|---|---:|---:|---:|---:|---:|
| `sortmap map/build-probe` | 6.0921 | 5.3962 | −11.4% | +4.1% | 14.5% |
| `regexp regexp/find-submatch` | 6.4397 | 6.2096 | −3.6% | +3.3% | 5.0% |
| `flate flate/decompress` | 4.7187 | 4.5695 | −3.2% | +2.7% | 5.0% |
| `text text/parse` | 7.7207 | 7.8860 | **+2.1%** | +1.7% | 5.0% |
| `json json/marshal` | 14.7342 | 14.9770 | **+1.6%** | −0.7% | 5.5% |
| `interp interp/bytecode-loop` | 19.0659 | 18.9109 | −0.8% | +0.7% | 5.0% |

8. **The `FuncPass` loop is serial over 5051 functions and the transforms are
   independent per function.** The backend already compiles functions in
   parallel and has a byte-identity test for it. Doing the same for the optimiser
   is the single largest remaining win available, and it is exactly the kind of
   thing that must not be done casually: it changes peak memory (several
   functions' analyses live at once) and it needs the same byte-identity guard
   the backend has. Left for a job that can own it.

9. **Pass ordering was not touched at all.** A separate job is researching how
   LLVM, GCC and Go's own compiler order their passes. The pipeline's shape —
   `clean` seven passes deep, re-entered after every inlining round — is the
   thing that made the tracking worth 2.3x, and it may be the wrong shape to
   begin with. Nothing here presumes an answer.

---

## 10. `make bench-perf` — and why it needed a second run

**The perf suite's eleven programs compile to byte-identical executables on this
branch and on `main` 5b085d2.** Checked directly:

    placement_bench/{interp,sha,regexp,json,sortmap,flate,text}
    perf_bench/{chase,conc,gcpress,float}

all eleven IDENTICAL. So `make bench-perf` on this branch is timing the same
binaries `main` would produce, and any movement it reports is the machine.

### Run 1 — no regression, but the run does not gate

`--- FAIL: TestPerformanceSuite (830.82s)`. The failure is **not** a ratio
regression. The only assertion that fired is the suite's own noise gate:

    chase/l1-resident      ratio 1.1018, one-repetition spread 20.3% (ceiling 15.0%), null spread 29.8%
    gcpress/gc/pointer-write  ratio 10.5738, one-repetition spread 33.1% (ceiling 15.0%), null spread 4.2%

No row failed its tolerance band. The control:

| | mean `control/spin-fixed-work` ratio over 11 programs |
|---|---:|
| committed baseline | **0.9260** |
| this run | **0.9225** |

0.4% *below* the baseline, which is the good direction, and far inside any band.

The run was polluted and the numbers say so plainly. The baseline's
one-repetition spreads are 0.03–0.07%; this run's are 0.4–33%. The control loop's
absolute times moved with them — 31.0 ms baseline against 50–54 ms here for goc,
and 33.5 ms against 55–59 ms for the host, i.e. **both arms slowed by the same
~70%**, which is a machine that is busy and not a compiler that changed. The box
is shared and the run started while the corpus determinism sweep's tail was still
draining (1-minute load 3.45, 5-minute load 46.8).

### The control run: `main` fails the same gate, on the same box, in the same hour

Rather than argue the point, it was measured. `main` 5b085d2 was checked out
into a worktree and `make bench-perf` run there:

    --- FAIL: TestPerformanceSuite (958.96s)
    gcpress gc/pointer-write     spread 35.0% (ceiling 15.0%), null 1.3%
    gcpress gc/live-heap-churn   spread 17.0% (ceiling 15.0%), null 3.4%

**`main` fails `make bench-perf` on this box today**, on the same assertion, on
one of the same rows. Its control mean is **0.9241** — *below* both of this
branch's readings.

| tree | control mean | verdict | rows over the noise ceiling |
|---|---:|---|---|
| committed baseline | 0.9260 | — | 0 (spreads 0.03–0.07%) |
| `main` 5b085d2, today | **0.9241** | FAIL (noise gate) | 2 |
| this branch, run 1 | **0.9225** | FAIL (noise gate) | 2 |
| this branch, run 2 | **0.9271** | FAIL (noise gate) | 5 |

**`make bench-perf` does not pass on this machine today, and it is not this
branch.** The required guard — "the control ratio is 0.9260x and a regression
there means you traded run-time for compile-time" — is met: 0.9225 and 0.9271
against a 0.9260 baseline, with `main`'s own reading at 0.9241 in between. No
tolerance band failed in any of the three runs. And the eleven benchmark
programs compile to byte-identical executables on both trees, so the suite is
timing the same machine code either way.

---

## 11. The tracking, measured from the other side

The same visit counters as §3, re-run on the new binary. The fixpoints iterate
exactly as many rounds as before (`clean` 13 calls / 50 rounds,
`inline-fixpoint` 2 calls / 10 rounds) — nothing converged early:

| pass | visits before | visits after | skipped | **changed, before → after** |
|---|---:|---:|---:|---|
| fold | 252,550 | 28,138 | 226,012 | 5,384 → **5,384** |
| copy | 252,550 | 27,281 | 226,869 | 3,814 → **3,814** |
| loadelim | 252,550 | 27,236 | 226,914 | 2,478 → **2,478** |
| deadalloc | 252,550 | 27,005 | 227,145 | 417 → **417** |
| gvn | 252,550 | 27,005 | 227,145 | 3,060 → **3,060** |
| jumpthread | 252,550 | 26,944 | 227,206 | 3,737 → **3,737** |
| simplifycfg | 257,601 | 31,999 | 227,234 | 9,858 → **9,858** |
| dce | 261,700 | 31,223 | 232,141 | 4,467 → **4,467** |
| **total** | **2,034,601** | **226,831** | **1,807,770** | — |

**88.9% of per-function pass invocations are gone, and every pass still makes
exactly the same number of changes it made before** — not approximately, exactly,
for all eight. That is the tracking's correctness argument turned into a
measurement: what was skipped was, to the last visit, work that would have
changed nothing.

---

## 12. The ledger

| | `main` 5b085d2 | this branch |
|---|---:|---:|
| **corpus-wide CPU, 406 programs, `-O`, `GOMAXPROCS=1`** | 21,838.0 CPU-s | **9,590.2 CPU-s** |
| **as a multiplier over the pre-pipeline floor** (`bounded`, 5,087.2 CPU-s) | **4.293x** | **1.885x** |
| **peak RSS, largest program, `GOMAXPROCS=1`, isolated** | 4.207 GiB | **4.160 GiB** |
| peak RSS, same program, 32-wide sweep | 4.231 GiB | 4.401 GiB |
| peak RSS, per-program ratio, median | — | 0.953 |
| corpus programs over 3 GiB | 6 | 6 |
| small program (`fmt_sprintf.go`), wall | 41.63s | **15.87s** (2.62x) |
| small program, user CPU | 82.07s | **39.89s** (2.06x) |
| http program, wall, default GOMAXPROCS | 257.98s | **76.73s** (3.36x) |
| bytes allocated, small / http | 11.76 GB / 69.18 GB | **4.26 GB / 17.75 GB** |
| capability matrix, `-O` arm, wall | 311.92s (wave-9 gate) | **141.17s** |
| corpus output images | — | **406/406 byte-identical** |

**Peak RSS is the number that did not move, and §7 explains why: it is the live
IR module times the GC's heap goal, and this branch removed garbage, not live
data.** `compileRuntimeCapabilityPeakBytes = 5 GiB` still stands on its own
evidence; nothing here justifies lowering it.

### The single biggest remaining cost

The garbage collector, at **36.4%** of the compile's CPU samples (15.09s of
41.45s on the small program) — a larger *share* than before precisely because
everything around it shrank, though 37.5% less in absolute seconds. After it,
the **arm64 backend at 26.2%**, which nobody has profiled and this job did not
touch. The optimiser, which was 46.6% and the whole subject of this job, is now
22.7%.

The eleven `control/spin-fixed-work` rows came back 0.9246–0.9265 against a
baseline range of 0.9247–0.9284.

The run's exit status is FAIL, and **not for a ratio**: the suite's noise gate
tripped because `chase/pointer-node`'s one-repetition spread was 13.65% against
1.27% in the baseline. That is my fault — I ran a `go build` and a `goc` compile
on the box during repetitions 1-2, and `chase` is the memory-latency workload.
Every row passed despite the extra noise (noise hides differences, it does not
manufacture agreement), but the `chase/*` rows should be read as not cleanly
measured.

## 5. Code quality — `make bench-perf` under `perfunc3`

`GOC_OPT_PIPELINE=perfunc3 make bench-perf`, 42 rows, 9 repetitions, 589 s.
**0 of 42 rows exceed the baseline's own tolerance** (compared row by row by
hand). Largest: `text/format-append` −6.2% (tol 12.8%), `map/build-probe` −5.6%
(tol 14.5%), `text/sprintf` +2.7% (tol 16.6%), `json/marshal` +1.1% (tol 5.5%),
`interp/bytecode-loop` +0.1% (tol 5.0%). Control rows 0.9252-0.9286 against a
baseline range of 0.9247-0.9284.

This run also exited FAIL on the noise gate, and this time not through anything
I did: the box's 1-minute load average hit 20.5 (other jobs on this shared
64-core machine), and two rows exceeded the suite's absolute 15% spread ceiling,
which aborts before the verdict table prints. Hence the by-hand comparison.

## 6. Verdict

| pipeline | wall on the reference compile | code |
|---|---:|---|
| `full` (module fixpoint, shipped) | 42.6 s | reference |
| `perfunc` (converge per function) | 20.2 s | **byte-identical**, 4/4 programs |
| `perfunc3` (+ inline capped at 3 rounds) | 17.9 s | 0.01% smaller; 0/42 perf rows moved |
| `ordered` (single traversal of everything) | 14.6 s | 0.29% larger; 0/42 perf rows moved |
| `bounded` (fold/copy/dce) | 6.8 s | — |

**Recommendation: keep the fixpoint, fix its granularity, then bound it.**
Moving `clean`'s convergence test inside the per-function loop is free — the
binaries are identical — and halves the compile. Capping the two inline
fixpoints at 3 rounds takes another 11% and changes the binary by a hundredth of
a percent. Converting the whole pipeline to a hand-ordered fixed list buys a
further 1.23x and costs the maintenance of a pass-interaction order, which is
not worth it while the pass set is still moving.

Full argument, the three compilers' sources, and every measurement:
`OPTIMISER_PIPELINE.md`.

## Scaffolding

The instrumentation and the alternative pipeline arms were an uncommitted patch
(`opt/trace.go` plus four `case` lines in `opt/pass.go`'s `ModulePipeline`).
They have been removed: `git diff --name-only main` on the delivered tree is
`CCWORK_REPORT.md` and `OPTIMISER_PIPELINE.md`, and `go build ./...` is clean.
No pipeline behaviour was changed, so the allocation census and determinism are
untouched by construction (`opt/pass.go` is byte-identical to `main`'s).

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

---
---

# WAVE 10 GATE REPORT — `integration/wave10` (2438919) onto `main` (5b085d2)

Gate job. Host: aarch64 Linux, 64 cores, 250 GiB RAM, **exclusive** (idle box).
Everything below was watched to completion by this job unless explicitly marked
UNVERIFIED. Sections are appended as each result lands.

## 0. Scope of the merge (read, not measured)

`git diff --stat 5b085d2..2438919`, per branch, code only (`.md` excluded):

| branch | files touched |
|---|---|
| `compile-time-profiling` (ef2eb2e^2) | `analysis/cfg.go`, `cmd/goc/main.go`, `cmd/goc/profile.go`, `ir/func.go`, `opt/inline.go`, `opt/pass.go` |
| `optimiser-pipeline-research` (d480d94^2) | **none — documentation only, confirmed** |
| `nosplit-frame-budget` (2438919^2) | `arm64/{assembly,mc,nosplit,nosplit_debt,nosplit_measure,parallel}.go`, `ir/clone.go`, `opt/{inline,nosplitinline}.go`, `plan9asm/*`, `stackcheck/*`, tests |

Claim 2 (`optimiser-pipeline-research` changes no behaviour) is **CONFIRMED by
construction**: its diff against `main` contains no non-`.md` file at all. Note
that `opt/pass.go` on the *merged* tree does differ from `main` — that is
branch 1's per-function change tracking (142 lines) plus 6 lines from branch 3,
not branch 2.

Neither `stdlib/` nor `goc/testdata/` is touched by any of the three branches,
so the byte-identity comparison in §8 compiles identical sources with two
compilers.


## 1. THE FINDING: does `main` already run over its nosplit reserve? — **YES, CONFIRMED**

Measured by this job, not taken from the branch.

The method matters, so it is stated first. `main`'s compiler contains no stack
check, so it cannot report chain heights; the merged compiler can, but it also
inlines into nosplit callers, which changes the code. `GOC_NO_NOSPLIT_INLINE=1`
disables that pass and nothing else. So the measurement is only of `main` if
that configuration reproduces `main`'s code exactly — **and it does, byte for
byte**:

| build of `goc/testdata/runtime_lock_osthread.go` | sha256 of the executable |
|---|---|
| `goc-main` (5b085d2), `-O` | `f5d5916dffbb8cc1…` |
| merged + `GOC_NO_NOSPLIT_INLINE=1`, `-O` | `f5d5916dffbb8cc1…` — **identical** |
| `goc-main` (5b085d2), no `-O` | `a7e7c4015e111722…` |
| merged + `GOC_NO_NOSPLIT_INLINE=1`, no `-O` | `a7e7c4015e111722…` — **identical** |

Both compilers were built from the same absolute path
(`/home/evan/.ccwork/ws/wave10-gate/repo`), so nothing in the comparison turns
on an embedded path.

The heights that configuration reports (`GOC_DEBUG_NOSPLIT=heights`,
`GOC_NOSPLIT_LIMIT=100000000` so the walk runs but nothing is rejected), against
the 920-byte reserve:

| arm | chains over 920 | deepest |
|---|---:|---|
| `main`, `-O`, whole program | **21** | **1824** `.Lruntime_asm_arm64_callRet` |
| `main`, no `-O`, whole program | **48** | **3024** `runtime_doubleCheckTypePointersOfType` |

and the function the whole line of work started from:

    runtime_mcache_nextFree   1104   (-O)      — 184 bytes over, nothing inlined
    runtime_mcache_nextFree   1216   (no -O)

**The claim is confirmed.** `main` — today, unmodified, with no inlining into
any nosplit caller — has tens of nosplit chains over the reserve, the deepest at
3024 bytes (3.3x the reserve), and `mcache.nextFree` is over by exactly the 184
bytes the branch reports *before anything is inlined into it*. The split-stack
crash was one instance of a standing condition, not something the inliner
created.

Two corrections of detail, neither changing the conclusion:

- The brief's headline numbers are **23** (`-O`) and **50** (no `-O`). On this
  program I measure **21** and **48**. The register's own text gives ranges —
  "every optimized build agrees on 22-24 over-limit chains; every unoptimized
  one has 48-50" — over twenty configurations (seven pack roots ± `-O` and four
  whole programs ± `-O`). My single whole-program build sits one below the
  bottom of the `-O` range and inside the unoptimised one. The count is
  configuration-dependent; the condition is not.
- The 3024 deepest is an **unoptimised** figure. Under `-O`, which is how the
  capability matrix and the crash both ran, the deepest is 1824.

Worth stating plainly because the register does not: **`main` would fail the
merged tree's own budget** if the debt register were empty. That is the finding,
inverted — the wave does not introduce a passing build, it introduces the
instrument that says the tree has been over its reserve all along, and then
freezes what it found.

One judgement in the instrument itself, flagged not faulted: the limit is 920,
not Go's 792, on the argument that cg12's guarded prologue always subtracts its
frame before comparing (`arm64/nosplit.go:23-47`). I read the derivation and it
is sound *given* `goStackPrologue`; it is also 128 bytes of reserve that Go's
own linker does not spend, and the file records 58 further chains between 792
and 920 that a Go-strict limit would reject. If `goStackPrologue` ever grows
Go's small-frame shortcut, `stackSmallReserved` must go to 0 in the same commit.
That coupling is documented in the constant's comment but not enforced by a
test.

## 2. Is the debt register a floor or a licence? — **A FLOOR. Confirmed in code.**

Four independent places have to be right for the register not to become
spendable headroom. All four are:

1. `stackcheck.Report.Headroom` is computed as `w.headroom(config.Limit)`
   (`stackcheck.go:381`), and `walker.headroom` (`:707`) subtracts from the
   passed limit only. It never consults `Config.Recorded`. Recorded heights are
   used in exactly one place, `limitFor` (`:426`), which is reached only from
   the over-limit test.
2. The budget the **inliner** is handed is built by
   `arm64.newNoSplitFrameBudget`, which calls `stackcheck.Check` with
   `Config{Limit, CallSize}` and **no `Recorded` field at all**
   (`arm64/nosplit_measure.go:76`). The inliner's `Headroom` therefore cannot
   see the register even in principle.
3. `opt.InlineIntoNoSplitCallers` takes `Headroom(name) - 16` and refuses any
   caller with `allowance <= 0`, so a function on an over-limit chain (negative
   headroom) is refused outright — which is why `mcache.nextFree` gets nothing
   and is reported under "no headroom".
4. Anything worse than recorded is rejected: `limitFor` returns the recorded
   height and `Check` fails on `walk(name) > limitFor(name)` — strictly
   greater, so recorded height passes and recorded+1 fails. `Recorded` is
   attached only when `limit == noSplitLimit` (`arm64/nosplit.go:219`), so a
   test that lowers the limit cannot be handed entries that outrank it.

The safety property that matters is structural rather than per-entry: a function
*interior* to an over-limit chain also has negative headroom, because
`headroom` charges both the frames above it (`depthAbove`) and the chain below
it (`walk`). There is no way to grow a frame anywhere on an over-limit chain.

Covered by `stackcheck.TestRecordedDebtAllowsItsOwnHeightAndNotOneByteMore` and
`TestRecordedDebtDoesNotWidenHeadroom`, plus
`opt.TestNoSplitInlineRefusesACallerAlreadyOverTheReserve` — all three run in
§ `make test-unit` below.

## 3. `make test-unit` — **PASS**

`make test-unit`, 38 packages (`go list ./...` minus `goc`, `cmd/goc`, `difftest`,
`cc`), exit **0**, zero `FAIL` lines. Includes `stackcheck` (0.033s), `opt`
(0.955s), `arm64` (9.497s), `ir`, `link`, `obj`, `plan9asm` — every package the
wave touches. This is the run that carries
`TestRecordedDebtAllowsItsOwnHeightAndNotOneByteMore`,
`TestRecordedDebtDoesNotWidenHeadroom` and
`TestNoSplitInlineRefusesACallerAlreadyOverTheReserve` from §2.

## 4. Regenerating `arm64/nosplit_debt.go` from the merged tree — **IDENTICAL, 50/50**

Regenerated exactly as the file documents: the shipping compiler
(`GOC_DEBUG_NOSPLIT=heights`, real 920 limit, register active, `CG12_NOCACHE=1`
so no pack is served from cache) over 22 configurations — seven capability
matrix pack roots (`nil`, `net/http`, `net/smtp`, `crypto/x509`, `crypto/ecdsa`,
`crypto/ecdh`, `crypto/hpke`) with and without `-O`, and whole-program builds of
`runtime_lock_osthread`, `runtime_gc_concurrent_mark`,
`stdlib_http_tls_client_server`, `stdlib_compress_zlib_lzw` with and without
`-O`. Maximum height per symbol over all 22.

- **All 22 builds exit 0.** The ratchet holds everywhere it was claimed to.
- The regenerated register has **50 entries, the same 50 names, and every one at
  the same height** as the committed file. Nothing to commit; no drift.
- Per-configuration counts of chains over 920:

| configuration | over-limit chains |
|---|---:|
| pack roots, no `-O` (7) | 48 (runtime-only) – 50 |
| whole programs, no `-O` (4) | 48 – 50 |
| pack roots, `-O` (7) | **25 – 27** |
| whole programs, `-O` (4) | 21 – 23 |

One correction to the register's own text, which says "every optimized build
agrees on 22-24 over-limit chains": the optimized **pack** builds have 25-27.
That is not a regression from the wave — I re-ran all seven optimized pack roots
with `GOC_NO_NOSPLIT_INLINE=1` and got **the same counts, and byte-for-byte the
same heights, for every one of the 50 chains** (`diff` over the height dumps: no
differences in any of the 7 configurations). The optimized-pack figure was
simply understated in the comment. The recorded heights themselves are
unaffected — they come from the unoptimised builds, which are the worst case.

**The inliner provably does not grow any over-limit chain.** That is the
strongest statement available here and it is measured, not argued: same chains,
same heights, with and without the new pass.

What the pass does buy, on `runtime_lock_osthread -O`
(`GOC_DEBUG_NOSPLIT=inline`): **106 callers accepted, 1 reverted after
measuring, 201 with no headroom at all**. Most accepted callers *shrink*
(`atomic.Pointer.Load` 32 → 16) because inlining removes the outgoing argument
area. The 201 refused is the standing cost of the debt.

### One structural caveat, not observed firing

`opt.InlineIntoNoSplitCallers` computes the budget **once** and then walks every
caller, spending `Headroom(name) - 16` per caller without recomputing. Two
nosplit functions on the *same* chain can therefore each be granted the same
slack. For a chain already over the reserve this cannot bite (every member has
negative headroom). For a chain under it, the double spend would push the chain
over 920 and the build would fail — unless the chain's root happens to be a name
in the debt register whose recorded height came from the unoptimised worst case,
in which case the register would absorb the growth silently. That is the one
place the register could act as a licence.

It does not fire on this tree: the height comparison above shows every recorded
chain at exactly its no-inline height in all seven optimized pack builds. It is
worth a test (two nosplit callers on one chain, each with headroom, both
inlined) before someone changes the inliner's ordering. Not a merge blocker.

## 5. Capability matrix, both arms, `-v` — **368/368 each, PASS SETS IDENTICAL**

`go test -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/... -v` with
`-runtime-status-shards=1`, and the same plus `-runtime-opt`.

| arm | exit | subtests PASS | subtests FAIL | wall |
|---|---|---:|---:|---:|
| default | **0** | **368** | **0** | 102.113s |
| `-O` | **0** | **368** | **0** | 144.398s |

The two `--- PASS` name sets were diffed against each other: **identical, 368
names, no difference in either direction**. Each arm carries exactly one
`EXPECTED FAILURE` (`runtime_panic_print_string.go`) and zero
`KNOWN GAP NOW PASSES`, matching what the branch reported.

The `-O` arm is **1.41x** the default arm's wall clock (144.4s vs 102.1s), in
line with the 1.39x the branch measured and far below the wave-9 gate's 2.21x.

## 8. `go test -timeout 60m -parallel 10 ./goc/...` — **PASS, 0 failures**

    ok  github.com/evanphx/cg12/goc  1245.971s      exit 0

20.8 minutes, no `FAIL` line of any kind. That run carries the four corpus
audits in check mode (`TestAllocationCensus`, `TestFrameEscapeAudit`,
`TestLoopAliasAudit`, `TestEscapeShadowPlacement`) against the committed
baselines, so §"audits" below is this run plus the explicit repeats.

`TestDeriveClassifiesEveryGenField`, which the brief flags as having failed in
five waves: **PASS**, re-run on its own to be sure
(`--- PASS: TestDeriveClassifiesEveryGenField (0.00s)`). No unclassified `gen`
field, so there is nothing to name. This is expected on inspection: the test
fails when a field is added to `gen` without being classified as
whole-compilation or per-function, and none of the three branches adds a field
to `gen` — the wave does not touch `goc/derive.go` or the front end at all.

## 9. THE THING I CANNOT EXPLAIN AWAY: the merged tree **fails to build a corpus program that `main` builds**

Found by `scripts/determinism-check.sh -corpus` (unoptimised), which is the only
guard in the tree that drives all 406 programs all the way to a written object.

    $ goc -o out goc/testdata/stdlib_os_exec_echo.go        # merged tree
    goc: nosplit frame budget: nosplit stack overflow:
        syscall_runtime_AfterForkInChild -> runtime_clearSignalHandlers ->
        runtime_setsig -> runtime_sigaction -> runtime_throw ->
        runtime_fatalthrow -> runtime_systemstack
      976 bytes of nosplit frames against a 920-byte limit, 56 over
            80  syscall_runtime_AfterForkInChild
            64  runtime_clearSignalHandlers
           128  runtime_setsig
           464  runtime_sigaction
           128  runtime_throw
            96  runtime_fatalthrow
            16  runtime_systemstack
    exit status 1

    $ goc-main -o out goc/testdata/stdlib_os_exec_echo.go   # main 5b085d2
    exit status 0

Attribution, measured:

- **The chain is not created by the wave.** With `GOC_NO_NOSPLIT_INLINE=1` —
  the configuration proved byte-identical to `main` in §1 — the build fails
  with the identical chain and the identical 976 bytes. `main` compiles this
  program today and ships a 976-byte nosplit chain through the fork-in-child
  path.
- **`syscall_runtime_AfterForkInChild` is not in the debt register.** The
  register was generated from 22 configurations (7 pack roots ± `-O`, 4 whole
  programs ± `-O`), none of which reaches `os/exec`'s fork path. It is a 51st
  chain the generation method never saw.
- It is unoptimised-only: the same program at `-O` builds clean (mem2reg
  shrinks `runtime_sigaction`'s 464-byte frame below the line).
- Nothing else in the tree catches it. The corpus audits compile to IR and stop;
  the budget runs in `compileToObjectWithBundle`, so a test that never writes an
  object never runs it. The capability matrix passes both arms because no
  capability program forks. `go test ./goc/...` passes. Only the determinism
  corpus sweep builds all 406 to completion, and it is not part of `make test`.

**Consequence: on the merged tree, `goc` without `-O` cannot compile a Go
program that uses `os/exec`.** That is a hard build failure — the budget
deliberately has no warning mode — on a program `main` compiles. One of 406
corpus programs today; the shape (anything importing `os/exec`, or `syscall`'s
fork path) is not exotic.

This is the correct behaviour of the instrument and a regression in the tree's
buildability at the same time. The fix is a one-line register entry
(`"syscall_runtime_AfterForkInChild": 976`) *if* one accepts the register's
premise, or a frame shrink on `runtime_sigaction` if one does not — but it is
a decision for the wave's author, and the register's generation recipe should
grow the program that exposes it, or the corpus, as an input.

## 10. Regenerating the baselines from the merged tree — **ALL IDENTICAL**

Every file regenerated from the merged tree with its own `-update-*` flag, in
one sitting, then `git status`:

| file | regenerated | diff against committed |
|---|---|---|
| `alloc_census_baseline.txt` | `-update-alloc-census-baseline` (257s) | **none** |
| `frame_escape_baseline.txt` | `-update-frame-escape-baseline` | **none** |
| `loop_alias_baseline.txt` | `-update-loop-alias-baseline` | **none** |
| `escape_shadow_baseline.txt` | `-update-escape-shadow-baseline` | **none** |
| `slog_allocations_baseline.txt` | `-slog-allocations -update-slog-allocations` (host go1.26.1) | **none** |
| `escape_gc_differential.txt` | `-escape-gc-differential -update-…` | **none** |
| `escape_gc_reason_differential.txt` | `-escape-gc-reason-differential -update-…` (236s) | **none** |
| `arm64/nosplit_debt.go` | 22-configuration sweep, §4 | **none** (50/50 identical) |
| `perf_suite_baseline.txt` | `make bench-perf-update` | see §13 |
| `crypto_signing_bench_baseline.txt` | `make bench-crypto-update` | see §13 |

All mtimes moved, so every file really was rewritten; `git status` afterwards
shows only this report as modified. **The merged tree reproduces every
committed baseline byte for byte** — no placement moved, no escape decision
moved, no reason string moved, no frame publication appeared or disappeared, and
the goc-vs-gc differential is unchanged (permissive 1483 lines, pessimistic 401;
reason differential 315/85).

That is the strongest available statement that branch 1's "same passes, same
changes, 88.9% fewer invocations" claim did not alter a single optimisation
outcome anywhere in the corpus, and that branch 3's inlining changes nothing the
front end or the escape analysis can see.

## 11. Audits (four, census twice) and determinism — **PASS, with the one exception above**

Audits, check mode, run twice with `-count=1` so the second run is a real
execution and not a cached result:

| audit | run 1 | run 2 (`-count=1`) |
|---|---|---|
| `TestAllocationCensus` | **PASS** (244.5s) | **PASS** (182.6s) |
| `TestFrameEscapeAudit` | **PASS** | **PASS** |
| `TestLoopAliasAudit` | **PASS** | **PASS** |
| `TestEscapeShadowPlacement` | **PASS** | **PASS** |

Both runs' census and shadow diagnostics are **byte-identical to each other**
(203,220 front-end placements, 5,706 distinct sites, 810 disagreement sites,
both times). Plus the same four inside `go test ./goc/...` — three passes total.

Determinism, `scripts/determinism-check.sh -corpus`, 406 programs × 4 rounds,
24 workers:

| arm | result | exit |
|---|---|---|
| unoptimised | **reproducible=405 varying=0 failed=1** | 1 |
| `-O` | **reproducible=406 varying=0 failed=0**, 0 layout-only residues | 0 |

**Zero non-determinism in either arm.** The unoptimised non-zero exit is the one
compile failure of §9 (`stdlib_os_exec_echo.go`), not a varying image. Every
program that compiles compiles to the same bytes every time.

`TestParallelBackendIsByteIdenticalToSerial`: **PASS** (`ok
github.com/evanphx/cg12/arm64 0.228s`).

## 13. `make bench-perf` and `make bench-crypto` on an idle box — **BOTH PASS**

This is the question the brief flags as open: both have been failing their noise
ceiling on `main` too, on a busy box, with no ratio band exceeded. The box was
exclusive and idle for this run (load average 8 falling, nothing else of mine
running), and both were run together as the Makefile intends — perf pinned to
core 62, crypto to core 63.

| target | exit | result |
|---|---|---|
| `make bench-perf` | **0** | **PASS**, 559.9s — **all 42 rows within tolerance**, no noise-ceiling failure |
| `make bench-crypto` | **0** | **PASS**, 102.0s — all 4 rows within tolerance |

**Control ratio against the 0.9260 baseline** (`control/spin-fixed-work`,
`goc ns / host ns`, one row per program):

    interp 0.9251   sha 0.9265   regexp 0.9253   json 0.9256   sortmap 0.9250
    flate  0.9251   text 0.9271  chase  0.9247   conc 0.9272   gcpress 0.9264
    float  0.9250

Range **0.9247–0.9272**, mean **0.9258**, against the committed 0.9260 — a
spread of ±0.14%, and every row's own `ratio-sd%` is 0.03–0.07% where the
baseline records 0.03–0.08%. The null column is 0.9996–1.0002 on all eleven
programs, so there is no ordering artefact and the other columns can be
believed.

**So the noise-ceiling failures the last two waves reported were the box, not
the tree.** On an idle machine the instrument reproduces its own baseline to
within a seventh of a percent, and the wave moves nothing in it: the largest
movement in any of the 42 rows is a fraction of a percent, and crypto's four
rows move −0.2% to −0.5% with resolved movements of ±0.1% (i.e. indistinguishable
from zero).

That last point is worth stating on its own: **the wave costs nothing at run
time.** Branch 3 re-enabled inlining into 106 nosplit callers in the runtime,
and neither the crypto signing path nor any of the eleven perf workloads can
tell the difference.

### 13a. The two timing baselines, regenerated

`make bench-perf-update` and `make bench-crypto-update` (exit 0 both), run
together on the idle box. Both files are rewritten in this branch; the diffs are
noise, and here is the evidence rather than the assertion.

- **crypto**: all four rows move −0.1% to −0.4% (`p256/sign-verify` 24.0648 →
  23.9755). Every one is smaller than the row's own ±% interval.
- **perf**: 42 rows, **median movement 0.13%**, and 34 of 42 move less than
  0.5%. The eight that move more are exactly the rows whose committed
  `ratio-sd%` is already 4–6% — `sortmap map/build-probe` (+6.5%, sd 4.83 →
  5.26), `text text/format-append` (+3.8%), `gcpress gc/live-heap-churn`
  (+3.6%), `conc chan/pingpong-unbuffered` (+3.4%), `conc goroutine/spawn-join`
  (+2.4%). Their noise is host-side (`null-sd%` up to 14.5%) and it is present in
  the committed file too, so this is the instrument's own spread, not a
  movement. Every one of them passed the check run inside its own tolerance.

I have committed both regenerated files on the gate branch so the diff is
reviewable, but **there is no reason to take them**: nothing in either file
moved by more than its own noise, and re-baselining timing files on noise loses
information. Reverting these two files before merge is a defensible call and my
recommendation; the other eight regenerated baselines (§10) were identical, so
nothing else is affected either way.

No `main` control run was needed for either target: every row sits inside its
committed tolerance, so there is no number that needs attributing to the wave.

### 13b. `main` control — also passes, so the failure really was the box

Ran on the same idle box, tree checked out at `main` 5b085d2 and restored
afterwards:

| target, `main` 5b085d2 | exit | result |
|---|---|---|
| `make bench-perf` | **0** | **PASS**, 855.7s, 42/42 rows within tolerance |
| `make bench-crypto` | **0** | **PASS**, 170.4s |

`main`'s control ratios: 0.9246–0.9284 (mean 0.9259) against the same 0.9260 —
statistically the same spread as the branch's 0.9247–0.9272.

**Both targets pass on `main` and on the merged tree on an idle machine, and
failed on both on a loaded one.** The two previous waves' bench-perf failures
were an artefact of a shared box, not a property of any branch. (The wall-clock
difference — 855.7s on `main` against 559.9s on the branch — is the *compiler*
being 2.2x faster, §12a: the suite builds eleven programs with `goc` before it
times anything.)

### 9a. Addendum: the failure of §9 depends on how the build is split

The capability matrix compiles `stdlib_os_exec_echo.go` (it is capability
`runtime_status_test.go:1633`) and **passes it in both arms**, which looked like
a contradiction. It is not:

    goc -o out goc/testdata/stdlib_os_exec_echo.go                  -> FAILS (976 > 920)
    goc -runtime pack.gocrt -o out goc/testdata/stdlib_os_exec_echo.go -> exit 0
    goc build-runtime -packages "" -o pack.gocrt                    -> exit 0

The chain's root, `syscall.runtime_AfterForkInChild` (frame 80) and
`runtime.clearSignalHandlers` (64), sit *above* the part of the chain that lives
in the runtime pack. Building the pack alone, the deepest chain through
`runtime_sigaction` (frame 464, unchanged in both modes) is 832 and passes;
compiling the program against a prebuilt pack, the walk stops at the module
boundary; only the whole-program build sees all seven frames at once and
computes 976.

So the budget is a **per-module** check and its verdict is not invariant across
the pack split. The linked capability binary contains the same seven functions
with the same frames, so it carries the same 976-byte chain at run time — no
build mode rejects it, and the one that would is the one nobody runs in CI.
(That last step is inference from the frame sizes the two builds report, not a
disassembly of the linked image.)

This does not change §9's conclusion — a build that `main` accepts now fails —
but it does mean the guarantee is narrower than "the budget keeps cg12's nosplit
chains fitting".

---

## 14. Verdict

Everything the brief asked for ran to completion and was watched. Nothing below
is unverified.

| # | check | result |
|---|---|---|
| 1 | `go test -timeout 60m -parallel 10 ./goc/...` | **PASS**, 1245.97s, 0 failures; `TestDeriveClassifiesEveryGenField` **PASS** |
| 2 | capability matrix, both arms, `-v` | **368/368 each**, pass sets identical, 0 FAIL |
| 3 | `make test-unit` | **PASS**, 38 packages |
| 4 | four corpus audits, census twice | **PASS** ×2 real runs, byte-identical output |
| 5 | determinism `-O` / unoptimised, parallel-vs-serial | **406/406 reproducible at `-O`**; 405 + 1 build failure unoptimised; `TestParallelBackendIsByteIdenticalToSerial` PASS |
| 6 | GC reducer 20× × 2 `GOGC` × (branch, `main`) | **80 runs, 0 failures** |
| 7 | crash loops (flate 250×2, p256 100, lock_osthread 400) | **1000 runs, 0 failures** |
| 8 | byte-identity vs `main` | branch 1: **406/406 identical**; merged wave: 0/406 (branch 3 changes runtime code, as designed) |
| 9 | compile time | **4.488x → 1.987x** (branch 1) / **2.043x** (wave). Peak RSS **unchanged** — keep the 5 GiB cap |
| 10 | `make bench-perf` / `make bench-crypto` | **BOTH PASS** on branch *and* on `main` control; control ratio 0.9247–0.9272 vs 0.9260 |
| — | 10 regenerated baselines | 8 **identical**; 2 timing files moved within their own noise |
| — | `main` already over its nosplit reserve | **CONFIRMED**: 21 chains at `-O`, 48 unoptimised, deepest 3024, `nextFree` 184 over before inlining |
| — | debt register is a floor, not a licence | **CONFIRMED** in four places; one structural caveat, not firing |
| — | one thing that does not fit | **`goc` without `-O` can no longer build a program that uses `os/exec`** (§9) |

### SAFE TO MERGE TO MAIN: **NOT SAFE — for one reason, with a one-line fix**

Everything measurable about this wave is good. Branch 1 makes the compiler 2.2x
cheaper and emits byte-identical code for all 406 corpus programs. Branch 2
changes nothing, provably. Branch 3's budget is soundly built, its register is
genuinely a floor, its inlining provably grows no over-limit chain, and it costs
2.8% of compile time and nothing at run time. Every baseline in the tree
regenerates identically; every audit, every capability, every determinism round,
1000 crash-loop runs and both timing benchmarks are green.

The one thing that stops it is §9: **the merged tree refuses to compile
`goc/testdata/stdlib_os_exec_echo.go` without `-O`, and `main` compiles it.**
The chain it rejects is real and pre-existing — that is the wave's own finding,
confirmed here — but the consequence is that a program shape as ordinary as
`os/exec` cannot be built unoptimised, and nothing in `make test` catches it,
because the only guard that builds all 406 programs to an object is
`scripts/determinism-check.sh -corpus`, which is not part of the suite.

Merge as soon as one of these is done, and neither needs new measurement:

1. add `"syscall_runtime_AfterForkInChild": 976` to `arm64/nosplit_debt.go`
   (accepting the register's own premise for a chain the 22-configuration recipe
   could not see), **and** widen the register's generation recipe so a corpus
   whole-program sweep is one of its inputs; or
2. shrink `runtime_sigaction`'s 464-byte frame, which brings the chain to 512
   and is where the real fix lives.

Either way, add the unoptimised whole-program corpus build to CI, since it is
the only thing that runs the budget over every program.

---

# NOSPLIT REGISTER RECIPE — `ccwork/nosplit-register-recipe` off `integration/wave10-gate` (8e7642f)

Job started. Host: aarch64 Linux. Everything below is measured by this job unless
marked UNVERIFIED.

## 0. Reproduced the blocker, first thing

    $ CG12_NOCACHE=1 goc -o out goc/testdata/stdlib_os_exec_echo.go
    goc: nosplit frame budget: nosplit stack overflow:
        syscall_runtime_AfterForkInChild -> runtime_clearSignalHandlers ->
        runtime_setsig -> runtime_sigaction -> runtime_throw ->
        runtime_fatalthrow -> runtime_systemstack
      976 bytes of nosplit frames against a 920-byte limit, 56 over
    exit=1

Identical to the gate's §9. Work proceeds in the order the brief sets: recipe
first, chain second, floor-not-licence third.

## 1. Why twenty-two configurations missed it — **the shape of the configurations, not their number**

Two properties decide whether the budget can see a chain at all, and the original
recipe held both of them fixed.

**(a) The budget is a per-module walk, so a chain is only measured where every
one of its frames is in the same module.** `goc build-runtime` compiles the
runtime as a module of its own and prunes it to what is reachable from the
runtime's own roots. `syscall.runtime_AfterForkInChild` is defined *in the
runtime* (`stdlib/src/runtime/proc.go:5228`) but is reachable only through the
`//go:linkname` that package `syscall` uses, and `runtime.clearSignalHandlers`
is called from nothing else — so both are dead code in a runtime pack and both
are dropped. Measured, not inferred, on the runtime-only pack this job built:

| symbol | occurrences in `p0.gocrt` |
|---|---:|
| `syscall_runtime_AfterForkInChild` | **0** |
| `clearSignalHandlers` | **0** |
| `sigaction` | 16 |
| `setsig` | 7 |

The pack contains the bottom four frames of the chain and neither of the top
two. Its deepest chain through `runtime_sigaction` is 832, which passes. **All
fourteen pack configurations were structurally incapable of finding this entry**
— not because the pack roots were the wrong seven, but because every one of them
is a runtime-shaped module and the chain's root is above the runtime's own
reachability. Adding pack roots would not have helped; a richer root carries
`syscall`, but nothing in `net/http` calls `os.StartProcess`, so the fork path is
still dead code.

**(b) The whole-program arm was a four-program sample of a 406-program corpus.**
`runtime_lock_osthread`, `runtime_gc_concurrent_mark`,
`stdlib_http_tls_client_server`, `stdlib_compress_zlib_lzw` — none imports
`os/exec`, and exactly **1 of the 406 corpus programs does**
(`stdlib_os_exec_echo.go`). Building those four at two optimisation levels is
eight configurations that sample one point of the space that actually matters,
which is *which functions are in the module*.

So "22 configurations" reads like breadth and is not: it is 2 module shapes x
(7 pack roots or 4 programs) x 2 optimisation levels, and the axis that decides
visibility — the program — has four values. The ±`-O` axis, which the register's
comment spends most of its words on, changes the *heights* but cannot change
*which chains exist*.


## 3. The structural caveat: **it was firing.** Closed.

The gate flagged `opt.InlineIntoNoSplitCallers` computing its budget once and then
spending `Headroom(name) - 16` per caller without recomputing, and recorded it as
"not observed firing". It fires. Measured on this tree by comparing the pass's
own report before and after the fix, `goc/testdata/runtime_lock_osthread.go -O`:

| | accepted | rejected after measuring | no headroom |
|---|---:|---:|---:|
| before (budget spent per function) | **106** | 1 | 201 |
| after (growth charged to the chain) | **101** | 4 | 203 |

Five callers had been granted slack that another function on the same chain had
already spent -- `runtime_dieFromSignal` (+64 of 72 "allowed"),
`runtime_needAndBindM` (+80 of 88), `runtime_fatalpanic`, and two more. Same
result on `stdlib_http_tls_client_server -O`: 118 accepted becomes 113.
**Unoptimised images are byte-identical before and after** (the pass finds almost
nothing to inline there), so this is an `-O`-only change.

It had not yet produced an overflow: the tightest under-limit chain on that
program is 8 bytes of headroom (`runtime_write`) both before and after, and no
non-recorded chain reaches the reserve in either build. What the double spend was
eating is margin the budget believed it still had.

**The fix.** `opt.FrameBudget` gains `Charge(name, bytes)`; the pass calls it when
it keeps a growth, and `arm64.noSplitFrameBudget` subtracts those bytes from the
headroom of every function that shares a nosplit chain with the one that grew.
Two functions share a chain exactly when one can reach the other with no stack
check in between, so the set is the function plus its nosplit ancestors plus its
nosplit descendants, over the same edges the walk itself follows
(`stackcheck.walker.calleesOf`: a splittable callee ends the chain, an indirect
call is assumed to check its own stack). Any two functions on one path are
related by ancestry along that path, so ancestors-plus-descendants is exactly the
sharing relation -- not wider, which would cost inlining the tree can afford, and
not narrower, which would leave the hole open.

A shrink is not credited back. Most accepted callers do shrink, but the headroom
map was measured before any of this and handing out bytes it never counted is the
same mistake in the other direction.

New tests, and the negative control that says they bind:

- `opt.TestNoSplitInlineDoesNotSpendOneChainsHeadroomTwice` -- two nosplit
  callers on one chain with room for one growth. With the `Charge` call removed
  it fails with "accepted 2 callers on one chain with room for one", both having
  grown 64 bytes against 72 of shared headroom. **Verified by running it with the
  fix backed out.**
- `opt.TestNoSplitInlineChargesTheWholeChain` -- the charge follows the chain and
  the second caller's allowance is what the first left. Also fails backed out.
- `arm64.TestChainSharing*` (5) -- the relation reaches up and down, stops at a
  splittable function, keeps separate chains apart, ignores undefined callees,
  terminates on a cycle.
- `arm64.TestChargeSpendsTheChainOnceAndOnlyItsOwnChain`,
  `TestChargeLeavesAnUnmeasuredFunctionWithNothing`.

All 9 pass. This does not weaken the floor: `Charge` only ever *reduces* what
`Headroom` returns, it is not consulted by `stackcheck` at all, and the register
is still invisible to the inliner's budget (still `Config{Limit, CallSize}` with
no `Recorded` field).

## 2. The widened recipe, and what it finds — **exactly one entry**

`analysis/nosplitdebt` (new), driven by `scripts/nosplit-debt-regen.sh` (new).
It sweeps the product of both axes the original recipe held fixed:

| arm | module shape | configurations |
|---|---|---:|
| `pack` | `goc build-runtime` per capability-matrix root, ±`-O` | **14** |
| `whole` | `goc prog.go` — runtime, stdlib and program in one module, ±`-O` | **812** (406 x 2) |
| `split` | `goc -runtime packs prog.go` — the boundary the matrix and the pack cache use, ±`-O` | **812** |
| | | **1638** |

Result, on the tree with the double-spend fix in:

    configurations: 1638 total  pack=14  whole=812  split=812
    outcomes: 1638 measured, 1 rejected by the budget (heights still valid), 0 failed to compile
    register: committed=50  original-recipe=50  widened-recipe=51

- **Every one of the 1638 configurations compiled.** The only non-zero exit in the
  whole sweep is the known one, `whole stdlib_os_exec_echo.go`, and a build the
  budget rejects still prints its heights from the finished walk before it fails
  — which is what lets the recipe find the entry that fixes it.
- **The original 22 configurations, re-run inside this sweep, reproduce the
  committed register exactly**: same 50 names, same 50 heights. The committed
  file was right about everything it could see.
- **The widened recipe finds exactly one entry the original did not**:
  `syscall_runtime_AfterForkInChild` at 976, first seen at
  `whole stdlib_os_exec_echo.go`. Nothing raised, nothing lowered, nothing
  removed.

**So it is one, not twenty.** That is worth saying plainly: 406 whole programs
and 406 pack-split builds at two optimisation levels — 1616 configurations the
original recipe never ran — turn up a single chain. The blind spot was narrow.
It was also exactly wide enough to block a merge, and it was invisible to
everything in `make test`.

Two design points in the driver, both load-bearing:

- **It runs the shipping compiler at the real 920-byte limit**, not with
  `GOC_NOSPLIT_LIMIT` raised. The raised limit is the obvious way to keep a
  measurement run from failing, and it is wrong here:
  `opt.InlineIntoNoSplitCallers` sizes its allowance from the same limit, so a
  run with the limit raised inlines into nosplit callers far past what any
  shipping build does and reports frames nobody ships.
- **Running with the register in place is not circular.** The inliner's budget is
  built with `Config{Limit, CallSize}` and no `Recorded` field
  (`arm64/nosplit_measure.go`), so the register cannot change a single frame; it
  only decides whether the finished walk is accepted. Heights are therefore
  identical with and without any given entry, which is why one pass converges.

### The `split` arm earns its place, but it found nothing

By construction a pack-split build sees a subset of a whole-program build's
chains — calls into the pack are undefined and end the walk. It is not *provably*
dominated, because the inliner's headroom is computed per module and a program
module on its own offers its nosplit functions a larger allowance, so a frame can
be bigger there. Over 812 configurations that never happened: the split arm
contributed no maximum. It is kept because it is the boundary the capability
matrix and the pack cache actually compile at, and because §9a of the gate report
is right that the *linked* image carries a chain no single module walk measures.


## 4. The chain: **recorded as pre-existing debt, not shrunk.** Here is what the shrink was worth.

I preferred the shrink and went looking for it. It is not there, and the
measurement says so more clearly than the argument does.

The chain, with the deepest edge out of each frame:

    80   syscall_runtime_AfterForkInChild
    64   runtime_clearSignalHandlers
    128  runtime_setsig
    464  runtime_sigaction        <- the one the gate named
    128  runtime_throw
    96   runtime_fatalthrow
    16   runtime_systemstack      = 976 against 920, 56 over

Read from the compiler rather than the source (a temporary `Calls` dump on
`GOC_DEBUG_NOSPLIT=frames`, not committed):

    runtime_sigaction  frame 464  calls=[sysSigaction fixSigactionForCgo callCgoSigaction systemstack memcpy throw]
    runtime_sysSigaction        frame  96  calls=[rt_sigaction systemstack]
    runtime_sysSigaction_func_548_16  frame 96  (address-taken)

So upstream's own overflow workaround is intact — `sysSigaction`'s
`systemstack(func(){ throw("sigaction failed") })` is a real closure reached
through `systemstack`, and the walk ends the chain at it. `runtime.sigaction`
reaches `throw` on a *different*, direct edge, and both of its branches sit above
the same 240-byte `throw -> fatalthrow -> systemstack` tail.

**The candidate shrink, built and measured.** Extract the `_cgo_sigaction` branch
into its own `//go:nosplit //go:nowritebarrierrec` function — semantics
preserved, the smallest contained change that moves those locals out of the
frame:

    before   ... setsig(128) -> sigaction(464) -> throw(128) -> fatalthrow(96) -> systemstack(16)   = 976
    after    ... setsig(128) -> sigaction(368) -> sigactionViaCgo(192) -> sysSigaction(96) -> systemstack(16) = 944

**944 against 920. Still 24 over, and the build still fails.** Moving code
between two functions on one chain does not shorten the chain; it splits one
frame into two that stack.

The number that decides it is the 368. After the extraction, `runtime.sigaction`
is a six-line function — four constant-folded sanitizer branches and one
`if/else` that calls one of two helpers — and cg12 still lays it out at **368
bytes** without `-O`. That is not a frame with 56 bytes of fat in it. It is what
this backend costs for a function of that shape when mem2reg has not run, which
is exactly what the register's own comment says about the other fifty entries:
"Go's linker accepts them because gc's frames are a fraction of cg12's". The same
source at `-O` compiles to a chain that fits, and always did.

So the shrink was not worth it, for four reasons in descending order of weight:

1. **It does not work.** The contained version measures 944 and still fails. To
   get under 920 I would have to keep carving up `runtime.sigaction`,
   `runtime.setsig` and the `throw` path with no principled stopping point.
2. **It would be fixing the wrong layer.** The excess exists only in the
   unoptimised build, and it is a property of cg12's frame layout, not of Go's
   source. Editing vendored upstream runtime code to compensate would have to be
   repeated for the other 50 entries — nobody proposes that, and it is the same
   work each time.
3. **It is the fork-in-child signal path.** `syscall.runtime_AfterForkInChild`
   runs in a process that may still share its address space with its parent;
   `clearSignalHandlers`, `setsig` and `sigaction` are all
   `//go:nowritebarrierrec` for that reason. Divergence from upstream Go here
   buys 32 bytes and costs a permanent local patch in the most delicate path in
   the runtime.
4. The blast radius is small but not zero — `cgo_sigaction.go` appears in none of
   the four audit baselines, checked — and it changes runtime code for every
   program in the tree for a 24-byte shortfall.

**Recorded**, therefore: one entry, `"syscall_runtime_AfterForkInChild": 976`,
generated by the widened recipe rather than typed in. The register goes 50 -> 51.
It is a real chain that can overflow in production on the same terms as the other
fifty, and the honest place to put that is the list that says so by name.


## 5. The two timing baselines — **reverted**

`goc/testdata/perf_suite_baseline.txt` and
`goc/testdata/crypto_signing_bench_baseline.txt` are restored to their values at
`2438919` (the merge point, before the gate's `bc80ec2`). `git diff` against
`2438919` for both files is now empty. The gate's own recommendation, and the
right one: nothing in either file moved by more than its own noise, and
re-baselining a timing file on noise loses the information it was cut for on an
idle box. The other eight regenerated baselines were byte-identical, so nothing
else is affected either way.


## 6. Guards

### 6a. Determinism, unoptimised corpus — **406/406, 0 failed** (this is the one that matters)

    scripts/determinism-check.sh -corpus -j 30      (406 programs x 4 rounds)
    reproducible=406 varying=0 failed=0 of 406 over 4 rounds
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    exit 0

The gate's run was `reproducible=405 varying=0 failed=1`, the failure being
`stdlib_os_exec_echo.go`. **The failure is gone and nothing else moved.**


### 6b. Crash loops and GC reducers — **1080 runs, 0 failures**

Run on an idle box (load average under 1), 8 at a time. Both bench programs
`panic` on a wrong answer, so these are correctness loops and not only crash
loops.

| loop | runs | `GOGC` | failures |
|---|---:|---|---:|
| `placement_bench/flate` `-O` | 250 | 100 | **0** |
| `placement_bench/flate` `-O` | 250 | 10 | **0** |
| `placement_bench/p256` `-O` | 100 | 10 | **0** |
| `runtime_lock_osthread` `-O` | 400 | 100 | **0** |
| GC reducer `runtime_gc_promoted_local_root` `-O` | 20 | 100 | **0** |
| GC reducer `runtime_gc_promoted_local_root` `-O` | 20 | 10 | **0** |
| GC reducer `runtime_gc_type_mask_padding` `-O` | 20 | 100 | **0** |
| GC reducer `runtime_gc_type_mask_padding` `-O` | 20 | 10 | **0** |

### 6c. Package tests for everything this branch touches — **PASS**

    ok  github.com/evanphx/cg12/opt         0.937s
    ok  github.com/evanphx/cg12/stackcheck  0.005s
    ok  github.com/evanphx/cg12/arm64       9.966s

`arm64` carries `TestParallelBackendIsByteIdenticalToSerial` and the seven new
sharing/charging tests; `opt` carries the two new double-spend tests and
`TestNoSplitInlineRefusesACallerAlreadyOverTheReserve`; `stackcheck` carries
`TestRecordedDebtAllowsItsOwnHeightAndNotOneByteMore` and
`TestRecordedDebtDoesNotWidenHeadroom`. (`make test-unit` and
`go test ./goc/...` were not run, as instructed.)


## 7. The register is still a floor, not a licence — re-verified, including for the new entry

The gate's four properties, re-checked on this tree, and none of them touched by
the widened recipe or by `Charge`:

1. `stackcheck.Report.Headroom` is `w.headroom(config.Limit)`
   (`stackcheck.go:381`) and `walker.headroom` subtracts from the passed limit
   only. `Config.Recorded` is read in exactly one place in the whole package,
   `limitFor` (`:426`), reached only from the over-limit test. **Unchanged** --
   this branch does not modify `stackcheck` at all.
2. The inliner's budget is still built with `Config{Limit, CallSize}` and **no
   `Recorded` field** (`arm64/nosplit_measure.go`). `Charge` only ever *subtracts*
   from what `Headroom` returns, so it cannot open a door here.
3. `Recorded` is still attached only when `limit == noSplitLimit`
   (`arm64/nosplit.go:219`), so a test that lowers the limit cannot be handed
   entries that outrank it.
4. Recorded height passes, recorded+1 fails.

Points 3 and 4 checked live on the **new** entry rather than argued. Setting it to
975 -- one byte under the height it actually is -- and rebuilding:

    goc: nosplit frame budget: nosplit stack overflow: syscall_runtime_AfterForkInChild -> ...
      976 bytes of nosplit frames against a 975-byte limit, 1 over
    exit status 1

And every member of the new chain has negative headroom, so no pass can grow any
of them (`GOC_DEBUG_NOSPLIT=headroom`, unoptimised):

    -56  syscall_runtime_AfterForkInChild
    -56  runtime_clearSignalHandlers
   -488  runtime_setsig
   -488  runtime_sigaction
  -2104  runtime_throw / runtime_fatalthrow / runtime_systemstack

The entry buys the chain the right to be exactly as deep as it already is, and
nothing else.

One more check the widened recipe makes possible and the old one did not: the
register was regenerated twice, once with the double-spend fix and once without,
over all 1638 configurations each time. **Both runs produce the same 51 entries at
the same 51 heights.** The `Charge` change reduces inlining at `-O` and moves no
recorded chain, which is the statement that matters -- a tightening that changed
the floor would be a floor that was never one.


### 6d. Determinism, `-O` corpus — **406/406, 0 failed**

    scripts/determinism-check.sh -corpus -j 30 -O   (406 programs x 4 rounds)
    failed to compile: 0
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    reproducible=406 varying=0 failed=0 of 406 over 4 rounds
    exit 0

Zero non-determinism in either arm, and now zero build failures in either arm.
The `Charge` change moves generated code at `-O` (§3) and the `-O` corpus is
still byte-reproducible across four rounds, including the layout-residue check.


### 6e. Capability matrix, both arms, `-v` — **368/368 each, PASS SETS IDENTICAL**

| arm | exit | subtests PASS | subtests FAIL | `EXPECTED FAILURE` | `KNOWN GAP NOW PASSES` | wall |
|---|---|---:|---:|---:|---:|---:|
| default | **0** | **368** | **0** | 1 | 0 | 71.1s |
| `-runtime-opt` | **0** | **368** | **0** | 1 | 0 | 140.1s |

The two `--- PASS` name sets were diffed against each other: **identical, 368
names, no difference in either direction**. The one `EXPECTED FAILURE` is
`runtime_panic_print_string.go` in both arms, matching the gate.

Note that `stdlib_os_exec_echo.go` is one of the 368 in each arm and passed
before this branch too -- the pack split hides the chain from the per-module walk
(gate §9a). It is not the capability matrix that establishes the fix; §6a and §8
are.


### 6f. The four corpus audits, check mode — **PASS, two real corpus executions**

    go test ./goc/ -run '^(TestAllocationCensus|TestFrameEscapeAudit|TestLoopAliasAudit|TestEscapeShadowPlacement)$' -count=1
    --- PASS: TestAllocationCensus      (182.00s)
    --- PASS: TestEscapeShadowPlacement (0.00s)
    --- PASS: TestFrameEscapeAudit      (0.00s)
    --- PASS: TestLoopAliasAudit        (0.00s)
    ok  github.com/evanphx/cg12/goc  182.341s      exit 0

The three at 0.00s share the corpus compile with whichever audit runs first, so
that alone would not say they did anything. Re-run without the census, with
`-count=1`, so a different one pays for the corpus:

    --- PASS: TestEscapeShadowPlacement (182.05s)
    --- PASS: TestFrameEscapeAudit      (0.00s)
    --- PASS: TestLoopAliasAudit        (0.00s)
    ok  github.com/evanphx/cg12/goc  182.422s      exit 0

Two real corpus executions, four audits green in both, against the committed
baselines -- which this branch does not touch. (`go test ./goc/...` in full was
not run, as instructed; these are the four named audits only.)

## 8. `goc/testdata/stdlib_os_exec_echo.go` — **builds and runs, both ways**

    CG12_NOCACHE=1 goc     -o out goc/testdata/stdlib_os_exec_echo.go   exit 0   (10,129,264 bytes)
    CG12_NOCACHE=1 goc -O  -o out goc/testdata/stdlib_os_exec_echo.go   exit 0   (13,082,352 bytes)

and both binaries run to exit 0. `CG12_NOCACHE=1` matters: it is what forces the
whole-program build, which is the only mode that sees all seven frames of the
chain at once.


## 9. Verdict

Everything below ran to completion on this branch's tree and was watched. Nothing
is unverified.

| # | check | required | result |
|---|---|---|---|
| 1 | why 22 configurations missed it | explain | **module shape, not count**: 14 pack builds structurally cannot see a chain rooted above the runtime's own reachability (measured: 0 occurrences of `syscall_runtime_AfterForkInChild` and `clearSignalHandlers` in the runtime-only pack); the whole-program arm was 4 of 406 programs, and exactly 1 of the 406 imports `os/exec` |
| 2 | widened recipe | every buildable configuration | `analysis/nosplitdebt` + `scripts/nosplit-debt-regen.sh`: **1638 builds** (14 pack, 812 whole-program, 812 pack-split, each ±`-O`), all compiling, ~10 min |
| 3 | what it finds that the old one did not | report the count | **exactly 1** — `syscall_runtime_AfterForkInChild: 976`. The original 22 configurations, re-run inside the sweep, reproduce the committed 50 **exactly** |
| 4 | shrink or record | prefer the shrink | **recorded.** The shrink was built and measured: extracting the `_cgo_sigaction` branch takes 976 -> **944**, still over. Splitting one frame on a chain into two that stack does not shorten the chain, and what is left is a six-line `runtime.sigaction` at 368 bytes without mem2reg |
| 5 | register still a floor | 4 properties intact | **intact**, re-checked in code, and checked live on the new entry: at 975 the build fails "1 over"; every member of the chain has negative headroom |
| 6 | the structural caveat | close if cheap | **closed, and it was firing** — 106 accepted callers becomes 101 at `-O`. `opt.FrameBudget.Charge`, 9 new tests, negative control run |
| 7 | `stdlib_os_exec_echo.go` | builds ±`-O` | **exit 0 both**, and both binaries run |
| 8 | `determinism-check.sh -corpus`, unoptimised | clean | **reproducible=406 varying=0 failed=0**, exit 0 (gate: 405 + 1 failure) |
| 9 | `determinism-check.sh -corpus -O` | clean | **reproducible=406 varying=0 failed=0**, exit 0, 0 layout residues |
| 10 | capability matrix, both arms | 368/368 | **368/368 each**, 0 FAIL, pass sets **identical** |
| 11 | GC reducer | 0/20 at `GOGC=10` and default | **0/20 x 2 programs x 2 `GOGC`** |
| 12 | crash loops | flate >=250, p256 >=100 @`GOGC=10`, lock_osthread >=400 | **1080 runs, 0 failures** |
| 13 | four corpus audits | pass | **PASS**, two real corpus executions |
| 14 | two timing baselines | revert | **reverted** to `2438919`; `git diff 2438919` empty for both |
| — | `opt`, `stackcheck`, `arm64` package tests | pass | **ok** (incl. `TestParallelBackendIsByteIdenticalToSerial`) |

### What is left, and is not this branch's to fix

- **The budget is a per-module check and its verdict is not invariant across the
  pack split** (gate §9a). The whole-program arm of the recipe now covers every
  corpus program, so the register describes the union of what any single module
  can contain -- but the *linked* image of a pack-split build carries frames from
  two modules, and no walk measures that composition. The split arm found no
  maximum over 812 configurations, so nothing is known to be wrong; the guarantee
  is just narrower than it reads.
- **The unoptimised whole-program corpus build is still not part of `make test`.**
  It is now one `scripts/nosplit-debt-regen.sh` away from being runnable in ten
  minutes, and `scripts/determinism-check.sh -corpus` is still the only thing in
  the tree that drives every program to a written object. The gate's
  recommendation to put one of them in CI stands.
- **`stackSmallReserved` and `goStackPrologue` are still coupled by a comment and
  not by a test** (gate §1). Untouched here.

### Merge

The one thing the gate found that stopped the merge is fixed, by the route it
asked for: the recipe first, then the entry. `goc` without `-O` compiles every
one of the 406 corpus programs again, and every guard the brief named is green.


# A pre-flight for the timing suites

Branch `ccwork/bench-preflight-check`, off `main` (`c6cdd48`). Test-harness code
only; no compiler behaviour changed and neither timing baseline was re-cut.

## The problem

`make bench-perf` and `make bench-crypto` both already refuse a contaminated
run -- the noise-growth ceiling in `comparePerfBench` and the precision ceiling
in `checkCryptoBenchInstrument` -- and both say the right thing: the box cannot
support the tolerances the run is about to be judged against. They say it after
the full measurement. That is eleven minutes for the perf suite and about eight
for crypto, and during this effort it was paid repeatedly: two jobs ended up
waiting for a quiet window and one abandoned its measurement.

The information was available in the first two seconds and was collected in the
last.

## What was built

`goc/benchpreflight_test.go`: `requireQuietBoxBefore`, called by both suites
before a byte is compiled -- ahead of the builds as well as the timing, since
the builds are a minute of their own and are equally wasted.

It measures contention **on the core the run will pin to**, because that is what
these suites are exposed to: every timed run is `taskset -c N binary`, so
sixty-three busy cores and a free core N cost the instrument far less than one
busy core N. Three readings from two independent sources, over one window of
about 1.4 s:

| reading | source | idle box | one competitor pinned to the same core | bound |
| --- | --- | --- | --- | --- |
| share of the core the calibration burst got | `wait4` rusage / wall clock | 99.5% | 50.2% | floor 90% |
| the core's time that went to somebody else | `/proc/stat` per-CPU counters | 0.0% | 49.8% | ceiling 10% |
| burst-to-burst wall-time spread | 6 bursts | 0.3% | 27.2% (intermittent load) | ceiling 12% |

The burst is a child process -- this test binary re-executed, so no `go build` in
the budget -- launched through **the suite's own pin prefix**, so it is pinned by
the same mechanism to the same core. Its work is half register arithmetic and
half a dependent chase through 8 MiB, so the spread reading is sensitive to a
busy memory system as well as to a busy core. Its size is fitted from two short
probes, so 200 ms per burst holds on a machine faster or slower than this one.

A measured burst rather than a load average, per this tree's standing
preference, and because the load average answers a different question: the whole
box over the last minute. A run pinned to a quiet core on a loaded box is fine
and a run sharing its core with one spinner on an idle box is not, and a load
average cannot tell those apart. The count of busy cores box-wide is reported
for context and deliberately not gated on.

**Override:** `GOC_BENCH_PREFLIGHT=off`, documented in the Makefile and in the
header of both committed baselines. The run then does exactly what it did before
this check existed.

**Not measured:** memory bandwidth and LLC pressure from cores this run is not
on. Gating on those needs a throughput number against a per-machine reference,
which is a second baseline with all of the first one's problems. The end-of-run
ceilings are unchanged and remain the backstop, both for that and for a box that
goes busy at minute two.

## Verified by making it fire

The tree's standing rule for a new check. Every figure below is from this box
(aarch64, 64 cores, Neoverse-N1, go1.26.1), with the load applied deliberately.

**It refuses on a loaded box.** One `bash` spinner pinned to core 62, which is
the core `make bench-perf` picks:

    $ taskset -c 62 bash -c 'while :; do :; done' &
    $ make bench-perf
      the calibration burst got 50.1% of core 62 (floor 90%) -- it is sharing that core with something
      core 62 spent 49.9% of the window on work that was not this run's (ceiling 10%), per /proc/stat
    --- FAIL: TestPerformanceSuite (1.38s)
    real  0m2.621s

and the same with a spinner on core 63, which is the core `make bench-crypto`
picks: refused in 2.5 s, `the calibration burst got 49.9% of core 63`. Two point
five seconds against eleven minutes and against a couple of minutes.

**The third reading fires on its own.** A 5%-duty-cycle competitor (a 40 ms spin
every 640 ms) on core 40 leaves both mean readings inside their bounds, and the
spread catches it alone:

      the burst's wall time spread 27.2% across 6 repetitions (ceiling 12%) --
      the speed of this core is changing underneath it

**It is about the core and not the box.** With both spinners still running,
`GOC_PERF_CORE=40 make bench-perf` passes the pre-flight (`core 40 is quiet
enough to measure on ... 99.5% ... 0.0% ... 0.4%`, box-wide 3 of 64 cores busy)
and measures normally.

**The override does what it says, and shows what it costs.** On the same loaded
box, `GOC_BENCH_PREFLIGHT=off` proceeds, and one program at four repetitions
takes 60 s to arrive at the noise-growth ceiling -- `one-repetition spread 0.11%
in the baseline, 2.44% in this run`. At the full eleven programs and nine
repetitions that is the eleven minutes this check exists to save.

**It passes on a quiet box and the suites run to completion.** Against the
committed baselines, unchanged, neither re-cut:

    make bench-crypto   PASS  102.45s   pre-flight: core 63, 99.5% / 0.0% / 0.3%, 1.3 s
    make bench-perf     PASS  558.69s   pre-flight: core 62, 99.5% / 0.0% / 0.4%, 1.3 s

Three full `make bench-perf` runs were made on the final tree and two were
green. The red one failed at the noise-growth ceiling on `text/format-append`,
the loudest row in the suite: one-repetition spread 4.27% in the baseline and
15.00% in that run, with the row's *null* arm going 3.40% -> 14.43% beside it.
The null is the same goc binary divided by itself, so a 14% null is the machine
and not the compiler, and that row sits closest to the ceiling on a good day
(5.55% in the green run before it, 8.70% in the green run after). It is the case
this pre-flight explicitly cannot see -- box-wide activity that arrives after
the first two seconds -- and it is the case the end-of-run ceiling is for. It
did its job.

## Guards

    TestAllocationCensus                            PASS  183.07s, baseline reproduced (no diff)
    TestCompilingTheSameSourceTwiceGivesTheSameModule PASS   4.33s
    TestParallelBackendIsByteIdenticalToSerial      PASS    0.21s
    gofmt -l goc/ && go vet ./goc/                  clean

The change is test-harness only: a new `_test.go` file, the two suites' call
into it, prose in the Makefile and in both baseline headers. No compiler code
was touched and no baseline number was rewritten. `goc/testdata/alloc_census_baseline.txt`,
`perf_suite_baseline.txt` and `crypto_signing_bench_baseline.txt` have no
changed rows -- the only diff in the last two is the paragraph documenting the
pre-flight and the override.

One incidental cleanup: `highestAllowedCPU` and `allowedCPUs` each carried their
own copy of the Cpus_allowed_list parser and the pre-flight needed a third, for
`taskset`'s argument. All three now go through one `parseCPUList`, exercised by
both suites in every run above.

## Where the same reasoning applies, and where it stops

Not widened; recorded so the next person does not have to rediscover it.

  - `scripts/cruby-diff.sh` step 5 -- "run the benchmark set on both builds and
    compare against a recorded baseline" -- is a timing gate on a much longer
    run. It is a stub today. When it is filled in it should call this
    pre-flight, and it is the one place where the argument transfers whole.
  - `scripts/matrix-timing.sh` times the capability matrix, up to an hour, and a
    busy box moves its numbers. The reasoning transfers but the instrument does
    not: that measurement is whole-box and many-core with no committed baseline,
    so what it would want is a box-wide idleness check rather than a reading of
    one pinned core.
  - `TestEscapeSummaryCost` prints compile timings that are equally
    box-dependent, but it only reports them; there is no gate to protect.
  - `make test-goc-status`, `test-goc-status-opt` and `test-goc-coverage` are
    long, and they count things rather than timing them. A busy box costs them
    wall clock and not correctness. Not candidates.

One thing noticed and not changed: `crypto_signing_bench_baseline.txt` says the
run takes "about eight minutes". Measured twice today on an idle box it is
1m43s. The pre-flight's own message quotes the measured figure rather than the
committed one.

# VERIFICATION TURNAROUND — `ccwork/verification-turnaround` off `main` (76069d9)

Host: aarch64 Linux, 64 cores, 250 GiB total / 243 GiB available, go1.26.1,
exclusive. Everything below was run and watched by this job unless marked
UNVERIFIED. Sections are appended as each result lands.

## 0. The brief's diagnosis is half right, and the half that is wrong matters

The brief says every gate ran `go test -parallel 10 ./goc/...`, that the 10 came
from an 8-core laptop, and that raising it is the first win. The conclusion — the
box is idle — is **correct and worse than stated**. The mechanism is **not**.

`./goc/...` is a **single package** (`github.com/evanphx/cg12/goc`, 349 top-level
tests, 45 files, 19,101 lines of test) and it contains **zero calls to
`t.Parallel()`**:

    $ go list ./goc/...
    github.com/evanphx/cg12/goc
    $ grep -rn '\.Parallel()' goc/ | wc -l
    0

`go test -parallel N` bounds *tests that call `t.Parallel`*. With no test opting
in, **`-parallel` is inert**: 10 and 64 are the same command. Every gate that
"ran the corpus at -parallel 10" was running it at -parallel 1, and every gate
that had instead been told to pass 48 would have measured exactly the same wall
clock. Raising the number alone buys nothing.

What the box was actually doing during the baseline run, sampled from
`/proc/stat` every 15 s:

    busy 4.5% of 64 cores = 2.9 cores

**2.9 of 64 cores for twenty-one minutes.** The residual parallelism is inside a
single compile (`arm64` backend workers, GOMAXPROCS-sized) and inside the three
or four audit tests that run their own pools; the 349 tests themselves are a
serial queue.

So the fix is not "change 10 to 48". It is "make the suite parallel at all, then
size the cap by measurement" — which is what the rest of this report does.

## 1. Corpus suite, before — 1121 s at 4.0 of 64 cores

Measured by this job on `main` (76069d9), exactly the command every gate was
briefed with, plus `-v` for a per-test profile:

    /usr/bin/time -v go test -timeout 60m -parallel 10 -v ./goc/...

    ok  github.com/evanphx/cg12/goc  1121.146s      exit 0
    341 PASS, 8 SKIP, 0 FAIL

    Elapsed (wall clock) time            18:57.13
    Percent of CPU this job got          396%
    Maximum resident set size            13,627,420 kB  (13.0 GiB)

**396% is 4.0 of 64 cores**, and that agrees with the independent `/proc/stat`
sampling above (2.9 cores averaged over the window; 4.0 is the process's own
figure over its whole life, which includes the audit block's 8-worker pool).
Peak RSS 13.0 GiB out of 243 available — **5%**.

This is the number to beat, and it is worth being precise about what it says:
the machine was 94% idle and had 95% of its memory free for nineteen minutes,
and no setting the caller could have passed would have changed that.

## 2. THE FINDING: the goc suite is not safe to run in parallel as it stands

At `-parallel 32` the suite runs in **4:01 instead of 18:57** — and **fails**:

    Percent of CPU this job got     2121%   (21.2 of 64 cores, up from 4.0)
    Elapsed (wall clock) time       4:01.12
    Maximum resident set size       13,311,592 kB (12.7 GiB — no higher than serial)
    FAIL                            exit 1

Three tests, and they are not three unrelated tests:

| test | failure |
|---|---|
| `TestInterfaceConversionsCallTheRuntimeHelpers` | "a `...any` object that stays in the frame should not be built by a call to `runtime.newobject`" |
| `TestFrameEscapeAudit` | 2 new publications, both `main.addressIntoANonRetainingVariadic`, `allocation_counts.go:549:34` |
| `TestAllocationCensus` | new heap sites: `main.forwardVariadicPastAnInterface`, plus a long tail of stdlib sites (`context.cancelCtx.propagateCancel`, `sha256`, `sha512`, `internal/bisect`, `internal/godebug`, `internal/poll`, `internal/strconv`) |

All three are **escape-placement** results, and all three moved in the same
direction — allocations the serial run keeps in a frame are on the heap here.
The same three pass at `-parallel 10`-as-briefed (which is to say, serially): the
baseline run in §1 is 341 PASS / 0 FAIL on this identical tree.

So this is not a slow test timing out and it is not a flaky assertion. **The
compiler's placement decisions depend on what else the process is compiling at
the time.** That is the shared resource, and it is in the compiler, not in the
test.

The prime suspect is the shared source world (`goc/source_world.go`). Compiles in
one process share parsed stdlib packages by pointer — `adopt` copies the map but
not the units, on the stated ground that "they are read-only once the world is
frozen" — and every concurrent compile in a `go test` process draws from the same
frozen world. If any unit is in fact written during a compile, concurrent
compiles interfere and the loser's placement changes. `CG12_NOCACHE=1` is exactly
the switch that turns that sharing off, so it is also the experiment; §3 runs it.

_(experiment and disposition follow)_

## 3. Isolating it: two experiments, one hypothesis refuted

**E1 — the same five tests, alone, at `-parallel 32`.**

    go test -count=1 -parallel 32 ./goc/ -run '^(TestAllocationCensus|TestFrameEscapeAudit|TestLoopAliasAudit|TestEscapeShadowPlacement|TestInterfaceConversionsCallTheRuntimeHelpers)$'
    ok  github.com/evanphx/cg12/goc  201.689s      exit 0

**PASS.** So it is not that these tests are internally racy, and not that the
audit's own 8-worker compile pool is at fault — that pool ran here exactly as it
runs in the failing case. What changes the answer is *other tests compiling in
the same process at the same time*.

**E2 — the whole suite at `-parallel 32` with `CG12_NOCACHE=1`,** which turns off
the shared source world, the hypothesis from §2:

    CG12_NOCACHE=1 go test -count=1 -parallel 32 ./goc/
    --- FAIL: TestFrameEscapeAudit   (207.98s)
    --- FAIL: TestAllocationCensus   (216.81s)
    FAIL  github.com/evanphx/cg12/goc  297.986s   exit 1

**The hypothesis is refuted.** Disabling world sharing does not fix the audits.
It does fix `TestInterfaceConversionsCallTheRuntimeHelpers`, which passed here —
so the shared world is implicated in *that* one and is not the whole story.

I looked for the remaining mechanism and did not find it. Ruled out by reading:
no time-based or step-budget cutoff anywhere in `opt/escape*.go` (which would
have explained the uniformly conservative direction neatly); no `sync.Pool`
anywhere in `goc`, `opt`, `ir`, `lower`, `arm64`; no goroutines in the escape
analysis itself; no package-level mutable state in the compile path beyond the
source world and read-only tables built in `init`. **The root cause is not
isolated, and this report does not claim it is.**

What can be said with the evidence in hand:

* It reproduces. Three tests at `-parallel 32` with world sharing on, two with it
  off, in the same direction both times.
* Every difference is conservative — an allocation the serial run keeps in a
  frame is on the heap. Nothing moved the unsafe way.
* **Nothing in production compiles concurrently inside one process.** `goc`
  compiles one program per process, and `goc compile-batch` — the thing the
  determinism sweep and the capability matrix drive — reads its request stream in
  a plain `for requests.Scan()` loop and compiles them **strictly in sequence**
  within each worker; concurrency there is across worker *processes*
  (`cmd/goc/batch.go`, and the file says so: "a worker outlives its programs").
  The shared world is designed for exactly that serial reuse.

So this is a constraint on how the suite may be parallelised, not a shipped
defect — but it is a real and unexplained property of the compiler, and it is
worth someone's time: an analysis whose answer depends on what else the process
is doing is one whose answer is not a function of its input.

## 4. The disposition: a listed sequential set, enforced

`goc/sequential_tests.txt` names every test that must not run concurrently with
another compile, with a reason tag (`placement`, `setenv`, `timing`). 83 tests:
77 placement, 4 `t.Setenv`, 2 timing. The other 267 call `t.Parallel` as their
first statement.

Go runs every sequential top-level test to completion **before** it resumes any
parallel one, so a listed test is guaranteed a quiet process — precisely the
conditions it runs in today. **No listed test's execution environment changes at
all**, which is why this does not weaken the verification.

The set is deliberately over-broad. It is not the three tests that were observed
to fail; it is every test that reaches a placement assertion by any call path,
computed with a `go/ast` pass that seeds on the placement markers (`newobject`,
`escapes to heap`, `does not escape`, the four audit baselines, …) and then
closes over the call graph, unioned with a coarser per-file grep. 61 from the AST
closure, 69 from the file scan, **81 in the union** — the union, because the cost
of an unnecessary sequential test is seconds and the cost of a missing one is a
flaky gate.

`goc/parallelpolicy_test.go` enforces both directions against that file and runs
in every corpus run: a new test with no `t.Parallel` and no listing fails, and so
does a listed test that acquires a `t.Parallel`. The file is the single source of
truth — `scripts/verify.sh` reads the same two columns to shard the suite.

One more laptop-era constant fixed while here: `compileCorpusForAudits` capped
its compile pool at a flat **8**, which made `TestAllocationCensus` the single
longest test in the tree (183 s of 1121 s) with 56 cores idle. It is now bounded
by `MemAvailable / 4.23 GiB` (the corpus's worst observed compile peak), the same
way the capability matrix bounds its fan-out — right on both machines, passed by
neither caller. `GOC_AUDIT_WORKERS` overrides it.

## 5. Corpus suite, after — 8:15, same pass set, stable across repeats

`go test -timeout 60m -count=1 -parallel 32 -v ./goc/...`, run twice back to back
on the same idle box:

| | before (`-parallel 10`, i.e. serial) | after, run 1 | after, run 2 |
|---|---:|---:|---:|
| wall clock | **18:57.13** | **8:15.56** | **8:26.66** |
| test time reported by `go test` | 1121.146 s | — | — |
| CPU | 396% (4.0 cores) | 1190% (11.9) | 1200% (12.0) |
| peak RSS | 13.0 GiB | 28.3 GiB | 29.8 GiB |
| PASS / SKIP / FAIL | 341 / 8 / 0 | **342 / 8 / 0** | **342 / 8 / 0** |

**2.3x, and the answer is the same answer.** Checked, not assumed:

* run 1's and run 2's `--- PASS` name sets are **byte-identical to each other**.
* against the serial baseline the only difference in either direction is
  `TestEveryTestIsParallelOrListedAsSequential`, the policy test this branch
  adds. 341 → 342.
* the `--- SKIP` sets are identical.

Peak RSS more than doubled — 13.0 → 29.8 GiB — which is the cost of the
concurrency and is **12% of the 243 GiB available**. Memory is nowhere near being
the binding constraint; it is not what stops this going wider.

What stops it going wider is the shape of the work, and the profile says so
exactly. Of the 495 s:

* **350 s is the sequential set** — 83 tests that must run one at a time, run
  before anything else starts.
* **145 s is the parallel set**, and that 145 s is one test:
  `TestSlogAttrInFrameIsNotScannedAsAPointer` takes **144.17 s** on its own. 266
  other tests finish inside its shadow.

So `-parallel` beyond 32 buys nothing here: the critical path is a single test,
not the width of the pool. That is also why §6 splits the suite across processes
rather than raising the number further.

The audit-pool change shows up cleanly in the same profile:
`TestAllocationCensus` **183.12 s → 82.57 s** (2.2x) from unbinding the flat
8-worker cap.

## 6. The capability matrix, measured today in the old shape

Unsharded, one arm after the other, `-count=1`, nothing else on the box — the way
every gate has run it:

| arm | wall | CPU | peak RSS | subtests PASS | FAIL | `EXPECTED FAILURE` | `KNOWN GAP NOW PASSES` |
|---|---:|---:|---:|---:|---:|---:|---:|
| default | **101.18 s** | 2843% | 2.55 GiB | **368** | **0** | 1 | 0 |
| `-O` | **140.82 s** | 2341% | 2.81 GiB | **368** | **0** | 1 | 0 |

Both exit 0. The two `--- PASS` name sets were diffed against each other:
**identical, 368 names, no difference in either direction**. This matches the
wave-10 gate's 102.1 s / 144.4 s on the same box, so nothing on this branch has
moved the matrix.

**GUARD MET: both capability arms 368/368.**

## 7. The full gate, before — 45.97 minutes

The brief's description of a gate is four long things one after another: the
corpus suite, the default arm, the `-O` arm, and a `main` control of each. Priced
from the measurements above, all taken on this box today:

| item | wall |
|---|---:|
| corpus suite, branch | 1137 s (18:57) |
| capability matrix, default arm, branch | 101 s |
| capability matrix, `-O` arm, branch | 141 s |
| corpus suite, `main` control | 1137 s |
| capability matrix, default arm, `main` control | 101 s |
| capability matrix, `-O` arm, `main` control | 141 s |
| **total, run one after another** | **2758 s = 45.97 min** |

This is **composed from measured parts, not one stopwatch**, and it is worth
being explicit about why that is legitimate here and where it is weakest.

* The corpus figure is `main`'s own serial suite, measured by this job in §1.
  Before this branch, the branch arm and the control arm ran *the same serial
  code*, so 1137 s is the honest figure for both.
* The matrix figures are from this branch, which does not touch the compiler,
  `cmd/goc`, or any corpus program — so branch and control are the same
  measurement.
* What it leaves out makes it an **under**-estimate of the real gates: no
  determinism sweep, no `test-goc-cmd`, no `test-ruby`, no unit tests, and no
  time to build the control's worktree. The brief's own range for these jobs is
  40–120 minutes, and 46 minutes for the four items alone sits at the bottom of
  it, which is what it should do.

## 8. `make verify-fast` — 3 minutes 58 seconds, everything green

    VERIFY_FAST wall 237.95 s   cpu 2893% (28.9 of 64 cores)   maxrss 26.3 GiB

    ITEM                      SECONDS  RESULT
    build                           0  0
    corpus-parallel               185  0
    corpus-sequential-0           228  0
    corpus-sequential-1            99  0
    corpus-sequential-2           105  0
    gofmt                           0  0
    matrix-default                 40  0
    matrix-opt                     41  0
    reducers                       95  0
    unit                           13  0
    vet                             0  0

    [verify] verify-fast PASS in 238s (3m58s)

**Under four minutes against a ten-minute budget**, which bought enough headroom
to put the *whole* corpus suite in the fast tier rather than a sample of it. That
was not the plan going in; it is what the numbers allowed.

Checked rather than assumed:

* The corpus split runs **every test exactly once**. The parallel half is
  `-skip` over the 83 listed names, the three sequential shards are a partition
  of those 83, and the union was computed against `go test -list`:
  350 = 267 + 83, `diff` empty. Not "should be" — computed.
* `matrix-default` and `matrix-opt` each ran **92 subtests, 0 FAIL** — 368/4,
  the shard-0 quarter of each arm.
* The reducers really ran: 130 program executions across six cases, 0 failures.
* `gofmt: clean`; `go vet ./...` silent.

The item table also shows the scheduler doing its job: it admitted
4+2+8+32+6+6+6 = **exactly 64** cores of declared weight and made the matrix arms
and the reducers wait for a slot rather than oversubscribing.

### What verify-fast cannot see

Written out in `docs/verification.md` and in the Makefile target's comment. In
short: three of every four capabilities on both arms; determinism
(`scripts/determinism-check.sh -corpus` is the only thing that drives all 406
programs to a written object and compares bytes, and nothing here substitutes);
`test-ruby`; `test-goc-cmd`; the runtime coverage report; any comparison against
`main`; any rare intermittent fault (the reducers run 5–40 repetitions, and
RUNTIME_PLAN.md 5.10 records a fault that showed up 3 times in 53); and anything
timing, ever.

It is the right signal for *is this change broken*. It is not a merge gate.

### A bug this run found in the script, and the guard added for it

The first `make verify-fast` **exited 0 in 1.3 seconds having run nothing but
`go build`**. `declare -a JOB_NAME` leaves the variable *unset* in bash, so
`${#JOB_NAME[@]}` inside `spawn` was fatal under `set -u`, every spawn died, and
the verdict loop cheerfully reported PASS over the one item that had a result.

A verification script that reports PASS for the jobs that happened to start is
the one failure mode it must not have, so the fix is two parts: initialise the
arrays explicitly, and record every scheduled item in an `EXPECTED` list that the
verdict reconciles against. An item with no result now prints `NEVER RAN` and
fails the run, and a run that scheduled nothing at all fails too. Worth recording
because it argues for itself: the silent-pass path existed for one commit and it
was found by running the thing, not by reading it.

## 9. The timing pre-flight still works — checked both ways

Neither verify tier ever schedules `bench-perf` or `bench-crypto`. That is the
design, not an omission: they pin a core and they refuse a busy box, and a tier
that ran one alongside a 32-wide corpus would be measuring the corpus. They stay
separate targets and `TestCryptoSigningBench`/`TestPerformanceSuite` are two of
the six non-`placement` entries in `goc/sequential_tests.txt`, so they cannot be
swept into a parallel run by accident either.

Demonstrated rather than asserted, in both directions:

**Accepts a quiet core.** Run while this job's `main` control was compiling (a
serial suite, ~4 of 64 cores — so core 63 was quiet):

    --- PASS: TestCryptoSigningBench (105.73s)
    p256/sign-verify   baseline 24.0648  this run 23.9607  change -0.4%  resolved +0.3%  within tolerance (6%)
    p256/verify        baseline 16.9991  this run 16.9367  change -0.4%  resolved +0.2%  within tolerance (6%)
    p384/sign-verify   baseline 20.3676  this run 20.3292  change -0.2%  resolved +0.0%  within tolerance (6%)
    rsa2048/sign-verify baseline 2.3470  this run  2.3324  change -0.6%  resolved -0.1%  within tolerance (6%)

**Refuses a busy one.** Three spinners pinned to core 63 with `taskset`, then the
same command:

    the calibration burst got 25.1% of core 63 (floor 90%) -- it is sharing that core with something
    core 63 spent 74.9% of the window on work that was not this run's (ceiling 10%), per /proc/stat
    the burst's wall time spread 30.4% across 6 repetitions (ceiling 12%) -- the speed of this core
      is changing underneath it
    this box cannot support a trustworthy timing measurement, so make bench-crypto is refusing to start

Refused in **1.6 seconds** instead of spending eleven minutes to say the same
thing, and it named which of the three ceilings it tripped. Exit 2.

**GUARD MET: the timing pre-flight is intact and no tier can schedule a timing
run.**

(Both `bench-crypto` figures above are incidental to this branch — it changes no
compiler code — but the tolerance check passing on the committed baseline is
worth having on the record.)

## 10. `make verify-full` — every guard met, and one pre-existing failure surfaced

    VERIFY_FULL wall 3365.27 s (56m05s, cold control)   cpu 2660%   maxrss 28.2 GiB

    ITEM                      SECONDS  RESULT
    build                           0  0
    control-corpus               1142  0
    control-matrix-default        147  0
    control-matrix-opt            195  0
    corpus-parallel               169  0
    corpus-sequential-0           224  0
    corpus-sequential-1           101  0
    corpus-sequential-2           105  0
    determinism                   410  0
    determinism-opt               886  0
    goc-cmd                       349  1     <-- pre-existing; see below
    gofmt                           0  0
    matrix-default-0..3      59/53/46/57  0
    matrix-opt-0..3          62/53/49/56  0
    reducers                      104  0
    ruby                           45  0
    unit                           10  0
    vet                             0  0

### The guards

| guard | required | result |
|---|---|---|
| capability matrix, **default** arm | 368/368 | **368 PASS, 0 FAIL** summed across the four shards |
| capability matrix, **`-O`** arm | 368/368 | **368 PASS, 0 FAIL** summed across the four shards |
| the four corpus audits, check mode | pass | **PASS** — they are `corpus-sequential-0`, and the whole corpus suite is green in all four processes |
| determinism, byte-identical, unoptimised | pass | **reproducible=406 varying=0 failed=0** over 4 rounds, 0 layout-only residues |
| determinism, byte-identical, `-O` | pass | **reproducible=406 varying=0 failed=0** over 4 rounds, 0 layout-only residues |
| targeted reducers, full counts | 0 failures | **PASS** |
| `test-ruby` | pass | **PASS** (45 s) |
| no baseline re-cut | — | **none.** No `-update-*` flag was passed anywhere in this job. |

### The one failure, and it is not this branch's

`goc-cmd` (`make test-goc-cmd`) failed on one test:

    --- FAIL: TestCheckedRuntimeCoverageBaselineDenominator (0.03s)
        capability "gc-invariants/slice-tail-pointer" is in neither the accepted baseline nor
        testdata/runtime_coverage_baseline_pending.json; record why the baseline does not cover
        it, or rerun and accept a new baseline

Checked against `main` in a clean worktree at 76069d9:

    --- FAIL: TestCheckedRuntimeCoverageBaselineDenominator (0.03s)   [byte-identical message]

**Pre-existing on `main`, and this branch cannot have caused it:**
`git diff main..HEAD --name-only` touches **no file under `cmd/goc/`** at all —
the diff is `Makefile`, `docs/`, `scripts/`, `CCWORK_REPORT.md` and `goc/*_test.go`.

It is a baseline-bookkeeping failure (a capability with no entry either way), not
a compiler regression, and it is 0.03 s of pure data checking. It is here because
**no gate has ever run `test-goc-cmd`** — the old recipe was corpus plus the two
arms plus controls, and this test was outside all four. Widening the full tier
found a standing failure on its first run. Not fixed here: cutting or accepting a
coverage baseline is exactly the "do not re-cut any baseline" the brief rules
out, and it wants a person to say why that capability is uncovered.

## 11. The `main` control cache — 1484 s to 0.96 s

Cold, on this run: `control-corpus` **1142 s** + `control-matrix-default`
**147 s** + `control-matrix-opt` **195 s** = **1484 s (24.7 min)**.

Warm, immediately after, `./scripts/verify.sh controls`:

    [verify] REUSE  control-corpus         (recorded 2026-08-06T07:53:46+00:00, key 39c516698c86)
    [verify] REUSE  control-matrix-default (recorded 2026-08-06T07:56:14+00:00, key 39c516698c86)
    [verify] REUSE  control-matrix-opt     (recorded 2026-08-06T07:59:29+00:00, key 39c516698c86)
    [verify] verify-controls PASS in 1s
    CONTROLS wall 0.96 s

**1484 s → 0.96 s.** And the key invalidates when it must — recomputed by hand
against the recorded value:

| key over | first 12 hex |
|---|---|
| `main` + current recipe (what was recorded) | `39c516698c86` ✓ matches |
| a different `main` commit (`main~1`) | `607112c74db7` — **different** |
| same `main`, one byte changed in `scripts/verify.sh` | `ff9460c1bc34` — **different** |

### What is in the key and why

`main` commit; CPU model, core count, total RAM, arch, **kernel release**;
`go version`, `GOOS/GOARCH`, the system `cc` version; and a **sha256 of
`scripts/verify.sh` itself**.

* The machine and toolchain are in it because a control is a *measurement*, and
  one taken on a different box or a different compiler is not a control for this
  one — it is a different experiment.
* Kernel release is in it because it moves scheduling and page-fault behaviour
  and costs nothing to include. The price is an occasional needless re-measure;
  that is the right side to err on.
* The script's own hash is in it so that changing **what a control measures**
  invalidates every record automatically, rather than depending on someone
  remembering to bump a version constant. The cost is that a comment edit also
  invalidates. Deliberate: cheap and never wrong in the dangerous direction.
* Deliberately **not** in the key: the branch under test, the working tree, and
  the time — anything describing the branch would defeat the reuse this exists
  for.

The cache lives at `${XDG_CACHE_HOME:-$HOME/.cache}/cg12/verify-controls`,
**outside the working tree**, because ccwork jobs each get their own worktree and
a tree-local cache would never hit across jobs — which is precisely the case that
matters.

One honest note on the cold figure: `control-corpus` costs 1142 s because `main`
is still the serial tree. Once this branch lands, the same control costs about
230 s, and a cold control drops from 24.7 min to about 9.5 min.

## 12. `CG12_NOCACHE=1` still works

The constraint was explicit and merge gates rely on it. Before the sequential
list existed, the cold path at `-parallel 32` failed (§3, experiment E2). After:

    CG12_NOCACHE=1 go test -timeout 60m -count=1 -parallel 32 ./goc/...
    ok  github.com/evanphx/cg12/goc  550.938s      exit 0
    NOCACHE wall 551.52 s   cpu 1253%   maxrss 38.2 GiB

**PASS.** Nothing in this branch touches `goc/source_world.go` or the flag; the
source-world map was already mutex-guarded and is shared across concurrent tests
exactly as it was across sequential ones. 551 s against 495 s warm is the cost of
re-parsing the stdlib per compile, which is what the flag is for.

## 13. THE THREE NUMBERS

### Corpus suite

| | wall | note |
|---|---:|---|
| **before**, `-parallel 10` (i.e. serial) | **18:57** | 396% CPU = 4.0 of 64 cores |
| **after**, one `go test -parallel 32` | **8:15** / 8:27 | two runs; what `make test-goc-corpus` now costs |
| **after**, split across 4 processes by `verify.sh` | **3:44** / 3:48 | what a verify tier costs |

**2.3x in one process, 5.1x split.** Same pass set, twice, byte-identical.

### Full gate

| | wall | measured or derived |
|---|---:|---|
| **before** — corpus + default arm + `-O` arm + a `main` control of each, one after another | **45:57** | derived: 2x(1137 + 101 + 141), every part stopwatched today |
| **after**, same four items, warm control | **~5:44** | derived from the verify-full item table (corpus 224 s, matrix phase ~119 s over two admission waves, controls 0.96 s) |
| **after**, `make verify-full` entire, cold control | **56:05** | stopwatched |
| **after**, `make verify-full` entire, warm control | **31:21** | derived: 3365 − 1484 |

**8.0x like-for-like.** The honest framing of the two totals: `verify-full` at
31:21 warm is *not* comparable to the old 45:57, because it does strictly more —
it adds both determinism sweeps (410 s + 886 s = **21.6 min, now the single
largest thing in the tier**), `test-ruby`, `test-goc-cmd`, the unit tests, vet,
gofmt and the full-count reducers, none of which the old four-item recipe ran. A
gate that wanted only what the old recipe covered now costs **under six minutes**.

### verify-fast

| | wall |
|---|---:|
| `make verify-fast` | **3:58** |

Against a ten-minute budget, which is why it carries the *whole* corpus suite and
not a sample.

**What it costs to skip:** three of every four capabilities on both arms; both
determinism sweeps; `test-ruby`; `test-goc-cmd`; the coverage report; any
comparison against `main`; any rare intermittent fault (reducers at 5–40
repetitions, against a recorded 3-in-53 fault); and anything timing, ever.
`docs/verification.md` has the table and the reasoning.

## 14. What changed, and what did not

**Changed**

| file | what |
|---|---|
| `goc/*_test.go` (31 files) | `t.Parallel()` on 267 top-level tests |
| `goc/sequential_tests.txt` | new — the 83 that must not be, with a reason tag each and the measurement behind them |
| `goc/parallelpolicy_test.go` | new — enforces both directions against that file |
| `goc/corpusaudit_test.go` | audit compile pool: flat 8 → `MemAvailable / 4.23 GiB`, `GOC_AUDIT_WORKERS` override |
| `Makefile` | `GO_TEST_PARALLEL` default on every `go test` target; `verify-fast`, `verify-full`, `verify-controls`, `verify-audits`, `verify-reducers` |
| `scripts/verify.sh` | new — the tiers, the core-budget scheduler, the corpus split, the control cache |
| `scripts/reducers.sh`, `scripts/gofmt-check.sh` | new |
| `docs/verification.md` | new — the coverage table and the reasoning |

**Not changed, deliberately**

* No compiler code. `git diff main..HEAD` touches no non-test `.go` file at all.
* No tolerance weakened, no audit skipped, no baseline re-cut. No `-update-*`
  flag was passed anywhere in this job.
* Nothing removed from the full tier — it gained `test-goc-cmd`, `test-ruby`,
  the unit tests, vet and gofmt relative to the old four-item recipe.
* `CG12_NOCACHE=1`: untouched and verified working (§12).
* The timing pre-flight: untouched and verified working in both directions (§9).
* `STATUS_SHARDS`/`STATUS_SHARD` semantics: untouched. The tiers use a separate
  `VERIFY_STATUS_SHARDS` so a caller sharding by hand is unaffected.

## 15. Left open

1. **The root cause of §2/§3.** Concurrent compiles in one process change escape
   placement, always conservatively, and I could not isolate why. Ruled out: the
   shared source world (E2), any time or step budget in `opt/escape*.go`,
   `sync.Pool`, goroutines in the escape analysis, package-level mutable state in
   the compile path. Nothing in production compiles concurrently in one process,
   so it is not urgent — but an analysis whose answer depends on what else the
   process is doing is one whose answer is not a function of its input, and that
   is worth knowing about.
2. **`TestCheckedRuntimeCoverageBaselineDenominator`**, failing on `main` today
   (§10). Wants a person to say why `gc-invariants/slice-tail-pointer` is
   uncovered, or to accept a new baseline. Out of scope here by the brief's own
   rule.
3. **`determinism-opt` at 886 s** is now the largest single item in the full
   tier — 44% of the warm-control total. It is a shell-driven sweep with its own
   `-j`, so it was outside this branch's parallelism work. It is where the next
   minute of turnaround is.
4. **The 83-entry sequential list is over-broad on purpose.** Someone who
   isolates (1) can shrink it, and 350 s of the corpus's critical path is the
   prize.

## 16. Final state

`make verify-fast` re-run on the committed tree, after every edit:

    [verify] verify-fast PASS in 236s (3m56s)     exit 0     cpu 2874%

Reproducing the 3m58s of §8 to within two seconds. Three commits on
`ccwork/verification-turnaround` off `main` (76069d9); working tree clean.

**THE ANSWER, in one line each:**

* **Safe parallelism: 32**, and it is now the Makefile's default
  (`min(nproc, MemAvailable/4 GiB, 32)`), not something a caller passes. It stops
  at 32 because the critical path became a single 144 s test and the sequential
  set, not because memory ran out — peak RSS at 32 is 29.8 GiB of 243, **12%**.
* **Corpus suite: 18:57 → 8:15** in one process, **→ 3:44** split across four.
* **Full gate, the same four items the old recipe ran: 45:57 → ~5:44** with a
  warm control (the control itself: 1484 s → 0.96 s). `make verify-full`, which
  covers strictly more, is 56:05 cold / 31:21 warm.
* **`make verify-fast`: 3:56–3:58.** It cannot see three of every four
  capabilities on either arm, either determinism sweep, `test-ruby`,
  `test-goc-cmd`, the coverage report, any comparison against `main`, a rare
  intermittent fault, or anything timing.

# Why concurrent compiles in one process change escape placement

Branch `ccwork/concurrent-escape-drift`, off `main` (`f67d5da`).

## The answer, in one line

`goc/escapesummary_test.go` writes `opt.EscapeSummaries` -- a package-level
`bool` in the compiler that every compile in the process reads -- and holds it
at `false` across six whole-program compiles. Any other test compiling during
that window gets the analysis with its cross-function fact table switched off,
which is the "assume every call escapes its arguments" arm. That is the
conservative drift. It is a plain data race on a `bool`, not anything about the
escape walk itself.

The perturbation is not intrinsically conservative. It follows the knob, and the
knob's *default* is what decides the direction -- see "The permissive direction"
below.

## The reduction

`analysis/escapedrift`, three modes, smallest first.

### Ceiling: the knob alone moves placement

    $ go run ./analysis/escapedrift -mode knob -victim goc/testdata/hello.go
    227 allocation decisions
    summaries on -> off             3 moved to the heap,    0 moved to a frame
                                   first to heap:  internal/strconv.roundShortest 292:11 runtime.newobject internal_strconv_decimal
    summaries off -> on again       0 moved to the heap,    3 moved to a frame
    on -> on again (control)        0 moved to the heap,    0 moved to a frame

Three allocations, `hello.go` compiled whole-program. The control row is the
important one: two compiles at the same knob setting agree exactly, so the
compile is deterministic and the knob is the whole difference.

### The reduction proper: two goroutines, a handshake, deterministic

    $ go run ./analysis/escapedrift -mode pair -victim goc/testdata/hello.go
    victim goc/testdata/hello.go, two goroutines, handshake (default knob true, held at false)
    227 allocation decisions compiled alone
    alone -> concurrent             3 moved to the heap,    0 moved to a frame
                                   first to heap:  internal/strconv.roundShortest 292:11 runtime.newobject internal_strconv_decimal

Goroutine A does nothing but what `TestEscapeSummaryCost` does -- save
`opt.EscapeSummaries`, set it, put it back. Goroutine B compiles the program.
The handshake puts B's whole compile inside A's window, so there is no timing
and no repetition: it flips every run.

`-mode race` drops the handshake and gives goroutine A the six compiles the real
test does. It perturbs too (1 of 1 rounds), which is the statistical form the
suite sees.

### What the race detector says

    $ go run -race ./analysis/escapedrift -mode race -spin -victim goc/testdata/hello.go
    WARNING: DATA RACE
    Read at 0x000000906749 by main goroutine:
      github.com/evanphx/cg12/opt.LowerHeapAllocations()
          opt/escape.go:117
      github.com/evanphx/cg12/goc.compile()
      github.com/evanphx/cg12/goc.CompileExecutable()
    Previous write at 0x000000906749 by goroutine 75:
      main.main.func3()

A plain unsynchronised `bool`. Worth knowing: the detector reports this only in
`-spin` mode, where the writing goroutine does not itself compile. When BOTH
goroutines compile -- the real suite's shape -- two runs under `-race` reported
nothing, because the compiles contend on shared locks inside the compile path
(`goc/source_world.go`'s `sourceWorldsMutex` among them) and those locks order
the write before the read. The value read is still the wrong one. So `-race` on
the suite is not a reliable net for this; it is a race condition that is only
sometimes a data race.

## Why the earlier eliminations all held

Everything the previous job ruled out really was innocent, which is why this was
hard to see: the perturbation is not in the escape analysis, in a cache, in a
budget, in a pool, or in map order. It is one `bool` a *test* writes.

- `CG12_NOCACHE=1` changes nothing, because the source world was never involved.
- The analysis has no time or step budget that load could move; `escapeRoundCap`
  is 64 rounds of a fixed point, counted in rounds.
- No compiler source reads `GOMAXPROCS`, `NumCPU`, `MemStats` or the clock. The
  hits under `analysis/` are standalone measurement binaries, not the compile
  path; the hits under `goc/testdata/` are programs being compiled.
- The other package-level knob the tests move, `opt.SetEscapeDiagLevel` (from
  `goc/escapediag_test.go` and `goc/gcdiffreason_test.go`), is placement-neutral,
  measured rather than assumed:

      $ go run ./analysis/escapedrift -mode diag -victim goc/testdata/hello.go
      -m=0 -> -m=2                    0 moved to the heap,    0 moved to a frame

  So it is not a second mechanism. It still redirects a process-global writer,
  which is its own reason for those tests to stay sequential.

`opt.EscapeSummaries` is the only write to compiler-visible global state any test
in the tree makes. `goc`'s tests are almost all in `package goc_test`, so they
can only reach exported identifiers; the ten internal ones write no package
global.

## The exact path

`goc/escapesummary_test.go:300` -- `TestEscapeSummaryCost`, which runs by
default (nothing skips it) and compiles `testdata/stdlib_crypto_ecdsa.go` six
times:

```go
compileWith := func(summaries bool) (time.Duration, *ir.Module) {
	previous := opt.EscapeSummaries
	opt.EscapeSummaries = summaries
	defer func() { opt.EscapeSummaries = previous }()
	for round := 0; round < rounds; round++ {   // rounds = 3
		compiled, err := goc.CompileExecutable(*escapeSummaryProgram, source)
		...
```

`off, _ := compileWith(false)` holds the knob at `false` for three whole-program
compiles of a crypto/ecdsa program -- tens of seconds. At `-parallel 32`, every
other test compiling in that window reads the wrong knob at `opt/escape.go:117`:

```go
var facts *EscapeFacts
if EscapeSummaries {
	facts = ComputeEscapeFacts(module)
}
```

With `facts == nil`, `LowerHeapAllocations` cannot tell what a callee does with a
pointer it is handed, and every call falls into the assume-the-worst arm. That
is the conservative drift, and it is why it is confined to the three placement
tests: they are the only ones that assert where an allocation went.

The second writer, `compileCorpusForLoweringStats` at line 386, is reached only
from `TestEscapeSummaryPromotionRate`, which is flag-gated behind
`-escape-promotion-rate` and skips in a normal run. `TestEscapeSummaryCost` is
the live one.

## Can it be perturbed the OTHER way? Yes, and it is easy

"Conservative direction only" is not a property of the mechanism. The direction
is decided by which way the knob is being moved, and that is decided by the
knob's default -- `GOC_ESCAPE_SUMMARIES`, an environment variable. The suite
happens to run with it unset, so the default is on and the test can only move it
off. Set it the way its own doc comment says to set it for a bisection, and the
same reduction perturbs the other way:

    $ GOC_ESCAPE_SUMMARIES=0 go run ./analysis/escapedrift -mode pair -victim goc/testdata/hello.go
    victim goc/testdata/hello.go, two goroutines, handshake (default knob false, held at true)
    227 allocation decisions compiled alone
    alone -> concurrent             0 moved to the heap,    3 moved to a frame
                                   first to frame: internal/strconv.roundShortest 292:11 runtime.newobject internal_strconv_decimal

Three allocations placed in a FRAME that the same compile alone put on the heap.
That is the permissive direction, produced on demand.

## Is it a latent correctness bug? No -- but not for the stated reason

It is benign, and the argument is not "the drift is conservative".

Both knob settings are placements the compiler is willing to ship. On is what
ships; off is the same analysis with a strictly smaller fact base, which can only
over-approximate escape. So a perturbed compile lands on one of two sound
answers, never on a third. The permissive perturbation above produces exactly the
placement the shipped default produces -- it is only "permissive" relative to a
baseline taken with the knob off.

So: no shipped defect, and none reachable. `goc` compiles one program per
process, and `goc compile-batch` reads its stream strictly in sequence within
each worker, so nothing in production is ever inside the window. But two things
the earlier disposition assumed are not true:

1. **The drift is not intrinsically conservative.** It follows an environment
   variable. Anyone who runs the suite under `GOC_ESCAPE_SUMMARIES=0` -- the
   documented way to bisect a placement -- gets frame placements a serial run
   would not produce, in the tests whose whole job is to assert placement. That
   is a bisection that lies to you.
2. **Escape placement does not depend on process load.** It depends on one
   `bool` that one test writes. That is a narrower and more fixable problem than
   "concurrent compiles perturb the analysis", which is what the list's header
   comment currently says.

## The fix, and why it was not made

The trivial-looking fix is not available. `TestEscapeSummaryCost` measures the
wall time of a *whole compile* with the table off, so the setting has to be in
force inside `goc.CompileExecutable`. Removing the global write means either
plumbing the knob through as a compile option -- a new field on
`compileOptions` and a new exported entry point, threaded to `opt/escape.go` --
or re-execing the test in a subprocess with `GOC_ESCAPE_SUMMARIES=0`, which is
maybe thirty lines but rewrites a timing test whose validation is six
whole-program compiles of crypto/ecdsa. Neither is a small change, and neither is
verifiable inside this job's guards. So the compiler is unchanged and the
sequential list keeps its entries.

What was done instead, all of it either measurement or documentation:

- `analysis/escapedrift`, the reduction, with the `knob`, `diag`, `pair` and
  `race` modes shown above.
- `opt.EscapeSummaries` now says in its doc comment that writing it under a
  running compile perturbs that compile, which is the one thing its previous
  comment -- five paragraphs on what the knob is for -- did not say.
- `goc/sequential_tests.txt`'s header said "the root cause is NOT isolated". It
  now names the cause, and says exactly which entries the finding licenses
  removing and what has to be run first.
- `goc/parallelpolicy_test.go` gains
  `TestOnlyKnownTestsWriteProcessGlobalCompilerState`: the set of test functions
  that write `opt` package state is frozen at five, and a sixth fails the test
  with an explanation. It matched the hand-found set exactly on its first run,
  which is the independent check that `opt.EscapeSummaries` and the two
  diagnostic setters really are the whole of it.

## What the list could become

83 entries today: 77 `placement`, 4 `setenv`, 2 `timing`.

Of the 77, exactly two need to be sequential for the reason found here --
`TestEscapeSummaryCost` and `TestEscapeSummaryPromotionRate`, the two that write
the knob. Another seven move the diagnostic level or writer instead
(`TestEscapeDiagnostic*`, `TestEscapeReasonDifferentialAgainstGC`,
`TestGCExplanationsParseTheFlowChain`, `TestGocFlagM*`); that is placement-neutral
but they perturb each other -- `diagnoseEscapes` sets the level, compiles, and
then reads reasons the compile only recorded because the level was up -- so they
need a `globals` tag rather than removal. The remaining ~68 have no reason left.

Narrowing it is worth roughly the difference between 68 tests running first and
alone and 68 running in the parallel pool, on the suite that already went 18:57
-> 8:15. It was not done here because showing it means running
`TestAllocationCensus`, `TestFrameEscapeAudit` and
`TestInterfaceConversionsCallTheRuntimeHelpers` at `-parallel 32` against the
whole suite -- the census, the audits and the corpus, all three explicitly out of
this job's scope. That run is the only evidence that matters, and it is cheap
next to the hunt that produced the list.

## Guards run

`go build ./...`, `go vet ./opt/ ./goc/ ./analysis/escapedrift/`, and
`go test ./goc/ -run 'TestOnlyKnownTestsWriteProcessGlobalCompilerState|TestEveryTestIsParallelOrListedAsSequential'`
(0.07s, ok). No compiler behaviour was changed, so there is no at-risk check to
add: the only edit to compiler source is a doc comment on `opt.EscapeSummaries`.
The corpus suite, capability matrix, `make test-unit`, audits, census,
determinism sweeps and crash loops were not run.


# The verifier's model of function entry was one input short

Branch `ccwork/ir-verify-entry-blocks`, from `main` (`76069d9`).

The build-cache design job found that 211 of 5083 functions (4.2%) fail
`ir.Verify` on a normal whole-program compile, all of them closure, `deferwrap`
and `methodvalue` entry blocks, and that `ir.DecodeModule` therefore cannot
round-trip a goc module. This is that defect.

**The verifier was wrong, not the IR.** The only non-test source change is
`ir/verify.go`.

**And emitted code does change** — closures get bigger, `.text` grows 0.5% on
`hello.go` — because the defect was not latent. `ir.CloneFunc` clones through
`MarshalBinary`/`DecodeModule`, `DecodeModule` gates on `VerifyModule`, and so
the verifier rejecting closures had been silently disabling
`opt.InlineIntoNoSplitCallers` for every one of them. Fixing the verifier
re-enables an optimisation that had been off for 4-6% of functions. See the
CORRECTION section near the end, which is the part of this report to read if you
read only one.

## What was violated

`verifySSA`'s use-before-definition rule: *every temporary a use reads must have
a definition somewhere*. It builds the set of already-defined temporaries by
walking `f.Params`, and `f.Params` is not the whole of a function's entry.

A function has two kinds of entry input. Parameters are the obvious kind. The
second is the closure environment: when `Func.HasClosureContext` is set,
ABIInternal delivers it in the architecture's dedicated closure register (X26 on
arm64, RDX on amd64) before the first instruction runs, and the temporary marked
`Temp.ClosureContext` receives it. It is defined on entry exactly as a parameter
is, and no instruction assigns it.

It is deliberately *not* in `f.Params`, because it is not an argument. It
consumes no argument register and no stack slot, and ABI lowering treats it
apart from the parameter sequence — `arm64/goabi.go` builds its spill group
outside the parameter loop, and `arm64/lower.go`'s `stabilizeClosureContext`
copies it out of the volatile register at entry precisely because it arrived
there rather than through the argument assigner.

So the verifier was right that nothing *in the body* defines it, and wrong that
this made it undefined. Reading the closure environment is not a use before a
definition; it is a read of the second entry input.

## Reproduced, and it is one construction path

`cmd/verifyprobe` (scratch, not committed) compiles a program, runs `ir.Verify`
over every function, and for each failure asks whether the undefined temporary
is the one marked `ClosureContext`.

| | functions | fail `ir.Verify` | |
|---|---:|---:|---|
| `hello.go` | 2744 | **125 (4.56%)** | 115 closure, 4 `deferwrap`, 3 `gowrap`, 3 `methodvalue` |
| `stdlib_http_client_server.go` | 14568 | **831 (5.70%)** | 446 closure, 310 `methodvalue`, 50 `deferwrap`, 25 `gowrap` |

Every failure is the same message — `start: add reads %N, which nothing
defines` — and in **831 of 831** and **125 of 125**, the undefined temporary is
the `ClosureContext` temporary. Not one exception.

The three named shapes are one construction path, as suspected:
`(*gen).closureContext` at `goc/compile.go:14604`, with four callers — the
func-literal closure (`compile.go:13723`), the range-over-func yield child
(`:11106`), the `deferwrap`/`gowrap` wrapper (`:10530`) and the `methodvalue`
wrapper (`:13295`). The `add` in every message is `g.offset(environment, 8*(i+1))`,
the load of the *i*-th captured variable out of the environment. That is also
why the count is lower than the number of such functions: on `hello.go` 169
functions have a closure context and 125 fail, the other 44 being closures that
capture nothing and so never read the environment.

The survey found the invariants hold exactly, over all 2744 functions of
`hello.go` and all 14568 of the http program, before and after `-O`:

  - `HasClosureContext` set ⟺ exactly one temporary marked `ClosureContext`
    (169 and 169; 1087 and 1087). No function has two, none has the flag
    without the temporary, none the temporary without the flag.
  - No `ClosureContext` temporary is ever assigned by an instruction (0).
  - No `ClosureContext` temporary is ever also in `f.Params` (0).

## The fix

`ir/verify.go`: `defineClosureContext` seeds the closure-context temporary as
defined on entry alongside the parameters, and the invariant is stated there in
full for the next person.

The exemption is granted only where the function claims it, so that "no
instruction assigns it" cannot become a way to smuggle a genuinely undefined
value past the check: the flag and the marked temporary must agree, and there
must be exactly one. Those three disagreements are now diagnostics of their own.
That also closes a hole in the inliner, which copies `Fixed` and `Reg` onto a
cloned temporary but not `ClosureContext` (`opt/inline.go:872`) — a callee
carrying the temporary without the flag would have been cloned into a caller as
an ordinary undefined value, and is now rejected at the source.

The check earned itself immediately: two of the repository's own round-trip
fixtures stated half the fact. `richModule`'s `vf` set `HasClosureContext` and
marked no temporary; `TestTempRoundTripsEveryField`'s fixture marked a temporary
and left the flag unset. Both go through `DecodeModule`, which is the front door
a build cache would use, so a fixture that is not well-formed IR is a weaker
fixture. One line each.

## What now runs it, which is the part worth more than the fix

`ir.Verify` was already there. Nothing on the path from the front end to the
backend called it. Its only callers were `ir.DecodeModule`, the lifter
(`lift/lift.go`), and `opt/jumpthread.go` under `CG12_JT_CHECK` — none of which
a goc compile goes through. That is the whole reason a 4.2% defect could sit
there: the instrument existed and was pointed somewhere else.

`TestIRVerifyAudit` (`goc/irverifyaudit_test.go`) runs it over every function of
every corpus program, inside the pass `TestFrameEscapeAudit`,
`TestLoopAliasAudit` and `TestAllocationCensus` already share. It costs one
linear walk per function against compiles that dominate everything in that pass.

It has **no baseline file**, deliberately, and that is the difference between it
and its neighbours. The escape and aliasing audits record what the compiler
currently does, because what it does is not all correct and the record is what
stops it drifting. This one has a single acceptable answer: IR that fails the
verifier is IR no pass downstream is entitled to assume anything about. Its
failure message says so, and says the thing that was actually hard here — decide
whether the front end is emitting malformed IR or the verifier's model is
missing a case, before changing either.

## The round trip, which is what the caching work was waiting on

`TestModuleRoundTripsThroughTheBinaryFormat` compiles a whole program, encodes
it, decodes it and re-encodes the result, before and after `-O`. Measured
directly on both programs, in all four configurations:

| | functions | encoded | decode | re-encode |
|---|---:|---:|---|---|
| `hello.go`, as compiled | 2744 | 11.1 MiB | OK | **byte-identical** |
| `hello.go`, `-O` | 2152 | 15.9 MiB | OK | **byte-identical** |
| http, as compiled | 14568 | 106.4 MiB | OK | **byte-identical** |
| http, `-O` | 12427 | 158.2 MiB | OK | **byte-identical** |

**`ir.DecodeModule` round-trips a whole goc module.** Nothing else was missing:
the encoding was never the problem, and `VerifyModule` at the end of
`DecodeModule` was the only thing rejecting it.

The re-encode is what makes it a claim rather than an absence of an error. A
decode that merely returns without error proves nothing about fields it dropped;
a re-encode that reproduces the original bytes proves the decoded module carries
everything the encoder writes. And `ir/binary_total_test.go` already guards the
other half — that the encoder writes every field the types carry — by requiring
each field of its fixtures to be non-zero, so a newly added field fails the test
by name rather than being silently dropped. Between the two, the round trip is
guarded from both ends.

Two things a cache still wants that this does not give it, both named by
`BUILD_CACHE.md` and neither blocking: the format carries a version byte but no
content digest, and there is no cache key. Those are cache design, not IR
soundness.

## Why the verifier and not the IR

The instruction was not to assume the IR is at fault because it is the thing
that changed less recently, so here is the case each way.

**For "the IR is wrong."** The closure context could have been an entry in
`f.Params`, and then nothing about the verifier would need to change. Or the IR
could have given it an explicit defining pseudo-instruction at entry, so that
"defined" meant one thing everywhere.

**Against, and this is what settles it.** Neither alternative is available
without making the IR worse:

  - `f.Params` is the *argument* sequence. `lowerABI` walks it in order and
    assigns argument registers and stack slots from it. Putting the closure
    context there would consume an argument register it does not use and shift
    every real argument by one. `arm64/goabi.go` already builds the closure
    context's spill group in a separate block *after* the parameter loop, for
    exactly this reason. It is not an argument, and the ABI code says so.
  - There is no entry pseudo-op in this IR, for anything. Parameters themselves
    have no defining instruction — `f.Params` is a list of temporaries, and what
    makes them defined is being in that list. "A temporary the ABI supplies at
    entry has no defining instruction" is therefore the *existing* convention,
    not a deviation from it. The closure context follows it exactly. What was
    missing was that `f.Params` is not the only list of such temporaries.

So the IR is internally consistent and models the thing properly: `Func.HasClosureContext`
and `Temp.ClosureContext` are first-class, the backend reads them
(`stabilizeClosureContext`), the inliner reads them (`inlineClosureContext`), and
the binary format carries both. Every consumer of the IR understood the closure
context except the one whose job is to say what the IR is.

**The one genuinely unusual thing, stated plainly:** the front end pins a
physical register on that temporary (`Fixed`, `Reg = 26`) at construction, long
before lowering, which is otherwise a lowering concern — `ir.Verify`'s own doc
comment says it "does not check the things lowering establishes ... register
assignments". That is a real oddity and it is how the dedicated register reaches
the backend at all. It is not what the verifier objected to, and changing it is
a different piece of work.

The 4.2% is also not a coincidence of three shapes. All four — closure,
`deferwrap`, `gowrap`, `methodvalue` — come through `(*gen).closureContext`, one
function, four callers. One construction path, as the brief guessed.

## A second instrument the defect had disabled

`opt/jumpthread.go` verifies each function it threads, under `CG12_JT_CHECK`,
and panics if threading broke SSA. That check could not be used at all:

    $ CG12_JT_CHECK=1 goc -O goc/testdata/hello.go     # main, 76069d9
    panic: jumpthread: runtime.timer.modify.func.638.16: threading
      logicshort5->logicend6 broke SSA: ir: runtime.timer.modify.func.638.16:
      start: add reads %0, which nothing defines

Threading had broken nothing. The verifier was reporting the pre-existing
closure-context reading, in the first closure the pass happened to touch, and
the pass turned it into a panic. So the tree had two instruments pointed at this
and both were silent for the same reason: one was never run, and the other
crashed the compiler when it was, which amounts to the same thing.

    $ CG12_JT_CHECK=1 goc -O goc/testdata/hello.go     # this branch
    $ ./hello
    hello from cg12 Go

That is not a fix in its own right — it is the same one fix — but it is the
second thing that was measurably unusable and is not any more.

## Scope of the new check, stated so nobody assumes more than it does

`verifySSA` only runs on unlowered functions (`Verify` gates it on
`LoweredFor() == ""`, because lowering destructs SSA and reassigns temporaries
freely). `defineClosureContext` is called from inside it, so both the entry
seeding and the flag/temporary consistency check are **pre-lowering
invariants**. After lowering, `stabilizeClosureContext` has copied the context
into an ordinary temporary and nothing checks the pairing any more.

That is the right place for it — it is where the exemption is granted, so it is
where the exemption has to be kept honest — but it does mean a pass that broke
the pairing *after* lowering would not be caught. Nothing does today.

## The corpus-wide before figure

The pre-fix count over the whole corpus, measured by putting the new audit test
into a worktree at `main` (`76069d9`) and leaving `ir/verify.go` as `main` has
it:

    goc emitted IR that ir.Verify rejects: 7899 distinct diagnostics,
    from 1559314 function verifications across 406 programs

**7,899 distinct functions** across the corpus, all the same shape. After the
fix, the same sweep: **0**, over the same 1,559,314 verifications.

Those two numbers have different denominators and dividing them would be wrong,
which is worth saying because the test's own failure message used to invite it.
A diagnostic names one function and is counted once however many of the 406
programs share it — and they share a lot, since every program links the same
stdlib. `functions` counts every verification. The honest per-program rates are
the direct ones: **125 of 2744 (4.56%)** for `hello.go` and **831 of 14568
(5.70%)** for the http program. The test now says this in the message rather
than leaving the next person to work it out from a ratio that does not mean
anything.

## Guards

| guard | required | result |
|---|---|---|
| `TestIRVerifyAudit` (new) | pass | **PASS** — 1,559,314 function verifications across 406 programs, 0 rejected (7,899 distinct functions rejected before the fix) |
| `TestFrameEscapeAudit` | pass | **PASS** |
| `TestLoopAliasAudit` | pass | **PASS** |
| `TestAllocationCensus` | pass | **PASS**, against the committed baseline unchanged — no census delta to review |
| the corpus builds | 406/406 | **406/406** — `auditCorpus` fails if any program does not compile, and it did not |
| capability matrix, default arm | 368/368 | **368 subtests PASS, 0 FAIL**, 1 EXPECTED FAILURE, 0 KNOWN GAP NOW PASSES; `ok ... 76.3s` |
| capability matrix, `-O` arm | 368/368 | **368 subtests PASS, 0 FAIL**, same 367 + 1 split; `ok ... 95.5s` |
| GC reducer `runtime_gc_promoted_local_root` `-O`, `GOMAXPROCS=3` | 0/20 at `GOGC=10` and default | **0/20 at `GOGC=10`, 0/20 at default** |
| GC reducer `runtime_gc_type_mask_padding` `-O`, `GOMAXPROCS=3` | 0/20 at `GOGC=10` and default | **0/20 at `GOGC=10`, 0/20 at default** |
| `go test ./ir` | pass | **PASS** (whole package, including the four new verifier tests and the two repaired round-trip fixtures) |
| whole-module round trip, 4 configurations | decode | **decode OK and re-encode byte-identical** in all four |
| determinism on this branch | byte-identical | **406/406 identical** across two `-O` corpus rounds; 131 of them identical a third time from a separate pool |
| branch vs `main` byte-for-byte | — | **406/406 differ**, expected and explained: the fix re-enables `InlineIntoNoSplitCallers` for closures. `hello.go` `.text` +8,064 bytes; verified by bisect to be the verifier's answer and not the binary's layout |

The three new verifier tests and the round-trip test were each confirmed to fail
on `main`'s `ir/verify.go` with the test files in place, and to pass with the
fix — the fail-before check, not just a passing test.

### A determinism-harness snag, recorded because it cost time

Two things went wrong with `analysis/determinism` and neither was the compiler.

First, `goc` resolves the standard-library overlay manifest relative to **its own
build tree**, not its working directory:

    load runtime: standard library overlay manifest
    .../main-tree/stdlib/overlays/manifest.json: no such file or directory

I had built a comparison `goc` inside a temporary `git worktree`, then removed
the worktree while keeping the binary. The binary still ran, and failed every
compile instantly — `round 0: 406 programs in 0.0s, 406 failed`. A `goc` binary
is not relocatable away from the checkout it was built from. Worth knowing
before anyone tries to cache or ship one.

Second, `analysis/determinism` reports failures by **name only**. When 406 of
406 programs "failed to compile" it printed 406 names and not one reason, and
the reason was a single line available in the very reply it had already parsed
(`response.Error`). Diagnosing it needed a hand-run of the batch protocol. That
is a one-line improvement to a tool this repository leans on, and it is not
made here only because it is not this branch's subject.

### Determinism on this branch — byte-identical

Measured by comparing the images directly, which is the evidence rather than the
harness's bookkeeping:

| comparison | programs | identical |
|---|---:|---:|
| branch `-O`, round 0 vs round 1 (same pool) | 406 | **406** |
| branch `-O`, round 0 vs a later independent run at a different worker count and box load | 131 | **131** |

So every corpus program compiles to the same bytes twice, and 131 of them do it a
third time from a separate process pool on a differently-loaded box. Spot-checked
by execution: the images run and print what they should.

One thing I could not explain and am not going to claim I did: on the first
whole-corpus run the harness's own summary said `failed=406 of 406` while
writing 812 correct, full-size, byte-identical ELF executables that execute
correctly. Its `failing` list is populated from `response.Error`, so some replies
carried an error alongside a good binary. The second run, at `-j 12` on a quieter
box, produced byte-identical output for every program the two runs share. The
compiler's output is not in doubt; the harness's error accounting on a saturated
box is, and that is a separate thread to pull.

## CORRECTION, and the most important thing on this branch: the fix DOES change emitted code

Everything above the guards table was written believing `ir.Verify` was not on
goc's compile path, and that the change was therefore verifier-only. **That is
wrong, and the byte comparison caught it.** Compiling `hello.go` with two
compilers built from the same directory, differing only in `ir/verify.go`:

    .text   with the fix 1,549,348   without 1,541,284    (+8,064 bytes)

Same 2,430 functions in both, none added or removed. What changed is that many
**closures got bigger** — larger frames and more zeroed GC slots. Bisected to
the behaviour and not the code layout: a build with `defineClosureContext`
present and called but stubbed to `return nil` is **byte-identical** to `main`'s
output, so it is the verifier's *answer* that moves the compiler, not the extra
code in the binary.

The mechanism, found by logging every `ir.Verify` call site during a real
compile:

    ir.Verify  <- ir.VerifyModule <- ir.DecodeModule <- ir.CloneFunc
               <- arm64.measureFunction <- arm64.noSplitFrameBudget.Frame
               <- opt.InlineIntoNoSplitCallersReporting

**`ir.CloneFunc` clones a function by encoding and decoding it**, and
`DecodeModule` ends with `VerifyModule`. So on `main`, cloning *any* closure,
`deferwrap`, `gowrap` or `methodvalue` that reads a captured variable returned
an error — and `opt.InlineIntoNoSplitCallers`, which clones a callee to measure
its frame before inlining it into a `nosplit` caller, silently skipped every one
of them. The optimisation was disabled for 4-6% of functions and nothing said
so, because a clone failure there is not an error, it is a "cannot inline this".

So the 4.2% verifier defect was not latent after all. It had been silently
costing optimisation this whole time, through a path nobody had connected to it:
`Verify` rejects closures → `DecodeModule` refuses them → `CloneFunc` fails →
the nosplit inliner declines. Fixing the verifier re-enables it, which is why
closures grew: they are now receiving inlined callees they were always meant to.

This also explains why `BUILD_CACHE.md`'s round-trip failure and this were the
same bug: `CloneFunc` and the build cache use the very same encode/decode door.

### Quantified, and is it safe?

Same probe, but with `defineClosureContext` stubbed so the verifier behaves as
`main` does, compiling `hello.go`:

| `ir.Verify` reached via | calls | failing |
|---|---:|---:|
| `CloneFunc` <- `measureFunction` <- `newNoSplitFrameBudget` | 465 | **86** |
| `CloneFunc` <- `measureFunction` <- `noSplitFrameBudget.Frame` | 312 | 0 |
| `CloneFunc` <- `InlineIntoNoSplitCallersReporting` | 209 | 0 |
| `parse.Parse` <- `applyNativeStdlibOverlays` | 1 | 0 |

So on `main`, **86 of the 465 functions the nosplit frame budget tries to
measure cannot be measured at all** on a hello-world, every one of them a
closure shape. With the fix, zero fail and all 465 are measured.

What a measurement failure means, at `arm64/nosplit_measure.go:76`:

    measured, err := measureFunction(f, name, conventions, bundle)
    if err != nil {
        // ... Leaving it out understates the headroom of everything that
        // calls it, which is the safe direction.
        continue
    }

The function is dropped from the facts handed to `stackcheck`. **I think that
comment has the direction backwards, and it is worth someone checking.**
`stackcheck` deliberately mirrors Go's `cmd/link` walk, in which *an unresolved
callee ends a chain* — so a `nosplit` function missing from the facts does not
constrain its callers, it stops the walk before its own frame and its whole
subtree are counted. That overstates headroom rather than understating it. I am
not asserting a stack-overflow bug: with the fix all 465 are measured, so
whichever direction it was, the budget is now computed from complete
information for these functions. But the comment and the `stackcheck` rule
disagree, and only one of them can be right.

Evidence the new code is sound, all on this branch's tree:

  - capability matrix **368/368 on both arms** — and `stackcheck` runs inside
    `compileToObjectWithBundle` and returns an error, so a blown nosplit budget
    fails the compile rather than shipping;
  - the corpus **compiles 406/406**;
  - the GC reducers are **0/20 at `GOGC=10` and at default**, both programs;
  - `TestAllocationCensus` passes **against the committed baseline, unchanged**.

### So: did emitted code change, or only the IR's internal form?

**Emitted code changed.** Not the IR's internal form — that is identical; the
front end emits exactly what it emitted before, and the fix does not touch it.
What changed is what the optimiser then does with it, because a pass that had
been silently declining to act on closures can now act on them.

The allocation census is **unchanged**, so there is no census delta to review
site by site: the extra inlining moves code into `nosplit` callers without
moving any allocation. That is the guard's question answered directly — code
changed, allocation placement did not.

## What I would tell the next person

1. **A verifier is not a passive observer here.** `ir.CloneFunc` clones through
   `MarshalBinary`/`DecodeModule`, and `DecodeModule` ends with `VerifyModule`.
   Anything the verifier rejects is therefore un-clonable, and every pass that
   clones-to-measure reads that as "cannot do this" rather than as an error. A
   false positive in `ir.Verify` is an optimisation switched off in silence.
   That coupling is not obvious from either file and is now written down in
   `ir/verify.go`.

2. **`ir.Verify` now runs over the corpus** (`TestIRVerifyAudit`), with no
   baseline, inside the existing audit pass. That is the cheap durable part.

3. **A whole module round-trips**, before and after `-O`, byte-identically, so
   the build-cache work is unblocked on this axis.

4. **One thing left open, deliberately.** `arm64/nosplit_measure.go:76` says
   dropping an unmeasurable function "understates the headroom of everything
   that calls it, which is the safe direction". By `stackcheck`'s own documented
   rule — an unresolved callee ends a chain — dropping it looks like the
   *unsafe* direction. The fix makes all 465 measurable so the question is no
   longer load-bearing for closures, but the comment and the rule still
   contradict each other and someone should decide which is right.


# Per-package build caching for goc (BUILD_CACHE.md)

Branch `ccwork/build-cache-design-2`, from `main` (76069d9). Deliverable is
`BUILD_CACHE.md`; this section records results as they land. Host: aarch64
Linux, 64 cores, 250 GiB, go1.26.1. No caching behaviour was changed.

## Measured: stage attribution, small program

`goc/testdata/fmt_sprintf.go` (10 lines; 5083 functions after the stdlib
closure), arm64, full pipeline, GOMAXPROCS=64. Wall and bytes allocated per
stage, from a harness that runs exactly the stages `cmd/goc` runs:

| stage | wall | allocated |
|---|---:|---:|
| frontend (parse + typecheck + IR lowering) | 4.05 s | 1.11 GiB |
| `opt.OptimizeModule` (13-pass whole-module) | 9.80 s | 3.21 GiB |
| backend + object (lower, regalloc, emit, ELF) | 2.05 s | 2.93 GiB |
| **total** | **15.91 s** | **7.25 GiB** |

Process total 17.0 s wall / 44.2 s CPU: the backend is parallel (10.4 CPU-s in
2.05 s wall) and GC is 39% of all CPU samples.

## Measured: goc's IR already serializes, and it is cheap

`ir.Module.MarshalBinary`/`ir.DecodeModule` exist and are documented as a
disk cache format. On the same module:

| | size | marshal | decode |
|---|---:|---:|---:|
| pre-opt IR | 20.2 MiB | 0.14 s | 0.17 s |
| post-opt IR | 35.8 MiB | 0.28 s | 0.36 s |

Decoding the *entire* 5083-function pre-optimisation module costs 0.17 s
against a 4.05 s front end.

## Measured: the module goc produces does not satisfy goc's own IR verifier

`ir.VerifyModule` on the pre-opt module fails immediately:
`ir: time.deferwrap.580.8: start: add reads %0, which nothing defines`
(net/http program: `net.methodvalue.net.file.close.22.8.2069`, same shape).
`ir.DecodeModule` calls `VerifyModule` before returning, so the round trip
fails on that gate, not on the encoding: marshal and decode themselves
complete. This is a prerequisite for any IR-on-disk cache and it is currently
broken.

## Read, not recalled: what gc's cached artifact contains

`cmd/compile/internal/noder/doc.go` defines Unified IR. `SectionBody` holds
function bodies as encoded IR elements (`(*pkgWriter).bodyIdx`,
`writer.go:1169`); reading one back materialises `ir` nodes
(`reader.go:3462`). Nothing is re-parsed or re-typechecked to inline across a
package boundary.

The writer emits a body for *every* function. The pruning is done afterwards by
`linker.exportBody` (`noder/linker.go:219`): drop it if `fn.Inl == nil` (i.e.
`inline.CanInline` rejected it against `inlineMaxBudget = 80`), otherwise carry
it if the function is local *or* if this compilation actually inlined it
(`fn.Inl.HaveDcl`). `relocFuncExt` writes alongside it the definition ABI,
per-parameter escape notes and the inline cost. The fingerprint is a sha256 of
the payload from `(*PkgEncoder).DumpTo`; `cmd/link` refuses a mismatch
(`ld/lib.go:2530`).

### The brief's one wrong detail, corrected

`buildActionID` (`cmd/go/internal/work/exec.go:261`) folds in
`contentID(a1.buildID)` for each dependency -- the **content hash of the
dependency's archive**, not its action ID. That is stronger, because it gives
cutoff. Probed on this box with a two-package module and a fresh `GOCACHE`:

| change to the leaf | recompiled |
|---|---|
| append a trailing comment (shifts no line numbers) | leaf only -- **the dependent was a cache hit** |
| change an inlinable body | leaf **and** dependent |
| change a constant inside a function far over the inline budget | leaf **and** dependent |

Row 3 is the limit: the cut is at the whole archive, which contains the object
as well as the export data, so gc buys soundness with a conservative key rather
than a precise dependency analysis.

Eviction: `DiskCache.Trim` is age-based with no size cap -- mtime refreshed on
use at most hourly, trim at most daily, 5-day cutoff, over 256 subdirectories.

## Measured: where the cacheable/non-cacheable line falls in goc

`opt.DefaultPipeline` splits at the first inline fixpoint. Timed apart
(`stagetime -split-opt`):

| | small | http |
|---|---:|---:|
| per-function prefix (`mem2reg` + `clean`) | 1.44 s | 8.34 s |
| whole-module remainder | 8.58 s | 36.08 s |
| sum vs unsplit `OptimizeModule` | 10.02 vs 9.98 | 44.42 vs 43.91 |

**85% of the optimiser is downstream of inlining.** Inside the front end,
per-package work (`funcDecl` 1.58 s / 7.78 s, `globalDecl`, parse, typecheck) is
61% on small and 53% on http; the rest is `reachableFunctions`,
`collectDynamicTypes`, `addInterfaceMethodWrappers` and the heap-allocation
module passes, which are whole-program by definition and consume ASTs, not IR.

## Measured: blast radius under whole-module optimisation

Control: two processes on the same source produce 5083/5083 and 4131/4131
identical function IRs -- the compiler is deterministic.

| change | pre-opt changed | post-opt changed |
|---|---:|---:|
| a constant, or an added statement, in the root package | 1 of 5083 | **1 of 4131** |
| two comment lines above `runtime.alignUp` (no semantic change) | 4 | 47 (1.1%) |
| `runtime.alignUp`'s body rewritten, net of the line shift | 1 | **28 (0.7%)** |

Whole-module optimisation does not smear changes across the program. Position
shifts propagate further than code changes do, because `ir.SrcPos` is in the IR
and inlining carries it -- gc has the same property. The stdlib edit was
reverted and the tree verified byte-identical.

## Measured: what packs save, and what they cannot

| | wall |
|---|---:|
| build a pack carrying `fmt` | 15.9 s, 20.8 MB |
| small program, monolithic | **16.45 s** |
| small program, against the pack | **5.02 s** (-69%) |
| `fmt`+`sort`+`strings`, against the same pack | 5.08 s |
| `os`-only program, against the same pack | **refused** ("none of the 1 prebuilt runtimes offered is usable"); falls back to 12.43 s |

Stage split of the pack-linked compile: front end **4.20 s**, opt 0.19 s, back
end 0.18 s. **The front end is 91% of what remains and is unchanged from the
monolithic compile** -- the split is subtractive and deliberately late, so all
5083 functions are lowered and then 4483 thrown away.

`cmd/goc/packcache.go` has no eviction of any kind, and the key covers the
hashed goc binary, so every compiler rebuild orphans every pack permanently.

## Recommendation

Keep packs. Per-package IR caching's measured ceiling in goc is 18-21% of a
compile against the packs' measured 69%, because goc's cacheable stage ends
before its optimiser's main work begins. Build the per-package unit anyway, but
aimed at the pack's floor: removing `funcDecl`+`globalDecl` from the pack path
takes it from 5.02 s to about 3.4 s, 79% off the monolithic compile. Work
items, in order: fix the 211 functions failing `ir.Verify`; add a payload
fingerprint to `ir/binary.go`; give the pack cache gc's 5-day trim; split
`MarshalBinary` into per-package units with cross-unit `AggType` unification;
cache after `mem2reg`+`clean`.

Full document: `BUILD_CACHE.md`. Harness: `cmd/stagetime`.


# Option C, stage 1: can the key say what the output already shows?

Branch `ccwork/optionc-stage1`, cut from `main` (`21ca7b3`).
Deliverable: `opt/depset.go` (the recorder), `cmd/depsets` (the harness),
`analysis/optionc/depsets.py` (the arithmetic), and this section.

Host: aarch64 Linux, 64 cores, 250 GiB RAM, go1.26.1. All figures are
`goc -O` whole-program arm64 builds at `GOMAXPROCS=64`, the same configuration
`BUILD_CACHE.md` Part 2 used, and the same two programs.

## Why this measurement exists

`BUILD_CACHE.md` §3.4 costs Option C -- Option B plus a memoised whole-module
stage and back end, keyed per function on its inline-dependency set -- at an
**85% ceiling on the small program and 86% on http**, and says in the same
paragraph: "Nothing here is measured as an implementation; only the ceiling is."

The ceiling comes from §2.4 and §2.4b, which measure that the *output* is
stable: after a root-package edit 4130 of 4131 post-optimisation functions are
byte-identical, and after rewriting a leaf helper with 115 call sites 4103 of
4131 still are. **A memoiser cannot use that fact.** It has to decide, before
doing the work, whether last compile's answer is still good, and the only thing
it has to decide with is a set it recorded last time. If the inliner's recorded
set is large or conservative -- if compiling one function consults a large
fraction of the module transitively -- then an edit invalidates most of the
cache and the saving collapses toward zero even though the output barely moved.

So the question stage 1 answers is not "is the output stable" (measured, yes)
but "**can a recorded set say so**".

## The answer, first

**The sets are small, and they invalidate almost exactly what the edit changed.
Option C's key can exist.**

| edit | functions whose *input* moved | functions whose *output* genuinely changed | memos the recorded sets **invalidate** | ratio | memo hit rate |
|---|---:|---:|---:|---:|---:|
| root-package (`42`→`43` in `main`) | 1 (`main.main`) | **1** of 4131 | **2** of 4131 | **2.00x** | **99.95%** |
| leaf helper (`runtime.alignUp` rewritten) | 1 (`runtime.alignUp`) | **29** of 4131 | **31** of 4131 | **1.07x** | **99.25%** |

(`alignUp` has 115 call sites in the vendored runtime source, which is the figure
§2.4b quotes; goc's front end lowers only reachable functions, so the module the
inliner sees carries **39** direct call sites to it.)

The brief's decision rule was: ~30 invalidated means the ceiling is real, ~3000
means Option C is worth roughly Option B. The measured answer is **31**.

And the key is not merely small, it is **sound on both edits**: every function
whose post-optimisation body changed was invalidated by its recorded set. Zero
functions changed output while the key said the memo was still valid. That is
the property a memoiser would miscompile on, and it held.

## The distribution, not the average

`opt.Record` files two sets per function.

**Consulted** is what soundness requires: every function the inliner resolved by
name on this one's behalf. It lands there whether or not it was inlined, because
the inliner read its size, its attributes, its structure and its place in the
call graph, and a decision about the caller came out of that read. A callee that
grew past the budget changes a caller that never inlined it.

**Spliced** is the optimistic floor: only the functions whose bodies actually
ended up inside this one. Both are transitively closed through splicing -- a
caller holding a clone of `mid` also holds the clone of `leaf` that was already
inside it, so it depends on `leaf` too.

Sizes over the functions that survive to the back end:

| | n | min | median | p95 | p99 | max | mean |
|---|---:|---:|---:|---:|---:|---:|---:|
| **small**, consulted | 4131 | 0 | **3** | **27** | 36 | **182** | 8.6 |
| **small**, spliced | 4131 | 0 | 2 | 18 | 23 | 39 | 5.5 |
| **http**, consulted | 12796 | 0 | **3** | **25** | 33 | **181** | 8.3 |
| **http**, spliced | 12796 | 0 | 2 | 18 | 21 | 39 | 4.8 |

11.1% of small's surviving functions (7.1% of http's) have an *empty* consulted
set: the inliner never read another function to produce them, so their memo can
be keyed on their own input alone.

**The distribution does not move between the two programs.** http is 3.1x the
functions of small and its median dependency set is the same 3, its p95 one
lower, its maximum one lower. The set is a property of how deep goc's inliner
reaches -- bounded by `inlineSmallBudget` (24 instructions) and
`inlineGrowthCap` (+128) -- and not of how large the module is. That is the
single most important fact for Option C: **the key does not degrade as the
program grows**, which is the failure mode that would have killed it.

## The number that actually decides an invalidation

A memo dies when something in its set changes, so what matters is the *reverse*
direction: how many memos does one changed function kill?

| | median | p95 | p99 | max |
|---|---:|---:|---:|---:|
| small, reverse fan-out | 1 | 9 | 71 | **1395** |
| http, reverse fan-out | 1 | 8 | 36 | **5462** |

The median function invalidates one memo. The tail is where the risk is, and it
is a short, nameable list -- on small: `goc_memcpy` (1395), `goc_memset` (1388),
`goc_storep` (1038), `runtime.atomicstorep` (1000), `runtime.inHeapOrStack`
(991), `runtime.inheap` (990), `runtime.noescape` (990),
`runtime.mSpanStateBox.get` (989), `runtime.atomicwb` (985), `runtime.spanOf`
(984). These are the write barrier, the heap-membership test and the memory
intrinsics: tiny, `nosplit`, and inlined into a third of the module.

So the honest statement of Option C's exposure is not a percentage, it is a
sentence: **editing anything in the runtime's write-barrier and heap-lookup core
costs a third of the cache; editing anything else costs a handful of functions.**
`runtime.alignUp`, the §2.4b target with 39 direct call sites in this module,
has a reverse fan-out of **31** -- which is where the leaf-edit row above comes
from, and it is 0.75% of the module.

## How the two edits were made, and what the controls say

Both programs are the ones `BUILD_CACHE.md` Part 2 used, compiled by
`cmd/depsets`: front end, then the per-function prefix (`mem2reg` + `clean`)
whose per-function digest is the memoiser's **input**, then the whole-module
remainder under `opt.Record`, whose per-function digest is the **output**. A
memo for `f` is invalid exactly when `f`'s recorded set meets the set of
functions whose input digest moved.

- **root-package edit** — `42` → `43` in the `fmt.Sprintf` call in `main`.
- **leaf edit** — `runtime.alignUp` in `stdlib/src/runtime/stubs.go`, the §2.4b
  target. Reproducing §2.4b's method exactly: a *control* that inserts two
  comment lines above it and changes nothing else, and a *treatment* that
  rewrites its body to compute the same value in three statements (`mask`,
  `sum`, `return`), also +2 lines. Comparing treatment against control isolates
  the body rewrite from the line-number shift, which is why the input population
  is exactly one function.

Four controls, all run:

| control | result |
|---|---|
| same source, two processes | **0 of 4131** post-opt digests differ — the compiler is deterministic, as §2.1 found |
| recorder on vs recorder off, same split pipeline | **0 of 4131** differ — **recording does not perturb what is measured** |
| split pipeline vs `opt.OptimizeModule` unsplit | **3 of 4131** differ (below) |
| the same program compiled from two different worktree paths | **3744 of 5083** input digests differ (below) |

### Two things the controls turned up that are worth recording

**goc's IR carries absolute source paths, so any content key is path-sensitive.**
The first attempt at the leaf experiment put control and treatment in two git
worktrees, and 3744 of 5083 functions differed before either edit was reached.
`ir.SrcPos` names a file through `Module.Files`, and `goc.StdlibRoot` is derived
from `runtime.Caller(0)` and is therefore absolute. Every position in the module
embeds the build directory. gc has the same problem and solves it with
`-trimpath`, which `buildActionID` folds into the key explicitly (§1.3). Any goc
cache — B or C — needs the equivalent before an artifact can be shared between
two checkouts, or its hit rate across machines is zero. The experiment was rerun
with both variants compiled from one path, which is what the table above reports.

**Splitting the pipeline at the Option B/C boundary is not output-neutral.**
Running `mem2reg`+`clean` as one `opt.Run` and the remainder as a second — which
is exactly the boundary §3.1 says the cacheable unit ends at — changes three
functions: `internal/strconv.trimZeros`, `runtime.decoderune`, `syscall.Write`.
It is deterministic (both split runs agree with each other, both unsplit runs
agree with each other) and it is not the recorder. The cause is the `changeLog`:
a split gives the second half a fresh one, so `clean`'s passes re-run on
functions the unsplit pipeline had already recorded as converged. `opt/pass.go`
states the invariant that makes skipping sound — "if a pass ran on f and reported
no change, and no pass has changed f since, running it again would report no
change again" — and for these three functions it does not hold.

**This is Option B's problem as much as Option C's**, and it is not currently
written down anywhere: the cacheable unit's own boundary moves the compiler's
output for 0.07% of functions before any caching exists. It is three functions
and it is deterministic, so it is a bug to find rather than a design obstacle —
but a byte-identity guard on a memoised compile will trip on it, so it has to be
found first.

## The three awkward passes, and a fourth that is worse than the three

§3.4 names three passes that "do not fit the per-function model". Measured, in
place, by wrapping each transform rather than by splitting the pipeline around
it (so the pass identities and the shared `changeLog` are exactly a normal
compile's):

| | small | http |
|---|---:|---:|
| `UnrollRecursion` | **0.064 s**, 86 functions unrolled | **0.451 s**, 1398 unrolled |
| `DeadFuncElim` | **0.077 s** | **0.318 s** |
| `InlineIntoNoSplitCallers` | **0.340 s**, 227 measured, 111 accepted | **0.637 s**, 230 measured, 113 accepted |
| call graph + SCC + call-site census | **0.499 s** over 10 rebuilds (0.050 s each) | **3.766 s** over 18 rebuilds (0.209 s each) |

**`DeadFuncElim` is not awkward — rerun it.** §3.4 guessed ~0.14 s on small; it
is 0.077 s, and 0.318 s on http. It only removes functions from `m.Funcs`; a
function it drops is by construction referenced by nothing live, so no surviving
body depends on the outcome. It is a pure function of the final bodies, which a
memoised compile has. Rerun it and forget about it.

**`UnrollRecursion` is not awkward either, and §3.4 is wrong to list it.** It
splices through the same `spliceCall` as the inliner, so the recorded sets
already cover it: the 86 functions it unrolls on small (1398 on http) carry
their unrolled callees in their dependency sets like any other inline. Its one
extra input is SCC membership, which is the same global the inliner needs
anyway (below).

**`InlineIntoNoSplitCallers` is genuinely awkward, and the measurement says how
much that costs: 227 functions on small, 230 on http — 5.5% and 1.8% of the
module.** Its input is a byte count the backend computes from the finished code,
and `FrameBudget.Charge` spends a chain's headroom in call-graph order, so one
accepted caller changes what a later caller on the same chain is allowed to do.
That is a dependency on an ordering, not on a set, and no per-function key
expresses it. The answer is not to express it: **exclude the ~230 nosplit
callers the pass measures from the memo entirely** and let them take the full
path. They are a fixed, small, nameable population — it does not grow with the
program (227 → 230 as the module goes from 5083 to 14901 functions) because it
is bounded by how many `nosplit` functions the runtime has, not by how much code
is around them. Costing 5.5% of the module its memo is a far better trade than
making the key model a frame budget.

**The fourth thing, which §3.4 does not mention, is the standing cost of the
whole-module analyses.** `inlineModule` rebuilds the call graph, its SCC
condensation and the call-site census on every round — 10 rounds on small
(0.499 s total), 18 on http (3.766 s). A memoiser skips the rounds, but it
cannot skip the analysis entirely, because that is what tells it whether the
recursion classification its key depends on still holds (below). One rebuild is
**0.050 s on small and 0.209 s on http**.

So the un-memoisable residue of the whole-module stage is one graph build plus
`DeadFuncElim` plus `InlineIntoNoSplitCallers`:

- small: 0.050 + 0.077 + 0.340 = **0.467 s** of a 16.16 s compile (2.9%)
- http: 0.209 + 0.318 + 0.637 = **1.164 s** of a 74.4 s compile (1.6%)

It shrinks as a fraction as the program grows, which is the right direction.

## What the recorded set does not cover, and what that costs

The set is what the inliner *read through `directCallee`* — the single choke
point through which nothing crosses the function boundary without resolving a
name. Three inputs reach an inlining decision without going through it, and each
has to be accounted for or the key is unsound. Two of them turn out to be free;
one is a real hole with a cheap fix.

**The call-site census is a dead input today, and there is now a test saying so.**
`worthInlining` derives its budget as `inlineOnceBudget/sites` floored at
`inlineSmallBudget`. Both constants are **24**, so the quotient is at or below
the floor for every `sites ≥ 1` and the budget is always 24 — the module-wide
call-site count cannot change any decision. This matters because the census is
the one input that is genuinely whole-program (every function that calls `g`
contributes), so a live one would have forced every caller of `g` into every
memo that mentions `g`. `TestCallSiteCountCannotChangeAnInlineDecisionToday`
asserts the two constants are equal and fails the moment someone separates them,
which is the moment the per-function key needs redesigning.

**`selectCostInline` selects nothing on either Go program** — 0 functions on
both. It fires only on callers containing a computed goto (`ir.JmpBr`), which is
an interpreter's threaded dispatch loop; the Go programs have none. It is a live
whole-module input for the C interpreter workloads and inert for Go, so it needs
a clause in the key (a digest of the selected set) but costs nothing here.

**SCC membership is a real hole.** `scc.recursive[callee]` gates inlining
absolutely, and it is a property of the whole call graph, not of any function the
recorded set names. The failure is constructible: `f` inlines small `g`; `g`
calls large `x`, which `g` did not inline, so `x`'s own callees never entered
`f`'s set; an edit makes `x`'s callee `y` call back into `g`'s component. Now `g`
is recursive and `f` may no longer inline it — but nothing in `f`'s recorded set
moved, so the key says the memo is valid and it is not.

Neither measured edit produced a single instance (the analysis checks for
exactly this: "changed output the key called valid" was **0** on both), but the
hole is structural rather than statistical. The fix is cheap and is already
paid for: **store the `recursive` bit of every function in the memo's set and
validate it against the rebuilt SCC.** The graph rebuild costs 0.050 s / 0.209 s
and the residue passes need it anyway.

**`ir.Module.SymAttrs` is a fourth module-wide input, and it is free.** The
inliner reads it through `isFrameScopedRuntimeCall`, and `LoadElim` and
`DeadAlloc` read it through `isAtomicPointerStore` — `opt/pass.go` already names
it as the one piece of module state the per-function passes touch. The front end
writes it once and no pass ever changes it, so one digest of the table in the key
covers it.

**The largest sets are not what one would guess.** The maximum on both programs
is `.goc.global.initfunc.*.unicode.Scripts` (182 on small, 181 on http) — a
package-level initializer that builds a large table and therefore names a great
many helpers. The largest *ordinary* functions are `runtime.findRunnable` (82),
`runtime.gcMarkTermination` (67) and `runtime.sweepLocked.sweep` (66): the
scheduler and collector, which is where one would expect a deep inline reach.
Nothing in either module comes within an order of magnitude of "half the module".

## The same measurement on http: the blast radius is absolute, not proportional

| edit | program | input moved | output genuinely changed | **invalidated** | ratio | hit rate |
|---|---|---:|---:|---:|---:|---:|
| root-package | small (4131) | 1 | 1 | **2** | 2.00x | 99.95% |
| root-package | http (12796) | 2 | 2 | **3** | 1.50x | 99.98% |
| leaf `runtime.alignUp` | small (4131) | 1 | 29 | **31** | 1.07x | 99.25% |
| leaf `runtime.alignUp` | http (12796) | 1 | 29 | **31** | 1.07x | **99.76%** |

The leaf edit invalidates **the same 31 functions** on a module 3.1x larger.
That is the strongest single result here. `alignUp`'s blast radius is a property
of `alignUp` — how many functions inline it — and not of how much other code is
in the module. So Option C's hit rate *improves* with program size (99.25% →
99.76%) rather than degrading, which is the opposite of what a conservative key
would do and the opposite of the risk this stage existed to test.

The http root edit changed two functions rather than one, because inserting a
statement in `main` moves `main.main` and the handler closure it defines. The
key invalidated three: the two, plus one caller that had inlined one of them.

**And the key was sound on all four.** Not one function changed its
post-optimisation body while its recorded set said the memo was still valid.
That is the property a memoiser miscompiles on, and it held on 4131 + 4131 +
12796 + 12796 functions.

## What Option C would actually deliver, arithmetic on measured inputs

This is a projection, not an implementation. It multiplies `BUILD_CACHE.md`
Part 2's stage times by the hit rates and residues measured above.

Small (16.16 s: front end 4.08, prefix 1.44, whole-module remainder 8.58, back
end 2.05):

| | s |
|---|---:|
| Option B underneath (`funcDecl`+`globalDecl` 1.64 + prefix 1.44) | 3.08 |
| whole-module remainder, less the 0.467 s residue, at the leaf edit's 96.2% work-hit rate | 7.80 |
| back end, less the ~230 nosplit callers excluded from the memo, at 99.25% | 1.92 |
| **saved** | **12.80 of 16.16 = 79%** |
| less decoding the post-opt unit (§2.5: 0.36 s) | **12.44 = 77%** |

http (74.4 s: front end 18.98, prefix 8.34, remainder 36.08, back end 11.49):

| | s |
|---|---:|
| Option B underneath (8.19 + 8.34) | 16.53 |
| remainder, less the 1.164 s residue, at the leaf edit's 98.8% work-hit rate | 34.5 |
| back end, less the excluded nosplit callers, at 99.76% | 11.26 |
| **saved** | **62.3 of 74.4 = 84%** |
| less decoding the post-opt unit (§2.5: 1.60 s) | **60.7 = 82%** |

**77–82% against a stated ceiling of 85–86%**, and against Option B's 18–21%.
The gap to the ceiling is not the miss rate — it is the residue, the decode, the
~230 nosplit callers and the recompute closure below, in that order.

### The one architectural constraint the measurement uncovered

**A memo hit cannot be served by storing only the final body.** Read from
`opt/inline.go`, not measured: `inlineModule` walks `scc.order` bottom-up and
`inlineInto` splices the callee's body *as it stands at that moment* — after that
round's inlining, before the `clean` that follows the inline pass. So the body a
caller receives is an intermediate state, not the finished one. A function whose
memo missed therefore needs the intermediate states of everything it spliced, not
their final bodies.

That is bounded, and the bound is measured. Taking the invalidated set and adding
the functions whose bodies were spliced into it:

| edit | invalidated | plus intermediates a miss needs | **total to recompute** | work-hit rate |
|---|---:|---:|---:|---:|
| small, leaf | 31 | 125 | **156** of 4131 | 96.2% |
| http, leaf | 31 | 126 | **157** of 12796 | 98.8% |
| small, root | 2 | 13 | **15** of 4131 | 99.6% |
| http, root | 3 | 5 | **8** of 12796 | 99.9% |

Only 1142 of 5083 functions on small (2181 of 14901 on http) are ever spliced
into a survivor at all, so the population that would need intermediate states
stored is a fifth of the module rather than all of it. Either answer works —
store the intermediates for that fifth, or recompute the ~125 the misses drag in
— and the arithmetic above uses the recompute reading, which is the cheaper one
to build and the more expensive one to run.

Two things the arithmetic is still optimistic about:

- **A miss is not priced per function.** The whole-module remainder is a
  fixpoint, not a sum of per-function costs. Re-running it for 156 functions
  still pays for the pipeline scaffolding around them. With 156 of 4131 the error
  is small; with a `goc_memcpy` edit invalidating 1395, it is not.
- **Validation is not priced at all.** Every build must hash each function's
  post-prefix IR to compare against the recorded input digests — a walk over the
  whole module. It is the first thing stage 2 must measure rather than assume.

## Verdict: stage 2 is worth doing

The brief's decision rule was explicit — ~30 invalidated functions means the
ceiling is real; ~3000 means Option C is worth roughly Option B and should not be
built. **It is 31, on both programs, for the hard edit.** The recorded set can
say what the output already shows.

The three facts that decide it:

1. **The set is small and does not grow with the program.** Median 3 consulted
   functions, p95 27, max 182 — and http, at 3.1x the functions, has median 3,
   p95 25, max 181. goc's inliner reaches a bounded distance (24 instructions of
   callee, +128 of caller growth), so the key's size is a property of the cost
   model, not of the module.
2. **Invalidation tracks the real blast radius to within 7%.** 31 against 29 for
   the leaf edit, 2 against 1 and 3 against 2 for the root edits. The
   conservatism a sound key costs — counting callees that were read and not
   inlined — is two functions, not two thousand.
3. **It was sound on every function of every comparison.** Zero cases of a
   changed output the key called valid, across 33 854 function comparisons.

What stage 2 has to build, in the order the measurement suggests:

1. **A `-trimpath` equivalent.** Absolute build paths are in `ir.SrcPos`, so
   today two checkouts share nothing. Nothing else matters if this is not fixed;
   it is also Option B's problem.
2. **Find the three functions the pipeline split moves** (`trimZeros`,
   `decoderune`, `syscall.Write`). A byte-identity guard is the correctness
   property of the whole design, and it cannot be turned on while the boundary
   itself moves output.
3. **The key**: per function, the recorded consulted set, each member's
   post-prefix input digest, each member's `recursive` bit (to close the SCC
   hole), a digest of `ir.Module.SymAttrs`, plus `opt.PipelineIdentity()`, the
   target, `-O` and the goc binary hash, exactly as §3.2 has them.
   A memo entry must hold the function's *intermediate* spliced body as well as
   its final one, or a miss has to recompute the ~125 functions it drags in.
4. **Exclude the ~230 nosplit callers `InlineIntoNoSplitCallers` measures.** They
   are 5.5% of small and 1.8% of http, they do not grow with the program, and
   they are the only population whose result depends on an ordering rather than
   on a set.
5. **Rerun, do not memoise: `DeadFuncElim` (0.077 s / 0.318 s), one call graph +
   SCC build (0.050 s / 0.209 s), `InlineIntoNoSplitCallers` (0.340 s /
   0.637 s).** `UnrollRecursion` needs neither — it splices through
   `spliceCall`, so the recorded sets already cover it.
6. **Measure validation cost before trusting the projection.**

The one thing that would change this answer: the reverse fan-out tail. Editing
`goc_memcpy`, `goc_memset`, `goc_storep`, `runtime.atomicstorep` or the
heap-membership helpers invalidates a third of the module (1395 of 4131, 5462 of
12796). That is the write-barrier and allocator core, it is a short list, and it
is edited rarely — but a build-cache design that does not say this out loud
would be a design that surprises somebody. **Option C is fast for edits to your
own package and to ordinary library code, and roughly Option B for edits to the
runtime's memory core.**

### Scope note

Packs were not touched, as instructed. `cmd/depsets` and `opt.Record` are
measurement scaffolding: the recorder is nil unless `opt.Record` installs it, and
the controls above show a recorded compile produces byte-identical output to an
unrecorded one on all 4131 functions.

### Guards run

Scaled to the change, which is measurement scaffolding plus five hook lines in
the inliner:

- `go build ./...`, `go vet ./opt ./cmd/depsets`, `gofmt` — clean.
- `go test ./opt` — the whole package, **ok in 0.94 s**. It covers the six new
  tests in `opt/depset_test.go`: that the spliced set closes transitively through
  a callee's own inlines, that a callee the inliner read and declined stays in the
  consulted set (and that consulted is a superset of spliced), that **recording
  produces byte-identical output to not recording**, that `Record` restores the
  recorder and refuses to nest, and that the call-site census cannot change an
  inline decision while `inlineOnceBudget == inlineSmallBudget`.
- `make verify-fast` — **PASS in 4m44s**, every item green (build, vet, gofmt,
  unit, three corpus shards, corpus-parallel, matrix-default, matrix-opt,
  reducers).
- The recorder's output-neutrality was also checked end to end on whole programs,
  which is the check that matters more than the unit test: a recorded compile and
  an unrecorded one agree on **4131 of 4131** post-optimisation function digests,
  and two recorded http compiles agree on **12796 of 12796**.

Not run, per the brief: the corpus suite on its own, the capability matrix on its
own, `make test-unit`, the four audits, the census, determinism sweeps, the crash
loops. `TestIRVerifyAudit` and the memoised-vs-unmemoised byte-identity check are
stage 2's guards and are not needed here, because nothing this branch commits
changes what the compiler emits.

The `stdlib/src/runtime/stubs.go` edits for the leaf experiment were made in a
throwaway `git worktree` and restored with `git checkout --` after each run; both
that worktree and this one are clean.

# Option C, stage 2: the memoiser

Branch `ccwork/optionc-stage2`, cut from `ccwork/optionc-stage1` (`67daeb8`).

Host: aarch64 Linux, 64 cores, 250 GiB RAM, go1.26.1. All figures are `goc -O`
whole-program arm64 builds, the same two programs stage 1 and `BUILD_CACHE.md`
Part 2 used.

## Blocker 2, first, because it is one line: it is not the changeLog

Stage 1 measured that splitting `DefaultPipeline` at the Option B/C boundary
moves three functions -- `internal/strconv.trimZeros`, `runtime.decoderune`,
`syscall.Write` -- and attributed it to the `changeLog`. It is not the
`changeLog`. It is the **jump-thread budget**, and the distinction matters
because the two have opposite fixes.

`cmd/splitprobe` runs a 2x2 over the two things a split changes -- whether the
second half gets a fresh `changeLog`, and whether it gets fresh *pass objects*:

| arm | pass objects | changeLog | functions differing from unsplit |
|---|---|---|---:|
| `unsplit` | one pipeline | one | — |
| `split-rebuilt` (stage 1's shape, `cmd/depsets`) | rebuilt for the second half | fresh | **3** |
| `split-shared` (one pipeline sliced, two `opt.Run`s) | shared | **fresh** | **0** |
| `split-session` (one pipeline sliced, one `opt.Session`) | shared | shared | **0** |
| `split-rebuilt-sharedjt` (rebuilt, but one `JumpThreadPass`) | rebuilt **except** jump-thread | fresh | **0** |

`split-shared` gives the second half a completely fresh `changeLog` and is
byte-identical on all 4131 functions. So re-running a pass that had already
reported convergence is exactly as harmless as `opt/pass.go` claims — the
invariant holds, and stage 1's suspicion of it was wrong.

What is not harmless is rebuilding the passes. `opt.JumpThreadPass` holds a
`map[*ir.Func]*jtState` in a closure — `origInstrs` (the function's size when
the pass first saw it), `grownInstrs` and `threads` — and the comment on it says
what it is for: *"bounds how much threading one function receives across the
whole pipeline, so the clean fixpoint that contains the pass always terminates"*.
A second `JumpThreadPass()` instance is a second budget. A function that spent
its budget in the prefix's `clean` gets a fresh one in the remainder's `clean`
and is threaded further, which is what moves those three functions and nothing
else. Sharing *only* the jump-thread instance across the split (last row)
restores byte-identity with everything else rebuilt.

**The fix is therefore not to make the split cheaper but to stop rebuilding the
pipeline across it.** `opt.Session` (new, `opt/pass.go`) carries the change log
across several `Run` calls, and `opt.PerFunctionPrefixLen` names where to cut one
`DefaultPipeline()` in two. A memoised compile builds the pipeline once and hands
the halves to one session; the split is then free, and the byte-identity guard
can be switched on.

## Blocker 1: `-trimpath`, and the two places the path was hiding

Stage 1 found that two git worktrees of one commit produced **3744 of 5083**
differing per-function digests before any edit. `ir.SrcPos` names a file through
`ir.Module.Files`, and `goc.StdlibRoot` comes from `runtime.Caller(0)`, so every
position in every module carried the build directory.

`goc.TrimPath` (new, `goc/trimpath.go`) rewrites a source path into the
repository-relative, slash-separated form before it is interned, at the three
places a name enters the file table, and the two consumers that compare against
that table move with it (`escapediag.srcPos` looks a name up in the table;
`canonicalCoveragePath` has to open the file, so it goes back through
`goc.UntrimPath`).

That took 3744 to **381**, which is the interesting part: **the file table was
not the only place the build directory was.** Three families of *symbol name* are
built from a source position, because a name alone does not identify them —

| symbol | why the position is in the name |
|---|---|
| `.goc.global.init.*` | a package may declare several blank globals with initializers, and every one is called `_` |
| `.goc.global.literal.*` | nistec's p224/p384/p521 are one generated file three times over, so their literals share a line and a column |
| `.goc.runtime.type.*` | a local type is identified by where it is declared (`appendLocalTypeIdentities`) |

Each of those keys is hashed into the emitted symbol, so an absolute path in it
put the build directory into the *object* even though no position ever reached
`ir.SrcPos`. `goc.positionKey` trims the file for all three.

| control | before | after |
|---|---:|---:|
| two worktrees, post-prefix (input) digests | 3744 of 5083 differ | **0 of 5083** |
| two worktrees, post-optimisation digests | — | **0 of 4131** |
| two worktrees, the whole `goc -O` image | — | **byte-identical** (`697c21a9…`) |

It is unconditional rather than a flag: goc emits no line table, so every reader
of the file table is inside this repository, and a switch that changes compiler
output without changing the compiler binary is exactly the pack-cache hazard
`opt.PipelineIdentity` exists to close.

**One path-keyed thing is left, and it is not in the IR.** `cmd/goc/prebuilt.go`
folds `goc.StdlibRoot()` — an absolute path — into the pack cache key, as a proxy
for "which standard library tree is this". Two checkouts therefore still miss
each other's *packs*. That is a key using a path where it means content; it does
not affect what the compiler emits, and it is left alone here because the brief
says not to touch packs.

## The memoiser: first end-to-end saving

`memo/` is the cache; `cmd/memoc` is a compiler that uses it and writes the
object and the translated assembly, so a memoised compile and an unmemoised one
can be compared byte for byte. `-no-memo` is the control: the same binary, the
same split, the same `opt.Session`, the store ignored.

**Small program (`goc/testdata/fmt_sprintf.go`, 5083 functions in, 4131 out),
warm with nothing changed:**

| arm | wall | front end | prefix | validate | lookup | **stage** | record | back end | store I/O |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| control (no memo) | **16.14 s** | 4.21 | 1.44 | — | — | **8.42** | — | 2.04 | — |
| cold (empty memo) | 16.76 s | 4.19 | 1.42 | 0.13 | 0.00 | 8.56 | 0.27 | 2.01 | 0.04 |
| **warm** | **8.87 s** | 4.15 | 1.43 | 0.13 | 0.36 | **0.00** | 0.00 | 2.00 | 0.10 |

**45.0% end to end, and the object and the assembly are byte-identical to the
control's** (`01af5728…`, `e5de208c…`).

The stage went 8.42 s → 0.00 s. What is left is the front end (4.15 s) and the
back end (2.00 s), neither of which Option C caches: **the front end is Option
B's job and Option B is not built.** Against the stages Option C actually
memoises, 8.42 of 8.42 s is gone.

**The cold cost is +0.62 s on 16.14 (3.8%)**: 0.13 s validating (marshalling and
hashing all 5083 post-prefix bodies, plus one call graph and SCC condensation),
0.27 s recording, 0.04 s writing 30 MB. Stage 1 said to measure validation rather
than assume it; it is **0.13 s, 0.8% of the compile**, and it is paid on every
build, hit or miss.

## What it is

`memo/` is the cache. Per function it stores the finished body (cg12's binary
unit format), and a key made of clauses rather than one opaque hash, so a miss
can say which clause moved:

| clause | why |
|---|---|
| the function's own post-prefix body digest | it is the input |
| for each member of the inliner's recorded **consulted** set: its name, its post-prefix digest, and its `recursive` bit | the set is stage 1's; the bit closes the SCC hole stage 1 named |
| `ir.Module.SymAttrs` | the one piece of module state the per-function passes read |
| the `CostInline` selection | the one live whole-module input that is neither |
| the file table | positions are indices into it and a per-function unit does not carry it |
| `opt.PipelineIdentity`, the target, the compiler binary's hash | the identity of the compile |

The **data section is deliberately not in a per-function key**, and putting it
there was a measured mistake: `DeadFuncElim` roots reachability at the globals,
so it looks like it belongs — but `DeadFuncElim` is rerun on every compile from
the actual module, so no per-function memo needs to know. With it in, changing
`42` to `43` in the program moved a string literal and invalidated **4607 of
4608** entries through a clause none of them depended on. It belongs to the
whole-module check below, whose claim does cover which functions were dropped.

A hit's body is installed and the function is **frozen** (`opt.Session.Freeze`,
new): no pass in the stage may transform it, every pass may still read it. The
back end's half is separate and simpler: lowering, allocation and emission read
one function and read-only module facts — the property the parallel backend
already relies on — so the finished code is a pure function of the finished body
and the key is that body's digest, with no dependency set at all.

Stage 1's four rulings are implemented as it left them: `DeadFuncElim` is rerun,
`UnrollRecursion` needs nothing of its own, the `nosplit` population is excluded
outright (475 of 5083 on small, 479 of 14901 on http — it does not grow with the
program), and one call graph + SCC condensation is rebuilt to revalidate the
`recursive` bits.

### The whole-module check, which is the only part that is exact by construction

Before the per-function lookup there is a whole-module one: if every function's
post-prefix digest matches its entry, the module clause matches, the data digest
matches, and the module holds exactly the functions the store was written from,
then **the input to the stage is bit-for-bit the input the stored output came
from**. The stage is a deterministic function of that input, so its output is the
stored output — for every function, including the excluded ones, and including
whatever the per-function key cannot express. It does not depend on any of the
per-function reasoning being right.

That is not a special case bolted on to flatter a benchmark; it is the case a
build cache is in most of the time (build again, having changed nothing), and it
is the case the "warm" rows below measure.

## The numbers

`cmd/memoc` is a compiler that uses the memo and writes the object and the
translated assembly. `-no-memo` is the control: same binary, same split, same
`opt.Session`, store ignored. Every row is one run on an otherwise idle box.

### small — `goc/testdata/fmt_sprintf.go`, 5083 functions in, 4131 out

| arm | wall | front end | prefix | validate | lookup | **stage** | record | back end | store I/O | **saving** |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| control | **16.14** | 4.21 | 1.44 | — | — | **8.42** | — | 2.04 | — | — |
| cold | 16.76 | 4.19 | 1.42 | 0.13 | 0.00 | 8.56 | 0.27 | 2.01 | 0.04 | **−3.8%** |
| warm, nothing changed | **8.87** | 4.15 | 1.43 | 0.13 | 0.36 | **0.00** | 0.00 | 2.00 | 0.10 | **45.0%** |
| warm, root-package edit | 12.37 | 4.20 | 1.44 | 0.16 | 0.20 | 3.81 | 0.12 | 2.06 | 0.09 | **23.4%** |
| warm, leaf edit (`runtime.alignUp`) | 11.76 | 4.17 | 1.45 | 0.16 | 0.23 | 3.24 | 0.10 | 2.05 | 0.09 | **27.1%** |

### http — `goc/testdata/stdlib_http_tls_client_server.go`, 14901 in, 12796 out

| arm | wall | front end | prefix | validate | lookup | **stage** | record | back end | store I/O | **saving** |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| control | **76.11** | 19.79 | 8.36 | — | — | **36.16** | — | 11.67 | — | — |
| cold | 79.26 | 19.60 | 8.42 | 0.89 | 0.00 | 37.02 | 1.10 | 12.03 | 0.12 | **−4.1%** |
| warm, nothing changed | **41.78** | 19.10 | 8.36 | 0.90 | 1.20 | **0.00** | 0.00 | 11.90 | 0.32 | **45.1%** |
| warm, leaf edit (`runtime.alignUp`) | 61.30 | 19.42 | 8.34 | 0.87 | 0.76 | 19.23 | 0.48 | 11.74 | 0.31 | **19.5%** |

**Every one of those memoised compiles emits an object and a translated assembly
byte-identical to its control's.** Checked as sha256 of both artifacts, on every
row, plus a per-function comparison of all 5083 / 14901 finished bodies.

| scenario | object | control |
|---|---|---|
| small, nothing changed | `01af572844ee64d8` | `01af572844ee64d8` |
| small, root edit | `6fb91174cde51622` | `6fb91174cde51622` |
| small, leaf edit | `052413aae94d9030` | `052413aae94d9030` |
| http, nothing changed | `08159f8899915c3c` | `08159f8899915c3c` |
| http, leaf edit | `a1a950950b183d9a` | `a1a950950b183d9a` |

### The hit rate in practice

| scenario | entries validated | **invalid** | served from the memo | recomputed to keep the reads honest | excluded (`nosplit`) |
|---|---:|---:|---:|---:|---:|
| small, nothing changed | 5083 | 0 | **5083 (100%)** | 0 | — |
| small, root edit | 4608 | **2** (99.96% valid) | 3243 (63.8%) | 1363 | 475 |
| small, leaf edit | 4608 | **56** (98.8% valid) | 3427 (67.4%) | 1125 | 475 |
| http, leaf edit | 14422 | **56** (99.6% valid) | 10516 (70.6%) | 3850 | 479 |

The root edit invalidates **2** — `main.main` and the closure it defines — which
is exactly what stage 1 measured. The leaf edit invalidates **56 on both
programs**, and stage 1's strongest single result survives: `alignUp`'s blast
radius is a property of `alignUp`, not of how much other code is around it, so
the hit rate improves with program size (98.8% → 99.6%).

(56 rather than stage 1's 31 because this key is the sound one and stage 1's
count was the recorded-set intersection alone: this one also fails an entry whose
*own* recursion classification moved, and closes a frozen callee's transitive set
in through its stored entry, both of which stage 1's arithmetic did not do.)

## Against the 77–82% projection

The projection is **not** met, and the gap is three specific things, two of which
are not the memoiser's to close.

| | small | http |
|---|---:|---:|
| stage 1's corrected projection | 77% | 82% |
| ...of which Option B underneath (front end + prefix), **not built** | 19% | 22% |
| so the ceiling available to Option C alone, on these measurements | **64.8%** | **62.8%** |
| **achieved, warm, nothing changed** | **45.0%** | **45.1%** |
| the same with the back-end memo persisted (it is in-process only) | 57.4% | 60.7% |
| achieved, warm, leaf edit | 27.1% | 19.5% |

Three separate statements, in order of size:

1. **Option B is not built, and it is 19–22 points of the 77–82%.** The
   projection is for Option B *plus* Option C. What this branch built is the
   Option C half. On the warm no-edit row the front end costs 4.15 s of 8.87 and
   19.10 s of 41.78 — 47% and 46% of what is left is a front end nothing caches.

2. **The whole-module stage is fully memoised: 8.42 s → 0.00 s and 36.16 s →
   0.00 s.** That half of Option C delivers everything the projection asked of
   it, and more than it asked: the projection allowed a 0.467 s / 1.164 s residue
   for the passes that must be rerun, and the whole-module identity check skips
   even those, soundly.

3. **The back end's memo is implemented and content-addressed but not persisted**,
   so across processes it never hits: 2.00 s of small's 8.87 and 11.90 s of
   http's 41.78 is a back end recompiling what it compiled last time. Persisting
   it means writing a codec for the per-function code record (code, relocations,
   block symbols, line rows, DWARF, Go metadata, stack maps, the nosplit budget's
   facts). Its size is measured below; the work was not attempted here rather
   than attempted badly, because a codec that drops a field emits a wrong object.

### And the edit rows are the real correction to stage 1

Stage 1 projected a **96.2% work-hit rate** on the leaf edit — 31 invalidated
plus 125 splice sources, 156 of 4131 recomputed — and said the choice was
between storing intermediate bodies and recomputing that closure, "and the
arithmetic above uses the recompute reading". **The recompute reading, as stage 1
defined it, does not emit the same bytes.** Measured, on the leaf edit:

| live set | stage | object |
|---|---:|---|
| misses only (`-closure=none`) | 1.06 s | `101e71ee…` — **differs** |
| misses + splice sources, stage 1's reading (`-closure=splice`) | 1.04 s | `152e5799…` — **differs** |
| misses + everything a live function *reads that the stage moves* (`-closure=read`) | 3.24 s | `052413aa…` — **identical** |

The mechanism, found by tracing one function through the stage pass by pass. The
inliner and the unroller do not read a callee's *finished* body: they walk the
SCC condensation bottom-up and size a callee with `funcSize` at that moment, and
splice it as it stands. A frozen function stands at its finished body from the
start. So a live function can see a callee that the reference compile would have
shown it in an earlier state.

It is not hypothetical and it is not rare enough to ignore. On the small program
with **no misses at all** — where the only live functions are the 475 excluded
`nosplit` ones — `runtime.unlockWithRank` measured 24 instructions or fewer at
its finished size where the reference measured more, the unroller folded it into
`runtime.sysMemStat.add`, and five functions and the object moved. Stage 1's
splice closure does not catch it, because this is a *size* read and not a splice.

So the sound live set is bigger than stage 1 costed, and this is the number that
replaces its 96.2%:

| | recomputed | of | **work-hit rate** |
|---|---:|---:|---:|
| stage 1's projection, small leaf | 156 | 4131 | 96.2% |
| **measured, small leaf** | **1656** | 5083 | **67.4%** |
| **measured, http leaf** | **4385** | 14901 | **70.6%** |

The closure is nothing like the whole module only because of one fact worth
keeping: **1736 of 5083 functions on the small program (4993 of 14901 on http)
have the same body after the whole-module stage as before it.** A function the
stage never moves reads the same at every moment, so freezing it is exact by
construction and it never enters the closure. Without that the closure would be
everything reachable from the live set, which from 475 runtime `nosplit`
functions is most of the runtime.

## What the memoiser found in the IR, which was not the memoiser's bug

Getting a warm compile to emit the same object took fixing two things in cg12's
binary unit format. Both are `ir.CloneFunc`'s problems as much as the memo's —
`CloneFunc` round-trips through this encoder, and `InlineIntoNoSplitCallers` uses
it to snapshot a caller it may have to restore.

**1. `Func.StackPointerWords` was not carried, and it is not recoverable.**
`ir.InferStackPointerWords` *adds* to that map rather than rebuilding it, so a
function that lost it came back with a smaller pointer map and a smaller frame:
`.text` 43712 bytes smaller and `.data` 30344 bytes larger across a whole module.
It is what the collector scans a stack allocation by. The three inline attributes
(`ForceInline`, `CostInline`, `NoInline`) were not carried either. Unit format
version 19 → 20.

**2. Inline provenance was written by value, which is faithful to the values and
not to the graph — and consumers read the graph.** `arm64.buildInlineTree` opens
and closes a DWARF inlined-subroutine context on `stack[k].site == chain[k]`,
pointer identity. Decoding gave every instruction its own chain, so one inlined
region became one region per instruction: **`.debug_info` 3.4 MB → 10.0 MB** for
identical code. Interning equal chains on decode recovers most of it and is still
not the same graph — two splices of one callee at one position are distinct
nodes, and merging them left `.debug_info` 3141 bytes short. It is now a
per-function site table addressed by index, which reproduces the sharing exactly
and is *smaller* on the wire: the small program's memo went 36.9 MB → 30.1 MB.

**Both fixes change what `goc -O` emits, by 208 bytes on the small program's
image.** That is the snapshot-restore path in `InlineIntoNoSplitCallers`: the one
caller it measures and rejects (`runtime.debugCallWrap1.func` on
`runtime_lock_osthread`) was being restored without its stack-pointer-word map.
The change is a fix, it is small, and it is stated here rather than found later.

## What the memo costs

| | small (5083 functions) | http (14901 functions) |
|---|---:|---:|
| memo file on disk | **30.1 MB** | **114.6 MB** |
| per function | 5.9 KB | 7.7 KB |
| writing it (cold) | 0.04 s | 0.12 s |
| reading and validating it (warm) | 0.13 s validate + 0.36 s lookup | 0.90 s + 1.20 s |
| peak RSS, control | — | 3.80 GB |
| peak RSS, warm | — | 4.76 GB |
| peak RSS, cold | — | 5.18 GB |

Two things to read off that:

- **Validation is cheap and stage 1 was right to ask.** Marshalling and hashing
  every post-prefix body, plus one call graph and SCC condensation, is **0.13 s
  of a 16.1 s compile (0.8%) and 0.90 s of 76.1 s (1.2%)**, paid on every build
  whether it hits or misses. It got there by not carrying the module's file table
  in every per-function unit: with the table in, the same walk cost 0.23 s and
  the memo was 81 MB instead of 30 MB.
- **Memory is where this costs most, and the direction is wrong.** http's peak
  RSS goes 3.80 GB → 4.76 GB warm and 5.18 GB cold: the store is 114.6 MB on
  disk but it is held decoded, alongside the module, and a cold run holds every
  finished body's encoding as well as the module it came from.
  `compileRuntimeCapabilityPeakBytes` is 5 GiB and a cold memoised http compile
  is at 5.18 GB. **A memo that is streamed to disk per entry rather than held as
  one map is the first thing to fix in stage 3**, and it is the same change that
  gives it gc's per-entry file layout and an eviction story.

**The back-end half, priced.** The per-function code records the back end would
have to persist — code, relocations, block symbols, line rows, DWARF, Go
metadata, stack maps and the nosplit budget's facts, counted field by field
rather than estimated — are **24.7 MB for the small program's 4131 functions,
6.1 KB each**, against 30.1 MB for the IR half. So a fully persisted Option C
memo for this program is about 55 MB, and buys the 2.00 s the back end costs.

## What is left standing, and what stage 3 has to decide

**1. The sound live set is the thing to attack.** Everything below 45% in the
edit rows is the read closure: 1125 functions on small, 3850 on http, recomputed
not because their memo was invalid but because something live *reads* them and
the stage moves them. Two ways out, and they are the same choice stage 1 named,
now priced properly:

- Store what a reader actually reads at the moment it reads it. That is not the
  whole body: the inliner and the unroller read `funcSize` and
  `inlinableStructure` — an integer and a boolean — and need the body only when
  they splice. Storing a per-round *profile* (size, inlinable, attributes) for
  every function and the body only for the fifth of the module that is ever
  spliced is a much smaller artifact than storing every intermediate body, and it
  would take the closure to nothing.
- Or make the reads not depend on the moment, by giving the inliner a size it can
  agree on across rounds. That is a change to the cost model, not to the cache.

**2. The whole-module identity check should not need the per-function memo to be
exact, and today it carries it.** The 45% rows are the whole-module path; the
per-function path delivers 19–27%. If stage 3 does nothing else, it should still
keep the whole-module check, because it is the only part of the design that is
correct by an argument rather than by measurement.

**3. Persist the back end.** 24.7 MB and a codec, worth 2.00 s of 8.87 and
11.90 s of 41.78 — the largest single number left on the table that does not
need a design decision.

**4. Stream the memo.** Peak RSS goes 3.80 → 5.18 GB on a cold http compile
against a 5 GiB capability ceiling. One file per entry under a fanout, written as
it is produced, fixes the peak and gives eviction at the same time.

**5. Option B.** 19–22 points of the projection, untouched by this branch. The
`-trimpath` work here was its blocker too and is now done.

### Scope notes

- **Packs were not touched.** `make verify-fast` builds and links against them
  throughout and is green.
- The unit format's version went 19 → 20, which is a cold rebuild for anything
  holding an older unit and is handled as one everywhere it is read.
- `cmd/splitprobe` and `cmd/memoc` are measurement scaffolding. `memo/` and the
  `opt`/`ir`/`arm64` hooks are not: they are off unless a caller turns them on
  (`opt.Session.Freeze` with an empty set, `arm64.SetFunctionCodeCache(nil)`),
  and `goc` itself does not turn them on. What `goc -O` emits changed only by the
  208 bytes the two round-trip fixes account for.

## Guards

Scaled to the change, which is a new package, hooks in `opt`, `ir` and `arm64`,
and a compiler-wide path change.

| guard | result |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt` | clean |
| `go test ./ir ./opt ./memo ./arm64 ./analysis` | **all ok** |
| `memo` tests (new, 7) | ok — the digest is the body and not the module, the round trip preserves the body, a corrupted unit is rejected, every key clause is checked including the SCC bit, the store round-trips, a missing file is a cold build, and **a frozen function is not transformed while an unfrozen one is** |
| `go test ./goc -run TestGCDiff\|TestSlogAttrFrame\|TestRuntimeCoverage\|TestEscape.*Diag` | ok (the tests that read the file table) |
| **`make verify-fast`** | **PASS in 4m0s** — build, vet, gofmt, unit, three corpus shards, corpus-parallel, matrix-default, matrix-opt, reducers, all green |
| **byte-identity, memoised vs unmemoised** | **5 scenarios, 2 programs: object and translated assembly identical in all of them**, plus a per-function comparison of every finished body |
| two worktrees of one commit | 0 of 5083 input digests differ, 0 of 4131 post-optimisation, image byte-identical |
| `cmd/splitprobe`, 5 arms | the pipeline split is output-neutral when the pipeline is not rebuilt |

`TestIRVerifyAudit` **does not exist in this tree** — nothing under any name
close to it. `go test ./ir` covers `ir/verify_test.go`, and `make verify-fast`
compiles the whole corpus, which is the check it was presumably meant to stand
for. Said here rather than quietly substituted.

`make verify-fast` failed on its first run, on `TestAllocationCensus`, and the
failure was the trimpath fix working: eleven of the baseline's 14520 lines
identify a type by a symbol whose name embedded `/home/evan/.ccwork/...`, so the
committed baseline could only ever have matched on the machine that recorded it.
Regenerated; every one of the eleven changes is confined to the type-symbol
column and **not one allocation moved between the heap and the frame**.

Not run, per the brief: the corpus suite on its own, the capability matrix on its
own, `make test-unit`, the four audits on their own, the census on its own,
determinism sweeps, the crash loops. (`verify-fast` runs the matrix, three corpus
shards and the reducers as part of its own tier; that is the broad smoke the
brief offered, not those gates run separately.)

## Verdict

**Both blockers cleared.** Absolute paths are out of `ir.SrcPos` *and* out of the
three symbol families that were hashing a position — two worktrees of one commit
now produce byte-identical images. The pipeline split is not the `changeLog`; it
is the jump-thread budget, and `opt.Session` makes the split free.

**Output is byte-identical**, on every scenario measured: two programs, cold,
warm, a root-package edit and a leaf edit, object and translated assembly both.

**The achieved saving is 45% warm against a 77–82% projection**, and the three
pieces of the gap are named and sized: Option B is 19–22 points of it and is not
built; the back end's memo is 12–16 points and is implemented but not persisted;
and the rest is the live-set closure, which is where stage 1's arithmetic was
wrong — its 96.2% work-hit rate assumed recomputing the splice closure, and the
splice closure **does not emit the same bytes**. The sound closure gives 67–71%,
which is why an edit saves 19–27% and not 45%.

---

# Gate: `integration/optionc` (c114580) — independent verification

Branch under test: `integration/optionc` = `c11458068c6b9946add72e6309a0b919e383589e`,
three real changes merged onto `main` `21ca7b3841fdb087223abb06e9c91b66e0cd8c8d`.
Gate branch: `ccwork/optionc-gate`.

Host: aarch64 Linux, 64 cores, 250 GiB total / 243 GiB available, go1.26.1,
cc 13.3.0 (Ubuntu), kernel 6.8.0-60-generic. Machine exclusive to this job.

Diagnose-only brief: nothing in the compiler was fixed or refactored by this
gate. All eight baselines were regenerated from the merged tree and every one
came back byte-identical to what is committed, so this report is the only
change in the tree.

**Status: COMPLETE.** Every item below was run to exit and its exit status
watched; nothing was left running.

## Item 0 — build

`go build ./...` clean (3.2 s).

## FINDING 1 (integration defect, merge blocker) — `TestEveryTestIsParallelOrListedAsSequential` fails on the merge

`make verify-full`'s `corpus-parallel` item fails, exit 1, 272 s:

```
--- FAIL: TestEveryTestIsParallelOrListedAsSequential (0.06s)
    parallelpolicy_test.go:79: TestIRVerifyAudit (irverifyaudit_test.go): does not call t.Parallel
        as its first statement, and sequential_tests.txt does not list it.
    parallelpolicy_test.go:79: TestModuleRoundTripsThroughTheBinaryFormat (irverifyaudit_test.go):
        does not call t.Parallel as its first statement, and sequential_tests.txt does not list it.
FAIL	github.com/evanphx/cg12/goc	269.195s
```

**Diagnosis — a semantic merge conflict, not a defect in either side.**
`ccwork/ir-verify-entry-blocks` was cut from `76069d9`, and
`goc/parallelpolicy_test.go` / `goc/sequential_tests.txt` do not exist at that
commit (`git cat-file -e 76069d9:goc/parallelpolicy_test.go` → absent). They
landed on `main` later (`142e907`). So the branch added two new tests in
`goc/irverifyaudit_test.go` under a policy that did not yet exist, the textual
merge is clean, and the merged tree violates a rule neither parent violated.

**Blast radius.** Every entry point that runs the `goc` package fails:
`go test ./goc/...`, `make test-goc-corpus`, `make verify-fast`, `make verify-full`.
It is a policy assertion, not a compiler-behaviour assertion — no emitted code
is implicated — and the other three corpus shards, which carry the four audits,
passed. But as merged, the tree is red.

**Not repaired here** (diagnose-only brief). The fix is one of: add
`t.Parallel()` as the first statement of both tests, or list both names in
`goc/sequential_tests.txt` with a reason tag. `TestIRVerifyAudit` calls
`auditCorpus`, the shared `sync.Once` corpus compile that the four audits use
and that `sequential_tests.txt` already lists the other users of, so the
sequential list is the likely correct side.

## FINDING 2 (pre-existing, to be confirmed against a `main` control) — `TestCheckedRuntimeCoverageBaselineDenominator`

`make verify-full`'s `goc-cmd` item fails, exit 1, 367 s, on exactly one test —
the one the brief names as known-failing on `main`:

```
--- FAIL: TestCheckedRuntimeCoverageBaselineDenominator (0.03s)
    capability "gc-invariants/slice-tail-pointer" is in neither the accepted baseline nor
    testdata/runtime_coverage_baseline_pending.json; record why the baseline does not cover it,
    or rerun and accept a new baseline
FAIL	github.com/evanphx/cg12/cmd/goc	365.901s
```

One `--- FAIL` in the whole package (`grep -c '^--- FAIL'` = 1). A `main`
control for this exact test is recorded below.

## THE PROPERTY THAT MATTERS MOST — memoised output vs unmemoised, 16 programs, 5 arms each

Harness: `$TMPDIR/gate/memoident.sh`, driving `cmd/memoc` (the branch's own
control-arm driver: `-no-memo` runs the identical code path with the store
ignored). Default `-closure read`, which is the sound closure and memoc's
default. Five arms per program:

| arm | what it is |
|---|---|
| `control` | `-no-memo`, no store — the reference |
| `cold` | `-memo M` with `M` absent: every function a miss, store written |
| `warm` | `-memo M` from the cold run: every function a hit |
| `e-ctl` | `-no-memo` on an **edited** source — the reference for the edit |
| `e-memo` | `-memo M'`, `M'` the **stale** store from the unedited source |

The edit is a root edit applied mechanically to every program: a new
package-level `var gateSink int`, a new `func gateNoise(n int)` that writes it,
and a call to it inserted as the first statement of `main`. That is the case
`memo/memo.go`'s doc comment names as the risky one — a *missed* function
reading *frozen* ones, plus a function that is not in the memo at all — and the
harness asserts the edited object actually differs from the unedited one, so a
scenario cannot pass by being inert.

**Result: 16 of 16 programs, object bytes identical across all five arms. No
difference of any kind, in any arm, on any program.** 80 `memoc` compiles.

| program | funcs | warm hits/misses | edit hits/misses | verdict |
|---|---:|---|---|---|
| `hello.go` | 2744 | 2744 / 0 | 1218 / 3 | identical |
| `gc_struct.go` | 2748 | 2748 / 0 | 1215 / 3 | identical |
| `runtime_closure_captures_roots_gc.go` | 2748 | 2748 / 0 | 1193 / 3 | identical |
| `interface_slice_equality.go` | 2743 | 2743 / 0 | 1214 / 3 | identical |
| `runtime_goroutine_channel.go` | 2745 | 2745 / 0 | 1218 / 4 | identical |
| `nested_defer.go` | 2747 | 2747 / 0 | 1220 / 4 | identical |
| `runtime_lock_osthread.go` | 2747 | 2747 / 0 | 1218 / 4 | identical |
| `runtime_gc_type_mask_padding.go` | 2752 | 2752 / 0 | 1216 / 4 | identical |
| `runtime_cleanup_frame_retention.go` | 3212 | 3212 / 0 | 1646 / 3 | identical |
| `reflect_makefunc.go` | 3710 | 3710 / 0 | 2072 / 4 | identical |
| `fmt_sprintf.go` (the branch's "small") | 5083 | 5083 / 0 | 3243 / 3 | identical |
| `allocation_counts.go` | 5214 | 5214 / 0 | 3572 / 12 | identical |
| `placement_bench/flate/main.go` | 5458 | 5458 / 0 | 3530 / 5 | identical |
| `runtime_defer_capture_allocs.go` | 5951 | 5951 / 0 | 3762 / **52** | identical |
| `stdlib_http_cookiejar.go` | 13944 | 13944 / 0 | 9547 / **36** | identical |
| `stdlib_http_tls_client_server.go` (the branch's "http") | 14901 | 14901 / 0 | 10464 / **37** | identical |

Fourteen programs the branch did not test, including two the tree has history
with: `runtime_defer_capture_allocs.go` (RUNTIME_PLAN 5.10's 25-images-in-30-compiles
program) and `stdlib_http_cookiejar.go` at 13 944 functions.

### Caveat on the branch's wording: "translated assembly" is a much weaker half of that claim than it sounds

The branch reports "object **and translated assembly** identical". The assembly
`memoc` writes is `arm64.WriteObjectAndAssembly`'s second return, which is
`bundle.source` — *the GNU assembly translated from the module's hand-written
Plan 9 sources*, not the compiler's generated code. Measured here: it is the
same 157 015-byte, 7283-line blob for `hello.go`, `gc_struct.go`,
`runtime_closure_captures_roots_gc.go`, `interface_slice_equality.go`,
`nested_defer.go`, `runtime_lock_osthread.go`, `runtime_goroutine_channel.go`
and `runtime_gc_type_mask_padding.go` — eight different programs, one digest
(`ae07e2c363f06b2a`) — and it is identical between the edited and unedited
sources of every program. It varies only with which Plan 9 assembly files the
module pulled in (five distinct digests over the 16 programs).

So that half of the claim detects a change in the *set of runtime assembly files
included* and nothing else. It is not wrong, it is nearly empty. **The object
comparison is the load-bearing one**, and it passes on all 16 programs. Stated
here so nobody reads "assembly identical" as "generated code compared twice".

## SECOND PROPERTY — one commit, two checkout paths, byte-identical image

Harness: `$TMPDIR/gate/twopath.sh`. Two `git worktree`s of `c114580` at paths of
different length (53 and 104 characters), `cmd/goc` built *inside each* so
`goc.StdlibRoot` (which comes from `runtime.Caller(0)`) points at that
worktree's own `stdlib/`. The two compiler binaries do differ, as they must —
`A=2dc77a9f94e3da48`, `B=ee51f9c2f74a3af4` — which is what makes the emitted
images a real comparison rather than a tautology.

**Six programs × two arms = 12 comparisons, 12 IDENTICAL, exit 0.**

| program | no `-O` | `-O` |
|---|---|---|
| `hello.go` | `aa4fdf3335aa067c` | `1b62c3a50fb185b7` |
| `fmt_sprintf.go` | `1789d833c509426f` | `a4bf61a51728a3c5` |
| `gc_struct.go` | `74e0bddf6bf31ce5` | `d5e926456bac58e2` |
| `runtime_defer_capture_allocs.go` | `bc789eae160ea117` | `ca3a0e49b7bb240a` |
| `reflect_makefunc.go` | `87ef2cde3323ed77` | `8abbd3d97765658d` |
| `stdlib_http_tls_client_server.go` | `a26650ad4c1f3677` | `42334b38212b7581` |

Every digest above is the *same* value from both checkouts. The `main` control
for the same experiment is below.

### `main` control for the same experiment — 8 of 8 DIFFERENT

Same harness, `REF=main` (`21ca7b3`), two worktrees at 58 and 109 characters:

```
goc/testdata/hello.go                          none DIFFERENT 567149451450e7c3 / 1aaddf7ad6269e69
goc/testdata/hello.go                          -O   DIFFERENT 60bfbfb21916c32c / a7968d4e81141e2d
goc/testdata/fmt_sprintf.go                    none DIFFERENT dd72ba928f2264d7 / 5b34540aeac2d451
goc/testdata/fmt_sprintf.go                    -O   DIFFERENT 7a921cce9e3a2b9b / 7e0dc1030e32262e
goc/testdata/gc_struct.go                      none DIFFERENT 36745d23cfe93709 / 374aade320f2f751
goc/testdata/gc_struct.go                      -O   DIFFERENT 9fd65558ddd8ee7a / 1288874ef937bf56
goc/testdata/stdlib_http_tls_client_server.go  none DIFFERENT e06547c3e9d722aa / 83018d3032365c25
goc/testdata/stdlib_http_tls_client_server.go  -O   DIFFERENT 9159074b4629767c / bb355641548bb2f1
```

So the property is **newly true**, and by a wide margin: on `main` every one of
the eight comparisons differs, on the merge every one of the twelve agrees. The
`-trimpath` work (`goc/trimpath.go`, plus `positionKey` for the three symbol
families named from a source position) is what moved it, and it is the first
time in this tree that two checkouts of one commit produce the same image.

## Item 1 — `make verify-full` (the whole tier, plus its `main` controls)

Ran `make verify-full`. **Verdict: FAIL in 2934 s (48 m 54 s), 2 items of 24
failed** — the two findings above, and nothing else.

| item | seconds | result |
|---|---:|---|
| build | 0 | PASS |
| vet | 3 | PASS |
| gofmt | 0 | PASS |
| unit | 13 | PASS |
| corpus-parallel | 272 | **FAIL** (Finding 1) |
| corpus-sequential-0 (carries the four audits) | 240 | PASS |
| corpus-sequential-1 | 109 | PASS |
| corpus-sequential-2 | 114 | PASS |
| goc-cmd | 367 | **FAIL** (Finding 2) |
| ruby | 47 | PASS |
| matrix-default-0..3 | 84 / 78 / 42 / 49 | PASS |
| matrix-opt-0..3 | 144 / 137 / 129 / 45 | PASS |
| determinism (406 programs × 4 rounds) | 427 | PASS |
| determinism-opt (406 × 4, the arm the branch changes) | 902 | PASS |
| reducers (full counts) | 109 | PASS |
| control-corpus (`go test ./goc/...` on `main`) | 533 | **PASS** |
| control-matrix-default (`main`) | 162 | PASS |
| control-matrix-opt (`main`) | 203 | PASS |

The controls were measured cold — the cached entry on this box was under a
different key (a different `main`), so nothing was reused.

**`control-corpus` passing is the attribution for Finding 1.** `go test ./goc/...`
is green on `main` (`21ca7b3`) and red on the merge, on the same box, in the
same run. The corpus failure is introduced by the merge and is not a
pre-existing condition.

### Capability matrix — 368/368 on each arm, pass sets identical to `main`

Counted from the `-v` logs, four shards per arm:

| arm | subtests PASS | subtests FAIL |
|---|---:|---:|
| default (shards 0–3: 92+92+92+92) | **368** | **0** |
| `-O` (shards 0–3: 92+92+92+92) | **368** | **0** |
| `main` control, default | 368 | 0 |
| `main` control, `-O` | 368 | 0 |

The branch's 368-name pass set was diffed against the `main` control's, on both
arms: **`diff` empty on both**. One `EXPECTED FAILURE` marker on the default arm
in both trees (shard 3 on the branch; the unsharded control), so that population
is unmoved too.

### Determinism (item 4, first half)

`scripts/determinism-check.sh -corpus` (406 programs × 4 rounds) and the same
with `-O` both PASS inside `verify-full`, 427 s and 902 s. That is the tree's
only measurement that drives every corpus program to a written object and
compares bytes, and it is green on both arms.

### Reducers at full counts (item 5, branch arm)

`scripts/reducers.sh full` PASS in 109 s — the gate counts, which are
`runtime_gc_type_mask_padding` 20× at `GOGC=100` and 20× at `GOGC=10`,
`flate` 250× at each of `GOGC=100` and `GOGC=10`, `p256` 100× at `GOGC=10`,
`runtime_lock_osthread` 400×. A `main` control of the same script is below.

## Item — baselines regenerated from the merged tree

Every one regenerated in place from `c114580` (never by picking a side), in
dependency order — the census first, because the two GC differentials read it.

| baseline | command | wall | file changed? |
|---|---|---:|---|
| `alloc_census_baseline.txt` | `-run TestAllocationCensus -update-alloc-census-baseline` | 95 s | **no** |
| `frame_escape_baseline.txt` | `-run TestFrameEscapeAudit -update-frame-escape-baseline` | 96 s | **no** |
| `loop_alias_baseline.txt` | `-run TestLoopAliasAudit -update-loop-alias-baseline` | 95 s | **no** |
| `escape_shadow_baseline.txt` | `-run TestEscapeShadowPlacement -update-escape-shadow-baseline` | 107 s | **no** |
| `escape_gc_differential.txt` | `-run TestEscapeDifferentialAgainstGC -escape-gc-differential -update-…` | 11 s | **no** |
| `escape_gc_reason_differential.txt` | `-run TestEscapeReasonDifferentialAgainstGC -escape-gc-reason-differential -update-…` | 197 s | **no** |
| `slog_allocations_baseline.txt` | `-run TestSlogAllocationsAgainstGC -slog-allocations -update-slog-allocations` | 13 s | **no** |

Each ran to `exit 0` and each log ends `baseline rewritten; rerun without …`, and
every file's mtime moved (19:30:04 … 19:38:45) — so these were *written* and then
found byte-identical, not skipped. `git status` after all seven: clean except
this report.

**What moved, and why nothing did.** The brief expected movement because two of
the three changes move emitted code. It does not appear here, and that is the
correct answer rather than a suspicious one:

* Six of the seven come from a corpus pass that stops at IR and escape analysis —
  before `opt`'s whole-module stage and before the back end. Branch 1's real
  effect is that `opt.InlineIntoNoSplitCallers` can now clone closures, which is
  a whole-module pass *after* that stopping point. Branch 3's two round-trip
  fixes (`StackPointerWords`, inline provenance) are exercised by encode/decode,
  which the audit path does not do. So these six are invariant *by construction*,
  and the brief is right that their invariance is not evidence about emitted code.
* The seventh, `slog_allocations_baseline.txt`, does see the back end. It counts
  mallocs per operation over 32 cases; a 208-byte `.text` move and a handful of
  newly-inlinable closures do not change how many times a case allocates. Its
  invariance is a (weak) positive signal and not a construction.
* The one instrument in this set that both sees the back end *and* is directly
  downstream of branch 1's change is the nosplit debt register — reported
  separately below.

For the record, the census baseline the branch itself regenerated (11 lines,
type-symbol column only, `65f7e89`) reproduces exactly here: the trimmed symbol
names are stable across this checkout too, which is the same property the
two-checkout-path test measures from the other side.

### Nosplit debt register — regenerated, unchanged

`scripts/nosplit-debt-regen.sh -update -j 32`, exit 0 in 566 s (the three-arm
sweep: `pack` per capability-matrix pack root with and without `-O`, `whole` for
every corpus program with no prebuilt runtime, `split` for every program against
the prebuilt packs — roughly 1600 compiles). **`arm64/nosplit_debt.go` is
byte-identical to what is committed.**

This is the interesting one, because it is the single instrument in the set that
is both downstream of the back end *and* directly downstream of branch 1: the
whole point of the `ir.Verify` fix is that `opt.InlineIntoNoSplitCallers` can now
clone and measure closures it silently declined before, and the debt register is
the record of which nosplit chains exceed the reserve. Newly-enabled inlining
into nosplit callers did **not** move any chain past the reserve, and did not
retire any recorded chain either. `git status` after all eight regenerations:
clean except this report.

## Item 2 — `make test-unit`, `make test-goc-cmd`, with `main` controls

| run | tree | exit | wall |
|---|---|---:|---:|
| `make test-unit` | branch | **0** | 12 s |
| `make test-unit` | `main` control | 0 | 12 s |
| `make test-goc-cmd` | branch | **2** (`make` wrapping `go test`'s 1) | 469 s |

`test-goc-cmd`'s only failure is `TestCheckedRuntimeCoverageBaselineDenominator`
— one `--- FAIL` in the package, same message as inside `verify-full`.

**Controls, run directly on both trees, same box, same minute:**

| test | `main` (`21ca7b3`) | merge (`c114580`) |
|---|---|---|
| `TestCheckedRuntimeCoverageBaselineDenominator` | **FAIL** (exit 1) | **FAIL** (exit 1) |
| `TestEveryTestIsParallelOrListedAsSequential` | **PASS** | **FAIL** (exit 1) |

So Finding 2 is confirmed **pre-existing on `main`** — identical message,
capability `gc-invariants/slice-tail-pointer` missing from both the accepted
baseline and the pending list — and is not attributable to this merge. Finding 1
is confirmed **introduced by this merge**.

## Item 3 — the four audits, `TestIRVerifyAudit`, and the census twice

All in check mode against the baselines regenerated above (which are the
committed ones).

| run | result | wall |
|---|---|---:|
| `make verify-audits` (`TestAllocationCensus`, `TestFrameEscapeAudit`, `TestLoopAliasAudit`, `TestEscapeShadowPlacement`) | **all 4 PASS**, `ok github.com/evanphx/cg12/goc 83.941s` | 85 s |
| `TestIRVerifyAudit` | **PASS** — `1 559 314 function verifications across 406 programs, all clean` | 84 s |
| `TestAllocationCensus`, run 1 | PASS (82.95 s) | 85 s |
| `TestAllocationCensus`, run 2 | PASS (82.48 s) | 84 s |

**Census twice: the two `-v` logs are identical once wall-clock numbers are
stripped.** No run-to-run drift.

`TestIRVerifyAudit` is the test the `optionc-stage2` report said "does not exist
in this tree" — it exists in the merge, it runs, and its number is the direct
measurement of what branch 1 fixed: 1 559 314 verifications over the whole
corpus, zero failures, where the same walk on `main` would reject roughly 4–6% of
functions. (It is also the test whose two names trip Finding 1.)

## Item 4 — determinism, both arms, plus the parallel back end

* `scripts/determinism-check.sh -corpus` — PASS (427 s), inside `verify-full`.
* `scripts/determinism-check.sh -corpus -O` — PASS (902 s), inside `verify-full`.
* `TestParallelBackendIsByteIdenticalToSerial` — **PASS**, all six worker counts
  (1, 2, 3, 8, 64, 256) byte-identical to the serial object, `ok ./arm64 0.228s`.
* Two checkout paths — 12/12 identical, reported above.

## Item 5 — reducers, branch and `main` control

Both arms ran `scripts/reducers.sh full`, the gate counts.

| case | branch | `main` control |
|---|---|---|
| `runtime_gc_type_mask_padding` GOGC=100, `GOMAXPROCS=3`, 20× | 0 of 20 failed | 0 of 20 failed |
| `runtime_gc_type_mask_padding` GOGC=10, `GOMAXPROCS=3`, 20× | 0 of 20 failed | 0 of 20 failed |
| `flate` GOGC=100, 250× | 0 of 250 failed | 0 of 250 failed |
| `flate` GOGC=10, 250× | 0 of 250 failed | 0 of 250 failed |
| `p256` GOGC=10, 100× | 0 of 100 failed | 0 of 100 failed |
| `runtime_lock_osthread` GOGC=100, 400× | 0 of 400 failed | 0 of 400 failed |
| total | **clean**, 109 s | **clean**, 104 s |

Both flate and p256 panic on a wrong answer rather than merely crashing, so
these are correctness loops. 1040 executions per arm, 2080 in total, zero
failures on either side.

## Item 6 — `make bench-crypto` and `make bench-perf`, on an otherwise idle box

Run alone, nothing else on the machine (load average 5.3 at launch, from these
two). Both pre-flights passed on their own terms: crypto pinned to core 63
(calibration burst got 99.4% of it, 0.0% lost, 1.0% spread, 2 of 64 cores busy),
perf pinned to core 62 (99.4%, 0.6% lost, 0.8% spread). **Both exit 0.**

### `make bench-crypto` — PASS, `ok ./goc 103.735s`

| case | baseline index | this run | change | resolved | verdict |
|---|---:|---:|---:|---:|---|
| `p256/sign-verify` | 24.0648 | 23.9998 | −0.3% | +0.1% | within tolerance (6%) |
| `p256/verify` | 16.9991 | 16.9714 | −0.2% | +0.0% | within tolerance |
| `p384/sign-verify` | 20.3676 | 20.3653 | −0.0% | −0.2% | within tolerance |
| `rsa2048/sign-verify` | 2.3470 | 2.3401 | −0.3% | −0.3% | within tolerance |

The brief asked for the triage note to be read before attributing a single-digit
swing to code quality. It did not need to be applied: every movement here is
**≤0.3%**, and the "resolved" column — the part of the change that survives both
confidence intervals — is at most 0.3% and mostly indistinguishable from zero.
The baseline header's own account of the residual placement effect is "low single
digits"; this run is an order of magnitude inside that. Branch 1 growing `.text`
and branch 3 moving it 208 bytes produced **no measurable movement on the crypto
signing path**, so there is nothing here to triage as either regression or
placement.

### `make bench-perf` — PASS

All 40 rows (eleven programs × their cases, plus each program's
`control/spin-fixed-work`) verdict **within tolerance**. The largest raw changes
and what they resolve to:

| row | change | resolved | tolerance |
|---|---:|---:|---:|
| `sortmap map/build-probe` | −5.2% | −3.6% | 14.5% |
| `conc chan/pingpong-unbuffered` | −3.9% | +2.7% | 5.0% |
| `conc goroutine/spawn-join` | −3.2% | −1.9% | 16.4% |
| `gcpress gc/alloc-churn` | +3.1% | −1.0% | 9.5% |
| `regexp regexp/replace` | −3.0% | −2.0% | 10.7% |
| `gcpress gc/live-heap-churn` | −2.3% | −2.9% | 17.5% |
| `json json/marshal` | +2.6% | −0.6% | 5.5% |
| `text text/sprintf` | +1.9% | −5.0% | 16.6% |

A negative "resolved" means the run cannot tell the movement from zero. Every
row with a change above 2% has a negative or sub-3% resolved figure, and the
eleven `control/spin-fixed-work` rows — the same fixed integer work in every
binary, which is the suite's own check that the box did not move — are all
within ±0.3%. Nothing here is a performance signal.

## Item 7 — the memoiser's saving, re-measured independently

Harness: `$TMPDIR/gate/memosaving.sh`. Three arms per program — `-no-memo`
(baseline), `-memo M` with `M` removed first (cold), `-memo M` in place (warm) —
**interleaved** within each repetition so any drift in the box hits all three
arms equally. Five repetitions each. Run alone on an idle box, one compile at a
time.

| program | baseline | cold | warm | **warm saving** | **cold overhead** | warm hits |
|---|---:|---:|---:|---:|---:|---|
| `fmt_sprintf.go` (small, 5083 funcs) | 16.25 s | 16.85 s | 8.21 s | **49.4%** | **+3.7%** | 5083/5083, whole-module |
| `stdlib_http_tls_client_server.go` (http, 14 901 funcs) | 75.57 s | 78.67 s | 41.58 s | **45.0%** | **+4.1%** | 14901/14901, whole-module |

Medians of five; the individual runs, which show how tight this is:

```
small  baseline  16.27 16.22 16.25 16.19 16.25
       cold      16.78 16.86 16.85 16.85 16.75
       warm       8.28  8.23  8.21  8.21  8.16
http   baseline  75.44 75.57 75.94 75.47 75.64
       cold      78.31 78.84 78.67 78.31 78.73
       warm      42.18 41.55 41.58 41.96 41.47
```

**The branch's numbers reproduce.** It claims 45% warm on both and +3.8%/+4.1%
cold; measured here, 49.4%/45.0% warm and +3.7%/+4.1% cold. The small program
comes out 4 points *better* than claimed, http exactly as claimed, and both cold
overheads land on the claimed figure. Across all 30 compiles in this
measurement the object digest never moved (`237ad34b7732f6b3` for small,
`76fe6efbcd80dc4c` for http), so the timing runs are a thirtieth independent
confirmation of the identity property.

Two things the 45% does **not** cover, both of which the branch states and
neither of which this gate contradicts: it is the *whole-module-hit* path (no
edit), and the branch's own edit rows are 19–27%; and the back-end memo is
implemented but not persisted, so a warm run still pays the back end unless it
is in the same process.

## What was not run, and what is therefore unverified

Stated plainly rather than left to inference.

* `make test-goc-coverage` and `make runtime-cover-diff` — the instrumented
  coverage report and its baseline diff. Not in the brief, and neither verify
  tier schedules it. The `cmd/goc` test that *checks the committed baseline's
  denominator* did run, and it is Finding 2.
* `make test-cruby` (`scripts/cruby-diff.sh`) — out of tree, not in the brief.
  `test-ruby` (the C differential and the miniruby regressions) did run, inside
  `verify-full`, and passed in 47 s.
* The memoiser's `-closure splice` arm was not exercised. `memoc` defaults to
  `-closure read`, the branch documents `splice` as *not* emitting the same
  bytes, and every measurement here is on the default.
* No conclusion is offered about branches 1 and 3's separate effects on emitted
  code size. The `.text` growth and the 208-byte move are the branch's own
  measurements; this gate measured their *consequences* (benchmarks, determinism,
  matrix, reducers, debt register) and found none, but did not re-measure the
  section sizes.

## Verdict

Every measurement asked for was run to completion and watched to exit. Two items
failed and both are accounted for.

**Byte-identity of memoised output: CONFIRMED, and more strongly than the branch
claimed it.** 16 programs (the branch tested 2), five arms each, 80 `memoc`
compiles plus 30 more in the timing run: the object bytes are identical in every
arm of every program — warm, cold, and against a stale memo after a root edit
that provably moved the output. The one qualification is on the branch's
*wording*: the "translated assembly" half of its claim is the module's
translated Plan 9 assembly, the same 157 KB blob for eight of the sixteen
programs, and it is close to empty as a check. The object comparison carries the
claim, and it holds.

**Two checkout paths: CONFIRMED, and newly true.** 12 of 12 comparisons
identical on the merge (six programs, `-O` and unoptimised, two worktrees at 53
and 104 characters, each with its own `cmd/goc` build). The same experiment on
`main` is 8 of 8 **different**. This is the first commit in the tree where two
checkouts of one commit produce the same image.

**The measured saving reproduces**: 49.4% warm on the small program and 45.0% on
http against a 45%/45% claim, at +3.7%/+4.1% cold overhead against a +3.8%/+4.1%
claim.

**But the merged tree is red.** `go test ./goc/...` — and therefore
`make test-goc-corpus`, `make verify-fast` and `make verify-full` — fails on
`c114580` and passes on `main` (`21ca7b3`), measured on this box in the same run
as `control-corpus`. The cause is Finding 1: a semantic merge conflict between
branch 1's two new tests and a parallel-test policy that landed on `main` after
branch 1 was cut. No emitted code is implicated and the repair is two lines, but
merging as it stands turns `main`'s own gate red.

### **NOT SAFE TO MERGE TO MAIN** — on one defect, with a named repair

Not safe *as `c114580` stands*. It becomes safe when `TestIRVerifyAudit` and
`TestModuleRoundTripsThroughTheBinaryFormat` either call `t.Parallel()` as their
first statement or are listed in `goc/sequential_tests.txt` with a reason tag
(the sequential list is the likely correct side: `TestIRVerifyAudit` shares the
audits' `sync.Once` corpus compile, and `sequential_tests.txt` already lists its
co-users), and `go test ./goc/...` is re-run green. Nothing else found in this
gate stands between this merge and `main`:
`TestCheckedRuntimeCoverageBaselineDenominator` fails identically on `main` and
is not this merge's to answer for.


---

# Stage 0 of per-package caching: making goc's IR binary format lossless

Branch `ccwork/ir-format-lossless`, off `integration/optionc-fix` (`3f4a6b8`).

The cache in BUILD_CACHE.md's Option C writes a package's compiled form,
including its IR, to disk and reads it back on a later compile. That is sound
only if the format carries everything. It did not. This is the list of what it
dropped, the fix for each, the test that fails without it, and what each one
moved in the emitted object.

None of these are hypothetical-until-the-cache-lands defects. `ir.CloneFunc`
clones a function by encoding and decoding it through this exact encoder, and two
production callers use it: `opt.InlineIntoNoSplitCallers` snapshots a caller it
may have to restore, and `arm64.measureFunction` clones every function whose
frame the nosplit budget measures. A field the encoder drops is a field those two
lose today.

## How the movement is measured

One harness, two modes, one program (`goc/testdata/hello.go`, the whole Go
runtime behind it -- 5083 functions):

- **plain** -- `CompileExecutable` + `opt.OptimizeModule` + `arm64.CompileObject`.
  No explicit round trip. This is what `goc -O -c` emits, and it moves when a
  format fix changes what `ir.CloneFunc` gives back to the nosplit passes.
- **roundtrip** -- the same, with `MarshalBinary` + `DecodeModuleUnverified`
  between the optimizer and the backend. This is the cache path. The gap between
  it and **plain** is what the format loses over a whole module.

Both report every ELF section size and the object's sha256. The harness is
scratch (it lives outside the tree); it is 90 lines and reproduced in this report
where it matters.

## Baseline, before any fix

| section | plain | roundtrip | gap |
|---|---:|---:|---:|
| `.text` | 1527964 | 1527964 | 0 |
| `.data` | 3624240 | 3624240 | 0 |
| **`.rela.data`** | **265392** | **304728** | **+39336** |
| `.rela.text` | 905184 | 905184 | 0 |
| `.debug_info` | 1321198 | 1321198 | 0 |
| object | 9991688 | 10031024 | +39336 |

`39336 / 24` is exactly **1639 extra `Elf64_Rela` entries**. That is
`DataItem.RelativeTo` (drop 1) and it is the only whole-module loss visible in
the object today: every other section is byte-identical across the round trip.
A relative item that decodes as an absolute symbol reference takes a different
relocation path in the backend, and 1639 data words on this program take it.

## Drop 1 — `DataItem.RelativeTo`. Real in goc, and it was the whole gap.

`encData` wrote `Sym`, `Off`, `Str` and not `RelativeTo` (`ir/binary.go`). An item
with both `Sym` and `RelativeTo` is `Sym - RelativeTo`: a 32-bit displacement from
the module's data base. Dropping `RelativeTo` turns it into a full symbol address,
and `arm64/mc.go:386` / `:692-703` take a **different relocation path** for each.

goc emits these everywhere: every `abi.Type`'s name offset and type offset, and
each method's two offset words (`goc/compile.go:8322`, `:8418-8419`, `:8459`,
`:8477-8478`, `:8515`), plus `internal/permodule`'s probe descriptors.

Nothing detected it. `ir.Verify` does not look at `Data` at all, and
`memo.DataDigest` digests `(&ir.Module{Data: m.Data}).MarshalBinary()` -- through
the same lossy encoder -- so the digest was equal on both sides of a difference it
had itself erased.

**Test that fails without it:** `TestDataRoundTripsEveryField` (`ir/binary_total_test.go`).
Before the fix it names the field:

```
    Diff:
    --- Expected
    +++ Actual
        Sym: (string) (len=5) "other",
    -   RelativeTo: (string) (len=4) "base",
    +   RelativeTo: (string) "",
```

**Emitted-code movement.**

| | before fix 1 | after fix 1 | moved |
|---|---:|---:|---:|
| roundtrip `.rela.data` | 304728 | 265392 | **−39336 (−1639 relocations)** |
| roundtrip object | 10031024 | 9991688 | −39336 |
| roundtrip sha256 | `f451f7db…` | `fc378a0a…` | — |
| plain sha256 | `fc378a0a…` | `fc378a0a…` | unchanged |

The round-tripped object is now **byte-identical to the object compiled without a
round trip** (`fc378a0a…` on both sides). This one fix closed the entire
whole-module gap on this program: encode, decode, emit is now a fixed point over
the emitted object, not merely over the encoding.

`plain` does not move, and that is the correct answer rather than a null result:
`Data` is module-level, and `ir.CloneFunc` puts one function into a scratch module
that has no `Data` at all. The two `CloneFunc` callers cannot reach this field.

## The census, which is the direct answer to "real, or safe by luck?"

An object-size delta of zero is consistent with two very different things: a
field goc never produces, and a field whose loss happens not to change this
program's code. So the harness also counts, in the real compiled+optimized module
for `hello.go`, how many instances of each dropped field exist:

```
census funcs=2152 data=6361 types-reached=63
  DataItem.RelativeTo   1639
  Data.GoTypeLink       499
  Const.Thread          0
  AggType.Packed        0
  Jmp.Likely            0
  Func.lowered          0
  Module.SymAttrs       84
  Module.SymAlign       0
  Module.Aliases        0
  Module.AllocDecisions 227   (diagnostic-only, deliberately not carried)
```

`DataItem.RelativeTo` is **1639**, and the round trip added exactly **1639**
relocations. The two numbers are the same number.

**Real in goc: `DataItem.RelativeTo` and `Module.SymAttrs`.** Everything else on
the list is produced only by the `cc/` front end and is safe by luck in goc --
which is a statement about goc's current output and not about the format, and it
stops being true the moment a Go construct starts emitting one.

## Drop 2 — `Const.Thread`. Safe by luck in goc; a wrong address in cc.

`encConst` wrote `Kind`, `Cls`, `Int`, `Flt`, `Sym` and not `Thread`
(`ir/binary.go`). A thread-local symbol constant is reached through the TLS ABI;
without the flag it decodes as an ordinary symbol address -- the same address in
every thread, where the point of the constant was that it is not.

Two things make it worse than a single wrong flag. `internConst`'s key *does*
include `Thread` (`ir/build.go:185`), so a function holding both `&g` and
thread-local `&g` has two distinct constants before the round trip and one after
it -- every use of one silently becomes a use of the other. And `opt/inline.go`
re-interns a spliced callee's thread constant through `f.ThreadSym`, so a callee
that came out of a cache spreads the demotion into its callers.

Produced by `cc/expr.go` and `cc/compile.go` (`__thread`); goc emits none.

**Test that fails without it:** `TestConstRoundTripsEveryField`, which reports
`Thread: (bool) true` expected, `false` actual.

**Emitted-code movement: none, on either path.** `plain` and `roundtrip` are both
byte-identical to their post-fix-1 objects (`fc378a0a…`). The census says why:
goc produces zero thread-local constants. That is evidence the field is genuinely
unreachable from goc today, not evidence the fix is inert.

## Drop 3 — `AggType.Packed`. Safe by luck in goc; a different struct layout in cc.

`encType` wrote `Name`, `Align`, `Size`, `Opaque`, `Union`, fields and cases, and
not `Packed` (`ir/binary.go`). `AggType.walk` reads it (`ir/type.go:282`, `:291`)
to place members with no inter-member padding and to stop a member raising the
aggregate's alignment. So this is not a note about where the type came from; it is
part of the layout. A packed struct that lost it answers `Layout()` with a
different size and a different alignment, and every member offset moves with them
-- which moves the stack slot, the by-value ABI classification and every field
access.

Set only by `cc/agg.go:61` from `__attribute__((packed))`; goc emits none.

**Test that fails without it:** `TestAggTypeRoundTripsEveryField`, which reports
`Packed: (bool) true` expected, `false` actual, and then checks the consequence
directly -- that `Layout()` returns the same size and alignment on both sides of
the round trip, which is the thing the flag is for.

**Emitted-code movement: none, on either path.** Both objects byte-identical to
their post-fix-2 state. Census: zero packed aggregates among the 63 aggregate
types goc's `hello.go` module reaches.

## Drop 4 — `Module.SymAttrs`, `Module.SymAlign`, `Module.Aliases`. `SymAttrs` is real in goc.

These were not fields the encoder forgot; there was no place in the format to put
them at all. What each one does, and why it is now in the format:

**`SymAttrs` — in, and it is the live one.** The front end declares it once
(`goc/compile.go:500`, `registerSymAttrs`; 84 symbols on `hello.go`) and four
optimizer passes read it: `LoadElim` and `DeadAlloc` through
`isAtomicPointerStore` (`opt/escape.go:1132`), the inliner through
`isFrameScopedRuntimeCall` (`opt/inline.go:817`), and the escape summaries through
`SymNoEscape` (`opt/escapesummary.go:170`, `opt/escapefacts.go:334`). Losing it
does not merely cost an optimization. An inliner that stops recognizing a
frame-scoped call is an inliner that may relocate a `defer` registration into a
different frame, and `SymNoEscape` is the only escape fact that exists anywhere
about the 619 bodiless `//go:noescape` declarations in `stdlib/src` -- without it
the summary has to assume the worst about all of them. This absence is exactly why
`memo.ModuleDigest` (`memo/memo.go:202-227`) had to hash `SymAttrs` from outside
the format: the memo was keying on a fact the format could not carry.

**`SymAlign` — in.** `arm64/select.go:331` folds a symbol's low bits into a
scaled load/store offset, and that fold is sound only when the symbol is aligned
to the access width. Losing the map is *conservative* -- the fold does not happen
and the code is merely worse -- but "quietly emits worse code" is what a cache
must not do, and the map has no source other than the front end.

**`Aliases` — in.** An alias is a symbol the backend defines (`addAliases`,
`arm64/mc.go:309`, `amd64/mc.go:196`). Losing one is not a degradation; it is a
symbol that is not there.

**`Module.AllocDecisions` and `Func.PlacedAllocs` — deliberately out.** Both are
documented diagnostic-only: no pass reads either, and a module with them empty
compiles to the same code. They exist so the two escape analyses can be compared
at the same allocation. Carrying them would cost every cached unit bytes no
compile looks at. The guard names them as explicit skips rather than passing over
them in silence, so the next reader learns it was a decision.

Both maps are written in sorted key order, so the encoding stays a function of the
module and not of Go's map iteration.

**Test that fails without it:** `TestModuleRoundTripsEveryField`, which reports
`[]*ir.Alias(nil)` where the fixture had one, and then the same for the two maps.

**Emitted-code movement on `plain` and `roundtrip`: none.** Both byte-identical
to their post-fix-3 state. That is expected rather than disappointing: those two
modes round-trip *after* `opt.OptimizeModule`, and `SymAttrs` is read by the
optimizer. The mode that shows it is the cache path -- decode, *then* optimize --
and measuring that turned up a further defect first. See drop 5.

## Drop 5 — `Func.nameSeq`, `Func.constIdx` and `Func.lowered`. Not on the brief; the first one stops the cache path compiling at all.

Measuring drop 4 needed a third harness mode -- **`roundtrip-pre`**: decode the
unit *before* `opt.OptimizeModule` runs, which is what a cache actually does. At
the branch base (`3f4a6b8`) that mode does not produce an object:

```
$ ./measure roundtrip-pre
measure: function runtime.sigblock: a64: duplicate label "b4"
```

Three separate causes, all in `Func`, none of them exported and so none of them
visible to the existing guard:

**`nameSeq` — carried now.** It is the counter behind generated block and
temporary names (`b7`, `t12`). A decoded function restarts it at zero, so the next
pass that asks for a block gets a name the function already uses, and the
assembler rejects the duplicate label. The names it belongs to are in the unit, so
the counter belongs in the unit.

**`constIdx` — rebuilt on decode, not carried.** It is the constant-interning
index and it is derived from `Consts`, so the format should not carry it -- but it
does have to be *rebuilt*, because `internConst` treats a nil index as an empty
one and appends rather than finding what is already there. A decoded function
grows a second copy of every constant a later pass asks for. Measured, cache path
with the rebuild removed:

| section | rebuilt | not rebuilt | moved |
|---|---:|---:|---:|
| `.text` | 1527964 | 1527708 | −256 |
| `.rela.text` | 905184 | 905592 | **+408** |
| `.data` | 3624240 | 3624056 | −184 |
| `.rela.data` | 265392 | 265344 | −48 |
| `.debug_info` | 1321198 | 1320695 | −503 |
| object | 9991688 | 9990560 | −1128 |

So this is not merely a bloated constant table: duplicated constants defeat the
value numbering that would have shared them, and the object that comes out is a
different program.

**`lowered` — carried now.** It gates what the rest of the tree believes about a
function: `VerifyModule` runs the SSA checks only on an unlowered one
(`ir/verify.go:52`) because lowering destructs SSA and those checks would reject
the form it produces; the interpreter refuses a lowered function outright
(`interp/interp.go:98`); and `MarkLowered` exists to stop a module lowered for one
target being lowered for another, which does not fail -- it emits a program built
around the first target's register assignment. A round trip that dropped the mark
disarmed all three. No movement measured: nothing in the module is lowered at the
point a cache would write it (census: 0), so this one is safe by luck too.

**Emitted-code movement for drop 5: from "does not compile" to byte-identical.**
With all of it in place, the cache path emits `fc378a0a…` -- the same object,
byte for byte, as the compile that never round-tripped.

## And now drop 4's number, which needed drop 5 to be measurable

With the cache path working, `SymAttrs` can be measured on its own by clearing it
after the decode -- exactly the pre-fix-4 behaviour, and nothing else changed:

| | sha256 | `.text` | object |
|---|---|---:|---:|
| cache path, `SymAttrs` carried | `fc378a0a…` | 1527964 | 9991688 |
| cache path, `SymAttrs` dropped | `25c4e7e0…` | 1527964 | 9991688 |

**Every section is the same size, and 755 bytes of the object differ — 691 of them
in `.text`.** That is the most dangerous shape a defect in this format can take: a
cache that dropped `SymAttrs` emitted a *different program* of *identical size*,
which no size check, and no check on the encoded bytes, would ever have noticed.
The differing bytes fall out as `.text` 691, `.rela.debug_info` 37, `.rela.text`
15, `.debug_line` 5, `.data` 2, `.debug_info` 2, `.symtab` 2, `.debug_loc` 1.
