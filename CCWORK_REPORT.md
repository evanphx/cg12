# Parallelising inside goc: moving the capability matrix's floor

Branch: `ccwork/goc-parallel-b`, off the `ccwork/matrix-speed` tip (`0f4ee02`), which already
carries `perf/test-suite`. The previous job's report has been moved to
`docs/report-matrix-speed.md` so this file is only about this job.

Status: **in progress.** Updated as each result lands; the unverified list is at the end.

## The short version so far

1. **The floor is not a parallelism problem — it is one function.** Profiled, the compile of
   `stdlib_http_tls_client_server.go` spends **66.4% of its whole wall clock inside
   `arm64.slotGroups`**, an all-pairs interference marking that re-adds every live pair to a
   `map[int]map[int]bool` at *every instruction*. Register allocation as a whole is 75.5%;
   the front end is 7.9%, not the 39% a `hello.go` profile suggests.
2. **Rewriting it — same relation, same greedy assignment, bit matrix instead of a map of
   maps, and an edge recorded only where a temp *becomes* live — took the floor program from
   182.8 s to 48.6 s single-threaded (3.76x)** and the runtime pack is byte-identical.
3. **The per-function back end is now compiled concurrently**, merged strictly in function
   order. Objects are byte-identical from 1 to 256 workers by unit test, and the concurrent
   driver is `-race` clean.
4. A **methodology trap** worth recording for the sibling jobs: two goc binaries built from
   trees at *different filesystem paths* produce different programs. Absolute source paths
   reach the emitted data, so a `git worktree` at a different-length path is not a valid
   reference build. The reference here is built in the same directory with only the backend
   files reverted.

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
function's map traffic.

`slotGroups` partitions spilled temps into stack-slot-sharing groups. It marked every pair of
simultaneously-live members at every instruction — O(instructions x live²) map operations,
almost all of them re-adding an edge that was already there.

## 2. The rewrite

`arm64/slotcolor.go`. The interference relation and the greedy assignment are unchanged; only
how they are computed changed.

- Members are numbered densely and interference is a square bit matrix over that numbering,
  not a `map[int]map[int]bool`.
- An edge is recorded only where a member *becomes* live, against the set live at that point,
  with a block's live-out set marked in full. Every pair simultaneously live at a mark is
  still recorded: pairs already live together were recorded at the previous mark, and any
  pair involving a newly-live member is recorded now. The relation is then symmetrised.
- The greedy pass carries, per group, the union of its members' interference, so "does t
  conflict with this group" is one bit rather than a scan of the group.

**Measured on the floor program** (`stdlib_http_tls_client_server.go`, against the prebuilt
pack, back to back on the same box under the same sibling load):

| | wall | user | cpu | maxrss |
| --- | ---: | ---: | ---: | ---: |
| before | 182.80 s | 198.91 s | 110% | 2.68 GB |
| after | **48.57 s** | 65.99 s | 141% | 2.83 GB |

**3.76x, single-threaded.** (The box is not idle — two sibling jobs are compiling. The
briefing's 157.6 s figure was taken on an idle box; 182.8 s is the same compiler under this
job's actual conditions, which is why the before/after pair was taken back to back.)

Output equivalence: the prebuilt runtime pack — the whole Go runtime, the largest module
goc compiles — is **byte-identical** before and after, as is `hello.go`.

## 3. The concurrent back end

`arm64/parallel.go`, with the emit loop in `arm64/mc.go` reduced to an order-preserving merge.

Lowering, register allocation and emission read one function plus read-only module facts
(`Module.SymAlign`, the assembly bundle's call conventions) and produce a result whose every
offset is relative to that function's own start. So the functions are compiled concurrently
and merged strictly in function order: every address, symbol, relocation and DWARF row below
the merge is derived from the merge order, never from which worker finished first.

- Worker count: `GOMAXPROCS`, overridable with `GOC_BACKEND_WORKERS`. The output does not
  depend on it.
- Look-ahead is `2 * workers`, so the number of finished-but-unmerged results follows the
  worker count rather than the module's function count.
- The first error *in function order* is reported, after draining what is in flight, so a
  failing compile reports the same message a serial one would.
- `functionCode` holds no IR, and the merge now releases both `m.Funcs[i]` and the local
  `functions[i]` — previously only the former, which freed nothing while the local slice
  still held every pointer.

Tests (`arm64/parallel_test.go`): objects byte-identical at 1, 2, 3, 8, 64 and 256 workers on
a 65-function module built to exercise text layout, intra-module call relocations, DWARF, and
heavy spilling; the reported error identical across worker counts; and a high-pressure
function compiled at 8 workers, linked and run for the right answer. `-race` clean.

## Progress log

- [done] Profile of the floor program; `slotGroups` named as the bound.
- [done] `slotGroups` rewrite; byte-identical pack; 3.76x on the floor program.
- [done] Concurrent per-function back end; byte-identical by unit test; `-race` clean.
- [running] Is `stdlib_http_tls_client_server.go` deterministic at all? Its executable differs
  before vs after the rewrite even though the pack does not, so the first question is whether
  the program is self-consistent across two runs of the *same* binary.
- [todo] Speedup of the concurrent back end on the floor program and on the full matrix.
- [todo] Corpus-wide byte-identity differential, `analysis/splitdiff`, full suite, full matrix.

## Still unverified

Everything under "todo" above.
