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
