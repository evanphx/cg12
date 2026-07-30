# Making the capability matrix fast: what actually bounds it

Branch: `ccwork/matrix-speed`. `perf/test-suite` is already an ancestor of this branch's
starting tip (`91e6a9f`), so no rebase was needed; the previous job's report has been moved
to `docs/report-driver-split.md` so this file is only about this job.

Status: **in progress — written as it lands.** Anything not verified is stated as such.

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
| default (64) | (running) | | | | | | |

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

55 of the 338 capabilities are marked. The run phase became two phases: the other 283 run
concurrently (bounded, default 4 at a time) as their programs become available, and the 55
exclusive ones run one at a time afterwards, after `drainCompiles` has waited for every
compile — so they get a quieter machine than the old "sequential" phase gave them, which ran
them alongside a saturated compile queue.

**Which 55, and why.** The rule, stated in the code and enforced by a new fast unit test
(`TestRuntimeCapabilityExclusiveClassification`), is that a capability is exclusive when its
outcome depends on how much of the machine it has. Mechanically: its source measures or waits
on wall clock (`time.Now/Since/Sleep/After/AfterFunc/Tick/NewTimer/NewTicker`), sets an I/O
deadline, sets its own `GOMAXPROCS`, asserts an allocation or GC statistic
(`ReadMemStats`/`MemStats`/`AllocsPerRun`/`NumGC`/`Mallocs`/`Frees`/`HeapAlloc`/`TotalAlloc`/
`HeapObjects`), changes GC pacing (`SetGCPercent`/`SetMemoryLimit`), asserts a goroutine or
thread count, or is in the `scheduler-stress` or `stdlib-netpoll-stress` category.

The test enforces that as a **floor, not an equivalence**: a source matching any pattern must
be marked, and extra markings are allowed. That is the part that matters going forward —
a new timing-sensitive capability cannot land unmarked without failing a unit test that takes
milliseconds, instead of producing a matrix that fails one run in twenty.

By category the 55 are: 17 `gc` (finalizers and cleanups, all of which sleep to let the
collector run, plus the two allocation-count assertions and the two `GOMAXPROCS` setters),
13 `runtime-packages` (timers, tickers, `select` timeouts, finalizers, GC controls),
10 `stdlib-netpoll` + `stdlib-netpoll-stress` (deadlines and churn), 5 `stdlib-signals`,
4 `stdlib-bytes` (allocation counts), 3 `scheduler-stress`, 2
`stdlib-runtime-diagnostics`, 1 `stdlib-bytes`-adjacent `gomaxprocs-memstats`. Their combined
run time is 7.0 s of the 14.0 s total.

I deliberately over-marked rather than under-marked. `runtime-packages/time-after` only
asserts `time.Since(start) >= 0` and could safely run concurrently; it is marked anyway,
because the rule that catches the dangerous ones has to be mechanical to be maintainable, and
the whole exclusive phase costs 7 s.

### 2.2 A look-ahead window that cannot deadlock

The look-ahead token is returned when a program *runs*, and an exclusive program does not run
until the very end. So the window has to be the concurrent budget **plus** the exclusive
count:

```go
lookahead: make(chan struct{}, 4*workers+exclusive)
```

Without the `+exclusive` term this is a real deadlock, not a slowdown: with 55 exclusive
capabilities and `4*workers` tokens, any worker count below 14 lets the exclusive programs
alone exhaust the window while the dispatcher waits for a token that only the final phase can
return. The disk bound the window exists to enforce is still a bound — `4*workers + 55`
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

(measurements for 2.1–2.3 below)
