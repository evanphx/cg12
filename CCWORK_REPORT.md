# Batch compilation: one goc process, many programs

Branch: `ccwork/goc-batch-b`, off `perf/test-suite` (`0f4ee02`). The previous job's report has
been moved to `docs/report-matrix-speed.md`, following the precedent that job set, so this file
is only about this job.

Status: **complete.** Everything stated below was measured on this box, while at least one
sibling job was compiling on it — which is a real limit on the timing numbers and is stated
wherever it bites. Anything not measured is named as such, in §9.

## Headline

`goc compile-batch` compiles many programs in one process, sharing the parsed and type-checked
runtime, and the matrix harness now dispatches through a pool of these. It is wired in by
default and `-runtime-status-batch-compile=false` turns it off.

**It is safe.** Every one of the 358 corpus programs was compiled three ways — one process per
program, a batch, and a batch fed the programs in reverse — and compared byte for byte and by
behaviour. **0 leaks; all 358 behave identically.** The 39 programs whose bytes differ are all
explained by pre-existing nondeterminism in the compiler, each one demonstrated individually
(§3.3). Fifteen full matrix runs, all 338/338.

**It is smaller than the briefing estimated, and larger than my own first estimate.** The
briefing sized it at ~700 s of the ~3030 s of compile CPU (23%) on the reasoning that most of
`hello.go`'s 2.11 s is the runtime's source closure. Measured, the *world* is only 0.53 s of
wall and 0.73 s of CPU per compile — but the full amortizable per-process cost, including
process start and a fresh 300 MB heap collected from scratch in every one of 338 processes, is
**1.10 s of CPU per compile**. Three independent measurements put the saving at:

| method | saving |
| --- | ---: |
| isolated per-compile bench, extrapolated | ~354 CPU-s |
| whole-corpus sweep, summed per-program compile wall | 196.8 s (5.8%) |
| matrix A/B, 16 workers, both orders | 502 CPU-s (11.2%) |

**Somewhere between about 5% and 12% of what the matrix costs to run, and this box could not
narrow it further** (§5.3).

**It does not move the floor**, and was never going to: the matrix is bounded by
`max(slowest compile, compile CPU / workers)`, and the first term is one 157.6 s single-threaded
compile. This lever moves the second term. That is why the wall clock did move here — on a
shared box the second term binds — and why on an exclusive box at 24+ workers it would move much
less. Its real value is that the suite costs a tenth less to run *anywhere*, and is less
sensitive to how much of the machine it gets.

Two things found on the way that are not this lever and matter more than it does:

- **Compile nondeterminism is at least 39 of 358 corpus programs (10.9%), not the one the
  five-program determinism check knows about** (§4). Pre-existing, verified at the branch point.
  It means byte-identical output cannot be this branch's merge gate.
- **The largest remaining cost in a small compile is the runtime IR that gets generated and then
  subtracted** — 1.5 s of `hello.go`'s 2.1 s (§10).

## 1. How big the per-process fixed cost actually is

The world is built by the first compile in a process and reused by every later one, so the
amortizable cost is exactly `first compile − later compile` in one process. Measured with a
scratch main that reads a prebuilt pack from disk and compiles the same program N times through
`prebuilt.CompileProgram`, the same call `goc -runtime` makes:

    pack read: 0.021s
    goc/testdata/hello.go  iter=0  2.055s   <- includes building the shared source world
    goc/testdata/hello.go  iter=1  1.496s
    goc/testdata/hello.go  iter=2  1.516s
    goc/testdata/hello.go  iter=3  1.544s
    goc/testdata/hello.go  iter=4  1.529s
    iters=1  wall=2.07 user=3.35 sys=0.31 maxrss=296448kB
    iters=5  wall=8.22 user=13.63 sys=0.67 maxrss=372512kB

From the two `/usr/bin/time` rows, one warm compile costs `(13.63 − 3.35) / 4 = 2.57 s` of CPU
and ~1.52 s of wall. The first compile costs ~3.3 s of CPU and 2.05 s of wall. So:

| | wall | CPU |
| --- | ---: | ---: |
| building the shared source world (once per process) | **0.53 s** | **0.73 s** |
| compiling `hello.go` with the world already built | 1.52 s | 2.57 s |
| reading the 8.8 MB pack (once per process) | 0.021 s | — |

For reference the whole `goc` process on the same program is `wall=2.16 user=3.67` (three runs:
2.16/2.15/2.17), so the ~0.1 s of wall unaccounted for above is process start plus the `cc`
link.

**The world is not the whole amortizable cost.** A one-shot `goc` process on the same program
costs 3.67 s of CPU, against 2.57 s for a compile in a worker that already has a world — so the
per-compile saving is **1.10 s of CPU**, not 0.73 s. The extra 0.37 s is process start, the pack
read, and the Go runtime collecting a fresh ~300 MB heap in every process rather than reusing a
live one. Over 322 amortized compiles that is ~354 CPU-s.

The saving is roughly constant per compile and independent of the program, because the world is
only the runtime's closure — everything a program imports beyond that is loaded privately either
way. It is emphatically **not** the ~700 s the briefing estimated.

**Where the other 1.5 s of `hello.go` goes** is not the world and is therefore not this lever:
against a prebuilt pack, `compile()` still generates IR for the whole runtime closure and only
then subtracts the functions the pack already defines. That is per-program work repeated 338
times. It is the largest single remaining item in a small program's compile and it is noted here
for whoever integrates the three branches; it is not touched by this job.

## 2. What was built

### 2.1 `goc compile-batch`: a request stream, not a manifest

    goc compile-batch [-O] [-target arch] [-runtime runtime.gocrt] < requests.jsonl

One JSON object per line of stdin — `{"source": "prog.go", "output": "prog.bin"}` — and one
JSON object per line of stdout in reply, carrying the error (if any), the compile's wall clock
and the worker's peak RSS. EOF on stdin ends the process.

**Why a stream and not a manifest.** A manifest — hand a process a fixed list of programs —
is simpler, and it partitions the work statically. The matrix's compile schedule is dynamic on
purpose: §17 of RUNTIME_PLAN dispatches longest-first through a shared pool so the eleven
125–167 s programs start at t=0, and bounds how far compilation may run ahead of the run phase
so compiled-but-not-yet-run executables cannot fill a small `/tmp`. Both are properties of a
*queue*. A static partition destroys them — one worker handed three expensive programs is still
compiling when the machine has gone idle. A request stream keeps the queue exactly as it was
and changes only what a worker is: a process that outlives its programs instead of one that
exits after each.

**Why the configuration is process-wide and not per request.** Target, prebuilt runtime and
`-O` are exactly the axes of goc's source-world key: they decide which files a package is built
from and how it type-checks. A worker that accepted them per request would silently build a
second world on the first request that differed, and double its memory where nobody could see
it. Making them command-line flags makes a worker one build configuration by construction.

**What a batch worker does not do:** `-runtime-covermeta`. Runtime coverage instruments the
runtime per program, which is the opposite of one build configuration per process, so the
coverage run keeps the one-shot path it already had (`newRuntimeCapabilityBatchPoolFor` returns
nil whenever `-runtime-coverprofile` is set). That is a deliberate boundary, not an omission:
adding a mode this job cannot verify end to end would be worse than not adding it.

### 2.2 One bad program costs one program

A one-shot `goc` that rejects a program exits and the next program gets a fresh process. A
worker has no fresh process to offer, so it has to keep going. Three things make that safe:

- a compile error is a response, not an exit;
- a **panic inside the compiler is recovered per request** and reported, with its stack, as
  that program's error — so a compiler bug costs one program instead of every program still
  queued behind that worker;
- the pool never reuses a worker whose request failed at the protocol level (died, or replied
  out of step). It stops it and the next acquire starts a replacement, so a worker that is
  killed — by the OOM killer, say — costs one capability, which is exactly what a one-shot
  compile killed the same way would have cost.

Diagnostics are never written to a worker's own stderr: that stream is shared by every program
it compiles and could not be attributed afterwards. `cc`'s output is captured per program and
folded into that program's error.

### 2.3 The matrix harness

`cmd/goc/runtime_status_batch_test.go` adds a pool of workers, and the compile queue dispatches
through it. Everything else about the schedule is untouched — same `compileRuntimeCapabilityWorkers()`
count, same longest-first order, same look-ahead bound. The pool shuts down inside
`drainCompiles`, so the exclusive run phase — the programs whose outcome depends on how much of
the machine they have — starts with no compilers on the box at all, rather than with W idle
workers each holding a parsed standard library.

`-runtime-status-batch-compile=false` restores the old one-process-per-program path. It exists
so the A/B below is a measurement rather than a comparison against a different tree.

## 3. Correctness: is anything leaking between compiles?

This is the whole risk, so it is checked three ways.

### 3.1 Unit tests (`cmd/goc/batch_test.go`)

| test | what it pins |
| --- | --- |
| `TestBatchCompilesMatchOneShotCompiles` | four small programs, each compiled alone twice, in a batch, and in a batch that sees them in the opposite order; the batch builds must be the same bytes as the solitary one |
| `TestBatchCompilerSurvivesAProgramItCannotCompile` | a program that does not type-check is reported against *its own* request, and the next program in the same worker still compiles and runs |
| `TestBatchCompilerSharesItsWorldAcrossPrograms` | the second and third compiles of the same program in one worker are faster than the first — the direct evidence that the world is actually shared |

The first test compiles alone *twice* on purpose. Some programs in this corpus do not compile
deterministically even alone (see §4), and for those, byte equality is not a question that can
be asked; the test says how many programs it was able to ask it of and fails if that falls
below two, so it cannot quietly become vacuous. All three tests pass; the third logged
`2.14s 1.60s 1.62s`.

### 3.2 The whole corpus, three ways: `analysis/batchdiff`

`analysis/batchdiff` compiles every program in `goc/testdata` three ways and compares the
executables byte for byte:

- **one-shot** — one `goc` process per program, which is what the matrix did before;
- **batch** — a pool of 16 `goc compile-batch` workers, dispatched dynamically, so the grouping
  is whatever the schedule produces;
- **batch-reversed** — the same pool fed the same programs in the opposite order, so every
  worker sees a different history.

All 358 programs, 16 workers:

    one-shot         358 programs in 303.4s, 0 failed
    batch            358 programs in 265.6s, 0 failed
    batch-reversed   358 programs in 221.2s, 0 failed
    summed per-program compile wall: one-shot=3386.3s batch=3189.5s (196.8s saved, 5.8%)

    identical=319 differing=39
    leaks=5 nondeterministic-alone=34
    behaviour: identical=358 differing=0

**The 5.8% is the lever, measured end to end on the real corpus,** and it lands within a
percent of the 6% §1 predicted from a single program.

**All 358 programs produce identical exit status and output from all three builds.** That is
the differential check the rules ask for, and it is clean.

### 3.3 The 39 that differ, and why none of them is a leak

`batchdiff` classifies a differing program as a leak only if compiling it alone a second time
reproduced the first solitary build exactly. 34 of the 39 failed that immediately — they are
not deterministic on their own, so byte equality is not a question that can be asked of them.

The remaining 5 were each compiled alone 8 more times (4 for the 136 s `smtp_session`):

| program | distinct binaries from repeated one-shot compiles |
| --- | --- |
| `fmt_println.go` | 2 of 8 |
| `runtime_loopvar_shared_scope.go` | 2 of 8 |
| `stdlib_runtime_trace_start_stop.go` | 2 of 8 |
| `stdlib_smtp_session.go` | 4 of 4 |
| `runtime_assembly.go` | **1 of 8** |

Four are settled: they vary between solitary compiles, so the two-sample check simply got two
matching samples. `runtime_assembly.go` needed one more experiment — eight compiles of *the
same program, back to back, in one worker*:

    b6b2ec323982   <- compile 1
    bb4bbccd20db   <- compiles 2, 4, 5, 6, 7, 8  (and every one-shot compile)
    cc51d6241aa3   <- compile 3

Six of the eight reproduce the one-shot bytes exactly, and the two outliers are the same two
variants `batchdiff` saw. A leak is a function of history: identical requests in one worker
would give a consistent — or at least monotone — answer, and the batch value would not be the
one-shot value. A coin flip gives exactly this. `runtime_assembly.go` is nondeterministic like
the other 38, with a per-compile probability low enough that eight solitary samples missed it.

**Conclusion: 0 leaks in 358 programs, across two different worker groupings, with identical
behaviour from every build.**

## 4. A finding outside this lever: compile nondeterminism is 11% of the corpus, not one program

The branch's determinism gate samples five programs and records one known exception
(`runtime_defer_capture_allocs.go`, "a known backend residue"). The sweep above says the
sample understates it by an order of magnitude:

- **39 of 358 corpus programs (10.9%) compiled to different bytes** within this run, and
- `runtime_assembly.go` shows the per-compile probability can be low enough to survive eight
  samples, **so 39 is a floor, not the count.**

It is **pre-existing and nothing to do with this job.** Verified directly: `goc` built from the
branch point `perf/test-suite` (`0f4ee02`), compiling `goc/testdata/allocs_per_run.go` three
times, produces three different binaries:

    abb87d396d8b...  121659db3d3d...  7f4717224648...

The differences are small and in `.text`: `allocs_per_run.go` differs in 106 bytes of 14.7 MB,
20 of which are the linker's build-id note (which differs *because* the text does). The
scattered single-byte differences inside 4-byte instructions are the shape of a register
allocation that came out differently — which matches §5.10 and the sibling `goc-parallel`
briefing's note that register allocation is the leading suspect.

This matters to the branch beyond my lever: **"output is byte-identical" cannot be the merge
gate for the whole corpus, because it is not true of the corpus today.** For eleven percent of
programs the gate would fire on the compiler's own coin flip. Behaviour equality — which
`batchdiff` now also checks, and which was clean for all 358 — is the property that actually
holds.

## 5. What it measures

### 5.1 The box was not mine, and that decides which metric is usable

Every number here was taken while **at least one sibling job was compiling on the same
machine**. Load average during the runs moved between 14 and 161, sometimes inside a single
pair. Absolute wall clock is therefore not comparable with the 203 s baseline, which was
measured on an exclusive box.

It also rules out the harness's own metric. `scripts/matrix-timing.sh` reports "compile CPU" as
the **sum of per-compile wall clock**. On a saturated machine that is not work: each compile's
wall clock is set by its CPU share, so the sum converges on `workers x elapsed` whatever the
compiler does. The first A/B pair showed exactly that — 3534 s against 3541 s, a 0.2%
difference, on a change that demonstrably removes work. So every run below is also measured
under `/usr/bin/time`, which reports the CPU actually consumed by the run and all its children.

### 5.2 Six A/B pairs, every run 338/338

Each pair is back to back, same tree, one flag apart. `load` is the 1-minute average when the
run started.

| workers | mode | wall | user+sys CPU | load | CPU saved by batch |
| ---: | --- | ---: | ---: | ---: | ---: |
| 8 | batch | 438.1 s | 4048.1 s | 23.6 | |
| 8 | one-shot | 403.5 s | 4150.0 s | 47.6 | 101.9 s (2.5%) |
| 16 | batch | 221.2 s | 3972.8 s | 20.5 | |
| 16 | one-shot | 249.3 s | 4456.4 s | 30.1 | 483.6 s (10.9%) |
| 16 | one-shot | 249.9 s | 4473.9 s | 55.0 | |
| 16 | batch | 219.6 s | 3953.6 s | 38.9 | 520.3 s (11.6%) |
| 24 | batch | 358.6 s | 4334.7 s | 26.7 | |
| 24 | one-shot | 258.3 s | 4449.3 s | 161.5 | 114.6 s (2.6%) |
| 24 | batch | 262.9 s | 4211.7 s | 33.3 | |
| 24 | one-shot | 233.6 s | 4458.9 s | 63.4 | 247.2 s (5.5%) |
| 64 (default) | batch | 208.4 s | 3918.7 s | 16.8 | |
| 64 (default) | one-shot | 273.5 s | 4522.5 s | 13.9 | 603.8 s (13.4%) |

Plus a final run on the exact committed tree at 16 workers: `wall=213.9 user=3803.1 sys=117.2`.

**Fifteen full unsharded matrix runs in total. Every one of them:**

    subtests=338 pass=338 fail=0 skip=0 declaredPASS=337 expectedFAILURE=1 knownGAP=0

### 5.3 What the six pairs actually support

**CPU favours batch in all six pairs, by 2.5% to 13.4%. Wall clock favours it in three of
six.** The wall-clock disagreements are not subtle and they are not about the change: the
24-worker pair where one-shot "won" by 100 s began its two runs at load 26.7 and load 161.5.

The tightest measurement is the 16-worker quadruple, taken in both orders inside a
twenty-minute window: within-mode spread of 1.6 s of wall and 19 s of CPU, against a 29 s /
502 s effect. It says 11%.

Two contention-free measurements bound it from the other side:

| method | work removed |
| --- | ---: |
| isolated per-compile bench (§1), 1.10 s of CPU x 322 amortized compiles | ~354 CPU-s |
| `batchdiff` over the 358-program corpus, summed per-program compile wall | 196.8 s (5.8%) |
| the 16-worker matrix quadruple | 502 CPU-s (11.2%) |

**The honest statement: batch compilation removes real work, somewhere between about 5% and
12% of what the matrix costs, and this box could not narrow it further.** The three methods
disagree because CPU time is itself inflated by contention — a run that overlaps a busy sibling
burns more CPU for the same work — and the two arms of a pair never see identical load.

### 5.4 Which term bounds the matrix afterwards

Unchanged: **the slowest single compile.** `stdlib-http/tls-client-server` is 157.6 s alone on
an idle box, and no part of this lever touches it — the world it skips is 0.53 s of that.

    wall ~ max( slowest compile , compile CPU / workers ) + run phase + setup

What moves is the second term. That is why the wall-clock win is largest exactly where the
second term binds — at 64 workers on a contended box (-23.8%) and at 16 (-11.7%) — and why on
an exclusive box at 24+ workers, where the first term dominates, the same saving would buy
close to nothing. §1 said this before any of it was measured and the measurements did not
change it.

**So the value of this lever is not the matrix's best case. It is that the suite costs about a
tenth less to run anywhere, and that the cost is less sensitive to how much of the machine the
suite actually gets** — which is the situation that produced the 406.5 s figure §17 was written
about.

### 5.5 The worker-count table: what can and cannot be said

The briefing asks for it, because a batch worker is a different thing from a compile slot.

**The memory bound per worker is unchanged, and that is measured.** Peak RSS over all 338
compiles was **2635.0 MiB batched against 2637.4 MiB one-shot**. A worker's peak is still the
largest program it compiles, not the sum — batching does not accumulate. What it adds is a
retained world per idle worker, which raises the *median* observed peak from 388 MiB to
1206 MiB but not the maximum, and the maximum is what the bound is for. So
`compileRuntimeCapabilityPeakBytes = 3 GiB` and the divisor built on it stand.

**The shape of the wall-clock-versus-workers curve cannot be said.** Two of the six pairs
inverted under load swings of 2x to 6x within the pair. A table built from those numbers would
describe the sibling jobs, not the change. **The default worker count should be re-derived on an
exclusive box before this branch merges; this job could not do it and did not fake it.** The
one thing worth carrying forward is that at the default (64) the batch run was the fastest
configuration measured here, 208.4 s, and its one-shot control was the slowest at that worker
count, 273.5 s.

## 6. A trap for whoever verifies this next

`goc` locates the vendored standard library with `runtime.Caller(0)` (`goc/source_import.go:325`),
so a `goc` binary built from a git worktree at a different path resolves the stdlib through that
path — and embeds it in every binary it produces. Comparing a build made by a `goc` built in
`/tmp/base` against one made by a `goc` built in `/repo` shows a 4096-byte size difference and a
million differing bytes, none of which is a code difference: it is 178 embedded path strings of
a different length.

That is how the "before" half of the determinism comparison must *not* be taken. The valid
statement is the one below.

## 7. Determinism: before and after

`scripts/determinism-check.sh -runtime <pack>` — cold (`CG12_NOCACHE=1`) against warm, twice,
five sample programs.

| program | branch point `0f4ee02` | this branch |
| --- | --- | --- |
| `hello.go` | identical ×2 | identical ×2 |
| `fmt_sprintf.go` | identical ×2 | identical ×2 |
| `gc_struct.go` | identical ×2 | identical ×2 |
| `runtime_cleanup_frame_retention.go` | identical ×2 | identical ×2 |
| `runtime_defer_capture_allocs.go` | **differs, then identical** | **differs ×2** |

4 of 5 identical on both, and the fifth is the documented exception. Note that at the branch
point the fifth was *identical* on the second round — more evidence for §4: it is a coin flip,
not a cache effect, and this check is sampling it rather than measuring the cache.

**The compiler is bit-identical to the branch point by construction.** The non-test Go files
this job changes are `cmd/goc/main.go`, `cmd/goc/prebuilt.go`, `cmd/goc/batch.go` and
`analysis/batchdiff/main.go`. Nothing under `goc/`, `ir/`, `opt/`, `arm64/`, `link/` or `obj/`
is touched, so no compilation this branch performs can differ from one the branch point would
have performed.

## 8. Suites

| suite | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | clean |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 893.407s` |
| `make test-goc-cmd` | `ok github.com/evanphx/cg12/cmd/goc 232.685s` |
| full capability matrix | 15 unsharded runs, every one 338/338 |
| `analysis/batchdiff`, whole corpus, three ways | 0 leaks, 358/358 identical behaviour |
| `scripts/determinism-check.sh` | 4 of 5 identical, before and after (§7) |

### The complete list of non-passing capabilities

One, and it is declared: **`defer-panic/panic-string-output`**, an `expectedFailure`, appearing
as `EXPECTED FAILURE runtime_panic_print_string.go`. Across all fifteen full runs: `FAIL=0`,
`KNOWN GAP=0`, `SKIP=0`.

## 9. Still unverified

- **The worker-count curve.** §5.5. Needs an exclusive box. The memory bound *was* measured and
  is unchanged.
- **The wall clock on an idle machine.** Every wall-clock number here has a sibling job in it.
  The CPU numbers are the ones to trust, and even they carry contention inflation.
- **The coverage path (`make test-goc-coverage`) was not run, and does not use batch mode.**
  `newRuntimeCapabilityBatchPoolFor` returns nil whenever `-runtime-coverprofile` is set, so
  that path is byte-for-byte the one that was there before; but the suite itself was not run,
  for the reason §17 already records — a targeted coverage run fails by construction, and the
  whole corpus under instrumentation did not fit in this job.
- **Interaction with the two sibling branches.** This branch was measured alone. `pack-stdlib`
  changes what the pack contains and `goc-parallel` changes what one compile does inside; both
  interact with a long-lived worker in ways nothing here exercises. Two notes for that
  integration:
  - a batch worker retains its source world for the life of the process, so if `pack-stdlib`
    makes the pack carry the standard library, the world a worker builds may become *larger*
    (more packages seeded) or unnecessary (subtracted earlier). The 3 GiB per-worker bound
    should be re-measured, not assumed, after that merge.
  - `goc compile-batch` compiles one program at a time per process. If `goc-parallel` makes a
    single compile use N cores, then a pool of W workers becomes W x N and the worker count has
    to be divided, not kept.
- **Whether batching changes the *rate* of the §4 nondeterminism.** All 39 differing programs
  are explained by pre-existing nondeterminism and every build behaved identically, but a
  different heap shape could plausibly change how often the coin lands the other way. Nothing
  here measures that, and behaviour equality is unaffected either way.

## 10. One thing found that is not mine to fix

The largest remaining item in a small program's compile is **not** the per-process cost this
job removed. Compiling `hello.go` against the prebuilt pack still costs 1.52 s of wall and
2.57 s of CPU with the world already built, and most of that is `compile()` generating IR for
the whole runtime closure and *then* subtracting the functions the pack already defines. That
work is repeated for all 338 programs and is nearly identical every time.

It is adjacent to `pack-stdlib`'s area rather than mine, so it is reported rather than touched:
if the split could subtract before generating rather than after, the floor for a small program
would fall from ~2.1 s to something near the link time.
