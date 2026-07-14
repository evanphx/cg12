package bpf

import "github.com/evanphx/cg12/ir"

// instr lowers one non-terminator instruction to eBPF.
func (c *comp) instr(in *ir.Instr) {
	if in.Pos.Valid() {
		c.curPos = in.Pos
	}
	switch {
	case in.Op == ir.ONop:
	case in.Op == ir.OCopy:
		d := c.destReg(in.To)
		c.into(d, in.Arg(0))
		c.spillBack(in.To, d)
	case in.Op.IsAlloc():
		d := c.destReg(in.To)
		c.emit(Mov64Reg(d, R10), Alu64Imm(aluADD, d, int32(c.allocaAt[in])))
		c.spillBack(in.To, d)
	case in.Op.IsLoad():
		c.lowerLoad(in)
	case in.Op.IsStore():
		c.lowerStore(in)
	case in.Op == ir.OCmp:
		c.lowerCmp(in)
	case in.Op == ir.ONeg:
		d := c.destReg(in.To)
		c.into(d, in.Arg(0))
		c.emit(Insn{Op: aluClass(in.Cls) | aluNEG, Dst: d})
		c.spillBack(in.To, d)
	case in.Op == ir.OExtsw, in.Op == ir.OExtsh, in.Op == ir.OExtsb:
		d := c.destReg(in.To)
		c.into(d, in.Arg(0))
		c.signExtend(d, extBits(in.Op))
		c.spillBack(in.To, d)
	case in.Op == ir.OExtuw:
		d := c.destReg(in.To)
		c.emit(Alu32Reg(aluMOV, d, c.regOf(in.Arg(0), scratchB))) // 32-bit mov zero-extends
		c.spillBack(in.To, d)
	case in.Op == ir.OExtub, in.Op == ir.OExtuh:
		d := c.destReg(in.To)
		c.into(d, in.Arg(0))
		c.emit(Alu64Imm(aluAND, d, extMask(in.Op)))
		c.spillBack(in.To, d)
	case in.Op == ir.OCall:
		c.lowerCall(in)
	default:
		if op, ok := aluOp(in.Op); ok {
			c.lowerBinary(in, op)
			return
		}
		c.fail("bpf: unsupported op %s", in.Op)
	}
}

// aluClass returns the ALU instruction class for a value class: 32-bit ALU for a
// word (its result is zero-extended into the register), 64-bit otherwise.
func aluClass(cls ir.Cls) uint8 {
	if cls == ir.ClsW {
		return clsALU
	}
	return clsALU64
}

func aluOp(op ir.Op) (uint8, bool) {
	switch op {
	case ir.OAdd:
		return aluADD, true
	case ir.OSub:
		return aluSUB, true
	case ir.OMul:
		return aluMUL, true
	case ir.ODiv, ir.OUDiv:
		return aluDIV, true // eBPF DIV is unsigned; signed division is not modelled
	case ir.ORem, ir.OURem:
		return aluMOD, true
	case ir.OAnd:
		return aluAND, true
	case ir.OOr:
		return aluOR, true
	case ir.OXor:
		return aluXOR, true
	case ir.OShl:
		return aluLSH, true
	case ir.OShr:
		return aluRSH, true
	case ir.OSar:
		return aluARSH, true
	}
	return 0, false
}

// lowerBinary emits dst = a op b. It computes in the destination register,
// moving the second operand aside first if it shares that register.
func (c *comp) lowerBinary(in *ir.Instr, op uint8) {
	d := c.destReg(in.To)
	breg := c.regOf(in.Arg(1), scratchB)
	if breg == d {
		c.emit(Mov64Reg(scratchB, breg))
		breg = scratchB
	}
	c.into(d, in.Arg(0))
	c.emit(Insn{Op: aluClass(in.Cls) | op | srcX, Dst: d, Src: breg})
	c.spillBack(in.To, d)
}

func (c *comp) lowerLoad(in *ir.Instr) {
	addr := c.regOf(in.Arg(0), scratchB)
	d := c.destReg(in.To)
	c.emit(LdxMem(memSize(in.Op), d, addr, 0))
	if signedLoad(in.Op) {
		c.signExtend(d, loadBits(in.Op))
	}
	c.spillBack(in.To, d)
}

func (c *comp) lowerStore(in *ir.Instr) {
	val := c.regOf(in.Arg(0), scratchA)
	addr := c.regOf(in.Arg(1), scratchB)
	c.emit(StxMem(memSize(in.Op), addr, 0, val))
}

// lowerCmp materializes a 0/1 result. The comparison reads the operands before
// any write to the destination, so the destination may safely reuse an operand's
// register.
func (c *comp) lowerCmp(in *ir.Instr) {
	op, ok := jmpOp(in.Cmp)
	if !ok {
		c.fail("bpf: unsupported comparison predicate %v (floats have no eBPF support)", in.Cmp)
		return
	}
	a := c.regOf(in.Arg(0), scratchA)
	b := c.regOf(in.Arg(1), scratchB)
	d := c.destReg(in.To)
	c.emit(
		JmpCondReg(op, a, b, 2), // true: skip to the "= 1"
		Mov64Imm(d, 0),
		JmpA(1),
		Mov64Imm(d, 1),
	)
	c.spillBack(in.To, d)
}

func jmpOp(c ir.Cmp) (uint8, bool) {
	switch c {
	case ir.CmpEq:
		return jmpJEQ, true
	case ir.CmpNe:
		return jmpJNE, true
	case ir.CmpSlt:
		return jmpJSLT, true
	case ir.CmpSle:
		return jmpJSLE, true
	case ir.CmpSgt:
		return jmpJSGT, true
	case ir.CmpSge:
		return jmpJSGE, true
	case ir.CmpUlt:
		return jmpJLT, true
	case ir.CmpUle:
		return jmpJLE, true
	case ir.CmpUgt:
		return jmpJGT, true
	case ir.CmpUge:
		return jmpJGE, true
	}
	return 0, false // float predicates: unsupported
}

func (c *comp) lowerCall(in *ir.Instr) {
	args := in.Args[1:]
	if len(args) > 5 {
		c.fail("bpf: call with %d arguments, eBPF passes at most 5", len(args))
		return
	}
	var moves []movePair
	for i, a := range args {
		moves = append(moves, movePair{dst: regLoc(Reg(int(R1) + i)), src: c.locOf(a)})
	}
	c.parallelMove(moves)

	callee := in.Arg(0)
	if callee.Kind != ir.RefConst || c.f.Consts[callee.ID].Kind != ir.ConstSym {
		c.fail("bpf: only direct calls to named helpers or functions are supported")
		return
	}
	name := c.f.Consts[callee.ID].Sym
	switch {
	case c.funcs[name]: // a BPF-to-BPF call to another module function
		c.subRelocs = append(c.subRelocs, subReloc{Insn: len(c.insns), Func: name})
		c.emit(CallRel(0)) // pc-relative offset patched once the callee is placed
	default:
		id, ok := helperID(name)
		if !ok {
			c.fail("bpf: unknown helper or function %q", name)
			return
		}
		c.emit(Call(id))
	}
	if !in.To.IsNone() && c.isUsed(in.To) {
		c.emitMove(c.locOf(in.To), regLoc(R0)) // result is in r0
	}
	// A call result that is never read stays in r0. This is required for
	// bpf_tail_call: on the fall-through path the verifier treats r0 as
	// uninitialized, so emitting "rN = r0" would be rejected as reading r0.
}

// term lowers a block's terminator, resolving phis on each out-edge.
func (c *comp) term(b *ir.Block) {
	switch b.Jmp.Kind {
	case ir.JmpRet:
		if !b.Jmp.Arg.IsNone() {
			c.into(R0, b.Jmp.Arg)
		} else {
			c.emit(Mov64Imm(R0, 0))
		}
		c.emit(Exit())
	case ir.JmpJmp:
		c.movePhis(b, b.Jmp.To)
		c.jumpTo(JmpA(0), b.Jmp.To)
	case ir.JmpJnz:
		// Resolve phis per edge so neither successor's registers are clobbered on
		// the path to the other (a critical-edge split done inline).
		creg := c.regOf(b.Jmp.Arg, scratchA)
		condAt := len(c.insns)
		c.emit(JmpCondImm(jmpJEQ, creg, 0, 0)) // if cond == 0, take the false edge
		c.movePhis(b, b.Jmp.To)
		c.jumpTo(JmpA(0), b.Jmp.To)
		c.insns[condAt].Off = int16(len(c.insns) - (condAt + 1)) // false pad starts here
		c.movePhis(b, b.Jmp.To2)
		c.jumpTo(JmpA(0), b.Jmp.To2)
	case ir.JmpHlt:
		c.emit(Mov64Imm(R0, 0), Exit())
	default:
		c.fail("bpf: block %q has an unsupported terminator", b.Name)
	}
}

// movePhis transfers, as one simultaneous move, each of succ's phi values for
// the edge from pred.
func (c *comp) movePhis(pred, succ *ir.Block) {
	var moves []movePair
	for _, phi := range succ.Phis {
		for k, pb := range phi.Blocks {
			if pb == pred {
				moves = append(moves, movePair{dst: c.locOf(phi.To), src: c.locOf(phi.Args[k])})
				break
			}
		}
	}
	c.parallelMove(moves)
}

// jumpTo emits a jump and records a fixup to patch its block-relative offset.
func (c *comp) jumpTo(in Insn, target *ir.Block) {
	c.fixups = append(c.fixups, fixup{at: len(c.insns), target: target})
	c.emit(in)
}

// signExtend sign-extends the low `bits` bits of reg to 64 bits via a shift pair.
func (c *comp) signExtend(reg Reg, bits int32) {
	sh := int32(64) - bits
	c.emit(Alu64Imm(aluLSH, reg, sh), Alu64Imm(aluARSH, reg, sh))
}

func memSize(op ir.Op) uint8 {
	switch op {
	case ir.OLoadsb, ir.OLoadub, ir.OStoreb:
		return sizeB
	case ir.OLoadsh, ir.OLoaduh, ir.OStoreh:
		return sizeH
	case ir.OLoadsw, ir.OLoaduw, ir.OStorew:
		return sizeW
	default:
		return sizeDW
	}
}

func signedLoad(op ir.Op) bool {
	return op == ir.OLoadsb || op == ir.OLoadsh || op == ir.OLoadsw
}

func loadBits(op ir.Op) int32 {
	switch op {
	case ir.OLoadsb:
		return 8
	case ir.OLoadsh:
		return 16
	default:
		return 32
	}
}

func extBits(op ir.Op) int32 {
	switch op {
	case ir.OExtsb:
		return 8
	case ir.OExtsh:
		return 16
	default:
		return 32
	}
}

func extMask(op ir.Op) int32 {
	if op == ir.OExtub {
		return 0xff
	}
	return 0xffff
}
