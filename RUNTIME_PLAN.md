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
- the gob round trip reaches an invalid zero `reflect.Value`;
- the runtime trace program runs out of memory or times out;
- two large HTTP programs exceed the current 3 GiB compilation limit;
- the existing ECDSA case remains a known compiler/stdlib gap.

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
- [x] Keep the runtime's `_panic` record stack-resident through
  `unsafe.Pointer` conversions passed to `runtime.noescape`; both panic-time GC
  cases now pass with `GOGC=1`.
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

- [x] Pass notification context, repeated stop/reset, delivery during GC,
  delivery during netpoll, and concurrent atomic contention.
- [x] Keep the temporary slice headers captured by `runtime.selectgo`'s
  synchronous unlock closure on the stack, while still promoting slice backing
  storage assigned to globals.
- [x] Pass the locked-global-select case and the full signal category ten
  executions per program with optimization and `GOMAXPROCS=1`, `2`, and `4`.

Exit criterion: signal tests pass under `GOMAXPROCS=1`, `2`, and `4`, including
race-like stress, without deadlock or runtime lock corruption.

### 5.3 Finalizer resurrection and cleanup

Test basic finalization, resurrection, dependency ordering, clearing a
finalizer, tiny objects, pointerful objects, stack growth in a finalizer,
KeepAlive, cleanup callbacks, and repeated GC cycles. Validate that pointer
liveness at `runtime.KeepAlive` and finalizer registration sites survives
optimization.

Current checkpoint:

- [x] Emit safepoint-specific local stack maps instead of conservatively
  retaining every pointer-bearing local for the entire function. Pointer words
  contained in a live alloca are now expanded into that safepoint's map.
- [x] Pass synchronized finalizer resurrection after explicitly clearing the
  last local pointer in a still-running frame.
- [x] Pass basic finalization, clearing, KeepAlive, pointerful stack growth,
  dependency ordering, tiny-object finalization, cleanup callbacks,
  `Cleanup.Stop`, and finalizer-before-cleanup ordering.
- [x] Pass the complete ten-program finalizer/cleanup batch ten times per
  program with optimization and `GOMAXPROCS=4`.
- [ ] Add multiple-cleanup and `Pinner` lifecycle cases, then repeat the batch
  at the remaining P counts before closing this item.

Exit criterion: finalizer and cleanup tests pass deterministically using bounded
GC/yield loops, and pinner/finalizer/cleanup coverage has deliberate tests for
every supported public behavior.

### 5.4 Reflection/gob failure

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

Exit criterion: trace terminates and produces parseable output within its
budget; all three HTTP programs compile below the agreed memory ceiling and
return coverage packets. The corpus reaches 294/294 covered programs.

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
   correctly reassigned outside runtime scope.
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
