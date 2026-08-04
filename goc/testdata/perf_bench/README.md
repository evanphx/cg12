# The runtime performance corpus

The programs `make bench-perf` times, that are not already in the tree.

Run the suite with

    make bench-perf

and read `goc/perfbench_test.go` for the method, `perf_suite_baseline.txt` for
the answers, and the rest of this file for why these four programs exist.

## What the suite is made of

Eleven workloads. Seven are `goc/testdata/placement_bench`, reused **unmodified**;
four are here.

Reusing rather than copying is deliberate. That corpus was built for a different
question — how much of a benchmark's number is decided by where its code landed
in `.text` — but it was built to the crypto benchmark's method, its programs are
deliberately unlike one another, and its committed sweep
(`placement_bench/analysis_shift_phase.txt`) already records each case's
placement residue under the alignment policy that ships. So when a row here
moves and the question is "did the code change or did it just move", the answer
for those seven programs is already measured, for that exact program. A copy
would have drifted and thrown that away.

`placement_bench/p256` is the one program in that corpus the suite does not
take. `make bench-crypto` already gates the ECDSA path with its own baseline and
its own triage note; two gates on one path means two red lights for one cause.

## The four here, and what each is for

| program   | what it presses                                                     |
|-----------|---------------------------------------------------------------------|
| `chase`   | dependent loads at three cache depths — memory latency               |
| `conc`    | goroutines, channels, mutexes — cost paid inside goc's runtime        |
| `gcpress` | allocation churn, a live heap, the write barrier, slice growth        |
| `float`   | floating-point arithmetic — a separate register file and lowering     |

They fill the four gaps the reused corpus leaves. Nothing in `placement_bench`
is bound by memory rather than by instruction count; nothing there starts a
goroutine or sends on a channel; nothing there is *about* what an allocation
costs, only about programs that happen to make some; and nothing there executes
a single floating-point instruction.

Each one earned its place by finding something. `float` found that `math.Sqrt`
is not lowered to the hardware instruction and costs 380 ns instead of 2.4 ns.
`conc` found that spawning and joining a goroutine costs goc 34× the host, where
an uncontended mutex costs 1.87×. `chase` supplies the suite's second
calibration row — a case bound by DRAM, which no compiler can speed up, and
which therefore has to read ≈ 1.00 or the instrument is lying.

## The shape every program has

The same as `crypto_signing_bench` and `placement_bench`, which is where it is
written down in full:

- each case is a closure called through a func value, so neither compiler can
  see through the call and delete the work;
- setup is outside the timer;
- `measure` warms up once and keeps the fastest of `rounds` timed rounds,
  because noise can only make a round slower;
- the first case every program prints is `control/spin-fixed-work`, byte-identical
  source in all eleven programs;
- output is one `case<TAB>nanoseconds` line per case, and nothing else.

A new program only has to do those five things to join the suite: add it to
`perfBenchPrograms` in `goc/perfbench_test.go` with a line saying what it is for,
then `make bench-perf-update`.

## Sizing

Every case is sized so the **goc-built** binary finishes a round in well under a
second. That is a hard constraint, not an aesthetic one: the suite runs each
program three times per repetition, nine repetitions deep, so a case that takes
goc one second a round costs the suite two minutes.

This is why `float/sqrt-sum` takes 1 M square roots and not 20 M, and why
`conc/goroutine/spawn-join` spawns 20 000 goroutines and not 100 000. Both
numbers are set by how slow goc is at them, and both are recorded in the source
next to the constant, because a constant chosen for a reason that is not written
down gets "tidied up" later.

## The control loop is also a measurement

All eleven programs contain the same 20-million-iteration dependent multiply-add
loop, from the same source. Eleven copies of one loop, laid out differently in
eleven binaries, is a direct reading of how much program-level code placement
moves a number on this box — so the eleven `control/spin-fixed-work` rows in the
baseline are worth reading side by side, and a spread among them is placement,
not compiler.
