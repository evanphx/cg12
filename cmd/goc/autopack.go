package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/internal/runtimepack"
)

// Auto-packing is what makes the on-disk cache the default rather than a
// two-step opt-in. A plain `goc file.go` works out which standard library
// packages the program names, finds the cached pack that carries them, builds and
// caches one if there is no usable hit, and links against it -- which is
// `goc build-runtime` followed by `goc -runtime`, without either step being
// typed.
//
// It exists because the two caches goc already had did not help the case anyone
// actually runs. goc/source_world.go caches parsed and type-checked packages in
// memory, so a test binary compiling four hundred programs gets nearly everything
// free and a command-line compile -- a fresh process -- gets nothing. The pack
// cache is on disk and does help, but only `goc build-runtime` ever wrote to it.
// So an ordinary compile of a 168-line program built a module of 5101 functions,
// the whole standard library closure, every single time.
//
// # What the pack carries
//
// Exactly the standard library packages the file's own import declarations name.
// Not the closure of those imports -- the pack's root blank-imports the list and
// the front end computes the closure itself -- and not a fixed ladder of package
// sets.
//
// A ladder was the obvious alternative and it is the wrong shape. A pack is
// usable by a program only if the program's closure *contains* the pack's, so a
// ladder can only ever be rounded down: a program importing net/http and finding
// no net/http tier falls back to the runtime-only tier and recompiles net/http
// from source. That is the whole cost. Compile cost across this tree's capability
// matrix is sharply bimodal -- eleven programs cost 125-167 s each and are 54% of
// all compile CPU, the other 327 average 4.2 s -- and the eleven are exactly the
// ones a rounded-down tier fails to help. So: build the pack the program needs.
//
// # What that costs, and what pays for it
//
// One pack per distinct import list, and a net/http pack is 98 MB. Two things
// keep that from filling the disk with near-duplicates.
//
// The first is that an import list is already a much coarser thing than a
// program: every program importing exactly {fmt, os} shares one pack whatever
// else it does.
//
// The second is the substitution rule in substitutePack, which collapses import
// lists that differ only in packages another import already pulls in. It is not a
// heuristic -- it is an equality, proved there -- so it costs nothing in fit.
//
// What is left over is bounded rather than eliminated: see trimPackCache.
//
// # When it does not apply
//
// CG12_NOCACHE=1 turns it off along with every other cache, which is what the
// merge gates rely on. GOC_AUTOPACK=0 turns off just this, leaving the in-memory
// world alone, which is what a fair measurement of the uncached compile needs.
// Anything that is not a plain arm64 executable build -- -c, -S, -emit-ir,
// -runtime-covermeta, an explicit -runtime, a non-arm64 target -- is left as it
// was. So is -m: escape diagnostics are printed by a pass that runs after the
// split, over a module the split has taken definitions out of, so a compile under
// -m has to be the whole-program compile it has always been.

// autoPackEnabled reports whether a compile may use a cached pack of its own
// accord.
func autoPackEnabled() bool {
	if os.Getenv("CG12_NOCACHE") != "" {
		return false
	}
	switch os.Getenv("GOC_AUTOPACK") {
	case "0", "off", "no":
		return false
	}
	return true
}

// autoPackDebugf reports what the cache did, when asked. Silent otherwise: the
// fallbacks below are all "compile the way goc always has", and a compiler that
// narrated its cache on every run would be noise in every test that compares
// output.
func autoPackDebugf(diagnostics io.Writer, format string, arguments ...any) {
	if diagnostics == nil || os.Getenv("GOC_AUTOPACK_DEBUG") == "" {
		return
	}
	fmt.Fprintf(diagnostics, "goc: autopack: "+format+"\n", arguments...)
}

// autoPackFor resolves the prebuilt runtime this program should link against,
// building and caching one if the cache has no usable pack.
//
// A nil result means "compile the way goc did before", and every failure here
// produces one. That is deliberate and it is the safety argument for turning this
// on by default: a cache that cannot arrange itself falls back to the compile
// that needs no cache, so the worst outcome available to it is the old cost.
func autoPackFor(target goc.Target, optimize bool, source []byte, diagnostics io.Writer) *packSet {
	if !autoPackEnabled() {
		autoPackDebugf(diagnostics, "disabled")
		return nil
	}
	if target != goc.TargetARM64 {
		// A prebuilt runtime is arm64-only; other targets compile the
		// freestanding subset and have no runtime to prebuild.
		return nil
	}
	directory := packCacheDirectory()
	if directory == "" {
		autoPackDebugf(diagnostics, "no cache directory")
		return nil
	}
	packages, err := stdlibImportsOf(source)
	if err != nil {
		// A file that does not parse is not this code's business to report: the
		// whole-program compile below will report it, with position information
		// and in the form every other syntax error takes.
		autoPackDebugf(diagnostics, "not parsed: %v", err)
		return nil
	}
	// The key is computed on every compile now, not once per `build-runtime`, so
	// what it costs is a tax on every build and is reported rather than assumed:
	// it hashes the goc binary, the 73 MB vendored tree and the C toolchain.
	keying := time.Now()
	prefix, err := packCacheKeyPrefix(runtimepack.Version, string(target), optimize, goc.StdlibRoot())
	autoPackDebugf(diagnostics, "key computed in %s", time.Since(keying).Round(time.Millisecond))
	if err != nil {
		autoPackDebugf(diagnostics, "no cache key: %v", err)
		return nil
	}
	resolving := time.Now()
	path, err := resolveAutoPack(target, optimize, directory, prefix, packages, diagnostics)
	autoPackDebugf(diagnostics, "pack resolved in %s", time.Since(resolving).Round(time.Millisecond))
	if err != nil {
		autoPackDebugf(diagnostics, "no pack: %v", err)
		return nil
	}
	set, err := readPackSet([]string{path})
	if err != nil {
		// The pack was there a moment ago and is not now, or is unreadable. An
		// eviction between the two is the ordinary way this happens.
		autoPackDebugf(diagnostics, "unreadable pack %s: %v", filepath.Base(path), err)
		return nil
	}
	return set
}

// resolveAutoPack returns the path of a pack this program can link against.
func resolveAutoPack(target goc.Target, optimize bool, directory, prefix string, packages []string, diagnostics io.Writer) (string, error) {
	key := packCacheKeyForPrefix(prefix, packages)
	if path, ok := cachedPack(directory, key); ok {
		autoPackDebugf(diagnostics, "hit %s for %v", key[:12], packages)
		return path, nil
	}
	if path, entry, ok := substitutePack(directory, prefix, packages); ok {
		autoPackDebugf(diagnostics, "substituted %s (carries %v) for %v", entry.Key[:12], entry.Packages, packages)
		return path, nil
	}
	autoPackDebugf(diagnostics, "miss %s for %v; building", key[:12], packages)
	return buildAndCachePack(target, optimize, directory, prefix, key, packages, diagnostics)
}

// substitutePack finds a cached pack that is not merely usable by this program
// but carries exactly as much as the pack this program would have built.
//
// The test is two containments on one cached pack C, against the package list P
// this program asked for:
//
//	C.Packages ⊆ P ⊆ C.Closure
//
// and when both hold, C's closure and the closure of the pack built from P are
// the same set. Write cl(S) for what a pack root importing S loads -- the runtime
// closure plus the closure of S -- so C.Closure = cl(C.Packages). Then
// C.Packages ⊆ P gives cl(C.Packages) ⊆ cl(P) because cl is monotone, and
// P ⊆ cl(C.Packages) gives cl(P) ⊆ cl(cl(C.Packages)) = cl(C.Packages) because cl
// is idempotent. The two together are cl(P) = C.Closure. And a program whose
// closure contains cl(P) therefore contains C.Closure, which is precisely
// chooseManifest's condition, so C is usable.
//
// This is what stops the cache filling with near-duplicates. A program importing
// {fmt, net/http} does not build a second 98 MB pack next to the {net/http} one:
// fmt is in net/http's closure, so the two packs would be the same pack.
//
// It is order-dependent and deliberately not more than that. If {fmt, net/http}
// is compiled first, its pack is the one that exists, and a later {net/http}
// program does not match it -- {fmt, net/http} ⊄ {net/http} -- and builds its
// own. Making that case collapse too would mean knowing each package's closure
// before compiling anything, which is a second import resolver to write, keep
// correct against build tags, and be wrong in. The cache carries the duplicate
// instead, and trimPackCache bounds what that can grow to.
//
// Candidates are read from the prefix's index, never by scanning the cache
// directory: see packIndexEntry for why a pack in the cache is not evidence of
// anything until a key names it.
func substitutePack(directory, prefix string, packages []string) (string, packIndexEntry, bool) {
	wanted := canonicalPackageList(packages)
	best := packIndexEntry{}
	bestPath := ""
	for _, entry := range readPackIndex(directory, prefix) {
		if !containsAll(wanted, entry.Packages) {
			continue
		}
		if !containsAll(entry.Closure, wanted) {
			continue
		}
		if bestPath != "" && len(entry.Closure) <= len(best.Closure) {
			continue
		}
		path, ok := cachedPack(directory, entry.Key)
		if !ok {
			continue
		}
		best, bestPath = entry, path
	}
	return bestPath, best, bestPath != ""
}

// containsAll reports whether every element of subset is in set.
func containsAll(set, subset []string) bool {
	if len(subset) == 0 {
		return true
	}
	present := make(map[string]bool, len(set))
	for _, value := range set {
		present[value] = true
	}
	for _, value := range subset {
		if !present[value] {
			return false
		}
	}
	return true
}

// buildAndCachePack builds the pack for a package list and stores it.
//
// Two layers of exclusion, because there are two ways for the same pack to be
// built twice at once. inProcessPackLocks covers the compile-batch worker, which
// compiles many programs in one process. The file lock covers the far more common
// case of a suite starting dozens of one-shot compiles at once, all of which want
// the same pack and none of which can see the others.
//
// Both re-check the cache after taking their lock, since the point of waiting is
// that the peer you waited for has left you a pack.
func buildAndCachePack(target goc.Target, optimize bool, directory, prefix, key string, packages []string, diagnostics io.Writer) (string, error) {
	release := lockPackKeyInProcess(key)
	defer release()
	if path, ok := cachedPack(directory, key); ok {
		return path, nil
	}

	var path string
	err := withPackBuildLock(directory, key, func() error {
		if cached, ok := cachedPack(directory, key); ok {
			autoPackDebugf(diagnostics, "another build cached %s while we waited", key[:12])
			path = cached
			return nil
		}
		pack, err := prebuilt.BuildRuntime(target, prebuilt.Options{
			Optimize: optimize,
			Packages: packages,
		})
		if err != nil {
			return err
		}
		encoded, err := pack.Marshal()
		if err != nil {
			return err
		}
		if err := writeCachedPack(directory, key, encoded); err != nil {
			return err
		}
		if err := writePackIndexEntry(directory, prefix, key, &pack.Manifest); err != nil {
			// The pack is cached and usable; only the substitution rule is
			// poorer for this, and only by one entry.
			autoPackDebugf(diagnostics, "could not index %s: %v", key[:12], err)
		}
		path = cachedPackPath(directory, key)
		return nil
	})
	if err != nil {
		return "", err
	}
	trimPackCache(directory)
	return path, nil
}

var (
	inProcessPackLocksMutex sync.Mutex
	inProcessPackLocks      = map[string]*sync.Mutex{}
)

// lockPackKeyInProcess serializes this process's builds of one key.
func lockPackKeyInProcess(key string) func() {
	inProcessPackLocksMutex.Lock()
	lock := inProcessPackLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		inProcessPackLocks[key] = lock
	}
	inProcessPackLocksMutex.Unlock()

	lock.Lock()
	return lock.Unlock
}

// stdlibImportsOf is the standard library packages a program's import
// declarations name, sorted and de-duplicated.
//
// Only the import declarations are parsed, which is why this is affordable on
// every compile: it reads the head of one file, not a package graph.
//
// Two kinds of path are dropped. "unsafe" and "C" are not packages a pack root
// can blank-import. Anything with no directory under the vendored tree is not a
// standard library package at all -- it would fail the pack build, and the
// program's own compile will report it far better than a cache-shaped error
// would. Dropping either can only make the pack carry less than the program
// loads, which is still a pack the program can use.
func stdlibImportsOf(source []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "goc-autopack.go", source, parser.ImportsOnly|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(goc.StdlibRoot(), "src")
	var packages []string
	for _, declared := range file.Imports {
		path, err := strconv.Unquote(declared.Path.Value)
		if err != nil {
			continue
		}
		if path == "unsafe" || path == "C" {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil || !info.IsDir() {
			continue
		}
		packages = append(packages, path)
	}
	sort.Strings(packages)
	return canonicalPackageList(packages), nil
}
