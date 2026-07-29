# Can the Go runtime be compiled separately? — feasibility spike

Branch: `ccwork/sepcompile-spike`, based on `perf/test-suite` (`14abbb8`).
Status: **in progress — findings written as they land.**

## Answer so far (one paragraph)

Yes, with one specific blocker that has to be fixed first. Measured over 12 capability
programs, **99.5% of a program's compiled `.text` bytes and 3742 of its 3751 functions are
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

### Programs measured (12)

`hello.go`, `fmt_sprintf.go`, `stdlib_http_parse_roundtrip.go`, `panic_recover.go`,
`nested_defer.go`, `interface_slice_equality.go`, `reflect_methods.go`,
`runtime_atomic_counter.go`, `runtime_buffered_channel_fifo.go`, `runtime_cleanup_basic.go`,
`runtime_closure_captures_roots_gc.go`, `runtime_array_map_key.go`.

### Result: symbols shared by all 12 programs

`.text` (functions):

| identity | of hello's 3751 funcs | of hello's 1,709,132 `.text` bytes |
| --- | ---: | ---: |
| same name **and** same content (Hash) | 3017 (80.4%) | 766,512 (44.8%) |
| same content, counter-named symbols renumbered (Colour) | **3742 (99.8%)** | **1,700,152 (99.5%)** |

`.data` (sized symbols only — see §1.1 for the unsymbolised metadata blob):

| identity | of hello's 8936 data syms | of hello's 497,088 symbol-covered `.data` bytes |
| --- | ---: | ---: |
| Hash | 6803 (76.1%) | 409,908 (82.5%) |
| Colour | 8415 (94.2%) | 451,564 (90.8%) |

The same invariant core is 61.1% of `fmt_sprintf.go`'s functions and 24.8% of
`stdlib_http_parse_roundtrip.go`'s — those programs are bigger because they pull in more
*stdlib*, not because the runtime changed.

### The nine functions that are not invariant across all 12

Complete list, from `hello.go`:

```
main_main                                    380 B   the user's program
main_init_0                                   84 B   the user's package init
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
