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
