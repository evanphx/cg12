# Escape analysis: the print-routine allocation, the field-address hole, and the interaction

Branch `ccwork/escape-analysis`, off `main` (`0505d90`).

**Status: in progress.** This file is written as results land; the trailing
"Still unverified" section is the honest list at any moment.

## Item 1 — four print routines heap-allocate

Reproduced on `main` before any change, with the reducer from RUNTIME_PLAN §5.10
and no runtime source involved:

```
viaReturn  newobject=true
viaDirect  newobject=false
```

`var buf [20]byte; consume(passthrough(buf[:0]))` calls `runtime.newobject`;
`consume(buf[:0])` does not.

Mechanism confirmed by reading the walk: `goc/compile.go`'s
`nonEscapingObjectUse` sends `buf[:0]` to `valueDoesNotEscapeWithin`, which
reaches the call and asks `parameterDoesNotEscape(passthrough, 0)`. That walks
`passthrough`'s body, finds `return dst`, and `nonEscapingObjectUse` has no
`*ast.ReturnStmt` case, so it falls to `default: return false`. The parameter is
therefore "escapes", and `findEscapingCaptures`' slice branch promotes `buf`.

## Still unverified

- Everything below item 1's reproduction.

## A pre-existing miscompile found while doing item 1

`parameterDoesNotEscape` consulted `g.resultObjects`, which holds the *named
results of the function being lowered*, while walking a **callee's** body. A
callee that assigns its parameter to its own named result and returns bare was
therefore reported as not letting the parameter escape:

```go
var pointerSink *int

func namedLeak(p *int) (out *int) {
	out = p
	return
}

func leakViaNamedResult() {
	value := 3
	pointerSink = namedLeak(&value)   // a package global now holds a frame address
}
```

Measured both ways with the same probe:

| tree | `leakViaNamedResult` calls `runtime.newobject` |
| --- | --- |
| `main` (`0505d90`) | **no** — `&value` stays on the frame |
| this branch | yes |

This is the §5.8 invariant ("a global holds a stack address") reached by a third
route, distinct from §5.8's own and from `phase2-alloc`'s field-address route.
It is closed by `gen.enterCalleeBody`, which now installs the *analysed*
function's named results for the duration of the walk.
