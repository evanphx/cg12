# The variadic-call allocation gap: what the three allocations were, and closing them

Branch `ccwork/variadic-allocations`, off `main` (`e7c8e33`). The gc differential
job's report is at `origin/ccwork/escape-gc-differential:CCWORK_REPORT.md`.

Status: IN PROGRESS. Numbers land here as they are produced. Anything not
watched to completion is marked UNVERIFIED.

**Headline so far: `fmt.Sprintf("value=%d", 42)` cost goc 3.00 allocations per
call and now costs 1.00 — exactly what go1.26.1 costs.**

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
  hypothesis (b) is confirmed as real. It does **not** appear in `Sprintf`
  because the box there is merged into the variadic object, which is now a frame
  slot — but every `f(x any)` with a small int in the corpus still pays it. It is
  left undone and is the obvious next job; see section 7.

## 3. The fixes

Four changes. Each was measured before and after, and each is committed
separately with its own evidence.

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

## 4. Where the numbers are now

`goc/testdata/allocation_counts.go`, run under both compilers. Allocations per
call. Deterministic over three runs each, and identical with and without `-O`.

| call | gc | goc before | goc after |
|---|---|---|---|
| `fmt.Sprintf("value=%d", n)` | 1.00 | 3.00 | **1.00** |
| `fmt.Sprintf("value=%s", s)` | 2.00 | 3.00 | **1.00** |
| `fmt.Sprintf("value=%v", struct)` | 2.00 | 3.00 | **1.00** |
| `fmt.Sprintf("a constant format")` | 1.00 | 2.00 | **1.00** |
| `f(...int)` with 2 args | 0.00 | 1.00 | **0.00** |
| `f(...any)` with an int and a string | 0.00 | 1.00 | **0.00** |
| `f(x any)` with a small int | 0.00 | 1.00 | 1.00 |
| `f(x any)` with a pointer | 0.00 | 0.00 | 0.00 |
| `func(int) any` | 0.00 | 2.00 | 1.00 |
| `func(*int) any` | 0.00 | 1.00 | **0.00** |
| `sync.Pool` Get/Put | 0.00 | 1.00 | **0.00** |
| `fmt.Sprintf` **written inside a loop body** | 1.00 | 3.00 | 2.00 |
| `f(...int)` **written inside a loop body** | 0.00 | 1.00 | 1.00 |

Two rows are goc paying **less** than gc: `%s` and `%v` of a value gc has to box
onto the heap, where goc's front end packs the box into the same frame object as
the `...` array.

Two rows are the honest limit of this work. `opt.promotionsBlockedByALoop`
refuses to promote any allocation written inside a loop body, because a frame
slot is one object and each iteration may need its own — that is the rule the
loop-aliasing job landed, and guard 4 says not to regress it. It is blunt: it
does not ask whether anything retains the object past the iteration, and for a
variadic backing array nothing does. A `fmt.Sprintf` written directly in a loop
body therefore still costs 2 rather than 1. Lowering that is a real optimisation
someone could do, and it is deliberately left alone here rather than attempted
alongside four other changes. The two `_in_loop` rows exist so the price is a
number somebody can see rather than a surprise.

## 5. The guards

### 5.1 Corpus output differential (my own check, not one the brief asked for)

389 corpus programs compiled, linked and run by goc built from `main`, and by
goc built from this branch, with the two outputs compared byte for byte. This is
what caught the `log.Logger.SetPrefix` miscompile in section 3.3, and it is the
reason I am willing to state the escape-analysis changes are sound rather than
only that the audits are clean.

### 5.2 `TestFrameEscapeAudit` — **zero new publications**

    $ go test ./goc -run TestFrameEscapeAudit -count=1 -timeout 90m
    ok  github.com/evanphx/cg12/goc  265.947s

The audit fails on any publication not in `goc/testdata/frame_escape_baseline.txt`
*and* on any listed publication that has gone away. It passed without `-update`,
so the 209-line baseline reproduces exactly: no frame address this branch newly
publishes, and none that stopped being published either.

(more to come: census delta, gc differential, wall clock)

### 5.3 Wall clock — the allocation win IS measurable

The proxy is allocations; the point is time. A realistic formatting workload:
100 000 `fmt.Sprintf("id=%d name=%s score=%d", int, string, int)` calls through
a `strings.Builder`, timed inside the program with `time.Since` so process
startup is not in the number. `GOMAXPROCS=1`, three runs each, all within 5%.

| | ns per Sprintf | allocations per Sprintf | GC cycles |
|---|---|---|---|
| goc @ `main` | **3 540** | 3.03 | 7 |
| goc @ this branch | **2 661** | 1.03 | 4 |
| host go1.26.1 | 370 | 3.34 | 5 |

**25% faster**, 354 ms → 266 ms for the same work, and reproducibly so. Roughly
half of that is the two allocations themselves and half is the collector: the
run does 4 GC cycles instead of 7 because it allocates a third as much.

Two things worth saying plainly:

- goc now allocates **less than gc** on this workload — 1.03 against 3.34,
  because gc boxes the `string` and the `int`s onto the heap where goc's front
  end packs them into the same frame object as the `...` array — and is still
  **7× slower**. Whatever is left between goc and gc on this path is not
  allocation. This branch closes the allocation gap it set out to close and
  should not be read as closing the performance gap.
- The 25% is measured on a call site in a helper function, which is where
  formatting calls usually are. A `fmt.Sprintf` written directly in a loop body
  keeps two of its three allocations, for the reason in section 4.

### 5.4 `TestLoopBodyAllocationsAreDistinctPerIteration` — guard 4, clean

    --- PASS: loop_alias_forms.go        (and -O)
    --- PASS: loop_alias_composite.go    (and -O)
    --- PASS: variadic_backing.go        (and -O)
    --- PASS: loop_alias_frame_local.go  (and -O)
    --- PASS: TestLoopAliasExpectationsMatchTheHostToolchain

All four forms still print what the host toolchain prints, and the
frame-local counter-example still prints `framed: 18 / within: 12 / literal: 18`.
`variadic_backing.go` is the one that moved underneath: its `retainNothing(&x)`
backing array is now a frame slot, so the frame address `&x` goes into storage
in the same frame rather than into a heap object — which is why the frame-escape
baseline is unchanged rather than one line shorter.
