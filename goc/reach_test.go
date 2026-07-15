package goc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

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
	reachable := reachableFunctions([]*ast.FuncDecl{root}, info, pkg, loader.units, false)
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
