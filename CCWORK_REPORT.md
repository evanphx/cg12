# Optimiser pipeline research — fixpoint vs. ordered

Branch `ccwork/optimiser-pipeline-research`, cut from `main` (5b085d2).
Deliverable: `OPTIMISER_PIPELINE.md`. No compiler behaviour changed in the
committed tree; measurement scaffolding was applied as an uncommitted patch and
removed (see "Scaffolding" at the end).

_(run in progress — sections appended as each result lands)_

## 1. What the three compilers actually do — read, not recalled

All three are **fixed ordered pass lists**. None re-converges a whole pipeline.
Each contains bounded, *local* iteration, in a different place.

### LLVM — fixed sequence, one bounded repeat, plus per-function memoisation

Evidence is the installed LLVM 18.1.3's own pipeline printer, so it is this
box's actual `-O2`, not a description of it:

    opt-18 -passes='default<O2>' -print-pipeline-passes -disable-output empty.ll

One comma-separated sequence. Pass-name frequencies in it:

| pass | times it appears in `-O2` |
|---|---:|
| `instcombine` | 8 |
| `simplifycfg` | 8 |
| `sroa` | 3 |
| `jump-threading` | 2 |

That is the black-magic pipeline in its purest form: the cleanup passes are
*written down* at eight chosen points rather than looped over.

- **No `repeat<N>` anywhere.** LLVM *has* a `repeat<N>` pipeline adaptor —
  `opt-18 -passes='repeat<3>(instcombine)'` parses and runs — and the default
  pipelines do not use it: `grep -c 'repeat<'` over the printed `-O1`, `-O2` and
  `-O3` pipelines is **0, 0, 0**.
- **The one iteration is `cgscc(devirt<4>(inline,...))`** — `DevirtSCCRepeatedPass`,
  which repeats the CGSCC body only when a pass turned an indirect call into a
  direct one, at most 4 times (`devirt<4>` is printed at both `-O2` and `-O3`).
  Its doc comment in `llvm/Analysis/CGSCCPassManager.h` states the reason for the
  bound outright: *"This repetition has the potential to be very large however, as
  each one might refine a single call site. As a consequence, in practice we use
  an upper bound on the number of repetitions to limit things."*
- **Re-visits are memoised per function.** The CGSCC walk can revisit an SCC as
  the call graph changes, so the function simplification pipeline nested inside it
  is added with `/*NoRerun=*/true` (`PassBuilderPipelines.cpp`, in
  `buildInlinerPipeline`), backed by `ShouldNotRunFunctionPassesAnalysis`, whose
  comment is: *"This is used to prevent running an expensive function pass
  (manager) on a function multiple times if SCC mutations cause a function to be
  visited multiple times and the function is not modified by other SCC passes."*
  In the printed pipeline this is the `function<eager-inv;no-rerun>(...)` wrapper.
- `instcombine<max-iterations=1;...>` — even InstCombine, the classic worklist
  peephole, is *capped*, and at `-O2` runs one iteration per invocation.

So LLVM's answer is: fixed order; iterate only where a *specific* interprocedural
event (devirtualisation) can unlock more; bound that iteration at 4; and never
re-run a function's simplification if the function has not changed.

### GCC — a straight list, repetition written out by hand

`gcc/passes.def` (GCC 13 branch, fetched): 540 lines, **360 `NEXT_PASS` entries,
no loop of any kind**. Repetition is expressed by writing the pass again;
`gen-pass-instances.awk` numbers the copies, which is why the local
`gcc -O2 -fdump-passes` shows `tree-ccp1 … tree-ccp5`.

From this box's `gcc (Ubuntu 13.3.0) -O2 -fdump-passes` (265 passes enabled):

| pass | instances at `-O2` |
|---|---:|
| `dce` | 8 (`dce1…dce6`, `cddce1…`) |
| `fre` | 5 |
| `dse` | 5 |
| `copyprop` | 5 |
| `ccp` | 5 |
| `forwprop` | 4 |

And where GCC does iterate, it is again *inside* one pass and explicitly flagged:
`NEXT_PASS (pass_fre, true /* may_iterate */)` early in the pipeline versus
`NEXT_PASS (pass_fre, false /* may_iterate */)` late — the same pass, permitted
to iterate at one point and forbidden at another.

### Go `cmd/compile` — a Go array, and a constraint list instead of a loop

`$GOROOT/src/cmd/compile/internal/ssa/compile.go:457`, `var passes = [...]pass{…}`
— 59 entries, run by `Compile(f *Func)` as `for _, p := range passes { … }`
(compile.go:439). One traversal, no outer loop, no convergence test.

Repetition is again spelled out: `deadcode` appears 8 times under 8 different
names (`early deadcode`, `pre-opt deadcode`, `opt deadcode`, `gcse deadcode`,
`generic deadcode`, `lowered deadcode for cse`, `lowered deadcode`, `late
deadcode`), the rewrite driver `opt` 3 times (`opt`, `middle opt`, `late opt`),
`cse` 3 times, `fuse` and `copyelim` and `nilcheckelim` twice each.

The *reasons* are checked, not just commented: `var passOrder = [...]constraint{…}`
(compile.go:526) is a machine-verified list of "a must come before b" pairs with
the black magic written next to each — *"prove relies on common-subexpression
elimination for maximum benefits"*, *"deadcode after prove to eliminate all new
dead blocks"*, *"cse substantially improves nilcheckelim efficacy"*.

Individual passes do iterate. `applyRewrite` (ssa/rewrite.go:41) — the engine
behind `opt`/`late opt`/`lower` — says *"repeat rewrites until we find no more
rewrites"*, and bounds itself: `itersLimit := f.NumBlocks()` (min 20), after which
it turns on cycle detection and can `f.Fatalf("rewrite cycle detected")`. The
comment records the empirical distribution: *"As of Sep 2021, 90% of rewrites
complete in 4 iterations or fewer and the maximum value encountered during
make.bash is 12."* That iteration is **per function and per pass**, not per module.

`-d=ssa/<phase>/<flag>[=value]` (compile.go:~380) turns individual phases
`on`/`off`, or asks for `time`, `mem`, `stats`, `dump`; phases marked
`required: true` refuse to be disabled. So the list is fixed but individually
addressable — which is what makes a fixed list debuggable.

## 2. cg12's fixpoint, measured

`opt.DefaultPipeline` is **already an ordered 16-entry list**; what re-converges
are two nested constructs — `clean` (8 local passes) and `inline-fixpoint`
(inline + clean). Both converge at **module** granularity: a round is a full
traversal of all 5101 functions by each member pass, repeated until no function
anywhere changed.

Instrumented compile, `goc -O -o out goc/testdata/fmt_sprintf.go`, arm64,
otherwise idle box. 42.8 s wall / 82.1 s user, of which the optimiser is
**36.28 s** in 421 pass invocations.

**Rounds.** `clean` is entered 13 times and takes **1 to 7 rounds** (50 rounds
total = 400 whole-module traversals). The two `inline-fixpoint`s take **7 rounds**
and **3 rounds**.

**What later rounds do.** Round 1 of the first `clean` changes 10288
function-instances; round 4 changes 15; rounds 5, 6 change 4 and 3; round 7
changes 0. That shape repeats everywhere: the tail rounds touch single-digit
numbers of functions out of 5101.

**What later rounds cost.** A round costs the same whether it changes 8625
functions or none, because the cost *is* the traversal:

| `clean` instance #5 | funcs changed | round cost |
|---|---:|---:|
| round 1 | 92 | 662 ms |
| round 2 | 4 | 650 ms |
| round 3 | 4 | 664 ms |
| round 4 | **0** | 658 ms |

- rounds ≥ 2 of `clean`: **22.25 s of the 33.20 s** `clean` spends (67 %).
- the 13 rounds that changed **nothing at all** — pure convergence proof —
  cost **8.16 s, 22.5 % of the entire optimiser**.
- the inliner itself is cheap: 1.34 s over 10 invocations. `inline-fixpoint#1`'s
  7 rounds are expensive only because each drags a whole `clean` fixpoint behind it.

**What later rounds change** (module instruction count, second traced run):

| fixpoint | r1 | r2 | r3 | r4 | r5 | r6 | r7 |
|---|---:|---:|---:|---:|---:|---:|---:|
| `clean#1` | −50,247 | −7,506 | −576 | −35 | −9 | −4 | 0 |
| `inline-fixpoint#1` | **+334,222** | +19,639 | +3,001 | +661 | +65 | +25 | 0 |
| `clean#2` | −101,735 | −8,067 | −1,046 | −16 | −1 | 0 | |

Rounds 4-7 of the first inline fixpoint inline **751 instructions of the 357,613
that fixpoint adds — 0.21% — for 8.26 s of its 22.31 s (37%)**.

## 3. The arms — same passes, different convergence granularity

`goc -O -o out goc/testdata/fmt_sprintf.go`, 3 reps each, means; reps agree to
1.5%. `full` is `main`'s behaviour.

| arm | wall (s) | user (s) | output |
|---|---:|---:|---|
| `full` (`DefaultPipeline`) | **42.58** | 82.13 | 13,932,592 B |
| `ordered` (every fixpoint → one traversal) | **14.61** | 38.56 | +0.29% |
| `ordered2` (hand-ordered, `clean` twice at chosen points) | **16.91** | 42.04 | −0.09% |
| `perfunc` (convergence inside the per-function loop) | **20.24** | 49.33 | **byte-identical to `full`** |
| `bounded` (fold/copy/dce) | 6.84 | 20.70 | −23.8% |

**`perfunc` is the result.** Every member of `clean` is a pure per-function
transform (`JumpThreadPass` keeps only `map[*ir.Func]*jtState`), so converging
one function before moving to the next gives the same program — measured, not
argued: identical sha256 and size, at **2.10x less wall time and 1.66x less
CPU**. Half of cg12's fixpoint cost is not the fixpoint; it is testing
convergence at module granularity when every transform is per-function.

_(`make bench-perf` under `ordered` in progress; capped arm to follow)_
