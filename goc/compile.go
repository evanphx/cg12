// Package goc implements a Go front end for cg12.
//
// Parsing and type checking are deliberately delegated to the standard
// library.  This package only translates the type-checked syntax into cg12 IR.
package goc

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/plan9asm"
)

// Compile parses and type-checks one Go source file and lowers it to cg12 IR
// for the host target.
func Compile(name string, src []byte) (*ir.Module, error) {
	return CompileFor(HostTarget(), name, src)
}

// CompileFor lowers one Go source file for a named target.
func CompileFor(target Target, name string, src []byte) (*ir.Module, error) {
	return compile(name, src, compileOptions{target: target})
}

// CompileExecutable lowers a main package together with the Go runtime needed
// to start and run it as a normal Go executable, for the host target.
func CompileExecutable(name string, src []byte) (*ir.Module, error) {
	return CompileExecutableFor(HostTarget(), name, src)
}

// CompileExecutableFor lowers a main package and the Go runtime for a named
// target.
func CompileExecutableFor(target Target, name string, src []byte) (*ir.Module, error) {
	return compile(name, src, compileOptions{target: target, executable: true})
}

// CompileExecutableWithRuntimeCoverage lowers an executable for the host target
// and instruments the build-selected runtime package. The returned metadata maps
// the binary coverage bitmap back to runtime source files and basic blocks.
func CompileExecutableWithRuntimeCoverage(name string, src []byte) (*ir.Module, *RuntimeCoverage, error) {
	return CompileExecutableWithRuntimeCoverageFor(HostTarget(), name, src)
}

// CompileExecutableWithRuntimeCoverageFor lowers an instrumented executable for
// a named target.
func CompileExecutableWithRuntimeCoverageFor(target Target, name string, src []byte) (*ir.Module, *RuntimeCoverage, error) {
	coverage := &RuntimeCoverage{}
	module, err := compile(name, src, compileOptions{
		target:          target,
		executable:      true,
		runtimeCoverage: coverage,
	})
	if err != nil {
		return nil, nil, err
	}
	return module, coverage, nil
}

func reportNoSplitViolations(module *ir.Module) {
	if os.Getenv("GOC_DEBUG_NOSPLIT") == "" {
		return
	}
	violations := opt.AuditNoSplitCalls(module)
	if len(violations) == 0 {
		fmt.Fprintln(os.Stderr, "goc: nosplit audit: no direct split callees")
		return
	}
	fmt.Fprintf(os.Stderr, "goc: nosplit audit: %d direct split callees remain\n", len(violations))
	for index, violation := range violations {
		if index >= 200 {
			fmt.Fprintf(os.Stderr, "goc: nosplit audit: ... %d more\n", len(violations)-index)
			break
		}
		fmt.Fprintf(os.Stderr, "goc: nosplit audit: %s -> %s\n", violation.Caller, violation.Callee)
	}
}

// reportEscapeDiagnostics prints goc's -m report: every allocation site in the
// compiled program, where the object went, and which rule put it there. -m on
// the command line and GOC_M in the environment turn it on, and it is off
// otherwise -- see opt.EscapeDiagLevel and docs/escape-diagnostics.md.
//
// It runs here, after opt.LowerHeapAllocations, because that is the first point
// at which both placers have decided: the AST walk recorded its placements while
// it lowered, and the IR pass has just recorded the rest.
//
// program is the file the compiler was pointed at, and the report is restricted
// to it. gc's -m reports the package being compiled; goc compiles the vendored
// standard library along with the program, and a report that included it would
// bury the lines the reader asked for under ten thousand others.
func reportEscapeDiagnostics(module *ir.Module, program string) {
	level := opt.EscapeDiagLevel()
	if level < 1 {
		return
	}
	opt.WriteEscapeDiagnostics(os.Stderr, module, program, level)
}

// reportFrameEscapes audits the finished escape decision: every allocation the
// front end or opt.LowerHeapAllocations left in a frame, checked against the
// stores the compiler actually emitted. GOC_DEBUG_ESCAPECHECK=1 enables it.
//
// It exists because a wrong "does not escape" is silent at compile time and
// arrives as a collector fault minutes later in an unrelated goroutine. The
// audit names the storing function and source line instead.
func reportFrameEscapes(module *ir.Module) {
	if os.Getenv("GOC_DEBUG_ESCAPECHECK") == "" {
		return
	}
	escapes := opt.FrameEscapes(module)
	if len(escapes) == 0 {
		fmt.Fprintln(os.Stderr, "goc: escape audit: no frame address is published past its frame")
		return
	}
	fmt.Fprintf(os.Stderr, "goc: escape audit: %d frame addresses published past their frame\n", len(escapes))
	for _, escape := range escapes {
		fmt.Fprintf(os.Stderr, "goc: escape audit: %s\n", escape)
	}
}

type compileOptions struct {
	// target is the machine being compiled for. The zero value means the host,
	// so an option struct built without thinking about the target behaves the
	// way goc did before the target existed.
	target               Target
	executable           bool
	testPackages         map[string]bool
	externalTestPackages map[string]string
	runtimeCoverage      *RuntimeCoverage
	// runtimeSplit, when set, makes this compilation one half of a driver split:
	// either the prebuilt runtime module or a program module compiled against
	// one. See runtime_split.go.
	runtimeSplit *runtimeSplit
}

func compile(name string, src []byte, options compileOptions) (*ir.Module, error) {
	target := options.target.resolve()
	if err := checkTargetTypeSizes(target); err != nil {
		return nil, err
	}
	executable := options.executable
	// A shared world carries its own FileSet, because the positions in its
	// already-parsed packages belong to it. The program has to be parsed into
	// that same set, so the world is chosen before anything is parsed rather
	// than after.
	//
	// Only executables share a world, and the reason is not performance.
	// A non-executable compile never imports the runtime -- see the guarded
	// Import below -- so its unit map is empty and nothing runtime-shaped is
	// ever generated for it. Handing it a world would put the runtime in that
	// map and the compile would then try to generate code for a package it was
	// never meant to see. Loaders configured with test packages select extra
	// files, so they do not share a world either; a key that ignored that would
	// be a key that lies.
	var world *sourceWorld
	if executable && len(options.testPackages) == 0 && len(options.externalTestPackages) == 0 {
		world = sharedSourceWorld(target, false)
	}
	fset := token.NewFileSet()
	if world != nil {
		fset = world.fset
	}
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
	loader := newSourceLoader(fset, target)
	loader.forcePureGo = !executable
	if world != nil {
		world.adopt(loader)
	}
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
	// Every package this compilation will use is loaded by now: the type checker
	// pulled the root's imports transitively and the runtime import pulled the
	// rest, and nothing below asks the loader for more.
	//
	// Both halves of a split read the closure here, at the same point, so a pack's
	// recorded closure and a program's measured one mean the same thing. It is the
	// first point at which the richest usable pack can be named, and the last point
	// before anything consults a manifest.
	if options.runtimeSplit.buildsRuntime() || options.runtimeSplit.againstRuntime() {
		closure := loadedPackagePaths(loader, pkg)
		options.runtimeSplit.closure = closure
		if options.runtimeSplit.againstRuntime() {
			if err := options.runtimeSplit.chooseManifest(closure); err != nil {
				return nil, err
			}
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
	interfaceCallWrappers := make(map[string]string)
	collectLinkNames([]*ast.File{file}, info, linkNames)
	collectGlobalLinkNames([]*ast.File{file}, pkg, globalLinkNames)
	for _, unit := range orderedUnits(loader.units) {
		collectLinkNames(unit.files, unit.info, linkNames)
		collectGlobalLinkNames(unit.files, unit.pkg, globalLinkNames)
	}
	functionDecls := collectFunctionDeclarations(info, pkg, []*ast.File{file}, loader.units)
	runtimeInits, initSymbols := runtimeInitDeclarations(loader.units)
	moduleInits := moduleInitDeclarations([]*ast.File{file}, info, pkg, loader.units, initSymbols)
	if options.runtimeSplit.buildsRuntime() {
		// The prebuilt module carries no part of any program, so it does not run
		// the root program's package init either. Listing it would leave the
		// module's inittasks pointing at a function the module does not define.
		moduleInits = dropPackageInit(moduleInits, pkg.Path())
	}
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
		assemblyReferences, err = sourceAssemblyReferences(target, loader.units)
		if err != nil {
			return nil, err
		}
	}
	dynamicTypes := collectDynamicTypes(fset, info, loader.units)
	functions, reachableGlobals := reachableFunctions(fset, roots, []*ast.File{file}, info, pkg, loader.units, dynamicTypes, compileRuntime, moduleInitFunctions, linkNames, assemblyReferences)
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
		defines := assemblyPackageDefines(target, unit.pkg)
		floatInputs, floatOutputs := assemblyPackageFloatSlots(target, unit)
		signatures := assemblyPackageSignatures(target, unit)
		for _, assembly := range unit.assembly {
			mod.Assembly = append(mod.Assembly, ir.AssemblyFile{
				PackagePath:  path,
				Path:         assembly.path,
				Source:       assembly.source,
				Defines:      defines,
				Includes:     assembly.includes,
				FloatInputs:  floatInputs,
				FloatOutputs: floatOutputs,
				Signatures:   signatures,
			})
		}
	}
	g := &gen{
		fset:                    fset,
		file:                    file,
		info:                    info,
		pkg:                     pkg,
		mod:                     mod,
		target:                  target,
		globals:                 map[types.Object]string{},
		globalLinkNames:         globalLinkNames,
		emitRuntimeTables:       emitRuntimeTables,
		runtimeAllocation:       compileRuntime,
		typeTags:                typeTags,
		functionDescriptors:     make(map[string]string),
		summaryParents:          make(map[ast.Node]map[ast.Node]ast.Node),
		literalData:             make(map[string]string),
		contentSymbols:          make(map[string]string),
		runtimeTypes:            runtimeTypes,
		goABITypes:              goABITypes,
		linkNames:               linkNames,
		initSymbols:             initSymbols,
		functionDecls:           functionDecls,
		noWriteBarrierFunctions: noWriteBarriers,
		interfaceMethods:        interfaceMethods,
		interfaceItabs:          interfaceItabs,
		interfaceCallWrappers:   interfaceCallWrappers,
		interfaceDispatchers:    make(map[string]bool),
		lastModuleSymbol:        lastModuleSymbolFor(options.runtimeSplit),
		dynamicTypes:            dynamicTypes,
		reachableGlobals:        reachableGlobals,
		reachableFunctions:      functions,
		interfaceCandidates:     make(map[interfaceCandidateKey][]interfaceMethodCandidate),
		escapeDiag:              newEscapeDiagnostics(),
	}
	g.mod.File(name)
	registerNoEscapeDirectives(g)
	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			g.globalDecl(gd)
		}
	}
	packageGlobals := map[string]map[types.Object]string{pkg.Path(): g.globals}
	for _, unit := range orderedUnits(loader.units) {
		path := unit.path
		globals := make(map[types.Object]string)
		packageGlobals[path] = globals
		if !globalPackages[path] {
			continue
		}
		generator := g.derive()
		generator.info = unit.info
		generator.pkg = unit.pkg
		// Each imported unit collects its globals into its own map; they are
		// merged into allGlobals below, once every unit has been walked.
		generator.globals = globals
		// Outside a runtime build only the globals the reachability pass kept are
		// emitted for imported packages.
		generator.filterGlobals = !compileRuntime
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
	// rootPackageFunctions are the functions a prebuilt runtime module must leave
	// undefined because they belong to whatever program is linked against it. The
	// attribution is by which declaration was being generated, not by the symbol's
	// spelling: a wrapper created while lowering the program's own code belongs to
	// the program even when its name says otherwise.
	var rootPackageFunctions []string
	for i := len(functions) - 1; i >= 0; i-- {
		function := functions[i]
		generator := g
		if function.pkg != pkg {
			// g.globals is allGlobals by now, so the derived generator resolves
			// globals across every package, as an imported function needs to.
			generator = g.derive()
			generator.info = function.info
			generator.pkg = function.pkg
		}
		generator.typeArguments = function.typeArguments
		generator.functionName = function.symbol
		emitted := len(mod.Funcs)
		generator.funcDecl(function.decl)
		if generator.err != nil {
			return nil, generator.err
		}
		if options.runtimeSplit.buildsRuntime() && function.pkg == pkg {
			for _, added := range mod.Funcs[emitted:] {
				rootPackageFunctions = append(rootPackageFunctions, added.Name)
			}
		}
	}
	addInterfaceMethodWrappers(g, functions)
	redirectedCallWrappers := redirectUnavailableInterfaceCallWrappers(mod)
	if compileRuntime {
		populateRuntimePointerTypes(fset, mod, typeTags, runtimeTypes)
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
		addInterfaceItabLinks(mod, splitItabLinks(interfaceItabs, options.runtimeSplit))
		if err := addRuntimeInitTask(mod, runtimeInits, initSymbols); err != nil {
			return nil, err
		}
		if err := addModuleInitTasks(mod, moduleInits, initSymbols, dynamicInitializers, dynamicInitializerFunctions, options.runtimeSplit); err != nil {
			return nil, err
		}
		mod.Data = append(mod.Data, &ir.Data{Name: ".goc.runtime.dataend", Align: 8, Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{0}}}})
	}
	addMemoryHelpers(mod, compileRuntime)
	mod.Runtime = compileRuntime
	// The module compiled as the program is the one that defines main, which
	// runtime.modulesinit puts at the head of activeModules.
	mod.GoHasMain = executable
	registerSymAttrs(mod)
	if compileRuntime {
		exportAssemblyReferencedFunctions(mod, assemblyReferences)
		for _, function := range mod.Funcs {
			if function.CallConv != ir.CallConvGoInternal {
				function.CallConv = ir.CallConvPlatform
			}
			function.ManagedFrame = true
		}
		opt.InlineNoSplitCalls(mod)
		reportNoSplitViolations(mod)
	}
	opt.InlineHeapAllocations(mod)
	opt.LowerHeapAllocations(mod)
	reportEscapeDiagnostics(mod, name)
	reportFrameEscapes(mod)
	if compileRuntime {
		setAAPCS64CallConvention(mod)
	}
	if err := applyNativeStdlibOverlays(mod, loader.units); err != nil {
		return nil, err
	}
	if options.runtimeCoverage != nil {
		if err := instrumentRuntimeCoverage(mod, target, loader.units["runtime"], options.runtimeCoverage, linkNames, initSymbols); err != nil {
			return nil, err
		}
	}
	if err := checkUniqueFunctionSymbols(mod); err != nil {
		return nil, err
	}
	// The split is applied last, on the finished whole-program module. Every
	// symbol it keeps is therefore exactly what a monolithic build would have
	// emitted; only the set of definitions changes.
	switch {
	case options.runtimeSplit.buildsRuntime():
		fingerprint, err := activeRuntimeSourceID(loader.units["runtime"])
		if err != nil {
			return nil, err
		}
		options.runtimeSplit.fingerprint = fingerprint
		rootPackageData := make([]string, 0, len(packageGlobals[pkg.Path()]))
		for _, symbol := range packageGlobals[pkg.Path()] {
			rootPackageData = append(rootPackageData, symbol)
		}
		for name := range g.interfaceDispatchers {
			rootPackageFunctions = append(rootPackageFunctions, name)
		}
		rootPackageFunctions = append(rootPackageFunctions, redirectedCallWrappers...)
		finishRuntimeModule(mod, options.runtimeSplit, rootPackageFunctions, rootPackageData)
	case options.runtimeSplit.againstRuntime():
		if err := finishProgramModule(mod, options.runtimeSplit, assemblyReferences); err != nil {
			return nil, err
		}
	}
	return g.mod, nil
}

// checkUniqueFunctionSymbols refuses a module in which two functions would land
// on one linker symbol.
//
// obj.prepareELF indexes symbols by name and keeps the last, so a collision is
// not a link error but a silent rebinding: every reference to the name resolves
// to whichever definition was emitted last, and the other function becomes
// unreachable code that some caller thought it was calling. Local symbols make
// it invisible to the system linker too, so nothing downstream would report it.
//
// The comparison is on the mangled spelling rather than the Go-level name,
// because the mangling is where distinct names can converge: ir.LinkerSymbol
// maps every character outside [A-Za-z0-9_] to '_'.
func checkUniqueFunctionSymbols(mod *ir.Module) error {
	byLinkerSymbol := make(map[string]string, len(mod.Funcs))
	var collisions []string
	for _, function := range mod.Funcs {
		symbol := ir.LinkerSymbol(function.Name)
		previous, seen := byLinkerSymbol[symbol]
		if !seen {
			byLinkerSymbol[symbol] = function.Name
			continue
		}
		collisions = append(collisions, fmt.Sprintf("%s and %s both compile to %s", previous, function.Name, symbol))
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Strings(collisions)
	shown := collisions
	if len(shown) > 8 {
		shown = shown[:8]
	}
	return fmt.Errorf("goc: %d function symbol collisions, so a call would bind to the wrong definition: %s",
		len(collisions), strings.Join(shown, "; "))
}

// dropPackageInit removes one package's initializer set from the module's init
// task list.
func dropPackageInit(packages []packageInit, path string) []packageInit {
	kept := make([]packageInit, 0, len(packages))
	for _, packageInitializer := range packages {
		if packageInitializer.path == path {
			continue
		}
		kept = append(kept, packageInitializer)
	}
	return kept
}

// splitItabLinks drops the static itabs a prebuilt runtime module already
// registers. runtime.itabsinit walks every module's itablinks, so an itab listed
// by both modules would be added to runtime.itabTable twice.
func splitItabLinks(itabs map[string]string, split *runtimeSplit) map[string]string {
	if !split.againstRuntime() {
		return itabs
	}
	defined := split.manifest.DefinedSet()
	kept := make(map[string]string, len(itabs))
	for key, symbol := range itabs {
		if defined[ir.LinkerSymbol(symbol)] {
			continue
		}
		kept[key] = symbol
	}
	return kept
}

// exportAssemblyReferencedFunctions keeps Go functions named by a separately
// assembled source file visible to both the linker and whole-module dead-code
// elimination. Those references do not appear as operands in the Go IR, but
// they are ordinary external roots of the completed program.
func exportAssemblyReferencedFunctions(module *ir.Module, references map[string]bool) {
	for _, function := range module.Funcs {
		if references[assemblySymbolName(function.Name)] {
			function.Export()
		}
	}
}

func setAAPCS64CallConvention(module *ir.Module) {
	for _, function := range module.Funcs {
		for _, block := range function.Blocks {
			for index := range block.Instrs {
				instruction := &block.Instrs[index]
				if instruction.Op != ir.OCall || instruction.CallConvSet {
					continue
				}
				instruction.CallConv = ir.CallConvPlatform
				instruction.CallConvSet = true
			}
		}
	}
}

func assemblyPackageDefines(target Target, pkg *types.Package) map[string]int64 {
	defines := make(map[string]int64)
	sizes := target.sizes()
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

func assemblyPackageFloatSlots(target Target, unit *sourceUnit) (map[string][]int, map[string][]int) {
	inputs := make(map[string][]int)
	outputs := make(map[string][]int)
	sizes := target.sizes()
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
		parameterOffsets, resultOffsets := assemblyABI0Offsets(parameters, results, sizes)
		for index := range parameters.Len() {
			if isAssemblyFloatType(parameters.At(index).Type()) {
				inputs[name] = append(inputs[name], int(parameterOffsets[index]))
			}
		}
		for index := range results.Len() {
			if isAssemblyFloatType(results.At(index).Type()) {
				outputs[name] = append(outputs[name], int(resultOffsets[index]))
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

func assemblyPackageSignatures(target Target, unit *sourceUnit) map[string]ir.AsmSignature {
	signatures := make(map[string]ir.AsmSignature)
	sizes := target.sizes()
	if sizes == nil {
		return signatures
	}
	for _, file := range unit.files {
		for _, declaration := range file.Decls {
			functionDeclaration, ok := declaration.(*ast.FuncDecl)
			if !ok || functionDeclaration.Recv != nil || functionDeclaration.Body != nil {
				continue
			}
			function, ok := unit.pkg.Scope().Lookup(functionDeclaration.Name.Name).(*types.Func)
			if !ok {
				continue
			}
			signature, ok := function.Type().(*types.Signature)
			if !ok || signature.Recv() != nil {
				continue
			}
			parameters := signature.Params()
			results := signature.Results()
			parameterOffsets, resultOffsets := assemblyABI0Offsets(parameters, results, sizes)
			nextGroup := 1
			var assemblySignature ir.AsmSignature
			for index := range parameters.Len() {
				variable := parameters.At(index)
				name := variable.Name()
				if name == "" {
					name = fmt.Sprintf("arg%d", index)
				}
				slots, grouped := assemblyValueSlots(name, variable.Type(), int(parameterOffsets[index]), nextGroup, sizes)
				assemblySignature.Params = append(assemblySignature.Params, slots...)
				if grouped {
					nextGroup++
				}
			}
			for index := range results.Len() {
				variable := results.At(index)
				name := variable.Name()
				if name == "" {
					name = "ret"
					if index != 0 {
						name = fmt.Sprintf("ret%d", index)
					}
				}
				slots, grouped := assemblyValueSlots(name, variable.Type(), int(resultOffsets[index]), nextGroup, sizes)
				assemblySignature.Results = append(assemblySignature.Results, slots...)
				if grouped {
					nextGroup++
				}
			}
			symbol := assemblySemanticSymbol(unit.pkg.Path(), function.Name())
			signatures[symbol] = assemblySignature
		}
	}
	return signatures
}

// assemblyABI0Offsets lays out the stack-only Go ABI. Parameters use their
// ordinary memory alignment. The result area begins at the next pointer-sized
// boundary, as required by Go's ABI0 function-argument layout.
func assemblyABI0Offsets(parameters, results *types.Tuple, sizes types.Sizes) ([]int64, []int64) {
	parameterOffsets, offset := assemblyTupleOffsets(parameters, 0, sizes)
	pointerSize := sizes.Sizeof(types.Typ[types.Uintptr])
	offset = alignAssemblyOffset(offset, pointerSize)
	resultOffsets, _ := assemblyTupleOffsets(results, offset, sizes)
	return parameterOffsets, resultOffsets
}

func assemblyTupleOffsets(tuple *types.Tuple, offset int64, sizes types.Sizes) ([]int64, int64) {
	offsets := make([]int64, tuple.Len())
	for index := range tuple.Len() {
		valueType := tuple.At(index).Type()
		offset = alignAssemblyOffset(offset, int64(sizes.Alignof(valueType)))
		offsets[index] = offset
		offset += sizes.Sizeof(valueType)
	}
	return offsets, offset
}

func alignAssemblyOffset(offset, alignment int64) int64 {
	return (offset + alignment - 1) &^ (alignment - 1)
}

func assemblyValueSlots(name string, valueType types.Type, base, group int, sizes types.Sizes) ([]ir.AsmSlot, bool) {
	valueType = types.Unalias(valueType)
	switch value := valueType.Underlying().(type) {
	case *types.Slice:
		return []ir.AsmSlot{
			{Name: name + "_base", Offset: base, Cls: ir.ClsP, Width: 8, GCRef: true, Group: group},
			{Name: name + "_len", Offset: base + 8, Cls: ir.ClsL, Width: 8, Group: group},
			{Name: name + "_cap", Offset: base + 16, Cls: ir.ClsL, Width: 8, Group: group},
		}, true
	case *types.Interface:
		return []ir.AsmSlot{
			{Name: name + "_type", Offset: base, Cls: ir.ClsP, Width: 8, GCRef: true, Group: group},
			{Name: name + "_data", Offset: base + 8, Cls: ir.ClsP, Width: 8, GCRef: true, Group: group},
		}, true
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		return []ir.AsmSlot{{Name: name, Offset: base, Cls: ir.ClsP, Width: 8, GCRef: true}}, false
	case *types.Array:
		var slots []ir.AsmSlot
		elementSize := int(sizes.Sizeof(value.Elem()))
		for index := int64(0); index < value.Len(); index++ {
			part, _ := assemblyValueSlots(fmt.Sprintf("%s_%d", name, index), value.Elem(), base+int(index)*elementSize, group, sizes)
			for slotIndex := range part {
				part[slotIndex].Group = group
			}
			slots = append(slots, part...)
		}
		return slots, true
	case *types.Struct:
		fields := make([]*types.Var, value.NumFields())
		for index := range fields {
			fields[index] = value.Field(index)
		}
		offsets := sizes.Offsetsof(fields)
		var slots []ir.AsmSlot
		for index, field := range fields {
			part, _ := assemblyValueSlots(name+"_"+field.Name(), field.Type(), base+int(offsets[index]), group, sizes)
			for slotIndex := range part {
				part[slotIndex].Group = group
			}
			slots = append(slots, part...)
		}
		return slots, true
	case *types.Basic:
		if value.Kind() == types.String || value.Kind() == types.UntypedString {
			return []ir.AsmSlot{
				{Name: name + "_base", Offset: base, Cls: ir.ClsP, Width: 8, GCRef: true, Group: group},
				{Name: name + "_len", Offset: base + 8, Cls: ir.ClsL, Width: 8, Group: group},
			}, true
		}
		class, ok := scalar(valueType)
		if !ok {
			return nil, false
		}
		width := int(sizes.Sizeof(valueType))
		gcReference := value.Kind() == types.UnsafePointer
		if gcReference {
			class = ir.ClsP
		}
		return []ir.AsmSlot{{Name: name, Offset: base, Cls: class, Width: width, GCRef: gcReference}}, false
	default:
		return nil, false
	}
}

func assemblySemanticSymbol(packagePath, name string) string {
	var symbol strings.Builder
	for _, character := range packagePath + "." + name {
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			symbol.WriteRune(character)
		} else {
			symbol.WriteByte('_')
		}
	}
	return symbol.String()
}

func sourceAssemblyReferences(target Target, units map[string]*sourceUnit) (map[string]bool, error) {
	references := make(map[string]bool)
	paths := make([]string, 0, len(units))
	for path := range units {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		unit := units[path]
		defines := assemblyPackageDefines(target, unit.pkg)
		floatInputs, floatOutputs := assemblyPackageFloatSlots(target, unit)
		for _, assembly := range unit.assembly {
			file, err := plan9asm.ParseWithOptions(strings.NewReader(assembly.source), plan9asm.ParseOptions{
				// GOARCH_* selects the architecture's arms of Go's runtime
				// assembly, so it must name the target. GOOS_* stays the host's:
				// Target carries no OS axis (see its doc comment).
				Defines: map[string]string{
					"GOARCH_" + target.GOARCH(): "1",
					"GOOS_" + runtime.GOOS:      "1",
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

func addModuleInitTasks(
	mod *ir.Module,
	packages []packageInit,
	initSymbols map[*types.Func]string,
	dynamicInitializers map[types.Object]*globalInitializer,
	dynamicInitializerFunctions map[types.Object]string,
	split *runtimeSplit,
) error {
	var backingItems []ir.DataItem
	var pointerWords []int
	// A package whose init task the prebuilt runtime module already carries is
	// initialized by that module: runtime.main walks the module chain in order and
	// the prebuilt module comes first, so its packages are initialized before this
	// one's. Listing the task again here would either point across modules or --
	// worse, if the task were re-emitted -- give one package two initTask records
	// and run its init twice, since doInit's guard is the record's own state.
	alreadyInitialized := map[string]bool{}
	if split.againstRuntime() {
		alreadyInitialized = split.manifest.DefinedSet()
	}
	for _, packageInitializer := range packages {
		dynamicSymbols := packageDynamicInitializerSymbols(
			packageInitializer.info,
			dynamicInitializers,
			dynamicInitializerFunctions,
		)
		functionCount := len(dynamicSymbols) + len(packageInitializer.declarations)
		if functionCount == 0 {
			continue
		}
		// Named for the package, not its index in this module's walk. Two
		// separately compiled modules each start that index at zero.
		taskName := contentSymbolName(".goc.module.inittask", packageInitializer.path)
		if alreadyInitialized[ir.LinkerSymbol(taskName)] {
			continue
		}
		taskItems := []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0, int64(functionCount)}}}
		for _, symbol := range dynamicSymbols {
			taskItems = append(taskItems, ir.DataItem{Sub: ir.SubL, Sym: symbol})
		}
		for _, declaration := range packageInitializer.declarations {
			object, ok := declaration.info.Defs[declaration.decl.Name].(*types.Func)
			if !ok || initSymbols[object] == "" {
				return fmt.Errorf("goc: package %s init function has no unique symbol", packageInitializer.path)
			}
			taskItems = append(taskItems, ir.DataItem{Sub: ir.SubL, Sym: initSymbols[object]})
		}
		mod.Data = append(mod.Data, &ir.Data{Name: taskName, Align: 8, Items: taskItems})
		pointerWords = append(pointerWords, len(backingItems))
		backingItems = append(backingItems, ir.DataItem{Sub: ir.SubL, Sym: taskName})
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

func packageDynamicInitializerSymbols(
	info *types.Info,
	dynamicInitializers map[types.Object]*globalInitializer,
	dynamicInitializerFunctions map[types.Object]string,
) []string {
	if info == nil {
		return nil
	}

	var symbols []string
	seen := make(map[string]bool)
	for _, initializer := range info.InitOrder {
		for _, variable := range initializer.Lhs {
			if dynamicInitializers[variable] == nil {
				continue
			}
			symbol := dynamicInitializerFunctions[variable]
			if symbol == "" || seen[symbol] {
				continue
			}
			seen[symbol] = true
			symbols = append(symbols, symbol)
		}
	}
	return symbols
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

	generated := make(map[string]bool)
	for _, method := range methods {
		signature := method.Type().(*types.Signature)
		interfaceType, ok := signature.Recv().Type().Underlying().(*types.Interface)
		if !ok {
			continue
		}
		name := g.functionSymbol(method)
		if generated[name] {
			continue
		}
		generated[name] = true
		g.interfaceDispatchers[name] = true
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
		wrapper := g.derive()
		wrapper.fn = function
		wrapper.cur = function.Entry()
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
			tagName := g.typeTags[goTypeKey(g.fset, candidate.dynamicType)]
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
				arguments = append(arguments, wrapper.embeddedInterfaceMethodReceiver(receiver, candidate.dynamicType, candidate.interfacePath, candidate.function))
				if methodHasInterfaceReceiver(candidate.function) {
					callee = function.Sym(name, 0)
					callSignature = signature
					callReceiverType = signature.Recv().Type()
				}
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
		if wrapper.runtimeAllocation {
			wrapper.cur.CallVoid(function.Sym("runtime_gocInterfaceDispatchFailure", 0), dynamicTag)
		} else {
			wrapper.cur.CallVoid(function.Sym("abort", 0))
		}
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
		key := g.functionSymbol(function) + "|" + goTypeKey(g.fset, dynamicType)
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
		if reachableMethods[function] || reachableMethods[function.Origin()] {
			if len(index) > 1 {
				add(function, dynamicType, index)
			} else {
				add(function, dynamicType, nil)
			}
			continue
		}
		candidateSignature, _ := function.Type().(*types.Signature)
		if candidateSignature != nil && candidateSignature.Recv() != nil && len(index) > 1 {
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
		left := g.functionSymbol(candidates[i].function) + "|" + goTypeKey(g.fset, candidates[i].dynamicType)
		right := g.functionSymbol(candidates[j].function) + "|" + goTypeKey(g.fset, candidates[j].dynamicType)
		return left < right
	})
	return candidates
}

func goTypeKey(fset *token.FileSet, valueType types.Type) string {
	key := types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Path()
	})
	return appendLocalTypeIdentities(fset, key, valueType)
}

func appendLocalTypeIdentities(fset *token.FileSet, key string, valueType types.Type) string {
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
			appendLocalTypeIdentity(fset, &identities, path, current.Obj())
			for index := 0; index < current.TypeArgs().Len(); index++ {
				visit(current.TypeArgs().At(index), fmt.Sprintf("%s.argument%d", path, index))
			}
		case *types.TypeParam:
			appendLocalTypeIdentity(fset, &identities, path, current.Obj())
		}
	}

	visit(valueType, "type")
	if identities.Len() == 0 {
		return key
	}
	return key + identities.String()
}

func appendLocalTypeIdentity(fset *token.FileSet, builder *strings.Builder, path string, object *types.TypeName) {
	if object == nil || object.Pkg() == nil || object.Parent() == object.Pkg().Scope() {
		return
	}
	if object.Pos() == token.NoPos {
		return
	}
	// fset.Position, not the raw token.Pos: a Pos is an offset into the whole
	// FileSet, so the same declaration has a different one depending on which
	// files were parsed before it. That made the key -- and every symbol named
	// from it -- differ between a compile that shared a preparsed standard
	// library and one that did not.
	fmt.Fprintf(builder, "|%s=%s.%s@%s", path, object.Pkg().Path(), object.Name(),
		fset.Position(object.Pos()))
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
// registerSymAttrs declares the semantic attributes of the runtime symbols the
// shared optimizer must recognize -- GC write barriers and defer registration --
// so the escape and inline passes test a declared attribute instead of sniffing
// these Go-specific names.
func registerSymAttrs(mod *ir.Module) {
	if mod.SymAttrs == nil {
		mod.SymAttrs = map[string]ir.SymAttr{}
	}
	for _, sym := range []string{"runtime.atomicstorep", "goc_storep"} {
		mod.SymAttrs[sym] |= ir.SymAtomicPointerStore
	}
	for _, sym := range []string{
		"runtime.deferproc", "runtime.deferprocStack",
		"runtime.deferprocat", "runtime.deferreturn",
	} {
		mod.SymAttrs[sym] |= ir.SymFrameScoped
	}
}

// registerNoEscapeDirectives carries //go:noescape onto the IR symbols it was
// written on.
//
// It is the one escape fact about a bodiless function that exists at all. The
// AST walk reads the directive off the declaration (parameterDoesNotEscape,
// receiverDoesNotEscape); an ir.Func with no body has no declaration and no
// code, so without this an IR-level summary would have to assume the worst
// about every one of the 619 functions in stdlib/src that carry it.
//
// The attribute is read by the summary table, which LowerHeapAllocations now
// consumes by default, so it does affect placement: a //go:noescape argument is
// one the pass will not escape a candidate for. ir.Module.SymAttrs is not
// serialised, which loses the directive rather than inventing one, so a module
// that arrives without it is answered more conservatively and never less.
func registerNoEscapeDirectives(g *gen) {
	if g.mod.SymAttrs == nil {
		g.mod.SymAttrs = map[string]ir.SymAttr{}
	}
	for function, declaration := range g.functionDecls {
		if declaration.decl == nil || declaration.decl.Body != nil {
			continue
		}
		if !hasCompilerDirective(declaration.decl, "go:noescape") {
			continue
		}
		g.mod.SymAttrs[g.functionSymbol(function)] |= ir.SymNoEscape
	}
}

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

// gen lowers one function body. Its fields fall into three groups, and keeping
// them straight is the whole reason derive exists:
//
//   - Whole-compilation state describes the compilation, not the function: the
//     target, the module under construction, the FileSet, and the module-level
//     tables every function shares. A derived generator must always see exactly
//     what its parent sees, so derive copies this group wholesale.
//
//   - Source context names which package's syntax and type information the
//     generator is reading. It is not inherited: a derived generator either
//     moves to another package (see compile) or synthesizes a body that has no
//     Go source at all, in which case it stays nil.
//
//   - Per-function state is everything about the one function being built --
//     its ir.Func, its current block, its locals, labels, defers, result shape.
//     derive resets all of it, so a derived generator can never see the
//     half-built function its parent is in the middle of.
//
// The invariant for whoever adds the next field: a new whole-compilation field
// needs nothing beyond being set on the outermost gen in compile, because
// derive carries it into all ten derived generators for free. A new source
// context or per-function field must be added to derive's reset list, or a
// wrapper will silently inherit its parent's.
//
// Reading the first group as inherited is a deliberate widening. The wrapper and
// adapter literals derive replaced listed only the tables they happened to need,
// so a synthesized wrapper used to see, say, no functionDecls and no linkNames
// while the function that created it saw both. That made a wrapper's lowering
// depend on which generator built it, which is the same class of bug as the
// missing target; the emitted IR is unchanged across goc/testdata either way.
//
// Both directions of that mistake are quiet. When target was first threaded
// through here the ten derived generators were still hand-written field by
// field, and only the outermost literal got the new field; the others received
// the zero Target and every diagnostic that names the architecture printed
// "unsupported on " instead of "unsupported on amd64". Not one test changed
// from pass to fail -- only the text of the failures did.
type gen struct {
	// Whole-compilation state. Inherited by derive.
	mod                     *ir.Module
	target                  Target
	fset                    *token.FileSet
	globals                 map[types.Object]string
	globalLinkNames         map[types.Object]string
	methodTargets           map[string]*types.Func
	functionDecls           map[*types.Func]functionDecl
	noWriteBarrierFunctions map[*types.Func]bool
	interfaceMethods        map[*types.Func]bool
	interfaceItabs          map[string]string
	interfaceCallWrappers   map[string]string
	// interfaceDispatchers names the generated interface-method dispatchers.
	// They switch over the concrete types the whole program contains, so a
	// prebuilt runtime module must leave them for the program module to define.
	interfaceDispatchers map[string]bool
	// lastModuleSymbol is the moduledata runtime.lastmoduledatap points at: the
	// tail of the module chain. Ordinarily an image is one module, so the tail is
	// also runtime.firstmoduledata. A prebuilt runtime module is followed by the
	// program module, and runtime.main stops its per-module init loop at the tail,
	// so getting this wrong means the program's package init never runs.
	lastModuleSymbol string
	dynamicTypes     []types.Type
	// reachableFunctions is the set addInterfaceMethodWrappers dispatches over,
	// kept so the escape walk can ask the same question about an interface
	// method call that the dispatcher answers about it.
	reachableFunctions []functionDecl
	// interfaceCandidates memoises interfaceMethodCandidates, which is O(dynamic
	// types x reachable declarations) and would otherwise be recomputed at every
	// interface method call the escape walk meets.
	interfaceCandidates map[interfaceCandidateKey][]interfaceMethodCandidate
	// escapeDiag is the escape diagnostic's explanation state, and is nil unless
	// -m is on. See goc/escapediag.go.
	escapeDiag        *escapeDiagnostics
	reachableGlobals  map[types.Object]bool
	filterGlobals     bool
	emitRuntimeTables bool
	runtimeAllocation bool
	typeTags          map[string]string
	// functionDescriptors dedupes static function descriptors by the symbol
	// they point at, which is also what names them. Shared with derived
	// generators, since derive copies the struct and so the map header.
	functionDescriptors map[string]string
	// summaryParents caches the parent map of each declaration a summary
	// question is asked about. The walk asks the same callee's summary once per
	// caller and once per argument position, and each question used to rebuild
	// the callee's parent map from its whole declaration -- over the corpus,
	// several times the program's own AST rebuilt to answer a few thousand
	// distinct questions. A parent map is a pure function of the syntax it was
	// built from and nothing writes into one after astParents returns, so
	// keeping it is a compile-time saving and nothing else.
	//
	// Shared with derived generators, since derive copies the struct and so the
	// map header, which is the point: the callers are in other functions and
	// often in other packages.
	summaryParents map[ast.Node]map[ast.Node]ast.Node
	// literalData interns byte-valued data symbols by their contents, so a
	// literal appearing twice is emitted once and is named the same way in
	// every module that contains it.
	literalData map[string]string
	// contentSymbols interns symbols whose name is derived from what they
	// describe, so a second request for the same content reuses the first
	// symbol instead of colliding with it.
	contentSymbols              map[string]string
	runtimeTypes                map[string]types.Type
	goABITypes                  map[string]*ir.AggType
	linkNames                   map[*types.Func]string
	initSymbols                 map[*types.Func]string
	dynamicInitializers         map[types.Object]*globalInitializer
	dynamicInitializerGuards    map[types.Object]string
	dynamicInitializerFunctions map[types.Object]string

	// Source context. Reset by derive; set by callers that lower Go source.
	file *ast.File
	info *types.Info
	pkg  *types.Package

	// Per-function state. Reset by derive.
	fn                 *ir.Func
	cur                *ir.Block
	seq                int
	err                error
	functionName       string
	currentFunction    *types.Func
	typeArguments      []types.Type
	noWriteBarrier     bool
	forceStackVariadic bool
	// variadicPayloadSlot is the reserved storage the payload about to be boxed
	// can be folded back into; nil for every conversion that is not one variadic
	// argument of a call being built. See variadicPayloadStorage.
	variadicPayloadSlot           *variadicPayloadSlot
	resultSlot                    ir.Ref
	resultType                    types.Type
	resultObjects                 map[types.Object]bool
	aggregateResult               ir.Ref
	extraResultSlots              []ir.Ref
	extraResultTypes              []types.Type
	vars                          map[types.Object]ir.Ref
	directValues                  map[types.Object]bool
	stackAddresses                map[uint32]bool
	heapCaptures                  map[types.Object]ir.Ref
	escapingCaptures              map[types.Object]bool
	iterationCaptures             map[types.Object]bool
	referenceCaptures             map[types.Object]bool
	objectEscapeChecks            map[types.Object]bool
	resultLeakBody                *ast.BlockStmt
	escapeWalkOuterObjects        []types.Object
	keepAliveObjects              map[types.Object]bool
	keepAliveValues               map[types.Object]ir.Ref
	keepAliveSlots                map[types.Object]ir.Ref
	transientInterfaceDescriptors map[uint32]bool
	initializingGlobals           map[types.Object]bool
	parents                       map[ast.Node]ast.Node
	currentBody                   *ast.BlockStmt
	breaks, continues             []*ir.Block
	labels                        map[string]*ir.Block
	labeledBreaks                 map[string]*ir.Block
	labeledContinues              map[string]*ir.Block
	deferSlots                    map[*ast.DeferStmt]ir.Ref
	deferFunctions                map[*ast.DeferStmt]ir.Ref
	deferOrder                    []*ast.DeferStmt
	deferActions                  []*ast.DeferStmt
	deferBlocks                   []*ir.Block
	runningDefers                 bool
	nextCallUsesClosure           bool
	nextCallClosure               ir.Ref
}

// derive returns a generator for another function in the same compilation: a
// wrapper, an adapter, a closure or rangefunc body, or a function from another
// package. Whole-compilation state is copied, source context and per-function
// state are reset. See the gen doc comment for the invariant this maintains.
//
// The caller installs the new function (fn, cur), the source context when there
// is any, and whatever per-function state that source implies -- typically
// functionName, currentFunction, typeArguments, noWriteBarrier and the result
// shape, all of which a nested body inherits from its enclosing function rather
// than from the compilation.
//
// TestDeriveClassifiesEveryGenField enforces the split one field at a time, so a
// gen field added without being classified fails rather than being silently
// carried or silently dropped.
func (g *gen) derive() *gen {
	derived := *g

	// Source context.
	derived.file = nil
	derived.info = nil
	derived.pkg = nil

	// The function being built, and the flags scoped to it.
	derived.fn = nil
	derived.cur = nil
	derived.seq = 0
	derived.err = nil
	derived.functionName = ""
	derived.currentFunction = nil
	derived.typeArguments = nil
	derived.noWriteBarrier = false
	derived.forceStackVariadic = false
	derived.variadicPayloadSlot = nil

	// Its result shape.
	derived.resultSlot = ir.R
	derived.resultType = nil
	derived.resultObjects = nil
	derived.aggregateResult = ir.R
	derived.extraResultSlots = nil
	derived.extraResultTypes = nil

	// Its locals. The maps that lowering writes into unconditionally are
	// allocated here; the ones lowering either creates lazily or assigns
	// wholesale (findEscapingCaptures, findKeepAliveObjects) are left nil.
	derived.vars = make(map[types.Object]ir.Ref)
	derived.directValues = make(map[types.Object]bool)
	derived.stackAddresses = make(map[uint32]bool)
	derived.heapCaptures = make(map[types.Object]ir.Ref)
	derived.initializingGlobals = make(map[types.Object]bool)
	derived.escapingCaptures = nil
	derived.iterationCaptures = nil
	derived.referenceCaptures = nil
	derived.objectEscapeChecks = nil
	derived.resultLeakBody = nil
	derived.escapeWalkOuterObjects = nil
	derived.keepAliveObjects = nil
	derived.keepAliveValues = nil
	derived.keepAliveSlots = nil
	derived.transientInterfaceDescriptors = nil

	// The syntax the function is being lowered from.
	derived.parents = nil
	derived.currentBody = nil

	// Its control flow.
	derived.breaks = nil
	derived.continues = nil
	derived.labels = make(map[string]*ir.Block)
	derived.labeledBreaks = make(map[string]*ir.Block)
	derived.labeledContinues = make(map[string]*ir.Block)

	// Its defers.
	derived.deferSlots = make(map[*ast.DeferStmt]ir.Ref)
	derived.deferFunctions = make(map[*ast.DeferStmt]ir.Ref)
	derived.deferOrder = nil
	derived.deferActions = nil
	derived.deferBlocks = nil
	derived.runningDefers = false

	// The one-shot closure hand-off between closureCall and the next call.
	derived.nextCallUsesClosure = false
	derived.nextCallClosure = ir.R

	return &derived
}

type atomicCallResult uint8

const (
	atomicReturnsValue atomicCallResult = iota
	atomicReturnsNewValue
	atomicReturnsBool
	atomicReturnsNothing
)

type atomicCallSpec struct {
	operation string
	class     ir.Cls
	result    atomicCallResult
}

func runtimeAtomicCallSpec(name string) (atomicCallSpec, bool) {
	switch name {
	case "Load", "LoadAcq", "Loadint32":
		return atomicCallSpec{operation: "load", class: ir.ClsW, result: atomicReturnsValue}, true
	case "Store", "StoreRel", "Storeint32":
		return atomicCallSpec{operation: "store", class: ir.ClsW, result: atomicReturnsNothing}, true
	case "Xchg", "Xchgint32":
		return atomicCallSpec{operation: "xchg", class: ir.ClsW, result: atomicReturnsValue}, true
	case "Cas", "CasRel", "Casint32":
		return atomicCallSpec{operation: "cas", class: ir.ClsW, result: atomicReturnsBool}, true
	case "Xadd", "Xaddint32":
		return atomicCallSpec{operation: "add", class: ir.ClsW, result: atomicReturnsNewValue}, true
	case "And":
		return atomicCallSpec{operation: "and", class: ir.ClsW, result: atomicReturnsNothing}, true
	case "Or":
		return atomicCallSpec{operation: "or", class: ir.ClsW, result: atomicReturnsNothing}, true
	case "And32":
		return atomicCallSpec{operation: "and", class: ir.ClsW, result: atomicReturnsValue}, true
	case "Or32":
		return atomicCallSpec{operation: "or", class: ir.ClsW, result: atomicReturnsValue}, true

	case "Load64", "LoadAcq64", "LoadAcquintptr", "Loaduintptr", "Loaduint", "Loadint64":
		return atomicCallSpec{operation: "load", class: ir.ClsL, result: atomicReturnsValue}, true
	case "Store64", "StoreRel64", "StoreReluintptr", "Storeint64", "Storeuintptr":
		return atomicCallSpec{operation: "store", class: ir.ClsL, result: atomicReturnsNothing}, true
	case "Xchg64", "Xchguintptr", "Xchgint64":
		return atomicCallSpec{operation: "xchg", class: ir.ClsL, result: atomicReturnsValue}, true
	case "Cas64", "Casuintptr", "Casint64":
		return atomicCallSpec{operation: "cas", class: ir.ClsL, result: atomicReturnsBool}, true
	case "Xadd64", "Xadduintptr", "Xaddint64":
		return atomicCallSpec{operation: "add", class: ir.ClsL, result: atomicReturnsNewValue}, true
	case "And64", "Anduintptr":
		return atomicCallSpec{operation: "and", class: ir.ClsL, result: atomicReturnsValue}, true
	case "Or64", "Oruintptr":
		return atomicCallSpec{operation: "or", class: ir.ClsL, result: atomicReturnsValue}, true

	case "Loadp":
		return atomicCallSpec{operation: "load", class: ir.ClsP, result: atomicReturnsValue}, true
	case "StorepNoWB":
		return atomicCallSpec{operation: "store", class: ir.ClsP, result: atomicReturnsNothing}, true
	case "Casp1":
		return atomicCallSpec{operation: "cas", class: ir.ClsP, result: atomicReturnsBool}, true
	}
	return atomicCallSpec{}, false
}

func syncAtomicCallSpec(name string) (atomicCallSpec, bool) {
	if name == "LoadPointer" {
		return atomicCallSpec{operation: "load", class: ir.ClsP, result: atomicReturnsValue}, true
	}

	class := ir.ClsW
	suffixes := []string{"Int32", "Uint32"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return syncAtomicOperationSpec(strings.TrimSuffix(name, suffix), class)
		}
	}

	class = ir.ClsL
	suffixes = []string{"Int64", "Uint64", "Uintptr"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return syncAtomicOperationSpec(strings.TrimSuffix(name, suffix), class)
		}
	}

	// StorePointer, SwapPointer, and CompareAndSwapPointer deliberately remain
	// calls. Their runtime implementations perform Go's write barrier before
	// reaching the uintptr atomic operation that is lowered here.
	return atomicCallSpec{}, false
}

func syncAtomicOperationSpec(operation string, class ir.Cls) (atomicCallSpec, bool) {
	switch operation {
	case "Load":
		return atomicCallSpec{operation: "load", class: class, result: atomicReturnsValue}, true
	case "Store":
		return atomicCallSpec{operation: "store", class: class, result: atomicReturnsNothing}, true
	case "Swap":
		return atomicCallSpec{operation: "xchg", class: class, result: atomicReturnsValue}, true
	case "CompareAndSwap":
		return atomicCallSpec{operation: "cas", class: class, result: atomicReturnsBool}, true
	case "Add":
		return atomicCallSpec{operation: "add", class: class, result: atomicReturnsNewValue}, true
	case "And":
		return atomicCallSpec{operation: "and", class: class, result: atomicReturnsValue}, true
	case "Or":
		return atomicCallSpec{operation: "or", class: class, result: atomicReturnsValue}, true
	}
	return atomicCallSpec{}, false
}

func atomicIntrinsicCallSpec(symbol string) (atomicCallSpec, bool) {
	const runtimeAtomicPrefix = "internal/runtime/atomic."
	if strings.HasPrefix(symbol, runtimeAtomicPrefix) {
		return runtimeAtomicCallSpec(strings.TrimPrefix(symbol, runtimeAtomicPrefix))
	}

	const syncAtomicPrefix = "sync/atomic."
	if strings.HasPrefix(symbol, syncAtomicPrefix) {
		return syncAtomicCallSpec(strings.TrimPrefix(symbol, syncAtomicPrefix))
	}

	return atomicCallSpec{}, false
}

func (g *gen) lowerAtomicCall(symbol string, arguments []ir.Ref) (ir.Ref, bool) {
	specification, ok := atomicIntrinsicCallSpec(symbol)
	if !ok {
		return ir.R, false
	}

	switch specification.operation {
	case "load":
		return g.cur.AtomicLoad(specification.class, arguments[0]), true
	case "store":
		g.cur.AtomicStore(specification.class, arguments[0], arguments[1])
		return g.fn.Word(0), true
	case "xchg":
		return g.cur.AtomicXchg(specification.class, arguments[0], arguments[1]), true
	case "cas":
		observed := g.cur.AtomicCAS(specification.class, arguments[0], arguments[1], arguments[2])
		swapped := g.cur.Cmp(ir.CmpEq, ir.ClsW, observed, arguments[1])
		return swapped, true
	case "add", "and", "or":
		previous := g.cur.AtomicRMW(specification.operation, specification.class, arguments[0], arguments[1])
		switch specification.result {
		case atomicReturnsNewValue:
			updated := g.cur.Add(specification.class, previous, arguments[1])
			return updated, true
		case atomicReturnsNothing:
			return g.fn.Word(0), true
		default:
			return previous, true
		}
	}

	panic("goc: unsupported atomic intrinsic operation " + specification.operation)
}

func (g *gen) lowerCompilerIntrinsicCall(symbol string, arguments []ir.Ref) (ir.Ref, bool) {
	if symbol == "crypto/internal/constanttime.boolToUint8" {
		// The standard library deliberately gives boolToUint8 a panicking body.
		// Go compilers replace the call with a zero-extending conversion. Boolean
		// values are represented in the word class here, so masking the low bit
		// gives the required canonical uint8 value without a call.
		value := g.cur.And(ir.ClsW, arguments[0], g.fn.Word(1))
		return value, true
	}

	return g.lowerAtomicCall(symbol, arguments)
}

type globalInitializer struct {
	expression    ast.Expr
	info          *types.Info
	pkg           *types.Package
	objects       []types.Object
	resultIndices []int
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
				if len(fields) < 2 || fields[0] != "go:linkname" {
					continue
				}
				if len(fields) == 2 {
					// A one-argument linkname exposes the function under its
					// ordinary linker name. Runtime assembly and compiler-generated
					// calls rely on these symbols even when Go IR has no callsite.
					names[object] = functionSymbol(object)
					continue
				}
				if len(fields) == 3 {
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
	for _, unit := range orderedUnits(units) {
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
) map[types.Object]*globalInitializer {
	initializers := make(map[types.Object]*globalInitializer)
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
					if len(values.Values) == 1 && len(values.Names) > 1 {
						initializer := &globalInitializer{
							expression: values.Values[0],
							info:       info,
							pkg:        pkg,
						}
						for index, name := range values.Names {
							object := info.Defs[name]
							if object == nil || globals[object] == "" {
								continue
							}
							initializer.objects = append(initializer.objects, object)
							initializer.resultIndices = append(initializer.resultIndices, index)
						}
						for _, object := range initializer.objects {
							initializers[object] = initializer
						}
						continue
					}
					if len(values.Values) != len(values.Names) {
						continue
					}
					for index, name := range values.Names {
						object := info.Defs[name]
						expression := values.Values[index]
						if object == nil || globals[object] == "" || info.Types[expression].Value != nil || staticallyInitializedGlobal(expression, object.Type(), info) {
							continue
						}
						initializers[object] = &globalInitializer{
							expression:    expression,
							info:          info,
							pkg:           pkg,
							objects:       []types.Object{object},
							resultIndices: []int{0},
						}
					}
				}
			}
		}
	}
	collect(rootFiles, rootInfo, rootPackage)
	for _, unit := range orderedUnits(units) {
		collect(unit.files, unit.info, unit.pkg)
	}
	return initializers
}

func dynamicInitializerFunctionSymbols(initializers map[types.Object]*globalInitializer) map[types.Object]string {
	groups := make([]*globalInitializer, 0, len(initializers))
	seen := make(map[*globalInitializer]bool)
	// Ordered by the objects being initialized, because these groups are
	// numbered in the order they are walked and that number becomes a symbol
	// name. Ranging the map directly numbered them differently on every build.
	for _, object := range sortedGlobalValues(initializers) {
		initializer := initializers[object]
		if seen[initializer] {
			continue
		}
		seen[initializer] = true
		groups = append(groups, initializer)
	}
	sort.Slice(groups, func(left, right int) bool {
		leftObject := groups[left].objects[0]
		rightObject := groups[right].objects[0]
		leftPackage := ""
		if leftObject.Pkg() != nil {
			leftPackage = leftObject.Pkg().Path()
		}
		rightPackage := ""
		if rightObject.Pkg() != nil {
			rightPackage = rightObject.Pkg().Path()
		}
		if leftPackage != rightPackage {
			return leftPackage < rightPackage
		}
		return leftObject.Name() < rightObject.Name()
	})

	symbols := make(map[types.Object]string, len(initializers))
	for index, initializer := range groups {
		object := initializer.objects[0]
		packagePath := "builtin"
		if object.Pkg() != nil {
			packagePath = object.Pkg().Path()
		}
		symbol := fmt.Sprintf(".goc.global.initfunc.%d.%s.%s", index, packagePath, object.Name())
		for _, groupObject := range initializer.objects {
			symbols[groupObject] = symbol
		}
	}
	return symbols
}

func generateDynamicInitializerFunctions(base *gen, initializers map[types.Object]*globalInitializer) error {
	groups := make([]*globalInitializer, 0, len(initializers))
	seen := make(map[*globalInitializer]bool)
	// Ordered by the objects being initialized, because these groups are
	// numbered in the order they are walked and that number becomes a symbol
	// name. Ranging the map directly numbered them differently on every build.
	for _, object := range sortedGlobalValues(initializers) {
		initializer := initializers[object]
		if seen[initializer] {
			continue
		}
		seen[initializer] = true
		groups = append(groups, initializer)
	}
	sort.Slice(groups, func(left, right int) bool {
		return base.dynamicInitializerFunctions[groups[left].objects[0]] < base.dynamicInitializerFunctions[groups[right].objects[0]]
	})

	for _, initializer := range groups {
		object := initializer.objects[0]
		generator := base.derive()
		generator.info = initializer.info
		generator.pkg = initializer.pkg
		generator.fn = base.mod.NewFuncVoid(base.dynamicInitializerFunctions[object])
		generator.cur = generator.fn.Entry()
		// The initializer expression is a bare expression rather than a body, so
		// only its parent links are needed.
		generator.parents = astParents(initializer.expression)
		generator.functionName = base.dynamicInitializerFunctions[object]

		generator.emitDynamicGlobalInitializer(initializer)
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

// declarationParents is astParents memoised per declaration. See
// gen.summaryParents for why the same declaration is asked about many times.
func (g *gen) declarationParents(declaration ast.Node) map[ast.Node]ast.Node {
	if cached, ok := g.summaryParents[declaration]; ok {
		return cached
	}
	parents := astParents(declaration)
	if g.summaryParents != nil {
		g.summaryParents[declaration] = parents
	}
	return parents
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
	saved := g.diagQuestion()
	local := g.nonEscapingAddressWithin(
		address,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
	g.diagResolve(saved, !local, nil)
	return local
}

// nonEscapingAddressWithin is nonEscapingAddress asked about a body other than
// the one being lowered. The escape walk needs the same answer this gives,
// because it is this predicate -- not the walk -- that decides whether &T{...}
// is emitted as frame storage or as a runtime.newobject, and a value placed
// inside the literal is in whichever of the two the emitter chose. Asking the
// walk's own, more precise question there instead is what left
// bigmod.Nat.Mul's T on the frame while its &Nat{limbs: T} went to the heap.
// methodCallDoesNotRetainReceiver reports that calling this method cannot make
// the receiver's storage outlive the call.
//
// An immediately called method selector used to be free: `x.m()` answered "does
// not escape" whatever m did with x. It is not free. A method is a function
// whose first argument is the receiver, and
//
//	func (b *rbox) keep() int { sink = b; return b.a }
//
// publishes it. The caller's `value := &rbox{...}` was left in the frame with a
// package-level pointer into it, and after the frame was reused the program read
// back junk where the reference implementation read back the values. gc says the
// same thing about the same function -- "leaking param: b" -- and puts the
// literal on the heap.
//
// The answer comes from receiverDoesNotEscape, which is the same walk
// parameterDoesNotEscape runs, so a method with no declaration the compiler can
// see -- an interface method, an assembly method, a generic instantiation --
// gets the conservative answer rather than a guess.

// resolvedCallee names the function a call expression calls, resolving a call
// through a function-typed local variable to the function assigned to it.
//
// The escape walk stopped at `f(arg)` where f is a `var f func(...)` -- there is
// no *types.Func on the identifier, so there was no summary to ask and the
// answer was the conservative one. gc reaches these by devirtualisation, and goc
// compiling whole-program can do at least as well: a local that is assigned once
// from a named function, never assigned again and never addressed, calls that
// function and nothing else.
//
// The three conditions are all load-bearing. More than one assignment and the
// call is not decided here; an address means some other code can assign through
// it; and a variable the walk cannot see every use of -- a parameter, a capture
// -- is assigned somewhere this body does not contain.
func (g *gen) resolvedCallee(
	fun ast.Expr,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
) *types.Func {
	if function := calledFunction(fun, info); function != nil {
		return function
	}
	identifier, isIdentifier := fun.(*ast.Ident)
	if !isIdentifier || body == nil {
		return nil
	}
	variable, isVariable := info.Uses[identifier].(*types.Var)
	if !isVariable || variable.Pkg() == nil {
		return nil
	}
	if _, global := g.globals[variable]; global {
		return nil
	}
	// Declared inside this body, not merely visible from it. A *parameter* of
	// function type passes escapeWalkSeesEveryUse -- signatureVariables puts it in
	// escapeWalkOuterObjects -- and its first value comes from the caller, so
	// `func f(g func()) { g(); g = h }` has exactly one assignment in the body and
	// the call is not to h.
	if variable.Pos() < body.Pos() || variable.Pos() >= body.End() {
		return nil
	}
	var assigned ast.Expr
	assignments := 0
	addressed := false
	ast.Inspect(body, func(node ast.Node) bool {
		other, ok := node.(*ast.Ident)
		if !ok || (info.Uses[other] != variable && info.Defs[other] != variable) {
			return true
		}
		switch parent := parents[other].(type) {
		case *ast.AssignStmt:
			for index, target := range parent.Lhs {
				if target != other {
					continue
				}
				assignments++
				if len(parent.Rhs) == len(parent.Lhs) {
					assigned = parent.Rhs[index]
				} else {
					assigned = nil
				}
			}
		case *ast.ValueSpec:
			for index, name := range parent.Names {
				if name != other {
					continue
				}
				assignments++
				if index < len(parent.Values) {
					assigned = parent.Values[index]
				} else {
					// `var f func()` with no value is the nil function, and a call
					// through it panics rather than reaching anything. Counting it
					// as an assignment keeps the "exactly one" test honest.
					assigned = nil
				}
			}
		case *ast.UnaryExpr:
			if parent.Op == token.AND {
				addressed = true
			}
		}
		return true
	})
	if addressed || assignments != 1 || assigned == nil {
		return nil
	}
	return calledFunction(assigned, info)
}

func (g *gen) methodCallDoesNotRetainReceiver(
	selector *ast.SelectorExpr,
	info *types.Info,
	checking map[parameterKey]bool,
) bool {
	method, ok := info.Uses[selector.Sel].(*types.Func)
	if !ok {
		return false
	}
	// The interface the *call site* names, which is narrower than the one the
	// method is declared on whenever the declaration is an embedded interface:
	// heap.Interface embeds sort.Interface, so `h.Less(...)` inside container/heap
	// resolves to (sort.Interface).Less and asking about sort.Interface drags in
	// every sorter in the program. The dynamic type has to implement the static
	// type, so narrowing to it is sound and strictly smaller.
	if static, isInterface := receiverInterfaceAtCallSite(selector, info); isInterface {
		return g.interfaceMethodDoesNotRetainReceiver(method, static, checking)
	}
	return g.methodDoesNotRetainReceiver(method, checking)
}

// receiverInterfaceAtCallSite names the interface type of the expression a
// method is being called on, for a call through an interface value.
func receiverInterfaceAtCallSite(selector *ast.SelectorExpr, info *types.Info) (*types.Interface, bool) {
	receiverType := info.TypeOf(selector.X)
	if receiverType == nil {
		return nil, false
	}
	interfaceType, isInterface := receiverType.Underlying().(*types.Interface)
	return interfaceType, isInterface
}

// methodDoesNotRetainReceiver is methodCallDoesNotRetainReceiver with the method
// already resolved, so that an interface implementation which is itself an
// embedded interface method can be asked the same question recursively.
// sort.reverse is that shape -- struct{ Interface } -- and its Len is the
// embedded interface's, which has no body to walk.
func (g *gen) methodDoesNotRetainReceiver(method *types.Func, checking map[parameterKey]bool) bool {
	if interfaceType, isInterfaceMethod := methodsInterface(method); isInterfaceMethod {
		return g.interfaceMethodDoesNotRetainReceiver(method, interfaceType, checking)
	}
	if receiverIsAPointerFreeCopy(method) {
		// The callee is handed a copy of a value that holds no reference to
		// anything, so there is nothing about the caller's storage for it to
		// retain and no body to walk. Without this, `t.Sub(u)` on a time.Time and
		// every other pointer-free value method fell to the conservative answer.
		return true
	}
	return g.receiverDoesNotEscape(method, checking)
}

// methodsInterface reports the interface a method is declared on, for a method
// reached through an interface value rather than through a concrete type.
func methodsInterface(method *types.Func) (*types.Interface, bool) {
	signature, ok := method.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return nil, false
	}
	interfaceType, isInterface := signature.Recv().Type().Underlying().(*types.Interface)
	return interfaceType, isInterface
}

// interfaceMethodDoesNotRetainReceiver answers "does calling this interface
// method retain the receiver" by asking every implementation the program can
// dispatch to.
//
// This is devirtualisation, and it is the one place in the escape walk where
// goc can be *more* precise than cmd/compile rather than less: gc devirtualises
// a call whose concrete type it can see in one function, while goc compiles the
// whole program and knows the complete set of types that can reach any interface.
//
// The set is interfaceMethodCandidates', which is the same set
// addInterfaceMethodWrappers builds the dispatcher out of. A type missing from
// it does not merely get a conservative escape answer -- it reaches
// runtime_gocInterfaceDispatchFailure and the program stops -- so believing it
// here adds no failure mode the dispatcher does not already have.
//
// An empty candidate list is answered "retains", not vacuously "does not": an
// interface the walk could find no implementation of is a question this has no
// information about, and the conservative answer is the one that keeps the
// object on the heap.
func (g *gen) interfaceMethodDoesNotRetainReceiver(
	method *types.Func,
	interfaceType *types.Interface,
	checking map[parameterKey]bool,
) bool {
	// An interface method has no body, so receiverDoesNotEscape's cycle-breaking
	// entry is never reached for one and a chain of embedded interfaces would
	// recurse without end. This is that entry, taken on the interface method
	// itself.
	key := parameterKey{function: method, index: -1}
	if checking[key] {
		return false
	}
	checking[key] = true
	defer delete(checking, key)

	cacheKey := interfaceCandidateKey{method: method, interfaceType: interfaceType.String()}
	candidates, cached := g.interfaceCandidates[cacheKey]
	if !cached {
		candidates = interfaceMethodCandidates(g, g.reachableFunctions, method, interfaceType)
		g.interfaceCandidates[cacheKey] = candidates
	}
	if escapeDebugLevel() >= 2 {
		fmt.Fprintf(os.Stderr, "escape devirt: %s on %s -> %d candidates\n", method.FullName(), interfaceType.String(), len(candidates))
	}
	if len(candidates) == 0 {
		return false
	}
	for _, candidate := range candidates {
		if !g.methodDoesNotRetainReceiver(candidate.function, checking) {
			return false
		}
	}
	return true
}

// interfaceCandidateKey names one devirtualisation question: a method, and the
// interface the call site named. The same method asked about two interfaces has
// two answers, because the narrower interface has fewer implementations.
type interfaceCandidateKey struct {
	method        *types.Func
	interfaceType string
}

// receiverIsAPointerFreeCopy reports that this method's receiver is passed by
// value and carries no pointer, so the callee cannot reach the caller's storage
// through it however it is used.
func receiverIsAPointerFreeCopy(method *types.Func) bool {
	signature, ok := method.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return false
	}
	receiverType := signature.Recv().Type()
	if _, isPointer := receiverType.Underlying().(*types.Pointer); isPointer {
		return false
	}
	if _, isInterface := receiverType.Underlying().(*types.Interface); isInterface {
		return false
	}
	pointers := false
	visitPointerWords(receiverType, 0, func(int64) { pointers = true })
	return !pointers
}

func (g *gen) nonEscapingAddressWithin(
	address *ast.UnaryExpr,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	var current ast.Node = address
	for {
		// Asked at every step rather than only of the address itself, because the
		// climb below turns the address into the expression that contains it and
		// the containing expression is the one an assignment sees. `value :=
		// scorer(&scoreBox{...})` climbs the conversion and then meets the
		// assignment; without the retry the assignment was the loop's default case
		// and every literal converted to an interface on the way into a local went
		// to the heap.
		if g.assignedNodeDoesNotEscapeWithin(current, info, parents, body, checking) {
			return true
		}
		parent := parents[current]
		switch parent := parent.(type) {
		case *ast.ParenExpr:
			current = parent
		case *ast.CallExpr:
			if len(parent.Args) != 1 || parent.Args[0] != current || !info.Types[parent.Fun].IsType() {
				return false
			}
			current = parent
		case *ast.StarExpr:
			return parent.X == current
		case *ast.SelectorExpr:
			selection := info.Selections[parent]
			return parent.X == current && selection != nil && selection.Kind() == types.FieldVal
		case *ast.KeyValueExpr:
			if parent.Value != current {
				return false
			}
			current = parent
		case *ast.CompositeLit:
			// The pointer is written into the literal's storage, so the object
			// it points at has to outlive whatever that storage is and nothing
			// more. compositeElementDoesNotEscape hands the question on only
			// for a struct or array literal, where the literal's value *is* the
			// storage; a slice or map literal has backing storage of its own
			// whose placement is decided at its own site.
			//
			// This is the `root := &item{index: i, next: &item{index: i + 1}}`
			// shape. Without it the inner address had no case here and took the
			// conservative answer, so a linked pair that gc keeps entirely in
			// the frame cost one allocation per link.
			return g.compositeElementDoesNotEscape(parent, info, parents, body, checking)
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
	saved := g.diagQuestion()
	escapes := g.addressEscapesWithin(
		address,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
	g.diagResolve(saved, escapes, nil)
	return escapes
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
		if g.boxedIntoInterface(current, info, parents) {
			return true
		}
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
			function := g.resolvedCallee(parent.Fun, info, parents, body)
			if function == nil {
				g.diagRule(func() string {
					return "passed to a call the walk cannot resolve to a single function"
				})
				g.diagUse(parent)
				return true
			}
			if g.parameterDoesNotEscape(function, argumentIndex, checking) {
				return false
			}
			if !g.parameterLeaksOnlyToResult(function, argumentIndex, checking) {
				g.diagRule(func() string {
					return fmt.Sprintf("passed to %s, which may retain argument %d", function.FullName(), argumentIndex)
				})
				g.diagUse(parent)
				return true
			}
			if !singleResultFunction(function) {
				return !g.leakedCallResultDoesNotEscape(parent, info, parents, body, checking)
			}
			current = parent
		case *ast.AssignStmt:
			return !g.assignedNodeDoesNotEscapeWithin(current, info, parents, body, checking)
		case *ast.ValueSpec:
			valueIndex := -1
			for index, value := range parent.Values {
				if value == current {
					valueIndex = index
					break
				}
			}
			if valueIndex < 0 || valueIndex >= len(parent.Names) {
				return true
			}
			object := info.Defs[parent.Names[valueIndex]]
			if object == nil || body == nil {
				return true
			}
			return !g.objectDoesNotEscape(object, info, parents, body, checking)
		case *ast.ReturnStmt:
			allowed := g.resultLeakIsAllowed(parent, parents)
			if !allowed {
				g.diagRule(func() string { return "returned" })
				g.diagUse(parent)
			}
			return !allowed
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
	saved := g.diagQuestion()
	local := g.assignedNodeDoesNotEscapeWithin(
		expression,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
	g.diagResolve(saved, !local, nil)
	return local
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
	destinations, ok := assignmentDestinations(assignment, expression)
	if !ok {
		return false
	}
	for _, destination := range destinations {
		if !g.destinationDoesNotEscape(destination, info, parents, body, checking) {
			return false
		}
	}
	return true
}

// assignmentDestinations names the left-hand sides one right-hand side value
// can reach. A positional assignment gives one; a single right-hand side spread
// across several left-hand sides -- d, s := formatBits(...) -- gives all of
// them, because the walk cannot tell which result carried the storage.
func assignmentDestinations(assignment *ast.AssignStmt, expression ast.Node) ([]ast.Expr, bool) {
	if len(assignment.Rhs) == 1 && assignment.Rhs[0] == expression {
		return assignment.Lhs, true
	}
	for index, rightHandSide := range assignment.Rhs {
		if rightHandSide != expression {
			continue
		}
		if index >= len(assignment.Lhs) {
			return nil, false
		}
		return assignment.Lhs[index : index+1], true
	}
	return nil, false
}

func (g *gen) destinationDoesNotEscape(
	destination ast.Expr,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	identifier, ok := destination.(*ast.Ident)
	if !ok {
		return false
	}
	if identifier.Name == "_" {
		// The blank identifier discards the value, so nothing reaches it.
		return true
	}
	object := info.Defs[identifier]
	if object == nil {
		object = info.Uses[identifier]
	}
	if object == nil || object.Pkg() == nil || body == nil {
		return false
	}
	if _, global := g.globals[object]; global {
		return false
	}
	if g.resultObjects[object] {
		return false
	}
	if g.objectEscapeChecks[object] {
		// The destination is the variable whose uses this walk is already
		// enumerating, as in dst = append(dst, b). Assigning a value derived
		// from it back into it opens no route the running enumeration will not
		// see, so answering "does not escape" here does not weaken the answer
		// that walk will give. Answering "escapes" instead would make every
		// accumulate-into-your-own-parameter loop opaque.
		return true
	}
	return g.objectDoesNotEscape(object, info, parents, body, checking)
}

// valueDoesNotEscape recognizes temporary values whose backing storage is
// consumed entirely within the current function. This is particularly
// important for slice literals used directly by range: their backing arrays
// are ordinary stack temporaries in Go, and allocating one on every range
// iteration can make runtime code allocate while scanning the stack.
func (g *gen) valueDoesNotEscape(expression ast.Expr) bool {
	saved := g.diagQuestion()
	local := g.valueDoesNotEscapeWithin(
		expression,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
	g.diagResolve(saved, !local, nil)
	return local
}

// conversionCopiesOperand reports whether a type conversion builds its result
// out of storage of its own, so that the operand's storage cannot be reached
// from the result and the operand does not escape through the conversion.
//
// Only the conversions between a string and a slice of bytes or runes do that.
// Every other conversion in Go either reinterprets the operand's own storage --
// T(s) where T and the operand share an underlying type hands back the same
// backing array -- or carries no storage at all, and the walk has to keep
// asking about the result.
//
// slicebytetostring and stringtoslicebyte copy. The slicebytetostringtmp form
// does alias the operand, and sliceString picks it only where the string cannot
// outlive the conversion's own use -- a comparison, a len, or an argument to a
// callee whose parameter provably does not escape -- so the operand's storage
// is bounded there too. Without this rule every `string(buffer)` in a
// comparison forced buffer's backing array onto the heap: the walk asked
// whether the *string* escaped, which is not the question.
func conversionCopiesOperand(call *ast.CallExpr, operand ast.Expr, info *types.Info) bool {
	if len(call.Args) != 1 || call.Args[0] != operand || !info.Types[call.Fun].IsType() {
		return false
	}
	sourceType, targetType := info.TypeOf(operand), info.TypeOf(call)
	if sourceType == nil || targetType == nil {
		return false
	}
	return stringToCharacterSlice(sourceType, targetType) || stringToCharacterSlice(targetType, sourceType)
}

// stringToCharacterSlice reports that converting from stringType to sliceType
// is the string-to-[]byte or string-to-[]rune conversion, which the runtime
// performs by copying.
func stringToCharacterSlice(stringType, sliceType types.Type) bool {
	basic, isBasic := stringType.Underlying().(*types.Basic)
	if !isBasic || basic.Kind() != types.String {
		return false
	}
	slice, isSlice := sliceType.Underlying().(*types.Slice)
	if !isSlice {
		return false
	}
	element, isElementBasic := slice.Elem().Underlying().(*types.Basic)
	return isElementBasic && (element.Kind() == types.Uint8 || element.Kind() == types.Int32)
}

func (g *gen) valueDoesNotEscapeWithin(
	expression ast.Expr,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	var current ast.Node = expression
	for {
		if g.boxedIntoInterface(current, info, parents) {
			return false
		}
		if g.assignedNodeDoesNotEscapeWithin(current, info, parents, body, checking) {
			return true
		}
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
		case *ast.UnaryExpr:
			if parent.Op != token.AND || parent.X != current {
				return false
			}
			if g.resultLeakBody != nil {
				// Taking an address makes fresh storage, and returning that
				// address puts the storage in the heap. A value placed inside
				// it therefore does not merely leak to the result -- it is in
				// the heap the moment the function returns, and it cannot live
				// in the caller's frame. slog.NewTextHandler's
				// &TextHandler{&commonHandler{w: w}} is that shape, and letting
				// the result rule apply through it left a caller's bytes.Buffer
				// on the frame with a heap handler pointing at it.
				//
				// This test comes first deliberately. The composite-literal
				// rule below is allowed to answer "does not escape", and in a
				// summary walk that answer would be reached through a local the
				// callee returns -- which is the hole above, reopened.
				return false
			}
			if _, isLiteral := parent.X.(*ast.CompositeLit); isLiteral {
				// &T{v} makes storage of its own and copies v into it, so v is
				// wherever that storage is. The emitter places it with
				// nonEscapingAddress, so this walk has to ask that same
				// question rather than continue climbing: continuing asked
				// whether the *pointer* escapes, which for
				// x.Mod(&Nat{limbs: T}, m) answered "no" while the emitter,
				// whose walk accepts only a one-argument type conversion above
				// an address, put the Nat in the heap and the barrier put T's
				// frame backing array in it.
				return g.nonEscapingAddressWithin(parent, info, parents, body, checking)
			}
			current = parent
		case *ast.SliceExpr:
			if parent.X != current {
				return false
			}
			current = parent
		case *ast.TypeAssertExpr:
			// i.(T) reads the interface's payload, so the value the walk is
			// following is reachable from the asserted value and from nothing
			// else. Keep climbing. Without this the walk stopped at the
			// conservative default, which is how a local interface that is only
			// ever asserted and read still put its payload on the heap.
			if parent.X != current {
				return false
			}
			current = parent
		case *ast.SelectorExpr:
			// The same rule nonEscapingObjectUse applies to x.f: reading a field
			// copies it out and carries nothing, while taking the field's
			// address makes an interior pointer that keeps the whole object
			// alive, so the object escapes exactly when that pointer does. A
			// method value is a closure over the receiver and is answered by
			// asking both what the method does with the receiver and where the
			// closure goes; an immediate call asks only the first.
			if parent.X != current {
				return false
			}
			selection := info.Selections[parent]
			if selection == nil {
				return true
			}
			if selection.Kind() == types.FieldVal {
				address := addressedExpression(parent, parents, info)
				return address == nil || !g.addressEscapesWithin(address, info, parents, body, checking)
			}
			if !g.methodCallDoesNotRetainReceiver(parent, info, checking) {
				return false
			}
			if call, calledImmediately := parents[parent].(*ast.CallExpr); calledImmediately && call.Fun == parent {
				return true
			}
			// See nonEscapingObjectUse: a method value is a closure over the
			// receiver, and the receiver goes where the closure goes.
			current = parent
		case *ast.RangeStmt:
			return parent.X == current
		case *ast.ReturnStmt:
			return g.resultLeakIsAllowed(parent, parents)
		case *ast.CallExpr:
			argumentIndex := -1
			for index, argument := range parent.Args {
				if argument == current {
					argumentIndex = index
					break
				}
			}
			if argumentIndex < 0 {
				if parent.Fun == current {
					return g.deferredFunctionValueStaysInFrame(parent, parents, body)
				}
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
			if appendDestination(parent, current, info) {
				current = parent
				continue
			}
			if appendSpreadSource(parent, current, info) {
				return true
			}
			if info.Types[parent.Fun].IsType() {
				if expression, isExpression := current.(ast.Expr); isExpression &&
					conversionCopiesOperand(parent, expression, info) {
					return true
				}
				current = parent
				continue
			}
			function := g.resolvedCallee(parent.Fun, info, parents, body)
			if function == nil {
				return false
			}
			if g.parameterDoesNotEscape(function, argumentIndex, checking) {
				return true
			}
			if !g.parameterLeaksOnlyToResult(function, argumentIndex, checking) {
				return false
			}
			if !singleResultFunction(function) {
				return g.leakedCallResultDoesNotEscape(parent, info, parents, body, checking)
			}
			current = parent
		default:
			return false
		}
	}
}

type parameterKey struct {
	function *types.Func
	index    int
	// summary distinguishes the two questions asked about the same parameter:
	// "does it escape at all" and "does it escape anywhere but the function's
	// own result". They recurse into each other, so they must not share a
	// cycle-breaking entry.
	summary bool
}

// resultLeakIsAllowed reports whether a return statement the escape walk has
// reached returns from the function whose "leaks only to result" summary is
// being computed. A return inside a nested function literal returns from the
// literal, so it is not that function's result and is not allowed.
func (g *gen) resultLeakIsAllowed(returnStatement *ast.ReturnStmt, parents map[ast.Node]ast.Node) bool {
	if g.resultLeakBody == nil {
		return false
	}
	var current ast.Node = returnStatement
	for {
		if block, isBlock := current.(*ast.BlockStmt); isBlock && block == g.resultLeakBody {
			return true
		}
		if _, insideLiteral := current.(*ast.FuncLit); insideLiteral {
			return false
		}
		parent, ok := parents[current]
		if !ok || parent == nil {
			return false
		}
		current = parent
	}
}

// leakedCallResultDoesNotEscape decides an argument that leaked only into the
// results of a call with more than one result. The walk cannot continue from
// such a call the way it continues from a single-valued one, because the call
// expression does not stand for one value; the only shape it accepts is the
// call being the whole right-hand side of an assignment, where every left-hand
// side is a destination it can check.
func (g *gen) leakedCallResultDoesNotEscape(
	call *ast.CallExpr,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	assignment, ok := parents[call].(*ast.AssignStmt)
	if !ok || len(assignment.Rhs) != 1 || assignment.Rhs[0] != call {
		return false
	}
	return g.assignedNodeDoesNotEscapeWithin(call, info, parents, body, checking)
}

func singleResultFunction(function *types.Func) bool {
	return function.Type().(*types.Signature).Results().Len() == 1
}

// appendSpreadSource reports that an argument is the `xs...` operand of an
// append: the elements are copied out of it into the destination's backing
// array, so the operand's own storage is not retained by the call. What the
// elements *point at* does become reachable from the destination, but that is a
// question about those objects and not about this one.
func appendSpreadSource(call *ast.CallExpr, argument ast.Node, info *types.Info) bool {
	if call.Ellipsis == token.NoPos || len(call.Args) != 2 || call.Args[1] != argument {
		return false
	}
	identifier, isIdentifier := call.Fun.(*ast.Ident)
	if !isIdentifier {
		return false
	}
	builtin, isBuiltin := info.Uses[identifier].(*types.Builtin)
	return isBuiltin && builtin.Name() == "append"
}

// appendDestination reports that expression is the destination slice of an
// append, whose backing storage the result may alias. Only argument zero
// qualifies: append copies the destination's *contents* into the result and
// never publishes the destination's address, so the destination's storage
// escapes exactly when the result does. The appended elements are the opposite
// case -- they are stored into storage that may already be reachable from the
// heap -- so they keep the conservative answer.
func appendDestination(call *ast.CallExpr, expression ast.Node, info *types.Info) bool {
	identifier, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	builtin, ok := info.Uses[identifier].(*types.Builtin)
	if !ok || builtin.Name() != "append" {
		return false
	}
	return len(call.Args) != 0 && call.Args[0] == expression
}

// boxedIntoInterface reports whether a value flowing out of expression is
// converted to an interface type there, which copies it into freshly allocated
// storage that is not part of this frame.
//
// This is a publication and the escape walk has to stop at it.
// adaptValueToInterface allocates the interface payload with allocateTyped --
// a runtime.newobject candidate -- for every source type that is not
// pointer-shaped, and stores the value into it through the write barrier. Every
// pointer inside the value, including a slice header's backing-array pointer,
// is in the heap from that instruction on, whatever the receiving function does
// with the interface afterwards.
//
// runtime.KeepAlive(values) is the shape that made this matter. The walk asked
// only whether KeepAlive lets its parameter escape -- it does not -- and left
// the backing array of make([]*record, 0, 4) on main's frame, while the caller
// boxed the slice header into a runtime.newobject one instruction earlier.
//
// Pointer-shaped source types are excluded because isDirectInterfaceType makes
// adaptValueToInterface store them straight into the two-word descriptor, which
// is an ordinary frame allocation: no fresh storage is made, so the walk should
// carry on through the conversion rather than stop.
func (g *gen) boxedIntoInterface(node ast.Node, info *types.Info, parents map[ast.Node]ast.Node) bool {
	expression, isExpression := node.(ast.Expr)
	if !isExpression {
		return false
	}
	sourceType := info.TypeOf(expression)
	if sourceType == nil || !interfaceConversionAllocates(sourceType) {
		return false
	}
	targetType := interfaceConversionTarget(expression, info, parents)
	if targetType == nil {
		return false
	}
	if isSharedTypeParameter(targetType) {
		// Shared generic code represents an unconstrained type parameter as one
		// pointer-sized value; the constraint interface is not its runtime
		// representation, so nothing is boxed. assignmentValue agrees.
		return false
	}
	_, isInterface := targetType.Underlying().(*types.Interface)
	if isInterface {
		// Every caller of this predicate treats true as "escapes", so this is a
		// deciding branch and names itself as one. See goc/escapediag.go.
		g.diagRule(func() string {
			return fmt.Sprintf("converted to %s, and boxing a %s makes fresh storage for the payload",
				targetType, sourceType)
		})
		g.diagUse(expression)
	}
	return isInterface
}

// interfaceConversionAllocates reports whether converting a value of this type
// to an interface makes fresh storage for the payload.
func interfaceConversionAllocates(sourceType types.Type) bool {
	if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
		return false
	}
	if isSharedTypeParameter(sourceType) {
		return false
	}
	if basic, ok := sourceType.Underlying().(*types.Basic); ok && basic.Kind() == types.UntypedNil {
		return false
	}
	return !isDirectInterfaceType(sourceType)
}

// interfaceConversionTarget names the type an expression's value is converted
// to by the context it sits in, for the contexts that perform an assignment
// conversion. It returns nil where there is no conversion, or where the context
// is one the escape walk already answers conservatively, so a nil answer never
// weakens the walk.
func interfaceConversionTarget(expression ast.Expr, info *types.Info, parents map[ast.Node]ast.Node) types.Type {
	switch parent := parents[expression].(type) {
	case *ast.CallExpr:
		return callArgumentTarget(parent, expression, info)
	case *ast.AssignStmt:
		for index, rightHandSide := range parent.Rhs {
			if rightHandSide != expression {
				continue
			}
			if index >= len(parent.Lhs) {
				return nil
			}
			return assignedTargetType(parent.Lhs[index], info)
		}
		return nil
	case *ast.ValueSpec:
		for index, value := range parent.Values {
			if value != expression {
				continue
			}
			if index >= len(parent.Names) {
				return nil
			}
			return assignedTargetType(parent.Names[index], info)
		}
		return nil
	case *ast.ReturnStmt:
		return returnedResultType(parent, expression, info, parents)
	case *ast.SendStmt:
		if parent.Value != expression {
			return nil
		}
		channelType, ok := info.TypeOf(parent.Chan).Underlying().(*types.Chan)
		if !ok {
			return nil
		}
		return channelType.Elem()
	case *ast.KeyValueExpr:
		literal, inLiteral := parents[parent].(*ast.CompositeLit)
		if !inLiteral {
			return nil
		}
		return compositeElementTarget(literal, parent, expression, info)
	case *ast.CompositeLit:
		return compositeElementTarget(parent, expression, expression, info)
	}
	return nil
}

// assignedTargetType is the type of an assignment's left-hand side, with the
// blank identifier reported as no destination at all.
func assignedTargetType(destination ast.Expr, info *types.Info) types.Type {
	if identifier, ok := destination.(*ast.Ident); ok && identifier.Name == "_" {
		return nil
	}
	return info.TypeOf(destination)
}

// callArgumentTarget names the type a call converts one argument to: the
// converted-to type for a type conversion, and the parameter's type for a call.
// Builtins are reported as no conversion except panic, which does box its
// operand.
func callArgumentTarget(call *ast.CallExpr, argument ast.Expr, info *types.Info) types.Type {
	if info.Types[call.Fun].IsType() {
		if len(call.Args) != 1 || call.Args[0] != argument {
			return nil
		}
		return info.TypeOf(call)
	}
	index := -1
	for position, other := range call.Args {
		if other == argument {
			index = position
			break
		}
	}
	if index < 0 {
		return nil
	}
	if identifier, ok := call.Fun.(*ast.Ident); ok {
		if builtin, isBuiltin := info.Uses[identifier].(*types.Builtin); isBuiltin {
			if builtin.Name() == "panic" {
				// panic's operand is an `any`, boxed exactly like any other
				// interface argument. The rest of the builtins either do not
				// take a value at all or take it without conversion.
				anyType := types.NewInterfaceType(nil, nil)
				anyType.Complete()
				return anyType
			}
			return nil
		}
	}
	functionType := info.TypeOf(call.Fun)
	if functionType == nil {
		return nil
	}
	signature, ok := functionType.Underlying().(*types.Signature)
	if !ok {
		return nil
	}
	parameters := signature.Params()
	last := parameters.Len() - 1
	if last < 0 {
		return nil
	}
	if !signature.Variadic() || index < last {
		if index >= parameters.Len() {
			return nil
		}
		return parameters.At(index).Type()
	}
	if call.Ellipsis.IsValid() {
		// f(values...) passes the slice itself; nothing is converted per element.
		return nil
	}
	slice, isSlice := parameters.At(last).Type().Underlying().(*types.Slice)
	if !isSlice {
		return nil
	}
	return slice.Elem()
}

// compositeElementTarget names the type a composite literal converts one
// element to. element is the whole element -- the key-value pair for a keyed
// literal -- and value is the expression whose target is wanted.
func compositeElementTarget(literal *ast.CompositeLit, element ast.Expr, value ast.Expr, info *types.Info) types.Type {
	literalType := info.TypeOf(literal)
	if literalType == nil {
		return nil
	}
	switch underlying := literalType.Underlying().(type) {
	case *types.Slice:
		return underlying.Elem()
	case *types.Array:
		return underlying.Elem()
	case *types.Map:
		pair, keyed := element.(*ast.KeyValueExpr)
		if keyed && pair.Key == value {
			return underlying.Key()
		}
		return underlying.Elem()
	case *types.Struct:
		if pair, keyed := element.(*ast.KeyValueExpr); keyed {
			name, ok := pair.Key.(*ast.Ident)
			if !ok {
				return nil
			}
			for index := 0; index < underlying.NumFields(); index++ {
				if underlying.Field(index).Name() == name.Name {
					return underlying.Field(index).Type()
				}
			}
			return nil
		}
		for index, other := range literal.Elts {
			if other == element && index < underlying.NumFields() {
				return underlying.Field(index).Type()
			}
		}
		return nil
	}
	return nil
}

// returnedResultType names the result a returned expression is assigned to. A
// return whose expression count does not match the signature is returning a
// multi-valued call, which stands for every result at once and is reported as
// no conversion.
func returnedResultType(statement *ast.ReturnStmt, expression ast.Expr, info *types.Info, parents map[ast.Node]ast.Node) types.Type {
	index := -1
	for position, result := range statement.Results {
		if result == expression {
			index = position
			break
		}
	}
	if index < 0 {
		return nil
	}
	signature := enclosingSignature(statement, info, parents)
	if signature == nil || signature.Results().Len() != len(statement.Results) {
		return nil
	}
	return signature.Results().At(index).Type()
}

// enclosingSignature is the signature of the function whose body a node sits
// in, which for a return statement is the function it returns from.
func enclosingSignature(node ast.Node, info *types.Info, parents map[ast.Node]ast.Node) *types.Signature {
	for current := ast.Node(node); current != nil; current = parents[current] {
		switch declaration := current.(type) {
		case *ast.FuncLit:
			signature, _ := info.TypeOf(declaration).(*types.Signature)
			return signature
		case *ast.FuncDecl:
			function, _ := info.Defs[declaration.Name].(*types.Func)
			if function == nil {
				return nil
			}
			signature, _ := function.Type().(*types.Signature)
			return signature
		}
	}
	return nil
}

// escapeWalkSeesEveryUse reports whether every use of an object is inside the
// body the walk enumerates. The walk is a scan of one body, so a variable
// declared outside it -- one the function literal being lowered captured, or
// the enclosing function's own -- has uses the walk cannot see, and finding
// nothing wrong inside the body is not evidence that it does not escape.
// regexp.(*Regexp).FindAllStringIndex assigns result = append(result, ...)
// inside a closure and returns result from the enclosing function; walking only
// the closure left the backing array on the closure's frame.
//
// The exception is the analysed function's own receiver, parameters and named
// results. They are declared in the signature rather than the body, and they
// are exactly what the walk is asked about.
func (g *gen) escapeWalkSeesEveryUse(object types.Object, body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	if object.Pos() >= body.Pos() && object.Pos() < body.End() {
		return true
	}
	for _, outer := range g.escapeWalkOuterObjects {
		if outer == object {
			return true
		}
	}
	return false
}

func (g *gen) objectDoesNotEscape(object types.Object, info *types.Info, parents map[ast.Node]ast.Node, body *ast.BlockStmt, checking map[parameterKey]bool) bool {
	saved := g.diagQuestion()
	local := g.objectDoesNotEscapeUnexplained(object, info, parents, body, checking)
	g.diagResolve(saved, !local, func() string {
		return fmt.Sprintf("%s, declared at %s", object.Name(), g.diagPosition(object.Pos()))
	})
	return local
}

// objectDoesNotEscapeUnexplained is objectDoesNotEscape's body; the split is
// gen.diagQuestion's, so that the walk over this object's uses is one link of
// the reported chain whatever branch inside it decides.
func (g *gen) objectDoesNotEscapeUnexplained(object types.Object, info *types.Info, parents map[ast.Node]ast.Node, body *ast.BlockStmt, checking map[parameterKey]bool) bool {
	if !g.escapeWalkSeesEveryUse(object, body) {
		g.diagRule(func() string {
			return fmt.Sprintf("%s is declared outside the body the walk can see every use in", object.Name())
		})
		return false
	}
	if g.objectEscapeChecks == nil {
		g.objectEscapeChecks = make(map[types.Object]bool)
	}
	if g.objectEscapeChecks[object] {
		g.diagRule(func() string {
			return fmt.Sprintf("the walk is already inside the question about %s: it breaks the cycle by answering \"escapes\"", object.Name())
		})
		return false
	}
	g.objectEscapeChecks[object] = true
	defer delete(g.objectEscapeChecks, object)

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
					g.reportEscapingUse(object, identifier)
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
			g.reportEscapingUse(object, identifier)
		}
		return true
	})
	return !escaped
}

// addressedExpression finds the address expression, if any, that an expression
// is the operand of. It climbs field selections and array indexes on the way,
// because &v.a.b and &v[i].f are addresses *of storage inside v* just as much
// as &v.a and &v[i] are: an interior pointer keeps the whole object alive, so
// the object escapes exactly when the pointer does. Only chains that stay
// within one object are climbed; a step through a pointer, a slice or a map
// lands in some other object and says nothing about this one.
func addressedExpression(expression ast.Node, parents map[ast.Node]ast.Node, info *types.Info) *ast.UnaryExpr {
	current := expression
	for {
		parent := parents[current]
		switch parent := parent.(type) {
		case *ast.ParenExpr:
			if parent.X != current {
				return nil
			}
			current = parent
		case *ast.SelectorExpr:
			if parent.X != current || !selectsWithinSameObject(parent, info) {
				return nil
			}
			current = parent
		case *ast.IndexExpr:
			if parent.X != current || !indexesWithinSameObject(parent, info) {
				return nil
			}
			current = parent
		case *ast.UnaryExpr:
			if parent.Op != token.AND || parent.X != current {
				return nil
			}
			return parent
		default:
			return nil
		}
	}
}

// selectsWithinSameObject reports that x.f names storage inside x rather than
// inside something x points at. i.s.next, where s is a pointer field, is a
// field of the *special* the iterator refers to and says nothing about the
// iterator; v.a.b, where a is a struct field, is storage inside v.
func selectsWithinSameObject(selector *ast.SelectorExpr, info *types.Info) bool {
	selection := info.Selections[selector]
	return selection != nil && selection.Kind() == types.FieldVal && !selection.Indirect()
}

// indexesWithinSameObject reports that x[i] names storage inside x. Only an
// array is stored inline; a slice and a pointer to an array both refer to
// storage held somewhere else.
func indexesWithinSameObject(index *ast.IndexExpr, info *types.Info) bool {
	baseType := info.TypeOf(index.X)
	if baseType == nil {
		return false
	}
	_, isArray := baseType.Underlying().(*types.Array)
	return isArray
}

// addressedVariableIdentifier names the variable whose *own storage* an address
// expression refers to, or reports that the address names storage somewhere
// else. &v.f and &v[i] are addresses inside v's slot; &p.f, &s[i] and &(*p).f
// are addresses inside whatever p or s points at, and say nothing about whether
// p's or s's slot has to outlive the function.
//
// findEscapingCaptures promotes the named variable's slot, so this is the
// question it has to ask. Promoting p because &p.f escaped moves the pointer,
// not the pointee, leaves the pointee exactly where it was, and made runtime
// code allocate on paths where allocation is forbidden.
func addressedVariableIdentifier(expression ast.Expr, info *types.Info) (*ast.Ident, bool) {
	for {
		switch value := expression.(type) {
		case *ast.Ident:
			return value, true
		case *ast.ParenExpr:
			expression = value.X
		case *ast.SelectorExpr:
			selection := info.Selections[value]
			if selection == nil || selection.Kind() != types.FieldVal || selection.Indirect() {
				return nil, false
			}
			expression = value.X
		case *ast.IndexExpr:
			baseType := info.TypeOf(value.X)
			if baseType == nil {
				return nil, false
			}
			if _, isArray := baseType.Underlying().(*types.Array); !isArray {
				return nil, false
			}
			expression = value.X
		default:
			return nil, false
		}
	}
}

func (g *gen) nonEscapingObjectUse(
	identifier *ast.Ident,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	if g.boxedIntoInterface(identifier, info, parents) {
		return false
	}
	parent := parents[identifier]
	switch parent := parent.(type) {
	case *ast.IndexExpr:
		if parent.X != identifier {
			return false
		}
		address := addressedExpression(parent, parents, info)
		return address == nil || !g.addressEscapesWithin(address, info, parents, body, checking)
	case *ast.SliceExpr:
		// A slice can carry the referenced storage into a result, interface, or
		// longer-lived aggregate, so the storage escapes exactly when the
		// resulting slice value does. Ask the same flow-sensitive question used
		// for other derived values rather than assuming the worst: helpers such
		// as func bytes(out *[32]byte) []byte { return out[:] } still force a
		// caller's local array onto the heap, because the returned slice escapes,
		// while runtime routines that slice a local scratch array only to pass it
		// to a non-retaining callee -- printuint's var buf [20]byte feeding
		// gwrite(buf[i:]) -- stay on the stack. Those routines must not allocate:
		// they run during mark termination and on fatal paths where mallocgc is
		// forbidden.
		if parent.X != identifier {
			return false
		}
		return g.valueDoesNotEscapeWithin(parent, info, parents, body, checking)
	case *ast.KeyValueExpr:
		if parent.Value != identifier {
			return false
		}
		literal, inLiteral := parents[parent].(*ast.CompositeLit)
		return inLiteral && g.compositeElementDoesNotEscape(literal, info, parents, body, checking)
	case *ast.CompositeLit:
		return g.compositeElementDoesNotEscape(parent, info, parents, body, checking)
	case *ast.SelectorExpr:
		if parent.X != identifier {
			return false
		}
		selection := info.Selections[parent]
		if selection == nil {
			return true
		}
		if selection.Kind() == types.FieldVal {
			// Reading a field does not carry the object out of the function,
			// but taking a field's address does: the resulting interior pointer
			// keeps the whole object alive, so the object escapes exactly when
			// that pointer does. This is the same question the index case above
			// asks about &v[i]. Omitting it let a package-level slice hold the
			// address of a field of a frame allocation.
			address := addressedExpression(parent, parents, info)
			return address == nil || !g.addressEscapesWithin(address, info, parents, body, checking)
		}
		if !g.methodCallDoesNotRetainReceiver(parent, info, checking) {
			return false
		}
		if call, calledImmediately := parents[parent].(*ast.CallExpr); calledImmediately && call.Fun == parent {
			return true
		}
		// A method value is a closure over the receiver, so the receiver is
		// wherever the closure is: `record := recorder.Add` puts recorder in
		// record and nowhere else. The method's own answer above already covers
		// what the call does with it; this covers where the closure goes.
		return g.valueDoesNotEscapeWithin(parent, info, parents, body, checking)
	case *ast.StarExpr:
		return parent.X == identifier
	case *ast.TypeAssertExpr:
		// See valueDoesNotEscapeWithin's case: the payload is reachable from
		// the asserted value, so the question continues from there.
		if parent.X != identifier {
			return false
		}
		return g.valueDoesNotEscapeWithin(parent, info, parents, body, checking)
	case *ast.RangeStmt:
		// Ranging over an object does not carry it anywhere. The clause's value
		// variable is not this object and does not alias it: declareRangeVariable
		// gives the variable storage of its own and storeAssignmentTarget copies
		// the element into it, for aggregates as much as for scalars. What the
		// copy carries away -- a pointer, a slice header, an interface -- names
		// some other object, whose placement is decided where that object was
		// made, and says nothing about where the range expression's own storage
		// has to live.
		//
		// This used to ask where the value variable went and charge the answer
		// to the range expression. That cost `for _, n := range []int{...}` its
		// backing array as soon as n reached anything the walk could not follow,
		// which for an int is every call, since an int has no storage to leak.
		return parent.X == identifier
	case *ast.BinaryExpr:
		return parent.Op == token.EQL || parent.Op == token.NEQ
	case *ast.AssignStmt:
		for _, leftHandSide := range parent.Lhs {
			if leftHandSide == identifier {
				return true
			}
		}
		assignmentIndex := -1
		for index, rightHandSide := range parent.Rhs {
			if rightHandSide == identifier {
				assignmentIndex = index
				break
			}
		}
		if assignmentIndex < 0 || assignmentIndex >= len(parent.Lhs) {
			return false
		}
		leftIdentifier, ok := parent.Lhs[assignmentIndex].(*ast.Ident)
		if !ok {
			return false
		}
		leftObject := info.Defs[leftIdentifier]
		if leftObject == nil {
			leftObject = info.Uses[leftIdentifier]
		}
		rightObject := info.Uses[identifier]
		if rightObject == nil {
			rightObject = info.Defs[identifier]
		}
		if leftObject == nil || leftObject == rightObject || leftObject.Pkg() == nil || g.resultObjects[leftObject] {
			return false
		}
		if _, global := g.globals[leftObject]; global {
			g.diagRule(func() string {
				return fmt.Sprintf("assigned to the package-level variable %s", leftObject.Name())
			})
			g.diagUse(parent)
			return false
		}
		if !copyAliasesStorage(rightObject) || !copyAliasesStorage(leftObject) {
			return false
		}
		return g.objectDoesNotEscape(leftObject, info, parents, body, checking)
	case *ast.ValueSpec:
		// `var x T = y` asks exactly what `x = y` asks above: the source's
		// storage is reachable from the destination, so it escapes exactly when
		// the destination does. Without this case the walk fell through to the
		// conservative default and a declaration form that an assignment form
		// answers put the object on the heap.
		valueIndex := -1
		for index, value := range parent.Values {
			if value == identifier {
				valueIndex = index
				break
			}
		}
		if valueIndex < 0 || valueIndex >= len(parent.Names) {
			return false
		}
		declared := info.Defs[parent.Names[valueIndex]]
		source := info.Uses[identifier]
		if declared == nil || declared == source || declared.Pkg() == nil || g.resultObjects[declared] {
			return false
		}
		if _, global := g.globals[declared]; global {
			return false
		}
		if !copyAliasesStorage(source) || !copyAliasesStorage(declared) {
			return false
		}
		return g.objectDoesNotEscape(declared, info, parents, body, checking)
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
				case "len", "cap", "copy", "print", "println":
					return true
				}
			}
		}
		if appendDestination(parent, identifier, info) {
			return g.valueDoesNotEscapeWithin(parent, info, parents, body, checking)
		}
		if appendSpreadSource(parent, identifier, info) {
			return true
		}
		if info.Types[parent.Fun].IsType() {
			convertedType := info.Types[parent].Type
			basic, ok := convertedType.Underlying().(*types.Basic)
			if ok && basic.Kind() == types.Uintptr {
				return true
			}
			if conversionCopiesOperand(parent, identifier, info) {
				return true
			}
			return g.valueDoesNotEscapeWithin(parent, info, parents, body, checking)
		}
		function := g.resolvedCallee(parent.Fun, info, parents, body)
		if function == nil {
			return false
		}
		if g.parameterDoesNotEscape(function, argumentIndex, checking) {
			return true
		}
		if !g.parameterLeaksOnlyToResult(function, argumentIndex, checking) {
			return false
		}
		if !singleResultFunction(function) {
			return g.leakedCallResultDoesNotEscape(parent, info, parents, body, checking)
		}
		return g.valueDoesNotEscapeWithin(parent, info, parents, body, checking)
	case *ast.ReturnStmt:
		return g.resultLeakIsAllowed(parent, parents)
	default:
		return false
	}
}

// compositeElementDoesNotEscape answers a use of a value as an element of a
// composite literal: the element escapes exactly when the composite value does.
// That is only true when the composite value *is* the storage, which is to say
// for struct and array literals. A slice or map literal has backing storage of
// its own that the literal expression only refers to, so an element placed in
// one is not bounded by where the literal is used and keeps the conservative
// answer.
//
// Without this, every value put in a struct literal escaped, which is how
// runtime.hexdumpWords' h := hexdumper{mark: symMark} made its callers'
// unwinder values heap-allocate.
func (g *gen) compositeElementDoesNotEscape(
	literal *ast.CompositeLit,
	info *types.Info,
	parents map[ast.Node]ast.Node,
	body *ast.BlockStmt,
	checking map[parameterKey]bool,
) bool {
	literalType := info.TypeOf(literal)
	if literalType == nil {
		return false
	}
	switch literalType.Underlying().(type) {
	case *types.Struct, *types.Array:
		return g.valueDoesNotEscapeWithin(literal, info, parents, body, checking)
	default:
		return false
	}
}

func isPointerLikeObject(object types.Object) bool {
	if object == nil {
		return false
	}
	switch object.Type().Underlying().(type) {
	case *types.Pointer, *types.Chan, *types.Map, *types.Signature:
		return true
	}
	basic, ok := object.Type().Underlying().(*types.Basic)
	return ok && basic.Kind() == types.UnsafePointer
}

// copyAliasesStorage reports that assigning one object to another hands the
// destination a reference to the same storage, so that "does the source escape"
// can be answered by asking the same of the destination. A pointer-shaped
// object does; so does an interface, which holds the pointer in its data word.
//
// An interface destination has to be included or `var value any = node` --
// where node is already pointer-shaped, so nothing is boxed and no storage is
// made -- has no answer but the conservative one, and every pointer put in a
// local interface goes to the heap however that interface is used.
func copyAliasesStorage(object types.Object) bool {
	if isPointerLikeObject(object) {
		return true
	}
	if object == nil {
		return false
	}
	_, isInterface := object.Type().Underlying().(*types.Interface)
	return isInterface
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

// enterCalleeBody makes the escape walk describe the function whose body it is
// about to enumerate rather than the function being lowered. Two pieces of
// per-function state are consulted from inside the walk and would otherwise
// still name the caller: resultLeakBody, which says whose result a return
// statement may leak to, and resultObjects, which makes an assignment to a
// named result count as an escape. Leaving the caller's named results in place
// meant a callee that assigned a parameter to its own named result and returned
// bare was reported as not letting the parameter escape.
func (g *gen) enterCalleeBody(signature *types.Signature, resultLeakBody *ast.BlockStmt) func() {
	savedLeakBody := g.resultLeakBody
	savedResults := g.resultObjects
	savedOuter := g.escapeWalkOuterObjects
	g.escapeWalkOuterObjects = signatureVariables(signature)
	g.resultLeakBody = resultLeakBody
	if resultLeakBody != nil {
		// A summary walk is allowed to reach the result, so a named result is
		// an ordinary local: its own uses decide whether the parameter goes
		// anywhere else.
		g.resultObjects = nil
	} else {
		g.resultObjects = resultObjectSet(signature)
	}
	return func() {
		g.resultLeakBody = savedLeakBody
		g.resultObjects = savedResults
		g.escapeWalkOuterObjects = savedOuter
	}
}

func (g *gen) parameterDoesNotEscape(function *types.Func, index int, checking map[parameterKey]bool) bool {
	saved := g.diagQuestion()
	local := g.parameterDoesNotEscapeUnexplained(function, index, checking)
	g.diagResolve(saved, !local, func() string {
		return fmt.Sprintf("argument %d of the call to %s", index, function.FullName())
	})
	return local
}

// parameterDoesNotEscapeUnexplained is parameterDoesNotEscape's body. The split
// exists so that the answer can be bracketed once -- see gen.diagQuestion -- and
// every refusal below can name itself without each one having to remember to.
func (g *gen) parameterDoesNotEscapeUnexplained(function *types.Func, index int, checking map[parameterKey]bool) bool {
	declaration, ok := g.functionDecls[function]
	if !ok {
		g.diagRule(func() string {
			return fmt.Sprintf("passed to %s, whose declaration this compilation does not have", function.FullName())
		})
		return false
	}
	signature := function.Type().(*types.Signature)
	if index < 0 {
		return false
	}
	if index >= signature.Params().Len() && !signature.Variadic() {
		return false
	}
	if declaration.decl.Body == nil {
		noescape := hasCompilerDirective(declaration.decl, "go:noescape")
		if !noescape {
			g.diagRule(func() string {
				return fmt.Sprintf("passed to %s, which has no Go body and is not marked //go:noescape", function.FullName())
			})
		}
		return noescape
	}
	if signature.Variadic() && index >= signature.Params().Len()-1 {
		// The callers of this walk hand it an argument position, and for a
		// variadic call every argument from the last parameter on is an
		// *element* of a slice the callee builds -- not the parameter. Answering
		// them with the parameter's own summary answers a different question:
		// `func keep(args ...*T) { sink = args[0] }` retains no pointer it was
		// handed, and retains everything they point at. That is what
		// testdata/variadic_element_address_retention.go is; answering it from
		// the parameter's own summary left a package-level variable pointing at a
		// caller's frame.
		//
		// So the question asked here is the deep one -- can the callee reach an
		// element at all -- and variadicParameterHoldsItsElements is the answer.
		held := g.variadicParameterHoldsItsElements(declaration, signature)
		if !held {
			g.diagRule(func() string {
				return fmt.Sprintf("passed in the variadic position of %s, whose ... parameter the walk cannot prove does not hold its elements", function.FullName())
			})
		}
		return held
	}
	key := parameterKey{function: function, index: index}
	if checking[key] {
		// The recursive edge is answered "does not escape", which is the
		// *optimistic* answer and the correct one. See recursiveEscapeAnswer.
		return recursiveEscapeAnswer
	}
	checking[key] = true
	defer delete(checking, key)
	restore := g.enterCalleeBody(signature, nil)
	defer restore()
	// Rooted at the declaration rather than at its body, so that a walk that
	// reaches a return statement can climb to the function it returns from and
	// ask what that result's type is. A summary walk allows a parameter to
	// reach the result, and a result of interface type boxes the value into
	// fresh heap storage on the way out; boxedIntoInterface needs the
	// signature to see that.
	parents := g.declarationParents(declaration.decl)
	return g.objectDoesNotEscape(signature.Params().At(index), declaration.info, parents, declaration.decl.Body, checking)
}

// variadicParameterHoldsItsElements reports that a variadic callee cannot reach
// an element of its `...` parameter, so an argument in a variadic position does
// not escape through the call.
//
// This is the deep question -- "is anything reachable *through* this slice
// retained" -- and the walk that answers every other position answers only the
// shallow one: whether the object itself is carried out. The two are not the
// same for a slice. `func keep(args ...*T) { for _, a := range args { sink = a } }`
// carries `args` nowhere, and carries out everything it points at.
//
// So this is a whitelist rather than a walk. A use of the parameter is accepted
// only where it provably cannot produce an element:
//
//   - len(args), cap(args)
//   - args == nil, args != nil
//   - for i := range args, with no value variable
//
// Everything else -- an index, a slice expression, a range with a value
// variable, a copy, being passed on, being stored, being returned -- is
// rejected. `func retainNothing(args ...any) int { return len(args) }` is the
// shape this exists for, and it is the shape a caller could not learn anything
// about before: testdata/variadic_backing.go's `retainNothing(&x)` put x on the
// heap, where gc keeps it in the frame.
//
// A wider rule is possible and is a walk of its own: it would have to follow an
// element out of an index or a range into the uses of the value it produced, and
// through a call into the callee's own deep answer for that position. That is
// the machinery ParamFact.Deep is in opt, and it is not this predicate.
func (g *gen) variadicParameterHoldsItsElements(declaration functionDecl, signature *types.Signature) bool {
	body := declaration.decl.Body
	if body == nil {
		return false
	}
	parameter := signature.Params().At(signature.Params().Len() - 1)
	if parameter.Name() == "" || parameter.Name() == "_" {
		// Unnamed: the body cannot mention it, so no element is reachable.
		return true
	}
	info := declaration.info
	parents := g.declarationParents(declaration.decl)
	reachable := false
	ast.Inspect(body, func(node ast.Node) bool {
		if reachable {
			return false
		}
		identifier, isIdentifier := node.(*ast.Ident)
		if !isIdentifier || info.Uses[identifier] != parameter {
			return true
		}
		if !variadicUseCannotReachAnElement(identifier, info, parents) {
			reachable = true
		}
		return true
	})
	return !reachable
}

// variadicUseCannotReachAnElement is variadicParameterHoldsItsElements' whitelist,
// asked of one use.
func variadicUseCannotReachAnElement(identifier *ast.Ident, info *types.Info, parents map[ast.Node]ast.Node) bool {
	switch parent := parents[identifier].(type) {
	case *ast.CallExpr:
		if parent.Fun == identifier {
			return false
		}
		called, isIdentifier := parent.Fun.(*ast.Ident)
		if !isIdentifier {
			return false
		}
		builtin, isBuiltin := info.Uses[called].(*types.Builtin)
		if !isBuiltin {
			return false
		}
		// copy is deliberately absent: it moves the elements into the
		// destination, which is exactly the thing this predicate is about.
		return builtin.Name() == "len" || builtin.Name() == "cap"
	case *ast.BinaryExpr:
		if parent.Op != token.EQL && parent.Op != token.NEQ {
			return false
		}
		other := parent.Y
		if other == identifier {
			other = parent.X
		}
		nilIdentifier, isIdentifier := other.(*ast.Ident)
		return isIdentifier && nilIdentifier.Name == "nil"
	case *ast.RangeStmt:
		// The index carries nothing. A value variable is a copy of an element and
		// carries everything the element points at.
		return parent.X == identifier && parent.Value == nil
	}
	return false
}

// parameterLeaksOnlyToResult reports that a parameter's storage cannot outlive
// the call except through the call's own result. It asks exactly the question
// parameterDoesNotEscape asks, with one difference: returning the parameter, or
// anything derived from it, from the summarised function is not an escape. A
// caller that gets this answer must therefore continue its walk from the call
// expression -- the storage escapes exactly when the result does.
//
// It does not describe a variadic parameter. parameterDoesNotEscape does, by a
// different question -- variadicParameterHoldsItsElements -- and there is no
// "leaks only to the result" form of that question here: an element reaching the
// result is an element reaching *out*, and the whitelist that answers the deep
// question has no case that produces an element at all. A caller that gets this
// answer for a function with more than one result cannot simply continue from
// the call expression, because that expression does not stand for one value; see
// leakedCallResultDoesNotEscape.
func (g *gen) parameterLeaksOnlyToResult(function *types.Func, index int, checking map[parameterKey]bool) bool {
	declaration, ok := g.functionDecls[function]
	if !ok || declaration.decl.Body == nil {
		return false
	}
	signature := function.Type().(*types.Signature)
	if signature.Results().Len() == 0 {
		return false
	}
	if index < 0 || index >= signature.Params().Len() {
		return false
	}
	if signature.Variadic() && index == signature.Params().Len()-1 {
		return false
	}
	key := parameterKey{function: function, index: index, summary: true}
	if checking[key] {
		return false
	}
	checking[key] = true
	defer delete(checking, key)
	restore := g.enterCalleeBody(signature, declaration.decl.Body)
	defer restore()
	// Rooted at the declaration rather than at its body, so that a walk that
	// reaches a return statement can climb to the function it returns from and
	// ask what that result's type is. A summary walk allows a parameter to
	// reach the result, and a result of interface type boxes the value into
	// fresh heap storage on the way out; boxedIntoInterface needs the
	// signature to see that.
	parents := g.declarationParents(declaration.decl)
	answer := g.objectDoesNotEscape(signature.Params().At(index), declaration.info, parents, declaration.decl.Body, checking)
	if !answer {
		g.diagRule(func() string {
			return fmt.Sprintf("passed to %s, whose parameter %d reaches more than the result", function.FullName(), index)
		})
	}
	reportResultLeakSummary(function, index, answer)
	return answer
}

// escapeDebugLevel reads GOC_DEBUG_ESCAPE. Level 1 traces every "leaks only to
// result" answer; level 2 also names the use that decided an object escapes.
// Both are off by default. They exist because neither fact is visible in the
// generated code: a local array stays on its frame only if every function in
// the chain below it answers yes, and when one does not, the useful questions
// are which one and which use.
func escapeDebugLevel() int {
	setting := os.Getenv("GOC_DEBUG_ESCAPE")
	if setting == "" {
		return 0
	}
	level, err := strconv.Atoi(setting)
	if err != nil {
		return 1
	}
	return level
}

func reportResultLeakSummary(function *types.Func, index int, leaksOnlyToResult bool) {
	if escapeDebugLevel() < 1 {
		return
	}
	fmt.Fprintf(os.Stderr, "escape summary: %s parameter %d leaks only to result: %v\n", function.FullName(), index, leaksOnlyToResult)
}

func (g *gen) reportEscapingUse(object types.Object, use ast.Node) {
	// The escape diagnostic hangs off the same call, because this is already the
	// one place the walk announces that a use decided.
	g.diagUse(use)
	g.diagRule(func() string {
		return fmt.Sprintf("%s is used here in a way the walk cannot prove keeps it local", object.Name())
	})
	if escapeDebugLevel() < 2 {
		return
	}
	fmt.Fprintf(os.Stderr, "escape use: %s escapes at %s\n", object.Name(), g.fset.Position(use.Pos()))
}

// recursiveEscapeAnswer is what parameterDoesNotEscape and receiverDoesNotEscape
// answer for the edge back into a question they are already inside.
//
// It is `true` -- "does not escape" -- and that is not an assumption. The walk
// computes, for one object, "does some chain of uses reach a use that publishes
// it". Written as a system of equations it is a pure monotone OR: a use is bad
// on its own, or it forwards the question to another parameter, and the object
// escapes exactly when some chain of forwards ends at a bad use. The answer that
// system defines is its *least* fixpoint, and a depth-first walk that answers the
// back edge "false" computes that fixpoint exactly, with no iteration:
//
//   - it never answers "escapes" without evidence. A `false` answer is only
//     returned by the branch that met a real publishing use, so every escape it
//     reports is a real chain of uses ending at a real bad use, which is a
//     derivation in the least fixpoint.
//   - it never answers "does not escape" without having checked everything. The
//     walk short-circuits only *after* something escaped, so an object it clears
//     was cleared with every one of its uses examined -- and the set of questions
//     visited on that walk is closed: each one's uses are either harmless or
//     forward to another question in the set, which was also cleared. Assigning
//     "does not escape" to all of them is consistent, so the least fixpoint
//     assigns it too.
//
// Answering the back edge "escapes" instead is sound but strictly weaker: it
// invents a publication that no use performs, and every question that forwards
// through the cycle inherits it. `log/slog`'s handleState is the case -- appendAttr
// and appendAttrs are mutually recursive, so the receiver could never be placed.
//
// The previous branch measured this change, found three frame-address
// publications in log/slog, and did not land it. Those publications were `defer
// state.free()` and had nothing to do with the cycle: see
// deferredFunctionValueStaysInFrame, which fixes them at the source and makes
// them reproducible with no compiler change at all.
//
// Everything else in the walk keeps its pessimistic break --
// interfaceMethodDoesNotRetainReceiver, parameterLeaksOnlyToResult, and
// objectDoesNotEscape's own per-object one all still answer "escapes" for a
// cycle. Mixing them is safe in one direction only, and this is that direction:
// a pessimistic break can only add escapes to a least fixpoint, never remove
// one.
const recursiveEscapeAnswer = true

func (g *gen) receiverDoesNotEscape(function *types.Func, checking map[parameterKey]bool) bool {
	declaration, ok := g.functionDecls[function]
	if !ok {
		return false
	}
	signature := function.Type().(*types.Signature)
	if signature.Recv() == nil {
		return true
	}
	if declaration.decl.Body == nil {
		return hasCompilerDirective(declaration.decl, "go:noescape")
	}
	key := parameterKey{function: function, index: -1}
	if checking[key] {
		// See recursiveEscapeAnswer, and the receiver half of the note there.
		return recursiveEscapeAnswer
	}
	checking[key] = true
	defer delete(checking, key)
	restore := g.enterCalleeBody(signature, nil)
	defer restore()
	// Rooted at the declaration rather than at its body, so that a walk that
	// reaches a return statement can climb to the function it returns from and
	// ask what that result's type is. A summary walk allows a parameter to
	// reach the result, and a result of interface type boxes the value into
	// fresh heap storage on the way out; boxedIntoInterface needs the
	// signature to see that.
	parents := g.declarationParents(declaration.decl)
	answer := g.objectDoesNotEscape(signature.Recv(), declaration.info, parents, declaration.decl.Body, checking)
	if !answer {
		g.diagRule(func() string {
			return fmt.Sprintf("used as the receiver of %s, which may retain it", function.FullName())
		})
	}
	if escapeDebugLevel() >= 2 {
		fmt.Fprintf(os.Stderr, "escape receiver: %s does not escape: %v\n", function.FullName(), answer)
	}
	return answer
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
					Items:        []ir.DataItem{{Sub: ir.SubL, Sym: g.lastModuleSymbol}},
					PointerWords: []int{0},
				})
				g.globals[obj] = name
				continue
			}
			if _, ok := obj.Type().Underlying().(*types.Interface); ok {
				g.globalInterface(id, obj, vs, i)
				name := g.pkg.Path() + "." + id.Name
				if linkedName := g.globalLinkNames[obj]; linkedName != "" {
					name = linkedName
				}
				g.markDataPointerWords(name, obj.Type())
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
	linkedName := g.globalLinkNames[object]
	if linkedName != "" {
		name = linkedName
	}
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
	valueExpression := initializer
	if _, isInterface := sourceType.Underlying().(*types.Interface); isInterface {
		conversion, ok := initializer.(*ast.CallExpr)
		if ok && len(conversion.Args) == 1 && g.info.Types[conversion.Fun].IsType() {
			valueExpression = conversion.Args[0]
			sourceType = g.info.Types[valueExpression].Type
		}
	}
	if isNilExpression(valueExpression) {
		emitZero()
		return
	}
	if g.info.Types[valueExpression].Value == nil {
		emitZero()
		return
	}

	items := g.staticInterfaceItems(name+".value", sourceType, object.Type(), valueExpression)
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:    name,
		Align:   8,
		Linkage: ir.Linkage{Export: ast.IsExported(id.Name) || linkedName != ""},
		Items:   items,
	})
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
	elementIndices, literalLength := compositeLiteralIndices(literal, g.info)
	backingName := name + ".backing"
	if structure, ok := slice.Elem().Underlying().(*types.Struct); ok {
		elements := make([][]ir.DataItem, literalLength)
		for expressionIndex, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			element, ok := expression.(*ast.CompositeLit)
			if !ok {
				elements[elementIndices[expressionIndex]] = []ir.DataItem{{Zero: int(typeSize(slice.Elem()))}}
				continue
			}
			elementIndex := elementIndices[expressionIndex]
			elementName := fmt.Sprintf("%s.element.%d", backingName, elementIndex)
			elements[elementIndex] = g.staticStructItems(elementName, structure, element)
		}
		items := make([]ir.DataItem, 0, literalLength*structure.NumFields())
		for _, elementItems := range elements {
			if elementItems == nil {
				elementItems = []ir.DataItem{{Zero: int(typeSize(slice.Elem()))}}
			}
			items = append(items, elementItems...)
		}
		arrayType := types.NewArray(slice.Elem(), int64(literalLength))
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:         backingName,
			Align:        int(typeAlign(slice.Elem())),
			Items:        items,
			PointerWords: pointerWordIndices(arrayType),
		})
		emitHeader([]ir.DataItem{
			{Sub: ir.SubL, Sym: backingName},
			{Sub: ir.SubL, Ints: []int64{int64(literalLength), int64(literalLength)}},
		})
		return
	}
	if pointer, ok := slice.Elem().Underlying().(*types.Pointer); ok {
		if structure, ok := pointer.Elem().Underlying().(*types.Struct); ok {
			items := make([]ir.DataItem, literalLength)
			for expressionIndex, expression := range literal.Elts {
				var value *ast.CompositeLit
				switch expression := expression.(type) {
				case *ast.CompositeLit:
					value = expression
				case *ast.UnaryExpr:
					if expression.Op == token.AND {
						value, _ = expression.X.(*ast.CompositeLit)
					}
				}
				elementIndex := elementIndices[expressionIndex]
				if value == nil {
					items[elementIndex] = ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
					continue
				}
				elementName := fmt.Sprintf("%s.element.%d", backingName, elementIndex)
				g.mod.Data = append(g.mod.Data, &ir.Data{
					Name:         elementName,
					Align:        8,
					Items:        g.staticStructItems(elementName, structure, value),
					PointerWords: pointerWordIndices(structure),
				})
				items[elementIndex] = ir.DataItem{Sub: ir.SubL, Sym: elementName}
			}
			g.mod.Data = append(g.mod.Data, &ir.Data{Name: backingName, Align: 8, Items: items, PointerWords: pointerWordIndices(types.NewArray(slice.Elem(), int64(literalLength)))})
			emitHeader([]ir.DataItem{
				{Sub: ir.SubL, Sym: backingName},
				{Sub: ir.SubL, Ints: []int64{int64(literalLength), int64(literalLength)}},
			})
			return
		}
	}
	var items []ir.DataItem
	var backingPointerWords []int
	elementBasic, basicElements := slice.Elem().Underlying().(*types.Basic)
	switch {
	case basicElements && elementBasic.Kind() == types.String:
		elements := make([][]ir.DataItem, literalLength)
		for expressionIndex, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			elementIndex := elementIndices[expressionIndex]
			value := g.info.Types[expression].Value
			if value == nil || value.Kind() != constant.String {
				elements[elementIndex] = []ir.DataItem{{Zero: int(typeSize(slice.Elem()))}}
				continue
			}
			contents := constant.StringVal(value)
			textName := fmt.Sprintf("%s.element.%d", backingName, elementIndex)
			g.mod.Data = append(g.mod.Data, &ir.Data{
				Name:  textName,
				Align: 1,
				Items: []ir.DataItem{
					{Sub: ir.SubUB, Str: contents},
				},
			})
			elements[elementIndex] = []ir.DataItem{
				ir.DataItem{Sub: ir.SubL, Sym: textName},
				ir.DataItem{Sub: ir.SubL, Ints: []int64{int64(len(contents))}},
			}
		}
		items = make([]ir.DataItem, 0, literalLength*2)
		for _, elementItems := range elements {
			if elementItems == nil {
				elementItems = []ir.DataItem{{Zero: int(typeSize(slice.Elem()))}}
			}
			items = append(items, elementItems...)
		}
		backingPointerWords = pointerWordIndices(types.NewArray(slice.Elem(), int64(literalLength)))
	case basicElements && elementBasic.Info()&(types.IsInteger|types.IsBoolean) != 0:
		values := make([]int64, literalLength)
		for expressionIndex, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			value := g.info.Types[expression].Value
			if value != nil {
				values[elementIndices[expressionIndex]] = constInt(value)
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
		values := make([]float64, literalLength)
		for expressionIndex, expression := range literal.Elts {
			if keyed, ok := expression.(*ast.KeyValueExpr); ok {
				expression = keyed.Value
			}
			value := g.info.Types[expression].Value
			if value == nil {
				continue
			}
			converted, _ := constant.Float64Val(value)
			values[elementIndices[expressionIndex]] = converted
		}
		sub := ir.SubD
		if elementBasic.Kind() == types.Float32 {
			sub = ir.SubS
		}
		items = []ir.DataItem{{Sub: sub, Flts: values}}
	default:
		items = []ir.DataItem{{Zero: int(typeSize(slice.Elem())) * literalLength}}
		backingPointerWords = pointerWordIndices(types.NewArray(slice.Elem(), int64(literalLength)))
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{Name: backingName, Align: 8, Items: items, PointerWords: backingPointerWords})
	emitHeader([]ir.DataItem{
		{Sub: ir.SubL, Sym: backingName},
		{Sub: ir.SubL, Ints: []int64{int64(literalLength), int64(literalLength)}},
	})
}

func compositeLiteralIndices(literal *ast.CompositeLit, info *types.Info) ([]int, int) {
	indices := make([]int, len(literal.Elts))
	nextIndex := 0
	literalLength := 0
	for expressionIndex, expression := range literal.Elts {
		if keyed, ok := expression.(*ast.KeyValueExpr); ok {
			nextIndex = int(constInt(info.Types[keyed.Key].Value))
		}
		indices[expressionIndex] = nextIndex
		nextIndex++
		if nextIndex > literalLength {
			literalLength = nextIndex
		}
	}
	return indices, literalLength
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
			descriptor := g.staticNamedFunctionDescriptor(function)
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
		descriptor := g.staticNamedFunctionDescriptor(function)
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
	// The full position, not line and column: nistec's p224/p384/p521 are one
	// generated file three times over, so their literals share a line and a
	// column and differ only in the file. Naming by position alone already
	// miscompiles elsewhere in the tree for exactly that reason. The enclosing
	// function is included too, since a generic body is compiled once per
	// instantiation from the same source position.
	literalKey := g.fset.Position(literal.Pos()).String()
	if g.functionName != "" {
		literalKey = g.functionName + "@" + literalKey
	}
	temporaryName, _ := g.internSymbol(".goc.global.literal", literalKey)
	temporary := g.mod.NewFuncVoid(temporaryName)

	savedFunction := g.fn
	savedBlock := g.cur
	savedVariables := g.vars
	savedDirectValues := g.directValues
	savedStackAddresses := g.stackAddresses
	savedHeapCaptures := g.heapCaptures
	savedEscapingCaptures := g.escapingCaptures
	savedReferenceCaptures := g.referenceCaptures
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
	g.referenceCaptures = make(map[types.Object]bool)
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
	g.referenceCaptures = savedReferenceCaptures
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

func (g *gen) functionLiteralRunsOnSystemStack(literal *ast.FuncLit) bool {
	call, ok := g.parents[literal].(*ast.CallExpr)
	if !ok {
		return false
	}
	isArgument := false
	for _, argument := range call.Args {
		if argument == literal {
			isArgument = true
			break
		}
	}
	if !isArgument {
		return false
	}
	function := calledFunction(call.Fun, g.info)
	if function == nil || function.Pkg() == nil || function.Pkg().Path() != "runtime" {
		return false
	}
	switch function.Name() {
	case "systemstack", "mcall":
		return true
	default:
		return false
	}
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
		// This module carries the runtime's own moduledata, so it is the head of
		// the module chain. Naming it here rather than letting the backend match
		// the spelling is what lets a second module carry a moduledata of its own.
		g.mod.GoModuleData = g.pkg.Path() + "." + id.Name
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
	if _, isArray := element.Underlying().(*types.Array); isArray {
		items := []ir.DataItem{{Zero: int(typeSize(array))}}
		if valueIndex < len(spec.Values) && staticallyInitializedGlobal(spec.Values[valueIndex], object.Type(), g.info) {
			literal, ok := spec.Values[valueIndex].(*ast.CompositeLit)
			if ok {
				items = g.staticArrayItems(name, array, literal)
			}
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
				descriptor := g.staticNamedFunctionDescriptor(function)
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
	file := g.mod.File(p.Filename)
	g.cur.At(ir.SrcPos{File: file, Line: uint32(p.Line), Col: uint32(p.Column)})
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
	cacheKey := goABIAggregateKey(g.fset, valueType)
	if aggregate := g.goABITypes[cacheKey]; aggregate != nil {
		return aggregate
	}
	var fields []ir.Field
	switch value := valueType.Underlying().(type) {
	case *types.Array:
		if value.Len() == 0 {
			// A zero-length array holds no elements and occupies no storage, and
			// ir.Field.Count cannot say so: Count is 0 for every ordinary scalar
			// field as well, so ir.Field.count() reads 0 as one element. A field
			// emitted here would be a phantom element of the element's size and
			// shape, sitting at the offset of whatever field follows the array
			// and displacing every later field by that much.
			//
			// When the element is pointer-shaped that phantom is a pointer word
			// claimed over a scalar, which is a frame word the collector and the
			// stack copier will walk as an address. log/slog's Value leads with
			// [0]func() to make itself incomparable and follows it with the
			// uint64 that carries every packed kind, so the claimed word is
			// exactly that integer. See goc/testdata/slog_attr_frame_gcmask.go.
			//
			// Contributing no field is both the correct layout and the only one
			// Count can express.
			break
		}
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
		if trailingZeroSizedFieldNeedsPadding(value) {
			// gc gives a struct whose last field is zero-sized one byte of
			// padding, so that a pointer to that field does not point past the
			// end of the object; typeSize reproduces the rule. The aggregate has
			// to agree, or the frame slot it sizes is short of the value the
			// front end copies into it.
			//
			// The padding is a byte-wide scalar and never a pointer. A
			// zero-length array of a pointer-shaped element used to supply this
			// size by being emitted as a whole phantom element, which put a
			// claimed pointer word in the padding.
			fields = append(fields, ir.Field{Sub: ir.SubUB})
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
		Name:   contentSymbolName(".goc.goabi", cacheKey),
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

func goABIAggregateKey(fset *token.FileSet, valueType types.Type) string {
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
	return "aggregate:" + goTypeKey(fset, valueType)
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

// materializeNilInterface gives an interface value a two-word descriptor to
// copy out of. A nil interface is carried as a nil pointer rather than as the
// address of a zeroed descriptor, so a copy has to branch on it.
//
// It does not have to branch on an address this frame allocated. Those are
// never nil, and the branch costs more than the instructions in it: the phi
// that merges the two arms is a use of the descriptor slot that opt's alias
// analysis cannot see through, so the slot is classified cEscaped and every
// candidate pointer stored into it is treated as published. That is what kept a
// variadic `...any` call's backing object on the heap -- the boxed payload
// lives inside that object, its address goes into the descriptor, and the
// descriptor looked like it escaped.
func (g *gen) materializeNilInterface(value ir.Ref) ir.Ref {
	if g.isStackAddress(value) {
		return value
	}
	zeroDescriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(zeroDescriptor, 0)
	g.markStackPointerWord(zeroDescriptor, 8)
	g.markTransientInterfaceDescriptor(zeroDescriptor)
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
	descriptor := done.Phi(ir.ClsP,
		ir.PhiEdge{From: useZero, Val: zeroDescriptor},
		ir.PhiEdge{From: useValue, Val: value},
	)
	return descriptor
}

func (g *gen) normalizeCallInterfaces(arguments []ir.Ref, signature *types.Signature, receiverType types.Type) []ir.Ref {
	if !g.runtimeAllocation {
		return arguments
	}
	normalized := append([]ir.Ref(nil), arguments...)
	argumentTypes := make([]types.Type, 0, signature.Params().Len()+1)
	if receiverType != nil {
		argumentTypes = append(argumentTypes, receiverType)
	}
	for parameterIndex := 0; parameterIndex < signature.Params().Len(); parameterIndex++ {
		argumentTypes = append(argumentTypes, signature.Params().At(parameterIndex).Type())
	}

	type preservedArgument struct {
		index     int
		storage   ir.Ref
		valueType types.Type
		class     ir.Cls
		slice     bool
	}
	var preserved []preservedArgument
	hasInterface := false
	for _, valueType := range argumentTypes {
		if isInterfaceValue(valueType) {
			hasInterface = true
			break
		}
	}
	if hasInterface {
		for argumentIndex, valueType := range argumentTypes {
			if argumentIndex >= len(normalized) || isInterfaceValue(valueType) {
				continue
			}
			value := normalized[argumentIndex]
			if isSliceType(valueType) {
				storage := g.localAllocTyped(valueType)
				g.store(value, storage, valueType)
				preserved = append(preserved, preservedArgument{
					index:     argumentIndex,
					storage:   storage,
					valueType: valueType,
					slice:     true,
				})
				continue
			}
			if value.Kind == ir.RefAggregate {
				continue
			}
			class := g.fn.ClassOf(value)
			alignment := class.Size()
			if alignment < 4 {
				alignment = 4
			}
			storage := g.localAlloc(alignment, class.Size())
			if class == ir.ClsP {
				g.markStackPointerWord(storage, 0)
			}
			g.cur.Store(value, storage)
			preserved = append(preserved, preservedArgument{
				index:   argumentIndex,
				storage: storage,
				class:   class,
			})
		}
	}

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
	for _, argument := range preserved {
		if argument.slice {
			normalized[argument.index] = g.load(argument.storage, argument.valueType)
			continue
		}
		normalized[argument.index] = g.cur.Load(argument.class, argument.storage)
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
	transientArguments := arguments
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
	instruction.ClosureCall, instruction.ClosureContext = g.consumeClosureCall()
	if instruction.ClosureCall {
		instruction.CallConv = ir.CallConvGoInternal
		instruction.CallConvSet = true
	}
	instruction.ArgGroups = argumentGroups
	instruction.AggArgs = aggregateArguments
	if signature.Results().Len() > 0 && instruction.RetAgg == nil {
		instruction.RetAgg = g.goABIAggregate(signature.Results().At(0).Type())
	}
	g.clearTransientInterfaceCallArguments(transientArguments, signature, receiverType)
	return result
}

func (g *gen) callVoidWithSignature(callee ir.Ref, arguments []ir.Ref, signature *types.Signature, receiverType types.Type) {
	arguments = g.normalizeCallInterfaces(arguments, signature, receiverType)
	transientArguments := arguments
	arguments, argumentGroups, aggregateArguments := g.flattenCallArguments(arguments, signature, receiverType)
	g.cur.CallVoid(callee, arguments...)
	instruction := &g.cur.Instrs[len(g.cur.Instrs)-1]
	instruction.ClosureCall, instruction.ClosureContext = g.consumeClosureCall()
	if instruction.ClosureCall {
		instruction.CallConv = ir.CallConvGoInternal
		instruction.CallConvSet = true
	}
	instruction.ArgGroups = argumentGroups
	instruction.AggArgs = aggregateArguments
	g.clearTransientInterfaceCallArguments(transientArguments, signature, receiverType)
}

func (g *gen) clearTransientInterfaceCallArguments(arguments []ir.Ref, signature *types.Signature, receiverType types.Type) {
	argumentIndex := 0
	clearArgument := func(valueType types.Type) {
		if argumentIndex >= len(arguments) {
			return
		}
		value := arguments[argumentIndex]
		argumentIndex++
		if _, isInterface := valueType.Underlying().(*types.Interface); !isInterface {
			return
		}
		if value.Kind != ir.RefTemp || !g.transientInterfaceDescriptors[value.ID] {
			return
		}
		nilPointer := g.fn.ConstInt(ir.ClsP, 0)
		g.cur.Store(nilPointer, value)
		g.cur.Store(nilPointer, g.offset(value, 8))
		delete(g.transientInterfaceDescriptors, value.ID)
	}
	if receiverType != nil {
		clearArgument(receiverType)
	}
	for parameterIndex := 0; parameterIndex < signature.Params().Len(); parameterIndex++ {
		clearArgument(signature.Params().At(parameterIndex).Type())
	}
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
	case types.Int, types.Uint, types.Int64, types.Uint64, types.Uintptr, types.UnsafePointer:
		return ir.SubL, true
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
	// The barrier decision comes before the sub-width store, not after it.
	// subOf reports a width for unsafe.Pointer as well as for the integer
	// kinds, and unsafe.Pointer is the one type it accepts that scalar
	// classifies as a pointer, so taking the sub-store path first silently
	// dropped the write barrier from every unsafe.Pointer store -- including
	// runtime.gostartcall's `buf.ctxt = ctxt`, which publishes a goroutine's
	// funcval into a g the collector may already have blackened
	// (RUNTIME_PLAN.md 5.12).
	class, _ := scalar(t)
	if g.runtimeAllocation && !g.noWriteBarrier && class == ir.ClsP && !g.isStackAddress(addr) && !isNotInHeapPointer(t) {
		g.cur.CallVoid(g.fn.Sym("goc_storep", 0), addr, v)
		return
	}
	if sub, ok := subOf(t); ok {
		g.cur.StoreSub(sub, v, addr)
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
	if g.runtimeAllocation && !g.noWriteBarrier && len(barrieredPointerWordIndices(valueType)) != 0 {
		g.storePointerAwareInlineValue(value, address, valueType)
		return
	}
	g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), address, value, g.fn.Long(typeSize(valueType)))
}

// storePointerAwareInlineValue copies the scalar regions of an aggregate with
// memcpy and publishes each barriered pointer word through the normal
// pointer-store path. The latter dynamically distinguishes heap and global
// destinations from any goroutine stack, so aggregate assignment preserves Go's
// write barrier semantics without changing the in-memory layout of the value.
// Words holding a pointer to a not-in-heap type are not barriered and travel
// with the surrounding memcpy instead; see barrieredPointerWordIndices.
func (g *gen) storePointerAwareInlineValue(value, address ir.Ref, valueType types.Type) {
	pointerWords := barrieredPointerWordIndices(valueType)
	valueSize := typeSize(valueType)
	nextOffset := int64(0)
	destinationIsStack := g.isStackAddress(address)

	copyBytes := func(offset, size int64) {
		if size == 0 {
			return
		}
		source := g.offset(value, offset)
		destination := g.offset(address, offset)
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), destination, source, g.fn.Long(size))
	}

	for _, pointerWord := range pointerWords {
		pointerOffset := int64(pointerWord) * pointerSize()
		copyBytes(nextOffset, pointerOffset-nextOffset)

		source := g.offset(value, pointerOffset)
		destination := g.offset(address, pointerOffset)
		pointer := g.cur.Load(ir.ClsP, source)
		if destinationIsStack {
			g.cur.Store(pointer, destination)
		} else {
			g.cur.CallVoid(g.fn.Sym("goc_storep", 0), destination, pointer)
		}
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
	// escapingCaptures is storage that outlives the frame; iterationCaptures is
	// storage that outlives one iteration of the loop that declares it. Both
	// need an allocation rather than a frame slot, and for the same reason: a
	// frame slot is one object, and each of these needs more than one. See
	// findIterationCaptures.
	if g.runtimeAllocation && (g.escapingCaptures[object] || g.iterationCaptures[object]) {
		var storage ir.Ref
		if isMemoryValue(valueType) {
			backing := g.allocateTyped(valueType)
			storageType := types.NewPointer(valueType)
			storage = g.allocateTyped(storageType)
			g.store(backing, storage, storageType)
		} else if isIndirectVariableValue(valueType) {
			// Strings, interfaces and complex128s are normally represented by a
			// local slot containing a pointer to their inline value. Once the
			// variable's address escapes, the heap allocation is the value itself.
			storage = g.allocateTyped(valueType)
			g.directValues[object] = true
		} else {
			storage = g.allocateTyped(valueType)
		}
		g.vars[object] = storage
		g.heapCaptures[object] = storage
		return storage
	}
	if g.referenceCaptures[object] && isIndirectVariableValue(valueType) {
		// A nested function body -- a function literal, or the yield function a
		// range-over-function body is lowered into -- reaches this variable
		// through the closure environment, which carries the address of this
		// slot. The slot therefore belongs to a frame the nested body does not
		// own.
		//
		// Keeping the value behind a pointer makes that fatal: assigning to the
		// variable copies the new value into an alloca of the *assigning*
		// function and then stores that address into the shared slot, so the
		// enclosing frame is left addressing the closure's dead frame. Give the
		// variable the same representation the escaping-capture arm above gives
		// it -- the storage is the value -- so an assignment copies the value's
		// bytes into storage that outlives the nested call. Unlike that arm this
		// costs no allocation: the value lives in the declaring frame.
		storage := g.localAllocTyped(valueType)
		g.zero(storage, valueType)
		g.vars[object] = storage
		g.directValues[object] = true
		return storage
	}
	storage := g.allocLocal(valueType)
	g.vars[object] = storage
	return storage
}

// isIndirectVariableValue reports whether a local variable of this type would
// otherwise be represented by a frame slot holding the address of a separate
// value, so that assigning to it replaces the address rather than the value.
//
// Structs and arrays are excluded because allocLocal already gives them stable
// backing storage and assignLocal copies into it, and slices because a slice
// local is stored inline. What is left is the three values that are wider than
// a register and carry no backing of their own: a string, an interface, and a
// complex128.
func isIndirectVariableValue(valueType types.Type) bool {
	if isMemoryValue(valueType) || isSliceType(valueType) {
		return false
	}
	return isInlineValue(valueType)
}

// perIterationVariable reports whether object, a variable declared by a `for`
// init statement or by a `range` clause, needs its own storage in every
// iteration.
//
// Go 1.22 made those variables per-iteration, so a closure created in one
// iteration keeps observing that iteration's value. Reproducing that only
// matters when the variable's storage can outlive the iteration, which is
// exactly the condition variableStorage already tests: a variable captured by
// an escaping closure, or whose address otherwise escapes, is heap-lifted, and
// its cell is what a later observer reads. A variable that stays in a frame
// slot cannot be observed after its iteration ends, so sharing one slot across
// iterations is unobservable and keeps the common loop free of allocation.
func (g *gen) perIterationVariable(object types.Object) bool {
	return object != nil && g.runtimeAllocation && g.escapingCaptures[object]
}

// freshVariableStorage allocates one more instance of the storage
// variableStorage builds for an escaping capture, with the same shape, so a
// value can be moved between instances with the ordinary variable read and
// assignment helpers.
//
// The allocation deliberately bypasses the heap-allocation candidate form that
// opt.LowerHeapAllocations may promote to a frame slot. That pass reasons about
// whether a pointer outlives the frame, not whether it outlives one iteration,
// so promoting a per-iteration cell would silently collapse every iteration
// back onto one slot -- the bug this storage exists to avoid.
func (g *gen) freshVariableStorage(valueType types.Type) ir.Ref {
	if isMemoryValue(valueType) {
		backing := g.allocateEscapingTyped(valueType)
		storageType := types.NewPointer(valueType)
		storage := g.allocateEscapingTyped(storageType)
		g.store(backing, storage, storageType)
		return storage
	}
	return g.allocateEscapingTyped(valueType)
}

// variableValue reads object's current storage the way an identifier reference
// would, so the value can be copied into a different instance of that same
// variable's storage.
func (g *gen) variableValue(object types.Object, valueType types.Type) ir.Ref {
	slot := g.vars[object]
	if g.directValues[object] {
		return slot
	}
	return g.load(slot, valueType)
}

// startIterationVariable gives object fresh storage for the iteration that is
// about to run and returns it. The caller stores the iteration's value into the
// returned slot; a `range` clause always assigns its variables from the range
// expression, so nothing needs to be carried in.
func (g *gen) startIterationVariable(object types.Object, valueType types.Type) ir.Ref {
	storage := g.freshVariableStorage(valueType)
	g.vars[object] = storage
	g.heapCaptures[object] = storage
	return storage
}

// enterIterationVariable gives object fresh storage for the iteration that is
// about to run and copies in the value the loop carries between iterations. A
// three-clause `for` needs this: its init statement and its post statement act
// on a value that flows from one iteration to the next.
func (g *gen) enterIterationVariable(object types.Object, carrier ir.Ref, valueType types.Type) ir.Ref {
	g.vars[object] = carrier
	carried := g.variableValue(object, valueType)
	storage := g.startIterationVariable(object, valueType)
	g.assignLocal(carried, storage, valueType)
	return storage
}

// leaveIterationVariable copies the value the finished iteration left in its own
// storage back into the loop's carrier, so the next iteration and the post
// statement see it.
func (g *gen) leaveIterationVariable(object types.Object, storage, carrier ir.Ref, valueType types.Type) {
	g.vars[object] = storage
	value := g.variableValue(object, valueType)
	g.vars[object] = carrier
	g.assignLocal(value, carrier, valueType)
}

func (g *gen) resultStorage(object types.Object, valueType types.Type) ir.Ref {
	if object != nil && g.runtimeAllocation && g.escapingCaptures[object] {
		storage := g.allocateTyped(valueType)
		g.zero(storage, valueType)
		g.vars[object] = storage
		g.heapCaptures[object] = storage
		if isInlineAggregate(valueType) || isInterfaceValue(valueType) {
			g.directValues[object] = true
		}
		return storage
	}
	if (isInlineAggregate(valueType) || isInterfaceValue(valueType)) && !(g.runtimeAllocation && isSliceType(valueType)) {
		storage := g.aggregateResult
		if storage == ir.R {
			storage = g.aggregateResultStorage(valueType)
		}
		g.zero(storage, valueType)
		if object != nil {
			g.directValues[object] = true
			g.vars[object] = storage
		}
		return storage
	}
	if g.runtimeAllocation && isSliceType(valueType) {
		storage := g.allocLocal(valueType)
		if object != nil {
			g.vars[object] = storage
		}
		return storage
	}
	storage := g.alloc(valueType)
	g.store(g.zeroValue(valueType), storage, valueType)
	if object != nil {
		g.vars[object] = storage
	}
	return storage
}

// findKeepAliveObjects returns the local variables this function body names in
// a runtime.KeepAlive call, both as a set and in source order. The order is
// what fixes the frame layout of their keep-alive slots, so it must not come
// from ranging over the set.
func (g *gen) findKeepAliveObjects(body *ast.BlockStmt) (map[types.Object]bool, []types.Object) {
	objects := make(map[types.Object]bool)
	var ordered []types.Object
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil {
			return true
		}
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		function, ok := g.info.Uses[selector.Sel].(*types.Func)
		if !ok || function.Pkg() == nil || function.Pkg().Path() != "runtime" || function.Name() != "KeepAlive" {
			return true
		}
		identifier, ok := call.Args[0].(*ast.Ident)
		if !ok {
			return true
		}
		object := g.info.Uses[identifier]
		if object != nil && !objects[object] {
			objects[object] = true
			ordered = append(ordered, object)
		}
		return true
	})
	return objects, ordered
}

func (g *gen) trackKeepAliveAssignment(object types.Object, value ir.Ref, valueType types.Type) {
	if object == nil || !g.keepAliveObjects[object] {
		return
	}
	if runtimePointerBytes(valueType) == 0 && !isDirectInterfaceType(valueType) {
		return
	}
	if value.Kind == ir.RefAggregate {
		return
	}
	g.keepAliveValues[object] = g.fn.MarkGCRef(value)
	if scalarClass, ok := scalar(valueType); ok && scalarClass == ir.ClsP {
		if slot, ok := g.keepAliveSlots[object]; ok {
			g.store(value, slot, types.Typ[types.UnsafePointer])
		}
	}
}

// declareKeepAliveSlots reserves one pointer-sized frame slot per variable the
// function keeps alive, in the entry block and in source order, and zeroes it.
//
// The slot is what makes the value a stack root for the collector between the
// assignment that produces it and the runtime.KeepAlive call that releases it;
// cg12 has no source-level notion of a variable, so without it the value's
// liveness ends at its last ordinary use.
//
// It has to be a frame slot rather than a global. A global is shared by every
// goroutine running the function, so concurrent calls overwrite each other's
// value, and -- because escape analysis may leave the kept-alive object on the
// stack -- a global would publish a goroutine stack address to a permanent GC
// root that no stack copy relocates and no goroutine exit clears. Zeroing at
// entry matters because the slot is a per-safepoint precise root from the
// moment the allocation is defined, which is before the first store to it.
func (g *gen) declareKeepAliveSlots(objects []types.Object) {
	for _, object := range objects {
		slot := g.alloc(types.Typ[types.UnsafePointer])
		g.store(g.fn.ConstInt(ir.ClsP, 0), slot, types.Typ[types.UnsafePointer])
		g.keepAliveSlots[object] = slot
	}
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
	g.fn.MarkGCRef(allocation)
}

func (g *gen) markTransientInterfaceDescriptor(descriptor ir.Ref) {
	if descriptor.Kind != ir.RefTemp {
		return
	}
	if g.transientInterfaceDescriptors == nil {
		g.transientInterfaceDescriptors = make(map[uint32]bool)
	}
	g.transientInterfaceDescriptors[descriptor.ID] = true
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

	if isComplexType(from) && isComplexType(to) {
		if from.Underlying().(*types.Basic).Kind() == to.Underlying().(*types.Basic).Kind() {
			return v
		}
		return g.complexConversion(v, from, to)
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
	if payload == ir.R {
		// Ahead of g.expr, not after it: emitting the constant would build the
		// frame descriptor a string literal is otherwise materialised into, and
		// nothing downstream would read it. See staticInterfaceDescriptor.
		if descriptor, static := g.staticInterfaceDescriptor(expression, sourceType, targetType); static {
			return descriptor
		}
	}
	value := g.expr(expression)
	return g.adaptValueToInterface(value, sourceType, targetType, payload, expression)
}

// staticInterfaceDescriptor builds the whole interface value for a constant
// conversion -- type word and a data word pointing at read-only storage -- and
// reports whether the conversion was one it could build.
func (g *gen) staticInterfaceDescriptor(source ast.Node, sourceType, targetType types.Type) (ir.Ref, bool) {
	if sourceType == nil {
		return ir.R, false
	}
	if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
		return ir.R, false
	}
	if isDirectInterfaceType(sourceType) {
		return ir.R, false
	}
	symbol, static := g.staticInterfacePayload(source, sourceType)
	if !static {
		return ir.R, false
	}
	// Nothing is stored into the payload: it already exists, in read-only data,
	// with the value it will always have.
	descriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(descriptor, 0)
	g.markStackPointerWord(descriptor, 8)
	g.markTransientInterfaceDescriptor(descriptor)
	g.cur.Store(g.interfaceTypeWordFor(sourceType, targetType), descriptor)
	g.cur.Store(g.fn.Sym(symbol, 0), g.offset(descriptor, 8))
	return descriptor, true
}

// interfaceTypeWordFor is word 0 of an interface value holding a value of the
// statically known sourceType: the itab for an interface with methods, and the
// plain type descriptor otherwise. interfaceTypeWord is the same question asked
// of a type only known at run time, and staticInterfaceTypeWord is this one
// answered as a data item rather than as an operand.
func (g *gen) interfaceTypeWordFor(sourceType, targetType types.Type) ir.Ref {
	typeTag := g.ensureTypeTag(sourceType)
	g.ensureRuntimeTypeEqual(sourceType, typeTag)
	if g.runtimeAllocation && interfaceHasMethods(targetType) {
		return g.fn.Sym(g.ensureInterfaceItab(sourceType, targetType), 0)
	}
	return g.fn.Sym(typeTag, 0)
}

func (g *gen) adaptValueToInterface(value ir.Ref, sourceType, targetType types.Type, payload ir.Ref, source ast.Node) ir.Ref {
	if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
		return g.adaptInterfaceToInterface(value, sourceType, targetType)
	}
	typeWord := g.interfaceTypeWordFor(sourceType, targetType)
	if isDirectInterfaceType(sourceType) {
		descriptor := g.localAlloc(8, 16)
		g.markStackPointerWord(descriptor, 0)
		g.markStackPointerWord(descriptor, 8)
		g.markTransientInterfaceDescriptor(descriptor)
		g.cur.Store(typeWord, descriptor)
		if value.Kind == ir.RefAggregate {
			g.fail(source, "direct interface payload %s lowered as an aggregate", sourceType)
			return descriptor
		}
		g.cur.Store(value, g.offset(descriptor, 8))
		return descriptor
	}
	if payload == ir.R {
		if descriptor, static := g.staticInterfaceDescriptor(source, sourceType, targetType); static {
			return descriptor
		}
		payload = g.allocateInterfacePayload(value, sourceType)
	}
	if isAddressRepresentedInterfacePayload(sourceType) {
		g.storeInlineValue(value, payload, sourceType)
	} else {
		g.store(value, payload, sourceType)
	}
	descriptor := g.localAlloc(8, 16)
	g.markStackPointerWord(descriptor, 0)
	g.markStackPointerWord(descriptor, 8)
	g.markTransientInterfaceDescriptor(descriptor)
	g.cur.Store(typeWord, descriptor)
	g.cur.Store(payload, g.offset(descriptor, 8))
	return descriptor
}

// staticInterfacePayload names the read-only object an interface's data word
// can point at when the value being boxed is a compile-time constant, and
// reports whether there is one.
//
// A constant conversion has no storage to allocate. The payload's contents are
// known at compile time and an interface payload is immutable by the language's
// own rules -- there is no way to obtain an addressable reference to the value
// inside an interface -- so one object per distinct (type, value) pair in
// read-only data serves every conversion of that constant in the program. This
// is what cmd/compile does; its equivalents are the `stmp_N` symbols it emits
// for a constant conversion and the &runtime.staticuint64s[c] it uses for a
// small integer one, and it is why gc reports zero allocations for `any("a")`
// where goc, until this existed, reported one 16-byte object.
//
// The gain is larger than the object it removes, because of what a payload
// costs elsewhere. A payload with no runtime conversion helper is laid out as a
// field of the same allocation as its variadic call's `[N]any` backing array
// (see variadicPayloadStorage), which makes that object point into itself, and
// opt.markSummarisedCall must send an object that points into itself to the
// heap whenever the callee may retain an element. A payload that has a helper is
// split out, but then counts against foldSplitPayloadsBackIn's budget and can
// pull the array to the heap that way instead. A constant needs no payload at
// all, so it does neither: `logger.Info("msg", "a", 1)` has nothing left in its
// argument object except the array, and the array stays in the frame.
//
// Only basic types are eligible. A constant of any other type either has no
// constant representation to emit (a nil pointer is pointer-shaped and never
// reaches here) or is not something go/types hands back a constant.Value for.
func (g *gen) staticInterfacePayload(source ast.Node, sourceType types.Type) (string, bool) {
	expression, isExpression := source.(ast.Expr)
	if !isExpression || sourceType == nil {
		return "", false
	}
	constantValue := g.typeAndValue(expression).Value
	if constantValue == nil || !staticInterfacePayloadKind(sourceType, constantValue) {
		return "", false
	}
	// Keyed by the type as well as the value: `int32(1)` and `int64(1)` are the
	// same constant and different objects, and the type is also what decides
	// which type descriptor word 0 of the interface gets.
	//
	// Interned by hand rather than through internSymbol, because the name may
	// not be recorded until the contents have been built: staticValueItems
	// answering no after the name was interned would leave a second conversion
	// of the same constant resolving to a name nothing emitted.
	// staticInterfacePayloadKind is meant to make that unreachable; not
	// interning early is what makes a gap between the two a missed
	// optimization instead.
	const payloadPrefix = ".goc.ifacedata"
	key := runtimeTypeKey(g.fset, sourceType) + "=" + constantValue.ExactString()
	if name := g.contentSymbols[payloadPrefix+":"+key]; name != "" {
		return name, true
	}
	name := contentSymbolName(payloadPrefix, key)
	items, built := g.staticValueItems(name, sourceType, expression)
	if !built || len(items) == 0 {
		return "", false
	}
	g.contentSymbols[payloadPrefix+":"+key] = name
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:         name,
		Align:        int(typeAlign(sourceType)),
		Items:        items,
		PointerWords: pointerWordIndices(sourceType),
	})
	return name, true
}

// staticInterfacePayloadKind reports whether staticValueItems will render a
// constant of this type, without emitting anything.
//
// It has to be asked separately because the symbol's name is interned before
// its contents are built -- a second conversion of the same constant must reuse
// the first object rather than emit a second -- and a name interned for a
// payload that then could not be built would be a reference to a symbol that
// does not exist.
func staticInterfacePayloadKind(sourceType types.Type, constantValue constant.Value) bool {
	basic, isBasic := sourceType.Underlying().(*types.Basic)
	if !isBasic || basic.Info()&types.IsUntyped != 0 {
		// An untyped constant has no runtime type to name in word 0. In an
		// assignment to an interface go/types has already given the operand its
		// default type, so this only excludes places that have not.
		return false
	}
	switch {
	case basic.Kind() == types.String:
		return constantValue.Kind() == constant.String
	case basic.Info()&types.IsComplex != 0:
		// Two words wide and address-represented; staticValueItems has no case
		// for it.
		return false
	case basic.Info()&types.IsFloat != 0:
		return true
	case basic.Kind() == types.UnsafePointer:
		// Pointer-shaped, so it is never given payload storage in the first
		// place, and there is no constant of the type to render.
		return false
	default:
		_, representable := subOf(sourceType)
		return representable
	}
}

// allocateInterfacePayload makes the storage an interface's data word points
// at, for a source type that is not pointer-shaped and so needs storage at all.
//
// It is allocateTyped plus one thing: where the runtime has a conversion helper
// for the source type, the candidate records it, and opt.LowerHeapAllocations
// calls the helper instead of the allocator for the payloads that have to leave
// the frame. runtime.convT64 and its siblings return a pointer into
// runtime.staticuint64s for a value that fits in a byte, so `any(7)` costs
// nothing rather than an 8-byte object. The frame case is untouched: a payload
// that stays local is still a frame slot, which is cheaper than any call.
//
// The store that follows this call in adaptValueToInterface is the
// initialization ir.Block.HeapAllocConverted's contract requires to sit
// immediately after the candidate. Nothing may be emitted between them.
func (g *gen) allocateInterfacePayload(value ir.Ref, sourceType types.Type) ir.Ref {
	conversion, ok := g.interfaceConversionHelper(sourceType)
	if !ok {
		return g.allocateTyped(sourceType)
	}
	// Consumed rather than read: the slot belongs to one payload of one variadic
	// call, and the conversion below is that payload. Leaving it set would offer
	// the same reserved bytes to whatever the next conversion in this call
	// happens to be.
	slot := g.variadicPayloadSlot
	g.variadicPayloadSlot = nil
	if slot != nil {
		payload := g.cur.HeapAllocConvertedField(
			g.fn.Sym("runtime.newobject", 0),
			g.runtimeType(sourceType),
			g.fn.Sym(conversion.symbol, 0),
			g.convertedPayloadValue(conversion, value),
			slot.container,
			slot.offset,
			int(typeSize(sourceType)),
			int(typeAlign(sourceType)),
		)
		return payload
	}
	payload := g.cur.HeapAllocConverted(
		g.fn.Sym("runtime.newobject", 0),
		g.runtimeType(sourceType),
		g.fn.Sym(conversion.symbol, 0),
		g.convertedPayloadValue(conversion, value),
		int(typeSize(sourceType)),
		int(typeAlign(sourceType)),
	)
	// A converted payload's type has no pointer words -- interfaceConversionHelper
	// only accepts types that have none -- so there is nothing here to mark.
	return payload
}

// interfaceConversion names one runtime conversion helper and how the value has
// to be presented to it.
type interfaceConversion struct {
	symbol string
	// size is the payload width the helper is being used for. gc picks its
	// helper by size and alignment rather than by kind, and a target whose int
	// was not eight bytes wide would pair the wrong helper with the kinds below,
	// so the kind that chose the helper also states the layout it assumed and
	// interfaceConversionHelper checks it against the layout goc computed.
	size int64
	// form is what has to happen to the source value to become the helper's
	// argument. The helper takes an unsigned integer and reads it as a small
	// non-negative number when deciding whether it can use the static table, so
	// a byte-wide payload has to arrive zero-extended and a float has to arrive
	// as its bits rather than as its value.
	form payloadArgumentForm
}

type payloadArgumentForm int

const (
	payloadArgumentAsIs payloadArgumentForm = iota
	payloadArgumentZeroExtendByte
	payloadArgumentZeroExtendHalfword
	payloadArgumentFloatBits32
	payloadArgumentFloatBits64
)

// interfaceConversionHelper reports the runtime helper that builds the
// interface payload for sourceType, if there is one.
//
// This is cmd/compile's dataWordFuncName, narrowed twice.
//
// The first narrowing is to types goc holds in a register. convT16/convT32/
// convT64 take the value itself, so a payload goc represents as an address --
// a struct, an array, a string, a slice -- would have to be spilled and read
// back to be passed, which is the copy the fast path exists to avoid. gc's
// convT/convTnoptr/convTstring/convTslice cover those, and are deliberately not
// wired here: measured against go1.26.1, convTstring and convTslice allocate
// for every value except the empty one, so they would buy no allocation goc is
// not already paying. See CCWORK_REPORT.md.
//
// The second is to targets where runtime.staticuint64s exists. It is defined in
// runtime/ints.s, and only the ARM64 translator consumes that file; on a target
// that does not, the array is a zero-filled Go var and the helper would hand
// back a pointer to a zero for every value below 256. That is a wrong answer,
// not a slow one, so the fast path is off there.
//
// It answers only about types isDirectInterfaceType rejects. adaptValueToInterface
// asks it after that question, and a pointer-shaped value never reaches storage
// at all.
func (g *gen) interfaceConversionHelper(sourceType types.Type) (interfaceConversion, bool) {
	if !g.runtimeAllocation || g.target != TargetARM64 {
		return interfaceConversion{}, false
	}
	if isDirectInterfaceType(sourceType) || isAddressRepresentedInterfacePayload(sourceType) {
		return interfaceConversion{}, false
	}
	basic, ok := sourceType.Underlying().(*types.Basic)
	if !ok {
		return interfaceConversion{}, false
	}
	var conversion interfaceConversion
	switch basic.Kind() {
	case types.Bool, types.Int8, types.Uint8:
		// The runtime has no convT8. gc inlines the &staticuint64s[value] lookup
		// for a one-byte payload instead; convT64 handed a zero-extended byte
		// computes the same address, is always on the table's fast path because
		// the table has 256 entries, and reads back correctly because the
		// payload type's own size is what a reader uses.
		conversion = interfaceConversion{symbol: "runtime.convT64", size: 1, form: payloadArgumentZeroExtendByte}
	case types.Int16, types.Uint16:
		conversion = interfaceConversion{symbol: "runtime.convT16", size: 2, form: payloadArgumentZeroExtendHalfword}
	case types.Int32, types.Uint32:
		conversion = interfaceConversion{symbol: "runtime.convT32", size: 4, form: payloadArgumentAsIs}
	case types.Float32:
		conversion = interfaceConversion{symbol: "runtime.convT32", size: 4, form: payloadArgumentFloatBits32}
	case types.Int, types.Uint, types.Int64, types.Uint64, types.Uintptr:
		conversion = interfaceConversion{symbol: "runtime.convT64", size: 8, form: payloadArgumentAsIs}
	case types.Float64:
		conversion = interfaceConversion{symbol: "runtime.convT64", size: 8, form: payloadArgumentFloatBits64}
	default:
		return interfaceConversion{}, false
	}
	if typeSize(sourceType) != conversion.size || typeAlign(sourceType) != conversion.size {
		return interfaceConversion{}, false
	}
	return conversion, true
}

// convertedPayloadValue puts a source value into the class and width its
// conversion helper takes its argument in.
func (g *gen) convertedPayloadValue(conversion interfaceConversion, value ir.Ref) ir.Ref {
	switch conversion.form {
	case payloadArgumentZeroExtendByte:
		return g.cur.Extub(ir.ClsL, value)
	case payloadArgumentZeroExtendHalfword:
		return g.cur.Extuh(ir.ClsW, value)
	case payloadArgumentFloatBits32:
		return g.cur.Cast(ir.ClsW, value)
	case payloadArgumentFloatBits64:
		return g.cur.Cast(ir.ClsL, value)
	}
	return value
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
	key := runtimeTypeKey(g.fset, sourceType) + "->" + runtimeTypeKey(g.fset, targetType)
	if symbol := g.interfaceItabs[key]; symbol != "" {
		return symbol
	}

	symbol := contentSymbolName(".goc.itab", key)
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
		object, indexes, _ := types.LookupFieldOrMethod(sourceType, true, interfaceMethod.Pkg(), interfaceMethod.Name())
		method, ok := object.(*types.Func)
		if !ok {
			g.err = fmt.Errorf("goc: %s does not implement %s", sourceType, targetType)
			items = append(items, ir.DataItem{Sub: ir.SubL, Ints: []int64{0}})
			continue
		}
		wrapper := g.ensureInterfaceCallWrapperFor(sourceType, method, indexes)
		items = append(items, ir.DataItem{Sub: ir.SubL, Sym: wrapper})
	}
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:         symbol,
		Align:        8,
		Items:        items,
		PointerWords: []int{0, 1},
	})
	return symbol
}

func (g *gen) ensureInterfaceCallWrapperFor(sourceType types.Type, method *types.Func, indexes []int) string {
	if len(indexes) <= 1 {
		return g.ensureInterfaceCallWrapper(method)
	}

	methodSymbol := g.functionSymbol(method)
	key := runtimeTypeKey(g.fset, sourceType) + "|" + methodSymbol + "|" + indexPathKey(indexes)
	if symbol := g.interfaceCallWrappers[key]; symbol != "" {
		return symbol
	}

	wrapperSymbol := contentSymbolName(methodSymbol+".interfacecall.promoted", key)
	g.interfaceCallWrappers[key] = wrapperSymbol

	signature := method.Type().(*types.Signature)
	receiver := signature.Recv()
	if receiver == nil {
		return methodSymbol
	}
	if methodHasInterfaceReceiver(method) {
		g.interfaceMethods[method] = true
	}

	var function *ir.Func
	if signature.Results().Len() == 0 {
		function = g.mod.NewFuncVoid(wrapperSymbol)
	} else {
		resultClass, supported := scalar(signature.Results().At(0).Type())
		if !supported {
			g.err = fmt.Errorf("goc: interface call wrapper %s has unsupported result %s", wrapperSymbol, signature.Results().At(0).Type())
			return methodSymbol
		}
		function = g.mod.NewFunc(wrapperSymbol, resultClass)
	}

	wrapper := g.derive()
	wrapper.fn = function
	wrapper.cur = function.Entry()
	if signature.Results().Len() > 0 {
		resultType := signature.Results().At(0).Type()
		function.RetAgg = wrapper.goABIAggregate(resultType)
		function.RetValues = wrapper.runtimeAllocation && isSliceType(resultType)
	}

	payload := function.Param("receiver", ir.ClsP)
	receiverValue := wrapper.promotedInterfaceMethodReceiver(payload, sourceType, indexes, method)

	arguments := make([]ir.Ref, 0, 1+signature.Params().Len()+signature.Results().Len())
	arguments = append(arguments, receiverValue)
	for index := 0; index < signature.Params().Len(); index++ {
		parameter := signature.Params().At(index)
		parameterClass, supported := scalar(parameter.Type())
		if !supported {
			g.err = fmt.Errorf("goc: interface call wrapper %s has unsupported parameter %s", wrapperSymbol, parameter.Type())
			return wrapperSymbol
		}
		arguments = append(arguments, wrapper.functionParameter(parameter.Name(), parameter.Type(), parameterClass))
	}
	if signature.Results().Len() > 0 && isInlineAggregate(signature.Results().At(0).Type()) && function.RetAgg == nil {
		arguments = append(arguments, function.ParamRef("result0"))
	}
	for index := 1; index < signature.Results().Len(); index++ {
		arguments = append(arguments, function.ParamRef(fmt.Sprintf("result%d", index)))
	}

	callee := function.Sym(methodSymbol, 0)
	if signature.Results().Len() == 0 {
		wrapper.callVoidWithSignature(callee, arguments, signature, receiver.Type())
		wrapper.cur.RetVoid()
		return wrapperSymbol
	}

	resultClass, _ := scalar(signature.Results().At(0).Type())
	result := wrapper.callWithSignature(resultClass, callee, arguments, signature, receiver.Type())
	wrapper.returnValue(result, signature.Results().At(0).Type())
	return wrapperSymbol
}

func indexPathKey(indexes []int) string {
	parts := make([]string, len(indexes))
	for index, value := range indexes {
		parts[index] = fmt.Sprintf("%d", value)
	}
	return strings.Join(parts, ".")
}

func (g *gen) ensureInterfaceCallWrapper(method *types.Func) string {
	methodSymbol := g.functionSymbol(method)
	signatureKey := methodSymbol + "|" + types.TypeString(method.Type(), func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		return pkg.Path()
	})
	if symbol := g.interfaceCallWrappers[signatureKey]; symbol != "" {
		return symbol
	}

	signature := method.Type().(*types.Signature)
	receiver := signature.Recv()
	if receiver == nil {
		return methodSymbol
	}
	// Named from the signature it wraps, not from how many wrappers came first.
	// An itab's method entries name these wrappers, so a counter here gives the
	// same itab two different contents in two compilations of the same program --
	// which is invisible in a monolithic build and fatal to a separately compiled
	// one, where the two copies must be interchangeable.
	wrapperSymbol := contentSymbolName(methodSymbol+".interfacecall", signatureKey)
	g.interfaceCallWrappers[signatureKey] = wrapperSymbol

	var function *ir.Func
	if signature.Results().Len() == 0 {
		function = g.mod.NewFuncVoid(wrapperSymbol)
	} else {
		resultClass, supported := scalar(signature.Results().At(0).Type())
		if !supported {
			g.err = fmt.Errorf("goc: interface call wrapper %s has unsupported result %s", wrapperSymbol, signature.Results().At(0).Type())
			return methodSymbol
		}
		function = g.mod.NewFunc(wrapperSymbol, resultClass)
	}

	wrapper := g.derive()
	wrapper.fn = function
	wrapper.cur = function.Entry()
	if signature.Results().Len() > 0 {
		resultType := signature.Results().At(0).Type()
		function.RetAgg = wrapper.goABIAggregate(resultType)
		function.RetValues = wrapper.runtimeAllocation && isSliceType(resultType)
	}

	receiverWord := function.Param("receiver", ir.ClsP)
	receiverValue := wrapper.interfaceCallReceiver(receiverWord, receiver.Type())

	arguments := make([]ir.Ref, 0, 1+signature.Params().Len()+signature.Results().Len())
	arguments = append(arguments, receiverValue)
	for index := 0; index < signature.Params().Len(); index++ {
		parameter := signature.Params().At(index)
		parameterClass, supported := scalar(parameter.Type())
		if !supported {
			g.err = fmt.Errorf("goc: interface call wrapper %s has unsupported parameter %s", wrapperSymbol, parameter.Type())
			return wrapperSymbol
		}
		arguments = append(arguments, wrapper.functionParameter(parameter.Name(), parameter.Type(), parameterClass))
	}
	if signature.Results().Len() > 0 && isInlineAggregate(signature.Results().At(0).Type()) && function.RetAgg == nil {
		arguments = append(arguments, function.ParamRef("result0"))
	}
	for index := 1; index < signature.Results().Len(); index++ {
		arguments = append(arguments, function.ParamRef(fmt.Sprintf("result%d", index)))
	}

	callee := function.Sym(methodSymbol, 0)
	if signature.Results().Len() == 0 {
		wrapper.callVoidWithSignature(callee, arguments, signature, receiver.Type())
		wrapper.cur.RetVoid()
		return wrapperSymbol
	}

	resultClass, _ := scalar(signature.Results().At(0).Type())
	result := wrapper.callWithSignature(resultClass, callee, arguments, signature, receiver.Type())
	wrapper.returnValue(result, signature.Results().At(0).Type())
	return wrapperSymbol
}

func (g *gen) interfaceCallReceiver(payload ir.Ref, receiverType types.Type) ir.Ref {
	if isDirectInterfaceType(receiverType) {
		receiverClass, supported := scalar(receiverType)
		if supported && receiverClass != ir.ClsP {
			return g.cur.Copy(receiverClass, payload)
		}
		return payload
	}
	if isInlineAggregate(receiverType) || isMemoryValue(receiverType) {
		return payload
	}
	return g.load(payload, receiverType)
}

func (g *gen) interfaceTypeWord(dynamicType ir.Ref, targetType types.Type) ir.Ref {
	if !g.runtimeAllocation || !interfaceHasMethods(targetType) {
		return dynamicType
	}
	targetInterface := targetType.Underlying().(*types.Interface)
	implementations := g.interfaceImplementations(targetInterface)
	if len(implementations) == 0 {
		return g.cur.Call(
			ir.ClsP,
			g.fn.Sym("runtime.getitab", 0),
			g.typeTag(targetType),
			dynamicType,
			g.fn.Word(0),
		)
	}

	done := g.block("interfaceitabdone")
	fallback := g.block("interfaceitabfallback")
	edges := make([]ir.PhiEdge, 0, len(implementations)+1)
	for index, implementation := range implementations {
		match := g.block(fmt.Sprintf("interfaceitabmatch%d_", index))
		next := g.block(fmt.Sprintf("interfaceitabnext%d_", index))
		matches := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicType, g.typeTag(implementation))
		g.cur.Jnz(matches, match, next)

		g.cur = match
		g.cur.Goto(done)
		edges = append(edges, ir.PhiEdge{
			From: match,
			Val:  g.fn.Sym(g.ensureInterfaceItab(implementation, targetType), 0),
		})

		g.cur = next
	}
	g.cur.Goto(fallback)

	g.cur = fallback
	runtimeItab := g.cur.Call(
		ir.ClsP,
		g.fn.Sym("runtime.getitab", 0),
		g.typeTag(targetType),
		dynamicType,
		g.fn.Word(0),
	)
	g.cur.Goto(done)
	edges = append(edges, ir.PhiEdge{From: fallback, Val: runtimeItab})

	g.cur = done
	return done.Phi(ir.ClsP, edges...)
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
	g.markTransientInterfaceDescriptor(descriptor)

	if types.Identical(sourceType, targetType) {
		// Assignment between identical interface types preserves the existing
		// type and data words. Copy them into fresh descriptor storage: returning
		// the source descriptor directly could retain a pointer to a temporary
		// call-result or parameter header.
		nilValue := g.block("interfacecopynil")
		nonNilValue := g.block("interfacecopynonnil")
		done := g.block("interfacecopyend")
		isNil := g.interfaceIsNil(value)
		g.cur.Jnz(isNil, nilValue, nonNilValue)

		g.cur = nilValue
		g.cur.Store(g.fn.ConstInt(ir.ClsP, 0), descriptor)
		g.cur.Store(g.fn.ConstInt(ir.ClsP, 0), g.offset(descriptor, 8))
		g.cur.Goto(done)

		g.cur = nonNilValue
		g.cur.Store(g.cur.Load(ir.ClsP, value), descriptor)
		data := g.cur.Load(ir.ClsP, g.offset(value, 8))
		g.cur.Store(data, g.offset(descriptor, 8))
		g.cur.Goto(done)

		g.cur = done
		return descriptor
	}

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
	return isInlineAggregate(valueType) || isComplex128Type(valueType)
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
	interfacePayloadSlots := make(map[int]variadicPayloadSlot)
	stackAllocateVariadic := !g.runtimeAllocation || g.fn.NoSplit || g.forceStackVariadic
	if !stackAllocateVariadic {
		allocationType := types.Type(arrayType)
		fields := []*types.Var{
			types.NewVar(token.NoPos, nil, "values", arrayType),
		}
		// Recorded in argument order rather than in a map: each entry emits an
		// address computation below, so a map would let iteration order pick the
		// order those instructions -- and therefore every temporary numbered
		// after them -- appear in.
		var payloadFields []variadicPayloadField
		if isInterfaceValue(elementType) {
			for index, argument := range variadicArguments {
				if !g.interfaceAssignmentNeedsPayload(argument) {
					continue
				}
				fieldType := g.typeAndValue(argument).Type
				payloadFields = append(payloadFields, variadicPayloadField{
					argument: index,
					field:    len(fields),
					split:    g.variadicPayloadStorage(fieldType) == payloadStorageOwnAllocation,
				})
				fieldName := fmt.Sprintf("payload%d", index)
				fields = append(fields, types.NewVar(token.NoPos, nil, fieldName, fieldType))
			}
		}
		if len(fields) > 1 {
			combinedType := types.NewStruct(fields, nil)
			allocationType = combinedType
			offsets := structOffsets(fields)
			backing = g.allocateTyped(allocationType)
			for _, payloadField := range payloadFields {
				if payloadField.split {
					// The payload gets an allocation of its own so its placement
					// is decided apart from the array's, and the field stays
					// reserved so opt.LowerHeapAllocations can fold it back in
					// when the array turned out to be on the heap anyway.
					interfacePayloadSlots[payloadField.argument] = variadicPayloadSlot{
						container: backing,
						offset:    offsets[payloadField.field],
					}
					continue
				}
				interfacePayloads[payloadField.argument] = g.offset(backing, offsets[payloadField.field])
			}
		} else {
			backing = g.allocateTyped(allocationType)
		}
	} else {
		backing = g.localAllocTyped(arrayType)
		if !g.runtimeAllocation {
			g.cur.Call(ir.ClsP, g.fn.Sym("goc_memset", 0), backing, g.fn.Word(0), g.fn.Long(typeSize(arrayType)))
		}
	}
	elementSize := typeSize(elementType)
	for index, argument := range variadicArguments {
		previousSlot := g.variadicPayloadSlot
		if slot, split := interfacePayloadSlots[index]; split {
			g.variadicPayloadSlot = &slot
		}
		value := g.assignmentValueWithInterfacePayload(argument, elementType, interfacePayloads[index])
		g.variadicPayloadSlot = previousSlot
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

// variadicPayloadField names the boxed interface payload for one variadic
// argument: which argument it belongs to, and which field of the combined
// allocation holds it.
type variadicPayloadField struct {
	argument int
	field    int
	// split marks a payload emitted as an allocation of its own, whose reserved
	// field is somewhere for opt.LowerHeapAllocations to fold it back into. See
	// variadicPayloadStorage.
	split bool
}

// variadicPayloadSlot is the storage reserved inside a variadic call's combined
// object for one boxed payload that is being emitted as a separate allocation.
// It travels on gen because the payload is allocated several frames down, inside
// the interface conversion, and only that call knows the payload's type and
// value.
type variadicPayloadSlot struct {
	container ir.Ref
	offset    int64
}

// payloadStorage says where a variadic `...any` argument's boxed payload lives.
type payloadStorage int

const (
	// payloadStorageCombinedField puts the payload in a field of the same object
	// as the `[N]any` backing array, so the whole call costs one allocation.
	payloadStorageCombinedField payloadStorage = iota
	// payloadStorageOwnAllocation gives the payload an allocation candidate of
	// its own, so opt.LowerHeapAllocations decides where it goes separately from
	// the backing array.
	payloadStorageOwnAllocation
)

// variadicPayloadStorage decides which of the two a payload of the given type
// gets, and is the whole of goc's answer to "does this variadic call's backing
// array have to be on the heap".
//
// # Why there is a choice at all
//
// One object is one placement. While the array and every payload were fields of
// a single allocation, a callee that retains an *element* -- which is the boxed
// payload, not the array -- retained the array too, because they are the same
// object. That is not a conservatism that better analysis can remove: it is
// true of the representation. fmt.pp.doPrintf assigns each element to p.arg, a
// field of a heap-allocated printer, so every `fmt.Sprintf` with an argument
// paid a heap allocation for its `[N]any` even though gc keeps that array in
// the frame and always has. Splitting the payload out is what makes the two
// decidable apart.
//
// # Why not split every payload
//
// Because the merge is worth something when the array does go to the heap. A
// callee that retains the slice itself -- log/slog.Logger.Info does, through
// Record.Add -- escapes the array whatever this decides, and then N+1 objects
// cost N+1 allocations where one combined object costs one. Splitting
// unconditionally turns the five-attribute slog call from one allocation into
// six.
//
// So the split is taken exactly where it cannot lose. A payload the runtime has
// a conversion helper for costs at most one allocation on its own and often
// none -- runtime.convT64 hands back a pointer into runtime.staticuint64s for a
// value below 256 -- which is the same shape gc emits; and those types are
// pointer-free by construction, so a split payload never needs a frame pointer
// map entry the combined object was carrying. A payload with no helper stays in
// the combined object, where the old arithmetic still holds.
//
// A mixed call keeps the array's fate tied to its unsplittable payloads, which
// is why the test is per-payload rather than per-call: splitting the int out of
// `fmt.Sprintf("%s %d", s, n)` while the string payload keeps the array on the
// heap would add an allocation rather than remove one.
func (g *gen) variadicPayloadStorage(payloadType types.Type) payloadStorage {
	if !g.runtimeAllocation {
		return payloadStorageCombinedField
	}
	if _, hasHelper := g.interfaceConversionHelper(payloadType); hasHelper {
		return payloadStorageOwnAllocation
	}
	return payloadStorageCombinedField
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
	if _, static := g.staticInterfacePayload(expression, sourceType); static {
		// The payload is already in read-only data, so the combined object needs
		// no field for it. This is what keeps `logger.Info("msg", "a", 1)` from
		// building an object that points into itself; see staticInterfacePayload.
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
			selection, function = g.concreteMethodSelection(selection, function)
			receiver = g.methodReceiver(expression, selection, function)
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

func (g *gen) emitDynamicGlobalInitializer(initializer *globalInitializer) {
	if !g.live() {
		return
	}
	object := initializer.objects[0]
	guardName := g.dynamicInitializerGuards[object]
	if guardName == "" {
		// Position, not just the name: a package may declare several blank
		// globals with initializers, and every one of them is called "_".
		guardName = contentSymbolName(".goc.global.init",
			objectPackagePath(object)+"."+object.Name()+"@"+g.fset.Position(object.Pos()).String())
		for _, groupObject := range initializer.objects {
			g.dynamicInitializerGuards[groupObject] = guardName
		}
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
	for _, groupObject := range initializer.objects {
		g.initializingGlobals[groupObject] = true
	}

	values := make([]ir.Ref, 1)
	if len(initializer.resultIndices) > 1 || initializer.resultIndices[0] != 0 {
		call, ok := initializer.expression.(*ast.CallExpr)
		if !ok {
			g.fail(initializer.expression, "multi-valued global initializer is not a call")
			return
		}
		values, _ = g.evaluateMultiValueCall(call)
	} else {
		values[0] = g.assignmentValue(initializer.expression, object.Type())
	}

	for index, groupObject := range initializer.objects {
		resultIndex := initializer.resultIndices[index]
		g.storeDynamicGlobalInitializer(groupObject, values[resultIndex])
	}

	for _, groupObject := range initializer.objects {
		delete(g.initializingGlobals, groupObject)
	}
	g.info = savedInfo
	g.pkg = savedPackage
	g.parents = savedParents
	g.currentBody = savedBody
	if g.live() {
		g.cur.Goto(done)
	}
	g.cur = done
}

func (g *gen) storeDynamicGlobalInitializer(object types.Object, value ir.Ref) {
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
}

func (g *gen) typeTag(valueType types.Type) ir.Ref {
	return g.fn.Sym(g.ensureTypeTag(valueType), 0)
}

func (g *gen) ensureTypeTag(valueType types.Type) string {
	valueType = canonicalAliasType(valueType)
	key := runtimeTypeKey(g.fset, valueType)
	if g.runtimeTypes != nil {
		g.runtimeTypes[key] = valueType
	}
	name := g.typeTags[key]
	if name == "" {
		// Named from the type rather than from a count of the types seen so
		// far. The counter recorded the order types were discovered in, which
		// came from map traversal, so the same source produced differently
		// named symbols on every build.
		name = runtimeTypeSymbolName(key)
		g.typeTags[key] = name
		gcDataName := name + ".gcdata"
		mask := paddedPointerMask(pointerMask(valueType))
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
			interfaceMethodSymbol := g.ensureInterfaceCallWrapper(method.function)
			items = append(items,
				ir.DataItem{Sub: ir.SubW, Sym: methodName, RelativeTo: ".goc.runtime.datastart"},
				ir.DataItem{Sub: ir.SubW, Sym: methodType, RelativeTo: ".goc.runtime.datastart"},
				ir.DataItem{Sub: ir.SubW, Sym: interfaceMethodSymbol},
				ir.DataItem{Sub: ir.SubW, Sym: methodSymbol},
			)
		}
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name: gcDataName, Align: int(pointerSize()), Items: []ir.DataItem{{Sub: ir.SubUB, Ints: mask}},
		}, &ir.Data{
			Name: name, Align: 8, Items: items,
			// The descriptor belongs in moduledata.typelinks only when it is
			// complete: runtime.typesEqual reads the type's name and its
			// kind-specific tail, and without the runtime tables there is no name
			// to read. See ir.Data.GoTypeLink.
			GoTypeLink: g.emitRuntimeTables,
		})
	}
	return name
}

func populateRuntimePointerTypes(fset *token.FileSet, module *ir.Module, typeTags map[string]string, runtimeTypes map[string]types.Type) {
	dataByName := make(map[string]*ir.Data, len(module.Data))
	for _, data := range module.Data {
		dataByName[data.Name] = data
	}
	for key, valueType := range runtimeTypes {
		typeSymbol := typeTags[key]
		pointerSymbol := typeTags[runtimeTypeKey(fset, types.NewPointer(valueType))]
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
		methods = append(methods, runtimeMethod{function: function, signature: runtimeMethodSignature(signature)})
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

func runtimeMethodSignature(signature *types.Signature) *types.Signature {
	return types.NewSignatureType(
		nil,
		nil,
		nil,
		signature.Params(),
		signature.Results(),
		signature.Variadic(),
	)
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

func runtimeTypeKey(fset *token.FileSet, valueType types.Type) string {
	valueType = canonicalAliasType(valueType)
	if signature, ok := valueType.(*types.Signature); ok {
		parameters := runtimeAnonymousTuple(signature.Params())
		results := runtimeAnonymousTuple(signature.Results())
		valueType = types.NewSignatureType(nil, nil, nil, parameters, results, signature.Variadic())
	}
	key := types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Path()
	})
	return appendLocalTypeIdentities(fset, key, valueType)
}

func runtimeAnonymousTuple(tuple *types.Tuple) *types.Tuple {
	variables := make([]*types.Var, tuple.Len())
	for index := 0; index < tuple.Len(); index++ {
		variables[index] = types.NewVar(token.NoPos, nil, "", tuple.At(index).Type())
	}
	return types.NewTuple(variables...)
}

// runtimeTypeName is a type descriptor's Str -- what reflect.Type.String()
// returns.
//
// It has to canonicalize a signature the same way runtimeTypeKey does, and for
// two reasons. Go's spec spells a function type without parameter names, so
// `func(uint64)` is the answer reflect must give; and the key is what decides the
// descriptor's identity, so a name derived differently from the key means one
// descriptor whose text depends on which declaration the compiler happened to
// describe first. Two compilations of the same program could then disagree about
// what reflect prints -- which is how this was found, when a separately compiled
// runtime and program produced `func(v uint64)` and `func(x uint64)` for one type.
func runtimeTypeName(valueType types.Type) string {
	if signature, ok := canonicalAliasType(valueType).(*types.Signature); ok {
		valueType = types.NewSignatureType(nil, nil, nil,
			runtimeAnonymousTuple(signature.Params()),
			runtimeAnonymousTuple(signature.Results()),
			signature.Variadic())
	}
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
				if item.Sub != ir.SubW || item.Sym == "" || item.RelativeTo != "" {
					continue
				}
				if functions[item.Sym] && !interfaceCallWrapperTargetUnavailable(item.Sym, functions) {
					continue
				}
				*item = ir.DataItem{Sub: ir.SubW, Sym: "runtime.unreachableMethod"}
			}
		}
		if strings.HasPrefix(data.Name, ".goc.itab.") {
			for index := 4; index < len(data.Items); index++ {
				item := &data.Items[index]
				if item.Sub != ir.SubL || item.Sym == "" {
					continue
				}
				if functions[item.Sym] && !interfaceCallWrapperTargetUnavailable(item.Sym, functions) {
					continue
				}
				*item = ir.DataItem{Sub: ir.SubL, Sym: "runtime.unreachableMethod"}
			}
		}
	}
}

// redirectUnavailableInterfaceCallWrappers points an interface-call wrapper whose
// method is not compiled into this module at runtime.unreachableMethod, and
// returns the wrappers it redirected.
//
// The returned names matter to the driver split: a redirected wrapper is a
// function whose body depends on what the module reached, and a prebuilt runtime
// module reaches less than any program linked against it. Leaving one in the pack
// would give the program a wrapper that throws where its own compilation would
// have called the real method.
func redirectUnavailableInterfaceCallWrappers(module *ir.Module) []string {
	var redirected []string
	functions := make(map[string]bool, len(module.Funcs))
	for _, function := range module.Funcs {
		functions[function.Name] = true
	}
	for _, function := range module.Funcs {
		methodSymbol, ok := interfaceCallWrapperMethodSymbol(function.Name)
		if !ok || functions[methodSymbol] {
			continue
		}
		redirected = append(redirected, function.Name)
		unreachable := function.Sym("runtime.unreachableMethod", 0)
		for _, block := range function.Blocks {
			for instructionIndex := range block.Instrs {
				instruction := &block.Instrs[instructionIndex]
				if instruction.Op == ir.OCall && len(instruction.Args) > 0 {
					instruction.Args[0] = unreachable
				}
			}
		}
	}
	return redirected
}

func interfaceCallWrapperTargetUnavailable(symbol string, functions map[string]bool) bool {
	methodSymbol, wrapper := interfaceCallWrapperMethodSymbol(symbol)
	return wrapper && !functions[methodSymbol]
}

func interfaceCallWrapperMethodSymbol(symbol string) (string, bool) {
	const suffix = ".interfacecall"
	if strings.HasSuffix(symbol, suffix) {
		return strings.TrimSuffix(symbol, suffix), true
	}
	const numbered = ".interfacecall."
	if index := strings.Index(symbol, numbered); index >= 0 {
		return symbol[:index], true
	}
	const promoted = ".interfacecall.promoted."
	if index := strings.Index(symbol, promoted); index >= 0 {
		return symbol[:index], true
	}
	return "", false
}

// emitRuntimeTypeHasher gives a type its runtime.typehash trampoline, which a
// map descriptor's Hasher field points at.
//
// The symbol is derived from the key type's tag, so every map type with the same
// key wants the same trampoline. net/http alone has three map types keyed by
// connectMethodKey, and without this guard the module carried three identical
// definitions of one symbol -- harmless while it was local, a duplicate-symbol
// error the moment a prebuilt runtime pack exports it. The guard matches
// ensureRuntimeTypeEqual's, which has always had one.
func (g *gen) emitRuntimeTypeHasher(valueType types.Type, typeTag string) string {
	symbol := typeTag + ".hash"
	for _, existing := range g.mod.Funcs {
		if existing.Name == symbol {
			return symbol
		}
	}
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
	case *types.Pointer:
		return !isNotInHeapPointerType(value)
	case *types.Map, *types.Chan, *types.Signature:
		return true
	case *types.Basic:
		return value.Kind() == types.UnsafePointer
	default:
		return false
	}
}

func isNotInHeapPointerType(pointer *types.Pointer) bool {
	return typeEmbedsNotInHeap(pointer.Elem(), make(map[types.Type]bool))
}

// isNotInHeapPointer reports whether valueType is a pointer to a not-in-heap
// type. internal/runtime/sys.NotInHeap documents that such a pointer never
// refers to a garbage-collected object, and that write barriers on it may
// therefore be omitted; the standard compiler implements that by reporting zero
// pointer-data bytes for the type. cg12 must omit them too, and not merely as
// an optimization: its write barrier reads the destination slot's previous
// contents to record the deleted pointer, so barriering a store of a
// not-in-heap pointer publishes an address that is not an object base -- for
// example runtime.heapBitsSlice's pointer into a span's own metadata tail --
// into the write barrier buffer, where the marker later mistakes it for a heap
// object.
func isNotInHeapPointer(valueType types.Type) bool {
	pointer, ok := valueType.Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	return isNotInHeapPointerType(pointer)
}

func typeEmbedsNotInHeap(valueType types.Type, seen map[types.Type]bool) bool {
	valueType = canonicalAliasType(valueType)
	if seen[valueType] {
		return false
	}
	seen[valueType] = true

	if named, ok := valueType.(*types.Named); ok {
		object := named.Obj()
		if object != nil && object.Name() == "NotInHeap" {
			if pkg := object.Pkg(); pkg != nil && pkg.Path() == "internal/runtime/sys" {
				return true
			}
		}
		return typeEmbedsNotInHeap(named.Underlying(), seen)
	}

	structure, ok := valueType.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for fieldIndex := 0; fieldIndex < structure.NumFields(); fieldIndex++ {
		field := structure.Field(fieldIndex)
		if field.Embedded() && typeEmbedsNotInHeap(field.Type(), seen) {
			return true
		}
		if field.Name() == "_" && typeEmbedsNotInHeap(field.Type(), seen) {
			return true
		}
	}
	return false
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

// paddedPointerMask returns a type's pointer bitmap padded with zero bytes to a
// whole number of pointer-sized words, which is the length the runtime requires.
//
// The runtime never reads an abi.Type's GCData a byte at a time. Every reader
// goes through runtime.readUintptr, which loads a whole uintptr:
// typePointersOfType takes the first word as its mask, and typePointers.next and
// fastForward take later words at eight-byte offsets. A mask emitted at its
// exact significant length therefore has the *next symbol* read as part of it,
// and every 1 bit in those bytes becomes a phantom pointer word at an offset far
// outside the object -- which bulkBarrierPreWrite and the scan then dereference.
//
// The host toolchain does the same rounding for the same reason; see
// cmd/compile/internal/reflectdata/reflect.go's dgcptrmask, "Runtime wants
// ptrmasks padded to a multiple of uintptr in size".
func paddedPointerMask(mask []int64) []int64 {
	width := int(pointerSize())
	length := len(mask)
	if length == 0 {
		length = 1
	}
	padded := (length + width - 1) / width * width
	if padded == len(mask) {
		return mask
	}
	result := make([]int64, padded)
	copy(result, mask)
	return result
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
	if runtimeImplicitNoSplit(g.pkg, fd.Name.Name) {
		g.fn.NoSplit = true
	}
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
	g.resultObjects = resultObjectSet(originalSignature)
	g.labels = make(map[string]*ir.Block)
	g.labeledBreaks = make(map[string]*ir.Block)
	g.labeledContinues = make(map[string]*ir.Block)
	g.deferSlots = make(map[*ast.DeferStmt]ir.Ref)
	g.deferFunctions = make(map[*ast.DeferStmt]ir.Ref)
	g.deferOrder = nil
	g.deferActions = nil
	// Kept out of the next function, like the rest of this block and like
	// derive() already does for a closure. addDeferRecoveryEdges wires every
	// block in here to the recovery block, so carrying the previous function's
	// blocks over gives the *previous* function a synthetic control-flow edge
	// into *this* function's blocks -- one function's dominance and liveness
	// then span another's.
	g.deferBlocks = nil
	g.runningDefers = false
	g.parents = astParents(fd.Body)
	g.currentBody = fd.Body
	predeclaredVariables := signatureVariables(originalSignature)
	g.escapeWalkOuterObjects = predeclaredVariables
	g.escapingCaptures = g.findEscapingCaptures(fd.Body, predeclaredVariables...)
	g.iterationCaptures = g.findIterationCaptures(fd.Body)
	g.referenceCaptures = g.findReferenceCaptures(fd.Body, predeclaredVariables...)
	keepAliveObjects, orderedKeepAliveObjects := g.findKeepAliveObjects(fd.Body)
	g.keepAliveObjects = keepAliveObjects
	g.keepAliveValues = make(map[types.Object]ir.Ref)
	g.keepAliveSlots = make(map[types.Object]ir.Ref)
	g.transientInterfaceDescriptors = make(map[uint32]bool)
	g.seq = 0
	g.cur = g.fn.Entry()
	g.at(fd)
	g.declareKeepAliveSlots(orderedKeepAliveObjects)
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
		g.trackKeepAliveAssignment(originalParameter, p, v.Type())
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
		if result.Name() != "" {
			g.resultSlot = g.resultStorage(originalResult, result.Type())
		} else {
			g.resultSlot = g.resultStorage(nil, result.Type())
		}
	}
	g.stmts(fd.Body.List)
	if g.err == nil && !g.live() && g.runtimeAllocation && len(g.deferActions) != 0 {
		deferReturn := g.block("deferreturn")
		deferReturn.SecondaryEntry = true
		g.addDeferRecoveryEdges(deferReturn)
		g.cur = deferReturn
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

func runtimeImplicitNoSplit(pkg *types.Package, name string) bool {
	if pkg == nil || pkg.Path() != "runtime" {
		return false
	}
	switch name {
	case "nextFreeFast", "nextFree", "nextFreeIndex", "refill":
		// The upstream compiler normally inlines this allocator fast-path helper
		// into mallocgc variants. cg12 keeps it outlined, so mark these helpers
		// nosplit to preserve the same runtime invariant: allocator fast paths
		// must not call morestack while tracing or while mallocing is set.
		return true
	default:
		return false
	}
}

func runtimeStackVariadicSymbol(symbol string) bool {
	switch symbol {
	case "runtime.traceWriter.event", "runtime.traceEventWriter.event":
		return true
	default:
		return false
	}
}

func (g *gen) functionParameter(name string, valueType types.Type, class ir.Cls) ir.Ref {
	if g.runtimeAllocation && isSliceType(valueType) {
		aggregate := g.goABIAggregate(valueType)
		parts := g.fn.ParamGroup(name, aggregate, ir.ClsP, ir.ClsL, ir.ClsL)
		value := g.fn.Aggregate(aggregate, parts...)
		return g.markManagedValue(value, valueType)
	}
	if g.runtimeAllocation && isInterfaceValue(valueType) {
		aggregate := g.goABIAggregate(valueType)
		parts := g.fn.ParamGroup(name, aggregate, ir.ClsP, ir.ClsP)
		value := g.localAllocTyped(valueType)
		g.cur.Store(parts[0], value)
		g.cur.Store(parts[1], g.offset(value, 8))
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
			selection, object = g.concreteMethodSelection(selection, object)
			receiver = g.methodReceiver(function, selection, object)
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

	// The destinations are resolved before the call's arguments, so the
	// statement follows Go's assignment order and so a map element on the left
	// is written through the map runtime rather than as though it were an index
	// into a slice.
	targets := make([]assignmentTarget, len(statement.Lhs))
	for i, lhs := range statement.Lhs {
		targets[i] = g.prepareAssignmentTarget(lhs, statement.Tok == token.DEFINE)
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

	for i, target := range targets {
		if target.kind == assignmentTargetDiscarded {
			continue
		}
		resultType := signature.Results().At(i).Type()
		value := values[i]
		if i > 0 && (!isInlineAggregate(resultType) || (g.runtimeAllocation && isSliceType(resultType))) {
			value = g.load(value, resultType)
		}
		g.storeAssignmentTarget(target, value, resultType)
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
	if _, ok := targetType.Underlying().(*types.Interface); ok {
		match = g.interfaceTypeMatch(dynamicTag, targetType)
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
	successFrom := g.cur
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
			ir.PhiEdge{From: successFrom, Val: successValue},
			ir.PhiEdge{From: failure, Val: failureValue},
		)
	}
	asserted := done.Phi(ir.ClsW,
		ir.PhiEdge{From: successFrom, Val: g.fn.Word(1)},
		ir.PhiEdge{From: failure, Val: g.fn.Word(0)},
	)
	return value, asserted
}

// interfaceTypeMatch answers, as a word, whether the dynamic type dynamicTag
// implements the interface targetType -- the test behind `x.(I)`, `x.(I)` with
// comma-ok, and an interface case of a type switch.
//
// The list of types that implement an interface is built from every method
// declared anywhere in the program, so an inline chain of comparisons against it
// makes an ordinary function's body depend on the whole program. That is fine
// for a monolithic build and wrong for a split one: a function the program
// module subtracts was compiled into the pack against the *pack's* method set,
// which is a subset of the program's, so the chain is missing the very
// candidates the program added. `stdlib_http_client_server.go` against a
// net/http pack asserted a type the pack had never seen, fell off the end of the
// chain and aborted.
//
// So the chain is a fast path and runtime.getitab is the answer. getitab
// consults the itab table and, failing that, walks the concrete type's method
// set; with canfail it returns nil rather than panicking. It depends on nothing
// but the two descriptors, which both modules agree on because the program owns
// the whole type region. interfaceTypeWord -- the conversion path, immediately
// above -- has always ended this way; only the test did not.
//
// It closes a second gap as well. interfaceImplementations enumerates method
// *receivers*, so a type whose method set comes entirely from an embedded field
// appears in no entry, and asserting it to an interface it genuinely implements
// used to answer no.
func (g *gen) interfaceTypeMatch(dynamicTag ir.Ref, targetType types.Type) ir.Ref {
	target, isInterface := targetType.Underlying().(*types.Interface)
	if !isInterface || target.NumMethods() == 0 {
		return g.fn.Word(1)
	}
	implementations := g.interfaceImplementations(target)
	if !g.runtimeAllocation {
		// The freestanding subset has no Go runtime to ask, so the chain is all
		// there is. It is also not split, so the chain is complete.
		match := g.fn.Word(0)
		for _, implementation := range implementations {
			matchesImplementation := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(implementation))
			match = g.cur.Or(ir.ClsW, match, matchesImplementation)
		}
		return match
	}

	done := g.block("ifacematchdone")
	fallback := g.block("ifacematchfallback")
	edges := make([]ir.PhiEdge, 0, len(implementations)+1)
	for index, implementation := range implementations {
		matched := g.block(fmt.Sprintf("ifacematchhit%d_", index))
		next := g.block(fmt.Sprintf("ifacematchnext%d_", index))
		matchesImplementation := g.cur.Cmp(ir.CmpEq, ir.ClsP, dynamicTag, g.typeTag(implementation))
		g.cur.Jnz(matchesImplementation, matched, next)

		g.cur = matched
		g.cur.Goto(done)
		edges = append(edges, ir.PhiEdge{From: matched, Val: g.fn.Word(1)})

		g.cur = next
	}
	g.cur.Goto(fallback)

	g.cur = fallback
	itab := g.cur.Call(
		ir.ClsP,
		g.fn.Sym("runtime.getitab", 0),
		g.typeTag(targetType),
		dynamicTag,
		g.fn.Word(1),
	)
	implemented := g.cur.Cmp(ir.CmpNe, ir.ClsP, itab, g.fn.ConstInt(ir.ClsP, 0))
	g.cur.Goto(done)
	edges = append(edges, ir.PhiEdge{From: fallback, Val: implemented})

	g.cur = done
	return done.Phi(ir.ClsW, edges...)
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

// assignmentTargetKind classifies where an assignment stores its value.
type assignmentTargetKind int

const (
	// assignmentTargetDiscarded is the blank identifier: the value is produced
	// and thrown away.
	assignmentTargetDiscarded assignmentTargetKind = iota
	// assignmentTargetVariable names a variable this function holds storage for,
	// which includes the package-level variables it can reach by symbol.
	assignmentTargetVariable
	// assignmentTargetAddress is any other assignable operand -- a struct field,
	// an index into an array, slice or pointer, or a pointer indirection --
	// whose storage holds the value itself.
	assignmentTargetAddress
	// assignmentTargetMapElement is an index into a map, which is written
	// through the map runtime rather than by storing to an address.
	assignmentTargetMapElement
)

// assignmentTarget is one prepared destination of an assignment. Preparing a
// target evaluates the operands its address depends on without storing
// anything, because Go assigns in two phases: every left operand's index
// expressions and pointer indirections are evaluated first, and only then are
// the values stored, left to right. `k, a[k] = 1, 2` therefore indexes a with
// the old k, and so does `for k, m[k] = range s` on every iteration.
type assignmentTarget struct {
	kind      assignmentTargetKind
	object    types.Object
	valueType types.Type
	source    ast.Expr

	// slot addresses the destination of every kind except a map element.
	slot ir.Ref
	// local marks an ordinary frame slot, which is one level indirect for the
	// aggregate and descriptor variables the frontend stores by reference.
	local bool
	// inline marks storage that holds an aggregate or interface value itself
	// rather than a pointer to it.
	inline bool

	// directVariable marks a variable whose own storage is its value -- what
	// variableStorage records in directValues -- so an assignment copies the
	// value's bytes into that storage instead of replacing a pointer to it.
	directVariable bool

	mapping ir.Ref
	mapKey  ir.Ref
	mapType *types.Map
}

// prepareAssignmentTarget resolves one assignment destination and evaluates the
// operands its address depends on. declare reports whether the enclosing
// statement is a short variable declaration, whose left operands may name
// variables that statement itself defines.
func (g *gen) prepareAssignmentTarget(destination ast.Expr, declare bool) assignmentTarget {
	for {
		parenthesized, ok := destination.(*ast.ParenExpr)
		if !ok {
			break
		}
		destination = parenthesized.X
	}
	if identifier, ok := destination.(*ast.Ident); ok && identifier.Name == "_" {
		return assignmentTarget{kind: assignmentTargetDiscarded, source: destination}
	}
	if index, ok := destination.(*ast.IndexExpr); ok {
		if mapType, isMap := g.typeAndValue(index.X).Type.Underlying().(*types.Map); isMap {
			return assignmentTarget{
				kind:      assignmentTargetMapElement,
				valueType: mapType.Elem(),
				source:    destination,
				mapping:   g.retainedAddress(g.expr(index.X)),
				mapKey:    g.retainedAddress(g.assignmentValue(index.Index, mapType.Key())),
				mapType:   mapType,
			}
		}
	}
	identifier, isIdentifier := destination.(*ast.Ident)
	if !isIdentifier {
		return assignmentTarget{
			kind:      assignmentTargetAddress,
			valueType: g.typeAndValue(destination).Type,
			source:    destination,
			slot:      g.retainedAddress(g.lvalue(destination)),
			inline:    true,
		}
	}

	object := g.info.Uses[identifier]
	if declare && object == nil {
		object = g.info.Defs[identifier]
	}
	valueType := g.objectType(object)
	slot, exists := g.addr(object)
	if !exists {
		slot = g.variableStorage(object, valueType)
	}
	_, global := g.globals[object]
	target := assignmentTarget{
		kind:      assignmentTargetVariable,
		object:    object,
		valueType: valueType,
		source:    destination,
		slot:      slot,
		local:     !global && !g.directValues[object],
	}
	target.inline = global && (isMemoryValue(valueType) || isDescriptorValue(valueType) || isInterfaceValue(valueType))
	if g.directValues[object] && isInlineValue(valueType) {
		target.inline = true
		target.directVariable = true
	}
	// A package-level string or slice symbol holds the address of its header, so
	// the assignment updates the header the symbol already points at.
	if global && isDescriptorValue(valueType) && !(g.runtimeAllocation && isSliceType(valueType)) {
		target.slot = g.cur.Load(ir.ClsP, target.slot)
	}
	return target
}

// retainedAddress marks a pointer a prepared destination has to survive with
// until the statement stores through it.
//
// Preparing a destination happens before the right-hand side runs, so the
// address outlives every call the right-hand side makes. Pointer arithmetic --
// how a field, index or indirection address is built -- yields a pointer-width
// value that is not a managed reference, so without this the frame's stack map
// omits it, copystack leaves it addressing the abandoned old stack, and the
// statement's store lands in freed memory with no fault of any kind. Only a
// pointer-classed temporary is marked; a scalar map key is left alone, because
// telling the collector an integer is a pointer is the opposite mistake.
func (g *gen) retainedAddress(reference ir.Ref) ir.Ref {
	if reference.Kind != ir.RefTemp || !g.fn.ClassOf(reference).IsPtr() {
		return reference
	}
	return g.fn.MarkGCRef(reference)
}

// assignmentTargetStoresInline reports whether reaching this destination means
// copying the value's bytes rather than storing one word.
//
// A direct variable qualifies whatever its type, because its storage is its
// value. Every other inline destination is an address, and there the question
// is only whether the type is one cg12 carries as an address: a complex128 in
// memory is not, so an address destination of that type keeps the word store it
// has always had.
func (g *gen) assignmentTargetStoresInline(target assignmentTarget) bool {
	if target.directVariable {
		return true
	}
	return target.inline && (isInlineAggregate(target.valueType) || isInterfaceValue(target.valueType))
}

// assignmentTargetValue reads a prepared destination, which an assignment
// operator such as `+=` needs before it can store the combined value.
func (g *gen) assignmentTargetValue(target assignmentTarget) ir.Ref {
	if target.kind == assignmentTargetMapElement {
		value, _ := g.mapLookupValue(target.mapping, target.mapKey, target.mapType, target.source)
		return value
	}
	if g.assignmentTargetStoresInline(target) &&
		!(g.runtimeAllocation && isSliceType(target.valueType)) {
		return target.slot
	}
	return g.load(target.slot, target.valueType)
}

// storeAssignmentTarget performs one assignment into a prepared destination.
// sourceType is the type of value as it was computed, or nil when the caller
// has already produced it in the destination's representation.
func (g *gen) storeAssignmentTarget(target assignmentTarget, value ir.Ref, sourceType types.Type) {
	if target.kind == assignmentTargetDiscarded {
		return
	}
	value = g.adaptAssignedValue(value, sourceType, target.valueType, target.source)
	if target.kind == assignmentTargetMapElement {
		g.mapAssignValue(target.mapping, target.mapKey, value, target.mapType, target.source)
		return
	}
	if target.local && isDescriptorValue(target.valueType) {
		value = g.copyInlineValue(value, target.valueType)
	}
	if target.local && isMemoryValue(target.valueType) {
		g.assignLocal(value, target.slot, target.valueType)
		g.trackKeepAliveAssignment(target.object, value, target.valueType)
		return
	}
	if g.assignmentTargetStoresInline(target) {
		g.storeInlineValue(value, target.slot, target.valueType)
	} else {
		g.store(value, target.slot, target.valueType)
	}
	g.trackKeepAliveAssignment(target.object, value, target.valueType)
}

// adaptAssignedValue converts a computed value into the representation its
// destination expects. Assigning a concrete value to an interface destination
// boxes it; everything else needs at most the narrow-integer coercion an
// ordinary assignment performs. Shared generic code is left alone because an
// unconstrained type parameter is already one pointer-sized value there.
func (g *gen) adaptAssignedValue(value ir.Ref, sourceType, targetType types.Type, source ast.Expr) ir.Ref {
	if targetType == nil {
		return value
	}
	if sourceType != nil && !types.Identical(sourceType, targetType) &&
		isInterfaceValue(targetType) && !isSharedTypeParameter(sourceType) {
		return g.adaptValueToInterface(value, sourceType, targetType, ir.R, source)
	}
	return g.coerce(value, targetType)
}

func (g *gen) assignResult(lhs ast.Expr, assignmentToken token.Token, value ir.Ref, valueType types.Type) {
	target := g.prepareAssignmentTarget(lhs, assignmentToken == token.DEFINE)
	g.storeAssignmentTarget(target, value, valueType)
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
				g.trackKeepAliveAssignment(obj, v, objectType)
			}
		}
	case *ast.AssignStmt:
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
		// Every destination is resolved before any value is produced or stored, so
		// the statement follows Go's two-phase assignment order: `k, a[k] = 1, 2`
		// indexes a with the k the statement is about to overwrite.
		targets := make([]assignmentTarget, len(n.Lhs))
		for i, lhs := range n.Lhs {
			targets[i] = g.prepareAssignmentTarget(lhs, n.Tok == token.DEFINE)
		}
		vals := make([]ir.Ref, len(n.Rhs))
		for i, e := range n.Rhs {
			targetType := targets[i].valueType
			if targetType == nil {
				targetType = g.typeAndValue(n.Lhs[i]).Type
			}
			if targetType == nil {
				targetType = g.typeAndValue(e).Type
			}
			vals[i] = g.assignmentValue(e, targetType)
			if len(n.Rhs) > 1 {
				vals[i] = g.snapshotAssignmentValue(vals[i], targetType)
			}
		}
		for i, target := range targets {
			if target.kind == assignmentTargetDiscarded {
				continue
			}
			value := vals[i]
			if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
				old := g.assignmentTargetValue(target)
				value = g.binary(n.Tok-token.ADD_ASSIGN+token.ADD, old, value, target.valueType, n)
			}
			g.storeAssignmentTarget(target, value, nil)
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
			g.deferBlocks = append(g.deferBlocks, g.cur)
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
			selection, function = g.concreteMethodSelection(selection, function)
			receiver = g.methodReceiver(target, selection, function)
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

	wrapper := g.derive()
	wrapper.fn = g.mod.NewFuncVoid(wrapperName)
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

	var closure ir.Ref
	if g.deferredFunctionValueStaysInFrame(call, g.parents, g.currentBody) {
		// Zeroed rather than merely reserved: the pointer words below are in the
		// frame's stack map from the moment the slot exists, and the argument
		// expressions that fill them can contain calls, so a safepoint can scan
		// the slot before any of them has been written.
		closure = g.localAllocTyped(closureType)
		g.zero(closure, closureType)
		g.recordPlacement(closure, "deferred-call-closure", ir.AllocInFrame, closureType)
	} else {
		closure = g.allocateTyped(closureType)
	}
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

// addDeferRecoveryEdges mirrors the Go compiler's synthetic edge from every
// defer registration to the shared recovery exit. The runtime never follows
// these edges as ordinary branches; they keep named results and other frame
// state live on the metadata-entered deferreturn path.
func (g *gen) addDeferRecoveryEdges(recovery *ir.Block) {
	seen := make(map[*ir.Block]bool)
	for _, block := range g.deferBlocks {
		if block == nil || seen[block] {
			continue
		}
		seen[block] = true
		block.SyntheticSuccs = append(block.SyntheticSuccs, recovery)
	}
}

// declareRangeVariable gives one side of a `range` clause its storage before
// the loop starts and reports the variable it names, together with whether the
// clause declares that variable.
//
// Only a declared iteration variable is per-iteration under Go 1.22 semantics;
// `for k, v = range x` assigns to operands that already exist and keeps their
// single storage. Such an operand need not be an identifier at all -- Go allows
// any assignable expression there -- and even when it is one it may name a
// package-level variable, which already has storage. Handing either of those to
// variableStorage would give it a fresh frame slot and silently discard every
// iteration's assignment, so nothing is allocated unless the clause really does
// declare a new variable or the variable has no storage yet.
func (g *gen) declareRangeVariable(statement *ast.RangeStmt, expression ast.Expr) (types.Object, bool) {
	identifier, ok := expression.(*ast.Ident)
	if !ok || identifier.Name == "_" {
		return nil, false
	}
	if declared := g.info.Defs[identifier]; declared != nil {
		g.variableStorage(declared, g.objectType(declared))
		return declared, statement.Tok == token.DEFINE
	}
	object := g.info.Uses[identifier]
	if object == nil {
		return nil, false
	}
	if _, exists := g.addr(object); !exists {
		g.variableStorage(object, g.objectType(object))
	}
	return object, false
}

// rangeTargets prepares the clause's key and element destinations for the
// iteration that is about to run. A declared variable that must be
// per-iteration gets its own storage first; every other form has its address
// operands evaluated here, before either value is stored, which is the order Go
// requires and what makes `for k, m[k] = range s` index m with the previous
// iteration's key.
func (g *gen) rangeTargets(statement *ast.RangeStmt, key, value types.Object) (assignmentTarget, assignmentTarget) {
	declare := statement.Tok == token.DEFINE
	if declare && g.perIterationVariable(key) {
		g.startIterationVariable(key, g.objectType(key))
	}
	if declare && g.perIterationVariable(value) {
		g.startIterationVariable(value, g.objectType(value))
	}
	var keyTarget, valueTarget assignmentTarget
	if statement.Key != nil {
		keyTarget = g.prepareAssignmentTarget(statement.Key, declare)
	}
	if statement.Value != nil {
		valueTarget = g.prepareAssignmentTarget(statement.Value, declare)
	}
	return keyTarget, valueTarget
}

// integerRangeKeyType is the type of the values `for i = range n` produces. The
// specification gives them the type of the range expression; an untyped
// constant contributes its default type instead.
func integerRangeKeyType(rangeType types.Type) types.Type {
	if basic, ok := rangeType.Underlying().(*types.Basic); ok && basic.Info()&types.IsUntyped != 0 {
		return types.Typ[types.Int]
	}
	return rangeType
}

// rangeTargetWritesMemory reports whether storing into target can change memory
// the clause has not read yet. When the key destination is one of those, the
// element has to be read before the key is stored, because both may address the
// range expression itself.
func rangeTargetWritesMemory(target assignmentTarget) bool {
	return target.kind == assignmentTargetAddress || target.kind == assignmentTargetMapElement
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
	if channelType, ok := rangeType.Underlying().(*types.Chan); ok {
		g.channelRangeStmt(statement, label, channelType)
		return
	}

	indexType := types.Typ[types.Int]
	// The loop counter is private storage. The key variable is assigned from it
	// at the top of every iteration, exactly as the spec describes, so writing to
	// the key inside the body cannot disturb the iteration and a per-iteration
	// key can live in storage of its own.
	indexSlot := g.alloc(indexType)
	g.store(g.fn.Long(0), indexSlot, indexType)

	keyObject, _ := g.declareRangeVariable(statement, statement.Key)

	// keyValueType and elementType describe the values the clause produces, which
	// are properties of the range expression rather than of the destinations. A
	// destination may have a different type -- `for _, x = range []int{1}` with x
	// an interface is ordinary Go -- and the element's size and representation
	// still have to come from the range expression.
	var keyValueType types.Type = indexType
	var elementType types.Type
	var upper ir.Ref
	var rangeData ir.Ref
	var stringDescriptor ir.Ref
	stringRange := false
	if slice, ok := rangeType.Underlying().(*types.Slice); ok {
		sliceValue := g.expr(statement.X)
		rangeData, upper, _ = g.sliceParts(sliceValue)
		elementType = slice.Elem()
	} else if array, ok := rangeType.Underlying().(*types.Array); ok {
		rangeData = g.expr(statement.X)
		upper = g.fn.Long(array.Len())
		elementType = array.Elem()
	} else if pointer, ok := rangeType.Underlying().(*types.Pointer); ok {
		if array, ok := pointer.Elem().Underlying().(*types.Array); ok {
			rangeData = g.expr(statement.X)
			upper = g.fn.Long(array.Len())
			elementType = array.Elem()
		} else {
			upper = g.expr(statement.X)
			upper = g.convert(upper, rangeType, indexType)
			keyValueType = integerRangeKeyType(rangeType)
		}
	} else if basic, ok := rangeType.Underlying().(*types.Basic); ok && (basic.Kind() == types.String || basic.Kind() == types.UntypedString) {
		stringRange = true
		stringDescriptor = g.expr(statement.X)
		rangeData = g.cur.Load(ir.ClsP, stringDescriptor)
		upper = g.cur.Load(ir.ClsL, g.offset(stringDescriptor, 8))
		elementType = types.Typ[types.Rune]
	} else {
		upper = g.expr(statement.X)
		upper = g.convert(upper, rangeType, indexType)
		keyValueType = integerRangeKeyType(rangeType)
	}

	valueObject, _ := g.declareRangeVariable(statement, statement.Value)
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
	keyTarget, valueTarget := g.rangeTargets(statement, keyObject, valueObject)
	if stringRange {
		g.storeAssignmentTarget(keyTarget, index, keyValueType)
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
		g.storeAssignmentTarget(valueTarget, runeValue, elementType)
	} else {
		var element ir.Ref
		assignsElement := valueTarget.kind != assignmentTargetDiscarded
		if assignsElement {
			elementOffset := index
			if size := typeSize(elementType); size != 1 {
				elementOffset = g.cur.Mul(ir.ClsL, index, g.fn.Long(size))
			}
			address := g.cur.Add(ir.ClsP, rangeData, elementOffset)
			element = address
			if (!isInlineAggregate(elementType) && !isInterfaceValue(elementType)) || (g.runtimeAllocation && isSliceType(elementType)) {
				element = g.load(address, elementType)
			}
			if rangeTargetWritesMemory(keyTarget) {
				// The key destination may address the range expression itself, so
				// the element is read before the key store can change it.
				element = g.snapshotAssignmentValue(element, elementType)
			}
		}
		g.storeAssignmentTarget(keyTarget, index, keyValueType)
		if assignsElement {
			g.storeAssignmentTarget(valueTarget, element, elementType)
		}
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

func (g *gen) channelRangeStmt(statement *ast.RangeStmt, label string, channelType *types.Chan) {
	channel := g.expr(statement.X)
	elementType := channelType.Elem()

	size := typeSize(elementType)
	if size < 4 {
		size = 4
	}
	valueAddress := g.localAlloc(4, int(size))
	visitPointerWords(elementType, 0, func(offset int64) {
		g.markStackPointerWord(valueAddress, int(offset))
	})

	valueObject, _ := g.declareRangeVariable(statement, statement.Key)

	test := g.block("rangetest")
	body := g.block("rangebody")
	post := g.block("rangepost")
	done := g.block("rangeend")
	g.cur.Goto(test)

	g.cur = test
	received := g.cur.Call(ir.ClsW, g.fn.Sym("runtime.chanrecv2", 0), channel, valueAddress)
	g.cur.Jnz(received, body, done)

	g.breaks = append(g.breaks, done)
	g.continues = append(g.continues, post)
	g.setLabeledControl(label, done, post)

	g.cur = body
	valueTarget, _ := g.rangeTargets(statement, valueObject, nil)
	if valueTarget.kind != assignmentTargetDiscarded {
		value := g.channelReceiveValue(valueAddress, elementType)
		g.storeAssignmentTarget(valueTarget, value, elementType)
	}
	g.stmts(statement.Body.List)
	if g.live() {
		g.cur.Goto(post)
	}

	g.cur = post
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
		// An assigning clause writes into the enclosing frame from inside the
		// yield function, and its destinations are not restricted to identifiers:
		// `for b.i, dst[j] = range seq` has to reach b, dst and j as well.
		captureTargetIdentifiers := func(destination ast.Expr) {
			ast.Inspect(destination, func(node ast.Node) bool {
				if identifier, ok := node.(*ast.Ident); ok {
					addCapture(g.info.Uses[identifier])
				}
				return true
			})
		}
		if statement.Key != nil {
			captureTargetIdentifiers(statement.Key)
		}
		if statement.Value != nil {
			captureTargetIdentifiers(statement.Value)
		}
	}

	position := g.fset.Position(statement.Pos())
	symbol := fmt.Sprintf("%s.rangefunc.%d.%d", g.enclosingFunctionName(), position.Line, position.Column)
	// The yield function is lowered from the range body, which lives in the
	// enclosing function's package and inherits its generic instantiation, its
	// name prefix and its write barrier setting.
	child := g.derive()
	child.info = g.info
	child.pkg = g.pkg
	child.typeArguments = g.typeArguments
	child.functionName = g.functionName
	child.currentFunction = g.currentFunction
	child.noWriteBarrier = g.noWriteBarrier
	// A yield function returns the bool that tells the iterator to keep going.
	child.resultType = types.Typ[types.Bool]
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
	child.escapeWalkOuterObjects = rangeVariables
	child.escapingCaptures = child.findEscapingCaptures(statement.Body, rangeVariables...)
	child.iterationCaptures = child.findIterationCaptures(statement.Body)
	child.referenceCaptures = child.findReferenceCaptures(statement.Body, rangeVariables...)
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
		captureAddress := child.cur.Load(ir.ClsP, child.offset(environment, int64(8*(index+1))))
		child.fn.MarkGCRef(captureAddress)
		child.vars[capture] = captureAddress
		if g.directValues[capture] {
			child.directValues[capture] = true
		}
	}
	// The destinations are prepared before either value is stored, exactly as in
	// the other range forms, so `for k, m[k] = range seq` indexes m with the key
	// the previous element left behind.
	var keyTarget, valueTarget assignmentTarget
	if statement.Key != nil {
		keyTarget = child.prepareAssignmentTarget(statement.Key, statement.Tok == token.DEFINE)
	}
	if statement.Value != nil && len(parameterValues) == 2 {
		valueTarget = child.prepareAssignmentTarget(statement.Value, statement.Tok == token.DEFINE)
	}
	if statement.Key != nil {
		child.storeAssignmentTarget(keyTarget, parameterValues[0], yieldSignature.Params().At(0).Type())
	}
	if statement.Value != nil && len(parameterValues) == 2 {
		child.storeAssignmentTarget(valueTarget, parameterValues[1], yieldSignature.Params().At(1).Type())
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

	keyObject, _ := g.declareRangeVariable(statement, statement.Key)
	valueObject, _ := g.declareRangeVariable(statement, statement.Value)
	variables := mapRangeVariables{key: keyObject, value: valueObject}
	if g.runtimeAllocation {
		g.runtimeMapRangeStmt(statement, label, mapping, mapType, variables)
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
	keyTarget, valueTarget := g.rangeTargets(statement, variables.key, variables.value)
	var key, value ir.Ref
	if keyTarget.kind != assignmentTargetDiscarded {
		keyAddress := g.mapElementAddress(mapping, mapKeysOffset, index, mapType.Key())
		key = g.mapElementValue(keyAddress, mapType.Key())
	}
	if valueTarget.kind != assignmentTargetDiscarded {
		valueAddress := g.mapElementAddress(mapping, mapValuesOffset, index, mapType.Elem())
		value = g.mapElementValue(valueAddress, mapType.Elem())
	}
	if keyTarget.kind != assignmentTargetDiscarded {
		g.storeAssignmentTarget(keyTarget, key, mapType.Key())
	}
	if valueTarget.kind != assignmentTargetDiscarded {
		g.storeAssignmentTarget(valueTarget, value, mapType.Elem())
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

// mapRangeVariables names the variables a `for ... range aMap` clause targets.
// Either field is nil when that side is absent, blank, or not an identifier at
// all; only a field the clause declares can be per-iteration.
type mapRangeVariables struct {
	key   types.Object
	value types.Object
}

func (g *gen) runtimeMapRangeStmt(
	statement *ast.RangeStmt,
	label string,
	mapping ir.Ref,
	mapType *types.Map,
	variables mapRangeVariables,
) {
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
	keyTarget, valueTarget := g.rangeTargets(statement, variables.key, variables.value)
	var key, value ir.Ref
	if keyTarget.kind != assignmentTargetDiscarded {
		key = g.mapElementValue(keyAddress, mapType.Key())
	}
	if valueTarget.kind != assignmentTargetDiscarded {
		valueAddress := g.cur.Load(ir.ClsP, g.offset(iterator, 8))
		value = g.mapElementValue(valueAddress, mapType.Elem())
	}
	if keyTarget.kind != assignmentTargetDiscarded {
		g.storeAssignmentTarget(keyTarget, key, mapType.Key())
	}
	if valueTarget.kind != assignmentTargetDiscarded {
		g.storeAssignmentTarget(valueTarget, value, mapType.Elem())
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

// isInlineValue reports whether a value of this type is carried in cg12 IR as
// the address of its storage rather than in a register, so copying one means
// copying its bytes. It is isInlineAggregate plus the two forms that are not
// aggregates and are still wider than a register: an interface value and a
// complex128.
func isInlineValue(t types.Type) bool {
	return isInlineAggregate(t) || isInterfaceValue(t) || isComplex128Type(t)
}

func isComplex128Type(t types.Type) bool {
	basic, ok := representativeType(t).Underlying().(*types.Basic)
	return ok && basic.Kind() == types.Complex128
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
		// registerAggregate is the same claim the inline-aggregate arm below
		// rests on: an interface result has a two-pointer ir.AggType, so
		// lowerGoAggregateReturn loads both words out of this pointer into the
		// result registers inside the returning block, before the epilogue
		// tears the frame down. The caller never sees the pointer, so the
		// storage need only outlive the load, and a frame slot does.
		//
		// Without it the storage has to outlive the frame, so it is a heap
		// object -- one runtime.newobject for every non-nil interface a
		// function returns anywhere in the program. sync.Pool.Get is one, which
		// is why fmt.Sprintf paid an allocation that has nothing to do with its
		// arguments.
		//
		// opt.FrameEscapes agrees rather than merely tolerating this: its
		// return rule is skipped outright when RetAgg is non-nil, for exactly
		// this reason.
		stable := ir.R
		if registerAggregate {
			stable = g.localAllocTyped(resultType)
		} else {
			stable = g.allocateTyped(resultType)
		}
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

func (g *gen) snapshotAssignmentValue(value ir.Ref, valueType types.Type) ir.Ref {
	if isMemoryValue(valueType) || isDescriptorValue(valueType) || isInterfaceValue(valueType) {
		return g.copyInlineValue(value, valueType)
	}
	return value
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
		index = g.indexOffset(index, g.typeAndValue(expression.Index).Type, typeSize(g.typeAndValue(expression).Type))
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
	if perIteration := g.perIterationForVariables(n); len(perIteration) != 0 {
		g.perIterationForStmt(n, label, perIteration)
		return
	}
	if n.Init != nil {
		g.stmt(n.Init)
	}
	test, body, post, done := g.block("fortest"), g.block("forbody"), g.block("forpost"), g.block("forend")
	g.cur.Goto(test)
	g.cur = test
	if n.Cond == nil {
		g.cur.Goto(body)
	} else {
		condition := g.expr(n.Cond)
		g.cur.Jnz(condition, body, done)
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
	g.haltUnreachableForEnd(n, done)
}

// haltUnreachableForEnd terminates the exit block of a condition-less `for`
// that nothing can branch to, so the block is still well formed.
func (g *gen) haltUnreachableForEnd(n *ast.ForStmt, done *ir.Block) {
	if n.Cond != nil {
		return
	}
	for _, block := range g.fn.Blocks {
		if block == done {
			continue
		}
		for _, successor := range block.Succs() {
			if successor == done {
				return
			}
		}
	}
	done.Hlt()
}

// perIterationForVariables returns the variables a three-clause `for` declares
// in its init statement that need their own storage per iteration.
func (g *gen) perIterationForVariables(n *ast.ForStmt) []types.Object {
	assignment, ok := n.Init.(*ast.AssignStmt)
	if !ok || assignment.Tok != token.DEFINE {
		return nil
	}
	var variables []types.Object
	for _, expression := range assignment.Lhs {
		identifier, ok := expression.(*ast.Ident)
		if !ok || identifier.Name == "_" {
			continue
		}
		object := g.info.Defs[identifier]
		if g.perIterationVariable(object) {
			variables = append(variables, object)
		}
	}
	return variables
}

// perIterationForStmt lowers a three-clause `for` whose declared variables are
// per-iteration under Go 1.22 semantics.
//
// The init statement's storage becomes a carrier that holds the value between
// iterations, and each iteration gets its own storage that is seeded from the
// carrier and written back to it when the iteration finishes. The post
// statement must act on the iteration's own storage -- `i++` has to increment
// the variable the previous iteration's closures captured -- so it moves to the
// top of the loop and is skipped on the first pass:
//
//	init'                       carrier := init value; first := true
//	header: i := carrier        fresh storage per iteration
//	        if first { first = false } else { post }
//	        if !cond { goto done }
//	body:   ...                 break -> done, continue -> post
//	post:   carrier = i
//	        goto header
//
// This is the same rewrite the standard compiler's loopvar pass performs.
func (g *gen) perIterationForStmt(n *ast.ForStmt, label string, variables []types.Object) {
	g.stmt(n.Init)
	carriers := make([]ir.Ref, len(variables))
	variableTypes := make([]types.Type, len(variables))
	for index, variable := range variables {
		carriers[index] = g.vars[variable]
		variableTypes[index] = g.objectType(variable)
	}

	booleanType := types.Typ[types.Bool]
	var firstSlot ir.Ref
	if n.Post != nil {
		firstSlot = g.alloc(booleanType)
		g.store(g.fn.Word(1), firstSlot, booleanType)
	}

	header, body, post, done := g.block("forheader"), g.block("forbody"), g.block("forpost"), g.block("forend")
	g.cur.Goto(header)

	g.cur = header
	storages := make([]ir.Ref, len(variables))
	for index, variable := range variables {
		storages[index] = g.enterIterationVariable(variable, carriers[index], variableTypes[index])
	}
	if n.Post != nil {
		firstIteration, laterIteration, posted := g.block("forfirst"), g.block("forpostrun"), g.block("forposted")
		isFirst := g.load(firstSlot, booleanType)
		g.cur.Jnz(isFirst, firstIteration, laterIteration)

		g.cur = firstIteration
		g.store(g.fn.Word(0), firstSlot, booleanType)
		g.cur.Goto(posted)

		g.cur = laterIteration
		g.stmt(n.Post)
		if g.live() {
			g.cur.Goto(posted)
		}
		g.cur = posted
	}
	if n.Cond == nil {
		g.cur.Goto(body)
	} else {
		condition := g.expr(n.Cond)
		g.cur.Jnz(condition, body, done)
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
	for index, variable := range variables {
		g.leaveIterationVariable(variable, storages[index], carriers[index], variableTypes[index])
	}
	g.cur.Goto(header)
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.continues = g.continues[:len(g.continues)-1]
	g.clearLabeledControl(label)
	g.cur = done
	for index, variable := range variables {
		g.vars[variable] = carriers[index]
		g.heapCaptures[variable] = carriers[index]
	}
	g.haltUnreachableForEnd(n, done)
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
				if _, ok := caseType.Underlying().(*types.Interface); ok {
					matches = g.interfaceTypeMatch(dynamicTag, caseType)
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
	if communicationCount == 1 && defaultClause != nil {
		g.selectSingleDefault(clauses, defaultClause, label)
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

func (g *gen) selectSingleDefault(clauses []*ast.CommClause, defaultClause *ast.CommClause, label string) {
	var communicationClause *ast.CommClause
	for _, clause := range clauses {
		if clause != defaultClause {
			communicationClause = clause
			break
		}
	}
	if communicationClause == nil {
		g.fail(defaultClause, "select default optimization missing communication case")
		return
	}

	selectedBlock := g.block("selectcase")
	defaultBlock := g.block("selectdefault")
	done := g.block("selectend")

	g.breaks = append(g.breaks, done)
	g.setLabeledControl(label, done, nil)
	continues := false

	switch operation := communicationClause.Comm.(type) {
	case *ast.SendStmt:
		channel := g.expr(operation.Chan)
		channelType := g.typeAndValue(operation.Chan).Type.Underlying().(*types.Chan)
		elementAddress := g.channelSendAddress(operation.Value, channelType.Elem())
		selected := g.cur.Call(ir.ClsW, g.fn.Sym("runtime.selectnbsend", 0), channel, elementAddress)
		g.cur.Jnz(selected, selectedBlock, defaultBlock)
	case *ast.ExprStmt:
		receive, ok := operation.X.(*ast.UnaryExpr)
		if !ok || receive.Op != token.ARROW {
			g.fail(operation, "invalid select receive case")
			return
		}
		channel, elementAddress, _ := g.prepareSelectReceive(receive)
		receivedSlot := g.alloc(types.Typ[types.Bool])
		selected := g.cur.Call(ir.ClsW, g.fn.Sym("runtime.selectnbrecv", 0), elementAddress, channel, receivedSlot)
		g.cur.Jnz(selected, selectedBlock, defaultBlock)
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
		channel, elementAddress, receiveType := g.prepareSelectReceive(receive)
		receivedSlot := g.alloc(types.Typ[types.Bool])
		selected := g.cur.Call(ir.ClsW, g.fn.Sym("runtime.selectnbrecv", 0), elementAddress, channel, receivedSlot)
		g.cur.Jnz(selected, selectedBlock, defaultBlock)

		g.cur = selectedBlock
		value := g.channelReceiveValue(elementAddress, receiveType)
		g.assignSelectValue(operation.Lhs[0], operation.Tok, value, receiveType)
		if len(operation.Lhs) == 2 {
			received := g.load(receivedSlot, types.Typ[types.Bool])
			g.assignSelectValue(operation.Lhs[1], operation.Tok, received, types.Typ[types.Bool])
		}
		g.stmts(communicationClause.Body)
		if g.live() {
			continues = true
			g.cur.Goto(done)
		}

		g.cur = defaultBlock
		g.stmts(defaultClause.Body)
		if g.live() {
			continues = true
			g.cur.Goto(done)
		}

		g.breaks = g.breaks[:len(g.breaks)-1]
		g.clearLabeledControl(label)
		g.cur = done
		if !continues {
			done.Hlt()
		}
		return
	default:
		g.fail(communicationClause.Comm, "unsupported select communication %T", communicationClause.Comm)
		return
	}

	g.cur = selectedBlock
	g.stmts(communicationClause.Body)
	if g.live() {
		continues = true
		g.cur.Goto(done)
	}

	g.cur = defaultBlock
	g.stmts(defaultClause.Body)
	if g.live() {
		continues = true
		g.cur.Goto(done)
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
	target := g.prepareAssignmentTarget(destination, assignment == token.DEFINE)
	g.storeAssignmentTarget(target, value, valueType)
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
		if tv.Value.Kind() == constant.Complex {
			return g.complexConstant(e, tv.Value, tv.Type)
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
				// The pointer may stay inside the frame and still have to name
				// a different object on every trip round a loop, which frame
				// storage cannot do. See addressOutlivesItsIteration.
				heap := !g.nonEscapingAddress(n) || g.addressOutlivesItsIteration(n)
				// Harvested here, at the decision, because everything between
				// this line and the recordPlacement calls below asks the walk
				// further questions about the literal's elements.
				why := g.escapeWhy(placementOf(heap))
				if g.noWriteBarrier {
					heap = false
					why = escapeExplanation{}
				}
				literalType := g.typeAndValue(literal).Type
				_, isMap := literalType.Underlying().(*types.Map)
				if isSliceType(literalType) || isMap {
					value := g.compositeLiteral(literal, heap, why)
					// The frame slot is emitted only in the arm that keeps it. A
					// slot allocated before the decision and then written over is
					// still in the finished IR with no uses, and nothing later
					// removes it: goc's pipeline has no DCE, and opt.Optimize runs
					// only under -O and gives up on any function with a secondary
					// entry.
					var storage ir.Ref
					if heap {
						storage = g.allocateEscapingTyped(literalType)
						g.recordPlacementWhy(storage, "composite-literal-descriptor", ir.AllocOnHeap, literalType, why)
					} else {
						storage = g.localAllocTyped(literalType)
						g.recordPlacement(storage, "composite-literal-descriptor", ir.AllocInFrame, literalType)
					}
					if isSliceType(literalType) {
						g.storeInlineValue(value, storage, literalType)
					} else {
						g.store(value, storage, literalType)
					}
					return storage
				}
				return g.compositeLiteral(literal, heap, why)
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
		return g.compositeLiteral(n, false, escapeExplanation{})
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
						return g.compositeLiteral(literal, false, escapeExplanation{})
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
				selection, obj = g.concreteMethodSelection(selection, obj)
				receiver = g.methodReceiver(fun, selection, obj)
				if methodHasInterfaceReceiver(obj) {
					g.interfaceMethods[obj] = true
				}
			}
		}
		var callee ir.Ref
		var sig *types.Signature
		var callSignature *types.Signature
		var closure ir.Ref
		stackVariadicCall := false
		if obj != nil {
			if obj.Pkg() != nil && obj.Pkg().Path() == "internal/abi" && obj.Name() == "EscapeNonString" && len(n.Args) == 1 {
				// EscapeNonString is an escape-analysis marker in the standard
				// compiler. It has no runtime operation; its source body panics so
				// compilers do not accidentally treat it as an ordinary function.
				return g.expr(n.Args[0])
			}
			if !g.runtimeAllocation && obj.Pkg() != nil && obj.Pkg().Path() == "maps" && obj.Name() == "clone" && len(n.Args) == 1 {
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
				// getg reads whichever register the target's Go ABI reserves for
				// the current goroutine. arm64 uses X28; amd64's is R14, settled
				// in AMD64_PARITY_PLAN B0 and recorded as regClosure's neighbour
				// regG in amd64/reg.go.
				//
				// amd64 is still refused here, and the gate outlives the decision
				// on purpose: knowing which register holds g is not the same as
				// being able to compile a function that uses it. The amd64 backend
				// does not yet reserve R14, lower ABIInternal arguments, or emit a
				// managed-frame prologue (Track B). Letting getg through before
				// then would trade this one accurate diagnostic for a scatter of
				// failures much further downstream.
				if g.target != TargetARM64 {
					g.fail(n, "runtime.getg intrinsic is unsupported on %s", g.target)
					return ir.R
				}
				const arm64GRegister = 28
				return g.cur.Load(ir.ClsP, g.fn.RegVar("g", arm64GRegister))
			}
			if obj.Pkg() != nil && obj.Pkg().Path() == "runtime" && obj.Name() == "KeepAlive" && len(n.Args) == 1 {
				g.keepAlive(obj, n.Args[0])
				return g.fn.Word(0)
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
			stackVariadicCall = runtimeStackVariadicSymbol(calleeName)
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
		previousStackVariadic := g.forceStackVariadic
		g.forceStackVariadic = previousStackVariadic || stackVariadicCall
		args = append(args, g.callArguments(n.Args, n.Ellipsis.IsValid(), sig)...)
		g.forceStackVariadic = previousStackVariadic
		args = g.adaptSharedGenericArguments(args, sig, callSignature, receiver != ir.R)
		if obj != nil && receiver == ir.R {
			g.at(n)
			if result, lowered := g.lowerCompilerIntrinsicCall(g.functionSymbol(obj), args); lowered {
				return result
			}
		}
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
		idx = g.indexOffset(idx, g.typeAndValue(n.Index).Type, typeSize(element))
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
			low = g.widenIndex(g.expr(n.Low), g.typeAndValue(n.Low).Type)
		}
		high := ir.R
		capacity := ir.R
		if n.High != nil {
			high = g.widenIndex(g.expr(n.High), g.typeAndValue(n.High).Type)
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
			maximum := g.widenIndex(g.expr(n.Max), g.typeAndValue(n.Max).Type)
			capacity = g.cur.Sub(ir.ClsL, maximum, low)
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
		_, targetIsInterface := targetType.Underlying().(*types.Interface)
		var matches ir.Ref
		if targetIsInterface {
			matches = g.interfaceTypeMatch(dynamicTag, targetType)
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
					if methodHasInterfaceReceiver(function) {
						g.interfaceMethods[function] = true
					}
					return g.methodExpressionValue(function, selection)
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
	signature, ok := g.typeAndValue(instantiation).Type.Underlying().(*types.Signature)
	if !ok {
		g.fail(instantiation, "generic function value has non-function type")
		return ir.R
	}
	switch expression := expression.(type) {
	case *ast.Ident:
		if function, ok := g.info.Uses[expression].(*types.Func); ok {
			if symbol, instantiated := g.instantiatedFunctionSymbol(function, expression); instantiated {
				adapter := g.goInternalFunctionAdapter(symbol, signature)
				return g.staticFunctionValue(adapter)
			}
			return g.functionValue(function)
		}
	case *ast.SelectorExpr:
		if function, ok := g.info.Uses[expression.Sel].(*types.Func); ok {
			if symbol, instantiated := g.instantiatedFunctionSymbol(function, expression); instantiated {
				adapter := g.goInternalFunctionAdapter(symbol, signature)
				return g.staticFunctionValue(adapter)
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

// typeParameterMethodSelection re-selects a method chosen on a value whose
// static type is a type parameter against the concrete type argument that the
// parameter is bound to in the enclosing instantiation.
//
// go/types reports the selected object for `p.M()`, where p has type parameter
// type P, as the method declared by P's constraint interface. That method has
// no body, and its receiver's underlying type is an interface, so without this
// resolution the call is lowered as dynamic interface dispatch against a
// receiver that is not an interface value at all. Inside an instantiated body
// the type argument is statically known, so the specification's meaning of the
// call is an ordinary direct call on the type argument's method.
//
// The result is a full selection rather than just the method because the
// concrete type may satisfy the constraint through an embedded field, in which
// case the receiver has to be advanced to that field before the call.
//
// substitutions binds the enclosing instantiation's type parameters. The second
// result is false when this is not a method selected through a type parameter,
// or when the parameter is not bound to a concrete type here -- a shared
// generic body keeps its existing lowering.
func typeParameterMethodSelection(
	selection *types.Selection,
	method *types.Func,
	substitutions map[*types.TypeParam]types.Type,
) (*types.Selection, bool) {
	if selection == nil || method == nil {
		return nil, false
	}
	if selection.Kind() != types.MethodVal {
		return nil, false
	}
	if _, isTypeParameter := types.Unalias(selection.Recv()).(*types.TypeParam); !isTypeParameter {
		return nil, false
	}
	concreteReceiver := substituteType(selection.Recv(), substitutions)
	if concreteReceiver == nil {
		return nil, false
	}
	if _, stillParameter := types.Unalias(concreteReceiver).(*types.TypeParam); stillParameter {
		return nil, false
	}
	concreteSelection := types.NewMethodSet(concreteReceiver).Lookup(method.Pkg(), method.Name())
	if concreteSelection == nil || concreteSelection.Kind() != types.MethodVal {
		return nil, false
	}
	if _, ok := concreteSelection.Obj().(*types.Func); !ok {
		return nil, false
	}
	return concreteSelection, true
}

// concreteMethodSelection applies typeParameterMethodSelection using the
// instantiation currently being generated. It returns the selection and method
// unchanged when the selection is not a constraint method call.
func (g *gen) concreteMethodSelection(
	selection *types.Selection,
	method *types.Func,
) (*types.Selection, *types.Func) {
	concreteSelection, resolved := typeParameterMethodSelection(selection, method, g.typeSubstitutions())
	if !resolved {
		return selection, method
	}
	return concreteSelection, concreteSelection.Obj().(*types.Func)
}

// methodReceiver evaluates the receiver for a method call. selection is the
// selection the call actually targets, which for a method reached through a
// type parameter is the one re-selected against the concrete type argument
// rather than the constraint's, so that an embedded receiver is advanced to the
// right field.
func (g *gen) methodReceiver(selector *ast.SelectorExpr, selection *types.Selection, method *types.Func) ir.Ref {
	var receiver ir.Ref
	if method != nil {
		signature := method.Type().(*types.Signature)
		if signature.Recv() != nil {
			_, wantsPointer := signature.Recv().Type().Underlying().(*types.Pointer)
			// Use the substituted type: in an instantiated body a receiver
			// expression typed as a type parameter already has the type
			// argument's representation, and the parameter's own underlying
			// type is its constraint interface, which describes nothing about
			// how the value is passed.
			receiverType := g.typeAndValue(selector.X).Type
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
	if selection == nil || len(selection.Index()) <= 1 {
		if method != nil {
			methodReceiverType := method.Type().(*types.Signature).Recv().Type()
			_, methodWantsPointer := methodReceiverType.Underlying().(*types.Pointer)
			expressionType := g.typeAndValue(selector.X).Type
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

func (g *gen) embeddedInterfaceMethodReceiver(descriptor ir.Ref, dynamicType types.Type, indexes []int, method *types.Func) ir.Ref {
	receiver := g.cur.Load(ir.ClsP, g.offset(descriptor, 8))
	return g.promotedInterfaceMethodReceiver(receiver, dynamicType, indexes, method)
}

func (g *gen) promotedInterfaceMethodReceiver(receiver ir.Ref, dynamicType types.Type, indexes []int, method *types.Func) ir.Ref {
	receiver = g.promotedInterfaceCallReceiver(receiver, dynamicType, indexes)
	methodReceiverType := method.Type().(*types.Signature).Recv().Type()
	if _, pointer := methodReceiverType.Underlying().(*types.Pointer); pointer {
		return receiver
	}
	if isInlineAggregate(methodReceiverType) || isInterfaceValue(methodReceiverType) {
		return receiver
	}
	return g.load(receiver, methodReceiverType)
}

func (g *gen) promotedInterfaceCallReceiver(receiver ir.Ref, dynamicType types.Type, indexes []int) ir.Ref {
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
	selection, method = g.concreteMethodSelection(selection, method)
	if methodHasInterfaceReceiver(method) {
		g.interfaceMethods[method] = true
	}
	signature := g.concreteType(selection.Type()).(*types.Signature)
	methodSignature := method.Type().(*types.Signature)
	receiverType := methodSignature.Recv().Type()
	methodSymbol := g.functionSymbol(method)
	if instantiatedSymbol, instantiated := g.instantiatedFunctionSymbol(method, expression); instantiated {
		methodSymbol = instantiatedSymbol
	}
	position := g.fset.Position(expression.Pos())
	wrapperName := methodValueWrapperName(g.pkg.Path(), methodSymbol, position, len(g.mod.Funcs))
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
	function.CallConv = ir.CallConvGoInternal
	wrapper := g.derive()
	wrapper.fn = function
	wrapper.cur = function.Entry()
	if signature.Results().Len() > 0 {
		resultType := signature.Results().At(0).Type()
		function.RetAgg = wrapper.goABIAggregate(resultType)
		function.RetValues = wrapper.runtimeAllocation && isSliceType(resultType)
	}
	context := wrapper.closureContext()
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
	callee := function.Sym(methodSymbol, 0)
	if signature.Results().Len() == 0 {
		wrapper.callVoidWithSignature(callee, arguments, methodSignature, receiverType)
		wrapper.cur.RetVoid()
	} else {
		result := wrapper.callWithSignature(resultClass, callee, arguments, methodSignature, receiverType)
		wrapper.returnValue(result, signature.Results().At(0).Type())
	}
	// See the composite-literal descriptor above: the slot is emitted in the arm
	// that keeps it, because a slot written over after the decision stays in the
	// IR with no uses.
	var descriptor ir.Ref
	if !g.valueDoesNotEscape(expression) {
		descriptor = g.allocateEscapingTyped(descriptorType)
		g.recordPlacement(descriptor, "method-value-descriptor", ir.AllocOnHeap, descriptorType)
	} else {
		descriptor = g.localAllocTyped(descriptorType)
		g.recordPlacement(descriptor, "method-value-descriptor", ir.AllocInFrame, descriptorType)
	}
	g.cur.Store(g.fn.Sym(wrapperName, 0), g.offset(descriptor, descriptorOffsets[0]))
	receiverValue := g.methodReceiver(expression, selection, method)
	receiverStorage := g.offset(descriptor, descriptorOffsets[1])
	if isInlineAggregate(receiverType) || isInterfaceValue(receiverType) {
		g.storeInlineValue(receiverValue, receiverStorage, receiverType)
	} else {
		g.store(receiverValue, receiverStorage, receiverType)
	}
	return descriptor
}

func methodValueWrapperName(packagePath, methodSymbol string, position token.Position, sequence int) string {
	return fmt.Sprintf(
		"%s.methodvalue.%s.%d.%d.%d",
		packagePath,
		methodSymbol,
		position.Line,
		position.Column,
		sequence,
	)
}

// compositeLiteral emits a composite literal into heap or frame storage. heap is
// the caller's escape decision, and why is that decision's explanation -- carried
// rather than re-asked, because between the decision and the placement recorded
// below the emitter asks other escape questions about the literal's elements.
func (g *gen) compositeLiteral(literal *ast.CompositeLit, heap bool, why escapeExplanation) ir.Ref {
	t := g.typeAndValue(literal).Type
	if pointer, isPointer := t.Underlying().(*types.Pointer); isPointer {
		structure, isStructure := pointer.Elem().Underlying().(*types.Struct)
		if !isStructure {
			g.fail(literal, "unsupported pointer composite literal type %s", t)
			return ir.R
		}
		var backing ir.Ref
		if heap {
			backing = g.allocateEscapingTyped(pointer.Elem())
			g.recordPlacementWhy(backing, "composite-literal", ir.AllocOnHeap, pointer.Elem(), why)
		} else {
			// The neutral candidate form: opt.LowerHeapAllocations decides this
			// one, so there is no front-end placement to record.
			backing = g.allocateTyped(pointer.Elem())
		}
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
		backingType := types.NewArray(elementType, length)
		// The frame slot, and the stack pointer words that describe it to the
		// collector, belong to the arm that keeps the slot. Allocating first and
		// overwriting on escape left an OAlloc with no uses in the finished IR
		// and an ir.Func.StackPointerWords entry for a temporary no instruction
		// defines any more.
		var backing ir.Ref
		local := !heap && g.valueDoesNotEscape(literal)
		sliceWhy := why
		if !heap {
			sliceWhy = g.escapeWhy(placementOf(!local))
		}
		if !local {
			backing = g.allocateEscapingTyped(backingType)
			g.recordPlacementWhy(backing, "slice-literal-backing", ir.AllocOnHeap, backingType, sliceWhy)
		} else {
			backing = g.localAlloc(alignment, int(backingSize))
			visitPointerWords(backingType, 0, func(offset int64) {
				g.markStackPointerWord(backing, int(offset))
			})
			g.recordPlacement(backing, "slice-literal-backing", ir.AllocInFrame, backingType)
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
	// As above: the slot and its stack pointer words are emitted by the arm that
	// keeps them, so an escaping literal leaves no unused OAlloc behind.
	var backing ir.Ref
	if heap {
		backing = g.allocateEscapingTyped(t)
		g.recordPlacementWhy(backing, "composite-literal", ir.AllocOnHeap, t, why)
	} else {
		backing = g.localAlloc(align, int(size))
		visitPointerWords(t, 0, func(offset int64) {
			g.markStackPointerWord(backing, int(offset))
		})
		g.recordPlacement(backing, "composite-literal", ir.AllocInFrame, t)
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

// functionLiteralIsDeferred reports whether the literal is the callee of a
// `defer func(){ ... }()` -- a directly-deferred closure, which runs within the
// enclosing frame during deferreturn.
func (g *gen) functionLiteralIsDeferred(literal *ast.FuncLit) bool {
	call, ok := g.parents[literal].(*ast.CallExpr)
	if !ok || call.Fun != literal {
		return false
	}
	_, deferred := g.parents[call].(*ast.DeferStmt)
	return deferred
}

// isResultParamSlot reports whether slot is one of the current function's extra
// result slots -- the pointer parameters (result1, result2, ...) through which a
// named result after the first is returned into the caller's storage. Such a
// slot must be captured into a closure by reference, not copied.
func (g *gen) isResultParamSlot(slot ir.Ref) bool {
	for _, s := range g.extraResultSlots {
		if s == slot {
			return true
		}
	}
	return false
}

// enclosingFunctionName is the symbol a function literal's own symbol is built
// on: the name of the declared function the literal is written inside.
//
// A literal used to be named after its *package* whenever the generator had no
// explicit name for the enclosing function -- which is the ordinary case, since
// functionName is set only for a generic instantiation or a package
// initializer. Position alone does not identify a literal within a package: two
// files generated from one template have their literals at the same line and
// column, and `crypto/internal/fips140/nistec`'s p224.go, p384.go and p521.go
// are exactly that. Three different closures came out of the compiler under the
// name `crypto/internal/fips140/nistec.func.114.16`, and obj's symbol index is
// keyed by name, so every reference to any of them resolved to whichever was
// emitted last: p224B would have run p521B's initializer. It was invisible only
// because those symbols were local, so the system linker never had to choose --
// exporting them for a prebuilt runtime pack turns it into a duplicate-symbol
// error, which is how it was found.
//
// Naming a literal after the function it is written in is what Go itself does
// (`pkg.Func.func1`) and it makes the symbol unique: a package cannot declare
// two functions with the same symbol, and a function cannot hold two literals at
// one position.
func (g *gen) enclosingFunctionName() string {
	if g.functionName != "" {
		return g.functionName
	}
	if g.currentFunction != nil {
		return g.functionSymbol(g.currentFunction)
	}
	return g.pkg.Path()
}

func (g *gen) functionLiteral(literal *ast.FuncLit) ir.Ref {
	originalSignature := g.typeAndValue(literal.Type).Type.(*types.Signature)
	signature := g.concreteType(originalSignature).(*types.Signature)
	escaping := g.functionLiteralEscapes(literal)
	// A deferred closure escapes (it is stored in the defer record) yet is
	// frame-scoped: it runs during deferreturn, before the frame is torn down.
	// That lets it capture a result slot by reference (see the capture loop); a
	// goroutine or a returned closure must not.
	frameScopedDefer := g.functionLiteralIsDeferred(literal)
	parameterObjects := g.fieldListObjects(literal.Type.Params)
	resultObjects := g.fieldListObjects(literal.Type.Results)
	position := g.fset.Position(literal.Pos())
	symbol := fmt.Sprintf("%s.func.%d.%d", g.enclosingFunctionName(), position.Line, position.Column)
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

	// The literal's body lives in the enclosing function's package and inherits
	// its generic instantiation, its name prefix and its write barrier setting.
	child := g.derive()
	child.info = g.info
	child.pkg = g.pkg
	child.typeArguments = g.typeArguments
	child.functionName = g.functionName
	child.currentFunction = g.currentFunction
	child.noWriteBarrier = g.noWriteBarrier
	child.resultObjects = resultObjectSet(signature)
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
	child.fn.CallConv = ir.CallConvGoInternal
	if g.functionLiteralRunsOnSystemStack(literal) {
		child.fn.NoSplit = true
		child.fn.SystemStack = true
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
	child.escapeWalkOuterObjects = predeclaredVariables
	child.escapingCaptures = child.findEscapingCaptures(literal.Body, predeclaredVariables...)
	child.iterationCaptures = child.findIterationCaptures(literal.Body)
	child.referenceCaptures = child.findReferenceCaptures(literal.Body, predeclaredVariables...)
	keepAliveObjects, orderedKeepAliveObjects := child.findKeepAliveObjects(literal.Body)
	child.keepAliveObjects = keepAliveObjects
	child.keepAliveValues = make(map[types.Object]ir.Ref)
	child.keepAliveSlots = make(map[types.Object]ir.Ref)
	child.declareKeepAliveSlots(orderedKeepAliveObjects)
	child.transientInterfaceDescriptors = make(map[uint32]bool)
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
		child.trackKeepAliveAssignment(originalParameter, value, parameter.Type())
		if i < len(parameterObjects) && parameterObjects[i] != nil {
			child.vars[parameterObjects[i]] = slot
			child.trackKeepAliveAssignment(parameterObjects[i], value, parameter.Type())
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
		captureAddress := child.cur.Load(ir.ClsP, child.offset(environment, int64(8*(i+1))))
		child.fn.MarkGCRef(captureAddress)
		child.vars[capture] = captureAddress
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
		if result.Name() != "" {
			child.resultSlot = child.resultStorage(originalResult, result.Type())
			if len(resultObjects) > 0 && resultObjects[0] != nil {
				child.vars[resultObjects[0]] = child.resultSlot
			}
		} else {
			child.resultSlot = child.resultStorage(nil, result.Type())
		}
	}
	child.stmts(literal.Body.List)
	if child.err != nil {
		g.err = child.err
		return ir.R
	}
	if !child.live() && child.runtimeAllocation && len(child.deferActions) != 0 {
		deferReturn := child.block("deferreturn")
		deferReturn.SecondaryEntry = true
		child.addDeferRecoveryEdges(deferReturn)
		child.cur = deferReturn
		child.runDefers()
		if signature.Results().Len() == 0 {
			child.cur.RetVoid()
		} else if child.resultSlot != ir.R {
			value := child.resultSlot
			if !(isInlineAggregate(child.resultType) || isInterfaceValue(child.resultType)) || (child.runtimeAllocation && isSliceType(child.resultType)) {
				value = child.load(child.resultSlot, child.resultType)
			}
			child.returnValue(value, child.resultType)
		} else {
			child.returnValue(child.zeroValue(child.resultType), child.resultType)
		}
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
			if frameScopedDefer && g.isResultParamSlot(originalSlot) {
				// A named result after the first is returned through the caller's
				// result slot, which originalSlot already addresses. A deferred
				// closure runs within the frame, so capture that slot by reference:
				// its writes then land in the same location the return stores to and
				// the caller reads. Copying the value into a fresh heap cell (as the
				// branches below do) would strand the closure's updates -- the return
				// would still write *result and the caller would read the un-updated
				// slot. (A non-deferred escaping closure keeps the cell: it observes
				// a snapshot and must not write the caller's slot after return.)
				g.heapCaptures[capture] = originalSlot
				g.store(originalSlot, g.offset(descriptor, int64(8*(i+1))), fields[i+1].Type())
				continue
			}
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
			} else if g.directValues[capture] && isInlineValue(captureType) {
				// The variable's storage already is its value, so the snapshot is
				// a copy of those bytes. Loading through the slot first, as the
				// aggregate arm below does, would read the value's first word as
				// the address of the value.
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
	saved := g.diagQuestion()
	escapes := g.functionLiteralEscapesWithin(
		literal,
		g.info,
		g.parents,
		g.currentBody,
		make(map[parameterKey]bool),
	)
	g.diagResolve(saved, escapes, nil)
	return escapes
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
			if deferStatement, deferred := parents[call].(*ast.DeferStmt); deferred && g.runtimeAllocation {
				return deferStatementRepeats(deferStatement, parents, body)
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
			if call, isCall := parents[use].(*ast.CallExpr); isCall && call.Fun == use {
				return true
			}
			// Anything other than calling the closure gets the same question
			// asked about every other value. Treating every such use as an
			// escape made a closure stored in a frame-local struct --
			// runtime.hexdumpWords' h := hexdumper{mark: symMark} -- lift its
			// captures, which reached copystack and scanstack.
			if !g.nonEscapingObjectUse(use, info, parents, body, checking) {
				escapes = true
			}
			return true
		})
		return escapes
	}
	return true
}

// deferredFunctionValueStaysInFrame reports that the function value a call is
// made through is the function value of a *directly deferred* call, and can
// therefore live in the registering frame.
//
// It is functionLiteralEscapesWithin's rule for `defer func(){...}()`, asked of
// the other two shapes the same statement can take. `defer mu.Unlock()` builds a
// method value -- a descriptor holding the receiver -- and `defer f(x)` builds a
// deferwrap closure holding the arguments; both are handed to
// runtime.deferproc, which stores them in a _defer record that deferreturn pops
// before this frame is torn down. Neither outlives the frame, so neither has to
// be in the heap, and putting the method value there is worse than a wasted
// allocation: the descriptor holds the *address* of a frame-local receiver, so a
// heap descriptor is a frame address published into a heap object. See the
// deferred-receiver case in escape_publication_test.go.
//
// The condition is the literal's: a defer that can be registered more than once
// needs a fresh descriptor per registration, because one frame slot cannot hold
// two.
func (g *gen) deferredFunctionValueStaysInFrame(call *ast.CallExpr, parents map[ast.Node]ast.Node, body *ast.BlockStmt) bool {
	deferStatement, deferred := parents[call].(*ast.DeferStmt)
	if !deferred {
		return false
	}
	if !g.runtimeAllocation {
		// Without a runtime there is no _defer record: runDefers replays the
		// statement inline at every exit out of one frame slot per defer
		// statement, so the descriptor is frame-scoped however often the
		// statement is reached.
		return true
	}
	return !deferStatementRepeats(deferStatement, parents, body)
}

// deferStatementRepeats reports whether one execution of the enclosing frame can
// reach the same defer statement more than once.
//
// A directly-deferred function literal runs during deferreturn, inside the frame
// that registered it, so it does not outlive that frame: its closure descriptor
// fits in a frame slot and it can capture the enclosing variables by reference.
// That reasoning only holds while the registration happens at most once. A defer
// that runs again reuses the single frame slot its descriptor was assigned, so
// every _defer record would point at the same descriptor and every deferred call
// would observe the last registration's captures. A repeating defer therefore
// still needs a fresh heap descriptor and heap-lifted captures per registration.
//
// A defer repeats when it sits inside a loop, or when a goto can jump back to a
// label declared at or before it. Labels declared after the defer cannot re-reach
// it, which is what keeps runtime.gcAssistAlloc's synctest defer -- it precedes
// that function's own retry label -- on the non-repeating path.
func deferStatementRepeats(deferStatement *ast.DeferStmt, parents map[ast.Node]ast.Node, body *ast.BlockStmt) bool {
	for node := ast.Node(deferStatement); node != nil; node = parents[node] {
		switch parents[node].(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return true
		case *ast.FuncLit:
			// The defer belongs to the nested literal's frame, which is entered
			// afresh on every call. Whatever encloses that literal here cannot
			// register this defer twice within one of those frames.
			return false
		}
	}
	return bodyJumpsBackTo(body, deferStatement.Pos())
}

// bodyJumpsBackTo reports whether the function body contains a goto whose target
// label is declared at or before position, so control can return to that point.
// Labels and gotos inside a nested function literal belong to a different frame
// and Go forbids jumping between the two, so those are not considered.
func bodyJumpsBackTo(body *ast.BlockStmt, position token.Pos) bool {
	if body == nil {
		return false
	}
	targetedLabels := make(map[string]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		branch, ok := node.(*ast.BranchStmt)
		if ok && branch.Tok == token.GOTO && branch.Label != nil {
			targetedLabels[branch.Label.Name] = true
		}
		return true
	})
	if len(targetedLabels) == 0 {
		return false
	}

	jumpsBack := false
	ast.Inspect(body, func(node ast.Node) bool {
		if jumpsBack {
			return false
		}
		if _, nestedFunction := node.(*ast.FuncLit); nestedFunction {
			return false
		}
		labeled, ok := node.(*ast.LabeledStmt)
		if ok && targetedLabels[labeled.Label.Name] && labeled.Pos() <= position {
			jumpsBack = true
		}
		return true
	})
	return jumpsBack
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

func resultObjectSet(signature *types.Signature) map[types.Object]bool {
	results := make(map[types.Object]bool, signature.Results().Len())
	for index := 0; index < signature.Results().Len(); index++ {
		result := signature.Results().At(index)
		if result != nil && result.Name() != "" {
			results[result] = true
		}
	}
	return results
}

// findReferenceCaptures returns the local variables of body that a nested
// function body refers to. A nested body is a function literal, or the body of
// a `range` over a function, which is lowered into a yield function; either one
// runs in a frame of its own and reaches the variable through the closure
// environment, which carries the address of the variable's slot.
//
// findEscapingCaptures answers the narrower question of which of those
// variables must be heap-lifted, because the nested function can outlive the
// frame. A non-escaping literal needs no heap cell, but it still assigns
// through a slot that belongs to the enclosing frame, and that is what makes
// this set matter to variableStorage.
func (g *gen) findReferenceCaptures(body *ast.BlockStmt, predeclared ...types.Object) map[types.Object]bool {
	locals := g.bodyLocals(body, predeclared...)
	captures := make(map[types.Object]bool)
	recordReferences := func(nested ast.Node) {
		if nested == nil {
			return
		}
		ast.Inspect(nested, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			if object := g.info.Uses[identifier]; locals[object] {
				captures[object] = true
			}
			return true
		})
	}
	ast.Inspect(body, func(node ast.Node) bool {
		if literal, ok := node.(*ast.FuncLit); ok {
			recordReferences(literal.Body)
			return false
		}
		if statement, ok := node.(*ast.RangeStmt); ok && g.rangesOverFunction(statement) {
			// The clause's own destinations are assigned from inside the yield
			// function too, so `for k, dst[i] = range seq` shares k, dst and i.
			recordReferences(statement.Body)
			recordReferences(statement.Key)
			recordReferences(statement.Value)
		}
		return true
	})
	return captures
}

// rangesOverFunction reports whether this `range` clause iterates a function,
// which is the form lowered into a separate yield function.
func (g *gen) rangesOverFunction(statement *ast.RangeStmt) bool {
	if statement.X == nil {
		return false
	}
	rangeType := g.info.Types[statement.X].Type
	if rangeType == nil {
		return false
	}
	_, ok := rangeType.Underlying().(*types.Signature)
	return ok
}

// bodyLocals returns the variables body declares itself, together with the
// predeclared ones the caller names, and stops at a nested function literal so
// that a literal's own locals are not mistaken for this body's.
func (g *gen) bodyLocals(body *ast.BlockStmt, predeclared ...types.Object) map[types.Object]bool {
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
	return locals
}

// findIterationCaptures returns the locals declared inside a loop body whose
// storage has to be a different object in every iteration.
//
// # The question the rest of the escape walk does not ask
//
// findEscapingCaptures asks whether a local's storage outlives the *frame*. An
// allocation can be entirely frame-local by that test and still need one object
// per iteration: Go gives a variable declared in a loop body one instance per
// trip round the loop, so
//
//	var p, q *[2]int
//	for i := 0; i < n; i++ {
//		var a [2]int
//		a[0] = i
//		p, q = q, &a
//	}
//	return p[0], q[0]      // Go: 1 2
//
// has two live arrays at the end, not one. Giving `a` a single frame slot makes
// `p` and `q` the same pointer and the function returns `2 2`. Nothing escapes
// the frame, so neither escape analysis objects and opt.FrameEscapes -- a
// may-analysis over publications -- has nothing to report either. It is an
// aliasing defect, and the missing fact is the one gc's escape analysis calls
// loop depth.
//
// # How it is asked here
//
// It is the same walk with a smaller scope. objectDoesNotEscape refuses to
// answer for an object whose uses it cannot all see (escapeWalkSeesEveryUse),
// and with the loop body as the scope that is exactly the objects declared
// inside the loop. So running the existing walk against the loop body asks
// "does this address reach anything that is not itself per-iteration", which is
// the question, and it inherits the walk's interprocedural parameter summaries
// rather than restating them.
//
// The scope has to be tightened at both ends: escapeWalkOuterObjects lists the
// function's parameters and results, which the walk normally trusts because it
// sees the whole body. Under a loop-body scope they are storage that outlives
// the iteration and their uses are mostly not in view, so nothing may be
// trusted but what the loop declares.
//
// # Why this is not "heap everything in a loop"
//
// A scratch buffer in a loop body -- `var b [64]byte; n, _ := r.Read(b[:])` --
// reaches only the loop's own locals and a parameter the callee does not let
// escape, so it stays in its frame slot, which is also where Go puts it. Only
// an address that reaches storage declared further out is moved, which is the
// same rule gc applies when it compares loop depths.
//
// Nested loops need no special handling. Widening the scope can only make the
// walk trust more objects and so report fewer captures, so for a local declared
// in an inner loop the innermost scope gives the strongest answer and the union
// over all the loops in the function is exactly that answer.
func (g *gen) findIterationCaptures(body *ast.BlockStmt) map[types.Object]bool {
	loops := loopBodiesWithin(body)
	if len(loops) == 0 {
		return nil
	}

	savedBody := g.currentBody
	savedOuter := g.escapeWalkOuterObjects
	defer func() {
		g.currentBody = savedBody
		g.escapeWalkOuterObjects = savedOuter
	}()

	var captures map[types.Object]bool
	for _, loop := range loops {
		g.currentBody = loop
		g.escapeWalkOuterObjects = nil
		for object := range g.findEscapingCaptures(loop) {
			if captures == nil {
				captures = make(map[types.Object]bool)
			}
			captures[object] = true
		}
	}
	return captures
}

// loopBodiesWithin lists the bodies of the `for` and `range` statements a
// function body runs itself, outermost first. A loop inside a function literal
// belongs to that literal's own frame and is found when the literal is lowered.
func loopBodiesWithin(body *ast.BlockStmt) []*ast.BlockStmt {
	var loops []*ast.BlockStmt
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ForStmt:
			loops = append(loops, node.Body)
		case *ast.RangeStmt:
			loops = append(loops, node.Body)
		}
		return true
	})
	return loops
}

// addressOutlivesItsIteration reports whether an address taken inside a loop
// body reaches storage that is still there on the next trip round the loop.
//
// It is findIterationCaptures's question asked about one address rather than
// about a variable, for the allocations that have no variable to ask about: the
// storage behind `&T{...}` is committed to the frame by the emitter, on
// nonEscapingAddress's word that the pointer does not outlive the function, and
// `c := &cell{v: i}` in a loop body is the same aliasing defect with the
// allocation named by nothing.
func (g *gen) addressOutlivesItsIteration(address *ast.UnaryExpr) bool {
	loop := g.enclosingLoopBody(address)
	if loop == nil {
		return false
	}

	savedBody := g.currentBody
	savedOuter := g.escapeWalkOuterObjects
	g.currentBody = loop
	g.escapeWalkOuterObjects = nil
	defer func() {
		g.currentBody = savedBody
		g.escapeWalkOuterObjects = savedOuter
	}()

	saved := g.diagQuestion()
	outlives := g.addressEscapesWithin(address, g.info, g.parents, loop, make(map[parameterKey]bool))
	if outlives {
		// The rule the loop imposes is not the one the climb found: the climb
		// answered about the enclosing loop body rather than about the function,
		// and what it means is that the storage is still reachable on the next
		// trip round. Named here rather than left to whichever branch of the
		// climb returned, which would report a publication that does not on its
		// own force the allocation out of the frame.
		g.diagRuleOverride(func() string {
			return "its address is still reachable on the next iteration of the enclosing loop"
		})
	}
	g.diagResolve(saved, outlives, nil)
	return outlives
}

// enclosingLoopBody returns the body of the innermost loop a node sits in, or
// nil if it is not in one.
//
// Only the body counts. A `for` clause's init statement runs once, and its
// condition and post statement are evaluated between iterations rather than
// inside one, so the per-iteration question does not arise for storage
// committed there. The climb stops at a function literal because that literal's
// body is a different frame, allocated afresh on every call.
func (g *gen) enclosingLoopBody(node ast.Node) *ast.BlockStmt {
	current := node
	for {
		parent := g.parents[current]
		if parent == nil {
			return nil
		}
		switch parent := parent.(type) {
		case *ast.FuncLit:
			return nil
		case *ast.ForStmt:
			if parent.Body == current {
				return parent.Body
			}
		case *ast.RangeStmt:
			if parent.Body == current {
				return parent.Body
			}
		}
		current = parent
	}
}

func (g *gen) findEscapingCaptures(body *ast.BlockStmt, predeclared ...types.Object) map[types.Object]bool {
	locals := g.bodyLocals(body, predeclared...)

	captures := make(map[types.Object]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if isCall {
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok {
				selection := g.info.Selections[selector]
				method, methodCall := g.info.Uses[selector.Sel].(*types.Func)
				if selection != nil && selection.Kind() == types.MethodVal && methodCall {
					signature := method.Type().(*types.Signature)
					_, wantsPointer := signature.Recv().Type().Underlying().(*types.Pointer)
					_, hasPointer := g.typeAndValue(selector.X).Type.Underlying().(*types.Pointer)
					if wantsPointer && !hasPointer && !g.receiverDoesNotEscape(method, make(map[parameterKey]bool)) {
						identifier, found := addressedVariableIdentifier(selector.X, g.info)
						if found {
							object := g.info.Uses[identifier]
							if object == nil {
								object = g.info.Defs[identifier]
							}
							if locals[object] {
								captures[object] = true
							}
						}
					}
				}
			}
		}

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
			identifier, ok := addressedVariableIdentifier(address.X, g.info)
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
	symbol := g.functionSymbol(function)
	signature := compiledFunctionSignature(function)
	adapter := g.goInternalFunctionAdapter(symbol, signature)
	return g.staticFunctionValue(adapter)
}

func (g *gen) goInternalFunctionAdapter(symbol string, signature *types.Signature) string {
	return g.goInternalCallAdapter(symbol, signature, signature, nil)
}

func (g *gen) methodExpressionValue(function *types.Func, selection *types.Selection) ir.Ref {
	symbol := g.functionSymbol(function)
	entrySignature := g.concreteType(selection.Type()).(*types.Signature)
	calleeSignature := g.concreteType(function.Type()).(*types.Signature)
	receiverType := calleeSignature.Recv().Type()
	adapter := g.goInternalCallAdapter(symbol, entrySignature, calleeSignature, receiverType)
	return g.staticFunctionValue(adapter)
}

func (g *gen) goInternalCallAdapter(
	symbol string,
	entrySignature *types.Signature,
	calleeSignature *types.Signature,
	receiverType types.Type,
) string {
	adapterKey := symbol + "|" + types.TypeString(entrySignature, nil) +
		"|" + types.TypeString(calleeSignature, nil)
	if receiverType != nil {
		adapterKey += "|" + types.TypeString(receiverType, nil)
	}
	adapterName, fresh := g.internSymbol(symbol+".gointernal.funcvalue", adapterKey)
	if !fresh {
		return adapterName
	}
	resultClass := ir.ClsW
	if entrySignature.Results().Len() > 0 {
		resultClass, _ = scalar(entrySignature.Results().At(0).Type())
	}

	var function *ir.Func
	if entrySignature.Results().Len() == 0 {
		function = g.mod.NewFuncVoid(adapterName)
	} else {
		function = g.mod.NewFunc(adapterName, resultClass)
	}
	function.CallConv = ir.CallConvGoInternal
	function.ManagedFrame = g.runtimeAllocation

	adapter := g.derive()
	adapter.fn = function
	adapter.cur = function.Entry()
	if entrySignature.Results().Len() > 0 {
		resultType := entrySignature.Results().At(0).Type()
		function.RetAgg = adapter.goABIAggregate(resultType)
		function.RetValues = adapter.runtimeAllocation && isSliceType(resultType)
	}

	arguments := make([]ir.Ref, 0, entrySignature.Params().Len()+entrySignature.Results().Len())
	for index := 0; index < entrySignature.Params().Len(); index++ {
		parameter := entrySignature.Params().At(index)
		parameterClass, _ := scalar(parameter.Type())
		arguments = append(arguments, adapter.functionParameter(parameter.Name(), parameter.Type(), parameterClass))
	}
	if entrySignature.Results().Len() > 0 && isInlineAggregate(entrySignature.Results().At(0).Type()) && function.RetAgg == nil {
		arguments = append(arguments, function.ParamRef("result0"))
	}
	for index := 1; index < entrySignature.Results().Len(); index++ {
		arguments = append(arguments, function.ParamRef(fmt.Sprintf("result%d", index)))
	}

	callee := function.Sym(symbol, 0)
	if entrySignature.Results().Len() == 0 {
		adapter.callVoidWithSignature(callee, arguments, calleeSignature, receiverType)
		adapter.cur.RetVoid()
		return adapterName
	}

	result := adapter.callWithSignature(resultClass, callee, arguments, calleeSignature, receiverType)
	adapter.returnValue(result, entrySignature.Results().At(0).Type())
	return adapterName
}

func (g *gen) staticFunctionValue(symbol string) ir.Ref {
	return g.fn.Sym(g.staticFunctionDescriptor(symbol), 0)
}

func (g *gen) staticNamedFunctionDescriptor(function *types.Func) string {
	symbol := g.functionSymbol(function)
	signature := compiledFunctionSignature(function)
	adapter := g.goInternalFunctionAdapter(symbol, signature)
	return g.staticFunctionDescriptor(adapter)
}

func (g *gen) staticFunctionDescriptor(symbol string) string {
	// Named from the symbol it describes rather than from a count of the data
	// emitted so far. A counter encodes the order code was generated in, and
	// that order came from map traversal, so the same source produced a
	// different binary on every build. Deriving the name from the content also
	// means one descriptor per function instead of one per reference.
	if name := g.functionDescriptors[symbol]; name != "" {
		return name
	}
	name := ".goc.funcval." + symbol
	g.functionDescriptors[symbol] = name
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubL, Sym: symbol}},
	})
	return name
}

func (g *gen) closureRegister() int {
	if g.target == TargetARM64 {
		// Go's ARM64 ABIInternal reserves X26 for the closure context.
		return 26
	}
	// RDX on amd64, which is what Go's ABIInternal uses -- "set RDX to point to
	// the closure (if a closure call)", stdlib/src/runtime/asm_amd64.s:2003.
	//
	// This is now the settled choice rather than a placeholder (AMD64_PARITY_PLAN
	// B0, recorded as regClosure in amd64/reg.go). RDX is also the high half of
	// amd64's div/rem and widening multiply, and mc_va.go's vararg scratch; the
	// collision is resolved by the backend copying the incoming context to an
	// allocatable register at entry, before any of those uses can run, which is
	// the same stabilization arm64 already performs for X26. The arm still goes
	// unexercised until B1 lowers ABIInternal, because getg refuses amd64 first.
	return 2
}

func (g *gen) closureContext() ir.Ref {
	g.fn.HasClosureContext = true
	context := g.fn.NewTemp("closure", ir.ClsP)
	temporary := g.fn.Temp(context)
	temporary.GCRef = true
	temporary.Fixed = true
	temporary.Reg = g.closureRegister()
	temporary.ClosureContext = true
	return context
}

func (g *gen) pinClosure(closure ir.Ref) {
	context := g.cur.Copy(ir.ClsP, closure)
	temporary := g.fn.Temp(context)
	temporary.GCRef = true
	temporary.Fixed = true
	temporary.Reg = g.closureRegister()
	g.nextCallUsesClosure = true
	g.nextCallClosure = context
}

func (g *gen) consumeClosureCall() (bool, ir.Ref) {
	closureCall := g.nextCallUsesClosure
	closure := g.nextCallClosure
	g.nextCallUsesClosure = false
	g.nextCallClosure = ir.R
	return closureCall, closure
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
			name := g.literalDataSymbol(".goc.bytes", 1, values)
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
				g.recordPlacement(buffer, "string-conversion-buffer", ir.AllocInFrame, nil)
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
	name := g.literalDataSymbol(".goc.string", 1, values)
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

func (g *gen) indexOffset(index ir.Ref, indexType types.Type, elementSize int64) ir.Ref {
	index = g.widenIndex(index, indexType)
	if elementSize != 1 {
		index = g.cur.Mul(ir.ClsL, index, g.fn.Long(elementSize))
	}
	return index
}

func (g *gen) widenIndex(index ir.Ref, indexType types.Type) ir.Ref {
	switch g.fn.ClassOf(index) {
	case ir.ClsW:
		if signed(indexType) {
			index = g.cur.Extsw(ir.ClsL, index)
		} else {
			index = g.cur.Extuw(ir.ClsL, index)
		}
	case ir.ClsL:
		// Already pointer-width.
	default:
		index = g.cur.Copy(ir.ClsL, index)
	}
	return index
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

func (g *gen) allocateEscapingTyped(valueType types.Type) ir.Ref {
	if g.runtimeAllocation {
		allocation := g.cur.Call(
			ir.ClsP,
			g.fn.Sym("runtime.newobject", 0),
			g.runtimeType(valueType),
		)
		g.recordPlacement(allocation, "escaping-typed", ir.AllocOnHeap, valueType)
		return g.fn.MarkGCRef(allocation)
	}
	allocation := g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(typeSize(valueType)))
	g.recordPlacement(allocation, "escaping-typed", ir.AllocOnHeap, valueType)
	return allocation
}

// recordPlacement notes that the front end placed one allocation itself rather
// than emitting the neutral OHeapAlloc candidate form and letting
// opt.LowerHeapAllocations decide.
//
// These are the allocations the IR pass never gets a say in, and they are the
// ones the escape rearchitecture has to move. Recording them is what makes the
// question "would the IR pass have agreed" askable at all: a committed frame
// placement is an OAlloc indistinguishable from a local variable's slot, and a
// committed heap placement is an allocator call indistinguishable from a
// lowered candidate. The record is diagnostic only -- see ir.PlacedAlloc -- and
// nothing in the compiler reads it.
//
// valueType is the type of the object placed, or nil at a site that has none.
// It is what lets a frame record name the same census site as the heap record
// the same site produces when the decision goes the other way, which is what
// makes "this object moved from a frame to the heap" expressible rather than
// arriving as one site vanishing and an unrelated one appearing. Recording it
// does not emit the descriptor: the symbol name is a pure function of the type.
func (g *gen) recordPlacement(storage ir.Ref, site string, placement ir.AllocPlacement, valueType types.Type) {
	g.recordPlacementWhy(storage, site, placement, valueType, g.escapeWhy(placement))
}

// recordPlacementWhy is recordPlacement for a site whose decision was taken
// further back than the line above it, so that the explanation belongs to the
// question that placed this object and not to whichever one the emitter asked
// most recently on the way here. why comes from gen.escapeWhy, called at the
// decision. It is the zero value when the escape diagnostic is off.
func (g *gen) recordPlacementWhy(storage ir.Ref, site string, placement ir.AllocPlacement, valueType types.Type, why escapeExplanation) {
	if storage.Kind != ir.RefTemp || g.fn == nil {
		return
	}
	if g.fn.PlacedAllocs == nil {
		g.fn.PlacedAllocs = make(map[uint32]ir.PlacedAlloc)
	}
	rule, use, chain := g.placedFor(why)
	g.fn.PlacedAllocs[storage.ID] = ir.PlacedAlloc{
		Site:      site,
		Placement: placement,
		Allocator: g.placementAllocator(valueType),
		Type:      g.placementTypeSymbol(valueType),
		Rule:      rule,
		Use:       use,
		Chain:     chain,
	}
}

// placementOf names the placement a heap/frame decision came to, so that a
// decision taken as a bool can be handed to gen.escapeWhy.
func placementOf(heap bool) ir.AllocPlacement {
	if heap {
		return ir.AllocOnHeap
	}
	return ir.AllocInFrame
}

// placementAllocator names the allocator a placement of valueType calls on the
// heap, or would have called from a frame. It is empty when the site has no
// type, and when the build allocates with calloc rather than the Go runtime --
// a calloc call is not one of the allocators the census counts, so a frame
// record naming it could never pair with anything.
func (g *gen) placementAllocator(valueType types.Type) string {
	if valueType == nil || !g.runtimeAllocation {
		return ""
	}
	return "runtime.newobject"
}

// placementTypeSymbol is the symbol runtimeTypeSymbol would intern valueType's
// descriptor under, computed without emitting the descriptor.
//
// Emitting it would be wrong twice over: a diagnostic record must not change
// the module, and a frame placement is precisely the case where no descriptor
// is needed, so asking for one would add data to every program that has a
// composite literal in a frame.
func (g *gen) placementTypeSymbol(valueType types.Type) string {
	if valueType == nil {
		return ""
	}
	return contentSymbolName(".goc.runtime.type", goTypeKey(g.fset, valueType))
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
		pointerSize := int64(g.target.sizes().Sizeof(types.Typ[types.Uintptr]))
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
				backingType := types.NewArray(sliceType.Elem(), fixedCapacity)
				data = g.localAlloc(alignment, int(fixedCapacity*elementSize))
				g.recordPlacement(data, "make-slice-backing", ir.AllocInFrame, backingType)
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
	case "complex":
		return g.complexValue(call)
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
		panicSignature := types.NewSignatureType(
			nil,
			nil,
			nil,
			types.NewTuple(types.NewParam(token.NoPos, nil, "e", anyType)),
			nil,
			false,
		)
		g.callVoidWithSignature(g.fn.Sym("runtime.gopanic", 0), []ir.Ref{value}, panicSignature, nil)
		g.cur.Hlt()
		return g.fn.Word(0)
	case "recover":
		if g.runtimeAllocation {
			anyType := types.NewInterfaceType(nil, nil)
			anyType.Complete()
			recoverSignature := types.NewSignatureType(
				nil,
				nil,
				nil,
				nil,
				types.NewTuple(types.NewParam(token.NoPos, nil, "", anyType)),
				false,
			)
			resultClass, _ := scalar(anyType)
			return g.callWithSignature(resultClass, g.fn.Sym("runtime.gorecover", 0), nil, recoverSignature, nil)
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

func (g *gen) complexValue(call *ast.CallExpr) ir.Ref {
	resultType, ok := g.typeAndValue(call).Type.Underlying().(*types.Basic)
	if !ok {
		g.fail(call, "complex result is not a complex number")
		return ir.R
	}

	realPart := g.expr(call.Args[0])
	imaginaryPart := g.expr(call.Args[1])
	switch resultType.Kind() {
	case types.Complex64:
		return g.packComplex64(realPart, imaginaryPart)
	case types.Complex128:
		result := g.localAllocTyped(g.typeAndValue(call).Type)
		g.cur.StoreSub(ir.SubD, realPart, result)
		g.cur.StoreSub(ir.SubD, imaginaryPart, g.offset(result, 8))
		return result
	default:
		g.fail(call, "complex result is not a complex number")
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
		realPart, imaginaryPart := g.complex64Parts(value)
		if imaginary {
			return imaginaryPart
		}
		return realPart
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

func (g *gen) complexConstant(node ast.Node, value constant.Value, valueType types.Type) ir.Ref {
	realValue, _ := constant.Float64Val(constant.Real(value))
	imaginaryValue, _ := constant.Float64Val(constant.Imag(value))
	basic, ok := valueType.Underlying().(*types.Basic)
	if !ok {
		g.fail(node, "complex constant has non-basic type %s", valueType)
		return ir.R
	}
	switch basic.Kind() {
	case types.Complex64:
		realBits := uint64(math.Float32bits(float32(realValue)))
		imaginaryBits := uint64(math.Float32bits(float32(imaginaryValue)))
		return g.fn.Long(int64(realBits | imaginaryBits<<32))
	case types.Complex128:
		result := g.localAllocTyped(valueType)
		g.cur.StoreSub(ir.SubD, g.fn.Double(realValue), result)
		g.cur.StoreSub(ir.SubD, g.fn.Double(imaginaryValue), g.offset(result, 8))
		return result
	default:
		g.fail(node, "unsupported complex constant type %s", valueType)
		return ir.R
	}
}

func (g *gen) complexBinary(op token.Token, left, right ir.Ref, valueType types.Type, node ast.Node) (ir.Ref, bool) {
	basic, ok := valueType.Underlying().(*types.Basic)
	if !ok {
		return ir.R, false
	}
	switch basic.Kind() {
	case types.Complex64:
		return g.complex64Binary(op, left, right, node), true
	case types.Complex128:
		return g.complex128Binary(op, left, right, valueType, node), true
	default:
		return ir.R, false
	}
}

// A complex64 is represented as its two float32 halves packed into one 64-bit
// integer, so moving a half between the two forms is a bitwise reinterpretation
// between a general-purpose and a floating-point register. That is OCast; OCopy
// is a plain register move and re-types only between classes of the same file,
// so using it here left the float half reading whatever the integer register
// happened to alias.
func (g *gen) complex64Parts(value ir.Ref) (realPart, imaginaryPart ir.Ref) {
	realPart = g.cur.Cast(ir.ClsS, g.cur.Copy(ir.ClsW, value))
	imaginaryPart = g.cur.Cast(ir.ClsS, g.cur.Copy(ir.ClsW, g.cur.Shr(ir.ClsL, value, g.fn.Long(32))))
	return realPart, imaginaryPart
}

func (g *gen) packComplex64(realPart, imaginaryPart ir.Ref) ir.Ref {
	realBits := g.cur.Extuw(ir.ClsL, g.cur.Cast(ir.ClsW, realPart))
	imaginaryBits := g.cur.Shl(ir.ClsL, g.cur.Extuw(ir.ClsL, g.cur.Cast(ir.ClsW, imaginaryPart)), g.fn.Long(32))
	return g.cur.Or(ir.ClsL, realBits, imaginaryBits)
}

// complexConversion converts between complex64 and complex128. The two have
// different representations -- packed halves against a 16-byte value addressed
// by a pointer -- so the generic scalar path in convert would reinterpret one
// as the other.
func (g *gen) complexConversion(value ir.Ref, from, to types.Type) ir.Ref {
	fromBasic, _ := from.Underlying().(*types.Basic)
	if fromBasic.Kind() == types.Complex64 {
		realPart, imaginaryPart := g.complex64Parts(value)
		result := g.localAllocTyped(to)
		g.cur.StoreSub(ir.SubD, g.cur.Exts(realPart), result)
		g.cur.StoreSub(ir.SubD, g.cur.Exts(imaginaryPart), g.offset(result, 8))
		return result
	}
	realPart := g.cur.Truncd(g.cur.Load(ir.ClsD, value))
	imaginaryPart := g.cur.Truncd(g.cur.Load(ir.ClsD, g.offset(value, 8)))
	return g.packComplex64(realPart, imaginaryPart)
}

func isComplexType(valueType types.Type) bool {
	basic, ok := valueType.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsComplex != 0
}

func (g *gen) complex64Binary(op token.Token, left, right ir.Ref, node ast.Node) ir.Ref {
	leftReal, leftImaginary := g.complex64Parts(left)
	rightReal, rightImaginary := g.complex64Parts(right)
	switch op {
	case token.ADD:
		return g.packComplex64(
			g.cur.Add(ir.ClsS, leftReal, rightReal),
			g.cur.Add(ir.ClsS, leftImaginary, rightImaginary),
		)
	case token.SUB:
		return g.packComplex64(
			g.cur.Sub(ir.ClsS, leftReal, rightReal),
			g.cur.Sub(ir.ClsS, leftImaginary, rightImaginary),
		)
	case token.MUL:
		realPart := g.cur.Sub(
			ir.ClsS,
			g.cur.Mul(ir.ClsS, leftReal, rightReal),
			g.cur.Mul(ir.ClsS, leftImaginary, rightImaginary),
		)
		imaginaryPart := g.cur.Add(
			ir.ClsS,
			g.cur.Mul(ir.ClsS, leftReal, rightImaginary),
			g.cur.Mul(ir.ClsS, leftImaginary, rightReal),
		)
		return g.packComplex64(realPart, imaginaryPart)
	case token.EQL, token.NEQ:
		realEqual := g.cur.Cmp(ir.CmpFeq, ir.ClsS, leftReal, rightReal)
		imaginaryEqual := g.cur.Cmp(ir.CmpFeq, ir.ClsS, leftImaginary, rightImaginary)
		equal := g.cur.And(ir.ClsW, realEqual, imaginaryEqual)
		if op == token.NEQ {
			return g.cur.Cmp(ir.CmpEq, ir.ClsW, equal, g.fn.Word(0))
		}
		return equal
	default:
		g.fail(node, "unsupported complex64 operator %s", op)
		return ir.R
	}
}

func (g *gen) complex128Parts(value ir.Ref) (realPart, imaginaryPart ir.Ref) {
	return g.cur.Load(ir.ClsD, value), g.cur.Load(ir.ClsD, g.offset(value, 8))
}

func (g *gen) complex128Binary(op token.Token, left, right ir.Ref, valueType types.Type, node ast.Node) ir.Ref {
	leftReal, leftImaginary := g.complex128Parts(left)
	rightReal, rightImaginary := g.complex128Parts(right)
	switch op {
	case token.ADD:
		return g.complex128Value(
			valueType,
			g.cur.Add(ir.ClsD, leftReal, rightReal),
			g.cur.Add(ir.ClsD, leftImaginary, rightImaginary),
		)
	case token.SUB:
		return g.complex128Value(
			valueType,
			g.cur.Sub(ir.ClsD, leftReal, rightReal),
			g.cur.Sub(ir.ClsD, leftImaginary, rightImaginary),
		)
	case token.MUL:
		realPart := g.cur.Sub(
			ir.ClsD,
			g.cur.Mul(ir.ClsD, leftReal, rightReal),
			g.cur.Mul(ir.ClsD, leftImaginary, rightImaginary),
		)
		imaginaryPart := g.cur.Add(
			ir.ClsD,
			g.cur.Mul(ir.ClsD, leftReal, rightImaginary),
			g.cur.Mul(ir.ClsD, leftImaginary, rightReal),
		)
		return g.complex128Value(valueType, realPart, imaginaryPart)
	case token.EQL, token.NEQ:
		realEqual := g.cur.Cmp(ir.CmpFeq, ir.ClsD, leftReal, rightReal)
		imaginaryEqual := g.cur.Cmp(ir.CmpFeq, ir.ClsD, leftImaginary, rightImaginary)
		equal := g.cur.And(ir.ClsW, realEqual, imaginaryEqual)
		if op == token.NEQ {
			return g.cur.Cmp(ir.CmpEq, ir.ClsW, equal, g.fn.Word(0))
		}
		return equal
	default:
		g.fail(node, "unsupported complex128 operator %s", op)
		return ir.R
	}
}

func (g *gen) complex128Value(valueType types.Type, realPart, imaginaryPart ir.Ref) ir.Ref {
	result := g.localAllocTyped(valueType)
	g.cur.StoreSub(ir.SubD, realPart, result)
	g.cur.StoreSub(ir.SubD, imaginaryPart, g.offset(result, 8))
	return result
}

func (g *gen) keepAlive(function *types.Func, argument ast.Expr) {
	anyType := types.NewInterfaceType(nil, nil)
	anyType.Complete()
	value := g.assignmentValue(argument, anyType)
	keepAliveSlot := ir.R
	if identifier, ok := argument.(*ast.Ident); ok {
		object := g.info.Uses[identifier]
		if tracked := g.keepAliveValues[object]; tracked != ir.R {
			value = g.adaptValueToInterface(tracked, g.typeAndValue(argument).Type, anyType, ir.R, argument)
		}
		if slot, ok := g.keepAliveSlots[object]; ok {
			keepAliveSlot = slot
		}
	}
	signature := function.Type().(*types.Signature)
	g.callVoidWithSignature(g.fn.Sym(g.functionSymbol(function), 0), []ir.Ref{value}, signature, nil)
	if keepAliveSlot != ir.R {
		g.store(g.fn.ConstInt(ir.ClsP, 0), keepAliveSlot, types.Typ[types.UnsafePointer])
	}
}

// appendedMakeLength recognises `append(s, make([]T, n)...)` and names the
// length expression.
//
// The idiom is how Go grows a slice by n zero elements, and the standard library
// relies on it costing one allocation: slices.Grow is written
//
//	s = append(s[:cap(s)], make([]E, n)...)[:len(s)]
//
// with the comment "This expression allocates only once (see test)". Compiled
// literally it allocates twice -- once for the fresh slice and once for the
// grown destination -- and the elements copied out of the fresh slice are every
// one of them zero. cmd/compile rewrites it (walk's extendslice); goc did not,
// so every slices.Grow in the program cost an allocation that the source says it
// does not. It is what `logattrs/6-attr` pays over gc in the slog benchmark.
//
// Only the two-argument make qualifies. `make([]T, n, m)` asks for a capacity as
// well, and the extension has no way to honour it.
func appendedMakeLength(call *ast.CallExpr, info *types.Info) (ast.Expr, bool) {
	if !call.Ellipsis.IsValid() || len(call.Args) != 2 {
		return nil, false
	}
	made, isCall := call.Args[1].(*ast.CallExpr)
	if !isCall || len(made.Args) != 2 || made.Ellipsis.IsValid() {
		return nil, false
	}
	identifier, isIdentifier := made.Fun.(*ast.Ident)
	if !isIdentifier {
		return nil, false
	}
	builtin, isBuiltin := info.Uses[identifier].(*types.Builtin)
	if !isBuiltin || builtin.Name() != "make" {
		return nil, false
	}
	madeType := info.TypeOf(made)
	if madeType == nil {
		return nil, false
	}
	if _, isSlice := madeType.Underlying().(*types.Slice); !isSlice {
		return nil, false
	}
	return made.Args[1], true
}

// guardExtensionLength keeps `append(s, make([]T, n)...)` panicking on a negative
// n, which is what the make it replaced did.
//
// It matters more here than it would as a bounds check. A negative n makes the
// new length smaller than the old, so the grow branch is not taken and the clear
// below runs against the *existing* backing array with a byte count that is
// negative as a signed number and enormous as the unsigned one memset reads. The
// branch is emitted rather than the check inlined so that the panic is
// runtime.makeslice's own, with the message the source would have produced.
func (g *gen) guardExtensionLength(made ast.Expr, length ir.Ref) {
	if !g.runtimeAllocation {
		// Without a runtime there is no runtime panic to raise, and this mode
		// already builds `make([]T, n)` out of an unchecked byte count.
		return
	}
	bad := g.block("appendextendbad")
	ok := g.block("appendextendok")
	negative := g.cur.Cmp(ir.CmpSlt, ir.ClsL, length, g.fn.Long(0))
	g.cur.Jnz(negative, bad, ok)
	g.cur = bad
	// runtime.panicmakeslicelen rather than runtime.makeslice, which would panic
	// the same way. makeslice is an *allocator*, so a call to it is an allocation
	// census row -- and one on a branch that only ever panics reads as "goc
	// allocates on the heap here" for every slices.Grow in the program, which is
	// what the first version of this guard produced and what the gc differential
	// then counted. This is the same panic makeslice would raise, with its
	// message, and nothing reads it as a placement.
	//
	// Positioned at the make it stands in for: a fresh block carries no source
	// position until one is set.
	g.at(made)
	g.cur.CallVoid(g.fn.Sym("runtime.panicmakeslicelen", 0))
	g.cur.Goto(ok)
	g.cur = ok
	g.at(made)
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
	zeroFill := false
	if call.Ellipsis.IsValid() {
		if length, extends := appendedMakeLength(call, g.info); extends {
			// append(s, make([]T, n)...) -- see appendedMakeLength. The elements
			// copied out of the fresh slice are all zero, so the fresh slice is
			// not built at all and the destination's new region is cleared
			// instead.
			added = g.expr(length)
			g.guardExtensionLength(call.Args[1], added)
			zeroFill = true
		} else {
			source := g.expr(call.Args[1])
			sourceType := g.typeAndValue(call.Args[1]).Type.Underlying()
			basicType, isBasic := sourceType.(*types.Basic)
			if isBasic && basicType.Kind() == types.String {
				sourceData = g.cur.Load(ir.ClsP, source)
				added = g.cur.Load(ir.ClsL, g.offset(source, 8))
			} else {
				sourceData, added, _ = g.sliceParts(source)
			}
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
		// A growing append gives Go a *new* backing array and leaves the old
		// one intact, because other slices may still refer to it. Growing in
		// place with realloc broke that, and it also assumed the old array came
		// from the allocator: a backing array the escape walk left on the frame
		// -- values := []int{7} whose slice never leaves the function -- was
		// handed to realloc and the program faulted.
		data := g.allocateZeroed(bytes)
		copiedBytes := oldLength
		if elementSize != 1 {
			copiedBytes = g.cur.Mul(ir.ClsL, oldLength, g.fn.Long(elementSize))
		}
		g.cur.Call(ir.ClsP, g.fn.Sym("goc_memcpy", 0), data, oldData, copiedBytes)
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
	if zeroFill {
		byteLength := added
		if elementSize != 1 {
			byteLength = g.cur.Mul(ir.ClsL, added, g.fn.Long(elementSize))
		}
		// The new region has to be cleared on both paths and not only the
		// growing one. runtime.growslice clears [newLen, cap) and leaves
		// [oldLen, newLen) alone for a pointer-free element type, because the
		// append that called it is expected to overwrite exactly that; and the
		// non-growing path is reusing a backing array whose tail holds whatever
		// a previous, longer life of the slice left there.
		//
		// A pointer-bearing element type is cleared through the barrier, for
		// the same reason `clear` is: zeroing a pointer word is a deletion and
		// the collector has to see it.
		if g.runtimeAllocation && !g.noWriteBarrier && len(pointerWordIndices(elementType)) != 0 {
			g.cur.CallVoid(g.fn.Sym("runtime.memclrHasPointers", 0), writeAt, byteLength)
		} else {
			g.cur.Call(ir.ClsP, g.fn.Sym("goc_memset", 0), writeAt, g.fn.Word(0), byteLength)
		}
	} else if sourceData != ir.R {
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

// channelType emits the abi.ChanType that runtime.makechan is called with.
//
// Its element field must be the same complete descriptor every other allocation
// site uses. makechan reads Elem.PtrBytes to decide whether the buffer holds
// pointers at all -- a zero sends it down the branch that allocates the buffer
// inside the no-scan hchan object, where the collector never sees the elements --
// and chansend, chanrecv and sendDirect hand Elem to typedmemmove and
// bulkBarrierPreWriteSrcOnly, both of which skip the barrier when PtrBytes is
// zero. A stub carrying only size, alignment and kind is therefore not enough.
func (g *gen) channelType(channel *types.Chan) ir.Ref {
	element := channel.Elem()
	elementName := g.runtimeTypeSymbol(element)
	channelName, fresh := g.internSymbol(".goc.channel.type", goTypeKey(g.fset, element))
	if !fresh {
		return g.fn.Sym(channelName, 0)
	}
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: channelName, Align: 8, Items: []ir.DataItem{
			{Zero: 48},
			{Sub: ir.SubL, Sym: elementName},
			{Sub: ir.SubL, Ints: []int64{3}},
		}},
	)
	return g.fn.Sym(channelName, 0)
}

func (g *gen) runtimeType(valueType types.Type) ir.Ref {
	return g.fn.Sym(g.runtimeTypeSymbol(valueType), 0)
}

// runtimeTypeSymbol emits the complete abi.Type descriptor for valueType, if it
// has not already been emitted, and returns the name of its data symbol. It is
// separate from runtimeType so that a descriptor can be referenced from another
// datum rather than only loaded as an address.
func (g *gen) runtimeTypeSymbol(valueType types.Type) string {
	name, fresh := g.internSymbol(".goc.runtime.type", goTypeKey(g.fset, valueType))
	if !fresh {
		return name
	}
	maskName := name + ".gcdata"
	size := typeSize(valueType)
	mask := make([]int64, (size+63)/64)
	lastPointer := int64(0)
	markPointerWords(valueType, 0, mask, &lastPointer)
	mask = paddedPointerMask(mask)
	alignment := typeAlign(valueType)
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: maskName, Align: int(pointerSize()), Items: []ir.DataItem{{Sub: ir.SubUB, Ints: mask}}},
		&ir.Data{Name: name, Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Ints: []int64{size, lastPointer}},
			{Sub: ir.SubW, Ints: []int64{0}},
			{Sub: ir.SubUB, Ints: []int64{0, alignment, alignment, int64(runtimeKind(valueType))}},
			{Sub: ir.SubL, Ints: []int64{0}},
			{Sub: ir.SubL, Sym: maskName},
			{Sub: ir.SubW, Ints: []int64{0, 0}},
		}},
	)
	return name
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
	return collectPointerWordIndices(valueType, false)
}

// barrieredPointerWordIndices returns the words of valueType whose stores need a
// write barrier. It is pointerWordIndices minus the words whose static type is a
// pointer to a not-in-heap type; see isNotInHeapPointer for why those must not
// go through the barrier. Pointer maps and GC metadata keep using
// pointerWordIndices, so neither the in-memory layout nor the emitted type
// descriptors change: a word dropped here is copied by the surrounding memcpy
// instead of being republished through goc_storep.
func barrieredPointerWordIndices(valueType types.Type) []int {
	return collectPointerWordIndices(valueType, true)
}

func collectPointerWordIndices(valueType types.Type, skipNotInHeap bool) []int {
	seen := make(map[int]bool)
	walkPointerWords(valueType, 0, skipNotInHeap, func(offset int64) {
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

// visitPointerWords reports every pointer-sized word of valueType that the
// garbage collector must treat as a pointer. It is the metadata view: pointer
// masks, stack maps and static data descriptors all use it, and it includes
// pointers to not-in-heap types.
func visitPointerWords(valueType types.Type, base int64, visit func(int64)) {
	walkPointerWords(valueType, base, false, visit)
}

func walkPointerWords(valueType types.Type, base int64, skipNotInHeap bool, visit func(int64)) {
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
	case *types.Pointer:
		if skipNotInHeap && isNotInHeapPointerType(value) {
			return
		}
		visit(base)
	case *types.Map, *types.Chan, *types.Signature:
		visit(base)
	case *types.Slice:
		visit(base)
	case *types.Interface:
		visit(base)
		visit(base + 8)
	case *types.Array:
		elementSize := typeSize(value.Elem())
		for index := int64(0); index < value.Len(); index++ {
			walkPointerWords(value.Elem(), base+index*elementSize, skipNotInHeap, visit)
		}
	case *types.Struct:
		fields := structFields(value)
		offsets := structOffsets(fields)
		for index, field := range fields {
			walkPointerWords(field.Type(), base+offsets[index], skipNotInHeap, visit)
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
	mapType := g.typeAndValue(index.X).Type.Underlying().(*types.Map)
	value, found := g.mapLookup(index)
	g.assignMapResult(statement, 0, value, mapType.Elem())
	g.assignMapResult(statement, 1, found, types.Typ[types.Bool])
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

func (g *gen) assignMapResult(statement *ast.AssignStmt, resultIndex int, value ir.Ref, valueType types.Type) {
	target := g.prepareAssignmentTarget(statement.Lhs[resultIndex], statement.Tok == token.DEFINE)
	g.storeAssignmentTarget(target, value, valueType)
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
	for _, operand := range g.printOperands(call, newline) {
		if operand.literal {
			g.cur.Call(ir.ClsW, printf, g.cString("%s"), g.cString(operand.text))
			continue
		}

		argument := operand.expr
		argumentType := g.info.Types[argument].Type
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
}

// printOperand is one element of the sequence a print or println statement
// writes. The Go specification requires println to separate its operands with
// spaces and to end with a newline, and the host toolchain implements that by
// rewriting the operand list: a " " string is inserted between operands, a
// "\n" string is appended, and runs of adjacent constant strings are then
// collapsed into a single one. cg12 builds the same sequence so that both
// toolchains write the same bytes with the same number of runtime calls.
type printOperand struct {
	// literal reports that this operand is a constant string whose text is
	// known here, either from the source or synthesized as a separator.
	literal bool
	text    string
	expr    ast.Expr
}

func (g *gen) printOperands(call *ast.CallExpr, newline bool) []printOperand {
	sequence := make([]printOperand, 0, 2*len(call.Args)+1)
	for index, argument := range call.Args {
		if newline && index > 0 {
			sequence = append(sequence, printOperand{literal: true, text: " "})
		}
		value := g.info.Types[argument]
		isConstantString := value.Value != nil && value.Value.Kind() == constant.String
		if isConstantString && !isRuntimeQuotedType(value.Type) {
			sequence = append(sequence, printOperand{literal: true, text: constant.StringVal(value.Value)})
			continue
		}
		sequence = append(sequence, printOperand{expr: argument})
	}
	if newline {
		sequence = append(sequence, printOperand{literal: true, text: "\n"})
	}
	return collapsePrintLiterals(sequence)
}

// collapsePrintLiterals joins runs of adjacent constant strings, matching the
// host toolchain's walkPrint. It is what turns println("x", "y") into a single
// printstring("x y\n") rather than five separate runtime calls.
func collapsePrintLiterals(sequence []printOperand) []printOperand {
	collapsed := make([]printOperand, 0, len(sequence))
	for _, operand := range sequence {
		last := len(collapsed) - 1
		if operand.literal && last >= 0 && collapsed[last].literal {
			collapsed[last].text += operand.text
			continue
		}
		collapsed = append(collapsed, operand)
	}
	return collapsed
}

// printStep is one runtime print call with its operand already evaluated.
type printStep struct {
	function  string
	arguments []ir.Ref
}

func (g *gen) builtinRuntimePrint(call *ast.CallExpr, newline bool) {
	// Every operand is evaluated before the print lock is taken, as the host
	// toolchain does: an operand whose own evaluation prints something must
	// not interleave with the output of the statement printing it.
	operands := g.printOperands(call, newline)
	steps := make([]printStep, 0, len(operands))
	for _, operand := range operands {
		step, ok := g.printStep(operand)
		if !ok {
			return
		}
		steps = append(steps, step)
	}

	// printlock and printunlock make the whole statement atomic against other
	// threads. runtime/print.go states outright that the compiler is required
	// to emit them around the calls implementing one print statement, and
	// runtime.minhexdigits is documented as protected by that lock.
	g.callRuntimePrint(call, "printlock")
	for _, step := range steps {
		g.callRuntimePrint(call, step.function, step.arguments...)
	}
	g.callRuntimePrint(call, "printunlock")
}

// printStep selects the runtime print routine for one operand and evaluates
// it, mirroring the host toolchain's walkPrint dispatch.
func (g *gen) printStep(operand printOperand) (printStep, bool) {
	if operand.literal {
		switch operand.text {
		case " ":
			return printStep{function: "printsp"}, true
		case "\n":
			return printStep{function: "printnl"}, true
		}
		return printStep{function: "printstring", arguments: []ir.Ref{g.stringConstant(operand.text)}}, true
	}

	argument := operand.expr
	argumentType := g.info.Types[argument].Type
	if isInterfaceValue(argumentType) {
		interfaceType, _ := argumentType.Underlying().(*types.Interface)
		function := "printiface"
		if interfaceType.NumMethods() == 0 {
			function = "printeface"
		}
		// runtime.printeface and printiface take the two-word pair by value, so
		// the operand has to be a real descriptor rather than the nil pointer a
		// nil interface is represented by.
		descriptor := g.materializeNilInterface(g.expr(argument))
		return printStep{function: function, arguments: []ir.Ref{descriptor}}, true
	}
	if isRuntimeQuotedType(argumentType) {
		return printStep{function: "printquoted", arguments: []ir.Ref{g.expr(argument)}}, true
	}
	if isStringType(argumentType) {
		return printStep{function: "printstring", arguments: []ir.Ref{g.expr(argument)}}, true
	}
	if isSliceType(argumentType) {
		return printStep{function: "printslice", arguments: []ir.Ref{g.expr(argument)}}, true
	}

	class, ok := scalar(argumentType)
	if !ok {
		g.fail(argument, "unsupported print operand %s", argumentType)
		return printStep{}, false
	}
	argumentValue := g.expr(argument)
	basic, isBasic := argumentType.Underlying().(*types.Basic)
	switch {
	case isBasic && basic.Kind() == types.Bool:
		return printStep{function: "printbool", arguments: []ir.Ref{argumentValue}}, true
	case isBasic && basic.Kind() == types.Complex64:
		return printStep{function: "printcomplex64", arguments: []ir.Ref{argumentValue}}, true
	case isBasic && basic.Kind() == types.Complex128:
		return printStep{function: "printcomplex128", arguments: []ir.Ref{argumentValue}}, true
	case class == ir.ClsS:
		return printStep{function: "printfloat32", arguments: []ir.Ref{argumentValue}}, true
	case class == ir.ClsD:
		return printStep{function: "printfloat64", arguments: []ir.Ref{argumentValue}}, true
	case isRuntimePrintPointer(argumentType):
		return printStep{function: "printhex", arguments: []ir.Ref{g.cur.Copy(ir.ClsL, argumentValue)}}, true
	case isRuntimeHexType(argumentType):
		return printStep{function: "printhex", arguments: []ir.Ref{g.cur.Copy(ir.ClsL, argumentValue)}}, true
	case isBasic && basic.Info()&types.IsUnsigned != 0:
		if class == ir.ClsW {
			argumentValue = g.cur.Extuw(ir.ClsL, argumentValue)
		}
		return printStep{function: "printuint", arguments: []ir.Ref{argumentValue}}, true
	default:
		if class == ir.ClsW {
			argumentValue = g.cur.Extsw(ir.ClsL, argumentValue)
		}
		return printStep{function: "printint", arguments: []ir.Ref{argumentValue}}, true
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

// isRuntimeHexType reports the runtime's own `type hex uint64`, which the
// runtime uses purely to select hexadecimal formatting for addresses. Its
// underlying type is an ordinary unsigned integer, so without this the print
// builtin would route it to printuint and every address in a runtime
// diagnostic would come out in decimal. The standard compiler special-cases
// the same named type.
func isRuntimeHexType(valueType types.Type) bool {
	return isRuntimeNamedType(valueType, "hex")
}

// isRuntimeQuotedType reports the runtime's own `type quoted string`, which
// selects the quoted, escaped rendering the standard compiler gives it. The
// runtime prints goroutine labels through it in tracebacks, so without this a
// label containing a quote or a newline would corrupt the traceback.
func isRuntimeQuotedType(valueType types.Type) bool {
	return isRuntimeNamedType(valueType, "quoted")
}

func isRuntimeNamedType(valueType types.Type, name string) bool {
	named, ok := valueType.(*types.Named)
	if !ok {
		return false
	}
	object := named.Obj()
	if object == nil || object.Name() != name || object.Pkg() == nil {
		return false
	}
	return object.Pkg().Path() == "runtime"
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
	name := g.literalDataSymbol(".goc.cstring", 1, values)
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

// typeSize is the size goc lays t out at. It, and the other helpers below that
// reach goTypeSizes, take no target on purpose -- see goTypeSizes for why, and
// for the check that keeps the omission honest.
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
	sizes := goTypeSizes()
	return sizes.Sizeof(t)
}

// trailingZeroSizedFieldNeedsPadding reports whether structure ends in a
// zero-sized field that follows something, which is the condition under which
// gc -- and typeSize above -- gives the struct an extra byte so that the address
// of that last field is still inside the object.
//
// It is asked by goABIAggregate, whose fields carry no size of their own for the
// zero-sized cases: an empty struct, an empty array, or a named type of either
// all flatten to nothing.
func trailingZeroSizedFieldNeedsPadding(structure *types.Struct) bool {
	fields := structFields(structure)
	if len(fields) == 0 {
		return false
	}
	last := fields[len(fields)-1]
	if typeSize(last.Type()) != 0 {
		return false
	}
	return structOffsets(fields)[len(fields)-1] > 0
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
	return int64(goTypeSizes().Alignof(t))
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
	return int64(goTypeSizes().Sizeof(types.Typ[types.Uintptr]))
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
	sizes := goTypeSizes()
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
		sizes := goTypeSizes()
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
		named, namedReceiver := types.Unalias(receiver).(*types.Named)
		if namedReceiver && named.TypeArgs().Len() > 0 {
			open := strings.IndexByte(typeName, '[')
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
	if name := g.linkNames[function.Origin()]; name != "" {
		return name
	}
	if function.Pkg() != nil && function.Pkg().Path() == "iter" {
		switch function.Name() {
		case "newcoro":
			return "runtime.newcoro"
		case "coroswitch":
			return "runtime.coroswitch"
		}
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
	if result, ok := g.complexBinary(op, x, y, t, n); ok {
		return result
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

// runtimeTypeSymbolName turns a type key into a stable symbol name.
//
// The key is a Go type string, so it carries characters a symbol cannot -- the
// stars, brackets, braces and spaces of `*[]struct{ x int }`. Sanitizing alone
// would collide (`[]int` and `[ ]int` sanitize alike), so a digest of the
// original key is appended: the readable part is for humans reading a
// disassembly, the digest is what actually distinguishes the symbols.
func runtimeTypeSymbolName(key string) string {
	return contentSymbolName(".goc.type", key)
}

// contentSymbolName builds a symbol name from what the symbol contains rather
// than from how many symbols came before it.
//
// A name derived from a running count encodes the order the module was built
// in. That made the compiler non-deterministic until the traversals were
// ordered, and it leaves the property conditional: any new unordered walk
// reintroduces it. More importantly, a counter cannot survive separate
// compilation, where a prebuilt runtime and a program each start counting at
// zero and collide, and where adding one datum to the program renumbers every
// symbol in the runtime.
//
// The key is arbitrary text -- a Go type string, a string literal, a symbol --
// so it carries characters a symbol name cannot. Sanitizing alone would
// collide, since "[]int" and "[ ]int" sanitize alike, so a digest of the
// original key decides identity and the readable prefix is only there for
// whoever is reading a disassembly.
func contentSymbolName(prefix, key string) string {
	var readable strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			readable.WriteRune(r)
		default:
			readable.WriteByte('_')
		}
	}
	trimmed := strings.Trim(readable.String(), "_")
	if len(trimmed) > 48 {
		trimmed = trimmed[:48]
	}
	digest := sha256.Sum256([]byte(key))
	if trimmed == "" {
		return fmt.Sprintf("%s.%x", prefix, digest[:8])
	}
	return fmt.Sprintf("%s.%s.%x", prefix, trimmed, digest[:8])
}

// literalDataSymbol interns a byte-valued data symbol under a name derived from
// its contents.
//
// Naming these by a running count of the module's data made the name depend on
// how much had been emitted first, which is why the same literal was called
// .goc.string.412 in one build and .goc.string.418 in the next. Interning also
// means a literal that appears twice is emitted once, which the counter form
// could not do.
func (g *gen) literalDataSymbol(prefix string, align int, values []int64) string {
	contents := make([]byte, len(values))
	for index, value := range values {
		contents[index] = byte(value)
	}
	key := prefix + ":" + string(contents)
	if name := g.literalData[key]; name != "" {
		return name
	}
	name := contentSymbolName(prefix, string(contents))
	g.literalData[key] = name
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: align,
		Items: []ir.DataItem{{Sub: ir.SubUB, Ints: values}},
	})
	return name
}

// internSymbol returns the content-derived name for a symbol, and whether the
// caller still has to emit it. A counter-named symbol was unique by
// construction; a content-named one is only unique if the second request for
// the same content reuses the first symbol.
func (g *gen) internSymbol(prefix, key string) (string, bool) {
	full := prefix + ":" + key
	if name := g.contentSymbols[full]; name != "" {
		return name, false
	}
	name := contentSymbolName(prefix, key)
	g.contentSymbols[full] = name
	return name, true
}
