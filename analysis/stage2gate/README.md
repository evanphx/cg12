# The stage-2 gate's harness

What `integration/stage2-gate` ran to check `ccwork/cache-store-and-merge`. Kept
because the gate's central finding — cross-program cache reuse produces images
that do not link — is reproduced by two `goc` commands, and everything else here
is what established the boundaries of that.

The reproduction, which needs nothing in this directory:

    export CG12_FUNC_CACHE=$(mktemp -d)
    goc -o a goc/testdata/fmt_sprintf.go     # succeeds, fills the cache
    goc -o b goc/testdata/hello.go           # undefined reference to `_goc_itab_...'

The rest:

- `corpus-run.sh` / `corpus-diff.sh` — compile every `goc/testdata/*.go` under
  one compiler and one environment through `goc compile-batch` workers, and
  compare two runs' images program by program. Used for the default-path
  invariance check (`main` against the branch, both compilers built from one
  absolute path) and for the shared-directory corpus runs.
- `staleness.sh` — the ten key-invalidation cases, end to end, each checked
  against a `CG12_NOCACHE=1` build of the same tree in the same configuration.
- `concurrency.sh` — N concurrent `goc` processes on one program and one shared
  cache directory, with the cross-program confound removed.
- `gatekey_test.go.txt` — a `package goc` test that moves each clause of a
  compile identity and requires both that `FunctionCacheEntry.Valid` names it and
  that `packageCacheKeyDigest` moves, plus truncation and bit-flip sweeps over a
  stored unit. Not a `_test.go` file here: it calls `t.Setenv`, so landing it in
  `goc/` needs an entry in `goc/sequential_tests.txt` first.
- `gateclosure.go.txt` — prints a program's compile closure and the packages that
  transitively import a named one, from `ProgramCompileIdentity`'s import graph.
  It is the independent answer §5.1 checks the cache's invalidation against.

See CCWORK_REPORT.md, "Stage 2 gate: verifying `ccwork/cache-store-and-merge`".
