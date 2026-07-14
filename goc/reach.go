package goc

import (
	"go/ast"
	"go/types"
)

type functionDecl struct {
	decl *ast.FuncDecl
	info *types.Info
	pkg  *types.Package
}

// reachableFunctions follows statically named function calls across source
// units. Calls through interfaces are recorded by their interface method and
// resolved later by interface lowering.
func reachableFunctions(roots []*ast.FuncDecl, rootInfo *types.Info, rootPkg *types.Package, units map[string]*sourceUnit) []functionDecl {
	declarations := make(map[*types.Func]functionDecl)
	methods := make(map[string][]functionDecl)
	for _, unit := range units {
		for _, file := range unit.files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Body == nil {
					continue
				}
				object, ok := unit.info.Defs[function.Name].(*types.Func)
				if !ok {
					continue
				}
				declarations[object] = functionDecl{decl: function, info: unit.info, pkg: unit.pkg}
				signature := object.Type().(*types.Signature)
				if signature.Recv() != nil {
					methods[object.Name()] = append(methods[object.Name()], declarations[object])
				}
			}
		}
	}

	var queue []functionDecl
	for _, root := range roots {
		queue = append(queue, functionDecl{decl: root, info: rootInfo, pkg: rootPkg})
	}
	seen := make(map[*ast.FuncDecl]bool)
	var reachable []functionDecl
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current.decl] {
			continue
		}
		seen[current.decl] = true
		reachable = append(reachable, current)

		ast.Inspect(current.decl.Body, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			object, ok := current.info.Uses[identifier].(*types.Func)
			if !ok {
				return true
			}
			if declaration, ok := declarations[object]; ok {
				queue = append(queue, declaration)
			} else if candidates := methods[object.Name()]; len(candidates) == 1 {
				queue = append(queue, candidates[0])
			}
			return true
		})
	}
	return reachable
}
