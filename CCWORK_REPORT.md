# Loop-body allocation aliasing: a live miscompile in goc

Branch `ccwork/loop-aliasing-fix`, off `main` (`efcd4d4`). The previous job's
report (cross-function escape summaries) is at `efcd4d4:CCWORK_REPORT.md`.

Status: IN PROGRESS. Numbers land here as they are produced. Anything not
watched to completion is marked UNVERIFIED.

## 0. The defect, reproduced on main before anything was changed

Host toolchain is `go1.26.1 linux/arm64`; goc built from `efcd4d4`, run as
`goc -run`.

| program | form | host `go run` | `goc -run` on main |
|---|---|---|---|
| `loop_alias_forms.go` | `new(int)` | `new:   1 2` | `new:   1 2` |
| `loop_alias_forms.go` | `make([]int, 0, 4)` | `make:  1 2` | `make:  1 2` |
| `loop_alias_forms.go` | `var a [2]int; &a` | `array: 1 2` | **`array: 2 2`** |
| `loop_alias_composite.go` | `&cell{v: i}` | `alternate: 1 2` + `distinct` | **`alternate: 2 2`** + **`ALIASED: the two iterations share one allocation`** |
| `variadic_backing.go` | `retainNothing(&x)` | `1` | `1` |

Confirmed exactly as the brief states: two forms are already fixed by the loop
rule inside `opt.LowerHeapAllocations`, two are live. `variadic_backing.go`
already agrees with the host; it lands as a regression guard, not as a failing
case.

## 1. The three programs, landed as failing corpus tests (commit `f343e38`)

`goc/loopalias_test.go` compiles each program, links it against the
cg12-compiled Go runtime, runs it and compares everything it printed against
what Go prints. Each program is run twice, unoptimized and optimized, because
the placement is decided in two different places and a fix in one leaves the
other untouched.

    $ go test ./goc -run TestLoopBodyAllocationsAreDistinctPerIteration -count=1
    --- FAIL: .../loop_alias_forms.go        got "array: 2 2", want "array: 1 2"
    --- FAIL: .../loop_alias_forms.go_-O     got "array: 2 2", want "array: 1 2"
    --- FAIL: .../loop_alias_composite.go    got "alternate: 2 2\nALIASED: ..."
    --- FAIL: .../loop_alias_composite.go_-O got "alternate: 2 2\nALIASED: ..."
    --- PASS: .../variadic_backing.go
    --- PASS: .../variadic_backing.go_-O

`TestLoopAliasExpectationsMatchTheHostToolchain` passes: the expectations are
`go run`'s own output, not a belief about it.

## 2. What the three new corpus programs cost the baselines, before any fix

The programs are in `goc/testdata/`, which the corpus audits glob, so they are
also 3 new programs in the census. Regenerated on the **unfixed** compiler so
that the fix's own diff is attributable:

    $ go test ./goc -run 'TestAllocationCensus$|TestEscapeShadowPlacement$|TestFrameEscapeAudit$' \
        -update-alloc-census-baseline -update-escape-shadow-baseline -update-frame-escape-baseline
    ok  166.549s, 388 programs

| baseline | delta |
|---|---|
| `alloc_census_baseline.txt` | **+5 lines**, all in the three new files, all `heap` |
| `escape_shadow_baseline.txt` | unchanged |
| `frame_escape_baseline.txt` | **unchanged** — the new programs publish no frame address |

The five lines are worth reading, because of what is *not* there:

    loop_alias_forms.go:7:8    viaNew     newobject int    heap   <- new(int),  loop rule
    loop_alias_forms.go:22:23  viaMake    newobject 4_int  heap   <- make(...), loop rule
    variadic_backing.go:9:9    (x3)       newobject 1_any  heap

The two forms that are already correct appear as `heap`, which is the loop rule
in `opt.LowerHeapAllocations` doing its job. The two **broken** forms appear
nowhere: `var a [2]int` and `&cell{v: i}` are committed frame placements, and
the census does not record ordinary front-end frame slots. The instrument is
blind to the defect from the frame side; it will only see the fix from the heap
side.
