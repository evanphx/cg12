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

The accepted 2026-07-22 coverage run compiled and ran the 294-program runtime
capability corpus, executing each successful binary three times. Its canonical
baseline is `cmd/goc/testdata/runtime_coverage_linux_arm64.json`.

| Measurement | Baseline |
| --- | ---: |
| Programs | 294 |
| Programs returning coverage | 291 |
| Active Linux/ARM64 runtime Go functions | 2,561 |
| Compiled runtime functions | 2,050 |
| Executed runtime functions | 1,269 |
| Active-function coverage | 49.6% |
| Compiled runtime blocks | 25,929 |
| Executed runtime blocks | 7,936 |
| Compiled-block coverage | 30.6% |

The baseline already exercises useful portions of allocation, marking,
sweeping, stack growth, channels, timers, and netpoll. It also exposes failures
that must not be hidden by adding more tests:

- defer/recover programs fail with `no deferreturn`;
- panic-time GC can report an address that is not a stack address;
- signal notification can crash in `internal/runtime/atomic.Xchg8` or runtime
  locking;
- finalizer resurrection did not run correctly. It is fixed and
  `runtime-packages/finalizer-resurrect` is `mustPass` as of 2026-07-28;
  see §5.3;
- the runtime trace program runs out of memory or times out;
- the accepted baseline still needs a rerun after recent trace and large HTTP
  fixes, and the runtime source fingerprint has since moved, so a diff against
  it is now refused rather than silently misleading;
- the ECDSA case was a compiler gap in generic method dispatch. It is fixed and
  `stdlib-crypto/ecdsa` is `mustPass` as of 2026-07-28; see §5.6.

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

The two denominators are now reconciled. There is only one denominator: the
capability matrix *is* the coverage corpus, and every capability reports one
explicit compile/run/coverage outcome, including the ones this environment
cannot run. The accepted baseline covers 294 programs while the matrix holds
338, and that gap is baseline staleness rather than an exclusion set — each of
the 44 capabilities was added after the 2026-07-22 run. They are listed with a
reason in `cmd/goc/testdata/runtime_coverage_baseline_pending.json`, and
`TestCheckedRuntimeCoverageBaselineDenominator` reconciles matrix, baseline, and
list in both directions: 294 + 44 = 338, no capability may appear in both, and
no baseline program may name a capability the matrix has dropped. Adding a
capability therefore requires either accepting a new baseline or recording why
the baseline does not cover it. The list empties when the pending full-corpus
rerun is accepted.

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
- [ ] Reach one usable coverage outcome per capability by repairing the current
  failures. The 2026-07-28 verification run reached 326 of 329: every capability
  compiled and ran, none was skipped or unreported, and the three absences are
  `goroutine/many-goroutines-gc`, `scheduler-stress/gc-churn`, and
  `stdlib-bytes/grow-allocs`, each killed at its 30s timeout with that recorded
  as its reason. All three are the §5.2.1 GC-assist failure, not a collector
  gap, so this box stays open until that runtime work lands.

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
  all six programs that exited non-zero still returned their packets, so the
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

The explicit outcomes hold as well. The 2026-07-28 verification run over all
329 capabilities produced 329 compile outcomes, 329 run outcomes, and 329
coverage outcomes, with a recorded reason on every outcome other than
`collected` and no silent absences. What remains is the runtime repair work that
turns the three classified timeouts into collected packets.

One consequence is worth stating plainly: the runtime source fingerprint has
moved since the accepted 2026-07-22 baseline, so `make runtime-cover-diff`
against that baseline now correctly refuses to compare rather than producing a
misleading delta. Diffing is usable between reports built from the same runtime
source; comparing against the accepted baseline again requires accepting a new
one, which needs the open runtime failures resolved first.

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
  limit. A full corpus rerun is still required before accepting a new baseline.
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
- [ ] Explain the residual `fatal error: found pointer to free object` recorded
  below. It is a different fault with a different traceback and is roughly two
  orders of magnitude rarer; it is not this section's bug and is not known to be
  fixed.

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
- A different fault survives, and it is **pre-existing and unaffected by this
  change**: `fatal error: found pointer to free object`. This section originally
  named it `marked free object in span` (from `mspan.reportZombies`); the
  independent verification of 2026-07-28 saw that string zero times in roughly
  600000 executions and identified the surviving fault as the one named above.
  Measured with matched 160000-run controls on a KeepAlive-free variant of the
  reducer, compiled by the merge-base and by the fixed compiler from
  sha256-identical sources: 19 versus 20 occurrences, plus a residual
  `found bad pointer in Go heap` at 4 versus 4 and rare hangs. Indistinguishable,
  which is what establishes it as pre-existing rather than introduced here. On
  `many-goroutines-gc` itself it ran 11 of 24000 on the merge base and 4 of 24000
  on the fix. It has not been reduced, it is about two orders of magnitude rarer
  than the fault this section closes, and it needs a reducer of its own before it
  is worth measuring.

Exit criterion: no global data word ever receives a goroutine stack address
(checkable with `GODEBUG=cg12checkwb=2` over the corpus), `gc/keepalive-stack-root`
passes at every `-runtime-procs` setting optimized and unoptimized, and the
`found pointer to free object` fault above is reduced and attributed.

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

Still open: `println` with several operands does not print the spaces the spec
requires (`println("a", 1)` gives `a1`). Found while reducing this bug, unrelated
to it, and not fixed here.

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

1. **M0 — coverage is reproducible:** stable report/diff, checked baseline,
   classifications, one explicit outcome per capability.
2. **M1 — current failures are green:** defer/panic, signal, finalizer, gob,
   trace, HTTP compiler memory, and the existing ECDSA gap are resolved or
   correctly reassigned outside runtime scope. The ECDSA gap is closed: it was
   a generic method-dispatch bug in the compiler, not runtime scope at all
   (§5.6). The `sweep increased allocation count` failure is closed: it was a
   missing `NotInHeap` write-barrier exemption in the compiler (§5.7). Signal
   delivery during GC (§5.2) and the cleanup/finalizer over-retention (§5.3)
   are still open.
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

Start with the following bounded sequence:

1. Add the stable report-diff and uncovered-function classification format.
2. Reduce the smallest `no deferreturn` failure and add defer/frame/stack-map
   validation around it.
3. Fix the panic/defer unwind path, then run every existing defer/panic case.
4. Fix panic-time GC stack scanning using the same frame validation.
5. Reduce and fix the signal/atomic crash.
6. Fix finalizer resurrection and cleanup liveness.
7. Reduce the gob reflection failure.
8. Separate and fix trace runtime growth from compiler memory growth.
9. Profile and reduce the three HTTP compilation OOMs.
10. Re-run the complete capability matrix, accept the first stable baseline, and
    begin the allocation/GC phase.

This order deliberately repairs the mechanisms used to diagnose later phases:
unwinding, stack metadata, GC pointer accuracy, atomics, and coverage reporting
must be trustworthy before scheduler and stress results can be interpreted.
