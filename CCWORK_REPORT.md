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
