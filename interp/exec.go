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
		v, err := mc.evalRef(fr, j.Arg)
		if err != nil {
			return nil, Value{}, false, err
		}
		blk, ok := mc.blockAt[v.u64()]
		if !ok {
			return nil, Value{}, false, mc.trapf("computed goto to %#x is not a block address", v.u64())
		}
		return blk, Value{}, false, nil

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
	// A directly named callee (the common case) is resolved by name.
	if callee.Kind == ir.RefConst {
		c := &fr.fn.Consts[callee.ID]
		if c.Kind == ir.ConstSym {
			return mc.dispatchCall(fr, in, c.Sym)
		}
	}
	// Otherwise the callee is a value (a function pointer): resolve its address.
	addr, err := mc.evalRef(fr, callee)
	if err != nil {
		return err
	}
	if f, ok := mc.funcAt[addr.u64()]; ok {
		return mc.doIRCall(fr, in, f)
	}
	if name, ok := mc.externAt[addr.u64()]; ok {
		return mc.doExternCall(fr, in, name)
	}
	return mc.trapf("indirect call to %#x is not a known function", addr.u64())
}

func (mc *Machine) dispatchCall(fr *frame, in *ir.Instr, name string) error {
	if f, ok := mc.funcs[name]; ok {
		return mc.doIRCall(fr, in, f)
	}
	if _, ok := mc.externs[name]; ok {
		return mc.doExternCall(fr, in, name)
	}
	return mc.trapf("call to undefined symbol %q (register it with WithExtern)", name)
}

func (mc *Machine) doExternCall(fr *frame, in *ir.Instr, name string) error {
	args := make([]Value, 0, len(in.Args)-1)
	for _, a := range in.Args[1:] {
		v, err := mc.evalRef(fr, a)
		if err != nil {
			return err
		}
		args = append(args, v)
	}
	ret, err := mc.externs[name](mc, args)
	if err != nil {
		return err
	}
	if !in.To.IsNone() {
		fr.vals[in.To.ID] = ret.asClass(in.Cls)
	}
	return nil
}

func (mc *Machine) doIRCall(fr *frame, in *ir.Instr, f *ir.Func) error {
	args := make([]Value, 0, len(in.Args)-1)
	for k, a := range in.Args[1:] {
		v, err := mc.evalRef(fr, a)
		if err != nil {
			return err
		}
		// A by-value aggregate argument arrives as a pointer; copy the aggregate
		// into fresh stack storage so the callee mutating its parameter cannot be
		// seen by the caller (QBE's by-value semantics).
		if k < len(in.AggArgs()) && in.AggArgs()[k] != nil {
			size, align := in.AggArgs()[k].Layout()
			buf := mc.stackAlloc(uint64(size), uint64(align))
			if err := mc.copyMem(buf, v.u64(), size); err != nil {
				return err
			}
			v = ptrVal(buf)
		}
		args = append(args, v)
	}
	ret, err := mc.callFunc(f, args)
	if err != nil {
		return err
	}
	// A by-value aggregate result is returned as a pointer into the callee's
	// now-freed stack; copy it into caller storage that outlives the call before
	// anything can reuse those bytes.
	if in.RetAgg != nil {
		size, align := in.RetAgg.Layout()
		buf := mc.stackAlloc(uint64(size), uint64(align))
		if err := mc.copyMem(buf, ret.u64(), size); err != nil {
			return err
		}
		ret = ptrVal(buf)
	}
	if !in.To.IsNone() {
		fr.vals[in.To.ID] = ret.asClass(in.Cls)
	}
	return nil
}
