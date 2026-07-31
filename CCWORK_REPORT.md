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
