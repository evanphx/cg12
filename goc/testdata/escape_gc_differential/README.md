# Discriminating programs for the goc-vs-gc escape differential

These are reductions written while triaging
`goc/testdata/escape_gc_differential.txt`. Each one isolates one class the
differential flags — a source line `cmd/compile` puts on the heap and goc does
not — and is written so that **it behaves differently if goc is wrong**: a frame
address that outlives its frame shows up as a wrong number or a wrong string,
not as a slower program.

They are deliberately **not** in `goc/testdata/`, so `filepath.Glob("testdata/*.go")`
does not pick them up and they do not change the corpus, the allocation census
or any baseline. Run one with

    go run ./cmd/goc -run goc/testdata/escape_gc_differential/<name>.go

and compare against

    go run goc/testdata/escape_gc_differential/<name>.go

Both compilers' current answers are recorded below, against
**go1.26.1 linux/arm64** and goc at `main` `efcd4d4`. A future run that disagrees
with the recorded answer is the finding.

To see where each compiler puts the allocations rather than what the program
prints:

    go test ./goc -run TestEscapeDifferentialProgram \
        -escape-gc-differential-program=goc/testdata/escape_gc_differential/<name>.go -v

## loopaddr.go — the loop-variable calibration case

`&index` of a three-clause loop variable, appended to a slice that outlives the
loop. `-m` says `moved to heap: index`; goc's census says **frame** at the same
position. The census is not lying and goc is not wrong: goc records a frame
decision at the loop header *and a positionless heap allocation* for the
per-iteration copy, and the positionless row cannot join on position. This is
the program that established that a "goc frames what gc heaps" line can be
entirely an artefact of the join.

Both print `DISTINCT` then `0 1 2 3`.

## mapliteral.go — a slice literal inside a heap object, in a callee

`map[string]*bucket{"left": {values: []int{7, 11}}}` built in a function that
returns, with the map escaping to a global. This is
`goc/testdata/runtime_core_types.go`'s shape moved out of `main`.

goc heap-allocates both backing arrays here, agreeing with `-m` completely. The
frame placement the differential flags in the corpus program happens **only**
when the whole structure stays local to the frame — which is goc being more
precise than gc, not less safe.

Both print `7 11 13`.

## stackmove_goroutine.go — the sharpest test of that class

The same map built in a goroutine, which keeps the backing arrays in that
goroutine's frame: `opt.FrameEscapes` records both addresses being stored
through the write barrier into memory returned by `runtime.newobject`, and
`-m` puts both on the heap. The goroutine then grows its stack by 400 frames of
`[512]int` and forces a collection.

A stack copy relocates pointers found *on* the stack. Nothing scans the heap for
pointers *into* the stack, so if goc's frame placement were unsafe this is where
it would show. It does not: both compilers print `intact`. The class is a
confirmed difference and an unconfirmed hole; what would settle it is knowing
why goc's stack copier leaves this pointer valid.

## stackmoved.go — the control for the program above

`stackmove_goroutine.go` proves nothing unless the stack really moves. This
takes the address of a goroutine-local before and after the same recursion and
compares. Both print `stack moved`, so the test above is exercising a real stack
copy and not a stack that happened to be big enough already.

## bytesconv.go — `[]byte(string)` whose result outlives the frame

goc lowers `[]byte(s)` to `runtime.stringtoslicebyte` and hands it a **32-byte
stack buffer** when its escape analysis says the conversion does not escape
(`goc/compile.go`'s `stringSlice`). The text here is 16 bytes, so it fits, and
the result is stored in an object that outlives the call. `runtime.stringtoslicebyte`
is not one of `opt.AllocationCensus`'s allocators, so the census records nothing
either way and the differential can only report that goc has no allocation on
the line — this program is how you find out which.

Both print `0123456789abcdef`.

## cleanupclosure.go — a function literal handed to `runtime.AddCleanup`

The runtime keeps the closure and runs it after the object dies, long after the
registering frame is gone. `-m` says `func literal escapes to heap`; goc's
census records nothing, and goc has a frame path for closures
(`gen.localAlloc`), so the question is whether that path can be reached with a
closure the runtime retains.

Both print `11` then `cleanup-ran`.
