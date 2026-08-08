package cc

import (
	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

// Atomics. The IR carries atomic operations (the atomic.* intrinsics with a real
// load-acquire / store-release lowering), so the explicit builtin families lower
// to them (see builtin.go): the C11-era __atomic_* ones -- load/store/exchange/
// compare_exchange and __atomic_fetch_* -- and the older __sync_* ones below.
// The memory-order argument is ignored: the intrinsics use acquire/release, which
// is at least as strong as any order a program can ask for, so honoring the
// weaker orders only would be an optimization, never a correctness gap.
//
// Neither family has a nand; builtin.go says so rather than emitting a call.
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

// atomicBadWidth reports an atomic builtin applied to an object cg12 has no
// atomic operation for. The machine has them at 1, 2, 4 and 8 bytes; anything
// else -- a struct, a 16-byte integer -- would have to be done under a lock,
// which cg12 does not emit. This is a different failure from an _Atomic object,
// so it gets its own explanation rather than atomicUnsupported's.
func (g *gen) atomicBadWidth(what string, ptr moderncc.ExpressionNode) {
	g.fail("cc: %s on a %d-byte object is not supported: cg12 has atomic operations "+
		"at 1, 2, 4 and 8 bytes only", what, pointee(ptr.Type()).Size())
}

// atomicWidth returns the atomic-intrinsic name suffix, result class, and access
// width in bytes for an atomic reached through ptr (a pointer to the atomic
// object). ok is false for a width cg12 has no atomic op for.
func atomicWidth(ptr moderncc.ExpressionNode) (suffix string, cls ir.Cls, width int, ok bool) {
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

// atomicOperand evaluates an atomic builtin's value argument converted to the
// type of the object being accessed, which is the type C says the operation is
// performed in. Without the conversion a narrower argument reaches a wider
// atomic unextended: __atomic_fetch_add(&somelong, -1) added 0xffffffff rather
// than subtracting one, taking 1000 to 4294968295.
func (g *gen) atomicOperand(ptr, val moderncc.ExpressionNode) ir.Ref {
	return g.convert(g.genExpr(val), val.Type(), pointee(ptr.Type()))
}

// atomicResult re-narrows an atomic's result to the accessed object's type.
//
// Two things make it necessary below a 4-byte object. The machine's sub-word
// atomics zero-extend what they load (ldarb, ldaxrh), so a signed char or short
// atomic otherwise hands back the raw bits rather than the negative number they
// stand for -- __atomic_load_n of a signed char holding -3 returned 253. And the
// fetch-and-op forms compute the new value in the wider class, so the result can
// carry out of the object's own width even though the store did not.
func (g *gen) atomicResult(ptr moderncc.ExpressionNode, v ir.Ref) ir.Ref {
	elem := pointee(ptr.Type())
	if elem.Size() >= 4 {
		return v
	}
	return g.storedValue(v, elem)
}

// atomicRaw reduces a value to the bits a sub-word atomic actually compares.
// The exclusive loads zero-extend, so a compare-and-swap expecting a signed char
// of -3 has to hand the intrinsic 0x000000fd; the sign-extended 0xfffffffd that
// the C conversion produces would never match what the loop loads.
func (g *gen) atomicRaw(ptr moderncc.ExpressionNode, v ir.Ref) ir.Ref {
	width := int(pointee(ptr.Type()).Size())
	if width >= 4 {
		return v
	}
	return g.cur.And(ir.ClsW, v, g.fn.ConstInt(ir.ClsW, int64(1)<<(width*8)-1))
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
func (g *gen) atomicFetchOp(op string, args []moderncc.ExpressionNode, newValue bool) ir.Ref {
	suf, cls, _, ok := atomicWidth(args[0])
	if !ok {
		g.atomicBadWidth("atomic "+op, args[0])
		return ir.R
	}
	ptr := g.genExpr(args[0])
	val := g.atomicOperand(args[0], args[1])
	old := g.cur.Intrinsic("atomic."+op+"."+suf, cls, ptr, val)
	if !newValue {
		return g.atomicResult(args[0], old)
	}
	var updated ir.Ref
	switch op {
	case "add":
		updated = g.cur.Add(cls, old, val)
	case "sub":
		updated = g.cur.Sub(cls, old, val)
	case "and":
		updated = g.cur.And(cls, old, val)
	case "or":
		updated = g.cur.Or(cls, old, val)
	case "xor":
		updated = g.cur.Xor(cls, old, val)
	default:
		updated = old
	}
	return g.atomicResult(args[0], updated)
}

func (g *gen) atomicLoad(ptr moderncc.ExpressionNode) ir.Ref {
	suf, cls, _, ok := atomicWidth(ptr)
	if !ok {
		g.atomicBadWidth("atomic load", ptr)
		return ir.R
	}
	return g.atomicResult(ptr, g.cur.Intrinsic("atomic.load."+suf, cls, g.genExpr(ptr)))
}

func (g *gen) atomicStore(ptr, val moderncc.ExpressionNode) {
	suf, _, _, ok := atomicWidth(ptr)
	if !ok {
		g.atomicBadWidth("atomic store", ptr)
		return
	}
	p := g.genExpr(ptr)
	g.cur.IntrinsicVoid("atomic.store."+suf, p, g.atomicOperand(ptr, val))
}

func (g *gen) atomicXchg(ptr, val moderncc.ExpressionNode) ir.Ref {
	suf, cls, _, ok := atomicWidth(ptr)
	if !ok {
		g.atomicBadWidth("atomic exchange", ptr)
		return ir.R
	}
	p := g.genExpr(ptr)
	return g.atomicResult(ptr, g.cur.Intrinsic("atomic.xchg."+suf, cls, p, g.atomicOperand(ptr, val)))
}

// atomicCAS lowers a compare-exchange whose expected value is at *expPtr and whose
// desired value is by value. It returns whether the swap happened, and updates
// *expPtr to the value seen (a no-op on success).
func (g *gen) atomicCAS(ptr, expPtr, desired moderncc.ExpressionNode) ir.Ref {
	suf, cls, width, ok := atomicWidth(ptr)
	if !ok {
		g.atomicBadWidth("atomic compare-exchange", ptr)
		return ir.R
	}
	p := g.genExpr(ptr)
	ep := g.genExpr(expPtr)
	exp := g.loadWidth(width, ep)
	des := g.atomicOperand(ptr, desired)
	old := g.cur.Intrinsic("atomic.cas."+suf, cls, p, exp, des)
	g.storeWidth(width, old, ep)
	return g.cur.Cmp(ir.CmpEq, ir.ClsW, old, exp)
}

// The legacy __sync_* family predates the C11 memory model and is what portable
// C reaches for when it cannot assume a compiler new enough for __atomic_*.
// GCC documents every one of them as a full barrier, which is exactly what the
// atomic.* intrinsics provide, and lets a caller append a list of "protected
// variables" that it then ignores -- so the extra arguments are ignored here too.
//
// Two shapes differ from the __atomic_* forms and need their own lowering: the
// compare-and-swaps take the expected value by value rather than by pointer, and
// the release takes no value at all.

// syncCAS lowers __sync_val_compare_and_swap (which yields the value that was in
// *ptr) and __sync_bool_compare_and_swap (which yields whether the swap
// happened). Both are the same instruction; only what they hand back differs.
func (g *gen) syncCAS(args []moderncc.ExpressionNode, boolResult bool) ir.Ref {
	suf, cls, _, ok := atomicWidth(args[0])
	if !ok {
		g.atomicBadWidth("__sync compare-and-swap", args[0])
		return ir.R
	}
	p := g.genExpr(args[0])
	// The comparison happens in the raw bits the exclusive load produces, so the
	// expected value is reduced to them; the result is converted back out.
	exp := g.atomicRaw(args[0], g.atomicOperand(args[0], args[1]))
	des := g.atomicOperand(args[0], args[2])
	old := g.cur.Intrinsic("atomic.cas."+suf, cls, p, exp, des)
	if boolResult {
		return g.cur.Cmp(ir.CmpEq, ir.ClsW, old, exp)
	}
	return g.atomicResult(args[0], old)
}

// syncRelease lowers __sync_lock_release, which writes zero to the lock word.
// GCC calls it a release barrier; the store-release the intrinsic emits is that.
func (g *gen) syncRelease(ptr moderncc.ExpressionNode) {
	suf, cls, _, ok := atomicWidth(ptr)
	if !ok {
		g.atomicBadWidth("__sync_lock_release", ptr)
		return
	}
	g.cur.IntrinsicVoid("atomic.store."+suf, g.genExpr(ptr), g.fn.ConstInt(cls, 0))
}

// checkAtomicDecl refuses an _Atomic object. C says every access to it is atomic;
// cg12 would give it ordinary ones.
func (g *gen) checkAtomicDecl(d *moderncc.Declarator) {
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
func (g *gen) checkAtomicMember(f *moderncc.Field) {
	if f == nil {
		return
	}
	if d := f.Declarator(); d != nil && d.IsAtomic() {
		g.atomicUnsupported("the _Atomic member " + f.Name())
	}
}
