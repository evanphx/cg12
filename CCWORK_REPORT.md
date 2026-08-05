# The interface-dispatch miscompile that keeps `goc -O`'s mem2reg switched off

Branch: `ccwork/mem2reg-iface-dispatch`, off `integration/wave8` = `7983abd`.
Fix: `792d2f0`.

## The answer

**mem2reg described the phis it minted for a managed slot and nothing else.**

`opt.Mem2Reg` already knew that promoting a pointer local changes who can see the
pointer, and it carried the slot's GC marking onto every phi it created for that
variable. It did not carry it onto the *other* kind of reaching definition — the
value a store put into the slot, which after promotion is what every later load
reads. For a local assigned once and read later, that value is the only
definition there is, and no phi is involved at all.

So a pointer that the safepoint map described while it sat in a frame slot moved
into an SSA value the map does not mention, and stayed there for the whole
function.

`stdlib-netpoll-stress/tcp-churn` is what that costs. In `main.main`,

```go
listener, err := net.Listen("tcp", "127.0.0.1:0")
```

is a local of interface type: a frame slot holding the *address* of the two-word
`(itab, data)` descriptor. For a multi-result call the descriptor is the result
home the call writes into — `ir.Func.AllocAggregate`'s slot, an allocation in
`main.main`'s own frame, marked a GC reference precisely because "under a managed
(copied) stack that interior pointer must be relocated". Promotion made the
call's result temporary the value every later read of `listener` sees. That
temporary is unmarked. `copystack` then walked `main.main`'s frame map, did not
find the pointer, and left `listener` addressing the stack the runtime had
already freed and handed to something else. The type word read back was zero:

    cg12: interface dispatch failed for dynamic type 0x0

in `net.Listener.Accept`, reached from the goroutine `main.main` had handed the
(by then already corrupt) descriptor to.

The fix is `opt/mem2reg.go`'s `markManagedDef`: every reaching definition of a
managed promoted variable is marked, not just the phis. A definition that already
carries a marking is left alone — its own type descriptor is at least as precise
as the slot's.

**`stdlib-netpoll-stress/tcp-churn` passes on the `-O` arm with promotion on.**

## The minimal reproducing set

Delta-debugging the set of functions mem2reg is allowed to promote, from the
**3494** it promotes in that program down to a **1-minimal four**:

    main.main
    net.ListenConfig.Listen
    net.Resolver.lookupIPAddr
    net.Resolver.resolveAddrList

1-minimal is measured, not assumed: each of the four 3-element subsets was
compiled and run, and all four are clean (`RUNS=3`, `GOMAXPROCS=1`).

The set reads exactly as the root cause predicts. `main.main` is where the local
lives — promote anything else and the local keeps its frame slot, which
`copystack` adjusts. The other three are on the call path *inside* `net.Listen`,
and what they contribute is frame size: they decide whether the stack has to grow
after `net.Listen` returns rather than only while it is running. Promote fewer
and the growth that would strand the pointer happens before there is a pointer to
strand. That is the "at least two `net` functions at once" the previous job
measured, and it is a threshold on stack depth, not a second semantic ingredient.

The four-function build fails slightly differently from the whole-program one —
`main.main` hands the goroutine a descriptor whose *both* words are zero, so
`net.Listener.Accept` faults on `*(0+8)` instead of reaching the dispatch-failure
report. Same corruption, same place, different garbage; the whole-program build
is the one that prints `dynamic type 0x0`.

## The reduction

`goc/testdata/runtime_opt_promoted_interface_root.go`, 75 lines and no network,
run by `TestOptimizedInterfaceLocalSurvivesStackGrowth` in
`goc/optgcroot_test.go`. It is the same shape stripped to nothing:

```go
c, n := makeCounter(7)   // multi-result, so the descriptor is main's own result home
deepen(600)              // grows the goroutine stack; main's frame moves
...64 goroutines, each deepen(600)...   // the freed stacks are handed out and written over
c.value()                // dispatch through whatever is there now
```

`deepen` fills every frame it makes with `0x5e5e5e5e5e5e + depth`, so the failure
names itself:

    unexpected fault address 0x5e5e5e5e60bd

That address *is* the pattern the goroutines wrote, which is what makes the
reducer deterministic rather than dependent on what happened to be left behind.

| configuration | result |
|---|---|
| `GOC_BOUNDED_MEM2REG=1 goc -O`, before the fix | faults 3/3 |
| `GOC_BOUNDED_MEM2REG=1 goc -O`, after the fix | passes 3/3 |
| `goc -O` (promotion off) | passes 3/3 |
| `goc` (unoptimized) | passes 3/3 |
| host `go run` | passes |

`go test ./goc -run TestOptimizedInterfaceLocalSurvivesStackGrowth` fails on the
tree without `markManagedDef` and passes with it. It uses
`optimizeProgramFunctions`, the existing harness that runs the real
intraprocedural pipeline over the program's own functions, so the test does not
depend on the environment switch and is live while the switch stays off.

The two results in `makeCounter` are load-bearing. A single-result assignment
goes through `adaptInterfaceToInterface`, which copies the descriptor into a
fresh alloca first; the backend readdresses an alloca rather than keeping it in a
value, so the promoted local cannot go stale. That is why the first reductions
attempted passed, and why `net.Listen`'s `(Listener, error)` is the shape that
breaks.

## How it was found

Bisecting *which functions get promoted* is what turned this from a guess into a
measurement, and it is thirty lines of scaffolding:

- wrap `Mem2Reg` in the bounded pipeline with a filter that consults a file of
  function names (`GOC_BOUNDED_MEM2REG_ONLY`) and appends every function it
  actually changed to another (`GOC_BOUNDED_MEM2REG_LIST`);
- collect the 3494-name list from one whole-program compile;
- run ddmin over it, ten candidate subsets at a time. One compile-and-run cycle
  is 8.4 s, so the whole descent from 3494 to 4 took about twenty minutes.

The scaffolding is not in the committed change — it puts environment-dependent
behaviour inside a core pass — but it is worth rebuilding for the next
promotion-shaped miscompile.

Two runtime instruments then named the mechanism without any guessing:

- a three-argument `gocInterfaceDispatchFailure` plus a nil-type-word guard in
  the dispatcher wrapper, which reported `itab 0x0 word1 0x0` — the receiver was
  wholly zero, not mis-paired, so no half of a two-word copy had gone missing;
- `GODEBUG=cg12checkstackcopy=2`, already in the tree, which reports every word
  left pointing into the old stack after `copystack`. It found **19** of them in
  `main_main`'s frame, all with `marked=0` — the frame map did not describe them,
  which is why nothing adjusted them and why the level-1 check (which throws only
  on *marked* slots) stayed silent.

## Measurements

**Reproduction on `7983abd`** (before the fix), `stdlib-netpoll-stress/tcp-churn`
compiled monolithically:

| configuration | runs |
|---|---|
| `GOC_BOUNDED_MEM2REG=1`, `GOMAXPROCS=1` | 5/5 fail |
| `GOC_BOUNDED_MEM2REG=1`, default `GOMAXPROCS` | 2/3 fail |
| promotion off | clean |

**After the fix**, same program, full promotion (all 3494 functions):

| configuration | runs |
|---|---|
| `GOMAXPROCS=1` | 5/5 pass |
| default `GOMAXPROCS` | 5/5 pass |

## What else fails with promotion on

Nothing in the capability matrix, and nothing in the corpus.

The whole matrix, both arms, with `GOC_BOUNDED_MEM2REG=1`, on the tree with the
fix (`go test -v -run TestARM64RuntimeCapabilityStatus ./cmd/goc/...`,
`-runtime-status-compile-workers=14`):

| arm | subtests | failures |
|---|---:|---:|
| `-runtime-opt`, switch **on** | 367 | **0** |
| default, switch **on** | 367 | **0** |

`stdlib-netpoll-stress/tcp-churn` is in that green `-O` arm. The default arm is
unchanged by construction — a build without `-O` never calls
`opt.OptimizeModule`, so the switch cannot reach it — and is reported because it
was asked for.

Every corpus program, not only the 366 the matrix registers: all **405** of
`goc/testdata/*.go` compiled with `goc -O` and run, once with the switch on and
once with it off, comparing outcomes program by program. **405/405 compile in
both**, and the only non-zero exit in either sweep is
`runtime_panic_print_string`, which panics on purpose and exits 2 in both.

## Guards, with the switch off

The switch stays off in this change, so these are the runs that show the tree is
where it was. Everything below is on the branch tip with the fix and with
`GOC_BOUNDED_MEM2REG` unset.

| guard | result |
|---|---|
| capability matrix, default arm | **367/367**, 0 failures |
| capability matrix, `-runtime-opt` arm | **367/367**, 0 failures |
| GC reducer `gc/type-mask-padding`, `-O`, `GOMAXPROCS=3`, default `GOGC` | **0/20** fail |
| GC reducer, `-O`, `GOGC=10` | **0/20** fail |
| GC reducer, unoptimized, default `GOGC` | **0/20** fail |
| GC reducer, unoptimized, `GOGC=10` | **0/20** fail |
| `goc.TestFrameEscapeAudit` | PASS, no baseline change |
| `goc.TestEscapeShadowPlacement` | PASS, no baseline change |
| `goc.TestLoopAliasAudit` | PASS, no baseline change |
| `goc.TestAllocationCensus` | PASS after `-update-alloc-census-baseline` |
| determinism, corpus, 3 rounds, no `-O` | **405/405 reproducible**, 0 varying, 0 failed, 0 content-varies, 0 layout-only |
| determinism, corpus, 2 rounds, `-O` | **405/405 reproducible**, 0 varying, 0 failed, 0 content-varies, 0 layout-only |

The matrix is 367 capabilities on `integration/wave8`, one more than the 366 the
job was written against.

The census moves by exactly four lines, all four in the new reducer and all four
in the new file: the `&box` the interface return escapes, the channel header and
its buffer, and the closure environment the goroutines are spawned through. **No
existing site moved**, which is what says the change alters no allocation
decision. `039129b`.

## The other blocker: not reproduced, and no claim made

The switch is also held back by the performance suite's `compress/flate`
workload dying in the collector ("runtime: pointer ... to unused region of
span"), measured at 3/5 by the job that added the switch. That is a different
job's, and this change is not offered as a fix for it.

It did not reproduce here. `goc/testdata/placement_bench/flate/main.go` built
`goc -O` with `GOC_BOUNDED_MEM2REG=1` by a compiler from `7983abd` **without**
this fix — the same compiler that fails tcp-churn 3/3, checked — ran clean:

| build | runs |
|---|---|
| pre-fix, default `GOGC`, unpinned | 0/5 fail |
| pre-fix, `GOGC=10`, unpinned | 0/8 fail |
| pre-fix, default `GOGC`, pinned to one core | 0/36 fail |
| pre-fix, `GOGC=10`, pinned to one core | 0/30 fail |
| **with** this fix, default `GOGC`, pinned | 0/30 fail |

79 pre-fix runs against a reported 3/5 says the crash needs something this did
not do — `make bench-perf` runs the suite's own harness and this ran the program
alone — or that something merged into `integration/wave8` since the switch
landed has already closed it. Either way it is open until someone runs the suite,
and nothing here should be read as having fixed it.

## What is left

- The switch stays **off**. Turning it on is a later job's call, and it should
  not be made until the flate crash is either reproduced and fixed or shown to
  be gone.
- `markManagedDef` restores the invariant "promotion does not change what the
  collector sees" for reaching definitions. The phi half was already there. The
  same question has not been asked of the *backend* — what the arm64 stack maps
  report for a marked temp the register allocator has put in a callee-saved
  register rather than a spill slot — and the flate report points at exactly that
  place. `TestGoStackMapsOmitAggregateResultHomeAtItsOwnCall` is the nearest
  existing instrument.
- The bisection scaffolding (`GOC_BOUNDED_MEM2REG_ONLY` / `_LIST` /
  `_VARS` / `_VARLIST`) is deliberately not committed. Rebuilding it is a filter
  in `BoundedPipeline`'s `Mem2Reg` wrapper plus a `mem2regVarFilter` hook in
  `findPromotable`, and it is what makes a promotion miscompile answerable in
  twenty minutes instead of a day.
