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

## The tests fail before the change

Verified by reverting only the implementation files to the parent commit and
keeping the four new test files:

    arm64/a64             build failed: undefined: Frintp, Frintm, Frinta, ...
    arm64 (e2e on CPU)    arm64: unsupported intrinsic "float.trunc.s"
    interp                ir: unknown intrinsic "float.abs.s" (not registered)
    cmd/goc               no float.sqrt.d in the emitted IR: the call was not
                          lowered to the instruction
                          the software implementation is still called:
                            %t3 =d call $math.archTrunc(d %t2)
                            %t3 =d call $math.archCeil(d %t2)
                            %t3 =d call $math.archFloor(d %t2)
                            %t3 =d call $math.sqrt(d %t2)

`TestARM64MathIntrinsicEdgeCasesExecute` -- the Go program that checks every
documented special case through `math.Sqrt` and friends -- passes both before
and after, and has to: both implementations are correct, which is the point.
What fails before is the assertion that the call is gone.

## One more candidate the brief did not name: math.FMA

`math.FMA` has no architecture-specific arm in the Go source at all -- the
gc compiler intrinsifies it -- so goc runs the portable 180-line software
emulation. Measured the same way:

    math.FMA    goc 153.60 ns    host 1.34 ns    115x

AArch64 computes it in one instruction, `FMADD Dd, Dn, Dm, Da`, which is IEEE
754's `fusedMultiplyAdd`, and that is exactly what `math.FMA` is specified to
be. Checked against `stdlib/src/math/fma.go` copied out verbatim under another
name, over 410,648 triples: all 12,167 combinations of 23 special values
(both zeros, both infinities, four NaNs including a signalling one, the
subnormal boundary, MaxFloat64), 200,000 random triples, and 200,000 triples
built so the product very nearly cancels the addend -- which is precisely where
a fused multiply-add and a separate multiply-then-add give different answers,
so it is where a wrong lowering would show.

    exact 410,398   nan-payload-only 250   disagree 0

It is lowered too; see below for its measured effect.

**Correction to the line above: FMA is not lowered here, and the reason is the
backend, not the semantics.** `FMADD` is a three-operand floating-point
instruction, and arm64 reserves exactly two floating-point scratch registers
(`arm64/reg.go:287`, V30 and V31) against five integer ones. `sel.triReg` hands
operands slots 0, 1 and 2, so a float three-operand form would index
`floatScratchRegs[2]` and there is no such register; when the result is spilled,
slot 0 is the destination's as well and only one operand can be staged at all.
This is not an oversight I found by reading -- `arm64/lower.go`'s `foldMulAdd`
declines to fuse a float multiply-add for the same reason, with `if
in.Cls.IsFloat() { return }` as its first line.

Lowering FMA therefore means reserving a third floating-point scratch register,
which comes out of `floatAllocOrder` and changes register allocation for every
float-using function in the tree. That is a change that deserves its own
measurement rather than a ride on this one. The semantics are settled and the
prize is quantified (153.60 ns -> about 1.3), so the work is scoped; it is just
not this change.

## The float/sqrt-sum ratio after

First `make bench-perf` run, against the committed baseline:

    float     float/sqrt-sum   ratio 1.1335   sd 0.49%   goc 53,169,582 ns   host 46,907,864 ns

against the baseline's

    float     float/sqrt-sum   ratio 171.2179 sd 0.14%   goc 401,435,550 ns  host 2,344,593 ns

**171.22x -> 1.13x**, a factor of 151. (The raw nanoseconds are not comparable
between the two lines: the workload now does 20,000,000 square roots a round
rather than 1,000,000, since the count was only kept small because goc was slow.
Per root: 2.66 ns against the host's 2.35 ns.)

That run failed, but not on this row and not on any row's movement: it tripped
the noise ceiling on `chase/pointer-node`, whose one-repetition spread read
27.99% against its committed 1.98%, with the null equally loud -- the signature
the suite's own message gives for a busy box. It was busy because of me: I was
running the FMA differential and compiler builds beside it. The remaining runs
were done with nothing else of mine on the machine.
