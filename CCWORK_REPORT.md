# Verification of `ccwork/variadic-allocations`

Job: run the suites to completion against the branch and report. No fixing, no
refactoring. Findings are appended as each suite exits.

## What is under test

| | commit | |
|---|---|---|
| branch | `034e58c` | `origin/ccwork/variadic-allocations` |
| control | `a535466` | `main` (the actual merge target) |
| merge-base | `e7c8e33` | what the task's reference numbers were taken at |

`main` has moved since the reference numbers were quoted: `a535466` merged
`origin/ccwork/escape-gc-differential`, which added `internal/gcdiff` and
`goc/gcdiff_test.go`. Reference figures quoted at `e7c8e33` therefore do not
match a fresh `main` run, and every difference below is reconciled against the
commit that caused it rather than accepted.

Branch content relative to `main` (`git diff main origin/ccwork/variadic-allocations`):

```
 goc/alloccount_test.go                  |  173 ++      (new)
 goc/testdata/allocation_counts.go       |  145 ++      (new)
 goc/compile.go                          |   38 +-
 opt/escape.go                           |   47 +-
 opt/escapefacts.go                      |   22 +-
 opt/escapegraph.go                      |   76 +-
 opt/escapesummary.go                    |   77 +-
 goc/testdata/alloc_census_baseline.txt  | 4923 +-      (regenerated)
 goc/testdata/escape_shadow_baseline.txt |  253 +-      (regenerated)
 goc/testdata/escape_gc_differential.txt |  284 +-      (regenerated)
```

`goc/testdata/frame_escape_baseline.txt` is **not** in that diff. The
`TestFrameEscapeAudit` ratchet is therefore unmodified by the branch: a pass
means zero new publications *and* zero vanished ones, checked against main's own
file.

Environment: `aarch64` Linux, host toolchain `go1.26.1`, 64 cores. Both
worktrees built clean (`go build ./...`).

---

## Suite results

### 4. `make test-unit` — PASS (both)

Run as `go test -count=1 -json <UNIT_PKGS>` (the `-json` form of the Makefile
target; `-count=1` so nothing could come back `(cached)` — zero `(cached)`
markers in either log).

| | PASS | FAIL | SKIP |
|---|---|---|---|
| branch `034e58c` | **1598** | 0 | 339 |
| main `a535466` | **1598** | 0 | 339 |

Reconciliation with the quoted 1591: main at `a535466` also reports 1598, so the
+7 is not the branch. The `internal/gcdiff` package contributes exactly 7
passing tests and was added by `e235e32` ("goc: a differential harness comparing
goc's allocation placement against gc's") in the `e7c8e33..a535466` range.
1591 + 7 = 1598. The branch changes the unit census by **0**.

### 2. `make test-goc-status` — PASS (both), identical set

Run as the Makefile target with `-count=1 -v` so the per-capability set is
visible and nothing can be cached (`ok github.com/evanphx/cg12/cmd/goc 164.8s`
on the branch — a real run, not a cache hit).

| | PASS | FAIL | total |
|---|---|---|---|
| branch `034e58c` | **364** | 0 | 364 |
| main `a535466` | **364** | 0 | 364 |

The two 364-line subtest sets are **byte-identical** after sorting
(`diff` empty), so this is a set match and not merely a count match. Matches the
known-good 364/364.

### 3. `make test-goc-status-opt` — 363 PASS / 1 FAIL on **both**

| | PASS | FAIL | total |
|---|---|---|---|
| branch `034e58c` | 363 | 1 | 364 |
| main `a535466` | 363 | 1 | 364 |

The single failure on each side is the same one:

```
FAIL: TestARM64RuntimeCapabilityStatus/stack-scan/loop-safepoints
```

Sorted subtest sets are identical between branch and main. The pre-existing
failure was **not inherited from the task description** — the main control was
run here and reproduces it. The branch adds no `-O` capability failure and fixes
none.

### 1. `go test -timeout 40m -parallel 10 ./goc/...` — PASS (both)

Run with `-count=1 -json`. Zero `(cached)` markers in either log; branch wall
clock ~19 min, main ~19 min (they shared the box).

| | PASS | FAIL | SKIP | total |
|---|---|---|---|---|
| branch `034e58c` | **632** | 0 | 3 | 635 |
| main `a535466` | **628** | 0 | 3 | 631 |

(top-level only: branch 314 P / 3 S, main 312 P / 3 S)

The three skips are the same on both sides and are opt-in measurements, not
silent losses: `TestEscapeSummaryPromotionRate`, `TestEscapeDifferentialAgainstGC`
and `TestEscapeDifferentialProgram` all `t.Skip` unless their flag is passed.
`TestEscapeDifferentialAgainstGC` is run explicitly under §10 below.

**Reconciliation of the +4.** The exact test-name sets were diffed, not just the
counts. Nothing present on main is missing on the branch (the "only in main" set
is empty). The branch-only set is exactly:

```
TestAllocationCounts
TestAllocationCounts/allocation_counts.go
TestAllocationCounts/allocation_counts.go_-O
TestAllocationCountsAgainstTheHostToolchain
```

all four added by `fb7fb90` ("test: hold the allocation counts of the calls Go
programs make most often") in `goc/alloccount_test.go`. 628 + 4 = 632.

Reconciliation back to the quoted `e7c8e33` reference: `git diff e7c8e33 a535466
-- goc/` adds only `goc/gcdiff_test.go` and its testdata. That file's two tests
both skip by default, so `main@e7c8e33` would report the same 628 passes with 1
skip instead of 3. The passing census is unchanged from `e7c8e33` to `a535466`;
the whole `./goc/...` delta on this branch is the four new tests it ships.

### 7. Determinism — PASS (both)

`scripts/determinism-check.sh -corpus -rounds 2 -j 16`, which compiles every
`goc/testdata/*.go` twice through `goc compile-batch` and byte-compares the
linked images, separating a layout residue from a real difference in generated
code.

| | reproducible | varying | failed | compiles |
|---|---|---|---|---|
| branch `034e58c` | **390 / 390** | 0 | 0 | 780 |
| main `a535466` | **389 / 389** | 0 | 0 | 778 |

Main reproduces the quoted reference exactly (389/389 over 778). The branch's
+1 program is `goc/testdata/allocation_counts.go`, which the branch adds.
`content varies between rounds: 0` and `image varies, content identical (layout
only): 0` on both sides — so the branch's changed output is still bit-for-bit
reproducible.

### 5. `TestFrameEscapeAudit -count=1` — PASS

```
--- PASS: TestFrameEscapeAudit (187.45s)
ok  github.com/evanphx/cg12/goc  187.84s
```

`goc/testdata/frame_escape_baseline.txt` is untouched by the branch, and the
test is a two-way ratchet: it fails on any publication not already listed *and*
on any listed publication that has gone away. A pass against main's own file is
therefore the strong statement — **zero new publications and zero vanished
ones**, over the whole corpus, with the branch's placement changes in effect.

This is the hard requirement in the task and it is met. Note that the branch
does move real objects into frames (see §7's frame/heap table below: corpus-wide
`frame` placements go 37 → 70 and `heap` 2341 → 2313), so the audit was
genuinely exercised rather than trivially clean.

### 6. Allocation census against its regenerated baseline, twice — PASS, stable

Two independent `-count=1` runs of `TestAllocationCensus` against the branch's
regenerated `goc/testdata/alloc_census_baseline.txt`:

```
run 1:  --- PASS: TestAllocationCensus (187.45s)   ok ... 187.787s
run 2:  --- PASS: TestAllocationCensus (174.89s)   ok ... 175.318s
```

Two different wall clocks, so these are two real compilations of the whole
corpus, not one run and one cache hit. The census is an exact whole-file
comparison, so a pass means every one of the corpus's allocation sites landed
where the committed baseline says it does, twice — the placement is stable, not
merely correct once.

Direct check of §8's frame-slot requirement, read off that same baseline:
`loop_alias_frame_local.go` contributes **0** lines to the census on the branch
and **0** on main. The census records every heap allocation and every frame
allocation that came out of an escape decision, so zero lines for that file means
none of its three loop-body allocations were heaped — they are still ordinary
frame slots, exactly as on main. `loop_alias_forms.go`,
`loop_alias_composite.go` and `variadic_backing.go` together contribute 7 lines
on the branch and 7 on main, unchanged.

### 8. Loop-aliasing programs — PASS, all forms

```
--- PASS: TestLoopBodyAllocationsAreDistinctPerIteration (24.84s)
    --- PASS: .../loop_alias_forms.go            (3.53s)
    --- PASS: .../loop_alias_forms.go_-O         (3.34s)
    --- PASS: .../loop_alias_composite.go        (3.05s)
    --- PASS: .../loop_alias_composite.go_-O     (3.18s)
    --- PASS: .../variadic_backing.go            (2.84s)
    --- PASS: .../variadic_backing.go_-O         (3.03s)
    --- PASS: .../loop_alias_frame_local.go      (2.87s)
    --- PASS: .../loop_alias_frame_local.go_-O   (3.01s)
--- PASS: TestLoopAliasExpectationsMatchTheHostToolchain (0.23s)
    --- PASS: .../loop_alias_forms.go            (0.06s)
    --- PASS: .../loop_alias_composite.go        (0.06s)
    --- PASS: .../variadic_backing.go            (0.05s)
    --- PASS: .../loop_alias_frame_local.go      (0.06s)
```

All four forms in `loop_alias_forms.go` (`new(int)`, `make([]int,0,4)`,
`&a` on an array, run through the one program) and `loop_alias_composite.go`
still print what the language says, unoptimized *and* `-O`, and
`TestLoopAliasExpectationsMatchTheHostToolchain` re-derives those expectations
from `go run` on this host rather than trusting the committed literals.

`loop_alias_frame_local.go` still prints `framed: 18 / within: 12 / literal: 18`
and is still in the corpus, so `TestAllocationCensus` (§6) is what holds its
three loop-body allocations to frame slots — a regression there would show up as
a new `runtime.newobject` line for that file, and the census passes. The
per-iteration rule is undisturbed.

### 9. `runtime_gc_type_mask_padding.go`, 20× under `GOGC=10` — 0/20, both

Compiled with each side's own `goc` (`goc -o out goc/testdata/runtime_gc_type_mask_padding.go`;
the two executables differ byte-wise, as expected for a branch that changes
emitted code), then executed 20 times each:

| config | branch `034e58c` | main `a535466` |
|---|---|---|
| `GOGC=10` | **0/20 failed** | 0/20 failed |

The required 0/20 holds.

The `GOGC=10 GOMAXPROCS=3` configuration was also tested, because the branch
moves objects into frames and that is the config the task flags as already
unsound. It fails on **both** sides, and the branch is not the worse of the two:

| config | branch | main |
|---|---|---|
| `GOGC=10 GOMAXPROCS=3`, n=20 | 4/20 | 2/20 |
| `GOGC=10 GOMAXPROCS=3`, n=60 | **7/60** (11.7%) | **11/60** (18.3%) |

Every failure on both sides is `fatal error: found bad pointer in Go heap`.
(One 20-run branch sample also produced two `found pointer to free object`, which
did not recur in the 60-run sample; it is the same heap corruption surfacing at a
different check, not a distinct branch-only mode — the 60-run sample is the one
with enough draws to compare.) On the larger sample the branch fails *less*
often than main. This is the pre-existing GC unsoundness, attributed against a
main control run here rather than inherited from the task description, and the
branch neither introduces nor fixes it.

### 10. Branch allocation-count test and gc differential

**The branch's own regression test — PASS:**

```
--- PASS: TestAllocationCounts (14.73s)
    --- PASS: TestAllocationCounts/allocation_counts.go     (7.51s)
    --- PASS: TestAllocationCounts/allocation_counts.go_-O  (7.22s)
--- PASS: TestAllocationCountsAgainstTheHostToolchain (0.08s)
```

**The improvement, measured independently.** Rather than take the branch's table
on trust, `goc/testdata/allocation_counts.go` was compiled and run with *main's*
`goc` as well (a scratch probe in the main worktree, since main has no such
test; removed afterwards, worktree left clean). Both columns below are numbers a
process printed in this job. Units are allocations per call × 100.

| row | main `a535466` | branch `034e58c` | host go1.26.1 |
|---|---|---|---|
| `sprintf_int` | **300** | **200** | 100 |
| `sprintf_string` | 300 | 200 | 200 |
| `sprintf_struct` | 300 | 200 | 200 |
| `sprintf_no_args` | 200 | 100 | 100 |
| `variadic_ints` | 100 | **0** | 0 |
| `variadic_any` | 100 | **0** | 0 |
| `box_small_int` | 100 | 100 | 0 |
| `box_pointer` | 0 | 0 | 0 |
| `return_any_from_int` | 200 | 100 | 0 |
| `return_any_from_pointer` | 100 | **0** | 0 |
| `sync_pool_round_trip` | 100 | **0** | 0 |
| `sprintf_in_loop` | 300 | 200 | 100 |
| `variadic_ints_in_loop` | 100 | 100 | 0 |

The headline claim checks out: `fmt.Sprintf("value=%d", 42)` cost **3.00**
allocations under main's goc and costs **2.00** under the branch, against **1.00**
on the host. So the improvement is real and independently reproduced, and it is
**partial, not a closure** — the residual 1.00 is `box_small_int`, which the host
serves out of `runtime.staticuint64s` and goc has no equivalent for. The branch's
own comments say exactly this; the numbers agree with them. No row regressed;
nine of thirteen improved. `variadic_ints` and `variadic_any` reach host parity
(the `...` backing array becomes a frame slot), which is the change the branch is
named for.

**The gc differential harness — PASS on the branch, and it fails on main:**

| | corpus | compared | census rows | gc decisions | permissive | pessimistic | result |
|---|---|---|---|---|---|---|---|
| branch `034e58c` | 390 | 386 | 2706 | 3388 | 209 | **563** | PASS |
| main `a535466` | 389 | 385 | 2677 | 3366 | 202 | 586 | **FAIL** |

Main's failure is **pre-existing and not the branch's doing.** `main` is a merge
(`a535466`) of `origin/ccwork/escape-gc-differential` whose committed
`goc/testdata/escape_gc_differential.txt` records `corpus programs 385`, while
main's corpus is now 389: the other parent of the merge added four corpus
programs after the baseline was recorded, and nothing regenerated it. Regenerating
on a clean main worktree produces a 39-line diff whose entire content is the
corpus-size counters and the row totals that follow from them. Verified on a
clean checkout after removing the probe files, so it is not contamination from
this job's own measurements. The branch regenerates the file (`348e87f`,
`f9decd3`) and therefore passes.

Reading the differential itself, main-regenerated vs branch-committed:

```
                frame    heap   mixed  absent   total
  main             37    2341      64     596    3038
  branch           70    2313      72     599    3054
```

Pessimistic lines (goc heaps where gc does not — pure cost) fall **586 → 563**.
Permissive lines (goc keeps in a frame where gc heaps — the direction that can be
*unsound*) rise **202 → 209**. That rise is the thing worth naming: it is the
branch doing what it set out to do, and the check on it is §5's
`TestFrameEscapeAudit`, which passes with an unmodified baseline, plus the corpus
actually executing (§1: 632/0, §2: 364/364).

---

## Summary

Every suite in the brief ran to completion and was waited on. Nothing is
UNVERIFIED.

| # | suite | branch `034e58c` | main `a535466` control |
|---|---|---|---|
| 1 | `go test -timeout 40m -parallel 10 ./goc/...` | 632 P / 0 F / 3 S | 628 P / 0 F / 3 S |
| 2 | `make test-goc-status` | 364 PASS, 0 FAIL | 364 PASS, 0 FAIL (identical set) |
| 3 | `make test-goc-status-opt` | 363 P / 1 F | 363 P / 1 F (identical set) |
| 4 | `make test-unit` | 1598 P / 0 F | 1598 P / 0 F |
| 5 | `TestFrameEscapeAudit -count=1` | PASS | (baseline unchanged) |
| 6 | allocation census ×2 | PASS, PASS | — |
| 7 | determinism, 2 rounds | 390/390 over 780 | 389/389 over 778 |
| 8 | loop-aliasing programs | 12/12 PASS | — |
| 9 | GC reducer 20× `GOGC=10` | 0/20 | 0/20 |
| 10 | alloc-count test + gc differential | PASS / PASS | — / **FAIL (pre-existing)** |

Every count above came out of a process run in this job. `-count=1` everywhere;
zero `(cached)` markers in any `-json` log.

**Three differences from the numbers quoted in the brief, all reconciled to a
commit rather than accepted:**

1. `test-unit` reads 1598, not 1591. Main reads 1598 too. `internal/gcdiff`,
   added by `e235e32` between `e7c8e33` and `a535466`, contributes exactly 7.
2. `./goc/...` reads 632 on the branch against 628 on main. The name-set diff is
   exactly the four tests `fb7fb90` adds; the "present on main, missing on the
   branch" set is empty.
3. Determinism reads 390/390 over 780 compiles, not 389/389 over 778. Main
   reproduces 389/389 over 778 exactly; the +1 program is the branch's own
   `allocation_counts.go`.

**Two failures observed, both pre-existing and both attributed by running main
here rather than by inheriting the claim:**

- `test-goc-status-opt` / `stack-scan/loop-safepoints` fails identically on main.
- `TestEscapeDifferentialAgainstGC` fails on a clean main worktree because
  `a535466`'s merge left the committed differential recording a 385-program
  corpus against main's current 389. The branch regenerates that file and passes.

**One thing to have an opinion about, not a failure.** The branch really does
move objects into frames — corpus-wide `frame` placements go 37 → 70, `heap`
goes 2341 → 2313 — and permissive lines in the gc differential (goc frames what
gc heaps, the direction that can be unsound) rise 202 → 209. That is the change
working as intended, and the instruments aimed at it all held:
`TestFrameEscapeAudit` passes against an **unmodified** baseline in both
directions, the corpus executes clean (632/0), the capability matrix is
set-identical at 364/364, and the GC reducer is 0/20 at `GOGC=10` and *less*
flaky than main at the known-broken `GOMAXPROCS=3` (7/60 vs 11/60).

The headline claim was reproduced independently against main's compiler rather
than read off the branch's table: `fmt.Sprintf("value=%d", 42)` costs 3.00
allocations on main and 2.00 on the branch, host 1.00. The improvement is real
and partial; the residual is `runtime.staticuint64s`, which goc does not have.

Method notes: two `git worktree`s (`wt-branch` at `034e58c`, `wt-main` at
`a535466`), both built clean, suites run detached and each waited on to exit.
Two scratch probe files were written into the worktrees to measure main's
allocation counts and were removed; `git status --porcelain` is empty in both,
and the gc-differential failure on main was re-confirmed on a clean tree
afterwards.

---

**SAFE TO MERGE TO MAIN.**

No suite regressed against a main control run in this job. Both failures seen
are reproducible on main without the branch, and one of them (the stale gc
differential baseline) the branch actually repairs.

