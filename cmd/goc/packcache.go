package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/internal/cachefile"
	"github.com/evanphx/cg12/opt"
)

// A prebuilt runtime pack that carries part of the standard library costs what
// that library costs to compile once: 4.6 s for the runtime alone, but 154 s for
// one carrying net/http. That is worth paying once and never again, and it is not
// worth paying per build, per test run, or per shard -- so `goc build-runtime`
// keeps its results in a content-addressed cache.
//
// The key has to cover everything that can change the bytes of a pack, because a
// stale hit is not a slow build but a wrong one:
//
//   - the pack format version, and the target, -O and package list the caller asked for;
//   - the code placement policy and the optimization pipeline, both of which the
//     environment can override without changing a byte of the compiler;
//   - the goc binary itself, hashed, which covers every compiler change;
//   - the vendored standard library tree, hashed, which covers every source change;
//   - the C toolchain's version banner, because `cc` assembles the Plan 9 sidecar
//     that is one of the pack's two members.
//
// The last one is the weakest link and is named as such: a banner identifies a
// toolchain release, not a particular assembler binary. Everything else is exact.
//
// CG12_NOCACHE=1 bypasses the cache entirely, which is the same switch
// goc/source_world.go already answers to.
//
// The mechanics -- where a key goes on disk, how it is written, how a tree is
// hashed, and when an entry nobody has read in five days goes away -- are
// internal/cachefile's, shared with the per-function cache. This file keeps only
// what is specific to a pack: which clauses go into the key.

// packCacheDirectory is where cached packs live. CG12_PACK_CACHE overrides it,
// and an empty result means "do not cache".
func packCacheDirectory() string {
	return cachefile.Directory("CG12_PACK_CACHE", "runtime-pack")
}

// packCacheKey identifies a pack by everything that determines its contents.
func packCacheKey(version int, target string, optimize bool, packages []string, stdlibRoot string) (string, error) {
	digest := sha256.New()
	fmt.Fprintf(digest, "packversion=%d;target=%s;optimize=%v;\n", version, target, optimize)
	// The code placement policy, which the compiler binary's bytes do not cover:
	// it can be overridden from the environment so a corpus can be built several
	// ways, and a pack laid out under one policy is the wrong pack under another.
	fmt.Fprintf(digest, "textlayout=%s;\n", arm64.TextLayoutIdentity())
	// The optimization pipeline, for the same reason: GOC_OPT_PIPELINE and
	// GOC_OPT_SKIP select which passes run, and a pack built by one pipeline is
	// the wrong pack under another. Without this a bisection run would link the
	// pack the previous run cached and measure the arm it was trying to rule out.
	fmt.Fprintf(digest, "pipeline=%s;\n", opt.PipelineIdentity())
	sorted := append([]string(nil), packages...)
	sort.Strings(sorted)
	fmt.Fprintf(digest, "packages=%s;\n", strings.Join(sorted, ","))

	compiler, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := cachefile.HashFileInto(digest, compiler); err != nil {
		return "", err
	}
	if err := cachefile.HashTreeInto(digest, stdlibRoot); err != nil {
		return "", err
	}
	digest.Write([]byte(cToolchainIdentity()))
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// cToolchainIdentity is the version banner of the `cc` that will assemble the
// pack's sidecar. A failure to run it is folded into the key as itself, so a box
// without a compiler does not silently share a key with one that has it.
func cToolchainIdentity() string {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		return "cc=absent"
	}
	banner, err := exec.Command(compiler, "--version").Output()
	if err != nil {
		return "cc=unidentified"
	}
	line, _, _ := strings.Cut(string(banner), "\n")
	return "cc=" + line
}

// readCachedPack copies a cached pack to destination, reporting whether there was
// one. A cache directory that cannot be read is not an error: the caller just
// builds the pack.
func readCachedPack(directory, key, destination string) bool {
	contents, found := cachefile.Read(directory, key, ".gocrt")
	if !found {
		return false
	}
	return cachefile.WriteFileAtomically(destination, contents) == nil
}

// writeCachedPack stores a pack under its key. A failure to store is not a build
// failure -- the pack has already been written where the caller asked for it --
// so it is reported for the caller to mention rather than returned as an error.
func writeCachedPack(directory, key string, contents []byte) error {
	// The one moment a pack build is guaranteed to be slow anyway, and so the one
	// moment it is worth walking the directory to evict. Trim rate-limits itself
	// to once a day per directory.
	cachefile.Trim(directory)
	return cachefile.Write(directory, key, ".gocrt", contents)
}
