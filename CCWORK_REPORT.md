# Asking the escape question for a variadic call: splitting the object that made it unanswerable

Branch `ccwork/variadic-escape-question`, off `ccwork/iface-convt-fastpath`
(`19488ee`). The previous jobs' reports are at `19488ee:CCWORK_REPORT.md` and
`4a6fd96:CCWORK_REPORT.md`.

Status: IN PROGRESS — numbers below are measured unless marked otherwise.

**Headline (provisional): `fmt.Sprintf("value=%d", 42)` costs goc 1.00
allocations against gc's 1.00 — exact parity, from 2.00. The `[N]any` backing
array of a variadic call is now a frame slot wherever the callee does not retain
the slice itself, and the boxed payload an element points at is decided
separately from it. The combined object was split, partially and deliberately;
section 2 prices both directions. The retention hole that forced the previous
attempt back to 2.00 is closed by construction rather than by an extra rule: the
callee that keeps `args[0]` now keeps a payload that is its own allocation, and
that payload goes to the heap while the array does not.**

## 1. What was actually wrong, confirmed before anything was changed

Two instruments, both on the base commit.

`goc/compile.go:6581` decides between a frame `[N]any` and a heap one:

    stackAllocateVariadic := !g.runtimeAllocation || g.fn.NoSplit || g.forceStackVariadic

and `forceStackVariadic` comes from a two-symbol allowlist. So the front end
never asks. That is true and it is not the whole story: the heap arm emits the
*neutral* `ir.OHeapAlloc` candidate, and `opt.LowerHeapAllocations` — which runs
unconditionally, `goc/compile.go:488`, not only under `-O` — does ask. The
question is asked; the representation is what made it unanswerable.

`goc/compile.go:6591-6613` builds one synthesized `struct{values [N]any;
payload0 T0; ...}` per call site and allocates the backing array and every boxed
payload as a single object. One object is one placement.

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

Regenerated `goc/testdata/slog_allocations_baseline.txt`. **Every allocation
count is identical to the base commit.**

| case | goc | gc |
|---|---|---|
| `info/5-attr` | 1.00 / 240 B | 0.00 |
| `disabled/3-attr` | 1.00 / 144 B | 0.00 |
| `info/3-attr-large-ints` | 1.00 / 176 B | 3.00 |
| `control/variadic-6-preboxed` | 0.00 | 0.00 |
| `control/variadic-6-literal` | 0.00 | 0.00 |

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
and now runs**, at 3.00 allocations / 272 B against gc's 2.00 / 24 B. That is
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

