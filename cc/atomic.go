package cc

import (
	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// Atomics. The IR now carries atomic operations (the atomic.* intrinsics with a
// real load-acquire / store-release lowering), so the explicit builtin families
// -- __atomic_load/store/exchange/compare_exchange and __atomic_fetch_* -- lower
// to them (see builtin.go). The memory-order argument is ignored: the intrinsics
// use acquire/release, which is at least as strong as any order a program can ask
// for, so honoring the weaker orders only would be an optimization, never a
// correctness gap.
//
// What is still refused is an _Atomic-qualified OBJECT, where the language makes
// *every* access atomic. Supporting that means routing each ordinary load, store,
// and read-modify-write of the object through the intrinsics, which the general
// expression path does not yet do; letting it through would compile `_Atomic int
// g; g++;` to an ordinary non-atomic read-modify-write -- code that looks right,
// passes a single-threaded test, and races under contention. An error is the
// honest answer until the access paths are taught the qualifier.

// atomicUnsupported is the shared explanation for the cases still refused.
func (g *gen) atomicUnsupported(what string) {
	g.fail("cc: %s is not supported: cg12 lowers the explicit __atomic_* builtins "+
		"but does not yet make every access to an _Atomic-qualified object atomic", what)
}

// atomicWidth returns the atomic-intrinsic name suffix, result class, and access
// width in bytes for an atomic reached through ptr (a pointer to the atomic
// object). ok is false for a width cg12 has no atomic op for.
func atomicWidth(ptr cc.ExpressionNode) (suffix string, cls ir.Cls, width int, ok bool) {
	switch pointee(ptr.Type()).Size() {
	case 1:
		return "b", ir.ClsW, 1, true
	case 2:
		return "h", ir.ClsW, 2, true
	case 4:
		return "w", ir.ClsW, 4, true
	case 8:
		return "l", ir.ClsL, 8, true
	}
	return "", 0, 0, false
}

// loadWidth reads a width-byte value (zero-extended) from addr; storeWidth writes
// the low width bytes of val to addr. They move the value the generic atomic
// forms pass by pointer.
func (g *gen) loadWidth(width int, addr ir.Ref) ir.Ref {
	switch width {
	case 1:
		return g.cur.LoadSub(ir.ClsW, ir.SubUB, addr)
	case 2:
		return g.cur.LoadSub(ir.ClsW, ir.SubUH, addr)
	case 4:
		return g.cur.Load(ir.ClsW, addr)
	default:
		return g.cur.Load(ir.ClsL, addr)
	}
}

func (g *gen) storeWidth(width int, val, addr ir.Ref) {
	switch width {
	case 1:
		g.cur.StoreSub(ir.SubB, val, addr)
	case 2:
		g.cur.StoreSub(ir.SubH, val, addr)
	default:
		g.cur.Store(val, addr) // val's class fixes the 4- vs 8-byte width
	}
}

// atomicFetchOp lowers a fetch-and-op (add/sub/and/or/xor). The intrinsic returns
// the previous value; newValue instead asks for the value after the op.
func (g *gen) atomicFetchOp(op string, args []cc.ExpressionNode, newValue bool) ir.Ref {
	suf, cls, _, ok := atomicWidth(args[0])
	if !ok {
		g.atomicUnsupported("atomic " + op)
		return ir.R
	}
	ptr := g.genExpr(args[0])
	val := g.genExpr(args[1])
	old := g.cur.Intrinsic("atomic."+op+"."+suf, cls, ptr, val)
	if !newValue {
		return old
	}
	switch op {
	case "add":
		return g.cur.Add(cls, old, val)
	case "sub":
		return g.cur.Sub(cls, old, val)
	case "and":
		return g.cur.And(cls, old, val)
	case "or":
		return g.cur.Or(cls, old, val)
	case "xor":
		return g.cur.Xor(cls, old, val)
	}
	return old
}

func (g *gen) atomicLoad(ptr cc.ExpressionNode) ir.Ref {
	suf, cls, _, ok := atomicWidth(ptr)
	if !ok {
		g.atomicUnsupported("atomic load")
		return ir.R
	}
	return g.cur.Intrinsic("atomic.load."+suf, cls, g.genExpr(ptr))
}

func (g *gen) atomicStore(ptr, val cc.ExpressionNode) {
	suf, _, _, ok := atomicWidth(ptr)
	if !ok {
		g.atomicUnsupported("atomic store")
		return
	}
	p := g.genExpr(ptr)
	g.cur.IntrinsicVoid("atomic.store."+suf, p, g.genExpr(val))
}

func (g *gen) atomicXchg(ptr, val cc.ExpressionNode) ir.Ref {
	suf, cls, _, ok := atomicWidth(ptr)
	if !ok {
		g.atomicUnsupported("atomic exchange")
		return ir.R
	}
	p := g.genExpr(ptr)
	return g.cur.Intrinsic("atomic.xchg."+suf, cls, p, g.genExpr(val))
}

// atomicCAS lowers a compare-exchange whose expected value is at *expPtr and whose
// desired value is by value. It returns whether the swap happened, and updates
// *expPtr to the value seen (a no-op on success).
func (g *gen) atomicCAS(ptr, expPtr, desired cc.ExpressionNode) ir.Ref {
	suf, cls, width, ok := atomicWidth(ptr)
	if !ok {
		g.atomicUnsupported("atomic compare-exchange")
		return ir.R
	}
	p := g.genExpr(ptr)
	ep := g.genExpr(expPtr)
	exp := g.loadWidth(width, ep)
	des := g.genExpr(desired)
	old := g.cur.Intrinsic("atomic.cas."+suf, cls, p, exp, des)
	g.storeWidth(width, old, ep)
	return g.cur.Cmp(ir.CmpEq, ir.ClsW, old, exp)
}

// checkAtomicDecl refuses an _Atomic object. C says every access to it is atomic;
// cg12 would give it ordinary ones.
func (g *gen) checkAtomicDecl(d *cc.Declarator) {
	if d != nil && d.IsAtomic() {
		g.atomicUnsupported("_Atomic " + d.Name())
	}
}

// checkAtomicMember refuses an access to an _Atomic struct or union member: the
// same object with the same promise, reached through a field rather than a name.
//
// It asks about the member being touched rather than about the whole aggregate.
// A struct may hold an atomic member beside ordinary ones, and reading an
// ordinary one is an ordinary load -- refusing that would refuse programs cg12
// compiles correctly.
func (g *gen) checkAtomicMember(f *cc.Field) {
	if f == nil {
		return
	}
	if d := f.Declarator(); d != nil && d.IsAtomic() {
		g.atomicUnsupported("the _Atomic member " + f.Name())
	}
}
