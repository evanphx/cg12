package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
	lowerpass "github.com/evanphx/cg12/lower"
)

// lower rewrites f from SSA into a form ready for register allocation and
// emission: critical edges are split, phis are replaced by copies, and the
// AAPCS64 calling convention is made explicit with copies to and from
// pre-coloured physical-register temporaries.
//
// Only the integer subset (classes w and l) is handled; anything outside it
// returns an explicit error rather than emitting silently wrong code.
func lower(f *ir.Func, tlsModel TLSModel) error {
	if err := f.MarkLowered("arm64"); err != nil {
		return err
	}
	lowerpass.JumpTables(f) // dense switches -> indexed branch (JmpTable)
	lowerpass.Switches(f)   // remaining multiway branches -> conditional branches
	// Before the folds: they rewrite addressing, and a thread-local is not an
	// address they can reason about.
	lowerTLS(f, tlsModel)
	lowerpass.HoistAllocas(f)
	foldIdioms(f)
	foldAddressing(f)
	lowerpass.SplitCriticalEdges(f)
	lowerpass.CoalescePhis(f)
	lowerpass.DestructSSA(f)
	return lowerABI(f)
}

// foldAddressing folds a load/store's address computation into an AArch64
// register-offset addressing mode. It matches
//
//	off = shl(sext(i), k) ; a = add(base, off) ; load/store [a]
//
// (with the sext and shift optional, and k matching the access width for a
// scaled index) and rewrites the memory op to carry (base, index) plus the
// extend/scale in Amode, dropping the now-dead address arithmetic. A load then
// carries [base, index]; a store carries [value, base, index].
func foldAddressing(f *ir.Func) {
	uses, defOf := defUse(f)
	single := func(r ir.Ref) bool { return r.Kind == ir.RefTemp && uses[r.ID] == 1 }

	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			var ai int
			switch {
			case in.Op.IsLoad():
				ai = 0
			case in.Op.IsStore():
				ai = 1
			default:
				continue
			}
			access := accessBytes(in.Op)
			if access == 0 || in.Cls.IsFloat() { // skip FP and 128-bit
				continue
			}
			addr := in.Args[ai]
			if !single(addr) {
				continue
			}
			add := defOf(addr)
			if add == nil || add.Op != ir.OAdd {
				continue
			}
			// A constant offset that fits the scaled unsigned immediate becomes
			// [base, #imm] with no index register at all.
			if c, cbase, ok := immOffset(f, add, access); ok {
				nop(add)
				in.Aux = c
				if in.Op.IsLoad() {
					in.Args = []ir.Ref{cbase}
				} else {
					in.Args = []ir.Ref{in.Args[0], cbase}
				}
				continue
			}
			base, off := add.Args[0], add.Args[1]
			// The index side is whichever operand is a shift or sign-extension; a
			// plain add folds too (base + index, no scale).
			if isIndexExpr(defOf(add.Args[0])) && !isIndexExpr(defOf(add.Args[1])) {
				base, off = add.Args[1], add.Args[0]
			}

			option, s := a64.ExtLSL, uint32(0)
			index := off
			// Peel a scale shift when it matches the access width.
			if sd := defOf(index); sd != nil && sd.Op == ir.OShl && single(index) {
				if sh, ok := intConst(f, sd.Args[1]); ok && sh == log2Bytes(access) && access > 1 {
					s = 1
					index = sd.Args[0]
					nop(sd)
				}
			}
			// Peel a 32-bit sign extension into an SXTW index.
			if ed := defOf(index); ed != nil && ed.Op == ir.OExtsw && single(index) {
				option = a64.ExtSXTW
				index = ed.Args[0]
				nop(ed)
			}

			nop(add)
			in.Amode = int32(option)<<1 | int32(s)
			if in.Op.IsLoad() {
				in.Args = []ir.Ref{base, index}
			} else {
				in.Args = []ir.Ref{in.Args[0], base, index}
			}
		}
	}
}

// immOffset reports whether one operand of add is a constant that fits an
// AArch64 scaled unsigned load/store offset for an access of the given width: a
// non-negative multiple of the width, at most 4095 of them. It returns the byte
// offset and the other operand (the base).
func immOffset(f *ir.Func, add *ir.Instr, access int) (int64, ir.Ref, bool) {
	for i := 0; i < 2; i++ {
		if c, ok := intConst(f, add.Args[i]); ok {
			if c >= 0 && c%int64(access) == 0 && c/int64(access) <= 4095 {
				return c, add.Args[1-i], true
			}
		}
	}
	return 0, ir.Ref{}, false
}

// isIndexExpr reports whether an instruction looks like the index side of an
// address (a scale shift or a sign extension).
func isIndexExpr(d *ir.Instr) bool {
	return d != nil && (d.Op == ir.OShl || d.Op == ir.OExtsw)
}

// accessBytes returns the memory-access width in bytes of a load/store op, or 0
// for the 128-bit ops this fold skips.
func accessBytes(op ir.Op) int {
	switch op {
	case ir.OLoadub, ir.OLoadsb, ir.OStoreb:
		return 1
	case ir.OLoaduh, ir.OLoadsh, ir.OStoreh:
		return 2
	case ir.OLoaduw, ir.OLoadsw, ir.OStorew, ir.OLoads:
		return 4
	case ir.OLoadl, ir.OStorel, ir.OLoadd:
		return 8
	}
	return 0
}

func log2Bytes(n int) int64 {
	switch n {
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	}
	return 0
}

// defUse returns, for f, a use count per temporary and a lookup from a ref to
// its defining instruction — enough to recognize and rewrite small multi-
// instruction idioms while still in SSA form.
func defUse(f *ir.Func) (uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	uses = map[uint32]int{}
	def := map[uint32]*ir.Instr{}
	mark := func(r ir.Ref) {
		if r.Kind == ir.RefTemp {
			uses[r.ID]++
		}
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				mark(a)
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			for _, a := range in.Args {
				mark(a)
			}
			if in.To.Kind == ir.RefTemp {
				def[in.To.ID] = in
			}
		}
		mark(b.Jmp.Arg)
		for _, a := range b.Jmp.Args {
			mark(a)
		}
	}
	return uses, func(r ir.Ref) *ir.Instr {
		if r.Kind != ir.RefTemp {
			return nil
		}
		return def[r.ID]
	}
}

// foldIdioms rewrites the shift-or rotate and and-not (bic) idioms into the
// single ORotr / OBic ops the emitters lower to one AArch64 instruction.
func foldIdioms(f *ir.Func) {
	uses, defOf := defUse(f)
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			switch in.Op {
			case ir.OOr:
				foldRotate(f, in, uses, defOf)
			case ir.OAnd:
				foldBic(f, in, uses, defOf)
			case ir.OAdd, ir.OSub:
				foldMulAdd(f, in, uses, defOf)
			}
		}
	}
}

// foldMulAdd fuses an integer multiply feeding an add or subtract into a single
// OMAdd/OMSub (AArch64 madd/msub: rd = ra + rn*rm and rd = ra - rn*rm), when the
// multiply feeds only this op so dropping it changes nothing else. A subtract
// fuses only when the multiply is the subtrahend, since msub negates the product.
func foldMulAdd(f *ir.Func, in *ir.Instr, uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	if in.Cls.IsFloat() {
		return
	}
	mulOf := func(r ir.Ref) *ir.Instr {
		if r.Kind != ir.RefTemp || uses[r.ID] != 1 {
			return nil
		}
		if d := defOf(r); d != nil && d.Op == ir.OMul && d.Cls == in.Cls {
			return d
		}
		return nil
	}
	fuse := func(op ir.Op, m *ir.Instr, addend ir.Ref) {
		in.Op = op
		in.Args = []ir.Ref{m.Args[0], m.Args[1], addend}
		nop(m)
	}
	if in.Op == ir.OAdd {
		if m := mulOf(in.Args[0]); m != nil {
			fuse(ir.OMAdd, m, in.Args[1])
		} else if m := mulOf(in.Args[1]); m != nil {
			fuse(ir.OMAdd, m, in.Args[0])
		}
		return
	}
	if m := mulOf(in.Args[1]); m != nil { // sub(z, x*y) -> msub x,y,z
		fuse(ir.OMSub, m, in.Args[0])
	}
}

// foldRotate rewrites the shift-or rotate idiom into a single ORotr, which the
// emitters turn into an AArch64 ROR. It matches
//
//	t1 = shr x, c1 ; t2 = shl x, c2 ; t3 = or t1, t2   (c1+c2 = width)
//
// (either shift order) and, when both shifts feed only the or, replaces the or
// with `rotr x, c1` and drops the now-dead shifts.
func foldRotate(f *ir.Func, in *ir.Instr, uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	x, ror, ok := rotateMatch(f, in, defOf)
	if !ok {
		return
	}
	// Only fold when each shift result is consumed solely by this or, so dropping
	// the shifts changes nothing else.
	if uses[in.Args[0].ID] != 1 || uses[in.Args[1].ID] != 1 {
		return
	}
	s1, s2 := defOf(in.Args[0]), defOf(in.Args[1])
	in.Op = ir.ORotr
	in.Args = []ir.Ref{x, f.ConstInt(in.Cls, int64(ror))}
	nop(s1)
	nop(s2)
}

// foldBic rewrites and(a, ~b) into a single OBic (a AND NOT b), when the NOT —
// a xor with an all-ones constant — feeds only this and.
func foldBic(f *ir.Func, in *ir.Instr, uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	notOf := func(r ir.Ref) (ir.Ref, bool) {
		if uses[r.ID] != 1 {
			return ir.Ref{}, false
		}
		d := defOf(r)
		if d == nil || d.Op != ir.OXor {
			return ir.Ref{}, false
		}
		if allOnes(f, d.Cls, d.Args[1]) {
			return d.Args[0], true
		}
		if allOnes(f, d.Cls, d.Args[0]) {
			return d.Args[1], true
		}
		return ir.Ref{}, false
	}
	a, b := in.Args[0], in.Args[1]
	if x, ok := notOf(b); ok {
		in.Op = ir.OBic
		in.Args = []ir.Ref{a, x}
		nop(defOf(b))
	} else if x, ok := notOf(a); ok {
		in.Op = ir.OBic
		in.Args = []ir.Ref{b, x}
		nop(defOf(a))
	}
}

// allOnes reports whether ref is the all-ones constant for cls's width.
func allOnes(f *ir.Func, cls ir.Cls, ref ir.Ref) bool {
	v, ok := intConst(f, ref)
	if !ok {
		return false
	}
	if cls.Size() == 4 {
		return uint32(v) == 0xffffffff
	}
	return uint64(v) == ^uint64(0)
}

// nop turns an instruction into a no-op that defines nothing.
func nop(in *ir.Instr) {
	in.Op = ir.ONop
	in.To = ir.Ref{}
	in.Args = nil
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

// lowerTLS rewrites references to thread-local variables into instructions the
// register allocator can see.
//
// A thread-local has no address of its own, and how one is reached depends on the
// model. Local-exec is a link-time constant that fits in a single register, so the
// emitter can improvise it. The others cannot be improvised: initial-exec adds the
// thread pointer to an offset -- two registers at once -- and general-dynamic ends
// in a call, which would clobber whatever the allocator happened to be keeping in
// caller-saved registers. Turning the access into ordinary instructions here lets
// the allocator supply the registers and (for the call) spill across it, instead
// of an addressing helper doing either behind its back.
func lowerTLS(f *ir.Func, model TLSModel) {
	if model == TLSLocalExec {
		return // a link-time constant, and one register is enough to build it
	}
	for _, b := range f.Blocks {
		out := make([]ir.Instr, 0, len(b.Instrs))
		for _, in := range b.Instrs {
			for i, a := range in.Args {
				c, ok := threadConst(f, a)
				if !ok {
					continue
				}
				var addr ir.Ref
				if model == TLSGeneralDynamic {
					// The variable's storage may not exist in this thread yet, so ask
					// __tls_get_addr for the address, handing it the descriptor. The call
					// is an ordinary one: the allocator sees it and spills across it.
					idx := f.NewTemp("", ir.ClsL)
					addr = f.NewTemp("", ir.ClsL)
					out = append(out,
						ir.Instr{Op: ir.OTLSIndexAddr, Cls: ir.ClsL, To: idx, Args: []ir.Ref{a}, Pos: in.Pos},
						ir.Instr{Op: ir.OCall, Cls: ir.ClsL, To: addr, Pos: in.Pos,
							Args: []ir.Ref{f.Sym("__tls_get_addr", 0), idx}},
					)
				} else {
					// addr = thread_pointer + offset(sym), an ordinary add of two temps.
					off := f.NewTemp("", ir.ClsL)
					tp := f.NewTemp("", ir.ClsL)
					addr = f.NewTemp("", ir.ClsL)
					out = append(out,
						ir.Instr{Op: ir.OTLSOffset, Cls: ir.ClsL, To: off, Args: []ir.Ref{a}, Pos: in.Pos},
						ir.Instr{Op: ir.OThreadPtr, Cls: ir.ClsL, To: tp, Pos: in.Pos},
						ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: addr, Args: []ir.Ref{tp, off}, Pos: in.Pos},
					)
				}
				if c.Int != 0 {
					sum := f.NewTemp("", ir.ClsL)
					out = append(out, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: sum,
						Args: []ir.Ref{addr, f.Long(c.Int)}, Pos: in.Pos})
					addr = sum
				}
				in.Args[i] = addr
			}
			out = append(out, in)
		}
		b.Instrs = out
	}
}

// threadConst reports whether ref names a thread-local symbol, and returns it.
func threadConst(f *ir.Func, ref ir.Ref) (ir.Const, bool) {
	if ref.Kind != ir.RefConst {
		return ir.Const{}, false
	}
	c := f.Consts[ref.ID]
	if c.Kind != ir.ConstSym || !c.Thread {
		return ir.Const{}, false
	}
	return c, true
}
