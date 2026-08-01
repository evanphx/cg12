# A checker that says "this escape decision disagrees with the emitted code"

Branch `ccwork/escape-checker`, off `main` (`ad4e9b2`). The task: build the check that
would have caught `ccwork/escape-analysis`'s `2724ac7` before it merged, rather than
letting it arrive as `fatal error: found bad pointer in Go heap` in the collector.

This file is written as each result lands. Sections are in the order they were measured.

## 0. Status

Work in progress. Confirmed so far, and each item below says how it was measured.

## 1. Reproduced, and the reproduction is configuration-dependent

`main` + `2724ac7` (cherry-picked; only `CCWORK_REPORT.md` conflicted, `goc/compile.go`
merged clean and the applied diff is byte-identical in size to the original commit,
488 insertions).

    make test-goc-status -run gc-invariants/mark-workers   →  FAIL in 32s
    runtime: pointer 0x4167d380fd40 to unallocated span span.base()=0x4167d380e000
    fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)

**The failure needs the split build.** Compiled monolithically (`goc -o out prog.go`)
the same tree passes 20/20 runs. Compiled the way the matrix does it — `goc
build-runtime -o pack.gocrt -packages ""` then `goc -runtime pack.gocrt -o out prog.go`
— it fails 10/10. Anyone bisecting this bug with a plain `goc prog.go` will conclude it
is fixed when it is not.

## 2. Does `cg12checkwb=2` already catch this? No, and the reason generalises

The brief asks this first, so it is answered first, with measurements.

**`cg12checkwb=2` does not catch it, and neither does a `=3` that widens the rule from
"a stack address into a global" to "a stack address into anything that outlives the
frame".** Both were run against the failing tree; both stay silent and the collector
throws later exactly as before.

Three separate reasons, all of which matter for anything built on that diagnostic:

1. **The hook was on one function.** `cg12CheckWriteBarrierPair` was called only from
   `runtime.atomicwb`. That is less narrow than it looks — every barriered pointer store
   goc emits goes `goc_storep → runtime.atomicstorep → atomicwb`, so ordinary Go stores
   were covered — but the *bulk* barriers were not. `bulkBarrierPreWrite` and
   `bulkBarrierPreWriteSrcOnly` buffer words straight into the write-barrier buffer, and
   those are the paths `typedmemmove`, `growslice` and `typedslicecopy` use. This branch
   adds the same validation to both (`stdlib/src/runtime/mbitmap.go`). With it, the
   mark-workers fault moves from "the background marker draining the buffer" to
   `main.buildGraph`'s own `growslice`, in the goroutine that owns the bad word.

2. **A barrier-based check has a duty cycle.** It only runs while `writeBarrier.enabled`
   is true, i.e. during a mark phase. A wrong escape decision publishes a frame address
   *whenever the program runs*. Measured: on the failing tree, `goc/testdata/runtime_slice_pointer_append_gc.go`
   and `runtime_debug_gc_controls.go` both emit a store of a frame-resident backing array
   into a fresh heap object, and both run clean under `cg12checkwb=3` — the store happens
   with the barrier off, so nothing looks at it.

3. **The value is often not a stack address by the time anyone looks.** In the
   mark-workers fault the rejected word is `0x…79f20` in a span with `state=1`,
   `elemsize=16`, `limit=0x…79f00` — i.e. *past the last object* of a live 16-byte span.
   That is a pointer to an object that was freed and whose span was recycled into a
   different size class, not a live goroutine stack. A rule phrased as "reject a
   goroutine stack address" cannot see it.

`cg12checkwb=3` is still worth having (it is in this branch) — it is the tightest
statement of the invariant at a barrier — but it is not the answer to this bug, and the
honest summary is: **nobody could have caught `2724ac7` by running the existing
diagnostic, because the existing diagnostic cannot see stores made outside a mark
phase.**

## 3. The compile-time check: `opt.FrameEscapes`

`opt/framecheck.go`. It runs over the finished IR, after `opt.LowerHeapAllocations`, and
answers one question per function: *does any pointer derived from one of this function's
own frame allocations get stored somewhere that is not part of the same frame?*

It is a may-analysis. A value is frame-derived if it is an `OAlloc*`/`OAllocN` result or
reaches one through copies, casts, pointer arithmetic, phis, the memory helpers that
return their destination, and loads from a frame slot that a frame address was stored
into. A finding is one of:

- `store` — a plain `OStore*` of a frame-derived value to a destination that is not a
  frame allocation of this function;
- `barrier` — the same through `goc_storep`, which is what a pointer field of a heap
  object receives;
- `return` — a function returning the address of its own frame.

Values that merely *reach a call* are deliberately not reported: `&local` passed to a
callee is how Go is written, and the store the callee performs is reported instead.

`GOC_DEBUG_ESCAPECHECK=1` prints the findings during any `goc` compilation, next to the
existing `GOC_DEBUG_NOSPLIT` audit.

### It finds the bug

Swept over all 384 `goc/testdata` programs on both trees (80 seconds each, 8-way):

| Tree | distinct findings |
| --- | ---: |
| `main` (`ad4e9b2`) | 70 |
| `main` + `2724ac7` | 72 |

Two findings are new on the failing tree, and both are the defect:

    runtime_slice_pointer_append_gc.go:25:20: main.main: barrier %t4  into memory reached through %t80
    runtime_debug_gc_controls.go:32:20:       main.main: barrier %t12 into memory reached through %t86

Both are `runtime.KeepAlive(values)` where `values := make([]*record, 0, 4)`. On
`2724ac7` the backing array is `alloc8 32` — a frame allocation — and `KeepAlive` boxes
the slice into a fresh `runtime.newobject` and stores the header, data pointer included,
into it. A frame address in a heap object, emitted unconditionally, decided at compile
time. On `main` the same array is a `runtime.newobject` and there is no finding.

Neither program *fails* at run time, because by line 25 the slice has grown past capacity
4 and the data pointer is heap again. That is the point of a static check: it reports the
wrong decision whether or not this run's data happens to trigger it.

### What it does not catch, stated plainly

`gc-invariants/mark-workers` itself produces **no** new finding on the failing tree. Its
`buildGraph` does put `frontier`'s one-element backing array in the frame under `2724ac7`
(`alloc8 8` where `main` emits `runtime.newobject`), but that array is only ever stored
into another frame slot, so the rule above does not fire. Whatever carries it out of the
frame is not a store this analysis can see. That is an open gap, not a solved one.

(remaining sections written as they land)
