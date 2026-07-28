package goc

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

type functionDecl struct {
	decl          *ast.FuncDecl
	info          *types.Info
	pkg           *types.Package
	typeArguments []types.Type
	symbol        string
}

type packageInit struct {
	path         string
	info         *types.Info
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
		packages = append(packages, packageInit{path: pkg.Path(), info: info, declarations: declarations})
	}
	visit(rootPkg)
	return packages
}

// reachableFunctions follows statically named function calls across source
// units. Calls through interfaces are recorded by their interface method and
// resolved later by interface lowering.
func reachableFunctions(roots []*ast.FuncDecl, rootFiles []*ast.File, rootInfo *types.Info, rootPkg *types.Package, units map[string]*sourceUnit, dynamicTypes []types.Type, runtimeAllocation bool, initializers []functionDecl, linkNames map[*types.Func]string, assemblyReferences map[string]bool) ([]functionDecl, map[types.Object]bool) {
	declarations := make(map[*types.Func]functionDecl)
	linkedDeclarations := make(map[string]functionDecl)
	methods := make(map[string][]functionDecl)
	runtimeFunctions := make(map[string]functionDecl)
	var genericRuntimeMethods []functionDecl
	for _, declaration := range roots {
		object, ok := rootInfo.Defs[declaration.Name].(*types.Func)
		if ok {
			declarations[object] = functionDecl{decl: declaration, info: rootInfo, pkg: rootPkg}
		}
	}
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
	type globalValueInitializer struct {
		expressions []ast.Expr
		info        *types.Info
	}
	globalValues := make(map[types.Object]globalValueInitializer)
	collectGlobalValues := func(files []*ast.File, info *types.Info) {
		for _, file := range files {
			for _, declaration := range file.Decls {
				global, ok := declaration.(*ast.GenDecl)
				if !ok || global.Tok != token.VAR {
					continue
				}
				for _, specification := range global.Specs {
					values := specification.(*ast.ValueSpec)
					for index, name := range values.Names {
						object := info.Defs[name]
						if object == nil || len(values.Values) == 0 {
							continue
						}
						expressions := values.Values
						if len(values.Values) == len(values.Names) {
							expressions = values.Values[index : index+1]
						}
						globalValues[object] = globalValueInitializer{expressions: expressions, info: info}
					}
				}
			}
		}
	}
	collectGlobalValues(rootFiles, rootInfo)
	for _, unit := range units {
		collectGlobalValues(unit.files, unit.info)
	}
	var assertedInterfaces []*types.Interface
	collectAssertedInterfaces := func(files []*ast.File, info *types.Info) {
		addAssertedType := func(assertedType types.Type) {
			if assertedType == nil {
				return
			}
			if interfaceType, ok := assertedType.Underlying().(*types.Interface); ok {
				assertedInterfaces = append(assertedInterfaces, interfaceType)
			}
		}
		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				switch expression := node.(type) {
				case *ast.TypeAssertExpr:
					if expression.Type == nil {
						return true
					}
					addAssertedType(info.Types[expression.Type].Type)
				case *ast.TypeSwitchStmt:
					for _, statement := range expression.Body.List {
						clause, ok := statement.(*ast.CaseClause)
						if !ok {
							continue
						}
						for _, caseType := range clause.List {
							addAssertedType(info.Types[caseType].Type)
						}
					}
				case *ast.CallExpr:
					identifier := functionIdentifier(expression.Fun)
					if identifier == nil {
						return true
					}
					function, ok := info.Uses[identifier].(*types.Func)
					if !ok || function.Pkg() == nil || function.Pkg().Path() != "reflect" || function.Name() != "TypeAssert" {
						return true
					}
					instance, ok := info.Instances[identifier]
					if !ok || instance.TypeArgs.Len() == 0 {
						return true
					}
					addAssertedType(instance.TypeArgs.At(0))
				}
				return true
			})
		}
	}
	collectAssertedInterfaces(rootFiles, rootInfo)
	for _, unit := range units {
		collectAssertedInterfaces(unit.files, unit.info)
	}
	dynamicInterfaceTypes := make(map[string]types.Type)
	var addDynamicInterfaceType func(types.Type)
	addDynamicInterfaceType = func(valueType types.Type) {
		if valueType == nil {
			return
		}
		valueType = canonicalAliasType(valueType)
		key := goTypeKey(valueType)
		if dynamicInterfaceTypes[key] != nil {
			return
		}
		dynamicInterfaceTypes[key] = valueType

		if named, ok := types.Unalias(valueType).(*types.Named); ok {
			addDynamicInterfaceType(types.NewPointer(named))
		}
		switch value := valueType.Underlying().(type) {
		case *types.Pointer:
			addDynamicInterfaceType(value.Elem())
		case *types.Array:
			addDynamicInterfaceType(value.Elem())
		case *types.Slice:
			addDynamicInterfaceType(value.Elem())
		case *types.Map:
			addDynamicInterfaceType(value.Key())
			addDynamicInterfaceType(value.Elem())
		case *types.Struct:
			for fieldIndex := 0; fieldIndex < value.NumFields(); fieldIndex++ {
				addDynamicInterfaceType(value.Field(fieldIndex).Type())
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
		addDynamicInterfaceType(sourceType)
		if !types.Implements(sourceType, interfaceType) {
			return
		}
		for methodIndex := 0; methodIndex < interfaceType.NumMethods(); methodIndex++ {
			method := interfaceType.Method(methodIndex)
			object, _, _ := types.LookupFieldOrMethod(sourceType, true, method.Pkg(), method.Name())
			if function, ok := object.(*types.Func); ok {
				enqueueObject(function)
			}
		}
		for _, assertedInterface := range assertedInterfaces {
			if !types.Implements(sourceType, assertedInterface) {
				continue
			}
			for methodIndex := 0; methodIndex < assertedInterface.NumMethods(); methodIndex++ {
				method := assertedInterface.Method(methodIndex)
				object, _, _ := types.LookupFieldOrMethod(sourceType, true, method.Pkg(), method.Name())
				if function, ok := object.(*types.Func); ok {
					enqueueObject(function)
				}
			}
		}
	}
	enqueueTypeImplementation := func(sourceType, targetType types.Type) {
		if sourceType == nil || targetType == nil {
			return
		}
		interfaceType, ok := targetType.Underlying().(*types.Interface)
		if !ok {
			return
		}
		if basic, ok := sourceType.Underlying().(*types.Basic); ok && basic.Info()&types.IsUntyped != 0 {
			sourceType = types.Default(sourceType)
		}
		enqueueImplementation(sourceType, interfaceType)
	}
	enqueueValueImplementation := func(expression ast.Expr, targetType types.Type, info *types.Info) {
		if expression == nil {
			return
		}
		enqueueTypeImplementation(info.Types[expression].Type, targetType)
	}
	enqueueCompositeImplementations := func(literal *ast.CompositeLit, info *types.Info) {
		literalType := info.Types[literal].Type
		if literalType == nil {
			return
		}
		switch compositeType := literalType.Underlying().(type) {
		case *types.Struct:
			for elementIndex, element := range literal.Elts {
				fieldIndex := elementIndex
				value := element
				if keyed, ok := element.(*ast.KeyValueExpr); ok {
					identifier, ok := keyed.Key.(*ast.Ident)
					if !ok {
						continue
					}
					fieldIndex = -1
					for index := 0; index < compositeType.NumFields(); index++ {
						if compositeType.Field(index).Name() == identifier.Name {
							fieldIndex = index
							break
						}
					}
					value = keyed.Value
				}
				if fieldIndex < 0 || fieldIndex >= compositeType.NumFields() {
					continue
				}
				enqueueValueImplementation(value, compositeType.Field(fieldIndex).Type(), info)
			}
		case *types.Array:
			for _, element := range literal.Elts {
				if keyed, ok := element.(*ast.KeyValueExpr); ok {
					element = keyed.Value
				}
				enqueueValueImplementation(element, compositeType.Elem(), info)
			}
		case *types.Slice:
			for _, element := range literal.Elts {
				if keyed, ok := element.(*ast.KeyValueExpr); ok {
					element = keyed.Value
				}
				enqueueValueImplementation(element, compositeType.Elem(), info)
			}
		case *types.Map:
			for _, element := range literal.Elts {
				keyed, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				enqueueValueImplementation(keyed.Key, compositeType.Key(), info)
				enqueueValueImplementation(keyed.Value, compositeType.Elem(), info)
			}
		}
	}
	seenGlobals := make(map[types.Object]bool)
	var enqueueGlobal func(types.Object)
	enqueueGlobal = func(object types.Object) {
		if seenGlobals[object] {
			return
		}
		seenGlobals[object] = true
		initializer, exists := globalValues[object]
		if !exists {
			return
		}
		for _, expression := range initializer.expressions {
			enqueueValueImplementation(expression, object.Type(), initializer.info)
			ast.Inspect(expression, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok && len(call.Args) == 1 && initializer.info.Types[call.Fun].IsType() {
					enqueueValueImplementation(call.Args[0], initializer.info.Types[call].Type, initializer.info)
				}
				if literal, ok := node.(*ast.CompositeLit); ok {
					enqueueCompositeImplementations(literal, initializer.info)
				}
				identifier, ok := node.(*ast.Ident)
				if !ok {
					return true
				}
				switch referenced := initializer.info.Uses[identifier].(type) {
				case *types.Var:
					enqueueGlobal(referenced)
				case *types.Func:
					if instance, instantiated := initializer.info.Instances[identifier]; instantiated {
						origin := referenced.Origin()
						if declaration, found := declarations[origin]; found {
							arguments := make([]types.Type, instance.TypeArgs.Len())
							for index := range arguments {
								arguments[index] = instance.TypeArgs.At(index)
							}
							declaration.typeArguments = arguments
							declaration.symbol = genericInstanceSymbol(origin, arguments)
							queue = append(queue, declaration)
							return true
						}
					}
					enqueueObject(referenced)
				}
				return true
			})
		}
	}
	if runtimeAllocation {
		for object := range globalValues {
			enqueueGlobal(object)
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
			"args", "atomicstorep", "c128equal", "c64equal", "check", "concatstring2", "deferproc", "deferreturn", "f32equal", "f64equal", "gopanic", "gorecover", "growslice",
			"goPanicSliceConvert", "interequal", "interhash", "main", "makeslice", "mallocgc",
			"getitab", "gocInterfaceDispatchFailure", "inheap", "inHeapOrStack", "makemap", "mapclear", "mapdelete",
			"memclrHasPointers",
			"memequal0", "memequal8", "memequal16", "memequal32", "memequal64", "memequal128",
			"memhash0", "memhash8", "memhash16", "memhash128", "mstart0",
			"morestackc", "newobject", "newstack", "nilinterequal", "nilinterhash", "osinit", "persistentalloc", "unreachableMethod",
			"printbool", "printfloat32", "printfloat64", "printhex", "printint", "printnl",
			"printstring", "printuint", "schedinit", "strequal", "typehash", "typedslicecopy",
		} {
			if declaration, exists := runtimeFunctions[name]; exists {
				queue = append(queue, declaration)
			}
		}
		for _, symbol := range []string{"runtime.mapaccess2", "runtime.mapassign"} {
			if declaration, exists := linkedDeclarations[symbol]; exists {
				queue = append(queue, declaration)
			}
		}
		for name, declaration := range runtimeFunctions {
			if name == "mallocPanic" || strings.HasPrefix(name, "mallocgcSmall") || strings.HasPrefix(name, "mallocgcTiny") || hasRuntimeMapsLinkName(declaration.decl) {
				queue = append(queue, declaration)
			}
		}
	}
	seen := make(map[string]bool)
	var reachable []functionDecl
	processQueue := func() {
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			key := fmt.Sprintf("%p", current.decl)
			if current.symbol != "" {
				key = current.symbol
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			reachable = append(reachable, current)

			currentFunction, _ := current.info.Defs[current.decl.Name].(*types.Func)
			currentSignature, _ := currentFunction.Type().(*types.Signature)
			parents := astParents(current.decl.Body)
			ast.Inspect(current.decl.Body, func(node ast.Node) bool {
				switch statement := node.(type) {
				case *ast.CompositeLit:
					enqueueCompositeImplementations(statement, current.info)
				case *ast.AssignStmt:
					if len(statement.Lhs) == len(statement.Rhs) {
						for index, value := range statement.Rhs {
							targetType := current.info.Types[statement.Lhs[index]].Type
							enqueueValueImplementation(value, targetType, current.info)
						}
					} else if len(statement.Rhs) == 1 && len(statement.Lhs) > 1 {
						call, ok := statement.Rhs[0].(*ast.CallExpr)
						if !ok {
							break
						}
						signature, ok := current.info.Types[call.Fun].Type.Underlying().(*types.Signature)
						if !ok || signature.Results().Len() != len(statement.Lhs) {
							break
						}
						for index, lhs := range statement.Lhs {
							targetType := current.info.Types[lhs].Type
							sourceType := signature.Results().At(index).Type()
							enqueueTypeImplementation(sourceType, targetType)
						}
					}
				case *ast.ValueSpec:
					for index, value := range statement.Values {
						if index >= len(statement.Names) {
							continue
						}
						object := current.info.Defs[statement.Names[index]]
						if object == nil {
							object = current.info.Uses[statement.Names[index]]
						}
						if object != nil {
							enqueueValueImplementation(value, object.Type(), current.info)
						}
					}
				case *ast.ReturnStmt:
					signature := currentSignature
					for parent := ast.Node(statement); parent != nil; parent = parents[parent] {
						if function, ok := parent.(*ast.FuncLit); ok {
							signature, _ = current.info.Types[function.Type].Type.(*types.Signature)
							break
						}
					}
					if signature != nil && len(statement.Results) == signature.Results().Len() {
						for index, value := range statement.Results {
							enqueueValueImplementation(value, signature.Results().At(index).Type(), current.info)
						}
					} else if signature != nil && len(statement.Results) == 1 {
						call, ok := statement.Results[0].(*ast.CallExpr)
						if ok {
							forwarded, ok := current.info.Types[call.Fun].Type.Underlying().(*types.Signature)
							if ok && forwarded.Results().Len() == signature.Results().Len() {
								for index := 0; index < forwarded.Results().Len(); index++ {
									sourceType := forwarded.Results().At(index).Type()
									targetType := signature.Results().At(index).Type()
									enqueueTypeImplementation(sourceType, targetType)
								}
							}
						}
					}
				case *ast.SendStmt:
					if channelType, ok := current.info.Types[statement.Chan].Type.Underlying().(*types.Chan); ok {
						enqueueValueImplementation(statement.Value, channelType.Elem(), current.info)
					}
				}
				if statement, ok := node.(*ast.RangeStmt); ok {
					rangeType := current.info.Types[statement.X].Type
					if basic, ok := rangeType.Underlying().(*types.Basic); ok && (basic.Kind() == types.String || basic.Kind() == types.UntypedString) {
						if decoderune, exists := runtimeFunctions["decoderune"]; exists {
							queue = append(queue, decoderune)
						}
					}
					if runtimeAllocation {
						if isMapRangeType(rangeType) {
							for _, name := range []string{"mapiterinit", "mapiternext"} {
								if iteratorFunction, exists := runtimeFunctions[name]; exists {
									queue = append(queue, iteratorFunction)
								}
							}
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
					if chanrecv, exists := runtimeFunctions["chanrecv2"]; exists {
						queue = append(queue, chanrecv)
					}
				}
				if _, ok := node.(*ast.SendStmt); ok {
					if chansend, exists := runtimeFunctions["chansend1"]; exists {
						queue = append(queue, chansend)
					}
				}
				if statement, ok := node.(*ast.SelectStmt); ok {
					if len(statement.Body.List) == 0 {
						if block, exists := runtimeFunctions["block"]; exists {
							queue = append(queue, block)
						}
					} else if nonblocking, send := singleDefaultSelect(statement); nonblocking {
						name := "selectnbrecv"
						if send {
							name = "selectnbsend"
						}
						if function, exists := runtimeFunctions[name]; exists {
							queue = append(queue, function)
						}
					} else {
						if selectGo, exists := runtimeFunctions["selectgo"]; exists {
							queue = append(queue, selectGo)
						}
					}
				}
				if call, ok := node.(*ast.CallExpr); ok {
					if current.info.Types[call.Fun].IsType() && len(call.Args) == 1 {
						enqueueValueImplementation(call.Args[0], current.info.Types[call].Type, current.info)
						if runtimeAllocation {
							source, sourceIsBasic := current.info.Types[call.Args[0]].Type.Underlying().(*types.Basic)
							target, targetIsBasic := current.info.Types[call].Type.Underlying().(*types.Basic)
							if sourceIsBasic && targetIsBasic && source.Info()&types.IsInteger != 0 && target.Kind() == types.String {
								if intstring, exists := runtimeFunctions["intstring"]; exists {
									queue = append(queue, intstring)
								}
							}
						}
					}
					if runtimeAllocation && current.pkg.Path() != "runtime" && current.info.Types[call.Fun].IsType() && len(call.Args) == 1 {
						sourceType := current.info.Types[call.Args[0]].Type
						targetType := current.info.Types[call].Type
						if sourceSlice, ok := sourceType.Underlying().(*types.Slice); ok {
							if target, ok := targetType.Underlying().(*types.Basic); ok && target.Kind() == types.String {
								if element, ok := sourceSlice.Elem().Underlying().(*types.Basic); ok {
									functionName := ""
									switch element.Kind() {
									case types.Uint8:
										functionName = "slicebytetostring"
										if comparison, ok := parents[call].(*ast.BinaryExpr); ok {
											switch comparison.Op {
											case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
												functionName = "slicebytetostringtmp"
											}
										}
									case types.Int32:
										functionName = "slicerunetostring"
									}
									if function, exists := runtimeFunctions[functionName]; exists {
										queue = append(queue, function)
									}
								}
							}
						}
						if source, ok := sourceType.Underlying().(*types.Basic); ok && source.Kind() == types.String {
							if targetSlice, ok := targetType.Underlying().(*types.Slice); ok {
								if element, ok := targetSlice.Elem().Underlying().(*types.Basic); ok {
									functionName := ""
									switch element.Kind() {
									case types.Uint8:
										functionName = "stringtoslicebyte"
									case types.Int32:
										functionName = "stringtoslicerune"
									}
									if function, exists := runtimeFunctions[functionName]; exists {
										queue = append(queue, function)
									}
								}
							}
						}
					}
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
				if variable, ok := current.info.Uses[identifier].(*types.Var); ok {
					enqueueGlobal(variable)
					return true
				}
				object, ok := current.info.Uses[identifier].(*types.Func)
				if !ok {
					return true
				}
				// A method selected on a value whose static type is a type
				// parameter resolves to the constraint interface's method,
				// which has no body. Inside an instantiated body the call
				// really targets the type argument's method, so that is the
				// method whose body has to be reachable.
				if selector, ok := parents[identifier].(*ast.SelectorExpr); ok && selector.Sel == identifier {
					selection := current.info.Selections[selector]
					substitutions := functionTypeSubstitutions(current)
					if concreteSelection, resolved := typeParameterMethodSelection(selection, object, substitutions); resolved {
						object = concreteSelection.Obj().(*types.Func)
					}
				}
				if instance, ok := current.info.Instances[identifier]; ok {
					origin := object.Origin()
					signature := origin.Type().(*types.Signature)
					if signature.TypeParams().Len() > 0 {
						declaration, exists := declarations[origin]
						if exists {
							arguments := make([]types.Type, instance.TypeArgs.Len())
							substitutions := functionTypeSubstitutions(current)
							for index := range arguments {
								arguments[index] = substituteType(instance.TypeArgs.At(index), substitutions)
							}
							declaration.typeArguments = arguments
							declaration.symbol = genericInstanceSymbol(origin, arguments)
							queue = append(queue, declaration)
							return true
						}
					}
				}
				origin := object.Origin()
				originSignature := origin.Type().(*types.Signature)
				if originSignature.RecvTypeParams().Len() != 0 {
					arguments := receiverTypeArguments(object)
					if len(arguments) == originSignature.RecvTypeParams().Len() {
						substitutions := functionTypeSubstitutions(current)
						for index := range arguments {
							arguments[index] = substituteType(arguments[index], substitutions)
						}
						if declaration, exists := declarations[origin]; exists {
							declaration.typeArguments = arguments
							declaration.symbol = genericInstanceSymbol(origin, arguments)
							queue = append(queue, declaration)
							return true
						}
					}
				}
				enqueueObject(object)
				return true
			})
		}
	}
	checkedDynamicTypes := make(map[string]bool)
	for {
		processQueue()
		added := false
		for key, dynamicType := range dynamicInterfaceTypes {
			if checkedDynamicTypes[key] {
				continue
			}
			checkedDynamicTypes[key] = true
			added = true
			for _, assertedInterface := range assertedInterfaces {
				enqueueImplementation(dynamicType, assertedInterface)
			}
		}
		if !added {
			break
		}
	}
	return reachable, seenGlobals
}

func isMapRangeType(valueType types.Type) bool {
	if valueType == nil {
		return false
	}
	if _, isMap := valueType.Underlying().(*types.Map); isMap {
		return true
	}
	parameter, isTypeParameter := valueType.(*types.TypeParam)
	if !isTypeParameter {
		return false
	}
	for _, term := range typeParameterTerms(parameter) {
		if _, isMap := term.Underlying().(*types.Map); isMap {
			return true
		}
	}
	return false
}

func collectDynamicTypes(rootInfo *types.Info, units map[string]*sourceUnit) []types.Type {
	byKey := make(map[string]types.Type)
	add := func(valueType types.Type) {
		if valueType == nil {
			return
		}
		key := goTypeKey(valueType)
		if _, exists := byKey[key]; !exists {
			byKey[key] = valueType
		}
		if named, ok := types.Unalias(valueType).(*types.Named); ok {
			pointer := types.NewPointer(named)
			pointerKey := goTypeKey(pointer)
			if _, exists := byKey[pointerKey]; !exists {
				byKey[pointerKey] = pointer
			}
		}
	}
	collect := func(info *types.Info) {
		for _, typeAndValue := range info.Types {
			add(typeAndValue.Type)
		}
		for _, object := range info.Defs {
			if typeName, ok := object.(*types.TypeName); ok {
				add(typeName.Type())
			}
		}
	}
	collect(rootInfo)
	for _, unit := range units {
		collect(unit.info)
	}

	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	dynamicTypes := make([]types.Type, 0, len(keys))
	for _, key := range keys {
		dynamicTypes = append(dynamicTypes, byKey[key])
	}
	return dynamicTypes
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

func singleDefaultSelect(statement *ast.SelectStmt) (ok bool, send bool) {
	communicationCount := 0
	for _, item := range statement.Body.List {
		clause := item.(*ast.CommClause)
		if clause.Comm == nil {
			continue
		}
		communicationCount++
		switch clause.Comm.(type) {
		case *ast.SendStmt:
			send = true
		case *ast.ExprStmt, *ast.AssignStmt:
			send = false
		default:
			return false, false
		}
	}
	return len(statement.Body.List) == 2 && communicationCount == 1, send
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
