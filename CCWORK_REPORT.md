# VERDICT: the assignment work is recoverable, and one line of it was the problem

`ccwork/rangekey-b` is right about Go's assignment order and wrong about what cg12 can
hold. It resolves every destination before the right-hand side, as the specification
requires, and that makes a **raw address live across every call the right-hand side
makes**. `ir.Block.Add(ir.ClsP, ...)` — how every field, index and indirection address is
built — produces a pointer-width value that is *not* a managed reference, so the frame's
stack map omits it, `copystack` does not adjust it, and a right-hand side that grew the
goroutine stack left the destination pointing into the abandoned old stack. **The store
landed in freed memory and the assignment vanished with no fault of any kind.** That is why
the failures were four `encoding/gob` crashes, a `reflect` segfault, and a TLS stream that
corrupted itself rather than crashing.

Marking the prepared address managed fixes all six. The rest of the branch — nine classes of
wrong answer in assignment destinations, verified 9/9 against the host toolchain — lands
unchanged.

**What is on `ccwork/wave4-rescue`, six commits on `61b96da`:**

| | |
| --- | --- |
| `040fcbd` | `ccwork/rangekey-b` (`1218627`), cherry-picked verbatim — it applies to current `main` with **zero conflicts** |
| `702a4e9` | the fix: keep a prepared assignment destination in the stack map |
| `c83be4f` | `ccwork/freeobject` (`b46b82c`), which §5.11 and §5.12 describe and which never reached `main` either |
| `a8c9ade` | `RUNTIME_PLAN` §5.13's real story, §5.10's general form of the hazard, §5.14 restored and re-measured |
| `8b46ae3` | a real defect the cherry-pick surfaced: `gc/goroutine-entry-stack-map` was not marked `exclusive` |
| `0f86181` | plan: which recovered claim was re-measured here and which was not |

**Headline numbers.** Matrix **342 subtests, 342 PASS, 341 declared PASS, 1 EXPECTED
FAILURE, 0 KNOWN GAP, 0 FAIL** (338 → 342 by addition only, four new capabilities, nothing
removed). `make test-unit`, `make test-goc-corpus` pass. Determinism unchanged. All 362
corpus programs compiled by `main` and by this branch and run: **355 identical, and all 7
differences accounted for**. `phase2-alloc` still cannot merge, re-measured rather than
assumed.

## Status

- [x] The six regressions reproduce, and they are caused by `1218627` alone.
- [x] Root cause identified, and confirmed by a positive experiment.
- [x] The correct part of the work landed on current `main`, with the defect fixed.
- [x] §5.11, §5.12 and §5.13 recovered — with `ccwork/freeobject`'s code, because two of
      the three describe it and it was not on `main`.
- [x] `phase2-alloc` × `freeobject` interaction re-checked against current `main`: it is
      still there, 40/40.
- [x] Full suite and matrix, on the branch as it stands.

### Not verified here — read §8 before relying on any of it

- `ccwork/freeobject`'s multi-thousand-run statistical campaigns (160 000 and 8 000 runs).
  Its reducer's rate was re-measured; its tables were not.
- The `phase2-alloc` × `freeobject` mechanism. It is re-measured as still present and is
  **not** explained; no guess is offered.
- `ccwork/baseline-accept` (`4e14a8f`), the fourth branch in `integration/wave4a`. It is
  `ccwork/coverage-baseline`'s area, so it is left alone.

## 1. Reproduction (done)

Two detached worktrees off this repo, `1218627` (`ccwork/rangekey-b`) and its parent
`ff6ef9e`, targeted capability run, three runs each:

```
go test -timeout 40m -v \
  -run 'TestARM64RuntimeCapabilityStatus/(stdlib-encoding|runtime-packages|stdlib-http)/(gob-int|gob-struct-int|gob-struct-mixed|gob-roundtrip|reflect-call-aggregate-probe|tls-client-server)$' \
  ./cmd/goc/... -args -runtime-status-runs=3
```

| capability | `ff6ef9e` (parent) | `1218627` (rangekey-b) |
| --- | --- | --- |
| `stdlib-encoding/gob-int` | PASS | FAIL |
| `stdlib-encoding/gob-struct-int` | PASS | FAIL |
| `stdlib-encoding/gob-struct-mixed` | PASS | FAIL |
| `stdlib-encoding/gob-roundtrip` | PASS | FAIL |
| `stdlib-http/tls-client-server` | PASS | FAIL |
| `runtime-packages/reflect-call-aggregate-probe` | PASS | FAIL |

Elapsed 255s (parent, all pass) and 248s (rangekey-b, all fail), so both runs really ran.
The verification verdict's claim is confirmed: `1218627` alone, not a merge interaction.

Failure shapes:

- `reflect-call-aggregate-probe`: `SIGSEGV addr=0x0` in
  `reflect.abiSeq.stepsForValue` ← `reflect.newAbiDesc` ← `reflect.funcLayout` ←
  `reflect.Value.call`.
- `stdlib-http/tls-client-server`: `local error: tls: bad record MAC` — silent stream
  corruption, no crash.

## 2. Root cause (found, and confirmed by a positive experiment)

**`ccwork/rangekey-b` is right about Go's assignment order, and cg12 cannot yet hold the
address it computes.**

### What the branch changed that matters

Go evaluates a statement's *operands* — index expressions and pointer indirections on the
left — before the right-hand side, and only then stores. `1218627` implements that: every
destination is resolved by `prepareAssignmentTarget` before any value is produced. For a
destination that is not a plain identifier, resolving means calling `g.lvalue`, which
computes a **raw address**, and that address is then live across the whole right-hand side —
including any call in it.

### The defect that exposes

`ir.Block.Add(ir.ClsP, …)` — how every field, index and indirection address is built —
produces a pointer-*width* value, not a *managed* one. `ir/pointer.go` documents the split:
`ClsM` (or an explicit `MarkGCRef`) is what makes a value a GC root and puts it in the
frame's stack map; plain `ClsP` does not. `Block.Load(ClsP, …)` marks its result, `Block.Add`
does not.

A value that is not in the stack map is not adjusted by `copystack`. So when the right-hand
side grows the goroutine stack, the prepared destination address keeps pointing into the
abandoned old stack, and the assignment's store lands in freed memory. The assignment is
**silently lost** — no fault, no bounds error, no barrier complaint.

Before this branch the window was always empty, because the destination address was computed
*after* the right-hand side. That was never a stated invariant; it was an accident of the old
code, and the branch's spec-correct reordering removes it.

### The exact statement, located

Bisected with a position filter on which destinations get prepared early
(`GOC_EXP_ONLY=<file:line>`), narrowing package → file → line:

```
reflect/abi.go:127:   a.valueStart = append(a.valueStart, pStart)
```

Preparing *only* that one destination early reproduces the crash; preparing every other
destination in the program early does not. Instrumenting `addArg` to print
`len(a.valueStart)` immediately after the statement shows **0** — the append ran, the store
vanished. `newAbiDesc` then indexes a zero-length `valueStart`, reads through its nil data
pointer, and takes `SIGSEGV addr=0x0` inside `stepsForValue`. `a` points at `newAbiDesc`'s
`var in abiSeq`, a *stack* local, which is why this destination and not another.

Corroborating: `GODEBUG=cg12checkstackcopy=1` makes the program pass deterministically
(3/3) while `madvdontneed`, `asyncpreemptoff`, `cg12checkwb=1`, `GOGC=off` and
`gcshrinkstackoff=1` all still fail — only the stack-copy knob perturbs it away.

### Confirmation

Marking the prepared address managed —

```go
slot: g.fn.MarkGCRef(g.lvalue(destination)),
```

— in `prepareAssignmentTarget`'s non-identifier case, changing nothing else, makes
`reflect-call-aggregate-probe` and `gob-int` pass. That is the positive test of the
mechanism: the address was invisible to the stack map, and making it visible fixes it.

### Bisect record

| variant | reflect probe |
| --- | --- |
| `1218627` as committed | FAIL |
| destinations prepared *after* the right-hand side | PASS |
| only *identifier* destinations prepared early | PASS |
| only *non-identifier* destinations prepared early | FAIL |
| as committed + `MarkGCRef` on the prepared address | PASS |

## 3. Landing strategy: cherry-pick, not redo

`git cherry-pick -n 1218627` onto current `main` applies with **zero conflicts** — `goc/compile.go`,
`cmd/goc/runtime_status_test.go` and `RUNTIME_PLAN.md` all auto-merge — and the result builds
and vets clean. `main`'s content-addressing and closure-naming work does not overlap the
assignment machinery. Re-doing 723 lines by hand against a reference would cost far more and
risk transcription errors, so the branch is cherry-picked and the defect fixed on top.

## 4. What landed, and what it was verified against

Branch `ccwork/wave4-rescue`, two commits on `61b96da` (current `main`):

- `040fcbd` — `ccwork/rangekey-b` (`1218627`) cherry-picked verbatim. Kept separate and
  deliberately still broken, so the defect and its fix are both visible in the history.
- `702a4e9` — `goc: keep a prepared assignment destination in the stack map`. Marks the
  prepared address, the map header and a pointer-classed map key managed in
  `prepareAssignmentTarget`. A scalar map key is left alone on purpose.

### Results

| check | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | pass, no failures |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 540.6s` |
| the six regressions + the three new capabilities, 3 runs each | 9/9 PASS |
| full capability matrix | **341 subtests, 341 PASS, 1 declared EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP**, 309.8s |

**The matrix count moves 338 → 341, by addition only.** `ccwork/rangekey-b` adds a new
`assignment-targets` category with three capabilities — `range-target-forms`,
`range-target-order`, `multi-assignment-forms`. The diff to `cmd/goc/runtime_status_test.go`
against `main` is +21 lines and removes nothing, so all 338 pre-existing subtests are still
present and still pass. The one expected failure is unchanged:
`EXPECTED FAILURE runtime_panic_print_string.go`.

The fix was also verified on the branch's own base: applied to `1218627` in its own worktree,
all six regressed capabilities pass 3/3 there too (245.7s). So it holds on both trees, not
just the port.

### `make test-goc-cmd`: two pre-existing flakes, neither caused by this work

`make test-goc-cmd` failed twice here, on two different tests, and both reproduce on
`main` without any of this branch's changes:

- `TestBatchCompilerSharesItsWorldAcrossPrograms` (`cmd/goc/batch_test.go:182`) asserts the
  third compile in a worker is faster than the first. It failed at 8.69s against 2.29s while
  the box carried a load average of 118 from sibling jobs; re-run on an idle box it reports
  `2.12s 1.57s 1.59s` and passes. It is a wall-clock assertion with no load guard.
- `TestBatchCompilesAgainstDifferentPacksMatchOneShotCompiles` asserts a batch-compiled
  program is byte-identical to the same program compiled alone. **On pristine `main`
  (`61b96da`) it fails 2 runs in 3.** On this branch it passed 3 runs in 3. This is §5.10's
  front-end nondeterminism reaching a byte-identity assertion; the differing bytes are
  addresses in the same symbols, which is exactly the residue §5.10 describes.

  This is in `ccwork/batch-reconcile`'s area, already on `main`, so it is reported rather
  than fixed here.

With those two skipped, the rest of `./cmd/goc/...` passes: `ok ... 173.6s`.

### Host-toolchain differential (RUNTIME_PLAN §3 step 2) and determinism

The three new programs, run under the host Go 1.26.1 toolchain and under `goc`:

| program | `goc` vs host | `-O` vs no `-O` |
| --- | --- | --- |
| `runtime_range_target_forms.go` | identical | identical |
| `runtime_range_target_order.go` | identical | identical |
| `runtime_assign_target_forms.go` | identical | identical |

`scripts/determinism-check.sh` is unchanged from the documented baseline — 4 of 5 sample
programs byte-identical cold against warm across two rounds, with
`runtime_defer_capture_allocs.go` the known §5.10 residue:

```
hello.go                            round1:identical  round2:identical
fmt_sprintf.go                      round1:identical  round2:identical
gc_struct.go                        round1:identical  round2:identical
runtime_cleanup_frame_retention.go  round1:identical  round2:identical
runtime_defer_capture_allocs.go     round1:DIFFERENT  round2:DIFFERENT
```

## 5. `ccwork/freeobject` recovered too, because §5.11 and §5.12 are its sections

The task asks for `RUNTIME_PLAN.md` §5.11, §5.12 and §5.13 back. Checking
`origin/integration/wave4`, which carries the reconciled numbering the wave4a merges
dropped, those are:

| section | branch | on `main`? |
| --- | --- | --- |
| §5.11 the entry stack map described a never-started goroutine's argument frame | `ccwork/freeobject` | **no** |
| §5.12 `unsafe.Pointer` stores lost their write barrier | `ccwork/freeobject` | **no** |
| §5.13 assignment destinations that were not identifiers | `ccwork/rangekey-b` | **no** |

So two of the three document code that is not on `main` at all — `b46b82c` never landed.
Restoring the prose alone would have described a compiler that does not exist, so
`ccwork/freeobject` is cherry-picked as well (`c83be4f`). Only `RUNTIME_PLAN.md`
conflicted; every code file auto-merged onto current `main` *and* onto the
assignment-destination work.

**Its central claim was re-verified here, not taken on trust.** `runtime_goroutine_entry_stack_map.go`
compiled at `-O` by the compiler immediately before that commit and by the compiler after
it, 100 runs each:

```
before: 92/100 failed
after:   0/100 failed
```

which is exactly the rate `b46b82c` reports. The full matrix on that tree is **342
subtests, 342 PASS, 0 FAIL** — the branch adds `gc/goroutine-entry-stack-map`.

What was *not* re-verified here: that branch's 160 000-run and 8 000-run statistical tables
for the `many-goroutines-gc` variant and the `checkmark` residual. Those are its own
measurements and stand or fall on its evidence, not on anything measured in this job.

## 6. `ccwork/phase2-alloc` × `ccwork/freeobject`: the interaction is still there

Re-checked against current `main` rather than assumed. `runtime_cleanup_basic.go`
(`gc/cleanup-basic`), 40 runs per tree:

| tree | `gc/cleanup-basic` |
| --- | --- |
| `main` + `phase2-alloc` alone | 0/40 failed |
| `main` + rangekey (fixed) + `phase2-alloc` | 0/40 failed |
| `main` + `freeobject` + `phase2-alloc` | **40/40 failed** |
| `main` + rangekey (fixed) + `freeobject` + `phase2-alloc` | **40/40 failed** |
| this branch (rangekey fixed + `freeobject`, no `phase2-alloc`) | passes, in the full matrix |

`fatal error: span has no free objects`, and one run segfaulted instead. So §5.14's finding
holds on current `main`, it is **`freeobject` × `phase2-alloc`**, and the assignment work is
not involved in it either way. `ccwork/phase2-alloc` stays out, for the same reason and now
with the reason re-measured. The mechanism is still not established and is not guessed at
here.

## 7. Final verification, on the branch as it stands

`ccwork/wave4-rescue`, four commits on `61b96da`:

```
a8c9ade plan: what the assignment work actually broke, and what still cannot be merged
c83be4f Fix two GC defects behind `found pointer to free object`      (ccwork/freeobject)
702a4e9 goc: keep a prepared assignment destination in the stack map  (this job's fix)
040fcbd goc: resolve every assignment destination before the right-hand side (ccwork/rangekey-b)
```

| check | result |
| --- | --- |
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `gofmt -l` over every changed non-vendored Go file | clean |
| `make test-unit` | pass |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 538.5s` |
| full capability matrix | see census below |
| `scripts/determinism-check.sh` | unchanged: 4 of 5 identical, `runtime_defer_capture_allocs.go` the known §5.10 residue |

### The matrix census, counted rather than trusted

Counted from a `-v -count=1` run of `^TestARM64RuntimeCapabilityStatus$`, 220.8s:

```
subtests:        342
--- PASS:        342
--- FAIL:          0
--- SKIP:          0
declared PASS:   341
EXPECTED FAILURE:  1
KNOWN GAP:         0
```

**The complete list of non-passing capabilities is: none.** The single declared exception is
`defer-panic/panic-string-output` (`EXPECTED FAILURE runtime_panic_print_string.go`), which
is the one `main` already carried.

The count is 342, not 338, and the difference is four additions with no removals:

| added capability | from |
| --- | --- |
| `assignment-targets/range-target-forms` | `ccwork/rangekey-b` |
| `assignment-targets/range-target-order` | `ccwork/rangekey-b` |
| `assignment-targets/multi-assignment-forms` | `ccwork/rangekey-b` |
| `gc/goroutine-entry-stack-map` | `ccwork/freeobject` |

`cmd/goc/testdata/runtime_coverage_baseline_pending.json` moves 44 → 48 entries and
`TestCheckedRuntimeCoverageBaselineDenominator` passes, so 294 + 48 = 342 reconciles.
`RUNTIME_PLAN.md` §1 is updated to match.

### The A/B behaviour differential over the whole corpus

A green matrix does not validate a codegen change, so every one of the 362 programs in
`goc/testdata` was compiled by `main`'s compiler and by this branch's, both were run, and
exit status, stdout and stderr compared. A disagreement is re-run three more times before it
is believed, because several corpus programs print scheduling-dependent statistics.

**355 of 362 identical. All 7 remaining are accounted for:**

| program | difference | why |
| --- | --- | --- |
| `runtime_range_target_forms` | 2 → 0 | the fix: `main` cannot compile a non-identifier range destination correctly |
| `runtime_range_target_order` | 2 → 0 | the fix |
| `runtime_assign_target_forms` | `main` fails to compile | `main` rejects `v, ok = m[k]` with a non-identifier destination outright |
| `runtime_goroutine_entry_stack_map` | 2 → 0 | §5.11's fix; `main` fails ~92 runs in 100 |
| `runtime_panic_print_string` | 2 → 2 | traceback offset only: `runtime_gopanic +0xc0c` against `+0xc10`, a 4-byte code-layout shift |
| `bytes_grow_stats` | printed statistics | not a compiler difference: **one executable gives 7 distinct outputs in 8 runs**, on both compilers, and the two compilers' output sets overlap. §5.10 names this program |
| `bytes_grow_compare` | printed statistics | same class; agreed with `main` in 2 of 3 reruns |

### One real defect the cherry-pick surfaced, and fixed

`TestRuntimeCapabilityExclusiveClassification` failed:
`gc/goroutine-entry-stack-map` sets its own `GOMAXPROCS` and `GOGC` — its own comment says
so — but was not marked `exclusive`. That test arrived with `main`'s concurrent run phase
and did not exist when `ccwork/freeobject` was written, which is exactly what a cherry-pick
across 82 commits should turn up. Fixed in `8b46ae3`; its sibling reducer
`gc/keepalive-stack-root` was already marked for the same reason.

With the matrix and the known-flaky `TestBatchCompilesAgainstDifferentPacksMatchOneShotCompiles`
skipped, `./cmd/goc/...` is `ok ... 173.1s`.

The final matrix, run again after `8b46ae3` changed how one capability is scheduled:

```
subtests:        342
--- PASS:        342
--- FAIL:          0
--- SKIP:          0
declared PASS:   341
EXPECTED FAILURE:  1
KNOWN GAP:         0
ok  github.com/evanphx/cg12/cmd/goc  193.8s
```

## 8. What is not verified, and what is left for someone else

### Not verified here

- **`ccwork/freeobject`'s statistical tables.** Its 160 000-run `many-goroutines-gc` campaign
  and its 8 000-run `checkmark` residual are that branch's own measurements, reproduced in
  `RUNTIME_PLAN` §5.11 and §5.12 as its evidence. What *was* re-measured here is its reducer:
  92/100 before, 0/100 after, on current `main`. §5.11 now says which is which.
- **The `phase2-alloc` × `freeobject` mechanism.** Re-measured as still present (40/40) and
  deliberately not explained. `span has no free objects` is an allocator invariant failure
  consistent with either reentry or an accounting disagreement, and guessing between them
  would be worse than saying so.
- **`ccwork/baseline-accept` (`4e14a8f`).** The fourth branch in `integration/wave4a`, and
  the one thing from that integration this job did not touch: it is
  `ccwork/coverage-baseline`'s area. `runtime_coverage_baseline_pending.json` here grows by
  the four capabilities this branch adds and nothing else.
- **The general interior-address hazard.** §5.10 now records it. The fix here covers the
  place the assignment machinery deliberately holds an address across a call; nothing
  prevents another interior address being held across a safepoint elsewhere in the front
  end. A cheap first step is named there: assert that no `ClsP`-classed value is live across
  a call instruction unless it is a GC reference, and see what the corpus reports.

### Defects found in other people's areas, reported rather than fixed

1. **`TestBatchCompilesAgainstDifferentPacksMatchOneShotCompiles` is flaky on `main`.**
   2 failures in 3 runs on pristine `61b96da`. It asserts byte-identity between a
   batch-compiled program and the same program compiled alone, which §5.10's front-end
   nondeterminism can break on its own. `ccwork/batch-reconcile`'s area.
2. **`TestBatchCompilerSharesItsWorldAcrossPrograms` has a bare wall-clock assertion.**
   It fails under load (8.69s against 2.29s at load average 118) and passes idle
   (`2.12s 1.57s 1.59s`). Same area. On a shared box this will keep firing.
3. **`println` with several operands prints no spaces**, which is visible in
   `bytes_grow_stats`'s output above (`mallocs246548302`). Already recorded in §5.10 and
   already `ccwork/println-spacing`'s job; noted only because it is what makes that
   program's output hard to read while diffing it.

### What the next job should pick up

- The interior-address checker described in §5.10. It is the cheapest way to find out
  whether §5.13 was the only place this bites.
- The `freeobject` × `phase2-alloc` reduction (§5.14). Both branches are preserved and
  both are individually verified; only the composition is broken.
