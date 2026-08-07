package cachefile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// expire moves the trim stamp back far enough that the next Trim runs. Trim
// rate-limits itself to once a day per directory, which is the behaviour a build
// wants and the one a test has to defeat.
func expire(t *testing.T, directory string) {
	t.Helper()
	stamp := filepath.Join(directory, "trim.txt")
	if _, err := os.Stat(stamp); err != nil {
		return
	}
	when := time.Now().Add(-2 * trimInterval)
	require.NoError(t, os.Chtimes(stamp, when, when))
	require.NoError(t, os.WriteFile(stamp,
		fmt.Appendf(nil, "%d\n", when.Unix()), 0o644))
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

// TestTrimIsRateLimited holds the property that makes it affordable to call on
// every compile: the second call in a day does nothing at all.
func TestTrimIsRateLimited(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()

	write(t, directory, "aa"+"stale", 16, trimCutoff+time.Hour)
	Trim(directory, 0)
	require.Empty(t, present(t, directory))

	write(t, directory, "bb"+"stale", 16, trimCutoff+time.Hour)
	Trim(directory, 0)
	require.Equal(t, []string{"bbstale.unit"}, present(t, directory),
		"a second Trim within the interval walked the tree anyway")

	expire(t, directory)
	Trim(directory, 0)
	require.Empty(t, present(t, directory))
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
	expire(t, directory)
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

	readonly := t.TempDir()
	write(t, readonly, "aa"+"stale", 16, trimCutoff+time.Hour)
	require.NoError(t, os.Chmod(readonly, 0o555))
	t.Cleanup(func() { os.Chmod(readonly, 0o755) })
	Trim(readonly, 1024)
	require.Equal(t, []string{"aastale.unit"}, present(t, readonly),
		"a read-only cache should keep serving what it has")
}

// TestDisabledBeatsEverything is the switch the merge gates depend on.
func TestDisabledBeatsEverything(t *testing.T) {
	t.Setenv("CG12_NOCACHE", "1")
	t.Setenv("CG12_TEST_CACHE", t.TempDir())
	require.True(t, Disabled())
	require.Equal(t, "", Directory("CG12_TEST_CACHE", "whatever"))
	require.Equal(t, "", Directory("", "whatever"))
}
