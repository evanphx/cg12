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
