package goc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
)

func TestCompileLowersSyncAtomicCallsToIntrinsics(t *testing.T) {
	source := []byte(`package atomictest

import "sync/atomic"

func exercise(word *uint32, wide *uint64) {
	atomic.StoreUint32(word, 1)
	atomic.LoadUint32(word)
	atomic.SwapUint32(word, 2)
	atomic.CompareAndSwapUint32(word, 2, 3)
	atomic.AddUint32(word, 4)
	atomic.AndUint32(word, 5)
	atomic.OrUint32(word, 6)

	atomic.StoreUint64(wide, 1)
	atomic.LoadUint64(wide)
	atomic.SwapUint64(wide, 2)
	atomic.CompareAndSwapUint64(wide, 2, 3)
	atomic.AddUint64(wide, 4)
	atomic.AndUint64(wide, 5)
	atomic.OrUint64(wide, 6)
}
`)

	module, err := Compile("atomic_intrinsics.go", source)
	if err != nil {
		t.Fatal(err)
	}

	function := findCompiledFunction(t, module, "atomictest.exercise")
	counts := make(map[string]int)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op == ir.OIntrinsic {
				counts[instruction.Intrin.Name]++
			}
			if instruction.Op == ir.OCall {
				calleeReference := instruction.Arg(0)
				if calleeReference.Kind != ir.RefConst {
					continue
				}
				callee := function.Consts[calleeReference.ID]
				if callee.Kind == ir.ConstSym && strings.HasPrefix(callee.Sym, "sync/atomic.") {
					t.Errorf("sync atomic call was not lowered: %s", callee.Sym)
				}
			}
		}
	}

	for _, width := range []string{"w", "l"} {
		for _, operation := range []string{"store", "load", "xchg", "cas", "add", "and", "or"} {
			name := "atomic." + operation + "." + width
			if counts[name] != 1 {
				t.Errorf("%s count = %d, want 1", name, counts[name])
			}
		}
	}
}

func TestCompilePreservesSyncAtomicPointerWriteBarrierCalls(t *testing.T) {
	source := []byte(`package atomictest

import (
	"sync/atomic"
	"unsafe"
)

func storePointer(address *unsafe.Pointer, value unsafe.Pointer) {
	atomic.StorePointer(address, value)
}
`)

	module, err := Compile("atomic_pointer.go", source)
	if err != nil {
		t.Fatal(err)
	}

	function := findCompiledFunction(t, module, "atomictest.storePointer")
	foundBarrierCall := false
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op == ir.OIntrinsic && strings.HasPrefix(instruction.Intrin.Name, "atomic.") {
				t.Errorf("pointer store bypassed its write-barrier call with %s", instruction.Intrin.Name)
			}
			if instruction.Op != ir.OCall {
				continue
			}
			calleeReference := instruction.Arg(0)
			if calleeReference.Kind != ir.RefConst {
				continue
			}
			callee := function.Consts[calleeReference.ID]
			if callee.Kind == ir.ConstSym && callee.Sym == "sync/atomic.StorePointer" {
				foundBarrierCall = true
			}
		}
	}
	if !foundBarrierCall {
		t.Fatal("sync/atomic.StorePointer write-barrier call was not preserved")
	}
}

func findCompiledFunction(t *testing.T, module *ir.Module, name string) *ir.Func {
	t.Helper()
	for _, function := range module.Funcs {
		if function.Name == name {
			return function
		}
	}
	t.Fatalf("compiled module does not contain %s", name)
	return nil
}

func TestRuntimeTypeKeyCanonicalizesAliasesInGenericArguments(t *testing.T) {
	netipPackage := types.NewPackage("net/netip", "netip")
	addressDetailName := types.NewTypeName(token.NoPos, netipPackage, "addrDetail", nil)
	addressDetail := types.NewNamed(addressDetailName, types.NewStruct(nil, nil), nil)
	aliasName := types.NewTypeName(token.NoPos, netipPackage, "AddrDetail", nil)
	addressDetailAlias := types.NewAlias(aliasName, addressDetail)

	uniquePackage := types.NewPackage("unique", "unique")
	typeParameterName := types.NewTypeName(token.NoPos, uniquePackage, "T", nil)
	typeParameter := types.NewTypeParam(typeParameterName, types.NewInterfaceType(nil, nil).Complete())
	mapName := types.NewTypeName(token.NoPos, uniquePackage, "uniqueMap", nil)
	mapType := types.NewNamed(mapName, types.NewStruct(nil, nil), nil)
	mapType.SetTypeParams([]*types.TypeParam{typeParameter})

	aliasInstance, err := types.Instantiate(nil, mapType, []types.Type{addressDetailAlias}, true)
	if err != nil {
		t.Fatal(err)
	}
	concreteInstance, err := types.Instantiate(nil, mapType, []types.Type{addressDetail}, true)
	if err != nil {
		t.Fatal(err)
	}

	aliasPointer := types.NewPointer(aliasInstance)
	concretePointer := types.NewPointer(concreteInstance)
	if got, want := runtimeTypeKey(aliasPointer), runtimeTypeKey(concretePointer); got != want {
		t.Fatalf("runtime type keys differ: %q != %q", got, want)
	}
}

func TestTypeKeysDistinguishLocalNamedTypes(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "local_types.go", `package sample

type T struct {
	PackageField int
}

func first() {
	type T struct {
		FirstField int
	}
	_ = T{}
}

func second() {
	type T struct {
		SecondField int
	}
	_ = T{}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}

	info := &types.Info{Defs: make(map[*ast.Ident]types.Object)}
	configuration := types.Config{}
	checkedPackage, err := configuration.Check("sample", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}

	packageType := checkedPackage.Scope().Lookup("T").Type()
	localTypes := make([]types.Type, 0, 2)
	for identifier, object := range info.Defs {
		if identifier.Name != "T" {
			continue
		}
		typeName, ok := object.(*types.TypeName)
		if !ok || typeName.Parent() == checkedPackage.Scope() {
			continue
		}
		localTypes = append(localTypes, typeName.Type())
	}
	if len(localTypes) != 2 {
		t.Fatalf("local T declarations = %d, want 2", len(localTypes))
	}

	keys := map[string]bool{
		runtimeTypeKey(types.NewPointer(packageType)): true,
		goTypeKey(types.NewSlice(packageType)):        true,
	}
	for _, localType := range localTypes {
		keys[runtimeTypeKey(types.NewPointer(localType))] = true
		keys[goTypeKey(types.NewSlice(localType))] = true
	}
	if len(keys) != 6 {
		t.Fatalf("type keys collapsed distinct local declarations: %#v", keys)
	}
}

func TestAssemblyPackageDefinesIncludeConstantsAndStructLayouts(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "layout.go", `package layout
const answer = 42
type inner struct {
	byte byte
	word uintptr
}
type outer struct {
	flag bool
	item inner
	ptr *outer
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	sizes := types.SizesFor("gc", runtime.GOARCH)
	configuration := types.Config{Sizes: sizes}
	pkg, err := configuration.Check("layout", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}

	defines := assemblyPackageDefines(pkg)
	outer := pkg.Scope().Lookup("outer").Type().Underlying().(*types.Struct)
	fields := make([]*types.Var, outer.NumFields())
	for index := range fields {
		fields[index] = outer.Field(index)
	}
	offsets := sizes.Offsetsof(fields)

	if defines["const_answer"] != 42 {
		t.Errorf("const_answer = %d, want 42", defines["const_answer"])
	}
	if defines["outer_flag"] != offsets[0] {
		t.Errorf("outer_flag = %d, want %d", defines["outer_flag"], offsets[0])
	}
	if defines["outer_item"] != offsets[1] {
		t.Errorf("outer_item = %d, want %d", defines["outer_item"], offsets[1])
	}
	if defines["outer_ptr"] != offsets[2] {
		t.Errorf("outer_ptr = %d, want %d", defines["outer_ptr"], offsets[2])
	}
	if defines["outer__size"] != sizes.Sizeof(pkg.Scope().Lookup("outer").Type()) {
		t.Errorf("outer__size = %d, want %d", defines["outer__size"], sizes.Sizeof(pkg.Scope().Lookup("outer").Type()))
	}
}

func TestCompileCoreGo(t *testing.T) {
	m, err := Compile("sum.go", []byte(`package main
func sum(n int64) int64 { s := int64(0); for i := int64(1); i <= n; i++ { if i == 3 { continue }; s += i }; return s }
func main() { if sum(5) != 12 { for { break } } }
`))
	if err != nil {
		t.Fatal(err)
	}
	s := m.String()
	for _, want := range []string{"function l $main.sum", "$main", "jnz", "add"} {
		if !strings.Contains(s, want) {
			t.Errorf("IR missing %q:\n%s", want, s)
		}
	}
}

func TestCompileGeneratesWrapperForPromotedInterfaceMultiResultMethod(t *testing.T) {
	module, err := Compile("promoted.go", []byte(`package main

type scanner interface {
	Read() (int, int, error)
}

type implementation struct{}

func (implementation) Read() (int, int, error) {
	return 17, 25, nil
}

type reader struct {
	scanner
}

func Test() int {
	value := reader{scanner: implementation{}}
	left, right, err := value.Read()
	if err != nil {
		return 0
	}
	return left + right
}
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, function := range module.Funcs {
		if function.Name == "main.scanner.Read" {
			return
		}
	}
	t.Fatal("promoted interface method wrapper main.scanner.Read was not generated")
}

func TestStaticInitializerIgnoresKeyedStructFieldNames(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "initializer.go", `package initializer
type debugVars struct {
	enabled int32
}
type debugVar struct {
	name  string
	value *int32
}
var debug debugVars
var dbgvars = []*debugVar{
	{name: "enabled", value: &debug.enabled},
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}

	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	configuration := types.Config{Sizes: types.SizesFor("gc", runtime.GOARCH)}
	if _, err := configuration.Check("initializer", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}

	declaration := file.Decls[3].(*ast.GenDecl)
	specification := declaration.Specs[0].(*ast.ValueSpec)
	if !staticallyInitialized(specification.Values[0], info) {
		t.Fatal("global pointer table should be representable as static data")
	}
}

func TestStaticInitializerRejectsMapLiteral(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "initializer.go", `package initializer
type holder struct {
	values map[string]int
}
var global = holder{values: map[string]int{"answer": 42}}
`, 0)
	if err != nil {
		t.Fatal(err)
	}

	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	configuration := types.Config{Sizes: types.SizesFor("gc", runtime.GOARCH)}
	if _, err := configuration.Check("initializer", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}

	declaration := file.Decls[1].(*ast.GenDecl)
	specification := declaration.Specs[0].(*ast.ValueSpec)
	if staticallyInitialized(specification.Values[0], info) {
		t.Fatal("map-containing composite requires dynamic initialization")
	}
}

func TestStaticNonEmptyInterfaceKeepsImplementationMethodReachable(t *testing.T) {
	module, err := CompileExecutable("interface.go", []byte(`package main

type stringError string

func (value stringError) Error() string {
	return string(value)
}

var staticError error = error(stringError("failure"))

func main() {}
`))
	if err != nil {
		t.Fatal(err)
	}
	foundMethod := false
	for _, function := range module.Funcs {
		if strings.HasSuffix(function.Name, "stringError.Error") {
			foundMethod = true
			break
		}
	}
	if !foundMethod {
		t.Fatal("static interface implementation method is not reachable")
	}

	foundItab := false
	for _, data := range module.Data {
		if strings.HasPrefix(data.Name, ".goc.itab.") {
			foundItab = true
			break
		}
	}
	if !foundItab {
		t.Fatal("static non-empty interface has no itab")
	}
}

func TestCompileAllowsExternalLinknameDeclaration(t *testing.T) {
	module, err := Compile("linkname.go", []byte(`package main
import _ "unsafe"
//go:linkname external runtime.external
func external(value int) int
func main() { _ = external(42) }
`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(module.String(), "$runtime.external") {
		t.Fatalf("IR does not call the external linkname:\n%s", module)
	}
}

func TestCompilePreservesNoSplitDirective(t *testing.T) {
	module, err := Compile("nosplit.go", []byte(`package main

//go:nosplit
func helper() {}
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, function := range module.Funcs {
		if function.Name == "main.helper" {
			if !function.NoSplit {
				t.Fatal("main.helper did not preserve //go:nosplit")
			}
			return
		}
	}
	t.Fatal("main.helper was not compiled")
}

func TestCompilePreservesSystemStackDirective(t *testing.T) {
	module, err := Compile("systemstack.go", []byte(`package main

//go:systemstack
func helper() {}
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, function := range module.Funcs {
		if function.Name == "main.helper" {
			if !function.SystemStack {
				t.Fatal("main.helper did not preserve //go:systemstack")
			}
			return
		}
	}
	t.Fatal("main.helper was not compiled")
}

func TestCompileKeepsAssignedNonEscapingAddressOnStack(t *testing.T) {
	module, err := Compile("address.go", []byte(`package main
type pair struct { left, right int }
func sum(value *pair) int { return value.left + value.right }
func Test() int {
	value := &pair{left: 17, right: 25}
	return sum(value)
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(module.String(), "$calloc") {
		t.Fatalf("non-escaping address was heap allocated:\n%s", module)
	}
}

func TestCompileExecutableIncludesRuntimeAndMainInitTask(t *testing.T) {
	module, err := CompileExecutable("main.go", []byte(`package main
var initialized int
func init() { initialized = 41 }
func main() {
	if initialized != 41 {
		panic("init did not run")
	}
}
`))
	if err != nil {
		t.Fatal(err)
	}

	functions := make(map[string]bool)
	for _, function := range module.Funcs {
		functions[function.Name] = true
	}
	for _, name := range []string{
		"main.init.0",
		"main.main",
		"runtime.schedinit",
		"runtime.sigprofNonGo",
		"runtime.sigtrampgo",
	} {
		if !functions[name] {
			t.Errorf("executable module is missing %s", name)
		}
	}
	wantAssembly := map[string]string{
		"internal/bytealg/compare_arm64.s":                 "internal/bytealg",
		"internal/bytealg/count_arm64.s":                   "internal/bytealg",
		"internal/bytealg/equal_arm64.s":                   "internal/bytealg",
		"internal/bytealg/index_arm64.s":                   "internal/bytealg",
		"internal/bytealg/indexbyte_arm64.s":               "internal/bytealg",
		"internal/abi/abi_test.s":                          "internal/abi",
		"internal/abi/stub.s":                              "internal/abi",
		"internal/cpu/cpu.s":                               "internal/cpu",
		"internal/cpu/cpu_arm64.s":                         "internal/cpu",
		"internal/chacha8rand/chacha8_arm64.s":             "internal/chacha8rand",
		"internal/runtime/sys/dit_arm64.s":                 "internal/runtime/sys",
		"internal/runtime/sys/empty.s":                     "internal/runtime/sys",
		"internal/runtime/syscall/linux/asm_linux_arm64.s": "internal/runtime/syscall/linux",
		"internal/runtime/atomic/atomic_arm64.s":           "internal/runtime/atomic",
		"runtime/asm.s":                                    "runtime",
		"runtime/asm_arm64.s":                              "runtime",
		"runtime/atomic_arm64.s":                           "runtime",
		"runtime/ints.s":                                   "runtime",
		"runtime/memclr_arm64.s":                           "runtime",
		"runtime/memmove_arm64.s":                          "runtime",
		"runtime/preempt_arm64.s":                          "runtime",
		"runtime/rt0_linux_arm64.s":                        "runtime",
		"runtime/secret_arm64.s":                           "runtime",
		"runtime/sys_linux_arm64.s":                        "runtime",
		"runtime/tls_arm64.s":                              "runtime",
	}
	if len(module.Assembly) != len(wantAssembly) {
		t.Fatalf("executable assembly files = %d, want %d", len(module.Assembly), len(wantAssembly))
	}
	var runtimeDefines map[string]int64
	for _, assembly := range module.Assembly {
		packagePath, ok := wantAssembly[assembly.Path]
		if !ok || assembly.PackagePath != packagePath || assembly.Source == "" {
			t.Errorf("invalid executable assembly source: %#v", assembly)
		}
		if assembly.Path == "internal/runtime/atomic/atomic_arm64.s" && assembly.Defines["const_offsetARM64HasATOMICS"] != 135 {
			t.Errorf("atomic go_asm.h offset = %d, want 135", assembly.Defines["const_offsetARM64HasATOMICS"])
		}
		if assembly.PackagePath == "runtime" {
			runtimeDefines = assembly.Defines
		}
		if assembly.Path == "runtime/tls_arm64.s" && assembly.Includes["tls_arm64.h"] == "" {
			t.Error("runtime tls_arm64.s is missing its retained tls_arm64.h include")
		}
		delete(wantAssembly, assembly.Path)
	}
	for path := range wantAssembly {
		t.Errorf("executable assembly is missing %s", path)
	}
	wantRuntimeDefines := map[string]int64{
		"g_m":              48,
		"m_p":              208,
		"p_xRegs":          13800,
		"xRegPerP_scratch": 0,
		"xRegPerP_cache":   512,
	}
	for name, want := range wantRuntimeDefines {
		if got := runtimeDefines[name]; got != want {
			t.Errorf("runtime assembly define %s = %d, want %d", name, got, want)
		}
	}

	var initTask, initTasks *ir.Data
	var arm64UseAlignedLoads *ir.Data
	var cgoCallers *ir.Data
	for _, data := range module.Data {
		switch data.Name {
		case ".goc.module.inittask.0":
			initTask = data
		case ".goc.module.inittasks":
			initTasks = data
		case "runtime.arm64UseAlignedLoads":
			arm64UseAlignedLoads = data
		case "_cgo_callers":
			cgoCallers = data
		}
	}
	if arm64UseAlignedLoads == nil {
		t.Error("runtime.arm64UseAlignedLoads global is missing")
	}
	if cgoCallers == nil || !cgoCallers.Linkage.Export {
		t.Error("runtime _cgo_callers linkname global is missing or local")
	}
	if initTask == nil || len(initTask.Items) != 2 || initTask.Items[1].Sym != "main.init.0" {
		t.Errorf("main init task = %#v", initTask)
	}
	if initTasks == nil || len(initTasks.Items) != 1 || initTasks.Items[0].Sym != ".goc.module.inittask.0" {
		t.Errorf("module init tasks = %#v", initTasks)
	}
}

func TestCompileExecutableUsesExactRuntimeFIPSIndicator(t *testing.T) {
	source := `package main

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
)

func main() {
	_ = md5.Sum(nil)
	_ = sha1.Sum(nil)
	_ = sha256.Sum256(nil)
}
`
	module, err := CompileExecutable("main.go", []byte(source))
	if err != nil {
		t.Fatal(err)
	}

	for _, symbol := range []string{
		"crypto/internal/fips140.getIndicator",
		"crypto/internal/fips140.setIndicator",
	} {
		count := 0
		usesRuntimeG := false
		for _, function := range module.Funcs {
			if function.Name == symbol {
				count++
				if !function.Linkage.Export {
					t.Errorf("%s is not exported", symbol)
				}
				for _, block := range function.Blocks {
					for _, instruction := range block.Instrs {
						usesRuntimeG = usesRuntimeG || instruction.Op == ir.OGetReg
					}
				}
			}
		}
		if count != 1 {
			t.Errorf("%s definitions = %d, want 1", symbol, count)
		}
		if !usesRuntimeG {
			t.Errorf("%s does not use the exact runtime per-g implementation", symbol)
		}
	}

	for _, symbol := range []string{
		"crypto/internal/fips140only.Enforced",
		"crypto/internal/boring.Unreachable",
	} {
		count := 0
		for _, function := range module.Funcs {
			if function.Name == symbol {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s definitions = %d, want the exact runtime definition only", symbol, count)
		}
	}
}

func TestCompileExecutableIncludesReachableSyscallAssembly(t *testing.T) {
	module, err := CompileExecutable("main.go", []byte(`package main
import "syscall"
func main() {
	if syscall.Getpid() <= 0 {
		panic("getpid returned an invalid process ID")
	}
}
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, assembly := range module.Assembly {
		if assembly.Path == "syscall/asm_linux_arm64.s" && assembly.PackagePath == "syscall" && assembly.Source != "" {
			return
		}
	}
	t.Fatal("reachable syscall/asm_linux_arm64.s was not retained")
}

func TestCompileExecutableUsesAggregateABIForInterfaceResults(t *testing.T) {
	module, err := CompileExecutable("main.go", []byte(`package main
import "sync"
var values sync.Map
func main() {
	values.LoadOrStore("key", 42)
}
`))
	if err != nil {
		t.Fatal(err)
	}

	foundConcrete := false
	foundGeneric := false
	for _, function := range module.Funcs {
		switch function.Name {
		case "sync.Map.LoadOrStore":
			foundConcrete = true
			if function.RetAgg == nil || len(function.RetAgg.Fields) != 2 {
				t.Fatalf("sync.Map.LoadOrStore aggregate result = %#v", function.RetAgg)
			}
		case "internal/sync.HashTrieMap.LoadOrStore":
			foundGeneric = true
			if function.RetAgg != nil {
				t.Fatalf("generic HashTrieMap.LoadOrStore aggregate result = %#v, want scalar descriptor", function.RetAgg)
			}
		}
	}
	if !foundConcrete {
		t.Error("sync.Map.LoadOrStore was not compiled")
	}
	if !foundGeneric {
		t.Error("internal/sync.HashTrieMap.LoadOrStore was not compiled")
	}
}

func TestCompileExecutableRepresentsSlicesAsGroupedScalarValues(t *testing.T) {
	module, err := CompileExecutable("main.go", []byte(`package main
func shorten(values []int) { values = values[:1] }
func tail(values []int) []int { return values[1:] }
func main() {
	values := []int{1, 2, 3}
	shorten(values)
	_ = tail(values)
}
`))
	if err != nil {
		t.Fatal(err)
	}

	var shorten *ir.Func
	var tail *ir.Func
	foundGroupedCall := false
	for _, function := range module.Funcs {
		switch function.Name {
		case "main.shorten":
			shorten = function
		case "main.tail":
			tail = function
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				for _, argument := range instruction.Args {
					if argument.Kind == ir.RefAggregate {
						t.Fatalf("aggregate bundle escaped into %s instruction %s", function.Name, instruction.Op)
					}
				}
				if instruction.Op != ir.OCall || len(instruction.Args) == 0 || instruction.Args[0].Kind != ir.RefConst {
					continue
				}
				callee := function.Consts[instruction.Args[0].ID]
				if callee.Kind == ir.ConstSym && callee.Sym == "main.shorten" {
					foundGroupedCall = len(instruction.ArgGroups) == 1 && instruction.ArgGroups[0].Count == 3
				}
			}
		}
	}

	if shorten == nil {
		t.Fatal("main.shorten was not compiled")
	}
	if len(shorten.Params) != 3 || len(shorten.ParamGroups) != 1 || shorten.ParamGroups[0].Count != 3 {
		t.Fatalf("shorten parameters = %d, groups = %#v", len(shorten.Params), shorten.ParamGroups)
	}
	if shorten.Params[0].Cls != ir.ClsP || shorten.Params[1].Cls != ir.ClsL || shorten.Params[2].Cls != ir.ClsL {
		t.Fatalf("shorten parameter classes = %s, %s, %s", shorten.Params[0].Cls, shorten.Params[1].Cls, shorten.Params[2].Cls)
	}
	if tail == nil || !tail.RetValues || tail.RetAgg == nil {
		t.Fatalf("tail aggregate result = %#v, value form = %v", tail, tail != nil && tail.RetValues)
	}
	if tail.RetAgg != shorten.ParamGroups[0].Type {
		t.Error("slice ABI metadata was not canonicalized to one aggregate type")
	}
	foundValueReturn := false
	for _, block := range tail.Blocks {
		if block.Jmp.Kind == ir.JmpRet && !block.Jmp.Arg.IsNone() && len(block.Jmp.Args) == 2 {
			foundValueReturn = true
		}
	}
	if !foundValueReturn {
		t.Error("tail has no three-part slice return")
	}
	if !foundGroupedCall {
		t.Error("call to main.shorten does not carry a three-part aggregate group")
	}
}

func TestCompileExecutableStoresGlobalSliceHeaderInline(t *testing.T) {
	module, err := CompileExecutable("main.go", []byte(`package main
var values = []int{7, 11, 13}
func main() { _ = values }
`))
	if err != nil {
		t.Fatal(err)
	}

	var global *ir.Data
	for _, data := range module.Data {
		if data.Name == "main.values" {
			global = data
		}
		if data.Name == "main.values.descriptor" {
			t.Fatal("global slice still has a separate descriptor object")
		}
	}
	if global == nil {
		t.Fatal("main.values was not emitted")
	}
	if !reflect.DeepEqual(global.PointerWords, []int{0}) {
		t.Fatalf("main.values pointer words = %v, want [0]", global.PointerWords)
	}
	if len(global.Items) != 2 || global.Items[0].Sym != "main.values.backing" || len(global.Items[1].Ints) != 2 {
		t.Fatalf("main.values data = %#v, want inline data/length/capacity header", global.Items)
	}
}

func TestCompileExecutableDynamicallyInitializesNestedByteSlice(t *testing.T) {
	module, err := CompileExecutable("main.go", []byte(`package main
type result struct {
	line []byte
}
type testCase struct {
	expect []result
}
var tests = []testCase{{expect: []result{{line: []byte("expected")}}}}
func main() { _ = tests[0].expect[0].line }
`))
	if err != nil {
		t.Fatal(err)
	}

	foundInitializer := false
	heapAllocations := 0
	for _, function := range module.Funcs {
		if strings.Contains(function.Name, "global.initfunc") && strings.HasSuffix(function.Name, ".main.tests") {
			foundInitializer = true
			for _, block := range function.Blocks {
				for _, instruction := range block.Instrs {
					if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
						continue
					}
					calleeReference := instruction.Arg(0)
					if calleeReference.Kind != ir.RefConst {
						continue
					}
					callee := function.Consts[calleeReference.ID]
					if callee.Kind == ir.ConstSym && callee.Sym == "runtime.newobject" {
						heapAllocations++
					}
				}
			}
		}
	}
	if !foundInitializer {
		t.Fatal("main.tests has no dynamic initializer")
	}
	if heapAllocations < 2 {
		t.Fatalf("main.tests initializer has %d heap allocations, want at least 2 for the outer and nested backing arrays", heapAllocations)
	}
}

func TestCompileExecutableKeepsInlinedFloatComparisonsTyped(t *testing.T) {
	module, err := CompileExecutable("main.go", []byte(`package main
import "fmt"
func main() {
	_ = fmt.Sprintf("%v", 42)
}
`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, function := range module.Funcs {
		if function.Name != "internal/fmtsort.compare" {
			continue
		}
		found = true
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				if instruction.Op != ir.OCmp || len(instruction.Args) == 0 {
					continue
				}
				operandClass := function.ClassOf(instruction.Args[0])
				if operandClass.IsFloat() && !instruction.Cmp.IsFloat() {
					t.Fatalf("float operands use comparison predicate %v in block %s", instruction.Cmp, block.Name)
				}
			}
		}
	}
	if !found {
		t.Fatal("internal/fmtsort.compare was not compiled")
	}
}

func TestCompileRecordsExactGlobalPointerWords(t *testing.T) {
	m, err := Compile("globals.go", []byte(`package main
type node struct {
	next *node
	value int
	tail *node
}
var root node
var roots [2]*node
var pointer *node
var text string
var dynamic any
var values []int
`))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]int{
		"main.root":              {0, 2},
		"main.roots":             {0, 1},
		"main.pointer":           {0},
		"main.text":              {0},
		"main.text.descriptor":   {0},
		"main.dynamic":           {0, 1},
		"main.values":            {0},
		"main.values.descriptor": {0},
	}
	for _, data := range m.Data {
		words, ok := want[data.Name]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(words, data.PointerWords) {
			t.Errorf("%s pointer words = %v, want %v", data.Name, data.PointerWords, words)
		}
		delete(want, data.Name)
	}
	for name := range want {
		t.Errorf("missing data definition %s", name)
	}
}

func TestSharedTypeParameterUsesOnePointerWord(t *testing.T) {
	constraint := types.NewInterfaceType(nil, nil)
	constraint.Complete()
	name := types.NewTypeName(token.NoPos, nil, "T", nil)
	typeParameter := types.NewTypeParam(name, constraint)

	var offsets []int64
	visitPointerWords(typeParameter, 24, func(offset int64) {
		offsets = append(offsets, offset)
	})

	if !reflect.DeepEqual(offsets, []int64{24}) {
		t.Fatalf("shared type parameter pointer offsets = %v, want [24]", offsets)
	}
}

func TestRejectUnsupportedSelect(t *testing.T) {
	_, err := Compile("bad.go", []byte("package p\nfunc f() { select {} }"))
	if err == nil || !strings.Contains(err.Error(), "select with no communication cases") {
		t.Fatalf("error = %v", err)
	}
}
