# Making type-descriptor offsets survive separate compilation — design spike

Branch: `ccwork/typeoff-alternatives`, based on `perf/test-suite` (`a8d8d51`).
Status: **in progress — findings written as they land.**

## Recommendation in one paragraph

**Give each separately compiled object its own `moduledata` and let its type offsets stay
relative to its own base — Go's own answer, option 2.** The decisive fact is that this
requires *no change at all* to how cg12 computes the offsets: `arm64.resolveRelativeDataFixups`
already resolves `value(target) - value(datastart)` against symbols in the same object, and
that number is *already correct* for a per-module base. What is missing is only the second
half of Go's design — a second `moduledata` chained onto `runtime.firstmoduledata` — and the
runtime machinery for that (`modulesinit`, `moduledataverify`, `typelinksinit`, `itabsinit`,
`resolveNameOff`/`resolveTypeOff`/`resolveTextOff` picking the module that contains the
*referring* type) is already vendored, already compiled into every goc binary, and already
called from `schedinit`. **A working two-module goc image is committed on this branch**
(`analysis/typeoff`): a separately compiled object contributes Go type descriptors whose data
lands 4,397,856 bytes from where it was compiled, and `reflect` reads its name, package path,
fields, `PtrToThis` and method metadata correctly — at four different insertion offsets. The
same object linked under today's single flat type region prints garbage and then throws
`runtime: name offset out of range`. The rejected delta-relocation is not needed, and neither
is any new relocation type.

---

## 1. The problem, restated precisely

`abi.Type` addresses its name, package path, method names and method signature types by
signed 32-bit offsets. cg12 emits each as
`ir.DataItem{Sub: ir.SubW, Sym: X, RelativeTo: ".goc.runtime.datastart"}` (seven sites in
`goc/compile.go`: 5622, 5718, 5719, 5759, 5777, 5778, 5808).
`arm64.resolveRelativeDataFixups` (`arm64/mc.go:738`) computes `value(X) - value(datastart)`
after the object is laid out and copies the number into the data bytes. **No relocation is
left behind**, so no later stage can revisit it.

A *uniform* shift of the whole `.data` block is harmless — both operands move together — so
the failure mode is narrower and more specific than "the data moved":

> **The baked value is wrong exactly when bytes are inserted *between*
> `.goc.runtime.datastart` and the type symbol.** That is what happens when a second,
> separately compiled object contributes type descriptors: its own base is at offset 0 of
> *its* object but at offset N of the merged image, and every offset it baked is N too small.

This is not a hypothesis. §4 reproduces it, with the exact failure text.

### The option space, stated as a taxonomy

The value `target − base` must be produced by *somebody* who knows the final layout. There
are only four places that knowledge can come from, and every candidate design is one of them:

| | who knows the layout | what it costs |
| --- | --- | --- |
| **A** | the linker resolves it later | needs the fixup recorded ⇒ a "difference of two symbols" relocation — **the rejected design** |
| **B** | the compiler is told the final base | option 1 (layout ordering): one number, silent if wrong |
| **C** | nobody needs a global base | **option 2 (per-module regions) — recommended** |
| **D** | the ABI stops needing a base | option 3 (self-relative/PREL32), or "absolute in 32 bits" (§3.4) |

Option 3 as posed — "section-relative addressing with the existing ELF vocabulary" — turns
out not to be a fifth entry: §3.3 shows it collapses into B or D depending on how it is
spelled.

---

## 2. What is already true in the tree (all read from the code, not assumed)

* **The vendored runtime is fully multi-module.** `runtime.moduledata` has `next`
  (`stdlib/src/runtime/symtab.go:450`); `modulesinit` walks the chain and builds
  `activeModules` (`symtab.go:544`); `moduledataverify` verifies every module
  (`symtab.go:606`); `typelinksinit` builds a cross-module `typemap`
  (`stdlib/src/runtime/type.go:437`); `itabsinit` iterates `activeModules()`
  (`stdlib/src/runtime/iface.go:259`).
* **All four already run in every goc binary.** `schedinit` calls them unconditionally at
  `stdlib/src/runtime/proc.go:874–883`. Today the chain simply has length one.
* **The resolvers pick the base from the referring pointer, not from a global.**
  `resolveNameOff` (`type.go:294`) and `resolveTypeOff` (`type.go:328`) both do
  `for md := &firstmoduledata; md != nil; md = md.next { if base >= md.types && base < md.etypes {...} }`
  where `base` is the address of the type descriptor doing the referencing. That *is* the
  per-module mechanism, in the tree, working.
* **cg12 sets `types`/`etypes` to the whole data section**: `internal/gometa/builder.go:248-249`
  writes `.goc.runtime.datastart` and `.goc.runtime.dataend`. So "the type region" and "the
  module's data" are the same range today. Nothing about the scheme depends on the type
  descriptors being contiguous or in a section of their own.
* **`link.merge` concatenates each object's `.data` in input order** at
  `alignUp(len(out.Data), o.DataAlign)` (`link/link.go:225-238`). Section ordering across
  objects is therefore already controllable, by input order. Nothing needs to be added for
  that.
* **`moduledata` is 592 bytes with `types` at 296, `etypes` at 304, `typelinks` at 360,
  `hasmain` at 536 and `next` at 584** on a 64-bit target. Verified by compiling the struct
  against the host toolchain and printing `unsafe.Offsetof`, not by counting fields by eye.
* **goc images are `-no-pie`** (`cmd/goc/main.go:242`, `goc/corpus_test.go:4317`), so every
  address in the image is a link-time constant. This matters to §3.4.

---

## 3. The alternatives, and why each is or is not the answer

### 3.1 Option 1 — layout ordering

*Claim.* If the linker lays the prebuilt runtime's data first and contiguously, the runtime's
own offsets never move, because nothing is inserted before them.

**The claim is true, and it is only half the problem.** The runtime's offsets are safe under
any scheme that keeps its data first — including doing nothing at all — because `datastart`
and every runtime type symbol shift together. The half that ordering does *not* fix is the
program's own descriptors: the program object's base is at offset 0 of its object and at
offset N of the image, so all of its offsets are N too small. Ordering does not change N; it
only makes N predictable.

To use that predictability the *compiler* must be told N, because the value is baked with no
relocation and the linker cannot revisit it. So option 1 is really: **pass the prebuilt
runtime's `.data` length into `goc`, and add it inside `resolveRelativeDataFixups`.** That is
genuinely small — one number, not a symbol table, because every `RelativeTo` target is
co-emitted with the type that names it (§3.2 proves this), so the compiler never needs to
resolve a target it does not itself define.

Objections, honestly:

* **The invariant is silent.** Nothing checks it. If any object contributes `.data` between
  the runtime and the program — the Plan 9 assembly sidecar that `cc -c` produces, the
  `_start` stub, a future `.o` — every offset in the program is wrong, and the symptom is a
  wrong type *name*, not a crash. That is the §14 failure class: a change whose symptom is
  invisible to the matrix.
* **The compiler's output becomes a function of a link-time artifact.** `goc` would produce
  different bytes for the same source against a different prebuilt runtime. That is not fatal
  but it is the opposite direction from commit `14abbb8` (deterministic compilation), which
  is what makes any of this verifiable.
* **It buys nothing else.** The whole-program pclntab/moduledata blob (the previous spike's
  Obstacle 2, and its "largest piece" of work) is untouched.
* It also needs the compiler to replicate the linker's alignment policy
  (`alignUp(runtimeDataLen, thisObjectDataAlign)`) exactly — linker policy leaking into the
  back end.

**Verdict: workable, cheapest of the "flat region" family, but it trades a silent-failure
mode for a saving that option 2 gets anyway.**

### 3.2 Option 2 — per-module type regions — RECOMMENDED

*Claim.* Each module resolves its own offsets against its own base, so nothing ever needs
adjusting.

**True, and stronger than it looks: the offsets need no new code at all.** A per-module base
is exactly what `resolveRelativeDataFixups` already computes, because `RelativeTo` is resolved
against a symbol in the same object. Today `.goc.runtime.datastart` happens to be the first
datum of the only module; under a split it is the first datum of *its* module, and the same
arithmetic is right for the same reason.

The one structural precondition is that **a NameOff/TypeOff must never cross a module
boundary**, because `resolveTypeOff` throws if `md.types + off` runs past `md.etypes`. I
checked all seven emission sites against that rule:

| site | target | co-emitted with the type? |
| --- | --- | --- |
| `compile.go:5622` | `<type>.name` | yes — `emitRuntimeName` immediately above |
| `compile.go:5718` | `<type>.imethod.N.name` | yes |
| `compile.go:5719` | `ensureTypeTag(method.Type())` | yes — emitted by this compile |
| `compile.go:5759` | `<type>.uncommon.pkgpath` | yes |
| `compile.go:5777` | `<type>.method.N.name` | yes |
| `compile.go:5778` | `ensureTypeTag(method.signature)` | yes |
| `compile.go:5808` | `typeTags[*T]` (`PtrToThis`) | yes — `populateRuntimePointerTypes` over one compile's map |

**Every TypeOff/NameOff target is produced by the same `ensureTypeTag` call that produces the
descriptor naming it.** So the rule is satisfied by construction, and the only way to break it
would be to deliberately dedup one of these targets against the other module. Everything else
a type descriptor points at — `GCData`, `Equal`, `StructField.Name`, `StructField.Typ`,
`SliceType.Elem`, `PtrType.Elem`, `MapType.Key`, the interface `imethods` array pointer — is an
8-byte pointer relocation, and pointers cross modules freely. That is why the program module
can still reference the prebuilt runtime's `type:int` directly rather than duplicating it.

The cost of the rule is that a program module re-emits a descriptor for any *signature* type
or *pointer* type it shares with the runtime module. Two descriptors for one Go type means
`reflect.TypeOf(x) == reflect.TypeOf(y)` can disagree. Go's own fix for that is
`moduledata.typelinks` + `typelinksinit`'s `typemap`, which the vendored runtime has and cg12
currently leaves as `builder.emptySlice()` (`internal/gometa/builder.go`, the `// typelinks`
line). **Populating `typelinks` is the one genuinely new piece of work this option adds**, and
it is bounded: a `[]int32` of module-relative offsets, one per named type.

#### Does it dissolve Obstacle 2 (the whole-program pclntab)? Substantially, yes.

The previous spike's Obstacle 2 was that `gometa.Builder.Build` sorts *all* functions by final
text offset and emits one blob of 32-bit offsets, so it "cannot be prebuilt" and metadata
generation would have to move into the linker — its step 3, "the largest piece".

Per-module regions remove the premise:

* `gometa.AddObjectMetadata` already builds the blob **per object**, from that object's
  own function list and that object's own `.goc.runtime.datastart`/`dataend`
  (`internal/gometa/builder.go:57-80`). A prebuilt runtime object would build its own once,
  at `goc build-runtime` time, and ship it. A program object builds its own from its own
  handful of functions.
* The runtime looks a PC up per module: `findmoduledatap` walks the chain testing
  `datap.minpc <= pc && pc < datap.maxpc` (`symtab.go:362`), and `moduledataverify` verifies
  each module independently (`symtab.go:606`).
* `runtime.main` runs init tasks per module (`proc.go:258`, `doInit(m.inittasks)`), and
  `itabsinit` adds itabs per module (`iface.go:259`).
* `moduledata.text` is already 0 in cg12 ("text base: function entry offsets contain absolute
  addresses", `internal/gometa/builder.go:236`), so a second module's text needs no rebasing.

So metadata generation does **not** have to move into the linker. It keeps running where it
has the laid-out object in hand, which is where the previous spike said the risk was
concentrated. **This makes option 2 worth materially more than the offset fix alone** — it
removes the step the previous spike called both the largest and the most dangerous.

**This is not argued from the source alone — the prototype runs it.** With `-functions`, the
second module carries real code and names its moduledata `runtime.firstmoduledata`, which is
what makes `arm64.finishGoModule` hand it to `internal/gometa`; the module then gets a
generated `pcHeader`/`funcnames`/`pctab`/`pclntable`/`functab` of its own. The program calls
into it through a Go func value and asks `runtime.FuncForPC` about a PC that only the second
module's tables describe:

```
foreign-func:_goc_probe_entry     <- runtime.FuncForPC -> findfunc -> the second module's pclntab
foreign-call:7                    <- the call actually ran the second module's code
```

So `moduledataverify1`, `modulesinit` and `findfunc` all accept a second module with real
functions and a real generated pclntab, in a cg12 image.

Two things this turned up that a real implementation will hit, both small and both concrete:

* **`internal/gometa`'s text-end symbol is a global constant.** `textEndSymbol =
  "runtime_gocTextEnd"` (`internal/gometa/builder.go:20`) bounds `maxpc`/`etext` and is the
  `functab` sentinel. The Plan 9 sidecar defines it once for the whole image, so a second
  module would take the *first* module's text end as its `maxpc` and `findfunc` would never
  select it. It has to become per-module. (The prototype works around it by defining a local
  `runtime.gocTextEnd` in the second object, which `link.merge` namespaces and rewrites that
  object's references to.)
* **A per-module dead-strip is still out of reach.** The pclntab is per-*module*, not
  per-program, so the previous spike's §4 argument ("100.0% of functions are pinned by the
  metadata blob") still applies *within* the prebuilt runtime module. Per-module regions make
  the metadata affordable; they do not by themselves let the linker drop unreferenced runtime
  functions.

### 3.3 Option 3 — section-relative addressing

*Claim.* Standard ELF can express "offset within a section" via a relocation against a section
symbol, so if the type region were its own section the existing vocabulary would suffice.

**This one does not hold, and it is worth being exact about why.**

* **There is no AArch64 ELF relocation meaning "the offset of S within its own section".**
  ELF `ABS` relocations are absolute: `R_AARCH64_ABS32` against a section symbol computes
  *section address* + addend, which is an address truncated to 32 bits, not an offset. The
  section symbol does not make the result relative; it only names where the addend is measured
  from *in the input*.
* **Upstream Go's `R_ADDROFF` is not an ELF relocation.** It lives in Go's own object format
  and is resolved by `cmd/link` itself; it is never handed to an external linker. So *even
  upstream Go cannot express this in ELF* — it resolves it internally, because its linker owns
  the layout. That is the honest reading of the rejected design: it is not an invention, it is
  what Go does, and the reason cg12 cannot copy it is that cg12 resolves the value in the back
  end and keeps no record.
* **The one thing standard AArch64 ELF does offer is `R_AARCH64_PREL32` (261): `S + A − P`.**
  That is genuinely expressible and genuinely useful — but it computes *target minus the
  address of the patched word*, not *target minus a module base*. To recover today's
  semantics you must set the addend to the patched word's offset from the module base, which
  the compiler knows only relative to its *own* object. So PREL32 needs exactly the same
  external number as option 1 — it collapses into B. Its only gain over option 1 is that the
  *target* is resolved by the linker, which would matter if cross-object TypeOffs were needed;
  §3.2 shows they are not.
* The other spelling — redefine `NameOff`/`TypeOff` to be self-relative, so PREL32 with a zero
  addend does everything — works mechanically, but changes the meaning of `internal/abi`'s
  offset types and requires editing `resolveNameOff`, `resolveTypeOff`, `resolveTextOff` and
  every `reflect` reader in the vendored tree. That is forking the Go ABI inside a vendored
  upstream tree, which is a much larger commitment than the relocation it saves.

**Verdict: rejected. The relocation people picture does not exist; what does exist (PREL32)
does not remove the dependency it is supposed to remove.**

### 3.4 A fifth option the code suggests: absolute-in-32-bits

cg12 already does this for `TextOff`. `moduledata.text` is written as literal `0`
(`internal/gometa/builder.go:236`, "text base: function entry offsets contain absolute
addresses"), and the method entries at `compile.go:5779-5780` are emitted as
`{Sub: ir.SubW, Sym: X}` with no `RelativeTo` — i.e. an ordinary `R_AARCH64_ABS32` relocation
whose resolved value is the low 32 bits of the target's address. The runtime's
`md.text + off` then yields the address because the base is zero.

The same trick applies to `NameOff`/`TypeOff`: **delete `RelativeTo:` from the seven sites and
write `moduledata.types = 0`.** Every offset becomes a standard `R_AARCH64_ABS32` relocation
the linker resolves, correct across any number of objects, with no module list and no new
vocabulary. It is by a wide margin the smallest diff of any option here — nine lines.

Why I am not recommending it:

* **It relies on an address-space assumption nothing checks.** `resolveTypeOff` selects a
  module with `base >= md.types && base < md.etypes`; with `types = 0` that test matches *any*
  pointer below `etypes`. The `md == nil` branch is what makes runtime-created types
  (`reflect.FuncOf`, `StructOf` — reached by `reflect.Type.Method`, which the corpus exercises)
  fall through to the negative-id `reflectOffs` table. It keeps working only because Go's heap
  arenas are mapped far above the static image, so a heap type still fails `base < etypes`.
  True today, unchecked, and a silent wrong-answer if it ever stops being true.
* It caps the image at 2 GiB and requires a fixed base. Both already hold (`-no-pie`), but
  it extends that dependency from `TextOff` to every type descriptor.
* It makes `moduledata.types` a lie relative to upstream, which is a real cost in a tree whose
  runtime is vendored upstream source and read against upstream constantly.
* It does nothing for Obstacle 2.

**Verdict: the right fallback if option 2 proves too big, and worth recording precisely
because it is so cheap. Not the recommendation.** I did not implement or test it; it is stated
as a reading of the code, not a measured result.

### 3.5 The rejected delta relocation, for completeness

It is unnecessary. Option 2 delivers correct offsets with no relocation at all, because the
compile-time resolution is already module-local-correct. The delta relocation's only unique
capability is a NameOff/TypeOff that crosses a module boundary, and §3.2's table shows cg12
never emits one.

---

## 4. The prototype: a working two-module goc image

`analysis/typeoff` builds a goc program plus a **separately compiled** object that carries Go
type descriptors of its own, links them with cg12's own linker (no `cc` for the link — the
`analysis/seplink -mode=native` path), and runs the program.

* `analysis/typeoff/probe.go` builds the second module as an ordinary `ir.Module` and compiles
  it with `arm64.CompileToObject`. **No compiler code was changed to make this work.** Every
  offset in it is written as `ir.DataItem{Sub: ir.SubW, Sym: X, RelativeTo: ".goc.probe.datastart"}`
  — the same construct as goc's seven sites — and the back end bakes
  `value(X) - value(.goc.probe.datastart)` into the bytes with no relocation, exactly as it
  does for a goc program. The module never learns where it will land.
  It carries a `runtime.moduledata` of its own whose `types`/`etypes` bound its own data, plus
  the minimum pclntab (`pcHeader` + one-entry `functab`) that `moduledataverify1` requires of a
  module with no functions.
* `analysis/testdata/typeoff_probe.go` is compiled by goc as any capability program is. It
  reads a package-level word the link step patches with the foreign type's address, forges an
  interface value around it, and asks `reflect` about it. Every line it prints is read through
  `resolveNameOff`, `resolveTypeOff` or `resolveTextOff`.
* The link step makes exactly two edits, both ordinary `R_AARCH64_ABS64` data relocations:
  the program's `probeSlot` gets the foreign type's address, and
  `runtime.firstmoduledata.next` (offset 584) gets the second module's `moduledata`.

### Result — `-mode=permodule` (the proposed scheme)

```
typeoff: probe module: 1073 bytes of .data, datastart at 0, Widget at +200
typeoff: probe module type region spans [+0, +1072)
typeoff: merged .data is 4398929 bytes; program base at 0, second module's base at 4397856
         (shifted 4397856 bytes from where it was compiled)
name:Widget                 <- Str, a NameOff into the second module
string:Widget
kind:25                     <- Struct
size:8
pkgpath:probe               <- UncommonType.PkgPath, a second NameOff
fields:1
field0:A 6                  <- StructField.Name/.Typ: pointers, which cross modules freely
ptr:*probe.Widget           <- PtrToThis, a TypeOff into the second module
ptr-elem:Widget
methods:1
method:Poke 19 1 0          <- Method.Name (NameOff), .Mtyp (TypeOff), .Tfn (TextOff)
probe: done
typeoff: exit status 0
```

Identical output at `-pad=0`, `-pad=8`, `-pad=4096` and `-pad=100001` (shift 4,397,856 /
4,397,864 / 4,401,952 / 4,497,864 bytes). The offsets do not depend on where the module lands
— which is the property the whole spike is about.

Because `firstmoduledata.next` is non-nil, this run also exercises `moduledataverify` over two
modules, `modulesinit` building a two-entry `activeModules`, `typelinksinit`'s full body
(including allocating the second module's `typemap`, which `resolveTypeOff` then consults on
every lookup) and `itabsinit` over both modules. None of that is stubbed.

### Control — `-mode=flat` (today's single type region) must fail, and does

Same probe object, byte for byte; the only difference is that the second `moduledata` is not
chained and the program's `etypes` is stretched to cover both objects — i.e. one flat region
spanning two separately compiled contributions, which is what the current design would produce.

```
name:<garbage>
string:<garbage>
kind:25
size:8
pkgpath:
fields:1
field0:A 6
ptr:runtime: nameOff 0x63007c out of range 0x630000 - 0xa61f50
fatal error: runtime: name offset out of range
  runtime_resolveNameOff() ... reflect_rtype_String() ... main_main()
typeoff: exit status 2
```

Note what the failure looks like: the *kind*, *size* and *pointer-reached field* are all
correct, and only the offset-addressed fields are wrong. Two of them are wrong *silently* —
`name` and `pkgpath` return garbage rather than throwing, because `md.types + off` happens to
land inside the region. That is the failure mode option 1 would leave latent, and it is why
"the matrix is green" would not be evidence here.

### A defect this spike turned up: the first function of every goc module is nameless

Building the second module with exactly one function printed `foreign-func:` with an empty
name. The mechanism is exact:

* `internal/gometa` lays the first function's name at offset **0** of `.goc.go.funcnames`
  (`builder.go:120-124`), with no leading sentinel byte.
* `runtime.moduledata.funcName` returns `""` for a name offset of 0
  (`stdlib/src/runtime/symtab.go:758-763`), and `funcname` goes straight through it
  (`symtab.go:1146`). Upstream Go's linker reserves offset 0 for exactly this reason.

So **the function at text offset 0 of any goc module has no name in any traceback,
`runtime.Caller`, or `runtime.FuncForPC` result.** For `analysis/testdata/typeoff_probe.go`
that function is `internal_runtime_cgroup_stringError_Error_interfacecall_0`; the tool prints
it. Confirmed both ways: with one function the name came back empty, and adding a filler
function ahead of it made the probe function's name resolve.

Severity is low (one nameless frame per module, in diagnostics only) and it is unrelated to
this spike's question, so I did not change `internal/gometa` for it — a codegen change on a
spike branch is exactly what RUNTIME_PLAN §14 warns about. It is recorded in RUNTIME_PLAN
§5.10.

### Reproducing

```
go build -o /tmp/typeoff ./analysis/typeoff
/tmp/typeoff -mode=permodule -pad=100001            analysis/testdata/typeoff_probe.go
/tmp/typeoff -mode=permodule -functions             analysis/testdata/typeoff_probe.go
/tmp/typeoff -mode=flat      -pad=0                 analysis/testdata/typeoff_probe.go
```

---

## 5. Cost estimate for the full change

Sizing the *remaining* work for a `goc build-runtime` split under option 2, given that the
offset mechanism itself needs nothing:

| piece | size | notes |
| --- | --- | --- |
| content-address the counter-named symbols | the previous spike's step 1, unchanged | unchanged prerequisite; independently valuable |
| export type/name symbols so the program object can point at the runtime's by pointer | small | they are local `ir.Data` today (`Linkage.Export` unset) |
| emit the program module's `moduledata` under a name other than `runtime.firstmoduledata` | small | that symbol belongs to the runtime object; `arm64/go_metadata_object.go` currently matches the name literally |
| chain `firstmoduledata.next` at link time | trivial | one `R_AARCH64_ABS64` data relocation — the prototype does it in 5 lines |
| set `hasmain` on the module that has `main` | trivial | `modulesinit` swaps on it (`symtab.go:566`); gometa zeroes the whole tail today |
| populate `moduledata.typelinks` | **the real new work** | currently `builder.emptySlice()`; needed so duplicate signature/pointer descriptors keep one identity |
| split the driver (`goc build-runtime` / `goc -runtime runtime.o`) | the previous spike's step 4, unchanged | unchanged |
| move metadata generation into the linker | **deleted** | §3.2: per-module regions remove the need |

**Implication for the previous spike's steps 3 and 4.** Step 3 ("move Go metadata generation
from the back end to the link step", described there as the largest piece and the one whose
defects "look like a GC or traceback bug") is **not required**. It is replaced by chaining a
second `moduledata`, which is a link-step edit of one pointer. Step 4 (splitting the driver) is
unchanged and becomes the dominant remaining cost.

What step 3 also bought — linker-level dead stripping, worth the previous spike's +67% size
growth — is **not** delivered by option 2. The metadata blob still pins 100% of its own
module's functions. If the size growth matters, step 3 comes back as an independent,
optional piece; if it does not (and for the capability matrix it does not), it can be dropped
entirely.

---

## 6. Verification status

| check | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | **clean** |
| `make test-unit` | **pass** |
| `make test-goc-corpus` | pending |
| `make test-goc-cmd` | **pass** |
| capability matrix | pending |
| goc compilation determinism unchanged | **pass** — two separate processes produce byte-identical images (no compiler code was changed) |

---

## 7. What I did not verify

* **Unwinding *through* a second module's frame.** `-functions` proves `findfunc` and
  `funcname` work across modules for a PC in the second module, and that its code runs, but
  the probe function is a leaf that returns immediately: no traceback walks its frame and no
  GC scans its stack. The pcsp/stack-map half of a second module's pclntab is therefore
  generated but not exercised.
* **`typelinks`/`typemap` doing real dedup work.** The prototype's second module has empty
  `typelinks`, so `typelinksinit` runs its full body but finds nothing to canonicalise. The
  duplicate-descriptor identity problem is identified and its fix named; it is not exercised.
* **No prebuilt runtime was built**, and no goc program was compiled against one. This is a
  design spike with a mechanism prototype, as scoped.
* **Option 3.4 (absolute-in-32-bits) was not implemented or run.** Its analysis is read from
  the code.
* **The second module's data is not scanned as GC globals** (its `[data, edata)` is empty by
  construction), which is correct for the prototype because every pointer it holds is to
  static data, but a real program module would set that range properly.
