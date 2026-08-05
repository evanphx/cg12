package amd64

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
	lowerpass "github.com/evanphx/cg12/lower"
)

// lower rewrites f from SSA into a form ready for register allocation and
// emission: critical edges are split, phis are replaced by copies, and the
// calling convention is made explicit with copies to and from pre-coloured
// physical-register temporaries.
//
// The scalar subset (integer classes w/l and float classes s/d) is handled;
// by-value aggregates, variadics, and tail calls return an explicit error rather
// than emitting silently wrong code.
//
// conventions is the object-wide callee-convention index (see convention.go). It
// decides which ABI each *call site* is lowered against, which is a different
// question from which ABI f's own body is emitted against (emissionConvention):
// goc's closures are ABIInternal functions that make ordinary platform-ABI calls
// out, and ordinary platform-ABI functions are what call them. A nil map is
// legal and means "this object defines no function I can resolve against", under
// which every unmarked direct call is platform ABI.
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
// their incoming registers, return values into the result registers, and call
// arguments/results through the argument/result registers.
//
// Parameters and returns follow *this function's* convention; each call follows
// the convention resolved for its own callee.
func lowerABI(f *ir.Func, conventions calleeConventions) error {
	cc := emissionConvention(f)
	if err := lowerTailCalls(f); err != nil {
		return err
	}
	if err := goInternalSignatureSupported(f, cc); err != nil {
		return err
	}
	// A System V function returning a MEMORY-class aggregate is given the result
	// buffer's address in rdi (the first integer argument); capture it so returns
	// can write there and return the pointer in rax. Go's ABIInternal has no
	// hidden result pointer at all -- a result too large for the registers is
	// placed on the stack by the caller -- so there is no buffer to capture.
	var retBuf ir.Ref
	if cc == ir.CallConvPlatform && f.RetAgg != nil && classifyAgg(f.RetAgg).memory {
		retBuf = f.NewTemp("retbuf", ir.ClsL)
	}
	if err := lowerParams(f, cc, retBuf); err != nil {
		return err
	}
	if err := lowerCalls(f, cc, conventions); err != nil {
		return err
	}
	return lowerReturns(f, cc, retBuf)
}

// goInternalSignatureSupported rejects the ABIInternal signatures this backend
// cannot lower, by name rather than by emitting something plausible.
//
// Two shapes are refused. A result too large for the result registers travels in
// a caller-provided stack slot, which needs the ir.Instr StackResult machinery
// amd64's emitter does not have. And ParamGroups/ArgGroups -- a by-value
// aggregate already split into scalar SSA values -- are not consulted by this
// file, while goabi.go's argument-frame layout does consult them; lowering an
// ABIInternal signature that has them would put the value somewhere the frame
// layout does not expect.
func goInternalSignatureSupported(f *ir.Func, cc ir.CallConvention) error {
	if cc != ir.CallConvGoInternal {
		return nil
	}
	if f.Variadic {
		return fmt.Errorf("amd64: variadic function %q cannot use the Go internal calling convention: "+
			"the vararg register save area and va_arg's scratch register are System V's", f.Name)
	}
	if len(f.ParamGroups) > 0 {
		return fmt.Errorf("amd64: function %q uses the Go internal calling convention with grouped "+
			"aggregate parameters, which this backend does not lower", f.Name)
	}
	if f.RetAgg != nil {
		if err := goInternalResultFitsRegisters(f.RetAgg, fmt.Sprintf("function %q", f.Name)); err != nil {
			return err
		}
	}
	return nil
}

// goInternalResultFitsRegisters reports whether an ABIInternal aggregate result
// is entirely register-assigned.
func goInternalResultFitsRegisters(agg *ir.AggType, what string) error {
	results := newArgAssignerFor(ir.CallConvGoInternal)
	if _, onStack, _ := assignGoAggregate(&results, agg); onStack {
		return fmt.Errorf("amd64: %s returns a Go ABIInternal aggregate that does not fit the result "+
			"registers, so it travels in a caller-provided stack slot, which this backend cannot emit", what)
	}
	return nil
}

// stabilizeClosureContext copies an incoming ABIInternal closure environment out
// of RDX at function entry, and rewrites every reference to it to read the copy.
//
// RDX is triply committed on amd64: Go's ABIInternal delivers the closure
// environment there ("set RDX to point to the closure", asm_amd64.s:2003), it is
// the high half of div/rem and of the widening multiply, and mc_va.go uses it as
// vararg scratch. The register cannot simply be reserved -- the instruction
// encodings need it -- so the pointer is moved out before any of those uses can
// run instead. arm64 does the same for X26, which is volatile across calls
// there; the amd64 reason is different but the shape and the placement are the
// same.
//
// The copy is inserted immediately after the OPar run, which the emitter treats
// as one parallel move, so it is the first ordinary instruction of the function.
// Nothing in that shuffle can have destroyed RDX on the way: ABIInternal does not
// pass arguments in RDX (goArgGP skips it), it is held out of allocation
// (reservedForFixedOps) so no parameter's home is there, and the shuffle's
// cycle-breaking scratch is R12/R13.
func stabilizeClosureContext(function *ir.Func) {
	if !function.HasClosureContext {
		return
	}

	var incoming ir.Ref
	for _, temporary := range function.Temps {
		if temporary != nil && temporary.ClosureContext {
			incoming = temporary.Ref()
			break
		}
	}
	if incoming.IsNone() {
		return
	}

	incomingTemporary := function.Temp(incoming)
	saved := function.NewTemp("closure.saved", incomingTemporary.Cls)
	savedTemporary := function.Temp(saved)
	savedTemporary.GCRef = incomingTemporary.GCRef
	savedTemporary.GCType = incomingTemporary.GCType

	for _, block := range function.Blocks {
		for _, phi := range block.Phis {
			for index, argument := range phi.Args {
				if argument == incoming {
					phi.Args[index] = saved
				}
			}
		}
		for index := range block.Instrs {
			for argumentIndex, argument := range block.Instrs[index].Args {
				if argument == incoming {
					block.Instrs[index].Args[argumentIndex] = saved
				}
			}
			if block.Instrs[index].ClosureContext == incoming {
				block.Instrs[index].ClosureContext = saved
			}
		}
		if block.Jmp.Arg == incoming {
			block.Jmp.Arg = saved
		}
		for index, argument := range block.Jmp.Args {
			if argument == incoming {
				block.Jmp.Args[index] = saved
			}
		}
	}

	copyContext := ir.Instr{Op: ir.OCopy, Cls: incomingTemporary.Cls, To: saved, Args: []ir.Ref{incoming}}
	insertAt := 0
	for insertAt < len(function.Start.Instrs) && function.Start.Instrs[insertAt].Op == ir.OPar {
		insertAt++
	}
	function.Start.Instrs = append(function.Start.Instrs, ir.Instr{})
	copy(function.Start.Instrs[insertAt+1:], function.Start.Instrs[insertAt:])
	function.Start.Instrs[insertAt] = copyContext
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

func lowerParams(f *ir.Func, cc ir.CallConvention, retBuf ir.Ref) error {
	a := newArgAssignerFor(cc)
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
			var ps, rs []ir.Instr
			if cc == ir.CallConvGoInternal {
				ps, rs = lowerGoAggParam(f, p, &a)
			} else {
				ps, rs = lowerAggParam(f, p, &a)
			}
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

// lowerGoAggParam is lowerAggParam under Go's ABIInternal, where an aggregate is
// decomposed by *field* rather than into System V eightbytes: each flattened
// scalar field gets its own argument register, and a value that cannot be placed
// entirely in registers goes entirely on the stack (assignGoAggregate).
//
// A register-assigned aggregate is rebuilt into a stack slot, field by field at
// its own offset and width, and the parameter becomes the slot's address -- the
// same representation the System V path produces, so nothing downstream has to
// know which convention placed it.
func lowerGoAggParam(f *ir.Func, p *ir.Temp, a *argAssigner) (pars, recon []ir.Instr) {
	parts, onStack, off := assignGoAggregate(a, p.Agg)
	if onStack {
		// The aggregate sits in the incoming argument area; the parameter is its
		// address (the emitter leas an aggregate stack param).
		return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Aux: int64(off)}}, nil
	}
	size, _ := p.Agg.Layout()
	slot := f.NewTemp("", ir.ClsL)
	recon = append(recon, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(size))}})
	for _, part := range parts {
		addr := offsetAddr(f, slot, part.offset, &recon)
		pin := newPinned(f, part.reg, part.sub.Cls())
		recon = append(recon, ir.Instr{Op: ir.StoreOpForSub(part.sub), Cls: part.sub.Cls(), Args: []ir.Ref{pin, addr}})
	}
	recon = append(recon, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: p.Ref(), Args: []ir.Ref{slot}})
	return nil, recon
}

func lowerReturns(f *ir.Func, cc ir.CallConvention, retBuf ir.Ref) error {
	for _, b := range f.Blocks {
		if b.Jmp.Kind != ir.JmpRet || b.Jmp.Arg.IsNone() {
			continue
		}
		if f.RetAgg != nil {
			if cc == ir.CallConvGoInternal {
				lowerGoAggReturn(f, b)
			} else {
				lowerAggReturn(f, b, retBuf)
			}
			continue
		}
		v := b.Jmp.Arg
		cls := f.ClassOf(v)
		pin := newPinned(f, retRegFor(cc, cls), cls)
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

// lowerGoAggReturn returns an aggregate under Go's ABIInternal: each flattened
// field is loaded from the result pointer into its own result register. The
// result banks restart from zero, so the registers are goArgGP/goArgFP from the
// beginning regardless of what the parameters consumed.
//
// A result too large for the registers was refused up front
// (goInternalSignatureSupported), so every field here has a register.
func lowerGoAggReturn(f *ir.Func, b *ir.Block) {
	ptr := b.Jmp.Arg
	results := newArgAssignerFor(ir.CallConvGoInternal)
	parts, _, _ := assignGoAggregate(&results, f.RetAgg)
	var pins []ir.Ref
	for _, part := range parts {
		addr := offsetAddr(f, ptr, part.offset, &b.Instrs)
		val := f.NewTemp("", part.sub.Cls())
		b.Instrs = append(b.Instrs, ir.Instr{Op: ir.LoadOpForSub(part.sub), Cls: part.sub.Cls(), To: val, Args: []ir.Ref{addr}})
		pin := newPinned(f, part.reg, part.sub.Cls())
		b.Instrs = append(b.Instrs, ir.Instr{Op: ir.OCopy, Cls: part.sub.Cls(), To: pin, Args: []ir.Ref{val}})
		pins = append(pins, pin)
	}
	b.Jmp.Arg = pins[0]
	b.Jmp.Args = pins[1:]
}

func lowerCalls(f *ir.Func, cc ir.CallConvention, conventions calleeConventions) error {
	for _, b := range f.Blocks {
		var out []ir.Instr
		for i := range b.Instrs {
			in := b.Instrs[i]
			if in.Op != ir.OCall {
				out = append(out, in)
				continue
			}
			// A call's convention comes from its callee, never from the enclosing
			// function: a platform-ABI function calling a closure and an ABIInternal
			// closure calling an ordinary function are both routine in goc output, and
			// System V and ABIInternal share no argument register at all.
			callCC := conventions.forCall(f, &in)
			goInternal := callCC == ir.CallConvGoInternal
			if err := goInternalCallSupported(f, &in, callCC); err != nil {
				return err
			}
			callee := in.Args[0]
			callArgs := in.Args[1:]
			pins := make([]ir.Ref, 0, len(callArgs))
			var argSetup, post []ir.Instr
			var callTo ir.Ref
			var callCls ir.Cls
			var callDefs []ir.Ref
			a := newArgAssignerFor(callCC)

			// A MEMORY-class result reserves rdi for the sret buffer pointer, ahead of
			// the arguments. ABIInternal has no such hidden pointer.
			retMem := !goInternal && in.RetAgg != nil && classifyAgg(in.RetAgg).memory
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
					if goInternal {
						argSetup, pins = lowerGoAggArg(f, arg, agg, &a, &out, argSetup, pins)
					} else {
						argSetup, pins = lowerAggArg(f, arg, agg, &a, &out, argSetup, pins)
					}
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

			// Everything the caller places on the stack ends here: the stack-passed
			// arguments, and (ABIInternal only) a stack-assigned result after them.
			resultEnd := roundUp(a.nsaa, ir.PointerWordBytes)

			switch {
			case in.To.IsNone():
				// no result
			case retMem:
				// handled above (result buffer)
			case in.RetAgg != nil && goInternal:
				callTo, callCls, callDefs, post = lowerGoAggResult(f, in.To, in.RetAgg, &out)
			case in.RetAgg != nil:
				callTo, callCls, callDefs, post = lowerAggResultReg(f, in.To, in.RetAgg, &out)
			default:
				pres := newPinned(f, retRegFor(callCC, in.Cls), in.Cls)
				callTo, callCls = pres, in.Cls
				post = append(post, ir.Instr{Op: ir.OCopy, Cls: in.Cls, To: in.To, Args: []ir.Ref{pres}})
			}

			if in.Tail {
				if callCC != cc {
					return fmt.Errorf("amd64: cannot tail-call across calling conventions in function %q", f.Name)
				}
				if a.stackBytes() > 0 {
					return fmt.Errorf("amd64: tail call with stack arguments is not yet supported")
				}
			}

			if err := checkCallScratchCollision(f, cc, callCC, pins); err != nil {
				return err
			}

			// An ABIInternal callee preserves nothing, which a System V body does not
			// otherwise know. Saying so as extra Defs is what keeps the allocator and
			// the caller-save pass from parking a live value in RBX or R12..R15 across
			// the call. See crossConventionClobbers.
			for _, r := range crossConventionClobbers(cc, callCC) {
				callDefs = append(callDefs, newPinned(f, r, ir.ClsL))
			}

			stackBytes, err := callAreaFor(f, &in, callCC, &a, resultEnd)
			if err != nil {
				return err
			}

			out = append(out, argSetup...)
			out = append(out, ir.Instr{
				Op: ir.OCall, Args: append([]ir.Ref{callee}, pins...),
				Aux: int64(stackBytes), To: callTo, Cls: callCls, Defs: callDefs,
				Tail: in.Tail, ClosureCall: in.ClosureCall, ClosureContext: in.ClosureContext,
				Pos: in.Pos, Inl: in.Inl,
				CallConv: callCC, CallConvSet: true,
			})
			out = append(out, post...)
		}
		b.Instrs = out
	}
	return nil
}

// goInternalCallSupported rejects the ABIInternal call shapes this backend
// cannot lower, mirroring goInternalSignatureSupported on the call side.
func goInternalCallSupported(f *ir.Func, call *ir.Instr, callCC ir.CallConvention) error {
	if callCC != ir.CallConvGoInternal {
		return nil
	}
	if len(call.ArgGroups) > 0 {
		return fmt.Errorf("amd64: Go internal call in function %q has grouped aggregate arguments, "+
			"which this backend does not lower", f.Name)
	}
	if call.RetAgg != nil {
		return goInternalResultFitsRegisters(call.RetAgg, fmt.Sprintf("a Go internal call in function %q", f.Name))
	}
	return nil
}

// checkCallScratchCollision refuses a call whose argument registers include the
// *caller's* emitter scratch pair.
//
// The scratch pair is a property of the body being emitted, not of the call, and
// the emitter reaches for it while staging the outgoing arguments -- to break a
// cycle in the parallel move, to route a memory-to-memory move, and to hold an
// indirect callee. Under one convention that is always safe, because neither
// convention's scratch pair is one of its own argument registers. Across
// conventions it is not: System V's scratch pair R10/R11 is ABIInternal's
// argument registers 8 and 9, so a System V function calling a closure with nine
// integer arguments would have argument 8 destroyed while argument 7 was being
// placed. The case needs per-call scratch to fix and is refused by name instead.
func checkCallScratchCollision(f *ir.Func, cc, callCC ir.CallConvention, pins []ir.Ref) error {
	if cc == callCC {
		return nil
	}
	scratch := scratchRegsFor(cc)
	for _, pin := range pins {
		r := Reg(f.Temp(pin).Reg)
		if r == scratch.gpScratch0 || r == scratch.gpScratch1 || r == scratch.fpScratch0 || r == scratch.fpScratch1 {
			return fmt.Errorf("amd64: cross-convention call in function %q passes an argument in %v, "+
				"which is this function's emitter scratch register", f.Name, r)
		}
	}
	return nil
}

// callAreaFor returns the outgoing-argument area one call site must reserve.
//
// Three answers, and the axis they turn on is not the same one: an ABIInternal
// call reserves home slots for its register arguments because the callee's
// stack-growth prologue spills them there, and so does a *managed* platform-ABI
// call -- goc emits managed-frame System V helpers, whose arguments are placed by
// System V's rules but still need somewhere for morestack to find them. Only an
// unmanaged System V call gets the plain stacked-argument size. Using
// argAssigner.stackBytes for the managed case (as this did before B1) puts the
// callee's home slots outside the area the caller reserved.
func callAreaFor(f *ir.Func, call *ir.Instr, callCC ir.CallConvention, a *argAssigner, resultEnd int) (int, error) {
	if callCC == ir.CallConvGoInternal {
		return goCallStackBytes(f, call, resultEnd)
	}
	if f.UsesManagedFrame() {
		return platformCallStackBytes(f, call)
	}
	return a.stackBytes(), nil
}

// aggArgAt returns the aggregate type of the k-th value argument, or nil.
func aggArgAt(in *ir.Instr, k int) *ir.AggType {
	if k < len(in.AggArgs) {
		return in.AggArgs[k]
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

// lowerGoAggArg is lowerAggArg under Go's ABIInternal: an aggregate is
// decomposed by field, each field loaded at its own offset and width into its own
// argument register, and a value that does not fit entirely in registers is
// copied whole into the outgoing area at the offset assignGoAggregate chose.
func lowerGoAggArg(f *ir.Func, argRef ir.Ref, agg *ir.AggType, a *argAssigner, out *[]ir.Instr, argSetup []ir.Instr, pins []ir.Ref) ([]ir.Instr, []ir.Ref) {
	parts, onStack, off := assignGoAggregate(a, agg)
	if onStack {
		size, _ := agg.Layout()
		coff := 0
		emit := func(loadOp ir.Op, elemCls ir.Cls, w int) {
			for size-coff >= w {
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
	for _, part := range parts {
		addr := offsetAddr(f, argRef, part.offset, out)
		val := f.NewTemp("", part.sub.Cls())
		*out = append(*out, ir.Instr{Op: ir.LoadOpForSub(part.sub), Cls: part.sub.Cls(), To: val, Args: []ir.Ref{addr}})
		pin := newPinned(f, part.reg, part.sub.Cls())
		argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: part.sub.Cls(), To: pin, Args: []ir.Ref{val}})
		pins = append(pins, pin)
	}
	return argSetup, pins
}

// lowerGoAggResult is lowerAggResultReg under Go's ABIInternal: the returned
// field registers are stored into a fresh slot at their own offsets, and the
// result temporary points at it. The result banks restart from zero, so the
// registers are the argument registers from the beginning.
//
// A result too large for the registers was refused up front
// (goInternalCallSupported), so every field here has a register.
func lowerGoAggResult(f *ir.Func, dst ir.Ref, agg *ir.AggType, out *[]ir.Instr) (callTo ir.Ref, callCls ir.Cls, defs []ir.Ref, post []ir.Instr) {
	size, _ := agg.Layout()
	results := newArgAssignerFor(ir.CallConvGoInternal)
	parts, _, _ := assignGoAggregate(&results, agg)
	slot := f.NewTemp("", ir.ClsL)
	*out = append(*out, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(size))}})
	for index, part := range parts {
		pin := newPinned(f, part.reg, part.sub.Cls())
		if index == 0 {
			callTo, callCls = pin, part.sub.Cls()
		} else {
			defs = append(defs, pin)
		}
		addr := offsetAddr(f, slot, part.offset, &post)
		post = append(post, ir.Instr{Op: ir.StoreOpForSub(part.sub), Cls: part.sub.Cls(), Args: []ir.Ref{pin, addr}})
	}
	post = append(post, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: dst, Args: []ir.Ref{slot}})
	return callTo, callCls, defs, post
}
