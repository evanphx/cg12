# Batch compilation: one goc process, many programs

Branch: `ccwork/goc-batch-b`, off `perf/test-suite` (`0f4ee02`). The previous job's report has
been moved to `docs/report-matrix-speed.md`, following the precedent that job set, so this file
is only about this job.

Status: **in progress.** Everything stated below was measured on this box. Anything not
measured is named as such.

## Headline, up front: the lever is real but it is a sixth of its estimate

The briefing sizes this lever at **~700 s of the ~3030 s of compile CPU (23%)**, on the
reasoning that `hello.go` costs `wall=2.11s` against the pack and "most of it is loading and
type-checking the runtime's source closure", which `goc/source_world.go` caches per process.

**Measured, the amortizable part is ~0.53 s of that 2.16 s, not ~2 s.** The rest is per-program
work that a shared world does not touch. See §1 for the measurement. That puts the lever at
roughly **180 s of the ~3030 s (6%)**, and — at the matrix's current floor — approximately
**zero wall clock**, because the matrix is bounded by one 190 s single-threaded compile and not
by compile CPU / workers.

That does not make the work pointless: it makes it a *precondition*. The moment the sibling
`pack-stdlib` lever removes the six 160 s `net/http` compiles, the floor drops and
`compile CPU / workers` becomes the bounding term — and then 180 s off the numerator is a real
6% off the wall clock. It is reported here honestly rather than claimed at the briefing's size.

## 1. How big the per-process fixed cost actually is

The world is built by the first compile in a process and reused by every later one, so the
amortizable cost is exactly `first compile − later compile` in one process. Measured with a
scratch main (`analysis/worldbench`) that reads a prebuilt pack from disk and compiles the same
program N times through `prebuilt.CompileProgram`, the same call `goc -runtime` makes:

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

**What this means for the matrix.** The saving is a constant ~0.53 s per compile, independent of
the program, because the world is only the runtime's closure — everything a program imports
beyond that is loaded privately either way. Over 338 programs that is ~180 s of the ~3030 s of
compile CPU the matrix spends, i.e. **6%**, not 23%.

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

### 5.1 The box was not mine

Every number in this section was taken while **two sibling jobs were compiling on the same
machine**. Load average during the runs ranged from 20 to 161. That makes absolute wall clock
incomparable with the 203 s baseline, which was measured on an exclusive box, so every
measurement below is an **A/B taken back to back**, and the A/B is repeated in both orders.

It also changes which metric is meaningful. `scripts/matrix-timing.sh` reports "compile CPU" as
the *sum of per-compile wall clock*. On a saturated machine that quantity is not work: each
compile's wall clock is set by its CPU share, so the sum converges on `workers × elapsed`
whatever the compiler does — which is exactly what the first A/B pair showed (3534 s vs 3541 s,
a 0.2% difference, on a change that demonstrably removes work). So the matrix was re-measured
under `/usr/bin/time`, which reports the CPU actually consumed by the run and all its children.

### 5.2 The A/B, at 16 compile workers, run in both orders

| run | mode | wall | user | sys | user+sys | subtests |
| ---: | --- | ---: | ---: | ---: | ---: | --- |
| 1 | batch | 221.2 s | 3857.7 s | 115.1 s | **3972.8 s** | 338/338 |
| 2 | one-shot | 249.3 s | 4284.9 s | 171.5 s | **4456.4 s** | 338/338 |
| 3 | one-shot | 249.9 s | 4301.8 s | 172.1 s | **4473.9 s** | 338/338 |
| 4 | batch | 219.6 s | 3835.6 s | 118.0 s | **3953.6 s** | 338/338 |

    batch    mean: wall 220.4 s, CPU 3963.2 s
    one-shot mean: wall 249.6 s, CPU 4465.2 s
    saving:        wall  29.2 s (11.7%), CPU 502.0 s (11.2%)

The spread within each mode is 1.6 s of wall and 19 s of CPU, against a 29 s / 502 s effect, so
the two modes do not overlap. **The matrix costs 11.2% less CPU with batch compilation**, of
which 56 s is system time — the process creations that no longer happen.

Every one of the four runs: `subtests=338 pass=338 fail=0 skip=0 declaredPASS=337
expectedFAILURE=1 knownGAP=0`.

### 5.3 Why the saving is larger than §1 predicted

§1 measures the world build at 0.73 s of CPU, which over 322 amortized compiles is ~235 s. The
matrix saves 502 s. The rest is the part of a *process* that is not the world:

    one-shot `goc` on hello.go, whole process   user 3.67 s
    first compile in a batch worker             user ~3.33 s   (world + program)
    later compiles in the same worker           user  2.57 s

so the amortizable cost is **1.10 s of CPU per compile**, not 0.73 s: the world, plus process
start, plus the Go runtime collecting a fresh 300 MB heap in every one of 338 processes instead
of reusing a live one. 322 × 1.10 = 354 s on `hello.go`'s shape; the corpus average is larger
because the larger programs have more garbage to collect against the same retained world.

### 5.4 Which term bounds the matrix afterwards

Unchanged: **the slowest single compile.** `stdlib-http/tls-client-server` took 199 s in the
batch runs and 217 s in the one-shot runs — the difference is not the compile getting faster,
it is 11% more of a saturated box being available to it. On an idle machine that compile is
157.6 s and no amount of this lever touches it.

    wall ≈ max( slowest compile , compile CPU / workers ) + run phase + setup
             199 s                  3963/16 = 248 s ... at 16 workers on a *contended* box the
                                    second term still binds, which is why the wall clock moved
                                    at all.

At 16 workers on this box the two terms are comparable, so removing CPU shows up in the wall
clock. On the exclusive box at 24+ workers the first term dominates and the same 502 s of CPU
would buy close to nothing — which is what §1 said up front. **This lever's value is that it
makes the wall clock less sensitive to how much of the machine the suite actually gets, and
that it takes 11% off the total cost of running the suite anywhere.**

### 5.5 The worker-count table could not be honestly re-measured

The briefing asks for it, because a batch worker is a different thing from a compile slot. Two
things can be said, and a third cannot.

**Said, from the data:** the memory bound per worker is unchanged. Peak RSS over all 338
compiles was **2635.0 MiB batched against 2637.4 MiB one-shot** — a worker's peak is still the
largest program it compiles, not the sum, so `compileRuntimeCapabilityPeakBytes = 3 GiB`
remains the right divisor. What batching adds is a retained world per *idle* worker, which
raises the median observed peak (388 MiB → 1206 MiB) but not the maximum, and the maximum is
what the bound is for.

**Said, from the data:** at 16 workers the default is not the optimum on a shared box. The
default picks 64 here (`MemAvailable` is 232 GiB, so memory never binds), and 24 workers
measured 358.6 s batched against 220 s at 16 — because the machine already had two other jobs
on it.

**Cannot be said:** the shape of the wall-clock-versus-workers curve. A run at 24 workers took
358.6 s and its sibling one-shot run took 258.3 s, in the opposite direction to every other
measurement here, because the load average went from 26 to 161 between them. Numbers like that
are about the box, not about the change, and reporting a table built from them would be worse
than reporting none. **The worker-count default should be re-derived on an exclusive box before
this branch is merged; this job could not do it and did not fake it.**

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

## 8. Still unverified

This section is updated as results land.
