# Verification: what to run, how long it takes, and what each tier cannot see

Two entry points:

    make verify-fast     a trustworthy answer in under ten minutes
    make verify-full     the exhaustive run

Everything below was measured on the reference worker: aarch64 Linux, 64 cores,
250 GiB total / 243 GiB available, go1.26.1, otherwise idle.

## The numbers

| | before | after |
|---|---:|---:|
| goc corpus suite, one `go test` | **18:57** (4.0 of 64 cores) | **8:15** |
| goc corpus suite, split across processes by `verify.sh` | — | **3:44** |
| a gate's four items (corpus + both arms + a `main` control of each) | **45:57** | **~5:44** (warm control) |
| `make verify-full`, which covers strictly more than those four | — | 56:05 cold control, **31:21** warm |
| `make verify-fast` | did not exist | **3:58** |
| the `main` control | re-measured every gate | **1484 s → 0.96 s** when the key matches |

`TestAllocationCensus`, on its own, went 183.1 s → 82.6 s when its compile pool
stopped being capped at a flat 8.

`verify-full` should not be read against the old 45:57 as though they were the
same run. It adds both determinism sweeps (410 s + 886 s — together now the
largest thing in the tier), `test-ruby`, `test-goc-cmd`, the unit tests, vet,
gofmt and the full-count reducers, none of which the old four-item recipe ran. A
gate that wants only what that recipe covered costs under six minutes.

## Why this exists

Verification in this tree took forty to a hundred and twenty minutes, which is
too slow to steer by — by the time a run says no, the thing it says no about is
three changes old. There were two causes and the smaller one was the one
everybody knew about.

**The goc suite ran serially.** 349 top-level tests, in one package, none of them
calling `t.Parallel`, on a 64-core box. Every gate was briefed to run
`go test -parallel 10 ./goc/...`, and the 10 was blamed for coming from an
eight-core laptop. It was worse than that: `-parallel` bounds tests that *opt in*
with `t.Parallel`, and no test in the package did, so the flag bounded nothing.
`-parallel 10` and `-parallel 64` were the same command. Measured, the suite ran
at **4.0 of 64 cores for nineteen minutes**, with 5% of memory in use.

Raising the number would have bought exactly nothing. 267 of the 350 top-level
tests in `goc` now call `t.Parallel` as their first statement, and
`goc/parallelpolicy_test.go` fails if a new one does not.

The other 83 are in `goc/sequential_tests.txt`, which says why about each. 77 of
them are there because of a finding: at `-parallel 32`, `TestAllocationCensus`,
`TestFrameEscapeAudit` and `TestInterfaceConversionsCallTheRuntimeHelpers` fail,
with every difference in the conservative direction — an allocation the serial
run keeps in a frame is on the heap. They pass at `-parallel 32` when nothing
else in the process is compiling, and they still fail with `CG12_NOCACHE=1`, so
the shared source world is not the mechanism. **Concurrent compiles inside one
process perturb escape placement, and the root cause is not isolated.** Nothing
in production does that — `goc` compiles one program per process, and
`goc compile-batch` reads its request stream in a plain `for requests.Scan()`
loop and compiles strictly in sequence within each worker — so this constrains
how the suite may be parallelised rather than describing a shipped defect.

Go runs every sequential top-level test to completion *before* it resumes any
parallel one, so a listed test gets exactly the quiet process it had when the
whole suite was serial. Nothing about how those 83 tests execute has changed,
which is the reason this does not weaken the verification.

The listed set is deliberately over-broad — every test that reaches a placement
assertion by any call path, not the three observed to fail. The cost of an
unnecessary entry is seconds; the cost of a missing one is a flaky gate.

**A gate ran its long items one after another.** The corpus suite, the default
capability arm, the `-O` arm, and a `main` control of each — four serial items on
a machine with sixty-four cores, three of which were idle at any moment.
`scripts/verify.sh` runs the independent ones concurrently under a core budget,
shards the capability matrix across `STATUS_SHARDS`/`STATUS_SHARD` (which existed
and which no gate had ever used), and reuses a recorded `main` control instead of
re-measuring one that cannot have changed.

## How many tests run at once

`GO_TEST_PARALLEL` in the Makefile, and it is a **default**, not something a
caller passes. It is bounded by memory rather than by cores: a goc compile of the
corpus's worst program peaks at 3.17 GiB RSS at the default GOMAXPROCS (4.23 GiB
at `GOMAXPROCS=1`), and every concurrent test can be compiling one. The default
is `min(nproc, MemAvailable / TEST_MEMORY_PER_JOB, TEST_PARALLEL_CAP)`, which is
32 on the reference worker and 8 on an eight-core laptop, neither caller needing
to know about the other.

**32 is where it stops because of what the profile says, not because memory ran
out.** At `-parallel 32` the suite peaks at 29.8 GiB — **12% of the 243 GiB
available** — so memory is nowhere near binding. What binds is the shape of the
work: of the 495 s the suite takes in one process, 350 s is the sequential set
(which no `-parallel` value touches) and the remaining 145 s is *one test*,
`TestSlogAttrInFrameIsNotScannedAsAPointer`, inside whose shadow the other 266
finish. Raising the number past 32 has nothing left to overlap.

That is also why `scripts/verify.sh` splits the suite across **processes** rather
than raising the number further: the sequential set and the parallel set share no
compiler state when they are separate processes, so they cost the larger of the
two instead of their sum. 8:15 in one process, 3:48 split.

## Timing suites are never scheduled by either tier

`make bench-perf` and `make bench-crypto` measure elapsed time. They pin to a
core and they have a pre-flight that refuses a busy box, because the ceilings
that catch a contaminated run catch it at the *end* — eleven minutes in. Neither
verify tier runs them, and neither should: a tier that ran a timing suite
alongside a 32-wide corpus would be measuring the corpus. Run them alone, on an
idle machine. `TestCryptoSigningBench` and `TestPerformanceSuite` are two of the
six tests explicitly kept sequential in `goc/parallelpolicy_test.go`, so they
cannot be swept into a parallel run by accident either.

## What each tier covers

| item | verify-fast | verify-full |
|---|---|---|
| `go build ./...` | yes | yes |
| `go vet ./...` | yes | yes |
| gofmt (excluding vendored `stdlib/`) | yes | yes |
| unit tests (38 packages) | yes | yes |
| goc corpus suite, all 349 tests | yes | yes |
| the four corpus audits, check mode | yes (inside the corpus suite) | yes |
| capability matrix, default arm | **shard 0 of 4** | all 4 shards |
| capability matrix, `-O` arm | **shard 0 of 4** | all 4 shards |
| targeted reducers | reduced counts (5–40) | gate counts (20–400) |
| `test-goc-cmd` (driver end-to-end) | **no** | yes |
| `test-ruby` (C differential + miniruby) | **no** | yes |
| determinism, all 406 programs, byte-identical | **no** | yes, both arms |
| runtime coverage report + baseline diff | no | no — `make test-goc-coverage` |
| comparison against a `main` control | **no** | yes (cached) |
| anything timing | **never** | **never** |

## How the suite is split across processes

`scripts/verify.sh` runs the goc corpus as four concurrent `go test` processes,
not one:

* **`corpus-parallel`** — `-skip` over the 83 listed names, at
  `-parallel $(GO_TEST_PARALLEL)`.
* **`corpus-sequential-0..2`** — a partition of those 83, each at `-parallel 1`,
  which is the same one-compile-at-a-time guarantee the in-process sequential
  phase gives. The four corpus audits are pinned to shard 0 because they share a
  single `sync.Once` corpus compile and splitting them would pay for it three
  times.

The split covers every test exactly once, and that is checked rather than
asserted: the parallel half's `-skip` set is the exact complement of the union of
the three shards' `-run` sets, verified against `go test -list` — 350 = 267 + 83,
`diff` empty.

Note that a plain `make test-goc-corpus`, or CI's `go test ./goc/...`, is still
correct on its own: the listed tests have no `t.Parallel`, so Go runs them in its
sequential phase before resuming anything. The process split is a speedup on top
of a default that is already safe, never a precondition for it.

## What verify-fast cannot catch

Stated plainly, because a fast tier that quietly skips something is worse than no
fast tier. In the same spirit as the benchmark suites' documented detection
limits, these are the things a green `verify-fast` does **not** rule out:

1. **A capability regression outside shard 0.** Three of every four capabilities
   are not compiled or executed on either arm. The matrix is partitioned by
   capability index modulo 4, so shard 0 is a fixed quarter, not a random one —
   the same 92 capabilities every time. A regression confined to the other 276 is
   invisible.
2. **A determinism regression.** `scripts/determinism-check.sh -corpus` is the
   only thing in the tree that drives all 406 programs to a written object and
   compares bytes across repeated compiles, and nothing in the fast tier
   substitutes for it. The corpus suite proves programs *work*, not that two
   compiles of them agree.
3. **A Ruby/C frontend regression.** `test-ruby` is not run.
4. **A goc driver regression.** `test-goc-cmd` is not run: pack building, batch
   compilation, the CLI's own end-to-end behaviour.
5. **A rare intermittent fault.** The reducers run at fast counts. RUNTIME_PLAN.md
   section 5.10 records a fault that appeared 3 times in 53 compiles; 5 or 30
   repetitions does not reach it. The fast reducer counts catch a gross
   regression — something that fails most of the time — and nothing subtler.
6. **A movement relative to `main`.** verify-fast reports whether the tree is
   self-consistent and green. It does not tell you whether the pass set, the
   binary output or the capability count moved, because it runs no control.
7. **Any performance change at all.** No tier does. That is `make bench-perf` and
   `make bench-crypto`, alone, on an idle box.

`verify-fast` is the right signal for *is this change broken*. It is not
sufficient for a merge gate; `verify-full` plus the timing suites is.

## The `main` control cache

A control only changes when `main` changes, so re-measuring one every gate was
the largest avoidable cost in the old recipe — it doubled everything. A recorded
control is reused when, and only when, all of these match:

| in the key | why |
|---|---|
| the `main` commit | the thing being controlled for |
| CPU model, core count, total RAM, arch, kernel release | a control is a *measurement*; one taken on a different box is a different experiment, not a control for this one |
| `go version`, `GOOS/GOARCH`, the system `cc` version | the toolchain produces the artifact being compared |
| a hash of `scripts/verify.sh` | changing what a control *measures* invalidates every record automatically, rather than depending on someone bumping a version constant |

Kernel release is in the key because it moves scheduling and page-fault behaviour
and costs nothing to include; the price is an occasional needless re-measure,
which is the right side to err on. The branch under test, the working tree and
the wall-clock time are deliberately *not* in the key — anything describing the
branch would defeat the reuse this exists for.

The cache lives at `${XDG_CACHE_HOME:-$HOME/.cache}/cg12/verify-controls`,
outside the working tree, because jobs get their own worktree and a tree-local
cache would never hit across jobs. `VERIFY_CACHE` overrides the location;
`VERIFY_NO_CONTROL_CACHE=1` forces a fresh measurement;
`VERIFY_NO_CONTROL=1` skips controls entirely.

## `CG12_NOCACHE=1`

Unchanged and still load-bearing. It disables the in-process source-world cache
(`goc/source_world.go`), which is what makes a merge gate build cold: a cache
defect can then cost time but cannot produce a passing test from a package that
was never rebuilt. Nothing in the parallelisation touches it — the source-world
map was already mutex-guarded and is shared across concurrent tests exactly as it
was across sequential ones.
