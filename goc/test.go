package goc

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/evanphx/cg12/ir"
)

// PackageTest describes one top-level Test function discovered in a package's
// build-selected _test.go files.
type PackageTest struct {
	Name string
}

// CompileTestExecutable compiles a package's internal tests into a normal Go
// executable linked against the cg12-compiled runtime. Test selection remains
// a runtime concern handled by the copied testing and regexp packages.
func CompileTestExecutable(packagePath string) (*ir.Module, []PackageTest, error) {
	tests, err := discoverPackageTests(packagePath)
	if err != nil {
		return nil, nil, err
	}

	source := testMainSource(packagePath, tests)
	module, err := compile("_testmain.go", []byte(source), compileOptions{
		executable:   true,
		testPackages: map[string]bool{packagePath: true},
	})
	if err != nil {
		return nil, nil, err
	}
	return module, tests, nil
}

func discoverPackageTests(packagePath string) ([]PackageTest, error) {
	fset := token.NewFileSet()
	loader := newSourceLoader(fset)
	loader.testPackages[packagePath] = true
	if _, err := loader.Import(packagePath); err != nil {
		return nil, fmt.Errorf("goc test: load %s: %w", packagePath, err)
	}
	unit := loader.units[packagePath]
	if unit == nil {
		return nil, fmt.Errorf("goc test: package %s is not available from source", packagePath)
	}

	var tests []PackageTest
	for _, file := range unit.files {
		filename := fset.Position(file.Package).Filename
		if !strings.HasSuffix(filename, "_test.go") {
			continue
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !isTestName(function.Name.Name, "Test") {
				continue
			}
			object, ok := unit.info.Defs[function.Name].(*types.Func)
			if !ok || !isTestingTSignature(object.Type()) {
				continue
			}
			tests = append(tests, PackageTest{Name: function.Name.Name})
		}
	}
	sort.Slice(tests, func(i, j int) bool {
		return tests[i].Name < tests[j].Name
	})
	return tests, nil
}

func isTestName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == "" {
		return true
	}
	first, _ := utf8.DecodeRuneInString(suffix)
	return !unicode.IsLower(first)
}

func isTestingTSignature(valueType types.Type) bool {
	signature, ok := valueType.(*types.Signature)
	if !ok || signature.Params().Len() != 1 || signature.Results().Len() != 0 {
		return false
	}
	pointer, ok := signature.Params().At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Pkg().Path() == "testing" && named.Obj().Name() == "T"
}

func testMainSource(packagePath string, tests []PackageTest) string {
	var source strings.Builder
	source.WriteString("package main\n\n")
	source.WriteString("import (\n")
	source.WriteString("\t\"regexp\"\n")
	source.WriteString("\t\"testing\"\n")
	source.WriteString("\ttestpkg ")
	source.WriteString(strconv.Quote(packagePath))
	source.WriteString("\n)\n\n")
	source.WriteString("func matchString(pattern, name string) (bool, error) {\n")
	source.WriteString("\treturn regexp.MatchString(pattern, name)\n")
	source.WriteString("}\n\n")
	source.WriteString("func main() {\n")
	source.WriteString("\ttesting.Main(matchString, []testing.InternalTest{\n")
	for _, test := range tests {
		source.WriteString("\t\t{Name: ")
		source.WriteString(strconv.Quote(test.Name))
		source.WriteString(", F: testpkg.")
		source.WriteString(test.Name)
		source.WriteString("},\n")
	}
	source.WriteString("\t}, nil, nil)\n")
	source.WriteString("}\n")
	return source.String()
}
