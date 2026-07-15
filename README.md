# cg12

A machine-code generation package for Go: a small, embeddable, SSA-based
compiler backend. cg12 starts as an idiomatic-Go re-imagining of
[QBE](https://c9x.me/compile/) and grows beyond it.

The repository also includes two source frontends: `cc`, built with
`modernc.org/cc`, and `goc`, built almost entirely on Go's standard
`go/parser`, `go/ast`, `go/types`, and `go/constant` packages. The Go frontend
currently compiles fixed-width and native integer/boolean functions, explicit
scalar conversions, local and package variables, direct calls and recursion,
short-circuit Boolean expressions, `if`, `for`, and expression or expressionless
`switch` statements (including `fallthrough`). Unsupported language features
are diagnosed with source positions.

The `goc` test corpus is arranged as a complexity ladder. It compiles, links,
and executes constants, arithmetic, assignments, branches, loops, calls,
recursion, switches, globals, arrays, structs, slices, pointers, methods, maps,
closures, goroutines, and selected interface operations. Core cases run through
both unoptimized and optimized cg12 IR. This remains an intentionally sound
subset rather than a claim of full Go compatibility.

Imports are resolved from build-selected source in the repository-owned
`stdlib/src` tree with `go/build` and type-checked through `go/types`. The first
ten copied packages and their activation order are documented in
`stdlib/README.md`. The `crypto/sha256` execution test uses the unchanged public
`Sum256([]byte) [32]byte` implementation selected with the `purego` build tag.
Its wrapper, FIPS implementation,
byte-order helpers, and `math/bits` dependencies are lowered into the same cg12
module as the importing program. The test hashes `"abc"` at runtime and checks a
fingerprint covering all 32 expected digest bytes; host hashing and generated
SHA substitutes are not used.

The repository also contains an unchanged copy of the Go 1.26.1 `runtime`
package. On ARM64, the normal executable path compiles that runtime through
cg12 even when the program does not import `runtime`, boots the scheduler through
`runtime.main`, and runs package initialization tasks recorded in module data.
The runtime execution test allocates traced structs, grows stacks, runs the
standard collector, verifies live heap objects, and exits with background
runtime threads active.

The ARM64 Go path uses ABIInternal register assignment for scalar and aggregate
arguments and results. Build-selected Plan 9 assembly is parsed into a syntax
tree and translated to GNU AArch64 syntax. The currently enabled unchanged
standard-library files are `runtime/atomic_arm64.s`, `runtime/memclr_arm64.s`,
`runtime/memmove_arm64.s`, and all five ARM64 files in `internal/bytealg`:
`compare_arm64.s`, `count_arm64.s`, `equal_arm64.s`, `index_arm64.s`, and
`indexbyte_arm64.s`. The compiler also adapts ABI0 stack operands to its
ABIInternal call path, allowing the exact `internal/cpu/cpu_arm64.s`,
`internal/runtime/sys/dit_arm64.s`, and
`internal/runtime/syscall/linux/asm_linux_arm64.s` files to compile and run.
The same ABI0 adapter compiles `syscall/asm_linux_arm64.s`, replacing the
handwritten syscall shims with the copied standard-library implementations.
The exact `internal/chacha8rand/chacha8_arm64.s` implementation is also active,
including its multiline `QR` macro, local frame, SIMD structure operations,
and `DATA`/`GLOBL RODATA` constant tables.
The unchanged `internal/runtime/atomic/atomic_arm64.s` file replaces the
handwritten atomic primitives. Its conditional LSE paths, exclusive-access
fallbacks, and generated `go_asm.h` offset constants are compiled directly;
eligible leaf ABI0 routines use a direct register adapter so runtime atomics do
not acquire an extra call frame.
Unsupported files are kept out of the build until the translator accepts every
construct they contain.

The runtime-assembly demo grows slices, validates their copied contents, clears
memory, runs the collector, and prints a checksum:

```sh
go build -o /tmp/goc ./cmd/goc
/tmp/goc -o /tmp/runtime-assembly-demo goc/testdata/runtime_assembly.go
/tmp/runtime-assembly-demo
# ABIInternal + Plan 9 assembly: 256 32896 3072
```

```sh
go run ./cmd/goc -emit-ir program.go
go run ./cmd/goc -O -c program.go
go run ./cmd/goc -run program.go
```

## Goals

- **Library-first.** cg12 is designed to be embedded. You construct IR through a
  fluent Go API (`f.Add(ir.ClsW, a, b)`), not only by parsing text. The textual
  IL is a debugging and interop format, not the primary interface.
- **QBE-compatible IL.** The same four base classes (word/long/single/double),
  block/phi/jump structure, and a compatible textual syntax, so QBE's corpus and
  documentation carry over.
- **Idiomatic and small.** Operations are polymorphic over class rather than
  duplicated per class, keeping the opcode set and the passes compact.
- **Beyond QBE.** The near-term target is arm64 (AAPCS64); the architecture is
  built to extend toward more aggressive optimization, additional targets, and
  direct in-memory machine-code emission.

## Status

| Component | State |
|-----------|-------|
| `ir` — SSA IR, builder API, textual printer | ✅ implemented, 100% covered |
| `analysis` — CFG, dominators, SSA liveness | ✅ implemented, 100% covered |
| `arm64` — SSA destruction, AAPCS64 ABI, linear-scan regalloc, emit | ✅ integers **and floats**, runs natively, 96% covered |
| `opt` — mem2reg, GVN, GCM, alias analysis + load elim, fold/copy/DCE/CFG, **inlining** + dead-func elim, pass manager | ✅ implemented, 96% covered |
| `parse` — textual IL lexer + parser (round-trips with the printer) | ✅ implemented, 92% covered |
| arm64 stack args, aggregates (GP/HFA/by-ref/x8), unions, variadics, large frames | ✅ implemented |
| `wasm` — WebAssembly backend: locals, structured control flow, linear memory, calls (direct/indirect), aggregates, variadics, WAT emit | ✅ runs on wasmtime, 95% covered (external/WASI calls pending) |
| `arm64/a64` — direct A64 instruction encoder (integer, float, loads/stores, branches), validated byte-for-byte vs a reference assembler | ✅ implemented, 94% covered |
| `obj` — ELF64 relocatable-object writer (symbols, .text/.data, relocations) | ✅ links + runs with `ld`/`gcc` |
| `arm64.CompileObject` — the arm64 backend: IR → A64 bytes → ELF `.o`, **no external assembler** | ✅ integers, floats, calls, tail calls, variadics, aggregates, and data; `CompileObjectAndAssembly` also returns translated GNU assembly sidecars |
| `link` — partial linker: merges objects from IR modules and `.o` files, resolves cross-object references | ✅ merges .text/.data, symbol resolution, PC-relative branch resolution |

## Example

```go
m := ir.NewModule()
f := m.NewFunc("add", ir.ClsW).Export()
a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
e := f.Entry()
e.Ret(e.Add(ir.ClsW, a, b))
fmt.Print(m)
```

```
export function w $add(w %a, w %b) {
@start
	%t1 =w add %a, %b
	ret %t1
}
```

## Compiling to machine code

The default path emits a linkable ELF object directly — no external assembler:

```go
m := ir.NewModule()
f := m.NewFunc("sumto", ir.ClsW).Export()
// ... build a loop with phis ...
obj, err := arm64.CompileObject(m) // ELF .o bytes for AArch64
os.WriteFile("sumto.o", obj, 0o644)
```

`CompileObject` runs the whole pipeline in-process — SSA destruction, the AAPCS64
ABI, linear-scan register allocation, A64 instruction encoding (`arm64/a64`), and
the ELF writer (`obj`) — and covers integers, floats, direct/indirect/tail calls,
variadics, aggregates, and data (including pointers via relocations). The `.o`
links with `aarch64-linux-gnu-gcc` (or native `cc`/`ld` on an arm64 host). The
backend's end-to-end tests link and run real programs — recursion, loops,
spilling, calls — natively.

When the module carries source positions, `CompileObject` also emits DWARF line
info (`.debug_line`/`.debug_info`/`.debug_abbrev`) directly — a debugger can step
the generated code with no assembler in the loop.

To read the generated code, disassemble it:

```go
o, err := arm64.CompileToObject(m)
fmt.Print(arm64.Disassemble(o)) // A64 assembly, with the relocated symbols named
```

That is the same code that runs, read back out of the object rather than
rendered a second time, so it cannot drift from it. It is meant for reading and
not for feeding to an assembler: it names symbols without declaring them. `a64`
also has the arch-only `a64.Disasm(word)` under it, which is checked against the
very asm/word pairs the encoders are validated with, read the other way round.

## Debug info

Instructions and blocks carry an optional source position (`ir.SrcPos` — a file,
line, and column), set on the builder with `b.At(pos)` and interned into the
module's file table with `m.File(name)`. The backend turns these into real
DWARF: `CompileObject` emits the `.debug_line`/`.debug_info`/`.debug_abbrev`
sections (with relocations) directly. Its `.debug_info` is a
full compilation unit — a subprogram DIE per function (name, PC range, frame
base, return type, and typed formal parameters) over base-type DIEs — so a
debugger can set breakpoints by name, produce backtraces, show signatures, and
step by source line. Inlining is preserved too: the inliner records provenance
on every spliced instruction, so inlined code emits `DW_TAG_inlined_subroutine`
DIEs (nested for nested inlines) with an abstract-instance origin and the call
site's file and line — a debugger reconstructs the inline call stack. Parameters
carry a `DW_AT_location` — a `.debug_loc` location list giving the register or
frame slot the value occupies over its live PC range — so a debugger can print
argument values. Positions round-trip through the textual IL as `dbgfile`/
`dbgloc` directives:

```
@start
	dbgfile "prog.src"
	dbgloc 10 3
	%t1 =w add %a, %b
	dbgloc 11 5
	%t2 =w mul %t1, %a
	ret %t2
```

## Architecture-specific IR: register variables

Low-level runtime code — a stack switch, a trampoline, the copying half of a
growable-stack `morestack` — needs to touch machine registers directly. A
**register variable** binds an IR variable to a specific physical register:
reading it (a `Load`) reads that register, writing it (a `Store`) writes it.

```go
sp := f.RegVar("sp", int(arm64.SP))
cur := e.Load(ir.ClsL, sp)                 // read the stack pointer
e.Store(e.Sub(ir.ClsL, cur, f.Long(64)), sp) // move it down 64 bytes
```

This makes the IR deliberately architecture-specific (the register number is the
backend's), and it is the primitive that lets the *entire* fixup-and-switch logic
of a growable stack be written in cg12 IR rather than hand assembly: allocate,
copy, walk the frames, adjust pointers via the stack maps, then read/write `sp`
to switch stacks. The non-allocatable registers (`sp`, `fp`, `lr`, the platform
register) are always safe to bind; binding a register the allocator uses is the
author's responsibility. Register reads and writes are volatile — the optimizer
never removes, reorders, or CSEs them. In the textual IL they are `getreg`/
`setreg`.

## Intrinsics

A low-level primitive that has no internal control flow — reading the stack
pointer, an atomic add, a memory fence, a byte swap — is an **intrinsic**: a
named operation carried by one generic `intrinsic` instruction, rather than its
own opcode. In the tree today are `stacksave`/`stackrestore` (which bracket a
variable-length array) and the atomic family (`atomic.load.l`, `atomic.cas.w`,
`atomic.fence`, …, with the operation and width in the name):

```
%sp =l intrinsic stacksave
intrinsic stackrestore %sp

%old =l intrinsic atomic.add.l %counter, 1
```

An intrinsic is registered with a description of its **effects** — whether an
unused one is dead, whether two equal ones may be shared, whether it moves, and
whether it touches memory — so the optimizer (DCE, GVN, GCM, load elimination,
alias analysis) reasons about it correctly without knowing what it does. The
printer, parser, binary format, and verifier all handle it by name, so adding
one is: register its effects, teach the interpreter to run it, and teach each
backend that supports it to lower it. See
[docs/intrinsics.md](docs/intrinsics.md) for the step-by-step guide.

## Linking

The `link` package combines relocatable objects into one, from either front-end:

```go
l := link.New()
l.AddModule(irModule)      // compile IR and add
l.AddObjectFile(dotO)      // parse an architecture .o and add
merged, err := l.Link()    // one relocatable object
```

Both inputs become the same in-memory object model — an IR module through
`arm64.CompileToObject`, an `.o` file through `obj.ReadELF` (the inverse of the
ELF writer). The linker concatenates their `.text`/`.data`, resolves symbols
across them (erroring on a duplicate definition), and re-bases their
relocations. Cross-object references it can settle now — PC-relative branches
whose target is in the combined `.text` — are patched directly into the
instruction and their relocations dropped; the relative distance is final
regardless of where `.text` is ultimately loaded. Everything else (absolute,
page-relative, and TLS relocations) is carried forward for a final link. The
result is another relocatable object, so a system linker can finish the job.

## Garbage-collector stack maps

The same liveness-and-location data that drives DWARF also feeds GC stack maps.
Mark a value as a managed heap reference with `f.ParamRef(name)` or
`f.MarkGCRef(ref)` (an `ir.Temp.GCRef` flag that survives pointer lowering).
`CompileObject` then emits a `.cg12_stackmaps` section: for every safepoint, the
address (via a relocation) and the location — register or frame-pointer offset —
of each managed reference live there. A garbage collector finds the table through
the `__cg12_stackmaps` symbol and uses it to locate (and, for a moving collector,
relocate) roots. Roots are precise: the allocator's per-value live ranges exclude
a call's own arguments and result, and only `GCRef`-flagged values are reported.

Each root carries a **type descriptor** (`f.MarkGCRefType(ref, typeID)`) into the
stack map, so the runtime knows how to process the pointer — which fields to
scan, and whether it may point into the stack (which a copying stack collector
must adjust).

Calls are safepoints implicitly. For the ones that are *not* calls — an inlined
allocation fast path, a loop back-edge poll — mark them with `b.Safepoint()`, an
`OSafepoint` marker that emits no code but pins a stack map at its PC. It
round-trips through the textual IL as `safept`.

### Growable stacks

A strategy can also emit a **prologue stack-growth guard** (Go-style growable
stacks) by implementing `EmitPrologue`. The built-in `StackGrowth` strategy emits
a check against a per-thread stack limit at the start of every function; if the
frame would overflow, it preserves the argument registers, calls a runtime
routine that reallocates and copies the stack, and re-checks:

```
mrs  x16, tpidr_el0 ; ...              ; load the per-thread stack limit
mov  x17, sp ; sub x17, x17, #frame
cmp  x17, x16
b.hi ok                                ; enough space
stp  x29,x30,... ; stp x0,x1,... x6,x7 ; preserve fp/lr and arg registers
mov  x0, #frame ; bl morestack ; ...   ; grow + copy, then
b    <retry>                           ; re-check from the top
ok:  <normal prologue>
```

Because of register variables, the runtime side of this is written in cg12 IR
rather than assembly, and the backend's tests exercise the *whole* mechanism —
compiled, linked, and run natively:

- A **stack switch** (`run_on_stack` saves `sp`, switches it to a new region,
  runs a function there, and switches back).
- A **map-driven pointer fixup engine** (`gc_move` reads its frame pointer via a
  register variable, walks to the caller's frame and its return address, looks
  that PC up in `__cg12_stackmaps`, and relocates each interior stack pointer).
- A complete **copying stack growth** (`grow`, in IR): allocate a larger stack,
  `memcpy` the frames, precisely relocate the saved frame-pointer chain (each
  frame's saved `x29` that still points into the old stack, adjusted by the move
  delta), and return the delta; the growing frame then adds it to `sp`/`x29` to
  continue on the new stack. In the end-to-end test a mutator running on a small
  heap stack grows onto a larger one and continues correctly — and the old stack
  is *poisoned* after copying, so the relocation's correctness is a hard
  requirement, not an accident of stale-but-valid data.

So `morestack` — including the stack switch that people usually write in
assembly — is expressible and working in cg12's own IR.

The typed stack maps are exactly what the copying runtime needs to fix up
pointers into the stack as it moves them, and the growth call carries an
argument pointer-map (the growing function's managed-reference parameters,
described in the guard's save area) so pointer arguments can be fixed up before
the frame exists.

The backend's tests include a proof-of-concept collector: a cg12 function holds
an interior pointer to one of its own stack slots (`f.MarkGCRefType(alloca,
stackType)`) across a call; a C "stack mover", invoked at that safepoint, walks
the frame-pointer chain — past intermediate frames — looks up each frame's typed
stack map, relocates what an interior pointer points at, and rewrites the spilled
pointer. The mutator then transparently uses the updated pointer, while heap
roots are left untouched. It demonstrates that the emitted metadata is sufficient
to find and update every spilled pointer a moving stack must adjust.

To keep the consuming side simple, a managed reference that is live across a
safepoint is spilled to a stack slot for its lifetime, so every root is reported
at a frame-pointer offset — a collector never has to chase a value through
callee-saved registers. The backend's tests include a small reference collector
that, invoked at a safepoint, walks the frame-pointer chain, looks up the return
address in `.cg12_stackmaps`, reads the root from the caller's frame, and
recovers the exact live pointer — proving the emitted data is directly usable.

### Pluggable GC strategy

The GC-specific *code* — the poll that turns an `OSafepoint` into a real stop, a
write barrier, an inlined allocation fast path — is emitted by a **strategy
plugin**, late, during machine-code emission, so neither the frontend nor the
backend's ordinary instruction selection needs to know about the collector.
Pass one via `arm64.CompileObjectWith(m, arm64.Options{GC: strategy})`; a
strategy implements `EmitSafepoint(*GCContext)` and emits instructions through
the context (raw words, scratch registers, symbol materialization, runtime
calls, and the live-root list). The built-in `PollStrategy` lowers each safepoint
to a cooperative poll:

```
adrp x16, gc_poll_flag        ; materialize the flag address
add  x16, x16, :lo12:gc_poll_flag
ldrb w16, [x16]               ; load the "collection requested" flag
cbz  w16, +8                  ; not set -> skip
bl   gc_safepoint             ; set -> call the runtime; stack map is here
```

The IR only carries `b.Safepoint()`; the strategy supplies the code.

### Thread-local variables

GC state is per-thread — the poll flag, a thread-local allocation buffer. The IR
addresses thread-local variables with `f.ThreadSym(name)`, and the backend emits
the platform TLS sequence (local-exec on AArch64: `mrs tpidr_el0` plus `tprel`
adds, with the symbol typed `STT_TLS`). `PollStrategy{ThreadLocal: true}` puts
the poll flag in thread-local storage, and `GCContext.TLSym` lets any strategy
address per-thread state. The backend's tests link against C `_Thread_local`
variables and verify per-thread isolation with pthreads.

## Machine-code encoding (no external assembler)

The arm64 backend currently emits GNU-assembler text. To remove that dependency,
`arm64/a64` encodes AArch64 instructions **directly to bytes** — every A64
instruction is a fixed 32-bit little-endian word, and each function returns that
word:

```go
a64.AddReg(true, 0, 1, 2)   // add x0, x1, x2  -> 0x8b020020
a64.Fadd(false, 0, 1, 2)    // fadd s0, s1, s2
a64.Stp(true, 29, 30, a64.SP, -16, a64.PreIndex) // stp x29, x30, [sp, #-16]!
```

The encoded set covers everything the backend emits: integer ALU, moves, division
and shifts, multiply, bitfield extends, conditional select, the full scalar
floating-point set (arithmetic, compare, conversions, `fmov`), loads/stores
(including pairs, signed sub-word, and FP), `adrp`, and all branches.

A small `Program` assembler resolves label-relative branches (forward and
backward) and produces the final `.text` bytes. Correctness is proven by
**assembling the same instructions with a reference assembler and comparing bytes
in the tests** — the reference is used only to validate, never at runtime.

`obj` writes those bytes into an **ELF64 relocatable object** (`.o`) — a
hand-rolled ELF writer with a symbol table, string tables, and relocations (e.g.
`R_AARCH64_CALL26` for external calls). The object links with a standard linker
and runs: the tests build an object, link it with `gcc`, and execute it, and also
parse it back with Go's `debug/elf` to validate the structure. Wiring the arm64
backend to emit through this (instead of assembler text) is the remaining step.

## Caching units to disk

A module encodes to a compact, versioned binary format so a compiled unit can be
cached and reloaded without rerunning the front end and optimizer:

```go
data, _ := m.MarshalBinary()   // cache the optimized unit
os.WriteFile("unit.cg12", data, 0644)
// ... later ...
m, err := ir.DecodeModule(data) // rejects a stale/foreign cache (magic + version)
```

Pointer references (blocks, aggregate types) are encoded as indices; integers use
varints. The encoding is deterministic, so units are content-addressable, and a
decoded unit is a complete module — it compiles to the same assembly and runs.

## Layout

```
ir/         SSA intermediate representation + builder API + textual printer
analysis/   CFG, dominators, dominance frontier, SSA liveness, loop nesting
opt/        SSA passes: mem2reg, GVN, GCM, alias analysis + load elim, fold/copy/DCE/CFG
parse/      textual IL lexer + recursive-descent parser (inverse of the printer)
arm64/      AArch64 backend: SSA destruction, ABI lowering, regalloc, emit  (int + float)
arm64/a64/  AArch64 machine-code encoder: instructions -> bytes, no external assembler
wasm/       WebAssembly backend: SSA temps -> locals, dominator-based control-flow structuring, WAT emit
difftest/   differential corpus: cg12 unopt vs opt vs QBE (when present)
```

## A second target: WebAssembly

The `wasm` package shows the architecture extending to a fundamentally different
machine: WebAssembly is a *typed stack machine with structured control flow*, not
a register machine. So this backend has **no register allocation** — SSA
temporaries map directly onto typed Wasm locals (`w/l/s/d` → `i32/i64/f32/f64`) —
and it restructures the arbitrary CFG into `block`/`loop`/`if` using a
dominator-tree relooper (each loop header becomes a `loop`, each merge point a
`block`, phis resolve to parallel local copies on each edge). It emits the
WebAssembly text format (WAT), which `wasmtime` runs directly:

```go
m, _ := parse.Parse(ilSource)
opt.OptimizeModule(m)
wat, _ := wasm.CompileModule(m)   // a (module ...) form
```

End-to-end tests compile real functions — arithmetic, conversions, loops, nested
loops with branches, and parallel phi swaps — and run them through `wasmtime` to
check results. Memory works too: `alloc` is backed by a shadow stack in linear
memory (Wasm's operand stack is not addressable), loads/stores map to
`i32.load`/`i64.store`/etc. at their sub-word widths, globals are laid out as
data segments with resolved symbol addresses, and direct calls enable recursion.

The full call ABI is implemented on top of that: **indirect calls** through a
function table (a function pointer is its table index) with inline
`call_indirect` signatures; **by-value aggregates** by reference — an aggregate
parameter is an i32 pointer, and an aggregate return uses an sret pointer the
caller allocates; and **variadics** via the common convention where the caller
packs the variadic arguments into a frame buffer and passes its address, which
`va_start`/`va_arg` walk.

### Mandatory tail calls

A call can be marked a **tail call** (`b.TailCall(...)`, printed `tailcall`). This
is not a best-effort optimization: the backend must emit a real tail call — one
that reuses the frame — or **error**. That `musttail` contract lets the IR author
guarantee tail-call elimination (unbounded recursion in constant stack) and
learn at compile time when a target cannot honor it, encoding platform
differences at the IR layer. On wasm a tail call is `return_call` /
`return_call_indirect`; on arm64 it is a frame teardown followed by `b`/`br`
instead of `bl`. arm64 tail calls even pass stack arguments — they are written
into the caller's own incoming-argument area, reused after teardown — but only
when the callee's stack arguments *fit* there; a tail call that would need more
stack space than the caller was given is rejected, because honoring it would
corrupt the caller's frame. The author sees these differences where they matter.

### Running QBE's own programs

The textual QBE IL types every pointer as `l` (i64), which does not match
wasm32's i32 addresses. Rather than a lossy pointer-inference pass, the backend
keeps such pointers full-width and **wraps each address to i32 at the
memory-access boundary** — sound, because addresses fit in 32 bits while pointer
values and arithmetic stay correct. Undefined symbols (a C driver's `int a;`)
become zeroed linear memory. With that, real QBE test programs — `collatz`,
`prime`, `max`, `euclid`, and more — parse, compile, and run on `wasmtime`
producing the right answers (see `difftest.TestWASMCorpus`). Programs whose
output goes through `printf` still need an external-call/WASI story.

### Pointers are register-width, by construction

The IR has an abstract pointer class `ClsP`. A backend resolves it once, up front,
to the target's word-register class — `ClsL` on arm64, `ClsW` (i32) on wasm32 —
with `ir.LowerPointers`. That single resolution drives **both** the value
representation and the memory width of a stored pointer, so pointer size and
register size can never diverge: on wasm a pointer is a 32-bit `i32` that occupies
4 bytes in memory, on arm64 a 64-bit value in an x-register occupying 8. The same
IR compiles to both without wasting space on either.

Parse text IL, optimize, and compile:

```go
m, _ := parse.Parse(ilSource)   // QBE-compatible textual IL -> ir.Module
opt.OptimizeModule(m)
code, _ := arm64.CompileObject(m)  // a relocatable ELF object
```

Run the optimizer before lowering:

```go
opt.OptimizeModule(m)          // runs opt.DefaultPipeline()
code, _ := arm64.CompileObject(m)
```

### The pass pipeline

Passes come in two kinds, unified by a small `Pass` interface. Most are
**intraprocedural** (`func(*ir.Func) bool`, returning whether they changed
anything) and enter the pipeline via `FuncPass`, which applies them to every
function. **Inlining** and dead-function elimination are **interprocedural**
(`ModulePass`) — they need the whole module. A `Fixpoint` combinator is itself a
`Pass`, so pipelines nest. `DefaultPipeline()` reads:

```
mem2reg
clean = fixpoint(fold, copy, loadelim, gvn, simplifycfg, dce)
fixpoint(inline, clean)      // inline exposes work; cleanup exposes more inlining
unroll                       // bounded in-place recursion unrolling (once)
fixpoint(inline, clean)      // clean up / inline what unrolling exposed
deadfunc
gcm ; dce                    // schedule once, at the end
```

Inlining processes the call graph **bottom-up over its SCC condensation**
(Tarjan's algorithm): a callee is finalized before its callers inline it, and
recursion — direct or mutual — is detected as SCC membership rather than a local
self-call scan. Non-recursive calls inline freely; recursion is instead
**unrolled in place** to a bounded depth (like loop unrolling of the cycle),
leaving a residual call so the program still terminates.

The order is deliberate: functions are cleaned before inlining so cost estimates
are honest; inlining and cleanup iterate together, because a constant argument or
a now-redundant load only pays off after the callee's body lands in the caller;
and global code motion schedules once, after the algebraic fixpoint settles.
Custom pipelines compose the same building blocks: `opt.Run(m, myPasses)`.

## Testing

```
go test ./...
```

The project aims for maximum test coverage; every package ships with thorough
`testify`-based tests. On a native aarch64 host the backend and end-to-end tests
assemble their output with `aarch64-linux-gnu-gcc` (or `cc`) and run it.

### Differential testing

`difftest/` runs cg12 against two corpora — a small curated set and **QBE's own
upstream test suite** (vendored under `difftest/testdata/qbe/`) — compiling each
program **both unoptimized and optimized** and checking they agree with the
expected result. That opt-vs-unopt comparison continuously verifies the
optimizer is semantics-preserving. Point it at a QBE binary for a true
three-way differential against the reference implementation:

```
CG12_QBE=/path/to/qbe go test ./difftest/
```

cg12 compiles and verifies **all 32** of QBE's test programs — mandelbrot, prime
sieves, N-queens, `strcmp`/`strspn`, Euclid's GCD, all six ABI/struct tests
(struct arguments and returns, unions, HFAs), both variadic tests, `collatz`
(a >4KB stack frame), and `double` (a floating-constant corner case) — matching
QBE (or, where this stale QBE build miscompiles variadics, the recorded expected
output). The harness has already earned its keep, catching a real backend bug
(wrong stack offsets for allocations in non-entry blocks).
