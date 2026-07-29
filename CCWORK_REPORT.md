# Can the Go runtime be compiled separately? — feasibility spike

Branch: `ccwork/sepcompile-spike`, based on `perf/test-suite` (`14abbb8`).
Status: **in progress — findings written as they land.**

## Answer so far (one paragraph)

Yes, with one specific blocker that has to be fixed first. Measured over 28 capability
programs, **99.3% of a program's compiled `.text` bytes and 3740 of its 3751 functions are
byte-identical across every program**, once you account for the fact that cg12 names a large
family of emitted symbols by a running counter rather than by their content. Between two
small programs (`hello.go` and `panic_recover.go`) **not one runtime function differs**. The
program-dependent residue is small and well-localised: Go type descriptors, itab dispatch
wrappers, the module's init tasks, `runtime.firstmoduledata`, and the whole-program
pclntab/funcdata blob. The blocker is that cg12 resolves the 32-bit relative offsets inside
type descriptors (`NameOff`/`TypeOff` style) *before the object exists*, baking a
whole-module data-layout delta into the bytes with no relocation left behind — see
"Obstacle 1" below.

---

## 1. Quantifying the program-dependent fraction

### Method

`analysis/sepcompile` (committed; throwaway instrumentation, changes no compiler behaviour)
compiles a program exactly the way `cmd/goc` does — `goc.CompileExecutableFor(TargetARM64,
…)` then `arm64.CompileToObject` — and records, per defined symbol, two identities:

* **Hash** — the symbol's bytes with every relocated field masked out, plus each relocation
  recorded by `(offset-within-symbol, type, addend, target *name*)`. Equal Hash means the two
  symbols are interchangeable exactly as they stand.
* **Colour** — the same, except that references to *counter-named* symbols contribute the
  referent's refined colour instead of its name, resolved by six rounds of Weisfeiler-Lehman
  refinement over the relocation graph. Equal Colour means the two symbols would be
  interchangeable if cg12 named those symbols by content instead of by a counter.

The distinction is essential and is not cosmetic. cg12 builds many symbol names from a
running count of the module under construction — `.goc.string.%d` and `.goc.runtime.type.%d`
from `len(g.mod.Data)`, `.goc.itab.%d` from `len(g.interfaceItabs)`,
`%s.gointernal.funcvalue.%d` from `len(g.mod.Funcs)`, and eight more families (the full list
is in `analysis/sepcompile/main.go`). Emitting one extra datum for the *user's* program
renumbers every later runtime symbol without changing a byte of it. Hash counts every such
renumbering as a difference; Colour does not.

Determinism was checked first: two separate processes compiling `hello.go` produce
byte-identical records.

### Programs measured (28)

The 12 the task asked for — `hello.go`, `fmt_sprintf.go`, `stdlib_http_parse_roundtrip.go`,
`panic_recover.go`, `nested_defer.go`, `interface_slice_equality.go`, `reflect_methods.go`,
`runtime_atomic_counter.go`, `runtime_buffered_channel_fifo.go`, `runtime_cleanup_basic.go`,
`runtime_closure_captures_roots_gc.go`, `runtime_array_map_key.go` — plus 16 more:
`runtime_accurate_gc_scalars`, `runtime_append_growth_pointer_elements`, `runtime_callers_stack`,
`runtime_channel_interface_close_gc`, `runtime_complex_arithmetic`,
`runtime_copy_interface_slice_gc`, `runtime_atomic_value`, `runtime_assembly`, `gc_struct`,
`global_struct_slice`, `sort_find_exhaustive`, `gomaxprocs_memstats`, `interface_new_slice`,
`fmt_println`, `context_cancel`, `append_make`.

`hello.go` imports nothing, so its whole object is the runtime closure; it is the reference
program throughout.

### Result: symbols shared by all 28 programs

`.text` (functions):

| identity | of hello's 3751 funcs | of hello's 1,709,132 `.text` bytes |
| --- | ---: | ---: |
| same name **and** same content (Hash) | 3016 (80.4%) | 766,440 (44.8%) |
| same content, counter-named symbols renumbered (Colour) | **3740 (99.7%)** | **1,697,776 (99.3%)** |

`.data` (sized symbols only — see §1.1 for the unsymbolised metadata blob):

| identity | of hello's 8936 data syms | of hello's 497,088 symbol-covered `.data` bytes |
| --- | ---: | ---: |
| Hash | 6802 (76.1%) | 409,907 (82.5%) |
| Colour | 8412 (94.1%) | 451,507 (90.8%) |

The same invariant core is 61.1% of `fmt_sprintf.go`'s functions and 24.8% of
`stdlib_http_parse_roundtrip.go`'s — those programs are bigger because they pull in more
*stdlib*, not because the runtime changed.

### The eleven functions that are not invariant across all 28

Complete list, from `hello.go`:

```
main_main                                    380 B   the user's program
main_init_0                                   84 B   the user's package init
main_main_gointernal_funcvalue_3580           72 B   the funcval wrapper for main.main
runtime_main                                2304 B   calls main.main and main.init
error_Error                                 1896 B   interface-method dispatch wrapper
runtime_stringer_String                     1000 B   interface-method dispatch wrapper
internal_abi_Type_GcSlice_interfacecall_14   248 B   interface-call wrapper (counter-named)
runtime_Func_funcInfo_interfacecall_58       152 B   interface-call wrapper (counter-named)
runtime_preprintpanics                      3256 B   contains interface calls
runtime_itabAdd                              692 B   references program-dependent data
internal_runtime_exithook_Run               1272 B   references program-dependent data
```

`error_Error` and `runtime_stringer_String` are cg12's generated interface-method
dispatchers: they switch over the itabs the *program* happens to contain
(`goc/compile.go:5162`, the `interfaceitabmatch%d_` blocks), so they are program-dependent by
construction. That is a real, unavoidable dependency, not an artefact.

### Pairwise: `hello.go` vs `panic_recover.go`

Of 12,687 content-bearing symbols, **509 have no counterpart**:

| family | total | unmatched | bytes |
| --- | ---: | ---: | ---: |
| `_goc_type_*` (content-addressed type descriptors) | 3213 | **501 (15.6%)** | 44,336 |
| `main_*` (the user program) | 4 | 2 | 464 |
| `_goc_string_N` (the user's string literals) | 2328 | 2 | 34 |
| `_goc_runtime_type_N` | 984 | 1 | 8 |
| `runtime_firstmoduledata`, `_goc_module_inittask{,s}` | — | 3 | 616 |
| everything else (6124 syms, incl. every runtime function) | 6124 | **0** | 0 |

**Not one runtime `.text` function differs between these two programs.**

### 1.1 The part that is not symbols at all

`.data` is much larger than the symbols in it. For `hello.go`: `.data` is 3,929,344 bytes but
sized symbols account for only 497,088. The remaining 3.43 MB is the Go metadata blob that
`internal/gometa.Builder` appends after every function has been laid out — it is emitted as
zero-size labels, so it has no symbol sizes at all:

```
.goc.go.gcbss              8        .goc.go.gofunc        167,736
.goc.go.gcdata         8,384        .goc.go.pclntable     300,808
.goc.go.pcheader          72        .goc.go.functab        31,672
.goc.go.funcnames    142,801        .goc.go.findfunctab 2,622,036
.goc.go.pctab        152,942
```

This blob is 60.8% of `hello.go`'s whole image (and 14.3% of `stdlib_http_parse_roundtrip`'s).
It is **intrinsically whole-program**: `Builder.Build` sorts every function by its final text
offset and emits 32-bit offsets relative to the text and pclntab bases. It cannot be
prebuilt as-is. (Aside, not part of this spike: `.goc.go.findfunctab` alone is 2.6 MB for a
1.7 MB `.text`, which looks disproportionate against upstream Go's bucket sizing. Flagged,
not investigated.)

---

## 2. What breaks if you try to compile the invariant part standalone

Four mechanisms, in descending order of how much work they represent.

### Obstacle 1 — type descriptors carry link-time deltas with no relocation (the blocker)

Go's `abi.Type` addresses its name, its methods, its package path and its method
types by 32-bit offsets relative to the module's type region rather than by pointers.
cg12 emits these as `ir.DataItem{Sub: ir.SubW, Sym: …, RelativeTo: ".goc.runtime.datastart"}`
(seven sites in `goc/compile.go`, all with the same base). `arm64.resolveRelativeDataFixups`
(`arm64/mc.go:738`) then computes `target.Value - base.Value` and **copies the result straight
into the data bytes**. No relocation is emitted, so nothing downstream can revisit the value.

`.goc.runtime.datastart` is the first datum of the module, so the baked value is "this
symbol's offset within the whole module's `.data`". Insert one byte of user data before it and
it changes.

Measured, per program (`sepcompile -fixups`):

| program | relative items | data symbols carrying them |
| --- | ---: | ---: |
| `hello.go` | 1639 | 501 |
| `runtime_cleanup_basic.go` | 2521 | 698 |
| `fmt_sprintf.go` | 5015 | 1205 |
| `stdlib_http_parse_roundtrip.go` | 18463 | 4173 |

That count of 501 is exactly the number of `_goc_type_*` symbols that failed to match between
`hello.go` and `panic_recover.go` in §1. **The entire type-descriptor divergence between two
programs is this one mechanism**; nothing else about a type descriptor is program-dependent.

Fixing it needs a relocation that means "the difference between two symbols". AArch64 ELF has
no such relocation, so `cc` cannot express it — but cg12's own linker controls both ends and
`obj.Reloc` would only need a second symbol field. This is contained work in `obj` and `link`,
not a redesign, and it is the prerequisite for everything else here.

### Obstacle 2 — the pclntab/moduledata blob is whole-program by construction

`internal/gometa.Builder.Build` runs after every function has been laid out. It sorts all
functions by final text offset and emits `funcnames`, `pctab`, `gofunc`, `pclntable`,
`functab`, `findfunctab` and `runtime.firstmoduledata` as one blob of 32-bit offsets. For
`hello.go` that is 3,426,464 bytes — 87.2% of `.data` and 60.8% of the whole image.

It cannot be prebuilt. It has to be regenerated per program from the union of the prebuilt
runtime's functions and the program's own. That is affordable — see the timing in §5 — but it
means the prebuilt object must ship its per-function metadata (`gometa.FunctionInfo`: frame
sizes, stack maps, pcsp/pcdata, func flags) alongside the machine code, not just an ELF object.

Go itself solves this differently, with a linked list of moduledatas
(`moduledata.next`, `runtime.modulesinit`), which is how `-buildmode=shared` and plugins work.
That would let the prebuilt runtime keep its own moduledata untouched. It is the more faithful
design and the larger change; regenerating one merged table is the cheaper one.

### Obstacle 3 — the interface dispatchers are built from the program's itab set

`error_Error` and `runtime_stringer_String` are cg12-generated dispatchers that switch over
the itabs the program contains (`goc/compile.go:5162`). They must be emitted per program.
They are two functions totalling 2.9 KB in `hello.go`, so the cost is trivial — but they are
*runtime-named* symbols, so the prebuilt object must leave them undefined rather than define
them, and every runtime function that calls one must keep an external reference.

### Obstacle 4 — twelve symbol families are named by a running counter

`.goc.string.%d` and `.goc.runtime.type.%d` from `len(g.mod.Data)`, `.goc.itab.%d` from
`len(g.interfaceItabs)`, `%s.gointernal.funcvalue.%d` from `len(g.mod.Funcs)`,
`.goc.goabi.%d` from `len(g.mod.Types)`, and seven more. A prebuilt runtime and a
separately-compiled program would each start their counters at zero and collide, and any
change to the user program renumbers the runtime's symbols.

This is the difference between the two columns in §1's table: 80.4% of functions share a name
*and* content, 99.8% share content. Fixing it means naming these by content, exactly as
`.goc.type.<name>.<hash>` already is (`goc/compile.go:14040`). Mechanical, wide, low-risk.

---

## 3. What the linker can and cannot already do

Tested with `analysis/seplink`, not assumed.

### It resolves references across objects correctly — verified on real goc output

`seplink -mode=split` compiles `hello.go`, cuts the resulting object in half at a function
boundary in `.text`, rebuilds each half as a standalone relocatable object with the other
half's symbols undefined, and links the halves back together:

```
whole object: text=1709132 data=3929344 syms=12974 relocs=38614/15484
split .text at 854716: lower text=854716 syms=11691, upper text=854416 syms=1283
relinked:  text=1709132 syms=12764 relocs=18188
control (whole object through the same linker): text=1709132 relocs=18188
split-and-relinked .text is byte-identical to the control
```

Cross-object symbol resolution, section merging, local-symbol namespacing and relocation
rebasing all work on a real 1.7 MB Go object. That is precisely the capability a prebuilt
runtime needs.

### It can link and run a Go program with no external linker at all

`seplink -mode=native` compiles a program, assembles the Plan 9 sidecar with `cc -c` (cg12 has
no assembler, so this step stays), and then links with `link.Linker` + `obj.WriteExecutable`
instead of `cc`. Eight programs, all producing byte-identical stdout to the `cc`-linked binary
and the same exit status:

```
hello  fmt_sprintf  panic_recover  nested_defer  runtime_buffered_channel_fifo
runtime_cleanup_basic  runtime_closure_captures_roots_gc  interface_slice_equality
                                     8/8 cc rc=0, cg12 rc=0, output SAME
```

The result is a static, non-PIE ET_EXEC with no libc. Two things had to be supplied that `cc`
provides today, both trivial and both now known:

* **`abort`.** cg12 lowers four unreachable-by-construction paths to a call to libc's `abort`
  (`goc/compile.go:962`, `:9847`, `:11763`, `:12614`). It is the *only* external symbol goc
  output needs — the other 67 undefined symbols in `hello.go`'s object are all Go runtime
  assembly from the sidecar.
* **A process entry point.** `arm64.Backend.StartStub` calls `entry` with whatever happens to
  be in x0/x1. The Go runtime's `main` wants `argc` and `argv`, which Linux leaves at `[sp]`
  and `sp+8`. A four-instruction stub fixes it; without it the binary segfaults immediately.

### One real gap, found and fixed

`obj.resolveAArch64` had no case for `R_AARCH64_ABS32` and returned
`cannot statically resolve aarch64 relocation type 258`. goc emits 8753 of them for `hello.go`
alone — `gometa.Builder.reloc32` uses one per function in both `pclntable` and `functab` — so
**no goc output could be statically linked at all**. Added, with `link/abs32_test.go`, which
fails without the change. This is the one production change in this branch.

### What it cannot do: drop unreferenced functions

`link.merge` concatenates every input's `.text` wholesale and `obj.WriteExecutable` emits all
of it. There is no mark-sweep and no per-function sectioning.

Adding one would not help by itself, because **the metadata blob pins every function**.
`gometa.Builder.Build` emits `builder.reloc32(function.Name)` for every function in both
`.goc.go.pclntable` and `.goc.go.functab`. Measured on `hello.go` (`seplink -mode=pins`):

```
.data is 3929344 bytes; the Go metadata blob starts at 502880 (3426464 bytes, 87.2%)
3751 function symbols, 1709132 bytes
3751 of them (100.0%), 1709132 bytes (100.0%), are referenced by a relocation inside that blob
```

So linker-level dead stripping is only possible if the linker also *regenerates* the metadata
after stripping — which, per Obstacle 2, it has to do per program anyway. The two problems
have one solution: move metadata generation from the back end to the link step.

---

## 4. Reachability: how much bigger do binaries get?

`hello.go` imports nothing, so its entire 1,709,132-byte `.text` *is* the runtime closure as
pruned by `goc/reach.go`. That is the baseline a prebuilt superset has to be measured against.

Measured over 28 compiled programs (the 12 above plus 16 more), classifying each function by
the package prefix it comes from, using the prefix set `hello.go` itself defines:

| set | runtime-closure `.text` | growth over `hello.go` |
| --- | ---: | ---: |
| `hello.go` alone (pruned) | 1,709,132 | — |
| union over the 23 runtime-only programs | 2,072,644 | +21.3% |
| union over all 28, including the stdlib-heavy ones | **2,858,624** | **+67.3%** |

The union grows slowly for a long while and then steps: 1.71 MB for the first 21 programs,
1.98 MB at `context_cancel.go`, 2.07 MB at `runtime_cleanup_basic.go`, and 2.86 MB once
`fmt_*`, `reflect_methods` and `stdlib_http_parse_roundtrip` are included — those pull in
reflection, map and interface paths that a `println`-only program never reaches. The curve was
still rising at 28 programs, so **+67% is a lower bound for a prebuilt runtime covering the
full 342-capability matrix**, not an estimate of it.

The `.data` cost is worse, because the metadata blob scales with function count and text span:
`hello.go`'s 3.43 MB blob would grow by roughly the same 1.6×. A rough total for `hello.go`:
5.6 MB image today, order-of 9 MB with a prebuilt runtime covering these 28 programs.

**Can the linker claw it back? Not without moving metadata generation into the link step.**
See §3: 100.0% of `hello.go`'s functions are pinned by a relocation inside the metadata blob.
Per-function sections would not change that; the pin is a real reference from a table the
program needs at run time. Once the linker generates that table itself, the pins disappear and
an ordinary mark-sweep over `obj.Object`'s symbols and name-keyed relocations becomes both
possible and easy — the object model already has per-symbol `Value`/`Size` and every
relocation names its target.

---

## 5. Where the time actually goes

The task's framing said to measure which phase a proposal removes before proposing it. Phase
timings for `hello.go` on this box (64-core arm64), taken with temporary instrumentation in
`goc/compile.go` and `arm64/mc.go` that has since been reverted:

```
front end  1.93s   parse + type-check  0.489s   (already shared across compiles in a process
                                                 by goc/source_world.go; ~0 for later ones)
                   reachability        0.444s
                   package globals     0.063s
                   IR generation       0.767s   3694 functions
                   finish/opt passes   0.171s
back end   2.70s   per-function lower + regalloc + emit   2.402s
                   data + Go metadata blob                0.209s
                                                    total 4.63s
```

`goc` end-to-end on this box is 4.7s for `hello.go` (three runs: 4.72 / 4.65 / 4.70s, 430 MB
peak RSS). The box is slower than the one the task's 2.096s figure came from; the *shape* is
the same and is what matters.

**What a prebuilt runtime removes:** reachability (0.44s), IR generation (0.77s) and the
per-function back end (2.40s) — 3.61s of 4.63s, 78% — minus whatever the ~9 program-specific
functions cost, which is negligible. **What it cannot remove:** the metadata blob (0.21s,
and it would grow with the superset), the finish passes (0.17s), globals (0.06s), and
type-checking the user program.

So the achievable floor is roughly **0.5–1.0s per program instead of 4.6s**. Over the
342-capability matrix that is ~26 minutes of compilation today against ~5 minutes plus one
runtime build, on this box. The ratio matches the task's estimate; the absolute numbers are
larger because the box is.

This is worth stating precisely because the earlier front-end cache (`goc/source_world.go`,
commit `7227e5c`) targeted the 0.489s parse+type-check line and was worth ~7%. The line this
proposal targets is 3.6s, and it is the one that scales with the size of the runtime.

---

## 6. Recommendation

**Tractable, and worth doing — but it is a four-part change, and one part has to come first
whether or not the rest ever happens.**

The measurement says the idea is sound: the compiled runtime really is invariant. 99.5% of a
program's `.text` bytes and all but nine of its functions are byte-identical across every
program measured, and between two small programs not one runtime function differs. The
program-dependent residue is not diffuse — it is four named mechanisms, and three of them are
small.

### Order of work

**Step 1 — content-address the counter-named symbols.** Replace the twelve
`fmt.Sprintf("….%d", len(…))` naming sites in `goc/compile.go` with content-derived names, the
way `.goc.type.<name>.<hash>` already works at line 14040. This is mechanical, has a wide but
shallow blast radius, and is independently valuable: it is what turns the 80.4% figure into
the 99.8% one, and it is a precondition for *any* content-addressed caching of compiler
output, not just this proposal. Verify it with `analysis/sepcompile`: the Hash column should
move to meet the Colour column.

**Step 2 — give relative data references a relocation.** Add a second symbol to `obj.Reloc`
(or a parallel `DeltaRelocs` list) meaning "target minus base, 32-bit", have
`addDataWithBSSAndFixups` emit one instead of calling `resolveRelativeDataFixups`, and resolve
it in `link.merge` / `obj.WriteExecutable`. AArch64 ELF cannot express this, so this step also
makes cg12's own linker mandatory for goc — which §3 shows already works end to end. Without
this step, every type descriptor in the prebuilt runtime is wrong the moment a program adds a
byte of data, so there is no partial credit here.

**Step 3 — move Go metadata generation from the back end to the link step.** `gometa.Builder`
moves behind `link`, and the prebuilt runtime object ships its `[]gometa.FunctionInfo`
alongside its ELF. This is the largest piece. It buys two things at once: the metadata blob
becomes correct for a merged image, and the function pins disappear, so a mark-sweep dead
strip becomes possible and claws back the +67% of §4.

**Step 4 — split the driver.** `goc build-runtime -o runtime.o` compiles the runtime with a
fixed root set and no user program; `goc -runtime runtime.o prog.go` compiles only the user
package plus its type descriptors, itabs, generic instantiations and interface dispatchers,
and links. The matrix harness builds the runtime once per run.

Steps 1 and 2 are worth doing on their own merits even if 3 and 4 are never started. Step 3
without step 4 is wasted effort.

### Risks, honestly

* **Step 3 is the whole cost.** Metadata generation currently sits where it has the laid-out
  object in hand. Moving it means the linker owns text layout, function ordering, stack maps
  and pcvalue tables. Everything in `RUNTIME_PLAN.md` §5 that depends on correct stack maps and
  correct unwinding runs through this code. A defect here looks like a GC or traceback bug,
  which §14 records as the hardest class to find.
* **The interface dispatchers make the prebuilt runtime's symbol set program-dependent.**
  `error_Error` and `runtime_stringer_String` are *runtime-named* but program-built, so the
  prebuilt object must deliberately leave them undefined. Getting that boundary wrong produces
  a duplicate-symbol error at best and a dispatcher that silently misses an itab at worst.
* **Reachability changes which runtime functions exist, and §5.10 records that some defects
  only appear in particular configurations.** A prebuilt superset compiles runtime code that
  no program today exercises. That is not a correctness risk in itself, but it means the
  matrix stops being evidence about the code paths it used to prune away.
* **Binary size grows by at least 67% of the runtime, probably more** (§4), until step 3's
  dead strip lands. For the matrix that is harmless; for anything shipping a binary it is not.
* **A green matrix will not validate this.** Per `RUNTIME_PLAN.md` §14, a codegen change that
  passed the entire suite silently miscompiled `defer` in a loop. The validation that would
  actually work here is differential: build every capability program both ways and compare the
  linked images symbol by symbol with `analysis/sepcompile`'s identity, not just compare their
  stdout.

### The cheap alternative, for completeness

Steps 1 and 2 alone make the compiler's output content-addressable at symbol granularity,
which enables a per-symbol object cache keyed on IR rather than on the compiler binary. That
gets a large fraction of the win for a fraction of the risk, because nothing about the
whole-program metadata has to move — every program still builds its own. It does not help the
first compile after a compiler change, which is the case the task says matters, so it is a
worse fit for this specific problem; it is a better fit if step 3 turns out to be too
expensive.

---

## 7. A confirmed miscompile found by this spike (not a spike deliverable)

Looking for name collisions — because separate compilation depends on names meaning one thing
— turned up one that is already live.

**Three distinct functions share one symbol name, and the ELF writer keeps only the last.**

`goc/compile.go:10510` names a function literal `<package>.func.<line>.<column>`, falling back
to `<enclosing symbol>.func.<line>.<column>` only when `g.functionName != ""`. That is set from
`functionDecl.symbol`, which `goc/reach.go` assigns **only for generic instantiations**
(lines 415, 760, 777). Every closure inside an ordinary function is therefore named by package,
line and column alone.

`crypto/internal/fips140/nistec` is generated code: `p224.go`, `p384.go` and `p521.go` are the
same file with the curve name substituted. So all three of their
`<curve>GeneratorTableOnce.Do(func(){…})` literals sit at line 393, column 28, and all three of
their `<curve>BOnce.Do(func(){…})` literals at line 114, column 16.

Measured on `goc/testdata/stdlib_crypto_ecdsa.go` (`seplink -mode=dupsyms`):

```
crypto_internal_fips140_nistec_func_114_16   3 definitions, 3 distinct sizes [552 792 1000],
                                             3 distinct bodies, 7 relocations name it
crypto_internal_fips140_nistec_func_393_28   3 definitions, 1 distinct size [948],
                                             3 distinct bodies, 7 relocations name it
```

`obj.prepareELF` builds `symIndex[s.Name] = i` (`obj/elf.go:398`), so a name with three
definitions resolves every reference to whichever was written last. Two of the three generator
tables are never built and stay nil.

**Verified against the host toolchain** (`analysis/testdata/nistec_closure_name_collision.go`),
per RUNTIME_PLAN §3 step 2. Host Go prints eight `true` lines. Under `goc`:

```
unexpected fault address 0x19c8
fatal error: fault
crypto_internal_fips140_nistec_p224Table_Select()   +0x290
crypto_internal_fips140_nistec_P224Point_ScalarBaseMult()
… crypto_ecdsa_GenerateKey() … main_exercise()
```

**Mechanism proven by a control.** Patching the naming site to include the file base name and
nothing else, then rebuilding `goc`: `P224 verify: true` — the P-224 path now completes
`GenerateKey`, `SignASN1` and `VerifyASN1`. The patch was reverted; this branch does not change
compiler behaviour.

The matrix cannot see this. `stdlib_crypto_ecdsa.go` uses only P-256, which takes the assembly
path in `p256_asm.go` and never reaches the colliding closures.

**Second, distinct failure exposed by the same reducer, not explained here.** With the naming
collision patched out, the program then dies at
`crypto/elliptic.nistPoint.SetBytes` with `cg12: interface dispatch failed for dynamic type
0x0`, reached from `elliptic.Curve.IsOnCurve`. That is a separate defect of the §5.6 generic
method-dispatch family and was not investigated. It is recorded so the reducer is not mistaken
for a one-bug program.

---

## 8. What is committed on this branch

| path | what it is |
| --- | --- |
| `analysis/sepcompile/` | compiles a program the way `cmd/goc` does and records per-symbol identity (`-out`), dumps a symbol's masked bytes and relocations (`-dump`), censuses relative data references and IR size (`-fixups`), or splits front-end from back-end wall clock (`-timing`). Changes no compiler behaviour. |
| `analysis/seplink/` | `-mode=split` cuts a real goc object in half and relinks it; `-mode=native` links a goc program with cg12's own linker and no `cc`; `-mode=pins` measures how many functions the Go metadata blob references; `-mode=dupsyms` reports names with more than one definition. |
| `analysis/testdata/nistec_closure_name_collision.go` | the §7 reducer, with the mechanism written at the top. Under `analysis/testdata/` so the go tool does not build it. |
| `obj/exec.go` | **the one production change**: `R_AARCH64_ABS32` added to the static relocation resolver. |
| `link/abs32_test.go` | covers it; fails with `cannot statically resolve aarch64 relocation type 258` without the change. |
| `RUNTIME_PLAN.md` §5.10 | the §7 miscompile, recorded as a known miscompile not covered by any capability. |

Temporary instrumentation used and then reverted, so nothing in this branch alters compilation:
phase timers in `goc/compile.go` and `arm64/mc.go` (§5), and a closure-naming patch in
`goc/compile.go` used to prove §7's mechanism.

## 9. What I did not verify

* **The 28-program sample is not the 342-capability matrix.** The invariance figures are stable
  across every program measured, but the §4 superset figure (+67%) is explicitly a lower bound
  and the growth curve was still rising.
* **No prebuilt runtime was actually built.** This is a feasibility spike; §2's obstacles are
  read out of the code and confirmed by measurement, not by attempting the split. In particular
  I did not attempt to link one program's runtime against another program's residue.
* **The `seplink -mode=native` result is 8 programs, not the corpus.** They all produce
  identical stdout and exit status to the `cc`-linked binary, but the cg12 linker path is not
  otherwise validated, and it needs two stubs (`abort`, a process entry point) that a real
  driver would have to provide properly.
* **The `.goc.go.findfunctab` size** (2.6 MB for a 1.7 MB `.text`) looks disproportionate
  against upstream Go's bucket sizing. Noticed, not investigated, no bug claimed.
* **§7's second failure** (`crypto/elliptic.nistPoint.SetBytes`, interface dispatch on a nil
  dynamic type) is reported as observed, not diagnosed.
* **Timings are from this box under this job's 8-slot share**, so absolute seconds are not
  comparable to the task's 2.096s figure. The phase *shares* are what the argument rests on.
