# Adding an intrinsic

An **intrinsic** is a named low-level primitive invoked by the `OIntrinsic`
instruction. It exists so that a new primitive — reading the stack pointer,
prefetching a line, a byte swap, a memory fence — does *not* need its own opcode
threaded by hand through the verifier, both interpreters, the optimizer, and
every backend. Instead an intrinsic is a **name** plus a **description of how it
behaves**. The name dispatches its execution and its lowering; the description
(its *effects*) is what the optimizer reads to reason about a call it does not
otherwise understand.

Prefer an intrinsic over a new `Op` whenever the operation is a leaf primitive
— it takes operands and produces a result (or a side effect) but has no internal
control flow the passes need to see. Reach for a real opcode only when a pass has
to pattern-match on the operation itself (as GVN and instruction selection do for
`OAdd`, `OCmp`, loads, and stores).

The intrinsics in the tree today are `stacksave`/`stackrestore` and the atomic
family (`atomic.load.l`, `atomic.add.w`, `atomic.cas.l`, `atomic.fence`, …). Read
`stacksave`/`stackrestore` end to end as a worked example of a lowered intrinsic
(`ir/intrinsic.go`, `interp/ops.go`, `interp/bc_exec.go`, `arm64/select.go`,
`amd64/xselect.go`); read the atomics (`ir/intrinsic.go`, `interp/atomic.go`) as
an example of a whole *family* — the operation and access width encoded in the
name, effects that make every one a barrier, and interpreter-only semantics with
backend lowering still to come.

## What you get for free

Because `OIntrinsic` carries its name generically, these need **no change** when
you add an intrinsic — they already handle any name:

- **Printing and parsing.** `%r =cls intrinsic NAME arg, arg` (with a result) or
  `intrinsic NAME arg, arg` (void). Round-trips through the textual IL.
- **The binary unit format.** The name is serialized with the instruction.
- **Verification.** `Verify` rejects an `OIntrinsic` with no name, or a name no
  one registered — so a typo is a diagnostic, not a backend panic.
- **The optimizer.** DCE, GVN, GCM, load elimination, and alias analysis all read
  your effect descriptor. You describe the intrinsic once; every pass obeys it.
- **The bytecode compiler.** `interp/bc_emit.go` packs any intrinsic's name,
  result register, and operand registers into a side table generically.

So the work of adding an intrinsic is: **register its effects**, teach the
**interpreter** how to run it, and teach each **backend** that supports it how to
lower it.

## Step 1 — Register it (required)

Add a `RegisterIntrinsic` call in `ir/intrinsic.go`'s `init` (or call it at
startup from wherever you introduce the intrinsic). This is the single source of
truth the optimizer reads, so it is required even if you never optimize — `Verify`
rejects an unregistered name.

```go
// A pure byte-swap: its result is a function of its one operand, with no memory
// or state dependence.
RegisterIntrinsic("bswap", ir.IntrinsicEffects{HasResult: true, Pure: true})
```

### The effect descriptor

`IntrinsicEffects` fields are chosen to match exactly what the passes consult.
The zero value describes an intrinsic that does nothing observable — so anything
that touches state **must** say so, or the optimizer will treat it as dead and
movable.

| Field | Set it when… | What reads it |
|-------|--------------|---------------|
| `HasResult` | the intrinsic defines a value (in `Instr.To`) | printing, general shape |
| `SideEffect` | executing it matters even if the result is unused (moves the stack pointer, hits a device, affects control) | DCE keeps it; it never moves |
| `Pure` | the result is a function of the operands alone — no memory, no mutable state | GVN may share two equal calls; GCM may reschedule it |
| `ReadsMemory` | it reads tracked memory | forbids `Pure`; it is not reordered across writes |
| `WritesMemory` | it writes/clobbers tracked memory | load elimination clears cached loads, like a call |
| `EscapesArgs` | a pointer operand may become reachable elsewhere (stored, returned, handed off) | alias analysis; when false, the pointed-to storage stays local across the call |

Rules of thumb:

- **`Pure` is mutually exclusive** with `SideEffect`, `ReadsMemory`, and
  `WritesMemory`. A pure intrinsic is the strongest promise: shareable and freely
  movable.
- `Pure` is honored by GVN only for intrinsics with **≤ 2 operands** (the value
  key canonicalizes two). A pure intrinsic with more operands is still correct —
  it simply is not value-numbered.
- Impure-but-not-side-effecting is a real point (it is where `stacksave` sits):
  the result is *not* a stable function of the operands (so it is never shared or
  moved), yet an unused one has no effect and DCE removes it.

## Step 2 — Run it in the interpreter

There are two interpreters and they must agree. Each has a single switch to
extend; the surrounding dispatch is generic.

**Tree-walker** — `interp/ops.go`, in `execIntrinsic`:

```go
case "bswap":
	v, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return err
	}
	fr.vals[in.To.ID] = intVal(in.Cls, int64(bits.ReverseBytes64(v.u64())))
	return nil
```

**Bytecode VM** — `interp/bc_exec.go`, in the `bcIntrin` case. The operand and
result registers are already resolved into `intrinInfo` for you:

```go
case "bswap":
	v := mc.regs[base+uint32(ii.args[0])]
	mc.regs[base+uint32(ii.dst)] = intVal(w.cls(), int64(bits.ReverseBytes64(v.u64())))
```

An intrinsic the interpreter does not recognize traps at run time (naming
itself), so you only add cases for the ones you want to interpret.

## Step 3 — Lower it in each backend that supports it

Each backend selects an `OIntrinsic` by name. Add a case to the inner switch;
compile-time behavior for names a backend does *not* handle is a clear error
naming the intrinsic.

**arm64** — `arm64/select.go`, in `selectData`'s `case ir.OIntrinsic`:

```go
case "bswap":
	d, done := s.dst(in.To, in.Cls.Size())
	s.b.rev(in.Cls.Size() == 8, d, s.src(in.Args[0], 1, in.Cls.Size()))
	done()
```

**amd64** — `amd64/xselect.go`, in `selectInt`'s `case ir.OIntrinsic`, in the
same shape (`s.gpDst`/`s.gpValue`).

The emit helper you call (`s.b.rev` above) must exist on the backend's assembler
interface — add it beside the existing helpers (e.g. arm64's `movFromSP`/`movToSP`
in `arm64/asmb.go`) and, for the object path, an encoder in `arm64/a64` validated
byte-for-byte against the reference assembler. A backend that has no lowering for
your intrinsic falls into the `default`, which fails with the intrinsic's name —
so `wasm` and `bpf` reject `bswap` cleanly until someone adds it, rather than
miscompiling.

## Step 4 (optional) — A convenience builder

The generic builders are always available:

```go
r := b.Intrinsic("bswap", ir.ClsL, x) // result-producing
b.IntrinsicVoid("fence")              // void
```

If the intrinsic is used often, add a named method next to `StackSave`/
`StackRestore` in `ir/build.go`:

```go
// Bswap reverses the byte order of x at cls's width.
func (b *Block) Bswap(cls Cls, x Ref) Ref { return b.Intrinsic("bswap", cls, x) }
```

## Checklist

- [ ] `ir/intrinsic.go` — `RegisterIntrinsic(name, effects)` **(required)**
- [ ] `interp/ops.go` — a case in `execIntrinsic` (tree-walker)
- [ ] `interp/bc_exec.go` — a case in the `bcIntrin` switch (bytecode VM)
- [ ] `arm64/select.go` and/or `amd64/xselect.go` — a case per supporting backend
      (plus the emit helper / `a64` encoder it needs)
- [ ] `ir/build.go` — an optional named builder method
- [ ] a test — register/effects (`ir`), a both-engines run (`interp`), and an
      end-to-end lowering test on any target you lowered it for

Everything else — print, parse, the binary format, verify, and every optimizer
pass — already handles it by name.
