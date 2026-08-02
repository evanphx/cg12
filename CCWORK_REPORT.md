# A differential yardstick: goc's escape decisions against the Go compiler's

Branch `ccwork/escape-gc-differential`, off `main` (`efcd4d4`).

Status: IN PROGRESS. Numbers land here as they are produced. Anything I have
not watched to completion is marked UNVERIFIED.

## 0. The host toolchain, pinned

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
| loop variable whose address escapes (`for index := …; { … &index … }`) | 6 | **not a hole** — the join artefact |
| aggregate/range loop variable, mixed with other allocations on the line | 3 | same |

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

**The loop-variable class (6 lines, and the calibration case).** The harness
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
|---|---|---|---|
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

Three of these classes are **not** census blind spots — they are constructs the
census can see and simply has no row for, which means goc placed them as
ordinary front-end frame slots that were never escape candidates. Those are the
ones worth a discriminating program, and they are where the strongest signal in
this whole job is.

### 6.3 The strongest signal: the differential rediscovers goc's own findings

`goc/testdata/frame_escape_baseline.txt` records **8 frame-address publications
in corpus programs** that `opt.FrameEscapes` can prove. The differential, which
knows nothing about that file and derives its answer from `cmd/compile`, flags
**3 of the 8** in its permissive direction:

```
runtime_core_types.go:24              -> IN PERMISSIVE
runtime_core_types.go:28              -> IN PERMISSIVE
runtime_map_struct_value_replace.go:12 -> IN PERMISSIVE
```

Same class in all three: a `[]int{…}` composite-literal backing array left in
the frame, whose address is stored through the write barrier into a heap object
(`barrier / memory reached through a call result $runtime.newobject`). Two
independent instruments, one of them a different compiler, pointing at the same
lines.

### 6.4 What the discriminating programs found

Six reductions, committed under `goc/testdata/escape_gc_differential/` with a
README recording both compilers' current answers. They are outside
`testdata/*.go`, so the corpus glob, the census and every baseline are
untouched.

- **`mapliteral.go`** — `runtime_core_types.go`'s map moved into a callee that
  returns, with the map escaping to a global. goc **heap-allocates both backing
  arrays**, agreeing with `-m` completely. So goc's frame placement in the
  corpus program happens only when the whole structure stays local to the frame:
  goc is being *more precise* than gc, which heaps map-stored values
  unconditionally.

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

```
fmt_sprintf.go:6      heap -> mixed
  src  formatted := fmt.Sprintf("value=%d", 42)
  goc  col 27  heap   newobject  struct_values__1_any__payload0_int
  gc   col 26  frame  slice      ... argument
  gc   col 39  heap   object     42
```

Measured with `runtime.MemStats` over 1 000 calls, compiled and run by each
compiler (`escape_gc_differential/`-style probe, not committed):

| | host go1.26.1 | goc |
|---|---|---|
| `fmt.Sprintf("value=%d", 42)` | **1.00** allocations/call | **3.00** |
| `fmt.Sprintf("value=%d", counter)` | **1.74** | **3.00** |

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
