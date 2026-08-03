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

Regenerated `goc/testdata/escape_gc_differential.txt`.

|  | base | this branch |
|---|---|---|
| **PESSIMISTIC** (goc heaps, gc does not) | 567 lines | **571** |
| PERMISSIVE (gc heaps, goc does not) | 209 lines | 220 |
| confusion matrix, goc heap × gc frame | 183 | 177 |
| joined source lines | 3 059 | 3 080 |

**Every line that entered or left either set is in a file this branch adds or
edits**: 4 pessimistic and 9 permissive in `allocation_counts.go`, 2 in
`variadic_element_retention.go`, 1 in `variadic_element_address_retention.go`,
1 permissive line left `allocation_counts.go`. Nothing in the stdlib, the
runtime, or any untouched corpus program moved in either direction.

**The pessimism set does not shrink, and that is a property of the instrument
rather than a disappointment.** `opt.conversionHelpers` records a `convT` site as
*heap* — because the payload did leave the frame, which is the escape decision
gc's `-m` reports at the same line — whether or not the value it is handed makes
the helper allocate. A `fmt.Sprintf("%d", n)` line has a `convT64` on it either
way, so the line-level verdict is unchanged while the measured cost halves. The
differential measures placement; `allocation_counts.go` measures cost. This is
the same disagreement the `iface-convt-fastpath` branch reported for the same
reason, and it is why both instruments are kept.

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

4. **The `slog.Attr` frame pointer map is still wrong.** Both reductions still
   reproduce at this HEAD and `json/logattrs-4-attrs` still dies. `json/kv-4-pairs`
   running again is a layout coincidence, not a fix, and it will stop being one.

## 8. The answer

**`fmt.Sprintf("value=%d", 42)` costs goc 1.00 allocations against gc's 1.00.**
It was 2.00. The `[1]any` is a frame slot, which is where gc has always kept it,
and the one allocation both compilers pay is the result string.

**`log/slog` is unchanged: `info/5-attr` 1.00 against gc's 0.00, `disabled/3-attr`
1.00 against gc's 0.00.** Thirty-one of the thirty-two slog rows are byte-identical
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

