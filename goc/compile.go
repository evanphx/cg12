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
	}
	loader := newSourceLoader(fset)
	conf := types.Config{Importer: loader}
	pkg, err := conf.Check(file.Name.Name, fset, []*ast.File{file}, info)
	if err != nil {
		return nil, err
	}
	mod := ir.NewModule()
	g := &gen{fset: fset, file: file, info: info, pkg: pkg, mod: mod, globals: map[types.Object]string{}}
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
		generator := &gen{fset: fset, info: unit.info, pkg: unit.pkg, mod: mod, globals: globals}
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
	functions := reachableFunctions(roots, info, pkg, loader.units)
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
				fset:          fset,
				info:          function.info,
				pkg:           function.pkg,
				mod:           mod,
				globals:       packageGlobals[function.pkg.Path()],
				methodTargets: methodTargets,
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
			cls, ok := scalar(obj.Type())
			if !ok {
				continue
			}
			name := g.pkg.Name() + "." + id.Name
			d := &ir.Data{Name: name, Align: 8, Linkage: ir.Linkage{Export: ast.IsExported(id.Name)}}
			var v int64
			if i < len(vs.Values) {
				tv := g.info.Types[vs.Values[i]]
				if tv.Value == nil {
					continue
				}
				v = constInt(tv.Value)
			}
			if cls == ir.ClsL {
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

func (g *gen) globalArray(id *ast.Ident, object types.Object, array *types.Array, spec *ast.ValueSpec, valueIndex int) {
	name := g.pkg.Path() + "." + id.Name
	element := array.Elem()
	sub, ok := subOf(element)
	if !ok {
		cls, scalarOK := scalar(element)
		if !scalarOK || cls == ir.ClsP {
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
			g.fail(spec.Values[valueIndex], "global array initializer must be a composite literal")
			return
		}
		for i, expression := range literal.Elts {
			value := g.info.Types[expression].Value
			if value == nil {
				return
			}
			values[i] = constInt(value)
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
	switch t.Underlying().(type) {
	case *types.Array, *types.Slice, *types.Struct, *types.Interface, *types.Pointer:
		return ir.ClsP, true
	}
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return 0, false
	}
	switch b.Kind() {
	case types.Bool, types.Int8, types.Uint8, types.Int16, types.Uint16, types.Int32, types.Uint32, types.UntypedBool, types.UntypedInt, types.UntypedRune:
		return ir.ClsW, true
	case types.Int, types.Uint, types.Int64, types.Uint64, types.Uintptr:
		return ir.ClsL, true
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
	if c == ir.ClsL || c == ir.ClsP {
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

func (g *gen) funcDecl(fd *ast.FuncDecl) {
	obj := g.info.Defs[fd.Name].(*types.Func)
	sig := obj.Type().(*types.Signature)
	isMain := g.pkg.Name() == "main" && obj.Name() == "main"
	name := functionSymbol(obj)
	if isMain {
		name = "main"
	}
	if isMain {
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
	if ast.IsExported(fd.Name.Name) || isMain {
		g.fn.Export()
	}
	g.vars = map[types.Object]ir.Ref{}
	g.seq = 0
	g.cur = g.fn.Entry()
	g.at(fd)
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
		if isMain {
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
		if g.err != nil || !g.live() {
			return
		}
		g.stmt(s)
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
			vs := sp.(*ast.ValueSpec)
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
					v = g.expr(vs.Values[i])
				}
				g.store(v, slot, obj.Type())
			}
		}
	case *ast.AssignStmt:
		vals := make([]ir.Ref, len(n.Rhs))
		for i, e := range n.Rhs {
			vals[i] = g.expr(e)
		}
		for i, lhs := range n.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
				continue
			}
			var slot ir.Ref
			var typ types.Type
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
			} else {
				typ = g.info.Types[lhs].Type
				slot = g.lvalue(lhs)
			}
			v := vals[i]
			if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
				old := g.load(slot, typ)
				v = g.binary(n.Tok-token.ADD_ASSIGN+token.ADD, old, v, typ, n)
			}
			g.store(g.coerce(v, typ), slot, typ)
		}
	case *ast.IncDecStmt:
		id, ok := n.X.(*ast.Ident)
		if !ok {
			g.fail(n, "unsupported increment target")
			return
		}
		obj := g.info.Uses[id]
		slot, ok := g.addr(obj)
		if !ok {
			g.fail(id, "unknown variable %s", id.Name)
			return
		}
		c, _ := scalar(obj.Type())
		v := g.load(slot, obj.Type())
		one := g.fn.ConstInt(c, 1)
		if n.Tok == token.INC {
			v = g.cur.Add(c, v, one)
		} else {
			v = g.cur.Sub(c, v, one)
		}
		g.store(g.coerce(v, obj.Type()), slot, obj.Type())
	case *ast.ExprStmt:
		g.expr(n.X)
	case *ast.ReturnStmt:
		if len(n.Results) == 0 {
			if g.fn.HasRet && g.resultSlot != ir.R {
				g.cur.Ret(g.load(g.resultSlot, g.resultType))
			} else {
				g.cur.RetVoid()
			}
		} else {
			g.cur.Ret(g.expr(n.Results[0]))
		}
	case *ast.IfStmt:
		g.ifStmt(n)
	case *ast.ForStmt:
		g.forStmt(n)
	case *ast.RangeStmt:
		g.rangeStmt(n)
	case *ast.SwitchStmt:
		g.switchStmt(n)
	case *ast.BranchStmt:
		if n.Tok == token.BREAK && len(g.breaks) > 0 {
			g.cur.Goto(g.breaks[len(g.breaks)-1])
		} else if n.Tok == token.CONTINUE && len(g.continues) > 0 {
			g.cur.Goto(g.continues[len(g.continues)-1])
		} else {
			g.fail(n, "unsupported branch %s", n.Tok)
		}
	case *ast.EmptyStmt:
	default:
		g.fail(n, "unsupported statement %T", n)
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
	var slice ir.Ref
	if _, ok := rangeType.Underlying().(*types.Slice); ok {
		slice = g.expr(statement.X)
		upper = g.cur.Load(ir.ClsL, g.offset(slice, 8))
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
		data := g.cur.Load(ir.ClsP, slice)
		elementOffset := index
		if size := typeSize(valueType); size != 1 {
			elementOffset = g.cur.Mul(ir.ClsL, index, g.fn.Long(size))
		}
		address := g.cur.Add(ir.ClsP, data, elementOffset)
		g.store(g.load(address, valueType), valueSlot, valueType)
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
	if g.live() {
		g.cur.Goto(done)
	}
	g.cur = no
	if n.Else != nil {
		g.stmt(n.Else)
	}
	if g.live() {
		g.cur.Goto(done)
	}
	g.cur = done
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

func (g *gen) expr(e ast.Expr) ir.Ref {
	g.at(e)
	tv := g.info.Types[e]
	c, _ := scalar(tv.Type)
	if tv.Value != nil {
		return g.fn.ConstInt(c, constInt(tv.Value))
	}
	switch n := e.(type) {
	case *ast.ParenExpr:
		return g.expr(n.X)
	case *ast.Ident:
		obj := g.info.Uses[n]
		if obj == nil {
			obj = g.info.Defs[n]
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
		if n.Op == token.AND {
			if _, ok := n.X.(*ast.CompositeLit); ok {
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
		return g.compositeLiteral(n)
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
		var obj *types.Func
		var receiver ir.Ref
		switch fun := n.Fun.(type) {
		case *ast.Ident:
			obj, _ = g.info.Uses[fun].(*types.Func)
		case *ast.SelectorExpr:
			obj, _ = g.info.Uses[fun.Sel].(*types.Func)
			if selection := g.info.Selections[fun]; selection != nil && selection.Kind() == types.MethodVal {
				receiver = g.expr(fun.X)
				if target := g.methodTargets[obj.Name()]; target != nil {
					obj = target
				}
			}
		}
		if obj == nil {
			g.fail(n, "only direct function calls are supported")
			return ir.R
		}
		name := functionSymbol(obj)
		args := make([]ir.Ref, 0, len(n.Args)+1)
		if receiver != ir.R {
			args = append(args, receiver)
		}
		for _, a := range n.Args {
			args = append(args, g.expr(a))
		}
		callee := g.fn.Sym(name, 0)
		sig := obj.Type().(*types.Signature)
		if sig.Results().Len() == 0 {
			g.cur.CallVoid(callee, args...)
			return g.fn.Word(0)
		}
		return g.cur.Call(c, callee, args...)
	case *ast.IndexExpr:
		base := g.indexBase(n.X)
		idx := g.expr(n.Index)
		element := g.info.Types[n].Type
		size := typeSize(element)
		if size != 1 {
			idx = g.cur.Mul(ir.ClsL, idx, g.fn.Long(size))
		}
		addr := g.cur.Add(ir.ClsP, base, idx)
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
		} else {
			descriptor := g.expr(n.X)
			high = g.cur.Load(ir.ClsL, g.offset(descriptor, 8))
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
		if selection == nil || selection.Kind() != types.FieldVal {
			g.fail(n, "unsupported selector %s", n.Sel.Name)
			return ir.R
		}
		addr := g.expr(n.X)
		offset := fieldOffset(selection)
		if offset != 0 {
			addr = g.cur.Add(ir.ClsP, addr, g.fn.Long(offset))
		}
		if _, ok := selection.Type().Underlying().(*types.Array); ok {
			return addr
		}
		return g.load(addr, selection.Type())
	}
	g.fail(e, "unsupported expression %T", e)
	return ir.R
}

func (g *gen) compositeLiteral(literal *ast.CompositeLit) ir.Ref {
	t := g.info.Types[literal].Type
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
			g.store(g.expr(expression), g.offset(backing, offsets[fieldIndex]), fieldType)
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

func (g *gen) stringSlice(expression ast.Expr) ir.Ref {
	value := g.info.Types[expression].Value
	if value == nil || value.Kind() != constant.String {
		g.fail(expression, "non-constant string conversion is not supported")
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

func (g *gen) indexBase(expression ast.Expr) ir.Ref {
	base := g.expr(expression)
	if _, ok := g.info.Types[expression].Type.Underlying().(*types.Slice); ok {
		return g.cur.Load(ir.ClsP, base)
	}
	return base
}

func (g *gen) sliceDescriptor(data, length, capacity ir.Ref) ir.Ref {
	descriptor := g.cur.Alloc(8, 24)
	g.cur.Store(data, descriptor)
	g.cur.Store(length, g.offset(descriptor, 8))
	g.cur.Store(capacity, g.offset(descriptor, 16))
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
	case "new":
		pointer := g.info.Types[call].Type.(*types.Pointer)
		size := typeSize(pointer.Elem())
		calloc := g.fn.Sym("calloc", 0)
		return g.cur.Call(ir.ClsP, calloc, g.fn.Long(1), g.fn.Long(size))
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
		default:
			g.fail(call, "unsupported %s operand %s", builtin.Name(), argumentType)
			return ir.R
		}
	case "panic":
		abort := g.fn.Sym("abort", 0)
		g.cur.CallVoid(abort)
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
		name = typeName + "." + name
	}
	if function.Pkg() == nil {
		return name
	}
	return function.Pkg().Path() + "." + name
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
