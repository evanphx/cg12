package goc

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/evanphx/cg12/ir"
)

func TestRuntimeInitDeclarationsHaveUniqueOrderedSymbols(t *testing.T) {
	fset := token.NewFileSet()
	loader := newSourceLoader(fset)
	if _, err := loader.Import("runtime"); err != nil {
		t.Fatal(err)
	}

	declarations, symbols := runtimeInitDeclarations(loader.units)
	if len(declarations) == 0 {
		t.Fatal("runtime has no init declarations")
	}
	seen := make(map[string]bool)
	for index, declaration := range declarations {
		object, ok := declaration.info.Defs[declaration.decl.Name].(*types.Func)
		if !ok {
			t.Fatalf("runtime init %d has no function object", index)
		}
		if object.Type().(*types.Signature).Recv() != nil {
			t.Errorf("runtime init %d is a method", index)
		}
		want := fmt.Sprintf("runtime.init.%d", index)
		if symbols[object] != want {
			t.Errorf("runtime init %d symbol = %q, want %q", index, symbols[object], want)
		}
		if seen[symbols[object]] {
			t.Errorf("duplicate runtime init symbol %q", symbols[object])
		}
		seen[symbols[object]] = true
		position := fset.Position(declaration.decl.Pos())
		t.Logf("%s = %s:%d", symbols[object], filepath.Base(position.Filename), position.Line)
	}
}

func TestRuntimeInitTaskContainsEveryPackageInitializer(t *testing.T) {
	fset := token.NewFileSet()
	loader := newSourceLoader(fset)
	if _, err := loader.Import("runtime"); err != nil {
		t.Fatal(err)
	}

	declarations, symbols := runtimeInitDeclarations(loader.units)
	module := ir.NewModule()
	descriptor := &ir.Data{Name: "runtime.runtime_inittasks"}
	module.Data = append(module.Data, descriptor)
	if err := addRuntimeInitTask(module, declarations, symbols); err != nil {
		t.Fatal(err)
	}

	task := module.Data[1]
	if task.Name != ".goc.runtime.inittask.runtime" {
		t.Fatalf("task name = %q", task.Name)
	}
	if !reflect.DeepEqual(task.Items[0].Ints, []int64{0, int64(len(declarations))}) {
		t.Errorf("task header = %v", task.Items[0].Ints)
	}
	for index, declaration := range declarations {
		object := declaration.info.Defs[declaration.decl.Name].(*types.Func)
		if task.Items[index+1].Sym != symbols[object] {
			t.Errorf("task function %d = %q, want %q", index, task.Items[index+1].Sym, symbols[object])
		}
	}

	backing := module.Data[2]
	if backing.Name != ".goc.runtime.inittasks.backing" || !reflect.DeepEqual(backing.PointerWords, []int{0}) {
		t.Errorf("runtime init backing = %#v", backing)
	}
	if len(descriptor.Items) != 2 || descriptor.Items[0].Sym != backing.Name || !reflect.DeepEqual(descriptor.Items[1].Ints, []int64{1, 1}) {
		t.Errorf("runtime init slice descriptor = %#v", descriptor.Items)
	}
}

func TestSHA256ReachabilityUsesExactSource(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", `package main
import "crypto/sha256"
func Test() byte { return sha256.Sum256(nil)[0] }
`, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	loader := newSourceLoader(fset)
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	pkg, err := (&types.Config{Importer: loader}).Check("main", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}
	root := file.Decls[1].(*ast.FuncDecl)
	dynamicTypes := collectDynamicTypes(info, loader.units)
	reachable, _ := reachableFunctions([]*ast.FuncDecl{root}, []*ast.File{file}, info, pkg, loader.units, dynamicTypes, false, nil, nil, nil)
	names := make(map[string]bool)
	for _, function := range reachable {
		names[function.pkg.Path()+"."+function.decl.Name.Name] = true
	}
	for _, want := range []string{
		"main.Test",
		"crypto/sha256.Sum256",
		"crypto/sha256.New",
		"crypto/internal/fips140/sha256.New",
	} {
		if !names[want] {
			t.Errorf("reachable functions do not contain %s", want)
		}
	}
}

func TestTypeSwitchInterfaceCaseMakesConcreteMethodsReachable(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", `package main

import (
	"fmt"
	"io/fs"
)

func Test() string {
	return fmt.Sprintf("%v", fs.FileMode(0))
}
`, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	loader := newSourceLoader(fset)
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Instances:  make(map[*ast.Ident]types.Instance),
	}
	pkg, err := (&types.Config{Importer: loader}).Check("main", fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatal(err)
	}
	root := file.Decls[1].(*ast.FuncDecl)
	dynamicTypes := collectDynamicTypes(info, loader.units)
	reachable, _ := reachableFunctions(
		[]*ast.FuncDecl{root},
		[]*ast.File{file},
		info,
		pkg,
		loader.units,
		dynamicTypes,
		false,
		nil,
		nil,
		nil,
	)

	for _, declaration := range reachable {
		if declaration.pkg.Path() == "io/fs" && declaration.decl.Name.Name == "String" {
			return
		}
	}
	t.Fatal("io/fs.FileMode.String was not reachable through fmt's stringer type-switch case")
}
