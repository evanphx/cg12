# Allocation-placement census: a committed instrument

Branch `ccwork/escape-alloc-census`, off `ccwork/escape-frame-publication` (`ddd03eb`).

Goal: turn the throwaway census that `ccwork/escape-frame-publication` ran by hand
-- "where does every allocation land across the 385-program corpus" -- into a
committed tool plus an accepted baseline plus a test that fails when placement
moves in either direction.

This file is written as each result lands.
(The previous contents of this file were the `escape-frame-publication` fix report
and its independent verification; they are unchanged in history at
`05946f2:CCWORK_REPORT.md`.)

---

## 1. What the finished IR does and does not carry (measured, not assumed)

Before designing anything I compiled `goc/testdata/hello.go` with
`goc.CompileExecutable` and counted what an analysis of the *finished* module can
actually see. The numbers below are from that one program.

| thing in the finished IR | count | carries a type? | carries a source position? |
|---|---|---|---|
| `OAlloc4`/`OAlloc8`/`OAlloc16` (frame slots) | 18,171 | **no** | mostly (12,411 distinct positions; 383 with none) |
| `OCall runtime.newobject` | 338 | yes (arg 1 is the type descriptor symbol) | 148 of 338; 190 have none |
| `OCall runtime.makeslice` | 13 | yes (element type) | yes |
| `OCall runtime.makemap` | 2 | yes | no |
| `OHeapAlloc` left over after lowering | 0 | -- | -- |

Two facts drove the design:

1. **The heap side is complete and typed from the finished IR alone.** Every heap
   allocation is a call to a named runtime allocator with the type descriptor as an
   argument. That covers both allocations the front end decided were escaping
   (`allocateEscapingTyped`) and heap-allocation candidates that
   `opt.LowerHeapAllocations` lowered to calls -- they are the same instruction
   afterwards, which is exactly right: the census should not care which pass made
   the decision, only where the object landed.

2. **The frame side is neither complete nor typed from the finished IR alone.**
   `opt.LowerHeapAllocations` rewrites a promoted `OHeapAlloc` into a bare
   `OAlloc{4,8,16}` whose only argument is the byte size; the type descriptor is
   dropped. Worse, a promoted candidate is then indistinguishable from the 18,000
   ordinary local variable slots in the same module. A census that recorded every
   `OAlloc` would be 12,411 lines *per program* and would carry no types.

So the frame side has to be recorded where the decision is made. That is one
place: the rewrite loop at the bottom of `opt/escape.go`.

## 2. Scope of the census, stated precisely

The instrument records, deduplicated across the whole corpus:

* every **heap** allocation: a call to `runtime.newobject`, `runtime.makeslice`,
  `runtime.makeslice64`, `runtime.makemap`, `runtime.makemap64`,
  `runtime.makemap_small`, `runtime.makechan`, `runtime.makechan64` or
  `runtime.newarray`, found by scanning the finished IR; and
* every **frame** allocation that `opt.LowerHeapAllocations` promoted out of a
  heap-allocation candidate, recorded by that pass as it rewrites.

It deliberately does **not** record ordinary front-end frame slots
(`gen.localAllocTyped`, `gen.localAlloc`). Those are the 18,000-per-program noise
floor, they have no type in the IR, and they are not the product of an escape
decision that the compiler re-derives.

That exclusion does **not** create a blind spot for the regression class this
instrument exists to catch. The heap side is complete, so a front-end change that
moves an object from `runtime.newobject` to a plain frame slot -- which is exactly
the "six functions LOSING a heap allocation" failure the ad-hoc census caught --
removes a line from the baseline and fails the test. What the census cannot do for
such a site is *label* the move as `heap -> frame` rather than "site removed"; it
still names the site, the function and the type. Sites the escape pass decides get
the stronger, labelled treatment in both directions.

(Findings and verification below are appended as they land.)
