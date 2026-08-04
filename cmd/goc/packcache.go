package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanphx/cg12/arm64"
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

// packCacheDirectory is where cached packs live. CG12_PACK_CACHE overrides it,
// and an empty result means "do not cache".
func packCacheDirectory() string {
	if os.Getenv("CG12_NOCACHE") != "" {
		return ""
	}
	if directory := os.Getenv("CG12_PACK_CACHE"); directory != "" {
		return directory
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "cg12", "runtime-pack")
}

// packCacheKey identifies a pack by everything that determines its contents.
func packCacheKey(version int, target string, optimize bool, packages []string, stdlibRoot string) (string, error) {
	digest := sha256.New()
	fmt.Fprintf(digest, "packversion=%d;target=%s;optimize=%v;\n", version, target, optimize)
	// The code placement policy, which the compiler binary's bytes do not cover:
	// it can be overridden from the environment so a corpus can be built several
	// ways, and a pack laid out under one policy is the wrong pack under another.
	fmt.Fprintf(digest, "textlayout=%s;\n", arm64.TextLayoutIdentity())
	sorted := append([]string(nil), packages...)
	sort.Strings(sorted)
	fmt.Fprintf(digest, "packages=%s;\n", strings.Join(sorted, ","))

	compiler, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := hashFileInto(digest, compiler); err != nil {
		return "", err
	}
	if err := hashTreeInto(digest, stdlibRoot); err != nil {
		return "", err
	}
	digest.Write([]byte(cToolchainIdentity()))
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func hashFileInto(digest io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	fmt.Fprintf(digest, "file=%s;\n", filepath.Base(path))
	_, err = io.Copy(digest, file)
	return err
}

// hashTreeInto folds every regular file under root into digest, by relative path
// and content.
//
// Content rather than modification time: a checkout, a rebase or a worktree copy
// all change mtimes without changing what the compiler reads, and the whole point
// of the cache is that those cases hit. The vendored tree is 73 MB and hashes in
// under 0.2 s warm, which is nothing against the 154 s it can save.
func hashTreeInto(digest io.Writer, root string) error {
	var paths []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(digest, "tree=%s;\n", relative)
		if err := hashFileInto(digest, path); err != nil {
			return err
		}
	}
	return nil
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
	if directory == "" {
		return false
	}
	contents, err := os.ReadFile(filepath.Join(directory, key+".gocrt"))
	if err != nil {
		return false
	}
	return writeFileAtomically(destination, contents) == nil
}

// writeCachedPack stores a pack under its key. A failure to store is not a build
// failure -- the pack has already been written where the caller asked for it --
// so it is reported for the caller to mention rather than returned as an error.
func writeCachedPack(directory, key string, contents []byte) error {
	if directory == "" {
		return nil
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return writeFileAtomically(filepath.Join(directory, key+".gocrt"), contents)
}

// writeFileAtomically writes through a temporary file in the same directory and
// renames it, so a reader never sees a half-written pack and two builds racing on
// one key both end with a whole file.
func writeFileAtomically(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	_, writeErr := temporary.Write(contents)
	closeErr := temporary.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(name)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
