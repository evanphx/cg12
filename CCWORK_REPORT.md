# A runtime performance suite: goc against the host Go toolchain

Branch: `ccwork/perf-suite-work`, off `ccwork/bench-crypto-noise` = `1af81e1`,
whose measurement method this reuses rather than reinventing.

Earlier reports on this file are superseded, not deleted — the bench-crypto-noise
report is `git show 1af81e1:CCWORK_REPORT.md`.

## What was missing

The tree measures allocation well and time badly. The allocation census, the
placement comparison against gc, the frame-escape audit, the loop-aliasing audit
and the slog allocation benchmark all count something. Exactly one instrument
measures elapsed time — `make bench-crypto` — and it watches one path.

So a change that cost 5 % everywhere outside ECDSA would land green, and any
performance work outside crypto and slog was unmeasurable. This adds
`make bench-perf`: eleven programs, goc against the host Go toolchain on
identical source, with a committed baseline and a per-row statement of what it
can and cannot see.

## Guards

All four run **on the final tree**, after everything below had landed:

| guard | result |
|---|---|
| `make test-goc-status` (default arm) | **366/366**, 0 failures, 0 skips |
| `make test-goc-status-opt` (`-O` arm) | **366/366**, 0 failures, 0 skips |
| `TestAllocationCensus` | **ok** — reproduces `alloc_census_baseline.txt` unchanged |
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` | **ok** |
| working tree | no goc-built binary added; `git status` shows only the intended files |

Both capability arms were also 366/366 on the starting tree, before any of this,
so the number is a before-and-after and not a single reading.

**No compiler behaviour change.** The diff is one test file, four programs under
`goc/testdata/perf_bench`, one baseline, two Makefile targets and documentation.
Nothing under `arm64/`, `ir/`, `opt/`, `lower/`, `link/`, `cmd/` or any other
compiler package is touched:

    CCWORK_REPORT.md                        |  453 +++-
    Makefile                                |   37 +-
    README.md                               |   37 +
    goc/perfbench_test.go                   | 1210 ++++++++++++++++++++++
    goc/testdata/perf_bench/README.md       |   91 ++
    goc/testdata/perf_bench/chase/main.go   |  168 +++
    goc/testdata/perf_bench/conc/main.go    |  150 +++
    goc/testdata/perf_bench/float/main.go   |  169 +++
    goc/testdata/perf_bench/gcpress/main.go |  206 ++++
    goc/testdata/perf_suite_baseline.txt    |  136 ++

(Per the brief, `go test ./goc/...` and `make test-unit` were not run. The census
and determinism guards were invoked with an explicit `-run` on the single test,
which is how `make bench-crypto` itself is defined.)

## The suite validated against itself

- `make bench-perf` against the committed baseline: **green in 10.5 minutes**,
  every row within tolerance.
- Reproducibility: two independent nine-repetition runs 25 minutes apart agree to
  within **0.5 % on 32 of 42 rows**. The largest movement was +3.0 % on
  `regexp/replace` (tolerance 6.6 %) and +2.9 % on `chase/dram` (tolerance
  18.7 %). On 41 of 42 rows the movement did not survive the two intervals at
  all — the "resolved" column is negative, meaning the run could not tell the
  change from zero.
- **It fails in both directions.** Proved by perturbing the committed baseline
  and rerunning:

        sha/hmac-1mib     baseline 0.8000 -> run 1.0157   +27.0%  PAST TOLERANCE  (slower bucket)
        sha/sha256-1mib   baseline 1.3000 -> run 1.0075   -22.5%  PAST TOLERANCE  (faster bucket)
        control/...       baseline 1.6307 -> run 1.6318    +0.1%  within tolerance

  The two rows landed in the two separate failure buckets, each with its own
  message; the unperturbed control row in the same program stayed green.

## The suite

One command:

    make bench-perf

Eleven programs, compiled by `goc -O` and by the host toolchain from identical
source, timed nine times each, interleaved and pinned, against
`goc/testdata/perf_suite_baseline.txt`. About twelve minutes. Fails in both
directions. `make bench-perf-update` rewrites the baseline.

### The workloads, and what each is for

Seven are `goc/testdata/placement_bench`, reused **unmodified**; four are new in
`goc/testdata/perf_bench`.

| program   | what it presses |
|-----------|-----------------|
| `interp`  | a bytecode interpreter: a switch dispatch loop |
| `sha`     | SHA-256 and HMAC over a buffer: one tight block loop, same assembly both sides |
| `regexp`  | regexp matching: a second, larger interpreter over a pointer-linked program |
| `json`    | `encoding/json` round trip: reflection, interface dispatch, a hand-written scanner |
| `sortmap` | `sort.Slice` and map build/probe: indirect calls through a callback, and hashing |
| `flate`   | `compress/flate` round trip: table-driven loops over byte slices |
| `text`    | `strconv`, `fmt`, `strings.Builder`: string building and formatting |
| `chase`   | dependent loads at three cache depths — the only memory-bound workload |
| `conc`    | goroutines, channels, mutexes — cost paid inside goc's runtime |
| `gcpress` | allocation churn, a live heap, the write barrier — what an allocation costs |
| `float`   | floating-point arithmetic — a separate register file and lowering decisions |

Reusing rather than copying the seven is the load-bearing decision. That corpus
was built for a different question, but it was built to the crypto benchmark's
method, its programs are deliberately unlike one another, and its committed sweep
`analysis_shift_phase.txt` already records **each case's code-placement residue
under the alignment policy that ships**. So when a row moves and the question is
"did the code change or did it just move", the answer for those seven programs is
already in the tree, for that exact program. A copy would have drifted.

`placement_bench/p256` is the one program not taken: `make bench-crypto` gates
that path already, and two gates on one path is two red lights for one cause.

The four new ones fill what that corpus leaves out. Nothing there is bound by
memory rather than instruction count, nothing there starts a goroutine, nothing
there is *about* what an allocation costs, and nothing there executes a single
floating-point instruction. Each found something; see the findings below.

### What is gated: a ratio, not an index

The crypto benchmark compares an **index** — a case divided by a control measured
in the same binary — which is machine-independent and is right for what it does.
This suite compares

    ratio = goc nanoseconds / host nanoseconds

formed inside one repetition, from two runs seconds apart on the same pinned
core, then averaged over repetitions.

The reason is specific. An index divides goc by goc, so a change that made goc's
control loop slower *too* leaves every index flat — and "a change that quietly
costs 5% everywhere" is precisely what this suite exists to catch. The ratio's
denominator is a fixed reference that only moves when the host toolchain does,
and the host version is recorded in the baseline so that case is separable.

Pairing inside a repetition matters: a repetition's two readings share whatever
the machine was doing in that minute, so dividing them there removes it.
Averaging the two arms first and dividing after would divide one repetition's goc
by another repetition's host.

### The null arm, and where tolerances come from

Every repetition runs the goc binary **twice** and divides one run by the other.
It is the same file, so the answer is exactly 1.0000 by construction. Two things
come out of that:

- **A hard check that the protocol is honest.** A deviation from 1 that survives
  its own confidence interval is not noise — it is a systematic artefact, one
  position in the rotation running consistently warmer than another. The crypto
  benchmark measured an artefact of exactly that kind at 1.28% before it
  interleaved. If the null is not 1.0000, no other column in the run is believed.
- **A split of the noise into goc-side and host-side.** The null sees goc-side
  noise only.

Tolerances come from the **ratio's** own spread and not the null's, and that
correction was forced by measurement. On this box `json/marshal`'s ratio moves
3.7% between repetitions while its null moves 1.0%; `goroutine/spawn-join`'s
ratio moves 5.8% while its null moves 0.09%. The host-built binary finishes those
cases in about 12 ms and it is the *host* run jittering. A tolerance drawn from
the null would have been three to sixty times too tight on exactly those rows —
and in the first full run of this suite it was, and four rows failed the
instrument check because of it.

So: `tolerance = max(3 × the row's own one-repetition spread, 5%)`, and the run
fails only when the whole interval on the difference clears that band. The 5%
floor is for code placement, which neither noise column can see, because both use
one pair of binaries and placement changes between *builds*.

### Two cases the instrument check condemned, and a finding in one of them

The check that no row may be noisier than 15 % is not decorative. It rejected two
baselines before it accepted one.

`chase/pointer-node` had the shortest timed region in its program and the highest
noise in it — 6.4 % spread. Its walk is now four times longer, and the row came
back at **0.58 %**, a tenfold improvement. That is what the ceiling is for.

`gc/slice-grow` — appending without a size hint — could not be fixed, and **that
is itself a result**. Growing a 4 MB slice four times a round gave 19 % spread.
Rewritten to the same total number of appends over forty ten-times-smaller
growths, it got *worse*: **52 % on the ratio and 71 % on the null**. The null is
the same goc binary against itself, so this is not a comparison artefact: the goc
binary's own cost for this case moves by tens of percent from one process to the
next. A 52 % spread earns a 157 % tolerance, which is a row that passes anything.

It was dropped rather than carried. Under goc, the un-hinted append path's cost
is dominated by when a collection lands, to the point where it cannot be measured
on this box — which says something about goc's collector pacing and nothing about
any compiler change. The sizes that were tried are recorded in `gcpress/main.go`
so that whoever fixes the pacing can put the case back.

### And a third the suite had to be built around: goc-built flate dies

The first nine-repetition run got to repetition 5 and stopped, because the
goc-built `flate` binary exited with status 2.

That is the defect `44d76cf` already recorded: goc-built `placement_bench/flate`
dies in the collector — *"pointer to unused region of span"*, inside
`compress/flate`'s 4864-byte compressor state — on **about one run in twenty** at
the default GOGC. It is pre-existing, it is not a placement effect, and nothing
in this branch touches it.

One run in twenty is invisible to a corpus that runs a program once and fatal to
a suite that runs `flate`'s goc binary eighteen times: at that rate **three runs
in five would die**, on something that is not a performance regression at all.

So the suite retries a run that dies, up to three times, and:

- logs every retry, with the last twelve lines of the dead run's stderr;
- prints a per-program crash rate at the end of every run;
- **fails** if any program's rate exceeds 20%, with a message that names the
  known flate defect so a reader can tell it from a new one;
- fails immediately if any binary dies three times in a row.

A crashed run is a *missing* measurement, not a slow one, so replacing it biases
nothing — unlike discarding a slow run, which would. The alternative was to drop
`flate` from the suite, which would have removed a workload and quietly routed
around a known miscompile.

### The numbers: goc against the host, per workload

Nine interleaved repetitions, pinned to core 62, `go1.26.1 linux/arm64`,
Neoverse-N1. Full table with all 42 rows in
`goc/testdata/perf_suite_baseline.txt`; "noise" is the row's one-repetition
spread, "detect" the smallest movement the suite can fail on.

| program | case | goc/host | noise | detect |
|---|---|---:|---:|---:|
| *all 11* | `control/spin-fixed-work` | **1.630–1.633** | 0.04–0.22 % | 5.0–5.2 % |
| `interp` | `interp/bytecode-loop` | **21.46×** | 0.05 % | 5.1 % |
| `sha` | `sha/sha256-1mib` | **1.008×** | 0.07 % | 5.1 % |
| `sha` | `sha/hmac-1mib` | **1.016×** | 0.09 % | 5.1 % |
| `regexp` | `regexp/find-submatch` | **7.95×** | 0.35 % | 5.4 % |
| `regexp` | `regexp/anchored-lines` | **6.14×** | 0.35 % | 5.4 % |
| `regexp` | `regexp/replace` | **7.07×** | 2.20 % | 9.0 % |
| `json` | `json/marshal` | **17.18×** | 1.71 % | 7.0 % |
| `json` | `json/unmarshal` | **11.29×** | 4.75 % | 19.4 % |
| `sortmap` | `sort/ints` | **3.82×** | 0.13 % | 5.1 % |
| `sortmap` | `sort/slice-callback` | **3.74×** | 0.64 % | 5.7 % |
| `sortmap` | `map/build-probe` | **7.88×** | 3.26 % | 13.3 % |
| `flate` | `flate/decompress` | **7.59×** | 0.17 % | 5.2 % |
| `flate` | `flate/compress` | **6.86×** | 0.79 % | 5.9 % |
| `text` | `text/parse` | **10.20×** | 0.16 % | 5.2 % |
| `text` | `text/utf8-decode` | **4.41×** | 0.22 % | 5.2 % |
| `text` | `text/format-append` | **10.99×** | 6.39 % | 26.1 % |
| `text` | `text/sprintf` | **11.17×** | 8.81 % | 36.0 % |
| `chase` | `chase/l1-resident` | **1.457×** | 0.35 % | 5.4 % |
| `chase` | `chase/pointer-node` | **1.010×** | 1.98 % | 8.1 % |
| `chase` | `chase/dram` | **1.027×** | 6.23 % | 25.5 % |
| `conc` | `mutex/uncontended` | **1.863×** | 0.04 % | 5.0 % |
| `conc` | `chan/send-buffered` | **4.81×** | 0.25 % | 5.3 % |
| `conc` | `chan/pingpong-unbuffered` | **6.49×** | 0.93 % | 6.0 % |
| `conc` | `goroutine/spawn-join` | **38.45×** | 4.27 % | 17.4 % |
| `gcpress` | `gc/pointer-write` | **9.33×** | 0.73 % | 5.8 % |
| `gcpress` | `gc/live-heap-churn` | **8.47×** | 4.33 % | 17.7 % |
| `gcpress` | `gc/alloc-churn` | **11.70×** | 3.06 % | 12.5 % |
| `float` | `float/int-convert` | **1.547×** | 0.03 % | 5.0 % |
| `float` | `float/mandelbrot` | **2.589×** | 0.11 % | 5.1 % |
| `float` | `float/dot-product` | **4.95×** | 0.29 % | 5.3 % |
| `float` | `float/sqrt-sum` | **171.22×** | 0.14 % | 5.1 % |

### The control row is eleven independent measurements of one loop

The same 20 M-iteration multiply-add loop is compiled into all eleven programs
from the same source, and lands in eleven differently laid out binaries. Its
ratio across them:

    1.6303  1.6307  1.6322  1.6322  1.6322  1.6322  1.6322  1.6326  1.6327  1.6330  1.6331

Peak to peak, **0.17 %**. That is a direct measurement of how much *whole-program
code placement* moves a number on this box for the alignment policy that ships,
on a loop-containing function — and it is the strongest single piece of evidence
that the instrument is sound, because eleven separate builds agreeing to two
parts in a thousand is not something a noisy measurement does.

### What a green run means, per workload

**The suite fails on a real movement of 5.0 % on 27 of 42 rows** and on ≤ 6.0 %
on 34 of them. Grouped by what a row can actually see:

| detect | rows | what those rows are |
|---:|---:|---|
| 5.0–5.5 % | 27 | all eleven controls, `interp`, both `sha`, `sort/ints`, `text/parse`, `text/utf8-decode`, `chase/l1-resident`, `mutex`, all four `float`, `flate/decompress`, two `regexp` |
| 5.5–10 % | 7 | `sort/slice-callback`, `flate/compress`, `gc/pointer-write`, `chan/pingpong`, `json/marshal`, `chase/pointer-node`, `regexp/replace` |
| 10–20 % | 5 | `gc/alloc-churn`, `map/build-probe`, `goroutine/spawn-join`, `gc/live-heap-churn`, `json/unmarshal` |
| > 20 % | 3 | `chase/dram` (25.5 %), `text/format-append` (26.1 %), `text/sprintf` (36.0 %) |

**Cannot**, and this is the honest part:

- **Nothing under 5 % anywhere.** The 5 % floor is code placement, not noise. A
  3 % regression passes green on every row. No instrument on this box both sees
  3 % and does not fire at random.
- **The three worst rows are effectively report-only.** `text/sprintf` cannot
  fail on less than a 36 % movement. It is kept because the committed number
  still moves and a reader can see it, and because `text/parse` and
  `text/utf8-decode` in the same program are two of the quietest rows in the
  suite — the noise is in `fmt`'s path, not in the workload's design.
- **`chase/dram` is noisy for a physical reason** and cannot be fixed by more
  repetitions: each run is a fresh process with a fresh physical page mapping for
  a 64 MiB ring, and DRAM latency depends on it. It earns its place as a
  calibration row that reads 1.03, not as a sensitive one.
- **A pure rebuild cannot fire it — checked, not assumed.** The placement sweep
  already in the tree measured, for each of these programs, how far its number
  moves when the text is shifted by 4…28 bytes and *not one instruction changes*.
  Compared against this suite's thresholds, all 17 reused case rows clear:

  | row | placement residue | detect |
  |---|---:|---:|
  | `text/sprintf` | 21.72 % | 36.0 % |
  | `text/format-append` | 15.62 % | 26.1 % |
  | `regexp/anchored-lines` | **4.48 %** | **5.4 %** ← thin |
  | `json/marshal` | 3.95 % | 7.0 % |
  | `map/build-probe` | 2.95 % | 13.3 % |
  | the other 12 | ≤ 2.3 % | ≥ 5.1 % |

  `regexp/anchored-lines` is the one thin margin: 4.48 % against a 5.4 % bar. A
  rebuild that moved code unluckily could land within a point of firing it. The
  four new programs have no such measurement — the sweep predates them — so their
  residue is assumed to resemble their neighbours' and is not known.
- **A change that slows the host toolchain reads as goc getting faster.** The
  denominator is the host. The host version is in the baseline header and a
  movement is reported in its own bucket with that stated.
- **It says nothing about multi-core behaviour.** Every run is pinned to one
  core, deliberately; `conc` measures the runtime's bookkeeping and context
  switches, not parallel speedup.

### Triage: when a row moves, which of three things happened

This is the part that made the crypto benchmark's 6.20% placement flip solvable
rather than argued about, so it is in the failure message where someone will hit
it, not in a document they would have to know to look for. `PROGRAM` is the
failing row's first column and `SOURCE` its path.

**Before any of the three**, rerun the one program at high repetitions. It costs
a minute instead of twelve and it is often the whole answer:

    go test -run '^TestPerformanceSuite$' ./goc -perf-bench \
        -perf-bench-only=PROGRAM -perf-bench-reps=25 -v

**1. An allocation moved.**

    go test -run '^TestAllocationCensus$' ./goc -v
    git diff -- goc/testdata/alloc_census_baseline.txt

A site that went `FRAME` → `HEAP` is the shape of the one regression this tree
has a record of. The `gcpress` rows moving together with the failing row points
here first.

**2. The generated code changed.** Compare *encoded instruction words*, which do
not move when the text does:

    go build -o "$TMPDIR/goc.suspect" ./cmd/goc
    git stash && go build -o "$TMPDIR/goc.parent" ./cmd/goc && git stash pop
    for side in suspect parent; do
      "$TMPDIR/goc.$side" -O -o "$TMPDIR/bench.$side" goc/SOURCE
      objdump -d "$TMPDIR/bench.$side" |
        awk '/^[0-9a-f]+ </{f=$2; next} /^ +[0-9a-f]+:/{print f, $2}' > "$TMPDIR/words.$side"
    done
    diff "$TMPDIR/words.parent" "$TMPDIR/words.suspect" | head -40

The encoding column, not objdump's rendering: the rendering prints absolute
branch targets, which differ whenever the text shifts even if the code is
identical.

**3. Nothing changed and the code moved.** If (2) says the words are identical,
this is placement and nothing got worse:

    nm "$TMPDIR/bench.parent"  | sort -k3 > "$TMPDIR/syms.parent"
    nm "$TMPDIR/bench.suspect" | sort -k3 > "$TMPDIR/syms.suspect"
    diff "$TMPDIR/syms.parent" "$TMPDIR/syms.suspect" | head

Identical words plus different addresses is the signature. Then measure how big
the effect is on that exact program with the sweep the tree already has —
`GOC_TEXT_PAD=K` puts K bytes of no-ops in front of the first function and
changes not one instruction:

    for K in 0 4 8 12 16 20 24 28; do
      GOC_TEXT_PAD=$K "$TMPDIR/goc.suspect" -O -o "$TMPDIR/pad.$K" goc/SOURCE
      taskset -c "$GOC_PERF_CORE" "$TMPDIR/pad.$K"
    done

If the row swings across K by as much as the movement being triaged, the movement
is placement. For the seven reused programs this is already measured:
`placement_bench/analysis_shift_phase.txt`, the `loop32` column.

Only (1) and (2) are regressions.

## Findings

Every number here is the mean of nine interleaved, core-pinned repetitions with
its 95 % interval, taken with the suite itself.

### goc's own control loop is 1.63× the host

Every program contains the same control: a 20-million-iteration dependent
integer multiply-add loop, byte-identical source in all eleven.

    ratio 1.6322  [1.6303, 1.6331 across the eleven programs]

That is the floor. It is a loop with no memory traffic, no allocation, no calls
and no runtime involvement — the simplest thing a code generator can be asked to
do — and goc takes 63 % longer. Every other ratio in the suite should be read
against this number, not against 1.00.

### `math.Sqrt` is not lowered to the hardware instruction: 171×

    float/sqrt-sum   ratio 171.22x +/- 0.10%   (goc 401 ns/sqrt, host 2.34 ns/sqrt)

The host's number is about seven cycles, which is `FSQRT D` throughput on this
core. goc's is two orders of magnitude away, which is not a code-generation
difference — it is a software square root being called per iteration. This is
the "obviously wrong" finding the suite was asked to look for, and it was found
by the first workload that pressed floating point at all.

The case had to be shrunk from 20 M iterations to 1 M to make the suite
runnable, because at 20 M the goc-built binary spent 7.7 seconds in this one case
per round.

### Goroutine creation is 38×, and channels are 5–6×

    goroutine/spawn-join       38.45x +/- 3.28%     (20k spawn+join)
    chan/pingpong-unbuffered    6.49x +/- 0.72%
    chan/send-buffered          4.81x +/- 0.20%
    mutex/uncontended           1.86x +/- 0.03%

The mutex row is close to the 1.63× floor, so the atomic fast path is fine. The
scheduler is not: spawning and joining a goroutine costs goc twenty times the
floor.

### The rows that read ≈ 1.00, and why that is the instrument working

    sha/sha256-1mib    1.0077x +/- 0.05%
    sha/hmac-1mib      1.0161x +/- 0.07%
    chase/pointer-node 1.0103x +/- 1.52%
    chase/dram         1.0265x +/- 4.79%

`sha` reads 1.00 because both binaries run the *same* code: both contain 56
ARMv8 SHA2 instructions, and goc's `crypto/internal/fips140/sha256.block`
dispatches to the same hand-written assembly the host uses. `chase/dram` and
`chase/pointer-node` read 1.00 because they are stalled on memory, which no
compiler can help. These are the suite's calibration rows: they are *supposed* to
read 1.00, so when they do not, the instrument is wrong before the compiler is.

`sha/sha256-1mib` at 1.0077 ± 0.05 % is the sharpest of them. Two binaries
running identical machine code for the timed work, measured on different
processes minutes apart, agree to eight parts in a thousand.

### The interpreter is 21×, and the reason is visible in the disassembly

`interp` is the workload the brief pointed at as the most realistic single
program available, and it is the one whose ratio is furthest out of line with
what the rest of the suite would predict. It is a switch over an opcode in a
loop: no allocation, no runtime calls, no memory traffic beyond two small local
arrays. On the control loop goc is 1.63×. Here it is **21.46× ± 0.04 %** — one of
the tightest intervals in the suite, so it is not a measurement artefact.

    interp/bytecode-loop   21.46x +/- 0.04%   (goc 678.0 ms, host 31.6 ms)

`goc -O` compiles `main.execute` to 964 instructions where the host toolchain
uses 188, and **52 % of goc's are loads and stores against 22 % of the host's**.
The pattern in the dispatch loop is a double indirection per local:

    ldr  x17, [x29, #48]        // load the *address* of a local from a frame slot
    ldr  x9,  [x17]             // load the local itself
    ...
    str  x9,  [x17]             // and store it straight back

Every local — `pc`, `top`, and the base addresses of `stack [64]int64` and
`registers [8]int64` — lives in a stack slot, is reloaded on each use, and is
reached through a pointer that itself lives in another stack slot. Nothing stays
in a register across an iteration.

That is also the best available explanation for the 1.63× floor on the control
loop, which is a single accumulator: 2.7 ns per iteration is about eight cycles,
which is a multiply plus a store-load round trip, where the host's 1.67 ns is
about five, which is a multiply in a register.

Read with care: this is a reading of one function, sampled across ~90 of its 964
instructions plus the whole-function memory-op count. It is a strong hint about
where a large constant factor lives, not a diagnosis of the register allocator.

### Where goc is obviously doing something wrong, in order

The brief asked whether any workload shows a ratio far out of line with the
others. Three do, and one is a factor of a hundred:

| finding | ratio | against the 1.63× floor |
|---|---:|---:|
| `math.Sqrt` is not lowered to `FSQRT` | **171×** | 105× the floor |
| goroutine create + join | **38×** | 24× |
| the interpreter dispatch loop | **21×** | 13× |
| reflective marshalling (`json/marshal`) | **17×** | 11× |
| map build + probe, `fmt` formatting, `flate` | 7–12× | 4–7× |
| sorting, interface/callback dispatch | 3.7–4.9× | 2–3× |
| uncontended mutex, int↔float conversion | 1.5–1.9× | ≈ 1× |
| identical assembly, or memory-bound | 1.01–1.03× | — |

The bottom two rows are the ones that make the top row believable: the suite can
produce 1.00 when 1.00 is the right answer.

`math.Sqrt` is the one to fix first. It is one instruction on this architecture,
the fix is local, and it is a hundred times the cost of everything around it.

### P-256 is 45×, and that is assembly against Go, not codegen against codegen

    p256/sign-verify   goc 293.1 ms   host 6.58 ms    44.6×
    p256/verify        goc 213.8 ms   host 4.70 ms    45.5×

The host toolchain uses the hand-written P-256 assembly; goc runs the generic Go
path. It is reported here and deliberately **not** made a row of this suite:
`make bench-crypto` already watches that path, and a second gate on the same code
means two red lights for one cause.

## What this does not do

- It does not run in CI. It is opt-in behind `-perf-bench` for the same reason
  the placement comparison and the slog benchmark are — a host Go toolchain —
  plus one of its own: it measures time, so a loaded box produces a number about
  the box. A parallel `go test ./...` is exactly the wrong place for it.
- It does not say what any ratio *ought* to be. Several rows are far from 1.00
  for reasons that are not defects. The baseline's job is to hold each row where
  it is and say when it moved.
- It does not measure parallel behaviour. Every run is pinned to one core.
- It does not cover the ECDSA path. `make bench-crypto` does, and the two use the
  same core-selection convention offset by one so they can run at the same time
  on one box.

## The line

Eleven workloads, `make bench-perf`, ~10.5 minutes, committed baseline, fails
both ways:

| workload | goc/host | noise floor | detect |
|---|---:|---:|---:|
| `interp` bytecode dispatch | 21.46× | 0.05 % | 5.1 % |
| `sha` SHA-256 / HMAC | 1.008× / 1.016× | 0.07 / 0.09 % | 5.1 % |
| `regexp` match / replace | 6.14–7.95× | 0.35–2.20 % | 5.4–9.0 % |
| `json` marshal / unmarshal | 17.18× / 11.29× | 1.71 / 4.75 % | 7.0 / 19.4 % |
| `sortmap` sort / map | 3.74–7.88× | 0.13–3.26 % | 5.1–13.3 % |
| `flate` compress / decompress | 6.86× / 7.59× | 0.79 / 0.17 % | 5.9 / 5.2 % |
| `text` parse / utf8 / sprintf | 4.41–11.17× | 0.16–8.81 % | 5.2–36.0 % |
| `chase` L1 / pointer / DRAM | 1.457× / 1.010× / 1.027× | 0.35–6.23 % | 5.4–25.5 % |
| `conc` mutex / chan / goroutine | 1.86–38.45× | 0.04–4.27 % | 5.0–17.4 % |
| `gcpress` churn / barrier | 8.47–11.70× | 0.73–4.33 % | 5.8–17.7 % |
| `float` convert / mandelbrot / sqrt | 1.547–171.22× | 0.03–0.29 % | 5.0–5.3 % |
| *the control, in all eleven* | 1.630–1.633 | 0.04–0.22 % | 5.0–5.2 % |

**The smallest regression the suite can detect is 5.0 %**, on 27 of its 42 rows,
and ≤ 6.0 % on 34 of them. Three rows cannot see less than 25 %, and the baseline
prints that number next to each of them.

---

# Investigation: the 1.63x floor and the double indirection (ccwork/locals-double-indirection)

## Root cause, found in the first hour: `-O` does not run mem2reg on any real program

`opt.OptimizeModule` picks between two pipelines by module size:

    opt/opt.go:45   if moduleOptimizationOverBudget(m) { Run(m, BoundedPipeline()); return }
    opt/opt.go:50   Run(m, DefaultPipeline())

`DefaultPipeline` (opt/pass.go:104) begins with `FuncPass("mem2reg", Mem2Reg)` and runs
it a second time after inlining. `BoundedPipeline` (opt/pass.go:163) is

    Fixpoint("bounded-clean", fold, copy, dce)

and contains **no mem2reg, no inlining, no loadelim, no GVN, no simplifycfg, no GCM**.

The budgets are 2048 funcs / 50000 blocks / 200000 instrs / 400000 temps. Measured on
the perf suite's `interp` program (a 168-line Go file) with a diagnostic added to
`moduleOptimizationOverBudget`:

    GOC_DEBUG_BUDGET: funcs=5101 blocks=70160 instrs=297389 temps=241206
                      (caps 2048/50000/200000/400000)

Three of the four caps are exceeded by 2.5x, 1.4x and 1.5x. **Every goc-compiled Go
program is over budget**, because the program module carries the stdlib closure the
prebuilt pack did not already contain. So `goc -O` on a real program is fold+copy+DCE.

Consequence: every alloca-backed local stays in memory. That is the double indirection.

## The evidence: the control loop, before and after

`main.control` is the whole floor:

    func control() {
        accumulator := uint64(1)
        for i := 0; i < 20_000_000; i++ {
            accumulator = accumulator*6364136223846793005 + 1442695040888963407
        }
        sink = accumulator
    }

**`goc -O` today** (22 instructions in the loop; both locals live in frame slots and
make a store-to-load round trip every iteration):

    69ff08:  add  x17, x29, #0x18     // recompute &i
    69ff0c:  ldr  x9,  [x17]          // load i
    69ff10:  mov  x17, #0x2d00
    69ff14:  movk x17, #0x131, lsl 16
    69ff18:  cmp  x9,  x17
    69ff1c:  b.lt 69ff4c
    ...
    69ff4c:  add  x17, x29, #0x10     // recompute &accumulator
    69ff50:  ldr  x9,  [x17]          // load accumulator
    69ff54-60: 4x mov/movk            // materialize 6364136223846793005
    69ff64-70: 4x mov/movk            // materialize 1442695040888963407
    69ff74:  madd x9,  x9, x17, x15
    69ff78:  add  x17, x29, #0x10     // recompute &accumulator again
    69ff7c:  str  x9,  [x17]          // store accumulator
    69ff80:  add  x17, x29, #0x18
    69ff84:  ldr  x9,  [x17]
    69ff88:  add  x9,  x9, #0x1
    69ff8c:  add  x17, x29, #0x18
    69ff90:  str  x9,  [x17]
    69ff94:  b    69ff08

The loop-carried critical path is `ldr -> madd -> str -> ldr` (store-to-load forwarding),
about 8 cycles; measured 2.73 ns/iteration. Note also that the address of each slot is
re-materialized with `add x17, x29, #imm` instead of being folded into the load's
base+offset addressing mode -- a second, smaller defect with the same origin.

**The host** does the same work in 14 instructions with `accumulator` in x1 and `i` in
x0, critical path `MUL -> ADD` ~5 cycles; measured 1.67 ns/iteration. (The host
rematerializes the two 64-bit constants inside the loop too, so that is *not* the
difference.)

**`goc -O` with mem2reg forced on** -- 14 instructions, no memory traffic at all:

    654fd4:  mov  x17, #0x2d00
    654fd8:  movk x17, #0x131, lsl 16
    654fdc:  cmp  x9,  x17
    654fe0:  b.lt 655008
    ...
    655008-24: 8x mov/movk            // the two constants, as the host does
    655028:  madd x10, x10, x17, x15  // accumulator stays in x10
    65502c:  add  x9,  x9, #0x1       // i stays in x9
    655030:  b    654fd4

## Measured effect (single run, core-pinned, `placement_bench/interp`)

    build                         control/spin-fixed-work    interp/bytecode-loop
    goc -O (shipped)                     54.66 ms                677.77 ms
    goc -O, bounded pipeline             54.63 ms                677.94 ms
    goc -O, bounded + mem2reg            30.99 ms                646.45 ms
    host go1.26.1                        33.48 ms                 31.59 ms

**The 1.63x floor is entirely this.** With mem2reg the control ratio goes 1.632 -> 0.926:
goc becomes *faster than the host* on that loop.

**The interpreter row does NOT share the cause.** 21.46x -> 20.46x is a 4.6% improvement,
nothing like the floor's 43%. The interpreter's gap is a different, still-open problem.

Cost of turning mem2reg on for a bounded module: +0.27 s wall (6.65 -> 6.92) and +6 MiB
peak RSS (604 -> 611 MiB) on this program. The budget exists (commit 48200ab) to keep
large HTTP programs under a 3 GiB compile ceiling; mem2reg is not what costs that.

## The interpreter's 21x is a different defect: a 16-byte struct copy becomes three calls

`main.execute`'s dispatch head is `in := program[pc]`, where `instruction` is
`{op int; operand int64}` -- sixteen bytes. `goc -O` lowers that to **three library
calls per dispatched opcode**:

    6548e8:  mov  w1, #0x0
    6548ec:  mov  x2, #0x10
    6548f0:  add  x0, x29, #0x2b0
    6548f4:  bl   goc_memset          // zero `in` (16 bytes)
    6548f8:  ldr  x17, [x29, #24]     // <- the double indirection: slot 24 holds &slot 0x58
    6548fc:  ldr  x9,  [x17]          //    program.ptr
    654900:  ldr  x17, [x29, #40]     //    pc
    654904:  add  x1,  x9, x17, lsl #4
    654908:  mov  x2, #0x10
    65490c:  add  x0, x29, #0x2c0
    654910:  bl   goc_memcpy          // program[pc] -> an unnamed 16-byte temp
    654914:  mov  x2, #0x10
    654918:  add  x0, x29, #0x2b0
    65491c:  add  x1, x29, #0x2c0
    654920:  bl   goc_memcpy          // that temp -> `in`

The host loads the same sixteen bytes with one `LDP`.

Causal test, no compiler change: rewrite the source to read the two fields directly
(`inOp := program[pc].op; inOperand := program[pc].operand`) and recompile both sides.

    build                                   interp/bytecode-loop     ratio
    goc -O (shipped), struct copy               677.8 ms            21.46x
    goc -O + mem2reg, struct copy               646.5 ms            20.46x
    goc -O + mem2reg, no struct copy             39.7 ms             1.26x
    host go1.26.1                                31.6 ms             1.00x

So the interpreter row is ~95% one defect -- small aggregate copies lowered to
memset+memcpy+memcpy -- and ~5% the missing mem2reg. **The floor and the interpreter
row do not share a cause.** Fixing the floor leaves the interpreter at 20x; fixing the
struct copy would take it to ~1.3x.

(The `ldr x17,[x29,#24]; ldr x9,[x17]` pattern the perf-suite report called "double
indirection" is real and is visible above, but it is a minor term: the two memcpy calls
around it cost far more. The control loop's version of the defect is the plainer one --
`add x17, x29, #imm; ldr x9, [x17]`, an address computed rather than folded, on a local
that should not have been in memory at all.)

## The tree already knew half of this

`RUNTIME_PLAN.md:4282`, written while triaging a determinism bug's blast radius:

> `opt.OptimizeModule` sends any module over `moduleOptimizationFunctionBudget`
> (2048 functions) to `BoundedPipeline`, which is `fold`/`copy`/`dce` only. The
> smallest program in the goc corpus, `hello.go`, emits **2,739** functions, because
> every goc program links the runtime. **No goc module has ever run
> `DefaultPipeline`**, so `inline`, `mem2reg`, `jumpthread`, `ifconvert` and `gcm`
> are all `cg12cc` paths in practice.

That was recorded as good news -- it bounded how far a bug in those passes could
reach. Nobody connected it to what `goc -O` therefore costs at run time. It is the
same fact; this job supplies the other half of it, and the measurement.

It also means mem2reg has never run on a Go-frontend module in this tree, only on
`cg12cc`'s C modules. That is why the guard set below matters more than usual, and
in particular why `test-goc-status-opt` -- the only arm that passes `-O` -- is the
one to read. The default arm never calls `opt.OptimizeModule` at all and is a pure
control.

## What would fix the interpreter, precisely

Not attempted here -- it is a second, independent change with its own risk, and the
brief says the floor is worth more. Stated so it can be picked up:

**The defect.** `goc/compile.go:6576` (`storeInlineValue`) lowers *every* aggregate
value copy to a `goc_memcpy` call, whatever the size, and `goc/compile.go:6598`
(`storePointerAwareInlineValue`) does the same for the scalar runs between barriered
pointer words. Nothing anywhere expands a constant-size call back into instructions.
A 16-byte struct assignment therefore costs a function call; `in := program[pc]` in a
dispatch loop costs three (a `goc_memset` to zero the destination, a `goc_memcpy` into
an unnamed temporary, and a `goc_memcpy` out of it).

**The fix.** An expansion of `goc_memcpy`/`goc_memset` with a constant length at or
below a threshold (16 bytes is `ldp`/`stp`; 32 covers most Go struct assignment) into
load/store pairs. Where it has to go is constrained from both sides:

- *After* the front end, because `opt/escape.go:1171`, `opt/framecheck.go:742` and
  `opt/loopalias.go` all recognise these calls **by name** and model them as
  "reads/writes the memory it is given, retains no address". Expanding them earlier
  turns each into raw stores of pointer words into frame slots, which those analyses
  would read as publications. That would move `alloc_census_baseline.txt` and
  possibly `frame_escape_baseline.txt`. (The corpus audits compile *without*
  `opt.OptimizeModule` -- `goc/corpusaudit_test.go:127` -- so an optimizer-stage pass
  is invisible to them, which is the property to preserve.)
- *Before or during* arm64 lowering, and it must be checked against the precise stack
  maps: RUNTIME_PLAN.md records one bug already in this area ("the frame slot a call
  homes its aggregate result into was described as a GC root at that very call"). A
  copy that moves a pointer word with `ldr`/`str` instead of inside `goc_memcpy`
  changes which PCs have a pointer live in a register, which is what the stack map
  describes.

So: a `FuncPass` in `opt`, placed in both pipelines after everything else, or an
expansion in the arm64 instruction selector. Perhaps 150-300 lines plus tests.

**What it is worth.** Measured, by doing the equivalent transformation in the source
instead of the compiler: `interp/bytecode-loop` 646.5 ms -> 39.7 ms, i.e. the row goes
from 20.5x to **1.26x**. It should also move `json`, `sortmap`, `text` and `flate`,
all of which copy small structs and interface headers in their inner loops -- but that
is a prediction, not a measurement, and the honest thing is to measure it after
building it.

## The fix as committed causes one capability regression, and it is a real one

    make test-goc-status       (no -O, pure control)   366 PASS  0 FAIL  0 SKIP  0 cached
    make test-goc-status-opt   (-O, the arm that matters)  365 PASS  1 FAIL

    --- FAIL: TestARM64RuntimeCapabilityStatus/stdlib-netpoll-stress/tcp-churn
        cg12: interface dispatch failed for dynamic type 0x0
        fatal error: cg12: interface dispatch failure
        net_Listener_Accept()
        main_serveTCPStressConnection()

Not flaky, and it is mine. Rerun three times each, `-runtime-opt`, same command:

    tree                            tcp-churn at -O
    with mem2reg in the bounded pipeline   FAIL, FAIL, FAIL
    d2855f5 (pre-fix control)              ok, ok, ok

An interface value reaches a dispatch with a nil dynamic type. mem2reg has never run
on Go-frontend IR before (RUNTIME_PLAN.md:4282), so this is a latent bug in the pass
that only Go's two-word interface representation reaches. Triaging it now.

Other guards, all clean on this tree:

    TestFrameEscapeAudit          PASS  (unchanged, and structurally unaffected: the
    TestLoopAliasAudit            PASS   corpus audits compile without OptimizeModule)
    TestEscapeShadowPlacement     PASS
    determinism, 4 programs x {default, -O}, compiled twice each:
                                  8/8 byte-identical

## The blocker, characterised

The change is committed **off by default** (`GOC_BOUNDED_MEM2REG=1` turns it on),
because it is not correct yet. With the switch unset the compiler's output is
byte-identical to the pre-fix compiler's, checked on the interpreter program.

What is established about the miscompile, each point measured:

| question | answer | how |
|---|---|---|
| is it real, or flaky? | real | 3/3 fail with, 3/3 pass without, `-runtime-opt` |
| deterministic? | **yes at GOMAXPROCS=1** | 15/15 fail; 13/25 at default GOMAXPROCS |
| the garbage collector? | **no** | `GOGC=off` still fails 6/6; `gctrace=1` shows zero collections in this program either way |
| code placement? | **no** | pre-fix compiler survives `GOC_TEXT_PAD` 0..28, 0/24 failures |
| stack copying? | no evidence | the runtime's own `cg12 unadjusted stack pointer after copystack` audit never fires |
| mem2reg, or the cleanup after it? | **mem2reg** | with fold/copy/dce removed and only mem2reg run, still 3/3 fail |
| which functions? | **main.main + >=2 in package net** | promotion scoped by name and by a 1024-way hash of the name |

The scoping is the part that makes it hard. Every single-package restriction is
clean (`only=main.` 0/3, `only=net.` 0/3, `skip=runtime.` 3/3) and so is every
half and quarter of package net on its own; `net` buckets `[0,256) + [512,768)`
together with `main.main` fail 3/3, but splitting `[0,256)` in half makes both
halves clean. So at least three promoted functions have to be present at once.
That is a strange shape for an intraprocedural pass, and the shape is the clue:
the only thing that couples two functions here is the calling convention, and the
value that arrives wrong is a two-word interface. The next step is a delta-debug
(ddmin) over the ~729 promoted `net` functions to get the minimal set, then read
those functions' IR before and after promotion. Budget: an hour of machine time
for the reduction, plus the read.

## One correction to the brief's framing

"Every goc-compiled program is at least 1.63x the host toolchain" is not what the
suite measures. `control/spin-fixed-work` is **one program, byte-identical in all
eleven sources** (`goc/perfbench_test.go:57` and each `main.go`'s `control()`), so
eleven readings of 1.630-1.633 are eleven measurements of one loop, not eleven
unrelated programs agreeing. The consistency is not evidence of a universal
constant; it is evidence that the instrument is repeatable, which is what that row
is for.

The suite in fact contains rows at **1.008x** (`sha/sha256-1mib`, where both
binaries run the same hand-written assembly) and **1.01-1.03x** (`chase/dram`,
`chase/pointer-node`, memory-bound). So goc is not 1.63x everywhere.

What is true, and is what the fix acts on: **code goc generates itself, for a loop
over scalar locals, runs at about 1.63x** -- and that is a floor in the useful
sense, because most of the suite's other rows sit above it. Fixing it moves that
row to 0.926x.

## Guard results

### `make bench-perf`, before (switch off) -- reproduces the committed baseline

Every row lands on its committed value: the eleven control rows read 1.6317-1.6332
against a committed 1.6303-1.6331, `interp/bytecode-loop` 21.4712 against 21.4647,
`float/sqrt-sum` 171.2507 against 171.2179, `sha/sha256-1mib` 1.0077 against 1.0077.

The run exits non-zero on one row, and it is a noise-ceiling breach, not a ratio
move: `chase/pointer-node` one-repetition spread 34.7% against a 15% ceiling, with
its null spread at 33.8% -- i.e. the noise is on the goc side of a row whose true
ratio is 1.01. I had `make bench-crypto` running on core 63 at the same time,
which the suite's protocol allows but which visibly cost the quiet rows (`chase`
and `gcpress` are all louder than their committed spreads). The after-run below was
taken with the box otherwise idle.

### `make bench-crypto` -- already failing on `ccwork/perf-suite`, before this change

    Error: the program measures a case testdata/crypto_signing_bench_baseline.txt
           does not list  [p256/sign-verify, p256/verify, p384/sign-verify,
                           rsa2048/sign-verify]

All four cases, i.e. every case there is -- and the indices themselves are on their
committed values (45.96 vs 45.80, 33.40 vs 34.38, 39.88 vs 40.53, 12.31 vs 12.56).
The committed baseline's header says `tolerance: 0.04` and the run says `0.06`, and
the run's table carries two interval columns the file does not have: the perf-suite
branch rewrote `goc/cryptobench_test.go` (663 lines) and did not regenerate
`goc/testdata/crypto_signing_bench_baseline.txt`. Verified against the pre-change
tree below.

### The other guards

| guard | result | note |
|---|---|---|
| `make test-goc-status` (no `-O`) | **366 PASS / 0 FAIL / 0 SKIP**, 0 cached | pure control; `-O` off means `opt.OptimizeModule` is never called |
| `make test-goc-status-opt` (`-O`), switch **on** | **365 PASS / 1 FAIL** | `stdlib-netpoll-stress/tcp-churn`; this is why the switch exists |
| `make test-goc-status-opt` (`-O`), switch **off** | see below | the compiler is byte-identical to pre-fix with the switch off |
| `TestFrameEscapeAudit` | PASS | and structurally immune: the corpus audits compile without `opt.OptimizeModule` (`goc/corpusaudit_test.go:127`) |
| `TestLoopAliasAudit` | PASS | same |
| `TestEscapeShadowPlacement` | PASS (217 s) | same |
| determinism | **8/8 byte-identical** | 4 programs x {default, `-O`}, each compiled twice |
| `make bench-crypto` | FAIL, **pre-existing** | `1af81e1` rewrote `goc/cryptobench_test.go` (663 lines) and left `goc/testdata/crypto_signing_bench_baseline.txt` untouched; my branch touches neither file |

### Guards, completed

| guard | result |
|---|---|
| `make test-goc-status` (no `-O`) | **366 / 0 / 0**, 0 cached |
| `make test-goc-status-opt` (`-O`), switch **off** (the shipped default) | **366 / 0 / 0**, 0 cached, 121.6 s |
| `make test-goc-status-opt` (`-O`), switch **on** | 365 / **1** -- `stdlib-netpoll-stress/tcp-churn` |
| GC reducer (`runtime_gc_type_mask_padding`), `GOGC=10`, 20 runs | **0/20 failures** |
| GC reducer, default `GOGC`, 20 runs | **0/20 failures** |
| determinism, 4 programs x {default, `-O`}, compiled twice each | **8/8 byte-identical** |
| `TestFrameEscapeAudit` / `TestLoopAliasAudit` / `TestEscapeShadowPlacement` | **PASS** |
| `TestAllocationCensus` | **PASS** (216.6 s), census reproduces the committed baseline |
| compiler output with the switch off vs the pre-change compiler | **byte-identical** |

The GC reducer ran under `GOMAXPROCS=3`, 180 s timeout, exactly 40 runs, each
counted a pass only on exit 0 **and** the literal output `type mask padding ok`. The
box was not idle for it (1-minute load average 41 on 64 cores, another job's), which
matters for the reducer only in that a slower run is closer to its timeout; none
came near it.

## Verdict

**1. The root cause of the double indirection.** `goc -O` never runs mem2reg on a
Go program. `opt.OptimizeModule` sends any module past 2048 functions / 50000 blocks
/ 200000 instructions to `BoundedPipeline`, which is fold, copy and DCE and nothing
else; every whole-program Go build is past all three (5101 / 70160 / 297389 for a
168-line source), because the program module carries the stdlib closure the prebuilt
pack did not already hold. So every alloca-backed local stays in memory: `add x17,
x29, #imm` then `ldr` on every read and `str` on every write, with the loop-carried
dependence running through store-to-load forwarding. The tree already recorded half
of this at `RUNTIME_PLAN.md:4282` -- "no goc module has ever run DefaultPipeline" --
as a bound on a bug's blast radius, without connecting it to run-time cost.

**2. The floor and the interpreter row do not share a cause.** Promotion takes the
control loop from 1.632x to **0.926x** -- goc ahead of the host -- and takes the
interpreter row only from 21.46x to 20.46x. The interpreter's 21x is `in :=
program[pc]`, a sixteen-byte struct load that `goc/compile.go:6576` lowers to a
`goc_memset` plus two `goc_memcpy` calls per dispatched opcode where the host uses
one `LDP`. Doing that transformation in the source instead of the compiler takes the
row to **1.26x**, which is the size of that finding.

**3. The fix, and the numbers.** `BoundedPipeline` gains mem2reg, behind
`GOC_BOUNDED_MEM2REG=1`, off by default. Sixteen rows measured across four programs,
all sixteen improve: the floor 1.63 -> 0.93 in all four, `float/int-convert` 1.55 ->
1.00, `float/mandelbrot` 2.59 -> 1.32, `text/utf8-decode` 4.42 -> 2.52,
`map/build-probe` 7.57 -> 5.47, `text/sprintf` 10.30 -> 7.59. Compile cost is nil on
the five programs the budget exists for.

**4. Why it is off, and what would finish it.** Two distinct miscompiles, neither
of which mem2reg had ever been in a position to cause before, because it had never
run on a Go-frontend module:

  * `compress/flate` dies with the collector finding a pointer into a freed span
    (`runtime: pointer ... to unused region of span`, in `scanObject`). 3/5 with the
    collector on, **0/5 with `GOGC=off`**, 0/5 with the switch off. This one is a
    GC-visibility bug: a promoted managed pointer stops being described to the
    collector once it lives in a register. `opt/mem2reg.go` carries `managed` and
    `gcType` forward, so the loss is downstream, in what the arm64 backend records
    for a spilled promoted temp. **This is the one to fix first** -- it has a
    deterministic, single-purpose, network-free reproducer.
  * `stdlib-netpoll-stress/tcp-churn` dies with `interface dispatch failed for
    dynamic type 0x0`. Deterministic at `GOMAXPROCS=1`, and **not** the collector
    (that program runs no collection at all). Needs promotion in `main.main` and in
    at least two functions of package `net` at once; every single-package and
    single-quarter restriction is clean. Next step is a delta-debug over the ~729
    promoted `net` functions to get the minimal set.

Estimate: the flate/GC half is a contained problem in how a promoted managed value
is described to the stack map -- days, not weeks, and testable against the existing
GC differential and reducer. The tcp-churn half is unknown until the minimal set is
in hand; budget an hour of machine time for the reduction before estimating it.

**5. One thing the brief got wrong, and it matters for what to measure next.**
`control/spin-fixed-work` is one program compiled eleven times, not eleven programs
agreeing, and the suite has rows at 1.008x and 1.01x. goc is not 1.63x everywhere.
It is ~1.63x on code it generates for scalar loops, which is a floor in the useful
sense -- most other rows sit above it -- and which this change removes.
