package goc

import (
	"reflect"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
)

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
	for _, name := range []string{"main.init.0", "main.main", "runtime.schedinit"} {
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
		"internal/cpu/cpu_arm64.s":                         "internal/cpu",
		"internal/chacha8rand/chacha8_arm64.s":             "internal/chacha8rand",
		"internal/runtime/sys/dit_arm64.s":                 "internal/runtime/sys",
		"internal/runtime/syscall/linux/asm_linux_arm64.s": "internal/runtime/syscall/linux",
		"internal/runtime/atomic/atomic_arm64.s":           "internal/runtime/atomic",
		"runtime/atomic_arm64.s":                           "runtime",
		"runtime/memclr_arm64.s":                           "runtime",
		"runtime/memmove_arm64.s":                          "runtime",
	}
	if len(module.Assembly) != len(wantAssembly) {
		t.Fatalf("executable assembly files = %d, want %d", len(module.Assembly), len(wantAssembly))
	}
	for _, assembly := range module.Assembly {
		packagePath, ok := wantAssembly[assembly.Path]
		if !ok || assembly.PackagePath != packagePath || assembly.Source == "" {
			t.Errorf("invalid executable assembly source: %#v", assembly)
		}
		if assembly.Path == "internal/runtime/atomic/atomic_arm64.s" && assembly.Defines["const_offsetARM64HasATOMICS"] != 135 {
			t.Errorf("atomic go_asm.h offset = %d, want 135", assembly.Defines["const_offsetARM64HasATOMICS"])
		}
		delete(wantAssembly, assembly.Path)
	}
	for path := range wantAssembly {
		t.Errorf("executable assembly is missing %s", path)
	}

	var initTask, initTasks *ir.Data
	var arm64UseAlignedLoads *ir.Data
	for _, data := range module.Data {
		switch data.Name {
		case ".goc.module.inittask.0":
			initTask = data
		case ".goc.module.inittasks":
			initTasks = data
		case "runtime.arm64UseAlignedLoads":
			arm64UseAlignedLoads = data
		}
	}
	if arm64UseAlignedLoads == nil {
		t.Error("runtime.arm64UseAlignedLoads global is missing")
	}
	if initTask == nil || len(initTask.Items) != 2 || initTask.Items[1].Sym != "main.init.0" {
		t.Errorf("main init task = %#v", initTask)
	}
	if initTasks == nil || len(initTasks.Items) != 1 || initTasks.Items[0].Sym != ".goc.module.inittask.0" {
		t.Errorf("module init tasks = %#v", initTasks)
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
		"main.dynamic":           {0},
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

func TestRejectUnsupportedSelect(t *testing.T) {
	_, err := Compile("bad.go", []byte("package p\nfunc f() { select {} }"))
	if err == nil || !strings.Contains(err.Error(), "unsupported statement *ast.SelectStmt") {
		t.Fatalf("error = %v", err)
	}
}
