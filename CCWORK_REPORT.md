# Parallelising inside goc: moving the capability matrix's floor

Branch: `ccwork/goc-parallel-b`, off the `ccwork/matrix-speed` tip (`0f4ee02`), which already
carries `perf/test-suite`. The previous job's report has been moved to
`docs/report-matrix-speed.md` so this file is only about this job.

Status: **in progress.** Updated as each result lands; the unverified list is at the end.

## The short version

1. **The floor was not a parallelism problem. It was one function.** Profiled, the compile of
   `stdlib_http_tls_client_server.go` spent **66.4% of its whole wall clock inside
   `arm64.slotGroups`**, which re-marked every pair of simultaneously-live temps into a
   `map[int]map[int]bool` at *every instruction*. Register allocation as a whole was 75.5%;
   the front end is 7.9%, not the 39% a `hello.go` profile suggests.
2. Rewriting it — same relation, same greedy assignment, bit matrix instead of a map of maps —
   took that program from **182.8 s to 48.6 s single-threaded**, with the prebuilt runtime
   pack byte-identical.
3. **The per-function back end is now compiled concurrently**, merged strictly in function
   order. That took it to **29.4 s**. Together **182.8 s → 29.4 s, 6.2x**, and the
   `build-runtime` step from 8.1 s to 2.8 s.
4. **Four real determinism bugs found and fixed** in the code the briefing pointed at, all
   letting Go's map iteration order into generated code. But they are **not** the residue: I
   instrumented every function's IR at back-end entry across two compiles and the remaining
   divergence is entirely in goc's **front end**, not the back end. Details in §5 — this
   contradicts the briefing's expectation and is the finding I am least willing to hedge on.
5. A **methodology trap** for the sibling jobs: two goc binaries built from trees at different
   filesystem *paths* produce different programs, because absolute source paths reach the
   emitted data. A `git worktree` at a different-length path is not a valid reference build.

## 1. Where the floor program's time actually goes

Added `goc -cpuprofile` (`cmd/goc/profile.go`) so this is read out of a profile rather than
guessed. `stdlib_http_tls_client_server.go` against the prebuilt pack, 193.7 s of samples:

| node | cum | share |
| --- | ---: | ---: |
| `arm64.CompileToObjectAndAssembly` | 162.3 s | 82.2% |
| ` ├ arm64.regAlloc` | 149.0 s | 75.5% |
| ` │  └ arm64.coalesceSpillSlots → slotGroups` | **131.2 s** | **66.4%** |
| ` └ arm64.emitMachine` | 4.4 s | 2.2% |
| `goc.compile` (front end) | 15.7 s | 7.9% |
| `runtime.mapassign_fast64` + `mapaccess1_fast64` (flat, nearly all from `slotGroups`) | 136 s | 68.8% |

The briefing's 61%/39% back-end/front-end split comes from `hello.go`. At standard-library
scale the shape is different: the front end is 8%, and two thirds of the compile is one
function's map traffic. **Profile the program that actually bounds you.**

## 2. The `slotGroups` rewrite

`arm64/slotcolor.go`. The interference relation and the greedy assignment are unchanged; only
how they are computed changed.

- Members are numbered densely and interference is a square bit matrix over that numbering.
- An edge is recorded only where a member *becomes* live, against the set live at that point,
  with a block's live-out set marked in full. Every pair simultaneously live at a mark is
  still recorded: pairs already live together were recorded at the previous mark, and any pair
  involving a newly-live member is recorded now. The relation is then symmetrised.
- The greedy pass carries, per group, the union of its members' interference, so "does t
  conflict with this group" is one bit rather than a scan of the group.

Output equivalence: the prebuilt runtime pack — the whole Go runtime, the largest module goc
compiles — is **byte-identical** before and after, as is `hello.go`.

## 3. The concurrent back end

`arm64/parallel.go`; the emit loop in `arm64/mc.go` is now an order-preserving merge.

Lowering, register allocation and emission read one function plus read-only module facts, and
produce a result whose every offset is relative to that function's own start. So functions are
compiled concurrently and merged strictly in function order: every address, symbol, relocation
and DWARF row is derived from the merge order, never from which worker finished first.

- Worker count: `GOMAXPROCS`, overridable with `GOC_BACKEND_WORKERS`. Output does not depend
  on it.
- Look-ahead is `2 * workers`, so the number of finished-but-unmerged results follows the
  worker count rather than the module's function count.
- The first error *in function order* is reported, after draining what is in flight.
- `functionCode` holds no IR, and the merge releases both `m.Funcs[i]` and the local
  `functions[i]` — previously only the former, which freed nothing while the local slice still
  held every pointer.

Tests (`arm64/parallel_test.go`): objects byte-identical at 1, 2, 3, 8, 64 and 256 workers on a
65-function module built to exercise text layout, intra-module call relocations, DWARF and
heavy spilling; the reported error identical across worker counts; and a high-pressure function
compiled at 8 workers, linked and run for the right answer. `-race` clean.

## 4. What it measures

`stdlib_http_tls_client_server.go` against the prebuilt pack. **The box is not idle** — two
sibling jobs are compiling throughout — so every pair below was taken back to back, and the
briefing's 157.6 s idle-box figure is not the baseline used here. The baseline is the same
compiler measured under the same conditions.

| | wall | user | cpu | maxrss |
| --- | ---: | ---: | ---: | ---: |
| baseline (branch point) | 182.80 s | 198.91 s | 110% | 2.68 GB |
| + `slotGroups` rewrite | 48.57 s | 65.99 s | 141% | 2.83 GB |
| + concurrent back end, 1 worker | 53.45 s | 64.99 s | 126% | 2.31 GB |
| + concurrent back end, 64 workers | **29.43 s** | 71.27 s | 252% | 2.28 GB |

- The rewrite is **3.76x** on its own; the concurrent back end is a further **1.82x**
  (53.45 → 29.43 measured against its own 1-worker leg, which is the honest comparison).
- **Total 6.2x on the program that bounds the matrix.**
- `cpu=252%` is the ceiling now, not 115%: with the back end no longer dominated by one
  function, the single-threaded front end and the serial merge are a much larger share, so
  Amdahl bites early. Getting past this needs the front end, not more back-end workers.
- `goc build-runtime` went from 8.11 s to 2.77 s at `cpu=379%`.
- Peak memory did not regress (2.68 → 2.28 GB), because the results held between a worker and
  the merge are bounded and the IR is now actually released.

## 5. Determinism: four bugs fixed, and where the residue actually is

Fixed (all let map iteration order into generated code):

- `analysis/freq.go`, `Frequency`: the loop's cyclic probability summed float contributions
  **over a map**. Float addition is not associative, so the loop multiplier, every block
  frequency derived from it, and every spill decision the cost model makes from those differed
  between runs of the same compiler.
- `analysis/freq.go`, `redistributeMesh`: the same, for the mesh's total flow.
- `analysis/loopforest.go`: the loop list was built by iterating the latch grouping map, and
  the parent and innermost-loop choices break ties by position in that list.
- `arm64/allocacolor.go`: stack-colouring candidates came from a map, and the stable sort by
  size then broke ties by that order — so which stack allocations shared a slot, and hence the
  frame layout, varied run to run.

**These are not the §5.10 residue.** `runtime_defer_capture_allocs.go` still compiles to a
different binary every time (6 distinct outputs in 6 compiles, before and after). I hashed
every function's IR at back-end entry, after lowering, after allocation and after emission,
across two compiles at one worker. The result:

- Every function's *content* hash matches between the two runs, except four.
- **441 functions appear in a different position in the module** — the interface-call wrapper
  thunks (`*.interfacecall.*`). Same functions, same code, different order, therefore different
  addresses and a different binary.
- The four exceptions (`testing.prettyPrint`, `testing.common.Attr`,
  `testing.common.makeArtifactDir`, `testing.outputWriter.writeLine`) differ **at back-end
  entry** — before `LowerPointers`, before lowering, before allocation. Diffing
  `testing.prettyPrint`'s IR shows two address computations emitted in swapped order
  (`%t108 =l add %t107, 32` / `%t109 =l add %t107, 40` against the reverse).

So after the four fixes above, the arm64 back end is deterministic for this program, and
**every remaining divergence is in goc's front end**: the order wrappers are created (which is
the order `ensureInterfaceCallWrapper` is first reached while emitting runtime type
descriptors, `goc/compile.go:5928`), and instruction order inside a handful of functions.

I have **not** fixed those. They are in the front end rather than the back end — outside this
job's lever — and changing the order functions land in a module changes the layout of every
program goc compiles, which deserves its own validation cycle rather than being folded into a
performance change. It is a contained, well-located bug and worth a job of its own.

Also noted, not fixed (pre-existing, `-O` only): `opt/inline.go:184` sorts cost-inline
candidates with an unstable `sort.Slice` over a slice built from map iteration, so which
callees get inlined when the budget runs out is not reproducible. The matrix runs `-O` under
`-runtime-opt`.

## 6. Verification

- `go build ./...`, `go vet ./...`: clean.
- `make test-unit`: **pass**.
- `make test-goc-corpus`: **pass**, 606 s (`ok github.com/evanphx/cg12/goc 606.369s`).
- `make test-goc-cmd`: **pass**, 150 s.
- `go test -race ./arm64/ -run TestParallelBackend`: clean.

### The full capability matrix

`scripts/matrix-timing.sh`, full unsharded matrix, `-count=1 -v -runtime-status-progress`.
**The box is shared with two sibling jobs throughout**, so the branch point was re-measured
here rather than compared against the 203.2 s figure recorded on an exclusive box.

| | wall | slowest compile | run phase | census |
| --- | ---: | ---: | ---: | --- |
| branch point, same box, same day | 303.9 s | 278.7 s (`stdlib-http/tls-client-server`) | 19.0 s | 338/338 |
| **this branch** | **116.0 s** | **99.0 s** (same program) | 24.3 s | 338/338 |
| branch point on an exclusive box (recorded, §17) | 203.2 s | 189.5 s | 14.6 s | 338/338 |

**2.6x against the branch point measured under the same load; 1.75x against the exclusive-box
figure while contended.** The bounding term is unchanged in kind — it is still
`max(slowest single compile, compile CPU / workers)`, and it is still the slowest single
compile (99.0 s against 338 programs' worth of work spread over 61 compile workers). The floor
moved; it did not stop being the floor.

Census, every run: **338 subtests, 338 `--- PASS`, 0 `--- FAIL`, 0 `--- SKIP`; 337 declared
PASS, 1 EXPECTED FAILURE, 0 KNOWN GAP.** The single non-passing capability is the declared one:

    runtime_status_test.go:2431: EXPECTED FAILURE runtime_panic_print_string.go

**There are no other non-passing capabilities.**

### The corpus differential: is the output worker-independent?

Every one of the 358 `goc/testdata` programs compiled three times with the final compiler —
twice at 8 back-end workers, once at 1:

| | count |
| --- | ---: |
| all three byte-identical | **319** |
| the two 8-worker runs already disagreed (nondeterministic program) | 34 |
| 8-worker runs agreed, 1-worker run differed | 5 |
| compile failed | **0** |

The five in the third row are the ones that matter, so each was compiled **30 times at 1 worker
and 30 times at 8**:

| program | at 1 worker | at 8 workers |
| --- | --- | --- |
| `runtime_loopvar_range` | 29 x A, 1 x B | 24 x A, 6 x B |
| `stdlib_log_buffer` | 27 x A, 3 x B | 23 x A, 7 x B |
| `stdlib_encoding_csv` | 6 distinct | 5 distinct, all also seen at 1 worker |
| `stdlib_encoding_gob_struct_int` | 25 distinct | 26 distinct |
| `stdlib_runtime_trace_buffer` | 27 x A, 3 x B | (1-worker leg alone settles it) |

Every one of them produces more than one output **at a single back-end worker**, where the
concurrent driver dispatches one function at a time. The three-compile classifier simply could
not distinguish a rare variant from a worker-dependent one. Controls: `runtime_defer_capture_allocs`
gives 25 distinct outputs in 30 single-worker compiles, and `hello` gives 1 in 30.

**No program produced an output at 8 workers that it never produced at 1.** Together with the
unit test (byte-identical objects from 1 to 256 workers) and the `-race` run, that is the case
that the concurrency does not affect the compiler's output.

### Determinism against the branch point

The prebuilt runtime pack — the whole Go runtime, the largest module goc compiles — is
byte-identical between the branch point and the `slotGroups` rewrite, as is `hello.go`. The
later determinism fixes deliberately change generated code (they replace an arbitrary
map-order result with a fixed one), so byte-identity against the branch point does not hold
past that commit and is not claimed.

## Still unverified

- `analysis/splitdiff` over the corpus (running).
- `go test -race` over real Go compiles rather than the synthetic module (running).
- Whether the 39 nondeterministic corpus programs were *already* nondeterministic at the
  branch point. Each variant reproduces at one back-end worker, and the concurrency is
  proven not to introduce one, so this is a completeness gap rather than a live suspicion.
- No idle-box measurement exists for anything here; two sibling jobs were compiling
  throughout.
