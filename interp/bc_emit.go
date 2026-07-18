package interp

import "github.com/evanphx/cg12/ir"

// emitInstr compiles one non-terminator instruction. It mirrors the tree-walker's
// execInstr dispatch (ops.go), but writes register bytecode instead of walking:
// the value families carry the concrete ir.Op/ir.Cmp in the aux byte so the
// executor calls the same pure helpers.
func (c *compiler) emitInstr(in *ir.Instr) {
	switch {
	case in.Op == ir.OCall:
		c.emitCall(in)
		return
	case in.Op == ir.OSafepoint, in.Op == ir.ONop:
		return
	case in.Op.IsLoad():
		c.emit(mkABC(bcLoad, in.Cls, uint8(in.Op), c.dst(in), c.refReg(in.Arg(0)), 0))
		return
	case in.Op.IsStore():
		c.emit(mkABC(bcStore, in.Cls, uint8(in.Op), c.refReg(in.Arg(0)), c.refReg(in.Arg(1)), 0))
		return
	case in.Op.IsAlloc() || in.Op == ir.OAllocN:
		c.emit(mkABC(bcAlloc, in.Cls, uint8(in.Op), c.dst(in), c.refReg(in.Arg(0)), 0))
		return
	case in.Op == ir.OBlit:
		if in.Aux < 0 || in.Aux > maxReg {
			c.fail("blit of %d bytes exceeds the %d-byte inline limit", in.Aux, maxReg)
			return
		}
		c.emit(mkABC(bcBlit, ir.ClsL, 0, c.refReg(in.Arg(0)), c.refReg(in.Arg(1)), uint16(in.Aux)))
		return
	case in.Op == ir.OIntrinsic:
		ii := intrinInfo{name: in.Intrin.Name, dst: c.dst(in)}
		for _, a := range in.Args {
			ii.args = append(ii.args, c.refReg(a))
		}
		idx := uint32(len(c.intrins))
		c.intrins = append(c.intrins, ii)
		c.emit(mkABx(bcIntrin, in.Cls, 0, c.dst(in), idx))
		return
	case in.Op == ir.OVaStart:
		c.emit(mkABC(bcVaStart, ir.ClsL, 0, c.refReg(in.Arg(0)), 0, 0))
		return
	case in.Op == ir.OVaArg:
		c.emit(mkABC(bcVaArg, in.Cls, 0, c.dst(in), c.refReg(in.Arg(0)), 0))
		return
	case in.Op == ir.OBlockAddr:
		addr, ok := c.mc.blockAddr[in.Blk]
		if !ok {
			c.fail("block address of a block whose address was not taken at load")
			return
		}
		// The address is a compile-time constant; load it like any other.
		poolIdx := uint32(len(c.consts))
		c.consts = append(c.consts, ptrVal(addr))
		c.emit(mkABx(bcLoadK, ir.ClsL, 0, c.dst(in), poolIdx))
		return
	case in.Op == ir.OCmp:
		c.emit(mkABC(bcCmp, in.Cls, uint8(in.Cmp), c.dst(in), c.refReg(in.Arg(0)), c.refReg(in.Arg(1))))
		return
	case in.Op == ir.OSel:
		idx := uint32(len(c.sels))
		c.sels = append(c.sels, selInfo{cond: c.refReg(in.Arg(0)), a: c.refReg(in.Arg(1)), b: c.refReg(in.Arg(2))})
		c.emit(mkABx(bcSel, in.Cls, 0, c.dst(in), idx))
		return
	}

	// Pure value ops: binary vs unary.
	if isBinop(in.Op) {
		c.emit(mkABC(bcBinop, in.Cls, uint8(in.Op), c.dst(in), c.refReg(in.Arg(0)), c.refReg(in.Arg(1))))
		return
	}
	if isUnop(in.Op) {
		c.emit(mkABC(bcUnop, in.Cls, uint8(in.Op), c.dst(in), c.refReg(in.Arg(0)), 0))
		return
	}
	c.fail("unhandled op %s", in.Op)
}

// dst returns an instruction's result register, or 0 when it has none (a slot the
// executor will not write).
func (c *compiler) dst(in *ir.Instr) uint16 {
	if in.To.IsNone() {
		return 0
	}
	return c.reg[in.To.ID]
}

func isBinop(op ir.Op) bool {
	switch op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.ODiv, ir.OUDiv, ir.ORem, ir.OURem,
		ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar:
		return true
	}
	return false
}

func isUnop(op ir.Op) bool {
	switch op {
	case ir.ONeg, ir.OClz,
		ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw,
		ir.OExts, ir.OTruncd, ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof,
		ir.OCast, ir.OCopy:
		return true
	}
	return false
}

// emitCall stages the arguments into the outgoing region and records a call site.
func (c *compiler) emitCall(in *ir.Instr) {
	args := in.Args[1:]
	if len(args) > c.maxArgs {
		c.fail("call with %d args exceeds staged max %d", len(args), c.maxArgs)
		return
	}
	for i, a := range args {
		c.emit(mkABC(bcMove, c.f.ClassOf(a), 0, c.argBase+uint16(i), c.refReg(a), 0))
	}

	site := callSite{
		argBase: c.argBase,
		nArgs:   len(args),
		aggArgs: in.AggArgs,
		retAgg:  in.RetAgg,
	}
	if !in.To.IsNone() {
		site.hasResult = true
		site.resultReg = c.reg[in.To.ID]
		site.resultCls = in.Cls
	}
	callee := in.Args[0]
	if callee.Kind == ir.RefConst {
		if con := &c.f.Consts[callee.ID]; con.Kind == ir.ConstSym {
			site.name = con.Sym
		}
	}
	if site.name == "" {
		site.calleeReg = c.refReg(callee)
	}
	c.emit(mkABx(bcCall, ir.ClsL, 0, 0, uint32(len(c.calls))))
	c.calls = append(c.calls, site)
}

// emitTerminator compiles a block's terminator, emitting the phi moves for each
// outgoing edge just before the branch that takes it.
func (c *compiler) emitTerminator(b *ir.Block) {
	j := &b.Jmp
	switch j.Kind {
	case ir.JmpJmp:
		c.emitEdge(b, j.To)
		c.emitJmp(j.To)

	case ir.JmpJnz:
		cond := c.refReg(j.Arg)
		jnz := c.emit(mkABx(bcJnz, ir.ClsW, 0, cond, 0)) // target patched to L_true below
		c.emitEdge(b, j.To2)                             // false edge falls through
		c.emitJmp(j.To2)
		ltrue := uint32(len(c.code))
		c.code[jnz] = mkABx(bcJnz, ir.ClsW, 0, cond, ltrue)
		c.emitEdge(b, j.To)
		c.emitJmp(j.To)

	case ir.JmpRet:
		if j.Arg.IsNone() {
			c.emit(mkABC(bcRet, c.f.Retty, 0, 0, 0, 0))
		} else {
			c.emit(mkABC(bcRet, c.f.Retty, 1, c.refReg(j.Arg), 0, 0))
		}

	case ir.JmpHlt:
		c.emit(mkABC(bcHlt, ir.ClsL, 0, 0, 0, 0))

	case ir.JmpSwitch:
		c.emitSwitch(b)

	case ir.JmpTable:
		c.emitTable(b)

	case ir.JmpBr:
		// A computed-goto target is dynamic, so per-edge phi moves cannot be
		// staged; the tree-walker has the same restriction.
		for _, tgt := range j.Targets {
			if len(tgt.Phis) != 0 {
				c.fail("computed-goto target %q has phis", tgt.Name)
				return
			}
		}
		c.emit(mkABC(bcBr, ir.ClsL, 0, c.refReg(j.Arg), 0, 0))

	default:
		c.fail("block %q has terminator kind %d", b.Name, j.Kind)
	}
}

func (c *compiler) emitSwitch(b *ir.Block) {
	j := &b.Jmp
	idx := len(c.switches)
	c.switches = append(c.switches, switchInfo{})
	c.emit(mkABx(bcSwitch, c.f.ClassOf(j.Arg), 0, c.refReg(j.Arg), uint32(idx)))

	info := switchInfo{signed: j.Signed, argCls: c.f.ClassOf(j.Arg)}
	for _, cs := range j.Cases {
		pc := uint32(len(c.code))
		c.emitEdge(b, cs.Blk)
		c.emitJmp(cs.Blk)
		info.cases = append(info.cases, switchCasePC{val: cs.Val, pc: pc})
	}
	info.dflt = uint32(len(c.code))
	c.emitEdge(b, j.To)
	c.emitJmp(j.To)
	c.switches[idx] = info
}

func (c *compiler) emitTable(b *ir.Block) {
	j := &b.Jmp
	idx := len(c.switches)
	c.switches = append(c.switches, switchInfo{})
	c.emit(mkABx(bcTable, c.f.ClassOf(j.Arg), 0, c.refReg(j.Arg), uint32(idx)))

	info := switchInfo{argCls: c.f.ClassOf(j.Arg)}
	for _, tgt := range j.Targets {
		pc := uint32(len(c.code))
		c.emitEdge(b, tgt)
		c.emitJmp(tgt)
		info.targets = append(info.targets, pc)
	}
	c.switches[idx] = info
}

// emitJmp emits an unconditional branch whose target block is patched once every
// block's start pc is known.
func (c *compiler) emitJmp(target *ir.Block) {
	pos := c.emit(mkABx(bcJmp, ir.ClsL, 0, 0, 0))
	c.fixups = append(c.fixups, fixup{pos: pos, blk: target})
}

// emitEdge writes the parallel phi copies for the edge b -> succ, sequentialized
// so no destination is clobbered before it is read.
func (c *compiler) emitEdge(b, succ *ir.Block) {
	if len(succ.Phis) == 0 {
		return
	}
	var moves []regMove
	for _, p := range succ.Phis {
		src, ok := phiArg(p, b)
		if !ok {
			c.fail("phi in %q has no argument for predecessor %q", succ.Name, b.Name)
			return
		}
		moves = append(moves, regMove{dst: c.reg[p.To.ID], src: c.refReg(src), cls: p.Cls})
	}
	c.scratch = c.localTop + uint16(c.maxArgs) // scratch is per-edge; reuse across edges
	for _, m := range c.sequentialize(moves) {
		c.emit(mkABC(bcMove, m.cls, 0, m.dst, m.src, 0))
	}
}

func phiArg(p *ir.Phi, pred *ir.Block) (ir.Ref, bool) {
	for k, b := range p.Blocks {
		if b == pred {
			return p.Args[k], true
		}
	}
	return ir.Ref{}, false
}

// sequentialize orders a set of parallel register copies so no destination is
// overwritten before it is read, breaking cycles with a fresh scratch register --
// the register-level twin of lower.sequentialize (which works on IR temps).
func (c *compiler) sequentialize(moves []regMove) []regMove {
	var work []regMove
	for _, m := range moves {
		if m.dst != m.src {
			work = append(work, m)
		}
	}
	var out []regMove
	for len(work) > 0 {
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
		// Every remaining destination is still read: break a cycle.
		p := work[0]
		tmp := c.newScratch()
		out = append(out, regMove{dst: tmp, src: p.dst, cls: p.cls})
		for i := range work {
			if work[i].src == p.dst {
				work[i].src = tmp
			}
		}
	}
	return out
}

// patchJumps rewrites every forward branch's bx with its target block's pc, now
// that all blocks have been laid out.
func (c *compiler) patchJumps() {
	for _, fx := range c.fixups {
		w := c.code[fx.pos]
		c.code[fx.pos] = mkABx(w.op(), w.cls(), w.aux(), w.ra(), c.blockStart[fx.blk])
	}
}
