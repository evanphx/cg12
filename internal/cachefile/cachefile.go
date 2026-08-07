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
// The policy here is the Go toolchain's where the Go toolchain's question is the
// same one: mtime-on-use at hourly granularity so a warm build does not write a
// timestamp per hit, and a cutoff of five days since last use. cmd/go arrived at
// those over a decade of people's disks.
//
// Its once-a-day rate limit is NOT kept, and the reason is the one thing cg12
// asks of a cache that cmd/go does not: a size bound. See [Trim].
package cachefile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// Trim removes entries nothing has read in trimCutoff, and then, if budget is
// positive, the least recently used of what is left until the directory fits in
// budget bytes.
//
// Two bounds rather than one, because they answer different questions. The age
// cutoff is "nobody wants this any more" and is what keeps a developer's cache
// from carrying last month's branches. The size budget is "this disk is not
// yours" and is what a cache on by default needs: the compiler binary's hash is a
// clause of both caches' keys, so a box that rebuilds the compiler -- a gate, a
// bisection, anyone working on cg12 itself -- mints a whole new generation of
// entries every time, and five days of that is unbounded in exactly the way the
// age cutoff cannot see.
//
// # When this runs, and why there is no rate limit any more
//
// There was one: a trim.txt stamp in the directory and a refusal to walk more
// than once per 24 hours, copied from cmd/go. It is gone, because it was
// answering the wrong question. A daily interval is right for an age cutoff --
// nothing can cross a five-day line between 10:00 and 10:05 -- and useless for a
// size cap, which is a claim about the disk right now. The measured failure was
// exactly that: 24 compiler generations in 45 minutes took a function cache to
// 1.41 GB against a 1 GiB bound, at ~55 MB a generation, with the stamp never
// moving. Anything that can be exceeded in an hour cannot be enforced on a day.
//
// The trigger is instead the only event that can make a cache directory grow: a
// build finishing a write to it. Callers call Trim at the end of a build, after
// their writes, so the bound holds at the moment the build ends rather than at
// the moment it started. Between those two points a build may exceed the budget
// by its own output, which is ~60 MB for the largest program in the corpus.
//
// That is affordable because the check is one readdir per fanout directory and
// one stat per entry, and nothing else -- no read of an entry's contents. At the
// budget, with the corpus's 355 kB mean unit, a function cache holds about 3000
// entries in 256 directories, and BenchmarkTrimWalk measures that walk at 14.5 ms
// -- 0.05% of the 30-second compile the gate ran it inside, and about a
// four-thousandth of what the cache saves that compile. A cache big enough for
// the walk to matter is a cache the walk is about to make smaller.
//
// The rate limit was also doing a second job -- keeping two builds that start
// together from both walking -- and that job is done instead by making a
// concurrent trim harmless. Eviction order is fully determined (oldest first,
// path within a timestamp), so two processes evicting at once choose the same
// files in the same order; and a Remove that fails because someone else already
// did it still counts against the total, so the second process stops at the same
// place rather than evicting the budget twice over.
//
// # What the walk looks at
//
// Both layouts. Entries live at directory/xx/key+extension, and that is the only
// place Write has ever put them since the fanout landed -- but the pack cache
// predates the fanout, and 33.7 GB of it on the box this was found on is sitting
// flat in the top of the cache directory where a walk that descends only into
// two-character directories can never see it. Not merely un-budgeted: unreachable
// by the age cutoff too, so nothing has ever been able to delete one.
//
// A top-level file is treated as an entry only if it is named like one -- a long
// run of hex before the first dot, which is what a key is -- so that a cache
// directory somebody pointed at a directory of their own does not lose files that
// were never ours.
//
// Least recently used is by modification time, which MarkUsed refreshes on every
// read. A unit a build is using is therefore younger than the cutoff and near the
// top of the budget's ordering, so the working set is the last thing to go.
//
// Errors are swallowed by design. A cache that cannot be trimmed is a cache that
// grows; a build that fails because a cache could not be trimmed is a broken
// build.
func Trim(directory string, budget int64) {
	if directory == "" {
		return
	}
	cutoff := time.Now().Add(-trimCutoff)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	type survivor struct {
		path string
		used time.Time
		size int64
	}
	var survivors []survivor
	var total int64
	consider := func(path string, info os.FileInfo) {
		if info.ModTime().Before(cutoff) {
			os.Remove(path)
			return
		}
		if budget <= 0 {
			return
		}
		survivors = append(survivors, survivor{path: path, used: info.ModTime(), size: info.Size()})
		total += info.Size()
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			// The flat layout that predates the fanout. Only what is named like a key.
			if !looksLikeKey(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.IsDir() {
				continue
			}
			consider(filepath.Join(directory, entry.Name()), info)
			continue
		}
		if len(entry.Name()) != 2 {
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
			consider(filepath.Join(subdirectory, file.Name()), info)
		}
	}
	if budget <= 0 || total <= budget {
		return
	}
	// Oldest first, and by path within one timestamp: MarkUsed writes at hourly
	// granularity, so ties are the rule rather than the exception and an arbitrary
	// order would make two boxes with the same cache evict differently -- and would
	// make two processes trimming one cache at the same moment evict twice.
	sort.Slice(survivors, func(i, j int) bool {
		if !survivors[i].used.Equal(survivors[j].used) {
			return survivors[i].used.Before(survivors[j].used)
		}
		return survivors[i].path < survivors[j].path
	})
	for _, entry := range survivors {
		if total <= budget {
			return
		}
		// A file another process removed first is a file that is gone, and counting
		// it is what keeps two concurrent trims from evicting the budget twice.
		if err := os.Remove(entry.path); err == nil || os.IsNotExist(err) {
			total -= entry.size
		}
	}
}

// looksLikeKey reports whether a file directly in a cache directory is one of
// ours: a key is hex, so a name that begins with a long run of hex before its
// first dot is an entry and anything else -- a README, a lock file, a directory
// somebody keeps their own things in -- is not.
//
// Sixteen characters rather than the full sixty-four a key actually has, because
// the point is to exclude names that were never keys, not to validate a digest,
// and a shorter key is a change this should survive.
func looksLikeKey(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	if len(stem) < 16 {
		return false
	}
	for index := 0; index < len(stem); index++ {
		switch character := stem[index]; {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}
