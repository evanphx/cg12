package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
	lowerpass "github.com/evanphx/cg12/lower"
)

// lower rewrites f from SSA into a form ready for register allocation and
// emission: critical edges are split, phis are replaced by copies, and the
// System V AMD64 calling convention is made explicit with copies to and from
// pre-coloured physical-register temporaries.
//
// The scalar subset (integer classes w/l and float classes s/d) is handled;
// by-value aggregates, variadics, and tail calls return an explicit error rather
// than emitting silently wrong code.
func lower(f *ir.Func) error {
	if err := f.MarkLowered("amd64"); err != nil {
		return err
	}
	lowerpass.JumpTables(f) // dense switches -> indexed branch (JmpTable)
	lowerpass.Switches(f)   // remaining multiway branches -> conditional branches
	lowerSelects(f)         // conditional selects -> CMOVcc, or diamonds where there is none
	lowerpass.HoistAllocas(f)
	foldAddressing(f) // array/alloca address computations -> [base+index*scale+disp]
	lowerpass.SplitCriticalEdges(f)
	lowerpass.CoalescePhis(f)
	lowerpass.DestructSSA(f)
	lowerpass.ThreadJumps(f) // collapse the empty forwarding blocks edge splitting left
	return lowerABI(f)
}

// lowerABI makes the calling convention explicit: parameters are copied out of
// their incoming registers, return values into rax/xmm0, and call
// arguments/results through the argument/result registers.
func lowerABI(f *ir.Func) error {
	if err := lowerTailCalls(f); err != nil {
		return err
	}
	// A function returning a MEMORY-class aggregate is given the result buffer's
	// address in rdi (the first integer argument); capture it so returns can write
	// there and return the pointer in rax.
	var retBuf ir.Ref
	if f.RetAgg != nil && classifyAgg(f.RetAgg).memory {
		retBuf = f.NewTemp("retbuf", ir.ClsL)
	}
	if err := lowerParams(f, retBuf); err != nil {
		return err
	}
	if err := lowerCalls(f); err != nil {
		return err
	}
	return lowerReturns(f, retBuf)
}

// lowerTailCalls neutralizes each tail-call block so the callee provides the
// return value directly: the block's return move is dropped and the call's result
// left uncaptured. The frame-reusing branch itself is emitted later. A tail-marked
// call anywhere but in tail position is rejected.
func lowerTailCalls(f *ir.Func) error {
	for _, b := range f.Blocks {
		call, ok := ir.TailCall(b)
		if !ok {
			if ir.HasTailCall(b) {
				return fmt.Errorf("amd64: tail call must be the last instruction and have its result returned")
			}
			continue
		}
		if call.RetAgg != nil {
			return fmt.Errorf("amd64: aggregate-returning tail call is not supported")
		}
		b.Jmp = ir.Jmp{Kind: ir.JmpRet} // the callee provides the return value
		call.To = ir.R                  // do not capture the result
	}
	return nil
}

func lowerParams(f *ir.Func, retBuf ir.Ref) error {
	a := newArgAssigner(false)
	// Register/stack parameter moves (OPar) must precede aggregate reconstruction,
	// so the emitter can treat the OPar run as one parallel move.
	var pars, recon []ir.Instr
	if !retBuf.IsNone() {
		// The sret pointer arrives in rdi, ahead of the arguments.
		pin := newPinned(f, argGP[0], ir.ClsL)
		a.ngrn = 1
		pars = append(pars, ir.Instr{Op: ir.OPar, Cls: ir.ClsL, To: retBuf, Args: []ir.Ref{pin}})
	}
	for _, p := range f.Params {
		if p.Agg != nil {
			ps, rs := lowerAggParam(f, p, &a)
			pars = append(pars, ps...)
			recon = append(recon, rs...)
			continue
		}
		loc := a.assign(p.Cls)
		if loc.onStack {
			pars = append(pars, ir.Instr{Op: ir.OPar, Cls: p.Cls, To: p.Ref(), Aux: int64(loc.stacky)})
			continue
		}
		pin := newPinned(f, loc.reg, p.Cls)
		pars = append(pars, ir.Instr{Op: ir.OPar, Cls: p.Cls, To: p.Ref(), Args: []ir.Ref{pin}})
	}
	f.Start.Instrs = append(append(pars, recon...), f.Start.Instrs...)
	return nil
}

// lowerAggParam handles one by-value aggregate parameter, returning the OPar
// instructions and the reconstruction instructions (register-class aggregates are
// rebuilt into a stack slot; MEMORY/stack aggregates are addressed in place).
func lowerAggParam(f *ir.Func, p *ir.Temp, a *argAssigner) (pars, recon []ir.Instr) {
	cls := classifyAgg(p.Agg)
	regs, onStack, off := a.assignAgg(cls)
	if cls.memory || onStack {
		// The aggregate sits in the incoming argument area; the parameter is its
		// address (the emitter leas an aggregate stack param).
		return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Aux: int64(off)}}, nil
	}
	slot := f.NewTemp("", ir.ClsL)
	recon = append(recon, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(len(regs) * 8))}})
	for i, r := range regs {
		addr := offsetAddr(f, slot, i*8, &recon)
		if r.float {
			_, storeOp, elemCls := ebOps(true, ebBytes(cls.size, i))
			pin := newPinned(f, r.reg, elemCls)
			recon = append(recon, ir.Instr{Op: storeOp, Cls: elemCls, Args: []ir.Ref{pin, addr}})
			continue
		}
		// The slot is a whole eightbyte, so storing the full register writes every
		// meaningful byte (its high bytes past an odd size are unused padding).
		pin := newPinned(f, r.reg, ir.ClsL)
		recon = append(recon, ir.Instr{Op: ir.OStorel, Cls: ir.ClsL, Args: []ir.Ref{pin, addr}})
	}
	recon = append(recon, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: p.Ref(), Args: []ir.Ref{slot}})
	return nil, recon
}

func lowerReturns(f *ir.Func, retBuf ir.Ref) error {
	for _, b := range f.Blocks {
		if b.Jmp.Kind != ir.JmpRet || b.Jmp.Arg.IsNone() {
			continue
		}
		if f.RetAgg != nil {
			lowerAggReturn(f, b, retBuf)
			continue
		}
		v := b.Jmp.Arg
		cls := f.ClassOf(v)
		pin := newPinned(f, retReg(cls), cls)
		b.Instrs = append(b.Instrs, ir.Instr{Op: ir.OCopy, Cls: cls, To: pin, Args: []ir.Ref{v}})
		b.Jmp.Arg = pin
	}
	return nil
}

// lowerAggReturn returns an aggregate: a MEMORY result is copied into the caller's
// buffer and the sret pointer returned in rax; a register result is loaded from
// the pointer into the return registers (rax/rdx, xmm0/xmm1).
func lowerAggReturn(f *ir.Func, b *ir.Block, retBuf ir.Ref) {
	ptr := b.Jmp.Arg
	cls := classifyAgg(f.RetAgg)
	if cls.memory {
		emitMemcpy(f, retBuf, ptr, cls.size, &b.Instrs)
		pin := newPinned(f, RAX, ir.ClsL)
		b.Instrs = append(b.Instrs, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: pin, Args: []ir.Ref{retBuf}})
		b.Jmp.Arg = pin
		return
	}
	ni, ns := 0, 0
	var pins []ir.Ref
	for i, part := range cls.parts {
		float := part == ebSSE
		addr := offsetAddr(f, ptr, i*8, &b.Instrs)
		var rr Reg
		elemCls := ir.ClsL
		var val ir.Ref
		if float {
			var loadOp ir.Op
			loadOp, _, elemCls = ebOps(true, ebBytes(cls.size, i))
			rr, ns = retSSERegs[ns], ns+1
			val = f.NewTemp("", elemCls)
			b.Instrs = append(b.Instrs, ir.Instr{Op: loadOp, Cls: elemCls, To: val, Args: []ir.Ref{addr}})
		} else {
			rr, ni = retIntRegs[ni], ni+1
			val = loadAggInt(f, addr, ebBytes(cls.size, i), &b.Instrs)
		}
		pin := newPinned(f, rr, elemCls)
		b.Instrs = append(b.Instrs, ir.Instr{Op: ir.OCopy, Cls: elemCls, To: pin, Args: []ir.Ref{val}})
		pins = append(pins, pin)
	}
	b.Jmp.Arg = pins[0]
	b.Jmp.Args = pins[1:]
}

func lowerCalls(f *ir.Func) error {
	for _, b := range f.Blocks {
		var out []ir.Instr
		for i := range b.Instrs {
			in := b.Instrs[i]
			if in.Op != ir.OCall {
				out = append(out, in)
				continue
			}
			callee := in.Args[0]
			callArgs := in.Args[1:]
			pins := make([]ir.Ref, 0, len(callArgs))
			var argSetup, post []ir.Instr
			var callTo ir.Ref
			var callCls ir.Cls
			var callDefs []ir.Ref
			a := newArgAssigner(false)

			// A MEMORY-class result reserves rdi for the sret buffer pointer, ahead of
			// the arguments.
			retMem := in.RetAgg != nil && classifyAgg(in.RetAgg).memory
			if retMem {
				buf := f.NewTemp("", ir.ClsL)
				out = append(out, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: buf, Args: []ir.Ref{f.Long(int64(classifyAgg(in.RetAgg).size))}})
				pin := newPinned(f, argGP[0], ir.ClsL)
				a.ngrn = 1
				argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: ir.ClsL, To: pin, Args: []ir.Ref{buf}})
				pins = append(pins, pin)
				post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: in.To, Args: []ir.Ref{buf}})
			}

			for k, arg := range callArgs {
				if agg := aggArgAt(&in, k); agg != nil {
					argSetup, pins = lowerAggArg(f, arg, agg, &a, &out, argSetup, pins)
					continue
				}
				cls := f.ClassOf(arg)
				loc := a.assign(cls)
				if loc.onStack {
					argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: cls, To: ir.R, Aux: int64(loc.stacky), Args: []ir.Ref{arg}})
					continue
				}
				pin := newPinned(f, loc.reg, cls)
				argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: cls, To: pin, Args: []ir.Ref{arg}})
				pins = append(pins, pin)
			}

			switch {
			case in.To.IsNone():
				// no result
			case retMem:
				// handled above (result buffer)
			case in.RetAgg != nil:
				callTo, callCls, callDefs, post = lowerAggResultReg(f, in.To, in.RetAgg, &out)
			default:
				pres := newPinned(f, retReg(in.Cls), in.Cls)
				callTo, callCls = pres, in.Cls
				post = append(post, ir.Instr{Op: ir.OCopy, Cls: in.Cls, To: in.To, Args: []ir.Ref{pres}})
			}

			if in.Tail && a.stackBytes() > 0 {
				return fmt.Errorf("amd64: tail call with stack arguments is not yet supported")
			}

			out = append(out, argSetup...)
			out = append(out, ir.Instr{
				Op: ir.OCall, Args: append([]ir.Ref{callee}, pins...),
				Aux: int64(a.stackBytes()), To: callTo, Cls: callCls, Extra: &ir.InstrExtra{Defs: callDefs},
				Tail: in.Tail, ClosureCall: in.ClosureCall, ClosureContext: in.ClosureContext,
				Pos: in.Pos, Inl: in.Inl,
			})
			out = append(out, post...)
		}
		b.Instrs = out
	}
	return nil
}

// aggArgAt returns the aggregate type of the k-th value argument, or nil.
func aggArgAt(in *ir.Instr, k int) *ir.AggType {
	if k < len(in.AggArgs()) {
		return in.AggArgs()[k]
	}
	return nil
}

// lowerAggArg lowers one by-value aggregate call argument. Value computation
// (loads/address arithmetic) is appended to *out; the OArg moves are appended to
// argSetup (returned) so they form one contiguous run before the call.
func lowerAggArg(f *ir.Func, argRef ir.Ref, agg *ir.AggType, a *argAssigner, out *[]ir.Instr, argSetup []ir.Instr, pins []ir.Ref) ([]ir.Instr, []ir.Ref) {
	cls := classifyAgg(agg)
	regs, onStack, off := a.assignAgg(cls)
	if cls.memory || onStack {
		// MEMORY class: copy the aggregate bytes into the outgoing stack area, chunk
		// by chunk, via stacked OArg stores at [rsp+off+coff].
		coff := 0
		emit := func(loadOp ir.Op, elemCls ir.Cls, w int) {
			for cls.size-coff >= w {
				addr := offsetAddr(f, argRef, coff, out)
				v := f.NewTemp("", elemCls)
				*out = append(*out, ir.Instr{Op: loadOp, Cls: elemCls, To: v, Args: []ir.Ref{addr}})
				argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: elemCls, To: ir.R, Aux: int64(off + coff), Args: []ir.Ref{v}})
				coff += w
			}
		}
		emit(ir.OLoadl, ir.ClsL, 8)
		emit(ir.OLoaduw, ir.ClsW, 4)
		emit(ir.OLoaduh, ir.ClsW, 2)
		emit(ir.OLoadub, ir.ClsW, 1)
		return argSetup, pins
	}
	for i, r := range regs {
		addr := offsetAddr(f, argRef, i*8, out)
		elemCls := ir.ClsL
		var val ir.Ref
		if r.float {
			var loadOp ir.Op
			loadOp, _, elemCls = ebOps(true, ebBytes(cls.size, i))
			val = f.NewTemp("", elemCls)
			*out = append(*out, ir.Instr{Op: loadOp, Cls: elemCls, To: val, Args: []ir.Ref{addr}})
		} else {
			val = loadAggInt(f, addr, ebBytes(cls.size, i), out)
		}
		pin := newPinned(f, r.reg, elemCls)
		argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: elemCls, To: pin, Args: []ir.Ref{val}})
		pins = append(pins, pin)
	}
	return argSetup, pins
}

// lowerAggResultReg prepares a call returning a register-class aggregate: the
// returned eightbyte registers are stored into a fresh slot, and the result
// temporary points at it.
func lowerAggResultReg(f *ir.Func, dst ir.Ref, agg *ir.AggType, out *[]ir.Instr) (callTo ir.Ref, callCls ir.Cls, defs []ir.Ref, post []ir.Instr) {
	cls := classifyAgg(agg)
	slot := f.NewTemp("", ir.ClsL)
	*out = append(*out, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(len(cls.parts) * 8))}})
	ni, ns := 0, 0
	for i, part := range cls.parts {
		float := part == ebSSE
		var rr Reg
		elemCls := ir.ClsL
		storeOp := ir.OStorel
		if float {
			_, storeOp, elemCls = ebOps(true, ebBytes(cls.size, i))
			rr, ns = retSSERegs[ns], ns+1
		} else {
			rr, ni = retIntRegs[ni], ni+1 // whole register: full store covers every byte
		}
		pin := newPinned(f, rr, elemCls)
		if i == 0 {
			callTo, callCls = pin, elemCls
		} else {
			defs = append(defs, pin)
		}
		addr := offsetAddr(f, slot, i*8, &post)
		post = append(post, ir.Instr{Op: storeOp, Cls: elemCls, Args: []ir.Ref{pin, addr}})
	}
	post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: dst, Args: []ir.Ref{slot}})
	return callTo, callCls, defs, post
}
