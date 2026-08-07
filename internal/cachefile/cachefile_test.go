package cachefile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Eviction is the part of a cache nobody notices until the disk is full, and it
// is the part that changes meaning when a cache stops being opt-in: a directory
// somebody typed is their problem, and a directory the compiler chose is not.
//
// The two bounds are tested separately because they answer different questions --
// the age cutoff is "nobody wants this any more" and the budget is "this disk is
// not yours" -- and then together, because a policy is only one policy if the two
// agree about what to keep.

// write puts one entry of a given size in the directory, aged by however long
// ago it was last used.
func write(t *testing.T, directory, key string, size int, age time.Duration) string {
	t.Helper()
	require.NoError(t, Write(directory, key, ".unit", make([]byte, size)))
	path := Path(directory, key, ".unit")
	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, when, when))
	return path
}

// present is the keys still in the directory, sorted.
func present(t *testing.T, directory string) []string {
	t.Helper()
	var keys []string
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(directory, entry.Name()))
		require.NoError(t, err)
		for _, file := range files {
			keys = append(keys, file.Name())
		}
	}
	sort.Strings(keys)
	return keys
}

// writeFlat puts one entry directly in the cache directory rather than in its
// fanout subdirectory, which is the layout the pack cache had before the fanout
// landed and which 33.7 GB of one real cache is still in.
func writeFlat(t *testing.T, directory, key string, size int, age time.Duration) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(directory, 0o755))
	path := filepath.Join(directory, key+".unit")
	require.NoError(t, os.WriteFile(path, make([]byte, size), 0o644))
	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, when, when))
	return path
}

// TestTrimRemovesWhatNobodyHasRead is the age bound.
func TestTrimRemovesWhatNobodyHasRead(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	write(t, directory, "aa"+"fresh", 16, time.Hour)
	write(t, directory, "bb"+"yesterday", 16, 26*time.Hour)
	write(t, directory, "cc"+"stale", 16, trimCutoff+time.Hour)
	write(t, directory, "dd"+"ancient", 16, 30*24*time.Hour)

	Trim(directory, 0)
	require.Equal(t, []string{"aafresh.unit", "bbyesterday.unit"}, present(t, directory))
}

// TestTrimIsNotRateLimited is the defect this file exists to hold shut. Trim used
// to refuse to walk more than once per 24 hours per directory, which meant a
// budget that can be exceeded in an hour was checked once a day: the gate that
// found it took a function cache to 1.41 GB against a 1 GiB bound in 45 minutes
// without the trim stamp ever moving.
//
// A size bound has to be a claim about the disk now, so the second call in a
// second does the same work as the first.
func TestTrimIsNotRateLimited(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	write(t, directory, "aa"+"stale", 16, trimCutoff+time.Hour)
	Trim(directory, 0)
	require.Empty(t, present(t, directory))

	write(t, directory, "bb"+"stale", 16, trimCutoff+time.Hour)
	Trim(directory, 0)
	require.Empty(t, present(t, directory),
		"a second Trim in the same second declined to walk")

	// And the same for the budget, which is the bound that actually needs it: two
	// rounds of writing inside one interval, each of which must be brought back
	// under the budget rather than the second being waved through.
	for round := 0; round < 2; round++ {
		for index := 0; index < 8; index++ {
			write(t, directory, fmt.Sprintf("%02d", index)+"unit", 1024, time.Duration(index)*time.Hour)
		}
		Trim(directory, 3*1024)
		require.Len(t, present(t, directory), 3, "round %d was not brought under budget", round)
	}
}

// TestTrimLeavesNoStamp is the second half of the same defect. The stamp was
// written before the walk, so a trim that crashed, was killed, or ran against a
// directory it could not change claimed the next 24 hours anyway -- one failure
// became a day-long outage of the only thing holding the bound. There is no stamp
// now, so there is nothing to leave behind and nothing to lock out.
func TestTrimLeavesNoStamp(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	write(t, directory, "aa"+"fresh", 16, time.Minute)
	Trim(directory, 1<<30)

	_, err := os.Stat(filepath.Join(directory, "trim.txt"))
	require.True(t, os.IsNotExist(err), "Trim left a stamp that can lock out the next trim")
}

// TestTrimReachesTheFlatLayout is the 33.7 GB. Entries have lived at
// directory/xx/key since the fanout landed, but the pack cache predates it and
// the walk descended only into two-character directories -- so every entry
// written before the fanout was invisible to both bounds forever.
func TestTrimReachesTheFlatLayout(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	stale := "aabbccddeeff00112233445566778899"
	fresh := "99887766554433221100ffeeddccbbaa"
	writeFlat(t, directory, stale, 16, trimCutoff+time.Hour)
	writeFlat(t, directory, fresh, 16, time.Hour)

	Trim(directory, 0)
	require.NoFileExists(t, filepath.Join(directory, stale+".unit"),
		"a pre-fanout entry is unreachable by the age cutoff")
	require.FileExists(t, filepath.Join(directory, fresh+".unit"))

	// And by the budget, alongside a fanout entry: one policy over both layouts.
	writeFlat(t, directory, fresh, 1024, 2*time.Hour)
	write(t, directory, "aa"+"newer", 1024, time.Hour)
	Trim(directory, 1024)
	require.NoFileExists(t, filepath.Join(directory, fresh+".unit"),
		"the budget skipped a pre-fanout entry")
	require.Equal(t, []string{"aanewer.unit"}, present(t, directory))
}

// TestTrimLeavesFilesThatWereNeverEntries is the price of reading the top of the
// directory: a cache directory somebody pointed at a directory of their own must
// not lose things that were never ours. A key is hex, so a name that is not is
// not an entry.
func TestTrimLeavesFilesThatWereNeverEntries(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	keep := []string{"README", "notes.txt", "trim.txt", "cafe.lock", "zzzzzzzzzzzzzzzzzzzz.unit"}
	for _, name := range keep {
		path := filepath.Join(directory, name)
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
		when := time.Now().Add(-30 * 24 * time.Hour)
		require.NoError(t, os.Chtimes(path, when, when))
	}
	Trim(directory, 1)
	for _, name := range keep {
		require.FileExists(t, filepath.Join(directory, name),
			"%s is not named like a key and was deleted anyway", name)
	}
}

// TestConcurrentTrimsEvictOnce is what replaced the rate limit's second job.
// Nothing stops two builds finishing together from both walking now, so both must
// be able to: eviction order is fully determined, and a Remove that lost the race
// still counts against the total, so the loser stops where the winner did instead
// of evicting a second budget's worth.
func TestConcurrentTrimsEvictOnce(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	for index := 0; index < 20; index++ {
		write(t, directory, fmt.Sprintf("%02d", index)+"unit", 1024, time.Duration(index)*time.Hour)
	}

	var waiting sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			Trim(directory, 10*1024)
		}()
	}
	waiting.Wait()

	require.Len(t, present(t, directory), 10,
		"concurrent trims did not converge on the budget")
}

// TestTrimKeepsTheDirectoryUnderBudget is the size bound: everything is inside
// the age cutoff, and the directory is still too big.
func TestTrimKeepsTheDirectoryUnderBudget(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	// Ten entries of 1 kB, each an hour older than the last, none of them stale.
	for index := 0; index < 10; index++ {
		write(t, directory, fmt.Sprintf("%02d", index)+"unit", 1024, time.Duration(index)*time.Hour)
	}
	require.Len(t, present(t, directory), 10)

	Trim(directory, 4*1024)
	// The four youngest survive: index 0 is an hour old, index 9 is ten hours old.
	require.Equal(t, []string{"00unit.unit", "01unit.unit", "02unit.unit", "03unit.unit"},
		present(t, directory), "the budget did not evict least-recently-used first")
}

// TestTrimLeavesTheDirectoryAloneUnderBudget is the case that has to be free: a
// cache well inside its budget loses nothing.
func TestTrimLeavesTheDirectoryAloneUnderBudget(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	for index := 0; index < 6; index++ {
		write(t, directory, fmt.Sprintf("%02d", index)+"unit", 1024, time.Duration(index)*time.Hour)
	}
	Trim(directory, 1<<30)
	require.Len(t, present(t, directory), 6)
}

// TestAHitKeepsAUnitAlive is the property that makes the two bounds safe
// together: a unit a build is still using never ages out, because reading it
// refreshes its timestamp.
//
// It is the difference between a cache and a rolling deletion. Without it the
// runtime unit -- the biggest and the most reused thing in the directory -- is
// evicted five days after it was WRITTEN, however many builds have read it since.
func TestAHitKeepsAUnitAlive(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	// Two units, both older than the cutoff. One of them gets read.
	write(t, directory, "aa"+"used", 1024, trimCutoff+time.Hour)
	write(t, directory, "bb"+"forgotten", 1024, trimCutoff+time.Hour)

	contents, found := Read(directory, "aa"+"used", ".unit")
	require.True(t, found)
	require.Len(t, contents, 1024)

	Trim(directory, 0)
	require.Equal(t, []string{"aaused.unit"}, present(t, directory),
		"a unit that was read was evicted, or one that was not was kept")

	// And the same against the budget, which orders by the same timestamp.
	write(t, directory, "cc"+"newer", 1024, 0)
	_, found = Read(directory, "aa"+"used", ".unit")
	require.True(t, found)
	Trim(directory, 2*1024)
	require.Equal(t, []string{"aaused.unit", "ccnewer.unit"}, present(t, directory))
}

// TestMarkUsedIsHourly holds the reason a warm build does not turn a read-only
// cache hit into a write per package.
func TestMarkUsedIsHourly(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	path := write(t, directory, "aa"+"recent", 16, 10*time.Minute)
	before, err := os.Stat(path)
	require.NoError(t, err)
	MarkUsed(path)
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, before.ModTime().Equal(after.ModTime()),
		"a read inside the interval rewrote the timestamp anyway")

	path = write(t, directory, "bb"+"older", 16, 3*time.Hour)
	before, err = os.Stat(path)
	require.NoError(t, err)
	MarkUsed(path)
	after, err = os.Stat(path)
	require.NoError(t, err)
	require.True(t, after.ModTime().After(before.ModTime()),
		"a read outside the interval did not refresh the timestamp")
}

// TestTrimSurvivesADirectoryItCannotChange is the promise every cache in this
// tree makes: a cache that cannot be maintained is a cache that grows, not a
// build that fails.
func TestTrimSurvivesADirectoryItCannotChange(t *testing.T) {
	t.Parallel()

	Trim("", 1<<30)
	Trim(filepath.Join(t.TempDir(), "does", "not", "exist"), 1<<30)

	file := filepath.Join(t.TempDir(), "notadirectory")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	Trim(file, 1<<30)

	// A cache nobody may write to. The fanout directory is the one that has to be
	// read-only, because that is where an entry's link lives and what os.Remove
	// needs permission on; the top of the cache was enough to stop the old Trim
	// only because it wrote its stamp there before walking, and the stamp is gone.
	readonly := t.TempDir()
	write(t, readonly, "aa"+"stale", 16, trimCutoff+time.Hour)
	require.NoError(t, os.Chmod(filepath.Join(readonly, "aa"), 0o555))
	require.NoError(t, os.Chmod(readonly, 0o555))
	t.Cleanup(func() {
		os.Chmod(readonly, 0o755)
		os.Chmod(filepath.Join(readonly, "aa"), 0o755)
	})
	Trim(readonly, 1024)
	require.Equal(t, []string{"aastale.unit"}, present(t, readonly),
		"a read-only cache should keep serving what it has")
}

// BenchmarkTrimWalk is the number the whole trigger rests on. Trim runs at the
// end of every build that used a cache now, instead of once a day, and that is
// only defensible if walking a full cache is noise against a compile.
//
// The shape is a function cache at its budget: a gibibyte in units of the
// corpus's 355 kB mean, which is about 3000 entries over the 256 fanout
// directories. The files are empty -- Trim stats, it never reads -- so this
// measures the readdir and stat traffic and nothing else, which is what a real
// walk of a directory whose metadata is in cache costs.
func BenchmarkTrimWalk(b *testing.B) {
	directory := b.TempDir()
	const entries = 3000
	for index := 0; index < entries; index++ {
		key := fmt.Sprintf("%064x", index)
		path := Path(directory, key, ".unit")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		// A budget nothing can exceed: the walk and the accounting, no eviction.
		Trim(directory, 1<<40)
	}
}

// TestDisabledBeatsEverything is the switch the merge gates depend on.
func TestDisabledBeatsEverything(t *testing.T) {
	t.Setenv("CG12_NOCACHE", "1")
	t.Setenv("CG12_TEST_CACHE", t.TempDir())
	require.True(t, Disabled())
	require.Equal(t, "", Directory("CG12_TEST_CACHE", "whatever"))
	require.Equal(t, "", Directory("", "whatever"))
}
