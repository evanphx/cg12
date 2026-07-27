# PLAN: Integrate the Go frontend cleanly into cg12

The `goc` Go frontend and its runtime/ABI support were merged into cg12. In the
process, Go-specific logic leaked into shared, frontend-agnostic code. This plan
makes the core stay frontend-agnostic while the Go work stays cleanly
Go-specific — either by *generalizing* a Go-framed mechanism that is genuinely
useful to any frontend, or by *extracting* Go-only logic behind a seam.

## The organizing idea: two axes

Almost every "how do we de-Go this?" question resolves to which of two axes the
concept lives on:

| axis | mechanism | governs |
| --- | --- | --- |
| **value** | `ClsM` (a managed-pointer class; replaces the `GCRef` flag) | GC roots, write barriers, GVN/GCM movability, stack-map pointer-ness |
| **function / ABI** | `CallConvention` descriptor + `ManagedFrame` | prologue/morestack, frame layout, register reservation, callee-saved set |

Managed-ness is a property of a *value*, not a whole function; ABI/frame
behavior is a property of a *function*. Keeping the two separate dissolves most
of the leakage, which came from using one as a proxy for the other (e.g.
`Cls == ClsP` as a stand-in for "managed", or per-function flags gating
value-level decisions).

`GCRef` (per-temp managed-reference flag) already existed pre-Go (Ruby era). The
regressions below came from the Go work reaching for the blunt `Cls == ClsP`
instead of that per-value signal. `ClsM` promotes the flag to a first-class type
so managed-ness rides the class through cloning/serialization/printing/verifying
and can never silently desync or drop.

---

## Priority 1 — Fix the C/Ruby regressions (they *restore* existing users)

- **1a. Add `ClsM`, the managed-pointer class (value axis; replaces `GCRef`).**
  Sibling of the abstract `ClsP`, register/width-identical to it. `LowerPointers`
  resolves `ClsM` → concrete register class and sets `GCRef` (mirrors `ClsP →
  ClsL`), so the backend is unchanged. Frontends emit `ClsM` for managed refs.
  Audit the ~329 `== ClsP` sites: register/width uses move to an "any pointer"
  predicate (`IsPtr()` over `ClsP`+`ClsM`); only the ~4 GC-semantic sites test
  `ClsM`. **DONE for the class + backend + 1b/1c/1d/1e. The frontend migration
  (goc emitting `ClsM`) is DEFERRED — see 1a′ below.**
- **1a′. Migrate goc onto `ClsM` (value axis). DEFERRED — not safely incremental.**
  Two attempts, both reverted after validation:
  1. *Choke point* (`MarkGCRef` sets `Cls = ClsM`): unsafe. `MarkGCRef` has only
     the value's `Ref`, so it retypes the temporary but not its defining
     instruction, and optimizer passes assume `temp.Cls == defInstr.Cls`. The
     divergence miscompiles — demonstrated: a C computed-goto interpreter whose
     pointer parameter constant-folds to `0` (`loadub 0`).
  2. *Gated loads only* (`Load` emits `ClsM` when `HasCopyingStack()`,
     divergence-free): still unsafe. It broke ~20 goc GC tests, including
     non-optimized `CompileExecutable` cases (`fault`/crash). goc's collector
     machinery is tightly coupled to the current representation: e.g. the `Load`
     gate also caught the `OGetReg` register-read path, so special runtime
     pointers (`g`, `sp`) became relocatable roots and corrupted the collector.
  Findings that constrain any future attempt: `ir/build.go`'s `Load` already
  auto-marks every `Load(ClsP,…)` as `GCRef` (shared by C and goc, so the value
  axis is *already* largely class-coupled); it fixes no current bug (the inliner
  and binary serialization already preserve `GCRef`); and the safe path is a
  large, goc-specific, per-creation-site emission of `ClsM` that must not touch
  `OAlloc` addresses (remat regression) or widen the root set — a dedicated
  effort with the GCRef-set harness and full goc GC validation, not an
  incremental change.
- **1b. GVN/GCM: `Cls == ClsP` → `Cls == ClsM`** (`opt/gvn.go:34`,
  `opt/gcm.go:118`). Raw C/Ruby pointers become CSE-able and movable again.
- **1c. Regalloc safepoint roots — drop the `(managed-frame && ClsP)` proxy. DONE.**
  Roots are now identified purely per value via `GCRef`; the register allocator
  has no calling-convention knowledge. Evidence that this is safe: the proxy's
  `ClsP` clause was instrumented and fires **0 times** across the entire test
  suite (arm64, opt, ir, and 360s of goc) — every managed pointer live at a
  safepoint is already `GCRef`-marked by goc's frontend (heap refs, aggregate
  pointer-words, pins). Two dead ends were ruled out along the way:
  - *Widening `ClsM` to "every pointer"* (a `TrackAllPointers` pass marking all
    pointers `GCRef`) is a **pessimization**: `remat.go` excludes `GCRef` temps
    from rematerialization, so a frame address that was a cheap `add x17, x29`
    after a call becomes a spill/reload (`TestGoABIRematerializes…` regress). The
    proxy avoided this by computing roots from *liveness* without setting `GCRef`,
    which is why a rematerialized frame address — recomputed from the
    runtime-updated frame pointer after stack growth — is correctly *not* a root.
  - The proxy caught *backend*-created `ClsP` temps (from arm64 `lower()`, which
    runs after `ir.LowerPointers`), so a frontend-side `ClsM` migration would not
    have reached them regardless. Since none are ever live-and-unmarked at a
    safepoint, dropping the proxy is a no-op for real code and a strict no-op for
    C/Ruby (the predicate was already false there).
- **1d. Aggregate-buffer GC marking** (`arm64/lower.go:933,1059`; `abi.go`
  `lowerAggResult`): mark `MarkGCRef` only when the buffer holds managed data, not
  on every AAPCS aggregate param/result.
- **1e. Per-function allocation order** (`arm64/reg.go:137`): stop dropping
  X26/X27/X28 from `intAllocOrder` for everyone; exclude them only under the Go
  convention (function axis — clean home is 2a). Document X28 = `g`.

## Priority 2 — Structural seams (collapse the `if go` branching)

- **2a. Per-convention descriptor** keyed by `ir.CallConvention`:
  `{intRegs, floatRegs, calleeSaved, clobberSet, stackLinkBytes, aggLowering}`.
  Collapses the `goABI`-bool ladders in `lower.go`, `abi.go`, `frame.go`,
  `callersave.go`, `reg.go` into property lookups. Keep per-call `CallConv`
  stamping and the cross-convention tail-call error.
- **2b. Split the emit driver.** `arm64/mc.go` `compileToObjectWithBundle` is both
  the neutral object driver and the Go metadata orchestrator. The function loop
  records a neutral facts struct; a frontend-registered `moduleFinisher` (Go's in
  `go_metadata_object.go`) consumes it. Move `goStackPrologue` behind the existing
  `PrologueEmitter`/`GCStrategy` hook; move `prepareGoABI`/`inferStackPointerWords`
  to an IR pass.
- **2c. Replace symbol-name sniffing with IR attributes.** `ir.Module.Runtime`
  flag kills `moduleUsesGoRuntime` (`runtime.schedinit` sniff); call-site/function
  attributes for defer/frame-scoped, benign-memory, caller-frame calls kill the
  name lists in `opt/inline.go`, `escape.go`, `alias.go`, `dse.go`; intrinsic
  flags for `getcallerpc/sp`.
- **2d. Formalize frontend-owned pipeline extension:** `opt.OptimizeModuleWith(m,
  opts)` with `PrePasses`/`PostPasses`; move `escape`, `nosplit`,
  `InlineNoSplitCalls`, `InlineHeapAllocations` there.

## Priority 3 — Core IR hygiene

- **3a. Extract `Module.Assembly []AssemblyFile`** from the core IR + binary
  format (~200 lines of `ir/binary.go` serialize Plan 9 asm source + Go ABI0 into
  the frontend-agnostic unit format). Move to a `goc`-owned artifact or an opaque
  "frontend attachment" section (`map[string][]byte`).
- **3b. Delete `Func.GoABI`** (migrate `goc` to `CallConv`+`ManagedFrame`);
  rename `CallConvAAPCS64` → `CallConvPlatform`; move `SystemStack` to a frontend
  attribute table.
- **3c. De-Go doc comments/names**: `ManagedFrame`, `ClosureContext` (static
  chain / `nest`), `StackResult`, `Field.Pointer`; rename `flattenGoAggregate` →
  `flattenAggregate`.
- **3d. Kill the `Name == "closure"` magic-temp match** (`lower.go`
  `stabilizeClosureContext`, `inline.go`) → explicit IR designation; move
  `stabilizeClosureContext` to `goabi.go`.

## Priority 4 — Bugs & cosmetics

- **4a. Serialize `Func.StackPointerWords`** in `ir/binary.go` (cache-correctness;
  the per-alloca pointer-word map, distinct from scalar `ClsM`).
- **4b. `floatingComparison`**: `goc` generics emit int-cmp predicates on float
  operands, patched downstream in the shared inliner/folder. Fix the frontend and
  have the verifier reject the malformed form.
- **4c. `opt/deadfunc.go` `linkerSymbol`** bakes one backend's name mangling into a
  shared pass → single canonical mangler exported by the backend.
- **4d. Cosmetics:** `msr dit` string carve-out → `MsrPstate` field table; LSE
  acquire-release-only note; trim debug state in `callersave.go`/`select.go` error
  strings; print/parse the new `Func` flags in the text IR.

## Sequencing & validation gate

Do **P1 first** — pure wins that restore C/Ruby codegen, directly measurable: the
C/Ruby path must stay **byte-identical to gcc** on the miniruby differential while
the `goc` corpus keeps passing. Within P1, land `ClsM` (1a) first (1b–1d key on
it); 1e is independent. **P2** is the structural payoff. **P3–P4** land
incrementally.
