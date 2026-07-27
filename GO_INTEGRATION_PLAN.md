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

- **2a. Per-convention descriptor** keyed by `ir.CallConvention`
  (`arm64/convention.go`). **DONE for the value-based ABI-axis properties**:
  `intArgRegs`/`floatArgRegs` (8/8 vs 16/16, via `newArgAssigner`),
  `savesCalleeRegs` (via `calleeSavedFor`), `stackLinkBytes` (collapsed four
  `if goInternal { += goStackLinkSize }` branches), `packsStackArgs`. Two listed
  fields do **not** fit a `CallConvention` value table and stay as they are:
  - `clobberSet` is **union-axis** (`UsesManagedFrame() || UsesGoInternalCallConvention()`,
    `callersave.go:178`), not pure ABI — it depends on the frame axis too, so it
    is not keyed by `CallConvention` alone.
  - `aggLowering` is **control flow** (distinct param/result lowering functions
    per convention in `lower.go`/`goabi.go`), not a value; tableizing it means
    strategy function pointers — deferred as a larger, separate change.
  The frame axis (morestack, outgoing area, GC marking) correctly stays on
  `UsesManagedFrame` — `CallConvention`'s own doc says it does not describe stack
  ownership. Three legacy `GoABI`-bool sites (`mc.go` `goRegisterPointerMask`
  [dead code], `framelessEligible` [redundant with the `PrologueEmitter` check])
  are non-issues left in place. Per-call `CallConv` stamping and the
  cross-convention tail-call error are unchanged.
- **2b. Split the emit driver. Core DONE.** `compileToObjectWithBundle` was both
  the neutral object driver and the Go metadata orchestrator, threaded together.
  Now:
  - `prepareGoABI`/`inferStackPointerWords` run as a pre-loop IR-annotation pass
    (proven order-independent of the emit loop), out of the per-function body.
  - The per-function loop is neutral but for one `goFunctionInfoFor` call that
    gathers the Go facts (`goFunctionInfo` was already the loop→finisher boundary).
  - The after-loop orchestration is split into `finishGoModule` (Go finisher:
    runtime text symbol, BSS/fixup data layout, pclntab/moduledata) and
    `addNeutralData`, dispatched once instead of `goRuntime` branches scattered
    through the data loop.
  Behavior-preserving (facts struct and call sequence unchanged). **Remaining
  refinements (optional):** a *frontend-registered* finisher instead of the single
  `if goRuntime` dispatch (arguably over-engineering for two frontends); moving
  `goStackPrologue` behind the `PrologueEmitter`/`GCStrategy` hook (delicate — it
  currently runs *before* the generic prologue hook, needs a Go `GCStrategy`);
  and relocating `inferStackPointerWords` from `arm64` into the `ir`/`opt` package
  (it is already a pure IR-to-IR pass, just not yet in a shared package).
- **2c. Replace symbol-name sniffing with IR attributes. Largely DONE.**
  - `ir.Module.Runtime` flag replaced `moduleUsesGoRuntime` (the
    `runtime.schedinit` sniff), serialized in the unit format (v14→v15).
  - `ir.Module.SymAttrs` (`SymAtomicPointerStore`, `SymFrameScoped`), populated
    by goc, replaced the hardcoded Go name lists in `opt/escape.go` (write
    barrier) and `opt/inline.go` (defer). The passes test the attribute via
    `Func.Module().SymAttrOf`.
  - **Left as-is on purpose:** the benign memory-intrinsic list
    (`memcpy`/`memset`/`memcmp`) in `escape.go` — those are standard C names
    legitimately recognized, shared by both frontends, not Go-specific sniffing.
  - **Remaining:** any name lists in `alias.go`/`dse.go` (caller-frame calls),
    and intrinsic flags for `getcallerpc`/`getcallersp` — audit and migrate if
    they are Go-specific.
- **2d. Formalize frontend-owned pipeline extension. RECONSIDERED — mostly moot.**
  The shared `DefaultPipeline` (`opt/pass.go`) contains **no** Go-specific passes;
  goc's Go passes (`InlineNoSplitCalls`, `InlineHeapAllocations`,
  `LowerHeapAllocations`, `AuditNoSplitCalls`) already run inside `goc.Compile` as
  mandatory frontend *lowering* (the non-optimized `CompileExecutable` path needs
  them), not as optional optimization. So there is nothing to extract from the
  shared default, and moving those passes to an optional `OptimizeModuleWith`
  post-pass would break unoptimized builds. A `PrePasses`/`PostPasses` seam could
  still be added as pure infrastructure, but it would have no current users — low
  value. Deferred unless a real frontend post-pass need appears.

## Priority 3 — Core IR hygiene

- **3a. Extract `Module.Assembly` serialization from the core unit format. DONE.**
  The ~200 lines of Plan 9 asm / ABI0 serialization moved out of
  `MarshalBinary`/`DecodeModule` into a standalone `EncodeAssembly`/`DecodeAssembly`
  codec (`ir/asm_binary.go`). `ir.Module` gained a generic
  `Attachments map[string][]byte` frontend-attachment section; the core format now
  serializes that (sorted keys, opaque length-prefixed bytes) and assembly rides
  under a reserved key, so the unit format is frontend-agnostic and extensible.
  The typed `Module.Assembly` field stays as the in-memory representation (goc
  producer, arm64 consumer, and all tests unchanged) — it is bridged to the
  attachment only at the serialization boundary. `binary_test` round-trips both a
  populated `Assembly` and a raw `Attachments` entry. Unit format v17→v18.
  (`AssemblyFile` itself stays in `ir` because goc and arm64 are peers that both
  use it; a fully goc-owned artifact would need a new shared package.)
- **3b. Delete `Func.GoABI`; rename `CallConvAAPCS64` → `CallConvPlatform`. DONE.**
  `GoABI` was the legacy dual-meaning bridge (it forced both `GoInternal` and a
  managed frame); no production code set it — only tests. Deleted the field, the
  `|| f.GoABI` fallback in `UsesGoInternalCallConvention`/`UsesManagedFrame`, and
  its serialization (unit format v15→v16); migrated the 28 test setters to
  `CallConv = CallConvGoInternal` + `ManagedFrame = true`, and the two live
  readers (`goRegisterPointerMask` [dead], `framelessEligible`) to the accessors.
  Renamed `CallConvAAPCS64` → `CallConvPlatform` since the default is the target's
  native C ABI (AAPCS64 on arm64, System V on amd64), not AAPCS specifically.
  **Remaining:** move `SystemStack` (`//go:systemstack`) to a frontend attribute
  table — more involved, as the backend reads it for the morestack prologue;
  deferred.
- **3c. De-Go doc comments/names. Partly done:** renamed `flattenGoAggregate` →
  `flattenAggregate` (it lives in `goabi.go`, so the "Go" only repeated the file
  context). Remaining (cosmetic doc edits): `ManagedFrame`, `ClosureContext` (static
  chain / `nest`), `StackResult`, `Field.Pointer`; rename `flattenGoAggregate` →
  `flattenAggregate`.
- **3d. Kill the `Name == "closure"` magic-temp match. DONE.** Added an explicit
  `ir.Temp.ClosureContext` flag (serialized, unit format v16→v17), set by goc on
  the closure-environment temporary. `stabilizeClosureContext` (`arm64/lower.go`)
  and the inliner (`opt/inline.go`) now test the flag instead of matching the
  magic temporary name `"closure"` plus `Fixed`/`Reg==X26`. Enforced by
  `binary_total_test.go`'s field-completeness check. **Remaining (cosmetic):**
  move `stabilizeClosureContext` from `lower.go` to `goabi.go`.

## Priority 4 — Bugs & cosmetics

- **4a. Serialize `Func.StackPointerWords`. MOOT.** After 2b hoisted
  `prepareGoABI`/`inferStackPointerWords` to recompute this map in the backend on
  every compile (over frontend IR, independent of emission), a cache-loaded
  module's nil map is fully recomputed before use — so there is nothing to
  serialize and no cache-correctness bug.
- **4b. `floatingComparison`. NOT A BUG (analyzed).** goc's comparison emission is
  self-consistent: `compile.go:~13160` and the `min`/`max` builtin pick the float
  or integer predicate from the operand class, so a generic body compiled with an
  integer *representative* type has an integer operand AND an integer predicate.
  The float-operand/integer-predicate mismatch is created only *transiently* by
  the **inliner** when it substitutes a float argument into that shared body, and
  `opt/inline.go` fixes it in the same pass via `floatingComparison` (correct
  specialization, not a bug-patch); `opt/fold.go` is a safety net. `ir.Verify`
  runs inside the `clean` fixpoint *before* inlining, so it never sees the
  transient form, and the passing float-generics tests show nothing malformed
  reaches the backend. There is no frontend bug to fix and no escaping form for
  the verifier to reject — the shared-body + inline-specialization design is
  correct. (A true fix would be monomorphization, a large change with no bug to
  justify it.)
- **4c. `opt/deadfunc.go` `linkerSymbol` → canonical mangler. DONE.** `linkerSymbol`
  (opt) and `sanitize` (arm64) were byte-identical manglings. Added
  `ir.LinkerSymbol` as the single canonical mangler (in `ir`, the package both
  peers import — opt and arm64 do not import each other); both now delegate to it,
  so the spelling cannot drift.
- **4d. Cosmetics:** `msr dit` string carve-out → `MsrPstate` field table; LSE
  acquire-release-only note; trim debug state in `callersave.go`/`select.go` error
  strings; print/parse the new `Func` flags in the text IR.

## Sequencing & validation gate

Do **P1 first** — pure wins that restore C/Ruby codegen, directly measurable: the
C/Ruby path must stay **byte-identical to gcc** on the miniruby differential while
the `goc` corpus keeps passing. Within P1, land `ClsM` (1a) first (1b–1d key on
it); 1e is independent. **P2** is the structural payoff. **P3–P4** land
incrementally.
