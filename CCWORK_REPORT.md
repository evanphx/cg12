# Asking the escape question for a variadic call: splitting the object that made it unanswerable

Branch `ccwork/variadic-escape-question`, off `ccwork/iface-convt-fastpath`
(`19488ee`). The previous jobs' reports are at `19488ee:CCWORK_REPORT.md` and
`4a6fd96:CCWORK_REPORT.md`.

Status: COMPLETE. Every number below was measured, under goc built from this
branch and under `go run`, and watched to completion.

**Headline: `fmt.Sprintf("value=%d", 42)` costs goc 1.00 allocations against
gc's 1.00 — exact parity, from 2.00. The `[N]any` backing
array of a variadic call is now a frame slot wherever the callee does not retain
the slice itself, and the boxed payload an element points at is decided
separately from it. The combined object was split, partially and deliberately;
section 2 prices both directions. The retention hole that forced the previous
attempt back to 2.00 is closed by construction rather than by an extra rule: the
callee that keeps `args[0]` now keeps a payload that is its own allocation, and
that payload goes to the heap while the array does not.**

## 1. What was actually wrong, confirmed before anything was changed

Two instruments, both on the base commit.

`goc/compile.go:6581` on the base commit (`goc/compile.go:6626` here, and
`:6428` on `main` before the interface-conversion branch moved it) decides
between a frame `[N]any` and a heap one:

    stackAllocateVariadic := !g.runtimeAllocation || g.fn.NoSplit || g.forceStackVariadic

and `forceStackVariadic` comes from a two-symbol allowlist. So the front end
never asks. That is true and it is not the whole story: the heap arm emits the
*neutral* `ir.OHeapAlloc` candidate, and `opt.LowerHeapAllocations` — which runs
unconditionally, `goc/compile.go:488`, not only under `-O` — does ask. The
question is asked; the representation is what made it unanswerable.

The lines under it build one synthesized `struct{values [N]any; payload0 T0;
...}` per call site and allocate the backing array and every boxed payload as a
single object. One object is one placement.

A diagnostic added to `opt` for this job (`GOC_DIAG_ESCAPE`, deleted before the
branch closes) prints where each candidate landed and the first use that escaped
it. On the base commit, for `fmt.Sprintf("value=%d", n)`:

    main.doSprintf .goc.runtime.type.struct_values__1_any__payload0_int  heap
        argument 1 of $fmt.Sprintf may retain something inside a self-referential object

and with the `needsDeepSummary` rule switched off, the same object is `frame`.
So **`fmt.Sprintf`'s `a []any` parameter does not escape at depth 0** — the
array was on the heap solely because the box inside it is retained.
`fmt.pp.doPrintf` assigns each element to `p.arg`, a field of a heap-allocated
printer, so the box genuinely is retained: this is not a conservatism to be
analysed away.

For `log/slog.Logger.Info` the same diagnostic says something different:

    argument 2 of $log/slog.Logger.Info escapes            (with the deep rule off)

and the parameter table agrees:

    FACT log/slog.Logger.Info param 2 "args.0" = escapes deep=false

The slice **itself** escapes there, through `Logger.log` → `Record.Add` →
`argsToAttr`, which returns a slice derived from `args` in a loop. That
difference decides the design, and section 2 is about it.

## 2. Pricing the two representations, which is the whole design

The brief asks what the combined-object representation costs and whether
splitting is the cheaper path. Both were measured, in that order, and the answer
is **neither on its own** — which is why the final shape is a split with a
fold-back rather than one or the other.

### 2.1 What the combined object costs

Exactly the remaining gap, and no analysis can remove it. One object is one
placement, so the array's placement is the *maximum* of the array's own answer
and every payload's. `fmt.Sprintf`'s array is `noescape` at depth 0 and its box
is genuinely retained, so the max is "heap" and the array pays an allocation gc
does not. This is not conservatism: `p.arg = arg` in `fmt.pp.doPrintf` really
does keep the box, and any sound analysis must say so.

Measured, base commit: `fmt.Sprintf("value=%d", 42)` = **2.00** allocations
against gc's 1.00, and the whole of the difference is the `[1]any`.

### 2.2 What splitting every payload costs

Built, measured, and rejected. With the array and every convertible payload as
separate candidates:

| case | base | split-everything | gc |
|---|---|---|---|
| `fmt.Sprintf("value=%d", 42)` | 2.00 | **1.00** | 1.00 |
| `slog.Info` 5 attributes | 1.00 / 288 B | 1.00 / 240 B | 0.00 |
| `slog.Info` 3 large integers | 1.00 / 176 B | **4.00** / 168 B | 3.00 |

The last row is the reason. `log/slog.Logger.Info` retains its `args` slice at
depth 0, so its array goes to the heap whatever the payloads do; three payloads
split out of an escaping array are three more allocations, and goc crossed from
*better than gc* (1.00) to *worse* (4.00 against 3.00). Splitting is right where
the array stays in the frame and wrong where it does not, and that is not
knowable when the front end emits the call.

### 2.3 What was built instead

The split, with somewhere to put it back.

- A payload the runtime has a **conversion helper** for (`runtime.convT16/32/64`
  — integers, bools, floats, small pointer-free types) is emitted as an
  allocation candidate of its own, by `ir.Block.HeapAllocConvertedField`, and the
  field the combined object reserved for it stays reserved.
- `opt.LowerHeapAllocations` folds it back to `container+offset` when the
  container escaped anyway, and when more than one payload escaped out of a
  container that did not — because K escaping payloads cost K allocations where
  the combined object costs one.
- Everything else stays a field of the combined object, exactly as before. A
  payload with no helper — a string, a struct, a `slog.Attr` — has no cheaper
  form to be split into, and splitting it out of a mixed call would only add an
  allocation to calls like `fmt.Sprintf("%s %d", s, n)` whose array is pinned by
  the string payload anyway.

The comparison is of **upper bounds**, deliberately. `convT64` allocates nothing
for a value below 256 and one object above it, so K split payloads may cost
nothing at all — but which values a call site sees is not something a compiler
knows, and choosing the representation on a guess about them would make the cost
of a `%d` depend on how big the number turned out to be.

### 2.4 The retention hole, and why it cannot come back

The `variadic-allocations` job promoted a merged object into the caller's frame
on a depth-0 `ParamNoEscape`, and a callee that kept `args[0]` was then holding
a pointer into a returned frame. It backed the change out to 2.00.

That hole is not avoided here by an extra rule. It is **not expressible**: the
thing the callee retains — the box — is a different allocation from the thing
that stays in the frame — the array — and the analysis gives them different
answers. `TestLowerHeapAllocationsEscapesOnlyThePayloadWhenTheCalleeRetainsAnElement`
is that case as a unit test, `variadic_element_retention.go` is it as a running
program whose retained value survives a stack copy, and
`variadic_retained_element` is it as an allocation count that would fail at 0.00.

What the design does owe is the reverse direction — everything inside an object
that escapes must escape with it — and that is `containedAllocationsEscape`, run
after the mark loop, again after the loop rule, and again after the fold-back
decision. The loop rule is the sharp one: it escapes candidates *after* the
analysis has finished, and a container it sends to the heap with a promoted
payload inside it is a frame address published into a heap object.

Containment is taken from the **declaration** — the candidate names its
container — rather than inferred from stores. Inferring it was implemented
first, and the allocation census is what rejected it: **585 sites moved from the
heap into frames**, because "a tracked pointer written into another tracked
allocation is contained, not published" is true of every `new(T)` in the tree,
and it opens a hole of its own. A pointer loaded back *out* of a container that
did not escape would then escape neither, and nothing tracks the loaded value.
The declared form is a closed set — goc emits these candidates at one site, for
one shape, and hands the container to the callee without ever reading it back —
and the load and block-copy cases escape a container's contents if anything ever
does read it back, so the closure is checked rather than assumed.

## 3. The summary machinery, and the variadic parameter it now describes

The brief points at `goc/compile.go:3532`, which says the escape summary "does
not describe a variadic parameter, whose argument is an element of a slice the
callee builds rather than the parameter itself". Both halves of that were true
and they needed opposite treatment.

### 3.1 `opt`'s fact table: the question it could already answer

`ParamFact.Deep` is the depth-1 claim — nothing reachable *through* the
parameter is retained — computed in `escapeGraph.summary` from `heapLeak` and
`resultLeak` one dereference further out. For a slice parameter, depth 1 is the
elements, so **`Deep` already is the variadic-element fact**. What was missing
was a consumer: it was read only for a *self-referential* allocation, which is
the shape the merged object happened to have.

`markSummarisedCall` now applies it generally. A `!Deep` argument escapes what
the argument *contains* — the declared payloads — without escaping the argument.
That is the summary describing a variadic parameter: "this callee may keep an
element" is now a thing a caller can act on, and acting on it costs the payload
rather than the array.

### 3.2 The front end's AST walk: the question it must refuse — and a bug

`parameterLeaksOnlyToResult` already refused the variadic parameter.
`parameterDoesNotEscape`, the predicate immediately above it, did not — and it
is handed an *argument* position by all five of its callers. For a variadic
call, argument positions from the last parameter onward name elements. Positions
past the parameter list were already refused; the position that lands exactly on
the variadic parameter was answered with the slice's summary.

**That is a live miscompile on the base commit.**

```go
func keepFirst(args ...*addressBox) { sink = args[0] }
func caller() { var local addressBox; ...; keepFirst(&local) }
```

`keepFirst` retains no pointer it was handed, so the walk answered "does not
escape" and `local` stayed in `caller`'s frame with a package-level variable
pointing at it. The collector finds it:

    runtime: pointer 0x60ccc3997d40 to unallocated span ... span.state=0
    runtime: found in object at *(0x5a0028+0x8)
    fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)

Reproduced on `19488ee` (the base) and on `4a6fd96` before it, so it is not
something this branch made. The reduction is committed as
`goc/testdata/variadic_element_address_retention.go`, with its expected output
checked against the host toolchain, and the walk now refuses the question the
way the predicate next to it always did. **No measured allocation count moves.**

A spread call, `f(xs...)`, does hand the parameter over as itself and loses
precision under the refusal. That is the price of one predicate serving both
shapes, and it is the safe direction. Answering it properly needs the argument's
ellipsis at the call, which is available at all five call sites and is a
worthwhile follow-up; it is not attempted here alongside the rest.


(filled in below as the numbers land)

## 4. The numbers

Host toolchain `go version go1.26.1 linux/arm64`, pinned in both baseline files.
Every number below was produced by running the program, under goc built from
this branch and under `go run`, not by reasoning about the generated code.

### 4.1 `goc/testdata/allocation_counts.go` — allocations per call

| call | base | **this branch** | gc |
|---|---|---|---|
| **`fmt.Sprintf("value=%d", 42)`** | 2.00 | **1.00** | **1.00** — parity |
| `fmt.Sprintf("value=%s", s)` | 2.00 | 2.00 | 2.00 — parity |
| `fmt.Sprintf("value=%v", struct)` | 2.00 | 2.00 | 2.00 — parity |
| `fmt.Sprintf("a constant format")` | 1.00 | 1.00 | 1.00 — parity |
| `f(...int)` with 2 args | 0.00 | 0.00 | 0.00 — parity |
| `f(...any)` with an int and a string | 0.00 | 0.00 | 0.00 — parity |
| **`keepElement(0x5eed)`, callee stores `args[0]` into a global** | 1.00 | **1.00** | 1.00 — parity |
| **the same with a struct payload** | 1.00 | **1.00** | 1.00 — parity |
| `fmt.Sprintf("%d/%d", 42, 42)` | 2.00 | 2.00 | 1.00 |
| `fmt.Sprintf("%d/%d", 1<<20, 1<<20)` | 2.00 | 2.00 | 3.00 — goc is cheaper |
| `fmt.Sprintf` inside a loop body | 2.00 | 2.00 | 1.00 |
| `f(...int)` inside a loop body | 1.00 | 1.00 | 0.00 |

The six `box_*` rows, the three `return_any_*` rows and `sync_pool_round_trip`
are unchanged in both columns; nothing outside the variadic rows moved.

**The headline row is at parity and the whole of it is the split.** The array is
a frame slot and the box is built by `runtime.convT64`, which for 42 returns a
pointer into `runtime.staticuint64s` and allocates nothing. gc's one allocation
and goc's are the same object: the result string.

The two `%d/%d` rows are the fold-back being visible. goc pays two allocations
for both — the result string and one combined object — where gc splits
unconditionally and pays one or three depending on the values. The row that
matters for judging the rule is the second: goc is *cheaper* than gc there, and
splitting unconditionally would have given that up to win the first.

The two `_in_loop` rows are unchanged and remain the price of
`opt.promotionsBlockedByALoop`, which is the loop-aliasing job's rule and not
this branch's to lower.

### 4.2 `log/slog` — the cases the brief named

Regenerated `goc/testdata/slog_allocations_baseline.txt`. **Thirty-one of the
thirty-two rows are byte-identical to the base commit**, allocation counts and
byte counts alike; the diff against the base is one row, discussed below.

| case | goc | gc |
|---|---|---|
| **`info/5-attr`** | **1.00 / 288 B** | **0.00** |
| **`disabled/3-attr`** | **1.00 / 176 B** | **0.00** |
| `info/3-attr-large-ints` | 1.00 / 176 B | 3.00 / 24 B — goc is cheaper |
| `control/variadic-6-preboxed` | 0.00 | 0.00 — parity |
| `control/variadic-6-literal` | 0.00 | 0.00 — parity |

**slog does not improve, and the reason is not the representation.**
`log/slog.Logger.Info`'s `args` slice escapes at **depth 0** — the summary says
`escapes`, not `noescape`+`!Deep` — through `Logger.log` → `Record.Add` →
`argsToAttr`, which returns a slice derived from `args` and assigns it back in a
loop. The array therefore goes to the heap however the payloads are represented,
and the one allocation slog pays is that array with every payload folded into
it. Improving it means making that chain's summary answer `noescape`, which is a
summary problem in `Record.Add`'s loop-carried leak-to-result and not something
the variadic call site can fix. The intermediate build that split
unconditionally measured `info/3-attr-large-ints` at **4.00 against gc's 3.00**,
which is what the fold-back exists to prevent.

One row is not identical: **`json/kv-4-pairs` used to kill the program under goc
and now runs**, at 5.00 allocations / 320 B against gc's 2.00 / 24 B. That is
**not a fix**. The two reductions the slog job committed
(`goc/testdata/slog_allocations/miscompiles/attr_bad_pointer.go` and
`attr_bad_pointer_stackcopy.go`) still reproduce at this HEAD, byte for byte —
`bad pointer in frame main_main at 0x…: 0xc8`. The frame pointer map is still
wrong for a `slog.Attr`; this branch moved the objects around it enough that
this particular program stops meeting a collection with one live in a frame.
`json/logattrs-4-attrs` still crashes. The JSON output goc now produces for that
shape is byte-identical to the host toolchain's, checked separately, so the row
is a real measurement and not a program that quietly stopped working.

### 4.3 The gc differential

Regenerated `goc/testdata/escape_gc_differential.txt`, and verified to reproduce
itself: two `-update` runs produce byte-identical files, and the test passes
against the committed one without `-update`.

|  | base | this branch |
|---|---|---|
| **PESSIMISTIC** (goc heaps, gc does not) | 567 lines | **573** |
| PERMISSIVE (gc heaps, goc does not) | 209 lines | **233** |
| confusion matrix, goc heap × gc frame | 183 | **172** |
| confusion matrix, goc mixed × gc mixed | 8 | **30** |
| joined source lines | 3 059 | 3 070 |

**The permissive set grows by 25 lines, and this is the direction that has to be
argued rather than counted.** Five are in files this branch adds or edits. The
other **20 are all one shape, with no exceptions** — checked by pattern over the
file, not by sampling:

    fmt_sprintf.go:6   mixed -> mixed
        src  formatted := fmt.Sprintf("value=%d", 42)
        goc  col 27  frame  runtime.newobject  struct_values__1_any__payload0_int
        goc  col 39  heap   runtime.convT64    int
        gc   col 26  frame  slice   ... argument
        gc   col 39  heap   object  42

goc frames the `[N]any` at the call's column and heaps the box at the argument's
column. **So does gc, at the same two columns.** These lines are goc's verdict
moving from `heap` to `mixed` and landing on gc's, which is why `mixed × mixed`
goes from 8 to 30 while `heap × frame` falls from 183 to 172. Every one of the 20
has a goc `frame … struct_values__*` decision paired with a gc `frame slice …
argument` decision; the classifier calls it permissive because a line-level rule
cannot tell one decision on a line from the other one.

The pessimistic set grows by 15 and loses 9. Fourteen of the 15 are in this
branch's own files; the fifteenth is `variadic_backing.go:8`, which is section
3.2's refusal — the same site the census reports, seen from the other
instrument.

**The pessimism headline does not shrink, and that is a property of the
instrument rather than a disappointment.** `opt.conversionHelpers` records a
`convT` site as *heap*, because the payload did leave the frame, which is the
escape decision gc's `-m` reports at the same line — whether or not the value it
is handed makes the helper allocate. A `fmt.Sprintf("%d", n)` line has a
`convT64` on it either way, so the line stays in the pessimistic set while its
measured cost halves. The differential measures placement; `allocation_counts.go`
measures cost. This is the same disagreement the `iface-convt-fastpath` branch
reported for the same reason, and it is why both instruments are kept.

## 5. The guards

### 5.1 `TestFrameEscapeAudit` — clean

`ok github.com/evanphx/cg12/goc`. Zero new frame-address publications and none
of the listed ones went away, at every intermediate state of this branch as well
as the final one. The brief warned that a failure here would be a real
correctness finding rather than a baseline to update; it did not fail, and the
one place where it *would* have is written into the code:
`promotionsBlockedByALoop` escapes candidates after the analysis has finished,
so `containedAllocationsEscape` runs again after it. A container the loop rule
sends to the heap with a promoted payload still inside it is exactly the
publication this audit exists to catch.

### 5.2 The allocation census — regenerated and read site by site

`goc/testdata/alloc_census_baseline.txt`. Three groups, and the whole diff is
one class.

**128 sites moved heap → frame, and every one is a `struct_values__*`** — the
synthesized combined type a variadic call allocates. Nothing else in the corpus,
the stdlib or the runtime moved in that direction: not one `new(T)`, map, slice
backing or closure. **127 of the 128 pair with a new `runtime.convT*` site on
the same source line and in the same function**, which is the split made
visible: the array went into the frame and the payload it used to contain became
a conversion call a few columns to the right. The 128th is
`runtime_range_target_order.go:88:15`, whose payload site is positionless and so
cannot join on location; it is there, under `?`.

**22 sites vanished, all in `testdata/allocation_counts.go`** — the corpus
program this branch adds four cases to, so every line number below the insertion
moved. No site vanished anywhere else.

**174 sites appeared.** 127 of them are the split payloads paired above. 43 are
in the three files this branch adds or edits. **Four are neither, and they are
the price of section 3.2's refusal**, in the frame → heap direction:

    net/http.http2Transport.newClientConn   h2_bundle.go:8143:21  [2]http2Setting
    main.leaky (and two inlined copies)     variadic_backing.go:8:2  int

Both are a value passed as a variadic *element* to a callee that keeps nothing,
which the AST walk could prove before and now declines to answer. Two distinct
source lines across 385 corpus programs and the whole vendored stdlib is what
the refusal costs, against a `fatal error: found bad pointer in Go heap` it
buys. Recovering the precision needs a walk that can ask about dereference
depth, which `opt`'s fact table has and the AST walk does not.

### 5.3 Loop aliasing — the programs still match the host

`TestLoopBodyAllocationsAreDistinctPerIteration` and
`TestLoopAliasExpectationsMatchTheHostToolchain` pass, over the existing five
programs and the two this branch adds. `variadic_backing.go` still prints `1`
even though it now costs an allocation it did not, which is the point of having
both instruments: the census saw the cost and the program proves the answer.

`TestLoopVarPerIteration`, `TestAllocationCounts`,
`TestAllocationCountsAgainstTheHostToolchain` and the `opt` and `ir` unit suites
all pass. `go test ./goc/...` in full, the capability matrix and `make test-unit`
were deliberately not run: a dependent job does that.

`TestEscapeShadowPlacement` gains **one line**, and it is the same site 5.2's
last paragraph is about:

    net/http.http2Transport.newClientConn  slice-literal-backing  heap -> frame

The AST walk now places that `[2]http2Setting` on the heap and the IR analysis
would keep it in a frame. That is the "front end heap, IR frame" direction — a
latent pessimisation in the walk, deliberately introduced, with the object on
the heap either way, and the same class already has lines in the baseline from
`httpcommon.go` and `servemux121.go`. Nothing moved in the other direction.

### 5.4 Determinism

`TestDeterminism` passes. Two places in this branch could have broken it and are
written not to:

- `foldSplitPayloadsBackIn` visits containers **in first-appearance order**, not
  in map order, so which container is folded first cannot depend on hash
  iteration.
- The front end already recorded payload fields in argument order rather than in
  a map, for the same reason, and the split respects that ordering: a payload
  that becomes its own allocation still consumes its reserved field in the order
  the argument list gives.

### 5.6 The verification at HEAD

Every instrument re-run against the final commit, after the branch's temporary
diagnostic was deleted and every baseline regenerated:

```
$ go test ./goc -run 'TestFrameEscapeAudit|TestAllocationCensus|TestLoopBodyAllocations|
                      TestLoopAlias|TestAllocationCounts|TestDeterminism|TestLoopVar|
                      TestEscapeShadowPlacement'
ok  github.com/evanphx/cg12/goc  217.448s

$ go test ./opt ./ir ./lower
ok  github.com/evanphx/cg12/opt   ok github.com/evanphx/cg12/ir   ok github.com/evanphx/cg12/lower

$ go test ./goc -run TestSlogAllocationsAgainstGC   -slog-allocations
ok  github.com/evanphx/cg12/goc   17.818s

$ go test ./goc -run TestEscapeDifferentialAgainstGC -escape-gc-differential
ok  github.com/evanphx/cg12/goc   10.611s
```

Both opt-in baselines re-derive their own committed output without `-update`,
and the differential is byte-identical across two independent `-update` runs.

## 6. What was committed

| file | what |
|---|---|
| `ir/build.go` | `Block.HeapAllocConvertedField`, and the contract it adds to `HeapAllocConverted`'s two |
| `goc/compile.go` | `variadicPayloadStorage` and the slot it hands to the interface conversion; `parameterDoesNotEscape`'s refusal |
| `opt/escape.go` | declared containment, `containedAllocationsEscape`, the read-out cases, `foldSplitPayloadsBackIn`, the fold in the rewrite |
| `opt/escapesummary.go` | `markContents`: a `!Deep` argument escapes what it holds, not itself |
| `opt/escape_test.go` | five unit tests over the shape, including the retention case and the read-out |
| `goc/testdata/variadic_element_retention.go` | the retained element, surviving a stack copy |
| `goc/testdata/variadic_element_address_retention.go` | the front-end reduction: a frame address the collector rejects |
| `goc/testdata/allocation_counts.go`, `goc/alloccount_test.go` | four new rows and the headline's new number |
| `goc/loopalias_test.go` | the two new programs, expectations checked against the host |
| `goc/testdata/alloc_census_baseline.txt`, `escape_gc_differential.txt`, `slog_allocations_baseline.txt` | regenerated |

## 7. What is left

1. **`log/slog` is still 1.00 against gc's 0.00**, and the reason is
   `Logger.Info`'s `args` slice escaping at depth 0 through `Record.Add`'s
   loop-carried `argsToAttr` leak-to-result. That is a summary problem in
   `opt.escapeGraph`, not a representation one, and it is the next thing worth
   doing for slog. It would turn `disabled/3-attr` free.

2. **The front end's refusal costs precision on a spread call**, `f(xs...)`,
   where handing the parameter over as itself does make the parameter's own
   summary the right question. All five callers of `parameterDoesNotEscape` have
   the `*ast.CallExpr` in scope, so passing `call.Ellipsis.IsValid()` would
   recover it. Two census sites is what it is worth today.

3. **`fmt.Sprintf("%d/%d", small, small)` is 2.00 against gc's 1.00**, because
   the fold-back compares upper bounds and two convertible payloads out of a
   framed array might cost two. A profile-free improvement would need to know
   the values; a profile-guided one would not.

4. **The gc differential's pessimism headline cannot see this change.** A
   `convT` site is recorded as heap because the payload left the frame, so a
   `fmt.Sprintf("%d", n)` line stays pessimistic while its cost halves. Splitting
   the record into "left the frame" and "called an allocator" would let the file
   say which of the 573 lines cost anything, and 552 of them are conversion
   sites.

5. **The `slog.Attr` frame pointer map is still wrong.** Both reductions still
   reproduce at this HEAD and `json/logattrs-4-attrs` still dies. `json/kv-4-pairs`
   running again is a layout coincidence, not a fix, and it will stop being one.

## 8. The answer

**`fmt.Sprintf("value=%d", 42)` costs goc 1.00 allocations against gc's 1.00.**
It was 2.00. The `[1]any` is a frame slot, which is where gc has always kept it,
and the one allocation both compilers pay is the result string.

**`log/slog` is unchanged: `info/5-attr` 1.00 against gc's 0.00,
`disabled/3-attr` 1.00 against gc's 0.00.** Thirty-one of the thirty-two slog rows are byte-identical
to the base commit. That is a result rather than a non-result: the build that
split unconditionally measured `info/3-attr-large-ints` at 4.00 against gc's 3.00,
and the fold-back is what holds slog level while `fmt` improves. slog's array
escapes at depth 0 and no variadic representation can help it.

**The combined object was split, partially and by construction.** A payload the
runtime has a conversion helper for is its own allocation candidate; a payload
without one stays a field. `opt` folds a split payload back to `container+offset`
whenever the container escaped anyway or more than one payload escaped out of a
container that did not, so the representation is chosen where the escape answer
is known and never where it is guessed.

**The retention hole is not avoided, it is unrepresentable.** The thing the
callee keeps — the box — is a different allocation from the thing that stays in
the frame — the array — so "the callee retains an element" and "the callee
retains the slice" are two answers rather than one, and `ParamFact.Deep` is what
tells them apart. What the split owes back is that everything inside an escaping
object escapes with it, which `containedAllocationsEscape` settles after the mark
loop, after the loop rule, and after the fold-back — the loop rule being the one
that would otherwise leave a promoted payload inside a heap container.

**One thing found along the way is worth more than the numbers.** The front end's
`parameterDoesNotEscape` was answering about the *slice* for an argument that is
an *element* of it, so a local whose address a variadic callee retained was left
in a frame that returned. The collector says `found bad pointer in Go heap`. It
reproduces on the base commit and on `main`, the reduction is committed with its
expected output checked against the host toolchain, and the walk now refuses the
question the predicate beside it always refused. It costs two census sites.
### 5.5 Corpus output differential — mine, not one the brief asked for

All **392 corpus programs** compiled, linked and run by goc built from the base
commit and by goc built from this branch, `-O` both times, stdout and stderr
compared byte for byte. **388 identical.** The four that differ:

| program | difference |
|---|---|
| `allocation_counts.go` | the program this branch adds four cases to; the diff **is** the table in 4.1 |
| `variadic_element_address_retention.go` | **dies under the base compiler and runs under this branch** — section 3.2's fix, from the outside |
| `bytes_grow_compare.go` | not a difference: identical on a serial re-run. The parallel harness raced on the linked binary's name |
| `bytes_grow_stats.go` | prints raw `runtime.MemStats`. **Not deterministic under either compiler**: three runs of the *base* give `mallocs 220`, `226`, `220`, and three of this branch give the same three values |

So: zero unexplained output differences, and the one real one is the bug being
fixed. This is the instrument the `variadic-allocations` job's dangling pointer
did *not* show up in — 387 of 390 were identical then too — which is why it is
run alongside the census and the audit rather than instead of them.



`disabled/3-attr`: **goc 3.00 allocations/op, gc 0.00**. `info/5-attr`: **goc
9.00 allocations/op, gc 0.00**. Of the three designs `log/slog` bets on the
compiler for, the inline `[5]Attr` still spills in the right place and buys
nothing else, the packed `Value` buys nothing at all, and the disabled-level
early return saves nothing because everything it was meant to save has already
been paid at the call site. Essentially none of slog's designed allocation
avoidance survives compilation by goc — and on the one path that exercises a
real handler, the compiled program does not survive either.

---

# A `slog.Attr` in a frame is scanned as a pointer: the mis-classification, found and fixed

Job `ccwork/slog-attr-gcmask`, branched off `main` `4a6fd96`. The subject is the
miscompile RUNTIME_PLAN §26 left open and CCWORK_REPORT §5a reported without
fixing: a `slog.Attr` live in a frame across a collection dies with

    runtime: bad pointer in frame main_main at 0x...: 0xc8
    fatal error: invalid pointer found on stack

## 0. Reproduced on main before anything was changed

    go run ./cmd/goc -run goc/testdata/slog_allocations/miscompiles/attr_bad_pointer.go
    runtime: bad pointer in frame main_main at 0x31b432e07d50: 0xc8
    fatal error: invalid pointer found on stack
    runtime_adjustpointers <- runtime_adjustframe <- runtime_copystack
      <- runtime_shrinkstack <- runtime_scanstack <- markroot <- gcDrain

0xc8 is 200, the integer `slog.Int("k", 200)` packs into `Value.num`. Note the
walker in this trace: `shrinkstack` inside `scanstack`, so the collector's own
stack scan reached it through the copier.

## 1. The reduction landed as a corpus test, failing (commit below)

The programs in `goc/testdata/`, run by `goc/slogattrframe_test.go`
unoptimized and optimized, plus a run of each under
`GODEBUG=cg12checkstackcopy=1`, plus a check that every expectation is `go run`'s
own output rather than a belief about it. On `main`'s compiler, before any
change:

    --- FAIL: TestSlogAttrInFrameIsNotScannedAsAPointer (52.10s)
        --- FAIL: .../slog_attr_frame_gcmask.go              (7.98s)
        --- FAIL: .../slog_attr_frame_gcmask.go -O           (7.89s)
        --- FAIL: .../slog_attr_frame_gcmask_stackcopy.go    (7.49s)
        --- FAIL: .../slog_attr_frame_gcmask_stackcopy.go -O (7.99s)
        --- FAIL: .../slog_attr_frame_gcmask_kinds.go        (7.54s)
        --- FAIL: .../slog_attr_frame_gcmask_kinds.go -O     (7.92s)
        --- PASS: .../slog_attr_frame_gcmask_control.go      (2.55s)
        --- PASS: .../slog_attr_frame_gcmask_control.go -O   (2.74s)
    --- FAIL: TestSlogAttrInFrameSurvivesTheStackCopyChecker (24.95s)
        --- FAIL: .../slog_attr_frame_gcmask.go              (7.46s)
        --- FAIL: .../slog_attr_frame_gcmask_stackcopy.go    (7.50s)
        --- FAIL: .../slog_attr_frame_gcmask_kinds.go        (7.49s)
        --- PASS: .../slog_attr_frame_gcmask_control.go      (2.50s)
    --- PASS: TestSlogAttrFrameExpectationsMatchTheHostToolchain (0.33s)

Every failure is `run: runtime: bad pointer in frame main_main at 0x...: 0xc8`.
The `_kinds` program holds an Int64, a Bool, a Duration and a Float64 in one
frame at once, because `num` carries all of them and a map that claims that word
claims it for every one.

A fifth program, `slog_attr_frame_gcmask_shape.go`, was added later -- it is
section 2.1's finding, the same shape with no `log/slog` import -- and was
checked against main's compiler the same way, by building it with `goc/compile.go`
reverted to `4a6fd96`:

    runtime: bad pointer in frame main_main at 0x7c2da5013d50: 0xc8

## 2. Where the mis-classification is

Not in the type descriptor, and not in the stack-map machinery. It is one line
of the front end's translation of a Go type into the **Go-ABI aggregate**
(`ir.AggType`) that the frame's pointer map is built from.

`goc/compile.go`'s `goABIAggregate` turns an array type into one field carrying
the element's shape and the array's length:

    case *types.Array:
        field, ok := g.goABIField(value.Elem())
        ...
        field.Count = int(value.Len())

`ir.Field.Count` cannot express zero. `Count` is 0 for an ordinary scalar field
too -- every `ir.Field{Sub: ir.SubL}` literal in the tree leaves it unset -- so
`ir.Field.count()` reads 0 as **one element**:

    func (f Field) count() int {
        if f.Count > 1 { return f.Count }
        return 1
    }

A `[0]func()` therefore becomes one pointer-shaped element of eight bytes. Every
consumer of the aggregate then agrees on the wrong answer. Measured directly on
the shape (`ir` unit probe, since removed):

    [0]func()   size=8  align=8                    Go says 0/8
    slog.Value  size=32 pointer offsets [0 16 24]  Go says 24, [8 16]
    slog.Attr   size=48 pointer offsets [0 16 32 40]  Go says 40, [0 24 32]

Offset 16 of `slog.Attr` is `Value.num`. The phantom element sits at the offset
of the field that follows it and shifts every later field by a word.

Confirmed end-to-end in the compiled program, with a temporary dump of each
frame's marked words (`arm64/mc.go`'s `goLocalPointerWords`, instrumentation
since removed), compiling the reduction:

    framemap main.main: localwords=[... 22 ...]
      alloc t41 at frame+176 marks=[0 16 32 40]

`marks` are byte offsets within the attribute's frame slot and are exactly
`ir.AggregatePointerOffsets` of the phantom aggregate. Frame+176+16 is local
word 22, and that word holds 200.

The heap path is **not** affected: `walkPointerWords`, which builds the
`abi.Type` GCData mask, iterates `for index := 0; index < value.Len()` and so
emits nothing for a zero-length array, and it takes struct offsets from
`go/types`. The two descriptions of the same type disagreed, and only the frame
one was wrong.

## 2.1 The control was not a control

The reduction's control -- `slog.Value`'s shape hand-written in the program's
own package -- passes on main, and the earlier report read that as evidence that
the shape is not the trigger. It is not evidence. Its frame map is **identical**:

    framemap main.main (control): alloc t41 at frame+176 marks=[0 16 32 40]

and the word it claims holds 200 there too. It survives only because nothing in
that program copies `main`'s stack while the attribute is live: the mark phase
tolerates 200 (`findObject` on an address that was never heap returns silently),
and only `adjustpointers` throws on it. Replacing its `runtime.GC()` with the
same deep recursion the stack-copy reduction uses makes it fail identically:

    runtime: bad pointer in frame main_main at 0x42c9ee193d50: 0xc8

with no `log/slog` import anywhere in the program. So the trigger is the shape
after all -- a zero-length array field ahead of a scalar -- and log/slog is
where it appears in ordinary code.

## 3. The fix

`goc/compile.go`, `goABIAggregate`, one branch:

    case *types.Array:
        if value.Len() == 0 {
            // ... contributing no field is both the correct layout and the
            // only one Count can express.
            break
        }
        field, ok := g.goABIField(value.Elem())
        ...

A zero-length array contributes no field to the Go-ABI aggregate. The nested
aggregate it produces is empty, so it lays out as zero bytes at the alignment
`typeAlign` gives it, which is what Go says, and it contributes no part to
`FlattenAggregate` and no offset to `AggregatePointerOffsets`.

The alternative -- teaching `ir.Field` to distinguish "zero elements" from
"unset" -- would have to touch every `ir.Field` literal in the tree, since a
scalar field leaves `Count` at 0 and means one. The representation cannot say
zero, so the producer must not ask it to.

Also fixed, because the first change exposed it: gc gives a struct whose last
field is zero-sized one byte of padding so that a pointer to that field is not
past the end of the object, and `typeSize` implements that rule. The phantom
element used to supply that byte by accident. `goABIAggregate` now appends an
explicit byte-wide scalar instead -- never a pointer. That case was **already
wrong on main** for a trailing empty struct, which never had a phantom element:
`struct{ n uint64; _ struct{} }` laid out as 8 bytes where the type is 16.

## 3.1 The guard: the two descriptions of a type, checked against each other

`goc/goabi_layout_test.go`. A Go type is described to the collector twice --
`walkPointerWords` builds the heap mask from `go/types` offsets, and
`goABIAggregate` builds the aggregate the frame's map and the frame's slot size
come from -- by two pieces of code that share nothing. The test asserts they
agree on size, alignment and pointer words, over shapes with a zero-sized field
in every position and over every named type in `log/slog`.

On main it fails, and what it prints is the defect:

    Value:  aggregate 32 bytes, pointer offsets [0 16 24], type: 24, [8 16]
    Attr:   aggregate 48 bytes, pointer offsets [0 16 32 40], type: 40, [0 24 32]
    Record: aggregate 328 bytes, 23 pointer words, type: 288, 18 words
    Alone/Empty/Leading/Middle/Trailing/EmptyArrayOfStruct/TrailingEmptyStruct

With the fix, both tests pass.

## 4. The reduction after the fix, through both walkers

    --- PASS: TestSlogAttrInFrameIsNotScannedAsAPointer (63.72s)   10/10 subtests
    --- PASS: TestSlogAttrInFrameSurvivesTheStackCopyChecker (29.06s) 5/5
    --- PASS: TestSlogAttrFrameExpectationsMatchTheHostToolchain (0.57s) 5/5

That is every program unoptimized and optimized, every program again under
`GODEBUG=cg12checkstackcopy=1` (which validates each word the map claims as the
stack is copied rather than waiting for one to look like an address), and the
collector reached through `runtime.GC()` in three of them.

## 5. The slog baseline: the two JSON rows

Regenerated with

    go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations \
        -update-slog-allocations

and then re-run without the update flag, which passes, so the file reproduces.

**Attribution first, because the diff against the committed file is misleading.**
The committed baseline predates the `ccwork/variadic-allocations` merge that is
already on `main`, so a plain diff shows twelve rows improving that this work did
not touch. Regenerating on pristine `main` (`4a6fd96`, in a separate worktree)
produces those same improved numbers with the JSON rows still `crash`. Against
*that* -- main's compiler measured today -- the diff from this fix is exactly:

    -json/kv-4-pairs                     crash       2.00      crash       24.0
    -json/logattrs-4-attrs               crash       0.00      crash        0.0
    -
    -cases that did not run:
    -
    -  json/kv-4-pairs        goc  fatal error: invalid pointer found on stack
    -                              (bad pointer in frame log_slog_handleState_appendAttr: 0xc8)
    -  json/logattrs-4-attrs  goc  fatal error: invalid pointer found on stack
    -                              (bad pointer in frame main_func_311_28: 0x3)
    +json/kv-4-pairs                      8.00       2.00      344.0       24.0
    +json/logattrs-4-attrs                8.00       0.00      264.0        0.0

Nothing else moves. **The numbers that appear where `crash` was:**

| case | goc a/op | gc a/op | goc B/op | gc B/op |
| --- | ---: | ---: | ---: | ---: |
| `json/kv-4-pairs` | **8.00** | 2.00 | **344.0** | 24.0 |
| `json/logattrs-4-attrs` | **8.00** | 0.00 | **264.0** | 0.0 |

The section listing cases that did not run is now empty and gone: every case in
the table runs.

For scale, the earlier report recorded one whole-program observation of
`json/kv-4-pairs` at 15.00 allocations / 456.0 bytes before the process died on
the next case. The committed one-case-per-process harness now measures it at
8.00 / 344.0.


## 6. How wide the defect was

`log/slog` is not the only user of the shape. Every standard-library type that
puts a zero-length array field in a struct uses a **pointer-shaped** element, so
every one of them claimed a pointer word over whatever followed it. Measured on
`main` by the same guard:

| type | goc's aggregate | the type |
| --- | --- | --- |
| `log/slog.Value` | 32 bytes, pointers `[0 16 24]` | 24, `[8 16]` |
| `log/slog.Attr` | 48 bytes, pointers `[0 16 32 40]` | 40, `[0 24 32]` |
| `log/slog.Record` | 328 bytes, 23 pointer words | 288, 18 |
| `sync/atomic.Pointer[T]` | 16 bytes, pointers `[0 8]` | 8, `[0]` |
| `weak.Pointer[T]` | 16 bytes, pointers `[0 8]` | 8, `[0]` |
| `runtime.PanicNilError` | 8 bytes, pointers `[0]` | 0, none |

`slog.Value` is the one that killed programs, because the field after its
`[0]func()` is the `uint64` carrying a number small enough for
`runtime.adjustpointers` to reject. `atomic.Pointer` and `weak.Pointer` claimed
a word one *past* the value, which is a frame word belonging to something else:
whatever it holds is walked as an address, and it is only luck that no corpus
program has died of it.

With the fix, the invariant holds for every named type in all four packages,
`runtime` included whole.

## 7. Guards

### 7.1 The loop-aliasing programs still match the host toolchain

    --- PASS: TestLoopBodyAllocationsAreDistinctPerIteration (22.83s)  8/8
    --- PASS: TestLoopAliasExpectationsMatchTheHostToolchain (0.59s)   4/4

### 7.2 `TestFrameEscapeAudit` — clean

    --- PASS: TestFrameEscapeAudit (0.00s)

Zero new publications: no frame address the compiler emits reaches a global, a
heap object, a result area or anything through a parameter that it did not
already reach on `main`. Expected -- this change describes existing storage
differently, it does not move any object -- and checked rather than assumed.

### 7.3 The allocation census delta: empty

    --- PASS: TestAllocationCensus (187.10s)
    --- PASS: TestEscapeShadowPlacement (0.00s)

The census compares site by site in four directions -- heap→frame, frame→heap,
appeared, vanished -- and all four are empty, over a corpus that now has five
more programs in it. So there is no delta to review: no allocation moved, no
site appeared, none vanished.

That five new corpus programs add no census line is worth one sentence rather
than suspicion. The census records heap allocations and the frame allocations
that came out of an escape decision, keyed by site identity and deduplicated
across the corpus; the new programs' own `main`s allocate nothing, and every
line of `log/slog` and `runtime` they reach was already reached by
`stdlib_slog_structured.go`.

Regenerated anyway, literally, because the brief asks for the file and not for
an argument about it:

    go test ./goc -run TestAllocationCensus$ -update-alloc-census-baseline
    ok  github.com/evanphx/cg12/goc  275.989s

`git status` reports `goc/testdata/alloc_census_baseline.txt` unmodified. The
regenerated file is byte-identical to the committed one, so there is no site to
review and nothing unexplained.

Two GC-path smoke tests, since this change touches every aggregate the compiler
builds and `sync/atomic.Pointer` is in the runtime's own code:

    goc/testdata/runtime_gc_type_mask_padding.go  -> "type mask padding ok"
    goc/testdata/runtime_stack_copy_roots.go      -> "stack copy roots ok"
      (the second under GODEBUG=cg12checkstackcopy=1)

## 8. The same collision in the C front end, measured and deliberately not fixed

`cc/agg.go`'s `fieldOf` builds the same kind of field from a C array:
`ir.Field{Sub: subOfType(elem), Count: int(at.Len())}`. A GNU zero-length array
member hits the identical `Count == 0` collision. It does **not** produce a bad
map, and it is not silent:

    struct s { long n; char buf[0]; };
    long g(struct s v);

    cc: cannot pass struct.s by value: cg12 lays it out as 16 bytes (align 8)
    but C says 8 (align 8) -- a bitfield, which cg12's aggregate types cannot
    yet express

`checkAggLayout` compares the aggregate against C's own size and refuses. C
frames are not managed, so no stack map is built from these aggregates and there
is no word to mis-classify; the consequence is a rejected program with a
misleading diagnostic ("a bitfield").

Not fixed here, for three reasons that are a scope judgement and not a
measurement: it is loud rather than silent, C's rule for a trailing zero-sized
member is *not* Go's (C says 8 where gc says 16, so the same padding would be
wrong there), and `fieldOf` returns one field per member so skipping one means
changing its caller. It is one line plus its caller for whoever wants it, and
the error message it produces is the wrong one for the cause.

## 9. What was committed

| commit | what |
| --- | --- |
| `240d720` | the reduction as five corpus programs and `goc/slogattrframe_test.go`, failing on main |
| `c613b4c` | the fix in `goABIAggregate`, the trailing-zero-size padding, and the aggregate/type agreement test |
| `9a76dd3` | the regenerated slog baseline, RUNTIME_PLAN §28, the corrected miscompiles README |
| `9e0f9a3` | the agreement test extended to `sync/atomic`, `weak` and `runtime` |

The compiler change is 20 lines in one function plus a 15-line helper next to the
size rule it mirrors. Everything else is tests, baselines and prose.

### 7.4 Determinism

    scripts/determinism-check.sh -corpus -rounds 3 -j 24
    programs=395 rounds=3 workers=24 optimize=false
    round 0: 395 programs in 120.7s, 0 failed
    round 1: 395 programs in 121.7s, 0 failed
    round 2: 395 programs in 125.7s, 0 failed
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    reproducible=395 varying=0 failed=0 of 395 over 3 rounds

and again optimized, which is the arm where the placement passes run:

    scripts/determinism-check.sh -corpus -rounds 2 -j 24 -O
    programs=395 rounds=2 workers=24 optimize=true
    round 0: 395 programs in 126.0s, 0 failed
    round 1: 395 programs in 132.4s, 0 failed
    content varies between rounds: 0
    image varies, content identical (layout only): 0
    reproducible=395 varying=0 failed=0 of 395 over 2 rounds

Same depth as section 26's: 3 rounds without `-O`, 2 with.

## 10. The answer

**Where the mis-classification was.** Not in the type descriptor's pointer mask,
which was right, and not in how the stack map is built from what it is given.
It was one line earlier than either: `goABIAggregate` in `goc/compile.go`
translating a Go array type into an `ir.Field` with `Count = int(value.Len())`.
`ir.Field.Count` has no way to say zero — it is 0 for every ordinary scalar
field too, and `ir.Field.count()` reads that as one element — so a `[0]func()`
became one **pointer-shaped element of eight bytes**, sitting at the offset of
the field after it. In `slog.Value` that field is the `uint64` `log/slog` packs
int64, uint64, bool, `time.Duration` and float64 into, so the frame's pointer
map claimed the integer the attribute was carrying, and both the collector's
stack walk and the stack copier walked 200 as an address. It was never a
`log/slog` bug and never a JSON-handler bug: `sync/atomic.Pointer[T]`,
`weak.Pointer[T]` and `runtime.PanicNilError` had the same phantom word.

A zero-length array now contributes no field, which is the correct layout and
the only one `Count` can express, and the gc padding rule for a struct ending in
a zero-sized field — which the phantom used to supply by accident, and which was
already wrong on `main` for a trailing empty struct — is applied explicitly as a
byte-wide scalar.

**The slog JSON rows.** Both were `crash`. They now run:

    json/kv-4-pairs         8.00 a/op, 344.0 B/op   (gc 2.00, 24.0)
    json/logattrs-4-attrs   8.00 a/op, 264.0 B/op   (gc 0.00,  0.0)

and `goc/testdata/slog_allocations_baseline.txt` has no "cases that did not run"
section any more.

## 11. Verification at the committed HEAD

Everything committed, working tree clean, no update flags:

    go test ./goc -run 'TestSlogAttr|TestGoABIAggregate'
    --- PASS: TestGoABIAggregateAgreesWithTheTypeLayout (0.00s)
    --- PASS: TestGoABIAggregatesAgreeWithTheirTypesInTheStdlib (2.07s)
    --- PASS: TestSlogAttrInFrameIsNotScannedAsAPointer (58.14s)
    --- PASS: TestSlogAttrInFrameSurvivesTheStackCopyChecker (27.78s)
    --- PASS: TestSlogAttrFrameExpectationsMatchTheHostToolchain (0.39s)
    ok  github.com/evanphx/cg12/goc  88.441s

and the two reductions this started from, run straight from the miscompiles
directory under the fixed compiler:

    goc -run .../attr_bad_pointer.go            -> 200
    goc -run .../attr_bad_pointer_stackcopy.go  -> 200
    goc -run .../attr_bad_pointer_control.go    -> 200

`go test ./goc/...`, the capability matrix and `make test-unit` were not run
here: a dependent job runs them, and running them twice on a loaded box helps
nobody. What that leaves unchecked from this tree is stated plainly rather than
implied — the corpus-wide run, the matrix, and the unit suites are that job's
result, not this one's.
