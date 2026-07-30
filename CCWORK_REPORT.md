# Splitting the goc driver: compile the Go runtime once, not once per program

Branch: `ccwork/driver-split`, based on `perf/test-suite` (`b3720bf`, which is also
`origin/ccwork/permodule-impl`).

Status: **in progress — written as it lands.** Anything not verified is stated as such.

## The design decision, up front

### The premise the briefing carried forward has changed

The briefing (from the `sepcompile` spike) says the prebuilt runtime object must ship its
per-function metadata (`[]gometa.FunctionInfo`) alongside the ELF, "because the program side
has to be able to chain modules". That was true of the spike's design, which assumed **one
merged pclntab per image** (spike Obstacle 2): the linker would have to regenerate the whole
blob from the union of both sides' functions, so it needed the runtime's per-function facts.

The mechanism that actually landed (`RUNTIME_PLAN.md` §14) is the *other* design the spike
named: **per-module moduledata**. Each object carries its own complete pclntab, its own type
region and its own text bounds, and joining them is one `R_AARCH64_ABS64` write into
`moduledata.next`. So the program side never needs the runtime's `FunctionInfo` — the runtime
object already describes itself.

What the program side *does* need is the **set of symbols the prebuilt object defines**, so it
can compile only the difference and reference the rest. That is the sidecar this step ships.

### Container format

`goc build-runtime -o <file>` writes one file holding three members: the runtime module's ELF
relocatable, the assembled Plan 9 sidecar ELF, and a JSON manifest. Justification for a
purpose-built container over the alternatives:

- **`ar` archive** — standard and `cc`-consumable, but `cc` pulls archive members only to
  resolve an undefined symbol, so a member the image needs but nothing references yet is
  silently dropped; and it has nowhere to put the manifest.
- **A non-alloc ELF section inside a single merged object** — elegant, but merging the Go
  object and the assembled sidecar into one `ET_REL` is a code path nothing else in the tree
  uses, on the critical path of every build.
- **A tiny purpose-built container** (chosen) — magic + version + JSON index + concatenated
  members. One artifact, so the manifest cannot drift from the objects it describes; a version
  stamp so a stale pack is refused rather than mislinked; ~60 lines with a unit test.

(Full design and measurements below, filled in as they land.)

## Baseline measurement (monolithic, before any change)

Full 338-capability matrix, one unsharded `go test` process with
`-runtime-status-compile-workers=10` (this job's declared CPU share), on the 64-core box:

```
ok  github.com/evanphx/cg12/cmd/goc  478.700s      (wall 7:59.53, peak RSS 2.87 GB, rc=0)
```

**479 s** is the number the split has to beat.
