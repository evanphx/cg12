// Package goc implements a Go front end for cg12.
//
// Parsing and type checking are deliberately delegated to the standard
// library.  This package only translates the type-checked syntax into cg12 IR.
package goc

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/plan9asm"
)

// Compile parses and type-checks one Go source file and lowers it to cg12 IR.
func Compile(name string, src []byte) (*ir.Module, error) {
	return compile(name, src, compileOptions{})
}

// CompileExecutable lowers a main package together with the Go runtime needed
// to start and run it as a normal Go executable.
func CompileExecutable(name string, src []byte) (*ir.Module, error) {
	return compile(name, src, compileOptions{executable: true})
}

type compileOptions struct {
	executable           bool
	testPackages         map[string]bool
	externalTestPackages map[string]string
}

func compile(name string, src []byte, options compileOptions) (*ir.Module, error) {
	executable := options.executable
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.AllErrors|parser.ParseComments)
	if err != nil {
		return nil, err
	}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Implicits:  make(map[ast.Node]types.Object),
		Instances:  make(map[*ast.Ident]types.Instance),
	}
	loader := newSourceLoader(fset)
	loader.forcePureGo = !executable
	for path := range options.testPackages {
		loader.testPackages[path] = true
	}
	for path, packagePath := range options.externalTestPackages {
		loader.externalTestPackages[path] = packagePath
	}
	conf := types.Config{Importer: loader}
	pkg, err := conf.Check(file.Name.Name, fset, []*ast.File{file}, info)
	if err != nil {
		return nil, err
	}
	if executable && pkg.Name() != "main" {
		return nil, fmt.Errorf("goc: executable source must declare package main")
	}
	if executable {
		if _, err := loader.Import("runtime"); err != nil {
			return nil, fmt.Errorf("load runtime: %w", err)
		}
	}
	mod := ir.NewModule()
	emitRuntimeTables := executable
	if !emitRuntimeTables {
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok {
				if function, ok := info.Uses[selector.Sel].(*types.Func); ok && function.Pkg() != nil && function.Pkg().Path() == "runtime" && function.Name() == "GC" {
					emitRuntimeTables = true
				}
			}
			return true
		})
	}
	compileRuntime := emitRuntimeTables
	if compileRuntime {
		mod.Data = append(mod.Data, &ir.Data{Name: ".goc.runtime.datastart", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}})
	}
	typeTags := make(map[string]string)
	runtimeTypes := make(map[string]types.Type)
	goABITypes := make(map[string]*ir.AggType)
	linkNames := make(map[*types.Func]string)
	globalLinkNames := make(map[types.Object]string)
	interfaceMethods := make(map[*types.Func]bool)
	interfaceItabs := make(map[string]string)
	collectLinkNames([]*ast.File{file}, info, linkNames)
	collectGlobalLinkNames([]*ast.File{file}, pkg, globalLinkNames)
	for _, unit := range loader.units {
		collectLinkNames(unit.files, unit.info, linkNames)
		collectGlobalLinkNames(unit.files, unit.pkg, globalLinkNames)
	}
	functionDecls := collectFunctionDeclarations(info, pkg, []*ast.File{file}, loader.units)
	runtimeInits, initSymbols := runtimeInitDeclarations(loader.units)
	moduleInits := moduleInitDeclarations([]*ast.File{file}, info, pkg, loader.units, initSymbols)
	var moduleInitFunctions []functionDecl
	for _, packageInitializer := range moduleInits {
		moduleInitFunctions = append(moduleInitFunctions, packageInitializer.declarations...)
	}
	noWriteBarriers := collectNoWriteBarrierFunctions(functionDecls)
	var roots []*ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil {
			roots = append(roots, function)
		}
	}
	assemblyReferences := make(map[string]bool)
	if compileRuntime {
		assemblyReferences, err = sourceAssemblyReferences(loader.units)
		if err != nil {
			return nil, err
		}
	}
	dynamicTypes := collectDynamicTypes(info, loader.units)
	functions, reachableGlobals := reachableFunctions(roots, []*ast.File{file}, info, pkg, loader.units, dynamicTypes, compileRuntime, moduleInitFunctions, linkNames, assemblyReferences)
	globalPackages := map[string]bool{pkg.Path(): true}
	if compileRuntime {
		for path := range loader.units {
			globalPackages[path] = true
		}
	} else {
		for _, function := range functions {
			globalPackages[function.pkg.Path()] = true
			ast.Inspect(function.decl.Body, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if !ok {
					return true
				}
				variable, ok := function.info.Uses[identifier].(*types.Var)
				if ok && variable.Pkg() != nil {
					globalPackages[variable.Pkg().Path()] = true
				}
				return true
			})
		}
	}
	var assemblyPackages []string
	for path := range loader.units {
		if globalPackages[path] {
			assemblyPackages = append(assemblyPackages, path)
		}
	}
	sort.Strings(assemblyPackages)
	for _, path := range assemblyPackages {
		unit := loader.units[path]
		defines := assemblyPackageDefines(unit.pkg)
		floatInputs, floatOutputs := assemblyPackageFloatSlots(unit)
		for _, assembly := range unit.assembly {
			mod.Assembly = append(mod.Assembly, ir.AssemblyFile{
				PackagePath:  path,
				Path:         assembly.path,
				Source:       assembly.source,
				Defines:      defines,
				Includes:     assembly.includes,
				FloatInputs:  floatInputs,
				FloatOutputs: floatOutputs,
			})
		}
	}
	g := &gen{
		fset:                    fset,
		file:                    file,
		info:                    info,
		pkg:                     pkg,
		mod:                     mod,
		globals:                 map[types.Object]string{},
		globalLinkNames:         globalLinkNames,
		emitRuntimeTables:       emitRuntimeTables,
		runtimeAllocation:       compileRuntime,
		typeTags:                typeTags,
		runtimeTypes:            runtimeTypes,
		goABITypes:              goABITypes,
		linkNames:               linkNames,
		initSymbols:             initSymbols,
		functionDecls:           functionDecls,
		noWriteBarrierFunctions: noWriteBarriers,
		interfaceMethods:        interfaceMethods,
		interfaceItabs:          interfaceItabs,
		dynamicTypes:            dynamicTypes,
		reachableGlobals:        reachableGlobals,
	}
	g.mod.File(name)
	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			g.globalDecl(gd)
		}
	}
	packageGlobals := map[string]map[types.Object]string{pkg.Path(): g.globals}
	for path, unit := range loader.units {
		globals := make(map[types.Object]string)
		packageGlobals[path] = globals
		if !globalPackages[path] {
			continue
		}
		generator := &gen{
			fset:                    fset,
			info:                    unit.info,
			pkg:                     unit.pkg,
			mod:                     mod,
			globals:                 globals,
			globalLinkNames:         globalLinkNames,
			emitRuntimeTables:       emitRuntimeTables,
			runtimeAllocation:       compileRuntime,
			typeTags:                typeTags,
			runtimeTypes:            runtimeTypes,
			goABITypes:              goABITypes,
			linkNames:               linkNames,
			initSymbols:             initSymbols,
			functionDecls:           functionDecls,
			noWriteBarrierFunctions: noWriteBarriers,
			interfaceMethods:        interfaceMethods,
			interfaceItabs:          interfaceItabs,
			reachableGlobals:        reachableGlobals,
			filterGlobals:           !compileRuntime,
		}
		for _, sourceFile := range unit.files {
			for _, declaration := range sourceFile.Decls {
				global, ok := declaration.(*ast.GenDecl)
				if ok && global.Tok == token.VAR {
					generator.globalDecl(global)
				}
			}
		}
		if generator.err != nil {
			return nil, generator.err
		}
	}
	dynamicInitializers := collectDynamicInitializers([]*ast.File{file}, info, pkg, loader.units, packageGlobals)
	dynamicInitializerGuards := make(map[types.Object]string)
	dynamicInitializerFunctions := dynamicInitializerFunctionSymbols(dynamicInitializers)
	allGlobals := make(map[types.Object]string)
	for _, globals := range packageGlobals {
		for object, symbol := range globals {
			allGlobals[object] = symbol
		}
	}
	g.globals = allGlobals
	g.dynamicInitializers = dynamicInitializers
	g.dynamicInitializerGuards = dynamicInitializerGuards
	g.dynamicInitializerFunctions = dynamicInitializerFunctions
	g.initializingGlobals = make(map[types.Object]bool)
	methodTargets := make(map[string]*types.Func)
	ambiguousMethods := make(map[string]bool)
	for _, function := range functions {
		object, ok := function.info.Defs[function.decl.Name].(*types.Func)
		if !ok {
			continue
		}
		signature := object.Type().(*types.Signature)
		if signature.Recv() != nil {
			name := object.Name()
			if existing := methodTargets[name]; existing != nil && existing != object {
				delete(methodTargets, name)
				ambiguousMethods[name] = true
			} else if !ambiguousMethods[name] {
				methodTargets[name] = object
			}
		}
	}
	g.methodTargets = methodTargets
	if err := generateDynamicInitializerFunctions(g, dynamicInitializers); err != nil {
		return nil, err
	}
	for i := len(functions) - 1; i >= 0; i-- {
		function := functions[i]
		generator := g
		if function.pkg != pkg {
			generator = &gen{
				fset:                        fset,
				info:                        function.info,
				pkg:                         function.pkg,
				mod:                         mod,
				globals:                     allGlobals,
				methodTargets:               methodTargets,
				emitRuntimeTables:           emitRuntimeTables,
				runtimeAllocation:           compileRuntime,
				typeTags:                    typeTags,
				runtimeTypes:                runtimeTypes,
				goABITypes:                  goABITypes,
				linkNames:                   linkNames,
				initSymbols:                 initSymbols,
				functionDecls:               functionDecls,
				noWriteBarrierFunctions:     noWriteBarriers,
				interfaceMethods:            interfaceMethods,
				interfaceItabs:              interfaceItabs,
				dynamicInitializers:         dynamicInitializers,
				dynamicInitializerGuards:    dynamicInitializerGuards,
				dynamicInitializerFunctions: dynamicInitializerFunctions,
				initializingGlobals:         make(map[types.Object]bool),
			}
		}
		generator.typeArguments = function.typeArguments
		generator.functionName = function.symbol
		generator.funcDecl(function.decl)
		if generator.err != nil {
			return nil, generator.err
		}
	}
	addInterfaceMethodWrappers(g, functions)
	if compileRuntime {
		populateRuntimePointerTypes(mod, typeTags, runtimeTypes)
		clearUnavailableRuntimeMethodOffsets(mod)
	}
	if loader.units["crypto/internal/fips140"] != nil && !compileRuntime {
		addFIPSRuntimeStubs(mod)
	}
	if !compileRuntime && (loader.units["crypto/sha1"] != nil || loader.units["crypto/md5"] != nil) {
		addLegacyCryptoRuntimeStubs(mod)
	}
	if g.err != nil {
		return nil, g.err
	}
	if compileRuntime {
		addInterfaceItabLinks(mod, interfaceItabs)
		if err := addRuntimeInitTask(mod, runtimeInits, initSymbols); err != nil {
			return nil, err
		}
		if err := addModuleInitTasks(mod, moduleInits, initSymbols); err != nil {
			return nil, err
		}
		mod.Data = append(mod.Data, &ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}})
	}
	addMemoryHelpers(mod, compileRuntime)
	if compileRuntime {
		for _, function := range mod.Funcs {
			function.GoABI = true
		}
		opt.InlineNoSplitCalls(mod)
	}
	opt.InlineHeapAllocations(mod)
	opt.LowerHeapAllocations(mod)
	return g.mod, nil
}

func assemblyPackageDefines(pkg *types.Package) map[string]int64 {
	defines := make(map[string]int64)
	sizes := types.SizesFor("gc", runtime.GOARCH)
	for _, name := range pkg.Scope().Names() {
		object := pkg.Scope().Lookup(name)
		switch object := object.(type) {
		case *types.Const:
			if object.Val().Kind() != constant.Int {
				continue
			}
			value, ok := constant.Int64Val(object.Val())
			if ok {
				defines["const_"+name] = value
			}
		case *types.TypeName:
			if sizes == nil {
				continue
			}
			named, ok := types.Unalias(object.Type()).(*types.Named)
			if !ok || named.TypeParams().Len() != 0 {
				continue
			}
			structure, ok := named.Underlying().(*types.Struct)
			if !ok {
				continue
			}
			fields := make([]*types.Var, structure.NumFields())
			for index := range fields {
				fields[index] = structure.Field(index)
			}
			offsets := sizes.Offsetsof(fields)
			defines[name+"__size"] = sizes.Sizeof(named)
			for index, field := range fields {
				if field.Name() != "_" {
					defines[name+"_"+field.Name()] = offsets[index]
				}
			}
		}
	}
	return defines
}

func assemblyPackageFloatSlots(unit *sourceUnit) (map[string][]int, map[string][]int) {
	inputs := make(map[string][]int)
	outputs := make(map[string][]int)
	sizes := types.SizesFor("gc", runtime.GOARCH)
	if sizes == nil {
		return inputs, outputs
	}
	assemblyFunctions := make(map[string]bool)
	for _, file := range unit.files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && function.Body == nil {
				assemblyFunctions[function.Name.Name] = true
			}
		}
	}
	for name := range assemblyFunctions {
		function, ok := unit.pkg.Scope().Lookup(name).(*types.Func)
		if !ok {
			continue
		}
		signature, ok := function.Type().(*types.Signature)
		if !ok || signature.Recv() != nil {
			continue
		}
		parameters := signature.Params()
		results := signature.Results()
		hasFloat := false
		for index := range parameters.Len() {
			hasFloat = hasFloat || isAssemblyFloatType(parameters.At(index).Type())
		}
		for index := range results.Len() {
			hasFloat = hasFloat || isAssemblyFloatType(results.At(index).Type())
		}
		if !hasFloat {
			continue
		}
		variables := make([]*types.Var, 0, parameters.Len()+results.Len())
		for index := range parameters.Len() {
			variables = append(variables, parameters.At(index))
		}
		for index := range results.Len() {
			variables = append(variables, results.At(index))
		}
		offsets := sizes.Offsetsof(variables)
		for index := range parameters.Len() {
			if isAssemblyFloatType(parameters.At(index).Type()) {
				inputs[name] = append(inputs[name], int(offsets[index]))
			}
		}
		for index := range results.Len() {
			if isAssemblyFloatType(results.At(index).Type()) {
				offsetIndex := parameters.Len() + index
				outputs[name] = append(outputs[name], int(offsets[offsetIndex]))
			}
		}
	}
	return inputs, outputs
}

func isAssemblyFloatType(typ types.Type) bool {
	basic, ok := types.Unalias(typ).Underlying().(*types.Basic)
	if !ok {
		return false
	}
	return basic.Kind() == types.Float32 || basic.Kind() == types.Float64
}

func sourceAssemblyReferences(units map[string]*sourceUnit) (map[string]bool, error) {
	references := make(map[string]bool)
	paths := make([]string, 0, len(units))
	for path := range units {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		unit := units[path]
		defines := assemblyPackageDefines(unit.pkg)
		floatInputs, floatOutputs := assemblyPackageFloatSlots(unit)
		for _, assembly := range unit.assembly {
			file, err := plan9asm.ParseWithOptions(strings.NewReader(assembly.source), plan9asm.ParseOptions{
				Defines: map[string]string{
					"GOARCH_" + runtime.GOARCH: "1",
					"GOOS_" + runtime.GOOS:     "1",
				},
				Includes: assembly.includes,
			})
			if err != nil {
				return nil, fmt.Errorf("parse assembly %s: %w", assembly.path, err)
			}
			translation, err := plan9asm.CompileARM64(file, plan9asm.ARM64Options{
				PackagePath:      path,
				Filename:         assembly.path,
				Defines:          defines,
				FloatInputs:      floatInputs,
				FloatOutputs:     floatOutputs,
				PreferDirectABI0: true,
			})
			if err != nil {
				return nil, fmt.Errorf("translate assembly %s: %w", assembly.path, err)
			}
			for _, reference := range translation.ExternalReferences {
				references[reference] = true
			}
		}
	}
	return references, nil
}

func addModuleInitTasks(mod *ir.Module, packages []packageInit, initSymbols map[*types.Func]string) error {
	var backingItems []ir.DataItem
	var pointerWords []int
	for packageIndex, packageInitializer := range packages {
		taskName := fmt.Sprintf(".goc.module.inittask.%d", packageIndex)
		taskItems := []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0, int64(len(packageInitializer.declarations))}}}
		for _, declaration := range packageInitializer.declarations {
			object, ok := declaration.info.Defs[declaration.decl.Name].(*types.Func)
			if !ok || initSymbols[object] == "" {
				return fmt.Errorf("goc: package %s init function has no unique symbol", packageInitializer.path)
			}
			taskItems = append(taskItems, ir.DataItem{Sub: ir.SubL, Sym: initSymbols[object]})
		}
		mod.Data = append(mod.Data, &ir.Data{Name: taskName, Align: 8, Items: taskItems})
		backingItems = append(backingItems, ir.DataItem{Sub: ir.SubL, Sym: taskName})
		pointerWords = append(pointerWords, packageIndex)
	}
	if len(backingItems) == 0 {
		return nil
	}
	mod.Data = append(mod.Data, &ir.Data{
		Name:         ".goc.module.inittasks",
		Align:        8,
		Items:        backingItems,
		PointerWords: pointerWords,
	})
	return nil
}

func addInterfaceItabLinks(mod *ir.Module, itabs map[string]string) {
	symbols := make([]string, 0, len(itabs))
	for _, symbol := range itabs {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	if len(symbols) == 0 {
		return
	}
	items := make([]ir.DataItem, len(symbols))
	pointerWords := make([]int, len(symbols))
	for index, symbol := range symbols {
		items[index] = ir.DataItem{Sub: ir.SubL, Sym: symbol}
		pointerWords[index] = index
	}
	mod.Data = append(mod.Data, &ir.Data{
		Name:         ".goc.module.itablinks",
		Align:        8,
		Items:        items,
		PointerWords: pointerWords,
	})
}

func addInterfaceMethodWrappers(g *gen, reachable []functionDecl) {
	methods := make([]*types.Func, 0, len(g.interfaceMethods))
	for method := range g.interfaceMethods {
		methods = append(methods, method)
	}
	sort.Slice(methods, func(i, j int) bool {
		return g.functionSymbol(methods[i]) < g.functionSymbol(methods[j])
	})

	for _, method := range methods {
		signature := method.Type().(*types.Signature)
		interfaceType, ok := signature.Recv().Type().Underlying().(*types.Interface)
		if !ok {
			continue
		}
		name := g.functionSymbol(method)
		var function *ir.Func
		if signature.Results().Len() == 0 {
			function = g.mod.NewFuncVoid(name)
		} else {
			resultClass, scalarResult := scalar(signature.Results().At(0).Type())
			if !scalarResult {
				g.err = fmt.Errorf("goc: interface method %s has unsupported first result %s", name, signature.Results().At(0).Type())
				return
			}
			function = g.mod.NewFunc(name, resultClass)
		}
		wrapper := &gen{
			fn:                function,
			cur:               function.Entry(),
			mod:               g.mod,
			typeTags:          g.typeTags,
			runtimeTypes:      g.runtimeTypes,
			goABITypes:        g.goABITypes,
			linkNames:         g.linkNames,
			initSymbols:       g.initSymbols,
			interfaceItabs:    g.interfaceItabs,
			runtimeAllocation: g.runtimeAllocation,
		}
		if signature.Results().Len() > 0 {
			resultType := signature.Results().At(0).Type()
			function.RetAgg = wrapper.goABIAggregate(resultType)
			function.RetValues = wrapper.runtimeAllocation && isSliceType(resultType)
		}
		receiver := wrapper.functionParameter("receiver", signature.Recv().Type(), ir.ClsP)
		parameters := make([]ir.Ref, signature.Params().Len())
		for index := range parameters {
			parameterType := signature.Params().At(index).Type()
			parameterClass, supported := scalar(parameterType)
			if !supported {
				g.err = fmt.Errorf("goc: interface method %s has unsupported parameter %s", name, parameterType)
				return
			}
			parameters[index] = wrapper.functionParameter(signature.Params().At(index).Name(), parameterType, parameterClass)
		}
		var resultPointers []ir.Ref
		if signature.Results().Len() > 0 && isInlineAggregate(signature.Results().At(0).Type()) && function.RetAgg == nil {
			resultPointers = append(resultPointers, function.ParamRef("result0"))
		}
		for index := 1; index < signature.Results().Len(); index++ {
			resultPointers = append(resultPointers, function.ParamRef(fmt.Sprintf("result%d", index)))
		}

		candidates := interfaceMethodCandidates(g, reachable, method, interfaceType)
		dynamicTag := wrapper.interfaceDynamicType(receiver, signature.Recv().Type())
		for index, candidate := range candidates {
			candidateSignature := candidate.function.Type().(*types.Signature)
			receiverType := candidateSignature.Recv().Type()
			tagName := g.typeTags[goTypeKey(candidate.dynamicType)]
			if tagName == "" {
				continue
			}
			invoke := function.NewBlock(fmt.Sprintf("invoke%d", index))
			next := function.NewBlock(fmt.Sprintf("next%d", index))
			matches := wrapper.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, function.Sym(tagName, 0))
			wrapper.cur.Jnz(matches, invoke, next)

			wrapper.cur = invoke
			arguments := make([]ir.Ref, 0, 1+len(parameters)+len(resultPointers))
			callee := function.Sym(g.functionSymbol(candidate.function), 0)
			callSignature := candidateSignature
			callReceiverType := receiverType
			if len(candidate.interfacePath) > 0 {
				arguments = append(arguments, wrapper.embeddedInterfaceMethodReceiver(receiver, candidate.dynamicType, candidate.interfacePath))
				callee = function.Sym(name, 0)
				callSignature = signature
				callReceiverType = signature.Recv().Type()
			} else {
				arguments = append(arguments, wrapper.interfaceMethodReceiver(receiver, candidate.function))
			}
			arguments = append(arguments, parameters...)
			arguments = append(arguments, resultPointers...)
			if signature.Results().Len() == 0 {
				wrapper.callVoidWithSignature(callee, arguments, callSignature, callReceiverType)
				wrapper.cur.RetVoid()
			} else {
				resultClass, _ := scalar(signature.Results().At(0).Type())
				result := wrapper.callWithSignature(resultClass, callee, arguments, callSignature, callReceiverType)
				wrapper.returnValue(result, signature.Results().At(0).Type())
			}
			wrapper.cur = next
		}
		wrapper.cur.CallVoid(function.Sym("abort", 0))
		wrapper.cur.Hlt()
	}
}

type interfaceMethodCandidate struct {
	function      *types.Func
	dynamicType   types.Type
	interfacePath []int
}

func interfaceMethodCandidates(g *gen, reachable []functionDecl, method *types.Func, interfaceType *types.Interface) []interfaceMethodCandidate {
	seen := make(map[string]bool)
	reachableMethods := make(map[*types.Func]bool)
	for _, declaration := range reachable {
		function, ok := declaration.info.Defs[declaration.decl.Name].(*types.Func)
		if ok {
			reachableMethods[function] = true
		}
	}
	var candidates []interfaceMethodCandidate
	add := func(function *types.Func, dynamicType types.Type, interfacePath []int) {
		key := g.functionSymbol(function) + "|" + goTypeKey(dynamicType)
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, interfaceMethodCandidate{
			function:      function,
			dynamicType:   dynamicType,
			interfacePath: append([]int(nil), interfacePath...),
		})
	}
	for _, dynamicType := range g.dynamicTypes {
		if _, isInterface := dynamicType.Underlying().(*types.Interface); isInterface {
			continue
		}
		if !types.Implements(dynamicType, interfaceType) {
			continue
		}
		object, index, _ := types.LookupFieldOrMethod(dynamicType, true, method.Pkg(), method.Name())
		function, ok := object.(*types.Func)
		if !ok {
			continue
		}
		if reachableMethods[function] {
			add(function, dynamicType, nil)
			continue
		}
		candidateSignature, _ := function.Type().(*types.Signature)
		if function == method && candidateSignature != nil && candidateSignature.Recv() != nil && len(index) > 1 {
			if _, embeddedInterface := candidateSignature.Recv().Type().Underlying().(*types.Interface); embeddedInterface {
				add(function, dynamicType, index)
			}
		}
	}
	for _, declaration := range reachable {
		candidate, ok := declaration.info.Defs[declaration.decl.Name].(*types.Func)
		if !ok || candidate.Name() != method.Name() {
			continue
		}
		signature, ok := candidate.Type().(*types.Signature)
		if !ok || signature.Recv() == nil {
			continue
		}
		receiverType := signature.Recv().Type()
		if types.Implements(receiverType, interfaceType) {
			add(candidate, receiverType, nil)
		}
		if _, pointerReceiver := receiverType.(*types.Pointer); !pointerReceiver {
			pointerType := types.NewPointer(receiverType)
			if types.Implements(pointerType, interfaceType) {
				add(candidate, pointerType, nil)
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := g.functionSymbol(candidates[i].function) + "|" + goTypeKey(candidates[i].dynamicType)
		right := g.functionSymbol(candidates[j].function) + "|" + goTypeKey(candidates[j].dynamicType)
		return left < right
	})
	return candidates
}

func goTypeKey(valueType types.Type) string {
	key := types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Path()
	})
	return appendLocalTypeIdentities(key, valueType)
}

func appendLocalTypeIdentities(key string, valueType types.Type) string {
	var identities strings.Builder
	active := make(map[types.Type]bool)
	var visit func(types.Type, string)
	visit = func(current types.Type, path string) {
		if current == nil {
			return
		}
		current = types.Unalias(current)
		if active[current] {
			return
		}
		active[current] = true
		defer delete(active, current)
		switch current := current.(type) {
		case *types.Array:
			visit(current.Elem(), path+".element")
		case *types.Slice:
			visit(current.Elem(), path+".element")
		case *types.Pointer:
			visit(current.Elem(), path+".element")
		case *types.Map:
			visit(current.Key(), path+".key")
			visit(current.Elem(), path+".element")
		case *types.Chan:
			visit(current.Elem(), path+".element")
		case *types.Struct:
			for index := 0; index < current.NumFields(); index++ {
				visit(current.Field(index).Type(), fmt.Sprintf("%s.field%d", path, index))
			}
		case *types.Signature:
			visit(current.Params(), path+".parameters")
			visit(current.Results(), path+".results")
		case *types.Tuple:
			for index := 0; index < current.Len(); index++ {
				visit(current.At(index).Type(), fmt.Sprintf("%s.item%d", path, index))
			}
		case *types.Interface:
			for index := 0; index < current.NumExplicitMethods(); index++ {
				visit(current.ExplicitMethod(index).Type(), fmt.Sprintf("%s.method%d", path, index))
			}
			for index := 0; index < current.NumEmbeddeds(); index++ {
				visit(current.EmbeddedType(index), fmt.Sprintf("%s.embedded%d", path, index))
			}
		case *types.Union:
			for index := 0; index < current.Len(); index++ {
				visit(current.Term(index).Type(), fmt.Sprintf("%s.term%d", path, index))
			}
		case *types.Named:
			appendLocalTypeIdentity(&identities, path, current.Obj())
			for index := 0; index < current.TypeArgs().Len(); index++ {
				visit(current.TypeArgs().At(index), fmt.Sprintf("%s.argument%d", path, index))
			}
		case *types.TypeParam:
			appendLocalTypeIdentity(&identities, path, current.Obj())
		}
	}

	visit(valueType, "type")
	if identities.Len() == 0 {
		return key
	}
	return key + identities.String()
}

func appendLocalTypeIdentity(builder *strings.Builder, path string, object *types.TypeName) {
	if object == nil || object.Pkg() == nil || object.Parent() == object.Pkg().Scope() {
		return
	}
	if object.Pos() == token.NoPos {
		return
	}
	fmt.Fprintf(builder, "|%s=%s.%s@%d", path, object.Pkg().Path(), object.Name(), object.Pos())
}

func addRuntimeInitTask(mod *ir.Module, declarations []functionDecl, initSymbols map[*types.Func]string) error {
	if len(declarations) == 0 {
		return nil
	}

	taskItems := []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0, int64(len(declarations))}}}
	for _, declaration := range declarations {
		object, ok := declaration.info.Defs[declaration.decl.Name].(*types.Func)
		if !ok || initSymbols[object] == "" {
			return fmt.Errorf("goc: runtime init function has no unique symbol")
		}
		taskItems = append(taskItems, ir.DataItem{Sub: ir.SubL, Sym: initSymbols[object]})
	}

	const taskName = ".goc.runtime.inittask.runtime"
	const backingName = ".goc.runtime.inittasks.backing"
	mod.Data = append(mod.Data,
		&ir.Data{Name: taskName, Align: 8, Items: taskItems},
		&ir.Data{
			Name:         backingName,
			Align:        8,
			Items:        []ir.DataItem{{Sub: ir.SubL, Sym: taskName}},
			PointerWords: []int{0},
		},
	)

	for _, data := range mod.Data {
		if data.Name != "runtime.runtime_inittasks" && data.Name != "runtime.runtime_inittasks.descriptor" {
			continue
		}
		data.Items = []ir.DataItem{
			{Sub: ir.SubL, Sym: backingName},
			{Sub: ir.SubL, Ints: []int64{1, 1}},
		}
		data.PointerWords = []int{0}
		return nil
	}
	return fmt.Errorf("goc: runtime init-task slice was not emitted")
}

// addMemoryHelpers keeps compiler-generated aggregate operations inside the
// cg12 calling convention. In particular, Go's ARM64 frame layout reserves a
// word below SP for its frame-pointer chain, which arbitrary libc routines are
// not required to preserve.
func addMemoryHelpers(mod *ir.Module, runtimeAllocation bool) {
	copyFunction := mod.NewFunc("goc_memcpy", ir.ClsP)
	copyFunction.NoSplit = true
	destination := copyFunction.Param("destination", ir.ClsP)
	source := copyFunction.Param("source", ir.ClsP)
	copySize := copyFunction.Param("size", ir.ClsL)
	copyEntry := copyFunction.Entry()
	copyLoop := copyFunction.NewBlock("loop")
	copyBody := copyFunction.NewBlock("body")
	copyDone := copyFunction.NewBlock("done")
	copyEntry.Goto(copyLoop)
	copyIndex := copyLoop.Phi(ir.ClsL, ir.PhiEdge{From: copyEntry, Val: copyFunction.Long(0)})
	copyLoop.Jnz(copyLoop.Cmp(ir.CmpUlt, ir.ClsL, copyIndex, copySize), copyBody, copyDone)
	sourceAddress := copyBody.Add(ir.ClsP, source, copyIndex)
	destinationAddress := copyBody.Add(ir.ClsP, destination, copyIndex)
	copyBody.StoreSub(ir.SubUB, copyBody.LoadSub(ir.ClsW, ir.SubUB, sourceAddress), destinationAddress)
	nextCopyIndex := copyBody.Add(ir.ClsL, copyIndex, copyFunction.Long(1))
	copyBody.Goto(copyLoop)
	copyLoop.Phis[0].Add(copyBody, nextCopyIndex)
	copyDone.Ret(destination)

	moveFunction := mod.NewFunc("goc_memmove", ir.ClsP)
	moveFunction.NoSplit = true
	moveDestination := moveFunction.Param("destination", ir.ClsP)
	moveSource := moveFunction.Param("source", ir.ClsP)
	moveSize := moveFunction.Param("size", ir.ClsL)
	moveEntry := moveFunction.Entry()
	moveForward := moveFunction.NewBlock("forward")
	moveForwardBody := moveFunction.NewBlock("forward_body")
	moveBackward := moveFunction.NewBlock("backward")
	moveBackwardBody := moveFunction.NewBlock("backward_body")
	moveDone := moveFunction.NewBlock("done")
	moveEntry.Jnz(moveEntry.Cmp(ir.CmpUlt, ir.ClsP, moveDestination, moveSource), moveForward, moveBackward)
	forwardIndex := moveForward.Phi(ir.ClsL, ir.PhiEdge{From: moveEntry, Val: moveFunction.Long(0)})
	moveForward.Jnz(moveForward.Cmp(ir.CmpUlt, ir.ClsL, forwardIndex, moveSize), moveForwardBody, moveDone)
	forwardSource := moveForwardBody.Add(ir.ClsP, moveSource, forwardIndex)
	forwardDestination := moveForwardBody.Add(ir.ClsP, moveDestination, forwardIndex)
	moveForwardBody.StoreSub(ir.SubUB, moveForwardBody.LoadSub(ir.ClsW, ir.SubUB, forwardSource), forwardDestination)
	nextForwardIndex := moveForwardBody.Add(ir.ClsL, forwardIndex, moveFunction.Long(1))
	moveForwardBody.Goto(moveForward)
	moveForward.Phis[0].Add(moveForwardBody, nextForwardIndex)
	backwardIndex := moveBackward.Phi(ir.ClsL, ir.PhiEdge{From: moveEntry, Val: moveSize})
	moveBackward.Jnz(moveBackward.Cmp(ir.CmpNe, ir.ClsL, backwardIndex, moveFunction.Long(0)), moveBackwardBody, moveDone)
	nextBackwardIndex := moveBackwardBody.Sub(ir.ClsL, backwardIndex, moveFunction.Long(1))
	backwardSource := moveBackwardBody.Add(ir.ClsP, moveSource, nextBackwardIndex)
	backwardDestination := moveBackwardBody.Add(ir.ClsP, moveDestination, nextBackwardIndex)
	moveBackwardBody.StoreSub(ir.SubUB, moveBackwardBody.LoadSub(ir.ClsW, ir.SubUB, backwardSource), backwardDestination)
	moveBackwardBody.Goto(moveBackward)
	moveBackward.Phis[0].Add(moveBackwardBody, nextBackwardIndex)
	moveDone.Ret(moveDestination)

	compareFunction := mod.NewFunc("goc_memcmp", ir.ClsW)
	compareFunction.NoSplit = true
	compareLeft := compareFunction.Param("left", ir.ClsP)
	compareRight := compareFunction.Param("right", ir.ClsP)
	compareSize := compareFunction.Param("size", ir.ClsL)
	compareEntry := compareFunction.Entry()
	compareLoop := compareFunction.NewBlock("loop")
	compareBody := compareFunction.NewBlock("body")
	compareUnequal := compareFunction.NewBlock("unequal")
	compareNext := compareFunction.NewBlock("next")
	compareEqual := compareFunction.NewBlock("equal")
	compareEntry.Goto(compareLoop)
	compareIndex := compareLoop.Phi(ir.ClsL, ir.PhiEdge{From: compareEntry, Val: compareFunction.Long(0)})
	compareLoop.Jnz(compareLoop.Cmp(ir.CmpUlt, ir.ClsL, compareIndex, compareSize), compareBody, compareEqual)
	leftByte := compareBody.LoadSub(ir.ClsW, ir.SubUB, compareBody.Add(ir.ClsP, compareLeft, compareIndex))
	rightByte := compareBody.LoadSub(ir.ClsW, ir.SubUB, compareBody.Add(ir.ClsP, compareRight, compareIndex))
	compareBody.Jnz(compareBody.Cmp(ir.CmpNe, ir.ClsW, leftByte, rightByte), compareUnequal, compareNext)
	compareUnequal.Ret(compareUnequal.Sub(ir.ClsW, leftByte, rightByte))
	nextCompareIndex := compareNext.Add(ir.ClsL, compareIndex, compareFunction.Long(1))
	compareNext.Goto(compareLoop)
	compareLoop.Phis[0].Add(compareNext, nextCompareIndex)
	compareEqual.Ret(compareFunction.Word(0))

	setFunction := mod.NewFunc("goc_memset", ir.ClsP)
	setFunction.NoSplit = true
	setDestination := setFunction.Param("destination", ir.ClsP)
	setValue := setFunction.Param("value", ir.ClsW)
	setSize := setFunction.Param("size", ir.ClsL)
	setEntry := setFunction.Entry()
	setLoop := setFunction.NewBlock("loop")
	setBody := setFunction.NewBlock("body")
	setDone := setFunction.NewBlock("done")
	setEntry.Goto(setLoop)
	setIndex := setLoop.Phi(ir.ClsL, ir.PhiEdge{From: setEntry, Val: setFunction.Long(0)})
	setLoop.Jnz(setLoop.Cmp(ir.CmpUlt, ir.ClsL, setIndex, setSize), setBody, setDone)
	setBody.StoreSub(ir.SubUB, setValue, setBody.Add(ir.ClsP, setDestination, setIndex))
	nextSetIndex := setBody.Add(ir.ClsL, setIndex, setFunction.Long(1))
	setBody.Goto(setLoop)
	setLoop.Phis[0].Add(setBody, nextSetIndex)
	setDone.Ret(setDestination)

	if runtimeAllocation {
		storePointerFunction := mod.NewFuncVoid("goc_storep")
		storePointerFunction.NoSplit = true
		storeAddress := storePointerFunction.Param("address", ir.ClsP)
		storeValue := storePointerFunction.Param("value", ir.ClsP)
		storeEntry := storePointerFunction.Entry()
		classifyAddress := storePointerFunction.NewBlock("classify_address")
		checkHeap := storePointerFunction.NewBlock("check_heap")
		storeDirect := storePointerFunction.NewBlock("direct")
		storeBarrier := storePointerFunction.NewBlock("barrier")
		addressValue := storeEntry.Copy(ir.ClsL, storeAddress)
		writeBarrierEnabled := storeEntry.Load(ir.ClsW, storePointerFunction.Sym("runtime.writeBarrier", 0))
		storeEntry.Jnz(writeBarrierEnabled, classifyAddress, storeDirect)

		inHeapOrStack := classifyAddress.Call(ir.ClsW, storePointerFunction.Sym("runtime.inHeapOrStack", 0), addressValue)
		classifyAddress.Jnz(inHeapOrStack, checkHeap, storeBarrier)

		inHeap := checkHeap.Call(ir.ClsW, storePointerFunction.Sym("runtime.inheap", 0), addressValue)
		checkHeap.Jnz(inHeap, storeBarrier, storeDirect)

		storeDirect.Store(storeValue, storeAddress)
		storeDirect.RetVoid()

		storeBarrier.CallVoid(storePointerFunction.Sym("runtime.atomicstorep", 0), storeAddress, storeValue)
		storeBarrier.RetVoid()
	}
}

func addFIPSRuntimeStubs(mod *ir.Module) {
	get := mod.NewFunc("crypto/internal/fips140.getIndicator", ir.ClsW)
	get.Entry().Ret(get.Word(0))

	set := mod.NewFuncVoid("crypto/internal/fips140.setIndicator")
	set.Param("indicator", ir.ClsW)
	set.Entry().RetVoid()
}

func addLegacyCryptoRuntimeStubs(mod *ir.Module) {
	enforced := mod.NewFunc("crypto/internal/fips140only.Enforced", ir.ClsW)
	enforced.Entry().Ret(enforced.Word(0))

	unreachable := mod.NewFuncVoid("crypto/internal/boring.Unreachable")
	unreachable.Entry().RetVoid()
}

type gen struct {
	fset                        *token.FileSet
	file                        *ast.File
	info                        *types.Info
	pkg                         *types.Package
	mod                         *ir.Module
	fn                          *ir.Func
	cur                         *ir.Block
	vars                        map[types.Object]ir.Ref
	globals                     map[types.Object]string
	globalLinkNames             map[types.Object]string
	breaks, continues           []*ir.Block
	seq                         int
	err                         error
	methodTargets               map[string]*types.Func
	resultSlot                  ir.Ref
	resultType                  types.Type
	aggregateResult             ir.Ref
	extraResultSlots            []ir.Ref
	extraResultTypes            []types.Type
	labels                      map[string]*ir.Block
	labeledBreaks               map[string]*ir.Block
	labeledContinues            map[string]*ir.Block
	deferSlots                  map[*ast.DeferStmt]ir.Ref
	deferFunctions              map[*ast.DeferStmt]ir.Ref
	deferOrder                  []*ast.DeferStmt
	deferActions                []*ast.DeferStmt
	runningDefers               bool
	parents                     map[ast.Node]ast.Node
	currentBody                 *ast.BlockStmt
	functionDecls               map[*types.Func]functionDecl
	noWriteBarrierFunctions     map[*types.Func]bool
	interfaceMethods            map[*types.Func]bool
	interfaceItabs              map[string]string
	dynamicTypes                []types.Type
	reachableGlobals            map[types.Object]bool
	filterGlobals               bool
	directValues                map[types.Object]bool
	emitRuntimeTables           bool
	runtimeAllocation           bool
	typeTags                    map[string]string
	runtimeTypes                map[string]types.Type
	goABITypes                  map[string]*ir.AggType
	linkNames                   map[*types.Func]string
	initSymbols                 map[*types.Func]string
	stackAddresses              map[uint32]bool
	heapCaptures                map[types.Object]ir.Ref
	escapingCaptures            map[types.Object]bool
	noWriteBarrier              bool
	dynamicInitializers         map[types.Object]globalInitializer
	dynamicInitializerGuards    map[types.Object]string
	dynamicInitializerFunctions map[types.Object]string
	initializingGlobals         map[types.Object]bool
	typeArguments               []types.Type
	functionName                string
	currentFunction             *types.Func
	nextCallUsesClosure         bool
}

type globalInitializer struct {
	expression ast.Expr
	info       *types.Info
	pkg        *types.Package
}

func collectLinkNames(files []*ast.File, info *types.Info, names map[*types.Func]string) {
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Doc == nil {
				continue
			}
			object, ok := info.Defs[function.Name].(*types.Func)
			if !ok {
				continue
			}
			for _, comment := range function.Doc.List {
				fields := strings.Fields(strings.TrimPrefix(comment.Text, "//"))
				if len(fields) == 3 && fields[0] == "go:linkname" {
					names[object] = fields[2]
				}
			}
		}
	}
}

func collectGlobalLinkNames(files []*ast.File, pkg *types.Package, names map[types.Object]string) {
	for _, file := range files {
		for _, commentGroup := range file.Comments {
			for _, comment := range commentGroup.List {
				fields := strings.Fields(strings.TrimPrefix(comment.Text, "//"))
				if len(fields) != 3 || fields[0] != "go:linkname" {
					continue
				}
				object := pkg.Scope().Lookup(fields[1])
				if _, ok := object.(*types.Var); ok {
					names[object] = fields[2]
				}
			}
		}
	}
}

func collectFunctionDeclarations(rootInfo *types.Info, rootPkg *types.Package, rootFiles []*ast.File, units map[string]*sourceUnit) map[*types.Func]functionDecl {
	declarations := make(map[*types.Func]functionDecl)
	collect := func(files []*ast.File, info *types.Info, pkg *types.Package) {
		for _, file := range files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok {
					continue
				}
				object, ok := info.Defs[function.Name].(*types.Func)
				if ok {
					declarations[object] = functionDecl{decl: function, info: info, pkg: pkg}
				}
			}
		}
	}
	collect(rootFiles, rootInfo, rootPkg)
	for _, unit := range units {
		collect(unit.files, unit.info, unit.pkg)
	}
	return declarations
}

func collectDynamicInitializers(
	rootFiles []*ast.File,
	rootInfo *types.Info,
	rootPackage *types.Package,
	units map[string]*sourceUnit,
	packageGlobals map[string]map[types.Object]string,
) map[types.Object]globalInitializer {
	initializers := make(map[types.Object]globalInitializer)
	collect := func(files []*ast.File, info *types.Info, pkg *types.Package) {
		globals := packageGlobals[pkg.Path()]
		for _, file := range files {
			for _, declaration := range file.Decls {
				global, ok := declaration.(*ast.GenDecl)
				if !ok || global.Tok != token.VAR {
					continue
				}
				for _, specification := range global.Specs {
					values := specification.(*ast.ValueSpec)
					if len(values.Values) != len(values.Names) {
						continue
					}
					for index, name := range values.Names {
						object := info.Defs[name]
						expression := values.Values[index]
						if object == nil || globals[object] == "" || info.Types[expression].Value != nil || staticallyInitializedGlobal(expression, object.Type(), info) {
							continue
						}
						initializers[object] = globalInitializer{
							expression: expression,
							info:       info,
							pkg:        pkg,
						}
					}
				}
			}
		}
	}
	collect(rootFiles, rootInfo, rootPackage)
	for _, unit := range units {
		collect(unit.files, unit.info, unit.pkg)
	}
	return initializers
}

func dynamicInitializerFunctionSymbols(initializers map[types.Object]globalInitializer) map[types.Object]string {
	objects := make([]types.Object, 0, len(initializers))
	for object := range initializers {
		objects = append(objects, object)
	}
	sort.Slice(objects, func(left, right int) bool {
		leftPackage := ""
		if objects[left].Pkg() != nil {
			leftPackage = objects[left].Pkg().Path()
		}
		rightPackage := ""
		if objects[right].Pkg() != nil {
			rightPackage = objects[right].Pkg().Path()
		}
		if leftPackage != rightPackage {
			return leftPackage < rightPackage
		}
		return objects[left].Name() < objects[right].Name()
	})

	symbols := make(map[types.Object]string, len(objects))
	for index, object := range objects {
		packagePath := "builtin"
		if object.Pkg() != nil {
			packagePath = object.Pkg().Path()
		}
		symbols[object] = fmt.Sprintf(".goc.global.initfunc.%d.%s.%s", index, packagePath, object.Name())
	}
	return symbols
}

func generateDynamicInitializerFunctions(base *gen, initializers map[types.Object]globalInitializer) error {
	objects := make([]types.Object, 0, len(initializers))
	for object := range initializers {
		objects = append(objects, object)
	}
	sort.Slice(objects, func(left, right int) bool {
		return base.dynamicInitializerFunctions[objects[left]] < base.dynamicInitializerFunctions[objects[right]]
	})

	for _, object := range objects {
		initializer := initializers[object]
		generator := *base
		generator.info = initializer.info
		generator.pkg = initializer.pkg
		generator.globals = base.globals
		generator.fn = base.mod.NewFuncVoid(base.dynamicInitializerFunctions[object])
		generator.cur = generator.fn.Entry()
		generator.vars = make(map[types.Object]ir.Ref)
		generator.directValues = make(map[types.Object]bool)
		generator.stackAddresses = make(map[uint32]bool)
		generator.heapCaptures = make(map[types.Object]ir.Ref)
		generator.escapingCaptures = make(map[types.Object]bool)
		generator.labels = make(map[string]*ir.Block)
		generator.labeledBreaks = make(map[string]*ir.Block)
		generator.labeledContinues = make(map[string]*ir.Block)
		generator.deferSlots = make(map[*ast.DeferStmt]ir.Ref)
		generator.deferFunctions = make(map[*ast.DeferStmt]ir.Ref)
		generator.deferOrder = nil
		generator.deferActions = nil
		generator.breaks = nil
		generator.continues = nil
		generator.resultSlot = ir.R
		generator.aggregateResult = ir.R
		generator.extraResultSlots = nil
		generator.extraResultTypes = nil
		generator.parents = astParents(initializer.expression)
		generator.currentBody = nil
		generator.initializingGlobals = make(map[types.Object]bool)
		generator.functionName = base.dynamicInitializerFunctions[object]
		generator.currentFunction = nil

		generator.emitDynamicGlobalInitializer(object, initializer)
		if generator.err != nil {
			return generator.err
		}
		if generator.live() {
			generator.cur.RetVoid()
		}
	}
	return nil
}

func staticallyInitializedGlobal(expression ast.Expr, targetType types.Type, info *types.Info) bool {
	if _, isMap := targetType.Underlying().(*types.Map); isMap {
		return false
	}
	if slice, isSlice := targetType.Underlying().(*types.Slice); isSlice {
		if element, isBasic := slice.Elem().Underlying().(*types.Basic); isBasic && element.Kind() == types.Uint8 {
			conversion, isConversion := expression.(*ast.CallExpr)
			if isConversion && len(conversion.Args) == 1 && info.Types[conversion.Fun].IsType() {
				value := info.Types[conversion.Args[0]].Value
				if value != nil && value.Kind() == constant.String {
					return true
				}
			}
		}
	}

	if _, isInterface := targetType.Underlying().(*types.Interface); !isInterface {
		return staticallyInitialized(expression, info)
	}

	// globalInterface currently emits static data only for constant string
	// conversions. Other interface initializers need executable code to build
	// the (dynamic type, data) pair, even when their concrete value is itself a
	// static composite literal.
	conversion, ok := expression.(*ast.CallExpr)
	if !ok || len(conversion.Args) != 1 {
		return false
	}
	sourceType := info.Types[expression].Type
	basic, ok := sourceType.Underlying().(*types.Basic)
	if !ok || basic.Kind() != types.String {
		return false
	}
	value := info.Types[conversion.Args[0]].Value
	return value != nil && value.Kind() == constant.String
}

func staticallyInitialized(expression ast.Expr, info *types.Info) bool {
	root := expression
	if address, ok := expression.(*ast.UnaryExpr); ok && address.Op == token.AND {
		root = address.X
	}
	if _, literal := root.(*ast.CompositeLit); !literal {
		return false
	}

	static := true
	ast.Inspect(root, func(node ast.Node) bool {
		if !static {
			return false
		}
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CompositeLit:
			valueType := info.Types[node].Type
			if valueType != nil {
				if _, isMap := valueType.Underlying().(*types.Map); isMap {
					static = false
					return false
				}
			}
		case *ast.UnaryExpr:
			if node.Op == token.AND {
				return false
			}
		case *ast.CallExpr:
			if info.Types[node].Value == nil {
				static = false
			}
			return false
		case *ast.Ident:
			if variable, ok := info.Uses[node].(*types.Var); ok && !variable.IsField() {
				static = false
				return false
			}
		}
		return true
	})
	return static
}

func collectNoWriteBarrierFunctions(declarations map[*types.Func]functionDecl) map[*types.Func]bool {
	disabled := make(map[*types.Func]bool)
	var recursive []*types.Func
	for function, declaration := range declarations {
		if hasCompilerDirective(declaration.decl, "go:nowritebarrier") || hasCompilerDirective(declaration.decl, "go:nowritebarrierrec") {
			disabled[function] = true
		}
		if hasCompilerDirective(declaration.decl, "go:nowritebarrierrec") {
			recursive = append(recursive, function)
		}
	}
	for len(recursive) != 0 {
		function := recursive[len(recursive)-1]
		recursive = recursive[:len(recursive)-1]
		declaration := declarations[function]
		if declaration.decl.Body == nil {
			continue
		}
		ast.Inspect(declaration.decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var callee *types.Func
			switch target := call.Fun.(type) {
			case *ast.Ident:
				callee, _ = declaration.info.Uses[target].(*types.Func)
			case *ast.SelectorExpr:
				callee, _ = declaration.info.Uses[target.Sel].(*types.Func)
			}
			if callee != nil && declarations[callee].decl != nil && !disabled[callee] {
				disabled[callee] = true
				recursive = append(recursive, callee)
			}
			return true
		})
	}
	return disabled
}

func astParents(root ast.Node) map[ast.Node]ast.Node {
	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) != 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parents
}

// nonEscapingAddress recognizes address expressions whose pointer is consumed
// only as the storage for an immediately reinterpreted or selected value. The
// heap-allocation IR pass handles the remaining, dataflow-dependent cases.
func (g *gen) nonEscapingAddress(address *ast.UnaryExpr) bool {
	if g.assignedResultDoesNotEscape(address) {
		return true
	}
	var current ast.Node = address
	for {
		parent := g.parents[current]
		switch parent := parent.(type) {
		case *ast.ParenExpr:
			current = parent
		case *ast.CallExpr:
			if len(parent.Args) != 1 || parent.Args[0] != current || !g.info.Types[parent.Fun].IsType() {
				return false
			}
			current = parent
		case *ast.StarExpr:
			return parent.X == current
		case *ast.SelectorExpr:
			selection := g.info.Selections[parent]
			return parent.X == current && selection != nil && selection.Kind() == types.FieldVal
		default:
			return false
		}
	}
}

// addressEscapesFunction reports address expressions that the frontend must
// promote before emitting the function. The runtime package keeps its local
// bootstrap storage because promoting it through runtime.newobject would make
// the allocator recursively depend on itself.
func (g *gen) addressEscapesFunction(address *ast.UnaryExpr) bool {
	return g.addressEscapesWithin(
		address,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
}

func (g *gen) addressEscapesWithin(
	address *ast.UnaryExpr,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	var current ast.Node = address
	for {
		parent := parents[current]
		switch parent := parent.(type) {
		case *ast.ParenExpr:
			current = parent
		case *ast.KeyValueExpr:
			if parent.Value != current {
				return false
			}
			current = parent
		case *ast.CompositeLit:
			current = parent
		case *ast.CallExpr:
			if len(parent.Args) == 1 && parent.Args[0] == current && info.Types[parent.Fun].IsType() {
				convertedType := info.Types[parent].Type
				if basic, ok := convertedType.Underlying().(*types.Basic); ok && basic.Kind() == types.Uintptr {
					return false
				}
				current = parent
				continue
			}
			if g.pkg.Path() == "runtime" {
				return false
			}
			argumentIndex := -1
			for index, argument := range parent.Args {
				if argument == current {
					argumentIndex = index
					break
				}
			}
			if argumentIndex < 0 {
				return false
			}
			function := calledFunction(parent.Fun, info)
			if function != nil && g.parameterDoesNotEscape(function, argumentIndex, checking) {
				return false
			}
			return true
		case *ast.AssignStmt:
			return !g.assignedNodeDoesNotEscapeWithin(current, info, parents, body, checking)
		case *ast.ReturnStmt:
			return true
		default:
			return false
		}
	}
}

func (g *gen) fixedSliceCapacity(call *ast.CallExpr) (int64, bool) {
	capacityExpression := call.Args[1]
	if len(call.Args) == 3 {
		capacityExpression = call.Args[2]
	}
	value := g.info.Types[capacityExpression].Value
	if value == nil {
		return 0, false
	}
	capacity, ok := constant.Int64Val(constant.ToInt(value))
	if !ok || capacity < 0 {
		return 0, false
	}
	return capacity, true
}

func (g *gen) makeResultDoesNotEscape(call *ast.CallExpr) bool {
	return g.assignedResultDoesNotEscape(call)
}

func (g *gen) assignedResultDoesNotEscape(expression ast.Expr) bool {
	return g.assignedNodeDoesNotEscape(expression)
}

func (g *gen) assignedNodeDoesNotEscape(expression ast.Node) bool {
	return g.assignedNodeDoesNotEscapeWithin(
		expression,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
}

func (g *gen) assignedNodeDoesNotEscapeWithin(
	expression ast.Node,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	assignment, ok := parents[expression].(*ast.AssignStmt)
	if !ok {
		return false
	}
	index := -1
	for candidate, rightHandSide := range assignment.Rhs {
		if rightHandSide == expression {
			index = candidate
			break
		}
	}
	if index < 0 || index >= len(assignment.Lhs) {
		return false
	}
	identifier, ok := assignment.Lhs[index].(*ast.Ident)
	if !ok {
		return false
	}
	object := info.Defs[identifier]
	if object == nil {
		object = info.Uses[identifier]
	}
	if object == nil || object.Pkg() == nil || body == nil {
		return false
	}
	return g.objectDoesNotEscape(object, info, parents, body, checking)
}

// valueDoesNotEscape recognizes temporary values whose backing storage is
// consumed entirely within the current function. This is particularly
// important for slice literals used directly by range: their backing arrays
// are ordinary stack temporaries in Go, and allocating one on every range
// iteration can make runtime code allocate while scanning the stack.
func (g *gen) valueDoesNotEscape(expression ast.Expr) bool {
	var current ast.Node = expression
	for {
		if g.assignedNodeDoesNotEscape(current) {
			return true
		}
		parent := g.parents[current]
		switch parent := parent.(type) {
		case *ast.ParenExpr:
			current = parent
		case *ast.KeyValueExpr:
			if parent.Value != current {
				return false
			}
			current = parent
		case *ast.CompositeLit:
			current = parent
		case *ast.UnaryExpr:
			if parent.Op != token.AND || parent.X != current {
				return false
			}
			current = parent
		case *ast.SliceExpr:
			if parent.X != current {
				return false
			}
			current = parent
		case *ast.RangeStmt:
			return parent.X == current
		case *ast.CallExpr:
			argumentIndex := -1
			for index, argument := range parent.Args {
				if argument == current {
					argumentIndex = index
					break
				}
			}
			if argumentIndex < 0 {
				return false
			}
			if calledIdentifier, ok := parent.Fun.(*ast.Ident); ok {
				if builtin, ok := g.info.Uses[calledIdentifier].(*types.Builtin); ok {
					switch builtin.Name() {
					case "len", "cap", "copy":
						return true
					}
				}
			}
			if g.info.Types[parent.Fun].IsType() {
				current = parent
				continue
			}
			function := calledFunction(parent.Fun, g.info)
			return function != nil && g.parameterDoesNotEscape(function, argumentIndex, make(map[parameterKey]bool))
		default:
			return false
		}
	}
}

type parameterKey struct {
	function *types.Func
	index    int
}

func (g *gen) objectDoesNotEscape(object types.Object, info *types.Info, parents map[ast.Node]ast.Node, body *ast.BlockStmt, checking map[parameterKey]bool) bool {
	escaped := false
	ast.Inspect(body, func(node ast.Node) bool {
		if escaped {
			return false
		}
		literal, isLiteral := node.(*ast.FuncLit)
		if isLiteral && g.functionLiteralEscapesWithin(literal, info, parents, body, checking) {
			ast.Inspect(literal.Body, func(literalNode ast.Node) bool {
				identifier, ok := literalNode.(*ast.Ident)
				if ok && info.Uses[identifier] == object {
					escaped = true
					return false
				}
				return !escaped
			})
			if escaped {
				return false
			}
		}
		identifier, ok := node.(*ast.Ident)
		if !ok || info.Uses[identifier] != object {
			return true
		}
		if !g.nonEscapingObjectUse(identifier, info, parents, body, checking) {
			escaped = true
		}
		return true
	})
	return !escaped
}

func addressedExpression(expression ast.Node, parents map[ast.Node]ast.Node) *ast.UnaryExpr {
	current := expression
	for {
		parent := parents[current]
		if parenthesized, ok := parent.(*ast.ParenExpr); ok && parenthesized.X == current {
			current = parenthesized
			continue
		}
		address, ok := parent.(*ast.UnaryExpr)
		if !ok || address.Op != token.AND || address.X != current {
			return nil
		}
		return address
	}
}

func (g *gen) nonEscapingObjectUse(
	identifier *ast.Ident,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	parent := parents[identifier]
	switch parent := parent.(type) {
	case *ast.IndexExpr:
		if parent.X != identifier {
			return false
		}
		address := addressedExpression(parent, parents)
		return address == nil || !g.addressEscapesWithin(address, info, parents, body, checking)
	case *ast.SliceExpr:
		return parent.X == identifier
	case *ast.SelectorExpr:
		return parent.X == identifier
	case *ast.StarExpr:
		return parent.X == identifier
	case *ast.RangeStmt:
		if parent.X != identifier {
			return false
		}
		valueIdentifier, hasValue := parent.Value.(*ast.Ident)
		if !hasValue || valueIdentifier.Name == "_" {
			return true
		}
		valueObject := info.Defs[valueIdentifier]
		if valueObject == nil {
			valueObject = info.Uses[valueIdentifier]
		}
		if valueObject == nil {
			return true
		}
		return g.objectDoesNotEscape(valueObject, info, parents, body, checking)
	case *ast.BinaryExpr:
		return parent.Op == token.EQL || parent.Op == token.NEQ
	case *ast.AssignStmt:
		object := info.Uses[identifier]
		if object == nil {
			return false
		}
		basic, isBasic := object.Type().Underlying().(*types.Basic)
		if !isBasic || basic.Kind() != types.UnsafePointer {
			return false
		}
		for _, leftHandSide := range parent.Lhs {
			if leftHandSide == identifier {
				return true
			}
		}
		return false
	case *ast.CallExpr:
		if _, asynchronous := parents[parent].(*ast.GoStmt); asynchronous {
			return false
		}
		if parent.Fun == identifier {
			return true
		}
		argumentIndex := -1
		for index, argument := range parent.Args {
			if argument == identifier {
				argumentIndex = index
				break
			}
		}
		if argumentIndex < 0 {
			return false
		}
		if calledIdentifier, ok := parent.Fun.(*ast.Ident); ok {
			if builtin, ok := info.Uses[calledIdentifier].(*types.Builtin); ok {
				switch builtin.Name() {
				case "len", "cap", "copy":
					return true
				}
			}
		}
		if info.Types[parent.Fun].IsType() {
			convertedType := info.Types[parent].Type
			basic, ok := convertedType.Underlying().(*types.Basic)
			return ok && basic.Kind() == types.Uintptr
		}
		function := calledFunction(parent.Fun, info)
		return function != nil && g.parameterDoesNotEscape(function, argumentIndex, checking)
	default:
		return false
	}
}

func calledFunction(expression ast.Expr, info *types.Info) *types.Func {
	switch expression := expression.(type) {
	case *ast.Ident:
		function, _ := info.Uses[expression].(*types.Func)
		return function
	case *ast.SelectorExpr:
		function, _ := info.Uses[expression.Sel].(*types.Func)
		return function
	default:
		return nil
	}
}

func (g *gen) parameterDoesNotEscape(function *types.Func, index int, checking map[parameterKey]bool) bool {
	declaration, ok := g.functionDecls[function]
	if !ok {
		return false
	}
	signature := function.Type().(*types.Signature)
	if index < 0 || index >= signature.Params().Len() {
		return false
	}
	if declaration.decl.Body == nil {
		return hasCompilerDirective(declaration.decl, "go:noescape")
	}
	key := parameterKey{function: function, index: index}
	if checking[key] {
		return false
	}
	checking[key] = true
	defer delete(checking, key)
	parents := astParents(declaration.decl.Body)
	return g.objectDoesNotEscape(signature.Params().At(index), declaration.info, parents, declaration.decl.Body, checking)
}

func constInt(v constant.Value) int64 {
	if v.Kind() == constant.Bool {
		if constant.BoolVal(v) {
			return 1
		}
		return 0
	}
	i, ok := constant.Int64Val(constant.ToInt(v))
	if ok {
		return i
	}
	u, _ := constant.Uint64Val(constant.ToInt(v))
	return int64(u)
}

func (g *gen) globalDecl(gd *ast.GenDecl) {
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		for i, id := range vs.Names {
			obj := g.info.Defs[id]
			if g.filterGlobals && !g.reachableGlobals[obj] {
				continue
			}
			if g.runtimeAllocation && g.pkg.Path() == "runtime" && id.Name == "lastmoduledatap" {
				name := g.pkg.Path() + "." + id.Name
				g.mod.Data = append(g.mod.Data, &ir.Data{
					Name:         name,
					Align:        8,
					Items:        []ir.DataItem{{Sub: ir.SubL, Sym: "runtime.firstmoduledata"}},
					PointerWords: []int{0},
				})
				g.globals[obj] = name
				continue
			}
			if _, ok := obj.Type().Underlying().(*types.Interface); ok {
				g.globalInterface(id, obj, vs, i)
				g.markDataPointerWords(g.pkg.Path()+"."+id.Name, obj.Type())
				continue
			}
			if basic, ok := obj.Type().Underlying().(*types.Basic); ok && basic.Kind() == types.String {
				g.globalString(id, obj, vs, i)
				name := g.pkg.Path() + "." + id.Name
				g.markDataPointerCell(name)
				g.markDataPointerWords(name+".descriptor", obj.Type())
				continue
			}
			if array, ok := obj.Type().Underlying().(*types.Array); ok {
				g.globalArray(id, obj, array, vs, i)
				g.markDataPointerWords(g.pkg.Path()+"."+id.Name, obj.Type())
				if g.err != nil {
					return
				}
				continue
			}
			if _, ok := obj.Type().Underlying().(*types.Struct); ok {
				g.globalStruct(id, obj, vs, i)
				g.markDataPointerWords(g.pkg.Path()+"."+id.Name, obj.Type())
				if g.err != nil {
					return
				}
				continue
			}
			if slice, ok := obj.Type().Underlying().(*types.Slice); ok {
				g.globalSlice(id, obj, slice, vs, i)
				name := g.pkg.Path() + "." + id.Name
				if g.runtimeAllocation {
					g.markDataPointerWords(name, obj.Type())
				} else {
					g.markDataPointerCell(name)
					g.markDataPointerWords(name+".descriptor", obj.Type())
				}
				continue
			}
			cls, ok := scalar(obj.Type())
			if !ok {
				continue
			}
			name := g.pkg.Path() + "." + id.Name
			linkedName := g.globalLinkNames[obj]
			if linkedName != "" {
				name = linkedName
			}
			d := &ir.Data{Name: name, Align: 8, Linkage: ir.Linkage{Export: ast.IsExported(id.Name) || linkedName != ""}}
			var v int64
			if i < len(vs.Values) {
				initializer := vs.Values[i]
				tv := g.info.Types[initializer]
				if g.runtimeAllocation && g.pkg.Path() == "runtime" && id.Name == "maxstackceiling" {
					v = 1 << 20
				} else if tv.Value == nil {
					if address, ok := initializer.(*ast.UnaryExpr); ok && address.Op == token.AND {
						if literal, ok := address.X.(*ast.CompositeLit); ok {
							pointer, pointerOK := obj.Type().Underlying().(*types.Pointer)
							structure, structOK := pointer.Elem().Underlying().(*types.Struct)
							staticInitializer := staticallyInitializedGlobal(initializer, obj.Type(), g.info)
							if pointerOK && structOK && staticInitializer {
								backingName := name + ".value"
								g.mod.Data = append(g.mod.Data,
									&ir.Data{Name: backingName, Align: 8, Items: g.staticStructItems(backingName, structure, literal), PointerWords: pointerWordIndices(structure)},
									d,
								)
								d.Items = []ir.DataItem{{Sub: ir.SubL, Sym: backingName}}
								g.globals[obj] = name
								continue
							}
						}
						target, identifier := address.X.(*ast.Ident)
						if identifier {
							targetObject := g.info.Uses[target]
							if targetObject != nil && targetObject.Pkg() != nil {
								targetName := targetObject.Pkg().Path() + "." + targetObject.Name()
								if linkedName := g.globalLinkNames[targetObject]; linkedName != "" {
									targetName = linkedName
								}
								d.Items = []ir.DataItem{{Sub: ir.SubL, Sym: targetName}}
								g.mod.Data = append(g.mod.Data, d)
								g.globals[obj] = name
								continue
							}
						}
					}
				} else {
					v = constInt(tv.Value)
				}
			}
			if cls == ir.ClsL || cls == ir.ClsP {
				d.Items = []ir.DataItem{{Sub: ir.SubL, Ints: []int64{v}}}
			} else {
				d.Align = 4
				d.Items = []ir.DataItem{{Sub: ir.SubW, Ints: []int64{v}}}
			}
			d.PointerWords = pointerWordIndices(obj.Type())
			g.mod.Data = append(g.mod.Data, d)
			g.globals[obj] = name
		}
	}
}

func (g *gen) globalInterface(id *ast.Ident, object types.Object, spec *ast.ValueSpec, valueIndex int) {
	name := g.pkg.Path() + "." + id.Name
	emitZero := func() {
		g.mod.Data = append(g.mod.Data, &ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0, 0}}}})
		g.globals[object] = name
	}
	if valueIndex >= len(spec.Values) {
		emitZero()
		return
	}

	initializer := spec.Values[valueIndex]
	sourceType := g.info.Types[initializer].Type
	call, ok := initializer.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		emitZero()
		return
	}
	value := g.info.Types[call.Args[0]].Value
	basic, ok := sourceType.Underlying().(*types.Basic)
	if !ok || basic.Kind() != types.String || value == nil || value.Kind() != constant.String {
		emitZero()
		return
	}

	contents := constant.StringVal(value)
	textName := name + ".text"
	stringName := name + ".string"
	typeWord := g.staticInterfaceTypeWord(sourceType, object.Type())
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: textName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}},
		&ir.Data{Name: stringName, Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Sym: textName},
			{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
		}},
		&ir.Data{Name: name, Align: 8, Linkage: ir.Linkage{Export: ast.IsExported(id.Name)}, Items: []ir.DataItem{
			typeWord,
			{Sub: ir.SubL, Sym: stringName},
		}},
	)
	g.globals[object] = name
}

func (g *gen) globalString(id *ast.Ident, object types.Object, spec *ast.ValueSpec, valueIndex int) {
	name := g.pkg.Path() + "." + id.Name
	contents := ""
	if valueIndex < len(spec.Values) {
		value := g.info.Types[spec.Values[valueIndex]].Value
		if value != nil && value.Kind() == constant.String {
			contents = constant.StringVal(value)
		}
	}
	textName := name + ".text"
	descriptorName := name + ".descriptor"
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: textName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}},
		&ir.Data{Name: descriptorName, Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Sym: textName},
			{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
		}},
		&ir.Data{Name: name, Align: 8, Linkage: ir.Linkage{Export: ast.IsExported(id.Name)}, Items: []ir.DataItem{{Sub: ir.SubL, Sym: descriptorName}}},
	)
	g.globals[object] = name
}

func (g *gen) globalSlice(id *ast.Ident, object types.Object, slice *types.Slice, spec *ast.ValueSpec, valueIndex int) {
	name := g.pkg.Path() + "." + id.Name
	emitHeader := func(items []ir.DataItem) {
		headerName := name + ".descriptor"
		if g.runtimeAllocation {
			headerName = name
		}
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:  headerName,
			Align: 8,
			Items: items,
		})
		if !g.runtimeAllocation {
			g.mod.Data = append(g.mod.Data, &ir.Data{
				Name:  name,
				Align: 8,
				Items: []ir.DataItem{{Sub: ir.SubL, Sym: headerName}},
			})
		}
		g.globals[object] = name
	}
	emitZero := func() {
		emitHeader([]ir.DataItem{{Sub: ir.SubL, Ints: []int64{0, 0, 0}}})
	}
	if valueIndex >= len(spec.Values) {
		emitZero()
		return
	}
	initializer := spec.Values[valueIndex]
	if conversion, ok := initializer.(*ast.CallExpr); ok && len(conversion.Args) == 1 {
		value := g.info.Types[conversion.Args[0]].Value
		basic, byteElements := slice.Elem().Underlying().(*types.Basic)
		if byteElements && basic.Kind() == types.Uint8 && value != nil && value.Kind() == constant.String {
			contents := constant.StringVal(value)
			backingName := name + ".backing"
			g.mod.Data = append(g.mod.Data, &ir.Data{Name: backingName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}})
			emitHeader([]ir.DataItem{
				{Sub: ir.SubL, Sym: backingName},
				{Sub: ir.SubL, Ints: []int64{int64(len(contents)), int64(len(contents))}},
			})
			return
		}
	}
	if !staticallyInitializedGlobal(initializer, object.Type(), g.info) {
		emitZero()
		return
	}
	literal, ok := initializer.(*ast.CompositeLit)
	if !ok {
		emitZero()
		return
	}
	backingName := name + ".backing"
	if structure, ok := slice.Elem().Underlying().(*types.Struct); ok {
		items := make([]ir.DataItem, 0, len(literal.Elts)*structure.NumFields())
		for index, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			element, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(typeSize(slice.Elem()))})
				continue
			}
			elementName := fmt.Sprintf("%s.element.%d", backingName, index)
			items = append(items, g.staticStructItems(elementName, structure, element)...)
		}
		arrayType := types.NewArray(slice.Elem(), int64(len(literal.Elts)))
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:         backingName,
			Align:        int(typeAlign(slice.Elem())),
			Items:        items,
			PointerWords: pointerWordIndices(arrayType),
		})
		emitHeader([]ir.DataItem{
			{Sub: ir.SubL, Sym: backingName},
			{Sub: ir.SubL, Ints: []int64{int64(len(literal.Elts)), int64(len(literal.Elts))}},
		})
		return
	}
	if pointer, ok := slice.Elem().Underlying().(*types.Pointer); ok {
		if structure, ok := pointer.Elem().Underlying().(*types.Struct); ok {
			items := make([]ir.DataItem, 0, len(literal.Elts))
			for index, expression := range literal.Elts {
				var value *ast.CompositeLit
				switch expression := expression.(type) {
				case *ast.CompositeLit:
					value = expression
				case *ast.UnaryExpr:
					if expression.Op == token.AND {
						value, _ = expression.X.(*ast.CompositeLit)
					}
				}
				if value == nil {
					items = append(items, ir.DataItem{Sub: ir.SubL, Ints: []int64{0}})
					continue
				}
				elementName := fmt.Sprintf("%s.element.%d", backingName, index)
				g.mod.Data = append(g.mod.Data, &ir.Data{
					Name:         elementName,
					Align:        8,
					Items:        g.staticStructItems(elementName, structure, value),
					PointerWords: pointerWordIndices(structure),
				})
				items = append(items, ir.DataItem{Sub: ir.SubL, Sym: elementName})
			}
			g.mod.Data = append(g.mod.Data, &ir.Data{Name: backingName, Align: 8, Items: items, PointerWords: pointerWordIndices(types.NewArray(slice.Elem(), int64(len(literal.Elts))))})
			emitHeader([]ir.DataItem{
				{Sub: ir.SubL, Sym: backingName},
				{Sub: ir.SubL, Ints: []int64{int64(len(literal.Elts)), int64(len(literal.Elts))}},
			})
			return
		}
	}
	var items []ir.DataItem
	var backingPointerWords []int
	elementBasic, basicElements := slice.Elem().Underlying().(*types.Basic)
	switch {
	case basicElements && elementBasic.Kind() == types.String:
		items = make([]ir.DataItem, 0, len(literal.Elts)*2)
		for index, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			value := g.info.Types[expression].Value
			if value == nil || value.Kind() != constant.String {
				items = append(items, ir.DataItem{Zero: int(typeSize(slice.Elem()))})
				continue
			}
			contents := constant.StringVal(value)
			textName := fmt.Sprintf("%s.element.%d", backingName, index)
			g.mod.Data = append(g.mod.Data, &ir.Data{
				Name:  textName,
				Align: 1,
				Items: []ir.DataItem{
					{Sub: ir.SubUB, Str: contents},
				},
			})
			items = append(items,
				ir.DataItem{Sub: ir.SubL, Sym: textName},
				ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
			)
		}
		backingPointerWords = pointerWordIndices(types.NewArray(slice.Elem(), int64(len(literal.Elts))))
	case basicElements && elementBasic.Info()&(types.IsInteger|types.IsBoolean) != 0:
		values := make([]int64, len(literal.Elts))
		for index, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			value := g.info.Types[expression].Value
			if value != nil {
				values[index] = constInt(value)
			}
		}
		sub, ok := subOf(slice.Elem())
		if !ok {
			class, _ := scalar(slice.Elem())
			sub = ir.SubW
			if class == ir.ClsL || class == ir.ClsP {
				sub = ir.SubL
			}
		}
		items = []ir.DataItem{{Sub: sub, Ints: values}}
	case basicElements && elementBasic.Info()&types.IsFloat != 0:
		values := make([]float64, len(literal.Elts))
		for index, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			value := g.info.Types[expression].Value
			if value == nil {
				continue
			}
			converted, _ := constant.Float64Val(value)
			values[index] = converted
		}
		sub := ir.SubD
		if elementBasic.Kind() == types.Float32 {
			sub = ir.SubS
		}
		items = []ir.DataItem{{Sub: sub, Flts: values}}
	default:
		items = []ir.DataItem{{Zero: int(typeSize(slice.Elem())) * len(literal.Elts)}}
		backingPointerWords = pointerWordIndices(types.NewArray(slice.Elem(), int64(len(literal.Elts))))
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{Name: backingName, Align: 8, Items: items, PointerWords: backingPointerWords})
	emitHeader([]ir.DataItem{
		{Sub: ir.SubL, Sym: backingName},
		{Sub: ir.SubL, Ints: []int64{int64(len(literal.Elts)), int64(len(literal.Elts))}},
	})
}

func (g *gen) staticStructItems(name string, structure *types.Struct, literal *ast.CompositeLit) []ir.DataItem {
	offsets := structOffsets(structFields(structure))
	items := make([]ir.DataItem, 0, structure.NumFields()*2)
	type fieldInitializer struct {
		fieldIndex int
		expression ast.Expr
	}
	initializers := make([]fieldInitializer, 0, len(literal.Elts))
	for elementIndex, expression := range literal.Elts {
		fieldIndex := elementIndex
		if keyed, ok := expression.(*ast.KeyValueExpr); ok {
			for index := 0; index < structure.NumFields(); index++ {
				if structure.Field(index).Name() == keyed.Key.(*ast.Ident).Name {
					fieldIndex = index
					break
				}
			}
			expression = keyed.Value
		}
		initializers = append(initializers, fieldInitializer{
			fieldIndex: fieldIndex,
			expression: expression,
		})
	}
	sort.Slice(initializers, func(left, right int) bool {
		return initializers[left].fieldIndex < initializers[right].fieldIndex
	})
	cursor := int64(0)
	for _, initializer := range initializers {
		fieldIndex := initializer.fieldIndex
		expression := initializer.expression
		offset := offsets[fieldIndex]
		if offset > cursor {
			items = append(items, ir.DataItem{Zero: int(offset - cursor)})
		}
		fieldType := structure.Field(fieldIndex).Type()
		fieldSize := typeSize(fieldType)
		if isInterfaceValue(fieldType) {
			if isNilExpression(expression) {
				items = append(items, ir.DataItem{Zero: int(fieldSize)})
				cursor = offset + fieldSize
				continue
			}
			sourceType := g.info.Types[expression].Type
			fieldName := name + ".interface." + structure.Field(fieldIndex).Name()
			items = append(items, g.staticInterfaceItems(fieldName, sourceType, fieldType, expression)...)
		} else if sliceType, isSlice := fieldType.Underlying().(*types.Slice); isSlice {
			literal, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(fieldSize)})
			} else {
				fieldName := name + ".slice." + structure.Field(fieldIndex).Name()
				items = append(items, g.staticSliceHeaderItems(fieldName, sliceType, literal)...)
			}
		} else if arrayType, isArray := fieldType.Underlying().(*types.Array); isArray {
			literal, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(fieldSize)})
			} else {
				fieldName := name + ".array." + structure.Field(fieldIndex).Name()
				items = append(items, g.staticArrayItems(fieldName, arrayType, literal)...)
			}
		} else if nestedStructure, isStructure := fieldType.Underlying().(*types.Struct); isStructure {
			literal, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(fieldSize)})
			} else {
				fieldName := name + ".struct." + structure.Field(fieldIndex).Name()
				items = append(items, g.staticStructItems(fieldName, nestedStructure, literal)...)
			}
		} else if value := g.info.Types[expression].Value; value != nil {
			if value.Kind() == constant.String {
				text := name + ".string." + structure.Field(fieldIndex).Name()
				contents := constant.StringVal(value)
				g.mod.Data = append(g.mod.Data, &ir.Data{Name: text, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}})
				items = append(items, ir.DataItem{Sub: ir.SubL, Sym: text}, ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(len(contents))}})
			} else if basic, ok := fieldType.Underlying().(*types.Basic); ok && basic.Info()&types.IsFloat != 0 {
				floatValue, _ := constant.Float64Val(value)
				sub := ir.SubD
				if basic.Kind() == types.Float32 {
					sub = ir.SubS
				}
				items = append(items, ir.DataItem{Sub: sub, Flts: []float64{floatValue}})
			} else {
				sub, ok := subOf(fieldType)
				if !ok {
					sub = ir.SubL
				}
				items = append(items, ir.DataItem{Sub: sub, Ints: []int64{constInt(value)}})
			}
		} else if address, ok := expression.(*ast.UnaryExpr); ok && address.Op == token.AND {
			if selector, ok := address.X.(*ast.SelectorExpr); ok {
				base, _ := selector.X.(*ast.Ident)
				object := g.info.Uses[base]
				items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.globals[object], Off: fieldOffset(g.info.Selections[selector])})
			} else if identifier, ok := address.X.(*ast.Ident); ok {
				object := g.info.Uses[identifier]
				if object == nil {
					object = g.info.Defs[identifier]
				}
				symbol := g.globals[object]
				if symbol == "" && object != nil && object.Pkg() != nil {
					symbol = object.Pkg().Path() + "." + object.Name()
					if linkedName := g.globalLinkNames[object]; linkedName != "" {
						symbol = linkedName
					}
				}
				if symbol == "" {
					items = append(items, ir.DataItem{Zero: int(fieldSize)})
				} else {
					items = append(items, ir.DataItem{Sub: ir.SubL, Sym: symbol})
				}
			} else {
				items = append(items, ir.DataItem{Zero: int(fieldSize)})
			}
		} else if function := calledFunction(expression, g.info); function != nil {
			descriptor := g.staticFunctionDescriptor(g.functionSymbol(function))
			items = append(items, ir.DataItem{Sub: ir.SubL, Sym: descriptor})
		} else if function, ok := expression.(*ast.FuncLit); ok {
			descriptor := g.staticFunctionLiteral(function)
			if descriptor == "" {
				items = append(items, ir.DataItem{Zero: int(fieldSize)})
			} else {
				items = append(items, ir.DataItem{Sub: ir.SubL, Sym: descriptor})
			}
		} else {
			items = append(items, ir.DataItem{Zero: int(fieldSize)})
		}
		cursor = offset + fieldSize
	}
	if size := typeSize(structure); cursor < size {
		items = append(items, ir.DataItem{Zero: int(size - cursor)})
	}
	return items
}

func (g *gen) staticInterfaceItems(name string, sourceType, targetType types.Type, expression ast.Expr) []ir.DataItem {
	tag := g.staticInterfaceTypeWord(sourceType, targetType)
	if isDirectInterfaceType(sourceType) {
		payload, ok := g.staticDirectInterfacePayload(name, sourceType, expression)
		if !ok {
			g.fail(expression, "unsupported static interface payload %s", sourceType)
			payload = ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
		}
		return []ir.DataItem{tag, payload}
	}

	payloadItems, ok := g.staticValueItems(name, sourceType, expression)
	if !ok {
		g.fail(expression, "unsupported static interface payload %s", sourceType)
		payloadItems = []ir.DataItem{{Zero: int(typeSize(sourceType))}}
	}
	if len(payloadItems) == 0 {
		payloadItems = []ir.DataItem{{Zero: 1}}
	}
	alignment := int(typeAlign(sourceType))
	if alignment < 1 {
		alignment = 1
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:         name,
		Align:        alignment,
		Items:        payloadItems,
		PointerWords: pointerWordIndices(sourceType),
	})
	return []ir.DataItem{tag, {Sub: ir.SubL, Sym: name}}
}

func (g *gen) staticValueItems(name string, valueType types.Type, expression ast.Expr) ([]ir.DataItem, bool) {
	switch value := valueType.Underlying().(type) {
	case *types.Basic:
		constantValue := g.info.Types[expression].Value
		if constantValue == nil {
			return nil, false
		}
		if value.Kind() == types.String || value.Kind() == types.UntypedString {
			if constantValue.Kind() != constant.String {
				return nil, false
			}
			contents := constant.StringVal(constantValue)
			textName := name + ".text"
			g.mod.Data = append(g.mod.Data, &ir.Data{
				Name:  textName,
				Align: 1,
				Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}},
			})
			return []ir.DataItem{
				{Sub: ir.SubL, Sym: textName},
				{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
			}, true
		}
		if value.Info()&types.IsFloat != 0 {
			floatValue, _ := constant.Float64Val(constantValue)
			sub := ir.SubD
			if value.Kind() == types.Float32 {
				sub = ir.SubS
			}
			return []ir.DataItem{{Sub: sub, Flts: []float64{floatValue}}}, true
		}
		sub, ok := subOf(valueType)
		if !ok {
			return nil, false
		}
		return []ir.DataItem{{Sub: sub, Ints: []int64{constInt(constantValue)}}}, true
	case *types.Struct:
		literal, ok := expression.(*ast.CompositeLit)
		if !ok {
			return nil, false
		}
		return g.staticStructItems(name, value, literal), true
	case *types.Array:
		literal, ok := expression.(*ast.CompositeLit)
		if !ok {
			return nil, false
		}
		return g.staticArrayItems(name, value, literal), true
	case *types.Slice:
		literal, ok := expression.(*ast.CompositeLit)
		if !ok {
			return nil, false
		}
		return g.staticSliceHeaderItems(name, value, literal), true
	default:
		return nil, false
	}
}

func (g *gen) staticDirectInterfacePayload(name string, sourceType types.Type, expression ast.Expr) (ir.DataItem, bool) {
	if isNilExpression(expression) {
		return ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}, true
	}
	if address, ok := expression.(*ast.UnaryExpr); ok && address.Op == token.AND {
		if literal, ok := address.X.(*ast.CompositeLit); ok {
			pointer, ok := sourceType.Underlying().(*types.Pointer)
			if !ok {
				return ir.DataItem{}, false
			}
			items, ok := g.staticValueItems(name, pointer.Elem(), literal)
			if !ok {
				return ir.DataItem{}, false
			}
			alignment := int(typeAlign(pointer.Elem()))
			if alignment < 1 {
				alignment = 1
			}
			g.mod.Data = append(g.mod.Data, &ir.Data{
				Name:         name,
				Align:        alignment,
				Items:        items,
				PointerWords: pointerWordIndices(pointer.Elem()),
			})
			return ir.DataItem{Sub: ir.SubL, Sym: name}, true
		}
		if identifier, ok := address.X.(*ast.Ident); ok {
			object := g.info.Uses[identifier]
			if object == nil {
				object = g.info.Defs[identifier]
			}
			symbol := g.globals[object]
			if symbol == "" && object != nil && object.Pkg() != nil {
				symbol = object.Pkg().Path() + "." + object.Name()
				if linkedName := g.globalLinkNames[object]; linkedName != "" {
					symbol = linkedName
				}
			}
			if symbol != "" {
				return ir.DataItem{Sub: ir.SubL, Sym: symbol}, true
			}
		}
	}
	if function := calledFunction(expression, g.info); function != nil {
		descriptor := g.staticFunctionDescriptor(g.functionSymbol(function))
		return ir.DataItem{Sub: ir.SubL, Sym: descriptor}, true
	}
	return ir.DataItem{}, false
}

func (g *gen) staticArrayItems(name string, arrayType *types.Array, literal *ast.CompositeLit) []ir.DataItem {
	initializers := make(map[int64]ast.Expr, len(literal.Elts))
	for position, expression := range literal.Elts {
		index := int64(position)
		if keyed, ok := expression.(*ast.KeyValueExpr); ok {
			index = constInt(g.info.Types[keyed.Key].Value)
			expression = keyed.Value
		}
		if index >= 0 && index < arrayType.Len() {
			initializers[index] = expression
		}
	}

	elementType := arrayType.Elem()
	items := make([]ir.DataItem, 0, arrayType.Len())
	for index := int64(0); index < arrayType.Len(); index++ {
		expression := initializers[index]
		if expression == nil {
			items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
			continue
		}

		elementName := fmt.Sprintf("%s.element.%d", name, index)
		switch element := elementType.Underlying().(type) {
		case *types.Basic:
			value := g.info.Types[expression].Value
			if value == nil {
				items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
				continue
			}
			if element.Kind() == types.String && value.Kind() == constant.String {
				contents := constant.StringVal(value)
				g.mod.Data = append(g.mod.Data, &ir.Data{
					Name:  elementName,
					Align: 1,
					Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}},
				})
				items = append(items,
					ir.DataItem{Sub: ir.SubL, Sym: elementName},
					ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
				)
				continue
			}
			if element.Info()&types.IsFloat != 0 {
				floatValue, _ := constant.Float64Val(value)
				sub := ir.SubD
				if element.Kind() == types.Float32 {
					sub = ir.SubS
				}
				items = append(items, ir.DataItem{Sub: sub, Flts: []float64{floatValue}})
				continue
			}
			sub, ok := subOf(elementType)
			if !ok {
				sub = ir.SubL
			}
			items = append(items, ir.DataItem{Sub: sub, Ints: []int64{constInt(value)}})
		case *types.Struct:
			elementLiteral, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
				continue
			}
			items = append(items, g.staticStructItems(elementName, element, elementLiteral)...)
		case *types.Array:
			elementLiteral, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
				continue
			}
			items = append(items, g.staticArrayItems(elementName, element, elementLiteral)...)
		case *types.Slice:
			elementLiteral, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
				continue
			}
			items = append(items, g.staticSliceHeaderItems(elementName, element, elementLiteral)...)
		default:
			items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
		}
	}
	return items
}

func (g *gen) staticSliceHeaderItems(name string, sliceType *types.Slice, literal *ast.CompositeLit) []ir.DataItem {
	backingName := name + ".backing"
	elementType := sliceType.Elem()
	items := make([]ir.DataItem, 0, len(literal.Elts))

	for index, expression := range literal.Elts {
		if keyed, ok := expression.(*ast.KeyValueExpr); ok {
			expression = keyed.Value
		}
		elementName := fmt.Sprintf("%s.element.%d", backingName, index)
		switch element := elementType.Underlying().(type) {
		case *types.Basic:
			value := g.info.Types[expression].Value
			if element.Kind() == types.String && value != nil && value.Kind() == constant.String {
				contents := constant.StringVal(value)
				g.mod.Data = append(g.mod.Data, &ir.Data{
					Name:  elementName,
					Align: 1,
					Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}},
				})
				items = append(items,
					ir.DataItem{Sub: ir.SubL, Sym: elementName},
					ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
				)
				continue
			}
			if value == nil {
				items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
				continue
			}
			if element.Info()&types.IsFloat != 0 {
				floatValue, _ := constant.Float64Val(value)
				sub := ir.SubD
				if element.Kind() == types.Float32 {
					sub = ir.SubS
				}
				items = append(items, ir.DataItem{Sub: sub, Flts: []float64{floatValue}})
				continue
			}
			sub, ok := subOf(elementType)
			if !ok {
				sub = ir.SubL
			}
			items = append(items, ir.DataItem{Sub: sub, Ints: []int64{constInt(value)}})
		case *types.Struct:
			elementLiteral, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
				continue
			}
			items = append(items, g.staticStructItems(elementName, element, elementLiteral)...)
		case *types.Slice:
			elementLiteral, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
				continue
			}
			items = append(items, g.staticSliceHeaderItems(elementName, element, elementLiteral)...)
		default:
			items = append(items, ir.DataItem{Zero: int(typeSize(elementType))})
		}
	}

	if len(items) == 0 {
		items = append(items, ir.DataItem{Zero: 1})
	}
	arrayType := types.NewArray(elementType, int64(len(literal.Elts)))
	align := int(typeAlign(elementType))
	if align < 1 {
		align = 1
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:         backingName,
		Align:        align,
		Items:        items,
		PointerWords: pointerWordIndices(arrayType),
	})
	length := int64(len(literal.Elts))
	return []ir.DataItem{
		{Sub: ir.SubL, Sym: backingName},
		{Sub: ir.SubL, Ints: []int64{length, length}},
	}
}

func (g *gen) staticFunctionLiteral(literal *ast.FuncLit) string {
	temporaryName := fmt.Sprintf(".goc.global.literal.%d", len(g.mod.Funcs))
	temporary := g.mod.NewFuncVoid(temporaryName)

	savedFunction := g.fn
	savedBlock := g.cur
	savedVariables := g.vars
	savedDirectValues := g.directValues
	savedStackAddresses := g.stackAddresses
	savedHeapCaptures := g.heapCaptures
	savedEscapingCaptures := g.escapingCaptures
	savedParents := g.parents
	savedBody := g.currentBody
	savedSequence := g.seq

	g.fn = temporary
	g.cur = temporary.Entry()
	g.vars = make(map[types.Object]ir.Ref)
	g.directValues = make(map[types.Object]bool)
	g.stackAddresses = make(map[uint32]bool)
	g.heapCaptures = make(map[types.Object]ir.Ref)
	g.escapingCaptures = make(map[types.Object]bool)
	g.parents = make(map[ast.Node]ast.Node)
	g.currentBody = literal.Body
	g.seq = 0

	dataCount := len(g.mod.Data)
	g.functionLiteral(literal)
	descriptor := ""
	if g.err == nil && len(g.mod.Data) > dataCount {
		descriptor = g.mod.Data[len(g.mod.Data)-1].Name
	}

	g.fn = savedFunction
	g.cur = savedBlock
	g.vars = savedVariables
	g.directValues = savedDirectValues
	g.stackAddresses = savedStackAddresses
	g.heapCaptures = savedHeapCaptures
	g.escapingCaptures = savedEscapingCaptures
	g.parents = savedParents
	g.currentBody = savedBody
	g.seq = savedSequence

	for index, function := range g.mod.Funcs {
		if function == temporary {
			g.mod.Funcs = append(g.mod.Funcs[:index], g.mod.Funcs[index+1:]...)
			break
		}
	}
	return descriptor
}

func (g *gen) globalStruct(id *ast.Ident, object types.Object, spec *ast.ValueSpec, valueIndex int) {
	var literal *ast.CompositeLit
	if valueIndex < len(spec.Values) && staticallyInitializedGlobal(spec.Values[valueIndex], object.Type(), g.info) {
		var ok bool
		literal, ok = spec.Values[valueIndex].(*ast.CompositeLit)
		if !ok {
			// Keep addressable storage for globals whose initializer requires
			// executable package-initialization code.
			literal = nil
		}
	}
	size := typeSize(object.Type())
	if g.runtimeAllocation && g.pkg.Path() == "runtime" && id.Name == "firstmoduledata" {
		headerName := ".goc.runtime.pcheader"
		functionTableName := ".goc.runtime.functab"
		g.mod.Data = append(g.mod.Data,
			&ir.Data{Name: headerName, Align: 8, Items: []ir.DataItem{
				{Sub: ir.SubW, Ints: []int64{0xfffffff1}},
				{Sub: ir.SubUB, Ints: []int64{0, 0, 4, 8}},
				{Sub: ir.SubL, Ints: make([]int64, 8)},
			}},
			&ir.Data{Name: functionTableName, Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0, 0}}}},
			&ir.Data{Name: g.pkg.Path() + "." + id.Name, Align: 8, Items: []ir.DataItem{
				{Sub: ir.SubL, Sym: headerName},
				{Zero: 120},
				{Sub: ir.SubL, Sym: functionTableName},
				{Sub: ir.SubL, Ints: []int64{1, 1, 0}},
				{Sub: ir.SubL, Sym: "main.main"},
				{Sub: ir.SubL, Sym: "main.main"},
				{Sub: ir.SubL, Sym: "main.main"},
				{Sub: ir.SubL, Sym: "main.main"},
				{Zero: 48},
				{Sub: ir.SubL, Sym: ".goc.runtime.datastart"},
				{Sub: ir.SubL, Sym: ".goc.runtime.dataend"},
				{Zero: int(size - 256)},
			}},
		)
		g.globals[object] = g.pkg.Path() + "." + id.Name
		return
	}
	structure := object.Type().Underlying().(*types.Struct)
	items := []ir.DataItem{{Zero: int(size)}}
	if literal != nil {
		items = g.staticStructItems(g.pkg.Path()+"."+id.Name, structure, literal)
	}
	data := &ir.Data{
		Name:  g.pkg.Path() + "." + id.Name,
		Align: int(typeAlign(object.Type())),
		Items: items,
	}
	if g.runtimeAllocation && g.pkg.Path() == "runtime" && (id.Name == "g0" || id.Name == "m0") {
		data.Linkage.Export = true
	}
	g.mod.Data = append(g.mod.Data, data)
	g.globals[object] = g.pkg.Path() + "." + id.Name
}

func (g *gen) globalArray(id *ast.Ident, object types.Object, array *types.Array, spec *ast.ValueSpec, valueIndex int) {
	name := g.pkg.Path() + "." + id.Name
	element := array.Elem()
	if structure, isStructure := element.Underlying().(*types.Struct); isStructure {
		initializers := make(map[int64]ast.Expr)
		if valueIndex < len(spec.Values) {
			literal, _ := spec.Values[valueIndex].(*ast.CompositeLit)
			if literal != nil {
				for position, expression := range literal.Elts {
					index := int64(position)
					if keyed, ok := expression.(*ast.KeyValueExpr); ok {
						index = constInt(g.info.Types[keyed.Key].Value)
						expression = keyed.Value
					}
					initializers[index] = expression
				}
			}
		}
		items := make([]ir.DataItem, 0, array.Len()*int64(structure.NumFields()))
		for index := int64(0); index < array.Len(); index++ {
			expression := initializers[index]
			literal, ok := expression.(*ast.CompositeLit)
			if !ok {
				items = append(items, ir.DataItem{Zero: int(typeSize(element))})
				continue
			}
			elementName := fmt.Sprintf("%s.element.%d", name, index)
			items = append(items, g.staticStructItems(elementName, structure, literal)...)
		}
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:         name,
			Align:        int(typeAlign(array)),
			Items:        items,
			PointerWords: pointerWordIndices(array),
		})
		g.globals[object] = name
		return
	}
	if _, isFunction := element.Underlying().(*types.Signature); isFunction {
		if !g.emitRuntimeTables {
			return
		}
		items := make([]ir.DataItem, array.Len())
		pointerWords := make([]int, array.Len())
		for i := range items {
			items[i] = ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
			pointerWords[i] = i
		}
		if valueIndex < len(spec.Values) {
			literal, ok := spec.Values[valueIndex].(*ast.CompositeLit)
			if !ok {
				return
			}
			for i, expression := range literal.Elts {
				index := i
				if keyed, ok := expression.(*ast.KeyValueExpr); ok {
					index = int(constInt(g.info.Types[keyed.Key].Value))
					expression = keyed.Value
				}
				identifier, ok := expression.(*ast.Ident)
				if !ok {
					return
				}
				function, ok := g.info.Uses[identifier].(*types.Func)
				if !ok {
					return
				}
				descriptor := fmt.Sprintf(".goc.funcval.%d", len(g.mod.Data))
				g.mod.Data = append(g.mod.Data, &ir.Data{
					Name:  descriptor,
					Align: 8,
					Items: []ir.DataItem{{Sub: ir.SubL, Sym: g.functionSymbol(function)}},
				})
				items[index] = ir.DataItem{Sub: ir.SubL, Sym: descriptor}
			}
		}
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:         name,
			Align:        8,
			Items:        items,
			PointerWords: pointerWords,
		})
		g.globals[object] = name
		return
	}
	sub, ok := subOf(element)
	floatElements := false
	if !ok {
		cls, scalarOK := scalar(element)
		if !scalarOK {
			return
		}
		if cls == ir.ClsP {
			size := typeSize(array)
			g.mod.Data = append(g.mod.Data, &ir.Data{
				Name:  name,
				Align: int(typeAlign(array)),
				Items: []ir.DataItem{{Sub: ir.SubUB, Ints: make([]int64, size)}},
			})
			g.globals[object] = name
			return
		}
		switch cls {
		case ir.ClsL:
			sub = ir.SubL
		case ir.ClsS:
			sub = ir.SubS
			floatElements = true
		case ir.ClsD:
			sub = ir.SubD
			floatElements = true
		default:
			sub = ir.SubW
		}
	}
	values := make([]int64, array.Len())
	floatValues := make([]float64, array.Len())
	if valueIndex < len(spec.Values) {
		literal, ok := spec.Values[valueIndex].(*ast.CompositeLit)
		if !ok {
			literal = nil
		}
		if literal != nil {
			for i, expression := range literal.Elts {
				index := int64(i)
				if keyed, ok := expression.(*ast.KeyValueExpr); ok {
					index = constInt(g.info.Types[keyed.Key].Value)
					expression = keyed.Value
				}
				value := g.info.Types[expression].Value
				if value == nil {
					if floatElements {
						var static bool
						floatValues[index], static = g.staticFloatValue(expression)
						if static {
							continue
						}
					}
					return
				}
				if floatElements {
					floatValues[index], _ = constant.Float64Val(value)
				} else {
					values[index] = constInt(value)
				}
			}
		}
	}
	item := ir.DataItem{Sub: sub, Ints: values}
	if floatElements {
		item.Ints = nil
		item.Flts = floatValues
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: int(typeSize(element)),
		Items: []ir.DataItem{item},
	})
	g.globals[object] = name
}

func (g *gen) staticFloatValue(expression ast.Expr) (float64, bool) {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return 0, false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	function, ok := g.info.Uses[selector.Sel].(*types.Func)
	if !ok || function.Pkg() == nil || function.Pkg().Path() != "math" {
		return 0, false
	}
	switch function.Name() {
	case "Inf":
		if len(call.Args) != 1 || g.info.Types[call.Args[0]].Value == nil {
			return 0, false
		}
		sign := constInt(g.info.Types[call.Args[0]].Value)
		return math.Inf(int(sign)), true
	case "NaN":
		if len(call.Args) != 0 {
			return 0, false
		}
		return math.NaN(), true
	default:
		return 0, false
	}
}

func (g *gen) fail(n ast.Node, format string, a ...any) {
	if g.err == nil {
		g.err = fmt.Errorf("%s: %s", g.fset.Position(n.Pos()), fmt.Sprintf(format, a...))
	}
}
func (g *gen) block(prefix string) *ir.Block {
	g.seq++
	return g.fn.NewBlock(fmt.Sprintf("%s%d", prefix, g.seq))
}
func (g *gen) live() bool {
	return g.cur != nil && g.cur.Jmp.Kind == ir.JmpNone
}
func (g *gen) at(n ast.Node) {
	if g.cur == nil {
		return
	}
	p := g.fset.Position(n.Pos())
	g.cur.At(ir.SrcPos{File: 1, Line: uint32(p.Line), Col: uint32(p.Column)})
}

func scalar(t types.Type) (ir.Cls, bool) {
	if t == nil {
		return 0, false
	}
	if parameter, ok := t.(*types.TypeParam); ok {
		terms := typeParameterTerms(parameter)
		if len(terms) == 0 {
			return ir.ClsP, true
		}
		class, supported := scalar(terms[0])
		if !supported {
			return 0, false
		}
		for _, term := range terms[1:] {
			candidate, supported := scalar(term)
			if !supported {
				return 0, false
			}
			if candidate != class {
				return ir.ClsP, true
			}
		}
		return class, true
	}
	switch t.Underlying().(type) {
	case *types.Array, *types.Slice, *types.Struct, *types.Interface, *types.Pointer, *types.Signature, *types.Map, *types.Chan:
		return ir.ClsP, true
	}
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return 0, false
	}
	switch b.Kind() {
	case types.UnsafePointer:
		return ir.ClsP, true
	case types.String, types.UntypedString:
		return ir.ClsP, true
	case types.Bool, types.Int8, types.Uint8, types.Int16, types.Uint16, types.Int32, types.Uint32, types.UntypedBool, types.UntypedInt, types.UntypedRune:
		return ir.ClsW, true
	case types.Int, types.Uint, types.Int64, types.Uint64, types.Uintptr:
		return ir.ClsL, true
	case types.Float32:
		return ir.ClsS, true
	case types.Float64, types.UntypedFloat:
		return ir.ClsD, true
	case types.Complex64:
		return ir.ClsL, true
	case types.Complex128:
		return ir.ClsP, true
	}
	return 0, false
}

func (g *gen) goABIAggregate(valueType types.Type) *ir.AggType {
	if !g.runtimeAllocation || valueType == nil {
		return nil
	}
	valueType = representativeType(valueType)
	if _, isTypeParameter := valueType.(*types.TypeParam); isTypeParameter {
		// An unconstrained type parameter is represented by a pointer to its
		// instantiated value in shared generic code. Its underlying constraint
		// may be interface{}, but that two-word interface layout belongs to the
		// instantiated caller, not to the generic function's ABI.
		return nil
	}
	cacheKey := goABIAggregateKey(valueType)
	if aggregate := g.goABITypes[cacheKey]; aggregate != nil {
		return aggregate
	}
	var fields []ir.Field
	switch value := valueType.Underlying().(type) {
	case *types.Array:
		field, ok := g.goABIField(value.Elem())
		if !ok {
			return nil
		}
		field.Count = int(value.Len())
		fields = []ir.Field{field}
	case *types.Slice:
		fields = []ir.Field{
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL},
			{Sub: ir.SubL},
		}
	case *types.Struct:
		fields = make([]ir.Field, 0, value.NumFields())
		for index := 0; index < value.NumFields(); index++ {
			field, ok := g.goABIField(value.Field(index).Type())
			if !ok {
				return nil
			}
			fields = append(fields, field)
		}
	case *types.Interface:
		fields = []ir.Field{
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL, Pointer: true},
		}
	case *types.Basic:
		if value.Kind() != types.String && value.Kind() != types.UntypedString {
			return nil
		}
		fields = []ir.Field{
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL},
		}
	default:
		return nil
	}

	aggregate := &ir.AggType{
		Name:   fmt.Sprintf(".goc.goabi.%d", len(g.mod.Types)),
		Align:  int(typeAlign(valueType)),
		Fields: fields,
	}
	g.mod.AddType(aggregate)
	if g.goABITypes == nil {
		g.goABITypes = make(map[string]*ir.AggType)
	}
	g.goABITypes[cacheKey] = aggregate
	return aggregate
}

func goABIAggregateKey(valueType types.Type) string {
	switch value := valueType.Underlying().(type) {
	case *types.Slice:
		return "descriptor:slice"
	case *types.Interface:
		return "descriptor:interface"
	case *types.Basic:
		if value.Kind() == types.String || value.Kind() == types.UntypedString {
			return "descriptor:string"
		}
	}
	return "aggregate:" + goTypeKey(valueType)
}

func (g *gen) goABIField(valueType types.Type) (ir.Field, bool) {
	if aggregate := g.goABIAggregate(valueType); aggregate != nil {
		return ir.Field{Type: aggregate}, true
	}
	valueType = representativeType(valueType)
	switch value := valueType.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		return ir.Field{Sub: ir.SubL, Pointer: true}, true
	case *types.Basic:
		switch value.Kind() {
		case types.Bool, types.Uint8:
			return ir.Field{Sub: ir.SubUB}, true
		case types.Int8:
			return ir.Field{Sub: ir.SubB}, true
		case types.Uint16:
			return ir.Field{Sub: ir.SubUH}, true
		case types.Int16:
			return ir.Field{Sub: ir.SubH}, true
		case types.Uint32:
			return ir.Field{Sub: ir.SubW}, true
		case types.Int32:
			return ir.Field{Sub: ir.SubW}, true
		case types.Int, types.Uint, types.Int64, types.Uint64, types.Uintptr:
			return ir.Field{Sub: ir.SubL}, true
		case types.UnsafePointer:
			return ir.Field{Sub: ir.SubL, Pointer: true}, true
		case types.Float32:
			return ir.Field{Sub: ir.SubS}, true
		case types.Float64:
			return ir.Field{Sub: ir.SubD}, true
		}
	}
	return ir.Field{}, false
}

func (g *gen) annotateABICall(instruction *ir.Instr, signature *types.Signature, receiverType types.Type) {
	if !g.runtimeAllocation || instruction == nil {
		return
	}
	argumentCount := len(instruction.Args) - 1
	instruction.AggArgs = make([]*ir.AggType, argumentCount)
	argumentIndex := 0
	if receiverType != nil && argumentIndex < argumentCount {
		instruction.AggArgs[argumentIndex] = g.goABIAggregate(receiverType)
		argumentIndex++
	}
	for parameterIndex := 0; parameterIndex < signature.Params().Len() && argumentIndex < argumentCount; parameterIndex++ {
		instruction.AggArgs[argumentIndex] = g.goABIAggregate(signature.Params().At(parameterIndex).Type())
		argumentIndex++
	}
	if signature.Results().Len() > 0 {
		instruction.RetAgg = g.goABIAggregate(signature.Results().At(0).Type())
	}
}

func (g *gen) materializeNilInterface(value ir.Ref) ir.Ref {
	zeroDescriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(zeroDescriptor, 0)
	g.markStackPointerWord(zeroDescriptor, 8)
	g.cur.Store(g.fn.ConstInt(ir.ClsP, 0), zeroDescriptor)
	g.cur.Store(g.fn.ConstInt(ir.ClsP, 0), g.offset(zeroDescriptor, 8))

	useZero := g.block("callinterfacezero")
	useValue := g.block("callinterfacevalue")
	done := g.block("callinterfaceend")
	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, value, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(isNil, useZero, useValue)
	useZero.Goto(done)
	useValue.Goto(done)

	g.cur = done
	return done.Phi(ir.ClsP,
		ir.PhiEdge{From: useZero, Val: zeroDescriptor},
		ir.PhiEdge{From: useValue, Val: value},
	)
}

func (g *gen) normalizeCallInterfaces(arguments []ir.Ref, signature *types.Signature, receiverType types.Type) []ir.Ref {
	if !g.runtimeAllocation {
		return arguments
	}
	normalized := append([]ir.Ref(nil), arguments...)
	argumentIndex := 0
	normalize := func(valueType types.Type) {
		if argumentIndex >= len(normalized) {
			return
		}
		if _, isTypeParameter := valueType.(*types.TypeParam); isTypeParameter {
			// callArguments already converted the value according to the
			// instantiated signature. A type parameter's underlying constraint may
			// be an interface, but wrapping the converted value here would change
			// the representation expected by the shared generic body.
			argumentIndex++
			return
		}
		if _, isInterface := valueType.Underlying().(*types.Interface); isInterface {
			normalized[argumentIndex] = g.materializeNilInterface(normalized[argumentIndex])
		}
		argumentIndex++
	}
	if receiverType != nil {
		normalize(receiverType)
	}
	for parameterIndex := 0; parameterIndex < signature.Params().Len(); parameterIndex++ {
		normalize(signature.Params().At(parameterIndex).Type())
	}
	return normalized
}

// Shared generic bodies carry an opaque type parameter as a pointer to the
// instantiated value. Calls from concrete code must therefore give the body a
// stable, correctly sized copy instead of passing the concrete value itself.
func (g *gen) adaptSharedGenericArguments(arguments []ir.Ref, concrete, compiled *types.Signature, hasReceiver bool) []ir.Ref {
	if concrete == nil || compiled == nil {
		return arguments
	}
	adapted := append([]ir.Ref(nil), arguments...)
	argumentIndex := 0
	if hasReceiver {
		argumentIndex++
	}
	parameterCount := compiled.Params().Len()
	if concrete.Params().Len() < parameterCount {
		parameterCount = concrete.Params().Len()
	}
	for parameterIndex := 0; parameterIndex < parameterCount && argumentIndex < len(adapted); parameterIndex++ {
		compiledType := compiled.Params().At(parameterIndex).Type()
		concreteType := concrete.Params().At(parameterIndex).Type()
		if isSharedTypeParameter(compiledType) && !isSharedTypeParameter(concreteType) {
			payload := g.allocateTyped(concreteType)
			if isAddressRepresentedInterfacePayload(concreteType) || isInterfaceValue(concreteType) {
				g.storeInlineValue(adapted[argumentIndex], payload, concreteType)
			} else {
				g.store(adapted[argumentIndex], payload, concreteType)
			}
			adapted[argumentIndex] = payload
		}
		argumentIndex++
	}
	return adapted
}

func (g *gen) callWithSignature(resultClass ir.Cls, callee ir.Ref, arguments []ir.Ref, signature *types.Signature, receiverType types.Type) ir.Ref {
	arguments = g.normalizeCallInterfaces(arguments, signature, receiverType)
	arguments, argumentGroups, aggregateArguments := g.flattenCallArguments(arguments, signature, receiverType)

	var result ir.Ref
	if signature.Results().Len() > 0 && g.runtimeAllocation && isSliceType(signature.Results().At(0).Type()) {
		aggregate := g.goABIAggregate(signature.Results().At(0).Type())
		parts := g.cur.CallAggregate(aggregate, []ir.Cls{ir.ClsP, ir.ClsL, ir.ClsL}, callee, arguments...)
		result = g.fn.Aggregate(aggregate, parts...)
	} else {
		result = g.cur.Call(resultClass, callee, arguments...)
	}
	instruction := &g.cur.Instrs[len(g.cur.Instrs)-1]
	instruction.ClosureCall = g.consumeClosureCall()
	instruction.ArgGroups = argumentGroups
	instruction.AggArgs = aggregateArguments
	if signature.Results().Len() > 0 && instruction.RetAgg == nil {
		instruction.RetAgg = g.goABIAggregate(signature.Results().At(0).Type())
	}
	return result
}

func (g *gen) callVoidWithSignature(callee ir.Ref, arguments []ir.Ref, signature *types.Signature, receiverType types.Type) {
	arguments = g.normalizeCallInterfaces(arguments, signature, receiverType)
	arguments, argumentGroups, aggregateArguments := g.flattenCallArguments(arguments, signature, receiverType)
	g.cur.CallVoid(callee, arguments...)
	instruction := &g.cur.Instrs[len(g.cur.Instrs)-1]
	instruction.ClosureCall = g.consumeClosureCall()
	instruction.ArgGroups = argumentGroups
	instruction.AggArgs = aggregateArguments
}

func (g *gen) flattenCallArguments(arguments []ir.Ref, signature *types.Signature, receiverType types.Type) ([]ir.Ref, []ir.ValueGroup, []*ir.AggType) {
	flat := make([]ir.Ref, 0, len(arguments)+4)
	groups := make([]ir.ValueGroup, 0, 2)
	aggregates := make([]*ir.AggType, 0, len(arguments)+4)
	argumentIndex := 0

	appendArgument := func(value ir.Ref, valueType types.Type) {
		if g.runtimeAllocation && isSliceType(valueType) {
			data, length, capacity := g.sliceParts(value)
			start := len(flat)
			flat = append(flat, data, length, capacity)
			aggregates = append(aggregates, nil, nil, nil)
			groups = append(groups, ir.ValueGroup{
				Index: start,
				Count: 3,
				Type:  g.goABIAggregate(valueType),
			})
			return
		}
		flat = append(flat, value)
		aggregates = append(aggregates, g.goABIAggregate(valueType))
	}

	if receiverType != nil && argumentIndex < len(arguments) {
		appendArgument(arguments[argumentIndex], receiverType)
		argumentIndex++
	}
	for parameterIndex := 0; parameterIndex < signature.Params().Len() && argumentIndex < len(arguments); parameterIndex++ {
		appendArgument(arguments[argumentIndex], signature.Params().At(parameterIndex).Type())
		argumentIndex++
	}
	for argumentIndex < len(arguments) {
		flat = append(flat, arguments[argumentIndex])
		aggregates = append(aggregates, nil)
		argumentIndex++
	}
	return flat, groups, aggregates
}
func signed(t types.Type) bool {
	if parameter, ok := t.(*types.TypeParam); ok {
		terms := typeParameterTerms(parameter)
		if len(terms) == 0 {
			return false
		}
		for _, term := range terms {
			if !signed(term) {
				return false
			}
		}
		return true
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsUnsigned == 0 && b.Info()&types.IsBoolean == 0
}

func subOf(t types.Type) (ir.SubCls, bool) {
	t = representativeType(t)
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return 0, false
	}
	switch b.Kind() {
	case types.Bool, types.Uint8:
		return ir.SubUB, true
	case types.Int8:
		return ir.SubB, true
	case types.Uint16:
		return ir.SubUH, true
	case types.Int16:
		return ir.SubH, true
	case types.Int32, types.Uint32:
		return ir.SubW, true
	}
	return 0, false
}

func (g *gen) alloc(t types.Type) ir.Ref {
	c, _ := scalar(t)
	var address ir.Ref
	if c == ir.ClsL || c == ir.ClsP || c == ir.ClsD {
		address = g.localAlloc(8, 8)
	} else {
		address = g.localAlloc(4, 4)
	}
	if c == ir.ClsP {
		g.markStackPointerWord(address, 0)
	}
	return address
}
func (g *gen) load(addr ir.Ref, t types.Type) ir.Ref {
	if g.runtimeAllocation {
		if _, ok := t.Underlying().(*types.Slice); ok {
			data := g.cur.Load(ir.ClsP, addr)
			length := g.cur.Load(ir.ClsL, g.offset(addr, 8))
			capacity := g.cur.Load(ir.ClsL, g.offset(addr, 16))
			return g.sliceValue(data, length, capacity)
		}
	}
	c, _ := scalar(t)
	if sub, ok := subOf(t); ok {
		return g.cur.LoadSub(c, sub, addr)
	}
	return g.cur.Load(c, addr)
}
func (g *gen) store(v, addr ir.Ref, t types.Type) {
	if g.runtimeAllocation {
		if slice, ok := t.Underlying().(*types.Slice); ok {
			data, length, capacity := g.sliceParts(v)
			g.store(data, addr, types.NewPointer(slice.Elem()))
			g.store(length, g.offset(addr, 8), types.Typ[types.Int])
			g.store(capacity, g.offset(addr, 16), types.Typ[types.Int])
			return
		}
	}
	if sub, ok := subOf(t); ok {
		g.cur.StoreSub(sub, v, addr)
		return
	}
	class, _ := scalar(t)
	if g.runtimeAllocation && !g.noWriteBarrier && class == ir.ClsP && !g.isStackAddress(addr) {
		g.cur.CallVoid(g.fn.Sym("goc_storep", 0), addr, v)
		return
	}
	g.cur.Store(v, addr)
}

// assignLocal stores a Go value into a frontend variable slot. Struct and
// array variables use an indirect slot so their address remains stable across
// assignments; assigning one of these values must copy into that backing
// storage rather than replace the slot with an alias of the source value.
func (g *gen) assignLocal(value, slot ir.Ref, valueType types.Type) {
	for object, direct := range g.directValues {
		if direct && g.vars[object] == slot {
			g.storeInlineValue(value, slot, valueType)
			return
		}
	}
	if isMemoryValue(valueType) {
		destination := g.load(slot, valueType)
		g.storeInlineValue(value, destination, valueType)
		return
	}
	g.store(value, slot, valueType)
}

// storeInlineValue copies a value represented by an address into storage that
// contains the value itself, such as a struct field or array element. A nil
// interface is represented internally by a nil descriptor pointer, so give it
// a concrete zero header before copying its two words.
func (g *gen) storeInlineValue(value, address ir.Ref, valueType types.Type) {
	if g.runtimeAllocation {
		if _, ok := valueType.Underlying().(*types.Slice); ok {
			g.store(value, address, valueType)
			return
		}
	}
	if isInterfaceValue(valueType) {
		value = g.materializeNilInterface(value)
	}
	if g.runtimeAllocation && !g.noWriteBarrier && len(pointerWordIndices(valueType)) != 0 {
		g.storePointerAwareInlineValue(value, address, valueType)
		return
	}
	g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), address, value, g.fn.Long(typeSize(valueType)))
}

// storePointerAwareInlineValue copies the scalar regions of an aggregate with
// memcpy and publishes each pointer word through the normal pointer-store
// path. The latter dynamically distinguishes heap and global destinations
// from any goroutine stack, so aggregate assignment preserves Go's write
// barrier semantics without changing the in-memory layout of the value.
func (g *gen) storePointerAwareInlineValue(value, address ir.Ref, valueType types.Type) {
	pointerWords := pointerWordIndices(valueType)
	valueSize := typeSize(valueType)
	nextOffset := int64(0)

	copyBytes := func(offset, size int64) {
		if size == 0 {
			return
		}
		source := g.offset(value, offset)
		destination := g.offset(address, offset)
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), destination, source, g.fn.Long(size))
	}

	pointerType := types.Typ[types.UnsafePointer]
	for _, pointerWord := range pointerWords {
		pointerOffset := int64(pointerWord) * pointerSize()
		copyBytes(nextOffset, pointerOffset-nextOffset)

		source := g.offset(value, pointerOffset)
		destination := g.offset(address, pointerOffset)
		pointer := g.cur.Load(ir.ClsP, source)
		g.store(pointer, destination, pointerType)
		nextOffset = pointerOffset + pointerSize()
	}
	copyBytes(nextOffset, valueSize-nextOffset)
}

func (g *gen) localAlloc(align, size int) ir.Ref {
	address := g.cur.Alloc(align, size)
	if g.stackAddresses == nil {
		g.stackAddresses = make(map[uint32]bool)
	}
	g.stackAddresses[address.ID] = true
	return address
}

func (g *gen) variableStorage(object types.Object, valueType types.Type) ir.Ref {
	if storage, exists := g.vars[object]; exists {
		return storage
	}
	if g.runtimeAllocation && g.escapingCaptures[object] {
		var storage ir.Ref
		if isMemoryValue(valueType) {
			backing := g.allocateTyped(valueType)
			storageType := types.NewPointer(valueType)
			storage = g.allocateTyped(storageType)
			g.store(backing, storage, storageType)
		} else if isStringType(valueType) || isInterfaceValue(valueType) {
			// Strings and interfaces are normally represented by a local slot
			// containing a pointer to their inline header. Once the variable's
			// address escapes, the heap allocation is the header itself.
			storage = g.allocateTyped(valueType)
			g.directValues[object] = true
		} else {
			storage = g.allocateTyped(valueType)
		}
		g.vars[object] = storage
		g.heapCaptures[object] = storage
		return storage
	}
	storage := g.allocLocal(valueType)
	g.vars[object] = storage
	return storage
}

func (g *gen) localAllocTyped(valueType types.Type) ir.Ref {
	alignment := int(typeAlign(valueType))
	if alignment < 4 {
		alignment = 4
	}
	address := g.localAlloc(alignment, int(typeSize(valueType)))
	visitPointerWords(valueType, 0, func(offset int64) {
		g.markStackPointerWord(address, int(offset))
	})
	return address
}

func (g *gen) markStackPointerWord(allocation ir.Ref, offset int) {
	if allocation.Kind != ir.RefTemp || offset < 0 || offset%8 != 0 {
		return
	}
	if g.fn.StackPointerWords == nil {
		g.fn.StackPointerWords = make(map[uint32]map[int]bool)
	}
	if g.fn.StackPointerWords[allocation.ID] == nil {
		g.fn.StackPointerWords[allocation.ID] = make(map[int]bool)
	}
	g.fn.StackPointerWords[allocation.ID][offset] = true
}

func (g *gen) isStackAddress(address ir.Ref) bool {
	return address.Kind == ir.RefTemp && g.stackAddresses[address.ID]
}
func (g *gen) coerce(v ir.Ref, t types.Type) ir.Ref {
	t = representativeType(t)
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return v
	}
	switch b.Kind() {
	case types.Bool, types.Uint8:
		return g.cur.Extub(ir.ClsW, v)
	case types.Int8:
		return g.cur.Extsb(ir.ClsW, v)
	case types.Uint16:
		return g.cur.Extuh(ir.ClsW, v)
	case types.Int16:
		return g.cur.Extsh(ir.ClsW, v)
	}
	return v
}

func (g *gen) convert(v ir.Ref, from, to types.Type) ir.Ref {
	if _, sourceIsSlice := from.Underlying().(*types.Slice); sourceIsSlice {
		if targetPointer, targetIsPointer := to.Underlying().(*types.Pointer); targetIsPointer {
			if targetArray, targetIsArray := targetPointer.Elem().Underlying().(*types.Array); targetIsArray {
				data, length, _ := g.sliceParts(v)
				enough := g.block("arraypointerconvertenough")
				tooShort := g.block("arraypointerconvertshort")
				hasEnoughElements := g.cur.Cmp(ir.CmpSge, ir.ClsL, length, g.fn.Long(targetArray.Len()))
				g.cur.Jnz(hasEnoughElements, enough, tooShort)
				g.cur = tooShort
				g.cur.CallVoid(g.fn.Sym("runtime.goPanicSliceConvert", 0), g.fn.Long(targetArray.Len()), length)
				g.cur.Hlt()
				g.cur = enough
				return data
			}
		}
		if targetArray, targetIsArray := to.Underlying().(*types.Array); targetIsArray {
			data, length, _ := g.sliceParts(v)
			enough := g.block("arrayconvertenough")
			tooShort := g.block("arrayconvertshort")
			hasEnoughElements := g.cur.Cmp(ir.CmpSge, ir.ClsL, length, g.fn.Long(targetArray.Len()))
			g.cur.Jnz(hasEnoughElements, enough, tooShort)
			g.cur = tooShort
			g.cur.CallVoid(g.fn.Sym("runtime.goPanicSliceConvert", 0), g.fn.Long(targetArray.Len()), length)
			g.cur.Hlt()

			g.cur = enough
			alignment := int(typeAlign(to))
			if alignment < 4 {
				alignment = 4
			}
			result := g.localAllocTyped(to)
			g.storeInlineValue(data, result, to)
			return result
		}
	}

	fc, _ := scalar(from)
	tc, _ := scalar(to)
	switch {
	case fc == ir.ClsS && tc == ir.ClsD:
		v = g.cur.Exts(v)
	case fc == ir.ClsD && tc == ir.ClsS:
		v = g.cur.Truncd(v)
	case fc.IsFloat() && !tc.IsFloat():
		if signed(to) {
			v = g.cur.Stosi(tc, v)
		} else {
			v = g.cur.Stoui(tc, v)
		}
	case !fc.IsFloat() && tc.IsFloat():
		if signed(from) {
			v = g.cur.Sltof(tc, v)
		} else {
			v = g.cur.Ultof(tc, v)
		}
	case fc == ir.ClsW && tc == ir.ClsL:
		if signed(from) {
			v = g.cur.Extsw(ir.ClsL, v)
		} else {
			v = g.cur.Extuw(ir.ClsL, v)
		}
	case fc != tc:
		v = g.cur.Copy(tc, v)
	}
	return g.coerce(v, to)
}

func (g *gen) assignmentValue(expression ast.Expr, targetType types.Type) ir.Ref {
	return g.assignmentValueWithInterfacePayload(expression, targetType, ir.R)
}

func (g *gen) assignmentValueWithInterfacePayload(expression ast.Expr, targetType types.Type, payload ir.Ref) ir.Ref {
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "nil" && isDescriptorValue(targetType) {
		return g.zeroValue(targetType)
	}
	if isSharedTypeParameter(targetType) {
		// Shared generic code represents an unconstrained type parameter as one
		// pointer-sized value. Its interface constraint describes permitted
		// instantiations; it is not the runtime representation of this variable.
		return g.expr(expression)
	}
	if _, isInterface := targetType.Underlying().(*types.Interface); !isInterface {
		value := g.expr(expression)
		if isMemoryValue(targetType) {
			return g.copyInlineValue(value, targetType)
		}
		return value
	}
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "nil" {
		return g.fn.ConstInt(ir.ClsP, 0)
	}
	sourceType := g.typeAndValue(expression).Type
	value := g.expr(expression)
	return g.adaptValueToInterface(value, sourceType, targetType, payload, expression)
}

func (g *gen) adaptValueToInterface(value ir.Ref, sourceType, targetType types.Type, payload ir.Ref, source ast.Node) ir.Ref {
	if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
		return g.adaptInterfaceToInterface(value, sourceType, targetType)
	}
	typeTag := g.ensureTypeTag(sourceType)
	g.ensureRuntimeTypeEqual(sourceType, typeTag)
	typeWord := g.fn.Sym(typeTag, 0)
	if g.runtimeAllocation && interfaceHasMethods(targetType) {
		typeWord = g.fn.Sym(g.ensureInterfaceItab(sourceType, targetType), 0)
	}
	if isDirectInterfaceType(sourceType) {
		descriptor := g.localAlloc(8, 16)
		g.markStackPointerWord(descriptor, 0)
		g.markStackPointerWord(descriptor, 8)
		g.cur.Store(typeWord, descriptor)
		if value.Kind == ir.RefAggregate {
			g.fail(source, "direct interface payload %s lowered as an aggregate", sourceType)
			return descriptor
		}
		g.cur.Store(value, g.offset(descriptor, 8))
		return descriptor
	}
	if payload == ir.R {
		payload = g.allocateTyped(sourceType)
	}
	if isAddressRepresentedInterfacePayload(sourceType) {
		g.storeInlineValue(value, payload, sourceType)
	} else {
		g.store(value, payload, sourceType)
	}
	descriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(descriptor, 0)
	g.markStackPointerWord(descriptor, 8)
	g.cur.Store(typeWord, descriptor)
	g.cur.Store(payload, g.offset(descriptor, 8))
	return descriptor
}

func interfaceHasMethods(valueType types.Type) bool {
	interfaceType, ok := valueType.Underlying().(*types.Interface)
	return ok && interfaceType.NumMethods() != 0
}

func (g *gen) staticInterfaceTypeWord(sourceType, targetType types.Type) ir.DataItem {
	sourceTypeTag := g.ensureTypeTag(sourceType)
	g.ensureRuntimeTypeEqual(sourceType, sourceTypeTag)
	if g.runtimeAllocation && interfaceHasMethods(targetType) {
		return ir.DataItem{Sub: ir.SubL, Sym: g.ensureInterfaceItab(sourceType, targetType)}
	}
	return ir.DataItem{Sub: ir.SubL, Sym: sourceTypeTag}
}

func (g *gen) ensureInterfaceItab(sourceType, targetType types.Type) string {
	key := runtimeTypeKey(sourceType) + "->" + runtimeTypeKey(targetType)
	if symbol := g.interfaceItabs[key]; symbol != "" {
		return symbol
	}

	symbol := fmt.Sprintf(".goc.itab.%d", len(g.interfaceItabs))
	g.interfaceItabs[key] = symbol
	interfaceType := targetType.Underlying().(*types.Interface)
	sourceTypeTag := g.ensureTypeTag(sourceType)
	g.ensureRuntimeTypeEqual(sourceType, sourceTypeTag)
	items := []ir.DataItem{
		{Sub: ir.SubL, Sym: g.ensureTypeTag(targetType)},
		{Sub: ir.SubL, Sym: sourceTypeTag},
		{Sub: ir.SubW, Ints: []int64{0}},
		{Zero: 4},
	}
	for methodIndex := 0; methodIndex < interfaceType.NumMethods(); methodIndex++ {
		interfaceMethod := interfaceType.Method(methodIndex)
		object, _, _ := types.LookupFieldOrMethod(sourceType, true, interfaceMethod.Pkg(), interfaceMethod.Name())
		method, ok := object.(*types.Func)
		if !ok {
			g.err = fmt.Errorf("goc: %s does not implement %s", sourceType, targetType)
			items = append(items, ir.DataItem{Sub: ir.SubL, Ints: []int64{0}})
			continue
		}
		items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.functionSymbol(method)})
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:         symbol,
		Align:        8,
		Items:        items,
		PointerWords: []int{0, 1},
	})
	return symbol
}

func (g *gen) interfaceTypeWord(dynamicType ir.Ref, targetType types.Type) ir.Ref {
	if !g.runtimeAllocation || !interfaceHasMethods(targetType) {
		return dynamicType
	}
	return g.cur.Call(
		ir.ClsP,
		g.fn.Sym("runtime.getitab", 0),
		g.typeTag(targetType),
		dynamicType,
		g.fn.Word(0),
	)
}

func (g *gen) interfaceDynamicType(descriptor ir.Ref, staticType types.Type) ir.Ref {
	typeWord := g.cur.Load(ir.ClsP, descriptor)
	if !g.runtimeAllocation || !interfaceHasMethods(staticType) {
		return typeWord
	}
	return g.cur.Load(ir.ClsP, g.offset(typeWord, 8))
}

func (g *gen) adaptInterfaceToInterface(value ir.Ref, sourceType, targetType types.Type) ir.Ref {
	descriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(descriptor, 0)
	g.markStackPointerWord(descriptor, 8)

	nilValue := g.block("interfaceconvertnil")
	nonNilValue := g.block("interfaceconvertnonnil")
	done := g.block("interfaceconvertend")
	isNil := g.interfaceIsNil(value)
	g.cur.Jnz(isNil, nilValue, nonNilValue)

	g.cur = nilValue
	g.cur.Store(g.fn.ConstInt(ir.ClsP, 0), descriptor)
	g.cur.Store(g.fn.ConstInt(ir.ClsP, 0), g.offset(descriptor, 8))
	g.cur.Goto(done)

	g.cur = nonNilValue
	dynamicType := g.interfaceDynamicType(value, sourceType)
	typeWord := g.interfaceTypeWord(dynamicType, targetType)
	g.cur.Store(typeWord, descriptor)
	data := g.cur.Load(ir.ClsP, g.offset(value, 8))
	g.cur.Store(data, g.offset(descriptor, 8))
	g.cur.Goto(done)

	g.cur = done
	return descriptor
}

func isAddressRepresentedInterfacePayload(valueType types.Type) bool {
	if isInlineAggregate(valueType) {
		return true
	}
	basic, ok := valueType.Underlying().(*types.Basic)
	return ok && basic.Kind() == types.Complex128
}

func (g *gen) callArguments(arguments []ast.Expr, hasEllipsis bool, signature *types.Signature) []ir.Ref {
	if !signature.Variadic() {
		if len(arguments) == 1 {
			if call, ok := arguments[0].(*ast.CallExpr); ok {
				if _, multiValued := g.info.Types[call].Type.(*types.Tuple); multiValued {
					values, resultSignature := g.evaluateMultiValueCall(call)
					if len(values) != signature.Params().Len() {
						g.fail(call, "multi-valued argument count does not match parameters")
						return nil
					}
					for index, value := range values {
						parameterType := signature.Params().At(index).Type()
						resultType := resultSignature.Results().At(index).Type()
						if !types.AssignableTo(resultType, parameterType) {
							g.fail(call, "result %s is not assignable to parameter %s", resultType, parameterType)
							return nil
						}
						if isInlineAggregate(parameterType) {
							values[index] = g.copyInlineValue(value, parameterType)
						}
					}
					return values
				}
			}
		}
		values := make([]ir.Ref, 0, len(arguments))
		for index, argument := range arguments {
			parameterType := signature.Params().At(index).Type()
			value := g.assignmentValue(argument, parameterType)
			if isInlineAggregate(parameterType) {
				value = g.copyInlineValue(value, parameterType)
			}
			values = append(values, value)
		}
		return values
	}

	variadicIndex := signature.Params().Len() - 1
	values := make([]ir.Ref, 0, signature.Params().Len())
	for index := 0; index < variadicIndex; index++ {
		parameterType := signature.Params().At(index).Type()
		value := g.assignmentValue(arguments[index], parameterType)
		if isInlineAggregate(parameterType) {
			value = g.copyInlineValue(value, parameterType)
		}
		values = append(values, value)
	}

	sliceType := signature.Params().At(variadicIndex).Type()
	if hasEllipsis {
		value := g.assignmentValue(arguments[variadicIndex], sliceType)
		values = append(values, g.copyInlineValue(value, sliceType))
		return values
	}

	elementType := sliceType.Underlying().(*types.Slice).Elem()
	variadicArguments := arguments[variadicIndex:]
	length := int64(len(variadicArguments))
	if length == 0 {
		zero := g.fn.Long(0)
		values = append(values, g.sliceDescriptor(g.fn.ConstInt(ir.ClsP, 0), zero, zero))
		return values
	}

	arrayType := types.NewArray(elementType, length)
	var backing ir.Ref
	interfacePayloads := make(map[int]ir.Ref)
	if g.runtimeAllocation {
		allocationType := types.Type(arrayType)
		fields := []*types.Var{
			types.NewVar(token.NoPos, nil, "values", arrayType),
		}
		payloadFields := make(map[int]int)
		if isInterfaceValue(elementType) {
			for index, argument := range variadicArguments {
				if !g.interfaceAssignmentNeedsPayload(argument) {
					continue
				}
				payloadFields[index] = len(fields)
				fieldName := fmt.Sprintf("payload%d", index)
				fieldType := g.typeAndValue(argument).Type
				fields = append(fields, types.NewVar(token.NoPos, nil, fieldName, fieldType))
			}
		}
		if len(fields) > 1 {
			combinedType := types.NewStruct(fields, nil)
			allocationType = combinedType
			offsets := structOffsets(fields)
			backing = g.allocateTyped(allocationType)
			for argumentIndex, fieldIndex := range payloadFields {
				interfacePayloads[argumentIndex] = g.offset(backing, offsets[fieldIndex])
			}
		} else {
			backing = g.allocateTyped(allocationType)
		}
	} else {
		backing = g.localAllocTyped(arrayType)
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memset", 0), backing, g.fn.Word(0), g.fn.Long(typeSize(arrayType)))
	}
	elementSize := typeSize(elementType)
	for index, argument := range variadicArguments {
		value := g.assignmentValueWithInterfacePayload(argument, elementType, interfacePayloads[index])
		elementAddress := g.offset(backing, int64(index)*elementSize)
		if isInlineAggregate(elementType) || isInterfaceValue(elementType) {
			g.storeInlineValue(value, elementAddress, elementType)
		} else {
			g.store(value, elementAddress, elementType)
		}
	}
	values = append(values, g.sliceDescriptor(backing, g.fn.Long(length), g.fn.Long(length)))
	return values
}

func (g *gen) interfaceAssignmentNeedsPayload(expression ast.Expr) bool {
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "nil" {
		return false
	}
	sourceType := g.typeAndValue(expression).Type
	if sourceType == nil {
		return false
	}
	if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
		return false
	}
	return !isDirectInterfaceType(sourceType)
}

func (g *gen) evaluateMultiValueCall(call *ast.CallExpr) ([]ir.Ref, *types.Signature) {
	var function *types.Func
	var receiver ir.Ref
	switch expression := call.Fun.(type) {
	case *ast.Ident:
		function, _ = g.info.Uses[expression].(*types.Func)
	case *ast.SelectorExpr:
		function, _ = g.info.Uses[expression.Sel].(*types.Func)
		selection := g.info.Selections[expression]
		if selection != nil && selection.Kind() == types.MethodVal {
			receiver = g.methodReceiver(expression, function)
			if methodHasInterfaceReceiver(function) {
				g.interfaceMethods[function] = true
			}
		}
	}
	signature, ok := g.typeAndValue(call.Fun).Type.Underlying().(*types.Signature)
	if !ok || signature.Results().Len() < 2 {
		g.fail(call, "call is not multi-valued")
		return nil, nil
	}

	callSignature := signature
	var callee ir.Ref
	var closure ir.Ref
	if function != nil {
		calleeName := g.functionSymbol(function)
		callSignature = compiledFunctionSignature(function)
		if instanceName, instantiated := g.instantiatedFunctionSymbol(function, call.Fun); instantiated {
			calleeName = instanceName
			callSignature = signature
		}
		callee = g.fn.Sym(calleeName, 0)
	} else {
		closure = g.expr(call.Fun)
		callee = g.cur.Load(ir.ClsP, closure)
	}

	arguments := make([]ir.Ref, 0, len(call.Args)+signature.Results().Len())
	if receiver != ir.R {
		arguments = append(arguments, receiver)
	}
	arguments = append(arguments, g.callArguments(call.Args, call.Ellipsis.IsValid(), signature)...)
	arguments = g.adaptSharedGenericArguments(arguments, signature, callSignature, receiver != ir.R)
	if closure != ir.R {
		g.pinClosure(closure)
	}

	values := make([]ir.Ref, signature.Results().Len())
	firstResultType := signature.Results().At(0).Type()
	if isInlineAggregate(firstResultType) && !g.runtimeAllocation {
		arguments = append(arguments, g.aggregateResultStorage(firstResultType))
	}
	for index := 1; index < signature.Results().Len(); index++ {
		resultType := signature.Results().At(index).Type()
		slot := g.alloc(resultType)
		if isInlineAggregate(resultType) {
			slot = g.aggregateResultStorage(resultType)
		}
		arguments = append(arguments, slot)
		values[index] = slot
	}

	resultClass, _ := scalar(firstResultType)
	var receiverType types.Type
	if receiver != ir.R && function != nil {
		functionSignature := compiledFunctionSignature(function)
		if functionSignature.Recv() != nil {
			receiverType = functionSignature.Recv().Type()
		}
	}
	values[0] = g.callWithSignature(resultClass, callee, arguments, callSignature, receiverType)
	for index := 1; index < len(values); index++ {
		resultType := signature.Results().At(index).Type()
		if !isInlineAggregate(resultType) || (g.runtimeAllocation && isSliceType(resultType)) {
			values[index] = g.load(values[index], resultType)
		}
	}
	return values, signature
}

func (g *gen) initializeGlobal(object types.Object) {
	if _, exists := g.dynamicInitializers[object]; !exists || g.initializingGlobals[object] || !g.live() {
		return
	}
	symbol := g.dynamicInitializerFunctions[object]
	if symbol == "" {
		return
	}
	g.cur.CallVoid(g.fn.Sym(symbol, 0))
}

func (g *gen) emitDynamicGlobalInitializer(object types.Object, initializer globalInitializer) {
	if !g.live() {
		return
	}
	guardName := g.dynamicInitializerGuards[object]
	if guardName == "" {
		guardName = fmt.Sprintf(".goc.global.init.%d", len(g.dynamicInitializerGuards))
		g.dynamicInitializerGuards[object] = guardName
		g.mod.Data = append(g.mod.Data, &ir.Data{Name: guardName, Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0}}}})
	}

	initialize := g.block("globalinit")
	done := g.block("globalinitdone")
	guard := g.fn.Sym(guardName, 0)
	alreadyInitialized := g.cur.Load(ir.ClsW, guard)
	g.cur.Jnz(alreadyInitialized, done, initialize)

	g.cur = initialize
	g.cur.Store(g.fn.Word(1), guard)
	savedInfo := g.info
	savedPackage := g.pkg
	savedParents := g.parents
	savedBody := g.currentBody
	g.info = initializer.info
	g.pkg = initializer.pkg
	g.parents = astParents(initializer.expression)
	g.currentBody = nil
	g.initializingGlobals[object] = true

	value := g.assignmentValue(initializer.expression, object.Type())
	destination := g.fn.Sym(g.globals[object], 0)
	valueType := object.Type()
	switch {
	case isMemoryValue(valueType), isInterfaceValue(valueType):
		g.storeInlineValue(value, destination, valueType)
	case g.runtimeAllocation && isSliceType(valueType):
		g.store(value, destination, valueType)
	case isDescriptorValue(valueType):
		// Legacy string and slice globals contain a pointer to permanent descriptor
		// storage. Copy a dynamic initializer into that storage instead of
		// retaining the address of a temporary descriptor in the init frame.
		descriptor := g.cur.Load(ir.ClsP, destination)
		g.storeInlineValue(value, descriptor, valueType)
	default:
		g.store(value, destination, valueType)
	}

	delete(g.initializingGlobals, object)
	g.info = savedInfo
	g.pkg = savedPackage
	g.parents = savedParents
	g.currentBody = savedBody
	if g.live() {
		g.cur.Goto(done)
	}
	g.cur = done
}

func (g *gen) typeTag(valueType types.Type) ir.Ref {
	return g.fn.Sym(g.ensureTypeTag(valueType), 0)
}

func (g *gen) ensureTypeTag(valueType types.Type) string {
	valueType = canonicalAliasType(valueType)
	key := runtimeTypeKey(valueType)
	if g.runtimeTypes != nil {
		g.runtimeTypes[key] = valueType
	}
	name := g.typeTags[key]
	if name == "" {
		name = fmt.Sprintf(".goc.type.%d", len(g.typeTags))
		g.typeTags[key] = name
		gcDataName := name + ".gcdata"
		mask := pointerMask(valueType)
		if len(mask) == 0 {
			mask = []int64{0}
		}
		alignment := typeAlign(valueType)
		tflag := int64(0)
		var methods []runtimeMethod
		if g.emitRuntimeTables {
			methods = runtimeTypeMethods(valueType)
		}
		hasUncommon := g.emitRuntimeTables && (runtimeTypeIsNamed(valueType) || len(methods) > 0)
		if hasUncommon {
			tflag |= 1 << 0
		}
		if g.emitRuntimeTables && runtimeTypeIsNamed(valueType) {
			tflag |= 1 << 2
		}
		if isRuntimeRegularMemory(valueType) {
			tflag |= 1 << 3
		}
		if isDirectInterfaceType(valueType) {
			tflag |= 1 << 5
		}
		equalItem := ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
		if g.emitRuntimeTables {
			if equalSymbol := runtimeEqualSymbol(valueType); equalSymbol != "" {
				equalItem = ir.DataItem{Sub: ir.SubL, Sym: g.staticFunctionDescriptor(equalSymbol)}
			}
		}
		typeNameItem := ir.DataItem{Sub: ir.SubW, Ints: []int64{0}}
		if g.emitRuntimeTables {
			typeName := name + ".name"
			g.emitRuntimeName(typeName, runtimeTypeName(valueType), "", runtimeTypeNameExported(valueType), false)
			typeNameItem = ir.DataItem{Sub: ir.SubW, Sym: typeName, RelativeTo: ".goc.runtime.datastart"}
		}
		items := []ir.DataItem{
			{Sub: ir.SubL, Ints: []int64{typeSize(valueType), runtimePointerBytes(valueType)}},
			{Sub: ir.SubW, Ints: []int64{0}},
			{Sub: ir.SubUB, Ints: []int64{tflag, alignment, alignment, int64(runtimeKind(valueType))}},
			equalItem,
			{Sub: ir.SubL, Sym: gcDataName},
			typeNameItem,
			{Sub: ir.SubW, Ints: []int64{0}},
		}
		var variableItems []ir.DataItem
		switch value := valueType.Underlying().(type) {
		case *types.Array:
			items = append(items,
				ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Elem())},
				ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(types.NewSlice(value.Elem()))},
				ir.DataItem{Sub: ir.SubL, Ints: []int64{value.Len()}},
			)
		case *types.Chan:
			items = append(items,
				ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Elem())},
				ir.DataItem{Sub: ir.SubL, Ints: []int64{runtimeChannelDirection(value.Dir())}},
			)
		case *types.Pointer:
			items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Elem())})
		case *types.Slice:
			items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Elem())})
		case *types.Struct:
			packagePathItem := ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
			if packagePath := runtimeTypePackagePath(valueType); packagePath != "" {
				packagePathName := name + ".pkgpath"
				g.emitRuntimeName(packagePathName, packagePath, "", false, false)
				packagePathItem = ir.DataItem{Sub: ir.SubL, Sym: packagePathName}
			}

			fields := structFields(value)
			offsets := structOffsets(fields)
			fieldItems := make([]ir.DataItem, 0, len(fields)*3)
			for index, field := range fields {
				fieldName := fmt.Sprintf("%s.field.%d.name", name, index)
				g.emitRuntimeName(fieldName, field.Name(), value.Tag(index), ast.IsExported(field.Name()), field.Embedded())
				fieldItems = append(fieldItems,
					ir.DataItem{Sub: ir.SubL, Sym: fieldName},
					ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(field.Type())},
					ir.DataItem{Sub: ir.SubL, Ints: []int64{offsets[index]}},
				)
			}

			fieldDataItem := ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
			if len(fieldItems) > 0 {
				fieldDataName := name + ".fields"
				g.mod.Data = append(g.mod.Data, &ir.Data{Name: fieldDataName, Align: 8, Items: fieldItems})
				fieldDataItem = ir.DataItem{Sub: ir.SubL, Sym: fieldDataName}
			}
			items = append(items,
				packagePathItem,
				fieldDataItem,
				ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(len(fields)), int64(len(fields))}},
			)
		case *types.Signature:
			outCount := value.Results().Len()
			if value.Variadic() {
				outCount |= 1 << 15
			}
			items = append(items,
				ir.DataItem{Sub: ir.SubUH, Ints: []int64{int64(value.Params().Len()), int64(outCount)}},
				ir.DataItem{Zero: 4},
			)
			for index := 0; index < value.Params().Len(); index++ {
				variableItems = append(variableItems, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Params().At(index).Type())})
			}
			for index := 0; index < value.Results().Len(); index++ {
				variableItems = append(variableItems, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Results().At(index).Type())})
			}
		case *types.Interface:
			methodCount := 0
			if g.emitRuntimeTables {
				methodCount = value.NumMethods()
			}
			packagePathItem := ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
			if packagePath := runtimeTypePackagePath(valueType); packagePath != "" {
				packagePathName := name + ".pkgpath"
				g.emitRuntimeName(packagePathName, packagePath, "", false, false)
				packagePathItem = ir.DataItem{Sub: ir.SubL, Sym: packagePathName}
			}

			methodDataItem := ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
			if methodCount > 0 {
				methodDataName := name + ".imethods"
				methodItems := make([]ir.DataItem, 0, methodCount*2)
				for index := 0; index < methodCount; index++ {
					method := value.Method(index)
					methodName := fmt.Sprintf("%s.imethod.%d.name", name, index)
					g.emitRuntimeName(methodName, method.Name(), "", method.Exported(), false)
					methodItems = append(methodItems,
						ir.DataItem{Sub: ir.SubW, Sym: methodName, RelativeTo: ".goc.runtime.datastart"},
						ir.DataItem{Sub: ir.SubW, Sym: g.ensureTypeTag(method.Type()), RelativeTo: ".goc.runtime.datastart"},
					)
				}
				g.mod.Data = append(g.mod.Data, &ir.Data{Name: methodDataName, Align: 4, Items: methodItems})
				methodDataItem = ir.DataItem{Sub: ir.SubL, Sym: methodDataName}
			}
			items = append(items,
				packagePathItem,
				methodDataItem,
				ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(methodCount), int64(methodCount)}},
			)
		case *types.Map:
			keyTypeTag := g.ensureTypeTag(value.Key())
			g.ensureRuntimeTypeEqual(value.Key(), keyTypeTag)
			elementTypeTag := g.ensureTypeTag(value.Elem())
			groupType, groupSize, slotSize, elementOffset, mapFlags := runtimeMapGroupType(value)
			groupTypeTag := g.ensureTypeTag(groupType)
			hasher := runtimeHashSymbol(value.Key())
			if g.emitRuntimeTables && hasher == "" {
				hasher = g.emitRuntimeTypeHasher(value.Key(), keyTypeTag)
			}
			hasherItem := ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
			if g.emitRuntimeTables && hasher != "" {
				hasherItem = ir.DataItem{Sub: ir.SubL, Sym: g.staticFunctionDescriptor(hasher)}
			}
			items = append(items,
				ir.DataItem{Sub: ir.SubL, Sym: keyTypeTag},
				ir.DataItem{Sub: ir.SubL, Sym: elementTypeTag},
				ir.DataItem{Sub: ir.SubL, Sym: groupTypeTag},
				hasherItem,
				ir.DataItem{Sub: ir.SubL, Ints: []int64{groupSize, slotSize, elementOffset}},
				ir.DataItem{Sub: ir.SubW, Ints: []int64{mapFlags}},
				ir.DataItem{Zero: 4},
			)
		}
		if hasUncommon {
			packagePathItem := ir.DataItem{Sub: ir.SubW, Ints: []int64{0}}
			if packagePath := runtimeTypePackagePath(valueType); packagePath != "" {
				packagePathName := name + ".uncommon.pkgpath"
				g.emitRuntimeName(packagePathName, packagePath, "", false, false)
				packagePathItem = ir.DataItem{Sub: ir.SubW, Sym: packagePathName, RelativeTo: ".goc.runtime.datastart"}
			}
			exportedMethodCount := runtimeExportedMethodCount(methods)
			methodOffset := 16 + runtimeDataItemsSize(variableItems)
			items = append(items,
				packagePathItem,
				ir.DataItem{Sub: ir.SubUH, Ints: []int64{int64(len(methods)), int64(exportedMethodCount)}},
				ir.DataItem{Sub: ir.SubW, Ints: []int64{int64(methodOffset), 0}},
			)
		}
		items = append(items, variableItems...)
		for index, method := range methods {
			methodName := fmt.Sprintf("%s.method.%d.name", name, index)
			g.emitRuntimeName(methodName, method.function.Name(), "", method.function.Exported(), false)
			methodType := g.ensureTypeTag(method.signature)
			methodSymbol := g.functionSymbol(method.function)
			items = append(items,
				ir.DataItem{Sub: ir.SubW, Sym: methodName, RelativeTo: ".goc.runtime.datastart"},
				ir.DataItem{Sub: ir.SubW, Sym: methodType, RelativeTo: ".goc.runtime.datastart"},
				ir.DataItem{Sub: ir.SubW, Sym: methodSymbol},
				ir.DataItem{Sub: ir.SubW, Sym: methodSymbol},
			)
		}
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name: gcDataName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: mask}},
		}, &ir.Data{Name: name, Align: 8, Items: items})
	}
	return name
}

func populateRuntimePointerTypes(module *ir.Module, typeTags map[string]string, runtimeTypes map[string]types.Type) {
	dataByName := make(map[string]*ir.Data, len(module.Data))
	for _, data := range module.Data {
		dataByName[data.Name] = data
	}
	for key, valueType := range runtimeTypes {
		typeSymbol := typeTags[key]
		pointerSymbol := typeTags[runtimeTypeKey(types.NewPointer(valueType))]
		if typeSymbol == "" || pointerSymbol == "" {
			continue
		}
		data := dataByName[typeSymbol]
		if data == nil || len(data.Items) < 7 {
			continue
		}
		data.Items[6] = ir.DataItem{
			Sub:        ir.SubW,
			Sym:        pointerSymbol,
			RelativeTo: ".goc.runtime.datastart",
		}
	}
}

func runtimeMapGroupType(mapType *types.Map) (types.Type, int64, int64, int64, int64) {
	const (
		mapGroupSlots    = 8
		mapMaxKeyBytes   = 128
		mapMaxValueBytes = 128

		mapNeedKeyUpdate  = 1 << 0
		mapHashMightPanic = 1 << 1
		mapIndirectKey    = 1 << 2
		mapIndirectValue  = 1 << 3
	)

	keyType := mapType.Key()
	valueType := mapType.Elem()
	flags := int64(0)
	if runtimeMapKeyNeedsUpdate(keyType) {
		flags |= mapNeedKeyUpdate
	}
	if runtimeMapHashMightPanic(keyType) {
		flags |= mapHashMightPanic
	}
	if typeSize(keyType) > mapMaxKeyBytes {
		keyType = types.NewPointer(keyType)
		flags |= mapIndirectKey
	}
	if typeSize(valueType) > mapMaxValueBytes {
		valueType = types.NewPointer(valueType)
		flags |= mapIndirectValue
	}

	slotFields := []*types.Var{
		types.NewVar(token.NoPos, nil, "key", keyType),
		types.NewVar(token.NoPos, nil, "value", valueType),
	}
	slotType := types.NewStruct(slotFields, nil)
	elementOffset := structOffsets(slotFields)[1]
	slotsType := types.NewArray(slotType, mapGroupSlots)
	groupType := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "control", types.Typ[types.Uint64]),
		types.NewVar(token.NoPos, nil, "slots", slotsType),
	}, nil)

	return groupType, typeSize(groupType), typeSize(slotType), elementOffset, flags
}

func runtimeMapKeyNeedsUpdate(valueType types.Type) bool {
	switch value := valueType.Underlying().(type) {
	case *types.Basic:
		switch value.Kind() {
		case types.Float32, types.Float64, types.Complex64, types.Complex128, types.String:
			return true
		default:
			return false
		}
	case *types.Interface:
		return true
	case *types.Array:
		return runtimeMapKeyNeedsUpdate(value.Elem())
	case *types.Struct:
		for index := 0; index < value.NumFields(); index++ {
			if runtimeMapKeyNeedsUpdate(value.Field(index).Type()) {
				return true
			}
		}
	}
	return false
}

func runtimeMapHashMightPanic(valueType types.Type) bool {
	switch value := valueType.Underlying().(type) {
	case *types.Interface:
		return true
	case *types.Array:
		return runtimeMapHashMightPanic(value.Elem())
	case *types.Struct:
		for index := 0; index < value.NumFields(); index++ {
			if runtimeMapHashMightPanic(value.Field(index).Type()) {
				return true
			}
		}
	}
	return false
}

type runtimeMethod struct {
	function  *types.Func
	signature *types.Signature
}

func runtimeTypeMethods(valueType types.Type) []runtimeMethod {
	if _, isInterface := valueType.Underlying().(*types.Interface); isInterface {
		return nil
	}
	if _, isTypeParameter := valueType.(*types.TypeParam); isTypeParameter {
		return nil
	}

	methodSet := types.NewMethodSet(valueType)
	methods := make([]runtimeMethod, 0, methodSet.Len())
	for index := 0; index < methodSet.Len(); index++ {
		selection := methodSet.At(index)
		function, ok := selection.Obj().(*types.Func)
		if !ok {
			continue
		}
		signature, ok := selection.Type().(*types.Signature)
		if !ok {
			continue
		}
		methods = append(methods, runtimeMethod{function: function, signature: signature})
	}
	sort.SliceStable(methods, func(left, right int) bool {
		leftExported := methods[left].function.Exported()
		rightExported := methods[right].function.Exported()
		if leftExported != rightExported {
			return leftExported
		}
		return methods[left].function.Id() < methods[right].function.Id()
	})
	return methods
}

func runtimeExportedMethodCount(methods []runtimeMethod) int {
	count := 0
	for _, method := range methods {
		if !method.function.Exported() {
			break
		}
		count++
	}
	return count
}

func runtimeDataItemsSize(items []ir.DataItem) int {
	size := 0
	for _, item := range items {
		switch {
		case item.Zero > 0:
			size += item.Zero
		case item.Str != "":
			size += len(item.Str)
		case item.Sym != "":
			size += item.Sub.Size()
		case len(item.Flts) > 0:
			size += len(item.Flts) * item.Sub.Size()
		default:
			size += len(item.Ints) * item.Sub.Size()
		}
	}
	return size
}

func runtimeTypeIsNamed(valueType types.Type) bool {
	_, named := types.Unalias(valueType).(*types.Named)
	return named
}

func runtimeTypeKey(valueType types.Type) string {
	valueType = canonicalAliasType(valueType)
	if signature, ok := valueType.(*types.Signature); ok {
		parameters := runtimeAnonymousTuple(signature.Params())
		results := runtimeAnonymousTuple(signature.Results())
		valueType = types.NewSignatureType(nil, nil, nil, parameters, results, signature.Variadic())
	}
	key := types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Path()
	})
	return appendLocalTypeIdentities(key, valueType)
}

func runtimeAnonymousTuple(tuple *types.Tuple) *types.Tuple {
	variables := make([]*types.Var, tuple.Len())
	for index := 0; index < tuple.Len(); index++ {
		variables[index] = types.NewVar(token.NoPos, nil, "", tuple.At(index).Type())
	}
	return types.NewTuple(variables...)
}

func runtimeTypeName(valueType types.Type) string {
	return types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Name()
	})
}

func runtimeTypeNameExported(valueType types.Type) bool {
	valueType = types.Unalias(valueType)
	if named, ok := valueType.(*types.Named); ok {
		return named.Obj() != nil && named.Obj().Exported()
	}
	if pointer, ok := valueType.(*types.Pointer); ok {
		if named, ok := types.Unalias(pointer.Elem()).(*types.Named); ok {
			return named.Obj() != nil && named.Obj().Exported()
		}
	}
	return false
}

func clearUnavailableRuntimeMethodOffsets(module *ir.Module) {
	functions := make(map[string]bool, len(module.Funcs))
	for _, function := range module.Funcs {
		functions[function.Name] = true
	}
	for _, data := range module.Data {
		if strings.HasPrefix(data.Name, ".goc.type.") {
			for index := range data.Items {
				item := &data.Items[index]
				if item.Sub != ir.SubW || item.Sym == "" || item.RelativeTo != "" || functions[item.Sym] {
					continue
				}
				*item = ir.DataItem{Sub: ir.SubW, Sym: "runtime.unreachableMethod"}
			}
		}
		if strings.HasPrefix(data.Name, ".goc.itab.") {
			for index := 4; index < len(data.Items); index++ {
				item := &data.Items[index]
				if item.Sub != ir.SubL || item.Sym == "" || functions[item.Sym] {
					continue
				}
				*item = ir.DataItem{Sub: ir.SubL, Sym: "runtime.unreachableMethod"}
			}
		}
	}
}

func (g *gen) emitRuntimeTypeHasher(valueType types.Type, typeTag string) string {
	symbol := typeTag + ".hash"
	function := g.mod.NewFunc(symbol, ir.ClsL)
	value := function.Param("value", ir.ClsP)
	seed := function.Param("seed", ir.ClsL)
	entry := function.Entry()
	hash := entry.Call(
		ir.ClsL,
		function.Sym("runtime.typehash", 0),
		function.Sym(typeTag, 0),
		value,
		seed,
	)
	entry.Ret(hash)
	return symbol
}

func (g *gen) ensureRuntimeTypeEqual(valueType types.Type, typeTag string) string {
	if equalSymbol := runtimeEqualSymbol(valueType); equalSymbol != "" {
		return equalSymbol
	}
	if !g.emitRuntimeTables || !types.Comparable(valueType) {
		return ""
	}
	if typeTag == "" {
		typeTag = g.ensureTypeTag(valueType)
	}

	symbol := typeTag + ".equal"
	for _, function := range g.mod.Funcs {
		if function.Name == symbol {
			return symbol
		}
	}

	g.emitRuntimeTypeEqual(valueType, symbol)
	descriptor := g.staticFunctionDescriptor(symbol)
	for _, data := range g.mod.Data {
		if data.Name != typeTag || len(data.Items) < 4 {
			continue
		}
		data.Items[3] = ir.DataItem{Sub: ir.SubL, Sym: descriptor}
		break
	}
	return symbol
}

func (g *gen) emitRuntimeTypeEqual(valueType types.Type, symbol string) {
	function := g.mod.NewFunc(symbol, ir.ClsW)
	left := function.Param("left", ir.ClsP)
	right := function.Param("right", ir.ClsP)
	entry := function.Entry()
	unequal := function.NewBlock("unequal")
	nextBlock := 0

	done := g.emitRuntimeTypeEqualityChecks(function, entry, unequal, valueType, left, right, &nextBlock)
	done.Ret(function.Word(1))
	unequal.Ret(function.Word(0))
}

func (g *gen) emitRuntimeTypeEqualityChecks(
	function *ir.Func,
	current *ir.Block,
	unequal *ir.Block,
	valueType types.Type,
	left ir.Ref,
	right ir.Ref,
	nextBlock *int,
) *ir.Block {
	switch value := valueType.Underlying().(type) {
	case *types.Array:
		if value.Len() == 0 {
			return current
		}
		elementEqual := g.ensureRuntimeTypeEqual(value.Elem(), "")
		elementSize := typeSize(value.Elem())
		loop := function.NewBlock("array.loop")
		body := function.NewBlock("array.body")
		equalElement := function.NewBlock("array.equal")
		done := function.NewBlock("array.done")
		current.Goto(loop)
		index := loop.Phi(ir.ClsL, ir.PhiEdge{From: current, Val: function.Long(0)})
		inRange := loop.Cmp(ir.CmpUlt, ir.ClsL, index, function.Long(value.Len()))
		loop.Jnz(inRange, body, done)

		offset := index
		if elementSize != 1 {
			offset = body.Mul(ir.ClsL, index, function.Long(elementSize))
		}
		leftElement := body.Add(ir.ClsP, left, offset)
		rightElement := body.Add(ir.ClsP, right, offset)
		equal := body.Call(ir.ClsW, function.Sym(elementEqual, 0), leftElement, rightElement)
		body.Jnz(equal, equalElement, unequal)
		nextIndex := equalElement.Add(ir.ClsL, index, function.Long(1))
		equalElement.Goto(loop)
		loop.Phis[0].Add(equalElement, nextIndex)
		return done
	case *types.Struct:
		fields := structFields(value)
		offsets := structOffsets(fields)
		for index, field := range fields {
			if field.Name() == "_" {
				continue
			}
			offset := offsets[index]
			equalSymbol := g.ensureRuntimeTypeEqual(field.Type(), "")
			leftField := offsetRuntimeTypeEqualityAddress(current, function, left, offset)
			rightField := offsetRuntimeTypeEqualityAddress(current, function, right, offset)
			equal := current.Call(ir.ClsW, function.Sym(equalSymbol, 0), leftField, rightField)
			current = branchRuntimeTypeEquality(function, current, unequal, equal, nextBlock)
		}
		return current
	default:
		panic(fmt.Sprintf("goc: no runtime equality implementation for %s", valueType))
	}
}

func branchRuntimeTypeEquality(
	function *ir.Func,
	current *ir.Block,
	unequal *ir.Block,
	equal ir.Ref,
	nextBlock *int,
) *ir.Block {
	name := fmt.Sprintf("equal.next.%d", *nextBlock)
	*nextBlock = *nextBlock + 1
	next := function.NewBlock(name)
	current.Jnz(equal, next, unequal)
	return next
}

func offsetRuntimeTypeEqualityAddress(current *ir.Block, function *ir.Func, address ir.Ref, offset int64) ir.Ref {
	if offset == 0 {
		return address
	}
	return current.Add(ir.ClsP, address, function.Long(offset))
}

func (g *gen) emitRuntimeName(symbol, name, tag string, exported, embedded bool) {
	encoded := runtimeNameBytes(name, tag, exported, embedded)
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  symbol,
		Align: 1,
		Items: []ir.DataItem{{Sub: ir.SubUB, Str: string(encoded)}},
	})
}

func runtimeNameBytes(name, tag string, exported, embedded bool) []byte {
	var flags byte
	if exported {
		flags |= 1 << 0
	}
	if tag != "" {
		flags |= 1 << 1
	}
	if embedded {
		flags |= 1 << 3
	}

	encoded := []byte{flags}
	encoded = appendRuntimeNameLength(encoded, len(name))
	encoded = append(encoded, name...)
	if tag != "" {
		encoded = appendRuntimeNameLength(encoded, len(tag))
		encoded = append(encoded, tag...)
	}
	return encoded
}

func appendRuntimeNameLength(encoded []byte, length int) []byte {
	for {
		value := byte(length & 0x7f)
		length >>= 7
		if length == 0 {
			return append(encoded, value)
		}
		encoded = append(encoded, value|0x80)
	}
}

func runtimeTypePackagePath(valueType types.Type) string {
	valueType = types.Unalias(valueType)
	named, ok := valueType.(*types.Named)
	if !ok {
		switch value := valueType.Underlying().(type) {
		case *types.Array:
			valueType = value.Elem()
		case *types.Chan:
			valueType = value.Elem()
		case *types.Pointer:
			valueType = value.Elem()
		case *types.Slice:
			valueType = value.Elem()
		default:
			return ""
		}
		named, ok = types.Unalias(valueType).(*types.Named)
	}
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path()
}

func runtimeChannelDirection(direction types.ChanDir) int64 {
	switch direction {
	case types.SendOnly:
		return 2
	case types.RecvOnly:
		return 1
	default:
		return 3
	}
}

func runtimeEqualSymbol(valueType types.Type) string {
	if interfaceType, ok := valueType.Underlying().(*types.Interface); ok {
		if interfaceType.NumMethods() != 0 {
			return "runtime.interequal"
		}
		return "runtime.nilinterequal"
	}
	switch valueType.Underlying().(type) {
	case *types.Pointer, *types.Chan:
		return "runtime.memequal64"
	}
	if basic, ok := valueType.Underlying().(*types.Basic); ok {
		switch basic.Kind() {
		case types.String:
			return "runtime.strequal"
		case types.Float32:
			return "runtime.f32equal"
		case types.Float64:
			return "runtime.f64equal"
		case types.Complex64:
			return "runtime.c64equal"
		case types.Complex128:
			return "runtime.c128equal"
		}
	}
	if isRuntimeRegularMemory(valueType) {
		return fixedSizeRuntimeFunction("runtime.memequal", typeSize(valueType))
	}
	return ""
}

func runtimeHashSymbol(valueType types.Type) string {
	if interfaceType, ok := valueType.Underlying().(*types.Interface); ok {
		if interfaceType.NumMethods() == 0 {
			return "runtime.nilinterhash"
		}
		return "runtime.interhash"
	}
	if basic, ok := valueType.Underlying().(*types.Basic); ok {
		switch basic.Kind() {
		case types.String:
			return "runtime.strhash"
		case types.Float32:
			return "runtime.f32hash"
		case types.Float64:
			return "runtime.f64hash"
		case types.Complex64:
			return "runtime.c64hash"
		case types.Complex128:
			return "runtime.c128hash"
		}
	}
	if isRuntimeRegularMemory(valueType) {
		return fixedSizeRuntimeFunction("runtime.memhash", typeSize(valueType))
	}
	return ""
}

func fixedSizeRuntimeFunction(prefix string, size int64) string {
	switch size {
	case 0:
		return prefix + "0"
	case 1:
		return prefix + "8"
	case 2:
		return prefix + "16"
	case 4:
		return prefix + "32"
	case 8:
		return prefix + "64"
	case 16:
		return prefix + "128"
	default:
		return ""
	}
}

func isRuntimeRegularMemory(valueType types.Type) bool {
	switch value := valueType.Underlying().(type) {
	case *types.Basic:
		switch value.Kind() {
		case types.Bool,
			types.Int, types.Int8, types.Int16, types.Int32, types.Int64,
			types.Uint, types.Uint8, types.Uint16, types.Uint32, types.Uint64, types.Uintptr,
			types.UnsafePointer:
			return true
		}
	case *types.Pointer, *types.Chan:
		return true
	case *types.Array:
		return isRuntimeRegularMemory(value.Elem())
	case *types.Struct:
		offsets := structOffsets(structFields(value))
		next := int64(0)
		for index := 0; index < value.NumFields(); index++ {
			field := value.Field(index)
			if offsets[index] != next || !isRuntimeRegularMemory(field.Type()) {
				return false
			}
			next += typeSize(field.Type())
		}
		return next == typeSize(value)
	}
	return false
}

func isDirectInterfaceType(valueType types.Type) bool {
	switch value := valueType.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		return true
	case *types.Basic:
		return value.Kind() == types.UnsafePointer
	default:
		return false
	}
}

func pointerMask(valueType types.Type) []int64 {
	words := pointerWordIndices(valueType)
	if len(words) == 0 {
		return nil
	}
	mask := make([]int64, words[len(words)-1]/8+1)
	for _, word := range words {
		mask[word/8] |= 1 << (word % 8)
	}
	return mask
}

func runtimePointerBytes(valueType types.Type) int64 {
	words := pointerWordIndices(valueType)
	if len(words) == 0 {
		return 0
	}
	return int64(words[len(words)-1]+1) * pointerSize()
}

func (g *gen) funcDecl(fd *ast.FuncDecl) {
	obj := g.info.Defs[fd.Name].(*types.Func)
	g.currentFunction = obj
	originalSignature := obj.Type().(*types.Signature)
	sig := g.concreteType(obj.Type()).(*types.Signature)
	isMain := g.pkg.Name() == "main" && obj.Name() == "main"
	platformMain := isMain && !g.runtimeAllocation
	name := g.functionSymbol(obj)
	if g.functionName != "" {
		name = g.functionName
	}
	if platformMain {
		name = "main"
	}
	if platformMain {
		// The executable entry point is linked by the platform C startup code,
		// whose main ABI returns int. A source-level Go main still has no result.
		g.fn = g.mod.NewFunc(name, ir.ClsW)
	} else if sig.Results().Len() == 0 {
		g.fn = g.mod.NewFuncVoid(name)
	} else if c, ok := scalar(sig.Results().At(0).Type()); ok {
		g.fn = g.mod.NewFunc(name, c)
	} else {
		g.fail(fd, "unsupported return type %s", sig.Results().At(0).Type())
		return
	}
	var resultAggregate *ir.AggType
	if sig.Results().Len() > 0 {
		resultAggregate = g.goABIAggregate(sig.Results().At(0).Type())
		g.fn.RetAgg = resultAggregate
		g.fn.RetValues = g.runtimeAllocation && isSliceType(sig.Results().At(0).Type())
	}
	g.fn.NoSplit = hasCompilerDirective(fd, "go:nosplit")
	g.fn.SystemStack = hasCompilerDirective(fd, "go:systemstack")
	exportRuntimeBootstrap := g.pkg.Path() == "runtime" && (fd.Name.Name == "args" || fd.Name.Name == "check" || fd.Name.Name == "main" || fd.Name.Name == "mstart0" || fd.Name.Name == "newproc" || fd.Name.Name == "newstack" || fd.Name.Name == "osinit" || fd.Name.Name == "schedinit" || fd.Name.Name == "throw")
	if ast.IsExported(fd.Name.Name) || isMain || exportRuntimeBootstrap || g.linkNames[obj] != "" {
		g.fn.Export()
	}
	g.vars = map[types.Object]ir.Ref{}
	g.directValues = map[types.Object]bool{}
	g.stackAddresses = make(map[uint32]bool)
	g.heapCaptures = make(map[types.Object]ir.Ref)
	g.noWriteBarrier = g.noWriteBarrierFunctions[obj]
	g.resultSlot = ir.R
	g.resultType = nil
	g.aggregateResult = ir.R
	g.extraResultSlots = nil
	g.extraResultTypes = nil
	g.labels = make(map[string]*ir.Block)
	g.labeledBreaks = make(map[string]*ir.Block)
	g.labeledContinues = make(map[string]*ir.Block)
	g.deferSlots = make(map[*ast.DeferStmt]ir.Ref)
	g.deferFunctions = make(map[*ast.DeferStmt]ir.Ref)
	g.deferOrder = nil
	g.deferActions = nil
	g.runningDefers = false
	g.parents = astParents(fd.Body)
	g.currentBody = fd.Body
	predeclaredVariables := signatureVariables(originalSignature)
	g.escapingCaptures = g.findEscapingCaptures(fd.Body, predeclaredVariables...)
	g.seq = 0
	g.cur = g.fn.Entry()
	g.at(fd)
	ast.Inspect(fd.Body, func(node ast.Node) bool {
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		label, ok := node.(*ast.LabeledStmt)
		if ok {
			g.labels[label.Label.Name] = g.block("label_" + label.Label.Name)
		}
		deferStatement, ok := node.(*ast.DeferStmt)
		if ok {
			slot := g.alloc(types.Typ[types.Bool])
			g.store(g.fn.Word(0), slot, types.Typ[types.Bool])
			g.deferSlots[deferStatement] = slot
			if len(deferStatement.Call.Args) == 0 {
				functionSlot := g.alloc(types.Typ[types.UnsafePointer])
				g.store(g.fn.ConstInt(ir.ClsP, 0), functionSlot, types.Typ[types.UnsafePointer])
				g.deferFunctions[deferStatement] = functionSlot
			}
			g.deferOrder = append(g.deferOrder, deferStatement)
		}
		return true
	})
	if receiver := sig.Recv(); receiver != nil {
		originalReceiver := originalSignature.Recv()
		cls, ok := scalar(receiver.Type())
		if !ok {
			g.fail(fd, "unsupported receiver type %s", receiver.Type())
			return
		}
		parameter := g.functionParameter(receiver.Name(), receiver.Type(), cls)
		slot := g.variableStorage(originalReceiver, receiver.Type())
		g.assignLocal(parameter, slot, receiver.Type())
		g.vars[originalReceiver] = slot
	}
	for i := 0; i < sig.Params().Len(); i++ {
		v := sig.Params().At(i)
		originalParameter := originalSignature.Params().At(i)
		c, ok := scalar(v.Type())
		if !ok {
			g.fail(fd, "unsupported parameter type %s", v.Type())
			return
		}
		p := g.functionParameter(v.Name(), v.Type(), c)
		slot := g.variableStorage(originalParameter, v.Type())
		g.assignLocal(p, slot, v.Type())
		g.vars[originalParameter] = slot
	}
	if sig.Results().Len() > 0 && isInlineAggregate(sig.Results().At(0).Type()) && resultAggregate == nil {
		g.aggregateResult = g.fn.ParamRef("result0")
	}
	if sig.Results().Len() > 0 {
		g.resultType = sig.Results().At(0).Type()
	}
	for i := 1; i < sig.Results().Len(); i++ {
		result := sig.Results().At(i)
		originalResult := originalSignature.Results().At(i)
		pointer := g.fn.ParamRef(fmt.Sprintf("result%d", i))
		g.extraResultSlots = append(g.extraResultSlots, pointer)
		g.extraResultTypes = append(g.extraResultTypes, result.Type())
		if result.Name() != "" || len(g.deferOrder) != 0 {
			if result.Name() != "" {
				g.vars[originalResult] = pointer
			}
			if isInlineAggregate(result.Type()) && !(g.runtimeAllocation && isSliceType(result.Type())) {
				g.zero(pointer, result.Type())
				if result.Name() != "" {
					g.directValues[originalResult] = true
				}
			} else {
				if g.runtimeAllocation && isSliceType(result.Type()) {
					g.zero(pointer, result.Type())
				} else {
					g.store(g.zeroValue(result.Type()), pointer, result.Type())
				}
			}
		}
	}
	if sig.Results().Len() > 0 && (sig.Results().At(0).Name() != "" || len(g.deferOrder) != 0) {
		result := sig.Results().At(0)
		originalResult := originalSignature.Results().At(0)
		g.resultType = result.Type()
		if (isInlineAggregate(result.Type()) || isInterfaceValue(result.Type())) && !(g.runtimeAllocation && isSliceType(result.Type())) {
			g.resultSlot = g.aggregateResult
			if g.resultSlot == ir.R {
				g.resultSlot = g.aggregateResultStorage(result.Type())
			}
			g.zero(g.resultSlot, result.Type())
			if result.Name() != "" {
				g.directValues[originalResult] = true
			}
		} else if g.runtimeAllocation && isSliceType(result.Type()) {
			g.resultSlot = g.allocLocal(result.Type())
		} else {
			g.resultSlot = g.alloc(result.Type())
			g.store(g.zeroValue(result.Type()), g.resultSlot, result.Type())
		}
		if result.Name() != "" {
			g.vars[originalResult] = g.resultSlot
		}
	}
	g.stmts(fd.Body.List)
	if g.err == nil && !g.live() && g.runtimeAllocation && len(g.deferActions) != 0 {
		g.cur = g.block("deferreturn")
		g.runDefers()
		if sig.Results().Len() == 0 {
			g.cur.RetVoid()
		} else if g.resultSlot != ir.R {
			value := g.resultSlot
			if !(isInlineAggregate(g.resultType) || isInterfaceValue(g.resultType)) || (g.runtimeAllocation && isSliceType(g.resultType)) {
				value = g.load(g.resultSlot, g.resultType)
			}
			g.returnValue(value, g.resultType)
		} else {
			g.returnValue(g.zeroValue(g.resultType), g.resultType)
		}
	}
	if g.err == nil && g.live() {
		g.runDefers()
		if platformMain {
			g.cur.Ret(g.fn.Word(0))
		} else if sig.Results().Len() == 0 {
			g.cur.RetVoid()
		} else {
			g.fail(fd.Body, "missing return")
		}
	}
	if g.err == nil {
		g.terminateUnusedLabels()
	}
}

func (g *gen) functionParameter(name string, valueType types.Type, class ir.Cls) ir.Ref {
	if g.runtimeAllocation && isSliceType(valueType) {
		aggregate := g.goABIAggregate(valueType)
		parts := g.fn.ParamGroup(name, aggregate, ir.ClsP, ir.ClsL, ir.ClsL)
		value := g.fn.Aggregate(aggregate, parts...)
		return g.markManagedValue(value, valueType)
	}
	parameter := g.fn.Param(name, class)
	g.fn.Temp(parameter).Agg = g.goABIAggregate(valueType)
	return g.markManagedValue(parameter, valueType)
}

func hasCompilerDirective(declaration *ast.FuncDecl, directive string) bool {
	if declaration.Doc == nil {
		return false
	}
	for _, comment := range declaration.Doc.List {
		if strings.TrimSpace(strings.TrimPrefix(comment.Text, "//")) == directive {
			return true
		}
	}
	return false
}

func (g *gen) stmts(ss []ast.Stmt) {
	for _, s := range ss {
		if g.err != nil {
			return
		}
		if !g.live() {
			if _, labeled := s.(*ast.LabeledStmt); !labeled {
				continue
			}
		}
		g.stmt(s)
	}
}

func (g *gen) terminateUnusedLabels() {
	for _, block := range g.labels {
		if block.Jmp.Kind == ir.JmpNone {
			block.Hlt()
		}
	}
}

func (g *gen) multiValueAssignment(statement *ast.AssignStmt, call *ast.CallExpr) {
	var object *types.Func
	var receiver ir.Ref
	switch function := call.Fun.(type) {
	case *ast.Ident:
		object, _ = g.info.Uses[function].(*types.Func)
	case *ast.SelectorExpr:
		object, _ = g.info.Uses[function.Sel].(*types.Func)
		selection := g.info.Selections[function]
		if selection != nil && selection.Kind() == types.MethodVal {
			receiver = g.methodReceiver(function, object)
			if methodHasInterfaceReceiver(object) {
				g.interfaceMethods[object] = true
			}
		}
	}
	signature, ok := g.typeAndValue(call.Fun).Type.Underlying().(*types.Signature)
	if !ok {
		g.fail(call, "multiple-result call target is not a function")
		return
	}
	callSignature := signature
	var callee ir.Ref
	var closure ir.Ref
	if object != nil {
		calleeName := g.functionSymbol(object)
		callSignature = compiledFunctionSignature(object)
		if instanceName, instantiated := g.instantiatedFunctionSymbol(object, call.Fun); instantiated {
			calleeName = instanceName
			callSignature = signature
		}
		callee = g.fn.Sym(calleeName, 0)
	} else {
		closure = g.expr(call.Fun)
		callee = g.cur.Load(ir.ClsP, closure)
	}
	if signature.Results().Len() != len(statement.Lhs) {
		g.fail(statement, "assignment count does not match function results")
		return
	}

	arguments := make([]ir.Ref, 0, len(call.Args)+signature.Results().Len())
	if receiver != ir.R {
		arguments = append(arguments, receiver)
	}
	arguments = append(arguments, g.callArguments(call.Args, call.Ellipsis.IsValid(), signature)...)
	arguments = g.adaptSharedGenericArguments(arguments, signature, callSignature, receiver != ir.R)
	if closure != ir.R {
		g.pinClosure(closure)
	}

	values := make([]ir.Ref, signature.Results().Len())
	firstResultType := signature.Results().At(0).Type()
	if isInlineAggregate(firstResultType) && !g.runtimeAllocation {
		arguments = append(arguments, g.aggregateResultStorage(firstResultType))
	}
	for i := 1; i < signature.Results().Len(); i++ {
		resultType := signature.Results().At(i).Type()
		slot := g.alloc(resultType)
		if isInlineAggregate(resultType) {
			slot = g.aggregateResultStorage(resultType)
		}
		arguments = append(arguments, slot)
		values[i] = slot
	}
	resultClass, _ := scalar(signature.Results().At(0).Type())
	var receiverType types.Type
	if receiver != ir.R && object != nil {
		objectSignature := compiledFunctionSignature(object)
		if objectSignature.Recv() != nil {
			receiverType = objectSignature.Recv().Type()
		}
	}
	values[0] = g.callWithSignature(resultClass, callee, arguments, callSignature, receiverType)

	for i, lhs := range statement.Lhs {
		if identifier, ok := lhs.(*ast.Ident); ok && identifier.Name == "_" {
			continue
		}
		resultType := signature.Results().At(i).Type()
		value := values[i]
		if i > 0 && (!isInlineAggregate(resultType) || (g.runtimeAllocation && isSliceType(resultType))) {
			value = g.load(value, resultType)
		}

		var slot ir.Ref
		localIdentifier := false
		if identifier, ok := lhs.(*ast.Ident); ok {
			variable := g.info.Uses[identifier]
			if statement.Tok == token.DEFINE && variable == nil {
				variable = g.info.Defs[identifier]
			}
			var exists bool
			slot, exists = g.addr(variable)
			if !exists {
				slot = g.variableStorage(variable, resultType)
			}
			_, global := g.globals[variable]
			localIdentifier = !global && !g.directValues[variable]
		} else {
			slot = g.lvalue(lhs)
		}
		value = g.coerce(value, resultType)
		if localIdentifier && isMemoryValue(resultType) {
			g.assignLocal(value, slot, resultType)
		} else if localIdentifier && isInterfaceValue(resultType) {
			g.store(value, slot, resultType)
		} else if localIdentifier && isDescriptorValue(resultType) {
			g.store(value, slot, resultType)
		} else if g.runtimeAllocation && isSliceType(resultType) {
			g.store(value, slot, resultType)
		} else if isInlineAggregate(resultType) || isInterfaceValue(resultType) {
			g.storeInlineValue(value, slot, resultType)
		} else {
			g.store(value, slot, resultType)
		}
	}
}

func (g *gen) typeAssertionAssignment(statement *ast.AssignStmt, assertion *ast.TypeAssertExpr) {
	if len(statement.Lhs) != 2 {
		g.fail(statement, "type assertion assignment requires two results")
		return
	}
	value, ok := g.typeAssertion(assertion)
	targetType := g.typeAndValue(assertion.Type).Type
	g.assignResult(statement.Lhs[0], statement.Tok, value, targetType)
	g.assignResult(statement.Lhs[1], statement.Tok, ok, types.Typ[types.Bool])
}

func (g *gen) typeAssertion(assertion *ast.TypeAssertExpr) (ir.Ref, ir.Ref) {
	descriptor := g.expr(assertion.X)
	targetType := g.typeAndValue(assertion.Type).Type
	targetClass, _ := scalar(targetType)
	failureValue := g.zeroValue(targetType)
	sliceResult := g.runtimeAllocation && isSliceType(targetType)
	resultStorage := ir.R
	if sliceResult {
		resultStorage = g.localAllocTyped(targetType)
	}

	nonNil := g.block("assertnonnil")
	success := g.block("assertsuccess")
	failure := g.block("assertfailure")
	done := g.block("assertdone")
	isNil := g.interfaceIsNil(descriptor)
	g.cur.Jnz(isNil, failure, nonNil)

	g.cur = nonNil
	sourceType := g.typeAndValue(assertion.X).Type
	dynamicTag := g.interfaceDynamicType(descriptor, sourceType)
	var match ir.Ref
	if targetInterface, ok := targetType.Underlying().(*types.Interface); ok {
		match = g.interfaceTypeMatch(dynamicTag, targetInterface)
	} else {
		match = g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(targetType))
	}
	g.cur.Jnz(match, success, failure)

	g.cur = success
	successValue := descriptor
	if _, targetIsInterface := targetType.Underlying().(*types.Interface); targetIsInterface {
		successValue = g.adaptInterfaceToInterface(descriptor, sourceType, targetType)
	} else {
		payload := g.cur.Load(ir.ClsP, g.offset(descriptor, 8))
		if sliceResult {
			successValue = g.load(payload, targetType)
		} else if isAddressRepresentedInterfacePayload(targetType) || isDirectInterfaceType(targetType) {
			successValue = payload
		} else {
			successValue = g.load(payload, targetType)
		}
	}
	if sliceResult {
		g.store(successValue, resultStorage, targetType)
	}
	g.cur.Goto(done)

	g.cur = failure
	if sliceResult {
		g.store(failureValue, resultStorage, targetType)
	}
	g.cur.Goto(done)

	g.cur = done
	var value ir.Ref
	if sliceResult {
		value = g.load(resultStorage, targetType)
	} else {
		value = done.Phi(targetClass,
			ir.PhiEdge{From: success, Val: successValue},
			ir.PhiEdge{From: failure, Val: failureValue},
		)
	}
	asserted := done.Phi(ir.ClsW,
		ir.PhiEdge{From: success, Val: g.fn.Word(1)},
		ir.PhiEdge{From: failure, Val: g.fn.Word(0)},
	)
	return value, asserted
}

func (g *gen) interfaceTypeMatch(dynamicTag ir.Ref, target *types.Interface) ir.Ref {
	if target.NumMethods() == 0 {
		return g.fn.Word(1)
	}
	match := g.fn.Word(0)
	for _, implementation := range g.interfaceImplementations(target) {
		matchesImplementation := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(implementation))
		match = g.cur.Or(ir.ClsW, match, matchesImplementation)
	}
	return match
}

func (g *gen) interfaceImplementations(target *types.Interface) []types.Type {
	implementations := make(map[string]types.Type)
	add := func(valueType types.Type) {
		if !types.Implements(valueType, target) {
			return
		}
		key := types.TypeString(valueType, func(pkg *types.Package) string {
			return pkg.Path()
		})
		implementations[key] = valueType
	}
	for function := range g.functionDecls {
		signature, ok := function.Type().(*types.Signature)
		if !ok || signature.Recv() == nil {
			continue
		}
		receiverType := signature.Recv().Type()
		add(receiverType)
		if _, isPointer := receiverType.(*types.Pointer); g.runtimeAllocation && !isPointer {
			add(types.NewPointer(receiverType))
		}
	}
	keys := make([]string, 0, len(implementations))
	for key := range implementations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]types.Type, 0, len(keys))
	for _, key := range keys {
		result = append(result, implementations[key])
	}
	return result
}

func (g *gen) assignResult(lhs ast.Expr, assignmentToken token.Token, value ir.Ref, valueType types.Type) {
	if identifier, ok := lhs.(*ast.Ident); ok && identifier.Name == "_" {
		return
	}
	var slot ir.Ref
	localIdentifier := false
	if identifier, ok := lhs.(*ast.Ident); ok {
		variable := g.info.Uses[identifier]
		if assignmentToken == token.DEFINE && variable == nil {
			variable = g.info.Defs[identifier]
		}
		var exists bool
		slot, exists = g.addr(variable)
		if !exists {
			slot = g.variableStorage(variable, valueType)
		}
		_, global := g.globals[variable]
		localIdentifier = !global && !g.directValues[variable]
	} else {
		slot = g.lvalue(lhs)
	}
	value = g.coerce(value, valueType)
	if localIdentifier && isMemoryValue(valueType) {
		g.assignLocal(value, slot, valueType)
	} else if localIdentifier && isInterfaceValue(valueType) {
		g.store(value, slot, valueType)
	} else if localIdentifier && isDescriptorValue(valueType) {
		g.store(value, slot, valueType)
	} else if isInlineAggregate(valueType) || isInterfaceValue(valueType) {
		g.storeInlineValue(value, slot, valueType)
	} else {
		g.store(value, slot, valueType)
	}
}

func (g *gen) stmt(s ast.Stmt) {
	g.at(s)
	switch n := s.(type) {
	case *ast.BlockStmt:
		g.stmts(n.List)
	case *ast.DeclStmt:
		gd, ok := n.Decl.(*ast.GenDecl)
		if !ok {
			g.fail(n, "unsupported declaration")
			return
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				if _, typeDeclaration := sp.(*ast.TypeSpec); typeDeclaration {
					continue
				}
				g.fail(sp, "unsupported local declaration")
				return
			}
			for i, id := range vs.Names {
				obj := g.info.Defs[id]
				objectType := g.objectType(obj)
				_, ok := scalar(objectType)
				if !ok {
					g.fail(id, "unsupported variable type %s", objectType)
					return
				}
				slot := g.variableStorage(obj, objectType)
				if i >= len(vs.Values) && isMemoryValue(objectType) {
					continue
				}
				v := g.zeroValue(objectType)
				if i < len(vs.Values) {
					v = g.assignmentValue(vs.Values[i], objectType)
				}
				if isDescriptorValue(objectType) {
					v = g.copyInlineValue(v, objectType)
				}
				g.assignLocal(v, slot, objectType)
			}
		}
	case *ast.AssignStmt:
		if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
			if index, ok := n.Lhs[0].(*ast.IndexExpr); ok {
				if _, isMap := g.typeAndValue(index.X).Type.Underlying().(*types.Map); isMap {
					g.mapAssign(index, n.Rhs[0])
					return
				}
			}
		}
		if len(n.Rhs) == 1 && len(n.Lhs) > 1 {
			if receive, ok := n.Rhs[0].(*ast.UnaryExpr); ok && receive.Op == token.ARROW {
				g.channelReceiveAssignment(n, receive)
				return
			}
			if index, ok := n.Rhs[0].(*ast.IndexExpr); ok {
				if _, isMap := g.typeAndValue(index.X).Type.Underlying().(*types.Map); isMap {
					g.mapLookupAssignment(n, index)
					return
				}
			}
			if assertion, ok := n.Rhs[0].(*ast.TypeAssertExpr); ok {
				g.typeAssertionAssignment(n, assertion)
				return
			}
			call, ok := n.Rhs[0].(*ast.CallExpr)
			if !ok {
				g.fail(n, "assignment of multiple values from one expression is not supported")
				return
			}
			g.multiValueAssignment(n, call)
			return
		}
		vals := make([]ir.Ref, len(n.Rhs))
		for i, e := range n.Rhs {
			targetType := g.typeAndValue(n.Lhs[i]).Type
			if targetType == nil {
				if identifier, ok := n.Lhs[i].(*ast.Ident); ok {
					object := g.info.Uses[identifier]
					if object == nil {
						object = g.info.Defs[identifier]
					}
					if object != nil {
						targetType = g.objectType(object)
					}
				}
			}
			if targetType == nil {
				targetType = g.typeAndValue(e).Type
			}
			vals[i] = g.assignmentValue(e, targetType)
		}
		for i, lhs := range n.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
				continue
			}
			var slot ir.Ref
			var typ types.Type
			destinationIsInline := false
			localIdentifier := false
			if id, ok := lhs.(*ast.Ident); ok {
				obj := g.info.Uses[id]
				if n.Tok == token.DEFINE && obj == nil {
					obj = g.info.Defs[id]
				}
				typ = g.objectType(obj)
				var exists bool
				slot, exists = g.addr(obj)
				if !exists {
					slot = g.variableStorage(obj, typ)
				}
				_, global := g.globals[obj]
				destinationIsInline = global && (isMemoryValue(typ) || isDescriptorValue(typ) || isInterfaceValue(typ))
				if g.directValues[obj] && (isInlineAggregate(typ) || isInterfaceValue(typ)) {
					destinationIsInline = true
				}
				if global && isDescriptorValue(typ) && !(g.runtimeAllocation && isSliceType(typ)) {
					slot = g.cur.Load(ir.ClsP, slot)
				}
				localIdentifier = !global && !g.directValues[obj]
			} else {
				typ = g.typeAndValue(lhs).Type
				slot = g.lvalue(lhs)
				destinationIsInline = true
			}
			v := vals[i]
			if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
				old := g.load(slot, typ)
				if destinationIsInline && (isInlineAggregate(typ) || isInterfaceValue(typ)) && !(g.runtimeAllocation && isSliceType(typ)) {
					old = slot
				}
				v = g.binary(n.Tok-token.ADD_ASSIGN+token.ADD, old, v, typ, n)
			}
			v = g.coerce(v, typ)
			if localIdentifier && isDescriptorValue(typ) {
				v = g.copyInlineValue(v, typ)
			}
			if localIdentifier && isMemoryValue(typ) {
				g.assignLocal(v, slot, typ)
				continue
			}
			if destinationIsInline && (isInlineAggregate(typ) || isInterfaceValue(typ)) {
				g.storeInlineValue(v, slot, typ)
			} else {
				g.store(v, slot, typ)
			}
		}
	case *ast.IncDecStmt:
		targetType := g.typeAndValue(n.X).Type
		if index, ok := n.X.(*ast.IndexExpr); ok {
			if mapType, isMap := g.typeAndValue(index.X).Type.Underlying().(*types.Map); isMap {
				mapping := g.expr(index.X)
				key := g.assignmentValue(index.Index, mapType.Key())
				value, _ := g.mapLookupValue(mapping, key, mapType, index)
				class, _ := scalar(targetType)
				one := g.fn.ConstInt(class, 1)
				if n.Tok == token.INC {
					value = g.cur.Add(class, value, one)
				} else {
					value = g.cur.Sub(class, value, one)
				}
				g.mapAssignValue(mapping, key, g.coerce(value, targetType), mapType, index)
				return
			}
		}
		var slot ir.Ref
		if identifier, ok := n.X.(*ast.Ident); ok {
			object := g.info.Uses[identifier]
			var exists bool
			slot, exists = g.addr(object)
			if !exists {
				g.fail(identifier, "unknown variable %s", identifier.Name)
				return
			}
		} else {
			slot = g.lvalue(n.X)
		}
		c, _ := scalar(targetType)
		v := g.load(slot, targetType)
		one := g.fn.ConstInt(c, 1)
		if n.Tok == token.INC {
			v = g.cur.Add(c, v, one)
		} else {
			v = g.cur.Sub(c, v, one)
		}
		g.store(g.coerce(v, targetType), slot, targetType)
	case *ast.ExprStmt:
		g.expr(n.X)
	case *ast.ReturnStmt:
		var values []ir.Ref
		multiValueReturn := false
		if len(n.Results) != 0 {
			if len(n.Results) == 1 {
				if call, ok := n.Results[0].(*ast.CallExpr); ok {
					if _, multi := g.info.Types[call].Type.(*types.Tuple); multi {
						var resultSignature *types.Signature
						values, resultSignature = g.evaluateMultiValueCall(call)
						for index, value := range values {
							var targetType types.Type
							if index == 0 {
								targetType = g.resultType
							} else {
								targetType = g.extraResultTypes[index-1]
							}
							if _, targetIsInterface := targetType.Underlying().(*types.Interface); targetIsInterface {
								sourceType := resultSignature.Results().At(index).Type()
								values[index] = g.adaptValueToInterface(value, sourceType, targetType, ir.R, call)
							}
						}
						multiValueReturn = true
					}
				}
			}
			if !multiValueReturn {
				values = make([]ir.Ref, len(n.Results))
				for i, result := range n.Results {
					resultType := g.resultType
					if i > 0 {
						resultType = g.extraResultTypes[i-1]
					} else if resultType == nil {
						resultType = g.typeAndValue(result).Type
					}
					if identifier, ok := result.(*ast.Ident); ok && identifier.Name == "nil" && isInlineAggregate(resultType) && !(g.runtimeAllocation && isSliceType(resultType)) {
						values[i] = g.localAllocTyped(resultType)
						g.zero(values[i], resultType)
					} else {
						values[i] = g.assignmentValue(result, resultType)
					}
				}
			}
			if g.err != nil {
				return
			}
			if g.resultSlot != ir.R {
				if (isInlineAggregate(g.resultType) || isInterfaceValue(g.resultType)) && !(g.runtimeAllocation && isSliceType(g.resultType)) {
					g.storeInlineValue(values[0], g.resultSlot, g.resultType)
				} else {
					g.store(values[0], g.resultSlot, g.resultType)
				}
			}
			for i := 1; i < len(values); i++ {
				resultType := g.extraResultTypes[i-1]
				if g.runtimeAllocation && isSliceType(resultType) {
					g.store(values[i], g.extraResultSlots[i-1], resultType)
				} else if isInlineAggregate(resultType) {
					g.storeInlineValue(values[i], g.extraResultSlots[i-1], resultType)
				} else {
					g.store(g.stableReturnValue(values[i], resultType, false), g.extraResultSlots[i-1], resultType)
				}
			}
		}

		g.runDefers()
		for index, resultType := range g.extraResultTypes {
			if !isInterfaceValue(resultType) {
				continue
			}
			resultSlot := g.extraResultSlots[index]
			result := g.load(resultSlot, resultType)
			g.store(g.stableReturnValue(result, resultType, false), resultSlot, resultType)
		}
		if g.resultType == nil {
			if g.fn.HasRet {
				g.cur.Ret(g.fn.Word(0))
			} else {
				g.cur.RetVoid()
			}
			return
		}
		if !g.fn.HasRet {
			g.cur.RetVoid()
			return
		}
		var value ir.Ref
		if g.resultSlot != ir.R {
			value = g.resultSlot
			if !(isInlineAggregate(g.resultType) || isInterfaceValue(g.resultType)) || (g.runtimeAllocation && isSliceType(g.resultType)) {
				value = g.load(g.resultSlot, g.resultType)
			}
		} else if len(values) != 0 {
			value = values[0]
		} else {
			g.fail(n, "return values are missing")
			return
		}
		g.returnValue(value, g.resultType)
	case *ast.IfStmt:
		g.ifStmt(n)
	case *ast.ForStmt:
		g.forStmt(n, "")
	case *ast.RangeStmt:
		g.rangeStmt(n, "")
	case *ast.SwitchStmt:
		g.switchStmt(n, "")
	case *ast.TypeSwitchStmt:
		g.typeSwitchStmt(n, "")
	case *ast.SelectStmt:
		g.selectStmt(n, "")
	case *ast.BranchStmt:
		if n.Tok == token.BREAK {
			target := (*ir.Block)(nil)
			if n.Label != nil {
				target = g.labeledBreaks[n.Label.Name]
			} else if len(g.breaks) > 0 {
				target = g.breaks[len(g.breaks)-1]
			}
			if target == nil {
				g.fail(n, "unsupported branch %s", n.Tok)
				return
			}
			g.cur.Goto(target)
		} else if n.Tok == token.CONTINUE {
			target := (*ir.Block)(nil)
			if n.Label != nil {
				target = g.labeledContinues[n.Label.Name]
			} else if len(g.continues) > 0 {
				target = g.continues[len(g.continues)-1]
			}
			if target == nil {
				g.fail(n, "unsupported branch %s", n.Tok)
				return
			}
			g.cur.Goto(target)
		} else if n.Tok == token.GOTO && n.Label != nil {
			target := g.labels[n.Label.Name]
			if target == nil {
				g.fail(n, "unknown label %s", n.Label.Name)
				return
			}
			g.cur.Goto(target)
		} else {
			g.fail(n, "unsupported branch %s", n.Tok)
		}
	case *ast.LabeledStmt:
		target := g.labels[n.Label.Name]
		if g.live() {
			g.cur.Goto(target)
		}
		g.cur = target
		switch statement := n.Stmt.(type) {
		case *ast.ForStmt:
			g.forStmt(statement, n.Label.Name)
		case *ast.RangeStmt:
			g.rangeStmt(statement, n.Label.Name)
		case *ast.SwitchStmt:
			g.switchStmt(statement, n.Label.Name)
		case *ast.TypeSwitchStmt:
			g.typeSwitchStmt(statement, n.Label.Name)
		case *ast.SelectStmt:
			g.selectStmt(statement, n.Label.Name)
		default:
			g.stmt(statement)
		}
	case *ast.DeferStmt:
		if g.runtimeAllocation {
			functionValue := g.callClosure(n.Call, "deferwrap")
			if functionValue == ir.R {
				return
			}
			g.cur.CallVoid(g.fn.Sym("runtime.deferproc", 0), functionValue)
			g.deferActions = append(g.deferActions, n)
			return
		}
		if functionSlot := g.deferFunctions[n]; functionSlot != ir.R {
			functionValue := g.expr(n.Call.Fun)
			g.store(functionValue, functionSlot, types.Typ[types.UnsafePointer])
		}
		g.store(g.fn.Word(1), g.deferSlots[n], types.Typ[types.Bool])
		g.deferActions = append(g.deferActions, n)
	case *ast.SendStmt:
		channel := g.expr(n.Chan)
		channelType := g.typeAndValue(n.Chan).Type.Underlying().(*types.Chan)
		address := g.channelSendAddress(n.Value, channelType.Elem())
		g.cur.CallVoid(g.fn.Sym("runtime.chansend1", 0), channel, address)
	case *ast.GoStmt:
		g.goStatement(n)
	case *ast.EmptyStmt:
	default:
		g.fail(n, "unsupported statement %T", n)
	}
}

func (g *gen) returnMultiValueCall(call *ast.CallExpr) {
	var function *types.Func
	var receiver ir.Ref
	functionExpression := call.Fun
	switch instantiation := call.Fun.(type) {
	case *ast.IndexExpr:
		functionExpression = instantiation.X
	case *ast.IndexListExpr:
		functionExpression = instantiation.X
	}
	switch target := functionExpression.(type) {
	case *ast.Ident:
		function, _ = g.info.Uses[target].(*types.Func)
	case *ast.SelectorExpr:
		function, _ = g.info.Uses[target.Sel].(*types.Func)
		selection := g.info.Selections[target]
		if selection != nil && selection.Kind() == types.MethodVal {
			receiver = g.methodReceiver(target, function)
			if methodHasInterfaceReceiver(function) {
				g.interfaceMethods[function] = true
			}
		}
	}

	signature, ok := g.typeAndValue(call.Fun).Type.Underlying().(*types.Signature)
	if !ok || signature.Results().Len() < 2 {
		g.fail(call, "return call is not multi-valued")
		return
	}

	var callee ir.Ref
	var closure ir.Ref
	callSignature := signature
	if function != nil {
		callSignature = compiledFunctionSignature(function)
		calleeName := g.functionSymbol(function)
		if instanceName, instantiated := g.instantiatedFunctionSymbol(function, functionExpression); instantiated {
			calleeName = instanceName
			callSignature = signature
		}
		callee = g.fn.Sym(calleeName, 0)
	} else {
		closure = g.expr(call.Fun)
		callee = g.cur.Load(ir.ClsP, closure)
	}
	arguments := make([]ir.Ref, 0, len(call.Args)+signature.Results().Len())
	if receiver != ir.R {
		arguments = append(arguments, receiver)
	}
	arguments = append(arguments, g.callArguments(call.Args, call.Ellipsis.IsValid(), signature)...)
	arguments = g.adaptSharedGenericArguments(arguments, signature, callSignature, receiver != ir.R)
	if closure != ir.R {
		g.pinClosure(closure)
	}
	resultType := signature.Results().At(0).Type()
	if isInlineAggregate(resultType) && !g.runtimeAllocation {
		arguments = append(arguments, g.aggregateResult)
	}
	arguments = append(arguments, g.extraResultSlots...)

	resultClass, _ := scalar(resultType)
	var receiverType types.Type
	if receiver != ir.R && function != nil {
		functionSignature := compiledFunctionSignature(function)
		if functionSignature.Recv() != nil {
			receiverType = functionSignature.Recv().Type()
		}
	}
	value := g.callWithSignature(resultClass, callee, arguments, callSignature, receiverType)
	g.returnValue(value, resultType)
}

func (g *gen) returnValue(value ir.Ref, resultType types.Type) {
	if g.runtimeAllocation && isSliceType(resultType) {
		data, length, capacity := g.sliceParts(value)
		g.cur.RetAggregate(data, length, capacity)
		return
	}
	g.cur.Ret(g.stableReturnValue(value, resultType, g.fn.RetAgg != nil))
}

func (g *gen) goStatement(statement *ast.GoStmt) {
	closure := g.callClosure(statement.Call, "gowrap")
	if closure != ir.R {
		g.cur.CallVoid(g.fn.Sym("runtime.newproc", 0), closure)
	}
}

func (g *gen) callClosure(call *ast.CallExpr, wrapperPrefix string) ir.Ref {
	signature, ok := g.typeAndValue(call.Fun).Type.Underlying().(*types.Signature)
	if !ok {
		g.fail(call, "asynchronous call target is not a function")
		return ir.R
	}
	if signature.Params().Len() == 0 && len(call.Args) == 0 {
		return g.expr(call.Fun)
	}

	position := g.fset.Position(call.Pos())
	wrapperName := fmt.Sprintf("%s.%s.%d.%d", g.pkg.Path(), wrapperPrefix, position.Line, position.Column)
	identifier, _ := call.Fun.(*ast.Ident)
	function, directFunction := g.info.Uses[identifier].(*types.Func)
	builtin, builtinCall := g.info.Uses[identifier].(*types.Builtin)
	directTarget := directFunction || builtinCall
	closureFields := make([]*types.Var, 0, len(call.Args)+2)
	closureFields = append(closureFields, types.NewVar(token.NoPos, nil, "code", types.Typ[types.Uintptr]))
	functionType := g.typeAndValue(call.Fun).Type
	parameterFieldStart := 1
	if !directTarget {
		closureFields = append(closureFields, types.NewVar(token.NoPos, nil, "function", functionType))
		parameterFieldStart = 2
	}
	for index := 0; index < signature.Params().Len(); index++ {
		parameter := signature.Params().At(index)
		fieldName := fmt.Sprintf("argument%d", index)
		closureFields = append(closureFields, types.NewVar(token.NoPos, nil, fieldName, parameter.Type()))
	}
	closureType := types.NewStruct(closureFields, nil)
	closureOffsets := structOffsets(closureFields)

	wrapper := &gen{
		fn:                g.mod.NewFuncVoid(wrapperName),
		mod:               g.mod,
		runtimeAllocation: g.runtimeAllocation,
		typeTags:          g.typeTags,
		runtimeTypes:      g.runtimeTypes,
		goABITypes:        g.goABITypes,
		linkNames:         g.linkNames,
		initSymbols:       g.initSymbols,
		interfaceItabs:    g.interfaceItabs,
	}
	wrapper.cur = wrapper.fn.Entry()
	context := wrapper.closureContext()
	var callee ir.Ref
	var targetClosure ir.Ref
	if directFunction {
		callee = wrapper.fn.Sym(g.functionSymbol(function), 0)
	} else if !builtinCall {
		targetClosure = wrapper.load(wrapper.offset(context, closureOffsets[1]), functionType)
		callee = wrapper.cur.Load(ir.ClsP, targetClosure)
	}
	arguments := make([]ir.Ref, len(call.Args))
	for index := range call.Args {
		parameterType := signature.Params().At(index).Type()
		parameterAddress := wrapper.offset(context, closureOffsets[index+parameterFieldStart])
		if (isInlineAggregate(parameterType) || isInterfaceValue(parameterType)) && !(wrapper.runtimeAllocation && isSliceType(parameterType)) {
			arguments[index] = parameterAddress
		} else {
			arguments[index] = wrapper.load(parameterAddress, parameterType)
		}
	}
	if targetClosure != ir.R {
		wrapper.pinClosure(targetClosure)
	}
	if builtinCall {
		switch builtin.Name() {
		case "close":
			wrapper.cur.CallVoid(wrapper.fn.Sym("runtime.closechan", 0), arguments[0])
		case "delete":
			mapType := g.typeAndValue(call.Args[0]).Type.Underlying().(*types.Map)
			wrapper.mapDeleteValues(arguments[0], arguments[1], mapType, call)
		default:
			g.fail(call, "unsupported asynchronous builtin %s", builtin.Name())
			return ir.R
		}
	} else {
		wrapper.callDiscardingResults(callee, arguments, signature, nil)
	}
	wrapper.cur.RetVoid()

	closure := g.allocateTyped(closureType)
	g.cur.Store(g.fn.Sym(wrapperName, 0), closure)
	if !directTarget {
		functionValue := g.expr(call.Fun)
		g.store(functionValue, g.offset(closure, closureOffsets[1]), functionType)
	}
	for i, argument := range call.Args {
		parameterType := signature.Params().At(i).Type()
		value := g.assignmentValue(argument, parameterType)
		address := g.offset(closure, closureOffsets[i+parameterFieldStart])
		if isInlineAggregate(parameterType) || isInterfaceValue(parameterType) {
			g.storeInlineValue(value, address, parameterType)
		} else {
			g.store(value, address, parameterType)
		}
	}
	return closure
}

func (g *gen) callDiscardingResults(callee ir.Ref, arguments []ir.Ref, signature *types.Signature, receiverType types.Type) {
	if signature.Results().Len() == 0 {
		g.callVoidWithSignature(callee, arguments, signature, receiverType)
		return
	}

	if isInlineAggregate(signature.Results().At(0).Type()) && !g.runtimeAllocation {
		arguments = append(arguments, g.aggregateResultStorage(signature.Results().At(0).Type()))
	}
	for index := 1; index < signature.Results().Len(); index++ {
		resultType := signature.Results().At(index).Type()
		if isInlineAggregate(resultType) {
			arguments = append(arguments, g.aggregateResultStorage(resultType))
		} else {
			arguments = append(arguments, g.alloc(resultType))
		}
	}
	resultClass, ok := scalar(signature.Results().At(0).Type())
	if !ok {
		resultClass = ir.ClsP
	}
	g.callWithSignature(resultClass, callee, arguments, signature, receiverType)
}

func (g *gen) runDefers() {
	if g.runningDefers {
		return
	}
	g.runningDefers = true
	defer func() {
		g.runningDefers = false
	}()
	if g.runtimeAllocation {
		if len(g.deferActions) != 0 {
			g.cur.CallVoid(g.fn.Sym("runtime.deferreturn", 0))
		}
		return
	}

	for i := len(g.deferActions) - 1; i >= 0; i-- {
		deferStatement := g.deferActions[i]
		run := g.block("deferrun")
		done := g.block("deferdone")
		active := g.load(g.deferSlots[deferStatement], types.Typ[types.Bool])
		g.cur.Jnz(active, run, done)
		g.cur = run
		g.store(g.fn.Word(0), g.deferSlots[deferStatement], types.Typ[types.Bool])
		if functionSlot := g.deferFunctions[deferStatement]; functionSlot != ir.R {
			functionValue := g.load(functionSlot, types.Typ[types.UnsafePointer])
			callee := g.cur.Load(ir.ClsP, functionValue)
			g.pinClosure(functionValue)
			signature := g.typeAndValue(deferStatement.Call.Fun).Type.Underlying().(*types.Signature)
			g.callVoidWithSignature(callee, nil, signature, nil)
		} else {
			g.expr(deferStatement.Call)
		}
		if g.live() {
			g.cur.Goto(done)
		}
		g.cur = done
	}
}

func (g *gen) rangeStmt(statement *ast.RangeStmt, label string) {
	rangeType := g.typeAndValue(statement.X).Type
	if mapType, ok := rangeType.Underlying().(*types.Map); ok {
		g.mapRangeStmt(statement, label, mapType)
		return
	}
	if iteratorSignature, ok := rangeType.Underlying().(*types.Signature); ok {
		g.iteratorRangeStmt(statement, label, iteratorSignature)
		return
	}

	indexType := types.Typ[types.Int]
	var indexSlot ir.Ref
	if key, ok := statement.Key.(*ast.Ident); ok && key.Name != "_" {
		object := g.info.Defs[key]
		if object == nil {
			object = g.info.Uses[key]
		}
		indexSlot = g.variableStorage(object, indexType)
	} else {
		indexSlot = g.alloc(indexType)
	}
	g.store(g.fn.Long(0), indexSlot, indexType)

	var upper ir.Ref
	var rangeData ir.Ref
	var stringDescriptor ir.Ref
	stringRange := false
	if _, ok := rangeType.Underlying().(*types.Slice); ok {
		sliceValue := g.expr(statement.X)
		rangeData, upper, _ = g.sliceParts(sliceValue)
	} else if array, ok := rangeType.Underlying().(*types.Array); ok {
		rangeData = g.expr(statement.X)
		upper = g.fn.Long(array.Len())
	} else if pointer, ok := rangeType.Underlying().(*types.Pointer); ok {
		if array, ok := pointer.Elem().Underlying().(*types.Array); ok {
			rangeData = g.expr(statement.X)
			upper = g.fn.Long(array.Len())
		} else {
			upper = g.expr(statement.X)
			upper = g.convert(upper, rangeType, indexType)
		}
	} else if basic, ok := rangeType.Underlying().(*types.Basic); ok && (basic.Kind() == types.String || basic.Kind() == types.UntypedString) {
		stringRange = true
		stringDescriptor = g.expr(statement.X)
		rangeData = g.cur.Load(ir.ClsP, stringDescriptor)
		upper = g.cur.Load(ir.ClsL, g.offset(stringDescriptor, 8))
	} else {
		upper = g.expr(statement.X)
		upper = g.convert(upper, rangeType, indexType)
	}

	var valueSlot ir.Ref
	var valueType types.Type
	if value, ok := statement.Value.(*ast.Ident); ok && value.Name != "_" {
		object := g.info.Defs[value]
		if object == nil {
			object = g.info.Uses[value]
		}
		valueType = g.objectType(object)
		valueSlot = g.variableStorage(object, valueType)
	}
	var nextStringIndex ir.Ref
	if stringRange {
		nextStringIndex = g.alloc(indexType)
	}

	test := g.block("rangetest")
	body := g.block("rangebody")
	post := g.block("rangepost")
	done := g.block("rangeend")
	g.cur.Goto(test)

	g.cur = test
	index := g.load(indexSlot, indexType)
	condition := g.cur.Cmp(ir.CmpSlt, ir.ClsW, index, upper)
	g.cur.Jnz(condition, body, done)

	g.breaks = append(g.breaks, done)
	g.continues = append(g.continues, post)
	g.setLabeledControl(label, done, post)
	g.cur = body
	if stringRange {
		byteAddress := g.cur.Add(ir.ClsP, rangeData, index)
		firstByte := g.cur.LoadSub(ir.ClsW, ir.SubUB, byteAddress)
		ascii := g.block("rangeascii")
		decode := g.block("rangedecode")
		decoded := g.block("rangedecoded")
		isASCII := g.cur.Cmp(ir.CmpUlt, ir.ClsW, firstByte, g.fn.Word(0x80))
		g.cur.Jnz(isASCII, ascii, decode)

		asciiRune := firstByte
		asciiNext := ascii.Add(ir.ClsL, index, g.fn.Long(1))
		ascii.Goto(decoded)

		g.cur = decode
		decodedNextSlot := g.alloc(indexType)
		stringData := g.cur.Load(ir.ClsP, stringDescriptor)
		stringLength := g.cur.Load(ir.ClsL, g.offset(stringDescriptor, 8))
		decodedRune := g.cur.Call(
			ir.ClsW,
			g.fn.Sym("runtime.decoderune", 0),
			stringData,
			stringLength,
			index,
			decodedNextSlot,
		)
		decodedNext := g.load(decodedNextSlot, indexType)
		g.cur.Goto(decoded)

		g.cur = decoded
		runeValue := decoded.Phi(
			ir.ClsW,
			ir.PhiEdge{From: ascii, Val: asciiRune},
			ir.PhiEdge{From: decode, Val: decodedRune},
		)
		nextIndex := decoded.Phi(
			ir.ClsL,
			ir.PhiEdge{From: ascii, Val: asciiNext},
			ir.PhiEdge{From: decode, Val: decodedNext},
		)
		g.store(nextIndex, nextStringIndex, indexType)
		if valueSlot != ir.R {
			g.assignLocal(runeValue, valueSlot, valueType)
		}
	} else if valueSlot != ir.R {
		elementOffset := index
		if size := typeSize(valueType); size != 1 {
			elementOffset = g.cur.Mul(ir.ClsL, index, g.fn.Long(size))
		}
		address := g.cur.Add(ir.ClsP, rangeData, elementOffset)
		value := address
		if (!isInlineAggregate(valueType) && !isInterfaceValue(valueType)) || (g.runtimeAllocation && isSliceType(valueType)) {
			value = g.load(address, valueType)
		}
		g.assignLocal(value, valueSlot, valueType)
	}
	g.stmts(statement.Body.List)
	if g.live() {
		g.cur.Goto(post)
	}

	g.cur = post
	if stringRange {
		index = g.load(nextStringIndex, indexType)
	} else {
		index = g.load(indexSlot, indexType)
		index = g.cur.Add(ir.ClsL, index, g.fn.Long(1))
	}
	g.store(index, indexSlot, indexType)
	g.cur.Goto(test)
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.continues = g.continues[:len(g.continues)-1]
	g.clearLabeledControl(label)
	g.cur = done
}

func (g *gen) iteratorRangeStmt(statement *ast.RangeStmt, label string, iteratorSignature *types.Signature) {
	if iteratorSignature.Params().Len() != 1 || iteratorSignature.Results().Len() != 0 {
		g.fail(statement.X, "range iterator must accept one yield function")
		return
	}
	yieldSignature, ok := iteratorSignature.Params().At(0).Type().Underlying().(*types.Signature)
	if !ok || yieldSignature.Params().Len() < 1 || yieldSignature.Params().Len() > 2 || yieldSignature.Results().Len() != 1 {
		g.fail(statement.X, "unsupported range iterator yield signature")
		return
	}
	result, ok := yieldSignature.Results().At(0).Type().Underlying().(*types.Basic)
	if !ok || result.Kind() != types.Bool {
		g.fail(statement.X, "range iterator yield function must return bool")
		return
	}
	containsReturn := false
	ast.Inspect(statement.Body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if _, ok := node.(*ast.ReturnStmt); ok {
			containsReturn = true
		}
		return true
	})
	if containsReturn {
		g.fail(statement, "return from range-over-function body is not supported yet")
		return
	}

	iterator := g.expr(statement.X)
	var captures []types.Object
	seenCapture := make(map[types.Object]bool)
	addCapture := func(object types.Object) {
		if object == nil || seenCapture[object] {
			return
		}
		if _, exists := g.vars[object]; !exists {
			return
		}
		seenCapture[object] = true
		captures = append(captures, object)
	}
	ast.Inspect(statement.Body, func(node ast.Node) bool {
		if nested, ok := node.(*ast.FuncLit); ok && nested != nil {
			return false
		}
		identifier, ok := node.(*ast.Ident)
		if ok {
			addCapture(g.info.Uses[identifier])
		}
		return true
	})
	if statement.Tok == token.ASSIGN {
		if identifier, ok := statement.Key.(*ast.Ident); ok {
			addCapture(g.info.Uses[identifier])
		}
		if identifier, ok := statement.Value.(*ast.Ident); ok {
			addCapture(g.info.Uses[identifier])
		}
	}

	position := g.fset.Position(statement.Pos())
	symbol := fmt.Sprintf("%s.rangefunc.%d.%d", g.functionName, position.Line, position.Column)
	if g.functionName == "" {
		symbol = fmt.Sprintf("%s.rangefunc.%d.%d", g.pkg.Path(), position.Line, position.Column)
	}
	child := &gen{
		fset:                        g.fset,
		info:                        g.info,
		pkg:                         g.pkg,
		mod:                         g.mod,
		globals:                     g.globals,
		methodTargets:               g.methodTargets,
		emitRuntimeTables:           g.emitRuntimeTables,
		runtimeAllocation:           g.runtimeAllocation,
		typeTags:                    g.typeTags,
		runtimeTypes:                g.runtimeTypes,
		goABITypes:                  g.goABITypes,
		linkNames:                   g.linkNames,
		vars:                        make(map[types.Object]ir.Ref),
		labels:                      make(map[string]*ir.Block),
		labeledBreaks:               make(map[string]*ir.Block),
		labeledContinues:            make(map[string]*ir.Block),
		deferSlots:                  make(map[*ast.DeferStmt]ir.Ref),
		deferFunctions:              make(map[*ast.DeferStmt]ir.Ref),
		functionDecls:               g.functionDecls,
		initSymbols:                 g.initSymbols,
		noWriteBarrierFunctions:     g.noWriteBarrierFunctions,
		interfaceMethods:            g.interfaceMethods,
		interfaceItabs:              g.interfaceItabs,
		dynamicInitializers:         g.dynamicInitializers,
		dynamicInitializerGuards:    g.dynamicInitializerGuards,
		dynamicInitializerFunctions: g.dynamicInitializerFunctions,
		initializingGlobals:         make(map[types.Object]bool),
		stackAddresses:              make(map[uint32]bool),
		heapCaptures:                make(map[types.Object]ir.Ref),
		noWriteBarrier:              g.noWriteBarrier,
		directValues:                make(map[types.Object]bool),
		typeArguments:               g.typeArguments,
		functionName:                g.functionName,
		currentFunction:             g.currentFunction,
		resultType:                  types.Typ[types.Bool],
	}
	child.fn = g.mod.NewFunc(symbol, ir.ClsW)
	child.cur = child.fn.Entry()
	child.parents = astParents(statement.Body)
	child.currentBody = statement.Body
	var rangeVariables []types.Object
	if statement.Tok == token.DEFINE {
		if identifier, ok := statement.Key.(*ast.Ident); ok {
			rangeVariables = append(rangeVariables, g.info.Defs[identifier])
		}
		if identifier, ok := statement.Value.(*ast.Ident); ok {
			rangeVariables = append(rangeVariables, g.info.Defs[identifier])
		}
	}
	child.escapingCaptures = child.findEscapingCaptures(statement.Body, rangeVariables...)
	ast.Inspect(statement.Body, func(node ast.Node) bool {
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		if labelStatement, ok := node.(*ast.LabeledStmt); ok {
			child.labels[labelStatement.Label.Name] = child.block("label_" + labelStatement.Label.Name)
		}
		if deferStatement, ok := node.(*ast.DeferStmt); ok {
			slot := child.alloc(types.Typ[types.Bool])
			child.store(child.fn.Word(0), slot, types.Typ[types.Bool])
			child.deferSlots[deferStatement] = slot
			if len(deferStatement.Call.Args) == 0 {
				functionSlot := child.alloc(types.Typ[types.UnsafePointer])
				child.store(child.fn.ConstInt(ir.ClsP, 0), functionSlot, types.Typ[types.UnsafePointer])
				child.deferFunctions[deferStatement] = functionSlot
			}
			child.deferOrder = append(child.deferOrder, deferStatement)
		}
		return true
	})

	parameterValues := make([]ir.Ref, yieldSignature.Params().Len())
	for index := range parameterValues {
		parameter := yieldSignature.Params().At(index)
		class, supported := scalar(parameter.Type())
		if !supported {
			g.fail(statement, "unsupported range iterator value %s", parameter.Type())
			return
		}
		parameterValues[index] = child.functionParameter(parameter.Name(), parameter.Type(), class)
	}
	environment := child.closureContext()
	for index, capture := range captures {
		child.vars[capture] = child.cur.Load(ir.ClsP, child.offset(environment, int64(8*(index+1))))
	}
	bindVariable := func(expression ast.Expr, value ir.Ref, valueType types.Type) {
		identifier, ok := expression.(*ast.Ident)
		if !ok || identifier.Name == "_" {
			return
		}
		object := g.info.Uses[identifier]
		if statement.Tok == token.DEFINE && object == nil {
			object = g.info.Defs[identifier]
		}
		slot, exists := child.vars[object]
		if !exists {
			slot = child.allocLocal(valueType)
			child.vars[object] = slot
		}
		child.assignLocal(value, slot, valueType)
	}
	if statement.Key != nil {
		bindVariable(statement.Key, parameterValues[0], yieldSignature.Params().At(0).Type())
	}
	if statement.Value != nil && len(parameterValues) == 2 {
		bindVariable(statement.Value, parameterValues[1], yieldSignature.Params().At(1).Type())
	}

	continueBlock := child.block("rangecontinue")
	breakBlock := child.block("rangebreak")
	child.breaks = append(child.breaks, breakBlock)
	child.continues = append(child.continues, continueBlock)
	child.setLabeledControl(label, breakBlock, continueBlock)
	child.stmts(statement.Body.List)
	if child.err != nil {
		g.err = child.err
		return
	}
	if child.live() {
		child.cur.Goto(continueBlock)
	}
	child.breaks = child.breaks[:len(child.breaks)-1]
	child.continues = child.continues[:len(child.continues)-1]
	child.clearLabeledControl(label)
	child.cur = continueBlock
	child.cur.Ret(child.fn.Word(1))
	child.cur = breakBlock
	child.cur.Ret(child.fn.Word(0))
	child.terminateUnusedLabels()

	closure := g.localAlloc(8, 8*(len(captures)+1))
	g.cur.Store(g.fn.Sym(symbol, 0), closure)
	for index, capture := range captures {
		g.markStackPointerWord(closure, 8*(index+1))
		g.cur.Store(g.vars[capture], g.offset(closure, int64(8*(index+1))))
	}
	callee := g.cur.Load(ir.ClsP, iterator)
	g.pinClosure(iterator)
	g.callVoidWithSignature(callee, []ir.Ref{closure}, iteratorSignature, nil)
}

func (g *gen) mapRangeStmt(statement *ast.RangeStmt, label string, mapType *types.Map) {
	mapping := g.expr(statement.X)

	rangeVariable := func(expression ast.Expr, valueType types.Type) ir.Ref {
		identifier, ok := expression.(*ast.Ident)
		if !ok || identifier.Name == "_" {
			return ir.R
		}

		object := g.info.Defs[identifier]
		if object == nil {
			object = g.info.Uses[identifier]
		}
		if statement.Tok != token.DEFINE {
			if slot, exists := g.addr(object); exists {
				return slot
			}
		}

		return g.variableStorage(object, valueType)
	}

	keySlot := rangeVariable(statement.Key, mapType.Key())
	valueSlot := rangeVariable(statement.Value, mapType.Elem())
	if g.runtimeAllocation {
		g.runtimeMapRangeStmt(statement, label, mapping, mapType, keySlot, valueSlot)
		return
	}

	indexType := types.Typ[types.Int]
	indexSlot := g.alloc(indexType)
	g.store(g.fn.Long(0), indexSlot, indexType)

	start := g.block("maprangestart")
	test := g.block("maprangetest")
	occupied := g.block("maprangeoccupied")
	body := g.block("maprangebody")
	post := g.block("maprangepost")
	done := g.block("maprangeend")
	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, mapping, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(isNil, done, start)

	g.cur = start
	capacity := g.cur.Load(ir.ClsL, g.offset(mapping, mapCapacityOffset))
	g.cur.Goto(test)

	g.cur = test
	index := g.load(indexSlot, indexType)
	inRange := g.cur.Cmp(ir.CmpSlt, ir.ClsL, index, capacity)
	g.cur.Jnz(inRange, occupied, done)

	g.cur = occupied
	used := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	usedAddress := g.cur.Add(ir.ClsP, used, index)
	isUsed := g.cur.LoadSub(ir.ClsW, ir.SubUB, usedAddress)
	g.cur.Jnz(isUsed, body, post)

	g.breaks = append(g.breaks, done)
	g.continues = append(g.continues, post)
	g.setLabeledControl(label, done, post)
	g.cur = body
	if keySlot != ir.R {
		keyAddress := g.mapElementAddress(mapping, mapKeysOffset, index, mapType.Key())
		key := g.mapElementValue(keyAddress, mapType.Key())
		g.assignLocal(key, keySlot, mapType.Key())
	}
	if valueSlot != ir.R {
		valueAddress := g.mapElementAddress(mapping, mapValuesOffset, index, mapType.Elem())
		value := g.mapElementValue(valueAddress, mapType.Elem())
		g.assignLocal(value, valueSlot, mapType.Elem())
	}
	g.stmts(statement.Body.List)
	if g.live() {
		g.cur.Goto(post)
	}

	g.cur = post
	index = g.load(indexSlot, indexType)
	index = g.cur.Add(ir.ClsL, index, g.fn.Long(1))
	g.store(index, indexSlot, indexType)
	g.cur.Goto(test)
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.continues = g.continues[:len(g.continues)-1]
	g.clearLabeledControl(label)
	g.cur = done
}

func (g *gen) runtimeMapRangeStmt(statement *ast.RangeStmt, label string, mapping ir.Ref, mapType *types.Map, keySlot, valueSlot ir.Ref) {
	pointerType := types.Typ[types.UnsafePointer]
	iteratorFields := []*types.Var{
		types.NewVar(token.NoPos, nil, "key", pointerType),
		types.NewVar(token.NoPos, nil, "element", pointerType),
		types.NewVar(token.NoPos, nil, "mapType", pointerType),
		types.NewVar(token.NoPos, nil, "iterator", pointerType),
	}
	iteratorType := types.NewStruct(iteratorFields, nil)
	iterator := g.localAllocTyped(iteratorType)
	g.zero(iterator, iteratorType)
	g.cur.CallVoid(g.fn.Sym("runtime.mapiterinit", 0), g.typeTag(mapType), mapping, iterator)

	test := g.block("maprangetest")
	body := g.block("maprangebody")
	post := g.block("maprangepost")
	done := g.block("maprangeend")
	g.cur.Goto(test)

	g.cur = test
	keyAddress := g.cur.Load(ir.ClsP, iterator)
	hasKey := g.cur.Cmp(ir.CmpNe, ir.ClsP, keyAddress, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(hasKey, body, done)

	g.breaks = append(g.breaks, done)
	g.continues = append(g.continues, post)
	g.setLabeledControl(label, done, post)
	g.cur = body
	if keySlot != ir.R {
		key := g.mapElementValue(keyAddress, mapType.Key())
		g.assignLocal(key, keySlot, mapType.Key())
	}
	if valueSlot != ir.R {
		valueAddress := g.cur.Load(ir.ClsP, g.offset(iterator, 8))
		value := g.mapElementValue(valueAddress, mapType.Elem())
		g.assignLocal(value, valueSlot, mapType.Elem())
	}
	g.stmts(statement.Body.List)
	if g.live() {
		g.cur.Goto(post)
	}

	g.cur = post
	g.cur.CallVoid(g.fn.Sym("runtime.mapiternext", 0), iterator)
	g.cur.Goto(test)
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.continues = g.continues[:len(g.continues)-1]
	g.clearLabeledControl(label)
	g.cur = done
}

func isMemoryValue(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Array, *types.Struct:
		return true
	default:
		return false
	}
}

func isInlineAggregate(t types.Type) bool {
	t = representativeType(t)
	if isMemoryValue(t) {
		return true
	}
	switch value := t.Underlying().(type) {
	case *types.Slice:
		return true
	case *types.Basic:
		return value.Kind() == types.String
	}
	return false
}

func isInterfaceValue(t types.Type) bool {
	t = representativeType(t)
	if _, isTypeParameter := t.(*types.TypeParam); isTypeParameter {
		return false
	}
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

func isSharedTypeParameter(valueType types.Type) bool {
	_, ok := representativeType(valueType).(*types.TypeParam)
	return ok
}

func isDescriptorValue(t types.Type) bool {
	switch value := t.Underlying().(type) {
	case *types.Slice:
		return true
	case *types.Basic:
		return value.Kind() == types.String
	default:
		return false
	}
}

func isStringType(valueType types.Type) bool {
	basic, ok := representativeType(valueType).Underlying().(*types.Basic)
	return ok && basic.Kind() == types.String
}

func isSliceType(valueType types.Type) bool {
	if valueType == nil {
		return false
	}
	_, ok := representativeType(valueType).Underlying().(*types.Slice)
	return ok
}

// The aggregate forms that have not been scalarized are represented by an
// address in cg12 IR. A returned value therefore needs storage whose lifetime
// extends beyond the callee's stack frame. Slices take the scalar return path
// before this helper. registerAggregate is true when ABIInternal consumes the
// pointed-to fields into result registers before tearing down the frame.
// Indirect extra results always escape through caller-provided storage.
func (g *gen) stableReturnValue(value ir.Ref, resultType types.Type, registerAggregate bool) ir.Ref {
	if isSharedTypeParameter(resultType) {
		return value
	}
	if _, isInterface := resultType.Underlying().(*types.Interface); isInterface {
		nilValue := g.fn.ConstInt(ir.ClsP, 0)
		if registerAggregate {
			nilValue = g.localAllocTyped(resultType)
			g.zero(nilValue, resultType)
		}
		nilResult := g.block("returninterfacenil")
		concreteResult := g.block("returninterfacevalue")
		done := g.block("returninterfaceend")
		isNil := g.interfaceIsNil(value)
		g.cur.Jnz(isNil, nilResult, concreteResult)

		nilResult.Goto(done)

		g.cur = concreteResult
		stable := g.allocateTyped(resultType)
		g.storeInlineValue(value, stable, resultType)
		concreteResult = g.cur
		g.cur.Goto(done)

		g.cur = done
		return done.Phi(ir.ClsP,
			ir.PhiEdge{From: nilResult, Val: nilValue},
			ir.PhiEdge{From: concreteResult, Val: stable},
		)
	}
	if !isInlineAggregate(resultType) {
		return value
	}
	if registerAggregate {
		// ABIInternal return lowering reads the aggregate fields into result
		// registers before tearing down this frame, so local result storage is
		// sufficient and does not escape.
		return value
	}

	if g.aggregateResult != ir.R {
		g.storeInlineValue(value, g.aggregateResult, resultType)
		return g.aggregateResult
	}
	result := g.allocateTyped(resultType)
	g.storeInlineValue(value, result, resultType)
	return result
}

func (g *gen) copyInlineValue(value ir.Ref, valueType types.Type) ir.Ref {
	if g.runtimeAllocation {
		if _, ok := valueType.Underlying().(*types.Slice); ok {
			return value
		}
	}
	copy := g.localAllocTyped(valueType)
	g.storeInlineValue(value, copy, valueType)
	return copy
}

func (g *gen) zeroValue(valueType types.Type) ir.Ref {
	if _, ok := valueType.Underlying().(*types.Slice); ok {
		zero := g.fn.Long(0)
		return g.sliceDescriptor(g.fn.ConstInt(ir.ClsP, 0), zero, zero)
	}
	if basic, ok := valueType.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
		return g.stringDescriptor(g.fn.ConstInt(ir.ClsP, 0), g.fn.Long(0))
	}
	if _, ok := valueType.Underlying().(*types.Interface); ok {
		return g.fn.ConstInt(ir.ClsP, 0)
	}
	if isInlineAggregate(valueType) {
		zero := g.localAllocTyped(valueType)
		g.zero(zero, valueType)
		return zero
	}
	class, _ := scalar(valueType)
	return g.fn.ConstInt(class, 0)
}

func (g *gen) allocLocal(t types.Type) ir.Ref {
	if g.runtimeAllocation {
		if _, ok := t.Underlying().(*types.Slice); ok {
			slot := g.localAllocTyped(t)
			g.zero(slot, t)
			return slot
		}
	}
	switch t.Underlying().(type) {
	case *types.Array, *types.Struct:
		slot := g.localAlloc(8, 8)
		g.markStackPointerWord(slot, 0)
		size := typeSize(t)
		align := 8
		if size < 8 {
			align = 4
		}
		backing := g.localAlloc(align, int(size))
		visitPointerWords(t, 0, func(offset int64) {
			g.markStackPointerWord(backing, int(offset))
		})
		memset := g.fn.Sym("goc_memset", 0)
		g.cur.Call(ir.ClsP, memset, backing, g.fn.Word(0), g.fn.Long(size))
		g.cur.Store(backing, slot)
		return slot
	default:
		return g.alloc(t)
	}
}

func (g *gen) lvalue(expression ast.Expr) ir.Ref {
	switch expression := expression.(type) {
	case *ast.ParenExpr:
		return g.lvalue(expression.X)
	case *ast.Ident:
		object := g.info.Uses[expression]
		if object == nil {
			object = g.info.Defs[expression]
		}
		addr, ok := g.addr(object)
		if !ok {
			g.fail(expression, "unknown variable %s", expression.Name)
		}
		return addr
	case *ast.SelectorExpr:
		selection := g.info.Selections[expression]
		if selection == nil {
			object, ok := g.info.Uses[expression.Sel].(*types.Var)
			if !ok || object.Pkg() == nil {
				g.fail(expression, "unsupported assignment selector %s", expression.Sel.Name)
				return ir.R
			}
			return g.fn.Sym(object.Pkg().Path()+"."+object.Name(), 0)
		}
		return g.selectorAddress(g.expr(expression.X), selection)
	case *ast.IndexExpr:
		base := g.indexBase(expression.X)
		index := g.expr(expression.Index)
		size := typeSize(g.typeAndValue(expression).Type)
		if size != 1 {
			index = g.cur.Mul(ir.ClsL, index, g.fn.Long(size))
		}
		return g.cur.Add(ir.ClsP, base, index)
	case *ast.StarExpr:
		return g.expr(expression.X)
	default:
		g.fail(expression, "unsupported assignment target %T", expression)
		return ir.R
	}
}

func (g *gen) sliceLvalue(expression ast.Expr) ir.Ref {
	identifier, ok := expression.(*ast.Ident)
	if !ok {
		return g.lvalue(expression)
	}
	object := g.info.Uses[identifier]
	if object == nil {
		object = g.info.Defs[identifier]
	}
	g.initializeGlobal(object)
	address, exists := g.addr(object)
	if !exists {
		g.fail(identifier, "unknown variable %s", identifier.Name)
		return ir.R
	}
	if _, global := g.globals[object]; global && !g.runtimeAllocation {
		return g.cur.Load(ir.ClsP, address)
	}
	return address
}

func (g *gen) addr(obj types.Object) (ir.Ref, bool) {
	if a, ok := g.vars[obj]; ok {
		return a, true
	}
	if name, ok := g.globals[obj]; ok {
		return g.fn.Sym(name, 0), true
	}
	return ir.R, false
}

func (g *gen) ifStmt(n *ast.IfStmt) {
	if n.Init != nil {
		g.stmt(n.Init)
	}
	if value := g.info.Types[n.Cond].Value; value != nil && value.Kind() == constant.Bool {
		if constant.BoolVal(value) {
			g.stmts(n.Body.List)
		} else if n.Else != nil {
			g.stmt(n.Else)
		}
		return
	}
	yes, no, done := g.block("if"), g.block("else"), g.block("ifend")
	g.cur.Jnz(g.expr(n.Cond), yes, no)
	g.cur = yes
	g.stmts(n.Body.List)
	yesContinues := g.live()
	if yesContinues {
		g.cur.Goto(done)
	}
	g.cur = no
	if n.Else != nil {
		g.stmt(n.Else)
	}
	noContinues := g.live()
	if noContinues {
		g.cur.Goto(done)
	}
	g.cur = done
	if !yesContinues && !noContinues {
		done.Hlt()
	}
}
func (g *gen) forStmt(n *ast.ForStmt, label string) {
	if n.Init != nil {
		g.stmt(n.Init)
	}
	test, body, post, done := g.block("fortest"), g.block("forbody"), g.block("forpost"), g.block("forend")
	g.cur.Goto(test)
	g.cur = test
	if n.Cond == nil {
		g.cur.Goto(body)
	} else {
		g.cur.Jnz(g.expr(n.Cond), body, done)
	}
	g.breaks = append(g.breaks, done)
	g.continues = append(g.continues, post)
	g.setLabeledControl(label, done, post)
	g.cur = body
	g.stmts(n.Body.List)
	if g.live() {
		g.cur.Goto(post)
	}
	g.cur = post
	if n.Post != nil {
		g.stmt(n.Post)
	}
	if g.live() {
		g.cur.Goto(test)
	}
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.continues = g.continues[:len(g.continues)-1]
	g.clearLabeledControl(label)
	g.cur = done
	if n.Cond == nil {
		reachable := false
		for _, block := range g.fn.Blocks {
			if block == done {
				continue
			}
			for _, successor := range block.Succs() {
				if successor == done {
					reachable = true
				}
			}
		}
		if !reachable {
			done.Hlt()
		}
	}
}

func (g *gen) setLabeledControl(label string, breakTarget, continueTarget *ir.Block) {
	if label == "" {
		return
	}
	g.labeledBreaks[label] = breakTarget
	if continueTarget != nil {
		g.labeledContinues[label] = continueTarget
	}
}

func (g *gen) clearLabeledControl(label string) {
	if label == "" {
		return
	}
	delete(g.labeledBreaks, label)
	delete(g.labeledContinues, label)
}

func (g *gen) switchStmt(n *ast.SwitchStmt, label string) {
	if n.Init != nil {
		g.stmt(n.Init)
	}
	var tag ir.Ref
	if n.Tag != nil {
		tag = g.expr(n.Tag)
	}
	clauses := make([]*ast.CaseClause, len(n.Body.List))
	blocks := make([]*ir.Block, len(clauses))
	done := g.block("switchend")
	def := done
	for i, s := range n.Body.List {
		clauses[i] = s.(*ast.CaseClause)
		blocks[i] = g.block("case")
		if clauses[i].List == nil {
			def = blocks[i]
		}
	}
	for i, cl := range clauses {
		if cl.List == nil {
			continue
		}
		for _, e := range cl.List {
			next := g.block("switchtest")
			var cond ir.Ref
			if n.Tag == nil {
				cond = g.expr(e)
			} else {
				tagType := g.typeAndValue(n.Tag).Type
				caseValue := g.expr(e)
				if isInterfaceValue(tagType) {
					caseValue = g.assignmentValue(e, tagType)
				}
				cond = g.binaryRaw(token.EQL, tag, caseValue, tagType, e)
			}
			g.cur.Jnz(cond, blocks[i], next)
			g.cur = next
		}
	}
	g.cur.Goto(def)
	g.breaks = append(g.breaks, done)
	g.setLabeledControl(label, done, nil)
	for i, cl := range clauses {
		g.cur = blocks[i]
		body := cl.Body
		fall := false
		if len(body) > 0 {
			if br, ok := body[len(body)-1].(*ast.BranchStmt); ok && br.Tok == token.FALLTHROUGH {
				fall = true
				body = body[:len(body)-1]
			}
		}
		g.stmts(body)
		if g.live() {
			if fall && i+1 < len(blocks) {
				g.cur.Goto(blocks[i+1])
			} else {
				g.cur.Goto(done)
			}
		}
	}
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.clearLabeledControl(label)
	g.cur = done
	reachable := false
	for _, b := range g.fn.Blocks {
		if b != done {
			for _, succ := range b.Succs() {
				if succ == done {
					reachable = true
				}
			}
		}
	}
	if !reachable {
		done.Hlt()
	}
}

func (g *gen) typeSwitchStmt(statement *ast.TypeSwitchStmt, label string) {
	if statement.Init != nil {
		g.stmt(statement.Init)
	}
	var assertion *ast.TypeAssertExpr
	switch assignment := statement.Assign.(type) {
	case *ast.AssignStmt:
		assertion, _ = assignment.Rhs[0].(*ast.TypeAssertExpr)
	case *ast.ExprStmt:
		assertion, _ = assignment.X.(*ast.TypeAssertExpr)
	}
	if assertion == nil {
		g.fail(statement, "invalid type switch assignment")
		return
	}
	interfaceValue := g.expr(assertion.X)
	clauses := make([]*ast.CaseClause, len(statement.Body.List))
	blocks := make([]*ir.Block, len(clauses))
	done := g.block("typeswitchend")
	defaultBlock := done
	for i, item := range statement.Body.List {
		clauses[i] = item.(*ast.CaseClause)
		blocks[i] = g.block("typecase")
		if clauses[i].List == nil {
			defaultBlock = blocks[i]
		}
	}

	testBlock := g.cur
	for i, clause := range clauses {
		if clause.List == nil {
			continue
		}
		for _, caseExpression := range clause.List {
			next := g.block("typetest")
			g.cur = testBlock
			if identifier, ok := caseExpression.(*ast.Ident); ok && identifier.Name == "nil" {
				isNil := g.interfaceIsNil(interfaceValue)
				g.cur.Jnz(isNil, blocks[i], next)
			} else {
				nonNil := g.block("typenonnil")
				isNil := g.interfaceIsNil(interfaceValue)
				g.cur.Jnz(isNil, next, nonNil)
				g.cur = nonNil
				sourceType := g.typeAndValue(assertion.X).Type
				dynamicTag := g.interfaceDynamicType(interfaceValue, sourceType)
				caseType := g.typeAndValue(caseExpression).Type
				var matches ir.Ref
				if caseInterface, ok := caseType.Underlying().(*types.Interface); ok {
					matches = g.interfaceTypeMatch(dynamicTag, caseInterface)
				} else {
					matches = g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(caseType))
				}
				g.cur.Jnz(matches, blocks[i], next)
			}
			testBlock = next
		}
	}
	g.cur = testBlock
	g.cur.Goto(defaultBlock)

	g.breaks = append(g.breaks, done)
	g.setLabeledControl(label, done, nil)
	for i, clause := range clauses {
		g.cur = blocks[i]
		if implicit, ok := g.info.Implicits[clause].(*types.Var); ok {
			slot := g.variableStorage(implicit, implicit.Type())
			if clause.List == nil || len(clause.List) != 1 {
				g.assignLocal(interfaceValue, slot, implicit.Type())
			} else if identifier, nilCase := clause.List[0].(*ast.Ident); nilCase && identifier.Name == "nil" {
				g.assignLocal(g.fn.ConstInt(ir.ClsP, 0), slot, implicit.Type())
			} else if isInterfaceValue(implicit.Type()) {
				sourceType := g.typeAndValue(assertion.X).Type
				converted := g.adaptInterfaceToInterface(interfaceValue, sourceType, implicit.Type())
				g.assignLocal(converted, slot, implicit.Type())
			} else {
				data := g.cur.Load(ir.ClsP, g.offset(interfaceValue, 8))
				if isMemoryValue(implicit.Type()) {
					g.assignLocal(data, slot, implicit.Type())
				} else if isAddressRepresentedInterfacePayload(implicit.Type()) || isDirectInterfaceType(implicit.Type()) {
					g.assignLocal(data, slot, implicit.Type())
				} else {
					g.assignLocal(g.load(data, implicit.Type()), slot, implicit.Type())
				}
			}
		}
		g.stmts(clause.Body)
		if g.live() {
			g.cur.Goto(done)
		}
	}
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.clearLabeledControl(label)
	g.cur = done
	reachable := false
	for _, block := range g.fn.Blocks {
		if block == done {
			continue
		}
		for _, successor := range block.Succs() {
			if successor == done {
				reachable = true
			}
		}
	}
	if !reachable {
		done.Hlt()
	}
}

func (g *gen) selectStmt(statement *ast.SelectStmt, label string) {
	type loweredCase struct {
		clause         *ast.CommClause
		receive        *ast.AssignStmt
		receiveAddress ir.Ref
		receiveType    types.Type
	}

	clauses := make([]*ast.CommClause, len(statement.Body.List))
	var defaultClause *ast.CommClause
	nsends := 0
	nreceives := 0
	for index, item := range statement.Body.List {
		clause := item.(*ast.CommClause)
		clauses[index] = clause
		if clause.Comm == nil {
			defaultClause = clause
			continue
		}
		switch clause.Comm.(type) {
		case *ast.SendStmt:
			nsends++
		case *ast.ExprStmt, *ast.AssignStmt:
			nreceives++
		default:
			g.fail(clause.Comm, "unsupported select communication %T", clause.Comm)
			return
		}
	}
	communicationCount := nsends + nreceives
	if communicationCount == 0 {
		if defaultClause == nil {
			g.cur.CallVoid(g.fn.Sym("runtime.block", 0))
			g.cur.Hlt()
			return
		}
		done := g.block("selectend")
		g.breaks = append(g.breaks, done)
		g.setLabeledControl(label, done, nil)
		g.stmts(defaultClause.Body)
		if g.live() {
			g.cur.Goto(done)
		}
		g.breaks = g.breaks[:len(g.breaks)-1]
		g.clearLabeledControl(label)
		g.cur = done
		return
	}

	caseStorage := g.localAlloc(8, communicationCount*16)
	for index := 0; index < communicationCount; index++ {
		g.markStackPointerWord(caseStorage, index*16)
		g.markStackPointerWord(caseStorage, index*16+8)
	}
	orderStorage := g.localAlloc(4, communicationCount*4)
	orderedCases := make([]loweredCase, communicationCount)
	sendIndex := 0
	receiveCount := 0
	for _, clause := range clauses {
		if clause.Comm == nil {
			continue
		}

		var caseIndex int
		var channel ir.Ref
		var elementAddress ir.Ref
		lowered := loweredCase{clause: clause}
		switch operation := clause.Comm.(type) {
		case *ast.SendStmt:
			caseIndex = sendIndex
			sendIndex++
			channel = g.expr(operation.Chan)
			channelType := g.typeAndValue(operation.Chan).Type.Underlying().(*types.Chan)
			elementAddress = g.channelSendAddress(operation.Value, channelType.Elem())
		case *ast.ExprStmt:
			receive, ok := operation.X.(*ast.UnaryExpr)
			if !ok || receive.Op != token.ARROW {
				g.fail(operation, "invalid select receive case")
				return
			}
			receiveCount++
			caseIndex = communicationCount - receiveCount
			channel, elementAddress, lowered.receiveType = g.prepareSelectReceive(receive)
			lowered.receiveAddress = elementAddress
		case *ast.AssignStmt:
			if len(operation.Rhs) != 1 || len(operation.Lhs) < 1 || len(operation.Lhs) > 2 {
				g.fail(operation, "invalid select receive assignment")
				return
			}
			receive, ok := operation.Rhs[0].(*ast.UnaryExpr)
			if !ok || receive.Op != token.ARROW {
				g.fail(operation, "invalid select receive assignment")
				return
			}
			receiveCount++
			caseIndex = communicationCount - receiveCount
			channel, elementAddress, lowered.receiveType = g.prepareSelectReceive(receive)
			lowered.receive = operation
			lowered.receiveAddress = elementAddress
		}

		descriptor := g.offset(caseStorage, int64(caseIndex*16))
		g.cur.Store(channel, descriptor)
		g.cur.Store(elementAddress, g.offset(descriptor, 8))
		orderedCases[caseIndex] = lowered
	}

	receivedSlot := g.alloc(types.Typ[types.Bool])
	blocking := defaultClause == nil
	blockingValue := int64(0)
	if blocking {
		blockingValue = 1
	}
	chosenIndex := g.cur.Call(
		ir.ClsL,
		g.fn.Sym("runtime.selectgo", 0),
		caseStorage,
		orderStorage,
		g.fn.ConstInt(ir.ClsP, 0),
		g.fn.Long(int64(nsends)),
		g.fn.Long(int64(nreceives)),
		g.fn.Word(blockingValue),
		receivedSlot,
	)
	received := g.load(receivedSlot, types.Typ[types.Bool])
	caseBlocks := make([]*ir.Block, communicationCount)
	for index := range caseBlocks {
		caseBlocks[index] = g.block("selectcase")
	}
	done := g.block("selectend")
	g.breaks = append(g.breaks, done)
	g.setLabeledControl(label, done, nil)
	continues := false
	if defaultClause != nil {
		defaultBlock := g.block("selectdefault")
		next := g.block("selectdispatch")
		isDefault := g.cur.Cmp(ir.CmpSlt, ir.ClsL, chosenIndex, g.fn.Long(0))
		g.cur.Jnz(isDefault, defaultBlock, next)
		g.cur = defaultBlock
		g.stmts(defaultClause.Body)
		if g.live() {
			continues = true
			g.cur.Goto(done)
		}
		g.cur = next
	}
	for index := 0; index < len(caseBlocks)-1; index++ {
		next := g.block("selectdispatch")
		matches := g.cur.Cmp(ir.CmpEq, ir.ClsL, chosenIndex, g.fn.Long(int64(index)))
		g.cur.Jnz(matches, caseBlocks[index], next)
		g.cur = next
	}
	g.cur.Goto(caseBlocks[len(caseBlocks)-1])

	for index, lowered := range orderedCases {
		g.cur = caseBlocks[index]
		if lowered.receive != nil {
			value := g.channelReceiveValue(lowered.receiveAddress, lowered.receiveType)
			g.assignSelectValue(lowered.receive.Lhs[0], lowered.receive.Tok, value, lowered.receiveType)
			if len(lowered.receive.Lhs) == 2 {
				g.assignSelectValue(lowered.receive.Lhs[1], lowered.receive.Tok, received, types.Typ[types.Bool])
			}
		}
		g.stmts(lowered.clause.Body)
		if g.live() {
			continues = true
			g.cur.Goto(done)
		}
	}
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.clearLabeledControl(label)
	g.cur = done
	if !continues {
		done.Hlt()
	}
}

func (g *gen) prepareSelectReceive(receive *ast.UnaryExpr) (ir.Ref, ir.Ref, types.Type) {
	channel := g.expr(receive.X)
	channelType := g.typeAndValue(receive.X).Type.Underlying().(*types.Chan)
	elementType := channelType.Elem()
	size := typeSize(elementType)
	if size < 4 {
		size = 4
	}
	valueAddress := g.localAlloc(4, int(size))
	visitPointerWords(elementType, 0, func(offset int64) {
		g.markStackPointerWord(valueAddress, int(offset))
	})
	return channel, valueAddress, elementType
}

func (g *gen) channelSendAddress(expression ast.Expr, elementType types.Type) ir.Ref {
	value := g.assignmentValue(expression, elementType)
	if isMemoryValue(elementType) {
		return value
	}
	address := g.localAllocTyped(elementType)
	if isInlineAggregate(elementType) || isInterfaceValue(elementType) {
		g.storeInlineValue(value, address, elementType)
	} else {
		g.store(value, address, elementType)
	}
	return address
}

func (g *gen) channelReceiveValue(address ir.Ref, elementType types.Type) ir.Ref {
	if g.runtimeAllocation && isSliceType(elementType) {
		return g.load(address, elementType)
	}
	if isMemoryValue(elementType) || isInlineAggregate(elementType) || isInterfaceValue(elementType) {
		return address
	}
	return g.load(address, elementType)
}

func (g *gen) channelReceiveAssignment(statement *ast.AssignStmt, receive *ast.UnaryExpr) {
	if len(statement.Lhs) != 2 {
		g.fail(statement, "channel receive assignment requires two results")
		return
	}
	channel := g.expr(receive.X)
	channelType := g.typeAndValue(receive.X).Type.Underlying().(*types.Chan)
	elementType := channelType.Elem()
	size := typeSize(elementType)
	if size < 4 {
		size = 4
	}
	valueAddress := g.localAlloc(4, int(size))
	visitPointerWords(elementType, 0, func(offset int64) {
		g.markStackPointerWord(valueAddress, int(offset))
	})
	received := g.cur.Call(ir.ClsW, g.fn.Sym("runtime.chanrecv2", 0), channel, valueAddress)
	value := g.channelReceiveValue(valueAddress, elementType)
	g.assignResult(statement.Lhs[0], statement.Tok, value, elementType)
	g.assignResult(statement.Lhs[1], statement.Tok, received, types.Typ[types.Bool])
}

func (g *gen) assignSelectValue(destination ast.Expr, assignment token.Token, value ir.Ref, valueType types.Type) {
	if identifier, ok := destination.(*ast.Ident); ok && identifier.Name == "_" {
		return
	}

	var slot ir.Ref
	localIdentifier := false
	destinationIsInline := false
	if identifier, ok := destination.(*ast.Ident); ok {
		object := g.info.Uses[identifier]
		if assignment == token.DEFINE && object == nil {
			object = g.info.Defs[identifier]
		}
		var exists bool
		slot, exists = g.addr(object)
		if !exists {
			slot = g.variableStorage(object, valueType)
		}
		_, global := g.globals[object]
		localIdentifier = !global && !g.directValues[object]
		destinationIsInline = global && (isMemoryValue(valueType) || isDescriptorValue(valueType) || isInterfaceValue(valueType))
		if g.directValues[object] && (isInlineAggregate(valueType) || isInterfaceValue(valueType)) {
			destinationIsInline = true
		}
	} else {
		slot = g.lvalue(destination)
		destinationIsInline = true
	}

	value = g.coerce(value, valueType)
	if localIdentifier && isMemoryValue(valueType) {
		g.assignLocal(value, slot, valueType)
		return
	}
	if localIdentifier && isDescriptorValue(valueType) {
		g.store(value, slot, valueType)
		return
	}
	if destinationIsInline && (isInlineAggregate(valueType) || isInterfaceValue(valueType)) {
		g.storeInlineValue(value, slot, valueType)
		return
	}
	g.store(value, slot, valueType)
}

func (g *gen) expr(e ast.Expr) (result ir.Ref) {
	g.at(e)
	tv := g.typeAndValue(e)
	defer func() {
		result = g.markManagedValue(result, tv.Type)
	}()
	c, _ := scalar(tv.Type)
	if tv.Value != nil {
		if tv.Value.Kind() == constant.String {
			return g.stringConstant(constant.StringVal(tv.Value))
		}
		if tv.Value.Kind() == constant.Float {
			value, _ := constant.Float64Val(tv.Value)
			if c == ir.ClsS {
				return g.fn.Single(value)
			}
			return g.fn.Double(value)
		}
		return g.fn.ConstInt(c, constInt(tv.Value))
	}
	switch n := e.(type) {
	case *ast.ParenExpr:
		return g.expr(n.X)
	case *ast.Ident:
		if n.Name == "nil" {
			return g.fn.ConstInt(ir.ClsP, 0)
		}
		obj := g.info.Uses[n]
		if obj == nil {
			obj = g.info.Defs[n]
		}
		if function, ok := obj.(*types.Func); ok {
			return g.functionValue(function)
		}
		g.initializeGlobal(obj)
		slot, ok := g.addr(obj)
		if !ok {
			g.fail(n, "unknown variable %s", n.Name)
			return ir.R
		}
		_, global := g.globals[obj]
		objectType := g.objectType(obj)
		if g.runtimeAllocation && global && isSliceType(objectType) {
			return g.load(slot, objectType)
		}
		if g.directValues[obj] || (global && (isMemoryValue(objectType) || isInterfaceValue(objectType))) {
			return slot
		}
		return g.load(slot, objectType)
	case *ast.BinaryExpr:
		if n.Op == token.LAND || n.Op == token.LOR {
			return g.logical(n)
		}
		if n.Op == token.EQL || n.Op == token.NEQ {
			leftType := g.typeAndValue(n.X).Type
			rightType := g.typeAndValue(n.Y).Type
			if isInterfaceValue(leftType) {
				left := g.assignmentValue(n.X, leftType)
				right := g.assignmentValue(n.Y, leftType)
				return g.binary(n.Op, left, right, leftType, n)
			}
			if isInterfaceValue(rightType) {
				left := g.assignmentValue(n.X, rightType)
				right := g.assignmentValue(n.Y, rightType)
				return g.binary(n.Op, left, right, rightType, n)
			}
		}
		return g.binary(n.Op, g.expr(n.X), g.expr(n.Y), g.typeAndValue(n.X).Type, n)
	case *ast.UnaryExpr:
		if n.Op == token.ARROW {
			channel := g.expr(n.X)
			channelType := g.typeAndValue(n.X).Type.Underlying().(*types.Chan)
			elementType := channelType.Elem()
			size := typeSize(elementType)
			if size < 4 {
				size = 4
			}
			value := g.localAlloc(4, int(size))
			visitPointerWords(elementType, 0, func(offset int64) {
				g.markStackPointerWord(value, int(offset))
			})
			g.cur.CallVoid(g.fn.Sym("runtime.chanrecv1", 0), channel, value)
			return g.channelReceiveValue(value, elementType)
		}
		if n.Op == token.AND {
			if literal, ok := n.X.(*ast.CompositeLit); ok {
				heap := !g.nonEscapingAddress(n)
				if g.noWriteBarrier {
					heap = false
				}
				literalType := g.typeAndValue(literal).Type
				_, isMap := literalType.Underlying().(*types.Map)
				if isSliceType(literalType) || isMap {
					value := g.compositeLiteral(literal, heap)
					storage := g.localAllocTyped(literalType)
					if heap {
						storage = g.allocateTyped(literalType)
					}
					if isSliceType(literalType) {
						g.storeInlineValue(value, storage, literalType)
					} else {
						g.store(value, storage, literalType)
					}
					return storage
				}
				return g.compositeLiteral(literal, heap)
			}
			// Interface values are represented by an address to their two-word
			// runtime header. Taking the address of one must expose that header,
			// as runtime helpers such as efaceOf expect, rather than the frontend
			// slot that contains the header address.
			if _, ok := g.typeAndValue(n.X).Type.Underlying().(*types.Interface); ok {
				return g.expr(n.X)
			}
			if isSharedTypeParameter(g.typeAndValue(n.X).Type) {
				return g.expr(n.X)
			}
			if g.runtimeAllocation && isSliceType(g.typeAndValue(n.X).Type) {
				return g.sliceLvalue(n.X)
			}
			if isInlineAggregate(g.typeAndValue(n.X).Type) {
				return g.expr(n.X)
			}
			return g.lvalue(n.X)
		}
		x := g.expr(n.X)
		switch n.Op {
		case token.ADD:
			return x
		case token.SUB:
			return g.cur.Neg(c, x)
		case token.XOR:
			return g.cur.Xor(c, x, g.fn.ConstInt(c, -1))
		case token.NOT:
			return g.cur.Cmp(ir.CmpEq, ir.ClsW, x, g.fn.ConstInt(c, 0))
		}
	case *ast.StarExpr:
		pointer := g.expr(n.X)
		starType := g.typeAndValue(n).Type
		if g.runtimeAllocation && isSliceType(starType) {
			return g.load(pointer, starType)
		}
		if isInlineAggregate(starType) || isInterfaceValue(starType) {
			return pointer
		}
		return g.load(pointer, starType)
	case *ast.CompositeLit:
		return g.compositeLiteral(n, false)
	case *ast.FuncLit:
		return g.functionLiteral(n)
	case *ast.CallExpr:
		if g.typeAndValue(n.Fun).IsType() {
			if len(n.Args) != 1 {
				g.fail(n, "conversion requires one argument")
				return ir.R
			}
			if isInterfaceValue(tv.Type) {
				return g.assignmentValue(n.Args[0], tv.Type)
			}
			if _, ok := tv.Type.Underlying().(*types.Slice); ok {
				if identifier, isNil := n.Args[0].(*ast.Ident); isNil && identifier.Name == "nil" {
					return g.zeroValue(tv.Type)
				}
				if basic, ok := g.typeAndValue(n.Args[0]).Type.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
					return g.stringSlice(n.Args[0], tv.Type)
				}
			}
			if target, ok := tv.Type.Underlying().(*types.Basic); ok && target.Kind() == types.String && isSliceType(g.typeAndValue(n.Args[0]).Type) {
				return g.sliceString(n.Args[0], n)
			}
			if target, ok := tv.Type.Underlying().(*types.Basic); ok && target.Kind() == types.String {
				sourceType := g.typeAndValue(n.Args[0]).Type
				if source, ok := sourceType.Underlying().(*types.Basic); ok && source.Info()&types.IsInteger != 0 {
					return g.integerString(n.Args[0], sourceType)
				}
			}
			if basic, ok := tv.Type.Underlying().(*types.Basic); ok && basic.Kind() == types.UnsafePointer {
				if address, ok := n.Args[0].(*ast.UnaryExpr); ok && address.Op == token.AND {
					if literal, ok := address.X.(*ast.CompositeLit); ok {
						return g.compositeLiteral(literal, false)
					}
				}
			}
			x := g.expr(n.Args[0])
			return g.convert(x, g.typeAndValue(n.Args[0]).Type, tv.Type)
		}
		if identifier, ok := n.Fun.(*ast.Ident); ok {
			if builtin, ok := g.info.Uses[identifier].(*types.Builtin); ok {
				return g.builtinCall(n, builtin)
			}
		}
		if selector, ok := n.Fun.(*ast.SelectorExpr); ok {
			if builtin, ok := g.info.Uses[selector.Sel].(*types.Builtin); ok {
				return g.builtinCall(n, builtin)
			}
		}
		var obj *types.Func
		var receiver ir.Ref
		functionExpression := n.Fun
		switch instantiation := n.Fun.(type) {
		case *ast.IndexExpr:
			functionExpression = instantiation.X
		case *ast.IndexListExpr:
			functionExpression = instantiation.X
		}
		switch fun := functionExpression.(type) {
		case *ast.Ident:
			obj, _ = g.info.Uses[fun].(*types.Func)
		case *ast.SelectorExpr:
			obj, _ = g.info.Uses[fun.Sel].(*types.Func)
			if selection := g.info.Selections[fun]; selection != nil && selection.Kind() == types.MethodVal {
				receiver = g.methodReceiver(fun, obj)
				if methodHasInterfaceReceiver(obj) {
					g.interfaceMethods[obj] = true
				}
			}
		}
		var callee ir.Ref
		var sig *types.Signature
		var callSignature *types.Signature
		var closure ir.Ref
		if obj != nil {
			if obj.Pkg() != nil && obj.Pkg().Path() == "internal/abi" && obj.Name() == "EscapeNonString" && len(n.Args) == 1 {
				// EscapeNonString is an escape-analysis marker in the standard
				// compiler. It has no runtime operation; its source body panics so
				// compilers do not accidentally treat it as an ordinary function.
				return g.expr(n.Args[0])
			}
			if obj.Pkg() != nil && obj.Pkg().Path() == "maps" && obj.Name() == "clone" && len(n.Args) == 1 {
				mapType, ok := g.typeAndValue(n.Args[0]).Type.Underlying().(*types.Map)
				if !ok {
					g.fail(n, "maps.clone argument is not a map")
					return ir.R
				}
				cloned := g.cloneMap(g.expr(n.Args[0]), mapType)
				descriptor := g.localAlloc(8, 16)
				g.markStackPointerWord(descriptor, 0)
				g.markStackPointerWord(descriptor, 8)
				g.cur.Store(g.typeTag(g.typeAndValue(n.Args[0]).Type), descriptor)
				g.cur.Store(cloned, g.offset(descriptor, 8))
				return descriptor
			}
			if obj.Pkg() != nil && obj.Pkg().Path() == "runtime" && obj.Name() == "getg" && len(n.Args) == 0 {
				if runtime.GOARCH != "arm64" {
					g.fail(n, "runtime.getg intrinsic is unsupported on %s", runtime.GOARCH)
					return ir.R
				}
				const arm64GRegister = 28
				return g.cur.Load(ir.ClsP, g.fn.RegVar("g", arm64GRegister))
			}
			if obj.Pkg() != nil && obj.Pkg().Path() == "internal/runtime/sys" && len(n.Args) == 0 {
				switch obj.Name() {
				case "GetCallerPC":
					return g.cur.CallerPC()
				case "GetCallerSP":
					return g.cur.CallerSP()
				}
			}
			if obj.Pkg() != nil && obj.Pkg().Path() == "internal/abi" &&
				(obj.Name() == "FuncPCABI0" || obj.Name() == "FuncPCABIInternal") && len(n.Args) == 1 {
				if identifier, ok := n.Args[0].(*ast.Ident); ok {
					if function, ok := g.info.Uses[identifier].(*types.Func); ok {
						return g.fn.Sym(g.functionSymbol(function), 0)
					}
				}
				functionValue := g.expr(n.Args[0])
				return g.cur.Load(ir.ClsP, functionValue)
			}
			calleeName := g.functionSymbol(obj)
			sig, _ = g.typeAndValue(n.Fun).Type.Underlying().(*types.Signature)
			if sig == nil {
				sig = g.objectType(obj).(*types.Signature)
			}
			callSignature = compiledFunctionSignature(obj)
			if instanceName, instantiated := g.instantiatedFunctionSymbol(obj, functionExpression); instantiated {
				calleeName = instanceName
				callSignature = sig
			}
			callee = g.fn.Sym(calleeName, 0)
		} else {
			var ok bool
			sig, ok = g.typeAndValue(n.Fun).Type.Underlying().(*types.Signature)
			if !ok {
				g.fail(n, "call target is not a function")
				return ir.R
			}
			closure = g.expr(n.Fun)
			callee = g.cur.Load(ir.ClsP, closure)
			callSignature = sig
		}
		args := make([]ir.Ref, 0, len(n.Args)+1)
		if receiver != ir.R {
			args = append(args, receiver)
		}
		args = append(args, g.callArguments(n.Args, n.Ellipsis.IsValid(), sig)...)
		args = g.adaptSharedGenericArguments(args, sig, callSignature, receiver != ir.R)
		if closure != ir.R {
			g.pinClosure(closure)
		}
		var receiverType types.Type
		if receiver != ir.R && obj != nil {
			objectSignature := compiledFunctionSignature(obj)
			if objectSignature.Recv() != nil {
				receiverType = objectSignature.Recv().Type()
			}
		}
		if sig.Results().Len() == 0 {
			g.callVoidWithSignature(callee, args, callSignature, receiverType)
			return g.fn.Word(0)
		}
		if isInlineAggregate(sig.Results().At(0).Type()) && !g.runtimeAllocation {
			args = append(args, g.aggregateResultStorage(sig.Results().At(0).Type()))
		}
		for i := 1; i < sig.Results().Len(); i++ {
			resultType := sig.Results().At(i).Type()
			if isInlineAggregate(resultType) {
				args = append(args, g.aggregateResultStorage(resultType))
			} else {
				args = append(args, g.alloc(resultType))
			}
		}
		resultClass, _ := scalar(sig.Results().At(0).Type())
		return g.callWithSignature(resultClass, callee, args, callSignature, receiverType)
	case *ast.IndexExpr:
		if _, function := g.typeAndValue(n).Type.Underlying().(*types.Signature); function && g.typeAndValue(n.Index).IsType() {
			return g.instantiatedFunctionValue(n.X, n)
		}
		if _, isMap := g.typeAndValue(n.X).Type.Underlying().(*types.Map); isMap {
			value, _ := g.mapLookup(n)
			return value
		}
		base := g.indexBase(n.X)
		idx := g.expr(n.Index)
		element := g.typeAndValue(n).Type
		size := typeSize(element)
		if size != 1 {
			idx = g.cur.Mul(ir.ClsL, idx, g.fn.Long(size))
		}
		addr := g.cur.Add(ir.ClsP, base, idx)
		if g.runtimeAllocation && isSliceType(element) {
			return g.load(addr, element)
		}
		if isInlineAggregate(element) || isInterfaceValue(element) {
			return addr
		}
		return g.load(addr, element)
	case *ast.IndexListExpr:
		return g.instantiatedFunctionValue(n.X, n)
	case *ast.SliceExpr:
		sourceType := representativeType(g.typeAndValue(n.X).Type)
		resultType := representativeType(g.typeAndValue(n).Type)
		sourceValue := g.expr(n.X)
		base := sourceValue
		sourceLength := ir.R
		sourceCapacity := ir.R
		if _, ok := sourceType.Underlying().(*types.Slice); ok {
			base, sourceLength, sourceCapacity = g.sliceParts(sourceValue)
		} else if basic, ok := sourceType.Underlying().(*types.Basic); ok && basic.Info()&types.IsString != 0 {
			base = g.cur.Load(ir.ClsP, sourceValue)
			sourceLength = g.cur.Load(ir.ClsL, g.offset(sourceValue, 8))
			sourceCapacity = sourceLength
		}
		low := g.fn.Long(0)
		if n.Low != nil {
			low = g.expr(n.Low)
		}
		high := ir.R
		capacity := ir.R
		if n.High != nil {
			high = g.expr(n.High)
		} else if array, ok := sourceType.Underlying().(*types.Array); ok {
			high = g.fn.Long(array.Len())
		} else if pointer, ok := sourceType.Underlying().(*types.Pointer); ok {
			array, isArray := pointer.Elem().Underlying().(*types.Array)
			if !isArray {
				g.fail(n, "cannot slice pointer to %s", pointer.Elem())
				return ir.R
			}
			high = g.fn.Long(array.Len())
		} else {
			high = sourceLength
		}
		if basic, ok := resultType.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			data := g.cur.Add(ir.ClsP, base, low)
			length := g.cur.Sub(ir.ClsL, high, low)
			return g.stringDescriptor(data, length)
		}
		slice, ok := resultType.Underlying().(*types.Slice)
		if !ok {
			g.fail(n, "unsupported slice result type %s", g.typeAndValue(n).Type)
			return ir.R
		}
		element := slice.Elem()
		if n.Max != nil {
			capacity = g.cur.Sub(ir.ClsL, g.expr(n.Max), low)
		} else if array, ok := sourceType.Underlying().(*types.Array); ok {
			capacity = g.cur.Sub(ir.ClsL, g.fn.Long(array.Len()), low)
		} else if pointer, ok := sourceType.Underlying().(*types.Pointer); ok {
			array := pointer.Elem().Underlying().(*types.Array)
			capacity = g.cur.Sub(ir.ClsL, g.fn.Long(array.Len()), low)
		} else {
			capacity = g.cur.Sub(ir.ClsL, sourceCapacity, low)
		}
		size := typeSize(element)
		dataOffset := low
		if size != 1 {
			dataOffset = g.cur.Mul(ir.ClsL, low, g.fn.Long(size))
		}
		data := g.cur.Add(ir.ClsP, base, dataOffset)
		length := g.cur.Sub(ir.ClsL, high, low)
		return g.sliceDescriptor(data, length, capacity)
	case *ast.TypeAssertExpr:
		if n.Type == nil {
			g.fail(n, "type switch assertion used as a value")
			return ir.R
		}
		interfaceValue := g.expr(n.X)
		nonNil := g.block("assertnonnil")
		failure := g.block("assertfail")
		success := g.block("assertsuccess")
		isNil := g.interfaceIsNil(interfaceValue)
		g.cur.Jnz(isNil, failure, nonNil)

		g.cur = nonNil
		targetType := g.typeAndValue(n).Type
		sourceType := g.typeAndValue(n.X).Type
		dynamicTag := g.interfaceDynamicType(interfaceValue, sourceType)
		targetInterface, targetIsInterface := targetType.Underlying().(*types.Interface)
		var matches ir.Ref
		if targetIsInterface {
			matches = g.interfaceTypeMatch(dynamicTag, targetInterface)
		} else {
			matches = g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(targetType))
		}
		g.cur.Jnz(matches, success, failure)

		g.cur = failure
		g.cur.CallVoid(g.fn.Sym("abort", 0))
		g.cur.Hlt()

		g.cur = success
		if targetIsInterface {
			return g.adaptInterfaceToInterface(interfaceValue, sourceType, targetType)
		}
		data := g.cur.Load(ir.ClsP, g.offset(interfaceValue, 8))
		if g.runtimeAllocation && isSliceType(targetType) {
			return g.load(data, targetType)
		}
		if isInlineAggregate(targetType) || isDirectInterfaceType(targetType) {
			return data
		}
		return g.load(data, targetType)
	case *ast.SelectorExpr:
		selection := g.info.Selections[n]
		if selection == nil {
			if function, ok := g.info.Uses[n.Sel].(*types.Func); ok {
				return g.functionValue(function)
			}
			object, ok := g.info.Uses[n.Sel].(*types.Var)
			if !ok || object.Pkg() == nil {
				g.fail(n, "unsupported selector %s", n.Sel.Name)
				return ir.R
			}
			g.initializeGlobal(object)
			address := g.fn.Sym(object.Pkg().Path()+"."+object.Name(), 0)
			objectType := g.objectType(object)
			if isMemoryValue(objectType) || isInterfaceValue(objectType) {
				return address
			}
			return g.load(address, objectType)
		}
		if selection.Kind() != types.FieldVal {
			if selection.Kind() == types.MethodVal {
				return g.methodValue(n, selection)
			}
			if selection.Kind() == types.MethodExpr {
				if function, ok := g.info.Uses[n.Sel].(*types.Func); ok {
					return g.functionValue(function)
				}
			}
			g.fail(n, "unsupported selector %s", n.Sel.Name)
			return ir.R
		}
		addr := g.selectorAddress(g.expr(n.X), selection)
		selectionType := g.concreteType(selection.Type())
		if g.runtimeAllocation && isSliceType(selectionType) {
			return g.load(addr, selectionType)
		}
		if isInlineAggregate(selectionType) || isInterfaceValue(selectionType) {
			return addr
		}
		return g.load(addr, selectionType)
	}
	g.fail(e, "unsupported expression %T", e)
	return ir.R
}

// markManagedValue records the pointer-bearing scalar parts of a Go value as
// garbage-collector roots. ClsP alone is not sufficient: the compiler also
// uses pointer-sized temporaries for stack addresses, static symbols, and
// address arithmetic that must not be presented to the runtime as heap roots.
func (g *gen) markManagedValue(value ir.Ref, valueType types.Type) ir.Ref {
	if value == ir.R || valueType == nil {
		return value
	}
	if value.Kind == ir.RefAggregate {
		aggregate := g.fn.AggregateValue(value)
		partIndex := 0
		var markFields func([]ir.Field)
		markFields = func(fields []ir.Field) {
			for _, field := range fields {
				count := field.Count
				if count < 1 {
					count = 1
				}
				for element := 0; element < count; element++ {
					if field.Type != nil {
						markFields(field.Type.Fields)
						continue
					}
					if partIndex >= len(aggregate.Parts) {
						return
					}
					if field.Pointer {
						g.fn.MarkGCRef(aggregate.Parts[partIndex])
					}
					partIndex++
				}
			}
		}
		markFields(aggregate.Type.Fields)
		return value
	}
	class, supported := scalar(valueType)
	if supported && class == ir.ClsP {
		g.fn.MarkGCRef(value)
	}
	return value
}

func (g *gen) instantiatedFunctionValue(expression ast.Expr, instantiation ast.Expr) ir.Ref {
	switch expression := expression.(type) {
	case *ast.Ident:
		if function, ok := g.info.Uses[expression].(*types.Func); ok {
			if symbol, instantiated := g.instantiatedFunctionSymbol(function, expression); instantiated {
				return g.staticFunctionValue(symbol)
			}
			return g.functionValue(function)
		}
	case *ast.SelectorExpr:
		if function, ok := g.info.Uses[expression.Sel].(*types.Func); ok {
			if symbol, instantiated := g.instantiatedFunctionSymbol(function, expression); instantiated {
				return g.staticFunctionValue(symbol)
			}
			return g.functionValue(function)
		}
	}
	g.fail(instantiation, "unsupported generic function value")
	return ir.R
}

func (g *gen) selectorAddress(address ir.Ref, selection *types.Selection) ir.Ref {
	currentType := g.concreteType(selection.Recv())
	if pointer, ok := currentType.(*types.Pointer); ok {
		currentType = pointer.Elem()
	}
	for position, index := range selection.Index() {
		structure := currentType.Underlying().(*types.Struct)
		offsets := structOffsets(structFields(structure))
		if offset := offsets[index]; offset != 0 {
			address = g.cur.Add(ir.ClsP, address, g.fn.Long(offset))
		}
		currentType = structure.Field(index).Type()
		if pointer, ok := currentType.(*types.Pointer); ok {
			if position != len(selection.Index())-1 {
				address = g.cur.Load(ir.ClsP, address)
			}
			currentType = pointer.Elem()
		}
	}
	return address
}

func (g *gen) aggregateResultStorage(resultType types.Type) ir.Ref {
	return g.localAllocTyped(resultType)
}

func (g *gen) isUnsafeAggregatePointerConversion(expression ast.Expr, resultType types.Type) bool {
	if !isInlineAggregate(resultType) {
		return false
	}
	conversion, ok := expression.(*ast.CallExpr)
	if !ok || len(conversion.Args) != 1 || !g.info.Types[conversion.Fun].IsType() {
		return false
	}
	basic, ok := g.info.Types[conversion.Args[0]].Type.Underlying().(*types.Basic)
	return ok && basic.Kind() == types.UnsafePointer
}

func methodHasInterfaceReceiver(method *types.Func) bool {
	if method == nil {
		return false
	}
	signature, ok := method.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return false
	}
	_, isInterface := signature.Recv().Type().Underlying().(*types.Interface)
	return isInterface
}

func (g *gen) methodReceiver(selector *ast.SelectorExpr, method *types.Func) ir.Ref {
	var receiver ir.Ref
	if method != nil {
		signature := method.Type().(*types.Signature)
		if signature.Recv() != nil {
			_, wantsPointer := signature.Recv().Type().Underlying().(*types.Pointer)
			receiverType := g.info.Types[selector.X].Type
			_, hasPointer := receiverType.Underlying().(*types.Pointer)
			if wantsPointer && !hasPointer {
				if isMemoryValue(receiverType) {
					receiver = g.expr(selector.X)
				} else {
					receiver = g.lvalue(selector.X)
				}
			}
		}
	}
	if receiver == ir.R {
		receiver = g.expr(selector.X)
	}
	selection := g.info.Selections[selector]
	if selection == nil || len(selection.Index()) <= 1 {
		if method != nil {
			methodReceiverType := method.Type().(*types.Signature).Recv().Type()
			_, methodWantsPointer := methodReceiverType.Underlying().(*types.Pointer)
			expressionType := g.info.Types[selector.X].Type
			_, expressionHasPointer := expressionType.Underlying().(*types.Pointer)
			if expressionHasPointer && !methodWantsPointer && !isInlineAggregate(methodReceiverType) && !isInterfaceValue(methodReceiverType) {
				return g.load(receiver, methodReceiverType)
			}
		}
		return receiver
	}
	receiver = g.promotedMethodReceiver(receiver, selection)
	methodReceiverType := method.Type().(*types.Signature).Recv().Type()
	if _, pointer := methodReceiverType.Underlying().(*types.Pointer); pointer {
		return receiver
	}
	if isInlineAggregate(methodReceiverType) || isInterfaceValue(methodReceiverType) {
		return receiver
	}
	return g.load(receiver, methodReceiverType)
}

func (g *gen) interfaceMethodReceiver(descriptor ir.Ref, method *types.Func) ir.Ref {
	payload := g.cur.Load(ir.ClsP, g.offset(descriptor, 8))
	receiverType := method.Type().(*types.Signature).Recv().Type()
	if isDirectInterfaceType(receiverType) {
		return payload
	}
	if isInlineAggregate(receiverType) || isMemoryValue(receiverType) {
		return payload
	}
	return g.load(payload, receiverType)
}

func (g *gen) embeddedInterfaceMethodReceiver(descriptor ir.Ref, dynamicType types.Type, indexes []int) ir.Ref {
	receiver := g.cur.Load(ir.ClsP, g.offset(descriptor, 8))
	currentType := dynamicType
	if pointer, ok := currentType.(*types.Pointer); ok {
		currentType = pointer.Elem()
	}
	for _, index := range indexes[:len(indexes)-1] {
		structure := currentType.Underlying().(*types.Struct)
		offsets := structOffsets(structFields(structure))
		if offset := offsets[index]; offset != 0 {
			receiver = g.offset(receiver, offset)
		}
		currentType = structure.Field(index).Type()
		if pointer, ok := currentType.(*types.Pointer); ok {
			receiver = g.cur.Load(ir.ClsP, receiver)
			currentType = pointer.Elem()
		}
	}
	return receiver
}

func (g *gen) promotedMethodReceiver(receiver ir.Ref, selection *types.Selection) ir.Ref {
	currentType := g.concreteType(selection.Recv())
	if pointer, ok := currentType.(*types.Pointer); ok {
		currentType = pointer.Elem()
	}
	indexes := selection.Index()
	for _, index := range indexes[:len(indexes)-1] {
		structure := currentType.Underlying().(*types.Struct)
		offsets := structOffsets(structFields(structure))
		if offset := offsets[index]; offset != 0 {
			receiver = g.offset(receiver, offset)
		}
		currentType = structure.Field(index).Type()
		if pointer, ok := currentType.(*types.Pointer); ok {
			receiver = g.cur.Load(ir.ClsP, receiver)
			currentType = pointer.Elem()
		}
	}
	return receiver
}

func (g *gen) methodValue(expression *ast.SelectorExpr, selection *types.Selection) ir.Ref {
	method, ok := selection.Obj().(*types.Func)
	if !ok {
		g.fail(expression, "method value target is not a function")
		return ir.R
	}
	if methodHasInterfaceReceiver(method) {
		g.interfaceMethods[method] = true
	}
	signature := g.concreteType(selection.Type()).(*types.Signature)
	position := g.fset.Position(expression.Pos())
	wrapperName := fmt.Sprintf("%s.methodvalue.%d.%d", g.pkg.Path(), position.Line, position.Column)
	resultClass := ir.ClsW
	if signature.Results().Len() > 0 {
		resultClass, _ = scalar(signature.Results().At(0).Type())
	}
	var function *ir.Func
	if signature.Results().Len() == 0 {
		function = g.mod.NewFuncVoid(wrapperName)
	} else {
		function = g.mod.NewFunc(wrapperName, resultClass)
	}
	wrapper := &gen{
		fn:                function,
		cur:               function.Entry(),
		mod:               g.mod,
		runtimeAllocation: g.runtimeAllocation,
		typeTags:          g.typeTags,
		runtimeTypes:      g.runtimeTypes,
		goABITypes:        g.goABITypes,
		linkNames:         g.linkNames,
		initSymbols:       g.initSymbols,
		interfaceItabs:    g.interfaceItabs,
	}
	if signature.Results().Len() > 0 {
		resultType := signature.Results().At(0).Type()
		function.RetAgg = wrapper.goABIAggregate(resultType)
		function.RetValues = wrapper.runtimeAllocation && isSliceType(resultType)
	}
	context := wrapper.closureContext()
	methodSignature := method.Type().(*types.Signature)
	receiverType := methodSignature.Recv().Type()
	descriptorFields := []*types.Var{
		types.NewVar(token.NoPos, nil, "code", types.Typ[types.Uintptr]),
		types.NewVar(token.NoPos, nil, "receiver", receiverType),
	}
	descriptorType := types.NewStruct(descriptorFields, nil)
	descriptorOffsets := structOffsets(descriptorFields)
	receiverAddress := wrapper.offset(context, descriptorOffsets[1])
	receiver := receiverAddress
	if wrapper.runtimeAllocation && isSliceType(receiverType) {
		receiver = wrapper.load(receiverAddress, receiverType)
	} else if !isInlineAggregate(receiverType) && !isInterfaceValue(receiverType) {
		receiver = wrapper.load(receiverAddress, receiverType)
	}
	arguments := []ir.Ref{receiver}
	for index := 0; index < signature.Params().Len(); index++ {
		parameter := signature.Params().At(index)
		class, _ := scalar(parameter.Type())
		arguments = append(arguments, wrapper.functionParameter(parameter.Name(), parameter.Type(), class))
	}
	if signature.Results().Len() > 0 && isInlineAggregate(signature.Results().At(0).Type()) && function.RetAgg == nil {
		arguments = append(arguments, function.ParamRef("result0"))
	}
	for index := 1; index < signature.Results().Len(); index++ {
		arguments = append(arguments, function.ParamRef(fmt.Sprintf("result%d", index)))
	}
	methodSymbol := g.functionSymbol(method)
	if instantiatedSymbol, instantiated := g.instantiatedFunctionSymbol(method, expression); instantiated {
		methodSymbol = instantiatedSymbol
	}
	callee := function.Sym(methodSymbol, 0)
	if signature.Results().Len() == 0 {
		wrapper.callVoidWithSignature(callee, arguments, methodSignature, receiverType)
		wrapper.cur.RetVoid()
	} else {
		result := wrapper.callWithSignature(resultClass, callee, arguments, methodSignature, receiverType)
		wrapper.returnValue(result, signature.Results().At(0).Type())
	}
	descriptor := g.localAllocTyped(descriptorType)
	if !g.valueDoesNotEscape(expression) {
		descriptor = g.allocateTyped(descriptorType)
	}
	g.cur.Store(g.fn.Sym(wrapperName, 0), g.offset(descriptor, descriptorOffsets[0]))
	receiverValue := g.methodReceiver(expression, method)
	receiverStorage := g.offset(descriptor, descriptorOffsets[1])
	if isInlineAggregate(receiverType) || isInterfaceValue(receiverType) {
		g.storeInlineValue(receiverValue, receiverStorage, receiverType)
	} else {
		g.store(receiverValue, receiverStorage, receiverType)
	}
	return descriptor
}

func (g *gen) compositeLiteral(literal *ast.CompositeLit, heap bool) ir.Ref {
	t := g.typeAndValue(literal).Type
	if pointer, isPointer := t.Underlying().(*types.Pointer); isPointer {
		structure, isStructure := pointer.Elem().Underlying().(*types.Struct)
		if !isStructure {
			g.fail(literal, "unsupported pointer composite literal type %s", t)
			return ir.R
		}
		backing := g.allocateTyped(pointer.Elem())
		g.zero(backing, pointer.Elem())
		g.initializeStructLiteral(backing, structure, literal)
		return backing
	}
	if mapType, isMap := t.Underlying().(*types.Map); isMap {
		capacity := int64(len(literal.Elts))
		if capacity < 8 {
			capacity = 8
		}
		mapping := g.allocateMap(mapType, g.fn.Long(capacity))
		for index, expression := range literal.Elts {
			entry, ok := expression.(*ast.KeyValueExpr)
			if !ok {
				g.fail(expression, "map literal entry must have a key")
				return ir.R
			}
			key := g.assignmentValue(entry.Key, mapType.Key())
			value := g.assignmentValue(entry.Value, mapType.Elem())
			if g.runtimeAllocation {
				g.runtimeMapAssign(mapping, key, value, mapType)
				continue
			}
			slot := g.fn.Long(int64(index))
			g.storeMapElement(key, g.mapElementAddress(mapping, mapKeysOffset, slot, mapType.Key()), mapType.Key())
			g.storeMapElement(value, g.mapElementAddress(mapping, mapValuesOffset, slot, mapType.Elem()), mapType.Elem())
			used := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
			g.cur.StoreSub(ir.SubUB, g.fn.Word(1), g.cur.Add(ir.ClsP, used, slot))
		}
		g.cur.Store(g.fn.Long(int64(len(literal.Elts))), g.offset(mapping, mapLengthOffset))
		return mapping
	}
	if slice, isSlice := t.Underlying().(*types.Slice); isSlice {
		length := int64(len(literal.Elts))
		for _, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				index := constInt(g.info.Types[keyed.Key].Value)
				if index >= length {
					length = index + 1
				}
			}
		}
		elementType := slice.Elem()
		elementSize := typeSize(elementType)
		alignment := int(typeAlign(elementType))
		if alignment < 4 {
			alignment = 4
		}
		backingSize := length * elementSize
		backing := g.localAlloc(alignment, int(backingSize))
		backingType := types.NewArray(elementType, length)
		visitPointerWords(backingType, 0, func(offset int64) {
			g.markStackPointerWord(backing, int(offset))
		})
		if heap || !g.valueDoesNotEscape(literal) {
			backing = g.allocateTyped(backingType)
		}
		g.zero(backing, types.NewArray(elementType, length))
		for i, expression := range literal.Elts {
			index := int64(i)
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				index = constInt(g.info.Types[keyed.Key].Value)
				expression = keyed.Value
			}
			value := g.assignmentValue(expression, elementType)
			address := g.offset(backing, index*elementSize)
			if isInlineAggregate(elementType) || isInterfaceValue(elementType) {
				g.storeInlineValue(value, address, elementType)
			} else {
				g.store(value, address, elementType)
			}
		}
		return g.sliceDescriptor(backing, g.fn.Long(length), g.fn.Long(length))
	}
	array, isArray := t.Underlying().(*types.Array)
	structure, isStruct := t.Underlying().(*types.Struct)
	if !isArray && !isStruct {
		g.fail(literal, "unsupported composite literal type %s", t)
		return ir.R
	}
	size := typeSize(t)
	align := 8
	if size < 8 {
		align = 4
	}
	backing := g.localAlloc(align, int(size))
	visitPointerWords(t, 0, func(offset int64) {
		g.markStackPointerWord(backing, int(offset))
	})
	if heap {
		backing = g.allocateTyped(t)
	}
	memset := g.fn.Sym("goc_memset", 0)
	g.cur.Call(ir.ClsP, memset, backing, g.fn.Word(0), g.fn.Long(size))
	if isStruct {
		g.initializeStructLiteral(backing, structure, literal)
		return backing
	}

	elementType := array.Elem()
	elementSize := typeSize(elementType)
	for i, expression := range literal.Elts {
		index := int64(i)
		if keyed, ok := expression.(*ast.KeyValueExpr); ok {
			index = constInt(g.info.Types[keyed.Key].Value)
			expression = keyed.Value
		}
		address := g.offset(backing, index*elementSize)
		value := g.assignmentValue(expression, elementType)
		if isInlineAggregate(elementType) || isInterfaceValue(elementType) {
			g.storeInlineValue(value, address, elementType)
		} else {
			g.store(value, address, elementType)
		}
	}
	return backing
}

func (g *gen) initializeStructLiteral(backing ir.Ref, structure *types.Struct, literal *ast.CompositeLit) {
	offsets := structOffsets(structFields(structure))
	for index, expression := range literal.Elts {
		fieldIndex := index
		if keyed, ok := expression.(*ast.KeyValueExpr); ok {
			name := keyed.Key.(*ast.Ident).Name
			for candidate := 0; candidate < structure.NumFields(); candidate++ {
				if structure.Field(candidate).Name() == name {
					fieldIndex = candidate
					break
				}
			}
			expression = keyed.Value
		}
		fieldType := structure.Field(fieldIndex).Type()
		value := g.assignmentValue(expression, fieldType)
		fieldAddress := g.offset(backing, offsets[fieldIndex])
		if isInlineAggregate(fieldType) || isInterfaceValue(fieldType) {
			g.storeInlineValue(value, fieldAddress, fieldType)
		} else {
			g.store(value, fieldAddress, fieldType)
		}
	}
}

func (g *gen) functionLiteral(literal *ast.FuncLit) ir.Ref {
	originalSignature := g.typeAndValue(literal.Type).Type.(*types.Signature)
	signature := g.concreteType(originalSignature).(*types.Signature)
	escaping := g.functionLiteralEscapes(literal)
	parameterObjects := g.fieldListObjects(literal.Type.Params)
	resultObjects := g.fieldListObjects(literal.Type.Results)
	position := g.fset.Position(literal.Pos())
	symbol := fmt.Sprintf("%s.func.%d.%d", g.pkg.Path(), position.Line, position.Column)
	if g.functionName != "" {
		symbol = fmt.Sprintf("%s.func.%d.%d", g.functionName, position.Line, position.Column)
	}
	var captures []types.Object
	seenCapture := make(map[types.Object]bool)
	ast.Inspect(literal.Body, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		object := g.info.Uses[identifier]
		if _, exists := g.vars[object]; exists && !seenCapture[object] {
			seenCapture[object] = true
			captures = append(captures, object)
		}
		return true
	})

	child := &gen{
		fset:                        g.fset,
		info:                        g.info,
		pkg:                         g.pkg,
		mod:                         g.mod,
		globals:                     g.globals,
		methodTargets:               g.methodTargets,
		emitRuntimeTables:           g.emitRuntimeTables,
		runtimeAllocation:           g.runtimeAllocation,
		typeTags:                    g.typeTags,
		runtimeTypes:                g.runtimeTypes,
		goABITypes:                  g.goABITypes,
		vars:                        make(map[types.Object]ir.Ref),
		labels:                      make(map[string]*ir.Block),
		labeledBreaks:               make(map[string]*ir.Block),
		labeledContinues:            make(map[string]*ir.Block),
		deferSlots:                  make(map[*ast.DeferStmt]ir.Ref),
		deferFunctions:              make(map[*ast.DeferStmt]ir.Ref),
		functionDecls:               g.functionDecls,
		initSymbols:                 g.initSymbols,
		noWriteBarrierFunctions:     g.noWriteBarrierFunctions,
		interfaceMethods:            g.interfaceMethods,
		interfaceItabs:              g.interfaceItabs,
		dynamicInitializers:         g.dynamicInitializers,
		dynamicInitializerGuards:    g.dynamicInitializerGuards,
		dynamicInitializerFunctions: g.dynamicInitializerFunctions,
		initializingGlobals:         make(map[types.Object]bool),
		noWriteBarrier:              g.noWriteBarrier,
		stackAddresses:              make(map[uint32]bool),
		heapCaptures:                make(map[types.Object]ir.Ref),
		directValues:                make(map[types.Object]bool),
		typeArguments:               g.typeArguments,
		functionName:                g.functionName,
		currentFunction:             g.currentFunction,
	}
	if signature.Results().Len() == 0 {
		child.fn = g.mod.NewFuncVoid(symbol)
	} else {
		class, ok := scalar(signature.Results().At(0).Type())
		if !ok {
			g.fail(literal, "unsupported function literal result %s", signature.Results().At(0).Type())
			return ir.R
		}
		child.fn = g.mod.NewFunc(symbol, class)
	}
	var resultAggregate *ir.AggType
	if signature.Results().Len() > 0 {
		resultAggregate = child.goABIAggregate(signature.Results().At(0).Type())
		child.fn.RetAgg = resultAggregate
		child.fn.RetValues = child.runtimeAllocation && isSliceType(signature.Results().At(0).Type())
	}
	child.cur = child.fn.Entry()
	child.parents = astParents(literal.Body)
	child.currentBody = literal.Body
	predeclaredVariables := append([]types.Object(nil), parameterObjects...)
	predeclaredVariables = append(predeclaredVariables, resultObjects...)
	child.escapingCaptures = child.findEscapingCaptures(literal.Body, predeclaredVariables...)
	ast.Inspect(literal.Body, func(node ast.Node) bool {
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		if label, ok := node.(*ast.LabeledStmt); ok {
			child.labels[label.Label.Name] = child.block("label_" + label.Label.Name)
		}
		if deferStatement, ok := node.(*ast.DeferStmt); ok {
			slot := child.alloc(types.Typ[types.Bool])
			child.store(child.fn.Word(0), slot, types.Typ[types.Bool])
			child.deferSlots[deferStatement] = slot
			if len(deferStatement.Call.Args) == 0 {
				functionSlot := child.alloc(types.Typ[types.UnsafePointer])
				child.store(child.fn.ConstInt(ir.ClsP, 0), functionSlot, types.Typ[types.UnsafePointer])
				child.deferFunctions[deferStatement] = functionSlot
			}
			child.deferOrder = append(child.deferOrder, deferStatement)
		}
		return true
	})
	for i := 0; i < signature.Params().Len(); i++ {
		parameter := signature.Params().At(i)
		originalParameter := originalSignature.Params().At(i)
		class, ok := scalar(parameter.Type())
		if !ok {
			g.fail(literal, "unsupported function literal parameter %s", parameter.Type())
			return ir.R
		}
		value := child.functionParameter(parameter.Name(), parameter.Type(), class)
		var storageObject types.Object = originalParameter
		if i < len(parameterObjects) && parameterObjects[i] != nil {
			storageObject = parameterObjects[i]
		}
		slot := child.variableStorage(storageObject, parameter.Type())
		child.assignLocal(value, slot, parameter.Type())
		child.vars[originalParameter] = slot
		if i < len(parameterObjects) && parameterObjects[i] != nil {
			child.vars[parameterObjects[i]] = slot
		}
	}
	if signature.Results().Len() > 0 && isInlineAggregate(signature.Results().At(0).Type()) && resultAggregate == nil {
		child.aggregateResult = child.fn.ParamRef("result0")
	}
	if signature.Results().Len() > 0 {
		child.resultType = signature.Results().At(0).Type()
	}
	environment := child.closureContext()
	for i, capture := range captures {
		child.vars[capture] = child.cur.Load(ir.ClsP, child.offset(environment, int64(8*(i+1))))
		if escaping || g.heapCaptures[capture] != ir.R {
			child.heapCaptures[capture] = child.vars[capture]
		}
		if g.directValues[capture] {
			child.directValues[capture] = true
		}
	}
	for i := 1; i < signature.Results().Len(); i++ {
		result := signature.Results().At(i)
		originalResult := originalSignature.Results().At(i)
		pointer := child.fn.ParamRef(fmt.Sprintf("result%d", i))
		child.extraResultSlots = append(child.extraResultSlots, pointer)
		child.extraResultTypes = append(child.extraResultTypes, result.Type())
		if result.Name() != "" || len(child.deferOrder) != 0 {
			if result.Name() != "" {
				child.vars[originalResult] = pointer
				if i < len(resultObjects) && resultObjects[i] != nil {
					child.vars[resultObjects[i]] = pointer
				}
			}
			if isInlineAggregate(result.Type()) && !(child.runtimeAllocation && isSliceType(result.Type())) {
				child.zero(pointer, result.Type())
				if result.Name() != "" {
					child.directValues[originalResult] = true
				}
			} else if child.runtimeAllocation && isSliceType(result.Type()) {
				child.zero(pointer, result.Type())
			} else {
				child.store(child.zeroValue(result.Type()), pointer, result.Type())
			}
		}
	}
	if signature.Results().Len() > 0 && (signature.Results().At(0).Name() != "" || len(child.deferOrder) != 0) {
		result := signature.Results().At(0)
		originalResult := originalSignature.Results().At(0)
		child.resultType = result.Type()
		if (isInlineAggregate(result.Type()) || isInterfaceValue(result.Type())) && !(child.runtimeAllocation && isSliceType(result.Type())) {
			child.resultSlot = child.aggregateResult
			if child.resultSlot == ir.R {
				child.resultSlot = child.aggregateResultStorage(result.Type())
			}
			child.zero(child.resultSlot, result.Type())
			if result.Name() != "" {
				child.directValues[originalResult] = true
			}
		} else if child.runtimeAllocation && isSliceType(result.Type()) {
			child.resultSlot = child.allocLocal(result.Type())
		} else {
			child.resultSlot = child.alloc(result.Type())
			child.store(child.zeroValue(result.Type()), child.resultSlot, result.Type())
		}
		if result.Name() != "" {
			child.vars[originalResult] = child.resultSlot
			if len(resultObjects) > 0 && resultObjects[0] != nil {
				child.vars[resultObjects[0]] = child.resultSlot
			}
		}
	}
	child.stmts(literal.Body.List)
	if child.err != nil {
		g.err = child.err
		return ir.R
	}
	if child.live() {
		child.runDefers()
	}
	if child.live() {
		if signature.Results().Len() == 0 {
			child.cur.RetVoid()
		} else {
			g.fail(literal, "function literal is missing a return")
			return ir.R
		}
	}
	child.terminateUnusedLabels()
	if len(captures) == 0 {
		return g.staticFunctionValue(symbol)
	}
	fields := make([]*types.Var, 0, len(captures)+1)
	fields = append(fields, types.NewVar(token.NoPos, nil, "code", types.Typ[types.Uintptr]))
	for index := range captures {
		fields = append(fields, types.NewVar(token.NoPos, nil, fmt.Sprintf("capture%d", index), types.Typ[types.UnsafePointer]))
	}
	var descriptor ir.Ref
	if escaping {
		descriptor = g.allocateTyped(types.NewStruct(fields, nil))
	} else {
		descriptor = g.localAlloc(8, 8*(len(captures)+1))
		for index := range captures {
			g.markStackPointerWord(descriptor, 8*(index+1))
		}
	}
	g.cur.Store(g.fn.Sym(symbol, 0), descriptor)
	for i, capture := range captures {
		if !escaping {
			g.cur.Store(g.vars[capture], g.offset(descriptor, int64(8*(i+1))))
			continue
		}
		cell := g.heapCaptures[capture]
		if cell == ir.R {
			captureType := capture.Type()
			originalSlot := g.vars[capture]
			if basic, ok := captureType.Underlying().(*types.Basic); ok && basic.Info()&types.IsUntyped != 0 {
				captureType = types.Default(captureType)
				if captureType == nil || captureType == types.Typ[types.UntypedNil] {
					captureType = types.Typ[types.UnsafePointer]
				}
			}
			if g.runtimeAllocation && isSliceType(captureType) {
				cell = g.allocateTyped(captureType)
				source := g.load(originalSlot, captureType)
				g.storeInlineValue(source, cell, captureType)
			} else if isInterfaceValue(captureType) && g.directValues[capture] {
				cell = g.allocateTyped(captureType)
				g.storeInlineValue(originalSlot, cell, captureType)
				if g.resultSlot == originalSlot {
					g.resultSlot = cell
				}
			} else if isInlineAggregate(captureType) {
				backing := g.allocateTyped(captureType)
				source := g.load(originalSlot, captureType)
				g.storeInlineValue(source, backing, captureType)

				// Aggregate frontend variables are one level indirect: their
				// variable slot contains the address of stable backing storage.
				// Preserve that representation when the slot itself escapes into
				// a closure environment.
				slotType := types.NewPointer(captureType)
				cell = g.allocateTyped(slotType)
				g.store(backing, cell, slotType)
			} else {
				cell = g.allocateTyped(captureType)
				g.store(g.load(originalSlot, captureType), cell, captureType)
			}
			g.heapCaptures[capture] = cell
			g.vars[capture] = cell
		}
		captureAddress := g.offset(descriptor, int64(8*(i+1)))
		g.store(cell, captureAddress, fields[i+1].Type())
	}
	return descriptor
}

func (g *gen) fieldListObjects(fields *ast.FieldList) []types.Object {
	if fields == nil {
		return nil
	}
	var objects []types.Object
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			objects = append(objects, nil)
			continue
		}
		for _, name := range field.Names {
			objects = append(objects, g.info.Defs[name])
		}
	}
	return objects
}

func (g *gen) functionLiteralEscapes(literal *ast.FuncLit) bool {
	return g.functionLiteralEscapesWithin(
		literal,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
}

func (g *gen) functionLiteralEscapesWithin(
	literal *ast.FuncLit,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	parent := parents[literal]
	if call, ok := parent.(*ast.CallExpr); ok {
		if call.Fun == literal {
			if _, asynchronous := parents[call].(*ast.GoStmt); asynchronous {
				return true
			}
			return false
		}
		for argumentIndex, argument := range call.Args {
			if argument == literal {
				function := calledFunction(call.Fun, info)
				if function == nil {
					return true
				}
				return !g.parameterDoesNotEscape(function, argumentIndex, checking)
			}
		}
	}
	assignment, ok := parent.(*ast.AssignStmt)
	if !ok {
		return true
	}
	for index, value := range assignment.Rhs {
		if value != literal || index >= len(assignment.Lhs) {
			continue
		}
		identifier, ok := assignment.Lhs[index].(*ast.Ident)
		if !ok {
			return true
		}
		object := info.Defs[identifier]
		if object == nil {
			return true
		}
		escapes := false
		ast.Inspect(body, func(node ast.Node) bool {
			use, ok := node.(*ast.Ident)
			if !ok || info.Uses[use] != object {
				return true
			}
			call, ok := parents[use].(*ast.CallExpr)
			if !ok || call.Fun != use {
				escapes = true
			}
			return true
		})
		return escapes
	}
	return true
}

// findEscapingCaptures identifies variables declared by the current function
// whose addresses must remain valid after the function returns. This includes
// variables referenced by escaping function literals and variables whose
// address otherwise escapes. Their storage must be chosen before any
// control-flow block starts using them; promoting a variable only at the
// escaping expression can leave earlier uses referring to a different slot.
func signatureVariables(signature *types.Signature) []types.Object {
	variables := make([]types.Object, 0, signature.Params().Len()+signature.Results().Len()+1)
	if signature.Recv() != nil {
		variables = append(variables, signature.Recv())
	}
	for index := 0; index < signature.Params().Len(); index++ {
		variables = append(variables, signature.Params().At(index))
	}
	for index := 0; index < signature.Results().Len(); index++ {
		variables = append(variables, signature.Results().At(index))
	}
	return variables
}

func (g *gen) findEscapingCaptures(body *ast.BlockStmt, predeclared ...types.Object) map[types.Object]bool {
	locals := make(map[types.Object]bool)
	for _, object := range predeclared {
		if object != nil {
			locals[object] = true
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		if variable, ok := g.info.Defs[identifier].(*types.Var); ok {
			locals[variable] = true
		}
		return true
	})

	captures := make(map[types.Object]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		slice, isSlice := node.(*ast.SliceExpr)
		if isSlice && !g.valueDoesNotEscape(slice) {
			base := slice.X
			for {
				parenthesized, ok := base.(*ast.ParenExpr)
				if !ok {
					break
				}
				base = parenthesized.X
			}
			identifier, ok := base.(*ast.Ident)
			if ok {
				object := g.info.Uses[identifier]
				if object == nil {
					object = g.info.Defs[identifier]
				}
				if locals[object] {
					if _, isArray := g.objectType(object).Underlying().(*types.Array); isArray {
						captures[object] = true
					}
				}
			}
		}

		address, isAddress := node.(*ast.UnaryExpr)
		if isAddress && address.Op == token.AND && g.addressEscapesFunction(address) {
			identifier, ok := address.X.(*ast.Ident)
			if ok {
				object := g.info.Uses[identifier]
				if object == nil {
					object = g.info.Defs[identifier]
				}
				if locals[object] {
					captures[object] = true
				}
			}
		}

		literal, ok := node.(*ast.FuncLit)
		if !ok || !g.functionLiteralEscapes(literal) {
			return true
		}
		ast.Inspect(literal.Body, func(literalNode ast.Node) bool {
			identifier, ok := literalNode.(*ast.Ident)
			if !ok {
				return true
			}
			object := g.info.Uses[identifier]
			if locals[object] {
				captures[object] = true
			}
			return true
		})
		return true
	})
	return captures
}

func (g *gen) functionValue(function *types.Func) ir.Ref {
	return g.staticFunctionValue(g.functionSymbol(function))
}

func (g *gen) staticFunctionValue(symbol string) ir.Ref {
	return g.fn.Sym(g.staticFunctionDescriptor(symbol), 0)
}

func (g *gen) staticFunctionDescriptor(symbol string) string {
	name := fmt.Sprintf(".goc.funcval.%d", len(g.mod.Data))
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubL, Sym: symbol}},
	})
	return name
}

func (g *gen) closureRegister() int {
	if runtime.GOARCH == "arm64" {
		// Go's ARM64 ABIInternal reserves X26 for the closure context.
		return 26
	}
	return 2
}

func (g *gen) closureContext() ir.Ref {
	g.fn.HasClosureContext = true
	context := g.fn.NewTemp("closure", ir.ClsP)
	temporary := g.fn.Temp(context)
	temporary.Fixed = true
	temporary.Reg = g.closureRegister()
	return context
}

func (g *gen) pinClosure(closure ir.Ref) {
	context := g.cur.Copy(ir.ClsP, closure)
	temporary := g.fn.Temp(context)
	temporary.Fixed = true
	temporary.Reg = g.closureRegister()
	g.nextCallUsesClosure = true
}

func (g *gen) consumeClosureCall() bool {
	closureCall := g.nextCallUsesClosure
	g.nextCallUsesClosure = false
	return closureCall
}

func (g *gen) stringSlice(expression ast.Expr, targetType types.Type) ir.Ref {
	elementType := targetType.Underlying().(*types.Slice).Elem()
	element, ok := elementType.Underlying().(*types.Basic)
	if !ok {
		g.fail(expression, "unsupported string conversion to %s", targetType)
		return ir.R
	}
	if element.Kind() == types.Uint8 && len(g.initializingGlobals) > 0 {
		value := g.info.Types[expression].Value
		if value != nil && value.Kind() == constant.String {
			contents := []byte(constant.StringVal(value))
			values := make([]int64, len(contents))
			for index, character := range contents {
				values[index] = int64(character)
			}
			if len(values) == 0 {
				values = append(values, 0)
			}
			name := fmt.Sprintf(".goc.bytes.%d", len(g.mod.Data))
			g.mod.Data = append(g.mod.Data, &ir.Data{
				Name:  name,
				Align: 1,
				Items: []ir.DataItem{{Sub: ir.SubUB, Ints: values}},
			})
			length := g.fn.Long(int64(len(contents)))
			return g.sliceDescriptor(g.fn.Sym(name, 0), length, length)
		}
	}

	stringValue := g.expr(expression)
	if g.runtimeAllocation {
		bufferType := types.NewPointer(types.NewArray(types.Typ[types.Uint8], 32))
		var functionName string
		var resultType types.Type
		switch element.Kind() {
		case types.Uint8:
			functionName = "runtime.stringtoslicebyte"
			resultType = types.NewSlice(types.Typ[types.Uint8])
		case types.Int32:
			functionName = "runtime.stringtoslicerune"
			bufferType = types.NewPointer(types.NewArray(types.Typ[types.Int32], 32))
			resultType = types.NewSlice(types.Typ[types.Int32])
		default:
			g.fail(expression, "unsupported string conversion to %s", targetType)
			return ir.R
		}
		buffer := g.fn.ConstInt(ir.ClsP, 0)
		if conversion, ok := g.parents[expression].(*ast.CallExpr); ok {
			_, isRange := g.parents[conversion].(*ast.RangeStmt)
			if isRange || g.valueDoesNotEscape(conversion) {
				bufferSize := int64(32)
				if element.Kind() == types.Int32 {
					bufferSize *= typeSize(types.Typ[types.Int32])
				}
				buffer = g.localAlloc(8, int(bufferSize))
			}
		}
		signature := conversionSignature([]types.Type{bufferType, types.Typ[types.String]}, resultType)
		return g.callWithSignature(
			ir.ClsP,
			g.fn.Sym(functionName, 0),
			[]ir.Ref{buffer, stringValue},
			signature,
			nil,
		)
	}
	if element.Kind() != types.Uint8 {
		g.fail(expression, "unsupported string conversion to %s without the runtime", targetType)
		return ir.R
	}
	data := g.cur.Load(ir.ClsP, stringValue)
	length := g.cur.Load(ir.ClsL, g.offset(stringValue, 8))
	copy := g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), length, g.fn.Long(1))
	g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), copy, data, length)
	return g.sliceDescriptor(copy, length, length)
}

func (g *gen) sliceString(expression ast.Expr, conversion *ast.CallExpr) ir.Ref {
	sliceType := g.typeAndValue(expression).Type
	elementType := sliceType.Underlying().(*types.Slice).Elem()
	element, ok := elementType.Underlying().(*types.Basic)
	if !ok {
		g.fail(expression, "unsupported conversion from %s to string", sliceType)
		return ir.R
	}
	sliceValue := g.expr(expression)
	data, length, _ := g.sliceParts(sliceValue)
	if g.runtimeAllocation {
		bufferType := types.NewPointer(types.NewArray(types.Typ[types.Uint8], 32))
		var functionName string
		var parameters []types.Type
		var arguments []ir.Ref
		switch element.Kind() {
		case types.Uint8:
			if g.temporaryStringConversion(conversion) {
				functionName = "runtime.slicebytetostringtmp"
				parameters = []types.Type{types.NewPointer(types.Typ[types.Uint8]), types.Typ[types.Int]}
				arguments = []ir.Ref{data, length}
			} else {
				functionName = "runtime.slicebytetostring"
				parameters = []types.Type{bufferType, types.NewPointer(types.Typ[types.Uint8]), types.Typ[types.Int]}
				arguments = []ir.Ref{g.fn.ConstInt(ir.ClsP, 0), data, length}
			}
		case types.Int32:
			functionName = "runtime.slicerunetostring"
			parameters = []types.Type{bufferType, types.NewSlice(types.Typ[types.Int32])}
			arguments = []ir.Ref{g.fn.ConstInt(ir.ClsP, 0), sliceValue}
		default:
			g.fail(expression, "unsupported conversion from %s to string", sliceType)
			return ir.R
		}
		signature := conversionSignature(parameters, types.Typ[types.String])
		return g.callWithSignature(ir.ClsP, g.fn.Sym(functionName, 0), arguments, signature, nil)
	}
	if element.Kind() != types.Uint8 {
		g.fail(expression, "unsupported conversion from %s to string without the runtime", sliceType)
		return ir.R
	}
	copy := g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), length, g.fn.Long(1))
	g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), copy, data, length)
	return g.stringDescriptor(copy, length)
}

func (g *gen) integerString(expression ast.Expr, sourceType types.Type) ir.Ref {
	if !g.runtimeAllocation {
		g.fail(expression, "integer to string conversion requires the Go runtime")
		return ir.R
	}

	value := g.expr(expression)
	value = g.convert(value, sourceType, types.Typ[types.Int64])
	bufferType := types.NewPointer(types.NewArray(types.Typ[types.Uint8], 4))
	signature := conversionSignature(
		[]types.Type{bufferType, types.Typ[types.Int64]},
		types.Typ[types.String],
	)
	return g.callWithSignature(
		ir.ClsP,
		g.fn.Sym("runtime.intstring", 0),
		[]ir.Ref{g.fn.ConstInt(ir.ClsP, 0), value},
		signature,
		nil,
	)
}

func (g *gen) temporaryStringConversion(conversion *ast.CallExpr) bool {
	parent := g.parents[conversion]
	switch parent := parent.(type) {
	case *ast.BinaryExpr:
		switch parent.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
			return true
		default:
			return false
		}
	case *ast.CallExpr:
		argumentIndex := -1
		for index, argument := range parent.Args {
			if argument == conversion {
				argumentIndex = index
				break
			}
		}
		if argumentIndex < 0 {
			return false
		}
		if identifier, ok := parent.Fun.(*ast.Ident); ok {
			if builtin, ok := g.info.Uses[identifier].(*types.Builtin); ok {
				return builtin.Name() == "len"
			}
		}
		function := calledFunction(parent.Fun, g.info)
		return function != nil && g.parameterDoesNotEscape(function, argumentIndex, make(map[parameterKey]bool))
	default:
		return false
	}
}

func conversionSignature(parameters []types.Type, resultType types.Type) *types.Signature {
	params := make([]*types.Var, len(parameters))
	for index, parameterType := range parameters {
		params[index] = types.NewParam(token.NoPos, nil, fmt.Sprintf("arg%d", index), parameterType)
	}
	result := types.NewParam(token.NoPos, nil, "result", resultType)
	return types.NewSignatureType(
		nil,
		nil,
		nil,
		types.NewTuple(params...),
		types.NewTuple(result),
		false,
	)
}

func (g *gen) stringConstant(contents string) ir.Ref {
	bytes := []byte(contents)
	values := make([]int64, len(bytes))
	for i, value := range bytes {
		values[i] = int64(value)
	}
	name := fmt.Sprintf(".goc.string.%d", len(g.mod.Data))
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: 1,
		Items: []ir.DataItem{{Sub: ir.SubUB, Ints: values}},
	})
	descriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(descriptor, 0)
	g.cur.Store(g.fn.Sym(name, 0), descriptor)
	g.cur.Store(g.fn.Long(int64(len(bytes))), g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) indexBase(expression ast.Expr) ir.Ref {
	base := g.expr(expression)
	expressionType := representativeType(g.typeAndValue(expression).Type)
	if _, ok := expressionType.Underlying().(*types.Slice); ok {
		data, _, _ := g.sliceParts(base)
		return data
	}
	if basic, ok := expressionType.Underlying().(*types.Basic); ok && basic.Info()&types.IsString != 0 {
		return g.cur.Load(ir.ClsP, base)
	}
	return base
}

func (g *gen) sliceDescriptor(data, length, capacity ir.Ref) ir.Ref {
	if g.runtimeAllocation {
		return g.sliceValue(data, length, capacity)
	}
	descriptor := g.localAlloc(8, 24)
	g.markStackPointerWord(descriptor, 0)
	g.cur.Store(data, descriptor)
	g.cur.Store(g.descriptorLength(length), g.offset(descriptor, 8))
	g.cur.Store(g.descriptorLength(capacity), g.offset(descriptor, 16))
	return descriptor
}

func (g *gen) sliceValue(data, length, capacity ir.Ref) ir.Ref {
	aggregate := g.goABIAggregate(types.NewSlice(types.Typ[types.Uint8]))
	return g.fn.Aggregate(
		aggregate,
		data,
		g.descriptorLength(length),
		g.descriptorLength(capacity),
	)
}

func (g *gen) sliceParts(value ir.Ref) (data, length, capacity ir.Ref) {
	if value.Kind == ir.RefAggregate {
		aggregate := g.fn.AggregateValue(value)
		if len(aggregate.Parts) != 3 {
			panic("goc: slice aggregate does not have three parts")
		}
		return aggregate.Parts[0], aggregate.Parts[1], aggregate.Parts[2]
	}
	return g.cur.Load(ir.ClsP, value),
		g.cur.Load(ir.ClsL, g.offset(value, 8)),
		g.cur.Load(ir.ClsL, g.offset(value, 16))
}

func (g *gen) materializeSlice(value ir.Ref) ir.Ref {
	if value.Kind != ir.RefAggregate {
		return value
	}
	storage := g.localAllocTyped(types.NewSlice(types.Typ[types.Uint8]))
	g.store(value, storage, types.NewSlice(types.Typ[types.Uint8]))
	return storage
}

func (g *gen) descriptorLength(value ir.Ref) ir.Ref {
	if g.fn.ClassOf(value) == ir.ClsW {
		return g.cur.Extsw(ir.ClsL, value)
	}
	return value
}

func (g *gen) allocateTyped(valueType types.Type) ir.Ref {
	if g.runtimeAllocation {
		allocation := g.cur.HeapAlloc(
			g.fn.Sym("runtime.newobject", 0),
			g.runtimeType(valueType),
			int(typeSize(valueType)),
			int(typeAlign(valueType)),
		)
		visitPointerWords(valueType, 0, func(offset int64) {
			g.markStackPointerWord(allocation, int(offset))
		})
		return allocation
	}
	return g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(typeSize(valueType)))
}

func (g *gen) allocateZeroed(size ir.Ref) ir.Ref {
	if g.runtimeAllocation {
		nilType := g.fn.ConstInt(ir.ClsP, 0)
		allocation := g.cur.Call(ir.ClsP, g.fn.Sym("runtime.mallocgc", 0), size, nilType, g.fn.Word(1))
		return g.fn.MarkGCRef(allocation)
	}
	return g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), size)
}

func (g *gen) stringDescriptor(data, length ir.Ref) ir.Ref {
	descriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(descriptor, 0)
	g.cur.Store(data, descriptor)
	g.cur.Store(g.descriptorLength(length), g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) offset(base ir.Ref, offset int64) ir.Ref {
	if offset == 0 {
		return base
	}
	address := g.cur.Add(ir.ClsP, base, g.fn.Long(offset))
	if base.Kind == ir.RefTemp && g.fn.Temp(base).GCRef {
		g.fn.MarkGCRef(address)
	}
	if g.isStackAddress(base) {
		if g.stackAddresses == nil {
			g.stackAddresses = make(map[uint32]bool)
		}
		g.stackAddresses[address.ID] = true
	}
	return address
}

func (g *gen) builtinCall(call *ast.CallExpr, builtin *types.Builtin) ir.Ref {
	switch builtin.Name() {
	case "Add":
		pointer := g.expr(call.Args[0])
		offset := g.expr(call.Args[1])
		return g.cur.Add(ir.ClsP, pointer, offset)
	case "Sizeof":
		argumentType := g.typeAndValue(call.Args[0]).Type
		pointerSize := int64(types.SizesFor("gc", runtime.GOARCH).Sizeof(types.Typ[types.Uintptr]))
		if _, isTypeParameter := argumentType.(*types.TypeParam); isTypeParameter {
			return g.fn.Long(pointerSize)
		}
		return g.fn.Long(typeSize(argumentType))
	case "make":
		if mapType, ok := g.typeAndValue(call).Type.Underlying().(*types.Map); ok {
			return g.makeMap(call, mapType)
		}
		if sliceType, ok := g.typeAndValue(call).Type.Underlying().(*types.Slice); ok {
			length := g.expr(call.Args[1])
			capacity := length
			if len(call.Args) == 3 {
				capacity = g.expr(call.Args[2])
			}
			fixedCapacity, hasFixedCapacity := g.fixedSliceCapacity(call)
			var data ir.Ref
			if hasFixedCapacity && fixedCapacity > 0 && g.makeResultDoesNotEscape(call) {
				elementSize := typeSize(sliceType.Elem())
				alignment := int(typeAlign(sliceType.Elem()))
				if alignment < 4 {
					alignment = 4
				}
				data = g.localAlloc(alignment, int(fixedCapacity*elementSize))
				backingType := types.NewArray(sliceType.Elem(), fixedCapacity)
				visitPointerWords(backingType, 0, func(offset int64) {
					g.markStackPointerWord(data, int(offset))
				})
				g.cur.Call(ir.ClsP, g.fn.Sym("goc_memset", 0), data, g.fn.Word(0), g.fn.Long(fixedCapacity*elementSize))
			} else if g.runtimeAllocation && hasFixedCapacity && fixedCapacity > 0 {
				backingType := types.NewArray(sliceType.Elem(), fixedCapacity)
				data = g.cur.HeapAlloc(
					g.fn.Sym("runtime.newobject", 0),
					g.runtimeType(backingType),
					int(typeSize(backingType)),
					int(typeAlign(backingType)),
				)
				visitPointerWords(backingType, 0, func(offset int64) {
					g.markStackPointerWord(data, int(offset))
				})
			} else if g.runtimeAllocation {
				data = g.cur.Call(ir.ClsP, g.fn.Sym("runtime.makeslice", 0), g.runtimeType(sliceType.Elem()), length, capacity)
			} else {
				bytes := capacity
				if size := typeSize(sliceType.Elem()); size != 1 {
					bytes = g.cur.Mul(ir.ClsL, capacity, g.fn.Long(size))
				}
				data = g.allocateZeroed(bytes)
			}
			return g.sliceDescriptor(data, length, capacity)
		}
		if channelType, ok := g.typeAndValue(call).Type.Underlying().(*types.Chan); ok {
			capacity := g.fn.Long(0)
			if len(call.Args) == 2 {
				capacity = g.expr(call.Args[1])
			}
			return g.cur.Call(ir.ClsP, g.fn.Sym("runtime.makechan", 0), g.channelType(channelType), capacity)
		}
		g.fail(call, "unsupported make result %s", g.typeAndValue(call).Type)
		return ir.R
	case "close":
		g.cur.CallVoid(g.fn.Sym("runtime.closechan", 0), g.expr(call.Args[0]))
		return g.fn.Word(0)
	case "String":
		data := g.expr(call.Args[0])
		length := g.expr(call.Args[1])
		return g.stringDescriptor(data, length)
	case "Slice":
		data := g.expr(call.Args[0])
		length := g.expr(call.Args[1])
		return g.sliceDescriptor(data, length, length)
	case "StringData", "SliceData":
		descriptor := g.expr(call.Args[0])
		if isSliceType(g.typeAndValue(call.Args[0]).Type) {
			data, _, _ := g.sliceParts(descriptor)
			return data
		}
		return g.cur.Load(ir.ClsP, descriptor)
	case "real", "imag":
		return g.complexComponent(call, builtin.Name() == "imag")
	case "min", "max":
		result := g.expr(call.Args[0])
		resultType := g.typeAndValue(call.Args[0]).Type
		class, _ := scalar(resultType)
		for _, argument := range call.Args[1:] {
			candidate := g.expr(argument)
			comparison := ir.CmpSlt
			if class.IsFloat() {
				comparison = ir.CmpFlt
			} else if !signed(resultType) {
				comparison = ir.CmpUlt
			}
			if builtin.Name() == "max" {
				if class.IsFloat() {
					comparison = ir.CmpFgt
				} else if signed(resultType) {
					comparison = ir.CmpSgt
				} else {
					comparison = ir.CmpUgt
				}
			}
			useCandidate := g.cur.Cmp(comparison, class, candidate, result)
			result = g.selectValue(useCandidate, candidate, result, class)
		}
		return result
	case "new":
		pointer := g.typeAndValue(call).Type.(*types.Pointer)
		return g.allocateTyped(pointer.Elem())
	case "len", "cap":
		argumentType := g.typeAndValue(call.Args[0]).Type
		switch t := representativeType(argumentType).Underlying().(type) {
		case *types.Array:
			return g.fn.Long(t.Len())
		case *types.Slice:
			descriptor := g.expr(call.Args[0])
			if builtin.Name() == "cap" {
				_, _, capacity := g.sliceParts(descriptor)
				return capacity
			}
			_, length, _ := g.sliceParts(descriptor)
			return length
		case *types.Map:
			if builtin.Name() == "cap" {
				g.fail(call, "cap is not defined for maps")
				return ir.R
			}
			return g.mapLength(call.Args[0])
		case *types.Basic:
			if t.Kind() == types.String && builtin.Name() == "len" {
				descriptor := g.expr(call.Args[0])
				return g.cur.Load(ir.ClsL, g.offset(descriptor, 8))
			}
			g.fail(call, "unsupported %s operand %s", builtin.Name(), argumentType)
			return ir.R
		default:
			g.fail(call, "unsupported %s operand %s", builtin.Name(), argumentType)
			return ir.R
		}
	case "panic":
		if !g.runtimeAllocation {
			g.cur.CallVoid(g.fn.Sym("abort", 0))
			g.cur.Hlt()
			return g.fn.Word(0)
		}
		anyType := types.NewInterfaceType(nil, nil)
		anyType.Complete()
		value := g.assignmentValue(call.Args[0], anyType)
		g.cur.CallVoid(g.fn.Sym("runtime.gopanic", 0), value)
		g.cur.Hlt()
		return g.fn.Word(0)
	case "recover":
		if g.runtimeAllocation {
			return g.cur.Call(ir.ClsP, g.fn.Sym("runtime.gorecover", 0))
		}
		return g.fn.ConstInt(ir.ClsP, 0)
	case "print", "println":
		g.builtinPrint(call, builtin.Name() == "println")
		return g.fn.Word(0)
	case "copy":
		destination := g.expr(call.Args[0])
		source := g.expr(call.Args[1])
		destinationData, destinationLength, _ := g.sliceParts(destination)
		var sourceData ir.Ref
		var length ir.Ref
		if isSliceType(g.typeAndValue(call.Args[1]).Type) {
			sourceData, length, _ = g.sliceParts(source)
		} else {
			sourceData = g.cur.Load(ir.ClsP, source)
			length = g.cur.Load(ir.ClsL, g.offset(source, 8))
		}
		element := g.typeAndValue(call.Args[0]).Type.Underlying().(*types.Slice).Elem()
		if g.runtimeAllocation && !g.noWriteBarrier && len(pointerWordIndices(element)) != 0 {
			return g.cur.Call(
				ir.ClsL,
				g.fn.Sym("runtime.typedslicecopy", 0),
				g.runtimeType(element),
				destinationData,
				destinationLength,
				sourceData,
				length,
			)
		}
		useSource := g.cur.Cmp(ir.CmpSle, ir.ClsW, length, destinationLength)
		shorter := g.selectValue(useSource, length, destinationLength, ir.ClsL)
		bytes := shorter
		if size := typeSize(element); size != 1 {
			bytes = g.cur.Mul(ir.ClsL, shorter, g.fn.Long(size))
		}
		memmove := g.fn.Sym("goc_memmove", 0)
		g.cur.Call(ir.ClsP, memmove, destinationData, sourceData, bytes)
		return shorter
	case "clear":
		argumentType := g.typeAndValue(call.Args[0]).Type
		var data ir.Ref
		var size ir.Ref
		var hasPointers bool
		switch target := argumentType.Underlying().(type) {
		case *types.Array:
			data = g.expr(call.Args[0])
			size = g.fn.Long(typeSize(target))
			hasPointers = len(pointerWordIndices(target)) != 0
		case *types.Slice:
			descriptor := g.expr(call.Args[0])
			var length ir.Ref
			data, length, _ = g.sliceParts(descriptor)
			hasPointers = len(pointerWordIndices(target.Elem())) != 0
			size = length
			if elementSize := typeSize(target.Elem()); elementSize != 1 {
				size = g.cur.Mul(ir.ClsL, length, g.fn.Long(elementSize))
			}
		case *types.Map:
			g.mapClear(call.Args[0], target)
			return g.fn.Word(0)
		default:
			g.fail(call, "unsupported clear operand %s", argumentType)
			return ir.R
		}
		if g.runtimeAllocation && !g.noWriteBarrier && hasPointers {
			g.cur.CallVoid(g.fn.Sym("runtime.memclrHasPointers", 0), data, size)
			return g.fn.Word(0)
		}
		memset := g.fn.Sym("goc_memset", 0)
		g.cur.Call(ir.ClsP, memset, data, g.fn.Word(0), size)
		return g.fn.Word(0)
	case "delete":
		g.mapDelete(call.Args[0], call.Args[1])
		return g.fn.Word(0)
	case "append":
		return g.appendCall(call)
	default:
		g.fail(call, "unsupported builtin %s", builtin.Name())
		return ir.R
	}
}

func (g *gen) complexComponent(call *ast.CallExpr, imaginary bool) ir.Ref {
	argumentType, ok := g.typeAndValue(call.Args[0]).Type.Underlying().(*types.Basic)
	if !ok {
		g.fail(call, "%s operand is not a complex number", call.Fun)
		return ir.R
	}

	value := g.expr(call.Args[0])
	switch argumentType.Kind() {
	case types.Complex64:
		if imaginary {
			value = g.cur.Shr(ir.ClsL, value, g.fn.Long(32))
		}
		return g.cur.Copy(ir.ClsS, g.cur.Copy(ir.ClsW, value))
	case types.Complex128:
		if imaginary {
			value = g.offset(value, 8)
		}
		return g.cur.Load(ir.ClsD, value)
	default:
		g.fail(call, "%s operand is not a complex number", call.Fun)
		return ir.R
	}
}

func (g *gen) appendCall(call *ast.CallExpr) ir.Ref {
	sliceType := g.typeAndValue(call).Type.Underlying().(*types.Slice)
	elementType := sliceType.Elem()
	elementSize := typeSize(elementType)
	destination := g.expr(call.Args[0])
	oldData, oldLength, oldCapacity := g.sliceParts(destination)

	var sourceData ir.Ref
	var values []ir.Ref
	var added ir.Ref
	if call.Ellipsis.IsValid() {
		source := g.expr(call.Args[1])
		sourceType := g.typeAndValue(call.Args[1]).Type.Underlying()
		basicType, isBasic := sourceType.(*types.Basic)
		if isBasic && basicType.Kind() == types.String {
			sourceData = g.cur.Load(ir.ClsP, source)
			added = g.cur.Load(ir.ClsL, g.offset(source, 8))
		} else {
			sourceData, added, _ = g.sliceParts(source)
		}
	} else {
		values = make([]ir.Ref, 0, len(call.Args)-1)
		for _, argument := range call.Args[1:] {
			values = append(values, g.assignmentValue(argument, elementType))
		}
		added = g.fn.Long(int64(len(values)))
	}
	newLength := g.cur.Add(ir.ClsL, oldLength, added)

	resultDataSlot := g.localAlloc(8, 8)
	g.markStackPointerWord(resultDataSlot, 0)
	resultLengthSlot := g.localAlloc(8, 8)
	resultCapacitySlot := g.localAlloc(8, 8)
	grow := g.block("appendgrow")
	reuse := g.block("appendreuse")
	done := g.block("appenddone")
	needsGrowth := g.cur.Cmp(ir.CmpUgt, ir.ClsL, newLength, oldCapacity)
	g.cur.Jnz(needsGrowth, grow, reuse)

	g.cur = grow
	var grown ir.Ref
	if g.runtimeAllocation {
		aggregate := g.goABIAggregate(sliceType)
		parts := g.cur.CallAggregate(
			aggregate,
			[]ir.Cls{ir.ClsP, ir.ClsL, ir.ClsL},
			g.fn.Sym("runtime.growslice", 0),
			oldData,
			newLength,
			oldCapacity,
			added,
			g.runtimeType(elementType),
		)
		grown = g.fn.Aggregate(aggregate, parts...)
	} else {
		doubled := g.cur.Mul(ir.ClsL, oldCapacity, g.fn.Long(2))
		useLength := g.cur.Cmp(ir.CmpUgt, ir.ClsL, newLength, doubled)
		newCapacity := g.selectValue(useLength, newLength, doubled, ir.ClsL)
		bytes := newCapacity
		if elementSize != 1 {
			bytes = g.cur.Mul(ir.ClsL, newCapacity, g.fn.Long(elementSize))
		}
		data := g.cur.Call(ir.ClsP, g.fn.Sym("realloc", 0), oldData, bytes)
		grown = g.sliceDescriptor(data, newLength, newCapacity)
	}
	grownData, grownLength, grownCapacity := g.sliceParts(grown)
	g.cur.Store(grownData, resultDataSlot)
	g.cur.Store(grownLength, resultLengthSlot)
	g.cur.Store(grownCapacity, resultCapacitySlot)
	g.cur.Goto(done)

	g.cur = reuse
	g.cur.Store(oldData, resultDataSlot)
	g.cur.Store(newLength, resultLengthSlot)
	g.cur.Store(oldCapacity, resultCapacitySlot)
	g.cur.Goto(done)

	g.cur = done
	resultData := g.cur.Load(ir.ClsP, resultDataSlot)
	resultLength := g.cur.Load(ir.ClsL, resultLengthSlot)
	resultCapacity := g.cur.Load(ir.ClsL, resultCapacitySlot)
	byteOffset := oldLength
	if elementSize != 1 {
		byteOffset = g.cur.Mul(ir.ClsL, oldLength, g.fn.Long(elementSize))
	}
	writeAt := g.cur.Add(ir.ClsP, resultData, byteOffset)
	if sourceData != ir.R {
		if g.runtimeAllocation && !g.noWriteBarrier && len(pointerWordIndices(elementType)) != 0 {
			g.cur.Call(
				ir.ClsL,
				g.fn.Sym("runtime.typedslicecopy", 0),
				g.runtimeType(elementType),
				writeAt,
				added,
				sourceData,
				added,
			)
		} else {
			byteLength := added
			if elementSize != 1 {
				byteLength = g.cur.Mul(ir.ClsL, added, g.fn.Long(elementSize))
			}
			g.cur.Call(ir.ClsP, g.fn.Sym("goc_memmove", 0), writeAt, sourceData, byteLength)
		}
	} else {
		for index, value := range values {
			address := g.offset(writeAt, int64(index)*elementSize)
			if isInlineAggregate(elementType) || isInterfaceValue(elementType) {
				g.storeInlineValue(value, address, elementType)
			} else {
				g.store(value, address, elementType)
			}
		}
	}
	return g.sliceDescriptor(resultData, resultLength, resultCapacity)
}

func (g *gen) channelType(channel *types.Chan) ir.Ref {
	element := channel.Elem()
	elementName := fmt.Sprintf(".goc.channel.element.%d", len(g.mod.Data))
	elementBytes := make([]int64, 48)
	size := typeSize(element)
	for i := 0; i < 8; i++ {
		elementBytes[i] = (size >> (8 * i)) & 0xff
	}
	alignment := typeAlign(element)
	elementBytes[21] = alignment
	elementBytes[22] = alignment
	elementBytes[23] = int64(runtimeKind(element))
	channelName := elementName + ".channel"
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: elementName, Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: elementBytes}}},
		&ir.Data{Name: channelName, Align: 8, Items: []ir.DataItem{
			{Zero: 48},
			{Sub: ir.SubL, Sym: elementName},
			{Sub: ir.SubL, Ints: []int64{3}},
		}},
	)
	return g.fn.Sym(channelName, 0)
}

func (g *gen) runtimeType(valueType types.Type) ir.Ref {
	name := fmt.Sprintf(".goc.runtime.type.%d", len(g.mod.Data))
	maskName := name + ".gcdata"
	size := typeSize(valueType)
	mask := make([]int64, (size+63)/64)
	lastPointer := int64(0)
	markPointerWords(valueType, 0, mask, &lastPointer)
	if len(mask) == 0 {
		mask = []int64{0}
	}
	alignment := typeAlign(valueType)
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: maskName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: mask}}},
		&ir.Data{Name: name, Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Ints: []int64{size, lastPointer}},
			{Sub: ir.SubW, Ints: []int64{0}},
			{Sub: ir.SubUB, Ints: []int64{0, alignment, alignment, int64(runtimeKind(valueType))}},
			{Sub: ir.SubL, Ints: []int64{0}},
			{Sub: ir.SubL, Sym: maskName},
			{Sub: ir.SubW, Ints: []int64{0, 0}},
		}},
	)
	return g.fn.Sym(name, 0)
}

func (g *gen) markDataPointerCell(name string) {
	for index := len(g.mod.Data) - 1; index >= 0; index-- {
		if g.mod.Data[index].Name == name {
			g.mod.Data[index].PointerWords = []int{0}
			return
		}
	}
}

func (g *gen) markDataPointerWords(name string, valueType types.Type) {
	words := pointerWordIndices(valueType)
	if len(words) == 0 {
		return
	}
	for index := len(g.mod.Data) - 1; index >= 0; index-- {
		if g.mod.Data[index].Name == name {
			g.mod.Data[index].PointerWords = words
			return
		}
	}
}

func pointerWordIndices(valueType types.Type) []int {
	seen := make(map[int]bool)
	visitPointerWords(valueType, 0, func(offset int64) {
		seen[int(offset/8)] = true
	})
	words := make([]int, 0, len(seen))
	for word := range seen {
		words = append(words, word)
	}
	sort.Ints(words)
	return words
}

func markPointerWords(valueType types.Type, base int64, mask []int64, lastPointer *int64) {
	visitPointerWords(valueType, base, func(offset int64) {
		word := offset / 8
		mask[word/8] |= 1 << (word % 8)
		if end := offset + 8; end > *lastPointer {
			*lastPointer = end
		}
	})
}

func visitPointerWords(valueType types.Type, base int64, visit func(int64)) {
	valueType = representativeType(valueType)
	if _, sharedTypeParameter := valueType.(*types.TypeParam); sharedTypeParameter {
		// Shared generic values use one pointer-sized word, regardless of the
		// interface used as their source-level constraint. Treating an
		// unconstrained T as its `any` constraint would incorrectly describe a
		// two-word interface and mark storage beyond the value itself.
		visit(base)
		return
	}
	switch value := valueType.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		visit(base)
	case *types.Slice:
		visit(base)
	case *types.Interface:
		visit(base)
		visit(base + 8)
	case *types.Array:
		elementSize := typeSize(value.Elem())
		for index := int64(0); index < value.Len(); index++ {
			visitPointerWords(value.Elem(), base+index*elementSize, visit)
		}
	case *types.Struct:
		fields := structFields(value)
		offsets := structOffsets(fields)
		for index, field := range fields {
			visitPointerWords(field.Type(), base+offsets[index], visit)
		}
	case *types.Basic:
		if value.Kind() == types.UnsafePointer || value.Kind() == types.String {
			visit(base)
		}
	}
}

func runtimeKind(valueType types.Type) int {
	switch valueType.Underlying().(type) {
	case *types.Array:
		return 17
	case *types.Chan:
		return 18
	case *types.Signature:
		return 19
	case *types.Interface:
		return 20
	case *types.Map:
		return 21
	case *types.Struct:
		return 25
	case *types.Pointer:
		return 22
	case *types.Slice:
		return 23
	}
	if basic, ok := valueType.Underlying().(*types.Basic); ok {
		switch basic.Kind() {
		case types.Bool:
			return 1
		case types.Int:
			return 2
		case types.Int8:
			return 3
		case types.Int16:
			return 4
		case types.Int32:
			return 5
		case types.Int64:
			return 6
		case types.Uint:
			return 7
		case types.Uint8:
			return 8
		case types.Uint16:
			return 9
		case types.Uint32:
			return 10
		case types.Uint64:
			return 11
		case types.Uintptr:
			return 12
		case types.Float32:
			return 13
		case types.Float64:
			return 14
		case types.Complex64:
			return 15
		case types.Complex128:
			return 16
		case types.String:
			return 24
		case types.UnsafePointer:
			return 26
		}
	}
	return 0
}

const (
	mapLengthOffset   = 0
	mapCapacityOffset = 8
	mapKeysOffset     = 16
	mapValuesOffset   = 24
	mapUsedOffset     = 32
	mapHeaderSize     = 40
)

func (g *gen) makeMap(call *ast.CallExpr, mapType *types.Map) ir.Ref {
	capacity := g.fn.Long(8)
	if len(call.Args) == 2 {
		hint := g.expr(call.Args[1])
		tooSmall := g.cur.Cmp(ir.CmpSlt, ir.ClsL, hint, capacity)
		capacity = g.selectValue(tooSmall, capacity, hint, ir.ClsL)
	}
	return g.allocateMap(mapType, capacity)
}

func (g *gen) allocateMap(mapType *types.Map, capacity ir.Ref) ir.Ref {
	if g.runtimeAllocation {
		return g.cur.Call(
			ir.ClsP,
			g.fn.Sym("runtime.makemap", 0),
			g.typeTag(mapType),
			capacity,
			g.fn.ConstInt(ir.ClsP, 0),
		)
	}

	headerType := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "len", types.Typ[types.Int]),
		types.NewVar(token.NoPos, nil, "cap", types.Typ[types.Int]),
		types.NewVar(token.NoPos, nil, "keys", types.Typ[types.UnsafePointer]),
		types.NewVar(token.NoPos, nil, "values", types.Typ[types.UnsafePointer]),
		types.NewVar(token.NoPos, nil, "used", types.Typ[types.UnsafePointer]),
	}, nil)
	header := g.allocateTyped(headerType)
	keyBytes := g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Key())))
	valueBytes := g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Elem())))
	keys := g.allocateZeroed(keyBytes)
	values := g.allocateZeroed(valueBytes)
	used := g.allocateZeroed(capacity)
	g.cur.Store(capacity, g.offset(header, mapCapacityOffset))
	g.cur.Store(keys, g.offset(header, mapKeysOffset))
	g.cur.Store(values, g.offset(header, mapValuesOffset))
	g.cur.Store(used, g.offset(header, mapUsedOffset))
	return header
}

func (g *gen) cloneMap(mapping ir.Ref, mapType *types.Map) ir.Ref {
	clonedSlot := g.alloc(types.NewMap(mapType.Key(), mapType.Elem()))
	g.store(g.fn.ConstInt(ir.ClsP, 0), clonedSlot, types.NewMap(mapType.Key(), mapType.Elem()))
	done := g.block("mapcloneend")
	clone := g.block("mapclone")
	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, mapping, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(isNil, done, clone)

	g.cur = clone
	capacity := g.cur.Load(ir.ClsL, g.offset(mapping, mapCapacityOffset))
	cloned := g.allocateMap(mapType, capacity)
	length := g.cur.Load(ir.ClsL, g.offset(mapping, mapLengthOffset))
	g.cur.Store(length, g.offset(cloned, mapLengthOffset))
	memcpy := g.fn.Sym("goc_memcpy", 0)
	for _, array := range []struct {
		offset      int64
		elementSize int64
	}{
		{offset: mapKeysOffset, elementSize: typeSize(mapType.Key())},
		{offset: mapValuesOffset, elementSize: typeSize(mapType.Elem())},
		{offset: mapUsedOffset, elementSize: 1},
	} {
		source := g.cur.Load(ir.ClsP, g.offset(mapping, array.offset))
		destination := g.cur.Load(ir.ClsP, g.offset(cloned, array.offset))
		byteCount := g.cur.Mul(ir.ClsL, capacity, g.fn.Long(array.elementSize))
		g.cur.Call(ir.ClsP, memcpy, destination, source, byteCount)
	}
	g.store(cloned, clonedSlot, types.NewMap(mapType.Key(), mapType.Elem()))
	g.cur.Goto(done)

	g.cur = done
	return g.load(clonedSlot, types.NewMap(mapType.Key(), mapType.Elem()))
}

func (g *gen) mapLookup(index *ast.IndexExpr) (ir.Ref, ir.Ref) {
	mapType := g.typeAndValue(index.X).Type.Underlying().(*types.Map)
	mapping := g.expr(index.X)
	key := g.assignmentValue(index.Index, mapType.Key())
	return g.mapLookupValue(mapping, key, mapType, index)
}

func (g *gen) mapLookupValue(mapping, key ir.Ref, mapType *types.Map, expression ast.Expr) (ir.Ref, ir.Ref) {
	if g.runtimeAllocation {
		keyAddress := g.runtimeMapValueAddress(key, mapType.Key())
		foundSlot := g.alloc(types.Typ[types.Bool])
		valueAddress := g.cur.Call(
			ir.ClsP,
			g.fn.Sym("runtime.mapaccess2", 0),
			g.typeTag(mapType),
			mapping,
			keyAddress,
			foundSlot,
		)
		value := g.mapElementValue(valueAddress, mapType.Elem())
		return value, g.load(foundSlot, types.Typ[types.Bool])
	}

	valueSlot := g.allocLocal(mapType.Elem())
	foundSlot := g.alloc(types.Typ[types.Bool])
	zero := g.zeroValue(mapType.Elem())
	if isMemoryValue(mapType.Elem()) {
		g.assignLocal(zero, valueSlot, mapType.Elem())
	} else {
		g.store(zero, valueSlot, mapType.Elem())
	}
	g.store(g.fn.Word(0), foundSlot, types.Typ[types.Bool])

	done := g.block("mapdone")
	start := g.block("mapstart")
	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, mapping, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(isNil, done, start)

	g.cur = start
	indexSlot := g.alloc(types.Typ[types.Int])
	g.store(g.fn.Long(0), indexSlot, types.Typ[types.Int])
	test := g.block("maptest")
	body := g.block("mapbody")
	next := g.block("mapnext")
	compare := g.block("mapcompare")
	found := g.block("mapfound")
	g.cur.Goto(test)

	g.cur = test
	i := g.load(indexSlot, types.Typ[types.Int])
	capacity := g.cur.Load(ir.ClsL, g.offset(mapping, mapCapacityOffset))
	inRange := g.cur.Cmp(ir.CmpSlt, ir.ClsL, i, capacity)
	g.cur.Jnz(inRange, body, done)

	g.cur = body
	used := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	isUsed := g.cur.LoadSub(ir.ClsW, ir.SubUB, g.cur.Add(ir.ClsP, used, i))
	g.cur.Jnz(isUsed, compare, next)

	g.cur = compare
	keyAddress := g.mapElementAddress(mapping, mapKeysOffset, i, mapType.Key())
	storedKey := g.mapElementValue(keyAddress, mapType.Key())
	equal := g.binaryRaw(token.EQL, storedKey, key, mapType.Key(), expression)
	g.cur.Jnz(equal, found, next)

	g.cur = found
	valueAddress := g.mapElementAddress(mapping, mapValuesOffset, i, mapType.Elem())
	value := g.mapElementValue(valueAddress, mapType.Elem())
	if isMemoryValue(mapType.Elem()) {
		g.assignLocal(value, valueSlot, mapType.Elem())
	} else {
		g.store(value, valueSlot, mapType.Elem())
	}
	g.store(g.fn.Word(1), foundSlot, types.Typ[types.Bool])
	g.cur.Goto(done)

	g.cur = next
	i = g.cur.Add(ir.ClsL, i, g.fn.Long(1))
	g.store(i, indexSlot, types.Typ[types.Int])
	g.cur.Goto(test)

	g.cur = done
	return g.load(valueSlot, mapType.Elem()), g.load(foundSlot, types.Typ[types.Bool])
}

func (g *gen) mapAssign(index *ast.IndexExpr, valueExpression ast.Expr) {
	mapType := g.typeAndValue(index.X).Type.Underlying().(*types.Map)
	mapping := g.expr(index.X)
	key := g.assignmentValue(index.Index, mapType.Key())
	value := g.assignmentValue(valueExpression, mapType.Elem())
	g.mapAssignValue(mapping, key, value, mapType, index)
}

func (g *gen) mapAssignValue(mapping, key, value ir.Ref, mapType *types.Map, expression ast.Expr) {
	if g.runtimeAllocation {
		g.runtimeMapAssign(mapping, key, value, mapType)
		return
	}

	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, mapping, g.fn.ConstInt(ir.ClsP, 0))
	valid := g.block("mapassign")
	invalid := g.block("mapassignnil")
	g.cur.Jnz(isNil, invalid, valid)
	invalid.CallVoid(g.fn.Sym("abort", 0))
	invalid.Hlt()
	g.cur = valid

	indexSlot := g.alloc(types.Typ[types.Int])
	g.store(g.fn.Long(0), indexSlot, types.Typ[types.Int])
	test := g.block("mapassigntest")
	body := g.block("mapassignbody")
	insert := g.block("mapinsert")
	compare := g.block("mapassigncompare")
	update := g.block("mapupdate")
	next := g.block("mapassignnext")
	done := g.block("mapassignend")
	full := g.block("mapfull")
	g.cur.Goto(test)

	g.cur = test
	i := g.load(indexSlot, types.Typ[types.Int])
	capacity := g.cur.Load(ir.ClsL, g.offset(mapping, mapCapacityOffset))
	inRange := g.cur.Cmp(ir.CmpSlt, ir.ClsL, i, capacity)
	g.cur.Jnz(inRange, body, full)
	g.cur = full
	newCapacity := g.cur.Mul(ir.ClsL, capacity, g.fn.Long(2))
	keys := g.cur.Load(ir.ClsP, g.offset(mapping, mapKeysOffset))
	values := g.cur.Load(ir.ClsP, g.offset(mapping, mapValuesOffset))
	grownUsed := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	if g.runtimeAllocation {
		oldKeys := keys
		oldValues := values
		oldUsed := grownUsed
		keySize := g.fn.Long(typeSize(mapType.Key()))
		valueSize := g.fn.Long(typeSize(mapType.Elem()))
		keys = g.allocateZeroed(g.cur.Mul(ir.ClsL, newCapacity, keySize))
		values = g.allocateZeroed(g.cur.Mul(ir.ClsL, newCapacity, valueSize))
		grownUsed = g.allocateZeroed(newCapacity)
		memcpy := g.fn.Sym("goc_memcpy", 0)
		g.cur.Call(ir.ClsP, memcpy, keys, oldKeys, g.cur.Mul(ir.ClsL, capacity, keySize))
		g.cur.Call(ir.ClsP, memcpy, values, oldValues, g.cur.Mul(ir.ClsL, capacity, valueSize))
		g.cur.Call(ir.ClsP, memcpy, grownUsed, oldUsed, capacity)
	} else {
		realloc := g.fn.Sym("realloc", 0)
		keys = g.cur.Call(ir.ClsP, realloc, keys, g.cur.Mul(ir.ClsL, newCapacity, g.fn.Long(typeSize(mapType.Key()))))
		values = g.cur.Call(ir.ClsP, realloc, values, g.cur.Mul(ir.ClsL, newCapacity, g.fn.Long(typeSize(mapType.Elem()))))
		grownUsed = g.cur.Call(ir.ClsP, realloc, grownUsed, newCapacity)
	}
	memset := g.fn.Sym("goc_memset", 0)
	g.cur.Call(ir.ClsP, memset, g.cur.Add(ir.ClsP, grownUsed, capacity), g.fn.Word(0), capacity)
	g.cur.Store(newCapacity, g.offset(mapping, mapCapacityOffset))
	g.cur.Store(keys, g.offset(mapping, mapKeysOffset))
	g.cur.Store(values, g.offset(mapping, mapValuesOffset))
	g.cur.Store(grownUsed, g.offset(mapping, mapUsedOffset))
	g.cur.Goto(insert)

	g.cur = body
	used := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	usedAddress := g.cur.Add(ir.ClsP, used, i)
	isUsed := g.cur.LoadSub(ir.ClsW, ir.SubUB, usedAddress)
	g.cur.Jnz(isUsed, compare, insert)

	g.cur = compare
	keyAddress := g.mapElementAddress(mapping, mapKeysOffset, i, mapType.Key())
	storedKey := g.mapElementValue(keyAddress, mapType.Key())
	equal := g.binaryRaw(token.EQL, storedKey, key, mapType.Key(), expression)
	g.cur.Jnz(equal, update, next)

	g.cur = insert
	insertUsed := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	insertUsedAddress := g.cur.Add(ir.ClsP, insertUsed, i)
	keyAddress = g.mapElementAddress(mapping, mapKeysOffset, i, mapType.Key())
	g.storeMapElement(key, keyAddress, mapType.Key())
	g.cur.StoreSub(ir.SubUB, g.fn.Word(1), insertUsedAddress)
	length := g.cur.Load(ir.ClsL, g.offset(mapping, mapLengthOffset))
	length = g.cur.Add(ir.ClsL, length, g.fn.Long(1))
	g.cur.Store(length, g.offset(mapping, mapLengthOffset))
	g.cur.Goto(update)

	g.cur = update
	valueAddress := g.mapElementAddress(mapping, mapValuesOffset, i, mapType.Elem())
	g.storeMapElement(value, valueAddress, mapType.Elem())
	g.cur.Goto(done)

	g.cur = next
	i = g.cur.Add(ir.ClsL, i, g.fn.Long(1))
	g.store(i, indexSlot, types.Typ[types.Int])
	g.cur.Goto(test)
	g.cur = done
}

func (g *gen) runtimeMapAssign(mapping, key, value ir.Ref, mapType *types.Map) {
	keyAddress := g.runtimeMapValueAddress(key, mapType.Key())
	valueAddress := g.cur.Call(
		ir.ClsP,
		g.fn.Sym("runtime.mapassign", 0),
		g.typeTag(mapType),
		mapping,
		keyAddress,
	)
	g.storeMapElement(value, valueAddress, mapType.Elem())
}

func (g *gen) runtimeMapValueAddress(value ir.Ref, valueType types.Type) ir.Ref {
	if isInlineAggregate(valueType) || isInterfaceValue(valueType) {
		return value
	}
	address := g.localAllocTyped(valueType)
	g.store(value, address, valueType)
	return address
}

func (g *gen) mapElementValue(address ir.Ref, valueType types.Type) ir.Ref {
	if g.runtimeAllocation && isSliceType(valueType) {
		return g.load(address, valueType)
	}
	if isInlineAggregate(valueType) || isInterfaceValue(valueType) {
		return address
	}
	return g.load(address, valueType)
}

func (g *gen) storeMapElement(value, address ir.Ref, valueType types.Type) {
	if isInlineAggregate(valueType) || isInterfaceValue(valueType) {
		g.storeInlineValue(value, address, valueType)
		return
	}
	g.store(value, address, valueType)
}

func (g *gen) mapElementAddress(mapping ir.Ref, arrayOffset int64, index ir.Ref, elementType types.Type) ir.Ref {
	base := g.cur.Load(ir.ClsP, g.offset(mapping, arrayOffset))
	offset := index
	if size := typeSize(elementType); size != 1 {
		offset = g.cur.Mul(ir.ClsL, index, g.fn.Long(size))
	}
	return g.cur.Add(ir.ClsP, base, offset)
}

func (g *gen) zero(address ir.Ref, valueType types.Type) {
	memset := g.fn.Sym("goc_memset", 0)
	g.cur.Call(ir.ClsP, memset, address, g.fn.Word(0), g.fn.Long(typeSize(valueType)))
}

func (g *gen) mapLookupAssignment(statement *ast.AssignStmt, index *ast.IndexExpr) {
	value, found := g.mapLookup(index)
	g.assignMapResult(statement, 0, value)
	g.assignMapResult(statement, 1, found)
}

func (g *gen) mapLength(expression ast.Expr) ir.Ref {
	mapping := g.expr(expression)
	result := g.alloc(types.Typ[types.Int])
	g.store(g.fn.Long(0), result, types.Typ[types.Int])
	nonNil := g.block("maplennonnil")
	done := g.block("maplenend")
	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, mapping, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(isNil, done, nonNil)
	g.cur = nonNil
	g.store(g.cur.Load(ir.ClsL, mapping), result, types.Typ[types.Int])
	g.cur.Goto(done)
	g.cur = done
	return g.load(result, types.Typ[types.Int])
}

func (g *gen) assignMapResult(statement *ast.AssignStmt, resultIndex int, value ir.Ref) {
	lhs := statement.Lhs[resultIndex]
	identifier, ok := lhs.(*ast.Ident)
	if !ok {
		g.fail(lhs, "map lookup result target must be an identifier")
		return
	}
	if identifier.Name == "_" {
		return
	}
	object := g.info.Uses[identifier]
	if statement.Tok == token.DEFINE && object == nil {
		object = g.info.Defs[identifier]
	}
	slot, exists := g.addr(object)
	if !exists {
		objectType := g.objectType(object)
		slot = g.variableStorage(object, objectType)
	}
	objectType := g.objectType(object)
	g.store(g.coerce(value, objectType), slot, objectType)
}

func (g *gen) mapDelete(mapExpression, keyExpression ast.Expr) {
	mapType := g.typeAndValue(mapExpression).Type.Underlying().(*types.Map)
	mapping := g.expr(mapExpression)
	key := g.assignmentValue(keyExpression, mapType.Key())
	g.mapDeleteValues(mapping, key, mapType, keyExpression)
}

func (g *gen) mapDeleteValues(mapping, key ir.Ref, mapType *types.Map, source ast.Node) {
	if g.runtimeAllocation {
		keyAddress := g.runtimeMapValueAddress(key, mapType.Key())
		g.cur.CallVoid(g.fn.Sym("runtime.mapdelete", 0), g.typeTag(mapType), mapping, keyAddress)
		return
	}

	done := g.block("mapdeleteend")
	start := g.block("mapdeletestart")
	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, mapping, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(isNil, done, start)

	g.cur = start
	indexSlot := g.alloc(types.Typ[types.Int])
	g.store(g.fn.Long(0), indexSlot, types.Typ[types.Int])
	test := g.block("mapdeletetest")
	body := g.block("mapdeletebody")
	compare := g.block("mapdeletecompare")
	remove := g.block("mapdeleteremove")
	next := g.block("mapdeletenext")
	g.cur.Goto(test)

	g.cur = test
	i := g.load(indexSlot, types.Typ[types.Int])
	capacity := g.cur.Load(ir.ClsL, g.offset(mapping, mapCapacityOffset))
	inRange := g.cur.Cmp(ir.CmpSlt, ir.ClsL, i, capacity)
	g.cur.Jnz(inRange, body, done)

	g.cur = body
	used := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	usedAddress := g.cur.Add(ir.ClsP, used, i)
	isUsed := g.cur.LoadSub(ir.ClsW, ir.SubUB, usedAddress)
	g.cur.Jnz(isUsed, compare, next)

	g.cur = compare
	keyAddress := g.mapElementAddress(mapping, mapKeysOffset, i, mapType.Key())
	storedKey := g.mapElementValue(keyAddress, mapType.Key())
	equal := g.binaryRaw(token.EQL, storedKey, key, mapType.Key(), source)
	g.cur.Jnz(equal, remove, next)

	g.cur = remove
	g.cur.StoreSub(ir.SubUB, g.fn.Word(0), usedAddress)
	g.zero(keyAddress, mapType.Key())
	valueAddress := g.mapElementAddress(mapping, mapValuesOffset, i, mapType.Elem())
	g.zero(valueAddress, mapType.Elem())
	length := g.cur.Load(ir.ClsL, mapping)
	g.cur.Store(g.cur.Sub(ir.ClsL, length, g.fn.Long(1)), mapping)
	g.cur.Goto(done)

	g.cur = next
	i = g.cur.Add(ir.ClsL, i, g.fn.Long(1))
	g.store(i, indexSlot, types.Typ[types.Int])
	g.cur.Goto(test)
	g.cur = done
}

func (g *gen) mapClear(expression ast.Expr, mapType *types.Map) {
	mapping := g.expr(expression)
	if g.runtimeAllocation {
		g.cur.CallVoid(g.fn.Sym("runtime.mapclear", 0), g.typeTag(mapType), mapping)
		return
	}

	done := g.block("mapclearend")
	clearBlock := g.block("mapclear")
	isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, mapping, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(isNil, done, clearBlock)
	g.cur = clearBlock
	capacity := g.cur.Load(ir.ClsL, g.offset(mapping, mapCapacityOffset))
	keys := g.cur.Load(ir.ClsP, g.offset(mapping, mapKeysOffset))
	values := g.cur.Load(ir.ClsP, g.offset(mapping, mapValuesOffset))
	used := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	memset := g.fn.Sym("goc_memset", 0)
	g.cur.Call(ir.ClsP, memset, keys, g.fn.Word(0), g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Key()))))
	g.cur.Call(ir.ClsP, memset, values, g.fn.Word(0), g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Elem()))))
	g.cur.Call(ir.ClsP, memset, used, g.fn.Word(0), capacity)
	g.cur.Store(g.fn.Long(0), mapping)
	g.cur.Goto(done)
	g.cur = done
}

func (g *gen) builtinPrint(call *ast.CallExpr, newline bool) {
	if g.runtimeAllocation {
		g.builtinRuntimePrint(call, newline)
		return
	}

	printf := g.fn.Sym("printf", 0)
	for _, argument := range call.Args {
		value := g.info.Types[argument]
		if value.Value != nil && value.Value.Kind() == constant.String {
			format := g.cString("%s")
			text := g.cString(constant.StringVal(value.Value))
			g.cur.Call(ir.ClsW, printf, format, text)
			continue
		}

		argumentType := value.Type
		if basic, ok := argumentType.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			descriptor := g.expr(argument)
			data := g.cur.Load(ir.ClsP, descriptor)
			length := g.cur.Load(ir.ClsL, g.offset(descriptor, 8))
			g.cur.Call(ir.ClsW, printf, g.cString("%.*s"), length, data)
			continue
		}
		class, ok := scalar(argumentType)
		if !ok {
			g.fail(argument, "unsupported print operand %s", argumentType)
			return
		}
		formatText := "%lld"
		if _, pointer := argumentType.Underlying().(*types.Pointer); pointer {
			formatText = "%p"
		} else if basic, basicOK := argumentType.Underlying().(*types.Basic); basicOK && basic.Info()&types.IsUnsigned != 0 {
			formatText = "%llu"
		}
		argumentValue := g.expr(argument)
		if class == ir.ClsW {
			argumentValue = g.cur.Extsw(ir.ClsL, argumentValue)
		}
		g.cur.Call(ir.ClsW, printf, g.cString(formatText), argumentValue)
	}
	if newline {
		g.cur.Call(ir.ClsW, printf, g.cString("\n"))
	}
}

func (g *gen) builtinRuntimePrint(call *ast.CallExpr, newline bool) {
	for _, argument := range call.Args {
		value := g.info.Types[argument]
		if value.Value != nil && value.Value.Kind() == constant.String {
			contents := constant.StringVal(value.Value)
			g.callRuntimePrint(call, "printstring", g.stringConstant(contents))
			continue
		}

		argumentType := value.Type
		if basic, ok := argumentType.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			g.callRuntimePrint(call, "printstring", g.expr(argument))
			continue
		}
		if isSliceType(argumentType) {
			// The runtime's diagnostic print builtin accepts otherwise unsupported
			// values as an address-shaped operand. Materialize a real header here;
			// ordinary slice calls continue to pass the three value components.
			header := g.materializeSlice(g.expr(argument))
			g.callRuntimePrint(call, "printint", header)
			continue
		}

		class, ok := scalar(argumentType)
		if !ok {
			g.fail(argument, "unsupported print operand %s", argumentType)
			return
		}
		argumentValue := g.expr(argument)
		basic, isBasic := argumentType.Underlying().(*types.Basic)
		switch {
		case isBasic && basic.Kind() == types.Bool:
			g.callRuntimePrint(call, "printbool", argumentValue)
		case class == ir.ClsS:
			g.callRuntimePrint(call, "printfloat32", argumentValue)
		case class == ir.ClsD:
			g.callRuntimePrint(call, "printfloat64", argumentValue)
		case isRuntimePrintPointer(argumentType):
			g.callRuntimePrint(call, "printhex", g.cur.Copy(ir.ClsL, argumentValue))
		case isBasic && basic.Info()&types.IsUnsigned != 0:
			if class == ir.ClsW {
				argumentValue = g.cur.Extuw(ir.ClsL, argumentValue)
			}
			g.callRuntimePrint(call, "printuint", argumentValue)
		default:
			if class == ir.ClsW {
				argumentValue = g.cur.Extsw(ir.ClsL, argumentValue)
			}
			g.callRuntimePrint(call, "printint", argumentValue)
		}
	}
	if newline {
		g.callRuntimePrint(call, "printnl")
	}
}

func (g *gen) callRuntimePrint(node ast.Node, name string, arguments ...ir.Ref) {
	for function := range g.functionDecls {
		if function.Pkg() == nil || function.Pkg().Path() != "runtime" || function.Name() != name {
			continue
		}
		signature := compiledFunctionSignature(function)
		if signature.Recv() != nil {
			continue
		}
		g.callVoidWithSignature(g.fn.Sym(g.functionSymbol(function), 0), arguments, signature, nil)
		return
	}
	g.fail(node, "runtime print function %s is unavailable", name)
}

func isRuntimePrintPointer(valueType types.Type) bool {
	switch valueType.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		return true
	}
	basic, ok := valueType.Underlying().(*types.Basic)
	return ok && basic.Kind() == types.UnsafePointer
}

func (g *gen) cString(contents string) ir.Ref {
	bytes := append([]byte(contents), 0)
	values := make([]int64, len(bytes))
	for i, value := range bytes {
		values[i] = int64(value)
	}
	name := fmt.Sprintf(".goc.cstring.%d", len(g.mod.Data))
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: 1,
		Items: []ir.DataItem{{Sub: ir.SubUB, Ints: values}},
	})
	return g.fn.Sym(name, 0)
}

func (g *gen) selectValue(condition, ifTrue, ifFalse ir.Ref, class ir.Cls) ir.Ref {
	trueBlock := g.block("selecttrue")
	falseBlock := g.block("selectfalse")
	done := g.block("selectend")
	g.cur.Jnz(condition, trueBlock, falseBlock)
	trueBlock.Goto(done)
	falseBlock.Goto(done)
	g.cur = done
	return done.Phi(class,
		ir.PhiEdge{From: trueBlock, Val: ifTrue},
		ir.PhiEdge{From: falseBlock, Val: ifFalse},
	)
}

func typeSize(t types.Type) int64 {
	t = representativeType(t)
	if _, parameter := t.(*types.TypeParam); parameter {
		return pointerSize()
	}
	switch value := t.Underlying().(type) {
	case *types.Array:
		return value.Len() * typeSize(value.Elem())
	case *types.Struct:
		fields := structFields(value)
		offsets := structOffsets(fields)
		if len(fields) == 0 {
			return 0
		}
		size := offsets[len(fields)-1] + typeSize(fields[len(fields)-1].Type())
		if size > 0 && typeSize(fields[len(fields)-1].Type()) == 0 {
			size++
		}
		return alignTo(size, typeAlign(t))
	}
	sizes := types.SizesFor("gc", runtime.GOARCH)
	return sizes.Sizeof(t)
}

func typeAlign(t types.Type) int64 {
	t = representativeType(t)
	if _, parameter := t.(*types.TypeParam); parameter {
		return pointerSize()
	}
	switch value := t.Underlying().(type) {
	case *types.Array:
		return typeAlign(value.Elem())
	case *types.Struct:
		alignment := int64(1)
		for index := 0; index < value.NumFields(); index++ {
			if fieldAlignment := typeAlign(value.Field(index).Type()); fieldAlignment > alignment {
				alignment = fieldAlignment
			}
		}
		return alignment
	}
	return int64(types.SizesFor("gc", runtime.GOARCH).Alignof(t))
}

func structOffsets(fields []*types.Var) []int64 {
	offsets := make([]int64, len(fields))
	offset := int64(0)
	for index, field := range fields {
		offset = alignTo(offset, typeAlign(field.Type()))
		offsets[index] = offset
		offset += typeSize(field.Type())
	}
	return offsets
}

func alignTo(value, alignment int64) int64 {
	return (value + alignment - 1) &^ (alignment - 1)
}

func pointerSize() int64 {
	return int64(types.SizesFor("gc", runtime.GOARCH).Sizeof(types.Typ[types.Uintptr]))
}

func representativeType(valueType types.Type) types.Type {
	parameter, ok := valueType.(*types.TypeParam)
	if !ok {
		return valueType
	}
	terms := typeParameterTerms(parameter)
	if len(terms) == 0 {
		return valueType
	}

	representative := terms[0]
	sizes := types.SizesFor("gc", runtime.GOARCH)
	for _, term := range terms[1:] {
		if sizes.Sizeof(term) > sizes.Sizeof(representative) {
			representative = term
		}
	}
	return representative
}

func typeParameterTerms(parameter *types.TypeParam) []types.Type {
	constraint, ok := parameter.Constraint().Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	constraint.Complete()

	var terms []types.Type
	for index := 0; index < constraint.NumEmbeddeds(); index++ {
		embedded := constraint.EmbeddedType(index)
		switch embedded := embedded.(type) {
		case *types.Union:
			for termIndex := 0; termIndex < embedded.Len(); termIndex++ {
				terms = append(terms, embedded.Term(termIndex).Type())
			}
		case *types.TypeParam:
			terms = append(terms, typeParameterTerms(embedded)...)
		case *types.Interface:
			for nestedIndex := 0; nestedIndex < embedded.NumEmbeddeds(); nestedIndex++ {
				terms = append(terms, embedded.EmbeddedType(nestedIndex))
			}
		default:
			terms = append(terms, embedded)
		}
	}
	return terms
}

func fieldOffset(selection *types.Selection) int64 {
	t := selection.Recv()
	if pointer, ok := t.(*types.Pointer); ok {
		t = pointer.Elem()
	}
	var offset int64
	for _, index := range selection.Index() {
		structure := t.Underlying().(*types.Struct)
		sizes := types.SizesFor("gc", runtime.GOARCH)
		offsets := sizes.Offsetsof(structFields(structure))
		offset += offsets[index]
		t = structure.Field(index).Type()
		if pointer, ok := t.(*types.Pointer); ok {
			t = pointer.Elem()
		}
	}
	return offset
}

func structFields(structure *types.Struct) []*types.Var {
	fields := make([]*types.Var, structure.NumFields())
	for i := range fields {
		fields[i] = structure.Field(i)
	}
	return fields
}

func (g *gen) logical(n *ast.BinaryExpr) ir.Ref {
	left := g.expr(n.X)
	leftBlock := g.cur
	rhsBlock, shortBlock, done := g.block("logicrhs"), g.block("logicshort"), g.block("logicend")
	if n.Op == token.LAND {
		leftBlock.Jnz(left, rhsBlock, shortBlock)
	} else {
		leftBlock.Jnz(left, shortBlock, rhsBlock)
	}
	g.cur = rhsBlock
	right := g.expr(n.Y)
	rightBlock := g.cur
	rightBlock.Goto(done)
	g.cur = shortBlock
	short := g.fn.Word(0)
	if n.Op == token.LOR {
		short = g.fn.Word(1)
	}
	shortBlock.Goto(done)
	g.cur = done
	return done.Phi(ir.ClsW, ir.PhiEdge{From: rightBlock, Val: right}, ir.PhiEdge{From: shortBlock, Val: short})
}

func functionSymbol(function *types.Func) string {
	name := function.Name()
	signature, _ := function.Type().(*types.Signature)
	if signature != nil && signature.Recv() != nil {
		receiver := signature.Recv().Type()
		if pointer, ok := receiver.(*types.Pointer); ok {
			receiver = pointer.Elem()
		}
		typeName := types.TypeString(receiver, func(*types.Package) string { return "" })
		typeName = strings.TrimPrefix(typeName, ".")
		if open := strings.IndexByte(typeName, '['); open >= 0 {
			if close := strings.LastIndexByte(typeName, ']'); close > open {
				typeName = typeName[:open] + typeName[close+1:]
			}
		}
		name = typeName + "." + name
	}
	if function.Pkg() == nil {
		return name
	}
	return function.Pkg().Path() + "." + name
}

func compiledFunctionSignature(function *types.Func) *types.Signature {
	return function.Origin().Type().(*types.Signature)
}

func genericInstanceSymbol(function *types.Func, arguments []types.Type) string {
	var symbol strings.Builder
	symbol.WriteString(functionSymbol(function.Origin()))
	symbol.WriteByte('[')
	for index, argument := range arguments {
		if index > 0 {
			symbol.WriteByte(',')
		}
		argument = canonicalAliasType(argument)
		symbol.WriteString(types.TypeString(argument, func(pkg *types.Package) string {
			return pkg.Path()
		}))
	}
	symbol.WriteByte(']')
	return symbol.String()
}

func functionTypeSubstitutions(function functionDecl) map[*types.TypeParam]types.Type {
	if len(function.typeArguments) == 0 {
		return nil
	}
	object, ok := function.info.Defs[function.decl.Name].(*types.Func)
	if !ok {
		return nil
	}
	parameters := signatureTypeParameters(object.Origin().Type().(*types.Signature))
	substitutions := make(map[*types.TypeParam]types.Type, len(parameters))
	for index := 0; index < len(parameters) && index < len(function.typeArguments); index++ {
		substitutions[parameters[index]] = function.typeArguments[index]
	}
	return substitutions
}

func substituteType(valueType types.Type, substitutions map[*types.TypeParam]types.Type) types.Type {
	if valueType == nil || len(substitutions) == 0 {
		return valueType
	}
	switch value := valueType.(type) {
	case *types.TypeParam:
		if replacement := substitutions[value]; replacement != nil {
			return canonicalAliasType(replacement)
		}
		return value
	case *types.Alias:
		return substituteType(types.Unalias(value), substitutions)
	case *types.Array:
		return types.NewArray(substituteType(value.Elem(), substitutions), value.Len())
	case *types.Slice:
		return types.NewSlice(substituteType(value.Elem(), substitutions))
	case *types.Pointer:
		return types.NewPointer(substituteType(value.Elem(), substitutions))
	case *types.Map:
		return types.NewMap(
			substituteType(value.Key(), substitutions),
			substituteType(value.Elem(), substitutions),
		)
	case *types.Chan:
		return types.NewChan(value.Dir(), substituteType(value.Elem(), substitutions))
	case *types.Tuple:
		variables := make([]*types.Var, value.Len())
		for index := range variables {
			variable := value.At(index)
			variables[index] = types.NewVar(
				variable.Pos(),
				variable.Pkg(),
				variable.Name(),
				substituteType(variable.Type(), substitutions),
			)
		}
		return types.NewTuple(variables...)
	case *types.Signature:
		var receiver *types.Var
		if value.Recv() != nil {
			receiver = types.NewVar(
				value.Recv().Pos(),
				value.Recv().Pkg(),
				value.Recv().Name(),
				substituteType(value.Recv().Type(), substitutions),
			)
		}
		parameters := substituteType(value.Params(), substitutions).(*types.Tuple)
		results := substituteType(value.Results(), substitutions).(*types.Tuple)
		return types.NewSignatureType(receiver, nil, nil, parameters, results, value.Variadic())
	case *types.Named:
		if value.TypeArgs().Len() == 0 {
			return value
		}
		arguments := make([]types.Type, value.TypeArgs().Len())
		changed := false
		for index := range arguments {
			arguments[index] = substituteType(value.TypeArgs().At(index), substitutions)
			changed = changed || arguments[index] != value.TypeArgs().At(index)
		}
		if !changed {
			return value
		}
		instantiated, err := types.Instantiate(nil, value.Origin(), arguments, false)
		if err == nil {
			return instantiated
		}
	}
	return valueType
}

func canonicalAliasType(valueType types.Type) types.Type {
	if valueType == nil {
		return nil
	}
	valueType = types.Unalias(valueType)
	switch value := valueType.(type) {
	case *types.Array:
		element := canonicalAliasType(value.Elem())
		if element != value.Elem() {
			return types.NewArray(element, value.Len())
		}
	case *types.Slice:
		element := canonicalAliasType(value.Elem())
		if element != value.Elem() {
			return types.NewSlice(element)
		}
	case *types.Pointer:
		element := canonicalAliasType(value.Elem())
		if element != value.Elem() {
			return types.NewPointer(element)
		}
	case *types.Map:
		key := canonicalAliasType(value.Key())
		element := canonicalAliasType(value.Elem())
		if key != value.Key() || element != value.Elem() {
			return types.NewMap(key, element)
		}
	case *types.Chan:
		element := canonicalAliasType(value.Elem())
		if element != value.Elem() {
			return types.NewChan(value.Dir(), element)
		}
	case *types.Tuple:
		variables, changed := canonicalAliasTuple(value)
		if changed {
			return types.NewTuple(variables...)
		}
	case *types.Signature:
		parameterVariables, parametersChanged := canonicalAliasTuple(value.Params())
		resultVariables, resultsChanged := canonicalAliasTuple(value.Results())
		if parametersChanged || resultsChanged {
			return types.NewSignatureType(
				value.Recv(),
				typeParameterList(value.RecvTypeParams()),
				typeParameterList(value.TypeParams()),
				types.NewTuple(parameterVariables...),
				types.NewTuple(resultVariables...),
				value.Variadic(),
			)
		}
	case *types.Struct:
		fields := make([]*types.Var, value.NumFields())
		tags := make([]string, value.NumFields())
		changed := false
		for index := range fields {
			field := value.Field(index)
			fieldType := canonicalAliasType(field.Type())
			fields[index] = types.NewField(field.Pos(), field.Pkg(), field.Name(), fieldType, field.Embedded())
			tags[index] = value.Tag(index)
			changed = changed || fieldType != field.Type()
		}
		if changed {
			return types.NewStruct(fields, tags)
		}
	case *types.Interface:
		methods := make([]*types.Func, value.NumExplicitMethods())
		embeddeds := make([]types.Type, value.NumEmbeddeds())
		changed := false
		for index := range methods {
			method := value.ExplicitMethod(index)
			methodType := canonicalAliasType(method.Type())
			methods[index] = types.NewFunc(method.Pos(), method.Pkg(), method.Name(), methodType.(*types.Signature))
			changed = changed || methodType != method.Type()
		}
		for index := range embeddeds {
			embeddeds[index] = canonicalAliasType(value.EmbeddedType(index))
			changed = changed || embeddeds[index] != value.EmbeddedType(index)
		}
		if changed {
			return types.NewInterfaceType(methods, embeddeds).Complete()
		}
	case *types.Union:
		terms := make([]*types.Term, value.Len())
		changed := false
		for index := range terms {
			term := value.Term(index)
			termType := canonicalAliasType(term.Type())
			terms[index] = types.NewTerm(term.Tilde(), termType)
			changed = changed || termType != term.Type()
		}
		if changed {
			return types.NewUnion(terms)
		}
	case *types.Named:
		if value.TypeArgs().Len() == 0 {
			return value
		}
		arguments := make([]types.Type, value.TypeArgs().Len())
		changed := false
		for index := range arguments {
			arguments[index] = canonicalAliasType(value.TypeArgs().At(index))
			changed = changed || arguments[index] != value.TypeArgs().At(index)
		}
		if changed {
			instantiated, err := types.Instantiate(nil, value.Origin(), arguments, false)
			if err == nil {
				return instantiated
			}
		}
	}
	return valueType
}

func canonicalAliasTuple(tuple *types.Tuple) ([]*types.Var, bool) {
	variables := make([]*types.Var, tuple.Len())
	changed := false
	for index := range variables {
		variable := tuple.At(index)
		variableType := canonicalAliasType(variable.Type())
		variables[index] = types.NewVar(variable.Pos(), variable.Pkg(), variable.Name(), variableType)
		changed = changed || variableType != variable.Type()
	}
	return variables, changed
}

func typeParameterList(list *types.TypeParamList) []*types.TypeParam {
	parameters := make([]*types.TypeParam, list.Len())
	for index := range parameters {
		parameters[index] = list.At(index)
	}
	return parameters
}

func (g *gen) typeSubstitutions() map[*types.TypeParam]types.Type {
	if len(g.typeArguments) == 0 || g.currentFunction == nil {
		return nil
	}
	parameters := signatureTypeParameters(g.currentFunction.Origin().Type().(*types.Signature))
	substitutions := make(map[*types.TypeParam]types.Type, len(parameters))
	for index := 0; index < len(parameters) && index < len(g.typeArguments); index++ {
		substitutions[parameters[index]] = g.typeArguments[index]
	}
	return substitutions
}

func (g *gen) concreteType(valueType types.Type) types.Type {
	return substituteType(valueType, g.typeSubstitutions())
}

func (g *gen) typeAndValue(expression ast.Expr) types.TypeAndValue {
	value := g.info.Types[expression]
	value.Type = g.concreteType(value.Type)
	return value
}

func (g *gen) objectType(object types.Object) types.Type {
	return g.concreteType(object.Type())
}

func functionIdentifier(expression ast.Expr) *ast.Ident {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression
	case *ast.SelectorExpr:
		return expression.Sel
	case *ast.IndexExpr:
		return functionIdentifier(expression.X)
	case *ast.IndexListExpr:
		return functionIdentifier(expression.X)
	}
	return nil
}

func (g *gen) instantiatedFunctionSymbol(function *types.Func, expression ast.Expr) (string, bool) {
	identifier := functionIdentifier(expression)
	origin := function.Origin()
	originSignature := origin.Type().(*types.Signature)
	var arguments []types.Type
	if identifier != nil && originSignature.TypeParams().Len() != 0 {
		instance, ok := g.info.Instances[identifier]
		if !ok {
			return "", false
		}
		arguments = make([]types.Type, instance.TypeArgs.Len())
		for index := range arguments {
			arguments[index] = g.concreteType(instance.TypeArgs.At(index))
		}
	} else if originSignature.RecvTypeParams().Len() != 0 {
		if selector, ok := expression.(*ast.SelectorExpr); ok {
			selection := g.info.Selections[selector]
			if selection != nil {
				arguments = receiverTypeArgumentsFromType(selection.Recv())
			}
		}
		if len(arguments) == 0 {
			arguments = receiverTypeArguments(function)
		}
		for index := range arguments {
			arguments[index] = g.concreteType(arguments[index])
		}
	} else {
		return "", false
	}
	if len(arguments) != len(signatureTypeParameters(originSignature)) {
		return "", false
	}
	return genericInstanceSymbol(origin, arguments), true
}

func signatureTypeParameters(signature *types.Signature) []*types.TypeParam {
	parameters := make([]*types.TypeParam, 0, signature.RecvTypeParams().Len()+signature.TypeParams().Len())
	for index := 0; index < signature.RecvTypeParams().Len(); index++ {
		parameters = append(parameters, signature.RecvTypeParams().At(index))
	}
	for index := 0; index < signature.TypeParams().Len(); index++ {
		parameters = append(parameters, signature.TypeParams().At(index))
	}
	return parameters
}

func receiverTypeArguments(function *types.Func) []types.Type {
	signature, ok := function.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return nil
	}
	return receiverTypeArgumentsFromType(signature.Recv().Type())
}

func receiverTypeArgumentsFromType(receiverType types.Type) []types.Type {
	if pointer, ok := receiverType.(*types.Pointer); ok {
		receiverType = pointer.Elem()
	}
	named, ok := types.Unalias(receiverType).(*types.Named)
	if !ok {
		return nil
	}
	arguments := make([]types.Type, named.TypeArgs().Len())
	for index := range arguments {
		arguments[index] = named.TypeArgs().At(index)
	}
	return arguments
}

func (g *gen) functionSymbol(function *types.Func) string {
	if name := g.linkNames[function]; name != "" {
		return name
	}
	if name := g.initSymbols[function]; name != "" {
		return name
	}
	return functionSymbol(function)
}

func (g *gen) binary(op token.Token, x, y ir.Ref, t types.Type, n ast.Node) ir.Ref {
	return g.coerce(g.binaryRaw(op, x, y, t, n), t)
}

func isNilExpression(expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "nil"
}

func (g *gen) interfaceIsNil(descriptor ir.Ref) ir.Ref {
	nilDescriptor := g.block("interfacenildescriptor")
	checkType := g.block("interfaceniltype")
	done := g.block("interfacenildone")

	descriptorIsNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, descriptor, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(descriptorIsNil, nilDescriptor, checkType)

	nilDescriptor.Goto(done)

	g.cur = checkType
	dynamicType := g.cur.Load(ir.ClsP, descriptor)
	typeIsNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicType, g.fn.ConstInt(ir.ClsP, 0))
	checkType = g.cur
	checkType.Goto(done)

	g.cur = done
	return done.Phi(ir.ClsW,
		ir.PhiEdge{From: nilDescriptor, Val: g.fn.Word(1)},
		ir.PhiEdge{From: checkType, Val: typeIsNil},
	)
}

func (g *gen) binaryRaw(op token.Token, x, y ir.Ref, t types.Type, n ast.Node) ir.Ref {
	c, _ := scalar(t)
	if isSharedTypeParameter(t) && (op == token.EQL || op == token.NEQ) {
		equal := g.cur.Call(ir.ClsW, g.fn.Sym("runtime.nilinterequal", 0), x, y)
		if op == token.NEQ {
			return g.cur.Cmp(ir.CmpEq, ir.ClsW, equal, g.fn.Word(0))
		}
		return equal
	}
	if basic, ok := t.Underlying().(*types.Basic); ok && basic.Kind() == types.String && op == token.ADD {
		if !g.runtimeAllocation {
			g.fail(n, "string concatenation requires the Go runtime")
			return ir.R
		}
		return g.concatStrings(x, y)
	}
	if (op == token.EQL || op == token.NEQ) && t != nil {
		if _, isInterface := t.Underlying().(*types.Interface); isInterface {
			binaryExpression, ok := n.(*ast.BinaryExpr)
			if ok && (isNilExpression(binaryExpression.X) || isNilExpression(binaryExpression.Y)) {
				interfaceValue := x
				if isNilExpression(binaryExpression.X) {
					interfaceValue = y
				}
				isNil := g.interfaceIsNil(interfaceValue)
				if op == token.NEQ {
					return g.cur.Cmp(ir.CmpEq, ir.ClsW, isNil, g.fn.Word(0))
				}
				return isNil
			}
			if g.runtimeAllocation {
				left := g.materializeNilInterface(x)
				right := g.materializeNilInterface(y)
				equalFunction := "runtime.nilinterequal"
				if interfaceHasMethods(t) {
					equalFunction = "runtime.interequal"
				}
				equal := g.cur.Call(ir.ClsW, g.fn.Sym(equalFunction, 0), left, right)
				if op == token.NEQ {
					return g.cur.Cmp(ir.CmpEq, ir.ClsW, equal, g.fn.Word(0))
				}
				return equal
			}
		}
	}
	if isMemoryValue(t) && (op == token.EQL || op == token.NEQ) {
		equal := g.memoryValuesEqual(x, y, t)
		if op == token.NEQ {
			return g.cur.Cmp(ir.CmpEq, ir.ClsW, equal, g.fn.Word(0))
		}
		return equal
	}
	if op == token.EQL || op == token.NEQ {
		if _, ok := t.Underlying().(*types.Slice); ok {
			x, _, _ = g.sliceParts(x)
			predicate := ir.CmpEq
			if op == token.NEQ {
				predicate = ir.CmpNe
			}
			return g.cur.Cmp(predicate, ir.ClsP, x, y)
		}
	}
	if basic, ok := t.Underlying().(*types.Basic); ok && basic.Kind() == types.String && (op == token.EQL || op == token.NEQ) {
		equal := g.stringsEqual(x, y)
		if op == token.NEQ {
			return g.cur.Cmp(ir.CmpEq, ir.ClsW, equal, g.fn.Word(0))
		}
		return equal
	}
	if basic, ok := t.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
		switch op {
		case token.LSS, token.LEQ, token.GTR, token.GEQ:
			return g.stringsOrdered(x, y, op)
		}
	}
	switch op {
	case token.ADD:
		return g.cur.Add(c, x, y)
	case token.SUB:
		return g.cur.Sub(c, x, y)
	case token.MUL:
		return g.cur.Mul(c, x, y)
	case token.QUO:
		if signed(t) {
			return g.cur.Div(c, x, y)
		}
		return g.cur.UDiv(c, x, y)
	case token.REM:
		if signed(t) {
			return g.cur.Rem(c, x, y)
		}
		return g.cur.URem(c, x, y)
	case token.AND:
		return g.cur.And(c, x, y)
	case token.AND_NOT:
		inverted := g.cur.Xor(c, y, g.fn.ConstInt(c, -1))
		return g.cur.And(c, x, inverted)
	case token.OR:
		return g.cur.Or(c, x, y)
	case token.XOR:
		return g.cur.Xor(c, x, y)
	case token.SHL:
		return g.shift(op, c, x, y, t, n)
	case token.SHR:
		return g.shift(op, c, x, y, t, n)
	}
	pred := ir.CmpEq
	if c.IsFloat() {
		switch op {
		case token.EQL:
			pred = ir.CmpFeq
		case token.NEQ:
			pred = ir.CmpFne
		case token.LSS:
			pred = ir.CmpFlt
		case token.LEQ:
			pred = ir.CmpFle
		case token.GTR:
			pred = ir.CmpFgt
		case token.GEQ:
			pred = ir.CmpFge
		default:
			g.fail(n, "unsupported operator %s", op)
		}
		return g.cur.Cmp(pred, ir.ClsW, x, y)
	}
	switch op {
	case token.EQL:
		pred = ir.CmpEq
	case token.NEQ:
		pred = ir.CmpNe
	case token.LSS:
		if signed(t) {
			pred = ir.CmpSlt
		} else {
			pred = ir.CmpUlt
		}
	case token.LEQ:
		if signed(t) {
			pred = ir.CmpSle
		} else {
			pred = ir.CmpUle
		}
	case token.GTR:
		if signed(t) {
			pred = ir.CmpSgt
		} else {
			pred = ir.CmpUgt
		}
	case token.GEQ:
		if signed(t) {
			pred = ir.CmpSge
		} else {
			pred = ir.CmpUge
		}
	default:
		g.fail(n, "unsupported operator %s", op)
	}
	return g.cur.Cmp(pred, ir.ClsW, x, y)
}

func (g *gen) concatStrings(left, right ir.Ref) ir.Ref {
	stringType := types.Typ[types.String]
	bufferType := types.NewPointer(types.NewArray(types.Typ[types.Uint8], 32))
	signature := types.NewSignatureType(
		nil,
		nil,
		nil,
		types.NewTuple(
			types.NewParam(token.NoPos, nil, "buf", bufferType),
			types.NewParam(token.NoPos, nil, "left", stringType),
			types.NewParam(token.NoPos, nil, "right", stringType),
		),
		types.NewTuple(types.NewParam(token.NoPos, nil, "result", stringType)),
		false,
	)
	return g.callWithSignature(
		ir.ClsP,
		g.fn.Sym("runtime.concatstring2", 0),
		[]ir.Ref{g.fn.ConstInt(ir.ClsP, 0), left, right},
		signature,
		nil,
	)
}

func (g *gen) memoryValuesEqual(left, right ir.Ref, valueType types.Type) ir.Ref {
	equal := g.fn.Word(1)
	compare := func(leftAddress, rightAddress ir.Ref, fieldType types.Type) {
		var fieldEqual ir.Ref
		if isMemoryValue(fieldType) {
			fieldEqual = g.memoryValuesEqual(leftAddress, rightAddress, fieldType)
		} else if basic, ok := fieldType.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			fieldEqual = g.stringsEqual(leftAddress, rightAddress)
		} else {
			leftValue := g.load(leftAddress, fieldType)
			rightValue := g.load(rightAddress, fieldType)
			class, _ := scalar(fieldType)
			predicate := ir.CmpEq
			if class.IsFloat() {
				predicate = ir.CmpFeq
			}
			fieldEqual = g.cur.Cmp(predicate, ir.ClsW, leftValue, rightValue)
		}
		equal = g.cur.And(ir.ClsW, equal, fieldEqual)
	}

	switch value := valueType.Underlying().(type) {
	case *types.Array:
		elementSize := typeSize(value.Elem())
		for index := int64(0); index < value.Len(); index++ {
			offset := index * elementSize
			compare(g.offset(left, offset), g.offset(right, offset), value.Elem())
		}
	case *types.Struct:
		fields := structFields(value)
		offsets := structOffsets(fields)
		for index, field := range fields {
			compare(g.offset(left, offsets[index]), g.offset(right, offsets[index]), field.Type())
		}
	}
	return equal
}

func (g *gen) shift(op token.Token, class ir.Cls, value, count ir.Ref, valueType types.Type, node ast.Node) ir.Ref {
	countClass := ir.ClsL
	if expression, ok := node.(*ast.BinaryExpr); ok {
		countClass, _ = scalar(g.info.Types[expression.Y].Type)
	}
	if countClass == ir.ClsW {
		count = g.cur.Extuw(ir.ClsL, count)
	}

	width := int64(64)
	if class == ir.ClsW {
		width = 32
	}
	tooLarge := g.cur.Cmp(ir.CmpUge, ir.ClsL, count, g.fn.ConstInt(ir.ClsL, width))

	var shifted ir.Ref
	var overflow ir.Ref
	switch {
	case op == token.SHL:
		shifted = g.cur.Shl(class, value, count)
		overflow = g.fn.ConstInt(class, 0)
	case signed(valueType):
		shifted = g.cur.Sar(class, value, count)
		overflow = g.cur.Sar(class, value, g.fn.ConstInt(ir.ClsL, width-1))
	default:
		shifted = g.cur.Shr(class, value, count)
		overflow = g.fn.ConstInt(class, 0)
	}
	return g.selectValue(tooLarge, overflow, shifted, class)
}

func (g *gen) stringsEqual(left, right ir.Ref) ir.Ref {
	result := g.localAlloc(4, 4)
	sameDescriptor := g.cur.Cmp(ir.CmpEq, ir.ClsP, left, right)
	same := g.block("stringeq_same")
	different := g.block("stringeq_different")
	compare := g.block("stringeq_compare")
	compareWithZero := g.block("stringeq_zero")
	done := g.block("stringeq_done")
	g.cur.Jnz(sameDescriptor, same, different)

	g.cur = same
	g.cur.Store(g.fn.Word(1), result)
	g.cur.Goto(done)

	g.cur = different
	leftNonNil := g.cur.Cmp(ir.CmpNe, ir.ClsP, left, g.fn.ConstInt(ir.ClsP, 0))
	rightNonNil := g.cur.Cmp(ir.CmpNe, ir.ClsP, right, g.fn.ConstInt(ir.ClsP, 0))
	bothNonNil := g.cur.And(ir.ClsW, leftNonNil, rightNonNil)
	g.cur.Jnz(bothNonNil, compare, compareWithZero)

	g.cur = compare
	leftLength := g.cur.Load(ir.ClsL, g.offset(left, 8))
	rightLength := g.cur.Load(ir.ClsL, g.offset(right, 8))
	lengthsEqual := g.cur.Cmp(ir.CmpEq, ir.ClsL, leftLength, rightLength)
	leftIsShorter := g.cur.Cmp(ir.CmpUle, ir.ClsL, leftLength, rightLength)
	compareLength := g.selectValue(leftIsShorter, leftLength, rightLength, ir.ClsL)
	leftData := g.cur.Load(ir.ClsP, left)
	rightData := g.cur.Load(ir.ClsP, right)
	bytes := g.cur.Call(ir.ClsW, g.fn.Sym("goc_memcmp", 0), leftData, rightData, compareLength)
	bytesEqual := g.cur.Cmp(ir.CmpEq, ir.ClsW, bytes, g.fn.Word(0))
	g.cur.Store(g.cur.And(ir.ClsW, lengthsEqual, bytesEqual), result)
	g.cur.Goto(done)

	g.cur = compareWithZero
	nonNil := g.selectValue(leftNonNil, left, right, ir.ClsP)
	length := g.cur.Load(ir.ClsL, g.offset(nonNil, 8))
	g.cur.Store(g.cur.Cmp(ir.CmpEq, ir.ClsL, length, g.fn.Long(0)), result)
	g.cur.Goto(done)

	g.cur = done
	return g.cur.Load(ir.ClsW, result)
}

func (g *gen) stringsOrdered(left, right ir.Ref, operator token.Token) ir.Ref {
	leftData, leftLength := g.stringComparisonParts(left)
	rightData, rightLength := g.stringComparisonParts(right)
	leftIsShorter := g.cur.Cmp(ir.CmpUlt, ir.ClsL, leftLength, rightLength)
	compareLength := g.selectValue(leftIsShorter, leftLength, rightLength, ir.ClsL)
	bytes := g.cur.Call(ir.ClsW, g.fn.Sym("goc_memcmp", 0), leftData, rightData, compareLength)
	bytesEqual := g.cur.Cmp(ir.CmpEq, ir.ClsW, bytes, g.fn.Word(0))

	var bytesOrdered ir.Ref
	var lengthsOrdered ir.Ref
	switch operator {
	case token.LSS:
		bytesOrdered = g.cur.Cmp(ir.CmpSlt, ir.ClsW, bytes, g.fn.Word(0))
		lengthsOrdered = g.cur.Cmp(ir.CmpUlt, ir.ClsL, leftLength, rightLength)
	case token.LEQ:
		bytesOrdered = g.cur.Cmp(ir.CmpSlt, ir.ClsW, bytes, g.fn.Word(0))
		lengthsOrdered = g.cur.Cmp(ir.CmpUle, ir.ClsL, leftLength, rightLength)
	case token.GTR:
		bytesOrdered = g.cur.Cmp(ir.CmpSgt, ir.ClsW, bytes, g.fn.Word(0))
		lengthsOrdered = g.cur.Cmp(ir.CmpUgt, ir.ClsL, leftLength, rightLength)
	case token.GEQ:
		bytesOrdered = g.cur.Cmp(ir.CmpSgt, ir.ClsW, bytes, g.fn.Word(0))
		lengthsOrdered = g.cur.Cmp(ir.CmpUge, ir.ClsL, leftLength, rightLength)
	default:
		panic("stringsOrdered called with a non-ordering operator")
	}

	equalPrefixOrdered := g.cur.And(ir.ClsW, bytesEqual, lengthsOrdered)
	return g.cur.Or(ir.ClsW, bytesOrdered, equalPrefixOrdered)
}

func (g *gen) stringComparisonParts(descriptor ir.Ref) (ir.Ref, ir.Ref) {
	dataAddress := g.localAlloc(8, 8)
	g.markStackPointerWord(dataAddress, 0)
	lengthAddress := g.localAlloc(8, 8)
	nilString := g.block("stringcompare_nil")
	nonNilString := g.block("stringcompare_non_nil")
	done := g.block("stringcompare_parts_done")
	descriptorIsNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, descriptor, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Jnz(descriptorIsNil, nilString, nonNilString)

	g.cur = nilString
	g.cur.Store(g.fn.ConstInt(ir.ClsP, 0), dataAddress)
	g.cur.Store(g.fn.Long(0), lengthAddress)
	g.cur.Goto(done)

	g.cur = nonNilString
	g.cur.Store(g.cur.Load(ir.ClsP, descriptor), dataAddress)
	g.cur.Store(g.cur.Load(ir.ClsL, g.offset(descriptor, 8)), lengthAddress)
	g.cur.Goto(done)

	g.cur = done
	return g.cur.Load(ir.ClsP, dataAddress), g.cur.Load(ir.ClsL, lengthAddress)
}

// OutputName returns the conventional output stem for a source file.
func OutputName(name string) string {
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	return filepath.Base(stem)
}
