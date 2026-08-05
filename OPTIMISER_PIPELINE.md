# Fixpoint or ordered pipeline?

What LLVM, GCC and Go's `cmd/compile` do, why they do it, what cg12 does, and
what it costs — measured.

Written on `ccwork/optimiser-pipeline-research`, cut from `main` (5b085d2).
**No compiler behaviour is changed by this document's branch.** The alternative
pipelines it measures existed only as an uncommitted, env-gated patch, described
in [Appendix A](#appendix-a--what-was-measured-and-how). Everything below is
labelled either *read* (from source or a tool's own output) or *measured* (on
this box, commands given).

---

## 1. The short answer

| | global fixpoint? | where it iterates | what bounds it |
|---|---|---|---|
| **LLVM** | no | `cgscc(devirt<4>(inline,…))`, loop pass managers, worklists inside passes | a hard count (4), and a per-function "already simplified" marker |
| **GCC** | no | inside individual passes (`pass_fre` with `may_iterate`), IPA worklists | pass-internal; the pass list itself never repeats |
| **Go `cmd/compile`** | no | inside `applyRewrite`, per function | `itersLimit = max(NumBlocks, 20)`, then cycle detection and `Fatalf` |
| **cg12** | **yes, twice** | `Fixpoint("clean", …)` and `Fixpoint("inline-fixpoint", …)`, both **module-scoped** | nothing |

All three production compilers run a **fixed, ordered list**, and get the effect
of iteration by *writing the cleanup passes down again* at chosen points — 8
`instcombine`s in LLVM's `-O2`, 8 `dce`s in GCC's, 8 `deadcode`s in Go's. Where
they do iterate, the loop is small (one function, one pass, one SCC), bounded by
a constant, and justified by a specific enabling event.

cg12 iterates the *whole module* to convergence, unbounded. Measured on a real
compile, that costs about **2.1x** the compile time of the same passes run to the
same result — because cg12 tests convergence over the whole module while every
transform in the loop is per-function.

**The recommendation, up front:** keep the iteration, move its convergence test
inside the per-function loop (measured: byte-identical binaries, 2.1x faster),
then cap the two interprocedural fixpoints at 3 rounds (measured: another 11%,
binaries 0.01% smaller, no perf row beyond tolerance). That is 42.6 s → 17.9 s on
the reference compile with no measurable change in the code produced. Converting
the whole pipeline to a hand-ordered fixed list buys a further 1.23x and is not
worth its maintenance cost yet. Evidence in §5, reasoning in §3 and §7.

---

## 2. What each compiler actually does

### 2.1 LLVM

*Read, from the installed LLVM 18.1.3's own pipeline printer — this is this
box's actual `-O2`, not a description of it:*

```
$ opt-18 -passes='default<O2>' -print-pipeline-passes -disable-output empty.ll
annotation2metadata,forceattrs,inferattrs,coro-early,function<eager-inv>(lower-expect,
simplifycfg<…>,sroa<modify-cfg>,early-cse<>),openmp-opt,ipsccp,called-value-propagation,
globalopt,function<eager-inv>(mem2reg,instcombine<max-iterations=1;…>,simplifycfg<…>),
always-inline,require<globals-aa>,function(invalidate<aa>),require<profile-summary>,
cgscc(devirt<4>(inline,function-attrs<…>,openmp-opt-cgscc,function<eager-inv;no-rerun>(
sroa,early-cse<memssa>,…,loop-mssa(loop-instsimplify,loop-simplifycfg,licm,loop-rotate,
licm,simple-loop-unswitch),…,gvn<>,sccp,bdce,instcombine<…>,jump-threading,…)…),…),
deadargelim,…,function<eager-inv>(float2int,…,loop-vectorize<…>,…,slp-vectorizer,…,
loop-unroll<O2>,…),globaldce,constmerge,cg-profile,rel-lookup-table-converter,verify
```

It is one comma-separated sequence. The repetition is hand-placed:

| pass | appearances in `-O2` |
|---|---:|
| `instcombine` | 8 |
| `simplifycfg` | 8 |
| `sroa` | 3 |
| `jump-threading` | 2 |

**Is there iteration?** In exactly one place in the default pipeline, plus two
kinds of local iteration:

1. **`cgscc(devirt<4>(...))`** — `DevirtSCCRepeatedPass`. It repeats the CGSCC
   body *only when a pass turned an indirect call into a direct one*, at most 4
   times. `devirt<4>` is printed at `-O1`, `-O2` and `-O3` alike. Its doc comment
   (`/usr/lib/llvm-18/include/llvm/Analysis/CGSCCPassManager.h:545-558`) states
   the reason for the bound directly:

   > *"This repetition has the potential to be very large however, as each one
   > might refine a single call site. As a consequence, in practice we use an
   > upper bound on the number of repetitions to limit things."*

2. **The CGSCC walk itself revisits SCCs** as inlining reshapes the call graph —
   and LLVM explicitly *suppresses* redundant work when it does. The function
   simplification pipeline nested in the CGSCC walk is added with
   `/*NoRerun=*/true` (`llvm/lib/Passes/PassBuilderPipelines.cpp:922`, inside
   `buildInlinerPipeline`, which starts at :848; printed as
   `function<eager-inv;no-rerun>`), backed by
   `ShouldNotRunFunctionPassesAnalysis` (`CGSCCPassManager.h:531`), whose comment
   is the whole idea in one sentence:

   > *"This is used to prevent running an expensive function pass (manager) on a
   > function multiple times if SCC mutations cause a function to be visited
   > multiple times and the function is not modified by other SCC passes."*

   The pipeline says the same thing in a comment right above it: *"Mark that the
   function is fully simplified and that it shouldn't be simplified again if we
   somehow revisit it due to CGSCC mutations unless it's been modified since."*

3. **Worklists inside passes** — InstCombine is the classic one, and it is
   capped: `instcombine<max-iterations=1;no-use-loop-info;no-verify-fixpoint>` at
   `-O2`. Loop pass managers (`loop(...)`, `loop-mssa(...)`) walk a worklist of
   loops that transforms may add to, which is iteration bounded by the loop nest,
   not by convergence of the module.

**And there is no `repeat<N>` in the default pipelines.** LLVM *has* a generic
repeat adaptor — `opt-18 -passes='repeat<3>(instcombine)'` parses and runs — and
`grep -c 'repeat<'` over the printed `-O1`, `-O2` and `-O3` pipelines is
**0, 0, 0**. The facility exists and the default pipelines decline to use it.

### 2.2 GCC

*Read, from `gcc/passes.def` on the GCC 13 release branch (fetched), and from
this box's `gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1)`:*

`passes.def` is 540 lines containing **360 `NEXT_PASS` entries and no loop
construct of any kind**. Repetition is expressed by writing the pass down again;
`gen-pass-instances.awk` numbers the copies, which is why the local
`gcc -O2 -fdump-passes` (265 passes enabled) prints `tree-ccp1 … tree-ccp5`:

| pass | instances at `-O2` |
|---|---:|
| `dce` | 8 (`dce1…dce6`, plus `cddce1…cddce3`) |
| `fre` | 5 |
| `dse` | 5 |
| `copyprop` | 5 |
| `ccp` | 5 |
| `forwprop` | 4 |

Each repeat is placed where a *named* transform is known to leave debris —
`passes.def:242`:

```c
/* Threading can leave many const/copy propagations in the IL.
   Clean them up.  Failure to do so well can lead to false
   positives from warnings for erroneous code.  */
NEXT_PASS (pass_copy_prop);
```

```c
/* All unswitching, final value replacement and splitting can expose
   empty loops.  Remove them now.  */
NEXT_PASS (pass_cd_dce, false /* update_address_taken_p */);
```

Where GCC iterates, it is inside one pass and is a *parameter of that instance*:

```c
NEXT_PASS (pass_fre, true  /* may_iterate */);   /* early, in the main opt block */
…
NEXT_PASS (pass_fre, false /* may_iterate */);   /* late, after unrolling/vectorising */
```

The same pass is permitted to iterate at one point in the pipeline and forbidden
at another. There is no construct anywhere in `passes.def` that re-runs the list.

### 2.3 Go, `cmd/compile`

*Read, from the local toolchain source, `go1.26.1`:*

`$GOROOT/src/cmd/compile/internal/ssa/compile.go:457` —

```go
// list of passes for the compiler
var passes = [...]pass{
	{name: "number lines", fn: numberLines, required: true},
	…
	{name: "trim", fn: trim}, // remove empty blocks
}
```

59 entries, executed by `Compile(f *Func)` as a plain `for _, p := range passes`
(compile.go:439). **One traversal, no outer loop, no convergence test.** It is a
fixed ordered list, exactly as the brief says.

Repetition is spelled out, with a distinct name per instance so it can be talked
about: `deadcode` appears **8 times** (`early deadcode`, `pre-opt deadcode`,
`opt deadcode`, `gcse deadcode`, `generic deadcode`, `lowered deadcode for cse`,
`lowered deadcode`, `late deadcode`); the rewrite driver `opt` appears 3 times
(`opt`, `middle opt`, `late opt`); `cse` 3 times; `fuse`, `copyelim` and
`nilcheckelim` twice each.

The black magic is not only commented, it is **machine-checked**. `var passOrder
= [...]constraint{…}` (compile.go:526) is a list of "a must come before b" pairs
verified at startup, each with its reason:

```go
// prove relies on common-subexpression elimination for maximum benefits.
{"generic cse", "prove"},
// deadcode after prove to eliminate all new dead blocks.
{"prove", "generic deadcode"},
// cse substantially improves nilcheckelim efficacy
{"generic cse", "nilcheckelim"},
```

**Individual passes do iterate**, per function, and are bounded.
`applyRewrite` (`ssa/rewrite.go:41`) — the engine behind `opt`, `late opt` and
`lower` — is introduced with *"repeat rewrites until we find no more rewrites"*
and then:

```go
// if the number of rewrite iterations reaches itersLimit we will
// at that point turn on cycle detection. Instead of a fixed limit,
// size the limit according to func size …
itersLimit := f.NumBlocks()
if itersLimit < 20 { itersLimit = 20 }
```

and at the limit (rewrite.go:178):

> *"We've done a suspiciously large number of rewrites … As of Sep 2021, 90% of
> rewrites complete in 4 iterations or fewer and the maximum value encountered
> during make.bash is 12. Start checking for cycles."* — ending, if the state
> repeats twice, in `f.Fatalf("rewrite cycle detected")`.

That is the whole design in one function: iterate locally, expect ≤4 rounds,
bound it by function size, and treat non-convergence as a compiler bug rather
than as more work to do.

The `-d=ssa/…` machinery (`compile.go`'s `DebugSSA`, ~line 380) makes the fixed
list addressable: `-d=ssa/<phase>/<flag>[=n]` sets `on`, `off`, `time`, `mem`,
`stats`, `debug` or `dump` for one phase (or `all`, or a `~regexp`), and a phase
with `required: true` refuses to be turned off — *"Cannot disable required SSA
phase %s"*. Plus `GOSSAFUNC=name` to dump the IR after every phase as HTML. A
fixed list is what makes that possible: every phase has a stable name and runs
exactly once, so "it broke after `late opt`" is a well-formed statement.

---

## 3. The common design, and why

**The design.** A fixed, ordered, hand-tuned sequence, in which cleanup passes
appear several times at points chosen by experience; iteration exists only
*inside* a pass or over a small unit (a function, a loop, an SCC); every such
loop has a hard bound; and re-visiting work that has not changed is actively
suppressed.

**Why. Six reasons, in descending order of how much they matter to cg12.**

1. **A global fixpoint multiplies two unrelated quantities.** Convergence is a
   property of one function; the traversal is a property of the whole module.
   Looping "run every pass over every function until nothing anywhere changes"
   costs `rounds(slowest function) × cost(whole module)`. The last round is a
   full traversal of everything to discover that one function has settled. This
   is not a small constant: it is measured at 22.5% of cg12's optimiser below.
   LLVM's `NoRerun`/`ShouldNotRunFunctionPassesAnalysis` exists precisely to
   break that product, and Go's per-function `applyRewrite` loop never forms it.

2. **The returns fall off a cliff, and everyone has measured it.** Go's comment
   records 90% of rewrite loops finishing in ≤4 iterations with a maximum of 12
   over all of `make.bash`. LLVM caps devirtualisation repeats at 4. In cg12,
   everything after round 3 of any fixpoint accounts for **less than 0.25%** of
   the instructions that fixpoint changes (§5.3). A bounded number of rounds
   captures essentially all of the benefit; the rest is spent proving a negative.

3. **A fixpoint is not an optimum, so it does not actually buy what it looks
   like it buys.** Optimisation passes are not monotone operators on a lattice
   with a unique greatest fixpoint: inlining, GVN and code motion each destroy
   opportunities for the others, so the result still depends on the order. What
   "run to convergence" delivers is *a state in which these passes, in this
   order, find nothing more* — a much weaker property than "optimal", and one
   that a carefully chosen order reaches in one or two passes anyway. Once you
   accept that order matters, the honest engineering move is to choose the order
   and repeat only where a specific enabling event justifies it. LLVM does
   exactly that, and names the event: an indirect call became direct.

4. **Termination and predictable cost.** Any pair of passes that undo each other
   turns an unbounded fixpoint into an infinite loop, and any pathological input
   turns it into a compile-time cliff. Go detects the cycle and calls it a
   compiler bug (`rewrite cycle detected`); LLVM offers
   `-abort-on-max-devirt-iterations-reached` and otherwise just stops. Build
   systems need a worst case, and an unbounded loop does not have one.

5. **Analysis caches.** Dominators, alias info, loop info are expensive and are
   cached per function. Re-traversing the module invalidates and rebuilds them
   for functions that did not change. Everything in LLVM's new pass manager —
   `eager-inv`, `no-rerun`, `require<>`/`invalidate<>` — is bookkeeping to avoid
   exactly the recomputation a naive fixpoint forces.

6. **Debuggability and bisection.** `-d=ssa/late_opt/off`, `-fdump-passes`,
   `opt -passes=…` all rest on every pass instance having a stable identity.
   "`ccp3` miscompiled it" is actionable; "round 5 of the clean fixpoint" is not
   reproducible in the same way, because the round a change lands in depends on
   what every other function did.

**What a fixpoint does buy, and it is not nothing:** you do not have to know the
pass interaction graph. Whoever writes an ordered pipeline must know that fold
enables copy which enables GVN which exposes another jump-thread. A fixpoint
finds that out at runtime, every time, correctly, for pass sets nobody has tuned.
For a compiler under construction, whose pass set changes month to month, that is
a real and defensible trade — as long as its cost is understood.

---

## 4. What cg12 does

`opt.DefaultPipeline` (`opt/pass.go:109`) is **already an ordered list of 16
entries**. What re-converges are two nested constructs:

- **`clean`** = `Fixpoint(fold, copy, loadelim, deadalloc, gvn, jumpthread,
  simplifycfg, dce)` — 8 per-function passes. It appears at 4 places in the
  pipeline and twice more inside the inline fixpoints, so one compile enters it
  **13 times**.
- **`inline-fixpoint`** = `Fixpoint(inline, clean)`, twice.

`Fixpoint.Run` (`opt/pass.go:80`) is:

```go
for {
	round := false
	for _, p := range fp.passes {
		if p.Run(m) { round = true }
	}
	if !round { return any }
	any = true
}
```

and each member is a `FuncPass`, whose `Run` walks **every function in the
module** (`opt/pass.go:33`). So a round is: 8 full-module traversals; and rounds
repeat until no function anywhere changed.

That is the crucial detail, and it is the one that differs from all three
reference compilers: **the transforms are per-function but the convergence test
is per-module.** A single stubborn function forces another traversal of all
5083.

---

## 5. Measurements

Program: `goc -O -o out goc/testdata/fmt_sprintf.go` — an 8-line program whose
module is the stdlib closure: **5083 functions with bodies, 69,977 blocks,
367,110 instructions** at pipeline entry (traced). Host: this box, arm64
(Neoverse-N1, 64 cores, 250 GB), otherwise idle, `go1.26.1`.

### 5.1 How long the fixpoint runs

Instrumented compile: 42.8 s wall / 82.1 s user, of which **the optimiser is
36.28 s** across 421 pass invocations.

| fixpoint | entries | rounds each | total rounds |
|---|---:|---|---:|
| `clean` | 13 | 7, 6, 7, 4, 4, 3, 4, 1, 2, 3, 4, 1, 4 | **50** |
| `inline-fixpoint` | 2 | **7**, **3** | 10 |

50 `clean` rounds × 8 passes = **400 whole-module traversals** of 5083 functions.

So the answer to "how many rounds does it take" is: **`clean` takes 1 to 7,
typically 3–4; the first inline fixpoint takes 7 and the second 3.** It is not
"round two rarely changes anything" and it is not six-rounds-everywhere; it is
in between, and — as §5.3 shows — the rounds past the third are almost empty and
cost exactly as much as the full ones.

### 5.2 A round costs the same whether it does work or not

`clean` instance #5, one row per round:

| round | functions changed | cost |
|---|---:|---:|
| 1 | 92 | 662 ms |
| 2 | 4 | 650 ms |
| 3 | 4 | 664 ms |
| 4 | **0** | 658 ms |

The cost of a round *is* the traversal. This is the mechanism behind everything
else here:

- rounds ≥ 2 of `clean`: **22.25 s of the 33.20 s** `clean` spends — 67%;
- the 13 rounds that changed **nothing at all** (one per entry, the convergence
  proof) cost **8.16 s = 22.5% of the whole optimiser**;
- the inliner itself is cheap — **1.34 s over 10 invocations**. The 7-round
  inline fixpoint is expensive only because each round drags a `clean` fixpoint
  behind it.

### 5.3 What the second and later rounds change

Instructions added or removed per round (module instruction count, traced):

| fixpoint | r1 | r2 | r3 | r4 | r5 | r6 | r7 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `clean#1` | −50,247 | −7,506 | −576 | −35 | −9 | −4 | 0 |
| `inline-fixpoint#1` | **+334,222** | +19,639 | +3,001 | +661 | +65 | +25 | 0 |
| `clean#2` | −101,735 | −8,067 | −1,046 | −16 | −1 | 0 | |
| `clean#13` | −9,440 | −9 | −1 | 0 | | | |

Round 2 is worth having: it is 6–15% of round 1 (in `clean#1`, 7,506
instructions and 3,178 function-level changes — `fold` alone fires on 1,087
functions). Round 3 is worth about a tenth of round 2. **Rounds 4 and beyond are
noise**: in `inline-fixpoint#1` they inline 751 instructions out of the 357,613
that fixpoint adds — **0.21%** — and cost 8.26 s of its 22.31 s, i.e. **37% of
the time for 0.21% of the work.**

### 5.4 The arms: fixed order vs. convergence

Same compile, six pipelines, 3 repetitions each (reps agree to within 1.5%; means
shown) except `perfunc3`, which is one repetition. `full` is `main`'s behaviour.

| arm | what it is | wall (s) | user CPU (s) | output binary |
|---|---|---:|---:|---|
| `full` | `DefaultPipeline`, as shipped | **42.58** | 82.13 | 13,932,592 B (reference) |
| `ordered` | every fixpoint collapsed to **one** traversal | **14.61** | 38.56 | 13,973,512 B (+0.29%) |
| `ordered2` | hand-ordered, `clean` twice at chosen points (§6) | **16.91** | 42.04 | 13,920,456 B (−0.09%) |
| `perfunc` | same passes, convergence moved **inside the per-function loop** | **20.24** | 49.33 | **byte-identical to `full`** |
| `perfunc3` | `perfunc` + the inline fixpoints capped at 3 rounds | **17.92** | 44.66 | −904 B (−0.006%) |
| `bounded` | fold/copy/dce only (the pre-2026-08 default) | 6.84 | 20.70 | 10,611,496 B |

**The `perfunc` row is the finding of this job.** `clean`'s members are all pure
per-function transforms — `Fold`, `Copy`, `LoadElim`, `DeadAlloc`, `GVN`,
`SimplifyCFG`, `DCE` take a `*ir.Func` and nothing else, and `JumpThreadPass`
keeps only per-function state (`map[*ir.Func]*jtState`) — so converging one
function at a time before moving to the next produces the same program as
converging the module. That is not an argument, it is a measurement: the output
binary is **byte-identical** (sha256 `910587597863cd52…`, identical size), while
wall time falls 42.58 s → 20.24 s (**2.10x**) and CPU 82.13 s → 49.33 s
(**1.66x**).

In other words, **half of cg12's fixpoint cost is not the fixpoint at all — it
is testing convergence at the wrong granularity**, and removing it costs nothing
whatsoever in code quality.

### 5.5 Code quality: `make bench-perf` under the ordered pipeline

`GOC_OPT_PIPELINE=ordered make bench-perf` — 11 programs, 42 rows, 9 interleaved
repetitions pinned to core 62, 547 s. The committed baseline
(`goc/testdata/perf_suite_baseline.txt`) *is* the full-pipeline reference: it was
re-cut on the tree with the full pipeline on (`c7171de`, then `bb66b35` on the
merged tree, "on an idle box"), and the gated number is a goc/host ratio formed
inside one repetition, so machine speed divides out and the file is comparable
across runs by construction.

**Every one of the 42 rows came back `within tolerance`.** The biggest movements,
with the "resolved" column (the part of the change that survives both intervals —
negative means indistinguishable from zero):

| row | baseline (fixpoint) | ordered | change | resolved | tol |
|---|---:|---:|---:|---:|---:|
| `sortmap map/build-probe` | 6.0921 | 5.3962 | −11.4% | +4.1% | 14.5% |
| `regexp regexp/replace` | 5.9579 | 5.7107 | −4.1% | +0.0% | 10.7% |
| `regexp regexp/find-submatch` | 6.4397 | 6.2096 | −3.6% | +3.3% | 5.0% |
| `conc chan/send-buffered` | 2.8896 | 2.7914 | −3.4% | +3.1% | 5.0% |
| `flate flate/decompress` | 4.7187 | 4.5695 | −3.2% | +2.7% | 5.0% |
| `text text/format-append` | 7.6592 | 7.4131 | −3.2% | −5.6% | 12.8% |
| `text text/parse` | 7.7207 | 7.8860 | **+2.1%** | +1.7% | 5.0% |
| `json json/marshal` | 14.7342 | 14.9770 | **+1.6%** | −0.7% | 5.5% |
| `interp interp/bytecode-loop` | 19.0659 | 18.9109 | −0.8% | +0.7% | 5.0% |

(lower is better; ratio = goc ns / host ns.) Movements go both ways and are
mostly not resolvable from zero. The eleven `control/spin-fixed-work` rows —
byte-identical source in every program — came back 0.9246 to 0.9265 against a
baseline range of 0.9247 to 0.9284, i.e. the instrument and the box agree with
the day the baseline was cut.

**So: on this instrument, the code the ordered pipeline produces is
indistinguishable from the code the fixpoint produces, at 2.9x less compile
time.** Two honest limits on that sentence. First, sensitivity: per-row
`detect%` runs from 5.0% to 45%, so this suite would not see a uniform 2-3%
regression, and the binary *is* 0.29% larger. Second, the run failed its noise
gate — not on any ratio, but because `chase/pointer-node`'s one-repetition spread
was 13.65% against 1.27% in the baseline, and that is my fault: I ran a
`go build` and a `goc` compile on the box during its first two repetitions.
`chase` is the memory-latency workload and is the one that would notice. The
verdict table above is unaffected in direction (every row passed *despite* the
extra noise, and noise can only hide a difference, not manufacture agreement),
but the `chase/*` rows specifically should be read as "not measured cleanly".

### 5.6 The `perfunc` result holds on four programs, and bounding the rest

`perfunc` was checked against `full` on four whole-program builds, one
compilation each, comparing the linked binaries byte for byte:

| program | `full` wall / user | `perfunc` wall / user | binaries |
|---|---:|---:|---|
| `fmt_sprintf.go` | 41.59 / 82.41 | 20.21 / 48.48 | **identical** |
| `placement_bench/interp` | 42.32 / 80.71 | 21.05 / 48.62 | **identical** |
| `placement_bench/json` | 51.58 / 99.99 | 25.41 / 60.83 | **identical** |
| `placement_bench/flate` | 46.80 / 88.98 | 22.12 / 52.60 | **identical** |

2.0–2.1x on wall, 1.66–1.70x on CPU, byte-identical output every time.

`perfunc3` adds a 3-round cap on the two inline fixpoints, on top of `perfunc`:

| program | `perfunc3` wall / user | vs `full` | binary vs `full` |
|---|---:|---:|---|
| `fmt_sprintf.go` | 17.92 / 44.66 | 2.4x / 1.8x | −904 B (−0.006%) |
| `placement_bench/interp` | 18.02 / 44.41 | 2.3x / 1.8x | −912 B (−0.007%) |
| `placement_bench/json` | 22.45 / 54.93 | 2.3x / 1.8x | −1,704 B (−0.011%) |
| `placement_bench/flate` | 19.49 / 47.90 | 2.4x / 1.9x | −1,240 B (−0.008%) |

The cap is worth another 11% of wall time over `perfunc` and changes the binary
by about a hundredth of a percent, in the direction of *less* code — which is
what §5.3 predicts, since what rounds 4-7 add is a few dozen instructions of
tail-end inlining. §5.7 measures whether that hundredth of a percent is visible
in the perf suite.

### 5.7 Code quality under the recommended arm (`perfunc3`)

`GOC_OPT_PIPELINE=perfunc3 make bench-perf`, 42 rows, 9 repetitions, 589 s.
**No row moved beyond its baseline tolerance** (checked row by row against
`perf_suite_baseline.txt`; 0 of 42 over). Largest movements:

| row | fixpoint | `perfunc3` | change | tol |
|---|---:|---:|---:|---:|
| `text text/format-append` | 7.6592 | 7.1840 | −6.2% | 12.8% |
| `sortmap map/build-probe` | 6.0921 | 5.7513 | −5.6% | 14.5% |
| `text text/sprintf` | 6.6966 | 6.8765 | **+2.7%** | 16.6% |
| `gcpress gc/alloc-churn` | 5.9451 | 5.8320 | −1.9% | 9.5% |
| `regexp regexp/anchored-lines` | 4.0798 | 4.0215 | −1.4% | 5.0% |
| `json json/marshal` | 14.7342 | 14.8902 | **+1.1%** | 5.5% |
| `interp interp/bytecode-loop` | 19.0659 | 19.0842 | +0.1% | 5.0% |

Control rows 0.9252–0.9286 against a baseline range of 0.9247–0.9284.

This run also exited FAIL on the noise gate, and this time it was not my doing:
the box's 1-minute load average reached 20.5 during it (this is a shared
64-core machine and other jobs started), and two rows —
`sortmap map/build-probe` at 15.0% and `chase/pointer-node` at 16.4% — exceeded
the suite's absolute 15% spread ceiling, which aborts before the verdict table is
printed. The per-row ratios above are from the run's own table and the comparison
was done by hand against the committed baseline; the two loud rows should be
discounted, and neither is near its tolerance anyway.

**Reading §5.5 and §5.7 together:** three pipelines — `full` (fixpoint),
`ordered` (one traversal of everything), `perfunc3` (per-function convergence,
inline capped at 3) — produce code this suite cannot tell apart, across 42
workload rows, while the compile takes 42.6 s, 14.6 s and 17.9 s respectively.

---

## 6. What an ordered cg12 pipeline would look like

If the fixpoints were removed entirely, the round data above says where the
repeats have to go. `clean`'s second round is worth 6–15% of its first and its
third about a tenth of that; the inliner's second round is worth 5.9% of its
first and its third 0.9%. So:

```
mem2reg
clean ; clean                     # r2 of clean#1 was -7,506 instrs: keep two
inline ; clean ; clean            # inline r2 = +19,639: keep two inline rounds
inline ; clean
mem2reg                           # promotes what inlining dissolved
clean
unroll
inline ; clean                    # inline what unrolling exposed (r2 there = +191, drop it)
constantp
ifconvert
clean ; clean
tailmerge ; simplifycfg ; dce
deadfunc
gcm ; dce
```

One reason the inliner genuinely needs more than one round, which is easy to miss
and is not about exposing new opportunities: `inlineFuncBudget = 64`
(`opt/inline.go:52`) caps how many call sites are inlined into one function *per
pass*, and its own comment says "the pipeline's fixpoint applies more on later
rounds if they remain worthwhile". So a function with more than 64 inlinable
sites — an interpreter dispatch loop is exactly that — needs round 2 to finish
what round 1 was capped out of. Any ordered or round-capped design inherits that:
three inline rounds is 192 sites per function, and if that is not enough for
some function the budget is the thing to raise, not the round count.

The `ordered2` arm above is this pipeline: 16.91 s (2.5x faster than `full`), and
a binary 0.09% *smaller* than the fixpoint's. Which passes genuinely need to run
twice, from the round-2 firing counts in `clean#2`: `fold` (1,139 functions),
`dce` (1,052), `simplifycfg` (920), `jumpthread` (859), `gvn` (478) — i.e.
almost all of the clean set, which is why the unit of repetition should be the
whole set and not individual passes. `copy`, `loadelim` and `deadalloc` mostly
fire in round 1 only (1, 8 and 6 functions respectively in round 2).

The strict single-traversal arm (`ordered`, 14.61 s) is 14% faster than
`ordered2` and produces a 0.38% larger binary than it. The second `clean`
traversal is cheap and evidently pays for itself in code size.

---

## 7. Recommendation

**Keep the iteration. Fix its granularity. Then bound it.** In that order, and
the first step is not a trade-off at all.

1. **Move `clean`'s convergence test inside the per-function loop** (the
   `perfunc` arm). Measured: **byte-identical output on all four programs
   tried**, 2.0–2.1x less wall time, 1.66–1.70x less CPU. There is no quality
   argument to have, because there is no quality difference. This is what LLVM's
   `ShouldNotRunFunctionPassesAnalysis` and Go's per-function `applyRewrite` loop
   are; cg12 is the only one of the four that re-walks 5,083 functions to learn
   that one of them changed.

2. **Bound what remains.** After (1), the residual iteration is the two
   `inline-fixpoint`s (7 and 3 rounds), which are genuinely interprocedural and
   genuinely need more than one round — but not seven: rounds 4-7 buy 0.21% of
   the inlining for 37% of that fixpoint's time. Cap them at 3, as LLVM caps
   `devirt` at 4 and Go caps `applyRewrite` at `max(NumBlocks, 20)`. Measured
   (§5.6, §5.7): another 11% off the wall time, binaries 0.006–0.011% *smaller*,
   and **no perf row beyond tolerance in 42**. Together (1)+(2) take the
   reference compile from 42.6 s to 17.9 s — **2.4x** — with the perf suite
   unable to tell the results apart.

3. **Do not convert the pipeline to a strict fixed list**, on this evidence. The
   `ordered` arm is 2.9x faster than `full` — but only **1.23x** faster than
   (1)+(2), and it is the only arm that made the binary bigger (+0.29%). Buying
   that last 23% means hand-maintaining a pass-interaction order — the thing
   LLVM, GCC and Go each spent decades tuning and still annotate line by line —
   while cg12's pass set is still changing month to month. Revisit when the pass
   set stabilises; the round data in §5.3 is the input that design would need,
   and §6 is a first draft of it that has already been measured.

**The trade-off, stated plainly.** Recommendation (1) is free and should be done.
Recommendation (2) trades a measured ~0.2% of inlining opportunity for a
measured fraction of compile time, and it makes the compiler's worst case
bounded, which it currently is not — an unbounded module-wide fixpoint has no
upper bound on rounds, and cg12 has no cycle detection of the sort Go has, so a
future pass pair that undoes each other's work hangs the compiler instead of
failing it. Recommendation (3) is where the actual "black magic" trade lives, and
the evidence does not support paying its maintenance cost yet.

**And the important caveat about where the money is.** Even the fastest arm here
(`ordered`, 14.61 s) is 2.1x the `bounded` floor (6.84 s), and the previously
measured corpus multiplier is 4.469x. The fixpoint is *not* the whole story:
skipping the inliner alone removes 85% of the cost above the floor, and mem2reg's
28% is entirely indirect (it exposes inlining opportunities; skipping it with the
inliner already off makes compiles *slower*). Those numbers say the cost is the
total work the inliner creates, not only the re-convergence. The two fixes are
independent: §5.4's arms cut re-traversal, and an inliner cost model would cut
the work. Doing (1) and (2) takes the reference compile from 42.6 s to a measured
17.9 s; getting below *that* means inlining less, not iterating less. If the
corpus multiplier scales the same way — not measured here — 4.469x would land
near 1.9x, and the rest of the gap is the inliner's own output.

---

## Appendix A — what was measured, and how

**Read** (no measurement involved): LLVM's `CGSCCPassManager.h` (installed
header) and `PassBuilderPipelines.cpp` (fetched, release/18.x); GCC's
`passes.def` (fetched, releases/gcc-13); Go's `ssa/compile.go` and
`ssa/rewrite.go` (local `go1.26.1` source). Quotations are verbatim.

**Tool output, on this box** (their own reports, not my interpretation):
`opt-18 -passes='default<O{1,2,3}>' -print-pipeline-passes -disable-output`;
`gcc -O2 -fdump-passes`.

**Measured, on this box:** every number in §5. The instrumentation was an
uncommitted patch to `opt/pass.go` and a temporary `opt/trace.go` adding
(a) `GOC_OPT_TRACE=1`, which reports per pass invocation the round, the number
of functions changed, the elapsed time and the module instruction count, and
(b) four extra `GOC_OPT_PIPELINE` arms — `ordered`, `ordered2`, `perfunc`,
`perfunc3` — built from the *same pass functions in the same order* as
`DefaultPipeline`, differing only in where convergence is tested. Default
behaviour was untouched (`GOC_OPT_TRACE` unset ⇒ one boolean test; the new arms
are unreachable without `GOC_OPT_PIPELINE`). The patch is **not** part of this
branch; `git diff main` on the delivered tree is this document alone.

Two `make bench-perf` runs were done, under `ordered` and under `perfunc3`, 9
repetitions each, ~9.5 minutes each. **Both exited FAIL on the suite's noise
gate rather than on any ratio** — see §5.5 and §5.7 for which rows and why (mine
in the first case, other jobs on this shared box in the second). Every row in
both runs is within its baseline tolerance; the `perfunc3` comparison was done by
hand because the noise gate aborts before the verdict table prints.

**Not measured, and so not claimed:** whether the `perfunc` byte-identity holds
on every program in the corpus (four whole-program builds were checked, §5.6);
whether the corpus-wide 4.469x multiplier falls by the same factor (only
single-program compiles were timed here); whether these arms preserve the
allocation census, the escape differentials or determinism (the brief excluded
running those suites, and no arm here is proposed for adoption without them);
and anything about `bounded`, which this job did not touch.

**Reproducing any of it.** The scaffolding is small: `collapse()` over
`DefaultPipeline` gives `ordered`; `perfunc` is `DefaultPipeline` with the
`clean` fixpoint replaced by a `FuncPass` that loops the same eight transforms
on one function until that function stops changing; `perfunc3` adds a 3-round
cap to the two `Fixpoint("inline-fixpoint", …)` wrappers. The round data comes
from printing, per pass invocation, the fixpoint instance, the round number, the
count of functions the pass changed, the elapsed time, and the module's
instruction count before and after.
