package goc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file holds the two things that change when a cache stops being opt-in:
// who gets one by default, and what happens when the disk says no.
//
// The second is the more important. An opt-in cache that breaks a build breaks
// the build of somebody who asked for a cache; a default-on cache that breaks a
// build breaks the build of somebody who has never heard of it. So every way the
// store can fail has to end in a cold compile with the right answer, and the ways
// are enumerated here rather than argued about.

// withDefaultOn turns the compiler binary's default on for one test and puts it
// back, because it is a process global and the rest of the suite depends on the
// library default being off.
func withDefaultOn(t *testing.T) {
	t.Helper()
	previous := functionCacheDefaultOn
	t.Cleanup(func() { functionCacheDefaultOn = previous })
	UseFunctionCacheByDefault()
}

// TestFunctionCacheDefaultIsPerCaller is the design decision, as a test: the
// library compiles without a cache and the compiler binary compiles with one.
func TestFunctionCacheDefaultIsPerCaller(t *testing.T) {
	t.Setenv("CG12_NOCACHE", "")
	t.Setenv("CG12_FUNC_CACHE", "")

	require.False(t, FunctionCacheEnabled(),
		"goc.Compile called in process picked up a cache nobody asked for")

	withDefaultOn(t)
	require.True(t, FunctionCacheEnabled(), "cmd/goc's default did not take effect")

	base, err := os.UserCacheDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(base, "cg12", "function-cache"), functionCacheDirectory(),
		"the default location is not the shape packCacheDirectory has")
}

// TestFunctionCacheSwitches holds every way of saying no, including the one the
// merge gates depend on.
func TestFunctionCacheSwitches(t *testing.T) {
	withDefaultOn(t)
	directory := t.TempDir()

	for _, each := range []struct {
		name       string
		nocache    string
		funcCache  string
		wantOn     bool
		wantWhere  string
		wantReason string
	}{
		{name: "default", wantOn: true},
		{name: "nocache beats the default", nocache: "1", wantOn: false},
		{name: "nocache beats an explicit directory", nocache: "1", funcCache: directory, wantOn: false},
		{name: "nocache beats auto", nocache: "1", funcCache: "auto", wantOn: false},
		{name: "off", funcCache: "off", wantOn: false},
		{name: "auto", funcCache: "auto", wantOn: true},
		{name: "a directory", funcCache: directory, wantOn: true, wantWhere: directory},
	} {
		t.Run(each.name, func(t *testing.T) {
			t.Setenv("CG12_NOCACHE", each.nocache)
			t.Setenv("CG12_FUNC_CACHE", each.funcCache)
			require.Equal(t, each.wantOn, FunctionCacheEnabled())
			if each.wantWhere != "" {
				require.Equal(t, each.wantWhere, functionCacheDirectory())
			}
		})
	}
}

// TestABrokenCacheStillCompiles is the promise a default-on cache has to keep.
//
// Each arm is a way the store can fail that a user could actually meet -- a cache
// directory they cannot write, a path where something else already is, a file cut
// short by a full disk or a killed build, a file whose bytes rotted -- and each
// one has to produce the same module a compile with no cache at all produces,
// with no error and nothing on stderr.
func TestABrokenCacheStillCompiles(t *testing.T) {
	t.Setenv("CG12_NOCACHE", "1")
	t.Setenv("CG12_FUNC_CACHE", "")
	control, err := CompileExecutableFor(TargetARM64, "broken.go", []byte(programCacheSmall))
	require.NoError(t, err)
	controlBytes := moduleBytes(t, control)

	// A directory filled by a good compile, to break in the arms below.
	filled := t.TempDir()
	compileWithCache(t, filled, "broken.go", programCacheSmall)
	require.NotEmpty(t, cachedUnitFiles(t, filled), "the filling compile stored nothing to break")

	for _, arm := range []struct {
		name    string
		prepare func(t *testing.T) string
	}{
		{
			// The commonest one on a shared machine: a cache directory owned by
			// somebody else.
			name: "a read-only directory",
			prepare: func(t *testing.T) string {
				directory := filepath.Join(t.TempDir(), "readonly")
				require.NoError(t, os.Mkdir(directory, 0o555))
				t.Cleanup(func() { os.Chmod(directory, 0o755) })
				return directory
			},
		},
		{
			name: "a read-only directory that already has units in it",
			prepare: func(t *testing.T) string {
				directory := copyTree(t, filled)
				require.NoError(t, os.Chmod(directory, 0o555))
				t.Cleanup(func() { os.Chmod(directory, 0o755) })
				return directory
			},
		},
		{
			// A file where the cache directory should be. MkdirAll fails, and so does
			// every read under it.
			name: "a file where the directory should be",
			prepare: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "notadirectory")
				require.NoError(t, os.WriteFile(path, []byte("this is not a cache"), 0o644))
				return path
			},
		},
		{
			// A file where a fanout directory should be: the directory exists and is
			// writable, and one of the 256 buckets under it cannot be created.
			name: "a file where a fanout directory should be",
			prepare: func(t *testing.T) string {
				directory := t.TempDir()
				for _, fanout := range []string{"00", "0a", "5f", "a3", "ff"} {
					require.NoError(t, os.WriteFile(filepath.Join(directory, fanout), []byte("x"), 0o644))
				}
				return directory
			},
		},
		{
			// What a full disk or a killed build leaves behind, if the atomic rename
			// were not there to prevent it.
			name: "truncated units",
			prepare: func(t *testing.T) string {
				directory := copyTree(t, filled)
				for _, path := range cachedUnitFiles(t, directory) {
					contents, err := os.ReadFile(path)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, contents[:len(contents)/2], 0o644))
				}
				return directory
			},
		},
		{
			name: "empty units",
			prepare: func(t *testing.T) string {
				directory := copyTree(t, filled)
				for _, path := range cachedUnitFiles(t, directory) {
					require.NoError(t, os.WriteFile(path, nil, 0o644))
				}
				return directory
			},
		},
		{
			// Bit rot, or a unit spliced from another cache: the header and the
			// version are right and the payload is not what was written.
			name: "corrupt units",
			prepare: func(t *testing.T) string {
				directory := copyTree(t, filled)
				for _, path := range cachedUnitFiles(t, directory) {
					contents, err := os.ReadFile(path)
					require.NoError(t, err)
					contents[len(contents)/2] ^= 0xff
					contents[len(contents)-1] ^= 0x01
					require.NoError(t, os.WriteFile(path, contents, 0o644))
				}
				return directory
			},
		},
		{
			// Not a cg12 unit at all, under a name that is one.
			name: "units that are somebody else's file",
			prepare: func(t *testing.T) string {
				directory := copyTree(t, filled)
				for _, path := range cachedUnitFiles(t, directory) {
					require.NoError(t, os.WriteFile(path, []byte("<html><body>404</body></html>\n"), 0o644))
				}
				return directory
			},
		},
		{
			// A unit that cannot be read back. The compile has to do the work itself
			// rather than treat an unreadable file as an empty one.
			name: "unreadable units",
			prepare: func(t *testing.T) string {
				directory := copyTree(t, filled)
				for _, path := range cachedUnitFiles(t, directory) {
					require.NoError(t, os.Chmod(path, 0o000))
				}
				return directory
			},
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			directory := arm.prepare(t)
			t.Setenv("CG12_NOCACHE", "")
			t.Setenv("CG12_FUNC_CACHE", directory)

			module, err := CompileExecutableFor(TargetARM64, "broken.go", []byte(programCacheSmall))
			require.NoError(t, err, "a broken cache failed the compile")
			require.Equal(t, string(controlBytes), string(moduleBytes(t, module)),
				"a broken cache produced a different module from an absent one")
			stats := LastFunctionCacheStats()
			t.Logf("%s: %d/%d packages hit, %d/%d declarations replayed, %d files written",
				arm.name, stats.PackagesHit, stats.Packages, stats.Hits, stats.Declarations, stats.Wrote)
		})
	}
}

// TestABrokenCacheStillCompilesWithTheDefaultLocation is the same promise for the
// path an ordinary `goc file.go` takes, where the directory is not named by
// anyone and a failure has nobody to report to.
func TestABrokenCacheStillCompilesWithTheDefaultLocation(t *testing.T) {
	t.Setenv("CG12_NOCACHE", "1")
	t.Setenv("CG12_FUNC_CACHE", "")
	control, err := CompileExecutableFor(TargetARM64, "broken.go", []byte(programCacheSmall))
	require.NoError(t, err)

	// XDG_CACHE_HOME is what os.UserCacheDir reads on this platform, so pointing it
	// at a file is how the default location becomes unusable without touching the
	// user's real cache.
	home := t.TempDir()
	blocked := filepath.Join(home, "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("not a cache directory"), 0o644))
	t.Setenv("XDG_CACHE_HOME", blocked)
	t.Setenv("CG12_NOCACHE", "")
	t.Setenv("CG12_FUNC_CACHE", "")
	withDefaultOn(t)
	require.True(t, FunctionCacheEnabled(), "this arm is not testing what it says")

	module, err := CompileExecutableFor(TargetARM64, "broken.go", []byte(programCacheSmall))
	require.NoError(t, err, "an unusable default cache location failed the compile")
	require.Equal(t, string(moduleBytes(t, control)), string(moduleBytes(t, module)))
	stats := LastFunctionCacheStats()
	t.Logf("unusable default location: %d/%d packages hit, %d files written",
		stats.PackagesHit, stats.Packages, stats.Wrote)
}

// copyTree is a filled cache directory, copied so an arm can break its copy.
func copyTree(t *testing.T, source string) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "cache")
	require.NoError(t, os.MkdirAll(destination, 0o755))
	entries, err := os.ReadDir(source)
	require.NoError(t, err)
	for _, entry := range entries {
		from := filepath.Join(source, entry.Name())
		to := filepath.Join(destination, entry.Name())
		if entry.IsDir() {
			require.NoError(t, os.MkdirAll(to, 0o755))
			children, err := os.ReadDir(from)
			require.NoError(t, err)
			for _, child := range children {
				contents, err := os.ReadFile(filepath.Join(from, child.Name()))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(to, child.Name()), contents, 0o644))
			}
			continue
		}
		contents, err := os.ReadFile(from)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(to, contents, 0o644))
	}
	return destination
}
