# Batch compilation, reconciled with multi-pack selection

Branch: `ccwork/batch-reconcile`, off `main` (`a639ec9`). The previous job's report has been
moved to `docs/report-pack-stdlib.md`, following the precedent those jobs set, so this file is
only about this job.

**Status: in progress.** This file is written as the work lands, not at the end. Anything not
yet verified is listed under "Still unverified" at the bottom and moved out of it only when a
command has actually been run and its output read.

## Headline so far

The two designs reconcile without giving anything up, and **the lever is much larger on top of
the standard-library packs than it was measured to be beneath them**: the full matrix goes from
**351.8 s to 273.6 s (-22.2%)** and from **2758.7 s to 1930.5 s of CPU (-30.0%)**, against a
matched control on this same box, every run 338 subtests / 338 pass / 337 declared PASS /
1 EXPECTED FAILURE / 0 KNOWN GAP / 0 FAIL.

`ccwork/goc-batch-b` measured the same lever at 5-12% of the matrix. It is bigger now for a
reason that is a consequence of §19 rather than of anything this job did: the packs cut the
*variable* cost of a compile by more than half, and the batch removes *fixed* cost, so the same
number of seconds is now a much larger fraction. §19's own open item -- every program parsing
seven manifests to choose between them, "74 s of CPU over the matrix" -- is also amortized by a
worker, for free.

## The collision, in one paragraph

`ccwork/goc-batch-b` hoists `runtimepack.Read(packPath)` out of the per-program loop so a batch
of programs shares one pack read. Multi-pack selection (§19, now on `main`) cannot allow that
hoist: `-runtime` is a comma-separated set, and which pack a program gets is decided by the
program's own import closure, which is not known until the front end has run. So `main` reads
only the manifests up front and reads the chosen pack's objects afterwards.

## The reconciliation, as implemented

A **`packSet`** (`cmd/goc/prebuilt.go`): every pack's manifest read once when the set is built,
and each full pack read lazily the first time some program selects it and retained afterwards.
One-shot `goc` and `goc compile-batch` both go through it, so they are the same code path with
a different lifetime.

- Selection is unchanged. `prebuilt.CompileProgram` still receives every manifest and still
  returns the one it chose; nothing about `Manifest.UsableBy` or the fallback to the
  runtime-only pack is touched.
- A worker that compiles ten `net/http` programs reads the `net/http` pack once.
- A worker that compiles programs choosing different packs reads each of those once.
- A one-shot `goc` does exactly what it did before: read the manifests, compile, read one pack.

Two deviations from the briefing's sketch, both small:

- **The pack a compile chose is matched back to its file by pointer identity, and a mismatch is
  an error rather than a fallback.** `main` had `chosen := packPaths[0]` as the default if the
  returned manifest matched none of the offered ones. That cannot happen today --
  `CompileProgram` returns `manifests[program.Chosen]` -- but if it ever did, the build would
  silently link one pack's objects against another pack's subtraction. It now says so instead.
- **`compile-batch` keeps `-runtime` as the same comma-separated set the one-shot flag takes**,
  rather than growing a new spelling. A worker is offered the whole set and each of its programs
  still takes the richest its own closure allows.

## What it measures

Box: linux/arm64, 64 cores, ~240 GB RAM, load average 2.9 at the start of the sequence, no
sibling job visibly running. Every run is a full unsharded matrix through
`scripts/matrix-timing.sh`, `-count=1 -v`, at **4 compile workers** -- this job's declared CPU
share (`CCWORK_CPU_SLOTS=4`) -- with the pack cache warm in every measured run. `main` was
measured by checking `main` out **in this same working directory**, so both compilers were
built from the same absolute path (§6 of the batch report: a `git worktree` at a different path
is not a valid reference build).

| run | wall | CPU (user+sys) | sum of per-compile wall | slowest single compile | max RSS |
| --- | ---: | ---: | ---: | ---: | ---: |
| `main` (a639ec9), warm packs | 351.8 s | 2758.7 s | 1365.5 s | 23.1 s | 2622 MB |
| this branch, `-runtime-status-batch-compile=false` | 351.3 s | 2755.6 s | 1363.9 s | 23.0 s | 2633 MB |
| this branch, batch on | **273.6 s** | **1930.5 s** | 1050.8 s | 22.7 s | 2624 MB |
| this branch, batch on (repeat) | **273.2 s** | **1926.8 s** | 1049.8 s | 23.0 s | 2725 MB |

Against `main`: **wall -78.2 s (-22.2%), CPU -828.2 s (-30.0%)**. The one-flag-apart control on
this tree lands within 0.15% of `main` on all three columns, which is what says the flag is the
only difference. The two batch runs land within 0.4 s of each other.

Five full matrix runs in this sequence. Every one:

    subtests=338 pass=338 fail=0 skip=0 declaredPASS=337 expectedFAILURE=1 knownGAP=0

### Which term bounds the matrix afterwards

    wall ~ max( slowest compile , compile CPU / workers ) + run phase + setup

At **4 workers** the second term binds and binds by an order of magnitude: 1050.8 / 4 = 262.7 s
against a slowest single compile of 23.0 s. Adding the 14.9 s run phase gives 277.6 s against
273.2 s observed, so the model accounts for the whole run. That is why the wall clock moved 22%
here.

At the **default 64 workers** the terms swap. 1050.8 / 64 = 16.4 s is below the 23.0 s slowest
compile, so the first term binds and it is the same 23.0 s compile in both arms: the predicted
wall clock is ~38 s either way and this lever would buy close to nothing. That is the honest
expectation the briefing asked for, and it is unchanged from what §17 and the batch report both
said -- the floor is one single-threaded compile of `stdlib_http_tls_client_server.go`, and
nothing here touches it.

**So the value is the 30% of CPU, not the wall clock.** The suite costs a third less to run,
and the cost is much less sensitive to how much of the machine it gets -- which is exactly the
situation this job ran in.

### Why the lever grew

`ccwork/goc-batch-b` measured 5-12% against a runtime-only pack, where the matrix spent about
4400 s of CPU. On top of §19's pack set the matrix spends 2759 s, because the packs removed
compile work, not process work. The per-process cost the batch removes -- building the source
world, starting the process, collecting a fresh ~300 MB heap, and now also parsing seven
manifests -- is roughly the same number of seconds as before, against a smaller total. The two
levers multiply rather than overlap.

### Memory

Peak RSS over the whole run is **unchanged**: 2622 MB on `main`, 2624 MB and 2725 MB batched.
A worker's peak is still the largest program it compiles, and retaining packs it has already
read does not move the maximum. `compileRuntimeCapabilityPeakBytes = 3 GiB` and the divisor
built on it stand.

## The compiler is bit-identical to `main` by construction

`git diff --name-only main...HEAD` over `goc/ ir/ opt/ arm64/ amd64/ link/ obj/ lower/ parse/
internal/` is **empty**. The ten files this branch changes are all under `cmd/goc/` and
`analysis/batchdiff/`, plus this report and `RUNTIME_PLAN.md`. No code that decides what a
compile emits is touched, so no compilation this branch performs can differ from one `main`
would have performed. What can differ is *when* things are read and *what a process carries
between programs*, which is exactly what the corpus-wide leak check below is for.

## A defect found while reconciling

The batch pool returned a worker to the free list **before** reading the stderr that worker
had accumulated, so the next program could start writing to the same buffer while this one was
still being attributed. It only ever affected diagnostics text on a failing compile -- a
worker writes a program's own errors into that program's response, never to its stderr -- but
"a linker complaint attributed to whichever program won the race" is the kind of thing that
would cost an hour to understand once. Fixed in `149d402`: the diagnostics are taken first.

## Progress log

- Read `RUNTIME_PLAN.md` §1/§3/§5.10/§14/§17/§18/§19 and the `goc-batch-b` report; confirmed
  the collision is exactly `cmd/goc/prebuilt.go`'s `linkAgainstPrebuiltRuntime` and
  `cmd/goc/batch.go`'s hoisted `runtimepack.Read`.
- Implemented `packSet`; `go build`, `go vet`, `gofmt` clean. Committed as `b0decae`.
- New tests pass (`TestAPackSetReadsOnlyTheChosenPackAndReadsItOnce`,
  `TestBatchCompilesAgainstDifferentPacksMatchOneShotCompiles`), as do the three the batch
  branch wrote and the five pack tests §19 wrote.
- Matrix A/B complete: the table above.

## Still unverified

Running now, in this order, each chained behind the last so nothing competes for the box:

- The corpus-wide leak check (`analysis/batchdiff`, all 358 programs, three ways, against the
  seven-pack set, 4 workers). **This is the safety property.**
- `scripts/determinism-check.sh -runtime <the seven packs>`.
- `make test-unit`, `make test-goc-cmd`, `make test-goc-corpus`.

Not yet started:

- The monolithic batch path (`-runtime-status-prebuilt-runtime=false`, where a worker compiles
  the runtime into each program instead of linking a pack). `main`'s coverage run uses the
  one-shot path by construction, so this is the only caller of that branch.
- `make test-goc-coverage`. It does not use batch mode -- `newRuntimeCapabilityBatchPoolFor`
  returns nil whenever `-runtime-coverprofile` is set -- so that path is byte-for-byte what was
  there before, but the suite itself has not been run here either.

One note on the briefing: it points at `RUNTIME_PLAN.md` §5.14 for a live interaction bug.
**There is no §5.14 in `main`'s plan** -- §5.10 is the last subsection of §5, and the string
"5.14" does not occur in the file. Nothing was closed there because there was nothing to close.
