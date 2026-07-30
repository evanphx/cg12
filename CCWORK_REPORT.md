# Letting the prebuilt pack carry the standard library

Branch: `ccwork/pack-stdlib`, off `perf/test-suite`. The previous job's report has been moved
to `docs/report-matrix-speed.md` so this file is only about this job.

Status: **in progress.** Everything below was measured or reproduced on this box. Anything
not yet verified is named as such.

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

---

_This report is updated as results land._
