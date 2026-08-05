# wave8 merge gate: verification of `integration/wave8` (7983abd)

(The previous contents of this file — `ccwork/goroutine-scheduler`'s report on
the 38x findfunctab scan — are at `git show 7983abd:CCWORK_REPORT.md`.)

Box: lab47, 64 cores, exclusive to this job. Host toolchain
`go1.26.1 linux/arm64`, system `cc` 13.3.0. Every timing measurement below
states the load average it was taken under.

Controls: `main` = 6034f73. `ccwork/perf-suite` = d2855f5, the base all four
branches sit on.

**Status: IN PROGRESS — this file is written as each item completes.**

---

## 0. The mem2reg switch is inert when off — VERIFIED

This was the wave's most important correctness question, because
`ccwork/locals-double-indirection` itself reports that turning the switch on
breaks two things (a GC-visibility defect in compress/flate and an interface
dispatch failure in `stdlib-netpoll-stress/tcp-churn`).

**Source-level.** Across the entire wave, `opt/pass.go` has exactly one
non-comment change against `ccwork/perf-suite`:

```go
+	if os.Getenv("GOC_BOUNDED_MEM2REG") == "" {
 		return []Pass{clean}
+	}
+	return []Pass{
+		FuncPass("mem2reg", Mem2Reg),
+		clean,
+	}
```

plus the `os` import. Everything else in the 56-line diff is comment. No other
file in the wave touches the pipeline.

**Byte-level.** Built two `goc` binaries **from the same absolute path** —
`/home/evan/.ccwork/ws/wave8-gate/repo` — one at 7983abd, one at 7983abd with
`opt/pass.go` restored to d2855f5's. Compiled 18 programs (the four
`perf_bench` mains plus fourteen `goc/testdata` programs spanning GC,
interfaces, slices, fmt, context and allocation) at `-O` and without `-O`:

| comparison | result |
|---|---|
| env **unset** vs pre-switch compiler | **36/36 byte-identical** |
| env unset vs `GOC_BOUNDED_MEM2REG=1` | 18/18 **differ** |

The second row is the positive control: it proves the first row is not vacuous.
The compiler is also self-reproducible (same tree, same program, two runs →
identical bytes) and independent of the compiler binary's own identity (a second
`go build` of the same tree produces the same program bytes).

**Method note for anyone repeating this.** A first attempt compared against a
`git worktree` at a different path and every pair differed, including at `-O`
off where `BoundedPipeline` cannot run. goc embeds absolute source paths in its
output, so `.../repo` vs `.../wt-nomem2reg` shifts ~1440 bytes and 856k byte
positions in a hello-world. The comparison is only meaningful from an identical
path. This is worth knowing: it makes the naive form of this check produce a
loud false positive.

**One finding, minor but real.** The guard is `os.Getenv(...) == ""`, so *any*
non-empty value enables the pass — including `GOC_BOUNDED_MEM2REG=0`, verified:
it produces the mem2reg output, not the control's. `GOC_BOUNDED_MEM2REG=` (empty)
is correctly off. Nothing in the tree sets the variable, so this does not affect
the merge; it is a footgun for whoever next tries to turn the switch *off*
explicitly.

---

## 3. `make test-unit` — PASS

Exit 0. Every unit package `ok`; no failures, no build errors.

---

## 1. `go test -timeout 40m -parallel 10 ./goc/...` — PASS

```
ok  	github.com/evanphx/cg12/goc	1020.656s
```

Exit 0, zero failures. Load average during the run 18–30 on 64 cores.

**Census.** Top-level test functions in `./goc/...`: **342** on 7983abd against
**341** on `main` (6034f73). One added: `TestPerformanceSuite`, from
`ccwork/perf-suite`. Nothing removed. The wave's other new tests land outside
this package and are covered by items 2 and 3:

| new test file | commit / branch |
|---|---|
| `cmd/goc/math_intrinsic_test.go` (305 lines) | `math-intrinsics` 15dc4ef |
| `arm64/floatmath_e2e_test.go` (249 lines) | `math-intrinsics` 3300ca6 |
| `interp/floatmath_test.go` (185 lines) | `math-intrinsics` |
| `arm64/a64/a64_test.go`, `widen_test.go` (+23) | `math-intrinsics` |
| `internal/gometa/findfunctab_test.go` (213 lines) | `goroutine-scheduler` e625d51 |
| `cmd/goc/runtime_status_test.go` (+16) | `flate-gc-crash` 800f47f — adds one capability |
| `goc/testdata/runtime_gc_slice_tail_pointer.go` (178 lines) | `flate-gc-crash` 800f47f, folded by 91a070d |
| `goc/perfbench_test.go`, `perf_bench/*` | `perf-suite` (already on `main`'s successor base) |

`TestDeriveClassifiesEveryGenField` **passed**. It has failed in five
consecutive waves; it did not this time, and no branch in this wave adds a `gen`
field. Nothing to name.

---

## 2. `make test-goc-status` and `make test-goc-status-opt` (both `-v`) — PASS / PASS

Run as the Makefile's commands with `-v` added, unsharded.

| arm | result | wall | PASS | FAIL | SKIP |
|---|---|---:|---:|---:|---:|
| default | exit 0 | 103.8 s | **367** | 0 | 0 |
| `-O` (`-runtime-opt`) | exit 0 | 118.5 s | **367** | 0 | 0 |

The two arms' capability *sets* are byte-identical (`diff` of the sorted
`category/name` lists is empty), so this is 367/367 twice over and not two
different 367s.

**It is 367, not 366.** `cmd/goc/runtime_status_test.go` holds 366 capabilities
at both `main` (6034f73) and `ccwork/perf-suite` (d2855f5) and 367 at 7983abd.
The added one is `gc-invariants/slice-tail-pointer`, the `flate-gc-crash`
reducer (800f47f). So the expected figure for this wave is 367/367 and both arms
hit it.

**`stdlib-netpoll-stress/tcp-churn` passes on the `-O` arm**, which is the
specific thing job 4 flagged. It passes on the default arm too. Consistent with
item 0: with `GOC_BOUNDED_MEM2REG` unset the `-O` compiler is the pre-switch
compiler byte for byte, so there is nothing here for the switch to break.

---

## 6. GC reducer `runtime_gc_type_mask_padding.go` — 0/20 everywhere

Compiled with each tree's own `goc`, run 20 times per cell. Both `-O` arms and
both default arms, because the capability matrix runs it both ways.

| tree | arm | `GOGC=10` | `GOGC` unset |
|---|---|---:|---:|
| 7983abd | `-O` | **0/20** | **0/20** |
| 7983abd | default | **0/20** | **0/20** |
| `main` 6034f73 | `-O` | 0/20 | 0/20 |
| `main` 6034f73 | default | 0/20 | 0/20 |

160 runs, zero failures. Every run prints `type mask padding ok` and exits 0, so
the instrument is doing its work and not exiting early.

---

## 7. The flate crash rate — 0 in 440, with a control that fires

`goc/testdata/placement_bench/flate/main.go`, built `goc -O`, run as separate
processes 8-way parallel. A run counts as a failure on any non-zero exit.

| tree | runs | crashes | rate |
|---|---:|---:|---|
| **7983abd** (fix in) | **440** | **0** | **0 %** |
| `main` 6034f73 (fix out) | 300 | 2 | 0.67 % |

The `main` column is the control and it matters: without it, 0/440 could mean
the harness cannot see a crash. It can. Both `main` failures are the documented
defect, at the same object offset:

```
runtime: pointer 0x6285d1d48000 to unallocated span span.base()=0x6285d1d48000 ...
runtime: found in object at *(0x6285d1d31300+0x10f8)
```
```
runtime: pointer 0x61a37c356000 to unused region of span ...
runtime: found in object at *(0x61a37c25d300+0x10f8)
```

The observed pre-fix rate here is 0.67 %, well below job 1's 7.5 %; the crash is
heap-layout and GC-timing dependent and these runs were 8-way parallel, which is
a different mix. Taking the conservative rate, 0/440 has p ≈ 0.05 under the null
that nothing changed; taking job 1's 7.5 %, p ≈ 10⁻¹⁵. Either way the fix holds
and nothing in this wave reintroduced the crash.

---

## 1a. Full subtest census (verbose re-run of item 1)

`go test -timeout 40m -parallel 10 -v ./goc/...` re-run for the census, exit 0,
`ok ... 1089.916s`:

| | count |
|---|---:|
| top-level tests PASS | 335 |
| top-level tests FAIL | **0** |
| top-level tests SKIP | 7 |
| PASS including subtests | **692** |
| FAIL including subtests | **0** |

`--- PASS: TestDeriveClassifiesEveryGenField (0.00s)`. It passed. No branch in
this wave adds a `gen` field, so the five-wave streak has nothing to feed on
here; there is no field to name and no `derive` reset to check.

The 7 skips are all the opt-in instruments — `TestCryptoSigningBench`,
`TestPerformanceSuite`, `TestSlogAllocationsAgainstGC`,
`TestEscapeDifferentialAgainstGC`, `TestEscapeDifferentialProgram`,
`TestEscapeReasonDifferentialAgainstGC`, `TestEscapeSummaryPromotionRate` —
each of which is run explicitly below.

---

## 4. The four audits — PASS

| test | result |
|---|---|
| `TestFrameEscapeAudit` | PASS |
| `TestLoopAliasAudit` | PASS |
| `TestEscapeShadowPlacement` | PASS |
| `TestAllocationCensus` (run 1) | PASS, 185.62 s |
| `TestAllocationCensus` (run 2) | PASS, 184.35 s |

The two census runs' verbose output is identical apart from the elapsed-time
line, so the census is stable run to run and not merely green twice.

---

## 5. Determinism — PASS

| test | package | result |
|---|---|---|
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` | `goc` | PASS |
| `TestBinaryDeterministic` | `ir` | PASS |
| `TestBuildIDIsDeterministic` | `link` | PASS |
| `TestImagesCarryABuildID` | `link` | PASS |
| `TestRuntimeCorpusCoverageRecordsConcurrentOutcomesDeterministically` | `cmd/goc` | PASS |
| **`TestParallelBackendIsByteIdenticalToSerial`** | `arm64` | **PASS** |

Corroborated end to end by item 0's byproduct: the same tree compiling the same
program twice produced byte-identical binaries, and two independently built
`goc` binaries from the same tree produced byte-identical output — so
determinism holds at the whole-program level, not only at the unit level.

---

## 9. gc differential and slog benchmark — slog PASS, **gc differential FAIL**

### slog — PASS, and it is the expected 30/32

`go test ./goc -slog-allocations -run TestSlogAllocationsAgainstGC` exits 0.
`32 cases`, and 30 of the 32 have `goc a/op == gc a/op` in
`goc/testdata/slog_allocations_baseline.txt`. The two that do not are the two
already committed as such:

| case | goc a/op | gc a/op |
|---|---:|---:|
| `info/3-attr-large-ints` | 1.00 | 3.00 |
| `json/kv-4-pairs` | 1.00 | 2.00 |

30/32 as expected. Nothing moved.

### gc differential — FAIL, and it is a regression this wave introduced

```
go test ./goc -escape-gc-differential -run TestEscapeDifferentialAgainstGC
--- FAIL: TestEscapeDifferentialAgainstGC (11.18s)
```

**Controls, measured, not assumed:**

| tree | result |
|---|---|
| `main` 6034f73 | **PASS** (exit 0) |
| `ccwork/perf-suite` d2855f5 (merge base) | **PASS** (exit 0) |
| `integration/wave8` 7983abd | **FAIL** (exit 1) |

So unlike `make bench-crypto` (item 8), this one is *not* pre-existing. It is
caused by this wave.

**Cause.** `ccwork/flate-gc-crash` (800f47f) added a corpus program,
`goc/testdata/runtime_gc_slice_tail_pointer.go`, and updated
`goc/testdata/alloc_census_baseline.txt` (+6 rows) — but did not regenerate
`goc/testdata/escape_gc_differential.txt`. The corpus is now one program larger
than the committed differential describes:

| quantity | committed reference | this run |
|---|---:|---:|
| corpus programs | 403 | **404** |
| compared | 399 | **400** |
| census rows joined | 1861 | **1867** |
| gc decisions joined | 3511 | **3529** |
| PERMISSIVE | 1467 lines | **1477 lines** |
| PESSIMISTIC | 399 lines | **401 lines** |
| confusion matrix, goc `heap` × gc `frame` | **96** | **97** |

That last row is the closest thing in this instrument to the "96 lines"
reference in the brief — it is the confusion matrix cell counting source lines
goc heaps and gc frames. I could not find any quantity in this instrument that
is 96 *lines* under another name; the two headline line-counts are 1467
permissive and 399 pessimistic. Flagging that rather than quietly matching a
number.

Every added line in the failure diff comes from the one new program — its
`panic(...)` strings and its `checkPointers`/`tail` functions. No pre-existing
program's verdict changed. This is a stale reference file, not a change in
goc's escape analysis.

**`TestEscapeReasonDifferentialAgainstGC` fails the same way**, for the same
reason: FAIL on 7983abd, PASS on d2855f5, and all 18 mentions of a program name
in its diff are `runtime_gc_slice_tail_pointer`.

Neither test runs without its opt-in flag, so neither gates `go test ./...` and
neither is why item 1 is green. **Not repaired here** — the brief says diagnose,
do not repair, and these are not the two baselines I was asked to re-cut. The
fix is two commands, each of which should have its diff read by a person:

```
go test ./goc -run TestEscapeDifferentialAgainstGC \
    -escape-gc-differential -update-escape-gc-differential
go test ./goc -run TestEscapeReasonDifferentialAgainstGC \
    -escape-gc-reason-differential -update-escape-gc-reason-differential
```

---

## 8. The two timing instruments — re-baselined on an idle box

All timing below was taken with the box otherwise idle and **one benchmark at a
time**, sequentially, never two at once. `bench-crypto` pins to core 63,
`bench-perf` to core 62. Load average at the start of the timing phase: 3.48 and
falling; nothing else of this job was running.

### 8a. `make bench-crypto` against the committed (stale) baseline — FAILS, and it is not this wave's fault

```
--- FAIL: TestCryptoSigningBench (280.85s)
the program measures a case testdata/crypto_signing_bench_baseline.txt does not list.
  p256/sign-verify      now index 44.8238 +/- 0.05% under goc
  p256/verify           now index 32.6617 +/- 0.12% under goc
  p384/sign-verify      now index 39.1345 +/- 0.04% under goc
  rsa2048/sign-verify   now index 12.1743 +/- 0.12% under goc
make: *** [Makefile:155: bench-crypto] Error 1
```

**Attribution — this is pre-existing and provable, not inferred.** Both files
involved are **byte-identical** between `ccwork/perf-suite` (d2855f5, the merge
base) and `integration/wave8` (7983abd):

```
IDENTICAL  goc/cryptobench_test.go
IDENTICAL  goc/testdata/crypto_signing_bench_baseline.txt
```

**Mechanism.** 1af81e1 rewrote the check to a mean-of-N with a confidence
interval and, with it, the baseline's column layout: `parseCryptoBenchBaseline`
requires **7 fields** per row (`case, gocIndex, goc±%, gcIndex, gc±%, gocNs,
gcNs`). The committed baseline is the pre-rewrite format and has **5**. Every
data row is skipped, the parser returns **zero** rows, and the comparison then
reports all four cases as cases the baseline does not list. The failure is a
pure function of the baseline text, which is the same text on both branches, and
is independent of any measured time — so it fails identically on the merge base.
Nothing in wave8 touches it.

### 8b. `make bench-perf` against the committed (stale) baseline — FAILS on exactly one row, plus a noise guard

`--- FAIL: TestPerformanceSuite (567.85s)`, exit 2. Forty-two rows; **41 within
tolerance**, one past it:

| program | case | baseline | this run | change | tol% | verdict |
|---|---|---:|---:|---:|---:|---|
| float | `float/sqrt-sum` | 171.0966 | **1.1302** | **−99.3 %** | 5.0 % | **PAST TOLERANCE** |

That is `ccwork/math-intrinsics` landing, reproduced independently here: the
branch reported `float/sqrt-sum` going from 171.22x the host to 1.13x, and this
gate measures **171.0966 → 1.1302**. It is a stale baseline row, not a
regression — the baseline was cut before that branch merged.

**The goroutine rows did *not* move**, which is worth stating because the brief
expected them to:

| case | baseline | this run | change |
|---|---:|---:|---:|
| `goroutine/spawn-join` | 5.3773 | 5.3617 | −0.3 % |
| `chan/pingpong-unbuffered` | 6.4725 | 6.5863 | +1.8 % |
| `chan/send-buffered` | 4.7903 | 4.7754 | −0.3 % |
| `mutex/uncontended` | 1.8611 | 1.8623 | +0.1 % |

They are already at their post-fix values in the committed file: 6d2fcbf
(*"gate: re-baseline the perf suite on the tree with the findfunctab fix"*) cut
that baseline on the `goroutine-scheduler` branch. So `float/sqrt-sum` is the
only row this wave leaves stale, because `math-intrinsics` merged *before*
`goroutine-scheduler` and 6d2fcbf's re-cut was made on a branch that did not
contain it.

**A guard also fired, and it is not about the compiler:**

```
gcpress gc/live-heap-churn
  one-repetition spread 2.45% in the baseline, 7.54% in this run (null 1.92% -> 2.18%)
this box is noisier than the box that produced the baseline, by more than 3x on the
spread of the gated ratio itself.
```

`perfBenchNoiseGrowthCeiling` (3.0x) rejecting the run. **Cause, measured:** this
box is quiet by ccwork standards — load average 2.5–3.5 against the wave-7 gate's
216 — but it is not idle. `ps` during the timing phase shows a non-ccwork system
daemon, `rpki-client`, running three processes at ~99 % of one core, plus `etcd`,
`victoria-metrics` and `miren`. Nothing of *this job* was running (the benchmarks
were driven strictly one at a time), but the box has a ~1-core background tenant
that is not pinned and can land on core 62. The null moved far less (1.92 % →
2.18 %), which by the file's own diagnostic means the noise is on the **host**
binary's side, not goc's — consistent with an external tenant rather than
anything in this wave.

### 8c. `goc/testdata/crypto_signing_bench_baseline.txt` — regenerated

`make bench-crypto-update`, exit 0. The file moves from the pre-1af81e1 5-column
layout to the current 7-column one, so it is now parseable at all:

| case | old index | **new index** | new ±% | change |
|---|---:|---:|---:|---:|
| `p256/sign-verify` | 45.7973 | **44.8636** | ±0.07 % | −2.04 % |
| `p256/verify` | 34.3805 | **32.7001** | ±0.09 % | −4.89 % |
| `p384/sign-verify` | 40.5258 | **39.1565** | ±0.08 % | −3.38 % |
| `rsa2048/sign-verify` | 12.5562 | **12.1813** | ±0.13 % | −2.99 % |

The gc column is the reference and barely moved (`p256/sign-verify` 1.6287 →
1.6312, +0.15 %), which says the host toolchain and the box are the same.

**What moved, and why.** The change column above is *not* attributable as a
compiler effect, because the two numbers were produced by different protocols:
the old ones are single unpinned runs from the pre-rewrite instrument, the new
ones are means of five interleaved repetitions pinned to core 63. The file's own
noise-floor note measures pinning alone as worth 3.03 % → 1.83 % of spread. A
control that isolates this is in 8f below.

### 8d. `goc/testdata/perf_suite_baseline.txt` — regenerated

`make bench-perf-update`, exit 0 (the test SKIPs by design after rewriting).
42 rows. **One ratio moved by design:**

| program | case | old ratio | **new ratio** | why |
|---|---|---:|---:|---|
| float | `float/sqrt-sum` | 171.0966 | **1.1289** | `ccwork/math-intrinsics`: `math.Sqrt` lowered to `FSQRT` |

Every other ratio moved by less than its own tolerance; the largest was
`chan/pingpong-unbuffered` 6.4725 → 6.6089 (+2.1 %) and the goroutine rows are
unchanged (`goroutine/spawn-join` 5.3773 → 5.3249, −1.0 %) because 6d2fcbf had
already cut them post-fix.

**The noise columns moved in both directions, and two rows got materially worse:**

| row | old ratio-sd% → new | old tol% → new | old detect% → new |
|---|---|---|---|
| `gcpress gc/live-heap-churn` | 2.45 → **13.49** | 7.3 → **40.5** | 10.0 → **55.1** |
| `chase chase/dram` | 4.37 → **7.66** | 13.1 → **23.0** | 17.9 → **31.3** |
| `text text/format-append` | 9.70 → 4.04 | 29.1 → 12.1 | 39.7 → **16.5** |
| `text text/sprintf` | 9.89 → 7.18 | 29.7 → 21.6 | 40.4 → **29.4** |
| `json json/marshal` | 3.97 → 2.04 | 11.9 → 6.1 | 16.2 → **8.3** |
| `json json/unmarshal` | 3.60 → 2.41 | 10.8 → 7.2 | 14.7 → **9.8** |

Four rows got quieter and better; two got noisier. `gc/live-heap-churn` at
detect% 55.1 is effectively **no longer a gate** — it would take a 55 %
regression to fail it. Both degraded rows are the memory- and GC-bound ones,
which are exactly the rows an external tenant sharing last-level cache and
memory bandwidth would hit; `rpki-client` was running for part of this cut. This
is followed up in 8g.

### 8e. Confirming runs — both instruments PASS twice

Each run is a separate `make` invocation, sequential, one benchmark on the box at
a time.

| run | result | wall |
|---|---|---:|
| `make bench-crypto` #1 | **PASS** | 282.8 s |
| `make bench-perf` #1 | **PASS** | 568.0 s |
| `make bench-crypto` #2 | **PASS** | 285.7 s |
| `make bench-perf` #2 | **PASS** | 567.4 s |

`bench-crypto` reproduces itself to within 0.1 % on every case, twice:

| case | baseline | check #1 | check #2 |
|---|---:|---:|---:|
| `p256/sign-verify` | 44.8636 | 44.8310 (−0.1 %) | 44.8433 (−0.0 %) |
| `p256/verify` | 32.7001 | 32.6772 (−0.1 %) | 32.6760 (−0.1 %) |
| `p384/sign-verify` | 39.1565 | 39.1375 (−0.0 %) | 39.1607 (+0.0 %) |
| `rsa2048/sign-verify` | 12.1813 | 12.1776 (−0.0 %) | 12.1770 (−0.0 %) |

`bench-perf`: no row past tolerance in either run, and no guard fired.

### 8g. The `gc/live-heap-churn` noise is the row's own, not the box's — and the wide tolerance is the right one to keep

I measured this row's one-repetition ratio spread four times on this box, in four
separate runs, all with the box otherwise idle and one benchmark at a time:

| run | `gc/live-heap-churn` ratio-sd% | `chase/dram` ratio-sd% |
|---|---:|---:|
| pre-baseline check | 7.54 | — |
| **the committed `-update` cut** | **13.49** | **7.66** |
| confirming run #1 | 7.53 | 4.56 |
| confirming run #2 | **1.48** | 7.87 |

A 9x spread in the *noise estimate itself*, on a quiet box, with the null arm
staying between 1.04 % and 2.71 % throughout — so it is the **host** binary
jittering, not goc. The host runs this case in ~22 ms and whether a collection
lands inside the timed region is close to a coin flip. My first hypothesis was
the `rpki-client` tenant; confirming run #2 drew 1.48 % under identical
conditions, which refutes it. This row is intrinsically heavy-tailed here.

**I deliberately did not re-cut to get a lower draw.** Two reasons. It would be
choosing a number rather than measuring one; and a *low* draw is the dangerous
one: the instrument derives both the tolerance and the noise-growth ceiling from
the committed spread, so a baseline cut at 1.48 % would give this row a 4.4 %
growth ceiling and any future run drawing 7–13 % would throw away the **whole
42-row run** with a noise-growth failure. The 13.49 % draw is the robust choice.

What it costs is stated plainly: **`gcpress gc/live-heap-churn` is no longer a
usable gate** — detect% 55.1. It is not blind, only silent: the ratio (8.7016)
is committed, so a real movement still shows up as a changed number on the next
`-update`. `chase/dram` is in the same position at detect% 31.3. The other 40
rows are unaffected, and 4 of them got materially *better* than the baseline
they replace.

### 8f. Merge-base controls for `bench-crypto` — both measured, not inferred

Two runs on a worktree at `ccwork/perf-suite` (d2855f5), the merge base.

**Control 1 — the merge base fails the same way.** With the merge base's own
committed baseline (byte-identical to wave8's):

```
--- FAIL: TestCryptoSigningBench (274.53s)   exit 2
the program measures a case testdata/crypto_signing_bench_baseline.txt does not list.
```

Same failure, same message, on a tree that contains none of this wave. The
`make bench-crypto` breakage is 1af81e1's and was already there.

**Control 2 — the merge base measures the same indexes.** With *my regenerated*
baseline dropped in, the merge base **PASSES**:

| case | my baseline (wave8) | merge base measures | change | verdict |
|---|---:|---:|---:|---|
| `p256/sign-verify` | 44.8636 | 44.9957 | +0.3 % | within tolerance |
| `p256/verify` | 32.7001 | 32.7395 | +0.1 % | within tolerance |
| `p384/sign-verify` | 39.1565 | 39.1171 | −0.1 % | within tolerance |
| `rsa2048/sign-verify` | 12.1813 | 12.1061 | −0.6 % | within tolerance |

This settles 8c. The −2 % to −4.9 % between the *old* committed numbers and the
new ones is **the protocol change, not the compiler**: the merge base, which has
none of wave8's four branches, produces the new numbers too. **Nothing in wave8
moved the crypto signing path**, and the regenerated baseline is valid for both
trees.

### 8h. Noise floor, and the smallest regression each instrument can detect

**`make bench-crypto` — noise floor.** The 95 % interval of the mean-of-5 that
each run drew: **±0.07 %, ±0.09 %, ±0.08 %, ±0.13 %** on the four cases. Across
three independent runs of the *same* tree the index reproduced to a range of
**0.035 %–0.074 %**; across four runs including a separately-built merge-base
binary, **0.11 %–0.62 %**. That is roughly 8x tighter than the ±0.60 % the file's
own noise-floor table predicts for a mean-of-5 on this box, and it is what
pinning to core 63 on a quiet machine buys.

> **`make bench-crypto` can now fail on a movement of about 6.1 % (6.2 % on
> rsa2048) and cannot see anything smaller: the 6 % tolerance plus the ~0.1 %
> that the run's and the baseline's intervals add in quadrature.**

**`make bench-perf` — noise floor.** Per row, in the regenerated baseline's
`ratio-sd%` column: **0.04 % (interp, float/int-convert) to 13.49 %
(gc/live-heap-churn)**, median 0.16 %. Thirty of 42 rows are below 0.5 %.

> **`make bench-perf` can fail on a ~5.1 % movement on 31 of its 42 rows, needs
> 6.6–17 % on nine more, and 29 %, 31 % and 55 % on `text/sprintf`,
> `chase/dram` and `gc/live-heap-churn` — so a green run means "no row moved by
> more than its own `detect%` column", and that column is printed beside every
> row for exactly this reason.**

Distribution of `detect%` across the 42 rows: 5.0–5.5 % on 28 rows, 5.6–6.0 % on
3, 6.6–9.8 % on 4, 15.3–16.9 % on 4, and 29.4 / 31.3 / 55.1 % on the three
noisiest.

---

## Summary

| # | item | result |
|---|---|---|
| 0 | `GOC_BOUNDED_MEM2REG` inert when unset | **PASS** — 36/36 byte-identical, positive control differs 18/18 |
| 1 | `go test -timeout 40m -parallel 10 ./goc/...` | **PASS** — 0 failures, 692 PASS incl. subtests, 1020 s |
| 2 | `make test-goc-status` / `-opt`, both `-v` | **PASS / PASS** — 367/367 each, identical sets, `tcp-churn` green on `-O` |
| 3 | `make test-unit` | **PASS** |
| 4 | 4 audits + census twice | **PASS** — census output identical between runs |
| 5 | determinism + `TestParallelBackendIsByteIdenticalToSerial` | **PASS** (6 tests) |
| 6 | GC reducer 20x, `GOGC=10` and default, branch + `main` | **0/20 in all 8 cells** (160 runs) |
| 7 | flate crash rate | **0/440**; `main` control 2/300 with the documented signature |
| 8 | `bench-perf` / `bench-crypto` re-baselined + 2 confirming runs each | **PASS ×2 each** |
| 9 | slog benchmark | **PASS** — 30/32 as expected |
| 9 | gc differential | **FAIL** — wave-introduced stale reference, see below |

### The one thing this wave broke

`TestEscapeDifferentialAgainstGC` and `TestEscapeReasonDifferentialAgainstGC`
fail on 7983abd and **pass on both `main` (6034f73) and the merge base
(d2855f5)** — measured, both directions. `ccwork/flate-gc-crash` added a corpus
program and updated `alloc_census_baseline.txt` but not
`escape_gc_differential.txt` / `escape_gc_reason_differential.txt`. Every added
line in both diffs comes from that one new program; no existing program's verdict
changed. Both are opt-in, so neither gates `go test ./...`. Not repaired here, per
the brief. Fix is one `-update-...` command each, whose diff a person should read.

### Things worth knowing that are not failures

- `GOC_BOUNDED_MEM2REG=0` **enables** the pass (`os.Getenv(...) == ""`). Nothing
  sets the variable, so it does not affect this merge; it will bite whoever tries
  to turn the switch off explicitly.
- Comparing goc output across two git worktrees is invalid — goc embeds absolute
  source paths, so different worktree path lengths shift the whole image. The
  byte-identity check must be run from one path.
- `gcpress gc/live-heap-churn` in the regenerated perf baseline has detect% 55.1
  and is no longer a usable gate. That is a deliberate choice, argued in 8g.
- `make bench-crypto`'s numbers moved 2–5 % from the old committed file. That is
  the protocol change, not the compiler — proved by running the merge base
  against the new baseline (8f).

### Verdict

The mem2reg switch **is inert when off** — 36 of 36 programs byte-identical to
the pre-switch compiler at `-O` and without it, against a positive control that
differs on all 18.

The flate crash count is **0 in 440 runs**, against a `main` control that
produced 2 crashes in 300 with the documented bad-pointer signature.

**SAFE TO MERGE TO MAIN**, with one required follow-up: this wave staled
`goc/testdata/escape_gc_differential.txt` and
`goc/testdata/escape_gc_reason_differential.txt`, and both must be regenerated in
the merge or immediately after it. Every behavioural check in the tree is clean —
0 failures across the corpus, 367/367 on both capability arms, both timing
instruments green twice against baselines re-cut here — and the only red light is
a committed data file that describes a corpus one program smaller than the one
that now exists.
