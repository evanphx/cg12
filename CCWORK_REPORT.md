# Stage 3: the function cache on by default

Branch `ccwork/cache-on-by-default`, off `main` at 0cfe5aa.

## 1. The delta-comparison probe, kept as a test

`goc/functioncachedelta_test.go`. The property it checks, stated once:

> A stored declaration's delta is a function of its package, not of the program
> that was being compiled when it was stored.

The check follows from the key. A unit's file name is a content address of every
clause of the key -- package source, transitive dependency identities, target,
layout, pipeline, compiler -- so two programs that write the same file name
agreed on all of it. If those two files then disagree about what one declaration
contributed, the disagreement came from the program.

Two pairs are run: the disjoint-closure pair (`reflect`/`container/list`/`cmplx`
against `bufio`/`strings`/`os`) and a shared-closure pair. Both compare every
declaration AND every interned artifact the two directories hold under the same
key, component by component -- encoded IR, artifact references, intern notes,
file table.

**First run, before any fix: 33 program-dependent deltas on the disjoint pair, 8
on the shared pair. Every one of them the file table.**

    internal/abi   .../abi.go:100:27  (IntArgRegBitmap.Get): [] against [stdlib/src/internal/abi/abi.go]
    runtime        .../slice.go:392:6 (runtime.slicecopy):    [stdlib/src/runtime/slice.go] against []
    sync           .../map.go:64:15   (sync.Map.LoadOrStore): [] against [stdlib/src/sync/map.go]

That is leak 1, reproduced as a named declaration and a diff rather than as a
corpus program that fails to link a stage later.

## 2. The two leaks, fixed

### Leak 1 -- `NewFiles`, now `Files`

`cachedDeclaration.NewFiles` was `module.Files[mark.files:]`: the files the
declaration appended to `Module.Files`. `Module.File` appends on first use, so
"the files a declaration added" is a fact about which declarations ran *before*
it. A declaration that added `[a, c]` in the program that filled the cache adds
`[a, b, c]` in a program that had not yet seen `b`; replaying the stored answer
puts `b` at the end instead of the middle. Nothing dangles -- `remapFilePositions`
appends whatever the unit references -- but the file TABLE comes out in a
different order, and that is DWARF's file numbering.

The repair records the files the declaration **touched**, in first-touch order,
which is a fact about the declaration. `internJournal.file` is called from `g.at`
(the one place lowering resolves a position) and the replay walks the list through
`Module.File`, which appends only what the receiving module lacks -- exactly what
the cold compile did.

The first attempt at it collapsed only *runs*, to keep `g.at` cheap. That was
program-dependent in the same way and the probe said so immediately: 37
declarations recorded an empty file list in one program and a one-file list in the
other, according to whether the declaration lowered before them happened to be in
the same file. `beginFileScope` now floors the deduplication at the start of each
declaration.

### Leak 2 -- the pointer key journalled with a runtime type

`runtimeTypeKey` strips a signature's parameter names only when the signature is
the top level, so `types.NewPointer` of one keeps them. `*func(p []byte)` from one
declaration and `*func([]byte)` from another are two spellings of one type, and
which one gets journalled is decided by which declaration reached the type first.
A replayed spelling overwrote the compile's own in `pointerTypeKeys` and
`PtrToThis` was then left unset.

The repair is `runtimePointerTypeKey`: canonicalise the element *before* taking the
pointer, at both the journal site (`ensureTypeTag`) and the live derivation
(`functionCache.pointerTypeKeys`).

**Measured before the change**, by instrumenting `populateRuntimePointerTypes`:

| program | pointer-key entries | with a named-parameter spelling | of those, resolving to a pointer descriptor today | ... under the canonical spelling |
|---|---|---|---|---|
| `hello.go` | 499 | 86 | **0** | **0** |
| `fmt_sprintf.go` | 1170 | 289 | **0** | **0** |
| `stdlib_http_tls_client_server.go` | 4191 | 1068 | **0** | **0** |

So the class this fixes fills no `PtrToThis` field under either spelling: the
change costs nothing that was working and removes the program-dependence. That is
why it is safe to make on the cold path.

## 3. Looking for a fifth

The probe compares **every** component of a stored delta -- encoded IR, artifact
references and their positions, intern notes, file table -- for every declaration
and every artifact two programs stored under the same key. Anything whose value
depends on the program rather than the package shows up as a named declaration
and a diff.

Run over a spread of 24 corpus programs (14 `stdlib_*`, from `net/netip` and
`text/template` through `crypto/hmac` and `encoding/json`, plus 10 runtime and
defer programs), each filling its own directory, each compared against the first:

    24 programs, 23 pairwise comparisons
    57442 declaration comparisons, 54263 artifact comparisons
    0 program-dependent deltas

and separately the first 30 of the corpus in name order: 29 comparisons, also
clean. **No fifth leak of this shape was found.** That is a negative result over
a wide sample, not a proof; what makes it worth something is that the same
instrument found leaks 1 and 3 (the run-collapse) on the first run.
