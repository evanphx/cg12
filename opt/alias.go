package opt

import "github.com/evanphx/cg12/ir"

// Memory-region classes for a resolved pointer base.
const (
	cLocal   = iota // a stack allocation that never escapes this function
	cEscaped        // a stack allocation whose address escapes
	cGlobal         // the address of a named global symbol
	cUnknown        // a parameter or otherwise opaque pointer
)

const (
	keyInvalid = iota
	keyGlobal
	keyConst
	keyTemp
	keyLocal
	keyEscaped
)

// aliasInfo answers must/may-alias questions about pointers in a function. It is
// deliberately conservative: it only ever reports "no alias" when it can prove
// two accesses touch disjoint memory.
type aliasInfo struct {
	f             *ir.Func
	def           []*ir.Instr // temp -> defining instruction
	allocBasePlus []uint32    // alloc-derived pointer temp -> alloc temp id + 1; 0 means none
	escaped       []bool      // alloc ids whose address escapes
}

// locInfo is a resolved memory location: a base region plus a (possibly known)
// byte offset and an access width.
type locInfo struct {
	keyKind  int
	keyID    uint64
	keySym   string
	class    int
	offset   int64
	offKnown bool
	width    int
}

type locKey struct {
	kind int
	id   uint64
	sym  string
}

func (loc locInfo) key() locKey {
	return locKey{kind: loc.keyKind, id: loc.keyID, sym: loc.keySym}
}

// allocTemp reports the temporary that allocated the storage this key names,
// when the key names one of this function's own allocations. keyLocal and
// keyEscaped both resolve to an alloc base; the others do not name storage this
// function made.
func (key locKey) allocTemp() (uint32, bool) {
	if key.kind != keyLocal && key.kind != keyEscaped {
		return 0, false
	}
	return uint32(key.id), true
}

func newAliasInfo(f *ir.Func) *aliasInfo {
	ai := &aliasInfo{
		f:             f,
		def:           make([]*ir.Instr, len(f.Temps)),
		allocBasePlus: make([]uint32, len(f.Temps)),
		escaped:       make([]bool, len(f.Temps)),
	}
	for _, b := range f.Blocks {
		for index := range b.Instrs {
			in := &b.Instrs[index]
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
		if in != nil && in.Op.IsAlloc() {
			ai.setAllocBase(uint32(id), uint32(id))
		}
	}
	for changed := true; changed; {
		changed = false
		for id, in := range ai.def {
			if in == nil {
				continue
			}
			if _, done := ai.allocBase(uint32(id)); done {
				continue
			}
			if in.Op != ir.OAdd && in.Op != ir.OSub {
				continue
			}
			x, y := in.Arg(0), in.Arg(1)
			if base, ok := ai.derivedBase(x, y); ok {
				ai.setAllocBase(uint32(id), base)
				changed = true
			} else if in.Op == ir.OAdd {
				if base, ok := ai.derivedBase(y, x); ok {
					ai.setAllocBase(uint32(id), base)
					changed = true
				}
			}
		}
	}
}

func (ai *aliasInfo) setAllocBase(id uint32, base uint32) {
	if int(id) >= len(ai.allocBasePlus) {
		return
	}
	ai.allocBasePlus[id] = base + 1
}

func (ai *aliasInfo) allocBase(id uint32) (uint32, bool) {
	if int(id) >= len(ai.allocBasePlus) {
		return 0, false
	}
	base := ai.allocBasePlus[id]
	if base == 0 {
		return 0, false
	}
	return base - 1, true
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
	base, ok := ai.allocBase(ptr.ID)
	return base, ok
}

// computeEscape marks an allocation as escaped whenever a pointer derived from
// it is used as anything other than a load/store address or a tracked
// constant-offset derivation.
func (ai *aliasInfo) computeEscape() {
	mark := func(r ir.Ref) {
		if r.Kind == ir.RefTemp {
			if base, ok := ai.allocBase(r.ID); ok && int(base) < len(ai.escaped) {
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
	_, ok := ai.allocBase(r.ID)
	return ok
}

// locOf resolves a pointer reference to a memory location of the given width,
// stripping constant offsets from pointer arithmetic.
func (ai *aliasInfo) locOf(addr ir.Ref, width int) locInfo {
	var offset int64
	cur := addr
	for cur.Kind == ir.RefTemp {
		if int(cur.ID) >= len(ai.def) {
			break
		}
		in := ai.def[cur.ID]
		if in == nil || (in.Op != ir.OAdd && in.Op != ir.OSub) {
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
	keyKind, keyID, keySym, class := ai.baseKind(cur)
	if cur.Kind == ir.RefConst {
		if c := ai.f.Consts[cur.ID]; c.Kind == ir.ConstSym {
			offset += c.Int
		}
	}
	return locInfo{
		keyKind:  keyKind,
		keyID:    keyID,
		keySym:   keySym,
		class:    class,
		offset:   offset,
		offKnown: true,
		width:    width,
	}
}

func (ai *aliasInfo) baseKind(r ir.Ref) (int, uint64, string, int) {
	switch r.Kind {
	case ir.RefConst:
		c := ai.f.Consts[r.ID]
		if c.Kind == ir.ConstSym {
			return keyGlobal, 0, c.Sym, cGlobal
		}
		return keyConst, uint64(c.Int), "", cUnknown
	case ir.RefTemp:
		if base, ok := ai.allocBase(r.ID); ok {
			if int(base) < len(ai.escaped) && ai.escaped[base] {
				return keyEscaped, uint64(base), "", cEscaped
			}
			return keyLocal, uint64(base), "", cLocal
		}
		return keyTemp, uint64(r.ID), "", cUnknown
	}
	return keyInvalid, 0, "", cUnknown
}

// mustAlias reports that two accesses touch exactly the same bytes.
func mustAlias(a, b locInfo) bool {
	return a.keyKind != keyInvalid && sameBase(a, b) && a.offKnown && b.offKnown &&
		a.offset == b.offset && a.width == b.width
}

// mayAlias reports that two accesses might touch overlapping bytes.
func mayAlias(a, b locInfo) bool {
	if sameBase(a, b) {
		if a.offKnown && b.offKnown {
			return overlap(a.offset, a.width, b.offset, b.width)
		}
		return true
	}
	return !distinct(a, b)
}

func sameBase(a, b locInfo) bool {
	if a.keyKind != b.keyKind {
		return false
	}
	if a.keyKind == keyInvalid {
		return false
	}
	if a.keyKind == keyGlobal {
		return a.keySym == b.keySym
	}
	return a.keyID == b.keyID
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
