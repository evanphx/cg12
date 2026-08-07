# Caching compiled work per package in goc

Branch `ccwork/build-cache-design-2`, cut from `main` (`76069d9`).
Deliverable: this document plus `cmd/stagetime`, the harness the numbers come
from. **No caching behaviour was changed.**

Host: aarch64 Linux, 64 cores, 250 GiB RAM, go1.26.1 at
`/home/evan/.local/go1.26.1`. All goc figures are `goc -O` whole-program arm64
builds at the default `GOMAXPROCS=64`.

Everything in Part 1 is read out of the host toolchain's source at that GOROOT,
or probed with `go build` on this box; the two are labelled separately.
Everything in Part 2 is measured on this box. Part 3 says which of its claims
rest on which.

---

## Part 1 — What gc caches, and why it stays sound under cross-package inlining

### 1.1 The artifact is serialized IR, not source and not an AST

The format is Unified IR, defined by `cmd/compile/internal/noder` and
documented in `noder/doc.go` (primitives in `internal/pkgbits/doc.go`). A
package's export data file is:

```
File        = Header Payload fingerprint .
Header      = version [ flags ] sectionEnds elementEnds .
Payload     = SectionString SectionMeta SectionPosBase SectionPkg SectionName
              SectionType SectionObj SectionObjExt SectionObjDict SectionBody .
fingerprint = [8]byte .    // sha256
```

`SectionBody` is the point. Function bodies live there as encoded IR elements,
written by `(*pkgWriter).bodyIdx` (`noder/writer.go:1169`), which walks the
type-checked syntax once and emits statements into a body element. Reading one
back is `reader.go:3462`, which materialises `ir` nodes directly.

So when gc inlines `fmt.Sprintf` into your `main`, it does not re-parse or
re-typecheck `fmt`. It reads `fmt`'s cached body element out of the `.a` file
and splices the nodes in. There is no AST in the picture on either side of the
package boundary.

`SectionPosBase` carries positions independently of any file being present:
each base is an absolute filename plus, for line directives, a line/column, and
a `Pos` is a `(base, line, column)` triple. That is how a body read out of a
cached artifact still reports source positions.

### 1.2 Which bodies are carried, and how eligibility is decided

Two different passes decide this, and conflating them is the easy mistake.

**The writer is unconditional.** `funcExt` (`writer.go:1056`) calls `bodyIdx`
for *every* function it writes and emits `w.Reloc(pkgbits.SectionBody, body)`
with no eligibility test. That is the compiler's own stub export data, used
within the compilation unit.

**The linker prunes.** After the package has been compiled, `writeUnifiedExport`
(`noder/unified.go:465`) builds the finished, self-contained file, and
`linker.exportBody` (`linker.go:219`) decides what goes in:

```go
fn := obj.Func
if fn.Inl == nil {
    return // not inlinable anyway
}
exportBody := local || fn.Inl.HaveDcl
if !exportBody {
    return
}
```

- `fn.Inl == nil` means `inline.CanInline` rejected the function, so its body is
  dropped from the artifact entirely. `CanInline` (`inline/inl.go:236`) is a cost
  walk against `inlineMaxBudget = 80`, with hard exclusions recorded as a
  `reason` string: `marked go:noinline`, `no function body`, `call to recover`,
  `not inlining functions with closures`, `marked go:cgo_unsafe_args`, and the
  rest at `inl.go:333-671`.
- `local` means the function was declared in this package.
- `fn.Inl.HaveDcl` means this compilation actually inlined that (imported)
  function somewhere, so the body is re-exported and travels onward. The comment
  is explicit that this is a heuristic — "in the worst case, adding a blank
  import ensures the function body is available for inlining".

Alongside each body the linker rewrites the object's extension data
(`relocFuncExt`, `linker.go:267`) to carry the results of compilation: the
definition ABI, per-parameter escape-analysis notes (`f.Note`), and the
inline cost and `CanDelayResults` flag. So the artifact is not just IR — it is
IR plus the summaries a consumer would otherwise have to recompute.

The fingerprint is the sha256 of the whole encoded payload, produced by
`(*PkgEncoder).DumpTo` (`internal/pkgbits/encoder.go:57`) as it writes, and
stored into `base.Ctxt.Fingerprint`.

### 1.3 The key: `cmd/go/internal/cache` and `buildActionID`

`cmd/go/internal/cache` is a content-addressed store with a two-level split
(`cache.go:28-72`):

- an **ActionID** is "the hash of a complete description of a computation";
- an **OutputID** is "the hash of an output of a computation".

On disk (`fileName`, `cache.go:117`) an entry is `<dir>/<first byte>/<hex id>-a`
— the index record mapping ActionID to (OutputID, size, time) — and the output
itself is `<dir>/<first byte>/<hex outputid>-d`. Two different actions that
happen to produce identical bytes therefore land on the same `-d` file and share
it; `Put` hashes the content to derive the OutputID (`cache.go:543`), and
`GetBytes` re-verifies `sha256.Sum256(data) == entry.OutputID` before returning
(`cache.go:290`).

The ActionID for compiling a package is `buildActionID`
(`cmd/go/internal/work/exec.go:261`). It folds in, in order: the literal string
`"compile"`; the package directory (or, under `-trimpath`, the module path and
version); `goos`/`goarch`; the import path; `omitdebug`/`standard`/`local`/
prefix; the cgo, C, C++ and Fortran toolchain IDs and flags when cgo is in play;
the coverage and fuzz-instrumentation settings; and then, for the gc toolchain:

```go
fmt.Fprintf(h, "compile %s %q %q\n", b.toolID("compile"), forcedGcflags, p.Internal.Gcflags)
key, val, _ := cfg.GetArchEnv()          // GOARM, GOMIPS, ...
fmt.Fprintf(h, "%s=%s\n", key, val)
if cfg.CleanGOEXPERIMENT != "" { fmt.Fprintf(h, "GOEXPERIMENT=%q\n", ...) }
```

`b.toolID("compile")` is the compiler binary's own build ID, read from
`-V=full`. Then every input file by content hash, and then the dependency
fold:

```go
for _, file := range inputFiles {
    fmt.Fprintf(h, "file %s %s\n", file, b.fileHash(filepath.Join(p.Dir, file)))
}
for _, a1 := range a.Deps {
    p1 := a1.Package
    if p1 != nil {
        fmt.Fprintf(h, "import %s %s\n", p1.ImportPath, contentID(a1.buildID))
    }
    if a1.Mode == "preprocess PGO profile" {
        fmt.Fprintf(h, "pgofile %s\n", b.fileHash(a1.built))
    }
}
```

### 1.4 One correction to the brief, and it matters

The brief said the action ID "folds in the action IDs of the action's
DEPENDENCIES". The code folds in `contentID(a1.buildID)` — the **content** half
of the dependency's build ID, i.e. the hash of the dependency's compiled
archive, not the hash of its recipe. A build ID is
`actionID/[.../]contentID` (`buildid.go:36-112`); `contentID` takes the part
after the last separator, and `updateBuildID` sets it to
`buildid.HashToString(hash)` where `hash` comes from `buildid.FindAndHash`,
which zeroes the embedded build ID before hashing so a file's content hash does
not depend on itself (`buildid.go:696-707`).

This is a strictly better mechanism than keying on the dependency's recipe,
because it gives **cutoff**: if a leaf changes in a way that produces a
byte-identical archive, the leaf recompiles but its dependents do not. Probed
on this box (module with `probe/leaf` and `main`, fresh `GOCACHE`):

| change to `leaf` | recompiled |
|---|---|
| append a trailing comment (shifts no line numbers) | `probe/leaf` only — **`main` was a cache hit** |
| change an inlinable function's body (`x*2` → `x*3`) | `probe/leaf` **and** `main` |
| change a constant deep inside a function far over the inline budget | `probe/leaf` **and** `main` |

The third row is the honest limit of gc's granularity. The cut is at the whole
archive, which contains the compiled object as well as the export data, so a
change confined to a body that could never cross the package boundary still
invalidates every dependent. gc buys soundness under cross-package inlining
with a conservative key, not with a precise dependency analysis. **That is the
mechanism to copy: the importer's key contains a hash of the exporter's
output, so any change to what could be inlined is a change to the key.**

### 1.5 The fingerprint is a second, independent check

The key protects the cache. The fingerprint protects the link. Each object
records the fingerprint of every package it imported; `cmd/link` compares the
fingerprint an importer recorded against the one the imported library carries
and, on disagreement, `Exitf("fingerprint mismatch: %s has %x, import from %s
expecting %x")` (`cmd/link/internal/ld/lib.go:2530`). It is also folded into the
ELF build-ID note (`ld/elf.go:1610`) and emitted as a symbol (`ld/symtab.go:580`).
So even if the cache were wrong, an image assembled from mismatched export data
fails loudly at link time rather than miscompiling.

### 1.6 Eviction and size management

`DiskCache.Trim` (`cache.go:378`) is age-based, not size-based, and there is no
size cap anywhere:

- `mtimeInterval = 1h`: a cache file's mtime is refreshed on use at most hourly
  (`markUsed`), so the mtime is an approximate last-used time bought cheaply.
- `trimInterval = 24h`: a trim runs at most once a day, gated by
  `<dir>/trim.txt`.
- `trimLimit = 5 * 24h`: entries whose mtime is older than
  `now - trimLimit - mtimeInterval` are removed. Only `-a` and `-d` files are
  eligible.

The trim walks all 256 subdirectories. `GOCACHEPROG` (`cache/prog.go`) allows an
external process to implement `Get`/`Put`/`Close` instead, which is how a shared
or remote cache is bolted on.

---

## Part 2 — goc, measured

Measured with `cmd/stagetime` (committed on this branch), which calls exactly
what `cmd/goc`'s default path calls, in the same order:
`goc.CompileExecutableFor` → `opt.OptimizeModule` →
`arm64.CompileToObjectAndAssembly` → `MarshalELF`.

Two programs:

- **small** — `goc/testdata/fmt_sprintf.go`, 10 lines, **5083 functions**,
  69 977 blocks, 287 952 instructions after the stdlib closure.
- **http** — `goc/testdata/stdlib_http_tls_client_server.go`, 52 lines,
  **14 901 functions**, 313 385 blocks, 1 153 659 instructions.

### 2.1 Stage attribution

| stage | small wall | small alloc | http wall | http alloc |
|---|---:|---:|---:|---:|
| front end (parse, typecheck, IR lowering) | 4.08 s | 1.11 GiB | 18.98 s | 5.14 GiB |
| `opt.OptimizeModule` | 9.98 s | 3.21 GiB | 43.91 s | 12.71 GiB |
| back end + object | 2.05 s | 2.93 GiB | 11.49 s | 14.23 GiB |
| **total** | **16.16 s** | **7.25 GiB** | **74.4 s** | **32.1 GiB** |

Run-to-run spread across five small-program runs: front end 4.05–4.21 s, opt
9.80–10.31 s, back end 2.00–2.35 s, total 15.9–16.8 s. The http program was run
twice; its front end was 18.98 s and 19.50 s. Whole-process figures:
small 17.0 s wall / 44.2 s CPU; http 79.3 s wall / 165 s CPU / **4.28 GiB peak
RSS**. The back end is the only parallel stage (10.4 CPU-s in 2.05 s wall on
small); GC is 39% of all CPU samples.

The compiler is **deterministic**: two separate processes on the same source
produced 5083/5083 identical pre-optimisation function IRs and 4131/4131
identical post-optimisation ones (sha256 of `ir.Func.String()`). Every
comparison below rests on that control.

### 2.2 Inside the front end — 61%/53% is per-package, 35%/44% is whole-program

Line-level CPU inside `goc.compile`, from a CPU profile (`pprof -list`). The
front end is essentially serial (3.78 s CPU against 4.08 s wall on small), so
these read as wall time.

| `goc/compile.go` | small | http | nature |
|---|---:|---:|---|
| `sharedSourceWorld` (parse + typecheck the closure) | 0.46 s | 0.47 s | per package |
| `conf.Check` (typecheck the root file) | 0.19 s | 0.70 s | per package |
| `generator.funcDecl` (**IR generation**) | **1.58 s** | **7.78 s** | per package |
| `generator.globalDecl` | 0.06 s | 0.41 s | per package |
| `collectDynamicTypes` | 0.44 s | 1.04 s | whole program |
| `reachableFunctions` | 0.29 s | 2.01 s | whole program |
| `addInterfaceMethodWrappers` | 0.18 s | 1.97 s | whole program |
| `opt.LowerHeapAllocations` | 0.34 s | 2.47 s | whole module |
| `opt.InlineHeapAllocations` | 0.09 s | 0.36 s | whole module |
| everything else | ~0.15 s | ~0.45 s | mixed |
| total CPU in `goc.compile` | 3.78 s | 17.66 s | |

Summing the classes: per-package 2.29 s of 3.78 s (61%) on small and 9.36 s of
17.66 s (53%) on http; whole-program-or-whole-module 1.34 s (35%) and 7.85 s
(44%).

Two facts here shape the whole design.

**Parse and typecheck are cheap, and cannot be skipped anyway.** They are
0.65 s of 4.08 s (small) and 1.17 s of 19.50 s (http). And
`reachableFunctions`, `collectDynamicTypes` and `addInterfaceMethodWrappers`
take `fset`, `*types.Info` and `loader.units` — they consume ASTs and go/types
objects, not IR. A cache that skipped the type checker would leave them with
nothing to run on. The expensive per-package work is `funcDecl`, goc's own IR
generation, and *that* is what a cached unit replaces.

**goc's front end only lowers reachable functions.** `funcDecl` is driven by the
list `reachableFunctions` returns. A cached per-package unit therefore has to
carry every function in the package, not the reachable subset — otherwise the
key would have to include the reachable set, which is a property of the whole
program and would make the unit unreusable. gc has the same property and
resolves it the same way: it compiles every function in a package regardless of
whether the importer uses it.

### 2.3 Inside the optimiser — 15% is per-package, 85% is whole-module

`opt.DefaultPipeline` splits cleanly at the first inline fixpoint. Everything
before it (`mem2reg`, then the `clean` fixpoint: fold, copy, loadelim,
deadalloc, gvn, jumpthread, simplifycfg, dce) touches one function at a time and
never looks at another function. Everything from `inline-fixpoint` onward can
splice any function into any other. `stagetime -split-opt` runs the two halves
as separate `opt.Run` calls and times them:

| | small | http |
|---|---:|---:|
| per-function prefix (`mem2reg` + `clean`) | **1.44 s** | **8.34 s** |
| whole-module remainder (inline, unroll, constantp, ifconvert, tailmerge, deadfunc, gcm, inline-nosplit) | **8.58 s** | **36.08 s** |
| sum | 10.02 s | 44.42 s |
| unsplit `OptimizeModule`, same tree | 9.98 s | 43.91 s |

The split costs 0.4–1.2%, from the two halves not sharing one `changeLog`. The
prefix is real work — it takes the small module from 287 952 to 183 950
instructions (−36%) and http from 1 153 659 to 810 647 (−30%) — but it is
**15% of the optimiser** on small and **19%** on http. The other 85% / 81% is
the part that reads the whole program.

Per-pass CPU inside the optimiser (small, from the profile) confirms where it
goes: the interprocedural passes are `inlineModule` 1.25 s and
`InlineIntoNoSplitCallers` 0.36 s; the rest is per-function passes re-run on the
code inlining produced — `jumpThread` 1.54 s, `GVN` 0.93 s, `SimplifyCFG`
0.87 s, `LoadElim` 0.85 s, `DCE` 0.84 s, `Fold` 0.63 s, `DeadAlloc` 0.49 s,
`GCM` 0.42 s, `IfConvert` 0.38 s, `Mem2Reg` 0.35 s. The inliner is not
expensive; cleaning up after it is.

### 2.4 The blast radius of a source change is one function

This is the measurement the design turns on. Three compiles of the small
program, diffed function by function on a digest of the IR:

| comparison | pre-opt | post-opt |
|---|---|---|
| same source, two processes (control) | 5083 of 5083 identical | 4131 of 4131 identical |
| `42` → `43` in `main` | **5082 identical, 1 changed** | **4130 identical, 1 changed** |
| an extra `fmt.Sprintf` statement in `main` | **5082 identical, 1 changed** | **4130 identical, 1 changed** |

No functions added or dropped in either case. Whole-module optimisation did
**not** spread a root-package edit across the module: the inliner splices
callees into callers, the callees did not change, and only the caller that
changed differs. This is the empirical reason a per-package cache is not
obviously doomed here.

The counterweight, from the same tooling: comparing the module after the
per-function prefix against the module after the whole-module remainder, **2936
of the 4131 surviving functions were rewritten and 952 were dropped**. So the
whole-module stage cannot simply be skipped — its output differs from its input
for 71% of surviving functions. The cache would have to reproduce that output,
not bypass it.

### 2.4b The blast radius of a *leaf* change is 0.7% of the module

The root-package edit above is the easy case: nothing inlines *out of* `main`.
The hard case is a widely inlined leaf, which is exactly what gc's key is
built for. Measured by temporarily editing `stdlib/src/runtime/stubs.go` and
restoring it (the tree is byte-identical afterwards; `git status` was checked).
The target was `runtime.alignUp`, a three-line `//go:nosplit` helper with 115
call sites in the vendored runtime.

Two edits, so the effect of changing the code can be separated from the effect
of moving it:

| edit | pre-opt changed | post-opt changed |
|---|---:|---:|
| **control** — two comment lines inserted above `alignUp`, nothing else | 4 of 5083 | **47 of 4131 (1.1%)** |
| **treatment** — `alignUp`'s body rewritten to compute the same value in three statements (also +2 lines) | 4 of 5083 | 47 of 4131 |
| **treatment vs control** — isolates the body rewrite | **1** (`runtime.alignUp`) | **28 of 4131 (0.7%)** |

Two things fall out.

**The real blast radius of changing a heavily inlined leaf is 28 functions, 0.7%
of the module** — `alignUp`'s inline sites: the `mallocgcTiny*` family,
`mallocgcSmallScanHeader`, `linearAlloc.alloc`, `SetFinalizer` and the rest.
Whole-module optimisation does not smear a leaf change across the program
either.

**Positions propagate, and they propagate further than the code does.** The
control changed no semantics at all, yet 4 pre-optimisation functions differ —
`alignUp`, `alignDown`, `bool2int`, `divRoundUp`. That is precisely every
function with a Go body below the insertion point in `stubs.go`; the other 15
declarations down there are assembly-implemented and have no body to shift.
They differ because `ir.SrcPos` line numbers moved — and
after inlining that reaches 47 functions. So a content-addressed key over goc
IR is line-shift-sensitive by construction, since positions are in the IR and
have to be. gc has the identical property, which is why the §1.4 probe used a
*trailing* comment: a comment that shifts no line numbers produces a
byte-identical archive and cuts off, and one that shifts lines does not. This
is a fact to design around, not a defect: it means the practical hit rate of
any such cache is governed by how often edits move lines in files other people
inline from, and in the common case — editing your own package — the answer
measured in §2.4 is one function.

### 2.5 goc's IR already serializes, and it is fast

`ir/binary.go` exists and its own doc comment says what it is for: "The on-disk
format lets a compiled unit (an optimized module) be cached to disk and
reloaded, skipping the front-end and optimizer." Measured:

| | size | `MarshalBinary` | `DecodeModule` |
|---|---:|---:|---:|
| small, pre-opt | 20.2 MiB | 0.14 s | 0.17 s |
| small, post-opt | 35.8 MiB | 0.28 s | 0.36 s |
| http, pre-opt | 111.3 MiB | 0.75 s | 0.89 s |
| http, post-opt | 165.6 MiB | 1.16 s | 1.60 s |

Decoding the *entire* 5083-function pre-optimisation module costs 0.17 s
against a 4.08 s front end. Serialization cost is not what stands in the way.

### 2.6 What does stand in the way: goc emits IR its own verifier rejects

`ir.DecodeModule` ends with `VerifyModule(m)` and returns the error instead of a
module. On both programs it fails:

```
ir: time.deferwrap.580.8: start: add reads %0, which nothing defines
ir: net.methodvalue.net.file.close.22.8.2069: start: add reads %0, which nothing defines
```

The failure is not in the encoding — marshal and decode both complete. Running
`ir.Verify` directly on the module straight out of the front end, before
anything is written: **211 of 5083 functions (4.2%) fail**, and they are all the
same shape — `deferwrap`s, `methodvalue`s and closures whose entry block reads a
temporary nothing defines:

```
time.deferwrap.580.8                                   start: add reads %0
time.deferwrap.408.8                                   start: add reads %0
syscall.methodvalue.sync.RWMutex.RUnlock.73.8.1115     start: add reads %0
runtime.traceRegionAlloc.alloc.func.63.14              start: add reads %0
runtime.pageAlloc.sysGrow.func.117.32                  start: add reads %6
```

So the round trip cannot be used today, and fixing this — either the front end's
closure/defer-wrapper lowering or the verifier's model of it — is the first item
of work for any IR-on-disk cache. It is a 4.2% defect, not a rewrite.

### 2.7 What the existing packs actually save

Measured end-to-end through the `goc` CLI, `CG12_PACK_CACHE` pointed at a
private directory so the shared cache was untouched:

| | wall |
|---|---:|
| build a pack carrying `fmt` (`goc build-runtime -O -packages fmt`) | 15.9 s, 20.8 MB |
| small program, monolithic `goc -O` | **16.45 s** |
| small program, `goc -O -runtime fmt.gocrt` | **5.02 s** (−69%) |
| `fmt`+`sort`+`strings` program, monolithic | 16.71 s |
| `fmt`+`sort`+`strings` program, against the `fmt` pack | 5.08 s (−70%) |
| `os`-only program, monolithic | 12.43 s |
| `os`-only program, against the `fmt` pack | **refused** |

Both pack-linked executables were run and exited 0.

The refusal is verbatim: `goc: none of the 1 prebuilt runtimes offered is usable
by this program`. A pack is usable only by a program whose loaded closure
*contains* the pack's closure (`runtimepack.Manifest.UsableBy`), so packs
degrade gracefully **upward** — the `fmt`+`sort`+`strings` program used the
`fmt` pack and compiled the extra two packages itself for no measurable extra
time (5.08 s against 5.02 s) — and fail closed **downward**. The granularity
weakness is real, but it is the subset direction, not the superset direction,
which is a narrower complaint than the brief's framing assumed.

**And the pack's floor is the front end.** Running the pack path through the
same stage timers:

| stage, small program against the `fmt` pack | wall |
|---|---:|
| read the pack manifest | 0.04 s |
| **front end** | **4.20 s** |
| `opt.OptimizeModule` (module now 600 functions, was 5083) | 0.19 s |
| back end + object | 0.18 s |
| total | 4.61 s |

The front end is **91% of what a pack-linked compile still costs**, and it is
unchanged from the monolithic 4.08–4.21 s. This is by construction, not by
accident: `goc/runtime_split.go` states that both halves of a split "run the
same whole-program front end — the same parse, the same type check, the same
reachability walk, the same interface analysis — and produce the same IR they
would have produced on their own. Only at the very end does the program module
drop the definitions the prebuilt object already has." The subtraction is
deliberately late because that is what makes a differential comparison of the
two images meaningful. Measured consequence: the front end lowered all 5083
functions and then 4483 of them were thrown away.

### 2.8 The pack cache has no eviction

`cmd/goc/packcache.go` has `readCachedPack` and `writeCachedPack` and nothing
else. No trim, no size cap, no mtime tracking, no `-a`/`-d` split. The key
covers the hashed goc binary, so **every rebuild of the compiler orphans every
pack**, permanently, at 20.8 MB per package set for `fmt` and tens of MB for
`net/http`. A week of compiler development at a few builds a day across three
package sets is a few GB of dead files in `~/.cache/cg12/runtime-pack`. gc's
5-day mtime trim is the thing to copy here, and it is about fifty lines.

---

## Part 3 — The design

### 3.1 The cacheable unit, and where the line falls

> **Superseded in one respect: the unit is a function, not a package.** A package
> that declares one generic cannot be a unit, because an instantiation is a
> function of that package which exists only because an importer asked for it —
> and at package granularity that excluded 79% of the small program's lowered IR
> and 47% of the http program's, `runtime` included. Making the unit a *function*
> and excluding only the instantiations themselves takes the cacheable share to
> 95.4% and 90.3% of lowered IR. The boundary that licenses it — a non-generic
> function lowers identically in two programs that make its package carry
> disjoint instantiation sets — is proved in `goc/functionlowering_test.go`; the
> classification and the key are `goc/functioncache.go`; the measurement is in
> CCWORK_REPORT.md, "Stage 2 of per-package caching". Everything below about
> *where the line falls in the pipeline*, and every clause of §3.2, still holds.
>
> **Built, and measured.** The store is `goc/functionstore.go`, the merge is
> `goc/functionmerge.go`, the shared disk mechanics and the eviction policy this
> document asked for in item 3 are `internal/cachefile`, and `goc/compile.go`'s
> lowering loop consults it per declaration. `CG12_FUNC_CACHE=<dir>` turns it on
> and `CG12_NOCACHE=1` turns it off.
>
> Storage is one file per package, content-addressed by the key's digest with one
> fanout level; validation is per function. A cold and a warm compile in separate
> processes produce byte-identical executables on four programs and both `-O` arms
> (`scripts/function-cache-check.sh`), and so does a program compiled entirely
> from units a *different* program wrote.
>
> Two corrections to what this document projected. First, the line does NOT fall
> after `mem2reg`+`clean` as §3.1 recommends and item 5 repeats: what is stored is
> the front end's output, before any optimiser pass. `goc/functionoptimiser_test.go`
> is why — 518 of 2453 shared cacheable functions do not survive
> `opt.OptimizeModule` in both of two programs, because `DeadFunc`'s answer is a
> whole-program fact, so anything downstream of the merge has to be redone anyway.
> Second, the delivered saving is 9.7% and 8.8% of a `-O` compile against §3.4's
> 18–21% ceiling — not because the cache underperformed (it removes 69–79% of the
> stage it covers) but because `opt.OptimizeModule` grew after that ceiling was
> measured, so the fraction has a larger denominator than it did. Without `-O` the
> same absolute saving is 21.8% and 17.0%.
>
> **A unit must be self-sufficient.** The scheme above had a defect the
> same-program check could not see. Interned artifacts -- itabs, runtime type
> descriptors, string literals, the eight content-keyed tables lowering journals --
> belong to no declaration: the first one to want `.goc.type.time.Time.<sha8>`
> mints it and every later one gets a table hit. A delta that recorded only what
> its declaration APPENDED therefore recorded the reference and not the definition,
> and was usable only by a program containing whichever declaration minted it.
> Across programs it was not: **357 of 408 corpus programs failed to link** against
> a cache one program had filled.
>
> The fix is that a unit carries the definition of every artifact it references,
> AND the position at which it referenced it. The position is the half that is easy
> to miss: a cold compile mints an artifact at the point of first reference, and
> `Module.Data` order is the order `arm64/assembly.go` lays data out in, so
> carrying definitions without positions gives a program that links and is a
> different binary — measured, the same symbols with 1874 of them at different
> addresses. The journal is in `goc/functionstore.go`; the walk that filters a
> stored sequence by what the module already defines, which is what a cold compile
> does at an `ensure*` site, is `goc/functionmerge.go`.
>
> Two things had to become uncacheable, and both are cases where a delta is not a
> function of its package. A declaration whose lowering read the program's
> implementation set — `materialiseInterfaceImplementations`, which mints the
> descriptor and itab of every type in the PROGRAM implementing the interface being
> converted to — carries one program's set into another; 89 of 406 corpus programs
> came out that way before it was refused. And an artifact minted before the
> journal exists cannot be carried, which is why the journal now starts before the
> global initializers lower rather than at the declaration loop.
>
> After it: **406 of 406** corpus programs link and match their own cold image
> against a cache filled by a different program, from either of two fillers. The
> cost is the refusals — the http/tls saving falls from 17.3% to 7.9% of a whole
> non-`-O` compile, `fmt_sprintf` from 21.2% to 19.7% — and it is recoverable by
> hoisting `materialiseInterfaceImplementations` into a whole-program pass, which
> is a change to the cold path and so a separate one.
>
> **It is now on by default, and the default is per caller.** `cmd/goc` calls
> `goc.UseFunctionCacheByDefault`; `goc.Compile` called in process does not have a
> cache unless it asks for one. The asymmetry is forced by clause 9 of §3.2, the
> compiler binary's own hash: right for a released binary, whose bytes do not move
> between compiles, and wrong inside `go test`, which builds a fresh test binary
> per package under test and would therefore fill a complete set of units and read
> none of them back. `CG12_NOCACHE=1` still turns off everything;
> `CG12_FUNC_CACHE=off` turns off this cache alone; `auto` and `<dir>` are
> unchanged. The default location is `os.UserCacheDir()/cg12/function-cache`.
>
> **The first compile is slower, and that is what a user meets first.** A cold fill
> pays for the key (hashing every source file of the closure and the compiler
> binary) and for encoding every unit it stores, and gets nothing back. Measured in
> separate processes against a `CG12_NOCACHE=1` control, median of three:
>
> | | cold fill | warm |
> |---|---|---|
> | `hello.go` | **+5.0%** | −28.6% |
> | `hello.go -O` | **+1.9%** | −13.6% |
> | `fmt_sprintf.go` | **+4.7%** | −18.3% |
> | `fmt_sprintf.go -O` | **+1.4%** | −9.6% |
> | `stdlib_http_tls_client_server.go` | **+2.9%** | −8.5% |
> | `stdlib_http_tls_client_server.go -O` | **+1.8%** | −4.8% |
>
> One to five per cent once, in exchange for five to twenty-nine per cent on every
> compile after. It is the right trade and it is not a free one, and the smallest
> program pays the largest cold penalty because the fill is a fixed cost against
> the least work.
>
> **Eviction is two bounds, not one.** Five days since last use, and least recently
> used beyond a 1 GiB budget, both in `internal/cachefile.Trim` and both tested in
> `internal/cachefile/cachefile_test.go`. The age cutoff alone is not enough for a
> cache on by default for the same reason the key needs clause 9: a box that
> rebuilds the compiler mints a whole new generation of units every time, and five
> days of that is unbounded. A read refreshes an entry's mtime (hourly
> granularity), so a unit a build is using is the last thing to go.
>
> **A broken cache never fails a compile.** A read that fails is a miss, a unit
> that does not decode is a miss — the format carries a sha256 of its own body — and
> a write that fails is counted and dropped. `goc/functioncachedefault_test.go`
> holds it against nine ways of breaking the store, including a read-only directory
> that already has units in it, which still serves 84% of the lowered IR.
>
> **A stored delta must be a function of its package, and there is now an
> instrument for that.** `goc/functioncachedelta_test.go` compares the deltas two
> programs stored under the same unit key: the key is a content address of the
> package source, the dependency identities, the target and the compiler, so a
> disagreement about what one declaration contributed came from the program. It
> found two latent leaks of the same shape as the `internTypeEqualTarget` one —
> `NewFiles` recorded which files a declaration was first *in the program* to
> touch, and the pointer key journalled with a runtime type was spelled by
> whichever declaration reached the type first — neither of which had yet produced
> a wrong image. Both are fixed; see CCWORK_REPORT.md, "Stage 3".

**The unit is: goc's IR for every function and global of one package, after
`funcDecl`/`globalDecl` and after the per-function prefix of the optimiser
(`mem2reg` + `clean`), and before anything that reads another package.**

That boundary is forced from both sides:

- It cannot be *later*. The next pass is the inline fixpoint, and from there on a
  function's form depends on which callees the inliner chose to splice into it,
  which depends on the whole program. Measured: the whole-module remainder
  rewrites 71% of surviving functions and drops 952 of 5083.
- It should not be *earlier*. The per-function prefix is 1.44 s / 8.34 s of
  genuinely per-package work that would otherwise be redone every build, and
  none of its passes reads another function: `opt/mem2reg.go`, `fold.go`,
  `copy.go`, `loadelim.go`, `dse.go`, `gvn.go`, `jumpthread.go`, `cfg.go`,
  `dce.go` and `alias.go` contain no reference to `ir.Module` or `Func.Module()`
  at all (checked, not assumed). The escape machinery that *is* module-wide
  (`opt/escapefacts.go`) is not in this prefix.

**The line between cached and not-cached therefore falls immediately before
`opt.DefaultPipeline`'s first `inline-fixpoint`.** What is saved is IR
generation and the per-function prefix. What is not saved is:

- parse and typecheck (0.65 s / 1.17 s), because goc's whole-program front-end
  analyses consume ASTs and `types.Info`, not IR;
- `reachableFunctions`, `collectDynamicTypes`, `addInterfaceMethodWrappers`,
  `LowerHeapAllocations`, `InlineHeapAllocations` (1.34 s / 7.85 s), which are
  whole-program by definition;
- the whole-module optimiser remainder (8.58 s / 36.08 s);
- the back end (2.05 s / 11.49 s).

This is the honest answer to the brief's question 3: goc keeps a per-package
cacheable stage and a whole-module stage that is not cached, and the line falls
much earlier than it does in gc — because gc's per-package stage *ends* with
codegen for that package, while goc's ends before its main optimiser has run at
all.

### 3.2 What the key must cover

Copy `buildActionID` structurally. For a unit belonging to package `P`:

1. a unit-format version number, so a format change is a miss and not a crash;
2. the import path of `P`;
3. every source file of `P` **by content hash** (`b.fileHash`'s role) — plus,
   for goc, the stdlib overlay and native-overlay files that apply to `P`, since
   `goc/stdlib_overlay.go` and `goc/native_overlay.go` can substitute
   definitions;
4. **for each package `P` imports, the content hash of that package's cached
   unit** — recursively, exactly `contentID(a1.buildID)`. This is the clause
   that makes the whole thing sound under cross-package inlining, and it is the
   one that must not be weakened;
5. the target (`goc.Target`);
6. `-O`;
7. `arm64.TextLayoutIdentity()` — the placement policy, which the environment
   can change without changing a byte of the compiler;
8. `opt.PipelineIdentity()` — which already exists and already exists *for this
   exact reason*: `GOC_OPT_PIPELINE` and `GOC_OPT_SKIP` change what is produced
   without changing the compiler binary;
9. the goc binary's own hash.

Items 5–9 are already the pack key's job and `cmd/goc/packcache.go` gets them
right; the per-package key should call the same helpers rather than grow a
second, drifting copy. The pack key's own weak link — `cToolchainIdentity`
hashing `cc --version` rather than the assembler binary — does not apply to a
per-package IR unit, which contains no assembled output.

Two goc-specific clauses have no gc counterpart:

- **The unit must be program-independent.** Since `funcDecl` today runs only on
  reachable functions, filling the cache must generate IR for every function in
  `P`. If instead the reachable set were folded into the key, every program with
  a different closure would miss, which is precisely the pack's failure mode
  reproduced at package granularity.
- **A fingerprint, checked on read.** gc's `[8]byte` sha256 of the payload, and
  `cmd/link`'s `checkFingerprint` refusing a mismatch, exist so that a wrong
  artifact fails loudly rather than miscompiling. `ir/binary.go` has a magic tag
  and a version byte but no content digest; add one and verify it after decode.

### 3.3 What goc's IR needs that it does not have

| need | status |
|---|---|
| serialization | **exists** — `ir/binary.go`, version 19, exercised in production by `ir/clone.go`. 20.2 MiB / 0.17 s to decode the whole small module. |
| a sound round trip | **broken** — 211 of 5083 functions (4.2%) fail `ir.Verify`, all `deferwrap`/`methodvalue`/closure entry blocks reading an undefined temporary. `DecodeModule` gates on `VerifyModule`, so nothing decodes today. **First work item.** |
| a stable fingerprint | **missing** — magic + version byte only. Add a payload digest, gc-style, and check it on read. |
| positions across a round trip | **exists, needs remapping** — `ir.SrcPos{File, Line, Col}` where `File` is a 1-based index into `Module.Files`. Within a module the round trip is exact. Merging *n* cached units means renumbering each unit's file indices into the combined table. Mechanical; `Module.File` already interns. |
| symbol identity across units | **exists** — cross-unit references are `ConstSym` carrying a `Sym string` name, and `ir.LinkerSymbol` mangles it. Names are the identity, so merging units needs no fixups. `checkUniqueFunctionSymbols` already refuses collisions. |
| type identity across units | **needs work** — `*AggType` is a pointer, serialized as an index into a per-module type table built by `collectTypes`. Two units each carry their own table, so merging must unify structurally-equal `AggType`s or the module ends up with duplicate descriptors for the same Go type. `collectTypes` already does this within one module; it needs a cross-unit version. |
| per-package granularity | **missing** — `MarshalBinary` is whole-module. A unit needs to be a slice of a module (its funcs, its data, its types, its files) that can be decoded independently and merged. |

### 3.4 The options, each with a measured saving

Baselines: small 16.16 s, http 74.4 s (harness totals); the small program is
16.45 s through the CLI, which adds the `cc` link.

**Option A — cache per-package IR at the front-end boundary only.**
Skips `funcDecl` + `globalDecl` for unchanged packages.
Saving: 1.64 s of 16.16 s = **10%** (small); 8.19 s of 74.4 s = **11%** (http).
Net of decode (0.17 s / 0.89 s): 9% / 10%.

**Option B — Option A plus the per-function optimiser prefix.**
Cache the unit after `mem2reg` + `clean`.
Saving: 3.08 s of 16.16 s = **19%** (small); 16.53 s of 74.4 s = **22%** (http).
Net of decode: **18% / 21%**.
This is the whole prize of per-package caching in goc as the compiler is
structured today. It is the ceiling, not an estimate of a first cut.

**Option C — Option B plus a memoised whole-module stage and back end, keyed
per function on its inline-dependency set.**
§2.4 and §2.4b give the ceiling: after a root-package edit 4130 of 4131
post-optimisation functions are byte-identical, and after rewriting a leaf
helper with 115 call sites 4103 of 4131 still are. So an ideal memoiser would
skip essentially all of the whole-module remainder (8.58 s / 36.08 s) and the
back end (2.05 s / 11.49 s). Ceiling, with Option B underneath it: 13.71 s of
16.16 s = **85%** (small); 64.10 s of 74.4 s = **86%** (http).
What it costs: the inliner must record, per function, the transitive set of
functions it consulted, and that set must be validated on lookup — a dynamic
dependency file, the same shape as gc's key but computed rather than declared.
Three passes do not fit the per-function model and need separate handling:
`DeadFuncElim` (a reachability computation over the whole call graph, cheap —
~0.14 s on small), `UnrollRecursion`, and `InlineIntoNoSplitCallers`, which by
its own comment runs last *because it measures frame layout*, so its input is
the code the back end will see. Nothing here is measured as an implementation;
only the ceiling is.

And that ceiling is measured against the *monolithic* compile — the
configuration packs already fix. Against a pack-linked compile there is almost
nothing left for Option C to take: the module is 600 functions by then, the
whole-module remainder costs 0.19 s and the back end 0.18 s. **Option C is only
worth its cost if packs are given up or cannot be used.**

**Option D — the existing whole-program packs.**
Measured: 16.45 s → 5.02 s, **−69%**, for a 15.9 s one-off pack build.
Floor: the front end, 4.20 s of the 4.61 s that remains (91%).
Failure mode: a program whose closure does not contain the pack's is refused
outright and falls back to the full 12.43 s compile or to building another pack.
No eviction, ever.

**Option B on top of Option D.** These compose, and the composition is the
interesting number: B removes `funcDecl`+`globalDecl` from the pack path's front
end, which is where 91% of the pack path's remaining time is. 5.02 s − 1.64 s ≈
**3.4 s**, i.e. **79% off the monolithic compile**, against the pack's 69%
alone.

### 3.5 Recommendation

**Keep packs as the primary mechanism. Do not replace them with gc's
per-package model — it does not transfer. Build Option B specifically to attack
the pack's floor, and give the pack cache the eviction it has never had.**

The evidence for not transferring gc's model is arithmetic. In gc, a package's
cacheable stage ends with codegen for that package, so a cache hit skips
essentially the whole cost of that package. In goc, the cacheable stage ends
before the optimiser's main work begins, because `opt.OptimizeModule` may splice
any function into any other and 85% of the optimiser's time is downstream of
that. Measured, per-package caching's ceiling in goc is 18–21% of a compile;
the pack's measured delivery is 69%. Reaching for gc's model because gc is a
good compiler would be reaching for the smaller number.

But packs are not the whole answer either, and the reason is measurable rather
than aesthetic: **91% of a pack-linked compile is a front end the pack cannot
touch**, because the split is subtractive and deliberately late. Option B is
worth building precisely there. It takes the pack path from 5.02 s to about
3.4 s on the small program — a further 33% — and its value grows with the
program, because `funcDecl` is 7.78 s on http against 1.58 s on small.

Option B, not Option C, is the complement to packs, even though C's ceiling is
four times larger. C's 85% is measured against a monolithic compile; once a pack
is in play the optimiser and back end it targets have already shrunk to 0.37 s
combined, so C would be buying back tenths of a second for the hardest piece of
machinery in this document. B targets the one stage a pack provably cannot
touch. If packs were ever abandoned the ranking inverts, which is worth
recording but is not the situation.

Work items, in order:

1. **Fix the 211 functions that fail `ir.Verify`** (4.2%, all closure /
   `deferwrap` / `methodvalue` entry blocks). Nothing else can start until
   `ir.DecodeModule` returns a module.
2. **Add a payload fingerprint to `ir/binary.go`** and check it after decode.
   Cheap, and it is the difference between a stale hit being a wrong binary and
   a stale hit being an error.
3. **Give the pack cache gc's trim** — mtime-on-use at hourly granularity, a
   daily trim, a 5-day cutoff. Fifty lines against unbounded growth that every
   compiler rebuild adds to.
4. **Split `MarshalBinary` into per-package units** with a cross-unit
   `AggType` unification step and file-index remapping, and add the key of
   §3.2 with recursive dependency content hashes.
5. **Cache after `mem2reg`+`clean`**, not before — it is 1.44 s / 8.34 s for no
   extra machinery.

**The trade-off, stated plainly.** Option B buys 18–21% of a cold compile, or a
third of a pack-linked one, in exchange for: a second cache with a second key to
keep honest; a per-package unit that must contain *every* function in the
package rather than the reachable ones, which makes filling the cache slower
than the compile it accelerates; and a new class of failure — a wrongly-keyed
unit is a miscompiled program, not a slow build. The recursive dependency
content hash of §3.2 clause 4 and the fingerprint of §3.3 are what keep that
class closed, and they are not optional.

If only one thing is built from this document, build item 3: it is fifty lines,
it needs none of the rest, and unbounded pack-cache growth is costing disk on
every machine that compiles goc today. Item 1 is worth doing on its own merits
too — 4.2% of the functions goc emits fail its own IR verifier, and that is a
defect in the tree whether or not anything is ever cached.

---

## Appendix — measured vs read, and how to reproduce

**Read from the host toolchain's source** (`/home/evan/.local/go1.26.1/src`):
the Unified IR grammar and sections; `bodyIdx`/`funcExt`/`exportBody`/
`relocFuncExt`/`writeUnifiedExport`; `CanInline` and `inlineMaxBudget`;
`DumpTo`'s fingerprint; `buildActionID` and every clause of it; the
ActionID/OutputID split, `fileName`, `Put`/`GetBytes` verification;
`checkFingerprint`; `Trim`/`markUsed`/`trimSubdir` and their constants;
`GOCACHEPROG`.

**Read from this repo:** `ir/binary.go`'s format and stated purpose; `ir.SrcPos`
and `Module.File`; `ir.Const.Sym`; `ir.AggType`; `opt.DefaultPipeline` and
`PipelineIdentity`; `cmd/goc/packcache.go`'s key and absence of eviction;
`goc/runtime_split.go`'s subtractive-split argument; `prebuilt.CompileProgram`'s
ordering (front end, then subtract, then optimise, then back end).

**Probed on this box with `go build`:** the three-row invalidation table in
§1.4, on a two-package module with a fresh `GOCACHE`.

**Measured on this box with `cmd/stagetime`:** every number in Part 2.

```
go build -o /tmp/stagetime ./cmd/stagetime

# stage attribution + serialization + verifier count
/tmp/stagetime goc/testdata/fmt_sprintf.go
/tmp/stagetime goc/testdata/stdlib_http_tls_client_server.go

# per-package prefix vs whole-module remainder
/tmp/stagetime -serialize=false -split-opt goc/testdata/fmt_sprintf.go

# blast radius: compile two variants, diff the per-function digests
/tmp/stagetime -serialize=false -posthash a.post prog_a.go
/tmp/stagetime -serialize=false -posthash b.post prog_b.go

# leaf blast radius: edit stdlib/src/runtime/stubs.go, rerun, git checkout --
#   control   = insert two comment lines above alignUp
#   treatment = rewrite alignUp's body to compute the same value in 3 statements

# what a pack does and does not save
export CG12_PACK_CACHE=$(mktemp -d)
goc build-runtime -O -packages fmt -o fmt.gocrt
/tmp/stagetime -serialize=false -runtime fmt.gocrt goc/testdata/fmt_sprintf.go
```

Per-pass and line-level attribution came from `-cpuprofile` plus
`go tool pprof -top -cum` and `pprof -list='cg12/goc\.compile$'`.

The leaf-change measurement in §2.4b temporarily edited
`stdlib/src/runtime/stubs.go` and restored it with `git checkout --`; the
working tree was verified byte-identical afterwards, and nothing in `stdlib/` is
modified on this branch.

**Not measured, and flagged as such:** Option C as an implementation — only its
ceiling (§3.4); the `net/http` pack's 154 s build cost, which is
`packcache.go`'s own figure and was not re-run here; the effect of any of this
on `goc compile-batch`, which amortises pack reads across a batch and would
change the arithmetic for a corpus run.
