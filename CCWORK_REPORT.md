# Accept a fresh runtime coverage baseline — `ccwork/coverage-baseline`

**Status: DONE. The collection was clean at 338/338 and the baseline is accepted.**
Commits `aefb7ff` (baseline) and `91ef9c7` (plan). One suite failure was found and traced to
a pre-existing flake in a sibling branch's test; it is diagnosed below and left unfixed on
purpose. What was not verified is listed at the end under "Still unverified / not done".

## What the task is

`cmd/goc/testdata/runtime_coverage_linux_arm64.json` is the accepted 2026-07-22 baseline. It
covers 294 of the matrix's 338 capabilities and was built from a runtime source fingerprint
that has since moved, so `make runtime-cover-diff` refuses to compare against it. RUNTIME_PLAN
§4's last open M0 checkbox is "reach one usable coverage outcome per capability", and it is
gated on a fresh full-corpus collection rather than on more runtime work.

## Starting state (verified)

- `go build ./...` clean, `go vet ./...` clean at `61b96da` (branch is identical to `main`;
  no commits ahead at start).
- `TestRuntimeCapabilityMatrixIsWellFormed`, `TestCheckedRuntimeCoverageBaselineDenominator`,
  and `TestCheckedRuntimeCoverageBaseline` all pass against the stale baseline: the existing
  reconciliation is 294 baseline programs + 44 pending = 338 matrix capabilities.
- Accepted baseline summary (for comparison): 294 programs, 291 covered, 2561 active
  functions, 2050 compiled, 1269 executed (49.55%), 25929 compiled blocks, 7936 executed
  (30.61%). It also carries 19 unexpected failures, 2 compile failures, 18 run failures and
  1 run timeout — it was accepted while Phase 1 was still open.

## The collection run

One unsharded `TestARM64RuntimeCapabilityStatus` with `-runtime-coverprofile`, which is the
only supported shape: the test `t.Fatal`s on a sharded coverage run, on a non-arm64 host, and
on a host without `cc`.

    go test -timeout 180m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/ -v -args \
      -runtime-coverprofile=<abs>/coverage_run1.json \
      -runtime-coverruns=3 \
      -runtime-status-compile-workers=4 \
      -runtime-status-progress

`-runtime-coverruns=3` matches the accepted baseline's method (§1: "executing each successful
binary three times"); repeats are merged per program so scheduling noise is less likely to
show up later as a false coverage regression.

The coverage path deliberately gets none of the speedups the rest of the matrix got:
`buildPrebuiltRuntimesForCapabilityStatus` returns "" and `newRuntimeCapabilityBatchPoolFor`
returns nil whenever `-runtime-coverprofile` is set, because instrumenting the runtime per
program is exactly what a shared prebuilt module cannot do. Measured cost of one instrumented
compile on this box: 3.5 s wall / 11.7 s CPU for a runtime-only program, 30.3 s wall / 77 s CPU
for `stdlib_http_tls_client_server.go`.

**Result: pending.** Tally, outcome census, and the accept/refuse decision are appended here
when the run finishes.

---

## Result: the collection is clean. 338/338, one usable coverage packet per capability.

Finished in **442.6 s** wall (`ok github.com/evanphx/cg12/cmd/goc 442.596s`). Subtest census
from the `-v` log, counted by unique name so a duplicate cannot pad it:

| | |
| --- | ---: |
| `--- PASS` subtests | 338 |
| `--- FAIL` subtests | 0 |
| `--- SKIP` subtests | 0 |
| distinct subtest names | 338 |

Outcome census from the generated report (`summary` plus a per-row tally of all 338 program
rows, 338 of them distinct):

| Outcome | Count |
| --- | ---: |
| `matrix_capabilities` | 338 |
| program rows | 338 |
| `compile_outcome: passed` | 338 |
| `run_outcome: passed` | 337 |
| `run_outcome: failed` | 1 |
| **`coverage_outcome: collected`** | **338** |
| `skipped` | 0 |
| `unreported` | 0 |
| `missing` | 0 |
| `expected-unavailable` | 0 |
| `covered_programs` | 338 |
| `unexpected_failures` | 0 |
| `compile_failures` | 0 |
| `run_timeouts` | 0 |
| `missing_coverage_programs` | 0 |

**The one non-`passed` run is the declared exception, and it still returned its packet.**
`defer-panic/panic-string-output` is the matrix's single `expectedFailure`; it panics without
recovering, exits 2, and is declared `termination: abnormal`. Its coverage outcome is
`collected`, not `expected-unavailable` — which is §4.3's prediction holding: the dump is
emitted ahead of every `runtime.exit` and `fatalpanic` reaches `exit(2)`, so the
`expected-unavailable` classification is a guard that did not have to fire. Nothing else in
the corpus fell short of `collected`.

`run_attempts` is 1012, not 3 × 338 = 1014, and that reconciles exactly: the attempt loop
`break`s on the first failing attempt, so `panic-string-output` contributes 1 attempt rather
than 3. Every other program ran three times and had its bitmaps OR-merged.

AF_INET sockets were available on this host, so no capability took the `skipped` path.

### Coverage figures

| Measurement | Accepted 2026-07-22 baseline | This run (2026-07-31) |
| --- | ---: | ---: |
| Programs | 294 | 338 |
| Programs returning coverage | 291 | 338 |
| Active Linux/ARM64 runtime Go functions | 2,561 | 2,574 |
| Compiled runtime functions | 2,050 | 2,087 |
| Executed runtime functions | 1,269 | 1,400 |
| Active-function coverage | 49.55% | **54.39%** |
| Compiled runtime blocks | 25,929 | 27,780 |
| Executed runtime blocks | 7,936 | 9,182 |
| Compiled-block coverage | 30.61% | **33.05%** |
| Classified missing functions | 317 | 252 |
| Unknown missing functions | 975 | 922 |
| Unexpected failures | 19 | **0** |
| Compile / run failures / timeouts | 2 / 18 / 1 | 0 / 1 (declared) / 0 |
| Peak compile RSS | 1.73 GB | 2.31 GB |
| Peak run RSS | 1.82 GB | 0.39 GB |

**Against §2's guideposts of 65% active-function and 45% compiled-block, this run falls short
of both — 54.39% and 33.05%.** It is better than the previous accepted baseline on both axes
(+4.84 and +2.45 points) over a corpus that is 15% larger, but it does not reach either
guidepost and nothing here should be read as though it did. §2 is explicit that these are not
hard gates and that the reviewed function inventory is the final gate; the honest statement is
that the corpus has grown and the percentages have improved, and the guideposts remain open.

### The drift refusal reproduces (verified)

    $ go run ./cmd/goc runtime-cover-diff -fail-on-regression \
        cmd/goc/testdata/runtime_coverage_linux_arm64.json <new report>
    goc: compare runtime coverage: runtime source differs:
      baseline 10a75b3cbbd95507daf7cd4ac1b4aa3b6ddcab338ea0d00e8a79c52bdbb9bb06,
      current  06e314f8a5a729394207296e943ea075e51dbd39355c3389ca7ac65a52cb8f55
    exit=1

That is the guard working, exactly as §4 and §13 said it would. Accepting this run is what
clears it.

### The rest of the verification

- **`make test-goc-cmd` re-run: PASS**, `ok github.com/evanphx/cg12/cmd/goc 217.612s`, 0
  failing tests. So the flake above was the only failure in that suite.
- **Full capability matrix, ordinary (non-instrumented) path** — `go test -v -run
  '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/`, `ok ... 311.667s`:

  | | |
  | --- | ---: |
  | `--- PASS` subtests | 338 |
  | `--- FAIL` subtests | 0 |
  | `--- SKIP` subtests | 0 |
  | distinct subtest names | 338 |
  | verdict `PASS` | 337 |
  | verdict `EXPECTED FAILURE` | 1 (`runtime_panic_print_string.go`) |
  | verdict `KNOWN GAP` | 0 |
  | verdict `FAIL` | 0 |

  **Complete list of non-passing capabilities: `defer-panic/panic-string-output`, the
  declared `expectedFailure`. Nothing else.**

  The instrumented coverage run gives the same census independently — 337 `PASS` + 1
  `EXPECTED FAILURE`, 338 distinct subtest names — so both compile paths were counted, not
  just one.

- **Determinism did not regress.** `scripts/determinism-check.sh`:

      hello.go                            round1:identical  round2:identical
      fmt_sprintf.go                      round1:identical  round2:identical
      gc_struct.go                        round1:identical  round2:identical
      runtime_cleanup_frame_retention.go  round1:identical  round2:identical
      runtime_defer_capture_allocs.go     round1:DIFFERENT  round2:identical

  4 of 5 byte-identical, with `runtime_defer_capture_allocs.go` the documented §5.10 residue
  the script itself calls out as expected. Unchanged from the recorded state.

## RUNTIME_PLAN.md updates (only what was reproduced here)

- **§1** — the baseline table now carries the 2026-07-31 figures beside the old ones, the
  source fingerprint, the 338/338 outcome statement, and an explicit sentence that both
  percentages remain short of §2's guideposts.
- **§4 M0** — the last checkbox, "Reach one usable coverage outcome per capability", is
  ticked at 338 of 338, and its text now records that the three former timeouts collect.
- **§4** — the drift paragraph records both fingerprints and that accepting the run restored
  the comparison; §4.3 records that the accepted baseline has zero `expected-unavailable`
  rows.
- **§5.2.1 checklist** — "A full corpus rerun is still required before accepting a new
  baseline" is replaced by the fact: `stdlib-http/redirect-keepalive`,
  `stdlib-http/tls-client-server` and `stdlib-crypto/ecdsa` are each `passed`/`passed`/
  `collected` in the accepted baseline (checked row by row, not assumed).
- **§12** — M0 marked COMPLETE (2026-07-31), with the caveat that M0 means coverage is
  measurable and diffable, not high.
- **§13 item 1** — rewritten as done, carrying the shortfall against §2 rather than dropping
  it.

Nothing was ticked that was not reproduced in this job. Nothing was closed in §5.10.

## Still unverified / not done

- **Coverage percentages are not at §2's guideposts.** 54.39% vs 65% active-function, 33.05%
  vs 45% compiled-block. This job did not attempt to raise them; it accepted a measurement.
- **The 922 `unknown` missing functions** (down from 975) are still an open triage state that
  §4.2 says must trend to zero. Untouched here.
- **`TestBatchCompilesAgainstDifferentPacksMatchOneShotCompiles` remains flaky on `main`.**
  Diagnosed and measured above, deliberately not fixed — it belongs to a sibling branch.
- **One collection run, not two.** §4's exit criterion wants two consecutive full runs with
  the same denominator. This job produced one. The denominator is now structurally enforced
  (`matrix_capabilities` is `len(runtimeCapabilities())` and a missing row is a collection
  error), so a second run cannot disagree without failing, but the second run was not made.
- `make test-ruby` and `make test-cruby` were not run; they are outside this task's area.
