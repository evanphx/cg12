# Front-end determinism: `runtime_defer_capture_allocs.go` — in progress

Branch `ccwork/frontend-determinism`, off `main` (`61b96da`).

Status: **cause 1 found and fixed and verified locally**; sweep for the remaining
causes (§5.10's interface-call-wrapper ordering, `opt/inline.go`) still running.

## Reproduction (before any change)

`goc` built from this tree, `goc/testdata/runtime_defer_capture_allocs.go`,
`CG12_NOCACHE=1`, one compile after another:

- 10 cold compiles → **7 distinct executables** (one hash 4x, six hashes 1x).

The diagnostic that localised it is already in the driver: `goc -emit-ir` prints
the whole module as textual IL. Four emissions gave four distinct files, so the
divergence is in the front end and visible before the back end runs, confirming
§5.10 and §18.

A record-level differ over that text (split into `type`/`data`/`function`
records, compare by name, then by body, then by index) says the module holds
22,493 records in every run, none missing on either side, **zero records at a
different position**, and **two or three functions whose body differs**. Which
functions differ changes run to run; across three pairs they were
`flag.FlagSet.PrintDefaults.func.609.13`, `testing.chattyPrinter.Printf`,
`testing.common.makeTempDir`, `testing.outputWriter.writeLine`,
`testing.prettyPrint` and `testing.common.Attr` — a superset of the four §5.10
names, and every one of them a function that calls a `...any` variadic.

## Cause 1: variadic interface payload addresses were emitted in map order

`goc/compile.go`, `callArgumentsWithVariadic`. When a call passes several
non-direct-interface values to a `...any` parameter and the backing array is
heap-allocated, goc allocates one combined object: the element array plus one
boxed `payloadN` field per argument that needs one. It recorded those in

```go
payloadFields := make(map[int]int)   // argument index -> field index
...
for argumentIndex, fieldIndex := range payloadFields {
        interfacePayloads[argumentIndex] = g.offset(backing, offsets[fieldIndex])
}
```

`g.offset` **emits an `add` instruction**. So the two payload address
computations were emitted in map iteration order, which renumbers every
temporary after them. That is exactly the §5.10 symptom, seen here in
`testing.outputWriter.writeLine`:

```
 	%t29 =p add %t28, 48          	%t29 =p add %t28, 32
 	%t30 =p add %t28, 32     vs   	%t30 =p add %t28, 48
 	...                           	...
 	call $goc_storep(p %t30, …)   	call $goc_storep(p %t29, …)
```

Same instructions, same offsets, swapped order, and every later use follows the
swap.

Fixed by recording the pairs in a slice in argument order — they are appended in
argument order already — and iterating that. No behaviour change is intended or
possible: the same two `add`s with the same operands are emitted, in a fixed
order.

## What that fixes, measured

Same tree, same box, immediately after the change:

| measurement | before | after |
| --- | --- | --- |
| `goc -emit-ir` textual module, 6 emissions | 4 distinct in 4 | **1 distinct in 6** |
| `CG12_NOCACHE=1` linked executable, 8 compiles | 7 distinct in 10 | **1 distinct in 8** |

The single surviving hash is `bbcba376…`, which is the hash that came up 4 times
in the pre-fix run of 10 — the fix collapses the distribution onto its own
majority rather than moving the program to a new layout.

## Not yet done

- Wider sweep: the other four sample programs, the corpus, `-O`.
- §5.10's 441 mis-ordered `*.interfacecall.*` wrappers did not reproduce in any
  `-emit-ir` pair here (zero position differences in all three pairs). Not yet
  explained; being checked against other programs before anything is claimed.
- `opt/inline.go`'s unstable `sort.Slice` over map-built candidates (`-O` only).
- Full suite: unit, corpus, cmd, matrix.

---

## Cause 2 (`-O` only, and not where §5.10 says): `opt/inline.go`'s tie-break

`selectCostInline` collected each dispatch caller's inline candidates by ranging
a map into a slice and then sorted that slice with `sort.Slice` keyed on size
alone. `sort.Slice` is not stable, so two callees of the same size came out in
whatever order the map put them, and that decides which one is inlined when the
per-caller growth budget runs out. Ties now break on name, which is a total
order, so the outcome no longer depends on the slice's input order. The caller
loop walks `m.Funcs` order for the same reason — that one only affects
`CG12_DUMP_COSTINLINE`'s output, since each caller's budget and selection are
independent and marking a callee is idempotent.

**§5.10 is wrong about the blast radius, and this is worth writing down.** It
says "the matrix runs `-O` under `-runtime-opt`, so this matters". It cannot.
`selectCostInline` only looks at callers that contain a computed goto
(`ir.JmpBr`), and `Block.BrIndirect` is reached from `cc/stmt.go` alone — the C
front end. `grep -c 'jmp \*'` over the emitted IR of the largest goc-compiled
program in the corpus is **0**. So this was never able to reach a goc-compiled
program, and it was never part of `runtime_defer_capture_allocs.go`'s residue.
It is live for `cg12cc`, which is why it is fixed rather than deleted.

## Cause 3 (latent): native standard library overlays applied in map order

`applyNativeStdlibOverlays` (`goc/native_overlay.go`) ranged `loader.units` — a
`map[string]*sourceUnit` — and appended each overlay's functions and data to the
module as it went. With two overlay-carrying packages the module's tail, and
every address in it, would come out in map order.

It does not bite today: the only native `.ssa` overlay in the tree is
`stdlib/overlays/linux_arm64/runtime/cg12_overlay.ssa`, so exactly one package
contributes and the walk has nothing to disagree about. Fixed anyway, with
`orderedUnits`, because the next overlay would reintroduce the whole class
silently.

## §5.10's 441 mis-ordered interface-call wrappers did not reproduce

§5.10 records that 441 `*.interfacecall.*` wrappers land at different positions
in the module on each compile. On this tree they do not. The record differ above
compares every record's *index* in the module, so a single moved function or
datum shifts every index after it; three independent pairs of `-emit-ir` output
reported **zero** position differences, before the fix as well as after, while
correctly reporting the two-or-three content differences in the same files.

Then, after the fix, 40 corpus programs drawn at random were each emitted three
times, with and without `-O`: **all 240 emissions identical**, module for module.

I cannot reproduce the wrapper-ordering claim and I am not going to assert it is
fixed. What I can say precisely is that the front-end module text of every
program measured here is now reproducible, and that whatever produced that
observation is not doing so on `main` + these changes. The corpus-wide sweep
below is the check that does not depend on `-emit-ir` being a complete view.

## The runtime pack builds reproducibly

`goc build-runtime -packages ""` under `CG12_NOCACHE=1`, three times:
byte-identical, `77eb840d0bd40404…`. That matters more than one program does —
the pack is the largest module goc compiles and every program built against it
inherits its bytes.

## Against the host toolchain (§3 step 2)

The variadic payload change is a codegen change, so it is checked the way §14
says codegen changes have to be checked, not by a green suite. A program that
exercises the changed path — seven-argument `Println` and `Printf` mixing a
`String()`-bearing struct, a plain struct, an array, a string, an int, a pointer
(direct interface, no payload) and a nil error; the same arguments in three
different orders; a `Println` whose arguments have observable side effects, to
pin evaluation order; and a spread `values...` that takes the `hasEllipsis`
branch instead:

    go run variadic.go   >  host.txt
    goc -o variadic.goc variadic.go && ./variadic.goc  >  goc.txt
    diff host.txt goc.txt   →  identical, exit 0

including `evaluation order: [one two three]`.

`make test-unit`: pass, 0 FAIL.
