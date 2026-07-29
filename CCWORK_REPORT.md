# Per-module type regions: making a goc image carry more than one Go module

Branch: `ccwork/permodule-impl`, based on `perf/test-suite` (`8b3b5ca`).
Status: **in progress — findings are written here as they land.**

## What this is

Landing the mechanism the two prior spikes identified (`ccwork/typeoff-alternatives`,
`ccwork/sepcompile-spike`): give each separately compiled object its own `moduledata`, so
its `NameOff`/`TypeOff` values stay relative to its own type region. Scope is items 1, 2, 3,
5 and 6 of the task list; the driver split (`goc build-runtime`) is explicitly *not* in
scope.

Sections below are filled in as each piece is verified. Anything not verified is stated as
such.

## Verification status

(filled in as it lands)

## Determinism baseline, before any change (reproduced, not assumed)

`CG12_NOCACHE=1` build vs. warm build, sha256 of the linked image:

| program | result |
| --- | --- |
| `hello.go` | identical |
| `fmt_sprintf.go` | identical |
| `gc_struct.go` | identical |
| `runtime_cleanup_frame_retention.go` | identical |
| `runtime_defer_capture_allocs.go` | **different** (the documented backend residue) |

4 of 5, exactly as RUNTIME_PLAN records. This is the number the post-change run has to match.

## moduledata field offsets, verified against the host toolchain

Not counted by eye and not taken from the spike: the vendored `runtime.moduledata`
(`stdlib/src/runtime/symtab.go:402`) was transcribed into a standalone program and compiled
with the host Go 1.26.1, printing `unsafe.Offsetof`:

```
sizeof=592  types=296  etypes=304  typelinks=360  inittasks=472
modulename=496  modulehashes=512  hasmain=536  bad=537
gcdatamask=544  gcbssmask=560  typemap=576  next=584
```

## The mechanism, running on a real goc image (2026-07-29)

`analysis/typeoff` (the spike's prototype, re-pointed at the landed mechanism) builds a
two-module image: a real goc-compiled program plus a separately compiled object built by the
new `internal/permodule`. The second module needs **no hand-added symbols** — the spike's
`runtime.gocTextEnd` workaround is gone.

`go run ./analysis/typeoff -o out cmd/goc/testdata/permodule_probe.go`:

```
typeoff: merged .data is 6772736 bytes; program base at 0,
         second module's base at 4149736 (shifted 4149736 bytes from where it was compiled)
foreign-int:int
foreign-int-kind:2
foreign-ptr:*int
ptr-identity: same          <- typelinks/typemap: one Go type, one identity, across modules
first-func:_goc_probe_entry <- the module's function at text offset 0 now has a name
first-call:7                <- its code ran
frames:6
frame:main_probeCallback
frame:_goc_probe_hold       <- the traceback walked a second-module frame
frame:main_main
...
payload: intact
probe: done
```

### The GC stack scan over the second module's frame

The spike explicitly did not verify this. `GODEBUG=cg12scanroots=2`, `GOMAXPROCS=1`:

```
cg12scanroots: frame _goc_probe_hold sp=0x30ad252f8e0 fp=0x30ad252f900
               varp=0x30ad252f900 argp=0x30ad252f900 locals=2 args=1
cg12scanroots: _goc_probe_hold local slot 0 at 0x30ad252f8f0
               retains 0x30ad24dc0e0 size 32 head 0x5ea1ed
```

`0x5ea1ed` is the program's `payloadMagic`. Filtering the whole scan for that object:

```
      2 cg12scanroots: _goc_probe_hold local slot 0 ... retains ... head 0x5ea1ed
```

**Exactly one frame retains it, and it is the second module's.** Both GC cycles. So the
object's survival is not incidental: it is held by the second module's locals stack map,
read out of the second module's `gofunc` region, for a frame located by the second module's
pcsp table and `_func` record. That is the pcsp and stack-map halves of a second module's
pclntab, exercised.

## Determinism, after the change

Same script, four runs. The other four programs produce the *same* hash on every run, which
is a stronger check than cold-vs-warm.

| program | result |
| --- | --- |
| `hello.go` | identical, and the same hash across all 4 runs |
| `fmt_sprintf.go` | identical, and the same hash across all 4 runs |
| `gc_struct.go` | identical, and the same hash across all 4 runs |
| `runtime_cleanup_frame_retention.go` | identical, and the same hash across all 4 runs |
| `runtime_defer_capture_allocs.go` | **different** in 3 of 4 runs; its hash also varies run to run |

Unchanged: 4 of 5, and the one exception is the documented backend residue. (It matched by
chance on the first post-change run; repeating three more times showed it varying freely,
so the honest reading is "still non-deterministic", not "fixed".)

## What landed

All of items 1, 2, 3, 5 and 6. Item 4 is **not** done and is explained below.

### 1. The text-end symbol is per module

`internal/gometa`'s `textEndSymbol = "runtime_gocTextEnd"` was one constant for the whole
image. It bounds `moduledata.maxpc`/`etext` and is the `functab` sentinel, and the Plan 9
sidecar defines it once, so a second module took the *first* module's text end as its own
`maxpc` and `runtime.findmoduledatap` could never select it.

`gometa.TextEndSymbol(dataSymbol)` now derives the name from the module's moduledata name;
the runtime's own module keeps `runtime_gocTextEnd`, so nothing about a single-module image
changes. Who *defines* it follows the same rule as who emits the module's last text: the
Plan 9 sidecar when the sidecar carries functions, otherwise the object itself
(`arm64.finishGoModule`). The spike's prototype had to hand-add a local
`runtime.gocTextEnd` to its second object; it no longer does.

### 2. `moduledata.typelinks` is populated

This was the one genuinely new piece. `builder.emptySlice() // typelinks` is now a real
`[]int32` of module-relative offsets, built from the descriptors the frontend marks with the
new `ir.Data.GoTypeLink`.

Two decisions worth checking rather than trusting:

- **Which descriptors qualify.** Only the *complete* ones — `goc`'s `ensureTypeTag` output
  when `emitRuntimeTables` is on. `runtime.typesEqual` reads a type's kind-specific tail
  (`StructType.Fields`, `FuncType`'s in/out slices, `PtrType.Elem`), so listing goc's other,
  bare 48-byte descriptor family (`runtimeType`, `.goc.runtime.type:*`, which carries a kind
  byte and no tail and whose `Str` is 0) would send `typesEqual` past the end of the symbol.
- **All complete descriptors, not only named ones.** The task described it as "one per named
  type", but the duplicates a module split actually creates are *unnamed*: the spike's own
  §3.2 says a program module re-emits a descriptor for any **signature** or **pointer** type
  it shares with the runtime. Upstream does the same thing under `-buildmode=shared`
  (`Flag_dynlink` forces every type into typelinks) for exactly this reason. `hello.go`
  yields 499 entries — under 2 KB.

`typelinksinit` returns immediately when `firstmoduledata.next == nil`, so a single-module
image pays the table's bytes and no startup time at all.

### 3. The moduledata symbol name is a parameter

`ir.Module.GoModuleData` carries it, `gometa.Module.DataSymbol` consumes it, and
`builder.label`'s literal `if name != ir.LinkerSymbol("runtime.firstmoduledata")` special
case is gone (replaced by a `labelOnly` for the one label whose symbol the caller emits
itself — under the old code any *other* moduledata name was defined twice).

A Go module that defines `runtime.firstmoduledata` without declaring it is now an error.
The alternative — silently emitting no metadata for it — produces an object that links and
then cannot start, which is the failure mode a per-module name must not introduce.

### 5. `hasmain`

`moduledata.hasmain` (offset 536) is set from `ir.Module.GoHasMain`, which `goc` sets when
compiling an executable. gometa used to zero the record's whole tail.

**Honest limit:** in the topology cg12 can currently produce, `hasmain` has no *observable*
effect. `runtime.modulesinit` uses it to move the main module to `modules[0]`, and
`firstmoduledata` — which is the module with `main` — is already `modules[0]`. So this piece
has a byte-level test and an integration assertion (1 on the program module, 0 on the second
module), not a behavioural one. It becomes load-bearing when the runtime is the first module
and the program is the second, which is what the driver split will produce.

### 6. Chaining

`gometa.ChainModule(object, parent, child, reloc)` records the one `R_AARCH64_ABS64` data
relocation into `moduledata.next` (offset 584, `gometa.ModuleNextOffset`). That is the whole
of joining a module to the image.

### Also fixed: the first function of every goc module was nameless

RUNTIME_PLAN §5.10 records this on the spike branch (it was never carried to
`perf/test-suite`, so there was no entry to remove here). `internal/gometa` laid the first
name at offset 0 of `.goc.go.funcnames`, and `runtime.moduledata.funcName` reads a name
offset of 0 as the empty string. One reserved sentinel byte fixes it; upstream's linker
reserves the same slot. It has its own unit test, and the two-module image demonstrates it
on real output: `first-func:_goc_probe_entry`, where the spike printed `foreign-func:` with
an empty name.

### Item 4 (exporting type and name symbols): NOT done

It does not fall out naturally and I left it. It only matters once the driver is split — its
purpose is to let a program object point at a *prebuilt runtime's* `type:int` by pointer
instead of duplicating it — and doing it now would make ~500–3000 previously-local symbols
per image global, with no consumer to justify the collision surface. It is recorded in
RUNTIME_PLAN §14 under "what is not done".

## Where things live

| path | what changed |
| --- | --- |
| `internal/gometa/builder.go` | `Module` (the per-module input), `TextEndSymbol`, typelinks table, `hasmain`, the funcname sentinel, `labelOnly` |
| `internal/gometa/gometa.go` | `ChainModule` |
| `internal/gometa/module_test.go` | new: one test per piece, plus the two refusal paths |
| `arm64/go_metadata_object.go` | builds the `gometa.Module`, collects type-descriptor offsets, defines the module's text end when the sidecar has no text, refuses an undeclared moduledata |
| `arm64/assembly.go` | the sidecar emits a per-module text-end label, and only when it carries functions |
| `ir/func.go`, `ir/binary.go` | `Module.GoModuleData`, `Module.GoHasMain`, `Data.GoTypeLink`; unit format bumped to v19 and carrying all three |
| `goc/compile.go` | names the moduledata, sets `GoHasMain`, marks complete type descriptors |
| `internal/permodule/` | new: builds a second Go module, and links a two-module image |
| `cmd/goc/permodule_test.go`, `cmd/goc/testdata/permodule_probe.go` | new: the end-to-end test and its program |
| `analysis/typeoff/` | the spike's prototype, re-pointed at the landed mechanism (its `probe.go` moved into `internal/permodule` so the tool and the test share one implementation) |
| `analysis/sepcompile/`, `analysis/seplink/`, `analysis/testdata/nistec_closure_name_collision.go`, `analysis/testdata/typeoff_probe.go` | cherry-picked verbatim from the spike branches |
| `RUNTIME_PLAN.md` | new §14; old §14 renumbered to §15; §13 gains the driver split as the next batch item |
