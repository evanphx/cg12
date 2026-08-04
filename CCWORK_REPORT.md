# Wave 7 fix verification — `integration/wave7-fix` (6034f73)

Verification only. No compiler code was changed on this branch by this job.

| ref | commit |
|---|---|
| `integration/wave7-fix` (branch under test) | `6034f73` |
| `integration/wave7-gate` (its parent, the already-gated tree) | `01b0cbf` |
| `main` (control) | `cae1430` |

`git ls-remote` confirms `integration/wave7-gate` = `01b0cbf` = `6034f73^`, so the
gate tree is exactly the parent commit and the diff under test is the whole of
commit 1 + commit 2:

```
 .gitignore                                            |   4 ++++
 goc/compile.go                                        |   5 +++++
 goc/derive_test.go                                    |   1 +
 goc/testdata/placement_bench/__pycache__/sweep...pyc  | Bin 9466 -> 0 bytes
```

Working branch for this job: `ccwork/wave7-fix-verify`. Nothing pushed to `main`.

---

## THE HEADLINE QUESTION — does commit 1 change emitted code? **NO. Not one byte.**

**424 corpus programs × {default, `-O`} = 848 fully linked binaries, every one
byte-identical between `integration/wave7-fix` and `integration/wave7-gate`.**

### How it was measured, and why the comparison is clean

`goc` bakes its own repository path into the binary at build time
(`goc/source_import.go:333`, `runtime.Caller(0)` → `<tree>/stdlib`), so compiling
the corpus in two different worktrees would differ in embedded paths for reasons
that have nothing to do with the change. To isolate the change and only the
change, **both compilers were built inside one worktree**
(`.../tmp/gate`, at `01b0cbf`):

    go build -o bin/goc-gate ./cmd/goc                  # tree as-is  = wave7-gate
    git checkout 6034f73 -- goc/compile.go
    go build -o bin/goc-fix  ./cmd/goc                  # + the 5-line reset = wave7-fix

Both binaries therefore bake the *same* stdlib root, read the *same* stdlib
bytes, and were run from the *same* cwd on the *same* input paths. The two
binaries do differ (`cmp` → byte 1457). Each got its own runtime pack: the pack
cache key hashes the compiler executable's bytes (`cmd/goc/packcache.go:71-75`),
so a stale pack cannot mask a codegen difference, and the pack itself — i.e. the
compiled vendored stdlib — is inside the comparison, not outside it.

Every program was compiled to a **fully linked executable** (not `-c`), twice per
compiler config, and `sha256sum`-compared:

| set | programs | comparisons | `SAME` | `DIFF` | compile failure |
|---|---|---|---|---|---|
| `goc/testdata/*.go` | 403 | 806 | **806** | **0** | 0 |
| `goc/testdata/*/**.go` (the multi-file benches: `crypto_signing_bench`, `slog_allocations`, `placement_bench`, `escape_gc_differential`) | 21 | 42 | **42** | **0** | 0 |
| **total** | **424** | **848** | **848** | **0** | **0** |

Both sweeps exit 0 with empty `stderr`.

**The crypto signing benchmark's own program is byte-identical**, which matters
for item 11 below: `goc/testdata/crypto_signing_bench/main.go` at `goc -O` —
exactly what `make bench-crypto` builds — is
`93d37e76300d0a4db2d0f068117eea1ddf1faf9f11a3326cc67011fc4990f208` under both
compilers.

**Positive control — the comparison can see a difference.** The same program
compiled by the same `goc-gate` binary with `GOC_FUNC_ALIGN=0` versus the shipped
`32` gives different hashes:

    ed93d774…  loop_alias_frame_local, GOC_FUNC_ALIGN=32 (shipped)
    cf53d0ab…  loop_alias_frame_local, GOC_FUNC_ALIGN=0

so 848/848 `SAME` is a measured negative, not a broken harness.

### Was the inherited state ever actually reached? — direct instrumentation

Byte-identity says the *output* did not move. To answer the sharper question the
brief asks — whether a `derive()` ever *did* inherit a mid-walk question — a
third compiler was built in a scratch worktree with a probe printed at the exact
point of the new reset:

```go
derived.objectEscapeChecks = nil
if g.escapeAsksWhatTheValueHolds {
        fmt.Fprintln(os.Stderr, "CCWORK-DERIVE-WITH-HOLDS-SET")
}
```

That probe fires whenever `derive()` is entered with the deep question still up
— i.e. exactly the case commit 1 exists to prevent.

| sweep | invocations | marker hits |
|---|---|---|
| 403 corpus programs × {default, `-O`}, `-c` (program code) | 806 | **0** |
| import-heaviest programs × {default, `-O`}, full link (program **+ vendored stdlib pack**) | 12 | **0** |

Both sweeps watched to exit 0, `stderr` files empty.

**Nothing in the corpus ever reaches a `derive()` with the deep question up.**
The `-c` arm covers every corpus program's own code; the full-link arm builds the
runtime pack, so the vendored standard library is compiled under the probe too.

### Static reading, which agrees

`escapeAsksWhatTheValueHolds` is set to `true` in exactly one place
(`goc/compile.go:4226-4228`, `compositeElementDoesNotEscape`), under a
`saved`/`defer`-restore pair that spans only the call to
`valueDoesNotEscapeWithin`. All eleven `derive()` call sites are *lowering*-time
(`compile`, `functionLiteral`, `callClosure`, `methodValue`, `iteratorRangeStmt`,
the interface-call wrappers, the dynamic-initialiser generator, the go-adapter),
not walk-time. For the inherited state to be observable, the escape walk would
have to re-enter lowering while the flag is up.

---

## Item 3 — `make test-unit` — **PASS**

Watched to exit, **exit 0**. 37 packages listed, **25 with tests all `ok`**, 12
with no test files, **0 `(cached)`** — every package was actually run.


## Item 1 — `go test -timeout 40m -parallel 10 ./goc/...` — **PASS, 0 failures**

Launched detached and polled for its exit file; watched to exit.

    ok  github.com/evanphx/cg12/goc  1130.412s
    exit 0        0 `(cached)`

**The wave-7 gate's blocker is gone.** The gate left exactly one test red —
`TestDeriveClassifiesEveryGenField` — and reported it rather than fixing it. On
this branch the package is green. `TestDeriveClassifiesEveryGenField` passes:
the guard's complaint was that `fullyPopulatedGen` left
`escapeAsksWhatTheValueHolds` zero, which made the classification vacuous for it;
commit 1 populates it (`goc/derive_test.go:151`) *and* resets it in `derive()`,
i.e. classifies it as per-function rather than adding it to the 33-entry
`wholeCompilationGenFields` list. The two halves are consistent — had the fixture
been populated without the reset, the guard would now fail the other way
("derive inherited it").

### Subtest census — **698, exactly the gate's 698**

A second full run with `-v -count=1` (exit 0, 1033.5 s, 0 `(cached)`):

| | count |
|---|---|
| distinct `=== RUN` names | **698** |
| `--- PASS` | 692 |
| `--- FAIL` | **0** |
| `--- SKIP` | 6 |

The gate measured **698** on the merged tree against 695 on `main` (+3, all
branch-2 additions). This branch is **698 — unchanged**: commit 1 adds no test
and removes none, it only populates an existing fixture field.

The 6 skips are the opt-in, host-toolchain-dependent tests, every one of which is
run explicitly elsewhere in this report: `TestCryptoSigningBench` (item 11),
`TestEscapeDifferentialAgainstGC` and `TestEscapeDifferentialProgram` (item 10),
`TestEscapeReasonDifferentialAgainstGC` (item 10 portability),
`TestSlogAllocationsAgainstGC` (item 9), and `TestEscapeSummaryPromotionRate`.
None is skipped for a reason this branch introduced.


## Item 2 — capability matrix, both arms — **366/366 PASS in both. No regression.**

Both run with `GOFLAGS=-v`, both watched to exit, both `exit 0`, no `(cached)`.

| arm | command | subtests | `--- PASS` | `--- FAIL` | wall |
|---|---|---|---|---|---|
| default | `make test-goc-status GOFLAGS=-v` | 366 | **366** | 0 | 136.4 s |
| `-O` | `make test-goc-status-opt GOFLAGS=-v` | 366 | **366** | 0 | 148.8 s |

**PASS/FAIL sets:** the two arms' 366-name `--- PASS` sets are **identical**
(`diff` empty). Both arms report the same single `EXPECTED FAILURE`
(`runtime_panic_print_string.go`, a charted known gap, not a test failure — the
subtest itself passes). Nothing failed in either arm, so there is nothing to
attribute and no `main` control is needed here.


## Item 4 — `TestFrameEscapeAudit -count=1` — **PASS, 182 entries, zero additions**

    go test -timeout 40m -run '^TestFrameEscapeAudit$' -count=1 -v ./goc
    --- PASS: TestFrameEscapeAudit (193.25s)     exit 0, not cached

`goc/testdata/frame_escape_baseline.txt` holds **182** non-comment entries. The
test is a two-way ratchet — it fails on a publication that is not listed *and* on
a listed publication that has gone away — so a pass is a statement that the set
is exactly the accepted 182: **zero additions, zero vanishings.** The file is
byte-identical to `main`'s (`git diff cae1430 HEAD -- …frame_escape_baseline.txt`
is empty), and this branch changes no file under `goc/testdata/` except deleting
the `.pyc`.

## Item 6 — determinism and parallel-vs-serial identity — **PASS**

`TestParallelBackendIsByteIdenticalToSerial` (`./arm64`, `-count=1 -v`):
**PASS at workers = 1, 2, 3, 8, 64, 256**, all six subtests, exit 0.


## Item 7 — loop aliasing — **clean**

One watched run, `-count=1 -v`, exit 0, no `(cached)`:

| check | result |
|---|---|
| `TestLoopAliasExpectationsMatchTheHostToolchain` | **PASS**, 6/6 programs match the host toolchain |
| `TestLoopBodyAllocationsAreDistinctPerIteration` | **PASS**, 12/12 subtests (6 programs × {default, `-O`}) |
| `TestLoopAliasAudit` | **PASS** (192.4 s) |
| `loop_alias_baseline.txt` | 589 entries, unchanged from `main` and from the gate |

`loop_alias_frame_local.go` in the committed allocation census — exactly **one**
row, and it is `frame`:

    12751: testdata/loop_alias_frame_local.go:53:8  main.literalWithin  runtime.newobject  main_point  frame

**1 frame / 0 heap**, same file, same line 12751 as the gate and as `main`.


### Determinism — same source, same tree, compiled twice

4 programs × {default, `-O`} through `go run ./cmd/goc -o`, each compiled twice:
**8/8 pairs byte-identical.**

    runtime_map_pointer_keys              default 5a95050…   -O 7366215…
    loop_alias_frame_local                default ace9e26…   -O 32802b7…
    stdlib_smtp_session                   default 192c4c5…   -O e96f0b6…
    runtime_package_initializer_dispatch  default 9612f60…   -O 099f7e7…

## Item 10 — gc differential — **goc heaps what gc frames: 96. Reference met.**

    go test -count=1 -v -run '^TestEscapeDifferentialAgainstGC$' ./goc -escape-gc-differential
    --- PASS: TestEscapeDifferentialAgainstGC (11.00s)     exit 0

    host toolchain: go version go1.26.1 linux/arm64
    compared 399 of 403 corpus programs, 1861 census rows, 3511 gc decisions
    permissive (gc heaps, goc does not): 1467 lines
    pessimistic (goc heaps, gc does not): 399 lines

The test does not merely report; it renders the whole differential and asserts it
equals the committed file, so a pass means the confusion matrix reproduces cell
for cell:

      goc\gc      frame     heap    mixed   absent    total
      frame         189       30       14      193      426
      heap           96      580      172       81      929
      mixed          13       89       24       13      139
      absent        420     1286       24        0     1730
      total         718     1985      234      287     3224

**goc-heaps-what-gc-frames = 96**, the reference figure, identical to the gate's
and to `main`'s. Note the 96 is the `heap`/`frame` matrix cell, not the 399-line
PESSIMISTIC total — those are different quantities and both are unmoved.


## Item 5 — allocation census twice — **STABLE, and it did not move. Nothing regenerated.**

Two independent `-update-alloc-census-baseline` runs on this branch (182.1 s and
182.0 s, both watched to exit 0, neither cached):

| | md5 | rows |
|---|---|---|
| run 1 | `42c139d49aaa95863d242f2bc0411eb4` | 14 501 |
| run 2 | `42c139d49aaa95863d242f2bc0411eb4` | 14 501 |
| committed on this branch | `42c139d49aaa95863d242f2bc0411eb4` | 14 501 |

Stable across runs **and byte-identical to the committed file**, which is itself
byte-identical to the gate's (this branch touches no file under `goc/testdata/`
except the deleted `.pyc`). The census is the tree's own answer to "did an
allocation move", and it says **no allocation moved** — an independent
confirmation of the byte-comparison above, from a different instrument.

**Nothing was regenerated.** The working tree was restored after each update run;
`git status` is clean apart from this report.


### Portability of the reason differential — **PASS, byte-identical across paths**

`escape_gc_reason_differential.txt` regenerated twice, each run watched to exit 0:

    /home/evan/.ccwork/ws/wave7-fix-verify/repo                                       (177 s)
    /home/evan/.ccwork/ws/wave7-fix-verify/tmp/
        portability-second-worktree-at-a-noticeably-different-path                     (182 s)

    760 120 bytes each
    sha256 41b0531d3c1efa78caa199545718ffb46be447137eefe5dabea02fe1579d3d5f   both
    cmp regenerated-vs-regenerated : BYTE-IDENTICAL
    cmp regenerated-vs-committed   : BYTE-IDENTICAL
    grep -c /home/evan             : 0 in both

All three agree, and the sha256 is the **same** `41b0531d…` the gate measured, so
the file did not move and the ratchet still works outside the directory that made
it. `TestReasonPositionsAreRepositoryRelative` — the cheap half of the same
guarantee — **PASSES**. Both worktrees were restored afterwards.

## Item 9 — slog benchmark — **30/32 at parity. Reference met.**

    go test -count=1 -v -run '^TestSlogAllocationsAgainstGC$' ./goc -slog-allocations
    --- PASS: TestSlogAllocationsAgainstGC (18.03s)     exit 0
    host toolchain: go version go1.26.1 linux/arm64,  32 cases

**30 of 32 rows are at parity on a/op.** The two that are not are the same two
the gate reported, and both are goc *ahead* of gc, not behind:

| case | goc a/op | gc a/op | goc B/op | gc B/op |
|---|---|---|---|---|
| `info/3-attr-large-ints` | 1.00 | 3.00 | 128.0 | 24.0 |
| `json/kv-4-pairs` | 1.00 | 2.00 | 176.0 | 24.0 |

`slog_allocations_baseline.txt` is byte-identical to the gate's (`git diff`
against `01b0cbf` empty), so these are the same rows, unchanged.


## Item 8 — GC reducer, 20× at `GOGC=10` and default, both trees — **0/20 everywhere**

Idle box (1-minute load average 4.46 at the start, 4.68 at the end; every other
job in this report had exited). `GOMAXPROCS=3`, serial, 180 s timeout per run. A
run counts as a pass only if it exits 0 **and** prints exactly
`type mask padding ok`.

| tree | `GOGC=10` | default `GOGC` |
|---|---|---|
| `integration/wave7-fix` `6034f73` | **0/20 failures** | **0/20 failures** |
| `main` `cae1430` (control) | **0/20 failures** | **0/20 failures** |

80 runs, **zero failures**, and the failure log is empty. The `main` control
reproduces its stated 0/20 at both settings, so the branch result is measured
against a control that behaved.



## Item 11 — `make bench-crypto` — **INTERMITTENTLY RED ON THIS BOX, AND NOT THIS BRANCH'S DOING**

This is the only item that is not simply green, so it is reported in full.

### What happened

Seven watched `make bench-crypto` runs on the branch, on an idle box:

| run | exit | failing row |
|---|---|---|
| 1 | 2 | `p256/verify` goc index 34.3805 → 32.9121 (**−4.3 %**) |
| 2 | 0 | — |
| 3 | 2 | `p256/verify` 34.3805 → 32.9531 (**−4.2 %**) |
| 4 | 2 | `p256/sign-verify` 45.7973 → 47.9711 (**+4.7 %**) |
| 5–7 | 0 | — |

Tolerance is `0.04` of the index in both directions. Note runs 1/3 and run 4 fail
on **different cases in opposite directions**.

### The control: the gate tree, same command, same baseline, interleaved

`integration/wave7-gate` carries the **identical** committed baseline
(`crypto_signing_bench_baseline.txt` md5 `7290b110…` in both trees) and produces a
**byte-identical benchmark binary** (`goc -O` on
`goc/testdata/crypto_signing_bench/main.go` →
`93d37e76300d0a4db2d0f068117eea1ddf1faf9f11a3326cc67011fc4990f208` under both
compilers, established in the byte-comparison at the top of this report).

* gate tree, `make bench-crypto`: **7 runs, 7 passes, 0 failures.**
* `main` `cae1430` control against its own committed baseline: **3 runs, 3 passes**
  — so the box is not globally broken and `main`'s baseline is not stale.

Four of the branch runs and four of the gate runs were **interleaved**
(branch, gate, branch, gate, …) so time-of-run drift could not land on one arm:
branch 3/4, gate 4/4.

### The numbers, interleaved, and the noise floor

Six `-update` runs alternating branch/gate on the idle box (load 1.00–1.29),
scratch checkouts so no committed baseline was rewritten:

| case | branch runs | gate runs | branch mean | gate mean | delta |
|---|---|---|---|---|---|
| `p256/sign-verify` | 45.3097 45.2777 46.1687 | 46.2362 46.6504 46.7729 | 45.5854 | 46.5532 | **−2.08 %** |
| `p256/verify` | 34.0237 34.1447 34.2266 | 33.9076 34.2914 35.0101 | 34.1317 | 34.4030 | **−0.79 %** |
| `p384/sign-verify` | 39.6440 40.1672 39.6449 | 39.9627 40.1568 40.2779 | 39.8187 | 40.1325 | **−0.78 %** |
| `rsa2048/sign-verify` | 12.4691 12.6250 12.3599 | 12.5206 12.2395 12.2527 | 12.4847 | 12.3376 | **+1.19 %** |

The sign of the branch-vs-gate delta **varies by case**, which is what a
byte-identical binary must produce. (An earlier, *non*-interleaved set had
`p256/verify` apparently +1.28 % on the branch; interleaving reversed it to
−0.79 %. That is the size of the ordering artefact, and it is why the interleaved
set is the one quoted.)

**Noise floor, measured, not quoted** — the pooled range across those six runs of
a byte-identical binary:

| case | min | max | same-source range | tolerance |
|---|---|---|---|---|
| `p256/sign-verify` | 45.2777 | 46.7729 | **3.25 %** | 4.00 % |
| `p256/verify` | 33.9076 | 35.0101 | **3.22 %** | 4.00 % |
| `p384/sign-verify` | 39.6440 | 40.2779 | **1.59 %** | 4.00 % |
| `rsa2048/sign-verify` | 12.2395 | 12.6250 | **3.11 %** | 4.00 % |

**This box's same-source noise today is up to 3.25 %, against a 4.00 % gate.** An
instrument whose noise is 81 % of its tolerance will fail intermittently on
*any* tree. Three failures in seven runs on one arm and none in seven on the
other is a chance split at that noise level, not a signal.

### Attribution, against the triage note

`goc/cryptobench_test.go`'s note says a movement here has three causes. All three
are excluded by measurement, not by argument:

1. **an allocation moved** — the allocation census regenerated twice on this
   branch is byte-identical to the committed file (item 5). No site moved.
2. **the generated code changed** — 848/848 corpus binaries byte-identical to the
   gate, the crypto benchmark program among them. Not one instruction changed.
3. **the code did not change and *moved*** — it did not move either: the whole
   linked image is byte-identical, so the text is at the same offsets. The
   32-byte entry alignment this branch inherits is live in *both* arms of the
   comparison and cannot differentiate them.

Nothing is left but the instrument. **The `bench-crypto` failures are not
attributable to `integration/wave7-fix`;** there is no mechanism by which a
change that emits identical bytes can move an elapsed-time measurement of those
bytes.

### Recommendation (not applied — this job does not fix code or baselines)

The baseline was re-cut by the gate at `d044ea3` from a **single** run of an
instrument whose noise is now 3.2 % against a 4 % tolerance, so it sits close to
an edge. Re-cutting it again from another single run would just move the edge.
If this check is to be trusted as a gate it needs either a wider tolerance or a
baseline taken from a median of several runs. **Left alone deliberately; flagged
for a person.**

### Carrying forward the correction the brief asked for

The alignment branch reported the crypto placement spread falling 6.1 % → 0.4 %;
the wave-7 gate could not reproduce that and measured 4.25 % → 2.73 % against a
1.88 % same-source noise floor. **This session's same-source noise floor is
higher still — up to 3.25 %** (interleaved, idle box, load ≈ 1.0). Any spread
figure measured on this box today would be at or below its own noise, so this
report does not quote one. That is the correction propagated: the number is
reportable only with its noise floor beside it, and here the noise floor swallows
it.


---

# Summary

| # | item | reference | measured | verdict |
|---|---|---|---|---|
| — | **does commit 1 change emitted code?** | — | **848/848 binaries byte-identical; 0 probe hits in 818 compiles** | **NO** |
| 1 | `go test -timeout 40m -parallel 10 ./goc/...` | 0 failures | exit 0, 1130 s, 0 cached; `TestDeriveClassifiesEveryGenField` **passes** | **PASS** |
| 1 | subtest census | gate's 698 | **698**, 692 PASS / 0 FAIL / 6 opt-in SKIP | **PASS** |
| 2 | `make test-goc-status` `-v` | 366/366 | **366/366**, 0 FAIL | **PASS** |
| 2 | `make test-goc-status-opt` `-v` | 366/366 | **366/366**, 0 FAIL, PASS set identical to the default arm | **PASS** |
| 3 | `make test-unit` | pass | exit 0, 25 packages, 0 cached | **PASS** |
| 4 | `TestFrameEscapeAudit -count=1` | 182 entries, 0 additions | **182**, 0 added, 0 vanished | **PASS** |
| 5 | allocation census ×2 | stable | `42c139d4…` twice, 14 501 rows, = committed. **Nothing regenerated** | **PASS** |
| 6 | determinism | byte-identical | 8/8 pairs identical | **PASS** |
| 6 | `TestParallelBackendIsByteIdenticalToSerial` | pass | PASS at 1/2/3/8/64/256 workers | **PASS** |
| 7 | loop aliasing | host match; 1 frame / 0 heap; audit clean | 6/6 match host, 12/12 distinct-per-iteration, audit PASS, `loop_alias_frame_local` **1 frame / 0 heap** | **PASS** |
| 8 | GC reducer 20× at `GOGC=10` and default | 0/20 both | **0/20 both, branch and `main` control** (80 runs, 0 failures) | **PASS** |
| 9 | slog benchmark | 30/32 at parity | **30/32**, same two rows, both goc ahead of gc | **PASS** |
| 10 | gc differential | 96 goc-heaps-what-gc-frames | **96**, whole matrix reproduces cell for cell | **PASS** |
| 10 | reason differential portable across paths | byte-identical | sha256 `41b0531d…` in both worktrees **and** = committed; 0 path leaks | **PASS** |
| 11 | `make bench-crypto` | pass | **3 failures in 7 runs; gate control 7/7 pass, `main` 3/3 pass** | **UNSTABLE — not this branch** |

Everything watched to exit. No number in this report was taken from a `(cached)`
result; `(cached)` count was checked and was 0 on every `go test` invocation.

## The one item that is not green, in one paragraph

`make bench-crypto` fails intermittently (3 of 7 runs) on the branch. It fails on
different cases in opposite directions; the gate tree, which produces a
**byte-identical** benchmark binary and carries the **identical** committed
baseline, passed 7/7 in the same session; and this box's same-source run-to-run
noise, measured interleaved on an idle box, is up to **3.25 % against the check's
4.00 % tolerance**. There is no mechanism by which a change that emits identical
bytes moves an elapsed-time measurement of those bytes. The instrument is sitting
too close to its own noise; that is a pre-existing property of the baseline the
gate cut at `d044ea3`, not something this branch introduced.

## Commit 2 — the `.pyc` — clean

`git ls-files | grep -E '\.pyc$|__pycache__'` → **0**. `.gitignore` gains
`__pycache__/` and `*.pyc`. No `__pycache__` directory exists in the tree. Working
tree clean apart from this report.

---

# ANSWER TO THE HEADLINE QUESTION

**Commit 1 does NOT change emitted code.** 424 corpus programs × {default, `-O`}
= 848 fully linked binaries are byte-identical between `integration/wave7-fix`
and `integration/wave7-gate`; the allocation census is byte-identical; the gc
placement differential and the reason differential are byte-identical; a probe
compiler shows `derive()` is entered with `escapeAsksWhatTheValueHolds` set
**zero times** across 818 compiles covering every corpus program and the vendored
standard library. **The inherited state was never reached in practice. The fix is
pure hygiene** — it makes the field's classification match its declared
per-function placement, and it closes the guard that had been red for five
consecutive waves. It is not a behaviour change in any program this tree
compiles.

That is the *measured* answer, and it is the good one: had the byte-comparison
moved, the old behaviour would have been live and every changed program would
have needed adjudicating. It did not move.

# VERDICT: **SAFE TO MERGE TO MAIN**

`integration/wave7-fix` (`6034f73`) is `integration/wave7-gate` plus a five-line
reset that provably changes nothing the compiler emits, a one-line test fixture,
and a deleted `.pyc`. The gate's single blocker is gone; `go test ./goc/...` is
green with the same 698 subtests. Every reference number the brief set is met
exactly: 366/366 in both capability arms, 182 frame-escape entries, 96
goc-heaps-what-gc-frames, 30/32 slog rows, 0/20 GC reducer at both `GOGC`
settings on both trees, 1 frame / 0 heap for `loop_alias_frame_local`,
byte-identical determinism and parallel-vs-serial identity, and a reason
differential that reproduces byte for byte at a second checkout path.

The one caveat, which is a caveat and not a blocker: **`make bench-crypto` is an
unreliable gate on this box today** — its noise floor is 3.25 % against a 4.00 %
tolerance — and it fails intermittently on `integration/wave7-fix`, on
`integration/wave7-gate`'s byte-identical binary, and would on anything else. It
should be re-cut from a median or given a wider tolerance by someone who owns
that decision. It does not implicate this branch.
