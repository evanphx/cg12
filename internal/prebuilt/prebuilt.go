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
}

// BuildRuntime compiles the fixed runtime root as a prebuilt Go module and
// returns the pack: the module's object, its assembled sidecar, and the manifest
// describing what the two define.
func BuildRuntime(target goc.Target, options Options) (*runtimepack.Pack, error) {
	if target != goc.TargetARM64 {
		return nil, fmt.Errorf("goc: a prebuilt runtime is only available for arm64, not %s", target)
	}
	runtimeModule, err := goc.CompileRuntimeModuleFor(target)
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
			ModuleDataSymbol:    gometa.DefaultModuleDataSymbol,
			ProgramModuleSymbol: ir.LinkerSymbol(goc.ProgramModuleDataSymbol),
			Defined:             definedGlobals(object, sidecarObject),
			AssemblyFiles:       assemblyFiles,
			DataDigests:         runtimeModule.DataDigests,
			ProgramSymbols:      runtimeModule.ProgramSymbols,
		},
		Object:  objectBytes,
		Sidecar: sidecar,
	}, nil
}

// Program is a compiled program module: its object, and the sidecar it needs for
// any package assembly the prebuilt runtime did not already carry.
type Program struct {
	Object []byte

	// Sidecar is empty when the program reaches no package the prebuilt module
	// had not already assembled, which is the common case.
	Sidecar []byte
}

// CompileProgram lowers a program as a module to be linked against pack.
func CompileProgram(target goc.Target, name string, source []byte, pack *runtimepack.Pack, options Options) (*Program, error) {
	if string(target) != pack.Manifest.Target {
		return nil, fmt.Errorf("goc: the prebuilt runtime is for %s, not %s", pack.Manifest.Target, target)
	}
	program, err := goc.CompileExecutableAgainstRuntimeFor(target, name, source, &pack.Manifest)
	if err != nil {
		return nil, err
	}
	if err := checkProgramSymbols(program.Module, &pack.Manifest); err != nil {
		return nil, err
	}
	if options.Optimize {
		opt.OptimizeModule(program.Module)
	}
	object, assembly, err := arm64.CompileToObjectAndAssembly(program.Module)
	if err != nil {
		return nil, err
	}
	objectBytes, err := object.MarshalELF()
	if err != nil {
		return nil, err
	}
	compiled := &Program{Object: objectBytes}
	if len(program.Module.Assembly) > 0 {
		compiled.Sidecar, err = assemble(assembly)
		if err != nil {
			return nil, err
		}
	}
	return compiled, nil
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
