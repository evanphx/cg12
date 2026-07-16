package goc

import (
	"fmt"
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

type packageInit struct {
	path         string
	declarations []functionDecl
}

func runtimeInitDeclarations(units map[string]*sourceUnit) ([]functionDecl, map[*types.Func]string) {
	unit := units["runtime"]
	initSymbols := make(map[*types.Func]string)
	if unit == nil {
		return nil, initSymbols
	}
	return packageInitDeclarations(unit.files, unit.info, unit.pkg, initSymbols), initSymbols
}

func packageInitDeclarations(files []*ast.File, info *types.Info, pkg *types.Package, initSymbols map[*types.Func]string) []functionDecl {
	var declarations []functionDecl
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || function.Name.Name != "init" {
				continue
			}
			object, ok := info.Defs[function.Name].(*types.Func)
			if !ok {
				continue
			}
			signature, ok := object.Type().(*types.Signature)
			if !ok || signature.Recv() != nil {
				continue
			}
			initSymbols[object] = fmt.Sprintf("%s.init.%d", pkg.Path(), len(declarations))
			declarations = append(declarations, functionDecl{decl: function, info: info, pkg: pkg})
		}
	}
	return declarations
}

func moduleInitDeclarations(rootFiles []*ast.File, rootInfo *types.Info, rootPkg *types.Package, units map[string]*sourceUnit, initSymbols map[*types.Func]string) []packageInit {
	visited := make(map[string]bool)
	var packages []packageInit
	var visit func(*types.Package)
	visit = func(pkg *types.Package) {
		if pkg == nil || visited[pkg.Path()] || pkg.Path() == "runtime" {
			return
		}
		visited[pkg.Path()] = true
		for _, imported := range pkg.Imports() {
			visit(imported)
		}

		files := rootFiles
		info := rootInfo
		if pkg != rootPkg {
			unit := units[pkg.Path()]
			if unit == nil {
				return
			}
			files = unit.files
			info = unit.info
		}
		declarations := packageInitDeclarations(files, info, pkg, initSymbols)
		if len(declarations) > 0 {
			packages = append(packages, packageInit{path: pkg.Path(), declarations: declarations})
		}
	}
	visit(rootPkg)
	return packages
}

// reachableFunctions follows statically named function calls across source
// units. Calls through interfaces are recorded by their interface method and
// resolved later by interface lowering.
func reachableFunctions(roots []*ast.FuncDecl, rootInfo *types.Info, rootPkg *types.Package, units map[string]*sourceUnit, runtimeAllocation bool, initializers []functionDecl, linkNames map[*types.Func]string, assemblyReferences map[string]bool) []functionDecl {
	declarations := make(map[*types.Func]functionDecl)
	linkedDeclarations := make(map[string]functionDecl)
	methods := make(map[string][]functionDecl)
	runtimeFunctions := make(map[string]functionDecl)
	var genericRuntimeMethods []functionDecl
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
	for function, declaration := range declarations {
		symbol := functionSymbol(function)
		if linked := linkNames[function]; linked != "" {
			symbol = linked
		}
		if !runtimeAllocation && declaration.pkg.Path() == "runtime" &&
			(symbol == "crypto/internal/fips140.getIndicator" || symbol == "crypto/internal/fips140.setIndicator") {
			continue
		}
		linkedDeclarations[symbol] = declaration
	}

	var queue []functionDecl
	enqueueObject := func(object *types.Func) {
		if declaration, ok := declarations[object]; ok {
			queue = append(queue, declaration)
			return
		}
		callSymbol := functionSymbol(object)
		if linked := linkNames[object]; linked != "" {
			callSymbol = linked
		}
		if declaration, ok := linkedDeclarations[callSymbol]; ok {
			queue = append(queue, declaration)
			return
		}
		candidates := methods[object.Name()]
		if len(candidates) == 1 {
			queue = append(queue, candidates[0])
		}
	}
	enqueueImplementation := func(sourceType types.Type, interfaceType *types.Interface) {
		if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
			return
		}
		if !types.Implements(sourceType, interfaceType) {
			return
		}
		for methodIndex := 0; methodIndex < interfaceType.NumMethods(); methodIndex++ {
			for _, candidate := range methods[interfaceType.Method(methodIndex).Name()] {
				candidateObject := candidate.info.Defs[candidate.decl.Name].(*types.Func)
				candidateReceiver := candidateObject.Type().(*types.Signature).Recv().Type()
				if types.Identical(candidateReceiver, sourceType) {
					queue = append(queue, candidate)
				}
			}
		}
	}
	if runtimeAllocation {
		for _, unit := range units {
			for _, file := range unit.files {
				for _, declaration := range file.Decls {
					global, ok := declaration.(*ast.GenDecl)
					if !ok || global.Tok != token.VAR {
						continue
					}
					for _, specification := range global.Specs {
						values := specification.(*ast.ValueSpec).Values
						for _, value := range values {
							ast.Inspect(value, func(node ast.Node) bool {
								identifier, ok := node.(*ast.Ident)
								if !ok {
									return true
								}
								if function, ok := unit.info.Uses[identifier].(*types.Func); ok {
									enqueueObject(function)
								}
								return true
							})
						}
					}
				}
			}
		}
	}
	for _, root := range roots {
		queue = append(queue, functionDecl{decl: root, info: rootInfo, pkg: rootPkg})
	}
	for symbol, declaration := range linkedDeclarations {
		if assemblyReferences[assemblySymbolName(symbol)] {
			queue = append(queue, declaration)
		}
	}
	if runtimeAllocation {
		runtimeInits, _ := runtimeInitDeclarations(units)
		queue = append(queue, runtimeInits...)
		queue = append(queue, initializers...)
		queue = append(queue, genericRuntimeMethods...)
		for _, name := range []string{
			"args", "c128equal", "c64equal", "check", "concatstring2", "f32equal", "f64equal", "growslice",
			"interequal", "interhash", "main", "makeslice", "mallocgc", "memequal8",
			"memequal16", "memequal32", "memequal64", "memequal128", "mstart0",
			"newobject", "newstack", "nilinterequal", "nilinterhash", "osinit",
			"persistentalloc", "schedinit", "strequal",
		} {
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
			if statement, ok := node.(*ast.RangeStmt); ok {
				if basic, ok := current.info.Types[statement.X].Type.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
					if decoderune, exists := runtimeFunctions["decoderune"]; exists {
						queue = append(queue, decoderune)
					}
				}
			}
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
				if signature, ok := current.info.Types[call.Fun].Type.Underlying().(*types.Signature); ok {
					for argumentIndex, argument := range call.Args {
						parameterIndex := argumentIndex
						if signature.Variadic() && parameterIndex >= signature.Params().Len()-1 {
							parameterIndex = signature.Params().Len() - 1
						}
						if parameterIndex < 0 || parameterIndex >= signature.Params().Len() {
							continue
						}
						parameterType := signature.Params().At(parameterIndex).Type()
						if signature.Variadic() && !call.Ellipsis.IsValid() && parameterIndex == signature.Params().Len()-1 {
							parameterType = parameterType.Underlying().(*types.Slice).Elem()
						}
						if interfaceType, ok := parameterType.Underlying().(*types.Interface); ok {
							enqueueImplementation(current.info.Types[argument].Type, interfaceType)
						}
					}
				}
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
						case "close":
							if closechan, exists := runtimeFunctions["closechan"]; exists {
								queue = append(queue, closechan)
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
			enqueueObject(object)
			return true
		})
	}
	return reachable
}

func assemblySymbolName(name string) string {
	var symbol strings.Builder
	for _, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			symbol.WriteRune(character)
		} else {
			symbol.WriteByte('_')
		}
	}
	return symbol.String()
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
