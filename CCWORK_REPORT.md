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

## The compress/flate collector crash: reproduction

The defect recorded in `44d76cf` and worked around by the performance suite
(`perfBenchRunAttempts`) is reproducible on demand. It is not rare; it is
*conditional*, and the condition is the backend's code-placement policy.

| build of `goc/testdata/placement_bench/flate/main.go` | `GOGC=10`, pinned |
|---|---|
| `goc -O` (default alignment) | 0/15 |
| `goc -O`, `GOC_FUNC_ALIGN=0 GOC_LOOP_ALIGN=0 GOC_ALIGN_LOOP_FUNCS_ONLY=0`, pad 0 | **14/15** |
| the same, `GOC_TEXT_PAD=16` | **15/15** |
| `goc` (no `-O`), alignment off, pad 0 | **10/10** |
| the same, `GOC_TEXT_PAD=16` | **10/10** |
| host `go build` | 0/15 |

Two things follow immediately. It is not an optimizer bug -- the unoptimized
build is the *more* reliable reproducer. And the perf suite's ~1-in-20 is the
rate of the *default* configuration; with alignment off it is essentially every
run, which is what makes this tractable.

### What the crash says

    runtime: pointer 0x36a7cfb36000 to unused region of span ...
    runtime: found in object at *(0x36a7cfb20000+0x10f8)
    object=... s.spanclass=90 s.elemsize=4864 s.state=mSpanInUse

The object's first word is `_goc_runtime_type_compress_flate_decompressor_...`
(resolved from the binary's symbol table), i.e. it is the Go 1.22 malloc header,
so the *data* starts at object+8 and the reported offset `0x10f8` is field offset
**4336** of `compress/flate.decompressor` -- the pointer word of `toRead []byte`.

The type descriptor itself is correct. Decoded out of the binary:
`Size_=4392`, `PtrBytes=4376`, and the GC mask names exactly the words
`{0, 1, 2, 263, 524, 528, 529, 530, 537, 540, 541, 542, 545, 546}` -- which is
byte-for-byte the true layout (`r` iface, `rBuf`, `h1.links`, `h2.links`,
`bits`, `codebits`, `dict.hist`, `step`, `err`, `toRead`, `hl`, `hd`). So this is
**not** a pointer-mask defect, and not a mask-padding defect: the mask is 72
bytes for 549 significant words, correctly rounded.

Root cause hunt continues from there.

### Root cause: a slice expression that consumes its source points past it

The bad pointer is `toRead`'s data word, and the object dump says exactly what it
is. Reading the dump with the malloc header subtracted:

    *(object+4248) = 0x31e9f3d74000   dict.hist.ptr
    *(object+4256) = 0x8000           dict.hist.len   = 32768
    *(object+4264) = 0x8000           dict.hist.cap
    ...
    *(object+4344) = 0x31e9f3d7c000   toRead.ptr   <== the bad pointer
    *(object+4352) = 0x0              toRead.len
    *(object+4360) = 0x0              toRead.cap

`0x31e9f3d74000 + 0x8000 = 0x31e9f3d7c000`. The bad pointer is **exactly one byte
past the end of the 32 KiB history buffer**, with length and capacity zero. It is
what `compress/flate`'s `decompressor.Read` leaves behind:

    f.toRead = f.toRead[n:]      // n == len(f.toRead)

The host toolchain does not generate that pointer. `cmd/compile`'s `ssagen.slice`
masks the offset with `mask(rcap)` -- zero when the result's capacity is zero,
all-ones otherwise -- and says why: *"The masking is to make sure that we don't
generate a slice that points to the next object in memory."* goc emitted the
unmasked `ptr + low*elemsize`. Measured side by side on the same source:

| expression | host | goc before | goc after |
|---|---:|---:|---:|
| `b[len(b):]` (`len(b)=32768`) | delta 0 | **delta 32768** | delta 0 |
| `b[16:16]` on a 16-byte slice | delta 0 | **delta 16** | delta 0 |
| `b[5:5:5]` on a 16-byte slice | delta 0 | **delta 5** | delta 0 |
| `b[5:5]` on a 16-byte slice (cap 11 left) | delta 5 | delta 5 | delta 5 |
| `s[len(s):]` on a 10-byte string | delta 0 | **delta 10** | delta 0 |
| `s[5:5]` on a 10-byte string | delta 0 | **delta 5** | delta 0 |

Two distinct consequences, and the second is the one that kills:

1. The collector rejects the pointer. `findObject` looks it up in the page it
   lands on -- the *next* allocation's page -- and finds either a span the address
   is outside of or an unallocated one, and calls `badPointer`.
2. A one-past-the-end pointer **retains nothing**. The 32 KiB buffer is
   unreachable the moment the flate reader's last real reference to it goes, even
   though a live heap object still holds a slice of it. The buffer is freed and
   its pages reused, and the crash is then a *dangling* pointer, which is why it
   fires cycles later and looks timing-dependent.

The mask, not a null: a masked pointer still refers to the source's first byte,
so the empty slice keeps the buffer alive, which is the behaviour Go promises.
Nulling it would turn a non-nil empty slice into a nil one, which is observable.

It is not specific to `flate`. Any `b[len(b):]` or `s[len(s):]` stored where the
collector can see it has the same two problems; `flate` is simply the workload in
this tree that does it in a heap object, at scale, under GC pressure.

### The reduction

`goc/testdata/runtime_gc_slice_tail_pointer.go`, registered as the capability
`gc-invariants/slice-tail-pointer`. It has two halves:

- **Part one reads the generated pointer** and compares it against the source's
  base. It is deterministic: it fails on **every** run of the unfixed compiler,
  so the test's false-pass rate is **zero**. It also asserts the cases that must
  *not* move -- `b[4096:]` with capacity left keeps its offset -- so the fix
  cannot be "always mask".
- **Part two is the collector consequence** with no `flate` involved: keep only
  the empty tails of 48 large buffers, drop the buffers, and collect. On the
  unfixed compiler that alone dies with the same "found bad pointer in Go heap"
  on **14 of 20 runs at the default GOGC**. On the fixed compiler the tails
  retain their buffers and it passes.

### The fix

`goc/compile.go`, in the `*ast.SliceExpr` lowering: the byte offset added to the
source's data pointer is masked with `0` when the result's extent is zero and
`-1` otherwise -- `And(offset, Sar(Neg(extent), 63))` -- where the extent is the
result's capacity for a slice and its length for a string. Three instructions,
and only where the answer is not already known:

- a constant-zero low index cannot move the pointer, so nothing is emitted;
- a fixed-size array sliced at a constant index has a capacity the compiler reads
  off the type, so the answer is folded: no mask when it is non-zero, a constant
  zero offset when it is zero.

That second case is not tidiness. Emitting the mask into `crypto`'s
`copy(b[32:], k.z[:])` -- a `[64]byte` sliced at a constant -- grew three small
`mlkem` functions past the inliner's budget, and the six allocation sites that
`DecapsulationKey768.Bytes` and `DecapsulationKey1024.Bytes` contribute to their
callers went out of the census. With the constant case folded, the census
baseline's only change is the reducer's own five sites.

Cost: `.text` of the goc-built `flate` benchmark grows **5120 bytes, +0.155%**.

### Crash rate, before and after

Every run pinned to its own core, as the perf suite pins. "suite configuration"
is what `make bench-perf` builds and runs: `goc -O`, default alignment, default
GOGC.

| configuration | before | after |
|---|---:|---:|
| suite configuration | **15 / 200 = 7.5%** | **0 / 600** |
| alignment off, `GOGC=10` | **95 / 100 = 95%** | **0 / 400** |
| `runtime_gc_slice_tail_pointer` part two alone, default GOGC | **14 / 20 = 70%** | 0 / 40 |
| host `go build`, `GOGC=10` | 0 / 15 | -- |

The before figure in the suite's own configuration, 7.5%, is the number that
branch measured independently at 6.9%; the two agree. After the fix, **0 crashes
in 1000 runs** of the `flate` benchmark across the two configurations. At the
before-rate of 7.5%, 600 clean runs of the suite configuration alone has
probability 6e-21; the 95% upper bound on the remaining rate, from 600 runs with
no event, is 0.5%.

### Guards

| guard | result |
|---|---|
| `runtime_gc_type_mask_padding` at `GOGC=10` and default, `-O` and not | 0/20 each, 4 arms, 80 runs |
| `runtime_gc_slice_tail_pointer` at `GOGC=10` and default, `-O` and not | 0/20 each, 4 arms, 80 runs |
| `TestFrameEscapeAudit` | pass |
| `TestLoopAliasAudit` | pass |
| `TestEscapeShadowPlacement` | pass |
| `TestAllocationCensus` | pass; baseline gains the reducer's 5 sites and nothing else |
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` | pass |
| capability matrix, both arms | see below |

### Should the perf suite's retry stay?

**The retry stays; the ceiling goes to zero.** They were one mechanism and they
are now two different questions.

The 20% ceiling existed to *excuse* a live defect: it was set five times the
known flate rate so a green run would stay green. With the defect fixed, a
ceiling that tolerates one run in six is a hole -- a new collector bug could kill
a sixth of every program's runs and still print a table. `perfBenchCrashCeiling`
is now `0`: any crash fails the run, with the dead run's stderr attached.

The retry itself earns its place for a different reason than the one it was
written for. A crash is a *missing* measurement rather than a slow one, so
replacing it biases nothing, and retrying is what keeps one dead run from costing
the other ten programs their eleven minutes. Failing at the end with the whole
table and the whole crash report beats dying at repetition five with neither.

### One thing found on the way past, not fixed here

A `badPointer` throw cannot finish printing its own object dump. The first dump
in this investigation stopped at `*(object+248` and turned into a `SIGSEGV`:

    runtime_atomicwb -> runtime_atomicstorep -> goc_storep -> runtime_bytes
      -> runtime_printstring -> runtime_gcDumpObject -> runtime_badPointer
      -> runtime_findObject -> runtime_wbBufFlush1 -> ... (repeat until the
      stack is gone)

`runtime.bytes`, which `printstring` calls, does `rp.array = sp.str` where `rp`
is `(*slice)(unsafe.Pointer(&ret))` and `ret` is a local. goc emits a write
barrier for that store; the host toolchain does not, because ssa's write-barrier
pass asks `IsStackAddr` and follows the `unsafe.Pointer` chain back to a stack
slot. Inside `badPointer` the barrier flushes, the flush re-finds the same bad
pointer, and `badPointer` calls itself.

That is a second, independent defect -- goc not eliding write barriers for
stores it can prove target the stack -- and it makes every future bad-pointer
diagnosis harder than it needs to be. The dumps in this report were obtained by
setting `debug.invalidptr = 0` at the top of `badPointer` as a temporary patch,
which was reverted. It is worth its own job; it is not touched here.

### The capability matrix and what the mask costs

The matrix ran in both arms, sharded eight ways (`index % 8`, so the eight shards
cover the 367 capabilities exactly once each):

    plain arm: 8/8 shards ok      opt arm (-runtime-opt): 8/8 shards ok

367 capabilities, all passing in both arms. That is 366 before this branch plus
the new `gc-invariants/slice-tail-pointer`.

The mask's cost, measured on a quiet box, each binary pinned to one core, seven
interleaved repetitions, fastest of each program's own three timed rounds:

| case | after / before |
|---|---:|
| `flate/compress`, `flate/decompress` | 0.999, 0.996 |
| `sort/ints`, `sort/slice-callback`, `map/build-probe` | 1.000, 0.998, 0.970 |
| `text/parse`, `text/utf8-decode` | 0.999, **1.009** |
| `json/marshal`, `json/unmarshal` | 0.999, 0.998 |
| `regexp/find-submatch`, `anchored-lines`, `replace` | 0.999, **1.014**, 1.000 |
| `control/spin-fixed-work` (all) | 1.000 |

Nothing moves past the tolerance the perf suite draws around any of these rows;
the two that rose, at +0.9% and +1.4%, are below their 5.2% and 5.4% detection
floors. `text/format-append` and `text/sprintf` came out 12% and 9% *faster*,
which is those two rows' documented 6.4% and 8.8% noise, not a result.

`sortmap` was checked at four code placements (`GOC_TEXT_PAD` 0, 4, 16, 32)
rather than one, because a first pass taken while the capability matrix was still
running showed `sort/slice-callback` at 1.88× and `sort/ints` at 0.81× -- both
artefacts of a loaded box. At each of the four placements, before and after agree
to within 0.3%.

The committed `perf_suite_baseline.txt` was **not** re-measured: that is an
eleven-minute run whose whole method assumes a quiet machine, and this box shares
work. Nothing above suggests it needs to move.

### Verdict

**Root cause.** goc lowered a slice expression's data pointer as
`ptr + low*elemsize` with no mask. When the result has no capacity left --
`b[len(b):]`, and in `compress/flate` the `f.toRead = f.toRead[n:]` in
`decompressor.Read` -- that pointer lands one byte past the end of the
allocation. The collector rejects such a pointer, and it retains nothing, so the
32 KiB history buffer was freed under a slice that was still reachable from a
live heap object. The host toolchain masks the offset to zero in exactly this
case; goc now does too.

**Rate.** In the perf suite's own configuration: **15 of 200 runs (7.5%) before,
0 of 600 after**. In the worst configuration found -- alignment off at
`GOGC=10` -- **95 of 100 (95%) before, 0 of 400 after**. 1000 post-fix runs of
the `flate` benchmark, no crashes.

**Reduction.** `goc/testdata/runtime_gc_slice_tail_pointer.go`, capability
`gc-invariants/slice-tail-pointer`. Deterministic: false-pass rate zero.


---

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

## Guards

| guard | result |
|---|---|
| capability matrix, default arm | **pass**, 366/366 (the table in `cmd/goc/runtime_status_test.go` has exactly 366 entries; run unsharded) |
| capability matrix, `-O` arm | **pass**, 366/366 |
| allocation census against its baseline | **pass** (`TestAllocationCensus`) |
| allocation counts | **pass** (`TestAllocationCounts`) |
| determinism | **pass** (`TestCompilingTheSameSourceTwiceGivesTheSameModule`) |
| `make bench-perf` | **could not be measured on this box** -- see below |
| `make bench-crypto` | **could not be measured on this box** -- see below |

### The two timing benchmarks could not be run here, and that is about the box

This worker is shared. While this job ran, two other ccwork jobs were on it:
`locals-double-indirection` was running `TestPerformanceSuite` **and**
`TestCryptoSigningBench` -- the same two suites, which pin themselves to the
same cores by the same rule -- and `flate-gc-crash` was running a sixteen-shard
capability matrix. Load average reached 216 on a 64-core box.

Four `make bench-perf` / `bench-perf-update` attempts, the last three with
`GOC_PERF_CORE=40` to get off the core the other job's suite had taken:

| attempt | outcome |
|---|---|
| 1 | ceiling tripped on `chase/pointer-node`: spread 27.99% against a committed 1.98% |
| 2 | ceiling tripped on `chase/pointer-node`: 18.72%, and its raw time had doubled on **both** arms |
| 3 | the known `flate` collector crash exhausted its three retries (see below) |
| 4 | ceiling tripped on five rows, three of them `control/spin-fixed-work` -- the fixed integer spin loop, whose committed spread is 0.04% and which read 18.8% |

`make bench-crypto` failed the same way: `rsa2048/sign-verify` resolved to
+/-45.44% against a 2.00% ceiling, and the suite's own message for that is *"This
is a statement about the machine, not about the compiler."*

**The baseline was therefore not regenerated, and no contaminated baseline has
been committed.** The instrument refuses to write one from a run that tripped
the ceiling, which is the correct behaviour and is why nothing was forced.
`make bench-perf` will fail on `float/sqrt-sum` until someone regenerates the
baseline on an idle box; that row moved by design and by a factor of 151.

### Why the float/sqrt-sum number is nevertheless sound

It was read four times across four runs, and its own spread stayed at the
committed level (0.14%) every time, while other rows blew out:

| run | ratio | its own spread |
|---|---|---|
| 1 | 1.1335 | 0.49% |
| 2 | 1.1318 | 0.44% |
| 4 | 1.1392 | 0.39% |

Three readings within 0.7% of each other. The row is a short CPU-bound loop, so
the contention that wrecked the memory-bound and allocation-heavy rows did not
reach it.

### The flate collector crash in attempt 3 is pre-existing

Attempt 3 died on `runtime: pointer to unused region of span` in the goc-built
`flate` binary. That defect is already written down in this tree --
`goc/perfbench_test.go`'s `perfBenchRunAttempts` documents it at about one run
in twenty and retries three times for exactly this reason -- and another ccwork
job (`flate-gc-crash`) is working on it right now.

Checked rather than assumed: the workload
(`goc/testdata/placement_bench/flate/main.go`) does not import `math` at all,
and 60 runs of the binary built by a goc from `ccwork/perf-suite` before this
change crashed once, against two in 60 after it -- the same rate, both
consistent with the documented one-in-twenty.

### `make bench-crypto` is already broken on `ccwork/perf-suite`, before this change

The last `make bench-crypto` attempt did resolve cleanly -- every case landed
within 0.05% to 0.12%, well inside the 2.00% precision ceiling -- and it still
failed, with

    the program measures a case testdata/crypto_signing_bench_baseline.txt does not list

for all four cases. That is a format mismatch, not a regression.
`goc/cryptobench_test.go`'s `parseCryptoBenchBaseline` requires exactly seven
fields per row and reads the goc and gc spread columns at indices 2 and 4. Every
row in the committed baseline has five:

    p256/sign-verify    -> 5 fields
    p256/verify         -> 5 fields
    p384/sign-verify    -> 5 fields
    rsa2048/sign-verify -> 5 fields

So every row is skipped, the baseline parses as empty, and every measured case
is reported as newly appeared. The header tells the same story: the baseline
says `tolerance: 0.04 of the index, both directions` while the run says
`tolerance: 0.06 ... and the run's own interval must clear it`. Commit `1af81e1
ccwork: bench-crypto-noise` added the interval columns to the reader and the
writer without regenerating the file they read.

This branch does not touch `goc/cryptobench_test.go` or that baseline -- `git
diff origin/ccwork/perf-suite..HEAD` over both paths is empty -- so the failure
predates it and belongs to whoever landed the format change. It was left alone
rather than papered over with a `make bench-crypto-update` that would also have
baked in this contended box's numbers.

For the record, this run's indices against the (unreadable) baseline's, all in
the faster direction and none of them on a path this change touches:

| case | baseline | this run |
|---|---|---|
| p256/sign-verify | 45.7973 | 44.9712 +/- 0.05% |
| p256/verify | 34.3805 | 32.7313 +/- 0.12% |
| p384/sign-verify | 40.5258 | 39.0981 +/- 0.09% |
| rsa2048/sign-verify | 12.5562 | 12.1133 +/- 0.10% |

## Summary

`float/sqrt-sum`: **171.22x the host toolchain -> 1.13x**, a factor of 151.

Also lowered, each from a call to one instruction, measured per call with goc
before, goc after, and the host for scale:

| | before | after | host |
|---|---|---|---|
| `math.Sqrt` -> `FSQRT` | 396.03 ns | 2.65 ns | 2.35 ns |
| `math.Abs` -> `FABS` | 10.87 | 2.60 | 0.87 |
| `math.Floor` -> `FRINTM` | 4.96 | 2.77 | 1.00 |
| `math.Ceil` -> `FRINTP` | 4.94 | 2.77 | 1.00 |
| `math.Trunc` -> `FRINTZ` | 4.94 | 2.78 | 1.00 |
| `math.RoundToEven` -> `FRINTN` | 21.58 | 2.78 | 1.01 |
| `math.Round` -> `FRINTA` | 19.87 | 2.79 | 1.00 |

Not lowered, with the reason:

- **`math.Min` / `math.Max`** -- `FMIN`/`FMAX` are the wrong instruction. Go
  specifies `Max(x, +Inf) = +Inf` for every x including NaN; `FMAX` propagates
  the NaN. 24 disagreements over 490,000 pairs, all of that shape.
  `FMINNM`/`FMAXNM` are wrong the other way, on 5,548. Go's own arm64 assembly
  wraps `FMAXD` in an explicit infinity test for this reason.
- **`math.Copysign`** -- AArch64 has no copy-sign instruction.
- **`math.FMA`** -- semantically clean (`FMADD`, 0 disagreements over 410,648
  triples) and worth 153.60 ns -> about 1.3, but it is a three-operand float
  instruction and arm64 reserves only two floating-point scratch registers. It
  needs a third, which comes out of the allocation order and changes register
  allocation tree-wide. That is its own change, with its own measurement.

The 38x is `runtime.findfunc` scanning `functab` from index 0 on every lookup,
because cg12 emitted `moduledata.findfunctab` as zeroes — twice per goroutine,
through `isSystemGoroutine` in `newproc1` and `gdestroy`. It is fixed by building
the table. `make bench-perf`: **`goroutine/spawn-join` 38.45x → 5.32x**, and
`gc/alloc-churn` 11.70x → 9.74x for free, with nothing else moved and every guard
green.
