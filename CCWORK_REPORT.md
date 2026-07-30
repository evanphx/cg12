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

---

_This report is updated as results land._
