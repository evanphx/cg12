# A checker that says "this escape decision disagrees with the emitted code"

Branch `ccwork/escape-checker`, off `main` (`ad4e9b2`). The task: build the check that
would have caught `ccwork/escape-analysis`'s `2724ac7` before it merged, rather than
letting it arrive as `fatal error: found bad pointer in Go heap` in the collector,
minutes later, in an unrelated goroutine. **Not** to fix the bug — `ccwork/escape-gc-fix`
owns that.

**Result: `opt.FrameEscapes` plus `TestFrameEscapeAudit`.** The test compiles all 384
corpus programs, audits the finished IR, and compares against an accepted baseline. It
passes on `main` and fails on `main + 2724ac7`, naming four publications the compiler
made that it did not make before, two of them in corpus programs and one of them in
`crypto/internal/fips140`. It also fails on `main + 2724ac7 + 9f76498` with three of
those four, which is how the second commit's effect was established.

The runtime counterpart (`cg12checkwb`) was widened too, and the honest headline about it
is in section 2: **it could not have caught this bug, and the reason is structural.**

This file was written as each result landed; sections are in the order they were
measured.

## 1. Reproduced, and the reproduction is configuration-dependent

`main` + `2724ac7` (cherry-picked; only `CCWORK_REPORT.md` conflicted, `goc/compile.go`
merged clean and the applied diff is the same 488 insertions as the original commit).

    go test ./cmd/goc -run TestARM64RuntimeCapabilityStatus/gc-invariants/mark-workers
    runtime: pointer 0x4167d380fd40 to unallocated span span.base()=0x4167d380e000
    fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)
    --- FAIL (32s)

**The failure needs the split build.** Built monolithically (`goc -o out prog.go`) the
same tree passes 20/20 runs. Built the way the matrix does it — `goc build-runtime -o
pack.gocrt -packages ""` then `goc -runtime pack.gocrt -o out prog.go` — it fails 10/10.
Anyone reducing this bug with a plain `goc prog.go` will conclude it is fixed when it is
not. Worth knowing before the next bisect.

Cross-linking the two halves says which one carries it. Both packs have the same stdlib
fingerprint, so either compiler's program module links against either pack:

| program module compiled by | linked against pack from | runs |
| --- | --- | ---: |
| `main` | `main` | 0/6 failed |
| `main` | `main + 2724ac7` | 0/6 failed |
| `main + 2724ac7` | `main` | **6/6 failed** |
| `main + 2724ac7` | `main + 2724ac7` | **6/6 failed** |

**The defect travels with the program module, not with the pack.** And that program
module is not a different compilation from the monolithic one: `goc -runtime` runs the
same whole-program `compile()` and applies the subtraction last, which is why the escape
audit reports exactly the same two findings for `runtime_gc_mark_workers.go` in both
configurations (`runtime.recovery` and `runtime.cgocallbackg1`, on both trees). So the
program's own code is identical in the two builds and only the *linked* runtime differs:
the pack's runtime is compiled by a separate `build-runtime` process from a generated
root, the monolithic one is compiled alongside `main`. Which of those two runtimes is
linked decides whether a program module carrying this defect faults. The mechanism is not
established here and is `ccwork/escape-gc-fix`'s to find; the measurements above are, and
they narrow it a long way.

## 2. Does `cg12checkwb=2` already catch this? No, and the reason generalises

The brief asks this first, so it is answered first, with measurements rather than
reading.

**No.** Neither `cg12checkwb=2` as it stood, nor a `=3` that widens the rule from "a
stack address into a global" to "a stack address into anything that outlives the frame",
catches `2724ac7`. Both were built and run against the failing tree; the collector throws
later exactly as it did without them.

Three separate reasons, each of which matters for anything built on that diagnostic:

1. **The hook was on one function, and it missed the bulk barriers.**
   `cg12CheckWriteBarrierPair` was called only from `runtime.atomicwb`. That is less
   narrow than it sounds — every barriered pointer store goc emits goes `goc_storep →
   runtime.atomicstorep → atomicwb`, so ordinary Go stores were covered. But
   `bulkBarrierPreWrite` and `bulkBarrierPreWriteSrcOnly` push words straight into the
   write-barrier buffer, and those are the paths `typedmemmove`, `growslice` and
   `typedslicecopy` take. This branch adds the same validation to both
   (`stdlib/src/runtime/mbitmap.go`). With it, the mark-workers fault moves from "the
   background marker that happened to drain the buffer" to `main.buildGraph`'s own
   `growslice`, in the goroutine that owns the bad word — a real improvement, and how
   the shape of the corruption below was measured.

2. **A barrier-based check has a duty cycle: it only runs during a mark phase.** A wrong
   escape decision publishes a frame address *whenever the program runs*.
   Measured: on the failing tree, `runtime_slice_pointer_append_gc.go` and
   `runtime_debug_gc_controls.go` each store a frame-resident backing array into a fresh
   heap object, and both run clean under `cg12checkwb=3`, because the store happens with
   `writeBarrier.enabled` false and nothing looks at it. This is the structural reason,
   and it applies to any check placed inside a write barrier.

3. **By the time anything looks, the value is often not a stack address.** In the
   mark-workers fault the rejected word is `0x…79f20`, in a span with `state=1`,
   `elemsize=16`, `limit=0x…79f00` — i.e. *past the last object* of a live 16-byte span.
   That is a pointer to an object that was freed and whose span was recycled into a
   different size class, not a live goroutine stack. A rule phrased as "reject a
   goroutine stack address" cannot see it.

`cg12checkwb=3` is still worth having, and is in this branch: it is the tightest
statement of the invariant available at a barrier, and it is now exercised by
`TestARM64WriteBarrierAuditRunsClean` in `make test-goc-cmd` so it does not become
another mode nobody runs. But it is not the answer to this class of bug.

## 3. The compile-time check: `opt.FrameEscapes`

`opt/framecheck.go`. It runs over the finished IR, after `opt.LowerHeapAllocations`, and
answers one question per function: *does any pointer derived from one of this function's
own frame allocations get stored somewhere that is not part of the same frame?*

That is the verifier for a decision nothing checked. The front end decides, per Go
expression, whether an allocation may stay in a frame; `opt.LowerHeapAllocations` decides
the same thing again for the candidates the front end leaves open. Neither decision is
compared against the code that is finally emitted.

It is a may-analysis over one function at a time. A value is frame-derived when it is an
`OAlloc*`/`OAllocN` result, or reaches one through copies, casts, pointer arithmetic,
phis, the memory helpers that return their destination, or a load from a frame slot a
frame address was stored into. A finding is one of:

- `store` — a plain `OStore*` of a frame-derived value to a destination that is not a
  frame allocation of this function;
- `barrier` — the same through `goc_storep`, which is what a pointer field of a heap
  object receives;
- `return` — a function returning the address of its own frame.

Destinations are classified into categories rather than temporaries — a global, the
caller's result area, memory reached through a parameter, through a call result (with the
callee named), through a loaded pointer — so a finding's identity survives the
renumbering that any unrelated change causes. That is what makes a baseline possible.

Values that merely *reach a call* are deliberately not reported: `&local` passed to a
callee is how Go is written, and whether the callee retains it is the interprocedural
question the front end already answers. What is reported is the store the callee
actually performs.

`GOC_DEBUG_ESCAPECHECK=1` prints the findings from any `goc` compilation, next to the
existing `GOC_DEBUG_NOSPLIT` audit.

## 4. It finds the bug, and that is checked by a test that runs

`TestFrameEscapeAudit` (`goc/framecheck_test.go`) compiles every `goc/testdata/*.go`
— all 384 — collects the findings, and compares them against
`goc/testdata/frame_escape_baseline.txt`. It fails on a publication the baseline does not
list **and** on a listed publication that has gone away, so the file tracks the compiler
instead of drifting from it. It lives in package `goc`, so `make test-goc-corpus` runs
it; no new target and no new configuration that nobody exercises.

| Tree | `TestFrameEscapeAudit` | new findings |
| --- | --- | --- |
| `main` (`ad4e9b2`) | **PASS** | — |
| `main` + `2724ac7` | **FAIL** | 4 |
| `main` + `2724ac7` + `9f76498` | **FAIL** | 3 |

The four on `2724ac7`:

    stdlib/src/crypto/internal/fips140/bigmod/nat.go:951:28  bigmod.Nat.Mul
        barrier into memory reached through a call result $runtime.newobject
    stdlib/src/regexp/regexp.go:1118:30  regexp.Regexp.FindAllStringIndex.func.1116.27
        barrier into memory reached through a loaded pointer
    testdata/runtime_debug_gc_controls.go:32:20        main.main
        barrier into memory reached through a call result $runtime.newobject
    testdata/runtime_slice_pointer_append_gc.go:25:20  main.main
        barrier into memory reached through a call result $runtime.newobject

The two corpus ones are the same shape and are worth reading, because they are the defect
in four lines of Go:

```go
values := make([]*record, 0, 4)      // 2724ac7: alloc8 32 — the backing array is in the frame
for index := 0; index < 128; index++ {
        values = append(values, &record{value: index})
}
runtime.KeepAlive(values)            // boxes the slice into runtime.newobject and stores
                                     // the header, data pointer included, into it
```

On `main` that array is a `runtime.newobject` and there is no finding. Neither program
*fails* at run time, because by line 25 the slice has grown past capacity 4 and the data
pointer is heap again. That is the whole point of a static check: it reports the wrong
decision whether or not this run's data happens to trigger it, and it reports it at
compile time in the function that made it.

`9f76498` removes the `regexp` one and leaves the other three. It does **not** fix
`gc-invariants/mark-workers`: that capability still fails on `main + 2724ac7 + 9f76498`,
with a different signature — `SIGSEGV` in `runtime.getGCMask` /
`mspan.typePointersOfUnchecked` rather than `found bad pointer in Go heap`. So the second
commit changes the failure without removing it. (Measured on a clean cherry-pick: only
`CCWORK_REPORT.md` conflicted; `goc/compile.go` applied without conflict, +49 lines,
matching the original commit.)

## 5. What it does not catch, stated plainly

`gc-invariants/mark-workers` itself produces **no** new finding. Its `buildGraph` does
put `frontier`'s one-element backing array in the frame under `2724ac7` — `%t16 = alloc8
8` where `main` emits `runtime.newobject` — but that array is only ever stored into
another frame slot, so the rule does not fire. Whatever carries it out of the frame is
not a store this analysis can see.

The gaps this analysis has, in order of how likely they are to matter:

- **`OBlit` and `goc_memcpy` of an aggregate.** A byte-copy of a struct that contains a
  frame pointer word is invisible: the analysis has no types, so it cannot say which
  words of the copied region are pointers. The compiler routes *barriered* pointer words
  of an aggregate through `goc_storep` (`storePointerAwareInlineValue`), which is
  covered; a word the compiler decided did not need a barrier travels with the memcpy
  and is not.
- **Slot tracking is per-slot, not per-path.** A frame slot that receives a frame address
  is treated as holding one from then on, which over-reports on paths where it was
  overwritten, and a slot written through a non-constant offset is not tracked at all,
  which under-reports.
- **Calls are not reported.** By design (above), but it means a frame address handed to a
  callee that stores it somewhere this analysis then cannot attribute — for instance into
  memory reached through a parameter three frames down — is only caught if that callee is
  in the same module.

## 6. What it found on `main`, which is not nothing

The baseline is 196 publications on a green tree. It is a record of what the compiler
does, not a certificate that any of it is safe. By category:

| Count | Shape |
| ---: | --- |
| 149 | `barrier` into **the caller's result area** |
| 26 | `barrier` into a **`runtime.newobject`** result |
| 13 | `barrier` into a **loaded pointer** |
| 4 | `return` **to the caller** |
| 2 | `barrier` into a **`runtime.mapassign`** result |
| 2 | `barrier` into an **opaque pointer** |

Two of these were read in full.

**The 149 are one convention.** cg12 represents a `string`, an interface, an `error` or a
`complex128` as a pointer to a sixteen-byte value, and returns one by storing the address
of a *frame slot* into the caller's result area. `syscall.read` is the clearest:

    %t39 =p alloc8 16                       ← the error header, in read's own frame
    ...
    call $goc_storep(p %result1, p %t39)    ← the caller gets a pointer to it
    ret

The caller then holds a pointer into a frame that no longer exists. In practice the
caller copies the two words immediately, which is why nothing has failed; a stack copy
between the return and the copy would not adjust it, and a collector scanning the caller
at that moment would not trace whatever the interface points at. This is
`RUNTIME_PLAN.md` §5.15's residual — the same "a sixteen-byte value lives behind a
pointer to a frame slot" defect, reached through function results rather than through a
closure assignment — and it is much wider than §5.15 records.

**`runtime_goroutine_closure_gc.go:12:9` is a live one, on `main`.**

```go
go run(func() int { return value + 2 }, done)
```

lowers to

    %t4 = call $runtime.newobject(...)   ← the goroutine's argument struct, on the heap
    %t5 =p alloc8 16                     ← the closure funcval, in start's FRAME
    storel $main.start.func.12.9, %t5
    storel %t1, %t5+8                    ← env = &value, also a frame slot
    call $goc_storep(p %t4+8, p %t5)     ← the frame funcval address, into the heap struct
    call $runtime.newproc(p %t4)

`start` then blocks on `<-done`, so its frame is still alive while the goroutine runs,
which is why the program passes. But a stack copy of `start` while the goroutine is live
would leave the heap word pointing at the old stack, which is §5.8's invariant reached by
a third route. This is not recorded anywhere in `RUNTIME_PLAN.md` and was found by
running the checker over a green tree.

Neither was fixed here; fixing them is not this job. They are listed in the baseline so
that the file is honest about being a description rather than an approval, and so the
next change to either is visible.

## 7. Cost

- `TestFrameEscapeAudit`: **2m20s wall, 24 CPU-minutes**, 8 workers, on a tree with a
  warm build cache. Essentially all of it is compiling 384 programs; the analysis itself
  is linear in instruction count and does not register on a profile. It is bounded to 8
  workers so a corpus-wide run does not multiply goc's ~380 MiB peak by the core count.
- `opt.FrameEscapes` is not called during a normal compilation. `GOC_DEBUG_ESCAPECHECK`
  gates the only in-compiler call site, so the default path is unchanged.
- The runtime check adds one `debug.cg12checkwb != 0` test per pointer word in the bulk
  barriers and is off by default.

## 8. Suites

| Suite | Result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | PASS |
| `make test-goc-cmd` | PASS, 293.7s — includes the new `TestARM64WriteBarrierAuditRunsClean` |
| `make test-goc-corpus` | PASS, 713.1s — includes the new `TestFrameEscapeAudit` |
| `make test-goc-status` | **363 subtests / 363 PASS / 0 FAIL / 0 SKIP / 1 declared EXPECTED FAILURE / 0 KNOWN GAP**, 212.9s |
| `make test-goc-status-opt` | **363 subtests / 362 PASS / 1 FAIL / 0 SKIP / 1 declared EXPECTED FAILURE / 0 KNOWN GAP**, 239.4s |
| compilation determinism | 384 corpus programs, 2 compiles each, **384 SAME / 0 DIFFER** |

Censused with `-v` and a per-subtest count, not `ok`. Both arms ran with
`-runtime-status-compile-workers=8`, this job's declared share.

### The one non-passing capability, and it is not this branch's

    --- FAIL: TestARM64RuntimeCapabilityStatus/stack-scan/loop-safepoints   (-runtime-opt only)
    panic: a stack slot live across a loop back edge was not a GC root

**This is pre-existing.** Measured, not assumed: a clean worktree at `origin/main`
(`ad4e9b2`), same box, same targeted run with `-runtime-opt`, fails identically. It is the
open item `RUNTIME_PLAN.md` §6.1 records under "Open: with `-O`, a loop-carried local is
not a GC root", with its own reducer at `goc/testdata/runtime_opt_loop_carried_root.go`,
and §6.1 already reports it failing 10/10 at `main` (`0505d90`). The brief's
non-negotiable states `main` is clean in both arms; for the optimized arm that is not the
case, and this branch does not change it either way. That is the complete list of
non-passing capabilities: one, in one arm, pre-existing.

The determinism sweep is 2 rounds, not §23's depth: 768 compiles, 0 varying. What makes
that proportionate is that nothing on the default compile path changed —
`reportFrameEscapes` returns before doing anything without `GOC_DEBUG_ESCAPECHECK`, and
`opt.FrameEscapes` has no other in-compiler call site — not that 2 rounds would be enough
for a compiler change. `TestCompilingTheSameSourceTwiceGivesTheSameModule` and
`TestParallelBackendIsByteIdenticalToSerial` also ran, inside `make test-goc-corpus`.

## 9. What changed

| File | What |
| --- | --- |
| `opt/framecheck.go` | `opt.FrameEscapes`, the audit |
| `opt/framecheck_test.go` | 12 unit tests: each finding shape, each thing that must stay silent, and key stability under renumbering |
| `goc/framecheck_test.go` | `TestFrameEscapeAudit`, the corpus run and the baseline comparison |
| `goc/testdata/frame_escape_baseline.txt` | the 196 accepted publications, with a header saying what the file is and is not |
| `goc/compile.go` | `reportFrameEscapes`, gated on `GOC_DEBUG_ESCAPECHECK` |
| `stdlib/src/runtime/mwbbuf.go` | `cg12checkwb=3` |
| `stdlib/src/runtime/mbitmap.go` | the same validation in `bulkBarrierPreWrite` and `bulkBarrierPreWriteSrcOnly` |
| `cmd/goc/main_test.go` | `TestARM64WriteBarrierAuditRunsClean`, so the modes are exercised |
| `RUNTIME_PLAN.md` | §25; §6's compiler-emitted-check list; §13 item 5's correction; two additions to §5.10 |

## 10. Still unverified, and what I would do next

- **`gc-invariants/mark-workers`'s own mechanism is not established.** The audit does not
  fire on it, the cross-link measurement says the program module carries it, and that is
  where I stopped: `ccwork/escape-gc-fix` owns the fix, and further reduction is its work,
  not this branch's. Section 5 lists the analysis gaps that could account for it.
- **The 196 baseline entries are classified into shapes, not individually reviewed.** Two
  were read in full (section 6) and both are real hazards. The other 194 are recorded as
  what the compiler does. Anyone treating the file as an approval list is misreading it;
  the header says so.
- **A compiler-emitted store check was designed but not built.** Section 2's second
  reason — a barrier-based check only runs during a mark phase — has an obvious answer:
  have `gen.store` emit `runtime.cg12CheckStackPublication(address, value)` alongside
  `goc_storep`, under a compile-time flag, for packages other than `runtime` (the
  runtime's own legitimate stack publications, `sudog.elem` above all, are all inside it
  and are maintained by hand in `copystack`). That would validate every pointer store,
  not the ones that happen during a mark phase. It is not built here: it is codegen, and
  codegen shipped without a full validation pass is exactly what this project's §14 warns
  about. The static audit covers the same ground without touching the emitted code.
- **The audit does not look at the `-O` configuration.** `TestFrameEscapeAudit` compiles
  without optimization. Since the one non-passing capability in the whole matrix is an
  `-O`-only stack-map defect, an `-O` arm of the audit is the obvious next thing to add,
  with its own baseline. Not done here.
