package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here are about one property: a compile that takes a pack out of the
// cache produces the same image as one that builds it. Everything else about
// auto-packing is a performance question, and a performance question that is
// wrong costs time. This one costs correctness -- a stale hit is a wrong image
// -- and it now decides every compile rather than an explicit command.
//
// So the key is checked against each thing it has to cover by *changing that
// thing and requiring a miss*, and the cold and warm compiles of one program are
// checked by comparing their bytes.

// TestTheKeyMovesWhenTheEnvironmentChangesWhatTheCompilerEmits checks the half of
// the key that is not the compiler's bytes and not the standard library's.
//
// These are the switches that change what goc emits without changing a byte of
// goc, and before this change the key named two of them explicitly and missed the
// rest. Two of the misses were real: GOC_ESCAPE_SUMMARIES=0 and GOC_PAYLOAD_FOLD=0
// change escape analysis, and GOC_STDLIB_OVERLAY changes which files a package is
// built from -- which the hashed tree cannot see, because the tree is the same and
// only the selection differs.
func TestTheKeyMovesWhenTheEnvironmentChangesWhatTheCompilerEmits(t *testing.T) {
	stdlib := goc.StdlibRoot()
	base, err := packCacheKey(runtimepack.Version, "arm64", false, nil, stdlib)
	require.NoError(t, err)

	for name, setting := range map[string][2]string{
		"the optimization pipeline":     {"GOC_OPT_PIPELINE", "bounded"},
		"a skipped optimizer pass":      {"GOC_OPT_SKIP", "inline"},
		"the function alignment":        {"GOC_FUNC_ALIGN", "64"},
		"the loop alignment":            {"GOC_LOOP_ALIGN", "32"},
		"the inter-function padding":    {"GOC_TEXT_PAD", "16"},
		"escape summaries":              {"GOC_ESCAPE_SUMMARIES", "0"},
		"payload folding":               {"GOC_PAYLOAD_FOLD", "0"},
		"if-conversion":                 {"CG12_NO_IFCONVERT", "1"},
		"the inliner's cost model":      {"CG12_NO_COSTINLINE", "1"},
		"aggregate-return inlining":     {"CG12_NO_AGGINLINE", "1"},
		"nosplit inlining":              {"GOC_NO_NOSPLIT_INLINE", "1"},
		"the nosplit frame budget":      {"GOC_NOSPLIT_LIMIT", "700"},
		"the standard library overlay":  {"GOC_STDLIB_OVERLAY", "off"},
		"a switch nobody has added yet": {"GOC_SOME_FUTURE_SWITCH", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(setting[0], setting[1])
			other, err := packCacheKey(runtimepack.Version, "arm64", false, nil, stdlib)
			require.NoError(t, err)
			assert.NotEqual(t, base, other, "the key has to move when %s does", name)
		})
	}

	// And the switches that name where the cache is must not move it, or two
	// cache directories would never share a pack and CG12_PACK_CACHE would be a
	// way of asking for a rebuild.
	for _, name := range []string{"CG12_PACK_CACHE", "CG12_PACK_CACHE_MAX_BYTES", "GOC_AUTOPACK_DEBUG"} {
		t.Run("not "+name, func(t *testing.T) {
			t.Setenv(name, "1")
			other, err := packCacheKey(runtimepack.Version, "arm64", false, nil, stdlib)
			require.NoError(t, err)
			assert.Equal(t, base, other, "%s says where the cache is, not what is in it", name)
		})
	}
}

// TestTheKeyMovesWhenTheCToolchainDoes is the weakest link in the key, named as
// such in packcache.go's header for as long as the cache has existed: a version
// banner identifies a toolchain release, not a particular assembler.
//
// It cannot be tested by swapping the host's assembler, so what is checked is
// that the identity is made of the things that would catch one -- the driver's
// bytes, the assembler's bytes, and the object the assembler actually produces --
// rather than of the banner alone.
func TestTheKeyMovesWhenTheCToolchainDoes(t *testing.T) {
	identity := cToolchainIdentity()
	if identity == "cc=absent" {
		t.Skip("no cc on this box")
	}
	assert.Contains(t, identity, "ccbytes=", "the driver's own bytes")
	assert.Contains(t, identity, "asbytes=", "the assembler the driver will exec")
	assert.Contains(t, identity, "probe=", "what the assembler produces for a fixed input")
	assert.NotContains(t, identity, "ccbytes=absent")
	assert.NotContains(t, identity, "probe=unassemblable")
	assert.NotContains(t, identity, "probe=unavailable")

	// The probe is only worth anything if it is stable: an unstable one would
	// make every compile a miss, which is the failure this cache exists to avoid.
	compiler, err := exec.LookPath("cc")
	require.NoError(t, err)
	assert.Equal(t, assemblerProbeDigest(compiler), assemblerProbeDigest(compiler),
		"the probe object has to be the same bytes every time")
}

// TestAPackIsSubstitutedOnlyWhenItCarriesTheSameClosure checks the rule that
// stops the cache filling with near-duplicates. See substitutePack for why the
// two containments are exactly the condition.
func TestAPackIsSubstitutedOnlyWhenItCarriesTheSameClosure(t *testing.T) {
	directory := t.TempDir()
	const prefix = "testprefix"

	// A cached pack carrying net/http, whose closure has swept up fmt and io.
	write := func(key string, packages, closure []string) {
		require.NoError(t, os.MkdirAll(packIndexDirectory(directory, prefix), 0o755))
		encoded, err := json.Marshal(packIndexEntry{Key: key, Packages: packages, Closure: closure})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(packIndexDirectory(directory, prefix), key+".json"), encoded, 0o644))
		require.NoError(t, os.WriteFile(cachedPackPath(directory, key), []byte("pack"), 0o644))
	}
	write("http", []string{"net/http"}, []string{"fmt", "io", "net/http", "runtime", "sync"})

	// A program importing fmt as well as net/http would build a second 98 MB
	// pack with the same closure. It takes this one instead.
	_, entry, ok := substitutePack(directory, prefix, []string{"fmt", "net/http"})
	require.True(t, ok)
	assert.Equal(t, "http", entry.Key)

	// A program importing net/http and something the pack's closure does not
	// have gets no substitute: its own pack would carry more.
	_, _, ok = substitutePack(directory, prefix, []string{"net/http", "net/smtp"})
	assert.False(t, ok, "a wider program must not take a narrower pack's word for its closure")

	// A program importing less than the pack carries gets no substitute either,
	// and this is the direction that would be a wrong image rather than a slow
	// build: the pack leaves its type region and its dispatchers for a program
	// that loaded all of net/http, and this one did not.
	_, _, ok = substitutePack(directory, prefix, []string{"fmt"})
	assert.False(t, ok, "a pack must never be offered to a program whose closure does not contain it")

	// An entry whose pack has been evicted is not a substitute.
	require.NoError(t, os.Remove(cachedPackPath(directory, "http")))
	_, _, ok = substitutePack(directory, prefix, []string{"fmt", "net/http"})
	assert.False(t, ok)
}

// TestTheCacheEvictsTheLeastRecentlyUsedPack. Nothing evicted before this; by the
// time it was measured the build host's shared cache held 985 packs and 45 GB,
// from opt-in `build-runtime` calls alone.
func TestTheCacheEvictsTheLeastRecentlyUsedPack(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CG12_PACK_CACHE_MAX_BYTES", "2048")

	for _, key := range []string{"oldest", "middle", "newest"} {
		require.NoError(t, os.WriteFile(cachedPackPath(directory, key), make([]byte, 1024), 0o644))
	}
	// Use the oldest, which is what keeps a pack every build links against from
	// being evicted however long ago it was built.
	_, ok := cachedPack(directory, "oldest")
	require.True(t, ok)

	trimPackCache(directory)

	assert.FileExists(t, cachedPackPath(directory, "oldest"), "a pack in use keeps its place")
	assert.FileExists(t, cachedPackPath(directory, "newest"))
	assert.NoFileExists(t, cachedPackPath(directory, "middle"), "the least recently used one goes")

	// And an unbounded cache evicts nothing.
	t.Setenv("CG12_PACK_CACHE_MAX_BYTES", "0")
	require.NoError(t, os.WriteFile(cachedPackPath(directory, "middle"), make([]byte, 1024), 0o644))
	trimPackCache(directory)
	assert.FileExists(t, cachedPackPath(directory, "middle"))
}

func TestOnlyStandardLibraryImportsReachThePackRoot(t *testing.T) {
	packages, err := stdlibImportsOf([]byte(`package main

import (
	"fmt"
	_ "net/http"
	"unsafe"

	"example.com/not/vendored"
)

func main() {}
`))
	require.NoError(t, err)
	assert.Equal(t, []string{"fmt", "net/http"}, packages,
		"unsafe is not a package a pack root can import, and a module path is not in the vendored tree")

	// Only the imports are parsed, so a body that is not Go is not this code's
	// business and does not stop it choosing a pack -- the compile that follows
	// reports the error, as it always did.
	broken, err := stdlibImportsOf([]byte("package main\n\nimport \"fmt\"\n\nfunc main() { this is not Go }\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{"fmt"}, broken)

	// A file whose imports themselves do not parse is reported, and the caller
	// leaves the real error to the compiler.
	_, err = stdlibImportsOf([]byte("package main\n\nimport (\n\t\"fmt\"\n"))
	assert.Error(t, err)
}

func TestAutoPackingIsOffWhenTheBuildAsksForNoCache(t *testing.T) {
	t.Setenv("GOC_AUTOPACK", "")
	t.Setenv("CG12_NOCACHE", "1")
	assert.False(t, autoPackEnabled(), "CG12_NOCACHE has to bypass this cache along with every other one")

	t.Setenv("CG12_NOCACHE", "")
	t.Setenv("GOC_AUTOPACK", "0")
	assert.False(t, autoPackEnabled())

	t.Setenv("GOC_AUTOPACK", "")
	assert.True(t, autoPackEnabled())
}

// TestACachedCompileIsTheSameImageAsAColdOne is the whole safety argument, run
// end to end: a program compiled with an empty cache and the same program
// compiled with a warm one have to be the same bytes.
//
// It is a new failure mode. Determinism used to mean "two runs of the same
// compile agree"; it now also has to mean "a compile that read a pack off disk
// agrees with the one that built it", because the pack is a compilation from
// another process, possibly from another week.
func TestACachedCompileIsTheSameImageAsAColdOne(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a program twice")
	}
	compiler := sharedGOCBinary(t)
	directory := t.TempDir()
	source := filepath.Join(directory, "program.go")
	require.NoError(t, os.WriteFile(source, []byte(`package main

import (
	"fmt"
	"sort"
	"strings"
)

func main() {
	words := strings.Fields("the quick brown fox")
	sort.Strings(words)
	fmt.Println(strings.Join(words, "-"), len(words))
}
`), 0o644))

	cache := filepath.Join(directory, "packs")
	compile := func(output string) string {
		build := exec.Command(compiler, "-o", filepath.Join(directory, output), source)
		build.Env = append(os.Environ(), "CG12_PACK_CACHE="+cache, "GOC_AUTOPACK_DEBUG=1")
		combined, err := build.CombinedOutput()
		require.NoError(t, err, "%s", combined)
		return string(combined)
	}

	cold := compile("cold.bin")
	assert.Contains(t, cold, "building pack ", "an empty cache has to build the pack")
	warm := compile("warm.bin")
	assert.Contains(t, warm, "hit ", "a warm cache has to hit")

	coldBytes, err := os.ReadFile(filepath.Join(directory, "cold.bin"))
	require.NoError(t, err)
	warmBytes, err := os.ReadFile(filepath.Join(directory, "warm.bin"))
	require.NoError(t, err)
	assert.Equal(t, coldBytes, warmBytes, "a cached pack has to produce the same image as a freshly built one")

	// And it runs.
	output, err := exec.Command(filepath.Join(directory, "warm.bin")).CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.Equal(t, "brown-fox-quick-the 4\n", string(output))
}

// TestEveryChangeThatWouldMakeAPackWrongProducesAMiss drives the real compiler,
// because the key covers two things a test in this process cannot change: the
// compiler binary's own bytes, and an environment variable that a package read at
// init.
//
// Each arm changes one thing and requires the compiler to report a miss for a
// program it has just compiled from a warm cache. A miss is the safe answer; a
// hit here would be a program linked against a runtime built by a different
// compiler, a different standard library, or a different placement policy.
func TestEveryChangeThatWouldMakeAPackWrongProducesAMiss(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a program once per arm")
	}
	compiler := sharedGOCBinary(t)
	directory := t.TempDir()
	source := filepath.Join(directory, "program.go")
	require.NoError(t, os.WriteFile(source, []byte("package main\n\nfunc main() { println(1) }\n"), 0o644))
	cache := filepath.Join(directory, "packs")

	compile := func(t *testing.T, compiler string, environment ...string) string {
		t.Helper()
		build := exec.Command(compiler, "-o", filepath.Join(t.TempDir(), "out.bin"), source)
		build.Env = append(append(os.Environ(), "CG12_PACK_CACHE="+cache, "GOC_AUTOPACK_DEBUG=1"), environment...)
		combined, err := build.CombinedOutput()
		require.NoError(t, err, "%s", combined)
		return string(combined)
	}

	// Warm the cache, and check it is warm, so that every miss below is caused by
	// the arm and not by an empty cache.
	compile(t, compiler)
	require.Contains(t, compile(t, compiler), "hit ", "the cache has to be warm before an arm can mean anything")

	// -O is a flag rather than a variable, so it gets its own arm.
	t.Run("-O", func(t *testing.T) {
		build := exec.Command(compiler, "-O", "-o", filepath.Join(t.TempDir(), "out.bin"), source)
		build.Env = append(os.Environ(), "CG12_PACK_CACHE="+cache, "GOC_AUTOPACK_DEBUG=1")
		combined, err := build.CombinedOutput()
		require.NoError(t, err, "%s", combined)
		assert.Contains(t, string(combined), "building pack ", "changing -O has to miss")
	})

	// A different compiler binary. Rebuilt stripped, so its bytes differ and
	// nothing it does differs -- the mildest compiler change there is, and
	// therefore the sharpest test of hashing the binary rather than versioning it.
	t.Run("the compiler binary", func(t *testing.T) {
		other := filepath.Join(directory, "goc-stripped")
		build := exec.Command("go", "build", "-ldflags=-s -w", "-o", other, ".")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build a second compiler: %v\n%s", err, output)
		}
		assert.Contains(t, compile(t, other), "building pack ", "a compiler with different bytes has to miss")
	})

	for name, environment := range map[string]string{
		"the placement policy":      "GOC_FUNC_ALIGN=64",
		"the optimization pipeline": "GOC_OPT_PIPELINE=bounded",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, compile(t, compiler, environment), "building pack ",
				"changing %s has to miss", name)
		})
	}

	// Changing the standard library the compiler reads has to miss too. Doing it
	// by editing the repository's tree would leave a test able to corrupt the
	// checkout it ran in, so the tree-content half of the key is checked against
	// a scratch tree, and the selection half -- GOC_STDLIB_OVERLAY, which changes
	// which files a package is built from without changing a byte of the tree --
	// against the key directly, in the environment test above.
	t.Run("the standard library tree", func(t *testing.T) {
		scratch := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(scratch, "a.go"), []byte("package a\n"), 0o644))
		before, err := packCacheKey(runtimepack.Version, "arm64", false, nil, scratch)
		require.NoError(t, err)

		require.NoError(t, os.MkdirAll(filepath.Join(scratch, "b"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(scratch, "b", "c.go"), []byte("package c\n"), 0o644))
		added, err := packCacheKey(runtimepack.Version, "arm64", false, nil, scratch)
		require.NoError(t, err)
		assert.NotEqual(t, before, added, "a file added anywhere under the tree has to move the key")

		require.NoError(t, os.WriteFile(filepath.Join(scratch, "b", "c.go"), []byte("package d\n"), 0o644))
		edited, err := packCacheKey(runtimepack.Version, "arm64", false, nil, scratch)
		require.NoError(t, err)
		assert.NotEqual(t, added, edited, "a byte changed anywhere under the tree has to move the key")
	})
}

// TestConcurrentColdCompilesAgreeOnOnePack is the concurrency case: a suite
// starts many compiles at once, they all want the same pack, and none of them
// can see the others.
//
// What has to hold is that every one of them ends with a whole pack and the same
// image. The lock is what stops them all building it, and it is an optimization
// over a sequence that is already safe -- so this checks the safety, and reports
// how many builds actually happened rather than requiring one.
func TestConcurrentColdCompilesAgreeOnOnePack(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a program several times at once")
	}
	compiler := sharedGOCBinary(t)
	directory := t.TempDir()
	source := filepath.Join(directory, "program.go")
	require.NoError(t, os.WriteFile(source, []byte("package main\n\nfunc main() { println(2) }\n"), 0o644))
	cache := filepath.Join(directory, "packs")

	const racers = 4
	outputs := make([]string, racers)
	logs := make([]string, racers)
	errors := make([]error, racers)
	done := make(chan int, racers)
	for index := 0; index < racers; index++ {
		outputs[index] = filepath.Join(directory, fmt.Sprintf("out%d.bin", index))
		go func(index int) {
			build := exec.Command(compiler, "-o", outputs[index], source)
			build.Env = append(os.Environ(), "CG12_PACK_CACHE="+cache, "GOC_AUTOPACK_DEBUG=1")
			combined, err := build.CombinedOutput()
			logs[index], errors[index] = string(combined), err
			done <- index
		}(index)
	}
	for index := 0; index < racers; index++ {
		<-done
	}

	built := 0
	for index := 0; index < racers; index++ {
		require.NoError(t, errors[index], "%s", logs[index])
		if strings.Contains(logs[index], "building pack ") {
			built++
		}
	}
	t.Logf("%d of %d concurrent compiles reached the build step", built, racers)

	first, err := os.ReadFile(outputs[0])
	require.NoError(t, err)
	for index := 1; index < racers; index++ {
		other, err := os.ReadFile(outputs[index])
		require.NoError(t, err)
		assert.Equal(t, first, other, "every racer has to end with the same image")
	}

	// Exactly one pack, whole: a half-written one must never be readable, and the
	// atomic rename is what guarantees it however many builders raced.
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	packs := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".gocrt") {
			packs++
			manifest, err := runtimepack.ReadManifest(filepath.Join(cache, entry.Name()))
			require.NoError(t, err, "a pack in the cache has to be whole")
			assert.Equal(t, runtimepack.Version, manifest.Version)
		}
	}
	assert.Equal(t, 1, packs, "the racers all wanted the same key, so there is one pack")
}
