# Wave 7 merge gate

Integration branch: **`integration/wave7-gate`** (created fresh off `main` = `cae1430`;
no collision — no `wave7`-named branch existed on origin before this run).

Merged in the required order:

| # | branch | tip | merge | kind |
|---|---|---|---|---|
| 1 | `ccwork/arm64-code-alignment` | `3743a81` | fast-forward | **CODE CHANGE** — changes emitted instruction layout |
| 2 | `ccwork/parity-reasons` | `c4210af` | `4427670` (2 conflicts) | **CODE CHANGE** — escape-analysis fixes |

Merge conflicts and how they were resolved:

* `CCWORK_REPORT.md` — both branches wrote their own report here. Resolved by
  replacing with this gate report; both branch reports remain intact in their
  own commits (`git show origin/ccwork/arm64-code-alignment:CCWORK_REPORT.md`).
* `goc/testdata/crypto_signing_bench_baseline.txt` — both branches re-baselined
  it. Resolved to branch 1's side **provisionally**; the file is regenerated
  from the merged tree in the baseline section below, so no side was picked.

Merged tree builds clean: `go build ./...` exit 0.

## Branch 1 — is it a code change or a report? **IT IS A CODE CHANGE.**

`ccwork/arm64-code-alignment` is not report-only. It adds `arm64/align.go` (172
lines) and changes the emitter, the linker and the object writers. The shipped
default is live, not opt-in:

```go
const (
        defaultFuncAlign         = 32
        defaultLoopFunctionsOnly = true
        defaultLoopAlign         = 0
)
```

so **every function that contains a backward branch now has its entry padded to
a 32-byte boundary**, with `nop` (`d503201f`) as the filler. The policy is
plumbed end to end:

* `arm64/mc.go` — `loopHeaders()` computes "some predecessor is laid out at or
  after this block", sets `machineCode.hasLoop`; `compileToObjectWithBundle`
  calls `alignText(o.Text, layout.alignFor(code.hasLoop))` before taking each
  function's base.
* `obj/elf.go` — new `Object.TextAlign`; `obj/dynamic.go` aligns the text
  segment offset to it; `link/link.go` maxes it across objects and pads at the
  merge point (otherwise per-object padding aligns against nothing).
* `cmd/goc/packcache.go` — the layout identity is mixed into the runtime pack
  cache key, so a pack built under one policy cannot be reused under another.
  This matters: the policy is environment-overridable
  (`GOC_FUNC_ALIGN`, `GOC_LOOP_ALIGN`, `GOC_ALIGN_LOOP_FUNCS_ONLY`,
  `GOC_TEXT_PAD`) and is therefore not covered by the compiler binary's hash.
* `arm64/a64/disasm.go` — the disassembler learns `nop`, which the backend now
  emits.

**Consequence for this gate, as anticipated in the brief:** emitted code differs
from `main` by design everywhere, `.text` grows, and every generated baseline
that records addresses or sizes has to be regenerated rather than diffed against
a side. Byte-identical determinism is unaffected — the padding is a pure
function of the code, not of worker order.

Branch 1 also ships a measurement corpus under `goc/testdata/placement_bench/`
(8 programs, a sweep driver, 4 856 lines of raw TSV results) and its analysis.

## Branch 2 — code change

`ccwork/parity-reasons` is not report-only either: `opt/escape.go` (+101),
`goc/compile.go` (+287), `internal/gcdiff/{reasons,reasonreport}.go`, and it
moves four generated baselines. Three substantive fixes:

1. a 64 KB bound on the backing array a fixed-capacity `make` may be given in
   the frame, matching `cmd/compile`'s `MaxImplicitFrameSize` (a 200 000-int
   `make` was taking 1.6 MB of the caller's frame and overflowing the stack);
2. the escape walk gains a "what does this value *hold*" question distinct from
   "where does it *go*" (new `gen.escapeAsksWhatTheValueHolds`);
3. a map **lookup** key is not retained but a map **write** key is
   (`mapLookupKeyIsNotRetained`, `deleteBuiltinRetainsNothing`).

---

_Gate in progress; items below are filled in as each run is watched to exit._

## Item 3 — `make test-unit` — **PASS**

Merged tree, watched to exit (12 s, exit 0), 37 packages. Every package that has
tests reports `ok` with a real elapsed time; **0 `(cached)` lines**. Notably
`arm64` (9.331 s), `link`, `obj` and `opt` — the four packages branch 1 touched —
all pass with the new alignment policy live.

## Item 1 — `go test -timeout 40m -parallel 10 ./goc/...` — **2 FAILURES on the merged tree**

Both trees run detached with `-v -count=1`, each waited to exit. **0 `(cached)`
lines in either log.**

| tree | exit | wall | result |
|---|---|---|---|
| merged `4427670` | **1** | 1021 s | `FAIL github.com/evanphx/cg12/goc 1020.218s` |
| `main` control `cae1430` | 0 | 1021 s | `ok  github.com/evanphx/cg12/goc 1017.203s` |

`main` is a **measured** control, not a quoted figure: it was run here, in its
own worktree, at the same time, and it passes.

### Subtest census: 698 vs 695 — **+3, all additions, nothing lost**

`comm` on the sorted `=== RUN` name sets. Nothing present on `main` is missing
from the merged tree. The three new names and the commits that add them:

| new subtest | commit | branch |
|---|---|---|
| `TestObjectsTooLargeForAFrameGoToTheHeap` | `95ea1a7` | 2 |
| `TestPointerReadBackOutOfAFrameLocalContainerEscapes` | `c765e09` | 2 |
| `TestMapLookupKeyStaysInTheFrameAndAMapWriteKeyDoesNot` | `b0768a6` | 2 |

All three are branch 2's, one per fix, matching `goc/escape_test.go` (+197).
Branch 1 adds no test to this package.

### FAILURE 1 — `TestDeriveClassifiesEveryGenField` — **the fifth consecutive wave**

    derive_test.go:255: fullyPopulatedGen leaves [escapeAsksWhatTheValueHolds] zero,
    so derive's handling of those fields is untested; a new gen field has to be
    given a non-zero value there and classified in wholeCompilationGenFields

**One field, named: `escapeAsksWhatTheValueHolds`** (`goc/compile.go:1725`,
added by branch 2's `c765e09`).

* **Does `derive` reset it? NO.** `derive()` (`goc/compile.go:1763`) copies the
  struct and then explicitly clears the per-function fields around it —
  `objectEscapeChecks` on the line above is set to `nil`, `resultLeakBody` on
  the line below is set to `nil` — and `escapeAsksWhatTheValueHolds`, sitting
  between them, is left alone. It is declared in the per-function block of the
  struct, not the whole-compilation block, so leaving it is inconsistent with
  its own placement.
* It is not in `wholeCompilationGenFields` either, and not in
  `fullyPopulatedGen()`. `goc/derive_test.go` is untouched by both branches.

Whether the leak is harmful in practice: the walk saves and restores the flag
itself (`goc/compile.go:4221-4223`, `saved := g.escapeAsksWhatTheValueHolds` /
`defer` restore), so within one generator it is balanced. The exposure is a
`derive()` taken while the flag is set — the derived generator inherits `true`
and every `parameterKey` it mints (`compile.go:2612`, `:4361`, `:4505`) carries
`holds: true`, which is a different cache key and a different answer. This gate
did not prove such a `derive()` happens; it proves the classification the test
demands was never made. **Not repaired here — this gate does not fix compiler
code.** It is a one-line classification decision that belongs in branch 2.

### FAILURE 2 — `TestEscapeShadowPlacement` — stale `escape_shadow_baseline.txt`

Both halves of the ratchet fire: **42 disagreement sites the baseline does not
list**, and **1 site the baseline lists that the run no longer produces**. This
is a listed regeneration target; the regenerated diff is reviewed under
"Generated baselines" below.

Run-level summary from the merged tree:

    front-end placements evaluated: 201899 (frame 177078, heap 24821)
    agree 184934; front frame -> IR heap 16016; front heap -> IR frame 949
    distinct front-end placement sites: 5703 (frame 3621, heap 2082)
    distinct disagreement sites: 810

### Attribution of both failures — **both are branch 2's, neither is branch 1's**

Two more worktrees, each at a branch tip, each running only the two failing
tests, watched to exit:

| tree | `TestDeriveClassifiesEveryGenField` | `TestEscapeShadowPlacement` | exit |
|---|---|---|---|
| `ccwork/arm64-code-alignment` `3743a81` | **PASS** | **PASS** (255.6 s) | 0 |
| `ccwork/parity-reasons` `c4210af` | **FAIL** | **FAIL** (254.8 s) | 1 |
| merged `4427670` | FAIL | FAIL | 1 |

Branch 2 alone reproduces both, with the identical message and the identical 42
observations. **The merge did not create either failure and branch 1 does not
contribute to either** — branch 2 ships them.

Why branch 2 did not see them: its own report's verification table runs
`TestFrameEscapeAudit`, `TestAllocationCensus`, `TestLoopAliasAudit`,
`TestCompilingTheSameSourceTwiceGivesTheSameModule`, `TestEscapeDiagnostic*`,
both capability arms and the GC reducer — but **not** `TestEscapeShadowPlacement`,
**not** `TestDeriveClassifiesEveryGenField`, and not a whole
`go test ./goc/...`, which is the run that catches both.

## Generated baselines — all eight regenerated from the merged tree

Every one regenerated on the merged tree (and the census also on the `main`
worktree as a control), each run watched to exit, all exit 0. No side was
picked; the tree was asked.

| baseline | regenerated by | diff vs what the merge committed |
|---|---|---|
| `alloc_census_baseline.txt` | `-update-alloc-census-baseline` | **none** (byte-identical) |
| `frame_escape_baseline.txt` | `-update-frame-escape-baseline` | **none** |
| `loop_alias_baseline.txt` | `-update-loop-alias-baseline` | **none** |
| `slog_allocations_baseline.txt` | `-slog-allocations -update-slog-allocations` | **none** |
| `escape_gc_differential.txt` | `-escape-gc-differential -update-escape-gc-differential` | **none** |
| `escape_shadow_baseline.txt` | `-update-escape-shadow-baseline` | **+21 / −1** — reviewed below |
| `escape_gc_reason_differential.txt` | `-escape-gc-reason-differential -update-…` | (below) |
| `crypto_signing_bench_baseline.txt` | `make bench-crypto-update` | (below, idle-box item) |

Seven of the eight the merge already had right. The one that moved is the one
neither branch regenerated.

### `escape_shadow_baseline.txt` — +21 / −1, and every line is branch 2's

The file records where goc's AST walk and the IR shadow analysis *disagree*; the
AST walk stays the placer, so a line here is a divergence between two analyses,
not a miscompile. The 21 additions are the mirror image of branch 2's own census
review, which is why they are explainable rather than mysterious:

* **12 × `crypto/internal/fips140/ecdh/cast.go:18:17` and `:24:16`, direction
  `heap → frame`** — branch 2's report lists these exact 12 sites as moving
  `frame → heap` in the census (the CAST's `privateKey`/`publicKey` byte slices
  stored into a `&PrivateKey{}`, six copies because the self-test is instantiated
  per init function). The front end now heaps them; the IR shadow still frames
  them. Same 12 sites, opposite column, as expected.
* **`crypto/x509/verify.go:375` and `os/user/lookup_unix.go:63`, `frame → heap`**
  — the two placements branch 2's `==`/`!=` gap gave back. The front end now
  frames them; the IR shadow has not learned the rule.
* **`testdata/runtime_map_pointer_keys.go:27:12, composite-literal,
  frame → heap, "store into non-local storage"`** — this is the `&mapPointerKey{value: 17}`
  in `values[&mapPointerKey{value: 17}]`, a map *lookup* key. Branch 2 taught the
  AST walk to frame it; the IR side (`opt/escape.go`) still calls a map index a
  store. Front end right, shadow behind.
* the remaining 6 (`os/exec`, `testing`, `bytes_replace_allocs` ×2,
  `stdlib_encoding_json_roundtrip`, `runtime_unsafe_struct_field`) are the same
  shape: one analysis moved and the other did not.
* **the single removal**, `time/zoneinfo_read.go:546:52 time.loadLocation` — a
  disagreement the run no longer produces, i.e. the two analyses now agree.

Assessment: this is a stale-baseline failure, not a placement regression. The
divergences are one-sided (the front end learned three rules the IR shadow has
not), the correctness-critical `frame → heap` half is the front end being *less*
conservative at exactly the two sites branch 2 proved retain nothing, and
`frame_escape_baseline.txt` — the audit that would catch a real frame-publication
— does not move at all. **It still means branch 2 shipped a red test.**

## Item 4 — `TestFrameEscapeAudit` — **182 entries, unchanged, no additions**

    main                                182 entries  md5 36b6c9a2…
    ccwork/arm64-code-alignment         182 entries  md5 36b6c9a2…
    ccwork/parity-reasons               182 entries  md5 36b6c9a2…
    merged 4427670                      182 entries  md5 36b6c9a2…
    regenerated on the merged tree      byte-identical to all of the above

Reference 182 met exactly. **Zero additions**, so there is nothing to justify and
nothing unsafe by this measure. Notably branch 1's alignment change does not move
it, and neither does branch 2's escape work.

## Item 10 — gc differential — **goc heaps what gc frames: 96. Reference met.**

Regenerated on the merged tree, byte-identical to what the merge committed.

    compared 399 of 403 corpus programs, 1861 census rows, 3511 gc decisions
    permissive (gc heaps, goc does not): 1467 lines
    pessimistic (goc heaps, gc does not): 399 lines

Confusion matrix (rows = goc's verdict, columns = gc's), merged tree:

      goc\gc      frame     heap    mixed   absent    total
      frame         189       30       14      193      426
      heap           96      580      172       81      929
      mixed          13       89       24       13      139
      absent        420     1286       24        0     1730
      total         718     1985      234      287     3224

**goc-heaps-what-gc-frames = 96**, the same 96 as `main`. Against the `main`
control the whole matrix moves by exactly one cell pair: `frame/heap` 31 → 30 and
`heap/heap` 579 → 580, i.e. one line goc used to frame that gc heaps is now
heaped by goc too. PERMISSIVE 1468 → **1467**; PESSIMISTIC 399 → **399**,
unchanged.

(Branch 2's prose says "the correctness-critical direction is unchanged at 1467
and the performance direction goes 399 → 400". Measured against a `main` control
here, it is the other way round: permissive moved 1468 → 1467 and pessimistic did
not move. Its committed file is right; only the sentence describing it is wrong.)

## Item 2 — capability matrix, both arms — **366/366 PASS in all three runs**

`-v` via `GOFLAGS`, each run detached and waited to exit, **0 `(cached)` lines**.

| run | tree | PASS | FAIL | SKIP | wall |
|---|---|---|---|---|---|
| `test-goc-status` | merged | **366** | **0** | 0 | 163.5 s |
| `test-goc-status-opt` (`-O`) | merged | **366** | **0** | 0 | 174.1 s |
| `test-goc-status` (control) | `main` `cae1430` | 366 | 0 | 0 | 123.7 s |

Sets, not counts: the 366 capability names are **identical** across all three
runs — `diff` clean merged-default vs merged-`-O`, and merged-default vs the
`main` control. Nothing added, removed, renamed or newly skipped. No regression;
the alignment change does not cost a single capability in either arm.

## Item 5 — allocation census twice — **STABLE, and byte-identical to the committed file**

Two independent `-update-alloc-census-baseline` runs on the merged tree (261 s,
272 s; both exit 0):

    run 1   md5 42c139d49aaa95863d242f2bc0411eb4   14501 rows
    run 2   md5 42c139d49aaa95863d242f2bc0411eb4   14501 rows
    committed on the merged tree                   42c139d4…   (git diff empty)

### Census composition — the delta composes exactly, **residue: none**

| tree | rows | md5 |
|---|---|---|
| `main` `cae1430` | 14499 | `8269d6f2…` |
| `ccwork/arm64-code-alignment` | 14499 | `8269d6f2…` (identical to `main`) |
| `ccwork/parity-reasons` | 14501 | `42c139d4…` |
| merged `4427670` | 14501 | `42c139d4…` (identical to branch 2) |

Regeneration on the `main` worktree reproduces `main`'s file byte for byte too,
so the control is measured, not quoted.

    merged (14501) − main (14499) = +2 = parity-reasons (14501) − main (14499)

Row-level: 19 lines in, 17 lines out, on both branch 2 and the merge — the same
19 and the same 17. Branch 1 is **placement-neutral**: an alignment change moves
addresses, not allocations, and the census confirms it rather than assuming it.
**Residue: none.**

## Item 6 — determinism — **byte-identical, both trees, with alignment live**

Four corpus programs × {default, `-O`}, each compiled twice through
`go run ./cmd/goc -o`, in each tree. **All 8 pairs byte-identical on the merged
tree; all 8 on `main`.** `TestParallelBackendIsByteIdenticalToSerial` (`./arm64`)
**PASSES** at workers = 1, 2, 3, 8, 64, 256 — the padding is a function of the
code, not of worker order, so parallel-vs-serial identity survives.

### The output does differ from `main`, by design, and the sizes moved

`runtime_map_pointer_keys.go`, default build:

| section | merged | `main` | delta |
|---|---|---|---|
| `.text` | 1 615 204 | 1 604 420 | **+10 784 (+0.67 %)** |
| `.debug_line` | 399 379 | 400 754 | −1 375 |
| `.data` | 3 750 112 | 3 750 120 | −8 |
| whole file | 6 900 336 | 6 901 720 | −1 384 |

`.text` grows +0.67 %, which is branch 1's claimed 0.72 % cost for the
loop-functions-only policy. The **whole file** gets slightly *smaller* only
because `.data` sits at a fixed file offset (1 638 424 in both) with slack ahead
of it that absorbs the whole `.text` growth; the file delta is then exactly
`−1375 − 8 = −1383` from the other two sections. Not a contradiction — the code
did get bigger.

Alignment is demonstrably live, not merely compiled in: of 3 020 `FUNC` symbols,
**1 088 (36.0 %) sit on a 32-byte boundary on the merged tree against 390
(12.9 %) on `main`**. Consistent with "functions containing a backward branch
only" rather than all of them.

## Item 7 — loop aliasing — **clean**

All on the merged tree, in one watched run (466 s, exit 0, 0 `(cached)`):

| check | result |
|---|---|
| `TestLoopAliasExpectationsMatchTheHostToolchain` | **PASS**, 6/6 programs match the host toolchain |
| `TestLoopBodyAllocationsAreDistinctPerIteration` | **PASS**, 12/12 subtests (6 programs × {default, `-O`}) |
| `TestLoopAliasAudit` | **PASS** |
| `loop_alias_baseline.txt` | 589 entries, **identical to `main`**, and regeneration on the merged tree reproduces it byte for byte |

`loop_alias_frame_local.go` — exactly **one** census row, and it is `frame`:

    testdata/loop_alias_frame_local.go:53:8  main.literalWithin  runtime.newobject  main_point  frame

**1 frame / 0 heap**, identical to `main` (same file, same line 12751).

## PORTABILITY CHECK — `escape_gc_reason_differential.txt` is portable. **PASS.**

Regenerated in two checkouts of the merged tree at deliberately different paths,
each run watched to exit:

    /home/evan/.ccwork/ws/wave7-gate/repo                                     (269 s)
    /home/evan/.ccwork/ws/wave7-gate/tmp/
        portability-second-worktree-at-a-noticeably-different-path            (224 s)

    760120 bytes each
    sha256 41b0531d3c1efa78caa199545718ffb46be447137eefe5dabea02fe1579d3d5f  both
    cmp: BYTE-IDENTICAL
    grep -c /home/evan : 0 in both

Both regenerations also reproduce the **committed** file byte for byte (`git
status` in the second worktree lists nothing for it), so all three agree.
`TestReasonPositionsAreRepositoryRelative` — the cheap half of the guarantee —
**PASSES** on the merged tree. Branch 2 touches `internal/gcdiff/reasons.go` and
the taxonomy comment but not `relativeToRepository`, and the measurement confirms
the fix survives the merge. **No regression: the ratchet works outside the
directory that made it.**

Run figures unchanged by the merge: 399 programs, 1103 goc rules, 2201 gc
explanations joined; agree-on-placement/disagree-on-reason **309**;
disagree-on-placement/agree-on-reason **84**.

## Item 12 — `goc -m` in all three forms — **PASS, and inert when off**

`goc/testdata/runtime_map_pointer_keys.go`, all four builds exit 0:

| invocation | diagnostic lines on stderr | binary vs the `-m`-off build |
|---|---|---|
| (none) | 0 | — |
| `-m` | 8 | **byte-identical** |
| `-m=1` | 8 | **byte-identical** |
| `-m=2` | 12 | **byte-identical** |

Bare `-m` is byte-identical to `-m=1` on stderr. `-m=2` adds the `from:` and
`at:` provenance lines:

    runtime_map_pointer_keys.go:10:11: main_mapPointerKey escapes to heap
            front end: composite-literal in main.main
            rule: first is used here in a way the walk cannot prove keeps it local
            from: first, declared at runtime_map_pointer_keys.go:10:2
            at:   runtime_map_pointer_keys.go:13:3

`-m` changes nothing the compiler emits — **byte-identical-inert confirmed** — and
the diagnostic independently corroborates branch 2's map-key rule: the two map
*literal* keys at 10:11 and 11:12 escape, and the map *lookup* key at 27:12 is
not reported, i.e. it stays in the frame.

## Ratchet re-verification after regeneration — all green

One watched run on the merged tree, no `-update` flags, 466 s, exit 0, 0 `(cached)`:

    TestAllocationCensus                     PASS (229.07s)
    TestFrameEscapeAudit                     PASS
    TestEscapeShadowPlacement                PASS   <- was the failure; baseline regenerated
    TestEscapeDifferentialAgainstGC          PASS (10.84s)
    TestEscapeReasonDifferentialAgainstGC    PASS (176.90s)
    TestReasonPositionsAreRepositoryRelative PASS
    TestLoopAliasAudit                       PASS
    TestLoopAliasExpectationsMatchTheHostToolchain      PASS (6 subtests)
    TestLoopBodyAllocationsAreDistinctPerIteration      PASS (12 subtests)
    TestSlogAllocationsAgainstGC             PASS (18.15s)

## Item 8 — GC reducer, 20× at `GOGC=10` and default, both trees — **0/20 everywhere**

Idle box (1-minute load average 0.77–3.10 across the runs; every other job in
this gate had exited). `GOMAXPROCS=3`, serial, 180 s timeout per run. A run
counts as a pass only if it exits 0 **and** prints exactly `type mask padding ok`.

| tree | `GOGC=10` | default `GOGC` |
|---|---|---|
| merged `4427670` | **0/20 failures** | **0/20 failures** |
| `main` `cae1430` (control) | **0/20 failures** | **0/20 failures** |

80 runs, zero failures. The control reproduces `main`'s stated 0/20 at both
settings, so the branch result is measured against a control that behaved.
Alignment does not disturb the stack scan.

## Item 9 — slog benchmark, every row against gc — **30/32 at parity. Reference met.**

`slog_allocations_baseline.txt` regenerated on the merged tree: **byte-identical
to the committed file, and byte-identical to `main`'s and to branch 2's.**
`TestSlogAllocationsAgainstGC` PASSES. 32 cases, host toolchain go1.26.1,
iterations=2000 rounds=5.

**30 of 32 rows are at parity on a/op.** The two that are not, and both are goc
ahead of gc, not behind:

| case | goc a/op | gc a/op | goc B/op | gc B/op |
|---|---|---|---|---|
| `info/3-attr-large-ints` | 1.00 | 3.00 | 128.0 | 24.0 |
| `json/kv-4-pairs` | 1.00 | 2.00 | 176.0 | 24.0 |

Neither branch moves this file by a single byte, so the two known rows are the
same two rows, unchanged. No regression.

## Item 11 — `make bench-crypto` — **PASS, after re-baselining on the merged tree**

Read the triage note first (`goc/cryptobench_test.go:168`, "The third cause"),
which is now partly obsolete by its own branch: it says a movement here has three
causes, the third being that the code did not change and *moved*, and that
branch 1's alignment is the fix for that third cause.

### Neither side's committed baseline is right for the merged tree

Three `bench-crypto-update` measurements on the merged tree, idle box, against
each side's committed numbers and the 0.04 tolerance:

| case | branch 1 base | band | merged ×3 | in branch 1's band? | in branch 2's? |
|---|---|---|---|---|---|
| `p256/sign-verify` | 45.8670 | 44.03–47.70 | 46.6644, 45.7973, 45.9423 | yes | yes |
| `p256/verify` | 33.8050 | 32.45–35.16 | 35.3334, 34.3805, 34.0769 | **no** (run 1) | **no** (2 of 3) |
| `p384/sign-verify` | 40.0934 | 38.49–41.70 | 40.5753, 40.5258, 39.9392 | yes | yes |
| `rsa2048/sign-verify` | 12.1153 | 11.63–12.60 | 12.7483, 12.5562, 12.4187 | **no** (run 1) | yes |

The merge conflict resolution (branch 1's side) was therefore **not** load-bearing
and was also not correct: `make bench-crypto` on the merged tree passes or fails
depending on the run. So the file was regenerated from the merged tree — one
`bench-crypto-update` run, the one closest to the three-run mean — and committed
as `d044ea3`:

    p256/sign-verify              45.7973       1.6287     2504982432       54456117
    p256/verify                   34.3805       1.1654     1880514637       38964849
    p384/sign-verify              40.5258       2.8764     2216646286       96175020
    rsa2048/sign-verify           12.5562       0.5933      686786950       19836767

**`make bench-crypto` then passes 3/3** against them (watched, exits 0/0/0).

### The `main` control, measured not quoted

`main` in its own worktree, same box, same session, three runs: **all four cases
inside `main`'s own committed band**, so the control is healthy and `main`'s
committed baseline is not stale.

| case | merged mean | `main` mean | delta |
|---|---|---|---|
| `p256/sign-verify` | 46.1347 | 48.1714 | **−4.23 %** |
| `p384/sign-verify` | 40.3468 | 41.1333 | −1.91 % |
| `p256/verify` | 34.5969 | 35.1094 | −1.46 % |
| `rsa2048/sign-verify` | 12.5744 | 12.7579 | −1.44 % |

Faster on all four, consistent with branch 1's "4–6 % faster" claim. **Movement
was expected and it is downward** — this is not a regression in either direction
that the instrument gates on.

### Did the spread get tighter? **Yes, measurably, but by less than branch 1 reports.**

Branch 1's whole argument is that alignment makes the number stop depending on
where the code lands, so I measured that directly rather than quoting it, using
branch 1's own `GOC_TEXT_PAD` knob to shift `.text` and `GOC_FUNC_ALIGN=0` to
switch the policy off inside the same tree. `p256/sign-verify` index, pad
K ∈ {0, 8, 16, 24}, one build and one measurement per point:

| policy | K=0 | K=8 | K=16 | K=24 | range | spread |
|---|---|---|---|---|---|---|
| `GOC_FUNC_ALIGN=32` (shipped) | 46.7196 | 45.7191 | 46.4663 | 46.9874 | 1.268 | **2.73 %** |
| `GOC_FUNC_ALIGN=0` (as `main`) | 45.1079 | 47.0310 | 45.5269 | 47.0695 | 1.962 | **4.25 %** |

**The spread got tighter — 4.25 % → 2.73 %, a ~35 % reduction.** Direction
confirmed. But two honest caveats, because this is a timing instrument:

* This box's run-to-run noise today is much higher than the 0.08 % floor branch
  1's report claims. Rebuilding and re-measuring the *same source* three times
  gave 46.6644 / 45.7973 / 45.9423 on the merged tree — a 1.88 % range — and
  47.9774 / 48.2784 / 48.2584 on `main` (0.62 %). The aligned sweep's 2.73 % is
  only ~1.5× that noise, so most of what remains in the aligned column is
  measurement, not placement.
* I therefore **cannot reproduce branch 1's 6.1 % → 0.4 %**. My 4-point sweep is
  much coarser than theirs (they swept K finely with repetitions) and my noise
  floor is 20× theirs. What I can say from my own measurement is: tighter, in the
  claimed direction, by a factor of about 1.6 rather than 15.

**Attribution of the −4.23 %: branch 1.** It is the only branch that changes code
generation, and turning its policy off inside the merged tree
(`GOC_FUNC_ALIGN=0`) moves `p256/sign-verify` back into the 45.1–47.1 unaligned
range that `main` occupies.

## ROOT ARTEFACTS — **clean, nothing new**

`git ls-files` at the repo root, merged tree, after every run in this gate:

    AGENTS.md  AMD64_PARITY_PLAN.md  CCWORK_REPORT.md  .gitignore
    GO_INTEGRATION_PLAN.md  go.mod  go.sum  Makefile  README.md  RUNTIME_PLAN.md
    cg12  cs.trace  RUNTIME_PLAN.md.orig  viz

Everything is source or documentation except the four the brief says to leave —
`cg12` and `viz` (ELF aarch64 executables), `cs.trace`, `RUNTIME_PLAN.md.orig`.
**No new goc-built binary at the root, and `git diff --diff-filter=A main...HEAD`
adds no root file at all.** `git status` is clean after the whole gate.

Branch 2 cleaned up after itself mid-branch: `c4210af` deletes `p1` (6 996 328 B),
`size` (7 022 248 B) and `size.s`, three probe artefacts an earlier commit on that
same branch had written into the root with a default `-o`. They are added and
removed inside the branch, so they never reach the merge.

### One non-source file the merge does add, outside the root

    goc/testdata/placement_bench/__pycache__/sweep.cpython-312.pyc   9 466 B

Byte-compiled CPython 3.12 output for `sweep.py`, committed by branch 1's
`9a76853`. It is build output, not source or documentation, it is not covered by
`.gitignore` (which has no `__pycache__` or `*.pyc` rule), and it will be
regenerated and go stale the moment anyone runs `sweep.py`. Outside the root, so
outside the letter of the artefact check — **flagged, not removed**, since this
gate does not edit branch content. A `__pycache__/` line in `.gitignore` and a
`git rm --cached` is the fix.
