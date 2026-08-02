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

# The escape walk's two publication holes (ccwork/escape-frame-publication)

Branch `ccwork/merge-gate-escape`, starting at 61ba39d. `TestFrameEscapeAudit`
reproduces on that tip exactly as the gate reported, in 143s:

    --- FAIL: TestFrameEscapeAudit (142.84s)
      stdlib/src/crypto/internal/fips140/bigmod/nat.go:951:28  bigmod.Nat.Mul  barrier  memory reached through a call result $runtime.newobject
      testdata/runtime_debug_gc_controls.go:32:20              main.main       barrier  memory reached through a call result $runtime.newobject
      testdata/runtime_slice_pointer_append_gc.go:25:20        main.main       barrier  memory reached through a call result $runtime.newobject

## 1. Two distinct holes, not one

The three findings share a shape -- a frame address stored through the write
barrier into a `runtime.newobject` result -- but they are two different missing
edges in `goc/compile.go`'s escape walk. Both are instances of one rule the walk
does not have: **copying a value into freshly allocated storage that may be in
the heap is a publication of everything reachable from that value.**

### 1a. Interface boxing is not modelled at all

`runtime_slice_pointer_append_gc.go:25` and `runtime_debug_gc_controls.go:32`
are both `runtime.KeepAlive(values)`. Verified IR (`goc -emit-ir`, `$main.main`,
before the fix):

    %t2  =p alloc8 24                     ; the `values` slice header, in the frame
    %t4  =p alloc8 32                     ; make([]*record,0,4) backing array, IN THE FRAME
    %t5  =p call $goc_memset(p %t4, w 0, l 32)
    storel %t4, %t2
    ...
    loc "runtime_slice_pointer_append_gc.go" 25 20
    %t75 =p loadl %t2                     ; values.ptr -- may be %t4
    %t80 =p call $runtime.newobject(p $.goc.runtime.type.main_record.57cf6c680c1a8218)
    call $goc_storep(p %t80, p %t75)      ; BARRIER: frame address into a heap object
    %t81 =p add %t80, 8
    storel %t77, %t81
    %t82 =p add %t80, 16
    storel %t79, %t82
    %t83 =p alloc8 16                     ; the interface descriptor (frame)
    storel $.goc.type.main_record.57cf6c680c1a8218, %t83
    %t84 =p add %t83, 8
    storel %t80, %t84
    ...
    call $runtime.KeepAlive(:...descriptor_interface... %t88)

`adaptValueToInterface` (goc/compile.go:5597) allocates the interface payload
with `allocateTyped` -- a `runtime.newobject` candidate -- for every source type
that is not pointer-shaped (`isDirectInterfaceType`), and stores the value into
it. A slice header is not pointer-shaped, so the backing-array pointer goes into
the heap box.

The walk never sees this. `nonEscapingObjectUse`'s `*ast.CallExpr` case asks
only `parameterDoesNotEscape(runtime.KeepAlive, 0)`; `KeepAlive`'s body is
`if cgoAlwaysFalse { println(x) }`, `println` is on the walk's benign-builtin
list, so the answer is "does not escape" and `make([]*record,0,4)` stays in the
frame. The boxing happens in the *caller*, before the callee is entered, so no
answer about the callee can be right here. **The user's reading is confirmed:
the walk models no interface conversion anywhere** -- not the call-argument one,
not an explicit `any(x)`, not an assignment to an interface variable.

Note the second half of the reading is *not* the defect: the walk does follow a
slice header to its backing array, because it is the `make` call's own
placement that is being decided, and `values`' every use is enumerated. What it
does not do is recognise the use at line 25 as a publication.

### 1b. `&T{v}` decides its own storage with a different, cruder walk

`bigmod/nat.go:951:28` is `return x.Mod(&Nat{limbs: T}, m)`; column 28 is `T`.
Verified IR (`$crypto/internal/fips140/bigmod.Nat.Mul`, before the fix):

    %t62  =p alloc8 512                   ; T := make([]uint, 0, preallocLimbs*2), IN THE FRAME
    %t63  =p call $goc_memset(p %t62, w 0, l 512)
    storel %t62, %t60                     ; into T's frame header
    ...
    loc ".../nat.go" 951 16
    %t138 =p call $runtime.newobject(p $...bigmod_Nat...)   ; &Nat{...} -- ON THE HEAP
    loc 951 28
    %t140 =p loadl %t60                   ; T.ptr -- may be %t62
    call $goc_storep(p %t138, p %t140)    ; BARRIER: frame address into a heap object

Here the walk *does* have a rule -- `compositeElementDoesNotEscape`: "the element
escapes exactly when the composite value does" -- and the rule is right. The
defect is that the two sides answer with different walks:

* the element's side climbs through the `&` in `valueDoesNotEscapeWithin`'s
  `*ast.UnaryExpr` case and asks `parameterDoesNotEscape(Nat.Mod, 0)`, which
  says the parameter does not escape;
* the literal's own storage is placed by `nonEscapingAddress`
  (goc/compile.go:2290), a far cruder walk whose `*ast.CallExpr` case accepts
  only a one-argument *type conversion* and returns "escapes" for every other
  call. `x.Mod(&Nat{limbs: T}, m)` has two arguments, so the `Nat` is heap.

So the literal is in the heap and its element is in the frame, from the same
front end, in the same statement. 9f76498 already found one face of this (the
`resultLeakBody != nil` guard in that same `*ast.UnaryExpr` case); the guard is
too narrow, because `&` makes fresh storage on the ordinary path too.

## 2. The fix

One rule, applied at both holes: a value copied into freshly allocated storage
that may be in the heap has escaped. Details and measurements below.

## 3. What the fix is

`goc/compile.go`, three additions. No finding was baselined; all three are fixed.

**3a. `boxedIntoInterface`, consulted by all three escape walks.** A value
converted to an interface type by the context it sits in has been copied into
fresh, possibly-heap storage, so the walk stops there and answers "escapes".
The predicate is `interfaceConversionAllocates(source)` -- not already an
interface, not a shared type parameter, not `isDirectInterfaceType` -- against
`interfaceConversionTarget(expression)`, which names the type the context
converts to for the contexts that perform an assignment conversion: a call
argument (variadic-aware), an explicit conversion, `panic`, an assignment, a
`var` initialiser, a `return`, a channel send, and a composite-literal element.
Contexts it does not name return nil, and every one of those is a context the
walks already answer conservatively, so nil never weakens an answer.

Hooked in at:

* `nonEscapingObjectUse` -- head of the function, before the use is classified;
* `valueDoesNotEscapeWithin` -- head of the climb loop, before the
  assigned-destination shortcut, which would otherwise answer first;
* `addressEscapesWithin` -- head of the climb loop, answering "escapes".

Pointer-shaped sources are excluded because `adaptValueToInterface` stores them
straight into the two-word descriptor, which is an ordinary frame allocation; no
fresh storage is made, so the walk should keep climbing rather than stop.

**3b. `nonEscapingAddressWithin`.** `nonEscapingAddress` is parameterised over
the `types.Info`/parent map/body it walks, so the escape walk can ask it about a
body other than the one being lowered. `nonEscapingAddress` is now a call to it
with the lowered function's context; the logic is unchanged.

**3c. `&T{v}` in `valueDoesNotEscapeWithin` stops climbing.** Where the operand
of `&` is a composite literal, the answer is now
`nonEscapingAddressWithin(&literal)` -- the emitter's own placement predicate --
instead of continuing up as though the address were an alias of existing
storage. Ordering matters and is deliberate: 9f76498's `resultLeakBody != nil`
guard is tested **first**, because in a summary walk `nonEscapingAddressWithin`
can answer "does not escape" through a local the callee returns, which is
exactly the `slog.NewTextHandler` hole 9f76498 closed. I had the two the wrong
way round in the first draft and the corpus measurement caught it: six functions
(`log.Logger.output`, `log/slog.Record.Source`, `net/http.relevantCaller`,
`reflect.valueMethodName`, `testing.common.frameSkip`, `testing.pcToName`) each
*lost* a heap allocation, which is the signature of that hole reopening. With
the order corrected, no allocation moves heap-to-frame anywhere in the corpus.

## 4. IR before and after, `runtime_slice_pointer_append_gc.go`

`$main.main`, `goc -emit-ir`. Before (61ba39d):

    loc 10 31
    %t4  =p alloc8 32                   ; make([]*record,0,4) backing array, IN THE FRAME
    %t5  =p call $goc_memset(p %t4, w 0, l 32)
    storel %t4, %t2
    ...
    loc 25 20
    %t75 =p loadl %t2                   ; values.ptr -- may be %t4, a frame address
    %t80 =p call $runtime.newobject(p $.goc.runtime.type.main_record.57cf6c680c1a8218)
    call $goc_storep(p %t80, p %t75)    ; BARRIER: frame address into a heap object

After:

    loc 10 31
    %t4  =p call $runtime.newobject(p $.goc.runtime.type.4__main_record.0e3226d0775457b5)
    storel %t4, %t2
    ...
    loc 25 20
    %t74 =p loadl %t2                   ; values.ptr -- now a heap address
    %t79 =p call $runtime.newobject(p $.goc.runtime.type.main_record.57cf6c680c1a8218)
    call $goc_storep(p %t79, p %t74)    ; heap into heap

The `alloc8 32` is gone: the backing array is allocated by `runtime.newobject`
at line 10, so the pointer the barrier at line 25 publishes into the box is a
heap address. The interface box and the barrier are still emitted -- the fix is
not about removing the store, it is about what the store can carry.

`crypto/internal/fips140/bigmod.Nat.Mul` moves the same way. Before:

    loc 939 24
    %t62  =p alloc8 512                 ; T := make([]uint, 0, preallocLimbs*2), IN THE FRAME
    ...
    loc 951 28
    call $goc_storep(p %t138, p %t140)  ; %t140 may be %t62

After:

    loc 939 24
    %t62  =p call $runtime.newobject(p $.goc.runtime.type.64_uint.1a3f714551d6ea67)
    ...
    loc 951 28
    call $goc_storep(p %t137, p %t139)  ; %t139 is a heap address

## 5. Cost: what moved, and one hot path that regresses

Measured by compiling all 385 corpus programs with each compiler and counting,
per function, frame allocations (`OAlloc*`, `OAllocN`) against allocator calls
(`runtime.newobject`, `newarray`, `makeslice`, `mallocgc`, `makemap`,
`makechan`, and any residual `OHeapAlloc`).

    corpus totals   before: frame 9 735 484   heap 509 897
                    after:  frame 9 735 471   heap 509 920

**22 (program, function) allocation sites move from frame to heap. None moves
the other way.** They are six distinct source sites, in eight distinct
functions:

| source site | functions | corpus programs |
|---|---|---|
| `bigmod/nat.go:939` `T := make([]uint, 0, preallocLimbs*2)` | `bigmod.Nat.Mul` | 10 |
| `x509/verify.go:1059` `[]uint64{2,5,29,32,0}` in `var anyPolicyOID = mustNewOIDFromInts(...)` | 3 `initfunc`s | 8 |
| `runtime_slice_pointer_append_gc.go:10` `make([]*record,0,4)` | `main.main` | 1 |
| `runtime_debug_gc_controls.go:15` `make([]*int,0,128)` | `main.main` | 1 |
| `stdlib_signal_during_gc.go:23` `make([]*int,1024)` | `main.main.func.17.5` | 1 |
| `runtime_range_target_order.go:144,152` two `[]int{...}` literals | `main.targetAliasingTheRangeExpression` | 1 |

Five of the six are the defect sites themselves. The sixth, the x509 one, is a
new-but-correct answer: `mustNewOIDFromInts` does
`panic(fmt.Sprintf("OIDFromInts(%v) unexpected error: %v", ints, err))`, so its
parameter is boxed into a `[]any` on the panic path and the walk is right to
stop. It is a package `init`, run once.

**One hot path regresses: `bigmod.Nat.Mul`'s `default` arm, and therefore
ECDSA.** 200 P-256 sign+verify round trips, `goc -O`, native arm64, eight runs
alternating:

    before  2.74 2.68 2.63 2.65 2.71 2.66 2.69 2.75   mean 2.689 s
    after   2.87 2.86 2.81 2.81 2.87 2.87 2.85 2.81   mean 2.844 s

**+5.8%.** The ranges do not overlap. `Nat.Mul` is the only changed function
this program reaches, so the cause is unambiguous: one 512-byte
`runtime.newobject` per call into the `default` arm, which P-256's four-limb
scalar arithmetic takes. RSA is not affected -- it uses the specialised
1024/1536/2048-bit arms, which allocate `T` the same way before and after.

That cost is real and I am not hiding it, but the store it removes is a
goroutine-stack address in a heap object, so the trade is not close.

### 5.1 Why I did not take the faster fix

There is a fix that would make `Nat.Mul` *faster* than the baseline rather than
slower: `&Nat{limbs: T}` does not actually escape -- `Nat.Mod` only reads its
`x`, and `addressEscapesWithin`, the walk `findEscapingCaptures` already uses
for `&localVar`, says so. Teaching `nonEscapingAddress` that same question would
keep the `Nat` in the frame *and* keep `T` in the frame, removing one heap
allocation rather than adding one.

I did not do it. It changes where `&T{...}` is placed for every composite
literal address in the tree, in the permissive direction -- the direction
2724ac7 and 9f76498 both got wrong -- and validating it means the whole matrix
plus a search for the cases where `parameterDoesNotEscape` is optimistic. That
is a separate change with a separate risk budget, and pairing it with a GC
correctness fix would make both harder to review and harder to revert. The
conservative rule shipped here is consistent by construction: element placement
and literal placement are now decided by the *same* function, so they cannot
disagree again whichever way that function is later made more precise.

## 6. Suite b: `TestFrameEscapeAudit` alone, `-count=1`

    $ go test ./goc -run TestFrameEscapeAudit -count=1 -v
    === RUN   TestFrameEscapeAudit
    --- PASS: TestFrameEscapeAudit (146.70s)
    ok  github.com/evanphx/cg12/goc  147.198s

PASS, at the same 147s it took to fail on 61ba39d, so it compiled the same 385
programs. **No baseline additions.** Three baseline lines were *removed* -- the
test fails on a vanished publication as well as on a new one, and it reported
these three when the fix landed:

    testdata/runtime_range_target_order.go:147:34  main.targetAliasingTheRangeExpression  barrier  ... $runtime.newobject
    testdata/runtime_range_target_order.go:155:34  main.targetAliasingTheRangeExpression  barrier  ... $runtime.newobject
    testdata/stdlib_signal_during_gc.go:29:23      main.main.func.17.5                    barrier  ... $runtime.newobject

Each is the same defect as the three the gate found, and each is genuinely
fixed, not merely renumbered:

* `runtime_range_target_order.go:147` and `:155` are
  `fmt.Sprintf("%v|", numbers)`; column 34 is `numbers`, a `[]int` boxed into
  `Sprintf`'s `...any`. The literal's backing array was on the frame. Hole 3a.
* `stdlib_signal_during_gc.go:29` is `runtime.KeepAlive(values)`; column 23 is
  `values`, `make([]*int, 1024)`. Hole 3a, byte-identical in shape to the two
  findings the gate reported -- the same defect was *already in the baseline*
  before this branch, which is why the baseline being a record and not a
  certificate matters.

The corpus allocation census in section 5 is the independent confirmation: those
three functions are exactly `main.targetAliasingTheRangeExpression` (+2 heap) and
`main.main.func.17.5` (frame 9 to 8, heap 2 to 3). The publications are gone
because the storage moved, not because the code did.

## 7. One more hole of the same class, found while re-reading the diff

`parameterLeaksOnlyToResult` lets a parameter reach the summarised function's
own result and tells the caller to continue its walk from the call expression.
If that result is an **interface**, the value was boxed into fresh heap storage
on the way out, and continuing is wrong for exactly the reason it is wrong at a
call argument:

    func toAny(b []byte) any { return b }   // b's backing array is in the heap here

`boxedIntoInterface` could not see it. The escape walks' parent maps were built
with `astParents(declaration.decl.Body)` -- rooted at the *body* -- so a return
statement had no way to climb to the function it returns from and ask what the
result's type is. The three call sites now root at `declaration.decl`. The
`*ast.FuncDecl` is the only node the maps gain, and every walk that reaches it
falls into the same `default` arm it reached with a nil parent before.

This is not in the three findings and the audit does not currently exercise it;
it is closed because the fix claims to model interface boxing, and a claim with
a known hole in it is worse than no claim. The corpus allocation census is
**byte-identical** to the revision before it -- 385 programs, frame 9 735 471,
heap 509 920, and a per-site diff of zero -- so it costs nothing.
