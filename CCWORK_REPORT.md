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

(sections 3–5 below are filled in as the runs complete)
