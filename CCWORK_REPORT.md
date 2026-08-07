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

## 7. The acceptance test: default-on against `CG12_NOCACHE=1`

`scripts/function-cache-default-check.sh`. Every corpus program compiled with
**nothing set at all** -- no `CG12_FUNC_CACHE`, so what is exercised is `cmd/goc`'s
own default and the default *location* -- against a `CG12_NOCACHE=1` control built
by the same binary, in a separate process. `XDG_CACHE_HOME` is redirected into the
work tree so the run is reproducible and leaves the caller's real cache alone.

Two passes per program: the first against whatever the other 405 have already put
in the shared directory, the second against its own units. Both compared to the
same control. 28 jobs writing into one directory concurrently, which is what
default-on actually looks like.

**Arm without `-O`: 406 identical, 0 different, 0 failed to build.**
The shared default cache ended with **204 units, 76 MB** -- which is the number
the 1 GiB budget was sized against: the whole 406-program corpus converges on the
union of their closures, not the sum.

**Arm with `-O`: 406 identical, 0 different, 0 failed to build.** Same 204 units,
76 MB. That arm matters more than it looks: `-O` is deliberately not a clause of
this cache's key -- units are the front end's output and the optimiser runs after
the merge -- so the two arms shared nothing except the compiler, and each had to
be served by units the other arm's shape does not appear in. Stage 2 checked the
`-O` cross-program property on three programs; this is 406.

    function-cache-default-check: 406 programs, arm 'none'
    function-cache-default-check: 406 identical, 0 different, 0 failed to build
    function-cache-default-check: the shared default cache ended with 204 units, 76M

    function-cache-default-check: 406 programs, arm '-O'
    function-cache-default-check: 406 identical, 0 different, 0 failed to build
    function-cache-default-check: the shared default cache ended with 204 units, 76M

## 8. What it costs and what it saves

`scripts/function-cache-check.sh -rounds 3`, separate processes, quiet box,
median of three. The script gained a **`funcoff`** column for this
(`CG12_FUNC_CACHE=off`, which the switch table above made possible): `nocache`
turns off the pack cache too, and a saving quoted against it would credit this
cache with that one's work.

| program | `-O` | nocache | funcoff | cold fill | warm | cold vs nocache | warm vs nocache | warm vs funcoff |
|---|---|---|---|---|---|---|---|---|
| `hello.go` | | 3.18 | 3.15 | 3.34 | 2.27 | **+5.0%** | **−28.6%** | −27.9% |
| `hello.go` | `-O` | 7.77 | 7.70 | 7.92 | 6.71 | **+1.9%** | **−13.6%** | −12.9% |
| `fmt_sprintf.go` | | 6.60 | 6.64 | 6.91 | 5.39 | **+4.7%** | **−18.3%** | −18.8% |
| `fmt_sprintf.go` | `-O` | 16.75 | 16.66 | 16.98 | 15.14 | **+1.4%** | **−9.6%** | −9.1% |
| http/tls | | 32.86 | 32.71 | 33.81 | 30.08 | **+2.9%** | **−8.5%** | −8.0% |
| http/tls | `-O` | 73.64 | 73.66 | 74.93 | 70.12 | **+1.8%** | **−4.8%** | −4.8% |

Seconds. Every one of the 24 builds byte-identical to its `nocache` control.

**`funcoff` is within 0.4% of `nocache` on every row.** That answers a question
nobody had asked and should have: a plain `goc -o out prog.go` does not consult
the pack cache, so the Stage 2 figures were not inflated by it and these are
directly comparable.

**Against Stage 2.** The brief quotes Stage 2 as −19.6% / −9.7% without `-O` and
−8.6% / −6.4% with it, for `fmt_sprintf` and http/tls.

| | Stage 2 | here | |
|---|---|---|---|
| `fmt_sprintf`, no `-O` | −19.6% | **−18.3%** | 1.3 points worse |
| http/tls, no `-O` | −8.6% | **−8.5%** | unchanged |
| `fmt_sprintf`, `-O` | −9.7% | **−9.6%** | unchanged |
| http/tls, `-O` | −6.4% | **−4.8%** | 1.6 points worse |

Two rows are unchanged and two are one to two points worse. Where it went: the
`Files` repair records every file a declaration touched rather than only the ones
it added, so a `g.at` call now costs a short backwards scan and every unit carries
a slightly longer file list; and `record` allocates that list per declaration
rather than reslicing `module.Files`. The `-O` http row is the most sensitive
because it is the row with the most declarations and the largest denominator, and
it is a whole-compile figure, so a 1.6-point move is about a second on a 74-second
build. Nothing about the saving's shape changed: the cache still removes most of
the stage it covers, and what bounds it is still the refusals §7 of the Stage 2
report describes.

**The cold fill.** +1.4% to +5.0%, and it is what a user meets first. It buys the
key -- hashing every source file of the closure and the compiler binary -- and the
encoding of every unit it stores, and gets nothing back. The smallest program pays
the largest penalty, because the fill is close to a fixed cost against the least
work. It is documented in BUILD_CACHE.md in those terms rather than as a footnote.

## 9. Guards

| | |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt` | clean |
| `scripts/function-cache-default-check.sh`, both arms | **812 of 812 byte-identical** (§7) |
| `scripts/function-cache-corpus-check.sh` | **406 identical, 0 different, 0 failed to link** against a cache filled by `fmt_sprintf.go` |
| `TestIRVerifyAudit` | ok (90.9 s) |
| `scripts/determinism-check.sh` | all five programs identical, both rounds, both caching paths, including `runtime_defer_capture_allocs.go` (the RUNTIME_PLAN §5.10 exception) |
| the goc cache test set | `TestWarmCompileIsByteIdenticalToCold`, `TestNoCacheBypassesTheStore`, `TestCacheFilledByAnotherProgramIsUsable`, `TestCacheFilledByAProgramWithADisjointClosure`, `TestPackageUnitRoundTrip`, `TestChangedDependencyInvalidatesTheUnit`, the two delta probes, the four default/robustness tests and `TestEveryTestIsParallelOrListedAsSequential`: ok (146 s) |
| `go test ./internal/cachefile/` | ok -- eight eviction tests where there were none |
| `make verify-fast` | **PASS in 5m34s** -- build, vet, gofmt, unit, the three corpus shards, the parallel corpus, both capability-matrix arms, the reducers |
| `scripts/function-cache-check.sh` | **all builds identical**: 4 programs x 2 arms x 4 conditions (`nocache`, `funcoff`, `cold`, `warm`), and all 12 ordered cross-program pairs on both arms |
| `CG12_DELTA_PROBE` sweep | 24 diverse corpus programs and the first 30 in name order: 0 program-dependent deltas |

`TestCheckedRuntimeCoverageBaselineDenominator` fails on `main` too and was not
investigated, per the brief. Not run, per the brief: the corpus suite beyond what
`make verify-fast` carries, the capability matrix beyond its two shards,
`make test-unit` standalone, the four audits and the crash loops.

## 10. What is not covered

- **The default-on corpus arm does not test a stale cache.** It tests an empty
  one filling and a full one serving, both from the same compiler. A cache left
  over from an older compiler is a key miss by construction (clause 9), and
  `TestChangedDependencyInvalidatesTheUnit` holds the finer-grained case, but no
  arm here compiles against a directory a *different* compiler filled.
- **The 1 GiB budget is not exercised at scale.** `cachefile_test.go` evicts
  kilobytes and asserts the ordering; nothing has run a real cache past a
  gibibyte and watched it recover. The failure mode if the ordering is wrong is a
  cache that evicts its working set and stops hitting -- slow, not incorrect.
- **The probe compares what two programs BOTH stored.** A declaration only one of
  them reached is not compared, and neither is one that both refused. What it
  covers is measured rather than assumed -- 57442 declarations and 54263
  artifacts over the 24-program sweep -- but it is not everything a unit can hold.
- **No fifth leak is a negative result, not a proof.** The instrument found leaks
  1 and 3 on its first run, which is the reason to believe it; it cannot see a
  fact that is program-dependent in a way that happens to agree across every pair
  it was pointed at.
- **The replay path can still return an error.** A unit whose digest is valid and
  whose IR does not decode aborts the compile rather than falling back, because
  by then the module has been partly spliced. The argument that it is unreachable
  is the key: clause 9 means a unit found under a key was written by a
  byte-identical compiler, so "digest valid" implies "decodable by this binary".
  That is an argument, not a test.
- **`cmd/goc` tests now exec a compiler with the cache on.** They get a real
  cache in the real default location, as a user would, and the binary they build
  changes with the tree -- so they fill a generation of units per build and read
  few back. That is what the 1 GiB budget is for, and it is why the budget went
  in with the default rather than after it.

## 11. Verdict

**Both leaks are fixed and a fifth was looked for.** `NewFiles` is now `Files` --
the files a declaration touched, in first-touch order, journalled at `g.at`
rather than inferred from what it appended -- and the pointer key journalled with
a runtime type is canonicalised before the pointer is taken, at both the journal
site and the live derivation. The instrument that found them is
`goc/functioncachedelta_test.go`, kept as a test: it compares the stored deltas of
two programs that agreed on a unit's key, component by component, and it found a
third leak of the same shape (a run-collapse that crossed the declaration
boundary) the moment the first repair was attempted. Pointed at 24 diverse corpus
programs -- 57442 declaration and 54263 artifact comparisons -- and separately at
the first 30 in name order, it finds nothing further.

**The default is per caller.** `cmd/goc` calls `goc.UseFunctionCacheByDefault`;
`goc.Compile` called in process does not have a cache unless it asks. Clause 9 of
the key is the compiler binary's own hash, which is right for a released binary
and wrong inside `go test`, which builds a fresh test binary per package under
test. `CG12_NOCACHE=1` still turns off everything, and is held against the
default, an explicit directory and `auto`. `CG12_FUNC_CACHE=off` is new and turns
off this cache alone. The default location is
`os.UserCacheDir()/cg12/function-cache`, `packCacheDirectory`'s shape, with the
same fanout.

**A broken cache degrades silently.** Nine ways of breaking the store -- read-only
directory, file where the directory should be, file where a fanout directory
should be, truncated, empty, corrupt, foreign and unreadable units -- each
produces the module a `CG12_NOCACHE=1` compile produces, with no error, and so
does an unusable default location on the path an ordinary `goc file.go` takes. The
read-only directory that already has units in it still serves 84% of the lowered
IR. Eviction, which existed and was untested, is now tested and has a second bound:
1 GiB of least-recently-used on top of the five-day cutoff, sized from a measured
76 MB full-corpus fill, with a read refreshing an entry's mtime so a unit in use
never ages out.

**The measured saving with it on by default**, against a `CG12_NOCACHE=1` control
in a separate process, median of three:

| | no `-O` | `-O` |
|---|---|---|
| `hello.go` | **−28.6%** | **−13.6%** |
| `fmt_sprintf.go` | **−18.3%** | **−9.6%** |
| `stdlib_http_tls_client_server.go` | **−8.5%** | **−4.8%** |

and the first compile, which is what a user meets first, is **1.4% to 5.0%
slower**.

**812 of 812 corpus programs, on both `-O` arms, compiled with nothing set at all
are byte-identical to the same program compiled with `CG12_NOCACHE=1`.**
