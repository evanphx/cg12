package amd64

import (
	"fmt"
	"math"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// This is the text (GNU-assembler) emitter: the readable counterpart to the
// machine-code emitter in mc.go. It consumes the same lowered, register-allocated
// IR and the same frame layout, but writes AT&T-syntax x86-64 assembly instead of
// bytes. The machine path is primary; this path aids inspection and is validated
// by assembling and running its output.

// gpNames gives each GP register's name at width [8, 16, 32, 64] bits.
var gpNames = [16][4]string{
	RAX: {"al", "ax", "eax", "rax"},
	RCX: {"cl", "cx", "ecx", "rcx"},
	RDX: {"dl", "dx", "edx", "rdx"},
	RBX: {"bl", "bx", "ebx", "rbx"},
	RSP: {"spl", "sp", "esp", "rsp"},
	RBP: {"bpl", "bp", "ebp", "rbp"},
	RSI: {"sil", "si", "esi", "rsi"},
	RDI: {"dil", "di", "edi", "rdi"},
	R8:  {"r8b", "r8w", "r8d", "r8"},
	R9:  {"r9b", "r9w", "r9d", "r9"},
	R10: {"r10b", "r10w", "r10d", "r10"},
	R11: {"r11b", "r11w", "r11d", "r11"},
	R12: {"r12b", "r12w", "r12d", "r12"},
	R13: {"r13b", "r13w", "r13d", "r13"},
	R14: {"r14b", "r14w", "r14d", "r14"},
	R15: {"r15b", "r15w", "r15d", "r15"},
}

func sizeIdx(size int) int {
	switch size {
	case 1:
		return 0
	case 2:
		return 1
	case 4:
		return 2
	default:
		return 3
	}
}

// suf returns the AT&T operand-size suffix for a byte width.
func suf(size int) string {
	switch size {
	case 1:
		return "b"
	case 2:
		return "w"
	case 4:
		return "l"
	default:
		return "q"
	}
}

func gpn(r Reg, size int) string { return "%" + gpNames[r][sizeIdx(size)] }
func xmmn(r Reg) string          { return fmt.Sprintf("%%xmm%d", int(r-XMM0)) }
func memn(base Reg, disp int32) string {
	if disp == 0 {
		return "(" + gpn(base, 8) + ")"
	}
	return fmt.Sprintf("%d(%s)", disp, gpn(base, 8))
}

// emitter renders one function to assembly text.
type emitter struct {
	f     *ir.Func
	alloc *allocation
	sb    *strings.Builder
	fname string
	frameLayout
	blockDone bool
	vaSeq     int
	lastPos   ir.SrcPos
}

func emitFunc(f *ir.Func, alloc *allocation, sb *strings.Builder) {
	e := &emitter{f: f, alloc: alloc, sb: sb, fname: sanitize(f.Name)}
	e.frameLayout = computeFrame(f, alloc)
	if f.Linkage.Export {
		fmt.Fprintf(sb, "\t.globl %s\n", e.fname)
	}
	fmt.Fprintf(sb, "\t.type %s,@function\n%s:\n", e.fname, e.fname)
	e.prologue()
	for _, b := range f.Blocks {
		if b.Sym != "" { // object symbol for an address-taken block (&&label in data)
			fmt.Fprintf(sb, "%s:\n", sanitize(b.Sym))
		}
		fmt.Fprintf(sb, "%s:\n", e.blabel(b))
		e.block(b)
	}
	fmt.Fprintf(sb, "\t.size %s, .-%s\n", e.fname, e.fname)
}

func (e *emitter) line(format string, a ...any) {
	fmt.Fprintf(e.sb, "\t"+format+"\n", a...)
}

func (e *emitter) blabel(b *ir.Block) string {
	return ".L" + e.fname + "_" + sanitize(b.Name)
}

// --- prologue / epilogue ---------------------------------------------------

func (e *emitter) prologue() {
	e.line("pushq %%rbp")
	e.line("movq %%rsp, %%rbp")
	if e.frame > 0 {
		e.line("subq $%d, %%rsp", e.frame)
	}
	for k, r := range e.calleeSaved {
		e.line("movq %s, %s", gpn(r, 8), memn(RBP, e.savedAddr(k)))
	}
	if e.f.Variadic {
		base := -e.regSaveDist
		for i, r := range argGP {
			e.line("movq %s, %s", gpn(r, 8), memn(RBP, int32(base+8*i)))
		}
		for i, r := range argFP {
			e.line("movsd %s, %s", xmmn(r), memn(RBP, int32(base+vaGPBytes+16*i)))
		}
	}
}

func (e *emitter) teardown() {
	for k, r := range e.calleeSaved {
		e.line("movq %s, %s", memn(RBP, e.savedAddr(k)), gpn(r, 8))
	}
	e.line("movq %%rbp, %%rsp")
	e.line("popq %%rbp")
}

func (e *emitter) epilogue() {
	e.teardown()
	e.line("ret")
}

// --- operands (shared loc abstraction) -------------------------------------

func (e *emitter) refLoc(r ir.Ref) loc {
	switch r.Kind {
	case ir.RefTemp:
		t := e.f.Temps[r.ID]
		size := t.Cls.Size()
		fl := t.Cls.IsFloat()
		if t.Reg != ir.NoReg {
			return regLoc(Reg(t.Reg), size, fl)
		}
		return memLoc(RBP, e.slotAddr(t.Slot), size, fl)
	case ir.RefConst:
		c := e.f.Consts[r.ID]
		switch c.Kind {
		case ir.ConstInt:
			return loc{kind: locImm, val: c.Int, size: c.Cls.Size()}
		case ir.ConstFloat:
			if c.Cls == ir.ClsS {
				return loc{kind: locImm, val: int64(math.Float32bits(float32(c.Flt))), size: 4, float: true}
			}
			return loc{kind: locImm, val: int64(math.Float64bits(c.Flt)), size: 8, float: true}
		case ir.ConstSym:
			return loc{kind: locSym, sym: c.Sym, symoff: c.Int, size: 8, tls: c.Thread}
		}
	}
	return loc{}
}

func (e *emitter) move(dst, src loc) {
	switch dst.kind {
	case locReg:
		e.moveToReg(dst, src)
	case locMem:
		e.moveToMem(dst, src)
	}
}

func (e *emitter) moveToReg(dst, src loc) {
	switch src.kind {
	case locReg:
		if dst.reg == src.reg {
			return
		}
		if dst.float {
			mn := "movsd"
			if !w64(src.size) {
				mn = "movss"
			}
			e.line("%s %s, %s", mn, xmmn(src.reg), xmmn(dst.reg))
		} else {
			e.line("mov%s %s, %s", suf(dst.size), gpn(src.reg, dst.size), gpn(dst.reg, dst.size))
		}
	case locMem:
		if dst.float {
			mn := "movsd"
			if !w64(src.size) {
				mn = "movss"
			}
			e.line("%s %s, %s", mn, memn(src.base, src.off), xmmn(dst.reg))
		} else {
			e.line("mov%s %s, %s", suf(src.size), memn(src.base, src.off), gpn(dst.reg, src.size))
		}
	case locImm:
		if dst.float {
			e.materializeFloat(dst.reg, src.val, src.size)
		} else {
			e.movImm(dst.reg, src.val, w64(dst.size))
		}
	case locSym:
		e.materializeSym(dst.reg, src.sym, src.symoff, src.tls)
	}
}

func (e *emitter) moveToMem(dst, src loc) {
	m := memn(dst.base, dst.off)
	switch src.kind {
	case locReg:
		if src.float {
			mn := "movsd"
			if !w64(dst.size) {
				mn = "movss"
			}
			e.line("%s %s, %s", mn, xmmn(src.reg), m)
		} else {
			e.line("mov%s %s, %s", suf(dst.size), gpn(src.reg, dst.size), m)
		}
	case locMem:
		e.moveToReg(regLoc(gpScratch0, src.size, src.float), src)
		e.moveToMem(dst, regLoc(gpScratch0, dst.size, dst.float))
	case locImm:
		if dst.float {
			e.materializeFloat(fpScratch0, src.val, src.size)
			e.moveToMem(dst, regLoc(fpScratch0, dst.size, true))
		} else {
			e.line("mov%s $%d, %s", suf(dst.size), src.val, m)
		}
	case locSym:
		e.materializeSym(gpScratch0, src.sym, src.symoff, src.tls)
		e.line("movq %s, %s", gpn(gpScratch0, 8), m)
	}
}

func (e *emitter) movImm(d Reg, val int64, w bool) {
	if w && (val < math.MinInt32 || val > math.MaxInt32) {
		e.line("movabsq $%d, %s", val, gpn(d, 8))
		return
	}
	sz := 4
	if w {
		sz = 8
	}
	e.line("mov%s $%d, %s", suf(sz), val, gpn(d, sz))
}

func (e *emitter) materializeFloat(d Reg, bits int64, size int) {
	if size == 8 {
		e.movImm(gpScratch0, bits, true)
		e.line("movq %s, %s", gpn(gpScratch0, 8), xmmn(d))
	} else {
		e.movImm(gpScratch0, bits, false)
		e.line("movd %s, %s", gpn(gpScratch0, 4), xmmn(d))
	}
}

func (e *emitter) materializeSym(d Reg, sym string, off int64, tls bool) {
	name := sanitize(sym)
	if tls {
		e.line("movq %%fs:0, %s", gpn(d, 8))
		e.line("leaq %s@tpoff(%s), %s", name, gpn(d, 8), gpn(d, 8))
		return
	}
	if off != 0 {
		e.line("leaq %s+%d(%%rip), %s", name, off, gpn(d, 8))
	} else {
		e.line("leaq %s(%%rip), %s", name, gpn(d, 8))
	}
}

func (e *emitter) gpValue(r ir.Ref, scratch Reg) Reg {
	l := e.refLoc(r)
	if l.kind == locReg {
		return l.reg
	}
	e.moveToReg(regLoc(scratch, l.size, false), l)
	return scratch
}

func (e *emitter) fpValue(r ir.Ref, scratch Reg) Reg {
	l := e.refLoc(r)
	if l.kind == locReg {
		return l.reg
	}
	e.moveToReg(regLoc(scratch, l.size, true), l)
	return scratch
}

func (e *emitter) gpInto(d Reg, r ir.Ref) {
	l := e.refLoc(r)
	e.moveToReg(regLoc(d, l.size, false), l)
}

func (e *emitter) fpInto(d Reg, r ir.Ref) {
	l := e.refLoc(r)
	e.moveToReg(regLoc(d, l.size, true), l)
}

func (e *emitter) gpDst(r ir.Ref) (Reg, func()) {
	t := e.f.Temps[r.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	size := t.Cls.Size()
	return gpScratch0, func() {
		e.line("mov%s %s, %s", suf(size), gpn(gpScratch0, size), memn(RBP, e.slotAddr(t.Slot)))
	}
}

func (e *emitter) fpDst(r ir.Ref) (Reg, func()) {
	t := e.f.Temps[r.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	size := t.Cls.Size()
	return fpScratch0, func() {
		mn := "movsd"
		if size != 8 {
			mn = "movss"
		}
		e.line("%s %s, %s", mn, xmmn(fpScratch0), memn(RBP, e.slotAddr(t.Slot)))
	}
}

func (e *emitter) constOf(r ir.Ref) *ir.Const {
	if r.Kind == ir.RefConst {
		return &e.f.Consts[r.ID]
	}
	return nil
}

// --- parallel moves --------------------------------------------------------

func (e *emitter) parallelMove(pairs []locPair) {
	var work []locPair
	for _, p := range pairs {
		if !sameLoc(p.dst, p.src) {
			work = append(work, p)
		}
	}
	for len(work) > 0 {
		idx := -1
		for i, p := range work {
			blocked := false
			for j, q := range work {
				if i != j && srcReadsDst(q.src, p.dst) {
					blocked = true
					break
				}
			}
			if !blocked {
				idx = i
				break
			}
		}
		if idx >= 0 {
			e.move(work[idx].dst, work[idx].src)
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		ci := -1
		for i, p := range work {
			if p.dst.kind == locReg {
				ci = i
				break
			}
		}
		if ci < 0 {
			return
		}
		saved := work[ci].dst
		rescue := gpScratch0
		if saved.float {
			rescue = fpScratch0
		}
		tmp := regLoc(rescue, saved.size, saved.float)
		e.move(tmp, saved)
		for i := range work {
			if srcReadsDst(work[i].src, saved) {
				work[i].src = tmp
			}
		}
	}
}

// --- block emission --------------------------------------------------------

func (e *emitter) block(b *ir.Block) {
	i := 0
	if b == e.f.Start {
		var pairs []locPair
		var aggParams []*ir.Instr
		for i < len(b.Instrs) && b.Instrs[i].Op == ir.OPar {
			in := &b.Instrs[i]
			if t := e.f.Temps[in.To.ID]; t.Agg != nil {
				aggParams = append(aggParams, in)
				i++
				continue
			}
			dst := e.refLoc(in.To)
			var src loc
			if len(in.Args) > 0 {
				src = e.refLoc(in.Args[0])
			} else {
				src = memLoc(RBP, int32(16+in.Aux), in.Cls.Size(), in.Cls.IsFloat())
			}
			pairs = append(pairs, locPair{dst, src})
			i++
		}
		e.parallelMove(pairs)
		for _, in := range aggParams {
			e.leaStackParam(in.To, int32(16+in.Aux))
		}
	}

	e.blockDone = false
	var argPending []*ir.Instr
	for ; i < len(b.Instrs); i++ {
		in := &b.Instrs[i]
		e.emitLoc(in.Pos)
		switch in.Op {
		case ir.OArg:
			argPending = append(argPending, in)
		case ir.OCall:
			e.emitArgs(argPending)
			nfloat := 0
			for _, ai := range argPending {
				if !ai.To.IsNone() && ai.Cls.IsFloat() {
					nfloat++
				}
			}
			argPending = nil
			e.line("movl $%d, %%eax", nfloat)
			if in.Tail {
				e.emitTailCall(in)
				e.blockDone = true
			} else {
				e.emitCall(in)
			}
		default:
			e.instr(in)
		}
	}
	e.term(b)
}

// emitLoc emits a .loc directive when the source position changes; GAS turns
// these into the .debug_line table.
func (e *emitter) emitLoc(pos ir.SrcPos) {
	if !pos.Valid() || pos == e.lastPos {
		return
	}
	e.lastPos = pos
	e.line(".loc %d %d %d", pos.File, pos.Line, pos.Col)
}

func (e *emitter) leaStackParam(to ir.Ref, off int32) {
	dst := e.refLoc(to)
	if dst.kind == locReg {
		e.line("leaq %s, %s", memn(RBP, off), gpn(dst.reg, 8))
		return
	}
	e.line("leaq %s, %s", memn(RBP, off), gpn(gpScratch0, 8))
	e.line("movq %s, %s", gpn(gpScratch0, 8), memn(RBP, dst.off))
}

func (e *emitter) emitArgs(args []*ir.Instr) {
	var pairs []locPair
	for _, ai := range args {
		var dst loc
		if ai.To.IsNone() {
			dst = memLoc(RSP, int32(ai.Aux), ai.Cls.Size(), ai.Cls.IsFloat())
		} else {
			dst = e.refLoc(ai.To)
		}
		pairs = append(pairs, locPair{dst, e.refLoc(ai.Args[0])})
	}
	e.parallelMove(pairs)
}

func (e *emitter) emitCall(in *ir.Instr) {
	callee := in.Args[0]
	if c := e.constOf(callee); c != nil && c.Kind == ir.ConstSym {
		e.line("call %s", sanitize(c.Sym))
		return
	}
	r := e.gpValue(callee, gpScratch0)
	e.line("call *%s", gpn(r, 8))
}

func (e *emitter) emitTailCall(in *ir.Instr) {
	callee := in.Args[0]
	if c := e.constOf(callee); c != nil && c.Kind == ir.ConstSym {
		e.teardown()
		e.line("jmp %s", sanitize(c.Sym))
		return
	}
	r := e.gpValue(callee, gpScratch1)
	if r != gpScratch1 {
		e.line("movq %s, %s", gpn(r, 8), gpn(gpScratch1, 8))
	}
	e.teardown()
	e.line("jmp *%s", gpn(gpScratch1, 8))
}

func (e *emitter) term(b *ir.Block) {
	if e.blockDone {
		return
	}
	if (&xsel{f: e.f, b: &textXasm{e: e}}).term(b) {
		return
	}
	switch b.Jmp.Kind {
	case ir.JmpRet:
		e.epilogue()
	case ir.JmpTable:
		// Indexed branch through a PC-relative offset table (see the machine-code
		// emitter). The assembler computes each entry as (case - table).
		idx := e.gpValue(b.Jmp.Arg, gpScratch1)
		tbl := e.blabel(b) + "_tbl"
		e.line("leaq %s(%%rip), %s", tbl, gpn(gpScratch0, 8))
		e.line("movslq (%s,%s,4), %s", gpn(gpScratch0, 8), gpn(idx, 8), gpn(gpScratch1, 8))
		e.line("addq %s, %s", gpn(gpScratch1, 8), gpn(gpScratch0, 8))
		e.line("jmp *%s", gpn(gpScratch0, 8))
		e.line("%s:", tbl)
		for _, t := range b.Jmp.Targets {
			e.line(".long %s - %s", e.blabel(t), tbl)
		}
	default:
		panic(fmt.Sprintf("amd64: block %q has an unsupported terminator %d", b.Name, b.Jmp.Kind))
	}
}
