package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// lower rewrites f from SSA into a form ready for register allocation and
// emission: critical edges are split, phis are replaced by copies, and the
// AAPCS64 calling convention is made explicit with copies to and from
// pre-coloured physical-register temporaries.
//
// Only the integer subset (classes w and l) is handled; anything outside it
// returns an explicit error rather than emitting silently wrong code.
func lower(f *ir.Func) error {
	splitCriticalEdges(f)
	destructSSA(f)
	return lowerABI(f)
}

// argLoc is where one AAPCS64 argument is passed: either a physical register or
// a stack slot at the given byte offset in the outgoing/incoming argument area.
type argLoc struct {
	reg     Reg
	onStack bool
	stacky  int // byte offset when onStack
}

// argAssigner walks a sequence of argument classes and assigns each to a
// register or, once a bank is exhausted, to the stack. Integer and
// floating-point arguments consume independent register banks (x0..x7, v0..v7).
type argAssigner struct {
	ngrn, nsrn int
	nsaa       int // next stacked-argument byte offset
}

func (a *argAssigner) assign(cls ir.Cls) argLoc {
	if cls.IsFloat() {
		if a.nsrn < 8 {
			r := vReg(a.nsrn)
			a.nsrn++
			return argLoc{reg: r}
		}
	} else if a.ngrn < 8 {
		r := Reg(int(X0) + a.ngrn)
		a.ngrn++
		return argLoc{reg: r}
	}
	off := a.nsaa
	a.nsaa += 8 // scalars occupy one 8-byte stack slot
	return argLoc{onStack: true, stacky: off}
}

// stackBytes returns the 16-aligned size of the stacked-argument area.
func (a *argAssigner) stackBytes() int { return roundUp(a.nsaa, 16) }

// retReg returns the register a value of the given class is returned in.
func retReg(cls ir.Cls) Reg {
	if cls.IsFloat() {
		return V0
	}
	return X0
}

// newPinned creates a fresh temporary hard-bound to physical register r.
func newPinned(f *ir.Func, r Reg, cls ir.Cls) ir.Ref {
	ref := f.NewTemp(fmt.Sprintf("R%s", r.xName()), cls)
	t := f.Temp(ref)
	t.Fixed = true
	t.Reg = int(r)
	return ref
}

// splitCriticalEdges inserts a block on every edge from a block with multiple
// successors to a block with multiple predecessors, so phi copies always have a
// dedicated home.
func splitCriticalEdges(f *ir.Func) {
	analysis.BuildCFG(f) // fills Preds
	// Snapshot the block list; we append split blocks as we go.
	blocks := append([]*ir.Block(nil), f.Blocks...)
	for _, u := range blocks {
		if u.Jmp.Kind != ir.JmpJnz {
			continue
		}
		if u.Jmp.To == u.Jmp.To2 {
			continue // degenerate: both edges to the same block
		}
		for _, edge := range []**ir.Block{&u.Jmp.To, &u.Jmp.To2} {
			v := *edge
			if len(v.Preds) < 2 {
				continue
			}
			s := f.NewBlock(u.Name + "_" + v.Name + "_edge")
			s.Goto(v)
			*edge = s
			// Redirect phi sources in v from u to the new split block.
			for _, p := range v.Phis {
				for k, b := range p.Blocks {
					if b == u {
						p.Blocks[k] = s
					}
				}
			}
		}
	}
}

// destructSSA replaces phi nodes with copies at the end of each predecessor,
// resolving the simultaneous assignment on each edge into a safe move sequence.
func destructSSA(f *ir.Func) {
	for _, v := range f.Blocks {
		if len(v.Phis) == 0 {
			continue
		}
		// Group phi (dst <- src) pairs by predecessor edge.
		perPred := map[*ir.Block][]movePair{}
		var order []*ir.Block
		for _, p := range v.Phis {
			for k, pred := range p.Blocks {
				if _, ok := perPred[pred]; !ok {
					order = append(order, pred)
				}
				perPred[pred] = append(perPred[pred], movePair{dst: p.To, src: p.Args[k]})
			}
		}
		for _, pred := range order {
			seq := sequentializeCopies(f, perPred[pred])
			for _, mv := range seq {
				pred.Instrs = append(pred.Instrs, ir.Instr{
					Op:   ir.OCopy,
					Cls:  f.ClassOf(mv.dst),
					To:   mv.dst,
					Args: []ir.Ref{mv.src},
				})
			}
		}
		v.Phis = nil
	}
}

type movePair struct{ dst, src ir.Ref }

// sequentializeCopies turns a set of parallel copies (all dsts distinct) into an
// ordered sequence with the same effect, breaking cycles with a fresh temp.
func sequentializeCopies(f *ir.Func, pairs []movePair) []movePair {
	var work []movePair
	for _, p := range pairs {
		if p.dst != p.src {
			work = append(work, p)
		}
	}
	var out []movePair
	for len(work) > 0 {
		// Prefer a copy whose destination is not still needed as a source.
		idx := -1
		for i, p := range work {
			needed := false
			for _, q := range work {
				if q.src == p.dst {
					needed = true
					break
				}
			}
			if !needed {
				idx = i
				break
			}
		}
		if idx >= 0 {
			out = append(out, work[idx])
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		// Every remaining destination is still read: break a cycle by saving one
		// value into a fresh temporary and rerouting its readers.
		p := work[0]
		tmp := f.NewTemp("", f.ClassOf(p.dst))
		out = append(out, movePair{dst: tmp, src: p.dst})
		for i := range work {
			if work[i].src == p.dst {
				work[i].src = tmp
			}
		}
	}
	return out
}

// lowerABI makes the calling convention explicit: parameters are copied out of
// their incoming registers, return values into x0, and call arguments/results
// through the argument/result registers.
func lowerABI(f *ir.Func) error {
	if err := lowerTailCalls(f); err != nil {
		return err
	}
	// A function returning a > 16-byte aggregate is given the result buffer's
	// address in x8; capture it at entry so the returns can write there.
	var retBuf ir.Ref
	if f.RetAgg != nil && classifyAgg(f.RetAgg).kind == aggMemory {
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

// lowerTailCalls prepares tail-call blocks: the callee returns the function's
// result directly, so the block's own return move is dropped and the call's
// result is left uncaptured. The actual tail branch (frame teardown + b) is
// emitted later. A tail-marked call anywhere but in tail position is rejected.
func lowerTailCalls(f *ir.Func) error {
	for _, b := range f.Blocks {
		call, ok := ir.TailCall(b)
		if !ok {
			if ir.HasTailCall(b) {
				return fmt.Errorf("arm64: tail call must be the last instruction and have its result returned")
			}
			continue
		}
		if call.RetAgg != nil {
			return fmt.Errorf("arm64: aggregate-returning tail call is not supported")
		}
		b.Jmp = ir.Jmp{Kind: ir.JmpRet} // the callee provides the return value
		call.To = ir.R                  // do not capture the result
	}
	return nil
}

func lowerParams(f *ir.Func, retBuf ir.Ref) error {
	var a argAssigner
	// Register/stack parameter moves (OPar) must precede any aggregate
	// reconstruction, so the emitter can treat the OPar run as one parallel move.
	var pars, recon []ir.Instr
	if !retBuf.IsNone() {
		// The indirect-result pointer arrives in x8, alongside the arguments.
		pin := newPinned(f, X8, ir.ClsL)
		pars = append(pars, ir.Instr{Op: ir.OPar, Cls: ir.ClsL, To: retBuf, Args: []ir.Ref{pin}})
	}
	for _, p := range f.Params {
		if p.Agg != nil {
			ps, rs, err := lowerAggParam(f, p, &a)
			if err != nil {
				return err
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
	prefix := append(pars, recon...)
	f.Start.Instrs = append(prefix, f.Start.Instrs...)
	return nil
}

// lowerAggParam handles one by-value aggregate parameter, returning the OPar
// instructions (for a Memory-class pointer) and the reconstruction instructions
// (for register-class aggregates, which are rebuilt into a stack slot).
func lowerAggParam(f *ir.Func, p *ir.Temp, a *argAssigner) (pars, recon []ir.Instr, err error) {
	cls := classifyAgg(p.Agg)
	if cls.kind == aggMemory {
		// Passed by reference: the incoming value is already a pointer.
		loc := a.assign(ir.ClsL)
		if loc.onStack {
			return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Aux: int64(loc.stacky)}}, nil, nil
		}
		pin := newPinned(f, loc.reg, ir.ClsL)
		return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Args: []ir.Ref{pin}}}, nil, nil
	}

	var regs []Reg
	var elemCls ir.Cls
	var elemSize int
	var onStack bool
	var off int
	if cls.kind == aggGP {
		regs, onStack, off = a.assignGP(cls.nregs, cls.size)
		elemCls, elemSize = ir.ClsL, 8
	} else {
		regs, onStack, off = a.assignHFA(cls.nregs, cls.size)
		elemCls, elemSize = cls.elem, cls.elem.Size()
	}
	if onStack {
		// The aggregate sits in the incoming argument area; the parameter is its
		// address (emitStackParam takes the address for aggregate temps).
		return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Aux: int64(off)}}, nil, nil
	}

	// Reconstruct: allocate a slot, store each incoming register into it, and
	// make the parameter point at the slot.
	slot := f.NewTemp("", ir.ClsL)
	recon = append(recon, ir.Instr{Op: ir.OAlloc16, Cls: ir.ClsL, To: slot, Args: []ir.Ref{f.Long(int64(len(regs) * elemSize))}})
	for i, r := range regs {
		pin := newPinned(f, r, elemCls)
		addr := slot
		if i > 0 {
			tmp := f.NewTemp("", ir.ClsL)
			recon = append(recon, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: tmp, Args: []ir.Ref{slot, f.Long(int64(i * elemSize))}})
			addr = tmp
		}
		recon = append(recon, ir.Instr{Op: storeOpFor(elemCls), Cls: elemCls, Args: []ir.Ref{pin, addr}})
	}
	recon = append(recon, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: p.Ref(), Args: []ir.Ref{slot}})
	return nil, recon, nil
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

// lowerAggReturn returns an aggregate: a Memory-class result is copied into the
// caller's buffer (x8) and the function returns void; a register-class result is
// loaded from the pointer into the return registers.
func lowerAggReturn(f *ir.Func, b *ir.Block, retBuf ir.Ref) {
	ptr := b.Jmp.Arg
	cls := classifyAgg(f.RetAgg)
	if cls.kind == aggMemory {
		emitMemcpy(f, retBuf, ptr, cls.size, &b.Instrs)
		b.Jmp = ir.Jmp{Kind: ir.JmpRet}
		return
	}

	var base Reg
	var elemCls ir.Cls
	var elemSize int
	if cls.kind == aggGP {
		base, elemCls, elemSize = X0, ir.ClsL, 8
	} else {
		base, elemCls, elemSize = V0, cls.elem, cls.elem.Size()
	}
	var pins []ir.Ref
	for i := 0; i < cls.nregs; i++ {
		addr := offsetAddr(f, ptr, i*elemSize, &b.Instrs)
		val := f.NewTemp("", elemCls)
		b.Instrs = append(b.Instrs, ir.Instr{Op: loadOpFor(elemCls), Cls: elemCls, To: val, Args: []ir.Ref{addr}})
		pin := newPinned(f, base+Reg(i), elemCls)
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
			var argSetup []ir.Instr // the OArg run, emitted after value computation
			var a argAssigner
			for k, arg := range callArgs {
				if agg := aggArgAt(&in, k); agg != nil {
					var err error
					argSetup, pins, err = lowerAggArg(f, arg, agg, &a, &out, argSetup, pins)
					if err != nil {
						return err
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
			// Result handling: build the call's To/Defs and the post-call
			// instructions, possibly adding an x8 setup to the argument run.
			var callTo ir.Ref
			var callCls ir.Cls
			var callDefs []ir.Ref
			var post []ir.Instr
			switch {
			case in.To.IsNone():
				// no result
			case in.RetAgg != nil:
				callTo, callCls, callDefs, argSetup, pins, post =
					lowerAggResult(f, in.To, in.RetAgg, &a, &out, argSetup, pins)
			default:
				pres := newPinned(f, retReg(in.Cls), in.Cls)
				callTo, callCls = pres, in.Cls
				post = []ir.Instr{{Op: ir.OCopy, Cls: in.Cls, To: in.To, Args: []ir.Ref{pres}}}
			}

			if in.Tail {
				// A tail call writes its stack arguments into this function's own
				// incoming-argument area (reused after the frame is torn down), so
				// the callee's stack args must fit in the space our caller gave us.
				_, _, inStack := computeNamedCounts(f)
				if incoming := roundUp(inStack, 16); a.stackBytes() > incoming {
					return fmt.Errorf("arm64: tail call needs %d bytes of stack arguments but the caller provides only %d; cannot tail-call", a.stackBytes(), incoming)
				}
			}
			out = append(out, argSetup...)
			out = append(out, ir.Instr{
				Op: ir.OCall, Args: append([]ir.Ref{callee}, pins...),
				Aux: int64(a.stackBytes()), To: callTo, Cls: callCls, Defs: callDefs,
				Tail: in.Tail, Pos: in.Pos, Inl: in.Inl,
			})
			out = append(out, post...)
		}
		b.Instrs = out
	}
	return nil
}
