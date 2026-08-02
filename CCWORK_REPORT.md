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

## 8. Two residuals this change deliberately does not close

Stated so nobody reads section 3 as "interface publication is now fully
modelled". Neither is one of the three findings, and neither produces a finding
on today's corpus.

**8a. A pointer-shaped value in a heap variadic backing array.**
`isDirectInterfaceType` values -- pointers, maps, channels, funcs -- are stored
straight into the two-word interface descriptor, which is a frame allocation, so
`boxedIntoInterface` correctly reports no box. But `buildVariadicSlice`
allocates the `[]any` backing array with `allocateTyped` unless the caller is
`NoSplit` or non-allocating, and copies the descriptor's two words into it. So
`f(&local)` for a variadic `f(...any)` can put a frame address in the heap
without any boxing having happened. What stops it today is the callee summary:
`fmt.Println`'s parameter escapes, so the walk answers "escapes" for the whole
argument. A variadic callee that provably retains nothing would not be stopped.
Closing this properly means modelling the variadic backing array as a
publication, which would make every `&local` passed to any non-retaining
variadic function escape -- a much larger cost than this change, and a separate
decision.

**8b. `nonEscapingAddress` remains cruder than the walk.** Section 5.1. The two
now agree by construction, which is what the correctness argument needs; they
agree at the conservative end.

## 9. Reduced tests, and the proof they discriminate

`goc/escape_publication_test.go`, three tests, one per shape. A regression test
that passes on the broken tree is decoration, so each was run against
`61ba39d`'s `goc/compile.go` with the rest of the tree unchanged:

| test | on 61ba39d | on the fix |
|---|---|---|
| `TestValueBoxedIntoAnInterfaceEscapes` | FAIL — `interface_box.go:17:10: main.boxed: barrier %t3 into memory reached through a call result $runtime.newobject` | PASS |
| `TestCompositeLiteralAddressCarriesItsElementsToTheHeap` | FAIL — `literal_address.go:19:27: main.passedToACall: barrier %t3 into ... $runtime.newobject` | PASS |
| `TestParameterLeakingToAnInterfaceResultEscapes` | FAIL | PASS |

The third also fails on `6245dbb`, the revision *after* the main fix and
*before* section 7's hardening, which is the regression it exists to guard.

Each test carries a control that a fix which simply made every slice escape
would break:

* a slice never converted to an interface keeps its frame backing array and
  calls no allocator;
* a literal address `nonEscapingAddress` keeps in the frame -- `(&box{limbs:
  limbs}).limbs` -- does not drag its element to the heap;
* leaking only to a *slice*-typed result copies no storage, so the backing array
  stays in the frame.

The third test asserts on where the allocation landed rather than on
`opt.FrameEscapes`, and the reason is worth recording: the publishing store is
inside the callee, where the address arrives as a *parameter* rather than as one
of that function's own frame allocations. `FrameEscapes` is a per-function
may-analysis over `OAlloc`-derived values, so it structurally cannot report that
shape. The corpus audit passing was never evidence about it.

# Independent verification run — `ccwork/escape-frame-publication` @ `ddd03eb`

Run in a detached worktree checked out at `ddd03eb` (`git worktree add ... ddd03eb`),
Linux/arm64, go1.26.1, 64 cores. Every suite below was launched, then blocked on in
the foreground until the process wrote its own exit code to the log. No number in
this section was inferred, extrapolated, or read from a still-running process.

Note on the box: a second ccwork job (`escape-alloc-census`) was resident on the
same machine for part of this run and itself compiles the corpus. Load average at
the start of suite (a) was 0.57 on 64 cores, so contention was low, but wall-clock
figures below are shared-machine figures and are not benchmark-grade.

## Suite (a) — `go test -timeout 40m -parallel 10 -v ./goc/...`

**PASS.** Exit code 0. `ok github.com/evanphx/cg12/goc 768.405s` (~12.8 min).
`-v` was added to the prescribed command line so the per-subtest census could be
taken; it changes what is reported, not what is run.

| | count |
| --- | --- |
| `=== RUN` | **601** |
| `--- PASS:` | **601** |
| `--- FAIL:` | **0** |
| `--- SKIP:` | **0** |
| packages `ok` / `FAIL` | 1 / 0 |

`./goc/...` resolves to the single package `github.com/evanphx/cg12/goc`, so this
is the whole suite, not a subset.

**The census reconciles with the stated 598 RUN / 597 PASS / 1 FAIL baseline
exactly, and the reconciliation was checked rather than assumed.** Comparing the
set of top-level `func Test*` names in `goc/*_test.go` at `eb9872e~1` against
`eb9872e` ("goc: reduced tests for the three publication shapes") shows that
commit adds exactly three, and no others anywhere on the branch add any:

- `TestValueBoxedIntoAnInterfaceEscapes` — PASS (1.68s)
- `TestCompositeLiteralAddressCarriesItsElementsToTheHeap` — PASS (1.64s)
- `TestParameterLeakingToAnInterfaceResultEscapes` — PASS
  (its sibling `TestParameterAssignedToACalleeNamedResultEscapes`, PASS 2.00s,
  predates `eb9872e`)

598 + 3 = 601, and the one prior failure is gone: `--- PASS: TestFrameEscapeAudit
(147.40s)`. So the suite did not get smaller and did not stop running anything —
it grew by precisely the three tests the branch added.

Against the merge-base `ad4e9b2` the branch adds nine top-level tests in total
(294 → 303 test functions) and removes none; the 598 baseline was evidently taken
mid-branch, after `TestFrameEscapeAudit` landed and before `eb9872e`.

Slowest subtests, for anyone judging whether this is timing-sensitive:
`TestFrameEscapeAudit` 147.40s, then `TestCapturedLoopVariableIsAllocatedInsideTheLoop`
11.63s and `TestUncapturedLoopVariableAllocatesNothing` 11.57s; everything else is
under 8s. Nothing here asserts on elapsed time, so the +5.8% `bigmod.Nat.Mul`
regression cannot have flipped a result in this suite — it can only have made the
768s longer.

## Suite (c1) — `make test-goc-status`

**PASS, 364/364.** Exit code 0. `ok github.com/evanphx/cg12/cmd/goc 107.793s`.
Run with `GOFLAGS=-v` so the per-capability set is visible; the make target's own
command line is unmodified (`go test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$'
./cmd/goc/... -args -runtime-status-shards=1 -runtime-status-shard=0`, one shard,
the whole matrix).

- `=== RUN` 365 = 1 parent + **364 capability subtests**
- `--- PASS:` 365, `--- FAIL:` 0, `--- SKIP:` 0
- 364 distinct capability names, 364 distinct in the PASS set — the empty FAIL set

**FAIL SET: empty.** This matches the known good state exactly, on count and on
membership. In particular `stack-scan/loop-safepoints` PASSes here (0.07s) — the
one known failure is an `-opt`-only failure, as documented.

On the 107s duration: this is not a truncated run. The subtests are individually
cheap (median well under 0.1s; the slowest are `stack-scan/stack-copy-roots` 6.21s,
`gc-stress/concurrent-mark` 4.23s, `gc-stress/memory-limit` 1.20s) and the matrix
runs against one shared prebuilt pack, so the wall clock is dominated by that build
rather than by 364 independent compiles. The count, not the clock, is the evidence:
all 364 named capabilities appear.

## Suite (c2) — `make test-goc-status-opt`

**FAIL, 363/364 — and the failure set is exactly the one known pre-existing entry.
It has not grown.** Exit code 2. `FAIL github.com/evanphx/cg12/cmd/goc 117.647s`.

- `=== RUN` 365 = 1 parent + **364 capability subtests**
- `--- PASS:` 363, `--- FAIL:` **1**, `--- SKIP:` 0

**FAIL SET (complete, one member):**

- `stack-scan/loop-safepoints`

The set of 364 capability names exercised under `-opt` is **byte-identical** to
the set exercised without `-opt` (`diff` of the two sorted name lists is empty),
so the `-opt` arm is not running a reduced matrix — same 364 capabilities, 363 of
them passing.

The failure text, for the record:

```
runtime_status_test.go:2664: runtime_stack_scan_loop_safepoints.go should pass: exit status 2
    ...
    collected while live: carried-0 at carried before rewrite
    panic: a stack slot live across a loop back edge was not a GC root
```

This is a stack-map/liveness hole under `-opt`: a slot live across a loop back
edge is missing from the GC root set, so the object is collected while still
reachable. It is a different mechanism from what this branch changes — the branch
moves allocations from frame to heap and does not touch stack-map emission or
`findEscapingCaptures` — and it is reported as reproducing on the merge-base
`ad4e9b2`. I did not independently re-run `ad4e9b2` to confirm that reproduction;
see the note at the end of this section for what I did check.

This capability is **not** timing-sensitive in a way the +5.8% `bigmod.Nat.Mul`
regression could touch: it fails deterministically in 0.02s with an assertion
about root-set membership, not a deadline.

## Suite (d) — `runtime_gc_type_mask_padding.go` reducer, 20× at `GOGC=10`

**PASS in the prescribed configuration: 0/20 failed.** Compiled with this tree's
`goc` (`go build -o goc ./cmd/goc`, then `goc -o reducer goc/testdata/runtime_gc_type_mask_padding.go`,
build exit 0, 2.9s). All twenty runs printed `type mask padding ok` and exited 0:

    === RESULT: 0/20 failed, 20/20 passed ===

That is the gate item, and it holds.

### An unprescribed variant does fail — reported, not swept up

The brief specified `GOGC=10`. The capability harness that owns this program
(`cmd/goc/runtime_status_test.go`, `gc-invariants/type-mask-padding`) runs it with
`env: []string{"GOMAXPROCS=3"}` at default `GOGC`. Because both are the program's
real configurations, I also ran **`GOGC=10 GOMAXPROCS=3`, 20×: 5/20 failed.**

The failure is not the type-mask assertion. It is a GC scan abort:

    runtime: pointer 0x394b6937420c to unused region of span
        span.base()=0x394b6928e000 span.limit=0x394b6928fef0 span.state=1
    runtime: found in object at *(0x394b69131c80+0x208)
    object=... s.spanclass=0 s.elemsize=8192 s.state=mSpanManual

`mSpanManual` with `elemsize=8192` is a goroutine stack, so this is the collector
finding a word inside a stack that points into an unallocated part of a span. Note
the bad pointer `0x...420c` is not 8-byte aligned — it is an interior/misaligned
value being scanned as a pointer.

**This is not the branch's regression.** See the merge-base comparison recorded
below; it reproduces identically on `ad4e9b2`.

### Merge-base comparison for the reducer

| build | `GOGC=10` (prescribed) | `GOGC=10 GOMAXPROCS=3` |
| --- | --- | --- |
| tip `ddd03eb` | **0/20 failed** | 5/20, then 4/20 failed |
| merge-base `ad4e9b2` | **20/20 failed** | 20/20 failed |

The reducer source is *added* by this branch (99 lines, absent at `ad4e9b2`), so
the merge-base column is this branch's reducer compiled by the merge-base `goc` —
the correct "before". It confirms the branch's claimed before/after: 20/20 → 0/20.

The merge-base column also shows why it cannot, by itself, tell us whether the
`GOMAXPROCS=3` failure is new: at `ad4e9b2` the original defect fires on every
run and masks everything behind it.

So I discriminated with a different program instead. Compiled by both compilers and
run 10× each at `GOGC=10 GOMAXPROCS=3`:

| program | tip `ddd03eb` | merge-base `ad4e9b2` |
| --- | --- | --- |
| `runtime_gc_concurrent_mark` | 0/10 | 0/10 |
| `runtime_gc_checkmark` | 0/10 | 0/10 |
| `runtime_gc_mark_workers` | **2/10 failed** | **4/10 failed** |

`runtime_gc_mark_workers` passes in the standard harness on both, and crashes under
`GOGC=10 GOMAXPROCS=3` on **both** — on the merge-base more often than on the tip
(there with a `SIGSEGV`, `addr=0x662d65646f82`, i.e. ASCII garbage being followed as
a pointer). **`GOGC=10` combined with `GOMAXPROCS=3` is therefore a configuration in
which this runtime's GC is already unsound on `main`, independent of this branch.**
The reducer's 4–5/20 there is that pre-existing instability, not a regression this
branch introduces, and the prescribed gate (`GOGC=10`, 20 runs) is clean at 0/20.

I did not root-cause the underlying GC/stack-scan instability; that is out of scope
for a verification job and it is not on this branch's ledger. It is worth a bug of
its own.

## Suite (e) — `make test-unit`

**PASS. 1556 passing, 0 failing — exactly the stated baseline.** Exit code 0.

| | count |
| --- | --- |
| `--- PASS:` | **1556** |
| `--- FAIL:` | **0** |
| `--- SKIP:` | 339 |
| packages `ok` | 24 |
| packages with no test files | 12 |
| packages in `UNIT_PKGS` | **36** (= 24 + 12, fully accounted for) |

The wall clock is ~10s, which is short enough to deserve the lie-detector test, so:
**`(cached)` appears on 0 of the 36 packages** — every test binary was built and run
in this invocation, nothing was served from the Go test cache. The suite is fast
because these are pure unit tests (slowest package `arm64` at 7.45s, next `link` at
2.01s) running 36-way in parallel on 64 cores, with the module's build cache already
warm from suites (a) and (c). 1556 + 339 = 1895 subtests actually reported.

The 339 skips are the pre-existing host-architecture skips (amd64 encode/exec tests
on an arm64 box, e.g. `TestAtomicExec*`, `TestObjClz*`), not new suppression: the
passing count matches the 1556 baseline exactly, so nothing moved from PASS to SKIP.

## Merge-base control run — `make test-goc-status-opt` on `ad4e9b2`

Because suite (c2) is the only red result, I ran the same target on the merge-base
rather than inheriting the claim that its one failure is pre-existing.

**`FAIL github.com/evanphx/cg12/cmd/goc 119.445s`, exit 2. 362 PASS / 1 FAIL.**

**Merge-base FAIL SET: `stack-scan/loop-safepoints` — identical, one member.**

The capability matrix delta between the two revisions is exactly one addition and
zero removals:

- only on tip `ddd03eb`: `gc-invariants/type-mask-padding` (the capability this
  branch adds, and which passes in both the `-opt` and non-`-opt` arms)
- only on merge-base: *nothing*

So 363 capabilities at `ad4e9b2` → 364 at `ddd03eb`; the failure set is the same
single pre-existing entry on both. **It has not grown, and it is measured here,
not assumed.**

## Verdict

| suite | result | evidence |
| --- | --- | --- |
| (a) `go test -timeout 40m -parallel 10 ./goc/...` | **PASS** | 601 RUN / 601 PASS / 0 FAIL / 0 SKIP, exit 0, 768.4s; = 598 baseline + the 3 tests `eb9872e` adds |
| (c1) `make test-goc-status` | **PASS** | 364/364, FAIL set empty, exit 0, 107.8s |
| (c2) `make test-goc-status-opt` | **FAIL, unchanged** | 363/364; FAIL set = {`stack-scan/loop-safepoints`}; byte-identical FAIL set on merge-base `ad4e9b2` |
| (d) reducer 20× at `GOGC=10` | **PASS** | 0/20 failed (merge-base: 20/20 failed) |
| (e) `make test-unit` | **PASS** | 1556 PASS / 0 FAIL / 339 SKIP, 0 cached, 36/36 packages, exit 0 |

All five runs were watched to completion in the foreground; every number above was
read out of a log whose process had already written its own exit code. Nothing was
left running and nothing is extrapolated.

Two things a merger should carry forward, neither of them blocking:

1. **The +5.8% `bigmod.Nat.Mul` regression is not covered by any suite here.** None
   of these tests assert on elapsed time — (c) and (d)'s failures are deterministic
   assertions about GC root sets and type masks, not deadlines — so a clean run is
   evidence about correctness only. The regression stands or falls on its own merits.
2. **`GOGC=10` together with `GOMAXPROCS=3` crashes the GC on `main` already**
   (`runtime_gc_mark_workers`: 4/10 on `ad4e9b2`, 2/10 on `ddd03eb`). Pre-existing,
   out of this branch's scope, and worth its own bug.

SAFE TO MERGE TO MAIN
