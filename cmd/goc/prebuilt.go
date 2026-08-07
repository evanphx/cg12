package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/cachefile"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/internal/runtimepack"
)

// buildRuntimeCommand implements `goc build-runtime -o runtime.gocrt`: it
// compiles the Go runtime once, as a Go module of its own, so that every program
// linked against it can skip compiling the runtime again.
//
// The result is not a bare object. A program compiled against it has to know
// exactly which symbols it defines, so the objects and that manifest travel
// together in one versioned file -- see internal/runtimepack for why that beats
// an archive or a bare ELF.
func buildRuntimeCommand(arguments []string, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("goc build-runtime", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	output := flags.String("o", "", "write the prebuilt runtime here")
	optimize := flags.Bool("O", false, "optimize cg12 IR")
	targetName := flags.String("target", defaultTargetName(), "arm64")
	packages := flags.String("packages", "",
		"comma-separated standard library packages the pack carries beyond the runtime; empty for the runtime alone")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *output == "" {
		fmt.Fprintln(errorOutput, "usage: goc build-runtime [-O] [-target arch] [-packages list] -o runtime.gocrt")
		return 2
	}
	target, err := goc.ParseTarget(*targetName)
	if err != nil {
		fmt.Fprintf(errorOutput, "%v\n", err)
		return 1
	}
	carried := splitCommaList(*packages)
	cache := packCacheDirectory()
	key, err := packCacheKey(runtimepack.Version, string(target), *optimize, carried, goc.StdlibRoot())
	if err != nil {
		// A key that cannot be computed means no caching, not a failed build.
		cache = ""
	}
	if readCachedPack(cache, key, *output) {
		return 0
	}
	pack, err := prebuilt.BuildRuntime(target, prebuilt.Options{
		Optimize: *optimize,
		Packages: carried,
	})
	if err != nil {
		fmt.Fprintf(errorOutput, "goc: %v\n", err)
		return 1
	}
	encoded, err := pack.Marshal()
	if err != nil {
		fmt.Fprintf(errorOutput, "goc: %v\n", err)
		return 1
	}
	if err := cachefile.WriteFileAtomically(*output, encoded); err != nil {
		fmt.Fprintf(errorOutput, "goc: %v\n", err)
		return 1
	}
	if err := writeCachedPack(cache, key, encoded); err != nil {
		fmt.Fprintf(errorOutput, "goc: could not cache the prebuilt runtime: %v\n", err)
	}
	return 0
}

// splitCommaList parses a comma-separated flag value. An empty or whitespace-only
// entry is dropped rather than passed through, so `-packages ""` and
// `-packages " "` both mean "the runtime alone" and a trailing comma in
// `-runtime` is not a path.
func splitCommaList(value string) []string {
	var entries []string
	for _, field := range strings.Split(value, ",") {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// packSet is the set of prebuilt runtimes one build may choose between: every
// pack's manifest, read up front, and each pack's objects read the first time
// some program picks that pack and kept afterwards.
//
// The two halves are read at different times because the choice depends on the
// program. A pack carrying part of the standard library is usable only by a
// program whose own loaded closure contains the pack's whole closure (§19), so
// the pack a program gets is not known until the front end has run. Choosing
// needs the manifests and nothing else, and a pack carrying net/http is tens of
// megabytes, so the manifests are read eagerly and the members lazily.
//
// Keeping what was read is what lets `goc compile-batch` amortize the read
// across a batch without giving up the selection: programs in one worker that
// pick the same pack share one read, and programs that pick different ones each
// pay for theirs once. A one-shot goc builds a set, uses it once and exits,
// which is exactly the work it did before.
type packSet struct {
	paths     []string
	manifests []*runtimepack.Manifest

	// mutex guards packs, and is held across the read rather than only across
	// the map write, so two callers wanting the same pack cannot both read it.
	// A batch worker compiles one program at a time, so nothing contends for it
	// today; it is here because a packSet is state shared by every compile in a
	// process, and a caller that compiled two programs at once would otherwise
	// race on the map with nothing in this tree to reproduce it.
	mutex sync.Mutex
	packs map[string]*runtimepack.Pack
}

// readPackSet reads every candidate pack's manifest.
func readPackSet(paths []string) (*packSet, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("goc: -runtime needs at least one prebuilt runtime")
	}
	set := &packSet{
		paths:     append([]string(nil), paths...),
		manifests: make([]*runtimepack.Manifest, 0, len(paths)),
		packs:     map[string]*runtimepack.Pack{},
	}
	for _, path := range paths {
		manifest, err := runtimepack.ReadManifest(path)
		if err != nil {
			return nil, err
		}
		set.manifests = append(set.manifests, manifest)
	}
	return set, nil
}

// packFor returns the objects belonging to a manifest this set offered, reading
// them on first use.
//
// The manifest is matched by identity, not by content: it is one of the pointers
// this set handed to the compiler, and the compiler hands the same pointer back
// to say which one it used. A manifest from anywhere else is a programming
// error, and it is reported as one rather than silently linking against
// whichever pack happened to be first -- an image built from one pack's objects
// and another pack's subtraction would fail at the linker in the best case and
// run with two copies of a package global in the worst.
func (set *packSet) packFor(manifest *runtimepack.Manifest) (*runtimepack.Pack, error) {
	path := ""
	for index, candidate := range set.manifests {
		if candidate == manifest {
			path = set.paths[index]
			break
		}
	}
	if path == "" {
		return nil, fmt.Errorf("goc: the compiler chose a prebuilt runtime that is not one of %v", set.paths)
	}

	set.mutex.Lock()
	defer set.mutex.Unlock()
	if pack := set.packs[path]; pack != nil {
		return pack, nil
	}
	pack, err := runtimepack.Read(path)
	if err != nil {
		return nil, err
	}
	set.packs[path] = pack
	return pack, nil
}

// linkAgainstPrebuiltRuntime compiles one program as a second Go module and links
// it with the prebuilt runtime it can use most of.
//
// Several packs may be offered, because a pack carrying part of the standard
// library is only usable by a program that loads all of that library. Only the
// manifests are consulted to choose, and only the chosen pack's objects are ever
// read -- see packSet.
//
// errorOutput takes whatever the C toolchain says. It is a parameter because the
// batch compiler runs many of these in one process and has to attribute each
// linker complaint to the program that caused it; the command-line path passes
// os.Stderr and is unchanged.
//
// The object order is load-bearing. Each module's moduledata records a [minpc,
// maxpc) that runtime.findmoduledatap resolves a PC against, so a module's text
// has to be contiguous: the prebuilt object and the sidecar that ends its text
// come first and adjacent, and the program's text follows.
func linkAgainstPrebuiltRuntime(
	target goc.Target,
	packs *packSet,
	name string,
	source []byte,
	executable string,
	optimize bool,
	errorOutput io.Writer,
) error {
	program, err := prebuilt.CompileProgram(target, filepath.Base(name), source, packs.manifests, prebuilt.Options{Optimize: optimize})
	if err != nil {
		return err
	}
	pack, err := packs.packFor(program.Manifest)
	if err != nil {
		return err
	}
	// A temporary directory with fixed names inside, rather than temporary files
	// with random ones: cc records an object's filename in the linked binary, so
	// random names make two builds of the same source differ.
	work, err := os.MkdirTemp("", "cg12-goc-split")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	runtimeObject := filepath.Join(work, "goc-runtime-module.o")
	sidecarObject := filepath.Join(work, "goc-runtime.o")
	programObject := filepath.Join(work, "goc.o")
	inputs := map[string][]byte{
		runtimeObject: pack.Object,
		sidecarObject: pack.Sidecar,
		programObject: program.Object,
	}
	linkInputs := []string{runtimeObject, sidecarObject, programObject}
	if len(program.Sidecar) > 0 {
		programSidecar := filepath.Join(work, "goc-program-runtime.o")
		inputs[programSidecar] = program.Sidecar
		linkInputs = append(linkInputs, programSidecar)
	}
	for path, contents := range inputs {
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			return err
		}
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		return err
	}
	command := exec.Command(cc, append([]string{"-no-pie", "-o", executable}, linkInputs...)...)
	command.Stderr = errorOutput
	return command.Run()
}
