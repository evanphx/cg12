# Letting the prebuilt pack carry the standard library

Branch: `ccwork/pack-stdlib`, off `perf/test-suite`. The previous job's report has been moved
to `docs/report-matrix-speed.md` so this file is only about this job.

Status: **the lever works.** The full capability matrix goes **201.2 s -> 56.4 s (3.6x)**
against a matched control on this same tree, at 338 subtests / 337 PASS / 1 EXPECTED FAILURE
/ 0 FAIL / 0 KNOWN GAP. Getting there turned up three defects in goc, two of them live
miscompiles that had nothing to do with packs. Everything below was measured or reproduced on
this box; anything not verified is named as such at the end.

## Baseline measured here, before any change

Box: linux/arm64, 64 cores, ~240 GB RAM. `cpu_slots: 14` for this job.

| | wall | cpu | image |
| --- | ---: | ---: | ---: |
| `goc build-runtime` (runtime-only root) | 4.57 s | 216% | 8.78 MB pack |
| `hello.go` against that pack | 2.09 s | 199% | 6.90 MB |
| `fmt_sprintf.go` against that pack | 7.24 s | 182% | 13.45 MB |
| `stdlib_http_tls_client_server.go` against that pack | **157.96 s** | 115% | 69.71 MB |

## First measurement of the lever itself

A pack built from a root that blank-imports `net/http` and `net/http/httptest`:

| | wall | size |
| --- | ---: | ---: |
| `goc build-runtime -packages net/http,net/http/httptest` | 153.8 s | 73.9 MB pack |
| `stdlib_http_tls_client_server.go` against it | **23.3 s** (was 158.0 s) | — |

So the compile itself is **6.8x** faster. The link then failed, and the reason is the first
finding below.

## Finding 1: two distinct closures could be compiled to one symbol (a real miscompile, not a pack problem)

Building a pack that carries `net/http` fails at the system linker with

    multiple definition of `crypto_internal_fips140_nistec_func_114_16'
    multiple definition of `crypto_internal_fips140_nistec_func_393_28'

Three definitions each, **with different sizes** (0x228, 0x3e8, 0x318) — three genuinely
different functions under one name. They are the closures passed to `sync.Once.Do` in
`p224B()`, `p384B()` and `p521B()`, and in `(*P224Point).generatorTable()` and its P384 and
P521 twins. `stdlib/src/crypto/internal/fips140/nistec/p224.go`, `p384.go` and `p521.go` are
generated from one template, so those literals sit at *identical* line and column.

`goc/compile.go`'s `functionLiteral` named a literal `<pkgpath>.func.<line>.<col>` whenever
`g.functionName` was empty — and `functionName` is set only for a generic instantiation or a
package initializer, so the package-path form was the ordinary case. Position alone does not
identify a literal within a package.

**This is not confined to the pack.** `obj.prepareELF` builds its symbol index with
`symIndex[s.Name] = i`, keeping the last, so in a *monolithic* build every reference to the
name binds to whichever definition was emitted last: `p224B` would have called `p521B`'s
initializer. It was invisible only because those symbols are local, so the system linker
never had to choose. Exporting them — which a prebuilt pack must do — is what made it loud.

Reproduced in 3.1 s by compiling

    package main
    import "crypto/internal/fips140/nistec"
    func main() {
        nistec.NewP224Point().ScalarBaseMult(make([]byte, 28))
        nistec.NewP384Point().ScalarBaseMult(make([]byte, 48))
        nistec.NewP521Point().ScalarBaseMult(make([]byte, 66))
    }

which yields, before the fix, `crypto/internal/fips140/nistec.func.114.16` and
`...func.393.28` three times each in `module.Funcs`.

Fixed by naming a literal after the declared function it is written in — which is what Go
itself does (`pkg.Func.func1`) and what the `functionName` branch was reaching for. Same fix
applied to the `rangefunc` yield symbol, which had the identical fallback.

## Finding 2: one runtime type hasher emitted three times

`emitRuntimeTypeHasher` had no "already emitted" guard, while its sibling
`ensureRuntimeTypeEqual` has always had one. Every map type with the same key type asks for
the same `<typetag>.hash` trampoline; `net/http` has three map types keyed by
`connectMethodKey`. The three definitions are byte-identical, so nothing was miscompiled —
but they are three definitions of one global symbol, which the system linker refuses.

## Guard added

`checkUniqueFunctionSymbols` now refuses any module in which two functions land on one
linker symbol, comparing the *mangled* spelling because that is where distinct Go names can
converge. A collision is a build error naming both functions rather than a silent rebinding.

## Finding 3: a *fixed superset* pack is not reachable from here, and the reason is bigger than the briefing's two items

With `net/http` in the pack, `stdlib_http_tls_client_server.go`:

| | |
| --- | ---: |
| compile against the runtime-only pack | 158.0 s |
| compile against the `net/http` pack | **23.7 s** (6.7x) |
| runs, exit status | 0 |
| output vs the runtime-only-pack build | byte-identical |
| output vs the host Go toolchain | byte-identical |

`hello.go` against that same pack is refused, and not by the linker: `checkProgramSymbols`
rejects it because the pack leaves **18,309 program symbols** undefined and `hello.go` defines
almost none of them. The briefing named two blockers -- interface dispatchers and the
package-init list. The list is far longer than that, and the extra entries are not
stubbable:

- **The whole Go type region** belongs to the program module by construction (RUNTIME_PLAN
  section 16: a descriptor's contents depend on what the program reaches, and cg12 compares
  descriptors by pointer). Every descriptor of every `net/http` type is a symbol the pack
  references and `hello.go` never generates.
- **Every static itab the pack degraded.** These are not unreachable: `runtime.itabsinit`
  walks each module's `itablinks` at startup, so a stubbed itab would be *read* on the way up,
  by every program, before `main`.

A stub is only safe where the thing it stands in for is unreachable. That is true of an
interface dispatcher and false of an itab the runtime enumerates at startup, so "stub
whatever the program did not supply" does not close this gap. Making a fixed superset pack
work needs the redesign section 16 already named -- the pack carrying enough about its types
to let a program reconstruct descriptors without lowering -- and that is not this job.

**What is reachable is a pack the program is allowed to be a superset of.** A program whose
import closure contains the pack's closure generates every symbol the pack left for it, which
is exactly the condition the existing `checkProgramSymbols` already enforces. So the pack
stops being one fixed artifact and becomes a set of candidates, each program taking the
richest one it is a superset of, and falling back to the runtime-only pack otherwise.

## Finding 4: the split's subtraction is unsound for any function containing an interface-to-interface type test

`stdlib_http_client_server.go` compiled against a `net/http` pack links and then aborts
deterministically (5/5 runs, exit 2), inside a goroutine `net/http.(*Transport).dialConn`
started. The same program against the runtime-only pack passes 3/3. `GOGC=off`,
`gcshrinkstackoff`, `GOMAXPROCS=1`, `asyncpreemptoff` and `gccheckmark` change nothing, so it
is not a GC or scheduling race. The signal is `SIGABRT` with `sigcode = -6` (SI_TKILL) and the
PC is in libc, i.e. a call to libc `abort()` — and goc emits exactly one thing that calls
`abort()` on a path a working program can reach: **a failed single-value type assertion**
(`goc/compile.go`'s `*ast.TypeAssertExpr` lowering).

The mechanism is `interfaceTypeMatch`. An assertion to an interface type is lowered to an
*inline chain of descriptor-pointer comparisons*, one per type that implements the interface,
and the candidate list comes from `g.functionDecls` — every method declared **anywhere in the
whole program**. That makes an ordinary function's body depend on the whole program's declared
method set.

RUNTIME_PLAN section 16 says a symbol the program module *keeps* is bit-for-bit what a
monolithic build would have emitted. That is true, and it is not the property that matters
here: a *subtracted* function is taken from the pack, and the pack compiled it against the
pack's method set. A program whose closure is strictly larger has more implementations, so a
pack function's chain is missing candidates — and the assertion fails on a type the program
introduced.

This is a pre-existing hole in the driver split, not something the standard-library pack
creates; carrying `net/http` is only what made it reachable. The runtime-only pack has the
same hole, and it has not bitten only because runtime code rarely asserts to a non-empty
interface.

**The fix is already sitting next to it.** `interfaceTypeWord` — the conversion path — emits
the same inline chain and then falls back to `runtime.getitab(inter, typ, canfail)`, which is
Go's own answer and depends on nothing but the two descriptors. `interfaceTypeMatch` has no
fallback: a miss is simply "no". Giving it the same fallback makes the test program-independent
and, as a side effect, fixes a second latent gap — today a type whose method set comes only
from an embedded field is in no `functionDecls` entry at all, so asserting it to an interface
it genuinely implements fails.

### The mechanism, measured directly

An instrumented compiler that prints each interface test's candidate count, run over the pack's
root (`package main; import _ "net/http"; func main() {}`) and over
`stdlib_http_client_server.go`, monolithically:

| interface | candidates when the pack compiled it | candidates the program would have used |
| --- | ---: | ---: |
| `interface{String() string}` | 184 | **198** |
| `interface{Read([]byte) (int, error)}` | 98 | **99** |
| `interface{Write([]byte) (int, error)}` | 76 | **77** |
| `interface{WriteString(string) (int, error)}` | 15 | **16** |

58 distinct interfaces are tested in the pack's compilation, 65 in the program's. Every
function the program subtracts and takes from the pack is testing against the left-hand column.

## What it measures

Box: linux/arm64, 64 cores, ~240 GB RAM, shared with two sibling jobs; this job declared 14
cpu slots. Every row is a full unsharded matrix run through `scripts/matrix-timing.sh`, with
`-v -count=1 -runtime-status-progress`, and every row is checked for
`subtests=338 pass=338 fail=0 declaredPASS=337 expectedFAILURE=1 knownGAP=0` rather than for
`ok`.

| run | wall | compile CPU | slowest single compile | run phase | bounding term |
| --- | ---: | ---: | ---: | ---: | --- |
| **quiet box** | | | | | |
| control: runtime-only pack, same tree (`-runtime-status-stdlib-packs=false`) | 201.2 s | 4392.7 s | 191.8 s | 14.6 s | slowest single compile |
| seven packs, **cold** cache | 210.3 s | 2783.6 s | 46.6 s | 15.1 s | building the packs (154 s) |
| seven packs, **warm** cache | **56.4 s** | 2801.4 s | 46.9 s | 15.3 s | slowest single compile |
| **loaded box (load average 168, siblings running)** | | | | | |
| control, runtime-only pack | 275.0 s | 6177.5 s | 264.4 s | 16.2 s | slowest single compile |
| seven packs, cold cache | 308.2 s | 5891.2 s | 97.6 s | 22.3 s | building the packs |
| seven packs, warm cache | 66.9 s | 3250.3 s | 56.4 s | 17.3 s | slowest single compile |
| seven packs, warm cache (repeat) | 99.4 s | 5360.6 s | 89.9 s | 27.7 s | slowest single compile |

Six full matrix runs, every one of them 338/338 with the right census. The box is shared with
two sibling jobs and their load moved during the day, so the honest statement is the *ratio*
against a control taken under the same conditions: **3.6x on the quiet box (201.2 -> 56.4 s)
and 2.8-4.1x under load (275.0 -> 66.9-99.4 s)**. The quiet-box control reproduces the 203.2 s
the previous job reported at `cpu_slots: 24`, which is what makes the two comparable at all.

**Afterwards the matrix is bounded by the slowest single compile again** — 46.9 s for
`stdlib-http/tls-client-server`, against `compile CPU / 64 = 43.8 s`, which is very nearly the
same number. Both terms would have to move to go much below 50 s; the pack lever has taken
this one about as far as it goes on its own.

**Cold, the packs cost exactly what the briefing predicted and buy nothing.** 210.3 s against
the control's 201.2 s: the 154 s of pack building replaces the 192 s slowest compile almost
one for one. The whole gain is in the cache.

### Per-program compiles

Standalone, no matrix load, against the seven packs:

| program | before | after |
| --- | ---: | ---: |
| `stdlib_http_tls_client_server.go` | 158.0 s | 27.9 s |
| `stdlib_http_redirect_keepalive.go` | 164.0 s | 25.3 s |
| `stdlib_http_client_server.go` | 163.9 s | 25.1 s |
| `stdlib_http_cookiejar.go` | 161.5 s | 20.9 s |
| `stdlib_http_multipart_form.go` | 161.1 s | 20.7 s |
| `stdlib_http_parse_roundtrip.go` | 161.3 s | 20.2 s |
| `stdlib_smtp_session.go` | 136.5 s | 12.3 s |
| `stdlib_crypto_x509_ed25519.go` | 133.4 s | 14.2 s |
| `stdlib_crypto_ecdsa.go` | 128.1 s | 10.4 s |
| `stdlib_crypto_ecdh_x25519.go` | 126.0 s | 9.4 s |
| `stdlib_crypto_hpke.go` | 125.3 s | 8.3 s |
| `stdlib_encoding_xml.go` (no usable pack) | 11.7 s | 12.3 s |
| `hello.go` (no usable pack) | 2.1 s | 2.4 s |

The last two are the cost side. A program with no usable pack pays 0.22 s to read the other
six packs' manifests and decide it cannot use them. Over 338 programs that is 74 s of CPU and
about 1 s of the matrix's wall clock; it is not free and it is not close to mattering. It
would go away if the manifest's selection fields were a separate, small blob in the container
— worth doing if the pack set grows much beyond seven.

## The design that survived

`-runtime` takes a comma-separated list of packs. goc runs the front end, and the moment it
knows which packages the program loaded it picks the pack carrying the most of those whose
closure the program's closure **contains**, then subtracts against that one. A runtime-only
pack carries nothing beyond the runtime, and every executable compiles the whole runtime
closure, so it is usable by every program and the list degrades to it rather than failing.

The seven roots the matrix builds are the largest package each of the eleven expensive
programs imports: nothing, `net/http`, `net/smtp`, `crypto/x509`, `crypto/ecdsa`,
`crypto/ecdh`, `crypto/hpke`. They share no common ancestor small enough to serve all of them
— `crypto/ecdh`'s closure is not a superset of `crypto/ecdsa`'s program's, and neither is
usable by an `encoding/xml` program — so it is one pack per shape rather than one pack.

| pack | build (cold) | size |
| --- | ---: | ---: |
| runtime only | 5.1 s | 8.8 MB |
| `crypto/ecdh` | 132.0 s | 48.5 MB |
| `crypto/hpke` | 134.2 s | 51.2 MB |
| `crypto/ecdsa` | 135.6 s | 51.9 MB |
| `crypto/x509` | 138.1 s | 54.1 MB |
| `net/smtp` | 141.5 s | 57.0 MB |
| `net/http` | 157.0 s | 73.7 MB |
| all seven, built concurrently | **157.0 s** | 345 MB |
| all seven, warm cache | **0.41 s** | |

### What a rich pack costs an image in bytes

Nothing, because no program carries a pack it cannot use. `hello.go` takes the runtime-only
pack and its image is byte-for-byte what it was. The question the briefing asked — what a
*fixed superset* costs a `hello.go` image — has no answer to measure, because `hello.go`
cannot be built against a `net/http` pack at all (Finding 3). For the programs that do take a
rich pack the image gets *smaller*: `stdlib_http_tls_client_server.go` is 69.71 MB against the
runtime-only pack and 67.32 MB against the `net/http` pack.

### The cache

`goc build-runtime` keeps its result under `$XDG_CACHE_HOME/cg12/runtime-pack`
(`CG12_PACK_CACHE` overrides, `CG12_NOCACHE=1` disables), keyed on a SHA-256 of: the pack
format version, the target, `-O`, the sorted package list, **the goc binary's own bytes**, the
**contents** of every file in the vendored `stdlib/` tree, and `cc --version`. A stale hit
would be a wrong image rather than a slow build, so the key is deliberately over-broad.
Hashing the tree by content rather than by mtime costs 0.19 s warm and means a checkout or a
worktree copy still hits.

One consequence worth knowing: `go build` stamps `vcs.revision` and `vcs.modified` into the
goc binary, so **every commit and every transition between a clean and a dirty tree
invalidates the whole cache**. That is correct — a different compiler must not reuse a pack —
but it means the cache pays off across repeated runs and shards of one revision, not across a
development loop. It cost me two full matrix runs to notice, which is why it is written down.

## Verification

Everything below was run on this tree at the tip of `ccwork/pack-stdlib`.

| check | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | ok, every package |
| `make test-goc-corpus` | ok, 860.3 s |
| `make test-goc-cmd` (`-count=1`, matrix skipped) | ok, 234.8 s |
| full capability matrix, warm packs | **338 subtests, 338 pass, 0 fail, 0 skip; 337 declared PASS, 1 EXPECTED FAILURE, 0 KNOWN GAP** |
| full capability matrix, cold packs | same census |
| full capability matrix, matched control (one pack) | same census |

**The complete list of non-passing capabilities is one entry:**
`runtime-panic/print-string` (`runtime_panic_print_string.go`), the declared EXPECTED FAILURE
the matrix has always carried. There are no FAILs, no SKIPs and no KNOWN GAPs. The
census is taken from a `-v` log by counting `--- PASS:` / `--- FAIL:` subtest lines and the
per-capability `PASS` / `EXPECTED FAILURE` / `KNOWN GAP` verdicts, not from `ok`.

### Determinism

`scripts/determinism-check.sh`, cold (`CG12_NOCACHE=1`) against warm, twice per program:

| program | before this branch | monolithic, this tree | against the seven packs |
| --- | --- | --- | --- |
| `hello.go` | identical | identical | identical |
| `fmt_sprintf.go` | identical | identical | identical |
| `gc_struct.go` | identical | identical | identical |
| `runtime_cleanup_frame_retention.go` | identical | identical | identical |
| `runtime_defer_capture_allocs.go` | known backend residue | DIFFERENT | DIFFERENT |

Unchanged: 4 of 5 byte-identical on both compile paths, with the one known residue
(RUNTIME_PLAN 5.10) still differing. No regression.

### Differential against the host toolchain

Every program that takes a rich pack, run against `go run` of the same source, comparing exit
status and full combined output:

    stdlib_http_client_server      MATCHES HOST      stdlib_crypto_x509_ed25519  MATCHES HOST
    stdlib_http_cookiejar          MATCHES HOST      stdlib_crypto_ecdsa         MATCHES HOST
    stdlib_http_multipart_form     MATCHES HOST      stdlib_crypto_ecdh_x25519   MATCHES HOST
    stdlib_http_parse_roundtrip    MATCHES HOST      stdlib_crypto_hpke          MATCHES HOST
    stdlib_http_redirect_keepalive MATCHES HOST      stdlib_smtp_session         MATCHES HOST
    stdlib_http_tls_client_server  MATCHES HOST      stdlib_encoding_xml         MATCHES HOST
    hello                          MATCHES HOST      fmt_sprintf                 MATCHES HOST

`stdlib_http_client_server` is the one that used to abort; it now exits 0 with output
identical to the host toolchain's, 5 runs out of 5.

### The corpus differential

`analysis/splitdiff` was extended to build a *set* of packs, let goc choose per program, link
with the objects of the pack it chose, and report which one that was. Over all 358 corpus
programs, compiled monolithically and against the pack set, run, and compared on exit status
and full output:

    programs=358  problems=2
    total CPU compile+link: split=1738.5s mono=4923.5s  ratio=2.83x
    total image bytes: split=4373055352 mono=3940369200  (11.0%)

    stdlib_http_client_server.go     split= 29.74s mono=217.20s pack=net/http
    stdlib_http_cookiejar.go         split= 24.77s mono=214.03s pack=net/http
    stdlib_http_redirect_keepalive.go split=30.40s mono=213.54s pack=net/http
    stdlib_http_multipart_form.go    split= 24.03s mono=213.10s pack=net/http
    stdlib_http_tls_client_server.go split= 33.58s mono=212.44s pack=net/http
    stdlib_http_parse_roundtrip.go   split= 23.57s mono=210.97s pack=net/http
    stdlib_smtp_session.go           split= 13.06s mono=204.15s pack=net/smtp
    stdlib_crypto_x509_ed25519.go    split= 15.39s mono=202.29s pack=crypto/x509
    stdlib_crypto_ecdsa.go           split= 10.76s mono=197.93s pack=crypto/ecdsa
    stdlib_crypto_hpke.go            split=  7.86s mono=196.70s pack=crypto/hpke

**The two differences are the two the driver-split job already recorded, with the same
numbers**: `gomaxprocs_memstats.go` prints `mallocs 105` monolithic against `120` split, and
`bytes_grow_stats.go` prints `16718922` against `18220422`. Both print absolute allocation
counters, which two different images are not obliged to agree on. Re-running splitdiff on
those two with **only the runtime-only pack** — the configuration section 16 shipped —
reproduces both deltas exactly, so they predate the standard-library packs and this branch
adds no new difference to the 358.

The pack closures, for the record:

| pack | packages loaded | object | globals defined |
| --- | ---: | ---: | ---: |
| runtime only | 29 | 7.8 MB | 7,732 |
| `crypto/ecdh` | 93 | 46.7 MB | 14,261 |
| `crypto/hpke` | 110 | 48.9 MB | 16,601 |
| `crypto/ecdsa` | 113 | 49.5 MB | 17,548 |
| `crypto/x509` | 140 | 51.1 MB | 20,355 |
| `net/smtp` | 161 | 53.5 MB | 23,124 |
| `net/http` | 181 | 68.1 MB | 36,547 |

## Still unverified

- **`make test-goc-coverage`** was not run, for the same reason the previous job gave: the
  coverage path compiles the runtime per program and so takes the monolithic path, and
  anything short of the whole corpus fails by construction. Nothing in this branch touches
  that path except the two codegen fixes, which the corpus and the matrix both cover.
- **The `getitab` fallback's cost was not measured.** It is on the cold path of an
  interface-to-interface type test — the inline chain still answers first — and the corpus and
  matrix run phases did not move outside their usual spread, but no benchmark was taken.
- **amd64 is untouched and unexercised.** `prebuilt.BuildRuntime` still refuses any target but
  arm64. The two codegen fixes are target-independent and covered by `make test-unit`.
- **Concurrent `goc build-runtime` for one key duplicates work.** Two shards starting at the
  same moment both miss, both build, and both write; the write is atomic so the result is
  correct, but the work is done twice. Not a lock, on purpose — a lock that outlives a killed
  build is worse than a duplicated build.
- **The pack cache is never evicted.** Seven packs are 345 MB and every distinct compiler
  binary gets its own set. Nothing prunes them.

## Notes on the briefing

- **The briefing's two named blockers were not the whole list**, and one of them turned out
  not to be a blocker at all. Stub dispatchers do not close the gap, because the pack also
  leaves behind its entire type region and its degraded itabs, and an itab is read by
  `runtime.itabsinit` before `main` — see Finding 3. Package init needed no work at all under
  the containment rule: `addModuleInitTasks` already skips a task the pack defines, and a pack
  package's dependencies are all in the pack, so the chain order is dependency order.
- **`RUNTIME_PLAN.md` still has no section 5.14** and no note about two independently-correct
  changes composing into a broken compiler; the previous job recorded the same gap. The plan's
  numbered subsections stop at 5.10. The advice was followed anyway — every claim here is
  differential against the host toolchain or against a matched control, not against a green
  suite.
- **`println` with several operands still omits the spaces the spec requires** (RUNTIME_PLAN
  5.10). It cost a test iteration; the test now prints one operand per call, as the existing
  split tests already did.

## For the integration of the three branches

Two things in here are not confined to the pack lever and the other two branches should know
about them:

1. **`interfaceTypeMatch` now calls `runtime.getitab`.** That is a codegen change on every
   interface-to-interface type assertion and interface case of a type switch. It is on the
   cold path, but it is new IR in a lot of functions.
2. **Function literals are named after the function they are written in.** Every closure's
   symbol changed, which changes every image's symbol table and the content hashes of the
   `.interfacecall` wrappers that name them. Anything comparing symbol names across the
   branches will see it. Two tests in `goc/compile_test.go` matched the old spelling.

Neither is a workaround: both fix a defect that makes the compiler emit the wrong code, and
both are covered by tests that fail without them.

## What changed

| file | why |
| --- | --- |
| `goc/compile.go` | name a function literal after the function it is written in; refuse a module whose functions collide on a linker symbol; guard the type hasher against re-emission; answer an interface type test with `runtime.getitab` when the inline chain misses; read the package closure once, where the loader is complete |
| `goc/runtime_root.go` | generate the pack's root from a package list instead of a fixed empty `main` |
| `goc/runtime_split.go` | carry several candidate manifests and choose among them by closure containment |
| `goc/source_import.go` | export `StdlibRoot`, which the cache key hashes |
| `internal/runtimepack/runtimepack.go` | manifest version 2: `Packages`, `Closure`, `UsableBy`, and a manifest-only read |
| `internal/prebuilt/prebuilt.go` | build a pack for a package list; compile against a set of manifests and report the choice |
| `cmd/goc/prebuilt.go`, `cmd/goc/main.go` | `-packages` on `build-runtime`, a comma-separated `-runtime` |
| `cmd/goc/packcache.go` | the content-addressed pack cache |
| `cmd/goc/runtime_status_test.go`, `cmd/goc/runtime_coverage_test.go` | build the seven packs concurrently; `-runtime-status-stdlib-packs=false` control; `-buildvcs=false` for the matrix compiler |
| `cmd/goc/packstdlib_test.go` | the selection, fallback, refusal, manifest and cache-key tests |
| `goc/function_literal_symbol_test.go` | the nistec regression test and the collision message |
| `analysis/splitdiff/main.go` | differential across a set of packs |
| `RUNTIME_PLAN.md` | section 18 |
