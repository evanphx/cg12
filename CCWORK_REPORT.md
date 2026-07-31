# Batch compilation, reconciled with multi-pack selection

Branch: `ccwork/batch-reconcile`, off `main` (`a639ec9`). The previous job's report has been
moved to `docs/report-pack-stdlib.md`, following the precedent those jobs set, so this file is
only about this job.

**Status: in progress.** This file is written as the work lands, not at the end. Anything not
yet verified is listed under "Still unverified" at the bottom and moved out of it only when a
command has actually been run and its output read.

## The collision, in one paragraph

`ccwork/goc-batch-b` hoists `runtimepack.Read(packPath)` out of the per-program loop so a batch
of programs shares one pack read. Multi-pack selection (§19, now on `main`) cannot allow that
hoist: `-runtime` is a comma-separated set, and which pack a program gets is decided by the
program's own import closure, which is not known until the front end has run. So `main` reads
only the manifests up front and reads the chosen pack's objects afterwards.

## The reconciliation, as implemented

A **`packSet`**: every pack's manifest read once at startup, and each full pack read lazily the
first time some program selects it and then retained. One-shot `goc` and `goc compile-batch`
both go through it, so they are the same code path with a different lifetime.

- Selection is unchanged: `prebuilt.CompileProgram` still receives every manifest and still
  returns the one it chose, and the fallback to the runtime-only pack is untouched.
- A batch worker that compiles ten `net/http` programs reads the `net/http` pack once.
- A batch worker that compiles programs choosing different packs reads each of those once.
- A one-shot `goc` behaves exactly as it did: it reads the manifests, compiles, reads one pack.

Deviation from the briefing's sketch: none in shape. Details and their costs are in the
sections below.

## Progress log

- Read `RUNTIME_PLAN.md` §1/§3/§5.10/§14/§17/§18/§19 and the `goc-batch-b` report; confirmed
  the collision is exactly `cmd/goc/prebuilt.go`'s `linkAgainstPrebuiltRuntime` and
  `cmd/goc/batch.go`'s hoisted `runtimepack.Read`.
- Implementation in progress.

## Still unverified

Everything. Nothing in this report has been measured yet.
