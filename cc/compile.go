// Package cc is a small C compiler front end that targets cg12. It parses and
// type-checks C with modernc.org/cc/v4, then walks the type-checked AST and
// emits cg12 IR, which any cg12 backend can turn into machine code. It supports a
// practical subset of C — enough to write real programs that link against libc.
package cc

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// gen holds the state of translating one translation unit to a cg12 module.
type gen struct {
	mod    *ir.Module
	fn     *ir.Func
	curRet cc.Type
	cur    *ir.Block
	scopes []map[string]lval
	strs   map[string]string // decoded string literal -> data symbol name
	names  map[string]int    // per-function temp-name uniquifier
	nblk   int               // block-name counter
	brk    []*ir.Block       // break targets
	cont   []*ir.Block       // continue targets
	err    error
}

// lval is a named lvalue: the address of its storage and its C type.
type lval struct {
	addr ir.Ref
	typ  cc.Type
}

// Compile parses C source and returns the equivalent cg12 module.
func Compile(name, src string) (*ir.Module, error) {
	cfg, err := cc.NewConfig("linux", "arm64")
	if err != nil {
		return nil, fmt.Errorf("cc config: %w", err)
	}
	ast, err := cc.Translate(cfg, []cc.Source{
		{Name: "<predefined>", Value: cfg.Predefined},
		{Name: "<builtin>", Value: cc.Builtin},
		{Name: name, Value: src},
	})
	if err != nil {
		return nil, fmt.Errorf("cc parse: %w", err)
	}

	g := &gen{mod: ir.NewModule(), strs: map[string]string{}}
	for tu := ast.TranslationUnit; tu != nil; tu = tu.TranslationUnit {
		ed := tu.ExternalDeclaration
		if ed == nil || ed.Position().Filename != name {
			continue // skip predefined/builtin declarations
		}
		switch ed.Case {
		case cc.ExternalDeclarationFuncDef:
			g.genFunc(ed.FunctionDefinition)
		case cc.ExternalDeclarationDecl:
			g.genGlobalDecl(ed.Declaration)
		}
		if g.err != nil {
			return nil, g.err
		}
	}
	return g.mod, nil
}

func (g *gen) fail(format string, a ...any) ir.Ref {
	if g.err == nil {
		g.err = fmt.Errorf(format, a...)
	}
	return ir.R
}

// --- type mapping ----------------------------------------------------------

// clsOf maps a C type to the cg12 value class used to compute with it. Pointers
// use the abstract pointer class ClsP, which each backend resolves to its native
// pointer width with LowerPointers (ClsL on 64-bit targets, ClsW on wasm32).
func clsOf(t cc.Type) ir.Cls {
	switch t.Kind() {
	case cc.Float:
		return ir.ClsS
	case cc.Double, cc.LongDouble:
		return ir.ClsD
	case cc.Ptr, cc.Function, cc.Array:
		return ir.ClsP
	case cc.Long, cc.ULong, cc.LongLong, cc.ULongLong:
		return ir.ClsL
	default: // Bool, Char, SChar, UChar, Short, UShort, Int, UInt, Enum
		return ir.ClsW
	}
}

// wide reports whether a class is a 64-bit-capable integer/pointer class (a long
// or a pointer), for which pointer-width constants and no-op width changes apply.
func wide(c ir.Cls) bool { return c == ir.ClsL || c == ir.ClsP }

// signed reports whether arithmetic/comparison on t is signed. char is unsigned
// on this target (aarch64).
func signed(t cc.Type) bool {
	switch t.Kind() {
	case cc.Bool, cc.Char, cc.UChar, cc.UShort, cc.UInt, cc.ULong, cc.ULongLong, cc.Ptr:
		return false
	default:
		return true
	}
}

func isFloat(t cc.Type) bool {
	k := t.Kind()
	return k == cc.Float || k == cc.Double || k == cc.LongDouble
}

// --- scopes ----------------------------------------------------------------

func (g *gen) push() { g.scopes = append(g.scopes, map[string]lval{}) }
func (g *gen) pop()  { g.scopes = g.scopes[:len(g.scopes)-1] }

func (g *gen) define(name string, v lval) { g.scopes[len(g.scopes)-1][name] = v }

func (g *gen) lookup(name string) (lval, bool) {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		if v, ok := g.scopes[i][name]; ok {
			return v, true
		}
	}
	return lval{}, false
}

func (g *gen) block(prefix string) *ir.Block {
	g.nblk++
	return g.fn.NewBlock(fmt.Sprintf("%s%d", prefix, g.nblk))
}

// setName names a temporary after the C construct it came from, keeping names
// unique within the function so the printed IR reads like the source (the first
// use of a base name gets it verbatim, later ones get a ".N" suffix).
func (g *gen) setName(ref ir.Ref, base string) {
	if ref.Kind != ir.RefTemp || base == "" {
		return
	}
	n := g.names[base]
	g.names[base]++
	name := base
	if n > 0 {
		name = fmt.Sprintf("%s.%d", base, n)
	}
	g.fn.Temp(ref).Name = name
}

// terminated reports whether the current block already has a terminator.
func (g *gen) terminated() bool { return g.cur.Jmp.Kind != ir.JmpNone }

// --- memory helpers --------------------------------------------------------

// loadVal loads a value of type t from addr, sign/zero-extending narrow types.
func (g *gen) loadVal(addr ir.Ref, t cc.Type) ir.Ref {
	cls := clsOf(t)
	switch t.Size() {
	case 1:
		if signed(t) {
			return g.cur.LoadSub(cls, ir.SubB, addr)
		}
		return g.cur.LoadSub(cls, ir.SubUB, addr)
	case 2:
		if signed(t) {
			return g.cur.LoadSub(cls, ir.SubH, addr)
		}
		return g.cur.LoadSub(cls, ir.SubUH, addr)
	default:
		return g.cur.Load(cls, addr)
	}
}

// storeVal stores val (already of type t's class) to addr at t's width.
func (g *gen) storeVal(addr, val ir.Ref, t cc.Type) {
	switch t.Size() {
	case 1:
		g.cur.StoreSub(ir.SubB, val, addr)
	case 2:
		g.cur.StoreSub(ir.SubH, val, addr)
	default:
		g.cur.Store(val, addr)
	}
}

// --- functions -------------------------------------------------------------

func (g *gen) genFunc(fd *cc.FunctionDefinition) {
	d := fd.Declarator
	ft, ok := d.Type().(*cc.FunctionType)
	if !ok {
		g.fail("cc: %s is not a function", d.Name())
		return
	}
	ret := ft.Result()
	g.curRet = ret
	if ret.Kind() == cc.Void {
		g.fn = g.mod.NewFuncVoid(d.Name())
	} else {
		g.fn = g.mod.NewFunc(d.Name(), clsOf(ret))
	}
	g.fn.Export()
	g.fn.Variadic = ft.IsVariadic()
	g.cur = g.fn.Entry()
	g.nblk = 0
	g.names = map[string]int{}
	g.push()
	defer g.pop()

	// Each parameter is copied into a stack slot so it is an addressable lvalue.
	for _, p := range ft.Parameters() {
		if p.Name() == "" {
			continue
		}
		pt := p.Type()
		pref := g.fn.Param(p.Name(), clsOf(pt))
		g.names[p.Name()] = 1 // the incoming parameter value already holds this name
		addr := g.cur.Alloc(align(pt), int(pt.Size()))
		g.setName(addr, p.Name()+".addr")
		g.storeVal(addr, pref, pt)
		g.define(p.Name(), lval{addr, pt})
	}

	g.genCompound(fd.CompoundStatement)

	// Fall-through: supply a default return.
	if !g.terminated() {
		if ret.Kind() == cc.Void {
			g.cur.RetVoid()
		} else {
			g.cur.Ret(g.zero(ret))
		}
	}

	// Guard: every block must be terminated. An unterminated block is a codegen
	// bug; catching it here fails cleanly instead of handing malformed IR to the
	// backend (which is free to loop or exhaust memory on it).
	for _, b := range g.fn.Blocks {
		if b.Jmp.Kind == ir.JmpNone {
			g.fail("cc: internal error: block %q in %s is not terminated", b.Name, d.Name())
			return
		}
	}
}

// align returns a stack-allocation alignment for type t, rounded up to one of
// cg12's supported alloc alignments (4, 8, 16).
func align(t cc.Type) int {
	a := t.Align()
	switch {
	case a <= 4:
		return 4
	case a <= 8:
		return 8
	default:
		return 16
	}
}

// zero returns a zero constant of the given type's class.
func (g *gen) zero(t cc.Type) ir.Ref {
	switch cls := clsOf(t); {
	case cls == ir.ClsS || cls == ir.ClsD:
		return g.fn.Double(0)
	case wide(cls):
		return g.fn.Long(0)
	default:
		return g.fn.Word(0)
	}
}

// genGlobalDecl handles a file-scope declaration (currently: prototypes and
// simple scalar globals with constant initializers).
func (g *gen) genGlobalDecl(d *cc.Declaration) {
	if d.Case != cc.DeclarationDecl {
		return
	}
	for l := d.InitDeclaratorList; l != nil; l = l.InitDeclaratorList {
		id := l.InitDeclarator
		dcl := id.Declarator
		if dcl == nil || dcl.IsSynthetic() {
			continue
		}
		t := dcl.Type()
		if t.Kind() == cc.Function {
			continue // a prototype needs no storage
		}
		data := &ir.Data{Name: dcl.Name(), Align: align(t)}
		if id.Case == cc.InitDeclaratorInit {
			if v, ok := constInt(id.Initializer.AssignmentExpression); ok {
				data.Items = []ir.DataItem{{Sub: subFor(int(t.Size())), Ints: []int64{v}}}
			}
		}
		if len(data.Items) == 0 {
			data.Items = []ir.DataItem{{Zero: int(t.Size())}}
		}
		g.mod.Data = append(g.mod.Data, data)
		g.define(dcl.Name(), lval{g.fn.Sym(dcl.Name(), 0), t}) // reachable via symbol
	}
}

func subFor(size int) ir.SubCls {
	switch size {
	case 1:
		return ir.SubB
	case 2:
		return ir.SubH
	case 4:
		return ir.SubW
	default:
		return ir.SubL
	}
}

// constInt returns the constant integer value of e, if it is one.
func constInt(e cc.ExpressionNode) (int64, bool) {
	if e == nil {
		return 0, false
	}
	if v, ok := e.Value().(cc.Int64Value); ok {
		return int64(v), true
	}
	if v, ok := e.Value().(cc.UInt64Value); ok {
		return int64(v), true
	}
	return 0, false
}
