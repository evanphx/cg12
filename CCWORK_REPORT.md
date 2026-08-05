# The mem2reg GC-visibility blocker: not in flate, and fixed

Branch: `ccwork/mem2reg-gc-visibility`, off `integration/wave8` (`7983abd`).

**Verdict up front.**

* **It does not reproduce in `flate`.** 0 crashes in 750 runs with promotion on,
  across three collector settings. That crash was the zero-capacity-slice defect
  the wave-8 `flate-gc-crash` job fixed, amplified by promotion; the tree it was
  measured on did not contain that fix.
* **The hypothesis it was handed to me under is refuted.** Spilled promoted temps
  *are* recorded in the safepoint map, that is the ordinary path, and promotion
  puts 23 % *fewer* values in registers at safepoints, not more. Counted on a real
  whole-program build, not argued.
* **A real promotion-caused collector defect does exist, in `p256`,** and running
  the corpus with promotion on at a raised collection rate is what found it:
  ECDSA verification silently fails on **35 runs in 40 at `GOGC=10`**, 0 with the
  collector off, 0 with promotion off. Root-caused to one local in one function,
  fixed in `opt/mem2reg.go`, with a deterministic reducer that fails 60/60 before
  the fix and 0/60 after.
* **The switch's other blocker, `tcp-churn`, is fixed by the same change** — 18/20
  failures before, 0 in 80 after — though *why* is only partly established; see
  section 8.
* **After: 0 failures in 500 runs of `p256`, 750 of `flate`, 960 of the whole
  benchmark corpus, and 368/368 capabilities with the switch on.** Every guard is
  green with the switch off, and goc's output with the switch off is byte-identical
  to the pre-fix compiler's.

Everything below is measured on this box, this branch.

## 1. Current crash rate, promotion on

Binary: `goc -O -o flate goc/testdata/placement_bench/flate/main.go`, built with
`GOC_BOUNDED_MEM2REG=1` from `7983abd`. Every run is a fresh process; a run
counts as a crash if it exits non-zero.

| build | collector | runs | crashes | rate |
|---|---|---|---|---|
| `GOC_BOUNDED_MEM2REG=1` | default (`GOGC=100`) | 250 | **0** | 0 % |
| `GOC_BOUNDED_MEM2REG=1` | `GOGC=off` | 250 | **0** | 0 % |
| `GOC_BOUNDED_MEM2REG=1` | `GOGC=10` (more collections) | 250 | **0** | 0 % |
| switch off (default compiler) | default | 250 | **0** | 0 % |

1000 runs total, no crash of any kind. `GOGC=10` is in the table because it is
the arm that would *raise* the rate of a missing-root defect: it collects far
more often, so a root the collector cannot see gets freed under a live reference
much sooner. It found nothing either.

## 2. The harness is not the reason it is quiet

A zero measured on a defect someone else measured at 3/5 is worth nothing until
the instrument is shown to be able to see it. So the same harness was pointed at
the tree the 3/5 was measured on.

`cee7e56` ("opt: the bounded pipeline can promote memory to registers, behind a
switch") — the commit whose report records the 3/5 — sits on top of `d2855f5`.
The `flate` zero-capacity fix is `800f47f` ("goc: a slice expression with nothing
left must not point past its source"), which is **not** an ancestor of `cee7e56`:

    $ git merge-base --is-ancestor 800f47f cee7e56 ; echo $?
    1

So the 3/5 was measured on a tree with the zero-capacity defect live. Building
that tree's compiler and running the same harness:

| tree | promotion | runs | crashes | rate |
|---|---|---|---|---|
| `cee7e56` (pre zero-cap fix) | **on** | 100 | 11 | **11 %** |
| `cee7e56` (pre zero-cap fix) | off | 100 | 6 | **6 %** |
| `7983abd` (this branch) | **on** | 250 | 0 | 0 % |
| `7983abd` (this branch) | off | 250 | 0 | 0 % |

The instrument reproduces the crash when the defect is present, and the crash is
present *with promotion off too*, at the ~7.5 % rate the `flate-gc-crash` job
already recorded for the zero-capacity defect. Promotion roughly doubled that
rate (11 % against 6 %); it did not create a second defect.

The failure signature on the pre-fix tree is the same one the brief quotes, and
it is the zero-capacity signature, not a missing-root one:

    runtime: pointer 0x... to unallocated span span.base()=0x... span.limit=0x... span.state=0
    fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)
    runtime_badPointer() / runtime_findObject() / runtime_wbBufFlush1()

9 of the 11 pre-fix crashes are `to unallocated span` — a pointer that is not in
any span at all, which is what a one-past-the-end slice base looks like — and 2
are `to unused region of span`. A root the collector never scanned does not
produce either message: it produces a *use-after-free*, not a bad pointer handed
*to* the collector. The message was evidence against the missing-root hypothesis
from the start.

Why `GOGC=off` was 0/5 in the original measurement: the zero-capacity defect is
also a collector-visibility defect — the collector is handed a pointer it
rejects, and the buffer is freed under a live slice. Turning the collector off
suppresses it exactly as it suppresses a missing root. That arm did not
discriminate between the two hypotheses.

## 3. The hypothesis, tested directly and refuted

The brief's hypothesis: *"if mem2reg promotes an alloca holding a pointer into an
SSA temp, and the register allocator later spills that temp to a stack slot, the
slot must appear in the safepoint map as holding a pointer. If it does not, the
collector will not see the reference."*

A zero crash rate does not refute that on its own — a latent missing record could
simply be going unexercised. So the recording was inspected, and then counted on
a real whole-program build.

### What the backend actually does

There is no separate path for a promoted temp. Every safepoint root goes through
one function, `arm64/mc.go:2826 recordSafepoint`, which reads the root's binding
straight off the temporary:

```go
if t.Reg != ir.NoReg {
        locs = append(locs, rootLoc{kind: rootReg, val: int32(mreg(Reg(t.Reg))), typ: t.GCType})
} else {
        locs = append(locs, rootLoc{kind: rootFrame, val: int32(m.spillBase + t.Slot), typ: t.GCType})
}
```

A spilled root *is* recorded, at `spillBase + Slot`, and that is the ordinary
case rather than an exception. Two things upstream make it so:

* `arm64/gcalloc.go:339` — the graph colourer force-spills a managed reference
  before colouring begins: `if g.gc[t] || ((f.UsesManagedFrame() || …) && g.crossFreq[t] > 0) { spill(t); removed[t] = true }`. On a goc frame (every goc
  function sets `ManagedFrame`), *anything* live across a call goes to a slot.
  So a GC ref is not given a register and spilled later; it never gets one.
* `arm64/gcalloc.go:49 coalesceSpillSlots` — spill slots are shared between
  temporaries with disjoint live ranges to shrink the frame, and GC references
  are explicitly excluded from that sharing and keep private slots, "their
  whole-life stack home is part of the safepoint stack map".

The membership question — which temporaries are roots at all — is
`arm64/regalloc.go:399 isSafepointRoot`, and on a managed frame it is *wider*
than `GCRef`: it reports **any** live pointer-class (`ClsP`) temporary, because a
copying stack has to relocate interior frame addresses too. goc types pointers
`ClsP` and never calls `ir.LowerPointers`, so that clause is live for goc code.

### And what it does on a real build, counted

`arm64` and `opt` were instrumented (env-gated, since reverted) to count, over a
whole-program `goc -O` build of `goc/testdata/placement_bench/flate/main.go` —
the program in question, with its full stdlib closure:

| | promotion off | promotion **on** |
|---|---|---|
| safepoint roots recorded in a **register** (`rootReg`) | 23,986 | **18,368** |
| … of those, carrying `GCRef` | 23,986 | 18,368 |
| safepoint roots recorded in a **frame slot** (`rootFrame`) | 1,640,094 | 1,499,918 |

Promotion does not create register roots — it *removes* 23 % of them, and every
one that remains is `GCRef` and is emitted into the safepoint map as a `rootReg`
entry. There is no population of "promoted managed pointers living in a register
that the collector was not told about": the register roots that exist are
frontend-fixed temporaries (the closure-context register and the like), they
exist identically with the switch off, and they are recorded.

The complementary question — does promotion turn a described slot into an
undescribed value? — was counted the same way. Before promotion a managed local's
slot word is described by `ir.InferStackPointerWords`, whose predicate is "the
stored value is `GCRef`, or is itself an allocation address"
(`ir/goabi.go:97`). After promotion the value itself must satisfy
`isSafepointRoot`. Counting every reaching definition of every promoted managed
variable that satisfies **neither**:

| program | promoted managed variables | their reaching definitions | definitions no rule would report |
|---|---|---|---|
| `flate` | 7,491 | 8,217 | **1** |
| `json` | 8,080 | 8,985 | **1** |
| `gcpress` | 7,250 | 7,959 | **1** |

The one, in all three, is the same value:

    AUDIT promote-unrooted func=reflect.Value.UnsafePointer var=t49 value=t49 cls=l gcref=false

`reflect.Value.UnsafePointer` returns an `unsafe.Pointer` that goc types `ClsL`,
a plain word, and does not mark `GCRef`. It fails the post-promotion predicate —
and it fails the pre-promotion one identically (`InferStackPointerWords` requires
`GCRef` or an allocation address, and it is neither), so the slot word it was
stored into was not described before promotion either. Promotion loses nothing
there. It is also the whole population: **one value in ~8,000**, in a function
none of these programs call.

So the loss the brief hypothesised does not exist in this backend, in either
direction: promoted managed pointers live in frame slots by construction, those
slots are recorded, and there is no reaching definition of a promoted managed
variable that promotion drops from the map.

## 4. What the crash actually was

The `flate` crash the switch was blocked on was **the zero-capacity slice defect
that `800f47f` fixed**, running at roughly double its usual rate because
promotion changes the program's allocation and timing. Three things say so:

1. It reproduces on the pre-fix tree **with promotion off** (6/100), which a
   promotion-introduced defect cannot do.
2. Its message is `found bad pointer in Go heap` from `badPointer` via
   `wbBufFlush1`/`findObject` — the collector *rejecting* a pointer handed to it.
   A root the collector cannot see produces the opposite symptom: a silent free
   and a later use of freed memory, with no complaint at collection time.
3. It is gone on the fixed tree at three collector settings over 750 runs,
   including `GOGC=10`, which collects far more often than the setting the 3/5
   was measured at and would raise, not lower, a missing-root rate.

The `GOGC=off` arm in the original measurement did not discriminate: the
zero-capacity defect is *also* only visible when the collector runs.


## 5. It does still reproduce — in `p256`, not in `flate`

Running the whole benchmark corpus with promotion on, 40 runs of each program at
the default collector setting and 40 at `GOGC=10`, found one program that dies:
`goc/testdata/placement_bench/p256/main.go`, an ECDSA P-256 sign/verify
workload. It does not crash — it **silently computes the wrong answer**:

    panic: signature did not verify

The full control matrix, 40 runs per cell, same box, same source:

| promotion | `GOGC` | failures / 40 |
|---|---|---|
| off | `off` … `10` | **0** at every setting |
| on | `off` | 0 |
| on | 100 (default) | 0 |
| on | 50 | 4 |
| on | 20 | 24 |
| on | **10** | **35** |

Monotone in how often the collector runs, zero when it never runs, and zero at
every setting with promotion off. That is the shape the brief was looking for,
and it is a much cleaner instrument than `flate` ever was: `flate`'s crash was
never promotion-specific, and this one is, at every collection frequency.

The symptom is the one a missing root produces. A root the collector cannot see
is not reported as a bad pointer — the object is freed while a live reference
still names it, the span is reused, and the program reads whatever is there now.
A P-256 scalar or field element read after its backing array was recycled gives a
signature that does not verify, with no diagnostic at all.

The rest of this section is the root cause.

### Bisection: one function, one local

Promotion was scoped by function name (an FNV hash of the name into 1024 buckets,
binary-searched) and then by variable index inside the function. Both searches
converged, which is what says the cause is one thing and not an interaction:

| scope | failures / 20 at `GOGC=10` |
|---|---|
| everything | 15–35 / 40 |
| buckets `[0,511]` | 0 |
| buckets `[512,1023]` | 13 / 16 |
| … down to bucket `698` alone (8 functions) | 14 / 16 |
| **`ecdsa.verifyGeneric[*nistec.P256Point]` alone** | **15 / 20** |
| that function, promoted variable 3 (`Q`) alone | 10 / 20 |
| that function, any of the other 11 variables alone | 0–2 / 20 |

`Q` is `verifyGeneric`'s public-key point:

```go
Q, err := c.newPoint().SetBytes(pub.q)
...                                    // ~66 safepoints: NewNat, SetBytes,
...                                    // hashToNat, inverse, ScalarBaseMult
p2, err := Q.ScalarMult(Q, w.Mul(r, c.N).Bytes(c.N))
```

### The safepoint maps, before and after promotion

Dumping every safepoint's root list for `verifyGeneric` (env-gated
instrumentation, since reverted) says it outright. **Without** promotion:

    TEMP  t19 name=t14 cls=l gcref=true reg=NoReg slot=48
    root  t19 name=t14 ... alloc=true@792 words=map[0:true]      x 66 safepoints

The alloca for `Q` is a root at 66 safepoints, and word 0 of the allocation — the
pointer itself — is in the frame map at each of them. **With** promotion:

    TEMP  t19 name=t14 cls=l gcref=true reg=9 slot=-1
    (no root line anywhere)

Diffing the two root sets, promotion loses exactly three names: `t14` (the
allocation) and `t313`/`t315` (the two loads of `Q`, which no longer exist). What
replaces them is `%t24`, the result of `P256Point.SetBytes`:

    TEMP  t29 name=t24 cls=l gcref=false reg=0 slot=-1

**`gcref=false`.** `Q` is now carried across 66 safepoints by a value nothing
reports.

### Root cause

`opt/mem2reg.go` carried the slot's managed-ness onto the **phis** it mints
(`f.MarkGCRefType(p.To, v.gcType)`) and onto nothing else. That is half the
invariant.

A managed local's slot has its pointer word in the frame map for the whole span
the allocation reaches, so the *values* that pass through the slot need no marking
of their own: between the call that produces one and the store that files it away,
nothing can collect. goc relies on that. It marks a **load** from a managed slot
as a GC reference — `t313`/`t315` above are both `gcref=true` — and leaves a
multi-result constructor's **result** unmarked.

Promotion deletes the slot and the loads, and the unmarked value becomes what
carries the variable across every safepoint in between.
`arm64/regalloc.go isSafepointRoot` asks the value, not the storage, so it is
reported at no safepoint; and `arm64/gcalloc.go`'s force-to-stack rule keys on
`GCRef` too, so the allocator was free to leave it in `x9`. At `GOGC=10` the
`P256Point` is freed and its span reused before `Q.ScalarMult(Q, …)` reads it, and
ECDSA verification fails against a signature it just produced.

This is not the mechanism the brief hypothesised — nothing is lost *because* a
value was spilled, and the spill records are correct — but it is the same class of
defect it was pointing at, one level up: promotion moved the question from "is
this slot described?" to "is this value marked?", and the frontend only ever
answered that question for loads.

### The fix

`opt/mem2reg.go`'s new `markManagedDef` marks every value that becomes a reaching
definition of a promoted **managed** variable, not just the phis. That is exactly
as conservative as the slot was and no more: the variable's own `GCRef` flag is
the frontend's statement that this storage holds a managed pointer, and it is what
the frame map described. A type descriptor the value already carries wins over the
slot's.

### The reduction

`goc/testdata/runtime_gc_promoted_local_root.go`, added to the capability matrix
as `gc-invariants/promoted-local-root`. A managed local assigned from a
multi-result constructor and held across six collections with nothing else
referring to it.

| compiler | promotion | runs | failures |
|---|---|---|---|
| before the fix | **on** | 60 | **60** |
| before the fix | off | 60 | 0 |
| after the fix | **on** | 60 | 0 |
| after the fix | off | 60 | 0 |

It uses a finalizer as the detector rather than the object's corrupted contents. A
freed object is not always overwritten, so checking its fields catches this about
five runs in six; a finalizer that runs while the program is still holding the
pointer is the defect itself, and fires every time.

## 6. Rates after the fix

| program | promotion | collector | runs | failures |
|---|---|---|---|---|
| `p256` | on | `GOGC=10` | 250 | **0** (was 35/40) |
| `p256` | on | default | 250 | **0** |
| `flate` | on | default | 250 | **0** |
| `flate` | on | `GOGC=10` | 250 | **0** |
| `flate` | on | `GOGC=off` | 250 | **0** |

## 7. The rest of the corpus, with promotion on

**The capability matrix, all 368 programs, `-runtime-opt` + `GOC_BOUNDED_MEM2REG=1`:**

| | before the fix | after |
|---|---|---|
| pass | 366 | **368** |
| fail | 1 (`stdlib-netpoll-stress/tcp-churn`) | **0** |
| (the matrix had 367 capabilities; this branch adds the reducer) | | |

**Every benchmark program, 40 runs at the default collector setting and 40 at
`GOGC=10`, 960 runs per pass:**

| | before | after |
|---|---|---|
| `interp` `sha` `regexp` `json` `sortmap` `flate` `text` `chase` `conc` `gcpress` `float` | 0 failures | 0 |
| `p256` at `GOGC=10` | **24 / 40** | **0** |
| `p256` at default | 0 | 0 |

`p256` was the only program in either corpus that showed the shape, and it showed
it only above the default collection rate. That is worth saying plainly: **the
whole corpus is run at the default `GOGC`, and at the default `GOGC` this defect
is silent.** It took raising the collection rate to find it, and the failure it
produces is a wrong answer rather than a crash.

## 8. The other blocker on the switch is also gone

The switch's second blocker — `stdlib-netpoll-stress/tcp-churn` dying with
`cg12: interface dispatch failed for dynamic type 0x0`, which the wave-8 report
assigned to a separate job — is fixed by the same change:

| compiler | promotion | collector | runs | failures |
|---|---|---|---|---|
| before the fix | on | default | 20 | **18** |
| before the fix | on | `GOGC=off` | 20 | **19** |
| after the fix | on | default | 80 | **0** |

**How much of that is established, and how much is not.** That it is fixed is
measured — 0 in 80 runs against 18 in 20. *Why* is not fully established, and the
`GOGC=off` row says it is not the mechanism found in `p256`: turning the collector
off does not suppress it, so `tcp-churn` was not dying of a freed object. Marking
a value `GCRef` does three other things in the backend besides putting it in the
safepoint map, and any of them could be what `tcp-churn` needed:

* `arm64/gcalloc.go` force-spills it to a stack slot for its whole life instead
  of letting it live in a call-crossing register;
* `coalesceSpillSlots` then gives it a **private** slot, where an unmarked temp's
  slot is shared with other temps whose live ranges are believed disjoint;
* the safepoint map is also what stack copying uses to relocate a growing frame,
  and `tcp-churn` is the one workload here that grows stacks hard.

The middle one is the most likely and is the one to look at first if it ever
comes back. Someone should confirm which; this job did not.

## 9. Guards

All with `GOC_BOUNDED_MEM2REG` **unset**, on the fixed tree.

| guard | result |
|---|---|
| capability matrix, default arm | **368 / 368 PASS** |
| capability matrix, `-runtime-opt` arm | **368 / 368 PASS** |
| `runtime_gc_type_mask_padding.go`, default `GOGC`, `GOMAXPROCS=3` | **0 / 20** |
| `runtime_gc_type_mask_padding.go`, `GOGC=10`, `GOMAXPROCS=3` | **0 / 20** |
| `TestFrameEscapeAudit` | PASS |
| `TestAllocationCensus` | PASS (see below) |
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` | PASS |
| `scripts/determinism-check.sh`, default and `-O` | 5 / 5 identical, both caching paths, both rounds |
| goc output vs the pre-fix compiler, 4 programs × {default, `-O`} | **8 / 8 byte-identical** |
| `make test-ruby` (mem2reg also runs in `DefaultPipeline`, which cg12's C frontend takes) | PASS |

The matrix is **368**, not the 366 the brief expected: this branch's tree already
carried 367 capabilities before I touched it (measured on `7983abd` with the
switch off, both arms), and the reduction adds the 368th.

**The census.** `goc/testdata/alloc_census_baseline.txt` gains nine lines, every
one of them the new reducer's own allocation sites. No existing program's census
moves — checked by filtering the diff to lines that do not name the new file, and
there are none. The fix marks values the collector already had to see through the
slot; it changes no allocation decision.

**Determinism with promotion on**, which is not required but is what the next job
will need: 4 programs (`interp`, `p256`, `gcpress`, `json`), compiled cold twice
each at `-O` with the switch set — byte-identical, and each differs from its
switch-off build, so promotion was genuinely running. `determinism-check.sh -O`
with the switch set is 5/5 identical too, on a different set of hashes from the
switch-off run.

## 10. What this leaves

The `flate` blocker was already fixed by someone else's change and nobody had
re-measured. The real defect was one function away and one collector setting away,
and it is fixed here with a deterministic reducer in the corpus.

Both blockers named in `opt/pass.go`'s `BoundedPipeline` comment are now clean, so
that comment has been rewritten: it said the switch "is not yet correct" and named
two failures that no longer happen. **I did not flip the switch** — that is a later
job's call, and turning it on changes every generated program. What a job flipping
it now needs is the performance re-measurement (`make bench-perf`,
`make bench-crypto`) and a compile-cost check, none of which is in scope here.

One thing that would have found this years earlier, and is cheap: **the capability
matrix runs every program once, at the default `GOGC`.** A defect that only frees
an object the collector cannot see needs the collector to run, and the default
setting collects too rarely. A `gc-invariants` arm at `GOGC=10` over the existing
corpus — the setting that took `p256` from 0/40 to 24/40 — would have caught this
without any new program being written.
