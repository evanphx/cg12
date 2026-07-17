package opt

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// Memory-region classes for a resolved pointer base.
const (
	cLocal   = iota // a stack allocation that never escapes this function
	cEscaped        // a stack allocation whose address escapes
	cGlobal         // the address of a named global symbol
	cUnknown        // a parameter or otherwise opaque pointer
)

// aliasInfo answers must/may-alias questions about pointers in a function. It is
// deliberately conservative: it only ever reports "no alias" when it can prove
// two accesses touch disjoint memory.
type aliasInfo struct {
	f         *ir.Func
	def       map[uint32]ir.Instr // temp -> defining instruction
	allocBase map[uint32]uint32   // alloc-derived pointer temp -> its alloc temp id
	escaped   map[uint32]bool     // alloc ids whose address escapes
}

// locInfo is a resolved memory location: a base region plus a (possibly known)
// byte offset and an access width.
type locInfo struct {
	key      string // identity of the base region
	class    int
	offset   int64
	offKnown bool
	width    int
}

func newAliasInfo(f *ir.Func) *aliasInfo {
	ai := &aliasInfo{
		f:         f,
		def:       map[uint32]ir.Instr{},
		allocBase: map[uint32]uint32{},
		escaped:   map[uint32]bool{},
	}
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			if in.To.Kind == ir.RefTemp {
				ai.def[in.To.ID] = in
			}
		}
	}
	ai.buildAllocBase()
	ai.computeEscape()
	return ai
}

// buildAllocBase maps every pointer that is an alloc, or a constant-offset
// derivative of one, to the underlying alloc's temp id.
func (ai *aliasInfo) buildAllocBase() {
	for id, in := range ai.def {
		if in.Op.IsAlloc() {
			ai.allocBase[id] = id
		}
	}
	for changed := true; changed; {
		changed = false
		for id, in := range ai.def {
			if _, done := ai.allocBase[id]; done {
				continue
			}
			if in.Op != ir.OAdd && in.Op != ir.OSub {
				continue
			}
			x, y := in.Arg(0), in.Arg(1)
			if base, ok := ai.derivedBase(x, y); ok {
				ai.allocBase[id] = base
				changed = true
			} else if in.Op == ir.OAdd {
				if base, ok := ai.derivedBase(y, x); ok {
					ai.allocBase[id] = base
					changed = true
				}
			}
		}
	}
}

// derivedBase reports the alloc base of ptr when off is a constant (so ptr+off
// stays within the same allocation with a known displacement).
func (ai *aliasInfo) derivedBase(ptr, off ir.Ref) (uint32, bool) {
	if ptr.Kind != ir.RefTemp {
		return 0, false
	}
	if _, ok := constInt(ai.f, off); !ok {
		return 0, false
	}
	base, ok := ai.allocBase[ptr.ID]
	return base, ok
}

// computeEscape marks an allocation as escaped whenever a pointer derived from
// it is used as anything other than a load/store address or a tracked
// constant-offset derivation.
func (ai *aliasInfo) computeEscape() {
	mark := func(r ir.Ref) {
		if r.Kind == ir.RefTemp {
			if base, ok := ai.allocBase[r.ID]; ok {
				ai.escaped[base] = true
			}
		}
	}
	for _, b := range ai.f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				mark(a) // a pointer flowing through a phi escapes
			}
		}
		for _, in := range b.Instrs {
			switch {
			case in.Op.IsLoad():
				// address operand is fine; nothing to mark
			case in.Op.IsStore():
				mark(in.Arg(0)) // storing the pointer value escapes it
			case in.Op.IsAlloc():
				// its own result is the allocation
			case in.Op.IsLifetime():
				// a lifetime marker names the allocation to bound its live region, not
				// to leak its address, so it does not escape its operand
			case benignMemoryCall(ai.f, in):
				// Memory helpers observe local storage but do not retain it.
			case isAtomicPointerStore(ai.f, in):
				// The destination is observed only during the store. The value being
				// stored can still make a local allocation reachable externally.
				mark(in.Arg(2))
			case (in.Op == ir.OAdd || in.Op == ir.OSub) && ai.tracked(in.To):
				// constant-offset derivation: operands stay local
			case in.Op == ir.OIntrinsic:
				// An intrinsic escapes its pointer operands only if its registry
				// entry says so (or it is unregistered, treated conservatively).
				if e := ir.LookupIntrinsic(in.Intrin.Name); e == nil || e.EscapesArgs {
					for _, a := range in.Args {
						mark(a)
					}
				}
			default:
				for _, a := range in.Args {
					mark(a)
				}
			}
		}
		mark(b.Jmp.Arg) // returning a pointer escapes it
		for _, argument := range b.Jmp.Args {
			mark(argument)
		}
	}
}

func (ai *aliasInfo) tracked(r ir.Ref) bool {
	if r.Kind != ir.RefTemp {
		return false
	}
	_, ok := ai.allocBase[r.ID]
	return ok
}

// locOf resolves a pointer reference to a memory location of the given width,
// stripping constant offsets from pointer arithmetic.
func (ai *aliasInfo) locOf(addr ir.Ref, width int) locInfo {
	var offset int64
	cur := addr
	for cur.Kind == ir.RefTemp {
		in, ok := ai.def[cur.ID]
		if !ok || (in.Op != ir.OAdd && in.Op != ir.OSub) {
			break
		}
		if c, ok := constInt(ai.f, in.Arg(1)); ok {
			if in.Op == ir.OAdd {
				offset += c
			} else {
				offset -= c
			}
			cur = in.Arg(0)
			continue
		}
		if c, ok := constInt(ai.f, in.Arg(0)); ok && in.Op == ir.OAdd {
			offset += c
			cur = in.Arg(1)
			continue
		}
		break
	}
	key, class := ai.baseKind(cur)
	if cur.Kind == ir.RefConst {
		if c := ai.f.Consts[cur.ID]; c.Kind == ir.ConstSym {
			offset += c.Int
		}
	}
	return locInfo{key: key, class: class, offset: offset, offKnown: true, width: width}
}

func (ai *aliasInfo) baseKind(r ir.Ref) (string, int) {
	switch r.Kind {
	case ir.RefConst:
		c := ai.f.Consts[r.ID]
		if c.Kind == ir.ConstSym {
			return "g:" + c.Sym, cGlobal
		}
		return fmt.Sprintf("i:%d", c.Int), cUnknown
	case ir.RefTemp:
		if base, ok := ai.allocBase[r.ID]; ok {
			if ai.escaped[base] {
				return fmt.Sprintf("e:%d", base), cEscaped
			}
			return fmt.Sprintf("a:%d", base), cLocal
		}
		return fmt.Sprintf("t:%d", r.ID), cUnknown
	}
	return "?", cUnknown
}

// mustAlias reports that two accesses touch exactly the same bytes.
func mustAlias(a, b locInfo) bool {
	return a.key != "?" && a.key == b.key && a.offKnown && b.offKnown &&
		a.offset == b.offset && a.width == b.width
}

// mayAlias reports that two accesses might touch overlapping bytes.
func mayAlias(a, b locInfo) bool {
	if a.key == b.key {
		if a.offKnown && b.offKnown {
			return overlap(a.offset, a.width, b.offset, b.width)
		}
		return true
	}
	return !distinct(a, b)
}

// distinct proves two different base regions never overlap.
func distinct(a, b locInfo) bool {
	if a.class == cLocal || b.class == cLocal {
		return true // a non-escaped local is unreachable through any other base
	}
	aAlloc := a.class == cEscaped
	bAlloc := b.class == cEscaped
	switch {
	case aAlloc && bAlloc:
		return true // two different allocations
	case aAlloc && b.class == cGlobal, a.class == cGlobal && bAlloc:
		return true // an allocation is never a global
	case a.class == cGlobal && b.class == cGlobal:
		return true // two different symbols
	}
	return false // an unknown pointer might reach the other region
}

func overlap(o1 int64, w1 int, o2 int64, w2 int) bool {
	return o1 < o2+int64(w2) && o2 < o1+int64(w1)
}
