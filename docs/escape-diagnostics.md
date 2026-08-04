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

**Yes for reasons too, as of `internal/gcdiff`'s reason differential** —
`goc/testdata/escape_gc_reason_differential.txt`, regenerated with

    go test ./goc -run TestEscapeReasonDifferentialAgainstGC -timeout 60m \
        -escape-gc-reason-differential -update-escape-gc-reason-differential

`gcdiff.ParseGocFlagM` keeps the `rule:`, `from:` and `at:` lines
`ParseGCFlagM` skips, and joins them against the flow chains `cmd/compile`
prints at `-m=2`.

This document used to say, in this section, that **comparing reasons across
compilers was not worth doing**, on the grounds that the two vocabularies are
not translations of each other and a differential comparing the strings would
report a disagreement on every joined line. The first half of that is right and
the conclusion did not follow: what is comparable is not the string but the
*mechanism* it names, and there are twelve of those. `internal/gcdiff/reasons.go`
documents them. Over the corpus the taxonomy leaves nothing uncategorised on
either side, and the comparison finds 309 source lines where the two compilers
place the object identically and name incompatible mechanisms — lines no
placement comparison can see, since both its cells say "heap".

What the differential then says about *this* diagnostic is worth reading before
extending it:

- **19% of goc's heap rules are the walk declining to answer.** 227 of 1 192
  are `the walk found a use it could not prove local` or `<name> is used here in
  a way the walk cannot prove keeps it local`, and the first of those carries no
  `at:` either — no rule, no position. They are the largest single obstacle to
  the comparison saying anything.
- **The IR pass explains the machine where gc explains the language.** Its
  reasons are stores (`write barrier into a candidate`, `store into non-local
  storage`) where gc names the construct (`captured by a closure`, `call
  parameter`, `return`). 153 of the 309 disagreements are that mismatch and
  nothing else: both descriptions are true of the same event. Naming what the
  store's destination *is* — a closure object, a result slot, a global — would
  close most of them.
- **A positionless allocation is dropped, not reported.** `EscapeSites` filters
  by file and a site with no position has no file, so goc's `-m` never mentions
  the per-iteration copies the loop rule makes — which are the largest
  documented caveat on the placement differential. Reporting them costs the
  property this whole section is about: `?: subject escapes to heap` does not
  parse as a `cmd/compile` diagnostic.
- **`Kind` is still wrong on goc's side** for anything that joins two
  `GCReport`s. `gcdiff.subjectKind` classifies gc's source-expression vocabulary
  and goc's subject is a type-descriptor symbol, so everything comes out
  `KindObject`. The reason differential does not join on `Kind` and so does not
  care; anything that does will.

**Classifying goc's own disagreements by goc's reason** — the thing this
document did recommend — is in the same file, under `PLACEMENT DISAGREES, ONLY
goc EXPLAINED`: the pessimistic direction grouped by the rule that caused it,
which is what two earlier triage jobs did by hand.

## Where the flag is wired

| what | where |
| --- | --- |
| the level, and the only reader of it | `opt.EscapeDiagLevel` / `opt.SetEscapeDiagLevel` (`opt/escapediag.go`) |
| `GOC_M` | read once at `opt` package init |
| `-m` | `cmd/goc/main.go`, overrides `GOC_M` |
| the report | `opt.WriteEscapeDiagnostics`, called from `goc.compile` after `opt.LowerHeapAllocations` |
| where the report goes | `opt.EscapeDiagWriter` / `opt.SetEscapeDiagWriter`, `os.Stderr` unless redirected. The reason differential redirects the compiler's own copy to `io.Discard` and calls `WriteEscapeDiagnostics` per module, because the level is process-wide and the corpus compiles concurrently |
| the AST walk's half | `goc/escapediag.go` |
| the IR pass's half | `opt/escape.go`'s `candidateEscapes.reasons` |

## The other escape knobs

`-m` is the one to reach for first. The others answer narrower questions:

| knob | what it does |
| --- | --- |
| `GOC_DEBUG_ESCAPE=1` / `=2` | traces the AST walk's "leaks only to result" summaries, and the use that decided an object escapes. Predates `-m` and is a trace, not a report |
| `GOC_DEBUG_ESCAPECHECK=1` | `opt.FrameEscapes`: audits the *emitted stores* for a frame address published past its frame. Answers "is a placement wrong", where `-m` answers "why is it what it is" |
| `GOC_ESCAPE_SUMMARIES=0` | turns off the cross-function fact table, for bisection |
