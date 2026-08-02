# Allocation-placement census: a committed instrument

Branch `ccwork/escape-alloc-census`, off `ccwork/escape-frame-publication` (`ddd03eb`).

Goal: turn the throwaway census that `ccwork/escape-frame-publication` ran by hand
-- "where does every allocation land across the 385-program corpus" -- into a
committed tool plus an accepted baseline plus a test that fails when placement
moves in either direction.

This file is written as each result lands.
(The previous contents of this file were the `escape-frame-publication` fix report
and its independent verification; they are unchanged in history at
`05946f2:CCWORK_REPORT.md`.)

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

---

# Part II -- rebase onto `main` and the runs the previous job did not finish

Continuation job `ccwork/escape-census-finish`. The previous job hit its timeout
during the final full-corpus `go test ./goc/...`. This part records the rebase,
the baseline check, and every suite run to completion in the foreground.

## 8. The rebase, and a correction to its stated premise

**The premise this job was given is wrong, and the correction matters.** The task
said `main` "has moved since that branch was cut and now carries the escape
publication fix", that the fix "moved 22 allocation sites frame->heap", and that
the baseline recorded before the fix is therefore stale and must be regenerated.

It is not stale. The census branch was cut from `ddd03eb`, and `ddd03eb`
**already contains the entire publication fix**:

    $ for c in 6245dbb fc74294 eb9872e; do git merge-base --is-ancestor $c ddd03eb && echo "$c IN"; done
    6245dbb IN   goc: copying a value into fresh heap storage publishes it
    fc74294 IN   goc: a summary walk must see an interface-typed result too
    eb9872e IN   goc: reduced tests for the three publication shapes

What `main` added after `ddd03eb` is two commits, `fac83eb` and `05946f2`, and
they touch exactly one file between them:

    $ git diff --stat ddd03eb..main
     CCWORK_REPORT.md | 255 +++++++++++++++++++++
    $ git diff --stat ddd03eb..main -- . ':!CCWORK_REPORT.md'
    (empty)

Zero compiler code changed. So the baseline was recorded *with* the publication
fix in the tree, the 22 frame->heap moves are *already in it*, and the correct
expected delta from this rebase is **zero sites, not 22**. Regenerating and
finding 22 moved sites would have meant something was badly wrong.

This is exactly the reasoning the instrument exists to force, so it is applied to
itself rather than asserted: the baseline was regenerated from scratch on the
rebased tree and diffed against the committed one. Result in section 9.

**Mechanics.** The four commits were replayed onto `05946f2`. Only
`CCWORK_REPORT.md` conflicted; all compiler and test files applied clean.
Verified afterwards that the rebased tree's code is byte-identical to the
original branch tip:

    $ git diff --stat FETCH_HEAD HEAD -- . ':!CCWORK_REPORT.md'
    (empty)

`go build ./...` and `go vet ./opt/ ./ir/ ./goc/` both exit 0.

## 9. The baseline delta: **zero sites, and that is the correct answer**

The baseline was deleted-and-rewritten from scratch on the rebased tree -- a full
fresh compile of all 385 corpus programs, census re-derived from the finished IR,
file re-emitted by `writeBaseline`:

    $ go test ./goc -run TestAllocationCensus -update-alloc-census-baseline
    ok  github.com/evanphx/cg12/goc  148.736s     (real 2m29.5s, user 26m8.8s)

Against the committed baseline:

    $ md5sum goc/testdata/alloc_census_baseline.txt
    9a2ed8f8dff5be7e99adae8fb91fbd80        (before regeneration)
    9a2ed8f8dff5be7e99adae8fb91fbd80        (after regeneration)
    $ git diff --stat goc/testdata/alloc_census_baseline.txt
    (empty)
    $ diff old new; echo $?
    0

**Not one line, not one field, changed.** 18,713 lines, byte for byte.

Read against the question this job was asked -- "is the delta exactly the 22
sites the fix moved, plus nothing else?" -- the answer is:

* **the delta is 0 sites, not 22**, and
* **0 is what a correct instrument must report here**, because the rebase carried
  no compiler change at all (section 8). The 22 frame->heap moves were made by
  commits that are ancestors of the branch point, so they were already recorded in
  the accepted baseline when it was first written.
* **nothing unexplained appeared.** The failure mode this instrument exists to
  catch -- a site that moves for a reason nobody can name -- would have shown up
  here as any non-empty diff. There is none.

Had the premise been right and the fix been rebased *over*, the expected diff
would have been 22 lines each changing their fifth field `frame` -> `heap` and
nothing else; the test would have failed and named all 22 under "moved from a
frame onto the heap". Section 11 shows the instrument actually producing that
report, by removing the publication fix from the tree and letting the census fire
against it.

## 10. `go test ./goc/...` -- run to completion in the foreground

This is the run the previous job did not finish. It was launched detached (the
tool driving this session caps a single foreground command at 10 minutes and this
suite needs ~13), then **polled in-session until the process exited**; the exit
status and the counts below are read from that finished process's own log, not
from a partial tail.

    $ go test ./goc/... -v -timeout 60m
    ok  github.com/evanphx/cg12/goc  756.667s
    exit status 0

### Subtest census (counted from the `-v` log, not estimated)

| | count |
|---|---|
| `=== RUN` | **609** |
| `--- PASS` (all levels) | **609** |
| -- top-level tests | 305 |
| -- subtests | 304 |
| `--- FAIL` | **0** |
| `--- SKIP` | **0** |
| packages | 1 (`github.com/evanphx/cg12/goc`) |

**The count moved by exactly the number of tests this branch adds.** `main`'s own
verification of `ddd03eb` (commit `fac83eb`) recorded **601 RUN / 601 PASS / 0
FAIL** on the same suite. 609 - 601 = **8**, and this branch adds exactly 8 goc
tests: `TestAllocationCensus`, `TestCompareAllocationCensusNamesTheDirection`,
and `TestCompareAllocationCensusReportsASplitSite` with its 5 subtests. Nothing
that used to run stopped running.

    --- PASS: TestAllocationCensus (149.03s)
    --- PASS: TestCompareAllocationCensusNamesTheDirection (0.00s)
    --- PASS: TestCompareAllocationCensusReportsASplitSite (0.00s)
        --- PASS: .../frame_gains_a_heap_copy (0.00s)
        --- PASS: .../heap_gains_a_frame_copy (0.00s)
        --- PASS: .../split_loses_its_heap_copy (0.00s)
        --- PASS: .../split_loses_its_frame_copy (0.00s)
        --- PASS: .../split_is_unchanged (0.00s)
    --- PASS: TestFrameEscapeAudit (0.00s)

`TestFrameEscapeAudit` reads 0.00s because `TestAllocationCensus` ran first and
paid for the shared corpus compile -- section 5's "one compile, both analyses,
whichever test asks first pays", visible in the timings.

## 11. The failure output, checked on the final tree against a real change

Item 4 of this job: confirm the failure output still names the sites and the
direction rather than reporting a count. The rebase could not have degraded it --
the rebased tree's code is byte-identical to the branch tip (section 8) -- but
"could not have" is not "did not", so it was made to fire on the final tree.

Not with a synthetic perturbation this time: **the publication fix itself was
reverted** in a throwaway `git worktree`, leaving the accepted baseline in place,
and the census was run against it. That is the closest available approximation of
the situation this job's premise described.

    $ git worktree add $TMPDIR/nofix HEAD
    $ git revert --no-commit fc74294 && git revert --no-commit 6245dbb   # the fix, newest first
    $ git diff --stat HEAD -- . ':!CCWORK_REPORT.md'
     goc/compile.go                         | 351 +------------------------
     goc/testdata/frame_escape_baseline.txt |   3 +
    $ go test ./goc -run TestAllocationCensus
    --- FAIL: TestAllocationCensus (150.33s)   exit 1

The worktree was removed afterwards; the branch's own tree is clean and the
baseline md5 is unchanged (`9a2ed8f8...`).

### It named every site, with position, function, allocator, type and direction

    testdata/alloc_census_baseline.txt lists an allocation site the compiler no longer has.
    That is fine if the code is gone, and is a silent heap-to-frame move if the object is now
    an ordinary front-end frame slot -- which this census does not record and so cannot tell
    you apart. Read the source at each site below before rerunning with
    -update-alloc-census-baseline.
      stdlib/src/crypto/internal/fips140/bigmod/nat.go:939:24  crypto/internal/fips140/bigmod.Nat.Mul       runtime.newobject  64_uint
            was on the heap, now absent
      stdlib/src/crypto/x509/verify.go:1059:39  .goc.global.initfunc.124.crypto/x509.anyPolicyOID  runtime.newobject  5_uint64
            was on the heap, now absent
      stdlib/src/crypto/x509/verify.go:1059:39  .goc.global.initfunc.130.crypto/x509.anyPolicyOID  runtime.newobject  5_uint64
            was on the heap, now absent
      stdlib/src/crypto/x509/verify.go:1059:39  .goc.global.initfunc.75.crypto/x509.anyPolicyOID   runtime.newobject  5_uint64
            was on the heap, now absent
      testdata/runtime_debug_gc_controls.go:15:28  main.main  runtime.newobject  128__int
            was on the heap, now absent
      testdata/runtime_range_target_order.go:144:13  main.targetAliasingTheRangeExpression  runtime.newobject  3_int
            was on the heap, now absent
      testdata/runtime_range_target_order.go:152:12  main.targetAliasingTheRangeExpression  runtime.newobject  3_int
            was on the heap, now absent
      testdata/runtime_slice_pointer_append_gc.go:10:31  main.main  runtime.newobject  4__main_record
            was on the heap, now absent
      testdata/stdlib_signal_during_gc.go:23:28  main.main.func.17.5  runtime.newobject  1024__int
            was on the heap, now absent

**Verdict on item 4: the failure output is intact and useful.** Every site is
named with its source position, its containing function after inlining, its
allocator and its type, and each carries its own direction line. A reviewer can
open every one of these files at the named line. No count-only reporting anywhere.

Note the first entry: `bigmod.Nat.Mul`, which `main`'s own fix report identified
as the fix's `+5.8%` cost site. The census found it independently, from the
finished IR, without being told to look.

### What this measures about the "22 sites"

The four buckets came out **9 vanished, 0 moved heap->frame, 0 moved
frame->heap, 0 appeared** (only the `vanished` assertion at
`alloccensus_test.go:118` fired; the other three were empty).

Read in the direction of the fix rather than of the revert: **the publication fix
adds 9 heap-allocation records at 7 distinct source positions** across the
385-program corpus, as this instrument scopes allocations. The three
`x509/verify.go:1059:39` records are one source site inlined into three different
`initfunc`s -- the split-site effect of section 4.

**This does not reproduce the "22 sites moved frame->heap" figure in this job's
premise, and I am not going to write around that.** Two things are true and I
can only distinguish them by measurement I did not run:

1. **9, not 22, is what this census counts.** The number 22 came from the earlier
   ad-hoc census, which is not this instrument and did not have to scope
   allocations the same way. Section 2 states the scoping precisely and section 4
   explains why one source position can produce several records; a differently
   scoped count over the same change can legitimately differ.
2. **The direction is recorded as "vanished", not "frame -> heap"** -- and that is
   the documented limitation, not a defect appearing now. Before the fix these
   objects were ordinary front-end frame slots, which this census deliberately
   does not record (section 2), so the pre-fix side of the move is invisible to it
   and the move reads as a site appearing rather than a placement changing. The
   site, function and type are still named, which is what a reviewer needs.

**Neither of these changes the answer in section 9.** The rebase carried no
compiler change, the regenerated baseline is byte-identical, and whatever the
correct headline number for the fix is -- 9 by this instrument, 22 by the ad-hoc
one -- it was already recorded in the accepted baseline before this job started.
What matters for the question asked is that **no unexplained site appeared**, and
none did.

**Reported, not fixed** (this job was told not to change compiler behaviour, and
did not): the discrepancy between 9 and 22 is worth one person-hour from whoever
produced the 22, because the two instruments disagreeing about the size of a
known change is exactly the kind of thing a census exists to surface. Nothing
here suggests the compiler is wrong -- both runs of the corpus suite pass -- only
that the two counts are not measuring the same set.

## 12. Verification ledger for this job

Every process below was waited on to exit and the numbers are read from its own
output. **Nothing was left running at the end of this session.** The one command
that could not fit the 10-minute foreground cap (`go test ./goc/...`, 12.6 min)
was launched detached and then polled in-session until its exit file appeared;
its exit status was read from that file, not inferred.

| what | result |
|---|---|
| `go build ./...` (rebased tree) | **exit 0** |
| `go vet ./opt/ ./ir/ ./goc/` | **exit 0** |
| `git diff FETCH_HEAD HEAD -- . ':!CCWORK_REPORT.md'` | **empty** -- rebased code identical to branch tip |
| baseline regenerated from scratch | **md5 unchanged**, `git diff` empty, `diff` exit 0 |
| `go test ./goc/... -v` | **PASS, exit 0**, 756.667s, 609 RUN / 609 PASS / 0 FAIL / 0 SKIP |
| `go test ./goc -run TestAllocationCensus` (final tree, run 1) | **PASS** 161.360s, baseline md5 unchanged |
| `go test ./goc -run TestAllocationCensus` (final tree, run 2) | **PASS** 156.262s, baseline md5 unchanged |
| `go test ./goc -run TestCompareAllocationCensus -v` | **PASS**, 1 test + 1 test with 5 subtests, 0.010s |
| `go test ./opt/ ./ir/` | **PASS** (opt 0.967s, ir 0.014s) |
| census with the publication fix reverted (control) | **FAIL as designed**, exit 1, 9 sites named |
| working tree after all of it | **clean**, baseline `9a2ed8f8dff5be7e99adae8fb91fbd80` |

**Stability across the rebase: confirmed.** Three independent full corpus passes
on the final tree -- run 1, run 2, and the one inside `go test ./goc/...` -- each
a fresh compile of all 385 programs re-deriving the census from scratch, all
three reproducing the accepted baseline exactly.

### Baseline contents, recounted independently

Section 4's figures were not taken on trust; they were recounted from the
committed file with `awk` on the final tree, and all of them hold:

| | count |
|---|---|
| file lines / comment lines / **records** | 18,713 / 30 / **18,683** |
| distinct sites (first four fields) | 18,244 |
| placed `heap` / `frame` | 18,183 / 500 |
| positionless (`?`) | 5,870 |
| allocators | `newobject` 17,208, `makemap` 813, `makeslice` 437, `makechan` 223, `newarray` 2 |
| corpus programs (`goc/testdata/*.go`) | 385 |

### Nothing marked UNVERIFIED

Every item this job was asked for was run to completion. The one number this job
could **not** confirm is the premise's "22 sites", and section 11 says so
explicitly rather than restating it: this instrument counts 9, and the difference
is recorded as an open discrepancy for whoever produced the 22.

## 13. Bottom line

* The rebase onto `main` (`05946f2`) is clean; only `CCWORK_REPORT.md` conflicted
  and no compiler or test file did.
* **No compiler behaviour was changed by this job.** The only code on this branch
  is the census instrument itself, which the previous job established is
  diagnostic-only.
* **Baseline delta: 0 sites.** The premise that it was stale is wrong -- the
  branch point already carried the publication fix -- and the regenerated file is
  byte-identical to the committed one. No unexplained site.
* **`go test ./goc/...`: 609 RUN / 609 PASS / 0 FAIL, exit 0**, +8 over `main`'s
  601, which is exactly this branch's new tests.
* The failure output names every moved site with position, function, allocator,
  type and direction. Verified by making it fire on the final tree.

---

# Part III -- reconciling "22 sites" with "9 sites"

Section 11 left a discrepancy open: `ccwork/escape-frame-publication` reported
that the publication fix moved **22 allocation sites frame->heap, none the other
way**, and the census reported **9 heap records at 7 source positions**. It
offered two candidate explanations and could not choose between them without
re-running the earlier instrument.

This part re-ran it. Both instruments were run over the *same* compiles of the
*same* 385 programs, on the fixed tree and on a tree with the fix reverted, and
the answer is now measured rather than argued.

## 14. The reconstruction, and that it is faithful

`goc/compile.go` was reverted to its pre-fix state in a throwaway worktree:

    git worktree add $TMPDIR/nofix HEAD --detach
    git revert --no-commit fc74294 && git revert --no-commit 6245dbb
    git checkout HEAD -- CCWORK_REPORT.md
    git diff 6245dbb~1 -- goc/compile.go     # empty: byte-identical to pre-fix

Those are the only two commits that have ever touched `goc/compile.go` since the
fix, so the reverted file is exactly the pre-fix file. No other code differs;
`goc/testdata/frame_escape_baseline.txt` regains its three pre-fix lines, which
is the fix's own recorded effect.

A single harness then compiled all 385 programs in each tree and emitted, from
one compile of each program, **both** measurements:

* the ad-hoc one, verbatim from its own description -- per function, frame
  allocations (`OAlloc*`, `OAllocN`) against allocator calls (`runtime.newobject`,
  `newarray`, `makeslice`, `mallocgc`, `makemap`, `makechan`) plus any residual
  `OHeapAlloc`; and
* `opt.AllocationCensus`, per program, un-folded.

**The reconstruction reproduces the ad-hoc report's corpus totals exactly:**

    | corpus totals | frame | heap |
    |---|---|---|
    | before (measured here) | 9 735 484 | 509 897 |
    | before (as published)  | 9 735 484 | 509 897 |
    | after  (measured here) | 9 735 471 | 509 920 |
    | after  (as published)  | 9 735 471 | 509 920 |

Four numbers, four matches. The harness is measuring what the earlier instrument
measured. And the census half of the same run reproduces the committed baseline
byte for byte -- 18,683 records, `diff` exit 0 -- so both sides are trustworthy.

## 15. Both numbers are right. They count different units.

    (program, function) pairs whose alloc counts changed:  22
    census records added:                                   9
    distinct source positions:                              7

Every one of the 22 pairs, printed:

| source position | function | corpus programs | ad-hoc pairs | census records |
|---|---|---|---|---|
| `bigmod/nat.go:939:24` | `bigmod.Nat.Mul` | 10 | 10 | **1** |
| `x509/verify.go:1059:39` | 3 distinct `initfunc` symbols | 8 | 8 | **3** |
| `range_target_order.go:144:13` + `:152:12` | `main.targetAliasingTheRangeExpression` | 1 | 1 | **2** |
| `debug_gc_controls.go:15:28` | `main.main` | 1 | 1 | **1** |
| `slice_pointer_append_gc.go:10:31` | `main.main` | 1 | 1 | **1** |
| `signal_during_gc.go:23:28` | `main.main.func.17.5` | 1 | 1 | **1** |
| | | | **22** | **9** |

Two independent factors separate them, pulling in opposite directions:

1. **Corpus multiplicity.** The ad-hoc count counts a site once per corpus
   program that contains it; the census deduplicates across the corpus.
   `Nat.Mul` is linked into 10 programs and `x509.anyPolicyOID` into 8, so 18 of
   the 22 pairs are 2 sites seen repeatedly. This is the whole of the gap
   except for the next point.
2. **Function versus site.** The ad-hoc unit is a *function*; the census unit is
   a *site*. `targetAliasingTheRangeExpression` contains two moved literals and
   counts once ad-hoc, twice in the census. Conversely the 8 x509 programs
   inline the init into only 3 distinct symbol names, so 8 pairs fold to 3
   records rather than 1.

        22  -  9 (Nat.Mul: 10 programs -> 1 record)
            -  5 (x509: 8 programs -> 3 records)
            +  1 (range: 1 function -> 2 sites)
        =   9

Neither instrument is wrong and neither is measuring the other's quantity.

## 16. All 22 are real moves -- and the ad-hoc frame counter under-reports by 10

The published deltas do not obviously agree with "22 moved": the corpus frame
total fell by **13**, not 22, and the heap total rose by **23**. Both anomalies
resolve, and neither is a counting mistake in the published table.

**+23 rather than +22** is the function-versus-site factor again:
`targetAliasingTheRangeExpression` contributes one (program, function) pair and
two allocations. 22 pairs contain 23 allocations. *22 was never an allocation
count.*

**-13 rather than -23** is an artifact of the ad-hoc instrument. Nine pairs gain
a heap allocation without losing a frame one:

    initfunc anyPolicyOID (x8 programs)      frame 2->2   heap 0->1
    main.targetAliasingTheRangeExpression    frame 48->48 heap 3->5

At first reading that looks like a heap allocation *added* beside a frame slot
that stays. It is not. The frame slot stays in the IR but stops being used:

    x509/verify.go:1059:39   before: OAlloc8 size=40  used by 7 instructions
                             after:  OAlloc8 size=40  used by 0 instructions   + heap newobject

    range_target_order.go:144:13   before: OAlloc8 size=24  used by 5   after: 0 uses + heap newobject
    range_target_order.go:152:12   before: OAlloc8 size=24  used by 5   after: 0 uses + heap newobject

`goc/compile.go:11703` emits the frame slot for a composite literal
**unconditionally** and then, four lines later at 11708, overwrites the variable
with a heap allocation if the object escapes:

    backing := g.localAlloc(alignment, int(backingSize))
    ...
    if heap || !g.valueDoesNotEscape(literal) {
        backing = g.allocateEscapingTyped(backingType)
    }

The orphaned `OAlloc` is still in the finished IR, so an instrument that counts
`OAlloc` instructions counts it. (`goc/compile.go:11739`/`11743` is the same
shape for array and struct literals.) The `make` path does not do this, which is
why `Nat.Mul`, `debug_gc_controls`, `slice_pointer_append_gc` and
`signal_during_gc` all show the frame slot genuinely gone:

    nat.go:939:24              before: OAlloc8 size=512   after: absent, heap newobject
    debug_gc_controls.go:15:28 before: OAlloc8 size=1024  after: absent, heap newobject
    slice_pointer_append:10:31 before: OAlloc8 size=32    after: absent, heap newobject
    signal_during_gc.go:23:28  before: OAlloc8 size=8192  after: absent, heap newobject

Ten dead slots (8 x509 + 2 in `targetAliasing`) explain the gap exactly:
**23 - 10 = 13**, the published frame delta, to the unit. That closure is what
makes this a measurement rather than a story.

**So the direction claim was right: all 22 pairs moved frame->heap, and none
moved the other way.** What was wrong was calling them "22 allocation sites".
They are 23 allocations at 9 sites at 7 source positions, seen 22 times.

**Noted in passing, not fixed** (out of scope, and no correctness consequence):
the front end leaves a dead `OAlloc` behind at every escaping composite literal.
I checked only that it survives into the finished IR that both instruments read;
I did **not** check whether frame layout still reserves the bytes, so this may
cost nothing at all. Worth one look by whoever owns `compositeLiteral`.

## 17. The census does have a blind spot -- but it is not what cost the count

Explanation (b) from section 11 is **confirmed**. In the pre-fix tree the census
holds **zero records at all 7 positions** -- not a `frame` record, not anything:

    bigmod/nat.go:939                      before=0  after=1
    x509/verify.go:1059                    before=0  after=3
    runtime_debug_gc_controls.go:15        before=0  after=1
    runtime_range_target_order.go:144      before=0  after=1
    runtime_range_target_order.go:152      before=0  after=1
    runtime_slice_pointer_append_gc.go:10  before=0  after=1
    stdlib_signal_during_gc.go:23          before=0  after=1

The cause is exact. `AllocationCensus` builds its `frame` half only from
`module.AllocDecisions`, and only `opt.LowerHeapAllocations` writes that slice.
All 7 positions were ordinary front-end frame slots (`OAlloc8`, from
`gen.localAlloc`/`localAllocTyped`) that never became `OHeapAlloc` candidates,
so the escape pass never saw them and never recorded a decision. Section 2
excludes those by design.

Three consequences, in increasing order of importance:

1. **Detection is not blind, and this is the proof.** All 9 records changed and
   `TestAllocationCensus` fails, naming every site with position, function,
   allocator and type. Section 2's claim that the exclusion "does not create a
   blind spot for the regression class this instrument exists to catch"
   **holds** -- measured, against the largest real placement change in the
   tree's history, not argued.
2. **Direction labelling is blind.** All 9 land in `appeared`/`vanished`; the
   `frame -> heap` bucket was **empty** for a change that moved 23 allocations
   frame->heap. A reviewer reading the four buckets the way section 3 documents
   them would conclude that nothing moved. That is the instrument saying
   something false, not merely saying less.
3. **It cannot tell a move from a new allocation.** A record appearing means
   "this is now on the heap"; whether a frame slot went away with it, or the
   allocation is net new on a path that had none, is not in the census. Section
   16 needed a liveness pass over the IR to answer that -- for exactly the
   allocations the census had just reported. Those two cases have opposite
   costs. For an instrument whose job is to make allocation placement reviewable
   during an escape-analysis rearchitecture, that is the gap worth closing.

**The blind spot cost the label, not the count.** The 22-vs-9 gap is entirely
explanation (a); explanation (b) is real but contributed nothing to it. Section
11 could not separate them and was right to refuse to guess.

## 18. Should the census record front-end frame slots? Measured: no.

The brief allowed implementing this behind an option **if proven right**. It is
not, and it is not close. Two independent reasons, both measured.

**1. It would not produce the label it is wanted for.** A site's identity is
`position + function + allocator + type` (`Allocation.Site()`,
`opt/alloccensus.go:61`). An `OAlloc` carries an alignment and a byte size and
nothing else -- no allocator, no type. So a front-end slot at
`x509/verify.go:1059:39` would be recorded as

    stdlib/src/crypto/x509/verify.go:1059:39  initfunc...  -  40  frame

against the post-fix heap record

    stdlib/src/crypto/x509/verify.go:1059:39  initfunc...  runtime.newobject  5_uint64  heap

Different site keys. The diff would **still** read as one line vanishing and
another appearing, exactly as it does today, plus 165,544 lines of noise. The
only way out is to weaken the site key to `position + function`, which would
merge distinct allocations that share a position -- and section 4's 439
both-ways sites show that is not hypothetical.

**2. The volume is prohibitive.** Measured over the same 385-program corpus:

    | | count |
    |---|---|
    | front-end frame-slot instructions | 9,735,471 |
    | deduplicated `(position, function, size)` | **165,544** |
    | deduplicated `(position, function)` | 145,254 |
    | of those, positionless (`?`) | 14,463 |
    | census records today | 18,683 |

A **8.9x** larger baseline: 2.0 MB becomes roughly **14 MB**, of which 12.3 MB
is untyped slot records that no escape decision can ever move. The census is
meant to be read as a diff by a person; 165k lines of `size=8` local slots is
how an instrument stops being read.

**What should be done instead.** The gap in section 17 is not "the census cannot
see frame slots". It is that **`goc`'s front end makes placement decisions and
does not record them**, while `opt.LowerHeapAllocations` makes the same kind of
decision and does. `ir.AllocDecision` and `Module.AllocDecisions` already exist,
are already diagnostic-only, and already carry exactly the right fields
(position, function, allocator, type, placement). The front end has all four at
the point of decision -- `compile.go:11708` knows `backingType` and knows which
branch it is taking; so do the seven other `allocateEscapingTyped` call sites
(`5600`, `5602`, `5606`, `10804`, `11617`, `11651`, `11709`, `11744`) and their
`localAlloc*` frame counterparts.

Recording an `AllocDecision` at each of those, with the type the front end
already has, would:

* keep the site key intact, so the x509 and `Nat.Mul` records would have read
  `frame -> heap` rather than `appeared`;
* distinguish a move from a new allocation, which is consequence 3 above;
* add records only for objects whose placement was *decided* -- the same
  population that already yields the 18,183 heap records -- not for the 9.7M
  ordinary locals;
* need no new mechanism and no behaviour change: it is the append that
  `opt/escape.go` already does, at the other decision site.

That is a change to `goc/compile.go`, so **this job did not make it** -- the
brief says do not change compiler behaviour, and while an append to a
diagnostic slice does not change behaviour, sizing and reviewing eight new
decision points is a separate change with its own risk budget. It is written up
here as the concrete proposal, with the call sites enumerated, so it can be
picked up as one.

## 19. Verification ledger for this job

Every process was waited on to exit in the foreground and every number is read
from its own output. **Nothing was left running.** No compiler or test file on
this branch was changed; the only tracked edit is this report. The three
measurement harnesses were scratch files in `$TMPDIR`, copied in, and deleted.

| what | result |
|---|---|
| reverted worktree vs pre-fix commit (`git diff 6245dbb~1 -- goc/compile.go`) | **empty** -- the control is exactly the pre-fix compiler |
| ad-hoc corpus totals, before | frame **9 735 484**, heap **509 897** -- matches published |
| ad-hoc corpus totals, after | frame **9 735 471**, heap **509 920** -- matches published |
| changed `(program, function)` pairs | **22** -- matches published |
| census records added by the fix | **9**, at **7** source positions, 0 removed, 0 re-placed |
| census run of this job's own harness vs committed baseline | **`diff` exit 0**, 18,683 records |
| pre-fix census records at the 7 positions | **0** -- blind spot confirmed |
| liveness of the 10 surviving frame slots, post-fix | **0 real uses** -- dead, so all 22 pairs are moves |
| front-end frame slots, deduplicated | **165,544** records / 12.3 MB |
| `go build ./...` | **exit 0** |
| `go vet ./opt/ ./ir/ ./goc/` | **exit 0** |
| `go test ./goc -run TestAllocationCensus` run 1 | **PASS**, 171.975s, baseline md5 unchanged |
| `go test ./goc -run TestAllocationCensus` run 2 | **PASS**, 148.149s, baseline md5 unchanged |
| `go test ./goc -run TestCompareAllocationCensus` | **PASS**, 0.011s |
| `go test ./opt/ ./ir/` | **PASS** (0.975s / 0.013s) |
| working tree | **clean** but for this report; baseline `9a2ed8f8dff5be7e99adae8fb91fbd80` |

The full corpus suite was not run; another job covers it.

Four independent full-corpus census passes now reproduce the accepted baseline:
sections 12's three, plus this job's own harness, which derived it from a
different program with a different code path into `opt.AllocationCensus`.

## 20. Bottom line

**How many sites did the publication fix move?** Under the definition that
should be used going forward -- a **site** is one source position in one
function after inlining, deduplicated across the corpus -- the answer is
**9 sites, at 7 distinct source positions**. All 9 moved **frame -> heap**;
none moved the other way.

The other quantities, so nobody has to re-derive them:

| quantity | value |
|---|---|
| **allocation sites moved (census scope)** | **9** |
| distinct source positions | 7 |
| `(program, function)` pairs affected | 22 |
| allocation instructions moved to the heap | 23 |
| frame allocations actually removed | 13 (10 more are left dead in the IR) |

**Neither the 9 nor the 22 was wrong; the sentence around the 22 was.** "22
allocation sites move from frame to heap" should have read "22 (program,
function) pairs", which is 9 sites seen repeatedly -- `bigmod.Nat.Mul` is linked
into 10 corpus programs and `x509.anyPolicyOID`'s init into 8. Repeating "22
sites" as a site count in task records and status reports overstates the fix's
footprint by 2.4x.

**Use the census definition going forward.** It is deduplicated, so it does not
scale with how many corpus programs happen to link a package; it is per site
rather than per function, so two moved literals in one function are two facts;
and it counts what changed rather than what an `OAlloc` instruction count says,
which section 16 shows can be wrong by 10 out of 23. The other numbers are still
worth quoting, but as what they are: 22 is corpus exposure, 23 is the runtime
allocation cost.

**Does the census have a blind spot? Yes, and it is a labelling one.**
It records frame placements only when `opt.LowerHeapAllocations` made them, so
every placement decided in `goc`'s front end is invisible on the frame side.
Detection is unaffected -- all 9 changes failed the baseline, each named with
position, function, allocator and type, which is the property section 2 claimed
and this job measured. But the direction is not: the fix's 23 frame->heap moves
were reported as `appeared`, and the `frame -> heap` bucket was empty. The
census also cannot distinguish a move from a net-new allocation.

**Recording front-end frame slots is not the fix** -- proven in section 18, not
guessed: the records would be untyped and so would not even match the site key
the label depends on, and there are 165,544 of them against 18,683 today. The
fix is to have the front end write `ir.AllocDecision` at its eight placement
decision points, which needs no new mechanism and no behaviour change. Written
up in section 18 with the call sites enumerated; **not implemented here**,
because it touches `goc/compile.go`.
