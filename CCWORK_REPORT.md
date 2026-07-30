# Making the capability matrix fast: what actually bounds it

Branch: `ccwork/matrix-speed`. `perf/test-suite` is already an ancestor of this branch's
starting tip (`91e6a9f`), so no rebase was needed; the previous job's report has been moved
to `docs/report-driver-split.md` so this file is only about this job.

Status: **complete.** Everything claimed here was measured on this box; the one thing that was
not checked is named under "Still unverified" at the end.

## The short version

1. **The 406.5 s figure was mostly the flag.** The unmodified harness takes **204.7 s** at its
   default worker count and 233.8 s at 24. But raising the flag was not the whole story: the
   harness left slack over its own model that *grew* with the worker count — 67 s at 8 workers,
   159 s at 16 — because the look-ahead budget was returned by a run phase that walked the matrix
   in index order, so a slow compile in the middle of the matrix pinned the dispatcher and idled
   the workers behind it.
2. **The run phase is now two phases.** `runtimeCapability` has an `exclusive` field, 60 of the
   338 are marked, and a new unit test enforces the classification from each program's source.
   That removed the dispatcher coupling; the slack fell to ~14 s at every worker count.
3. **Longest-first dispatch helps, and was measured against a control**: −14% at 16 workers,
   −11% at 24, −3% at 64. Kept. The ordering comes from each program's transitive import
   closure, because source file size — the obvious proxy — ranks these programs at random.
4. **At `cpu_slots: 24` the matrix went 233.8 s → 203.2 s; against the reported 406.5 s, 2.0x.**
   The best case on the whole box barely moved (204.7 → 203.2), because it was already close to
   the floor.
5. **The floor is one program.** `stdlib_http_tls_client_server.go` compiles in 157.6 s alone
   (`cpu=115%` — goc's compile is single-threaded) and 190 s under the matrix's own load. Five
   consecutive full runs: 201.8–203.1 s, 338/338, results byte-identical.

## The model being measured

    matrix wall clock ≈ max( slowest single compile , total compile CPU / workers )
                        + whatever the run phase does not overlap
                        + fixed setup (goc build, prebuilt-runtime build, test binary)

Every measurement below names which of those three terms bounds it. All measurements come
from `scripts/matrix-timing.sh`, which runs the full unsharded matrix with `-v -count=1
-runtime-status-progress` and derives, from the per-program progress lines, the compile CPU
(the sum of per-compile wall clock), the slowest single compile, and the summed run time.
`-count=1` defeats `go test`'s result cache; `-v` plus a subtest census is the lie detector
the briefing asks for — every row below is checked for `subtests=338 pass=338 fail=0` and
`declaredPASS=337 expectedFAILURE=1 knownGAP=0`.

Box: linux/arm64, 64 cores, ~240 GB RAM, exclusive, `cpu_slots: 24`.

## 1. The real baseline

Unmodified harness (`91e6a9f`), full 338-capability matrix, prebuilt-runtime path (the
default). Every row: `subtests=338 pass=338 fail=0 declaredPASS=337 expectedFAILURE=1
knownGAP=0`.

| workers | wall clock | compile CPU | CPU/workers | slowest single compile | run phase | bounding term | slack |
| ---: | ---: | ---: | ---: | ---: | ---: | --- | ---: |
| 8 | 442.8 s | 3010.3 s | 376.3 s | 167.0 s | 14.0 s | compile CPU / workers | 67 s |
| 16 | 351.9 s | 3085.0 s | 192.8 s | 168.3 s | 13.9 s | scheduling slack | 159 s |
| 24 | 233.8 s | 3280.4 s | 136.7 s | 177.1 s | 14.6 s | slowest single compile | 57 s |
| default (64) | 204.7 s | 4142.6 s | 64.7 s | 179.6 s | 15.0 s | slowest single compile | 25 s |

"slack" is wall clock minus the model's `max(slowest compile, CPU/workers)`. It is the part
the model does not explain, and it is the thing to attack: **it grows as workers are added.**
That is the signature of workers going idle, not of a real floor.

### Where the compile CPU is

The distribution is sharply bimodal. Eleven programs cost 125–167 s each and account for
1628 s — 54% of all compile CPU. The other 327 average 4.2 s.

| compile | capability | matrix index |
| ---: | --- | ---: |
| 167.0 s | stdlib-http/tls-client-server | 222 |
| 164.0 s | stdlib-http/redirect-keepalive | 223 |
| 163.9 s | stdlib-http/client-server | 218 |
| 161.5 s | stdlib-http/cookiejar | 220 |
| 161.3 s | stdlib-http/parse-roundtrip | 219 |
| 161.1 s | stdlib-http/multipart-form | 221 |
| 136.5 s | stdlib-net-values/smtp-session | 212 |
| 133.4 s | stdlib-crypto/x509-ed25519 | 156 |
| 128.1 s | stdlib-crypto/ecdsa | 158 |
| 126.0 s | stdlib-crypto/ecdh-x25519 | 155 |
| 125.3 s | stdlib-crypto/hpke | 162 |
| 11.7 s | stdlib-encoding/xml (the 12th) | |

### Why the slack grows with workers

`startRuntimeCapabilityCompiles` takes two budgets per program: a worker slot, returned when
the compile finishes, and a **look-ahead token, returned only when that program's *run*
finishes**. The run phase is strictly sequential and walks the matrix in index order, so the
dispatcher can never get more than `4*workers` indices ahead of the run frontier.

The eleven expensive programs sit at indices 155–223. When the run frontier reaches index 218
it blocks for ~164 s on that program's compile. During that block no look-ahead token is
returned, so the dispatcher is pinned at index ~218+4*workers and the workers have only the
programs inside that window to chew on. Six of them are the http programs; the rest are ~4 s
each and are exhausted in seconds. From then until the frontier moves, most workers are idle.

Adding workers widens the window but does not remove the stall, and it adds more idle workers
to it — which is exactly why the slack rose from 67 s at 8 workers to 159 s at 16.

So the ~406 s the driver-split job reported at `-runtime-status-compile-workers=10` was *not*
simply an artifact of that flag. The flag cost something, but the dominant term is a
structural coupling between the sequential run phase and the compile dispatcher.

### Two other facts the baseline establishes

- **Compile CPU rises with workers**: 3010 s at 8, 3085 s at 16, 3280 s at 24. Adding workers
  buys less than `1/workers`, because the compiles contend. So `compile CPU / workers` is not
  a term that can be driven to zero by throwing cores at it.
- **The run phase is 14 s**, and its largest single program is 5.1 s
  (`stdlib-signals/atomic-contention`). Peak RSS over all 338 runs is 78 MiB; peak RSS over
  all 338 compiles is 2.65 GiB. So parallelising the run phase is worth at most ~14 s
  *directly*. Its real value is indirect, and it is the paragraph above: it is what lets the
  compile dispatcher stop taking its order from the run frontier.

## 2. What was changed

Three changes, all in `cmd/goc`. None of them touches the compiler, the runtime, or any
capability program.

### 2.1 `exclusive` on `runtimeCapability`, and a two-phase run

`runtimeCapability` gains one field:

```go
// exclusive marks a capability that must run with nothing else running.
exclusive bool
```

60 of the 338 capabilities are marked. The run phase became two phases: the other 278 run
concurrently (bounded, default 4 at a time) as their programs become available, and the 60
exclusive ones run one at a time afterwards, after `drainCompiles` has waited for every
compile — so they get a quieter machine than the old "sequential" phase gave them, which ran
them alongside a saturated compile queue.

**Which 60, and why.** The rule, stated in the code and enforced by a new millisecond-scale
unit test (`TestRuntimeCapabilityExclusiveClassification`), is that a capability is exclusive
when its outcome depends on how much of the machine it has. Mechanically, a source is exclusive
if it:

| pattern | reason |
| --- | --- |
| `time.Now/Since/Sleep/After/AfterFunc/Tick/NewTimer/NewTicker` | measures or waits on wall clock |
| `Set[Read\|Write]Deadline`, `DialTimeout`, `WithTimeout`, `WithDeadline` | bounds an operation by wall clock |
| `GOMAXPROCS` | sets its own GOMAXPROCS |
| `ReadMemStats`, `MemStats`, `AllocsPerRun`, `NumGC`, `Mallocs`, `Frees`, `HeapAlloc`, `TotalAlloc`, `HeapObjects` | asserts allocation or GC statistics |
| `SetGCPercent`, `SetMemoryLimit`, `SetMaxThreads` | changes a process-wide runtime limit |
| `runtime.NumGoroutine` | asserts a goroutine count |
| `runtime.Gosched` | yields to the scheduler or the collector and then asserts what happened |

plus every capability in the `scheduler-stress` and `stdlib-netpoll-stress` categories, whose
whole point is contention.

The test enforces that as a **floor, not an equivalence**: a source matching any pattern must
be marked, and extra markings are allowed for reasons no pattern finds. That is the part that
matters going forward — a new timing-sensitive capability cannot land unmarked without failing
a unit test that takes 70 ms, instead of producing a matrix that fails one run in twenty.

By category the 60 are 18 `gc`, 13 `runtime-packages`, 10 `stdlib-netpoll`, 5 `stdlib-signals`,
4 `stdlib-netpoll-stress`, 4 `stdlib-bytes`, 3 `scheduler-stress`, 2
`stdlib-runtime-diagnostics`, 1 `goroutine`. Their combined run time is **7.17 s of the
14.01 s total**, so the concurrent half of the phase has ~6.8 s to hide.

**I deliberately over-marked.** Three of the 60 I judged safe and marked anyway:
`runtime-packages/time-after` only asserts `time.Since(start) >= 0`;
`goroutine/gosched-progress` spins on `runtime.Gosched` with no iteration bound;
`gc/stack-argument-roots` calls `runtime.GC(); runtime.Gosched()` and then *non-blockingly*
drains a channel, asserting a negative (nothing was collected), which load can only make less
sensitive. Each is marked because the rule that catches the dangerous ones has to be mechanical
to stay maintainable, and I would rather serialise a program I have judged robust than depend
on that judgement across 278 concurrent runs. Three others earn their marking on a margin I
would not have guessed: `stdlib-netpoll/{tcp-echo,close-unblocks-read,tcp-concurrent-clients}`
each call `net.DialTimeout(..., time.Second)` on loopback — a huge margin, but still a
wall-clock bound, and the pattern that catches them is the same one that catches the 20 ms
deadlines.

### 2.2 A look-ahead window that cannot deadlock

The look-ahead token is returned when a program *runs*, and an exclusive program does not run
until the very end. So the window has to be the concurrent budget **plus** the exclusive
count:

```go
lookahead: make(chan struct{}, 4*workers+exclusive)
```

Without the `+exclusive` term this is a real deadlock, not a slowdown: with 60 exclusive
capabilities and `4*workers` tokens, any worker count below 15 lets the exclusive programs
alone exhaust the window while the dispatcher waits for a token that only the final phase can
return. The disk bound the window exists to enforce is still a bound — `4*workers + 60`
compiled programs rather than `4*workers` — just a larger one.

### 2.3 Longest-first dispatch, from the import closure

`startRuntimeCapabilityCompiles` now dispatches
`runtimeCapabilitiesByDescendingCompileCost(capabilities)`.

The cost model is **the total size of the Go and assembly sources in the program's transitive
import closure**, resolved against the vendored standard library goc actually compiles
(`GOROOT=../../stdlib`, `GOARCH=arm64`, `purego`, including `src/vendor`), memoized across
capabilities.

I chose that over the briefing's suggested "source file size is a cheap proxy" because **the
measurement says source file size is not a proxy at all**: the capability sources cluster
around 575 bytes, the single most expensive program (`stdlib_http_tls_client_server.go`, 167 s)
is 1303 bytes, and the largest source in the matrix is 6553 bytes and compiles in seconds.
Ranking by file size ranks these programs at random. What separates them is how much standard
library they pull in, so that is what the model measures. A wrong estimate costs wall clock
and nothing else — the queue compiles the same set either way.

**How well it ranks.** Its eleven largest estimates are exactly the eleven programs measured
at 125–167 s, and nothing else is within 15% of the eleventh:

| model rank | capability | closure | measured compile |
| ---: | --- | ---: | ---: |
| 1 | stdlib-http/redirect-keepalive | 11.49 MB | 164.0 s |
| 2 | stdlib-http/client-server | 11.47 MB | 163.9 s |
| 3 | stdlib-http/tls-client-server | 11.47 MB | 167.0 s |
| 4 | stdlib-http/cookiejar | 11.42 MB | 161.5 s |
| 5 | stdlib-http/parse-roundtrip | 11.40 MB | 161.3 s |
| 6 | stdlib-http/multipart-form | 11.40 MB | 161.1 s |
| 7 | stdlib-net-values/smtp-session | 9.18 MB | 136.5 s |
| 8 | stdlib-crypto/x509-ed25519 | 8.39 MB | 133.4 s |
| 9 | stdlib-crypto/ecdsa | 7.25 MB | 128.1 s |
| 10 | stdlib-crypto/hpke | 6.87 MB | 125.3 s |
| 11 | stdlib-crypto/ecdh-x25519 | 6.69 MB | 126.0 s |
| 12 | stdlib-crypto/rsa | 6.14 MB | 10.8 s |

An intermediate version of the model summed the closure once per import *edge* rather than
once per package. That ranked almost as well but its numbers were meaningless (5.6e12 "bytes"),
so it was replaced with the set union, which is both honest and slightly better: the
edge-weighted version put `stdlib-crypto/rsa` (10.8 s) 10th and pushed `ecdh-x25519` (126 s)
to 14th.

`TestRuntimeCapabilityCompileCostRanksTheExpensivePrograms` pins the six `stdlib-http`
capabilities inside the top twelve. That is the guard that matters: if the vendored-tree
resolution ever breaks, every estimate ties, the order collapses back to matrix order, those
six land at indices 218–223, and the test fails instead of the matrix silently getting slower.

## 3. What it measures

Same harness, same box, same census check on every row.

| workers | baseline | concurrent run phase only | + longest-first (final) | final vs baseline |
| ---: | ---: | ---: | ---: | ---: |
| 8 | 442.8 s | — | **394.4 s** | −11% |
| 16 | 351.9 s | 257.9 s | **221.2 s** | −37% |
| 24 (`cpu_slots`) | 233.8 s | 229.5 s | **203.2 s** | −13% |
| default (64) | 204.7 s | 210.5 s | **204.4 s** | −0.1% |

The middle column is a control: the same tree with `runtimeCapabilitiesByDescendingCompileCost`
replaced by matrix order, so the two changes can be told apart.

**Longest-first earns its keep**: −36.7 s at 16 workers (−14%), −26.3 s at 24 (−11%), −6.1 s
at 64 (−3%). It is kept.

**The honest headline is smaller than it looks.** The best wall clock available on this box
barely moved: 204.7 s before (at the default 64 workers) against 203.2 s after (at 24). What
changed is everything *except* the best case — the matrix is no longer sensitive to the worker
count. At 16 workers it went from 352 s to 221 s. That matters for exactly the situation that
produced the 406.5 s figure in the first place: a job told to use its declared CPU share rather
than the whole box. **At `cpu_slots: 24` the matrix went from 233.8 s to 203.2 s, and against
the previously reported 406.5 s it is 2.0x.**

### Where the remaining slack went

At 24 workers the model now says `max(189.5, 3428/24 = 142.8) = 189.5 s` and the wall clock is
203.2 s: **13.7 s of slack**, against 57 s before. That 13.7 s is the fixed setup — the `goc`
build (cached), the test binary, and the 4.6 s prebuilt-runtime build — plus the 7.2 s
exclusive run phase at the end. There is no idle-worker term left to remove.

### One cost the change does incur

Longest-first makes the critical-path compile *slower*, because it now starts all eleven
expensive compiles at t=0 and they contend with each other:

| | slowest single compile |
| --- | ---: |
| alone on an idle box | 157.6 s |
| baseline, 24 workers (started late, mostly alone) | 177.1 s |
| final, 24 workers (started first, with ten peers) | 189.5 s |
| final, 64 workers | 190.8 s |

Starting it early wins more than the contention costs, but the two partly cancel, and that is
the main reason the 64-worker column is a wash.

## 4. What bounds the matrix now, and the next lever

**The floor is one program: `stdlib_http_tls_client_server.go`.** Measured alone on the idle
box, against the prebuilt runtime pack:

    wall=157.61s user=179.02s sys=3.01s cpu=115% maxrss=2.97 GB

`cpu=115%` is the whole story. **goc's compile is single-threaded** — there is no `go func`,
`sync.WaitGroup` or `errgroup` anywhere in `goc/compile.go` or its neighbours; the 15% above
one core is the Go runtime's own background mark. So for 158 of the matrix's 203 seconds, 63 of
64 cores have nothing to do with that program, and no amount of worker tuning changes it.

Three levers, in the order I would take them:

1. **Make the pack carry the standard library** (§16 already names this). The six `stdlib-http`
   programs have import closures that differ by less than 1% — 11.49, 11.47, 11.47, 11.42,
   11.40, 11.40 MB — and each costs ~157 s, so **~940 s of the ~3030 s of compile CPU is the
   same net/http closure compiled six times.** Collapsing it needs a stub dispatcher for
   program symbols the pack does not generate, plus moving the image's package-init list to the
   program module. Note this does *not* by itself move the floor: building such a pack costs one
   net/http compile. It moves the floor only if the pack is built once and cached across runs.

2. **Cut the per-process fixed cost.** `hello.go` against the pack costs `wall=2.11s
   user=3.86s`, and every one of the 338 compiles pays that — mostly loading and type-checking
   the runtime's source closure, which `sharedSourceWorld` caches *per goc process* and the
   matrix runs 338 separate processes. That is roughly **700 s of the 3030 s**, or 23%. A goc
   mode that compiles several programs in one process would share the world and recover most of
   it. It would not move the floor either, but it is the largest remaining chunk of compile CPU
   after (1).

3. **Parallelise inside goc.** This is the only one of the three that moves the *floor* rather
   than the total, because the floor is one single-threaded process. It is also the largest
   piece of work.

## 5. Correctness: what was checked

Every full-matrix run reported in this document — 11 of them — was checked for
`subtests=338 pass=338 fail=0 skip=0 declaredPASS=337 expectedFAILURE=1 knownGAP=0`, and
every one satisfied it. The single declared exception is `defer-panic/panic-string-output`.

| check | result |
| --- | --- |
| `make test-unit` | clean |
| `make test-goc-cmd` | `ok github.com/evanphx/cg12/cmd/goc 205.959s` |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 819.623s` |
| full matrix × 5, identical results | **yes — all five identical, see below** |
| both compile paths | prebuilt (default) and `-runtime-status-prebuilt-runtime=false` both pass |
| sharding | `STATUS_SHARDS=4`, all four shards: 22+22+23+23 = 90 subtests, exactly the unsharded selection, 0 fail |
| the look-ahead `+exclusive` term | the 8-worker run (`4*8+60 = 92` tokens) completes; without the term it would have had 32 tokens for 60 unreclaimable holds |
| collector under concurrency | new `TestRuntimeCorpusCoverageRecordsConcurrentOutcomesDeterministically` passes, including under `-race` |
| determinism, no pack | 4 of 5 identical over two rounds; `runtime_defer_capture_allocs.go` differs — the §5.10 backend residue |
| determinism, with the pack | 4 of 5 identical over two rounds; same single exception |
| the pack itself | three `goc build-runtime` runs (two warm, one `CG12_NOCACHE=1`) byte-identical |

### The five repeated runs

The concurrent run phase is the change that could produce a flaky suite, so five full
unsharded matrix runs at 24 workers, back to back:

| run | wall | compile CPU | slowest compile | subtests | pass | fail |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 202.6 s | 3421.7 s | 188.9 s | 338 | 338 | 0 |
| 2 | 201.8 s | 3425.1 s | 187.9 s | 338 | 338 | 0 |
| 3 | 201.8 s | 3410.1 s | 188.1 s | 338 | 338 | 0 |
| 4 | 203.1 s | 3416.3 s | 189.5 s | 338 | 338 | 0 |
| 5 | 203.1 s | 3422.2 s | 189.6 s | 338 | 338 | 0 |

**Identical, and not just in the counts.** For each run I extracted the sorted set of
`--- PASS/FAIL/SKIP: TestARM64RuntimeCapabilityStatus/<category>/<name>` lines with the
per-subtest timings stripped, and separately the sorted set of the 338 declared verdict lines
(`PASS <source>` / `EXPECTED FAILURE <source>` / `KNOWN GAP <source>`). All four comparisons
against run 1 are byte-identical, for both extractions. Wall clock spread is 1.3 s (0.6%).

### One more full run on the final committed tree

The five repeats were taken before the last commit (the memory-divisor fix, which does not
change the worker count on this box). One more full run on the exact committed tree, at the
default worker count:

    label=final-default wall=204.0 subtests=338 pass=338 fail=0 skip=0
    declaredPASS=337 expectedFAILURE=1 knownGAP=0
    compile_cpu=4237.5 slowest_compile=190.3 (stdlib-http/tls-client-server) run_total=16.2

### The complete list of non-passing capabilities

One, and it is declared: **`defer-panic/panic-string-output`**, an `expectedFailure`. It appears
as `EXPECTED FAILURE runtime_panic_print_string.go` in every run. Across all five runs:
`FAIL=0 KNOWN GAP=0 SKIP=0`.

The change **touches no non-test Go file**: `git diff --name-only` over this job's commits is
five `_test.go` files under `cmd/goc`, two Markdown files, and two scripts. So the compiler and
runtime are bit-identical to the branch point, which is why determinism could not have moved —
and it did not.

## Still unverified

- **A full instrumented coverage run (`make test-goc-coverage`) was not made.** That path shares
  the collector I put a mutex on and the report I now sort, and both are covered by the new unit
  test above (200 capabilities recorded concurrently, encoded report byte-identical across two
  runs, clean under `-race`), but the end-to-end path is not exercised here. A targeted coverage
  run cannot substitute: `-runtime-coverprofile` refuses a sharded run and flags every capability
  that reported nothing, so anything short of the whole corpus fails by construction.
- **Flakiness beyond five runs.** Five identical runs bound the per-run failure probability at
  roughly 45% with 95% confidence, which is weak. The classification's real defence is
  `TestRuntimeCapabilityExclusiveClassification`, not the repeat count.
