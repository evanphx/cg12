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

## 6. The instrument was checked by making it fire

A test that has never failed has not been shown to work. So I perturbed
`LowerHeapAllocations` -- forcing every candidate in `archive/zip.*` to the heap
and every candidate in `archive/tar.*` into a frame -- ran the census over the
real corpus, and read what it said. The perturbation was then reverted
(`git checkout opt/escape.go`, verified against the index) and is not on this
branch.

`TestAllocationCensus` failed as intended, naming sites and directions:

    an allocation moved from the heap into a frame. This is the correctness-critical
    direction: ...
      ?	archive/tar.Reader.Next	runtime.newobject	error
            heap -> frame, seen compiling stdlib_archive_tar_roundtrip.go
      ...

    an allocation moved from a frame onto the heap. This is a possible performance
    regression: ...
      stdlib/src/archive/zip/reader.go:340:2	archive/zip.File.findBodyOffset	runtime.newobject	30_byte
            frame+heap -> heap, seen compiling stdlib_archive_zip_roundtrip.go
      ...

### It also found a real bug in the reporting, which is now fixed

`stdlib/src/archive/tar/common.go:426:2` came out as `frame+heap -> frame` and was
filed under **moved to the heap**. It is the opposite: that site lost a heap
allocation.

The cause is the split sites of section 4. The reporter compared the two
placement *strings*, so anything other than exactly `heap`->`frame` or
`frame`->`heap` fell through to a default arm that assumed the heap direction. A
site that dropped its heap copy therefore landed in the bucket whose advice is
about performance instead of the bucket that asks what proves the object cannot
outlive the frame -- which is the whole point of separating them. 439 corpus
sites are split, so this was not a corner case.

Fixed in `231d671`: the direction is now which placement the site *gained or
lost*, not which of two values it changed to. Covered by a five-case subtest
(`TestCompareAllocationCensusReportsASplitSite`) over gain-frame, gain-heap,
lose-frame, lose-heap and unchanged.

This is worth stating plainly: the only reason that bug is not in the committed
instrument is that the instrument was deliberately made to fail once.

## 7. Verification

Every run below was waited on to exit; nothing was left running.

| what | result |
|---|---|
| `go test ./goc -run TestAllocationCensus` (final tree, run 1) | **PASS** 148.24 s |
| `go test ./goc -run TestAllocationCensus` (final tree, run 2) | **PASS** 148.37 s |
| `go test ./opt/ ./ir/` | **PASS** (opt 0.94 s, ir 0.01 s) |
| `go test ./goc/...` (before the split-site fix) | **PASS** `ok ... 770.363s`, exit 0 |
| `go test ./goc/...` (final tree) | (below) |

**Stability.** Two separate `go test` processes -- each a fresh compile of all 385
programs, each re-deriving the census from scratch -- both reproduced the
committed baseline exactly. The test asserts set equality in both directions
(nothing found that is not accepted, nothing accepted that is not found), and
`writeBaseline` emits the accepted set sorted, so reproducing the set is
reproducing the file byte for byte. The census is **stable across repeated runs**.

The one thing in the harness that is *not* reproducible is the "seen compiling
X" hint in a failure message: which program is named, when several produce the
same finding, depends on which of the eight workers finished first. It is a hint
in a diagnostic and appears in no baseline. The comment in
`goc/corpusaudit_test.go` says so.

### Test counts (actual, from `-v` output)

* `opt`: 10 tests for the census -- `TestAllocationCensus{ReportsAPromotedCandidateAsFrame,
  ReportsAnEscapingCandidateAsHeap, ReportsAFrontEndAllocatorCall, ReportsMakeAllocators,
  IgnoresGrowslice, IgnoresOrdinaryFrameSlots, IsSortedAndDeduplicated,
  KeepsPositionlessSites}`, `TestAllocationSiteExcludesPlacement`,
  `TestAllocationTypeNameStripsPrefixAndDigest`. No subtests. 8 ms.
* `goc`: `TestAllocationCensus` (1 test, no subtests, ~148 s);
  `TestCompareAllocationCensusNamesTheDirection` (1 test, no subtests);
  `TestCompareAllocationCensusReportsASplitSite` (1 test, **5 subtests**: frame
  gains a heap copy, heap gains a frame copy, split loses its heap copy, split
  loses its frame copy, split is unchanged). The three fast ones total 18 ms.
