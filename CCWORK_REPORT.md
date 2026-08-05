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

### A differential the existing suites do not run

The audits above read unoptimized IR, and the capability matrix checks each
program against its own expectation rather than against the other arm. Neither
would notice a pass that changes a program's *answer* in a way the program itself
does not assert on. So: every one of the 406 corpus programs was built twice —
`GOC_OPT_PIPELINE=bounded` and the full default — and both binaries run, with the
bounded build run twice first so a program that disagrees with itself is excluded
as nondeterministic rather than reported as a miscompile. Results below.

    SAME    403 / 406
    NONDET    1  (bytes_grow_stats — disagrees with itself under the bounded build)
    DIFF      2

**`runtime_panic_print_string` — not a defect.** The only difference is a
traceback PC offset: `runtime_gopanic() ?:0 +0xa3c` under bounded, `+0xb78`
under full. `gopanic` is a different size when it is optimized, so the offset
inside it differs. Both print `panic: boom` and exit 2 with the same frame list.

**`runtime_lock_osthread` — a real defect, and the third blocker.**

    bounded build: 0 failures / 100 runs
    full build:   14 failures / 100 runs

    fatal error: runtime: split stack overflow
     runtime: split stack overflow: 0x1c4ca5fc87e0 < 0x1c4ca5fc8800
    goroutine 3: runtime_mcentral_uncacheSpan <- runtime_mcache_refill
      <- runtime_mcache_nextFree <- runtime_mallocgcSmallScanHeader
      <- runtime_mallocgc <- runtime_allocm <- runtime_newm
      <- runtime_LockOSThread <- main.main.func1

The program is nine lines: a goroutine that calls `runtime.LockOSThread`,
defers `UnlockOSThread` and sends 42 on a channel.

*Attribution.* Bisected with `GOC_OPT_SKIP`, 100 runs per arm:

| arm | failures / 100 |
|-----|----------------|
| `bounded` | 0 |
| `promote` (bounded + mem2reg) | 0 |
| full, `skip=inline` | **0** |
| full, `skip=mem2reg` | 0 |
| full, `skip=loadelim` | 0 |
| full, `skip=simplifycfg` | 0 |
| full, `skip=tailmerge` | 7 |
| full, `skip=deadalloc` | 10 |
| full, `skip=deadfunc` | 10 |
| full, `skip=gcm` | 13 |
| **full (nothing skipped)** | **14** |
| full, `skip=ifconvert` | 17 |
| full, `skip=jumpthread` | 21 |
| full, `skip=gvn` | 23 |

The arms that reach 0 are inline and the three passes that feed it (a function
that is not promoted, load-forwarded and CFG-simplified is too big to inline, and
too big for its callees to inline into it). The arms that make it *worse* are the
ones that leave more code for the inliner to move. So: **inlining**.

*Mechanism, measured from the emitted code.* Frame sizes of the chain, in bytes:

| function | bounded | full, `skip=inline` | full |
|----------|---------|---------------------|------|
| `runtime_mcache_nextFree` | 384 | 368 | **656** |
| `runtime_mcache_refill` | 368 | 352 | 320 |
| `runtime_mallocgcSmallScanHeader` | 416 | 352 | 352 |

`nextFree` and `refill` carry **no stack-growth guard in either build** — and
they are not `//go:nosplit` in the Go source. `goc/compile.go:9487`
`runtimeImplicitNoSplit` marks `nextFreeFast`, `nextFree`, `nextFreeIndex` and
`refill` nosplit itself, with this reason: *"The upstream compiler normally
inlines this allocator fast-path helper into mallocgc variants. cg12 keeps it
outlined, so mark these helpers nosplit to preserve the same runtime invariant."*

So the nosplit run between `mallocgcSmallScanHeader`'s guard and
`mcentral_uncacheSpan`'s is `nextFree` + `refill`: **752 bytes under the bounded
pipeline, 976 under the full one.** Go reserves ~800-928 bytes below
`stackguard0` for exactly this. The bounded pipeline fit inside the reserve by
about fifty bytes; inlining 288 bytes of callee locals into `nextFree` puts it
outside, and the next guarded call finds `sp` already below `stack.lo`.

It is intermittent because the chain is only entered when the mcache actually
needs a refill.

**This is not a miscompile of user code.** It is a missing constraint: cg12 has
no nosplit stack-depth budget. Go's linker enforces one (`nosplit stack over 792
byte limit` is a link-time error); `opt.AuditNoSplitCalls` checks only that a
nosplit function does not *call* a splittable one, not how deep the nosplit run
gets. The bounded pipeline never grew a frame enough to matter, so the gap was
invisible.

Note the capability matrix runs `runtime_lock_osthread.go` (`cmd/goc/runtime_status_test.go:660`)
and passed it twice. At a 14% failure rate that is a 74% chance — the matrix was
lucky, not clean. A one-shot matrix is not an instrument for a 14% defect, which
is why the differential above exists.

*The fix.* `opt/inline.go`: `inlineModule` no longer inlines into a `NoSplit`
caller (`frameIsSpentFromTheNoSplitReserve`). That is the conservative rule and
the only one statable without a frame-size model — the inliner works on IR and
cannot see a frame. `InlineNoSplitCalls` is untouched: it inlines *into* nosplit
callers deliberately, to keep a stack check off a signal-entry path, and only
small accessors.

| | bounded | full, before the fix | full, after |
|---|---|---|---|
| `runtime_mcache_nextFree` frame | 384 B | 656 B | **368 B** |
| `runtime_mcache_refill` frame | 368 B | 320 B | 352 B |
| nosplit run | 752 B | 976 B | **720 B** |
| `runtime_lock_osthread` failures | 0 / 100 | 14 / 100 | **0 / 400** |

The fix leaves the chain smaller than the bounded pipeline left it.

*What it costs, and what would let it come back.* Not inlining into nosplit
functions costs the runtime's allocator fast paths their inlining, which is
exactly where it would pay. Upstream Go inlines there freely and pays for it with
a linker check cg12 does not have. **The fix that would let this come back is a
nosplit frame budget in the backend** — arm64 knows every function's frame size
after lowering and `opt.AuditNoSplitCalls` already builds the call graph, so the
missing piece is walking nosplit runs and rejecting a build that overruns the
reserve. That turns a 14%-of-runs fatal error into a compile-time error, which is
the right shape. It is out of scope here and is the single most valuable
follow-up this exercise found.

`opt/inline_test.go:TestInliningDoesNotGrowANoSplitCaller` is the regression
guard: deterministic, no runtime needed.

*One caveat on that bisection table.* At the time it was taken, both inliner
stages were `Fixpoint("inline", inline, clean)`, and `GOC_OPT_SKIP` matches a
fixpoint's own name — so the `skip=inline` row also removed two rounds of
cleanup. The attribution does not rest on that row: it rests on the fix, which
removes only inlining-into-nosplit and takes `nextFree`'s frame from 656 bytes
back to 368 and the failure rate from 14/100 to 0/400. The fixpoints are renamed
`inline-fixpoint` (commit d1168d2) so the next bisection does not have the
problem.

## 4. Compile-time cost

### One whole-program build

`hello.go` (fmt.Println plus a multiply-add loop), `goc -O`, no prebuilt pack:

| arm | wall | peak RSS |
|-----|------|----------|
| bounded | 6.69 s | 599 MB |
| full | 30.99 s | 876 MB |

**4.6x.** Where it goes, from the CPU profile — `opt.OptimizeModule` is 24.66 s of
66.63 s of samples over 31.11 s wall (the compiler's own GC accounts for 36% of
samples, running in parallel):

    gvn         4.10 s      jumpthread   2.42 s      gcm        0.45 s
    loadelim    3.87 s      fold         2.18 s      ifconvert  0.39 s
    simplifycfg 3.39 s      deadalloc    1.92 s      copy       0.37 s
    dce         3.19 s      inline       1.07 s      mem2reg    0.37 s

mem2reg — the pass this whole exercise is about — is **0.37 s**, 1.5% of the
optimizer's time. The cost is the `clean` fixpoint (gvn + loadelim + simplifycfg
+ dce + jumpthread + fold + deadalloc = 21 s of the 24.7) run to convergence over
5101 functions, and `DefaultPipeline` runs `clean` in four places plus twice more
inside the inliner fixpoints. A secondary cost: the arm64 backend goes 7.0 s →
11.1 s on the optimized IR, register allocation over longer live ranges.

### The whole corpus

Every `goc/testdata/*.go` compiled with `goc -O` as a whole-program build, 24 at
a time on the 64-core box (so these are comparable to each other, not to a
single-process figure):

| arm | total | mean per program |
|-----|-------|------------------|
| bounded | 2877 CPU-s | 7.09 s |
| full | 13030 CPU-s | 32.09 s |

**4.53x across the corpus.** The distribution is not flat — the worst programs
are the ones that already cost the most:

    stdlib_http_tls_client_server   47.4 s -> 280.2 s   5.9x
    stdlib_http_client_server       45.8 s -> 268.9 s   5.9x
    stdlib_http_redirect_keepalive  46.0 s -> 268.7 s   5.9x
    stdlib_http_cookiejar           43.6 s -> 254.5 s   5.8x
    stdlib_http_parse_roundtrip     43.4 s -> 252.8 s   5.8x
    stdlib_http_multipart_form      44.2 s -> 256.3 s   5.8x
    stdlib_crypto_x509_ed25519      20.9 s -> 137.5 s   6.6x
    stdlib_crypto_ecdsa             16.2 s -> 105.2 s   6.5x

A single HTTP program now takes four and a half minutes to compile.

### What it does *not* cost

The capability matrix, with the prebuilt runtime packs warm, is **130.0 s on the
default arm and 131.5 s on the `-O` arm** — the same. The pack build is where the
`-O` arm's extra cost lives, and `goc build-runtime` caches on disk keyed by
compiler bytes, stdlib contents and (now) the pipeline, so it is paid once per
tree rather than per run. A cold matrix run is 313.6 s against the default arm's
109.6 s.

### The memory number, which is the uncomfortable one

The `net/http` prebuilt pack under the full pipeline peaks at **2.99 GiB RSS** and
takes about 240 s. The ceiling this project has worked against is 3 GiB, and the
module budget I deleted (commit 48200ab, "goc: bound large runtime optimization
memory") was introduced to hold three HTTP programs under it — they were at
1.2-1.3 GiB with the budget in place.

Two things keep this from being a regression to the state 48200ab fixed. The
per-*function* budget in `opt/budget.go` is untouched and is the one that
actually bounds the pathological cases: `gvn` (`opt/gvn.go:66`), `loadelim`
(`opt/loadelim.go:96`) and `simplifycfg`'s block coalescing (`opt/cfg.go:233`)
all skip any function over 200 blocks / 4000 temps / 1000 instructions — the
three passes that build the big per-function structures. (`mem2reg`, `inline`,
`gcm` and `dce` do not consult it; they do not build one.) And the whole
368-program matrix, both arms, passes on this box. But 2.99 against 3.00 is not
margin, and a box with a lower limit than this one will fail on the `net/http`
pack. **If a memory ceiling has to be honoured, the thing to reinstate is a
module budget on the pack build alone** (`internal/prebuilt.BuildRuntime`), not
on program modules — which is where the whole performance prize is.

### Where the compile time should be spent — a proposal, not a change

4.5x is large and I am not going to describe it as fine. Three observations, in
the order I would act on them:

1. **The `clean` fixpoint is 85% of the cost, and most of its work is
   re-examining functions nothing touched.** `DefaultPipeline` runs `clean` in
   four places plus twice inside the inliner fixpoints, and each `clean` is
   itself a fixpoint that re-runs seven passes over all 5101 functions until a
   whole round changes nothing. A function the previous round did not modify
   cannot have new opportunities in a purely intraprocedural pass. Tracking a
   dirty set per function across `Fixpoint.Run` — and seeding it from the
   inliner's list of modified callers between stages — is the change with by far
   the best ratio here, and it is a change to `opt/pass.go` alone.

2. **The module is 5101 functions, and about 4900 of them are stdlib the program
   does not call.** `deadfunc` (`DeadFuncElim`) runs *last*. Running a
   reachability pass first, before the first `clean`, would cut everything
   downstream by whatever fraction of the module is dead — which for a
   168-line program is most of it. The reason it is last today is presumably that
   inlining can make functions dead; that argues for running it at both ends, not
   only at the end.

3. **The prebuilt pack path already fixes this for the matrix and should be the
   default for corpus runs.** With packs warm the `-O` matrix costs the same as
   the default arm. The programs that hurt (the `net/http` and crypto ones, 4-5
   minutes each) are exactly the ones a pack exists for.

None of these is a reason to keep the pipeline off. The 4.5x buys a control loop
that goes from 1.63x the host toolchain to under 1.0 (below), and the matrix —
the thing that runs in CI — does not get slower once its packs are built. But if
somebody has to pay 4.5x on a laptop compiling one program, (1) is where to look.
