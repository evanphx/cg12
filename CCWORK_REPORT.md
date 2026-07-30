# Parallelising inside goc: moving the capability matrix's floor

Branch: `ccwork/goc-parallel-b`, off the `ccwork/matrix-speed` tip (`0f4ee02`), which already
carries `perf/test-suite`. The previous job's report is now `docs/report-matrix-speed.md` so
this file is only about this job.

Status: **complete.** Everything claimed here was measured on this box. What was not checked
is under "Still unverified" at the end.

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
   order. Together **182.8 s → 30.8 s on the floor program**, and `goc build-runtime` from
   8.1 s to 2.8 s.
4. **The full matrix went 225.8 s → 71.4 s in consecutive runs, 3.2x**, at 338 subtests /
   337 declared PASS / 1 EXPECTED FAILURE / 0 FAIL / 0 KNOWN GAP every time.
5. **Five determinism bugs found and fixed.** Four in the code the briefing pointed at (map
   iteration order reaching generated code). The fifth was found by the concurrency: `go test
   -race` over the *real corpus* reported 20 data races, and the cause was a **pre-existing
   front-end defect** giving one function a control-flow edge into another's blocks. A green
   suite and a green synthetic byte-identity test both said the concurrency was fine.
6. **The §5.10 determinism residue is in goc's front end, not the back end.** That contradicts
   the briefing's expectation. §5 has the evidence; I located it and deliberately did not fix
   it.
7. A **methodology trap** for the sibling jobs: two goc binaries built from trees at different
   filesystem *paths* produce different programs, because absolute source paths reach the
   emitted data. A `git worktree` at a different-length path is not a valid reference build —
   it cost me one wrong conclusion before I noticed.

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

`arm64/slotcolor.go`. `slotGroups` partitions spilled temps into stack-slot-sharing groups.
The interference relation and the greedy assignment are unchanged; only how they are computed:

- Members are numbered densely and interference is a square bit matrix over that numbering,
  not a map of maps.
- An edge is recorded only where a member *becomes* live, against the set live at that point,
  with a block's live-out set marked in full. Every pair simultaneously live at a mark is still
  recorded: pairs already live together were recorded at the previous mark, and any pair
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
- The first error *in function order* is reported, after draining what is in flight, so a
  failing compile says the same thing a serial one would.
- `functionCode` holds no IR, and the merge releases both `m.Funcs[i]` and the local
  `functions[i]` — previously only the former, which freed nothing while the local slice still
  held every pointer. Peak memory fell rather than rose.

Tests (`arm64/parallel_test.go`): objects byte-identical at 1, 2, 3, 8, 64 and 256 workers on a
65-function module built to exercise text layout, intra-module call relocations, DWARF and
heavy spilling; the reported error identical across worker counts; and a high-pressure function
compiled at 8 workers, linked and run for the right answer.

## 4. What it measures

### The floor program

`stdlib_http_tls_client_server.go` against the prebuilt pack. **The box is not idle** — two
sibling jobs are compiling throughout — so every pair below was taken back to back, and the
briefing's 157.6 s idle-box figure is *not* the baseline used here. The baseline is the same
compiler measured under the same conditions.

| | wall | user | cpu | maxrss |
| --- | ---: | ---: | ---: | ---: |
| branch point | 182.80 s | 198.91 s | 110% | 2.68 GB |
| + `slotGroups` rewrite | 48.57 s | 65.99 s | 141% | 2.83 GB |
| + concurrent back end, 1 worker | 53.45 s | 64.99 s | 126% | 2.31 GB |
| + concurrent back end, 64 workers | 29.43 s | 71.27 s | 252% | 2.28 GB |
| final tree, 1 worker | 48.73 s | 65.87 s | 140% | 2.30 GB |
| **final tree, 64 workers** | **30.75 s** | 70.98 s | 241% | 2.30 GB |

- The rewrite is **3.8x** on its own; the concurrency a further **1.6x** measured against its
  own 1-worker leg on the final tree (48.73 → 30.75), which is the honest comparison.
- **Total 5.9x on the program that bounds the matrix.**
- `cpu=241%` is the new ceiling, not 115%. With the back end no longer dominated by one
  function, the single-threaded front end and the serial merge are a much larger share, so
  Amdahl bites early. Getting past this needs the front end, not more back-end workers.
- `goc build-runtime`: 8.11 s → 2.77 s at `cpu=379%`.

### The full capability matrix

`scripts/matrix-timing.sh`, full unsharded matrix, `-count=1 -v -runtime-status-progress`.
The box is shared with two sibling jobs whose load fell over the afternoon, so the branch point
was re-measured here three times rather than compared against the 203.2 s recorded on an
exclusive box. Runs in the order taken:

| run | wall | slowest compile | run phase | census |
| --- | ---: | ---: | ---: | --- |
| branch point | 303.9 s | 278.7 s | 19.0 s | 338/338 |
| this branch | 116.0 s | 99.0 s | 24.3 s | 338/338 |
| this branch | 107.1 s | 90.8 s | 24.9 s | 338/338 |
| branch point | 212.9 s | 199.2 s | 15.2 s | 338/338 |
| this branch | 67.8 s | 55.9 s | 16.9 s | 338/338 |
| **this branch (final tree)** | **71.4 s** | **59.5 s** | 17.3 s | 338/338 |
| **branch point (consecutive)** | **225.8 s** | **211.7 s** | 17.4 s | 338/338 |
| **this branch (consecutive)** | **81.5 s** | **69.0 s** | 18.8 s | 338/338 |

- **3.2x against the branch point measured immediately before it** (225.8 → 71.4); 2.8x
  against the one measured immediately after (225.8 → 81.5).
- **3.0x against §17's exclusive-box 203.2 s**, while contended.
- The slowest single compile fell from ~212 s to ~60 s in the same runs.

**Which term bounds it afterwards:** the same one as before —
`max(slowest single compile, compile CPU / workers) + run phase + setup` — and still the
slowest single compile. In the 71.4 s run that is 59.5 s of compile against 17.3 s of run
phase and ~7 s of setup, with 338 programs' work spread over 61 compile workers. The floor
moved; it did not stop being the floor. The other two levers on `perf/test-suite` reduce
*total* compile CPU, which this work does not, so they compose with it rather than competing.

**Census, every one of the eight runs above: 338 subtests, 338 `--- PASS`, 0 `--- FAIL`,
0 `--- SKIP`; 337 declared PASS, 1 EXPECTED FAILURE, 0 KNOWN GAP.** The single non-passing
capability is the declared one:

    runtime_status_test.go:2431: EXPECTED FAILURE runtime_panic_print_string.go

**There are no other non-passing capabilities.**

## 5. Determinism

### The four fixed in the back end and its analyses

All let Go's map iteration order into generated code:

- `analysis/freq.go`, `Frequency`: a loop's cyclic probability summed float contributions
  **over a map**. Float addition is not associative, so the loop multiplier, every block
  frequency derived from it, and every spill decision the cost model makes from those differed
  between runs of the same compiler.
- `analysis/freq.go`, `redistributeMesh`: the same, for the mesh's total flow.
- `analysis/loopforest.go`: the loop list was built by iterating the latch grouping map, and
  the parent and innermost-loop choices break ties by position in that list.
- `arm64/allocacolor.go`: stack-colouring candidates came from a map, and the stable sort by
  size then broke ties by that order — so which stack allocations shared a slot, and hence the
  frame layout, varied run to run.

### The fifth, which the concurrency exposed

`go test -race ./arm64/` on the synthetic module was clean, and that was not enough.
`go test -race ./goc/` — real Go programs through the real front end — reported **20 data
races**, all on `ir.Block.Preds`, between goroutines compiling *different* functions.

The cause is a **front-end defect that predates this branch**. `gen.funcDecl` resets the
generator's per-function defer state — the slots, the functions, the order, the actions — but
not `deferBlocks`, which `derive()` *does* reset for a closure. `addDeferRecoveryEdges` wires
every block in that list to the function's `deferreturn` block, so the list surviving into the
next function gave the **previous** function a synthetic control-flow edge into the **next**
one's blocks. Dominance, liveness and frequency, all built from that graph, spanned two
functions at once.

It was invisible while the back end compiled one function at a time, because the predecessor
lists those analyses rebuild live on the blocks themselves, so each function overwrote the
previous one's damage on its way past. Concurrency turned a silently-wrong graph into a data
race, which is the only reason it was found.

Fixed at the cause (`goc/compile.go`, one line in the reset block it was missing from).
`TestEachFunctionsControlFlowStaysInsideThatFunction` fails on the unfixed compiler with
exactly that edge —

    main.updateForwardedResults: block start has a successor deferreturn1
    owned by main.updateFloatResult

— and passes with it. The race re-run over eight corpus tests is clean, and the whole suite,
the matrix and `splitdiff` were re-run after it.

**This is the result I would keep if I could keep only one.** Both of the gates I had built
said the concurrency was fine. The run that disagreed was the race detector over real programs.

### Where the §5.10 residue actually is — and it is not the back end

`runtime_defer_capture_allocs.go` still compiles to a different binary nearly every time: **25
distinct executables in 30 compiles at a single back-end worker**. To find out where, I hashed
every function's IR at back-end entry, after lowering, after allocation and after emission,
across two compiles. The result:

- Every function's *content* hash matches between the two runs, except four.
- **441 functions appear at a different position in the module** — the interface-call wrapper
  thunks (`*.interfacecall.*`). Same functions, same code, different addresses, different
  binary. The order is the order `ensureInterfaceCallWrapper` (`goc/compile.go:5198`) is first
  reached while runtime type descriptors are emitted, and that walk is driven from a map.
- The four exceptions (`testing.prettyPrint`, `testing.common.Attr`,
  `testing.common.makeArtifactDir`, `testing.outputWriter.writeLine`) differ **at back-end
  entry** — before `LowerPointers`, before lowering, before allocation. Diffing
  `testing.prettyPrint`'s IR shows two address computations emitted in swapped order
  (`%t108 =l add %t107, 32` / `%t109 =l add %t107, 40` against the reverse).

So after the fixes above, **the arm64 back end is deterministic for this program and every
remaining divergence is in goc's front end.** The briefing expected register allocation; the
evidence says otherwise.

I have **not** fixed those. They are in the front end rather than the back end — outside this
job's lever — and changing the order functions land in a module changes the layout of every
program goc compiles, which deserves its own validation cycle rather than being folded into a
performance change. Both are recorded in RUNTIME_PLAN §5.10 with their locations.

Also noted, not fixed (pre-existing, `-O` only): `opt/inline.go:184` sorts cost-inline
candidates with an unstable `sort.Slice` over a slice built from map iteration, so which
callees are inlined when the budget runs out is not reproducible. The matrix runs `-O` under
`-runtime-opt`.

## 6. Verification

- `gofmt`: clean over every tracked non-`stdlib/` Go file. `go build ./...`, `go vet ./...`:
  clean.
- `make test-unit`: **pass**.
- `make test-goc-corpus`: **pass**, 567 s on the final tree (606 s before the front-end fix).
- `make test-goc-cmd`: **pass**, 150 s on the final tree.
- `go test -race ./goc/` over eight corpus tests and `go test -race ./arm64/`: **clean** on the
  final tree. See §5 for what the first found before it was.
- Full capability matrix: eight runs, table above, always 338/338.

### `analysis/splitdiff`: every corpus program built both ways and run

Every program compiled monolithically and against the prebuilt runtime, both linked and run,
comparing exit status and **full combined output** — stricter than the matrix, whose gate is
the exit status. Run twice, once before the front-end fix and once after:

    programs=358  problems=2
    total CPU compile+link: split=2350.9s mono=3085.7s  ratio=1.31x

**356 identical; 2 differences, and they are the same two the previous job's final tree already
reported** (`bytes_grow_stats.go`, `gomaxprocs_memstats.go`), both printing an allocation count
that differs because the two images differ in size, both exiting 0. No compile failure, no link
failure, no exit-status difference. **No regression against the branch point's result.** Whole
differential 4 m 46 s; slowest monolithic compile in it 43.2 s.

### The corpus differential: is the output worker-independent?

All 358 `goc/testdata` programs compiled three times with the final compiler — twice at 8
back-end workers, once at 1 (run before and after the front-end fix):

| | before the fix | after |
| --- | ---: | ---: |
| all three byte-identical | 319 | **321** |
| the two 8-worker runs already disagreed (nondeterministic program) | 34 | 34 |
| 8-worker runs agreed, 1-worker run differed | 5 | 3 |
| compile failed | 0 | **0** |

The third row is the one that matters, so each such program was compiled **30 times at 1 worker
and 30 at 8**. Every one produces more than one output **at a single back-end worker**, where
the concurrent driver dispatches one function at a time:

| program | at 1 worker | at 8 workers |
| --- | --- | --- |
| `runtime_loopvar_range` | 29 x A, 1 x B | 24 x A, 6 x B |
| `stdlib_log_buffer` | 27 x A, 3 x B | 23 x A, 7 x B |
| `stdlib_runtime_trace_buffer` | 27 x A, 3 x B | — |
| `stdlib_encoding_csv` | 6 distinct | 5 distinct, all also seen at 1 worker |
| `stdlib_encoding_gob_struct_int` | 25 distinct | 26 distinct |
| `stdlib_encoding_json_roundtrip` | 27 x A, 3 x B | 24 x A, 6 x B |
| `stdlib_flag_parse` | 10 distinct | 11 distinct, 9 shared with the 1-worker set |
| `stdlib_image_png_roundtrip` | 26 x A, 4 x B | 24 x A, 6 x B |

The three-compile classifier cannot distinguish a rare variant from a worker-dependent one —
which is also why the flagged set changed completely between the two runs
(`runtime_loopvar_range`, `stdlib_encoding_csv`, `stdlib_encoding_gob_struct_int`,
`stdlib_log_buffer`, `stdlib_runtime_trace_buffer` before; `stdlib_encoding_json_roundtrip`,
`stdlib_flag_parse`, `stdlib_image_png_roundtrip` after). A genuine worker bug would flag the
same programs twice. For the same reason the ~37 nondeterministic programs are a *lower bound*:
30 members are shared between the two sets and the rest drift, so a 3-compile sample undercounts.

The one dash is a leg that was not needed: that program's 1-worker column alone shows it varies
without any concurrency at all. Every other row has both legs, and in each the two columns are
the same set of outputs in different proportions. Controls: `runtime_defer_capture_allocs` gives 25 distinct
outputs in 30 single-worker compiles; `hello` gives 1 in 30.

**No program produced an output at 8 workers that it never produced at 1.** With the unit test
(byte-identical objects from 1 to 256 workers) and the clean race run, that is the case that
the concurrency does not affect the compiler's output.

### Determinism against the branch point

The prebuilt runtime pack — the whole Go runtime — is byte-identical between the branch point
and the `slotGroups` rewrite, as is `hello.go`. The later determinism fixes deliberately change
generated code (they replace an arbitrary map-order result with a fixed one), so byte-identity
against the branch point does not hold past that commit and is not claimed.

## 7. For the sibling jobs and the integration

- **Do not build a reference compiler in a worktree at a different path.** Absolute source
  paths reach the emitted data, so `hello.go` built by a compiler from `.../tmp/ref` differs
  from one built from `.../repo` in 168 bytes of `.data`, with no compiler change at all.
  Revert the files in place instead.
- `GOC_BACKEND_WORKERS` defaults to `GOMAXPROCS`. The matrix runs 61 concurrent compiles, each
  now with up to 64 back-end workers, and that measured faster than the branch point on this
  box — but a job with a smaller CPU share may want to set it.
- This branch touches `goc/compile.go` (one line, in `funcDecl`'s reset block). If
  `ccwork/goc-batch` touches the same function, that is the conflict to look at first.
- The front-end determinism bugs in §5 are unclaimed and precisely located; they are a good,
  contained next job.

## Still unverified

- **No idle-box measurement exists for anything here.** Two sibling jobs were compiling
  throughout; every baseline was re-measured under the same load rather than taken from §17.
- Whether the ~37 nondeterministic corpus programs were *already* nondeterministic at the
  branch point was not measured program-by-program. Every variant reproduces at one back-end
  worker and the concurrency is shown not to introduce one, so this is a completeness gap
  rather than a live suspicion. `runtime_defer_capture_allocs` specifically was nondeterministic
  before and after.
- `make test-goc-coverage` was not run. It was not run by the previous job either, for the
  reason recorded in `docs/report-matrix-speed.md`.
- The front end was profiled but not attacked. At `cpu=241%` it and the serial merge are now
  what bound a single compile; §18 of the plan says what I think is possible there.
- `-O` (`-runtime-opt`) paths were exercised only through the matrix's own configuration, not
  measured separately.
