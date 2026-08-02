# A closure leaves a captured string variable pointing at its dead frame

Branch `ccwork/closure-string`, off `main` (`0505d90`). RUNTIME_PLAN.md §5.10, first
bullet under "Known miscompiles, not covered by any capability".

**Verdict: fixed, and it was three type classes rather than one.** The defect is
not about strings and not about `range`; it is about every local variable cg12
keeps in a frame slot as a pointer to a separate sixteen-byte value — a string,
an interface, or a `complex128` — assigned from inside a closure that captured it
by reference. 27 of 70 differential programs disagreed with the host toolchain;
26 agree now, and the 27th is a different defect this branch measures and
deliberately does not fix (section 3).

Both matrix arms are 347 subtests / 346 PASS / 1 declared EXPECTED FAILURE /
0 FAIL / 0 KNOWN GAP, all 367 corpus programs are still byte-reproducible in all
three configurations, and §5.13's three range-over-function cases assert what
they originally wanted to. What is *not* established is section 9.

This file was written as each result landed; the sections are in the order they
were measured.

## 1. Reproduced

`RUNTIME_PLAN.md` §5.10's program, verbatim, compiled with `goc` built from this
tree:

```
inside az
outside
```

The host toolchain prints `inside az` / `outside az`. Confirmed on `0505d90`.

## 2. The full shape, measured against the host toolchain

65 differential programs (§3 step 2: each run with `go run` and with `goc`, stdout
and exit status compared). **27 differ.**

### What is affected

A **local variable whose cg12 representation is "the frame slot holds a pointer to
an inline 16-byte value"**, assigned inside a **non-escaping function literal that
captured it by reference**. That is three type classes:

| type class | example | verdict |
| --- | --- | --- |
| `string` | `log = log + s` | **wrong** |
| named type with underlying `string` | `type label string` | **wrong** |
| interface (`any`, `error`, …) | `v = n`, `err = errors.New(...)` | **wrong** |
| `complex128` | `c = complex(3, 4)` | **wrong** |

Every assignment form is affected — the operator and the right-hand side are not
the discriminator:

| form | program | goc |
| --- | --- | --- |
| `log = log + s` (computed) | c01 | `fatal error: runtime: out of memory` |
| `log = "az"` (**string literal**) | c02 | garbage bytes |
| `log += s` | c03 | `fatal error: runtime: out of memory` |
| `log = fmt.Sprint(n)` | c04 | OOM |
| `log = s` (**plain parameter copy**) | c05 | OOM |
| `log = string(b)` | c27 | OOM |
| `n, log = 1, log+s` (tuple) | c25 | OOM |
| `a, b = b, a` (swap) | c26 | OOM |

and every way of reaching the closure:

| shape | program | goc |
| --- | --- | --- |
| named function-literal variable, called | c01 | wrong |
| immediately-invoked function literal | c28 | wrong |
| function literal nested in a function literal | c21 | wrong |
| function literal passed to a **generic** function | c62 | wrong |
| `for i := range seq` over a function iterator | c19, c40 | wrong |
| deferred literal inside a non-escaping literal | c17 | SIGSEGV |
| the captured variable is a **parameter** of the enclosing function | c42 | wrong |

### What is *not* affected, and why

| shape | program | why it is correct |
| --- | --- | --- |
| read-only capture | c06, c44, c49 | nothing is assigned |
| **slice** | c07, c08, c38, c47 | under `runtimeAllocation` a slice local is stored **inline** (3 words in the slot); `assignLocal` writes the three words, it does not rebind |
| **struct / array** | c11, c12, c46, c63 | `isMemoryValue`: the slot holds a *stable backing address* and `assignLocal` copies into the backing |
| `int`, `bool`, pointer, map, chan, func, `complex64` | c13, c15, c14, c53, c52 | one word in the slot |
| **escaping** closure — goroutine | c16, c48 | `variableStorage` heap-lifts the variable and sets `directValues`, so the storage *is* the header |
| **escaping** closure — returned | c34 | same |
| named result written by a closure | c22, c43, c45 | `resultStorage` gives direct storage |
| `&v` taken and escaping | c50 | heap-lifted for the same reason |
| write through a pointer parameter | c20, c36 | the address path stores inline |
| assignment to a **field or element** of a captured variable | c23, c24, c63 | goes through `assignmentTargetAddress`, which stores inline |
| `defer` registered in a loop | c18 | §5.1 heap-lifts those captures |

Two cases matched *by luck* on the first pass and differ once a call clobbers the
dead frame — recorded because they are the reason a green matrix would not find
this: `c37` (`if ok { log = log + s }` inside the closure) and `c39` (the captured
variable is a parameter). Adding a recursive call between the write and the read
turns both into `result ` — see `c41`, `c42`. **A "passing" observation of this bug
is worth nothing unless the frame is clobbered between the write and the read.**

### The symptoms, and why there are so many

§5.10 recorded two observable shapes. There are at least six, and which one you get
is decided by what the dead frame happens to hold when it is read back:

- silently empty string (the §5.10 reducer)
- garbage bytes printed as the string's content (c02, c33)
- `fatal error: runtime: out of memory` — the length word is read as a huge number
  (c01, c03, c05, …)
- `SIGSEGV` in `runtime_concatstrings` / `goc_memmove` (c17, c19, c40)
- `unexpected fault address 0x3fffff` (c29)
- `panic: can't call pointer on a non-pointer Value` inside `reflect`, or
  `<invalid reflect.Value>`, for the interface cases (c09, c10, c32)

### The mechanism

A local `string` (or interface, or `complex128`) variable's frame slot holds an
*8-byte pointer* to a 16-byte header. `goc/compile.go`'s `storeAssignmentTarget`,
for a destination classified `assignmentTargetVariable` with `local` true:

```go
if target.local && isDescriptorValue(target.valueType) {
        value = g.copyInlineValue(value, target.valueType)   // fresh localAllocTyped in THIS frame
}
...
g.store(value, target.slot, target.valueType)                // rebinds the slot to it
```

`copyInlineValue` calls `localAllocTyped`, an alloca **in the function doing the
assigning**. The slot, however, belongs to whichever frame declared the variable.
When the assigning function is a closure that captured the variable by reference —
`funcLit` stores `g.vars[capture]`, the slot address, into the closure descriptor,
and the child reads it back as `child.vars[capture]` — the store publishes a
pointer to the *closure's* frame into the *parent's* slot. It dangles the moment
the closure returns.

`complex128` is worse: it is `ir.ClsP`-classed and 16 bytes, but
`isDescriptorValue` is false for it, so it does not even get the `copyInlineValue`
step; the slot is rebound to whatever address the right-hand side produced.

The escaping-closure path is unaffected because `variableStorage`'s heap-lift arm
sets `g.directValues[object] = true` for strings and interfaces, which makes the
storage the header itself and routes the assignment through `storeInlineValue`.
**The fix is to give the same representation to a variable captured by reference by
a non-escaping literal**, which costs no allocation: the header lives in the
declaring frame instead of behind a pointer.

## 3. A neighbouring defect found while measuring: `complex128` in memory

`complex128` shares the affected representation, so it came along with the
measurement. The *variable* half of it is the same defect and is fixed below
(c54, c56). The *memory* half is a different defect, is **not fixed**, and is
recorded here with its reducers.

**A `complex128` written through an address destination stores the address of a
frame allocation into the destination, not the value.** `h.c = complex(3.0, 4.0)`
with `h` a `*holder`:

```
%t3 =p alloc8 16
stored d_3, %t3
%t4 =p add %t3, 8
stored d_4, %t4
call $goc_storep(p %t2, p %t3)      <-- the field gets %t3, the frame address
```

`storeAssignmentTarget`'s `assignmentTargetAddress` arm stores inline only when
`isInlineAggregate(valueType) || isInterfaceValue(valueType)`, and `complex128`
is neither, so it falls to `g.store`, which is a one-word store for an
`ir.ClsP`-classed type. Reads are consistent with it — `g.load` returns the word
and `real`/`imag` dereference it — which is why it looks correct until the frame
that produced the value dies. Three reducers:

| program | host | goc |
| --- | --- | --- |
| `*p = complex(3, 4)` through a `*complex128` (c59) | `3 4` | `3.8004551335784e-310 3.8004551335772e-310` |
| write `h.c` in one function, read it in another (d01) | `3 4 7` | `3.499451e-317 5.29561993437e-310 7` |
| return a struct holding a `complex128` by value (d07) | `{(3+4i) 7}` | `{(0+0i) 7}` |

It also hands `goc_storep` a goroutine-stack address as the value being
published into a heap object, which is the §5.8 invariant reached by a fourth
route.

**Why it is not fixed here.** Widening the store arm alone is wrong and was
measured to be wrong: doing it turned d02 (`fmt.Println(h)` on a struct with a
`complex128` field, which reads the real sixteen bytes through reflect) and c58
from passing to faulting, because the *read* side still loads one word. Making
`complex128` a value carried as an address consistently means putting it into
`isInlineAggregate`, which is consulted in dozens of places across the frontend
— a change with its own blast radius and its own validation cycle, of exactly
the shape RUNTIME_PLAN §5.14 records going wrong when bundled. The narrow rule
this branch does adopt (`assignmentTargetStoresInline`) deliberately leaves the
address path exactly as it was, so nothing here regresses and nothing here
papers over it.

## 4. The fix

`variableStorage` already had the right representation for this — it is what the
*escaping*-capture arm uses. The fix extends it to non-escaping captures, where
it costs nothing at all: no heap cell, the value simply lives in the declaring
frame's slot instead of behind a pointer to another sixteen bytes.

- **`findReferenceCaptures`** (new, `goc/compile.go`) returns the local variables
  a nested function body refers to. A nested body is a function literal *or* the
  body of a `range` over a function, which is lowered into a yield function; both
  reach the variable through the closure environment, which carries the address
  of its slot. `findEscapingCaptures` answers the narrower question — which of
  those must be heap-lifted because the nested function can outlive the frame —
  and both now share a `bodyLocals` helper so they agree on what a local is.

- **`variableStorage`** gives such a variable direct storage when its type would
  otherwise be a pointer to a separate value: `localAllocTyped`, zeroed, with
  `directValues[object] = true`. `localAllocTyped` marks the value's pointer
  words in the frame's stack map, so the string's data word and the interface's
  two words stay GC roots, which the old header alloca also was.

- **`isIndirectVariableValue`** names the type set: not a struct or array
  (`allocLocal` already gives those stable backing), not a slice (stored inline
  under `runtimeAllocation`), leaving string, interface and `complex128`.

- **`isInlineValue`** = `isInlineAggregate || isInterfaceValue || isComplex128Type`
  is the "carried as an address" predicate the direct-value paths now use, and
  `isAddressRepresentedInterfacePayload` is expressed in terms of it.

- **`assignmentTarget.directVariable`** marks a destination whose own storage is
  its value, and `assignmentTargetStoresInline` is what the read (`+=`) and the
  store consult. It is deliberately *not* the same question as `target.inline`:
  an address destination of type `complex128` keeps the word store it has always
  had, because `complex128` is only carried as an address as a *value*, not in
  memory. Widening that arm broke `c58` (a `complex128` struct field), which is
  how the distinction was found; the narrower rule leaves `c58` passing.

- The escaping-closure descriptor's snapshot arm (`cell == ir.R`) now copies a
  direct value's bytes for any of the three types rather than only for an
  interface. Reaching it needs a direct value that was never heap-lifted, which
  no reducer produced; it is generalised because loading through the slot first,
  as the aggregate arm below it does, would read the value's first word as the
  address of the value.

### Result

**69 of the 70 differential programs now match the host**, up from 43. The one
that still differs is `c59`, `*p = complex(3, 4)` through a `*complex128`, which
is the pre-existing defect in section 3 and differed before this change too.

Every affected case in section 2's tables is fixed, including all six symptom
shapes, both range-over-function forms, and the `complex128` cases from section
3's second bullet.

## 5. What was landed with it

- **`closure-capture/assigned-string`** and
  **`closure-capture/assigned-header-values`**, two new capabilities
  (`goc/testdata/runtime_closure_captured_{string,header_values}.go`). Every case
  calls a recursive `clobber` between the closure's write and the read, because
  two reducers found while measuring passed *without* that and failed *with* it.
  Both carry their controls: read-only capture, escaping closure, returned
  closure, captured slice, captured struct field, captured slice element.

- **`goc/closurecapture_test.go`**, four mechanism tests over the three affected
  types: the closure does not store one of its own frame allocations through the
  captured pointer; the enclosing function allocates the value's full width for
  it; a non-escaping capture does not start calling `runtime.newobject`
  (§5.9's cost model); and the range-over-function yield function obeys the first
  rule with no function literal in the source.

- **§5.13's rewritten range-over-function cases are restored.**
  `goc/testdata/runtime_range_target_forms.go`'s `iteratorTargets` accumulated
  into a slice while every other subject in the file accumulated into a string;
  §5.10 records that as a workaround for this defect. All four cases now use
  `observed += ...` like the rest of the file, which makes them captured-variable
  cases in their own right.

## 6. Suites

Every one of these was re-run at the tip commit `7182ddf`, after the plan and
report edits, not only at the commit that introduced the fix.

| suite | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `gofmt -l goc/ cmd/goc/` | clean |
| `make test-unit` | ok, every package |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 567.755s` |
| `make test-goc-cmd` | `ok github.com/evanphx/cg12/cmd/goc 229.315s` |
| capability matrix, default arm | **347 subtests, 346 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP** |
| capability matrix, `-runtime-opt` arm | **347 subtests, 346 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP** |

**The complete list of non-passing capabilities, both arms: none.** The only
non-`PASS` verdict in either arm is `defer-panic/panic-string-output`, the
declared `expectedFailure`. `--- FAIL:` and `--- SKIP:` appear zero times in
either arm.

347 rather than 345 because this branch adds two capabilities. The census is from
the per-capability `PASS` / `EXPECTED FAILURE` / `KNOWN GAP` log lines and a
count of `--- PASS:`/`--- FAIL:`/`--- SKIP:` subtest lines, not from `ok`. Four
shards per arm, `-runtime-status-shards=4 -runtime-status-compile-workers=16`,
47-54s per shard.

## 7. The reducers fail on the compiler they describe

A reducer that has only ever reproduced once is a claim (RUNTIME_PLAN §15). Each
one was re-run against a compiler built at **this same path** from `HEAD~2`'s
`goc/compile.go` — the tree's own commit reverted, not a worktree, so the
path-dependence §23 records does not enter.

| program | base compiler | this branch |
| --- | --- | --- |
| `runtime_closure_captured_string.go` | `SIGSEGV`, nil pointer dereference | `closure captured string ok` |
| `runtime_closure_captured_header_values.go` | `unexpected fault address 0x6e6f2d64616572` | `closure captured header values ok` |
| `runtime_range_target_forms.go` (restored) | `SIGSEGV`, nil pointer dereference | passes |

`0x6e6f2d64616572` is `"read-non"` little-endian: the fault address is the
program's own text, read out of the frame the closure abandoned.

**The first two mechanism unit tests were wrong and had to be rewritten.** As
first written they passed against the base compiler, which makes them worthless.
Two reasons, both worth recording:

- A pointer store lowers to a **`goc_storep` call**, not an `ir` store
  instruction, whenever the destination is not a known stack address — and a
  captured pointer never is. A check that looked only at `Op.IsStore()` could not
  see the store the defect consists of.
- "the enclosing function allocates 16 bytes somewhere" was true on both
  compilers. What separates them is the width of the allocation whose address
  goes *into the closure environment*: sixteen for the value, eight for a pointer
  to it.

Rewritten (`6878d1a`), all three defect tests fail on the base compiler across
all three types — 7 failing subtests — and pass here.
`TestNonEscapingCaptureStaysOffTheHeap` passes on both by design: it is a guard
on §5.9's cost model, not a defect test.

## 8. Determinism (§23) is not regressed

`analysis/determinism` over all 367 `goc/testdata` programs, 4 rounds each, 16
workers, in three configurations:

| configuration | result |
| --- | --- |
| no `-O` | `reproducible=367 varying=0 failed=0 of 367 over 4 rounds` |
| `-O` | `reproducible=367 varying=0 failed=0 of 367 over 4 rounds` |
| against a prebuilt pack (`goc build-runtime`) | `reproducible=367 varying=0 failed=0 of 367 over 4 rounds` |

`content varies between rounds: 0` and `image varies, content identical (layout
only): 0` in each. `scripts/determinism-check.sh`'s five-program sample agrees,
cold and warm, two rounds.

The fix cannot introduce nondeterminism by construction — `findReferenceCaptures`
returns a map that is only ever *looked up* in, never iterated — but §23's rule
is to measure rather than argue, so it is measured.

## 9. What is left open

- **`complex128` in memory** — section 3. Three reducers, an IR excerpt, and a
  measurement showing why the one-line widening is wrong. Not fixed; recorded in
  RUNTIME_PLAN §5.15 under "Residual".
- **A `slice` local under `!runtimeAllocation`** has the same indirect
  representation as a string and would have the same defect. It is excluded from
  `isIndirectVariableValue` deliberately: `!runtimeAllocation` is the `goc -c`
  path, which compiles to objects and never executes, so the change could not be
  validated. Stated rather than made silently.
- **`c++` on a `complex128`** goes through `IncDecStmt`'s word load and is
  nonsense on both compilers. Untouched and unmeasured beyond that reading.
- **§5.14's `span has no free objects` interaction** is untouched by this branch,
  which changes no escape decision: `findEscapingCaptures` is unmodified and
  `variableStorage`'s heap-lift arm still fires on exactly the same set.

---

# Independent verification run — `ccwork/escape-frame-publication` @ `ddd03eb`

Run in a detached worktree checked out at `ddd03eb` (`git worktree add ... ddd03eb`),
Linux/arm64, go1.26.1, 64 cores. Every suite below was launched, then blocked on in
the foreground until the process wrote its own exit code to the log. No number in
this section was inferred, extrapolated, or read from a still-running process.

Note on the box: a second ccwork job (`escape-alloc-census`) was resident on the
same machine for part of this run and itself compiles the corpus. Load average at
the start of suite (a) was 0.57 on 64 cores, so contention was low, but wall-clock
figures below are shared-machine figures and are not benchmark-grade.

## Suite (a) — `go test -timeout 40m -parallel 10 -v ./goc/...`

**PASS.** Exit code 0. `ok github.com/evanphx/cg12/goc 768.405s` (~12.8 min).
`-v` was added to the prescribed command line so the per-subtest census could be
taken; it changes what is reported, not what is run.

| | count |
| --- | --- |
| `=== RUN` | **601** |
| `--- PASS:` | **601** |
| `--- FAIL:` | **0** |
| `--- SKIP:` | **0** |
| packages `ok` / `FAIL` | 1 / 0 |

`./goc/...` resolves to the single package `github.com/evanphx/cg12/goc`, so this
is the whole suite, not a subset.

**The census reconciles with the stated 598 RUN / 597 PASS / 1 FAIL baseline
exactly, and the reconciliation was checked rather than assumed.** Comparing the
set of top-level `func Test*` names in `goc/*_test.go` at `eb9872e~1` against
`eb9872e` ("goc: reduced tests for the three publication shapes") shows that
commit adds exactly three, and no others anywhere on the branch add any:

- `TestValueBoxedIntoAnInterfaceEscapes` — PASS (1.68s)
- `TestCompositeLiteralAddressCarriesItsElementsToTheHeap` — PASS (1.64s)
- `TestParameterLeakingToAnInterfaceResultEscapes` — PASS
  (its sibling `TestParameterAssignedToACalleeNamedResultEscapes`, PASS 2.00s,
  predates `eb9872e`)

598 + 3 = 601, and the one prior failure is gone: `--- PASS: TestFrameEscapeAudit
(147.40s)`. So the suite did not get smaller and did not stop running anything —
it grew by precisely the three tests the branch added.

Against the merge-base `ad4e9b2` the branch adds nine top-level tests in total
(294 → 303 test functions) and removes none; the 598 baseline was evidently taken
mid-branch, after `TestFrameEscapeAudit` landed and before `eb9872e`.

Slowest subtests, for anyone judging whether this is timing-sensitive:
`TestFrameEscapeAudit` 147.40s, then `TestCapturedLoopVariableIsAllocatedInsideTheLoop`
11.63s and `TestUncapturedLoopVariableAllocatesNothing` 11.57s; everything else is
under 8s. Nothing here asserts on elapsed time, so the +5.8% `bigmod.Nat.Mul`
regression cannot have flipped a result in this suite — it can only have made the
768s longer.

## Suite (c1) — `make test-goc-status`

**PASS, 364/364.** Exit code 0. `ok github.com/evanphx/cg12/cmd/goc 107.793s`.
Run with `GOFLAGS=-v` so the per-capability set is visible; the make target's own
command line is unmodified (`go test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$'
./cmd/goc/... -args -runtime-status-shards=1 -runtime-status-shard=0`, one shard,
the whole matrix).

- `=== RUN` 365 = 1 parent + **364 capability subtests**
- `--- PASS:` 365, `--- FAIL:` 0, `--- SKIP:` 0
- 364 distinct capability names, 364 distinct in the PASS set — the empty FAIL set

**FAIL SET: empty.** This matches the known good state exactly, on count and on
membership. In particular `stack-scan/loop-safepoints` PASSes here (0.07s) — the
one known failure is an `-opt`-only failure, as documented.

On the 107s duration: this is not a truncated run. The subtests are individually
cheap (median well under 0.1s; the slowest are `stack-scan/stack-copy-roots` 6.21s,
`gc-stress/concurrent-mark` 4.23s, `gc-stress/memory-limit` 1.20s) and the matrix
runs against one shared prebuilt pack, so the wall clock is dominated by that build
rather than by 364 independent compiles. The count, not the clock, is the evidence:
all 364 named capabilities appear.

## Suite (c2) — `make test-goc-status-opt`

**FAIL, 363/364 — and the failure set is exactly the one known pre-existing entry.
It has not grown.** Exit code 2. `FAIL github.com/evanphx/cg12/cmd/goc 117.647s`.

- `=== RUN` 365 = 1 parent + **364 capability subtests**
- `--- PASS:` 363, `--- FAIL:` **1**, `--- SKIP:` 0

**FAIL SET (complete, one member):**

- `stack-scan/loop-safepoints`

The set of 364 capability names exercised under `-opt` is **byte-identical** to
the set exercised without `-opt` (`diff` of the two sorted name lists is empty),
so the `-opt` arm is not running a reduced matrix — same 364 capabilities, 363 of
them passing.

The failure text, for the record:

```
runtime_status_test.go:2664: runtime_stack_scan_loop_safepoints.go should pass: exit status 2
    ...
    collected while live: carried-0 at carried before rewrite
    panic: a stack slot live across a loop back edge was not a GC root
```

This is a stack-map/liveness hole under `-opt`: a slot live across a loop back
edge is missing from the GC root set, so the object is collected while still
reachable. It is a different mechanism from what this branch changes — the branch
moves allocations from frame to heap and does not touch stack-map emission or
`findEscapingCaptures` — and it is reported as reproducing on the merge-base
`ad4e9b2`. I did not independently re-run `ad4e9b2` to confirm that reproduction;
see the note at the end of this section for what I did check.

This capability is **not** timing-sensitive in a way the +5.8% `bigmod.Nat.Mul`
regression could touch: it fails deterministically in 0.02s with an assertion
about root-set membership, not a deadline.

## Suite (d) — `runtime_gc_type_mask_padding.go` reducer, 20× at `GOGC=10`

**PASS in the prescribed configuration: 0/20 failed.** Compiled with this tree's
`goc` (`go build -o goc ./cmd/goc`, then `goc -o reducer goc/testdata/runtime_gc_type_mask_padding.go`,
build exit 0, 2.9s). All twenty runs printed `type mask padding ok` and exited 0:

    === RESULT: 0/20 failed, 20/20 passed ===

That is the gate item, and it holds.

### An unprescribed variant does fail — reported, not swept up

The brief specified `GOGC=10`. The capability harness that owns this program
(`cmd/goc/runtime_status_test.go`, `gc-invariants/type-mask-padding`) runs it with
`env: []string{"GOMAXPROCS=3"}` at default `GOGC`. Because both are the program's
real configurations, I also ran **`GOGC=10 GOMAXPROCS=3`, 20×: 5/20 failed.**

The failure is not the type-mask assertion. It is a GC scan abort:

    runtime: pointer 0x394b6937420c to unused region of span
        span.base()=0x394b6928e000 span.limit=0x394b6928fef0 span.state=1
    runtime: found in object at *(0x394b69131c80+0x208)
    object=... s.spanclass=0 s.elemsize=8192 s.state=mSpanManual

`mSpanManual` with `elemsize=8192` is a goroutine stack, so this is the collector
finding a word inside a stack that points into an unallocated part of a span. Note
the bad pointer `0x...420c` is not 8-byte aligned — it is an interior/misaligned
value being scanned as a pointer.

**This is not the branch's regression.** See the merge-base comparison recorded
below; it reproduces identically on `ad4e9b2`.

### Merge-base comparison for the reducer

| build | `GOGC=10` (prescribed) | `GOGC=10 GOMAXPROCS=3` |
| --- | --- | --- |
| tip `ddd03eb` | **0/20 failed** | 5/20, then 4/20 failed |
| merge-base `ad4e9b2` | **20/20 failed** | 20/20 failed |

The reducer source is *added* by this branch (99 lines, absent at `ad4e9b2`), so
the merge-base column is this branch's reducer compiled by the merge-base `goc` —
the correct "before". It confirms the branch's claimed before/after: 20/20 → 0/20.

The merge-base column also shows why it cannot, by itself, tell us whether the
`GOMAXPROCS=3` failure is new: at `ad4e9b2` the original defect fires on every
run and masks everything behind it.

So I discriminated with a different program instead. Compiled by both compilers and
run 10× each at `GOGC=10 GOMAXPROCS=3`:

| program | tip `ddd03eb` | merge-base `ad4e9b2` |
| --- | --- | --- |
| `runtime_gc_concurrent_mark` | 0/10 | 0/10 |
| `runtime_gc_checkmark` | 0/10 | 0/10 |
| `runtime_gc_mark_workers` | **2/10 failed** | **4/10 failed** |

`runtime_gc_mark_workers` passes in the standard harness on both, and crashes under
`GOGC=10 GOMAXPROCS=3` on **both** — on the merge-base more often than on the tip
(there with a `SIGSEGV`, `addr=0x662d65646f82`, i.e. ASCII garbage being followed as
a pointer). **`GOGC=10` combined with `GOMAXPROCS=3` is therefore a configuration in
which this runtime's GC is already unsound on `main`, independent of this branch.**
The reducer's 4–5/20 there is that pre-existing instability, not a regression this
branch introduces, and the prescribed gate (`GOGC=10`, 20 runs) is clean at 0/20.

I did not root-cause the underlying GC/stack-scan instability; that is out of scope
for a verification job and it is not on this branch's ledger. It is worth a bug of
its own.

## Suite (e) — `make test-unit`

**PASS. 1556 passing, 0 failing — exactly the stated baseline.** Exit code 0.

| | count |
| --- | --- |
| `--- PASS:` | **1556** |
| `--- FAIL:` | **0** |
| `--- SKIP:` | 339 |
| packages `ok` | 24 |
| packages with no test files | 12 |
| packages in `UNIT_PKGS` | **36** (= 24 + 12, fully accounted for) |

The wall clock is ~10s, which is short enough to deserve the lie-detector test, so:
**`(cached)` appears on 0 of the 36 packages** — every test binary was built and run
in this invocation, nothing was served from the Go test cache. The suite is fast
because these are pure unit tests (slowest package `arm64` at 7.45s, next `link` at
2.01s) running 36-way in parallel on 64 cores, with the module's build cache already
warm from suites (a) and (c). 1556 + 339 = 1895 subtests actually reported.

The 339 skips are the pre-existing host-architecture skips (amd64 encode/exec tests
on an arm64 box, e.g. `TestAtomicExec*`, `TestObjClz*`), not new suppression: the
passing count matches the 1556 baseline exactly, so nothing moved from PASS to SKIP.

## Merge-base control run — `make test-goc-status-opt` on `ad4e9b2`

Because suite (c2) is the only red result, I ran the same target on the merge-base
rather than inheriting the claim that its one failure is pre-existing.

**`FAIL github.com/evanphx/cg12/cmd/goc 119.445s`, exit 2. 362 PASS / 1 FAIL.**

**Merge-base FAIL SET: `stack-scan/loop-safepoints` — identical, one member.**

The capability matrix delta between the two revisions is exactly one addition and
zero removals:

- only on tip `ddd03eb`: `gc-invariants/type-mask-padding` (the capability this
  branch adds, and which passes in both the `-opt` and non-`-opt` arms)
- only on merge-base: *nothing*

So 363 capabilities at `ad4e9b2` → 364 at `ddd03eb`; the failure set is the same
single pre-existing entry on both. **It has not grown, and it is measured here,
not assumed.**

## Verdict

| suite | result | evidence |
| --- | --- | --- |
| (a) `go test -timeout 40m -parallel 10 ./goc/...` | **PASS** | 601 RUN / 601 PASS / 0 FAIL / 0 SKIP, exit 0, 768.4s; = 598 baseline + the 3 tests `eb9872e` adds |
| (c1) `make test-goc-status` | **PASS** | 364/364, FAIL set empty, exit 0, 107.8s |
| (c2) `make test-goc-status-opt` | **FAIL, unchanged** | 363/364; FAIL set = {`stack-scan/loop-safepoints`}; byte-identical FAIL set on merge-base `ad4e9b2` |
| (d) reducer 20× at `GOGC=10` | **PASS** | 0/20 failed (merge-base: 20/20 failed) |
| (e) `make test-unit` | **PASS** | 1556 PASS / 0 FAIL / 339 SKIP, 0 cached, 36/36 packages, exit 0 |

All five runs were watched to completion in the foreground; every number above was
read out of a log whose process had already written its own exit code. Nothing was
left running and nothing is extrapolated.

Two things a merger should carry forward, neither of them blocking:

1. **The +5.8% `bigmod.Nat.Mul` regression is not covered by any suite here.** None
   of these tests assert on elapsed time — (c) and (d)'s failures are deterministic
   assertions about GC root sets and type masks, not deadlines — so a clean run is
   evidence about correctness only. The regression stands or falls on its own merits.
2. **`GOGC=10` together with `GOMAXPROCS=3` crashes the GC on `main` already**
   (`runtime_gc_mark_workers`: 4/10 on `ad4e9b2`, 2/10 on `ddd03eb`). Pre-existing,
   out of this branch's scope, and worth its own bug.

SAFE TO MERGE TO MAIN
