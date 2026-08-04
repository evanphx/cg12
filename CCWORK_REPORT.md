# math intrinsics: lowering the single-instruction math functions on arm64

Branch: `ccwork/math-intrinsics`, off `ccwork/perf-suite` (`d2855f5`).

## Where this starts

`float/sqrt-sum` in the committed perf baseline
(`goc/testdata/perf_suite_baseline.txt:136`):

    float     float/sqrt-sum           171.2179   sd 0.10%  ...  401435550 ns / 2344593 ns

## Candidate survey, measured before any change

A probe program calling each candidate 2,000,000 times in a loop, built with
`goc -O` and with the host toolchain, pinned to one core. Nanoseconds per call:

| function | goc | host | ratio |
|---|---|---|---|
| math.Sqrt        | 396.03 | 2.35 | 168.5x |
| math.Abs         |  10.87 | 0.87 |  12.5x |
| math.Floor       |   4.96 | 1.00 |   5.0x |
| math.Ceil        |   4.94 | 1.00 |   4.9x |
| math.Trunc       |   4.94 | 1.00 |   4.9x |
| math.RoundToEven |  21.58 | 1.01 |  21.4x |
| math.Round       |  19.87 | 1.00 |  19.9x |
| math.Min         |   6.84 | 3.61 |   1.9x |
| math.Max         |   6.94 | 3.61 |   1.9x |
| math.Copysign    |  14.61 | 1.42 |  10.3x |

Every one of them is a call in goc today. Floor/Ceil/Trunc/Min/Max already reach
the hardware instruction, but through a translated Plan 9 assembly stub called
over ABI0 (`stdlib/src/math/floor_arm64.s`, `dim_arm64.s`, both in
`plan9asm`'s supported list). Sqrt, Abs, Round, RoundToEven and Copysign have no
assembly at all on arm64 and run the pure-Go implementations.

(work in progress -- semantics verification next)

## What the hardware actually does, checked rather than assumed

The reference is `stdlib/src/math`'s own portable algorithms, copied out under
other names so the host compiler could not intrinsify them (it lowers
`math.Sqrt` and friends to these very instructions, so calling them would only
have asked the hardware to confirm itself). Both were run over 50,263 inputs:
every documented special case, both zeros, both infinities, six NaNs including
signalling ones, every ties-to-even boundary, all 2,048 exponents at three
significands each, every subnormal boundary, and 40,000 pseudo-random bit
patterns. The two-operand functions were run over all 490,000 ordered pairs
drawn from the first 700.

| Go function | instruction | verdict |
|---|---|---|
| `math.Sqrt` | `FSQRT D` | agrees everywhere; NaN payload differs only for `Sqrt(x<0)` (below) |
| `math.Abs` | `FABS D` | bit-identical on all 50,263, NaN payloads included |
| `math.Floor` | `FRINTM D` | bit-identical except signalling-NaN quieting (below) |
| `math.Ceil` | `FRINTP D` | same |
| `math.Trunc` | `FRINTZ D` | same |
| `math.RoundToEven` | `FRINTN D` | same |
| `math.Round` | `FRINTA D` | same |
| `math.Min` | `FMIN D` | **wrong** -- see below |
| `math.Max` | `FMAX D` | **wrong** -- see below |
| `math.Copysign` | (none exists) | not lowerable |

The two divergences, both of which the Go specification leaves open and both of
which move goc's answer *towards* the host toolchain's:

1. `Sqrt(x)` for `x < 0`. The portable code returns `math.NaN()`, payload 1;
   `FSQRT` returns the architecture's default NaN, payload 0. Go specifies only
   `Sqrt(x < 0) = NaN`, and the host toolchain already returns the latter.
2. A **signalling** NaN operand to one of the roundings: `FRINT*` sets the quiet
   bit, the portable code returns the operand untouched. 7 of the 50,263 inputs.
   A Go program can only obtain a signalling NaN through `Float64frombits`, and
   `math.Floor` already reaches `FRINTMD` on this target through assembly.

Everything else -- every finite value, both zeros, both infinities, every quiet
NaN -- is bit-identical.

### math.Min and math.Max are not safely lowerable, and that is a real finding

Go specifies `Max(x, +Inf) = +Inf` and `Min(x, -Inf) = -Inf` *for every x,
including NaN*. `FMAX`/`FMIN` propagate the NaN instead. Over the 490,000 pairs
there are exactly 24 disagreements and they are all of that shape:

    Max(NaN, +Inf):  Go = 7ff0000000000000   FMAX = 7ff8000000000001
    Min(NaN, -Inf):  Go = fff0000000000000   FMIN = 7ff8000000000001

`FMAXNM`/`FMINNM` are wrong the other way -- they return the non-NaN operand
where Go returns NaN -- and disagree on 5,548 pairs. Go's own arm64 assembly
(`stdlib/src/math/dim_arm64.s`) is `FMAXD` wrapped in an explicit `+Inf` test
for exactly this reason. There is no single instruction to lower to, so they
were left alone.

`math.Copysign` has no candidate: AArch64 has no copy-sign instruction, and the
math package already expresses it as the bit-field operation it is.

## What was lowered

`math.Sqrt`, `math.Abs`, `math.Floor`, `math.Ceil`, `math.Trunc`, `math.Round`
and `math.RoundToEven`, plus the implementations they delegate to (`math.sqrt`,
`math.archFloor`, `math.archCeil`, `math.archTrunc`) so that the compiled body
of `math.Sqrt` is itself the instruction -- otherwise an indirect call through a
function value, and every caller inside the math package, would still have run
the software implementation.

New IR intrinsics `float.{sqrt,abs,floor,ceil,trunc,roundeven,roundaway}.{s,d}`,
registered Pure (a function of their operand alone, so GVN may share them and
GCM may move them), selected in `arm64/select.go`, executed by both interpreter
engines, and emitted by `goc/compile.go` only for the arm64 target.

## Verification so far

- The seven new encoders are checked against the system assembler and read back
  by the disassembler (`arm64/a64`, existing round-trip tables).
- `arm64/floatmath_e2e_test.go`: each intrinsic compiled to machine code, linked
  and run on the CPU, compared on the **bits** (`-0.0 == 0.0` and `NaN != NaN`
  would let a wrong answer through a comparison on values) against literal
  expected patterns for NaN, ±0, ±Inf, negative operands to Sqrt, and the ties.
- `interp/floatmath_test.go`: the same facts for both interpreter engines.
- The interpreter was diffed against the hardware over all 50,263 inputs x 7
  intrinsics -- 351,841 comparisons, **0 disagreements** -- so the difftest
  oracle cannot drift from the backend.

## Measured after the change (same probe, same core)

| function | goc before | goc after | host |
|---|---|---|---|
| math.Sqrt        | 396.03 | 2.65 | 2.35 |
| math.Abs         |  10.87 | 2.60 | 0.87 |
| math.Floor       |   4.96 | 2.77 | 1.00 |
| math.Ceil        |   4.94 | 2.77 | 1.00 |
| math.Trunc       |   4.94 | 2.78 | 1.00 |
| math.RoundToEven |  21.58 | 2.78 | 1.01 |
| math.Round       |  19.87 | 2.79 | 1.00 |
| math.Min         |   6.84 | 6.85 | 3.61 | (not lowered)
| math.Max         |   6.94 | 6.92 | 3.61 | (not lowered)
| math.Copysign    |  14.61 | 14.40 | 1.42 | (not lowered)

The residual gap against the host is the rest of the probe loop -- the integer
to float conversion and the accumulate -- not these functions.

(work in progress -- perf suite and guards next)
