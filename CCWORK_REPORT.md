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

See the commit on this branch. Verification results are appended below as they land.
