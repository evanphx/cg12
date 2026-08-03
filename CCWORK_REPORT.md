# Asking the escape question for a variadic call: splitting the object that made it unanswerable

Branch `ccwork/variadic-escape-question`, off `ccwork/iface-convt-fastpath`
(`19488ee`). The previous jobs' reports are at `19488ee:CCWORK_REPORT.md` and
`4a6fd96:CCWORK_REPORT.md`.

Status: IN PROGRESS — numbers below are measured unless marked otherwise.

**Headline (provisional): `fmt.Sprintf("value=%d", 42)` costs goc 1.00
allocations against gc's 1.00 — exact parity, from 2.00. The `[N]any` backing
array of a variadic call is now a frame slot wherever the callee does not retain
the slice itself, and the boxed payload an element points at is decided
separately from it. The combined object was split, partially and deliberately;
section 2 prices both directions. The retention hole that forced the previous
attempt back to 2.00 is closed by construction rather than by an extra rule: the
callee that keeps `args[0]` now keeps a payload that is its own allocation, and
that payload goes to the heap while the array does not.**

## 1. What was actually wrong, confirmed before anything was changed

Two instruments, both on the base commit.

`goc/compile.go:6581` decides between a frame `[N]any` and a heap one:

    stackAllocateVariadic := !g.runtimeAllocation || g.fn.NoSplit || g.forceStackVariadic

and `forceStackVariadic` comes from a two-symbol allowlist. So the front end
never asks. That is true and it is not the whole story: the heap arm emits the
*neutral* `ir.OHeapAlloc` candidate, and `opt.LowerHeapAllocations` — which runs
unconditionally, `goc/compile.go:488`, not only under `-O` — does ask. The
question is asked; the representation is what made it unanswerable.

`goc/compile.go:6591-6613` builds one synthesized `struct{values [N]any;
payload0 T0; ...}` per call site and allocates the backing array and every boxed
payload as a single object. One object is one placement.

A diagnostic added to `opt` for this job (`GOC_DIAG_ESCAPE`, deleted before the
branch closes) prints where each candidate landed and the first use that escaped
it. On the base commit, for `fmt.Sprintf("value=%d", n)`:

    main.doSprintf .goc.runtime.type.struct_values__1_any__payload0_int  heap
        argument 1 of $fmt.Sprintf may retain something inside a self-referential object

and with the `needsDeepSummary` rule switched off, the same object is `frame`.
So **`fmt.Sprintf`'s `a []any` parameter does not escape at depth 0** — the
array was on the heap solely because the box inside it is retained.
`fmt.pp.doPrintf` assigns each element to `p.arg`, a field of a heap-allocated
printer, so the box genuinely is retained: this is not a conservatism to be
analysed away.

For `log/slog.Logger.Info` the same diagnostic says something different:

    argument 2 of $log/slog.Logger.Info escapes            (with the deep rule off)

and the parameter table agrees:

    FACT log/slog.Logger.Info param 2 "args.0" = escapes deep=false

The slice **itself** escapes there, through `Logger.log` → `Record.Add` →
`argsToAttr`, which returns a slice derived from `args` in a loop. That
difference decides the design, and section 2 is about it.

## 2. Pricing the two representations

(filled in below as the numbers land)

