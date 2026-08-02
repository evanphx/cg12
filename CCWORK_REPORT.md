# Allocation-placement census: a committed instrument

Branch `ccwork/escape-alloc-census`, off `ccwork/escape-frame-publication` (`ddd03eb`).

Goal: turn the throwaway census that `ccwork/escape-frame-publication` ran by hand
-- "where does every allocation land across the 385-program corpus" -- into a
committed tool plus an accepted baseline plus a test that fails when placement
moves in either direction.

This file is written as each result lands.

---

## 1. What the finished IR does and does not carry (measured, not assumed)

Before designing anything I compiled `goc/testdata/hello.go` with
`goc.CompileExecutable` and counted what an analysis of the *finished* module can
actually see. The numbers below are from that one program.

| thing in the finished IR | count | carries a type? | carries a source position? |
|---|---|---|---|
| `OAlloc4`/`OAlloc8`/`OAlloc16` (frame slots) | 18,171 | **no** | mostly (12,411 distinct positions; 383 with none) |
| `OCall runtime.newobject` | 338 | yes (arg 1 is the type descriptor symbol) | 148 of 338; 190 have none |
| `OCall runtime.makeslice` | 13 | yes (element type) | yes |
| `OCall runtime.makemap` | 2 | yes | no |
| `OHeapAlloc` left over after lowering | 0 | -- | -- |

Two facts drove the design:

1. **The heap side is complete and typed from the finished IR alone.** Every heap
   allocation is a call to a named runtime allocator with the type descriptor as an
   argument. That covers both allocations the front end decided were escaping
   (`allocateEscapingTyped`) and heap-allocation candidates that
   `opt.LowerHeapAllocations` lowered to calls -- they are the same instruction
   afterwards, which is exactly right: the census should not care which pass made
   the decision, only where the object landed.

2. **The frame side is neither complete nor typed from the finished IR alone.**
   `opt.LowerHeapAllocations` rewrites a promoted `OHeapAlloc` into a bare
   `OAlloc{4,8,16}` whose only argument is the byte size; the type descriptor is
   dropped. Worse, a promoted candidate is then indistinguishable from the 18,000
   ordinary local variable slots in the same module. A census that recorded every
   `OAlloc` would be 12,411 lines *per program* and would carry no types.

So the frame side has to be recorded where the decision is made. That is one
place: the rewrite loop at the bottom of `opt/escape.go`.

## 2. Scope of the census, stated precisely

The instrument records, deduplicated across the whole corpus:

* every **heap** allocation: a call to `runtime.newobject`, `runtime.makeslice`,
  `runtime.makeslice64`, `runtime.makemap`, `runtime.makemap64`,
  `runtime.makemap_small`, `runtime.makechan`, `runtime.makechan64` or
  `runtime.newarray`, found by scanning the finished IR; and
* every **frame** allocation that `opt.LowerHeapAllocations` promoted out of a
  heap-allocation candidate, recorded by that pass as it rewrites.

It deliberately does **not** record ordinary front-end frame slots
(`gen.localAllocTyped`, `gen.localAlloc`). Those are the 18,000-per-program noise
floor, they have no type in the IR, and they are not the product of an escape
decision that the compiler re-derives.

That exclusion does **not** create a blind spot for the regression class this
instrument exists to catch. The heap side is complete, so a front-end change that
moves an object from `runtime.newobject` to a plain frame slot -- which is exactly
the "six functions LOSING a heap allocation" failure the ad-hoc census caught --
removes a line from the baseline and fails the test. What the census cannot do for
such a site is *label* the move as `heap -> frame` rather than "site removed"; it
still names the site, the function and the type. Sites the escape pass decides get
the stronger, labelled treatment in both directions.

## 3. What was built

| file | what it is |
|---|---|
| `opt/alloccensus.go` | `opt.AllocationCensus(module) []Allocation` -- the instrument. One record per site: position, function, allocator, type, placement. Sorted, deduplicated, no map iteration order, no addresses, no timestamps. |
| `ir/alloc.go`, `ir/func.go` | `ir.AllocDecision` and `Module.AllocDecisions`, where `LowerHeapAllocations` writes each placement decision as it makes it. Diagnostic only: nothing in the compiler reads it. |
| `opt/escape.go` | `LowerHeapAllocations` records its decisions. **No behaviour change** -- the rewrite it performs is byte-for-byte what it was; the only new code appends to a slice. |
| `opt/alloccensus_test.go` | 10 unit tests of the census itself: promotion, escape, front-end call, the `make` allocators, what it ignores, sorting, deduplication, positionless sites, the site/placement split, symbol normalization. Runs in 8 ms. |
| `goc/corpusaudit_test.go` | The corpus harness, extracted from `framecheck_test.go` so both audits share one pass. |
| `goc/alloccensus_test.go` | `TestAllocationCensus` and two fast unit tests of the failure reporting. |
| `goc/testdata/alloc_census_baseline.txt` | The accepted census. |

A record looks like this (tab separated, positions relative to the repo root):

    stdlib/src/archive/zip/reader.go:340:2	archive/zip.File.findBodyOffset	runtime.newobject	30_byte	frame
    stdlib/src/internal/abi/bounds.go:110:9	internal/abi.BoundsDecode	runtime.newobject	string	heap

The first four fields are the site's identity and the fifth is the decision, so
an object moving changes **one field of one line** rather than deleting a line
and adding another. That is what makes `heap -> frame` and `frame -> heap`
distinguishable in the failure output instead of both reading as churn.

### The failure output

`TestAllocationCensus` sorts the difference into the four things a reviewer has
to answer separately, and names every site in each:

* **moved heap -> frame** -- correctness-critical; the object now dies with the
  frame.
* **moved frame -> heap** -- possible performance regression; an allocation
  appeared where there was none.
* **site vanished** -- fine if the code is gone, and the 9f76498 failure if the
  object is now an ordinary front-end frame slot.
* **site appeared** -- expected when corpus programs are added, suspicious
  otherwise.

Each line reads e.g.

    stdlib/src/runtime/mgc.go:1575:3	runtime.gcMarkTermination	runtime.newobject	24_byte
          frame -> heap, seen compiling fmt_sprintf.go

`compareAllocationCensus` is a plain function over two maps, so that reporting is
covered by two unit tests that run in milliseconds and never touch the corpus.

### Updating the baseline

    go test ./goc -run TestAllocationCensus -update-alloc-census-baseline

The same shape as `-update-frame-escape-baseline`. It is a flag, not a
self-healing baseline, and `TestAllocationCensus`'s doc comment is five numbered
questions a reviewer should answer before committing the diff -- what proves the
object cannot outlive the frame, whether the site is on a hot path, whether a
vanished site means the allocation went away or the code did, whether an appeared
site is new code, and whether the size of the diff matches the size of the change.

## 4. The census

Baseline `goc/testdata/alloc_census_baseline.txt`, from all **385** corpus
programs:

| | count |
|---|---|
| **allocation-site records** | **18,683** |
| distinct sites (ignoring placement) | 18,244 |
| records placed on the heap | 18,183 |
| records placed in a frame | 500 |
| sites decided *both* ways somewhere in the corpus | 439 |
| sites with no source position (written `?`) | 5,870 |

By allocator: `runtime.newobject` 17,208, `runtime.makemap` 813,
`runtime.makeslice` 437, `runtime.makechan` 223, `runtime.newarray` 2.

File size 2.0 MB. That is large for a file meant to be read, and it is the honest
size: it is one line per allocation site in the whole standard library as this
tree compiles it, deduplicated across 385 programs. It is meant to be read as a
*diff*, not as a document, and a placement change touches a handful of lines out
of the 18,683.

**The 439 both-ways sites are not a defect.** A "site" is a source position plus
the function containing it *after inlining*, and inlining puts several copies of
one source site into one function, each decided separately. `hello.go` alone has
17 such sites out of 190; `fmt_sprintf.go` has 46 out of 1,277. The census reports
such a site as `frame+heap` rather than picking one, so it neither hides the split
nor depends on which line was read first.

## 5. What it costs, and the tier it belongs in

**The census is free.** It rides on a corpus pass that already existed.

`TestFrameEscapeAudit` already compiled all 385 corpus programs; that pass
dominates everything either audit does. Rather than add a second one, this branch
extracts the walk into `goc/corpusaudit_test.go` and has both audits share it --
one compile, both analyses, whichever test asks first pays.

| measurement | wall | CPU |
|---|---|---|
| `TestFrameEscapeAudit` alone, before this branch | 150.4 s | 26.0 min |
| both audits sharing one pass, run 1 | 149.6 s | 26.0 min |
| both audits sharing one pass, run 2 | 180.3 s | 26.6 min |

The marginal cost of `opt.AllocationCensus` over 385 modules is inside the
run-to-run noise on this box, which was running another job's corpus compile
concurrently during these measurements. (The 209.7 s first run also wrote the
2 MB baseline.)

**Tier: the same one `TestFrameEscapeAudit` is in, whatever that is.** It is the
same three minutes of wall clock and the same eight-worker, several-hundred-
megabyte-peak compile, and it now buys two audits instead of one. Nothing is
sampled and no program is skipped. If that tier is too slow for a
pre-merge gate then `TestFrameEscapeAudit` already had that problem and the two
should move together; splitting them would double the cost, which is the one
outcome worth avoiding.

The census logic itself is separately covered by 10 unit tests in
`opt/alloccensus_test.go` (8 ms) and the failure reporting by 2 in
`goc/alloccensus_test.go` (10 ms), so a fast tier can check that the instrument
works even where it cannot afford to run it over the corpus.

## 6. Verification

(Appended as each run exits.)
