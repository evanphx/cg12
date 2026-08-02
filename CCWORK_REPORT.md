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
