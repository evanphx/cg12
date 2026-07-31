# wave4-rescue: recovering `ccwork/rangekey-b`

*This file is written incrementally as results land. Anything not yet verified is
marked so explicitly.*

## Status

- [x] The six regressions reproduce, and they are caused by `1218627` alone.
- [ ] Root cause identified.
- [ ] Correct part of the work landed on current `main`.
- [ ] §5.11/§5.12/§5.13 recovered.
- [ ] `phase2-alloc` × `freeobject` interaction rechecked against current `main`.
- [ ] Full suite + matrix.

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
