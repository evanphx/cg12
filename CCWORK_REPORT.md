# Wiring goc's interface-conversion fast paths: what `convT*` was worth

Branch `ccwork/iface-convt-fastpath`, off `main` (`4a6fd96`). The previous jobs'
reports are at `4a6fd96:CCWORK_REPORT.md`.

Status: IN PROGRESS. Numbers land here as they are produced. Anything not
watched to completion is marked UNVERIFIED.

**Headline: `any(7)` cost goc one 8-byte allocation and now costs none, and the
five-attribute `slog.Info` call went from 9.00 allocations to 1.00 against gc's
0.00. `slog.Int`, `slog.Bool`, `slog.Duration` and `slog.Float64` are each at
exact parity with go1.26.1, and `slog.Info` at a disabled level is at 0.00
against gc's 0.00.**

## 0. The host toolchain, pinned

Host toolchain is `go1.26.1 linux/arm64`; goc built from this branch and from
`4a6fd96` for the before/after columns, run as `goc -run`.

## 1. The defect, confirmed on main before anything was changed

`runtime.convT16/convT32/convT64` are vendored in at
`stdlib/src/runtime/iface.go:366,379,400`, and `runtime.staticuint64s` — the
256-entry read-only table they hand back a pointer into for a value that fits in
a byte — is defined in `stdlib/src/runtime/ints.s`, which the ARM64 translator
does assemble (`plan9asm.SupportsARM64File("runtime", "ints.s")` is true).
Nothing in goc's codegen called any of them.

Measured on `4a6fd96` with `goc -run`, allocations per 100 calls:

```
box_small_int        100      # takeAny(theInt), theInt = 42
box_bool             100      # takeAny(theBool)
return_any_from_int  100      # func(v int) any { return v }
```

The host toolchain pays 0 for all three.

## 2. What was built

The escape decision has to be made before the conversion helper is a good idea,
because a payload that stays in the frame is cheaper than any call — and
`convT64` handed a value past the table *allocates*, so calling it
unconditionally would have turned free frame slots into allocations. gc has the
same ordering: `walkConvInterface` tries a stack temporary first and only reaches
`dataWordFuncName` when the value escapes.

So the choice is made where the escape decision already lives:

- `ir.Block.HeapAllocConverted` (`ir/build.go`) emits the same allocation
  candidate as before, carrying two more operands: the helper that could build
  the object, and the value to hand it. Its doc comment carries the two rules
  the emitter has to keep.
- `opt.rewriteHeapAllocations` (`opt/escape.go`) promotes such a candidate to a
  frame slot exactly as before when it can, and calls the helper instead of the
  allocator when it cannot — dropping the store that would have initialized the
  object, because the helper's result already holds the value and, for a small
  value, points into read-only memory that must not be written.
- `gen.interfaceConversionHelper` (`goc/compile.go`) is the predicate. It is
  `cmd/compile`'s `dataWordFuncName` narrowed to types goc holds in a register,
  and it is asked *after* `isDirectInterfaceType`, inside the same branch of
  `adaptValueToInterface` that already decides direct versus indirect — so there
  is one direct/indirect predicate, not two.
- `goc/reach.go` roots the three helpers. Nothing in any AST calls them; they
  only become callees during lowering, so they are roots or they are absent —
  which is what the first build failed on.

### What is deliberately not wired

`convT`, `convTnoptr`, `convTstring` and `convTslice`. Their payloads are not
register-shaped, so passing one to a helper means spilling it and reading it
back, which is the copy the fast path exists to avoid. More to the point, they
buy nothing here: measured against go1.26.1, `convTstring` and `convTslice`
allocate for every value except the empty one, and goc already pays exactly one
allocation for those — `control/any-string-variable` is 1.00/16 B under both
compilers. gc's 0.00 on `any("literal")` comes from its readonly-global path,
not from `convTstring`.

The fast path is also off for targets other than arm64, because `staticuint64s`
is defined in `ints.s` and only the ARM64 translator consumes that file. On a
target that does not, the array is a zero-filled Go `var` and the helper would
hand back a pointer to a zero for every value below 256 — a wrong answer, not a
slow one. That is a note for whoever finishes the amd64 Go target.

## 3. Allocation counts: before, after, and gc

`goc/testdata/allocation_counts.go` and the table in `goc/alloccount_test.go`,
regenerated per their headers. Allocations per 100 calls.

| row | goc @4a6fd96 | goc now | gc |
|---|---|---|---|
| `box_small_int` | 100 | **0** | 0 |
| `box_bool` | 100 | **0** | 0 |
| `box_large_int` | 100 | 100 | 0 |
| `box_float64` | 100 | 100 | 0 |
| `box_string` | 100 | 100 | 0 |
| `box_pointer` | 0 | 0 | 0 |
| `return_any_from_int` | 100 | **0** | 0 |
| `return_any_from_large_int` | 100 | 100 | 100 |
| `return_any_from_pointer` | 0 | 0 | 0 |
| `sprintf_int` | 200 | 200 | 100 |
| `variadic_any` | 0 | 0 | 0 |

Read the four rows where goc still pays and gc does not carefully: they are gc's
*escape analysis*, not gc's static table. `-gcflags=-m` says "theLargeInt does
not escape", "theFloat does not escape", "theString does not escape" at each of
them, because `takeAny` does not leak its parameter, so gc puts the payload in a
frame and never reaches a conversion helper at all. `return_any_from_large_int`
is the same large value in a shape gc cannot frame, and there the two agree
exactly at 100 — which is the row that proves goc's fast path is about the value
and not about the type. `box_pointer` and `return_any_from_pointer` stay at 0:
a pointer-shaped value goes in the interface word with no box at all, and
`TestInterfaceConversionsCallTheRuntimeHelpers` asserts it calls no helper.

The new table fails at `4a6fd96`: run there, `TestAllocationCounts` reports
`box_small_int costs 100 allocations per 100 calls under goc, and this table
says 0`.

`sprintf_int` does not move. Its remaining allocation is not the box — 42 fits
the static table under both compilers now — it is the `...` object, which goc
packs the array and the boxed payload into and puts on the heap because fmt's
`doPrintf` assigns each element to a field of a heap-allocated printer.

### A fixture that had to change, and why

`theFloat` in the testdata is assigned in `init()` rather than in its
declaration. **goc compiles a package-level `float64` initialized to a constant
to zero.** That is a pre-existing defect — it reproduces identically at
`4a6fd96` — and it has nothing to do with boxing, but zero's bit pattern *does*
fit the static table, so declaring `theFloat = 3.5` the ordinary way made the
float row measure `any(0.0)` and report a fast path that was not running.

Two more pre-existing float defects turned up next to it, both reproduced at
`4a6fd96` and neither touched here:

- `var fromInt = float64(theInt) / 12` at package scope is also zero.
- `var fromCall = makeFloat()` at package scope, where `makeFloat` returns
  `3.5`, is `3.500000477186404` — the value has been through a `float32`.

## 4. log/slog: the numbers this was really about

`goc/testdata/slog_allocations_baseline.txt`, regenerated per its header.
Allocations per operation, goc against gc.

| case | goc before | goc now | gc |
|---|---|---|---|
| `control/any-int-small` | 1.00 | **0.00** | 0.00 |
| `control/any-int-large` | 1.00 | 1.00 | 1.00 |
| `control/any-bool` | 1.00 | **0.00** | 0.00 |
| `control/return-interface` | 1.00 | **0.00** | 0.00 |
| `control/context-background` | 1.00 | **0.00** | 0.00 |
| `control/handler-enabled` | 1.00 | **0.00** | 0.00 |
| `control/variadic-6-preboxed` | 1.00 | **0.00** | 0.00 |
| `control/variadic-6-literal` | 1.00 | **0.00** | 0.00 |
| `attr/slog.Int` | 1.00 | **0.00** | 0.00 |
| `attr/slog.Bool` | 1.00 | **0.00** | 0.00 |
| `attr/slog.Duration` | 1.00 | **0.00** | 0.00 |
| `attr/slog.Float64` | 1.00 | **0.00** | 0.00 |
| `info/1-attr` | 5.00 | **1.00** | 0.00 |
| `info/3-attr` | 7.00 | **1.00** | 0.00 |
| `info/5-attr` | 9.00 | **1.00** | 0.00 |
| `info/6-attr` | 11.00 | **2.00** | 1.00 |
| `info/3-attr-large-ints` | 7.00 | **1.00** | 3.00 |
| `logattrs/3-attr` | 6.00 | **1.00** | 0.00 |
| `logattrs/6-attr` | 11.00 | **3.00** | 1.00 |
| `disabled/no-attrs` | 2.00 | **0.00** | 0.00 |
| `disabled/3-attr` | 3.00 | **1.00** | 0.00 |
| `disabled/logattrs-3-attr` | 5.00 | **1.00** | 0.00 |

`attr/slog.Float64` reaching 0.00 is not the float fast path: `slog.Float64`
stores the float in `Value.num` and boxes the *Kind*, which is a small integer.
`info/3-attr-large-ints` is goc paying **less** than gc — three values past the
static table cost gc three `convT64` allocations, where goc packs all three
payloads into the one `...` object it was already allocating.

The two `json/*` rows still crash. That is the separate slog miscompile
`slog-attr-gcmask` is fixing on another branch; the only thing that moved is the
frame name in the message (`main_func_304_28` → `main_func_311_28`), which is
generated-function numbering shifting because the runtime now compiles three
more functions.
