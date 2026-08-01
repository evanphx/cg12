# Merge gate: `ccwork/merge-verify` (8f29b3f)

`main` (`ad4e9b2`) + `ccwork/escape-gc-fix` + `ccwork/escape-checker`.
This file is written as each result lands; sections are in the order they were measured.
(The previous contents of this file were `ccwork/escape-checker`'s report; it is
unchanged on that branch and in history at `8f29b3f:CCWORK_REPORT.md`.)

---

## 1. THE CONFLICT RESOLUTION IN `stdlib/src/runtime/mbitmap.go` — **NOT CORRECT AS
   RESOLVED. It lost checking power. Fixed on this branch.**

### What the two sides did

Both branches instrumented the *same three* sites in `mbitmap.go`
(`bulkBarrierPreWrite`'s two arms and `bulkBarrierPreWriteSrcOnly`):

    escape-checker:  cg12CheckWriteBarrierPair(addr, *dstx, *srcx)
    escape-gc-fix:   cg12CheckBulkBarrierWord(addr, *dstx, *srcx, dst, src, size)

`escape-gc-fix` additionally instrumented two sites `escape-checker` did not
(`bulkBarrierBitmap` and `typeBitsBulkBarrier`, mbitmap.go:1441 and :1498) — so on
*sites* the resolution is a strict superset (5 vs 3). Verified by diffing each branch's
`mbitmap.go` against `main`; the merged file is byte-identical to `escape-gc-fix`'s.

### The loss

The two functions are **not** in a superset relation, because of the fast-path gate in
the bulk wrapper (`stdlib/src/runtime/mwbbuf.go`):

    func cg12CheckBulkBarrierWord(slot, old, new, dst, src, size uintptr) {
        if !cg12WriteBarrierValueIsBad(old) && !cg12WriteBarrierValueIsBad(new) {
            return                      // <-- only mode 1's rule
        }
        ...record range...
        cg12CheckWriteBarrierPair(slot, old, new)
    }

`cg12CheckWriteBarrierPair` enforces **three** rules, not one:

| mode | rule | in the bulk gate? |
|---|---|---|
| `cg12checkwb=1` | `old`/`new` is a word `findObject` would reject | yes |
| `cg12checkwb=2` | `new` is a goroutine stack address and `slot` is a global (`stackNew`) | **no** |
| `cg12checkwb=3` | `new` is a goroutine stack address and `slot` is not on any stack (`published`) | **no** |

So a bulk copy that publishes a goroutine stack address — a `typedmemmove`,
`growslice`, or `typedslicecopy` of a struct or slice element holding a frame address
into a heap object or a global — returns from `cg12CheckBulkBarrierWord` at the first
`if` and is never seen at modes 2 and 3. On `escape-checker`, those same three sites
called `cg12CheckWriteBarrierPair` directly and *did* see it.

This is not the loss of a mode nobody runs: mode 3 is the escape checker's whole
runtime counterpart, and `TestARM64WriteBarrierAuditRunsClean` (`cmd/goc`, part of
`make test-goc-cmd`) runs six programs under `cg12checkwb=1` **and** `cg12checkwb=3`.
With the resolution as written, that suite's mode-3 arm no longer examines any word
that reaches the buffer through a bulk barrier.

Note the asymmetry that made this easy to miss: it is a *merge-induced* regression, not
a defect on either parent. On `escape-gc-fix` alone, `cg12CheckWriteBarrierPair` had no
`published` rule at all (mode 3 arrived with `escape-checker`), and its mode-2 rule
requires a *global* slot, which a bulk `dst` rarely is — so there the narrow gate cost
almost nothing. The merged `mwbbuf.go` took `escape-checker`'s three-rule
`cg12CheckWriteBarrierPair` and `escape-gc-fix`'s one-rule gate: a combination that
existed on neither parent.

### The fix (committed on this branch)

`cg12WriteBarrierWordIsRejected(slot, old, new)` is factored out as the single gate, and
both `cg12CheckWriteBarrierPair` and `cg12CheckBulkBarrierWord` use it. The bulk path
now enforces exactly the same rule set as the single-word path at every mode, and the
two cannot drift apart again.

Two properties were deliberately preserved rather than "cleaned up":

- The gate still runs *before* `cg12BulkBarrierRange` is written, so that range global
  is still only written on the path that immediately throws. RUNTIME_PLAN.md records
  that an always-written range global raced between two goroutines in
  `bulkBarrierPreWriteSrcOnly` and reported a range from the wrong call. Making the
  wrapper unconditionally record and delegate would have reintroduced exactly that.
- Everything stays `//go:nosplit`, and the fast path's call depth is unchanged
  (`bulk -> gate -> valueIsBad`, as before).

Calling both functions at the three sites — the other repair the brief allowed — was
rejected: it double-scans every barriered word and emits two reports for one bad write.

`cg12CheckWriteBarrierPair` remains live from `atomic_pointer.go:35` and from the bulk
wrapper; nothing became dead. Confirmed by grep.

### One unrelated nit, pre-existing on `escape-checker`, not fixed

`mwbbuf.go`'s doc comment ends "see `cg12StackPublicationChecked`". No such symbol
exists in the tree, on either parent, or on `main`; it names a compiler-side check
RUNTIME_PLAN.md explicitly says was *designed but not built*. A dangling comment
reference, not a behaviour defect. Left alone: it is not the merge's doing.

---

*(gate results follow as they land)*

## 2. Gate item 1 — gofmt / build / vet

    $ gofmt -l .
    stdlib/src/net/http/pprof/testdata/delta_mutex.go
    stdlib/src/runtime/testdata/testgoroutineleakprofile/goker/cockroach10214.go
    stdlib/src/runtime/testdata/testgoroutineleakprofile/goker/cockroach10790.go
    stdlib/src/runtime/testdata/testgoroutineleakprofile/goker/cockroach1462.go
    stdlib/src/runtime/testdata/testgoroutineleakprofile/goker/cockroach16167.go
    stdlib/src/runtime/testdata/testgoroutineleakprofile/goker/cockroach2448.go
    stdlib/src/runtime/testdata/testgoroutineleakprofile/goker/hugo3251.go
    stdlib/src/runtime/testdata/testgoroutineleakprofile/goker/syncthing4829.go
    stdlib/src/runtime/testdata/testprog/gomaxprocs.go
    stdlib/src/runtime/testdata/testprog/gomaxprocs_windows.go
    stdlib/src/runtime/testdata/testprog/stw_mexit.go
    stdlib/src/runtime/testdata/testprog/stw_trace.go
    stdlib/src/runtime/testdata/testprognet/waiters.go

**PASS, with a stated qualification.** Not literally empty, but **pre-existing on
`main` and untouched by the merge**: all thirteen are vendored upstream Go testdata,
and none of them appears in `git diff --name-only ad4e9b2 HEAD`, whose sixteen entries
are the whole merge (listed in §9). Same list, same reason, on `main`. Every file the
merge does touch is gofmt-clean, including the fix in §1.

    $ go build ./...      -> exit 0, no output          PASS
    $ go vet ./...        -> exit 0, no output          PASS

(Both fast, ~2.5 s, because the build cache on this box is warm from the parent jobs;
`go build` on a cold cache is checked in §9's cold run.)

## 3. Gate item 2 — `make test-unit` — **PASS**

    $ make test-unit
    ok  github.com/evanphx/cg12/amd64            0.639s
    ok  github.com/evanphx/cg12/amd64/x64        0.038s
    ok  github.com/evanphx/cg12/analysis         0.039s
    ok  github.com/evanphx/cg12/arm64            7.289s
    ok  github.com/evanphx/cg12/arm64/a64        0.073s
    ok  github.com/evanphx/cg12/bpf              0.690s
    ok  github.com/evanphx/cg12/cmd/cc           0.787s
    ok  github.com/evanphx/cg12/cmd/cg12         0.038s
    ok  github.com/evanphx/cg12/internal/backendtest  0.135s
    ok  github.com/evanphx/cg12/internal/gometa  0.056s
    ok  github.com/evanphx/cg12/internal/runtimepack 0.038s
    ok  github.com/evanphx/cg12/internal/testenv 0.024s
    ok  github.com/evanphx/cg12/interp           0.042s
    ok  github.com/evanphx/cg12/ir               0.042s
    ok  github.com/evanphx/cg12/lift             0.967s
    ok  github.com/evanphx/cg12/link             1.970s
    ok  github.com/evanphx/cg12/lower            0.039s
    ok  github.com/evanphx/cg12/obj              0.311s
    ok  github.com/evanphx/cg12/opt              0.944s
    ok  github.com/evanphx/cg12/parse            0.672s
    ok  github.com/evanphx/cg12/pe               1.267s
    ok  github.com/evanphx/cg12/plan9asm          0.267s
    ok  github.com/evanphx/cg12/plan9asm/sem     0.040s
    ok  github.com/evanphx/cg12/wasm             0.043s
    real 0m8.814s   EXIT 0

24 packages `ok`, 10 `[no test files]`, none cached (`go test` printed real per-package
times, not `(cached)`).

**Counted, not assumed.** Re-run with `-count=1 -v` over the identical package list:

    --- PASS (top level)   1073
    --- PASS (incl. subtests) 1556
    --- FAIL                  0
    --- SKIP                339

The 339 skips are all missing-external-tool guards, not merge-related: `ld.lld not
available` (×~70), `llvm-mc not available` (×~35), `wasmtime not available` (×~25), `no
privilege to load eBPF`, and the x86 host guards. `opt` — the package `escape-checker`
adds `framecheck.go` to — is in the list and passes.

## 4. The A/B that proves §1 — and that `cg12checkwb` fires at those three sites

Two ~50-line control programs, compiled with the merged `goc` (sources kept at
`$TMPDIR/wb_bulk_publish.go` and `wb_store_publish.go`; both reproduced verbatim at the
end of this file). Each publishes a laundered goroutine-stack address into a heap
object while a second goroutine keeps a mark phase running, one through
`copy()`/`typedslicecopy` (the **bulk** path), one through a plain pointer store (the
`atomicwb` path the merge did not touch).

    GODEBUG=cg12checkwb=3

| runtime | bulk-copy control | single-store control |
|---|---|---|
| **as merged** (8f29b3f, `mwbbuf.go` checked out at that commit) | `bulk publish control finished without a report`, exit 0 | throws, exit 2 |
| **with the §1 fix** (b2e96c5) | **throws, exit 2** | throws, exit 2 |

The store arm is the control on the control: it fires in both, which proves the address
really is a frame address and the mode really is on — so the bulk arm's silence on the
as-merged runtime is the gate, not the program. Verbatim, on the fixed runtime:

    cg12checkwb: pointer write barrier published a goroutine stack address into storage that outlives the frame
    cg12checkwb: slot=0x3a37b0474200 old=0x3a37b05a3c28 new=0x3a37b05a3c28 bad=new-is-stack
    cg12checkwb: bulk copy dst=0x3a37b0474200 src=0x3a37b05a3c88 size=16
    cg12checkwb: bulk-dst 0x3a37b0474200 span base=0x3a37b0474000 limit=0x3a37b0475e00 state=1 elemsize=512 offset=0x200
    cg12checkwb: bulk-dst object base=0x3a37b0474200 size=512 head=0x3a37b05a3c28
    cg12checkwb: bulk-src 0x3a37b05a3c88 span base=0x3a37b05a0000 limit=0x3a37b05a8000 state=2 elemsize=16384 offset=0x3c88
    cg12checkwb: src[0] = 0x3a37b05a3c28
    cg12checkwb: src[1] = 0x3a37b05a3c28
    fatal error: cg12checkwb: a frame address was stored into storage that outlives the frame

and on the as-merged runtime, same binary source, same environment:

    bulk publish control finished without a report

That answers the brief's second question at the same time: **the sites do still fire.**
The `bulk copy dst=/src=/size=` lines are printed only when `cg12BulkBarrierRange.valid`
is set, and only `cg12CheckBulkBarrierWord` sets it — so the report above *is* one of
the three resolved `mbitmap.go` sites executing.

**Mode 1 fires there too**, shown with the real defect rather than a synthetic one.
`goc` built at `ad40d76` (the commit before the type-mask padding fix) with the merged
tree's `mwbbuf.go` dropped in — pre-fix codegen, merged+fixed barrier code — compiling
`goc/testdata/runtime_gc_type_mask_padding.go`, run 12× at `GOGC=10`,
`GODEBUG=cg12checkwb=1`: **7/12 runs throw at a bulk site**, e.g.

    cg12checkwb: pointer write barrier buffered a word the collector will reject
    cg12checkwb: slot=0x36a793e10b58 old=0x0 new=0x36a793922000 bad=new
    cg12checkwb: bulk copy dst=0x36a793e10980 src=0x36a7935af6d0 size=16
    cg12checkwb: bulk-src object base=0x36a7935af6d0 size=16 head=0x36a793917350
    cg12checkwb: src[0] = 0x36a793917350
    cg12checkwb: src[1] = 0x36a7939173b0

(the other 5/12 die first in the scan, as "marked free object in span" — the barrier
check races the collector, which is the known duty-cycle limitation RUNTIME_PLAN.md
records, not a gap in the instrumentation.)

That same `ad40d76` build also gives an **independent confirmation of gate item 7's
"before"**: the reducer fails **20/20** at `GOGC=10` on the pre-fix compiler.

## 5. Gate item 7 — the `runtime_gc_type_mask_padding` reducer — **PASS (0/20)**

Compiled with the merged tree's `goc`, run 20× at `GOGC=10`:

    $ for i in $(seq 1 20); do GOGC=10 ./reducer_merged; done
    type mask padding ok        (×20)
    MERGED+FIXED reducer: 0/20 failed

Independently measured "before", so the 0/20 means something: `goc` built at
`ad40d76` — the commit immediately before `9c7a209` "pad every type's GC pointer mask
to a whole uintptr" — compiling the same source, run 20× at `GOGC=10`:

    pre-fix goc reducer: 20/20 failed
    runtime: marked free object in span 0xf102a5df1ea8, elemsize=16 freeindex=11 ...

20/20 before, 0/20 after, on this box, this hour. Matches the figure the brief cites.

A note for whoever tries to reproduce the "before" by hand-reverting the fix rather
than checking out the parent commit: `paddedPointerMask` has **two** call sites in
`goc/compile.go` (6327, in `ensureTypeTag`, and 13372). Reverting only the first — the
one the fix commit's diff shows — leaves the reducer passing 20/20 and looks like the
bug was never there.

## 6. Gate item 9 — determinism — **PASS (8/8 pairs byte-identical, plus cold==warm)**

Four capability programs, compiled twice each on both arms with the merged tree's
`goc`, `sha256sum` over the linked executables:

    default arm                                        -O arm
    f3af45e2…  gc_struct.1                             5a0b168d…  gc_struct.O1
    f3af45e2…  gc_struct.2                             5a0b168d…  gc_struct.O2
    d50e370b…  runtime_gc_mark_workers.1               5fd16d2e…  runtime_gc_mark_workers.O1
    d50e370b…  runtime_gc_mark_workers.2               5fd16d2e…  runtime_gc_mark_workers.O2
    7b266155…  runtime_goroutine_channel.1             218bd0e8…  runtime_goroutine_channel.O1
    7b266155…  runtime_goroutine_channel.2             218bd0e8…  runtime_goroutine_channel.O2
    83f01158…  runtime_slice_pointer_append_gc.1       f9afb92d…  runtime_slice_pointer_append_gc.O1
    83f01158…  runtime_slice_pointer_append_gc.2       f9afb92d…  runtime_slice_pointer_append_gc.O2

8 pairs, 8 matches, 0 differences.

Additionally, **cold equals warm**: recompiling with `CG12_NOCACHE=1` reproduces the
cached image bit for bit (`d50e370b…` and `f3af45e2…` again), so the compile cache is
not hiding a nondeterministic path — the thing §9's cold matrix run is really testing.

## 7. Was anything else dropped? A mechanical audit of the whole merge

Being adversarial about §1 raises the obvious follow-up: *is `mbitmap.go` the only place a
side went missing?* Answered mechanically rather than by reading. Five files are touched
by **both** parents (`git diff --name-only ad4e9b2 <parent>`, intersected):

    CCWORK_REPORT.md   RUNTIME_PLAN.md   goc/compile.go
    stdlib/src/runtime/mbitmap.go   stdlib/src/runtime/mwbbuf.go

The other eleven are touched by exactly one parent and the merge kept that parent's
version byte for byte (checked with `git rev-parse <commit>:<path>`).

For each shared file: take `main`'s version, apply `diff(main → escape-gc-fix)`, then
apply `diff(main → escape-checker)`, and compare with `8f29b3f`'s version.

| file | both patches apply? | merged == union? |
|---|---|---|
| `goc/compile.go` | yes | **yes** — no hunk lost |
| `stdlib/src/runtime/mwbbuf.go` | yes | **yes** — no hunk lost |
| `stdlib/src/runtime/mbitmap.go` | **no**, 3/3 of escape-checker's hunks reject | merged == the escape-gc-fix side alone |
| `RUNTIME_PLAN.md` | 3/4 | superset of the union (the resolver hand-merged the prose); nothing dropped |
| `CCWORK_REPORT.md` | no | merged == escape-checker's report; `escape-gc-fix`'s 340-line report is **not** in the merge |

So `mbitmap.go` is the *only* code file where a side was dropped, and that is exactly
§1. `goc/compile.go` — the other file both branches edited, and the one that would have
been far worse to get wrong — is a clean union. That is a real result, not an
assumption: it is the check that would have caught a second §1 if there were one.

The `CCWORK_REPORT.md` row is a documentation loss, not a code one: each branch's report
survives on its own branch and in history (`git show 13c94be:CCWORK_REPORT.md`). Worth
knowing before anyone looks for the GC fix's write-up on `main` and does not find it.

## 8. The §1 fix does not create false positives

The widened gate makes the bulk barrier paths enforce two rules they were not enforcing,
so the first question about it is whether anything legitimate now trips.
`TestARM64WriteBarrierAuditRunsClean` is exactly that control — six allocating,
collecting, channel-blocking programs run under `cg12checkwb=1` **and** `cg12checkwb=3`
— and on the fixed tree it passes with all six subtests:

    --- PASS: TestARM64WriteBarrierAuditRunsClean (18.11s)
        --- PASS: .../gc_struct.go (3.02s)
        --- PASS: .../runtime_goroutine_channel.go (2.92s)
        --- PASS: .../runtime_select_channels.go (2.91s)
        --- PASS: .../runtime_channel_struct_pointer_gc.go (2.92s)
        --- PASS: .../runtime_goroutine_closure_gc.go (3.41s)
        --- PASS: .../runtime_slice_pointer_append_gc.go (2.93s)

Note what this suite is worth **after** the fix compared with before: on the as-merged
tree its `cg12checkwb=3` arm could not have rejected anything a `typedmemmove`,
`growslice` or `typedslicecopy` buffered, because the gate returned first (§1, §4). It
now covers those words, and still passes. The rest of `make test-goc-cmd` is §10.

---

# PART TWO — the heavy suites, run in the foreground at `61ba39d`

The previous job ended its session with these suites still running under background
monitors and was never re-invoked; the processes were killed and the results never
existed. Everything below was run in the foreground of a single session and waited
on to completion. Where a suite did not complete, it is marked UNVERIFIED rather
than assumed.

Tree under test: `ccwork/merge-gate-escape` = `61ba39d`, working tree clean.
Box: 64 cores, 250 GiB.

## 9. Gate item 1 — the corpus, `go test ./goc/... -parallel 10` — **FAIL**

```
go test -timeout 40m -parallel 10 -v ./goc/...
FAIL	github.com/evanphx/cg12/goc	741.464s
```

Census of the `-v` log (`=== RUN` lines: 598):

| outcome | count |
| --- | ---: |
| `--- PASS` | 597 |
| `--- FAIL` | 1 |
| `--- SKIP` | 0 |

597 + 1 = 598 = the `=== RUN` count, so nothing was skipped or swallowed. 741 s is in
line with the ~600 s `escape-gc-fix` recorded for `make test-goc-corpus`, plus the
143 s `TestFrameEscapeAudit` that `escape-checker` added; the suite really ran.

The single failure is `TestFrameEscapeAudit` — which is also gate item 6, so items 1
and 6 fail together and for the same reason. It is analysed in §10.

```
--- FAIL: TestFrameEscapeAudit (143.54s)
    framecheck_test.go:77:
        a frame address is published past its frame in a place
        testdata/frame_escape_baseline.txt does not list.
          stdlib/src/crypto/internal/fips140/bigmod/nat.go:951:28	crypto/internal/fips140/bigmod.Nat.Mul	barrier	memory reached through a call result $runtime.newobject
              first seen compiling: stdlib_crypto_ecdsa.go
          testdata/runtime_debug_gc_controls.go:32:20	main.main	barrier	memory reached through a call result $runtime.newobject
              first seen compiling: runtime_debug_gc_controls.go
          testdata/runtime_slice_pointer_append_gc.go:25:20	main.main	barrier	memory reached through a call result $runtime.newobject
              first seen compiling: runtime_slice_pointer_append_gc.go
```

Three publications appeared that the accepted baseline does not list; nothing
vanished. Every other test in `./goc/...` passes.

## 10. Gate item 6 — `TestFrameEscapeAudit` — **FAIL**, and it is the merge that causes it

Item 6 is the same failure as §9's; run on its own it fails the same way:

```
go test -run '^TestFrameEscapeAudit$' -v ./goc      →  --- FAIL (143.54s)
```

### It is not pre-existing on either parent

The merge-base of the two parents is `ad4e9b2` = `main`, where this test does not
exist — `escape-checker` is what adds it. So the meaningful control is the checker
parent itself, which is `main` plus the test and nothing else:

| tree | what it is | `TestFrameEscapeAudit` |
| --- | --- | --- |
| `origin/ccwork/escape-checker` `f09d58d` | merge-base `ad4e9b2` + the audit + the baseline | **PASS** (138.36 s) |
| `7854888` on `escape-gc-fix` | merge-base + `ccwork/escape-analysis`, audit files copied in | **FAIL**, the same 3 findings |
| `61ba39d` (this merge) | both parents + `b2e96c5` | **FAIL**, the same 3 findings |

`opt/framecheck.go`, `goc/framecheck_test.go` and `goc/testdata/frame_escape_baseline.txt`
are byte-identical between `escape-checker` and the merge, and both corpus programs
named in the findings are byte-identical too, so the whole difference is on the
compiler side.

The `7854888` row localises it further. That commit is `escape-gc-fix` at the moment
it merged `ccwork/escape-analysis`, i.e. **before** `9c7a209` (the GC-mask padding
fix), before `2bd5089`/`536125f` (the reducer and its unit test), and before
`b2e96c5`. Reproducing it there was done by copying only the four audit files onto
that commit and leaving `goc/compile.go` alone — an earlier attempt that also took
`compile.go` from the checker branch was discarded, because that file is where
`escape-analysis` lives and taking it would have reverted the very change under test.

**Conclusion: the three publications come from `ccwork/escape-analysis` (`2724ac7`
+ `9f76498`), which arrived through the `escape-gc-fix` parent. Neither the GC mask
padding fix this merge exists to deliver, nor `b2e96c5`, is implicated.** The merge is
the first tree in which that compiler change and the checker that verifies it are in
the same place, so this is a defect the merge *reveals* rather than one it creates —
but it is a defect, and it fails the gate on this commit.

### What the three findings are

All three have the same shape: `barrier … into memory reached through a call result
$runtime.newobject` — an allocation left in the frame whose address is then written,
through the write-barrier helper, into a heap object. Dumping the IR for
`runtime_slice_pointer_append_gc.go` (line 25 is `runtime.KeepAlive(values)`):

```
loc 10 31
  %t4  =p alloc8 32                      ; make([]*record, 0, 4) backing array — IN THE FRAME
  %t5  =p call $goc_memset(p %t4, w 0, l 32)
  storel %t4, %t2                        ; …its address into the frame slice header
...
loc 25 20
  %t75 =p loadl %t2                      ; values.ptr  (may be %t4)
  %t80 =p call $runtime.newobject(...)   ; the heap box for the interface conversion
  call $goc_storep(p %t80, p %t75)       ; BARRIER: a frame address into a heap object
```

`runtime.KeepAlive(values)` takes an `interface{}`, so the slice header is boxed into
a fresh `runtime.newobject` and the data pointer is stored into it with a write
barrier. `escape-analysis` decided the backing array may stay in the frame; the boxing
then publishes its address into the heap. A heap object holding a goroutine-stack
pointer is exactly the "bad pointer in the heap" fault the audit was written to catch,
and exactly the class of `2724ac7`.

`FrameEscapes` is a may-analysis, so it is worth being precise about how much of a
hazard each site is. In these two corpus programs the loop appends 128 elements into a
cap-4 slice, so `runtime.growslice` has certainly replaced the data pointer with a heap
one before line 25 is reached, and the box receives a heap address at run time. That is
consistent with both programs passing the capability matrix and the corpus. It is not,
however, a reason to dismiss the finding: the store is emitted unconditionally, and the
same code shape with an append count inside the initial capacity would put a live stack
address into a heap object. The third finding, `bigmod.Nat.Mul` at `nat.go:951:28`
(`return x.Mod(&Nat{limbs: T}, m)`), is on the crypto path reached by
`stdlib_crypto_ecdsa.go`.

Deciding whether each is a live fault or a conservative report is fix work, not gate
work, and it is not done here. The gate's finding is narrower and firm: **a test that
this merge brings in fails on this commit, it fails because of code the other parent
brings in, and it passes on both the merge-base and the parent that owns the test.**

## 11. Gate item 2 — `make test-goc-cmd` — **PASS**

```
go test -timeout 15m -skip 'TestARM64RuntimeCapabilityStatus' ./cmd/goc/...
ok  	github.com/evanphx/cg12/cmd/goc	301.490s
```

Run with `GOFLAGS=-v` so the run could be censused; the target is otherwise unmodified.

| outcome | count |
| --- | ---: |
| `--- PASS` | 105 |
| `--- FAIL` | 0 |
| `--- SKIP` | 2 |
| `=== RUN` | 107 |

105 + 2 = 107, so nothing vanished. The two skips are the pre-existing
`TestStandardLibraryRuntimeAssemblyIsTranslated` and
`TestTranslatedAssemblyPrecedesRuntimeTextEnd`, both skipping in 0.00 s.
301 s against the ~292 s / ~282 s `escape-gc-fix` recorded for the same target: the
suite really ran.

## 12. Gate item 3 — `make test-goc-status`, the full capability matrix — **PASS**

```
go test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/... \
	-args -runtime-status-shards=1 -runtime-status-shard=0
ok  	github.com/evanphx/cg12/cmd/goc	103.572s
```

Unsharded, `GOFLAGS=-v` for the census, target otherwise unmodified.

**PASS/FAIL set: 364 capabilities PASS, FAIL set is empty.** `=== RUN` lines: 365 =
364 capability subtests + the parent. `--- PASS`: 365 (364 subtests + parent).
`--- FAIL`: 0. No subtest ran in 0.00 s. The 364 matches the default-arm census
`escape-gc-fix` recorded exactly (363 on `main` + `gc-invariants/type-mask-padding`),
so the merge neither gained nor lost a capability.

One of the 364 passes is the declared `expectedFailure`
`defer-panic/panic-string-output` (`runtime_status_test.go:2687: EXPECTED FAILURE
runtime_panic_print_string.go`), exactly as on both parents.

The 364 by category:

```
runtime-packages 45  core-types 40  goroutine 30  gc 30  defer-panic 22
stdlib-io 17  stdlib-encoding 15  stdlib-netpoll 13  stdlib-os 12  stdlib-crypto 12
stdlib-text 10  stdlib-runtime-diagnostics 8  stack 8  stdlib-math 6  stdlib-http 6
stdlib-generics 6  stdlib-bytes 6  stack-scan 6  loop-variables 6  gc-stress 6
stdlib-signals 5  stdlib-netpoll-stress 4  gc-invariants 4  stdlib-sync 3
stdlib-net-values 3  stdlib-image 3  stdlib-compress 3  scheduler-stress 3
assignment-targets 3  stdlib-url 2  stdlib-runtime-values 2  stdlib-log 2
stdlib-hash 2  stdlib-fmt 2  stdlib-containers 2  stdlib-archive 2
print-builtin 2  closure-capture 2  stdlib-unicode 1  stdlib-time 1  stdlib-testing 1
stdlib-runtime 1  stdlib-path 1  stdlib-os-process 1  stdlib-mime 1  stdlib-fs 1
stdlib-flag 1  stdlib-errors 1  stdlib-context 1
```
49 categories, 364 capabilities.

**On the 103 s wall clock.** That is much shorter than the "many minutes" the Makefile
warns about, so it was checked rather than accepted. The subtests are parallel: their
self-reported durations sum to 9383 s (mean 25.8 s each), which is real work
overlapped across the box's 64 cores, and the pack cache was warm from §9 and §11.
Nothing was skipped — 365 `=== RUN` lines, 364 capability results, no zero-duration
subtest, and the run-cold repeat in §14 does the same 364 with the cache bypassed.

## 13. Gate item 4 — `make test-goc-status-opt`, the `-O` arm — **FAIL, pre-existing**

```
go test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/... \
	-args -runtime-opt -runtime-status-shards=1 -runtime-status-shard=0
FAIL	github.com/evanphx/cg12/cmd/goc	111.232s
```

**PASS/FAIL set: 363 capabilities PASS; the FAIL set is exactly one —
`stack-scan/loop-safepoints`.** `=== RUN`: 365 (364 subtests + parent).
`--- PASS`: 363. `--- FAIL`: 2 (the subtest and the parent it fails). 363 + 1 = 364,
the same 364 as the default arm, so the `-O` arm ran the whole matrix too.

```
--- FAIL: TestARM64RuntimeCapabilityStatus/stack-scan/loop-safepoints (0.02s)
    runtime_status_test.go:2664: runtime_stack_scan_loop_safepoints.go should pass: exit status 2
        cg12scanroots: main_carried local slot 27 at 0x219ce97bda58 retains 0x219ce976e0d0 size 16 ...
        collected while live: carried-0 at carried before rewrite
        panic: a stack slot live across a loop back edge was not a GC root
        main_drain() <- main_carried() <- main_main()
```

### Checked against the merge-base directly, not assumed

The merge-base of the two parents is `ad4e9b2` = `main`. Run there, in its own
worktree, same command restricted to the one capability:

```
(worktree at ad4e9b2)
go test -run '^TestARM64RuntimeCapabilityStatus$/^stack-scan$/^loop-safepoints$' \
    ./cmd/goc -args -runtime-opt
--- FAIL: TestARM64RuntimeCapabilityStatus/stack-scan/loop-safepoints (0.02s)
        collected while live: carried-0 at carried before rewrite
        panic: a stack slot live across a loop back edge was not a GC root
FAIL	github.com/evanphx/cg12/cmd/goc	32.205s
```

Same capability, same assertion, same message, on the merge-base. **This failure is
pre-existing and is not caused by the merge.** It is an `-O`-only stack-map defect,
independent of the GC mask padding; `escape-gc-fix` reached the same conclusion from
its own measurements (its §10: `main` fails it 3/3 individually and 1/363 in the full
`-runtime-opt` arm), and this run confirms it on the merge-base from this gate.

It does not block the merge on its own. It does mean the `-O` arm is not green on
`main` either, which is worth stating plainly rather than filing under "matrix passes".

## 14. Gate item 5 — the matrix run cold, `CG12_NOCACHE=1` — **PASS**

```
CG12_NOCACHE=1 GOFLAGS=-v make test-goc-status
ok  	github.com/evanphx/cg12/cmd/goc	104.970s
```

**PASS/FAIL set: the same 364 capabilities PASS, FAIL set is empty.** `=== RUN`: 365.
`--- PASS`: 365 (364 + parent). `--- FAIL`: 0. Identical set to the warm §12 run,
per-subtest durations differing as expected between runs (e.g.
`gc-invariants/type-mask-padding` 0.68 s warm, 0.31 s cold).

### The cold run took the same wall clock as the warm one, so that was checked too

104.97 s cold against 103.57 s warm looks like `CG12_NOCACHE=1` did nothing. It is not
what happened. The status test spawns `goc build-runtime` children, and those consult
the on-disk pack cache at `~/.cache/cg12/runtime-pack`, so the switch is observable as
writes into that directory. Counting files by mtime window:

| window | suite | pack files written |
| --- | --- | ---: |
| 19:05–19:33 | §9 corpus, §11 `test-goc-cmd` | 0 |
| 19:33–19:36 | §12 matrix, default arm | **8** |
| 19:36–19:39 | §13 matrix, `-O` arm (+ the `ad4e9b2` control) | 13 |
| 19:39–19:42 | §14 matrix, `CG12_NOCACHE=1` | **0** |

Two things follow. First, `CG12_NOCACHE=1` really reached the `goc build-runtime`
children: the run wrote nothing to the cache, where every other matrix run wrote its
packs. (It also disables `goc`'s in-process source-world sharing, the other reader of
that variable.) Second, §12 was itself a cache **miss** — it wrote 8 packs, taking
about 24 s at 19:33:12–19:33:36 before the capabilities started — because the cache key
hashes the `goc` binary and the stdlib tree, and this tree had never been built on this
box. So the warm and cold runs cost the same because both built their packs from
scratch; the equality is an explanation, not a missing step.
