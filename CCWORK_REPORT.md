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
