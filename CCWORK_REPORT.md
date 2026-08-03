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
