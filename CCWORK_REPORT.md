# Design spike: moving cg12's escape analysis off the AST and onto the IR

Branch `ccwork/escape-ir-design`, off `ccwork/escape-frame-publication` (`ddd03eb`).
Deliverable is `ESCAPE_IR_PLAN.md`; this file is the measurement log, written as each
result landed. The previous contents of this file were the escape-frame-publication
report; it is unchanged on that branch and in history at `ddd03eb:CCWORK_REPORT.md`.

**Verdict: RECOMMEND HYBRID.** The full statement is at the end of this file and in
`ESCAPE_IR_PLAN.md`. In one paragraph: the two-phase shape the brief guesses at is not
speculative — it already exists and already carries 91.6% of the traffic. `ir.OHeapAlloc`
*is* the neutral op, `opt.LowerHeapAllocations` *is* the legaliser, and it already runs
at `goc/compile.go:487` before any other pass. But that pass promotes only **3.1%** of the
candidates it sees, because it has no callee summaries — so finishing the migration
without first giving it a fact table would move 11 255 frame placements to the heap
(§6). And the decision that genuinely *cannot* move — variable representation, which
`variableStorage` and `perIterationVariable` fix before any IR for the function exists —
is a different circularity from the one the brief names, and the neutral op does not
reach it. So: move placement to the IR, keep the summary computation on the typed AST,
connect them through a fact table, and do the fact table *first*.

---

## 1. The circularity is already resolved in the tree. Nobody wrote it down.

The brief's worry — "by the time the IR exists, the decision is baked in, so you would
be reading your own answer" — is true of *some* allocations and false of most of them.

`goc/compile.go` has three allocation emitters:

| emitter | emits | committed? |
|---|---|---|
| `allocateTyped` (12968) | `ir.OHeapAlloc` | **no — neutral** |
| `allocateEscapingTyped` (12984) | `call $runtime.newobject` | yes, heap |
| `localAlloc` / `localAllocTyped` / `allocLocal` | `OAlloc4/8/16` | yes, frame |

`ir.OHeapAlloc` is the neutral op the brief proposes inventing. `ir/build.go:505` even
says so: *"emits a typed heap-allocation candidate. The optimizer promotes it to a stack
allocation when its result cannot escape and otherwise lowers it to
allocator(typeDescriptor)."* `opt.LowerHeapAllocations` (`opt/escape.go:37`) is the
legalisation pass, and `goc/compile.go:487` runs it on the finished whole-program module
**before** `OptimizeModule` and before any backend lowering.

So the ordering question has an answer that is already shipping, and the size of the
remaining migration is measurable rather than hypothetical. See §3.

## 2. Cost of the current walk, measured

Instrumentation: `goc/escapecost.go` (recording is off unless a test turns it on) plus
`goc/escapecost_test.go`. `astParents` is attributed to the caller that rebuilt the map,
the outermost escape query is timed (nested queries are not double-counted), and every
cross-function summary question is keyed by `(function, index, summary)` so repeats are
visible.

    $ go test ./goc -run TestEscapeWalkCost -count=1 -v \
        -escape-cost-program=testdata/stdlib_crypto_ecdsa.go

(Timings are from an uncontended re-run; the counts were identical on every run.)

| | `runtime_slice_pointer_append_gc.go` | `stdlib_encoding_json_roundtrip.go` | `stdlib_crypto_ecdsa.go` |
|---|---|---|---|
| functions emitted | 2 743 | 5 680 | 6 965 |
| compile | 2.23 s | 4.74 s | 6.47 s |
| **`astParents` calls, summary** | **5 269** | **9 542** | **12 612** |
| **AST nodes rebuilt, summary** | **688 401** | **1 176 254** | **1 713 256** |
| AST nodes, all lowering | 207 579 | 315 421 | 438 174 |
| **summary rebuild / whole program** | **3.32×** | **3.73×** | **3.91×** |
| largest body rebuilt | 1 267 | 1 615 | 11 446 |
| summary queries | 5 269 | 9 557 | 12 635 |
| **distinct questions** | **530** | **1 033** | **1 485** |
| **queries per distinct question** | **9.94** | **9.25** | **8.51** |
| escape walk, wall clock | — | — | **0.456 s (7.1% of the compile)** |
| outermost escape queries | — | — | 7 495 |

Read the two bold rows together. On the ECDSA program the walk rebuilds parent maps over
**3.91× the entire program's AST** to answer **1 485 distinct questions**, asking each one
**8.5 times**. A `pprof` run agrees: `goc.astParents` is 4.21% of all samples and 6.7% of
`CompileExecutable`'s cumulative time, and the timer says the whole walk is 7.1%.

The point is not that 7.1% is intolerable. It is that ~90% of it is recomputation of
answers already computed, and that the fix for *that* (memoize `(function, index)`) is
independent of moving to the IR and much cheaper. See `ESCAPE_IR_PLAN.md` §4.

### 2.1 Where the 7.1% actually goes

`pprof` puts `goc.astParents` at 0.42 s of the ECDSA compile (4.21% of all samples, 6.7%
of `CompileExecutable`'s cumulative time). The depth-guarded timer puts the *whole* walk
at 0.456 s. Of the 2.57 M AST nodes `astParents` visits in that compile, 1.71 M (67%) are
visited on behalf of summary queries, so roughly **0.28 s of the 0.456 s walk — about
60% — is rebuilding parent maps**, not walking them.

That is the shape of the cost, and it is what makes stage 3 the right first move on this
side: the expensive thing is not the analysis, it is answering the same 1 485 questions
8.5 times each and rebuilding a parent map for every one.

(The timer's own `defer` is inside the 0.456 s. The `astParents` figure is from a profile
taken before the timers were added, so it is not.)

## 3. How much of the placement decision is already on the IR (sample; §6 is the full run)

`goc/placementcensus_test.go` records, per front-end decision site, how each allocation
was placed, and counts what `LowerHeapAllocations` then did with the neutral candidates
(`opt.HeapAllocLoweringStats`). Over 40 corpus programs:

| decision site | frame | heap |
|---|---|---|
| `allocateTyped` → **`OHeapAlloc` (neutral)** | **28 831** | 0 |
| `allocateEscapingTyped` → committed heap | — | 1 711 |
| `&CompositeLit` (`nonEscapingAddress`, 10794) | 160 | 1 050 |
| `make([]T,·,k)` backing (`makeResultDoesNotEscape`, 13055) | 48 | 290 |
| method-value descriptor (`valueDoesNotEscape`, 11616) | 56 | 466 |
| slice-literal backing (`valueDoesNotEscape`, 11708) | 387 | 195 |
| `string`→`[]byte` buffer (`valueDoesNotEscape`, 12721) | 339 | 211 |
| local variable heap-lifted (`findEscapingCaptures`) | 441 777 | 1 349 |

The heap-lifted locals allocate *through* `allocateTyped`, so they are already inside the
28 831 and must not be counted twice. The allocations whose placement the AST walk
**commits**, and which an IR pass therefore cannot revisit, are

    1 711 committed heap  +  990 committed frame  =  2 701

against **28 831** already neutral. **91.4% of heap-capable allocations already reach the
IR undecided** on this sample; the full 385-program run in §6 says 91.6%. The remaining
migration is five call sites in one file, not a rewrite — but §6 also says why it cannot
be done first.

## 4. Loop depth: confirmed absent, and it is a live miscompile

The brief asks whether cg12 tracks loop depth. It does not, and the consequence is not
theoretical.

`RUNTIME_PLAN.md` §5.9 already states the hazard, for the one case it handles:

> The per-iteration cell is allocated with `allocateEscapingTyped`, not the promotable
> `OHeapAlloc` candidate form. `opt.LowerHeapAllocations` decides whether a pointer
> outlives the *frame*, not whether it outlives one *iteration*, so promoting a
> per-iteration cell to a frame slot would silently put every iteration back on one slot.

That workaround covers the loop *variable*. It does not cover an ordinary allocation in a
loop body, and there both analyses are wrong. Four programs, each allocating in a loop
and keeping the previous iteration's pointer in a local that never leaves the function:

    //go:noinline
    func alternate(n int) (int, int) {
        var p, q *cell
        for i := 0; i < n; i++ {
            c := &cell{v: i}
            p = q
            q = c
        }
        return p.v, q.v
    }

| form | host `go run` | `goc -run` | `goc -O -run` |
|---|---|---|---|
| `&cell{v: i}` | `1 2` | **`2 2`** | **`2 2`** |
| `new(int)` | `1 2` | **`2 2`** | **`2 2`** |
| `make([]int, 0, 4)` | `1 2` | **`2 2`** | **`2 2`** |
| `var a [2]int; &a` | `1 2` | **`2 2`** | **`2 2`** |

Every iteration gets the same frame slot, so `p` and `q` are the same object. Neither
analysis is wrong about *escape* — nothing leaves the frame — which is exactly why
`opt.FrameEscapes` cannot see it either: it is a may-analysis over publications, and
there is no publication. It is an **aliasing** defect, and the missing fact is the one
gc calls loop depth.

This is new information: `RUNTIME_PLAN.md` records the hazard for per-iteration loop
variables and nothing else, and no capability or corpus program exercises the shape.

### 4.1 The fix, built and run

`opt/escapeloop.go` on this branch is the rule, 90 lines over `analysis/loopforest.go`
and `analysis/live.go`, hooked into `lowerFunctionHeapAllocations` and gated on
`GOC_ESCAPE_LOOP=1` so it changes nothing by default:

> A candidate allocated inside a loop is not promoted if a temporary holding it is live
> out of a latch, or if it is stored into a frame slot whose own allocation is outside
> the loop.

    $ GOC_ESCAPE_LOOP=1 go run ./cmd/goc -run goc/testdata/spike/loop_alias_forms.go

| form | how it is placed | knob off | **knob on** | host |
|---|---|---|---|---|
| `new(int)` | `allocateTyped` → **candidate** | `2 2` | **`1 2`** | `1 2` |
| `make([]int, 0, 4)` | falls to **candidate** | `2 2` | **`1 2`** | `1 2` |
| `var a [2]int; &a` | `allocLocal`, committed frame | `2 2` | `2 2` | `1 2` |
| `&cell{v: i}` | `nonEscapingAddress`, committed frame | `2 2` | `2 2` | `1 2` |

**Two of four fixed; the other two cannot be, because their allocations never reach the
IR as candidates.** That is the whole argument for moving placement, in four lines of
output: how much of the loop miscompile the IR rule fixes is a direct function of how
much of placement has moved.

### 4.2 And it costs nothing measurable

The full 385-program census re-run with `GOC_ESCAPE_LOOP=1` is **byte-identical** to the
run without it:

    knob off   frame 9 735 471   heap 509 920   promoted 14 433   lowered 453 319
    knob on    frame 9 735 471   heap 509 920   promoted 14 433   lowered 453 319

Since a fire escapes a candidate that would otherwise have been promoted, identical
`promoted` counts mean the rule fired **zero times across the whole corpus**.

That is only worth anything with a control, because a rule that is not wired up produces
the same zero. `TestSpikeLoopRuleFires` is the control:

    SPIKE loop rule loop_alias_composite.go: on=true escaped=0
    SPIKE loop rule loop_alias_forms.go:     on=true escaped=2
    SPIKE loop rule variadic_backing.go:     on=true escaped=0

Two fires, exactly the two candidates that were miscompiled, in a compile that links the
same stdlib as every corpus program.

**Stage 1 is free.** It fixes a live miscompile at zero measured allocation cost. Why the
corpus is untouched is not mysterious: the rule can only bite a candidate that would
otherwise be *promoted*, and only 14 433 of 467 752 are, and an allocation inside a loop
whose address reaches a slot outside it has almost always already been escaped by the
pass's existing conservatism.

**This is the strongest single argument for the IR side of the proposal.** Loop structure
is not recoverable from an upward AST walk, is trivially present in the CFG
(`opt/cfg.go`, and `opt/liferegions.go` already computes loop regions), and the rule is
mechanical: a frame allocation inside a loop may not be reachable from a value that is
live across the loop's back edge.

## 5. What the emitted IR shows about barriers

`goc/compile.go:5398`'s `store` decides the write barrier at emission time from
`g.isStackAddress(addr)`, a set of temporaries seeded by `localAlloc`. An `OHeapAlloc`
result is not in that set, so **every store into a neutral candidate is barriered**, and
promotion to a frame slot does not remove the barrier. Verified on the emitted IR for

    n := &node{v: v}          // frame:  storel %t5, %t6                  (no barrier)
    m := &node{v: v + 1}      // heap:   call $runtime.newobject
    n.next = m                // call $goc_storep(p %t14, p %t15)         (barrier)

`n`'s own field store is barrier-free because `n` is a `localAlloc`. Under the neutral-op
design it would be an `OHeapAlloc`, and every field store into it would be a
`goc_storep` — correct, but a per-store cost on code that used to have none. This is the
main *quantified* risk of moving the remaining 9%, and `ESCAPE_IR_PLAN.md` §6 makes
retiring the barrier after promotion a required part of the stage that moves them, not a
follow-up.

The same IR shows a second, smaller cost that already exists: `%t8 =p alloc8 16` in
`$main.build` is dead — the frame storage emitted before the heap decision was taken —
and survives at `-O0`.

## 6. Full-corpus placement census — and the number that changes the staging

    $ go test ./goc -run TestAllocationPlacementCensus -count=1 -v -timeout 50m
    --- PASS (1123.79s), 385 programs

| decision site | frame | heap |
|---|---|---|
| `allocateTyped` → **`OHeapAlloc`, neutral** | **456 631** | 0 |
| `allocateEscapingTyped` → committed heap | — | 30 810 |
| `&CompositeLit` | 2 062 | 19 034 |
| `make([]T,·,k)` backing | 720 | 4 341 |
| method-value descriptor | 867 | 7 792 |
| slice-literal backing | 4 226 | 3 913 |
| `string`→`[]byte` buffer | 3 380 | 2 460 |
| local variable heap-lifted | 4 685 295 | 19 091 |

    emitted frame allocations   9 735 471
    emitted allocator calls       509 920

Those last two are **exactly** the reference figures section 5 of the
escape-frame-publication report recorded for `ddd03eb` (frame 9 735 471, heap 509 920),
computed here by a different tool on a different day. The census instrument agrees with
the one it is replacing.

The neutral share holds at full scale: committed placements are
`30 810 + (2 062+720+867+4 226+3 380) = 42 065` against `456 631` neutral, so **91.6%**
(the 40-program sample said 91.4%).

**And then the number I did not have from the sample:**

    opt.HeapAllocLoweringStats:  promoted to a frame slot   14 433
                                 lowered to an allocator   453 319

`LowerHeapAllocations` promotes **3.1%** of the candidates it sees. It lowers the other
96.9% to `runtime.newobject`. That is what an intraprocedural analysis with no callee
summaries can do: `opt/escape.go:243`'s `default` arm escapes every candidate that
reaches any call it does not recognise, and most allocations reach a call.

This reframes the migration and I am revising the plan's staging because of it. The AST
walk's **11 255 committed-frame** placements are precisely the ones it proved with
interprocedural summaries the IR pass does not have. Moving those five sites to the
neutral op *before* giving the IR pass a fact table would push most of them to the heap:
+11 255 allocator calls, a 2.2% rise on 509 920. For scale, this branch moved **22** sites
frame→heap and paid 5.8% on `bigmod.Nat.Mul`.

**So stage 6 must not precede stage 4.** That is now an ordering constraint in
`ESCAPE_IR_PLAN.md` §7, not a preference.

The same number read the other way is the opportunity: a 3.1% promotion rate has a great
deal of headroom, and an IR pass with summaries should promote far more than that — which
is the only route by which this rearchitecture ends up with *fewer* allocations than
today rather than more.

## 7. The scaffolding changes no compiler behaviour, checked two ways

Everything in §2 and §3 required adding counters to `goc/compile.go` and `opt/escape.go`.
A measurement that perturbs the thing it measures is worse than no measurement, so this
was checked rather than asserted:

1. **The corpus allocation census is byte-identical to the reference.** frame 9 735 471 /
   heap 509 920, which is exactly what the escape-frame-publication report recorded for
   `ddd03eb` with a different tool. If any placement decision had moved, this would not
   match.
2. **`TestFrameEscapeAudit` passes.**

        $ go test ./goc -run TestFrameEscapeAudit -count=1 -v
        --- PASS: TestFrameEscapeAudit (148.99s)
        ok  github.com/evanphx/cg12/goc  149.461s

   No baseline additions and no vanished lines, at the same 147–149 s the audit took on
   `ddd03eb`, so it compiled the same 385 programs.

`go build ./...`, `go vet ./goc ./opt ./ir ./analysis` and `gofmt -l` are clean, and
`go test ./opt ./ir ./analysis ./lift ./lower` passes.

One caveat, stated because it is inside a number I quote: the walk timer's own `defer` is
counted in the 0.456 s it reports. The `astParents` figure (0.42 s) comes from a profile
taken before the timers were added and is not.

## 8. Acceptance criteria

See `ESCAPE_IR_PLAN.md` §8.

## 9. Recommendation

Stated in full in `ESCAPE_IR_PLAN.md` §9; here it is standalone.

**(a) Fix the IR pass where it is already wrong, now.** The loop rule (§4) fixes a live
miscompile in four allocation forms at both `-O` settings that no analysis in the tree can
see, and it costs **nothing measurable** on the corpus (§4.2). `opt/framecheck.go`'s
blindness to a phi'd slot base (§5's variadic case) is why residual 8a's reproduction
passes the audit; that is a checker fix, also standalone. Neither needs a rearchitecture
and neither should wait for one.

**(b) Move placement — and only placement — onto the IR, after the fact table.** The
neutral op exists, the legaliser exists and runs in the right place, and 91.6% of
heap-capable allocations already go through both. Finishing it is five call sites in one
file. But the legaliser promotes 3.1% of what it sees, so it must get callee summaries
*first*, or the five sites' 11 255 frame placements become heap allocations — a 2.2% rise
against the 22 sites that cost 5.8% on `bigmod.Nat.Mul`. What the move buys is what the
brief claims: interface boxing, variadic backing arrays and composite-literal storage stop
being things a syntactic walk has to be taught about and become instructions.

**(c) Do not move the interprocedural layer, and do not move variable representation.**
Their answers are consumed *before any IR for the function exists* — `variableStorage`
picks a different memory representation and `perIterationVariable` a different loop shape,
on the first materialisation of a variable. That is a second circularity, and unlike the
placement one the neutral op does not reach it. Make that layer good where it is instead:
memoize it (recovers most of a measured 7.1% of compile time, with no answer change), then
give it a real SCC fixed point, which cg12 can make *more* precise than gc's because
whole-program-from-source means the summary table needs no export data, no tags and no
versioning.

The disagreement with the brief, stated plainly: the diagnosis is right and the remedy is
right for placement, but the circularity the brief names is not the one that blocks a full
move. The blocking one is representation, not placement, and it is better solved by
leaving it where it is.

RECOMMEND HYBRID
