package main

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/internal/prebuilt"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the driver split end to end: build the Go runtime once as a
// Go module of its own, compile a program as a second module holding only the
// difference, link the two, and run the result.
//
// The interesting assertions are not "it printed the right thing". They are that
// the program's own package init ran (which needs the module chain's tail to be
// right), that its own type descriptors are the image's only copies (which is
// what keeps interface dispatch working), and that the runtime module carries no
// part of the program.

var (
	prebuiltPackOnce sync.Once
	prebuiltPackData *runtimepack.Pack
	prebuiltPackErr  error
)

// sharedPrebuiltRuntime builds the pack once for the package's tests. It costs
// about as much as one monolithic compile, which is the whole point.
func sharedPrebuiltRuntime(t *testing.T) *runtimepack.Pack {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("linux/arm64 Go runtime image")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc is required to assemble the Go runtime's Plan 9 sidecar")
	}
	prebuiltPackOnce.Do(func() {
		prebuiltPackData, prebuiltPackErr = prebuilt.BuildRuntime(goc.TargetARM64, prebuilt.Options{})
	})
	require.NoError(t, prebuiltPackErr)
	return prebuiltPackData
}

const prebuiltSplitProgram = `package main

type shape interface{ area() int }

type square struct{ side int }

func (s square) area() int { return s.side * s.side }

var initialized int

func init() { initialized = 3 }

func main() {
	var value shape = square{side: 4}
	println("init")
	println(initialized)
	area := value.area()
	println("area")
	println(area)
	defer func() { println("deferred") }()
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				println("recovered")
			}
		}()
		panic("boom")
	}()
}
`

// linkAgainstPack compiles source as a program module and links it with the pack.
func linkAgainstPack(t *testing.T, pack *runtimepack.Pack, source string) string {
	t.Helper()
	return linkAgainstPackWith(t, pack, source, prebuilt.Options{})
}

// linkAgainstPackWith is linkAgainstPack for a pack that was not built with the
// default options. Both halves of a split have to agree on them, so the caller
// passes the same options it built the pack with.
func linkAgainstPackWith(t *testing.T, pack *runtimepack.Pack, source string, options prebuilt.Options) string {
	t.Helper()
	work := t.TempDir()
	program, err := prebuilt.CompileProgram(goc.TargetARM64, "split.go", []byte(source), []*runtimepack.Manifest{&pack.Manifest}, options)
	require.NoError(t, err)

	runtimeObject := filepath.Join(work, "runtime.o")
	sidecarObject := filepath.Join(work, "runtime-sidecar.o")
	programObject := filepath.Join(work, "program.o")
	require.NoError(t, os.WriteFile(runtimeObject, pack.Object, 0o644))
	require.NoError(t, os.WriteFile(sidecarObject, pack.Sidecar, 0o644))
	require.NoError(t, os.WriteFile(programObject, program.Object, 0o644))

	executable := filepath.Join(work, "split")
	// Order matters: each module's [minpc, maxpc) has to cover a contiguous run of
	// text, so the prebuilt object and the sidecar its text ends with come first.
	arguments := []string{"-no-pie", "-o", executable, runtimeObject, sidecarObject, programObject}
	if len(program.Sidecar) > 0 {
		programSidecar := filepath.Join(work, "program-sidecar.o")
		require.NoError(t, os.WriteFile(programSidecar, program.Sidecar, 0o644))
		arguments = append(arguments, programSidecar)
	}
	link := exec.Command("cc", arguments...)
	output, err := link.CombinedOutput()
	require.NoError(t, err, "link: %s", output)
	return executable
}

func TestProgramCompiledAgainstAPrebuiltRuntimeRuns(t *testing.T) {
	pack := sharedPrebuiltRuntime(t)
	executable := linkAgainstPack(t, pack, prebuiltSplitProgram)

	output, status := runImage(t, executable)

	assert.Equal(t, 0, status, output)
	// The program's own package init is the assertion that matters. runtime.main
	// runs each module's init tasks by walking the chain and stopping at
	// runtime.lastmoduledatap, so a prebuilt module that still claimed to be the
	// chain's tail would print "init 0" here and nothing else would look wrong.
	assert.Contains(t, output, "init\n3\n")
	assert.Contains(t, output, "area\n16\n")
	assert.Contains(t, output, "recovered")
	assert.Contains(t, output, "deferred")
}

// A program is only a second module if it really is one. Read the linked image's
// moduledata records back and check the chain.
func TestTheLinkedImageCarriesTwoChainedModules(t *testing.T) {
	pack := sharedPrebuiltRuntime(t)
	executable := linkAgainstPack(t, pack, prebuiltSplitProgram)

	file, err := elf.Open(executable)
	require.NoError(t, err)
	defer file.Close()
	symbols, err := file.Symbols()
	require.NoError(t, err)
	addresses := map[string]uint64{}
	for _, symbol := range symbols {
		addresses[symbol.Name] = symbol.Value
	}
	firstModule, ok := addresses[gometa.DefaultModuleDataSymbol]
	require.True(t, ok, "the image defines the runtime module's moduledata")
	programModule, ok := addresses[ir.LinkerSymbol(goc.ProgramModuleDataSymbol)]
	require.True(t, ok, "the image defines the program module's moduledata")

	assert.Equal(t, programModule, readImageWord(t, file, firstModule+gometa.ModuleNextOffset),
		"runtime.firstmoduledata.next names the program module")
	assert.Equal(t, uint64(0), readImageWord(t, file, programModule+gometa.ModuleNextOffset),
		"the program module is the end of the chain")
	assert.Equal(t, programModule, readImageWord(t, file, addresses["runtime_lastmoduledatap"]),
		"runtime.lastmoduledatap names the chain's tail, not its head")

	// moduledata.hasmain, offset 536. The prebuilt module does not define main;
	// the program module does.
	assert.Equal(t, byte(0), readImageByte(t, file, firstModule+536))
	assert.Equal(t, byte(1), readImageByte(t, file, programModule+536))
}

// The image has one type region and it belongs to the program module. That is
// what keeps a value tagged by the runtime's code and a dispatcher built by the
// program agreeing about what type it is: cg12 compares descriptors by pointer,
// so there must only ever be one.
func TestOnlyTheProgramModuleCarriesTypeLinks(t *testing.T) {
	pack := sharedPrebuiltRuntime(t)
	executable := linkAgainstPack(t, pack, prebuiltSplitProgram)

	file, err := elf.Open(executable)
	require.NoError(t, err)
	defer file.Close()
	symbols, err := file.Symbols()
	require.NoError(t, err)
	addresses := map[string]uint64{}
	for _, symbol := range symbols {
		addresses[symbol.Name] = symbol.Value
	}
	// moduledata.typelinks is a slice header at offset 360.
	const typelinksOffset = 360
	runtimeTypeLinks := readImageWord(t, file, addresses[gometa.DefaultModuleDataSymbol]+typelinksOffset+8)
	programTypeLinks := readImageWord(t, file, addresses[ir.LinkerSymbol(goc.ProgramModuleDataSymbol)]+typelinksOffset+8)

	assert.Equal(t, uint64(0), runtimeTypeLinks, "the prebuilt runtime module describes no types")
	assert.Greater(t, programTypeLinks, uint64(100), "the program module describes the image's types")
}

// The prebuilt runtime is the whole runtime and none of any program.
func TestThePrebuiltRuntimeLeavesTheProgramsSymbolsUndefined(t *testing.T) {
	pack := sharedPrebuiltRuntime(t)

	defined := pack.Manifest.DefinedSet()
	assert.True(t, defined["runtime_mallocgc"], "the prebuilt runtime defines the runtime")
	for _, symbol := range pack.Manifest.ProgramSymbols {
		assert.False(t, defined[symbol], "%s is left for the program module, so the pack must not define it", symbol)
	}
	assert.Contains(t, pack.Manifest.ProgramSymbols, "error_Error")
	assert.Contains(t, pack.Manifest.ProgramSymbols, "main_main")
}

var (
	optimizedPackOnce sync.Once
	optimizedPackData *runtimepack.Pack
	optimizedPackErr  error
)

// sharedOptimizedPrebuiltRuntime builds the -O pack once for the package's tests.
//
// It is a second pack rather than a flag on the first because a pack records the
// options it was built with and CompileProgram refuses a program compiled with
// different ones: -O plus a pack is its own configuration, and it is the one
// nothing was covering.
func sharedOptimizedPrebuiltRuntime(t *testing.T) *runtimepack.Pack {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("linux/arm64 Go runtime image")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc is required to assemble the Go runtime's Plan 9 sidecar")
	}
	optimizedPackOnce.Do(func() {
		optimizedPackData, optimizedPackErr = prebuilt.BuildRuntime(goc.TargetARM64, prebuilt.Options{Optimize: true})
	})
	require.NoError(t, optimizedPackErr)
	return optimizedPackData
}

// prebuiltReflectProgram reaches reflect's Plan 9 assembly stubs, which is the
// only way to reach the Go functions nothing in Go calls.
//
// reflect.MakeFunc's returned closure enters reflect_makeFuncStub, and a method
// value's enters reflect_methodValueCall. Both are assembly, and both call
// reflect.moveMakeFuncArgPtrs and then reflect.callReflect or
// reflect.callMethod -- Go functions with no Go caller anywhere in the image.
const prebuiltReflectProgram = `package main

import "reflect"

type counter struct{ base int }

func (c counter) Plus(delta int) int { return c.base + delta }

func main() {
	doubled := reflect.MakeFunc(
		reflect.TypeOf(func(int) int { return 0 }),
		func(arguments []reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(int(arguments[0].Int() * 2))}
		},
	).Interface().(func(int) int)
	println("doubled")
	println(doubled(21))

	plus := reflect.ValueOf(counter{base: 10}).MethodByName("Plus").Interface().(func(int) int)
	println("plus")
	println(plus(7))
}
`

// The configuration this test covers is -O *and* a pack, because neither alone
// reproduced the defect it is here for.
//
// A Go function only Plan 9 assembly calls has no caller in the IR, so the
// export bit exportAssemblyReferencedFunctions gives it is the only thing
// keeping opt.DeadFuncElim -- which runs after the split and cannot see
// assembly -- from deleting it. finishProgramModule used to assign the
// manifest's answer over that bit, so every optimized program module lost
// reflect.callReflect, reflect.callMethod and reflect.moveMakeFuncArgPtrs, and
// with them the ABI0 wrappers arm64 emits only for functions the module still
// has. The link failed naming three symbols nothing defined.
func TestAnOptimizedProgramKeepsTheFunctionsOnlyAssemblyCalls(t *testing.T) {
	pack := sharedOptimizedPrebuiltRuntime(t)

	executable := linkAgainstPackWith(t, pack, prebuiltReflectProgram, prebuilt.Options{Optimize: true})
	output, status := runImage(t, executable)

	assert.Equal(t, 0, status, output)
	assert.Contains(t, output, "doubled\n42\n")
	assert.Contains(t, output, "plus\n17\n")
}

// A pack and a program that disagree about the compiler options are one image
// built two ways, which nothing has ever been tested in.
func TestTheDriverRefusesAnOptimizationMismatch(t *testing.T) {
	pack := sharedPrebuiltRuntime(t)
	work := t.TempDir()
	path := filepath.Join(work, "runtime.gocrt")
	require.NoError(t, pack.Write(path))
	source := filepath.Join(work, "split.go")
	require.NoError(t, os.WriteFile(source, []byte(prebuiltSplitProgram), 0o644))

	err := linkAgainstPacksForTest(t, []string{path}, source, []byte(prebuiltSplitProgram),
		filepath.Join(work, "split"), true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "-O")
}

func readImageWord(t *testing.T, file *elf.File, address uint64) uint64 {
	t.Helper()
	return binary.LittleEndian.Uint64(readImageBytes(t, file, address, 8))
}

func readImageByte(t *testing.T, file *elf.File, address uint64) byte {
	t.Helper()
	return readImageBytes(t, file, address, 1)[0]
}

func readImageBytes(t *testing.T, file *elf.File, address uint64, size int) []byte {
	t.Helper()
	for _, section := range file.Sections {
		if section.Flags&elf.SHF_ALLOC == 0 || section.Type == elf.SHT_NOBITS {
			continue
		}
		if address < section.Addr || address+uint64(size) > section.Addr+section.Size {
			continue
		}
		data, err := section.Data()
		require.NoError(t, err)
		offset := address - section.Addr
		return data[offset : offset+uint64(size)]
	}
	t.Fatalf("address %#x is not inside any loaded section of the image", address)
	return nil
}
