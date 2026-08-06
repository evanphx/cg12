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
# Caching by default for every goc compile

Branch `ccwork/default-compile-cache`, off `main` (`76069d9`). Host: aarch64
Linux, 64 cores, 250 GiB, go1.26.1, `cc` = gcc 13.3.0.

_(run in progress — sections appended as each result lands)_

## The situation, confirmed

Read against the tree at `76069d9`:

 1. `goc/source_world.go:29` caches parsed/type-checked stdlib packages in a
    process-level `map[sourceWorldKey]*sourceWorld`. A fresh `goc` process gets
    nothing from it.
 2. `cmd/goc/packcache.go` is the on-disk content-addressed cache under
    `~/.cache/cg12/runtime-pack`. Its only non-test caller is
    `buildRuntimeCommand` (`cmd/goc/prebuilt.go:47`) — `goc build-runtime`.
 3. `-runtime a.gocrt,b.gocrt` (`cmd/goc/main.go:46`) links against packs you
    already built; `runtimeSplit.chooseManifest` (`goc/runtime_split.go:149`)
    picks the richest usable one.

So the benefit is real and costs two manual steps. The shared cache on this box
already holds **985 packs / 45 GB**, all written in the last week, entirely from
opt-in `build-runtime` calls — which settles the eviction question before it is
asked.

## The numbers

Wall clock, aarch64/64 cores/250 GiB, median of three, private pack cache
(`CG12_PACK_CACHE` under the job's scratch, so the shared 45 GB cache was never
touched or trimmed by this run). "monolithic" is `GOC_AUTOPACK=0` — today's
default, the whole-program compile, with the in-memory source world still on.
"cold" is an empty pack cache: it includes building and caching the pack. "warm"
is a cache hit.

Three programs: `small.go` is `println("hi")`; `hello.go` imports fmt, os,
strings; `httpsrv.go` imports net/http, net/http/httptest, fmt, io, strings and
runs a request against its own test server.

### Default (no -O)

| program | monolithic | cold | warm | warm vs monolithic |
|---|---|---|---|---|
| small   | 3.05 s  | 2.90 s + 2.9 s pack | 2.62 s  | 1.16× |
| hello   | 6.45 s  | 10.35 s             | 4.90 s  | 1.32× |
| httpsrv | 31.5 s  | 49.7 s              | 22.2 s  | 1.42× |

### With -O

| program | monolithic | cold | warm | warm vs monolithic |
|---|---|---|---|---|
| small   | 7.39 s  | 9.54 s  | 2.65 s | **2.79×** |
| hello   | 16.22 s | 20.16 s | 5.24 s | **3.10×** |

The two tables say different things and the difference is the finding.

**A pack saves the back end and the optimiser, not the front end.** The split is
subtractive and it happens at the very end: both halves run the same parse, the
same type check, the same reachability walk and produce the same IR, and only
then does the program module drop what the pack already defines
(`goc/runtime_split.go:15`). So a warm compile still parses and type-checks the
whole closure. Without `-O` that front end is most of the compile, and the win is
16–42%. With `-O` the optimiser runs *after* the split, over a module the
subtraction has emptied out, and the win is 2.8–3.1×.

Cold is slower than monolithic, by the pack build. That is the deal: one compile
pays, every later compile of any program with that import list collects.

## The headline: warm goc against warm gc

Every compile-time comparison in this project so far has been goc against
itself. This is goc against the host toolchain, both fully warm, on identical
source. One line is appended to the file before each compile, alternating
between the two, so neither is answering from a whole-program result cache: gc
has its stdlib package archives and goc has its pack, and both have to compile
the program.

| program | goc warm | gc warm | ratio |
|---|---|---|---|
| small (`println`)      | 2.62 s  | 0.16 s | **16×** |
| hello (fmt/os/strings) | 4.90 s  | 0.24 s | **20×** |
| httpsrv (net/http)     | 22.1 s  | 0.55 s | **40×** |

With the source *unchanged*, so that gc's action cache answers the whole build
and it does no compiling at all, gc is 0.05 / 0.07 / 0.10 s — 52× / 70× / 222×.
That is the literal "already in each one's cache" reading of the question; the
table above is the one that describes an edit-compile loop, and it is the fairer
number of the two.

The ratio is what it is because the two caches cache different things. gc caches
*per package*: a warm gc compiles one package, the program's own, and links
against archives. goc's pack caches the runtime module's **object**, but the
program compile still runs the whole-program front end over the entire closure
and only subtracts at the end. So goc's warm compile is still parsing and
type-checking ~150 packages that gc does not look at.

That is the shape of the remaining gap, and it is not something a cache of packs
can close. Closing it means caching the front end — the parsed and type-checked
`sourceWorld` that `goc/source_world.go` already builds and then throws away when
the process exits. That is a different piece of work and this measurement is the
argument for it.

## The decisions asked for

**Pack selection — build the pack the program needs, don't fall back to a
narrower one.** The pack carries exactly the standard library packages the file's
own import declarations name. When the program imports something no cached pack
carries, a wider pack is built and cached.

The alternative — a fixed ladder of package sets, compile the remainder normally
— cannot work, because usability runs the wrong way. A pack is usable only by a
program whose closure *contains* the pack's, so a ladder can only be rounded
*down*: a net/http program that finds no net/http tier falls back to the
runtime-only tier and recompiles net/http from source. And net/http programs are
the entire prize. This tree's own matrix measured it: eleven of 368 capability
programs cost 125–167 s each and are 54% of all compile CPU; the other 327
average 4.2 s. A rounded-down ladder helps precisely the 327 that did not need
help.

Near-duplicates are the cost of that, and they are handled by an equality rather
than a heuristic. If a cached pack C satisfies `C.Packages ⊆ P ⊆ C.Closure` for
the requested list P, then C's closure and the closure of the pack that would be
built from P are *the same set* — monotonicity gives one containment, idempotence
the other (the proof is in `substitutePack`). So a program importing
{fmt, io, net/http, strings} takes the existing {net/http} pack instead of
building a second 98 MB copy. Verified end to end: the second compile logs
`substituted 1a07d580925a (carries [net/http])` and the cache stays at two packs.

It is order-dependent and deliberately not more than that: {fmt, net/http}
compiled first leaves a pack a later {net/http} program cannot match. Fixing that
means knowing every package's closure before compiling anything — a second import
resolver to write, keep correct against build tags, and be wrong in. The
duplicate is cheaper than the resolver, and eviction bounds it.

**Concurrency — a per-key advisory file lock, layered over a sequence that is
already safe without it.** `writeFileAtomically` (temp file + rename, already in
the tree) is what guarantees a half-written pack is never readable and that two
racing writers both end with a whole file; that property does not depend on the
lock. The lock exists so that a suite starting dozens of compiles at once pays
for one 98 MB pack build rather than dozens. It is `flock` on `<key>.lock`, held
across the build, with the cache re-checked after acquiring; plus an in-process
mutex per key for `compile-batch`, which compiles many programs in one process.

Every way of failing to take the lock ends in building anyway, never in failing:
no flock on the platform, an unwritable directory, or a wait that gives up after
15 minutes because a peer is wedged rather than merely slow. The kernel drops a
flock when the holder dies however it dies, so a killed builder cannot wedge the
cache.

Measured: four concurrent cold compiles of one program, `1 of 4 concurrent
compiles reached the build step`, four byte-identical images, one whole pack in
the cache.

**Cache growth — bounded now, not a follow-up.** The question answers itself: the
shared cache on this box already held **985 packs and 45 GB**, written in a week,
from opt-in `build-runtime` calls alone. Making the cache the default multiplies
that. So `trimPackCache` evicts least-recently-used packs to a 20 GiB budget
(`CG12_PACK_CACHE_MAX_BYTES` overrides; 0 is unbounded), sweeping after each
cached write. Least recently *used*, not oldest: a hit touches the pack's mtime,
so a pack every build links against never becomes the oldest thing in the cache.
Unlinking a pack a reader has open is safe on POSIX, and a reader that has not
opened it yet gets a miss, not a fault.

Two consequences worth stating plainly. The first run of a `goc` carrying this
change will trim the existing shared cache from 45 GB down to 20 GiB — that is
the feature working, but it is a deletion, and the packs it deletes are the
least recently used ones. The second is that this change alters the key (see
below), so those 45 GB are already garbage under the new key and eviction is the
only thing that will ever reclaim them.

**`CG12_NOCACHE=1` bypasses everything.** It is checked in `autoPackEnabled`
before anything else, so a compile under it never computes a key, never reads the
cache, never builds a pack, and takes the whole-program path. Verified: `goc:
autopack: disabled`, zero files written to the cache directory, and the image is
byte-identical to the `GOC_AUTOPACK=0` monolithic build. `GOC_AUTOPACK=0` is a
separate switch that turns off only this, leaving the in-memory source world
alone — which is what the monolithic column of the tables above was measured
with, since `CG12_NOCACHE=1` would have disabled that too and measured something
else.

## Auditing the key, which now decides every compile

The key already covered the pack version, target, `-O`, the package list, the
placement policy, the optimisation pipeline, the goc binary's bytes and the
vendored stdlib tree's bytes. Auditing it against what a default-on cache now
depends on found three gaps, two of them real.

**Gap 1 — the environment beyond the two variables that were named.** The key
named `GOC_OPT_PIPELINE`/`GOC_OPT_SKIP` (via `opt.PipelineIdentity`) and the text
layout (via `arm64.TextLayoutIdentity`). It named nothing else. A sweep of every
`os.Getenv` in the non-test tree found these, all of which change what goc emits
and none of which changed the key:

| variable | what it changes |
|---|---|
| `GOC_ESCAPE_SUMMARIES=0` | turns off escape summaries — allocation placement |
| `GOC_PAYLOAD_FOLD=0` | turns off payload folding — allocation placement |
| `CG12_NO_IFCONVERT` | turns off if-conversion |
| `CG12_FORCE_INLINE`, `CG12_NO_COSTINLINE`, `CG12_NO_AGGINLINE` | the inliner |
| `GOC_NO_NOSPLIT_INLINE`, `GOC_NOSPLIT_LIMIT`, `GOC_NOSPLIT_INDIRECT` | the nosplit frame budget, which decides nosplit inlining |
| `GOC_STDLIB_OVERLAY` | **which files a standard library package is built from** |

The last is the sharpest: the key hashes the *contents* of the vendored tree, and
`GOC_STDLIB_OVERLAY=off` changes nothing in the tree — only which of its files
are selected. A hashed tree cannot see it.

The fix is deliberately blunt: **every `GOC_` and `CG12_` variable, name and
value, sorted, goes into the key**, except the four that name where the cache is
rather than what is in it. The alternative is a maintained list of the variables
that matter, and a switch added to the optimiser and not added to that list is a
silent miscompile. A sweep of the namespace cannot miss one. What it costs is a
miss when a diagnostic-only variable is set — and a miss is a slow build, which
is the direction this cache is allowed to be wrong in.

It also closes a subtlety: `arm64.TextLayoutIdentity()` reads its environment at
package init (`var layout = layoutFromEnvironment()`), so a process that changes
`GOC_FUNC_ALIGN` after start gets a stale identity. The sweep covers those
variables directly and does not care when they were read.

**Gap 2 — the C toolchain, which the old comment named as its own weakest link.**
It was the `cc --version` banner: a release, not a binary. That was tolerable
while a pack came only from an explicit command. It is not tolerable now. The
identity is three things:

  - the banner, cheap, names the release;
  - the **bytes of the `cc` driver and of the `as` it reports it will exec**
    (`cc -print-prog-name=as`), exact for those two binaries;
  - the **bytes of the object `cc` actually produces for a fixed probe** — the
    only one that measures behaviour rather than provenance, and so the only one
    that catches a changed shared library under an unchanged `as`.

The probe is a frame, an `adrp`/`:lo12:` pair, a `bl` to an undefined symbol, an
FP instruction and an aligned datum — the constructs the Plan 9 sidecar is made
of. What is still outside the key is the assembler's behaviour on instructions
the probe does not contain. That is a sample, not a proof, and it is stated as
one; it is strictly stronger than a version string. Cost: 26 ms, once per
process. The probe object was checked byte-identical across directories before
being made part of the key.

**Gap 3 (not real) — the `-m` escape diagnostics.** These are printed by a pass
that runs *after* the split, over a module the subtraction has taken definitions
out of, so a pack would change what `-m` prints. Rather than key on it, `-m`
declines auto-packing: a compile asked for its escape decisions gets the
whole-program compile whose decisions those are.

### Verified by construction

`TestEveryChangeThatWouldMakeAPackWrongProducesAMiss` drives the real compiler,
warms the cache, checks it is warm, then changes one thing per arm and requires a
miss. All pass:

| changed | result |
|---|---|
| the compiler binary (rebuilt `-ldflags=-s -w`: different bytes, identical behaviour) | miss |
| `-O` | miss |
| the placement policy (`GOC_FUNC_ALIGN=64`) | miss |
| the optimisation pipeline (`GOC_OPT_PIPELINE=bounded`) | miss |
| the standard library tree (a file added, then a byte changed) | miss |

and `TestTheKeyMovesWhenTheEnvironmentChangesWhatTheCompilerEmits` does the same
for fourteen environment switches including `GOC_STDLIB_OVERLAY` and a variable
that does not exist yet, while requiring that `CG12_PACK_CACHE`,
`CG12_PACK_CACHE_MAX_BYTES` and `GOC_AUTOPACK_DEBUG` do *not* move it.

### One thing the key cannot save you from, stated plainly

Every existing pack in the shared cache is unreachable under the new key. That is
correct — the old key did not cover the environment or the assembler, so an old
pack is a pack whose provenance is not fully known — but it means the first run
after this change is cold for everything, and the 45 GB already there is garbage
that only eviction reclaims.

## Guards

| guard | result |
|---|---|
| capability matrix, default arm | **368/368**, 0 fail, 0 skip |
| capability matrix, `-O` arm | **368/368**, 0 fail, 0 skip |
| capability matrix **through the new auto-pack path** | **368/368**, 0 fail |
| `TestAllocationCensus`, `TestFrameEscapeAudit`, `TestLoopAliasAudit` | pass (208 s) |
| determinism: cold vs warm, byte-identical | yes, six ways — see below |

The third row is the guard that matters most and it did not exist before. The
matrix normally builds six packs by hand and passes them with `-runtime`, which
exercises the *explicit* pack path. Run with `-runtime-status-prebuilt-runtime=false`
the harness passes no `-runtime` at all, so every one of the 368 programs goes
through `autoPackFor` — including through `goc compile-batch` workers, which is
the batch arm of the new code. All 368 pass.

That run is also the honest answer to the cache-growth question, measured rather
than guessed: **368 capability programs produced 138 packs and 2.4 GB**, well
inside the 20 GiB budget. And the wall clock is the point of the whole change:

| the matrix, 368 programs | wall clock |
|---|---|
| hand-built packs passed with `-runtime` (today) | 71 s |
| auto-packed, cold cache (builds all 138 packs) | 108 s |
| auto-packed, warm cache | **67 s** |

Automatic per-program packs on a warm cache beat the tree's hand-chosen six pack
roots, with nobody having chosen anything.

### Determinism, including its new failure mode

Determinism used to mean "two runs of the same compile agree". A cached pack adds
a second meaning: a compile that read a pack written by another process, possibly
in another week, has to produce the same bytes as the compile that built it.
Checked explicitly by compiling cold, compiling warm, and comparing:

| program | default | `-O` |
|---|---|---|
| small   | identical | identical |
| hello   | identical | identical |
| httpsrv | identical | identical |

plus warm-vs-warm identical, and `TestACachedCompileIsTheSameImageAsAColdOne` in
the committed suite, which does the same for a fmt/sort/strings program and then
runs it.

`CG12_NOCACHE=1` produces an image byte-identical to `GOC_AUTOPACK=0`, which is
the whole-program compile — so the gates' cold arm is the same compile it always
was.

### Eviction, end to end

A cache holding two packs (139 MB) with a 150 MB budget, asked to build a third:
the least recently used pack was evicted, its index entry with it, the cache came
back to 83 MB, and the program built and ran. The sweep runs after a cached
write, so a cache that is over budget and gains nothing new is left alone —
eviction is a consequence of adding, not a background process.

## One consequence to be explicit about

**The default output binary changes.** A plain `goc file.go` used to produce a
monolithic image and now produces a split one: the prebuilt runtime object, its
sidecar, and the program module, linked in that order. Same program, different
layout and different addresses.

This is not a new configuration — it is the one the capability matrix has been
running in both arms, and it is now 368/368 through the automatic path as well —
but it is newly the *default*. Anything cut against the monolithic image
describes something goc no longer emits by default. The two runtime benchmarks
(`make bench-crypto`, `make bench-perf`) are unaffected: both are library tests
in `./goc` that call the compiler in-process and never run `cmd/goc`. Their
baselines were not touched, nor were any timing baselines re-cut.

`GOC_AUTOPACK=0` gets the monolithic image back.

## What it cost, and what is left

The key is now computed on every compile rather than once per `build-runtime`.
That is **206 ms**: 30 ms to walk the vendored tree, 131 ms to hash its 5423
files and 73 MB (concurrently — it was 256 ms single-threaded, and that mattered
much less when only an explicit command paid it), 3 ms for the goc binary, 26 ms
for the C toolchain and its probe. It is 7.9% of a warm trivial compile and 0.9%
of a warm net/http one.

It is deliberately not memoised behind cheaper metadata. Recording the expensive
digest against the tree's sizes and mtimes would take it to near zero, and would
reintroduce exactly the hazard the tree-hash exists to avoid: a file whose
content changed without its mtime changing is then a stale hit, and a stale hit
is a wrong image. 206 ms of honest hashing is the right side of that trade.

What is left is the front end. A warm goc still parses and type-checks the whole
closure — the pack saves the back end and, under `-O`, the optimiser. That is why
the win is 1.16–1.42× by default and 2.8–3.1× under `-O`, and why warm goc is
still 16–40× warm gc. The next thing worth caching is the `sourceWorld` that
`goc/source_world.go` builds and discards at process exit.

## Measurement conditions, and why the ratios are the number to quote

This is a shared build host and it was not idle: another ccwork job was
compiling throughout, and the shared pack cache grew from 985 to 999 entries
while this job ran. Load average over the measurement window ranged from the
high teens to 72.

The absolute times therefore move — a warm net/http compile is 22.1 s at low
load and 29.9 s at load 65 — but every comparison here was taken by
**alternating the two arms on the same file**, so both see the same machine.
Three independent sets, taken hours apart at different loads:

| | set 1 (light) | set 2 | set 3 (load 52–66) |
|---|---|---|---|
| small, goc ÷ gc      | 2.62 / 0.16 = 16× | 3.28 / 0.20 = 16× | 3.28 / 0.19 = **17×** |
| hello, goc ÷ gc      | 4.90 / 0.24 = 20× | 6.25 / 0.28 = 22× | 5.95 / 0.27 = **22×** |
| httpsrv, goc ÷ gc    | 22.1 / 0.55 = 40× | 29.0 / 0.61 = 47× | 29.9 / 0.66 = **45×** |

Ratios stable within ~12% across a 4× swing in load; absolutes vary by up to
35%. The same holds for the speedup this change buys — re-measured at load 72,
`hello` is 8.2 s monolithic → 6.3 s warm (1.30×, was 1.32×) and 20.6 s → 6.5 s
under `-O` (3.17×, was 3.10×).

So: **warm goc is 16–22× warm gc on small programs and 40–47× on a net/http
program**, and the absolute figures in the tables above are the low-load set.

## Also run

`make test-goc-cmd` — the whole goc driver end-to-end suite, with auto-packing on
by default, since this change touches the driver more than anything else. One
failure, `TestCheckedRuntimeCoverageBaselineDenominator`, which **pre-exists on
`main` (76069d9)**: verified by running that test in a clean worktree at
76069d9, where it fails the same way. It is a bookkeeping check that every
capability appears in the coverage baseline or the pending list, and it names
whichever unlisted capability map iteration reaches first. Nothing else in the
suite fails.

Not run, as instructed: `go test ./goc/...` and `make test-unit` — a gate job
does those. The three named guard tests were run individually with `-run`. No
timing baseline was re-cut.

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

# Integration gate: `integration/cache-wave`

Branch `integration/cache-wave`, cut from `main` (`76069d9`) and merging, in
order, `ccwork/ir-verify-entry-blocks`, `ccwork/default-compile-cache` and
`ccwork/build-cache-design-2`. Host: aarch64 Linux, 64 cores, 250 GiB RAM,
go1.26.1, box exclusive to this run.

## The merges

Three merge commits, `--no-ff`, in the order given. The only conflict in any of
the three was `CCWORK_REPORT.md`, where all three branches append their own
section at the end of a file all three cut from the same commit. Resolved by
keeping every side in merge order; no branch's prose was dropped or reworded.
No source file conflicted. `go build ./...` is clean on the merged tree and
`gofmt -l` reports nothing on any file the merge touched.

## The defect: `nosplitdebt`'s `whole` arm was not whole

`analysis/nosplitdebt/main.go` sweeps three module shapes and folds their
measured nosplit chain heights into `arm64.noSplitDebt`, the register that
decides which over-limit chains are tolerated. The `whole` arm's doc comment
states its whole purpose: "the runtime, the standard library and the program are
one module and every frame of every chain is visible to the walk at once". It
ran `goc -o /dev/null program`.

With `ccwork/default-compile-cache` merged, that command no longer means what it
meant. A plain `goc file.go` now finds or builds a pack for the program's import
list and links against it, which is the `split` arm's module boundary arriving
without being asked for. The `pack` arm three functions above already guarded
itself with `CG12_NOCACHE=1`; the `whole` arm guarded nothing.

**Measured on the merged tree**, `goc/testdata/stdlib_os_exec_echo.go`, heights
dumped with `GOC_DEBUG_NOSPLIT=heights`:

| how it was compiled | over-reserve chains reported |
|---|---|
| whole-program (`GOC_AUTOPACK=0 CG12_NOCACHE=1`) | **51** |
| plain `goc`, cold pack cache | 50 |
| plain `goc`, warm pack cache | **0** |

The warm row is the one that matters, and it is worse than "measures less". On a
warm cache the program module alone has no chain over the reserve, so the arm
reports *nothing at all* — and which row you get depends on whether some earlier
compile in the same sweep happened to leave that import list's pack behind. The
51 chains lost include `syscall_runtime_AfterForkInChild` at 976, which is the
exact chain this recipe was widened to find and the reason the recipe exists in
its present form.

Because the register is a floor — a chain it does not name may not exceed the
reserve at all — a regeneration from the split view would have *deleted* entries
and turned them into build failures for the programs that contain them.

**The fix** (`37d4225`): the `whole` arm now runs with `GOC_AUTOPACK=0` and
`CG12_NOCACHE=1`. Both are named on purpose. `GOC_AUTOPACK=0` is the switch
whose meaning is "do not choose a pack" and is the one that makes the module
whole; `CG12_NOCACHE=1` is what the `pack` arm sets, for the same reason it sets
it — nothing in this measurement may be served out of a cache some other
compiler filled. The `split` arm needs no change: it passes `-runtime`
explicitly, and `cmd/goc/main.go` returns on that path before auto-packing is
considered.

## The register, regenerated from the merged tree — **it did not move**

`scripts/nosplit-debt-regen.sh -j 32 -update` on `integration/cache-wave` with
the fix in place. **1638 configurations** — `pack=14 whole=812 split=812`, the
whole 406-program corpus in both module shapes at both optimization levels —
in 18 minutes:

    outcomes: 1638 measured, 0 rejected by the budget, 0 failed to compile
    register: committed=51 original-recipe=50 widened-recipe=51
    against the committed register:
      no change: 51 entries, same names, same heights

`git status` after `-update` is clean: the rewritten file is byte-identical to
the committed one. **The register does not move.** Same 51 names, same 51
heights.

Two things that answers, and one it does not.

**The committed register was generated correctly, and it still reproduces.**
The one entry the original 22-configuration recipe could not reach —
`syscall_runtime_AfterForkInChild` at 976 — is found again, and by the same
configuration: `whole stdlib_os_exec_echo.go`. That is the fixed arm working
end to end, since it is precisely the entry the unfixed arm loses.

**`ccwork/ir-verify-entry-blocks` does not deepen any recorded chain.** This was
the open question. Branch 1 re-enables `opt.InlineIntoNoSplitCallers` for every
closure shape — 86 of the 465 functions the budget tries to measure on a
hello-world were previously unmeasurable — and the branch's own report notes
that a function missing from the facts stops `stackcheck`'s walk before its
subtree is counted, which *overstates* headroom. So heights could have gone up
and entries could have been added. Across all 1638 configurations, none did, and
none of the 1638 was rejected by the budget.

**What it does not answer** is what a naive regeneration would have done, and
that is worth stating because it nearly happened. The register is a floor: an
entry that disappears becomes a hard build failure for the program that contains
that chain. The unfixed arm on `stdlib_os_exec_echo.go` reports 0 chains on a
warm cache and 50 on a cold one against 51 whole-program, and the one it drops
in the cold case is `syscall_runtime_AfterForkInChild` — the entry no other arm
reaches. So a regeneration on the unfixed recipe would have deleted at least
that entry from the register and broken `goc/testdata/stdlib_os_exec_echo.go`,
which is the same program that motivated widening the recipe in the first place.
I did not run the full unfixed sweep to enumerate the rest; the per-program
measurement above is direct and the register-level consequence follows from the
`source` attribution the sweep prints.

## Other places that assume monolithic output

Three more harnesses reach the `goc` **binary** with neither `-runtime` nor a
cache switch, so all three now measure a split build where they used to measure
a monolithic one. None of them is wrong the way `nosplitdebt` was wrong — none
claims in its own words to be whole-program — but all three changed what they
measure without anyone editing them:

  - `goc/slogalloc_test.go:204` builds `cmd/goc` and runs `driver -o binary
    program`. It generates `slog_allocations_baseline.txt`, one of the files
    this gate regenerates. Split vs monolithic is a different module boundary
    for escape analysis, so this one can move the numbers.
  - `goc/cryptobench_test.go:484` and `goc/perfbench_test.go:581` do the same
    with `-O`, for `crypto_signing_bench_baseline.txt` and
    `perf_suite_baseline.txt` — the two baselines this gate is told not to
    re-cut. They now time a program built the new default way.
  - `scripts/determinism-check.sh` is the sharpest case. Its five-program arm
    compares a `CG12_NOCACHE=1` build against a plain one and calls a difference
    a determinism failure. `CG12_NOCACHE=1` is now "compile whole-program" and
    the plain build is now "compile split", so the script compares two different
    compiles and reports the difference as non-determinism. Measured below.

Everything else that generates a baseline in `goc/` — the allocation census, the
frame-escape, loop-alias and escape-shadow audits, and both GC differentials —
compiles in-process through the `goc` package. Auto-packing lives in `cmd/goc`,
so those are untouched by it.

## Every generated file, regenerated from the merged tree — **none moved**

Each file was rewritten from the merged tree by its own `-update` flag, never by
picking a side of a merge. All seven wrote (mtimes confirm it, and each test
prints "baseline rewritten"), and `git status` is clean afterwards: every one is
byte-identical to what `main` carries.

| file | regenerated by | moved? |
|---|---|---|
| `alloc_census_baseline.txt` | `TestAllocationCensus -update-alloc-census-baseline` | no |
| `frame_escape_baseline.txt` | `TestFrameEscapeAudit -update-frame-escape-baseline` | no |
| `loop_alias_baseline.txt` | `TestLoopAliasAudit -update-loop-alias-baseline` | no |
| `escape_shadow_baseline.txt` | `TestEscapeShadowPlacement -update-escape-shadow-baseline` | no |
| `escape_gc_differential.txt` | `TestEscapeDifferentialAgainstGC -update-escape-gc-differential` | no |
| `escape_gc_reason_differential.txt` | `TestEscapeReasonDifferentialAgainstGC -update-escape-gc-reason-differential` | no |
| `slog_allocations_baseline.txt` | `TestSlogAllocationsAgainstGC -update-slog-allocations` | no |
| `arm64/nosplit_debt.go` | `scripts/nosplit-debt-regen.sh -update` | no |

All eight regenerations passed; four ran concurrently in 12–196 s each.

**Why nothing moved, and what that is and is not evidence of.** Branch 1 changes
emitted code on all 406 corpus programs, so movement was the expectation. Six of
these eight files are produced by a corpus pass that **compiles to IR and
stops** (`goc/corpusaudit_test.go`), and `opt.InlineIntoNoSplitCallers` — the
one optimisation branch 1 re-enables — is driven from the arm64 back end, after
the point those audits look. So their invariance says the branch does not
perturb escape analysis, aliasing or allocation placement. It is **not**
evidence that emitted code is unchanged; branch 1's own report measured 406/406
executables differing and `hello.go` `.text` growing 8,064 bytes, and that
remains true.

Two of the eight do see the back end, and are the load-bearing ones here:

  - `slog_allocations_baseline.txt` is measured by *running* a binary the `goc`
    driver built — which, post-merge, is a **split** build rather than a
    monolithic one (see the section above). Same allocation counts either way.
  - `arm64/nosplit_debt.go` is measured from the back end's own frame layout
    across 1638 configurations. Same 51 entries at the same heights.

`perf_suite_baseline.txt` and `crypto_signing_bench_baseline.txt` were not
re-cut; see the timing sections below for whether a measurement forces it.

## Package suites

`make test-unit`'s package set — every package outside `goc`, `cmd/goc`,
`difftest` and `cc` — **all green**, 26 packages, `ir` and `opt` and `arm64` and
`stackcheck` among them.

`TestParallelBackendIsByteIdenticalToSerial` **PASS**, all four worker counts
(3, 8, 64, 256).

`go test ./cmd/goc/...` minus the capability matrix: **434.99 s, one failure**,
and it is the known one —

    --- FAIL: TestCheckedRuntimeCoverageBaselineDenominator (0.03s)
      capability "gc-invariants/promoted-local-root" is in neither the accepted
      baseline nor testdata/runtime_coverage_baseline_pending.json

`main` fails the same assertion at the same line. The capability it names varies
between runs on `main` alone — three runs gave `slice-tail-pointer`,
`promoted-local-root`, `slice-tail-pointer` — so the branch naming a different
one than a given `main` run is map iteration order, not a different failure.
Pre-existing, unrelated to all three branches.

## 6. The nosplit budget under branch 1's re-enabled inlining

**It still rejects a constructed overflow.** `goc/nosplitbudget_test.go`, all
five, on the merged tree:

    TestNoSplitBudgetRejectsAnOverflowingChain                PASS  7.38s
    TestNoSplitBudgetAcceptsAChainThatFits                    PASS  7.05s
    TestNoSplitBudgetProducesNoObject                         PASS  6.80s
    TestNoSplitBudgetAcceptsACorpusProgram                    PASS  7.01s
    TestNoSplitBudgetErrorNamesTheAllocatorChainWhenItIsForced PASS  6.62s

**How much more inlining there now is.** `GOC_DEBUG_NOSPLIT=inline`, `goc -O`
on `goc/testdata/hello.go`, both compilers driven `GOC_AUTOPACK=0
CG12_NOCACHE=1` so both are the whole-program compile and the comparison is
like for like:

| | accepted | rejected after measuring | no headroom at all |
|---|---:|---:|---:|
| `main` | 99 | 4 | 203 |
| `integration/cache-wave` | **160** | 5 | 124 |

**99 → 160 nosplit callers receive inlining, +62%.** The "no headroom" column
falls 203 → 124 in step: a caller whose callees could not be measured had no
measured allowance to spend, and 86 of the 465 measurement attempts on this
program were the ones failing.

`.text` on the two executables is 1,541,284 → 1,549,348, **+8,064 bytes**, which
is exactly the figure branch 1 reported. Independently reproduced here from two
separately built compilers.

And the 1638-configuration sweep above is the wider statement: with all of that
extra inlining, **no configuration was rejected by the budget and no recorded
chain grew by one byte**.

## 2. Determinism

### `scripts/determinism-check.sh` was comparing two different compiles

The five-program arm on the merged tree, before any change:

    hello.go        round1:DIFFERENT(7d3a5c5c…/23533968…)  round2:DIFFERENT(7d3a5c5c…/23533968…)
    fmt_sprintf.go  round1:DIFFERENT(e0e4903e…/ec82d502…)  round2:DIFFERENT(e0e4903e…/ec82d502…)
    … 10 of 10 DIFFERENT

**This is not non-determinism**, and the hashes say so: each column reproduces
exactly across both rounds. Isolated to the module shape with four compiles of
`hello.go`:

| compile | image |
|---|---|
| plain `goc`, twice | `23533968…`, `23533968…` |
| `GOC_AUTOPACK=0` | `7d3a5c5c…` |
| `CG12_NOCACHE=1` | `7d3a5c5c…` |

`CG12_NOCACHE=1` used to mean "do not read the on-disk cache" and now also means
"do not choose a pack", so the script's cold arm takes the whole-program path
while its warm arm takes the split one. Two different compiles, both perfectly
deterministic, reported as a determinism failure. On `main` the same two
compiles are `988ae906…` and `988ae906…` — identical, because there was no
second path to take.

Fixed (`scripts/determinism-check.sh`): cold is now an empty private
`CG12_PACK_CACHE` and warm is a hit in the same one, a fresh cache per round.
The whole-program image is deliberately not compared against the split one —
they are different modules and always will be; what matters is that each is
reproducible.

After the fix, and this is also the answer to "cold vs warm byte-identity":

| | round 1 | round 2 |
|---|---|---|
| default, 5 programs | **identical 5/5** | **identical 5/5** |
| `-O`, 5 programs | **identical 5/5** | **identical 5/5** |

20 of 20 identical, and the cold image equals the warm one byte for byte, so a
pack written by one process and read by another produces the same executable.

### The corpus arm — 406 programs, 4 rounds, both optimization levels

Through `goc compile-batch` with no `-runtime`, which post-merge means each
worker picks each program's pack out of the cache — the new default path, and
the one worth measuring.

| | rounds | result |
|---|---|---|
| default | 4 × 406 | **reproducible=406 varying=0 failed=0** |
| `-O` | 4 × 406 | **reproducible=406 varying=0 failed=0** |

No program produced two different images, and none produced two images with the
same content digest and different layout either. Round 0 pays for the cold cache
(182.7 s default, 307.7 s at `-O`) and rounds 1–3 are 99–103 s each.

## 1. The audits

`go test ./goc/...` in full: **PASS, 1142.3 s**, zero failures. That is the whole
corpus package on the merged tree, with every regenerated baseline in place.

The five named audits, run explicitly in check mode:

    --- PASS: TestAllocationCensus      (207.63s)   baseline reproduced, no diff
    --- PASS: TestEscapeShadowPlacement
    --- PASS: TestFrameEscapeAudit
    --- PASS: TestIRVerifyAudit
    --- PASS: TestLoopAliasAudit

`TestIRVerifyAudit` is branch 1's, and it passes on the merged tree.

**The allocation census ran three separate times**, in three separate processes,
each doing a full corpus compile and comparing against
`alloc_census_baseline.txt`: the `-update` regeneration (196.3 s), the full
`./goc/...` package run, and the explicit check above (207.6 s). No diff in any
of them. (A `-count=2` repeat inside one process is not a second census — the
corpus compile is memoized, and the second iteration takes 0.06 s. Three
processes is the honest reading of "twice".)

## 3. Crash loops and GC reducers — **1310 runs, 0 failures**

Eight at a time. Both `placement_bench` programs `panic` on a wrong answer, so
these are correctness loops as well as crash loops. Everything compiled `goc -O`
by each compiler's own default — the branch's default is now the split build,
which is the point.

| loop | compiler | runs | `GOGC` | failures |
|---|---|---:|---|---:|
| `placement_bench/flate` | branch | 250 | 100 | **0** |
| `placement_bench/flate` | branch | 250 | 10 | **0** |
| `placement_bench/p256` | branch | 100 | 10 | **0** |
| `runtime_lock_osthread` | branch | 400 | 100 | **0** |
| GC reducer `runtime_gc_promoted_local_root` | branch | 20 | 100 | **0** |
| GC reducer `runtime_gc_promoted_local_root` | branch | 20 | 10 | **0** |
| GC reducer `runtime_gc_type_mask_padding` | branch | 20 | 100 | **0** |
| GC reducer `runtime_gc_type_mask_padding` | branch | 20 | 10 | **0** |
| GC reducer `runtime_gc_promoted_local_root` | **`main` control** | 20 | 100 | **0** |
| GC reducer `runtime_gc_promoted_local_root` | **`main` control** | 20 | 10 | **0** |
| GC reducer `runtime_gc_type_mask_padding` | **`main` control** | 20 | 100 | **0** |
| GC reducer `runtime_gc_type_mask_padding` | **`main` control** | 20 | 10 | **0** |

Branch and control agree at zero, so the reducers say nothing has moved rather
than that the instrument is asleep — but note that a reducer at 0/20 on both
sides is a weak instrument by construction; it is the flate and p256 loops, 600
runs of real decompression and real signature verification, that carry the
weight here.
