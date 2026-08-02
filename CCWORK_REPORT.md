# Test-suite verification of `ccwork/escape-summaries-land` (`4b2a6fd`)

Job: run the suites to completion against `origin/ccwork/escape-summaries-land`
and report. No fixes, no refactors — diagnose only.
(The previous contents of this file were `ccwork/merge-verify`'s report; it is
unchanged on that branch and in history at `8f29b3f:CCWORK_REPORT.md`.)

- Branch under test: `origin/ccwork/escape-summaries-land` @ `4b2a6fd`,
  checked out into a worktree at `…/escape-summaries-land-verify/land`.
- Reference: `main` @ `05946f2`, worktree at `…/escape-summaries-land-verify/mainref`.
  `git merge-base` of the two is `05946f2` — i.e. the branch is 18 commits
  ahead of the current `main` and contains no `main` commits the reference lacks.
  (The prompt named `ad4e9b2` as the merge base; that was an *older* `main`.
  Everything here is attributed against `main` @ `05946f2`.)
- Host: linux/arm64, 64 cores, `go1.26.1`.
- Every run below is `-count=1`, so nothing can come from `go test`'s result
  cache; every log was checked for `(cached)` regardless.

Sections are written as each suite exits. Anything not run is marked
**UNVERIFIED**.

## Summary — all 8 items run to completion, nothing UNVERIFIED

| # | suite | result |
| --- | --- | --- |
| 1 | `go test -timeout 40m -parallel 10 ./goc/...` | **615 RUN / 614 PASS / 0 FAIL / 1 SKIP**, exit 0, 1024s. `main` = 601/601/0/0. +14 reconciled by name against the commits that add them. |
| 2 | `make test-goc-status` | **PASS set = all 364; FAIL set = ∅**, exit 0 |
| 3 | `make test-goc-status-opt` | **FAIL set = {`stack-scan/loop-safepoints`}**, 363 PASS. `main` control: the identical one-element FAIL set. Not grown. |
| 4 | `make test-unit` | **1591 PASS / 0 FAIL / 339 SKIP**, exit 0. `main` = 1556/0/339; +35 reconciled by name. |
| 5 | `TestFrameEscapeAudit -count=1` | **PASS**, 172s, against a baseline byte-identical to `main`'s: **0 new publications, 0 vanished** |
| 6 | allocation census ×2 | **PASS, PASS** (173.2s, 165.2s), against the baseline `7897325` regenerated |
| 7 | determinism, 2 compiles, byte-compare | **reproducible=385 varying=0 failed=0**, 770 compiles |
| 8 | reducer 20× `GOGC=10` | **0/20 failed** (and 0/20 again on a repeat). `GOMAXPROCS=3` extra: branch 2/20 vs **`main` 5/20** — pre-existing, worse on `main`. |

Every number above was read out of a log written by a process this job started
and waited on to exit. No suite was left running. No log contains `(cached)`;
every run carried `-count=1`. Durations are consistent with real work (the
corpus compiles take 165–175s each; `./goc/...` took 17 minutes).

---

## 4. `make test-unit` — **PASS**

Run as `GOFLAGS="-v -count=1" make test-unit` in the branch worktree.

| | branch `4b2a6fd` | `main` `05946f2` |
| --- | --- | --- |
| exit status | 0 | 0 |
| `=== RUN` | 1930 | 1895 |
| `--- PASS` | 1591 | 1556 |
| `--- FAIL` | **0** | **0** |
| `--- SKIP` | 339 | 339 |
| `(cached)` lines | 0 | 0 |

`main` reproduces the prompt's reference exactly (1556 PASS / 0 FAIL). The
branch is +35 PASS, +35 RUN, ±0 SKIP.

**The +35 reconciled by name.** Diffing the `--- PASS` name sets: 35 names are
new on the branch, **0 names disappeared**. Every new name belongs to a
test file the branch's own commits add or extend:

- 8 `TestAllocationCensus*` + `TestAllocationSiteExcludesPlacement` +
  `TestAllocationTypeNameStripsPrefixAndDigest` (10) — `opt/alloccensus_test.go`,
  added by `5041f9a` "census where every allocation lands", extended by `e59e32f`.
- 19 `TestEscapeFacts*` — the cross-function summary unit tests, `700916f`
  "cross-function escape summaries on the IR" and `1c056cf` "make
  `ParamNoEscape` mean one thing".
- 6 `TestLowerHeapAllocations*` — the promotion-side tests, including
  `…RefusesToPromoteInsideALoop` / `…PromotesBesideALoop` (the loop guard from
  `68ab078`) and `…AllowsAWriteBarrieredLocalSlot` /
  `…TracksEscapeThroughAWriteBarrieredLocalSlot` (the write-barrier fix in
  `1c056cf`) and `…EscapesACandidateInASlotHandedToANoescapeCallee`.

Nothing unexplained in either direction.

---

## 2. `make test-goc-status` (default arm) — **PASS, 364/364**

`GOFLAGS="-v -count=1" make test-goc-status` in the branch worktree.
`ok github.com/evanphx/cg12/cmd/goc 158.414s`, exit 0, no `(cached)`.

**The set, not a count:**

- capability subtests run: **364** (365 `=== RUN` lines = 1 parent + 364 subtests)
- **PASS set: all 364.**
- **FAIL set: empty.**
- SKIP: 0. `KNOWN GAP`: 0.
- Harness verdict lines: 363 `PASS <program>.go` + **1 `EXPECTED FAILURE`**,
  which is `defer-panic/panic-string-output` (`runtime_panic_print_string.go`),
  the long-standing by-design exit-2 program. It is counted `--- PASS` by the
  harness because failing is its expectation.

This matches the "known good: 364/364" reference exactly.

---

## 3. `make test-goc-status-opt` (`-O` arm) — **1 FAIL, the known one, NOT grown**

`GOFLAGS="-v -count=1" make test-goc-status-opt` in the branch worktree.
`FAIL github.com/evanphx/cg12/cmd/goc 162.350s`, exit 2 (make Error 1), no `(cached)`.

- capability subtests run: **364** — and the 364 capability *names* are
  byte-identical to the default arm's set (`diff` of the two sorted name lists
  is empty), so the arm is not silently running a different or smaller matrix.
- **FAIL set: exactly one — `stack-scan/loop-safepoints`.**
- **PASS set: the other 363.**
- SKIP: 0. `KNOWN GAP`: 0. 1 `EXPECTED FAILURE`
  (`defer-panic/panic-string-output`), same as the default arm.

The failure is the known pre-existing one, and its signature is the
pre-existing signature — the precise **stack** scan, not anything the summaries
touch:

```
runtime_stack_scan_loop_safepoints.go should pass: exit status 2
  cg12scanroots: main_carried local slot 27 … retains … head 0x7272616300000062
  collected while live: carried-0 at carried before rewrite
  panic: a stack slot live across a loop back edge was not a GC root
```

i.e. `main_drain` ← `main_carried` ← `main_main`, the `cg12scanroots`
loop-carried-root loss recorded in `RUNTIME_PLAN.md` as an open `-O` defect.

**It has not grown: one failure, and it is that one.**

**Control, same target on `main` @ `05946f2`, same box, same hour** (the prompt
cited `ad4e9b2`; `main` has since moved, so the control was re-measured against
the real merge base):

| | branch `4b2a6fd` | `main` `05946f2` |
| --- | --- | --- |
| capability subtests | 364 | 364 |
| PASS | 363 | 363 |
| FAIL set | `{stack-scan/loop-safepoints}` | `{stack-scan/loop-safepoints}` |
| exit | 2 | 2 |

Identical FAIL sets. The `-O` arm's one failure is pre-existing and the branch
neither adds to it nor fixes it.

---

## 1. `go test -timeout 40m -parallel 10 ./goc/...` — **PASS, and the census reconciles**

Run as `go test -v -count=1 -timeout 40m -parallel 10 ./goc/...` (`-v` is what
makes a subtest census possible; `-count=1` defeats the result cache). Run on
both trees concurrently on the same box.

| | branch `4b2a6fd` | `main` `05946f2` |
| --- | --- | --- |
| exit status | 0 | 0 |
| `=== RUN` | **615** | **601** |
| `--- PASS` | 614 | 601 |
| `--- FAIL` | **0** | **0** |
| `--- SKIP` | 1 | 0 |
| wall clock | `ok …/goc 1024.375s` | `ok …/goc 959.098s` |
| `(cached)` | 0 | 0 |

`main` reproduces the prompt's 601 reference exactly. The branch is **+14 RUN**.

**The +14 reconciled by name, not accepted as a number.** Diffing the sorted
`--- PASS` name sets: **13 names new on the branch, 0 names lost.** The 14th is
the one new `--- SKIP`:

| new subtest | file | commit that added the file |
| --- | --- | --- |
| `TestAllocationCensus` | `goc/alloccensus_test.go` | `5041f9a` census + baseline |
| `TestCompareAllocationCensusNamesTheDirection` | " | `5041f9a` |
| `TestCompareAllocationCensusReportsASplitSite` + its 5 subtests (`frame_gains_a_heap_copy`, `heap_gains_a_frame_copy`, `split_is_unchanged`, `split_loses_its_frame_copy`, `split_loses_its_heap_copy`) | " | `e59e32f` "file a split site's move" |
| `TestEscapeShadowPlacement` | `goc/escapesummary_test.go` | `700916f` summaries + shadow diff |
| `TestEscapeSummaryFacts` | " | `700916f` |
| `TestEscapeSummaryCost` | " | `700916f` |
| `TestEscapeSummaryPointerPassedInsideAStructEscapes` | `goc/escapesummary_reduction_test.go` | `77ed65b` the two safety reductions |
| `TestEscapeSummarySliceBackingBarrieredIntoItsHeaderIsStillTracked` | " | `77ed65b` |

That is 13 PASS. The remaining RUN is **`TestEscapeSummaryPromotionRate`, which
skipped**, by design and not silently — it prints
`pass -escape-promotion-rate to compile the corpus twice and measure`. It is a
measurement harness, not an assertion, and it is opt-in. Nothing else skips on
either tree.

601 + 13 PASS + 1 SKIP = 615. Fully accounted for, and **no test that ran on
`main` stopped running or stopped passing on the branch.**

Note that `TestFrameEscapeAudit` and `TestAllocationCensus` both ran inside this
suite and passed here as well; they are re-run in isolation in §5 and §6.

---

## 7. Determinism — **PASS, 385/385 reproducible**

`scripts/determinism-check.sh -corpus -rounds 2 -j 12` on the branch worktree —
i.e. every capability/corpus program in `goc/testdata/*.go` compiled twice with
this branch's `goc` and the linked images byte-compared. Exit 0.

```
programs=385 rounds=2 workers=12 optimize=false pack=""
round 0: 385 programs in 206.7s, 0 failed
round 1: 385 programs in 173.7s, 0 failed
failed to compile: 0
content varies between rounds: 0
image varies, content identical (layout only): 0
reproducible=385 varying=0 failed=0 of 385 over 2 rounds
```

770 compiles, **zero varying, zero layout-only residue, zero compile failures**.
The point of the check on this branch: the emitted code is *expected* to differ
from `main` (that is what the branch does), but it must be a function of the
input alone. It is.

---

## 5. `TestFrameEscapeAudit`, `-count=1`, in isolation — **PASS, zero new publications**

```
$ go test -v -count=1 -timeout 40m -run '^TestFrameEscapeAudit$' ./goc/
=== RUN   TestFrameEscapeAudit
--- PASS: TestFrameEscapeAudit (172.36s)
ok  github.com/evanphx/cg12/goc  172.888s
```

exit 0, no `(cached)`, 172s of real corpus compilation.

**Why this PASS is the load-bearing one, and why it is not vacuous.** The audit
is a two-sided ratchet against `goc/testdata/frame_escape_baseline.txt`: it
fails on a publication that is not listed *and* on a listed publication that has
gone away. So the result only means "zero new publications" if the branch did
not move the goalposts. It did not:

```
$ git diff --stat main..HEAD -- goc/testdata/frame_escape_baseline.txt
(no output — byte-identical to main)
$ git log --oneline main..HEAD -- goc/testdata/frame_escape_baseline.txt
(no commits — untouched by the branch)
```

The baseline is the same 193 entries `main` carries, and the branch passes
against it unchanged. **Zero appeared, zero vanished** — with ~211 allocation
sites newly promoted into frames (see §6), the set of frame addresses this tree
publishes past their frame is exactly `main`'s set. That is the requirement the
job stated as hard, and it is met.

---

## 8. `runtime_gc_type_mask_padding.go` reducer, 20× at `GOGC=10` — **PASS, 0/20**

Built from the branch tree: `go build -o goc ./cmd/goc`, then
`goc -o reducer goc/testdata/runtime_gc_type_mask_padding.go`, on an idle box
(load avg 2.5 at start; every other suite had exited).

```
=== RESULT branch GOGC=10, default GOMAXPROCS: 0/20 failed, 20/20 passed ===
$ GOGC=10 ./reducer
type mask padding ok
```

**0/20 in the prescribed configuration.** The reducer source is byte-identical
on both trees (`git diff main..HEAD -- goc/testdata/runtime_gc_type_mask_padding.go`
is empty); only the emitted image differs, which is the point of the branch.
**The `GOMAXPROCS=3` variant, attributed against `main` as the job required.**
Same box, same minute, sequential, load avg ~5, 20 runs per cell:

| tree | `GOGC=10`, default `GOMAXPROCS` | `GOGC=10 GOMAXPROCS=3` |
| --- | ---: | ---: |
| branch `4b2a6fd` | **0/20 failed** (twice: 0/20 and 0/20) | 2/20 failed |
| `main` `05946f2` | **0/20 failed** | **5/20 failed** |

The `GOMAXPROCS=3` failures are the pre-existing GC unsoundness, and they are
**worse on `main` than on the branch** — exactly what the job's note said to
expect, and what `RUNTIME_PLAN.md` §"What this does not fix" records (the
precise stack scan at low `GOGC`, an open defect on both trees). The signature
is the pre-existing one, not a frame address:

```
runtime: marked free object in span 0x…, elemsize=256 freeindex=7
```

Nothing here is chargeable to this branch; in the prescribed configuration the
requirement (0/20) is met, and in the extra configuration the branch is
strictly better than the tree it merges into.

---

## 6. Allocation census against the regenerated baseline, twice — **PASS, PASS**

Two independent invocations, each `-count=1`, each a full corpus compile:

| run | result | wall clock |
| --- | --- | --- |
| 1 | `--- PASS: TestAllocationCensus`, exit 0 | 173.20s |
| 2 | `--- PASS: TestAllocationCensus`, exit 0 | 165.23s |

No `(cached)` in either log; the 165–173s each is the corpus being compiled,
not a cache hit. Stable across the two runs.

The test is a four-way ratchet against
`goc/testdata/alloc_census_baseline.txt` — it fails on a site that moved
heap→frame, on one that moved frame→heap, on a site that appeared, and on one
that vanished. The baseline was regenerated on this branch by `7897325`
("regenerate the census and shadow baselines for the summaries being on"), so
the PASS means the emitted placement matches that regenerated file exactly.

**What the regenerated baseline actually changed** (`git show 7897325 --
goc/testdata/alloc_census_baseline.txt`): 231 lines removed / 212 added; of
those, **211 sites flipped `heap` → `frame`** and 20 flipped the other way
(the branch's own report attributes the 20 to the loop rule and to split-site
effects). The prompt's "~2 981 allocations move from heap to frame" is the
*object*-level count of the same movement over 385 corpus programs; the branch's
report records it as +2 947 objects, and notes explicitly that "§8.1's +2 947
objects and this 211 are the same movement counted two ways". Site-level 211 is
what I could verify directly from the baseline diff; I did not independently
re-derive the object-level figure, and nothing in the suites depends on it.

Read with §5: 211 sites moved into frames and the set of published frame
addresses did not grow by one.

---

## What this run does and does not establish

**Does.** The branch changes what the compiler emits — 211 corpus allocation
sites move off the heap and into frames — and after that change:

- every test that passed on `main` still runs and still passes (0 lost names in
  both `./goc/...` and `test-unit`);
- the frame-escape audit, the check that exists precisely because a wrong
  "does not escape" is silent at compile time, finds **no publication `main`
  did not already make**, against a baseline this branch did not touch;
- the capability matrix is 364/364 on the default arm and unchanged on the
  `-O` arm, where the single failure is the known `cg12scanroots` loop-carried
  root loss, reproduced identically on `main` in the same hour;
- the new emission is reproducible: 385/385 programs byte-identical across two
  compiles;
- the GC reducer is clean in the prescribed configuration, and in the harsher
  `GOMAXPROCS=3` configuration the branch fails *less* than `main`.

**Does not.**

- The `-O` arm's `stack-scan/loop-safepoints` failure is *not fixed*; it merges
  in as-is. It is pre-existing and measured on `main`, so it is not a reason to
  block, but the `-O` configuration ships with a known GC root loss either way.
- The `GOGC=10 GOMAXPROCS=3` GC unsoundness likewise remains open on both trees
  (branch 2/20, `main` 5/20). Neither figure is 0.
- `TestEscapeSummaryPromotionRate` is opt-in and skipped in a default run, so
  the promotion-rate figures in the branch's own report are not re-measured
  here. That test asserts nothing; it prints.
- I verified the census movement at **site** level (211 heap→frame from the
  baseline diff). The object-level "≈2 981" from the brief was not
  independently re-derived; the branch's report gives +2 947 for the same
  movement, and no suite depends on the number.
- Not run, because not asked for: `make test-goc-corpus` as its own target
  (it is `./goc/...`, covered by item 1), `test-goc-cmd`, `test-ruby`,
  `test-goc-coverage`, `go vet`, and the `-O` determinism arm.

---

# VERDICT: SAFE TO MERGE TO MAIN

All eight suites ran to completion and were waited on. The only failing subtest
anywhere is `stack-scan/loop-safepoints` on the `-O` arm, which fails
identically on `main` @ `05946f2` — measured, not assumed. Every other result is
green, and the two results that matter most for a branch that changes emission
are unambiguous: **zero new frame-address publications** against an untouched
baseline, and **385/385 byte-reproducible compiles**.
