# The package-initializer dispatch miscompile: what the registration walk missed

Branch `ccwork/iface-init-dispatch`, off `main` (`4a6fd96`). The reduction this
starts from was committed, unfixed, by the `slog-allocations` job and lives at
`goc/testdata/slog_allocations/miscompiles/pkginit_dispatch.go`. Earlier jobs'
reports are at `4a6fd96:CCWORK_REPORT.md`.

Status: IN PROGRESS. Everything below was watched to completion unless marked
UNVERIFIED.

Host toolchain: go1.26.1 linux/arm64.

## 0. The defect, reproduced on main before anything was changed

At `4a6fd96`, with nothing changed:

    $ go run ./cmd/goc -run goc/testdata/slog_allocations/miscompiles/pkginit_dispatch.go
    cg12: interface dispatch failed for dynamic type 0x8512f8
    fatal error: cg12: interface dispatch failure
      log_slog_Handler_Enabled
      log_slog_Logger_Enabled
      ...
    $ go run goc/testdata/slog_allocations/miscompiles/pkginit_dispatch.go
    json ok

## 1. The shape survey, measured on main

The brief named four neighbouring shapes to check. I measured those and seven
more. Each row is a nine-to-fifteen-line program built on the same
`*log/slog.JSONHandler` -> `log/slog.Handler` conversion (row I uses
`*strings.Reader` -> `io.Reader`), differing only in where the conversion sits.
The `goc` column is `go run ./cmd/goc -run`, `gc` is `go run`, both at `main`
`4a6fd96`.

| # | shape | goc on main | gc |
|---|---|---|---|
| A | call argument in a package-level `var` initializer | **dispatch failure** | ok |
| B | call argument inside `func init()` | ok | ok |
| C | slice composite literal at package scope | ok | ok |
| C2 | struct composite literal at package scope | ok | ok |
| D | method value taken at package scope | ok | ok |
| E | call argument nested inside a package-scope composite literal | **dispatch failure** | ok |
| F | call argument inside a package-scope function literal | **dispatch failure** | ok |
| G | assignment inside a package-scope function literal | **dispatch failure** | ok |
| H | `return` inside a package-scope function literal | **dispatch failure** | ok |
| I | *variadic* call argument in a package-level `var` initializer | **dispatch failure** | ok |
| J | `var` spec inside a package-scope function literal | **dispatch failure** | ok |

Of the four shapes the brief asked about, **one was broken** (A, the package-level
`var` initializer) and **three were already sound** (B `init()`, C/C2 composite
literal at package scope, D method value at package scope). B is sound for a
different reason from C/C2/D, and the difference is the whole story:

  * `func init()` is an ordinary top-level `*ast.FuncDecl`, so it is a *root* of
    the reachability walk and its body goes through the full function-body
    walker. Nothing about it is special-cased; it is simply not on the
    initializer path at all.
  * composite literals at package scope and method values at package scope are
    the two implicit-conversion sites the initializer walk already handled
    (`enqueueCompositeImplementations`, and the identifier case that enqueues a
    referenced `*types.Func`).

The seven extra rows (E, F, G, H, I, J and the variadic half of A) are shapes
the brief did not name and that were also broken. They are the same defect from
a different angle: implicit conversions the *function-body* walk handles and the
*initializer* walk did not.

## 2. Root cause

`goc/reach.go` has two walks that decide which concrete methods are reachable,
and so which dynamic types the generated dispatcher gets an entry for
(`interfaceMethodCandidates` in `goc/compile.go` admits a candidate only if its
method is in the reachable set):

  * `processQueue`, over function bodies. It handled conversions at composite
    literals, assignments, `var` specs, `return` statements, channel sends,
    explicit `T(x)` conversions, **and call arguments**, including variadic ones.
  * `enqueueGlobal`, over package-level `var` initializer expressions. It
    handled only the conversion to the variable's own declared type, explicit
    `T(x)` conversions, and composite literals.

Call arguments were the missing site, and they are the commonest one: the
natural way to write a package-level interface value is
`var x = f(concreteValue)`. Nothing else in such a program converts that
concrete type, so if the argument site is not what registers it nothing does,
the dispatcher is generated with no entry for the type, and the first call
through the interface reaches `runtime.gocInterfaceDispatchFailure`.

The same divergence explains E through J: any implicit-conversion site inside a
package-scope composite literal or function literal is inside an initializer
expression, so it is walked by `enqueueGlobal`, which did not know about it.

(The fix and the guard results land below as they are produced.)
