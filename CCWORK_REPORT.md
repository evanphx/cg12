# Escape analysis: the print-routine allocation, the field-address hole, and the interaction

Branch `ccwork/escape-analysis`, off `main` (`0505d90`). Commit `2724ac7`.

**Status: items 1 and 2 done and measured; item 3 in progress.** This file is
written as results land. The "Still unverified" section at the end is the honest
list at any moment.

---

## Item 1 — the four print routines heap-allocate. Closed.

### Reproduced first, on `main`

The RUNTIME_PLAN §5.10 reducer, with no runtime source involved:

```
viaReturn  newobject=true      // consume(passthrough(buf[:0]))
viaDirect  newobject=false     // consume(buf[:0])
```

Mechanism, read out of `goc/compile.go`: `nonEscapingObjectUse` sends `buf[:0]`
to `valueDoesNotEscapeWithin`, which reaches the call and asks
`parameterDoesNotEscape(passthrough, 0)`. That walks `passthrough`'s body, finds
`return dst`, and `nonEscapingObjectUse` had no `*ast.ReturnStmt` case, so it
fell to `default: return false`. The parameter was therefore "escapes", and
`findEscapingCaptures`' slice branch promoted `buf`.

### The fix

`gen.parameterLeaksOnlyToResult` asks *exactly* the question
`parameterDoesNotEscape` asks — the same use enumeration, the same recursion —
with one difference: a `return` from the summarised function is not an escape.
A caller that gets "yes" continues its walk from the call expression, so the
caller's storage escapes exactly when the call's result does.

Deliberate restrictions, because a wrong summary stores a stack pointer into the
heap:

- Returns are allowed only for the body being summarised, identified by node
  identity (`gen.resultLeakBody`), and a return inside a nested `*ast.FuncLit`
  is rejected because it returns from the literal.
- `parameterDoesNotEscape` and `receiverDoesNotEscape` clear that state around
  their own walks, so a recursive strict query can never inherit it.
- A variadic parameter gets no summary: the argument is an element of a slice
  the callee builds, not the parameter.
- A summary for a function with more than one result is only usable where the
  call is the whole right-hand side of an assignment, because the call
  expression does not stand for one value.

Three supporting rules were needed for the chain through `internal/strconv` to
reach:

1. **`append`'s destination.** `append(dst, x)` copies `dst`'s *contents* into
   the result and never publishes `dst`'s address, so `dst`'s storage escapes
   exactly when the result does. Argument 0 only — an appended *element* is
   stored into storage that may already be reachable from the heap, so it keeps
   the conservative answer. `fmtE`, `fmtF`, `fmtB`, `fmtX` and `formatDigits`
   are all `dst = append(dst, ...)` … `return dst`.
2. **Self-assignment.** `dst = append(dst, b)` re-entered `objectDoesNotEscape`
   for a variable whose uses were already being enumerated, and that returns
   "escapes". It now returns "does not escape": the running enumeration sees
   every route out of that variable, so the assignment opens none.
3. **One right-hand side, several left-hand sides.** `d, _ = formatBits(...)` in
   `AppendUint`. `assignedNodeDoesNotEscapeWithin` matched `Rhs[i]` to `Lhs[i]`,
   which for a single spread right-hand side checked only the first
   destination — a latent hole, now closed by checking every destination. The
   blank identifier discards, so it is a destination nothing reaches.

`GOC_DEBUG_ESCAPE=1` traces every summary answer. It is how the missing link was
found: `internal/strconv.formatBits` was excluded by the single-result
restriction, which failed `AppendUint`, `fmtB` and `genericFtoa` in turn.

### Verified

`goc -emit-ir` on `println(1.5); println(complex(1.5, 2.5))`, before and after,
built from the same tree at the same path:

| function | `main` | this branch |
| --- | --- | --- |
| `runtime.printfloat64` | `call $runtime.newobject` | `alloc8 20` |
| `runtime.printfloat32` | `call $runtime.newobject` | `alloc8 20` |
| `runtime.printcomplex64` | `call $runtime.newobject` | `alloc8 44` |
| `runtime.printcomplex128` | `call $runtime.newobject` | `alloc8 44` |
| `runtime.printDebugLogImpl` | `call $runtime.newobject` | frame |

The `goc_storep` write barrier that published the heap pointer disappears with
it. Every summary the chain needs answers yes:
`AppendFloat`, `AppendComplex`, `AppendInt`, `AppendUint`, `formatBits`,
`genericFtoa`, `bigFtoa`, `formatDigits`, `fmtB`, `fmtE`, `fmtF`, `fmtX`.

**Whole-image diff for that program:** exactly five functions changed, plus the
now-dead `[21]byte` and `[44]byte` type descriptors. 2,737 functions, everything
else byte-identical.

### What this does *not* buy

The prior job's honest limit stands: no fatal consequence was produced. This is
unsoundness on nosplit and fatal paths plus unconditional bloat, not a
reproduced crash.

---

## A live miscompile found while doing item 1

`parameterDoesNotEscape` consulted `g.resultObjects` — the named results of the
function *being lowered* — while walking a **callee's** body. A callee that
assigns its parameter to its own named result and returns bare was therefore
reported as not letting the parameter escape:

```go
var pointerSink *int

func namedLeak(value *int) (out *int) {
	out = value
	return
}

func leakViaNamedResult() {
	value := 3
	pointerSink = namedLeak(&value)   // a package global holds a frame address
}
```

| tree | `leakViaNamedResult` calls `runtime.newobject` |
| --- | --- |
| `main` (`0505d90`) | **no** — `&value` stays on the frame |
| this branch | yes |

This is the §5.8 invariant reached by a third route, distinct from §5.8's own and
from `phase2-alloc`'s field-address route. Closed by `gen.enterCalleeBody`, which
installs the *analysed* function's named results for the duration of the walk.
Guarded by `TestParameterAssignedToACalleeNamedResultEscapes`.

---

## A Go-semantics violation the change exposed

`TestAdvancedExecutionCorpus/append_slice_ellipsis` segfaulted. The
non-runtime `append` growth path was

```
%t40 =p call $realloc(p %t20, l %t39)
```

`realloc` on the old backing array. That assumes the array came from the
allocator — my change legitimately left `values := []int{7}` on the frame — and
it also breaks Go's rule that a growing `append` leaves the old array intact for
any other slice that refers to it. It now allocates a new array and copies, which
is what `runtime.growslice` does on the runtime path.

---

## Item 2 — the field-address hole, ungated. Closed.

`ccwork/phase2-alloc` fixed `nonEscapingObjectUse` returning "does not escape"
for every field selection, but **gated** the fix to the one question
"may this fresh allocation stay on the frame", because ungating it made
`copystack`, `scanstack` and `sweepLocked.sweep` call `runtime.newobject` —
104 of 400 runs died with `failed to set sweep barrier`.

The gate leaves a real hole, which is why it is not reproduced here:

```go
func store(p *record) { sink = append(sink, &p.tag) }
func caller()         { var v record; store(&v) }   // v must not stay on the frame
```

`findEscapingCaptures` asks `addressEscapesFunction(&v)`, which asks
`parameterDoesNotEscape(store, 0)`. With the gate off inside that walk, `p.tag`
is a non-escaping use and `v` stays on the frame.

### What was actually wrong, in three parts

1. **`findEscapingCaptures` asked the wrong question.** It promoted the variable
   the address expression was *rooted at*. `&p.f` on a pointer-typed `p`
   addresses the pointee; promoting `p` moves the pointer and leaves the pointee
   exactly where it was. `addressedVariableIdentifier` now names the variable
   whose own slot the address refers to, stopping at any step through a pointer,
   a slice or a map. This is the fix `phase2-alloc`'s report says was "not
   attempted here".

2. **A value in a composite literal always escaped.** `nonEscapingObjectUse` had
   no `*ast.CompositeLit` or `*ast.KeyValueExpr` case, so
   `h := hexdumper{mark: symMark}` in `runtime.hexdumpWords` made `symMark`
   escape, which made `mark` escape, which made `tracebackHexdump`'s `frame`
   escape, which made `&u.frame` escape in `(*unwinder).next`, which made
   `copystack`'s and `scanstack`'s `var u unwinder` heap-allocate. An element
   escapes exactly when the composite does — for struct and array literals only,
   because a slice or map literal has backing storage of its own.

3. **A closure escaped on any use but a direct call.**
   `functionLiteralEscapesWithin` scanned the closure variable's uses and treated
   everything that was not `f()` as an escape. It now asks
   `nonEscapingObjectUse`, the same predicate every other value gets.

And the chain walk itself had to be restricted: `&v.a.b` and `&v[i].f` are
addresses inside `v`, but `&i.s.next` — where `s` is a pointer field — is a field
of the `special` the iterator refers to, not of the iterator.
`addressedExpression` now climbs only steps that stay within one object
(`selection.Indirect()` false; index base an array). Without that restriction
`(*specialsIter).next` reported its receiver as escaping and
`runtime.sweepLocked.sweep` allocated — the sweeper runs on g0.

### Measured, whole-image

`goc/testdata/stdlib_http_tls_client_server.go`, 14,871 functions, both
compilers built from this tree at this path:

| | `main` | this branch |
| --- | ---: | ---: |
| functions calling `runtime.newobject` | 4,130 | 4,110 |
| `runtime.newobject` call sites | 15,284 | 15,222 |

**Gained (1):** `net/netip.Addr.v6u16`.
**Lost (21):** `runtime.printfloat32/64`, `runtime.printcomplex64/128`,
`runtime.printDebugLogImpl`, `runtime.hexdumpWords`,
`runtime.tracebackHexdump`, `runtime.traceback2`, `runtime.traceRegionAlloc.alloc`,
`context.afterFuncCtx.cancel`, `internal/godebug.Setting.Value`,
`net/http.relevantCaller`, `reflect.valueMethodName`, three `nistec` generators,
`crypto/internal/fips140/aes/gcm.ghashUpdate`,
`crypto/internal/fips140/drbg.Counter.update`, two `chacha20poly1305` helpers and
one `net/http` interface-call wrapper.

`copystack`, `scanstack`, `sweepLocked.sweep` and `cg12ReportStaleStackWord` —
the four that made `phase2-alloc` hold the fix back — do **not** allocate.

**`net/netip.Addr.v6u16` is a real regression and is not fixed here.**
`(*uint128).halves` returns `[2]*uint64{&u.hi, &u.lo}`; the host reports
`leaking param: u to result ~r0 level=0` and keeps the caller's value on the
frame, because the returned array is indexed and dereferenced immediately.
cg12 has the summary for *parameters* but not for *receivers*, and
`valueDoesNotEscapeWithin` has no `*ast.IndexExpr` or `*ast.StarExpr` case, so
the result of `ip.addr.halves()` cannot be walked through. Closing it means a
`receiverLeaksOnlyToResult` summary plus two more walk cases; it is bloat on a
non-fatal path, so it was left rather than adding more surface to the highest-risk
change class in the repo in one round.

---

## Two miscompiles this branch introduced, found by the matrix and fixed

Both were in the new "leaks only to result" machinery, both reduced before being
fixed, and both are recorded here because they are the failure mode the task
warned about: a wrong summary that leaves a stack address reachable from the
heap.

### `stdlib-log/slog-structured` — fresh storage is not a result

```go
var out bytes.Buffer
logger := slog.New(slog.NewTextHandler(&out, nil))
logger.Info("hello", "k", 1)
// out.Len() == 0 under goc; 55 under the host
```

`out` stayed on `main`'s frame while `slog.NewTextHandler`'s
`&TextHandler{&commonHandler{w: w}}` put its address in a heap object. The walk
climbed `w` into the composite, through the address-of, to the return, and the
result rule said "leaks only to result".

It does not. Taking an address makes *fresh storage*, and returning that address
puts the storage in the heap, so a value placed inside it is in the heap the
moment the function returns — the caller's frame cannot hold it. The summary
walk no longer climbs through an address-of. Outside a summary walk nothing
changes, because a return was already an escape there.

### `stdlib-text/regexp-find-replace` — the walk must see every use

`(*Regexp).Split` faulted on a nil pointer. In `FindAllStringIndex`:

```go
re.allMatches(s, nil, n, func(match []int) {
	if result == nil {
		result = make([][]int, 0, startSize)
	}
	result = append(result, match[0:2])
})
return result
```

The walk was enumerating the **closure's** body while deciding whether
`result`'s backing array escapes. Every use it could see was benign, so the
array went on the closure's frame — and the enclosing function returns it.

`objectDoesNotEscape` now refuses any object whose declaration is outside the
body it is scanning, with the analysed function's own receiver, parameters and
named results as the one exception: they are declared in the signature and are
exactly what the walk is asked about. **This hole predates this branch** — a
closure that assigns a `make` to a captured variable reaches it on `main` too;
`append`'s conservatism was hiding the route this program takes.

Neither was visible in `test-unit`, `test-goc-corpus` or `test-goc-cmd`. The
matrix found both.

## Item 3 — `fatal error: span has no free objects`. Explained, and it is not the escape change.

### Reproduced first

`main` (`0505d90`) with `ccwork/phase2-alloc` cherry-picked onto it, built in this
tree at this path, linked against a pack built by the same compiler:

| tree | `runtime_cleanup_basic.go`, 40 runs at `GOMAXPROCS=4` |
| --- | --- |
| `main` | 0/40 failed |
| `main` + `phase2-alloc` | **40/40 failed**, every one `fatal error: span has no free objects` |
| this branch | 0/40 failed |

### It is not an allocator interaction. It is a lost sub-width store.

The escape fix contributes *nothing* to this program. What the merge actually
does is take `phase2-alloc`'s rewrite of `gen.store`, which was written against
`ff6ef9e` and predates §5.12. `git` merges it cleanly, and the result is:

```go
	class, _ := scalar(t)
	if class != ir.ClsP {
		g.cur.Store(v, addr)     // phase2-alloc's early return
		return
	}
	barriered := ...
	if barriered { ...; return }
	if sub, ok := subOf(t); ok { // main's sub-width path, now unreachable
		g.cur.StoreSub(sub, v, addr)
		return
	}
	g.cur.Store(v, addr)
```

§5.12 moved the barrier decision *ahead* of the sub-width store, so `subOf` sits
below it. `phase2-alloc` replaced the same region with an early return for every
non-pointer class. The two edits do not overlap textually, so the merge keeps
both — and the early return shadows `subOf` for every type that is not a
pointer. **Every `byte`, `bool`, `uint8`, `uint16` and `uint32` store becomes a
full-width store**, writing over the three to seven bytes next to it.

Counted in the IR of `runtime_cleanup_basic.go`:

| tree | `storeb` instructions |
| --- | ---: |
| `main` | 1,858 |
| `main` + `phase2-alloc` | **4** |
| `main` + `phase2-alloc`, sub-width path restored | 1,858 |

`span has no free objects` is `mcentral.cacheSpan`'s assertion that a span it
just obtained has a free slot. `mspan` is full of byte-wide fields — `state`,
`sweepgen`'s neighbours, `needzero`, `spanclass`, `allocCountBeforeCache`'s
packing — and so are the `mheap` and `mcache` structures around it. Writing four
bytes where one belongs corrupts them, so the allocator's bookkeeping stops
agreeing with itself. The failure being 40/40 rather than intermittent is what a
deterministic miscompile looks like, not what a race looks like.

### Proof

Restoring the sub-width path *inside* `phase2-alloc`'s early return, and nothing
else:

```go
	if class != ir.ClsP {
		if sub, ok := subOf(t); ok {
			g.cur.StoreSub(sub, v, addr)
			return
		}
		g.cur.Store(v, addr)
		return
	}
```

| tree | 40 runs |
| --- | --- |
| `main` + `phase2-alloc` | 40/40 failed |
| `main` + `phase2-alloc` + that one restoration | **0/40 failed** |

and the IR of `runtime_cleanup_basic.go` becomes **byte-identical to `main`'s**:
0 of 4,111 functions differ. The escape fix changes nothing in this program at
all, which is the direct disproof of the attribution.

### The control that rules out the escape fix independently

At `61b96da` — the tree §5.14 names, which does **not** contain §5.11/§5.12
(`c83be4f` is not an ancestor of it) — `phase2-alloc`'s whole commit produces a
**byte-identical** prebuilt runtime pack *and* a byte-identical
`runtime_cleanup_basic` executable:

```
568d49e2...  61b96da        runtime pack
568d49e2...  61b96da + phase2-alloc

46e7b279...  61b96da        cleanup_basic
46e7b279...  61b96da + phase2-alloc
```

There, `gen.store` still had `phase2-alloc`'s own shape, so nothing was lost and
nothing changed. §5.14's "main + phase2-alloc alone passes" row is that identity.
The 40/40 row is the merge with §5.12, and it is §5.12's line that goes missing.

### What §5.14 should say instead

Its two method conclusions survive intact and are, if anything, sharper: the
integration run is the gate, and merge order is not neutral. But the sentence
"Both changes move objects between the frame and the heap and both change what
the collector is told about them" is not what happened. Nothing about the
allocator, the write barrier or the escape decision is involved. A clean
three-way merge of two functions' worth of adjacent edits silently deleted a
branch, and the only diagnostic that would have caught it is the one §23
established: compare the two images by content, not by whether the suite is
green. 1,854 `storeb` instructions disappearing is not subtle once the question
is asked.

**This does not need fixing here**: `phase2-alloc` is unmerged, and whoever lands
it must rebase rather than merge, and must diff the resulting IR against `main`.

## Verification

Everything below was measured on the final tree (`HEAD` of
`ccwork/escape-analysis`), in this directory, with both compilers built from this
tree at this path.

### The capability matrix, both arms

| arm | subtests | PASS | EXPECTED FAILURE | FAIL | KNOWN GAP |
| --- | ---: | ---: | ---: | ---: | ---: |
| `make test-goc-status` | 345 | 344 | 1 (`defer-panic/panic-string-output`) | 0 | 0 |
| `make test-goc-status-opt` | 345 | 344 | 1 | 0 | 0 |

Censused from `-v` output: `=== RUN` lines counted (345 each), `--- PASS`/`--- FAIL`
tallied, and the expected failure confirmed by its
`EXPECTED FAILURE runtime_panic_print_string.go` line. The complete list of
non-passing capabilities is **empty** in both arms.

### The same matrix under the write-barrier checker

`GODEBUG=cg12checkwb=2`, both arms: 345/345, **zero** `cg12checkwb:` reports.

That is meaningful only because a positive control fires. A laundered frame
address stored into a package global while a concurrent mark runs:

```
cg12checkwb: pointer write barrier stored a goroutine stack address into a global
cg12checkwb: slot=0x5a0030 old=0x6ed0f2527ca8 new=0x6ed0f2527ca8 bad=new-is-stack
fatal error: cg12checkwb: global data word holds a goroutine stack address
```

**Its limit, stated plainly.** `cg12checkwb=2` flags a stack address reaching
*data or bss*. It does not flag a stack address reaching a *heap object*:
`cg12WriteBarrierValueIsBad` returns false for `mSpanManual` addresses, and the
`stackNew` rule requires `cg12AddressIsGlobal(slot)`. The `slog` bug was exactly
that class, and the matrix caught it, not the diagnostic. Widening the check is
not free — `sudog.elem` and other runtime structures legitimately hold stack
addresses in heap objects — so it was not attempted.

### Other diagnostics

- `GODEBUG=gccheckmark=1,invalidptr=1`, full matrix: 345/345.
- `GODEBUG=cg12scanroots=1`, the whole `gc` category: all pass. It reports rather
  than checks, so this says the precise scan runs clean there; it is not a
  corpus-wide proof.

### Suites

- `make test-unit` — green.
- `make test-goc-corpus` (`./goc/...`, the non-executable path) — green, 580s.
- `make test-goc-cmd` — green, 272s.

### Determinism (§23), measured before and after

`scripts/determinism-check.sh -corpus`:

| configuration | rounds | reproducible | varying | failed |
| --- | ---: | ---: | ---: | ---: |
| default | 3 | 365 | 0 | 0 |
| `-O` | 2 | 365 | 0 | 0 |

No layout-only residue in either.

### Host-toolchain comparison (§3 step 2)

- `go build -gcflags='runtime=-m -m'` confirms the host moves **no** `unwinder`
  to the heap in `copystack`, `scanstack` or `(*unwinder).next`, and allocates
  nothing in `sweepLocked.sweep` — which is what made the ungated field-address
  rule's first two attempts identifiable as cg12 conservatism rather than
  correct answers.
- `go build -gcflags='net/netip=-m -m'` gives
  `uint128.go:67:7: leaking param: u to result ~r0 level=0`, which is the summary
  cg12 lacks for receivers, and is why `Addr.v6u16` newly allocates.
- The two miscompiles this branch introduced were both found by running the same
  program under both toolchains: `out.Len()` is 55 on the host and was 0 under
  goc; `re.Split` returns three parts on the host and faulted under goc.

## Still unverified, and what is deliberately not done

- **`net/netip.Addr.v6u16` newly heap-allocates.** Bloat on a non-fatal path,
  measured and explained above, not fixed. Closing it needs a
  `receiverLeaksOnlyToResult` summary plus `*ast.IndexExpr` and `*ast.StarExpr`
  cases in `valueDoesNotEscapeWithin`.
- **`addressEscapesWithin` has no `*ast.UnaryExpr` case**, so an address nested
  inside `&T{...}` — `return &holder{p: &local}` — reports "does not escape".
  Pre-existing, untouched, and **not measured for consequences**. It is the
  nearest neighbour of what this branch fixed and is the first thing the next
  job in this area should look at.
- **`escapeWalkSeesEveryUse` is blunt.** A captured variable can never be shown
  not to escape now, whatever it does. Sound, but it gives up precision that a
  walk following the capture to its declaring function would keep.
- **No fatal consequence was produced for item 1.** As §5.10 recorded, the
  diagnostic printing floats closest to mark termination runs clean because
  `gcController.endCycle` is called after `setGCPhase(_GCoff)`.
- **The `phase2-alloc` branch is not landed here.** Its escape fix is superseded
  by this branch's; its fourteen allocation/write-barrier capabilities and its
  `GOC_DEBUG_WRITEBARRIER` audit are not, and landing them would move the matrix
  from 345 capabilities, which this round's constraints pin. Whoever lands it
  must rebase rather than merge and diff the resulting IR against `main` — see
  §5.14.
- **Rate.** The matrix was run once per arm per configuration, not repeatedly.
  `gc/cleanup-basic` specifically was run 40 times on this branch (0/40 failed)
  because that is the capability §5.14 concerns. Nothing here is a rate
  measurement in the §5.8 sense.
- The `ccwork/closure-string` defect (§5.10's first bullet) is untouched; it is
  a sibling job's area and this branch does not affect it either way.

## Commits

| commit | what |
| --- | --- |
| `2724ac7` | the leaks-only-to-result summary, the named-result fix, the field-address hole ungated, the `append` growth fix, `GOC_DEBUG_ESCAPE` |
| `9f76498` | the two miscompiles the matrix found: fresh storage is not a result, and the walk must see every use |
| (plan) | RUNTIME_PLAN §5.10 bullets closed, §5.14 rewritten with the mechanism, §24 added |

---

# Previous report on this tree (`ccwork/reportzombies`)

# A closure leaves a captured string variable pointing at its dead frame

Branch `ccwork/closure-string`, off `main` (`0505d90`). RUNTIME_PLAN.md §5.10, first
bullet under "Known miscompiles, not covered by any capability".

**Verdict: fixed, and it was three type classes rather than one.** The defect is
not about strings and not about `range`; it is about every local variable cg12
keeps in a frame slot as a pointer to a separate sixteen-byte value — a string,
an interface, or a `complex128` — assigned from inside a closure that captured it
by reference. 27 of 70 differential programs disagreed with the host toolchain;
26 agree now, and the 27th is a different defect this branch measures and
deliberately does not fix (section 3).

Both matrix arms are 347 subtests / 346 PASS / 1 declared EXPECTED FAILURE /
0 FAIL / 0 KNOWN GAP, all 367 corpus programs are still byte-reproducible in all
three configurations, and §5.13's three range-over-function cases assert what
they originally wanted to. What is *not* established is section 9.

This file was written as each result landed; the sections are in the order they
were measured.

## 1. Reproduced

`RUNTIME_PLAN.md` §5.10's program, verbatim, compiled with `goc` built from this
tree:

```
inside az
outside
```

The host toolchain prints `inside az` / `outside az`. Confirmed on `0505d90`.

## 2. The full shape, measured against the host toolchain

65 differential programs (§3 step 2: each run with `go run` and with `goc`, stdout
and exit status compared). **27 differ.**

### What is affected

A **local variable whose cg12 representation is "the frame slot holds a pointer to
an inline 16-byte value"**, assigned inside a **non-escaping function literal that
captured it by reference**. That is three type classes:

| type class | example | verdict |
| --- | --- | --- |
| `string` | `log = log + s` | **wrong** |
| named type with underlying `string` | `type label string` | **wrong** |
| interface (`any`, `error`, …) | `v = n`, `err = errors.New(...)` | **wrong** |
| `complex128` | `c = complex(3, 4)` | **wrong** |

Every assignment form is affected — the operator and the right-hand side are not
the discriminator:

| form | program | goc |
| --- | --- | --- |
| `log = log + s` (computed) | c01 | `fatal error: runtime: out of memory` |
| `log = "az"` (**string literal**) | c02 | garbage bytes |
| `log += s` | c03 | `fatal error: runtime: out of memory` |
| `log = fmt.Sprint(n)` | c04 | OOM |
| `log = s` (**plain parameter copy**) | c05 | OOM |
| `log = string(b)` | c27 | OOM |
| `n, log = 1, log+s` (tuple) | c25 | OOM |
| `a, b = b, a` (swap) | c26 | OOM |

and every way of reaching the closure:

| shape | program | goc |
| --- | --- | --- |
| named function-literal variable, called | c01 | wrong |
| immediately-invoked function literal | c28 | wrong |
| function literal nested in a function literal | c21 | wrong |
| function literal passed to a **generic** function | c62 | wrong |
| `for i := range seq` over a function iterator | c19, c40 | wrong |
| deferred literal inside a non-escaping literal | c17 | SIGSEGV |
| the captured variable is a **parameter** of the enclosing function | c42 | wrong |

### What is *not* affected, and why

| shape | program | why it is correct |
| --- | --- | --- |
| read-only capture | c06, c44, c49 | nothing is assigned |
| **slice** | c07, c08, c38, c47 | under `runtimeAllocation` a slice local is stored **inline** (3 words in the slot); `assignLocal` writes the three words, it does not rebind |
| **struct / array** | c11, c12, c46, c63 | `isMemoryValue`: the slot holds a *stable backing address* and `assignLocal` copies into the backing |
| `int`, `bool`, pointer, map, chan, func, `complex64` | c13, c15, c14, c53, c52 | one word in the slot |
| **escaping** closure — goroutine | c16, c48 | `variableStorage` heap-lifts the variable and sets `directValues`, so the storage *is* the header |
| **escaping** closure — returned | c34 | same |
| named result written by a closure | c22, c43, c45 | `resultStorage` gives direct storage |
| `&v` taken and escaping | c50 | heap-lifted for the same reason |
| write through a pointer parameter | c20, c36 | the address path stores inline |
| assignment to a **field or element** of a captured variable | c23, c24, c63 | goes through `assignmentTargetAddress`, which stores inline |
| `defer` registered in a loop | c18 | §5.1 heap-lifts those captures |

Two cases matched *by luck* on the first pass and differ once a call clobbers the
dead frame — recorded because they are the reason a green matrix would not find
this: `c37` (`if ok { log = log + s }` inside the closure) and `c39` (the captured
variable is a parameter). Adding a recursive call between the write and the read
turns both into `result ` — see `c41`, `c42`. **A "passing" observation of this bug
is worth nothing unless the frame is clobbered between the write and the read.**

### The symptoms, and why there are so many

§5.10 recorded two observable shapes. There are at least six, and which one you get
is decided by what the dead frame happens to hold when it is read back:

- silently empty string (the §5.10 reducer)
- garbage bytes printed as the string's content (c02, c33)
- `fatal error: runtime: out of memory` — the length word is read as a huge number
  (c01, c03, c05, …)
- `SIGSEGV` in `runtime_concatstrings` / `goc_memmove` (c17, c19, c40)
- `unexpected fault address 0x3fffff` (c29)
- `panic: can't call pointer on a non-pointer Value` inside `reflect`, or
  `<invalid reflect.Value>`, for the interface cases (c09, c10, c32)

### The mechanism

A local `string` (or interface, or `complex128`) variable's frame slot holds an
*8-byte pointer* to a 16-byte header. `goc/compile.go`'s `storeAssignmentTarget`,
for a destination classified `assignmentTargetVariable` with `local` true:

```go
if target.local && isDescriptorValue(target.valueType) {
        value = g.copyInlineValue(value, target.valueType)   // fresh localAllocTyped in THIS frame
}
...
g.store(value, target.slot, target.valueType)                // rebinds the slot to it
```

`copyInlineValue` calls `localAllocTyped`, an alloca **in the function doing the
assigning**. The slot, however, belongs to whichever frame declared the variable.
When the assigning function is a closure that captured the variable by reference —
`funcLit` stores `g.vars[capture]`, the slot address, into the closure descriptor,
and the child reads it back as `child.vars[capture]` — the store publishes a
pointer to the *closure's* frame into the *parent's* slot. It dangles the moment
the closure returns.

`complex128` is worse: it is `ir.ClsP`-classed and 16 bytes, but
`isDescriptorValue` is false for it, so it does not even get the `copyInlineValue`
step; the slot is rebound to whatever address the right-hand side produced.

The escaping-closure path is unaffected because `variableStorage`'s heap-lift arm
sets `g.directValues[object] = true` for strings and interfaces, which makes the
storage the header itself and routes the assignment through `storeInlineValue`.
**The fix is to give the same representation to a variable captured by reference by
a non-escaping literal**, which costs no allocation: the header lives in the
declaring frame instead of behind a pointer.

## 3. A neighbouring defect found while measuring: `complex128` in memory

`complex128` shares the affected representation, so it came along with the
measurement. The *variable* half of it is the same defect and is fixed below
(c54, c56). The *memory* half is a different defect, is **not fixed**, and is
recorded here with its reducers.

**A `complex128` written through an address destination stores the address of a
frame allocation into the destination, not the value.** `h.c = complex(3.0, 4.0)`
with `h` a `*holder`:

```
%t3 =p alloc8 16
stored d_3, %t3
%t4 =p add %t3, 8
stored d_4, %t4
call $goc_storep(p %t2, p %t3)      <-- the field gets %t3, the frame address
```

`storeAssignmentTarget`'s `assignmentTargetAddress` arm stores inline only when
`isInlineAggregate(valueType) || isInterfaceValue(valueType)`, and `complex128`
is neither, so it falls to `g.store`, which is a one-word store for an
`ir.ClsP`-classed type. Reads are consistent with it — `g.load` returns the word
and `real`/`imag` dereference it — which is why it looks correct until the frame
that produced the value dies. Three reducers:

| program | host | goc |
| --- | --- | --- |
| `*p = complex(3, 4)` through a `*complex128` (c59) | `3 4` | `3.8004551335784e-310 3.8004551335772e-310` |
| write `h.c` in one function, read it in another (d01) | `3 4 7` | `3.499451e-317 5.29561993437e-310 7` |
| return a struct holding a `complex128` by value (d07) | `{(3+4i) 7}` | `{(0+0i) 7}` |

It also hands `goc_storep` a goroutine-stack address as the value being
published into a heap object, which is the §5.8 invariant reached by a fourth
route.

**Why it is not fixed here.** Widening the store arm alone is wrong and was
measured to be wrong: doing it turned d02 (`fmt.Println(h)` on a struct with a
`complex128` field, which reads the real sixteen bytes through reflect) and c58
from passing to faulting, because the *read* side still loads one word. Making
`complex128` a value carried as an address consistently means putting it into
`isInlineAggregate`, which is consulted in dozens of places across the frontend
— a change with its own blast radius and its own validation cycle, of exactly
the shape RUNTIME_PLAN §5.14 records going wrong when bundled. The narrow rule
this branch does adopt (`assignmentTargetStoresInline`) deliberately leaves the
address path exactly as it was, so nothing here regresses and nothing here
papers over it.

## 4. The fix

`variableStorage` already had the right representation for this — it is what the
*escaping*-capture arm uses. The fix extends it to non-escaping captures, where
it costs nothing at all: no heap cell, the value simply lives in the declaring
frame's slot instead of behind a pointer to another sixteen bytes.

- **`findReferenceCaptures`** (new, `goc/compile.go`) returns the local variables
  a nested function body refers to. A nested body is a function literal *or* the
  body of a `range` over a function, which is lowered into a yield function; both
  reach the variable through the closure environment, which carries the address
  of its slot. `findEscapingCaptures` answers the narrower question — which of
  those must be heap-lifted because the nested function can outlive the frame —
  and both now share a `bodyLocals` helper so they agree on what a local is.

- **`variableStorage`** gives such a variable direct storage when its type would
  otherwise be a pointer to a separate value: `localAllocTyped`, zeroed, with
  `directValues[object] = true`. `localAllocTyped` marks the value's pointer
  words in the frame's stack map, so the string's data word and the interface's
  two words stay GC roots, which the old header alloca also was.

- **`isIndirectVariableValue`** names the type set: not a struct or array
  (`allocLocal` already gives those stable backing), not a slice (stored inline
  under `runtimeAllocation`), leaving string, interface and `complex128`.

- **`isInlineValue`** = `isInlineAggregate || isInterfaceValue || isComplex128Type`
  is the "carried as an address" predicate the direct-value paths now use, and
  `isAddressRepresentedInterfacePayload` is expressed in terms of it.

- **`assignmentTarget.directVariable`** marks a destination whose own storage is
  its value, and `assignmentTargetStoresInline` is what the read (`+=`) and the
  store consult. It is deliberately *not* the same question as `target.inline`:
  an address destination of type `complex128` keeps the word store it has always
  had, because `complex128` is only carried as an address as a *value*, not in
  memory. Widening that arm broke `c58` (a `complex128` struct field), which is
  how the distinction was found; the narrower rule leaves `c58` passing.

- The escaping-closure descriptor's snapshot arm (`cell == ir.R`) now copies a
  direct value's bytes for any of the three types rather than only for an
  interface. Reaching it needs a direct value that was never heap-lifted, which
  no reducer produced; it is generalised because loading through the slot first,
  as the aggregate arm below it does, would read the value's first word as the
  address of the value.

### Result

**69 of the 70 differential programs now match the host**, up from 43. The one
that still differs is `c59`, `*p = complex(3, 4)` through a `*complex128`, which
is the pre-existing defect in section 3 and differed before this change too.

Every affected case in section 2's tables is fixed, including all six symptom
shapes, both range-over-function forms, and the `complex128` cases from section
3's second bullet.

## 5. What was landed with it

- **`closure-capture/assigned-string`** and
  **`closure-capture/assigned-header-values`**, two new capabilities
  (`goc/testdata/runtime_closure_captured_{string,header_values}.go`). Every case
  calls a recursive `clobber` between the closure's write and the read, because
  two reducers found while measuring passed *without* that and failed *with* it.
  Both carry their controls: read-only capture, escaping closure, returned
  closure, captured slice, captured struct field, captured slice element.

- **`goc/closurecapture_test.go`**, four mechanism tests over the three affected
  types: the closure does not store one of its own frame allocations through the
  captured pointer; the enclosing function allocates the value's full width for
  it; a non-escaping capture does not start calling `runtime.newobject`
  (§5.9's cost model); and the range-over-function yield function obeys the first
  rule with no function literal in the source.

- **§5.13's rewritten range-over-function cases are restored.**
  `goc/testdata/runtime_range_target_forms.go`'s `iteratorTargets` accumulated
  into a slice while every other subject in the file accumulated into a string;
  §5.10 records that as a workaround for this defect. All four cases now use
  `observed += ...` like the rest of the file, which makes them captured-variable
  cases in their own right.

## 6. Suites

Every one of these was re-run at the tip commit `7182ddf`, after the plan and
report edits, not only at the commit that introduced the fix.

| suite | result |
| --- | --- |
| `go build ./...`, `go vet ./...` | clean |
| `gofmt -l goc/ cmd/goc/` | clean |
| `make test-unit` | ok, every package |
| `make test-goc-corpus` | `ok github.com/evanphx/cg12/goc 567.755s` |
| `make test-goc-cmd` | `ok github.com/evanphx/cg12/cmd/goc 229.315s` |
| capability matrix, default arm | **347 subtests, 346 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP** |
| capability matrix, `-runtime-opt` arm | **347 subtests, 346 PASS, 1 EXPECTED FAILURE, 0 FAIL, 0 KNOWN GAP** |

**The complete list of non-passing capabilities, both arms: none.** The only
non-`PASS` verdict in either arm is `defer-panic/panic-string-output`, the
declared `expectedFailure`. `--- FAIL:` and `--- SKIP:` appear zero times in
either arm.

347 rather than 345 because this branch adds two capabilities. The census is from
the per-capability `PASS` / `EXPECTED FAILURE` / `KNOWN GAP` log lines and a
count of `--- PASS:`/`--- FAIL:`/`--- SKIP:` subtest lines, not from `ok`. Four
shards per arm, `-runtime-status-shards=4 -runtime-status-compile-workers=16`,
47-54s per shard.

## 7. The reducers fail on the compiler they describe

A reducer that has only ever reproduced once is a claim (RUNTIME_PLAN §15). Each
one was re-run against a compiler built at **this same path** from `HEAD~2`'s
`goc/compile.go` — the tree's own commit reverted, not a worktree, so the
path-dependence §23 records does not enter.

| program | base compiler | this branch |
| --- | --- | --- |
| `runtime_closure_captured_string.go` | `SIGSEGV`, nil pointer dereference | `closure captured string ok` |
| `runtime_closure_captured_header_values.go` | `unexpected fault address 0x6e6f2d64616572` | `closure captured header values ok` |
| `runtime_range_target_forms.go` (restored) | `SIGSEGV`, nil pointer dereference | passes |

`0x6e6f2d64616572` is `"read-non"` little-endian: the fault address is the
program's own text, read out of the frame the closure abandoned.

**The first two mechanism unit tests were wrong and had to be rewritten.** As
first written they passed against the base compiler, which makes them worthless.
Two reasons, both worth recording:

- A pointer store lowers to a **`goc_storep` call**, not an `ir` store
  instruction, whenever the destination is not a known stack address — and a
  captured pointer never is. A check that looked only at `Op.IsStore()` could not
  see the store the defect consists of.
- "the enclosing function allocates 16 bytes somewhere" was true on both
  compilers. What separates them is the width of the allocation whose address
  goes *into the closure environment*: sixteen for the value, eight for a pointer
  to it.

Rewritten (`6878d1a`), all three defect tests fail on the base compiler across
all three types — 7 failing subtests — and pass here.
`TestNonEscapingCaptureStaysOffTheHeap` passes on both by design: it is a guard
on §5.9's cost model, not a defect test.

## 8. Determinism (§23) is not regressed

`analysis/determinism` over all 367 `goc/testdata` programs, 4 rounds each, 16
workers, in three configurations:

| configuration | result |
| --- | --- |
| no `-O` | `reproducible=367 varying=0 failed=0 of 367 over 4 rounds` |
| `-O` | `reproducible=367 varying=0 failed=0 of 367 over 4 rounds` |
| against a prebuilt pack (`goc build-runtime`) | `reproducible=367 varying=0 failed=0 of 367 over 4 rounds` |

`content varies between rounds: 0` and `image varies, content identical (layout
only): 0` in each. `scripts/determinism-check.sh`'s five-program sample agrees,
cold and warm, two rounds.

The fix cannot introduce nondeterminism by construction — `findReferenceCaptures`
returns a map that is only ever *looked up* in, never iterated — but §23's rule
is to measure rather than argue, so it is measured.

## 9. What is left open

- **`complex128` in memory** — section 3. Three reducers, an IR excerpt, and a
  measurement showing why the one-line widening is wrong. Not fixed; recorded in
  RUNTIME_PLAN §5.15 under "Residual".
- **A `slice` local under `!runtimeAllocation`** has the same indirect
  representation as a string and would have the same defect. It is excluded from
  `isIndirectVariableValue` deliberately: `!runtimeAllocation` is the `goc -c`
  path, which compiles to objects and never executes, so the change could not be
  validated. Stated rather than made silently.
- **`c++` on a `complex128`** goes through `IncDecStmt`'s word load and is
  nonsense on both compilers. Untouched and unmeasured beyond that reading.
- **§5.14's `span has no free objects` interaction** is untouched by this branch,
  which changes no escape decision: `findEscapingCaptures` is unmodified and
  `variableStorage`'s heap-lift arm still fires on exactly the same set.
