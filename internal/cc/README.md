# internal/cc — vendored fork of modernc.org/cc/v4

This is a fork of [`modernc.org/cc/v4`](https://modernc.org/cc/v4), a C99/C11
front end written in Go, rehomed under `github.com/evanphx/cg12/internal/cc` so
cg12 can modify it for its own use.

- **Upstream:** `modernc.org/cc/v4`
- **Forked at:** v4.29.1
- **License:** BSD-style, see `LICENSE` (unchanged from upstream).

## What changed from upstream

- Import path rehomed to `github.com/evanphx/cg12/internal/cc`; the package name
  stays `cc`. cg12's own `cc` package imports this one aliased as `moderncc`.
- The canonical-import comments (`// import "modernc.org/cc/v4"`) were removed.
- Upstream test files, `testdata/`, and the `fakecc.go` code generator were not
  vendored — cg12 validates the front end through its own `cc` package tests and
  the difftest corpus.

## Behavior fixes on top of v4.29.1

Called out here so the delta from upstream stays legible:

- **`__attribute__((packed))` is applied, not just recorded.** Upstream parses the
  attribute but lays structs/unions out with natural alignment anyway (`struct {
  char c; int i; }` reported as size 8 with `i` at 4, where C says size 5 and `i`
  at 1). The field allocator now removes inter-member padding and lowers the
  aggregate alignment to 1 for packed types, composing with `aligned(N)`. See
  `fieldAllocator.packed` / `packedAlign` in `check.go` and `Attributes.IsPacked`
  in `type.go`. (Packed *bitfield* allocation is still not modeled.)
- **The leading-attribute spelling is preserved.** `struct __attribute__((packed))
  S { ... }` had its attribute dropped: `structOrUnionSpecifier` in `parser.go`
  overwrote the leading attribute list with the (empty) post-`}` one. It now stores
  the trailing attributes in the second list, as the anonymous-struct case already
  did, so both spellings reach the type checker.

Beyond that, the sources are upstream's. Any further cg12-specific behavior changes
should be called out in commit messages so the delta from v4.29.1 stays legible.
