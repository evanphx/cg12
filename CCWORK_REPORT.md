# Batch compilation: one goc process, many programs

Branch: `ccwork/goc-batch-b`, off `perf/test-suite` (`0f4ee02`). The previous job's report has
been moved to `docs/report-matrix-speed.md`, following the precedent that job set, so this file
is only about this job.

Status: **in progress.** Everything stated below was measured on this box. Anything not
measured is named as such.

## Headline, up front: the lever is real but it is a sixth of its estimate

The briefing sizes this lever at **~700 s of the ~3030 s of compile CPU (23%)**, on the
reasoning that `hello.go` costs `wall=2.11s` against the pack and "most of it is loading and
type-checking the runtime's source closure", which `goc/source_world.go` caches per process.

**Measured, the amortizable part is ~0.53 s of that 2.16 s, not ~2 s.** The rest is per-program
work that a shared world does not touch. See §1 for the measurement. That puts the lever at
roughly **180 s of the ~3030 s (6%)**, and — at the matrix's current floor — approximately
**zero wall clock**, because the matrix is bounded by one 190 s single-threaded compile and not
by compile CPU / workers.

That does not make the work pointless: it makes it a *precondition*. The moment the sibling
`pack-stdlib` lever removes the six 160 s `net/http` compiles, the floor drops and
`compile CPU / workers` becomes the bounding term — and then 180 s off the numerator is a real
6% off the wall clock. It is reported here honestly rather than claimed at the briefing's size.

## 1. How big the per-process fixed cost actually is

The world is built by the first compile in a process and reused by every later one, so the
amortizable cost is exactly `first compile − later compile` in one process. Measured with a
scratch main (`analysis/worldbench`) that reads a prebuilt pack from disk and compiles the same
program N times through `prebuilt.CompileProgram`, the same call `goc -runtime` makes:

    pack read: 0.021s
    goc/testdata/hello.go  iter=0  2.055s   <- includes building the shared source world
    goc/testdata/hello.go  iter=1  1.496s
    goc/testdata/hello.go  iter=2  1.516s
    goc/testdata/hello.go  iter=3  1.544s
    goc/testdata/hello.go  iter=4  1.529s
    iters=1  wall=2.07 user=3.35 sys=0.31 maxrss=296448kB
    iters=5  wall=8.22 user=13.63 sys=0.67 maxrss=372512kB

From the two `/usr/bin/time` rows, one warm compile costs `(13.63 − 3.35) / 4 = 2.57 s` of CPU
and ~1.52 s of wall. The first compile costs ~3.3 s of CPU and 2.05 s of wall. So:

| | wall | CPU |
| --- | ---: | ---: |
| building the shared source world (once per process) | **0.53 s** | **0.73 s** |
| compiling `hello.go` with the world already built | 1.52 s | 2.57 s |
| reading the 8.8 MB pack (once per process) | 0.021 s | — |

For reference the whole `goc` process on the same program is `wall=2.16 user=3.67` (three runs:
2.16/2.15/2.17), so the ~0.1 s of wall unaccounted for above is process start plus the `cc`
link.

**What this means for the matrix.** The saving is a constant ~0.53 s per compile, independent of
the program, because the world is only the runtime's closure — everything a program imports
beyond that is loaded privately either way. Over 338 programs that is ~180 s of the ~3030 s of
compile CPU the matrix spends, i.e. **6%**, not 23%.

**Where the other 1.5 s of `hello.go` goes** is not the world and is therefore not this lever:
against a prebuilt pack, `compile()` still generates IR for the whole runtime closure and only
then subtracts the functions the pack already defines. That is per-program work repeated 338
times. It is the largest single remaining item in a small program's compile and it is noted here
for whoever integrates the three branches; it is not touched by this job.

## 2. Still unverified

Everything below §1. This section is updated as results land.
