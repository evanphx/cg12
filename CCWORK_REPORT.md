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

## What was built

### `goc build-runtime -o runtime.gocrt`

Compiles a fixed root program (`package main; func main() {}`, in
`goc/runtime_root.go`) with the ordinary executable pipeline, then keeps everything
except the parts that are program-built by construction. The result is one pack file:
the module's ELF relocatable, its assembled Plan 9 sidecar, and the manifest.

The runtime module's `moduledata.next` is written before the program it will be chained
to exists, so it *names* `goc.programmoduledata` and the system linker resolves it
(`gometa.ChainModuleToExternal`).

### `goc -runtime runtime.gocrt prog.go`

Compiles the program with the same whole-program front end, then drops every definition
the pack already has and links `runtime.o sidecar.o prog.o [prog-sidecar.o]` with `cc`.

**The split is applied to the finished module**, after IR generation and every
module-level pass. So a symbol the program module keeps is bit-for-bit what a monolithic
build would have emitted. That is the design's whole safety argument, and it is what
makes a differential comparison meaningful (`goc.TestKeptSymbolsMatchAMonolithicBuild`
asserts it at IR level for every kept datum).

### Three things could not be subtracted

1. **The interface-method dispatchers** (`error_Error`, `runtime_stringer_String`). As the
   briefing predicted, this is the highest-risk boundary. They switch over the concrete
   types the *program* contains and their fall-through is
   `runtime_gocInterfaceDispatchFailure` — a fatal throw, not a `getitab` fallback. The
   pack leaves them undefined and lists them in `manifest.ProgramSymbols`; the program
   module defines and exports them; `prebuilt.CompileProgram` refuses to proceed if the
   program does not define one, so the failure is a build error naming the symbol rather
   than a dispatcher that silently misses an itab.

2. **The whole Go type region** — and this inverts task item 3. The briefing asks to
   *export the runtime's* type and name symbols so the program can point at them. That is
   backwards, and the differential run is what showed it. A type descriptor's contents are
   program-dependent: `clearUnavailableRuntimeMethodOffsets` writes
   `runtime.unreachableMethod` into a method entry whose function is not in the image, and
   `populateRuntimePointerTypes` fills `PtrToThis` only when the pointer type is also
   described. The prebuilt module reaches fewer methods than a program that imports more,
   so **its descriptor is not merely different, it is strictly poorer** — freezing it in
   would silently disable reflect method calls. And two copies is not an option: cg12
   compares descriptors by pointer (the inline itab match in `interfaceTypeWord`, every
   candidate test in a dispatcher), so a value tagged with one module's descriptor would
   not match the other's and the dispatcher would throw.

   So the program module owns the type region: every datum holding a module-relative
   offset and everything such an offset addresses. One descriptor per type, in the module
   that knows the most about it. A useful consequence: **the image has no duplicate
   descriptors at all**, so `typelinksinit` has nothing to canonicalise.

3. **Package assembly the prebuilt module never loaded.** A program reaching
   `reflect.methodValueCall` or a crypto block function needs that package's Plan 9
   assembly; the pack records which files it assembled and the program translates the rest
   into a sidecar of its own.

### Bugs found and fixed on the way

- **`runtime.lastmoduledatap` was hardcoded to `&runtime.firstmoduledata`.** `runtime.main`
  runs each module's init tasks by walking the chain and stopping at the tail, so with two
  modules the program's own package init never ran. `hello.go` failed with
  `panic: init did not run` — a failure that looks like a miscompiled program, not a
  linking mistake.
- **Two symbol families were still named by a running counter**
  (`%s.interfacecall.%d`, `%s.interfacecall.promoted.%d`). An itab's method entries name
  those wrappers, so the same itab had different contents in two compilations of the same
  program. Now content-named, like every other family.
- **goc emits a few package globals twice** (`runtime.divideError`,
  `runtime.overflowError`, `internal/runtime/maps.errNilAssign`): a zeroed placeholder and
  then the record holding the itab. `obj.prepareELF`'s symbol index keeps the last, so the
  placeholder was dead bytes nothing referenced — invisible while the symbols were local,
  `multiple definition of runtime_divideError` from `ld` once they went global. The split
  drops the shadowed copy; the duplicate emission itself is recorded below, not fixed.
- **`findfunctab` was a flat 2.6 MB in every module.** The bucket count falls back to a
  512 MB-covering floor when the module's text span is unknown, which it always was
  because the sidecar carries the module's end — and the floor then beat the real count
  unconditionally. A module bounded entirely by its own object knows its span, so it now
  sizes the table to it. Without this the split added 2.6 MB to every image for a table
  cg12 never populates.

## Where the time goes, and the ceiling this design has

Per-program compile, warm process (the shape the matrix compiles in), `hello.go`:

| phase | monolithic | split |
| --- | ---: | ---: |
| reachability + IR generation + module passes | ~1.4 s | ~1.4 s |
| per-function back end (lower, regalloc, emit) + metadata blob | ~2.6 s | ~0.05 s |
| `cc` link | 0.11 s | 0.08 s |
| **total** | **~4.0 s** | **~1.5 s** |

**The split removes the back end, not the front end.** The sepcompile spike projected
89% (reachability + IR generation + back end); this design gets ~60%, and the reason is
the same fact that makes it correct. The program module has to own the type region,
because descriptor contents depend on what the program reaches — and cg12 discovers
which descriptors a program needs *by generating its IR*. `ensureTypeTag` is called
from the lowering of a conversion, an interface assignment, a `new`. So the program
module cannot skip generating IR for functions the prebuilt object already has: doing
so would silently drop the type descriptors those functions need, and a missing
descriptor is not a link error — it is a dispatcher that quietly stops matching.

Getting past this needs the prebuilt pack to carry enough about its functions' type
requirements to reconstruct them without lowering, which is a redesign, not a tuning
knob. It is written down here rather than attempted.

## Verification status (updated as each result lands)

### Landed

- `internal/runtimepack`, `internal/gometa`, `goc` and `cmd/goc` unit/e2e tests for the
  split all pass. The `cmd/goc` ones link a real two-module image and read the chain,
  `hasmain` and the typelinks counts back out of it.
- 30-program differential (compile both ways, run both, compare exit status **and full
  output**): 28 identical, 2 differing only in a printed allocation count (both exit 0).

### Outstanding at the time of writing

- The full 358-program corpus differential (running).
- The full 338-capability matrix built the new way, and its wall clock against the 479 s
  baseline.
- `make test-unit`, `make test-goc-corpus`, `make test-goc-cmd`.
- Determinism (`CG12_NOCACHE=1` vs warm) before and after.
- Startup cost of a two-module image.

### `make test-unit` — **pass** (rc=0)

24 packages, no `FAIL`. Includes the new `internal/runtimepack`, the `internal/gometa`
findfunctab and `ChainModuleToExternal` tests, and `arm64`.
