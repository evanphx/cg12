# PLAN: Validate and complete the cg12 Go runtime

Bring the Linux/ARM64 runtime used by `goc` from "a broad corpus usually runs"
to a runtime whose core invariants are directly tested, whose known failures are
fixed, and whose remaining uncovered code is understood. Coverage guides the
work, but passing semantics and runtime invariants are the gates: executing a
line is not evidence that stack maps, write barriers, unwinding, or scheduler
state are correct.

This plan deliberately excludes runtime files for other operating systems and
architectures. Its source denominator is exactly the Go files selected by
`go/build` for Linux/ARM64 with the build tags used by `goc`. Selected Plan 9
assembly is tracked separately until it can be instrumented meaningfully.

## 1. Baseline

The accepted 2026-07-31 coverage run compiled and ran the whole 338-capability
matrix, executing each successful binary three times and merging its hits. Its
canonical baseline is `cmd/goc/testdata/runtime_coverage_linux_arm64.json`,
built from runtime source `06e314f8a5a729394207296e943ea075e51dbd39355c3389ca7ac65a52cb8f55`.

| Measurement | Baseline (2026-07-31) | Previous (2026-07-22) |
| --- | ---: | ---: |
| Programs | 338 | 294 |
| Programs returning coverage | 338 | 291 |
| Active Linux/ARM64 runtime Go functions | 2,574 | 2,561 |
| Compiled runtime functions | 2,087 | 2,050 |
| Executed runtime functions | 1,400 | 1,269 |
| Active-function coverage | 54.4% | 49.6% |
| Compiled runtime blocks | 27,780 | 25,929 |
| Executed runtime blocks | 9,182 | 7,936 |
| Compiled-block coverage | 33.1% | 30.6% |

The corpus denominator and the baseline are now the same set: 338 program rows
for 338 capabilities, every one `compile_outcome: passed` and
`coverage_outcome: collected`, with no `skipped`, `unreported`, `missing` or
`expected-unavailable` row anywhere. The single non-`passed` run outcome is
`defer-panic/panic-string-output`, the declared `expectedFailure`, which exits 2
by design and still returns its packet — §4.3's guard did not have to fire. The
run recorded 0 unexpected failures, 0 compile failures and 0 timeouts, against
the previous baseline's 19, 2 and 1.

Both figures remain short of §2's guideposts of 65% active-function and 45%
compiled-block coverage. They improved by 4.8 and 2.4 points over a corpus 15%
larger, which is progress and not arrival; the guideposts stay open.

The baseline already exercises useful portions of allocation, marking,
sweeping, stack growth, channels, timers, and netpoll.

Every capability failure this baseline exposed has since been closed. They are
listed here with their resolution because each one turned out to be a compiler
defect rather than a runtime defect, which is the single most useful pattern in
this document:

| Baseline failure | Resolution |
| --- | --- |
| defer/recover programs fail with `no deferreturn` | §5.1 |
| panic-time GC reports a non-stack address | §5.1 |
| signal notification crashes in atomics or runtime locking | §5.2 |
| signal delivery during GC times out | one bug with three other timeouts: §5.2.1 |
| finalizer resurrection does not run | §5.3 |
| cleanup callbacks never run | §5.3 |
| the runtime trace program OOMs or times out | §5.5 |
| the ECDSA case | generic method dispatch, §5.6 |

The rerun this section used to ask for has happened. Before it, the accepted
baseline covered 294 of the matrix's 338 capabilities and had been built from a
runtime source fingerprint that had since moved, so `make runtime-cover-diff`
refused the comparison outright — the drift guard working rather than a failure.
The 2026-07-31 baseline replaces it, and a diff against it now runs and reports
zero deltas.

### Current state (2026-07-31)

The capability matrix holds **341 capabilities and no `knownGap` at all**. The
only declared exception is `defer-panic/panic-string-output`, a deliberate
`expectedFailure`. Phase 1 (§5) is complete; §5.10 records what remains open,
none of which is a failing capability.

The count was 338 until §21 added three: `print-builtin/operand-separation`,
`print-builtin/statement-atomicity` and `core-types/complex64-parts`. Passages
below that quote 338 are records of runs made before that, and are left as they
were measured.

That is a statement about the matrix, not about the runtime. Per §2, executing a
line is not evidence that stack maps, write barriers, unwinding, or scheduler
state are correct, and the reviewed function inventory — not the matrix — is the
completion gate. Several of the bugs closed in §5.7 through §5.9 were invisible
to a green matrix and were found only by statistical repetition, by comparing
against the host toolchain, or by a diagnostic built for the purpose.

The 2026-07-22 baseline predates the sysmon segfault fix (`54e7c7e`), which was
the first change that let the capability matrix run to completion. Commit
`52757f9` then reclassified 13 always-failing capabilities as `knownGap`; six
were the net/http type-check gap and have since been repaired and restored to
`mustPass`. Four more — `goroutine/many-goroutines-gc`,
`scheduler-stress/gc-churn`, `stdlib-bytes/grow-allocs` and
`stdlib-signals/during-gc` — were one bug, the §5.2.1 GC-assist allocation
recursion, and became `mustPass` on 2026-07-28 together with that section's two
reducers. `runtime-packages/finalizer-resurrect`, the last remaining `knownGap`,
became `mustPass` on 2026-07-28 with the merged-address fix in §5.3, so the
matrix now holds no `knownGap` at all; `defer-panic/panic-string-output` is the
one deliberate `expectedFailure`.

The two denominators are now reconciled, and since 2026-07-31 they are equal.
There is only one denominator: the capability matrix *is* the coverage corpus,
and every capability reports one explicit compile/run/coverage outcome,
including the ones this environment cannot run. The accepted baseline covers all
338 that existed when it ran. Seven capabilities were added after it, so
`cmd/goc/testdata/runtime_coverage_baseline_pending.json` holds those seven and
`TestCheckedRuntimeCoverageBaselineDenominator` reconciles in both directions:
338 + 7 = 345, no capability may appear in both, and no baseline
program may name a capability the matrix has dropped. Adding a capability still
requires either accepting a new baseline or recording in that file why the
baseline does not cover it, so the list is a mechanism that is currently unused
rather than one that has been retired.

Control runs without coverage instrumentation reproduce the runtime failures
and large-program compiler memory failures. They are not coverage-counter
artifacts.

## 2. Definition of done

Runtime work is complete for the current Linux/ARM64 scope when all of the
following hold:

1. Every `must-pass` runtime capability passes repeatedly in normal and stress
   configurations. No capability crashes, hangs, or exceeds its memory budget.
2. Every active runtime Go function is classified as covered, intentionally
   unreachable in this configuration, failure-only, optional-feature code, or
   requiring an external integration environment. Classifications include a
   reason and, where applicable, a test that proves the configuration boundary.
3. Every correctness-critical function in allocation, GC, stack management,
   panic/unwind, scheduling, synchronization, timers, netpoll, and signals has a
   focused semantic test. Aggregate percentages are supporting evidence, not a
   substitute for this inventory.
4. Coverage is collected from every corpus program. A missing coverage packet
   is itself a failure unless the program intentionally terminates in a way the
   collector has explicitly classified.
5. The suite passes with at least `GOMAXPROCS=1`, `2`, and `4`, low `GOGC`,
   forced GC, repeated stack growth, and bounded stress runs.
6. The generated stack maps, pointer metadata, write barriers, call frames, and
   unwind metadata have compiler-emitted validation modes and no validation
   failures across the corpus.
7. Selected runtime assembly has either block/edge instrumentation or a
   scenario-based inventory proving each required entry point and important
   transition is exercised.
8. A clean full run produces a checked, diffable coverage report and does not
   regress the previous accepted baseline without an explicit explanation.

Initial quantitative guideposts are 65% active-function coverage and 45%
compiled-block coverage after the core phases, then 75% and 60% respectively
after breadth work. These are not hard completion gates because disabled
sanitizer/race stubs and fatal-only paths would make raw percentages misleading.
The reviewed function inventory is the final gate.

## 3. Working method for every gap

Each investigation follows the same ladder:

1. Add the smallest source program that demonstrates one semantic property.
2. Run the same program with the host Go toolchain and record its observable
   result. Avoid timing-dependent assertions where a synchronization event can
   be asserted instead.
3. Run the cg12 binary once normally, then with the relevant stress knobs.
4. If it fails, reduce it before changing the compiler. Identify whether the
   first bad state is introduced in parsing/type lowering, IR, ABI lowering,
   generated metadata, runtime assembly, or runtime execution.
5. Prefer an opt-in compiler-emitted invariant check over a debugger-only
   diagnosis. Keep generally useful checks as compiler test/debug modes.
6. Fix the lowest layer that violates Go semantics. Do not add runtime overlays
   to compensate for incorrect compiler behavior.
7. Run the focused test, its subsystem batch, the complete runtime status suite,
   and the normal `goc` tests under the established memory limit.
8. Generate a new coverage report and record newly covered and regressed
   functions/blocks. A test is retained even if it adds no coverage when it
   protects a distinct semantic invariant.

Use deterministic bounded loops for ordinary tests. Put longer repetition,
randomized scheduling, and high allocation pressure in an explicit stress
batch. Compilation and execution memory are reported separately so compiler
OOMs cannot be mistaken for runtime OOMs.

## 4. Coverage infrastructure backlog

Before using percentages as a progress metric, make the collector a dependable
regression tool.

Current M0 checkpoint:

- [x] Record a fingerprint of the exact build-selected runtime Go source.
- [x] Record compile, run, and coverage outcomes separately for every program.
- [x] Use stable source identities rather than executable-local counter IDs in
  aggregate reports.
- [x] Add a report comparison command with an optional regression exit status.
- [x] Add a reviewed classification format and leave unmatched functions
  explicitly `unknown`.
- [x] Add category summaries and separate compiler/program elapsed-time and
  peak-memory measurements.
- [x] Merge repeated executions of each compiled program so scheduling noise is
  less likely to produce false coverage regressions.
- [x] Produce and check in an accepted version-2 full-corpus baseline.
- [x] Make the capability matrix the single corpus denominator, recorded in each
  report as `matrix_capabilities` and enforced by the collector: a capability
  that reports no outcome is a collection failure, not a smaller corpus.
- [x] Give every program an explicit named outcome. A capability the environment
  cannot run is `skipped` with a recorded reason instead of being dropped; a
  deliberate abnormal termination is classified `expected-unavailable`; a
  capability that reported nothing at all is `unreported`; every other absent
  packet keeps a `missing` outcome and says how the process ended. Every outcome
  other than `collected` carries a reason, and the report is published even when
  the collector detected a hole, so the hole is diffable instead of costing the
  reviewer the whole run.
- [x] Keep the recorded outcome and the accumulated totals in agreement. A
  packet the collector discards — no runtime source ID, a source ID from a
  different copy of the runtime, or a repeat execution that lost its packet —
  is recorded as `missing` with the rejection as its reason rather than as
  collected coverage that never entered the totals.
- [x] Check capability identity, not just capability count. A duplicated program
  row is rejected by report validation, so it cannot cancel out a capability
  that never reported and leave the totals looking correct.
- [x] Reconcile the accepted baseline with the matrix in
  `testdata/runtime_coverage_baseline_pending.json`, checked by a test.
- [x] Refuse a sharded coverage run, which would publish a fraction of the
  corpus as a complete report, and refuse a coverage run on a host that cannot
  execute the matrix rather than skipping silently.
- [x] Reach one usable coverage outcome per capability. The 2026-07-31
  collection reached **338 of 338**: every capability compiled, ran, and returned
  a usable coverage packet, with no `skipped`, `unreported`, `missing` or
  `expected-unavailable` row. The three absences the previous run carried —
  `goroutine/many-goroutines-gc`, `scheduler-stress/gc-churn` and
  `stdlib-bytes/grow-allocs`, each killed at its 30s timeout — were one bug, the
  §5.2.1 GC-assist allocation recursion, and all three now collect.

  The one non-`passed` run outcome is `defer-panic/panic-string-output`, the
  matrix's declared `expectedFailure`. It exits 2 without recovering and its
  packet still arrives, because `insertRuntimeCoverageDumpCalls` emits the dump
  ahead of every `runtime.exit` and `fatalpanic` reaches `exit(2)`.

### 4.1 Make reports stable and comparable

Each checkbox below names the test that proves it. A claim without a test is
not ticked.

- [x] Add a report-diff command keyed by function, file, line, and block index.
  `TestCompareRuntimeCoverageReportsKeysFunctionsAndBlocksPrecisely` moves one
  function's line, then its file, then one block's index, and requires each to
  appear as a gain plus a loss rather than as the same entry.
- [x] Store the accepted Linux/ARM64 baseline in the repository without
  generated binaries or per-program counter blobs.
  `TestCheckedRuntimeCoverageBaseline` validates the checked-in file.
- [x] Report gained/lost functions and blocks, missing packets, compile
  failures, runtime failures, timeouts, and peak compile/run memory separately.
  The diff also lists added and removed programs, so comparing against a stale
  baseline announces the capabilities that baseline never ran instead of
  ignoring them. `TestCompareRuntimeCoverageReportsSeparatesProgramOutcomes`
  covers the compile-failure, timeout, added, and removed buckets;
  `TestCompareRuntimeCoverageReportsReportsANewRunFailureOnItsOwn` covers a run
  failure on its own; `TestRuntimeCoverageDiffCommandReportsEachOutcomeSeparately`
  requires one printed line per bucket and separate compile and run peak RSS, so
  a compiler OOM cannot be read as a runtime OOM.
- [x] Add per-category and per-subsystem summaries in addition to per-file
  totals. `TestRuntimeCorpusCoverageReportsCategoryResources`.
- [x] Detect source drift so a report from a different copied Go runtime cannot
  be compared silently. `TestCompareRuntimeCoverageReportsRejectsSourceDrift`
  and `TestRuntimeCoverageDiffCommandRejectsSourceDrift` refuse the comparison
  and print nothing; `TestRuntimeCorpusCoverageNamesADiscardedPacket` refuses a
  drifted packet inside a single collection run.
- [x] Make coverage collection opt-in for normal tests and provide a documented,
  resource-bounded command for the complete run: `make test-goc-coverage`, then
  `make runtime-cover-diff`.

### 4.2 Classify the denominator

Maintain machine-readable classifications for uncovered active functions:

- `required`: expected in supported ordinary Go programs;
- `rare-required`: failure, recovery, or unusual scheduler/GC path that needs a
  deliberate test;
- `configuration-disabled`: race, MSan, ASan, Valgrind, cgo, or another feature
  not enabled in this build;
- `fatal-only`: terminates the process and needs a subprocess assertion;
- `external-integration`: requires a foreign thread, profiler signal, plugin,
  or other explicitly provisioned environment;
- `unreachable`: proven dead for the selected build, with the reason recorded;
- `compiler-not-emitted`: active source that reachability did not compile;
- `unknown`: temporary triage state that must trend to zero.

Foreign-platform files never enter this inventory. Configuration-disabled
functions remain visible but do not become artificial coverage targets.

### 4.3 Cover abnormal termination and assembly

- Preserve coverage on `runtime.exit`, panic, throw, and fatal subprocess paths
  where it is safe to do so. This already holds for every abnormal termination
  the corpus currently produces: `insertRuntimeCoverageDumpCalls` emits the dump
  ahead of every call to `runtime.exit`, and Go's `fatalpanic` and `fatalthrow`
  both reach `exit(2)`. In the 2026-07-28 run the deliberate uncaught panic and
  all six programs that exited non-zero still returned their packets, and the
  accepted 2026-07-31 baseline repeats the result with nothing left to excuse:
  its only non-zero exit is `defer-panic/panic-string-output`, whose packet is
  `collected`, and the report carries zero `expected-unavailable` rows. So the
  `expected-unavailable` classification is a guard that did not have to fire
  rather than an active exclusion. It stays in place because terminations that
  bypass `runtime.exit` — `runtime.abort`, `dieFromSignal` re-raising a fatal
  signal — would otherwise silently lose a packet.
- List selected Plan 9 assembly entry points and callers in the report.
- First add scenario coverage for assembly routines such as stack switching,
  signal trampolines, atomics, memmove/memclr, syscall entry, and preemption.
- Later add native cg12 assembly counters at safe basic-block boundaries. Do not
  insert calls or stack-using instrumentation into raw stack-switching code.

Exit criterion: two consecutive full runs have the same denominator, every
capability in the matrix reports an explicit compile/run/coverage outcome, and
report diffs are usable in code review.

The denominator is no longer an empirical observation across runs. It is
`len(runtimeCapabilities())`, recorded in every report as `matrix_capabilities`,
and a run either emits one row per capability or fails: a capability that
reports nothing gets an `unreported` row plus a collection error, a duplicated
row is rejected by report validation, and a sharded coverage run is refused
outright. Two runs cannot disagree about the denominator without one of them
failing.

The explicit outcomes hold as well. The 2026-07-31 collection over all 338
capabilities produced 338 compile outcomes, 338 run outcomes, and 338 coverage
outcomes, every one of the last `collected`, with no silent absences. The three
classified timeouts the 2026-07-28 run carried are gone.

The drift guard did its job on the way here. Between the 2026-07-22 baseline and
this run the runtime source fingerprint moved from
`10a75b3cbbd95507daf7cd4ac1b4aa3b6ddcab338ea0d00e8a79c52bdbb9bb06` to
`06e314f8a5a729394207296e943ea075e51dbd39355c3389ca7ac65a52cb8f55`, and
`make runtime-cover-diff` refused to compare across that boundary rather than
producing a misleading delta. Accepting the 2026-07-31 run is what restored the
comparison: diffing the accepted baseline against the report it was built from
runs to completion and reports zero deltas in every bucket.

## 5. Phase 1: Repair current hard failures

This phase has priority over increasing coverage.

### 5.1 Defer, panic, recover, and stack unwinding

Current checkpoint:

- [x] Preserve metadata-entered `runtime.deferreturn` continuations through IR
  lowering, and reject defer registrations without an emitted continuation PC.
- [x] Model the standard compiler's synthetic edge from each defer registration
  to the shared recovery exit so recovery-result liveness and register
  allocation remain valid on metadata-entered paths.
- [x] Heap-lift direct deferred function literals in runtime builds when the
  same `defer` statement can register more than once — inside a loop, or below a
  label a `goto` can jump back to — and keep named result slots on the same heap
  cell when such an escaping deferred closure captures the result. A `defer`
  that registers at most once per frame keeps its closure descriptor in a frame
  slot and captures by reference; heap-lifting those too was the §5.2.1 bug and
  was corrected on 2026-07-28.
- [x] Keep the runtime's `_panic` record stack-resident through
  `unsafe.Pointer` conversions passed to `runtime.noescape`; both panic-time GC
  cases now pass with `GOGC=1`.
- [x] Include stack-allocated local object pointer fields in every call-site
  local stack map, not just the static local map, so stack copying relocates
  `_panic.gopanicFP` and similar stack-resident runtime fields.
- [x] Pass the basic, nested, typed, named-result, panic-replacement, goroutine,
  Goexit, deep-unwind, error-interface, and panic-time GC cases in normal builds
  (21 of 21 must-pass cases).
- [x] Pass the complete defer/panic batch under optimization (21 of 21
  must-pass cases, plus the deliberate uncaught-panic subprocess).
- [x] Run all three optimized panic/stack GC cases 100 times with `GOGC=1`.
- [x] Pass the complete optimized defer/panic batch with `GOMAXPROCS=1`, `2`,
  and `4` under the 3 GiB compiler/process limit.

Start with the smallest `no deferreturn` case, then move through nested defers,
typed values, named results, panic replacement, goroutine recovery, Goexit,
stack growth, and panic-time GC.

Add compiler validation for:

- every function containing a defer has a valid defer-return continuation;
- call-site PCs and return PCs resolve to the correct function and stack map;
- open-coded and linked defer records agree with the generated frame layout;
- unwinding restores SP, FP, LR, and the goroutine stack bounds at every frame;
- live pointer slots at panic and defer calls match emitted GC metadata.

Current post-rebase checkpoint: the focused stack and recover reducers pass in
normal builds, and the stack panic pair passes with `-runtime-opt` under the
3 GiB limit. The complete defer/panic category and the 100-run `GOGC=1` stress
batch still need to be rerun after the safepoint-map fix.

The previous full optimized run's hard failures were spot-checked after the
safepoint-map fix. `gc/pinner-lifecycle`,
`stdlib-signals/notify-context`, `stdlib-os-process/exec-echo`, and
`runtime-packages/timer-gc-channel` now pass in both normal and `-runtime-opt`
single-run configurations under the 3 GiB limit.

Exit criterion: the entire defer/panic category passes at multiple optimization
levels, with forced stack growth and `GOGC=1`, and the panic-stack-GC tests run
at least 100 repetitions without invariant failures.

### 5.2 Signal notification and runtime atomics

Reduce the notification-context crash to an atomic/locking or signal-delivery
test. Verify atomic width, alignment, memory ordering, address lowering, and the
signal path's preservation of the current goroutine and machine state.

Add tests for notification, stop/reset, repeated delivery, delivery during GC,
delivery while blocked in netpoll, and concurrent atomic operations. Keep
unsupported fatal-signal behavior in subprocess tests.

Current checkpoint:

- [x] Pass notification context, repeated stop/reset, delivery during netpoll,
  and concurrent atomic contention.
- [x] Keep the temporary slice headers captured by `runtime.selectgo`'s
  synchronous unlock closure on the stack, while still promoting slice backing
  storage assigned to globals.
- [x] Pass delivery during GC. It was never a signal bug: every signal was
  delivered, but the receiving goroutine was stalled past the program's own 2 s
  deadline by the GC-assist allocation recursion. Fixed with 5.2.1 on
  2026-07-28; `stdlib-signals/during-gc` is `mustPass` again and runs in 0.03 s.
- [ ] Pass the locked-global-select case and the full signal category ten
  executions per program with optimization and `GOMAXPROCS=1`, `2`, and `4`.
  Four of the five signal programs are clean over 10 optimized runs at all three
  `GOMAXPROCS` values. `during-gc` is clean at `GOMAXPROCS=1` and `2` and fails
  3 of 60 optimized runs at `GOMAXPROCS=4` with `fatal error: found bad pointer
  in Go heap`. **This is not a signal or a 5.2.1 problem**: the bad pointer is
  reported with no containing object, so the reference is a root — a stale stack
  address surviving into a scan. Reduced and fixed on 2026-07-28; the root was
  `runtime.KeepAlive`'s global slot, which `stdlib_signal_during_gc.go` uses.
  See §5.8 for the reduction, the fix and the before/after rates. The claim in
  this bullet's earlier text that the same fault reproduced in
  `goroutine/worker-fanin-gc` was measured on 381f67c and no longer holds: that
  program contains no `runtime.KeepAlive` and now runs 4000 of 4000 clean at
  `GOMAXPROCS=4` both before and after the §5.8 fix.

Exit criterion: signal tests pass under `GOMAXPROCS=1`, `2`, and `4`, including
race-like stress, without deadlock or runtime lock corruption.

#### 5.2.1 Fixed: `runtime.gcAssistAlloc` allocated (2026-07-28)

`goroutine/many-goroutines-gc`, `scheduler-stress/gc-churn`,
`stdlib-bytes/grow-allocs`, and `stdlib-signals/during-gc` were all recorded as
"times out under sustained GC allocation pressure; root cause not yet separated
from the 5.3 over-retention bug". They are one bug, and it is **not** the 5.3
over-retention bug. It is:

**`runtime.gcAssistAlloc` allocates, so the GC assist path recurses without
bound during marking.**

`mgcmark.go` opens `gcAssistAlloc` with the synctest block

```go
if gp := getg(); gp.bubble != nil {
	bubble := gp.bubble
	gp.bubble = nil
	defer func() { gp.bubble = bubble }()
}
```

cg12 heap-lifts `gp` because a deferred function literal captures it, and it
emits that `runtime.newobject` call *before* the `gp.bubble != nil` test, so the
allocation happens on every call even though the branch is never taken outside
synctest. `mallocgc` calls `deductAssistCredit` whenever `gcBlackenEnabled != 0`,
and `deductAssistCredit` calls `gcAssistAlloc` when the goroutine is in debt, so
during the mark phase the cycle

```
mallocgc -> deductAssistCredit -> gcAssistAlloc -> newobject -> mallocgc
```

repeats. It is unbounded: the allocation sits above `gcAssistAlloc`'s `retry`
label and above the `systemstack(gcAssistAlloc1)` call, so every level takes on
fresh assist debt *before* any level performs scan work. `debug.SetMaxStack(1 <<
20)` on eight goroutines calling `runtime.GC()` concurrently produces `fatal
error: stack overflow` with 3,100+ frames of exactly that four-function cycle.

Ordinarily the recursion unwinds when the background mark worker finishes and
clears `gcBlackenEnabled`, so the depth is set by how long the mark phase lasts.
That closes a positive feedback loop: deeper recursion means larger goroutine
stacks, larger stacks mean a longer stack scan, a longer mark phase means deeper
recursion on the next cycle. `shrinkstack` can only halve a stack per cycle and
loses the race. Measured with eight goroutines calling `runtime.GC()`,
`GOMAXPROCS=1`:

| | host Go | cg12 |
| --- | ---: | ---: |
| `StackInuse` after cycle 1 | 288 KiB | 8.6 MiB |
| after cycle 3 | 288 KiB | 53 MiB |
| after cycle 5 | 288 KiB | 1.01 GiB |

`GODEBUG=gctrace=1` on the failing capabilities shows the same shape from the
other side: the heap is flat (`0->0->0 MB`) while the stack-scan volume and the
mark phase quadruple per cycle — `many-goroutines-gc` reports 11 MB / 1842 ms,
then 22 MB / 3856 ms, then 89 MB / 15030 ms. **Unbounded stack, flat live heap**
is the opposite of the over-retention signature, which is why the 5.3 hypothesis
is wrong.

Classification of the four, all confirmed against the host toolchain, which runs
each program in 0.00-0.04 s:

| Capability | Verdict |
| --- | --- |
| `goroutine/many-goroutines-gc` | performance shortfall, not a hang; instrumented, 3 of its 48 workers finish their `runtime.GC()` in the first 60 s and only 5 in 400 s, because each cycle costs more than the last |
| `scheduler-stress/gc-churn` | same |
| `stdlib-bytes/grow-allocs` | same; it reaches the final case and stalls there. Its spurious `AllocsPerRun` counts are the recursive assist allocations, not a `bytes.Buffer` bug |
| `stdlib-signals/during-gc` | performance shortfall, **not** a delivery gap. Instrumented delivery latencies were 150 ms, 437 ms, 1749 ms, 8657 ms, 2 ms, ..., 33441 ms, 25203 ms, 36106 ms. Every signal arrives; the program's own 2 s deadline is what fails |

Proof of causality: rewriting the synctest block so the captured variable is
declared inside the taken branch removes the hot-path allocation without
changing semantics. With that one change all four capabilities pass, unoptimized
and with `-O`, at `GOMAXPROCS=1`, `2`, and `4`, in 0.03-0.35 s. The experiment
was reverted; the vendored stdlib is unmodified. `gc/cleanup-frame-retention`
still fails with the experiment applied, which confirms 5.2.1 and 5.3 are
independent bugs.

The fix belongs in cg12's escape analysis, not in the runtime. Two reductions
are committed:

| Capability | Expectation | Role |
| --- | --- | --- |
| `gc/assist-alloc-recursion` | `mustPass` | the runtime-level failure: concurrent `runtime.GC()` drives `StackInuse` past 4 MiB in one cycle |
| `gc/defer-capture-allocs` | `mustPass` | the compiler-level failure, with its controls |

`gc/defer-capture-allocs` reports four allocation counts, all of which are 0 on
the host toolchain:

| Shape | cg12 before | cg12 after |
| --- | ---: | ---: |
| variable captured by a deferred literal on an **untaken** branch | 1 | 0 |
| variable captured by an unconditional deferred literal | 2 | 0 |
| variable captured by an immediately invoked literal | 0 | 0 |
| deferred literal with no capture | 0 | 0 |

##### The fix

The trigger was specifically `defer` plus capture, and the reason was the
blanket heap lift recorded in 5.1: `goc.functionLiteralEscapesWithin` treated
*every* directly-deferred function literal in a runtime build as escaping.

That premise is wrong. `deferreturn` and `gopanic` run a deferred closure inside
the frame that registered it, so a directly-deferred literal does not outlive
its frame: its descriptor belongs in a frame slot and it can capture the
enclosing variables by reference. All three relocation paths for a frame-resident
descriptor already existed — `markStackPointerWord` puts the descriptor's capture
words in the frame pointer map (which `3f11295` unions into every safepoint map),
`runtime.adjustdefers` relocates `_defer.fn`, and `goRegisterSpills` spills the
closure-context register as a pointer root at the `morestack` safepoint so
`adjustctxt` relocates it. `runtime.popDefer` even documents `d.fn` as something
that "can in theory point to the stack". Range-over-func lowering already builds
its yield closure exactly this way.

The premise only fails when the same `defer` statement can register more than
once. The frame holds one descriptor slot per `defer` statement, so a second
registration would overwrite the first, and every `_defer` record would end up
pointing at one descriptor holding the last registration's captures. Those defers
keep the heap descriptor and the heap-lifted captures.

`goc.deferStatementRepeats` therefore decides the escape, and a defer repeats
when either:

- it is lexically inside a `for` or `range` statement; or
- some `goto L` in the same function body targets a label declared at or before
  it, so control can jump back to it. Labels declared *after* the defer cannot
  re-reach it — which is what keeps `gcAssistAlloc` on the non-repeating path,
  since its synctest defer sits above its own `retry:` label.

An ancestor `*ast.FuncLit` ends the walk: the defer then belongs to that
literal's frame, which is entered afresh on every call.

Verified: `gc/defer-capture-allocs` reports 0/0/0/0, matching the host
toolchain; `gc/assist-alloc-recursion` passes; and all four attributed
capabilities, plus both reducers, run clean for 10 executions each unoptimized
and with `-O` at `GOMAXPROCS=1` and `2`, in 0.03-0.37 s at `GOMAXPROCS=1` — the
same range the runtime-source experiment produced. `GOMAXPROCS=4` is clean too
except for rare hits of the *separate, pre-existing* `found bad pointer in Go
heap` fault recorded in the §5.2 checkpoint above (measured 1 of 60 for
`many-goroutines-gc`, 3 of 60 for `during-gc`, against 8 of 60 for
`worker-fanin-gc` on the pre-5.2.1 tree) — reduced and fixed in §5.8 on
2026-07-28. The whole `defer-panic` category (21
must-pass plus the deliberate uncaught-panic subprocess) passes unoptimized and,
at three executions per program, with `-O` at `GOMAXPROCS=1`, `2`, and `4`; the
three panic/stack GC cases ran 100 times each, optimized, with `GOGC=1`, at
`GOMAXPROCS=1`, `2`, and `4` without a failure, compiled under a 3 GiB address
space limit.

`goc/escape_test.go` pins both directions of the rule and
`goc/corpus_test.go`'s `TestRuntimeRepeatedDeferCapturesEachRegistration`
executes the loop and backward-goto shapes to prove each registration still
captures its own value.

This was the second instance of the same class as the print-routine allocation
fixed in 5.3 — cg12 introducing a heap allocation into a runtime path that must
not allocate — and an audit for further instances is still worth doing.

Incidental, found while probing and not investigated further:
`runtime.NumGoroutine()` returns 5 in a cg12 program where the host toolchain
returns 1. `isSystemGoroutine` classifies by `HasPrefix(funcname(f),
"runtime.")`, and cg12's names appear as `runtime_bgsweep` rather than
`runtime.bgsweep`, so `sched.ngsys` is never incremented and the runtime's own
helper goroutines are counted as user goroutines. This is consistent with the
observed counts but was not proven; anything else keyed on runtime function
names deserves the same check.

### 5.3 Finalizer resurrection and cleanup

Test basic finalization, resurrection, dependency ordering, clearing a
finalizer, tiny objects, pointerful objects, stack growth in a finalizer,
KeepAlive, cleanup callbacks, and repeated GC cycles. Validate that pointer
liveness at `runtime.KeepAlive` and finalizer registration sites survives
optimization.

Current checkpoint:

- [x] Emit safepoint-specific local stack maps instead of conservatively
  retaining every pointer-bearing local for the entire function. Pointer words
  contained in a live alloca are now expanded into that safepoint's map. The
  function-wide map is no longer unioned into each safepoint; only allocations
  whose address leaves the frame stay conservative.
- [x] Write the argument pointer map at every stack-map index rather than only
  at index 0, and keep body safepoints off the entry index, so a stack-passed
  pointer argument is a root for the whole call and the register home slots are
  named only in the prologue window that writes them. See "The argument
  stack-map hole" below for what was and was not observable.
- [x] Pass synchronized finalizer resurrection after explicitly clearing the
  last local pointer in a still-running frame
  (`runtime-packages/finalizer-resurrect`, now `mustPass`). The `SetFinalizer`
  interface temporary's address reaches a destructed phi; the derivation map is
  now set-valued, so the merge is tracked instead of forcing the allocation
  conservative. See "Closing the merged-address boundary" below.
- [x] Pass basic finalization, clearing, KeepAlive, pointerful stack growth,
  dependency ordering, tiny-object finalization, `Cleanup.Stop`, and
  finalizer-before-cleanup ordering.
- [x] Pass standalone cleanup callbacks (`gc/cleanup-basic`,
  `gc/cleanup-multiple`) and the minimal reducer `gc/cleanup-frame-retention`,
  three runs each, unoptimized and with `-runtime-opt`, at `GOMAXPROCS` 1, 2
  and 4. The three `cleanup-frame-retention` reducers additionally pass six of
  six runs at `GOMAXPROCS=64`, where they failed six of six before the §5.7
  write barrier fix. That failure was §5.7's, not this section's.
- [x] Pass the complete finalizer/cleanup batch ten times per program with
  optimization and `GOMAXPROCS=4`. Verified 2026-07-28 over the sixteen
  programs the batch has grown to: `gc/keepalive-finalizer`,
  `finalizer-stack-growth`, `setfinalizer-clear`, `cleanup-basic`,
  `cleanup-stop`, `cleanup-multiple`, the three `cleanup-frame-retention`
  reducers, `finalizer-cleanup-order`, `finalizer-dependency-order`,
  `finalizer-tiny`, `pinner-lifecycle`, `pinner-invalid`, plus
  `runtime-packages/finalizer-basic` and `runtime-packages/finalizer-resurrect`.
- [x] Add multiple-cleanup and `Pinner` lifecycle/invalid-use cases.
- [x] Add `GODEBUG=cg12checkstackcopy=1` runtime validation that detects stale
  old-stack pointers at stack-copy boundaries and finalizer queue corruption
  before the later nil-function `reflectcall` crash.
- [x] Isolate the tiny-finalizer corruption to stack shrinking: the optimized
  binary passed 100 runs with `GODEBUG=gcshrinkstackoff=1`, and the detector
  reported stale old-stack-looking words in copied `runtime_main` frames.
- [x] Broaden Go-managed stack metadata so pointer-class spilled temporaries
  participate in safepoint and conservative local pointer maps for stack
  relocation.
- [x] Pass `runtime_finalizer_tiny.go` 500 direct optimized executions with
  `GOMAXPROCS=2`, `GOGC=10`, and `GOMEMLIMIT=768MiB`.
- [x] Pass the cleanup/finalizer/Pinner status subset three times per program
  with optimization and `GOMAXPROCS=2`. Verified 2026-07-28 over the same
  sixteen programs as the box above.

#### Root cause of the cleanup/finalizer regression (2026-07-28)

The 2026-07-27 diagnosis in this section was wrong, and the correction matters
because it was pointing the next investigation at the wrong subsystem. It read:

> The retaining reference is a stale pointer word in `register`'s *abandoned*
> frame, below `main`'s SP. [...] cg12's goroutine stack scan is treating dead
> stack below the live frames as live.

It is not. `GODEBUG=cg12scanroots=1` (added for this, see below) names the
retaining frame directly, and for `runtime_cleanup_frame_retention.go` it is
`main.main` itself:

```
cg12scanroots: main_main local slot 7  at 0x...5ca8 retains 0x...998300 size 16 head 0x2a
cg12scanroots: main_main local slot 9  at 0x...5cb8 retains 0x...998300 size 16 head 0x2a
cg12scanroots: main_main local slot 28 at 0x...5d50 retains 0x...998300 size 16 head 0x2a
```

`head 0x2a` is `retainedBox{value: 42}`. `registerRetainedCleanup` is **inlined
into `main`** -- the out-of-line symbol still exists, but `main` calls
`runtime.AddCleanup` directly -- so `box` never had a frame of its own to
abandon. Its three words are `main`'s own locals: the spilled `newobject`
result, a reload of it, and the variable's alloca cell. `main`'s frame lives for
the whole `runtime.GC()` loop, so the object never dies.

Two further facts contradict the old reading. cg12's prologue already zeroes
every conservatively marked pointer slot (`mc.zeroGoPointerSlots`), so no
runtime frame can observe what a returned frame left in a slot it has not
written. And `runtime_cleanup_frame_retention_scribble.go` passes with the
`scribbleDeadStack` call removed: its helper is simply not inlined, so `box`
never enters `main`'s frame. The scribble is not what releases the object and
that file is not a positive proof of anything; the difference between the two
programs is inlining.

The mechanism is in the metadata, not the scan. `gometa.FunctionStackMaps` built
each safepoint's locals map as the union of that safepoint's live roots with the
function-wide `LocalPointerWords` -- every pointer-bearing local word and every
pointer spill slot the frame ever uses. Every call in a function therefore
reported every pointer the frame ever held as a live root, for as long as the
frame existed. That is a general leak: any dead pointer local in a long-lived
frame is retained. Cleanups and finalizers are only how user code notices.

This bug's blast radius is narrower than it looked. The four GC-pressure
capabilities that were provisionally attributed to it are a separate bug with a
separate fix; see 5.2.1.

The reduction is committed rather than left in a scratch directory, as three
`gc` capabilities:

| Capability | Expectation | Role |
| --- | --- | --- |
| `cleanup-frame-retention` | `mustPass` | the minimal failure; now passing |
| `cleanup-frame-retention-masked` | `mustPass` | the eliminated hypotheses — argument shape, allocation pressure, cleanup count, finalizers-work |
| `cleanup-frame-retention-scribble` | `mustPass` | kept as a regression test — but *not* the positive proof it was taken for; see below |

All three are frame-layout sensitive, which is why they are three programs and
not one. Do not consolidate them, and do not add statements to
`runtime_cleanup_frame_retention.go` — either turns a real failure into a false
pass. Each file records this in its own header. The headers were corrected on
2026-07-28: they previously described the scribble file as the positive proof of
the stale-frame theory, which the diagnostic below disproved.

#### The stack-scan diagnostic (2026-07-28)

`GODEBUG=cg12scanroots=1` prints, for every frame the precise stack scan walks,
each pointer word that retains a heap object: the function, whether the word is
a local or an argument, the stack-map slot index, the word's address, and the
object's base, size and first word. `cg12scanroots=2` adds each frame's
sp/fp/varp/argp geometry. It examines exactly the words `scanblock` would
process, so it follows no pointer the scan would not. It lives in
`runtime.scanframeworker` (`stdlib/src/runtime/mgcmark.go`).

This is the tool that names a retaining frame, which is what the previous
investigation was missing.

#### Why per-safepoint precision is not sufficient on its own

Emitting only each safepoint's own live roots fixes the reducer and
`gc/cleanup-basic`, `gc/cleanup-multiple`, but on its own it broke three
must-pass capabilities: `gc/pinner-lifecycle` (`found leaking pinned pointer`),
`runtime-packages/timer-gc-channel` (segfault) and `stack/panic-stack-gc`
(`missing panic`). All three are address-taken locals.

cg12 has no notion of a source-level variable, so it approximates the liveness
of an address-taken local by the liveness of the temporaries that hold its
address. That is only sound while every use is visible in the frame. It is not
when the address leaves: `runtime.gopanic` publishes `&p` through `g._panic`,
and `main` hands `&pinner` to a callee that stores a heap pointer into it. The
conservative map was covering those cases by accident.

The compiler therefore keeps a boundary (`arm64.frameEscapingAllocations`):
a pointer-bearing allocation whose address is used for anything beyond
addressing itself is reported at every safepoint, exactly as before; everything
else -- spill slots and frame-local allocations -- is per-safepoint precise.
Loads, stores, lifetime markers, constant-offset derivations and cg12's own
`goc_memcpy`/`goc_memset`/`goc_storep` primitives are address-only uses;
every other use, including any other call, is an escape.

#### Closing the merged-address boundary (2026-07-28)

`runtime-packages/finalizer-resurrect` was the last capability left behind by
that boundary, and it is now `mustPass`. The diagnosis above was right about the
retaining word and wrong about what it would take to fix.

`GODEBUG=cg12scanroots=1` on the unoptimized binary named the two words
directly:

```
cg12scanroots: main_main local slot 29 at 0x...5bc98 retains 0x...dfe2f0 size 16 head 0x29
cg12scanroots: main_main local slot 31 at 0x...5bca8 retains 0x...dfe310 size 16 head 0x59dae4
```

`head 0x29` is 41, the `resurrectedObject`. The two slots are 16 bytes apart:
they are the data words of the two 16-byte interface descriptors
`normalizeCallInterfaces` builds for `runtime.SetFinalizer`'s two arguments. The
emitted IR shows why both were conservative:

```
%t12 =p alloc8 16          ; the object's interface descriptor
storel $.goc.type.692, %t12
%t13 =p add %t12, 8
storel %t11, %t13          ; ... its data word is the object
%t18 =p alloc8 16          ; a zeroed descriptor
%t20 =p ceqp %t12, 0       ; goc's interface nil check
jnz %t20, @callinterfacezero1, @callinterfacevalue2
@callinterfaceend3
%t21 =p phi @callinterfacezero1 %t18, @callinterfacevalue2 %t12
```

Two separate rules fired on this shape. `pointerAllocationSources` mapped each
temporary to *one* allocation, so the destructed phi -- whose two copies name
`%t18` on one edge and `%t12` on the other -- was a conflict it recorded as
`merged`, and `frameEscapingAllocations` seeded its escape set from `merged`.
Independently, the `ceqp` that compares the descriptor's address against nil was
not a recognized address operand, which escapes the allocation on its own.

So the backend *could* recover this syntactically; what it lacked was a
representation for an address that may name more than one allocation. Neither
the frontend escape result nor lifetime markers were needed, and both would have
been larger changes for a narrower result -- goc's escape analysis answers "does
this value outlive the frame", not "which frame slots may this temporary
address", and lifetime markers say when a slot is dead, not where its address
went. The fix is in `arm64/regalloc.go`:

- `pointerAllocationSources` returns `allocationAddresses`, a temporary to the
  *set* of allocations it may address, computed as a worklist fixpoint over
  address derivations. Copies, constant-offset additions, conditional selects
  (`OSel`) and phis all carry a base's set into the result.
- `frameEscapingAllocations` escapes every allocation a non-address-only operand
  may address, rather than the single one it was mapped to.
- `addressOnlyOperand` additionally recognizes comparisons (`OCmp`, `OCCmp`),
  block copies (`OBlit`) and conditional selects: all consume an address without
  letting it out of the frame. `addressOnlyTerminatorOperand` does the same for a
  conditional branch and a switch, which test their operand and produce no value;
  a return and a computed goto stay escapes.

Both halves of that last rule are load-bearing, one per optimization level.
Unoptimized, the nil check is the `ceqp` above and the compare rule covers it.
With `-O` the compare is folded into the branch -- the lowered body reads
`jnz %t12, @callinterfacevalue2, @callinterfacezero1`, testing the alloca
address directly -- and only the terminator rule covers that. The `OSel` case is
not reached by this program; a select is what if-conversion produces from the
same diamond in other shapes, and it is covered by unit test rather than by a
capability.

`frameEscapingAllocations` also now reads `Instr.ClosureContext`, which is a
value operand carried outside `Instr.Args` (see `Instr.Uses`). The operand walk
had never visited it, so a frame address handed to a callee as a closure
environment would have escaped unnoticed. No program is known to have hit it;
it is closed because the walk claims to be exhaustive.

The escape boundary itself is unchanged in kind -- an address passed to any
other call, stored into memory, returned, or added to a runtime value is still
conservative for the life of the frame. What changed is that merging two frame
addresses, and testing one, are no longer escapes.

This is a general shrink, not a one-program fix. Counting over a whole
`finalizer-resurrect` build -- 4229 functions, 14047 pointer-bearing stack
allocations -- the conservative set drops from 6335 allocations to 5543, 12.5%
fewer.

`runtime.gopanic` is the case worth checking by hand, because it is one of the
three the boundary exists for. It goes from 23 of its 37 allocations
conservative to 11, and the twelve that stop escaping all have one or two
pointer words: they are the 16-byte interface descriptors built for its
`print` and `throw` calls. The allocation with nine pointer words -- `_panic`
itself, whose nine are `arg`'s two, `link`, `startSP`, `sp`, `fp`,
`deferBitsPtr`, `slotsPtr` and `gopanicFP` -- is escaping before and after,
because `&p` reaches `p.start`, `p.nextDefer`, `preprintpanics` and
`fatalpanic` as an ordinary call operand. Nothing moved in the other direction:
no allocation became escaping that was not before.

Covered by `TestGoStackMapsDropMergedInterfaceDescriptorsAfterTheirCall`,
`TestFrameEscapingAllocationsKeepPublishedAddressesConservative` and
`TestPointerAllocationSourcesTrackEveryMergedAllocation` in
`arm64/unit_test.go`.

The three capabilities that broke the last time this boundary moved --
`gc/pinner-lifecycle`, `runtime-packages/timer-gc-channel` and
`stack/panic-stack-gc` -- were run 15 times each as direct executions,
unoptimized and with `-O`, at `GOMAXPROCS` 1, 2 and 4: 270 runs, no failures.
They then passed 10 harness runs each with `-runtime-opt -runtime-procs=4`,
which also checks their expected output.

`GODEBUG=cg12scanroots=1` on the fixed `finalizer-resurrect` binary reports one
`main_main` root, the `done` channel's box. The two descriptor data words that
kept the object alive are gone.

#### The argument stack-map hole (2026-07-28)

The previous agent flagged that `builder.stackMaps` writes the argument pointer
map only at index 0 while safepoints select index >= 1, so stack-passed
arguments are never scanned at a safepoint. That reading of the code is
accurate, and the runtime does index both tables with one value -- `getStackMap`
computes `pcdata` once and uses it for `FUNCDATA_LocalsPointerMaps` and
`FUNCDATA_ArgsPointerMaps` alike (`stdlib/src/runtime/stkframe.go`). But index 0
is not an arbitrary index. It is the entry window: `StackMapPCData` selects it
over `[0, frameStart)`, which is exactly the stack-growth prologue and therefore
the call to `morestack` -- the one point where the argument frame is the only
home of the caller's stack-passed arguments, because the callee has not loaded
them yet. Writing the argument map there is deliberate and necessary.

Two real defects fall out of writing it *only* there.

**Body safepoints read an all-zero argument map.** Nothing described the
caller's stack-passed pointer arguments for the duration of the call. Today no
object is lost to this, and the reason is worth recording because it is an
accident of the frontend rather than a designed invariant: goc gives every
parameter its own local variable slot and copies the incoming value into it in
the entry block, before the first safepoint -- scalars through the `OPar` loads,
by-value aggregates through a memcpy off the incoming argument address. The
callee's own frame therefore roots every pointer argument, and cg12 never
re-reads the argument frame afterwards (`OPar` is not rematerializable and a
GCRef temp is never rematerialized). `GODEBUG=cg12scanroots=1` confirms this for
pointer, string, interface, slice and by-value-struct arguments passed past the
eight integer argument registers: before the fix nothing in `main_stacked`'s
argument frame was reported and the callee's locals held every payload. So this
half is a latent defect, not a live one -- but it is one line away from being
live, and it is precisely the redundancy the section above is working to remove.

**Rootless body safepoints read the *entry* map.** This one is live.
`FunctionStackMaps` matched a safepoint with no live frame roots against
`pointerMaps[0]`, which is empty, so such a safepoint resolved to index 0 --
sharing the entry index, and with it the entry *argument* map. That map marks
the register-argument home slots, and `goStackPrologue` writes those only on the
path that falls through the stack-guard check to `morestack`. On every other
path they are words of the caller's outgoing area that nobody wrote. The
collector was therefore scanning uninitialised stack memory as roots at ordinary
calls. It is not rare: a single `fmt.Sprintf` program has 1835 functions with at
least one rootless safepoint and a non-empty argument pointer map, almost all of
them the generated `_interfacecall_` and `_gointernal_funcvalue_` adapters,
which forward their arguments and hold no frame root at the forwarding call.
`time_Time_After_interfacecall` is representative: `argSize=16`,
`argPtrWords=[1]`, `localPtrWords=[]`, one safepoint, and word 1 is the home
slot for X1.

The fix is at the metadata layer and has two halves:

- `gometa.FunctionStackMaps` reserves index 0 for the entry window. A safepoint
  never resolves to it, even when its locals map would be identical; a rootless
  safepoint gets its own empty map instead.
- `gometa.ArgumentStackMaps` writes one argument bitmap per locals map. Index 0
  keeps the full argument map. Every body index gets the caller-initialised
  subset: `arm64.goArgumentFrameFor` now returns the stack-passed argument words
  separately from the words only the callee ever writes, so a stack-passed
  pointer argument is a root for the whole call while a register home slot is
  named only in the window where the prologue has just written it.

Covered by `TestGoFunctionStackMapsKeepRootlessSafepointsOffTheEntryIndex`,
`TestGoFunctionStackMapsShareTheBodyIndexWhenNoFrameRootExists`,
`TestGoArgumentStackMapsCoverEveryStackMapIndex`,
`TestGoArgumentStackMapsEmitOneBitmapPerLocalsMap`,
`TestManagedAAPCS64SeparatesIncomingArgumentsFromRegisterHomes`,
`TestManagedAAPCS64KeepsStackedAggregateWordsInBothArgumentMaps` and
`TestManagedAAPCS64LeavesRegisterOnlyArgumentsOutOfTheBodyMap`, and by the
`gc/stack-argument-roots` capability
(`goc/testdata/runtime_stack_argument_roots.go`), which passes pointer, string,
interface and slice arguments past the register budget and holds them live
across `runtime.GC()` and a stack copy. Be honest about what that capability is:
it passes 10/10 at `GOMAXPROCS=1` both before and after the fix, because of the
frontend redundancy above. It pins the property; it is not a reducer for a
failure. What it does change is where the roots come from --
`GODEBUG=cg12scanroots=1` reports `main_stacked arg slot 0/1/4` only after the
fix. (At the box's default `GOMAXPROCS=64` it fails 10/10 both before and after,
in the pre-existing `sweep increased allocation count`; so does the existing
`gc/cleanup-basic`, which is why the status harness runs these at
`-runtime-procs=1`.)

Two things this did not close, both stated so the next investigation does not
have to rediscover them:

- The entry map still marks the stack result area of a Go-ABIInternal aggregate
  return. Those words are written by the callee just before it returns, so at
  the `morestack` call they are uninitialised, exactly like the home slots on
  the fast path. They are excluded from the body map by the fix but left in the
  entry map, because changing what `morestack` scans is a separate change with
  its own evidence to gather.
- No runtime reducer was built for the rootless-safepoint garbage scan. It needs
  a caller frame whose outgoing area holds a stale heap pointer at the word an
  adapter's home slot lands on, which is as frame-layout sensitive as the
  reducers above, and every shape that arranges it -- an interface method call
  driven by a bounded `runtime.GC()` loop -- dies first in the pre-existing
  `fatal error: sweep increased allocation count`, identically before and after
  this fix. The evidence for it is therefore static: the emitted index points
  and argument maps, reproduced from real compilations.

  That failure is worth a second look from whoever picks up 5.2.1. Its printout
  is `nelems=16 nalloc=17`, so the sweeper counted more mark bits than the span
  has elements -- a mark bit set past the end of the span, which is what
  `greyobject` does when handed a word that is not a real object pointer.
  Scanning uninitialised stack memory as roots produces exactly that. This is a
  lead, not a diagnosis: the fix above removes one source of such scans and the
  failure still reproduces, so if the two are related there is at least one more
  source.

#### Fixed: runtime print routines allocated (2026-07-27)

`GODEBUG=checkfinalizers=2` — the mode that prints the retention path — used to
die with `fatal error: mallocgc called with gcphase == _GCmarktermination`. The
cause was not in the checkmark code: **`runtime.printuint`, `printint`, and
`printhexopts` each heap-allocated.** Every integer printed by the runtime went
through `runtime.newobject`, so any runtime diagnostic allocated, which is fatal
during mark termination and unsound on nosplit and fatal paths generally. The
scope was much wider than the diagnostic that exposed it.

Root cause: `nonEscapingObjectUse` treated *any* slice expression over a local
or parameter as escaping. That forced `var buf [20]byte; gwrite(buf[i:])` onto
the heap. The rule now asks whether the resulting slice value escapes, using the
same flow-sensitive walk applied to other derived values, so a scratch array
passed to a non-retaining callee stays on the stack while `return buf[:]` still
promotes. Covered by `TestLocalArraySlicedIntoNonRetainingCalleeStaysOnStack`
and `TestLocalArraySlicedIntoReturnedSliceEscapes` in `goc/escape_test.go`.

Also fixed alongside it: cg12 routed the runtime's `type hex uint64` to
`printuint`, so every address in every runtime diagnostic printed in decimal.
The print builtin now special-cases the named `runtime.hex` type to `printhex`,
matching the standard compiler.

Known remaining conservatism, not required here: ranging over a slice parameter
(`for _, v := range text`) still forces the caller's backing array to the heap.
The runtime's own print path uses len/index/copy and is unaffected.

With the diagnostic working, `checkfinalizers=2` shows no `WARNING: LIKELY
CLEANUP/FINALIZER ISSUES` for the reducer — the object is *not* reachable from
the cleanup or its argument. That independently confirms the retention is
external to the cleanup, consistent with the stale-frame finding above, but it
does not by itself name the retaining frame. Naming that frame is the next step
and still needs a stack-scan-side diagnostic.

Exit criterion: finalizer and cleanup tests pass deterministically using bounded
GC/yield loops, and pinner/finalizer/cleanup coverage has deliberate tests for
every supported public behavior.

### 5.4 Reflection/gob failure

Current checkpoint:

- [x] Reduce the gob failure to `gob.NewEncoder(&buf).Encode(42)`, where the
  `encOpTable` static function value pointed directly at `encoding/gob.encInt`
  instead of a Go-internal function-value adapter.
- [x] Route static named function values in global composites through the same
  `.gointernal.funcvalue` adapters used for dynamic function values.
- [x] Add focused reflect and gob reducers for direct `Value.Int`, indirect
  `reflect.Value` calls, gob int encoding, gob single-int structs, and gob mixed
  structs.
- [x] Pass the optimized gob reducer batch and complete gob round trip.

Reduce gob to the first invalid `reflect.Value` operation. Exercise zero values,
interfaces, addressability, method calls, maps, slices, structs, pointers, and
typed nils before returning to the complete gob round trip.

Exit criterion: the reduced reflection matrix and gob round trip match host Go,
with no overlay that special-cases gob.

### 5.5 Trace and large-program resource failures

Measure compiler and generated-program memory independently. For trace, locate
whether growth is in compilation, runtime trace buffers, goroutine leakage, or
failure to drain. For HTTP, profile compilation by phase and package so the
largest retained structure is visible.

Current checkpoint:

- [x] Add focused trace reducers for start-only, start/stop, trace logging, and
  a probe that proves the failure occurs before `trace.Start` returns.
- [x] Add an opt-in nosplit audit (`GOC_DEBUG_NOSPLIT=1`) that reports direct
  calls from nosplit functions to functions that still contain stack-growth
  checks.
- [x] Preserve systemstack/mcall function literals as nosplit system-stack
  functions so cg12 does not emit stack-growth checks at the closure entry.
- [x] Avoid heap allocation for non-ellipsis variadic backing arrays created
  inside nosplit functions. This matches trace event writer requirements, where
  allocation while emitting trace events can recurse through the allocator.
- [x] Isolate the current trace-start crash beyond the trace buffer allocation:
  `unsafe.Sizeof(traceBuf{})` lowers to 64 KiB, while the constrained run OOMs
  in 512 MiB heap arena reservations and the unconstrained run reaches
  allocator stack-growth failures.
- [x] Define and implement a coherent policy for outlined allocator helpers.
  The first failing stack-growth points after `StartTrace` were
  `runtime.nextFreeFast`, `runtime.mcache.nextFree`, `runtime.mcache.refill`,
  and then `runtime.mcentral.cacheSpan`. cg12 currently outlines helpers that
  the upstream compiler either inlines into malloc paths or compiles with a
  stack budget that avoids `morestack` while `mallocing` is set.
- [x] Mark the outlined allocator fast-path helpers that can execute while
  `mallocing` as implicit nosplit functions, and force trace event writer
  variadic backing arrays onto the stack. This avoids recursive heap allocation
  while the runtime is emitting allocator/sweeper trace events.
- [x] Pass the optimized trace start-only, start/stop, start-probe, log, and
  buffer reducers individually under `GOMAXPROCS=1`, `GOGC=10`,
  `GOMEMLIMIT=768MiB`, and the 3 GiB process limit.
- [x] Re-check the stale large-program failures from the accepted baseline:
  optimized `stdlib-http/redirect-keepalive`, `stdlib-http/tls-client-server`,
  and `stdlib-crypto/ecdsa` now pass individually under the 3 GiB process
  limit. The full corpus rerun this asked for happened on 2026-07-31 and all
  three collect coverage in the accepted baseline.
- [x] Remove the duplicate legacy `.cg12_stackmaps` section from Go-runtime
  binaries; the native Go `moduledata`/pclntab stack maps are the authoritative
  metadata there. This fixed the unoptimized `stdlib-http/parse-roundtrip`
  object-emission OOM caused by a 54 MiB sparse stack-map allocation.
- [x] Add memory budgets for large-function CFG optimizations and switch
  huge modules to a bounded linear cleanup pipeline. Optimized
  `stdlib-http/parse-roundtrip`, `stdlib-http/cookiejar`, and
  `stdlib-http/multipart-form` now compile and run under the 3 GiB process
  limit with peak compile usage around 1.2-1.3 GiB.

Exit criterion: trace terminates and produces parseable output within its
budget; all three HTTP programs compile below the agreed memory ceiling and
return coverage packets. Every capability in the matrix returns a coverage
packet.

### 5.6 Generic method dispatch (the ECDSA gap)

Current checkpoint:

- [x] Reduce `stdlib-crypto/ecdsa` to a standalone program: a constraint whose
  type set is a union of pointer types and whose methods are written in terms
  of the type parameter, a carrier struct holding a `func() P` constructor, and
  one generic body instantiated for two concrete types.
- [x] Resolve a method selected on a type-parameter-typed value against the
  type argument bound by the enclosing instantiation, instead of leaving it on
  the constraint interface's method.
- [x] Carry the concrete selection, not just the concrete method, so a type
  argument that satisfies its constraint through an embedded field has its
  receiver advanced to that field.
- [x] Make the resolved method reachable, including the correct instantiation
  when the type argument is itself a generic type.
- [x] Pass `stdlib-crypto/ecdsa` three times per program in normal and
  `-runtime-opt` configurations.

The failure was not in the runtime and not in stdlib. go/types reports the
object selected by `p.M()`, where `p` has type-parameter type `P`, as the
method declared by `P`'s *constraint interface*. goc took that object at face
value: `methodHasInterfaceReceiver` saw an interface receiver, the call was
routed to the synthesized interface-dispatch wrapper
(`crypto/internal/fips140/ecdsa.Point.ScalarBaseMult`), and that wrapper
decoded its receiver as a two-word interface descriptor even though the caller
had passed a bare `*nistec.P256Point`. The wrapper also had no candidates,
because `types.Implements` is false between a concrete type and an
uninstantiated constraint whose method signatures still mention `P`. So the
concrete type never reached dispatch from either side, and the program
segfaulted in the wrapper's type-word load.

The fix is at the selection layer, in `goc/compile.go` and `goc/reach.go`: in
an instantiated body the type argument is statically known, so the call is an
ordinary direct call on that type's method, and the shared interface-dispatch
machinery is not involved at all. A body with no bound type argument keeps its
previous lowering. Nothing in the change names a package, type, or method.

Covered by `core-types/type-param-method-dispatch` and
`core-types/type-param-method-shapes`, and by
`TestTypeParameterMethodSelection`,
`TestTypeParameterMethodCallsLowerToTheConcreteMethod`, and
`TestOrdinaryInterfaceDispatchIsUnchanged` in `goc/`.

Exit criterion: met. `stdlib-crypto/rsa` was already `mustPass` and was not
affected.

### 5.7 Write barriers on not-in-heap pointers (the sweep-count gap)

Current checkpoint:

- [x] Reduce `fatal error: sweep increased allocation count` to a single
  compiler defect: cg12 emitted a write barrier for a store of a pointer to a
  not-in-heap type.
- [x] Omit the write barrier for pointers to types marked with
  `internal/runtime/sys.NotInHeap`, in both the scalar store path and the
  pointer-aware aggregate store path.
- [x] Pass `cmd/goc`'s `TestARM64GoRuntimeGarbageCollectorExecutes` and
  `TestARM64StandardLibraryIOAndFmtExecute/runtime_Linux_assembly`, which both
  failed on `origin/main`.
- [x] Pass `gc/cleanup-frame-retention`, `-masked` and `-scribble` at
  `GOMAXPROCS=64`. All three failed 6 of 6 runs there before the fix; the §5.3
  reducers were only ever exercised at `GOMAXPROCS` 1, 2 and 4.
- [ ] Close the general hole this exposed in `goc_storep`; see "What is still
  open" below.

#### Root cause (2026-07-28)

The failing throw is upstream's, and it is correct to fail:

```
runtime: nelems=16 nalloc=16 previous allocCount=15 nfreed=65535
fatal error: sweep increased allocation count
```

`nfreed=65535` is `^uint16(0)`, so the count did not increase -- `nalloc`, the
population count of `gcmarkBits`, exceeded `allocCount` by exactly one. Dumping
the span at the throw gives the same picture every time:

```
elemsize=480 spc=50 freeindex=15 inline=true
markwords=0x17fff  premove=0x0  imbmarks=0x17fff
base=...a8000 limit=...a9e00 (7680 bytes of objects)
```

`0x17fff` is bits 0-14, the fifteen live objects, plus **bit 16**. `nelems` is
16, so object index 16 does not exist, and the bit arrived in the Green Tea
inline mark bits (`imbmarks`) rather than in `gcmarkBits` (`premove=0x0`).
Object index 16 sits at `base+7680`; the pointer that produced it was
`base+7936`, which is `spanHeapBitsRange`'s base -- the span's own heap-bits
bitmap, in the 256-byte tail the span reserves for the pointer/scan bitmap and
the inline mark bits.

A check in `runtime.atomicwb` naming the frame that queued that address gives
the whole chain:

```
runtime_heapBitsSlice <- runtime_mspan_heapBits <- runtime_mspan_initHeapBits
  <- runtime_mcentral_grow <- runtime_mcentral_cacheSpan <- runtime_mcache_refill
  <- runtime_mcache_nextFree <- runtime_mallocgcSmallScanNoHeader
  <- runtime_newobject <- runtime_malg <- runtime_allocm <- runtime_newm
```

`runtime.heapBitsSlice` builds the span's bitmap slice by hand:

```go
var sl notInHeapSlice
sl = notInHeapSlice{(*notInHeap)(unsafe.Pointer(base)), elems, elems}
return *(*[]uintptr)(unsafe.Pointer(&sl))
```

`notInHeapSlice.array` is a `*notInHeap`. `internal/runtime/sys.NotInHeap`
states that write barriers on pointers to not-in-heap types may be omitted, and
the standard compiler implements that by reporting zero pointer-data bytes for
such a pointer, so upstream emits no barrier here at all. cg12 emitted one.
That barrier is not merely redundant, it is unsound: `goc_storep` forwards to
`runtime.atomicstorep`, whose `atomicwb` records both the stored value and the
destination slot's previous contents in the P's write barrier buffer. The
buffer outlives the store, so at `gcMarkDone` the address reached
`wbBufFlush1` -> `tryDeferToSpanScan`, which divides the offset by the size
class without bounding the result against `nelems` and set inline mark bit 16.
Sweep then counted 16 marked objects in a span whose `allocCount` was 15.

Two conditions gate it, and together they explain why only high `GOMAXPROCS`
showed the failure. First, `goc_storep` stores directly when the destination is
inside a heap span or a runtime-managed stack span, so the barrier is only
reached when `sl` lives on a stack the runtime did not allocate -- in practice
the initial thread's `g0` stack, which is the process stack. Second, the span
has to be grown while the mark phase is running. `newm`/`allocm`/`malg` on the
initial thread satisfies both, and how often that happens scales with the
number of Ps: `goc/testdata/gc_struct.go` passed 3 of 3 runs at `GOMAXPROCS` 1,
2, 4, 8 and 16 and failed 3 of 3 at 64. The capability matrix runs at 1-4,
which is why it never saw this.

The fix is in `goc/compile.go`: `isNotInHeapPointer` gates the scalar pointer
store, and `barrieredPointerWordIndices` drops not-in-heap pointer words from
the aggregate store path so they are copied by the surrounding `goc_memcpy`
instead of republished through `goc_storep`. Pointer maps, stack maps and
emitted type descriptors are unchanged: `pointerWordIndices` and
`visitPointerWords` still report every pointer word.

Covered by `TestNotInHeapPointerStoreSkipsWriteBarrier` in `goc/escape_test.go`,
which fails on the pre-fix compiler, and by the capability
`gc/span-metadata-barrier` (`runtime_span_metadata_barrier.go`). That reducer is
the one program in the matrix that raises `GOMAXPROCS` itself, because the
matrix's `-runtime-procs` only reaches 4. It is *not* deterministic: measured
against the pre-fix compiler it failed 22 of 30 runs, and against the fixed
compiler it passed 30 of 30. Treat the compiler test as the exact guard and the
capability as the end-to-end one.

#### What is still open

- `goc_storep` barriers every pointer store whose destination is neither a heap
  span nor a runtime-managed stack span. That set includes globals, which is
  intended, but it also includes any non-Go-managed stack, which is not. The
  deletion half of the barrier reads the destination slot's previous contents,
  so an uninitialized slot on such a stack publishes whatever bytes were there
  as a pointer. Honouring `NotInHeap` removes the instance found here; the
  general hole is untouched. Making it precise needs either a data-section range
  test in `goc_storep` or keeping frame-local aggregate destinations
  syntactically recognisable -- `assignLocal` currently loses the alloca
  identity by loading the indirect slot, which is why a plain frame local
  reaches the dynamic classifier at all.
- cg12's pointer maps still include not-in-heap pointer words, where upstream's
  `PtrDataSize` reports zero. That was left alone deliberately: it is a metadata
  change with a much wider blast radius than the barrier fix, and it is not
  required for this failure. It costs the collector wasted `findObject` calls
  into `fixalloc`/`persistentalloc` memory, and it leaves the same
  "address inside a span but not an object" hazard reachable from a stack map --
  gated, as upstream is, by the allocator paths being non-preemptible.
- Found while trying to build a deterministic reducer, not investigated and
  **not** caused by this bug: a program that creates goroutines in bulk while a
  concurrent `runtime.GC()` runs dies with `runtime: pointer 0x... to
  unallocated span ... found in object at ...` where the containing object is an
  `mSpanManual` span, i.e. a goroutine stack. It reproduces identically before
  and after this change. Two candidate reducers had to be abandoned because of
  it.

Exit criterion: the two `cmd/goc` tests and the §5.3 cleanup trio pass at high
`GOMAXPROCS`, no store of a not-in-heap pointer reaches `goc_storep`, and the
remaining `goc_storep` imprecision above is either closed or shown to be
unreachable.

### 5.8 The `runtime.KeepAlive` global root (the "found bad pointer" gap)

Current checkpoint:

- [x] Reduce `fatal error: found bad pointer in Go heap` to a single compiler
  defect: cg12 rooted every `runtime.KeepAlive` value in a process-global
  pointer word, so a kept-alive object that escape analysis left on the stack
  published a goroutine stack address to a permanent GC root.
- [x] Move the keep-alive slot from a `.goc.keepalive.N` data symbol to a
  pointer-marked frame allocation, one per kept-alive variable, created in the
  entry block in source order and zeroed there.
- [x] Add `GODEBUG=cg12checkwb` write-barrier validation, which turns the
  invariant violation into a throw at the store that commits it.
- [x] Land the reducer as the `gc/keepalive-stack-root` capability. After all of
  wave 3 merged the matrix is 338 of 338 at `STATUS_SHARDS=8`, with
  `defer-panic/panic-string-output` (deliberate `expectedFailure`) as the only
  declared exception and no `knownGap` remaining.
- [x] Explain the residual `fatal error: found pointer to free object` recorded
  below. It was two unrelated compiler defects, not one, and neither is this
  section's bug: see §5.11 and §5.12. The description below of it as "a
  different fault with a different traceback" is right; the description of it as
  two orders of magnitude rarer is not, because it and
  `found bad pointer in Go heap` are the same two mechanisms seen through
  different stale words.

#### Root cause (2026-07-28)

cg12 has no source-level notion of a variable, so it cannot end a value's
liveness at a `runtime.KeepAlive` call by liveness analysis alone. It kept the
value alive by storing it somewhere the collector scans: `keepAliveSlot` created
a module data symbol

```go
&ir.Data{Name: ".goc.keepalive.N", Align: 8, Items: ..., PointerWords: []int{0}}
```

wrote the value into it through `goc_storep` at every assignment to the
variable, and wrote nil into it after the `runtime.KeepAlive` call.

`PointerWords: []int{0}` makes that word a GC root for the whole process. Two
independent defects follow, and both are visible in a single program:

- **The slot is shared.** One global per (function, variable) is shared by every
  goroutine executing the function, so concurrent calls overwrite each other's
  value. `runtime.KeepAlive` in a function running on several goroutines
  therefore did not reliably keep anything alive.
- **The slot can hold a stack address.** Escape analysis leaves a kept-alive
  object on the stack -- that is the normal case, since `runtime.KeepAlive` is
  cg12's only reason to think the value outlives its last ordinary use. Storing
  its address into a global breaks Go's invariant that no global holds a stack
  pointer: nothing relocates the word when `copystack` moves the frame and
  nothing clears it when the goroutine exits. The word is stale from the first
  stack growth onwards, and the collector faults as soon as the old stack's span
  has been returned from the stack pool to the heap, where its state becomes
  `mSpanDead`.

Both crash sites in the field reports are the same stale word seen from
different directions. `runtime.wbBufFlush1` throws when the nil-store's deletion
barrier buffers the stale *old* value; the root scan throws when a collection
reaches the global first. That is why the pointer was always "reported with no
containing object": `wbBufFlush1` calls `findObject(ptr, 0, 0)` with no
referent, and a global root has none either.

The address shape in every captured failure confirms it. The reported pointer is
always 0xb8 below an 8 KiB stack boundary, in a span with `state=0` and
`elemsize` 4096 or 8192 -- a released goroutine stack, at the frame offset a
top-level goroutine function's local lands on.

#### The fix

`gen.keepAliveSlots` now maps each kept-alive variable to an `ir.Ref` frame
allocation instead of a symbol name. `declareKeepAliveSlots` reserves them in
the entry block, in the source order `findKeepAliveObjects` now returns, and
zeroes each one; `trackKeepAliveAssignment` and `keepAlive` store through
`gen.store`, which takes the direct-store path because the destination is a
known stack address.

Three properties fall out. The slot is per-goroutine, so the sharing defect is
gone. It is a frame word the stack maps describe, so `copystack` relocates it
along with everything else in the frame. And it needs no write barrier at all,
so no keep-alive store can put anything into the write barrier buffer.

The slot is a per-safepoint precise root -- `addressOnlyOperand` already treats
a `goc_storep`/store destination as an address-only use, so the allocation is
not forced conservative by §5.3's `frameEscapingAllocations`. Its live range
runs from the entry-block allocation to the nil store after the
`runtime.KeepAlive` call, which is exactly the interval the value must survive.
Zeroing at entry matters for the same reason: the slot is reported as a root
from the allocation onward, which precedes the first real store to it.

#### The write-barrier diagnostic (2026-07-28)

`GODEBUG=cg12checkwb=1` validates both words a pointer write barrier is about to
buffer -- the slot's previous contents and the value being stored -- against
exactly `findObject`'s acceptance rule, and throws at the store rather than at
the flush, so the traceback names the function that performed the write instead
of the background marker that happened to drain the buffer. It lives in
`runtime.atomicwb` (`stdlib/src/runtime/atomic_pointer.go`), with the checking
and reporting in `stdlib/src/runtime/mwbbuf.go`.

`cg12checkwb=2` additionally rejects a store of a goroutine stack address into a
module's data or bss. That converts this section's race into a deterministic
failure: the reducer below throws on every run at `=2` and roughly one run in
three without it.

Both report on the system stack, because every caller is on a nosplit path and
the goroutine's nosplit reserve is not large enough to print from. An earlier
version printed in place and corrupted the stack it was reporting on.

#### Verification

The reducer is `goc/testdata/runtime_keepalive_stack_root.go`, landed as the
`gc/keepalive-stack-root` capability. Its header records why each ingredient is
needed. Failures before the fix, against the same binary after it, all with
`GOGC` and `GOMAXPROCS` set by the program itself:

| Configuration | Before | After |
| --- | --- | --- |
| `-O`, `-runtime-procs=1` | 129 / 400 | 0 / 3000 |
| `-O`, `-runtime-procs=4` | 127 / 400 | 0 / 3000 |
| unoptimized, `-runtime-procs=1` | 106 / 400 | 0 / 3000 |

And on the capability this was found in, `goroutine/many-goroutines-gc`, `-O` at
`GOMAXPROCS=4` with `GOGC=10`, 16000 runs before against 24000 after:

| Outcome | Before (16000 runs) | After (24000 runs) |
| --- | --- | --- |
| `found bad pointer in Go heap` | 449 | **0** |
| `panic: many goroutines GC result mismatch` | 2 | 0 |
| SIGSEGV | 1 | 0 |
| `marked free object in span` | 2 | 3 |

The two wrong-result panics are the sharing half of the defect showing through
as a wrong answer rather than a fault. The residual-fault rate is unchanged; see
below.

Independent verification on 2026-07-28 reran this at 24000 runs on each side and
reproduced the headline result exactly — 472 against **0** — but did not
reproduce the last row: it saw `marked free object in span` zero times in about
600000 executions, and the fault that actually survives at this scale is
`found pointer to free object` (11 of 24000 before, 4 of 24000 after). Read the
last row as misattributed rather than as a second measurement.

The deterministic guard is
`TestKeepAliveStoresIntoAFrameSlotRatherThanAGlobal` in `goc/escape_test.go`,
which asserts that no `.goc.keepalive.` data symbol is emitted and that the
kept-alive value is stored into a frame allocation carrying a pointer word. It
fails on the pre-fix tree with the symbol name in the message.

For the general property rather than this one instance: all 352 programs in
`goc/testdata` were compiled with `-O` and run once under
`GODEBUG=cg12checkwb=2` at `GOMAXPROCS=4`, and none stored a goroutine stack
address into a global. That is a single run per program, so it is a sweep for
other instances of the same class, not a proof that none exists.

#### What is still open

- `goroutine/worker-fanin-gc`, the capability the §5.2 checkpoint names as the
  original sighting, **no longer reproduces at all on this tree**: 0 of 4000
  runs, `-O` at `GOMAXPROCS=4` with `GOGC=10`, both before and after this
  change. It contains no `runtime.KeepAlive`, so this section's defect cannot
  have been its cause. Its 7-of-60 failure was measured on 381f67c and was
  presumably closed by §5.2.1 or §5.7. The §5.2 checkpoint text still describes
  it as live and should be read with that in mind.
- A different fault survived, **pre-existing and unaffected by this change**:
  `fatal error: found pointer to free object`. It is now closed by §5.11 and
  §5.12, and two things this section said about it were wrong. First, the
  correction above — that `marked free object in span` was a misattribution and
  the real string is `found pointer to free object` — was a correction in the
  wrong direction: `mspan.reportZombies` *prints* the first line and then
  *throws* the second, so they are one report and either name is the same fault.
  A verification that greps for one and not the other will see zeroes on one
  side, which is what happened. Second, "two orders of magnitude rarer" counted
  only one of the two messages the same defects produce; measured together
  (§5.12's table) the merge base loses 161 runs in 160000 on the KeepAlive-free
  control, not 20.

Exit criterion: no global data word ever receives a goroutine stack address
(checkable with `GODEBUG=cg12checkwb=2` over the corpus), `gc/keepalive-stack-root`
passes at every `-runtime-procs` setting optimized and unoptimized, and the
`found pointer to free object` fault above is reduced and attributed. All three
now hold; the last one is §5.11 and §5.12.

### 5.9 Fixed: loop variables were per-loop, not per-iteration (2026-07-28)

Go 1.22 made the variables a three-clause `for` declares in its init statement,
and the iteration variables a `range` clause declares, **per-iteration**. cg12
gave each such variable one storage slot for the whole loop, so every closure
created in the body observed the last iteration's value. That is a silent
miscompile of very ordinary Go -- goroutines started in a loop, closures
deferred in a loop, callbacks appended to a slice in a loop, `&loopvar` -- and
it was not detected by anything in the matrix.

Measured against the host toolchain before any change, cg12 was wrong for:

- three-clause `for` with `i := 0` (host `0 1 2`, cg12 `3 3 3`);
- `for i := range n`, `for k, v := range slice`, `for k, v := range array`,
  `for i, r := range string`, `for k, v := range map`, `for v := range chan`;
- every capture route: goroutine, `defer`, append-to-slice, and `&i`;
- every value shape: scalar, string, interface, slice, struct, array.

and already correct for:

- range-over-function iterators. Their body is lowered into a yield function
  that is entered afresh per element, so the iteration variables are function
  parameters and are naturally per-iteration.

A second, related defect fell out of the same reduction: for a slice, array,
string, or integer `range`, the **key variable's own slot was the loop
counter**. Assigning to the key inside the body therefore changed the
iteration -- `for k := range []int{1,2,3} { if k == 0 { k = 5 } }` ran one
iteration instead of three -- and after an assigning `for k = range x` the
variable held `len(x)` rather than the last index.

#### The fix

`goc/compile.go` only lowers a variable per-iteration when the variable's
storage can outlive the iteration, which is exactly the condition
`variableStorage` already tests: `escapingCaptures`, i.e. captured by an
escaping closure or address-taken into something that escapes. A variable that
stays in a frame slot cannot be observed after its iteration ends, so sharing
one slot is unobservable and the loop keeps allocating nothing. This is the
same syntactic over-approximation the standard compiler's `loopvar` pass uses.

- `perIterationForStmt` performs the standard compiler's three-clause rewrite:
  the init statement's storage becomes a carrier, each iteration allocates its
  own instance and copies the carrier in, the post statement moves to the top
  of the loop and is skipped on the first pass, and the iteration's value is
  copied back to the carrier at the `continue` target.
- Every `range` form allocates the declared iteration variables' storage at the
  top of the body, before the clause assigns them.
- The indexed `range` forms now keep a private loop counter, so the key
  variable is an ordinary assignment target.
- The per-iteration cell is allocated with `allocateEscapingTyped`, not the
  promotable `OHeapAlloc` candidate form. `opt.LowerHeapAllocations` decides
  whether a pointer outlives the *frame*, not whether it outlives one
  *iteration*, so promoting a per-iteration cell to a frame slot would silently
  put every iteration back on one slot.

This agrees with §5.2.1: a `defer` inside a loop already took the
"registers more than once" path, so it already had a fresh heap descriptor and
heap-lifted captures per registration; per-iteration scoping gives those
captures the right *value*. A `defer` that registers at most once is not in a
loop, so nothing about it changed, and
`TestConditionallyDeferredFunctionLiteralDoesNotAllocateBeforeItsBranch` still
holds `runtime.gcAssistAlloc` at zero allocations.

#### Cost

- **No runtime hot path allocates.** Instrumenting the compiler to log every
  per-iteration variable found **zero** sites in `runtime` and in everything
  reachable from `runtime_core_types.go`, `runtime_many_goroutines_gc.go`,
  `runtime_scheduler_gc_churn.go`, `stdlib_netpoll_stress_pipe_close_churn.go`,
  `reflect_makefunc.go`, `fmt_sprintf.go`, `context_cancel.go`, and
  `stdlib_net_netip.go`. Across the largest programs that compile in reasonable
  time (`stdlib_crypto_ecdsa.go`, `stdlib_encoding_gob_roundtrip.go`,
  `stdlib_http_cookiejar.go`) exactly three stdlib sites qualify:
  `crypto/tls/ech.go:153` and `crypto/tls/handshake_client.go:1269` (both
  `return &loopvar`, where the standard compiler also allocates per iteration),
  and `internal/poll/writev.go:42`, where cg12's escape analysis treats
  `&chunk[0]` as taking the address of the slice header `chunk` rather than of
  its backing array. That last one is pre-existing conservatism -- `chunk` was
  already heap-lifted before this change -- but it now costs one cell per
  iovec instead of one per `Writev` call. Narrowing `findEscapingCaptures` for
  `&slice[i]` would remove it and is not attempted here.
- A per-iteration variable also keeps the one cell `variableStorage` allocates
  before the loop starts, which becomes the three-clause loop's carrier and is
  dead immediately in the `range` forms. That is one allocation per loop
  *execution*, not per iteration, and it keeps the per-iteration cell's
  representation -- indirect for aggregates, inline header for strings and
  interfaces -- identical to the one every other part of the frontend expects.
- **Compile time and compiler memory are unchanged**: three interleaved runs of
  each compiler on `stdlib_encoding_gob_roundtrip.go` gave 13.13/13.09/12.98 s
  and 745/746/739 MB peak for the base compiler against 12.99/12.98/13.01 s and
  739/738/751 MB after, on a box under other load.

#### Checkpoint

- [x] Reduce every loop form against the host toolchain and record which cg12
  gets wrong; land the reducers as the `loop-variables` capability category
  (`three-clause`, `range-forms`, `goroutine-and-defer`, `address-gc`,
  `value-shapes`, `shared-scope`). All six fail on c87ec2d and pass after the
  fix, normally and with `-O`.
- [x] Keep the boundary from the other side: a variable declared outside the
  loop, and an assigning `for k, v = range x`, keep one instance
  (`loop-variables/shared-scope`, `TestAssigningRangeClauseKeepsOneInstance`).
- [x] Prove the cost model in unit tests:
  `TestUncapturedLoopVariableAllocatesNothing`,
  `TestCapturedLoopVariableIsAllocatedInsideTheLoop`,
  `TestOneCapturedScalarCostsOneAllocationPerIteration`, and
  `TestPerIterationCellSurvivesHeapAllocationLowering`.
- [x] Verified on 2026-07-28: `make test-unit`, `go test -timeout 40m ./goc/...`
  (869.7 s), `make test-goc-cmd`, and the full matrix at `STATUS_SHARDS=4`, all
  green. Re-verified after all of wave 3 merged: 338 of 338 at
  `STATUS_SHARDS=8`, with `defer-panic/panic-string-output` the only deliberate
  expected failure and no `knownGap` remaining. goc output is byte-identical to
  the host toolchain across nine capture shapes in all six
  optimization/GOMAXPROCS configurations, where the pre-fix compiler differs on
  nine of ten lines.

`println` with several operands not printing the spaces the spec requires
(`println("a", 1)` gave `a1`) was found while reducing this bug and is unrelated
to it; it is fixed in §21. Two further loop-related defects this work exposed --
the non-identifier range key and the `runtimeAllocation` gating -- are carried in
§5.10 with the rest of Phase 1's open items.

### 5.10 Open items carried out of Phase 1 (2026-07-28)

Phase 1 closed every failing capability, but the work surfaced defects and gaps
that no capability covers. They are recorded here because a green matrix will not
find them again, and because three of them were nearly lost: they existed only in
the reports of the jobs that found them.

#### Known miscompiles, not covered by any capability

- **A closure that assigns to a captured string variable leaves it pointing at
  the closure's dead frame.** Fixed 2026-08-01, §5.15.

- **A few package globals are emitted twice.** `runtime.divideError`,
  `runtime.overflowError` and `internal/runtime/maps.errNilAssign` each get a
  zeroed placeholder datum and then a second datum, under the same name, holding
  the itab and value. `obj.prepareELF` keys its symbol index by name and keeps the
  last, so every reference resolves to the real one and the placeholder is dead
  bytes -- but the object genuinely defines one name twice, which the system
  linker rejects outright the moment such a symbol is global. Found by §16, which
  works around it rather than fixing the emission. The cause is in `globalDecl`'s
  interface path and has not been traced.

- **Per-iteration loop lowering is gated on `g.runtimeAllocation`.** With that
  mode off, escaping captures are not heap-lifted at all, so loops keep the
  pre-§5.9 shared-slot behaviour — that is, the miscompile returns. `goc` always
  compiles with it on, so this is latent rather than live, but it is an
  undocumented correctness cliff: any future caller that turns the mode off
  silently loses Go 1.22 loop semantics.

#### Residual runtime faults

- **`fatal error: found pointer to free object`** is closed. It was two
  independent compiler defects, reduced and fixed in §5.11 and §5.12. Read the
  measurement in §5.12 before trusting any earlier rate quoted for it: this
  fault and `found bad pointer in Go heap` share both mechanisms and differ only
  in what the stale word happened to address, so counting either one alone
  understates the rate.

- **Rare hangs and deadlocks.** The §5.8 verification saw runs exceed a 60s
  timeout on both the base and the fixed compiler, at a low rate, unexplained
  and unattributed. §5.11's and §5.12's campaigns reproduce this — 1 timeout in
  160000 and 2 in 2000 on the base compiler — and also saw
  `fatal error: all goroutines are asleep - deadlock!` at 7 in 160000 on the
  base compiler and 3 in 160000 after §5.11 alone. Both are absent from every
  post-§5.12 campaign (160000 + 2000 + 800 runs), which is consistent with a
  lost closure hanging or deadlocking the program rather than faulting it, but a
  zero at that scale is not an attribution and neither has a reducer. Any
  harness that measures fault rates on `many-goroutines-gc` must still impose a
  per-run timeout or it will stall instead of reporting.

#### Compiler bloat and conservatism

- **`materializeNilInterface` is emitted for every interface argument at every
  call site** — a compare, two extra blocks, a phi, and a zeroed 16-byte alloca.
  The nil check is provably dead whenever the operand is an alloca address, which
  is the common case. This is real bloat throughout the runtime and the standard
  library, and removing it is a frontend change with a large blast radius, so it
  was deliberately not bundled into §5.3's fix.

- **`internal/poll/writev.go`'s `&chunk[0]`** is treated as addressing the slice
  header, so `chunk` is heap-lifted. Pre-existing conservatism, but since §5.9 it
  costs one cell per iovec rather than one per `Writev`. Narrowing
  `findEscapingCaptures` for `&slice[i]` would remove it.

- The §5.3 escape boundary's `OSel` derivation path is covered by unit test only;
  no capability's `-O` build keeps the diamond rather than if-converting it.

- **`runtime.printfloat32`, `printfloat64`, `printcomplex64` and
  `printcomplex128` still heap-allocate their scratch array**, which is exactly
  the §5.3 defect for the four print routines §5.3 did not reach. They differ
  from `printuint` in shape: `gwrite(strconv.AppendFloat(buf[:0], v, 'g', -1,
  64))` passes the derived slice to a callee that *returns* a slice derived from
  it, and cg12 has no rule for a parameter that leaks only to its result, so the
  call site assumes the worst and `buf` is lifted. The host toolchain's
  `-gcflags='runtime=-m -m'` reports nothing in `runtime/print.go` escaping at
  all. Reduced without any runtime source:

  ```go
  func passthrough(dst []byte) []byte { return dst }
  func viaReturn() { var buf [20]byte; consume(passthrough(buf[:0])) }
  ```

  `viaReturn` calls `runtime.newobject`; the same function with
  `consume(buf[:0])` does not. Found by §21, which routes complex operands to
  these routines; it is pre-existing for the two float ones. Closing it means
  giving the escape walk a "leaks only to result" summary and continuing the
  walk from the call expression, which is a frontend change with a large blast
  radius — a wrong summary stores a stack pointer into the heap — so it wants
  its own validation cycle rather than being folded into §21.

#### Compiling the same program twice does not give the same binary

#### The `-runtime-opt` arm of the matrix does not link

Measured 2026-07-31 on `main` (`61b96da`) and on `ccwork/println-spacing`, four
shards each, failure sets byte-identical: 322 pass and **16 fail** under
`-runtime-opt`, every one of them the same link error.

```
goc-program-runtime.o: in function `reflect_makeFuncStub_abi0':
undefined reference to `reflect_moveMakeFuncArgPtrs'
undefined reference to `reflect_callReflect_abi0'
undefined reference to `reflect_callMethod_abi0'
```

Thirteen `runtime-packages/reflect-*`, `stdlib-crypto/ecdh-x25519`,
`stdlib-encoding/binary` and `stdlib-encoding/binary-varint`. The Go functions
that `reflect`'s assembly stubs call are not in the split runtime object when
`-O` is on. Unattributed and unfixed: it is not caused by §21, and the
determinism note above records that the matrix is supposed to run `-O` under
this flag.

**No longer reproducible as of 2026-08-01.** §24's job ran the full unsharded
`-runtime-opt` arm on `ccwork/reportzombies` (off current `main`) and got 345
subtests, 344 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP --- including all
thirteen `runtime-packages/reflect-*`, `stdlib-crypto/ecdh-x25519`,
`stdlib-encoding/binary` and `stdlib-encoding/binary-varint`. That job did not
fix it and did not attribute the change, so this stays recorded rather than
deleted: whoever fixed it should replace this paragraph with the cause.

#### Compiling the same program twice gives the same binary (closed)

Closed. §22 records the three causes, the measurement that closed it, and the two
claims this subsection used to carry that turned out to be wrong -- including its
first bullet, that 441 interface-call wrapper functions land in the module in a
different order on each compile, which does not happen and, on the compiler that
*was* irreproducible, did not happen either. `opt/inline.go:184`'s unstable
cost-inline tie-break is fixed there too; the sentence that said the matrix's `-O`
runs made it matter was wrong for a structural reason §22 measures. The matrix does
run `-O` under `-runtime-opt`; that part stands.

Two pieces of method are worth keeping here, because both of them cost a job a day.

**A behaviour comparison has to run a disagreement again before believing it.**
`goc/testdata/bytes_grow_compare.go` and `goc/testdata/bytes_grow_stats.go` print
allocation and GC statistics that move with scheduling: one single executable of
the first gave three distinct outputs in 21 runs at 7-way concurrency and the
second two in 21, while both gave one output in 12 serial runs on an idle box.
`bytes_grow_stats.go` showed this across three builds that were the same file byte
for byte, which is what makes it conclusive. `analysis/batchdiff` now re-runs a
disagreement before reporting it.

**Comparing two images by *content* rather than by bytes is still the right triage
for a contaminated compile**, now that bytes agree rather than because they do not.
Every defined symbol's name, size, kind and section, sorted, plus the image size:
a compile that gained, lost or resized a symbol is a different failure from one
that only moved something, and only the first is what a compile contaminated by a
previous compile looks like. `analysis/batchdiff` and `analysis/determinism` both
report it separately from the image digest.

- **An interior address is not a managed pointer, so it is invisible to the
  stack map (2026-07-31).** `ir.Block.Add(ir.ClsP, ...)` -- every field, index
  and indirection address -- produces a pointer-width value that is not a GC
  reference, while `Block.Load(ClsP, ...)` marks its result. Such an address
  live across a call is not adjusted by `copystack` and not seen by the
  collector. §5.13 hit this and fixes it where the assignment machinery
  deliberately holds an address across the right-hand side, but the rule is
  general: **nothing prevents another interior address from being held across a
  safepoint.**

  Not fixed generally here. Marking every `ClsP` result managed would change the
  GC root set of the whole compiler -- `arm64/regalloc.go` forces a GCRef
  temporary live across a safepoint to spill whole-life -- so it is a codegen
  and frame-size change that needs its own measurement, and a one-past-the-end
  address in a pointer slot would newly reach `findObject`. A cheaper first step
  is a checker: assert that no `ClsP`-classed value is live across a call
  instruction unless it is a GC reference, and see what the corpus reports.

#### The legacy stack map's root order is not reproducible, `cg12cc` only (2026-07-31)

Located by §22's static audit of every range-over-map in the tree, unfixed, and
provably unable to reach `goc`. (`opt/mem2reg.go` and `opt/jumpthread.go`, found by
the same audit and in the same class, *are* fixed --- §22 records them and their
limits.)

- **`arm64/mc.go:2786` orders a safepoint's frame roots in map order.** `gcRoots`
  ranges `m.f.StackPointerWords[id]`, a `map[int]bool`, so an allocation with two
  or more pointer words contributes its `rootFrame` entries in map order --- and
  `setStackMap` (`arm64/mc.go:458`) writes `sp.roots` to the `__cg12_stackmaps`
  section in slice order. A root set is a set, so this is a reproducibility defect
  and not a collector defect.

  It cannot reach `goc` either: `arm64/parallel.go:91` keeps a function's
  safepoints for that section only when `!goRuntime`, and a goc image confirms it
  --- `readelf -S` shows no stack-map section and `nm` no `__cg12_stackmaps`
  symbol. The Go-format stack maps goc does emit go through `goStackMapPoints`,
  which collects into a set and sorts.

#### Unmeasured

- The compile-time cost of §5.3's allocation-set fixpoint was never measured. The
  box it ran on was shared, so no timing measurement taken there would have been
  trustworthy.

- The stdlib per-iteration sites in §5.9 come from a sample of large programs,
  not the whole corpus; `stdlib_http_tls_client_server.go` exceeded the
  instrumented compile budget. There may be more.

- The matrix runs at `GOMAXPROCS` 1 through 4. Both §5.7 and §5.8 were invisible
  below `GOMAXPROCS=4`, and §5.7's was found only at 64. Raising the matrix
  ceiling was considered and rejected as a flakiness risk; the reducers instead
  set their own `GOMAXPROCS`. That keeps the coverage but leaves the general
  question open: defects that need many Ps are still structurally hard to see.

### 5.11 Fixed: the entry stack map described a never-started goroutine's argument frame (2026-07-28)

Recovered on 2026-07-31. This section and §5.12 are `ccwork/freeobject`'s, and
the `integration/wave4a` merges dropped them; the code they describe never
reached `main` either, so both were cherry-picked back rather than restored as
prose alone. One claim was re-measured here on current `main` rather than taken
on trust: `runtime_goroutine_entry_stack_map.go` at `-O`, 100 runs against the
compiler immediately before the fix and 100 against the compiler after it, gives
92/100 failures before and 0/100 after — the rate this section states. The
multi-thousand-run campaigns below were **not** re-run; they remain that
branch's own evidence.

Current checkpoint:

- [x] Build a fault-rate harness with a hard per-run timeout and parallel
  execution, and establish that `runtime: marked free object in span` and
  `fatal error: found pointer to free object` are **the same report**, not two
  faults: `mspan.reportZombies` prints the first line and then throws the second.
  §5.8's correction to itself was therefore a correction in the wrong direction.
- [x] Reduce the fault from an intermittent capability failure to
  `gc/goroutine-entry-stack-map`, which fails on about 92 runs in 100.
- [x] Name the first bad state: a goroutine `runtime.newproc` has created and not
  yet scheduled, whose closure wrapper frame is scanned precisely at
  `pc == entry` with an argument map that marks a word nothing has written.
- [x] Fix the metadata layer and prove it statistically over matched
  160000-run controls.

#### Root cause

cg12 starts every goroutine through a generated closure wrapper
(`gen.callClosure`, spelled `<pkg>_gowrap_<line>_<col>`). The wrapper takes no
Go parameters, but ABIInternal supplies its closure environment in X26, and
`goArgumentFrameFor` gives that register a home slot at argument-frame offset 0
so the stack-growth prologue can spill it around `morestack`. That slot is a
`prologueOffset`: only the callee writes it, and only on the path that grows the
stack.

`gometa.ArgumentStackMaps` put the prologue's map — home slots included — at
stack-map index 0, because `StackMapPCData` selects index 0 for the whole
prologue range. The comment on `FunctionStackMaps` already recorded the hazard
and guarded the one case it saw: a *body* safepoint must never resolve to index
0, because it would inherit those never-written words.

The case it did not see is that the runtime reaches index 0 by a second route.
`runtime.stkframe.getStackMap` short-circuits the PCDATA lookup when a frame's pc
is exactly the function entry and hardcodes index 0 — "we want to use the entry
map (-1), even if the first instruction of the function changes the stack map".
A goroutine created by `newproc` and not yet scheduled is in exactly that state:
`gostartcall` leaves `sched.pc` at the wrapper's entry and `sched.sp` at
`stack.hi-48`, and `newproc1` initialises only the two words at and below `sp`.
The home slot sits at `sp+8`, which is `frame.argp+0` once `gentraceback`
computes `fp = sp` and `argp = fp + MinFrameSize`. Nothing has ever written it,
so the precise scan read whatever the previous user of that recycled stack left
there and marked it.

Upstream Go is safe here by construction rather than by care: the function a Go
goroutine starts at has an argument frame of size zero, and the closure is kept
alive through `gp.sched.ctxt`, which `scanstack` scans separately.

What the stale word addressed decided which fatal message appeared, which is why
these must be counted together:

- a released goroutine stack or an unallocated span →
  `fatal error: found bad pointer in Go heap`, thrown at the scan;
- a free object in a live span → the mark bit is set on a free object, and one
  sweep later `mspan.reportZombies` throws
  `fatal error: found pointer to free object`;
- a free object whose span the sweeper then counts as allocated → the object is
  handed out twice and the program returns a wrong answer with no fault at all.

#### The fix

`gometa.FunctionStackMaps` now appends a *growth* pointer map, distinct from the
entry map, whenever a function's entry and safepoint argument maps differ — that
is, whenever it has register home slots. `StackMapPCData` selects that index for
the prologue range, `ArgumentStackMaps` gives it the full argument map, and every
other index, index 0 included, gets the caller-initialised subset.

This works because the runtime never consults the PCDATA table at `pc == entry`;
it hardcodes index 0 there. So index 0 can mean what upstream means by "the entry
map" — the argument frame as the caller left it — while the prologue window keeps
its own map for `morestack`. The growth map's locals are empty, which is correct:
`funcspdelta` is 0 throughout the prologue, so the frame does not exist yet and
`getStackMap` scans no locals there.

The deterministic guard is `TestGoEntryArgumentMapOmitsTheClosureHomeSlot` in
`arm64/unit_test.go`, with `TestGoArgumentStackMapsKeepPrologueOnlyWordsOffTheEntryIndex`
and `TestGoStackMapPCDataSelectsTheGrowthIndexInThePrologue` in
`internal/gometa/gometa_test.go` covering the two halves separately.

#### Verification

The reducer is `goc/testdata/runtime_goroutine_entry_stack_map.go`, landed as
`gc/goroutine-entry-stack-map`. Its header records why each ingredient is needed.
400 runs per cell, same source file, merge-base compiler against fixed compiler.
The middle column is this section's fix alone, which is what shows that the
unoptimized build still had §5.12 underneath it:

| Configuration | merge base | §5.11 only | §5.11 + §5.12 |
| --- | --- | --- | --- |
| `-O` | 369 / 400 | 0 / 400 | **0 / 400** |
| unoptimized | 372 / 400 | 2 / 400 | **0 / 400** |

The two unoptimized survivors were `panic: sync: negative WaitGroup counter`,
which is the same lost-closure corruption §5.12 describes seen through a
different field of the reused object.

On a 200-round loop of the KeepAlive-free `many-goroutines-gc` variant §5.8 used
as its control, 2000 runs per column at `-O`:

| Outcome | merge base | §5.11 only | §5.11 + §5.12 |
| --- | --- | --- | --- |
| `found bad pointer in Go heap` | 1870 | 0 | 0 |
| `found pointer to free object` | 74 | 2 | 0 |
| wrong-answer panic | 1 | 0 | 0 |
| timeout (>120s) | 2 | 0 | 0 |
| clean | 53 | 1998 | **2000** |

The two survivors in the middle column were not individually attributed --
`reportZombies` cannot name its zombie, see below -- but they are gone once
§5.12 lands, and their rate matches §5.12 rather than this section. Two in 2000
processes over 200 rounds each is one fault in 200000 rounds, while the same
defect measured on the single round of §5.12's table is one in 800 *processes*:
§5.12's fault is per-process, not per-round, because it needs goroutines created
while a collection is running and one round creates them once. That is also why
§5.11's fix alone does not move the single-round control at all.

#### What this section could not determine

`mspan.reportZombies` could not name the zombie object on any span the Green Tea
collector gives inline mark bits, which is every span with `elemsize` between 16
and 512 — including the 32-byte spans this fault lands on. That is why the
residual above had to be attributed with `GODEBUG=gccheckmark=1` instead.

**Fixed on 2026-08-01; see §24.** It was an upstream defect, in upstream's own
code on upstream's own default configuration, and it reproduces on the host
`go1.26.1` toolchain with no cg12 involved. The two runs this section reports as
"not individually attributed" were not re-run, so they stay unattributed; what
changed is that the next one need not be.

### 5.12 Fixed: unsafe.Pointer stores lost their write barrier (2026-07-28)

Current checkpoint:

- [x] Attribute the residual §5.11 left behind, using `GODEBUG=gccheckmark=1` to
  turn a lost object into a throw naming the reference that reached it.
- [x] Reduce it deterministically at the compiler, to two functions differing
  only in a field's type, one of which loses its barrier.
- [x] Fix the frontend and prove it statistically over matched 160000-run
  controls.
- [ ] There is **no runtime capability for this defect on its own**. Its rate is
  about one process in 800 even on the program built to provoke it, which is far
  too rare for the matrix; `gc/goroutine-entry-stack-map` catches it only
  incidentally, at 2 runs in 400 unoptimized. What guards it is the compile-time
  test below. A runtime reducer that concentrates `newproc` calls into the mark
  phase would be worth building and was not attempted here.

#### Root cause

`gen.store` chose the store *width* before it chose the *barrier*:

```go
if sub, ok := subOf(t); ok {
    g.cur.StoreSub(sub, v, addr)
    return                       // barrier check never reached
}
class, _ := scalar(t)
if g.runtimeAllocation && !g.noWriteBarrier && class == ir.ClsP && ... {
    g.cur.CallVoid(g.fn.Sym("goc_storep", 0), addr, v)
```

`subOf` reports a width for the integer kinds *and* for `types.UnsafePointer`,
and `unsafe.Pointer` is the only type it accepts that `scalar` classifies as
`ir.ClsP`. So exactly one type slipped through: every store of an
`unsafe.Pointer` into the heap was emitted as a plain store with no write
barrier. Two functions differing only in a field's type make it visible —
`h.ordinary = p` for a `*cell` emits `goc_storep`, `h.opaque = p` for an
`unsafe.Pointer` emitted `storel`.

The consequence in the runtime is `runtime.gostartcall`:

```go
buf.ctxt = ctxt
```

`gobuf.ctxt` is declared `unsafe.Pointer` precisely so that writes from Go get
write barriers — its own comment in `runtime2.go` says so, because the assembly
that also touches it cannot. `newproc1` reaches it through `gostartcallfn` and
publishes the new goroutine's funcval, which cg12 allocates immediately before
with `runtime.newobject`, into a `g` the collector may already have blackened.
White into black with no barrier is a lost object: the funcval is freed while
`sched.ctxt` still names it, so the goroutine starts with its closure register
pointing at freed memory. That yields wrong arguments (the wrong-answer panics),
and the freed object being marked again later yields
`fatal error: found pointer to free object`.

This is the direction §6 asks about and §5.7 and §5.8 did not cover: those two
were cg12 emitting a barrier where upstream omits one. This is cg12 omitting one
upstream emits, which is strictly worse.

#### The fix

The barrier decision now precedes the sub-width store in `gen.store`. Nothing
else changes: `unsafe.Pointer` is the only type whose lowering moves, and
`isNotInHeapPointer` still keeps §5.7's not-in-heap pointers out of the barrier.

The deterministic guard is `TestUnsafePointerStoreKeepsWriteBarrier` in
`goc/escape_test.go`, which asserts the barrier on an `unsafe.Pointer` field, the
barrier on an ordinary pointer field, and no barrier on a `uintptr` field, so it
fails on the pre-fix tree and would also fail if the fix over-reached.

Aggregate stores were already correct and are unchanged: `walkPointerWords`
already treats an `unsafe.Pointer` field as a pointer word, so a struct copy
barriered it and the emitted pointer maps and type descriptors always described
it. Only the scalar store path was wrong.

The added barriers were checked for the opposite failure -- §5.7's, a barrier on
a pointer the marker cannot interpret -- by running the 49 `gc`, `finalizer` and
`cleanup` capability programs at `-O` under `GODEBUG=cg12checkwb=2` at
`GOMAXPROCS=4`. None fired. That is one run per program, so it is a sweep rather
than a proof.

#### Verification

Matched 160000-run campaigns on the KeepAlive-free `many-goroutines-gc` variant
-- `goc/testdata/runtime_many_goroutines_gc.go` with its one
`runtime.KeepAlive(root)` line deleted, which is the control §5.8 used and is not
itself a capability -- at `-O`, `GOMAXPROCS=4`, `GOGC=10`, from one source file
compiled by each compiler, with a 60s per-run timeout:

| Outcome | merge base | §5.11 only | §5.11 + §5.12 |
| --- | --- | --- | --- |
| `found pointer to free object` | 101 | 126 | **0** |
| wrong-answer panic | 47 | 73 | **0** |
| `found bad pointer in Go heap` | 5 | 4 | **0** |
| `all goroutines are asleep - deadlock!` | 7 | 3 | **0** |
| timeout (>60s) | 1 | 0 | **0** |

The middle column is what makes the attribution: §5.11's fix does not move this
program at all, because one round of it creates its goroutines once and the
§5.11 fault needs recycled goroutine stacks. The 126-against-101 and 73-against-47
differences are in the same direction and larger than one would like; they are
not explained here, the box was shared and loaded for both campaigns, and no
timing-derived claim is made from them.

Under `GODEBUG=gccheckmark=1`, the §5.11-only compiler threw
`fatal error: checkmark found unmarked object` on 3 of 8000 runs, naming
`gp.sched.ctxt` as the reference and a 32-byte object whose first word was the
`main_gowrap_35_6` code pointer — the goroutine closure — as the lost object.
That is the observation the reduction was built from.

### 5.13 Fixed: assignment destinations that were not identifiers (2026-07-28)

A `range` clause that assigns with `=`, and every other statement that stores a
value it did not compute from an expression, resolved its destination with its
own private rules. Each of those rules understood a local identifier and little
else, so a destination that was anything more interesting was mishandled -- in
the `range` case, dropped in silence.

#### What was wrong

Measured against the host toolchain across 86 differential programs covering the
cross product of range subject (slice, array, pointer-to-array, string, map,
channel, integer, range-over-function) against destination form (struct field,
nested field, slice index, array index, map index, pointer indirection,
package-level variable, blank, and the mixed cases where one side is an
identifier and the other is not), cg12 was wrong for:

- **every non-identifier destination in every range form.** `rangeVariableObject`
  returned nil for anything that was not an `*ast.Ident`, the caller then left
  the destination slot empty, and the clause stored nothing. The loop iterated
  the right number of times and the target was never written.
- **every package-level destination except in the map form.** The indexed and
  channel forms called `variableStorage` unconditionally, which does not consult
  `g.globals`, so the global was given a *fresh frame slot*; the
  range-over-function form gave it a local in the yield function instead. This
  is the reading a same-function check cannot make: `for gi = range s` followed
  by `fmt.Println(gi)` prints the right answer, because the read resolves to the
  same frame slot the write went to. Only a second function reading the global
  shows that the symbol was never written. The map form was already correct; it
  consulted `g.addr` first.
- **a `range` element destination whose type differs from the element type.**
  `for _, x = range []int{1,2,3}` with `x` an interface used the *destination's*
  type to compute the element's size and representation, so it read 16 bytes of
  an 8-byte element and never boxed the value. It crashed in `reflect`.
- **an element variable of string type**, which was made to point *into* the
  range expression's backing array rather than hold a copy: after
  `for i, s := range strs { strs[i] = "z"; use(s) }` cg12 saw `zzz` where the
  host sees `abc`. This one needs no non-identifier destination at all.
- **the two-phase assignment order.** `for k, m[k] = range "abc"` must index m
  with the key the clause is about to overwrite, and `k, a[k] = 3, 4` must do the
  same. cg12 evaluated each destination immediately before storing into it, so
  the second destination saw the first destination's new value -- in the ordinary
  tuple assignment that wrote past the end of a two-element slice.
- **the order of the left operands against the right-hand expressions.** Go
  evaluates a statement's left index expressions and pointer indirections first;
  cg12 ran the right-hand side first. `a[f(0)], a[f(1)] = g(5), g(6)` traced
  `[g5 g6 f0 f1]` where the host traces `[f0 f1 g5 g6]`, and the two-result call
  form traced the call before either index.
- **a map element as a destination of a tuple assignment.** `x.f, m[k] = a, b`
  went through `lvalue`, which computed the address as though the map header were
  a slice base, and stored through it. That is memory corruption, and it faulted.
- **`m[k] += v`**, which was routed to a map-assign helper that ignored the
  operator: `m["k"] += 5` on an entry holding 10 produced 5.
- **`v, ok = m[k]` with a non-identifier destination**, which was a hard compile
  error: `map lookup result target must be an identifier`.
- **a package-level string assigned by a two-result receive or by a `select`
  receive.** `assignResult` and `assignSelectValue` were missing the step that
  dereferences a descriptor symbol before copying into it, so the symbol's
  header pointer was overwritten with the address of another header and the next
  read of the variable panicked inside `fmt`. The same statement with a global
  slice, struct or interface destination was already correct, which is what made
  the divergence between these helpers and ordinary assignment easy to miss.

Already correct, and left alone: the blank identifier; a `select` receive into a
struct field or slice element; ordinary tuple assignment into a struct field or
slice element; `v, ok` from a channel receive or a type assertion into struct
fields; and a `range` element variable of struct type, which was already copied.

#### The fix

There is now one assignment destination in `goc/compile.go` --
`assignmentTarget`, with `prepareAssignmentTarget` and
`storeAssignmentTarget` -- and every statement that assigns a value it did not
compute from an expression uses it: the four range lowerings, ordinary tuple
assignment, multi-value call assignment, the two-result map lookup, channel
receive, type assertion, and `select` receive.

- It classifies a destination as discarded, a variable, a plain address, or a
  map element, and the map element is written through `mapAssignValue` rather
  than through an address.
- Preparing a destination evaluates the operands its address depends on and
  stores nothing. Every caller prepares all of its destinations before storing
  into any of them, which is what Go's two-phase assignment order requires.
- The store discipline is the one ordinary assignment already used, including
  the dereference of a package-level descriptor symbol, so the paths that had
  drifted are now the same code.
- `storeAssignmentTarget` converts the value to the destination's type, which
  boxes a concrete value assigned to an interface destination. Shared generic
  code is exempt because an unconstrained type parameter is already one
  pointer-sized value there.
- `declareRangeVariable` replaces `rangeVariableObject`. It allocates storage
  only when the clause really declares a variable, or when an assigned variable
  has none yet, so a package-level destination keeps its symbol.
- The `:=` path is untouched. `rangeTargets` still calls
  `startIterationVariable` for a declared variable that `perIterationVariable`
  reports, before preparing the destinations, so §5.9's per-iteration semantics
  and its cost model are unchanged.
- Where a key destination writes memory, the indexed range forms read the
  element before storing the key, because both may address the range expression
  (`for a[0], a[1] = range a`). That check costs nothing in the ordinary case,
  where the key destination is a variable or blank.

#### Checkpoint

- [x] Reduce the cross product of range subject against destination form against
  the host toolchain and record which cases cg12 gets wrong before changing
  anything; land the reducers as the `assignment-targets` capability category
  (`range-target-forms`, `range-target-order`, `multi-assignment-forms`). All
  three fail on `ff6ef9e` and pass after the fix, with and without `-O`.
- [x] Pin the mechanism in unit tests, not just the answer:
  `TestRangeClauseWritesAPackageLevelTarget` (every range form stores the
  symbol), `TestRangeTargetOperandsAreEvaluatedEveryIteration`, and
  `TestMapElementDestinationsUseTheMapRuntime`. All fail on `ff6ef9e`.
- [x] Keep §5.9 intact: `loop-variables/*` and
  `TestAssigningRangeClauseKeepsOneInstance` still pass, and the per-iteration
  allocation-cost tests are unchanged.

#### What it also broke, and the lower layer that was actually wrong (2026-07-31)

Resolving a destination before the right-hand side is what the specification
requires, and cg12 could not hold the address that produces. On the branch as
written, six capabilities regressed 20/20 to 0/20 --
`stdlib-encoding/gob-{int,struct-int,struct-mixed,roundtrip}`,
`runtime-packages/reflect-call-aggregate-probe`, and worst,
`stdlib-http/tls-client-server`, which corrupted the stream and reported
`tls: bad record MAC` rather than crashing.

`ir.Block.Add(ir.ClsP, ...)` -- how every field, index and indirection address
is built -- yields a pointer-*width* value, not a *managed* one. `ir/pointer.go`
draws that line deliberately: `ClsM`, or an explicit `MarkGCRef`, is what puts a
value in the frame's stack map, and `Block.Load(ClsP, ...)` marks its result
while `Block.Add` does not. A value the stack map omits is not adjusted by
`copystack`, so a right-hand side that grew the goroutine stack left the
prepared destination addressing the abandoned old stack. **The store then landed
in freed memory and the assignment was lost with no fault, no bounds error and
no write-barrier complaint.**

The first statement that hits is `reflect/abi.go:127`,
`a.valueStart = append(a.valueStart, pStart)`, with `a` addressing
`newAbiDesc`'s `var in abiSeq` -- a stack local, which is why this destination
and not another. `len(a.valueStart)` reads 0 immediately after the append.

Located by gating *which* destinations are prepared early on source position and
narrowing package, then file, then line; the bisect is in the table below. The
old code was never safe by rule, only by accident: it computed the address after
the right-hand side, so the window was always empty.

| variant | `reflect-call-aggregate-probe` |
| --- | --- |
| the branch as written | FAIL |
| destinations prepared *after* the right-hand side | PASS |
| only *identifier* destinations prepared early | PASS |
| only *non-identifier* destinations prepared early | FAIL |
| the branch + the prepared address marked managed | PASS |

`prepareAssignmentTarget` now marks the prepared address, the map header and a
pointer-classed map key managed. A scalar map key is deliberately left alone:
telling the collector an integer is a pointer is the opposite mistake. With
that, all six regressed capabilities pass, on this tree and on the branch's own
base.

The wider hazard -- *any* interior address held across a safepoint, not just an
assignment destination -- is carried in §5.10 rather than fixed here.

#### The coverage lesson

Nothing in the 338-capability matrix used a non-identifier range destination,
which is exactly why a wrong-answer bug in ordinary Go survived §5.9's
reduction of the same statement. The matrix covers *subjects* well and
*destinations* hardly at all, and the same asymmetry is what let the
package-level case hide: the obvious reducer reads the global back in the
function that wrote it, and that reads the frame slot the bug created. A
destination-shaped reducer has to read its result through a second function.

### 5.14 Held back: the Phase 2 escape change interacts with the goroutine entry fix

`ccwork/phase2-alloc` is **not merged.** It is correct in isolation and it is
held back because of an interaction, which is worth recording in full because no
per-change verification could have caught it.

That branch fixes `nonEscapingObjectUse`, which returned "does not escape" for
every field selection, so `&v.payload` kept its object on the frame and a
package-level slice could hold a goroutine stack address — the §5.8 invariant
reached by a new route. The fix is real and its own reducer proves it.

But merged together with §5.11 and §5.12, `gc/cleanup-basic` dies with
`fatal error: span has no free objects`.

**Re-measured against current `main` on 2026-07-31**, because `main` had moved a
long way since the original bisect and the interaction could have gone away.
`runtime_cleanup_basic.go`, 40 runs per tree:

| Tree | `gc/cleanup-basic` |
| --- | --- |
| `main` (`61b96da`) | passes |
| `main` + `phase2-alloc` alone | 0/40 failed |
| `main` + §5.13 (with its fix) + `phase2-alloc` | 0/40 failed |
| `main` + §5.11/§5.12 + `phase2-alloc` | **40/40 failed** |
| `main` + §5.13 + §5.11/§5.12 + `phase2-alloc` | **40/40 failed** |
| `main` + §5.13 + §5.11/§5.12, no `phase2-alloc` | passes, in the full matrix |

So it is still there, it is still `freeobject` × `phase2-alloc`, and §5.13 is not
involved on either side. One run segfaulted rather than reporting the allocator
error.

Both changes move objects between the frame and the heap and both change what
the collector is told about them: §5.12 makes `unsafe.Pointer` stores emit a
write barrier that was previously skipped, and the escape fix changes which
allocations are heap allocations in the first place. `span has no free objects`
is an allocator invariant failure, which is consistent with the allocator being
reentered or with an accounting disagreement, but the mechanism is still not
established and is not guessed at here.

Two things follow for method, beyond this particular bug:

- **Per-branch verification is not sufficient when changes touch the same
  invariant.** Every one of these branches passed its own full matrix. The
  failure exists only in the combination, so the integration run is the gate
  that matters, not the branch runs.
- **The order of merging is not neutral.** Had `phase2-alloc` merged first and
  `freeobject` second, the same failure would have been attributed to
  `freeobject`, which is the change with the stronger evidence behind it.

Next: reduce the interaction to the smallest program that needs both changes,
determine whether the escape fix exposes a latent defect in the barrier or the
barrier exposes one in the escape fix, and fix whichever is actually wrong. The
branch is preserved at `ccwork/phase2-alloc` with its capabilities and its
allocation-family classification intact; none of that work is lost, and §6's
`malloc_generated.go` finding below stands independently of the escape change.

### 5.15 Fixed: a closure assigning to a captured variable wrote into its own frame (2026-08-01)

§5.10's first known miscompile. A local `string` variable's frame slot held the
address of a sixteen-byte header, and assigning to it copied the new header into
an alloca of the function *doing the assigning*, then stored that alloca's
address into the slot. The slot belongs to whichever frame declared the
variable, so when the assigning function was a closure that had captured the
variable by reference, the assignment published the closure's frame to its
caller and the variable dangled the moment the closure returned.

#### The shape, measured against the host toolchain

70 differential programs, each run with `go run` and with `goc`; **27 differed**.
The discriminator is not the operator, the right-hand side, or the presence of a
`range` statement. It is the *representation of the variable's type*: the three
types cg12 keeps in a frame slot as a pointer to a separate sixteen-byte value.

| affected | not affected |
| --- | --- |
| `string`, and a named type whose underlying type is `string` | `slice` — stored inline, three words in the slot |
| interface (`any`, `error`, …), whatever the payload | struct, array — `allocLocal` gives them stable backing and `assignLocal` copies into it |
| `complex128` | `int`, `bool`, pointer, map, chan, func, `complex64` — one word |

Every assignment form was wrong, including `x = "literal"` and `x = parameter`,
which need no computation at all: `=`, `+=`, a call result, a `string([]byte)`
conversion, tuple assignment and a swap. Every route to a closure was wrong: a
named function-literal variable, an immediately-invoked literal, a literal
nested in a literal, a literal passed to a generic function, a deferred literal
inside a non-escaping literal, and `for i := range seq` over a function iterator,
which has no function literal in the source at all. The variable being a
parameter of the enclosing function did not help.

What was already correct, and stayed correct: a read-only capture; an *escaping*
closure, because `variableStorage` heap-lifts and marks the variable a direct
value; a named result, for the same reason via `resultStorage`; `&v` where the
address escapes; a write through a pointer parameter or a pointer receiver; an
assignment to a *field or element* of a captured variable, which goes through the
address path; and a `defer` in a loop, which §5.1 heap-lifts.

**Six symptoms, one bug.** §5.10 recorded two. Which one a program shows is
decided by what the dead frame happens to hold when it is read back: a silently
empty string, garbage bytes printed as the string, `fatal error: runtime: out of
memory` from a huge length word, `SIGSEGV` in `runtime_concatstrings` or
`goc_memmove`, `unexpected fault address 0x3fffff`, and — for the interface
cases — `<invalid reflect.Value>` or `panic: can't call pointer on a non-pointer
Value` inside `reflect`.

#### The fix

`variableStorage` already had the right representation; it is what the escaping
arm uses. This extends it to non-escaping captures, where it costs *nothing*: no
heap cell, the value simply lives in the declaring frame's slot instead of behind
a pointer to another sixteen bytes.

- `findReferenceCaptures` returns the locals a nested function body refers to. A
  nested body is a function literal **or** the body of a `range` over a
  function, which is lowered into a yield function; both reach the variable
  through the closure environment, which carries the address of its slot.
  `findEscapingCaptures` answers the narrower question — which of those must be
  heap-lifted because the nested function can outlive the frame — and the two now
  share a `bodyLocals` helper so they agree on what a local is.
- `variableStorage` gives such a variable `localAllocTyped` storage, zeroed, with
  `directValues[object] = true`. `localAllocTyped` marks the value's pointer
  words in the frame's stack map, so the string's data word and the interface's
  two words remain GC roots.
- `isIndirectVariableValue` names the type set; `isInlineValue` is the
  "carried as an address" predicate, and `isAddressRepresentedInterfacePayload`
  is now expressed in terms of it.
- `assignmentTarget.directVariable` and `assignmentTargetStoresInline` separate
  "this destination's storage *is* the value" from "this destination is an
  address". They are deliberately not the same question — see the residual below.

#### Checkpoint

- [x] Reduce the full cross product against the host toolchain and record which
  cases are wrong *before* changing anything. 27 of 70 differed; 26 are fixed and
  the 27th is the separate defect below.
- [x] Land the reducers as capabilities: `closure-capture/assigned-string` and
  `closure-capture/assigned-header-values`. Both fail on the base compiler
  (SIGSEGV and `unexpected fault address`) and pass after the fix, in both matrix
  arms.
- [x] Pin the mechanism, not the answer:
  `TestClosureWritesIntoACapturedVariablesStorage`,
  `TestCapturedVariableStorageHoldsTheValue` and
  `TestRangeOverFunctionBodyWritesIntoACapturedVariablesStorage` fail on the base
  compiler across all three types; `TestNonEscapingCaptureStaysOffTheHeap` guards
  §5.9's cost model and passes on both.
- [x] Restore §5.13's three rewritten range-over-function cases.
  `runtime_range_target_forms.go`'s `iteratorTargets` accumulated into a slice
  while every other subject in the file accumulated into a string, because this
  defect made the string form fail. All four cases now use `observed += ...` like
  the rest of the file, and the file itself fails on the base compiler.
- [x] Both matrix arms: 347 subtests, 346 PASS, 1 declared EXPECTED FAILURE,
  0 FAIL, 0 KNOWN GAP. 347 rather than 345 because of the two new capabilities.
- [x] §23 unaffected: 367/367 corpus programs reproducible over 4 rounds without
  `-O`, with `-O`, and against a prebuilt pack.

**A reducer for this bug is worthless unless it clobbers the frame.** Two
programs in the differential corpus matched the host on the first pass and
differed once a recursive call was inserted between the closure's write and the
read — the value simply had not been overwritten yet. Every case in both new
capabilities calls a `clobber` helper in that gap for exactly this reason, and it
is why a 345-capability matrix never saw a wrong-answer bug in ordinary Go.

#### Residual: `complex128` in memory, not fixed

Found by the same measurement and left open, with reducers. A `complex128`
written through an *address* destination stores the address of a frame
allocation into the destination rather than the value:

```
%t3 =p alloc8 16
stored d_3, %t3
%t4 =p add %t3, 8
stored d_4, %t4
call $goc_storep(p %t2, p %t3)      // the field gets %t3, a frame address
```

`storeAssignmentTarget`'s address arm copies bytes only for
`isInlineAggregate || isInterfaceValue`, and `complex128` is neither, so it takes
the one-word store its `ir.ClsP` class implies. Reads are consistent with that,
so it looks right until the producing frame dies: `*p = complex(3, 4)` through a
`*complex128`, a field written in one function and read in another, and returning
a struct holding a `complex128` by value all disagree with the host. It also
hands `goc_storep` a goroutine-stack address as the value being published into a
heap object, which is §5.8's invariant reached by a fourth route.

Widening that arm was tried and is wrong on its own: it turns a passing
`complex128` struct-field case into a fault, because the read side still loads
one word. Making `complex128` consistently a value carried as an address means
putting it into `isInlineAggregate`, which is consulted throughout the frontend —
its own change with its own validation cycle, which is why §5.15 keeps the two
questions apart rather than bundling them.

## 6. Phase 2: Memory safety, allocation, and accurate GC

Exercise both semantic behavior and generated metadata.

### Tests to add

- Specialized allocations for every size-class family represented in
  `malloc_generated.go`, including pointer-free, pointerful, tiny, zero-sized,
  large, aligned, and array allocations.
- Heap-to-heap, stack-to-heap, global-to-heap, interface, slice, map, channel,
  closure, and finalizer write-barrier cases.
- Interior pointers, pointer-containing aggregates, unsafe-but-valid pointer
  round trips, zero-sized fields, and objects crossing span boundaries.
- Concurrent allocation during marking, assist work, sweep pacing, scavenging,
  heap growth/shrink cycles, and low-memory pressure.
- Stack scanning at calls, loops, panic points, channel blocking, syscall
  transitions, and stack copying.
- Rare invariant paths: checkmark, zombie detection in a controlled negative
  subprocess, conservative scan boundaries, dedicated/fractional/idle mark
  workers, and GC metadata huge-page transitions where the host permits them.

### Compiler-emitted checks

- Validate every pointer-marked stack slot against the current stack and known
  heap spans at GC-safe points.
- Poison dead pointer slots in a debug mode and verify stack maps do not retain
  or scan them.
- Audit write-barrier insertion with IR annotations explaining each elided or
  emitted barrier.
- Validate aggregate layout, pointer bitmaps, allocation size/alignment, and
  stack-copy relocation ranges.

Exit criterion: the allocation families are reached intentionally; GC stress
passes with low `GOGC`, forced collection, stack movement, and concurrent
mutation; no compiler validation fires; and core allocation/GC uncovered paths
are either tested or classified.

### 6.1 Stack scanning and GC stress: done (2026-08-01)

The stack-scanning, GC-stress and rare-invariant halves of the list above are
closed by `ccwork/phase2-gc`. The allocation-family and write-barrier halves
remain with `ccwork/phase2-alloc`, which is still held back by §5.14.

**It found a live GC defect on its first program, and the defect was in the
compiler.** A buffered channel's elements were not GC roots.
`(*gen).channelType` hand-rolled the `abi.ChanType` handed to `runtime.makechan`
and filled in only `Size_`, `Align_`, `FieldAlign_` and `Kind_`, leaving
`PtrBytes` zero and `GCData` nil. `makechan` reads `elem.Pointers()` to choose
between a separate scannable buffer and one carved out of the no-scan `hchan`
allocation, so every buffered element of every pointer-containing type -- string,
pointer, slice, interface, struct -- sat where the mark phase never looked. The
same descriptor reaches `typedmemmove` and `bulkBarrierPreWriteSrcOnly` from
`chansend`, `chanrecv` and `sendDirect`, so the element copy lost its write
barrier too.

The symptom was `runtime: marked free object in span`, at 30/30 with
`GOMAXPROCS=1` and 60/100 at 4, against 0/100 on the host toolchain. The reducer
is 30 lines with no `unsafe`, no cleanups and no goroutines: fill a
`chan string` of capacity 64, collect three times, receive. `channelType` now
uses the descriptor `runtimeType` already emits for every other allocation site;
`goc/channel_type_test.go` reads the emitted datum and requires `PtrBytes` and
`GCData`, and fails on the pre-fix compiler.

Three things about method, since §15 collects them:

- **`GODEBUG=clobberfree=1` plus `cg12scanroots=2` is a decisive pair.**
  clobberfree makes a reclaimed object's first word `0xdeadbeefdeadbeef`, and
  `cg12scanroots` prints that word as `head` next to the frame and stack-map slot
  retaining it. Grepping one run's output for `head 0xdeadbeef` named the frame
  holding a freed object directly. Neither diagnostic alone would have.
- **No capability in the 345-entry matrix caught this**, measured rather than
  inferred: compiled by the pre-fix compiler and run under
  `GODEBUG=clobberfree=1`, `goroutine/channel-of-slices-gc`,
  `interface-channel-gc`, `channel-struct-pointer-gc` and `buffered-channel-fifo`
  all pass 6/6. They send one element and collect once, which is not enough for
  the sweeper to reach the buffer and hand the memory out again. `gc/channel-buffer-roots`
  now fills a buffer, churns, collects repeatedly, and then drains, across six
  element types.
- **`reportZombies` could not name the object**, exactly as the end of §5.11
  predicted. The dump printed every object `alloc unmarked` and named no zombie
  at all. Attribution came from `cg12scanroots` instead.

#### Capabilities added

`stack-scan/{loop-safepoints, blocked-goroutines, syscall-transitions,
panic-unwind, stack-copy-roots, callfree-loop-roots}`,
`gc/channel-buffer-roots`, `gc-stress/{concurrent-mark, assist-credit,
sweep-pacing, scavenge-release, heap-growth-shrink, memory-limit}`, and
`gc-invariants/{checkmark, mark-workers, metadata-hugepages}`. The matrix goes
from 345 to 361 capabilities, all `mustPass`, with the single declared
`expectedFailure` unchanged. The plain arm is 361 PASS / 0 FAIL / 0 KNOWN GAP;
the optimized arm is 360 PASS / 1 FAIL, and that one failure is the pre-existing
`-O` defect recorded below rather than anything this branch introduced.

Every one runs under a diagnostic where one applies: the five stack-scanning
programs under `cg12scanroots=1`, `stack-copy-roots` under
`cg12checkstackcopy=1`, `gc-stress/concurrent-mark` under `cg12checkwb=2`,
`gc-invariants/checkmark` under `gccheckmark=1`, and `gc/channel-buffer-roots`
under `clobberfree=1`. `runtimeCapability` grew an `env` field for that, matching
the one `ccwork/phase2-alloc` adds, and
`TestRuntimeCapabilityExclusiveClassification` now reads it: pinning `GOMAXPROCS`
or turning on a stack-walking `GODEBUG` is invisible in the program source, so
the source-pattern floor could not see it.

#### Classified unreachable: the conservative stack scan

`internal/gometa.UnsafePointPCData` marks every generated function
`abi.UnsafePointUnsafe` from entry to `0xffffffff`, deliberately: cg12 keeps
managed references in registers between calls while its stack maps describe the
spill state at call safepoints. `isAsyncSafePoint` reads that table and refuses,
so `runtime.asyncPreempt` is never injected, no frame is ever marked
conservative, and all three calls to `runtime.scanConservative` are dead.

Measured rather than argued. A long call-free loop compiled with runtime coverage
executes `suspendG`, `preemptM`, `doSigPreempt` and `isAsyncSafePoint` and does
not execute `asyncPreempt2` or `scanConservative`; a per-block reading of
`isAsyncSafePoint` shows the taken exit is `preempt.go:448`, the
`up == abi.UnsafePointUnsafe` one. `internal/gometa`'s
`TestUnsafePointPCDataMarksTheWholeFunctionUnsafe` decodes the table the way
`runtime.pcvalue` does, and `cmd/goc`'s
`TestAsynchronousPreemptionIsRefusedForGeneratedCode` asserts the whole chain and
goes red if asynchronous preemption is ever enabled.

**This is Phase 3's problem, not Phase 2's.** §7 asks for asynchronous preemption
in compute loops; cg12 has none, so a non-terminating call-free loop has no
preemption point and `stopTheWorld` would wait for it indefinitely. Every loop
tried here terminates, so that is a consequence of the mechanism rather than a
reproduced hang.

#### Zombie detection, in a controlled negative subprocess

`cmd/goc/runtime_zombie_detection_test.go` makes the sweeper's zombie check fire
on purpose: the subprocess launders a pointer through a `uintptr`, collects until
the object is swept, then publishes the integer back as a pointer where the
collector follows it. It dies with `runtime: marked free object in span` and
`found pointer to free object`; the control, the same program without the
resurrection, exits 0. So the detector is known to work rather than assumed to.

The same test confirms the §5.11 blind spot independently, and pins the
mechanism: `sweepLocked.sweep` calls `moveInlineMarks`, which copies a Green Tea
span's inline marks into `gcmarkBits` and then **resets** them; the detection
reads `gcmarkBits` and is right, but `reportZombies` reads `markBitsForBase`,
which on such a span returns those same reset bits. Detection works, attribution
does not. `ccwork/reportzombies` owns the fix and nothing here touches it; the
test logs the gap rather than asserting it in either direction.

#### Open: with `-O`, a loop-carried local is not a GC root

`stack-scan/loop-safepoints` passes in the plain arm and **fails in the optimized
arm**, and the defect predates this branch: a goc built from `main` (`0505d90`)
in the same tree fails the reducer identically, 10/10.

The reducer is `goc/testdata/runtime_opt_loop_carried_root.go` — 60 lines, no
cleanups, no channels, no `unsafe`. A chain whose head is a loop-carried local is
reclaimed while the local still points at it; under
`GODEBUG=clobberfree=1` the walk afterwards faults on `0xdeadbeefdeadbeef`. The
same shape with the loop removed passes with `-O`, and the host toolchain passes
either way.

| build | reducer |
| --- | ---: |
| goc, no `-O` | 0/10 fail |
| goc, `-O` | 10/10 fail |
| goc from `main` (`0505d90`), `-O` | 10/10 fail |
| host Go 1.26.1 | passes |

`cg12scanroots` shows the frame reporting no `*node` root at all in the optimized
build, while the unoptimized build reports two 32-byte objects from the same
source. The pointer is in the frame — the emitted code stores it at `[x29,#40]`
and reaches it through an address parked at `[x29,#16]` — and the stack map does
not describe it.

Narrowed with a throwaway print in `arm64.(*mc).recordSafepoint` (not committed),
run with the compiler serialised so the output does not interleave. At the
collection inside the loop, `main.carried` reports:

```
-O    : roots 5  stackPointerWords 9  stackAllocTmp 6   -- all five are alloc temps
no -O : roots 8  stackPointerWords 9  stackAllocTmp 13  -- all eight are alloc temps
```

`-O` promotes four pointer-bearing allocations out of the frame while
`StackPointerWords` still lists nine, so the loop-carried pointer is an SSA value
rather than a frame allocation --- and **no promoted value is reported at that
safepoint at all**. `arm64.isSafepointRoot` would accept one (it returns true for
`Cls == ir.ClsP` on a managed frame, and `opt.Mem2Reg` keeps the pointer class for
exactly that reason), so the value is lost before that test: either it is not live
in `analysis.Liveness` at the call, or it is no longer a temporary there.

The obvious candidate fix was tried and **does not work**: a scratch build that
reports every pointer-bearing frame allocation at every safepoint still fails the
reducer 10/10 with `-O`, which is consistent with the narrowing --- under `-O` the
allocations that matter are not frame allocations any more. Recorded so the next
attempt does not spend that experiment again. What is left is `opt.Mem2Reg`'s
promoted values and how they reach `computeSafepointRoots`; changing that is
register-level root reporting for every function in both arms, and it is not
attempted here.

`stack-scan/loop-safepoints` is deliberately left `mustPass` and left failing
rather than reclassified as a `knownGap`: reclassifying would restore the
"0 KNOWN GAP" headline while hiding a live, reproducible miscompile.

#### Open: `//go:noinline` does nothing, and `-O` inlines through it

Found while narrowing the defect above. `goc/compile.go` parses exactly one
directive -- `g.fn.NoSplit = hasCompilerDirective(fd, "go:nosplit")` -- and there
is no `go:noinline` handling anywhere in `goc/`, `opt/` or `ir/`; grep finds the
string only inside `goc/testdata`. The `-O` build of
`runtime_opt_loop_carried_root.go` has no `main_loop`, `main_simple` or
`main_newNode` symbol at all: all three `//go:noinline` functions were folded
into `main_main`.

This matters twice. It is a plausible proximate cause of the root loss above --
after inlining, the allocation helper, the loop and the collection are one frame,
and that frame is the one whose stack map loses the pointer -- though the two
could not be separated, because `CG12_NO_COSTINLINE` and `CG12_NO_AGGINLINE` do
not disable the ordinary size-budget inliner. And it silently weakens every test
in this repository that uses `//go:noinline` to keep a frame distinct so a stack
map can be reasoned about --- 25 of the 382 programs in `goc/testdata`, including
`gc/stack-argument-roots`, `gc/goroutine-entry-stack-map` and all six new
`stack-scan` programs. §15 already
records one investigation where "the real difference was inlining"; this is the
mechanism that makes that failure mode easy to hit.

No test is added, because a test asserting the directive is honoured would be red
on arrival.

#### What is not done

- **Dedicated versus fractional mark workers cannot be told apart**, so §6's
  "dedicated/fractional/idle mark workers" is only partly delivered.
  `cpuStats.accumulate` adds fractional time into `GCDedicatedTime`, so
  `runtime/metrics` folds the two together, and the three per-mode drain
  wrappers -- the only other discriminator -- report **unexecuted** in the
  coverage bitmap at GOMAXPROCS 1, 2, 3, 4 and 8, on runs where
  `gcBgMarkWorker` gets past its `mode not set` throw and `gcDrain` executes.
  `gcDrain`'s only callers are those three wrappers and `mcheckmark.go`, so
  either the modes are never handed out or the instrumentation is missing these
  functions' counters. **If it is the latter, §1's coverage percentages
  understate coverage.** Unresolved; `TestBackgroundMarkWorkersAreScheduledAndDrain`
  asserts only what is measurable.
- The huge-page transition is proved to *run* (`mheap.enableMetadataHugePages`
  and `pageAlloc.enableChunkHugePages` both execute once the heap goal crosses
  1 GiB) and the heap is proved intact across it. Whether the kernel honours the
  `madvise` is not asserted; it is advisory and host-dependent.
- The channel element descriptor is now correct but is still not the *same*
  datum `reflect` reports for that type. goc emits more than one descriptor
  family per type. Pre-existing, independent of this defect, and untraced.
- §6's compiler-emitted checks list is untouched by this branch: no new
  validation mode was added, per §13 item 5, and the existing three were used
  instead.

## 7. Phase 3: Scheduler, goroutines, synchronization, and stacks

Remove the corpus-wide assumption that one P is sufficient.

### Tests to add

- Run-queue overflow, local/global queue transfer, stealing, spinning/non-
  spinning workers, handoff, idle P transitions, and locked goroutines.
- Cooperative and asynchronous preemption in compute loops, calls, allocation,
  channel operations, and foreign/syscall transitions.
- Stop-the-world/start-the-world during allocation, stack growth, timers,
  netpoll, and finalizers.
- Deep recursion, repeated grow/shrink cycles, pointerful frames, closures,
  method values, defer chains, and stack copying while goroutines block/wake.
- Mutex, RWMutex, Once, WaitGroup, Cond, semaphore, notify-list, and starvation/
  handoff behavior under contention.
- Buffered/unbuffered channels, select fairness, close/range, nil channels,
  cancellation, send/receive during stack growth, and many-waiter wakeups.

Run deterministic matrices with `GOMAXPROCS=1`, `2`, and `4`, followed by a
bounded randomized stress batch whose seed is printed and reproducible.

Add debug validation of `g`, `m`, and `p` ownership, run-queue indices,
goroutine status transitions, sudog linkage, stack bounds, and lock rank at
scheduler transition points.

Exit criterion: multi-P scheduler functions such as queue steal/grab/drain and
preemption paths execute; concurrency tests pass repeatedly under all three P
counts; stack-copy and scheduler validators remain clean.

## 8. Phase 4: Timers, netpoll, syscalls, and I/O

Netpoll currently has high function coverage but low block coverage, so target
state transitions and errors rather than adding more happy-path echo tests.

### Tests to add

- Read, write, accept, and connect deadlines: expiry, reset before expiry,
  reset after expiry, simultaneous readiness/timeout, cancellation, and close.
- File-descriptor reuse, duplicate close, half-close, peer reset, hangup,
  interrupted syscalls, nonblocking transitions, and poll descriptor eviction.
- TCP and UDP bursts with multiple Ps, many waiters, concurrent close, and GC.
- Timer heap movement, timer modification/deletion races, ticker drop behavior,
  AfterFunc reset/stop, timer channel GC, fake-time-independent ordering, and
  timer wakeups racing with netpoll.
- Syscall enter/exit and blocking-syscall scheduler handoff, including signal
  interruption and stack growth immediately before and after the call.

Exit criterion: every supported netpoll state transition has a focused test;
the stress batch is stable under multiple Ps; `runtime/netpoll*.go`, timer, and
syscall gaps are classified at block level.

## 9. Phase 5: Types, interfaces, maps, slices, and reflection

Target runtime entry points that ordinary source lowering can bypass.

### Tests to add

- Empty/non-empty interface conversions for scalar, pointer, string, slice,
  struct, array, and zero-sized values; typed nil; failed/successful assertions;
  assertion-cache growth; interface equality and hashing.
- Map access/assign/delete/clear/iteration for all key/value size families,
  indirect keys/elements, NaNs, growth/evacuation, concurrent-read-safe usage,
  reflective map operations, and fat access entry points.
- Slice growth with and without aliasing, pointerful and pointer-free elements,
  append overlap, copy overlap, zero-sized elements, bounds failures, and
  reflective make/grow/set operations.
- Reflective channel make/send/receive/close/select, MakeFunc/call results,
  method values, variadics, promoted methods, aggregate returns, and zero
  `reflect.Value` behavior.

Exit criterion: required functions in `runtime/iface.go`, map runtime files,
slice helpers, `runtime/type.go`, and reflection bridges are covered or have an
explicit lowering-based reason they cannot be reached from supported Go.

## 10. Phase 6: Diagnostics, tracing, profiling, and fatal paths

These paths are less common but are essential when programs fail in production.

### Tests to add

- Runtime trace start/stop, buffer rotation, concurrent events, goroutine and GC
  events, and parseability by the matching trace parser.
- CPU and memory profiling, goroutine/block/mutex profiles, profile-buffer wrap
  and overflow behavior, and concurrent profile readers.
- Stack traces for ordinary calls, inlined/non-inlined calls, goroutines,
  panics, signals, system stack, and created-by frames.
- `runtime.Caller`, `Callers`, `CallersFrames`, function/file/line lookup, and
  symbolization at boundary PCs.
- Debug log buffering and controlled fatal/throw paths in subprocesses, with
  coverage harvested before exit where safe.

Exit criterion: diagnostic output is structurally valid, trace/profile readers
consume cg12-generated output, traceback metadata resolves every tested frame,
and all remaining debug/trace/profile gaps are classified.

## 11. Phase 7: External integration and optional configurations

Do this after the core runtime is stable because it expands the environment,
not the base language semantics.

- Define whether cgo, foreign callbacks, foreign threads, plugins, race, MSan,
  ASan, and Valgrind are supported products, future work, or permanent
  exclusions.
- For supported foreign linkage, test calls in both directions, callbacks on a
  foreign thread, TLS/goroutine attachment, blocking foreign calls, signals,
  panic containment, and GC visibility of referenced Go objects.
- Verify AAPCS64/Go-call ABI boundaries with aggregate parameters/results,
  floating point, variadics where applicable, callee-saved registers, and stack
  unwinding across language boundaries.
- Run configuration-specific coverage separately. Never merge disabled race or
  sanitizer stubs into the ordinary Linux/ARM64 coverage denominator.

Exit criterion: each optional configuration has an explicit support status and
its own test/coverage command or a documented exclusion.

## 12. Milestones and commit discipline

Work in reviewable vertical slices. Each slice contains the reproducer, the
fix, compiler diagnostics where useful, status-suite registration, and updated
coverage classification.

1. **M0 — coverage is reproducible: COMPLETE (2026-07-31).** Stable report/diff,
   checked baseline, classifications, and one explicit outcome per capability.
   The last open box was a full-corpus collection over the current tree; the
   2026-07-31 run collected a usable packet from all 338 capabilities and was
   accepted as the baseline, which also emptied
   `runtime_coverage_baseline_pending.json`. M0 says coverage is *measurable and
   diffable*, not that it is high: §1 records 54.4% active-function and 33.1%
   compiled-block, both short of §2's guideposts.
2. **M1 — current failures are green: COMPLETE (2026-07-28).** Every failure
   this milestone named is closed, and each turned out to be a compiler defect:
   defer/panic (§5.1), signal delivery during GC (§5.2.1), cleanup and finalizer
   over-retention (§5.3), gob (§5.4), trace and HTTP compiler memory (§5.5),
   ECDSA generic method dispatch (§5.6), and `sweep increased allocation count`
   (§5.7). The matrix carries no `knownGap`. Three further defects were found
   while closing these and are also fixed: the argument stack-map hole, where
   rootless safepoints resolved to the entry map and the collector scanned
   uninitialised stack as roots (§5.3, "The argument stack-map hole"); the
   `runtime.KeepAlive` global root (§5.8); and per-iteration loop variables
   (§5.9). §5.10 lists what is
   open; none of it is a failing capability.
3. **M2 — memory foundation is trusted:** allocation families, barriers, stack
   maps, stack copying, and GC stress pass with emitted validators.
4. **M3 — concurrency is trusted:** multi-P scheduling, preemption, stacks,
   synchronization, channels, and select pass deterministic and stress runs.
5. **M4 — OS integration is trusted:** timers, netpoll, syscalls, signals, and
   I/O error transitions pass.
6. **M5 — language-runtime bridges are trusted:** interfaces, maps, slices,
   reflection, and runtime type operations pass.
7. **M6 — diagnostics are trustworthy:** traceback, trace, profiles, debug, and
   fatal paths work and are covered.
8. **M7 — inventory is closed:** every active function and selected assembly
   entry point is covered or justified; optional integrations have explicit
   status.

At every milestone, save the accepted report, compare it to the previous one,
run `gofmt` on changed Go files, run focused and full tests within the memory
limit, and commit only when the milestone's stated invariants are true.

## 13. Immediate next batch

The original batch here is complete: every item from the stable report-diff
through the HTTP compilation OOMs is closed, and each is recorded in its own §5
subsection. What follows replaces it.

1. **Done: the first stable baseline is accepted (2026-07-31).** The full
   collection ran over all 338 capabilities and every one of them returned a
   usable packet, so the run was accepted, `runtime_coverage_baseline_pending.json`
   is empty, and the denominator test reconciles baseline to matrix directly.
   The source-drift check did refuse a diff against the 2026-07-22 baseline
   first, as expected. This closed the last M0 checkbox (§4). What it did *not*
   do is move coverage to where §2 wants it: 54.4% active-function and 33.1%
   compiled-block against guideposts of 65% and 45%.

2. **Fix `for x.f = range s`: done (§5.13).** It was one instance of a wider
   defect — every statement that assigns a value it did not compute from an
   expression had its own destination rules — and the reduction found eight more
   wrong answers, including memory corruption from `x.f, m[k] = a, b` and a
   dropped operator in `m[k] += v`. What replaces it in this list is the defect
   that reduction exposed and did not fix: a closure that assigns a computed
   string to a captured string variable leaves it dangling (§5.10).

3. ~~**Reduce `found pointer to free object`**~~ — done, and it was two defects:
   §5.11 (the entry stack map described a never-started goroutine's argument
   frame) and §5.12 (`unsafe.Pointer` stores lost their write barrier). The
   reducer is `gc/goroutine-entry-stack-map`. What is left from that
   investigation is the rare hang and the rare deadlock recorded in §5.10, and
   the `mspan.reportZombies` blind spot recorded at the end of §5.11 — the next
   fault in this family will be much harder to attribute until that diagnostic
   can name its zombie.

4. **Done: the driver is split** (§16). `goc build-runtime` compiles the Go
   runtime once as a Go module of its own and `goc -runtime` compiles a program
   as a second module holding only the difference. Two of this item's three parts
   turned out not to be wanted: the type region belongs to the *program* module,
   not the prebuilt one, because a descriptor's contents depend on what the
   program reaches; and with no duplicate descriptors in the image,
   `typelinksinit` has nothing to canonicalise, so `abi.Type.Hash` never becomes
   the bottleneck it was expected to. §16 records what the split actually buys.

5. **Begin Phase 2 (§6).** The allocation-family, write-barrier and stack-map
   work is now on a foundation that has been independently exercised, and §6's
   compiler-emitted checks should be built on the diagnostics Phase 1 produced
   rather than new ones: `cg12scanroots`, `cg12checkwb` and `cg12checkstackcopy`
   already cover root reporting, barrier validation and stack-copy staleness.

The original order was chosen so that the mechanisms used to diagnose later
phases — unwinding, stack metadata, GC pointer accuracy, atomics, coverage
reporting — were trustworthy before scheduler and stress results could be
interpreted. That reasoning held, and it is worth restating as a lesson rather
than an instruction: three of the four defects found in §5.7 through §5.9 were
caught by tools built during earlier sections, and none of them would have been
visible in a pass/fail suite.

## 14. Per-module type regions: a goc image can carry more than one Go module (2026-07-29)

Recorded here rather than in §5 because it is not a Phase 1 failure repair: it is
new capability, and it is the prerequisite for compiling the runtime once and
linking it into many programs. Two spikes established the design and measured the
prize (`ccwork/typeoff-alternatives`, `ccwork/sepcompile-spike`): 99.3% of a
program's `.text` bytes are byte-identical across 28 different programs, so a
prebuilt runtime would take the capability matrix from about 12.5 minutes to
about one.

The obstacle was that `abi.Type` addresses its name and its methods by 32-bit
offsets relative to the module's type region, and `arm64.resolveRelativeDataFixups`
bakes `value(target) - value(datastart)` into the data with no relocation left
behind. A second object's offsets are therefore all wrong by however many bytes
precede it.

**The answer is Go's own: per-module type regions.** It needs no change to how an
offset is computed, because `RelativeTo` is already resolved against a symbol in
the same object, which is already correct for a per-module base. The vendored
runtime already implements the other half — `modulesinit`, `moduledataverify`,
`typelinksinit` and `itabsinit` are all called from `schedinit`, and
`resolveNameOff`/`resolveTypeOff` already walk the chain picking the module that
contains the *referring* type. The chain simply had length one.

What changed, and why each piece was necessary:

- **`internal/gometa`'s text-end symbol is per module.** It was the constant
  `runtime_gocTextEnd`, defined once for the whole image by the Plan 9 sidecar,
  so a second module would take the first module's text end as its own `maxpc`
  and `findmoduledatap` would never select it. `gometa.TextEndSymbol` derives the
  name from the module's moduledata name. The sidecar emits it when the sidecar
  carries the module's last text; otherwise the object defines its own bound, so
  an object-only Go module needs no hand-added symbol (the spike's prototype
  needed one).
- **The moduledata symbol name is a parameter.** `runtime.firstmoduledata` is
  global and belongs to the module carrying the runtime's own state; a second
  module declares its own through `ir.Module.GoModuleData`. A Go module that
  defines `runtime.firstmoduledata` without naming it is now an error rather than
  a silently metadata-less object.
- **`moduledata.typelinks` is populated.** This was the one genuinely new piece.
  A second module re-emits a descriptor for any signature or pointer type it
  shares with the first, and two descriptors for one Go type means
  `reflect.TypeOf(x) == reflect.TypeOf(y)` can disagree. `typelinksinit` builds
  the `typemap` that canonicalises them, but only from `typelinks`, which cg12
  left empty. The frontend marks its complete descriptors (`ir.Data.GoTypeLink`)
  and the backend lists them as module-relative `int32` offsets. Only complete
  descriptors qualify: `runtime.typesEqual` reads a type's kind-specific tail, so
  a bare 48-byte header with a kind byte would send it past the end of the symbol.
- **`moduledata.hasmain` is set** on the module that defines `main`; gometa
  zeroed the whole tail of the record.
- **`gometa.ChainModule`** records the one `R_AARCH64_ABS64` relocation into
  `moduledata.next` (offset 584) that joins a module to the chain.

Also fixed, deliberately and with its own test: **the first function of every goc
module was nameless.** `internal/gometa` laid the first name at offset 0 of
`.goc.go.funcnames`, and `runtime.moduledata.funcName` reads a name offset of 0 as
the empty string, so the function at text offset 0 of any module had no name in
any traceback, `runtime.Caller` or `runtime.FuncForPC` result. Upstream reserves
the slot; so does cg12 now. One byte.

### How it is verified

Per §15's rule that a green matrix is weak evidence, the demonstration is an
end-to-end two-module image, not a suite result. `cmd/goc`'s
`TestGoImageCarriesASecondModule` links a real goc-compiled program with a second
Go module built by `internal/permodule` — an ordinary relocatable object whose
data lands 4.1 MB from where it was compiled — and runs it:

- `reflect` reads the second module's type name and kind through its own
  `NameOff`;
- `reflect.PointerTo` on the second module's `int` comes back as the **program
  module's** `*int`, because `PtrToThis` is a `TypeOff` answered from the
  `typemap` `typelinksinit` built — one Go type, one identity, two modules;
- `runtime.FuncForPC` names the second module's function at text offset 0;
- `runtime.Callers` walks a frame only the second module's pcsp table describes;
- a GC stack scan over that frame is the **only** thing retaining its payload:
  `GODEBUG=cg12scanroots=1` filtered for the payload shows exactly one retaining
  (frame, slot) pair, `_goc_probe_hold local slot 0`, in both GC cycles.

The last two are what the spike explicitly could not verify: its second module's
function was a leaf, so the pcsp and stack-map halves of a second module's
pclntab were generated but never exercised. They are exercised now.

Three controls each fail, so no part of the mechanism is decorative: one flat
type region spanning both objects (`nameOff out of range`), a shared text-end
symbol (`minpc or maxpc invalid`, caught by `moduledataverify1`), and no
`typelinks` (`ptr-identity: different` — the same Go type with two identities).

### What is not done

All three items recorded here on 2026-07-29 are resolved by §16, two of them by
being answered differently than expected:

- **The driver is not split.** Done: §16.
- **Type and name symbols are still module-local.** Deliberately still local, and
  §16 explains why exporting the *runtime's* descriptors would have been wrong: a
  descriptor's method entries and `PtrToThis` depend on what the program reaches,
  so the prebuilt module's copy is strictly poorer, and cg12 compares descriptors
  by pointer so two copies break dispatch. The program module owns the whole type
  region instead.
- **`abi.Type.Hash` is zero for every cg12 type.** Still zero, and it no longer
  matters in this topology: the split leaves the image with exactly one type
  region, so `typelinksinit` has no duplicate descriptors to canonicalise and
  never calls `typesEqual` at all. It would matter again for an image that did
  carry two type regions, and for `runtime.itabTable`, which buckets on the same
  field.

## 15. What Phase 1 established about method

Recorded because the same mistakes are cheap to repeat.

- **Every Phase 1 failure was a compiler defect, not a runtime defect.** The
  runtime is vendored upstream source and was almost never wrong. When a runtime
  symptom appears, the prior should be that cg12 generated bad code or bad
  metadata for it.

- **A green suite is weak evidence for a codegen change.** The candidate fix for
  §5.2.1 passed the entire suite while silently miscompiling `defer` inside a
  loop; it was caught only by diffing against the host toolchain. §5.9's bug had
  been live indefinitely for the same reason. Compare against host Go per §3
  step 2 — that step is the one that actually finds things.

- **Rate bugs need rate measurements.** §5.8 moved a fault from 472 in 24000 runs
  to 0 in 24000. A single run, or a handful, would have shown nothing either way.
  Matched controls compiled from identical sources are what separate "fixed" from
  "made rarer".

- **A reduction is a claim and can be wrong.** `cleanup-frame-retention-scribble`
  was committed as the positive proof of a stale-frame theory. The theory was
  wrong and the file proved nothing: it passes with the scribble removed, and the
  real difference was inlining. A reducer earns its status by being re-tested
  against the mechanism it supposedly demonstrates, not by having reproduced once.

- **Build the diagnostic before the fix.** `cg12scanroots` named the retaining
  frame in minutes after an earlier investigation had failed to find it at all.
  §3 step 5 asks for this; it repaid the cost every time it was followed.

## 16. The driver is split: the runtime is compiled once, not once per program (2026-07-30)

§14 made a goc image able to carry more than one Go module. This section is the
payoff: `goc build-runtime` compiles the Go runtime once as a module of its own,
and `goc -runtime <pack> prog.go` compiles a program as a second module holding
only the difference and links the two. The capability matrix builds the runtime
once per run.

### The shape of it

`goc build-runtime -o runtime.gocrt` compiles a fixed root program
(`package main; func main() {}`) through the ordinary executable pipeline and then
keeps everything except the parts that are program-built. The output is one
container file holding three members -- the module's ELF relocatable, its
assembled Plan 9 sidecar, and a manifest of what the two define. One file, so the
manifest cannot drift from the objects it describes, and a version stamp so a
stale pack is refused rather than mislinked.

The manifest carries a **symbol list, not `[]gometa.FunctionInfo`**. The
sepcompile spike said the prebuilt object would have to ship its per-function
metadata, because that spike assumed one merged pclntab per image. §14's design is
the other one the spike named: each module carries its own complete pclntab, so
the program side never needs the runtime's function facts. What it needs is the
set of symbols it may leave out.

`goc -runtime` runs the same whole-program front end and applies the subtraction
to the **finished** module, after IR generation and every module-level pass. A
symbol the program module keeps is therefore bit-for-bit what a monolithic build
would have emitted, which is what makes comparing the two images mean anything.
`goc.TestKeptSymbolsMatchAMonolithicBuild` asserts it.

### Three things cannot be subtracted

- **The interface-method dispatchers.** They switch over the concrete types the
  *program* contains and fall through to `runtime_gocInterfaceDispatchFailure`,
  which throws; there is no `getitab` fallback to absorb a miss. The prebuilt
  module leaves them undefined and names them in the manifest, and the program
  build refuses to proceed if the program does not define one -- so a boundary
  mistake is a build error naming the symbol, not a silent miss.

- **The whole Go type region, which belongs to the program module.** §14's "what
  is not done" expected the opposite: export the runtime's descriptors so the
  program can point at them. That is backwards. A descriptor's contents depend on
  the program: `clearUnavailableRuntimeMethodOffsets` writes
  `runtime.unreachableMethod` into a method entry whose function is not in the
  image, and `populateRuntimePointerTypes` fills `PtrToThis` only when the pointer
  type is also described. The runtime root reaches fewer methods than a program
  that imports more, so the prebuilt module's descriptor is not merely different,
  it is **strictly poorer** -- freezing it in would silently disable reflect
  method calls. Two copies is not an option either: cg12 compares descriptors by
  pointer (the inline itab match in `interfaceTypeWord`, every candidate test in a
  dispatcher), so a value tagged by one module would not match the other and the
  dispatcher would throw. One descriptor per type, in the module that knows the
  most about it.

- **Package assembly the prebuilt module never loaded.** A program reaching
  `reflect.methodValueCall` or a crypto block function needs that package's Plan 9
  assembly, so the pack records which files it assembled and the program
  translates the rest into a sidecar of its own.

### Four defects found by doing it

- **`runtime.lastmoduledatap` was hardcoded to `&runtime.firstmoduledata`**
  (`goc/compile.go`'s `globalDecl`). `runtime.main` runs each module's init tasks
  by walking the chain and stopping at the tail, so with two modules the program's
  own package init never ran: `hello.go` died with `panic: init did not run`, which
  looks like a miscompiled program rather than a linking mistake.

- **Two symbol families were still named by a running counter**
  (`%s.interfacecall.%d` and `%s.interfacecall.promoted.%d`). An itab's method
  entries name those wrappers, so one itab had two contents in two compilations of
  the same program. Now content-named like every other family.

- **goc emits a few package globals twice** -- `runtime.divideError`,
  `runtime.overflowError`, `internal/runtime/maps.errNilAssign`: a zeroed
  placeholder, then the record holding the itab. `obj.prepareELF`'s symbol index
  keeps the last, so the placeholder is dead bytes nothing references. Invisible
  while those symbols were local; `multiple definition of runtime_divideError`
  from `ld` the moment they went global. The split drops the shadowed copy. **The
  duplicate emission itself is not fixed** and is recorded in §5.10.

- **A type descriptor's name kept parameter names while its key stripped them.**
  `runtimeTypeKey` anonymizes a signature's tuples; `runtimeTypeName` did not, so
  one descriptor identity had two possible texts and `reflect.TypeOf(f).String()`
  returned `func(v uint64)` where Go's spec and the host toolchain say
  `func(uint64)` -- with *which* parameter name appearing depending on which
  declaration the compiler described first. Fixed by canonicalizing the name the
  same way as the key.

- **`findfunctab` was a flat 2.6 MB in every module.** `findFuncBucketCount` falls
  back to a 512 MB-covering floor when the module's text span is unknown -- which
  it always was, because the sidecar carries the module's end -- and the floor then
  beat the real count unconditionally. A module bounded entirely by its own object
  knows its span and now sizes the table to it. Without this the split added 2.6 MB
  of zeroes to every image, for a table cg12 never populates (every bucket is zero
  and `findfunc` linearly scans functab from index 0).

### What it measures

| | |
| --- | ---: |
| capability matrix, monolithic (matched control) | 478.4 s |
| capability matrix, split | **406.5 s** |
| per-program compile, a program the runtime dominates | 4.0 s -> 1.5 s (**2.7x**) |
| compile+link CPU over all 358 corpus programs | 4152 s -> 3158 s (1.31x) |
| linked image size over all 358 | +11.6% |

**1.18x on the matrix, not the 12x the prize was stated as.** Both matrix figures here
were taken at `-runtime-status-compile-workers=10` and are therefore not the harness's
capability; §17 re-measures them and shows the matrix *was* still compile-bound, with the
sequential run phase pinning the compile dispatcher rather than starving it. Eight
standard-library programs at 140-185 s each dominate what remains
and gain nothing, because the pack holds only the runtime. Letting the pack carry the
common standard library is where the rest is, and it needs a stub dispatcher for any
program symbol a program does not generate plus moving the image's package-init list to
the program module. Neither was attempted.

### What this does *not* buy, and why

The split removes the back end, not the front end: about 60% of a compile rather
than the sepcompile spike's projected 89%. The reason is the same fact that makes
it correct. The program module must own the type region, and cg12 discovers which
descriptors a program needs *by generating its IR* -- `ensureTypeTag` is called
from the lowering of a conversion, an interface assignment, a `new`. So the
program build cannot skip generating IR for functions the pack already has: that
would drop descriptors, and a missing descriptor is not a link error but a
dispatcher that quietly stops matching. Getting past it needs the pack to carry
enough about its functions' type requirements to reconstruct them without
lowering. That is a redesign, and it is written down rather than attempted.

`moduledata.hasmain` is written correctly (0 on the prebuilt module, 1 on the
program module, asserted on the linked image) but **still has no observable
effect**, for a different reason than in §14: the split leaves the image with
exactly one type region, so `runtime.typelinksinit` -- the only consumer of the
ordering `hasmain` controls -- has no duplicate descriptors to canonicalise.

## 17. The capability matrix's real bound (2026-07-30)

§16 measured the matrix at 406.5 s and called it "no longer compile-bound". Both
halves of that were wrong, and finding out why is the useful part.

The 406.5 s was taken with `-runtime-status-compile-workers=10`, so it was partly
an artifact of the flag. But raising the flag does not explain the rest. The
matrix's wall clock is

    max( slowest single compile , total compile CPU / workers ) + the run phase

and measured against that model on 64 exclusive cores, the unmodified harness left
slack that **grew** with the worker count:

| workers | wall | compile CPU | CPU/workers | slowest compile | bounding term | slack |
| ---: | ---: | ---: | ---: | ---: | --- | ---: |
| 8 | 442.8 s | 3010 s | 376 s | 167.0 s | CPU/workers | 67 s |
| 16 | 351.9 s | 3085 s | 193 s | 168.3 s | scheduling | 159 s |
| 24 | 233.8 s | 3280 s | 137 s | 177.1 s | slowest compile | 57 s |
| 64 (default) | 204.7 s | 4143 s | 65 s | 179.6 s | slowest compile | 25 s |

Slack that grows with workers is the signature of idle workers, not of a floor.

### The coupling that caused it

The compile queue takes two budgets per program: a worker slot, returned when the
compile finishes, and a look-ahead token, **returned only when that program's run
finishes**. The run phase was strictly sequential in matrix order, so the
dispatcher could never get more than `4*workers` indices ahead of the run frontier.

Compile cost is sharply bimodal: eleven net/http, net/smtp and crypto programs
cost 125-167 s each and are 54% of all compile CPU, while the other 327 average
4.2 s. Those eleven sit at matrix indices 155-223 of 338. So when the run frontier
reached one of them it blocked for minutes, no token came back, and the workers had
only the programs inside a fixed window to chew on -- a window they emptied in
seconds. Adding workers widened the window but added more idle workers to the
stall, which is why 16 workers were *worse* against the model than 8.

### What changed

`runtimeCapability` gained an `exclusive` field, and the run phase became two
phases: 278 capabilities run concurrently as their programs become available, and
the 60 whose outcome depends on how much of the machine they have run one at a
time afterwards, with the compile queue drained -- which is more isolation than the
old sequential phase gave them, because that one ran them alongside a saturated
compile queue.

The classification is enforced as a floor by
`TestRuntimeCapabilityExclusiveClassification`, which reads each capability's
source: measuring or waiting on wall clock, bounding an operation by wall clock,
setting `GOMAXPROCS`, asserting an allocation or GC statistic, changing a
process-wide runtime limit, asserting a goroutine count, yielding to the scheduler
and then asserting what happened, or being in a stress category all require the
mark. Extra marks are allowed; a missing one fails a 70 ms unit test instead of one
matrix run in twenty. Three of the 60 are marked despite my judging them robust,
because a mechanical rule that over-includes is worth more than a judgement call
repeated 278 times.

The look-ahead window became `4*workers + exclusive`. That term is not an
optimisation: an exclusive program holds its token until the final phase, so at 60
exclusive capabilities any worker count below 15 lets them alone exhaust a
`4*workers` window and the dispatcher deadlocks.

The queue also dispatches longest-first, ordered by the total size of the Go and
assembly sources in each program's transitive import closure, resolved against the
vendored standard library. Source file size -- the obvious proxy -- is not one: the
capability sources cluster around 575 bytes and the matrix's most expensive program
is 1303 bytes against a 6553-byte maximum. The closure measure's eleven largest
estimates are exactly the eleven measured 125-167 s programs.

### What it measures

| workers | before | concurrent run phase only | + longest-first |
| ---: | ---: | ---: | ---: |
| 8 | 442.8 s | -- | **394.4 s** |
| 16 | 351.9 s | 257.9 s | **221.2 s** |
| 24 | 233.8 s | 229.5 s | **203.2 s** |
| 64 (default) | 204.7 s | 210.5 s | **204.4 s** |

The middle column is a control taken with the cost model replaced by matrix order,
so the two changes can be told apart. Longest-first is worth -14% at 16 workers,
-11% at 24 and -3% at 64, so it stays.

The best wall clock on this box barely moved -- 204.7 s to 203.2 s -- because at 64
workers the old harness was already within 25 s of the floor. What moved is the
matrix's *sensitivity to the worker count*, and that is the number that produced
§16's 406.5 s: a job using its declared CPU share rather than the whole box now
gets 203 s instead of 234 s, and 2.0x against the reported 406.5 s.

At 24 workers the model now says `max(189.5, 3428/24) = 189.5 s` against a 203.2 s
wall clock. The 13.7 s that remains is fixed setup plus the 7.2 s exclusive phase.
There is no idle-worker term left.

One cost the change incurs: longest-first starts all eleven expensive compiles at
once, so the critical-path compile runs under maximum contention and gets slower --
157.6 s alone, 177.1 s in the old schedule, 189.5 s in the new one. Starting it
early wins more than the contention costs, but the two partly cancel, which is why
the 64-worker column is a wash.

### Verification

Five consecutive full unsharded runs at 24 workers, 201.8-203.1 s, and the results
are identical in more than the counts: the sorted set of per-subtest
`--- PASS/FAIL/SKIP` lines and the sorted set of the 338 declared verdict lines are
both byte-identical across all five. 338 subtests, 337 `PASS`, one
`EXPECTED FAILURE` (`defer-panic/panic-string-output`), no `FAIL`, no `KNOWN GAP`,
no skips.

Also: `make test-unit`, `make test-goc-cmd`, `make test-goc-corpus`; both compile
paths (prebuilt and `-runtime-status-prebuilt-runtime=false`); four-way sharding
summing to exactly the unsharded selection; `CG12_NOCACHE=1` versus warm at 4 of 5
with and without the pack, the exception being §5.10's known residue; and the pack
byte-identical across three builds. The change touches no non-test Go file, so the
compiler is bit-identical to the branch point.

### The floor, and what would move it

The bound is now **one program**. `stdlib_http_tls_client_server.go`, compiled alone
on the idle box against the pack:

    wall=157.61s user=179.02s sys=3.01s cpu=115% maxrss=2.97 GB

`cpu=115%` is the finding: goc's compile is single-threaded, and the 15% is the Go
runtime's own background mark. For 158 of the matrix's 203 seconds, 63 of 64 cores
have nothing to do with that program.

Three levers, in the order worth taking them:

1. **Let the pack carry the standard library** (§16 names this). The six
   `stdlib-http` programs' import closures differ by under 1% -- 11.49, 11.47,
   11.47, 11.42, 11.40, 11.40 MB -- and each costs ~157 s, so about **940 s of the
   ~3030 s of compile CPU is the same net/http closure compiled six times**. Note
   it does not move the *floor* unless the pack is built once and cached across
   runs, because building it costs one net/http compile.

2. **Cut the per-process fixed cost.** `hello.go` against the pack costs
   `wall=2.11s user=3.86s`, and all 338 compiles pay it -- mostly loading and
   type-checking the runtime's closure, which `sharedSourceWorld` caches per goc
   *process* while the matrix runs 338 of them. That is roughly **700 s, 23% of the
   compile CPU**. A goc mode that compiles several programs in one process shares
   the world and recovers most of it.

3. **Parallelise inside goc.** The only one of the three that moves the floor rather
   than the total, and the largest piece of work.

### What was not done

A full instrumented coverage run (`make test-goc-coverage`) was not made. That path
shares the collector this section put a mutex on and the report it now sorts; both
are covered by `TestRuntimeCorpusCoverageRecordsConcurrentOutcomesDeterministically`
(200 capabilities recorded concurrently, encoded report byte-identical across two
independently scheduled recordings, clean under `-race`), but the end-to-end path is
unexercised. A targeted coverage run cannot substitute, because
`-runtime-coverprofile` flags every capability that reported nothing, so anything
short of the whole corpus fails by construction.

Five identical runs bound the per-run failure probability only at roughly 45% with
95% confidence. The exclusive classification's real defence is
`TestRuntimeCapabilityExclusiveClassification`, not the repeat count.

## 18. The matrix's floor was one function, not one thread (2026-07-30)

§17 measured the capability matrix at 203 s and identified its bound as one
program: `stdlib_http_tls_client_server.go`, which compiled in 157.6 s at
`cpu=115%`. The reading was that goc's compile is single-threaded and the floor
therefore needs parallelism. The first half is true. The second was wrong about
where the time went, and finding that out is the useful part.

### Profile the program that bounds you

`goc` gained `-cpuprofile`. Profiling the floor program rather than `hello.go`:

| node | cum | share |
| --- | ---: | ---: |
| `arm64.CompileToObjectAndAssembly` | 162.3 s | 82.2% |
| ` ├ arm64.regAlloc` | 149.0 s | 75.5% |
| ` │  └ arm64.coalesceSpillSlots → slotGroups` | **131.2 s** | **66.4%** |
| ` └ arm64.emitMachine` | 4.4 s | 2.2% |
| `goc.compile` (front end) | 15.7 s | 7.9% |

The 61%/39% back-end/front-end split §17 worked from comes from `hello.go`. At
standard-library scale the front end is 8%, and two thirds of the whole compile
is one function: `slotGroups` marked every pair of simultaneously-live spilled
temps into a `map[int]map[int]bool` at *every instruction*, so almost every map
write re-added an edge that was already there.

### What changed

1. **`slotGroups` computes the same relation differently.** Members are numbered
   densely and interference is a bit matrix over that numbering; an edge is
   recorded only where a member *becomes* live, against the set live there, with
   a block's live-out set marked in full; and the greedy assignment carries each
   group's union so a conflict test is one bit instead of a scan. The relation and
   the assignment are unchanged, and the prebuilt runtime pack -- the largest
   module goc compiles -- is byte-identical before and after.

2. **The per-function back end is compiled concurrently** (`arm64/parallel.go`).
   Lowering, allocation and emission read one function plus read-only module
   facts and produce a result whose offsets are all relative to that function's
   own start, so functions compile in parallel and merge strictly in function
   order. Every address, symbol, relocation and DWARF row comes from the merge
   order, never from which worker finished first. Worker count is `GOMAXPROCS`,
   overridable with `GOC_BACKEND_WORKERS`; the output does not depend on it, and
   `TestParallelBackendIsByteIdenticalToSerial` checks that from 1 to 256 workers.
   The first error *in function order* is reported, so a failing compile says the
   same thing a serial one would.

3. **Four determinism bugs fixed**, all letting map iteration order into generated
   code: the cyclic probability in `analysis.Frequency` and the mesh total in
   `redistributeMesh` both summed floats over a map (float addition is not
   associative, so the loop multiplier and every spill decision derived from it
   varied per run); `LoopForest` built its loop list from a map, and the parent
   and innermost-loop choices break ties by position in it; and `allocaGroups`
   took its stack-colouring candidates from a map, so which allocations shared a
   slot varied.

### What it measures

`stdlib_http_tls_client_server.go`, against the pack, on a **shared** box (two
sibling jobs compiling throughout), each pair taken back to back:

| | wall | cpu | maxrss |
| --- | ---: | ---: | ---: |
| branch point | 182.8 s | 110% | 2.68 GB |
| + `slotGroups` rewrite | 48.6 s | 141% | 2.83 GB |
| + concurrent back end, 1 worker | 53.5 s | 126% | 2.31 GB |
| + concurrent back end, 64 workers | **29.4 s** | 252% | 2.28 GB |

**6.2x on the program that bounds the matrix**, of which 3.8x is the rewrite and
1.8x the concurrency. `goc build-runtime` went 8.1 s to 2.8 s.

Full unsharded matrix, same box, same day, every run 338 subtests / 337 declared
PASS / 1 EXPECTED FAILURE / 0 FAIL / 0 SKIP / 0 KNOWN GAP:

| | wall | slowest compile |
| --- | ---: | ---: |
| branch point | 303.9 s, 212.9 s, 225.8 s | 278.7 s, 199.2 s, 211.7 s |
| this work | 116.0 s, 107.1 s, 67.8 s, **71.4 s**, 81.5 s | 99.0 s, 90.8 s, 55.9 s, **59.5 s**, 69.0 s |

The spread is sibling load, which fell over the afternoon. The fairest reading is
the three consecutive runs 71.4 s (this work), 225.8 s (branch point), 81.5 s
(this work): **3.2x**. Against §17's exclusive-box 203.2 s, 3.0x.

**The bound has not changed in kind.** It is still
`max(slowest single compile, compile CPU / workers) + run phase + setup`, and it
is still the slowest single compile: 55.9 s of a 67.8 s run. What changed is how
big that term is.

### The defect the concurrency exposed

`go test -race` over the *corpus* -- real programs through the real front end, not
the synthetic module the back-end unit test builds -- reported 20 data races on
`ir.Block.Preds` between goroutines compiling different functions. The cause is in
the front end and predates this work: `gen.funcDecl` resets the generator's
per-function defer state but not `deferBlocks`, which `derive()` does reset for a
closure, and `addDeferRecoveryEdges` wires every block in that list to the
function's `deferreturn` block. The previous function therefore gained a synthetic
control-flow edge into the next function's blocks, and dominance, liveness and
frequency all spanned two functions.

It was invisible while the back end compiled one function at a time: the
predecessor lists those analyses rebuild live on the blocks, so each function
overwrote the previous one's damage on its way past. Fixed at the cause;
`TestEachFunctionsControlFlowStaysInsideThatFunction` fails on the unfixed
compiler with exactly that edge.

The general lesson is the same one §14 records. A green suite and a green
synthetic byte-identity test both said the concurrency was fine. The run that
disagreed was the race detector over real programs.

### What would move it further

`cpu=252%` is the new ceiling, not 115%. With the back end no longer dominated by
one function, the single-threaded front end (8% of the old compile, a much larger
share of the new one) and the serial merge bound the speedup. Going further means
the front end -- a whole-program parse, type-check and reachability walk -- which
is a much harder target than per-function work and was deliberately not attempted
here. The other two levers on `perf/test-suite` (letting the pack carry the
standard library, and batching programs into one process) reduce *total* compile
CPU, which this work does not; they compose with it rather than competing.


## 19. The pack carries the standard library, and each program takes the richest it can (2026-07-30)

§17 left the matrix bounded by one program: `stdlib_http_tls_client_server.go`,
158 s single-threaded, with 63 of 64 cores idle. §16 and §17 both named the same
next move -- let the prebuilt pack carry the standard library, so the six
`stdlib-http` programs stop compiling the same `net/http` closure six times.

They also both assumed one pack. That is the part that is wrong.

### One pack cannot serve every program

A pack built from a root that imports `net/http` compiles
`stdlib_http_tls_client_server.go` in 23.7 s instead of 158 s, and the image runs
with output identical to the host toolchain's. `hello.go` against that same pack is
refused before the linker sees it: `checkProgramSymbols` reports **18,309 program
symbols** the pack left undefined and `hello.go` does not define.

§16 named two blockers -- the interface dispatchers and the package-init list --
and proposed a stub dispatcher for the first. The list is much longer than two, and
the rest is not stubbable:

- **The whole Go type region belongs to the program module**, by §16's own
  argument: a descriptor's contents depend on what the program reaches, and cg12
  compares descriptors by pointer. Every descriptor of every `net/http` type is a
  symbol the pack references and `hello.go` never generates.
- **Every static itab the pack degraded.** A stub is safe only where the thing it
  replaces is unreachable. `runtime.itabsinit` walks every module's `itablinks` at
  startup, before `main`, so a stubbed itab is *read* by every program that links
  the pack.

So a fixed superset pack still needs the redesign §16 wrote down. What does not is
a pack a program is allowed to be a **superset of**.

### Containment, and why it is the whole safety argument

A pack records the closure it was compiled from. A program may use it only if the
program's own loaded closure contains all of it. The pack leaves its type region,
its dispatchers and its degraded itabs for the program module; a program that
loaded everything the pack loaded generates a superset of them, and `checkProgramSymbols`
already enforces exactly that condition. A program that loaded less falls back.

`-runtime` therefore takes a list. goc runs the front end and, the moment it knows
what the program loaded -- before anything consults a manifest -- picks the pack
carrying the most of those it can use. The runtime-only pack carries nothing beyond
the runtime and every executable compiles the whole runtime closure, so it is
usable by every program and the list degrades rather than failing.

Package init needs no special handling under that rule, which is the second thing
§16 expected to have to build. A pack package's dependencies are all in the pack, a
program-only package can never be one of them, and `runtime.main` walks the module
chain pack-first -- so the pack's inits run before the program's extras, in
dependency order, and `addModuleInitTasks` already skips a task the pack defines,
so each package has exactly one `initTask` record and `doInit`'s guard does the
rest.

### Three defects found by doing it, two of them live miscompiles

- **Two distinct closures could compile to one symbol.** A function literal was
  named `<pkgpath>.func.<line>.<column>` whenever the generator had no explicit
  name for the enclosing function -- the ordinary case, since `functionName` is set
  only for a generic instantiation or a package initializer. Position does not
  identify a literal within a package:
  `crypto/internal/fips140/nistec`'s `p224.go`, `p384.go` and `p521.go` are
  generated from one template, so their `sync.Once.Do` literals sit at identical
  line and column, and three different closures came out of the compiler as
  `crypto/internal/fips140/nistec.func.114.16` (sizes 0x228, 0x3e8, 0x318).
  `obj.prepareELF` keys its symbol index by name and keeps the last, so **every
  reference resolved to whichever was emitted last**: `p224B` would have run
  `p521B`'s initializer. Local symbols kept the system linker from ever having to
  choose, which is why nothing saw it; exporting them for a pack made it loud.
  Literals are now named after the declared function they are written in, as Go
  itself does, and `checkUniqueFunctionSymbols` refuses any module whose functions
  do not have distinct linker symbols.

- **An interface type test was built from a list the whole program decides.**
  `x.(I)` was lowered to an inline chain of descriptor comparisons over every type
  that implements `I` *anywhere in the program*. That makes an ordinary function's
  body depend on the whole program's declared method set -- fine monolithically,
  wrong for a split, because a function the program module subtracts was compiled
  into the pack against the pack's method set. Measured: the pack builds
  `interface{String() string}` with 184 candidates where
  `stdlib_http_client_server.go` would use 198, `io.Reader` with 98 against 99,
  `io.Writer` with 76 against 77. The program linked and aborted in a goroutine
  `net/http.(*Transport).dialConn` started, on an assertion that fell off the end
  of the pack's chain. The chain is now a fast path and `runtime.getitab` is the
  answer -- which is what `interfaceTypeWord`, the conversion path directly above
  it, has always done. **This was a pre-existing hole in the driver split**, not
  something the standard-library pack created; the runtime-only pack has it too and
  has escaped only because runtime code rarely asserts to a non-empty interface. It
  also closes a second gap: `interfaceImplementations` enumerates method
  *receivers*, so a type whose method set comes entirely from an embedded field
  appeared in no entry and used to answer no to an interface it implements.

- **One runtime type hasher emitted three times.** `emitRuntimeTypeHasher` had no
  already-emitted guard where its sibling `ensureRuntimeTypeEqual` has always had
  one, and `net/http` has three map types keyed by `connectMethodKey`. The three
  definitions are byte-identical, so nothing was miscompiled -- but they are three
  definitions of one symbol, which the system linker refuses once it is global.

### What it measures

Full unsharded matrix, `-v -count=1`, every row checked for `subtests=338
pass=338 fail=0 declaredPASS=337 expectedFAILURE=1 knownGAP=0`:

| run | wall | compile CPU | slowest single compile | bounding term |
| --- | ---: | ---: | ---: | --- |
| control: runtime-only pack, same tree | 201.2 s | 4392.7 s | 191.8 s | slowest single compile |
| seven packs, cold cache | 210.3 s | 2783.6 s | 46.6 s | building the packs (154 s) |
| seven packs, warm cache | **56.4 s** | 2801.4 s | 46.9 s | slowest single compile |

**3.6x**, and the matrix is bounded by the slowest single compile again -- 46.9 s
against `compile CPU / 64 = 43.8 s`, so both terms would have to move to go much
below 50 s. Cold, the packs buy nothing: their 154 s replaces the 192 s compile
almost exactly. The whole gain is the cache, as §17's lever 1 predicted.

Those three rows were taken on a quiet box. Repeated later with two sibling jobs
loading it (load average 168) the same three are 275.0 s, 308.2 s and 66.9-99.4 s,
so the ratio holds at 2.8-4.1x while the absolute numbers do not. Six full matrix
runs in all, every one of them 338 subtests / 338 pass / 337 declared PASS /
1 EXPECTED FAILURE / 0 KNOWN GAP.

`analysis/splitdiff` over all 358 corpus programs, each compiled monolithically and
against the pack set, run, and compared on exit status and full output: 2
differences, and they are the two §16 already recorded, with the same numbers
(`gomaxprocs_memstats.go` prints 105 mallocs monolithic against 120 split;
`bytes_grow_stats.go` 16718922 against 18220422). Re-running those two against the
runtime-only pack alone reproduces both, so they predate the standard-library packs.
Compile+link CPU over the 358 is 4923.5 s monolithic against 1738.5 s split, 2.83x;
image bytes +11.0%.

Determinism is unchanged on both compile paths: `CG12_NOCACHE=1` against warm is
byte-identical for `hello.go`, `fmt_sprintf.go`, `gc_struct.go` and
`runtime_cleanup_frame_retention.go`, with `runtime_defer_capture_allocs.go` still
the known residue of §5.10.

`goc build-runtime` caches a pack under `$XDG_CACHE_HOME/cg12/runtime-pack`, keyed
on the pack format version, target, `-O`, the package list, the goc binary's own
bytes, the *contents* of the whole vendored `stdlib/` tree, and `cc --version`.
`CG12_NOCACHE=1` disables it. Note that `go build` stamps the commit and a
clean/dirty bit into a binary by default, so the matrix builds its compiler with
`-buildvcs=false` -- otherwise a comment-only commit invalidates 157 s of packs the
compiler's code did not change.

The seven roots are the largest package each of §17's eleven expensive programs
imports: nothing, `net/http`, `net/smtp`, `crypto/x509`, `crypto/ecdsa`,
`crypto/ecdh`, `crypto/hpke`. They share no ancestor small enough to serve all of
them, so it is one pack per closure shape. Built concurrently they cost 157 s of
wall clock -- one `net/http` compile -- and 345 MB; warm, 0.41 s.

### What is not done

The manifest's selection fields sit in the same JSON blob as its 36,755-entry
`Defined` list, so a program pays 0.22 s parsing packs it will not use. Over the
matrix that is 74 s of CPU and about 1 s of wall clock. Splitting a small selection
header out of the container would remove it; worth doing if the pack set grows.
§20 reduces it rather than removing it -- a batch worker parses the set once for
every program it compiles -- so the cost is still there for any one-shot build.

The floor is still one program and it is still single-threaded, so §17's levers 2
and 3 are unchanged in kind, only smaller in absolute terms.

## 20. One process compiles many programs, and they share a pack set (2026-07-31)

§16 and §17 both left the same residue: the matrix runs one `goc` process per
program, and every one of those processes rebuilds the parsed and type-checked
closure of the Go runtime before it looks at the program. `goc/source_world.go`
caches that per *process*, so 338 processes build it 338 times.

`goc compile-batch` reads one JSON request per line of stdin -- `{"source":
"prog.go", "output": "prog.bin"}` -- and writes one response per line of stdout.
A worker outlives its programs, so the world is built once per worker instead of
once per program. The matrix dispatches through a pool of these, and
`-runtime-status-batch-compile=false` restores the old path so the A/B below is a
measurement rather than a comparison against a different tree.

### Why the request stream, and why the configuration is not per request

A static list handed to a process at startup is simpler and it partitions the
work statically, which destroys §17's schedule: longest-first dispatch through a
shared queue, and a bound on how far compilation may run ahead of the run phase.
Both are properties of a queue. A request stream keeps the queue and changes only
what a worker is.

Target, `-runtime` and `-O` are exactly the axes of goc's source-world key, so
they are command-line flags rather than request fields: a worker that accepted
them per request would silently build a second world on the first request that
differed. `-runtime-covermeta` is not offered at all, and the coverage run keeps
the one-shot path, because instrumenting the runtime per program is the opposite
of one build configuration per process.

### The pack set is the part §19 constrains

The obvious way to make a batch share the packs is to read them at startup and
hoist the read out of the per-program loop. §19 forbids that: `-runtime` is a
set, and which pack a program gets depends on that program's own loaded closure,
which is not known until the front end has run.

`packSet` (`cmd/goc/prebuilt.go`) holds both halves at their natural lifetimes.
Every candidate's **manifest** is read when the set is built, because choosing
needs the closures and nothing else. Each pack's **objects** are read the first
time some program selects that pack, and kept. Programs in one worker that choose
the same pack share one read; programs that choose different ones each pay once.
One-shot `goc` and `goc compile-batch` use the same code path with different
lifetimes, so a batch build and a solitary build are the same compile.

This is strictly better than either half alone. A batch that hoisted the read
would have to re-read per program the moment two programs disagreed, and a
one-shot process re-parses all seven manifests every time -- which is §19's own
open item, 74 s of CPU across the matrix, and a worker amortizes it for free.

The pack a compile chose is matched back to its file by pointer identity, and a
manifest that is not one the set offered is an error rather than a fallback to
the first pack. It cannot happen today, but an image built from one pack's
objects and another pack's subtraction is precisely the mislink the manifest
exists to prevent.

### One bad program costs one program

A one-shot `goc` that rejects a program exits and the next program gets a fresh
process; a worker has no fresh process to offer. Three things make that safe: a
compile error is a response rather than an exit; **a panic inside the compiler is
recovered per request** and reported with its stack as that program's error, so a
compiler bug costs one program instead of every program queued behind that
worker; and the pool never reuses a worker whose request failed at the protocol
level, so a worker killed by the OOM killer costs one capability. Diagnostics are
never written to a worker's own stderr, which is shared and could not be
attributed; `cc`'s output is captured per program and folded into that program's
error.

### What it measures

Full unsharded matrix, `scripts/matrix-timing.sh`, `-count=1 -v`, **4 compile
workers**, warm pack cache, on a quiet box. `main` was measured by checking it out
in the same working directory, because a `goc` built from a tree at a different
absolute path embeds different strings and is not a valid control.

| run | wall | CPU | sum of per-compile wall | slowest single compile |
| --- | ---: | ---: | ---: | ---: |
| `main` (a639ec9) | 351.8 s | 2758.7 s | 1365.5 s | 23.1 s |
| this tree, `-runtime-status-batch-compile=false` | 351.3 s | 2755.6 s | 1363.9 s | 23.0 s |
| this tree, batch on | **273.6 s** | **1930.5 s** | 1050.8 s | 22.7 s |
| this tree, batch on (repeat) | **273.2 s** | **1926.8 s** | 1049.8 s | 23.0 s |

**Wall -22.2%, CPU -30.0%.** The one-flag-apart control lands within 0.15% of
`main` on every column, which is what says the flag is the only difference.

`ccwork/goc-batch-b` measured this same lever at 5-12% beneath the packs. It is
larger on top of them, and the reason is §19 rather than anything in this
section: the packs removed compile work, not process work, so the per-process
cost the batch removes is roughly the same number of seconds against a total that
is 40% smaller. The two levers multiply.

Peak RSS is unchanged: 2622 MB on `main` against 2624 and 2725 MB batched. A
worker's peak is still the largest program it compiles, and retaining packs it
has already read does not move the maximum, so
`compileRuntimeCapabilityPeakBytes = 3 GiB` and the divisor built on it stand.

### Which term bounds the matrix afterwards

    wall ~ max( slowest compile , compile CPU / workers ) + run phase + setup

At 4 workers the second term binds by an order of magnitude -- 1050.8 / 4 =
262.7 s against a 23.0 s slowest compile -- and 262.7 + 14.9 s of run phase
accounts for the 273.2 s observed. At the default 64 workers the terms swap:
1050.8 / 64 = 16.4 s is below the slowest compile, so the bound is the same
23.0 s program in both arms and the wall clock would barely move. **The value of
this lever is the CPU, not the floor**, and the floor is still one
single-threaded compile of `stdlib_http_tls_client_server.go`.


### Verification (2026-07-31)

§20 was measured and committed before its safety property had been checked. This
is that check, run on `ccwork/verify-batch-reconcile` off the same tree.

**The corpus-wide leak check.** All 358 corpus programs, compiled three ways
against the full seven-pack set -- one `goc` process per program, one batch, and
a batch fed the same programs in reverse -- with the eleven programs that select
a rich pack spread evenly through the 347 that take the fallback, because
alphabetically they cluster and a shared queue would hand them out at once.

    one-shot 358 in 217.2s, 0 failed | batch 358 in 183.6s, 0 failed
    batch-reversed 358 in 170.5s, 0 failed | identical=325 differing=33
    behaviour: identical=358 differing=0

The whole check was then run a second time, from scratch, as an independent
draw: `identical=324 differing=34`, a partly different set of programs differing,
and again **0 leaks** with all 34 identical in content.

**Zero leaks.** No program built one way and failed another. Every one of the 33
whose bytes differ is nondeterministic without any batch process in the picture:
recompiled alone, 29 gave 2-5 distinct images in 5 solo compiles, and the other
four gave the batch's exact bytes from a solitary compile within 20-60 repeats
(`stdlib_net_mail_textproto.go` takes its minority branch 3 times in 53). All 33
are also structurally identical across the three builds -- same symbols, same
sizes, same image size, only addresses moved -- which is the §5.10 residue and
not a difference in content.

**The interleaved case**, which neither `goc-batch-b` nor §20 had: one worker,
33 programs ordered so a rich-pack program sits between fallback programs
throughout, so that single process read all seven packs and kept each while
compiling programs that chose the others. 0 leaks, and behaviour identical for
all 33.

**Everything else.** `scripts/determinism-check.sh` gives 4 of 5 byte-identical
cold against warm on both the seven-pack and the no-pack path, with
`runtime_defer_capture_allocs.go` the only difference, in both rounds.
`make test-unit`, `make test-goc-cmd` (213.8 s) and `make test-goc-corpus`
(528.4 s) all pass.

**The matrix, four arms, all at 8 compile workers, all 338 subtests / 337 `PASS`
/ 1 `EXPECTED FAILURE` / 0 `KNOWN GAP` / 0 `FAIL`:**

| arm | wall | compile CPU | process CPU | slowest compile | max RSS |
| --- | ---: | ---: | ---: | ---: | ---: |
| `main` (a639ec9) | 212.4 s | 1407.3 s | 2916.8 s | 24.2 s | 2620 MB |
| this tree, `-runtime-status-batch-compile=false` | 186.1 s | 1409.3 s | 2691.4 s | 24.3 s | 2619 MB |
| this tree, default | 174.4 s | 1095.9 s | 2217.3 s | 24.0 s | 2631 MB |
| this tree, `-runtime-status-prebuilt-runtime=false` | 191.8 s | 1452.9 s | 4352.9 s | 33.1 s | 2264 MB |

Compile CPU **-22.1%** against `main` and **-22.2%** against the one-flag control
on this tree, which reproduces §20's claim; the control lands 0.14% from `main`,
which is what says the flag is the only difference. Wall clock moved -17.9%
rather than §20's -22.2% because that was measured at 4 workers and this at 8 --
`compile CPU / workers` is a smaller share of the wall clock here. The box was
shared throughout (load average 8-22), so the wall-clock column is the least
trustworthy of the three.

The **monolithic batch path** -- a worker compiling the runtime into each program
rather than linking a pack, which §20 never exercised -- passes with the same
census. `ps` sampled during it shows a live `goc compile-batch` worker with no
`-runtime` argument, so it really is that branch of `compileBatchProgram`.

**`make test-goc-coverage`** passes, 338/338 with the same census, and it does
bypass batch mode: `newRuntimeCapabilityBatchPoolFor` and
`buildPrebuiltRuntimesForCapabilityStatus` both return nothing when
`-runtime-coverprofile` is set, and `ps` sampled during a coverage run sees no
`goc compile-batch` process at all. Separately, `runtime-cover-diff` cannot
compare against the checked-in baseline -- `RuntimeSourceID` differs -- and that
is pre-existing: the baseline commit `750c9c2` is an ancestor of `b5537b5`, the
last commit to touch `stdlib/`, and `git diff main...HEAD -- stdlib/ goc/` is
empty. The baseline needs refreshing; that is not this change's to make.

## 21. The print built-in: separators, dispatch, and the print lock (2026-07-31)

`println("a", 1, true)` printed `a1true`. The Go specification's table for the
two print built-ins says `println` is "like `print` but prints spaces between
arguments and a newline at the end", so the separators and the trailing newline
are required; only the rendering of an individual operand is left
implementation-specific. cg12 walked `call.Args`, emitted one runtime print call
per operand and a single `printnl`, and never emitted the separators at all.

The host toolchain implements the rule in `cmd/compile/internal/walk.walkPrint`
by rewriting the operand list before it lowers anything: insert a `" "` string
between operands, append a `"\n"`, then collapse runs of adjacent constant
strings into one. cg12 now builds the same sequence, so `println("x", "y", "z")`
is a single `printstring("x y z\n")` here as it is there.

### The spacing was one of five

Auditing the whole of `walkPrint` against cg12's dispatch — the step §3 calls
comparing against the host toolchain — found four more differences, all
wrong-answer bugs in valid Go and all fixed together:

| Operand | cg12 printed | host prints |
| --- | --- | --- |
| `[]byte{1,2,3}` | `54422416784368` | `[3/3]0x...` |
| `any(nil)` | `0` | `(0x0,0x0)` |
| `any(42)` | `54422416784416` | `(0x963e0,0xacbf8)` |
| `complex(1,2)` | `54422416784632` | `(1+2i)` |
| `complex64` | `4647714816524288000` | `(3.5+4.5i)` |

and the fifth, which is not about an operand at all:

**A print statement took no lock.** `runtime/print.go` states the requirement
outright — *"The compiler emits calls to printlock and printunlock around the
multiple calls that implement a single Go print or println statement"* — and
`runtime.minhexdigits` is documented as protected by it. cg12 emitted neither,
so one statement was a run of unsynchronized `write(2, …)` calls. Eight
goroutines at `GOMAXPROCS=4` printing a thirteen-operand line corrupted
**3092, 3035 and 3002 of 3200 lines** in three runs; the host corrupts none.
Every runtime diagnostic goes through these same routines, so every traceback
and every `GODEBUG` line was exposed to it.

Two smaller items came with them. **Operands are now evaluated before the lock**,
as the host does, so an operand whose evaluation prints no longer interleaves
into the middle of the statement printing it. And **`runtime.quoted`**
(`type quoted string`) routes to `printquoted`: `traceback.go:1294` prints
goroutine labels through it, and before this a label containing a quote or a
newline went out raw. `runtime_goroutineheader` calls `printstring` 21 times and
`printquoted` never on `main`; it calls `printquoted` twice here.

Pointer-shaped operands keep going to `printhex` rather than the host's
`printpointer`/`printuintptr`, which is not a difference: both of those are
one-line wrappers that call `printhex` with the same value.

### Two complex64 defects it exposed

Routing a `complex64` operand to `runtime.printcomplex64` segfaulted, because
that routine converts to `complex128` internally. Both causes were pre-existing
and independent of print; both are fixed here, because otherwise this change
would have turned a silently-wrong `println(c64)` into a crash.

- **`real()` and `imag()` of a `complex64` returned garbage, the same garbage for
  both halves.** A `complex64` is two `float32` halves packed into one 64-bit
  integer, so reading a half is a bitwise reinterpretation between a
  general-purpose and a floating-point register — `ir.OCast`, which lowers to
  `fmov`. cg12 used `ir.OCopy`, which re-types only within one register file.
  `var b complex64 = complex(3.5, 4.5); println(real(b), imag(b))` gave
  `-2.8673504e+25 -2.8673504e+25`. Every `complex64` arithmetic operation was
  wrong with it, since they all go through `complex64Parts`/`packComplex64`.

- **`gen.convert` had no complex case at all**, so `complex128(b)` `Copy`d the
  packed pair into a pointer and the program took SIGSEGV at
  `0x4090000040600000` — the packed `(4.5, 3.5)` bit pattern used as an address.

### What it is verified against

Per §15, a green matrix is weak evidence, so the evidence is the host comparison.

- **43 of 46 corpus programs that print now produce byte-identical output to the
  host Go toolchain, against 34 on `main`.** Six programs
  (`allocs_per_run.go`, `bytes_grow_allocs.go`, `bytes_grow_capacity.go`,
  `bytes_replace_allocs.go`, `reflect_methods.go`,
  `runtime_defer_capture_allocs.go`) differed from the host only by the missing
  separator and now match. The three that still differ are
  `bytes_grow_compare.go`, `bytes_grow_stats.go` and `gomaxprocs_memstats.go`,
  which print allocation and GC statistics — §5.10 already records the first two
  as varying with scheduling.

- **Three reducers, each passing under the host toolchain, passing here, and
  failing on `main`.** `print-builtin/operand-separation` asserts the separator
  rule and the whole operand table byte for byte, plus the address-shaped
  operands by shape, the nil interface, `print` versus `println`, the degenerate
  operand counts, and the evaluation-order rule; on `main` it fails with
  `println wrote "a1true\n", want "a 1 true\n"`.
  `print-builtin/statement-atomicity` is the concurrency check.
  `core-types/complex64-parts` pins `real`/`imag`, arithmetic, comparison and
  both conversions, with `complex128` as the control.

- **A control build fails the atomicity reducer for the right reason.** With the
  separators kept and only the `printlock`/`printunlock` emission removed, it
  fails with `two print statements interleaved inside one line: worker 0 round 3
  tail 1 2 worker 37  round 40  tail 51  62  73  84`. The check is not
  decorative.

- **Nothing the print path emits allocates, except what already did.**
  Disassembling a linked image and looking for `runtime.newobject` inside every
  print routine: `printlock`, `printunlock`, `printsp`, `printnl`, `printbool`,
  `printint`, `printuint`, `printhex`, `printhexopts`, `printstring`,
  `printslice`, `printeface`, `printiface`, `printquoted` and `printpointer` are
  all allocation-free. `printfloat32`, `printfloat64`, `printcomplex64` and
  `printcomplex128` are not, which is the §5.10 item above and pre-existing for
  the two float ones. `printquoted`'s `[]byte("\"")` conversions stay on the
  stack: cg12 passes a real stack `tmpBuf` to `stringtoslicebyte`, so
  `rawbyteslice` is not reached.

- **The nosplit audit moved 287 → 359 direct split callees, and every added edge
  is `printlock` (27), `printunlock` (27) or `printnl` (18).** No edge was
  removed and nothing unrelated appeared. Upstream has the same shape: its
  `printlock` is not `nosplit` either, and every `nosplit` function that prints
  calls it.

- `cg12checkwb=1`, `cg12checkwb=2`, `GOC_DEBUG_WRITEBARRIER=1`,
  `gccheckmark=1,invalidptr=1,clobberfree=1`, `gcshrinkstackoff=1`,
  `checkfinalizers=1` and `gctrace=1` are all clean on a print-heavy
  allocate-and-collect program and on both new reducers.

- A panic traceback reads exactly as it did before, frame for frame.

- Determinism is unchanged: `scripts/determinism-check.sh` gives 4 of 5 sample
  programs byte-identical cold against warm, twice, with
  `runtime_defer_capture_allocs.go` the known §5.10 residue.

### What is not done

- The four allocating print routines are recorded in §5.10 rather than fixed;
  the fix is an escape-analysis feature, not a print-lowering change.

- `printquoted` is exercised through a `//go:linkname` probe, which produces
  `"with \"quote\"\nand\ttab and é and \U0001f600"`, and by the disassembly
  showing `goroutineheader` calling it. It is *not* exercised through a real
  traceback carrying goroutine labels, because `runtime/pprof.Do` does not
  type-check under goc — `context.Context does not implement context.Context` —
  which is a pre-existing, unrelated defect this did not chase.

- The non-`runtimeAllocation` `printf` path in `builtinPrint` gets the same
  operand sequence and therefore the same separators, but goc always compiles
  with `runtimeAllocation` on, so nothing in this repository's test suites
  executes it. Only its construction is shared, not its verification.

## 22. The export bit meant two things, and the split kept only one (2026-07-31)

`goc -O -runtime pack.gocrt prog.go` did not link, for as long as the split in
§16 has existed. It failed at the system linker naming three symbols:

```
goc-program-runtime.o: in function `reflect_makeFuncStub_abi0':
undefined reference to `reflect_moveMakeFuncArgPtrs'
undefined reference to `reflect_callReflect_abi0'
undefined reference to `reflect_callMethod_abi0'
```

Sixteen capabilities failed this way under `-runtime-opt` — thirteen
`runtime-packages/reflect-*`, `stdlib-crypto/ecdh-x25519`,
`stdlib-encoding/binary` and `stdlib-encoding/binary-varint` — and §5.10 carried
it as unattributed. Neither `-O` alone nor a pack alone reproduces it.

### The two meanings

`ir.Func.Linkage.Export` is read in two places that want different things.

The backend reads it as ELF binding, and for the case that matters here it does
not even need it: `compileToObjectWithBundle` sets a symbol global on its own
when the module's assembly names it (`Global: code.export ||
assemblyReferences[code.name]`, plus a sweep over every symbol at the end). So
the bit is redundant for binding.

`opt.DeadFuncElim` reads it as a keep-alive root: it keeps a function iff the
function is exported or some symbol operand in the module names it. That is the
meaning that carries information, because **a Go function whose only caller is
Plan 9 assembly has no caller in the IR at all.** `reflect.callReflect` is
entered only from `reflect_makeFuncStub`; nothing in Go calls it, and the
optimizer cannot see assembly — the module carries the assembly *files*, not
their translated references. `exportAssemblyReferencedFunctions` is what marks
those functions, and the export bit is the only channel it has.

`finishProgramModule` then wrote

```go
function.Linkage.Export = programSymbols[name]
```

— an assignment. It is answering the question "which symbols did the pack leave
for this module to export", which is a binding question, and the answer
overwrote the keep-alive marker. `opt.OptimizeModule` runs *after* the split, in
`internal/prebuilt.CompileProgram`, so `DeadFuncElim` saw a function with no
export bit and no IR reference and deleted it.

The second-order effect is what produced the actual error text.
`arm64.emitGoABI0AssemblyWrappers` emits the ABI0-to-Go-internal bridge only for
names still in `module.Funcs`, so deleting the Go function also deleted
`reflect_callReflect_abi0`. The sidecar therefore lost the definition and kept
the reference, and the object the linker complained about was the program's own
sidecar. `reflect.moveMakeFuncArgPtrs` is a `PreferDirectABI0` symbol, so the
assembly names it directly and it went undefined by the same route with no
wrapper involved.

### The fix

Exporting is additive for the functions this compilation's assembly names:

```go
function.Linkage.Export = programSymbols[name] || assemblyReferences[assemblySymbolName(function.Name)]
```

Three alternatives were considered and rejected. **`goc/reach.go`'s seeding from
`assemblyReferences`** is not implicated: the functions are present in the module
the front end produces, in both configurations, with the right names — measured,
not assumed. **Preserving every pre-existing export** would also preserve the bit
`ast.IsExported` gives every capitalized Go function, so `DeadFuncElim` would
stop eliminating anything in a split build. **Teaching `DeadFuncElim` about
assembly** would mean giving `opt` a dependency on `plan9asm`; the established
design is "assembly-referenced implies exported implies a DCE root", and the
defect was the split breaking it, not the design.

On the unoptimized path nothing linkage-visible changes, because the backend
already made those symbols global. Measured with two goc binaries built from the
same tree path, each compiling `reflect_makefunc.go` against a non-optimized pack
three times: the pack is byte-identical between them, each compiler's three
images are byte-identical to each other, and the pre-fix and post-fix images
differ in **23 bytes of 11178048** — three `DW_AT_external` flags in
`.debug_info` going 0 to 1 (`obj/dwarf.go:343`), one per affected function, plus
the 20-byte `.note.gnu.build-id` derived from them. The ELF symbol table's names,
values, sizes, types, bindings, section indices and order are identical.

### What it measures

| | |
| --- | ---: |
| capability matrix, `-runtime-opt`, after | **345 subtests, 344 pass, 1 expected failure, 0 fail, 0 known gap** |
| the same matrix, default arm, matched control | 345 subtests, 344 pass, 1 expected failure, 0 fail |
| wall clock, `-runtime-opt` | 222.3 s |
| wall clock, default arm | 203.3 s |
| the 22 capabilities in the affected categories, `-runtime-opt`, before | 6 pass, **16 fail** |
| the same 22, after | **22 pass** |

The "before" row is a targeted re-run of the three affected categories on this
branch with the fix reverted, not a full matrix. The full pre-fix figure is
§5.10's, taken when the matrix held 338 capabilities: 322 pass, 16 fail. The
failing set is the same sixteen in both.

Both matrix figures are one process at `-runtime-status-compile-workers=8` with
a cold pack cache, on a box shared with another job, so they are comparable to
each other and not to §16's or §18's absolute numbers. The optimized arm costs
**9.4% more wall clock** than the default one — the extra is the optimizer
running over both modules, not extra programs.

### Why nobody had seen it

Every CI job and every agent job has run the matrix's default arm. The optimized
arm has never been run to completion by anything. A configuration that ships and
is never run is a configuration whose state nobody knows, which is the general
form of this defect rather than an incidental fact about it.

So the arm is now wired into things that run:

- `make test-goc-status-opt`, sharded by `STATUS_SHARDS`/`STATUS_SHARD` exactly
  like `test-goc-status`.
- a `runtime-status-opt` CI job, four shards, alongside `runtime-status` rather
  than replacing it. The two arms differ in what they eliminate, so neither
  covers the other.

`cmd/goc.TestAnOptimizedProgramKeepsTheFunctionsOnlyAssemblyCalls` is the cheap
guard that does not need the matrix: it builds an optimized pack, compiles a
`reflect.MakeFunc` and method-value program against it with `-O`, links it, runs
it, and checks the output against what the host toolchain prints. It costs about
8 s, and it fails with exactly the three undefined symbols above when the fix is
reverted.

### What is not done

- **A pack is cached without regard to the compiler that built it.**
  `packCacheKey` hashes the runtime-pack version, the target, `-O`, the carried
  package list and the *stdlib source*, and `Manifest.Fingerprint` is the runtime
  source identity. Neither depends on the goc binary, so changing the compiler
  and rebuilding a pack silently reuses the pack the old compiler wrote. It did
  not affect this fix, which only changes the program half, but it will mislead
  the next change that touches `finishRuntimeModule`. Every measurement in this
  section was taken with a per-run `CG12_PACK_CACHE` directory for that reason.

- `DeadFuncElim` does nothing at all on the pack side, because
  `finishRuntimeModule` exports every function it keeps. That is correct — the
  program module must be able to reference any of them — but it means the
  optimized pack is not smaller than the unoptimized one by a single function,
  and nothing records that as a decision.

- The conflation itself is unfixed. `Linkage.Export` still means both "global in
  the object" and "a root for dead-function elimination", and the split now has
  to know about assembly to keep the two apart. A distinct `ir.Func` flag for
  "reachable from outside the IR" would let the split answer only the binding
  question, which is the one it is actually qualified to answer.



## 23. Compiling the same program twice gives the same program (2026-07-31)

§18 fixed four back-end and analysis causes of goc's irreproducibility and left
the rest, correctly, in the front end. §5.10 recorded what remained: 39 of 358
corpus programs varied at all, and `goc/testdata/runtime_defer_capture_allocs.go`
gave **25 distinct executables in 30 compiles**. That is closed. Every one of the
365 corpus programs now compiles to the same bytes every time, with and without
`-O`.

This section is `ccwork/frontend-determinism` and `ccwork/frontend-determinism-2`
together. The first located and fixed causes 1 to 3; the second verified them
against the whole corpus, found cause 4 by auditing the class rather than the
instance, and retired two claims this plan was carrying that turned out not to be
true. **Only cause 1 was ever live for `goc`** --- which is the useful shape of the
result, because it means one map walk whose body emitted an instruction accounted
for every irreproducible goc build.

### Cause 1: variadic interface payload addresses were emitted in map order

`goc/compile.go`, the `...any` path of `callArguments`. When a call passes several
non-direct-interface values to a `...any` parameter and the backing array is
heap-allocated, goc allocates one combined object -- the element array plus one
boxed `payloadN` field per argument that needs one -- and then computes one
address per payload. It recorded those in a `map[int]int` from argument index to
field index, and:

```go
for argumentIndex, fieldIndex := range payloadFields {
        interfacePayloads[argumentIndex] = g.offset(backing, offsets[fieldIndex])
}
```

**`g.offset` emits an `add`.** So the address computations were emitted in map
iteration order, and each `add` takes the next temporary number, so a swap
renumbers every temporary after it in the function. Fixed by recording the pairs
in a slice, which they are already appended to in argument order.

That is the whole of the front-end residue. The diff between two emissions of
`runtime_defer_capture_allocs.go` on the unfixed compiler is **110 lines in three
functions** -- `testing.common.makeTempDir`, `testing.common.checkFuzzFn`,
`testing.common.Attr`, every one a `...any` caller -- and it reads:

```
< 	%t323 =p add %t322, 48        > 	%t323 =p add %t322, 64
< 	%t324 =p add %t322, 64        > 	%t324 =p add %t322, 68
< 	%t325 =p add %t322, 68        > 	%t325 =p add %t322, 48
```

with every later use following the renumbering.

### Cause 2: `opt/inline.go`'s cost-inline tie-break was unstable

`selectCostInline` built its candidate slice by ranging a map and sorted it with
`sort.Slice` on size alone. `sort.Slice` is not stable, so two callees of the same
size came out in whatever order the map put them, and that decides which one is
inlined when the per-caller growth budget runs out. Ties now break on name. The
caller loop walks `m.Funcs` order for the same reason, which only affects
`CG12_DUMP_COSTINLINE`'s output since each caller's budget is independent and
marking a callee is idempotent.

### Cause 3: native standard-library overlays were applied in map order

`applyNativeStdlibOverlays` ranged `loader.units`, a `map[string]*sourceUnit`, and
appended each overlay's functions and data straight to the module. With two
overlay-carrying packages the module's tail, and every address in it, would come
out in map order. It does not bite today -- the only native `.ssa` overlay in the
tree is runtime's, so one package contributes and the walk has nothing to disagree
about -- and is fixed with `orderedUnits` because the next overlay would
reintroduce the whole class silently.

### Cause 4: `opt` numbered phi temporaries in dominance-frontier map order

Found by the audit below rather than by a failing compile, and in the same class as
cause 2. `opt/mem2reg.go` and `opt/jumpthread.go`'s `reconstructThreaded` both
placed phis by ranging `analysis.IteratedFrontier`'s result, which is a
`map[*ir.Block]bool`, and placing a phi calls `f.NewTemp`. So which phi got which
temporary id was decided by map iteration order, and temporary ids reach register
allocation and slot assignment. Both now walk `cfg.RPO` filtered by frontier
membership, which is sound because `DominanceFrontier` only ever adds blocks drawn
from `cfg.RPO`. Phi *placement* is identical either way --- one per (variable,
block) --- so this was a numbering defect and not a placement one.

Like cause 2 it cannot reach `goc`, and it is fixed for the same reason: `cg12cc`
is real. `opt/determinism_test.go`'s
`TestMem2RegPlacesPhisInTheSameOrderEveryTime` promotes one two-diamond function
twenty times in a single process and **fails 5 times out of 5** on the unfixed
pass, at the first attempt each time. `make test-ruby` --- the cg12-vs-gcc
differential, which is the gate for a change to the C path --- is green with it.

**What is not established about it.** No C program in this repository is large
enough for the defect to reach the *emitted assembly*: `difftest/testdata/comp.c`,
`cc/testdata/rubric/int128.c`, `vla.c`, `cmd/viz/testdata/collatz.c` and a
purpose-built four-diamond function with two promotable locals each give one
distinct `-O -S` output in 8 to 10 compiles on the **pre-fix** compiler. So the
defect is demonstrated at the IR level only, and its practical blast radius on
cg12cc's output is unmeasured and may be nil at these program sizes. It is fixed
because the numbering is wrong, not because a symptom was observed.

### The measurement

Every figure below is on `ccwork/frontend-determinism-2`, one tree, one path, on a
box shared with one sibling job.

| measurement | before | after |
| --- | --- | --- |
| `runtime_defer_capture_allocs.go`, `goc -emit-ir`, 3 emissions | 3 distinct module texts | 1 |
| the five `scripts/determinism-check.sh` samples, cold and warm, twice | 4 of 5 | **5 of 5** |
| the same with `-O` | not measured before | **5 of 5** |
| whole corpus, 365 programs x 4 compiles | 39 of 358 varied (§18) | **365 of 365 identical** |
| whole corpus with `-O`, 365 x 4 | not measured before | **365 of 365 identical** |
| 45-program sample x 12 compiles, including every program §5.10 named as skewed | -- | **45 of 45 identical** |
| whole corpus linked against the prebuilt pack, 365 x 6 | not measured before | **365 of 365 identical** |
| the whole thing again with the finished compiler, 365 x 3 and 365 x 3 with `-O` | -- | **365 of 365 identical, same hashes** |

2,920 corpus compiles across the two monolithic sweeps, 2,190 against the pack, 540
in the deep repeat, and 2,190 more re-run from the shipped script against a
compiler built after the last commit: **7,920 compiles, 0 varying, 0 failed**. The
final re-run reproduced all ten sample hashes the earlier binary gave, which is the
`BoundedPipeline` argument for cause 4 confirmed rather than asserted.
`scripts/determinism-check.sh` gained a `-corpus` mode so the sweep is a command
rather than a one-off, and `goc build-runtime` is byte-identical across three cold
builds with `-O` and without --- which matters more than any single program, since
the pack is the largest module goc compiles and every program built against it
inherits its bytes.

`-O` against the pack is deliberately not measured: that is the 16-capability link
failure §5.10 records under `-runtime-opt`, and `ccwork/opt-pack-link` owns it.

### The two claims that were wrong

**§5.10's "441 interface-call wrapper functions land in the module in a different
order on each compile" does not happen.** It was tested where it counts, on the
compiler that *is* irreproducible: `git checkout main -- goc/compile.go` puts the
pre-fix front end back, and three `CG12_NOCACHE=1 goc -emit-ir` emissions of
`runtime_defer_capture_allocs.go` give three distinct module texts -- and the
ordinal position of every one of the 5,942 `function` headers, and of all 1,318
`*.interfacecall.*` wrappers among them, is **identical in all three**. Nothing
moves. The count is not reproducible either; that program has 1,318 wrappers
today, not 441. The likeliest reading of the original observation is that it was
taken at the linked-image level, where a renumbered temporary changes a frame size
and moves every later function's address -- so hundreds of wrappers appear to be
"at different positions" when three function bodies upstream of them changed
length.

**§5.10's "the matrix runs `-O` under `-runtime-opt`, so this matters" was wrong
about cause 2's blast radius, for a reason worth writing down.**
`opt.OptimizeModule` sends any module over `moduleOptimizationFunctionBudget`
(2048 functions) to `BoundedPipeline`, which is `fold`/`copy`/`dce` only. The
smallest program in the goc corpus, `hello.go`, emits **2,739** functions, because
every goc program links the runtime. **No goc module has ever run
`DefaultPipeline`**, so `inline`, `mem2reg`, `jumpthread`, `ifconvert` and `gcm`
are all `cg12cc` paths in practice. Cause 2 is fixed rather than deleted because
`cg12cc` is real; it was never part of `runtime_defer_capture_allocs.go`'s
residue. §5.10 records two more sites in the same class, found by the audit below
and left for the Ruby/C validation cycle they belong to.

### The audit, so this is a class and not an instance

Empirical sweeps only find what the corpus exercises. `golang.org/x/tools/go/packages`
is in the module cache, so a throwaway analyzer type-checked `.`, `./goc/...`,
`./obj/...`, `./internal/gometa/...`, `./ir/...`, `./arm64/...`, `./opt/...` and
`./cmd/goc/...` and printed every `for … range m` whose ranged expression's
underlying type is a map: **108 statements outside tests, 56 of which append**.
All 108 were read.

Everything in the front end and in serialization is safe, and it is worth knowing
*why*, because the safe shapes are the ones to copy: sort the keys before use
(`compile.go:268/866/1000/1023/7353`, `reach.go:867/931/946/980`,
`ir/binary.go:61`, `ir/asm_binary.go:24/34/84/101`,
`internal/gometa/gometa.go:479`); collect into a set that something else sorts
(`compile.go:12786`, `runtime_split.go:300/303`); fill a map rather than a slice
(`compile.go:150/153/360/361/564`); or drain a worklist to a fixpoint, where the
order cannot change the answer (`compile.go:2203`). The one shape that is not safe
is the one cause 1 had: **a map walk whose body emits.**

Note that cause 1's body was an assignment, not an append, so an
`x = append(x, …)` heuristic would have missed it. The signal is not the append,
it is the side effect.

### Validated as a codegen change, not by a green suite

Per §3 step 2 and §14, and re-run on this base rather than inherited, since
`main` moved between the two jobs and §5.14 records two independently-correct
changes composing into a broken compiler.

- A differential program driving the changed path -- an eight-argument `Println`
  and matching `Printf` mixing a `String()`-bearing struct, a plain struct, an
  array, a string, an int, a pointer-derived bool and a nil `error`; the same
  arguments in three orders; a nested `fmt.Sprint`; three `...any` arguments with
  observable side effects, to pin evaluation order; a spread `values...` taking
  the `hasEllipsis` branch instead -- is **byte-identical to `go run`** under
  `goc` and under `goc -O`.

- The **pre-fix** compiler produces a program with the same output, byte for byte.
  The change reorders two `add` instructions with identical operands; it does not
  change what is computed.

- `goc/determinism_test.go`'s `TestCompilingTheSameSourceTwiceGivesTheSameModule`
  compiles one source twice in one process and compares the serialized modules. It
  works because Go randomizes each `range` over a map independently, so two
  in-process compiles draw different traversal orders exactly as two processes do
  -- no repeated linking, no corpus. It **fails 3 times out of 3** with cause 1
  reverted, naming `main.nested` or `main.mixed` (the program's two `...any`
  callers), and **passes 5 times out of 5** with it. It compares `MarshalBinary`
  rather than the printed text, because the text form omits a datum's relocation
  base, pointer words and typelink flag, and those reach the image too. It lives in
  `./goc/...`, so it runs under `make test-goc-corpus`.

### Suites

On `ccwork/frontend-determinism-2`, after all four causes:

| | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | pass, exit 0, 0 FAIL |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 549.755s` |
| `make test-goc-cmd` | `ok github.com/evanphx/cg12/cmd/goc 219.657s` |
| `make test-ruby` | `ok …/difftest 42.175s`, `ok …/cc 15.251s` |
| full unsharded capability matrix, `-v` | 345 subtests, **344 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 SKIP, 0 KNOWN GAP**, `ok … 183.013s` |

The matrix census is taken from the `-v` output --- `=== RUN` lines, `--- PASS`
lines, and the harness's own logged `PASS <program>.go` / `EXPECTED FAILURE` /
`KNOWN GAP` lines --- not from `ok`. The one expected failure is
`defer-panic/panic-string-output` (`runtime_panic_print_string.go`), which exits 2
by design. **No capability is non-passing.**

### What this does not establish

- **A reproducible compile is not a correct compile.** The corpus sweeps compare a
  program against itself, so they cannot see a systematic miscompile; that is what
  the suites, the matrix and the host-toolchain differential are for, and all of
  them were run.

- **The sample depth is finite.** Four to twelve draws per program cannot rule out a
  branch taken 1 time in 100; §5.10's `stdlib_net_mail_textproto.go` was 3 in 53,
  and a 5.7% minority survives 12 draws about half the time. The deep repeat aims
  at exactly the programs this plan named as skewed, and the class audit is what
  covers the rest, but neither is a campaign.

- **`cg12cc`'s reproducibility is not measured, only reasoned about.** Cause 4's
  fix is validated at the IR level and by the gcc differential; no C program here
  is large enough for it to have reached emitted assembly either way. The
  `arm64/mc.go:2786` stack-map root order recorded in §5.10 is still open and is
  `cg12cc`-only for a reason that is measured, not assumed.

## 24. `reportZombies` read a bitmap the sweeper had just cleared (2026-08-01)

`mspan.reportZombies` is what the runtime prints when the sweeper finds an object
marked in this cycle that was free at the end of the last one. §5.11 recorded
that it was blind on every span the Green Tea collector gives inline mark bits —
`gcUsesSpanInlineMarkBits` is `heapBitsInSpan(size) && size >= 16`, so `elemsize`
16 through 512, which is most of the heap and is exactly where §5.11's and
§5.12's faults landed. It printed every object as `unmarked`, never printed the
`zombie` line and never hexdumped the object, while still throwing
`found pointer to free object`.

### The defect

`sweepLocked.sweep` (`stdlib/src/runtime/mgcsweep.go`) does three things in order:

1. line 655 — `if gcUsesSpanInlineMarkBits(s.elemsize) { s.moveInlineMarks(s.gcmarkBits) }`.
   `moveInlineMarks` ORs the span's inline mark bits into `gcmarkBits` and then
   **clears them** (`imb.init(s.spanclass, true)`, `mgcmark_greenteagc.go:215`).
2. lines 660-676 — the zombie *check*, which reads `s.gcmarkBits` directly.
3. line 668/673 — `s.reportZombies()`, which read the marks back through
   `s.markBitsForBase()`. On such a span that returns
   `&s.inlineMarkBits().marks[0]` (`mgcmark_greenteagc.go:239`): the bits step 1
   had just zeroed.

So the check and the report consulted two different bitmaps, and after step 1 one
of them is all zeros. Both `reportZombies` call sites are after the move, so
there is no path on which the inline bits are still live when it runs.

### It is an upstream bug, not a cg12 artifact

`mgcsweep.go`, `mgcmark_greenteagc.go` and `mgcmark_nogreenteagc.go` are
byte-identical to `go1.26.1`'s (`diff` against `$(go env GOROOT)/src/runtime` is
empty), and `goexperiment.greenteagc` is on by default in Go 1.26 —
`build.Default.ToolTags` carries it, which is also why goc selects the same file.
The probe below reproduces it under plain `go build`, with no cg12 anywhere:

| build | `zombie` lines | `marked` | `unmarked` | hexdump |
| --- | --- | --- | --- | --- |
| `go build` (Green Tea, the default) | **0** | 0 | 252 | none |
| `GOEXPERIMENT=nogreenteagc go build` | 1 | 64 | 191 | yes |

The fix is therefore written the way upstream would take it, and the maintenance
cost against a future vendored-tree update is one statement in one function.

### The audit, so this is a class and not an instance

Every reader of a span's mark bits was checked against the `moveInlineMarks`
boundary:

- `mgcsweep.go:559` (specials) and `mgcsweep.go:620` (the
  `traceAllocFree`/`clobberfree`/sanitizer loop) run **before** the move and use
  `markBitsForIndex`/`markBitsForBase`, so they read the inline bits while those
  are still the live ones. Correct — and load-bearing, because a wrong answer at
  :620 would have `clobberfree` scribbling on live objects.
- `mbitmap.go:1495` `countAlloc`, and the zombie check itself, run after the move
  and already read `gcmarkBits`.
- `mgcmark.go:1698` (`greyobject`), `mgcmark.go:1813` (`gcmarknewobject`),
  `mwbbuf.go:249` and `mbitmap.go:1276` all run during the mark phase, where the
  inline bits are the live ones.

`reportZombies` was the only post-move reader still going through
`markBitsForBase`. There is no second instance.

`gcmarknewobject` marking through `markBitsForIndex` also settles the ordering
question: on such a span nothing writes `gcmarkBits` by another route, so
`gcmarkBits` after the move is the complete and only record of the cycle's marks.

### The fix

```go
	// Read the marks out of gcmarkBits rather than through markBitsForBase.
	// On a span with inline mark bits, sweep has already merged them into
	// gcmarkBits and cleared them, so markBitsForBase would return the cleared
	// inline bits: every object would print as unmarked and the zombie the
	// caller detected would never be named or dumped. gcmarkBits is also the
	// bitmap the caller's zombie check consulted.
	mbits := markBits{&s.gcmarkBits.x, uint8(1), 0}
```

replacing `mbits := s.markBitsForBase()`, plus a line on the doc comment saying
`reportZombies` must be called after inline marks have been moved. On a
non-Green-Tea span the expression is exactly what `markBitsForBase` returned, so
that configuration is unchanged by construction.

### Proved on a real fault

A diagnostic that is not exercised on a failure is not fixed.
`cmd/goc/testdata/zombie_report_probe.go` builds a genuine zombie — case 1 of
`reportZombies`' own list — by allocating 64 pointer-free 32-byte objects,
keeping 63 alive in a global, hiding the 64th in a `uintptr` across a collection,
and resurrecting it into a global root before the next. The surviving 63 keep the
span in use; being pointer-free routes the mark through
`tryDeferToSpanScan`'s noscan fast path, so the collector never reads the dead
object and the run fails as a zombie report rather than as
`found bad pointer in Go heap`; and the payload word `0x7a6f6d6269650000 + i`
identifies which object the report named.

Same program, compiled by goc, before and after the fix:

| goc build | `zombie` lines | `marked` | `unmarked` |
| --- | --- | --- | --- |
| before | **0** | 0 | 252 |
| after | **1** | 64 | 188 |

```
0x727a33d024e0 alloc marked
0x727a33d02500 free  marked   zombie
                   7 6 5 4  3 2 1 0   f e d c  b a 9 8  0123456789abcdef
0000727a33d02500: 7a6f6d62 69650028  11111111 11111111  (.eibmoz........
0000727a33d02510: 22222222 22222222  33333333 33333333  """"""""33333333
0x727a33d02520 alloc marked
```

`0x…0028` is index 40, the object the probe hid — the same object, the same
payload and the same shape the `nogreenteagc` reference build printed.

### The blindness covers exactly the inline-mark-bit range

The probe parameterised by object size, compiled by one goc binary against the
reverted and the fixed runtime tree. (The tree is what defines before and after:
goc compiles the runtime from `stdlib/src` at compile time, so two goc binaries
built at different times from the same tree are the *same* runtime.)

| object | `elemsize` | inline mark bits | before | after |
| --- | --- | --- | --- | --- |
| `[2]int64` | 16 | yes, low bound | fault, **0 zombie**, 0 marked, 504 unmarked | fault, 1 zombie + dump, 74 marked |
| `[4]int64` | 32 | yes | fault, **0 zombie**, 0 marked, 252 unmarked | fault, 1 zombie + dump, 64 marked |
| `[64]int64` | 512 | yes, high bound | fault, **0 zombie**, 0 marked, 15 unmarked | fault, 1 zombie + dump, 15 marked |
| `[128]int64` | 1024 | no | fault, 1 zombie + dump, 8 marked | **identical** |

Both directions in one table: the report was blind across the whole
`gcUsesSpanInlineMarkBits` range and only there, and the fix leaves the
already-working large-span path unchanged. `[1]int64` is left out rather than
reported as a zero --- below `maxTinySize` a pointer-free allocation is not its
own heap object, so the probe creates no zombie on either side.

### The guard

`cmd/goc/zombie_report_test.go` compiles the probe with goc, runs it, and
requires a `zombie` line, at least one `marked` object, and the hexdump carrying
the probe's payload word. Checked in both directions: it passes with the fix, and
with `markBitsForBase()` put back and nothing else changed it fails with
`"0" is not greater than "0" / every object printed as unmarked`, which is the
defect's exact signature. A revert cannot pass it.

The fixture is in `cmd/goc/testdata`, not `goc/testdata`, deliberately: the
capability matrix stays at 345 subtests and §23's determinism corpus stays at 365
programs.

Stability, all naming the zombie with the right payload: 60/60 at goc default,
30/30 at `goc -O`, 10/10 at each of `GOMAXPROCS` 1, 2, 4, 8 and 64.

### Suites

On `ccwork/reportzombies`:

| | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | pass, 0 FAIL |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 721.570s` |
| `make test-goc-cmd` | `ok github.com/evanphx/cg12/cmd/goc 292.389s`, including the new guard |
| full unsharded matrix, default arm, `-v` | **345 subtests, 344 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 SKIP, 0 KNOWN GAP**, `ok … 369.507s` |
| full unsharded matrix, `-runtime-opt` arm, `-v` | **345 subtests, 344 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 SKIP, 0 KNOWN GAP**, `ok … 445.036s` |

The census is taken from the `-v` output --- three-part `=== RUN` lines,
`--- PASS` lines, and the harness's own `PASS <program>.go` /
`EXPECTED FAILURE` / `KNOWN GAP` lines --- not from `ok`. The one expected
failure is `defer-panic/panic-string-output`. No capability is non-passing in
either arm.

Determinism, measured on both sides of the change in the same working tree at the
same filesystem path (§23's rule: a worktree at a different path is not a valid
reference build). `scripts/determinism-check.sh` reports every one of the five
sample programs `identical` across all four compiles, before the fix and after it,
with and without `-O`; the digests differ between the two sides because the
runtime source differs. `-corpus -rounds 2 -j 4` on the fixed tree:
`reproducible=365 varying=0 failed=0 of 365 over 2 rounds`, 730 compiles.

### What this does not establish

- **It is a diagnostic, not a collector fix.** No program's behaviour changes
  except the text printed on a fault that already threw. It buys attribution for
  the next fault in this family, nothing more.

- **§5.10's rare hang and rare deadlock are not attributed by it, and structurally
  cannot be.** A hang prints nothing, and `all goroutines are asleep - deadlock!`
  never enters `mspan.sweep`; `reportZombies` runs only on a span already found to
  hold a marked free object. A measurement was taken rather than only an argument:
  the KeepAlive-free `many-goroutines-gc` control at `-O`, 200 rounds per process,
  400 processes at `GOMAXPROCS=4` with a 120s kill, gave **400 clean out of 400**
  --- 80000 rounds, no zombie, no bad pointer, no deadlock, no timeout. That closes
  nothing. §5.11 already had 2000 clean on the same control post-§5.12, and
  §5.10's rates are 1 timeout and 7 deadlocks in 160000 *on the base compiler*.
  400 runs is well below the scale that would resolve either. Both remain open in
  §5.10 with no reducer.

- **The two unattributed survivors in §5.11's middle column stay unattributed.**
  They were not re-run with the working diagnostic; that would be a 2000-process
  campaign against a compiler that no longer exists on `main`.

- **No upstream report was filed.** This environment has no network. The change is
  written the way a CL would be and this section records everything a report would
  need, but somebody has to send it.

- **The corpus determinism sweep here is 2 rounds, not §23's depth.** 730 compiles,
  0 varying. §5.10 records a program that took a minority branch 3 times in 53
  compiles, so 2 draws cannot rule out a rare one. What makes that proportionate is
  that the change is not in the compiler, not that 2 rounds is enough.
