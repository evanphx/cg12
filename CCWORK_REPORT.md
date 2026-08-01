# `escape-analysis` × `main`: the heap corruption on `gc-invariants/mark-workers`

Branch: **`ccwork/escape-gc-fix`** = `main` (`ad4e9b2`) + `origin/ccwork/escape-analysis`
merged. Basing on the merge rather than on the branch, because `main` has moved a long way
(it now carries `phase2-gc`, `closure-string`, `reportzombies`) and the gate is that the
*merged* tree is clean. Only the two reports and `RUNTIME_PLAN.md` conflicted;
`goc/compile.go` merged without conflict. The escape branch's plan section was renumbered
24 -> 25, `main`'s 24 being `reportZombies`.

## Status: IN PROGRESS — root cause not yet established

## 1. Reproduced, in six seconds

```
goc build-runtime -o rt0.gocrt
goc -o mw -runtime rt0.gocrt goc/testdata/runtime_gc_mark_workers.go
GOMAXPROCS=3 ./mw
```

**100/100 runs fail** on the merged tree at the default `GOGC`. Compile cost is ~6 s total,
so this is a fast iteration loop, not a capability run.

## 2. The finding that reframes the task: `main` has the same fault, live, today

`main` (`ad4e9b2`) was measured with the identical harness — same program, same split-runtime
configuration, same `GOMAXPROCS=3`, only the compiler differs.

| compiler | `GOGC` | failures |
| --- | ---: | ---: |
| `main` `ad4e9b2` | default | 0/100 |
| `main` `ad4e9b2` | 50 | 0/100 |
| `main` `ad4e9b2` | **20** | **10/100** |
| `main` `ad4e9b2` | **10** | **14/100** |
| merged (`main` + escape) | default | **100/100** |

So `gc-invariants/mark-workers` is **not** a capability that `main` passes and the escape
change breaks. It is a capability `main` passes *only because the matrix runs it at the
default `GOGC`*. Turn the collector up and `main` corrupts its own heap at a 10–14% rate.

`main`'s failures are the same class of fault:

```
runtime: pointer 0x… to unused region of span span.base()=0x… span.state=1
runtime: found in object at *(0x…+0x208)
object=0x… s.elemsize=8192 s.state=mSpanManual
```

`s.state=mSpanManual`, `elemsize=8192` — the referring word is **inside a goroutine stack**,
i.e. the precise stack scan is retaining a word that no longer points at anything. 13 of
`main`'s 14 GOGC=10 failures have exactly this shape; the 14th is a SIGSEGV.

On the merged tree the dominant route is different — the word is rejected by
`wbBufFlush1` on a barrier buffered inside `main.buildGraph` — but 4 of 100 take `main`'s
stack-scan route too.

Whether these are one defect that the escape change amplifies from 14% to 100%, or two, is
**not yet established** and is the next thing to settle. It matters: if it is one, then
narrowing the escape summary would only push the rate back down to `main`'s and would be a
symptom fix, which this project's rules forbid.

## 3. Not yet established

- The mechanism. Nothing below is a conclusion yet.
- Whether `main`'s 14% and the merged tree's 100% are the same defect.

## 4. The bisect said `appendDestination`, and the bisect was wrong

A temporary `GOC_ESCAPE_DISABLE=<rule>` knob was added to `goc/compile.go` to turn each of
`2724ac7`'s rules off one at a time. At the default `GOGC`, disabling `appenddest` alone
took the reproducer from 10/10 failing to 0/10.

It is an artifact, and it is worth recording because it would have been an easy wrong answer:

- The two binaries have **byte-identical code**. A symbol-resolving disassembly diff over
  every function in both images reports four differences, all of them a `.data` displacement
  in `runtime_load_g`, `runtime_save_g`, `runtime_memclrNoHeapPointers` and
  `__do_global_dtors_aux`.
- The whole difference is **168 bytes of dead `.data`**: the passing image carries three
  extra type descriptors (`[16]*main.vertex` and two `[44]byte`) that nothing references —
  verified by scanning every 8-byte word of every PT_LOAD segment for their addresses; the
  only references are each descriptor's own `gcdata`.
- Re-measured at `GOGC=10`, the "fix" evaporates: `appenddest`-disabled fails **16/50**, and
  all-seven-rules-disabled fails **7/50**.

`goc build-runtime` caches its pack keyed on the compiler binary and the standard library,
not on the environment, so every bisect arm linked the *same* runtime pack — the knob only
varied the program's own module. That does not change the conclusion, it sharpens it: the
program module's code was identical and the outcome still flipped 100% -> 0%.

## 5. Therefore: one defect, on `main`, that the escape change exposes far more often

Every arm fails at `GOGC=10`, including `main` itself (3/50 in this batch, 14/100 in the
earlier one). Nothing in `2724ac7` is the cause; what it does is raise the exposure of a
pre-existing defect from a few percent to certainty. Chasing the escape rules further would
have been chasing a rate, which §15 of the plan warns about in exactly these words.

The hunt is now for the defect itself.

## 6. A reducer that fails 30/30 on plain `main`

`$TMPDIR/red1.go` — `runtime_gc_mark_workers.go` with the metrics, the `sync.WaitGroup` and
the sum-check removed, keeping only `buildGraph` + `churn` + eight goroutines + six
`runtime.GC()`s. It is committed as `goc/testdata/runtime_gc_graph_churn.go`.

| compiler | configuration | failures |
| --- | --- | ---: |
| `main` `ad4e9b2` | prebuilt runtime pack | **30/30** |
| `main` `ad4e9b2` | runtime compiled inline | **20/20** |
| merged | prebuilt runtime pack | 20/20 |

At the **default `GOGC`**, on `main`, both compile paths. The escape change is not involved.

The fault is always the same word:

```
runtime_badPointer <- runtime_findObject <- runtime_wbBufFlush1
goroutine N: bulkBarrierPreWriteSrcOnly <- runtime_growslice <- main_buildGraph
```

`cg12checkwb` was extended (`stdlib/src/runtime/mbitmap.go`) to validate the words the four
*bulk* barrier paths buffer, not just `atomicwb`'s. That is what named the writer:
`atomicwb`'s existing check never fired, because the bad word does not enter the buffer
through a pointer store at all — it enters through `growslice`'s
`bulkBarrierPreWriteSrcOnly`, which reads the **old backing array** it is about to copy. So
the bad pointer is *already in a live `[]*vertex` backing array* before any barrier runs.

## 7. ROOT CAUSE: goc's type GC masks are not padded to a `uintptr`, and the runtime reads them a `uintptr` at a time

`goc` emits each `abi.Type`'s pointer bitmap as exactly `ceil(sizeInWords/8)` bytes
(`goc/compile.go`, `ensureTypeTag` and `runtimeTypeSymbol`, both `Align: 1`). For a type with
one pointer word — `*main.vertex` — that is **one byte**.

The Go runtime never reads that bitmap a byte at a time. `mbitmap.go`:

```go
func (span *mspan) typePointersOfType(typ *abi.Type, addr uintptr) typePointers {
	gcmask := getGCMask(typ)
	return typePointers{elem: addr, addr: addr, mask: readUintptr(gcmask), typ: typ}
}
```

`readUintptr` loads **eight bytes**. The seven bytes past a one-byte mask are whatever symbol
the linker put next, and every 1 bit in them is a phantom pointer word.

Measured in the failing image:

```
_goc_runtime_type_main_vertex_86131734c57ebf15_gcdata  size=1
  readUintptr -> 0x0800000000000001   bits 0 and 59  ->  word offsets 0 and 472
```

Bit 59 is the low byte of the *next* symbol, the type descriptor itself, whose first field is
`Size_`. And 472 = `0x1d8` is exactly the displacement the diagnostic reported, in every run:

```
run 1: dst=0x1ebea22e1880 size=64  slot=0x1ebea22e1a58   slot-dst = 0x1d8
run 2: dst=0x91956f345e0  size=16  slot=0x91956f347b8    slot-dst = 0x1d8
run 3: dst=0x12e81f270120 size=8   slot=0x12e81f2702f8   slot-dst = 0x1d8
```

`typePointers.next` takes the fast path while `mask != 0` and `nextFast` does **not** consult
`limit`, so `growslice`'s `bulkBarrierPreWriteSrcOnly(newArray, oldArray, 8, *vertex)` faithfully
reads word 59 of the old array — 464 bytes past its end — and buffers whatever is there as a
pointer. The collector then either throws `found bad pointer in Go heap`, or, worse, *marks*
that word's target and drags a dead object back into the live set.

**The host toolchain states the requirement in its own words.** `cmd/compile/internal/reflectdata/reflect.go:1261`:

```go
func dgcptrmask(t *types.Type, write bool) *obj.LSym {
	// Bytes we need for the ptrmask.
	n := (types.PtrDataSize(t)/int64(types.PtrSize) + 7) / 8
	// Runtime wants ptrmasks padded to a multiple of uintptr in size.
	n = (n + int64(types.PtrSize) - 1) &^ (int64(types.PtrSize) - 1)
```

goc omits that line. This is a cg12 defect, not a runtime defect, which is the pattern §15
of the plan records for every Phase 1 failure.

### Why it explains everything that looked inconsistent

- **Why it is layout-sensitive.** Whether a mask is followed by nonzero bytes, and which bit
  positions those set, is decided by symbol placement. That is why adding 168 bytes of *dead*
  `.data` flipped the reducer from 100% failing to 0% (§4), and why `appendDestination`
  looked like the culprit when it changes no code at all.
- **Why the escape change amplified it.** It changes which types get emitted and therefore
  which bytes follow each mask. It never was the cause.
- **Why `"node-" + string(rune(x))` is needed and a constant label is not.** The concat is
  what makes `vertex` big enough and the churn heavy enough that `next` grows through
  `growslice` while a mark phase is running, which is the one path that calls
  `bulkBarrierPreWriteSrcOnly` with a small `size` and a short mask.
- **Why `main` fails only at low `GOGC`.** The phantom bit is read on every bulk barrier, but
  it only produces a *rejected* word when the phantom target happens to be a stale pointer
  and a mark phase is running.
