# cg12 Go standard library

This tree mirrors `GOROOT/src` for packages compiled by the cg12 Go frontend.
The initial files were copied unchanged from `/usr/local/go` and retain the Go
project’s license headers. `LICENSE` is the corresponding Go distribution
license.

The nested `go.mod` is only a boundary that keeps the repository's normal
`go test ./...` from treating GOROOT packages as module packages (which would
break Go's `internal` import rules). The cg12 loader uses `src` as a GOROOT-style
tree and does not resolve these packages by the nested module path.

The first ten packages, in the intended implementation order, are:

1. `internal/byteorder`
2. `math/bits`
3. `cmp`
4. `unicode/utf8`
5. `hash`
6. `errors`
7. `crypto/internal/fips140deps/byteorder`
8. `crypto/internal/fips140`
9. `crypto/internal/fips140/sha256`
10. `crypto/sha256`

Source activation is incremental. Packages in the active source set are parsed,
type-checked, and lowered from this tree. Other copied packages remain available
for the next compiler rung; they may temporarily use host export data when
mixing source and export-data versions would create incompatible Go type
identities.

The second ten-package progression is:

1. `encoding/binary`
2. `encoding/hex`
3. `hash/adler32`
4. `hash/crc32`
5. `hash/fnv`
6. `crypto/md5`
7. `crypto/sha1`
8. `crypto/sha512`
9. `unicode/utf16`
10. `path`

The third ten-package progression is:

1. `container/list`
2. `container/ring`
3. `sort`
4. `container/heap`
5. `maps`
6. `bufio`
7. `encoding/ascii85`
8. `encoding/base32`
9. `encoding/base64`
10. `encoding/csv`

All ten third-progression packages have linked ARM64 execution tests. These
tests compile the copied Go sources and their dependencies through cg12, link
them with the cg12-compiled runtime, and execute representative container,
sorting, heap, map, buffered I/O, encoding, and CSV operations. The buffered
I/O and CSV cases also read through `io.EOF`, exercising non-nil interface
equality and multi-result interface returns across ABIInternal calls.

The unchanged Go 1.26.1 `testing`, `regexp`, and `regexp/syntax` packages are
now active as well. `goc test` discovers same-package `TestXxx(*testing.T)`
functions and generates the ordinary `testing.Main` registration wrapper while
leaving all matching and execution to those copied packages. The complete
copied suites for `container/list`, `container/ring`, and `container/heap`
currently pass through this path. The complete copied suites for `unicode/utf8`,
`unicode/utf16`, `path`, and `hash/adler32` also pass, including their
allocation-count, interface, clone, and binary-marshaling tests.

The next package-test progression starts with `testing/iotest`, `log/internal`,
and `log`. Their sources are copied unchanged from the same Go 1.26.1 tree and
are compiled from this repository rather than resolved from host export data.
`encoding/json` is mirrored and active as the next dependency required by the
unchanged `net/netip` tests. Its unchanged tests now also source-load the copied
`math/big`, `net/http`, and `net/http/httptest` packages as the dependency
frontier expands.

The complete Go 1.26.1 `runtime` source tree is also mirrored here unchanged.
The cg12 loader build-selects and type-checks it from this repository. Normal
ARM64 executables compile the runtime through cg12, initialize the scheduler,
allocate traced heap objects, run garbage collection, and execute package init
tasks. Supported build-selected Plan 9 files, including the unchanged
`runtime/sys_linux_arm64.s`, are retained with their included headers and
translated after cg12 IR generation rather than replaced with generated Go or
handwritten syscall substitutes.

The unchanged `archive/tar` and `archive/zip` packages are mirrored and active.
Their cg12 status probes write and read in-memory archives, covering USTAR and
PAX tar headers, stored and deflated zip members, central-directory metadata,
and the zip reader's `fs.FS` implementation. The pure-Go `os/user` package is
also active to complete tar's Unix source dependency closure.
