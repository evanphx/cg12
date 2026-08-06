// Package prebuilt builds a prebuilt goc runtime and compiles programs against
// one.
//
// It is the driver half of the split: goc lowers the two modules (see
// goc/runtime_split.go), internal/runtimepack is their on-disk form, and this
// package is what turns one into the other -- running the backend, assembling the
// Plan 9 sidecar, writing moduledata.next, and reading back the symbol table that
// tells the program side what it may leave out.
package prebuilt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/evanphx/cg12/opt"
)

// Options are the compilation settings both halves of a split must agree on.
// They are recorded in the manifest, so a program built with different ones is
// refused rather than linked against a runtime that was compiled differently.
type Options struct {
	Optimize bool

	// Packages are the standard library packages the pack carries beyond the Go
	// runtime itself, as import paths. Empty means the runtime alone.
	Packages []string

	// Compose makes a program build optimize the whole program -- the pack's
	// functions included -- and only then subtract the definitions the pack
	// already has, instead of subtracting first and optimizing the remainder.
	//
	// It is what stops a pack costing code quality: the optimizer sees the module
	// a monolithic build sees, so a program function can inline a pack function.
	// It costs the optimizer saving the object pack had, because the module the
	// optimizer runs over is the whole program again; see PackMode.
	Compose bool

	// CarryIR makes a runtime build serialize the optimized prebuilt module into
	// the pack alongside its object, and makes a program build seed its own
	// module with those bodies rather than re-optimizing them.
	//
	// This is gc's arrangement -- an object for the callee's own code, serialized
	// IR for the caller to inline from -- and it is the only reason for a pack to
	// carry IR at all in goc: goc's front end is whole-program and re-derives
	// every function the pack holds regardless, so pack IR cannot save the front
	// end. What it can save is the optimizer's work on the packed functions.
	CarryIR bool

	// PackIR yields the IR member of a pack this build was offered. It is a
	// callback rather than a field because which pack a program uses is not known
	// until its front end has run, and a pack is tens of megabytes: the manifests
	// are read to choose, and only the chosen pack's members are ever read.
	PackIR func(*runtimepack.Manifest) ([]byte, error)
}

// BuildRuntime compiles the fixed runtime root as a prebuilt Go module and
// returns the pack: the module's object, its assembled sidecar, and the manifest
// describing what the two define.
func BuildRuntime(target goc.Target, options Options) (*runtimepack.Pack, error) {
	if target != goc.TargetARM64 {
		return nil, fmt.Errorf("goc: a prebuilt runtime is only available for arm64, not %s", target)
	}
	runtimeModule, err := goc.CompileRuntimeModuleFor(target, options.Packages)
	if err != nil {
		return nil, err
	}
	assemblyFiles := make([]string, 0, len(runtimeModule.Module.Assembly))
	for _, assembly := range runtimeModule.Module.Assembly {
		assemblyFiles = append(assemblyFiles, assembly.Path)
	}
	sort.Strings(assemblyFiles)
	if options.Optimize {
		opt.OptimizeModule(runtimeModule.Module)
	}
	// Serialized before the backend, which mutates the module it lowers. These are
	// the bodies a program build inlines from, so they must be the bodies the
	// pack's own object was generated from and nothing later.
	var carriedIR []byte
	if options.CarryIR {
		carriedIR, err = runtimeModule.Module.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("serialize the prebuilt module: %w", err)
		}
	}
	object, assembly, err := arm64.CompileToObjectAndAssembly(runtimeModule.Module)
	if err != nil {
		return nil, fmt.Errorf("arm64: %w", err)
	}
	// The one write that joins the program module to the image. The child is not
	// defined here and will not be until a program is compiled against this pack,
	// so it is named rather than pointed at, and the system linker resolves it.
	err = gometa.ChainModuleToExternal(
		object,
		gometa.DefaultModuleDataSymbol,
		ir.LinkerSymbol(goc.ProgramModuleDataSymbol),
		obj.R_AARCH64_ABS64,
	)
	if err != nil {
		return nil, err
	}
	objectBytes, err := object.MarshalELF()
	if err != nil {
		return nil, err
	}
	sidecar, err := assemble(assembly)
	if err != nil {
		return nil, err
	}
	sidecarObject, err := obj.ReadELF(sidecar)
	if err != nil {
		return nil, fmt.Errorf("read assembled sidecar: %w", err)
	}
	return &runtimepack.Pack{
		Manifest: runtimepack.Manifest{
			Version:             runtimepack.Version,
			Target:              string(target),
			Fingerprint:         runtimeModule.Fingerprint,
			Optimize:            options.Optimize,
			IRVersion:           irVersionFor(carriedIR),
			IRDigest:            runtimepack.DigestOf(carriedIR),
			Packages:            append([]string(nil), options.Packages...),
			Closure:             runtimeModule.Closure,
			ModuleDataSymbol:    gometa.DefaultModuleDataSymbol,
			ProgramModuleSymbol: ir.LinkerSymbol(goc.ProgramModuleDataSymbol),
			Defined:             definedGlobals(object, sidecarObject),
			AssemblyFiles:       assemblyFiles,
			DataDigests:         runtimeModule.DataDigests,
			ProgramSymbols:      runtimeModule.ProgramSymbols,
		},
		Object:  objectBytes,
		Sidecar: sidecar,
		IR:      carriedIR,
	}, nil
}

// irVersionFor reports the ir binary format version a carried payload was written
// with, or 0 when the pack carries no IR.
//
// It is read out of the payload rather than named as a constant here, so the key
// cannot claim a version the encoder did not write. A pack whose IRVersion does
// not match this compiler's is refused on read: an IR format change that was not
// a cache miss would be a wrong binary rather than a slow build.
func irVersionFor(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	return ir.BinaryVersion
}

// Program is a compiled program module: its object, and the sidecar it needs for
// any package assembly the prebuilt runtime did not already carry.
type Program struct {
	Object []byte

	// Sidecar is empty when the program reaches no package the prebuilt module
	// had not already assembled, which is the common case.
	Sidecar []byte

	// Manifest is the pack this module was compiled against, chosen from the ones
	// the caller offered. It is what the caller must link with.
	Manifest *runtimepack.Manifest
}

// CompileProgram lowers a program as a module to be linked against whichever of
// manifests it can use most of. It returns the choice, so the caller can pair the
// module with that pack's objects.
func CompileProgram(target goc.Target, name string, source []byte, manifests []*runtimepack.Manifest, options Options) (*Program, error) {
	for _, manifest := range manifests {
		if string(target) != manifest.Target {
			return nil, fmt.Errorf("goc: the prebuilt runtime is for %s, not %s", manifest.Target, target)
		}
		if manifest.Optimize != options.Optimize {
			return nil, fmt.Errorf("the prebuilt runtime was built with -O=%v, but this program is being compiled with -O=%v",
				manifest.Optimize, options.Optimize)
		}
	}
	compile := goc.CompileExecutableAgainstRuntimeFor
	if options.Compose {
		compile = goc.CompileComposedExecutableAgainstRuntimeFor
	}
	program, err := compile(target, name, source, manifests...)
	if err != nil {
		return nil, err
	}
	chosen := manifests[program.Chosen]
	if !options.Compose {
		if err := checkProgramSymbols(program.Module, chosen); err != nil {
			return nil, err
		}
		if options.Optimize {
			opt.OptimizeModule(program.Module)
		}
	} else {
		// The module is still the whole program here. Seed it with the pack's own
		// optimized bodies where the pack has them, so the inliner works from the
		// code the pack's object actually contains rather than re-deriving it, then
		// optimize the whole thing, then subtract.
		if options.CarryIR {
			if err := seedFromPackIR(program.Module, chosen, options.PackIR); err != nil {
				return nil, err
			}
		}
		if options.Optimize {
			opt.OptimizeModule(program.Module)
		}
		if err := program.Finish(program.Module); err != nil {
			return nil, err
		}
		if err := checkProgramSymbols(program.Module, chosen); err != nil {
			return nil, err
		}
	}
	object, assembly, err := arm64.CompileToObjectAndAssembly(program.Module)
	if err != nil {
		return nil, err
	}
	objectBytes, err := object.MarshalELF()
	if err != nil {
		return nil, err
	}
	compiled := &Program{Object: objectBytes, Manifest: chosen}
	if len(program.Module.Assembly) > 0 {
		compiled.Sidecar, err = assemble(assembly)
		if err != nil {
			return nil, err
		}
	}
	return compiled, nil
}

// seedFromPackIR replaces a composed program module's copy of each function the
// pack carries with the pack's own optimized body.
//
// This is what the IR member is for, and it is the only work in goc a pack's IR
// can save. It cannot save the front end: goc lowers a whole program at once and
// funcDecl fills go/types-keyed maps (typeTags, runtimeTypes, interfaceItabs,
// interfaceCallWrappers) that four whole-program steps consume after the lowering
// loop, so a package's lowering cannot be skipped in favour of a serialized
// artifact without under-populating the module's type region. What it can save is
// the optimizer: the pack's bodies arrive at their fixpoint already, so the
// passes converge on them instead of re-deriving what the pack build derived.
//
// The substitution is by symbol, which is the identity cross-unit references
// already use (ir.LinkerSymbol mangles a name; ir.Const.Sym carries one), so
// nothing has to be fixed up. The functions substituted here are all subtracted
// again by ProgramModule.Finish -- the pack's object defines them -- so their
// only effect on the output is what the inliner copied out of them.
func seedFromPackIR(module *ir.Module, manifest *runtimepack.Manifest, read func(*runtimepack.Manifest) ([]byte, error)) error {
	if read == nil {
		return fmt.Errorf("goc: an IR pack build needs a way to read the pack's IR")
	}
	if manifest.IRVersion == 0 {
		return fmt.Errorf("goc: the chosen prebuilt runtime carries no IR; rebuild it")
	}
	if manifest.IRVersion != ir.BinaryVersion {
		return fmt.Errorf("goc: the prebuilt runtime's IR is format version %d, and this goc reads version %d; rebuild it",
			manifest.IRVersion, ir.BinaryVersion)
	}
	encoded, err := read(manifest)
	if err != nil {
		return err
	}
	packed, err := ir.DecodeModule(encoded)
	if err != nil {
		return fmt.Errorf("decode the prebuilt runtime's IR: %w", err)
	}
	bodies := make(map[string]*ir.Func, len(packed.Funcs))
	for _, function := range packed.Funcs {
		bodies[function.Name] = function
	}
	for index, function := range module.Funcs {
		body := bodies[function.Name]
		if body == nil {
			continue
		}
		// The linkage is this compilation's, not the pack build's: the pack
		// exported everything it kept so a separately compiled program could
		// reference it, and this module is not separately compiled from those
		// functions -- it is about to inline them and then drop them.
		body.Linkage = function.Linkage
		module.Funcs[index] = body
	}
	return nil
}

// checkProgramSymbols refuses a program module that does not define everything
// the prebuilt object left for it.
//
// The link would fail anyway, but it would fail naming a mangled symbol with no
// explanation of whose job it was to define it. The interface-method dispatchers
// are the case that matters: a prebuilt module that called one the program does
// not generate is the "dispatcher that silently misses an itab" hazard, caught
// here at its source instead of at the link or, worse, at run time.
func checkProgramSymbols(module *ir.Module, manifest *runtimepack.Manifest) error {
	defined := make(map[string]bool, len(module.Funcs)+len(module.Data))
	for _, function := range module.Funcs {
		defined[ir.LinkerSymbol(function.Name)] = true
	}
	for _, data := range module.Data {
		defined[ir.LinkerSymbol(data.Name)] = true
	}
	var missing []string
	for _, symbol := range manifest.ProgramSymbols {
		if !defined[symbol] {
			missing = append(missing, symbol)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("goc: the prebuilt runtime expects this program to define %v, and it does not", missing)
}

// definedGlobals is every global symbol the pack's two objects define, in linker
// spelling. It is what the program side subtracts, so it is read back from the
// objects rather than predicted from the IR: a symbol the backend renamed, or one
// the assembler introduced, is in the object whether or not the frontend knew
// about it.
func definedGlobals(objects ...*obj.Object) []string {
	seen := map[string]bool{}
	for _, object := range objects {
		for _, symbol := range object.Syms {
			if symbol.Global && symbol.Section != obj.SecUndef {
				seen[symbol.Name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// assemble turns the translated Plan 9 sidecar into an object. cg12 has no
// assembler, so this step needs cc whichever linker finishes the job.
func assemble(source string) ([]byte, error) {
	work, err := os.MkdirTemp("", "goc-prebuilt")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	sourcePath := filepath.Join(work, "goc-runtime.S")
	objectPath := filepath.Join(work, "goc-runtime.o")
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		return nil, err
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		return nil, err
	}
	command := exec.Command(cc, "-c", "-o", objectPath, sourcePath)
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("assemble the Go runtime sidecar: %w", err)
	}
	return os.ReadFile(objectPath)
}
