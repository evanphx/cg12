# Gate: `integration/escape-wave2` (four-way merge) — verification and baseline regeneration

Branch `ccwork/escape-wave2-gate`, off `integration/escape-wave2` (`24a65d8`),
which merges four branches onto `main` (`4a6fd96`). The four merged jobs' own
reports are at `24a65d8:CCWORK_REPORT.md` (which is itself a naive concatenation
of two of them); earlier reports are at `4a6fd96:CCWORK_REPORT.md`.

Host: linux/arm64, 64 cores, `go version go1.26.1 linux/arm64`, `cc` = gcc.
Control worktree: `main` @ `4a6fd96` at `../main-control`, built and run by this
job, not quoted from anyone else's report.

STATUS: IN PROGRESS — this file is written as each suite exits.

---

## Results so far

### 4. `make test-unit` — PASS

Ran `go test -v -count=1` over `UNIT_PKGS` on both trees (the bare `make
test-unit` also passed on both, rc=0, no `(cached)` lines).

| | PASS | FAIL | SKIP |
|---|---|---|---|
| main `4a6fd96` | 1598 | 0 | 339 |
| branch `24a65d8` | 1607 | 0 | 339 |

Main reproduces the stated 1598/0 exactly. The +9 is fully attributed: diffing
the PASS *name sets* gives nine tests present on the branch and absent on main,
zero present on main and absent on the branch, and all nine are the new
`opt/escape_test.go` cases:

```
TestLowerHeapAllocationsCallsTheConversionHelperForAnEscapingObject
TestLowerHeapAllocationsEscapesAPayloadReadBackOutOfItsContainer
TestLowerHeapAllocationsEscapesOnlyThePayloadWhenTheCalleeRetainsAnElement
TestLowerHeapAllocationsFoldsBackWhenMoreThanOnePayloadEscapes
TestLowerHeapAllocationsFoldsThePayloadBackWhenTheContainerEscapes
TestLowerHeapAllocationsIgnoresAConversionWithoutItsStore
TestLowerHeapAllocationsKeepsBothWhenTheCalleeRetainsNothing
TestLowerHeapAllocationsPromotesAConvertedCandidateWithItsStore
TestLowerHeapAllocationsRecordsTheConversionHelperAsTheAllocator
```

`git diff main...HEAD -- opt/escape_test.go` adds exactly those nine `func Test`
lines and no others, from `iface-convt-fastpath` (`b798af9`) and
`variadic-escape-question` (`d5698c6`, `577e2d5`, `5b3a173`). No unit test was
lost or renamed. Nothing unattributed.

### Merge damage found by inspection (not a suite result)

Two things the merge carried in that are not baselines and are worth fixing
before this lands:

1. **22 MB of build artefacts were committed at the repository root.**
   `pkginit_dispatch` (12 193 904 B) and `runtime_package_initializer_dispatch`
   (10 695 104 B) are both `ELF 64-bit LSB executable, ARM aarch64 … with
   debug_info, not stripped` — goc-built reproduction binaries from the
   `iface-init-dispatch` job, added by `0f80c37` and `5c10aa4`. Nothing in the
   tree references either path (the only matches are for the `.go` sources of
   the same names). `.gitignore` already lists `/hello` and `/runtime_assembly`
   at the root for exactly this class. Recommend `git rm` and a `.gitignore`
   entry.

2. **`CCWORK_REPORT.md` was resolved by concatenation.** `24a65d8`'s copy opens
   with two H1 headings and two "Branch … off …" paragraphs stacked on top of
   each other, and the body interleaves the `variadic-escape-question` and
   `iface-init-dispatch` reports. It is not a merged document. This file
   replaces it; the previous content is at `24a65d8:CCWORK_REPORT.md`.
