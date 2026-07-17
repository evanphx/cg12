// Package cc is a small C compiler front end that targets cg12. It parses and
// type-checks C with modernc.org/cc/v4, then walks the type-checked AST and
// emits cg12 IR, which any cg12 backend can turn into machine code. It supports a
// practical subset of C — enough to write real programs that link against libc.
package cc

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// gen holds the state of translating one translation unit to a cg12 module.
type gen struct {
	mod      *ir.Module
	fn       *ir.Func
	curRet   cc.Type
	cur      *ir.Block
	scopes   []map[string]lval
	strs     map[string]string                  // decoded string literal -> data symbol name
	ldconsts map[string]string                  // quad constant bytes -> data symbol name
	quad     *ir.AggType                        // the canonical long-double (quad) aggregate type
	ti128    *ir.AggType                        // the canonical __int128 {lo,hi} aggregate type
	cplx     map[ir.SubCls]*ir.AggType          // memoized _Complex {re,im} aggregate types
	aggs     map[cc.Type]*ir.AggType            // memoized C struct/union -> cg12 aggregate type
	names    map[string]int                     // per-function temp-name uniquifier
	labels   map[string]*ir.Block               // goto label -> block (per function)
	caseBlk  map[*cc.LabeledStatement]*ir.Block // switch case/default -> block
	nblk     int                                // block-name counter
	nstatic  int                                // static-local mangling counter
	brk      []*ir.Block                        // break targets
	brkVla   []int                              // vlaScope depth at each break target
	cont     []*ir.Block                        // continue targets
	contVla  []int                              // vlaScope depth at each continue target
	vlaScope []ir.Ref                           // saved stack pointers of active VLA scopes
	err      error
}

// lval is a named lvalue: its storage and its C type. A local or parameter is
// held in a stack slot (addr); a global lives at a symbol (sym) resolved lazily
// per function, since a symbol ref is interned in the referencing function.
type lval struct {
	addr ir.Ref
	sym  string
	typ  cc.Type
	// vaRef marks a va_list that is a parameter: its storage holds a pointer to
	// the actual __va_list state (which lives in the caller), rather than being
	// the state itself as a local va_list is.
	vaRef bool
	// thread marks a symbol as thread-local: its address is per-thread, reached
	// through the TLS ABI rather than being a link-time constant.
	thread bool
}

// vaStorage returns the address of a va_list's __va_list state. A local va_list
// is that state, so its own address is used; a va_list parameter holds a pointer
// to state elsewhere, so the pointer is loaded.
func (g *gen) vaStorage(v lval) ir.Ref {
	a := g.addrOf(v)
	if v.vaRef {
		return g.cur.Load(ir.ClsP, a)
	}
	return a
}

// addrOf returns the address of an lvalue, resolving a global symbol in the
// current function (symbol refs are interned per function, so a global cannot
// carry a prebuilt ref across function boundaries).
func (g *gen) addrOf(v lval) ir.Ref {
	if v.sym != "" {
		if v.thread {
			return g.fn.ThreadSym(v.sym)
		}
		return g.fn.Sym(v.sym, 0)
	}
	return v.addr
}

// headerCompat makes the host's glibc system headers parseable by
// modernc.org/cc/v4, which does not model two GCC extensions glibc leans on:
// the AArch64 SIMD/SVE vector math declarations (gated on __GNUC__ >= 9) and the
// ISO/IEC 60559 _FloatN extended floating types. Pretending to be an older GCC
// without the _FloatN types skips both; the affected declarations are library
// vector intrinsics that ordinary C never references.
const headerCompat = `
#undef __GNUC__
#define __GNUC__ 8
#define __HAVE_FLOAT128 0
#define __HAVE_DISTINCT_FLOAT128 0
#define __HAVE_FLOAT64X 0
#define __HAVE_FLOAT32 0
#define __HAVE_FLOAT64 0
#define __HAVE_FLOAT32X 0
#define __HAVE_FLOAT16 0
`

// Target names the machine C is being compiled for. It decides the semantics
// the standard leaves to the implementation -- the width of a long, and whether
// a plain char is signed, which AArch64 and x86-64 answer differently.
type Target string

const (
	TargetARM64 Target = "arm64"
	TargetAMD64 Target = "amd64"
)

// Compile parses C source for the default target and returns the equivalent
// cg12 module.
func Compile(name, src string) (*ir.Module, error) {
	return CompileFor(TargetARM64, name, src)
}

// CompileFor parses C source for a named target.
//
// The target is not decoration. `char c = 200; c < 0` is false on AArch64, where
// a plain char is unsigned, and true on x86-64, where it is signed -- so the
// same source compiled for the wrong one is a program that computes something
// else. cg12's amd64 backend has its own tests that feed this C, and every one
// of them was getting AArch64's answers.
func CompileFor(target Target, name, src string) (*ir.Module, error) {
	cfg, err := cc.NewConfig("linux", string(target))
	if err != nil {
		return nil, fmt.Errorf("cc config for %s: %w", target, err)
	}
	ast, err := cc.Translate(cfg, []cc.Source{
		{Name: "<predefined>", Value: cfg.Predefined + headerCompat},
		{Name: "<builtin>", Value: cc.Builtin},
		{Name: name, Value: src},
	})
	if err != nil {
		return nil, fmt.Errorf("cc parse: %w", err)
	}

	g := &gen{mod: ir.NewModule(), strs: map[string]string{}, ldconsts: map[string]string{}, cplx: map[ir.SubCls]*ir.AggType{}, aggs: map[cc.Type]*ir.AggType{}}
	g.push() // file scope: holds globals, visible to every function
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
	if cc.IsComplexType(t) {
		return ir.ClsP // a complex value is the address of its {re,im} storage
	}
	if isInt128(t) {
		return ir.ClsP // a 128-bit integer is the address of its {lo,hi} storage
	}
	switch t.Kind() {
	case cc.Float:
		return ir.ClsS
	case cc.Double:
		return ir.ClsD
	case cc.LongDouble:
		return ir.ClsQ // 128-bit quad (handled entirely via memory + soft-float calls)
	case cc.Ptr, cc.Function, cc.Array, cc.Struct, cc.Union:
		return ir.ClsP // an aggregate value is handled as a pointer to its storage
	case cc.Long, cc.ULong, cc.LongLong, cc.ULongLong:
		return ir.ClsL
	default: // Bool, Char, SChar, UChar, Short, UShort, Int, UInt, Enum
		return ir.ClsW
	}
}

// wide reports whether a class is a 64-bit-capable integer/pointer class (a long
// or a pointer), for which pointer-width constants and no-op width changes apply.
func wide(c ir.Cls) bool { return c == ir.ClsL || c == ir.ClsP }

// signed reports whether arithmetic/comparison on t is signed.
//
// For an integer this asks the type checker, which knows the target's answer.
// A plain char is the case that matters: unsigned on AArch64, signed on x86-64,
// and a list here would have to name one of them. An address compares unsigned;
// everything else -- the floating types -- compares signed, where the question
// arises at all.
func signed(t cc.Type) bool {
	if t.Kind() == cc.Ptr {
		return false
	}
	if cc.IsIntegerType(t) {
		return cc.IsSignedInteger(t)
	}
	return true
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

// at stamps the current block's emit position from an AST node's source
// location, so instructions emitted next carry a line and column for debug info
// (DWARF on native backends, BTF.ext line_info on eBPF).
func (g *gen) at(n cc.Node) {
	if n == nil || g.cur == nil {
		return
	}
	p := n.Position()
	if p.Line == 0 {
		return
	}
	g.cur.At(ir.SrcPos{File: g.mod.File(p.Filename), Line: uint32(p.Line), Col: uint32(p.Column)})
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

// rvalue reads the value of an lvalue at addr. An array decays to its own
// address, and a by-value aggregate is likewise represented by its address (it
// is too big for a register); everything else loads.
func (g *gen) rvalue(addr ir.Ref, t cc.Type) ir.Ref {
	if isArray(t) || isMemValue(t) || isVaList(t) {
		return addr // an array, aggregate, long double, or va_list is its address
	}
	return g.loadVal(addr, t)
}

// isMemValue reports whether a value of type t is represented by the address of
// its storage rather than loaded into a register: a struct/union or a long
// double (a 128-bit quad), both of which are passed and copied through memory.
func isMemValue(t cc.Type) bool {
	return isAggType(t) || isLongDouble(t) || cc.IsComplexType(t) || isInt128(t)
}

// loadVal loads a value of type t from addr, sign/zero-extending narrow types.
func (g *gen) loadVal(addr ir.Ref, t cc.Type) ir.Ref {
	cls := clsOf(t)
	var r ir.Ref
	switch t.Size() {
	case 1:
		if signed(t) {
			r = g.cur.LoadSub(cls, ir.SubB, addr)
		} else {
			r = g.cur.LoadSub(cls, ir.SubUB, addr)
		}
	case 2:
		if signed(t) {
			r = g.cur.LoadSub(cls, ir.SubH, addr)
		} else {
			r = g.cur.LoadSub(cls, ir.SubUH, addr)
		}
	default:
		r = g.cur.Load(cls, addr)
	}
	g.markVolatile(t)
	return r
}

// markVolatile flags the instruction just emitted as an observable access when
// the type it accesses is volatile. Reading a volatile object is an event, not
// just a way to obtain its value, so the optimizer must not fold two reads
// together or drop one whose value goes unused.
func (g *gen) markVolatile(t cc.Type) {
	if a := t.Attributes(); a != nil && a.IsVolatile() {
		if n := len(g.cur.Instrs); n > 0 {
			g.cur.Instrs[n-1].Volatile = true
		}
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
	g.markVolatile(t)
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
	switch {
	case ret.Kind() == cc.Void:
		g.fn = g.mod.NewFuncVoid(d.Name())
	case isMemValue(ret):
		// Returning a struct/union or long double by value: the function yields a
		// pointer the backend copies into the caller's buffer (or registers).
		g.fn = g.mod.NewFuncVoid(d.Name())
		g.fn.HasRet = true
		g.fn.RetAgg = g.aggTypeOf(ret)
	default:
		g.fn = g.mod.NewFunc(d.Name(), clsOf(ret))
	}
	if d.Linkage() != cc.Internal { // a `static` function keeps internal linkage
		g.fn.Export()
	}
	g.fn.Linkage.Section = sectionOf(ft) // eBPF attach section (xdp, kprobe/..., ...)
	g.fn.Variadic = ft.IsVariadic()
	g.cur = g.fn.Entry()
	g.nblk = 0
	g.names = map[string]int{}
	g.labels = map[string]*ir.Block{}
	g.caseBlk = map[*cc.LabeledStatement]*ir.Block{}
	g.collectLabels(fd.CompoundStatement)
	g.push()
	defer g.pop()

	// Each parameter is copied into a stack slot so it is an addressable lvalue.
	for _, p := range ft.Parameters() {
		if p.Name() == "" {
			continue
		}
		pt := p.Type()
		g.names[p.Name()] = 1 // the incoming parameter value already holds this name
		if isMemValue(pt) {
			// A by-value aggregate or long double parameter arrives as a pointer to a
			// slot the backend reconstructs; use that address as the lvalue directly.
			pref := g.aggParam(p.Name(), g.aggTypeOf(pt))
			g.setName(pref, p.Name())
			g.define(p.Name(), lval{addr: pref, typ: pt})
			continue
		}
		pref := g.fn.Param(p.Name(), clsOf(pt))
		addr := g.allocAligned(pt, int(pt.Size()))
		g.setName(addr, p.Name()+".addr")
		g.storeVal(addr, pref, pt)
		// A va_list parameter's slot holds a pointer to the caller's __va_list
		// state; mark it so va_arg loads the pointer rather than treating the slot
		// as the state itself.
		g.define(p.Name(), lval{addr: addr, typ: pt, vaRef: isVaList(pt)})
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

// maxAllocAlign is the strictest alignment a single alloc op can ask for: the
// IR names the alignment in the opcode (alloc4/alloc8/alloc16), so there is no
// op for more. A local wanting more is over-allocated and its address rounded up
// instead — see allocAligned.
const maxAllocAlign = 16

// align returns the alloc op's alignment for type t: one of 4, 8, 16.
//
// This is what the op can promise, not what the type asked for. A type wanting
// more than 16 does not get it from here, and a caller placing such a type must
// go through allocAligned; align alone would silently under-align it, which is
// what `char buf[64] __attribute__((aligned(64)))` used to get.
func align(t cc.Type) int {
	a := t.Align()
	switch {
	case a <= 4:
		return 4
	case a <= 8:
		return 8
	default:
		return maxAllocAlign
	}
}

// dataAlign returns the alignment for a global's ir.Data. Unlike a stack slot,
// this one is a number the object layer places the datum by, so it can be any
// power of two the source asked for.
func dataAlign(t cc.Type) int {
	if a := t.Align(); a > 0 {
		return a
	}
	return 1
}

// allocAligned reserves stack storage for a value of type t and returns its
// address, honouring an alignment stricter than any alloc op can express.
//
// The stack pointer is only 16-aligned, so a slot at a fixed offset from it
// cannot be 64-aligned however the frame is laid out: the alignment has to be
// established at run time. This over-allocates by align-1 and rounds the address
// up, which costs two instructions and no frame realignment. gcc realigns the
// frame instead and gets a cheaper address; both give the program what it asked
// for, which is what the alignment is for.
func (g *gen) allocAligned(t cc.Type, size int) ir.Ref {
	a := t.Align()
	if a <= maxAllocAlign {
		return g.cur.Alloc(align(t), size)
	}
	raw := g.cur.Alloc(maxAllocAlign, size+a-1)
	up := g.cur.Add(ir.ClsL, raw, g.fn.Long(int64(a-1)))
	return g.cur.And(ir.ClsL, up, g.fn.Long(int64(^(a - 1))))
}

// zero returns a zero constant of the given type's class.
func (g *gen) zero(t cc.Type) ir.Ref {
	if isLongDouble(t) {
		return g.quadZero()
	}
	switch cls := clsOf(t); {
	case cls == ir.ClsS || cls == ir.ClsD:
		return g.floatOf(0, t)
	case wide(cls):
		return g.fn.Long(0)
	default:
		return g.fn.Word(0)
	}
}

// floatOf builds a floating-point constant of t's class: a single for float, a
// double for double. Using the wrong width both stores the wrong number of bytes
// and mismatches the class of neighbouring float operations.
func (g *gen) floatOf(v float64, t cc.Type) ir.Ref {
	if clsOf(t) == ir.ClsS {
		return g.fn.Single(v)
	}
	return g.fn.Double(v)
}

// genGlobalDecl handles a file-scope declaration: prototypes and globals with
// constant scalar, aggregate, or string initializers.
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
		thread := dcl.IsThreadLocal()

		// `extern int x;` says x is defined in another translation unit: it needs a
		// name to reference, not storage. Defining it here would give this unit a
		// private zero-filled x that quietly shadows the real one. `extern int x =
		// 1;` is a definition despite the keyword, hence the initializer test.
		if dcl.IsExtern() && !dcl.HasInitializer() {
			g.define(dcl.Name(), lval{sym: dcl.Name(), typ: t, thread: thread})
			continue
		}

		sec := sectionOf(t)
		if sec == "" {
			// Classify by const-ness so a backend can place it in read-only (.rodata)
			// or writable (.data) storage, as eBPF requires.
			sec = ".data"
			if a := t.Attributes(); a != nil && a.IsConst() {
				sec = ".rodata"
			}
		}
		data := &ir.Data{Name: dcl.Name(), Align: dataAlign(t), Linkage: ir.Linkage{
			Section: sec,
			// External linkage is what makes a definition visible to the linker;
			// `static` is internal and stays private to this unit.
			Export: dcl.Linkage() == cc.External,
			// A thread-local's bytes are the image each thread's block starts from,
			// not data used in place.
			Thread: thread,
		}}
		if id.Case == cc.InitDeclaratorInit {
			data.Items = g.globalItems(t, id.Initializer)
		}
		if len(data.Items) == 0 {
			data.Items = []ir.DataItem{{Zero: int(t.Size())}}
		}
		g.mod.Data = append(g.mod.Data, data)
		g.define(dcl.Name(), lval{sym: dcl.Name(), typ: t, thread: thread}) // reachable via symbol
	}
}

// globalItems lays out a constant initializer as an ordered list of data items,
// filling gaps between initialized fields (and the trailing remainder) with
// zero. Each leaf initializer carries its absolute byte offset within the
// aggregate, so nested braces and designated initializers need no special care.
func (g *gen) globalItems(t cc.Type, init *cc.Initializer) []ir.DataItem {
	type leaf struct {
		off, sz int
		item    ir.DataItem
	}
	var leaves []leaf
	add := func(off int, et cc.Type, e cc.ExpressionNode) {
		sz := int(et.Size())
		switch {
		case isArray(et):
			if s, ok := e.Value().(cc.StringValue); ok {
				b := append([]byte(s), 0)
				if len(b) > sz {
					b = b[:sz]
				}
				leaves = append(leaves, leaf{off, len(b), ir.DataItem{Str: string(b)}})
			}
		case isPointer(et):
			if s, ok := e.Value().(cc.StringValue); ok {
				leaves = append(leaves, leaf{off, sz, ir.DataItem{Sub: subFor(sz), Sym: g.internStr(string(s))}})
			} else if sym, symOff, ok := g.constAddr(e); ok {
				leaves = append(leaves, leaf{off, sz, ir.DataItem{Sub: subFor(sz), Sym: sym, Off: symOff}})
			} else if v, ok := constInt(e); ok {
				leaves = append(leaves, leaf{off, sz, ir.DataItem{Sub: subFor(sz), Ints: []int64{v}}})
			}
		case isLongDouble(et):
			if ldv, ok := e.Value().(*cc.LongDoubleValue); ok {
				leaves = append(leaves, leaf{off, 16, ir.DataItem{Str: string(quadBytes((*big.Float)(ldv)))}})
			}
		case isComplex(et):
			if cv, ok := constComplex(e); ok {
				cs := compSize(complexCls(et))
				leaves = append(leaves, leaf{off, cs, ir.DataItem{Sub: subFor(cs), Flts: []float64{real(cv)}}})
				leaves = append(leaves, leaf{off + cs, cs, ir.DataItem{Sub: subFor(cs), Flts: []float64{imag(cv)}}})
			}
		case isInt128(et):
			if bi, ok := constInt128(e); ok {
				lo, hi := split128(bi)
				leaves = append(leaves, leaf{off, 8, ir.DataItem{Sub: ir.SubL, Ints: []int64{lo}}})
				leaves = append(leaves, leaf{off + 8, 8, ir.DataItem{Sub: ir.SubL, Ints: []int64{hi}}})
			}
		case isFloat(et):
			// The initializer is converted to the declared type, so an integer
			// literal is as valid here as a floating one: `double d = 1;`.
			v, ok := constFloat(e)
			if !ok {
				g.fail("initializer for a floating type is not a constant")
				return
			}
			leaves = append(leaves, leaf{off, sz, ir.DataItem{Sub: subFor(sz), Flts: []float64{v}}})
		default:
			// Likewise the other way: `int i = 1.9;` converts, truncating toward zero.
			v, ok := constIntFrom(e)
			if !ok {
				g.fail("initializer for an integer type is not a constant")
				return
			}
			leaves = append(leaves, leaf{off, sz, ir.DataItem{Sub: subFor(sz), Ints: []int64{v}}})
		}
	}
	// Bit fields sharing an access unit are OR-ed together into one integer image
	// rather than emitted as overlapping items.
	bitUnits := map[int]*struct {
		bytes int
		val   uint64
	}{}
	var walk func(il *cc.InitializerList)
	walk = func(il *cc.InitializerList) {
		for ; il != nil; il = il.InitializerList {
			in := il.Initializer
			switch {
			case in.Case == cc.InitializerInitList:
				walk(in.InitializerList)
			case in.Field() != nil && in.Field().IsBitfield():
				f := in.Field()
				if v, ok := constInt(in.AssignmentExpression); ok {
					off := int(in.Offset())
					u := bitUnits[off]
					if u == nil {
						u = &struct {
							bytes int
							val   uint64
						}{bytes: int(f.AccessBytes())}
						bitUnits[off] = u
					}
					u.val |= (uint64(v) << f.OffsetBits()) & f.Mask()
				}
			default:
				add(int(in.Offset()), in.Type(), in.AssignmentExpression)
			}
		}
	}
	if init.Case == cc.InitializerInitList {
		walk(init.InitializerList)
	} else {
		add(0, t, init.AssignmentExpression)
	}
	for off, u := range bitUnits {
		leaves = append(leaves, leaf{off, u.bytes, ir.DataItem{Sub: subFor(u.bytes), Ints: []int64{int64(u.val)}}})
	}
	sort.SliceStable(leaves, func(i, j int) bool { return leaves[i].off < leaves[j].off })

	var items []ir.DataItem
	pos := 0
	for _, lf := range leaves {
		if lf.off > pos {
			items = append(items, ir.DataItem{Zero: lf.off - pos})
		}
		items = append(items, lf.item)
		pos = lf.off + lf.sz
	}
	if size := int(t.Size()); pos < size {
		items = append(items, ir.DataItem{Zero: size - pos})
	}
	return items
}

func isArray(t cc.Type) bool   { _, ok := t.(*cc.ArrayType); return ok }
func isPointer(t cc.Type) bool { _, ok := t.(*cc.PointerType); return ok }

// sectionOf returns the __attribute__((section("..."))) name attached to t, with
// its trailing NUL trimmed, or "" if none. It carries eBPF placement — a map
// ("maps"), a program's attach type ("xdp", "kprobe/..."), the license, etc. —
// through to the backend as ir.Linkage.Section.
func sectionOf(t cc.Type) string {
	a := t.Attributes()
	if a == nil {
		return ""
	}
	for _, v := range a.AttrValue("section") {
		if s, ok := v.(cc.StringValue); ok {
			return strings.TrimRight(string(s), "\x00")
		}
	}
	return ""
}

// constAddr evaluates a constant address expression — the address of a global
// object, optionally displaced by array indexing or member selection — to a
// symbol name and byte offset, for a global pointer initializer that a compiler
// resolves to a relocation rather than code.
// isFuncDesignator reports whether t is a function type or a pointer to one — as
// a bare function name has after its function-to-pointer decay.
func isFuncDesignator(t cc.Type) bool {
	if t.Kind() == cc.Function {
		return true
	}
	if pt, ok := t.(*cc.PointerType); ok {
		return pt.Elem().Kind() == cc.Function
	}
	return false
}

// blockSym assigns b a unique object-symbol name (once) and returns it, so a
// static initializer that takes b's address (&&label) can reference the block
// via a relocation the backend resolves to its code address.
func (g *gen) blockSym(b *ir.Block) string {
	if b.Sym == "" {
		b.Sym = fmt.Sprintf("%s.blk.%s", g.fn.Name, b.Name)
	}
	return b.Sym
}

func (g *gen) constAddr(e cc.ExpressionNode) (string, int64, bool) {
	switch n := e.(type) {
	case *cc.PrimaryExpression:
		switch n.Case {
		case cc.PrimaryExpressionIdent: // a global object (an array here decays to its address)
			name := n.Token.SrcStr()
			if v, ok := g.lookup(name); ok && v.sym != "" {
				return v.sym, 0, true
			}
			// A function name isn't in the variable scope; used as a value it decays
			// to a pointer to the function, so the initializer references its symbol.
			if isFuncDesignator(n.Type()) {
				return name, 0, true
			}
		case cc.PrimaryExpressionString: // a string literal decays to its data symbol
			if s, ok := n.Value().(cc.StringValue); ok {
				return g.internStr(string(s)), 0, true
			}
		case cc.PrimaryExpressionExpr:
			return g.constAddr(n.ExpressionList)
		}
	case *cc.CastExpression:
		return g.constAddr(n.CastExpression)
	case *cc.UnaryExpression:
		switch n.Case {
		case cc.UnaryExpressionAddrof: // &lvalue
			return g.constAddr(n.CastExpression)
		case cc.UnaryExpressionLabelAddr: // &&label as a static initializer
			if b, ok := g.labels[n.Token2.SrcStr()]; ok {
				return g.blockSym(b), 0, true
			}
		}
	case *cc.PostfixExpression:
		switch n.Case {
		case cc.PostfixExpressionIndex: // base[i]
			if sym, off, ok := g.constAddr(n.PostfixExpression); ok {
				if idx, ok := constInt(n.ExpressionList); ok {
					return sym, off + idx*elemSize(n.PostfixExpression.Type()), true
				}
			}
		case cc.PostfixExpressionSelect: // base.field
			if sym, off, ok := g.constAddr(n.PostfixExpression); ok {
				return sym, off + n.Field().Offset(), true
			}
		}
	}
	return "", 0, false
}

// elemSize returns the element size of an array or pointer type.
func elemSize(t cc.Type) int64 {
	switch x := t.(type) {
	case *cc.ArrayType:
		return int64(x.Elem().Size())
	case *cc.PointerType:
		return int64(x.Elem().Size())
	}
	return 1
}

// isPtrOrArray reports whether t participates in pointer arithmetic as the
// pointer operand: a pointer, or an array that decays to one.
func isPtrOrArray(t cc.Type) bool {
	switch t.(type) {
	case *cc.PointerType, *cc.ArrayType:
		return true
	}
	return false
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
// constFloat is the value of a constant expression as a float, whatever form the
// literal took: an initializer is converted to the type it initializes, so an
// integer literal is a perfectly good initializer for a double.
func constFloat(e cc.ExpressionNode) (float64, bool) {
	if e == nil {
		return 0, false
	}
	switch v := e.Value().(type) {
	case cc.Float64Value:
		return float64(v), true
	case cc.Int64Value:
		return float64(v), true
	case cc.UInt64Value:
		return float64(v), true
	}
	return 0, false
}

// constIntFrom is constInt, but it also converts a floating literal, as C does
// when one initializes an integer: the fractional part is discarded.
func constIntFrom(e cc.ExpressionNode) (int64, bool) {
	if v, ok := constInt(e); ok {
		return v, true
	}
	if e != nil {
		if v, ok := e.Value().(cc.Float64Value); ok {
			return int64(float64(v)), true // C truncates toward zero
		}
	}
	return 0, false
}

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
