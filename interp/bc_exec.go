package interp

import (
	"math/bits"

	"github.com/evanphx/cg12/ir"
)

// compileCached compiles f to bytecode once and memoizes it, so mutual recursion
// and indirect calls resolve on first use.
func (mc *Machine) compileCached(f *ir.Func) (*bcFunc, error) {
	if bf, ok := mc.bc[f]; ok {
		return bf, nil
	}
	bf, err := mc.compile(f)
	if err != nil {
		return nil, err
	}
	mc.bc[f] = bf
	return bf, nil
}

// vmCall is the bytecode counterpart of callFunc: the top-level entry that places
// arguments at the base of the register window and runs the function.
func (mc *Machine) vmCall(f *ir.Func, args []Value) (Value, error) {
	bf, err := mc.compileCached(f)
	if err != nil {
		return Value{}, err
	}
	switch {
	case len(args) < bf.nParams:
		return Value{}, mc.trapf("%s: got %d args, want %d", f.Name, len(args), bf.nParams)
	case len(args) > bf.nParams && !f.Variadic:
		return Value{}, mc.trapf("%s: got %d args, want %d (not variadic)", f.Name, len(args), bf.nParams)
	}
	mc.ensureRegs(uint32(bf.frameSize))
	for i, a := range args {
		mc.regs[i] = a // normalized in vmInvoke
	}
	var va []Value
	if f.Variadic {
		va = append([]Value(nil), args[bf.nParams:]...)
	}
	return mc.vmInvoke(bf, 0, va)
}

// vmInvoke runs a compiled function with its parameters already staged at
// regs[base..], and va holding the variadic arguments (for OVaStart). It reuses
// callFunc's discipline exactly: the depth guard and the sp-restore that frees
// the frame's stack allocations on return.
func (mc *Machine) vmInvoke(bf *bcFunc, base uint32, va []Value) (Value, error) {
	if mc.depth >= mc.maxDepth {
		return Value{}, mc.trapf("call stack too deep (%d frames)", mc.depth)
	}
	mc.ensureRegs(base + uint32(bf.frameSize))
	for i := 0; i < bf.nParams; i++ {
		mc.regs[base+uint32(i)] = mc.regs[base+uint32(i)].asClass(bf.paramCls[i])
	}
	savedSP := mc.sp
	mc.depth++
	ret, err := mc.vmLoop(bf, base, va)
	mc.depth--
	mc.sp = savedSP
	return ret, err
}

func (mc *Machine) ensureRegs(n uint32) {
	if int(n) <= len(mc.regs) {
		return
	}
	grow := len(mc.regs) * 2
	if grow < int(n) {
		grow = int(n)
	}
	regs := make([]Value, grow)
	copy(regs, mc.regs)
	mc.regs = regs
}

// vmLoop is the dispatch loop: the bytecode counterpart of the tree-walker's run.
func (mc *Machine) vmLoop(bf *bcFunc, base uint32, va []Value) (Value, error) {
	pc := uint32(0)
	for {
		if err := mc.tick(); err != nil {
			return Value{}, err
		}
		w := bf.code[pc]
		switch w.op() {
		case bcNop:
			pc++
		case bcMove:
			mc.regs[base+uint32(w.ra())] = mc.regs[base+uint32(w.rb())].asClass(w.cls())
			pc++
		case bcLoadK:
			mc.regs[base+uint32(w.ra())] = bf.consts[w.bx()]
			pc++
		case bcBinop:
			x := mc.regs[base+uint32(w.rb())]
			y := mc.regs[base+uint32(w.rc())]
			var r Value
			var err error
			if w.cls().IsFloat() {
				r, err = mc.floatBinop(ir.Op(w.aux()), w.cls(), x, y)
			} else {
				r, err = mc.intBinop(ir.Op(w.aux()), w.cls(), x, y)
			}
			if err != nil {
				return Value{}, err
			}
			mc.regs[base+uint32(w.ra())] = r
			pc++
		case bcCmp:
			pred := ir.Cmp(w.aux())
			x := mc.regs[base+uint32(w.rb())]
			y := mc.regs[base+uint32(w.rc())]
			var r bool
			if pred.IsFloat() {
				r = floatCmp(pred, x.f64(), y.f64())
			} else {
				r = intCmp(pred, x.Cls, x.i64(), y.i64())
			}
			mc.regs[base+uint32(w.ra())] = intVal(w.cls(), b2i(r))
			pc++
		case bcSel:
			si := &bf.sels[w.bx()]
			pick := si.b
			if mc.regs[base+uint32(si.cond)].u64() != 0 {
				pick = si.a
			}
			mc.regs[base+uint32(w.ra())] = mc.regs[base+uint32(pick)].asClass(w.cls())
			pc++
		case bcUnop:
			op := ir.Op(w.aux())
			switch op {
			case ir.OStackSave:
				mc.regs[base+uint32(w.ra())] = ptrVal(mc.sp)
			case ir.OStackRestore:
				mc.sp = mc.regs[base+uint32(w.rb())].u64()
			default:
				r, err := mc.evalUnop(op, w.cls(), mc.regs[base+uint32(w.rb())])
				if err != nil {
					return Value{}, err
				}
				mc.regs[base+uint32(w.ra())] = r
			}
			pc++
		case bcLoad:
			r, err := mc.vmLoad(ir.Op(w.aux()), w.cls(), mc.regs[base+uint32(w.rb())].u64())
			if err != nil {
				return Value{}, err
			}
			mc.regs[base+uint32(w.ra())] = r
			pc++
		case bcStore:
			if err := mc.vmStore(ir.Op(w.aux()), mc.regs[base+uint32(w.ra())], mc.regs[base+uint32(w.rb())].u64()); err != nil {
				return Value{}, err
			}
			pc++
		case bcAlloc:
			addr, err := mc.vmAlloc(ir.Op(w.aux()), mc.regs[base+uint32(w.rb())].u64())
			if err != nil {
				return Value{}, err
			}
			mc.regs[base+uint32(w.ra())] = ptrVal(addr)
			pc++
		case bcBlit:
			if err := mc.copyMem(mc.regs[base+uint32(w.ra())].u64(), mc.regs[base+uint32(w.rb())].u64(), int(w.rc())); err != nil {
				return Value{}, err
			}
			pc++
		case bcCall:
			if err := mc.vmCallSite(bf, base, &bf.calls[w.bx()]); err != nil {
				return Value{}, err
			}
			pc++
		case bcVaStart:
			token := uint64(len(mc.vaLists))
			mc.vaLists = append(mc.vaLists, vaState{args: va})
			ap := mc.regs[base+uint32(w.ra())].u64()
			if !mc.mem.store(ap, 8, token) {
				return Value{}, mc.trapf("va_start: list storage at %#x unmapped", ap)
			}
			pc++
		case bcVaArg:
			ap := mc.regs[base+uint32(w.rb())].u64()
			token, ok := mc.mem.load(ap, 8)
			if !ok || token >= uint64(len(mc.vaLists)) {
				return Value{}, mc.trapf("va_arg: invalid va_list at %#x", ap)
			}
			v, ok := mc.vaLists[token].take()
			if !ok {
				return Value{}, mc.trapf("va_arg: no more variadic arguments")
			}
			mc.regs[base+uint32(w.ra())] = v.asClass(w.cls())
			pc++
		case bcBr:
			next, err := mc.vmBr(bf, base, w)
			if err != nil {
				return Value{}, err
			}
			pc = next
		case bcJmp:
			pc = w.bx()
		case bcJnz:
			if mc.regs[base+uint32(w.ra())].u64() != 0 {
				pc = w.bx()
			} else {
				pc++
			}
		case bcRet:
			if w.aux() == 0 {
				return Value{}, nil
			}
			v := mc.regs[base+uint32(w.ra())]
			if bf.fn.HasRet && bf.fn.RetAgg == nil {
				v = v.asClass(w.cls())
			}
			return v, nil
		case bcHlt:
			return Value{}, mc.trapf("hlt reached")
		case bcSwitch:
			pc = mc.vmSwitch(bf, base, w)
		case bcTable:
			next, err := mc.vmTable(bf, base, w)
			if err != nil {
				return Value{}, err
			}
			pc = next
		default:
			return Value{}, mc.trapf("bytecode: unhandled opcode %d", w.op())
		}
	}
}

// evalUnop mirrors evalValueOp's unary cases (ops.go), reusing the same pure
// helpers so the two engines cannot drift.
func (mc *Machine) evalUnop(op ir.Op, cls ir.Cls, src Value) (Value, error) {
	switch op {
	case ir.ONeg:
		if cls.IsFloat() {
			return negFloat(cls, src), nil
		}
		return intVal(cls, truncInt(cls, -src.i64())), nil
	case ir.OClz:
		if cls == ir.ClsW {
			return intVal(ir.ClsW, int64(bits.LeadingZeros32(uint32(src.u64())))), nil
		}
		return intVal(ir.ClsL, int64(bits.LeadingZeros64(src.u64()))), nil
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw:
		return intVal(cls, extendInt(op, src.i64())), nil
	case ir.OExts:
		return floatVal(ir.ClsD, src.f64()), nil
	case ir.OTruncd:
		return floatVal(ir.ClsS, src.f64()), nil
	case ir.OStosi:
		return intVal(cls, f2iSigned(src.f64(), intBits(cls))), nil
	case ir.OStoui:
		return intVal(cls, int64(f2iUnsigned(src.f64(), intBits(cls)))), nil
	case ir.OSltof:
		return floatVal(cls, float64(asSigned(src.Cls, src.i64()))), nil
	case ir.OUltof:
		return floatVal(cls, uint64ToFloat(asUnsigned(src.Cls, src.i64()))), nil
	case ir.OCast:
		return castValue(cls, src), nil
	case ir.OCopy:
		return src.asClass(cls), nil
	}
	return Value{}, mc.trapf("bytecode: unhandled unary op %s", op)
}

// vmCallSite performs a compiled call: an IR function rolls the window up to its
// staged arguments; an extern receives the staged arguments as a slice.
func (mc *Machine) vmCallSite(bf *bcFunc, base uint32, site *callSite) error {
	name := site.name
	if name == "" {
		// Indirect: resolve the callee address to a function or extern.
		addr := mc.regs[base+uint32(site.calleeReg)].u64()
		if f, ok := mc.funcAt[addr]; ok {
			return mc.vmIRCall(bf, base, site, f)
		}
		if n, ok := mc.externAt[addr]; ok {
			return mc.vmExternCall(base, site, n)
		}
		return mc.trapf("bytecode: indirect call to %#x is not a known function", addr)
	}
	if f, ok := mc.funcs[name]; ok {
		return mc.vmIRCall(bf, base, site, f)
	}
	if _, ok := mc.externs[name]; ok {
		return mc.vmExternCall(base, site, name)
	}
	return mc.trapf("bytecode: call to undefined symbol %q", name)
}

func (mc *Machine) vmIRCall(bf *bcFunc, base uint32, site *callSite, f *ir.Func) error {
	// The callee's window rolls up to where its arguments were staged.
	calleeBase := base + uint32(site.argBase)

	// A by-value aggregate argument arrives as a pointer; copy the aggregate into
	// fresh stack so the callee gets its own copy (the tree-walker's discipline).
	for k := 0; k < site.nArgs; k++ {
		if k < len(site.aggArgs) && site.aggArgs[k] != nil {
			size, align := site.aggArgs[k].Layout()
			buf := mc.stackAlloc(uint64(size), uint64(align))
			if err := mc.copyMem(buf, mc.regs[calleeBase+uint32(k)].u64(), size); err != nil {
				return err
			}
			mc.regs[calleeBase+uint32(k)] = ptrVal(buf)
		}
	}

	callee, err := mc.compileCached(f)
	if err != nil {
		return err
	}
	var vararg []Value
	if f.Variadic {
		for i := callee.nParams; i < site.nArgs; i++ {
			vararg = append(vararg, mc.regs[calleeBase+uint32(i)])
		}
	}
	ret, err := mc.vmInvoke(callee, calleeBase, vararg)
	if err != nil {
		return err
	}

	// A by-value aggregate result is a pointer into the callee's now-freed stack;
	// copy it into caller storage that outlives the call.
	if site.retAgg != nil {
		size, align := site.retAgg.Layout()
		buf := mc.stackAlloc(uint64(size), uint64(align))
		if err := mc.copyMem(buf, ret.u64(), size); err != nil {
			return err
		}
		ret = ptrVal(buf)
	}
	if site.hasResult {
		mc.regs[base+uint32(site.resultReg)] = ret.asClass(site.resultCls)
	}
	return nil
}

// vmBr resolves a computed goto: the address a BLOCKADDR minted maps back to a
// block, whose pc is this function's landing point.
func (mc *Machine) vmBr(bf *bcFunc, base uint32, w inst) (uint32, error) {
	addr := mc.regs[base+uint32(w.ra())].u64()
	blk, ok := mc.blockAt[addr]
	if !ok {
		return 0, mc.trapf("computed goto to %#x is not a block address", addr)
	}
	pc, ok := bf.blockPC[blk]
	if !ok {
		return 0, mc.trapf("computed goto leaves the current function")
	}
	return pc, nil
}

func (mc *Machine) vmExternCall(base uint32, site *callSite, name string) error {
	args := make([]Value, site.nArgs)
	for i := 0; i < site.nArgs; i++ {
		args[i] = mc.regs[base+uint32(site.argBase)+uint32(i)]
	}
	ret, err := mc.externs[name](mc, args)
	if err != nil {
		return err
	}
	if site.hasResult {
		mc.regs[base+uint32(site.resultReg)] = ret.asClass(site.resultCls)
	}
	return nil
}

func (mc *Machine) vmSwitch(bf *bcFunc, base uint32, w inst) uint32 {
	info := &bf.switches[w.bx()]
	v := mc.regs[base+uint32(w.ra())]
	for _, cs := range info.cases {
		if switchMatch(info.argCls, info.signed, v, cs.val) {
			return cs.pc
		}
	}
	return info.dflt
}

func (mc *Machine) vmTable(bf *bcFunc, base uint32, w inst) (uint32, error) {
	info := &bf.switches[w.bx()]
	idx := mc.regs[base+uint32(w.ra())].u64()
	if idx >= uint64(len(info.targets)) {
		return 0, mc.trapf("jmptable index %d out of range (%d targets)", idx, len(info.targets))
	}
	return info.targets[idx], nil
}
