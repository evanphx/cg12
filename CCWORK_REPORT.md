# A closure leaves a captured string variable pointing at its dead frame

Branch `ccwork/closure-string`, off `main` (`0505d90`). RUNTIME_PLAN.md §5.10, first
bullet under "Known miscompiles, not covered by any capability".

This file is written incrementally as results land. Sections are in the order they
were measured. **Status: shape established, fix in progress.**

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

## 3. Two neighbouring defects found while measuring (not the closure bug)

Both are in the assignment machinery §5.13 closed, not in a sibling job's area.

- **`*p = complex(3, 4)` through a `*complex128` corrupts the destination** (c59).
  Host prints `3 4`; goc prints
  `5.1494056002632e-310 5.14940560026203e-310`. `storeAssignmentTarget`'s
  `assignmentTargetAddress` arm stores inline only when
  `isInlineAggregate(valueType) || isInterfaceValue(valueType)`, and `complex128`
  is neither, so it falls to `g.store`, which writes the 8-byte *address* of the
  value into the 16-byte destination. Needs no closure at all.

- **An escaping closure that assigns a `complex128` reads back half garbage**
  (c56). `go func() { c = complex(3, 4) }()` prints `3.6611173e-317 4` where the
  host prints `3 4`. `variableStorage`'s heap-lift arm marks only strings and
  interfaces `directValues`, so a heap-lifted `complex128` cell holds an address
  where the reader expects the value.

Both are the same missing type case — `complex128` is a value represented by an
address, exactly like a string or an interface, and three separate predicates
disagree about that.

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

## 6. Still unverified

The suites and the matrix. Section 7 will record them as they land.
