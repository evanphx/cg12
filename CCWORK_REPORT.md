# Enable goc's full optimisation pipeline by default

Branch: `ccwork/enable-full-pipeline` = `ccwork/mem2reg-iface-dispatch` + merge of
`ccwork/mem2reg-gc-visibility` (merge commit 4803808).

## Status: IN PROGRESS — this file is written as work lands, not at the end.

## Merge of the two prerequisite branches

Both branches independently landed the *same* fix: `opt.markManagedDef` in
`opt/mem2reg.go`, called from `renameBlock` on every store that becomes a promoted
managed variable's reaching definition. The conflict was a genuine duplicate, not two
halves of one change.

Difference between the two versions, and what I kept:

- `mem2reg-iface-dispatch`: returns early if the temp already has `GCRef`, else
  `f.MarkGCRefType(value, v.gcType)`.
- `mem2reg-gc-visibility`: always sets `GCRef = true`, and sets `GCType` only when
  it is currently 0.

I kept **gc-visibility's**: it is a strict superset. It preserves an existing
type descriptor exactly as iface-dispatch does, and additionally fills in the slot's
descriptor for a value that was marked `GCRef` with no type. `MarkGCRefType`
overwrites `GCType` unconditionally (`ir/build.go:117`), so iface-dispatch's early
return was the only thing stopping it from clobbering a more precise descriptor.

Both branches' tests are kept: `goc/optgcroot_test.go` +
`goc/testdata/runtime_opt_promoted_interface_root.go` (iface-dispatch) and
`goc/testdata/runtime_gc_promoted_local_root.go` +
`cmd/goc/runtime_status_test.go` (gc-visibility).

Both branches claim their blocker fixed. gc-visibility additionally claims the
flate crash was never mem2reg's — it was the zero-capacity-slice defect fixed in
800f47f, whose *rate* promotion roughly doubled. Both claims are re-verified from
scratch below, on the merged tree, rather than taken on trust.

## 1. What the full pipeline contains that the bounded one does not

`BoundedPipeline` is one fixpoint of three passes: **fold, copy, dce**. That is all
`goc -O` has ever meant for a whole-program Go build.

`DefaultPipeline` (`opt/pass.go:108`) is, in order:

| # | pass | kind | in bounded? |
|---|------|------|-------------|
| 1 | `mem2reg` | func | no |
| 2 | `clean` fixpoint: `fold`, `copy`, `loadelim`, `deadalloc`, `gvn`, `jumpthread`, `simplifycfg`, `dce` | func | fold/copy/dce only |
| 3 | `inline` fixpoint: `inline` + `clean` | module + func | no |
| 4 | `mem2reg` (again, to catch what inlining exposed) | func | no |
| 5 | `clean` | | partial |
| 6 | `unroll` (`UnrollRecursion`) | module | no |
| 7 | `inline` fixpoint again | | no |
| 8 | `constantp` (`ResolveConstantP`) | func | no |
| 9 | `ifconvert` | func | no |
| 10 | `clean` | | partial |
| 11 | `tailmerge`, `simplifycfg`, `dce` | func | dce only |
| 12 | `deadfunc` (`DeadFuncElim`) | module | no |
| 13 | `gcm`, `dce` | func | dce only |

So the bounded path skips **thirteen distinct passes**: mem2reg, loadelim,
deadalloc, gvn, jumpthread, simplifycfg, inline, unroll, constantp, ifconvert,
tailmerge, deadfunc, gcm. mem2reg is the one that was investigated; it is not the
only one, and it is not even the expensive one.

**Are the other twelve safe to enable?** They are not new code and they are not
untested — every one of them has unit tests in `opt/`, and every one of them has
been running on cg12cc's C modules (Ruby, miniruby, CRuby) for as long as it has
existed. What is new is that they had never seen a *Go-frontend* module, which is
where the two known blockers came from: both were mem2reg interacting with goc's
GC metadata, a thing C modules do not have. The passes with a comparable exposure
to Go-specific metadata are the ones that move or delete values across safepoints
— `gcm` (schedules values, so it lengthens live ranges), `inline` (copies a
callee's frame into a caller's, including its GC roots), `deadalloc` and
`deadfunc` (delete allocations and functions). Those are where I looked hardest;
the campaign below runs every arm on the whole matrix, corpus, GC reducer, escape
audits and flate loop rather than reasoning about it.

## 2. The change

`opt.OptimizeModule` no longer consults a size budget at all. `moduleOptimizationOverBudget`
and its four constants are deleted. The pipeline is chosen by `opt.ModulePipeline`:

- `GOC_OPT_PIPELINE` unset / `full` / `default` → `DefaultPipeline` (**the new default**)
- `GOC_OPT_PIPELINE=promote` → `BoundedPipeline` + mem2reg (the promotion-only arm)
- `GOC_OPT_PIPELINE=bounded` → `BoundedPipeline` (the pre-change default; the bisection escape hatch)
- an unrecognized value panics rather than silently selecting the default

`GOC_OPT_SKIP=name1,name2` drops named passes from whichever arm was selected, at
any nesting depth (it descends into fixpoints). The corpus and the capability
matrix compile in-process or through `goc` subprocesses with fixed argv, so there
is no compiler flag to thread through; an environment variable is the only handle
an outside job has, and attributing a miscompile to one pass is the whole game
here.

## 4a. Compile-time cost — first measurement, whole-program build

168-line `hello.go` (fmt.Println plus a 1000-iteration multiply-add loop),
`goc -O`, whole-program (no prebuilt pack), on the 64-core box, load average ~4:

| arm | wall | peak RSS |
|-----|------|----------|
| `GOC_OPT_PIPELINE=bounded` | 6.69 s | 599 MB |
| `GOC_OPT_PIPELINE=full` | 30.99 s | 876 MB |

**4.6x, and it is not mem2reg.** From a CPU profile of the full run
(`opt.OptimizeModule` cum 24.66 s of 66.63 s samples over 31.11 s wall):

    gvn         4.10 s      jumpthread   2.42 s      gcm        0.45 s
    loadelim    3.87 s      fold         2.18 s      ifconvert  0.39 s
    simplifycfg 3.39 s      deadalloc    1.92 s      copy       0.37 s
    dce         3.19 s      inline       1.07 s      mem2reg    0.37 s

mem2reg — the pass this whole exercise is about — costs 0.37 s. The cost is the
`clean` fixpoint (gvn + loadelim + simplifycfg + dce + jumpthread + fold +
deadalloc = 21 s) being re-run to a fixpoint over 5101 functions, four times over
in the pipeline's shape. Two secondary costs: the arm64 backend goes 7.0 s → 11.1 s
on the optimized IR (register allocation over longer live ranges), and 36% of all
samples are the *compiler's own* GC — the pipeline allocates enough to make the
Go runtime the largest single consumer.

Where the cost should be spent is discussed at the end, once the correctness
campaign has said which passes have to stay.

## 3. The failure campaign

### Capability matrix — both arms, full pipeline (368 capabilities in the table)

| arm | result | wall |
|-----|--------|------|
| default (no `-O`) | **PASS** | 109.6 s |
| `-runtime-opt`, `GOC_OPT_PIPELINE` default (= full) | **PASS** | 313.6 s |

The `-O` arm is 2.9x the wall clock of the default arm, where before this change
the two were close. Most of that is the prebuilt packs: `net/http` with the full
pipeline takes ~240 s and peaks at **2.99 GiB RSS**, which is the memory the
budget was originally introduced to bound (against a 3 GiB ceiling). That is the
one number in this whole exercise that is uncomfortably close to a limit, and it
is called out again in the cost section.

Note the matrix here has **368** capabilities, not the 366 the brief expected:
the tree carried 367 before either prerequisite branch, and the two branches add
one reducer each — `runtime_gc_promoted_local_root.go` and
`runtime_opt_promoted_interface_root.go`. Both arms are 368/368, counted below.

Counted, second run (packs warm, so the wall figures are the steady-state ones):

| arm | PASS | FAIL | wall |
|-----|------|------|------|
| default | **368** | 0 | 130.0 s |
| `-runtime-opt`, full pipeline | **368** | 0 | 131.5 s |

With the packs cached the two arms cost the same. The 313.6 s figure above is a
cold-pack run and is paid once per tree, not per run.

### GC reducers, full pipeline, 20 runs each

`GOMAXPROCS=3`, both collector settings, `goc -O` whole-program builds:

| reducer | `GOGC=10` | default `GOGC` |
|---------|-----------|----------------|
| `runtime_gc_type_mask_padding.go` | **0 / 20** | **0 / 20** |
| `runtime_gc_promoted_local_root.go` (gc-visibility's reducer) | **0 / 20** | **0 / 20** |
| `runtime_opt_promoted_interface_root.go` (iface-dispatch's reducer) | **0 / 20** | **0 / 20** |
| `runtime_opt_loop_carried_root.go` | **0 / 20** | **0 / 20** |

Both prerequisite branches' blockers are fixed on the merged tree, checked
against their own reducers rather than taken on trust.

### Loop-aliasing programs and allocation counts — full pipeline

`TestLoopBodyAllocationsAreDistinctPerIteration` runs each loop-alias reduction
unoptimized and optimized; the optimized arm is now `opt.OptimizeModule` over the
whole monolithic module (runtime included), which before this change degraded to
fold/copy/dce and promoted nothing. All 17 subtests PASS:

    loop_alias_forms.go                     2.51s / -O 15.40s
    loop_alias_composite.go                 2.54s / -O 28.15s
    variadic_backing.go                     5.31s / -O 28.79s
    variadic_element_retention.go           2.60s / -O 15.15s
    variadic_element_address_retention.go   2.50s / -O 15.12s
    loop_alias_frame_local.go               2.48s / -O 15.09s
    allocation_counts.go                    6.50s / -O 39.01s
    TestLoopAliasAudit                      268.48s (unoptimized; see below)

The per-iteration allocation identity the loop rule exists to protect survives
promotion, inlining and GCM. The `-O` column is 6-11x the unoptimized one; that
is the compile-time cost again, in the goc suite this time.

### Escape / census / shadow audits — PASS, and a caveat about what they prove

| guard | result |
|-------|--------|
| `TestFrameEscapeAudit` | PASS |
| `TestAllocationCensus` | PASS (183.85 s, the corpus compile) |
| `TestEscapeShadowPlacement` | PASS |
| `TestLoopAliasAudit` | PASS (268.48 s) |
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` | PASS |
| `TestParallelBackendIsByteIdenticalToSerial` (`./arm64`) | PASS |

**These four audits are insensitive to this change, and it is worth saying so
rather than counting them as evidence.** `auditCorpus` (`goc/corpusaudit_test.go:130`)
compiles each of the 406 corpus programs with `goc.CompileExecutable` and never
calls `opt.OptimizeModule`; the escape, census, loop-alias and shadow analyses all
read the unoptimized module. They are a real guard against the *merge* having
disturbed the front end, and they are the guard the brief asked for, but a green
run here says nothing about whether the full pipeline miscompiles. The evidence
that bears on that is the capability matrix's `-O` arm, the reducers, the run
loops and the sweep.

### flate crash loop and p256, full pipeline

| program | collector | runs | failures |
|---------|-----------|------|----------|
| `placement_bench/flate` | default | 250 | **0** |
| `placement_bench/flate` | `GOGC=10` | 250 | **0** |
| `placement_bench/p256` | `GOGC=10` | 100 | **0** |

500 flate runs, twice the 200 the brief asked for, at two collector settings.
p256 is the program the gc-visibility branch found the promotion defect in (35
failures in 40 at `GOGC=10` before its fix); it is clean under the whole pipeline,
not just under promotion.

### Determinism — byte-identical, both configurations

`scripts/determinism-check.sh`, five programs, cold (`CG12_NOCACHE=1`) and warm,
two rounds each — four hashes per program, all four must agree:

| configuration | result |
|---------------|--------|
| default (no `-O`) | 5/5 identical, both caching paths, both rounds |
| `-O` (full pipeline) | 5/5 identical, both caching paths, both rounds |

The `-O` hashes differ from the default ones, which is the check that the
pipeline actually ran. `TestParallelBackendIsByteIdenticalToSerial` passes, and
`TestMem2RegPlacesPhisInTheSameOrderEveryTime` — the opt-side determinism
property, which `opt/determinism_test.go` recorded as unreachable from goc
because every goc module took the bounded path — now guards goc builds too.

### Compile sweep — every corpus program, `goc -O`, full pipeline

All 406 `goc/testdata/*.go` compiled with `goc -O` as whole-program builds:

    arm=full  compiled=406  failures=0  (mean 32.1 s, 13030 CPU-seconds total)

Zero compiler crashes, assertion failures or lowering errors on any of the 406.
This is broader than the capability matrix's 368 — it covers the 38 corpus
programs the matrix does not run — but it is a compile-only check.
