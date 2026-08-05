# Wave 9 merge gate — turning goc's real optimiser on

`integration/wave9-gate`, built from `main` (6034f73) by merging, in order:

| # | branch | tip | what it carries |
|---|--------|-----|-----------------|
| 1 | `ccwork/wave8-gate` | `73aee03` | the wave-8 verification; re-cut `perf_suite_baseline.txt` and `crypto_signing_bench_baseline.txt` (the latter converted 5 → 7 columns by a protocol change) |
| 2 | `ccwork/wave8-differential-refresh` | `b4470ad` | regenerated escape differentials for the corpus program the flate fix added |
| 3 | `ccwork/enable-full-pipeline` | `5c62311` | both mem2reg blocker fixes **and** the pipeline change |

Merge result: `618427a`.

## 0. The merge itself

Merge 1 (`73aee03`) applied clean. Merge 2 (`b4470ad`) was a fast-forward — it
descends from `73aee03`. Merge 3 (`5c62311`) conflicted in exactly two files:

- `goc/testdata/perf_suite_baseline.txt` — both sides had re-cut it. **Not
  resolved by picking a side**; the conflict was closed with a placeholder and
  the file regenerated from the merged tree (§8).
- `CCWORK_REPORT.md` — prose, rewritten as this document.

Everything else auto-merged, including `goc/testdata/alloc_census_baseline.txt`
(each prerequisite branch had added its own reducer's sites) and both escape
differentials. **Auto-merged is not accepted**: every generated file listed in
the brief was regenerated from the merged tree and its diff read (§§ below).

`go build ./...` on the merged tree: clean.

### Capability count, corrected

The brief says the matrix goes 366 → 368 because "two capabilities were added by
the mem2reg branches". The count is right, the attribution is not. Diffing
`cmd/goc/runtime_status_test.go` against `main`, the two added `source:` entries
are:

- `runtime_gc_slice_tail_pointer.go` — from **wave 8**'s slice-tail fix, not a
  mem2reg branch.
- `runtime_gc_promoted_local_root.go` — from `ccwork/mem2reg-gc-visibility`.

A third new corpus program, `runtime_opt_promoted_interface_root.go`
(mem2reg-iface-dispatch), is in `goc/testdata` and so is in the four corpus
audits, but it is **not** wired into the capability matrix. That is why the
matrix gains two and the census gains three programs' worth of sites.

---

_(run in progress — sections appended as each item completes)_

## Item 3 — `make test-unit`

**PASS**, exit 0. 25 packages reported `ok`, 12 `[no test files]`, zero `FAIL`,
and no line was `(cached)` — every package was compiled and run on this tree.


## Item 4a — the four corpus audits, regenerated from the merged tree

Regenerated in one corpus pass (they share `auditCorpus`'s `sync.Once`):

    go test ./goc -run 'TestAllocationCensus|TestFrameEscapeAudit|TestLoopAliasAudit|TestEscapeShadowPlacement' \
        -update-alloc-census-baseline -update-frame-escape-baseline \
        -update-loop-alias-baseline -update-escape-shadow-baseline -v

`ok github.com/evanphx/cg12/goc 201.305s`, exit 0, wall 3:23.67, peak RSS
13.84 GiB (the corpus audit compiles the whole corpus in parallel).

**All four regenerated files came out byte-identical to the merged tree's
committed versions** — `git status goc/testdata/` was empty afterwards. The
auto-merge of `alloc_census_baseline.txt` from the two prerequisite lines
happened to be exactly right, but that is now a measured fact rather than a
merge artefact.

### What moved against `main`, and why

| baseline | vs `main` |
|---|---|
| `frame_escape_baseline.txt` | **no change** |
| `loop_alias_baseline.txt` | **no change** |
| `escape_shadow_baseline.txt` | **no change** |
| `alloc_census_baseline.txt` | **+19 lines, −0** |

All 19 added census lines are in the three corpus programs the merge adds, and
nothing else moved in any direction — no `HEAP -> FRAME`, no `FRAME -> HEAP`, no
vanished site:

- `runtime_gc_promoted_local_root.go` — 8 sites (mem2reg-gc-visibility's reducer)
- `runtime_gc_slice_tail_pointer.go` — 6 sites (wave 8's slice-tail reducer)
- `runtime_opt_promoted_interface_root.go` — 4 sites (mem2reg-iface-dispatch's reducer)

The nineteenth is `?  main.make2  runtime.newobject  error  heap` — a site with
no source position. `main.make2` exists only in
`runtime_gc_promoted_local_root.go`, so it belongs to the same new program; it is
a compiler-synthesized `error` boxing that carries no position. It is an
addition, on the heap, in new code, so it raises none of the census's five
review questions.

**Why the optimiser changing placement did not show up here, and why that is
correct**: `auditCorpus` compiles with `goc.CompileExecutable` and never calls
`opt.OptimizeModule`, so all four audits read *unoptimized* IR. They are a real
guard that the merge did not disturb the front end. They are **not** evidence
about the pipeline, and a reader should not take their silence as such.


## Item 2 — the capability matrix, both arms

Run as `GOFLAGS=-v make test-goc-status` then `GOFLAGS=-v make test-goc-status-opt`,
`STATUS_TIMEOUT=90m`, sequentially, each waited on to exit.

### Default arm — `make test-goc-status`

**368 PASS, 0 FAIL, 0 SKIP.** `--- PASS: TestARM64RuntimeCapabilityStatus (141.03s)`,
`ok github.com/evanphx/cg12/cmd/goc 141.046s`, exit 0. Not cached.

The two capabilities over `main`'s 366 are `gc-invariants/slice-tail-pointer`
(wave 8) and `gc-invariants/promoted-local-root` (mem2reg-gc-visibility); both
PASS. Note this arm compiles **without** `-O`, so `opt.OptimizeModule` never
runs on it: it is a guard on the merge, not on the pipeline. The `-O` arm below
is the one that exercises this wave's change.

### `-O` arm — `make test-goc-status-opt`

**368 PASS, 0 FAIL, 0 SKIP.** `--- PASS: TestARM64RuntimeCapabilityStatus (311.92s)`,
`ok github.com/evanphx/cg12/cmd/goc 311.936s`, exit 0. Not cached.

**The two PASS sets are identical, capability for capability** (diffed as sorted
sets of subtest names: no line differs). This is the wave's central correctness
guard: it is the arm that runs `DefaultPipeline` — thirteen passes that had never
executed on a Go program — over every one of the 368 capability programs and the
pack they link against, and it agrees exactly with the unoptimized arm.

The `-O` arm took 311.92s against the default arm's 141.03s, a **2.21x
wall-clock ratio on the whole matrix**, which is the compile-time cost of this
wave showing up in the cheapest place to see it. (It is below the 4.5x
single-program figure because the matrix's wall clock includes execution and
its compiles run 64-wide.)


## Items 6 and 7 — the GC reducer and the two crash loops

All programs were built **with `-O`**, which is the configuration this wave
changes: `opt.OptimizeModule` is only reached on an optimized build, so an
unoptimized run of these programs would be a guard on nothing.

### Item 6 — `runtime_gc_type_mask_padding.go`, 20 runs each, `GOMAXPROCS=3`

| tree | default `GOGC` | `GOGC=10` |
|---|---|---|
| `integration/wave9-gate` (`-O`, full pipeline) | **0 / 20 failed** | **0 / 20 failed** |
| `main` 6034f73 control (`-O`, bounded pipeline) | **0 / 20 failed** | **0 / 20 failed** |

Both trees are clean, so the reducer attributes nothing to this wave — which is
the expected and required result: it is the wave-7 mask-padding fix's guard, and
turning the optimiser on must not reopen it. Every run's exit status was checked;
the program `panic`s on a bad word, so a non-zero exit is the failure signal.

### Item 7 — the two crash loops that motivated the blocker fixes

| loop | required | measured |
|---|---|---|
| `placement_bench/flate`, `-O`, default `GOGC` | 0 over ≥250 | **0 / 250** |
| `placement_bench/flate`, `-O`, `GOGC=10` | 0 over ≥250 | **0 / 250** |
| `placement_bench/p256`, `-O`, `GOGC=10` | 0 over ≥100 | **0 / 100** |

620 runs, zero non-zero exits. Both programs `panic` on a wrong answer
(`"signature did not verify"` in p256; decompression mismatch in flate), so this
is a correctness loop and not only a crash loop.

These ran **while `go test ./goc/...` was saturating the box**, which makes the
zero stronger rather than weaker: the collector was under more scheduling
pressure than the branch's own 0/500 and 0/100 measurements had.


## Item 1 — `go test -timeout 60m -parallel 10 ./goc/...`

    ok  github.com/evanphx/cg12/goc  1347.907s

exit 0, **0 failures**. Wall 22:28.51, 4589.94 user + 150.25 system CPU-seconds,
351% average CPU, peak RSS 13.52 GiB. Run with `-count=1`; there is no `(cached)`
line in the output and could not be — every test was compiled and executed on
this tree.

`TestDeriveClassifiesEveryGenField` **PASSES**. It has failed in five waves; it
does not fail here. Re-run on its own to be sure it was not merely skipped:

    === RUN   TestDeriveClassifiesEveryGenField
    --- PASS: TestDeriveClassifiesEveryGenField (0.00s)

`main` (6034f73) is the tree that fixed it — its head commit is *"goc: derive
must reset the deep-escape question"* — so this wave inherits the fix rather than
re-establishing it. There is no field to name.

### Census differences attributable to the commits that add tests

The only census movement in the whole merge is the +19 lines of §4a, and every
one is in one of the three corpus programs the merge adds. The commits are
`ccwork/mem2reg-gc-visibility` (`runtime_gc_promoted_local_root.go`),
`ccwork/mem2reg-iface-dispatch` (`runtime_opt_promoted_interface_root.go`) and
wave 8's slice-tail fix (`runtime_gc_slice_tail_pointer.go`). Nothing in the
pre-existing corpus moved in either direction.

**Control**: `main`'s four committed audit baselines were regenerated in the
`main` worktree by the same command and came out **byte-identical** to what
`main` has committed. The corpus audit is therefore reproducible on this box,
which is what makes the +19 a real difference rather than run-to-run noise.

## Item 5 — determinism and the parallel backend

`TestParallelBackendIsByteIdenticalToSerial` (`./arm64`): **PASS**, all six
worker counts (1, 2, 3, 8, 64, 256), `ok github.com/evanphx/cg12/arm64 0.223s`,
exit 0, `-count=1`.

`scripts/determinism-check.sh`, default arm — five programs, each built cold
(`CG12_NOCACHE=1`) and warm, two rounds, all four hashes per program equal:

    hello.go                            round1:identical(951709b19f9a626b)  round2:identical(951709b19f9a626b)
    fmt_sprintf.go                      round1:identical(3230e0ac5b7cdebf)  round2:identical(3230e0ac5b7cdebf)
    gc_struct.go                        round1:identical(e3f954773ded353a)  round2:identical(e3f954773ded353a)
    runtime_cleanup_frame_retention.go  round1:identical(8a5f916a5cd5dec7)  round2:identical(8a5f916a5cd5dec7)
    runtime_defer_capture_allocs.go     round1:identical(285733a189f227f8)  round2:identical(285733a189f227f8)
    DEFAULT_EXIT 0

**5/5, both caching paths, both rounds.** The `-O` arm is reported below.

`scripts/determinism-check.sh -O` — the arm this wave actually changes, since
`opt.OptimizeModule` is only reached on an optimized build:

    hello.go                            round1:identical(72f3b26d6ea60972)  round2:identical(72f3b26d6ea60972)
    fmt_sprintf.go                      round1:identical(0d5afdf8645bb423)  round2:identical(0d5afdf8645bb423)
    gc_struct.go                        round1:identical(69e8abb19da304b3)  round2:identical(69e8abb19da304b3)
    runtime_cleanup_frame_retention.go  round1:identical(09f3cc3645d5703f)  round2:identical(09f3cc3645d5703f)
    runtime_defer_capture_allocs.go     round1:identical(3444a6675d7e01c6)  round2:identical(3444a6675d7e01c6)
    OPT_EXIT 0

**5/5, both caching paths, both rounds, with all thirteen passes running.** Every
hash differs from the default arm's, which is the point: the optimiser changed
the image and it changed it reproducibly.

## Item 10 — the gc differential and the slog benchmark

### `escape_gc_differential.txt` — regenerated, **+40 / −18**

    go test ./goc -run TestEscapeDifferentialAgainstGC \
        -escape-gc-differential -update-escape-gc-differential

Check arm afterwards (no `-update`): **PASS**, `compared 402 of 406 corpus
programs, 1879 census rows, 3548 gc decisions; permissive 1483, pessimistic 401`.

**Every one of the 18 removed lines is a count** — a header total, a cell of the
placement matrix, or a construct-histogram bucket. **Every added data row is in
one of the two corpus programs the merge adds**, and no pre-existing line changed
its classification:

    runtime_gc_promoted_local_root.go:59     mixed  -> heap
    runtime_gc_promoted_local_root.go:65     absent -> mixed
    runtime_gc_promoted_local_root.go:121    frame  -> heap
    runtime_opt_promoted_interface_root.go:61,64,77   absent -> heap

The corpus count moves 404 → 406 exactly as expected: branch 2 had already
refreshed this file for wave 8's `runtime_gc_slice_tail_pointer.go`, and the two
mem2reg reducers are what is new.

`runtime_gc_promoted_local_root.go:121 frame -> heap` is in the permissive
(correctness-critical) direction and deserves the sentence the census asks for:
goc frames a `1_any` at column 2 — the `[]any` backing for `fmt.Println` — while
gc heaps the *string constant* at column 14. They are different objects joined
only by sharing a source line, which is this instrument's documented coarseness
(`internal/gcdiff`: the key is (file, line) because columns do not survive the
trip). It joins the 1,483-line permissive population whose largest bucket,
`- | object/heap` at 1,296, is exactly this pattern. `TestFrameEscapeAudit` — the
instrument that reads the *emitted stores* rather than a line join — reports no
new publication anywhere in the corpus.

### `escape_gc_reason_differential.txt` — regenerated, **+93 / −26**

Check arm: **PASS**, `reasons on both sides for 402 programs; 1115 goc rules,
2227 gc explanations joined; agree-placement/disagree-reason 315,
disagree-placement/agree-reason 85`. Same shape as above: all 26 removals are
counts, all added rows are in the two new programs.

### `slog_allocations_baseline.txt` — regenerated, **no change at all**

    go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations -update-slog-allocations
    32 cases; baseline rewritten
    git status goc/testdata/ -> empty

Check arm: **PASS** (11.93s, 32 cases).

The brief expected this to move "because placement changed". **It did not, and
the reason is structural rather than lucky**: `buildWithGoc` in
`goc/slogalloc_test.go:204` invokes the driver as `driver -o binary program` —
**no `-O`**. `opt.OptimizeModule` is never reached, so no pass in
`DefaultPipeline` can touch this measurement. The same is true of
`escape_gc_differential.txt`, which reads goc's side out of the committed census
rather than compiling anything with goc.

**This is worth stating plainly: of the nine generated files in the brief, only
three are capable of seeing the pipeline change at all** — the two timing
baselines (`perf_suite_baseline.txt`, `crypto_signing_bench_baseline.txt`) and
nothing else. The six analysis baselines all read unoptimized IR or a
pre-existing census by construction. A reviewer should not read their stability
as evidence that the optimiser is safe; the matrix's `-O` arm, the `-O`
determinism arm and the crash loops are that evidence.


## Item 9 — compile-time cost, measured independently

### Single small program, sequential, idle box, 3 repetitions per arm

`goc -O -o out goc/testdata/fmt_sprintf.go` — a 10-line program whose module is
the stdlib closure. Repetitions agree to within 1%, so the means below are the
measurement, not a sample of one.

| arm | wall (s) | user CPU (s) | peak RSS |
|---|---:|---:|---:|
| `main` 6034f73 (bounded pipeline was the default) | **6.63** | 20.66 | 0.59 GiB |
| branch, default (full pipeline) | **41.37** | 81.57 | 0.90 GiB |
| branch, `GOC_OPT_PIPELINE=bounded` | 6.66 | 20.75 | 0.57 GiB |
| branch, `GOC_OPT_SKIP=mem2reg` | 31.50 | 66.40 | 0.85 GiB |
| branch, `GOC_OPT_SKIP=clean` | 10.38 | 33.93 | 0.93 GiB |
| branch, `GOC_OPT_SKIP=inline-fixpoint` | 10.70 | 25.48 | 0.56 GiB |
| branch, `GOC_OPT_SKIP=inline` | 11.98 | 27.35 | — |
| branch, `GOC_OPT_SKIP=mem2reg,inline` | 12.40 | 29.30 | — |
| branch, `GOC_OPT_SKIP=gvn` | 35.45 | 72.93 | — |

**6.24x on wall clock, 3.95x on CPU-seconds.** The brief's 4.5x sits between the
two; both are reported here because they are different claims and the branch's
figure does not say which it is.

`GOC_OPT_PIPELINE=bounded` on the branch reproduces `main` to 0.5% (6.66 vs
6.63). That matters: it says the entire cost is the pipeline selection and
**nothing else the branch changed contributes measurably** — the mem2reg fixes,
the nosplit inliner restriction and the pack-cache key are all free.

### Attribution — the branch's diagnosis is right, with a correction

The branch attributes the cost to the `clean` fixpoint re-converging over 5101
functions, and explicitly **not** to mem2reg (0.37s of 24.7s). The skip arms
above confirm the conclusion and sharpen the mechanism. Cost above the bounded
floor is 41.37 − 6.66 = **34.71s**; each arm removes:

| removed | seconds removed | share of the 34.71s |
|---|---:|---:|
| `clean` (the whole cleanup set, everywhere) | 30.99 | **89.3%** |
| the two `inline-fixpoint` blocks | 30.67 | 88.4% |
| the `inline` pass alone, leaving both cleanup fixpoints running | 29.39 | **84.7%** |
| `gvn` | 5.92 | 17.1% |
| `mem2reg` | 9.87 | 28.4% |

The 84.7% row is the informative one. Removing **only the inliner** — every
`clean` round still runs, over the same 5101 functions — removes 85% of the
cost. So `clean` is where the cycles are spent, but `clean` over an un-inlined
module costs only 5.3s (11.98 − 6.66); it is the inliner enlarging and dirtying
the module that gives `clean` its work. The two are a product, and removing
either collapses the cost.

**The correction is to the mem2reg row.** Skipping mem2reg with the inliner *on*
removes 9.87s (28%), which looks like it contradicts "0.37s of 24.7s". It does
not: skipping mem2reg on top of skipping the inliner makes the compile *slower*,
not faster (12.40s vs 11.98s). mem2reg's own cost really is negligible; the 9.87s
is entirely mem2reg's **indirect** cost — promotion exposes inlining
opportunities, and the inliner then does more work. A CPU profile's self-time for
the pass (which is what 0.37s reads like) cannot see that, and an end-to-end skip
cannot separate it. Both numbers are right about different questions.

### Corpus-wide, 406 programs, `-O`, whole-program builds

Each program compiled on its own at `GOMAXPROCS=1` (so CPU-seconds are the work
done and not contention), 32 compiles concurrently. Restricted to the **403
programs both trees have**:

| tree | CPU-seconds (user+sys) |
|---|---:|
| `main` 6034f73 | **4,733.9** |
| `integration/wave9-gate` | **21,157.7** |

**4.469x.** The branch reported 2877 → 13030 CPU-seconds, which is **4.53x**.
The ratio reproduces to within 1.5% on an independent harness; the absolute
numbers differ because the per-compile concurrency differs, and the ratio is the
claim. Confirmed.

The cost is uniform, not concentrated: the ten heaviest programs all land between
5.10x and 5.59x, and the biggest single compile
(`stdlib_http_tls_client_server`) goes 73.6 → 380.5 CPU-seconds.

### Memory — this is the finding that matters most in this section

The brief asks to watch the `net/http` pack against a 3 GiB ceiling. Measured on
both trees, `goc build-runtime -O -packages net/http`:

| tree | wall | user CPU | **peak RSS** |
|---|---:|---:|---:|
| `main` | 29.34s | 70.84s | 2.06 GiB |
| branch | 215.68s | 350.61s | **2.83 GiB** (2,965,428 kB) |

That reproduces the branch's report (it quotes 2.99 GiB; the difference is GiB
vs GB on the same measurement) and it is under 3 GiB.

**But the pack is not the worst case, and the corpus sweep found the worse one.**
Peak RSS of the whole-program `-O` compile, over all 403 common programs:

| | `main` | branch |
|---|---:|---:|
| programs peaking over **3.0 GiB** | **0** | **6** |
| largest single compile | 2.85 GiB (`stdlib_http_tls_client_server`) | **4.23 GiB** (same program) |

The six are `stdlib_http_tls_client_server` (4.23), `stdlib_http_redirect_keepalive`
(4.15), `stdlib_http_client_server` (4.06), `stdlib_http_cookiejar` (3.93),
`stdlib_http_multipart_form` (3.88) and `stdlib_http_parse_roundtrip` (3.85).

The module budget that this wave deletes was introduced (48200ab) to hold peak
memory under a 3 GiB ceiling. On this tree, **six corpus programs are already
over that ceiling on a whole-program Go build**, at up to 1.41 GiB above it,
where `main` had none over it. The branch's own mitigation proposal — reinstate a
module budget on `internal/prebuilt.BuildRuntime` alone — would **not** cover
these, because they are program modules, not the pack. If the 3 GiB ceiling is
real on any target machine, this is the thing that will hit it, and it is not
what the branch is watching.


## Item 8 — the two timing instruments, regenerated and checked

Run on an idle box, sequentially, nothing else on the machine. `bench-perf` pins
to core 62 and `bench-crypto` to core 63 and the tree says they may overlap; they
were still run one at a time, because each rebuilds its programs with `goc -O`
and a build phase is not pinned.

### `perf_suite_baseline.txt` — regenerated (`make bench-perf-update`)

`--- SKIP: TestPerformanceSuite (854.88s)` (the skip is how `-update` reports),
exit 0. The merge conflict in this file was resolved by **this measurement**, not
by either side.

**Confirming run 1** — `make bench-perf` against the file just written:
`--- PASS: TestPerformanceSuite (853.90s)`, exit 0, all 42 rows within tolerance.

The regenerated file reproduces branch 3's committed values row for row within
each row's own noise (e.g. `interp` control 0.9269 → 0.9265, `float/sqrt-sum`
0.9991 → 0.9990, `json/marshal` 14.7980 → 14.7342). Two rows moved more than a
percent and both are rows the file itself marks as loud: `sortmap/map/build-probe`
(5.6277 → 6.0921, ratio-sd 4.83%, tol 14.5%) and `chase/dram` (1.0346 → 1.0196,
tol 8.8%).

### The control ratio, and every measurement

**Control: 1.6296x → 0.9260x** (mean of the eleven `control/spin-fixed-work`
rows, before = wave 8's baseline at `73aee03`, after = this run). The claim is
0.9262x; it reproduces to 0.02%.

**All 42 measurements improved. None regressed.** (The brief says 44; the suite
has 42 rows — 11 controls plus 31 workload cases. Counted from both the before
and after files.)

| program | case | before | after | change |
|---|---|---:|---:|---:|
| float | float/dot-product | 4.9424 | **1.4991** | −69.7% |
| sortmap | sort/ints | 3.8162 | **1.5375** | −59.7% |
| text | text/utf8-decode | 4.4520 | **2.1409** | −51.9% |
| float | float/mandelbrot | 2.5878 | **1.3026** | −49.7% |
| gcpress | gc/live-heap-churn | 8.7016 | **4.6294** | −46.8% |
| flate | control/spin-fixed-work | 1.6297 | **0.9247** | −43.3% |
| chase | control/spin-fixed-work | 1.6298 | **0.9260** | −43.2% |
| conc | control/spin-fixed-work | 1.6297 | **0.9249** | −43.2% |
| float | control/spin-fixed-work | 1.6296 | **0.9251** | −43.2% |
| json | control/spin-fixed-work | 1.6305 | **0.9262** | −43.2% |
| regexp | control/spin-fixed-work | 1.6294 | **0.9247** | −43.2% |
| sortmap | control/spin-fixed-work | 1.6295 | **0.9248** | −43.2% |
| interp | control/spin-fixed-work | 1.6292 | **0.9265** | −43.1% |
| sha | control/spin-fixed-work | 1.6294 | **0.9266** | −43.1% |
| text | control/spin-fixed-work | 1.6303 | **0.9276** | −43.1% |
| gcpress | control/spin-fixed-work | 1.6293 | **0.9284** | −43.0% |
| conc | chan/send-buffered | 4.7826 | 2.8896 | −39.6% |
| gcpress | gc/alloc-churn | 9.8475 | 5.9451 | −39.6% |
| text | text/sprintf | 10.7332 | 6.6966 | −37.6% |
| flate | flate/decompress | 7.5504 | 4.7187 | −37.5% |
| float | float/int-convert | 1.5444 | **1.0004** | −35.2% |
| conc | chan/pingpong-unbuffered | 6.6089 | 4.3782 | −33.8% |
| regexp | regexp/anchored-lines | 6.0837 | 4.0798 | −32.9% |
| chase | chase/l1-resident | 1.4566 | **1.0011** | −31.3% |
| text | text/format-append | 10.9524 | 7.6592 | −30.1% |
| conc | mutex/uncontended | 1.8626 | 1.3155 | −29.4% |
| flate | flate/compress | 6.7972 | 4.9410 | −27.3% |
| text | text/parse | 10.2902 | 7.7207 | −25.0% |
| conc | goroutine/spawn-join | 5.3249 | 4.0102 | −24.7% |
| sortmap | map/build-probe | 7.7366 | 6.0921 | −21.3% |
| regexp | regexp/find-submatch | 7.9350 | 6.4397 | −18.8% |
| gcpress | gc/pointer-write | 9.2299 | 7.5793 | −17.9% |
| json | json/unmarshal | 11.2835 | 9.2838 | −17.7% |
| regexp | regexp/replace | 7.1710 | 5.9579 | −16.9% |
| sortmap | sort/slice-callback | 3.7323 | 3.1340 | −16.0% |
| json | json/marshal | 17.4048 | 14.7342 | −15.3% |
| float | float/sqrt-sum | 1.1289 | **0.9990** | −11.5% |
| interp | interp/bytecode-loop | 21.3077 | 19.0659 | −10.5% |
| chase | chase/dram | 1.0349 | 1.0196 | −1.5% |
| chase | chase/pointer-node | 1.0121 | **0.9986** | −1.3% |
| sha | sha/hmac-1mib | 1.0150 | 1.0110 | −0.4% |
| sha | sha/sha256-1mib | 1.0068 | 1.0052 | −0.2% |

Seven rows are now at or below the host toolchain: all eleven controls, plus
`float/sqrt-sum`, `float/int-convert`, `chase/pointer-node`, `chase/l1-resident`
and (within noise) `chase/dram`.

### A control for the headline, measured on this box from a different program

The "before" figure of 1.6294 comes from a baseline another job produced on
another day. That is not a control, so one was measured here. `placement_bench/flate`
contains the identical `control/spin-fixed-work` loop, is not part of the perf
suite, and exists on both trees. Built with each tree's own compiler at `-O` and
run alternately, pinned to core 61, three repetitions each:

| tree | control/spin-fixed-work (ns) |
|---|---:|
| `main` 6034f73 (`goc-main -O`, bounded pipeline) | 54,681,364 / 54,744,324 / 54,717,964 |
| branch (`goc-branch -O`, full pipeline) | 31,010,298 / 31,000,018 / 31,004,618 |

Means 54,714,551 and 31,004,978 — **1.765x faster**. Against the host control
time this run measured for the same loop (33,500,642 ns), that is **1.633x before
and 0.9255x after**, from a program the perf suite never touches. The headline is
reproduced independently.

**Confirming run 2** — `make bench-perf` again, fresh binaries, fresh timings:
`--- PASS: TestPerformanceSuite (855.29s)`, exit 0. Its eleven controls mean
**0.9259x** against the update run's 0.9260x. The baseline was written by one run
and then held to by two runs that did not produce it.

### `crypto_signing_bench_baseline.txt` — regenerated (`make bench-crypto-update`)

`--- SKIP: TestCryptoSigningBench (168.84s)`, exit 0. This file had **never** been
re-cut with the pipeline on: wave 8 converted it from 5 columns to 7 and branch 3
left it alone, so every row on the merged tree was far outside its 0.06 tolerance
before this run.

**Confirming runs 1 and 2** — `make bench-crypto` twice against the new file:
`--- PASS (170.46s)` and `--- PASS (170.61s)`, exit 0 both, all four rows *within
tolerance (6%)* in both, worst deviation 0.1% of the index.

| case | index before | index after | goc ns before | goc ns after | goc speedup |
|---|---:|---:|---:|---:|---:|
| p256/sign-verify | 44.8636 | **24.0648** | 2,448,557,393 | 745,617,014 | **3.28x** |
| p256/verify | 32.7001 | **16.9991** | 1,784,703,100 | 526,695,764 | **3.39x** |
| p384/sign-verify | 39.1565 | **20.3676** | 2,137,080,720 | 631,062,255 | **3.39x** |
| rsa2048/sign-verify | 12.1813 | **2.3470** | 664,828,410 | 72,717,829 | **9.14x** |

The host columns are the internal control and they did not move: `gc index`
1.6312 → 1.6290, 1.1652 → 1.1651, 2.8740 → 2.8738, 0.5929 → 0.5930, and host ns
within 0.2% on every row. The entire movement is on goc's side.

### goc against the host, every measurement in both instruments

`crypto_signing_bench` is an index against each binary's own control, so the
goc-versus-host ratio has to be formed from the raw nanoseconds:

| case | goc/host before | goc/host after |
|---|---:|---:|
| p256/sign-verify | 44.80x | **13.67x** |
| p256/verify | 45.74x | **13.50x** |
| p384/sign-verify | 22.20x | **6.56x** |
| rsa2048/sign-verify | 33.48x | **3.66x** |

For the performance suite the ratio *is* the reported number, so the 42-row table
above is the goc-versus-host figure for every measurement in it. Combined, this
gate reports 46 goc-versus-host measurements across the two instruments, and
**every one of them improved**.


## Item 4b — the four audits in check mode, and the census run twice

After the regeneration pass of §4a, the four audits were run again in **check**
mode (no `-update`), which is a second, independent census over the corpus:

    --- PASS: TestAllocationCensus (184.07s)
    --- PASS: TestEscapeShadowPlacement (0.00s)
    --- PASS: TestFrameEscapeAudit (0.00s)
    --- PASS: TestLoopAliasAudit (0.00s)
    ok  github.com/evanphx/cg12/goc  184.407s

exit 0. So the census ran **twice** on this tree — once writing, once checking —
plus a third time inside `go test ./goc/...` (§1), and all three agree. The
shadow-placement totals are identical between the two passes (203,220 front-end
placements, 186,155 agreements, 16,115 conservative and 950 permissive
disagreements, 810 distinct disagreement sites), so the corpus audit is
deterministic on this box.

The tests that read a regenerated file *unconditionally* were re-run after the
regeneration, because `go test ./goc/...` had read the pre-regeneration versions:
`TestReasonPositionsAreRepositoryRelative`, `TestReasonTaxonomyCoversBothVocabularies`,
`TestGocFlagMParsesIntoSites`, `TestGCExplanationsParseTheFlowChain`,
`TestGocFlagMStillParsesAsGCFlagM`, `TestCompareAllocationCensus*` — **all PASS**,
`ok github.com/evanphx/cg12/goc 0.097s`.


## The memory finding, stated on its own

`cmd/goc/runtime_status_test.go:2915`:

    // It is measured rather than guessed: the largest net/http program peaks at
    // 2.65 GiB inside the matrix and 2.97 GiB compiled on its own, so the 2 GiB
    // this used to assume was under-provisioned by a third. Under-provisioning
    // here is not a slowdown -- the bound exists so an unbounded fan-out cannot
    // swap or OOM a small machine, and a divisor below the real peak lets it do
    // exactly that.
    const compileRuntimeCapabilityPeakBytes = 3 << 30

That constant is **live code**: `compileRuntimeCapabilityWorkers()` divides
`MemAvailable` by it to decide how many compiles to run at once. On this tree the
number it is measured from has moved, and the constant has not.

`stdlib_http_tls_client_server.go`, compiled on its own with `-O`, peak RSS:

| GOMAXPROCS | `main` 6034f73 | branch |
|---|---:|---:|
| 1 (the corpus sweep's setting) | 2.85 GiB | **4.23 GiB** |
| default (64) | 2.46 GiB | **3.17 GiB** |

Six corpus programs exceed 3 GiB on the branch at `GOMAXPROCS=1`; **none** do on
`main`. The `net/http` *pack* the branch reports at 2.99 GiB is real and is under
the line, but it is not the worst case — program modules are, and the branch's
proposed mitigation (a module budget on `internal/prebuilt.BuildRuntime` alone)
does not reach them.

This is the one number in this gate that is worse than `main` and is not the
advertised price of the wave.


## Does the compile-time cost break CI? Measured, and no.

The 4.5x is the wave's advertised price, but CI has hard `timeout-minutes` on
every job, so the price was checked against the budget rather than assumed to
fit. A CI shard was simulated by pinning the job to **4 cores** (`taskset -c 0-3`,
GitHub's `ubuntu-24.04-arm` runner size) with the same sharding CI uses, and with
a **cold pack cache** (`CG12_PACK_CACHE` pointed at an empty directory), because a
warm cache hides the pack build entirely:

    taskset -c 0-3 make test-goc-status-opt STATUS_SHARDS=4 STATUS_SHARD=0

| tree | 4-core shard, cold pack | CI budget |
|---|---:|---:|
| `main` 6034f73 | 216.5s | 25 min |
| branch | **475.0s** | 25 min |

**2.19x, and 7.9 minutes against a 25-minute budget.** Both passed. The reason
the matrix costs so much less than the 4.5x whole-program figure is that every
capability links a prebuilt pack, so the module `OptimizeModule` sees is the
program and not the stdlib closure — which is also why the pack's own cost
(cold-cache) is the part that grew most.

A warm-cache run of the same shard took 175.4s on the branch and 213.2s on
`main`; that pair is contaminated by the branch's pack already being in the
shared cache from an earlier run here, and is recorded only so the cold-cache
numbers above are not mistaken for the same measurement.


### `main` control for item 1's cost

`go test -timeout 60m -parallel 10 -count=1 ./goc/...` on the `main` worktree,
same box, same flags:

| | `main` 6034f73 | branch | ratio |
|---|---:|---:|---:|
| package wall | 1005.152s | 1347.907s | **1.34x** |
| CPU (user+sys) | 4250.3s | 4740.2s | **1.12x** |
| peak RSS | 13.60 GiB | 13.52 GiB | 0.99x |
| failures | 0 | 0 | — |

The suite costs 12% more CPU, not 4.5x, because almost all of it compiles
*without* `-O` — `auditCorpus` and the corpus runner call `goc.CompileExecutable`
and never reach `opt.OptimizeModule`. The handful of tests that do optimize
(`goc/optgcroot_test.go`, `goc/loopalias_test.go`, `runCase(optimized: true)`)
are long serial compiles, which is why wall clock grew 34% while CPU grew 12%.
Part of even that is the three corpus programs the merge adds.

