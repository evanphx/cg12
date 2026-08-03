# Escape wave 2 — integration report

`integration/escape-wave2` merges four changes onto `main` (`4a6fd96`) through
three merge commits, plus this job's baseline regeneration and gate run.

    main 4a6fd96
      ├── ccwork/iface-convt-fastpath ──── ccwork/variadic-escape-question   (change 1)
      ├── ccwork/slog-attr-gcmask                                            (change 2)
      └── ccwork/iface-init-dispatch                                         (change 3)

`iface-convt-fastpath` is contained in `variadic-escape-question` and is reported
on together with it below; its own standalone report is at
`19488ee:CCWORK_REPORT.md`.

Contents:

  - **Part 0** — the merge gate: everything run against the integrated tree.
  - **Part 1** — asking the escape question for a variadic call.
  - **Part 2** — a `slog.Attr` in a frame scanned as a pointer.
  - **Part 3** — the package-initializer dispatch miscompile.

Host toolchain throughout: `go1.26.1 linux/arm64`.

---

# Part 0 — merge gate

Verification of the four-change integration branch (`iface-convt-fastpath` →
`variadic-escape-question`, `slog-attr-gcmask`, `iface-init-dispatch`) against
`main` (4a6fd96). Every number below was produced by a process this job started
and watched exit. Host toolchain `go1.26.1 linux/arm64`, 64 cores.

## 0. Topology, restated correctly

    main 4a6fd96
      ├── ccwork/iface-convt-fastpath ──── ccwork/variadic-escape-question
      ├── ccwork/slog-attr-gcmask
      └── ccwork/iface-init-dispatch

`git merge-base main <branch>` is 4a6fd96 for all three merged heads.
`variadic-escape-question` *contains* `iface-convt-fastpath` (b798af9…19488ee are
in its history); `slog-attr-gcmask` and `iface-init-dispatch` are independent of
both. So the merge composes four changes through three merge commits.

## 1. The four regenerated baselines

All four were regenerated from this tree with the command in each file's header,
each run waited on to exit rc=0.

### 1.1 `slog_allocations_baseline.txt` — WAS STALE, now correct

    go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations -update-slog-allocations
    → rc=0, 32 cases, 20.9s

The committed file was byte-identical to `slog-attr-gcmask`'s version, i.e.
measured on a tree **without** the convT fast path. The merge resolved the
conflict to that side. Regenerating moves 16 rows, all downward:

    case                        committed   regenerated   host gc
    control/any-int-small          1.00        0.00        0.00
    control/any-bool               1.00        0.00        0.00
    attr/slog.Int                  1.00        0.00        0.00
    attr/slog.Bool                 1.00        0.00        0.00
    attr/slog.Duration             1.00        0.00        0.00
    attr/slog.Float64              1.00        0.00        0.00
    info/1-attr                    2.00        1.00        0.00
    info/3-attr                    4.00        1.00        0.00
    info/5-attr                    6.00        1.00        0.00
    info/6-attr                    8.00        2.00        1.00
    info/3-attr-large-ints         4.00        1.00        3.00
    logattrs/3-attr                4.00        1.00        0.00
    logattrs/6-attr                9.00        3.00        1.00
    disabled/logattrs-3-attr       4.00        1.00        0.00
    json/kv-4-pairs                8.00        5.00        2.00
    json/logattrs-4-attrs          8.00        5.00        0.00

**Settled: the merged tree really does achieve the convT branch's 1.00 on
`info/5-attr`.** The 6.00 was a stale generated file, exactly as hypothesised.
No row moved *up*, and no row became `crash`.

### 1.2 `alloc_census_baseline.txt` — one stale line, and it is explicable

    go test ./goc -run TestAllocationCensus -update-alloc-census-baseline
    → rc=0, 188.2s

Regenerating changes **exactly one line** of 14846, and totals are unchanged
(14846 sites, 13676 heap, 1170 frame, before and after):

    - runtime_package_initializer_dispatch.go:46:51  …insideComposite  runtime.newobject  io_fs_FileMode  heap
    + runtime_package_initializer_dispatch.go:46:51  …insideComposite  runtime.convT32    io_fs_FileMode  heap

Site by site composition check. Taking each branch's own census delta against
`main` as a multiset of lines and applying them together:

    expected = main − removed(variadic) − removed(init) + added(variadic) + added(init)

gives 14846 lines that match the regenerated file **line for line except that
single pair**. (`slog-attr-gcmask` has a zero census delta; `iface-convt-fastpath`
is subsumed by `variadic`.) So:

    branch                     census lines   added vs main   removed vs main
    main                          14670            —               —
    iface-convt-fastpath          14680           562             552
    variadic-escape-question      14834           843             679
    slog-attr-gcmask              14670             0               0
    iface-init-dispatch           14682            24              12
    merged (regenerated)          14846           867             691

**The one unexplained line is the cross-branch interaction, and it is benign.**
`iface-init-dispatch` adds `runtime_package_initializer_dispatch.go`, which at
line 46 converts `fs.FileMode(0o644)` (a uint32) to `fmt.Stringer` inside a
package-scope composite literal. Its branch measured that site off `main`, where
the box is built by `runtime.newobject`. With `iface-convt-fastpath` also merged,
the same site is built by `runtime.convT32` — which is what that branch does to
every boxing site (its own delta is 552 `newobject` rows out, 466 `convT64` +
70 `convT32` + 16 `newobject` + 10 `convT16` rows in). Same position, same
function, same type, **same `heap` decision**; only the helper changed. Nothing
in the composition is unaccounted for.

### 1.3 `escape_shadow_baseline.txt` — was already correct

    go test ./goc -run TestEscapeShadowPlacement -update-escape-shadow-baseline
    → rc=0, 187.7s. Regenerated file is byte-identical to the committed one.

The merge resolution happened to take the right side here.

### 1.4 `escape_gc_differential.txt` — WAS STALE (see §9)

    go test ./goc -run TestEscapeDifferentialAgainstGC -escape-gc-differential -update-escape-gc-differential
    → rc=0, 10.8s

The committed file compared 388 corpus programs; this tree has 398, of which the
host builds 394. It was generated by `variadic-escape-question` before the other
two branches (which add 6 further corpus programs) were merged. Regenerated
numbers are in §9.

## 2. `go test -timeout 40m -parallel 10 ./goc/...` — **BRANCH FAILS, MAIN PASSES**

Both arms run to completion, waited on:

    tree      wall      result
    main      975.7s    ok      rc=0
    branch   1078.2s    FAIL    rc=1

### Census

    top level            main   branch
      PASS                314      319
      FAIL                  0        1
      SKIP                  4        4
    subtests
      PASS                212      242
      FAIL                  0        0

The +5/+30 is fully attributed by name. Six top-level tests exist only on the
branch, all passing, each traced to the commit that adds it:

    TestGoABIAggregateAgreesWithTheTypeLayout               387bb78  slog-attr-gcmask
    TestGoABIAggregatesAgreeWithTheirTypesInTheStdlib (+4)  70ff27d  slog-attr-gcmask
    TestSlogAttrFrameExpectationsMatchTheHostToolchain (+5) 88fb7e1  slog-attr-gcmask
    TestSlogAttrInFrameIsNotScannedAsAPointer (+10)         88fb7e1  slog-attr-gcmask
    TestSlogAttrInFrameSurvivesTheStackCopyChecker (+5)     88fb7e1  slog-attr-gcmask
    TestInterfaceConversionsCallTheRuntimeHelpers           5d4d7ec  iface-convt-fastpath

The remaining 6 new subtests are the two corpus programs
`variadic-escape-question` adds, appearing under the two existing loop-alias
tests (`variadic_element_retention.go`, `variadic_element_address_retention.go`,
each ×`TestLoopBodyAllocationsAreDistinctPerIteration` with and without `-O`, and
×`TestLoopAliasExpectationsMatchTheHostToolchain`). No test disappeared.
4 + 5 + 5 + 10 + 5 + 1(no subtests) + 6 = 30 exactly, nothing unaccounted for.

### The failure — a real regression, and a merge blocker

    --- FAIL: TestDeriveClassifiesEveryGenField (0.00s)
        derive_test.go:239: fullyPopulatedGen leaves [variadicPayloadSlot] zero,
        so derive's handling of those fields is untested; a new gen field has to
        be given a non-zero value there and classified in wholeCompilationGenFields

Deterministic; reproduces standalone in 0.02s (`go test ./goc -run
TestDeriveClassifiesEveryGenField -count=1`). Passes on `main`.

**Attribution: commit 577e2d5 "opt: give a split variadic payload somewhere to go
back to", on `ccwork/variadic-escape-question`.** It fails on that branch in
isolation too (checked at f68f513), so it is that branch's, not a merge artefact.
That branch's own report states `go test ./goc/...` in full was "deliberately not
run: a dependent job does that" — this is the thing that job was for.

**What it is and is not.** 577e2d5 adds `gen.variadicPayloadSlot`
(`goc/compile.go:1660`) and *does* reset it in `derive` (`goc/compile.go:1730`),
which is the correct classification for per-generator state; `variadicPayloadSlot`
is correctly absent from `wholeCompilationGenFields`. So the compiler's behaviour
is right. What is wrong is that the test fixture `fullyPopulatedGen()` in
`goc/derive_test.go:73` was not extended, so the field is zero in the parent, the
inherited-vs-reset comparison for it is vacuous, and the guard says so by design
rather than silently passing. **It is a test-hygiene defect, not a miscompile** —
but it makes `go test ./goc/...` red on the branch, and this is a guard whose
whole purpose is to fail when a `gen` field is added without being classified.

Diagnosis only, per the brief: no fix applied. The fix is to give
`variadicPayloadSlot` a non-zero value in `fullyPopulatedGen()`.

## 3. The capability matrix — `main` arms (branch arms in §3b)

`make test-goc-status` / `make test-goc-status-opt`, `GOFLAGS=-v`, run in
separate worktrees so the two pack builds cannot collide. No `(cached)`.

    main, default arm  rc=0   364 capabilities, 364 PASS, 0 FAIL   164.5s
    main, -O arm       rc=2   364 capabilities, 363 PASS, 1 FAIL   165.7s

The capability *sets* are identical between the two arms (364 names, `diff`
clean). The single `-O` failure on main:

    stack-scan/loop-safepoints
      runtime_stack_scan_loop_safepoints.go should pass: exit status 2
      collected while live: carried-0 at carried before rewrite
      panic: a stack slot live across a loop back edge was not a GC root

## 3b. The capability matrix — branch arms

    branch, default arm  rc=0   365 capabilities, 365 PASS, 0 FAIL   154.8s
    branch, -O arm       rc=2   365 capabilities, 364 PASS, 1 FAIL

No `(cached)` in any of the four logs. The branch's capability set is `main`'s
365th name added and nothing removed:

    + core-types/package-initializer-dispatch      (iface-init-dispatch, 0f80c37)

The `-O` failure set is **exactly `{stack-scan/loop-safepoints}` on both trees**,
with the same diagnostic and the same panic:

    collected while live: carried-0 at carried before rewrite
    panic: a stack slot live across a loop back edge was not a GC root

Confirmed as asked: identical capability, identical panic, pre-existing on `main`,
not introduced or widened by the merge. The `-O` arm's capability set is
identical to the default arm's on both trees, so the failure is a real `-O`
regression in the tree and not a matrix that ran a different set of programs.

## 4. `TestFrameEscapeAudit -count=1` — clean, and the entry count did not move

    branch   PASS  196.0s   rc=0
    main     PASS  190.7s   rc=0

**Zero new frame-address publications.** `goc/testdata/frame_escape_baseline.txt`
is byte-identical between `main` and this tree (`git diff` empty), 193 entries in
both, and the audit passes against it on both.

**Why the count did not change, given two branches move storage across the
frame/heap line.** The audit is a may-analysis over *publications* — sites where
the address of a frame slot becomes reachable from outside the frame. Both moves
are the wrong shape to add one:

  - `variadic-escape-question` splits a variadic call's convertible payloads out
    of the combined backing object. The array goes to the **frame** and the
    payload it used to contain becomes a `convT*` on the **heap**. A publication
    is only created by putting a *frame* address somewhere outliving; moving a
    payload to the heap removes that possibility rather than adding one. The
    branch's own report notes the one place it could have gone wrong —
    `promotionsBlockedByALoop` escaping candidates after the analysis has run,
    which is why `containedAllocationsEscape` runs again afterwards.
  - `slog-attr-gcmask` changes the GC *mask* of a `slog.Attr` already in a frame.
    Placement is unchanged, so there is no new address to publish.

So a flat 193 is the expected result, not an absence of evidence: the audit ran,
passed, and its input set is provably the same file.

## 5. The allocation census test, twice — stable

Run without `-update`, so both runs check the regenerated baseline:

    run 1   PASS  195.8s
    run 2   PASS  177.5s

The regenerated `alloc_census_baseline.txt` is reproducible: two checks and the
`-update` run that produced it all agree.

## 6. Determinism — 398/398 over 796 compiles

    scripts/determinism-check.sh -corpus -rounds 2 -j 24
    programs=398 rounds=2 workers=24 optimize=false
    round 0: 398 programs in 112.1s, 0 failed
    round 1: 398 programs in 110.9s, 0 failed
    failed to compile: 0
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    reproducible=398 varying=0 failed=0 of 398 over 2 rounds

`main`'s reference is 390/390 over 780 compiles. This tree has 398 corpus
programs (the 8 the three branches add) and every one is byte-identical across
both rounds — including the layout-residue column, which is 0. **No determinism
regression, and the eight new programs are themselves reproducible.**
`TestCompilingTheSameSourceTwiceGivesTheSameModule` also passes in the `./goc/...`
run (§2), which is the front-end half of the same property.

## 7. Loop aliasing — still matches the host, and the frame-locals stayed framed

`TestLoopBodyAllocationsAreDistinctPerIteration`,
`TestLoopAliasExpectationsMatchTheHostToolchain`, `TestLoopVarPerIteration`,
`-count=1`, on the branch (49.7s) and on `main` (26.1s) as control. **All PASS.**

The branch runs 7 programs where `main` runs 5; the two extra are
`variadic_element_retention.go` and `variadic_element_address_retention.go` from
`variadic-escape-question`. Every program passes both unoptimised and `-O`.

**`loop_alias_frame_local.go`'s allocations are still in frame slots.** The check
is the census, and it is a negative one: that program contributes **zero rows** to
`alloc_census_baseline.txt` on `main` and zero rows on this tree. The census
records every heap allocation and every frame allocation that came out of an
escape decision; the three loop-body allocations in that program are ordinary
front-end frame slots, so they appear nowhere. `goc/loopalias_test.go`'s own
comment states the failure mode explicitly — "a rule that heaped loop-body
allocations … TestAllocationCensus would gain a `runtime.newobject` line for
each". There is no such line. The program also still prints
`framed: 18 / within: 12 / literal: 18`, matching the host.

## 8. GC reducer at `GOGC=10` — **NOT 0/20, and it is not the branch**

`goc/testdata/runtime_gc_type_mask_padding.go`, `GOMAXPROCS=3`, 20 runs per arm.
Run twice per tree: once while the determinism sweep was loading the box, once on
an idle box.

    arm                      loaded    idle    total
    branch, GOGC=10           4/20     2/20    6/40   (15%)
    main,   GOGC=10           5/20     3/20    8/40   (20%)
    branch, GOGC=100 (default)   —     0/20    0/20
    main,   GOGC=100 (default)   —     1/20    1/20

**The hard requirement as stated is not met — but `main` fails it too, at a
slightly higher rate.** This is a documented pre-existing defect, not a merge
regression. `RUNTIME_PLAN.md` §26 ("What this does not fix") records it: *"At
`GOGC=10` both trees still fail the reducer and `mark-workers` at 3-15%, and
`main`'s rate at that `GOGC` before this fix was 14/100."* My 15% and 20% sit at
and just above the top of that band, on 40 samples each — the difference between
the two arms is not resolvable at this sample size.

The signature matches the route §26 names, and is **not** the type-mask defect
the reducer was built for:

    runtime: pointer 0x139ec6222058 to unused region of span … span.state=1
    runtime: found in object at *(0x139ec5fb3c90+0x208)
    object= … s.elemsize=8192 s.state=mSpanManual

`mSpanManual` with an 8192-byte element is a **goroutine stack**, and the words
around the reported offset are small integers (`0xf3`, `0x18a`, `0x18b`, `0x25f`)
— an imprecise precise-stack-scan root, i.e. §26's
`badPointer ← findObject ← scanblock ← scanframeworker ← scanstack ← markroot`.
The original defect died with *"found bad pointer in Go heap"* out of a bulk
barrier; nothing in these 120 runs produced that message.

At the default `GOGC` the branch is **0/20** and `main` is **1/20**, and the
matrix capability `gc-invariants/type-mask-padding` passes on both trees in all
four matrix runs (§3, §3b).

## 9. The headline measurements, taken fresh

### 9.1 `fmt.Sprintf("value=%d", 42)`

`goc/testdata/allocation_counts.go`, row `sprintf_int`, run under each compiler
just now. Counts are allocations per 100 calls:

    compiler / tree                    sprintf_int
    goc, this tree                        100   = 1.00 per call
    goc, main                             200   = 2.00 per call
    host go1.26.1 (gc), this tree         100   = 1.00 per call
    host go1.26.1 (gc), main              100   = 1.00 per call

**`fmt.Sprintf("value=%d", 42)` costs 1.00 allocation under the merged tree,
which is exactly the host toolchain's number, down from 2.00 on `main`.** The
allocation that went away is the box: the `[1]any` backing array is now a frame
slot and the `int` payload is a `runtime.convT64` whose value 42 is inside
`runtime.staticuint64s`, so it does not allocate at all. gc reaches 1.00 the same
way, at the same two columns.

Neighbouring rows measured in the same run, for honesty about where the tree is
still behind gc:

    row                        goc    gc
    sprintf_two_small_ints    2.00   1.00   goc pays a second box gc does not
    sprintf_two_large_ints    2.00   3.00   goc pays one fewer
    box_small_int             0.00   0.00
    box_bool                  0.00   0.00
    box_large_int             1.00   0.00   past staticuint64s; gc uses a
    box_float64               1.00   0.00     read-only box for a package-level
    box_string                1.00   0.00     variable, goc allocates
    box_pointer               0.00   0.00

### 9.2 slog — settled

Fresh, from the regeneration in §1.1 and re-measured twice since:
`TestSlogAllocationsAgainstGC` **passes** against the regenerated file (26.3s),
and a second independent `-update` run in a separate worktree produced a
**byte-identical** file. So these are measurements, not a table:

    case                      goc a/op   gc a/op   goc B/op   gc B/op
    info/5-attr                  1.00      0.00      288.0       0.0
    disabled/no-attrs            0.00      0.00        0.0       0.0
    disabled/3-attr              1.00      0.00      176.0       0.0
    disabled/logattrs-3-attr     1.00      0.00      128.0       0.0

**The merged tree really does achieve the convT branch's 1.00 on the
5-attribute case.** The committed 6.00 was the stale generated file described in
§1.1 and nothing else; three independent measurements of this tree agree on 1.00.

The disabled-level cases: `disabled/no-attrs` is 0.00 against gc's 0.00 — goc
matches. `disabled/3-attr` and `disabled/logattrs-3-attr` are 1.00 against gc's
0.00: goc still allocates the `[]any`/`[]Attr` backing array for a call whose
level is disabled, because the array is built before `Enabled` is consulted and
goc does not sink it past the branch. That is a **1-allocation gap, down from 4**
on the committed baseline, and it is the remaining slog gap worth naming.

## 10. The gc differential — the new matrix, and what moved

Regenerated in §1.4. Coverage grew because the branches add corpus programs:

                                        main    this tree
    corpus programs                      390        398
    compared (host built them)           386        394
    census rows joined                  2706       2791
    gc decisions joined                 3388       3434

### The confusion matrix

    rows are goc's verdict for the line, columns are gc's.

    main (4a6fd96)                      this tree (63c7d4e)
      goc\gc  frame  heap  mixed  absent  total    goc\gc  frame  heap  mixed  absent  total
      frame      35     3      2      30     70    frame      37     3      3      30     73
      heap      179  1768    192     174   2313    heap      172  1778    175     170   2295
      mixed       3    54      8       7     72    mixed      14    56     30      13    113
      absent    457   119     23       0    599    absent    463   121     23       0    607
      total     674  1944    225     211   3054    total     686  1958    231     213   3088

    PERMISSIVE (gc heaps, goc does not)   209  ->  236
    PESSIMISTIC (goc heaps, gc does not)  563  ->  574

The interesting cell is `mixed × mixed`, **8 → 30**. That is the variadic split
landing on gc's own answer: one source line where goc now puts the `[N]any` in
the frame and the box on the heap, at the same two columns gc does. `heap × mixed`
falls 192 → 175 and `heap × frame` falls 179 → 172 in step with it.

### Did the count of lines where goc heaps what gc frames rise or fall?

It rose, 563 → 574 (+11). **It is not a regression.** Resolving the set by
`(file, line)` key rather than by count:

    19 source lines entered the pessimistic set
     8 source lines left it

and every one of the 27 is in a file a branch added or edited:

    entered                                            why
      allocation_counts.go × 15                        convt + variadic add
      variadic_element_retention.go:39,42              new program (variadic)
      variadic_backing.go:8                            see below
      runtime_package_initializer_dispatch.go:46       new program (init)
    left
      allocation_counts.go × 7                         line numbers moved
      runtime_range_target_order.go:88                 real improvement

**Not one line in a corpus program no branch touched changed its membership.**

`runtime_range_target_order.go:88` is the one that *left*, and it left for the
right reason — goc's verdict on it went `heap → frame`:

    -  goc  col 15  heap   runtime.newobject  struct_values__1_any__payload0_int
    +  goc  col 15  frame  runtime.newobject  struct_values__1_any__payload0_int

`variadic_backing.go:8` is the one entry in an *unedited* file, and it is a known,
documented cost rather than a surprise. In `main` that program contributes one
census site (the `[1]any` backing, in the frame); in this tree it contributes that
plus three new rows for `x := 42` at 8:2, on the heap, where gc keeps `x` in the
frame. This is `variadic-escape-question`'s §3.2 "refusal": the front end's AST
walk consulted `parameterDoesNotEscape` at the variadic *parameter* to answer for
an *element* of it, which was unsound (it is what the `found bad pointer in Go
heap` reduction was about), so the walk now declines to answer and the value is
heaped. **Two source lines across 394 corpus programs and the whole vendored
stdlib is the total price** — the other is `h2_bundle.go:8143`. The branch's own
report states this and the census I regenerated confirms it independently.

### Is the earlier 563 → 567 an artefact?

Yes, and specifically it is an artefact of **added test source**, not of the
census recording `convT` sites. On `iface-convt-fastpath` alone the pessimistic
delta against `main` is 11 lines in, 7 out, **all eleven and all seven in
`goc/testdata/allocation_counts.go`** — the single corpus program that branch
edits, adding `box_large_int`, `box_bool`, `box_float64`, `box_string` and
`return_any_from_large_int` (40 lines, which also renumber everything below).
No line in any other program changed membership. So the +4 is four new
deliberately-pessimistic rows the branch wrote in order to measure them.

There is a second, independent reason the pessimism headline cannot fall here,
which `iface-convt-fastpath` and `variadic-escape-question` both state and which
my numbers bear out: `opt.conversionHelpers` records a `convT` site as **heap**
because the payload left the frame — which is the escape decision gc's `-m`
reports at that line — *whether or not the helper allocates at runtime*. A
`fmt.Sprintf("%d", 42)` line therefore keeps its pessimistic entry while its
measured cost halves from 2.00 to 1.00 (§9.1). The differential measures
placement; `allocation_counts.go` measures cost. Both were run; they disagree for
a reason that is understood and written down.

## 11. What this job did not run

`make test-unit` was not re-run: the previous gate job confirmed it (main
1598/0, branch 1607/0, the +9 attributed by name to `opt/escape_test.go`'s
`TestLowerHeapAllocations*` cases). Everything else in the brief was run here and
watched to exit. `test-goc-corpus`, `test-goc-cmd` outside the matrix, and the
runtime-coverage target were not in the brief and were not run.

## 12. Verdict

**NOT SAFE TO MERGE TO MAIN** — one blocker:

> `go test ./goc/...` **fails on this branch and passes on `main`**:
> `TestDeriveClassifiesEveryGenField`, deterministic, 0.02s to reproduce.
> Commit 577e2d5 on `ccwork/variadic-escape-question` adds `gen.variadicPayloadSlot`
> without extending `fullyPopulatedGen()` in `goc/derive_test.go:73`. The
> compiler's handling of the field is correct (`derive` resets it, and it is
> rightly absent from `wholeCompilationGenFields`); what is broken is that the
> guard cannot verify it and says so. It is a one-line test-fixture fix, not a
> miscompile — but it leaves the tree red, and this guard exists precisely to
> stop a `gen` field landing unclassified.

Nothing else found is disqualifying. For the record, everything else passed or
is pre-existing on `main`:

    baselines regenerated, diffs reviewed, committed          63c7d4e
    census composition                    exact but 1 line, explained (§1.2)
    capability matrix, default arm        365/365 branch, 364/364 main
    capability matrix, -O arm             {stack-scan/loop-safepoints} on BOTH
    TestFrameEscapeAudit                  PASS both; 193 entries, unchanged
    allocation census, twice              PASS, PASS
    determinism                           398/398 over 796 compiles
    loop aliasing                         PASS both trees, both -O arms
    loop_alias_frame_local.go             still zero census rows (framed)
    GC reducer, GOGC=10                   6/40 branch vs 8/40 main — §26's
                                          known stack-scan defect, on main too
    GC reducer, default GOGC              0/20 branch vs 1/20 main
    fmt.Sprintf("value=%d", 42)           1.00 goc vs 1.00 gc  (main: 2.00)
    slog info/5-attr                      1.00 goc vs 0.00 gc  (stale file: 6.00)
    slog disabled/3-attr                  1.00 goc vs 0.00 gc  (stale file: 4.00)
    gc differential pessimistic           563 -> 574, every moved line in a
                                          file a branch added or edited

---

# Part 1 — Asking the escape question for a variadic call: splitting the object that made it unanswerable

Branch `ccwork/variadic-escape-question`, off `ccwork/iface-convt-fastpath`
(`19488ee`). The previous jobs' reports are at `19488ee:CCWORK_REPORT.md` and
`4a6fd96:CCWORK_REPORT.md`.

Status: COMPLETE. Every number below was measured, under goc built from this
branch and under `go run`, and watched to completion.

**Headline: `fmt.Sprintf("value=%d", 42)` costs goc 1.00 allocations against
gc's 1.00 — exact parity, from 2.00. The `[N]any` backing
array of a variadic call is now a frame slot wherever the callee does not retain
the slice itself, and the boxed payload an element points at is decided
separately from it. The combined object was split, partially and deliberately;
section 2 prices both directions. The retention hole that forced the previous
attempt back to 2.00 is closed by construction rather than by an extra rule: the
callee that keeps `args[0]` now keeps a payload that is its own allocation, and
that payload goes to the heap while the array does not.**

## 1. What was actually wrong, confirmed before anything was changed

Two instruments, both on the base commit.

`goc/compile.go:6581` on the base commit (`goc/compile.go:6626` here, and
`:6428` on `main` before the interface-conversion branch moved it) decides
between a frame `[N]any` and a heap one:

    stackAllocateVariadic := !g.runtimeAllocation || g.fn.NoSplit || g.forceStackVariadic

and `forceStackVariadic` comes from a two-symbol allowlist. So the front end
never asks. That is true and it is not the whole story: the heap arm emits the
*neutral* `ir.OHeapAlloc` candidate, and `opt.LowerHeapAllocations` — which runs
unconditionally, `goc/compile.go:488`, not only under `-O` — does ask. The
question is asked; the representation is what made it unanswerable.

The lines under it build one synthesized `struct{values [N]any; payload0 T0;
...}` per call site and allocate the backing array and every boxed payload as a
single object. One object is one placement.

A diagnostic added to `opt` for this job (`GOC_DIAG_ESCAPE`, deleted before the
branch closes) prints where each candidate landed and the first use that escaped
it. On the base commit, for `fmt.Sprintf("value=%d", n)`:

    main.doSprintf .goc.runtime.type.struct_values__1_any__payload0_int  heap
        argument 1 of $fmt.Sprintf may retain something inside a self-referential object

and with the `needsDeepSummary` rule switched off, the same object is `frame`.
So **`fmt.Sprintf`'s `a []any` parameter does not escape at depth 0** — the
array was on the heap solely because the box inside it is retained.
`fmt.pp.doPrintf` assigns each element to `p.arg`, a field of a heap-allocated
printer, so the box genuinely is retained: this is not a conservatism to be
analysed away.

For `log/slog.Logger.Info` the same diagnostic says something different:

    argument 2 of $log/slog.Logger.Info escapes            (with the deep rule off)

and the parameter table agrees:

    FACT log/slog.Logger.Info param 2 "args.0" = escapes deep=false

The slice **itself** escapes there, through `Logger.log` → `Record.Add` →
`argsToAttr`, which returns a slice derived from `args` in a loop. That
difference decides the design, and section 2 is about it.

## 2. Pricing the two representations, which is the whole design

The brief asks what the combined-object representation costs and whether
splitting is the cheaper path. Both were measured, in that order, and the answer
is **neither on its own** — which is why the final shape is a split with a
fold-back rather than one or the other.

### 2.1 What the combined object costs

Exactly the remaining gap, and no analysis can remove it. One object is one
placement, so the array's placement is the *maximum* of the array's own answer
and every payload's. `fmt.Sprintf`'s array is `noescape` at depth 0 and its box
is genuinely retained, so the max is "heap" and the array pays an allocation gc
does not. This is not conservatism: `p.arg = arg` in `fmt.pp.doPrintf` really
does keep the box, and any sound analysis must say so.

Measured, base commit: `fmt.Sprintf("value=%d", 42)` = **2.00** allocations
against gc's 1.00, and the whole of the difference is the `[1]any`.

### 2.2 What splitting every payload costs

Built, measured, and rejected. With the array and every convertible payload as
separate candidates:

| case | base | split-everything | gc |
|---|---|---|---|
| `fmt.Sprintf("value=%d", 42)` | 2.00 | **1.00** | 1.00 |
| `slog.Info` 5 attributes | 1.00 / 288 B | 1.00 / 240 B | 0.00 |
| `slog.Info` 3 large integers | 1.00 / 176 B | **4.00** / 168 B | 3.00 |

The last row is the reason. `log/slog.Logger.Info` retains its `args` slice at
depth 0, so its array goes to the heap whatever the payloads do; three payloads
split out of an escaping array are three more allocations, and goc crossed from
*better than gc* (1.00) to *worse* (4.00 against 3.00). Splitting is right where
the array stays in the frame and wrong where it does not, and that is not
knowable when the front end emits the call.

### 2.3 What was built instead

The split, with somewhere to put it back.

- A payload the runtime has a **conversion helper** for (`runtime.convT16/32/64`
  — integers, bools, floats, small pointer-free types) is emitted as an
  allocation candidate of its own, by `ir.Block.HeapAllocConvertedField`, and the
  field the combined object reserved for it stays reserved.
- `opt.LowerHeapAllocations` folds it back to `container+offset` when the
  container escaped anyway, and when more than one payload escaped out of a
  container that did not — because K escaping payloads cost K allocations where
  the combined object costs one.
- Everything else stays a field of the combined object, exactly as before. A
  payload with no helper — a string, a struct, a `slog.Attr` — has no cheaper
  form to be split into, and splitting it out of a mixed call would only add an
  allocation to calls like `fmt.Sprintf("%s %d", s, n)` whose array is pinned by
  the string payload anyway.

The comparison is of **upper bounds**, deliberately. `convT64` allocates nothing
for a value below 256 and one object above it, so K split payloads may cost
nothing at all — but which values a call site sees is not something a compiler
knows, and choosing the representation on a guess about them would make the cost
of a `%d` depend on how big the number turned out to be.

### 2.4 The retention hole, and why it cannot come back

The `variadic-allocations` job promoted a merged object into the caller's frame
on a depth-0 `ParamNoEscape`, and a callee that kept `args[0]` was then holding
a pointer into a returned frame. It backed the change out to 2.00.

That hole is not avoided here by an extra rule. It is **not expressible**: the
thing the callee retains — the box — is a different allocation from the thing
that stays in the frame — the array — and the analysis gives them different
answers. `TestLowerHeapAllocationsEscapesOnlyThePayloadWhenTheCalleeRetainsAnElement`
is that case as a unit test, `variadic_element_retention.go` is it as a running
program whose retained value survives a stack copy, and
`variadic_retained_element` is it as an allocation count that would fail at 0.00.

What the design does owe is the reverse direction — everything inside an object
that escapes must escape with it — and that is `containedAllocationsEscape`, run
after the mark loop, again after the loop rule, and again after the fold-back
decision. The loop rule is the sharp one: it escapes candidates *after* the
analysis has finished, and a container it sends to the heap with a promoted
payload inside it is a frame address published into a heap object.

Containment is taken from the **declaration** — the candidate names its
container — rather than inferred from stores. Inferring it was implemented
first, and the allocation census is what rejected it: **585 sites moved from the
heap into frames**, because "a tracked pointer written into another tracked
allocation is contained, not published" is true of every `new(T)` in the tree,
and it opens a hole of its own. A pointer loaded back *out* of a container that
did not escape would then escape neither, and nothing tracks the loaded value.
The declared form is a closed set — goc emits these candidates at one site, for
one shape, and hands the container to the callee without ever reading it back —
and the load and block-copy cases escape a container's contents if anything ever
does read it back, so the closure is checked rather than assumed.

## 3. The summary machinery, and the variadic parameter it now describes

The brief points at `goc/compile.go:3532`, which says the escape summary "does
not describe a variadic parameter, whose argument is an element of a slice the
callee builds rather than the parameter itself". Both halves of that were true
and they needed opposite treatment.

### 3.1 `opt`'s fact table: the question it could already answer

`ParamFact.Deep` is the depth-1 claim — nothing reachable *through* the
parameter is retained — computed in `escapeGraph.summary` from `heapLeak` and
`resultLeak` one dereference further out. For a slice parameter, depth 1 is the
elements, so **`Deep` already is the variadic-element fact**. What was missing
was a consumer: it was read only for a *self-referential* allocation, which is
the shape the merged object happened to have.

`markSummarisedCall` now applies it generally. A `!Deep` argument escapes what
the argument *contains* — the declared payloads — without escaping the argument.
That is the summary describing a variadic parameter: "this callee may keep an
element" is now a thing a caller can act on, and acting on it costs the payload
rather than the array.

### 3.2 The front end's AST walk: the question it must refuse — and a bug

`parameterLeaksOnlyToResult` already refused the variadic parameter.
`parameterDoesNotEscape`, the predicate immediately above it, did not — and it
is handed an *argument* position by all five of its callers. For a variadic
call, argument positions from the last parameter onward name elements. Positions
past the parameter list were already refused; the position that lands exactly on
the variadic parameter was answered with the slice's summary.

**That is a live miscompile on the base commit.**

```go
func keepFirst(args ...*addressBox) { sink = args[0] }
func caller() { var local addressBox; ...; keepFirst(&local) }
```

`keepFirst` retains no pointer it was handed, so the walk answered "does not
escape" and `local` stayed in `caller`'s frame with a package-level variable
pointing at it. The collector finds it:

    runtime: pointer 0x60ccc3997d40 to unallocated span ... span.state=0
    runtime: found in object at *(0x5a0028+0x8)
    fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)

Reproduced on `19488ee` (the base) and on `4a6fd96` before it, so it is not
something this branch made. The reduction is committed as
`goc/testdata/variadic_element_address_retention.go`, with its expected output
checked against the host toolchain, and the walk now refuses the question the
way the predicate next to it always did. **No measured allocation count moves.**

A spread call, `f(xs...)`, does hand the parameter over as itself and loses
precision under the refusal. That is the price of one predicate serving both
shapes, and it is the safe direction. Answering it properly needs the argument's
ellipsis at the call, which is available at all five call sites and is a
worthwhile follow-up; it is not attempted here alongside the rest.


(filled in below as the numbers land)

## 4. The numbers

Host toolchain `go version go1.26.1 linux/arm64`, pinned in both baseline files.
Every number below was produced by running the program, under goc built from
this branch and under `go run`, not by reasoning about the generated code.

### 4.1 `goc/testdata/allocation_counts.go` — allocations per call

| call | base | **this branch** | gc |
|---|---|---|---|
| **`fmt.Sprintf("value=%d", 42)`** | 2.00 | **1.00** | **1.00** — parity |
| `fmt.Sprintf("value=%s", s)` | 2.00 | 2.00 | 2.00 — parity |
| `fmt.Sprintf("value=%v", struct)` | 2.00 | 2.00 | 2.00 — parity |
| `fmt.Sprintf("a constant format")` | 1.00 | 1.00 | 1.00 — parity |
| `f(...int)` with 2 args | 0.00 | 0.00 | 0.00 — parity |
| `f(...any)` with an int and a string | 0.00 | 0.00 | 0.00 — parity |
| **`keepElement(0x5eed)`, callee stores `args[0]` into a global** | 1.00 | **1.00** | 1.00 — parity |
| **the same with a struct payload** | 1.00 | **1.00** | 1.00 — parity |
| `fmt.Sprintf("%d/%d", 42, 42)` | 2.00 | 2.00 | 1.00 |
| `fmt.Sprintf("%d/%d", 1<<20, 1<<20)` | 2.00 | 2.00 | 3.00 — goc is cheaper |
| `fmt.Sprintf` inside a loop body | 2.00 | 2.00 | 1.00 |
| `f(...int)` inside a loop body | 1.00 | 1.00 | 0.00 |

The six `box_*` rows, the three `return_any_*` rows and `sync_pool_round_trip`
are unchanged in both columns; nothing outside the variadic rows moved.

**The headline row is at parity and the whole of it is the split.** The array is
a frame slot and the box is built by `runtime.convT64`, which for 42 returns a
pointer into `runtime.staticuint64s` and allocates nothing. gc's one allocation
and goc's are the same object: the result string.

The two `%d/%d` rows are the fold-back being visible. goc pays two allocations
for both — the result string and one combined object — where gc splits
unconditionally and pays one or three depending on the values. The row that
matters for judging the rule is the second: goc is *cheaper* than gc there, and
splitting unconditionally would have given that up to win the first.

The two `_in_loop` rows are unchanged and remain the price of
`opt.promotionsBlockedByALoop`, which is the loop-aliasing job's rule and not
this branch's to lower.

### 4.2 `log/slog` — the cases the brief named

Regenerated `goc/testdata/slog_allocations_baseline.txt`. **Thirty-one of the
thirty-two rows are byte-identical to the base commit**, allocation counts and
byte counts alike; the diff against the base is one row, discussed below.

| case | goc | gc |
|---|---|---|
| **`info/5-attr`** | **1.00 / 288 B** | **0.00** |
| **`disabled/3-attr`** | **1.00 / 176 B** | **0.00** |
| `info/3-attr-large-ints` | 1.00 / 176 B | 3.00 / 24 B — goc is cheaper |
| `control/variadic-6-preboxed` | 0.00 | 0.00 — parity |
| `control/variadic-6-literal` | 0.00 | 0.00 — parity |

**slog does not improve, and the reason is not the representation.**
`log/slog.Logger.Info`'s `args` slice escapes at **depth 0** — the summary says
`escapes`, not `noescape`+`!Deep` — through `Logger.log` → `Record.Add` →
`argsToAttr`, which returns a slice derived from `args` and assigns it back in a
loop. The array therefore goes to the heap however the payloads are represented,
and the one allocation slog pays is that array with every payload folded into
it. Improving it means making that chain's summary answer `noescape`, which is a
summary problem in `Record.Add`'s loop-carried leak-to-result and not something
the variadic call site can fix. The intermediate build that split
unconditionally measured `info/3-attr-large-ints` at **4.00 against gc's 3.00**,
which is what the fold-back exists to prevent.

One row is not identical: **`json/kv-4-pairs` used to kill the program under goc
and now runs**, at 5.00 allocations / 320 B against gc's 2.00 / 24 B. That is
**not a fix**. The two reductions the slog job committed
(`goc/testdata/slog_allocations/miscompiles/attr_bad_pointer.go` and
`attr_bad_pointer_stackcopy.go`) still reproduce at this HEAD, byte for byte —
`bad pointer in frame main_main at 0x…: 0xc8`. The frame pointer map is still
wrong for a `slog.Attr`; this branch moved the objects around it enough that
this particular program stops meeting a collection with one live in a frame.
`json/logattrs-4-attrs` still crashes. The JSON output goc now produces for that
shape is byte-identical to the host toolchain's, checked separately, so the row
is a real measurement and not a program that quietly stopped working.

### 4.3 The gc differential

Regenerated `goc/testdata/escape_gc_differential.txt`, and verified to reproduce
itself: two `-update` runs produce byte-identical files, and the test passes
against the committed one without `-update`.

|  | base | this branch |
|---|---|---|
| **PESSIMISTIC** (goc heaps, gc does not) | 567 lines | **573** |
| PERMISSIVE (gc heaps, goc does not) | 209 lines | **233** |
| confusion matrix, goc heap × gc frame | 183 | **172** |
| confusion matrix, goc mixed × gc mixed | 8 | **30** |
| joined source lines | 3 059 | 3 070 |

**The permissive set grows by 25 lines, and this is the direction that has to be
argued rather than counted.** Five are in files this branch adds or edits. The
other **20 are all one shape, with no exceptions** — checked by pattern over the
file, not by sampling:

    fmt_sprintf.go:6   mixed -> mixed
        src  formatted := fmt.Sprintf("value=%d", 42)
        goc  col 27  frame  runtime.newobject  struct_values__1_any__payload0_int
        goc  col 39  heap   runtime.convT64    int
        gc   col 26  frame  slice   ... argument
        gc   col 39  heap   object  42

goc frames the `[N]any` at the call's column and heaps the box at the argument's
column. **So does gc, at the same two columns.** These lines are goc's verdict
moving from `heap` to `mixed` and landing on gc's, which is why `mixed × mixed`
goes from 8 to 30 while `heap × frame` falls from 183 to 172. Every one of the 20
has a goc `frame … struct_values__*` decision paired with a gc `frame slice …
argument` decision; the classifier calls it permissive because a line-level rule
cannot tell one decision on a line from the other one.

The pessimistic set grows by 15 and loses 9. Fourteen of the 15 are in this
branch's own files; the fifteenth is `variadic_backing.go:8`, which is section
3.2's refusal — the same site the census reports, seen from the other
instrument.

**The pessimism headline does not shrink, and that is a property of the
instrument rather than a disappointment.** `opt.conversionHelpers` records a
`convT` site as *heap*, because the payload did leave the frame, which is the
escape decision gc's `-m` reports at the same line — whether or not the value it
is handed makes the helper allocate. A `fmt.Sprintf("%d", n)` line has a
`convT64` on it either way, so the line stays in the pessimistic set while its
measured cost halves. The differential measures placement; `allocation_counts.go`
measures cost. This is the same disagreement the `iface-convt-fastpath` branch
reported for the same reason, and it is why both instruments are kept.

## 5. The guards

### 5.1 `TestFrameEscapeAudit` — clean

`ok github.com/evanphx/cg12/goc`. Zero new frame-address publications and none
of the listed ones went away, at every intermediate state of this branch as well
as the final one. The brief warned that a failure here would be a real
correctness finding rather than a baseline to update; it did not fail, and the
one place where it *would* have is written into the code:
`promotionsBlockedByALoop` escapes candidates after the analysis has finished,
so `containedAllocationsEscape` runs again after it. A container the loop rule
sends to the heap with a promoted payload still inside it is exactly the
publication this audit exists to catch.

### 5.2 The allocation census — regenerated and read site by site

`goc/testdata/alloc_census_baseline.txt`. Three groups, and the whole diff is
one class.

**128 sites moved heap → frame, and every one is a `struct_values__*`** — the
synthesized combined type a variadic call allocates. Nothing else in the corpus,
the stdlib or the runtime moved in that direction: not one `new(T)`, map, slice
backing or closure. **127 of the 128 pair with a new `runtime.convT*` site on
the same source line and in the same function**, which is the split made
visible: the array went into the frame and the payload it used to contain became
a conversion call a few columns to the right. The 128th is
`runtime_range_target_order.go:88:15`, whose payload site is positionless and so
cannot join on location; it is there, under `?`.

**22 sites vanished, all in `testdata/allocation_counts.go`** — the corpus
program this branch adds four cases to, so every line number below the insertion
moved. No site vanished anywhere else.

**174 sites appeared.** 127 of them are the split payloads paired above. 43 are
in the three files this branch adds or edits. **Four are neither, and they are
the price of section 3.2's refusal**, in the frame → heap direction:

    net/http.http2Transport.newClientConn   h2_bundle.go:8143:21  [2]http2Setting
    main.leaky (and two inlined copies)     variadic_backing.go:8:2  int

Both are a value passed as a variadic *element* to a callee that keeps nothing,
which the AST walk could prove before and now declines to answer. Two distinct
source lines across 385 corpus programs and the whole vendored stdlib is what
the refusal costs, against a `fatal error: found bad pointer in Go heap` it
buys. Recovering the precision needs a walk that can ask about dereference
depth, which `opt`'s fact table has and the AST walk does not.

### 5.3 Loop aliasing — the programs still match the host

`TestLoopBodyAllocationsAreDistinctPerIteration` and
`TestLoopAliasExpectationsMatchTheHostToolchain` pass, over the existing five
programs and the two this branch adds. `variadic_backing.go` still prints `1`
even though it now costs an allocation it did not, which is the point of having
both instruments: the census saw the cost and the program proves the answer.

`TestLoopVarPerIteration`, `TestAllocationCounts`,
`TestAllocationCountsAgainstTheHostToolchain` and the `opt` and `ir` unit suites
all pass. `go test ./goc/...` in full, the capability matrix and `make test-unit`
were deliberately not run: a dependent job does that.

`TestEscapeShadowPlacement` gains **one line**, and it is the same site 5.2's
last paragraph is about:

    net/http.http2Transport.newClientConn  slice-literal-backing  heap -> frame

The AST walk now places that `[2]http2Setting` on the heap and the IR analysis
would keep it in a frame. That is the "front end heap, IR frame" direction — a
latent pessimisation in the walk, deliberately introduced, with the object on
the heap either way, and the same class already has lines in the baseline from
`httpcommon.go` and `servemux121.go`. Nothing moved in the other direction.

### 5.4 Determinism

`TestDeterminism` passes. Two places in this branch could have broken it and are
written not to:

- `foldSplitPayloadsBackIn` visits containers **in first-appearance order**, not
  in map order, so which container is folded first cannot depend on hash
  iteration.
- The front end already recorded payload fields in argument order rather than in
  a map, for the same reason, and the split respects that ordering: a payload
  that becomes its own allocation still consumes its reserved field in the order
  the argument list gives.

### 5.6 The verification at HEAD

Every instrument re-run against the final commit, after the branch's temporary
diagnostic was deleted and every baseline regenerated:

```
$ go test ./goc -run 'TestFrameEscapeAudit|TestAllocationCensus|TestLoopBodyAllocations|
                      TestLoopAlias|TestAllocationCounts|TestDeterminism|TestLoopVar|
                      TestEscapeShadowPlacement'
ok  github.com/evanphx/cg12/goc  217.448s

$ go test ./opt ./ir ./lower
ok  github.com/evanphx/cg12/opt   ok github.com/evanphx/cg12/ir   ok github.com/evanphx/cg12/lower

$ go test ./goc -run TestSlogAllocationsAgainstGC   -slog-allocations
ok  github.com/evanphx/cg12/goc   17.818s

$ go test ./goc -run TestEscapeDifferentialAgainstGC -escape-gc-differential
ok  github.com/evanphx/cg12/goc   10.611s
```

Both opt-in baselines re-derive their own committed output without `-update`,
and the differential is byte-identical across two independent `-update` runs.

## 6. What was committed

| file | what |
|---|---|
| `ir/build.go` | `Block.HeapAllocConvertedField`, and the contract it adds to `HeapAllocConverted`'s two |
| `goc/compile.go` | `variadicPayloadStorage` and the slot it hands to the interface conversion; `parameterDoesNotEscape`'s refusal |
| `opt/escape.go` | declared containment, `containedAllocationsEscape`, the read-out cases, `foldSplitPayloadsBackIn`, the fold in the rewrite |
| `opt/escapesummary.go` | `markContents`: a `!Deep` argument escapes what it holds, not itself |
| `opt/escape_test.go` | five unit tests over the shape, including the retention case and the read-out |
| `goc/testdata/variadic_element_retention.go` | the retained element, surviving a stack copy |
| `goc/testdata/variadic_element_address_retention.go` | the front-end reduction: a frame address the collector rejects |
| `goc/testdata/allocation_counts.go`, `goc/alloccount_test.go` | four new rows and the headline's new number |
| `goc/loopalias_test.go` | the two new programs, expectations checked against the host |
| `goc/testdata/alloc_census_baseline.txt`, `escape_gc_differential.txt`, `slog_allocations_baseline.txt` | regenerated |

## 7. What is left

1. **`log/slog` is still 1.00 against gc's 0.00**, and the reason is
   `Logger.Info`'s `args` slice escaping at depth 0 through `Record.Add`'s
   loop-carried `argsToAttr` leak-to-result. That is a summary problem in
   `opt.escapeGraph`, not a representation one, and it is the next thing worth
   doing for slog. It would turn `disabled/3-attr` free.

2. **The front end's refusal costs precision on a spread call**, `f(xs...)`,
   where handing the parameter over as itself does make the parameter's own
   summary the right question. All five callers of `parameterDoesNotEscape` have
   the `*ast.CallExpr` in scope, so passing `call.Ellipsis.IsValid()` would
   recover it. Two census sites is what it is worth today.

3. **`fmt.Sprintf("%d/%d", small, small)` is 2.00 against gc's 1.00**, because
   the fold-back compares upper bounds and two convertible payloads out of a
   framed array might cost two. A profile-free improvement would need to know
   the values; a profile-guided one would not.

4. **The gc differential's pessimism headline cannot see this change.** A
   `convT` site is recorded as heap because the payload left the frame, so a
   `fmt.Sprintf("%d", n)` line stays pessimistic while its cost halves. Splitting
   the record into "left the frame" and "called an allocator" would let the file
   say which of the 573 lines cost anything, and 552 of them are conversion
   sites.

5. **The `slog.Attr` frame pointer map is still wrong.** Both reductions still
   reproduce at this HEAD and `json/logattrs-4-attrs` still dies. `json/kv-4-pairs`
   running again is a layout coincidence, not a fix, and it will stop being one.

## 8. The answer

**`fmt.Sprintf("value=%d", 42)` costs goc 1.00 allocations against gc's 1.00.**
It was 2.00. The `[1]any` is a frame slot, which is where gc has always kept it,
and the one allocation both compilers pay is the result string.

**`log/slog` is unchanged: `info/5-attr` 1.00 against gc's 0.00,
`disabled/3-attr` 1.00 against gc's 0.00.** Thirty-one of the thirty-two slog rows are byte-identical
to the base commit. That is a result rather than a non-result: the build that
split unconditionally measured `info/3-attr-large-ints` at 4.00 against gc's 3.00,
and the fold-back is what holds slog level while `fmt` improves. slog's array
escapes at depth 0 and no variadic representation can help it.

**The combined object was split, partially and by construction.** A payload the
runtime has a conversion helper for is its own allocation candidate; a payload
without one stays a field. `opt` folds a split payload back to `container+offset`
whenever the container escaped anyway or more than one payload escaped out of a
container that did not, so the representation is chosen where the escape answer
is known and never where it is guessed.

**The retention hole is not avoided, it is unrepresentable.** The thing the
callee keeps — the box — is a different allocation from the thing that stays in
the frame — the array — so "the callee retains an element" and "the callee
retains the slice" are two answers rather than one, and `ParamFact.Deep` is what
tells them apart. What the split owes back is that everything inside an escaping
object escapes with it, which `containedAllocationsEscape` settles after the mark
loop, after the loop rule, and after the fold-back — the loop rule being the one
that would otherwise leave a promoted payload inside a heap container.

**One thing found along the way is worth more than the numbers.** The front end's
`parameterDoesNotEscape` was answering about the *slice* for an argument that is
an *element* of it, so a local whose address a variadic callee retained was left
in a frame that returned. The collector says `found bad pointer in Go heap`. It
reproduces on the base commit and on `main`, the reduction is committed with its
expected output checked against the host toolchain, and the walk now refuses the
question the predicate beside it always refused. It costs two census sites.
### 5.5 Corpus output differential — mine, not one the brief asked for

All **392 corpus programs** compiled, linked and run by goc built from the base
commit and by goc built from this branch, `-O` both times, stdout and stderr
compared byte for byte. **388 identical.** The four that differ:

| program | difference |
|---|---|
| `allocation_counts.go` | the program this branch adds four cases to; the diff **is** the table in 4.1 |
| `variadic_element_address_retention.go` | **dies under the base compiler and runs under this branch** — section 3.2's fix, from the outside |
| `bytes_grow_compare.go` | not a difference: identical on a serial re-run. The parallel harness raced on the linked binary's name |
| `bytes_grow_stats.go` | prints raw `runtime.MemStats`. **Not deterministic under either compiler**: three runs of the *base* give `mallocs 220`, `226`, `220`, and three of this branch give the same three values |

So: zero unexplained output differences, and the one real one is the bug being
fixed. This is the instrument the `variadic-allocations` job's dangling pointer
did *not* show up in — 387 of 390 were identical then too — which is why it is
run alongside the census and the audit rather than instead of them.



`disabled/3-attr`: **goc 3.00 allocations/op, gc 0.00**. `info/5-attr`: **goc
9.00 allocations/op, gc 0.00**. Of the three designs `log/slog` bets on the
compiler for, the inline `[5]Attr` still spills in the right place and buys
nothing else, the packed `Value` buys nothing at all, and the disabled-level
early return saves nothing because everything it was meant to save has already
been paid at the call site. Essentially none of slog's designed allocation
avoidance survives compilation by goc — and on the one path that exercises a
real handler, the compiled program does not survive either.


---

# Part 2 — A `slog.Attr` in a frame is scanned as a pointer: the mis-classification, found and fixed

Job `ccwork/slog-attr-gcmask`, branched off `main` `4a6fd96`. The subject is the
miscompile RUNTIME_PLAN §26 left open and CCWORK_REPORT §5a reported without
fixing: a `slog.Attr` live in a frame across a collection dies with

    runtime: bad pointer in frame main_main at 0x...: 0xc8
    fatal error: invalid pointer found on stack

## 0. Reproduced on main before anything was changed

    go run ./cmd/goc -run goc/testdata/slog_allocations/miscompiles/attr_bad_pointer.go
    runtime: bad pointer in frame main_main at 0x31b432e07d50: 0xc8
    fatal error: invalid pointer found on stack
    runtime_adjustpointers <- runtime_adjustframe <- runtime_copystack
      <- runtime_shrinkstack <- runtime_scanstack <- markroot <- gcDrain

0xc8 is 200, the integer `slog.Int("k", 200)` packs into `Value.num`. Note the
walker in this trace: `shrinkstack` inside `scanstack`, so the collector's own
stack scan reached it through the copier.

## 1. The reduction landed as a corpus test, failing (commit below)

The programs in `goc/testdata/`, run by `goc/slogattrframe_test.go`
unoptimized and optimized, plus a run of each under
`GODEBUG=cg12checkstackcopy=1`, plus a check that every expectation is `go run`'s
own output rather than a belief about it. On `main`'s compiler, before any
change:

    --- FAIL: TestSlogAttrInFrameIsNotScannedAsAPointer (52.10s)
        --- FAIL: .../slog_attr_frame_gcmask.go              (7.98s)
        --- FAIL: .../slog_attr_frame_gcmask.go -O           (7.89s)
        --- FAIL: .../slog_attr_frame_gcmask_stackcopy.go    (7.49s)
        --- FAIL: .../slog_attr_frame_gcmask_stackcopy.go -O (7.99s)
        --- FAIL: .../slog_attr_frame_gcmask_kinds.go        (7.54s)
        --- FAIL: .../slog_attr_frame_gcmask_kinds.go -O     (7.92s)
        --- PASS: .../slog_attr_frame_gcmask_control.go      (2.55s)
        --- PASS: .../slog_attr_frame_gcmask_control.go -O   (2.74s)
    --- FAIL: TestSlogAttrInFrameSurvivesTheStackCopyChecker (24.95s)
        --- FAIL: .../slog_attr_frame_gcmask.go              (7.46s)
        --- FAIL: .../slog_attr_frame_gcmask_stackcopy.go    (7.50s)
        --- FAIL: .../slog_attr_frame_gcmask_kinds.go        (7.49s)
        --- PASS: .../slog_attr_frame_gcmask_control.go      (2.50s)
    --- PASS: TestSlogAttrFrameExpectationsMatchTheHostToolchain (0.33s)

Every failure is `run: runtime: bad pointer in frame main_main at 0x...: 0xc8`.
The `_kinds` program holds an Int64, a Bool, a Duration and a Float64 in one
frame at once, because `num` carries all of them and a map that claims that word
claims it for every one.

A fifth program, `slog_attr_frame_gcmask_shape.go`, was added later -- it is
section 2.1's finding, the same shape with no `log/slog` import -- and was
checked against main's compiler the same way, by building it with `goc/compile.go`
reverted to `4a6fd96`:

    runtime: bad pointer in frame main_main at 0x7c2da5013d50: 0xc8

## 2. Where the mis-classification is

Not in the type descriptor, and not in the stack-map machinery. It is one line
of the front end's translation of a Go type into the **Go-ABI aggregate**
(`ir.AggType`) that the frame's pointer map is built from.

`goc/compile.go`'s `goABIAggregate` turns an array type into one field carrying
the element's shape and the array's length:

    case *types.Array:
        field, ok := g.goABIField(value.Elem())
        ...
        field.Count = int(value.Len())

`ir.Field.Count` cannot express zero. `Count` is 0 for an ordinary scalar field
too -- every `ir.Field{Sub: ir.SubL}` literal in the tree leaves it unset -- so
`ir.Field.count()` reads 0 as **one element**:

    func (f Field) count() int {
        if f.Count > 1 { return f.Count }
        return 1
    }

A `[0]func()` therefore becomes one pointer-shaped element of eight bytes. Every
consumer of the aggregate then agrees on the wrong answer. Measured directly on
the shape (`ir` unit probe, since removed):

    [0]func()   size=8  align=8                    Go says 0/8
    slog.Value  size=32 pointer offsets [0 16 24]  Go says 24, [8 16]
    slog.Attr   size=48 pointer offsets [0 16 32 40]  Go says 40, [0 24 32]

Offset 16 of `slog.Attr` is `Value.num`. The phantom element sits at the offset
of the field that follows it and shifts every later field by a word.

Confirmed end-to-end in the compiled program, with a temporary dump of each
frame's marked words (`arm64/mc.go`'s `goLocalPointerWords`, instrumentation
since removed), compiling the reduction:

    framemap main.main: localwords=[... 22 ...]
      alloc t41 at frame+176 marks=[0 16 32 40]

`marks` are byte offsets within the attribute's frame slot and are exactly
`ir.AggregatePointerOffsets` of the phantom aggregate. Frame+176+16 is local
word 22, and that word holds 200.

The heap path is **not** affected: `walkPointerWords`, which builds the
`abi.Type` GCData mask, iterates `for index := 0; index < value.Len()` and so
emits nothing for a zero-length array, and it takes struct offsets from
`go/types`. The two descriptions of the same type disagreed, and only the frame
one was wrong.

## 2.1 The control was not a control

The reduction's control -- `slog.Value`'s shape hand-written in the program's
own package -- passes on main, and the earlier report read that as evidence that
the shape is not the trigger. It is not evidence. Its frame map is **identical**:

    framemap main.main (control): alloc t41 at frame+176 marks=[0 16 32 40]

and the word it claims holds 200 there too. It survives only because nothing in
that program copies `main`'s stack while the attribute is live: the mark phase
tolerates 200 (`findObject` on an address that was never heap returns silently),
and only `adjustpointers` throws on it. Replacing its `runtime.GC()` with the
same deep recursion the stack-copy reduction uses makes it fail identically:

    runtime: bad pointer in frame main_main at 0x42c9ee193d50: 0xc8

with no `log/slog` import anywhere in the program. So the trigger is the shape
after all -- a zero-length array field ahead of a scalar -- and log/slog is
where it appears in ordinary code.

## 3. The fix

`goc/compile.go`, `goABIAggregate`, one branch:

    case *types.Array:
        if value.Len() == 0 {
            // ... contributing no field is both the correct layout and the
            // only one Count can express.
            break
        }
        field, ok := g.goABIField(value.Elem())
        ...

A zero-length array contributes no field to the Go-ABI aggregate. The nested
aggregate it produces is empty, so it lays out as zero bytes at the alignment
`typeAlign` gives it, which is what Go says, and it contributes no part to
`FlattenAggregate` and no offset to `AggregatePointerOffsets`.

The alternative -- teaching `ir.Field` to distinguish "zero elements" from
"unset" -- would have to touch every `ir.Field` literal in the tree, since a
scalar field leaves `Count` at 0 and means one. The representation cannot say
zero, so the producer must not ask it to.

Also fixed, because the first change exposed it: gc gives a struct whose last
field is zero-sized one byte of padding so that a pointer to that field is not
past the end of the object, and `typeSize` implements that rule. The phantom
element used to supply that byte by accident. `goABIAggregate` now appends an
explicit byte-wide scalar instead -- never a pointer. That case was **already
wrong on main** for a trailing empty struct, which never had a phantom element:
`struct{ n uint64; _ struct{} }` laid out as 8 bytes where the type is 16.

## 3.1 The guard: the two descriptions of a type, checked against each other

`goc/goabi_layout_test.go`. A Go type is described to the collector twice --
`walkPointerWords` builds the heap mask from `go/types` offsets, and
`goABIAggregate` builds the aggregate the frame's map and the frame's slot size
come from -- by two pieces of code that share nothing. The test asserts they
agree on size, alignment and pointer words, over shapes with a zero-sized field
in every position and over every named type in `log/slog`.

On main it fails, and what it prints is the defect:

    Value:  aggregate 32 bytes, pointer offsets [0 16 24], type: 24, [8 16]
    Attr:   aggregate 48 bytes, pointer offsets [0 16 32 40], type: 40, [0 24 32]
    Record: aggregate 328 bytes, 23 pointer words, type: 288, 18 words
    Alone/Empty/Leading/Middle/Trailing/EmptyArrayOfStruct/TrailingEmptyStruct

With the fix, both tests pass.

## 4. The reduction after the fix, through both walkers

    --- PASS: TestSlogAttrInFrameIsNotScannedAsAPointer (63.72s)   10/10 subtests
    --- PASS: TestSlogAttrInFrameSurvivesTheStackCopyChecker (29.06s) 5/5
    --- PASS: TestSlogAttrFrameExpectationsMatchTheHostToolchain (0.57s) 5/5

That is every program unoptimized and optimized, every program again under
`GODEBUG=cg12checkstackcopy=1` (which validates each word the map claims as the
stack is copied rather than waiting for one to look like an address), and the
collector reached through `runtime.GC()` in three of them.

## 5. The slog baseline: the two JSON rows

Regenerated with

    go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations \
        -update-slog-allocations

and then re-run without the update flag, which passes, so the file reproduces.

**Attribution first, because the diff against the committed file is misleading.**
The committed baseline predates the `ccwork/variadic-allocations` merge that is
already on `main`, so a plain diff shows twelve rows improving that this work did
not touch. Regenerating on pristine `main` (`4a6fd96`, in a separate worktree)
produces those same improved numbers with the JSON rows still `crash`. Against
*that* -- main's compiler measured today -- the diff from this fix is exactly:

    -json/kv-4-pairs                     crash       2.00      crash       24.0
    -json/logattrs-4-attrs               crash       0.00      crash        0.0
    -
    -cases that did not run:
    -
    -  json/kv-4-pairs        goc  fatal error: invalid pointer found on stack
    -                              (bad pointer in frame log_slog_handleState_appendAttr: 0xc8)
    -  json/logattrs-4-attrs  goc  fatal error: invalid pointer found on stack
    -                              (bad pointer in frame main_func_311_28: 0x3)
    +json/kv-4-pairs                      8.00       2.00      344.0       24.0
    +json/logattrs-4-attrs                8.00       0.00      264.0        0.0

Nothing else moves. **The numbers that appear where `crash` was:**

| case | goc a/op | gc a/op | goc B/op | gc B/op |
| --- | ---: | ---: | ---: | ---: |
| `json/kv-4-pairs` | **8.00** | 2.00 | **344.0** | 24.0 |
| `json/logattrs-4-attrs` | **8.00** | 0.00 | **264.0** | 0.0 |

The section listing cases that did not run is now empty and gone: every case in
the table runs.

For scale, the earlier report recorded one whole-program observation of
`json/kv-4-pairs` at 15.00 allocations / 456.0 bytes before the process died on
the next case. The committed one-case-per-process harness now measures it at
8.00 / 344.0.


## 6. How wide the defect was

`log/slog` is not the only user of the shape. Every standard-library type that
puts a zero-length array field in a struct uses a **pointer-shaped** element, so
every one of them claimed a pointer word over whatever followed it. Measured on
`main` by the same guard:

| type | goc's aggregate | the type |
| --- | --- | --- |
| `log/slog.Value` | 32 bytes, pointers `[0 16 24]` | 24, `[8 16]` |
| `log/slog.Attr` | 48 bytes, pointers `[0 16 32 40]` | 40, `[0 24 32]` |
| `log/slog.Record` | 328 bytes, 23 pointer words | 288, 18 |
| `sync/atomic.Pointer[T]` | 16 bytes, pointers `[0 8]` | 8, `[0]` |
| `weak.Pointer[T]` | 16 bytes, pointers `[0 8]` | 8, `[0]` |
| `runtime.PanicNilError` | 8 bytes, pointers `[0]` | 0, none |

`slog.Value` is the one that killed programs, because the field after its
`[0]func()` is the `uint64` carrying a number small enough for
`runtime.adjustpointers` to reject. `atomic.Pointer` and `weak.Pointer` claimed
a word one *past* the value, which is a frame word belonging to something else:
whatever it holds is walked as an address, and it is only luck that no corpus
program has died of it.

With the fix, the invariant holds for every named type in all four packages,
`runtime` included whole.

## 7. Guards

### 7.1 The loop-aliasing programs still match the host toolchain

    --- PASS: TestLoopBodyAllocationsAreDistinctPerIteration (22.83s)  8/8
    --- PASS: TestLoopAliasExpectationsMatchTheHostToolchain (0.59s)   4/4

### 7.2 `TestFrameEscapeAudit` — clean

    --- PASS: TestFrameEscapeAudit (0.00s)

Zero new publications: no frame address the compiler emits reaches a global, a
heap object, a result area or anything through a parameter that it did not
already reach on `main`. Expected -- this change describes existing storage
differently, it does not move any object -- and checked rather than assumed.

### 7.3 The allocation census delta: empty

    --- PASS: TestAllocationCensus (187.10s)
    --- PASS: TestEscapeShadowPlacement (0.00s)

The census compares site by site in four directions -- heap→frame, frame→heap,
appeared, vanished -- and all four are empty, over a corpus that now has five
more programs in it. So there is no delta to review: no allocation moved, no
site appeared, none vanished.

That five new corpus programs add no census line is worth one sentence rather
than suspicion. The census records heap allocations and the frame allocations
that came out of an escape decision, keyed by site identity and deduplicated
across the corpus; the new programs' own `main`s allocate nothing, and every
line of `log/slog` and `runtime` they reach was already reached by
`stdlib_slog_structured.go`.

Regenerated anyway, literally, because the brief asks for the file and not for
an argument about it:

    go test ./goc -run TestAllocationCensus$ -update-alloc-census-baseline
    ok  github.com/evanphx/cg12/goc  275.989s

`git status` reports `goc/testdata/alloc_census_baseline.txt` unmodified. The
regenerated file is byte-identical to the committed one, so there is no site to
review and nothing unexplained.

Two GC-path smoke tests, since this change touches every aggregate the compiler
builds and `sync/atomic.Pointer` is in the runtime's own code:

    goc/testdata/runtime_gc_type_mask_padding.go  -> "type mask padding ok"
    goc/testdata/runtime_stack_copy_roots.go      -> "stack copy roots ok"
      (the second under GODEBUG=cg12checkstackcopy=1)

## 8. The same collision in the C front end, measured and deliberately not fixed

`cc/agg.go`'s `fieldOf` builds the same kind of field from a C array:
`ir.Field{Sub: subOfType(elem), Count: int(at.Len())}`. A GNU zero-length array
member hits the identical `Count == 0` collision. It does **not** produce a bad
map, and it is not silent:

    struct s { long n; char buf[0]; };
    long g(struct s v);

    cc: cannot pass struct.s by value: cg12 lays it out as 16 bytes (align 8)
    but C says 8 (align 8) -- a bitfield, which cg12's aggregate types cannot
    yet express

`checkAggLayout` compares the aggregate against C's own size and refuses. C
frames are not managed, so no stack map is built from these aggregates and there
is no word to mis-classify; the consequence is a rejected program with a
misleading diagnostic ("a bitfield").

Not fixed here, for three reasons that are a scope judgement and not a
measurement: it is loud rather than silent, C's rule for a trailing zero-sized
member is *not* Go's (C says 8 where gc says 16, so the same padding would be
wrong there), and `fieldOf` returns one field per member so skipping one means
changing its caller. It is one line plus its caller for whoever wants it, and
the error message it produces is the wrong one for the cause.

## 9. What was committed

| commit | what |
| --- | --- |
| `240d720` | the reduction as five corpus programs and `goc/slogattrframe_test.go`, failing on main |
| `c613b4c` | the fix in `goABIAggregate`, the trailing-zero-size padding, and the aggregate/type agreement test |
| `9a76dd3` | the regenerated slog baseline, RUNTIME_PLAN §28, the corrected miscompiles README |
| `9e0f9a3` | the agreement test extended to `sync/atomic`, `weak` and `runtime` |

The compiler change is 20 lines in one function plus a 15-line helper next to the
size rule it mirrors. Everything else is tests, baselines and prose.

### 7.4 Determinism

    scripts/determinism-check.sh -corpus -rounds 3 -j 24
    programs=395 rounds=3 workers=24 optimize=false
    round 0: 395 programs in 120.7s, 0 failed
    round 1: 395 programs in 121.7s, 0 failed
    round 2: 395 programs in 125.7s, 0 failed
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    reproducible=395 varying=0 failed=0 of 395 over 3 rounds

and again optimized, which is the arm where the placement passes run:

    scripts/determinism-check.sh -corpus -rounds 2 -j 24 -O
    programs=395 rounds=2 workers=24 optimize=true
    round 0: 395 programs in 126.0s, 0 failed
    round 1: 395 programs in 132.4s, 0 failed
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    reproducible=395 varying=0 failed=0 of 395 over 2 rounds

Same depth as section 26's: 3 rounds without `-O`, 2 with.

## 10. The answer

**Where the mis-classification was.** Not in the type descriptor's pointer mask,
which was right, and not in how the stack map is built from what it is given.
It was one line earlier than either: `goABIAggregate` in `goc/compile.go`
translating a Go array type into an `ir.Field` with `Count = int(value.Len())`.
`ir.Field.Count` has no way to say zero — it is 0 for every ordinary scalar
field too, and `ir.Field.count()` reads that as one element — so a `[0]func()`
became one **pointer-shaped element of eight bytes**, sitting at the offset of
the field after it. In `slog.Value` that field is the `uint64` `log/slog` packs
int64, uint64, bool, `time.Duration` and float64 into, so the frame's pointer
map claimed the integer the attribute was carrying, and both the collector's
stack walk and the stack copier walked 200 as an address. It was never a
`log/slog` bug and never a JSON-handler bug: `sync/atomic.Pointer[T]`,
`weak.Pointer[T]` and `runtime.PanicNilError` had the same phantom word.

A zero-length array now contributes no field, which is the correct layout and
the only one `Count` can express, and the gc padding rule for a struct ending in
a zero-sized field — which the phantom used to supply by accident, and which was
already wrong on `main` for a trailing empty struct — is applied explicitly as a
byte-wide scalar.

**The slog JSON rows.** Both were `crash`. They now run:

    json/kv-4-pairs         8.00 a/op, 344.0 B/op   (gc 2.00, 24.0)
    json/logattrs-4-attrs   8.00 a/op, 264.0 B/op   (gc 0.00,  0.0)

and `goc/testdata/slog_allocations_baseline.txt` has no "cases that did not run"
section any more.

## 11. Verification at the committed HEAD

Everything committed, working tree clean, no update flags:

    go test ./goc -run 'TestSlogAttr|TestGoABIAggregate'
    --- PASS: TestGoABIAggregateAgreesWithTheTypeLayout (0.00s)
    --- PASS: TestGoABIAggregatesAgreeWithTheirTypesInTheStdlib (2.07s)
    --- PASS: TestSlogAttrInFrameIsNotScannedAsAPointer (58.14s)
    --- PASS: TestSlogAttrInFrameSurvivesTheStackCopyChecker (27.78s)
    --- PASS: TestSlogAttrFrameExpectationsMatchTheHostToolchain (0.39s)
    ok  github.com/evanphx/cg12/goc  88.441s

and the two reductions this started from, run straight from the miscompiles
directory under the fixed compiler:

    goc -run .../attr_bad_pointer.go            -> 200
    goc -run .../attr_bad_pointer_stackcopy.go  -> 200
    goc -run .../attr_bad_pointer_control.go    -> 200

`go test ./goc/...`, the capability matrix and `make test-unit` were not run
here: a dependent job runs them, and running them twice on a loaded box helps
nobody. What that leaves unchecked from this tree is stated plainly rather than
implied — the corpus-wide run, the matrix, and the unit suites are that job's
result, not this one's.

Host toolchain: go1.26.1 linux/arm64.

---

# Part 3 — The package-initializer dispatch miscompile: what the registration walk missed

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

