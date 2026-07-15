package goc

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

type functionDecl struct {
	decl *ast.FuncDecl
	info *types.Info
	pkg  *types.Package
}

// reachableFunctions follows statically named function calls across source
// units. Calls through interfaces are recorded by their interface method and
// resolved later by interface lowering.
func reachableFunctions(roots []*ast.FuncDecl, rootInfo *types.Info, rootPkg *types.Package, units map[string]*sourceUnit, runtimeAllocation bool) []functionDecl {
	declarations := make(map[*types.Func]functionDecl)
	methods := make(map[string][]functionDecl)
	runtimeFunctions := make(map[string]functionDecl)
	var genericRuntimeMethods []functionDecl
	var runtimeSupportFunctions []functionDecl
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
				if unit.path == "internal/chacha8rand" && object.Name() == "block_generic" {
					runtimeSupportFunctions = append(runtimeSupportFunctions, declarations[object])
				}
				signature := object.Type().(*types.Signature)
				if unit.path == "runtime" && signature.Recv() == nil {
					runtimeFunctions[object.Name()] = declarations[object]
				}
				if signature.Recv() != nil {
					methods[object.Name()] = append(methods[object.Name()], declarations[object])
					if unit.path == "internal/runtime/atomic" || unit.path == "internal/runtime/gc/scan" {
						receiver := types.TypeString(signature.Recv().Type(), nil)
						if strings.Contains(receiver, "[") {
							genericRuntimeMethods = append(genericRuntimeMethods, declarations[object])
						}
					}
				}
			}
		}
	}

	var queue []functionDecl
	for _, root := range roots {
		queue = append(queue, functionDecl{decl: root, info: rootInfo, pkg: rootPkg})
	}
	if runtimeAllocation {
		queue = append(queue, genericRuntimeMethods...)
		queue = append(queue, runtimeSupportFunctions...)
		for _, name := range []string{"args", "check", "growslice", "osinit", "schedinit"} {
			if declaration, exists := runtimeFunctions[name]; exists {
				queue = append(queue, declaration)
			}
		}
		for name, declaration := range runtimeFunctions {
			if name == "mallocPanic" || strings.HasPrefix(name, "mallocgcSmall") || strings.HasPrefix(name, "mallocgcTiny") || hasRuntimeMapsLinkName(declaration.decl) {
				queue = append(queue, declaration)
			}
		}
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
			if runtimeAllocation {
				if expression, ok := node.(*ast.UnaryExpr); ok && expression.Op == token.AND {
					if _, composite := expression.X.(*ast.CompositeLit); composite {
						if newobject, exists := runtimeFunctions["newobject"]; exists {
							queue = append(queue, newobject)
						}
					}
				}
			}
			if _, ok := node.(*ast.GoStmt); ok {
				if newproc, exists := runtimeFunctions["newproc"]; exists {
					queue = append(queue, newproc)
				}
			}
			if expression, ok := node.(*ast.UnaryExpr); ok && expression.Op == token.ARROW {
				if chanrecv, exists := runtimeFunctions["chanrecv1"]; exists {
					queue = append(queue, chanrecv)
				}
			}
			if _, ok := node.(*ast.SendStmt); ok {
				if chansend, exists := runtimeFunctions["chansend1"]; exists {
					queue = append(queue, chansend)
				}
			}
			if call, ok := node.(*ast.CallExpr); ok {
				if identifier, ok := call.Fun.(*ast.Ident); ok {
					if builtin, ok := current.info.Uses[identifier].(*types.Builtin); ok {
						switch builtin.Name() {
						case "new":
							if !runtimeAllocation {
								break
							}
							if newobject, exists := runtimeFunctions["newobject"]; exists {
								queue = append(queue, newobject)
							}
						case "make":
							if _, channel := current.info.Types[call].Type.Underlying().(*types.Chan); channel {
								if makechan, exists := runtimeFunctions["makechan"]; exists {
									queue = append(queue, makechan)
								}
							}
						}
					}
				}
			}
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

func hasRuntimeMapsLinkName(function *ast.FuncDecl) bool {
	if function.Doc == nil {
		return false
	}
	for _, comment := range function.Doc.List {
		fields := strings.Fields(strings.TrimPrefix(comment.Text, "//"))
		if len(fields) == 3 && fields[0] == "go:linkname" && strings.HasPrefix(fields[2], "internal/runtime/maps.") {
			return true
		}
	}
	return false
}
