package interp

import "github.com/evanphx/cg12/ir"

// run drives a frame from its entry block to a return.
func (mc *Machine) run(fr *frame) (Value, error) {
	cur := fr.fn.Start
	var from *ir.Block
	for {
		fr.block = cur
		if err := mc.resolvePhis(fr, cur, from); err != nil {
			return Value{}, err
		}
		for i := range cur.Instrs {
			fr.index = i
			in := &cur.Instrs[i]
			fr.opDesc = in.Op.String()
			if err := mc.tick(); err != nil {
				return Value{}, err
			}
			if err := mc.execInstr(fr, in); err != nil {
				return Value{}, err
			}
		}
		fr.index = -1
		fr.opDesc = "terminator"
		next, ret, done, err := mc.execTerminator(fr, cur)
		if err != nil {
			return Value{}, err
		}
		if done {
			return ret, nil
		}
		from, cur = cur, next
	}
}

// tick charges one instruction against the fuel budget (0 = unlimited).
func (mc *Machine) tick() error {
	if mc.fuel > 0 {
		mc.fuel--
		if mc.fuel == 0 {
			return mc.trapf("fuel exhausted")
		}
	}
	return nil
}

// execTerminator evaluates a block's terminator, returning the next block, or a
// return value with done=true.
func (mc *Machine) execTerminator(fr *frame, b *ir.Block) (next *ir.Block, ret Value, done bool, err error) {
	j := &b.Jmp
	switch j.Kind {
	case ir.JmpJmp:
		return j.To, Value{}, false, nil

	case ir.JmpJnz:
		v, err := mc.evalRef(fr, j.Arg)
		if err != nil {
			return nil, Value{}, false, err
		}
		if v.u64() != 0 {
			return j.To, Value{}, false, nil
		}
		return j.To2, Value{}, false, nil

	case ir.JmpRet:
		if j.Arg.IsNone() {
			return nil, Value{}, true, nil
		}
		v, err := mc.evalRef(fr, j.Arg)
		if err != nil {
			return nil, Value{}, false, err
		}
		if fr.fn.HasRet && fr.fn.RetAgg == nil {
			v = v.asClass(fr.fn.Retty)
		}
		return nil, v, true, nil

	case ir.JmpHlt:
		return nil, Value{}, false, mc.trapf("hlt reached")

	case ir.JmpSwitch:
		v, err := mc.evalRef(fr, j.Arg)
		if err != nil {
			return nil, Value{}, false, err
		}
		argCls := fr.fn.ClassOf(j.Arg)
		for _, c := range j.Cases {
			if switchMatch(argCls, j.Signed, v, c.Val) {
				return c.Blk, Value{}, false, nil
			}
		}
		return j.To, Value{}, false, nil // default

	case ir.JmpTable:
		v, err := mc.evalRef(fr, j.Arg)
		if err != nil {
			return nil, Value{}, false, err
		}
		idx := v.u64()
		if idx >= uint64(len(j.Targets)) {
			return nil, Value{}, false, mc.trapf("jmptable index %d out of range (%d targets)", idx, len(j.Targets))
		}
		return j.Targets[idx], Value{}, false, nil

	case ir.JmpBr:
		return nil, Value{}, false, mc.trapf("computed goto requires block-address memory (Phase C)")

	default:
		return nil, Value{}, false, mc.trapf("unterminated or unknown terminator kind %d", j.Kind)
	}
}

// switchMatch reports whether the dispatched value equals a case value, using
// the switch's signed/unsigned interpretation at the argument's class width.
func switchMatch(argCls ir.Cls, signed bool, v Value, caseVal int64) bool {
	if signed {
		return asSigned(argCls, v.i64()) == asSigned(argCls, caseVal)
	}
	return v.u64() == asUnsigned(argCls, caseVal)
}

// execCall performs an OCall: it resolves the callee (a named symbol) and runs
// the corresponding IR function, binding the result.
func (mc *Machine) execCall(fr *frame, in *ir.Instr) error {
	if len(in.Args) == 0 {
		return mc.trapf("call has no callee operand")
	}
	callee := in.Args[0]
	if callee.Kind == ir.RefConst {
		c := &fr.fn.Consts[callee.ID]
		if c.Kind == ir.ConstSym {
			if f, ok := mc.funcs[c.Sym]; ok {
				return mc.doIRCall(fr, in, f)
			}
			return mc.trapf("call to undefined symbol %q (external intrinsics are Phase B)", c.Sym)
		}
	}
	return mc.trapf("indirect call (function pointer) requires memory (Phase B)")
}

func (mc *Machine) doIRCall(fr *frame, in *ir.Instr, f *ir.Func) error {
	args := make([]Value, 0, len(in.Args)-1)
	for _, a := range in.Args[1:] {
		v, err := mc.evalRef(fr, a)
		if err != nil {
			return err
		}
		args = append(args, v)
	}
	ret, err := mc.callFunc(f, args)
	if err != nil {
		return err
	}
	if !in.To.IsNone() {
		fr.vals[in.To.ID] = ret.asClass(in.Cls)
	}
	return nil
}
