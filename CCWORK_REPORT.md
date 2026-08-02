# Loop-body allocation aliasing: a live miscompile in goc

Branch `ccwork/loop-aliasing-fix`, off `main` (`efcd4d4`). The previous job's
report (cross-function escape summaries) is at `efcd4d4:CCWORK_REPORT.md`.

Status: IN PROGRESS. Numbers land here as they are produced. Anything not
watched to completion is marked UNVERIFIED.

## 0. The defect, reproduced on main before anything was changed

Host toolchain is `go1.26.1 linux/arm64`; goc built from `efcd4d4`, run as
`goc -run`.

| program | form | host `go run` | `goc -run` on main |
|---|---|---|---|
| `loop_alias_forms.go` | `new(int)` | `new:   1 2` | `new:   1 2` |
| `loop_alias_forms.go` | `make([]int, 0, 4)` | `make:  1 2` | `make:  1 2` |
| `loop_alias_forms.go` | `var a [2]int; &a` | `array: 1 2` | **`array: 2 2`** |
| `loop_alias_composite.go` | `&cell{v: i}` | `alternate: 1 2` + `distinct` | **`alternate: 2 2`** + **`ALIASED: the two iterations share one allocation`** |
| `variadic_backing.go` | `retainNothing(&x)` | `1` | `1` |

Confirmed exactly as the brief states: two forms are already fixed by the loop
rule inside `opt.LowerHeapAllocations`, two are live. `variadic_backing.go`
already agrees with the host; it lands as a regression guard, not as a failing
case.

## 1. The three programs, landed as failing corpus tests (commit `f343e38`)

`goc/loopalias_test.go` compiles each program, links it against the
cg12-compiled Go runtime, runs it and compares everything it printed against
what Go prints. Each program is run twice, unoptimized and optimized, because
the placement is decided in two different places and a fix in one leaves the
other untouched.

    $ go test ./goc -run TestLoopBodyAllocationsAreDistinctPerIteration -count=1
    --- FAIL: .../loop_alias_forms.go        got "array: 2 2", want "array: 1 2"
    --- FAIL: .../loop_alias_forms.go_-O     got "array: 2 2", want "array: 1 2"
    --- FAIL: .../loop_alias_composite.go    got "alternate: 2 2\nALIASED: ..."
    --- FAIL: .../loop_alias_composite.go_-O got "alternate: 2 2\nALIASED: ..."
    --- PASS: .../variadic_backing.go
    --- PASS: .../variadic_backing.go_-O

`TestLoopAliasExpectationsMatchTheHostToolchain` passes: the expectations are
`go run`'s own output, not a belief about it.

## 2. What the three new corpus programs cost the baselines, before any fix

The programs are in `goc/testdata/`, which the corpus audits glob, so they are
also 3 new programs in the census. Regenerated on the **unfixed** compiler so
that the fix's own diff is attributable:

    $ go test ./goc -run 'TestAllocationCensus$|TestEscapeShadowPlacement$|TestFrameEscapeAudit$' \
        -update-alloc-census-baseline -update-escape-shadow-baseline -update-frame-escape-baseline
    ok  166.549s, 388 programs

| baseline | delta |
|---|---|
| `alloc_census_baseline.txt` | **+5 lines**, all in the three new files, all `heap` |
| `escape_shadow_baseline.txt` | unchanged |
| `frame_escape_baseline.txt` | **unchanged** — the new programs publish no frame address |

The five lines are worth reading, because of what is *not* there:

    loop_alias_forms.go:7:8    viaNew     newobject int    heap   <- new(int),  loop rule
    loop_alias_forms.go:22:23  viaMake    newobject 4_int  heap   <- make(...), loop rule
    variadic_backing.go:9:9    (x3)       newobject 1_any  heap

The two forms that are already correct appear as `heap`, which is the loop rule
in `opt.LowerHeapAllocations` doing its job. The two **broken** forms appear
nowhere: `var a [2]int` and `&cell{v: i}` are committed frame placements, and
the census does not record ordinary front-end frame slots. The instrument is
blind to the defect from the frame side; it will only see the fix from the heap
side.

## 3. The fix: approach (a), with the per-iteration question asked by the walk
   that is already there

**Approach (b) was rejected on evidence, not taste.** (b) is "make `allocLocal`
and `nonEscapingAddress` emit IR candidates so the loop rule in
`LowerHeapAllocations` covers them". The loop rule is
`promotionsBlockedByALoop`, and it is blunt by design: *any* promotable
candidate whose defining instruction sits in a natural loop is sent to the
allocator, with no test of whether anything retains it. That is affordable
today because candidates are rare — 441 blocked promotions over the whole
corpus. It is not affordable at these two sites. From the spike's own census:

| decision site | committed frame placements, corpus-wide |
|---|---|
| `&CompositeLit` (`nonEscapingAddress`) | 2 062 |
| local variable (`allocLocal`) | **4 685 295** |

`allocLocal` is *the* path for every array and struct local in the runtime and
the standard library. Handing those to a rule that heaps everything in a loop
would put `var b [64]byte` on the heap on every trip round every loop that
declares a scratch buffer, whether or not its address goes anywhere — a large,
silent performance regression, and exactly the over-correction step 3 warns
about. Making the rule precise enough to avoid that means rewriting the rule
the 441 existing fires depend on.

So the fix is (a): ask the per-iteration question at the two committing sites.
What makes it small is that **the question does not need a new analysis**.

`objectDoesNotEscape` already refuses to answer for an object whose uses it
cannot all see — `escapeWalkSeesEveryUse` demands the object be declared inside
the body being walked. Run the *existing* walk with the **loop body** as the
scope instead of the function body, and it answers a different question with no
new code: an address is fine if it reaches only things the loop itself
declares, and outlives the iteration the moment it reaches anything further
out. The scope has to be tightened at both ends — `escapeWalkOuterObjects`
lists the function's parameters and results, which are storage that outlives
the iteration — so the iteration walk trusts nothing but the loop's own locals.

    goc/compile.go   findIterationCaptures      the variable site (allocLocal)
                     addressOutlivesItsIteration the &T{...} site
                     enclosingLoopBody / loopBodiesWithin

    +160 lines, 120 of them comment; 6 lines changed at the call sites.

It inherits the walk's interprocedural parameter summaries, which is why it
does not over-correct: `var b [64]byte; n, _ := r.Read(b[:])` in a loop reaches
a parameter the callee does not let escape, so it stays in its frame slot,
which is where Go puts it too. Only an address that reaches storage declared
further out moves — the same comparison gc makes with loop depths.

Nested loops need no special case: widening the scope can only make the walk
trust *more* objects and so report *fewer* captures, so the innermost scope
gives the strongest answer and the union over a function's loops is exactly
that answer.

### 3.1 All four forms now match the host toolchain

    $ go test ./goc -run 'TestLoopBodyAllocationsAreDistinctPerIteration|TestLoopAliasExpectationsMatchTheHostToolchain'
    ok      github.com/evanphx/cg12/goc     17.153s

All six subtests pass — the two that were already right stay right, so the
existing loop rule was not disturbed.

| form | host | goc before | goc after |
|---|---|---|---|
| `new(int)` | `1 2` | `1 2` | `1 2` |
| `make([]int, 0, 4)` | `1 2` | `1 2` | `1 2` |
| `var a [2]int; &a` | `1 2` | **`2 2`** | **`1 2`** |
| `&cell{v: i}` | `1 2` | **`2 2`** | **`1 2`** |
| `variadic_backing` | `1` | `1` | `1` |

### 3.2 It costs no measurable compile time

Full executable build of `stdlib_crypto_ecdsa.go` (the program the spike
measured the AST walk on), three runs each:

    main   11.99  11.98  12.06     mean 12.01 s
    fixed  11.90  12.16  12.09     mean 12.05 s

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
    --- PASS: TestFrameEscapeAudit (0.00s)
    ok  175.531s

    $ go test ./goc -run 'TestLoopBodyAllocationsAreDistinctPerIteration|TestLoopAliasExpectationsMatchTheHostToolchain'
    ok  21.433s   (8 + 4 subtests, all pass)

    $ go test ./goc -run TestCompilingTheSameSourceTwiceGivesTheSameModule
    ok  4.657s

Per the brief, `go test ./goc/...`, the capability matrix and `make test-unit`
were **not** run here; a dependent verification job does that.

## 9. What this does not cover

Stated so nobody reads the four green rows as more than they are.

1. **Loops are recognised syntactically** -- the bodies of `for` and `range`
   statements. A loop built out of a backward `goto` is not one of those, and
   the AST rule will not see it. The IR loop rule still covers any
   `OHeapAlloc` candidate inside it.
2. **Only the loop body.** Storage committed in a `for` clause's init runs once
   and is right as it is; its condition and post statement are between
   iterations rather than inside one and are not asked.
3. **`//go:nowritebarrier` functions keep the defect at the `&T{...}` site.**
   `heap = false` is forced there, before and after this change, because those
   functions must not allocate. The variable site has no such gate -- the
   escaping-capture arm it shares never had one -- so it *can* now heap-lift in
   such a function. Across 389 programs it never did: the census gained no site
   outside the two reduction programs.
4. **Freestanding builds are unaffected**, like the rest of the escape
   machinery, which is gated on `runtimeAllocation`.
5. **The rule is as precise as the walk it reuses.** Where the walk cannot
   prove containment it answers "escapes", which under a loop-body scope means
   "outlives the iteration" and sends the object to the heap. That is the safe
   direction, and §4 and §7 are what it costs: two allocations.

---

## Result

**Approach taken: (a)** -- the per-iteration question is asked at the two
committing sites in the AST front end. It was chosen over (b) on the spike's
own census: (b) hands those sites to `promotionsBlockedByALoop`, which heaps
*every* promotable candidate in a loop with no test of whether anything retains
it, and `allocLocal` is 4 685 295 committed frame placements corpus-wide --
every array and struct local in the runtime and standard library. That would
have heaped every scratch buffer declared in every loop. Making the rule
precise enough to avoid it means rewriting the rule the existing 441 fires
depend on. (a) needed no new analysis: `objectDoesNotEscape` already refuses to
answer for an object whose uses it cannot all see, so running the existing walk
with the loop body as its scope asks the per-iteration question and inherits
the walk's interprocedural parameter summaries. The brief's one point for (b)
-- census visibility -- is answered anyway: what the rule moves becomes a heap
allocation, and heap allocations are exactly what the census records.

**All four forms now match the host toolchain**, at `-O0` and `-O`, and so do
two further shapes that were also broken on main and that nobody had reduced (a
struct local reached through a pointer method, and the same inside a `range`
loop).

**Census delta: 2 allocations, both in the reduction programs.** Zero sites
moved frame->heap or heap->frame anywhere in the standard library or runtime
across 389 programs; the promotion count is identical to the pre-fix tree
(17 005), so the existing loop rule was not disturbed -- it gains exactly one
fire, 447 -> 448. Compile time is unchanged within the run-to-run spread
(12.01 s -> 12.05 s on `stdlib_crypto_ecdsa.go`).

**`TestFrameEscapeAudit` is clean**: zero new frame-address publications, and
none of the 209 already listed went away.
