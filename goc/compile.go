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
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// Compile parses and type-checks one Go source file and lowers it to cg12 IR.
func Compile(name string, src []byte) (*ir.Module, error) {
	return compile(name, src, false)
}

// CompileExecutable lowers a main package together with the Go runtime needed
// to start and run it as a normal Go executable.
func CompileExecutable(name string, src []byte) (*ir.Module, error) {
	return compile(name, src, true)
}

func compile(name string, src []byte, executable bool) (*ir.Module, error) {
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
	}
	loader := newSourceLoader(fset)
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
	linkNames := make(map[*types.Func]string)
	interfaceMethods := make(map[*types.Func]bool)
	collectLinkNames([]*ast.File{file}, info, linkNames)
	for _, unit := range loader.units {
		collectLinkNames(unit.files, unit.info, linkNames)
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
	functions := reachableFunctions(roots, info, pkg, loader.units, compileRuntime, moduleInitFunctions, linkNames)
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
		for _, assembly := range unit.assembly {
			mod.Assembly = append(mod.Assembly, ir.AssemblyFile{
				PackagePath: path,
				Path:        assembly.path,
				Source:      assembly.source,
			})
		}
	}
	g := &gen{fset: fset, file: file, info: info, pkg: pkg, mod: mod, globals: map[types.Object]string{}, emitRuntimeTables: emitRuntimeTables, runtimeAllocation: compileRuntime, typeTags: typeTags, linkNames: linkNames, initSymbols: initSymbols, functionDecls: functionDecls, noWriteBarrierFunctions: noWriteBarriers, interfaceMethods: interfaceMethods}
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
		generator := &gen{fset: fset, info: unit.info, pkg: unit.pkg, mod: mod, globals: globals, emitRuntimeTables: emitRuntimeTables, runtimeAllocation: compileRuntime, typeTags: typeTags, linkNames: linkNames, initSymbols: initSymbols, functionDecls: functionDecls, noWriteBarrierFunctions: noWriteBarriers, interfaceMethods: interfaceMethods}
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
	g.dynamicInitializers = dynamicInitializers
	g.dynamicInitializerGuards = dynamicInitializerGuards
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
	for i := len(functions) - 1; i >= 0; i-- {
		function := functions[i]
		generator := g
		if function.pkg != pkg {
			generator = &gen{
				fset:                     fset,
				info:                     function.info,
				pkg:                      function.pkg,
				mod:                      mod,
				globals:                  packageGlobals[function.pkg.Path()],
				methodTargets:            methodTargets,
				emitRuntimeTables:        emitRuntimeTables,
				runtimeAllocation:        compileRuntime,
				typeTags:                 typeTags,
				linkNames:                linkNames,
				initSymbols:              initSymbols,
				functionDecls:            functionDecls,
				noWriteBarrierFunctions:  noWriteBarriers,
				interfaceMethods:         interfaceMethods,
				dynamicInitializers:      dynamicInitializers,
				dynamicInitializerGuards: dynamicInitializerGuards,
				initializingGlobals:      make(map[types.Object]bool),
			}
		}
		generator.funcDecl(function.decl)
		if generator.err != nil {
			return nil, generator.err
		}
	}
	addInterfaceMethodWrappers(g, functions)
	if loader.units["crypto/internal/fips140"] != nil {
		addFIPSRuntimeStubs(mod)
	}
	if loader.units["crypto/sha1"] != nil || loader.units["crypto/md5"] != nil {
		addLegacyCryptoRuntimeStubs(mod)
	}
	if g.err != nil {
		return nil, g.err
	}
	if compileRuntime {
		if err := addRuntimeInitTask(mod, runtimeInits, initSymbols); err != nil {
			return nil, err
		}
		if err := addModuleInitTasks(mod, moduleInits, initSymbols); err != nil {
			return nil, err
		}
		mod.Data = append(mod.Data, &ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}})
	}
	addMemoryHelpers(mod)
	if compileRuntime {
		for _, function := range mod.Funcs {
			function.GoABI = true
		}
	}
	opt.LowerHeapAllocations(mod)
	return g.mod, nil
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
		receiver := function.Param("receiver", ir.ClsP)
		function.Temp(receiver).Agg = g.goABIAggregate(signature.Recv().Type())
		parameters := make([]ir.Ref, signature.Params().Len())
		for index := range parameters {
			parameterType := signature.Params().At(index).Type()
			parameterClass, supported := scalar(parameterType)
			if !supported {
				g.err = fmt.Errorf("goc: interface method %s has unsupported parameter %s", name, parameterType)
				return
			}
			parameters[index] = function.Param(signature.Params().At(index).Name(), parameterClass)
			function.Temp(parameters[index]).Agg = g.goABIAggregate(parameterType)
		}
		var resultPointers []ir.Ref
		if signature.Results().Len() > 0 && isInlineAggregate(signature.Results().At(0).Type()) {
			resultPointers = append(resultPointers, function.Param("result0", ir.ClsP))
		}
		for index := 1; index < signature.Results().Len(); index++ {
			resultPointers = append(resultPointers, function.Param(fmt.Sprintf("result%d", index), ir.ClsP))
		}

		candidates := interfaceMethodCandidates(g, reachable, method, interfaceType)
		wrapper := &gen{
			fn:                function,
			cur:               function.Entry(),
			mod:               g.mod,
			typeTags:          g.typeTags,
			linkNames:         g.linkNames,
			initSymbols:       g.initSymbols,
			runtimeAllocation: g.runtimeAllocation,
		}
		dynamicTag := wrapper.cur.Load(ir.ClsP, receiver)
		for index, candidate := range candidates {
			candidateSignature := candidate.Type().(*types.Signature)
			receiverType := candidateSignature.Recv().Type()
			tagName := g.typeTags[goTypeKey(receiverType)]
			if tagName == "" {
				continue
			}
			invoke := function.NewBlock(fmt.Sprintf("invoke%d", index))
			next := function.NewBlock(fmt.Sprintf("next%d", index))
			matches := wrapper.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, function.Sym(tagName, 0))
			wrapper.cur.Jnz(matches, invoke, next)

			wrapper.cur = invoke
			arguments := make([]ir.Ref, 0, 1+len(parameters)+len(resultPointers))
			arguments = append(arguments, wrapper.interfaceMethodReceiver(receiver, candidate))
			arguments = append(arguments, parameters...)
			arguments = append(arguments, resultPointers...)
			callee := function.Sym(g.functionSymbol(candidate), 0)
			if signature.Results().Len() == 0 {
				wrapper.callVoidWithSignature(callee, arguments, candidateSignature, receiverType)
				wrapper.cur.RetVoid()
			} else {
				resultClass, _ := scalar(signature.Results().At(0).Type())
				result := wrapper.callWithSignature(resultClass, callee, arguments, candidateSignature, receiverType)
				wrapper.cur.Ret(result)
			}
			wrapper.cur = next
		}
		wrapper.cur.CallVoid(function.Sym("abort", 0))
		wrapper.cur.Hlt()
	}
}

func interfaceMethodCandidates(g *gen, reachable []functionDecl, method *types.Func, interfaceType *types.Interface) []*types.Func {
	seen := make(map[*types.Func]bool)
	var candidates []*types.Func
	for _, declaration := range reachable {
		candidate, ok := declaration.info.Defs[declaration.decl.Name].(*types.Func)
		if !ok || seen[candidate] || candidate.Name() != method.Name() {
			continue
		}
		signature, ok := candidate.Type().(*types.Signature)
		if !ok || signature.Recv() == nil || !types.Implements(signature.Recv().Type(), interfaceType) {
			continue
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return g.functionSymbol(candidates[i]) < g.functionSymbol(candidates[j])
	})
	return candidates
}

func goTypeKey(valueType types.Type) string {
	return types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Path()
	})
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
		if data.Name != "runtime.runtime_inittasks.descriptor" {
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
func addMemoryHelpers(mod *ir.Module) {
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
	fset                     *token.FileSet
	file                     *ast.File
	info                     *types.Info
	pkg                      *types.Package
	mod                      *ir.Module
	fn                       *ir.Func
	cur                      *ir.Block
	vars                     map[types.Object]ir.Ref
	globals                  map[types.Object]string
	breaks, continues        []*ir.Block
	seq                      int
	err                      error
	methodTargets            map[string]*types.Func
	resultSlot               ir.Ref
	resultType               types.Type
	aggregateResult          ir.Ref
	extraResultSlots         []ir.Ref
	extraResultTypes         []types.Type
	labels                   map[string]*ir.Block
	labeledBreaks            map[string]*ir.Block
	labeledContinues         map[string]*ir.Block
	deferSlots               map[*ast.DeferStmt]ir.Ref
	deferOrder               []*ast.DeferStmt
	deferActions             []*ast.DeferStmt
	runningDefers            bool
	parents                  map[ast.Node]ast.Node
	currentBody              *ast.BlockStmt
	functionDecls            map[*types.Func]functionDecl
	noWriteBarrierFunctions  map[*types.Func]bool
	interfaceMethods         map[*types.Func]bool
	directValues             map[types.Object]bool
	emitRuntimeTables        bool
	runtimeAllocation        bool
	typeTags                 map[string]string
	linkNames                map[*types.Func]string
	initSymbols              map[*types.Func]string
	stackAddresses           map[uint32]bool
	heapCaptures             map[types.Object]ir.Ref
	noWriteBarrier           bool
	dynamicInitializers      map[types.Object]globalInitializer
	dynamicInitializerGuards map[types.Object]string
	initializingGlobals      map[types.Object]bool
}

type globalInitializer struct {
	expression ast.Expr
	info       *types.Info
	pkg        *types.Package
	globals    map[types.Object]string
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

func collectFunctionDeclarations(rootInfo *types.Info, rootPkg *types.Package, rootFiles []*ast.File, units map[string]*sourceUnit) map[*types.Func]functionDecl {
	declarations := make(map[*types.Func]functionDecl)
	collect := func(files []*ast.File, info *types.Info, pkg *types.Package) {
		for _, file := range files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Body == nil {
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
						if object == nil || globals[object] == "" || info.Types[expression].Value != nil || staticallyInitialized(expression) {
							continue
						}
						initializers[object] = globalInitializer{
							expression: expression,
							info:       info,
							pkg:        pkg,
							globals:    globals,
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

func staticallyInitialized(expression ast.Expr) bool {
	if _, literal := expression.(*ast.CompositeLit); literal {
		return true
	}
	address, ok := expression.(*ast.UnaryExpr)
	if !ok || address.Op != token.AND {
		return false
	}
	_, literal := address.X.(*ast.CompositeLit)
	return literal
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

func (g *gen) fixedSliceMakeCapacity(call *ast.CallExpr) (int64, bool) {
	capacityExpression := call.Args[1]
	if len(call.Args) == 3 {
		capacityExpression = call.Args[2]
	}
	value := g.info.Types[capacityExpression].Value
	if value == nil {
		return 0, false
	}
	capacity, ok := constant.Int64Val(constant.ToInt(value))
	if !ok || capacity < 0 || !g.makeResultDoesNotEscape(call) {
		return 0, false
	}
	return capacity, true
}

func (g *gen) makeResultDoesNotEscape(call *ast.CallExpr) bool {
	assignment, ok := g.parents[call].(*ast.AssignStmt)
	if !ok {
		return false
	}
	index := -1
	for candidate, expression := range assignment.Rhs {
		if expression == call {
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
	object := g.info.Defs[identifier]
	if object == nil {
		object = g.info.Uses[identifier]
	}
	if object == nil || object.Pkg() == nil || g.currentBody == nil {
		return false
	}
	return g.objectDoesNotEscape(object, g.info, g.parents, g.currentBody, make(map[parameterKey]bool))
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
		identifier, ok := node.(*ast.Ident)
		if !ok || info.Uses[identifier] != object {
			return true
		}
		if !g.nonEscapingObjectUse(identifier, info, parents, checking) {
			escaped = true
		}
		return true
	})
	return !escaped
}

func (g *gen) nonEscapingObjectUse(identifier *ast.Ident, info *types.Info, parents map[ast.Node]ast.Node, checking map[parameterKey]bool) bool {
	parent := parents[identifier]
	switch parent := parent.(type) {
	case *ast.IndexExpr:
		return parent.X == identifier
	case *ast.SliceExpr:
		return parent.X == identifier
	case *ast.RangeStmt:
		return parent.X == identifier
	case *ast.BinaryExpr:
		return parent.Op == token.EQL || parent.Op == token.NEQ
	case *ast.CallExpr:
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
				g.markDataPointerCell(g.pkg.Path() + "." + id.Name)
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
				g.markDataPointerCell(name)
				g.markDataPointerWords(name+".descriptor", obj.Type())
				continue
			}
			cls, ok := scalar(obj.Type())
			if !ok {
				continue
			}
			name := g.pkg.Path() + "." + id.Name
			d := &ir.Data{Name: name, Align: 8, Linkage: ir.Linkage{Export: ast.IsExported(id.Name)}}
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
							if pointerOK && structOK {
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
								d.Items = []ir.DataItem{{Sub: ir.SubL, Sym: targetObject.Pkg().Path() + "." + targetObject.Name()}}
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
		g.mod.Data = append(g.mod.Data, &ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}})
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
	payloadName := name + ".payload"
	descriptorName := name + ".descriptor"
	tagName := g.ensureTypeTag(sourceType)
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: textName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}},
		&ir.Data{Name: stringName, Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Sym: textName},
			{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
		}},
		&ir.Data{Name: payloadName, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Sym: stringName}}},
		&ir.Data{Name: descriptorName, Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Sym: tagName},
			{Sub: ir.SubL, Sym: payloadName},
		}},
		&ir.Data{Name: name, Align: 8, Linkage: ir.Linkage{Export: ast.IsExported(id.Name)}, Items: []ir.DataItem{{Sub: ir.SubL, Sym: descriptorName}}},
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
	emitZero := func() {
		descriptorName := name + ".descriptor"
		g.mod.Data = append(g.mod.Data,
			&ir.Data{Name: descriptorName, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0, 0, 0}}}},
			&ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Sym: descriptorName}}},
		)
		g.globals[object] = name
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
			g.mod.Data = append(g.mod.Data,
				&ir.Data{Name: backingName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}},
				&ir.Data{Name: name + ".descriptor", Align: 8, Items: []ir.DataItem{
					{Sub: ir.SubL, Sym: backingName},
					{Sub: ir.SubL, Ints: []int64{int64(len(contents)), int64(len(contents))}},
				}},
				&ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Sym: name + ".descriptor"}}},
			)
			g.globals[object] = name
			return
		}
	}
	literal, ok := initializer.(*ast.CompositeLit)
	if !ok {
		emitZero()
		return
	}
	backingName := name + ".backing"
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
			g.mod.Data = append(g.mod.Data,
				&ir.Data{Name: backingName, Align: 8, Items: items, PointerWords: pointerWordIndices(types.NewArray(slice.Elem(), int64(len(literal.Elts))))},
				&ir.Data{Name: name + ".descriptor", Align: 8, Items: []ir.DataItem{
					{Sub: ir.SubL, Sym: backingName},
					{Sub: ir.SubL, Ints: []int64{int64(len(literal.Elts)), int64(len(literal.Elts))}},
				}},
				&ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Sym: name + ".descriptor"}}},
			)
			g.globals[object] = name
			return
		}
	}
	items := make([]ir.DataItem, 0, len(literal.Elts))
	for _, expression := range literal.Elts {
		value := g.info.Types[expression].Value
		if value == nil || value.Kind() != constant.String {
			items = append(items, ir.DataItem{Sub: ir.SubL, Ints: []int64{0}})
			continue
		}
		contents := constant.StringVal(value)
		textName := fmt.Sprintf(".goc.global.string.%d", len(g.mod.Data))
		descriptorName := textName + ".descriptor"
		g.mod.Data = append(g.mod.Data,
			&ir.Data{Name: textName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}},
			&ir.Data{Name: descriptorName, Align: 8, Items: []ir.DataItem{
				{Sub: ir.SubL, Sym: textName},
				{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
			}},
		)
		items = append(items, ir.DataItem{Sub: ir.SubL, Sym: descriptorName})
	}
	var backingPointerWords []int
	if _, stringElements := slice.Elem().Underlying().(*types.Basic); !stringElements {
		items = []ir.DataItem{{Zero: int(typeSize(slice.Elem())) * len(literal.Elts)}}
		backingPointerWords = pointerWordIndices(types.NewArray(slice.Elem(), int64(len(literal.Elts))))
	}
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: backingName, Align: 8, Items: items, PointerWords: backingPointerWords},
		&ir.Data{Name: name + ".descriptor", Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Sym: backingName},
			{Sub: ir.SubL, Ints: []int64{int64(len(literal.Elts)), int64(len(literal.Elts))}},
		}},
		&ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Sym: name + ".descriptor"}}},
	)
	g.globals[object] = name
}

func (g *gen) staticStructItems(name string, structure *types.Struct, literal *ast.CompositeLit) []ir.DataItem {
	offsets := structOffsets(structFields(structure))
	items := make([]ir.DataItem, 0, structure.NumFields()*2)
	cursor := int64(0)
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
		offset := offsets[fieldIndex]
		if offset > cursor {
			items = append(items, ir.DataItem{Zero: int(offset - cursor)})
		}
		fieldType := structure.Field(fieldIndex).Type()
		fieldSize := typeSize(fieldType)
		if value := g.info.Types[expression].Value; value != nil {
			if value.Kind() == constant.String {
				text := name + ".string." + structure.Field(fieldIndex).Name()
				contents := constant.StringVal(value)
				g.mod.Data = append(g.mod.Data, &ir.Data{Name: text, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Str: contents}}})
				items = append(items, ir.DataItem{Sub: ir.SubL, Sym: text}, ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(len(contents))}})
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
			} else {
				items = append(items, ir.DataItem{Zero: int(fieldSize)})
			}
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

func (g *gen) staticFunctionLiteral(literal *ast.FuncLit) string {
	temporaryName := fmt.Sprintf(".goc.global.literal.%d", len(g.mod.Funcs))
	temporary := g.mod.NewFuncVoid(temporaryName)

	savedFunction := g.fn
	savedBlock := g.cur
	savedVariables := g.vars
	savedDirectValues := g.directValues
	savedStackAddresses := g.stackAddresses
	savedHeapCaptures := g.heapCaptures
	savedParents := g.parents
	savedBody := g.currentBody
	savedSequence := g.seq

	g.fn = temporary
	g.cur = temporary.Entry()
	g.vars = make(map[types.Object]ir.Ref)
	g.directValues = make(map[types.Object]bool)
	g.stackAddresses = make(map[uint32]bool)
	g.heapCaptures = make(map[types.Object]ir.Ref)
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
	if valueIndex < len(spec.Values) {
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
		if cls == ir.ClsL {
			sub = ir.SubL
		} else {
			sub = ir.SubW
		}
	}
	values := make([]int64, array.Len())
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
					return
				}
				values[index] = constInt(value)
			}
		}
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: int(typeSize(element)),
		Items: []ir.DataItem{{Sub: sub, Ints: values}},
	})
	g.globals[object] = name
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
	return aggregate
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
	if signature.Results().Len() == 1 {
		instruction.RetAgg = g.goABIAggregate(signature.Results().At(0).Type())
	}
}

func (g *gen) materializeNilInterface(value ir.Ref) ir.Ref {
	zeroDescriptor := g.localAlloc(8, 16)
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

func (g *gen) callWithSignature(resultClass ir.Cls, callee ir.Ref, arguments []ir.Ref, signature *types.Signature, receiverType types.Type) ir.Ref {
	arguments = g.normalizeCallInterfaces(arguments, signature, receiverType)
	result := g.cur.Call(resultClass, callee, arguments...)
	instruction := &g.cur.Instrs[len(g.cur.Instrs)-1]
	g.annotateABICall(instruction, signature, receiverType)
	return result
}

func (g *gen) callVoidWithSignature(callee ir.Ref, arguments []ir.Ref, signature *types.Signature, receiverType types.Type) {
	arguments = g.normalizeCallInterfaces(arguments, signature, receiverType)
	g.cur.CallVoid(callee, arguments...)
	instruction := &g.cur.Instrs[len(g.cur.Instrs)-1]
	g.annotateABICall(instruction, signature, receiverType)
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
	case types.Int32:
		return ir.SubW, true
	}
	return 0, false
}

func (g *gen) alloc(t types.Type) ir.Ref {
	c, _ := scalar(t)
	if c == ir.ClsL || c == ir.ClsP || c == ir.ClsD {
		return g.localAlloc(8, 8)
	}
	return g.localAlloc(4, 4)
}
func (g *gen) load(addr ir.Ref, t types.Type) ir.Ref {
	c, _ := scalar(t)
	if sub, ok := subOf(t); ok {
		return g.cur.LoadSub(c, sub, addr)
	}
	return g.cur.Load(c, addr)
}
func (g *gen) store(v, addr ir.Ref, t types.Type) {
	if sub, ok := subOf(t); ok {
		g.cur.StoreSub(sub, v, addr)
		return
	}
	class, _ := scalar(t)
	if g.runtimeAllocation && !g.noWriteBarrier && class == ir.ClsP && !g.isStackAddress(addr) {
		g.cur.CallVoid(g.fn.Sym("runtime.atomicstorep", 0), addr, v)
		return
	}
	g.cur.Store(v, addr)
}

// assignLocal stores a Go value into a frontend variable slot. Struct and
// array variables use an indirect slot so their address remains stable across
// assignments; assigning one of these values must copy into that backing
// storage rather than replace the slot with an alias of the source value.
func (g *gen) assignLocal(value, slot ir.Ref, valueType types.Type) {
	if isMemoryValue(valueType) {
		destination := g.load(slot, valueType)
		g.cur.Call(
			ir.ClsP,
			g.fn.Sym("goc_memcpy", 0),
			destination,
			value,
			g.fn.Long(typeSize(valueType)),
		)
		return
	}
	g.store(value, slot, valueType)
}

func (g *gen) localAlloc(align, size int) ir.Ref {
	address := g.cur.Alloc(align, size)
	if g.stackAddresses == nil {
		g.stackAddresses = make(map[uint32]bool)
	}
	g.stackAddresses[address.ID] = true
	return address
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
	fc, _ := scalar(from)
	tc, _ := scalar(to)
	if fc == ir.ClsW && tc == ir.ClsL {
		if signed(from) {
			v = g.cur.Extsw(ir.ClsL, v)
		} else {
			v = g.cur.Extuw(ir.ClsL, v)
		}
	} else if fc != tc {
		v = g.cur.Copy(tc, v)
	}
	return g.coerce(v, to)
}

func (g *gen) assignmentValue(expression ast.Expr, targetType types.Type) ir.Ref {
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "nil" && isDescriptorValue(targetType) {
		return g.zeroValue(targetType)
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
	sourceType := g.info.Types[expression].Type
	if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
		return g.expr(expression)
	}
	value := g.expr(expression)
	if isDirectInterfaceType(sourceType) {
		descriptor := g.localAlloc(8, 16)
		g.cur.Store(g.typeTag(sourceType), descriptor)
		g.cur.Store(value, g.offset(descriptor, 8))
		return descriptor
	}
	payload := g.allocLocal(sourceType)
	g.store(value, payload, sourceType)
	descriptor := g.localAlloc(8, 16)
	g.cur.Store(g.typeTag(sourceType), descriptor)
	g.cur.Store(payload, g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) callArguments(arguments []ast.Expr, hasEllipsis bool, signature *types.Signature) []ir.Ref {
	if !signature.Variadic() {
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
	if g.runtimeAllocation {
		backing = g.allocateTyped(arrayType)
	} else {
		backing = g.localAlloc(int(typeAlign(arrayType)), int(typeSize(arrayType)))
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memset", 0), backing, g.fn.Word(0), g.fn.Long(typeSize(arrayType)))
	}
	elementSize := typeSize(elementType)
	for index, argument := range variadicArguments {
		value := g.assignmentValue(argument, elementType)
		g.store(value, g.offset(backing, int64(index)*elementSize), elementType)
	}
	values = append(values, g.sliceDescriptor(backing, g.fn.Long(length), g.fn.Long(length)))
	return values
}

func (g *gen) initializeGlobal(object types.Object) {
	initializer, exists := g.dynamicInitializers[object]
	if !exists || g.initializingGlobals[object] || !g.live() {
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
	savedGlobals := g.globals
	savedParents := g.parents
	savedBody := g.currentBody
	g.info = initializer.info
	g.pkg = initializer.pkg
	g.globals = initializer.globals
	g.parents = astParents(initializer.expression)
	g.currentBody = nil
	g.initializingGlobals[object] = true

	value := g.assignmentValue(initializer.expression, object.Type())
	destination := g.fn.Sym(initializer.globals[object], 0)
	if isMemoryValue(object.Type()) {
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), destination, value, g.fn.Long(typeSize(object.Type())))
	} else {
		g.store(value, destination, object.Type())
	}

	delete(g.initializingGlobals, object)
	g.info = savedInfo
	g.pkg = savedPackage
	g.globals = savedGlobals
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
	key := types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Path()
	})
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
		if isDirectInterfaceType(valueType) {
			tflag = 1 << 5
		}
		items := []ir.DataItem{
			{Sub: ir.SubL, Ints: []int64{typeSize(valueType), runtimePointerBytes(valueType)}},
			{Sub: ir.SubW, Ints: []int64{0}},
			{Sub: ir.SubUB, Ints: []int64{tflag, alignment, alignment, int64(runtimeKind(valueType))}},
			{Sub: ir.SubL, Ints: []int64{0}},
			{Sub: ir.SubL, Sym: gcDataName},
			{Sub: ir.SubW, Ints: []int64{0, 0}},
		}
		switch value := valueType.Underlying().(type) {
		case *types.Pointer:
			items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Elem())})
		case *types.Slice:
			items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Elem())})
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
				items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Params().At(index).Type())})
			}
			for index := 0; index < value.Results().Len(); index++ {
				items = append(items, ir.DataItem{Sub: ir.SubL, Sym: g.ensureTypeTag(value.Results().At(index).Type())})
			}
		}
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name: gcDataName, Align: 1, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: mask}},
		}, &ir.Data{Name: name, Align: 8, Items: items})
	}
	return name
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
	sig := obj.Type().(*types.Signature)
	isMain := g.pkg.Name() == "main" && obj.Name() == "main"
	platformMain := isMain && !g.runtimeAllocation
	name := g.functionSymbol(obj)
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
	if sig.Results().Len() == 1 {
		resultAggregate = g.goABIAggregate(sig.Results().At(0).Type())
		g.fn.RetAgg = resultAggregate
	}
	g.fn.NoSplit = hasCompilerDirective(fd, "go:nosplit")
	exportRuntimeBootstrap := g.pkg.Path() == "runtime" && (fd.Name.Name == "args" || fd.Name.Name == "check" || fd.Name.Name == "main" || fd.Name.Name == "mstart0" || fd.Name.Name == "newproc" || fd.Name.Name == "newstack" || fd.Name.Name == "osinit" || fd.Name.Name == "schedinit" || fd.Name.Name == "throw")
	if ast.IsExported(fd.Name.Name) || isMain || exportRuntimeBootstrap {
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
	g.deferOrder = nil
	g.deferActions = nil
	g.runningDefers = false
	g.parents = astParents(fd.Body)
	g.currentBody = fd.Body
	g.seq = 0
	g.cur = g.fn.Entry()
	g.at(fd)
	ast.Inspect(fd.Body, func(node ast.Node) bool {
		label, ok := node.(*ast.LabeledStmt)
		if ok {
			g.labels[label.Label.Name] = g.block("label_" + label.Label.Name)
		}
		deferStatement, ok := node.(*ast.DeferStmt)
		if ok {
			slot := g.alloc(types.Typ[types.Bool])
			g.store(g.fn.Word(0), slot, types.Typ[types.Bool])
			g.deferSlots[deferStatement] = slot
			g.deferOrder = append(g.deferOrder, deferStatement)
		}
		return true
	})
	if receiver := sig.Recv(); receiver != nil {
		cls, ok := scalar(receiver.Type())
		if !ok {
			g.fail(fd, "unsupported receiver type %s", receiver.Type())
			return
		}
		parameter := g.fn.Param(receiver.Name(), cls)
		g.fn.Temp(parameter).Agg = g.goABIAggregate(receiver.Type())
		slot := g.alloc(receiver.Type())
		g.store(parameter, slot, receiver.Type())
		g.vars[receiver] = slot
	}
	for i := 0; i < sig.Params().Len(); i++ {
		v := sig.Params().At(i)
		c, ok := scalar(v.Type())
		if !ok {
			g.fail(fd, "unsupported parameter type %s", v.Type())
			return
		}
		p := g.fn.Param(v.Name(), c)
		g.fn.Temp(p).Agg = g.goABIAggregate(v.Type())
		slot := g.alloc(v.Type())
		g.store(p, slot, v.Type())
		g.vars[v] = slot
	}
	if sig.Results().Len() > 0 && isInlineAggregate(sig.Results().At(0).Type()) && resultAggregate == nil {
		g.aggregateResult = g.fn.Param("result0", ir.ClsP)
	}
	if sig.Results().Len() > 0 {
		g.resultType = sig.Results().At(0).Type()
	}
	for i := 1; i < sig.Results().Len(); i++ {
		result := sig.Results().At(i)
		pointer := g.fn.Param(fmt.Sprintf("result%d", i), ir.ClsP)
		g.extraResultSlots = append(g.extraResultSlots, pointer)
		g.extraResultTypes = append(g.extraResultTypes, result.Type())
		if result.Name() != "" {
			g.vars[result] = pointer
			if isInlineAggregate(result.Type()) {
				g.zero(pointer, result.Type())
				g.directValues[result] = true
			} else {
				g.store(g.zeroValue(result.Type()), pointer, result.Type())
			}
		}
	}
	if sig.Results().Len() > 0 && sig.Results().At(0).Name() != "" {
		result := sig.Results().At(0)
		g.resultType = result.Type()
		if isInlineAggregate(result.Type()) {
			g.resultSlot = g.aggregateResult
			if g.resultSlot == ir.R {
				g.resultSlot = g.aggregateResultStorage(result.Type())
			}
			g.zero(g.resultSlot, result.Type())
			g.directValues[result] = true
		} else {
			g.resultSlot = g.alloc(result.Type())
			g.store(g.zeroValue(result.Type()), g.resultSlot, result.Type())
		}
		g.vars[result] = g.resultSlot
	}
	g.stmts(fd.Body.List)
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
			if _, isInterface := selection.Recv().Underlying().(*types.Interface); isInterface {
				if target := g.methodTargets[object.Name()]; target != nil {
					receiver = g.interfaceMethodReceiver(receiver, target)
					object = target
				} else {
					g.interfaceMethods[object] = true
				}
			}
		}
	}
	var signature *types.Signature
	var callee ir.Ref
	var closure ir.Ref
	if object != nil {
		signature = object.Type().(*types.Signature)
		callee = g.fn.Sym(g.functionSymbol(object), 0)
	} else {
		var ok bool
		signature, ok = g.info.Types[call.Fun].Type.Underlying().(*types.Signature)
		if !ok {
			g.fail(call, "multiple-result call target is not a function")
			return
		}
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
	if closure != ir.R {
		g.pinClosure(closure)
	}

	values := make([]ir.Ref, signature.Results().Len())
	firstResultType := signature.Results().At(0).Type()
	if isInlineAggregate(firstResultType) {
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
		objectSignature := object.Type().(*types.Signature)
		if objectSignature.Recv() != nil {
			receiverType = objectSignature.Recv().Type()
		}
	}
	values[0] = g.callWithSignature(resultClass, callee, arguments, signature, receiverType)

	for i, lhs := range statement.Lhs {
		if identifier, ok := lhs.(*ast.Ident); ok && identifier.Name == "_" {
			continue
		}
		resultType := signature.Results().At(i).Type()
		value := values[i]
		if i > 0 && !isInlineAggregate(resultType) {
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
				slot = g.allocLocal(resultType)
				g.vars[variable] = slot
			}
			_, global := g.globals[variable]
			localIdentifier = !global && !g.directValues[variable]
		} else {
			slot = g.lvalue(lhs)
		}
		value = g.coerce(value, resultType)
		if localIdentifier && isMemoryValue(resultType) {
			g.assignLocal(value, slot, resultType)
		} else if localIdentifier && isDescriptorValue(resultType) {
			g.store(value, slot, resultType)
		} else if isInlineAggregate(resultType) {
			g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), slot, value, g.fn.Long(typeSize(resultType)))
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
	targetType := g.info.Types[assertion.Type].Type
	g.assignResult(statement.Lhs[0], statement.Tok, value, targetType)
	g.assignResult(statement.Lhs[1], statement.Tok, ok, types.Typ[types.Bool])
}

func (g *gen) typeAssertion(assertion *ast.TypeAssertExpr) (ir.Ref, ir.Ref) {
	descriptor := g.expr(assertion.X)
	targetType := g.info.Types[assertion.Type].Type
	targetClass, _ := scalar(targetType)
	failureValue := g.zeroValue(targetType)

	nonNil := g.block("assertnonnil")
	success := g.block("assertsuccess")
	failure := g.block("assertfailure")
	done := g.block("assertdone")
	isNil := g.interfaceIsNil(descriptor)
	g.cur.Jnz(isNil, failure, nonNil)

	g.cur = nonNil
	dynamicTag := g.cur.Load(ir.ClsP, descriptor)
	match := g.fn.Word(0)
	if targetInterface, ok := targetType.Underlying().(*types.Interface); ok {
		if targetInterface.NumMethods() == 0 {
			match = g.fn.Word(1)
		} else {
			for _, implementation := range g.interfaceImplementations(targetInterface) {
				matchesImplementation := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(implementation))
				match = g.cur.Or(ir.ClsW, match, matchesImplementation)
			}
		}
	} else {
		match = g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(targetType))
	}
	g.cur.Jnz(match, success, failure)

	g.cur = success
	successValue := descriptor
	if _, targetIsInterface := targetType.Underlying().(*types.Interface); !targetIsInterface {
		payload := g.cur.Load(ir.ClsP, g.offset(descriptor, 8))
		if isMemoryValue(targetType) || isDirectInterfaceType(targetType) {
			successValue = payload
		} else {
			successValue = g.load(payload, targetType)
		}
	}
	g.cur.Goto(done)

	g.cur = failure
	g.cur.Goto(done)

	g.cur = done
	value := done.Phi(targetClass,
		ir.PhiEdge{From: success, Val: successValue},
		ir.PhiEdge{From: failure, Val: failureValue},
	)
	asserted := done.Phi(ir.ClsW,
		ir.PhiEdge{From: success, Val: g.fn.Word(1)},
		ir.PhiEdge{From: failure, Val: g.fn.Word(0)},
	)
	return value, asserted
}

func (g *gen) interfaceImplementations(target *types.Interface) []types.Type {
	implementations := make(map[string]types.Type)
	for function := range g.functionDecls {
		signature, ok := function.Type().(*types.Signature)
		if !ok || signature.Recv() == nil {
			continue
		}
		receiverType := signature.Recv().Type()
		if !types.Implements(receiverType, target) {
			continue
		}
		key := types.TypeString(receiverType, func(pkg *types.Package) string {
			return pkg.Path()
		})
		implementations[key] = receiverType
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
			slot = g.allocLocal(valueType)
			g.vars[variable] = slot
		}
		_, global := g.globals[variable]
		localIdentifier = !global && !g.directValues[variable]
	} else {
		slot = g.lvalue(lhs)
	}
	value = g.coerce(value, valueType)
	if localIdentifier && isMemoryValue(valueType) {
		g.assignLocal(value, slot, valueType)
	} else if localIdentifier && isDescriptorValue(valueType) {
		g.store(value, slot, valueType)
	} else if isInlineAggregate(valueType) {
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), slot, value, g.fn.Long(typeSize(valueType)))
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
				_, ok := scalar(obj.Type())
				if !ok {
					g.fail(id, "unsupported variable type %s", obj.Type())
					return
				}
				slot := g.allocLocal(obj.Type())
				g.vars[obj] = slot
				if i >= len(vs.Values) && isMemoryValue(obj.Type()) {
					continue
				}
				v := g.zeroValue(obj.Type())
				if i < len(vs.Values) {
					v = g.assignmentValue(vs.Values[i], obj.Type())
				}
				if isDescriptorValue(obj.Type()) {
					v = g.copyInlineValue(v, obj.Type())
				}
				g.assignLocal(v, slot, obj.Type())
			}
		}
	case *ast.AssignStmt:
		if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
			if index, ok := n.Lhs[0].(*ast.IndexExpr); ok {
				if _, isMap := g.info.Types[index.X].Type.Underlying().(*types.Map); isMap {
					g.mapAssign(index, n.Rhs[0])
					return
				}
			}
		}
		if len(n.Rhs) == 1 && len(n.Lhs) > 1 {
			if index, ok := n.Rhs[0].(*ast.IndexExpr); ok {
				if _, isMap := g.info.Types[index.X].Type.Underlying().(*types.Map); isMap {
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
			targetType := g.info.Types[n.Lhs[i]].Type
			if targetType == nil {
				if identifier, ok := n.Lhs[i].(*ast.Ident); ok {
					object := g.info.Uses[identifier]
					if object == nil {
						object = g.info.Defs[identifier]
					}
					if object != nil {
						targetType = object.Type()
					}
				}
			}
			if targetType == nil {
				targetType = g.info.Types[e].Type
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
				typ = obj.Type()
				var exists bool
				slot, exists = g.addr(obj)
				if !exists {
					slot = g.allocLocal(typ)
					g.vars[obj] = slot
				}
				_, global := g.globals[obj]
				destinationIsInline = global && (isMemoryValue(typ) || isDescriptorValue(typ))
				if g.directValues[obj] && isInlineAggregate(typ) {
					destinationIsInline = true
				}
				if global && isDescriptorValue(typ) {
					slot = g.cur.Load(ir.ClsP, slot)
				}
				localIdentifier = !global && !g.directValues[obj]
			} else {
				typ = g.info.Types[lhs].Type
				slot = g.lvalue(lhs)
				destinationIsInline = true
			}
			v := vals[i]
			if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
				old := g.load(slot, typ)
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
			if destinationIsInline && isInlineAggregate(typ) {
				g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), slot, v, g.fn.Long(typeSize(typ)))
			} else {
				g.store(v, slot, typ)
			}
		}
	case *ast.IncDecStmt:
		targetType := g.info.Types[n.X].Type
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
		g.runDefers()
		if len(n.Results) == 0 {
			if g.fn.HasRet && g.resultSlot != ir.R {
				value := g.resultSlot
				if !isInlineAggregate(g.resultType) {
					value = g.load(g.resultSlot, g.resultType)
				}
				g.cur.Ret(g.stableReturnValue(value, g.resultType))
			} else {
				g.cur.RetVoid()
			}
		} else {
			if len(n.Results) == 1 {
				if call, ok := n.Results[0].(*ast.CallExpr); ok {
					if _, multi := g.info.Types[call].Type.(*types.Tuple); multi {
						g.returnMultiValueCall(call)
						return
					}
				}
			}
			values := make([]ir.Ref, len(n.Results))
			for i, result := range n.Results {
				resultType := g.resultType
				if i > 0 {
					resultType = g.extraResultTypes[i-1]
				} else if resultType == nil {
					resultType = g.info.Types[result].Type
				}
				if identifier, ok := result.(*ast.Ident); ok && identifier.Name == "nil" && isInlineAggregate(resultType) {
					values[i] = g.localAlloc(8, int(typeSize(resultType)))
					g.zero(values[i], resultType)
				} else {
					values[i] = g.assignmentValue(result, resultType)
				}
			}
			for i := 1; i < len(values); i++ {
				resultType := g.extraResultTypes[i-1]
				if isInlineAggregate(resultType) {
					g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), g.extraResultSlots[i-1], values[i], g.fn.Long(typeSize(resultType)))
				} else {
					g.store(g.stableReturnValue(values[i], resultType), g.extraResultSlots[i-1], resultType)
				}
			}
			g.cur.Ret(g.stableReturnValue(values[0], g.resultType))
		}
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
		default:
			g.stmt(statement)
		}
	case *ast.DeferStmt:
		g.store(g.fn.Word(1), g.deferSlots[n], types.Typ[types.Bool])
		g.deferActions = append(g.deferActions, n)
	case *ast.SendStmt:
		channel := g.expr(n.Chan)
		elementType := g.info.Types[n.Value].Type
		value := g.assignmentValue(n.Value, elementType)
		address := value
		if !isMemoryValue(elementType) {
			address = g.allocLocal(elementType)
			g.store(value, address, elementType)
		}
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
	switch target := call.Fun.(type) {
	case *ast.Ident:
		function, _ = g.info.Uses[target].(*types.Func)
	case *ast.SelectorExpr:
		function, _ = g.info.Uses[target.Sel].(*types.Func)
		selection := g.info.Selections[target]
		if selection != nil && selection.Kind() == types.MethodVal {
			receiver = g.methodReceiver(target, function)
			if _, isInterface := selection.Recv().Underlying().(*types.Interface); isInterface {
				if concrete := g.methodTargets[function.Name()]; concrete != nil {
					receiver = g.interfaceMethodReceiver(receiver, concrete)
					function = concrete
				} else {
					g.interfaceMethods[function] = true
				}
			}
		}
	}

	signature, ok := g.info.Types[call.Fun].Type.Underlying().(*types.Signature)
	if !ok || signature.Results().Len() < 2 {
		g.fail(call, "return call is not multi-valued")
		return
	}

	var callee ir.Ref
	var closure ir.Ref
	if function != nil {
		callee = g.fn.Sym(g.functionSymbol(function), 0)
	} else {
		closure = g.expr(call.Fun)
		callee = g.cur.Load(ir.ClsP, closure)
	}
	arguments := make([]ir.Ref, 0, len(call.Args)+signature.Results().Len())
	if receiver != ir.R {
		arguments = append(arguments, receiver)
	}
	arguments = append(arguments, g.callArguments(call.Args, call.Ellipsis.IsValid(), signature)...)
	if closure != ir.R {
		g.pinClosure(closure)
	}
	resultType := signature.Results().At(0).Type()
	if isInlineAggregate(resultType) {
		arguments = append(arguments, g.aggregateResult)
	}
	arguments = append(arguments, g.extraResultSlots...)

	resultClass, _ := scalar(resultType)
	var receiverType types.Type
	if receiver != ir.R && function != nil {
		functionSignature := function.Type().(*types.Signature)
		if functionSignature.Recv() != nil {
			receiverType = functionSignature.Recv().Type()
		}
	}
	value := g.callWithSignature(resultClass, callee, arguments, signature, receiverType)
	g.cur.Ret(g.stableReturnValue(value, resultType))
}

func (g *gen) goStatement(statement *ast.GoStmt) {
	call := statement.Call
	identifier, ok := call.Fun.(*ast.Ident)
	if !ok {
		g.fail(call, "go statement currently requires a direct function")
		return
	}
	function, ok := g.info.Uses[identifier].(*types.Func)
	if !ok {
		g.fail(call, "go statement target is not a function")
		return
	}
	signature := function.Type().(*types.Signature)
	if signature.Results().Len() != 0 {
		g.fail(call, "go statement function must not return values")
		return
	}

	position := g.fset.Position(statement.Pos())
	wrapperName := fmt.Sprintf("%s.gowrap.%d.%d", g.pkg.Path(), position.Line, position.Column)
	wrapper := &gen{fn: g.mod.NewFuncVoid(wrapperName)}
	wrapper.cur = wrapper.fn.Entry()
	context := wrapper.closureContext()
	arguments := make([]ir.Ref, len(call.Args))
	for i, argument := range call.Args {
		parameterType := signature.Params().At(i).Type()
		arguments[i] = wrapper.load(wrapper.offset(context, int64(8*(i+1))), parameterType)
		_ = argument
	}
	wrapper.cur.CallVoid(wrapper.fn.Sym(g.functionSymbol(function), 0), arguments...)
	wrapper.cur.RetVoid()

	closureFields := make([]*types.Var, 0, len(call.Args)+1)
	closureFields = append(closureFields, types.NewVar(token.NoPos, nil, "code", types.Typ[types.Uintptr]))
	for index := 0; index < signature.Params().Len(); index++ {
		parameter := signature.Params().At(index)
		closureFields = append(closureFields, types.NewVar(token.NoPos, nil, parameter.Name(), parameter.Type()))
	}
	closureType := types.NewStruct(closureFields, nil)
	closure := g.allocateTyped(closureType)
	g.cur.Store(g.fn.Sym(wrapperName, 0), closure)
	for i, argument := range call.Args {
		parameterType := signature.Params().At(i).Type()
		value := g.assignmentValue(argument, parameterType)
		g.store(value, g.offset(closure, int64(8*(i+1))), parameterType)
	}
	g.cur.CallVoid(g.fn.Sym("runtime.newproc", 0), closure)
}

func (g *gen) runDefers() {
	if g.runningDefers {
		return
	}
	g.runningDefers = true
	defer func() {
		g.runningDefers = false
	}()

	for i := len(g.deferActions) - 1; i >= 0; i-- {
		deferStatement := g.deferActions[i]
		run := g.block("deferrun")
		done := g.block("deferdone")
		active := g.load(g.deferSlots[deferStatement], types.Typ[types.Bool])
		g.cur.Jnz(active, run, done)
		g.cur = run
		g.store(g.fn.Word(0), g.deferSlots[deferStatement], types.Typ[types.Bool])
		if literal, ok := deferStatement.Call.Fun.(*ast.FuncLit); ok && len(deferStatement.Call.Args) == 0 && literal.Type.Params.NumFields() == 0 {
			g.stmts(literal.Body.List)
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
	indexType := types.Typ[types.Int]
	indexSlot := g.alloc(indexType)
	g.store(g.fn.Long(0), indexSlot, indexType)

	if key, ok := statement.Key.(*ast.Ident); ok && key.Name != "_" {
		object := g.info.Defs[key]
		if object == nil {
			object = g.info.Uses[key]
		}
		g.vars[object] = indexSlot
	}

	rangeType := g.info.Types[statement.X].Type
	var upper ir.Ref
	var rangeData ir.Ref
	var stringDescriptor ir.Ref
	stringRange := false
	if _, ok := rangeType.Underlying().(*types.Slice); ok {
		slice := g.expr(statement.X)
		rangeData = g.cur.Load(ir.ClsP, slice)
		upper = g.cur.Load(ir.ClsL, g.offset(slice, 8))
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
	} else if basic, ok := rangeType.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
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
		valueType = object.Type()
		valueSlot = g.allocLocal(valueType)
		g.vars[object] = valueSlot
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
		if !isInlineAggregate(valueType) {
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

// Aggregate values and multiword descriptors are represented by an address in
// cg12 IR. A returned value therefore needs storage whose lifetime extends
// beyond the callee's stack frame. Scalars continue to use a result register.
func (g *gen) stableReturnValue(value ir.Ref, resultType types.Type) ir.Ref {
	if _, isInterface := resultType.Underlying().(*types.Interface); isInterface {
		nilValue := g.fn.ConstInt(ir.ClsP, 0)
		if g.fn.RetAgg != nil {
			nilValue = g.localAlloc(8, int(typeSize(resultType)))
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
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), stable, value, g.fn.Long(typeSize(resultType)))
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
	if g.fn.RetAgg != nil {
		// ABIInternal return lowering reads the aggregate fields into result
		// registers before tearing down this frame, so local result storage is
		// sufficient and does not escape.
		return value
	}

	size := typeSize(resultType)
	if g.aggregateResult != ir.R {
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), g.aggregateResult, value, g.fn.Long(size))
		return g.aggregateResult
	}
	result := g.allocateTyped(resultType)
	g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), result, value, g.fn.Long(size))
	return result
}

func (g *gen) copyInlineValue(value ir.Ref, valueType types.Type) ir.Ref {
	size := typeSize(valueType)
	copy := g.localAlloc(8, int(size))
	g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), copy, value, g.fn.Long(size))
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
		zero := g.localAlloc(8, int(typeSize(valueType)))
		g.zero(zero, valueType)
		return zero
	}
	class, _ := scalar(valueType)
	return g.fn.ConstInt(class, 0)
}

func (g *gen) allocLocal(t types.Type) ir.Ref {
	switch t.Underlying().(type) {
	case *types.Array, *types.Struct:
		slot := g.localAlloc(8, 8)
		size := typeSize(t)
		align := 8
		if size < 8 {
			align = 4
		}
		backing := g.localAlloc(align, int(size))
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
		size := typeSize(g.info.Types[expression].Type)
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
				cond = g.cur.Cmp(ir.CmpEq, ir.ClsW, tag, g.expr(e))
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
				dynamicTag := g.cur.Load(ir.ClsP, interfaceValue)
				caseType := g.info.Types[caseExpression].Type
				matches := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(caseType))
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
			slot := g.allocLocal(implicit.Type())
			if clause.List == nil || len(clause.List) != 1 {
				g.store(interfaceValue, slot, implicit.Type())
			} else if identifier, nilCase := clause.List[0].(*ast.Ident); nilCase && identifier.Name == "nil" {
				g.store(g.fn.ConstInt(ir.ClsP, 0), slot, implicit.Type())
			} else {
				data := g.cur.Load(ir.ClsP, g.offset(interfaceValue, 8))
				if isDirectInterfaceType(implicit.Type()) {
					g.store(data, slot, implicit.Type())
				} else {
					g.store(g.load(data, implicit.Type()), slot, implicit.Type())
				}
			}
			g.vars[implicit] = slot
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

func (g *gen) expr(e ast.Expr) ir.Ref {
	g.at(e)
	tv := g.info.Types[e]
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
		if g.directValues[obj] || (global && isMemoryValue(obj.Type())) {
			return slot
		}
		return g.load(slot, obj.Type())
	case *ast.BinaryExpr:
		if n.Op == token.LAND || n.Op == token.LOR {
			return g.logical(n)
		}
		return g.binary(n.Op, g.expr(n.X), g.expr(n.Y), g.info.Types[n.X].Type, n)
	case *ast.UnaryExpr:
		if n.Op == token.ARROW {
			channel := g.expr(n.X)
			channelType := g.info.Types[n.X].Type.Underlying().(*types.Chan)
			elementType := channelType.Elem()
			size := typeSize(elementType)
			if size < 4 {
				size = 4
			}
			value := g.localAlloc(4, int(size))
			g.cur.CallVoid(g.fn.Sym("runtime.chanrecv1", 0), channel, value)
			if isMemoryValue(elementType) {
				return value
			}
			return g.load(value, elementType)
		}
		if n.Op == token.AND {
			if literal, ok := n.X.(*ast.CompositeLit); ok {
				return g.compositeLiteral(literal, !g.nonEscapingAddress(n))
			}
			// Interface values are represented by an address to their two-word
			// runtime header. Taking the address of one must expose that header,
			// as runtime helpers such as efaceOf expect, rather than the frontend
			// slot that contains the header address.
			if _, ok := g.info.Types[n.X].Type.Underlying().(*types.Interface); ok {
				return g.expr(n.X)
			}
			if isInlineAggregate(g.info.Types[n.X].Type) {
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
		if isInlineAggregate(g.info.Types[n].Type) {
			return pointer
		}
		return g.load(pointer, g.info.Types[n].Type)
	case *ast.CompositeLit:
		return g.compositeLiteral(n, false)
	case *ast.FuncLit:
		return g.functionLiteral(n)
	case *ast.CallExpr:
		if g.info.Types[n.Fun].IsType() {
			if len(n.Args) != 1 {
				g.fail(n, "conversion requires one argument")
				return ir.R
			}
			if _, ok := tv.Type.Underlying().(*types.Slice); ok {
				if basic, ok := g.info.Types[n.Args[0]].Type.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
					return g.stringSlice(n.Args[0])
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
			return g.convert(x, g.info.Types[n.Args[0]].Type, tv.Type)
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
				if _, isInterface := selection.Recv().Underlying().(*types.Interface); isInterface {
					if target := g.methodTargets[obj.Name()]; target != nil {
						receiver = g.interfaceMethodReceiver(receiver, target)
						obj = target
					} else {
						g.interfaceMethods[obj] = true
					}
				}
			}
		}
		var callee ir.Ref
		var sig *types.Signature
		var closure ir.Ref
		if obj != nil {
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
			callee = g.fn.Sym(g.functionSymbol(obj), 0)
			sig, _ = g.info.Types[n.Fun].Type.Underlying().(*types.Signature)
			if sig == nil {
				sig = obj.Type().(*types.Signature)
			}
		} else {
			var ok bool
			sig, ok = g.info.Types[n.Fun].Type.Underlying().(*types.Signature)
			if !ok {
				g.fail(n, "call target is not a function")
				return ir.R
			}
			closure = g.expr(n.Fun)
			callee = g.cur.Load(ir.ClsP, closure)
		}
		args := make([]ir.Ref, 0, len(n.Args)+1)
		if receiver != ir.R {
			args = append(args, receiver)
		}
		args = append(args, g.callArguments(n.Args, n.Ellipsis.IsValid(), sig)...)
		if closure != ir.R {
			g.pinClosure(closure)
		}
		var receiverType types.Type
		if receiver != ir.R && obj != nil {
			objectSignature := obj.Type().(*types.Signature)
			if objectSignature.Recv() != nil {
				receiverType = objectSignature.Recv().Type()
			}
		}
		if sig.Results().Len() == 0 {
			g.callVoidWithSignature(callee, args, sig, receiverType)
			return g.fn.Word(0)
		}
		if isInlineAggregate(sig.Results().At(0).Type()) && !(g.runtimeAllocation && sig.Results().Len() == 1) {
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
		return g.callWithSignature(c, callee, args, sig, receiverType)
	case *ast.IndexExpr:
		if _, function := g.info.Types[n].Type.Underlying().(*types.Signature); function && g.info.Types[n.Index].IsType() {
			return g.instantiatedFunctionValue(n.X, n)
		}
		if _, isMap := g.info.Types[n.X].Type.Underlying().(*types.Map); isMap {
			value, _ := g.mapLookup(n)
			return value
		}
		base := g.indexBase(n.X)
		idx := g.expr(n.Index)
		element := g.info.Types[n].Type
		size := typeSize(element)
		if size != 1 {
			idx = g.cur.Mul(ir.ClsL, idx, g.fn.Long(size))
		}
		addr := g.cur.Add(ir.ClsP, base, idx)
		if isInlineAggregate(element) {
			return addr
		}
		return g.load(addr, element)
	case *ast.IndexListExpr:
		return g.instantiatedFunctionValue(n.X, n)
	case *ast.SliceExpr:
		base := g.indexBase(n.X)
		low := g.fn.Long(0)
		if n.Low != nil {
			low = g.expr(n.Low)
		}
		high := ir.R
		capacity := ir.R
		if n.High != nil {
			high = g.expr(n.High)
		} else if array, ok := g.info.Types[n.X].Type.Underlying().(*types.Array); ok {
			high = g.fn.Long(array.Len())
		} else if pointer, ok := g.info.Types[n.X].Type.Underlying().(*types.Pointer); ok {
			array, isArray := pointer.Elem().Underlying().(*types.Array)
			if !isArray {
				g.fail(n, "cannot slice pointer to %s", pointer.Elem())
				return ir.R
			}
			high = g.fn.Long(array.Len())
		} else {
			descriptor := g.expr(n.X)
			high = g.cur.Load(ir.ClsL, g.offset(descriptor, 8))
		}
		if basic, ok := g.info.Types[n].Type.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			data := g.cur.Add(ir.ClsP, base, low)
			length := g.cur.Sub(ir.ClsL, high, low)
			return g.stringDescriptor(data, length)
		}
		element := g.info.Types[n].Type.Underlying().(*types.Slice).Elem()
		if n.Max != nil {
			capacity = g.cur.Sub(ir.ClsL, g.expr(n.Max), low)
		} else if array, ok := g.info.Types[n.X].Type.Underlying().(*types.Array); ok {
			capacity = g.cur.Sub(ir.ClsL, g.fn.Long(array.Len()), low)
		} else if pointer, ok := g.info.Types[n.X].Type.Underlying().(*types.Pointer); ok {
			array := pointer.Elem().Underlying().(*types.Array)
			capacity = g.cur.Sub(ir.ClsL, g.fn.Long(array.Len()), low)
		} else {
			descriptor := g.expr(n.X)
			capacity = g.cur.Sub(ir.ClsL, g.cur.Load(ir.ClsL, g.offset(descriptor, 16)), low)
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
		targetType := g.info.Types[n].Type
		dynamicTag := g.cur.Load(ir.ClsP, interfaceValue)
		matches := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(targetType))
		g.cur.Jnz(matches, success, failure)

		g.cur = failure
		g.cur.CallVoid(g.fn.Sym("abort", 0))
		g.cur.Hlt()

		g.cur = success
		data := g.cur.Load(ir.ClsP, g.offset(interfaceValue, 8))
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
			if isMemoryValue(object.Type()) {
				return address
			}
			return g.load(address, object.Type())
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
		if isInlineAggregate(selection.Type()) {
			return addr
		}
		return g.load(addr, selection.Type())
	}
	g.fail(e, "unsupported expression %T", e)
	return ir.R
}

func (g *gen) instantiatedFunctionValue(expression ast.Expr, instantiation ast.Expr) ir.Ref {
	switch expression := expression.(type) {
	case *ast.Ident:
		if function, ok := g.info.Uses[expression].(*types.Func); ok {
			return g.functionValue(function)
		}
	case *ast.SelectorExpr:
		if function, ok := g.info.Uses[expression.Sel].(*types.Func); ok {
			return g.functionValue(function)
		}
	}
	g.fail(instantiation, "unsupported generic function value")
	return ir.R
}

func (g *gen) selectorAddress(address ir.Ref, selection *types.Selection) ir.Ref {
	currentType := selection.Recv()
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
	size := int(typeSize(resultType))
	align := int(typeAlign(resultType))
	if align < 4 {
		align = 4
	}
	return g.localAlloc(align, size)
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
		return receiver
	}
	return g.promotedMethodReceiver(receiver, selection)
}

func (g *gen) interfaceMethodReceiver(descriptor ir.Ref, method *types.Func) ir.Ref {
	payload := g.cur.Load(ir.ClsP, g.offset(descriptor, 8))
	receiverType := method.Type().(*types.Signature).Recv().Type()
	if _, pointer := receiverType.Underlying().(*types.Pointer); pointer {
		return payload
	}
	if isMemoryValue(receiverType) {
		return payload
	}
	return g.load(payload, receiverType)
}

func (g *gen) promotedMethodReceiver(receiver ir.Ref, selection *types.Selection) ir.Ref {
	currentType := selection.Recv()
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
	signature := selection.Type().(*types.Signature)
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
	wrapper := &gen{fn: function}
	wrapper.cur = function.Entry()
	context := wrapper.closureContext()
	receiverType := method.Type().(*types.Signature).Recv().Type()
	receiver := wrapper.load(wrapper.offset(context, 8), receiverType)
	arguments := []ir.Ref{receiver}
	for index := 0; index < signature.Params().Len(); index++ {
		parameter := signature.Params().At(index)
		class, _ := scalar(parameter.Type())
		arguments = append(arguments, function.Param(parameter.Name(), class))
	}
	callee := function.Sym(g.functionSymbol(method), 0)
	if signature.Results().Len() == 0 {
		wrapper.cur.CallVoid(callee, arguments...)
		wrapper.cur.RetVoid()
	} else {
		result := wrapper.cur.Call(resultClass, callee, arguments...)
		wrapper.cur.Ret(result)
	}
	descriptor := g.localAlloc(8, 16)
	g.cur.Store(g.fn.Sym(wrapperName, 0), descriptor)
	g.store(g.methodReceiver(expression, method), g.offset(descriptor, 8), receiverType)
	return descriptor
}

func (g *gen) compositeLiteral(literal *ast.CompositeLit, heap bool) ir.Ref {
	t := g.info.Types[literal].Type
	if mapType, isMap := t.Underlying().(*types.Map); isMap {
		mapping := g.allocateMap(mapType, g.fn.Long(8))
		if len(literal.Elts) != 0 {
			g.fail(literal, "non-empty map literals are not supported")
		}
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
		backing = g.allocateTyped(types.NewArray(elementType, length))
		g.zero(backing, types.NewArray(elementType, length))
		for i, expression := range literal.Elts {
			index := int64(i)
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				index = constInt(g.info.Types[keyed.Key].Value)
				expression = keyed.Value
			}
			g.store(g.assignmentValue(expression, elementType), g.offset(backing, index*elementSize), elementType)
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
	if heap {
		backing = g.allocateTyped(t)
	}
	memset := g.fn.Sym("goc_memset", 0)
	g.cur.Call(ir.ClsP, memset, backing, g.fn.Word(0), g.fn.Long(size))
	if isStruct {
		offsets := structOffsets(structFields(structure))
		for i, expression := range literal.Elts {
			fieldIndex := i
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
			value := g.expr(expression)
			fieldAddress := g.offset(backing, offsets[fieldIndex])
			if isInlineAggregate(fieldType) {
				g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), fieldAddress, value, g.fn.Long(typeSize(fieldType)))
			} else {
				g.store(value, fieldAddress, fieldType)
			}
		}
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
		g.store(g.expr(expression), address, elementType)
	}
	return backing
}

func (g *gen) functionLiteral(literal *ast.FuncLit) ir.Ref {
	signature := g.info.Types[literal.Type].Type.(*types.Signature)
	position := g.fset.Position(literal.Pos())
	symbol := fmt.Sprintf("%s.func.%d.%d", g.pkg.Path(), position.Line, position.Column)
	var captures []types.Object
	seenCapture := make(map[types.Object]bool)
	ast.Inspect(literal.Body, func(node ast.Node) bool {
		if nested, ok := node.(*ast.FuncLit); ok && nested != literal {
			return false
		}
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
		fset:                     g.fset,
		info:                     g.info,
		pkg:                      g.pkg,
		mod:                      g.mod,
		globals:                  g.globals,
		methodTargets:            g.methodTargets,
		emitRuntimeTables:        g.emitRuntimeTables,
		runtimeAllocation:        g.runtimeAllocation,
		typeTags:                 g.typeTags,
		vars:                     make(map[types.Object]ir.Ref),
		labels:                   make(map[string]*ir.Block),
		deferSlots:               make(map[*ast.DeferStmt]ir.Ref),
		functionDecls:            g.functionDecls,
		initSymbols:              g.initSymbols,
		noWriteBarrierFunctions:  g.noWriteBarrierFunctions,
		interfaceMethods:         g.interfaceMethods,
		dynamicInitializers:      g.dynamicInitializers,
		dynamicInitializerGuards: g.dynamicInitializerGuards,
		initializingGlobals:      make(map[types.Object]bool),
		noWriteBarrier:           g.noWriteBarrier,
		stackAddresses:           make(map[uint32]bool),
		heapCaptures:             make(map[types.Object]ir.Ref),
		directValues:             make(map[types.Object]bool),
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
	if signature.Results().Len() == 1 {
		resultAggregate = child.goABIAggregate(signature.Results().At(0).Type())
		child.fn.RetAgg = resultAggregate
	}
	child.cur = child.fn.Entry()
	child.parents = astParents(literal.Body)
	child.currentBody = literal.Body
	ast.Inspect(literal.Body, func(node ast.Node) bool {
		if label, ok := node.(*ast.LabeledStmt); ok {
			child.labels[label.Label.Name] = child.block("label_" + label.Label.Name)
		}
		if deferStatement, ok := node.(*ast.DeferStmt); ok {
			slot := child.alloc(types.Typ[types.Bool])
			child.store(child.fn.Word(0), slot, types.Typ[types.Bool])
			child.deferSlots[deferStatement] = slot
			child.deferOrder = append(child.deferOrder, deferStatement)
		}
		return true
	})
	for i := 0; i < signature.Params().Len(); i++ {
		parameter := signature.Params().At(i)
		class, ok := scalar(parameter.Type())
		if !ok {
			g.fail(literal, "unsupported function literal parameter %s", parameter.Type())
			return ir.R
		}
		value := child.fn.Param(parameter.Name(), class)
		child.fn.Temp(value).Agg = child.goABIAggregate(parameter.Type())
		slot := child.alloc(parameter.Type())
		child.store(value, slot, parameter.Type())
		child.vars[parameter] = slot
	}
	if signature.Results().Len() > 0 && isInlineAggregate(signature.Results().At(0).Type()) && resultAggregate == nil {
		child.aggregateResult = child.fn.Param("result0", ir.ClsP)
	}
	if signature.Results().Len() > 0 {
		child.resultType = signature.Results().At(0).Type()
	}
	environment := child.closureContext()
	for i, capture := range captures {
		child.vars[capture] = child.cur.Load(ir.ClsP, child.offset(environment, int64(8*(i+1))))
	}
	for i := 1; i < signature.Results().Len(); i++ {
		result := signature.Results().At(i)
		pointer := child.fn.Param(fmt.Sprintf("result%d", i), ir.ClsP)
		child.extraResultSlots = append(child.extraResultSlots, pointer)
		child.extraResultTypes = append(child.extraResultTypes, result.Type())
		if result.Name() != "" {
			child.vars[result] = pointer
			if isInlineAggregate(result.Type()) {
				child.zero(pointer, result.Type())
				child.directValues[result] = true
			} else {
				child.store(child.zeroValue(result.Type()), pointer, result.Type())
			}
		}
	}
	if signature.Results().Len() > 0 && signature.Results().At(0).Name() != "" {
		result := signature.Results().At(0)
		child.resultType = result.Type()
		if isInlineAggregate(result.Type()) {
			child.resultSlot = child.aggregateResult
			if child.resultSlot == ir.R {
				child.resultSlot = child.aggregateResultStorage(result.Type())
			}
			child.zero(child.resultSlot, result.Type())
			child.directValues[result] = true
		} else {
			child.resultSlot = child.alloc(result.Type())
			child.store(child.zeroValue(result.Type()), child.resultSlot, result.Type())
		}
		child.vars[result] = child.resultSlot
	}
	child.stmts(literal.Body.List)
	if child.err != nil {
		g.err = child.err
		return ir.R
	}
	if child.live() {
		if signature.Results().Len() == 0 {
			child.cur.RetVoid()
		} else {
			g.fail(literal, "function literal is missing a return")
			return ir.R
		}
	}
	if len(captures) == 0 {
		return g.staticFunctionValue(symbol)
	}
	fields := make([]*types.Var, 0, len(captures)+1)
	fields = append(fields, types.NewVar(token.NoPos, nil, "code", types.Typ[types.Uintptr]))
	for index := range captures {
		fields = append(fields, types.NewVar(token.NoPos, nil, fmt.Sprintf("capture%d", index), types.Typ[types.UnsafePointer]))
	}
	escaping := g.functionLiteralEscapes(literal)
	var descriptor ir.Ref
	if escaping {
		descriptor = g.allocateTyped(types.NewStruct(fields, nil))
	} else {
		descriptor = g.localAlloc(8, 8*(len(captures)+1))
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
			if basic, ok := captureType.Underlying().(*types.Basic); ok && basic.Info()&types.IsUntyped != 0 {
				captureType = types.Default(captureType)
				if captureType == nil || captureType == types.Typ[types.UntypedNil] {
					captureType = types.Typ[types.UnsafePointer]
				}
			}
			cell = g.allocateTyped(captureType)
			if isInlineAggregate(captureType) {
				g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), cell, g.vars[capture], g.fn.Long(typeSize(captureType)))
			} else {
				g.cur.Store(g.load(g.vars[capture], captureType), cell)
			}
			g.heapCaptures[capture] = cell
			g.vars[capture] = cell
		}
		g.cur.Store(cell, g.offset(descriptor, int64(8*(i+1))))
	}
	return descriptor
}

func (g *gen) functionLiteralEscapes(literal *ast.FuncLit) bool {
	parent := g.parents[literal]
	if call, ok := parent.(*ast.CallExpr); ok {
		for _, argument := range call.Args {
			if argument == literal {
				return false
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
		object := g.info.Defs[identifier]
		if object == nil {
			return true
		}
		escapes := false
		ast.Inspect(g.currentBody, func(node ast.Node) bool {
			use, ok := node.(*ast.Ident)
			if !ok || g.info.Uses[use] != object {
				return true
			}
			call, ok := g.parents[use].(*ast.CallExpr)
			if !ok || call.Fun != use {
				escapes = true
			}
			return true
		})
		return escapes
	}
	return true
}

func (g *gen) functionValue(function *types.Func) ir.Ref {
	return g.staticFunctionValue(g.functionSymbol(function))
}

func (g *gen) staticFunctionValue(symbol string) ir.Ref {
	name := fmt.Sprintf(".goc.funcval.%d", len(g.mod.Data))
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubL, Sym: symbol}},
	})
	return g.fn.Sym(name, 0)
}

func (g *gen) closureRegister() int {
	if runtime.GOARCH == "arm64" {
		// Go's ARM64 ABIInternal reserves X26 for the closure context.
		return 26
	}
	return 2
}

func (g *gen) closureContext() ir.Ref {
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
}

func (g *gen) stringSlice(expression ast.Expr) ir.Ref {
	value := g.info.Types[expression].Value
	if value == nil {
		stringValue := g.expr(expression)
		data := g.cur.Load(ir.ClsP, stringValue)
		length := g.cur.Load(ir.ClsL, g.offset(stringValue, 8))
		return g.sliceDescriptor(data, length, length)
	}
	if value.Kind() != constant.String {
		g.fail(expression, "invalid string conversion")
		return ir.R
	}
	contents := constant.StringVal(value)
	bytes := []byte(contents)
	values := make([]int64, len(bytes))
	for i, b := range bytes {
		values[i] = int64(b)
	}
	name := fmt.Sprintf(".goc.string.%d", len(g.mod.Data))
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: 1,
		Items: []ir.DataItem{{Sub: ir.SubUB, Ints: values}},
	})
	data := g.fn.Sym(name, 0)
	length := g.fn.Long(int64(len(bytes)))
	return g.sliceDescriptor(data, length, length)
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
	g.cur.Store(g.fn.Sym(name, 0), descriptor)
	g.cur.Store(g.fn.Long(int64(len(bytes))), g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) indexBase(expression ast.Expr) ir.Ref {
	base := g.expr(expression)
	expressionType := representativeType(g.info.Types[expression].Type)
	if _, ok := expressionType.Underlying().(*types.Slice); ok {
		return g.cur.Load(ir.ClsP, base)
	}
	if basic, ok := expressionType.Underlying().(*types.Basic); ok && basic.Info()&types.IsString != 0 {
		return g.cur.Load(ir.ClsP, base)
	}
	return base
}

func (g *gen) sliceDescriptor(data, length, capacity ir.Ref) ir.Ref {
	descriptor := g.localAlloc(8, 24)
	g.cur.Store(data, descriptor)
	g.cur.Store(g.descriptorLength(length), g.offset(descriptor, 8))
	g.cur.Store(g.descriptorLength(capacity), g.offset(descriptor, 16))
	return descriptor
}

func (g *gen) descriptorLength(value ir.Ref) ir.Ref {
	if g.fn.ClassOf(value) == ir.ClsW {
		return g.cur.Extsw(ir.ClsL, value)
	}
	return value
}

func (g *gen) allocateTyped(valueType types.Type) ir.Ref {
	if g.runtimeAllocation {
		return g.cur.HeapAlloc(
			g.fn.Sym("runtime.newobject", 0),
			g.runtimeType(valueType),
			int(typeSize(valueType)),
			int(typeAlign(valueType)),
		)
	}
	return g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(typeSize(valueType)))
}

func (g *gen) allocateZeroed(size ir.Ref) ir.Ref {
	if g.runtimeAllocation {
		nilType := g.fn.ConstInt(ir.ClsP, 0)
		return g.cur.Call(ir.ClsP, g.fn.Sym("runtime.mallocgc", 0), size, nilType, g.fn.Word(1))
	}
	return g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), size)
}

func (g *gen) stringDescriptor(data, length ir.Ref) ir.Ref {
	descriptor := g.localAlloc(8, 16)
	g.cur.Store(data, descriptor)
	g.cur.Store(g.descriptorLength(length), g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) offset(base ir.Ref, offset int64) ir.Ref {
	if offset == 0 {
		return base
	}
	address := g.cur.Add(ir.ClsP, base, g.fn.Long(offset))
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
		argumentType := g.info.Types[call.Args[0]].Type
		pointerSize := int64(types.SizesFor("gc", runtime.GOARCH).Sizeof(types.Typ[types.Uintptr]))
		if _, isTypeParameter := argumentType.(*types.TypeParam); isTypeParameter {
			return g.fn.Long(pointerSize)
		}
		return g.fn.Long(typeSize(argumentType))
	case "make":
		if mapType, ok := g.info.Types[call].Type.Underlying().(*types.Map); ok {
			return g.makeMap(call, mapType)
		}
		if sliceType, ok := g.info.Types[call].Type.Underlying().(*types.Slice); ok {
			length := g.expr(call.Args[1])
			capacity := length
			if len(call.Args) == 3 {
				capacity = g.expr(call.Args[2])
			}
			var data ir.Ref
			if fixedCapacity, stack := g.fixedSliceMakeCapacity(call); stack {
				elementSize := typeSize(sliceType.Elem())
				alignment := int(typeAlign(sliceType.Elem()))
				if alignment < 4 {
					alignment = 4
				}
				data = g.localAlloc(alignment, int(fixedCapacity*elementSize))
				g.cur.Call(ir.ClsP, g.fn.Sym("goc_memset", 0), data, g.fn.Word(0), g.fn.Long(fixedCapacity*elementSize))
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
		if channelType, ok := g.info.Types[call].Type.Underlying().(*types.Chan); ok {
			capacity := g.fn.Long(0)
			if len(call.Args) == 2 {
				capacity = g.expr(call.Args[1])
			}
			return g.cur.Call(ir.ClsP, g.fn.Sym("runtime.makechan", 0), g.channelType(channelType), capacity)
		}
		g.fail(call, "unsupported make result %s", g.info.Types[call].Type)
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
		return g.cur.Load(ir.ClsP, descriptor)
	case "real", "imag":
		return g.complexComponent(call, builtin.Name() == "imag")
	case "min", "max":
		result := g.expr(call.Args[0])
		resultType := g.info.Types[call.Args[0]].Type
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
		pointer := g.info.Types[call].Type.(*types.Pointer)
		return g.allocateTyped(pointer.Elem())
	case "len", "cap":
		argumentType := g.info.Types[call.Args[0]].Type
		switch t := representativeType(argumentType).Underlying().(type) {
		case *types.Array:
			return g.fn.Long(t.Len())
		case *types.Slice:
			descriptor := g.expr(call.Args[0])
			offset := int64(8)
			if builtin.Name() == "cap" {
				offset = 16
			}
			return g.cur.Load(ir.ClsL, g.offset(descriptor, offset))
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
		abort := g.fn.Sym("abort", 0)
		g.cur.CallVoid(abort)
		g.cur.Hlt()
		return g.fn.Word(0)
	case "recover":
		return g.fn.ConstInt(ir.ClsP, 0)
	case "print", "println":
		g.builtinPrint(call, builtin.Name() == "println")
		return g.fn.Word(0)
	case "copy":
		destination := g.expr(call.Args[0])
		source := g.expr(call.Args[1])
		length := g.cur.Load(ir.ClsL, g.offset(source, 8))
		destinationLength := g.cur.Load(ir.ClsL, g.offset(destination, 8))
		useSource := g.cur.Cmp(ir.CmpSle, ir.ClsW, length, destinationLength)
		shorter := g.selectValue(useSource, length, destinationLength, ir.ClsL)
		element := g.info.Types[call.Args[0]].Type.Underlying().(*types.Slice).Elem()
		bytes := shorter
		if size := typeSize(element); size != 1 {
			bytes = g.cur.Mul(ir.ClsL, shorter, g.fn.Long(size))
		}
		memcpy := g.fn.Sym("goc_memcpy", 0)
		destinationData := g.cur.Load(ir.ClsP, destination)
		sourceData := g.cur.Load(ir.ClsP, source)
		g.cur.Call(ir.ClsP, memcpy, destinationData, sourceData, bytes)
		return shorter
	case "clear":
		argumentType := g.info.Types[call.Args[0]].Type
		var data ir.Ref
		var size ir.Ref
		switch target := argumentType.Underlying().(type) {
		case *types.Array:
			data = g.expr(call.Args[0])
			size = g.fn.Long(typeSize(target))
		case *types.Slice:
			descriptor := g.expr(call.Args[0])
			data = g.cur.Load(ir.ClsP, descriptor)
			length := g.cur.Load(ir.ClsL, g.offset(descriptor, 8))
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
	argumentType, ok := g.info.Types[call.Args[0]].Type.Underlying().(*types.Basic)
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
	sliceType := g.info.Types[call].Type.Underlying().(*types.Slice)
	elementType := sliceType.Elem()
	elementSize := typeSize(elementType)
	destination := g.expr(call.Args[0])
	oldData := g.cur.Load(ir.ClsP, destination)
	oldLength := g.cur.Load(ir.ClsL, g.offset(destination, 8))
	oldCapacity := g.cur.Load(ir.ClsL, g.offset(destination, 16))

	var source ir.Ref
	var values []ir.Ref
	var added ir.Ref
	if call.Ellipsis.IsValid() {
		source = g.expr(call.Args[1])
		added = g.cur.Load(ir.ClsL, g.offset(source, 8))
	} else {
		values = make([]ir.Ref, 0, len(call.Args)-1)
		for _, argument := range call.Args[1:] {
			values = append(values, g.assignmentValue(argument, elementType))
		}
		added = g.fn.Long(int64(len(values)))
	}
	newLength := g.cur.Add(ir.ClsL, oldLength, added)

	resultSlot := g.localAlloc(8, 8)
	grow := g.block("appendgrow")
	reuse := g.block("appendreuse")
	done := g.block("appenddone")
	needsGrowth := g.cur.Cmp(ir.CmpUgt, ir.ClsL, newLength, oldCapacity)
	g.cur.Jnz(needsGrowth, grow, reuse)

	g.cur = grow
	var grown ir.Ref
	if g.runtimeAllocation {
		grown = g.cur.Call(ir.ClsP, g.fn.Sym("runtime.growslice", 0), oldData, newLength, oldCapacity, added, g.runtimeType(elementType))
		instruction := &g.cur.Instrs[len(g.cur.Instrs)-1]
		instruction.RetAgg = g.goABIAggregate(sliceType)
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
	g.cur.Store(grown, resultSlot)
	g.cur.Goto(done)

	g.cur = reuse
	g.cur.Store(g.sliceDescriptor(oldData, newLength, oldCapacity), resultSlot)
	g.cur.Goto(done)

	g.cur = done
	result := g.cur.Load(ir.ClsP, resultSlot)
	resultData := g.cur.Load(ir.ClsP, result)
	byteOffset := oldLength
	if elementSize != 1 {
		byteOffset = g.cur.Mul(ir.ClsL, oldLength, g.fn.Long(elementSize))
	}
	writeAt := g.cur.Add(ir.ClsP, resultData, byteOffset)
	if source != ir.R {
		sourceData := g.cur.Load(ir.ClsP, source)
		byteLength := added
		if elementSize != 1 {
			byteLength = g.cur.Mul(ir.ClsL, added, g.fn.Long(elementSize))
		}
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memmove", 0), writeAt, sourceData, byteLength)
	} else {
		for index, value := range values {
			address := g.offset(writeAt, int64(index)*elementSize)
			if isInlineAggregate(elementType) {
				g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), address, value, g.fn.Long(elementSize))
			} else {
				g.store(value, address, elementType)
			}
		}
	}
	return result
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

func (g *gen) mapLookup(index *ast.IndexExpr) (ir.Ref, ir.Ref) {
	mapType := g.info.Types[index.X].Type.Underlying().(*types.Map)
	mapping := g.expr(index.X)
	key := g.expr(index.Index)
	valueSlot := g.allocLocal(mapType.Elem())
	foundSlot := g.alloc(types.Typ[types.Bool])
	g.zero(valueSlot, mapType.Elem())
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
	storedKey := g.load(keyAddress, mapType.Key())
	keyClass, _ := scalar(mapType.Key())
	equal := g.cur.Cmp(ir.CmpEq, keyClass, storedKey, key)
	g.cur.Jnz(equal, found, next)

	g.cur = found
	valueAddress := g.mapElementAddress(mapping, mapValuesOffset, i, mapType.Elem())
	g.store(g.load(valueAddress, mapType.Elem()), valueSlot, mapType.Elem())
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
	mapType := g.info.Types[index.X].Type.Underlying().(*types.Map)
	mapping := g.expr(index.X)
	key := g.expr(index.Index)
	value := g.expr(valueExpression)
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
	storedKey := g.load(keyAddress, mapType.Key())
	keyClass, _ := scalar(mapType.Key())
	equal := g.cur.Cmp(ir.CmpEq, keyClass, storedKey, key)
	g.cur.Jnz(equal, update, next)

	g.cur = insert
	keyAddress = g.mapElementAddress(mapping, mapKeysOffset, i, mapType.Key())
	g.store(key, keyAddress, mapType.Key())
	g.cur.StoreSub(ir.SubUB, g.fn.Word(1), usedAddress)
	length := g.cur.Load(ir.ClsL, g.offset(mapping, mapLengthOffset))
	length = g.cur.Add(ir.ClsL, length, g.fn.Long(1))
	g.cur.Store(length, g.offset(mapping, mapLengthOffset))
	g.cur.Goto(update)

	g.cur = update
	valueAddress := g.mapElementAddress(mapping, mapValuesOffset, i, mapType.Elem())
	g.store(value, valueAddress, mapType.Elem())
	g.cur.Goto(done)

	g.cur = next
	i = g.cur.Add(ir.ClsL, i, g.fn.Long(1))
	g.store(i, indexSlot, types.Typ[types.Int])
	g.cur.Goto(test)
	g.cur = done
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
		slot = g.alloc(object.Type())
		g.vars[object] = slot
	}
	g.store(g.coerce(value, object.Type()), slot, object.Type())
}

func (g *gen) mapDelete(mapExpression, keyExpression ast.Expr) {
	mapType := g.info.Types[mapExpression].Type.Underlying().(*types.Map)
	mapping := g.expr(mapExpression)
	key := g.expr(keyExpression)
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
	storedKey := g.load(keyAddress, mapType.Key())
	keyClass, _ := scalar(mapType.Key())
	equal := g.cur.Cmp(ir.CmpEq, keyClass, storedKey, key)
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
	printString := g.fn.Sym("runtime_gocPrintString", 0)
	printInteger := g.fn.Sym("runtime_gocPrintInteger", 0)
	for _, argument := range call.Args {
		value := g.info.Types[argument]
		if value.Value != nil && value.Value.Kind() == constant.String {
			contents := constant.StringVal(value.Value)
			g.cur.Call(ir.ClsW, printString, g.cString(contents), g.fn.Long(int64(len(contents))))
			continue
		}

		argumentType := value.Type
		if basic, ok := argumentType.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			descriptor := g.expr(argument)
			data := g.cur.Load(ir.ClsP, descriptor)
			length := g.cur.Load(ir.ClsL, g.offset(descriptor, 8))
			g.cur.Call(ir.ClsW, printString, data, length)
			continue
		}

		class, ok := scalar(argumentType)
		if !ok {
			g.fail(argument, "unsupported print operand %s", argumentType)
			return
		}
		mode := int64(0)
		isUnsigned := false
		if _, pointer := argumentType.Underlying().(*types.Pointer); pointer {
			mode = 2
			isUnsigned = true
		} else if basic, basicOK := argumentType.Underlying().(*types.Basic); basicOK && basic.Info()&types.IsUnsigned != 0 {
			mode = 1
			isUnsigned = true
		}
		argumentValue := g.expr(argument)
		if class == ir.ClsW {
			if isUnsigned {
				argumentValue = g.cur.Extuw(ir.ClsL, argumentValue)
			} else {
				argumentValue = g.cur.Extsw(ir.ClsL, argumentValue)
			}
		}
		g.cur.Call(ir.ClsW, printInteger, argumentValue, g.fn.Long(mode))
	}
	if newline {
		g.cur.Call(ir.ClsW, printString, g.cString("\n"), g.fn.Long(1))
	}
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
			x = g.cur.Load(ir.ClsP, x)
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

// OutputName returns the conventional output stem for a source file.
func OutputName(name string) string {
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	return filepath.Base(stem)
}
