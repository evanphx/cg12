# Moving cg12's escape analysis onto the IR

A design spike, not an implementation. Written against `ccwork/escape-frame-publication`
(`ddd03eb`). Every number in it was measured on this tree; the harnesses are committed
alongside (`goc/escapecost.go`, `goc/escapecost_test.go`, `goc/placementcensus_test.go`,
`goc/spike_evidence_test.go`, `opt/escapeloop.go`, `opt.HeapAllocLoweringStats`) and the measurement log is
`CCWORK_REPORT.md`.

**Recommendation: RECOMMEND HYBRID.** Stated in full in §9.

---

## 0. The two sentences that change the shape of the question

**The neutral op already exists.** `ir.OHeapAlloc` is a typed allocation candidate that
has not committed to frame or heap; `opt.LowerHeapAllocations` (`opt/escape.go:37`) is
its legaliser; `goc/compile.go:487` runs that legaliser on the finished whole-program
module before any other pass. `ir/build.go:505` documents it in exactly those terms.

**91.6% of cg12's heap-capable allocations already flow through it** — and the pass that
decides them promotes only **3.1%** to frame slots, because it has no callee summaries.
Both measured, §2.

So the brief's circularity is not a wall the idea has to get past. It is a migration that
someone started, got most of the way through, and did not finish. But the second number
says the unfinished part is not the interesting one either: what is actually missing is
that the analysis deciding those candidates (`lowerFunctionHeapAllocations`) is
intraprocedural, has no loop concept, and has no summaries — so it says "escapes" to
almost everything — while the analysis that *is* precise (`goc/compile.go`'s walk) is on
the AST, answers a different question a step earlier, and is the only reason 11 255
allocations stay in frames at all.

**Finish the neutral-op migration without first giving the IR pass summaries and you move
those 11 255 allocations to the heap.** That is the single most important constraint in
this plan and it is why the stages are ordered the way they are.

---

## 1. Answer to question 1: the neutral op, and when legalisation runs

### 1.1 What the neutral op looks like — it looks like this

    %t9 =p heapalloc $runtime.newobject, $.goc.runtime.type.main_node.954c…, 16   ; Aux = align

`ir.OHeapAlloc`, `ir/op.go:79`. `Args` are `{allocator, typeDescriptor, size}`, `Aux` is
the alignment, `Cls` is `ClsP`, and the result is marked `GCRef`. `ir/build.go:508`'s
`Block.HeapAlloc` is the only constructor.

Legalisation, `opt/escape.go:255-292`:

* **to the frame** — the op becomes `OAlloc4`/`OAlloc8`/`OAlloc16` on `Aux`, `Args`
  becomes `{size}`, and a `goc_memset(result, 0, size)` call is appended, because the
  allocator zeroed and a frame slot does not;
* **to the heap** — the op becomes `OCall`, `Args` truncates to `{allocator, typeDescriptor}`,
  `Aux` cleared. The instruction is *already shaped like the call it becomes*, which is
  why the rewrite is four lines.

Nothing needs inventing. The one addition this plan proposes to the op itself is §1.4.

### 1.2 What is *not* neutral today

`goc/compile.go` has three emitters:

| emitter | line | emits | committed |
|---|---|---|---|
| `allocateTyped` | 12989 | `OHeapAlloc` | no |
| `allocateEscapingTyped` | 13006 | `call $runtime.newobject` | heap |
| `localAlloc` / `localAllocTyped` / `allocLocal` | 5507 / 5785 / 9855 | `OAlloc4/8/16` | frame |

and exactly **five** places choose between the committed pair by asking the AST walk:

| site | `goc/compile.go` | predicate |
|---|---|---|
| `&CompositeLit` placement | 10811 | `nonEscapingAddress` |
| method-value descriptor | 11631 | `valueDoesNotEscape` |
| slice-literal backing array | 11725 | `valueDoesNotEscape` |
| `string`→`[]byte`/`[]rune` buffer | 12741 | `valueDoesNotEscape` |
| `make([]T, ·, k)` backing, constant `k` | 13080 | `makeResultDoesNotEscape` |

plus a sixth that is a different animal and is treated separately in §3.3:

| site | `goc/compile.go` | predicate |
|---|---|---|
| local-variable heap lift | `findEscapingCaptures`, 12447, consumed at `variableStorage`, 5516 | `receiverDoesNotEscape` / `valueDoesNotEscape` / `addressEscapesFunction` |

Making the first five neutral is mechanical: each becomes `g.allocateTyped(...)` and the
`heap`/`!heap` branch disappears. Four of the five already have a `localAllocTyped` call
immediately above the heap branch that becomes dead — see the stray `%t8 =p alloc8 16` in
`CCWORK_REPORT.md` §5, which is that dead frame storage surviving at `-O0` today.

The fifth, the string-conversion buffer, is not a plain allocation: `goc/compile.go:12736`
passes either a frame buffer pointer or **the constant 0** to
`runtime.stringtoslicebyte`, and 0 means "allocate for me". A neutral form has to pass a
candidate pointer and let legalisation rewrite the escaped case back to 0 — a rewrite the
pass does not have. Either teach `LowerHeapAllocations` a "candidate that legalises to a
null argument" form, or leave this site on the AST. It is 5 840 decisions across the
corpus against 456 631 neutral ones; leaving it is the right call for the first pass.

### 1.3 When legalisation runs, relative to the opt passes

It already runs in the right place and must stay there:

    goc/compile.go:483   opt.InlineNoSplitCalls(mod)        (runtime builds only)
    goc/compile.go:486   opt.InlineHeapAllocations(mod)     <- SCC-ordered constructor inlining
    goc/compile.go:487   opt.LowerHeapAllocations(mod)      <- LEGALISATION
    goc/compile.go:488   reportFrameEscapes(mod)            <- the verifier
    ... later ...        opt.OptimizeModule(mod)            (only under -O)

Three constraints pin it:

1. **After `InlineHeapAllocations`, before everything else.** That pass
   (`opt/inline.go:300`) exists solely to expose a constructor's `OHeapAlloc` to a caller
   that can keep it in a frame, and it is already SCC-ordered bottom-up
   (`opt/callgraph.go:67`). Legalising first would waste it.
2. **Before `Mem2Reg`.** The analysis reasons about frontend variables as memory —
   `slotBases`/`localSlot` in `opt/escape.go:66` and `opt/framecheck.go:157`. `Mem2Reg`
   promotes those slots to SSA temporaries and phis, and the slot-granular facts go with
   them. (This is not hypothetical: §5 shows `frameFacts` already losing a fact to a phi.)
3. **Before the backend.** `OHeapAlloc` has no machine lowering; `opt/escape.go` is the
   only thing that removes it.

`LowerHeapAllocations` runs unconditionally, at `-O0` as well as `-O`, which is why
placement is a front-end-pipeline decision rather than an optimisation. Any new analysis
inherits that: **it must be cheap enough to run at `-O0`.** §4 measures the budget.

### 1.4 The one thing the neutral op is missing

An `OHeapAlloc` whose result is promoted keeps every write barrier that was emitted into
it. `goc/compile.go`'s `store` (5398) decides the barrier at emission time from
`g.isStackAddress(addr)` (`goc/compile.go:5821`), a set seeded only by `localAlloc`. So:

    n := &node{v: v}     // localAlloc  -> storel %t5, %t6                 no barrier
    m := &node{v: v+1}   // heap        -> call $runtime.newobject
    n.next = m           //             -> call $goc_storep(%t14, %t15)    barrier

If `n` became a candidate, its field store would be a `goc_storep` and stay one after
promotion. That is correct but not free, and it is the single biggest *measurable* risk of
moving the remaining five sites.

**The fix belongs in the same stage as the move**, not after it: when
`lowerFunctionHeapAllocations` promotes a candidate, it already rewrites the instruction;
it should also rewrite every `goc_storep(p, v)` whose destination `p` resolves (through
the `aliases`/`bases` maps it has already built) to that promoted allocation into a plain
`OStore`. It has the information — `isAtomicPointerStore` at `opt/escape.go:316` already
recognises the call and `heapBase` already resolves the destination.

---

## 2. What the placement census says: five call sites, and one ordering constraint

`goc/placementcensus_test.go`, **all 385 corpus programs**, whole-program compiles,
1 123 s:

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
| **emitted frame allocations** | **9 735 471** | |
| **emitted allocator calls** | | **509 920** |

The last two rows are exactly the reference figures the escape-frame-publication report
recorded for `ddd03eb`, computed by a different tool: the instrument agrees with the one
it replaces.

Heap-lifted locals allocate *through* `allocateTyped`, so they are inside the 456 631 and
are not counted twice. AST-committed placements are `30 810 + 11 255 = 42 065` against
`456 631` neutral: **91.6% already neutral.**

### 2.1 The number that constrains the staging

`opt.HeapAllocLoweringStats`, same run:

    promoted to a frame slot    14 433
    lowered to an allocator    453 319

**`LowerHeapAllocations` promotes 3.1% of the candidates it sees.** It is intraprocedural
and has no callee summaries, so `opt/escape.go:243`'s `default` arm escapes every
candidate reaching a call it does not recognise — and most allocations reach a call.

The AST walk's **11 255 committed-frame** placements are exactly the ones it proved using
summaries the IR pass does not have. Routing those five sites through the neutral op
*before* the IR pass has a fact table would send most of them to the heap: **+11 255
allocator calls on a base of 509 920, a 2.2% rise.** For scale: this branch moved *22*
sites frame→heap and paid 5.8% on `bigmod.Nat.Mul`.

**Stage 6 must not precede stage 4.** §7 states it as a constraint, not a preference.

Read the other way, 3.1% is the opportunity. An IR pass *with* summaries has a great deal
of headroom above that rate, and it is the only route by which this rearchitecture ends up
allocating less than today rather than more.

The 4 685 295 frame locals are ordinary variable slots, not candidates; they are in the
table for scale. Whatever a new analysis does, it must not turn a measurable fraction of
*those* into allocations.

---

## 3. Answer to question 2: what the AST knows that the IR would lose

Each item: does the IR lose it, and if so how is it recovered.

### 3.1 Source-level types — **not lost, and mostly not needed**

* A candidate carries its type: `OHeapAlloc.Args[1]` is the `$.goc.runtime.type.…`
  descriptor symbol.
* A frame allocation carries its *pointer map*: `ir.Func.StackPointerWords[allocID]` is
  the set of pointer-word offsets, written by `markStackPointerWord`
  (`goc/compile.go:5797`) for every typed frame allocation, and `Temp.GCType` carries a
  descriptor id. "Slice header vs three words" is precisely the question
  `StackPointerWords` answers: offset 0 pointer, 8 and 16 not.
* The analysis does not want types anyway. `frameFacts` and `lowerFunctionHeapAllocations`
  are *provenance* analyses — a value is frame-derived because it descends from an
  `OAlloc`, not because of its type — and provenance is the sound formulation for a
  may-analysis. `opt/escape.go:107` already handles a `memcpy` at word granularity using
  the constant size operand.

Where the AST's types were load-bearing was `interfaceConversionAllocates` /
`isDirectInterfaceType` (`goc/compile.go:2812`) — deciding whether a conversion *makes*
storage. On the IR that question does not exist: the storage is either there as an
instruction or it is not. **This is the brief's central claim and it holds.**

### 3.2 `go:noescape` on bodiless functions — **lost, cheaply recovered**

`parameterDoesNotEscape` (`goc/compile.go:3467`) and `receiverDoesNotEscape` (`:3580`)
answer a bodiless declaration with `hasCompilerDirective(decl, "go:noescape")`. An
`ir.Func` with `Start == nil` has no directive. There are **621** `//go:noescape`
directives on **619** functions in `stdlib/src`, so this is not a rounding error.

Recovery is three lines. `ir.SymAttr` (`ir/func.go:357`) already exists with one member,
and `goc/compile.go:1374`'s `registerSymAttrs` already populates `Module.SymAttrs`:

    // ir/func.go
    SymNoEscape                    // callee retains no pointer argument past the call

    // goc/compile.go registerSymAttrs
    if declaration.decl.Body == nil && hasCompilerDirective(declaration.decl, "go:noescape") {
        mod.SymAttrs[symbol] |= ir.SymNoEscape
    }

The same mechanism is the natural carrier for the whole summary table (§4.3).

### 3.3 The distinction between a value and its storage — **not lost. But it hides the real ordering problem.**

Before `Mem2Reg`, cg12's IR is *more* explicit than the AST here: a variable is an
`OAlloc` slot, a value is a temp, and `opt/escape.go` already distinguishes them
(`slotBases` versus `bases`). Nothing is lost.

What *is* a genuine blocker sits next to it, and the brief's neutral-op sketch does not
address it. `variableStorage` (`goc/compile.go:5516`) does not merely *place* a
heap-lifted variable; it gives it a **different representation**:

    escaping capture + isMemoryValue:   backing = allocateTyped(T)
                                        storage = allocateTyped(*T);  *storage = backing
    escaping capture + string/iface/complex128:
                                        storage = allocateTyped(T);  g.directValues[obj] = true
    otherwise:                          storage = allocLocal(T)                    // one slot

`g.directValues[object]` changes how *every read and write of that variable anywhere in
the function* is emitted (`assignLocal`, `goc/compile.go:5432`). And
`perIterationVariable` (`goc/compile.go:5597`) reads the same `escapingCaptures` set to
decide whether the *loop* gets the three-clause per-iteration rewrite — a change to the
control-flow shape, not to an allocation.

Both are consumed the first time the variable is materialised, which is before the first
instruction of the enclosing function exists. **You cannot defer them to an IR pass
without emitting a representation that does not depend on the answer**, and that means
always emitting the indirect form and having a pass collapse it — a change on the scale
of the whole variable-lowering path, pessimising `-O0` until the collapse pass lands.

That is the load-bearing reason this plan is a hybrid rather than a move. `findEscapingCaptures`
and the summary layer under it stay on the AST.

### 3.4 Named parameter identity for the summaries — **carried, by convention**

`ir.Func.Params` is `[]*Temp` with `Name` set, and `opt/framecheck.go:421` already reads
it; `:374` already distinguishes a result-area parameter by the `result` name prefix. A
summary keyed on `(ir.Func.Name, parameter index)` is well defined.

Two wrinkles, both real and both survivable:

* The receiver is `Params[0]` with no marker, so `receiverDoesNotEscape` and
  `parameterDoesNotEscape(f, 0)` are the same query on the IR. That is fine — they are
  the same question — but the two AST entry points must agree on the index convention
  when they publish facts.
* An aggregate result is passed as a `result0` *parameter*, so a function with an inline
  aggregate result has a parameter that is not a Go parameter. Index arithmetic between
  the AST signature and `ir.Func.Params` is therefore not the identity. `functionParameter`
  and `callWithSignature` in `goc/compile.go` know the mapping; the fact table must be
  keyed on the Go index and translated once, in one place, rather than at each use.

### 3.5 Four more the brief does not list

* **Which `*ast.FuncLit` a closure is** — *better* on the IR. Each literal is its own
  `ir.Func`, so `resultLeakIsAllowed`'s "is this `return` inside a nested literal"
  question (`goc/compile.go:2703`) cannot be asked wrongly; it is structurally impossible.
* **Variadic argument construction** — *better* on the IR, and this closes a stated
  residual. `buildVariadicSlice` allocates the `[]any` backing with `allocateTyped`, so
  the IR shows the backing array and the copies into it. §5 shows a live instance the AST
  walk cannot see.
* **Package identity and `noWriteBarrier`** — `addressEscapesWithin` special-cases
  `g.pkg.Path() == "runtime"` (`goc/compile.go:2387`). Symbol names carry the package, and
  `ir.Func.NoSplit`/`ManagedFrame` carry the rest. Recoverable.
* **Shared type parameters** — `isSharedTypeParameter` exists because cg12 shape-shares
  generic code. On the IR a shape-shared function is one function and a summary over it
  applies to every instantiation: sound, and no less precise than today.

---

## 4. Answer to question 3: cross-function summaries

### 4.1 What it costs now — measured

`goc/escapecost.go` attributes every `astParents` rebuild to its caller, times the
outermost escape query, and keys every summary question so repeats are visible.

| | `runtime_slice_pointer_append_gc.go` | `stdlib_encoding_json_roundtrip.go` | `stdlib_crypto_ecdsa.go` |
|---|---|---|---|
| functions emitted | 2 743 | 5 680 | 6 965 |
| compile | 2.23 s | 4.74 s | 6.79 s |
| `astParents` rebuilds for summaries | 5 269 | 9 542 | 12 612 |
| AST nodes rebuilt for summaries | 688 401 | 1 176 254 | 1 713 256 |
| AST nodes for all of lowering | 207 579 | 315 421 | 438 174 |
| **rebuild traffic ÷ whole program** | **3.32×** | **3.73×** | **3.91×** |
| distinct `(function, index, summary)` questions | 530 | 1 033 | **1 485** |
| **questions asked per distinct question** | **9.94** | **9.25** | **8.51** |
| escape walk, wall clock | — | — | **0.460 s = 6.8% of the compile** |

`pprof` on the ECDSA compile agrees independently: `goc.astParents` is 4.21% of samples,
6.7% of `CompileExecutable` cumulative.

And it says where the cost is. `astParents` is 0.42 s of the 0.46 s the timer attributes
to the walk; 67% of the nodes it visits are visited for summary queries, so about **0.28 s
of the 0.46 s — 60% — is rebuilding parent maps rather than walking them**. The expensive
thing is not the analysis. It is answering 1 485 questions 8.5 times each and rebuilding a
map for every answer.

For scale on the other side: **`opt.FrameEscapes` — one whole-module, per-function,
fixed-point pointer dataflow analysis over the same 6 965 functions — takes 0.182 s.**
An IR-level analysis of this shape is not a compile-time problem; the AST walk is.

### 4.2 The fixed point, and why it is not what is there now

`parameterDoesNotEscape` breaks recursion by returning `false` — "escapes" — when
`checking[key]` is already set. That is conservative and it is also *query-order
dependent*: the same `(f, i)` answers `true` asked standalone and `false` asked from
inside a walk that already has `f` on the stack. Two consequences:

1. Naive memoization is **not** answer-preserving.
2. Mutual recursion is systematically pessimised. `func f(p *T) { g(p) }` /
   `func g(p *T) { f(p) }` retains nothing and both are reported as escaping.

The right solve is the standard one, and cg12 has all the machinery:

    build the direct-call graph over *types.Func        (new; the IR one is opt/callgraph.go:17)
    Tarjan -> SCCs in reverse topological order          (the IR one is opt/callgraph.go:67)
    for each SCC bottom-up:
        if |SCC| == 1 and not self-recursive:
            answer each (f, i) directly; the answer cannot depend on context
        else:
            initialise every (f, i) in the SCC to TRUE ("does not escape")
            re-run the walk over the SCC until no answer changes
    cache

The lattice is `true > false`, the transfer function is monotone, and starting optimistic
converges to the **greatest** fixed point — which is the correct semantics for "no use
anywhere in the mutually recursive set publishes this" and is strictly more precise than
today. A direct publication (`global = p`) demotes on the first round and propagates.

**Be suspicious of this stage in a way the others do not need.** Optimistic initialisation
is a *permissive* change: if the transfer function turns out not to be monotone in some
corner — and `objectDoesNotEscape` carries mutable state (`objectEscapeChecks`,
`enterCalleeBody`) that a purely functional transfer would not — the fixed point can
settle on "does not escape" for something that does, and that is a silent miscompile in
exactly the class 2724ac7 and 9f76498 were. It is the one stage in this plan that must not
land without the shadow diff (stage 5) reading clean in the permissive direction, and
without an argument for why the state the walk carries does not break monotonicity.

### 4.3 The representation

    // opt/escapefacts.go  (new)

    // ParamEscape is what a caller needs to know about one parameter.
    type ParamEscape uint8
    const (
        ParamEscapes      ParamEscape = iota // assume the worst
        ParamLeaksToResult                   // reaches only the function's own results
        ParamNoEscape                        // cannot outlive the call
    )

    // EscapeFacts is the whole program's summary table, keyed by IR symbol name.
    type EscapeFacts struct {
        Params map[string][]ParamEscape   // index 0 is the receiver when there is one
    }

Three properties worth stating because they are what cg12 can do and gc cannot:

* **No export data, no tags, no versioning.** gc encodes summaries as signature tags in
  export data because it compiles a package at a time. cg12 compiles whole-program from
  source (`goc/compile.go:41 CompileExecutable`), so the table is an in-memory map built
  once per compile and thrown away. Nothing has to be parsed, nothing can go stale, and
  there is no ABI to keep compatible.
* **It can be more precise than gc's.** gc's tags are a fixed small vocabulary with a
  bounded leak level because they have to fit in a string. cg12's can carry anything —
  including, later, "parameter 0 flows to parameter 1" and per-result leak sets — because
  nothing serialises it.
* **It is the natural carrier for §3.2's `go:noescape`.** A bodiless function gets
  `ParamNoEscape` for every parameter from the directive; the IR pass reads one table
  rather than two mechanisms.

The table is built by the front end from the AST (§3.3's ordering constraint forces
that), handed to the legaliser as an argument —
`opt.LowerHeapAllocations(module, facts)` — and consumed there. That signature change is
the entire coupling between the two halves.

### 4.4 The memoization stage, stated so it is provably safe

Before any of the above, one cheap, strictly answer-preserving stage:

> Cache `(function, index, summary) -> bool`, but **only when the computation of that
> answer used no cycle break.** Propagate a "a `checking` entry was hit somewhere below
> me" flag up through the recursion and refuse to cache a tainted answer.

For a function outside every recursive SCC, no `checking` entry can be hit while
computing it, so its answer is context-independent and caching it changes nothing. For a
function inside one, nothing is cached and behaviour is exactly as today. Given 8.5
queries per distinct question, this is most of the 6.8%, for a change with no answer
delta at all — which means it can land, and be reverted, without touching the corpus
baseline.

---

## 5. Answer to question 4: loop depth — confirmed absent, and it is a miscompile today

`goc/compile.go`'s walk has no notion of iteration. `*ast.RangeStmt` appears twice, both
times as a context to climb through (`:2638` returns `parent.X == current`; `:3270`
recurses into the value variable). `*ast.ForStmt` appears in neither switch.
`opt/escape.go` has no loop concept either.

`RUNTIME_PLAN.md` §5.9 already knows this, for the one case it handles:

> The per-iteration cell is allocated with `allocateEscapingTyped`, not the promotable
> `OHeapAlloc` candidate form. `opt.LowerHeapAllocations` decides whether a pointer
> outlives the *frame*, not whether it outlives one *iteration*, so promoting a
> per-iteration cell to a frame slot would silently put every iteration back on one slot.

That workaround protects the loop *variable*. It does not protect an ordinary allocation
in a loop body, and there cg12 is wrong. `goc/testdata/spike/loop_alias_forms.go`:

    //go:noinline
    func alternate(n int) (int, int) {
        var p, q *cell
        for i := 0; i < n; i++ {
            c := &cell{v: i}     // never leaves the function
            p = q
            q = c
        }
        return p.v, q.v
    }

| allocation form | host `go run` | `goc -run` | `goc -O -run` |
|---|---|---|---|
| `&cell{v: i}` | `1 2` | **`2 2`** | **`2 2`** |
| `new(int)` | `1 2` | **`2 2`** | **`2 2`** |
| `make([]int, 0, 4)` | `1 2` | **`2 2`** | **`2 2`** |
| `var a [2]int; &a` | `1 2` | **`2 2`** | **`2 2`** |

Every iteration reuses one frame slot, so `p` and `q` name the same object. Both analyses
are *right* about escape — nothing leaves the frame — which is why `opt.FrameEscapes` is
silent too (`TestSpikeReductionsFrameEscapes`: zero findings on these programs, before or
after `OptimizeModule`). It is an **aliasing** defect and no publication analysis can see
it. This is not recorded anywhere in the tree and no corpus program or capability
exercises it.

### 5.1 The fix, built and run

Loops are invisible to an upward AST walk and structural in the CFG, and the CFG side is
already built: `analysis/loopforest.go` gives `LoopForest.In[block]` (innermost enclosing
natural loop), `Loop.Latches` (the back-edge sources), and `Loop.Parent`;
`analysis/live.go` gives `Liveness.Out[block]`. The rule:

> A candidate allocated inside a loop may be promoted only if nothing derived from it can
> outlive one iteration: no temporary holding it is live out of a latch, and it is not
> stored into a frame slot whose own allocation is outside the loop.

`opt/escapeloop.go` on this branch is that rule, 90 lines, hooked into
`lowerFunctionHeapAllocations` after the `bases`/`slotBases` fixed point and gated on
`GOC_ESCAPE_LOOP=1` so it changes nothing by default. A knob like this must not survive
into a landed change — `f397b28` removed the previous one for exactly that reason — but it
makes the claim checkable rather than asserted:

    $ GOC_ESCAPE_LOOP=1 go run ./cmd/goc -run goc/testdata/spike/loop_alias_forms.go

| allocation form | how it is placed | knob off | **knob on** | host |
|---|---|---|---|---|
| `new(int)` | `allocateTyped` → **candidate** | `2 2` | **`1 2`** | `1 2` |
| `make([]int, 0, 4)` | falls to **candidate** | `2 2` | **`1 2`** | `1 2` |
| `var a [2]int; &a` | `allocLocal`, committed frame | `2 2` | `2 2` | `1 2` |
| `&cell{v: i}` | `nonEscapingAddress`, committed frame | `2 2` | `2 2` | `1 2` |

**Two of the four are fixed by the IR pass alone. The other two are not, and cannot be —
their allocations never reach the IR as candidates.** That is the plan's central argument
reduced to four lines of output: the loop fix is only *complete* once placement is on the
IR, and until then it covers whatever fraction of allocations happens to be neutral.

`new(int)` is `g.allocateTyped(pointer.Elem())` (`goc/compile.go:13172`), so it is a
candidate. `make([]int, 0, 4)` in this program takes the
`else if g.runtimeAllocation && hasFixedCapacity` arm and is also a candidate. The array
and the composite literal are `allocLocal` and `nonEscapingAddress` respectively, and are
`alloc8` before any pass runs.

**Stage 1 stands alone anyway.** It is a correctness fix to the existing IR pass, it needs
none of the rest of this plan, and it should land first (§7, stage 1) — it just does not
finish the job on its own.

---

## 6. What the IR closes that the AST cannot: the branch's own residuals

`CCWORK_REPORT.md` §8 on `ccwork/escape-frame-publication` left two residuals open. They
are the best available evidence about whether the AST approach can be finished.

### 6.1 Residual 8a is live, and the IR sees it

8a: a pointer-shaped value in a heap variadic backing array. `boxedIntoInterface`
correctly reports no box (a pointer goes straight into the two-word descriptor), but
`buildVariadicSlice` allocates the `[]any` backing with `allocateTyped` and copies the
descriptor's words into it. The report says what stops it today is that real variadic
callees retain their arguments. Give it one that does not —
`goc/testdata/spike/variadic_backing.go`:

    //go:noinline
    func retainNothing(args ...any) int { return len(args) }

    //go:noinline
    func leaky() int { x := 42; return retainNothing(&x) }

and the emitted IR is unambiguous:

    %t1  =p alloc8 8                                            ; x, in the frame
    %t2  =p call $runtime.newobject($…type.1_any…)               ; the []any backing, in the HEAP
    %t3  =p alloc8 16                                            ; the interface descriptor
    storel %t1, %t4                                              ; %t4 = %t3+8 : descriptor holds &x
    …
    %t12 =p loadl %t10                                           ; %t10 = %t8+8, %t8 phis to %t3
    call $goc_storep(p %t11, p %t12)                             ; &x into the heap backing array

A frame address in a fresh heap object, at `-O0` and at `-O`. On the IR this needs no new
rule at all: the backing array is an allocation, the store into it is a store, and a
whole-program analysis over that IR reports it the way it reports any other.

### 6.2 And the checker misses it, for a reason worth fixing on its own

`opt.FrameEscapes` reports nothing here. The chain runs through `%t8`, a phi of two frame
allocations (the interface-descriptor nil test, which the optimizer does not fold because
it does not know an `OAlloc` is non-nil). `frameFacts.frameSlot` resolves an address via
`aliasInfo.locOf`, whose `baseKind` (`opt/alias.go:250`) returns `keyTemp`/`cUnknown` for
anything without an `allocBase` — and a phi result has none, because `opt/alias.go:153`
treats a pointer through a phi as escaping. So the slot fact stored under `%t3` is never
read back through `%t8`.

`frameFacts.propagate` already iterates `block.Phis` for the `bases` map. Extending
`frameSlot` to resolve a phi whose arguments all resolve to the same local base would
close it. **That is a checker fix, independent of everything else here, and it should
land early** — the shadow-mode stage in §7 is only as good as the checker behind it.

### 6.3 Residual 8b dissolves

8b is `nonEscapingAddress` being cruder than the walk, with the two forced to agree at the
conservative end. If placement moves to the IR there is only one placer, so there is
nothing left to disagree. This is the class the brief wants made unrepresentable, and for
placement it genuinely is.

---

## 7. Answer to question 5: the migration, in independently revertable stages

Every stage builds, keeps the corpus green, and can be reverted alone. Stages 1–3 are
worth doing even if the rest is abandoned.

**One hard ordering constraint, from §2.1:** stage 6 (route the committed sites through
the neutral op) must come after stage 4 (give the IR pass a fact table). Today's IR pass
promotes 3.1% of the candidates it sees, so moving the five sites first would send most of
their 11 255 frame placements to the heap — a 2.2% rise in allocator calls, against the
22 sites that cost 5.8% on `bigmod.Nat.Mul`. Every other stage is independent.

### Stage 1 — the loop rule in `LowerHeapAllocations` *(correctness, standalone)*

* **Already written on this branch**, as `opt/escapeloop.go`, behind `GOC_ESCAPE_LOOP=1`.
  Landing it is: delete the knob, delete the `os.Getenv`, call
  `escapeLoopCarriedCandidates` unconditionally from `lowerFunctionHeapAllocations`.
  `opt/alias.go` gains `locKey.allocTemp`, six lines.
* Uses `analysis/loopforest.go` and `analysis/live.go` as they stand; no new analysis.
* Tests: `goc/testdata/spike/loop_alias_*.go` promoted into a differential test in `goc/`
  (the two forms the rule fixes must match the host toolchain; the two it cannot must be
  marked as waiting on stage 6, not quietly left passing-because-broken);
  `opt/escape_test.go` gains the IR-level unit form.
* Expected cost: some loop-body candidates move frame→heap. **Price it with the corpus
  census before tightening or loosening the query** — the rule as written escapes on
  *any* store into a slot allocated outside the loop, which is stricter than back-edge
  liveness alone and is the conservative end of the design space.
* Revert: one commit.

### Stage 2 — the checker learns phis *(verification, standalone)*

* `opt/framecheck.go`: `frameSlot`, and `classify`, resolve a phi whose arguments share a
  base.
* Test: `variadic_backing.go` must produce a finding.
* Expect **new `frame_escape_baseline.txt` lines**, because the checker is now seeing
  publications that were always there. Each has to be read and either fixed or baselined
  with a reason. Do this before stage 5 or the shadow diff is measuring a blind checker.

### Stage 3 — memoize the summary walk *(speed, provably no answer change)*

* `goc/compile.go`: `parameterDoesNotEscape`, `parameterLeaksOnlyToResult`,
  `receiverDoesNotEscape` consult a `map[parameterKey]bool` on `gen`; taint-propagation
  refuses to cache an answer that used a cycle break (§4.4).
* Test: `goc/escapecost_test.go` — `queriesPerDistinct` must fall toward 1.0 and
  `walkSeconds` with it; the corpus census must be **byte-identical**.
* Revert: one commit. If the census is not byte-identical, the taint rule is wrong and
  the stage is wrong.

### Stage 4 — the fact table, computed on the AST, published to `opt`

* New `opt/escapefacts.go` (types), new `goc/escapesummary.go` (the SCC solve).
* `opt.LowerHeapAllocations(module)` → `opt.LowerHeapAllocations(module, facts)`; the pass
  consults `facts` where it currently falls into `default: mark(arg)` for a call.
* `ir/func.go` gains `SymNoEscape`; `goc/compile.go:1374 registerSymAttrs` sets it.
* This stage makes the IR pass **more permissive** — the direction 2724ac7 and 9f76498 got
  wrong — so it is where the acceptance criteria in §8 bite hardest, and it should land
  behind the shadow mode of stage 5 rather than in front of it.
* **It is also the stage that unblocks stage 6.** The measurable success condition is
  `opt.HeapAllocLoweringStats`'s promotion rate: 3.1% today (14 433 of 467 752). Stage 6
  is safe to start when the rate is high enough that the five sites' 11 255 frame
  placements survive the move — which is checkable directly, per site, with the shadow
  diff of stage 5 before a single line of stage 6 is written.

### Stage 5 — shadow mode *(no behaviour change; the whole point of the exercise)*

The brief asks for this and it is the right instinct. Concretely:

* `goc/compile.go` keeps deciding placement exactly as today.
* A new `opt.ShadowPlacement(module, facts)` runs the *new* analysis over the emitted IR
  and, for every allocation, records what it would have decided. It needs an identity for
  each allocation that survives to the IR: the `ir.SrcPos` already on the instruction plus
  the function symbol is enough, and is exactly the key `FrameEscape.Key()` already uses.
* Disagreements are written to a diffable file, `goc/testdata/escape_shadow_baseline.txt`,
  by a corpus test shaped like `TestFrameEscapeAudit` — same worker pool, same
  ratchet-on-new-lines, same `-update-…-baseline` flag.
* Success condition for the stage: the file exists, it is stable across runs, and every
  line in it has been read.

Two directions of disagreement, and they are not symmetric:

| | new says heap | new says frame |
|---|---|---|
| old says frame | conservative; costs allocations; itemise per source site | — |
| old says heap | — | **permissive; every one is a potential miscompile until argued** |

### Stage 6 — switch the five committed sites, one at a time

**Not before stage 4.** In increasing order of how many frame placements are at risk,
from the full-corpus census:

| order | site | frame placements at risk | total decisions |
|---|---|---|---|
| 1 | `make([]T,·,k)` backing | 720 | 5 061 |
| 2 | method-value descriptor | 867 | 8 659 |
| 3 | `&CompositeLit` | 2 062 | 21 096 |
| 4 | `string`→`[]byte` buffer | 3 380 | 5 840 |
| 5 | slice-literal backing | 4 226 | 8 139 |

`&CompositeLit` is third by count but is the one `bigmod.Nat.Mul` rides on, so it wants
the benchmark run whatever its rank. The string buffer additionally needs the "legalises
to a null argument" form (§1.2), or is left on the AST.

Each is one commit: delete the `heap`/`!heap` branch, call `allocateTyped`, run the
corpus census and the audit, land or revert. The barrier retirement of §1.4 lands with
step 1 and is exercised by all of them.

### Stage 7 — retire what is dead

Only after stage 6 has all five: `nonEscapingAddress`, `nonEscapingAddressWithin`,
`makeResultDoesNotEscape`, `assignedResultDoesNotEscape` and most of
`valueDoesNotEscapeWithin` lose their callers. `addressEscapesFunction`,
`findEscapingCaptures`, `functionLiteralEscapes` and the summary layer **stay** (§3.3).

### What is explicitly *not* in this plan

Moving `findEscapingCaptures` and variable representation to the IR (§3.3). It is a
larger change than everything above put together, it pessimises `-O0` until a collapse
pass lands, and nothing in the two residuals or in the loop-aliasing defect argues for it.

---

## 8. Answer to question 6: acceptance criteria, defined before the work starts

### 8.1 Reference numbers, all from `ddd03eb`

| measurement | reference |
|---|---|
| corpus allocation census, 385 programs | **frame 9 735 471 / heap 509 920** |
| `TestFrameEscapeAudit` | passes against `goc/testdata/frame_escape_baseline.txt` |
| `bigmod.Nat.Mul`, 200 P-256 sign+verify, `goc -O`, native arm64 | **2.844 s** mean of 8 (the pre-fix 2.689 s is what a *successful* rearchitecture beats) |
| capability matrix, both arms | 347 subtests / 346 PASS / 1 EXPECTED FAILURE / 0 FAIL |
| determinism | all 385 corpus programs byte-reproducible in all three configurations |
| compile time, `stdlib_crypto_ecdsa.go` | 6.79 s, of which the escape walk is 0.460 s |

### 8.2 Reject outright — no benchmark can buy these back

1. **Any new line in `frame_escape_baseline.txt`** that is not accompanied by an argument
   for why that publication is safe. The baseline is a ratchet; a rearchitecture that
   loosens it has failed at the thing it was for.
2. **Any allocation introduced on a runtime no-allocation path** — anything
   `reportNoSplitViolations` flags, anything reachable from mark termination or a fatal
   path. This is a correctness failure, not a slowdown; `RUNTIME_PLAN.md` records that
   allocating there deadlocks the collector.
3. **Any capability regression in either matrix arm**, or a loss of byte-reproducibility.
4. **Any shadow-mode disagreement in the permissive direction** (old heap, new frame) that
   has not been individually argued. This is the 2724ac7 / 9f76498 direction.

### 8.3 Accept

* **Allocation count: net heap delta ≤ 0** over the 385-program census — that is,
  `emittedAllocatorCal` ≤ 509 920 — with a per-source-site breakdown for every site that
  moved in either direction. A rearchitecture that is *better* should reduce it: §4.2's
  greatest-fixed-point solve and §4.3's summaries both move allocations frame-ward, and
  §2.1's 3.1% promotion rate is the headroom.
* **Promotion rate: `opt.HeapAllocLoweringStats` must rise from 14 433 / 467 752.** It is
  the single number that says whether the IR pass got smarter or just got more work. If it
  is still near 3% after stage 4, stage 6 is not safe to start and the fact table is not
  doing its job.
* **Per stage, not just at the end.** Every one of stages 1, 4 and 6 changes placement, so
  each gets its own census run and its own per-site table. The stages that change nothing
  (2, 3, 5) must produce a census **byte-identical** to the stage before them; if they do
  not, they are not the stages they claim to be.
* **Benchmarks: no named benchmark more than 2% slower**, measured as the escape-frame-publication
  report measured `Nat.Mul` — 8 alternating runs, report the ranges, and only claim a
  difference when they do not overlap. Named set, at minimum:
  `bigmod.Nat.Mul` (200 P-256 sign+verify), `fmt_sprintf`, `stdlib_encoding_json_roundtrip`,
  `stdlib_encoding_gob_roundtrip`, and one GC-heavy program
  (`runtime_slice_pointer_append_gc`).
* **Compile time: not more than 2% slower** on `stdlib_crypto_ecdsa.go`. Stage 3 alone
  should make it *faster*; if the finished thing is slower than `ddd03eb`, the 0.182 s
  `FrameEscapes` figure says the IR analysis is not the reason and something else needs
  looking at.

### 8.4 The tolerance band, stated honestly

The brief is right to worry: this branch moved 22 sites frame→heap for +5.8% on one hot
path, and a rearchitecture that is slightly more conservative everywhere could cost far
more. So the interesting number is not the total — it is the **per-site** table. A change
that moves 500 cold allocations to the heap and 0 hot ones is fine; a change that moves 3
allocations to the heap and one of them is in `Nat.Mul`'s `default` arm is not. Require the
per-site table at every stage, and read it before reading the totals.

One asymmetry to keep in view while reading it: a frame→heap move costs time, and a
heap→frame move can cost *correctness*. They do not belong in the same budget.

---

## 9. Recommendation

**RECOMMEND HYBRID**, in three parts, in this order:

**(a) Fix the IR pass where it is already wrong, now, independently of everything else.**
Stage 1 is a live miscompile in four allocation forms at both `-O` settings, invisible to
the AST walk, to `FrameEscapes`, and to the whole corpus. The rule that fixes it is
written and run on this branch (`opt/escapeloop.go`, §5.1) and it repairs two of the four
forms. Stage 2 (the checker's phi blindness) is why residual 8a's reproduction passes the
audit. Neither needs a rearchitecture and neither should wait for one.

**(b) Move placement — and only placement — onto the IR.** The neutral op exists, the
legaliser exists and already runs in the right place, and 91.6% of heap-capable
allocations already go through both. Finishing it is five call sites in one file, staged
one at a time behind a shadow-mode diff — but **only after** the IR pass has a fact table,
because the pass as it stands promotes 3.1% of what it sees and the 11 255 frame
placements those five sites make today are exactly the ones it would get wrong (§2.1).

What it buys is exactly what the brief claims:
interface boxing, variadic backing arrays and composite-literal storage stop being things
a syntactic walk has to be taught about and become instructions, and the two-walks-
disagreeing shape of `bigmod.Nat.Mul` stops being expressible. §6.1 shows a live residual
that the IR sees and the AST cannot. And §5.1 shows the sharper version of the same
point: the loop fix repairs exactly the allocations that are already candidates and
cannot touch the ones the AST placed, so *how much of the loop miscompile gets fixed is a
direct function of how much of placement has moved*.

**(c) Do not move the interprocedural layer, and do not move variable representation.**
Keep `findEscapingCaptures`, `addressEscapesFunction`, `functionLiteralEscapes` and the
parameter summaries on the typed AST — not because the IR is a worse place to compute
them, but because their answers are consumed *before any IR exists*: `variableStorage`
picks a different memory representation and `perIterationVariable` picks a different loop
shape, both on the first materialisation of a variable. The neutral-op trick does not
reach that, and making it reach that is a larger change than the entire rest of this plan.
Instead, make that layer good where it is: memoize it (stage 3, provably answer-preserving,
recovers most of a measured 6.8% of compile time), then give it a real SCC fixed point
(stage 4) that is *more* precise than gc's — which cg12 can afford, because compiling
whole-program from source means the summary table is an in-memory map with no export data,
no tags, and no versioning.

The honest summary of the disagreement with the brief: the proposal's diagnosis is right
and its remedy is right for placement, but the circularity it names is not the one that
actually blocks a full move. The blocking one is representation, not placement, and it is
better solved by leaving it where it is.

RECOMMEND HYBRID
