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

Beyond that, the sources are upstream's. Any cg12-specific behavior changes made
from here on should be called out in commit messages so the delta from v4.29.1
stays legible.
