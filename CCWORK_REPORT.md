# Dead frame slots, and the direction the allocation record reports

Branch `ccwork/dead-slots-and-census-direction`, off `main` (6b9fbb0).

Two changes, reported in the order they were made rather than the order they
were asked for:

  - **Part 2 first** — the allocation census learns to record the front end's
    own frame placements, so that an object moving from a front-end frame slot
    to the heap is reported as `frame -> heap` instead of as a site that
    appeared out of nowhere. This had to come first: Part 1 is a change to the
    front end's frame slots, and reading its effect on the census requires the
    census to be able to say what happened.
  - **Part 1** — the front end stops emitting a composite literal's frame slot
    before it knows whether the object is going in the frame at all.

Host toolchain: `go1.26.1 linux/x86_64` unless a section says otherwise.

*(written as the work proceeds; sections are filled in as each measurement
completes)*

---

# Measurement 0 — how many dead frame slots there are

Before anything was changed. The measurement compiles all 398 corpus programs
with `goc.CompileExecutable`, which is the same entry the corpus audits use, and
counts every `OAlloc4/8/16` instruction whose result temporary is never read:
not as an operand of any instruction, not as a phi argument, not as a branch
argument.

    programs                                398
    alloc instructions, all programs   9 833 082
    dead occurrences                      51 928
    distinct dead sites                    2 880
    dead GC stack pointer words          105 096

A "site" here is (source position, containing IR function, op), deduplicated
across the programs that share stdlib code; an "occurrence" is one per program
that produced it.

The 105 096 figure is the second cost and the one that is not obvious. Three of
the four sites in Part 1 call `visitPointerWords`/`markStackPointerWord` on the
slot *before* the escape decision overwrites it, so `ir.Func.StackPointerWords`
keeps an entry for a temporary that no instruction defines any more.

Note on when a dead slot costs anything: `opt.Optimize`'s DCE would remove a
dead `OAlloc` (`hasSideEffect` does not list `IsAlloc`), but `opt.OptimizeModule`
runs only under `goc -O`, and `opt.Optimize` returns early for any function with
a secondary entry (defer/recover). Unoptimized builds keep every one of them,
and those are what the corpus audits, the census and the differential all
measure.

---

# Part 2 — the allocation record now names the direction

## 2.1 What was wrong

`opt.AllocationCensus` builds its two halves from two places. Heap placements
are read out of the finished IR, so every allocator call is a line whoever
emitted it. Frame placements come from `module.AllocDecisions`, which only
`opt.LowerHeapAllocations` writes -- so the census saw the IR pass's frame
placements and none of the front end's.

goc's AST walk commits some allocations to a frame itself, at six sites that
call `recordPlacement(..., ir.AllocInFrame, ...)`. Those records existed (they
are what `opt.ShadowPlacement` runs on) and the census ignored them. So a site
the front end framed had no census line at all; when a stricter escape rule
moved it to the heap it gained an allocator call, and the reporter -- which had
never seen the site before -- filed it under **appeared**, the bucket whose
question is "is this new code or a new allocation in old code". The answer was
neither: it was an object that moved, and the `frame -> heap` bucket stayed
empty while it happened.

## 2.2 The fix

`ir.PlacedAlloc` gains an `Allocator` field and starts carrying its `Type`, both
filled in by `goc`'s `recordPlacement`. Neither costs anything at compile time:
the type-descriptor symbol is `contentSymbolName(".goc.runtime.type",
goTypeKey(...))`, a pure function of the type, computed **without** interning or
emitting the descriptor -- a diagnostic record must not add data to the module,
and a frame placement is exactly the case where no descriptor is needed.

`opt.AllocationCensusWith(module, opt.AllocationCensusOptions{
IncludeFrontEndFrameSlots: true})` then records those frame placements as census
lines. `opt.AllocationCensus` is unchanged and still excludes them.

Why the type and allocator are the whole point: a census site is
`position TAB function TAB allocator TAB type`, and the direction of a move is
only expressible if the frame record and the heap record of one decision
produce the *same* site string. `runtime.newobject` and the descriptor symbol
are what the heap side is identified by, so the frame side has to name them too.
A front-end frame placement that cannot name an allocator is still left out --
`string-conversion-buffer`'s heap form is a nil argument and an allocation
inside `runtime.stringtoslicebyte`, which is not a census site on either side,
so a line for it could only ever vanish, never move.

## 2.3 Proof that it is right, not a guess

Two independent measurements.

**The site identities really do coincide.** Turning the option on over the
corpus adds 2 971 lines and removes none. 2 761 of them are at sites the census
had never seen. The other **210 are at sites that already had a `heap` line**
and now read `frame+heap`: the same position, the same function, the same
allocator and the same type, decided one way in one inlined copy and the other
way in another. Those 210 are the identity claim demonstrated rather than
asserted -- the frame record computed from `ir.PlacedAlloc` lands on a site
string that an allocator call read out of the IR had already produced.

**The direction is now reported.** A temporary knob (removed again; see 2.5)
made every front-end escape predicate answer "escapes", which is the class of
change the escape-publication fix made. The corpus was compiled twice, and both
censuses were run through `compareAllocationCensus` -- the reporter a reviewer
actually reads -- over the 398 programs, all of which compiled both ways:

    census                    heap -> frame   frame -> heap   appeared   vanished
    without the option              0               0            451         0
    with the option                 0             317             84         0

Before: 451 moves, every one of them filed as "appeared", and the frame-to-heap
bucket empty. That reproduces the reported defect exactly, at 451 sites instead
of 23. After: 317 of them are named `frame -> heap`.

## 2.4 The 84 that are still "appeared", and why they are a different thing

Every one of the 84 was read back to its source line:

    63   var declaration          var b [utf8.UTFMax]byte
    13   short declaration        x25519Basepoint := [32]byte{9}
     6   func declaration line    parameter/receiver storage
     2   source not readable

These are **local variables**, not placement decisions. `variableStorage`'s
escaping arm allocates with `allocateTyped` -- the neutral `OHeapAlloc`
candidate -- so the heap side is an `AllocDecision` and gets a census line,
while the frame side is `allocLocal`: an ordinary frame slot with no type, no
allocator, and nothing recorded. 76 of the 84 arrive as `frame+heap` rather
than `heap`, which is the fingerprint of the candidate path: the IR pass
promoted some copies back into frames.

Closing those 84 means recording every local variable's slot. That is the
category the census rejects by design, and the price is why: the corpus emits
9 833 082 alloc instructions across 398 programs, about 24 700 per program,
against a census of 17 817 lines. It would be a two-orders-of-magnitude file
that no one would read as a diff, to name a frame slot that carries no type to
be named by. Not done, and it should not be.

**A liveness pass would not help here.** The brief asks whether one is needed to
tell a move from a genuinely new allocation, and the answer is no: the missing
ingredient was identity, not liveness. Knowing that a frame slot is live tells
you nothing about which type it holds or which allocator its heap form would
call, so it cannot produce a site string that unifies with the heap record --
which is the only thing that turns two events into one move. Its cost is not the
objection: the dead-slot scan in Measurement 0 *is* a liveness pass, about eighty
lines, one walk of each function, negligible next to compilation. It is simply
the answer to a different question, and that question is Part 1's.

## 2.5 What was temporary

The forced-heap knob was three `if forceFrontEndHeap { return false }` guards in
`nonEscapingAddress`, `makeResultDoesNotEscape` and `valueDoesNotEscape`, plus
the `var` reading the environment. All four are gone from the tree; `git diff
main -- goc/compile.go` contains none of them. It existed only to produce the
table in 2.3, which needs a change of that class and could not be produced by
reverse-applying the historical one: `git apply -R -3` of 6245dbb's
`goc/compile.go` hunks onto today's tree conflicts, and hand-resolving a
322-line escape-analysis patch to reconstruct a "before" would put more
uncertainty into the measurement than it takes out. The knob's version of the
change is bigger and blunter than the historical one -- 451 moves rather than
23 -- which is the direction of error that makes the conclusion safer, not
weaker.

---

# Part 1 — the front end stops emitting a slot before it knows where the object goes

## 1.1 The shape of the defect

Four sites in `goc/compile.go` allocated a frame slot, then asked whether the
object escapes, then overwrote the variable holding the slot with a heap
allocation:

    backing := g.localAlloc(align, int(size))     // emitted unconditionally
    visitPointerWords(t, 0, ...markStackPointerWord(backing)...)
    if heap {
        backing = g.allocateEscapingTyped(t)      // the slot is now unreachable
        ...

The `OAlloc` stays in the finished IR with zero uses. The sites, located by
reading the code rather than by trusting the line numbers in the brief (which
had moved):

  - `expr`, `&T{...}` for a slice or map literal — the descriptor slot
  - `methodValue` — the method-value descriptor
  - `compositeLiteral`, the slice arm — the backing array
  - `compositeLiteral`, the struct/array arm — the literal's storage

The last two also registered the slot's pointer words with
`markStackPointerWord` before overwriting it, leaving `StackPointerWords`
entries keyed by a temporary that no instruction defines.

## 1.2 Which fix, and what the other one costs

Not emitting the slot until the placement is known. The four edits are the same
edit: hoist the type computation, declare `var backing ir.Ref`, and allocate
inside the arm that keeps it — for the two array sites, moving
`visitPointerWords` into the frame arm with it, since stack pointer words
describe a stack slot and there is no longer one in the other arm.

Eliminating instead was priced and is not simpler:

  - goc's own pipeline (`compile.go`'s `InlineNoSplitCalls` →
    `InlineHeapAllocations` → `LowerHeapAllocations`) has no DCE, so a pass
    would have to be added to it, with a position in that order to argue for.
  - `opt.DCE` exists but is reached only through `opt.OptimizeModule`, which
    `cmd/goc` runs under `-O`. Every unoptimized build — which is what the
    corpus audits, the census and the gc differential all measure — keeps the
    slots.
  - `opt.Optimize` returns early for any function with a secondary entry, so
    even under `-O` every function containing a defer/recover keeps them.
  - A dead `OAlloc` and a live one are the same instruction; a general pass
    removing dead allocations is a real change to every function in the module,
    not a targeted fix. Four `var` declarations are less code and less risk.

So: not emitting wins on both counts, and elimination was not taken.

## 1.3 What it removed

The same measurement as Measurement 0, re-run on the fixed tree:

                                    before        after      delta
    distinct dead slot sites         2 880          403     -2 477
    dead occurrences                51 928       19 643    -32 285
    dead GC stack pointer words    105 096       19 643    -85 453
    alloc instructions, corpus   9 833 082    9 800 801    -32 281

No site gained dead occurrences and no new dead site appeared: of the 2 880
sites, 2 477 are gone entirely and the remaining 403 are unchanged, occurrence
for occurrence. The four sites accounted for 86% of the corpus's dead frame
slots and 81% of its dead GC stack words.

The ten dead slots the brief counts are the subset of these that the
escape-publication fix's 23 moved allocations produced. This measurement is over
the whole corpus at once rather than over one commit's delta, so it is a
different and larger number of the same thing; both are the composite-literal
sites leaving a slot behind.

## 1.4 The 403 that remain, and why they are not this change

Read back to their source lines, they are almost entirely multi-value
assignments and calls:

    218  short declaration     key, value, residual, err := parsePAXRecord(sbuf)
    116  other (assignment)    k, v, ok = strings.Cut(rec, "=")
     24  return statement      return ek.encapsulate(&cc)
     18  if statement          if a, b, err = f(); err != nil {
     15  source not readable
     12  no source position

All 403 are the same shape: `alloc8` with exactly one pointer word, at the
position of a call whose results are being bound. None is at one of the four
sites fixed here, none is a placement decision (`placed=false` for every one),
and none is overwritten by an escape decision -- the slot is reserved for a
call result and then not used, because the value is consumed as an SSA
temporary instead. That is a different defect with a different fix (how call
results are materialised), and it is not this change. They are named here so
the residual is a known quantity rather than an unexplained one.

## 1.5 Part 1's effect on the census: five lines, reviewed

With Part 2 in place the census can be read across Part 1. It moved five lines,
all additions, all in one program, and none of them a placement change:

    + testdata/runtime_cleanup_frame_retention_masked.go:51:1   main.main  runtime.newobject  chan_struct                   heap
    + testdata/runtime_cleanup_frame_retention_masked.go:52:9   main.main  runtime.newobject  main_maskedBox                heap
    + testdata/runtime_cleanup_frame_retention_masked.go:53:26  main.main  runtime.newobject  struct_code_uintptr__capture0 heap
    + testdata/runtime_cleanup_frame_retention_masked.go:53:65  main.main  runtime.newobject  struct                        frame
    + testdata/runtime_cleanup_frame_retention_masked.go:54:9   main.main  runtime.newobject  main_maskedBox                heap

Site by site: these are the same five source positions that already appear in
the baseline attributed to `main.registerWithTrailingAllocation`, with the same
allocators, the same types and the same placements, now *also* attributed to
`main.main`. Those five original lines are still there; nothing was removed.

That is one inlining decision changing. `registerWithTrailingAllocation` lost
the dead slots from its body, dropped under the inliner's size budget, and was
spliced into `main`; the census counts a site once per function it lands in
after inlining, which is the documented behaviour and the reason a constructor
inlined into three callers is three sites. Its three near-identical siblings
(`registerWithKeepAlive`, `registerTwoObjects`, `registerFinalizerOnly`) did not
cross the threshold and are unchanged.

Nothing moved between the frame and the heap: `heap -> frame` and `frame -> heap`
are both empty for Part 1.

Because that program exists to test whether a stale frame word retains an object
past a `runtime.AddCleanup`, and inlining changes `main`'s frame, it was run
rather than reasoned about: it prints nothing when all four cleanups run, and it
printed nothing, both unoptimized and under `-O`.

## 1.6 The census delta as a whole

    committed on main                14 846 lines
    after Part 2 alone               17 817 lines   (+2 971, all frame, none removed)
    after Part 2 and Part 1          17 822 lines   (+5, the inlining above)
    ------------------------------------------------------------------
    committed on this branch         17 822 lines   (+2 976 against main)

The +2 971 are front-end frame placements the census now records. 2 761 are at
sites it had never seen; 210 are at sites that already carried a `heap` line and
now read `frame+heap`. Nothing was removed and no placement changed, which is
the check that this is a change in what is recorded rather than in what the
compiler does.

---

# The gc differential

Regenerated from the new census (the differential reads goc's side out of the
committed baseline, so it had to be). Host toolchain `go version go1.26.1
linux/arm64`, the same one the committed file records, so nothing here is a
toolchain difference.

    coverage                                  before    after
    census rows joined                          2 791    3 053
    census rows outside the corpus directory   10 279   12 991
    PERMISSIVE (gc heaps, goc frames)             236      267
    PESSIMISTIC (goc heaps, gc frames)            574      574

The pessimistic set did not move at all. The permissive set gained 31 lines and
lost none, and 20 further lines were **relabelled**:

    runtime_core_types.go:24                absent -> heap   becomes   frame -> heap
    runtime_reflect_call_aggregate_matrix.go:103    "                     "
    stdlib_container_heap.go:31                     "                     "
    stdlib_image_gif_animation.go:27,28,30          "                     "
    stdlib_io_readall_limited_reader.go:9           "                     "
    stdlib_netpoll_syscall_socket_listen.go:19      "                     "
    stdlib_slog_structured.go:36                    "                     "
    sync_pool_interface.go:43                       "                     "
    ... 20 in total, one of them "absent -> mixed" becoming "frame -> mixed"

That is the same defect as Part 2, one level up. The differential's own text
says it includes lines where goc's census says nothing, because "no record" was
consistent with "framed without ever being called a candidate". At those 20
lines goc *was* framing the object, and the differential can now say so instead
of recording an absence.

The 31 new lines are all `mixed -> heap` (30) or `mixed -> mixed` (1) on goc's
side. None is a line whose goc verdict changed from heap to frame: they are
lines that allocate more than one object, where goc heaps one and frames
another, and where only the heaped one used to be visible. With the framed one
recorded the line's goc verdict becomes "mixed", and a line that is partly
framed where gc heaps everything is by definition a permissive disagreement.
So the +31 is the instrument seeing 262 more goc decisions, not goc making
different ones.

Nothing in either part changed a placement anywhere except the one inlining in
1.5, and the pessimistic count staying at exactly 574 is the independent check
on that.

---

# Guards

Every one run to completion on this tree and watched exit. The gate job's
`go test ./goc/...`, the capability matrix and `make test-unit` were not run
here, as asked.

| guard | result |
| --- | --- |
| `TestFrameEscapeAudit` | PASS — no frame address published past its frame |
| `TestAllocationCensus` | PASS against the regenerated baseline (181 s) |
| `TestCompareAllocationCensusNamesTheDirection`, `...ReportsASplitSite` | PASS |
| `TestEscapeShadowPlacement` | PASS — `escape_shadow_baseline.txt` did not move |
| `TestLoopBodyAllocationsAreDistinctPerIteration` | PASS, all 6 programs, unoptimized and `-O` |
| `TestLoopAliasExpectationsMatchTheHostToolchain` | PASS — the literals are still `go run`'s own output |
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` | PASS — determinism holds |
| `TestEscapeDifferentialAgainstGC` | PASS against the regenerated differential |
| `opt` census unit tests (4 new) | PASS |

**`loop_alias_frame_local.go`'s allocations stay in frame slots**, and this is
now *positively* stated rather than inferred from an absence. On `main` that
program contributed no census line at all. It now contributes

    testdata/loop_alias_frame_local.go:53:8	main.literalWithin	runtime.newobject	main_point	frame

-- the `&point{x: i, y: i * 2}` in the loop body, recorded as framed. Before
this branch, that literal moving to the heap would have shown up as a site
appearing; now it would be a `frame -> heap` line at a site the baseline names.
The guard got stronger as a side effect of Part 2.

**Determinism.** The census output is sorted and deduplicated by key, and the
new front-end frame records are read out of a Go map, so `sortedAllocationIDs`
orders the ids before recording rather than relying on two duplicates happening
to be identical. The regenerated baseline is byte-sorted, and
`TestCompilingTheSameSourceTwiceGivesTheSameModule` compares two compiles by
SHA-256 of the marshalled module.

**One control worth naming.** Before any change, the census produced by this
tree was compared against the committed `alloc_census_baseline.txt` and was
**identical, all 14 846 lines**. Every delta reported above is therefore this
branch's, not a pre-existing drift.

---

# Summary

Branch `ccwork/iface-init-dispatch`, off `main` (`4a6fd96`). The reduction this
starts from was committed, unfixed, by the `slog-allocations` job and lives at
`goc/testdata/slog_allocations/miscompiles/pkginit_dispatch.go`. Earlier jobs'
reports are at `4a6fd96:CCWORK_REPORT.md`.

Status: COMPLETE, as of that branch's own run; the integrated tree is Part 0.

## 0. The defect, reproduced on main before anything was changed

At `4a6fd96`, with nothing changed:

    $ go run ./cmd/goc -run goc/testdata/slog_allocations/miscompiles/pkginit_dispatch.go
    cg12: interface dispatch failed for dynamic type 0x8512f8
    fatal error: cg12: interface dispatch failure
      log_slog_Handler_Enabled
      log_slog_Logger_Enabled
      ...
    $ go run goc/testdata/slog_allocations/miscompiles/pkginit_dispatch.go
    json ok

## 1. The shape survey, measured on main

The brief named four neighbouring shapes to check. I measured those and seven
more. Each row is a nine-to-fifteen-line program built on the same
`*log/slog.JSONHandler` -> `log/slog.Handler` conversion (row I uses
`*strings.Reader` -> `io.Reader`), differing only in where the conversion sits.
The `goc` column is `go run ./cmd/goc -run`, `gc` is `go run`, both at `main`
`4a6fd96`.

| # | shape | goc on main | gc |
|---|---|---|---|
| A | call argument in a package-level `var` initializer | **dispatch failure** | ok |
| B | call argument inside `func init()` | ok | ok |
| C | slice composite literal at package scope | ok | ok |
| C2 | struct composite literal at package scope | ok | ok |
| D | method value taken at package scope | ok | ok |
| E | call argument nested inside a package-scope composite literal | **dispatch failure** | ok |
| F | call argument inside a package-scope function literal | **dispatch failure** | ok |
| G | assignment inside a package-scope function literal | **dispatch failure** | ok |
| H | `return` inside a package-scope function literal | **dispatch failure** | ok |
| I | *variadic* call argument in a package-level `var` initializer | **dispatch failure** | ok |
| J | `var` spec inside a package-scope function literal | **dispatch failure** | ok |

Of the four shapes the brief asked about, **one was broken** (A, the package-level
`var` initializer) and **three were already sound** (B `init()`, C/C2 composite
literal at package scope, D method value at package scope). B is sound for a
different reason from C/C2/D, and the difference is the whole story:

  * `func init()` is an ordinary top-level `*ast.FuncDecl`, so it is a *root* of
    the reachability walk and its body goes through the full function-body
    walker. Nothing about it is special-cased; it is simply not on the
    initializer path at all.
  * composite literals at package scope and method values at package scope are
    the two implicit-conversion sites the initializer walk already handled
    (`enqueueCompositeImplementations`, and the identifier case that enqueues a
    referenced `*types.Func`).

The seven extra rows (E, F, G, H, I, J and the variadic half of A) are shapes
the brief did not name and that were also broken. They are the same defect from
a different angle: implicit conversions the *function-body* walk handles and the
*initializer* walk did not.

## 2. Root cause

`goc/reach.go` has two walks that decide which concrete methods are reachable,
and so which dynamic types the generated dispatcher gets an entry for
(`interfaceMethodCandidates` in `goc/compile.go` admits a candidate only if its
method is in the reachable set):

  * `processQueue`, over function bodies. It handled conversions at composite
    literals, assignments, `var` specs, `return` statements, channel sends,
    explicit `T(x)` conversions, **and call arguments**, including variadic ones.
  * `enqueueGlobal`, over package-level `var` initializer expressions. It
    handled only the conversion to the variable's own declared type, explicit
    `T(x)` conversions, and composite literals.

Call arguments were the missing site, and they are the commonest one: the
natural way to write a package-level interface value is
`var x = f(concreteValue)`. Nothing else in such a program converts that
concrete type, so if the argument site is not what registers it nothing does,
the dispatcher is generated with no entry for the type, and the first call
through the interface reaches `runtime.gocInterfaceDispatchFailure`.

The same divergence explains E through J: any implicit-conversion site inside a
package-scope composite literal or function literal is inside an initializer
expression, so it is walked by `enqueueGlobal`, which did not know about it.

The user's framing was that the pass collecting itabs and dispatch wrappers
"walks function bodies but misses, or mis-scopes, the synthesized initializer
function". That is close but not quite it, and the difference matters for where
the fix goes. `interfaceItabs`, `interfaceMethods`, `interfaceCallWrappers` and
`interfaceDispatchers` in `goc/compile.go` are all downstream: the itab and the
call wrapper are made on demand at the conversion site, and the dispatcher's
candidate list is filtered in `interfaceMethodCandidates` by whether the
candidate method is in the *reachable* set. So none of those four maps was
wrong. The reachable set was, and it was wrong in `goc/reach.go`, one pass
earlier. The initializer is not mis-scoped either -- it has its own walk,
`enqueueGlobal`, which runs and does find things. It just had a shorter list of
sites than the walk next to it.

## 3. The fix

`goc/reach.go`: the two site lists are now one list, `enqueueStatementConversions`
plus `enqueueConversionCall` and `enqueueCallConversions`, called from both
walks. The function-body walk's behaviour is unchanged -- the extracted code is
the code that was there, called from the same points in the same order, so the
queue order it produces is byte-identical. The initializer walk gains the sites
it was missing.

131 lines added, 91 removed, all in `goc/reach.go`.

After the fix, all eleven shapes from the survey pass under goc, as does the
original reduction:

    $ go run ./cmd/goc -run goc/testdata/slog_allocations/miscompiles/pkginit_dispatch.go
    json ok

The regression test is `goc/testdata/runtime_package_initializer_dispatch.go`,
in the corpus, with a `core-types/package-initializer-dispatch` entry in the
capability matrix. It carries seven of the shapes, each with a *different*
interface and a *different* standard-library concrete type -- `io.Reader` and
`*strings.Reader`, `io.Writer` and `*bytes.Buffer`, `fmt.Stringer` and
`fs.FileMode`, `io.ByteReader` and `*bytes.Reader`, `io.ByteWriter` and
`*bufio.Writer`, `io.StringWriter` and `*strings.Builder`, `io.RuneReader` and
`*bufio.Reader` -- so that no shape can pass on another shape's registration.
It was committed failing (`0f80c37`), ahead of the fix (`5c10aa4`).

### What is still only in the function-body walk

`enqueueGlobal` still does not enqueue the runtime helpers the body walk
enqueues for channel operations, `make`/`new`/`close`, string-slice conversions
and `range` over a string or map. I probed that: a package-scope function
literal that makes a buffered channel and sends on it, and one that round-trips
`[]byte` through `string`, both run correctly on `main` and after the fix, so
those helpers are reaching the link some other way and this is not a second
latent miscompile of the same shape. It is left alone rather than "fixed"
speculatively.

## 4. Guards

**Loop aliasing against the host toolchain: clean.**

    $ go test ./goc -run 'TestLoopAliasExpectationsMatchTheHostToolchain|TestLoopBodyAllocationsAreDistinctPerIteration' -count=1
    ok  github.com/evanphx/cg12/goc

All four programs -- `loop_alias_forms.go`, `loop_alias_composite.go`,
`variadic_backing.go`, `loop_alias_frame_local.go` -- pass, unoptimised and
under `-O`.

**Determinism: holds.**

    $ go test ./goc -run TestCompilingTheSameSourceTwiceGivesTheSameModule -count=1
    --- PASS (4.94s)

**`TestFrameEscapeAudit`: clean.**

    $ go test ./goc -run TestFrameEscapeAudit -count=1
    ok  github.com/evanphx/cg12/goc	184.174s

`goc/testdata/frame_escape_baseline.txt` is unchanged: nothing in the corpus
publishes a frame address anywhere it did not before, and nothing stopped.

**`TestAllocationCensus`: moved, regenerated, delta reviewed.**

`goc/testdata/alloc_census_baseline.txt` gains 24 lines and loses 12. The delta
is two things and no third thing.

*Twelve lines become eleven, all in `net/http`'s bundled HTTP/2, all renames.*
Every removed line has an added line with the same file, the same line and
column, the same allocator, the same type and the same `heap` decision:

    - h2_bundle.go:5083:29  net/http.methodvalue...onSettingsTimer.4961.61.5026  ...  heap
    + h2_bundle.go:5083:29  net/http.methodvalue...onSettingsTimer.4961.61.5000  ...  heap

Only the trailing number moves. That number is the generated-symbol counter, a
running count of emitted items, so enqueueing more implementations -- and
enqueueing them earlier -- renumbers it. Nothing moved between the frame and the
heap, which `TestFrameEscapeAudit` says independently.

Twelve lines became eleven because two of the three programs that build an
`onShutdownTimer` method value now land on the *same* counter, and the census is
a set of keys. I checked that rather than assuming it: compiling the three
programs one at a time and listing their `onShutdownTimer` census records gives
three records each, all `heap`, in all three -- and
`stdlib_http_redirect_keepalive.go` and `stdlib_http_client_server.go` now both
name theirs `...5496.39.4753`. No allocation was lost; two names collided.

*Twelve added lines are the new corpus program.* All at sites in
`testdata/runtime_package_initializer_dispatch.go` -- one per `panic`-guard
string, one per value the initializers build. The one worth naming is

    testdata/runtime_package_initializer_dispatch.go:43:24
        .goc.global.initfunc.68.main.variadicArgument  runtime.newobject  1_io_Writer  frame

-- the variadic backing array for `firstWriter(new(bytes.Buffer))` stays on the
frame inside a package initializer, which is the earlier variadic work holding
up in this shape too.

**Two baselines the brief did not name.** Adding a corpus program can also move
`escape_shadow_baseline.txt` (`TestEscapeShadowPlacement`) and
`escape_gc_differential.txt` (`TestEscapeDifferentialAgainstGC`), so both were
run as well.

---

# gc-stackscan-gogc10 — precise stack scan defect (RUNTIME_PLAN §26 residue)

## Reproduction (baseline, before any change)

Tree: `ccwork/gc-stackscan-gogc10` off `main` `6b9fbb0`. Box load ~0.9 at start.
Reducer `goc/testdata/runtime_gc_type_mask_padding.go`, built with a plain
`goc -o repro.bin`, run sequentially, `GOMAXPROCS=3`.

| setting | rate |
| --- | ---: |
| `GOGC=10` | **10/40 fail** |

Every failure has the identical stack:

```
runtime_throw <- runtime_badPointer <- runtime_findObject <- runtime_scanblock
  <- runtime_scanframeworker <- runtime_scanstack <- markroot <- gcDrainN
  <- gcAssistAlloc1 <- systemstack
```

and the identical shape: the containing "object" is a **goroutine stack**
(`s.state=mSpanManual`, `s.elemsize=8192`), the reported word is a heap address
whose span is `state=0` (returned to the page allocator), and the neighbouring
words are small integers (`0x3`, `0x4`, `0x1f5`, `0x25f`, `0x120`, `0x6`).
This is the precise stack scan, not a bulk barrier — §26's open residue, not the
type-mask padding bug the reducer is named for (that one is closed).

## Localisation: one frame, one PC, one slot — every time

A scratch diagnostic (per-`m` record of the frame `scanframeworker` is walking,
printed from `badPointer`) names it identically in 8/8 failures:

```
cg12badframe: fn=main_buildGraph entry=0x558244 pc=0x5586a4 pcoff=0x460
              sp=... fp=... varp=fp argp=fp locals=76 args=1
cg12badframe: localsbase=... slot=65
```

The PC is a single call site. Disassembling `main_buildGraph`:

```
  558694:  add  x16, x29, #0x218     ; x16 = &tmp, the alloca for string(rune(..))
  558698:  str  x16, [x29, #184]     ; spill the address across the call
  55869c:  mov  x0, #0
  5586a0:  bl   runtime_intstring    ; <-- SAFEPOINT, return pc = 0x5586a4
  5586a4:  ldr  x17, [x29, #184]
  5586a8:  str  x0, [x17]            ; NOW the alloca gets its data pointer
  5586b0:  add  x9, x16, #8
  5586b4:  str  x1, [x9]             ; ... and its length
```

`x29+0x218` is local slot 65 (localsbase = `x29+0x10`). The prologue does zero it
(`str xzr, [x29, #536]` at `0x55831c`). The crash dump confirms the identity of
the slot: slot 65 = pointer, slot 66 = `0x1` — a one-rune string header — and
slots 67/68 are the `"node-"+r` header with length `0x6`.

So the slot is only **written after the call returns**, yet it is claimed as a GC
root *at* the call. `buildGraph`'s body is a loop, so on every iteration after
the first the slot still holds **the previous iteration's `string(rune(...))`
header**, long dead. When a collection catches the goroutine exactly at
`0x5586a4` and that dead string's span has already been returned to the page
allocator, `findObject` throws.

## Root cause

`arm64/goabi.go:lowerGoAggregateResult` (and its `lowerGoValueResult` twin)
lowers a call that returns an aggregate as

```go
slot := f.AllocAggregate(aggregate, out)   // alloc emitted BEFORE the call
for _, part := range parts {
        pin := newPinned(f, part.reg, ...)      // result register
        address := offsetAddr(f, slot, part.offset, &post)
        post = append(post, Store{pin -> address})   // written AFTER the call
}
post = append(post, Copy{To: destination, Args: []ir.Ref{slot}})
```

`AllocAggregate` calls `MarkAggregatePointerWords`, so `slot`'s pointer words
land in `f.StackPointerWords`. `arm64/regalloc.go:computeSafepointRoots` then
reports the allocation at the call, because `pointerAllocationSources` maps the
live `slot` temporary to its allocation and `slot` **is** live across the call —
its only uses are the post-call result stores.

The existing code already removes `instruction.To` and `instruction.Defs` at a
call, on the grounds that "a result does not exist until after its defining
instruction". The aggregate result *home* is a definition of the call in exactly
the same sense, and it is not removed.

Straight-line code survives this because the prologue zeroes every
pointer-bearing allocation word, so an unwritten home reads as nil. **A loop does
not**: the `alloc` is inside the loop body, the slot is never re-zeroed, and on
iteration *n* the home still holds iteration *n-1*'s value at the moment the call
is entered. If a collection catches the goroutine there and that value's span has
already been released, `findObject` throws `found bad pointer in Go heap`.

The general statement, of which the result home is one instance:

> A frame allocation is reported as a GC root at safepoints between its `alloc`
> and its first store. Inside a loop those safepoints see the previous
> iteration's pointer, which the collector is under no obligation to keep alive.

This is an **over**-reporting defect: an extra, stale root. `stack-scan/loop-safepoints`
is an **under**-reporting defect ("a stack slot live across a loop back edge was
not a GC root", `-O` only, `opt.Mem2Reg` promotes the pointer out of the frame and
no promoted value is reported). Opposite polarity, different mechanism — see
below for the measurement.

## The fix

`arm64/regalloc.go`, new `undefinedAllocationsAtSafepoints`, plus one guard in
`arm64/mc.go:recordSafepoint`.

A forward may-dataflow over "the program has written this allocation since its
`OAlloc`":

- entry: every allocation **defined** (the prologue zeroes their pointer words);
- an `OAlloc` **undefines** its own allocation — this is what cuts the loop back
  edge, because the `OAlloc` names a fresh local each iteration;
- anything that touches an address into an allocation, other than deriving a
  further address from it, defines it — deliberately coarser than "writes it";
- merge by **union**, so an allocation written on one path into a join is still
  reported at the join. Only an allocation *no* path has written since its
  `OAlloc` is suppressed, and such a slot holds nothing the program may read.
- allocations whose address escapes the frame (`frameEscapingAllocations`) are
  excluded from the analysis and stay reported at every safepoint: a callee handed
  `&local` can fill it, so "no write seen here" says nothing about their contents.

`recordSafepoint` then skips the **pointer words** of an allocation that is
undefined at that safepoint. The allocation's own address (its register or spill
slot) is still reported, so stack copying keeps relocating the interior pointer
that a growing stack inside the call depends on.

## Rates after the fix (same box, `GOMAXPROCS=3`, sequential)

| build | `GOGC=10` | default `GOGC` |
| --- | ---: | ---: |
| `main` `6b9fbb0` | **10/40 fail** | 0/20 (measured previously) |
| this tree | **0/200 fail** | **0/60 fail** |

At the observed pre-fix rate of 10/40 = 0.25, a clean run of 200 has a
probability of `0.75^200 ≈ 1e-25` of happening by chance.

## Confirmed final rates (clean tree, no scratch diagnostics)

`goc/testdata/runtime_gc_type_mask_padding.go`, `GOMAXPROCS=3`, sequential:

| tree | `GOGC=10` | default `GOGC` |
| --- | ---: | ---: |
| `main` `6b9fbb0` | 10/40 fail | 0/20 (given) |
| this tree | **0/200 fail** | **0/100 fail** |

`goc/testdata/runtime_gc_stale_result_alloca.go` (the new reducer), 0/30 at
`GOGC=10`, and 0/25 at each of `GOMAXPROCS` 1, 2, 3 and 8 at the default `GOGC`.
On the compiler before the fix it is **100/100** across those same four
`GOMAXPROCS` values and 20/20 at `GOGC=10` — deterministic, not statistical.

Registered as `gc-invariants/stale-result-home`, `runtimeCapabilityMustPass`.

## Deterministic compiler-level tests (arm64)

- `TestGoStackMapsOmitAggregateResultHomeAtItsOwnCall` — the emitted map at the
  call that produces an aggregate result does not contain the home's pointer
  word, and the map at the next safepoint does. Verified to fail without the
  `recordSafepoint` guard: the map there is `{0, 2}` instead of `{0}`. Word 0 is
  the home's own *address* spill slot, which the fix deliberately keeps, so a
  stack that grows inside the call still gets the interior pointer relocated.
- `TestUndefinedAllocationsCoverTheWindowBeforeTheFirstStore` — the analysis's
  rule stated over unlowered IR.
- `TestGoStackMapsKeepAllocationWrittenOnOnlyOnePath` — the union merge: a slot
  written on one path into a join is still described at the join. This is the
  guard against a future "tighten it to intersection" change, which would drop a
  word the copying stack needs.

## `stack-scan/loop-safepoints` is a different defect, and is not fixed

Reproduced in the matrix's own `-O` configuration — a prebuilt runtime pack built
with `goc build-runtime -O`, then `goc -O -runtime <pack>`, run with
`GODEBUG=cg12scanroots=1`:

| build | `stack-scan/loop-safepoints` |
| --- | --- |
| `main` `6b9fbb0`, `-O` + pack | **3/3 fail** |
| this tree, `-O` + pack | **3/3 fail** — unchanged |
| this tree, no `-O`, + pack | 3/3 pass |

The panic is `a stack slot live across a loop back edge was not a GC root`,
preceded by `collected while live: carried-0`. That is the opposite polarity from
the defect fixed here: `loop-safepoints` is a **missing** root (a live pointer is
collected), this was an **extra, stale** root (a dead pointer is followed).
Section 6.1's narrowing still stands — under `-O` `opt.Mem2Reg` promotes the
pointer out of the frame and no promoted value is reported at the safepoint at
all, so there is no allocation for this analysis to say anything about. The two
are not the same bug and one change does not fix both.

`goc/testdata/runtime_opt_loop_carried_root.go`, §6.1's reducer for the same
defect, likewise fails 3/3 with `-O` + pack on both trees. (Its symptom shape
moved — `main` reports the truncated chain, this tree faults on
`0xdeadbeefdeadbeef` under `clobberfree` — but both are the same premature
collection and both fail on every run.)

One measurement worth recording for whoever picks §6.1 up: **`loop-safepoints`
fails only with a prebuilt `-O` pack.** A monolithic `goc -O` build of the same
program passes 5/5 on `main` and 3/3 here. Whatever §6.1 is, it needs the split
build, not just `-O`.

## Guards

- **`TestFrameEscapeAudit`**: PASS (182 s). It globs `testdata/*.go`, so it covers
  the new program too.
- **`goc/testdata/alloc_census_baseline.txt`**: moved, and regenerated. The delta
  is **three added lines, all in the new corpus program**
  (`41:27` twice — the 8 MB global backing array — and `63:9`, the `panic`
  string). No existing site changed in either direction, so the safepoint change
  moves **no** allocation decision. `escape_gc_differential.txt` is opt-in
  (`-escape-gc-differential`) and joins against the census by source line; it is
  not regenerated here, and its only staleness is the three new lines.
- **Loop aliasing against the host toolchain**:
  `TestLoopBodyAllocationsAreDistinctPerIteration` and
  `TestLoopAliasExpectationsMatchTheHostToolchain` both PASS, all subtests, in
  both the plain and `-O` arms.
- **`arm64` stack-map tests**: the three new ones pass, and the map test is
  verified to fail with the guard removed.
- **Targeted runtime programs**, compiled with the fixed compiler against a
  prebuilt runtime pack and run at `GOMAXPROCS=3`: the twelve
  `runtime_cleanup_*` / `runtime_finalizer_*` programs, and the eighteen
  `runtime_gc_*` / `runtime_stack_*` / `runtime_stack_scan_*` programs including
  `mark-workers`, `checkmark`, `concurrent-mark`, `assist-stack-growth`,
  `stack-copy-roots`, `stack-growth`, `blocked-goroutines`, `panic-unwind` and
  `syscall`. **30/30 pass, 0 fail.**
- The defect is **arm64-only**: `amd64/regalloc.go`'s `computeSafepointRoots`
  reports only `GCRef` temporaries and `amd64` has no `stackAllocTmp` or
  `StackPointerWords` reporting at all, so there is nothing there to suppress.

## Determinism

`scripts/determinism-check.sh -corpus -j 16`, over the 399-program corpus
(the 398 that were there plus the new reducer):

| arm | rounds | reproducible | varying | failed | content varies | layout only |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| no `-O` | 3 | 399 | 0 | 0 | 0 | 0 |
| `-O` | 2 | 399 | 0 | 0 | 0 | 0 |

## Head-to-head rate, same box, back to back

`runtime_gc_type_mask_padding.go`, `GOMAXPROCS=3`, sequential, 60 runs each,
the four cells run one after another so they see the same machine:

| tree | `GOGC=10` | default `GOGC` |
| --- | ---: | ---: |
| `main` `6b9fbb0` | **10/60 fail** | 0/60 |
| this tree | **0/60 fail** | 0/60 |

Together with the earlier 200-run batch, this tree is **0/260 at `GOGC=10`** and
**0/160 at the default**. Against the measured pre-fix rate of 20/100 = 0.20,
a clean 260 has probability `0.8^260 ~ 1e-25`.

## Cost

`goc -o /dev/null goc/testdata/runtime_gc_mark_workers.go`, three alternating
runs of each compiler: 3.99/4.06/4.11 s before, 4.11/4.13/4.35 s after — about
3-4% on a whole-runtime compile. The analysis is a forward may-dataflow that
converges in two or three rounds over blocks, and it runs only on managed frames
that have pointer-bearing, non-escaping allocations.

## Capability bookkeeping, and one pre-existing failure fixed with it

Registering a new capability requires an entry in the accepted coverage baseline
or in `cmd/goc/testdata/runtime_coverage_baseline_pending.json`;
`TestCheckedRuntimeCoverageBaselineDenominator` reconciles the two against the
matrix. `gc-invariants/stale-result-home` is now listed there.

While doing that, the same test turned out to be **already failing on `main`
(`6b9fbb0`)**, on an unrelated capability: `core-types/package-initializer-dispatch`,
added to the matrix by `0f80c37` ("goc: the reduction, as a corpus program that
fails", 2026-08-03) without the matching pending entry. Verified by running the
test in a clean worktree at `main`. It is the same one-line bookkeeping omission,
in the file this change already touches, so it is fixed here rather than left to
look like a consequence of this work — flagged explicitly because it is not mine.
`TestCheckedRuntimeCoverageBaselineDenominator`,
`TestRuntimeCapabilityMatrixIsWellFormed`,
`TestRuntimeCapabilityExclusiveClassification`,
`TestCheckedRuntimeCoverageBaseline` and
`TestRuntimeCorpusCoverageReportsCategoryResources` all pass now.

## The one way this fix could be wrong, checked directly

Suppressing a word is only safe if nothing needs it. The thing that would need it
is stack copying: `runtime.adjustframe` relocates exactly the frame words a
safepoint's map marks, so dropping a word that holds an interior stack address
would leave a stale old-stack pointer behind. Two things say it does not happen:

- the fix keeps the allocation's **own address** in the map (the unit test pins
  this: `{0}` remains, only `{2}` goes), so the spilled `&home` a growing stack
  depends on is still relocated;
- sixteen stack-copy-sensitive programs run under
  **`GODEBUG=cg12checkstackcopy=1`**, which throws at a stale old-stack pointer
  instead of leaving it to be found later: the five `slog_attr_frame_gcmask*`
  programs (§28), `stack-growth`, `stack-copy-roots`, `assist-stack-growth`,
  `finalizer-stack-growth`, `goroutine-closure-gc`, `goroutine-entry-stack-map`,
  `many-defers-stack`, `many-goroutines-gc`, `defer-closure-stack-gc`, and both
  reducers. **16/16 pass.**

---

# Summary — dead frame slots, and a census that names the direction

- **Dead frame slots removed: 2 477 distinct sites, 32 285 occurrences across
  the 398-program corpus**, together with 85 453 dead GC stack pointer words.
  403 dead sites remain, all of one different kind (unused call-result slots),
  named in 1.4. Fixed by not emitting the slot, not by eliminating it; both were
  priced and not-emitting is the smaller change (1.2).
- **Direction labelling is now correct** for every allocation that is a
  placement decision: measured over the whole corpus, a change that moves
  front-end frame slots to the heap was reported as 451 sites "appearing" and 0
  "frame -> heap", and is now reported as 317 `frame -> heap`. The 84 that
  remain are ordinary local variable slots, a category the census excludes by
  design and whose price for inclusion is quantified in 2.4. Detection did not
  regress: nothing was removed from the census, every change still fails the
  baseline, and every line still names position, function, allocator and type.
- **Census delta: 14 846 -> 17 822 lines** (+2 976). +2 971 are the front-end
  frame placements now recorded, of which 210 land on sites that already carried
  a heap line; +5 are one inlining decision Part 1 changed, reviewed in 1.5. No
  line was removed and no placement changed direction.
