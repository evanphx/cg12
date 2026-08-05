# The interface-dispatch miscompile that keeps `goc -O`'s mem2reg switched off

Branch: `ccwork/mem2reg-iface-dispatch`, off `integration/wave8` = `7983abd`.

**Status: in progress.** This file is written as the job runs; the sections
below are filled in as each is measured.

## The defect

With `GOC_BOUNDED_MEM2REG=1` — the switch that lets `opt.BoundedPipeline` (the
only pipeline a whole-program Go build ever takes) run `Mem2Reg` — the
capability `stdlib-netpoll-stress/tcp-churn` dies with

    cg12: interface dispatch failed for dynamic type 0x0

in `net_Listener_Accept`, called from `main_serveTCPStressConnection`.

Reproduced on this tree at `7983abd`:

    go build -o goc ./cmd/goc
    GOC_BOUNDED_MEM2REG=1 ./goc -O -o churn.bin \
        goc/testdata/stdlib_netpoll_stress_tcp_churn.go
    GOMAXPROCS=1 ./churn.bin        # 5/5 fail
    ./goc -O -o churn.bin goc/testdata/stdlib_netpoll_stress_tcp_churn.go
    ./churn.bin                     # clean

At default `GOMAXPROCS` it is 2/3; at `GOMAXPROCS=1` it is 5/5, confirming the
previous job's determinism result. One compile-and-run cycle is 8.4 s, which is
what makes a delta-debug over the promoted-function set affordable.

## Notes

`opt.BoundedPipeline`'s mem2reg is wrapped in a bisection filter for this job:
`GOC_BOUNDED_MEM2REG_LIST=<file>` records every function mem2reg changed, and
`GOC_BOUNDED_MEM2REG_ONLY=<file>` restricts promotion to the functions named in
that file. tcp-churn promotes **3494** functions with the switch on.
