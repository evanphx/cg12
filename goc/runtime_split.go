package goc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/types"
	"sort"
	"strconv"

	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/evanphx/cg12/ir"
)

// This file splits one goc compilation into two: a prebuilt Go module holding
// the runtime (and whatever standard library the root program reaches), and a
// program module holding only the difference.
//
// The split is subtractive, and that is the whole of its safety argument. Both
// halves run the same whole-program front end -- the same parse, the same type
// check, the same reachability walk, the same interface analysis -- and produce
// the same IR they would have produced on their own. Only at the very end does
// the program module drop the definitions the prebuilt object already has, and
// reference them instead. A kept symbol is therefore bit-for-bit what a
// monolithic build would have emitted, which is what makes a differential
// comparison of the two images meaningful rather than merely encouraging.
//
// Two things must not be subtracted, for opposite reasons:
//
//   - The interface-method dispatchers (goc's generated error_Error,
//     runtime_stringer_String, ...) switch over the concrete types the *program*
//     contains and end in runtime_gocInterfaceDispatchFailure, with no fallback.
//     They are runtime-named but program-built, so the prebuilt object leaves
//     them undefined and the program module defines and exports them.
//   - A datum a kept type descriptor addresses with a 32-bit module-relative
//     offset (NameOff/TypeOff) has to live in the same module as the descriptor.
//     Pointers cross modules freely; these offsets do not. So the subtraction is
//     closed under RelativeTo: subtracting such a target is undone.

// ProgramModuleDataSymbol names the runtime.moduledata record of a program
// module compiled against a prebuilt runtime.
//
// It cannot be runtime.firstmoduledata: that symbol is global and belongs to the
// module carrying the runtime's own state, which in this topology is the
// prebuilt object. The prebuilt object's moduledata.next names this symbol, so
// the two compilations agree on the spelling and the system linker resolves it.
const ProgramModuleDataSymbol = "goc.programmoduledata"

// sharedModuleSymbols are the data symbols every Go module defines for itself.
// They must stay local in both halves: exporting either module's copy would make
// the two collide at link time, and the program module genuinely needs its own.
var sharedModuleSymbols = map[string]bool{
	ir.LinkerSymbol(".goc.runtime.datastart"): true,
	ir.LinkerSymbol(".goc.runtime.dataend"):   true,
	ir.LinkerSymbol(".goc.module.inittasks"):  true,
	ir.LinkerSymbol(".goc.module.itablinks"):  true,
}

// lastModuleSymbolFor names the moduledata at the tail of the image's module
// chain, which is what runtime.lastmoduledatap holds.
//
// runtime.main runs each module's package init tasks by walking the chain from
// runtime.firstmoduledata and stopping at the tail, so a prebuilt runtime module
// that still claimed to be the tail would run its own packages' init and then
// stop -- the program's own init would never run, and the failure looks like a
// program whose globals were never assigned rather than like a linking mistake.
// Both halves of a split answer the same way -- the tail is the program module
// either way -- so the datum has one content and one digest, and the subtraction
// treats it like any other runtime global rather than needing an exception.
func lastModuleSymbolFor(split *runtimeSplit) string {
	if split.buildsRuntime() || split.againstRuntime() {
		return ProgramModuleDataSymbol
	}
	return "runtime.firstmoduledata"
}

// runtimeSplitMode selects which half of a split compilation this is.
type runtimeSplitMode int

const (
	runtimeSplitNone runtimeSplitMode = iota

	// runtimeSplitBuildRuntime compiles the prebuilt module: everything the root
	// program reaches except the parts that are program-built by construction.
	runtimeSplitBuildRuntime

	// runtimeSplitAgainstRuntime compiles a program module against a manifest.
	runtimeSplitAgainstRuntime
)

// runtimeSplit carries the split's inputs into compile and its findings back out.
type runtimeSplit struct {
	mode runtimeSplitMode

	// candidates are the packs the caller offered a program build. compile picks
	// one as soon as it knows what the program loads; manifest is that choice.
	candidates []*runtimepack.Manifest
	manifest   *runtimepack.Manifest

	// chosen indexes candidates, so the caller knows which pack's objects to link.
	chosen int

	// programSymbols is filled by a runtime build: the symbols it deliberately
	// left undefined for the program module to supply.
	programSymbols []string

	// dataDigests is filled by a runtime build: one digest per data symbol it
	// defines, so a program build can refuse to subtract a datum whose contents
	// have drifted.
	dataDigests map[string]string

	// fingerprint identifies the runtime source the prebuilt module was compiled
	// from, so a caller can tell a current pack from a stale one.
	fingerprint string

	// closure is filled by a runtime build: every package path its compilation
	// loaded. A program may only use the pack if it loads all of them.
	closure []string

	// The counts a caller reports.
	keptFunctions       int
	keptData            int
	subtractedFunctions int
	subtractedData      int
}

func (split *runtimeSplit) buildsRuntime() bool {
	return split != nil && split.mode == runtimeSplitBuildRuntime
}

func (split *runtimeSplit) againstRuntime() bool {
	return split != nil && split.mode == runtimeSplitAgainstRuntime
}

// chooseManifest picks the pack this program will be compiled against: the one
// carrying the most, of those whose closure the program's closure contains.
//
// Containment is the condition, not equality, and it is what the whole
// arrangement rests on. The pack leaves its Go type region, its interface
// dispatchers and its degraded itabs for the program module, and a program only
// generates those for packages it loaded. Load everything the pack loaded and the
// program generates a superset; load less and the link fails naming symbols
// nobody defines.
//
// A runtime-only pack has an empty carried set and is therefore usable by every
// executable, since goc compiles the whole runtime closure into any of them. So
// offering a rich pack alongside a plain one degrades to the plain one rather
// than to an error.
func (split *runtimeSplit) chooseManifest(closure []string) error {
	loaded := make(map[string]bool, len(closure))
	for _, path := range closure {
		loaded[path] = true
	}
	best := -1
	for index, candidate := range split.candidates {
		if !candidate.UsableBy(loaded) {
			continue
		}
		if best < 0 || len(candidate.Closure) > len(split.candidates[best].Closure) {
			best = index
		}
	}
	if best < 0 {
		return fmt.Errorf("goc: none of the %d prebuilt runtimes offered is usable by this program", len(split.candidates))
	}
	split.chosen = best
	split.manifest = split.candidates[best]
	return nil
}

// RuntimeModule is the result of compiling the prebuilt runtime module.
type RuntimeModule struct {
	Module *ir.Module

	// ProgramSymbols are the symbols the module leaves undefined because they are
	// program-built. The program module must define and export exactly these.
	ProgramSymbols []string

	// DataDigests identifies the contents of each data symbol the module defines.
	DataDigests map[string]string

	// Fingerprint identifies the runtime source the module was compiled from.
	Fingerprint string

	// Closure is every package path the module's compilation loaded, sorted.
	Closure []string
}

// ProgramModule is the result of compiling a program against a prebuilt runtime.
type ProgramModule struct {
	Module *ir.Module

	// Chosen indexes the manifests the caller offered: the pack this module was
	// actually compiled against, and therefore the one to link it with.
	Chosen int

	KeptFunctions       int
	KeptData            int
	SubtractedFunctions int
	SubtractedData      int
}

// CompileRuntimeModuleFor lowers a prebuilt runtime module for a target: the Go
// runtime, plus the closure of the standard library packages named in packages.
//
// The root program is generated here rather than supplied by the caller as
// source, because the whole point is that the result does not depend on any
// particular user program -- only on a list of packages. What the root reaches is
// a superset, not an exact fit: a program needing more compiles the difference
// itself, which is what makes the subtraction degrade gracefully rather than
// requiring the prebuilt module to anticipate every program.
func CompileRuntimeModuleFor(target Target, packages []string) (*RuntimeModule, error) {
	source, err := runtimeRootSourceFor(packages)
	if err != nil {
		return nil, err
	}
	split := &runtimeSplit{mode: runtimeSplitBuildRuntime, dataDigests: map[string]string{}}
	module, err := compile(runtimeRootName, []byte(source), compileOptions{
		target:       target,
		executable:   true,
		runtimeSplit: split,
	})
	if err != nil {
		return nil, err
	}
	return &RuntimeModule{
		Module:         module,
		ProgramSymbols: split.programSymbols,
		DataDigests:    split.dataDigests,
		Fingerprint:    split.fingerprint,
		Closure:        split.closure,
	}, nil
}

// CompileExecutableAgainstRuntimeFor lowers a main package as a program module to
// be linked against one of the prebuilt runtimes described by manifests.
//
// More than one is allowed because a pack carrying part of the standard library
// is only usable by a program that loads all of it. Offering a rich pack and a
// plain one lets each program take the richest it can and the rest fall back,
// instead of forcing one pack to fit every program.
func CompileExecutableAgainstRuntimeFor(target Target, name string, src []byte, manifests ...*runtimepack.Manifest) (*ProgramModule, error) {
	if len(manifests) == 0 {
		return nil, fmt.Errorf("goc: compiling against a prebuilt runtime needs its manifest")
	}
	for _, manifest := range manifests {
		if manifest == nil {
			return nil, fmt.Errorf("goc: compiling against a prebuilt runtime needs its manifest")
		}
	}
	split := &runtimeSplit{mode: runtimeSplitAgainstRuntime, candidates: manifests}
	module, err := compile(name, src, compileOptions{
		target:       target,
		executable:   true,
		runtimeSplit: split,
	})
	if err != nil {
		return nil, err
	}
	return &ProgramModule{
		Module:              module,
		Chosen:              split.chosen,
		KeptFunctions:       split.keptFunctions,
		KeptData:            split.keptData,
		SubtractedFunctions: split.subtractedFunctions,
		SubtractedData:      split.subtractedData,
	}, nil
}

// loadedPackagePaths is every standard library package a compilation loaded,
// sorted.
//
// The compiled program's own package is left out: it is `main` in both halves of
// a split, so counting it would make every pack look usable by every program for
// the wrong reason.
func loadedPackagePaths(loader *sourceLoader, own *types.Package) []string {
	paths := make([]string, 0, len(loader.units))
	for path := range loader.units {
		if own != nil && path == own.Path() {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// finishRuntimeModule turns a finished whole-program module into the prebuilt
// runtime half: it drops the program-built definitions and exports what is left,
// so a separately compiled program module can reference all of it.
func finishRuntimeModule(module *ir.Module, split *runtimeSplit, programFunctions, programData []string) {
	module.Data = dropShadowedDefinitions(module.Data)
	programSymbols := make(map[string]bool, len(programFunctions)+len(programData))
	for _, name := range programFunctions {
		programSymbols[ir.LinkerSymbol(name)] = true
	}
	for _, name := range programData {
		programSymbols[ir.LinkerSymbol(name)] = true
	}
	for name := range typeRegionSymbols(module.Data) {
		programSymbols[name] = true
	}
	for name := range degradedSymbols(module.Data) {
		programSymbols[name] = true
	}
	dropItabLinks(module.Data, programSymbols)

	keptFunctions := module.Funcs[:0]
	for _, function := range module.Funcs {
		if programSymbols[ir.LinkerSymbol(function.Name)] {
			split.subtractedFunctions++
			continue
		}
		function.Export()
		keptFunctions = append(keptFunctions, function)
		split.keptFunctions++
	}
	module.Funcs = keptFunctions

	keptData := module.Data[:0]
	for _, data := range module.Data {
		name := ir.LinkerSymbol(data.Name)
		if programSymbols[name] {
			split.subtractedData++
			continue
		}
		if !sharedModuleSymbols[name] {
			data.Linkage.Export = true
		}
		split.dataDigests[name] = dataDigest(data)
		keptData = append(keptData, data)
		split.keptData++
	}
	module.Data = keptData

	// The prebuilt module is the head of the chain and does not define main, so
	// runtime.modulesinit moves the program module -- not this one -- to the front
	// of activeModules. That ordering only matters to typelinksinit, which has
	// nothing to do here: the program module owns the whole type region, so the
	// image has no duplicate descriptors to canonicalise.
	module.GoHasMain = false

	split.programSymbols = sortedUnique(programSymbols)
}

// finishProgramModule turns a finished whole-program module into the program
// half: it drops every definition the prebuilt object already has, exports the
// ones the prebuilt object left for it, and gives the module a moduledata of its
// own.
func finishProgramModule(module *ir.Module, split *runtimeSplit) error {
	module.Data = dropShadowedDefinitions(module.Data)
	manifest := split.manifest
	defined := manifest.DefinedSet()
	programSymbols := manifest.ProgramSymbolSet()

	// A kept type descriptor addresses its name, its package path and its method
	// signatures with 32-bit offsets from this module's base, so those targets
	// have to be in this module. Subtracting one is undone -- transitively, since
	// undoing it can expose more of them.
	dataByName := lastDefinitionByName(module.Data)
	keep := make(map[string]bool, len(module.Data))
	var pending []*ir.Data
	for _, data := range module.Data {
		name := ir.LinkerSymbol(data.Name)
		if defined[name] && !sharedModuleSymbols[name] {
			continue
		}
		keep[name] = true
		pending = append(pending, data)
	}
	for len(pending) > 0 {
		data := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, item := range data.Items {
			if item.Sym == "" || item.RelativeTo == "" {
				continue
			}
			target := ir.LinkerSymbol(item.Sym)
			if keep[target] {
				continue
			}
			referenced := dataByName[target]
			if referenced == nil {
				// Not a datum of this module at all: a method entry's TextOff names
				// a function, which the backend resolves against the text base.
				continue
			}
			keep[target] = true
			pending = append(pending, referenced)
		}
	}

	// "Same name, different content" is the failure mode a subtractive split has
	// to rule out, because it produces an image that links cleanly and then
	// behaves as though the program's own definition never existed. Compare the
	// digest of the datum this compilation would have emitted against the one the
	// prebuilt object recorded, and refuse the build if they disagree.
	var drifted []string
	for name, data := range lastDefinitionByName(module.Data) {
		if keep[name] || !defined[name] {
			continue
		}
		recorded, known := manifest.DataDigests[name]
		if known && recorded != dataDigest(data) {
			drifted = append(drifted, data.Name)
		}
	}
	if len(drifted) > 0 {
		sort.Strings(drifted)
		shown := drifted
		if len(shown) > 8 {
			shown = shown[:8]
		}
		return fmt.Errorf("goc: %d data symbols differ from the prebuilt runtime's definitions of the same name (%v); rebuild the runtime", len(drifted), shown)
	}

	keptData := module.Data[:0]
	for _, data := range module.Data {
		name := ir.LinkerSymbol(data.Name)
		if !keep[name] {
			split.subtractedData++
			continue
		}
		// Only a symbol the prebuilt object left for this module goes global. A
		// re-emitted duplicate stays local, so it cannot collide with the prebuilt
		// object's copy of the same name.
		data.Linkage.Export = programSymbols[name] && !defined[name]
		keptData = append(keptData, data)
		split.keptData++
	}
	module.Data = keptData

	keptFunctions := module.Funcs[:0]
	for _, function := range module.Funcs {
		name := ir.LinkerSymbol(function.Name)
		if defined[name] {
			split.subtractedFunctions++
			continue
		}
		function.Linkage.Export = programSymbols[name]
		keptFunctions = append(keptFunctions, function)
		split.keptFunctions++
	}
	module.Funcs = keptFunctions

	// The prebuilt object's Plan 9 sidecar already carries the assembly of every
	// package the prebuilt module loaded, so translating those files again would
	// define each of their symbols twice. Everything else stays: a program that
	// reaches reflect's methodValueCall or a crypto block function needs that
	// package's assembly, and the prebuilt module never saw it.
	assembled := manifest.AssemblyFileSet()
	keptAssembly := module.Assembly[:0]
	for _, assembly := range module.Assembly {
		if assembled[assembly.Path] {
			continue
		}
		keptAssembly = append(keptAssembly, assembly)
	}
	module.Assembly = keptAssembly

	// This module's own moduledata. runtime.firstmoduledata belongs to the
	// prebuilt object, which names this record in its moduledata.next.
	module.GoModuleData = ProgramModuleDataSymbol
	module.GoHasMain = true
	module.Data = append(module.Data, &ir.Data{
		Name:    ProgramModuleDataSymbol,
		Align:   8,
		Linkage: ir.Linkage{Export: true},
		Items:   []ir.DataItem{{Zero: moduleDataSize}},
	})
	return nil
}

// typeRegionSymbols is the set of data symbols that make up a module's Go type
// region: every datum holding a 32-bit module-relative offset (a descriptor) and
// everything such an offset addresses (its name, its package path, its method
// names, the signature descriptors of its methods, its PtrToThis).
//
// The prebuilt runtime module leaves all of it to the program module, and the
// reason is the opposite of what it looks like. It is not that the offsets cannot
// cross a module boundary -- that is true, but it would be satisfied by either
// module owning the region. It is that **a type descriptor's contents depend on
// the program**:
//
//   - clearUnavailableRuntimeMethodOffsets replaces a method entry with
//     runtime.unreachableMethod when the method is not compiled into the image.
//     The prebuilt module reaches fewer methods than a program that imports more,
//     so its descriptor for internal/abi.ArrayType says "unreachable" where the
//     program's names the real method.
//   - populateRuntimePointerTypes fills PtrToThis only when the pointer type is
//     also described, which again depends on what the program reaches.
//
// So the prebuilt module's descriptor is not merely different, it is *poorer*.
// Freezing it into every image would silently disable reflect method calls on
// types the program can reach and the runtime root could not. And two copies is
// not an option either: cg12 compares type descriptors by pointer -- the inline
// itab match in interfaceTypeWord and every candidate test in a generated
// interface dispatcher -- so a value tagged with one module's descriptor would
// not match the other's, and the dispatcher's failure path is a fatal throw.
//
// One descriptor per type, owned by the module that knows the most about it.
func typeRegionSymbols(definitions []*ir.Data) map[string]bool {
	byName := lastDefinitionByName(definitions)
	region := map[string]bool{}
	var pending []*ir.Data
	for _, data := range definitions {
		for _, item := range data.Items {
			if item.Sym == "" || item.RelativeTo == "" {
				continue
			}
			if !region[ir.LinkerSymbol(data.Name)] {
				region[ir.LinkerSymbol(data.Name)] = true
				pending = append(pending, data)
			}
			break
		}
	}
	for len(pending) > 0 {
		data := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, item := range data.Items {
			if item.Sym == "" || item.RelativeTo == "" {
				continue
			}
			target := ir.LinkerSymbol(item.Sym)
			if region[target] {
				continue
			}
			referenced := byName[target]
			if referenced == nil {
				// A method entry's Ifn/Tfn is a text offset naming a function, not a
				// datum of this module.
				continue
			}
			region[target] = true
			pending = append(pending, referenced)
		}
	}
	return region
}

// unreachableMethodSymbol is what goc writes into a method entry whose function
// this module does not compile.
const unreachableMethodSymbol = "runtime.unreachableMethod"

// degradedSymbols is the set of data symbols the prebuilt runtime module has
// written a "this method is not in the image" placeholder into.
//
// `clearUnavailableRuntimeMethodOffsets` replaces a method entry --  in a type
// descriptor or in a static itab -- with `runtime.unreachableMethod` when the
// method's function is not compiled into the module. That test is "is it in *this*
// module", and the prebuilt module's reachable set is a subset of every program's,
// so wherever it wrote the placeholder a program may well have the real method.
// Its copy of that datum is therefore strictly poorer, exactly as with a type
// descriptor's contents, and belongs to the program module.
//
// Detecting it by the placeholder rather than by a name prefix is deliberate: it
// is the degradation that matters, not which family the datum is in, and a datum
// the prebuilt module got right stays in the pack where it saves work.
func degradedSymbols(definitions []*ir.Data) map[string]bool {
	degraded := map[string]bool{}
	for _, data := range definitions {
		for _, item := range data.Items {
			if item.Sym == unreachableMethodSymbol {
				degraded[ir.LinkerSymbol(data.Name)] = true
				break
			}
		}
	}
	return degraded
}

// dropItabLinks removes the static itabs the prebuilt module is no longer going to
// define from its own itablinks list, which runtime.itabsinit walks. The list is
// built before the split runs, so an itab handed to the program module would
// otherwise be left in it as a dangling reference.
func dropItabLinks(definitions []*ir.Data, programSymbols map[string]bool) {
	for _, data := range definitions {
		if data.Name != gometaModuleItabLinksName {
			continue
		}
		kept := data.Items[:0]
		for _, item := range data.Items {
			if programSymbols[ir.LinkerSymbol(item.Sym)] {
				continue
			}
			kept = append(kept, item)
		}
		data.Items = kept
		data.PointerWords = data.PointerWords[:len(kept)]
	}
}

// gometaModuleItabLinksName is internal/gometa's ModuleItabLinksName, spelled here
// so this package does not depend on the backend's metadata emitter.
// TestModuleItabLinksNameMatchesGometa keeps the two in step.
const gometaModuleItabLinksName = ".goc.module.itablinks"

// dropShadowedDefinitions removes every data definition another definition of
// the same name shadows, keeping the last.
//
// goc emits a few package globals twice. An interface-typed global such as
// runtime.divideError gets a zeroed placeholder and then, later, the record
// holding its itab and its value; obj.prepareELF's symbol index keeps whichever
// came last, so the placeholder is dead bytes that every reference already skips.
// That went unnoticed while these symbols were local. Exporting them, which the
// split has to do so a program module can reference the runtime's globals rather
// than duplicating them, turns it into `multiple definition of
// runtime_divideError` from the system linker.
//
// Removing the shadowed copy is what the object writer already does in effect;
// doing it here means the module says what it means. The duplicate emission
// itself is a separate defect, recorded rather than fixed under this change.
func dropShadowedDefinitions(definitions []*ir.Data) []*ir.Data {
	lastIndex := make(map[string]int, len(definitions))
	for index, data := range definitions {
		lastIndex[ir.LinkerSymbol(data.Name)] = index
	}
	if len(lastIndex) == len(definitions) {
		return definitions
	}
	kept := definitions[:0]
	for index, data := range definitions {
		if lastIndex[ir.LinkerSymbol(data.Name)] != index {
			continue
		}
		kept = append(kept, data)
	}
	return kept
}

// lastDefinitionByName indexes a module's data by symbol, keeping the last
// definition of each name.
//
// goc emits a handful of package globals twice -- an interface-typed global such
// as runtime.divideError gets a zeroed placeholder and then the record holding
// its itab -- and the ELF writer's symbol index keeps whichever came last, so
// that is the definition every reference resolves to. Comparing the earlier one
// against the prebuilt module's digest would report drift where there is none.
func lastDefinitionByName(definitions []*ir.Data) map[string]*ir.Data {
	byName := make(map[string]*ir.Data, len(definitions))
	for _, data := range definitions {
		byName[ir.LinkerSymbol(data.Name)] = data
	}
	return byName
}

// moduleDataSize is runtime.moduledata's size on a 64-bit target. Only the
// placeholder's length; internal/gometa writes the record over it.
const moduleDataSize = 592

// dataDigest identifies a data definition by what it holds: its alignment, its
// pointer words, and each item's encoding, symbol and relative base. Two data
// symbols with the same name and the same digest are interchangeable, which is
// exactly the property the subtraction relies on.
func dataDigest(data *ir.Data) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "align=%d;typelink=%v;", data.Align, data.GoTypeLink)
	for _, word := range data.PointerWords {
		fmt.Fprintf(hash, "p%d,", word)
	}
	for _, item := range data.Items {
		fmt.Fprintf(hash, "|%d:%d:%d:%q:%s:%s:",
			item.Sub, item.Zero, item.Off, item.Str,
			ir.LinkerSymbol(item.Sym), ir.LinkerSymbol(item.RelativeTo))
		for _, value := range item.Ints {
			hash.Write([]byte(strconv.FormatInt(value, 16)))
			hash.Write([]byte{','})
		}
		for _, value := range item.Flts {
			hash.Write([]byte(strconv.FormatFloat(value, 'x', -1, 64)))
			hash.Write([]byte{','})
		}
	}
	sum := hash.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func sortedUnique(set map[string]bool) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}
