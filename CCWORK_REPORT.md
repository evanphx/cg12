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
