# PLAN: Semantic lowering of Plan 9 assembly into cg12 inline assembly

Turn `plan9asm` from a syntax pass-through into a semantic layer: parse Plan 9
assembly into an abstract representation of *what a routine needs*, then lower
that into cg12's own inline-assembly mechanism under cg12's own calling
convention. Unchanged Go `.s` files keep compiling; cg12's ABI stops being bound
to Go's at those boundaries.

## 1. Problem statement and target data flow

### Today

1. `goc/compile.go:157` collects the stdlib's `.s` sources into
   `ir.Module.Assembly` (`ir.AssemblyFile`, `ir/func.go:227`). A second,
   throwaway parse in `sourceAssemblyReferences` (`goc/compile.go:376`) exists
   only to seed reachability.
2. At backend time `arm64/assembly.go:prepareAssembly` calls
   `plan9asm.CompileARM64` (`plan9asm/arm64_compile.go:125`), which **textually
   translates** Plan 9 syntax to GNU AArch64 text (`plan9asm/arm64.go` and the
   other `arm64_*.go` files, ~4,500 lines) and returns symbol/frame metadata
   (`ARM64Function`).
3. That GNU text is written to a `.S` file and assembled by the **system C
   compiler** (`cmd/goc/main.go:187 compileRuntimeSupport`), then linked next to
   the cg12 object.
4. Because goc-compiled Go uses ABIInternal (`f.GoABI = true`,
   `goc/compile.go:323`) while the `.s` bodies speak ABI0, two adapter layers
   exist:
   - **asm side** — `plan9asm/arm64_abi0.go`: `collectABI0Layouts` infers
     argument/result stack slots from `name+off(FP)`; `collectDirectABI0` /
     `emitDirectABI0Load` rewrite FP loads into ABIInternal register moves
     (`R{index}`); `emitABI0Wrapper` emits register→stack shims. Go's ABIInternal
     register assignment, hard-coded inside the translator (literally
     `"R"+strconv.Itoa(index)` at `arm64_abi0.go:98`).
   - **IR side** — `arm64/go_abi0_assembly.go:emitGoABI0AssemblyWrappers`: emits
     `name_abi0` text shims so assembly can call IR functions, using the full
     ABIInternal assigner from `arm64/goabi.go`.
5. Frame/pcln metadata for the sidecar functions flows through
   `bundle.functions` into `arm64/go_metadata_object.go`.

### Target

```
Plan 9 .s text
  → plan9asm.Parse            (unchanged; unchanged .s files keep compiling)
  → plan9asm/sem              (NEW: abstract semantic unit — operations, operands,
                               pseudo-register intent, signatures, directives; no
                               Plan 9 spellings, no ABI register numbers)
  → arm64/asmlower            (NEW: lowering to cg12's own forms)
       ├─ "bindable" funcs  → ir.Func{ params, OAsm body }   — bound by cg12's own
       │                       call lowering, whatever convention the Func carries
       ├─ "raw" funcs       → machine functions assembled by a64.AssembleProgram
       │                       → merged into the cg12 object (programObject path,
       │                       arm64/backend.go:78)
       └─ GLOBL/DATA        → ir.Data
  → one relocatable object; no GNU text sidecar, no external assembler,
    no ABI0 wrapper layer in either direction.
```

The decoupling is precisely this: an argument is expressed as *"operand bound to
parameter N of this function"* (an `AsmOp` operand, `ir/instr.go:121`) instead of
*"8(FP), which under ABIInternal means x1"*. The backend's existing call lowering
(`arm64/goabi.go` today, AAPCS64 or anything else tomorrow) decides which register
that is, on both sides of the boundary, so `plan9asm` no longer contains any
calling-convention knowledge.

## 2. The abstract representation (`plan9asm/sem`)

A new package between the existing AST (`plan9asm/ast.go`) and the lowering. It
consumes `*plan9asm.File` plus the same `Defines` the translator resolves today
(`plan9asm/arm64_operands.go:resolveIntegerExpression`) and produces a fully
resolved, machine-level but ABI-abstract unit.

### Core types (concrete sketch)

```go
package sem

type Unit struct {
    Funcs      []*Func
    Datas      []*Data            // merged GLOBL+DATA (from recordData logic)
    References []SymRef           // every external symbol touched (replaces
                                  // ARM64Translation.ExternalReferences)
}

type ABIKind uint8                // as *declared* in the source, carried as fact,
const (                           // not acted on: ABIDefault (no selector),
    ABIDefault ABIKind = iota     // ABIInternal (<ABIInternal>), ABI0 (<ABI0>)
    ABIInternal
    ABI0
)

type Func struct {
    Sym       string      // resolved, package-qualified (·name expansion; today's
                          // arm64Translator.symbol, arm64.go:1181)
    Static    bool        // <> file-local
    DeclABI   ABIKind
    Flags     Flags       // NOSPLIT | NOFRAME | TOPFRAME | WRAPPER | DUPOK ...
    FrameSize int         // TEXT $frame
    ArgsSize  int         // TEXT $frame-args
    Kind      FuncKind    // Bindable | Raw (classification, §2.3)
    Sig       Signature   // recovered ABI intent (Bindable only)
    Body      []Stmt      // Label | Inst | Align (PCALIGN)
}

// Signature is what the body *needs* from its caller, keyed by ABI0 slot
// offset purely as an identity (never re-emitted as a stack offset).
type Signature struct {
    Params  []Slot
    Results []Slot
}
type Slot struct {
    Name    string // "ptr" from ptr+0(FP)
    Offset  int    // ABI0 offset — identity/matching key only
    Width   int    // widest access seen
    Float   bool
    Group   int    // slice/string grouping (today's "_base/_len/_cap" rule,
                   // plan9asm/arm64_abi0.go:178)
}

type Inst struct {
    Op   MOp     // normalized machine op: mnemonic family + width + addressing
                 // mode (the .P/.W suffixes), already validated
    Args []Opnd
    Pos  Position
}
```

`Opnd` is a closed sum. This is where the pseudo-registers are *dissolved into
intent*:

```go
type Reg struct{ R MReg }            // real machine register. MReg includes
                                     // G (the g register), LR, FP29, RSP, ZR,
                                     // NZCV, FPSR, V0..V31 with arrangement.
type Imm struct{ V int64 }           // fully resolved (defines/expressions folded)
type Mem struct {                    // real-register memory reference
    Base MReg; Off int64; Idx MReg; Mode AddrMode // offset/post/pre/register
}
type ArgSlot struct {                // was  name+off(FP)  as a load source
    Slot int                         // index into Sig.Params
    Width int; Signed bool
}
type ResSlot struct{ Slot int; Width int }   // was  ret+off(FP)  as a store target
type ArgAddr struct{ Slot int }              // was  $name+off(FP)
type Local struct{ Off int64; Width int }    // was  off(SP)  (pseudo-SP frame slot)
type LocalAddr struct{ Off int64 }           // was  $off(SP)
type SymMem struct{ Sym SymRef; Off int64 }  // was  sym+off(SB) load/store
type SymAddr struct{ Sym SymRef; Off int64 } // was  $sym(SB)
type LabelRef struct{ Name string }          // local branch target
type FuncRef struct{ Sym SymRef }            // B/BL sym(SB): call or tail-alias
// RegList, VecReg, VecList, Shifted, Extended: structural forms kept as-is
```

Notes:

- **Everything Plan-9-specific is gone**: pseudo-FP became
  `ArgSlot`/`ResSlot`/`ArgAddr`; pseudo-SP became `Local`; SB became
  `SymMem`/`SymAddr`; `g` is just an `MReg` (its *meaning* — "the current
  goroutine" — is a lowering contract, not a representation problem).
- **Expressions are resolved** at build time using the same `Defines` map
  (`go_asm.h` structure offsets from `goc/compile.go:assemblyPackageDefines`), so
  `(p_xRegs+xRegPerP_scratch)` in `preempt_arm64.s` is a plain `Imm`/`Mem.Off` by
  the time lowering sees it.
- **`MOp` is a normalized machine-op enum** (LDAR{B,W,X}, STLXR, CASAL, SWPAL,
  LD1/ST1 with arrangement, SHA256H, DC ZVA, MRS/MSR, SVC, DMB with a decoded
  barrier domain, WORD as `RawWord{uint32}`, etc.). Building this enum is mostly
  a transliteration of the dispatch switch in `plan9asm/arm64.go:88-255` — that
  switch is the complete inventory of what the supported corpus uses.
- **Directives**: `PCALIGN` → `Align{N}` statement; `FUNCDATA`/`PCDATA` are
  recorded but produce nothing (as today, `arm64_directives.go:143`);
  `GLOBL`/`DATA` fold into `sem.Data{Sym, Size, RO, NoPtr, Items []DataItem}`
  where `DataItem` mirrors `ir.DataItem` (value / bytes / `Sym` relocation — the
  three cases of `recordData`, `arm64_directives.go:263`).

### 2.3 The Bindable/Raw classification

This is the load-bearing semantic judgement, generalizing today's
`collectDirectABI0`:

- **Bindable**: the function's only interaction with its caller is through
  `ArgSlot`/`ResSlot`/`ArgAddr` operands, `RET` (to LR), and tail-aliases
  `B ·other(SB)`; it never addresses the caller's frame through raw `RSP` beyond
  its own declared locals and never writes SP non-locally. All of
  `internal/runtime/atomic`, the five `bytealg` files, `memmove`/`memclr`,
  `chacha8`, md5/sha/crc, `internal/cpu`, `syscall.Syscall6`, most of
  `sys_linux_arm64.s` (e.g. `runtime·exit` is `MOVW code+0(FP), R0;
  MOVD $SYS_exit_group, R8; SVC; RET`) fall here.
- **Raw**: the function *is* machine state manipulation — it switches stacks,
  returns to a caller-of-caller, or is entered by the kernel or by injected
  preemption: `gogo`, `mcall`, `systemstack`, `morestack`, `asyncPreempt`,
  `sigtramp`, `rt0_linux_arm64.s`, `tls_arm64.s`, `_cgo_sys_thread_start`-style
  clone trampolines in `sys_linux_arm64.s` (the `BL (R2)` sequences after
  `SVC clone`), `DC ZVA` setup reading `block_size<>`. These have no signature to
  re-express; their contract is register-level and must be preserved verbatim.
- A function declared `<ABIInternal>` with a register-commented body
  (`chacha8`'s `TEXT ·block<ABIInternal>(SB)`, "seed in R0") is **Bindable with
  declared-register slots**: the sem builder maps the ABIInternal *declared*
  assignment (from the Go signature, §3.1) to slots, and every occurrence of
  those registers in the body becomes `ArgSlot`-as-register (the operand list
  marks R0 as "parameter 0's home"). Lowering then *renames* those registers to
  allocator-chosen operand registers — this is what makes even
  ABIInternal-annotated source convention-independent.

Classification is per-function and total: anything the builder cannot prove
Bindable is Raw, and anything Raw containing an unsupported construct is a loud
error (matching the fail-loudly stance in `cc/asm.go` and the translator).

## 3. Lowering to cg12 (`arm64/asmlower.go`)

### 3.1 Signatures: where ABI intent really comes from

Two sources, cross-checked:

1. **Go declarations.** goc already collects bodyless `func` declarations
   (`functionDecls`, `goc/compile.go:106`). For each assembly symbol with a Go
   declaration, goc computes the ir-level signature (classes, `GCRef` pointer
   flags, `ValueGroup`s for strings/slices — `ir/func.go:45` and goc's existing
   param building) *and* the ABI0 stack offsets of each scalar (via
   `types.SizesFor`, which `assemblyPackageDefines` already uses). Add a field to
   `ir.AssemblyFile`:
   ```go
   // ir/func.go
   type AsmSignature struct {
       Params, Results []AsmSlot // AsmSlot{Offset int; Cls Cls; GCRef bool; Group int}
   }
   type AssemblyFile struct { /* ... */; Sigs map[string]AsmSignature }
   ```
2. **FP inference** (today's `collectABI0Layouts`) survives as a *validator and
   fallback*: the sem `Signature` offsets must be a subset of the declared
   layout; mismatch is a compile error naming the slot. Symbols with no Go
   declaration (file-static helpers) keep the inferred layout.

This replaces the fragile width/`_base`-suffix heuristics with the true types,
which also fixes pointer identification for GC (`GCRef`), something the current
wrapper path can only approximate.

### 3.2 Bindable functions → `ir.Func` + `OAsm`

For each Bindable `sem.Func`, build an `ir.Func`:

- `Name` = resolved symbol; `Linkage.Export` as needed; `NoSplit` = NOSPLIT flag;
  `GoABI` = whatever the module's functions use (today `true` for the runtime —
  the point is that this is now *one flag set by goc*, not knowledge inside
  plan9asm).
- **Params/results** from the `AsmSignature` (real `ir.Temp` params,
  `ParamGroups` for slices). Multiple results use the existing `RetValues`
  machinery.
- **Body** = a small prologue + one `OAsm` (built with `Block.Asm`,
  `ir/build.go:467`) + a return:
  - Each `Sig.Params` slot referenced by the body → one `AsmRegIn` operand whose
    `Ref` is the parameter temp. Each `ResSlot` → `AsmRegOut`. `ArgAddr` (address
    of an argument — only legal for stack-homed data; today's direct path refuses
    it, `arm64.go:378`) → materialize the value into an `OAlloc` slot and pass the
    slot's address as `AsmRegIn`.
  - `SymAddr`/`SymMem` → an `AsmRegIn` operand whose `Ref` is a `ConstSym`
    (`ir.Const{Kind: ConstSym}`). `mc.asmInputReg` already materializes these via
    `materializeSym` with proper `ADR_PREL_PG_HI21`/`ADD_ABS_LO12_NC` relocations
    (`arm64/mc.go:2318`, `1235`) — **so symbol addressing exits the template
    entirely and no new relocation plumbing is needed on the inline path**.
    `SymMem` loads become "materialize address operand, then `ldr` through it" in
    the template.
  - `Local` slots (Plan 9 `$framesize` locals) → one `OAlloc16` of `FrameSize`
    bytes; its address is another `AsmRegIn` operand `%L`, and `off(SP)` in the
    body is rewritten to `[%L, #off]`. This turns the Plan 9 frame into ordinary
    cg12 stack allocation, coexisting correctly with cg12's own frame layout
    (`arm64/frame.go`) and the Go stack-growth prologue.
  - **Template text**: regenerate GNU mnemonics from the `sem.Inst` stream
    (reusing the mnemonic tables that already exist in the translator), with
    `%N`/`%wN`/`%xN` placeholders where slot operands appear
    (`arm64/asm.go:expandAsm` already supports these, plus `%s/%d`; extend with a
    `%qN`/vector form). Labels stay in-template, uniquified per function.
  - **Clobbers** = every raw register the body writes (computable exactly from the
    sem operands) — `in.Asm.Clobbers`, which the allocator honours
    (`arm64/asm.go:asmClobberRegs`, `regalloc`).
  - **Tail aliases** (`B ·other(SB)` as the entire body — dozens in
    `atomic_arm64.s`): lower to an ir tail call (`Instr.Tail`, `ir/instr.go:63`),
    not asm at all. The backend already guarantees frame-reusing tail calls. This
    alone removes the alias-propagation special case in `collectABI0Layouts`
    (`arm64_abi0.go:195`).
  - **Calls inside a bindable body** (`BL ·secretEraseRegisters(SB)` in
    `sys_linux`): where they occur in bindable bodies, split the body at the call
    into `OAsm` / `OCall` / `OAsm` segments, with values that cross the split
    carried in temporaries. Only legal when no label crosses a segment boundary —
    verified by the sem layer; otherwise the function is reclassified Raw.
- **`Volatile`**: not needed; `OAsm` is already an unknown memory effect for
  `opt/loadelim`/`opt/dce` (documented at `cc/asm.go:84`).

Because the function is now an ordinary `ir.Func`, *all* ABI work happens in the
backend's one place: `compileToObjectWithBundle` → `prepareGoABI`/`lower`/
`regAlloc` (`arm64/mc.go:119`), morestack prologue and NoSplit handling included
(`mc.prologue`, `mc.go:823`). Callers need nothing special: the call is an
intra-module `OCall`. **Both ABI0 adapter layers become dead.**

**Required `emitAsm` extensions** (`arm64/mc.go:2229`):
1. Assemble templates with `a64.AssembleProgram` + `Link` (today it calls
   `a64.Assemble`, which errors on any undefined label) and re-anchor the returned
   `a64.Reloc`s by the emission base into `m.relocs` — needed the moment a
   template keeps a `bl`/`b` to an external symbol.
2. Scratch discipline: `emitAsm` materializes spilled/const operands into
   `x16/x17/x15` (`intScratchRegs`). Bodies that *use* x15/x16/x17 (bytealg does,
   preempt does) must not have operands routed through them — pick scratch from
   outside the template's clobber set, and fail loudly if impossible.
3. Vector operand widths (`%q`, arrangement-suffixed substitution) for the SIMD
   files.

### 3.3 Raw functions → assembled machine functions

Raw functions are emitted straight through the in-tree assembler at object-build
time, inside `compileToObjectWithBundle`:

- Lower `sem.Inst` directly to `a64.Program` calls / encoder words (going through
  the extended `a64` text assembler is an acceptable first implementation since
  disassembly-based tests exist).
- `Program.Link` returns `CALL26/JUMP26` relocations
  (`arm64/backend.go:programObject`, `aarch64RelType`); extend `a64.RelKind` with
  `RelAdrPage`/`RelAddLo12`/`RelLdst64Lo12` for `SymAddr`/`SymMem` inside Raw
  bodies (`sys_linux`'s vDSO symbol loads, `memclr`'s `block_size<>`), and map
  them in `aarch64RelType`.
- Append code to `o.Text` with a per-function `goFunctionInfo` (name, `frameSize`,
  `frameStart`, argsize, funcID/funcFlag) so pclntab/pcsp metadata is unchanged
  (`go_metadata_object.go`). Frame metadata comes from the sem layer's SP-delta
  tracking where the pattern is simple (the standard `sub sp/str x30` prologue,
  `arm64_directives.go:69`), with the existing explicit override table for the
  irregular ones (asyncPreempt's 240-byte hand-built frame,
  `arm64_directives.go:106`, and the funcID/flag tables in
  `arm64/assembly.go:94`).
- The `g` register (x28) and x26/x27 are already outside the allocator's pool
  (`arm64/reg.go:129`), so Raw code touching them is safe by construction; TLS
  (`tls_arm64.s`) uses `MRS TPIDR_EL0`, for which the encoder already exists
  (`a64.MrsTPIDR`, `a64/a64.go:83`).

This removes the external `cc` assembly step: `cmd/goc/main.go:link` links cg12
objects only, and `compileRuntimeSupport` disappears.

### 3.4 Data

`sem.Data` → `ir.Data` (`ir/func.go:236`): numeric items, byte strings, and `Sym`
relocation items map 1:1 onto `ir.DataItem`; `RODATA` → `Linkage.Section =
".rodata"` (the backend already routes address-holding rodata to `.data.rel.ro`,
`ir.Data.HoldsAddress`); `NOPTR` → no `PointerWords`. The `bundle.definitions`
dedup hack in `compileToObjectWithBundle` (`mc.go:200`) becomes unnecessary.

## 4. What Go-ABI coupling goes away, and what remains

**Goes away entirely:**
- `plan9asm/arm64_abi0.go` — layout inference used for codegen, direct-ABI0
  register rewriting (the literal `R+strconv.Itoa(index)` ABIInternal assumption
  at line 98), and `emitABI0Wrapper`. plan9asm retains *zero* register-assignment
  knowledge.
- `arm64/go_abi0_assembly.go` — the IR→ABI0 wrapper generator and its `_abi0`
  symbol scheme, plus `ABI0References`/`abi0Symbols` bookkeeping and `.hidden`
  gymnastics in `arm64_directives.go:55`.
- The GNU-text sidecar and external assembler; `TranslateAssembly`,
  `assemblyBundle.source`, `CompileObjectAndAssembly`'s string result.
- The double parse in `goc/compile.go:sourceAssemblyReferences` (replaced by
  `sem.Unit.References` from a single parse, cached on the module).

**Narrows but remains (correctly, as backend/goc policy rather than plan9asm
knowledge):**
- `ir.Func.GoABI/NoSplit/HasClosureContext/ParamGroups` and `arm64/goabi.go`:
  these describe how *goc-compiled Go code* calls Go code — the runtime's
  morestack contract (`mc.goStackPrologue` spilling argument registers for
  `runtime_morestack_noctxt`), the GC argument stack maps (`goArgumentFrameFor`),
  and closures. Assembly no longer forces this choice; flipping the runtime off
  ABIInternal later becomes a goc/arm64-only change. The plan does **not** attempt
  that flip.
- Raw functions' register contracts (g in x28, `gogo`'s gobuf layout, morestack's
  x3-holds-LR protocol): inherent to the Go runtime's design, kept verbatim; the
  `mc.goStackPrologue` ↔ `morestack` handshake must keep matching what
  `asm_arm64.s`'s Raw `morestack` expects.
- `go_metadata_object.go` pclntab/pcsp emission — unchanged, but now fed uniformly
  from one object.
- The special knowledge that `asyncPreempt` is entered ABIInternal-with-no-args
  (`arm64_compile.go:146`) becomes a one-line "Raw, exported, funcID 3" table
  entry.

## 5. Phased migration

The two pipelines coexist behind a per-file switch: extend
`supportedARM64Files` (`arm64_compile.go:38`) with a parallel
`semanticallyLoweredFiles` set; `prepareAssembly` skips files in the new set, and
the new lowering handles only them. Every phase ends with the full suite green:
`goc` corpus (`goc/corpus_test.go`), runtime boot (`CompileTestExecutable` +
`goc test` on stdlib packages), `plan9asm` tests, and `a64`'s reference-assembler
tests.

- **Phase 0 — foundations (no behavior change).**
  a. Extend `a64` encoders + text assembler: acquire/release atomics
     (LDAR/STLR/LDAXR/STLXR), LSE (CASAL/SWPAL/LDADDAL/LDCLRAL/LDSETAL), barriers
     (DMB/DSB/ISB), SVC/MSR/MRS(named)/DC, adrp/lo12, and the SIMD set actually
     used (inventory = the switch in `plan9asm/arm64.go:88` plus
     `arm64_vector.go`). Each encoder lands with a `TestEncodingsMatchAssembler`
     byte comparison (`a64/a64_test.go`). *(Note: much of the atomics/barrier/
     mrs/adrp encoder + parser + disasm work already landed on `main`; rebase or
     port it.)*
  b. Extend `a64.RelKind` + `programObject`/`aarch64RelType` for page/lo12 relocs.
     *(The adrp/adr + inline-asm relocation-propagation fix also landed on `main`;
     port it.)*
  c. Extend `mc.emitAsm`: `AssembleProgram`+`Link` with reloc re-anchoring;
     clobber-aware scratch selection; `%q`/vector substitution.
- **Phase 1 — `plan9asm/sem`.** Build the semantic package. Gate: it ingests
  **every** file in `supportedARM64Files` without error, classifies each TEXT, and
  its inferred `Signature`s equal `collectABI0Layouts`' output on the whole corpus
  (a pure differential unit test — no codegen yet).
- **Phase 2 — data.** Route GLOBL/DATA of migrated files to `ir.Data`. First
  migrated content: `chacha8` tables, `bytealg` masks, `memclr`'s `block_size<>`.
  Keep data migration per-symbol within a file to avoid split-brain.
- **Phase 3 — vertical slice: `internal/runtime/atomic/atomic_arm64.s`.** The
  proving ground: tiny bodies exercising every Bindable mechanism (FP slot binding
  for loads *and* result stores, acquire/release + exclusive-loop + LSE encoders,
  in-template labels for `Cas`'s retry loops, tail aliases, NOSPLIT, and heavy
  real-world traffic — every runtime boot uses it). Migrate `sync/atomic/asm.s`
  (pure aliases) with it. Gate: runtime boots, corpus + `goc test sync/atomic`
  pass, and the differential harness (§7) compares behavior against the old path.
- **Phase 4 — the leaf library set.** `bytealg` (5 files), `memmove`/`memclr`
  (DC ZVA path in memclr classified Raw if needed), `chacha8` (ABIInternal-declared
  registers → renamed operands; local `$16` frame → OAlloc),
  md5/sha1/sha256/sha512/crc32, `internal/cpu`, `dit_arm64.s`. The SIMD/crypto
  encoder shakeout.
- **Phase 5 — the runtime core (Raw machinery).** `sys_linux_arm64.s` (mostly
  Bindable syscall stubs; sigtramp/clone Raw), `tls_arm64.s`, `rt0_linux_arm64.s`,
  `preempt_arm64.s`, `asm_arm64.s` (`gogo`/`mcall`/`systemstack`/`morestack` — Raw
  with the funcID/flag/frame table), `runtime/atomic_arm64.s`, `secret_arm64.s`,
  stubs. As each file's last symbol migrates, its wrapper needs vanish; when
  `abi0References` is empty, delete `emitGoABI0AssemblyWrappers`.
- **Phase 6 — demolition.** Delete `arm64/assembly.go`'s text path, the
  `plan9asm/arm64*.go` translation files (parser + sem remain),
  `arm64/go_abi0_assembly.go`, `plan9asm/arm64_abi0.go`; drop
  `compileRuntimeSupport` and the `cc` dependency from `cmd/goc/main.go`; collapse
  `CompileObjectAndAssembly` into `CompileObject`.
- **Phase 7 (out of scope, enabled).** Revisit `GoABI=true`-for-everything in
  `goc/compile.go:323` — now purely a compiler-side decision.

## 6. Risks, unknowns, mitigations

1. **Encoder coverage** (biggest volume risk): dozens of SIMD/crypto encodings.
   Mitigation: the corpus-driven inventory is closed and known; every encoder is
   differential-tested against `aarch64-linux-gnu-as` byte-for-byte; phase-gated
   so untested encoders never ship live.
2. **Scratch/clobber collisions in `emitAsm`**: bodies freely use x15/x16/x17 and
   V30/V31 (cg12's scratch, `arm64/reg.go:55`). Mitigation in Phase 0c; add a
   verifier that rejects an OAsm whose clobbers cover all scratch while operands
   need materialization.
3. **Bindable bodies whose operands must survive an internal call** (segment
   splitting): label-crossing splits are rejected and the function falls back to
   Raw — a safe, always-available escape hatch. Unknown: exactly which functions
   trigger it; resolve empirically in Phase 1 by reporting classification for the
   whole corpus.
4. **`asyncPreempt`**: entered by PC-hijack with a signal-pushed 16-byte record,
   hand-built 240-byte frame, `RET (R27)`, NZCV/FPSR save. Must remain
   byte-equivalent Raw output; keep its explicit pcsp override (240/8).
   Differential disassembly against the old translator's output is the acceptance
   test.
5. **pcsp fidelity for Raw functions**: the unwinder crashes on wrong pcsp.
   Mitigation: keep the current per-function values (already computed/overridden in
   `ARM64Function`/`assemblyFrameSize`), assert sem-computed deltas match them
   where both exist.
6. **PCALIGN inside functions** (`memmove` loops): `a64.Program` is word-granular;
   add an `Align` fixup that pads with nops and ensure function base alignment ≥16.
   Inline-OAsm alignment is relative to an allocator-dependent base — restrict
   `Align` to Raw functions, or pad conservatively; `memmove` may need Raw
   classification if 16-byte alignment must be exact (performance-only, so
   nop-padding is acceptable).
7. **Signature mismatches between Go decls and FP usage** (assembly reading only
   `b_base` of a slice): the ValueGroup-aware signature covers this, but Phase 1's
   differential against `collectABI0Layouts` is the systematic detector.
8. **`WORD`/`.inst` escape hatch**: keep `RawWord` in both Bindable templates
   (extend the a64 text parser with `.inst`) and Raw programs — guarantees no
   instruction is ever unrepresentable.
9. **Behavioral drift while both pipelines coexist**: symbols must resolve across
   the object/sidecar boundary during Phases 2–5. The existing
   `assemblyReferences`-driven `Global` symbol promotion (`mc.go:232`) handles
   direction one; migrated ir.Funcs are ordinary global text symbols for direction
   two. Add a link-time assertion that no `_abi0` symbol is both defined and
   migrated.

## 7. Test strategy

1. **Encoder truth**: extend the existing `a64` reference-assembler harness
   (`a64/a64_test.go:assemble` + `TestEncodingsMatchAssembler`) for every new
   encoder and text-parser production; same for `disasm.go` round-trips.
2. **Sem differential (Phase 1)**: for the full corpus, assert (a) total
   classification with an explicit expected Bindable/Raw list, (b) `Signature` ≡
   `collectABI0Layouts`, (c) reference set ≡ `ARM64Translation.ExternalReferences`.
3. **Codegen differential**: for each migrated file, build the same functions
   through both pipelines and compare `arm64.Disassemble` output of Raw functions
   instruction-for-instruction (Raw must be near-identical modulo label names);
   for Bindable functions compare *semantics* via execution, not bytes (register
   allocation legitimately differs).
4. **Execution**: the existing `goc/corpus_test.go` suites,
   `TestRuntimePanicRecover`, `TestStandardLibrarySHA256`, plus targeted
   `goc test` runs of `sync/atomic`, bytealg-dependent packages (`strings`,
   `bytes`), `internal/chacha8rand` (`math/rand/v2`), and crypto packages — each
   promoted into CI at the phase that migrates its assembly.
5. **Boot + preemption stress**: a corpus program that forces stack growth (deep
   recursion, exercising morestack↔Raw interplay), a tight-loop program that
   provokes async preemption, and a signal-heavy program (sigtramp), run under
   both pipelines during Phases 3–5.
6. **Metadata checks**: unit tests asserting pclntab pcsp/args entries for migrated
   functions equal the pre-migration values (extracted via `goFunctionInfo` on
   each side).

## Critical files for implementation

- `plan9asm/arm64.go` (plus `arm64_abi0.go`/`arm64_compile.go` — the semantics to
  be reified, and the ABI coupling to delete)
- `arm64/mc.go` (`emitAsm`, `materializeSym`, `compileToObjectWithBundle` — the
  lowering target and object integration point)
- `arm64/a64/textasm.go` (with `a64.go`/`asm.go` — encoder and relocation surface
  that must grow)
- `arm64/assembly.go` (pipeline switch-point where the new per-file lowering
  replaces the text bundle)
- `ir/instr.go` (`AsmOp`/`AsmSpec` — operand-kind and signature extensions)

## First things to resolve

1. The exact Bindable/Raw partition over the corpus (the Phase 1 report answers
   it).
2. Whether any bindable body needs call-splitting across labels (risk 3).
3. Whether `memmove`'s PCALIGN tolerance permits Bindable classification (risk 6).
