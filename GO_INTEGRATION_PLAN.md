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
  `ClsM`.
- **1b. GVN/GCM: `Cls == ClsP` → `Cls == ClsM`** (`opt/gvn.go:34`,
  `opt/gcm.go:118`). Raw C/Ruby pointers become CSE-able and movable again.
- **1c. Regalloc safepoint roots** (`arm64/regalloc.go:138`): drop the
  `(UsesManagedFrame || GoInternal) && Cls == ClsP` proxy — roots are exactly the
  managed values, per-value.
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
