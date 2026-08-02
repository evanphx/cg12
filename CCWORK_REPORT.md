# Cross-function escape summaries for `opt.LowerHeapAllocations`

Branch `ccwork/escape-summaries`, off `main` (`05946f2`), with
`ccwork/escape-alloc-census` cherry-picked on top (3 commits: `5041f9a`,
`20e0f61`, `e59e32f` — the tree is byte-identical to
`origin/ccwork/escape-alloc-census` apart from this file).

Status: IN PROGRESS. Numbers appear here as they are produced, and every one of
them is watched to completion. Anything not watched is marked UNVERIFIED.

## 0. The premise, read not re-derived

From `ESCAPE_IR_PLAN.md` and `CCWORK_REPORT.md` on `ccwork/escape-ir-design`
(`667c667`), measured at `ddd03eb`:

| | |
|---|---|
| heap-capable allocations already neutral (`ir.OHeapAlloc`) | **91.6%** (456 631 of 498 696) |
| `LowerHeapAllocations` promotion rate | **3.1%** — 14 433 promoted / 453 319 lowered |
| AST-walk committed-frame placements the IR pass would get wrong | **11 255** |
| corpus allocation census, 385 programs | frame **9 735 471** / heap **509 920** |
| AST escape walk, `stdlib_crypto_ecdsa.go` | **0.456 s of a 6.47 s compile = 7.1%** |
| of which parent-map rebuilding | ~0.28 s (60% of the walk) |
| `astParents` rebuilds for summaries, same program | 12 612 calls over 1 713 256 AST nodes = **3.91×** the whole program's AST |
| distinct summary questions / queries | 1 485 distinct, asked **8.51×** each |
| `opt.FrameEscapes` (whole-module IR dataflow, same 6 965 functions) | **0.134 s** |

That last pair is the budget: an IR-level whole-module fixed point over the same
functions costs 0.134 s where the AST walk costs 0.456 s.

---

## 1. What was built

Four new files in `opt/`, one new attribute in `ir/`, and a diagnostic record the
front end writes. Nothing in the compiler's default path reads any of it.

| file | what |
|---|---|
| `opt/escapefacts.go` | `EscapeFacts`: the per-parameter summary table, and the bottom-up SCC solve that fills it |
| `opt/escapegraph.go` | the per-function weighted location graph and its Bellman-Ford solve — the analysis that produces one function's summary |
| `opt/escapesummary.go` | the consumer: what `LowerHeapAllocations` does with a summary at a call |
| `opt/escapeshadow.go` | `ShadowPlacement`: run the IR analysis over the allocations goc's AST walk placed, and report the disagreements |
| `ir/func.go` | `SymNoEscape` (carries `//go:noescape`) and `Func.PlacedAllocs` (the front end's own placements) |
| `goc/compile.go` | `registerNoEscapeDirectives` and `recordPlacement` — both diagnostic only |

### 1.1 Dereference depth, not a boolean

Locations are this function's parameters and temporaries, each frame slot it
writes through, one node per result, and one node for the heap. An edge
`dst <- src` with weight k means "the value obtained by dereferencing src k
times is assigned to dst": 0 for a copy, **-1 for an address**, **+1 for a
load**. A location escapes when the heap node reaches it at a net depth below
zero — when its *address* flows to the heap. A parameter's summary is the
smallest depth at which the heap, or a result, reaches it.

That is what makes `global = *p` and `global = p` different answers, which a
boolean cannot express, and it is the same distinction `addressedVariableIdentifier`
makes in `goc/compile.go:3174`. `opt/escapefacts_test.go` states both as IR:
`TestEscapeFactsPointeeEscapesButPointerDoesNot` is the first, and it fails for
any boolean formulation.

The `-1` edge is also what connects goc's memory-shaped frontend variables to
the value graph: an `OAlloc` result is the *address* of a slot, so
`alloc; store p, slot; load slot` composes to depth 0 from the parameter — the
parameter reaches whatever the reload reaches. Without that edge every parameter
of every function goc emits would look escaping at its first store.

### 1.2 A real fixed point over the SCCs

`opt/callgraph.go`'s Tarjan already emits components in reverse topological
order, so `componentsOf` just splits that order back into its contiguous runs.
A component of one non-self-recursive function is answered directly. A recursive
component starts every parameter at `ParamNoEscape` and re-analyses until
nothing changes — the greatest fixed point.

`goc`'s `parameterDoesNotEscape` instead answers "escapes" whenever a query
re-enters a function already on its stack, which is both pessimistic and
query-order dependent. `ComputeEscapeFactsBreakingCycles` reproduces that rule
exactly so the two can be diffed; §3 has the numbers.

Optimistic initialisation is permissive, so the plan's §4.2 warning applies:
if the transfer function is not monotone the fixed point can settle on "does not
escape" for something that escapes. It is monotone by construction here —
`analyzeFunction` is a pure function of the IR and the current table, with none
of the mutable walk state (`objectEscapeChecks`, `enterCalleeBody`) the AST
version carries — and `meetFacts` additionally clamps each round's answer to no
higher than the previous one, so the iteration is a descending chain whatever
the transfer function does. `TestEscapeSummaryFacts` asserts `Bailouts == 0`
(no component ever reaches the round cap) and that the fixed point is never
*less* precise than breaking cycles.

---

# ADJUDICATION (job `escape-shadow-adjudicate`, continuing at `3b3c30b`)

Sections 2 onward, written by the follow-on job. Sections 0 and 1 above are the
previous job's and are not re-derived. Everything below is measured on this
tree; anything not measured is marked so in place.

The table under adjudication is `goc/testdata/escape_shadow_baseline.txt`:
341 disagreements, **267 `frame -> heap`** (the IR analysis is more
conservative than goc's AST walk) and **74 `heap -> frame`** (the IR analysis is
more permissive). 22 rows are inside a loop -- 12 conservative, 10 permissive.

