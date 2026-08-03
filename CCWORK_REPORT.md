# The variadic-call allocation gap: what the three allocations were, and closing them

Branch `ccwork/variadic-allocations`, off `main` (`e7c8e33`). The gc differential
job's report is at `origin/ccwork/escape-gc-differential:CCWORK_REPORT.md`.

Status: COMPLETE. Every number below was watched to completion.

**Headline: `fmt.Sprintf("value=%d", 42)` cost goc 3.00 allocations per call and
now costs 2.00, against gc's 1.00 — and gc's 1.00 is 1.74 with a value it cannot
box into `runtime.staticuint64s`. Seven of the thirteen measured calls reach
exact parity with go1.26.1 that did not before, including every variadic call
whose callee does not retain what it was handed. On a realistic formatting
workload that is 12% of wall clock.**

## 0. Where the harness actually is

The brief says the gc differential harness is "on main". It is not:
`internal/gcdiff`, `goc/gcdiff_test.go` and
`goc/testdata/escape_gc_differential.txt` live on
`origin/ccwork/escape-gc-differential`, which was never merged. `main` at
`e7c8e33` has none of them. This branch starts from `main` as instructed and
brings the harness across so the differential can be re-run and re-compared.

## 1. The gap, reproduced before anything was changed

Host `go1.26.1 linux/arm64`, 64-core aarch64. goc built from `e7c8e33`.
`runtime.MemStats.Mallocs` delta over 1 000 calls after a 1 000-call warm-up,
reported as allocations per call.

| probe | host go1.26.1 | goc @ e7c8e33 |
|---|---|---|
| `fmt.Sprintf("value=%d", 42)` | **1.00** | **3.00** |

Reproduced exactly as the brief states.

## 2. The decomposition — what the three allocations ARE

The brief asked for measurement, not hypotheses. The method: a battery of
reductions, each an inline loop with no closure and no helper allocation, each
measured on both compilers, so every allocation is attributed to a construct
rather than to a belief about one. Control probes (`nothing`, `control`) confirm
the harness itself is allocation-free on both.

| probe | host | goc | what it isolates |
|---|---|---|---|
| `f(args ...int)` with 1 arg | 0 | **1** | the `...` backing array alone |
| same with 0 args | 0 | 0 | nil slice, no allocation either side |
| `f(args ...any)` with an `int` | 0 | **1** | array **and** the `int`→`any` box, already one object |
| `f(args ...any)` with a `*int` | 0 | **1** | array alone; a pointer needs no box |
| `f(x any)` with an `int` | **0** | **1** | interface boxing of a small int, no variadic anywhere |
| `f(x any)` with a `*int` | 0 | 0 | direct-interface pointers are free in goc too |
| `var x any = someInt` | **0** | **1** | the same box, no call at all |
| `sync.Pool` Get/Put round trip, warmed | **0** | **1** | — see below |
| `func(v int) any { return v }` | 0 | **2** | the box, plus one more for *returning* an interface |
| `func(v *int) any { return v }` | 0 | **1** | no box needed, so this **1** is the return overhead alone |
| `string(buf)` | 1 | 1 | agree |
| `[2]int` returned by value | 0 | 0 | agree — it is not "aggregate returns" |
| `fmt.Sprintf("plainstring")` — **no variadic args at all** | **1** | **2** | the +1 is inside `fmt`, not in the `...` |

The three allocations in `fmt.Sprintf("value=%d", 42)` were:

1. **The variadic backing object.** goc allocates one heap object of type
   `struct{values [1]any; payload0 int}` — the front end already merges the
   `[]any` backing array and the boxed `int` payload into a single
   `runtime.newobject`, so the brief's hypotheses (a) and (b) are **not two
   allocations here, they are one**. gc pays **zero**: its `...` array is a frame
   slot and `42` boxes to `runtime.staticuint64s`.
2. **An interface returned from a function.** `fmt.Sprintf` calls
   `newPrinter()`, which is `sync.Pool.Get() any`. goc represents an interface
   value as the address of a two-word descriptor, so a function returning one
   handed back an address that had to outlive its frame — a fresh 16-byte
   `runtime.newobject` at **every non-nil interface return in the program**. The
   `sprintf_no_args` row is what isolates it: a `Sprintf` with no variadic
   arguments at all still cost goc 2 against gc's 1. The pool was pooling
   correctly the whole time; a `Put` then `Get` returns the same object under
   goc, verified by identity.
3. **The result string**, `string(p.buf)`. Both compilers pay this. It is the
   whole of gc's 1.00.

So the brief's framing needs one correction, and it is the most useful thing the
measurement produced: **the variadic backing array is only one of the three, and
it is already merged with the interface box.** Naming this class "the variadic
call" undercounts what is really two independent defects that happen to meet in
`fmt.Sprintf`.

One further gap the decomposition turned up, which is **not** in `Sprintf`'s
three and is broader than variadic calls:

- **goc has no `convT64`/`staticuint64s` family at all.** `grep` finds
  `convT64`, `convTstring`, `convTslice` and `staticuint64s` only in
  `stdlib/src/runtime/iface.go` — the Go source goc compiles — and nowhere in
  the compiler (`parse/`, `ir/`, `opt/`, `lower/`, `goc/compile.go`). goc emits
  `runtime.newobject` for every non-pointer interface conversion. The brief's
  hypothesis (b) is confirmed as real, and section 4 shows it is now the whole
  of the remaining `Sprintf` gap: the box there is merged into the variadic
  object, which has to be on the heap because `fmt` retains an element of it,
  and gc pays nothing for the same box only because 42 fits in
  `runtime.staticuint64s`. It is left undone, with the measurement; sections 4
  and 7 say why and what the right shape is.

## 3. The fixes

Six changes. Each was measured before and after, and each is committed
separately with its own evidence. 3.6 exists because 3.5 opened a hole; the
sequence is left as it happened rather than tidied into one commit, because the
hole is the most instructive thing in this report.

### 3.1 `goc`: an interface result does not need a heap object (commit `6f66dad`)

`stableReturnValue` heap-allocated a 16-byte descriptor at every non-nil
interface return. It does not have to: an interface result already has a
two-pointer `ir.AggType`, so `lowerGoAggregateReturn` loads both words out of
that pointer into the result registers **inside the returning block, before the
epilogue tears the frame down**. The caller never sees the pointer. That is the
same claim the inline-aggregate arm of the same function has always rested on,
and `opt.FrameEscapes` agrees rather than merely tolerating it — its
return-publication rule is skipped outright when `RetAgg != nil`, for exactly
this reason.

| probe | before | after | gc |
|---|---|---|---|
| `func(v *int) any { return v }` | 1.00 | **0.00** | 0.00 |
| `func(v int) any { return v }` | 2.00 | **1.00** | 0.00 |
| `sync.Pool` Get/Put round trip | 1.00 | **0.00** | 0.00 |
| `fmt.Sprintf("plainstring")` | 2.00 | **1.00** | 1.00 |
| `fmt.Sprintf("value=%d", 42)` | 3.00 | **2.00** | 1.00 |

### 3.2 `opt`: a summary is readable through scalarised aggregate arguments

`summarisedCallee` refused to read a callee's escape summary for any call with
`ArgGroups` — that is, any call passing a slice or an interface, because those
are scalarised into consecutive argument entries. Every such call was answered
"assume the worst".

It does not have to be. The fact table is indexed by `ir.Func.Params` position,
which is the *flattened* list — a slice parameter is three entries there, an
interface two — so when the call scalarises its arguments the same way the
callee scalarised its parameters, position still names the same value on both
sides. `scalarisedArgumentsAlign` checks exactly that: equal `(Index, Count)`
runs in the same order over lists already known to be the same length. A call
with no groups is left alone, so this can only widen what the summaries answer,
never narrow it.

Effect on its own: the reason a `...int` backing array escaped changed from
"call to `$main.varInt` with scalarised aggregate arguments" to a different
reason — a necessary step, not sufficient by itself.

### 3.3 `opt`: a value that is not a pointer publishes nothing

The escape graph's regions are field-insensitive on purpose (a closure
environment is one `alloc8 32` and escaping one word of it would miss the other
captures). A slice parameter is reconstructed into one frame slot, so **the load
of its length and the load of its data pointer come out of the same node**. Any
use of the length therefore published the pointer:

- `sinkI = len(args)` made every parameter of the function escape.
- `return len(args)` made every parameter leak-to-result, and then the caller
  escaped the argument at the call.
- `a[1:]` computes `len(a)-1` and `cap(a)-1`, and those two subtractions were
  enough to escape `a` — which is what `fmt.pp.doPrintf` does at the end of
  every call, in the `%!(EXTRA` check. That is why **`fmt.Sprintf`'s `a []any`
  escaped**, and with it every variadic call to `fmt` in the tree.

`flow` now drops a depth-0 edge whose source cannot be a pointer.

**This is where I broke something and had to narrow it.** The first version
tested `ClassOf(src) != ir.ClsP` alone. That is wrong in goc's IR: the front end
coerces a pointer into a width class when it feeds one to a width-typed
intrinsic, and `sync/atomic.StorePointer` really does end

```
%t7 =p loadl %t2
%t8 =l copy %t7
intrinsic atomic.store.l %t6, %t8
```

Reading that `copy` as a scalar loses the store, so `log.Logger.SetPrefix`'s
`l.prefix.Store(&prefix)` no longer escaped `prefix`, the string was left in a
dead frame, and `stdlib_log_buffer.go` started printing a log line with no
prefix. The 389-program corpus output differential (section 5.1) caught it.
`scalarValue` now walks back through copies and casts and answers "may be a
pointer" if any link in that chain is pointer-class; a parameter or phi, whose
definition is not an instruction, is also answered conservatively.

### 3.4 `goc`: an interface copy does not branch on an address this frame allocated

`materializeNilInterface` gives an interface value a two-word descriptor to copy
out of, branching because a nil interface is carried as a nil pointer rather
than as the address of a zeroed descriptor. It branched on frame allocations
too, which are never nil. The phi merging the two arms is a use of the
descriptor slot that `opt`'s alias analysis cannot see through, so the slot was
classified `cEscaped` and every candidate pointer stored into it was treated as
published — which is what kept a **`...any`** call's backing object on the heap:
the boxed payload lives inside that object, its address goes into the
descriptor, and the descriptor looked like it escaped.

### 3.5 `opt`: an object pointing at part of itself is not a publication

A store whose value and whose destination are both derived from the same tracked
allocation says nothing about whether that allocation escapes. The front end
packs a `...any` call's `[]any` array and the payloads it points at into one
object, so writing each element's data word writes a pointer into that object,
into that same object.

Read 3.6 with it: this is true and it has a consequence that has to be paid for.

### 3.6 `opt`: a self-referential object needs the *deep* summary

3.5 opened a hole and this closes it. 3.5 is right that an object pointing at
part of itself has not escaped; what it does not say, and what turns out to
matter, is that such an object's contents are no longer a separate allocation
with an escape decision of their own.

A computed `ParamNoEscape` is a claim about dereference depth 0 — the pointer
handed over is not kept — and says nothing about depth 1. For an ordinary
allocation that split is right, because the pointee is a different object,
decided separately. For the merged variadic object it is not: a callee that
retains an *element* retains the whole thing.

```go
var sink any
//go:noinline
func keepElement(args ...any) { sink = args[0] }
//go:noinline
func leakElement(x int)       { keepElement(x) }
```

`keepElement`'s summary is `noescape` at depth 0 and true, so the merged object
was promoted into `leakElement`'s frame — and `sink` then held an interface
whose data word pointed into a frame that had returned. **The program still
printed the right number**, which is what makes this the bad kind of bug: no
test in the tree failed, the frame-escape audit was clean, and 387 of 390 corpus
programs were byte-identical. I found it by reading the gc differential's
permissive column, which is what that column is for.

`ParamFact` therefore grows a `Deep` flag — nothing reachable through the
parameter is retained either — computed from the same `heapLeak` and
`resultLeak` distances one dereference further out. `//go:noescape` sets it,
since a directive is that promise by construction. `markSummarisedCall` requires
it for an argument naming a self-referential allocation.

The cost of getting this right is the headline number: `fmt.Sprintf("value=%d",
n)` goes from 1.00 back to **2.00**. `fmt.pp.doPrintf` assigns each element to
`p.arg`, a field of a heap-allocated printer, so the box genuinely has to be on
the heap. It is cleared again by `p.free()`, but a may-analysis cannot see that
and should not.

## 4. Where the numbers are now

`goc/testdata/allocation_counts.go`, run under both compilers. Allocations per
call. Deterministic over three runs each, and identical with and without `-O`.

| call | gc | goc before | goc after |
|---|---|---|---|
| `fmt.Sprintf("value=%d", n)` | 1.00 | 3.00 | **2.00** |
| `fmt.Sprintf("value=%s", s)` | 2.00 | 3.00 | **2.00** — parity |
| `fmt.Sprintf("value=%v", struct)` | 2.00 | 3.00 | **2.00** — parity |
| `fmt.Sprintf("a constant format")` | 1.00 | 2.00 | **1.00** — parity |
| `f(...int)` with 2 args | 0.00 | 1.00 | **0.00** — parity |
| `f(...any)` with an int and a string | 0.00 | 1.00 | **0.00** — parity |
| `f(x any)` with a small int | 0.00 | 1.00 | 1.00 |
| `f(x any)` with a pointer | 0.00 | 0.00 | 0.00 — parity |
| `func(int) any` | 0.00 | 2.00 | 1.00 |
| `func(*int) any` | 0.00 | 1.00 | **0.00** — parity |
| `sync.Pool` Get/Put | 0.00 | 1.00 | **0.00** — parity |
| `fmt.Sprintf` **inside a loop body** | 1.00 | 3.00 | 2.00 |
| `f(...int)` **inside a loop body** | 0.00 | 1.00 | 1.00 |

Seven rows reach parity with go1.26.1 that did not before. The `...` backing
array — the thing the brief named — is free in every case where the callee does
not retain it, which is what gc's answer is too.

**The one row that does not reach parity is `sprintf_int`, and the reason is
exactly the brief's hypothesis (b).** goc pays 2.00 where gc pays 1.00, and the
whole of that difference is the boxed `42`: gc returns a pointer into
`runtime.staticuint64s` and allocates nothing, goc calls `runtime.newobject`.
With a value gc cannot put in the static table the two are level — gc measured
**1.74** for `fmt.Sprintf("value=%d", counter)` in the earlier job, against
goc's 2.00.

**Why (b) is not implemented here, having been measured.** The naive form —
route integer payloads through `runtime.convT64` in the front end — would make
`sprintf_int` 1.00 and would *lose* two of the parity rows above. gc does not
use `staticuint64s` unconditionally either: when a box does not escape, gc
stack-allocates it, which is what `f(...any)` with an int and a string at 0.00
already is on both compilers. The correct shape is to use the static table only
where the box has been decided onto the heap, and that decision is made in
`opt`, after the front end has emitted the allocation. That is a real change
with its own risk, and doing it at the end of a session that has already moved
five things in the escape analysis would be the wrong call. It is the obvious
next job and section 7 says what it is worth.

## 5. Two rows that are the honest limit of this work

`opt.promotionsBlockedByALoop` refuses to promote any allocation written inside
a loop body, because a frame slot is one object and each iteration may need its
own. That is the rule the loop-aliasing job landed and guard 4 says not to
regress it. It is blunt: it does not ask whether anything retains the object
past the iteration, and for a variadic backing array nothing does. A
`fmt.Sprintf` written directly in a loop body therefore keeps an allocation a
`fmt.Sprintf` in a helper does not.

Lowering that is a real optimisation someone could do, and it is deliberately
left alone here rather than attempted alongside six other changes. The two
`_in_loop` rows in the regression test exist so the price is a number somebody
can see rather than a surprise.

## 6. The guards

### 6.1 Corpus output differential (mine, not one the brief asked for)

390 corpus programs compiled, linked and run by goc built from `main` and by goc
built from this branch, outputs compared byte for byte. **386 identical.** The
four that differ:

| program | difference |
|---|---|
| `allocation_counts.go` | the new program; the diff **is** the before/after table in section 4 |
| `bytes_grow_stats.go` | prints `runtime.MemStats`: 162 mallocs → 143 |
| `gomaxprocs_memstats.go` | prints `Mallocs`: 100 → 99 |
| `runtime_panic_print_string.go` | two PC offsets in a stack trace; the code got smaller |

Both `MemStats` programs are `runtimeCapabilityMustPass` in the capability
matrix — they are checked on exit status, not on the number.

This differential is what caught the `log.Logger.SetPrefix` miscompile in
section 3.3, and it is why I am willing to state that the escape-analysis
changes are sound rather than only that the audits are clean.

### 6.2 `TestFrameEscapeAudit` — **zero new publications** (guard 1)

    $ go test ./goc -run TestFrameEscapeAudit -count=1 -timeout 90m
    ok  github.com/evanphx/cg12/goc  279.069s

The audit fails on any publication not in `frame_escape_baseline.txt` **and** on
any listed publication that has gone away. It passed without `-update`, so the
209-line baseline reproduces exactly: nothing this branch newly publishes, and
nothing that stopped being published.

`variadic_backing.go` is the program guard 1 names, and it did move underneath:
`retainNothing(&x)`'s backing array was three heap rows in the census and is now
a frame slot, so the frame address `&x` goes into storage in the same frame
instead of into a heap object. The baseline has no line for it in either
direction — it had none before, because the heap object it went into was already
proved not to outlive the frame, and it has none now.

### 6.3 The census delta, site by site (guard 2)

`alloc_census_baseline.txt`: 18 701 rows → 14 700.

| direction | sites | what they are |
|---|---|---|
| **vanished** | 4 124 | every one an **interface-typed, positionless heap object** |
| **heap → frame** | 339 | variadic backing objects, and slice/string backings behind them |
| **appeared** | 126 | 18 from the new corpus program; the rest re-attributed, below |
| **frame → heap** | 1 | a positionless-join artefact; below |

**The 4 124.** All `?`-positioned, all `heap`, and every distinct type among them
is an interface: 2 783 `error`, then `reflect.Type`, `any`, `hash.Hash`,
`text/template/parse.Node`, `net.Addr`, `net.Conn`, `image.Image`,
`context.Context`, `log/slog.Handler`, and 92 more. Three read as concrete and
are not — `strings.replacer`, `archive/zip.fileInfoDirEntry` and the corpus's
own `reflectTypeAssertStringer` are interface declarations. This is the object
section 3.1 removed. It is positionless because it was emitted into the
synthetic block `stableReturnValue` created, which carries no source position.

The census's review note says a vanished heap site is either gone code or an
object that has become an ordinary front-end frame slot, and that the latter is
the correctness-critical direction the census cannot see. It is the latter,
deliberately, and section 3.1 is the argument.

The follow-up question is whether the **payload** inside such a returned
interface also moved into a frame — that would be a real dangling pointer, since
the payload's address is one of the two words the caller receives. It did not.
Checked three ways: directly on `func fail(code int) error { return myErr{...} }`,
where the payload is `runtime.newobject` and only the descriptor is `alloc8 16`;
by construction, with a program that keeps five such errors, grows the stack by
400 frames of `[512]int` twice with a collection between, then type-asserts each
back and checks its fields (both compilers print all five intact); and across
the corpus, where every non-interface type among the vanished sites reappears in
the same function, same type, still on the **heap**, now with a source position
instead of `?`.

**The other appeared sites are inlining.** `context.Background` used to be one
row; it is now also a row under `crypto/tls.Conn.Handshake`,
`log/slog.Logger.Info`, `net.Dialer.Dial`, `net/http.NewRequest` and a dozen more
callers. A one-line interface-returning function that no longer allocates is
small enough to inline.

**The one frame → heap.** The site is positionless — `? main.main
runtime.newobject any` — so it stands for every such allocation in every
`main.main` at once. Compiling `runtime_loopvar_value_shapes.go` under both
compilers shows the same allocation at `pos={0 0 0}` **frame** before and
`pos={1 65 40}` **frame** after: it gained a position, not a heap placement, and
the positioned row is in the appeared set.

`escape_shadow_baseline.txt` also moved: 168 lines out, 85 in. The lines that
went are the "with scalarised aggregate arguments" disagreements section 3.2
resolved. Nothing there changes emitted code.

### 6.4 The gc differential (guard 3), and why its number understates this

Re-run against host `go1.26.1`, with the harness brought across from
`ccwork/escape-gc-differential`:

```
  goc\gc      frame     heap    mixed   absent    total
  frame          35        3        2       30       70
  heap          179     1768      192      174     2313
  mixed           3       54        8        7       72
  absent        457      119       23        0      599
  total         674     1944      225      211     3054
```

| | before | after |
|---|---|---|
| **PESSIMISTIC** (goc heaps, gc does not) | **585** | **563** |
| PERMISSIVE (gc heaps, goc does not) | 202 | 209 |
| census rows with **no source position** | **5 863** | **1 738** |

**The pessimism set shrank by 22 lines** — 30 removed, 8 added, and 7 of the 8
are the new corpus program's own lines. Net of the new program it is 29 lines.

That is a much smaller number than the work behind it, and the differential's
own coverage table says why. **4 125 census rows stopped being positionless.**
Those rows never join — the differential's key is (file, source line) and they
have no line — so the largest single effect of this branch, removing a heap
allocation from every non-nil interface return in the whole compiled program
including 2 783 `error` returns, **cannot appear in either direction of this
instrument at all**. The 22 lines are what the join can see of a change most of
which it structurally cannot. The allocation-count test in section 4 is the
instrument that sees it, which is a large part of why it now exists.

All eight new permissive lines were read, since that is the
correctness-critical direction and it is where I found the section 3.6 hole:

- Four are `reflect.Append(slice, reflect.ValueOf(17))` sites where the two
  compilers **agree** — both frame the `...` array, both heap the boxed `17` —
  and the line folds to "mixed vs mixed".
- Two are closure lines in `runtime_loopvar_value_shapes.go` where goc heaps the
  closure exactly as gc does, and frames a `[1]any` that gc also frames.
- One is the new program's own inlined function literal, decided differently in
  two functions on one source line.
- One (`allocation_counts.go:124`) is the same shape.

None is a frame address outliving its frame.

### 6.5 `TestLoopBodyAllocationsAreDistinctPerIteration` (guard 4) — clean

    --- PASS: loop_alias_forms.go        (and -O)
    --- PASS: loop_alias_composite.go    (and -O)
    --- PASS: variadic_backing.go        (and -O)
    --- PASS: loop_alias_frame_local.go  (and -O)
    --- PASS: TestLoopAliasExpectationsMatchTheHostToolchain

All four forms still print what the host toolchain prints, and the frame-local
counter-example still prints `framed: 18 / within: 12 / literal: 18`.

### 6.6 Wall clock — the allocation win is measurable, and small

The proxy is allocations; the point is time. A realistic formatting workload:
100 000 `fmt.Sprintf("id=%d name=%s score=%d", int, string, int)` calls through a
`strings.Builder`, timed inside the program with `time.Since` so process startup
is not in the number. `GOMAXPROCS=1`, four runs each on an otherwise idle box.

| | ns per Sprintf | allocations per Sprintf | GC cycles |
|---|---|---|---|
| goc @ `main` | **3 445** (333–349) | 3.03 | 7 |
| goc @ this branch | **3 040** (301–310) | 2.03 | 6 |
| host go1.26.1 | 348 | 3.34 | 4–5 |

**12% faster**, 348 ms → 302 ms for the same work, with non-overlapping ranges
across runs. One of the three allocations is gone and one GC cycle with it.

Two things worth saying plainly:

- goc now allocates **less than gc** on this workload — 2.03 against 3.34,
  because goc packs all three boxed arguments and the `...` array into one
  object where gc boxes each argument separately — and is still **9× slower**.
  Whatever is left between goc and gc on this path is not allocation. This
  branch closes part of the allocation gap it set out to close and should not be
  read as closing the performance gap.
- 12% is the honest figure for a helper-function call site. It would have been
  25% under the version of this branch that was unsound; that number is in the
  git history and it was wrong.

## 7. What is left, ranked

1. **`convT64`/`staticuint64s` — brief hypothesis (b), confirmed and not done.**
   It is the whole of the remaining `sprintf_int` gap and it is not limited to
   `Sprintf`: every interface conversion of a small non-pointer value that has
   to be on the heap pays a `runtime.newobject` where gc pays nothing. Section 4
   says why the naive form would cost two of the parity rows and what the right
   shape is.
2. **The loop rule.** `opt.promotionsBlockedByALoop` blocks every promotion in a
   loop body without asking whether anything retains the object past the
   iteration. A `fmt.Sprintf` written directly in a loop keeps an allocation one
   in a helper does not. Section 5.
3. **Interface results beyond the first.** Section 3.1 fixed result 0. An extra
   result — the `error` in `(T, error)` — still goes back through a
   caller-provided pointer that has to name storage outliving the callee, so it
   still heap-allocates its descriptor. The same argument applies; the ABI
   change is bigger.
4. The `go`/`defer` closure class, 138 lines across 89 programs, explicitly out
   of scope for this job and untouched by it.

## 8. What was committed

| commit | what |
|---|---|
| `6f66dad` | `goc`: an interface result does not need a heap object to be returned in |
| `5e4ceeb` | `opt`: read a callee's summary through scalarised aggregate arguments |
| `dad9626` | `opt`: reading a slice's length is not a publication of its pointer |
| `c713426` | `opt`: an object pointing at part of itself has not escaped |
| `53b2c77` | `goc`: do not branch on the nil-ness of an address this frame allocated |
| `fb7fb90` | `test`: the allocation-count regression test and its corpus program |
| `386bedf` | `opt`: a self-referential object needs the deep summary, not the shallow one |
| `348e87f` | the gcdiff harness, and all three regenerated baselines |

The regression test the brief asked for is `goc/alloccount_test.go` plus
`goc/testdata/allocation_counts.go`. It holds thirteen calls to a measured
number under goc, unoptimized and optimized, and fails on exact inequality in
**both** directions — more allocations is the regression, fewer is a placement
change that has to be understood before it is written down.
`TestAllocationCountsAgainstTheHostToolchain` holds the gc column to `go run`,
so the gap is a measurement and not a belief about one.

## 9. Verification at the committed HEAD

Every baseline in the tree reproduces at `4ae184c` without `-update`:

```
$ go test ./goc -run TestFrameEscapeAudit -count=1 -timeout 90m
ok  github.com/evanphx/cg12/goc  279.069s

$ go test ./goc -run 'TestAllocationCensus$|TestEscapeShadowPlacement$' -count=1 -timeout 90m
ok  github.com/evanphx/cg12/goc  174.634s

$ go test ./goc -run 'TestAllocationCounts' -count=1
ok  github.com/evanphx/cg12/goc  12.751s

$ go test ./goc -run 'TestLoopBodyAllocationsAreDistinctPerIteration|TestLoopAliasExpectationsMatchTheHostToolchain' -count=1
ok  github.com/evanphx/cg12/goc  22.882s
```

`go test ./goc/...`, the capability matrix and `make test-unit` were deliberately
not run: the brief assigns them to a dependent verification job.

## 10. The answer

**The three allocations in `fmt.Sprintf("value=%d", 42)` were:** one heap object
holding the `...` backing array and the boxed argument together; one 16-byte
interface descriptor, allocated because `sync.Pool.Get` returns `any` and goc
heap-allocated a descriptor at every non-nil interface return in the program;
and the result string, which gc pays for too and which is the whole of gc's
1.00. The brief's framing of this as one class was the thing worth correcting:
the backing array and the interface box were already merged into a single
allocation, and the second allocation had nothing to do with variadic calls at
all.

**Two of the three are gone.** The interface-return descriptor is a frame slot —
4 124 heap allocation sites removed across the compiled program, 2 783 of them
`error` returns. The `...` backing array is a frame slot wherever the callee
does not retain what it was handed, which is the case gc frames too.

**The new `fmt.Sprintf("value=%d", 42)` count is 2.00 against gc's 1.00**, down
from 3.00. The remaining one is the boxed argument, which has to be on the heap
because `fmt.pp.doPrintf` assigns each element into a heap-allocated printer;
gc pays nothing for it only because 42 fits in `runtime.staticuint64s`, and
measures **1.74** on the same call with a value that does not. Seven of the
thirteen measured calls now match go1.26.1 exactly, including every variadic
call whose callee retains nothing, `fmt.Sprintf` with no variadic arguments,
`fmt.Sprintf` of a string and of a struct, and the `sync.Pool` round trip.

**The 585-line pessimism set shrank to 563** — 22 lines, or 29 net of the new
corpus program's own lines. That number is small because the differential joins
on source line and **4 125 census rows stopped being positionless**: the
interface descriptors this branch removed have no source line, never joined, and
cannot appear in either direction of that instrument. The permissive set went
202 → 209 and all eight new lines were read; none is a frame address outliving
its frame.

**It is measurable in wall clock: 12%.** 100 000 `fmt.Sprintf` calls through a
`strings.Builder` go from 348 ms to 302 ms, four runs each with non-overlapping
ranges, one fewer GC cycle. goc now allocates less than gc on that workload
(2.03 against 3.34) and is still 9× slower, so this closes part of an allocation
gap and none of the performance gap behind it.

**One thing found along the way is worth more than the numbers.** Section 3.6 is
a dangling pointer this branch created and then removed: a callee retaining an
*element* of a variadic `...any` call retains the whole merged object, and a
depth-0 `ParamNoEscape` does not say it does not. The program printed the right
answer, the frame-escape audit was clean, 387 of 390 corpus outputs were
identical, and the only instrument that showed it was the gc differential's
permissive column — read by hand, one line at a time, because that is what the
column is for.
