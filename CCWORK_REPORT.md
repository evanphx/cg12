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

## 2. The 74 permissive rows: **the loop rule is NOT sufficient. There is a
   counterexample, it is not in a loop, and it is a real correctness hole.**

### 2.0 The headline

Three of the 74 (`h2_bundle.go:6705`, `:6740`, `:6966`, all in
`net/http.http2responseWriterState.writeChunk`/`writeHeader`) are **not** an
AST-walk pessimisation. The IR analysis would put
`&http2writeResHeaders{...}` in the frame of a function that hands its address
to another goroutine through a channel. The loop column is blank on all three.

The defect is one line of semantics, and it is general -- these three rows are
where it happens to surface in this corpus, not the extent of it:

> **`ParamNoEscape` means "the callee does not retain the *pointer value* I passed".
> `markSummarisedCall` and `escapeGraph.call` consume it as "the callee retains
> nothing reachable *through* what I passed", and record nothing at all for such
> an argument. For every argument whose *pointee* is what matters -- which is
> every aggregate passed by frame address, the calling convention goc uses for
> any struct wider than a couple of words -- those are different claims.**

`opt/escapegraph.go`'s own doc comment states the intended reading exactly
("A parameter is summarised by the smallest net count at which it is reached
from the heap or from a result: zero means the pointer itself is retained, one
or more means only what it points at is"). The consumer in
`opt/escapesummary.go:52` (`case ParamNoEscape:` -- "The callee cannot make the
argument outlive the call") assumes the stronger claim.

### 2.1 The chain, in the corpus

`stdlib/src/net/http/h2_bundle.go`:

    writeChunk:              rws.conn.writeHeaders(rws.stream, &http2writeResHeaders{...})
    writeHeaders(sc, st, headerData *http2writeResHeaders):
                             sc.writeFrameFromHandler(http2FrameWriteRequest{write: headerData, ...})
    writeFrameFromHandler(sc, wr http2FrameWriteRequest):
                             sc.wantWriteFrameCh <- wr        // to the serve goroutine

Measured on `goc/testdata/stdlib_http_client_server.go` (reproduces on all five
other `stdlib_http_*` corpus programs):

| symbol | summary |
|---|---|
| `$runtime.chansend1` | `0:escapes 1:escapes` -- correct |
| `$net/http.http2serverConn.writeFrameFromHandler` | `0:noescape 1:noescape` |
| `$net/http.http2serverConn.writeHeaders` | `0:noescape 1:noescape 2:noescape` |

`writeFrameFromHandler`'s parameter 1 is `:.goc.goabi.aggregate_net_http_http2FrameWriteRequest`
-- the *address of the caller's frame slot*. `noescape` on it is literally true
and useless: the callee loads the words out of that slot (`%t6 =p loadl %wr`)
and sends them down a channel. The graph records `heap <- %wr` at depth +1,
which is `ParamNoEscape` by the threshold above. `writeHeaders` then reads that
`noescape` at the call and records nothing, so `headerData` -- an ordinary
pointer parameter, depth 0 -- is never reached by the heap, and comes out
`noescape` too. `writeChunk` reads *that* and keeps the object in its frame.

### 2.2 Reduced, and demonstrated

`$TMPDIR/red/addr2.go`, 30 lines, no imports beyond `unsafe` for the address
print:

```go
type node struct{ n int }
type request struct{ ptr, a, b, c *node }   // all-pointer: goc copies word by word
var sink *node
func publish(r request) { sink = r.ptr }    // summary: 0:noescape   <-- true of the address
func hold(x *node)      { publish(request{ptr: x}) }  // summary: 0:noescape  <-- FALSE
func stash()            { hold(&node{n: 42}) }
```

`opt.ShadowPlacement` on it: `aggescape.go:23:7 | main.stash | composite-literal
| heap -> frame | (not in a loop)`. `$main.hold: 0:noescape` is stated by the
table and is refuted by three lines of source.

All four fields of `request` are pointers deliberately. With scalar tail fields
goc copies the tail with `goc_memcpy`, `escapeGraph.copyMemory` publishes the
whole source region at depth 1, `regionLoc` collapses offsets, and the pointer
escapes *by accident*. That accident is the only thing standing between the
first draft of this reduction and the miscompile; it is not a rule.

**Demonstrated, not argued.** A scratch build (`$TMPDIR/build`, not committed)
migrates exactly one decision site -- `compositeLiteral`'s struct/array branch
emits the neutral `OHeapAlloc` where it used to call `allocateEscapingTyped` --
which is precisely the migration this table is deciding about. Output of
`addr2.go`, `-O`:

| build | stack sample | `sink` | distance |
|---|---|---|---|
| real `go run` | 22133392140024 | 22133391089840 | 1 050 184 bytes below -- **heap** |
| migrated, knob **off** | 92356695653544 | 92356694147240 | 1 506 304 bytes below -- **heap** |
| migrated, knob **on** | 69801682795672 | 69801682795872 | **200 bytes above -- inside the goroutine stack** |

and the IR says the same thing outright: `%t2 =p alloc8 8` in `$main.stash` with
the knob on, `%t2 =p call $runtime.newobject` with it off. A package-level
variable holds a dead frame's address. That is the 2724ac7 shape.

The reads still print 42 in this program: the slot is not overwritten by the
calls that follow. That is what makes this class dangerous rather than
self-announcing -- it is latent corruption found later by something unrelated,
which is exactly how 2724ac7 presented.

### 2.3 What does *not* catch it

- **The loop rule does not.** None of the three corpus rows, and not the
  reduction, is in a loop. The loop column is orthogonal to this: it protects
  against one object per iteration, not against publication.
- **`opt.FrameEscapes` does not.** Run with `GOC_DEBUG_ESCAPECHECK=1` over the
  miscompiled build it reports the same 2 pre-existing runtime findings
  (`runtime.recovery`, `runtime.cgocallbackg1`) as the correct build and says
  nothing about `main.stash`. It is a same-function publication checker; this
  publication is three frames away. **A clean frame-escape audit does not clear
  this migration** -- a second thing that audit is structurally blind to, on top
  of the iteration case already on record.
- **The corpus running green does not.** Every corpus program compiles and runs
  identically with the knob on today, because the front end still owns this
  placement and `LowerHeapAllocations` never sees these allocations. The hole is
  **latent under the current architecture and goes live on the migration.** It
  also goes live sooner for any allocation that reaches `LowerHeapAllocations`
  as a neutral candidate today -- with the knob on, the summary is already the
  only thing standing between such a candidate and the frame.

### 2.4 The fix this implies

The summary needs to carry the depth it already computes, not collapse it to a
tri-state, or the consumer must record `heap <- argument` at depth +1 whenever
it accepts a `ParamNoEscape` -- i.e. "I believe you do not keep the pointer; I
do not believe you keep nothing it points at". The second is a two-line change
in `markSummarisedCall`/`escapeGraph.call` and costs precision only where the
argument is an address of something that holds tracked pointers. Neither is
attempted here: this job adjudicates, and the change wants its own shadow diff.

