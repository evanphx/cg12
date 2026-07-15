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
	"strings"

	"github.com/evanphx/cg12/ir"
)

// Compile parses and type-checks one Go source file and lowers it to cg12 IR.
// The initial frontend supports scalar integer and boolean code.  More complex
// Go values are rejected explicitly, rather than being lowered with a wrong ABI.
func Compile(name string, src []byte) (*ir.Module, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.AllErrors)
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
	mod := ir.NewModule()
	emitRuntimeTables := false
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok {
			if function, ok := info.Uses[selector.Sel].(*types.Func); ok && function.Pkg() != nil && function.Pkg().Path() == "runtime" && function.Name() == "GC" {
				emitRuntimeTables = true
			}
		}
		return true
	})
	compileRuntime := emitRuntimeTables
	typeTags := make(map[string]string)
	linkNames := make(map[*types.Func]string)
	collectLinkNames([]*ast.File{file}, info, linkNames)
	for _, unit := range loader.units {
		collectLinkNames(unit.files, unit.info, linkNames)
	}
	g := &gen{fset: fset, file: file, info: info, pkg: pkg, mod: mod, globals: map[types.Object]string{}, emitRuntimeTables: emitRuntimeTables, runtimeAllocation: compileRuntime, typeTags: typeTags, linkNames: linkNames}
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
		generator := &gen{fset: fset, info: unit.info, pkg: unit.pkg, mod: mod, globals: globals, emitRuntimeTables: emitRuntimeTables, runtimeAllocation: compileRuntime, typeTags: typeTags, linkNames: linkNames}
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
	var roots []*ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			roots = append(roots, function)
		}
	}
	functions := reachableFunctions(roots, info, pkg, loader.units, compileRuntime)
	methodTargets := make(map[string]*types.Func)
	for _, function := range functions {
		object, ok := function.info.Defs[function.decl.Name].(*types.Func)
		if !ok {
			continue
		}
		signature := object.Type().(*types.Signature)
		if signature.Recv() != nil {
			methodTargets[object.Name()] = object
		}
	}
	g.methodTargets = methodTargets
	for i := len(functions) - 1; i >= 0; i-- {
		function := functions[i]
		generator := g
		if function.pkg != pkg {
			generator = &gen{
				fset:              fset,
				info:              function.info,
				pkg:               function.pkg,
				mod:               mod,
				globals:           packageGlobals[function.pkg.Path()],
				methodTargets:     methodTargets,
				emitRuntimeTables: emitRuntimeTables,
				runtimeAllocation: compileRuntime,
				typeTags:          typeTags,
				linkNames:         linkNames,
			}
		}
		generator.funcDecl(function.decl)
		if generator.err != nil {
			return nil, generator.err
		}
	}
	if loader.units["crypto/internal/fips140"] != nil {
		addFIPSRuntimeStubs(mod)
	}
	if loader.units["crypto/sha1"] != nil || loader.units["crypto/md5"] != nil {
		addLegacyCryptoRuntimeStubs(mod)
	}
	if g.err != nil {
		return nil, g.err
	}
	return g.mod, nil
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
	fset              *token.FileSet
	file              *ast.File
	info              *types.Info
	pkg               *types.Package
	mod               *ir.Module
	fn                *ir.Func
	cur               *ir.Block
	vars              map[types.Object]ir.Ref
	globals           map[types.Object]string
	breaks, continues []*ir.Block
	seq               int
	err               error
	methodTargets     map[string]*types.Func
	resultSlot        ir.Ref
	resultType        types.Type
	extraResultSlots  []ir.Ref
	extraResultTypes  []types.Type
	labels            map[string]*ir.Block
	deferSlots        map[*ast.DeferStmt]ir.Ref
	deferOrder        []*ast.DeferStmt
	deferActions      []*ast.DeferStmt
	runningDefers     bool
	emitRuntimeTables bool
	runtimeAllocation bool
	typeTags          map[string]string
	linkNames         map[*types.Func]string
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
			if array, ok := obj.Type().Underlying().(*types.Array); ok {
				g.globalArray(id, obj, array, vs, i)
				if g.err != nil {
					return
				}
				continue
			}
			if _, ok := obj.Type().Underlying().(*types.Struct); ok {
				g.globalStruct(id, obj, vs, i)
				if g.err != nil {
					return
				}
				continue
			}
			if slice, ok := obj.Type().Underlying().(*types.Slice); ok {
				g.globalSlice(id, obj, slice, vs, i)
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
				if tv.Value == nil {
					address, ok := initializer.(*ast.UnaryExpr)
					if !ok || address.Op != token.AND {
						continue
					}
					target, ok := address.X.(*ast.Ident)
					if !ok {
						continue
					}
					targetObject := g.info.Uses[target]
					if targetObject == nil || targetObject.Pkg() == nil {
						continue
					}
					d.Items = []ir.DataItem{{Sub: ir.SubL, Sym: targetObject.Pkg().Path() + "." + targetObject.Name()}}
					g.mod.Data = append(g.mod.Data, d)
					g.globals[obj] = name
					continue
				}
				v = constInt(tv.Value)
			}
			if cls == ir.ClsL || cls == ir.ClsP {
				d.Items = []ir.DataItem{{Sub: ir.SubL, Ints: []int64{v}}}
			} else {
				d.Align = 4
				d.Items = []ir.DataItem{{Sub: ir.SubW, Ints: []int64{v}}}
			}
			g.mod.Data = append(g.mod.Data, d)
			g.globals[obj] = name
		}
	}
}

func (g *gen) globalSlice(id *ast.Ident, object types.Object, slice *types.Slice, spec *ast.ValueSpec, valueIndex int) {
	name := g.pkg.Path() + "." + id.Name
	if valueIndex >= len(spec.Values) {
		g.mod.Data = append(g.mod.Data, &ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}})
		g.globals[object] = name
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
		return
	}
	backingName := name + ".backing"
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
	if _, stringElements := slice.Elem().Underlying().(*types.Basic); !stringElements {
		items = []ir.DataItem{{Zero: int(typeSize(slice.Elem())) * len(literal.Elts)}}
	}
	g.mod.Data = append(g.mod.Data,
		&ir.Data{Name: backingName, Align: 8, Items: items},
		&ir.Data{Name: name + ".descriptor", Align: 8, Items: []ir.DataItem{
			{Sub: ir.SubL, Sym: backingName},
			{Sub: ir.SubL, Ints: []int64{int64(len(literal.Elts)), int64(len(literal.Elts))}},
		}},
		&ir.Data{Name: name, Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Sym: name + ".descriptor"}}},
	)
	g.globals[object] = name
}

func (g *gen) globalStruct(id *ast.Ident, object types.Object, spec *ast.ValueSpec, valueIndex int) {
	if valueIndex < len(spec.Values) {
		if _, ok := spec.Values[valueIndex].(*ast.CompositeLit); !ok {
			return
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
				{Zero: int(size - 192)},
			}},
		)
		g.globals[object] = g.pkg.Path() + "." + id.Name
		return
	}
	data := &ir.Data{
		Name:  g.pkg.Path() + "." + id.Name,
		Align: int(types.SizesFor("gc", runtime.GOARCH).Alignof(object.Type())),
		Items: []ir.DataItem{{Sub: ir.SubUB, Ints: make([]int64, size)}},
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
		for i := range items {
			items[i] = ir.DataItem{Sub: ir.SubL, Ints: []int64{0}}
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
		g.mod.Data = append(g.mod.Data, &ir.Data{Name: name, Align: 8, Items: items})
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
				Align: int(types.SizesFor("gc", runtime.GOARCH).Alignof(array)),
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
			return
		}
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
func signed(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsUnsigned == 0 && b.Info()&types.IsBoolean == 0
}

func subOf(t types.Type) (ir.SubCls, bool) {
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
		return g.cur.Alloc(8, 8)
	}
	return g.cur.Alloc(4, 4)
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
	} else {
		g.cur.Store(v, addr)
	}
}
func (g *gen) coerce(v ir.Ref, t types.Type) ir.Ref {
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
	if _, isInterface := targetType.Underlying().(*types.Interface); !isInterface {
		return g.expr(expression)
	}
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Name == "nil" {
		return g.fn.ConstInt(ir.ClsP, 0)
	}
	sourceType := g.info.Types[expression].Type
	if _, alreadyInterface := sourceType.Underlying().(*types.Interface); alreadyInterface {
		return g.expr(expression)
	}
	value := g.expr(expression)
	payload := g.allocLocal(sourceType)
	g.store(value, payload, sourceType)
	descriptor := g.cur.Alloc(8, 16)
	g.cur.Store(g.typeTag(sourceType), descriptor)
	g.cur.Store(payload, g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) typeTag(valueType types.Type) ir.Ref {
	key := types.TypeString(valueType, func(pkg *types.Package) string {
		return pkg.Path()
	})
	name := g.typeTags[key]
	if name == "" {
		name = fmt.Sprintf(".goc.type.%d", len(g.typeTags))
		g.typeTags[key] = name
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:  name,
			Align: 1,
			Items: []ir.DataItem{{Sub: ir.SubUB, Ints: []int64{1}}},
		})
	}
	return g.fn.Sym(name, 0)
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
	exportRuntimeBootstrap := g.pkg.Path() == "runtime" && (fd.Name.Name == "args" || fd.Name.Name == "check" || fd.Name.Name == "osinit" || fd.Name.Name == "schedinit" || fd.Name.Name == "throw")
	if ast.IsExported(fd.Name.Name) || isMain || exportRuntimeBootstrap || (g.pkg.Path() == "internal/chacha8rand" && fd.Name.Name == "block_generic") {
		g.fn.Export()
	}
	g.vars = map[types.Object]ir.Ref{}
	g.resultSlot = ir.R
	g.resultType = nil
	g.extraResultSlots = nil
	g.extraResultTypes = nil
	g.labels = make(map[string]*ir.Block)
	g.deferSlots = make(map[*ast.DeferStmt]ir.Ref)
	g.deferOrder = nil
	g.deferActions = nil
	g.runningDefers = false
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
		slot := g.alloc(v.Type())
		g.store(p, slot, v.Type())
		g.vars[v] = slot
	}
	for i := 1; i < sig.Results().Len(); i++ {
		result := sig.Results().At(i)
		pointer := g.fn.Param(fmt.Sprintf("result%d", i), ir.ClsP)
		g.extraResultSlots = append(g.extraResultSlots, pointer)
		g.extraResultTypes = append(g.extraResultTypes, result.Type())
		if result.Name() != "" {
			g.vars[result] = pointer
			zero, _ := scalar(result.Type())
			g.store(g.fn.ConstInt(zero, 0), pointer, result.Type())
		}
	}
	if sig.Results().Len() > 0 && sig.Results().At(0).Name() != "" {
		result := sig.Results().At(0)
		g.resultSlot = g.alloc(result.Type())
		g.resultType = result.Type()
		g.vars[result] = g.resultSlot
		zero, _ := scalar(result.Type())
		g.store(g.fn.ConstInt(zero, 0), g.resultSlot, result.Type())
	}
	g.stmts(fd.Body.List)
	if g.err == nil && g.live() {
		if platformMain {
			g.cur.Ret(g.fn.Word(0))
		} else if sig.Results().Len() == 0 {
			g.cur.RetVoid()
		} else {
			g.fail(fd.Body, "missing return")
		}
	}
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
					object = target
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
	for _, argument := range call.Args {
		arguments = append(arguments, g.expr(argument))
	}
	if closure != ir.R {
		g.pinClosure(closure)
	}

	values := make([]ir.Ref, signature.Results().Len())
	for i := 1; i < signature.Results().Len(); i++ {
		resultType := signature.Results().At(i).Type()
		slot := g.alloc(resultType)
		arguments = append(arguments, slot)
		values[i] = slot
	}
	resultClass, _ := scalar(signature.Results().At(0).Type())
	values[0] = g.cur.Call(resultClass, callee, arguments...)

	for i, lhs := range statement.Lhs {
		if identifier, ok := lhs.(*ast.Ident); ok && identifier.Name == "_" {
			continue
		}
		resultType := signature.Results().At(i).Type()
		value := values[i]
		if i > 0 {
			value = g.load(value, resultType)
		}

		var slot ir.Ref
		if identifier, ok := lhs.(*ast.Ident); ok {
			variable := g.info.Uses[identifier]
			if statement.Tok == token.DEFINE && variable == nil {
				variable = g.info.Defs[identifier]
			}
			var exists bool
			slot, exists = g.addr(variable)
			if !exists {
				slot = g.alloc(resultType)
				g.vars[variable] = slot
			}
		} else {
			slot = g.lvalue(lhs)
		}
		g.store(g.coerce(value, resultType), slot, resultType)
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
				c, ok := scalar(obj.Type())
				if !ok {
					g.fail(id, "unsupported variable type %s", obj.Type())
					return
				}
				slot := g.allocLocal(obj.Type())
				g.vars[obj] = slot
				if i >= len(vs.Values) && isMemoryValue(obj.Type()) {
					continue
				}
				v := g.fn.ConstInt(c, 0)
				if _, ok := obj.Type().Underlying().(*types.Slice); ok {
					zero := g.fn.Long(0)
					v = g.sliceDescriptor(g.fn.ConstInt(ir.ClsP, 0), zero, zero)
				}
				if i < len(vs.Values) {
					v = g.assignmentValue(vs.Values[i], obj.Type())
				}
				g.store(v, slot, obj.Type())
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
			if id, ok := lhs.(*ast.Ident); ok {
				obj := g.info.Uses[id]
				if n.Tok == token.DEFINE && obj == nil {
					obj = g.info.Defs[id]
				}
				typ = obj.Type()
				var exists bool
				slot, exists = g.addr(obj)
				if !exists {
					slot = g.alloc(typ)
					g.vars[obj] = slot
				}
				_, global := g.globals[obj]
				destinationIsInline = global && isMemoryValue(typ)
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
			if destinationIsInline && isInlineAggregate(typ) {
				g.cur.Call(ir.ClsP, g.fn.Sym("memcpy", 0), slot, v, g.fn.Long(typeSize(typ)))
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
				value := g.load(g.resultSlot, g.resultType)
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
				resultType := g.info.Types[result].Type
				values[i] = g.assignmentValue(result, resultType)
			}
			for i := 1; i < len(values); i++ {
				resultType := g.extraResultTypes[i-1]
				if isMemoryValue(resultType) {
					g.cur.Call(ir.ClsP, g.fn.Sym("memcpy", 0), g.extraResultSlots[i-1], values[i], g.fn.Long(typeSize(resultType)))
				} else {
					g.store(values[i], g.extraResultSlots[i-1], resultType)
				}
			}
			resultType := g.info.Types[n.Results[0]].Type
			g.cur.Ret(g.stableReturnValue(values[0], resultType))
		}
	case *ast.IfStmt:
		g.ifStmt(n)
	case *ast.ForStmt:
		g.forStmt(n)
	case *ast.RangeStmt:
		g.rangeStmt(n)
	case *ast.SwitchStmt:
		g.switchStmt(n)
	case *ast.TypeSwitchStmt:
		g.typeSwitchStmt(n)
	case *ast.BranchStmt:
		if n.Tok == token.BREAK && len(g.breaks) > 0 {
			g.cur.Goto(g.breaks[len(g.breaks)-1])
		} else if n.Tok == token.CONTINUE && len(g.continues) > 0 {
			g.cur.Goto(g.continues[len(g.continues)-1])
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
		g.stmt(n.Stmt)
	case *ast.DeferStmt:
		literal, ok := n.Call.Fun.(*ast.FuncLit)
		if !ok || len(n.Call.Args) != 0 || literal.Type.Params.NumFields() != 0 {
			g.fail(n, "defer currently requires a parameterless function literal")
			return
		}
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
					function = concrete
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
	for _, argument := range call.Args {
		arguments = append(arguments, g.expr(argument))
	}
	if closure != ir.R {
		g.pinClosure(closure)
	}
	arguments = append(arguments, g.extraResultSlots...)

	resultType := signature.Results().At(0).Type()
	resultClass, _ := scalar(resultType)
	value := g.cur.Call(resultClass, callee, arguments...)
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

	calloc := g.fn.Sym("calloc", 0)
	closure := g.cur.Call(ir.ClsP, calloc, g.fn.Long(1), g.fn.Long(int64(8*(len(call.Args)+1))))
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
		literal, ok := deferStatement.Call.Fun.(*ast.FuncLit)
		if !ok || len(deferStatement.Call.Args) != 0 || literal.Type.Params.NumFields() != 0 {
			continue
		}
		run := g.block("deferrun")
		done := g.block("deferdone")
		active := g.load(g.deferSlots[deferStatement], types.Typ[types.Bool])
		g.cur.Jnz(active, run, done)
		g.cur = run
		g.store(g.fn.Word(0), g.deferSlots[deferStatement], types.Typ[types.Bool])
		g.stmts(literal.Body.List)
		if g.live() {
			g.cur.Goto(done)
		}
		g.cur = done
	}
}

func (g *gen) rangeStmt(statement *ast.RangeStmt) {
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
		valueSlot = g.alloc(valueType)
		g.vars[object] = valueSlot
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
	g.cur = body
	if valueSlot != ir.R {
		elementOffset := index
		if size := typeSize(valueType); size != 1 {
			elementOffset = g.cur.Mul(ir.ClsL, index, g.fn.Long(size))
		}
		address := g.cur.Add(ir.ClsP, rangeData, elementOffset)
		value := address
		if !isInlineAggregate(valueType) {
			value = g.load(address, valueType)
		}
		g.store(value, valueSlot, valueType)
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
	if _, typeParameter := t.(*types.TypeParam); typeParameter {
		return false
	}
	if isMemoryValue(t) {
		return true
	}
	switch value := t.Underlying().(type) {
	case *types.Slice, *types.Interface:
		return true
	case *types.Basic:
		return value.Kind() == types.String
	}
	return false
}

// Aggregate values are represented by an address in cg12 IR. A returned
// aggregate therefore needs storage whose lifetime extends beyond the callee's
// stack frame. Scalar returns continue to use the native result register.
func (g *gen) stableReturnValue(value ir.Ref, resultType types.Type) ir.Ref {
	if !isMemoryValue(resultType) {
		return value
	}

	size := typeSize(resultType)
	result := g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(size))
	g.cur.Call(ir.ClsP, g.fn.Sym("memcpy", 0), result, value, g.fn.Long(size))
	return result
}

func (g *gen) allocLocal(t types.Type) ir.Ref {
	switch t.Underlying().(type) {
	case *types.Array, *types.Struct:
		slot := g.cur.Alloc(8, 8)
		size := typeSize(t)
		align := 8
		if size < 8 {
			align = 4
		}
		backing := g.cur.Alloc(align, int(size))
		memset := g.fn.Sym("memset", 0)
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
		addr := g.expr(expression.X)
		offset := fieldOffset(selection)
		if offset != 0 {
			addr = g.cur.Add(ir.ClsP, addr, g.fn.Long(offset))
		}
		return addr
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
func (g *gen) forStmt(n *ast.ForStmt) {
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

func (g *gen) switchStmt(n *ast.SwitchStmt) {
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

func (g *gen) typeSwitchStmt(statement *ast.TypeSwitchStmt) {
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
				isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, interfaceValue, g.fn.ConstInt(ir.ClsP, 0))
				g.cur.Jnz(isNil, blocks[i], next)
			} else {
				nonNil := g.block("typenonnil")
				isNil := g.cur.Cmp(ir.CmpEq, ir.ClsP, interfaceValue, g.fn.ConstInt(ir.ClsP, 0))
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
				g.store(g.load(data, implicit.Type()), slot, implicit.Type())
			}
			g.vars[implicit] = slot
		}
		g.stmts(clause.Body)
		if g.live() {
			g.cur.Goto(done)
		}
	}
	g.breaks = g.breaks[:len(g.breaks)-1]
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
		slot, ok := g.addr(obj)
		if !ok {
			g.fail(n, "unknown variable %s", n.Name)
			return ir.R
		}
		if _, global := g.globals[obj]; global && isMemoryValue(obj.Type()) {
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
			value := g.cur.Alloc(4, int(size))
			g.cur.CallVoid(g.fn.Sym("runtime.chanrecv1", 0), channel, value)
			if isMemoryValue(elementType) {
				return value
			}
			return g.load(value, elementType)
		}
		if n.Op == token.AND {
			if literal, ok := n.X.(*ast.CompositeLit); ok {
				return g.compositeLiteral(literal, true)
			}
			if isMemoryValue(g.info.Types[n.X].Type) {
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
		if isMemoryValue(g.info.Types[n].Type) {
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
		switch fun := n.Fun.(type) {
		case *ast.Ident:
			obj, _ = g.info.Uses[fun].(*types.Func)
		case *ast.SelectorExpr:
			obj, _ = g.info.Uses[fun.Sel].(*types.Func)
			if selection := g.info.Selections[fun]; selection != nil && selection.Kind() == types.MethodVal {
				receiver = g.methodReceiver(fun, obj)
				if _, isInterface := selection.Recv().Underlying().(*types.Interface); isInterface {
					if target := g.methodTargets[obj.Name()]; target != nil {
						obj = target
					}
				}
			}
		}
		if obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "runtime" && obj.Name() == "systemstack" && len(n.Args) == 1 {
			if literal, ok := n.Args[0].(*ast.FuncLit); ok {
				g.stmts(literal.Body.List)
				return g.fn.Word(0)
			}
		}
		var callee ir.Ref
		var sig *types.Signature
		var closure ir.Ref
		if obj != nil {
			callee = g.fn.Sym(g.functionSymbol(obj), 0)
			sig = obj.Type().(*types.Signature)
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
		for i, a := range n.Args {
			parameterIndex := i
			if parameterIndex >= sig.Params().Len() {
				parameterIndex = sig.Params().Len() - 1
			}
			args = append(args, g.assignmentValue(a, sig.Params().At(parameterIndex).Type()))
		}
		if closure != ir.R {
			g.pinClosure(closure)
		}
		if sig.Results().Len() == 0 {
			g.cur.CallVoid(callee, args...)
			return g.fn.Word(0)
		}
		for i := 1; i < sig.Results().Len(); i++ {
			args = append(args, g.alloc(sig.Results().At(i).Type()))
		}
		return g.cur.Call(c, callee, args...)
	case *ast.IndexExpr:
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
	case *ast.SliceExpr:
		base := g.indexBase(n.X)
		low := g.fn.Long(0)
		if n.Low != nil {
			low = g.expr(n.Low)
		}
		high := ir.R
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
		size := typeSize(element)
		dataOffset := low
		if size != 1 {
			dataOffset = g.cur.Mul(ir.ClsL, low, g.fn.Long(size))
		}
		data := g.cur.Add(ir.ClsP, base, dataOffset)
		length := g.cur.Sub(ir.ClsL, high, low)
		return g.sliceDescriptor(data, length, length)
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
			g.fail(n, "unsupported selector %s", n.Sel.Name)
			return ir.R
		}
		addr := g.expr(n.X)
		offset := fieldOffset(selection)
		if offset != 0 {
			addr = g.cur.Add(ir.ClsP, addr, g.fn.Long(offset))
		}
		if isInlineAggregate(selection.Type()) {
			return addr
		}
		return g.load(addr, selection.Type())
	}
	g.fail(e, "unsupported expression %T", e)
	return ir.R
}

func (g *gen) methodReceiver(selector *ast.SelectorExpr, method *types.Func) ir.Ref {
	if method != nil {
		signature := method.Type().(*types.Signature)
		if signature.Recv() != nil {
			_, wantsPointer := signature.Recv().Type().Underlying().(*types.Pointer)
			receiverType := g.info.Types[selector.X].Type
			_, hasPointer := receiverType.Underlying().(*types.Pointer)
			if wantsPointer && !hasPointer {
				if isMemoryValue(receiverType) {
					return g.expr(selector.X)
				}
				return g.lvalue(selector.X)
			}
		}
	}
	return g.expr(selector.X)
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
	descriptor := g.cur.Alloc(8, 16)
	g.cur.Store(g.fn.Sym(wrapperName, 0), descriptor)
	g.store(g.expr(expression.X), g.offset(descriptor, 8), receiverType)
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
		alignment := int(types.SizesFor("gc", runtime.GOARCH).Alignof(elementType))
		if alignment < 4 {
			alignment = 4
		}
		backing := g.cur.Alloc(alignment, int(length*elementSize))
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
	backing := g.cur.Alloc(align, int(size))
	if heap {
		if g.runtimeAllocation {
			backing = g.cur.Call(ir.ClsP, g.fn.Sym("runtime.newobject", 0), g.runtimeType(t))
		} else {
			backing = g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(size))
		}
	}
	memset := g.fn.Sym("memset", 0)
	g.cur.Call(ir.ClsP, memset, backing, g.fn.Word(0), g.fn.Long(size))
	if isStruct {
		offsets := types.SizesFor("gc", runtime.GOARCH).Offsetsof(structFields(structure))
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
				g.cur.Call(ir.ClsP, g.fn.Sym("memcpy", 0), fieldAddress, value, g.fn.Long(typeSize(fieldType)))
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
		fset:              g.fset,
		info:              g.info,
		pkg:               g.pkg,
		mod:               g.mod,
		globals:           g.globals,
		methodTargets:     g.methodTargets,
		emitRuntimeTables: g.emitRuntimeTables,
		typeTags:          g.typeTags,
		vars:              make(map[types.Object]ir.Ref),
		labels:            make(map[string]*ir.Block),
		deferSlots:        make(map[*ast.DeferStmt]ir.Ref),
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
	child.cur = child.fn.Entry()
	for i := 0; i < signature.Params().Len(); i++ {
		parameter := signature.Params().At(i)
		class, ok := scalar(parameter.Type())
		if !ok {
			g.fail(literal, "unsupported function literal parameter %s", parameter.Type())
			return ir.R
		}
		value := child.fn.Param(parameter.Name(), class)
		slot := child.alloc(parameter.Type())
		child.store(value, slot, parameter.Type())
		child.vars[parameter] = slot
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
			class, _ := scalar(result.Type())
			child.store(child.fn.ConstInt(class, 0), pointer, result.Type())
		}
	}
	if signature.Results().Len() > 0 && signature.Results().At(0).Name() != "" {
		result := signature.Results().At(0)
		child.resultSlot = child.alloc(result.Type())
		child.resultType = result.Type()
		child.vars[result] = child.resultSlot
		class, _ := scalar(result.Type())
		child.store(child.fn.ConstInt(class, 0), child.resultSlot, result.Type())
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
	descriptor := g.cur.Alloc(8, 8*(len(captures)+1))
	g.cur.Store(g.fn.Sym(symbol, 0), descriptor)
	for i, capture := range captures {
		g.cur.Store(g.vars[capture], g.offset(descriptor, int64(8*(i+1))))
	}
	return descriptor
}

func (g *gen) functionValue(function *types.Func) ir.Ref {
	name := fmt.Sprintf(".goc.funcval.%d", len(g.mod.Data))
	g.mod.Data = append(g.mod.Data, &ir.Data{
		Name:  name,
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubL, Sym: g.functionSymbol(function)}},
	})
	return g.fn.Sym(name, 0)
}

func (g *gen) closureRegister() int {
	if runtime.GOARCH == "arm64" {
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
	descriptor := g.cur.Alloc(8, 16)
	g.cur.Store(g.fn.Sym(name, 0), descriptor)
	g.cur.Store(g.fn.Long(int64(len(bytes))), g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) indexBase(expression ast.Expr) ir.Ref {
	base := g.expr(expression)
	if _, ok := g.info.Types[expression].Type.Underlying().(*types.Slice); ok {
		return g.cur.Load(ir.ClsP, base)
	}
	if basic, ok := g.info.Types[expression].Type.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
		return g.cur.Load(ir.ClsP, base)
	}
	return base
}

func (g *gen) sliceDescriptor(data, length, capacity ir.Ref) ir.Ref {
	descriptor := g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(24))
	g.cur.Store(data, descriptor)
	g.cur.Store(length, g.offset(descriptor, 8))
	g.cur.Store(capacity, g.offset(descriptor, 16))
	return descriptor
}

func (g *gen) stringDescriptor(data, length ir.Ref) ir.Ref {
	descriptor := g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(16))
	g.cur.Store(data, descriptor)
	g.cur.Store(length, g.offset(descriptor, 8))
	return descriptor
}

func (g *gen) offset(base ir.Ref, offset int64) ir.Ref {
	if offset == 0 {
		return base
	}
	return g.cur.Add(ir.ClsP, base, g.fn.Long(offset))
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
			bytes := capacity
			if size := typeSize(sliceType.Elem()); size != 1 {
				bytes = g.cur.Mul(ir.ClsL, capacity, g.fn.Long(size))
			}
			data := g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), bytes)
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
	case "String":
		data := g.expr(call.Args[0])
		length := g.expr(call.Args[1])
		return g.stringDescriptor(data, length)
	case "Slice":
		data := g.expr(call.Args[0])
		length := g.expr(call.Args[1])
		return g.sliceDescriptor(data, length, length)
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
		if g.runtimeAllocation {
			return g.cur.Call(ir.ClsP, g.fn.Sym("runtime.newobject", 0), g.runtimeType(pointer.Elem()))
		}
		return g.cur.Call(ir.ClsP, g.fn.Sym("calloc", 0), g.fn.Long(1), g.fn.Long(typeSize(pointer.Elem())))
	case "len", "cap":
		argumentType := g.info.Types[call.Args[0]].Type
		switch t := argumentType.Underlying().(type) {
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
		memcpy := g.fn.Sym("memcpy", 0)
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
		memset := g.fn.Sym("memset", 0)
		g.cur.Call(ir.ClsP, memset, data, g.fn.Word(0), size)
		return g.fn.Word(0)
	case "delete":
		g.mapDelete(call.Args[0], call.Args[1])
		return g.fn.Word(0)
	case "append":
		destination := g.expr(call.Args[0])
		source := g.expr(call.Args[1])
		destinationData := g.cur.Load(ir.ClsP, destination)
		destinationLength := g.cur.Load(ir.ClsL, g.offset(destination, 8))
		capacity := g.cur.Load(ir.ClsL, g.offset(destination, 16))
		sourceData := g.cur.Load(ir.ClsP, source)
		sourceLength := g.cur.Load(ir.ClsL, g.offset(source, 8))
		element := g.info.Types[call].Type.Underlying().(*types.Slice).Elem()
		size := typeSize(element)
		byteOffset := destinationLength
		byteLength := sourceLength
		if size != 1 {
			byteOffset = g.cur.Mul(ir.ClsL, destinationLength, g.fn.Long(size))
			byteLength = g.cur.Mul(ir.ClsL, sourceLength, g.fn.Long(size))
		}
		writeAt := g.cur.Add(ir.ClsP, destinationData, byteOffset)
		memcpy := g.fn.Sym("memcpy", 0)
		g.cur.Call(ir.ClsP, memcpy, writeAt, sourceData, byteLength)
		length := g.cur.Add(ir.ClsL, destinationLength, sourceLength)
		return g.sliceDescriptor(destinationData, length, capacity)
	default:
		g.fail(call, "unsupported builtin %s", builtin.Name())
		return ir.R
	}
}

func (g *gen) channelType(channel *types.Chan) ir.Ref {
	element := channel.Elem()
	elementName := fmt.Sprintf(".goc.channel.element.%d", len(g.mod.Data))
	elementBytes := make([]int64, 48)
	size := typeSize(element)
	for i := 0; i < 8; i++ {
		elementBytes[i] = (size >> (8 * i)) & 0xff
	}
	alignment := types.SizesFor("gc", runtime.GOARCH).Alignof(element)
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
	alignment := types.SizesFor("gc", runtime.GOARCH).Alignof(valueType)
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

func markPointerWords(valueType types.Type, base int64, mask []int64, lastPointer *int64) {
	mark := func(offset int64) {
		word := offset / 8
		mask[word/8] |= 1 << (word % 8)
		if end := offset + 8; end > *lastPointer {
			*lastPointer = end
		}
	}
	switch value := valueType.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		mark(base)
	case *types.Slice:
		mark(base)
	case *types.Interface:
		mark(base)
		mark(base + 8)
	case *types.Array:
		elementSize := typeSize(value.Elem())
		for index := int64(0); index < value.Len(); index++ {
			markPointerWords(value.Elem(), base+index*elementSize, mask, lastPointer)
		}
	case *types.Struct:
		fields := structFields(value)
		offsets := types.SizesFor("gc", runtime.GOARCH).Offsetsof(fields)
		for index, field := range fields {
			markPointerWords(field.Type(), base+offsets[index], mask, lastPointer)
		}
	case *types.Basic:
		if value.Kind() == types.UnsafePointer || value.Kind() == types.String {
			mark(base)
		}
	}
}

func runtimeKind(valueType types.Type) int {
	switch valueType.Underlying().(type) {
	case *types.Struct:
		return 25
	case *types.Pointer:
		return 22
	}
	if basic, ok := valueType.Underlying().(*types.Basic); ok {
		switch basic.Kind() {
		case types.Bool:
			return 1
		case types.Int:
			return 2
		case types.Uint:
			return 7
		case types.Uintptr:
			return 12
		case types.String:
			return 24
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
	calloc := g.fn.Sym("calloc", 0)
	header := g.cur.Call(ir.ClsP, calloc, g.fn.Long(1), g.fn.Long(mapHeaderSize))
	keyBytes := g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Key())))
	valueBytes := g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Elem())))
	keys := g.cur.Call(ir.ClsP, calloc, g.fn.Long(1), keyBytes)
	values := g.cur.Call(ir.ClsP, calloc, g.fn.Long(1), valueBytes)
	used := g.cur.Call(ir.ClsP, calloc, g.fn.Long(1), capacity)
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
	realloc := g.fn.Sym("realloc", 0)
	keys := g.cur.Load(ir.ClsP, g.offset(mapping, mapKeysOffset))
	values := g.cur.Load(ir.ClsP, g.offset(mapping, mapValuesOffset))
	grownUsed := g.cur.Load(ir.ClsP, g.offset(mapping, mapUsedOffset))
	keys = g.cur.Call(ir.ClsP, realloc, keys, g.cur.Mul(ir.ClsL, newCapacity, g.fn.Long(typeSize(mapType.Key()))))
	values = g.cur.Call(ir.ClsP, realloc, values, g.cur.Mul(ir.ClsL, newCapacity, g.fn.Long(typeSize(mapType.Elem()))))
	grownUsed = g.cur.Call(ir.ClsP, realloc, grownUsed, newCapacity)
	memset := g.fn.Sym("memset", 0)
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
	memset := g.fn.Sym("memset", 0)
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
	memset := g.fn.Sym("memset", 0)
	g.cur.Call(ir.ClsP, memset, keys, g.fn.Word(0), g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Key()))))
	g.cur.Call(ir.ClsP, memset, values, g.fn.Word(0), g.cur.Mul(ir.ClsL, capacity, g.fn.Long(typeSize(mapType.Elem()))))
	g.cur.Call(ir.ClsP, memset, used, g.fn.Word(0), capacity)
	g.cur.Store(g.fn.Long(0), mapping)
	g.cur.Goto(done)
	g.cur = done
}

func (g *gen) builtinPrint(call *ast.CallExpr, newline bool) {
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
	sizes := types.SizesFor("gc", runtime.GOARCH)
	return sizes.Sizeof(t)
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
	return functionSymbol(function)
}

func (g *gen) binary(op token.Token, x, y ir.Ref, t types.Type, n ast.Node) ir.Ref {
	return g.coerce(g.binaryRaw(op, x, y, t, n), t)
}

func (g *gen) binaryRaw(op token.Token, x, y ir.Ref, t types.Type, n ast.Node) ir.Ref {
	c, _ := scalar(t)
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
		return g.cur.Shl(c, x, y)
	case token.SHR:
		if signed(t) {
			return g.cur.Sar(c, x, y)
		}
		return g.cur.Shr(c, x, y)
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

// OutputName returns the conventional output stem for a source file.
func OutputName(name string) string {
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	return filepath.Base(stem)
}
