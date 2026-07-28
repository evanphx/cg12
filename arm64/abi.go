package arm64

import "github.com/evanphx/cg12/ir"

// aggKind is how AAPCS64 passes a by-value aggregate.
type aggKind int

const (
	aggMemory aggKind = iota // > 16 bytes: passed by reference (a pointer)
	aggGP                    // <= 16 bytes, not an HFA: 1-2 general registers
	aggHFA                   // homogeneous float aggregate: 1-4 SIMD registers
)

// aggClass is the classification of an aggregate for parameter passing.
type aggClass struct {
	kind  aggKind
	nregs int    // GP: 1-2 x registers; HFA: 1-4 v registers
	elem  ir.Cls // HFA element class (S or D)
	size  int    // byte size of the aggregate
}

// classifyAgg classifies an aggregate type per the AAPCS64 rules.
func classifyAgg(t *ir.AggType) aggClass {
	size, _ := t.Layout()
	if isQuadAgg(t) {
		// A 128-bit quad (long double) travels in a single SIMD register.
		return aggClass{kind: aggHFA, nregs: 1, elem: ir.ClsQ, size: 16}
	}
	if elem, n, ok := hfaOf(t); ok {
		return aggClass{kind: aggHFA, nregs: n, elem: elem, size: size}
	}
	if size <= 16 {
		return aggClass{kind: aggGP, nregs: (size + 7) / 8, size: size}
	}
	return aggClass{kind: aggMemory, size: size}
}

// isQuadAgg reports whether t is the aggregate that models a 128-bit quad (a
// single SubQ field), which AAPCS64 passes in one full SIMD register.
func isQuadAgg(t *ir.AggType) bool {
	return !t.Union && !t.Opaque && len(t.Fields) == 1 &&
		t.Fields[0].Type == nil && t.Fields[0].Sub == ir.SubQ && t.Fields[0].Count <= 1
}

// hfaOf reports whether t is a Homogeneous Floating-point Aggregate — every leaf
// member is the same single/double type and there are 1 to 4 of them.
func hfaOf(t *ir.AggType) (ir.Cls, int, bool) {
	// A union has no single sequence of members -- its cases overlap -- and an
	// opaque type's members are unknown by construction, so neither can be
	// homogeneous in a way this question means.
	leaves, simple := t.Leaves()
	if !simple || len(leaves) == 0 || len(leaves) > 4 {
		return 0, 0, false
	}
	elem := leaves[0].Sub
	if elem != ir.SubS && elem != ir.SubD && elem != ir.SubQ {
		return 0, 0, false
	}
	for _, l := range leaves[1:] {
		if l.Sub != elem {
			return 0, 0, false
		}
	}
	return elem.Cls(), len(leaves), true
}

// assignGP allocates n consecutive integer registers for an aggregate of the
// given byte size, or places the whole aggregate on the stack.
func (a *argAssigner) assignGP(n, size int) (regs []Reg, onStack bool, off int) {
	if a.ngrn+n <= a.intRegs {
		for i := 0; i < n; i++ {
			regs = append(regs, Reg(int(X0)+a.ngrn+i))
		}
		a.ngrn += n
		return regs, false, 0
	}
	a.ngrn = a.intRegs
	off = a.nsaa
	a.nsaa += roundUp(size, 8)
	return nil, true, off
}

// assignHFA allocates n consecutive SIMD registers for a homogeneous float
// aggregate of the given byte size, or places it on the stack.
func (a *argAssigner) assignHFA(n, size int) (regs []Reg, onStack bool, off int) {
	if a.nsrn+n <= a.floatRegs {
		for i := 0; i < n; i++ {
			regs = append(regs, vReg(a.nsrn+i))
		}
		a.nsrn += n
		return regs, false, 0
	}
	a.nsrn = a.floatRegs
	off = a.nsaa
	a.nsaa += roundUp(size, 8)
	return nil, true, off
}

// offsetAddr returns a reference to base + off, appending an add when off != 0.
func offsetAddr(f *ir.Func, base ir.Ref, off int, out *[]ir.Instr) ir.Ref {
	if off == 0 {
		return base
	}
	t := f.NewTemp("", ir.ClsL)
	*out = append(*out, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: t, Args: []ir.Ref{base, f.Long(int64(off))}})
	return t
}

// emitMemcpy appends load/store instructions copying size bytes from src to dst,
// using the largest power-of-two chunk at each step.
func emitMemcpy(f *ir.Func, dst, src ir.Ref, size int, out *[]ir.Instr) {
	chunk := func(off, w int, loadOp, storeOp ir.Op, cls ir.Cls) {
		s := offsetAddr(f, src, off, out)
		d := offsetAddr(f, dst, off, out)
		v := f.NewTemp("", cls)
		*out = append(*out, ir.Instr{Op: loadOp, Cls: cls, To: v, Args: []ir.Ref{s}})
		*out = append(*out, ir.Instr{Op: storeOp, Cls: cls, Args: []ir.Ref{v, d}})
	}
	off := 0
	for size-off >= 8 {
		chunk(off, 8, ir.OLoadl, ir.OStorel, ir.ClsL)
		off += 8
	}
	for size-off >= 4 {
		chunk(off, 4, ir.OLoaduw, ir.OStorew, ir.ClsW)
		off += 4
	}
	for size-off >= 2 {
		chunk(off, 2, ir.OLoaduh, ir.OStoreh, ir.ClsW)
		off += 2
	}
	for size-off >= 1 {
		chunk(off, 1, ir.OLoadub, ir.OStoreb, ir.ClsW)
		off++
	}
}

// aggArgAt returns the aggregate type of the k-th value argument of a call, or
// nil for a scalar argument.
func aggArgAt(in *ir.Instr, k int) *ir.AggType {
	if k < len(in.AggArgs) {
		return in.AggArgs[k]
	}
	return nil
}

// lowerAggArg lowers one by-value aggregate call argument. Value-computation
// (loads/address arithmetic) is appended to *out; the OArg moves that place the
// pieces into registers or the outgoing stack area are appended to argSetup
// (returned) so they form one contiguous run before the call.
func lowerAggArg(f *ir.Func, argRef ir.Ref, agg *ir.AggType, goInternal bool, a *argAssigner, out *[]ir.Instr, argSetup []ir.Instr, pins []ir.Ref) ([]ir.Instr, []ir.Ref, error) {
	if goInternal {
		return lowerGoAggregateArg(f, argRef, agg, a, out, argSetup, pins)
	}
	cls := classifyAgg(agg)
	if cls.kind == aggMemory {
		// Passed by reference: hand over the pointer like a scalar.
		loc := a.assign(ir.ClsL)
		if loc.onStack {
			argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: ir.ClsL, To: ir.R, Aux: int64(loc.stacky), Args: []ir.Ref{argRef}})
			return argSetup, pins, nil
		}
		pin := newPinned(f, loc.reg, ir.ClsL)
		argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: ir.ClsL, To: pin, Args: []ir.Ref{argRef}})
		return argSetup, append(pins, pin), nil
	}

	var regs []Reg
	var elemCls ir.Cls
	var elemSize int
	var onStack bool
	var stackOffset int
	if cls.kind == aggGP {
		regs, onStack, stackOffset = a.assignGP(cls.nregs, cls.size)
		elemCls, elemSize = ir.ClsL, 8
	} else {
		regs, onStack, stackOffset = a.assignHFA(cls.nregs, cls.size)
		elemCls, elemSize = cls.elem, cls.elem.Size()
	}
	if onStack {
		argSetup = append(argSetup, ir.Instr{
			Op:     ir.OArg,
			To:     ir.R,
			Aux:    int64(stackOffset),
			Args:   []ir.Ref{argRef},
			RetAgg: agg,
		})
		return argSetup, pins, nil
	}

	for i, r := range regs {
		addr := argRef
		if i > 0 {
			tmp := f.NewTemp("", ir.ClsL)
			*out = append(*out, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: tmp, Args: []ir.Ref{argRef, f.Long(int64(i * elemSize))}})
			addr = tmp
		}
		val := f.NewTemp("", elemCls)
		*out = append(*out, ir.Instr{Op: loadOpFor(elemCls), Cls: elemCls, To: val, Args: []ir.Ref{addr}})
		pin := newPinned(f, r, elemCls)
		argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: elemCls, To: pin, Args: []ir.Ref{val}})
		pins = append(pins, pin)
	}
	return argSetup, pins, nil
}

// lowerAggResult prepares a call whose result is a by-value aggregate. It
// returns the call's result register (callTo/callCls) and extra defined
// registers (defs), the updated argument run and pins (a Memory result adds the
// x8 buffer pointer), and the post-call instructions that place the result.
func lowerAggResult(f *ir.Func, dst ir.Ref, agg *ir.AggType, a *argAssigner, out *[]ir.Instr, argSetup []ir.Instr, pins []ir.Ref) (callTo ir.Ref, callCls ir.Cls, defs []ir.Ref, newSetup []ir.Instr, newPins []ir.Ref, post []ir.Instr) {
	cls := classifyAgg(agg)
	if cls.kind == aggMemory {
		// Allocate a buffer and hand its address to the callee in x8.
		buf := f.AllocAggregate(agg, out)
		pinX8 := newPinned(f, X8, ir.ClsL)
		argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: ir.ClsL, To: pinX8, Args: []ir.Ref{buf}})
		pins = append(pins, pinX8)
		post = []ir.Instr{{Op: ir.OCopy, Cls: ir.ClsL, To: dst, Args: []ir.Ref{buf}}}
		return ir.R, 0, nil, argSetup, pins, post
	}

	// Register result: store the returned registers into a fresh slot and point
	// the result temporary at it.
	base, elemCls, elemSize := X0, ir.ClsL, 8
	if cls.kind == aggHFA {
		base, elemCls, elemSize = V0, cls.elem, cls.elem.Size()
	}
	slot := f.AllocAggregate(agg, out)

	var defRefs []ir.Ref
	for i := 0; i < cls.nregs; i++ {
		pin := newPinned(f, base+Reg(i), elemCls)
		if i == 0 {
			callTo, callCls = pin, elemCls
		} else {
			defRefs = append(defRefs, pin)
		}
		addr := offsetAddr(f, slot, i*elemSize, &post)
		post = append(post, ir.Instr{Op: storeOpFor(elemCls), Cls: elemCls, Args: []ir.Ref{pin, addr}})
	}
	post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: dst, Args: []ir.Ref{slot}})
	return callTo, callCls, defRefs, argSetup, pins, post
}

// storeOp / loadOp return the full-width memory ops for a register class.
func storeOpFor(cls ir.Cls) ir.Op {
	switch cls {
	case ir.ClsL:
		return ir.OStorel
	case ir.ClsS:
		return ir.OStores
	case ir.ClsD:
		return ir.OStored
	case ir.ClsQ:
		return ir.OStoreq
	default:
		return ir.OStorew
	}
}

func loadOpFor(cls ir.Cls) ir.Op {
	switch cls {
	case ir.ClsL:
		return ir.OLoadl
	case ir.ClsS:
		return ir.OLoads
	case ir.ClsD:
		return ir.OLoadd
	case ir.ClsQ:
		return ir.OLoadq
	default:
		return ir.OLoaduw
	}
}
