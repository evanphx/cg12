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

## 3. All 74 permissive rows, adjudicated

### 3.1 Method

Three instruments, in decreasing order of authority:

1. **`gc`'s own escape analysis on byte-identical source.** goc's vendored
   `stdlib/src` is byte-identical to `go1.26.1`'s `GOROOT/src` for every file
   involved here (checked with `diff` on `bigmod/nat.go`, `nistec/p224.go`,
   `math/big/int.go`, `net/http/h2_bundle.go`). So
   `go build -gcflags=-m <pkg>` is a sound, production-grade oracle at the exact
   line and column. gc reports an *inlined* allocation at the **call site**, not
   at the constructor -- verified on a 16-line reduction -- which is what makes
   it comparable to a shadow row, whose caller function is post-inlining.
2. **The IR's own per-copy verdicts**, counted from `opt.ShadowPlacement`'s
   undeduplicated output (the committed baseline deduplicates by key; the tool
   in `$TMPDIR/probe` does not). This resolves rows where one function holds
   several copies of one constructor with mixed verdicts.
3. **Reading the source**, where gc is knowably conservative -- generic method
   calls through a `go.shape` dictionary are the case that arises here.

### 3.2 The tally

| | rows | |
|---|---:|---|
| **safe on their merits**, corroborated at the exact site | **60** | 3.3 |
| **safe only because the loop rule blocks them** | **10** | 3.4 |
| **NOT safe** -- the IR is wrong and would miscompile | **4** | 2, 3.5 |
| unadjudicated | **0** | |

The four are the three `h2_bundle.go` rows of section 2, plus
`testdata/runtime_loopvar_value_shapes.go:33:17`, which is a **second and
entirely independent hole** -- section 3.5. It is also not in a loop.

### 3.3 The 60 that are the AST walk's pessimism

**(a) `return &T{...}` from a constructor the IR then inlines -- 32 rows.**
`nonEscapingAddressWithin`'s parent walk has no `*ast.ReturnStmt` case, so it
falls to `default: return false`: **every `return &T{...}` in the tree is a heap
allocation for goc, whatever the caller does.** The IR decides after
`opt.InlineHeapAllocations`, in the caller, and `opt/inline.go`'s new
`PlacedAllocs` propagation is what carries the question there. Upstream says so
in as many words at `bigmod/nat.go:71`: *"NewNat inlines, so the allocation can
live on the stack."*

| goc caller (row) | gc at that function | verdict |
|---|---|---|
| `bigmod.Nat.{Exp, ExpShortVarTime, Mul, Sub, SubOne, maybeSubtractModulus, montgomeryReduction, shiftIn}` | 16/1/5/1/1/1/1/1 sites, **0 escaping** | safe |
| `ecdsa.precomputeParams[P224/P256/P384/P521]` (4 rows) | 8 sites, 0 escaping | safe |
| `rsa.{checkPrivateKey, decrypt, encrypt}` | 20/7/2 sites, 0 escaping | safe |
| `nistec.{P224,P384,P521}Point.{SetBytes, ScalarMult, ScalarBaseMult}`, `{p224,p384,p521}Table.Select` (12 rows) | 1/17/2/1 sites each, 0 escaping | safe |
| `bigmod.extendedGCD` | 6 sites: 4 safe, 2 escaping (`u`, `A` -- the *named results*) | safe: **the IR frames exactly 4** |
| `nistec.PxxxPoint.generatorTable.func.393.28` (3 rows) | 3 sites: 1 safe (`base := NewP224Point()...`), 2 escaping (stored into the package-level table) | safe: **the IR frames exactly 1** (counted on p224; p384/p521 are the same source, generated from one template) |
| `rsa.newPrivateKey` | 8 sites: 7 safe, 1 escaping | safe, by argument: the escaping copy is the one stored into the returned `&PrivateKey{...}`, and a store into a `runtime.newobject` result is not `cLocal`, which is the one rule `analyzeCandidateEscapes` applies most directly. **The IR frames 2 of the 7** -- less precise than gc, never more. |

**(b) `&T{...}` passed straight to a call -- 12 rows (of 15; 3 are section 2).**
`nonEscapingAddressWithin` accepts exactly four parents: `ParenExpr`, a
*one-argument type conversion*, `StarExpr`, and a `FieldVal` `SelectorExpr`.
There is **no call-argument case at all** -- it never asks
`parameterDoesNotEscape`, which the sibling predicate `valueDoesNotEscapeWithin`
*does* ask for ordinary values. goc's AST walk is internally inconsistent here,
and that inconsistency is most of this class.

| row | gc |
|---|---|
| `bigmod/nat.go:951,961,968,975` -- `x.Mod(&Nat{limbs: T}, m)` (4) | does not escape |
| `x509/parser.go:1011` -- `parsePublicKey(&publicKeyInfo{...})` | does not escape |
| `poll/splice_linux.go:198` -- `destroyPipe(&splicePipe{...})` | does not escape |
| `math/big/int.go:920` -- `(&Int{abs: z}).ModInverse(&Int{abs: g}, &Int{abs: n})` (2) | does not escape (all three literals) |
| `testdata/runtime_type_param_method_shapes.go:229,232,236,239` (4) | **escapes to heap** -- gc is conservative, see below |

The four generic ones are the case where the oracle is wrong and the IR is
right. `scorePointer[P pointerScorer](value P) int { return value.Score() }` and
`countCell[P cellLike](c P) int { return c.Count() }` -- gc compiles these once
per *shape* and dispatches `Score`/`Count` through the dictionary, so it cannot
see the callee and assumes the receiver escapes. `scoreValueA.Score()` has a
**value** receiver and reads one field; `(*cell[T]).Count()` reads one field.
Neither can retain anything. goc monomorphises and the IR sees the real body.
This is a proof from the source, not a vote.

**(c) `var z = &[32]byte{...}` -- 4 rows.** `mlkem/cast.go:23:11`, one per
`init` instantiation. gc: does not escape.

**(d) slice literals -- 12 rows (of 13; 1 is section 3.5).**
`crypto/tls/auth.go:259`, `x509/root_unix.go:37`,
`httpcommon/httpcommon.go:79`, `net/http/servemux121.go:196`,
`net/ipsock_posix.go:45`, `fstest/testfs.go:611`,
`testdata/bytes_grow_allocs.go:12,13`,
`testdata/runtime_append_self_overlap.go:4`,
`testdata/runtime_range_target_forms.go:46`,
`testdata/runtime_range_target_order.go:160`,
`testdata/runtime_slice_copy_overlap.go:4`.
gc: **does not escape at every one.** These go through `valueDoesNotEscape`,
which *does* ask about callees, so they are the walk's ordinary conservatism
(an `append` destination, a `range` subject, a `copy` operand) rather than a
structural gap.

### 3.4 The 10 the loop rule blocks -- 4 of them load-bearing, 6 of them cost

| rows | what | is the loop rule doing work? |
|---|---|---|
| 4 | `escaping-typed`, position `?`, in `tls.Config.getCertificate`, `tls.Conn.getClientCertificate`, `tls.pickECHConfig`, `main.main` | **YES.** These are `freshVariableStorage` (`goc/compile.go:5626`) -- the per-iteration cells `perIterationVariable` demands for a capture that escapes. The IR says frame because nothing publishes them *within the frame*; promoting collapses every iteration onto one slot. This is the case `RUNTIME_PLAN.md` 5.9 exists for and the only thing standing in front of it is the loop column. |
| 4 | `bigmod/nat.go:1158-1163` -- `A.Add(C, &Modulus{nat: m})` inside `for {}` | No. gc: **does not escape**. Nothing survives the iteration, so one slot for all iterations is unobservable. The loop rule rejects these as collateral. |
| 1 | `xml/marshal.go:1118` -- `s.p.writeStart(&StartElement{...})` | No. gc: does not escape. Collateral. |
| 1 | `strings/replace.go:86` -- the `[]byte{o}` backing of `string([]byte{o})` | No. gc: `[]byte{...} does not escape` (the *string* it builds escapes; the backing does not). Collateral. |

So the loop rule is **necessary but blunt**: it earns its place on 4 rows and
costs precision on 6. It cannot be relaxed on the evidence here -- distinguishing
the two needs a notion of "does any observer of this object survive the
iteration", which neither analysis has.

### 3.5 The second hole: a candidate loses its identity across a write barrier

`testdata/runtime_loopvar_value_shapes.go:33:17`, `main.main`,
`slice-literal-backing`, **not in a loop** (the allocation is in the `for`-init,
which runs once). gc: `[]int{...} escapes to heap`, and gc is right --

```go
for numbers := []int{0}; len(numbers) < 4; numbers = append(numbers, len(numbers)) {
    captured = append(captured, func() string { return fmt.Sprint(numbers) })   // closure outlives
}
```

The emitted IR, read at `loc 33 17`:

```
%t259 =p alloc8 24                          ; the slice header, a frame slot
%t261 =p call $runtime.newobject(...)       ; <-- the candidate, the [1]int backing
call $goc_storep(p %t259, p %t261)          ; backing pointer -> the header, THROUGH THE BARRIER
@forheader85
%t266 =p loadl %t259                        ; reload it
%t271 =p call $runtime.newobject(...)       ; the per-iteration cell (heap, correctly)
call $goc_storep(p %t271, p %t266)          ; <-- publication into the heap cell
```

The publication on the last line *is* reached: the `isAtomicPointerStore` arm of
`analyzeCandidateEscapes` sees a barrier into non-`cLocal` storage and calls
`mark(%t266)`. It does nothing, because **`%t266` has no base**. The
base-propagation fixed point (`opt/escape.go:216-302`) recognises exactly two
ways a candidate can enter a frame slot -- `instruction.Op.IsStore()` and
`memoryCopyOperands` -- and a write barrier is **an `OCall`, so neither**.
`slotBases[{%t259,0}]` is never written, the reload recovers nothing, and every
later use of the reloaded pointer is invisible to the analysis.

That is:

> **A candidate stored into a local slot through `goc_storep` -- which is what
> goc emits for every pointer field of every pointer-bearing local -- is
> untracked from that point on. The marking switch models the barrier; the
> base propagation does not. The two halves of the same analysis disagree about
> what a barrier is.**

This one is **not summary-dependent**. The same code runs with `facts == nil`,
so the mechanism is present in the pass `main` ships today for `ir.OHeapAlloc`
candidates. Whether any *current* neutral candidate is reachable this way I did
not establish -- the corpus evidence here is about front-end-placed allocations,
which the shipping pass never sees. **It needs its own check before the fix in
section 2.4 is written, because a fix to the summary side would not touch it.**

## 4. THE KNOB IS INERT. Byte-identical, corpus-wide, both `-O` settings.

Two independent proofs, one by reading and one by measuring, plus a control that
shows the measurement has power.

### 4.1 By reading: nothing off the knob reaches the emitted code

`opt.EscapeSummaries` is read from the environment once, at package
initialisation (`opt/escape.go:22`), and has exactly **one** consumer
(`opt/escape.go:68`). With it false, `LowerHeapAllocations` passes `facts == nil`
to `lowerHeapAllocations`, and then:

- `moduleFuncsByName` returns `nil` without building the index;
- the new summary arm of `analyzeCandidateEscapes` is guarded
  `case facts != nil && instruction.Op == ir.OCall && ...` -- **unreachable**,
  so every call falls into the same assume-the-worst `default` arm it always did;
- `leakedCallResultBase` is called only under `if !ok && facts != nil`.

Everything else the branch adds to the default path writes diagnostic state that
nothing in the compiler reads:

| addition | read by |
|---|---|
| `ir.Func.PlacedAllocs` (`goc/compile.go` `recordPlacement`, 11 sites) | `opt.ShadowPlacement` and `opt.AllocationCensus` only |
| `ir.Module.AllocDecisions` (`recordAllocDecision`) | `opt.HeapAllocLoweringStats`, `opt.AllocationCensus` only |
| `ir.SymNoEscape` (`registerNoEscapeDirectives`) | `opt/escapefacts.go:185,318` and `opt/escapesummary.go:126` -- **both in the summary path only**. `ir.Module.SymAttrs` is not serialised (`goc/compile.go:1400`). |
| `opt/inline.go` +17 lines | copies `PlacedAllocs` onto the inlined temporaries; touches nothing else |

`grep` for `EscapeSummaries` over the tree returns four hits: the declaration,
the one consumer, and the test that toggles it around its own corpus run.

### 4.2 By measuring: 770 linked executables, byte for byte

Two `cmd/goc` binaries, one from `main` (`05946f2`) and one from this branch,
each built from `git archive` unpacked at **the same absolute path**
(`$TMPDIR/build`) -- necessary because `goc/source_import.go:334` bakes in
`runtime.Caller(0)`'s path to find `stdlib/`, and a different build directory
puts different file-name strings in the image and changes 930 485 bytes of an
otherwise identical binary. With the path held equal:

- every `goc/testdata/*.go` -- **385 programs** -- compiled and **linked to a
  final executable** by both compilers, with `GOC_ESCAPE_SUMMARIES` unset;
- once without `-O` and once with;
- **770 of 770 pairs are sha256-identical. Zero differences, zero compile
  failures on either side.**

The comparison is the linked executable, not the IR: it includes the data
section, relocations, pointer-word maps and the typelink flags that
`ir.Module.String()` omits.

**The control.** The same 770 compiles with `GOC_ESCAPE_SUMMARIES=1` differ from
`main`'s output in **770 of 770** cases. So the harness is comparing something
that can move, and the knob is what moves it. (`goc` itself is deterministic:
two runs of the branch compiler on the same input give the same sha256.)

### 4.3 A third witness, already committed

`goc/testdata/alloc_census_baseline.txt` -- 18 713 lines, one per allocation
site in the corpus with where it landed -- is **byte-identical between this
branch and `origin/ccwork/escape-alloc-census`**, a branch that contains no
summary code at all (`git diff origin/ccwork/escape-alloc-census HEAD --
goc/testdata/alloc_census_baseline.txt` is empty). With the knob off, not one
allocation in the corpus moved.

**Verdict: airtight. The branch can sit on `main` unfinished.**

---

## 5. The prize, quantified -- and it is not where the plan expects it

### 5.1 On the candidate population: 3.09% -> 3.72%

`go test ./goc -run TestEscapeSummaryPromotionRate -escape-promotion-rate`,
the whole corpus compiled twice (302 s):

| | promoted | lowered | rate |
|---|---:|---:|---:|
| knob off | 14 433 | 453 319 | **3.09%** |
| knob on | 17 414 | 450 338 | **3.72%** |

Same 467 752 candidates both runs. **+2 981 objects, +20.7% relative, +0.63
points absolute.** The table is doing real work -- an intraprocedural pass
cannot promote a candidate that reaches any call -- but the population it works
on is small, because it is *only what the front end already declined to place*.

### 5.2 On the population the migration would actually move: the summaries LOSE

The corpus shadow run over the same 385 programs:

```
front-end placements evaluated: 189094  (frame 157641, heap 31453)
agree 180626;  front frame -> IR heap 8008;  front heap -> IR frame 460
distinct front-end placement sites: 5469 (frame 2986, heap 2483)
distinct disagreement sites: 341
```

The front end frames **157 641 of 189 094 = 83.4%** of its own placements. Route
every one of those decision sites through the IR and the summary-fed analysis
frames `157641 - 8008 + 460 = 150 093 = 79.4%`.

> **The migration as it stands is worth -7 548 frame placements, or -4.0 points,
> on the population it moves.** Adding §5.1's +2 981 on the candidate side, the
> whole-program net is **-4 567 objects moved from frame to heap**. Today's
> arrangement -- AST walk places, IR pass decides the leftovers -- promotes more
> than an all-IR pass with these summaries would.

And the ceiling is low: fix *every one* of the 267 conservative sites and the IR
frames 158 101 of 189 094 = 83.6%, **+0.24 points** over the AST walk. The
placement population is not where the win is.

### 5.3 So what is the hybrid for?

On these numbers the justification is **not** promotion count. It is:

- **compile time** -- §0's measurement, unchanged and still the strongest case:
  the AST walk is 0.456 s of a 6.47 s compile (7.1%), 0.28 s of it rebuilding
  parent maps 12 612 times to answer 1 485 distinct questions; the IR fixed
  point over the same 6 965 functions is 0.134 s and answers each question once;
- **one analysis instead of two**, with the placement decision expressible in
  the IR at all (today a committed frame placement is an `OAlloc`
  indistinguishable from a variable slot, which is why the census had to be
  invented to ask the question);
- **+2 981 objects** on the candidates, which is the honest optimisation figure.

A migration proposal that leads with "3.1% -> 3.7%" is defensible. One that
leads with "the IR places better than the walk" is not, on this evidence.


---

# LANDING (job `escape-summaries-land`, continuing at `41af1be`)

Sections 6 onward. The decision is settled by sections 2-5 and is not
relitigated here: the AST walk stays the placer, the IR migration is dropped,
and what lands is the summary table feeding `LowerHeapAllocations`' own
candidate population. Everything below is measured on this tree.

## 6. Both safety holes are fixed, and both reductions fail on the unfixed tree

### 6.1 Hole one: `ParamNoEscape` now means the same thing on both sides

`opt/escapegraph.go`'s `call` used to consume its own output as a stronger claim
than it produces. Fixed by taking §2.4's second option -- the consumer states
the summary's actual claim rather than rounding it up:

```go
// escapeGraph.call, for every argument of a call with a usable summary
graph.flow(escapeHeapLoc, argument, 1)   // "I do not believe you keep nothing it points at"
switch fact := graph.facts.Param(callee, index); fact.Escape {
case ParamNoEscape:
        // Nothing further: the callee cannot retain the pointer itself.
```

Depth 1 is the exact statement of what `summary()` produces: a parameter is
`ParamNoEscape` when the heap reaches it at a dereference count of **one or
more**, so the pointee is published and the pointer is not. `ParamEscapes`
already flows at depth 0, which subsumes it.

**Why it costs almost nothing.** The extra edge only bites where the argument is
one dereference *below* storage the caller owns -- an `OAlloc` slot address,
which is goc's convention for any aggregate wider than a couple of words, and
every `&x`. An ordinary pointer parameter forwarded to a callee is still reached
at depth 1 and still summarises `ParamNoEscape`; a pointer loaded out of a frame
slot is reached at depth 2. `TestEscapeFactsForwardedPointerStillDoesNotEscape`
is the control that says so, and it is what distinguishes this fix from
collapsing the table to "everything escapes".

`//go:noescape` is deliberately *not* changed: `calleeRetainsNothing` keeps the
strong reading, because a directive is a promise about the whole argument
written by whoever wrote the assembly -- which is how gc reads it -- whereas a
computed summary is a depth this analysis derived. The existing
`TestEscapeFactsDataSymbolIsNotAClosure` encodes that reading and still passes.

### 6.2 Hole two: the base propagation now knows what a write barrier is

`opt/escape.go`'s propagation recognised two ways a candidate enters a frame
slot, `Op.IsStore()` and `memoryCopyOperands`, and a write barrier is an `OCall`
so it was neither -- while the *marking* switch in the same function has
understood the barrier all along. New `trackedPointerStore` gives both halves one
answer:

```go
func trackedPointerStore(function *ir.Func, instruction ir.Instr) (value, address ir.Ref, ok bool) {
	if instruction.Op.IsStore() {
		return instruction.Arg(0), instruction.Arg(1), true
	}
	if isAtomicPointerStore(function, instruction) {
		return instruction.Arg(2), instruction.Arg(1), true   // dst is Arg(1), value Arg(2)
	}
	return ir.R, ir.R, false
}
```

This one is **not summary-dependent**: it runs with `facts == nil`, so it is a
fix to the pass `main` ships today, not only to the new path.

### 6.3 The reductions, as tests that fail on the unfixed analysis

Four new tests, stated as IR because that is what the analysis reads:

| test | file | on the unfixed tree |
|---|---|---|
| `TestEscapeFactsPointerPassedInsideAFrameAggregateEscapes` | `opt/escapefacts_test.go` | **FAILS**: `hold`'s parameter is `noescape` (`0x2`), must be `escapes` (`0x0`) |
| `TestEscapeFactsForwardedPointerStillDoesNotEscape` | `opt/escapefacts_test.go` | passes -- it is the precision control |
| `TestLowerHeapAllocationsTracksEscapeThroughAWriteBarrieredLocalSlot` | `opt/escape_test.go` | **FAILS**: the object is promoted (`OAlloc8`, `0x22`), must stay an allocator call (`OCall`, `0x37`) |
| `TestLowerHeapAllocationsAllowsAWriteBarrieredLocalSlot` | `opt/escape_test.go` | passes -- the control that the newly-followed slot did not become a slot the analysis gives up on |

Verified by reverting `opt/escape.go`, `opt/escapegraph.go` and
`opt/escapesummary.go` to `41af1be` with the tests in place: exactly the two
regression tests fail, exactly the two controls pass. `go test ./opt` is green
with the fixes in.

### 6.4 The same two reductions through the front end, and the corpus chain

`goc/escapesummary_reduction_test.go` compiles both reductions from Go. Both use
`CompileExecutable`, not `Compile`: the aggregate calling convention and the
write barrier are what the defects are about, and a package compiled on its own
gets neither -- `goc.Compile` emits `calloc` and plain stores, and the §2.2
reduction does not reproduce under it at all.

| reduction | unfixed | fixed |
|---|---|---|
| §2.2, `hold`/`publish`/`stash` | `main.hold: 0:noescape`, and `red3.go:13:21 main.stash composite-literal heap -> frame` | `main.hold: 0:escapes`, zero permissive rows |
| §3.5, reduced to ten lines from the corpus row | `barrierescape.go:7:17 main.Test slice-literal-backing heap -> frame` | zero permissive rows |

Both assert the same end property -- `opt.ShadowPlacement` reports no
`heap -> frame` row anywhere in the reduction -- which is the direction 2724ac7
got wrong, and both name the mechanism as well (the summary; the barrier count),
so neither can pass by compiling to something that no longer contains the case.

§3.5's reduction is ten lines rather than the corpus program:

```go
var captured []func() int
func Test() int {
	for numbers := []int{0}; len(numbers) < 4; numbers = append(numbers, len(numbers)) {
		captured = append(captured, func() int { return len(numbers) })
	}
	return len(captured)
}
```

The allocation is in the for-init, so it runs once and the loop rule has nothing
to say about it. The header is an `alloc8 24` the front end addresses directly,
which is what makes it the barrier case: goc double-indirects a *named* local, so
a load of the pointer-to-storage slot already defeats `locOf` and the object
escapes by a blunter route. That is why the first draft of this reduction, an
ordinary struct local, passed on the unfixed tree.

### 6.5 The corpus chain of §2.1, before and after

`go test ./goc -run TestEscapeSummaryFacts -escape-summary-program
testdata/stdlib_http_client_server.go -escape-summary-symbol http2serverConn.write`:

| symbol | §2.1, before | now |
|---|---|---|
| `$net/http.http2serverConn.writeFrameFromHandler` | `0:noescape 1:noescape` | `0:noescape 1:noescape` |
| `$net/http.http2serverConn.writeHeaders` | `0:noescape 1:noescape 2:noescape` | **`0:noescape 1:escapes 2:escapes`** |

`writeFrameFromHandler`'s answer is unchanged and still correct: it does not
retain the frame-slot address it was handed. What changed is that its *caller*
now models what it does with the contents, so `headerData` -- parameter 2, an
ordinary pointer -- comes out `escapes`, and `writeChunk` keeps
`&http2writeResHeaders{}` on the heap. That is the three-frame chain of §2.1
closed at its source.

And on `runtime_loopvar_value_shapes.go`, the §3.5 row is gone: the program's
only remaining permissive disagreement is `splice_linux.go:198:15`, which §3.3(b)
adjudicated safe against gc.
