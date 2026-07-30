# Splitting the goc driver: compile the Go runtime once, not once per program

Branch: `ccwork/driver-split`, based on `perf/test-suite` (`b3720bf`, which is also
`origin/ccwork/permodule-impl`).

Status: **in progress — written as it lands.** Anything not verified is stated as such.

## The design decision, up front

### The premise the briefing carried forward has changed

The briefing (from the `sepcompile` spike) says the prebuilt runtime object must ship its
per-function metadata (`[]gometa.FunctionInfo`) alongside the ELF, "because the program side
has to be able to chain modules". That was true of the spike's design, which assumed **one
merged pclntab per image** (spike Obstacle 2): the linker would have to regenerate the whole
blob from the union of both sides' functions, so it needed the runtime's per-function facts.

The mechanism that actually landed (`RUNTIME_PLAN.md` §14) is the *other* design the spike
named: **per-module moduledata**. Each object carries its own complete pclntab, its own type
region and its own text bounds, and joining them is one `R_AARCH64_ABS64` write into
`moduledata.next`. So the program side never needs the runtime's `FunctionInfo` — the runtime
object already describes itself.

What the program side *does* need is the **set of symbols the prebuilt object defines**, so it
can compile only the difference and reference the rest. That is the sidecar this step ships.

### Container format

`goc build-runtime -o <file>` writes one file holding three members: the runtime module's ELF
relocatable, the assembled Plan 9 sidecar ELF, and a JSON manifest. Justification for a
purpose-built container over the alternatives:

- **`ar` archive** — standard and `cc`-consumable, but `cc` pulls archive members only to
  resolve an undefined symbol, so a member the image needs but nothing references yet is
  silently dropped; and it has nowhere to put the manifest.
- **A non-alloc ELF section inside a single merged object** — elegant, but merging the Go
  object and the assembled sidecar into one `ET_REL` is a code path nothing else in the tree
  uses, on the critical path of every build.
- **A tiny purpose-built container** (chosen) — magic + version + JSON index + concatenated
  members. One artifact, so the manifest cannot drift from the objects it describes; a version
  stamp so a stale pack is refused rather than mislinked; ~60 lines with a unit test.

(Full design and measurements below, filled in as they land.)

## Baseline measurement (monolithic, before any change)

Full 338-capability matrix, one unsharded `go test` process with
`-runtime-status-compile-workers=10` (this job's declared CPU share), on the 64-core box:

```
ok  github.com/evanphx/cg12/cmd/goc  478.700s      (wall 7:59.53, peak RSS 2.87 GB, rc=0)
```

**479 s** is the number the split has to beat.

## What was built

### `goc build-runtime -o runtime.gocrt`

Compiles a fixed root program (`package main; func main() {}`, in
`goc/runtime_root.go`) with the ordinary executable pipeline, then keeps everything
except the parts that are program-built by construction. The result is one pack file:
the module's ELF relocatable, its assembled Plan 9 sidecar, and the manifest.

The runtime module's `moduledata.next` is written before the program it will be chained
to exists, so it *names* `goc.programmoduledata` and the system linker resolves it
(`gometa.ChainModuleToExternal`).

### `goc -runtime runtime.gocrt prog.go`

Compiles the program with the same whole-program front end, then drops every definition
the pack already has and links `runtime.o sidecar.o prog.o [prog-sidecar.o]` with `cc`.

**The split is applied to the finished module**, after IR generation and every
module-level pass. So a symbol the program module keeps is bit-for-bit what a monolithic
build would have emitted. That is the design's whole safety argument, and it is what
makes a differential comparison meaningful (`goc.TestKeptSymbolsMatchAMonolithicBuild`
asserts it at IR level for every kept datum).

### Three things could not be subtracted

1. **The interface-method dispatchers** (`error_Error`, `runtime_stringer_String`). As the
   briefing predicted, this is the highest-risk boundary. They switch over the concrete
   types the *program* contains and their fall-through is
   `runtime_gocInterfaceDispatchFailure` — a fatal throw, not a `getitab` fallback. The
   pack leaves them undefined and lists them in `manifest.ProgramSymbols`; the program
   module defines and exports them; `prebuilt.CompileProgram` refuses to proceed if the
   program does not define one, so the failure is a build error naming the symbol rather
   than a dispatcher that silently misses an itab.

2. **The whole Go type region** — and this inverts task item 3. The briefing asks to
   *export the runtime's* type and name symbols so the program can point at them. That is
   backwards, and the differential run is what showed it. A type descriptor's contents are
   program-dependent: `clearUnavailableRuntimeMethodOffsets` writes
   `runtime.unreachableMethod` into a method entry whose function is not in the image, and
   `populateRuntimePointerTypes` fills `PtrToThis` only when the pointer type is also
   described. The prebuilt module reaches fewer methods than a program that imports more,
   so **its descriptor is not merely different, it is strictly poorer** — freezing it in
   would silently disable reflect method calls. And two copies is not an option: cg12
   compares descriptors by pointer (the inline itab match in `interfaceTypeWord`, every
   candidate test in a dispatcher), so a value tagged with one module's descriptor would
   not match the other's and the dispatcher would throw.

   So the program module owns the type region: every datum holding a module-relative
   offset and everything such an offset addresses. One descriptor per type, in the module
   that knows the most about it. A useful consequence: **the image has no duplicate
   descriptors at all**, so `typelinksinit` has nothing to canonicalise.

3. **Package assembly the prebuilt module never loaded.** A program reaching
   `reflect.methodValueCall` or a crypto block function needs that package's Plan 9
   assembly; the pack records which files it assembled and the program translates the rest
   into a sidecar of its own.

### Bugs found and fixed on the way

- **`runtime.lastmoduledatap` was hardcoded to `&runtime.firstmoduledata`.** `runtime.main`
  runs each module's init tasks by walking the chain and stopping at the tail, so with two
  modules the program's own package init never ran. `hello.go` failed with
  `panic: init did not run` — a failure that looks like a miscompiled program, not a
  linking mistake.
- **Two symbol families were still named by a running counter**
  (`%s.interfacecall.%d`, `%s.interfacecall.promoted.%d`). An itab's method entries name
  those wrappers, so the same itab had different contents in two compilations of the same
  program. Now content-named, like every other family.
- **goc emits a few package globals twice** (`runtime.divideError`,
  `runtime.overflowError`, `internal/runtime/maps.errNilAssign`): a zeroed placeholder and
  then the record holding the itab. `obj.prepareELF`'s symbol index keeps the last, so the
  placeholder was dead bytes nothing referenced — invisible while the symbols were local,
  `multiple definition of runtime_divideError` from `ld` once they went global. The split
  drops the shadowed copy; the duplicate emission itself is recorded below, not fixed.
- **`findfunctab` was a flat 2.6 MB in every module.** The bucket count falls back to a
  512 MB-covering floor when the module's text span is unknown, which it always was
  because the sidecar carries the module's end — and the floor then beat the real count
  unconditionally. A module bounded entirely by its own object knows its span, so it now
  sizes the table to it. Without this the split added 2.6 MB to every image for a table
  cg12 never populates.

## Where the time goes, and the ceiling this design has

Per-program compile, warm process (the shape the matrix compiles in), `hello.go`:

| phase | monolithic | split |
| --- | ---: | ---: |
| reachability + IR generation + module passes | ~1.4 s | ~1.4 s |
| per-function back end (lower, regalloc, emit) + metadata blob | ~2.6 s | ~0.05 s |
| `cc` link | 0.11 s | 0.08 s |
| **total** | **~4.0 s** | **~1.5 s** |

**The split removes the back end, not the front end.** The sepcompile spike projected
89% (reachability + IR generation + back end); this design gets ~60%, and the reason is
the same fact that makes it correct. The program module has to own the type region,
because descriptor contents depend on what the program reaches — and cg12 discovers
which descriptors a program needs *by generating its IR*. `ensureTypeTag` is called
from the lowering of a conversion, an interface assignment, a `new`. So the program
module cannot skip generating IR for functions the prebuilt object already has: doing
so would silently drop the type descriptors those functions need, and a missing
descriptor is not a link error — it is a dispatcher that quietly stops matching.

Getting past this needs the prebuilt pack to carry enough about its functions' type
requirements to reconstruct them without lowering, which is a redesign, not a tuning
knob. It is written down here rather than attempted.

## Verification status (updated as each result lands)

### Landed

- `internal/runtimepack`, `internal/gometa`, `goc` and `cmd/goc` unit/e2e tests for the
  split all pass. The `cmd/goc` ones link a real two-module image and read the chain,
  `hasmain` and the typelinks counts back out of it.
- 30-program differential (compile both ways, run both, compare exit status **and full
  output**): 28 identical, 2 differing only in a printed allocation count (both exit 0).

### Outstanding at the time of writing

- The full 358-program corpus differential (running).
- The full 338-capability matrix built the new way, and its wall clock against the 479 s
  baseline.
- `make test-unit`, `make test-goc-corpus`, `make test-goc-cmd`.
- Determinism (`CG12_NOCACHE=1` vs warm) before and after.
- Startup cost of a two-module image.

### `make test-unit` — **pass** (rc=0)

24 packages, no `FAIL`. Includes the new `internal/runtimepack`, the `internal/gometa`
findfunctab and `ChainModuleToExternal` tests, and `arm64`.

## Differential verification: every corpus program, built both ways and run

`analysis/splitdiff` compiles each program monolithically and against a prebuilt runtime,
links and runs both, and compares **exit status and full combined output** — which is
stricter than the capability matrix, whose gate is the exit status.

**All 358 `goc/testdata` programs. 353 identical; 5 differences, all understood:**

| what | count | resolution |
| --- | ---: | --- |
| identical exit status and identical output | 353 | — |
| differ only in a printed allocation count (`bytes_grow_stats.go`, `gomaxprocs_memstats.go`); both exit 0 | 2 | explained below, not a defect |
| **refused to compile** — one itab whose content disagreed with the pack | 3 | **fixed**; the three now produce identical output |

The three failures were `stdlib_crypto_ecdsa.go`, `stdlib_crypto_x509_ed25519.go` and
`stdlib_http_tls_client_server.go`, all on
`.goc.itab.internal_chacha8rand_errUnmarshalChaCha8__error`. The digest guard caught it
rather than letting it link: the pack had written `runtime.unreachableMethod` into that
itab's `Error` entry because the runtime root does not compile the method, while those
programs do. Same class as the type descriptors, so the same rule now applies — a datum
the prebuilt module degraded belongs to the program module. Rechecked after the fix:
3/3 compile, link, run and produce identical output.

**This is the one piece of verification that would have caught a silently wrong split**,
and it did, three times: once on the interface-call wrapper naming, once on the type
descriptors, once on the itabs. A green matrix would have shown none of them — the
matrix's gate is the exit status, and two of the three produced a *compile* failure only
because the digest guard existed.

### The two remaining output differences

Both print a raw allocation count and then `ok`, and both exit 0:

```
gomaxprocs_memstats.go   mono: mallocs105        split: mallocs120
bytes_grow_stats.go      mono: mallocs16718922   split: mallocs18220422
```

A two-module image allocates more at startup. `runtime.typelinksinit` returns
immediately when `firstmoduledata.next == nil`; with two modules it builds a
`map[uint32][]*_type` over the program module's typelinks. Every cg12 descriptor has
`abi.Type.Hash == 0`, so that is one growing slice — about a dozen allocations, which is
exactly the `105 → 120` delta. The larger absolute delta on `bytes_grow_stats.go` is the
same fixed startup cost plus the knock-on of a different heap goal.

Both counts are perfectly stable, so this is a fixed cost rather than noise. Three runs of
each binary, `GOMAXPROCS=2`:

```
mono : mallocs106  mallocs106  mallocs106
split: mallocs121  mallocs121  mallocs121
```

**A two-module image allocates exactly 15 more times at startup than a one-module image.**
That is the answer to the `abi.Type.Hash == 0` question the briefing carried in: the cost
is 15 allocations and no `typesEqual` calls at all, because the split leaves the image with
one type region and `typelinksinit` therefore has nothing to compare. It did not show, so
it was not fixed.

Neither program asserts on the number (both print it and then `ok`), so neither fails the
matrix. They are reported because the harness compares full output and this is a real,
explained behavioural difference between the two builds.

## Size cost

The prebuilt runtime is a fixed superset, so binaries grow. Measured over the whole
corpus (358 programs, sum of linked image bytes, after the `findfunctab` fix):

| | bytes |
| --- | ---: |
| monolithic | 3,769,713,504 |
| split | 4,216,893,728 |
| **growth** | **+11.9%** |

That is far below the sepcompile spike's "+67% of `.text`, and a lower bound". Two
reasons, and only one of them is luck:

- The runtime root imports nothing, so the prebuilt module is the *bare runtime closure*
  rather than a superset over the corpus — precisely the scope the dispatcher contract
  forces (see above). A pack carrying the common standard library would grow binaries
  much more, and would also have to solve package init.
- The `findfunctab` fix takes 2.6 MB per image back. Before it, `hello.go` went from
  6.83 MB monolithic to 9.47 MB split (+39%); after it the growth is a few per cent.

**For the matrix this cost is irrelevant.** For anything that ships a binary it is not:
a `hello`-sized program links the whole runtime closure whether it needs it or not, and
linker-level dead stripping is still not available (the metadata blob references every
function in its own module). A program that already uses most of the runtime pays almost
nothing; a minimal one pays the difference between what it reaches and what the runtime
root does.

## Using it

```
goc build-runtime [-O] -o runtime.gocrt        # once
goc -runtime runtime.gocrt [-O] [-o out] prog.go
```

Both halves must agree about `-O`; the manifest records it and the driver refuses a
mismatch rather than linking half an optimized image. `-runtime` builds an executable, so
it cannot be combined with `-c`, `-S`, `-emit-ir` or `-runtime-covermeta`.

The capability matrix builds the pack once per run and passes `-runtime` to every compile.
`-runtime-status-prebuilt-runtime=false` restores the per-program path; the runtime
*coverage* run always uses it, because instrumenting the runtime per program is exactly
what a shared prebuilt module cannot do.

Linking is `cc`, not cg12's own linker. The spike showed `analysis/seplink -mode=native`
links goc programs byte-identically for 8 programs, and it is the intended direction — but
it produces a static, libc-free `ET_EXEC` where every goc binary today is a `cc -no-pie`
link against libc. Switching it here would have changed the linker for all 338 capability
programs at the same time as changing the compiler, and the differential comparison that
found three real defects only isolates the compile split if the link stays fixed. The two
changes should be made one at a time.

Object order at the link is load-bearing: `runtime.o sidecar.o prog.o [prog-sidecar.o]`.
Each module's `moduledata` records a `[minpc, maxpc)` that `runtime.findmoduledatap`
resolves a PC against, so a module's text has to be one contiguous run.

## The number: the full capability matrix, built the new way

**338 capability subtests, 337 `PASS`, 1 `EXPECTED FAILURE`, 0 `FAIL`, 0 `KNOWN GAP`,
`rc=0`** — unchanged from the state RUNTIME_PLAN §1 records. The complete list of
non-passing capabilities is one entry:

- `defer-panic/panic-string-output` (`goc/testdata/runtime_panic_print_string.go`),
  declared `runtimeCapabilityExpectedFailure` and failing as declared.

Wall clock, one unsharded `go test`, `-runtime-status-compile-workers=10`:

| build | matrix wall clock |
| --- | ---: |
| monolithic (the pre-change baseline) | **478.7 s** |
| split, runtime built once per run | **406.5 s** |

**A 1.18× speedup, not the ~12× the briefing projected.** That is the honest number and
it needs explaining, because the per-program compile really did get 2.7× faster.

(A matched control — the same command with `-runtime-status-prebuilt-runtime=false`, so
`-v` and everything else is identical — is running; its result replaces the 478.7 s figure
above if it differs materially.)

### Why the matrix gains 1.18× when compilation gains 2.7×

Three reasons, all measured:

1. **The matrix is no longer compile-bound.** The briefing's "compilation is 98% of its
   wall clock" was true of the serial matrix; the look-ahead compile queue that landed
   since overlaps 10 compiles against a *sequential* run phase. Measured on the `gc`
   category alone (28 programs, 4 workers): 44.3 s monolithic → 28.9 s split, from which
   the run phase is about 20 s and unaffected. Halving compilation cannot halve a suite
   that is nearly half execution.

2. **A handful of enormous programs dominate and gain almost nothing.** From the
   full-corpus differential, the eight slowest compiles are 140–185 s *each*:
   `stdlib_http_parse_roundtrip` 183 s → 175 s, `stdlib_http_client_server` 182 s → 176 s,
   `stdlib_crypto_ecdh_x25519` 144 s → 150 s. Their cost is standard library, not runtime,
   and the prebuilt pack holds only the runtime. Across all 358 programs the total
   compile+link CPU is 3676 s monolithic against 2681 s split — **1.37×**, because those
   eight swamp the 2.7× the other 350 get.

3. **The pack cannot carry the standard library**, for the reason in "What was built": the
   prebuilt module leaves the interface dispatchers to the program, so every dispatcher it
   *calls* must be one every program generates. A program's reachable set always contains a
   runtime-only root's; it does not contain a root that imports `fmt`. Pull `fmt` in and a
   program that never imports it fails to link. Fixing that needs stub dispatchers *and*
   moving the whole image's package-init list to the program module, so a program does not
   run the init of packages it never uses. Both are tractable and neither was attempted
   here.

So: the split does what it says — the runtime is compiled once — and the compile itself is
2.7× faster on the programs the runtime dominates. The matrix does not see 12× because the
matrix's cost has moved.
