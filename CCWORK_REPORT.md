# Front-end determinism — finishing `ccwork/frontend-determinism`

Branch `ccwork/frontend-determinism-2`, off `main` (`9cd2621`).

**Status: in progress.** This file is updated as each result lands.

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
