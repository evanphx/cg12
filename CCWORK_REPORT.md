# What survives compilation by goc of log/slog's allocation avoidance

Branch `ccwork/slog-allocations`, off `main` (`a535466`). A MEASUREMENT job: it
builds an allocation benchmark over the paths `log/slog` was designed around,
runs it under goc and under the host Go toolchain, and reports the gap. It
changes no compiler behaviour. Earlier jobs' reports are in git history at
`a535466:CCWORK_REPORT.md`.

Status: COMPLETE. Everything below was run to completion on this machine unless
a line says otherwise.

## The answer, first

    case                goc allocs/op   gc allocs/op   goc B/op   gc B/op
    disabled/3-attr              3.00           0.00      208.0       0.0
    info/5-attr                  9.00           0.00      376.0       0.0

**Almost none of it survives.** All three of the designs named in the brief are
bets on the compiler, and goc loses all three:

  * The **packed `Value`** buys nothing. Its whole point is that `Value.any`
    holds a `Kind` constant instead of a boxed value; under goc that constant is
    a heap allocation, so `slog.Int`, `.Bool`, `.Duration` and `.Float64` each
    cost one allocation where gc costs zero. Only `slog.String` is free, because
    it boxes a pointer.
  * The **disabled-level early return** costs 3 allocations and 208 bytes for a
    call that does nothing. gc reaches zero.
  * The **inline `[5]Attr`** is the one design that partly works: the sixth
    attribute is the first to spill on both compilers. It costs 9 allocations to
    put five attributes into an array that was chosen so it would cost none.

And two of the 32 rows are not numbers at all. goc miscompiles `log/slog` badly
enough that the JSON-handler cases die with `fatal error: invalid pointer found
on stack`. That is section 5, and it is more important than any number here.

## 1. Method

One program, `goc/testdata/slog_allocations/main.go`, compiled and run by both
compilers, measuring itself. Each case is a closure called through a func value,
so neither compiler can see through the call and delete the work. `measure` runs
it 2000 times to warm up, then 5 rounds of 2000, reading `runtime.MemStats`
before and after each round and keeping the round with the fewest mallocs and
the fewest bytes. `runtime.ReadMemStats` stops the world and flushes every mcache
before it answers, so `Mallocs` and `TotalAlloc` are exact counts, not samples.
The minimum over rounds is the right estimator: background runtime work can add
allocations to a round, never remove them.

Same source, same method, both sides. Whatever the instrument gets wrong it gets
wrong equally. One case per process, so a case that kills the runtime costs one
row instead of every row after it.

The instrument is calibrated inside the table. `control/empty-body` must be 0.00
allocations and `control/new-64-byte-object` must be exactly one 64-byte
allocation, under both compilers; both are, so goc's `ReadMemStats` accounts
allocations and bytes the way the reference runtime does and the rest of the
table is measurement rather than artefact. The test asserts these two before it
compares anything.

Host `go1.26.1 linux/arm64`, arm64, 64 cores. One documented command:

    go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations

and to re-record it after a fix:

    go test ./goc -run TestSlogAllocationsAgainstGC \
        -slog-allocations -update-slog-allocations

## 2. The table

Committed at `goc/testdata/slog_allocations_baseline.txt`. `iterations=2000
rounds=5`.

    case                             goc a/op    gc a/op   goc B/op    gc B/op
    control/empty-body                   0.00       0.00        0.0        0.0
    control/new-64-byte-object           1.00       1.00       64.0       64.0
    control/any-int-small                1.00       0.00        8.0        0.0
    control/any-int-large                1.00       1.00        8.0        8.0
    control/any-bool                     1.00       0.00        1.0        0.0
    control/any-string-constant          1.00       0.00       16.0        0.0
    control/any-string-variable          1.00       1.00       16.0       16.0
    control/any-pointer                  0.00       0.00        0.0        0.0
    control/variadic-0-args              0.00       0.00        0.0        0.0
    control/variadic-6-preboxed          1.00       0.00       96.0        0.0
    control/variadic-6-literal           1.00       0.00      176.0        0.0
    control/return-interface             1.00       0.00       16.0        0.0
    control/return-int                   0.00       0.00        0.0        0.0
    control/context-background           1.00       0.00       16.0        0.0
    control/handler-enabled              1.00       0.00       16.0        0.0
    attr/slog.Int                        1.00       0.00        8.0        0.0
    attr/slog.String                     0.00       0.00        0.0        0.0
    attr/slog.Bool                       1.00       0.00        8.0        0.0
    attr/slog.Duration                   1.00       0.00        8.0        0.0
    attr/slog.Float64                    1.00       0.00        8.0        0.0
    info/1-attr                          5.00       0.00      120.0        0.0
    info/3-attr                          7.00       0.00      248.0        0.0
    info/5-attr                          9.00       0.00      376.0        0.0
    info/6-attr                         11.00       1.00      496.0       48.0
    info/3-attr-large-ints               7.00       3.00      248.0       24.0
    logattrs/3-attr                      6.00       0.00      184.0        0.0
    logattrs/6-attr                     11.00       1.00      416.0       48.0
    disabled/no-attrs                    2.00       0.00       32.0        0.0
    disabled/3-attr                      3.00       0.00      208.0        0.0
    disabled/logattrs-3-attr             5.00       0.00      168.0        0.0
    json/kv-4-pairs                     crash       2.00      crash       24.0
    json/logattrs-4-attrs               crash       0.00      crash        0.0

## 3. Which cause each loss is

Three causes, each isolated by a control rather than inferred. Two are the ones
the brief named; the third is new and is the largest single contributor to the
fixed cost of a log call.

### (a) Interface boxing with no static table — confirmed

`control/any-int-small` boxes 7. gc costs nothing because
`runtime.staticuint64s` (`stdlib/src/runtime/iface.go:366`) holds a pre-made
object for every value below 256 and `runtime.convT64` (`:400`) returns a
pointer into it. goc allocates 8 bytes. `control/any-int-large` boxes 2^20,
outside that table, and both compilers allocate 8 bytes.

So the loss is exactly the missing table, not a worse boxing path in general.
`control/any-string-constant` (goc 16 B, gc 0) and `control/any-string-variable`
(both 16 B) say the same for strings, and `control/any-pointer` costs neither
compiler anything, so goc does implement direct interfaces for pointer-shaped
types.

That last fact is what splits the `attr/` rows. `slog.String` boxes a
`stringptr` and is free under goc; `slog.Int`, `.Bool`, `.Duration` and
`.Float64` box a `Kind` constant -- an integer -- and cost 8 bytes each. This is
the packed `Value` design being charged for, directly and per attribute.

The cost is bounded: `info/3-attr-large-ints` costs goc exactly what
`info/3-attr` does, while gc pays 3 extra allocations for it. Values a real
caller logs -- IDs, byte counts, durations -- are outside gc's table too, so on
that row goc's gap is 4 allocations rather than 7.

### (b) The variadic backing array — confirmed, and worse than documented

`control/variadic-6-preboxed` passes six *already-boxed* values to a
`//go:noinline` callee that keeps none of them. Nothing needs converting, so the
only thing left to pay for is the backing array: gc 0, goc 96 bytes, which is six
16-byte interface words exactly. `control/variadic-0-args` costs neither
compiler anything, so it is the array and not the call.

The brief cites `goc/compile.go:3532`, which says the escape summary "does not
describe a variadic parameter". The generated code is more absolute than that.
At `goc/compile.go:6428`:

    stackAllocateVariadic := !g.runtimeAllocation || g.fn.NoSplit || g.forceStackVariadic

`forceStackVariadic` is set only by `runtimeStackVariadicSymbol`
(`goc/compile.go:7916`), a two-name allowlist -- `runtime.traceWriter.event` and
`runtime.traceEventWriter.event`. So a variadic call in ordinary Go code takes
the frame path only when there is no runtime allocator at all or the caller is
`//go:nosplit`. **The escape question is never asked at this site.** Nothing
about `Logger.log` could be learned that would change the answer.

What goc allocates there is not just the array. `goc/compile.go:6431-6458`
builds one synthesized struct per call site, `struct{values [N]any; payload0 T0;
payload1 T1; ...}`, holding the backing array *and* the storage for every
argument that needs boxing, and heap-allocates it in a single `runtime.newobject`.
goc's own decision dump for the benchmark shows it:

    236:42  heap  runtime.newobject  struct_values__2_any__payload0_string__payload1_
    237:42  heap  runtime.newobject  struct_values__6_any__payload0_string__payload1_
    238:42  heap  runtime.newobject  struct_values__10_any__payload0_string__payload1
    239:42  heap  runtime.newobject  struct_values__12_any__payload0_string__payload1

(lines 236-239 are `info/1-attr` through `info/6-attr`.) That is one allocation
per call rather than one per argument -- which is why goc's allocation *counts*
are lower than naive per-box arithmetic predicts and its *byte* counts are
higher. `control/variadic-6-literal` is 176 bytes: `[6]any` (96) plus three
16-byte string payloads plus three 8-byte int payloads, rounded to a size class.

The good news in this is for whoever fixes it: because the boxes live inside the
combined object, proving that one object can stay in the frame removes the
payload allocations with it.

### (c) Returning an interface value allocates — new

`control/return-interface` calls a `//go:noinline` function that takes an `any`
and returns the same `any`. Nothing is converted: the value arrives boxed and
leaves boxed. gc costs nothing; **goc allocates 16 bytes on every call**.
`control/return-int`, the identical call shape with an `int` result, costs
neither compiler anything, so it is returning an *interface* and not returning a
value.

16 bytes is an interface value -- a type word and a data word -- so goc is
materialising the result in heap storage rather than returning it in registers.
The census confirms the shape in the program's own code, where every interface-
returning method has a `runtime.newobject` of the interface type against it:

    ?  main.nopHandler.WithAttrs  runtime.newobject  log_slog_Handler  heap
    ?  main.nopHandler.Handle     runtime.newobject  error             heap

This is the single largest contributor to the fixed cost of a slog call, because
`Logger.log` crosses three interface-returning calls before it does anything:
`context.Background()`, `Logger.Handler()`, and the handler's `Handle` returning
`error`. Each is 16 bytes. `control/context-background` and
`control/handler-enabled` measure two of them at 1 allocation / 16 bytes each.

It also means the cost is not confined to slog. Every `error`-returning call in
a goc-compiled program pays it.

### The decomposition, which closes

Every slog row but one is fully accounted for by (a), (b) and (c) with nothing
left over:

    info/1-attr    5 / 120 = ctx 1/16 + Handler() 1/16 + error 1/16 + combined 1/64  + 1 Kind  1/8
    info/3-attr    7 / 248 = ctx 1/16 + Handler() 1/16 + error 1/16 + combined 1/176 + 3 Kind  3/24
    info/5-attr    9 / 376 = ctx 1/16 + Handler() 1/16 + error 1/16 + combined 1/288 + 5 Kind  5/40
    info/6-attr   11 / 496 = the same, combined 1/352 + 6 Kind 6/48 + Record.back 1/48
    logattrs/3     6 / 184 =           Handler() 1/16 + error 1/16 + combined 1/128 + 3 Kind  3/24
    disabled/none  2 /  32 = ctx 1/16 + Handler() 1/16
    disabled/3     3 / 208 = ctx 1/16 + Handler() 1/16 + combined 1/176
    disabled/lo-3  5 / 168 =           Handler() 1/16 + combined 1/128 + 3 Kind 3/24

`logattrs/6-attr` (11 / 416) leaves one 48-byte allocation unattributed against
that scheme; the analogous `info/6-attr` closes exactly, so the difference is
somewhere in `Record.AddAttrs`'s `slices.Grow` path rather than in the three
causes. Not chased further.

Two things fall out of the decomposition that the raw numbers hide:

  * **The marginal cost of an attribute is 1 allocation and 64 bytes**: one
    8-byte boxed `Kind`, plus 56 bytes of growth in the combined object that
    costs no extra allocation. So the *counts* scale gently and the *bytes* do
    not.
  * **The disabled call's 208 bytes are paid entirely at the call site, before
    slog is entered.** 176 of them are the combined variadic object and 32 are
    two interface-returning calls. `Logger.log`'s early return is doing its job
    perfectly; there is simply nothing left for it to save.

### Where goc matches gc, which bounds the problem

Worth stating as results in their own right: `control/any-pointer`,
`control/return-int`, `control/variadic-0-args`, `attr/slog.String`,
`control/any-int-large` and `control/any-string-variable` all cost the two
compilers the same. goc's boxing is not worse in general, its calls are not
worse in general, and pointer-shaped interface conversion is already right. The
gap is three specific mechanisms, all of them local.

And `Record.front` works: the step from `info/5-attr` to `info/6-attr` gains the
same 48-byte `Record.back` allocation on both compilers. Five attributes really
do fit in the inline array under goc. They just cost 9 allocations to get there.

## 4. What a fix would be worth

Rough, from the decomposition, for `info/5-attr` (9 allocations / 376 B):

  * Fix (c) alone -- return interfaces in registers: **6 / 328**.
  * Fix (b) alone -- ask the escape question at the variadic site, and let one
    that does not escape stay in the frame: **8 / 88**, since the payloads live
    inside the object that moved.
  * Fix (a) alone -- a static table for small integer conversions: **4 / 336**.
  * All three: **0 / 0**, gc's number, since nothing else contributes.

`disabled/3-attr` (3 / 208) goes to **0 / 0** on (b) plus (c) alone; (a) does not
enter it.

## 5. Two miscompiles, reported and not fixed

Found by this benchmark, both reduced, both committed with their controls under
`goc/testdata/slog_allocations/miscompiles/` (README there has the details).
Neither is about allocation. Per the brief, neither is fixed.

### 5a. `slog.Attr` in a frame is scanned as a pointer — serious

    fatal error: invalid pointer found on stack
    runtime: bad pointer in frame main_main at 0x...: 0xc8

Thirteen lines: `slog.Int("k", 200)` passed to a `//go:noinline` function that
calls `runtime.GC()` before touching it. `0xc8` is 200 -- the integer the
attribute carries in `Value.num`, the `uint64` field `log/slog` packs values
into *precisely so that it is not a pointer*. The frame's pointer map says that
word is one, and the collector rejects it. The bad word is in the caller's
frame, so the map is wrong at the value's origin, not at the call.

`attr_bad_pointer_stackcopy.go` reaches the same rejection through
`runtime.adjustpointers` instead, by recursing deep enough to copy the stack. Two
independent walkers disagree with the same map, so the map is what is wrong.

Deterministic: 5 runs out of 5, same offset, same value. This is why both
`json/*` rows are `crash` -- the JSON handler allocates enough and recurses deep
enough to meet a collection with an attribute live in a frame, which nothing
else in the table does. Reading it the other way: **any goroutine holding a
`slog.Attr` in a frame when a collection happens can die.** The rest of the
table only survives because the collections in it happen between calls.

The control (`attr_bad_pointer_control.go`) hand-writes `slog.Value`'s exact
shape -- `_ [0]func()`, a `uint64`, an `any`, behind a `string` key, returned by
value from a non-inlined constructor, held across `runtime.GC()` -- in the
program's own package, and it works. So the zero-length function field alone
does not produce the bad map. I did not find the trigger; that is the next
person's question, and the two programs are in the tree so it can be asked
without rebuilding any of this.

One number was measurable before the crash: a whole-program run got as far as
printing `json/kv-4-pairs` at 15.00 allocations / 456.0 bytes before dying on
the next case. It is not in the baseline, because in the committed one-case-per-
process harness that row crashes too.

### 5b. An interface built in a package-level initializer never registers

    var jsonLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

Nine lines. It links; the first call through `slog.Handler` dies with
`cg12: interface dispatch failed for dynamic type 0x...`. Controls narrow it:
the same expression assigned from inside `main` works; the same conversion
straight into a variable whose declared type is the interface works; the same
shape with a main-package type and a main-package function works; a
main-package type passed to a stdlib function in an initializer works; a stdlib
type passed to a *main-package* function in an initializer fails. So the missed
registration follows the converted type, not the callee, and it is specifically
a conversion at a **call argument** inside a package-level initializer.

`main.go` routes around it by building its loggers in a function. Worth noting
that the workaround was needed at all: the natural way to write a package-level
logger is the way that does not work.

## 6. Proof that no compiler behaviour changed

    go test ./goc -run 'TestAllocationCensus$|TestFrameEscapeAudit$'
    --- PASS: TestAllocationCensus (171.09s)
    --- PASS: TestFrameEscapeAudit (0.00s)

Run on this branch with everything committed. Both reproduce their baselines
exactly. Nothing outside `goc/testdata/slog_allocations*` and
`goc/slogalloc_test.go` was touched; the benchmark lives in a subdirectory so
`filepath.Glob("testdata/*.go")` does not pick it up and the corpus is
unchanged.

## 7. The permanent artefact

  * `goc/testdata/slog_allocations/main.go` — the benchmark, 32 cases, one
    self-measuring program run by both compilers.
  * `goc/testdata/slog_allocations_baseline.txt` — the measured output, both
    columns, committed.
  * `goc/slogalloc_test.go` — `TestSlogAllocationsAgainstGC`, gated behind
    `-slog-allocations` exactly as `TestEscapeDifferentialAgainstGC` is gated
    behind `-escape-gc-differential`. Not wired into any required target.
  * `goc/testdata/slog_allocations/miscompiles/` — the two reductions above,
    each with its control, and a README recording what both compilers print.

The test fails in **both** directions, verified by perturbing the baseline and
reading the failures back:

  * a number that went up: *"goc allocates more than the baseline says ... say
    which cause it is"*
  * a number that went down: *"goc allocates less than the baseline says. That
    is the good direction and it is still a change someone has to look at"*
  * a row that started crashing: *"a case that used to run under goc now kills
    the program. This is a correctness regression"*

and it reports a move in gc's column separately, as a toolchain change rather
than a goc change.

## 8. For the `variadic-allocations` job

These are the before numbers. Re-run is one command
(`go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations`), and the
diff is the evidence. Three things from here that may save that job time:

 1. The variadic placement decision at `goc/compile.go:6428` does not consult
    escape analysis at all; teaching the summary about variadic parameters will
    not change anything until that line asks.
 2. The boxed payloads live *inside* the same object as the backing array
    (`goc/compile.go:6431-6458`), so moving that object to the frame removes
    both costs at once. `info/5-attr` would go from 376 bytes to 88.
 3. Interface-returning calls (cause c) are a separate 16 bytes each and will
    still be there afterwards: 3 allocations of the 9 in `info/5-attr`, and 2 of
    the 3 in `disabled/3-attr`. Fixing variadics alone will not make the
    disabled case free.

## Bottom line

`disabled/3-attr`: **goc 3.00 allocations/op, gc 0.00**. `info/5-attr`: **goc
9.00 allocations/op, gc 0.00**. Of the three designs `log/slog` bets on the
compiler for, the inline `[5]Attr` still spills in the right place and buys
nothing else, the packed `Value` buys nothing at all, and the disabled-level
early return saves nothing because everything it was meant to save has already
been paid at the call site. Essentially none of slog's designed allocation
avoidance survives compilation by goc — and on the one path that exercises a
real handler, the compiled program does not survive either.
