# wave8 differential refresh: regenerate the two gc-differential reference files

(The previous contents of this file — the wave8 merge gate's verification of
`integration/wave8` — are at `git show 73aee03:CCWORK_REPORT.md`.)

Branch: `ccwork/wave8-differential-refresh`, off `ccwork/wave8-gate` (73aee03).
Host toolchain: `go version go1.26.1 linux/arm64` — the same release both files
already recorded, so nothing in these diffs is a toolchain-wording change.

**No compiler code was touched.** The only files changed are the two reference
files and this report.

---

## The verdict

| question | answer |
|---|---|
| lines where goc heaps and gc does not (`heap`/`frame` cell), before | **96** |
| same, after | **97** |
| what moved it | one line, `runtime_gc_slice_tail_pointer.go:76`, in the new program |
| did every added line come from the one new program? | **yes** — verified by set difference, not by eye |
| is the reason file byte-identical when regenerated at a second path? | **yes** — same sha256 from a path twice as long |
| compiler behaviour change? | **none** — census reproduces its committed baseline, all guards pass |

---

## 1. What was stale, and why

`ccwork/flate-gc-crash` (merged at 887206b) added one corpus program,
`goc/testdata/runtime_gc_slice_tail_pointer.go`, and added the six census rows
it produces to `goc/testdata/alloc_census_baseline.txt`. It did not regenerate
the two files that join that census against the host toolchain's `-m` output,
so both drifted by exactly that program.

The six census rows the merge added:

```
runtime_gc_slice_tail_pointer.go:76:25   main.checkPointers    newobject  32768_byte  heap
runtime_gc_slice_tail_pointer.go:98:2    main.checkPointers    newobject  64_byte     frame
runtime_gc_slice_tail_pointer.go:98:2    main.checkPointers    newobject  64_byte     heap
runtime_gc_slice_tail_pointer.go:115:30  main.checkPointers    newobject  64_byte     frame
runtime_gc_slice_tail_pointer.go:145:26  main.checkCollector   newobject  32768_byte  heap
runtime_gc_slice_tail_pointer.go:152:30  main.checkCollector   newobject  512_byte    heap
```

---

## 2. `escape_gc_differential.txt` — regenerated, reviewed

Regenerated with the command in its own header:

```
go test ./goc -run TestEscapeDifferentialAgainstGC \
    -escape-gc-differential -update-escape-gc-differential
```

Coverage moved by exactly one program:

| | before | after |
|---|---|---|
| corpus programs | 403 | 404 |
| compared (host built them) | 399 | 400 |
| not compared | 4 | 4 |
| census rows joined | 1861 | 1867 |
| gc decisions joined | 3511 | 3529 |

Confusion matrix, before → after:

```
  goc\gc      frame     heap    mixed   absent    total
  frame         189→190    30       14      193      426→427
  heap           96→97    580→582  172       81      929→932
  mixed          13        89       24       13→14   139→140
  absent        420→422  1286→1296  24        0     1730→1742
  total         718→722  1985→1997 234      287→288 3224→3241
```

+17 joined lines in total, and all seventeen are accounted for by the new
program alone:

| cell | Δ | which lines |
|---|---|---|
| `absent`/`heap` | +10 | the 10 new PERMISSIVE entries (gc heaps panic strings and one `append`; goc's census says nothing) |
| `absent`/`frame` | +2 | gc frames them, goc's census says nothing — agreement, so listed in neither section |
| `heap`/`heap` | +2 | census lines 145 and 152 — both compilers heap them |
| `frame`/`frame` | +1 | census line 115 — both compilers frame it |
| `heap`/`frame` | +1 | **census line 76** — the one that moves the headline count 96 → 97 |
| `mixed`/`absent` | +1 | census line 98, which has both a frame and a heap row |

### 2a. Proof that no other program moved

Not an eyeball check. I stripped every entry belonging to
`runtime_gc_slice_tail_pointer.go` (the `file.go:line` header and its indented
`src`/`goc`/`gc` continuation lines) from the committed file and from the
regenerated one, then diffed the remainders. The **entire** residual diff is
aggregate counters — coverage totals, the matrix, the two section headings, and
two `by construct` histogram tallies:

```
-corpus programs                             403     +corpus programs           404
-compared (host toolchain built them)        399     +compared ...              400
-census rows joined                         1861     +census rows joined       1867
-gc decisions joined                        3511     +gc decisions joined      3529
-  frame         189 ...                             +  frame         190 ...
-  heap           96 ...                             +  heap           97 ...
-  mixed          13 ...                             +  mixed          13 ...
-  absent        420 ...                             +  absent        422 ...
-  total         718 ...                             +  total         722 ...
-## PERMISSIVE: 1467 lines                           +## PERMISSIVE: 1477 lines
-   1283  - | object/heap                            +   1293  - | object/heap
-## PESSIMISTIC: 399 lines                           +## PESSIMISTIC: 401 lines
-     25  newobject/heap | slice/frame               +     26  newobject/heap | slice/frame
-      7  newobject/frame,newobject/heap | -         +      8  newobject/frame,newobject/heap | -
```

Zero per-line entries for any other corpus program were added, removed, or
altered. Every histogram delta is traceable to one of the two new PESSIMISTIC
entries (`26 newobject/heap | slice/frame` is line 76; `8 newobject/frame,
newobject/heap | -` is line 98).

### 2b. The two new PESSIMISTIC entries, in full

```
runtime_gc_slice_tail_pointer.go:76	heap -> frame
	src  buffer := make([]byte, size)
	goc  col 25  heap  runtime.newobject  32768_byte  [main.checkPointers]
	gc   col 16  frame  slice  make([]byte, 32768)

runtime_gc_slice_tail_pointer.go:98	mixed -> absent
	src  var fixed [64]byte
	goc  col 2  frame  runtime.newobject  64_byte  [main.checkPointers]
	goc  col 2  heap  runtime.newobject  64_byte  [main.checkPointers]
```

Line 76 is a 32 KiB `make([]byte, size)` that gc keeps in a frame and goc heaps.
It is the sole contributor to the 96 → 97 move, and it is a pre-existing goc
behaviour newly *observed* by a new program, not a behaviour change: the census
row for it arrived with the corpus program in 887206b and reproduces from the
committed baseline unchanged (§4).

---

### 2c. It really was failing before

Not assumed. In a clean second clone still holding the committed file,
`go test ./goc -run TestEscapeDifferentialAgainstGC -escape-gc-differential`
fails with the 403/399/1861/3511 coverage block against the same 404/400/1867/
3529 run. After the refresh, in this checkout, it passes.

---

## 3. `escape_gc_reason_differential.txt` — regenerated, reviewed

```
go test ./goc -run TestEscapeReasonDifferentialAgainstGC -timeout 60m \
    -escape-gc-reason-differential -update-escape-gc-reason-differential
```

Diffstat: 85 insertions, 22 deletions. Grouping every added or removed per-line
entry by the program it belongs to gives exactly one program:

```
13 +  runtime_gc_slice_tail_pointer.go
```

Thirteen added, none removed, and nothing from any other program.

The same strip-and-diff proof as §2a — remove every
`runtime_gc_slice_tail_pointer.go` entry from both files and diff the
remainders — leaves aggregate counters and two content lines:

```
+  NOT IN -m: runtime_gc_slice_tail_pointer.go:149:20: append(retained, tail(buffer, len(buffer)))
+  NOT IN -m: runtime_gc_slice_tail_pointer.go:152:20: append(scenery, new([512]byte))
```

Both name the new program, and both fall in a category the file already
documents at length: gc allocations that `-m=2` explains and `-m` never
mentions, of which "the backing array an escaping `append` reallocates" is one
of the two named shapes. There were 112 such lines; there are now 114.

Counters that moved, all by the new program's contribution:

| | before | after |
|---|---|---|
| programs with reasons on both sides | 399 | 400 |
| goc `-m` decisions at joinable positions | 1797 | 1803 |
| of those on the heap / carrying a rule | 1103 / 1103 | 1107 / 1107 |
| gc `-m=2` explained blocks parsed | 3014 | 3027 |
| lines both record, and agree about | 1341 | 1346 |
| AGREE ON PLACEMENT, DISAGREE ON REASON | 309 | 311 |
| PLACEMENT DISAGREES, ONLY goc EXPLAINED | 159 | 161 |
| PLACEMENT DISAGREES, ONLY gc EXPLAINED | 1327 | 1336 |
| UNCATEGORISED | 0 | 0 |

**No new `NO RULE` rows.** That list is the file's own drift detector — a line
where the committed census says heap and goc's freshly compiled `-m` gives no
heap rule. Its membership is unchanged, which is independent evidence that the
tree has not moved under the census.

---

## 4. Guards — all pass, no compiler behaviour change

| check | result |
|---|---|
| `TestAllocationCensus` (reproduces the committed baseline) | **PASS**, 226.54 s |
| `TestFrameEscapeAudit` | **PASS**, 193.92 s |
| `TestLoopAliasAudit` | **PASS** |
| `TestCompilingTheSameSourceTwiceGivesTheSameModule` (`goc`) | **PASS** |
| `TestBinaryDeterministic` (`ir`) | **PASS** |
| `TestBuildIDIsDeterministic` (`link`) | **PASS** |
| `TestImagesCarryABuildID` (`link`, 3 subtests) | **PASS** |
| `TestParallelBackendIsByteIdenticalToSerial` (`arm64`, 4 worker counts) | **PASS** |
| `TestRuntimeCorpusCoverageRecordsConcurrentOutcomesDeterministically` (`cmd/goc`) | **PASS** |

`TestAllocationCensus` passing without `-update` is the load-bearing one: goc's
side of both differential files is read out of
`alloc_census_baseline.txt`, and that file reproduces byte-for-byte from a fresh
compile of the whole corpus. Nothing about where goc puts an allocation changed;
only the join against the host toolchain was re-taken.

Per instruction, `go test ./goc/...`, `make test-unit` and the timing benchmarks
were **not** run — the benchmarks were re-cut on an idle box and re-running them
under this job's load would only add noise.

---

## 5. Portability of the reason file — byte-identical across two paths

The file's own doc comment calls the two-checkout regeneration "the expensive
half" of the guarantee, and names the cheap half
(`TestReasonPositionsAreRepositoryRelative`) as the in-suite stand-in. Both were
run.

A second clone of this commit was made at a path **twice as long** as the
original:

```
/home/evan/.ccwork/ws/wave8-differential-refresh/repo                     53 chars
$TMPDIR/checkout2-portability-a-longer-path-than-the-original            106 chars
```

and the reason differential regenerated there from scratch, independently.

```
A  765081 bytes  sha256 8849e1b7c74a060ac9e6710d6588b559fce89da5ae008f1a81ff94ac24dda8ca
B  765081 bytes  sha256 8849e1b7c74a060ac9e6710d6588b559fce89da5ae008f1a81ff94ac24dda8ca
cmp: identical
```

Byte-identical. `grep -c /home/evan` is 0 in both, so no absolute path has crept
back in — the `gcdiff.relativeToRepository` fix still holds, and this file does
not record the directory that made it.

---

## 6. Final verification

Both tests re-run **without** their `-update` flags against the committed files:

| test | result |
|---|---|
| `TestEscapeDifferentialAgainstGC` | **PASS** |
| `TestEscapeReasonDifferentialAgainstGC` | **PASS** |
| `TestReasonPositionsAreRepositoryRelative` | **PASS** |

---

## Commits

```
1f0b03e  goc: refresh escape_gc_differential.txt for the corpus program wave8 brought in
a2669f9  goc: refresh escape_gc_reason_differential.txt for the same corpus program
```

