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
  global when the module's own assembly names it, so in the non-`-O` path this changes no
  emitted byte.

## Verification

| check | result |
| --- | --- |
| `goc build-runtime -O` + `goc -O -runtime` on `reflect_makefunc.go` | links, runs, exit 0; output identical to `go run` |
| the 19 `runtime-packages/reflect-*` capabilities under `-runtime-opt` | **19/19 PASS** (49.4 s) |
| `stdlib-crypto/ecdh-x25519`, `stdlib-encoding/binary`, `stdlib-encoding/binary-varint` under `-runtime-opt` | **3/3 PASS** (11.9 s) |
| the same 22, with the fix reverted (`git stash`), as a control | fail with the reported link error |
| `TestAnOptimizedProgramKeepsTheFunctionsOnlyAssemblyCalls` | PASS with the fix, FAILs with the same three undefined symbols without it |

New test: `cmd/goc/prebuilt_test.go`. It builds an optimized pack, compiles a
`reflect.MakeFunc` plus method-value program against it with `-O`, links and runs it, and
checks the output. Its output (`doubled 42`, `plus 17`) matches `go run` on the host
toolchain. It costs about 8 s: an optimized pack build is ~3.5 s cold.

### Still unverified at the time of writing

- The full matrix under `-runtime-opt` (running).
- The full matrix on the default arm, `make test-unit`, `make test-goc-corpus`,
  `make test-goc-cmd`.

## Keeping it fixed

The reason this shipped broken is that nothing ran it. Added:

- `make test-goc-status-opt` — the matrix with `-runtime-opt`, sharded the same way as
  `test-goc-status`.
- a `runtime-status-opt` CI job, 4 shards, alongside the existing `runtime-status`.

It runs *alongside* the default arm rather than replacing it: the two differ in what they
eliminate, so neither covers the other. Wall-clock cost is recorded below once measured.

