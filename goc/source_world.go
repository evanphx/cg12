package goc

import (
	"go/token"
	"maps"
	"os"
	"sync"
)

// A sourceWorld is a frozen set of standard-library packages, parsed and
// type-checked once and then shared by every compile that selects the same
// files.
//
// It exists because the fixed cost of a compile dwarfs the variable one. A
// program whose whole body is `println(1)` takes about two seconds, of which
// three milliseconds is the program: the rest is the Go runtime and its
// internal dependencies -- 28 packages, some 311 files -- being re-parsed and
// re-type-checked from source, exactly as they were for the previous compile
// and the one before that. Nothing about that work depends on the program, and
// nothing about it changes between programs, so a suite that compiles hundreds
// of small programs spends nearly all of its time rediscovering the same
// standard library.
//
// A world is immutable once built. Loaders do not write to it; they take a
// shallow copy of its unit map, so a package the world does not have is loaded
// privately and thrown away with the loader. That keeps sharing free of locking
// on the hot path: after the first compile populates a world, every later
// compile only reads it.
type sourceWorld struct {
	fset  *token.FileSet
	units map[string]*sourceUnit
}

// sourceWorldKey identifies a world by everything that decides which files a
// package is built from, or how they type-check.
//
// Getting this wrong is the one way a cache like this can be worse than no
// cache at all: a world reused across a boundary it should not cross would
// serve a package built from the wrong files, and the compile would succeed
// with the wrong answer. Both fields therefore describe file *selection*, not
// preference. target picks the architecture's files and build tags;
// forcePureGo decides whether assembly is used, which adds the purego tag and
// changes which files exist for a package. Compile sets forcePureGo for
// non-executables while CompileExecutable does not, so those two paths are
// legitimately different worlds rather than one world with a flag.
//
// Test packages are deliberately absent: a loader configured with them selects
// additional files, so it does not use a world at all.
type sourceWorldKey struct {
	target      Target
	forcePureGo bool
}

var (
	sourceWorldsMutex sync.Mutex
	sourceWorlds      = map[sourceWorldKey]*sourceWorld{}
)

// sourceWorldsDisabled reports whether world sharing is turned off.
//
// The merge gate builds cold. A cache defect can then cost time but cannot
// produce a passing test from a package that was never rebuilt, which is the
// failure this project can least afford to add.
func sourceWorldsDisabled() bool {
	return os.Getenv("CG12_NOCACHE") != ""
}

// sharedSourceWorld returns the world for a key, building it on first use.
//
// The build is serialized rather than single-flighted per key. Worlds are built
// once per process and the work is the same for every caller, so the simpler
// lock costs one compile's wait at startup and nothing afterwards.
func sharedSourceWorld(target Target, forcePureGo bool) *sourceWorld {
	if sourceWorldsDisabled() {
		return nil
	}
	key := sourceWorldKey{target: target.resolve(), forcePureGo: forcePureGo}

	sourceWorldsMutex.Lock()
	defer sourceWorldsMutex.Unlock()
	if world := sourceWorlds[key]; world != nil {
		return world
	}

	fset := token.NewFileSet()
	loader := newSourceLoader(fset, key.target)
	loader.forcePureGo = key.forcePureGo
	// runtime is the seed because every executable imports it unconditionally
	// and its closure is the part every program shares. A program that imports
	// more than this loads the remainder privately; a world is a floor, not a
	// prediction.
	if _, err := loader.Import("runtime"); err != nil {
		// A world is an optimization. If the seed cannot be built, the caller
		// compiles without one and reports the real error itself.
		return nil
	}

	world := &sourceWorld{fset: fset, units: maps.Clone(loader.units)}
	sourceWorlds[key] = world
	return world
}

// adopt pre-populates a loader with the world's packages.
//
// The units are shared, not copied: they are read-only once the world is
// frozen, and copying them would give back the memory this is meant to save.
// The map is copied, so a package the loader loads on its own stays local to
// it.
func (world *sourceWorld) adopt(loader *sourceLoader) {
	for path, unit := range world.units {
		loader.units[path] = unit
	}
}
