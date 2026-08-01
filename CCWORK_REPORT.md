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
