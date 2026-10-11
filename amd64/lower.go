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
//
// conventions is the object-wide callee-convention index (see convention.go);
// it decides which ABI each call site is lowered against. A nil index is legal
// and resolves every unmarked direct call to the platform ABI.
func lower(f *ir.Func, conventions calleeConventions) error {
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
	if err := lowerABI(f, conventions); err != nil {
		return err
	}
	stabilizeClosureContext(f)
	return nil
}

// lowerABI makes the calling convention explicit: parameters are copied out of
// their incoming registers, return values into their result registers, and call
// arguments/results through the argument/result registers -- System V's or Go
// ABIInternal's, per function and per call.
func lowerABI(f *ir.Func, conventions calleeConventions) error {
	if err := lowerTailCalls(f); err != nil {
		return err
	}
	// A System V function returning a MEMORY-class aggregate is given the result
	// buffer's address in rdi (the first integer argument); an ABIInternal one
	// whose result does not fit its registers finds the result area after its
	// stack arguments. Either way, capture it so returns can write there.
	var retBuf ir.Ref
	if f.RetAgg != nil {
		if f.UsesGoInternalCallConvention() {
			resultAssigner := newArgAssigner(true)
			_, onStack, _ := assignGoAggregate(&resultAssigner, f.RetAgg)
			if onStack {
				retBuf = f.NewTemp("retbuf", ir.ClsL)
				f.Temp(retBuf).Agg = f.RetAgg
			}
		} else if classifyAgg(f.RetAgg).memory {
			retBuf = f.NewTemp("retbuf", ir.ClsL)
		}
	}
	if !retBuf.IsNone() && f.UsesManagedFrame() {
		// Under a managed (growable, copied) stack the hidden result buffer points
		// into the caller's stack and must be rewritten if a nested call grows and
		// copies it, so the collector has to track it.
		f.MarkGCRef(retBuf)
	}
	if err := lowerParams(f, retBuf); err != nil {
		return err
	}
	if err := lowerCalls(f, conventions); err != nil {
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
		if call.RetAgg() != nil {
			return fmt.Errorf("amd64: aggregate-returning tail call is not supported")
		}
		b.Jmp = ir.Jmp{Kind: ir.JmpRet} // the callee provides the return value
		call.To = ir.R                  // do not capture the result
	}
	return nil
}

func lowerParams(f *ir.Func, retBuf ir.Ref) error {
	goInternal := f.UsesGoInternalCallConvention()
	a := newArgAssigner(goInternal)
	// Register/stack parameter moves (OPar) must precede aggregate reconstruction,
	// so the emitter can treat the OPar run as one parallel move.
	var pars, recon []ir.Instr
	if !retBuf.IsNone() && !goInternal {
		// The sret pointer arrives in rdi, ahead of the arguments.
		pin := newPinned(f, argGP[0], ir.ClsL)
		a.ngrn = 1
		pars = append(pars, ir.Instr{Op: ir.OPar, Cls: ir.ClsL, To: retBuf, Args: []ir.Ref{pin}})
	}
	for parameterIndex := 0; parameterIndex < len(f.Params); {
		// A grouped parameter is one Go value split into SSA parts. ABIInternal
		// assigns the whole value at once, and so does System V: there a group is
		// passed exactly like a by-value aggregate of its type, which is what lets
		// a caller that holds the value as an aggregate and a callee that takes it
		// as parts agree (see lowerSysVGroupParams).
		if group, ok := ir.ValueGroupAt(f.ParamGroups, parameterIndex); goInternal && ok {
			end := parameterIndex + group.Count
			if end > len(f.Params) {
				return fmt.Errorf("amd64: aggregate parameter group extends beyond parameter list")
			}
			groupParameters, err := lowerGoValueParams(f, f.Params[parameterIndex:end], group.Type, &a)
			if err != nil {
				return err
			}
			pars = append(pars, groupParameters...)
			parameterIndex = end
			continue
		}
		if group, ok := ir.ValueGroupAt(f.ParamGroups, parameterIndex); !goInternal && ok {
			end := parameterIndex + group.Count
			if end > len(f.Params) {
				return fmt.Errorf("amd64: aggregate parameter group extends beyond parameter list")
			}
			ps, rs, err := lowerSysVGroupParams(f, f.Params[parameterIndex:end], group.Type, &a)
			if err != nil {
				return err
			}
			pars = append(pars, ps...)
			recon = append(recon, rs...)
			parameterIndex = end
			continue
		}
		p := f.Params[parameterIndex]
		parameterIndex++
		if p.Agg != nil {
			if goInternal {
				ps, rs := lowerGoAggregateParam(f, p, &a)
				pars = append(pars, ps...)
				recon = append(recon, rs...)
				continue
			}
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
		if p.GCRef {
			f.Temp(pin).GCRef = true
		}
		pars = append(pars, ir.Instr{Op: ir.OPar, Cls: p.Cls, To: p.Ref(), Args: []ir.Ref{pin}})
	}
	if !retBuf.IsNone() && goInternal {
		resultAssigner := newArgAssigner(true)
		resultAssigner.nsaa = roundUp(a.nsaa, 8)
		_, onStack, resultOffset := assignGoAggregate(&resultAssigner, f.RetAgg)
		if !onStack {
			return fmt.Errorf("amd64: internal error: Go ABI result buffer assigned to registers")
		}
		resultPar := ir.Instr{
			Op:  ir.OPar,
			Cls: ir.ClsL,
			To:  retBuf,
			Aux: int64(resultOffset),
		}
		resultPar.SetRetAgg(f.RetAgg)
		pars = append(pars, resultPar)
	}
	f.Start.Instrs = append(append(pars, recon...), f.Start.Instrs...)
	return nil
}

// lowerAggParam handles one by-value aggregate parameter, returning the OPar
// instructions and the reconstruction instructions (register-class aggregates are
// rebuilt into a stack slot; MEMORY/stack aggregates are addressed in place).
func lowerAggParam(f *ir.Func, p *ir.Temp, a *argAssigner) (pars, recon []ir.Instr) {
	// Aggregate parameters are represented by an address: into this frame, or
	// into the incoming argument area. Only a managed (copying) stack must
	// relocate it, and so track it.
	if f.UsesManagedFrame() {
		f.MarkGCRef(p.Ref())
	}
	cls := classifyAgg(p.Agg)
	regs, onStack, off := a.assignAgg(cls)
	if cls.memory || onStack {
		// The aggregate sits in the incoming argument area; the parameter is its
		// address (the emitter leas an aggregate stack param).
		return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Aux: int64(off)}}, nil
	}
	slot := f.NewTemp("", ir.ClsL)
	recon = append(recon, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(len(regs) * 8))}})
	pointerAt := map[int]bool{}
	if f.UsesManagedFrame() {
		f.MarkGCRef(slot)
		f.MarkAggregatePointerWords(slot, p.Agg)
		for _, offset := range ir.AggregatePointerOffsets(p.Agg) {
			pointerAt[offset] = true
		}
	}
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
		if pointerAt[i*8] {
			f.Temp(pin).GCRef = true
		}
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
			var err error
			switch {
			case f.UsesGoInternalCallConvention() && f.RetValues:
				err = lowerGoValueReturn(f, b, retBuf)
			case f.UsesGoInternalCallConvention():
				err = lowerGoAggregateReturn(f, b, retBuf)
			case f.RetValues:
				err = lowerSysVValueReturn(f, b, retBuf)
			default:
				lowerAggReturn(f, b, retBuf)
			}
			if err != nil {
				return err
			}
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

func lowerCalls(f *ir.Func, conventions calleeConventions) error {
	for _, b := range f.Blocks {
		var out []ir.Instr
		for i := range b.Instrs {
			in := b.Instrs[i]
			if in.Op != ir.OCall {
				out = append(out, in)
				continue
			}
			goInternal := conventions.forCall(f, &in) == ir.CallConvGoInternal
			var err error
			if goInternal {
				out, err = lowerGoCall(f, in, out)
			} else {
				out, err = lowerPlatformCall(f, in, out)
			}
			if err != nil {
				return err
			}
		}
		b.Instrs = out
	}
	return nil
}

// lowerPlatformCall lowers one System V call. A grouped argument -- one Go
// value split into SSA parts -- is passed as the by-value aggregate of its type,
// which is how lowerParams receives it.
func lowerPlatformCall(f *ir.Func, in ir.Instr, out []ir.Instr) ([]ir.Instr, error) {
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
	retMem := in.RetAgg() != nil && classifyAgg(in.RetAgg()).memory
	var retBuffer ir.Ref
	if retMem {
		retBuffer = allocResultBuffer(f, in.RetAgg(), classifyAgg(in.RetAgg()).size, &out)
		pin := newPinned(f, argGP[0], ir.ClsL)
		a.ngrn = 1
		argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: ir.ClsL, To: pin, Args: []ir.Ref{retBuffer}})
		pins = append(pins, pin)
	}

	for k := 0; k < len(callArgs); k++ {
		arg := callArgs[k]
		if group, ok := ir.ValueGroupAt(in.ArgGroups(), k); ok {
			end := k + group.Count
			if end > len(callArgs) {
				return nil, fmt.Errorf("amd64: aggregate argument group extends beyond call argument list")
			}
			address, err := materializeGroup(f, callArgs[k:end], group.Type, &out)
			if err != nil {
				return nil, err
			}
			argSetup, pins = lowerAggArg(f, address, group.Type, &a, &out, argSetup, pins)
			k = end - 1
			continue
		}
		if agg := aggArgAt(&in, k); agg != nil {
			argSetup, pins = lowerAggArg(f, arg, agg, &a, &out, argSetup, pins)
			continue
		}
		if arg.Kind == ir.RefAggregate {
			return nil, fmt.Errorf("amd64: ungrouped aggregate call argument %d in %s", k, f.Name)
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
	case retMem && in.RetValues:
		destinations := append([]ir.Ref{in.To}, in.Defs()...)
		loads, err := loadValueParts(f, retBuffer, in.RetAgg(), destinations)
		if err != nil {
			return nil, err
		}
		post = append(post, loads...)
	case retMem:
		post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: in.To, Args: []ir.Ref{retBuffer}})
	case in.RetAgg() != nil && in.RetValues:
		pointer := f.NewTemp("", ir.ClsL)
		callTo, callCls, callDefs, post = lowerAggResultReg(f, pointer, in.RetAgg(), &out)
		destinations := append([]ir.Ref{in.To}, in.Defs()...)
		loads, err := loadValueParts(f, pointer, in.RetAgg(), destinations)
		if err != nil {
			return nil, err
		}
		post = append(post, loads...)
	case in.RetAgg() != nil:
		callTo, callCls, callDefs, post = lowerAggResultReg(f, in.To, in.RetAgg(), &out)
	default:
		pres := newPinned(f, retReg(in.Cls), in.Cls)
		callTo, callCls = pres, in.Cls
		post = append(post, ir.Instr{Op: ir.OCopy, Cls: in.Cls, To: in.To, Args: []ir.Ref{pres}})
	}

	if in.Tail && a.stackBytes() > 0 {
		return nil, fmt.Errorf("amd64: tail call with stack arguments is not yet supported")
	}

	stackBytes := a.stackBytes()
	if f.UsesManagedFrame() && !in.Tail {
		stackBytes = platformCallStackBytes(f, &in)
	}
	out = append(out, argSetup...)
	out = append(out, ir.Instr{
		Op: ir.OCall, Args: append([]ir.Ref{callee}, pins...),
		Aux: int64(stackBytes), To: callTo, Cls: callCls, Extra: &ir.InstrExtra{Defs: callDefs},
		Tail: in.Tail, ClosureCall: in.ClosureCall,
		Pos: in.Pos, Inl: in.Inl,
		CallConv: ir.CallConvPlatform, CallConvSet: true,
	})
	out[len(out)-1].SetClosureContext(in.ClosureContext())
	out = append(out, post...)
	return out, nil
}

// lowerGoCall lowers one ABIInternal call: a closure call, or a call to a
// function the module marks CallConvGoInternal (including every assembly
// function called through its ABIInternal entry).
func lowerGoCall(f *ir.Func, in ir.Instr, out []ir.Instr) ([]ir.Instr, error) {
	callee := in.Args[0]
	callArgs := in.Args[1:]
	pins := make([]ir.Ref, 0, len(callArgs))
	var argSetup []ir.Instr
	a := newArgAssigner(true)
	for argumentIndex := 0; argumentIndex < len(callArgs); {
		if group, ok := ir.ValueGroupAt(in.ArgGroups(), argumentIndex); ok {
			end := argumentIndex + group.Count
			if end > len(callArgs) {
				return nil, fmt.Errorf("amd64: aggregate argument group extends beyond call argument list")
			}
			var err error
			argSetup, pins, err = lowerGoValueArg(f, callArgs[argumentIndex:end], group.Type, &a, argSetup, pins)
			if err != nil {
				return nil, err
			}
			argumentIndex = end
			continue
		}
		arg := callArgs[argumentIndex]
		if agg := aggArgAt(&in, argumentIndex); agg != nil {
			argSetup, pins = lowerGoAggregateArg(f, arg, agg, &a, &out, argSetup, pins)
			argumentIndex++
			continue
		}
		if arg.Kind == ir.RefAggregate {
			return nil, fmt.Errorf("amd64: ungrouped aggregate call argument %d in %s", argumentIndex, f.Name)
		}
		cls := f.ClassOf(arg)
		loc := a.assign(cls)
		if loc.onStack {
			argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: cls, To: ir.R, Aux: int64(loc.stacky), Args: []ir.Ref{arg}})
			argumentIndex++
			continue
		}
		pin := newPinned(f, loc.reg, cls)
		argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: cls, To: pin, Args: []ir.Ref{arg}})
		pins = append(pins, pin)
		argumentIndex++
	}
	if goFloatArgRegSpill(a.nsrn) {
		return nil, fmt.Errorf("amd64: ABIInternal call in %s needs more float argument registers than cg12 assigns", f.Name)
	}

	result := goCallResult{stackEnd: roundUp(a.nsaa, 8)}
	switch {
	case in.To.IsNone():
		// no result
	case in.RetAgg() != nil && in.RetValues:
		destinations := append([]ir.Ref{in.To}, in.Defs()...)
		var err error
		result, err = lowerGoValueResult(f, destinations, in.RetAgg(), result.stackEnd, &out)
		if err != nil {
			return nil, err
		}
	case in.RetAgg() != nil:
		result = lowerGoAggregateResult(f, in.To, in.RetAgg(), result.stackEnd, &out)
	default:
		pres := newPinned(f, retReg(in.Cls), in.Cls)
		result.callTo = pres
		result.callClass = in.Cls
		result.post = []ir.Instr{{Op: ir.OCopy, Cls: in.Cls, To: in.To, Args: []ir.Ref{pres}}}
	}

	if in.Tail {
		return nil, fmt.Errorf("amd64: tail calls under the Go ABI are not supported")
	}
	out = append(out, argSetup...)
	loweredCall := ir.Instr{
		Op: ir.OCall, Args: append([]ir.Ref{callee}, pins...),
		Aux: int64(goCallStackBytes(f, &in, result.stackEnd)), To: result.callTo, Cls: result.callClass,
		Extra:       &ir.InstrExtra{Defs: result.definitions},
		ClosureCall: in.ClosureCall,
		Pos:         in.Pos, Inl: in.Inl,
		CallConv: ir.CallConvGoInternal, CallConvSet: true,
	}
	if !result.stackResult.IsNone() {
		loweredCall.SetRetAgg(in.RetAgg())
		loweredCall.SetStackResult(result.stackResult, int64(result.stackOffset))
	}
	// The closure environment was pinned to RDX by the front end; keeping it on
	// the call is what keeps that copy live up to the call.
	loweredCall.SetClosureContext(in.ClosureContext())
	out = append(out, loweredCall)
	out = append(out, result.post...)
	return out, nil
}

// loadValueParts loads each SSA part of a multi-valued result from the
// aggregate it was returned in.
func loadValueParts(f *ir.Func, pointer ir.Ref, aggregate *ir.AggType, destinations []ir.Ref) ([]ir.Instr, error) {
	parts, flattenable := ir.FlattenAggregate(aggregate)
	if !flattenable || len(parts) != len(destinations) {
		return nil, fmt.Errorf("amd64: System V aggregate result has %d SSA parts, want %d", len(destinations), len(parts))
	}
	var loads []ir.Instr
	for index, part := range parts {
		destination := destinations[index]
		if f.ClassOf(destination) != part.Sub.Cls() {
			return nil, fmt.Errorf("amd64: System V aggregate result part %d has class %s, want %s", index, f.ClassOf(destination), part.Sub.Cls())
		}
		address := offsetAddr(f, pointer, part.Offset, &loads)
		loads = append(loads, ir.Instr{
			Op:   ir.LoadOpForSub(part.Sub),
			Cls:  part.Sub.Cls(),
			To:   destination,
			Args: []ir.Ref{address},
		})
	}
	return loads, nil
}

// lowerSysVValueReturn returns a multi-valued result under System V: the parts
// are stored into a frame aggregate, which is then returned as an ordinary
// aggregate (registers or the caller's buffer).
func lowerSysVValueReturn(f *ir.Func, b *ir.Block, retBuf ir.Ref) error {
	values := append([]ir.Ref{b.Jmp.Arg}, b.Jmp.Args...)
	parts, flattenable := ir.FlattenAggregate(f.RetAgg)
	if !flattenable || len(parts) != len(values) {
		return fmt.Errorf("amd64: System V aggregate return has %d SSA parts, want %d", len(values), len(parts))
	}
	pointer := f.AllocAggregate(f.RetAgg, &b.Instrs)
	for index, part := range parts {
		value := values[index]
		if f.ClassOf(value) != part.Sub.Cls() {
			return fmt.Errorf("amd64: System V aggregate return part %d has class %s, want %s", index, f.ClassOf(value), part.Sub.Cls())
		}
		address := offsetAddr(f, pointer, part.Offset, &b.Instrs)
		b.Instrs = append(b.Instrs, ir.Instr{
			Op:   ir.StoreOpForSub(part.Sub),
			Cls:  part.Sub.Cls(),
			Args: []ir.Ref{value, address},
		})
	}
	b.Jmp.Arg = pointer
	b.Jmp.Args = nil
	lowerAggReturn(f, b, retBuf)
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
	slot := allocResultBuffer(f, agg, len(cls.parts)*8, out)
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

// allocResultBuffer reserves a 16-aligned frame buffer of size bytes for an
// aggregate result. Under a managed frame the buffer is what holds the result's
// pointers across the code that consumes it, so the collector and the stack
// copier have to know where its pointer words are.
func allocResultBuffer(f *ir.Func, aggregate *ir.AggType, size int, out *[]ir.Instr) ir.Ref {
	slot := f.NewTemp("", ir.ClsL)
	*out = append(*out, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(size))}})
	if f.UsesManagedFrame() {
		f.MarkGCRef(slot)
		f.MarkAggregatePointerWords(slot, aggregate)
	}
	return slot
}

// lowerSysVGroupParams receives a grouped parameter -- one Go value split into
// SSA parts -- as System V passes a by-value aggregate of the group's type: its
// eightbytes in registers when they all fit, otherwise the whole value in the
// incoming argument area. Register eightbytes are stored into a frame slot and
// the parts loaded back out of it, which is right for any packing of parts into
// eightbytes; stack-passed parts are read in place.
func lowerSysVGroupParams(f *ir.Func, parameters []*ir.Temp, aggregate *ir.AggType, a *argAssigner) (pars, recon []ir.Instr, err error) {
	parts, flattenable := ir.FlattenAggregate(aggregate)
	if !flattenable || len(parts) != len(parameters) {
		return nil, nil, fmt.Errorf("amd64: aggregate parameter has %d SSA parts, want %d", len(parameters), len(parts))
	}
	cls := classifyAgg(aggregate)
	regs, onStack, off := a.assignAgg(cls)
	if cls.memory || onStack {
		for index, part := range parts {
			parameter := parameters[index]
			pars = append(pars, ir.Instr{Op: ir.OPar, Cls: parameter.Cls, To: parameter.Ref(), Aux: int64(off + part.Offset)})
		}
		return pars, nil, nil
	}
	slot := allocResultBuffer(f, aggregate, len(regs)*8, &recon)
	pointerAt := map[int]bool{}
	for _, offset := range ir.AggregatePointerOffsets(aggregate) {
		pointerAt[offset] = true
	}
	for index, register := range regs {
		address := offsetAddr(f, slot, index*8, &recon)
		if register.float {
			_, storeOp, elemCls := ebOps(true, ebBytes(cls.size, index))
			pin := newPinned(f, register.reg, elemCls)
			recon = append(recon, ir.Instr{Op: storeOp, Cls: elemCls, Args: []ir.Ref{pin, address}})
			continue
		}
		pin := newPinned(f, register.reg, ir.ClsL)
		if pointerAt[index*8] && f.UsesManagedFrame() {
			f.Temp(pin).GCRef = true
		}
		recon = append(recon, ir.Instr{Op: ir.OStorel, Cls: ir.ClsL, Args: []ir.Ref{pin, address}})
	}
	for index, part := range parts {
		parameter := parameters[index]
		address := offsetAddr(f, slot, part.Offset, &recon)
		recon = append(recon, ir.Instr{
			Op:   ir.LoadOpForSub(part.Sub),
			Cls:  parameter.Cls,
			To:   parameter.Ref(),
			Args: []ir.Ref{address},
		})
	}
	return nil, recon, nil
}

// materializeGroup stores a grouped argument's SSA parts into a frame slot laid
// out as the group's aggregate type, so it can be passed as that aggregate.
func materializeGroup(f *ir.Func, arguments []ir.Ref, aggregate *ir.AggType, out *[]ir.Instr) (ir.Ref, error) {
	parts, flattenable := ir.FlattenAggregate(aggregate)
	if !flattenable || len(parts) != len(arguments) {
		return ir.R, fmt.Errorf("amd64: aggregate argument has %d SSA parts, want %d", len(arguments), len(parts))
	}
	size, _ := aggregate.Layout()
	slot := allocResultBuffer(f, aggregate, roundUp(size, 8), out)
	for index, part := range parts {
		address := offsetAddr(f, slot, part.Offset, out)
		*out = append(*out, ir.Instr{
			Op:   ir.StoreOpForSub(part.Sub),
			Cls:  part.Sub.Cls(),
			Args: []ir.Ref{arguments[index], address},
		})
	}
	return slot, nil
}
