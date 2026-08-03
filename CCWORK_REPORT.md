# The variadic-call allocation gap: what the three allocations were, and closing them

# A differential yardstick: goc's escape decisions against the Go compiler's

Branch `ccwork/loop-aliasing-fix`, off `main` (`efcd4d4`). The previous job's
report (cross-function escape summaries) is at `efcd4d4:CCWORK_REPORT.md`.

Branch `ccwork/variadic-allocations`, off `main` (`e7c8e33`). The gc differential
job's report is at `origin/ccwork/escape-gc-differential:CCWORK_REPORT.md`.

Branch `ccwork/escape-gc-differential`, off `main` (`efcd4d4`).

Status: IN PROGRESS. Numbers land here as they are produced. Anything not
watched to completion is marked UNVERIFIED.

Status: COMPLETE. Every number below was watched to completion.

Status: IN PROGRESS. Numbers land here as they are produced. Anything I have
not watched to completion is marked UNVERIFIED.

## 0. The defect, reproduced on main before anything was changed

**Headline: `fmt.Sprintf("value=%d", 42)` cost goc 3.00 allocations per call and
now costs 2.00, against gc's 1.00 — and gc's 1.00 is 1.74 with a value it cannot
box into `runtime.staticuint64s`. Seven of the thirteen measured calls reach
exact parity with go1.26.1 that did not before, including every variadic call
whose callee does not retain what it was handed. On a realistic formatting
workload that is 12% of wall clock.**

## 0. The host toolchain, pinned

Host toolchain is `go1.26.1 linux/arm64`; goc built from `efcd4d4`, run as
`goc -run`.

## 0. Where the harness actually is

| program | form | host `go run` | `goc -run` on main |

```
go version go1.26.1 linux/arm64
```

`-gcflags=-m`'s wording is not stable across releases, so every number below is
against **go1.26.1** and a rerun on another release has to re-derive them. The
harness records the version string in its output file for exactly this reason.

## 1. What was already measured, and why it did not answer the question

Everything measured in this effort so far compares two of goc's own analyses:
the AST walk frames 83.4% of placements, the summary-fed IR pass 79.4%. That
says the walk beats goc's own alternative. It says nothing about whether either
is good. `go` is installed on this box and prints its escape decisions on
request; this branch asks it.

## 2. The comparable universe

goc compiles a vendored stdlib out of `stdlib/src`; the host `go` compiles its
own. Positions in those two trees do not correspond, so no join across them is
possible. The comparable universe is therefore exactly **the allocations whose
source text is written in the corpus program's own file** — `goc/testdata/*.go`,
which both compilers read byte-for-byte identically.

Sizing that, from the checked-in census (`goc/testdata/alloc_census_baseline.txt`,
18 664 rows):

| | rows |
|---|---|
| census rows total | 18 664 |
| …at a `goc/testdata/*.go` position | **2 707** (14.5%) |
| …at a `stdlib/src/**` position | 10 094 |
| …with no position at all (`?`) | 5 863 |

The 2 707 comparable rows split **109 frame / 2 598 heap** and cover 378 of the
385 corpus programs.

## 3. What was built

Two files, both committed, plus one committed output:

- `internal/gcdiff/` — parses `-gcflags=-m`, parses the census baseline, joins
  them, renders the report. Unit-tested against a pinned sample of every `-m`
  message shape go1.26.1 produces over this corpus (`go test ./internal/gcdiff`,
  10 ms, runs in ordinary CI).
- `goc/gcdiff_test.go` — the corpus driver, opt-in exactly as
  `TestEscapeSummaryPromotionRate` is, because it depends on the host toolchain.
- `goc/testdata/escape_gc_differential.txt` — the output, checked in.

One command:

```
go test ./goc -run TestEscapeDifferentialAgainstGC \
    -escape-gc-differential -update-escape-gc-differential
```

It takes **10 seconds**, not minutes: it compiles nothing with goc, reading
goc's side out of the already-committed `alloc_census_baseline.txt`, so it
measures the census *as committed* rather than a second census built beside it.
Run without `-update` it re-derives the file and fails on any difference.

A second, per-program mode explains one line instead of counting the corpus:

```
go test ./goc -run TestEscapeDifferentialProgram \
    -escape-gc-differential-program=testdata/runtime_loopvar_address_gc.go -v
```

which prints every `ir.AllocDecision` goc recorded for that program (including
the heap placements the loop rule forced and the ones with no source position),
every census row, every frame address `opt.FrameEscapes` can prove it publishes,
and `-m`'s decisions for the same file, side by side. §6 is what that tool
found; it is the difference between a count and a triage.

## 4. The methodology traps, each answered

**Vendored stdlib.** Not handled by correction — handled by exclusion. Only
allocations whose *source text is written in the corpus program's own file*
are comparable, because that file is the one thing both compilers read
byte-for-byte identically. **10 094 census rows are dropped for being in
`stdlib/src`** and **5 863 for having no position at all**. 2 707 remain.

**Inlining.** The two compilers have opposite conventions, and I checked both
rather than assumed. goc's inliner clones each instruction with the *callee's*
position (`opt/inline.go`'s `Pos: in.Pos`), so an inlined allocation stays
attributed to the line it is written on. `cmd/compile` prints `-m` at the
*outermost* position, so an inlined allocation is reported at the call site —
verified directly: `gc_struct.go` reports `&record{...} escapes to heap` at both
`16:13` (where it is written) and `31:17` (where `allocateGarbage` is inlined).

The join key drops the containing function entirely and keys on
**(corpus file, source line)**. On the gc side, a decision printed at a position
that also carries an `inlining call to` diagnostic is a copy of an allocation
written elsewhere — usually in the *host* stdlib, which goc records under
`stdlib/src` where this join cannot see it — so it is excluded and counted:
**834 excluded**.

**Columns, which is the trap nobody named.** They do not correspond either, and
no offset fixes them. goc records `new(record)` at the column of `new` and gc at
the column of `(`; a map literal at `map` vs at `{`; a boxed `index * 7` at `7`
vs at `*`. Measured: an exact `(line, column)` join matches 1 559 of 2 607 census
positions where the line alone matches 2 115 of 2 473, and the residual deltas
run from −18 to +17 with no mode. Hence the line, not the column.

**Programs that do not build.** **381 of 385 compared.** The 4 that do not are
`abi0_assembly.go`, `bytealg_compare.go`, `chacha8rand_assembly.go` and
`runtime_atomic_assembly.go`, all rejected by the same rule — `use of internal
package internal/… not allowed` — because they import runtime-internal packages
a normal module may not. They cost **37 census rows**, and the coverage table
names each program and the rows it costs, so the denominator can never shrink
quietly.

**Different lowering.** Classified, not counted as disagreement. The
largest single case is channels: `cmd/compile` prints **no** `-m` diagnostic for
`make(chan …)` in any form and has no stack path for one — verified with
`-gcflags=-S`, where a provably non-escaping `make(chan int, 1)` still calls
`runtime.makechan`. So gc heap-allocates every channel unconditionally. Without
that rule all **136** of goc's comparable channel sites would have joined against
nothing and read as "goc allocates something gc does not", which is exactly
backwards — the two compilers agree completely about channels. The harness
supplies the decision, marks it synthesized, and reports the count.

**What counts as a site.** One source position in one function after inlining,
deduplicated, then folded up to the line, because the column and the function
are not portable. A line carrying two allocations the compilers split differently
gets the verdict `mixed` and its own row in the matrix; it is never folded into
either direction, because folding it would be a guess about which object is
which.

**One trap I found that was not on the list, and it is the important one.**
5 863 census rows have no source position. **89 of them are in a corpus
program's own functions, 88 of those on the heap.** A positionless row cannot
join, so a line can read as "goc allocated nothing here" when goc allocated
something anonymous — and §6 shows this is not hypothetical: it is the entire
explanation of the loop-variable class. 88 is the hard bound on how wrong the
position join can be about goc, and it is now printed in the coverage table.

## 5. The confusion matrix

**381 of 385 programs. 2 670 census rows. 3 357 gc decisions. 3 029 joined
source lines.**

```
  goc\gc      frame     heap    mixed   absent    total
  frame          15        3        2       17       37
  heap          194     1762      194      186     2336
  mixed           3       53        1        7       64
  absent        449      119       24        0      592
  total         661     1937      221      210     3029
```

`absent` means "this compiler reported no allocation on this line", which is
*not* "it put the object in a frame". On goc's side the census records every
heap allocation but only those frame placements that came out of an escape
decision — an ordinary front-end slot is invisible. On gc's side `-m` says
nothing about a local it never had to think about. The row and column exist
because pretending otherwise would be the lie.

Read off the matrix:

- **1 762 lines both compilers put on the heap** and **15 both put in a frame**:
  the agreement diagonal, 1 777 lines.
- **585 lines goc heaps and gc does not** — the pessimistic direction. §7.
- **202 lines gc heaps and goc does not** — the permissive direction, and the
  answer to the question this job exists to ask. §6.

## 6. PERMISSIVE — 202 lines gc heaps and goc does not

This is the set the job exists to produce: every one is a candidate frame
address outliving its frame. **202 source lines**, and they are not one thing.
Split by what the census can actually see:

| | lines |
|---|---|
| goc has a **frame** census row on the line | **59** |
| goc has **no census row at all** on the line | **143** |

The second group is not evidence of anything on its own — the census does not
record ordinary front-end frame slots, and several constructs allocate through
runtime helpers the census deliberately does not track. Both groups are
triaged below; the classification is mechanical where the census supports it
and by reading the emitted decisions where it does not.

### 6.1 The 59 with a frame census row

| class | lines | verdict |
|---|---|---|
| `var x T` where goc records **both** a frame and a heap row at the same position | 50 | **not a hole** |
| loop header whose variable's address escapes (`for index := …; { … &index … }`) | 8 | **not a hole** — a join artefact |
| a pair of closures returned together (`runtime_closure_captured_string.go:296`) | 1 | **not a hole** — see below |

**The `var x T` class (50 lines).** `var buffer bytes.Buffer` and its
relatives — `strings.Builder`, `sync.WaitGroup`, `atomic.Value`, `debug.GCStats`.
`-m` says `moved to heap: buffer`. goc records **two** decisions at that one
position in that one function, one heap and one frame, so the line folds to
`mixed`. Ran the per-program tool on `stdlib_log_buffer.go` and
`io_write_string.go`: goc heap-allocates the object whose address escapes and
frames a second temporary of the same type at the same position, and
`opt.FrameEscapes` reports nothing for either program. The escaping object is on
the heap. The line-level join cannot separate the two, which is a limit of the
join, not a defect in goc.

**The closure-pair line (1 line).** `return func(suffix string) { … }, func() string { … }`
returns two closures over a captured string. goc records four rows at that one
line: both closures framed in `main.controls` and both heaped in
`main.stringPair`, which is the same source line decided separately in two
functions. The framing function is the one that does not return them past its
own frame; `opt.FrameEscapes` reports no publication anywhere in the program,
and the program runs correctly under goc (`closure captured string ok`).

**The loop-variable class (8 lines, and the calibration case).** The harness
flagged `runtime_loopvar_address_gc.go:18`, `:36`, `:48`,
`runtime_loopvar_three_clause.go:38`, `:85` and
`runtime_loopvar_value_shapes.go:25` — the family the brief named as the live
calibration example. Good: it means the join works. But the finding does not
survive triage, and the reason is worth recording because it is the sharpest
methodological trap in the whole job.

Reduced `for index := 0; index < 4; index++ { pointers = append(pointers, &index) }`
to `escape_gc_differential/loopaddr.go`. Both compilers print `DISTINCT` and
`0 1 2 3`; goc allocates four cells. The per-program tool shows why the census
disagrees:

```
== goc allocation decisions in loopaddr.go
  5:6   frame  runtime.newobject  int  main.main
== positionless census rows in this program's own functions
  ?     main.main  runtime.newobject  int  heap
```

goc records a **frame** decision at the loop header and a **positionless heap
allocation** for the per-iteration copy. The join is on position, the
positionless row cannot join, and so the line reads as "goc frames what gc
heaps" when goc heaps the thing that escapes. **88 positionless heap rows exist
in corpus-program functions**, and that number is now in the coverage table as
the bound on this effect.

### 6.2 The 143 with no census row

Classified by the construct `-m` named, which is the only description of the
allocation gc gives:

| class | lines | what goc does | verdict |

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

| string concatenation (`a + b`) | 49 | `runtime.concatstring2`, always with a nil buffer (`goc/compile.go`) — always heap | **agreement**, invisible to the census |
| `append` | 25 | `runtime.growslice` — always heap, and explicitly excluded from the census by `opt/alloccensus.go` | **agreement**, invisible by design |
| `[]byte(string)` / `[]rune(string)` | 33 | `runtime.stringtoslicebyte` with a **32-byte stack buffer** when goc's walk says the conversion does not escape | **candidate** — see below |
| `string(byteslice)` | 6 | `runtime.slicebytetostring` | candidate, same reason |
| slice literal `[]T{…}` | 12 | ordinary front-end slot | **candidate** |
| composite literal `&T{…}` / `T{…}` | 11 | ordinary front-end slot | **candidate** |
| `func literal` given to `runtime.AddCleanup` | 10 | goc has a frame path for closures (`gen.localAlloc`) | **candidate** |
| `var x T` with no census row at all | 10 | ordinary front-end slot | candidate |
| string literal boxed into an interface | 4 | | candidate |
| `make(…)` | 2 | | candidate |

Confirmed exactly as the brief states: two forms are already fixed by the loop
rule inside `opt.LowerHeapAllocations`, two are live. `variadic_backing.go`
already agrees with the host; it lands as a regression guard, not as a failing
case.

The three allocations in `fmt.Sprintf("value=%d", 42)` were:

Three of these classes are **not** census blind spots — they are constructs the
census can see and simply has no row for, which means goc placed them as
ordinary front-end frame slots that were never escape candidates. Those are the
ones worth a discriminating program, and they are where the strongest signal in
this whole job is.

## 1. The three programs, landed as failing corpus tests (commit `f343e38`)

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

### 6.3 The strongest signal: the differential rediscovers goc's own findings

`goc/loopalias_test.go` compiles each program, links it against the
cg12-compiled Go runtime, runs it and compares everything it printed against
what Go prints. Each program is run twice, unoptimized and optimized, because
the placement is decided in two different places and a fix in one leaves the
other untouched.

So the brief's framing needs one correction, and it is the most useful thing the
measurement produced: **the variadic backing array is only one of the three, and
it is already merged with the interface box.** Naming this class "the variadic
call" undercounts what is really two independent defects that happen to meet in
`fmt.Sprintf`.

`goc/testdata/frame_escape_baseline.txt` records **8 frame-address publications
in corpus programs** that `opt.FrameEscapes` can prove. The differential, which
knows nothing about that file and derives its answer from `cmd/compile`, flags
**3 of the 8** in its permissive direction:

    $ go test ./goc -run TestLoopBodyAllocationsAreDistinctPerIteration -count=1
    --- FAIL: .../loop_alias_forms.go        got "array: 2 2", want "array: 1 2"
    --- FAIL: .../loop_alias_forms.go_-O     got "array: 2 2", want "array: 1 2"
    --- FAIL: .../loop_alias_composite.go    got "alternate: 2 2\nALIASED: ..."
    --- FAIL: .../loop_alias_composite.go_-O got "alternate: 2 2\nALIASED: ..."
    --- PASS: .../variadic_backing.go
    --- PASS: .../variadic_backing.go_-O

One further gap the decomposition turned up, which is **not** in `Sprintf`'s
three and is broader than variadic calls:

```
runtime_core_types.go:24              -> IN PERMISSIVE
runtime_core_types.go:28              -> IN PERMISSIVE
runtime_map_struct_value_replace.go:12 -> IN PERMISSIVE
```

`TestLoopAliasExpectationsMatchTheHostToolchain` passes: the expectations are
`go run`'s own output, not a belief about it.

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

Same class in all three: a `[]int{…}` composite-literal backing array left in
the frame, whose address is stored through the write barrier into a heap object
(`barrier / memory reached through a call result $runtime.newobject`). Two
independent instruments, one of them a different compiler, pointing at the same
lines.

## 2. What the three new corpus programs cost the baselines, before any fix

## 3. The fixes

### 6.4 What the discriminating programs found

The programs are in `goc/testdata/`, which the corpus audits glob, so they are
also 3 new programs in the census. Regenerated on the **unfixed** compiler so
that the fix's own diff is attributable:

Six changes. Each was measured before and after, and each is committed
separately with its own evidence. 3.6 exists because 3.5 opened a hole; the
sequence is left as it happened rather than tidied into one commit, because the
hole is the most instructive thing in this report.

Six reductions, committed under `goc/testdata/escape_gc_differential/` with a
README recording both compilers' current answers. They are outside
`testdata/*.go`, so the corpus glob, the census and every baseline are
untouched.

    $ go test ./goc -run 'TestAllocationCensus$|TestEscapeShadowPlacement$|TestFrameEscapeAudit$' \
        -update-alloc-census-baseline -update-escape-shadow-baseline -update-frame-escape-baseline
    ok  166.549s, 388 programs

### 3.1 `goc`: an interface result does not need a heap object (commit `6f66dad`)

- **`mapliteral.go`** — `runtime_core_types.go`'s map moved into a callee that
  returns, with the map escaping to a global. goc **heap-allocates both backing
  arrays**, agreeing with `-m` completely. So goc's frame placement in the
  corpus program happens only when the whole structure stays local to the frame:
  goc is being *more precise* than gc, which heaps map-stored values
  unconditionally.

| baseline | delta |

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

- **`stackmove_goroutine.go`** — the sharpest test I could construct. Same map
  built on a **goroutine** stack, so the backing arrays are in a frame that is
  not `main`'s. `opt.FrameEscapes` confirms both addresses are stored into heap
  objects; `-m` heaps both. The goroutine then grows its stack by 400 frames of
  `[512]int` and collects. A stack copy relocates pointers found *on* the stack;
  nothing scans the heap for pointers *into* the stack, so this is where an
  unsafe frame placement has to show. **It does not.** Both print `intact`.

- **`stackmoved.go`** — the control, because the test above means nothing if the
  stack did not move. Both compilers print `stack moved`, so the copy really
  happened.

- **`bytesconv.go`**, **`cleanupclosure.go`**, **`loopaddr.go`** — the
  `[]byte(string)`-escapes, `AddCleanup`-closure and loop-variable classes. All
  three agree with the host.

**Verdict on the permissive set: 0 confirmed holes, and one class I could not
clear.** The `[]int{…}`-inside-a-heap-object class is a confirmed *difference*
that two independent instruments agree on, and an unconfirmed *hole*: I built
the program that should break it, proved the precondition it needs (a real stack
copy) is met, and it did not break. What would settle it is an answer to why
goc's stack copier leaves a heap-held pointer into a moved frame valid — a
question for whoever owns that code, not something this harness can answer.

## 7. PESSIMISTIC — 585 lines goc heaps and gc does not

The performance direction. 585 of 3 029 joined lines — **19.3%** of every line
either compiler decides — cost an allocation, a zeroing and the collector's
attention that the reference implementation does not pay. Ranked, with the
number of distinct corpus programs each class appears in, since I did not
profile execution counts and breadth is the honest proxy I do have:

| lines | programs | class |
|---|---|---|
| **146** | 18 | **variadic call**: goc packs the `...` slice *and* its payloads into one heap object; gc keeps the `...` array in the frame |
| **146** | 95 | other single objects: `new(T)` gc frames, values goc boxes on the heap |
| **138** | 89 | **closures**: `go f(x)`, `defer x.Done()`, `defer runtime.GOMAXPROCS(…)` |
| **91** | 59 | fixed-size backing stores: `[]int{1, 2, 3, 4}` and friends, heaped as `N_int`/`N_byte` |
| **39** | 28 | maps goc heaps and gc frames (`map[string]int{}`, `map[[3]int]string{…}`) |
| **19** | 13 | strings boxed into an interface that gc keeps in a frame |
| **6** | 5 | slices: `make([]byte, n)` gc frames |

Two of these are worth more than their line counts.

**Variadic calls are the largest single class and cost a measured 3×.** Every
`fmt.Sprintf`, `fmt.Println` and `fmt.Sprint` in the corpus is one. The shapes
differ exactly as the differential says:

The five lines are worth reading, because of what is *not* there:

Both `MemStats` programs are `runtimeCapabilityMustPass` in the capability
matrix — they are checked on exit status, not on the number.

```
fmt_sprintf.go:6      heap -> mixed
  src  formatted := fmt.Sprintf("value=%d", 42)
  goc  col 27  heap   newobject  struct_values__1_any__payload0_int
  gc   col 26  frame  slice      ... argument
  gc   col 39  heap   object     42
```

    loop_alias_forms.go:7:8    viaNew     newobject int    heap   <- new(int),  loop rule
    loop_alias_forms.go:22:23  viaMake    newobject 4_int  heap   <- make(...), loop rule
    variadic_backing.go:9:9    (x3)       newobject 1_any  heap

This differential is what caught the `log.Logger.SetPrefix` miscompile in
section 3.3, and it is why I am willing to state that the escape-analysis
changes are sound rather than only that the audits are clean.

Measured with `runtime.MemStats` over 1 000 calls, compiled and run by each
compiler (`escape_gc_differential/`-style probe, not committed):

The two forms that are already correct appear as `heap`, which is the loop rule
in `opt.LowerHeapAllocations` doing its job. The two **broken** forms appear
nowhere: `var a [2]int` and `&cell{v: i}` are committed frame placements, and
the census does not record ordinary front-end frame slots. The instrument is
blind to the defect from the frame side; it will only see the fix from the heap
side.

### 6.2 `TestFrameEscapeAudit` — **zero new publications** (guard 1)

    $ go test ./goc -run TestFrameEscapeAudit -count=1 -timeout 90m
    ok  github.com/evanphx/cg12/goc  279.069s

The audit fails on any publication not in `frame_escape_baseline.txt` **and** on
any listed publication that has gone away. It passed without `-update`, so the
209-line baseline reproduces exactly: nothing this branch newly publishes, and
nothing that stopped being published.

| | host go1.26.1 | goc |
|---|---|---|
| `fmt.Sprintf("value=%d", 42)` | **1.00** allocations/call | **3.00** |
| `fmt.Sprintf("value=%d", counter)` | **1.74** | **3.00** |

| decision site | committed frame placements, corpus-wide |

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

+0.3%, inside the run-to-run spread. The extra walk is per loop body rather
than per function, and a loop body is a small fraction of a function.

## 4. What it moves: two allocations, both of them the defect

    $ go test ./goc -run 'TestAllocationCensus$|TestEscapeShadowPlacement$|TestFrameEscapeAudit$'
    --- FAIL: TestAllocationCensus (168.13s)
    --- FAIL: TestEscapeShadowPlacement
    --- PASS: TestFrameEscapeAudit

**`TestFrameEscapeAudit` is clean.** Zero new frame-address publications across
388 programs, and none of the 209 the tree already makes went away.

The census, over 388 programs each linking the whole standard library and
runtime, reports **two new sites and nothing else**:

    testdata/loop_alias_composite.go:9:8   main.alternate  runtime.newobject  main_cell   now on the heap
    testdata/loop_alias_forms.go:37:3      main.viaArray   runtime.newobject  2_int       now on the heap

    moved frame -> heap:  0        moved heap -> frame:  0        vanished:  0

They read as "appeared" rather than "frame -> heap" because a committed
front-end frame slot is not a census line at all; the same fact from the frame
side is invisible. **Corpus-wide, this change moves exactly two allocations,
and they are the two the bug is about.** The existing loop rule blocks 441
promotions; this adds two allocations. Nothing in the standard library or the
runtime changed placement.

That the census sees exactly two is also a proof that no fire was silently
undone: a variable this rule heap-lifts becomes an `OHeapAlloc` candidate, and
had `LowerHeapAllocations` promoted one back to a frame the census would list
it as a new `frame` site. There are none.

### 4.1 Why two is the right number, checked by hand rather than assumed

A rule that never fires and a rule that is not wired up produce the same zero,
so the "does not over-correct" half was checked directly against the host
toolchain and against the emitted IR, not inferred from the census being quiet.

| probe | shape | host | goc | `newobject` in the loop, main -> fixed |
|---|---|---|---|---|
| stays | `var a [2]int; addTo(&a, i); q := &a` | `18` | `18` | **0 -> 0** |
| nested | inner-loop `var t node`, kept in the *outer* loop's local | `12 12` | `12 12` | 0 -> **2** |
| within | `var x int; p := &x; total += *p` | `12` | `12` | 0 -> 0 |
| range | `x := new(int)` in a `range` loop, kept across iterations | `2 3` | `2 3` | already heap |

`stays` is the over-correction test: the address is taken, passed to a callee
and re-taken, and it keeps its frame slot exactly as it did before, because the
callee does not let it escape. `nested` is the precision test in the other
direction: an allocation in an *inner* loop kept in a variable declared in the
*outer* loop body outlives the inner iteration but not the outer one, and it is
the innermost scope that answers, so it moves.

### 4.2 One new shadow disagreement, and it is the IR analysis being wrong

`TestEscapeShadowPlacement` gains one line:

    loop_alias_composite.go:9:8  main.alternate  composite-literal  heap -> frame  in a loop

Shadow mode asks what the summary-fed IR analysis would have chosen for a
placement the front end kept for itself. The front end now says heap; the IR
analysis says frame, because nothing about the pointer outlives the *frame*.
The `in a loop` column on the same line is the flag that says why acting on
that would be wrong. This is the IR analysis's existing blind spot showing up
against a front end that no longer shares it -- the disagreement is new, the
blind spot is not.

### 4.3 Determinism

    $ go test ./goc -run TestCompilingTheSameSourceTwiceGivesTheSameModule
    ok  4.657s

## 5. A corpus program for the direction the census cannot otherwise see

`goc/testdata/loop_alias_frame_local.go` is the over-correction ratchet. Three
allocations in loop bodies, each finished with before its iteration ends:

    framed          var a [2]int; addTo(&a, i); q := &a    (address to a
                                                            non-retaining callee
                                                            and back to a local)
    consumedWithin  x := i * 2; p := &x; total += *p
    literalWithin   p := &point{x: i, y: i * 2}

Zero `runtime.newobject` in all three, on main and after the fix, checked on
the emitted IR. Output matches the host toolchain. Because the program is in
the corpus, a future change that heaps loop-body allocations without asking
whether anything retains them adds three lines to
`alloc_census_baseline.txt` -- which is the only way that direction gets
noticed, since a frame slot that is *correct* is not a census line at all.

## 6. How general the fix is: four more forms, probed against the host

The two forms named in the brief are not the whole of what was broken. Because
the rule is asked at the variable site rather than at one syntax, the same walk
fixes shapes nobody had reduced:

| shape | host | goc on main | goc fixed |
|---|---|---|---|
| `var b box; b.set(i); q = &b` (pointer method) | `1 2` | **`2 2`** | `1 2` |
| the same inside `for _, v := range values` | `2 3` | **`3 3`** | `2 3` |
| `s := []int{i, i*2}` kept across iterations | `1 2` | `1 2` | `1 2` |
| `b := []byte("ab")` kept across iterations | `49 50` | `49 50` | `49 50` |

The first two were live miscompiles on main, found by asking the fix what else
it covered. The last two are the other frame-committing sites the spike's
census names -- slice-literal backing and the `string`->`[]byte` buffer -- and
they were already correct, so nothing was needed there.

## 7. The existing loop rule is untouched, measured on the same corpus

The brief's scale is "the existing loop rule fires 441 times corpus-wide".
That number was measured on 385 programs; this branch has 389. So the fix was
priced against **the pre-fix tree carrying the same four new programs**
(`a3efe11` in a scratch worktree, `loop_alias_frame_local.go` copied in), which
makes the difference the fix and nothing else:

    $ go test ./goc -run TestEscapeSummaryPromotionRate -escape-promotion-rate

| summaries on | pre-fix, 389 programs | fixed, 389 programs | delta |
|---|---|---|---|
| promoted to a frame slot | 17 005 | 17 005 | **0** |
| lowered to an allocator | 452 128 | 452 130 | **+2** |
| promotion rate | 3.62% | 3.62% | — |
| blocked by the loop rule | 447 | 448 | **+1** |

| summaries off | pre-fix | fixed | delta |
|---|---|---|---|
| promoted | 14 054 | 14 054 | **0** |
| lowered | 455 079 | 455 081 | +2 |
| blocked by the loop rule | 413 | 414 | +1 |

**Not one candidate changed from promoted to lowered or back.** The existing
rule decides exactly what it decided before; the corpus gains two allocations
and one more loop-rule fire, which is the arithmetic of the two new heap
objects: `alternate`'s `cell` becomes an `OHeapAlloc` candidate inside a loop
and is blocked by the rule (+1), and `viaArray`'s `[2]int` is escaped by the
analysis on its own before the rule is consulted.

(441 -> 447 is the four added corpus programs, measured before the fix; the fix
itself is 447 -> 448.)

## 8. The committed tree, checked with no update flags

    $ go test ./goc -run 'TestAllocationCensus$|TestEscapeShadowPlacement$|TestFrameEscapeAudit$'
    --- PASS: TestAllocationCensus (175.20s)
    --- PASS: TestEscapeShadowPlacement (0.00s)

# What survives compilation by goc of log/slog's allocation avoidance

Branch `ccwork/slog-allocations`, off `main` (`a535466`). A MEASUREMENT job: it
builds an allocation benchmark over the paths `log/slog` was designed around,
runs it under goc and under the host Go toolchain, and reports the gap. It
changes no compiler behaviour. Earlier jobs' reports are in git history at
`a535466:CCWORK_REPORT.md`.

Status: COMPLETE. Everything below was run to completion on this machine unless
a line says otherwise.

## The answer, first

    case                goc allocs/op   gc allocs/op   goc B/op   gc B/op
    disabled/3-attr              3.00           0.00      208.0       0.0
    info/5-attr                  9.00           0.00      376.0       0.0

**Almost none of it survives.** All three of the designs named in the brief are
bets on the compiler, and goc loses all three:

  * The **packed `Value`** buys nothing. Its whole point is that `Value.any`
    holds a `Kind` constant instead of a boxed value; under goc that constant is
    a heap allocation, so `slog.Int`, `.Bool`, `.Duration` and `.Float64` each
    cost one allocation where gc costs zero. Only `slog.String` is free, because
    it boxes a pointer.
  * The **disabled-level early return** costs 3 allocations and 208 bytes for a
    call that does nothing. gc reaches zero.
  * The **inline `[5]Attr`** is the one design that partly works: the sixth
    attribute is the first to spill on both compilers. It costs 9 allocations to
    put five attributes into an array that was chosen so it would cost none.

And two of the 32 rows are not numbers at all. goc miscompiles `log/slog` badly
enough that the JSON-handler cases die with `fatal error: invalid pointer found
on stack`. That is section 5, and it is more important than any number here.

## 1. Method

One program, `goc/testdata/slog_allocations/main.go`, compiled and run by both
compilers, measuring itself. Each case is a closure called through a func value,
so neither compiler can see through the call and delete the work. `measure` runs
it 2000 times to warm up, then 5 rounds of 2000, reading `runtime.MemStats`
before and after each round and keeping the round with the fewest mallocs and
the fewest bytes. `runtime.ReadMemStats` stops the world and flushes every mcache
before it answers, so `Mallocs` and `TotalAlloc` are exact counts, not samples.
The minimum over rounds is the right estimator: background runtime work can add
allocations to a round, never remove them.

Same source, same method, both sides. Whatever the instrument gets wrong it gets
wrong equally. One case per process, so a case that kills the runtime costs one
row instead of every row after it.

The instrument is calibrated inside the table. `control/empty-body` must be 0.00
allocations and `control/new-64-byte-object` must be exactly one 64-byte
allocation, under both compilers; both are, so goc's `ReadMemStats` accounts
allocations and bytes the way the reference runtime does and the rest of the
table is measurement rather than artefact. The test asserts these two before it
compares anything.

Host `go1.26.1 linux/arm64`, arm64, 64 cores. One documented command:

    go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations

and to re-record it after a fix:

    go test ./goc -run TestSlogAllocationsAgainstGC \
        -slog-allocations -update-slog-allocations

## 2. The table

Committed at `goc/testdata/slog_allocations_baseline.txt`. `iterations=2000
rounds=5`.

    case                             goc a/op    gc a/op   goc B/op    gc B/op
    control/empty-body                   0.00       0.00        0.0        0.0
    control/new-64-byte-object           1.00       1.00       64.0       64.0
    control/any-int-small                1.00       0.00        8.0        0.0
    control/any-int-large                1.00       1.00        8.0        8.0
    control/any-bool                     1.00       0.00        1.0        0.0
    control/any-string-constant          1.00       0.00       16.0        0.0
    control/any-string-variable          1.00       1.00       16.0       16.0
    control/any-pointer                  0.00       0.00        0.0        0.0
    control/variadic-0-args              0.00       0.00        0.0        0.0
    control/variadic-6-preboxed          1.00       0.00       96.0        0.0
    control/variadic-6-literal           1.00       0.00      176.0        0.0
    control/return-interface             1.00       0.00       16.0        0.0
    control/return-int                   0.00       0.00        0.0        0.0
    control/context-background           1.00       0.00       16.0        0.0
    control/handler-enabled              1.00       0.00       16.0        0.0
    attr/slog.Int                        1.00       0.00        8.0        0.0
    attr/slog.String                     0.00       0.00        0.0        0.0
    attr/slog.Bool                       1.00       0.00        8.0        0.0
    attr/slog.Duration                   1.00       0.00        8.0        0.0
    attr/slog.Float64                    1.00       0.00        8.0        0.0
    info/1-attr                          5.00       0.00      120.0        0.0
    info/3-attr                          7.00       0.00      248.0        0.0
    info/5-attr                          9.00       0.00      376.0        0.0
    info/6-attr                         11.00       1.00      496.0       48.0
    info/3-attr-large-ints               7.00       3.00      248.0       24.0
    logattrs/3-attr                      6.00       0.00      184.0        0.0
    logattrs/6-attr                     11.00       1.00      416.0       48.0
    disabled/no-attrs                    2.00       0.00       32.0        0.0
    disabled/3-attr                      3.00       0.00      208.0        0.0
    disabled/logattrs-3-attr             5.00       0.00      168.0        0.0
    json/kv-4-pairs                     crash       2.00      crash       24.0
    json/logattrs-4-attrs               crash       0.00      crash        0.0

## 3. Which cause each loss is

Three causes, each isolated by a control rather than inferred. Two are the ones
the brief named; the third is new and is the largest single contributor to the
fixed cost of a log call.

### (a) Interface boxing with no static table — confirmed

`control/any-int-small` boxes 7. gc costs nothing because
`runtime.staticuint64s` (`stdlib/src/runtime/iface.go:366`) holds a pre-made
object for every value below 256 and `runtime.convT64` (`:400`) returns a
pointer into it. goc allocates 8 bytes. `control/any-int-large` boxes 2^20,
outside that table, and both compilers allocate 8 bytes.

So the loss is exactly the missing table, not a worse boxing path in general.
`control/any-string-constant` (goc 16 B, gc 0) and `control/any-string-variable`
(both 16 B) say the same for strings, and `control/any-pointer` costs neither
compiler anything, so goc does implement direct interfaces for pointer-shaped
types.

That last fact is what splits the `attr/` rows. `slog.String` boxes a
`stringptr` and is free under goc; `slog.Int`, `.Bool`, `.Duration` and
`.Float64` box a `Kind` constant -- an integer -- and cost 8 bytes each. This is
the packed `Value` design being charged for, directly and per attribute.

The cost is bounded: `info/3-attr-large-ints` costs goc exactly what
`info/3-attr` does, while gc pays 3 extra allocations for it. Values a real
caller logs -- IDs, byte counts, durations -- are outside gc's table too, so on
that row goc's gap is 4 allocations rather than 7.

### (b) The variadic backing array — confirmed, and worse than documented

`control/variadic-6-preboxed` passes six *already-boxed* values to a
`//go:noinline` callee that keeps none of them. Nothing needs converting, so the
only thing left to pay for is the backing array: gc 0, goc 96 bytes, which is six
16-byte interface words exactly. `control/variadic-0-args` costs neither
compiler anything, so it is the array and not the call.

The brief cites `goc/compile.go:3532`, which says the escape summary "does not
describe a variadic parameter". The generated code is more absolute than that.
At `goc/compile.go:6428`:

    stackAllocateVariadic := !g.runtimeAllocation || g.fn.NoSplit || g.forceStackVariadic

`forceStackVariadic` is set only by `runtimeStackVariadicSymbol`
(`goc/compile.go:7916`), a two-name allowlist -- `runtime.traceWriter.event` and
`runtime.traceEventWriter.event`. So a variadic call in ordinary Go code takes
the frame path only when there is no runtime allocator at all or the caller is
`//go:nosplit`. **The escape question is never asked at this site.** Nothing
about `Logger.log` could be learned that would change the answer.

What goc allocates there is not just the array. `goc/compile.go:6431-6458`
builds one synthesized struct per call site, `struct{values [N]any; payload0 T0;
payload1 T1; ...}`, holding the backing array *and* the storage for every
argument that needs boxing, and heap-allocates it in a single `runtime.newobject`.
goc's own decision dump for the benchmark shows it:

    236:42  heap  runtime.newobject  struct_values__2_any__payload0_string__payload1_
    237:42  heap  runtime.newobject  struct_values__6_any__payload0_string__payload1_
    238:42  heap  runtime.newobject  struct_values__10_any__payload0_string__payload1
    239:42  heap  runtime.newobject  struct_values__12_any__payload0_string__payload1

(lines 236-239 are `info/1-attr` through `info/6-attr`.) That is one allocation
per call rather than one per argument -- which is why goc's allocation *counts*
are lower than naive per-box arithmetic predicts and its *byte* counts are
higher. `control/variadic-6-literal` is 176 bytes: `[6]any` (96) plus three
16-byte string payloads plus three 8-byte int payloads, rounded to a size class.

The good news in this is for whoever fixes it: because the boxes live inside the
combined object, proving that one object can stay in the frame removes the
payload allocations with it.

### (c) Returning an interface value allocates — new

`control/return-interface` calls a `//go:noinline` function that takes an `any`
and returns the same `any`. Nothing is converted: the value arrives boxed and
leaves boxed. gc costs nothing; **goc allocates 16 bytes on every call**.
`control/return-int`, the identical call shape with an `int` result, costs
neither compiler anything, so it is returning an *interface* and not returning a
value.

16 bytes is an interface value -- a type word and a data word -- so goc is
materialising the result in heap storage rather than returning it in registers.
The census confirms the shape in the program's own code, where every interface-
returning method has a `runtime.newobject` of the interface type against it:

    ?  main.nopHandler.WithAttrs  runtime.newobject  log_slog_Handler  heap
    ?  main.nopHandler.Handle     runtime.newobject  error             heap

This is the single largest contributor to the fixed cost of a slog call, because
`Logger.log` crosses three interface-returning calls before it does anything:
`context.Background()`, `Logger.Handler()`, and the handler's `Handle` returning
`error`. Each is 16 bytes. `control/context-background` and
`control/handler-enabled` measure two of them at 1 allocation / 16 bytes each.

It also means the cost is not confined to slog. Every `error`-returning call in
a goc-compiled program pays it.

### The decomposition, which closes

Every slog row but one is fully accounted for by (a), (b) and (c) with nothing
left over:

    info/1-attr    5 / 120 = ctx 1/16 + Handler() 1/16 + error 1/16 + combined 1/64  + 1 Kind  1/8
    info/3-attr    7 / 248 = ctx 1/16 + Handler() 1/16 + error 1/16 + combined 1/176 + 3 Kind  3/24
    info/5-attr    9 / 376 = ctx 1/16 + Handler() 1/16 + error 1/16 + combined 1/288 + 5 Kind  5/40
    info/6-attr   11 / 496 = the same, combined 1/352 + 6 Kind 6/48 + Record.back 1/48
    logattrs/3     6 / 184 =           Handler() 1/16 + error 1/16 + combined 1/128 + 3 Kind  3/24
    disabled/none  2 /  32 = ctx 1/16 + Handler() 1/16
    disabled/3     3 / 208 = ctx 1/16 + Handler() 1/16 + combined 1/176
    disabled/lo-3  5 / 168 =           Handler() 1/16 + combined 1/128 + 3 Kind 3/24

`logattrs/6-attr` (11 / 416) leaves one 48-byte allocation unattributed against
that scheme; the analogous `info/6-attr` closes exactly, so the difference is
somewhere in `Record.AddAttrs`'s `slices.Grow` path rather than in the three
causes. Not chased further.

Two things fall out of the decomposition that the raw numbers hide:

  * **The marginal cost of an attribute is 1 allocation and 64 bytes**: one
    8-byte boxed `Kind`, plus 56 bytes of growth in the combined object that
    costs no extra allocation. So the *counts* scale gently and the *bytes* do
    not.
  * **The disabled call's 208 bytes are paid entirely at the call site, before
    slog is entered.** 176 of them are the combined variadic object and 32 are
    two interface-returning calls. `Logger.log`'s early return is doing its job
    perfectly; there is simply nothing left for it to save.

### Where goc matches gc, which bounds the problem

Worth stating as results in their own right: `control/any-pointer`,
`control/return-int`, `control/variadic-0-args`, `attr/slog.String`,
`control/any-int-large` and `control/any-string-variable` all cost the two
compilers the same. goc's boxing is not worse in general, its calls are not
worse in general, and pointer-shaped interface conversion is already right. The
gap is three specific mechanisms, all of them local.

And `Record.front` works: the step from `info/5-attr` to `info/6-attr` gains the
same 48-byte `Record.back` allocation on both compilers. Five attributes really
do fit in the inline array under goc. They just cost 9 allocations to get there.

## 4. What a fix would be worth

Rough, from the decomposition, for `info/5-attr` (9 allocations / 376 B):

  * Fix (c) alone -- return interfaces in registers: **6 / 328**.
  * Fix (b) alone -- ask the escape question at the variadic site, and let one
    that does not escape stay in the frame: **8 / 88**, since the payloads live
    inside the object that moved.
  * Fix (a) alone -- a static table for small integer conversions: **4 / 336**.
  * All three: **0 / 0**, gc's number, since nothing else contributes.

`disabled/3-attr` (3 / 208) goes to **0 / 0** on (b) plus (c) alone; (a) does not
enter it.

## 5. Two miscompiles, reported and not fixed

Found by this benchmark, both reduced, both committed with their controls under
`goc/testdata/slog_allocations/miscompiles/` (README there has the details).
Neither is about allocation. Per the brief, neither is fixed.

### 5a. `slog.Attr` in a frame is scanned as a pointer — serious

    fatal error: invalid pointer found on stack
    runtime: bad pointer in frame main_main at 0x...: 0xc8

Thirteen lines: `slog.Int("k", 200)` passed to a `//go:noinline` function that
calls `runtime.GC()` before touching it. `0xc8` is 200 -- the integer the
attribute carries in `Value.num`, the `uint64` field `log/slog` packs values
into *precisely so that it is not a pointer*. The frame's pointer map says that
word is one, and the collector rejects it. The bad word is in the caller's
frame, so the map is wrong at the value's origin, not at the call.

`attr_bad_pointer_stackcopy.go` reaches the same rejection through
`runtime.adjustpointers` instead, by recursing deep enough to copy the stack. Two
independent walkers disagree with the same map, so the map is what is wrong.

Deterministic: 5 runs out of 5, same offset, same value. This is why both
`json/*` rows are `crash` -- the JSON handler allocates enough and recurses deep
enough to meet a collection with an attribute live in a frame, which nothing
else in the table does. Reading it the other way: **any goroutine holding a
`slog.Attr` in a frame when a collection happens can die.** The rest of the
table only survives because the collections in it happen between calls.

The control (`attr_bad_pointer_control.go`) hand-writes `slog.Value`'s exact
shape -- `_ [0]func()`, a `uint64`, an `any`, behind a `string` key, returned by
value from a non-inlined constructor, held across `runtime.GC()` -- in the
program's own package, and it works. So the zero-length function field alone
does not produce the bad map. I did not find the trigger; that is the next
person's question, and the two programs are in the tree so it can be asked
without rebuilding any of this.

One number was measurable before the crash: a whole-program run got as far as
printing `json/kv-4-pairs` at 15.00 allocations / 456.0 bytes before dying on
the next case. It is not in the baseline, because in the committed one-case-per-
process harness that row crashes too.

### 5b. An interface built in a package-level initializer never registers

    var jsonLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

Nine lines. It links; the first call through `slog.Handler` dies with
`cg12: interface dispatch failed for dynamic type 0x...`. Controls narrow it:
the same expression assigned from inside `main` works; the same conversion
straight into a variable whose declared type is the interface works; the same
shape with a main-package type and a main-package function works; a
main-package type passed to a stdlib function in an initializer works; a stdlib
type passed to a *main-package* function in an initializer fails. So the missed
registration follows the converted type, not the callee, and it is specifically
a conversion at a **call argument** inside a package-level initializer.

`main.go` routes around it by building its loggers in a function. Worth noting
that the workaround was needed at all: the natural way to write a package-level
logger is the way that does not work.

## 6. Proof that no compiler behaviour changed

    go test ./goc -run 'TestAllocationCensus$|TestFrameEscapeAudit$'
    --- PASS: TestAllocationCensus (171.09s)
    --- PASS: TestFrameEscapeAudit (0.00s)

Run on this branch with everything committed. Both reproduce their baselines
exactly. Nothing outside `goc/testdata/slog_allocations*` and
`goc/slogalloc_test.go` was touched; the benchmark lives in a subdirectory so
`filepath.Glob("testdata/*.go")` does not pick it up and the corpus is
unchanged.

## 7. The permanent artefact

  * `goc/testdata/slog_allocations/main.go` — the benchmark, 32 cases, one
    self-measuring program run by both compilers.
  * `goc/testdata/slog_allocations_baseline.txt` — the measured output, both
    columns, committed.
  * `goc/slogalloc_test.go` — `TestSlogAllocationsAgainstGC`, gated behind
    `-slog-allocations` exactly as `TestEscapeDifferentialAgainstGC` is gated
    behind `-escape-gc-differential`. Not wired into any required target.
  * `goc/testdata/slog_allocations/miscompiles/` — the two reductions above,
    each with its control, and a README recording what both compilers print.

The test fails in **both** directions, verified by perturbing the baseline and
reading the failures back:

  * a number that went up: *"goc allocates more than the baseline says ... say
    which cause it is"*
  * a number that went down: *"goc allocates less than the baseline says. That
    is the good direction and it is still a change someone has to look at"*
  * a row that started crashing: *"a case that used to run under goc now kills
    the program. This is a correctness regression"*

and it reports a move in gc's column separately, as a toolchain change rather
than a goc change.

## 8. For the `variadic-allocations` job

These are the before numbers. Re-run is one command
(`go test ./goc -run TestSlogAllocationsAgainstGC -slog-allocations`), and the
diff is the evidence. Three things from here that may save that job time:

 1. The variadic placement decision at `goc/compile.go:6428` does not consult
    escape analysis at all; teaching the summary about variadic parameters will
    not change anything until that line asks.
 2. The boxed payloads live *inside* the same object as the backing array
    (`goc/compile.go:6431-6458`), so moving that object to the frame removes
    both costs at once. `info/5-attr` would go from 376 bytes to 88.
 3. Interface-returning calls (cause c) are a separate 16 bytes each and will
    still be there afterwards: 3 allocations of the 9 in `info/5-attr`, and 2 of
    the 3 in `disabled/3-attr`. Fixing variadics alone will not make the
    disabled case free.

## Bottom line

**Census delta: 2 allocations, both in the reduction programs.** Zero sites
moved frame->heap or heap->frame anywhere in the standard library or runtime
across 389 programs; the promotion count is identical to the pre-fix tree
(17 005), so the existing loop rule was not disturbed -- it gains exactly one
fire, 447 -> 448. Compile time is unchanged within the run-to-run spread
(12.01 s -> 12.05 s on `stdlib_crypto_ecdsa.go`).

**`TestFrameEscapeAudit` is clean**: zero new frame-address publications, and
none of the 209 already listed went away.

gc pays one allocation for the constant case — the result string. Its `...`
array is in the frame and a small integer boxes to `runtime.staticuint64s` with
no allocation at all. goc pays three. That is the single largest concrete
performance gap this comparison found, and it sits on the most-used formatting
path in the tree.

**Closures (138 lines, 89 programs) are `go` and `defer`.** `defer wait.Done()`
and `go worker(done)` allocate a closure object on the heap in goc; gc frames
the argument struct, because a `defer`'s closure provably dies with the frame
and a `go` statement's argument struct is copied by the runtime. 89 of 381
compared programs contain the pattern, so it is not a corner.

Nothing here is a correctness question, and none of it is a bug this branch
should fix. It is the ranked list of where goc's escape analysis costs something
measurable against the reference, and the first two entries are worth more than
the remaining five put together.

## 8. Proof that nothing in the compiler changed

The brief says report bugs, do not fix them, and prove it. Every file this
branch changes is either this report or a new file: `git diff --stat main..HEAD`
lists `CCWORK_REPORT.md` and thirteen additions, and nothing under `parse/`,
`ir/`, `opt/`, `lower/`, `arm64/`, `amd64/` or `goc/compile.go` is among them.
The corpus is still 385 programs — the discriminating reductions live in a
subdirectory that `filepath.Glob("testdata/*.go")` does not match.

The observable proof is that the baselines that record what the compiler does
still reproduce, byte for byte, at this HEAD:

```
$ go test ./goc -run 'TestAllocationCensus|TestFrameEscapeAudit|TestEscapeShadowPlacement' -timeout 90m
ok  github.com/evanphx/cg12/goc  165.943s
```

All three: the 18 664-row allocation census, the 193-line frame-escape audit,
and the escape shadow-placement baseline. Plus `go test ./internal/gcdiff`
(10 ms) and the differential re-deriving its own committed output without
`-update`.

## 9. What was committed

| file | what |
|---|---|
| `internal/gcdiff/gcdiff.go` | `-m` parser and census reader, with the join documented at length |
| `internal/gcdiff/join.go` | the join, the verdict folding, the two directions |
| `internal/gcdiff/report.go` | the rendered, diffable output |
| `internal/gcdiff/gcdiff_test.go` | unit tests pinning the go1.26.1 `-m` grammar; ordinary CI, 10 ms |
| `goc/gcdiff_test.go` | the corpus driver and the per-program triage mode, both opt-in |
| `goc/testdata/escape_gc_differential.txt` | the output, 4 100 lines, checked in |
| `goc/testdata/escape_gc_differential/` | six discriminating programs and a README recording both compilers' answers |

Two commands, both documented in the files themselves:

```
go test ./goc -run TestEscapeDifferentialAgainstGC \
    -escape-gc-differential -update-escape-gc-differential          # 10 s

go test ./goc -run TestEscapeDifferentialProgram \
    -escape-gc-differential-program=testdata/<program>.go -v        # ~2 min
```

Neither runs in CI: both need a host Go toolchain, and pinning the build
machine's go version into the contract is exactly what makes an
externally-dependent test a liability. The output is committed so the numbers
can be diffed instead of reconstructed, which is what the earlier ad-hoc
measurement in this effort failed to do.

## 10. The answer

**381 of 385 corpus programs compared** (the 4 excluded are named, with the 37
census rows they cost). **2 670 census rows and 3 357 gc decisions joined into
3 029 source lines.**

```
  goc\gc      frame     heap    mixed   absent    total
  frame          15        3        2       17       37
  heap          194     1762      194      186     2336
  mixed           3       53        1        7       64
  absent        449      119       24        0      592
  total         661     1937      221      210     3029
```

- **1 777 lines agree** (1 762 both heap, 15 both frame).
- **585 lines goc heaps and gc does not** — pessimism. Largest class is the
  variadic call, measured at **3.00 allocations per `fmt.Sprintf` against gc's
  1.00**.
- **202 lines gc heaps and goc does not** — the number this job exists to
  produce.

**Of those 202: 59 have a hard frame row in goc's census, 143 have no census row
at all. Every one was triaged, and none is a confirmed hole.**

- 50 of the 59 are `var x bytes.Buffer`-shaped, where goc records a heap
  allocation *and* a frame temporary at the same position; the escaping object
  is on the heap and the line-level join cannot separate the two.
- 8 of the 59 are loop headers, including the calibration case the brief
  named — and the harness flagging them is a sign it works, but they are a join
  artefact: goc records a frame decision at the loop header and a
  **positionless** heap allocation for the per-iteration copy, which cannot
  join on position. 88 such positionless heap rows exist corpus-wide, and that
  bound is now printed in the coverage table.
- 74 of the 143 are string concatenation and `append`, which goc always heaps
  through runtime helpers the census does not track: agreement the census
  cannot see.
- The remaining 69 are candidates, and the strongest of them —
  `[]int{…}` left in a frame with its address stored into a heap object — is a
  class the differential and goc's own `opt.FrameEscapes` audit **independently
  agree on**, 3 lines of it. I built the program that should break it, proved
  its precondition (a real goroutine stack copy) is met, and it did not break.

So: **goc is not more dangerous than the reference on this corpus — it is
different, and measurably more expensive.** The permissive direction has one
open question, stated precisely and with a committed program that will answer it
the day goc's stack copier stops covering for it. The pessimistic direction has
a ranked, quantified list of where the 585 lines cost real allocations.

The yardstick exists, it is one command, and its output is in the tree.

**One thing found along the way is worth more than the numbers.** Section 3.6 is
a dangling pointer this branch created and then removed: a callee retaining an
*element* of a variadic `...any` call retains the whole merged object, and a
depth-0 `ParamNoEscape` does not say it does not. The program printed the right
answer, the frame-escape audit was clean, 387 of 390 corpus outputs were
identical, and the only instrument that showed it was the gc differential's
permissive column — read by hand, one line at a time, because that is what the
column is for.

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

Four programs in `goc/testdata/`, run by `goc/slogattrframe_test.go`
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
The fourth program (`_kinds`) holds an Int64, a Bool, a Duration and a Float64
in one frame at once, because `num` carries all five packed kinds and a map that
claims that word claims it for every one of them.

