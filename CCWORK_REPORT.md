# `println` dropped the spaces the spec requires — and four other print defects

Branch: `ccwork/println-spacing`, off `main` (`61b96da`).

**Status: implementation complete; verification in progress. Sections marked
UNVERIFIED have not been re-run yet.**

## The reported defect

`println("a", 1, true)` printed `a1true`; the host Go toolchain prints `a 1 true`.

The Go specification's own table for the two built-ins reads:

> `println`  like `print` but prints spaces between arguments and a newline at the end

so the separators and the trailing newline are required, not implementation-defined.
(Only the *formatting of each operand* is implementation-specific.) cg12's
`builtinRuntimePrint` walked `call.Args` and emitted one runtime print call per
operand, then a single `printnl` — the separators were simply never emitted.

The host toolchain implements the rule in `cmd/compile/internal/walk.walkPrint`
by rewriting the operand list before lowering: a `" "` string is inserted
between operands, a `"\n"` string is appended, and runs of adjacent constant
strings are then collapsed into one. cg12 now builds the same sequence
(`goc/compile.go`, `printOperands` / `collapsePrintLiterals`), so `println("x",
"y", "z")` becomes a single `printstring("x y z\n")` exactly as it does under the
host compiler, and `println("a", 1, true)` becomes
`printstring("a ") printint(1) printsp() printbool(true) printnl()`.

## Four further defects the audit against the host found

The task asked for the whole of `print`/`println` semantics to be checked
against the host toolchain rather than just the spacing. Four more differences
came out of that, all of them wrong-answer bugs in valid Go:

1. **A slice operand printed the address of its header as a decimal integer.**
   The host prints `[len/cap]0xaddr` via `runtime.printslice`. cg12 materialized
   a header and passed it to `printint`.

2. **An interface operand printed one decimal number.** The host prints
   `(0xtype,0xdata)` via `runtime.printeface` / `runtime.printiface`. cg12 fell
   through to `printint` on the interface's descriptor pointer, so a nil
   interface printed `0` where the host prints `(0x0,0x0)`.

3. **A complex operand printed a decimal integer.** The host prints `(1+2i)` via
   `runtime.printcomplex128` / `printcomplex64`.

4. **The statement was not atomic.** `runtime/print.go` states outright that
   *"the compiler emits calls to printlock and printunlock around the multiple
   calls that implement a single Go print or println statement"*, and
   `runtime.minhexdigits` is documented as protected by that lock. cg12 emitted
   neither, so two threads printing concurrently interleaved mid-operand, and
   every runtime diagnostic — every traceback, every `GODEBUG` message — was
   exposed to the same interleaving.

Also matched to the host, in the same pass:

- **Operands are evaluated before the lock is taken.** cg12 evaluated and
  printed each operand in turn, so `println("A", f(), "B", g())` where `f` and
  `g` print produced output interleaved into the middle of the statement; the
  host evaluates every operand first and then prints `A 1 B 2` as one run.
- **`runtime.quoted`** (`type quoted string`) now routes to `printquoted`. The
  runtime prints goroutine labels through it in `traceback.go:1294`, so without
  it a label containing a quote or a newline corrupted the traceback.

`print` versus `println` is unchanged in kind: only `println` gets separators and
the trailing newline, and both take the print lock.

## A fifth defect, outside print, that print exposed

Routing a `complex64` operand to `runtime.printcomplex64` **segfaulted**, because
that routine does `strconv.AppendComplex(buf[:0], complex128(c), ...)` and cg12's
`complex64` handling was broken in two independent ways. Both are pre-existing
and neither is reachable from the spacing bug; they are fixed here because
otherwise this change would have turned a silently-wrong `println(c64)` into a
crash.

- **`real()` and `imag()` of a `complex64` returned garbage, and the same garbage
  for both halves.** A `complex64` is represented as its two `float32` halves
  packed into one 64-bit integer, so extracting a half is a bitwise
  reinterpretation between a general-purpose and a floating-point register —
  `ir.OCast` (`fmov`). cg12 used `ir.OCopy`, a plain register move that re-types
  only within a register file, so the float half read whatever the integer
  register aliased. Reduced to: `var b complex64 = complex(3.5, 4.5);
  println(real(b), imag(b))` → host `3.5 4.5`, cg12 `-2.8673504e+25
  -2.8673504e+25`. Addition, subtraction, multiplication and conversion of
  `complex64` were all wrong for the same reason.

- **Converting between `complex64` and `complex128` reinterpreted one
  representation as the other.** `complex128` is a 16-byte value addressed by a
  pointer and `complex64` is a packed scalar, and `gen.convert` had no complex
  case at all, so `complex128(b)` `Copy`d the packed bits into a pointer.
  Reduced to: `var b complex64 = complex(3.5, 4.5); var w complex128 =
  complex128(b)` → **SIGSEGV at `0x4090000040600000`**, which is the packed
  `(4.5, 3.5)` bit pattern used as an address.

## Files changed

- `goc/compile.go` — `printOperands`, `collapsePrintLiterals`, `printStep`,
  rewritten `builtinRuntimePrint` and `builtinPrint`; `isRuntimeQuotedType`;
  `complex64Parts`, `packComplex64`, `complexComponent`, `complexConversion`,
  `convert`.
- `goc/reach.go` — the runtime print routines the new lowering can call are
  rooted: `printcomplex64`, `printcomplex128`, `printeface`, `printiface`,
  `printlock`, `printquoted`, `printslice`, `printsp`, `printunlock`.

## Verification

(filled in as it lands)
