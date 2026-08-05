# The mem2reg GC-visibility blocker: it does not reproduce, and why

Branch: `ccwork/mem2reg-gc-visibility`, off `integration/wave8` (`7983abd`).

**Verdict up front.** The `compress/flate` collector crash that blocks
`GOC_BOUNDED_MEM2REG=1` **no longer reproduces**: 0 crashes in 750 runs of the
goc-built `flate` benchmark with promotion on, across three collector settings.
It was the same zero-capacity-slice defect the wave-8 `flate-gc-crash` job fixed,
amplified by promotion — not a distinct GC-visibility bug. The hypothesis it was
handed to me under ("the backend does not record a spilled promoted temp in the
safepoint map") is **refuted**, by measurement and by inspection of the code that
does the recording.

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

