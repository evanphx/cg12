# Why creating and joining a goroutine costs 38x the host toolchain

Branch: `ccwork/goroutine-scheduler`, off `ccwork/perf-suite` = `d2855f5`, whose
`make bench-perf` suite produced the measurement this investigates. The
perf-suite report is `git show d2855f5:CCWORK_REPORT.md`.

## The answer in one line

`goroutine/spawn-join` is 38x because **`runtime.findfunc` is O(number of
functions in the image)** in a goc-built binary — cg12 emits
`moduledata.findfunctab` as all zeroes, so `findfunc` starts its scan at functab
index 0 every time — and the goroutine path calls `findfunc` **twice per
goroutine**, once in `newproc1` and once in `gdestroy`, on a `startpc` that sits
near the end of a 5,522-function text.

It is a **linker-metadata** problem, not a scheduler-algorithm problem and not a
code-generation problem. `stdlib/src/runtime/proc.go` and `chan.go` are
byte-identical to the host toolchain's `go1.26.1` sources.

## Status

- [x] Reproduced, 40.6x on a single pinned run.
- [x] Profiled: 87% of the working thread's samples are in `runtime.findfunc`.
- [x] Mechanism proved directly and independently of the profile.
- [ ] Fix + `make bench-perf` before/after — in progress.

