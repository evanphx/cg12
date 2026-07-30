package goc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/evanphx/cg12/ir"
)

const splitTestProgram = `package main

type shape interface{ sides() int }

type square struct{ side int }

func (s square) sides() int { return 4 }

var initialized int

func init() { initialized = 7 }

func main() {
	var s shape = square{side: 2}
	if s.sides()*initialized != 28 {
		panic("bad")
	}
	println("ok")
}
`

// manifestFor builds the manifest a prebuilt module would ship, from the module
// itself. It is what internal/prebuilt does with the object's symbol table; here
// the IR names are enough, because this test only exercises the frontend half of
// the split.
func manifestFor(t *testing.T, runtimeModule *RuntimeModule) *runtimepack.Manifest {
	t.Helper()

	defined := make([]string, 0, len(runtimeModule.Module.Funcs)+len(runtimeModule.Module.Data))
	for _, function := range runtimeModule.Module.Funcs {
		if function.Linkage.Export {
			defined = append(defined, ir.LinkerSymbol(function.Name))
		}
	}
	for _, data := range runtimeModule.Module.Data {
		if data.Linkage.Export {
			defined = append(defined, ir.LinkerSymbol(data.Name))
		}
	}
	assembly := make([]string, 0, len(runtimeModule.Module.Assembly))
	for _, file := range runtimeModule.Module.Assembly {
		assembly = append(assembly, file.Path)
	}
	return &runtimepack.Manifest{
		Version:        runtimepack.Version,
		Target:         string(TargetARM64),
		Defined:        defined,
		DataDigests:    runtimeModule.DataDigests,
		AssemblyFiles:  assembly,
		ProgramSymbols: runtimeModule.ProgramSymbols,
	}
}

func buildSplit(t *testing.T) (*RuntimeModule, *runtimepack.Manifest, *ProgramModule) {
	t.Helper()

	runtimeModule, err := CompileRuntimeModuleFor(TargetARM64, nil)
	require.NoError(t, err)
	manifest := manifestFor(t, runtimeModule)
	program, err := CompileExecutableAgainstRuntimeFor(TargetARM64, "split.go", []byte(splitTestProgram), manifest)
	require.NoError(t, err)
	return runtimeModule, manifest, program
}

// The prebuilt module must leave the interface-method dispatchers to the program.
// They switch over the concrete types the program contains and end in a fatal
// throw, so one built from the runtime root would silently miss the program's own
// implementations.
func TestPrebuiltRuntimeLeavesTheInterfaceDispatchersToTheProgram(t *testing.T) {
	runtimeModule, _, program := buildSplit(t)

	assert.Contains(t, runtimeModule.ProgramSymbols, "error_Error")
	assert.Contains(t, runtimeModule.ProgramSymbols, "main_main")
	for _, function := range runtimeModule.Module.Funcs {
		assert.NotEqual(t, "error_Error", ir.LinkerSymbol(function.Name),
			"the prebuilt runtime module must not define an interface dispatcher")
	}

	defined := map[string]bool{}
	for _, function := range program.Module.Funcs {
		defined[ir.LinkerSymbol(function.Name)] = true
	}
	assert.True(t, defined["error_Error"], "the program module defines the dispatcher")
	assert.True(t, defined["main_main"])
}

// A type descriptor's contents depend on the program -- which of its methods are
// compiled, whether its pointer type is described -- so the whole type region is
// the program module's. Two copies would break dispatch, because cg12 compares
// descriptors by pointer.
func TestPrebuiltRuntimeLeavesTheTypeRegionToTheProgram(t *testing.T) {
	runtimeModule, _, program := buildSplit(t)

	for _, data := range runtimeModule.Module.Data {
		for _, item := range data.Items {
			assert.Empty(t, item.RelativeTo,
				"the prebuilt runtime module must hold no module-relative type offsets, but %s does", data.Name)
		}
		assert.False(t, data.GoTypeLink,
			"the prebuilt runtime module must contribute no typelinks, but %s does", data.Name)
	}

	descriptors := 0
	for _, data := range program.Module.Data {
		if data.GoTypeLink {
			descriptors++
		}
	}
	assert.Greater(t, descriptors, 100, "the program module carries the image's type descriptors")
}

// Everything the program module keeps has to resolve. A datum addressed by a
// 32-bit module-relative offset must be in this module, and a relative reference
// to a symbol the module does not define is refused by the backend rather than
// mislinked -- so the closure is checked here, where the message is useful.
func TestProgramModuleKeepsWhatItsRelativeOffsetsAddress(t *testing.T) {
	_, _, program := buildSplit(t)

	defined := map[string]bool{}
	for _, data := range program.Module.Data {
		defined[ir.LinkerSymbol(data.Name)] = true
	}
	functions := map[string]bool{}
	for _, function := range program.Module.Funcs {
		functions[ir.LinkerSymbol(function.Name)] = true
	}
	for _, data := range program.Module.Data {
		for _, item := range data.Items {
			if item.RelativeTo == "" || item.Sym == "" {
				continue
			}
			target := ir.LinkerSymbol(item.Sym)
			if functions[target] {
				continue
			}
			assert.True(t, defined[target],
				"%s addresses %s by module-relative offset, so %s must be in this module",
				data.Name, item.Sym, item.Sym)
		}
	}
}

// The split is applied to the finished whole-program module, so a symbol it keeps
// is what a monolithic build would have emitted. That is the property the whole
// design rests on, and it is cheap to check at the IR level.
func TestKeptSymbolsMatchAMonolithicBuild(t *testing.T) {
	_, _, program := buildSplit(t)
	monolithic, err := CompileExecutableFor(TargetARM64, "split.go", []byte(splitTestProgram))
	require.NoError(t, err)

	monolithicData := lastDefinitionByName(monolithic.Data)
	compared := 0
	for name, data := range lastDefinitionByName(program.Module.Data) {
		if name == ir.LinkerSymbol(ProgramModuleDataSymbol) {
			// The program module's own moduledata record has no monolithic
			// counterpart: a single-module image calls it runtime.firstmoduledata
			// and the runtime source declares it.
			continue
		}
		if sharedModuleSymbols[name] {
			// Every module has its own copy of these, and two of them are
			// deliberately shorter here than in a monolithic build: the init tasks
			// and static itabs the prebuilt module already carries are dropped, so
			// one package is not initialized twice and one itab is not registered
			// twice.
			continue
		}
		reference := monolithicData[name]
		require.NotNil(t, reference, "the program module kept %s, which a monolithic build does not emit", name)
		assert.Equal(t, dataDigest(reference), dataDigest(data), "%s differs from the monolithic build", name)
		compared++
	}
	assert.Greater(t, compared, 100)
}

// The program module subtracts almost everything: the point of the exercise is
// that the runtime is compiled once. A regression that stopped subtracting would
// still pass every behavioural test, just slowly, so the ratio is asserted.
func TestProgramModuleSubtractsTheRuntime(t *testing.T) {
	_, _, program := buildSplit(t)

	assert.Greater(t, program.SubtractedFunctions, 2000)
	assert.Less(t, program.KeptFunctions, 200,
		"the program module should hold its own code, the dispatchers and the degraded interface-call wrappers, and little else")
	assert.Greater(t, program.SubtractedData, 1000)
}

// A prebuilt module and a program that disagree about what a symbol contains
// produce an image that links cleanly and behaves as though the program's own
// definition never existed. The digest check turns that into a build error.
func TestDriftedDataIsRefused(t *testing.T) {
	runtimeModule, manifest, _ := buildSplit(t)

	drifted := ""
	for _, data := range runtimeModule.Module.Data {
		if data.Linkage.Export && len(data.Items) > 0 {
			drifted = ir.LinkerSymbol(data.Name)
			break
		}
	}
	require.NotEmpty(t, drifted)
	manifest.DataDigests[drifted] = "0000000000000000"

	_, err := CompileExecutableAgainstRuntimeFor(TargetARM64, "split.go", []byte(splitTestProgram), manifest)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "differ from the prebuilt runtime's definitions")
}

// The itablinks name is duplicated so goc does not depend on the backend's
// metadata emitter. Keep the two in step.
func TestModuleItabLinksNameMatchesGometa(t *testing.T) {
	assert.Equal(t, gometa.ModuleItabLinksName, gometaModuleItabLinksName)
}

// A prebuilt module writes runtime.unreachableMethod into a method entry whose
// function it does not compile, and it compiles less than any program linked
// against it. Such a datum is strictly poorer than the program's own, so it goes
// to the program module -- the same reason the whole type region does.
func TestDegradedItabsGoToTheProgram(t *testing.T) {
	runtimeModule, _, program := buildSplit(t)

	for _, data := range runtimeModule.Module.Data {
		for _, item := range data.Items {
			assert.NotEqual(t, "runtime.unreachableMethod", item.Sym,
				"the prebuilt runtime module kept %s, whose method entry it degraded", data.Name)
		}
	}

	degraded := 0
	for _, data := range program.Module.Data {
		for _, item := range data.Items {
			if item.Sym == "runtime.unreachableMethod" {
				degraded++
				break
			}
		}
	}
	assert.Greater(t, degraded, 0, "the program module carries the data whose methods are genuinely absent")
}
