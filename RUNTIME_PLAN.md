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
- finalizer resurrection does not run correctly;
- the runtime trace program runs out of memory or times out;
- the accepted baseline still needs a rerun after recent trace and large HTTP
  fixes;
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
reducers. `runtime-packages/finalizer-resurrect` is now the matrix's only
remaining `knownGap` (§5.3); `defer-panic/panic-string-output` is the one
deliberate `expectedFailure`.

Note also that the coverage baseline covers 294 programs while the matrix now
holds 329 capabilities; the denominators must be reconciled before a new
baseline is accepted.

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
- [ ] Reach 294/294 usable coverage outcomes by repairing the current failures.

### 4.1 Make reports stable and comparable

- Add a report-diff command keyed by function, file, line, and block index.
- Store the accepted Linux/ARM64 baseline in the repository without generated
  binaries or per-program counter blobs.
- Report gained/lost functions and blocks, missing packets, compile failures,
  runtime failures, timeouts, and peak compile/run memory separately.
- Add per-category and per-subsystem summaries in addition to per-file totals.
- Detect source drift so a report from a different copied Go runtime cannot be
  compared silently.
- Make coverage collection opt-in for normal tests and provide a documented,
  resource-bounded command for the complete run.

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
  where it is safe to do so.
- List selected Plan 9 assembly entry points and callers in the report.
- First add scenario coverage for assembly routines such as stack switching,
  signal trampolines, atomics, memmove/memclr, syscall entry, and preemption.
- Later add native cg12 assembly counters at safe basic-block boundaries. Do not
  insert calls or stack-using instrumentation into raw stack-switching code.

Exit criterion: two consecutive full runs have the same denominator, all 294
programs report an explicit compile/run/coverage outcome, and report diffs are
usable in code review.

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
  in Go heap`. **This is not a signal or a 5.2.1 problem**: the same fatal error
  reproduces on the pre-5.2.1 tree, 8 of 60 runs at `GOMAXPROCS=4`, in
  `goroutine/worker-fanin-gc`, which is `mustPass` and clean at the matrix's
  `GOMAXPROCS=1`. The bad pointer is reported with no containing object, so the
  reference is a root — a stale stack address surviving into a scan. It needs
  its own reduction; it blocks this box and the §5.2 exit criterion.

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
`worker-fanin-gc` on the pre-5.2.1 tree). The whole `defer-panic` category (21
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
- [ ] Pass synchronized finalizer resurrection after explicitly clearing the
  last local pointer in a still-running frame. Still failing
  (`runtime-packages/finalizer-resurrect`, `finalizer did not run`), now for the
  understood reason recorded below: the `SetFinalizer` interface temporary's
  address reaches a destructed phi, so its allocation stays conservative.
- [x] Pass basic finalization, clearing, KeepAlive, pointerful stack growth,
  dependency ordering, tiny-object finalization, `Cleanup.Stop`, and
  finalizer-before-cleanup ordering.
- [x] Pass standalone cleanup callbacks (`gc/cleanup-basic`,
  `gc/cleanup-multiple`) and the minimal reducer `gc/cleanup-frame-retention`,
  three runs each, unoptimized and with `-runtime-opt`, at `GOMAXPROCS` 1, 2
  and 4. The three `cleanup-frame-retention` reducers additionally pass six of
  six runs at `GOMAXPROCS=64`, where they failed six of six before the §5.7
  write barrier fix. That failure was §5.7's, not this section's.
- [ ] Pass the complete ten-program finalizer/cleanup batch ten times per
  program with optimization and `GOMAXPROCS=4`.
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
- [ ] Pass the cleanup/finalizer/Pinner status subset three times per program
  with optimization and `GOMAXPROCS=2`.

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
pass. Each file records this in its own header. Note that those headers still
describe the scribble as demonstrating the mechanism, which the diagnostic below
disproved; the headers need correcting.

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

`runtime-packages/finalizer-resurrect` is still failing because of that
boundary, and stays `knownGap`. Its retaining word is the data word of the
interface temporary built for `runtime.SetFinalizer`: the aggregate's address
reaches a destructed phi (the interface nil check), which merges it with an
unrelated value, so the allocation is classified as escaping and stays
conservative. Closing this needs the frontend's escape result, or alloca
lifetime markers, to reach the backend -- the backend cannot recover it
syntactically. That is the next step for this section.

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
return coverage packets. The corpus reaches 294/294 covered programs.

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
   classifications, 294 explicit outcomes.
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
10. Re-run all 294 programs, accept the first stable baseline, and begin the
    allocation/GC phase.

This order deliberately repairs the mechanisms used to diagnose later phases:
unwinding, stack metadata, GC pointer accuracy, atomics, and coverage reporting
must be trustworthy before scheduler and stress results can be interpreted.
