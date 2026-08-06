package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/evanphx/cg12/opt"
)

// A prebuilt runtime pack that carries part of the standard library costs what
// that library costs to compile once: 4.6 s for the runtime alone, but 154 s for
// one carrying net/http. That is worth paying once and never again, and it is not
// worth paying per build, per test run, or per shard -- so a compile keeps its
// results in a content-addressed cache.
//
// The key has to cover everything that can change the bytes of a pack, because a
// stale hit is not a slow build but a wrong one:
//
//   - the pack format version, and the target, -O and package list the caller asked for;
//   - the code placement policy and the optimization pipeline, both of which the
//     environment can override without changing a byte of the compiler;
//   - every other GOC_ and CG12_ environment variable, for the same reason and
//     because enumerating the ones that matter is exactly the audit that is easy
//     to get wrong -- see compilerEnvironmentIdentity;
//   - the goc binary itself, hashed, which covers every compiler change;
//   - the vendored standard library tree, hashed, which covers every source change;
//   - the C toolchain, because `cc` assembles the Plan 9 sidecar that is one of
//     the pack's two members -- see cToolchainIdentity for what "the C toolchain"
//     is taken to mean and what is still outside it.
//
// The key is computed in two halves. packCacheKeyPrefix covers everything above
// except the package list, and packCacheKeyForPrefix folds the package list in.
// The split exists so the cache can name a directory after the prefix and know
// that every pack indexed there was built by an identical configuration: the
// substitution rule in autopack.go compares one cached pack's package set against
// another's, and comparing across configurations would be the stale hit this
// whole comment is about.
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
	prefix, err := packCacheKeyPrefix(version, target, optimize, stdlibRoot)
	if err != nil {
		return "", err
	}
	return packCacheKeyForPrefix(prefix, packages), nil
}

// packCacheKeyPrefix is everything that determines a pack's contents except which
// packages it carries.
func packCacheKeyPrefix(version int, target string, optimize bool, stdlibRoot string) (string, error) {
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
	// And the rest of the environment, which is neither of those two and is not
	// nothing: GOC_ESCAPE_SUMMARIES=0 and GOC_PAYLOAD_FOLD=0 change escape
	// analysis, CG12_NO_IFCONVERT and CG12_FORCE_INLINE and CG12_NO_COSTINLINE
	// and CG12_NO_AGGINLINE and GOC_NO_NOSPLIT_INLINE change the optimizer,
	// GOC_NOSPLIT_LIMIT changes the frame budget that decides nosplit inlining,
	// and GOC_STDLIB_OVERLAY changes which files a standard library package is
	// built from -- which the hashed tree below cannot see, because the tree is
	// the same and only the selection differs.
	fmt.Fprintf(digest, "environment=%s;\n", compilerEnvironmentIdentity())

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

// packCacheKeyForPrefix folds a package list into a prefix. The list is sorted
// and de-duplicated, so the same request always names the same key however the
// caller spelled it.
func packCacheKeyForPrefix(prefix string, packages []string) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "prefix=%s;\npackages=%s;\n", prefix, strings.Join(canonicalPackageList(packages), ","))
	return hex.EncodeToString(digest.Sum(nil))
}

// canonicalPackageList sorts and de-duplicates a pack's package list.
func canonicalPackageList(packages []string) []string {
	sorted := append([]string(nil), packages...)
	sort.Strings(sorted)
	unique := sorted[:0]
	for index, path := range sorted {
		if index > 0 && path == sorted[index-1] {
			continue
		}
		unique = append(unique, path)
	}
	return unique
}

// packCacheControlVariables are the GOC_/CG12_ variables that select *where* a
// pack is cached rather than *what* is in it. They are the only ones left out of
// the key: folding them in would make CG12_PACK_CACHE=/tmp/x and
// CG12_PACK_CACHE=/tmp/y name different packs, which is precisely backwards.
var packCacheControlVariables = map[string]bool{
	"CG12_NOCACHE":              true,
	"CG12_PACK_CACHE":           true,
	"CG12_PACK_CACHE_MAX_BYTES": true,
	"GOC_AUTOPACK":              true,
	"GOC_AUTOPACK_DEBUG":        true,
}

// compilerEnvironmentIdentity is every GOC_ and CG12_ environment variable, name
// and value, sorted.
//
// Blunt on purpose. The alternative is a list of the variables that are known to
// change what the compiler emits, and that list is a thing a person maintains: a
// switch added to the optimizer and not added to the list is a silent miscompile,
// and the switches exist precisely so that a corpus can be built several ways.
// Sweeping the whole namespace cannot miss one. What it costs is a miss when a
// diagnostic-only variable is set -- CG12_DUMP_IR, GOC_DEBUG_NOSPLIT -- and a
// miss is a slow build, which is the direction this cache is allowed to be wrong
// in.
func compilerEnvironmentIdentity() string {
	var entries []string
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(name, "GOC_") && !strings.HasPrefix(name, "CG12_") {
			continue
		}
		if packCacheControlVariables[name] {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	return strings.Join(entries, "\x1f")
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
// of the cache is that those cases hit.
//
// The files are hashed concurrently and folded in sorted order, so the result is
// the same on any machine while the cost is not. That mattered less when only
// `goc build-runtime` computed a key; it is now paid by every compile, and 5423
// files and 73 MB is 256 ms of one core against 30 ms of several. Each file's
// digest is taken separately and only the digests are folded, which is what makes
// the concurrency possible at all: streaming contents into one hash forces the
// order of the bytes to be the order of the work.
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

	digests := make([]string, len(paths))
	failures := make([]error, len(paths))
	workers := runtime.GOMAXPROCS(0)
	if workers > len(paths) {
		workers = len(paths)
	}
	if workers < 1 {
		workers = 1
	}
	next := make(chan int, len(paths))
	for index := range paths {
		next <- index
	}
	close(next)
	var hashing sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		hashing.Add(1)
		go func() {
			defer hashing.Done()
			for index := range next {
				digests[index], failures[index] = hashFile(paths[index])
			}
		}()
	}
	hashing.Wait()

	for index, path := range paths {
		if failures[index] != nil {
			return failures[index]
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(digest, "tree=%s;file=%s;%s\n", relative, filepath.Base(path), digests[index])
	}
	return nil
}

// hashFile is the SHA-256 of one file's contents.
func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// cToolchainIdentity identifies the `cc` that will assemble the pack's sidecar.
//
// The banner used to be all of it, and it was named as the key's weakest link:
// it identifies a toolchain *release*, not a particular assembler, so a rebuilt
// or locally patched `as` with an unchanged version string was a stale hit
// waiting to happen. That was tolerable while a pack was only produced by an
// explicit `goc build-runtime`. It is not tolerable now that the key decides
// every compile, so the identity is three things instead of one:
//
//   - the banner, which is cheap and names the release;
//   - the bytes of the `cc` driver and of the `as` it says it will exec, which
//     is exact for those two binaries;
//   - the bytes of the object `cc` actually produces for a fixed probe, which is
//     the only one of the three that measures behaviour rather than provenance,
//     and so is the one that catches a changed shared library underneath an
//     unchanged `as`.
//
// What is still outside it: the assembler's behaviour on instructions the probe
// does not contain. The probe is a sample, not a proof -- but it is a sample of
// exactly the constructs the sidecar is made of (a frame, an adrp/lo12 pair, a
// call relocation to an undefined symbol, an FP instruction, an aligned datum),
// which is a strictly better guarantee than a version string.
//
// A failure at any step is folded into the key as itself, so a box without a
// compiler does not silently share a key with one that has it.
//
// Measured once per process: it runs two subprocesses and hashes a few megabytes,
// and every key a process computes shares the answer.
func cToolchainIdentity() string {
	cToolchainIdentityOnce.Do(func() {
		cToolchainIdentityValue = measureCToolchainIdentity()
	})
	return cToolchainIdentityValue
}

var (
	cToolchainIdentityOnce  sync.Once
	cToolchainIdentityValue string
)

func measureCToolchainIdentity() string {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		return "cc=absent"
	}
	identity := &strings.Builder{}
	if banner, err := exec.Command(compiler, "--version").Output(); err != nil {
		identity.WriteString("cc=unidentified;")
	} else {
		line, _, _ := strings.Cut(string(banner), "\n")
		fmt.Fprintf(identity, "cc=%s;", line)
	}
	fmt.Fprintf(identity, "ccbytes=%s;", fileDigest(compiler))
	fmt.Fprintf(identity, "asbytes=%s;", fileDigest(assemblerPath(compiler)))
	fmt.Fprintf(identity, "probe=%s;", assemblerProbeDigest(compiler))
	return identity.String()
}

// assemblerPath asks the driver which assembler it will exec. An answer that is
// not an existing file -- a bare "as" to be found on PATH, or a driver that does
// not understand the question -- is returned as-is, and fileDigest reports it as
// unreadable rather than pretending to have hashed something.
func assemblerPath(compiler string) string {
	output, err := exec.Command(compiler, "-print-prog-name=as").Output()
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		return ""
	}
	if resolved, err := exec.LookPath(path); err == nil {
		return resolved
	}
	return path
}

// fileDigest is the SHA-256 of a file, or a marker naming why there is none.
func fileDigest(path string) string {
	if path == "" {
		return "absent"
	}
	digest := sha256.New()
	file, err := os.Open(path)
	if err != nil {
		return "unreadable:" + filepath.Base(path)
	}
	defer file.Close()
	if _, err := io.Copy(digest, file); err != nil {
		return "unreadable:" + filepath.Base(path)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// assemblerProbe is a fixed input whose translation exercises the parts of the
// assembler the Plan 9 sidecar depends on. It is deliberately never linked or
// run: only the bytes `cc -c` produces for it are used.
const assemblerProbe = `.text
.balign 16
.globl cg12_pack_cache_probe
cg12_pack_cache_probe:
	stp x29, x30, [sp, #-16]!
	mov x29, sp
	adrp x0, cg12_pack_cache_probe_data
	add x0, x0, :lo12:cg12_pack_cache_probe_data
	ldr x1, [x0]
	fmov d0, x1
	fadd d0, d0, d0
	bl cg12_pack_cache_probe_callee
	ldp x29, x30, [sp], #16
	ret
.data
.balign 8
cg12_pack_cache_probe_data:
	.quad 0x0123456789abcdef
`

// assemblerProbeDigest is the SHA-256 of the object cc produces for
// assemblerProbe.
//
// The probe is written under a fixed base name, because a driver that records
// the input's name in the object would otherwise make the digest depend on the
// temporary directory. Verified byte-identical across directories on this host
// before being made part of the key.
func assemblerProbeDigest(compiler string) string {
	work, err := os.MkdirTemp("", "cg12-cc-probe")
	if err != nil {
		return "unavailable"
	}
	defer os.RemoveAll(work)
	source := filepath.Join(work, "probe.S")
	object := filepath.Join(work, "probe.o")
	if err := os.WriteFile(source, []byte(assemblerProbe), 0o644); err != nil {
		return "unavailable"
	}
	// The probe assembles for the host, which is what the sidecar does too. A
	// cross-targeted goc that could not assemble its own sidecar cannot build a
	// pack at all, so there is nothing here to make architecture-aware.
	if err := exec.Command(compiler, "-c", "-o", object, source).Run(); err != nil {
		return "unassemblable"
	}
	return fileDigest(object)
}

// cachedPackPath names where a key's pack lives, whether or not it is there.
func cachedPackPath(directory, key string) string {
	return filepath.Join(directory, key+".gocrt")
}

// cachedPack reports whether a key's pack is in the cache, and marks it as used.
//
// The mark is the file's modification time, which is what trimPackCache evicts
// by. Using mtime for both means a pack that every build links against is never
// the oldest thing in the cache, which is the whole point of evicting by use
// rather than by age.
func cachedPack(directory, key string) (string, bool) {
	if directory == "" {
		return "", false
	}
	path := cachedPackPath(directory, key)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return path, true
}

// readCachedPack copies a cached pack to destination, reporting whether there was
// one. A cache directory that cannot be read is not an error: the caller just
// builds the pack.
func readCachedPack(directory, key, destination string) bool {
	path, ok := cachedPack(directory, key)
	if !ok {
		return false
	}
	contents, err := os.ReadFile(path)
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
	return writeFileAtomically(cachedPackPath(directory, key), contents)
}

// packIndexEntry is what the cache records about a pack besides its bytes: the
// package list it was asked for and the closure that list turned out to load.
//
// It is a file of its own rather than a field read back from the pack because
// the reader is a decision made before any pack is opened, and because the
// directory it lives in is named after the key prefix -- which is what makes an
// entry safe to compare against. A manifest read straight out of a `.gocrt` in
// the cache directory carries no evidence of which compiler built it: the
// fingerprint covers the runtime source and nothing else, so two packs built by
// different compilers from the same tree have the same one. Only the key knows,
// and the key is a hash nobody can invert. Hence: the cache is never consulted by
// scanning packs, only by naming keys, and the index is how a key gets named.
type packIndexEntry struct {
	Key      string   `json:"key"`
	Packages []string `json:"packages"`
	Closure  []string `json:"closure"`
}

func packIndexDirectory(directory, prefix string) string {
	return filepath.Join(directory, "index", prefix)
}

// writePackIndexEntry records a freshly cached pack in the prefix's index.
func writePackIndexEntry(directory, prefix, key string, manifest *runtimepack.Manifest) error {
	if directory == "" {
		return nil
	}
	indexDirectory := packIndexDirectory(directory, prefix)
	if err := os.MkdirAll(indexDirectory, 0o755); err != nil {
		return err
	}
	encoded, err := json.Marshal(packIndexEntry{
		Key:      key,
		Packages: canonicalPackageList(manifest.Packages),
		Closure:  manifest.Closure,
	})
	if err != nil {
		return err
	}
	return writeFileAtomically(filepath.Join(indexDirectory, key+".json"), encoded)
}

// readPackIndex reads every entry the prefix's index holds. An unreadable index
// is an empty one: it can only cost a pack build.
func readPackIndex(directory, prefix string) []packIndexEntry {
	if directory == "" {
		return nil
	}
	names, err := os.ReadDir(packIndexDirectory(directory, prefix))
	if err != nil {
		return nil
	}
	entries := make([]packIndexEntry, 0, len(names))
	for _, name := range names {
		if name.IsDir() || !strings.HasSuffix(name.Name(), ".json") {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(packIndexDirectory(directory, prefix), name.Name()))
		if err != nil {
			continue
		}
		var entry packIndexEntry
		if err := json.Unmarshal(contents, &entry); err != nil {
			continue
		}
		if entry.Key == "" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// withPackBuildLock runs build with this key's build lock held, so that the
// hundreds of compiles a test suite starts at once do not all build the same
// 98 MB pack.
//
// The lock is an optimization and never a correctness requirement. Everything it
// guards is already safe without it -- writeFileAtomically means a reader never
// sees a partial pack and two writers both end with a whole one -- so every way
// of failing to take it ends in building anyway rather than in failing. That
// includes the wait giving up: a peer that has held the lock for longer than any
// pack takes to build is a peer that is stuck, and blocking on it forever would
// turn one stuck process into a stuck machine.
//
// The kernel drops a flock when the holding process dies, however it dies, so a
// killed builder does not leave the cache wedged.
func withPackBuildLock(directory, key string, build func() error) error {
	if directory == "" {
		return build()
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return build()
	}
	unlock, ok := acquireFileLock(filepath.Join(directory, key+".lock"), packBuildLockWait)
	if ok {
		defer unlock()
	}
	return build()
}

// packBuildLockWait is longer than the longest pack build measured on this tree
// (154 s, for net/http) by enough of a margin that waiting is always the better
// bet, and short enough that a stuck peer costs one build's worth of latency
// rather than the job.
const packBuildLockWait = 15 * time.Minute

// packCacheBudget is how many bytes of packs the cache may hold. Nothing evicted
// them before this, and by the time it was measured the shared cache on the build
// host held 985 packs and 45 GB, written in a week, from opt-in `build-runtime`
// calls alone. Making the cache the default multiplies that, so it gets a bound
// in the same change rather than a follow-up.
//
// 20 GiB because a pack ranges from 8 MB (runtime alone) to 98 MB (net/http), and
// a working set is the packs of the configurations under active development:
// two -O arms times the handful of distinct closures a corpus produces, times a
// couple of compiler builds still being switched between. That is single-digit
// gigabytes, so 20 GiB keeps every live configuration and evicts the builds
// nobody is on any more.
//
// CG12_PACK_CACHE_MAX_BYTES overrides it; 0 means unbounded.
const packCacheBudget = 20 << 30

func packCacheBudgetBytes() int64 {
	setting := os.Getenv("CG12_PACK_CACHE_MAX_BYTES")
	if setting == "" {
		return packCacheBudget
	}
	value, err := strconv.ParseInt(setting, 10, 64)
	if err != nil || value < 0 {
		return packCacheBudget
	}
	return value
}

// trimPackCache evicts the least recently used packs until the cache is inside
// its budget.
//
// Least recently used, not oldest: cachedPack touches a pack on every hit, so a
// pack every build links against keeps its place however long ago it was built.
//
// Unlinking a pack another process is reading is safe and is the reason this
// needs no coordination with readers: the reader holds an open file, and POSIX
// keeps the inode alive until it closes. A reader that has not opened it yet
// fails its read and builds the pack again, which is a miss, not a fault.
//
// Two trimmers at once would each delete more than they needed to, so the sweep
// takes a non-blocking lock and skips the pass if another process holds it.
func trimPackCache(directory string) {
	budget := packCacheBudgetBytes()
	if directory == "" || budget == 0 {
		return
	}
	unlock, ok := acquireFileLock(filepath.Join(directory, "trim.lock"), 0)
	if !ok {
		return
	}
	defer unlock()

	names, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	type cached struct {
		key   string
		bytes int64
		used  time.Time
	}
	var packs []cached
	var total int64
	for _, name := range names {
		if name.IsDir() || !strings.HasSuffix(name.Name(), ".gocrt") {
			continue
		}
		info, err := name.Info()
		if err != nil {
			continue
		}
		packs = append(packs, cached{
			key:   strings.TrimSuffix(name.Name(), ".gocrt"),
			bytes: info.Size(),
			used:  info.ModTime(),
		})
		total += info.Size()
	}
	if total <= budget {
		return
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].used.Before(packs[j].used) })
	for _, pack := range packs {
		if total <= budget {
			return
		}
		if err := os.Remove(cachedPackPath(directory, pack.key)); err != nil {
			continue
		}
		total -= pack.bytes
		removePackIndexEntries(directory, pack.key)
	}
}

// removePackIndexEntries drops the index entries of an evicted pack. The index is
// small and its prefix directories are few, so this walks them rather than
// remembering which prefix an entry was written under -- and a stale entry is
// only a wasted stat anyway, since every reader checks that the pack still
// exists.
func removePackIndexEntries(directory, key string) {
	prefixes, err := os.ReadDir(filepath.Join(directory, "index"))
	if err != nil {
		return
	}
	for _, prefix := range prefixes {
		if !prefix.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(directory, "index", prefix.Name(), key+".json"))
	}
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
