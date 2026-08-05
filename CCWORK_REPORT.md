# Why creating and joining a goroutine costs 38x the host toolchain

Branch: `ccwork/goroutine-scheduler`, off `ccwork/perf-suite` = `d2855f5`, whose
`make bench-perf` produced the measurement this investigates. That report is
`git show d2855f5:CCWORK_REPORT.md`.

## The answer

`goroutine/spawn-join` is 38x because **`runtime.findfunc` is O(number of
functions in the image)** in a goc-built binary, and the goroutine path calls it
**twice per goroutine**.

cg12 emitted `moduledata.findfunctab` — the bucket table `findfunc` uses to start
its `functab` scan near the answer — as all zeroes. Every lookup therefore
started at index 0 and scanned forward. The program that produced these numbers
is 150 lines of Go and its module holds **5,406** functions, because it links the
whole runtime and the stdlib closure it reaches.
`runtime.newproc1` and `runtime.gdestroy` each call `findfunc`
once per goroutine through `isSystemGoroutine`, on a `startpc` that sits near the
end of the text, so each goroutine paid two nearly full scans: **24 µs** against
the host toolchain's **0.6 µs**.

It is a **linker-metadata** problem. Not a scheduler-algorithm problem:
`stdlib/src/runtime/proc.go`, `chan.go`, `lock_futex.go`, `malloc.go`,
`select.go` and `sema.go` are byte-identical to the host toolchain's `go1.26.1`
sources. Not a code-generation problem either: the emitted code for the
scheduler is no worse than goc's code anywhere else.

Fixed in `4bda6d7`, by building the table.

| | before | after | host | ratio before | ratio after |
|---|---:|---:|---:|---:|---:|
| `goroutine/spawn-join` (20k spawn+join) | 471.6 ms | 63.7 ms | 12.1 ms | **38.9x** | **5.3x** |

The other three `conc` rows do not move, which is the control on the change:

| case | before | after | host |
|---|---:|---:|---:|
| `control/spin-fixed-work` | 54.67 ms | 54.57 ms | 33.50 ms |
| `chan/pingpong-unbuffered` | 99.64 ms | 99.87 ms | 15.34 ms |
| `chan/send-buffered` | 40.66 ms | 40.51 ms | 8.49 ms |
| `mutex/uncontended` | 43.63 ms | 43.65 ms | 23.45 ms |

## Where the channel rows sit, as asked

They did not move, because they were never in this path: a channel operation
does not create or destroy a goroutine, so it never calls `isSystemGoroutine`.

    chan/send-buffered          4.81x     unchanged
    chan/pingpong-unbuffered    6.49x     unchanged
    goroutine/spawn-join       38.45x ->  ~5.3x

After the fix goroutine creation sits *between* the two channel rows rather than
six times above them, which is the shape the workload should have had: a
`pingpong` round trip is two sends, two receives and two goroutine switches, and
it costs more than one spawn+join.

## How this was established

### 1. Reproduced

    goc:   goroutine/spawn-join  482.6 ms   (24.1 µs per goroutine)
    host:  goroutine/spawn-join   11.9 ms   ( 0.6 µs per goroutine)

40.6x on a single pinned run against the suite's committed 38.45x.

Splitting the case apart shows the cost is the goroutine and not the WaitGroup:

| variant | goc | host |
|---|---:|---:|
| `go func(){...}` + `WaitGroup` | 23.6 µs/op | 0.60 µs/op |
| spawn one, join it, repeat | 25.3 µs/op | 0.68 µs/op |
| the identical body called **inline**, same `WaitGroup` traffic | 0.066 µs/op | 0.019 µs/op |

### 2. It is not syscalls

`strace -c -f` over 2,000 spawn-and-join goroutines:

    goc     297 syscalls total, of which 4 futex, 3 clone, 29 mmap
    host    262 syscalls total, of which 10 futex, 3 clone, 23 mmap

No per-goroutine futex, no per-goroutine `mmap`, no per-goroutine anything. The
park/ready path named in the brief as a candidate is not involved: the whole
25 ms of goc time is user space.

### 3. Profiled

`perf` is unusable on this box (`perf_event_paranoid` is 4) and `runtime/pprof`
does not link under goc — `undefined reference to runtime_pprof_StartCPUProfile`
— so the profile was taken with a ptrace sampler written for the job
(`PTRACE_ATTACH` per thread, `PTRACE_GETREGSET`/`NT_PRSTATUS` for the PC,
symbolised against the binary's ELF symbol table; the sampler has to be the
target's parent because `yama/ptrace_scope` is 1).

4,359 samples over the 20k-spawn loop. Three threads run, so the two idle ones —
`sysmon` in `usleep` and a parked M in `futex` — take two thirds of the samples
by construction. Of the working thread's third:

    33.31%  runtime_usleep     <- sysmon, idle
    33.33%  runtime_futex      <- parked M, idle
    29.16%  runtime_findfunc   <- 87% of the one thread doing the work
     0.67%  goc_memcpy
     0.30%  goc_memset
     0.12%  runtime_gfget
     0.12%  runtime_isSystemGoroutine
     0.09%  runtime_findRunnable
     0.07%  runtime_newproc1
     0.07%  runtime_execute

The whole `isSystemGoroutine` chain is visible under it: `findmoduledatap`,
`moduledata_textOff`, `funcname`, `gostringnocopy`, `findnull`,
`stringslite_HasPrefix`. `findRunnable`, `runqget`, `runqput`, `schedule` and
`execute` together account for **11 samples out of 4,359** — the scheduler proper
is not where the time is.

### 4. The mechanism, proved independently of the profile

`runtime.FuncForPC` is `findfunc` with a wrapper, so the cost can be measured
from user code at chosen PCs. Sweeping the module's whole text:

| pc − minpc | goc, before | host |
|---:|---:|---:|
| 0 | 644 ns | 33 ns |
| 549,882 | 3.74 µs | 22 ns |
| 1,099,764 | 5.62 µs | 119 ns |
| 1,649,647 | 7.46 µs | 22 ns |
| 2,199,529 | 9.10 µs | 22 ns |
| 2,749,412 (`main`) | 10.79 µs | 23 ns |

A straight line in the PC's rank on one side, flat on the other. That is a linear
scan, and the source says so in as many words —
`internal/gometa/gometa.go`, before this branch:

> cg12 never populates the table (every bucket is zero and findfunc falls back to
> a linear scan of functab from index 0), so its only requirement is to be long
> enough to index.

Two scans of ~10.8 µs each is the 24 µs the goroutine case pays; `newproc1` at
`stdlib/src/runtime/proc.go:5358` is one and `gdestroy` at `:4509` is the other.

## Is it a compilation problem or a runtime-algorithm problem?

Neither, and both alternatives were checked rather than assumed.

- **Not the runtime algorithm.** The vendored scheduler is upstream's, byte for
  byte: `diff go1.26.1/src/runtime/{proc,chan,lock_futex,malloc,select,sema}.go`
  against `stdlib/src/runtime/` is empty. `newproc`, `findRunnable`, the run-next
  slot, park/ready and the g free lists are the host's code.
- **Not code generation.** goc's code quality is a real and separate problem —
  the perf suite's own report measures the 1.63x floor on a bare loop and 21x on
  an interpreter — but it is not what makes this row 38x. If it were, the row
  would have moved with a code change; it moved with a **data** change, and the
  two binaries either side of the fix have byte-identical text layout (same
  `minpc`, same `maxpc`, same function count).
- **It is the metadata.** One table, emitted as zeroes.

The residue after the fix — 5.3x, against the suite's 1.63x floor — *is* the
code-quality problem, and it is spread out rather than concentrated. Re-profiled
after the fix (percentages of all samples, of which two thirds are the two idle
threads, so multiply by three for the working thread's share): `goc_memcpy` 4.8%
and `goc_memset` 2.3% — a goroutine's `g` and its stack being allocated and
cleared — `findfunc` down from 29.16% to **0.66%**, and nothing else above 1.2%.

## What was changed

`internal/gometa`: build the table instead of emitting zeroes.
`internal/gometa/builder.go` is a one-line change; the algorithm is
`FindFuncTab` in `gometa.go`, which is upstream `cmd/link`'s
`writeFindfunctab` adapted to what cg12 knows at object-emission time.

Nothing under `stdlib/src/runtime/`, `arm64/`, `ir/`, `opt/`, `lower/`, `link/`
or `cmd/` is touched. **No scheduler change, no runtime change, no code
generation change.**

### Why this is safe, in the terms the table itself sets

`runtime.findfunc` reads

    idx := bucket.idx + uint32(bucket.subbuckets[sub])
    for ftab[idx+1].entryoff <= pcOff { idx++ }

so the stored index is a **lower bound** and the error is one-sided: an index
below the true one is corrected by the scan and costs steps, an index above it
returns the wrong function. Every choice in the new code keeps to the low side —
the clamp when a bucket spans more than 256 functions, the fill for a subbucket
no function starts in, and the buckets past the last function the object defines.
Leaving the table zero, as before, was the extreme of that same safe side.

The one thing the code assumes is that a function's offset in the object equals
its final address minus a single common base — i.e. that the linker places the
object's single `.text` input section contiguously. That was measured, not
assumed: across all **5,210** text symbols of `goc.o` in a linked binary, the
object-offset-to-final-address delta takes exactly **one** distinct value
(`0x400640`). Functions the object does not define — on arm64 the translated
Plan 9 sidecar, linked as a second object — have no offset here; they sort last
and land above every one of `goc.o`'s (measured: `goc.o` text ends at
`0x6e9788`, the sidecar spans `0x6e9830`–`0x6eefd0`), and their buckets get the
last known function's index, which is a lower bound for every PC above it.
`runtime.moduledataverify1` already throws on a `functab` that is not in
ascending entry order, so that ordering is checked at startup rather than taken
on trust.

### Correctness evidence for the table itself

A sweep of **every 4th byte** of the module's 3.06 MB text — 766,244 lookups —
asserting `FuncForPC(pc).Entry() <= pc` and that the answer never moves
backwards. That is exactly the failure an over-large bucket index causes.

    populated table   checked 766244 pcs, 5406 distinct functions, 0 violations
    zero table (ref)  checked 766244 pcs, 5406 distinct functions, 0 violations

and, differentially, a 64-bit FNV chain over every one of those 766,244 answers:

    populated table   checksum 0x9bdeff57cbfb27ac
    zero table (ref)  checksum 0x9bdeff57cbfb27ac

The zero-table build is the reference implementation — an unconditional linear
scan — and the two agree on every PC in the image.

### The change touches one table and nothing else

The same program built either side of the fix, compared byte for byte:

    same size          10,628,776 bytes both
    differing bytes    270,438
      inside .goc.go.findfunctab   270,418
      .note.gnu.build-id            20   (a hash of the image)
      .text                          0

Not one instruction changed, which is why the other three `conc` rows sit still
and why the triage note's step 2 — compare encoded instruction words — is
answered before it is asked.

## What else was on this path

`findfunc` is not only the goroutine path. Every caller of it in a goc-built
binary was paying the same scan:

- **GC stack scanning.** `runtime.unwinder.init`/`next` calls `findfunc(frame.pc)`
  once per frame, and `scanframeworker` needs the `funcInfo` it returns to reach
  the frame's stack maps. Every frame of every goroutine, every cycle.
- **Traceback, panic and `Goexit`.** Same unwinder.
- **`runtime.Caller`, `runtime.Callers`, `runtime.FuncForPC`.**
- **`gopanic`/`deferreturn`** reaching for a frame's `_func`.

So the suite's `gcpress` rows and anything that panics were on it too. What that
is worth is in the full-suite numbers below, which is the honest way to say it:
this was not measured by reasoning about the call graph.

## What remains, and what would fix it

After the fix `goroutine/spawn-join` is ~5.3x, against the suite's measured
**1.63x floor** on a bare loop. The residue is 3.2 µs a goroutine against the
host's 0.6 µs, and it is *not* one hotspot: re-profiled, the largest single
entries are `goc_memcpy` at 4.8% of all samples and `goc_memset` at 2.3% — a
goroutine's `g` and its 2 KB stack being allocated and cleared — `findfunc` at
0.66%, and nothing else above 1.2%. Two thirds of every sample count here is the
two idle threads, so those are roughly 14% and 7% of the thread doing the work.

That is the general code-quality problem the perf suite already documents, and it
has a known cause on record: `goc -O` exceeds its own optimization budget on
every real program (`f1f7abf`: `funcs=5101 blocks=70160 instrs=297389` against
caps of `2048/50000/200000/400000`), so `-O` degrades to fold+copy+DCE, every
alloca-backed local stays in memory, and nothing stays in a register across a
call. Raising or scoping that budget is the fix for the residue, and it is a much
larger and riskier change than this one. It is not a scheduler change either.

**No scheduler change is called for.** The scheduler in this tree is upstream
Go's, unmodified, and the profile puts 11 samples out of 4,359 in it.

## `make bench-perf`, before and after

The full suite against its committed baseline, nine interleaved core-pinned
repetitions, `go1.26.1 linux/arm64`, on the tree with the fix. It **failed in the
faster direction on two rows and moved nothing else**:

    program   case                      baseline  this run   change   resolved     tol verdict
    conc      goroutine/spawn-join       38.4529    5.3188   -86.2%     +82.9%   12.8% PAST TOLERANCE
    gcpress   gc/alloc-churn             11.6967    9.7386   -16.7%     +14.2%    9.2% PAST TOLERANCE

`goroutine/spawn-join` is the row this branch went after: **38.45x → 5.32x**, a
7.2x speedup, 471.6 ms → 65.5 ms on 20,000 spawn-and-joins.

`gc/alloc-churn` is the one that was not aimed at and is the more interesting
confirmation: **11.70x → 9.74x**. That row's cost is dominated by collections,
and a collection scans every frame of every goroutine stack through
`findfunc(frame.pc)`. It moved because the same scan was in it.

Every other row held. All eleven copies of the control loop:

    baseline  1.6303 1.6307 1.6322 1.6322 1.6322 1.6322 1.6322 1.6326 1.6327 1.6330 1.6331
    this run  1.6281 1.6291 1.6291 1.6292 1.6292 1.6292 1.6294 1.6294 1.6296 1.6302 1.6307

and the three `conc` rows that share the program with the one that moved:

    conc      chan/pingpong-unbuffered    6.4916    6.5201    +0.4%   within tolerance
    conc      chan/send-buffered          4.8052    4.7802    -0.5%   within tolerance
    conc      mutex/uncontended           1.8632    1.8616    -0.1%   within tolerance
    conc      control/spin-fixed-work     1.6322    1.6294    -0.2%   within tolerance

The null arm read 0.9936–1.0118 across all 42 rows, so the protocol is honest for
this run. `flate` died once in 28 runs (3.6%) and was retried — the known
pre-existing defect the suite is built to survive, well under its 20% bar.

### The instrument asked the right question, and here is the answer

The failure message for a faster-than-baseline row says, correctly, that the
cheap way to get faster is to stop heap-allocating something, and that an escape
analysis which got *permissive* looks identical from here. It asks for the
allocation census diff.

The census does not move, and it cannot: the two binaries' `.text` is
byte-identical (see above), so no allocation decision changed. `TestAllocationCensus`
is in the guard table below.

## Guards

All run on the final tree, after the fix and the re-baseline had landed.

| guard | result |
|---|---|
| `make test-goc-status` (default arm) | **366 subtests, 0 failures, 0 skips** — 365 `PASS` plus the one capability whose expectation is `EXPECTED FAILURE` (`runtime_panic_print_string.go`), 100.4 s |
| `make test-goc-status-opt` (`-O` arm) | **366 subtests, 0 failures, 0 skips**, same split, 93.3 s |
| `TestAllocationCensus` | **ok**, 326.9 s — `alloc_census_baseline.txt` reproduces **unchanged**, `git diff` on it is empty |
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` | **ok**, 8.7 s |
| GC reducer (`runtime_gc_type_mask_padding`), default `GOGC`, `GOMAXPROCS=3`, 20 runs | **0/20 failures** |
| GC reducer, `GOGC=10`, `GOMAXPROCS=3`, 20 runs | **0/20 failures** |
| `TestFrameEscapeAudit` | **ok**, 327.6 s |
| `make bench-perf` after the re-baseline | the update run itself reported **PASS** on all 42 rows, 591 s |
| working tree | only `goc/testdata/perf_suite_baseline.txt` changed by the benchmark, and it is committed |

A reducer run counted as a pass only on exit 0 **and** the literal output
`type mask padding ok`; the script is in the branch history of this report.

Per the brief, `go test ./goc/...` and `make test-unit` were not run. The census,
determinism and frame-escape guards were invoked with an explicit `-run` on the
single test, which is how `make bench-crypto` is itself defined.
`go test ./internal/gometa/` — a package neither of those two commands covers —
was run, and its seven new tests plus the three existing bucket-count tests pass.

The capability matrix is the guard that matters most here, because `findfunc` is
what the GC reads a frame's stack maps through: a table that pointed one function
too far would hand the collector the wrong pointer map, and 366 programs covering
goroutines, channels, panics, defers, finalizers and GC stress are what would
show it.

## The line

The 38x is `runtime.findfunc` scanning `functab` from index 0 on every lookup,
because cg12 emitted `moduledata.findfunctab` as zeroes — twice per goroutine,
through `isSystemGoroutine` in `newproc1` and `gdestroy`. It is fixed by building
the table. `make bench-perf`: **`goroutine/spawn-join` 38.45x → 5.32x**, and
`gc/alloc-churn` 11.70x → 9.74x for free, with nothing else moved and every guard
green.
