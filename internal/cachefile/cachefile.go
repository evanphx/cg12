// Package cachefile is the disk mechanics every cg12 on-disk cache shares: how a
// key becomes a path, how a file is written so a concurrent reader never sees
// half of it, how a tree or a file is folded into a key, and when an entry that
// nobody has asked for in days goes away.
//
// It exists because there was one copy of each of those in cmd/goc/packcache.go
// and the per-function cache needed the same four. Two copies of an atomic write
// is two chances to get the rename wrong; two eviction policies is one cache that
// grows without bound while the other does not.
//
// The policy here is the Go toolchain's, and deliberately: mtime-on-use at hourly
// granularity so a warm build does not write a timestamp per hit, a trim no more
// than once a day, and a cutoff of five days since last use. cmd/go arrived at
// those constants over a decade of people's disks and there is nothing about
// cg12 that argues for different ones.
package cachefile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Disabled reports the one switch that turns every cg12 cache off.
//
// It is checked here rather than in each cache so that a new cache cannot be
// added that forgets it. The merge gates depend on CG12_NOCACHE=1 producing a
// build that reads nothing and writes nothing.
func Disabled() bool { return os.Getenv("CG12_NOCACHE") != "" }

// Directory resolves a cache directory: the named environment override if it is
// set, otherwise a subdirectory of the user cache directory. An empty result
// means "do not cache", which is what CG12_NOCACHE produces and what a box with
// no user cache directory produces.
func Directory(override string, elements ...string) string {
	if Disabled() {
		return ""
	}
	if directory := os.Getenv(override); directory != "" {
		return directory
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{base, "cg12"}, elements...)...)
}

// Path is where the entry for key lives: one fanout level of two hex characters,
// then the key itself.
//
// The fanout is gc's shape and is there for the same reason. A module is 50-250
// packages and a corpus run compiles hundreds of programs against several
// configurations, so a flat directory reaches tens of thousands of entries; ext4
// copes, but `ls` does not, and neither does a directory scan during eviction.
// 256 subdirectories keeps every one of them small enough to read.
func Path(directory, key, extension string) string {
	if directory == "" || len(key) < 2 {
		return ""
	}
	return filepath.Join(directory, key[:2], key+extension)
}

// Read returns the contents of the entry for key, and whether there was one.
// A cache directory that cannot be read is not an error; the caller does the
// work instead.
func Read(directory, key, extension string) ([]byte, bool) {
	path := Path(directory, key, extension)
	if path == "" {
		return nil, false
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	MarkUsed(path)
	return contents, true
}

// Write stores contents under key. A failure to store is not a build failure --
// the caller has the bytes already -- so callers generally report it rather than
// returning it.
func Write(directory, key, extension string, contents []byte) error {
	path := Path(directory, key, extension)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return WriteFileAtomically(path, contents)
}

// WriteFileAtomically writes through a temporary file in the same directory and
// renames it, so a reader never sees a half-written entry and two builds racing
// on one key both end with a whole file.
func WriteFileAtomically(path string, contents []byte) error {
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

// HashFileInto folds one file's base name and contents into digest.
func HashFileInto(digest io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	fmt.Fprintf(digest, "file=%s;\n", filepath.Base(path))
	_, err = io.Copy(digest, file)
	return err
}

// HashTreeInto folds every regular file under root into digest, by relative path
// and content.
//
// Content rather than modification time: a checkout, a rebase or a worktree copy
// all change mtimes without changing what the compiler reads, and the whole point
// of a cache is that those cases hit.
//
// This is the right key for a cache whose unit IS the whole tree -- the runtime
// pack. It is the wrong key for a per-package cache, and cmd/goc/packcache.go's
// use of it is not a precedent for one: hashing 73 MB of vendored standard
// library into every package's key makes any edit anywhere in stdlib/ a 100% miss
// rate across every package. The per-package key in goc/functioncache.go hashes
// each package's own files and folds in its imports' identities instead.
func HashTreeInto(digest io.Writer, root string) error {
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
		if err := HashFileInto(digest, path); err != nil {
			return err
		}
	}
	return nil
}

// Digest is the hex sha256 of a string, for callers building a key out of
// clauses rather than out of files.
func Digest(clauses string) string {
	sum := sha256.Sum256([]byte(clauses))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Eviction
// ---------------------------------------------------------------------------

const (
	// usedInterval is how stale an entry's mtime may get before a read refreshes
	// it. A warm build reads every entry it has; writing a timestamp for each
	// would turn a read-only cache hit into a write per package. An hour is fine
	// against a five-day cutoff.
	usedInterval = 1 * time.Hour
	// trimInterval is how often a process bothers to look for expired entries.
	trimInterval = 24 * time.Hour
	// trimCutoff is how long an entry survives without being read.
	trimCutoff = 5 * 24 * time.Hour
)

// MarkUsed refreshes an entry's modification time, at hourly granularity, so
// that Trim can tell a live entry from an abandoned one. Failures are ignored:
// a cache on a read-only filesystem should still serve hits.
func MarkUsed(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if time.Since(info.ModTime()) < usedInterval {
		return
	}
	now := time.Now()
	os.Chtimes(path, now, now)
}

// Trim removes entries nothing has read in trimCutoff, at most once per
// trimInterval per directory.
//
// The last-trim time is a file in the directory rather than process state,
// because the processes that use these caches are compilers: they start, compile
// one program and exit, so anything remembered in memory is remembered for the
// length of one build. cmd/go keeps the same stamp for the same reason.
//
// Errors are swallowed by design. A cache that cannot be trimmed is a cache that
// grows; a build that fails because a cache could not be trimmed is a broken
// build.
func Trim(directory string) {
	if directory == "" {
		return
	}
	stamp := filepath.Join(directory, "trim.txt")
	now := time.Now()
	if contents, err := os.ReadFile(stamp); err == nil {
		if seconds, err := strconv.ParseInt(strings.TrimSpace(string(contents)), 10, 64); err == nil {
			if now.Sub(time.Unix(seconds, 0)) < trimInterval {
				return
			}
		}
	}
	// The stamp is written first. Two builds starting together should not both
	// walk the tree, and a walk that fails halfway should not make every
	// subsequent build retry it.
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return
	}
	if err := WriteFileAtomically(stamp, []byte(strconv.FormatInt(now.Unix(), 10)+"\n")); err != nil {
		return
	}
	cutoff := now.Add(-trimCutoff)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 2 {
			continue
		}
		subdirectory := filepath.Join(directory, entry.Name())
		files, err := os.ReadDir(subdirectory)
		if err != nil {
			continue
		}
		for _, file := range files {
			info, err := file.Info()
			if err != nil || info.IsDir() {
				continue
			}
			if info.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(subdirectory, file.Name()))
			}
		}
	}
}
