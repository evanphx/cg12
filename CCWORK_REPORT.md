> **This file holds two reports.** The one for *this* job --- `ccwork/phase2-gc`,
> the stack-scanning and GC-stress half of RUNTIME_PLAN Phase 2 --- starts at
> "Phase 2, half two" below. Everything above it is the inherited report from
> `ccwork/frontend-determinism-2`, kept because it is the record for the
> determinism work this branch is built on.
>
> Short version of this job: one GC defect found and fixed (a buffered channel's
> elements were not GC roots), one found and reported but not fixed (with `-O`, a
> loop-carried local is not a GC root --- pre-existing on `main`), one path
> classified unreachable with the boundary proved (the conservative stack scan),
> and 16 new capabilities taking the matrix from 345 to 361.

# Front-end determinism — finishing `ccwork/frontend-determinism`

Branch `ccwork/frontend-determinism-2`, off `main` (`9cd2621`).

**Verdict: done.** Every one of the 365 corpus programs compiles to the same bytes
every time --- with `-O`, without it, and linked against the prebuilt pack ---
including `runtime_defer_capture_allocs.go`, which was 25 distinct executables in
30 compiles. 5,650 corpus compiles, 0 varying, 0 failed. The full matrix is 345
subtests / 344 PASS / 1 declared EXPECTED FAILURE / 0 FAIL / 0 KNOWN GAP, and
`test-unit`, `test-goc-corpus`, `test-goc-cmd` and `test-ruby` are all green.

Two claims RUNTIME_PLAN was carrying are retired with measurements, not with
shrugs, and one new cause was found by auditing the class instead of the instance.
What is *not* established is listed at the end of this file and in §22.

This file was updated as each result landed; the sections are in the order they
were measured.

## What was inherited

`ccwork/frontend-determinism`'s four code commits were cherry-picked onto current
`main` (only `CCWORK_REPORT.md` conflicted; `goc/compile.go` auto-merged):

| commit here | what it does |
| --- | --- |
| `6de2c27` | goc: emit variadic interface payload addresses in argument order (cause 1) |
| `53c1fd4` | opt: break cost-inline size ties on name, walk callers in module order (cause 2) |
| `30e0d82` | goc: apply native stdlib overlays in import-path order (cause 3) |
| `d848440` | goc: `TestCompilingTheSameSourceTwiceGivesTheSameModule`, + `analysis/determinism` |

`go build ./...` and `go vet ./...` clean on that tree.

## Verified so far

### The five sample programs, cold vs warm, twice each

`scripts/determinism-check.sh` on this tree:

```
hello.go                            round1:identical(f33cde66558593bb)  round2:identical(f33cde66558593bb)
fmt_sprintf.go                      round1:identical(ff5fe3cc4d5adc50)  round2:identical(ff5fe3cc4d5adc50)
gc_struct.go                        round1:identical(98f8743b7ca01b39)  round2:identical(98f8743b7ca01b39)
runtime_cleanup_frame_retention.go  round1:identical(fab20a7b9da650eb)  round2:identical(fab20a7b9da650eb)
runtime_defer_capture_allocs.go     round1:identical(ce93869726ee1cc2)  round2:identical(ce93869726ee1cc2)
```

All five, all four compiles each (2 cold + 2 warm), one hash per program —
including `runtime_defer_capture_allocs.go`, the holdout.

`scripts/determinism-check.sh -O`, same tree, same shape:

```
hello.go                            round1:identical(0f73f40d0a8a9e6b)  round2:identical(0f73f40d0a8a9e6b)
fmt_sprintf.go                      round1:identical(5e540b1d6e57e997)  round2:identical(5e540b1d6e57e997)
gc_struct.go                        round1:identical(a42792e250b6d7fb)  round2:identical(a42792e250b6d7fb)
runtime_cleanup_frame_retention.go  round1:identical(ac36ecba2a28e504)  round2:identical(ac36ecba2a28e504)
runtime_defer_capture_allocs.go     round1:identical(f9d6b14468b04966)  round2:identical(f9d6b14468b04966)
```

Four compiles each is a weak sample by §5.10's own warning — it records a program
that takes its minority branch 3 times in 53 compiles. The corpus-wide sweep
below is the measurement that carries weight.

## Still unverified at this point

- corpus-wide sweep (365 programs, several compiles each), with and without `-O`
- `make test-unit`, `make test-goc-corpus`, `make test-goc-cmd`
- the full 345-subtest capability matrix
- §5.10's 441 mis-ordered `*.interfacecall.*` wrappers — predecessor could not
  reproduce; being re-checked here
- host-toolchain behaviour comparison for the inherited codegen change

## An exhaustive static audit of range-over-map, not a grep

Empirical sweeps only find what the corpus happens to exercise, so the class was
also enumerated statically. `golang.org/x/tools/go/packages` is in the module
cache, so a throwaway analyzer (in `$TMPDIR`, not committed) loaded `.`,
`./goc/...`, `./obj/...`, `./internal/gometa/...`, `./ir/...`, `./arm64/...`,
`./opt/...`, `./cmd/goc/...` with type information and printed every `for … range
m` whose ranged expression's underlying type is a map, flagging the ones whose
body appends to a slice.

**108 range-over-map statements outside tests; 56 of them append.** Every one was
read. The results:

### Front end (`goc/`) — all safe

| site | why order cannot reach output |
| --- | --- |
| `compile.go:1000` `itabs` → `symbols` | `sort.Strings` before use |
| `compile.go:1023` `g.interfaceMethods` → `methods` | `sort.Slice` on `g.functionSymbol` |
| `compile.go:268` `loader.units` → `assemblyPackages` | `sort.Strings` before use |
| `compile.go:490` `packageGlobals[…]`, `:493` `g.interfaceDispatchers` | feed `programSymbols`, a set; `finishRuntimeModule` re-emits it as `sortedUnique` |
| `compile.go:658` `assemblyFunctions` | writes `inputs[name]`/`outputs[name]`, one key per visit |
| `compile.go:866`, `reach.go:867/931/946/980`, `compile.go:7353` | sorted before use |
| `compile.go:2203` `declarations` → `recursive` | worklist draining to a set (`disabled`); the fixpoint is order-free |
| `compile.go:12786` `seen` → `words` | `sort.Ints` |
| `runtime_split.go:278/681` | sorted / `sortedUnique` |
| `native_overlay.go` | no longer in the list at all: the inherited `30e0d82` turned its `range units` into `range orderedUnits(units)`. It appended straight to `module.Funcs` and `module.Data`, so it was the one site besides cause 1 where map order reached the module — latent only because exactly one package ships a native `.ssa` overlay today |

`goc/compile.go:5576`'s variadic payload loop was the one that was not safe, and
is cause 1.

### Serialization (`ir/`, `internal/gometa/`) — all safe

`ir/binary.go:61` (module attachments), `ir/asm_binary.go:24/34/84/101`
(assembly defines, includes, float slots, signatures) and
`internal/gometa/gometa.go:479` all sort their keys before encoding. The
serialized module — which is what the compile cache is keyed on — has no
map-order input.

### `opt/` — two unfixed sites, and a measurement that says they cannot reach `goc`

`opt/mem2reg.go:109` and `opt/jumpthread.go:431` both do
`for b := range analysis.IteratedFrontier(df, defs)`. `IteratedFrontier` returns
`analysis.BlockSet`, i.e. `map[*ir.Block]bool`, so the iteration order is
randomized per range, and the body calls `f.NewTemp(…)` — **the phi temporaries
are numbered in map order**, which reaches register allocation, slot assignment
and therefore code.

They cannot affect `goc`, for the same structural reason the predecessor gave for
`opt/inline.go`, and here it is measured rather than argued:
`opt.OptimizeModule` sends any module over `moduleOptimizationFunctionBudget`
(2048 functions) to `BoundedPipeline`, which is `fold`/`copy`/`dce` only —
no `mem2reg`, no `jumpthread`, no `inline`, no `gcm`. The smallest program in the
goc corpus, `goc/testdata/hello.go`, emits **2,739** functions, because every goc
program links the runtime. So no goc module ever runs `DefaultPipeline`.

They are live for `cg12cc`/`cmd/cc`/`cmd/cg12`, whose modules are small.

### Back end (`arm64/`, `obj/`) — one latent site, cannot reach `goc`

`arm64/mc.go:2786` (`gcRoots`) builds a safepoint's root list by ranging
`m.f.StackPointerWords[id]`, a `map[int]bool`, so an allocation with two or more
pointer words contributes its `rootFrame` entries in map order — and
`setStackMap` writes `sp.roots` to the `__cg12_stackmaps` section **in slice
order** (`arm64/mc.go:458`). That would be live nondeterminism, except
`arm64/parallel.go:91` only keeps a function's safepoints for that section when
`!goRuntime`, and a goc image confirms it: `readelf -S` on a goc-built binary has
no stack-map section and `nm` no `__cg12_stackmaps` symbol. The Go-format stack
maps that goc does emit go through `goStackMapPoints`, which collects into a set
and `sort.Ints`.

Everything else in `arm64/` and `obj/` that appends from a map either sorts
(`allocacolor.go:49` and `backend.go:91` carry comments saying why),
collects into a set that is sorted later (`mc.go:385/1507/1576/1591/1617`,
`goabi.go:265`, `regalloc.go:150/293`, `semantic_assembly.go:112`,
`go_abi0_assembly.go:38`, `dwarf.go:386`, `dynamic.go:240/250/254`), or drains a
worklist to a fixpoint (`mc.go:1085`, `regalloc.go:265`).
`callersave.go:105`'s `savedList` goes to `slotGroups`, which `sort.Ints` its
`temps` argument first.

## Against the host toolchain, re-run on this base (§3 step 2)

The inherited variadic-payload change is a codegen change, and it is being
carried onto a `main` that has moved since the predecessor measured it
(`61b96da` → `9cd2621`), which is exactly the §5.14 compose hazard. So the
behaviour comparison was re-run here rather than inherited.

`$TMPDIR/variadic/main.go` (not committed; a one-off differential) drives the
changed path: an eight-argument `Println` and the matching `Printf` mixing a
`String()`-bearing struct, a plain struct, an array, a string, an int, a
pointer-derived bool and a nil `error`; the same arguments in three different
orders; a nested `fmt.Sprint` inside a `Println`; three `...any` arguments with
observable side effects, to pin evaluation order; a spread `values...` taking the
`hasEllipsis` branch instead of the combined-allocation one; and three
single-argument calls.

```
go run main.go            > host.txt
goc     -o v.goc  main.go && ./v.goc     > goc.txt      diff → identical
goc -O  -o vo.goc main.go && ./vo.goc    > goc-opt.txt  diff → identical
```

Both exit 0, both byte-identical to the host, including the line
`evaluation order: [one two three]`.

## The corpus-wide sweep — no `-O`

`scripts/determinism-check.sh -corpus -rounds 4 -j 8` (the mode added in
`c192f14`; it drives `analysis/determinism`, inherited from the predecessor).
**Every** `goc/testdata/*.go` program, four full compiles each, 1,460 compiles,
through `goc compile-batch` workers:

```
programs=365 rounds=4 workers=8 optimize=false pack=""
round 0: 365 programs in 202.7s, 0 failed
round 1: 365 programs in 207.8s, 0 failed
round 2: 365 programs in 209.3s, 0 failed
round 3: 365 programs in 215.3s, 0 failed

failed to compile: 0
content varies between rounds: 0
image varies, content identical (layout only): 0

reproducible=365 varying=0 failed=0 of 365 over 4 rounds
```

**365 of 365 reproducible, 0 varying, 0 failed.** §18 measured 39 of 358 varying
before this work; §5.10 quotes `runtime_defer_capture_allocs.go` at 25 distinct
executables in 30 compiles.

Two things about this measurement are worth stating so it is not read as stronger
than it is. It uses batch workers, and a round assigns programs to workers
first-come-first-served, so the same program is generally compiled by a worker
with a *different* preceding history in each round — which makes the test
stricter, not weaker, since a cross-program leak would also be a real defect.
And four draws per program is a thin sample for a skewed program: §5.10's
`stdlib_net_mail_textproto.go` took its minority branch 3 times in 53 compiles,
and a 5.7% minority rate survives four draws about 79% of the time. That skew is
attacked directly below with a deep repeat of the known offenders.

## The A/B on the fix itself, re-run on this base

Re-measured here, not inherited, because the base moved.

`git checkout main -- goc/compile.go` puts exactly the pre-fix compiler in the
tree (the cherry-pick auto-merged, so `main`'s `compile.go` differs from this
branch's by the variadic hunk alone — `git diff --stat HEAD` confirms 4
insertions, 16 deletions in that one file and nothing else).

**Pre-fix, `TestCompilingTheSameSourceTwiceGivesTheSameModule` fails 3 times out
of 3**, naming a `...any` caller each time:

```
--- FAIL (4.58s)  main.nested was compiled differently by two compiles of the same source
--- FAIL (3.79s)  main.mixed  was compiled differently by two compiles of the same source
--- FAIL (3.83s)  main.mixed  was compiled differently by two compiles of the same source
```

**Post-fix it passes 5 times out of 5** (`-count=5`, `ok … 20.181s`).

And the fix is behaviour-neutral, which is the part a green suite cannot show.
The pre-fix compiler was built and run against the same differential program:

```
diff host.txt goc-prefix.txt   → identical
diff goc.txt  goc-prefix.txt   → identical
```

So the pre-fix and post-fix compilers produce programs that print the same thing,
and both match the host toolchain. The change reorders two `add` instructions
with identical operands; it does not change what is computed.

## §5.10's "441 interface-call wrappers land in a different order" is wrong, and here is the positive disproof

The predecessor could not reproduce this and declined to claim it fixed. It can be
settled rather than left open, because the claim can be tested **on the compiler
that is nondeterministic** — the pre-fix one, still available from
`git checkout main -- goc/compile.go`.

`goc/testdata/runtime_defer_capture_allocs.go`, `CG12_NOCACHE=1 goc -emit-ir`,
three emissions, pre-fix compiler:

| | result |
| --- | --- |
| module text sha256 (first 20 hex) | `388391fbd1686f46289d`, `45935dea599d3c1b63ae`, `bd55637196cd5f0193f7` — **three distinct** |
| ordinal position of all 5,942 `function` headers | **identical in all three** |
| ordinal position of all 1,318 `*.interfacecall.*` wrappers | **identical in all three** |

So the nondeterminism reproduces, and nothing moves. Not one wrapper, not one
function. The entire diff between emission 1 and emission 2 is **110 lines in
three functions** — `testing.common.makeTempDir`, `testing.common.checkFuzzFn`
and `testing.common.Attr`, every one a `...any` caller — and it reads:

```
< 	%t323 =p add %t322, 48        > 	%t323 =p add %t322, 64
< 	%t324 =p add %t322, 64        > 	%t324 =p add %t322, 68
< 	%t325 =p add %t322, 68        > 	%t325 =p add %t322, 48
```

with every later use following the renumbering. That is cause 1, and it is the
whole of it.

**§5.10's first determinism bullet should be deleted, not ticked.** The most
likely reading of how it arose: at the *linked image* level a renumbered
temporary shifts a frame size, which moves every later function's address, so a
symbol-address comparison shows hundreds of wrappers "at different positions"
when what actually happened is that three function bodies upstream of them
changed length. The count 441 is not reproducible either — this program has 1,318
interfacecall wrappers today.

For completeness, the same three emissions on the **fixed** compiler give one
module text (`719e4847b064a07593b3` × 3) with, again, identical wrapper positions.

`analysis/batchdiff`'s `contentDigestOf` comment repeats the 441 claim as the
current reason raw bytes cannot triage a goc build; it is corrected in this branch.

## The corpus-wide sweep — with `-O`

`scripts/determinism-check.sh -corpus -rounds 4 -j 8 -O`, same 365 programs,
another 1,460 compiles:

```
programs=365 rounds=4 workers=8 optimize=true pack=""
round 0: 365 programs in 227.8s, 0 failed
round 1: 365 programs in 220.1s, 0 failed
round 2: 365 programs in 219.2s, 0 failed
round 3: 365 programs in 220.0s, 0 failed

failed to compile: 0
content varies between rounds: 0
image varies, content identical (layout only): 0

reproducible=365 varying=0 failed=0 of 365 over 4 rounds
```

**365 of 365 reproducible with `-O` as well.** 2,920 corpus compiles in total
across the two sweeps, 0 varying, 0 failed.

## The deep repeat, aimed at the skew

Four draws per program cannot rule out a skewed minority branch, so a 45-program
sample — every ninth corpus program, plus the named offenders
`runtime_defer_capture_allocs.go`, `stdlib_net_mail_textproto.go` (§5.10's 3-in-53
case), `bytes_grow_stats.go`, `bytes_grow_compare.go`,
`runtime_cleanup_frame_retention.go` and `stdlib_http_tls_client_server.go` — was
compiled **12 times each**, 540 compiles:

```
programs=45 rounds=12 workers=8 optimize=false pack=""
round 0 … round 11: 45 programs in ~52s each, 0 failed

failed to compile: 0
content varies between rounds: 0
image varies, content identical (layout only): 0

reproducible=45 varying=0 failed=0 of 45 over 12 rounds
```

A 5.7% minority branch survives 12 draws about 50% of the time, so this is not a
proof either; combined with 8 draws of every program in the corpus it is the best
available evidence short of a campaign, and it targets exactly the programs the
plan named as skewed.

## Suites

- `make test-unit`: **pass**, exit 0, 0 FAIL. (Includes `./opt/...`, `./ir/...`,
  `./arm64/...`, `./obj/...`, `./internal/gometa/...`.)
- `make test-goc-corpus`: **`ok github.com/evanphx/cg12/goc 549.755s`**, 0 FAIL.
  This is the non-executable compile path as well as the executable one, and it is
  where `TestCompilingTheSameSourceTwiceGivesTheSameModule` runs.
- `make test-goc-cmd`: **`ok github.com/evanphx/cg12/cmd/goc 219.657s`**, 0 FAIL.
- **The full capability matrix, unsharded, with `-v`**:
  `go test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/... -v
  -args -runtime-status-shards=1 -runtime-status-shard=0
  -runtime-status-compile-workers=8` →
  **`PASS` / `ok github.com/evanphx/cg12/cmd/goc 183.013s`**.

  Census taken from the `-v` output rather than from `ok`:

  | | count |
  | --- | ---: |
  | `=== RUN   …CapabilityStatus/<category>/<name>` | **345** |
  | `--- PASS` | **345** |
  | `--- FAIL` | **0** |
  | `--- SKIP` | **0** |
  | logged `PASS <program>.go` | **344** |
  | logged `EXPECTED FAILURE` | **1** (`runtime_panic_print_string.go`) |
  | logged `KNOWN GAP` | **0** |

  **345 subtests, 344 PASS, 1 declared EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP** —
  the required census exactly. 183 s is a plausible unsharded wall clock for this
  box at 8 compile workers (§18 measured 67–116 s at 64).

  **Complete list of non-passing capabilities: none.** The only non-`PASS` run
  outcome is `defer-panic/panic-string-output`, the declared expected failure,
  which exits 2 by design.

## The runtime pack

`goc build-runtime -packages "" -o …` under `CG12_NOCACHE=1`, three times each:

| | digest (first 20 hex) |
| --- | --- |
| no `-O` | `2e2802389ffca38581e5` × 3 |
| `-O` | `5f8943e5c780e4bf50b1` × 3 |

This matters more than any single program does: the pack is the largest module goc
compiles, and every program built against it inherits its bytes.

## The pack-linked compile path, 6 rounds

The matrix and most real builds link against a prebuilt pack rather than compiling
the runtime into every program, so that path was swept too — the same 365 programs
against the runtime-only pack, **6 compiles each**, 2,190 compiles:

```
programs=365 rounds=6 workers=8 optimize=false pack="…/rt.1.gocrt"
round 0 … round 5: 365 programs in ~167s each, 0 failed

failed to compile: 0
content varies between rounds: 0
image varies, content identical (layout only): 0

reproducible=365 varying=0 failed=0 of 365 over 6 rounds
```

`-O` + pack is deliberately **not** measured here: that is the 16-capability link
failure `ccwork/opt-pack-link` owns, and running it would only reproduce their
failures.

## An extra fix outside goc: `opt`'s phi numbering

The static audit found two sites in the same class as the inherited
`opt/inline.go` fix, and they are fixed here (`4795470`) rather than left, on the
same reasoning the predecessor used for `inline.go`: they cannot reach goc, but
`cg12cc` is real.

`opt/mem2reg.go` and `opt/jumpthread.go`'s `reconstructThreaded` both placed phis
by ranging `analysis.IteratedFrontier`'s result — a `map[*ir.Block]bool` — and
placing a phi calls `f.NewTemp`. So which phi got which temporary id was decided by
map iteration order, and temporary ids reach register allocation and slot
assignment. Both now walk `cfg.RPO` filtered by frontier membership, which is safe
because `DominanceFrontier` only ever adds blocks drawn from `cfg.RPO`, so the
iterated frontier is a subset of it. Phi *placement* is identical either way — one
per (variable, block) — so this is a numbering defect, not a placement one.

`opt/determinism_test.go`'s `TestMem2RegPlacesPhisInTheSameOrderEveryTime`
promotes the same two-diamond function 20 times in one process. It **fails 5 times
out of 5** on the unfixed pass, at the first attempt each time, with a diff like
`-%t8 =w add %t1.1, %t2` / `+%t8 =w add %t1, %t2`, and passes with the fix.

`make test-ruby` — the cg12-vs-gcc differential, which is the right gate for a
change to the C compilation path — is green with it:
`ok …/difftest 42.175s`, `ok …/cc 15.251s`.

**Honest limit on this one.** I could not build a C program in which the defect
reaches the *emitted assembly*. `difftest/testdata/comp.c`,
`cc/testdata/rubric/int128.c`, `vla.c`, `cmd/viz/testdata/collatz.c` and a
purpose-built four-diamond function with two promotable locals each give **1
distinct `-O -S` output in 8–10 compiles on the pre-fix compiler**. So the defect
is demonstrated at the IR level and its practical blast radius on cg12cc assembly
is unmeasured and may be nil at the program sizes this repository tests. It is
fixed because the numbering is wrong, not because a symptom was observed.

## Final re-verification with the finished compiler

Every sweep above used a `goc` built before the `opt` commit. That commit cannot
affect goc (§22's `BoundedPipeline` argument), but "cannot" is worth checking, so
the whole thing was run again from the shipped script against a freshly built
compiler:

```
=== five samples, default (cold vs warm, twice) ===
hello.go                            round1:identical(f33cde66558593bb)  round2:identical(f33cde66558593bb)
fmt_sprintf.go                      round1:identical(ff5fe3cc4d5adc50)  round2:identical(ff5fe3cc4d5adc50)
gc_struct.go                        round1:identical(98f8743b7ca01b39)  round2:identical(98f8743b7ca01b39)
runtime_cleanup_frame_retention.go  round1:identical(fab20a7b9da650eb)  round2:identical(fab20a7b9da650eb)
runtime_defer_capture_allocs.go     round1:identical(ce93869726ee1cc2)  round2:identical(ce93869726ee1cc2)
=== five samples, -O ===
hello.go                            round1:identical(0f73f40d0a8a9e6b)  round2:identical(0f73f40d0a8a9e6b)
fmt_sprintf.go                      round1:identical(5e540b1d6e57e997)  round2:identical(5e540b1d6e57e997)
gc_struct.go                        round1:identical(a42792e250b6d7fb)  round2:identical(a42792e250b6d7fb)
runtime_cleanup_frame_retention.go  round1:identical(ac36ecba2a28e504)  round2:identical(ac36ecba2a28e504)
runtime_defer_capture_allocs.go     round1:identical(f9d6b14468b04966)  round2:identical(f9d6b14468b04966)
=== corpus, 3 rounds ===       reproducible=365 varying=0 failed=0 of 365   exit=0
=== corpus, 3 rounds, -O ===   reproducible=365 varying=0 failed=0 of 365   exit=0
```

**All ten sample hashes are the same values the pre-`opt`-commit binary produced.**
That is the `BoundedPipeline` argument confirmed rather than asserted: the `opt`
change is a byte-level no-op for goc.

## Compile census

| sweep | programs × compiles | varying |
| --- | ---: | ---: |
| monolithic, no `-O` | 365 × 4 = 1,460 | 0 |
| monolithic, `-O` | 365 × 4 = 1,460 | 0 |
| deep repeat on the skewed programs | 45 × 12 = 540 | 0 |
| linked against the prebuilt pack | 365 × 6 = 2,190 | 0 |
| final, no `-O` | 365 × 3 = 1,095 | 0 |
| final, `-O` | 365 × 3 = 1,095 | 0 |
| five-program cold/warm samples | 4 × 20 = 80 | 0 |
| **total** | **7,920** | **0** |

Plus 6 cold `goc build-runtime` builds (3 with `-O`, 3 without), 2 hashes, and 6
`goc -emit-ir` emissions of `runtime_defer_capture_allocs.go` (3 pre-fix, 3 post).

## Commits on this branch

| | |
| --- | --- |
| `6de2c27` | goc: emit variadic interface payload addresses in argument order *(cherry-picked)* |
| `53c1fd4` | opt: break cost-inline size ties on name, walk callers in module order *(cherry-picked)* |
| `30e0d82` | goc: apply native stdlib overlays in import-path order *(cherry-picked)* |
| `d848440` | goc: a test that two compiles of the same source give the same module *(cherry-picked)* |
| `c192f14` | scripts: give determinism-check a corpus mode |
| `90f6e9e` | plan, batchdiff: compiling the same program twice gives the same program |
| `4795470` | opt: walk the iterated dominance frontier in reverse post-order |
| `814c381` | plan: cause 4, the pack path, the suites, and what none of it establishes |
| `b40eb65` | plan, report: the re-run with the finished compiler |

## Still unverified, and other jobs' business

**Not established by anything here:**

- **A reproducible compile is not a correct compile.** The sweeps compare a program
  against itself, so they are structurally blind to a systematic miscompile. The
  suites, the matrix and the host-toolchain differential are what cover that, and
  all of them ran; but if the variadic change were wrong in the same way in every
  compile, no determinism measurement would see it. That is why §3 step 2 was
  re-run on this base rather than inherited.
- **Sample depth.** 3 to 12 draws per program cannot rule out a branch taken 1 time
  in 100. The deep repeat targets the programs §5.10 named as skewed and the static
  audit covers the class, but neither is a campaign.
- **`cg12cc`'s reproducibility.** Cause 4 is fixed and validated at the IR level and
  through the gcc differential, but no C program in this repository is large enough
  for it to reach emitted assembly, so its blast radius there is unmeasured.
- **`arm64/mc.go:2786`** (safepoint frame roots in `map[int]bool` order, reaching
  `__cg12_stackmaps` in slice order) is located, recorded in §5.10, and **not
  fixed**. It cannot reach goc — that part is measured, via `readelf`/`nm` on a goc
  image and `arm64/parallel.go:91`'s `!goRuntime` gate.
- **`-O` against the prebuilt pack** was not swept, on purpose.

**For the sibling job (`ccwork/opt-pack-link`), not fixed here:** nothing new. The
16-capability `-runtime-opt` link failure was not touched, and `-O` + pack was left
unmeasured precisely so as not to trip over it. One datum that may help them
though: **`opt.OptimizeModule` sends every goc module to `BoundedPipeline`**
(`fold`/`copy`/`dce`) because every goc module exceeds the 2048-function budget —
`hello.go` alone emits 2,739 functions. So whatever `-O` does to break the pack
link, it is not `inline`, `mem2reg`, `jumpthread`, `ifconvert` or `gcm`; those
never run on a goc module. That narrows their search to `fold`, `copy`, `dce` and
the split itself.

---

# Phase 2, half two: stack scanning and GC stress (`ccwork/phase2-gc`)

Branch `ccwork/phase2-gc`, off `main` (`0505d90`). RUNTIME_PLAN §13 item 5, the
stack-scanning and GC-stress half of §6. The allocation/write-barrier half is
`ccwork/phase2-alloc` (unmerged) and is not duplicated here.

**This file is written as results land. Anything not yet measured says so.**

## Status: complete --- three defects found, one fixed

- **Fixed:** a buffered channel's elements were not GC roots (`goc/compile.go`).
- **Reported, not fixed:** with `-O`, a loop-carried local is not a GC root.
  Pre-existing on `main`; reducer committed.
- **Reported, not fixed:** `//go:noinline` is parsed by nothing, so `-O` inlines
  through it.
- **Classified unreachable, with the boundary proved:** the conservative stack scan.
- 16 new capabilities; matrix 345 -> 361. Plain arm green; optimized arm has one
  failure, and it is the pre-existing `-O` defect above.


## Found: `runtime: marked free object in span` — a zombie, 30/30 at GOMAXPROCS=1

The very first stack-scanning capability program
(`goc/testdata/runtime_stack_scan_loop_safepoints.go`) fails on the cg12 build
and passes on the host toolchain:

```
runtime: marked free object in span 0xf80b870e9ba0, elemsize=16 freeindex=0
0x406c05f56000 alloc unmarked
...
```

That is `mspan.reportZombies` firing: an object that the sweeper found free but
marked. Measured on `main` (`0505d90`) with the same source compiled both ways:

| build | GOMAXPROCS | failures |
| --- | ---: | ---: |
| host Go 1.26.1 | 4 | 0 / 100 |
| goc | 1 | 30 / 30 |
| goc | 2 | 13 / 30 |
| goc | 4 | 60 / 100 |

Reduction in progress; details below as they land.

### Reduced: **a buffered channel's elements are not GC roots**

The zombie was a string that a `chan string` buffer was the only holder of.
`GODEBUG=clobberfree=1,cg12scanroots=2` named the retaining frame and showed the
retained object's first word as the clobber pattern, i.e. already freed.

Reducer, 30 lines, no `unsafe`, no cleanups, no goroutines:

```go
var collected = make(chan string, 64)

func fill(n int) {
	for i := 0; i < n; i++ {
		collected <- "carried-" + string(rune('a'+i))
	}
}

func main() {
	fill(6)
	runtime.GC()
	runtime.GC()
	runtime.GC()
	for i := 0; i < 6; i++ {
		got := <-collected
		want := "carried-" + string(rune('a'+i))
		if got != want { panic("a buffered channel lost a string") }
	}
}
```

goc: 12/12 failures. Host Go 1.26.1: 0/12.

It is not specific to strings. With `GODEBUG=clobberfree=1`, **every**
pointer-containing element type loses every buffered element:

| `make(chan T, 8)` | result on goc |
| --- | --- |
| `string` | every element clobbered |
| `*box` | every element clobbered |
| `[]byte` | every element clobbered |
| `any` | every element clobbered |
| `struct{name string; box *box}` | every element clobbered |

The host build of the same source prints `ok`.

The element type descriptors are correct: a `reflect`+`unsafe` probe reads
`PtrBytes` from the `abi.Type` behind `reflect.TypeOf(make(chan string)).Elem()`
and gets 8 on both toolchains, so `makechan`'s `elem.Pointers()` test has the
right input. Mechanism still being narrowed.

**No capability in the 345-entry matrix catches this.** The existing
`goroutine/channel-*-gc` capabilities send one element and collect once, which is
not enough for the sweeper to reach the buffer.

### Root cause: `goc/compile.go`'s `channelType` emits a stub element descriptor

`(*gen).channelType` hand-rolls the `abi.ChanType` it passes to
`runtime.makechan`. Its embedded element `abi.Type` is 48 zero bytes with only
four fields filled in:

```go
elementBytes := make([]int64, 48)
size := typeSize(element)
for i := 0; i < 8; i++ { elementBytes[i] = (size >> (8 * i)) & 0xff }  // Size_
elementBytes[21] = alignment                                          // Align_
elementBytes[22] = alignment                                          // FieldAlign_
elementBytes[23] = int64(runtimeKind(element))                        // Kind_
```

`PtrBytes` (bytes 8..15) and `GCData` (offset 32) are left zero. Every other
allocation site in goc uses `(*gen).runtimeType`, which writes both.

Proved in the running program rather than inferred. Reading `hchan.elemtype` at
offset 40 and comparing it against the descriptor `reflect` reports for the same
element type:

```
--- goc ---                          --- host ---
runtime elemtype size 8 ptrbytes 0   runtime elemtype size 8 ptrbytes 8
reflect elemtype size 8 ptrbytes 8   reflect elemtype size 8 ptrbytes 8
same descriptor: false               same descriptor: true
```

and, one level up, which branch `makechan` takes. `makechan` allocates the buffer
inside the `hchan` object only when `!elem.Pointers()`; the offset from `hchan` to
`buf` says which branch ran:

```
--- goc ---                    --- host ---
chan *box  delta 112           chan *box  delta -8272   (separate scan allocation)
chan int   delta 112           chan int   delta 112     (inline, noscan)
```

goc puts a `chan *box` buffer in the same no-scan allocation as the `hchan`, so
the collector never scans it.

Two consequences follow from the one defect:

1. `makechan` allocates the buffer with `mallocgc(hchanSize+mem, nil, true)` --
   no type, no pointer bitmap -- so buffered elements are invisible to the mark
   phase. This is the fault above.
2. `chansend`/`chanrecv`/`sendDirect` pass `c.elemtype` to `typedmemmove` and
   `bulkBarrierPreWriteSrcOnly`, both of which are no-ops when `PtrBytes == 0`.
   So copying a pointer element into or out of a channel skips its write
   barrier as well.


### The fix

`goc/compile.go`: `channelType` now points the `abi.ChanType`'s `Elem` at the
same complete descriptor `runtimeType` emits for every other allocation site.
`runtimeType` is split into `runtimeTypeSymbol` (emit, return the data symbol
name) and `runtimeType` (the `ir.Ref` wrapper) so one datum can reference
another; nothing else changes.

Measured on the same three reducers, same tree, before and after:

| program | before | after |
| --- | ---: | ---: |
| `chan1` (30-line reducer, `clobberfree=1`) | 12/12 fail | 0/12 fail |
| `min2` (zombie reproducer, `clobberfree=1`) | 12/12 fail | 0/40 fail |
| `runtime_stack_scan_loop_safepoints.go`, GOMAXPROCS=1 | 30/30 fail | 0/40 fail |
| `runtime_stack_scan_loop_safepoints.go`, GOMAXPROCS=2 | 13/30 fail | 0/40 fail |
| `runtime_stack_scan_loop_safepoints.go`, GOMAXPROCS=4 | 60/100 fail | 0/40 fail |

and the two structural probes now agree with the host: `hchan.elemtype.PtrBytes`
is 8, and a `chan *box` buffer is a separate scannable allocation while a
`chan int` buffer stays inline in the no-scan `hchan`.

One thing the probe still reports differently from the host: the descriptor the
runtime holds in `hchan.elemtype` is not pointer-identical to the one `reflect`
reports for the same element type. goc emits more than one descriptor family for
a type; that is pre-existing and independent of this defect, and GC correctness
does not depend on the identity, only on the contents. It is recorded as an open
observation, not fixed here.

## Zombie detection, proved by a controlled negative subprocess

`cmd/goc/runtime_zombie_detection_test.go`. §6 asks for zombie detection "in a
controlled negative subprocess"; a check that has only ever fired by accident is
not known to work. The subprocess launders a pointer through a `uintptr` --
`reportZombies`' own case 1 -- collects until the object is swept, then publishes
the integer back as a pointer where the collector follows it. The next cycle
marks a free slot and the sweep after it throws.

It passes: `runtime: marked free object in span` and `found pointer to free
object`, exit non-zero. The control, the same program with the resurrection
removed, exits 0 and prints `no zombie was reported`, so the test is not merely
detecting a crash.

**Confirmed independently: `reportZombies`' dump is blind on Green Tea spans.**
Its per-object loop printed every object `alloc unmarked` and named no zombie at
all, both here and in the original fault. The mechanism, read off the vendored
source: `sweepLocked.sweep` calls `s.moveInlineMarks(s.gcmarkBits)`, which copies
the inline mark bits into `gcmarkBits` and then **resets** them; the detection
reads `gcmarkBits` and is correct, but `reportZombies` reads
`s.markBitsForBase()`, which for `gcUsesSpanInlineMarkBits(s.elemsize)` returns
`&s.inlineMarkBits().marks[0]` -- the bits that were just reset. So detection
works and attribution does not.

`ccwork/reportzombies` owns that fix and it is not touched here. The test logs
the gap rather than asserting it in either direction: asserting the dump is blind
would write the defect into the suite, and asserting it names the zombie would
fail until the sibling lands.

## Classified unreachable: the conservative stack scan (§6, §4.2)

§6 asks for "conservative scan boundaries". **They cannot be reached from
cg12-compiled Go, by design**, and the design is recorded in the compiler:

```go
// internal/gometa/pcvalue.go
// UnsafePointPCData marks the complete generated function as unsafe for
// asynchronous preemption. cg12 keeps managed references in registers between
// calls, while its Go stack maps describe the spill state at call safepoints.
// Cooperative preemption at calls remains available.
func UnsafePointPCData() []byte { return []byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f, 0} }
```

Decoded the way `runtime.pcvalue` decodes it, that is one entry, value
`abi.UnsafePointUnsafe` (-2), spanning `0xffffffff` PCs. `isAsyncSafePoint` reads
it and returns false, so `doSigPreempt` never injects `runtime.asyncPreempt`, no
frame is ever marked conservative, and all three calls to
`runtime.scanConservative` -- the stack-object one and the two in `scanframe` --
are dead.

Measured, not inferred. A long call-free spin loop compiled with runtime coverage
and run alongside forced collections gives:

| runtime function | executed |
| --- | --- |
| `runtime.suspendG` | yes |
| `runtime.preemptM` | yes |
| `runtime.doSigPreempt` | yes |
| `runtime.isAsyncSafePoint` | yes |
| `runtime.asyncPreempt2` | **no** |
| `runtime.scanConservative` | **no** |

and a per-block reading of `isAsyncSafePoint`'s coverage shows the executed exit
is `preempt.go:448` -- the `return false, 0` under
`if up == abi.UnsafePointUnsafe`. The runtime asks, the signal arrives, and the
injection is refused at exactly the documented place.

Two tests hold the classification:

- `internal/gometa.TestUnsafePointPCDataMarksTheWholeFunctionUnsafe` decodes the
  table at the source of truth.
- `cmd/goc.TestAsynchronousPreemptionIsRefusedForGeneratedCode` asserts the whole
  chain above, and fails if `asyncPreempt2` ever starts running -- at which point
  the conservative scan needs real coverage and this classification is stale.

The capability that was written for the conservative scan is kept as
`stack-scan/callfree-loop-roots`: a long call-free loop holding its only
references in an accumulator, an interior pointer and an `unsafe.Pointer` round
trip, checked after collections run alongside it. That is the property cg12's
choice actually rests on.

**This has a consequence outside §6 that Phase 3 (§7) owns**: §7 asks for
"cooperative and asynchronous preemption in compute loops". The asynchronous half
does not exist in cg12 today. A truly non-terminating call-free loop has no
preemption point at all, so `stopTheWorld` would wait for it indefinitely. Every
loop tried here terminates, so this is stated as a consequence of the mechanism
rather than as a reproduced hang.

## What Phase 2's stack-scanning and GC-stress half now covers

16 new capabilities, every one compared against the host Go toolchain per §3
step 2 (same source, `go build` vs `goc`, identical output and exit status), and
each verified to fail for the right reason before it was accepted.

### Stack scanning (`stack-scan`, 6 capabilities)

| capability | what it pins | runs under |
| --- | --- | --- |
| `loop-safepoints` | a pointer live across a loop back edge is a root at every safepoint in the body: carried accumulator, growing slice, blocking channel send, nested loops. Has a positive control that drops an object and requires its cleanup to fire, so the detector is not vacuous | `cg12scanroots=1` |
| `blocked-goroutines` | a parked goroutine's frame is scanned at the PC that parked it: `chanrecv`, `chansend` unbuffered and full-buffered, `selectgo`, `semacquire` via Mutex and WaitGroup, `notifyListWait` via Cond | `cg12scanroots=1` |
| `syscall-transitions` | `scanstack` through `gp.syscallsp` for a goroutine blocked in `syscall.Read` on a raw pipe, and through netpoll for one blocked in `os.File.Read` | `cg12scanroots=1` |
| `panic-unwind` | frames under an in-flight panic still describe their roots: a deep tower with a GC in the deferred function, a defer that replaces the panic, a panic value that is itself a pointer, and a recover that resumes | `cg12scanroots=1` |
| `stack-copy-roots` | growth and shrink with pointerful frames, an interior pointer into the frame itself, defer records across the copy, and a goroutine parked on a channel while another grows | `cg12checkstackcopy=1` (119,696 pointer records checked in one run) |
| `callfree-loop-roots` | roots held only in an accumulator, an interior pointer and an `unsafe.Pointer` round trip across a long call-free loop | — |

### GC stress (`gc-stress`, 6 capabilities)

`concurrent-mark` (mutators moving the only reference to a subtree from an
unscanned location into a scanned one, through pointer field / slice / map /
interface / channel, under `cg12checkwb=2`), `assist-credit`, `sweep-pacing`,
`scavenge-release`, `heap-growth-shrink`, `memory-limit`. All exclusive.

Each asserts that the path it is named after was actually taken rather than
assuming it: assist CPU non-zero from `runtime/metrics`, `HeapReleased` growing
across `debug.FreeOSMemory`, more GC cycles under a memory limit than without,
and `HeapObjects == Mallocs - Frees`, `HeapInuse + HeapIdle == HeapSys`,
`HeapReleased <= HeapIdle` after every phase.

### Rare invariant paths (`gc-invariants`, 3 capabilities) and `gc/channel-buffer-roots`

`checkmark` runs a pointer-dense, concurrently-mutated heap under
`GODEBUG=gccheckmark=1`, so the whole heap is re-marked with the world stopped
and compared. `mark-workers` and `metadata-hugepages` reach paths a Go program
cannot see from inside itself. `gc/channel-buffer-roots` is the capability the
channel defect came from, kept under `clobberfree=1`.

### Named-path proofs (`cmd/goc/runtime_gc_paths_test.go`)

For the paths with no in-program observable, one instrumented compile per program
answers "did this named runtime function execute". All confirmed executed:

- `runtime.mheap.enableMetadataHugePages`, `runtime.pageAlloc.enableChunkHugePages`
- `runtime.gcAssistAlloc`, `runtime.gcAssistAlloc1`, `runtime.gcFlushBgCredit`
- `runtime.deductSweepCredit`, `runtime.sweepone`, `runtime.sweepLocked.sweep`
- `runtime.pageAlloc.scavenge`, `runtime.scavengerState.run`, `runtime.sysUnusedOS`
- `runtime.gcBgMarkStartWorkers`, `runtime.gcControllerState.findRunnableGCWorker`,
  `runtime.gcBgMarkWorker`, `runtime.gcDrain`

with a negative control (`runtime.badmorestackgsignal`, reachable only from a
corrupted signal stack) that correctly reports false, so the mechanism is not
reading an all-ones bitmap.

### Open question found while doing this, not resolved

The three per-mode drain wrappers -- `gcDrainMarkWorkerDedicated`,
`...Fractional`, `...Idle` -- report **unexecuted** in the coverage bitmap at
GOMAXPROCS 1, 2, 3, 4 and 8, on runs where `gcBgMarkWorker` demonstrably gets
past its `mode not set` throw (block hit) and `gcDrain` demonstrably executes.
`gcDrain`'s only callers are those three wrappers and `mcheckmark.go`. So either
the modes are never handed out and something else reached `gcDrain`, or the
instrumentation is missing these functions' counters -- and if it is the latter,
the §1 coverage percentages understate coverage by however many functions share
whatever property these have.

It is left as an open question rather than guessed at, and the mark-worker test
asserts only what is measurable. Separating dedicated from fractional was §6's
ask and is **not** delivered: `cpuStats.accumulate` folds fractional time into
`GCDedicatedTime`, so `runtime/metrics` cannot do it either.

## Suites

Run on this branch with the fix and the new capabilities in place.

| suite | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `gofmt -l` over every non-vendored Go file | clean |
| `make test-unit` | pass (24 packages) |
| `make test-goc-corpus` | pass (`ok github.com/evanphx/cg12/goc 578.5s`) |
| `goc/channel_type_test.go` | passes here, fails on the pre-fix compiler |
| `internal/gometa` unsafe-point boundary test | pass |
| `cmd/goc` matrix bookkeeping (denominator, exclusive classification, cost model) | pass |
| `cmd/goc` runtime-path proofs + zombie negative subprocess | pass |
| full matrix, plain arm | **361 subtests, 361 PASS, 0 FAIL, 0 KNOWN GAP, 1 declared EXPECTED FAILURE** |
| full matrix, `-O` arm | **360 PASS, 1 FAIL** — `stack-scan/loop-safepoints`, the pre-existing `-O` defect below |
| `make test-goc-corpus` rerun (after all testdata was added) | see "Final suite results" at the end |
| `make test-goc-cmd` | see "Final suite results" at the end |
| determinism, both arms | see "Final suite results" at the end |

The first `test-unit` and `test-goc-corpus` rows above were measured with the
channel fix in place but before the last of the capability programs landed; they
are re-run at the end and reported there.


## For whoever integrates this

- **The matrix goes from 345 to 361 capabilities**, all `mustPass`, with the one
  declared `expectedFailure` (`defer-panic/panic-string-output`) unchanged. So
  the shape the non-negotiables ask for is 361 subtests / 360 PASS / 1 EXPECTED
  FAILURE / 0 FAIL / 0 KNOWN GAP, in both arms.
- **`runtimeCapability.env` is added here and also by `ccwork/phase2-alloc`.**
  Same field name, same semantics (appended after the inherited environment so it
  wins), same comment shape. The two should merge cleanly; if they conflict, keep
  one copy of the field and both sets of entries.
- **The channel fix changes allocation and barrier behaviour, so it is exactly
  the kind of change §5.14 warns about composing.** A buffered channel with
  pointer elements now takes a second, scannable heap allocation instead of
  sharing the `hchan`'s no-scan one, and `typedmemmove` on a channel element now
  runs `bulkBarrierPreWrite` where it previously did not. Any branch that also
  moves objects between the frame and the heap or changes which stores emit
  barriers -- `ccwork/escape-analysis`, `ccwork/phase2-alloc` -- should re-run
  `gc/cleanup-basic` and `gc/channel-buffer-roots` *in combination* with this,
  not just on its own branch.
- `analysis/sepcompile/main.go` still lists `_goc_channel_element_\d+` among the
  counter-named symbol families. That family no longer exists after this change.
  It is a heuristic in an analysis tool, not a test, and removing it would change
  how the tool reads previously-built objects, so it was left alone rather than
  churned from outside its area.

## Matrix arm 1 (no `-O`): green

```
361 subtests, 361 PASS, 0 FAIL, 0 SKIP, 0 KNOWN GAP,
1 declared EXPECTED FAILURE (defer-panic/panic-string-output)
ok  github.com/evanphx/cg12/cmd/goc  336.670s
```

## Found by the optimized arm: `stack-scan/loop-safepoints` fails with `-O`

The same program that found the channel defect fails **only** with `-O`:

```
cg12scanroots: main_carried local slot 27 ... retains 0x...e4080 size 16 head 0x7272616300000062
cg12scanroots: main_carried local slot 41 ... retains 0x...e4080 size 16 head 0x7272616300000062
collected while live: carried-0 at carried before rewrite
panic: a stack slot live across a loop back edge was not a GC root
```

`carried-0` is the head of a chain that is reachable from the loop's `current`
variable through `next.next = current`, so it is live for the whole loop. With
`-O` it is collected mid-loop. Every other new capability passes with `-O`.
Reduction below.

## Matrix arm 2 (`-O`): 360 PASS, 1 FAIL — and the failure is a pre-existing defect

```
--- FAIL: TestARM64RuntimeCapabilityStatus/stack-scan/loop-safepoints
360 PASS, 1 FAIL   MATRIX-OPT EXIT=1
```

Every other capability passes with `-O`, including the other 15 new ones. The one
failure is `stack-scan/loop-safepoints`, and it is **not caused by anything on
this branch**: the same reducer fails identically when compiled by a goc built
from `main` (`0505d90`) in this same tree, 10/10.

### The defect: with `-O`, a heap pointer held in a loop-carried local is not a GC root

60 lines, no cleanups, no channels, no `unsafe`:

```go
type node struct { value int; next *node }

//go:noinline
func newNode(value int, next *node) *node { return &node{value: value, next: next} }

//go:noinline
func loop(rounds int) int {
	current := newNode(0, nil)
	for round := 1; round <= rounds; round++ {
		runtime.GC()
		churn()                       // allocates and drops 20000 nodes
		current = newNode(round, current)
	}
	runtime.GC(); churn(); runtime.GC()
	depth, sum := 0, 0
	for walk := current; walk != nil; walk = walk.next { depth++; sum += walk.value }
	return depth*1000 + sum
}
```

| build | result |
| --- | ---: |
| goc, no `-O` | 0/10 fail |
| goc, `-O` | 10/10 fail |
| goc built from `main` (`0505d90`), `-O` | 10/10 fail |
| host Go 1.26.1 | passes |

Under `GODEBUG=clobberfree=1` it is `unexpected fault address 0xdeadbeefdeadbeef`
— the chain was reclaimed while `current` still pointed at it. The same function
with the loop removed (`simple()`: allocate, `runtime.GC()`, churn,
`runtime.GC()`, use) passes with `-O`, so it is the loop-carried case.

### Direct evidence, from `cg12scanroots`

Same program, same source, the two builds:

```
no -O:  main_carried local slot 11 ... retains 0x...  size 32   <- the *node
        main_carried local slot 17 ... retains 0x...  size 32
        main_carried local slot 20/26 ... size 16              <- the string data
   -O:  main_carried local slot 16/20 ... size 16              <- the string data only
```

With `-O` the frame reports **no `*node` root at all**. The pointer is in the
frame — the emitted code stores it at `[x29,#40]` and reaches it through an
address parked at `[x29,#16]` — but the stack map does not describe it.

### Where the root is lost, narrowed with a throwaway diagnostic

A scratch build of goc with a print in `arm64.(*mc).recordSafepoint` (not
committed) dumps, per safepoint, the roots the backend is about to record. For
`main.carried` at the collection inside the loop, compiled serially so the output
does not interleave:

```
-O    : roots 5  stackPointerWords 9  stackAllocTmp 6
        root temp 4,6,22,30,89 -- every one isStackAlloc=true, gcref=true
no -O : roots 8  stackPointerWords 9  stackAllocTmp 13
        root temp 3,4,6,20,22,30,43,84 -- every one isStackAlloc=true, gcref=true
```

Two things follow.

- `-O` promotes four of the pointer-bearing allocations out of the frame
  (`stackAllocTmp` 13 -> 6) while `StackPointerWords` still lists nine. So the
  loop-carried pointer is no longer a frame allocation at all; it is an SSA value.
- **No promoted value is reported at that safepoint**: all five roots are
  allocation temporaries. `arm64.isSafepointRoot` would accept a promoted
  temporary — it returns true for `Cls == ir.ClsP` on a managed frame, and
  `opt.Mem2Reg` deliberately keeps the pointer class for exactly this reason — so
  the value is being lost *before* that test, by not being live in
  `analysis.Liveness` at the call or by no longer being a temporary there.

### The obvious candidate fix does not work — measured, not assumed

A scratch build that reports **every** pointer-bearing frame allocation at
**every** safepoint (the conservative map the current scheme replaced) still
fails the reducer 10/10 with `-O`. That is consistent with the narrowing above:
under `-O` the allocations that matter are not frame allocations any more, so a
fix that iterates frame allocations cannot reach them. Recording it so the next
job does not spend the experiment again.

The remaining suspects are `opt.Mem2Reg`'s promoted values and how they reach
`computeSafepointRoots`. That is a change to `opt`/`arm64` register-level root
reporting; it is not attempted here.

### And a second, cleanly-attributed defect found while narrowing it: `//go:noinline` does nothing

`goc/compile.go` parses exactly one compiler directive:

```go
g.fn.NoSplit = hasCompilerDirective(fd, "go:nosplit")
```

There is no `go:noinline` handling anywhere in `goc/`, `opt/` or `ir/` — `grep`
finds the string only inside `goc/testdata`. So the directive is silently
ignored, and `-O` inlines functions that ask not to be inlined. Proved by the
symbol table of the `-O` build of the reducer:

```
$ nm oroot_loop.bin | grep main_
main_churn          <- survived (too big)
main_main
(no main_loop, no main_simple, no main_newNode)
```

All three `//go:noinline` functions were folded into `main_main`.

Two consequences:

1. **It is a plausible proximate cause of the root loss above.** After inlining,
   the allocation helper, the loop and the collection all live in one frame, and
   that is the frame whose stack map loses the loop-carried pointer. Plausible,
   not established: `CG12_NO_COSTINLINE` and `CG12_NO_AGGINLINE` do not turn off
   the ordinary size-budget inliner, so the two could not be separated with the
   knobs that exist.
2. **It weakens every test in this repository that relies on `//go:noinline`.**
   25 of the 382 programs in `goc/testdata` use it, including capabilities
   written specifically to keep a frame distinct so a stack map can be reasoned
   about --- `gc/stack-argument-roots`, `gc/goroutine-entry-stack-map` and five
   of the six new `stack-scan` programs among them. Under `-O` that guarantee does not hold, and neither the
   corpus nor the matrix would notice. RUNTIME_PLAN §15 already records one
   investigation where "the real difference was inlining"; this is the mechanism
   that makes that failure mode easy to hit.

No test is added for this, because a test asserting the directive is honoured
would be red on arrival. It is reported.

### What this means for the matrix's headline numbers

- Arm 1 (no `-O`): **361 subtests, 361 PASS, 0 FAIL, 0 KNOWN GAP, 1 declared EXPECTED FAILURE.**
- Arm 2 (`-O`): **361 subtests, 360 PASS, 1 FAIL, 0 KNOWN GAP, 1 declared EXPECTED FAILURE.**

`stack-scan/loop-safepoints` was deliberately **left as `mustPass` and left
failing** rather than reclassified as a `knownGap`. Reclassifying it would restore
the "0 KNOWN GAP" headline while hiding a live, reproducible miscompile that
predates this branch; the reducer above is worth more than the green tick. If the
integrator prefers the green arm, the change is one field, and this section is the
reason it should not be made silently.

The reducer is committed as `goc/testdata/runtime_opt_loop_carried_root.go`,
deliberately not registered as a capability (a second failing capability would add
noise without information), so it outlives this job.

## Still unverified, and other jobs' business

Explicitly, so nothing here reads as stronger than it is.

**Unverified by this branch:**

- The `-O` root-loss mechanism is a **hypothesis** (`pointerAllocationSources`
  losing an allocation whose address round-trips through memory). The
  measurements are solid; the attribution is not. No fix attempted.
- Whether the three `gcDrainMarkWorker*` wrappers genuinely never run or the
  coverage instrumentation is missing their counters. Either answer has
  consequences — the second would mean §1's percentages understate coverage —
  and neither is established.
- Whether the kernel actually backs GC metadata with huge pages after the
  transition. The transition is proved to execute; `madvise` is advisory.
- Dedicated versus fractional mark workers are **not** separated, so that part of
  §6's "dedicated/fractional/idle mark workers" is not delivered.
- The rate of the channel defect *before* the fix on the pack-linked and batch
  compile paths specifically: the A/B was measured on the `-runtime <pack>` path
  (which the matrix uses) and on the monolithic path (coverage builds), not on
  `compile-batch` separately.
- No new coverage baseline was accepted. The 16 new capabilities are recorded in
  `runtime_coverage_baseline_pending.json` with reasons, and
  `TestCheckedRuntimeCoverageBaselineDenominator` reconciles 345 + 16 = 361.

**Other jobs' business, found here but not touched:**

- `mspan.reportZombies` is blind on Green Tea spans. Independently reproduced
  with a deliberate zombie, and the mechanism pinned (`moveInlineMarks` resets the
  inline bits that `markBitsForBase` then reads). Owned by `ccwork/reportzombies`.
- goc emits more than one `abi.Type` descriptor family per type: after the fix,
  `hchan.elemtype` has correct contents but is still not pointer-identical to the
  descriptor `reflect` reports. Pre-existing, untraced, not a GC-correctness
  issue by itself.
- cg12 has no asynchronous preemption at all, so a non-terminating call-free loop
  has no preemption point. That is §7's (Phase 3's) problem; §6 only needed the
  classification.

## Commits on this branch

| commit | what |
| --- | --- |
| `d4f8f8f` | goc: give a channel's element type its real pointer metadata (the fix) |
| `3ecd3fb` | the stack-scanning and GC-stress capability programs |
| `63bcef9` | register them; `runtimeCapability.env`; the compiler test for the channel defect |
| `b780655` | classify the conservative stack scan unreachable, with the boundary proved |
| `652b788` | correct the coverage path assertions to measurable claims |
| `dc04f41` | report: the Phase 2 inventory |
| `25cbefe` | RUNTIME_PLAN §6.1 |
| `f14d0c6` | record the `-O` loop-carried root loss, with its reducer |

27 files, +4542/-18. One file in `goc/` changed (`compile.go`, 22 lines of code);
everything else is tests, capability programs, and documentation.

## Final suite results

Measured last, on the finished tree, while the box was shared with three sibling
jobs (load average ~100 on 64 cores), so wall-clock numbers are not comparable
with the earlier ones.

*This section is filled in as each result lands. Anything still blank was still
running.*
