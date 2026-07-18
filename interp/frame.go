package interp

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// frame is one function activation: its SSA temporaries (indexed densely by
// Temp.ID) plus the execution cursor used for trap locations and, from Phase B,
// its stack region.
type frame struct {
	fn      *ir.Func
	vals    []Value
	varargs []Value // extra call args beyond fn.Params, for OVaArg

	block  *ir.Block // currently executing block
	index  int       // instruction index, or -1 at a terminator
	opDesc string    // description of the current op, for traps

	spBase, sp uint64 // this frame's stack region (Phase B)
}

// callFunc binds arguments to parameters and runs the function to a return.
func (mc *Machine) callFunc(f *ir.Func, args []Value) (Value, error) {
	np := len(f.Params)
	switch {
	case len(args) < np:
		return Value{}, fmt.Errorf("interp: %s: got %d args, want %d", f.Name, len(args), np)
	case len(args) > np && !f.Variadic:
		return Value{}, fmt.Errorf("interp: %s: got %d args, want %d (not variadic)", f.Name, len(args), np)
	}
	if mc.depth >= mc.maxDepth {
		return Value{}, mc.trapf("call stack too deep (%d frames)", mc.depth)
	}

	fr := &frame{fn: f, vals: make([]Value, len(f.Temps)), index: -1, spBase: mc.sp}
	for i, p := range f.Params {
		fr.vals[p.ID] = args[i].asClass(p.Cls)
	}
	if f.Variadic {
		fr.varargs = append([]Value(nil), args[np:]...)
	}

	prev := mc.cur
	mc.cur, mc.depth = fr, mc.depth+1
	ret, err := mc.run(fr)
	mc.cur, mc.depth = prev, mc.depth-1
	mc.sp = fr.spBase // free this frame's stack allocations
	return ret, err
}

// evalRef resolves an operand reference to a Value.
func (mc *Machine) evalRef(fr *frame, r ir.Ref) (Value, error) {
	switch r.Kind {
	case ir.RefTemp:
		return fr.vals[r.ID], nil
	case ir.RefConst:
		return mc.evalConst(&fr.fn.Consts[r.ID])
	case ir.RefNone:
		return Value{}, mc.trapf("absent operand")
	default:
		// RefType/RefSlot/RefReg only appear post-lowering or as non-value operands.
		return Value{}, mc.trapf("operand ref kind %d has no value", r.Kind)
	}
}

// evalConst materializes a constant. A ConstSym is only meaningful once memory
// exists (Phase B) — except as a call target, which execCall resolves by name
// before ever reaching here.
func (mc *Machine) evalConst(c *ir.Const) (Value, error) {
	switch c.Kind {
	case ir.ConstInt:
		return intVal(c.Cls, c.Int), nil
	case ir.ConstFloat:
		return floatVal(c.Cls, c.Flt), nil
	case ir.ConstSym:
		addr, ok := mc.symAddr(c.Sym)
		if !ok {
			return Value{}, mc.trapf("undefined symbol %q", c.Sym)
		}
		return ptrVal(addr + uint64(c.Int)), nil
	}
	return Value{}, mc.trapf("unknown constant kind %d", c.Kind)
}

// resolvePhis performs the parallel phi transfer on entry to block b from block
// from. All phi arguments are read against the pre-transfer frame, then committed
// together, so a phi whose argument is another phi's target (a swap) is correct.
func (mc *Machine) resolvePhis(fr *frame, b, from *ir.Block) error {
	if len(b.Phis) == 0 {
		return nil
	}
	staged := make([]Value, len(b.Phis))
	for i, p := range b.Phis {
		k := -1
		for j, pred := range p.Blocks {
			if pred == from {
				k = j
				break
			}
		}
		if k < 0 {
			return mc.trapf("phi in %s has no argument for predecessor %s", b.Name, blockName(from))
		}
		v, err := mc.evalRef(fr, p.Args[k])
		if err != nil {
			return err
		}
		staged[i] = v.asClass(p.Cls)
	}
	for i, p := range b.Phis {
		fr.vals[p.To.ID] = staged[i]
	}
	return nil
}

func blockName(b *ir.Block) string {
	if b == nil {
		return "<entry>"
	}
	return b.Name
}
