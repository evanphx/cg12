# PLAN: Bring the amd64 backend to arm64 parity

`arm64` compiles and runs real Go programs; `amd64` does not. This plan closes
that gap. It is written for concurrent execution: every phase states file-level
ownership so independent agents cannot collide, and the serial prerequisites are
called out explicitly because getting them wrong makes parallel work produce
silently-wrong results rather than failures.

Sizes below are measured, not estimated: arm64 is 21,798 lines across 68 files,
amd64 is 7,189 across 34. Eighteen arm64 files have no amd64 counterpart.

**Standing decisions.** Go runtime assembly is handled by pure-Go stdlib
overlays first, with the Plan 9 amd64 translator deferred (§6, Track D) — the
goal is a running Go program on amd64 early, not a faithful reproduction of how
arm64 got there. Concurrency is capped at roughly six agents at the widest point,
each wave gated on the previous wave's tests passing, so that contract drift is
caught while it is still cheap to fix.

## 1. Measured starting state

**amd64 is a working freestanding-Go-subset target, and not a Go target at all.**

Confirmed by running: `goc -run hello.go` prints `hi` on an x86_64 host. Slices,
`append`, `range`, map literals, `len`/index/`delete`, interface method dispatch,
generics, pointer-receiver methods, `errors.New().Error()`, and
`string(rune(65))` all compile and execute. 15 of 148 corpus execution cases run
through amd64 (`goc/corpus_test.go:4170` → `nativeObject`, which already
switches on `runtime.GOARCH`); the other 133 skip.

The first wall is in the **frontend**, before amd64 emits an instruction:

    stdlib/src/runtime/mgc.go:318:7: runtime.getg intrinsic is unsupported on amd64

`goc/compile.go:9249` hard-fails `runtime.getg` off arm64. Forcing
`goc.CompileExecutable` → `amd64.CompileObject` never reaches codegen, even for
an empty `main`.

Four further failure classes, each at a different stage:

| Symptom | Stage | Cause |
| --- | --- | --- |
| `string concatenation requires the Go runtime` | frontend | `goc/compile.go:13106`, gated on `g.runtimeAllocation` |
| undefined `runtime_makechan`/`newproc`/`chansend1` | link | `cmd/goc/main.go:200-204` links no runtime support object off arm64 |
| undefined `internal_bytealg_IndexByteString` | link | `goc/source_import.go:349` disables assembly; the `purego` tag does not satisfy `//go:build amd64 && !plan9`, so a declaration-only file is selected with its `.s` body skipped |
| `panic`/`recover` → SIGABRT | run | `panic` lowers to libc `abort()` |

Test state: `go build ./...` passes. `go test ./amd64/...` passes. `go test
./goc/...` has **48 failures**, 45 of them the same `getg` wall.

### 1.1 Three findings that dictate the sequencing

**(a) amd64 silently accepts Go-ABI annotations.** `amd64.CompileObject` given
`CallConv = ir.CallConvGoInternal` and `ManagedFrame = true` returns `err = nil`
and emits a plain System V frame with no stack-growth check:

    push %rbp; mov %rsp,%rbp; mov $0x2a,%eax; mov %rbp,%rsp; pop %rbp; ret

Every downstream agent whose work reaches amd64 codegen would get quietly wrong
code instead of an error. A hard `unsupported` guard must land first.

**(b) 52 of 63 amd64 tests do not run.** `amd64/mc_test.go:40` and
`amd64/asm_test.go:24` call `testenv.Tool(t, "qemu-x86_64")` — the harness was
written on an arm64 dev box and emulates x86-64 even when the host *is* x86-64.
qemu is absent here, so `testenv.Tool` skips and every `mc_test.go` test, all
aggregate-ABI tests, int128, complex, VLA, packed, switch, jumptable, va,
tailcall, gcstrategy, regvar, and fixedreg vanish. Verified that native
execution works: freestanding `_start` + `ld.lld -static -nostdlib`, run
directly, exits 7 as expected. This is the cheapest high-value fix in the plan.

**(c) A live miscompile, independent of Go support.** `amd64/xselect.go:342-355`
collapses signed and unsigned float conversions. `OStoui` and `OUltof` both emit
signed SSE (`cvttsd2si`/`cvtsi2sd`); x86-64 has no unsigned 64-bit conversion, so
values ≥ 2^63 yield the indefinite `0x8000000000000000` or a negative double.
Because the ops *are* handled, no unsupported-op error fires. arm64 is correct
natively (`fcvtzu`/`ucvtf`).

## 2. The parity bar

`RUNTIME_PLAN.md` scopes itself to Linux/ARM64 and defines done in 8 points with
a measured baseline (294 programs, 49.6% active-function coverage, 30.6%
compiled-block). Parity means that document's scope, definition of done, and
gates hold for `linux/amd64` — not a new standard.

Concretely: `TestExecutionCorpus` and `runExecutableCase` both green on amd64;
the 322-capability matrix runnable with an amd64 expectation set; a checked-in
`runtime_coverage_linux_amd64.json` baseline; and an x86_64 CI leg.

**The prize is iteration speed.** CI is arm64-only by deliberate choice
(`.github/workflows/ci.yml`). arm64's 103 local skips are un-overridable without
a cross toolchain. amd64 at parity makes the full suite runnable natively on
x86_64 — seconds instead of a CI round-trip.

## 3. Critical path

Two items dominate. Everything else fits around them.

**3.1 Plan 9 assembly is the dominant cost.** Go's runtime ships hand-written
Plan 9 asm per architecture. `plan9asm/` is arm64-only across all three layers,
and the parser *fails silently* rather than erroring — real Go amd64 runtime
assembly degrades to untyped raw operands:

    MOVQ (TLS), AX          → both operands kind=0
    MOVQ (SI)(BX*8), CX     → raw
    LEAQ 16(SP)(AX*4), DX   → raw
    ADDQ $16, DI            → only the immediate survives

`isRegister` (`plan9asm/parser.go:968-987`) accepts only AArch64 spellings, and
`ir.Operand` (`plan9asm/ast.go:101-118`) has **no `Scale` field**, so x86 SIB
addressing is unrepresentable. `sem/` claims "ABI-independent machine semantics"
in its own doc comment but bakes AArch64 into `registerName`
(`sem/build.go:808-832`) and `normalizeOperation` (`:923-971`). Cost: ~1,100 LOC
of shared parser/sem to generalize, plus a translator at the scale of the
existing 5,311-LOC arm64 one, plus an allowlist over 19 packages / ~38 files.

The container is fine: `ir.Module.Assembly` (`ir/func.go:310-380`) and the ABI0
layout (`goc/compile.go:602-608`) are already arch-neutral. Only the translator
is arm64.

**3.2 amd64 register pressure is a genuine design problem with no arm64
precedent. SETTLED — see §3.3.** arm64 reserved X28 for `g` out of 31 registers —
trivial. On amd64:

- Go ABIInternal passes args in `RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11`, but
  amd64 reserves R10/R11 as its *only* scratch pair and holds RAX/RCX/RDX out of
  allocation for fixed-register instructions (return value, `div`/`rem` in
  RDX:RAX, shift count in CL). Only 9 GPRs are allocatable.
- `g` in R14 costs 1 of just 5 callee-saved registers (arm64 loses 1 of 10).
- RDX is the Go closure register but is reserved for div/rem *and* used as
  `vaArg` scratch (`amd64/mc_va.go:54`).

The register sequence is also non-contiguous, so arm64's arithmetic register
computation (`X0 + Reg(a.ngrn)`, ~8 sites) becomes table indexing — which is why
the convention descriptor is not a straight copy.

**This must be settled before ABI work fans out.** It is one agent's decision,
recorded as a written contract, then consumed by everyone else.

### 3.3 The register decision (B0) — settled

Recorded in `amd64/reg.go` and `amd64/convention.go`, enforced by
`amd64/convention_test.go`. Zero behavior change: the System V tables are
byte-identical to what they were, nothing yet asks for the ABIInternal ones, and
the full suite is unchanged.

Most of it is not a choice. Go's amd64 ABIInternal fixes the argument registers
(`RAX, RBX, RCX, RDI, RSI, R8, R9, R10, R11`, `IntArgRegs = 9` —
`stdlib/src/internal/abi/abi_amd64.go:10`), `g` in **R14** (`MOVQ DX, R14 // set
the g register`, `asm_amd64.s:444`), the closure pointer in **RDX**
(`asm_amd64.s:2003`), and **X15 as a zero register** (`asm_amd64.s:1089`). The
runtime reads all four by name, so they are transcribed, not decided.

Four things did have to be decided, and three of them contradict this plan's own
assumptions:

**(a) There is no scratch pair that works under both conventions.** §3.2 framed
R10/R11 as "amd64's only scratch pair" without noticing they are also
ABIInternal's argument registers 8 and 9. System V's only caller-saved
non-argument registers are RAX, R10, R11; ABIInternal, after its nine argument
registers plus RDX/R14/RSP/RBP, leaves exactly R12 and R13 — which is why Go's
own compiler keeps that pair free. So `scratchGPFor` is per-convention: R10/R11
under System V, R12/R13 under ABIInternal. Sharing one pair was rejected because
R12/R13 are callee-saved under System V, which would cost every C-path function
two pushes and two pops to protect a caller's live value.

**(b) The float scratch pair collides with the zero register.** cg12's emitter
needs two float scratch registers (they are used together in `xasm_float.go`'s
unsigned-conversion bias). Go passes float arguments in X0–X14 and requires X15
to be zero, which leaves nothing to improvise with. `goArgFP` is therefore capped
at X0–X12, deviating from Go's `FloatArgRegs = 15`. A signature wanting a 14th
float register argument must be **refused by name** (`goFloatArgRegSpill`), not
quietly passed on the stack: cg12-compiled code would agree with itself, but the
runtime's `abi.RegArgs` is sized by Go's 15 and a reflect-mediated call would
read a register cg12 never wrote.

**(c) RAX/RCX/RDX stay out of allocation**, as today. They are ABIInternal
argument registers 1–3, but that is not a conflict: an argument arrives in a
Fixed temp pinned to its register, which is independent of whether the allocator
may hand that register out. Keeping them reserved preserves the property
`xselect_bits.go:52` and `xselect_atomic.go:61` state their correctness in terms
of. Cost is a copy out at entry — the copy arm64 already pays for X26.

**(d) The `CallConv` overload resolves in favour of the callee, and arm64's rule
must not be copied.** §Track B asked whether to narrow goc's marking or add a
distinct signal. Neither: the marking stays, and resolution changes. Measured
from real IR (`TestGoInternalFunctionsMakeUnmarkedPlatformCalls`), a
`CallConvGoInternal` function is **never** the target of a direct call — it is
reached only through a func value, whose call site carries an explicit
`CallConvSet`. But such a function **does** make unmarked direct calls to
ordinary platform-ABI functions. arm64's `callUsesGoInternal` inherits the
enclosing function's convention for exactly those calls, which lowers them as
ABIInternal against a System V callee.

That is latently wrong on arm64 too; it is invisible there only because both
AAPCS64 and ABIInternal assign integer arguments from X0 upward, so small
argument counts pick the same registers. amd64 has no overlap — System V starts
at RDI, ABIInternal at RAX — so copying the rule would miscompile the first
method value it met. `calleeConventions.forCall` resolves instead from the call's
explicit convention, then the callee's own, then platform ABI for symbols outside
the module.

**arm64 carried the same bug and is now fixed.** The follow-up audit this section
originally called for found it reproducible: a nine-integer-argument unmarked
direct call to a platform-ABI callee lowers to x0..x7 plus a stack slot from a
platform caller, but to x0..x8 from an ABIInternal caller — the ninth argument
lands in x8 where the callee reads the stack. `arm64/convention.go` now carries
the identical `calleeConventions` rule, resolved once per object and threaded to
lowering, frame layout, and the emitter so they cannot disagree. Two notes for
whoever meets it next: `applyAssemblyCallConventions` already stamps
`CallConvSet` on direct calls to symbols the object defines, so in the real
driver the unsound fallback only reached calls to *external* symbols and unmarked
indirect calls; and the pre-existing "cannot tail-call across calling
conventions" guard is now reachable where it was structurally dead before, so an
ABIInternal function tail-calling an external symbol is a hard error rather than
a silent mismatch.

Also settled, for the contracts §7 says must be fixed in writing before fan-out:
`stackLinkBytes = 0` under both conventions (the call instruction pushes the
return address, so arm64's hand-reserved `goStackLinkSize` link already exists);
and `reservesRuntimeRegs` keys off **frame or convention**, not convention alone,
because goc emits managed-frame platform-ABI helpers whose prologues still read g
out of R14.

**What B0 does not do.** The emitter still spells `gpScratch0`/`gpScratch1`
directly at ~150 sites and so implements only the System V row. That is correct
today — `lowerParams`/`lowerCalls` build platform assigners unconditionally, so a
`CallConvGoInternal` function is emitted as self-consistent System V code, which
is what the 14 corpus subtests depend on. Adopting `scratchGPFor` in the emitter
is B2's, and `TestGoABIScratchDoesNotAliasArgumentRegisters` pins why it cannot
be skipped. The `getg` gate at `goc/compile.go` also stays: knowing R14 holds g
is not the same as being able to compile a function that uses it, and trading one
accurate diagnostic for downstream scatter helps nobody until Track B lands.

## 4. Phase 0 — make failure loud (4 agents, fully parallel)

Small, independent, different files. No cross-dependencies.

| # | Work | Files owned |
| --- | --- | --- |
| 0a | Native runner: use the host directly when `runtime.GOARCH == "amd64"`, qemu only as cross fallback. Unlocks 52 tests. | `amd64/mc_test.go`, `amd64/asm_test.go` |
| 0b | Hard `unsupported` guard for `CallConvGoInternal`/`ManagedFrame`/`NoSplit`/`SystemStack` reaching amd64 codegen | `amd64/mc.go` (entry only) |
| 0c | Split the signed/unsigned float conversion cases; add the compare-and-bias sequences | `amd64/xselect.go` |
| 0d | Delegate the amd64 symbol mangler to `ir.LinkerSymbol` (GO_INTEGRATION_PLAN 4c missed amd64) | `amd64/data.go` |

Gate: `CG12_REQUIRE_TOOLS=1 go test ./amd64/...` with a **zero or explicitly
justified** skip count. Without that env var an agent can "finish" a file that
skips 100%.

## 5. Phase 1 — enabling refactors (6 agents; 1a/1b sequenced)

Behavior-preserving restructuring that creates the seams Phase 2 needs. Doing
these first is what makes Phase 2 append-only.

| # | Work | Files owned | Notes |
| --- | --- | --- | --- |
| 1a | Split the `xasm` interface by embedding (`xasmCore`/`Int`/`Float`/`Mem`/`Flow`/`Stack` + empty `Atomic`/`Bits`/`Wide`/`TLS`); turn `selectInt` into a probe chain | `amd64/xasm.go`, `amd64/xselect.go` | Reduces the 5 contended lines to ~0. Run before 1b. |
| 1b | Move `frameLayout`/`computeFrame`/`allocShape`/`slotAddr`/`savedAddr` out of `mc.go` → new `amd64/frame.go`; move `argLoc`/`argAssigner`/`retReg`/`newPinned` out of `lower.go` → new `amd64/convention.go` | `amd64/mc.go`, `amd64/lower.go`, `amd64/frame.go`, `amd64/convention.go` | Pure moves. Without this, Phase 2's prologue and frame agents both edit `mc.go`. |
| 1c | Extract `internal/backendtest`: `{Machine, startStub, linkArgs, runner}`, runner = native-when-matching / qemu-when-available / skip. Migrate `runObj`/`buildAndRun`; fold `runAsmSrc` (~90% duplicate of `runObjWith`) | `internal/backendtest/`, `amd64/mc_test.go`, `amd64/switch_test.go` | After 0a. Makes amd64 test work additive rather than duplicative. |
| 1d | Thread an explicit target through `goc`, mirroring the existing `cc.Target` model (`cc/compile.go:161-165`). Replaces ~14 `runtime.GOARCH` reads in `goc/` and 8 in `cmd/goc/` | `goc/` target plumbing, `cmd/goc/main.go` | **Highest leverage in the plan.** Without it there is no way to *ask* for amd64 Go output, so nothing downstream is testable. Also unblocks cross-compilation. |
| 1e | Hoist the arch-neutral ~55-60% of `arm64/go_metadata_object.go` into a shared package, parameterized by `{pcQuantum, reloc32Type, reloc64Type, frameBaseBytes}` | new shared pkg, `arm64/go_metadata_object.go` | moduledata is byte-identical across arches (every field a uintptr or slice header; no arch discriminator). Stack maps carry no register numbers. Tests move verbatim. |
| 1f | Relocate `inferStackPointerWords` and the aggregate pointer-word helpers into `ir`/`opt` (already pure IR-to-IR; GO_INTEGRATION_PLAN 2b flags this) | `ir/` or `opt/`, `arm64/mc.go`, `arm64/goabi.go` | ~270 lines that would otherwise be copied. Follow the `lower/alloca.go:18` precedent. |

| 1g | Add a copy-based derivation helper (`g.derive()`) for `goc/compile.go`'s eleven `&gen{}` literals. The ten derived wrapper/child/adapter generators are currently built field-by-field from their parent, so any new whole-compilation field added to `gen` is silently zero in all ten unless every literal is updated by hand. This already bit 1d once (see §8 item 2a) and Phase 2 will add more such fields. | `goc/compile.go` | Small, and worth doing before Phase 2 rather than after. |

1d, 1e, 1f are independent of each other and of 1a/1b — four concurrent lanes.
1d is **done** (`goc.Target`, `-target` flag; cross-compilation now expressible —
`-target arm64` on x86-64 emits a real AArch64 object where it previously died at
`getg`).

**Target vocabulary is now fragmented five ways** and wants consolidating:
`cc.Target`, `goc.Target`, `ir.Func.MarkLowered(string)`, `cmd/cg12 -target`, and
`cmd/cg12cc`'s `CG12_TARGET` env var. The valuable half is unifying `MarkLowered`
onto a typed shared `Target` **and serializing it in the unit format** — today the
marker is not serialized at all (`ir/binary.go`), so it vanishes on round-trip,
which silently removes the tripwire that `ir/func.go:266-267` documents as
catching a real bug (an arm64-lowered module later compiled for amd64). Own item,
not urgent, but it is a real hole.

## 6. Phase 2 — fan-out

### Track A · op coverage (5 agents, append-only after 1a)

Each owns new files only; one line each in `xasm.go`.

| Agent | Work | New files |
| --- | --- | --- |
| A1 | **Atomics. DONE.** Not 22 intrinsics — **37**: `ir/intrinsic.go`'s `registerAtomics` crosses 4 widths × 9 operations plus the fence. Load is a plain move (TSO already gives acquire); store is `XCHG`, the one place a barrier is unavoidable, matching Go's own amd64 runtime; and/or/xor take a CMPXCHG loop when the previous value is wanted, collapsing to a single locked ALU instruction in the void form. | `x64/atomic.go`, `amd64/xasm_atomic.go`, `amd64/xselect_atomic.go` |
| A2 | **Bit ops. DONE** for `OClz`, `OUMulh`, `OSMulh`. **`BSR`, not `LZCNT`:** LZCNT is BSR with an F3 prefix, which a part lacking ABM *ignores*, so it silently computes a bit index where a count was wanted — and there is no CPU-feature model in the tree to gate it on. `OClz(0)` is recovered from ZF via `CMOVE`, since the IR defines it as the operand width while BSR leaves its destination undefined. Rotates, popcount, byte-swap, trailing-zero, double-shift and bit-test are **deliberately absent**: no IR op reaches them (the C frontend synthesizes those builtins), and `ORotr`'s only producer is an arm64 pass. | `x64/bits.go`, `amd64/xasm_bits.go`, `amd64/xselect_bits.go` |
| A3 | **Immediates + CMOV** — `x64.AddImm`/`AndImm`/`OrImm`/`XorImm`/`ImulImm`/`Cmovcc` all exist and are used **zero** times by selection. Biggest code-quality win. Mirror `arm64/imm.go`. Still to do. | `amd64/imm.go`, `amd64/xasm_imm.go`, `amd64/xselect_imm.go` |
| A4 | **128-bit memory. DONE.** All memory forms are `MOVDQU`; the aligned form appears only register-to-register, since nothing in the IR promises alignment and `MOVDQA` faults rather than degrades. The `foldaddr.go` skip was **load-bearing for the scalar FP ops** and stays for them — they resolve addresses through `memAddr`, which never reads the folded fields, so folding would be silently dropped. Also claims the `ClsQ` phi copy, which previously fell through to `MOVSS` — a 4-byte move of a 16-byte value. | `x64/sse2.go`, `amd64/xasm_wide.go`, `amd64/xselect_wide.go` |
| A5 | **TLS. DONE** for local-exec (unchanged, pinned byte-for-byte) and initial-exec. The GOT add is its own encoder because linkers relax IE→LE by rewriting exactly that encoding in place; the arithmetically equivalent load-plus-add is unrelaxable. General-dynamic is refused. | `amd64/tls.go`, `x64/tls.go`, `amd64/mc.go` (`Options` + 4 threading sites) |

**Two corrections to the plan's own assumptions, found by doing the work:**

- **General-dynamic TLS has a second blocker that F2 does not remove.** Beyond `obj/`'s missing x86-64 TLSGD/DTPMOD64/DTPOFF64 relocations and its typing of a GOTTPOFF symbol as ordinary data rather than `STT_TLS`, the `__tls_get_addr` **call clobbers every caller-saved register**, so GD cannot be reached from the post-allocation materialization path at all. It needs arm64's shape — a `lowerTLS` pass emitting `OTLSIndexAddr` + `OCall` — which is why `xasm_tls.go`/`xselect_tls.go` are held open. The canonical sequence also carries mandated padding prefixes so linkers can relax it in place; omitting them yields a non-relaxable sequence.
- **128-bit values must stay register-resident.** Every amd64 spill slot is 8 bytes, fixed in `gcalloc.go`, `callersave.go`, and `frameLayout.slotAddr`'s stride, so a 16-byte spill overruns its neighbour. arm64 sizes slots per class. `OSpill`/`OReload` of `ClsQ` therefore **fail loudly** rather than truncate; lifting this is B3/B4 work and is a prerequisite for any Go code that keeps a 128-bit value live across a call.

**New optimization item (from A1):** the IR's atomic intrinsics carry **no memory order** — `ir/intrinsic.go` encodes only operation and width, and `cc/atomic.go` discards C's order argument. The backend must therefore assume sequential consistency, so every `atomic.store` pays for an `XCHG` where a release store would be a plain `MOV`. Go's runtime distinguishes `Store` from `StoreRel` and gets the cheap form. Adding an order field to the intrinsic would recover it.

A3 and A5 touch pre-existing bodies (`binInt`/`cmp`; `Options` + `lower()`
signature). Land A3 before A5. A1, A2, A4 are fully concurrent.

`x64` also needs memory-*destination* ALU forms, which are structurally absent —
it has only `reg ← reg OP mem`. Required for every locked RMW. Assign to A1.

### Track B · ABI and frame (1 serial + 5 parallel)

**B0 (serial, blocks the track): DONE — see §3.3.** The §3.2 register decision,
plus per-convention `argGP []Reg`/`argFP []Reg` tables, R14=`g` reservation, RDX
closure register, `calleeSavedFor`/`reservesRuntimeRegs`/`intAllocOrderFor`, and
`argAssigner{intRegs, floatRegs, goABI}` + `assignStack`. Owns
`amd64/convention.go`, `amd64/reg.go`.

> **B0 must not key the convention table on `CallConv` alone without first
> resolving this.** Found while building the Phase 0b tripwire: goc marks every
> function literal, method-value wrapper, and funcvalue adapter
> `CallConvGoInternal` *unconditionally*, while actually passing the closure
> environment through a fixed-register temp rather than through the convention's
> register assignment. Because amd64 ignores `CallConv` today, those bodies come
> out as self-consistent System V code and run correctly — 14 corpus subtests
> depend on it.
>
> **Resolved in §3.3(d):** neither of the two options this plan offered. The
> marking stays as it is and *resolution* moves to the callee —
> `calleeConventions.forCall`. Measurement showed the hazard is not the one
> anticipated: an ABIInternal function is never the target of a direct call, so
> the marking cannot mis-lower a call *into* it. The real exposure is the
> unmarked calls such a function makes *out* to platform-ABI functions, where
> arm64's enclosing-function fallback would pick the wrong convention.

**Contract for B1: `lower` needs the module.** `amd64/lower.go`'s `lower(f
*ir.Func)` cannot resolve a direct callee's convention from a function alone.
B1 threads `calleeConventions` (built once per module in `CompileObjectWith`)
through to the call-lowering sites.

Then, disjoint ownership:

| Agent | Work | Files |
| --- | --- | --- |
| B1 | Go ABI lowering: aggregate flattening/assignment, pointer-word discovery, param/arg/result/return lowering, argument home slots, call-area sizing, closure stabilization (~900 lines) | `amd64/goabi.go` (new) + 4 dispatch sites in `lower.go` |
| B2 | Prologue: morestack guard off R14, argument-home spills, `runtime_morestack_noctxt`/`morestackc`, retry, pointer-slot zeroing | `amd64/mc.go` `prologue()`, `amd64/gc.go` new strategy |
| B3 | Caller-save + spill-slot coalescing; managed-frame spill policy; `registerSurvivesManagedCalls` | `amd64/slotcolor.go` (new), `amd64/callersave.go`, 1 line in `gcalloc.go` |
| B4 | Frame layout: managed `outgoing`, allocation-order-stable callee-save list, alloca coloring | `amd64/frame.go`, `amd64/allocacolor.go` (new) |
| B5 | Fall-through block ordering (pure optimization, zero ABI coupling) | `amd64/layout.go` (new) |

**Do not port** (~200 lines of arm64 code that exists only for AArch64
constraints): `spillImmFits`, `frameAddr` hi/lo splitting, `adjustSP`'s
`#hi12<<12 + #lo12`, `stpPairable`'s ±512 window, `framelessEligible`,
`goRegisterPointerMask` (dead). amd64 disp32 always fits, and the return address
is always on the stack. `stackLinkBytes = 0`, so arm64's `goStackLinkSize`
offsetting disappears throughout.

**Zero work, do not touch:** `amd64/regalloc.go` and `amd64/remat.go` already
match arm64 and already carry the load-bearing GC-ref exclusion.

### Track C · Go metadata, amd64 side (4 agents, after 1e + B0)

| Agent | Work |
| --- | --- |
| C1 | pcvalue tables — `goPCSP`, `goStackMapPCData`, `goUnsafePointPCData`, `goPCValueTable`. amd64 is *simpler*: `minLC=1`, `prog.Len()` already in bytes, so the `/4` sites drop. |
| C2 | Stack and argument pointer maps. Largest genuinely-new work in the track. |
| C3 | **PCDATA slot 5** — must be *re-derived*, not ported. The runtime consumer is `usesLR`-gated (`stdlib/src/runtime/traceback.go:371-381`), false on amd64; but `:412` and `:416-420` fire on both. Needs a matching runtime-source patch. Highest risk in the track — strongest agent, do not parallelize with C2. |
| C4 | Integration: `finishGoModule`/`goFunctionInfoFor`/`addNeutralData` split; suppress `.cg12_stackmaps` for Go modules (`amd64/mc.go:123` needs the `!goRuntime` guard) |

**Hard cross-agent contract:** arm64 roots are *positive* offsets from x29 with
the frame record at [0,16), decoded `(val-16)/8`. amd64's are *negative* from RBP
(`amd64/mc.go:602`). Every word-index conversion inverts sign. B0 fixes the
base/sign convention in writing; B2, C2, and C3 all consume it.

Free wins found in recon: itabs, typelinks, inittasks, type descriptors, and
write barriers are 100% frontend (`goc/compile.go` producing `ir.Data`) — no
backend work. `link/link.go` contains no Go-specific code.

### Track D · Go runtime assembly (2 agents; translator deferred)

**Decided: pure-Go overlays first.** The `stdlib/overlays/` mechanism exists for
exactly this — its README permits `.go` and cg12-native `.ssa` overlays and
forbids assembly ones, with a hash-pinned `manifest.json` that fails closed when
upstream changes. Building a `linux_amd64` overlay tree gets Go programs running
on amd64 far sooner than a translator would, and the translator becomes a later
fidelity/performance pass rather than a blocker.

| # | Work |
| --- | --- |
| D0 | **Still required.** De-arch `plan9asm/parser.go:968-987` `isRegister`; add `Scale` to `ir.Operand` (`ast.go:101`). Not deferrable even though the translator is: today the parser accepts x86 assembly and silently degrades every register and memory operand to untyped raw, which is a latent trap for anything that later feeds it amd64 input. Make it *error* instead. |
| D0b | While in `plan9asm`: two further copies of the canonical linker mangling live at `plan9asm/sem/build.go:843` and `plan9asm/arm64_operands.go:380`, spelled via `unicode.IsLetter`/`IsDigit` gated on ASCII. They look equivalent to `ir.LinkerSymbol` but were not verified to that standard, so GO_INTEGRATION_PLAN 4c likely missed them too. Collapse them only after checking equivalence the same way amd64's was checked (all 256 byte values, Go-style paths, invalid UTF-8). **`wasm/emit.go:349` is a genuinely different mangler** — it preserves `.` and `$` and has no `"anon"` fallback — and must not be collapsed. |
| D1 | `linux_amd64` overlay tree + manifest, covering the packages whose assembly bodies currently dangle. Note the specific trap found in recon: the `purego` build tag does **not** satisfy `//go:build amd64 && !plan9`, so Go's own constraints still select declaration-only files (e.g. `internal/bytealg/indexbyte_native.go`) while skipping their `.s` bodies. Overlays must replace those declarations, not merely add a tag. Also widen `goc/native_overlay.go:57` to accept `"sysv"` alongside `"aapcs64"`. |

**Deferred (revisit after amd64 runs Go programs):** per-arch tables behind an
interface for `sem/build.go:808-832` `registerName` and `:923-971`
`normalizeOperation`, and an amd64 translator + allowlist paralleling
`plan9asm/arm64_compile.go:39-121`. Sizing if resurrected: ~1,100 LOC of shared
parser/sem generalization plus a translator at the scale of the existing
5,311-LOC arm64 one.

### Track E · tests (9 agents; see §7 for hazards)

After 1c. Per the recon partition: `unit_test.go` (new internal `package amd64`
file — largest single deliverable, arm64's is 59KB/76 tests); `disasm_test.go`
(the differential — amd64 has **no** in-package disassembler; wire `llvm-mc` as
oracle, already the pattern in `amd64/x64/x64_test.go:25`); the GC/stack-map
cluster; DWARF (extend 3 → 9); select+fuse; the SysV ABI cluster;
morestack+TLS; the regalloc-quality cluster; and Plan 9 asm + Go metadata (gated
on Track D).

amd64 has **zero** byte-level encoding assertions and **zero** disassembly
assertions today; byte-checking lives one level down in `amd64/x64/x64_test.go`
against `llvm-mc`. `disasm_test.go` is the highest-value single addition — it is
what makes every other file's mnemonic assertions trustworthy.

### Track F · shared plumbing (3 agents, independent)

| Agent | Work |
| --- | --- |
| F1 | Widen the `Backend` interface (`link/link.go:24-29`) and route the Go driver through it. It has **zero production consumers** today — `cmd/goc` calls `arm64.*` directly and shells to `cc`. `arm64.WriteObjectAndAssembly`'s (object, assembly-sidecar) shape is the seam to design; decide whether amd64 needs a sidecar assembler at all. Fix `link.New()`'s arm64 default and `merge`'s `EM_AARCH64` default. |
| F2 | x86-64 TLS general-dynamic in `obj/` (`dynamic.go:897-898`, `elf.go:95`, `dynamic.go:1265-1270`). Deferrable until A5 lands, since nothing can request GD before then. |
| F3 | CI: add an x86_64 matrix leg; `CG12_REQUIRE_TOOLS=1` everywhere; parameterize the arm64-only bodies in `link/`. |

`obj/` and `link/` are otherwise the most arch-neutral layers in the tree — x86-64
static and dynamic relocs, full PLT/GOT stub encoders, local-exec and
initial-exec TLS all already present. `ir/`, `opt/`, `lower/`, `analysis/`, and
`interp/` contain zero arch names in non-test code.

## 7. Collision hazards — state these in every agent brief

- `internal/testenv/testenv.go` — shared by all, **read-only for agents**.
- `amd64/mc_test.go` (`runObj`/`runObjWith`/`entry`/`startStub`) and
  `amd64/switch_test.go` (`runC`/`runCOpt`) are shared helper homes. Exactly one
  owner (Phase 1c); everyone else is import-only.
- `cmd/goc/runtime_status_test.go` is a single 2,321-line literal and
  `goc/corpus_test.go` is 4,285 lines — **exactly one agent may ever touch
  each**.
- `amd64/lower.go` is the hottest contended file (421 lines holding
  `lowerParams`, `lowerCalls`, `lowerReturns`, and four aggregate helpers).
  Phase 1b's extraction is what makes it safe.
- `link/` generalization is one serial workstream (16 files, ~20 arm64 gates),
  not a split.

Cross-agent contracts to fix in writing before fan-out, so they cannot drift:
the `goRegisterSpill` struct (B1 produces, B2 consumes); `frameLayout.outgoing` +
`frameTop()` (B4 produces, B1 and B2 read); and the `rootFrame` base/sign
convention (B0 decides, B2/C2/C3 consume).

## 8. Validation gates

1. `CG12_REQUIRE_TOOLS=1` in every agent's test command. A missing tool must be
   a named `Fatal`, never a skip.
2. Skip count zero or explicitly justified. A green run with 52 silent skips is
   the failure mode this plan exists to end.
2a. **For behavior-preserving refactors, diffing failing test *names* is not
   sufficient — diff the normalized full output.** Learned the hard way in 1d:
   its first pass set the new target field on only the outermost of `compile.go`'s
   eleven `&gen{}` literals, so the ten derived generators silently received a
   zero value and every diagnostic lost its architecture name. The set of failing
   tests was byte-identical; only the message text changed. Normalize run-to-run
   timings and the pre-existing nondeterministic choice of reported `getg` site
   (map iteration order — it varies on an unmodified tree too), then require an
   empty diff.
3. No regression in the C/Ruby path: `make test-ruby` must stay byte-identical
   to gcc on the miniruby differential (GO_INTEGRATION_PLAN's standing gate).
4. arm64 must not regress. Locally its heavy tests skip for lack of a cross
   toolchain, so **arm64 validation stays in CI** and every agent's work is
   CI-gated before merge.
5. Phase 0b's guard must remain in force until the corresponding capability
   actually works — it is the tripwire that keeps silent wrong-codegen from
   reaching an agent's green test run.

## 9. Known asymmetry, accepted

`lift/` gives arm64 host-free differential validation (machine code → IR′, assert
`interp(IR) == interp(IR′)`) via `a64.Decode`. There is no `x64.Decode`, so amd64
gets no equivalent oracle and its codegen confidence rests on native execution
plus the `llvm-mc` differential at the encoder level. Writing an x86 decoder
would close this; it is out of scope here but worth an explicit decision rather
than silent omission.
