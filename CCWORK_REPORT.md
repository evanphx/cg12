# `-O` plus the prebuilt runtime pack does not link

**Status: root cause found and explained. Fix in progress; verification below is updated as
it lands.**

## The defect

    goc build-runtime -O -o rt.gocrt
    goc -O -runtime rt.gocrt -o out goc/testdata/reflect_makefunc.go

    goc-program-runtime.o: in function `reflect_makeFuncStub_abi0':
    undefined reference to `reflect_moveMakeFuncArgPtrs'
    undefined reference to `reflect_callReflect_abi0'
    undefined reference to `reflect_callMethod_abi0'

Reproduced on this branch at `9cd2621`. Without `-O` the same command links; `-O` with no
pack links.

## Root cause

`ir.Func.Linkage.Export` carries two unrelated meanings, and the split destroys one of
them.

1. `reflect.callReflect`, `reflect.callMethod` and `reflect.moveMakeFuncArgPtrs` have **no
   caller in Go**. Their only callers are in `reflect/asm_arm64.s`, which is Plan 9 assembly
   and is not part of the cg12 IR module.

2. `goc/compile.go`'s `exportAssemblyReferencedFunctions` therefore marks every
   assembly-referenced Go function `Linkage.Export = true`. In the IR this bit is not
   really about ELF binding — the backend sets a symbol `Global` for assembly references on
   its own (`arm64/mc.go`: `Global: code.export || assemblyReferences[code.name]`, and the
   sweep at the end of `compileToObjectWithBundle`). Its load-bearing job is to be the
   **keep-alive root for `opt.DeadFuncElim`**, which keeps a function iff
   `f.Linkage.Export || referenced-by-a-symbol-operand-in-this-module`.

3. `goc/runtime_split.go`'s `finishProgramModule` then does

       function.Linkage.Export = programSymbols[name]

   an **assignment**, not an addition. Every function the pack did not leave for the program
   loses its export bit, including the assembly-referenced ones.

4. `opt.OptimizeModule` runs **after** the split (`internal/prebuilt.CompileProgram`), so
   `DeadFuncElim` sees three functions with no export bit and no IR reference, and deletes
   them.

5. The consequence is not only a missing Go symbol. `arm64.emitGoABI0AssemblyWrappers`
   emits the ABI0→Go-internal bridge `reflect_callReflect_abi0` **only for names present in
   `module.Funcs`**. With the function gone the wrapper is never emitted either, so the
   sidecar both loses the definition and keeps the reference — which is exactly the
   error text: the sidecar `goc-program-runtime.o` is the object that references all three.
   `moveMakeFuncArgPtrs` is a `PreferDirectABI0` symbol, so the assembly names the Go symbol
   directly and it goes undefined the same way.

Measured directly rather than inferred — the program module's own function list, before and
after `opt.OptimizeModule`, compiled against the optimized pack:

    before opt (2 funcs match "reflect_call"):
      reflect_callMethod    export=false
      reflect_callReflect   export=false
    after opt (0 funcs match "reflect_call"):

and the resulting object tables:

| | `-O` + pack | no `-O` + pack |
| --- | --- | --- |
| `reflect_callReflect` in program object | absent | `DEF STB_GLOBAL` |
| `reflect_callReflect_abi0` in program sidecar | `UND` | `DEF STB_GLOBAL` |

Neither half is at fault alone. Without `-O` nothing eliminates the function, so clearing
the bit is invisible. Without the pack nothing clears the bit, so `DeadFuncElim` keeps it.
It needs both, which is why the configuration that ships is the one that fails.

`goc/reach.go`'s seeding from `assemblyReferences` (the hypothesis in the task) is **not**
the problem: the functions are present in the module the front end produces, with the right
names, in both configurations. The loss happens after the split, in the optimizer.

## Fix

`ea04425` — `goc/runtime_split.go`, one line, plus the parameter that feeds it:

```go
-		function.Linkage.Export = programSymbols[name]
+		function.Linkage.Export = programSymbols[name] || assemblyReferences[assemblySymbolName(function.Name)]
```

Exporting is additive for the functions this compilation's Plan 9 assembly names.
`finishProgramModule` takes `assemblyReferences` — the same map `compile` already
computes and already hands to `exportAssemblyReferencedFunctions`.

Why this and not something else:

- **Not `reach.go`.** The functions *are* in the module the front end produces, in both
  configurations, with the right names. Reachability seeding is fine.
- **Not "preserve every pre-existing export".** That would also preserve the export bit
  `ast.IsExported` gives every capitalized Go function, so `DeadFuncElim` would stop
  eliminating anything in a split build and images would grow for no reason. The bit that
  carries information the split cannot reconstruct is the assembly one.
- **Not a change to `DeadFuncElim`.** It cannot see Plan 9 assembly; the module does not
  carry the translated references, only the source files. The existing design is
  "assembly-referenced ⇒ exported ⇒ a DCE root", and the defect was that the split broke it.
- Making these symbols global costs nothing at link: the backend already forces a symbol
  global when the module's own assembly names it. Measured rather than asserted — two goc
  binaries built from the same tree path, one at `9cd2621` and one with the fix, each
  compiling `reflect_makefunc.go` against a non-optimized pack three times:

  | | |
  | --- | --- |
  | the pack itself | **byte-identical** between the two compilers |
  | each compiler's three images | byte-identical to each other (so the diff below is real, not §5.10 nondeterminism) |
  | pre-fix vs post-fix image | **23 bytes differ out of 11178048**, same size, same sections |
  | ELF symbol table — name, value, size, type, binding, section index, *and order* | **identical** |

  The 23 bytes are three `DW_AT_external` flags in `.debug_info` going 0 → 1
  (`obj/dwarf.go:343`), one each immediately before `reflect_moveMakeFuncArgPtrs`,
  `reflect_callReflect` and `reflect_callMethod`, plus the 20-byte
  `.note.gnu.build-id` the linker derives from the image. So nothing linkage-visible
  changes on the unoptimized path; the debugger now agrees the three functions are
  external, which they are.

## Verification

| check | result |
| --- | --- |
| `goc build-runtime -O` + `goc -O -runtime` on `reflect_makefunc.go` | links, runs, exit 0; output identical to `go run` |
| the 19 `runtime-packages/reflect-*` capabilities under `-runtime-opt` | **19/19 PASS** (49.4 s) |
| `stdlib-crypto/ecdh-x25519`, `stdlib-encoding/binary`, `stdlib-encoding/binary-varint` under `-runtime-opt` | **3/3 PASS** (11.9 s) |
| the same 22 with the fix reverted to `9cd2621`, as a control | **6 PASS, 16 FAIL**, all with the reported link error |
| `TestAnOptimizedProgramKeepsTheFunctionsOnlyAssemblyCalls` | PASS with the fix, FAILs with the same three undefined symbols without it |

New test: `cmd/goc/prebuilt_test.go`. It builds an optimized pack, compiles a
`reflect.MakeFunc` plus method-value program against it with `-O`, links and runs it, and
checks the output. Its output (`doubled 42`, `plus 17`) matches `go run` on the host
toolchain. It costs about 8 s: an optimized pack build is ~3.5 s cold.

### The full capability matrix, both arms

Both run as one process at `-runtime-status-compile-workers=8` with a cold, per-run
`CG12_PACK_CACHE` (see the caveat below), on a box shared with one sibling job.

| arm | subtests | PASS | EXPECTED FAILURE | FAIL | KNOWN GAP | wall clock |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `-runtime-opt` | **345** | **344** | 1 | **0** | **0** | 222.3 s |
| default | **345** | **344** | 1 | **0** | **0** | 203.3 s |

Census taken from `-v` output: 345 distinct `--- PASS: .../<category>/<name>` lines, zero
`--- FAIL`, zero `--- SKIP`, one `EXPECTED FAILURE runtime_panic_print_string.go`, no
`KNOWN GAP` line. **The complete list of non-passing capabilities under `-runtime-opt` is
empty**, other than the one declared expected failure, which is the same one the default
arm declares.

The optimized arm costs **9.4% more wall clock** than the default arm. That is the
optimizer running over both modules; it compiles and runs the same programs.

### The sixteen, named

The pre-fix control at `9cd2621`, `-runtime-opt`, over the three affected categories: 22
subtests, 6 PASS, **16 FAIL**, every one the same link error. Exactly the set §5.10
recorded.

```
runtime-packages/reflect-call-aggregate            stdlib-crypto/ecdh-x25519
runtime-packages/reflect-call-aggregate-function   stdlib-encoding/binary
runtime-packages/reflect-deep-equal                stdlib-encoding/binary-varint
runtime-packages/reflect-interface-extract
runtime-packages/reflect-interface-method
runtime-packages/reflect-make-values
runtime-packages/reflect-map-slice
runtime-packages/reflect-method-metadata
runtime-packages/reflect-select
runtime-packages/reflect-set-fields
runtime-packages/reflect-type-assert
runtime-packages/reflect-type-metadata
runtime-packages/reflect-value-call
```

All 16 pass with the fix, as does the rest of the matrix.

### The rest of the suite

| | |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `gofmt -l` over every non-`stdlib/` Go file | clean |
| `make test-unit` | PASS |
| `make test-goc-cmd` | PASS, 248.3 s |
| `make test-goc-corpus` (the non-executable compile path) | PASS, 598.0 s |

### Still unverified

- Nothing in this change's scope. Everything claimed above was run on this branch.
- Not attempted, and stated so it is not mistaken for coverage: no measurement of what the
  `-O` split now costs in image size, and no differential run of the corpus under `-O`
  against the host toolchain. The corpus's own `-O` coverage is whatever
  `make test-goc-corpus` already does.

## Keeping it fixed

The reason this shipped broken is that nothing ran it. Added:

- `make test-goc-status-opt` — the matrix with `-runtime-opt`, sharded the same way as
  `test-goc-status`.
- a `runtime-status-opt` CI job, 4 shards, alongside the existing `runtime-status`.

It runs *alongside* the default arm rather than replacing it: the two differ in what they
eliminate, so neither covers the other.

**What it costs.** In CI it is four more `ubuntu-24.04-arm` jobs running in parallel with
the four existing `runtime-status` shards, so it adds no critical-path wall clock — the
whole matrix is already the longest job and the two arms run concurrently. It roughly
doubles the matrix's CI *machine* time. Measured locally, one full arm is 222.3 s against
the default arm's 203.3 s at eight compile workers, so a shard's job timeout is set to 25
minutes against the default arm's 20.

There is also a cheap guard that does not need the matrix at all:
`TestAnOptimizedProgramKeepsTheFunctionsOnlyAssemblyCalls` in `make test-goc-cmd`, ~8 s.
That is the one that actually fails fast; the matrix arm is what catches the next defect
that is specific to `-O` and is not about reflect.

## Defects found on the way, not fixed here

- **A runtime pack is cached and fingerprinted without regard to the compiler that built
  it.** `packCacheKey` hashes the pack-format version, the target, `-O`, the carried
  package list and the *stdlib source*; `Manifest.Fingerprint` is `activeRuntimeSourceID`,
  the runtime source identity. Neither depends on the goc binary. So changing the compiler
  and rebuilding a pack silently reuses the pack the old compiler wrote, and the staleness
  check that exists (`Fingerprint`) will not catch it. It did not affect this fix, which
  only changes the program half, but it is a trap for the next change that touches
  `finishRuntimeModule` — and it makes any measurement taken without clearing
  `CG12_PACK_CACHE` untrustworthy. Every measurement above used a fresh per-run cache
  directory. Recorded in RUNTIME_PLAN §22 "What is not done".

- **`opt.DeadFuncElim` does nothing on the pack side.** `finishRuntimeModule` exports every
  function it keeps, so the optimized pack is not smaller than the unoptimized one by a
  single function. This is *correct* — the program module has to be able to reference any
  of them — but it is undocumented, and it means "the pack is built with `-O`" buys less
  than it sounds like.

- **The conflation itself is unfixed.** `ir.Func.Linkage.Export` still means both "global
  in the object" and "a root for dead-function elimination", and the split now has to know
  about Plan 9 assembly in order to keep the two apart. A separate `ir.Func` flag for
  "reachable from outside the IR" would let the split answer only the binding question,
  which is the one it is qualified to answer. Not attempted: it touches every producer and
  consumer of the flag, which is a change with its own validation cycle.

Nothing was found in the sibling job's area (`ccwork/frontend-determinism-2`, the front
end's emission order).

