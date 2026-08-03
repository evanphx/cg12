# Wave 3 — integration gate report

`integration/wave3` = `main` (6b9fbb0) + three changes, merged in the order
given, plus one commit regenerating the baselines the merge could not resolve.

Host toolchain `go1.26.1 linux/arm64`, 64 cores. Every number below was
produced by a process this job started and watched exit; anything not watched
to completion is marked UNVERIFIED and named as such.

    main 6b9fbb0
      ├── ccwork/gc-stackscan-gogc10             (change 1)  fast-forward
      ├── ccwork/dead-slots-and-census-direction (change 2)  merge a4ec2cf
      └── ccwork/slog-residual-allocation        (change 3)  merge e2aa32c
                                                 baselines   e67d38a

All three branches carry a real compiler change; none ended empty.

  - change 1 — `arm64/regalloc.go`, `arm64/mc.go`: a call's result home is no
    longer described as a GC root at that call.
  - change 2 — `goc/compile.go`, `ir/alloc.go`, `opt/alloccensus.go`: dead frame
    slots are not emitted, and the census names the direction of a placement.
  - change 3 — `goc/compile.go`, `opt/escapegraph.go`, `opt/escapesummary.go`:
    a constant boxed into an interface without allocating, plus four escape
    precision defects.

## Merge conflicts and how they were resolved

    CCWORK_REPORT.md                        text conflict x2  — both sides kept,
                                                                doc only
    goc/testdata/alloc_census_baseline.txt  text conflict     — REGENERATED
    goc/testdata/escape_gc_differential.txt auto-merged       — REGENERATED
    goc/testdata/escape_shadow_baseline.txt auto-merged       — regenerated,
                                                                byte-identical
    goc/testdata/slog_allocations_baseline.txt auto-merged    — regenerated,
                                                                byte-identical
    goc/testdata/frame_escape_baseline.txt  auto-merged       — see item 4
    goc/compile.go                          auto-merged clean (both sides edit it)

`go build ./...` and `go vet ./goc/... ./opt/... ./ir/... ./arm64/...` are clean
on the merged tree.

## Root build artefacts: CLEAN

`git ls-files` at the repo root on `integration/wave3` is 14 entries. The only
non-source entries are the four pre-existing ones named in the brief — `cg12`
(5.1 MB ELF), `viz` (7.2 MB ELF), `cs.trace`, `RUNTIME_PLAN.md.orig` — all
present on `main` at 6b9fbb0 and left alone. **No new artefact was introduced by
any of the three branches.** Nothing was removed.

---

## Baseline regeneration

All four were regenerated from the merged tree with the command in their own
header, after the merge and before any test run.

| baseline | regenerated | vs. the merge result |
|---|---|---|
| `alloc_census_baseline.txt` | `go test ./goc -run TestAllocationCensus -update-alloc-census-baseline` (181 s) | **differs** — 3 598 lines |
| `escape_gc_differential.txt` | `go test ./goc -run TestEscapeDifferentialAgainstGC -escape-gc-differential -update-escape-gc-differential` (11 s) | **differs** — 3 984 lines |
| `escape_shadow_baseline.txt` | `go test ./goc -run TestEscapeShadowPlacement -update-escape-shadow-baseline` (181 s) | byte-identical |
| `slog_allocations_baseline.txt` | `go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations -update-slog-allocations` (20 s) | byte-identical |

Only change 3 touched the shadow and slog baselines, so the merge result was
that branch's version and regeneration confirms it is still correct in the
merged tree. The census and the differential are the two that no side had right.

### The census, site by site

Data lines (header excluded):

    main       14 846
    change 1   14 849   (+3, -0)      its new reducer's three heap sites
    change 2   17 822   (+2 976, -0)  front-end frame placements now recorded
    change 3   11 551   (+149, -3 444)

Composing the three deltas onto `main` predicts **14 530** lines. The merged
tree actually produces **14 533**. The residue is 5 lines, and both directions
are cross-terms between two branches — neither is a surprise, but both are only
visible in the integration:

**4 lines present that neither branch predicted** — all `frame`, all in
`goc/testdata/allocation_counts.go`, the corpus program change 3 adds:

    allocation_counts.go:219:10  main.packOne                          main_packed  frame
    allocation_counts.go:221:10  main.packOne                          main_packed  frame
    allocation_counts.go:223:10  main.packOne                          main_packed  frame
    allocation_counts.go:252:32  .goc.global.initfunc.49.main.theStringer  main_reason  frame

Change 2 taught the census to record front-end frame placements; change 3 added
a program with four of them. Neither branch could see this because change 2's
census predates the file and change 3's census predates the recording. This is
the product of the two changes and is correct.

**1 line predicted that is absent** — in `goc/testdata/runtime_gc_stale_result_alloca.go`,
the corpus program change 1 adds:

    runtime_gc_stale_result_alloca.go:63:9  main.main  runtime.newobject  string  heap

Line 63 is `panic("the loop did not build every string")`. Change 3's first
commit, "box a constant into an interface without allocating", removes the
allocation entirely, so the site stops existing rather than moving to a frame.
Change 1 reported three heap sites for its reducer; in the merged tree it has
two. That is change 3 improving change 1's program, and it is the only
placement in the corpus that one branch's fix erased from another branch's
new code.

No line changed *direction* unexpectedly: the 5-line residue is 4 additions and
1 deletion, and every one of the 14 528 lines both predictions share agrees on
its `frame`/`heap` field.

---

# 1. `go test -timeout 40m -parallel 10 ./goc/...`

Run with `-count=1 -v` on `integration/wave3` and on a `main` control worktree,
both watched to exit. Neither log contains `(cached)`.

    integration/wave3   1022.0 s   324 top-level, 348 subtests, 672 results
                                   667 PASS   1 FAIL   4 SKIP     -> FAIL
    main 6b9fbb0        1013.5 s   324 top-level, 348 subtests, 672 results
                                   668 PASS   0 FAIL   4 SKIP     -> ok

**Subtest census: the two trees run exactly the same 672 tests.** The name sets
are identical — no branch adds or removes a test under `./goc/...`, so there is
nothing to attribute. (Change 2's new tests are in `./opt`, change 1's in
`./arm64` and `./cmd/goc`; change 3's `goc/testdata/allocation_counts.go` is a
new corpus program consumed by existing tests, not a new test.) The four SKIPs
are the same four on both trees: `TestEscapeDifferentialAgainstGC`,
`TestEscapeDifferentialProgram`, `TestEscapeSummaryPromotionRate`,
`TestSlogAllocationsAgainstGC` — all gated behind their own flags.

`TestDeriveClassifiesEveryGenField`: **PASS.** It did not recur; `main` at
6b9fbb0 already extended `fullyPopulatedGen()` for `variadicPayloadSlot`, and
none of the three branches adds a field to `gen`.

## The one failure

    --- FAIL: TestCompileExecutableKeepsRuntimeSelectgoStackSliceHeadersOnStack
        compile_test.go:484: runtime.selectgo runtime.newobject calls = 0,
                             want only the send-on-closed panic allocation

**Attributed to change 3 alone, and it is a stale assertion, not a miscompile.**
Run on each branch head in its own worktree:

    ccwork/gc-stackscan-gogc10              ok
    ccwork/dead-slots-and-census-direction  ok
    ccwork/slog-residual-allocation         FAIL, same message, same count 0

So this is not a merge interaction — change 3 carries it on its own. That
branch's report says `go test ./goc/...` was "not run here, by instruction",
which is why it landed unnoticed.

The test asserts `newObjectCalls != 1` is a failure: it wants the two `selectgo`
stack slice headers off the heap and tolerates exactly one allocation, the
`panic(plainError("send on closed channel"))` box. Change 3's first commit,
"box a constant into an interface without allocating", removes that last one, so
the count is 0 — **strictly better than what the test demands**, and the test
fails only because it pins an exact count instead of an upper bound.

Confirmed the panic itself is intact rather than optimised away, compiled with
goc from the merged tree and run against the host toolchain on the same program:

    goc:   recovered: send on closed channel
    go:    recovered: send on closed channel

Fixing it is a one-character change (`!= 1` -> `> 1`) in
`goc/compile_test.go:483`. Per this job's brief that fix is **not** applied here;
it is diagnosed and left.

---

# 2. `make test-goc-status` and `make test-goc-status-opt`

Both arms, both trees, all four runs with `GOFLAGS=-v`, each watched to exit.
No `(cached)` in any log. Capability sets, not counts:

    default arm   integration/wave3   366 capabilities   366 PASS   0 FAIL   ok (252 s)
                  main 6b9fbb0        365 capabilities   365 PASS   0 FAIL   ok (250 s)

    -O arm        integration/wave3   366 capabilities   365 PASS   1 FAIL   FAIL (267 s)
                  main 6b9fbb0        365 capabilities   364 PASS   1 FAIL   FAIL (263 s)

**The capability set difference between the two trees is exactly one name, in
both arms:** `gc-invariants/stale-result-home`, added by change 1 as the guard
for its own fix. It PASSES in both arms. Nothing else was added, removed or
renamed, and no capability changed verdict.

Note for the record: **`main`'s default arm measures 365/365 here, not the
364/364 the brief quotes.** I did not establish which capability accounts for
the difference — the 364 figure predates 6b9fbb0 and I have no list to diff it
against. Every comparison above is against the control measured on this box at
6b9fbb0, not against the quoted figure.

## The `-O` arm is NOT clean. `stack-scan/loop-safepoints` still fails.

    -O FAIL set, integration/wave3:   { stack-scan/loop-safepoints }
    -O FAIL set, main 6b9fbb0:        { stack-scan/loop-safepoints }

Same single member on both sides. **None of the three merged branches fixed it**
— and the diagnostic is not merely the same category, it is the same output:

    runtime_stack_scan_loop_safepoints.go should pass: exit status 2
      cg12scanroots: main_carried local slot 27 ... retains ... size 16 head 0x7272616300000062
      cg12scanroots: main_carried local slot 41 ... retains ... size 16 head 0x7272616300000062
      collected while live: carried-0 at carried before rewrite
      panic: a stack slot live across a loop back edge was not a GC root

byte-identical between the two trees but for the addresses. Same slot numbers
(27 and 41), same object, same head word. The standing failure is untouched.

In the **default** arm `stack-scan/loop-safepoints` PASSES on both trees, as it
did before; it is only the `-O` configuration that fails.

---

# 3. `make test-unit`

Both trees, `GOFLAGS=-v`, watched to exit, no `(cached)`.

    integration/wave3   1614 PASS   0 FAIL   339 SKIP   25/25 packages ok
    main 6b9fbb0        1607 PASS   0 FAIL   339 SKIP   25/25 packages ok

**+7 tests, 0 failures, and every one of the 7 is a new test a merged branch
adds:**

    change 1, arm64/unit_test.go
      TestGoStackMapsOmitAggregateResultHomeAtItsOwnCall
      TestGoStackMapsKeepAllocationWrittenOnOnlyOnePath
      TestUndefinedAllocationsCoverTheWindowBeforeTheFirstStore

    change 2, opt/alloccensus_test.go
      TestAllocationCensusOmitsFrontEndFrameSlotsByDefault
      TestAllocationCensusPairsAFrontEndFrameSlotWithItsHeapForm
      TestAllocationCensusDoesNotDoubleCountAFrontEndHeapPlacement
      TestAllocationCensusOmitsAFrontEndFrameSlotWithNoAllocator

No test was removed or renamed, and the SKIP set is the same 339 on both trees.

`main` measures **1607**, not the 1598 the brief quotes; as with the capability
matrix, the quoted figure is older than 6b9fbb0. The delta above is against the
measured control.

---

# 4. `TestFrameEscapeAudit -count=1`

    integration/wave3   --- PASS: TestFrameEscapeAudit (182.29s)   no (cached)

**Zero new publications. The hard requirement holds.**

Entry count **moved, downward: 193 on `main` -> 192 on `integration/wave3`.**
The whole diff against `main`, five lines:

    - ?  main.derive  barrier  memory reached through a call result $runtime.newobject
    ~ stdlib/src/internal/strconv/atof.go:609:9 -> :609:3   internal/strconv.atof32
    ~ stdlib/src/internal/strconv/atof.go:660:9 -> :660:3   internal/strconv.atof64
    ~ stdlib/src/syscall/exec_unix.go:231:10    -> :231:4   syscall.forkExec
    ~ .../x/net/idna/idna10.0.0.go:492:11       -> :492:5   golang.org/x/net/idna.validateAndMap

Keyed on (function, how the address left, what received it) with the position
column dropped, the diff is **one removal and nothing else** — the four
remaining lines are the same publication at a shifted column, not new ones:

    < main.derive  barrier  memory reached through a call result $runtime.newobject

**Why it moved.** The merged tree's audit output is byte-identical to change 3's
committed `frame_escape_baseline.txt` (192 entries), and change 3 is the only
branch that touched the file. So both the removal and the four column shifts are
change 3's escape-precision work: `main.derive`'s call result is no longer a
`runtime.newobject` whose address reaches a barrier, so the publication stops
existing. The column shifts are the position now attributed to the `return`
statement rather than to the expression inside it.

The audit passing on the merged tree against change 3's file is also the
statement that **change 1 and change 2 introduce no frame-address publication at
all** — had either done so, the file would not have matched.

---

# 5. The allocation census, twice

`go test ./goc -run '^TestAllocationCensus$' -count=1 -v`, run twice back to
back against the committed (regenerated) baseline. No `(cached)`.

    run A   --- PASS: TestAllocationCensus (182.04s)
    run B   --- PASS: TestAllocationCensus (181.03s)

`sha256sum goc/testdata/alloc_census_baseline.txt` after each:

    A  5e8ab8ef6a3f5637c9edafd3035814951d10c9f0d091434c9035a73f779fa8cf
    B  5e8ab8ef6a3f5637c9edafd3035814951d10c9f0d091434c9035a73f779fa8cf

Identical, and identical to what the `-update` run produced. **Stable.**

---

# 6. Determinism — 399/399 over 798 compiles

    scripts/determinism-check.sh -corpus -rounds 2 -j 24

    programs=399 rounds=2 workers=24 optimize=false pack=""
    round 0: 399 programs in 112.9s, 0 failed
    round 1: 399 programs in 111.4s, 0 failed
    failed to compile: 0
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    reproducible=399 varying=0 failed=0 of 399 over 2 rounds

`main`'s reference is 398/398 over 796 compiles. The corpus gained exactly one
program — `goc/testdata/runtime_gc_stale_result_alloca.go`, change 1's reducer —
and it is reproducible like the rest. Layout residue is 0. **No determinism
regression.**

`TestCompilingTheSameSourceTwiceGivesTheSameModule` (the front-end half of the
same property) also PASSes, in 4.71 s.

---

# 7. Loop aliasing

`TestLoopAliasExpectationsMatchTheHostToolchain` and
`TestLoopBodyAllocationsAreDistinctPerIteration`, `-count=1 -v`, no `(cached)`:
**all PASS**, including every `-O` subtest. Loop-aliasing programs still match
the host toolchain.

`loop_alias_frame_local.go`'s allocations are still in frame slots. The
regenerated census records for it, in full:

    testdata/loop_alias_frame_local.go:53:8  main.literalWithin  runtime.newobject  main_point  frame

**One line, and it says `frame`. There is no `heap` line for that file.** On
`main` the census had no line for it at all — change 2's front-end frame-slot
recording is what makes the placement visible, so the merged tree states
positively what `main` could only state by absence. `framed`'s `var a [2]int`
and `consumedWithin`'s `x` are ordinary local variable slots, the category the
census excludes by design, and neither has acquired an allocator.

---

# 10. The gc differential

Regenerated from the merged tree (change 3 never regenerated it, so the merge
result was `main`'s file plus change 2's, and neither was right).

    coverage: 399 corpus programs, 395 compared, 1840 census rows joined,
              3443 gc decisions joined, 0 gc diagnostics the parser did not know

## The new confusion matrix

      goc\gc      frame     heap    mixed   absent    total
      frame         139       33       14      105      291
      heap          134      573      173      168     1048
      mixed          13       86       24       16      139
      absent        402     1269       22        0     1693
      total         688     1961      233      289     3171

`main`, for comparison:

      goc\gc      frame     heap    mixed   absent    total
      frame          37        3        3       30       73
      heap          172     1778      175      170     2295
      mixed          14       56       30       13      113
      absent        463      121       23        0      607
      total         686     1958      231      213     3088

## "goc heaps what gc keeps in a frame": 172 -> **134**

It moved, downward, and it decomposes **exactly**. Extracting the `heap -> frame`
key set from each tree's own differential:

    main                                    172
    change 2 alone                          164   (-8,  +0)
    change 3 alone                          141   (-32, +1)
    predicted composition                   134
    integration/wave3, measured             134

and not merely the same size — **the same 134 source lines, element for
element**. Nothing in that cell is a surprise and nothing was introduced by the
integration. Change 1 adds no line to it: its new corpus program contributes two
differential entries, both `absent -> heap`.

## The two big movements, and why neither is a defect

`goc absent / gc heap` goes **121 -> 1269**, and PERMISSIVE with it, 236 -> 1448.
**1142 of those 1269 are `panic("...")` lines** — gc heap-allocates the boxed
string constant and goc, since change 3's "box a constant into an interface
without allocating", allocates nothing at all. This is goc doing *less* than the
reference, not goc keeping something in a frame that gc could not prove
frame-safe. The `frame_escape_baseline` audit (item 4) is the check that would
catch the dangerous reading of the same number, and it is clean.

`goc heap / gc heap` goes **1778 -> 573**, and PESSIMISTIC 574 -> 528. That is
the same fix plus change 3's four escape-precision defects, seen from the other
side: a large block of lines where both compilers heaped now has goc allocating
nothing.

---

# 9. The slog numbers, taken fresh

    go test ./goc -run '^TestSlogAllocationsAgainstGC$' -slog-allocations -count=1 -v
    --- PASS: TestSlogAllocationsAgainstGC (18.47s)   32 cases   no (cached)
    host toolchain: go version go1.26.1 linux/arm64
    measurement:    iterations=2000 rounds=5

Then re-run with `-update-slog-allocations`: the file's SHA-256 is unchanged
(`181eabab…`) and `git status` reports it clean, so the numbers below are the
fresh measurement, not a re-read of a committed table.

Every row, goc against gc, with `main` alongside (a/op; B/op in brackets):

    case                      main a/op   NEW a/op    gc a/op   NEW B/op    gc B/op
    control/empty-body             0.00       0.00       0.00        0.0        0.0
    control/new-64-byte-object     1.00       1.00       1.00       64.0       64.0
    control/any-int-small          0.00       0.00       0.00        0.0        0.0
    control/any-int-large          1.00       1.00       1.00        8.0        8.0
    control/any-bool               0.00       0.00       0.00        0.0        0.0
    control/any-string-constant    1.00       0.00 *     0.00        0.0        0.0
    control/any-string-variable    1.00       1.00       1.00       16.0       16.0
    control/any-pointer            0.00       0.00       0.00        0.0        0.0
    control/variadic-0-args        0.00       0.00       0.00        0.0        0.0
    control/variadic-6-preboxed    0.00       0.00       0.00        0.0        0.0
    control/variadic-6-literal     0.00       0.00       0.00        0.0        0.0
    control/return-interface       0.00       0.00       0.00        0.0        0.0
    control/return-int             0.00       0.00       0.00        0.0        0.0
    control/context-background     0.00       0.00       0.00        0.0        0.0
    control/handler-enabled        0.00       0.00       0.00        0.0        0.0
    attr/slog.Int                  0.00       0.00       0.00        0.0        0.0
    attr/slog.String               0.00       0.00       0.00        0.0        0.0
    attr/slog.Bool                 0.00       0.00       0.00        0.0        0.0
    attr/slog.Duration             0.00       0.00       0.00        0.0        0.0
    attr/slog.Float64              0.00       0.00       0.00        0.0        0.0
    info/1-attr                    1.00       0.00 *     0.00        0.0        0.0
    info/3-attr                    1.00       0.00 *     0.00        0.0        0.0
    info/5-attr                    1.00       0.00 *     0.00        0.0        0.0
    info/6-attr                    2.00       1.00 *     1.00       48.0       48.0
    info/3-attr-large-ints         1.00       1.00       3.00      128.0       24.0
    logattrs/3-attr                1.00       0.00 *     0.00        0.0        0.0
    logattrs/6-attr                3.00       2.00 *     1.00       96.0       48.0
    disabled/no-attrs              0.00       0.00       0.00        0.0        0.0
    disabled/3-attr                1.00       0.00 *     0.00        0.0        0.0
    disabled/logattrs-3-attr       1.00       0.00 *     0.00        0.0        0.0
    json/kv-4-pairs                5.00       5.00       2.00      256.0       24.0
    json/logattrs-4-attrs          5.00       4.00 *     0.00       80.0        0.0

    * = improved against main.   32 rows: 10 improved, 0 regressed, 22 unchanged.
    28 of 32 rows are now at exact parity with gc on a/op (main: 21).

**`info/5-attr` is 0.00 against gc's 0.00.** It was 1.00 / 288 B on `main`; it
is now zero allocations and zero bytes, which is gc's number. So is
`info/1-attr`, `info/3-attr`, `logattrs/3-attr`, `disabled/3-attr` and
`disabled/logattrs-3-attr`. `info/6-attr` is at exact parity on both columns,
1.00 / 48 B. Change 3's report claimed exactly these seven rows and every one of
them reproduces here.

**No row got worse, on either column.** Four rows remain off gc:

    info/3-attr-large-ints   1.00 vs 3.00   goc allocates FEWER times than gc,
                                            but 128 B against gc's 24 B
    logattrs/6-attr          2.00 vs 1.00   96 B against 48 B
    json/kv-4-pairs          5.00 vs 2.00   256 B against 24 B
    json/logattrs-4-attrs    4.00 vs 0.00   80 B against 0 B

All four improved or held against `main` (400->48, 336->96, 320->256, 240->80 B);
none is a regression this integration introduced.

---

# 8. The GC reducer — `runtime_gc_type_mask_padding.go`

The point of change 1. Both trees' binaries built once with
`go run ./cmd/goc -o <bin> goc/testdata/runtime_gc_type_mask_padding.go`, then
run **serially, one process at a time, on an idle box** (load average 4.6 and
falling at the start; nothing else of this job's was running), `GOMAXPROCS=3` as
the capability matrix sets it, 180 s timeout per run. A run counts as a pass
only if it exits 0 *and* prints exactly `type mask padding ok`.

**Both trees, both settings:**

    tree                GOGC=10            GOGC default
    main 6bb0           10/40 FAILED        0/20 failed
    integration/wave3    0/40 failed        0/20 failed

The `GOGC=10` figures are two independent blocks of 20, run in the order
main-20, integration-20, main-20, integration-20:

    main         block 1: 5/20 failed    block 2: 5/20 failed    -> 10/40
    integration  block 1: 0/20 failed    block 2: 0/20 failed    ->  0/40

**Change 1 fixes it.** `main`'s failure rate at `GOGC=10` reproduces at 25 %
(10/40), close to the 6/40 the brief records and clearly non-zero; at that rate,
40 clean runs on the integration tree would happen by chance about 1 time in
700. Every `main` failure is the same defect, e.g.

    runtime: pointer 0x5b7e718daca8 to unused region of span
             span.base()=0x5b7e717e0000 span.limit=0x5b7e717e2000 span.state=1
    runtime: found in object at *(0x5b7e7179fc90+0x208)
    object=... s.state=mSpanManual

— the same `+0x208` offset into an `mSpanManual` object on every one of the ten.

At **default `GOGC` both trees are 0/20**, as the brief says: the defect only
shows under aggressive collection, and change 1 does not disturb the default
arm. `gc-invariants/type-mask-padding` also PASSes in both capability arms on
both trees (item 2).

For the record, and as the brief instructs: **`main` is 10/40 at `GOGC=10`, not
0/20.** It fails, repeatedly and reproducibly.

---

# Summary

| # | item | result |
|---|---|---|
| — | merge of the three branches | clean; 2 doc conflicts + 1 baseline conflict, all resolved |
| — | root build artefacts | **clean** — no new ones; the four pre-existing left alone |
| — | four generated baselines regenerated | census and differential changed; shadow and slog byte-identical |
| — | census composition | 14 533 lines vs 14 530 predicted; **5-line residue, both cross-terms explained** |
| 1 | `go test ./goc/...` | **1 FAIL** — 667 PASS / 1 FAIL / 4 SKIP vs main's 668 / 0 / 4 |
| 2 | capability matrix, default arm | 366/366 PASS (main 365/365) |
| 2 | capability matrix, `-O` arm | 365/366 — `stack-scan/loop-safepoints` **still fails**, as on main |
| 3 | `make test-unit` | 1614 PASS / 0 FAIL (main 1607 / 0); +7 tests, all from the branches |
| 4 | `TestFrameEscapeAudit` | **PASS, zero new publications**; 193 -> 192 entries |
| 5 | allocation census x2 | PASS, PASS, byte-identical output |
| 6 | determinism | **399/399 over 798 compiles**, 0 varying, 0 layout residue |
| 7 | loop aliasing | PASS; `loop_alias_frame_local.go` framed, no heap line |
| 8 | GC reducer at `GOGC=10` | **main 10/40 FAIL, integration 0/40** — change 1 works |
| 8 | GC reducer at default `GOGC` | 0/20 on both |
| 9 | slog table, fresh | 28/32 rows at gc parity (main 21); **`info/5-attr` 0.00 vs gc 0.00**; 0 regressions |
| 10 | gc differential | goc-heaps-what-gc-frames **172 -> 134**, decomposes exactly |

## The two things worth carrying forward

**The `-O` arm was not fixed.** `stack-scan/loop-safepoints` fails on
`integration/wave3` exactly as it fails on `main` — same slots, same object,
same panic. None of the three branches touched it. The brief's hope that one of
them had is not borne out.

**One test is red, and it is a fixture, not a miscompile.**
`TestCompileExecutableKeepsRuntimeSelectgoStackSliceHeadersOnStack` asserts
`runtime.selectgo` makes *exactly one* `runtime.newobject` call, the
send-on-closed panic box. Change 3 removed that allocation, so the count is 0 —
better than the test demands. Reproduced on `ccwork/slog-residual-allocation`
alone, so it is not a merge interaction; that branch's report says
`go test ./goc/...` was not run there, which is why it shipped. The panic still
carries its message under goc, checked against the host toolchain. The fix is
`compile_test.go:483`, `!= 1` -> `> 1`, and per this job's brief it was
diagnosed and **not applied**.

## Verdict

Every one of the ten items ran to completion and was watched to exit. Nothing is
UNVERIFIED. The compiler evidence is good in every direction that matters: zero
new frame-address publications, determinism intact at 399/399, no capability
regression in either arm, the GC reducer's `GOGC=10` failure gone from 10/40 to
0/40, ten slog rows improved and none regressed, and both regenerated baselines
composing from the three branches' own reported deltas with a residue of five
lines that are fully accounted for.

But `go test ./goc/...` is red on the integration branch, and merging it would
make `main` red.

**NOT SAFE TO MERGE TO MAIN** — one test fails,
`TestCompileExecutableKeepsRuntimeSelectgoStackSliceHeadersOnStack`, because
`ccwork/slog-residual-allocation` improved `runtime.selectgo` past an exact-count
assertion (`newobject calls = 0`, test wants exactly 1) without updating it. It
is a one-character fixture change at `goc/compile_test.go:483` and no compiler
defect was found behind it; with that one line changed, everything in this
report says merge.

---

# Appendix — the three merged branches' own reports

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

---

# Residual slog allocation: the combined variadic object

**The 64-byte allocation on `info/1-attr` was goc's combined variadic object for
`Logger.Info`'s `...any` argument list** --
`struct { values [2]any; payload0 string; payload1 int }`, 56 bytes in the
64-byte size class: a `[2]any` backing array plus a reserved slot for the boxed
key `"a"` and one for the boxed value `1`. Not a Record, not a Value, not an
Attr copy. gc keeps the array in the frame and makes both payloads static
symbols, so it allocates nothing.

**It is fixed.** Two independent things held it on the heap and both had to go:
the object pointed into itself (a `string` payload has no runtime conversion
helper, so it is laid out as a field of the array's own object), and goc's
escape summary said `Logger.Info` retains the pointer where gc says only its
contents leak. `info/1-attr`, `info/3-attr`, `info/5-attr`, `logattrs/3-attr`,
`disabled/3-attr` and `disabled/logattrs-3-attr` are now 0.00 allocations, which
is gc's number, and `info/6-attr` is at exact parity at 1.00 / 48 B. The full
table is below.

The rest of this section is the diagnosis in the order it was done.

Baseline re-run at 6b9fbb0 reproduces the committed table exactly
(`go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations`, PASS, 19.6s).

## First arithmetic on the byte counts

The bytes/op column identifies the object before any IR is dumped. Under goc:

    info/1-attr   64 B    info/3-attr  176 B    info/5-attr  288 B

Differences are 112 B per two attributes -- 56 B per attribute, with 8 B of slack
at the bottom. Fitting the size classes:

    n attrs  ->  variadic []any backing array   32n B   (2n elements, 16 B each)
             +   boxed payload slot per string  16n B
             +   boxed payload slot per int      8n B
             =                                  56n B  -> 56, 168, 280
             -> Go size classes                          64, 176, 288   MATCH

`info/3-attr-large-ints` is 176 B too, which the fit predicts (the payload slot
is reserved whether or not staticuint64s ends up being used), and gc's 3x8 B
there confirms the 8 B int slot is the right unit.

`logattrs/3-attr` is 128 B: 3 x sizeof(slog.Attr)=40 = 120 -> class 128. That is
the `...Attr` variadic backing array with no payload slots, since Attr is not an
interface. Same shape, same cause.

So the single residual allocation is NOT a Record, NOT a Value box, and NOT an
Attr copy. It is **the combined variadic object -- the `...any` backing array
plus its reserved boxing payload slots -- being heap-allocated at the call site
instead of living in the caller's frame.**

That explains `disabled/3-attr` costing 176 B while `disabled/no-attrs` costs 0:
the cost is paid building the argument list before `Logger.log` is entered and
before the level is ever checked. It is attribute-related because a call with no
attributes has no backing array to build.

Still to establish: why goc's escape question answers "escapes" for
`slog.Logger.Info` when it answers "does not escape" for the in-package
`noRetain(...)` control (0.00) and for `fmt.Sprintf` (parity at 1.00).

## Confirmed from the IR: what the 64-byte object is

Reduction (`hot` is `//go:noinline` so the call site is isolated):

    //go:noinline
    func hot() { logger.Info("msg", "a", 1) }

`goc -O -emit-ir` gives, in `$main.hot`:

    %t9 =p call $runtime.newobject(
        p $.goc.runtime.type.struct_values__2_any__payload0_string__payload1_...)
    %t10 =p add %t9, 32      ; &payload0  -- the boxed "a"
    ...
    %t23 =p add %t9, 48      ; &payload1  -- the boxed 1
    storel 1, %t23
    call $log/slog.Logger.Info(p %t1, <msg> %t4, p %t9, l 2, l 2)

and the type descriptor records its size directly:

    data $.goc.runtime.type.struct_values__2_any__payload0_string__payload1_... =
        align 8 { l 64 56, ... }        ; size 56, ptrdata 64-class

**The 64-byte allocation is goc's combined variadic object for `Logger.Info`'s
`...any` argument list**: `struct { values [2]any; payload0 string; payload1 int }`
-- a 32-byte `[2]any` backing array, a 16-byte reserved slot holding the boxed
key `"a"`, and an 8-byte reserved slot holding the boxed value `1`. 56 bytes,
allocated by `runtime.newobject` into the 64-byte size class. gc keeps the
`[2]any` in the frame and makes both payloads static symbols, so it allocates
nothing.

## Why it goes to the heap

Traced by instrumenting `opt.lowerFunctionHeapAllocations` to report
`candidateEscapes.reason` (temporary; not committed):

    main.hot: t8  escaped=true reason="argument 2 of $log/slog.Logger.Info
                  may retain something inside a self-referential object"
    main.hot: t22 escaped=true reason="argument 2 of $log/slog.Logger.Info
                  may retain something it points at"

Two independent constraints, and **both** have to be removed:

1. **The object is self-referential.** Element 0's data word points at
   `payload0`, a field of the same allocation. `opt.markSummarisedCall` runs
   `needsDeepSummary` before it ever consults the escape verdict: a callee that
   may retain an *element* retains the whole object, because they are one
   object. `string` is what makes this true here -- `variadicPayloadStorage`
   splits a payload out only when `interfaceConversionHelper` has one (bool,
   int8..int64, uint*, uintptr, float32/64), and there is no `convTstring`.

2. **The summary itself says the array escapes.** goc's fact table:

       $log/slog.Logger.Info: 0:escapes 1:noescape 2:escapes 3:escapes 4:escapes
                                                   ^ the args backing-array pointer
       $log/slog.Record.Add:  0:noescape 1:escapes 2:escapes 3:escapes
       $log/slog.argsToAttr:  0:leaks-to-result(0) 1:leaks-to-result(0) ...

   gc, for the identical source (`go build -gcflags=-m log/slog`):

       logger.go:208:35: leaking param content: args      # (*Logger).Info
       record.go:129:22: leaking param content: args      # (*Record).Add
       record.go:168:17: leaking param content: args      # argsToAttr
       record.go:168:17: leaking param: args to result ~r0 level=1
       record.go:168:17: leaking param: args to result ~r1 level=0

   gc says *content* leaks and the pointer does not. goc has exactly that
   vocabulary -- `ParamFact.Deep` is the level distinction -- but its
   leaks-to-result edge has no level, so `Any(x, args[1])`, which is a *load*
   through `args`, propagates the array pointer instead of its contents. From
   `argsToAttr` the imprecision walks up through `Record.Add`, `Logger.log` and
   `Logger.Info`.

Neither fix helps alone: remove (1) and the verdict in (2) still escapes the
array; remove (2) and `needsDeepSummary` still escapes it. That is why this
survived three previous rounds -- it is not a variant of the convT, variadic-
escape-question, or interface-return causes, all of which are confirmed closed
by the control rows in the same table.

## The fix, in two parts

Both blockers had to go, and neither is a variant of a previously closed cause.

### 1. goc: box a compile-time constant without payload storage (34a4275)

A constant conversion has no storage to allocate. cmd/compile emits a read-only
`stmp_N` symbol (or `&runtime.staticuint64s[c]`); goc allocated. Now goc emits
one read-only object per distinct (type, value) pair, reusing the same
`staticValueItems` renderer the package-level static-initializer path already
had, so the pointer-word metadata and the per-kind rendering are shared.

That removes `control/any-string-constant`'s 16 bytes, but the larger effect is
that the payload *field* disappears:

    before: newobject(struct{ values [2]any; payload0 string; payload1 int })
    after:  [2]any                                       -- 56 bytes down to 32

so the argument object no longer points into itself and `needsDeepSummary` no
longer has to escape it.

### 2. opt: four precision defects in the escape summary (e6999bd)

Found by comparing goc's fact table against `go build -gcflags=-m log/slog`
line by line.

  a. **Merged result nodes.** `resultCount` took the *larger* of "values
     returned" and "result-area out-parameters" instead of the sum, and numbered
     the out-parameters from zero on top of the returns. `argsToAttr` returns
     `(Attr, []any)` -- an aggregate return plus one `%result1` out-parameter --
     so its two results were one node, and gc's

         args to result ~r0 level=1     (the Attr: content only)
         args to result ~r1 level=0     (the slice: the pointer)

     collapsed into a single level-0 leak. Now they are disjoint nodes.

  b. **A leak into caller-owned storage read as a publication.** With (a) fixed,
     `argsToAttr` says "leaks to result 1", and result 1 is storage the *caller*
     passed in. The caller now models that as a store into that argument --
     which for a frame slot keeps it in the frame -- instead of giving up.

  c. **The depth-1 publication applied to a callee that had already disclaimed
     it.** Every call published `flow(heap, argument, 1)`: "I do not believe you
     keep nothing this points at." `ParamFact.Deep` is exactly the disclaimer
     that makes it unnecessary, and the out-slot's own summary carries it. Left
     in, it republished everything the callee had just written into the result
     area -- including the slice (b) had just proved stays in the frame.

  d. **Aggregate arguments that the callee had scalarised.** goc's parameter
     builder flattens an interface parameter into two words; its call emitter
     hands the interface over as one descriptor. `Logger.Info` calling
     `Logger.log(ctx, level, msg, args...)` therefore passes seven values to
     eight parameters, and `summarisedCallee` threw the whole call's summary
     away over the one argument that did not line up -- the backing array being
     one of the other six. An aggregate argument is now repeated once per
     parameter it carries, so each consumer asks about it once per parameter and
     gets the worse answer, which is the right answer for a value holding both
     words.

After all four, goc's table says what gc's does:

    $log/slog.Logger.Info: 0:noescape 1:noescape 2:noescape 3:noescape 4:noescape
    $log/slog.Record.Add:  0:noescape 1:noescape 2:noescape 3:noescape
    $log/slog.argsToAttr:  0:leaks-to-result(1) ... 3:noescape

    logger.go:208:35: leaking param content: args    <- gc, same conclusion

and `main.hot` allocates nothing: the `[2]any` is `alloc8 32` in the frame.

## The JSON rows: one row is the same cause, the rest is not

Decomposed by running the two JSON cases under goc with `runtime.MemProfileRate = 1`
and reading `runtime.MemProfile` stacks, then reading the source position off the
`newobject` call in the emitted IR.

    json/logattrs-4-attrs   goc 4.00 / 80 B    gc 0.00 / 0
      2 x 16 B  log/slog/handler.go:272 and :321 -- `defer state.free()` and
                `defer h.mu.Unlock()`. goc heap-allocates a deferred call's
                closure environment, struct{code uintptr; receiver ...}; gc
                open-codes the defer and allocates nothing.
      2 x 24 B  internal/strconv/itoa.go:72 -- `var a [24]byte` in AppendUint,
                once for the status int and once for the duration. It is passed
                to formatBase10 as a[:] and goc cannot place it in the frame.

    json/kv-4-pairs         goc 5.00 / 256 B   gc 2.00 / 24 B
      the same four, 80 B, plus
      1 x 176 B  the combined variadic object, size 168:
                 struct{ values [8]any; payload1 string; payload3 ... }

`main.attrs` emits no `runtime.newobject` at all -- its `[4]Attr` is
`alloc8 160` in the frame -- so **none** of `json/logattrs-4-attrs` is this
cause. Four allocations in the handler and in strconv, neither of which slog's
allocation-avoidance design has anything to do with.

`json/kv-4-pairs`'s extra 176 bytes **is** the same cause, unfixed for the one
reason that survives: `method` is a string *variable*. A non-constant payload
with no runtime conversion helper -- and there is no `convTstring` --
still gets a field inside the array's own object, so the object still points
into itself and `needsDeepSummary` still sends it to the heap. gc pays one
16-byte box for the same value and keeps its array in the frame.

Closing that last one means either giving `string` a conversion helper (which
costs an allocation per split payload, and `foldSplitPayloadsBackIn` would pull
two or more back into the array anyway) or teaching LowerHeapAllocations to
split a combined object late, when the array turns out not to escape but a
payload does. Neither is this change.

## Every slog row against gc, regenerated

`goc/testdata/slog_allocations_baseline.txt`, rewritten from a fresh run
(go1.26.1 linux/arm64, iterations=2000 rounds=5). allocations/op then bytes/op,
goc first, gc second.

    case                        before (goc)      after (goc)        gc
    control/any-string-constant  1.00   16 B      0.00    0 B      0.00   0 B   parity
    info/1-attr                  1.00   64 B      0.00    0 B      0.00   0 B   parity
    info/3-attr                  1.00  176 B      0.00    0 B      0.00   0 B   parity
    info/5-attr                  1.00  288 B      0.00    0 B      0.00   0 B   parity
    info/6-attr                  2.00  400 B      1.00   48 B      1.00  48 B   parity
    info/3-attr-large-ints       1.00  176 B      1.00  128 B      3.00  24 B   goc allocates less
    logattrs/3-attr              1.00  128 B      0.00    0 B      0.00   0 B   parity
    logattrs/6-attr              3.00  336 B      2.00   96 B      1.00  48 B   one over
    disabled/no-attrs            0.00    0 B      0.00    0 B      0.00   0 B   parity
    disabled/3-attr              1.00  176 B      0.00    0 B      0.00   0 B   parity
    disabled/logattrs-3-attr     1.00  128 B      0.00    0 B      0.00   0 B   parity
    json/kv-4-pairs              5.00  320 B      5.00  256 B      2.00  24 B   three over
    json/logattrs-4-attrs        5.00  240 B      4.00   80 B      0.00   0 B   four over

Every other row in the file is unchanged and already at parity. The three rows
that are not at parity are accounted for above: `logattrs/6-attr` and
`json/*` are the spill slice and the handler/strconv sites, and
`json/kv-4-pairs`'s 176 bytes is the one remaining instance of this cause, held
open by a non-constant string payload.

`info/3-attr-large-ints` is goc allocating *less* than gc -- one combined object
against gc's three separate boxes -- which is `opt.foldSplitPayloadsBackIn`
deliberately choosing one allocation over three. It is on the table because the
bytes are worse, not the count.

## Guards

    TestExecutionCorpus                             PASS
    TestAllocationCounts                            PASS  (four new rows, table unchanged otherwise)
    TestAllocationCountsAgainstTheHostToolchain     PASS
    TestCompilingTheSameSourceTwiceGivesTheSameModule  PASS   determinism holds
    TestLoopBodyAllocationsAreDistinctPerIteration  PASS   loop-aliasing programs match the host
    TestLoopAliasExpectationsMatchTheHostToolchain  PASS
    TestSlogAttrInFrameIsNotScannedAsAPointer       PASS
    TestSlogAllocationsAgainstGC                    PASS after regenerating the baseline
    TestFrameEscapeAudit                            baseline moved -- reviewed below
    TestEscapeShadowPlacement                       baseline moved -- reviewed below

### TestFrameEscapeAudit, site by site

Five entries vanished and four appeared. Every appeared entry pairs with a
vanished one: same function, same kind (`barrier`), same destination (the
caller's result area), same source *line*, and a column that moved from the
right-hand side of the assignment to the assignment itself.

    -  internal/strconv/atof.go:609:9   atof32           ->  609:3
    -  internal/strconv/atof.go:660:9   atof64           ->  660:3
    -  syscall/exec_unix.go:231:10      forkExec         ->  231:4
    -  x/net/idna/idna10.0.0.go:492:11  validateAndMap   ->  492:5

Reading the four lines: `err = ErrRange`, `err = ErrRange`, `err = EPIPE`,
`err = runeError(utf8.RuneError)`. Column 3/4/5 is `err`; column 9/10/11 is the
right-hand side. `FrameEscape.Pos` is the position of the *publishing
instruction*, and goc emits a `loc` only when it changes, so removing the
payload allocate-and-store that a boxed right-hand side used to emit leaves the
statement's own position in effect at the store. Same publication, same
statement, relabelled.

These are the documented pre-existing hazard the baseline header names: cg12
returns an error by writing the address of a sixteen-byte frame slot into the
caller's result area (RUNTIME_PLAN.md 5.15's residual). **No function acquired a
publication it did not already make**, which is the form of the check that
matters: a newly-framed object published somewhere new would show as a triple
that appears with no partner.

The fifth, with no partner, is a publication that stopped:

    -  ?  main.derive  barrier  memory reached through a call result $runtime.newobject

`derive` is the shared generic body in
goc/testdata/runtime_type_param_method_dispatch.go, returning `(int, error)`. A
frame address that used to be barrier-stored into a heap object is not stored
there any more. That is the safe direction, and it is the only unpaired change.

### The allocation census, reviewed by category

`goc/testdata/alloc_census_baseline.txt`: **3444 lines removed, 149 added.**
That is a large diff for two changes, so it needs the fifth review question
answered first -- does the size match the change?

**3429 of the 3429 removed heap sites are one thing: a constant that no longer
needs a payload object.** Grouped by what was being allocated:

    2146  runtime.newobject  string                     a boxed string constant
     701  runtime.newobject  runtime_errorString        panic("...") in the runtime
     177  runtime.convT64    syscall_Errno              `err = EPIPE` and friends
      48  runtime.convT64    int
      47  runtime.convT32    net_http_http2ConnectionError
      40  runtime.convT64    uint8
      36  runtime.newobject  image_jpeg_FormatError
      27  runtime.convT64    internal_strconv_Error
      22  runtime.newobject  compress_bzip2_StructuralError
      ... 14 x struct_values__2_any__payload0_string__payload1_ and the rest,
          all constant conversions or the payload fields of one

Every stdlib package that panics with a string constant or returns a typed
constant error is in there once per site. 3429 is what "goc allocated for every
constant conversion in the program and now does not" looks like.

Census review question 3 asks whether a vanished site means the allocation is
gone or the *code* is gone -- the 9f76498 failure being a heap site vanishing
because the object quietly became a frame slot. Neither applies: the object
moved to **read-only data**, which outlives every frame and every heap object,
so publishing its address is safe by construction wherever it was safe to
publish the heap object's.

The 149 added lines are 47 placement moves plus 102 new site keys:

  - **47 sites moved heap -> frame** (question 1, the correctness-critical
    direction). They are the variadic combined objects the change is about
    (`net/http.http2summarizeFrame`, `testing.BenchmarkResult.String`,
    `chunkWriter.Write`, `http2FrameHeader.writeDebug`), ordinary objects the
    more precise summary now proves local (`crypto/tls.Dialer.DialContext`'s
    `net.Dialer`, `crypto/ecdsa.verifyLegacy`'s `big.Int`,
    `errors.AsType[...]`, `crypto/x509.checkConstraints[...]`), and nine
    closure environments in `net`/`os` -- the `ctrlCtxFn` literals passed to
    `internetSocket`, which call them and return.

  - **102 new site keys**, of which the ones under `.goc.global.initfunc.*` are
    package initializers whose allocation used to be attributed to a site that
    also had a constant conversion on it, and the `convT64`/`convT32` ones are
    the other half of the moves above: the payload that used to be a field of
    the combined object is now its own conversion-helper call at the same
    source line. `net/http.http2summarizeFrame` appears in both lists for
    exactly that reason.

What backs the heap -> frame direction, since "the analysis says so" is not an
answer:

  - `TestFrameEscapeAudit` reads the **emitted stores**, not the analysis, and
    is clean: across the whole corpus not one frame address is published
    anywhere new, and one publication that used to happen has stopped.
  - The retention rows in `TestAllocationCounts` -- `variadic_retained_element`
    and `variadic_retained_struct_element`, written for the earlier attempt that
    got this wrong in the dangerous direction -- are unchanged at 100.
  - A direct check of the closure shape: a closure literal passed to a callee
    that stores it in a global stays on the heap, and one passed to a callee
    that only calls it stays in the frame, identically before and after.
  - `TestExecutionCorpus`, the loop-aliasing suite and the slog-attr-frame
    suite all pass.

I did not hand-prove all 47 individually. The stdlib corpus programs these sites
live in are executed by the capability matrix, which this job does not run.

### TestEscapeShadowPlacement

19 lines added, 21 removed. This test changes nothing the compiler emits: it
asks what the IR analysis *would* have decided about allocations the front end
placed itself, and `goc.gen.recordPlacement`'s own comment says those are "the
allocations the IR pass never gets a say in". The new `heap -> frame` lines are
sites where the IR analysis now reaches the answer gc reaches --
`readDirectoryHeader(&File{}, rs)`, `jpeg.Encode(..., &jpeg.Options{Quality: 95})`,
`(&edwards25519.Point{}).ScalarBaseMult(s)`, `append([]byte{0}, pskIDHash...)` --
while the front end stays conservative and its placement is what is emitted.

### Guard results against the regenerated baselines

    TestAllocationCensus                            PASS
    TestFrameEscapeAudit                            PASS
    TestEscapeShadowPlacement                       PASS
    TestAllocationCounts                            PASS
    TestAllocationCountsAgainstTheHostToolchain     PASS
    TestExecutionCorpus                             PASS
    TestEscapeSummaryFacts                          PASS
    TestCompilingTheSameSourceTwiceGivesTheSameModule  PASS
    TestLoopBodyAllocationsAreDistinctPerIteration  PASS
    TestLoopAliasExpectationsMatchTheHostToolchain  PASS
    TestSlogAttrInFrameIsNotScannedAsAPointer       PASS
    TestSlogAllocationsAgainstGC                    PASS

Not run here, by instruction: `go test ./goc/...` as a whole, the capability
matrix, and `make test-unit`. The dependent gate job runs those.

---

# parity-remaining-134 (branch ccwork/parity-remaining-134, off 9cdc2d8)

Task: classify the 134 source lines where goc heaps what gc frames, fix the
systematic classes, say what is irreducible.

## Status log

- Extracted the 134 `heap -> frame` entries from the PESSIMISTIC section of
  `goc/testdata/escape_gc_differential.txt`. They span 86 corpus files; the
  goc-side allocators are 142 sites in total (some lines carry two).

## Classification of the 134

Method: a scratch driver (`censusdump`) compiles one corpus program and prints
`opt.AllocationCensusWith(..., IncludeFrontEndFrameSlots: true)`, the same census
the committed baseline is built from. Running it over the 86 corpus files that
carry the 134 lines reproduces all 142 goc allocation sites on those 134 lines
exactly, so a fix can be attributed by re-running and diffing rather than by
reading. `GOC_DEBUG_ESCAPE=2` names the use that decided each object escapes; a
temporary level-3 patch also named the rule that rejected it.

Split by allocator (goc side) and construct (gc side):

     43  newobject | slice     fixed-size backing arrays
     41  newobject | object    &T{...}, new(T), interface payloads
     35  makemap   | map
      6  convT64   | object    boxed scalars
      5  makeslice | slice     non-constant-length make
      2  makemap   | object    named map types (url.Values, textproto.MIMEHeader)
      1  newobject | object,slice
      1  makemap   | map,object

The 134 fall into these groups. Verdict = which compiler is right.

### Progress (lines where goc heaps what gc frames, over the 86 affected files)

    134  baseline
    127  after F1: a string<->[]byte/[]rune conversion copies, so the operand
         does not escape through it   (-7)
    126  after F2: `var x T = y` answers what `x = y` answers; interfaces alias
         the storage they hold; i.(T) and p.f keep the walk going   (-1)
    120  after F3: ranging over an object does not carry it anywhere   (-6)

## Verdicts, group by group

Numbers are lines of the 134.

**G1 -- no frame-allocated map header (38).** `makemap|map` 35, `makemap|object`
2 (`url.Values{}`, `textproto.MIMEHeader{}`), `makemap|map,object` 1. The whole
committed census contains **zero** `runtime.makemap ... frame` rows, and zero
for `makeslice`, `makechan` and `convT*` as well: `newobject` is the only
allocator goc has a frame form of. `gen.allocateMap` calls
`runtime.makemap(t, hint, 0)` unconditionally, so the escape analysis is never
consulted for a map at all. gc stack-allocates the `maps.Map` header when the
map does not escape, and one group with it when the hint is small; the vendored
runtime already supports it (`runtime/map.go`: "If m != nil, the map can be
created directly in m"). VERDICT: gc is right. This is a representation gap, not
an analysis defect, and it is the single largest group.

**G2 -- pre-inlining vs post-inlining (8).** Every `make([]byte, N)` handed to
`(*os.File).Read`: the six netpoll programs, `stdlib_os_file_roundtrip`,
`stdlib_os_pipe_goroutine_close`, `runtime_println_operand_separation:168`,
`runtime_stack_scan_syscall:115`. goc's walk stops at
`internal/poll.ignoringEINTRIO`, which calls `fn(fd, p)` through a
function-typed *parameter*. Measured against the host toolchain:
`fd_unix.go:736:70: leaking param: p` -- **gc's own summary for that function
leaks p too**. gc reaches "p does not escape" only because it inlines
`ignoringEINTRIO` first and the call becomes a static `syscall.Read`, whose
summary is clean. VERDICT: gc is right about the buffer, but not by a rule goc
is missing. goc's walk runs on the AST before any inlining and structurally
cannot see through a call to a function-valued parameter. Irreducible without
either an inliner ahead of the walk or a whole-program resolution of
function-valued parameters.

## Guards

- GC reducer `runtime_gc_type_mask_padding.go`: **0/20 failures at GOGC=10**
  (compiled with the branch's compiler, run 20 times).
- `TestFrameEscapeAudit`: clean. `frame_escape_baseline.txt` is unchanged by the
  whole branch -- no new frame address reaches non-local storage.
- Census regenerated and reviewed site by site: **73 heap -> frame, 0
  frame -> heap, 50 vanished (all previously heap), 0 appeared**, ignoring the
  line-number shifts in `testdata/allocation_counts.go`, which this branch edits.
  The 50 vanished are the direction the census cannot see both sides of: the
  object became an ordinary front-end frame slot, which carries no allocator and
  is not recorded. Spot-checked: `crypto/sha1.digest.Sum`'s `newobject 20_byte`
  is `hash` in `return append(in, hash[:]...)`, a local `[20]byte` whose storage
  the spread operand used to publish. The 73 that moved are mostly
  `bytes.Equal([]byte{...}, ...)` in the FIPS self-tests (23 in
  crypto/internal), which move because `bytes.Equal` is
  `string(a) == string(b)`.
- `TestEscapeShadowPlacement` needed its baseline regenerated: the front end now
  frames things the summary-fed IR analysis would still heap, so the recorded
  disagreements moved. Front-end placements in frames went 83.4% -> **83.9%**
  (165750 of 197518).

## 4. The slog rows

`slog_allocations_baseline.txt` had 4 of 32 rows off gc. Two of them moved on
this branch:

    json/kv-4-pairs        5.00 -> 3.00 allocs/op, 256.0 -> 208.0 B/op  (gc 2.00, 24.0)
    json/logattrs-4-attrs  4.00 -> 2.00 allocs/op,  80.0 ->  32.0 B/op  (gc 0.00,  0.0)

**Same causes.** Compiling `goc/testdata/slog_allocations/main.go` with the
branch point's compiler and with this branch's, 24 heap sites in that program
stopped being allocations, and they are the number-formatting buffers the JSON
handler runs once per attribute:

    internal/strconv.formatBits            newobject 65_byte   append(dst, a[i:]...)
    internal/strconv.FormatInt/AppendUint  newobject 24_byte   same
    time.Duration.String                   newobject 32_byte   string(buf[w:])
    time.Time.Format                       newobject 64_byte
    io/fs.FileMode.String                  newobject 32_byte   string(buf[:w])
    log.itoa, runtime.appendIntStr         newobject 20_byte   append spread

-- that is, exactly two of the causes behind the 134: append's spread operand
and the []byte-to-string conversion.

The residue is in the same families as the 134's remaining groups, not in new
ones. In the program's own census, `json/kv-4-pairs`'s call site is one heap
object, `struct_values__8_any__payload1_string__payload3_...` -- goc folds the
`...any` backing array and the payloads it boxes into a single allocation, where
gc frames the array and boxes each escaping value separately. The two remaining
allocations in `json/logattrs-4-attrs` are 32 B in two objects, the size and
shape of `log/slog.Value.Any`'s `newobject string` payloads: interface payloads,
which goc has no frame form of (zero `convT64 ... frame` and zero
`newobject <payload> frame` rows in the whole census).

`info/3-attr-large-ints` is the same folding seen from the other side: goc 1.00
alloc / 128 B against gc's 3.00 / 24 B. It is off gc and it is not a regression
-- one combined object instead of three boxes.

`logattrs/6-attr` (goc 2.00 / 48 B over gc's 1.00) is the variadic backing array
for `...Attr`, which is the documented variadic-summary gap and is group G10
below.
- `TestCompilingTheSameSourceTwiceGivesTheSameModule` (determinism): PASS.
- `TestLoopBodyAllocationsAreDistinctPerIteration` and
  `TestLoopAliasExpectationsMatchTheHostToolchain`: PASS -- loop-aliasing
  programs still match the host toolchain.
- `TestAllocationCounts` and `TestAllocationCountsAgainstTheHostToolchain`:
  PASS, including the five new rows.
- `TestTypeGCMasksArePaddedToAPointerWord`: PASS.

## The classification, group by group

The 134 fell into **17 groups**: 5 that this branch fixed, and 12 that remain.
Every line is in exactly one group; the lists below are generated from the
regenerated differential and sum to 113 + 21 = 134.

### The 12 groups that remain (113 lines)

**G1 -- maps have no frame-allocated header (38). VERDICT: gc is right.**
`gen.allocateMap` calls `runtime.makemap(t, hint, 0)` unconditionally, so the
escape analysis is never asked about a map. The whole census contains zero
`makemap ... frame` rows. gc stack-allocates the `maps.Map` header when the map
does not escape and one group with it when the hint is small, and the vendored
runtime already accepts it (`runtime/map.go`: "If m != nil, the map can be
created directly in m"). This is a representation gap, not an analysis defect,
and it is the largest single group by a factor of two. Closing it means giving
the front end `internal/runtime/maps.Map`'s layout, a pointer-word mark for its
`dirPtr`, and an escape question for map variables that has never been asked
once -- which would move hundreds of census sites in the correctness-critical
direction at one go. It is a task of its own and I did not attempt it here.

**G4 -- a value boxed into an interface (17). VERDICT: gc is right; two causes,
both structural.** Twelve of these are the *payload*: goc has no frame form for
one, so `convT64` and the `newobject` behind a boxed string or array are always
heap (zero `convT64 ... frame` rows in the census). Five are the *source*: the
walk's `boxedIntoInterface` is unconditional, so a slice whose header is copied
into a box that provably dies at the call -- `runtime.KeepAlive(values)`,
`reflect.TypeOf([]int{})` -- is charged with the box's escape. Boxing copies, so
the right question is where the box goes; asking it needs the payload to have a
frame form first, or the two halves disagree.

**G2 -- the Read buffer (10). VERDICT: gc is right, but not by a rule goc is
missing.** Measured above: `internal/poll.ignoringEINTRIO`'s own gc summary is
`leaking param: p`. gc gets "does not escape" by inlining it first. goc's walk
runs on the AST before any inlining and cannot see through `fn(fd, p)` where
`fn` is a function-typed parameter. **Irreducible** without an inliner ahead of
the walk or a whole-program resolution of function-valued parameters.

**G7 -- a stdlib summary the walk could not get through (11). VERDICT: gc is
right, one callee at a time.** `slices.Sort`, `sort.Ints`, `reflect.ValueOf`,
`(reflect.Value).Call`, `runtime.SetFinalizer`, `runtime/metrics.Read`,
`crypto/mlkem.NewDecapsulationKey768`, `(*zip.Writer).CreateHeader`. Each is one
callee whose body defeats the walk somewhere; they are not one cause and fixing
them is not one change. Reducible, slowly.

**G6 -- the callee is a func value, an interface method, or a literal the walk
places on the heap (8). VERDICT: gc is right where it devirtualises.** `f(arg,
...)` through a `var f func(...)` assigned once, `(cipher.Block).Encrypt`,
`(io.Writer).Write`. gc resolves these by devirtualisation, which goc, compiling
whole-program, could do better than gc -- but does not do at all in this walk.

**G10 -- a composite literal handed to a call the walk cannot follow (6).
VERDICT: gc is right.** The four `&http.Cookie{...}` in a `[]*http.Cookie`
passed to `(*cookiejar.Jar).SetCookies`, plus `&jpeg.Options{...}` and one more
cookie. gc's per-package summaries say the callee copies out of them. Reachable
in principle, deep in practice.

**G5 -- `new(T)` (5). VERDICT: gc is right; wrong lever.** `new(T)` and boxed
payloads are emitted as the neutral `HeapAlloc` candidate and decided by
`opt.LowerHeapAllocations`, not by the AST walk. These five are the IR pass's
answers and nothing in this branch's area moves them.

**G3 -- non-constant `make([]T, n)` (5). VERDICT: neither compiler frames these;
the line is not a real difference.** Measured against the host toolchain:
`make([]int, n)` with non-constant n gets `does not escape` from `-m` *and* a
`CALL runtime.makeslice`. `gcdiff` maps "does not escape" to Frame
unconditionally (`gcdiff.go:363`), so these five count as a difference when both
compilers allocate. **A measurement artifact.** I left the ruler alone rather
than change the join in the same branch that changes the compiler; narrowing it
to "not a make with a non-constant length" is a clean follow-up that would take
the count to 108.

**G12 -- an aggregate copied whole, or a map lookup key (5). VERDICT: goc is
over-conservative, reducible.** `copy := source` where source is `[4]*T` is a
copy, and `copyAliasesStorage` refuses arrays; `values[&mapPointerKey{...}]` is
a lookup key that `mapaccess` does not retain, and `nonEscapingAddressWithin`
has no index case. Both are small, and both need care about the assignment
direction (a map *assignment* key is retained).

**G8 -- a recursive callee has no summary (3). VERDICT: gc is right.**
`parameterDoesNotEscape` breaks its cycle by answering "escapes". A fixpoint
would answer these; the cycle-breaking answer is the safe direction and the walk
has no fixpoint.

**G9 -- a variadic parameter has no summary (3). VERDICT: gc is right.** The
documented gap at `goc/compile.go`'s `parameterDoesNotEscape`: the summary
refuses variadic positions because an argument there is an element of a slice
the callee builds. Still open, and it is what `logattrs/6-attr` costs in the
slog table.

**G11 -- a method value (2). VERDICT: gc is right.** `record := recorder.Add`
makes a closure over the receiver; the receiver escapes exactly when the closure
does, and the walk answers only the immediately-called case.

### The 5 groups this branch fixed (21 lines)

    F1  a string <-> []byte/[]rune conversion copies                    7 lines
    F3  ranging over an object does not carry it anywhere               6 lines
    F4  an address that is an element of a struct or array literal      5 lines
    F5  append reads its `xs...` operand and does not keep it           2 lines
    F2  `var x T = y`; interfaces alias; i.(T) and p.f keep going       1 line

Each has an allocation-count row in `goc/testdata/allocation_counts.go` measured
under both compilers, and each row is 1 allocation per call before its change
and 0 after -- verified by removing that one change and re-running.

**G1 -- maps have no frame-allocated header (38)**

```
runtime_array_map_key.go                          4  makemap    table := map[[3]int]string{
runtime_assign_target_forms.go                   42  makemap    counts := map[string]int{}
runtime_assign_target_forms.go                   47  makemap    counts = map[string]int{}
runtime_assign_target_forms.go                   59  makemap    counts := map[string]int{"k": 10}
runtime_assign_target_forms.go                   97  makemap    counts := map[string]int{"a": 3}
runtime_assign_target_forms.go                  125  makemap    texts := map[int]string{1: "one"}
runtime_core_types.go                            21  makemap    buckets := map[string]*bucket{
runtime_interface_comparable_map.go               9  makemap    table := map[interface{}]int{
runtime_loopvar_range.go                         46  makemap    counts := map[string]int{"a": 1, "b": 2, "c": 3}
runtime_map_clear.go                              4  makemap    counts := map[string]int{
runtime_map_delete_iter_gc.go                    11  makemap    values := make(map[int]*payload)
runtime_map_growth_gc.go                         12  makemap    values := make(map[int]*entry)
runtime_map_interface_keys_gc.go                 11  makemap    values := make(map[any]int)
runtime_map_interface_values_gc.go               11  makemap    values := make(map[int]interface{})
runtime_map_iter_delete_insert.go                 4  makemap    values := map[int]int{
runtime_map_pointer_keys.go                      12  makemap    values := map[*mapPointerKey]int{
runtime_map_pointer_values_gc.go                 11  makemap    values := make(map[int]*mapPointerBox)
runtime_map_range_delete_all.go                   4  makemap    values := make(map[int]int)
runtime_map_range_insert_growth.go                4  makemap    values := make(map[int]int)
runtime_map_struct_value_replace.go               9  makemap    items := map[string]mapStructValue{
runtime_println_operand_separation.go            88  makemap    assertShape("map", capture(func() { println(map[int]
runtime_range_target_forms.go                    78  makemap    counts := map[string]int{}
runtime_range_target_forms.go                   227  makemap    single := map[int]string{7: "x"}
runtime_range_target_forms.go                   236  makemap    weights := map[int]int{1: 10, 2: 20, 3: 30}
runtime_range_target_forms.go                   243  makemap    source := map[int]int{7: 9}
runtime_range_target_forms.go                   244  makemap    destination := map[int]int{}
runtime_range_target_order.go                    69  makemap    counts := map[string]int{}
runtime_range_target_order.go                   102  makemap    runes := map[int]rune{}
runtime_reflect_make_values.go                   11  makemap    mapType := reflect.TypeOf(map[string]int{})
runtime_reflect_map_slice.go                      6  makemap    mapType := reflect.TypeOf(map[string]int{})
runtime_string_rune_map.go                        4  makemap    counts := map[rune]int{}
stdlib_encoding_pem.go                            8  makemap    Headers: map[string]string{
stdlib_maps_keys.go                               6  makemap    scores := map[string]int{
stdlib_maps_keys.go                              12  makemap    seen := map[string]bool{}
stdlib_maps_slices.go                             9  makemap    scores := map[string]int{
stdlib_maps_slices.go                            14  makemap    clone := map[string]int{}
stdlib_net_mail_textproto.go                     20  makemap    header := textproto.MIMEHeader{}
stdlib_url_values_encode.go                       6  makemap    values := url.Values{}
```

**G4 -- a value boxed into an interface (17)**

```
allocation_counts.go                            121  convT64    func boxSmallInt() { sinkInt = takeAny(theInt) }
allocation_counts.go                            124  convT64    func boxLargeInt() { sinkInt = takeAny(theLargeInt) 
allocation_counts.go                            127  convT64    func boxBool() { sinkInt = takeAny(theBool) }
allocation_counts.go                            130  convT64    func boxFloat64() { sinkInt = takeAny(theFloat) }
allocation_counts.go                            133  newobject  func boxString() { sinkInt = takeAny(theString) }
interface_slice_equality.go                       6  convT64    if value != values[0] {
interface_slice_equality.go                       9  convT64    if value == any(2) {
runtime_debug_gc_controls.go                     15  newobject  values := make([]*int, 0, 128)
runtime_debug_gc_controls.go                     32  newobject  runtime.KeepAlive(values)
runtime_panic_stack_gc.go                        19  newobject  runtime.KeepAlive(scratch)
runtime_panic_stack_recover_gc.go                19  newobject  runtime.KeepAlive(scratch)
runtime_reflect_make_values.go                    6  newobject  sliceType := reflect.TypeOf([]int{})
runtime_reflect_map_slice.go                     19  newobject  sliceType := reflect.TypeOf([]int{})
runtime_slice_pointer_append_gc.go               10  newobject  values := make([]*record, 0, 4)
runtime_slice_pointer_append_gc.go               25  newobject  runtime.KeepAlive(values)
stdlib_signal_during_gc.go                       23  newobject  values := make([]*int, 1024)
stdlib_signal_during_gc.go                       29  newobject  runtime.KeepAlive(values)
```

**G2 -- the Read buffer: gc inlines before escape analysis, goc cannot (10)**

```
runtime_println_operand_separation.go           168  newobject  buffer := make([]byte, 8192)
runtime_stack_scan_syscall.go                   115  newobject  buffer := make([]byte, 4)
stdlib_netpoll_pipe_afterfunc_close.go           24  newobject  buffer := make([]byte, 1)
stdlib_netpoll_pipe_close_unblocks_read.go       17  newobject  buffer := make([]byte, 1)
stdlib_netpoll_pipe_deadline.go                  23  newobject  buffer := make([]byte, 1)
stdlib_netpoll_pipe_past_deadline.go             23  newobject  buffer := make([]byte, 1)
stdlib_netpoll_stress_pipe_close_churn.go        17  newobject  buffer := make([]byte, 1)
stdlib_netpoll_stress_pipe_deadline_reset.go     22  newobject  buffer := make([]byte, 1)
stdlib_os_file_roundtrip.go                      26  newobject  buffer := make([]byte, 10)
stdlib_os_pipe_goroutine_close.go                24  newobject  buffer := make([]byte, 8)
```

**G7 -- a stdlib summary the walk could not get through (11)**

```
adler32_marshal_loop.go                          21  newobject  appendedState, err := hash.(encoding.BinaryAppender)
runtime_closure_captured_string.go               94  newobject  assignBytes([]byte{'a', 'z'})
runtime_finalizer_basic.go                       22  newobject  runtime.SetFinalizer(value, func(item *finalizable) 
runtime_reflect_call_aggregate_matrix.go         70  newobject  results := reflect.ValueOf(function).Call([]reflect.
runtime_reflect_method_metadata.go               27  newobject  results := method.Func.Call([]reflect.Value{
runtime_reflect_set_fields.go                    11  newobject  payload := &reflectSetPayload{}
runtime_reflect_value_call.go                    11  newobject  arguments := []reflect.Value{
stdlib_crypto_mlkem.go                            9  newobject  seed := make([]byte, mlkem.SeedSize)
stdlib_maps_slices.go                            25  newobject  values := []int{5, 1, 3, 2, 4}
stdlib_slices_string_sort.go                      6  newobject  keys := []string{"gamma", "alpha", "beta"}
stdlib_sort_search_slice.go                       6  newobject  values := []int{9, 1, 5, 3, 7}
```

**G6 -- the callee is a func value, an interface method, or an escaping literal (8)**

```
runtime_interface_method_gc.go                   23  newobject  value := scorer(&scoreBox{
runtime_interface_method_gc.go                   25  newobject  next:  &scoreBox{value: 25},
runtime_interface_to_interface.go                29  newobject  var combined interfaceReadCloser = &interfaceResourc
runtime_println_operand_separation.go            82  newobject  assertShape("empty slice", capture(func() { println(
runtime_reflect_value_indirect_call.go           28  newobject  instruction := &reflectValueInstruction{field: 5}
runtime_reflect_value_indirect_call.go           29  newobject  state := &reflectValueState{}
runtime_timer_callback_shape.go                  16  newobject  box := &callbackBox{value: 10}
stdlib_compress_zlib_lzw.go                      72  newobject  lowWidthInput := []byte{0, 1, 2, 3, 0, 2, 1, 3, 3, 2
```

**G10 -- a composite literal handed to a call the walk cannot follow (6)**

```
stdlib_http_cookiejar.go                         17  newobject  {Name: "host", Value: "api", Path: "/"},
stdlib_http_cookiejar.go                         18  newobject  {Name: "account", Value: "private", Path: "/account"
stdlib_http_cookiejar.go                         19  newobject  {Name: "secure", Value: "yes", Path: "/", Secure: tr
stdlib_http_cookiejar.go                         20  newobject  {Name: "expired", Value: "gone", Path: "/", Expires:
stdlib_http_redirect_keepalive.go                24  newobject  http.SetCookie(response, &http.Cookie{
stdlib_image_jpeg_roundtrip.go                   18  newobject  if err := jpeg.Encode(&encoded, source, &jpeg.Option
```

**G5 -- new(T): the decision is the IR pass’s, not the walk’s (5)**

```
gc_struct.go                                     25  newobject  root := new(record)
runtime_println_operand_separation.go            84  newobject  pointer := new(int)
runtime_range_target_forms.go                   369  newobject  pointer := new(string)
runtime_span_metadata_barrier.go                 47  newobject  root := new(spanRecord)
stdlib_math_big_rat_int.go                       15  newobject  delta := new(big.Rat).Sub(denominator, numerator)
```

**G3 -- non-constant make([]T, n): both compilers heap-allocate (5)**

```
runtime_copy_interface_slice_gc.go               23  makeslice  destination := make([]interface{}, len(source))
runtime_gc_mark_workers.go                      117  makeslice  expected := make([]int, len(retained))
runtime_loopvar_range.go                        112  makeslice  values := make([]string, 0, len(closures))
stdlib_encoding_ascii85.go                        7  makeslice  encoded := make([]byte, ascii85.MaxEncodedLen(len(so
stdlib_encoding_ascii85.go                       10  makeslice  decoded := make([]byte, len(source)+4)
```

**G12 -- an aggregate copied whole, or a map lookup key (5)**

```
runtime_array_copy_pointer_gc.go                 11  newobject  {value: 3},
runtime_array_copy_pointer_gc.go                 12  newobject  {value: 5},
runtime_array_copy_pointer_gc.go                 13  newobject  {value: 7},
runtime_array_copy_pointer_gc.go                 14  newobject  {value: 11},
runtime_map_pointer_keys.go                      27  newobject  if values[&mapPointerKey{value: 17}] != 0 {
```

**G8 -- a recursive callee has no summary (3)**

```
runtime_panic_stack_gc.go                        23  newobject  retained := &marker{value: 42}
runtime_stack_growth.go                          24  newobject  root := &payload{
runtime_stack_growth.go                          26  newobject  next:  &payload{value: 25},
```

**G9 -- a variadic parameter has no summary (3)**

```
allocation_counts.go                            273  newobject  sinkInt = variadicInts(theInt, theInt)
runtime_range_target_forms.go                   137  newobject  observed += fmt.Sprintf("%v|", box.value)
runtime_range_target_forms.go                   144  newobject  observed += fmt.Sprintf("%v|", box.value)
```

**G11 -- a method value (2)**

```
runtime_defer_method_value_order.go              12  newobject  recorder := &deferMethodRecorder{}
runtime_method_value_gc.go                       16  newobject  counter := &methodValueCounter{value: 17}
```

Total: 113


## The 21 lines this branch moved into a frame

```
bytes_grow_allocs.go                             12  newobject  startSizes := []int{0, 100, 1000, 10000, 100000}
bytes_grow_allocs.go                             13  newobject  growSizes := []int{10000, 100000}
runtime_append_self_overlap.go                    4  newobject  values := []int{1, 2, 3, 4}
runtime_copy_string_to_bytes.go                   4  newobject  buffer := []byte{'x', 'x', 'x', 'x', 'x'}
runtime_goroutine_entry_stack_map.go             88  newobject  next:  &link{index: index + 1},
runtime_interface_pointer_equality_gc.go         10  newobject  node := &interfacePointerEqualityNode{value: 42}
runtime_keepalive_stack_root.go                  61  newobject  next:  &keptItem{index: index + 1},
runtime_many_goroutines_gc.go                    18  newobject  next:  &item{index: index + 1},
runtime_range_target_forms.go                    46  newobject  letters := []string{"a", "b", "c"}
runtime_range_target_order.go                   160  newobject  letters := []string{"a", "b", "c"}
runtime_scheduler_gc_churn.go                    29  newobject  next:  &schedulerGCNode{value: round},
runtime_slice_copy_overlap.go                     4  newobject  values := []int{1, 2, 3, 4, 5, 6}
runtime_stack_scan_syscall.go                   101  newobject  buffer := make([]byte, 4)
runtime_unsafe_struct_field.go                   15  newobject  next:  &unsafeFieldNode{left: 13, right: 17},
stdlib_archive_zip_roundtrip.go                  74  newobject  if !bytes.Equal(raw, []byte{1, 3, 5, 7, 9}) {
stdlib_bytes_reader_unread.go                    18  newobject  buffer := make([]byte, 3)
stdlib_crypto_des_rc4.go                         42  newobject  rc4Ciphertext := make([]byte, len("Plaintext"))
stdlib_encoding_hex.go                            6  newobject  source := []byte{0, 1, 2, 10, 15, 16, 255}
stdlib_maps_slices.go                            31  newobject  numbers := []int{1, 2, 3, 4}
stdlib_netpoll_udp_loopback.go                   21  newobject  buffer := make([]byte, 8)
stdlib_strings_reader_seek.go                    15  newobject  buffer := make([]byte, 3)
```

## What is irreducible, and why

- **G2, the Read buffer (10).** Irreducible in this walk. gc's answer comes from
  inlining, and its own un-inlined summary for the same function agrees with
  goc. This is a pipeline-position difference, not a missing rule.
- **G3, non-constant makes (5).** Not a difference at all. Both compilers call
  `runtime.makeslice`; the join reads gc's "does not escape" as "frame".
- **G8, recursion (3).** Irreducible without a fixpoint. The cycle-breaking
  answer is the safe one.
- **G1, maps (38).** Reducible, and the biggest thing left, but it is a
  representation change with a GC-safety obligation, not an analysis fix.
- **G4, boxing (17).** Reducible only after interface payloads have a frame
  form; fixing the walk's half alone would let a frame-charged source pair with
  a heap payload.
- Everything else (G5, G6, G7, G9, G10, G11, G12 -- 40 lines) is reducible by
  ordinary work, one cause at a time, with no representation change needed.

So of the 113: **15 are irreducible or not real** (G2 + G3), **55 need a
representation change first** (G1 + G4), and **43 are ordinary analysis work**.

## Compile time

The escape walk asks a callee's summary once per caller and once per argument
position and rebuilt that callee's parent map every time. Compiling
`testdata/allocation_counts.go` asked 8812 such questions with 734 distinct
answers; the cache removes 8078 of the 8812 rebuilds.

Measured: ~0.1s of CPU and about 2% of wall on that program (6.43s -> 6.28s
median of five), and inside the noise on `stdlib_http_redirect_keepalive.go`
(75.3s vs 75.6s user, 33.0s vs 32.8s wall), whose compile is dominated by
everything else. That is less than the 60%-of-7.1% the brief expected, and the
profile says why: after the cache, `astParents` is 0.14s cumulative of a 20s
CPU profile, and what is left is the per-function parent maps that lowering
needs and that were never the repeated ones. Memoising the *answers* rather
than the maps is the remaining move there, and it is not free: an answer
produced while a `checking` cycle was broken is not the same answer as one
produced from a clean stack, so it would need a "did this computation hit a
cycle" flag before anything could be cached.
