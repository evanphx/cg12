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

## 4. The default, per caller

**Chosen: the default differs by caller.** `cmd/goc`'s `main` calls
`goc.UseFunctionCacheByDefault()`; `goc.Compile` called in process does not have
one unless it asks with `CG12_FUNC_CACHE`.

The reason is clause 9 of the key -- the compiler binary's own hash, which is what
covers every compiler change without anyone having to remember a version number.
It is exactly right for a released binary compiling a user's program, because the
binary does not move between compiles, and exactly wrong inside `go test`, which
builds a fresh test binary for every package under test. On by default there
means: write a complete set of package units per test binary and read none of them
back, on this tree's own suite, forever. The Stage 2 report called that "pure
cost" and it was right; what it did not say is that the cost is confined to one
caller.

The alternative considered and rejected was on-everywhere with the suite opting
out through the environment. It fails for a plain reason: an opt-out that lives in
a Makefile is an opt-out that a developer running `go test ./goc/` by hand does
not get.

The switches, in the order they are consulted:

| | |
|---|---|
| `CG12_NOCACHE=1` | nothing read, nothing written, whatever else is set. Checked in `internal/cachefile.Disabled` so no cache can be added that forgets it. Held by `TestFunctionCacheSwitches` against the default, against an explicit directory and against `auto`. |
| `CG12_FUNC_CACHE=off` | this cache off, the pack cache and the source world untouched. New; the counterpart `auto` already needed. |
| `CG12_FUNC_CACHE=auto` | the default location, whatever the caller's default is. |
| `CG12_FUNC_CACHE=<dir>` | that directory. |
| unset | the default location for `cmd/goc`, no cache for a library caller. |

**The default location** is `os.UserCacheDir()/cg12/function-cache`, which is the
shape `cmd/goc/packcache.go`'s `packCacheDirectory` already had, through the same
`cachefile.Directory` helper and with the same two-hex-character fanout. A box
with no user cache directory gets no cache rather than an error.

## 5. A broken cache never fails a compile

`goc/functioncachedefault_test.go`. Nine ways the store can fail, each compiled
against the same program and required to produce the module a `CG12_NOCACHE=1`
compile produces, with no error:

| arm | what the compile did |
|---|---|
| a read-only cache directory | 0/46 packages hit, 0 files written |
| a read-only directory **that already has units in it** | **46/46 packages hit, 3043/3217 declarations replayed**, 0 files written |
| a file where the directory should be | 0/46 hit, 0 written |
| a file where a fanout directory should be | 0/46 hit, 46 written (the other 251 buckets work) |
| units truncated to half length | 0/46 hit, 46 written -- it repairs itself |
| units emptied | 0/46 hit, 46 written |
| units with two bytes flipped | 0/46 hit, 46 written |
| units replaced with an HTML error page | 0/46 hit, 46 written |
| units `chmod 000` | 0/46 hit, 46 written |

All nine byte-identical to the uncached module. The second row is the one worth
keeping: a cache nobody can write is still a cache, and it served 84% of the
lowered IR.

A tenth arm covers the path an ordinary `goc file.go` takes, where nobody named
the directory and a failure has nobody to report to: `XDG_CACHE_HOME` pointed at a
regular file, cache on by default, compile identical.

What makes this hold rather than being nine lucky cases: a read that fails is a
miss (`cachefile.Read` returns `found=false` for any error), a unit that does not
decode is a miss (the format carries a sha256 of its own body, so a truncation or
a flipped byte fails the digest rather than decoding into something plausible),
and a write that fails is counted and dropped (`flush` only increments `Wrote` on
success). The one path that still returns an error is a replay that fails after it
has begun splicing, and that is deliberate: the compiler binary's hash is a clause
of the key, so a unit found under a key was written by a byte-identical compiler,
and a replay failure there is an assertion rather than a cache-integrity event.

## 6. Eviction

**It exists**, and the previous gate was right not to have taken that on trust:
`internal/cachefile` has had `Trim` and `MarkUsed` since the pack cache, and
`goc/functionmerge.go:flush` calls `Trim` on every compile that uses the cache.
It was **untested**. It is now, in `internal/cachefile/cachefile_test.go`, and it
has a second bound.

- **Age.** Five days since last use, checked at most once a day per directory
  (a stamp file, so it survives the process). `TestTrimRemovesWhatNobodyHasRead`,
  `TestTrimIsRateLimited`.
- **Size, new.** Least recently used by mtime until the directory fits a budget.
  `TestTrimKeepsTheDirectoryUnderBudget`, and
  `TestTrimLeavesTheDirectoryAloneUnderBudget` for the case that has to be free.
- **A hit keeps a unit alive.** `cachefile.Read` calls `MarkUsed`, which refreshes
  mtime at hourly granularity so a warm build does not turn every read into a
  write. `TestAHitKeepsAUnitAlive` holds it against *both* bounds -- a unit older
  than the cutoff that is read survives, and it sorts young in the budget's
  ordering. `TestMarkUsedIsHourly` holds the granularity.
- **It cannot fail a build.** `TestTrimSurvivesADirectoryItCannotChange`: an empty
  path, a path that does not exist, a file where the directory should be, and a
  read-only directory with a stale entry in it -- which keeps serving.

**The budget: 1 GiB.** The arithmetic, from the corpus fill. One program's units
are 16.9 MB in 45 files for `fmt_sprintf` and 55 MB in 156 files for the http/tls
program. A package's key does not mention the program, so compiling the whole
corpus with one compiler converges on the *union* of their closures rather than
the sum -- measured on 406 programs in §7 below. Call a full corpus fill against
one compiler 60 MB. A gibibyte is about sixteen of those, and sixteen is the
number that matters, because what multiplies this cache is not how much a user
builds but how often the compiler binary changes: its hash is a clause of the key,
so a gate box or anyone working on cg12 itself mints a whole new generation of
units per build, and five days of that is unbounded in exactly the way an age
cutoff cannot see.

The pack cache keeps age-only eviction (`Trim(directory, 0)`): it holds a handful
of large files rather than a generation of small ones, and nothing has measured
what its budget should be.
