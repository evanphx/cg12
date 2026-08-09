package memo

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// A corrupt memo file must be a cache miss, not a crash.
//
// This is not a hypothetical. goc compiles a package set in parallel and every
// one of those processes shares one memo path; Save truncated the destination
// and filled it in place, so a reader could see half a file. Load then sized an
// allocation from whatever number the truncation happened to end on, and
// "makeslice: len out of range" took out 64 subtests of the capability matrix at
// once. A cache that cannot be read is a cache that should be rebuilt.
//
// Every prefix is tested rather than a few chosen ones, because the interesting
// lengths are the ones that stop midway through a length field -- and which
// offsets those are is a property of the encoding, not something worth
// hard-coding here.
func TestLoadRejectsTruncatedFileWithoutPanicking(t *testing.T) {
	store := NewStore()
	store.Put(&Entry{
		Name: "example", Body: []byte("a body long enough to be cut in several places"),
		Deps:    []Dep{{Name: "dep-one"}, {Name: "dep-two"}},
		Spliced: []string{"spliced-one", "spliced-two"},
	})
	dir := t.TempDir()
	full := filepath.Join(dir, "full.memo")
	require.NoError(t, store.Save(full))
	whole, err := os.ReadFile(full)
	require.NoError(t, err)
	require.NotEmpty(t, whole)

	for n := 0; n < len(whole); n++ {
		path := filepath.Join(dir, "cut.memo")
		require.NoError(t, os.WriteFile(path, whole[:n], 0o644))
		require.NotPanics(t, func() {
			loaded, err := Load(path)
			// Either outcome is fine -- a short prefix is not a memo file, a longer
			// one is a truncated one -- but it must not be a panic, and it must not
			// be a store claiming entries it does not have.
			if err == nil && loaded != nil {
				for name, e := range loaded.Entries {
					require.NotNil(t, e, "entry %q", name)
					require.Equal(t, name, e.Name, "entries are keyed by their own name")
				}
			}
		}, "truncated at %d bytes", n)
	}
}

// A file whose length fields are corrupt rather than merely short. A truncation
// only ever produces lengths that were once honest; flipping bytes produces the
// enormous ones that reach the allocator.
func TestLoadRejectsCorruptLengthsWithoutPanicking(t *testing.T) {
	store := NewStore()
	store.Put(&Entry{Name: "example", Body: []byte("body"), Spliced: []string{"s"}})
	dir := t.TempDir()
	full := filepath.Join(dir, "full.memo")
	require.NoError(t, store.Save(full))
	whole, err := os.ReadFile(full)
	require.NoError(t, err)

	for i := len(memoMagic); i < len(whole); i++ {
		for _, b := range []byte{0xff, 0x80, 0x7f} {
			corrupt := append([]byte(nil), whole...)
			corrupt[i] = b
			path := filepath.Join(dir, "bad.memo")
			require.NoError(t, os.WriteFile(path, corrupt, 0o644))
			require.NotPanics(t, func() { _, _ = Load(path) },
				"byte %d set to %#x", i, b)
		}
	}
}

// A length field that is enormous rather than merely wrong. This is the shape
// that actually took the build out: uvarint decodes a value far past any real
// file, and the old reader detected the overrun, set its error, and then sized an
// allocation from it anyway on the way out.
//
// It is built rather than fuzzed toward. Truncating a file only ever produces
// lengths that were once honest, and flipping single bytes rarely reaches the
// allocator's ceiling -- the first version of this test did both and passed
// against the bug it was written for.
func TestLoadRejectsEnormousLengthWithoutPanicking(t *testing.T) {
	huge := make([]byte, 0, 16)
	huge = binary.AppendUvarint(huge, 1<<62) // a length no file can honour

	var file []byte
	file = append(file, memoMagic...)
	file = binary.AppendUvarint(file, memoVersion)
	file = append(file, make([]byte, 32)...) // Store.Data
	file = binary.AppendUvarint(file, 1)     // one entry
	file = append(file, huge...)             // ...whose name is that long

	dir := t.TempDir()
	path := filepath.Join(dir, "huge.memo")
	require.NoError(t, os.WriteFile(path, file, 0o644))

	require.NotPanics(t, func() {
		_, err := Load(path)
		require.Error(t, err, "a length past the end of the file is not loadable")
	})
}

// And the same length in the entry-count position, which sizes a different
// allocation on a different line.
func TestLoadRejectsEnormousEntryCountWithoutPanicking(t *testing.T) {
	var file []byte
	file = append(file, memoMagic...)
	file = binary.AppendUvarint(file, memoVersion)
	file = append(file, make([]byte, 32)...)
	file = binary.AppendUvarint(file, 1<<62) // that many entries follow

	dir := t.TempDir()
	path := filepath.Join(dir, "count.memo")
	require.NoError(t, os.WriteFile(path, file, 0o644))

	require.NotPanics(t, func() {
		_, err := Load(path)
		require.Error(t, err)
	})
}

// Save must be atomic, because the reader may be another process mid-compile.
// Readers running against a directory being written see either the previous
// store or the new one, never a partial file.
func TestSaveIsAtomicUnderConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.memo")

	first := NewStore()
	first.Put(&Entry{Name: "first", Body: []byte("one")})
	require.NoError(t, first.Save(path))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers, hammering the path while it is rewritten underneath them.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				require.NotPanics(t, func() {
					if loaded, err := Load(path); err == nil {
						require.NotNil(t, loaded)
					}
				})
			}
		}()
	}
	for i := 0; i < 40; i++ {
		s := NewStore()
		s.Put(&Entry{Name: "later", Body: make([]byte, 4096)})
		require.NoError(t, s.Save(path))
	}
	close(stop)
	wg.Wait()

	// And the file that survives is loadable.
	loaded, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)
}

// Save must not leave its temporary behind: the cache directory is swept by size
// elsewhere, and a stray file per compile would defeat that.
func TestSaveLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.memo")
	s := NewStore()
	s.Put(&Entry{Name: "n", Body: []byte("b")})
	require.NoError(t, s.Save(path))
	require.NoError(t, s.Save(path)) // and again, over an existing file

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the memo itself should remain")
	require.Equal(t, "x.memo", entries[0].Name())
}
