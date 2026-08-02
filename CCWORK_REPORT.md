# A differential yardstick: goc's escape decisions against the Go compiler's

Branch `ccwork/escape-gc-differential`, off `main` (`efcd4d4`).

Status: IN PROGRESS. Numbers land here as they are produced. Anything I have
not watched to completion is marked UNVERIFIED.

## 0. The host toolchain, pinned

```
go version go1.26.1 linux/arm64
```

`-gcflags=-m`'s wording is not stable across releases, so every number below is
against **go1.26.1** and a rerun on another release has to re-derive them. The
harness records the version string in its output file for exactly this reason.

## 1. What was already measured, and why it did not answer the question

Everything measured in this effort so far compares two of goc's own analyses:
the AST walk frames 83.4% of placements, the summary-fed IR pass 79.4%. That
says the walk beats goc's own alternative. It says nothing about whether either
is good. `go` is installed on this box and prints its escape decisions on
request; this branch asks it.

## 2. The comparable universe

goc compiles a vendored stdlib out of `stdlib/src`; the host `go` compiles its
own. Positions in those two trees do not correspond, so no join across them is
possible. The comparable universe is therefore exactly **the allocations whose
source text is written in the corpus program's own file** — `goc/testdata/*.go`,
which both compilers read byte-for-byte identically.

Sizing that, from the checked-in census (`goc/testdata/alloc_census_baseline.txt`,
18 664 rows):

| | rows |
|---|---|
| census rows total | 18 664 |
| …at a `goc/testdata/*.go` position | **2 707** (14.5%) |
| …at a `stdlib/src/**` position | 10 094 |
| …with no position at all (`?`) | 5 863 |

The 2 707 comparable rows split **109 frame / 2 598 heap** and cover 378 of the
385 corpus programs.
