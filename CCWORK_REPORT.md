# `println` dropped the spaces the spec requires — and four other print defects

Branch: `ccwork/println-spacing`, off `main` (`61b96da`). Four commits.

**Verdict: the reported defect is fixed, four more in the same lowering were
found and fixed, and two pre-existing `complex64` defects had to be fixed for
the complex operand not to crash. Everything below has been run here. Two things
are reported rather than fixed and are listed under "Not fixed" at the end.**

## The reported defect

`println("a", 1, true)` printed `a1true`; the host Go toolchain prints `a 1 true`.

The Go specification's table for the two print built-ins reads:

> `println`  like `print` but prints spaces between arguments and a newline at the end

so the separators and the trailing newline are required, not implementation-
defined; only the *rendering of an individual operand* is left open. cg12's
`builtinRuntimePrint` walked `call.Args`, emitted one runtime print call per
operand and a single `printnl`, and never emitted the separators at all.

The host implements the rule in `cmd/compile/internal/walk.walkPrint` by
rewriting the operand list before lowering: insert a `" "` string between
operands, append a `"\n"`, then collapse runs of adjacent constant strings into
one. cg12 now builds the same sequence (`goc/compile.go`, `printOperands` /
`collapsePrintLiterals`), so `println("x", "y", "z")` is a single
`printstring("x y z\n")` here as it is there, and `println("a", 1, true)` is
`printstring("a ") printint(1) printsp() printbool(true) printnl()`.

## The spacing was one of five

The task asked for the whole of `print`/`println` to be checked against the host
rather than just the spacing. Auditing cg12's dispatch against `walkPrint` found
four more, all wrong-answer bugs in valid Go:

| Operand | cg12 printed | host prints |
| --- | --- | --- |
| `[]byte{1,2,3}` | `54422416784368` | `[3/3]0x...` |
| `any(nil)` | `0` | `(0x0,0x0)` |
| `any(42)` | `54422416784416` | `(0x963e0,0xacbf8)` |
| `complex(1,2)` | `54422416784632` | `(1+2i)` |
| `complex64` | `4647714816524288000` | `(3.5+4.5i)` |

Slices now go to `runtime.printslice`, interfaces to `printeface`/`printiface`,
complex to `printcomplex128`/`printcomplex64`.

And the fifth, which is not about an operand at all:

**A print statement took no lock.** `runtime/print.go` states the requirement
outright — *"The compiler emits calls to printlock and printunlock around the
multiple calls that implement a single Go print or println statement"* — and
`runtime.minhexdigits` is documented as protected by it. cg12 emitted neither,
so one statement was a run of unsynchronized `write(2, …)` calls. Eight
goroutines at `GOMAXPROCS=4` printing a thirteen-operand line corrupted **3092,
3035 and 3002 of 3200 lines** across three runs on `main`; the host corrupts
none, and this branch corrupts none. Every runtime diagnostic goes through these
same routines, so every traceback and every `GODEBUG` line was exposed to it.

Two smaller items came with them:

- **Operands are now evaluated before the lock**, as the host does. Before,
  `println("A", f(), "B", g())` where `f` and `g` print interleaved their output
  into the middle of the statement printing them.
- **`runtime.quoted`** (`type quoted string`) routes to `printquoted`.
  `traceback.go:1294` prints goroutine labels through it, so a label containing a
  quote or a newline previously went out raw. Confirmed in the disassembly:
  `runtime_goroutineheader` calls `printstring` 21 times and `printquoted` never
  on `main`; here it calls `printquoted` twice.

Pointer-shaped operands keep going to `printhex` rather than the host's
`printpointer`/`printuintptr`. That is not a difference — both of those are
one-line wrappers around `printhex` with the same value — and it keeps two
routines out of the runtime pack.

## Two `complex64` defects the complex operand exposed

Routing a `complex64` to `runtime.printcomplex64` **segfaulted**, because that
routine does `strconv.AppendComplex(buf[:0], complex128(c), …)`. Both causes are
pre-existing and independent of print; both are fixed here, because otherwise
this change would have turned a silently-wrong `println(c64)` into a crash.

- **`real()` and `imag()` of a `complex64` returned garbage, the same garbage for
  both halves.** A `complex64` is two `float32` halves packed into one 64-bit
  integer, so reading a half is a bitwise reinterpretation between a
  general-purpose and a floating-point register — `ir.OCast`, which lowers to
  `fmov`. cg12 used `ir.OCopy`, which re-types only within one register file.
  `var b complex64 = complex(3.5, 4.5); println(real(b), imag(b))` gave
  `-2.8673504e+25 -2.8673504e+25` where the host gives `3.5 4.5`. Addition,
  subtraction, multiplication and conversion were all wrong with it, since they
  all go through `complex64Parts`/`packComplex64`.

- **`gen.convert` had no complex case at all**, so `complex128(b)` `Copy`d the
  packed pair into a pointer: **SIGSEGV at `0x4090000040600000`**, which is the
  packed `(4.5, 3.5)` bit pattern used as an address.

## Verification

Per RUNTIME_PLAN §15 a green matrix is weak evidence, so the load-bearing
evidence is the host comparison.

### Against the host toolchain (§3 step 2)

**43 of the 46 corpus programs that use `print`/`println` now produce
byte-identical output to the host Go toolchain, against 34 on `main`.** Every
program was run under `go run` and under a goc-built binary, stdout and stderr
merged, exit status compared. Six programs — `allocs_per_run.go`,
`bytes_grow_allocs.go`, `bytes_grow_capacity.go`, `bytes_replace_allocs.go`,
`reflect_methods.go`, `runtime_defer_capture_allocs.go` — differed from the host
only by the missing separator and now match exactly.

The three that still differ are `bytes_grow_compare.go`, `bytes_grow_stats.go`
and `gomaxprocs_memstats.go`. All three print allocation and GC statistics, which
are not expected to match a different compiler's allocator, and §5.10 already
records the first two as varying with scheduling even between identical builds.

Separately, every operand type was compared against the host one at a time:
string constant and variable, `int8/16/32/64`, `uint8/64` at their extremes, rune
constant, `bool`, `float32`, `float64`, `complex64`, `complex128`, pointer,
`unsafe.Pointer`, `uintptr`, channel, map, func, slice, empty slice, nil
interface, nil error, non-nil interface, empty call, single operand, `print`
versus `println`, and `println("")`.

### Reducers landed as capabilities

Three, each of which **passes under the host Go toolchain, passes here, and fails
on `main`**:

- `print-builtin/operand-separation` — the spec rule and the whole operand table,
  byte for byte, with the address-shaped operands checked by shape, plus the
  degenerate operand counts and the evaluation-order rule. On `main`:
  `panic: println wrote "a1true\n", want "a 1 true\n"`.
- `print-builtin/statement-atomicity` — 8 goroutines × 300 rounds at
  `GOMAXPROCS=4`, no two statements may interleave inside a line. Marked
  `exclusive` per `TestRuntimeCapabilityExclusiveClassification`.
- `core-types/complex64-parts` — `real`/`imag`, arithmetic, comparison, both
  conversions, and the packed bit layout, with `complex128` as the control.

**A control build fails the atomicity reducer for the right reason.** Rebuilding
with the separators kept and *only* the `printlock`/`printunlock` emission
removed fails it with `two print statements interleaved inside one line: worker 0
round 3 tail 1 2 worker 37  round 40  tail 51  62  73  84`, three runs out of
three. The check is not decorative.

### Allocation, per the task's §5.3 requirement

Disassembled a linked image and looked for `runtime.newobject` inside every print
routine.

Allocation-free: `printlock`, `printunlock`, `printsp`, `printnl`, `printbool`,
`printint`, `printuint`, `printhex`, `printhexopts`, `printstring`, `printslice`,
`printeface`, `printiface`, `printquoted`, `printpointer`. `printquoted`'s
`[]byte("\"")` conversions stay on the stack — cg12 passes a real stack `tmpBuf`
to `stringtoslicebyte`, so `rawbyteslice` is never reached.

**Not allocation-free: `printfloat32`, `printfloat64`, `printcomplex64`,
`printcomplex128`.** See "Not fixed" below. This is pre-existing for the two
float routines, which were already wired before this change.

`GOC_DEBUG_NOSPLIT=1`, full lists compared (the built-in report truncates at 200
lines, so both sides were regenerated through `opt.AuditNoSplitCalls` directly):
**287 → 359 direct split callees, and every added edge is `printlock` (27),
`printunlock` (27) or `printnl` (18).** No edge removed, nothing unrelated
appeared. Upstream has the same shape — its `printlock` is not `nosplit` either,
and every `nosplit` function that prints calls it.

`cg12checkwb=1`, `cg12checkwb=2`, `GOC_DEBUG_WRITEBARRIER=1`,
`gccheckmark=1,invalidptr=1,clobberfree=1`, `gcshrinkstackoff=1`,
`checkfinalizers=1`, `checkfinalizers=2` and `gctrace=1` are all clean on a
print-heavy allocate-and-collect program and on both new reducers.

### Tracebacks

A three-deep panic traceback reads frame for frame identically to the one `main`
produces, and the frame list matches the host's. `checkfinalizers=2` still prints
its retention path on `runtime_cleanup_frame_retention.go`.

### Suites

| Suite | Result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `make test-unit` | pass |
| `make test-goc-corpus` | pass, 560.9 s |
| `make test-goc-cmd` | pass, 221.2 s (after the fix below) |
| Capability matrix, 4 shards, `-v` | **341 subtests, 340 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP, 0 SKIP** |
| Capability matrix, `-runtime-opt` | 325 PASS, 16 FAIL — **identical failure set to `main`**, see below |
| `scripts/determinism-check.sh` | 4 of 5 byte-identical cold vs warm, twice; `runtime_defer_capture_allocs.go` is the known §5.10 residue |

The matrix census is 338 + 3: every one of the original 338 still reports the
same verdict, and the three added capabilities pass. Counted from
`--- PASS/FAIL/SKIP` subtest lines and cross-checked against the declared
`PASS`/`EXPECTED FAILURE` verdict lines; 341 unique subtest names.

`make test-goc-cmd` initially failed. `TestGoImageCarriesASecondModule` asserted
`"foreign-int-kind:2"` and `"first-call:7"` against a probe that prints them with
`println("foreign-int-kind:", kind)` — the two expectations encoded the
spec-violating bytes. Corrected to `"foreign-int-kind: 2"` and `"first-call: 7"`;
the neighbouring assertions in the same test already carry the space because
their probe lines are single string literals. No test was weakened, skipped or
deleted.

## Not fixed — reported instead

### 1. Four print routines heap-allocate their scratch array

`runtime.printfloat32`, `printfloat64`, `printcomplex64` and `printcomplex128`
each call `runtime.newobject`. This is exactly the §5.3 defect, for the four
routines §5.3 did not reach. Under the host, `go build -gcflags='runtime=-m -m'`
reports **nothing at all** in `runtime/print.go` escaping.

The mechanism: they differ in shape from `printuint`. Where `printuint` does
`gwrite(buf[i:])` — a slice into a callee that does not retain it, which §5.3
taught the escape walk to allow — these do
`gwrite(strconv.AppendFloat(buf[:0], v, 'g', -1, 64))`, passing the derived slice
to a callee that *returns* a slice derived from it. cg12 has no rule for a
parameter that leaks only to its result, so the call site assumes the worst.
Reduced with no runtime source involved:

```go
func passthrough(dst []byte) []byte { return dst }
func viaReturn()  { var buf [20]byte; consume(passthrough(buf[:0])) }  // newobject
func viaDirect()  { var buf [20]byte; consume(buf[:0]) }               // no allocation
```

Not fixed here because closing it means giving the escape walk a "leaks only to
result" summary and continuing the walk from the call expression — an
escape-analysis feature whose blast radius is every function in the tree, and
where a wrong summary stores a stack pointer into the heap. That is the highest-
risk change class in this project and it wants its own validation cycle. It is
recorded in RUNTIME_PLAN §5.10 with the reducer.

I could not produce a *fatal* consequence for it. `GODEBUG=gcpacertrace=1`, which
is the diagnostic that prints floats closest to mark termination, runs clean:
`gcController.endCycle` is called after `setGCPhase(_GCoff)`, so `mallocgc` does
not throw there. The defect stands as unsoundness on `nosplit` and fatal paths
plus unconditional bloat, not as a reproduced crash.

### 2. The `-runtime-opt` arm of the matrix does not link — on `main` too

16 capabilities fail under `-runtime-opt`, every one with the same link error:

```
goc-program-runtime.o: in function `reflect_makeFuncStub_abi0':
undefined reference to `reflect_moveMakeFuncArgPtrs'
undefined reference to `reflect_callReflect_abi0'
undefined reference to `reflect_callMethod_abi0'
```

Thirteen `runtime-packages/reflect-*`, `stdlib-crypto/ecdh-x25519`,
`stdlib-encoding/binary`, `stdlib-encoding/binary-varint`. **Measured on `main`
as well, four shards each: the failure sets are byte-identical** (`main` 322 pass
/ 16 fail; this branch 325 pass / 16 fail, the extra three being the new
capabilities, which pass under `-O` too). Not caused by this work, not fixed
here, recorded in §5.10. Possibly a sibling branch's area.

### 3. `printquoted` is not exercised through a real labelled traceback

It is exercised through a `//go:linkname` probe, which produces
`"with \"quote\"\nand\ttab and é and \U0001f600"` — the exact upstream escaping —
and through the disassembly showing `goroutineheader` calling it. It is not
exercised through a traceback carrying real goroutine labels, because
`runtime/pprof.Do` does not type-check under goc:
`context.Context does not implement context.Context (wrong type for method
Deadline)`. That reproduces identically on `main`, is unrelated, and was not
chased.

### 4. The `printf` path in `builtinPrint` is constructed but not executed

`builtinPrint`'s non-`runtimeAllocation` branch gets the same operand sequence
and therefore the same separators, but goc always compiles with
`runtimeAllocation` on, so nothing in this repository's suites runs it. Only its
construction is shared with the verified path, not its verification.

## Files changed

- `goc/compile.go` — `printOperand`, `printOperands`, `collapsePrintLiterals`,
  `printStep`, rewritten `builtinRuntimePrint` and `builtinPrint`,
  `isRuntimeQuotedType`/`isRuntimeNamedType`; `complex64Parts`, `packComplex64`,
  `complexComponent`, `complexConversion`, `isComplexType`, `convert`.
- `goc/reach.go` — roots the runtime print routines the new lowering can call:
  `printcomplex64`, `printcomplex128`, `printeface`, `printiface`, `printlock`,
  `printquoted`, `printslice`, `printsp`, `printunlock`.
- `goc/testdata/runtime_println_operand_separation.go`,
  `runtime_println_statement_atomicity.go`, `runtime_complex64_parts.go` — new.
- `cmd/goc/runtime_status_test.go` — the three capabilities.
- `cmd/goc/testdata/runtime_coverage_baseline_pending.json` — the three
  capabilities, with the standard "added after the accepted 2026-07-22 baseline
  run" reason. **This file is also `ccwork/coverage-baseline`'s area**; the
  denominator test requires the entries, so they are here, but that job's
  version should win if the two conflict.
- `cmd/goc/permodule_test.go` — two expectations that encoded the missing
  separator.
- `RUNTIME_PLAN.md` — §21; §5.10 loses the `println` item and gains the two
  above; §1's matrix count 338 → 341.
