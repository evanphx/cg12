# `goc -m`: asking the compiler why an object went where it went

`goc -m` prints, for every allocation in the program being compiled, where the
object was placed and which rule placed it. `goc -m=2` also prints the chain of
questions between the object and the use that decided, the way `cmd/compile`'s
`-m=2` explains an escape path.

It is off by default and costs nothing off. See "Cost when off" below.

    goc -m   file.go        # placements, and the rule that decided each one
    goc -m=2 file.go        # also the chain to the deciding use
    GOC_M=2 goc file.go     # the environment form, for a driver that is not goc

`-m` on the command line overrides `GOC_M`. The report goes to stderr, as gc's
does.

## Asking about a package the program only imported

By default the report covers the file the compiler was pointed at and nothing
else, because goc compiles a whole program including a vendored standard library
and the lines a reader asked for would otherwise be lost among ten thousand
others.

The question that matters is often about the standard library, though -- "why is
`log/slog`'s `handleState` on the heap" is not answerable from the program's own
file. `-m-match` takes a substring and reports every source path containing it:

    goc -m=2 -m-match log/slog prog.go
    GOC_M=2 GOC_M_MATCH=slices/slices.go goc prog.go

    stdlib/src/log/slog/handler.go:119:2: log_slog_handleState does not escape
    	ir pass: heap-alloc-candidate in log/slog.defaultHandler.Handle
    stdlib/src/log/slog/handler.go:120:8: struct_code_uintptr__receiver__log_slog_handleSt does not escape
    	front end: method-value-descriptor in log/slog.defaultHandler.Handle

The match replaces the default restriction rather than adding to it, so
`-m-match log/slog` does not report the program's own file. `-m-match ""` is the
default; there is no way to ask for everything, and asking for it would not be
useful.

## Why it exists

Every escape-analysis defect found in this tree so far was found by building a
new instrument: a static audit of the emitted IR (`opt.FrameEscapes`), an
allocation census with a two-way baseline (`opt.AllocationCensus`), a
differential against `cmd/compile`'s own `-m` (`internal/gcdiff`), a `log/slog`
allocation benchmark. Every one of them answers *where the object went*, and
after it has answered, the next question is always *why* — and the way that
question got answered was by reading `goc/compile.go` and reasoning about which
branch must have fired.

This answers it directly, from the compiler, in the compiler's own words.

## The output

    escape_diagnostic.go:13:7: main_point does not escape
    	front end: composite-literal in main.framed
    escape_diagnostic.go:18:7: main_point escapes to heap
    	front end: composite-literal in main.throughCall
    	rule: assigned to the package-level variable keptPointer
    	from: p, declared at escape_diagnostic.go:8:18
    	from: argument 0 of the call to main.keepPointer
    	from: p, declared at escape_diagnostic.go:18:2
    	at:   escape_diagnostic.go:8:30

The unindented line is the decision, worded exactly as `-m` words it: a
position, a subject, and `does not escape` or `escapes to heap`. Everything goc
has to add is on tab-indented continuation lines, which is how gc formats its
own continuations — and, deliberately, what makes this output parseable by the
same code that reads gc's. See "Can `internal/gcdiff` read this?" below.

The continuation lines are:

| line | at level | what it is |
| --- | --- | --- |
| `front end:` / `ir pass:` | 1 | which of goc's two placers decided, the construct it recorded, and the IR function after inlining |
| `rule:` | 1 | the rule that decided, in the deciding analysis's own words. Absent on a frame placement, which is the absence of a publication rather than the presence of a rule — gc says only "does not escape" there too |
| `from:` | 2 | one link of the chain between the object and the deciding use, ordered from the use outwards |
| `at:` | 2 | where the deciding use is written |

The subject on the decision line is the type-descriptor symbol, rendered the way
`goc/testdata/alloc_census_baseline.txt` renders it (`opt.AllocationTypeName`),
so a line of this report and a line of the census name the same thing. gc puts
the source expression there; goc has no rendering of the source expression at
the point it decides.

One source line can produce several blocks. A function inlined into three
callers is decided three times — each copy separately, and each can land
differently — so each copy is reported under the function it ended up in. That
is the same convention `opt.AllocationCensus` uses and the reason
`internal/gcdiff` does not join on the containing function.

### Scope

The report covers the file the compiler was pointed at. goc compiles the
vendored standard library along with the program; a report that included it
would be ten thousand lines of `stdlib/src` around the handful the reader asked
for. gc's `-m` makes the same choice for the same reason: it reports the package
being compiled.

## The two placers

goc decides an allocation's placement in one of two places, and `-m` reports
both because a reader looking at one source line has no way to know which one
answered it:

- **`front end`** — the AST walk in `goc/compile.go`, which commits most
  allocations itself (`&T{...}`, slice and composite literals, method-value
  descriptors, `make` with a constant capacity). Recorded in `ir.PlacedAlloc`.
- **`ir pass`** — `opt.LowerHeapAllocations`, which decides the neutral
  `OHeapAlloc` candidates the front end declines to place (`new(T)`, boxed
  interface payloads, variadic backing arrays). Recorded in `ir.AllocDecision`.

## Where the reasons come from

From the decision, not from a re-derivation of it.

This matters more than it sounds. This tree has twice shipped a defect that was
two components enforcing one rule differently — a write-barrier check whose gate
was narrower than the function it mirrored, and an escape summary produced with
one meaning and consumed with another. A diagnostic that recomputed the answer
would be a third component of the same kind, and its failure mode is the worst
one available: it would confidently explain a decision the compiler did not
make.

So nothing here recomputes anything.

- The **IR pass** already had a per-allocation reason map
  (`opt.candidateEscapes.reasons`, built for shadow mode). It is written by the
  mark loop *as it marks* — the `mark` and `markContents` closures in
  `analyzeCandidateEscapes` take the reason as an argument from the branch that
  is marking. The pass now asks for that map exactly when the diagnostic is on,
  and `recordAllocDecision` carries the reason into the record. A source
  position was added alongside it, taken from the marking instruction at that
  moment — it has to be taken there, because the rewrite immediately afterwards
  replaces those instructions.

- The **AST walk** deposits its explanation in `goc/escapediag.go` as it
  answers. `gen.diagRule` is called from inside the branch that returns the
  escaping answer, and first write wins: the walk short-circuits, so the first
  branch on a failing path to name itself is the deepest one reached, which is
  the one that decided. `gen.diagQuestion`/`gen.diagResolve` bracket each
  sub-question — one that answers "does not escape" decided nothing and whatever
  it recorded is dropped; one that answers "escapes" keeps it and adds itself to
  the chain, which is what makes the chain come out ordered from the deciding use
  outwards.

The explanation is harvested by `gen.escapeWhy` at the instant the placement is
recorded. Where the decision and the record are separated by further escape
questions — the `&T{...}` path asks about the literal's elements in between — it
is carried through explicitly (`gen.recordPlacementWhy`, and `compositeLiteral`'s
`why` parameter) rather than re-read, so the explanation printed beside a
placement is always the answer to the question that made *that* placement.

## Cost when off

At level 0:

- `analyzeCandidateEscapes` is called with `wantReasons` false, so the reason map
  is never allocated and no reason string is ever formatted;
- `gen.escapeDiag` is a nil pointer, and every hook on it is a nil check. Each
  message is built inside a closure the hook never calls, so no `FullName`, no
  `Sprintf`, and nothing allocated. The closures do not escape the hooks;
- `allocationsInLoops` does not build its loop-header position map;
- nothing is printed.

The records the reasons ride on — `ir.PlacedAlloc` and `ir.AllocDecision` — were
already being made on every compile before this existed, and neither is carried
by `ir.Module`'s binary encoding.

Two tests state the claim:
`TestEscapeDiagnosticOffRecordsNothing` (no reason, use or chain is recorded at
level 0) and `TestEscapeDiagnosticDoesNotChangeTheEmittedModule` (compiling the
same source at level 0 and level 2 gives byte-identical serialized modules).

## Can `internal/gcdiff` read this?

**Yes for placements, today, with no change to either side.** goc's `-m` output
goes through `gcdiff.ParseGCFlagM` — the strict parser for `cmd/compile`'s `-m`,
which refuses to skip a diagnostic it has not been taught — with zero
`Unknown` lines. That is what the decision line's wording and the tab-indented
continuations are for, and `TestEscapeDiagnosticParsesAsGCFlagM` pins it.

**Two things would have to change to make it useful for a reason-level
differential**, and they are not the same size:

1. **`Kind` is wrong on goc's side, and the fix is small.**
   `gcdiff.subjectKind` classifies gc's *source-expression* vocabulary —
   `make(chan `, `map[`, `make([]`, `... argument` — and goc's subject is a
   type-descriptor symbol, so everything comes out `KindObject`. gcdiff already
   has the right mapping for goc on the other side of the join:
   `CensusRow.Kind()` classifies by allocator. Emitting the allocator in goc's
   subject, or teaching `subjectKind` to recognise it, is a few lines. Worth
   doing if anything ever joins two `GCReport`s.

2. **Reasons are dropped, and keeping them is not the hard part.**
   `ParseGCFlagM` skips every tab-indented line, so goc's `rule:`, `from:` and
   `at:` are discarded. Adding a `Reason` field to `GCDecision` and reading those
   lines is mechanical.

   What is *not* mechanical is what you would do with it. gc's `-m=2` reason
   vocabulary and goc's are different vocabularies describing different
   analyses: gc says `leaking param: p to result ~r0 level=0` and
   `flow: {heap} = &{storage for ...}`, goc says `passed to F, which may retain
   argument 0` and `write barrier into non-local storage`. There is no mapping
   between them, and a differential that compared the two strings would report a
   disagreement on every joined line. **Comparing reasons across compilers is not
   worth doing.**

   **Classifying goc's own disagreements by goc's reason is worth doing**, and it
   is what the last two triage jobs did by hand: the 113 lines where goc heaps
   what gc frames were sorted into twelve groups, and the groups are goc rules.
   That grouping is exactly `GROUP BY rule` over this diagnostic. Doing it would
   turn a day of reading `goc/compile.go` into a run of the differential, which
   is the actual return on making the output parseable.

   The concrete change: have `TestEscapeDifferentialAgainstGC` compile each
   corpus program with `GOC_M=1` alongside the census it already reads, join by
   `(file, line)` as it already does, and print the disagreement classes with
   their goc rule. No new analysis, one new column.

## Where the flag is wired

| what | where |
| --- | --- |
| the level, and the only reader of it | `opt.EscapeDiagLevel` / `opt.SetEscapeDiagLevel` (`opt/escapediag.go`) |
| `GOC_M` | read once at `opt` package init |
| `-m` | `cmd/goc/main.go`, overrides `GOC_M` |
| the file filter | `opt.EscapeDiagMatch` / `opt.SetEscapeDiagMatch`, read by `opt.EscapeSites` |
| `GOC_M_MATCH` | read once at `opt` package init |
| `-m-match` | `cmd/goc/main.go`, overrides `GOC_M_MATCH` when non-empty |
| the report | `opt.WriteEscapeDiagnostics`, called from `goc.compile` after `opt.LowerHeapAllocations` |
| the AST walk's half | `goc/escapediag.go` |
| the IR pass's half | `opt/escape.go`'s `candidateEscapes.reasons` |

## The other escape knobs

`-m` is the one to reach for first. The others answer narrower questions:

| knob | what it does |
| --- | --- |
| `GOC_DEBUG_ESCAPE=1` / `=2` | traces the AST walk's "leaks only to result" summaries, and the use that decided an object escapes. Predates `-m` and is a trace, not a report |
| `GOC_DEBUG_ESCAPECHECK=1` | `opt.FrameEscapes`: audits the *emitted stores* for a frame address published past its frame. Answers "is a placement wrong", where `-m` answers "why is it what it is" |
| `GOC_ESCAPE_SUMMARIES=0` | turns off the cross-function fact table, for bisection |
