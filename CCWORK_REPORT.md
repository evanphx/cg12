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

# `mspan.reportZombies` is blind on Green Tea spans — `ccwork/reportzombies`

**Status: investigation in progress; this file is updated as each result lands.**

## The defect, read out of the source

`stdlib/src/runtime/mgcsweep.go` `sweepLocked.sweep`:

- line 655: `if gcUsesSpanInlineMarkBits(s.elemsize) { s.moveInlineMarks(s.gcmarkBits) }`
  — merges the span's inline mark bits into `s.gcmarkBits` **and clears them**
  (`imb.init(s.spanclass, true)` at `mgcmark_greenteagc.go:215`).
- lines 660-676: the zombie *check* reads `s.gcmarkBits` directly. Correct.
- line 862, inside `reportZombies`: `mbits := s.markBitsForBase()`, which on a
  Green Tea span returns `&s.inlineMarkBits().marks[0]`
  (`mgcmark_greenteagc.go:239`) — the bits `moveInlineMarks` just zeroed.

So on every span with `16 <= elemsize <= 512` (`gcUsesSpanInlineMarkBits` =
`heapBitsInSpan(size) && size >= 16`), the report prints every object `unmarked`,
never prints the `zombie` line, and never hexdumps the object — while still
throwing `found pointer to free object`.

Both `reportZombies` call sites (mgcsweep.go:668 and :673) are after the
`moveInlineMarks`, so there is no call path on which the inline bits are still
live.

## It is a genuine upstream Go bug, not a cg12 artifact

The three files involved — `mgcsweep.go`, `mgcmark_greenteagc.go`,
`mgcmark_nogreenteagc.go` — are **byte-identical** to `go1.26.1`'s
(`diff -u $(go env GOROOT)/src/runtime/... stdlib/src/runtime/...` is empty), and
`goexperiment.greenteagc` is on by default in Go 1.26, so the host toolchain
compiles the same code path goc does (`build.Default.ToolTags` contains
`goexperiment.greenteagc`).

It reproduces on the **host toolchain with no cg12 involved at all**.
`$TMPDIR/zomb/zombie.go` (kept as `cmd/goc/testdata/zombie_report_probe.go`)
builds a genuine zombie the way `reportZombies`' own comment describes as case 1:
allocate 64 32-byte pointer-free objects, keep 63 alive in a global, hide the
64th as a `uintptr`, let one collection free it, resurrect it through the
`uintptr` into a global, and collect again.

| build | zombie lines | `marked` | `unmarked` | hexdump |
| --- | --- | --- | --- | --- |
| `go build` (Green Tea, default) | **0** | 0 | 252 | none |
| `GOEXPERIMENT=nogreenteagc go build` | 1 | 64 | 191 | yes |

Green Tea, exactly as §5.11 predicted:

```
runtime: marked free object in span 0xfb1799233e08, elemsize=32 freeindex=0 (...)
0x2f2767122000 alloc unmarked
... 252 lines, every one "unmarked", no "zombie" line ...
fatal error: found pointer to free object
```

`nogreenteagc`, the same program and the same fault:

```
0x3af6b140e500 free  marked   zombie
                   7 6 5 4  3 2 1 0   f e d c  b a 9 8  0123456789abcdef
00003af6b140e500: 7a6f6d62 69650028  11111111 11111111  (.eibmoz........
00003af6b140e510: 22222222 22222222  33333333 33333333  """"""""33333333
```

`0x7a6f6d6269650028` is the payload the program wrote into object index 40 —
the report names the right object and dumps its contents.

**Verdict: upstream bug, in upstream's own code, on upstream's own default
configuration.** `reportZombies` was simply not updated when Green Tea moved
small-span marks into the span. Everything else in `sweep` that reads marks after
`moveInlineMarks` already reads `gcmarkBits`: the zombie check itself
(mgcsweep.go:667,672) and `countAlloc` (mbitmap.go:1507). `reportZombies` is the
one straggler. The pre-move `traceAllocFree`/`clobberfree` loop at mgcsweep.go:620
*does* correctly use `markBitsForBase`, because at that point the inline bits are
still the live ones — checked, because if it were wrong `clobberfree` would be
scribbling on live objects. It is not wrong.

`gcmarknewobject` (mgcmark.go:1813) also marks through `markBitsForIndex`, which
routes to the inline bits on such a span, so there is no mark that reaches
`gcmarkBits` by another route and no ordering hazard in reading it.

## The fix

`stdlib/src/runtime/mgcsweep.go`, one statement plus comments, written the way a
CL would be:

```go
	mbits := markBits{&s.gcmarkBits.x, uint8(1), 0}
```

in place of `mbits := s.markBitsForBase()`, with a comment on the function saying
it must be called after inline marks have been moved. Both call sites (:668, :673)
are already after `moveInlineMarks`, so there is no path this breaks; on a
non-Green-Tea span the expression is exactly what `markBitsForBase` returned.

### Proved on the fault, through goc

Same program, compiled by goc, before and after:

| goc build | zombie lines | `marked` | `unmarked` |
| --- | --- | --- | --- |
| before the fix | **0** | 0 | 252 |
| after the fix | **1** | 64 | 188 |

After:

```
0x727a33d024e0 alloc marked
0x727a33d02500 free  marked   zombie
                   7 6 5 4  3 2 1 0   f e d c  b a 9 8  0123456789abcdef
0000727a33d02500: 7a6f6d62 69650028  11111111 11111111  (.eibmoz........
0000727a33d02510: 22222222 22222222  33333333 33333333  """"""""33333333
0x727a33d02520 alloc marked
```

Same object index (40), same payload, same shape as the `nogreenteagc` reference
above. `fatal error: found pointer to free object` still follows, as it must.

## The regression guard

`cmd/goc/zombie_report_test.go` +
`cmd/goc/testdata/zombie_report_probe.go`: compiles the probe with goc, runs it,
and requires the report to name and dump the zombie. It is in `cmd/goc`, so
`make test-goc-cmd` runs it; the fixture is in `cmd/goc/testdata` rather than
`goc/testdata` deliberately, so the capability matrix stays at 345 subtests and
the determinism corpus stays at 365 programs.

Checked in both directions, which is the only thing that makes it a guard:

- with the fix: PASS (3.4s).
- with `mbits := s.markBitsForBase()` put back and nothing else changed:
  `FAIL ... "0" is not greater than "0" / every object printed as unmarked`.
  That is the defect's exact signature, so a revert cannot pass this test.

Stability of the probe, all runs naming the zombie with the right payload:
60/60 at goc default, 30/30 at `goc -O`, and 10/10 at each of
`GOMAXPROCS` 1, 2, 4, 8, 64.

## The audit, so this is a class and not an instance

Every reader of a span's mark bits, checked against the `moveInlineMarks`
boundary in `sweep`:

| site | when | bitmap it reads | verdict |
| --- | --- | --- | --- |
| `mgcsweep.go:559` specials loop | before the move | inline (`markBitsForIndex`) | correct |
| `mgcsweep.go:620` trace/clobberfree/sanitizer | before the move | inline (`markBitsForBase`) | correct |
| `mgcsweep.go:667,672` zombie check | after | `gcmarkBits` | correct |
| `mbitmap.go:1507` `countAlloc` | after | `gcmarkBits` | correct |
| `mgcsweep.go:862` `reportZombies` | after | **inline, just cleared** | **the bug** |
| `mgcmark.go:1698` `greyobject`, `:1813` `gcmarknewobject`, `mwbbuf.go:249`, `mbitmap.go:1276` | mark phase | inline | correct |

`reportZombies` was the only post-move reader still going through
`markBitsForBase`; there is no second instance. `gcmarknewobject` marking through
`markBitsForIndex` also settles the ordering question: nothing writes `gcmarkBits`
by another route on such a span, so after the move it is the complete record.

`mgcsweep.go:620` was worth checking rather than assuming: if it were on the wrong
side of the boundary, `GODEBUG=clobberfree=1` would be scribbling over live
objects. It is not.

## Suites on this branch

| | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | pass, 0 FAIL |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 721.570s` |
| `make test-goc-cmd` | `ok github.com/evanphx/cg12/cmd/goc 292.389s` (includes the new guard) |
| full unsharded matrix, default arm, `-v` | **345 subtests, 344 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 SKIP, 0 KNOWN GAP**, `ok … 369.507s` |

Census taken from the `-v` output: 345 three-part `=== RUN` lines, 345 `--- PASS`
lines, 344 harness `PASS <program>.go` lines, one `EXPECTED FAILURE
runtime_panic_print_string.go`, zero `FAIL`, zero `SKIP`, zero `KNOWN GAP`.
**No capability is non-passing.**

| full unsharded matrix, **`-runtime-opt` arm**, `-v` | **345 subtests, 344 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 SKIP, 0 KNOWN GAP**, `ok … 445.036s` |

Both arms, same census method, same numbers. **The complete list of non-passing
capabilities is empty in both arms**; the single declared exception is
`defer-panic/panic-string-output` (`runtime_panic_print_string.go`), which is an
`expectedFailure` by design and unrelated to this work.

## Determinism, measured before and after

`scripts/determinism-check.sh` on this tree — five programs, four compiles each
(2 cold with `CG12_NOCACHE=1`, 2 warm), one hash per program:

```
after the fix, default:
hello.go                            round1:identical(1849f132ac7e2a19)  round2:identical(1849f132ac7e2a19)
fmt_sprintf.go                      round1:identical(70abdf422a1d655c)  round2:identical(70abdf422a1d655c)
gc_struct.go                        round1:identical(d0432862dd169ab6)  round2:identical(d0432862dd169ab6)
runtime_cleanup_frame_retention.go  round1:identical(6679afc39c6ed814)  round2:identical(6679afc39c6ed814)
runtime_defer_capture_allocs.go     round1:identical(a8a21559e46176c6)  round2:identical(a8a21559e46176c6)

after the fix, -O:
hello.go                            round1:identical(0d9a8de6aea30832)  round2:identical(0d9a8de6aea30832)
fmt_sprintf.go                      round1:identical(d49eed212fd50f5a)  round2:identical(d49eed212fd50f5a)
gc_struct.go                        round1:identical(9ad945d8804b9565)  round2:identical(9ad945d8804b9565)
runtime_cleanup_frame_retention.go  round1:identical(526cf79535de4b94)  round2:identical(526cf79535de4b94)
runtime_defer_capture_allocs.go     round1:identical(827f563d1e1431ad)  round2:identical(827f563d1e1431ad)

before the fix (same tree, same path, only reportZombies reverted), default:
hello.go                            round1:identical(b53aadefd9385c97)  round2:identical(b53aadefd9385c97)
fmt_sprintf.go                      round1:identical(7c4cc8393cbbde0e)  round2:identical(7c4cc8393cbbde0e)
gc_struct.go                        round1:identical(8ffce275c5592c9e)  round2:identical(8ffce275c5592c9e)
runtime_cleanup_frame_retention.go  round1:identical(16298d5490a9c2b5)  round2:identical(16298d5490a9c2b5)
runtime_defer_capture_allocs.go     round1:identical(bee60a6b3b9babfd)  round2:identical(bee60a6b3b9babfd)
```

The "before" column was measured by reverting only the one statement in the same
working tree at the same filesystem path, per §23's rule that a worktree at a
different path is not a valid reference build. Every program is `identical` in
every configuration on both sides. The digests differ between the two sides
because the runtime source differs — that is the change landing, not a
regression.

Full corpus sweep on this tree, `scripts/determinism-check.sh -corpus -rounds 2 -j 4`:

```
programs=365 rounds=2 workers=4 optimize=false pack=""
round 0: 365 programs in 618.7s, 0 failed
round 1: 365 programs in 512.3s, 0 failed
failed to compile: 0
content varies between rounds: 0
image varies, content identical (layout only): 0
reproducible=365 varying=0 failed=0 of 365 over 2 rounds
```

730 compiles, 0 varying. Two rounds is a weaker sample than §23's, which is
stated rather than glossed: §5.10 records a program that took its minority branch
3 times in 53 compiles, so two draws cannot rule out a rare branch. What it does
establish is that nothing in this branch turned a reproducible compile into an
irreproducible one, and the change is not in the compiler at all — it is one
statement of vendored runtime source.

