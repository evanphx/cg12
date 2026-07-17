package a64

import (
	"fmt"
	"strconv"
	"strings"
)

// Assemble parses AArch64 assembly text -- one instruction or "label:" per line,
// with `//` or `;` comments -- and returns the encoded machine code, resolving
// branches to labels. It drives the same instruction encoders the compiler's
// machine-code emitter uses, so a hand-written or inline-asm sequence can be
// turned into object code without an external assembler.
//
// It covers the common integer instructions (mov, add/sub, and/orr/eor, mul,
// sdiv/udiv, the shifts, neg, mvn, cmp), the base and base+offset loads and
// stores, and the branches (b, b.cond, cbz/cbnz, bl, ret, nop, brk). Unsupported
// mnemonics or operand shapes are a reported error, never silently skipped.
func Assemble(src string) ([]byte, error) {
	p, err := AssembleProgram(src)
	if err != nil {
		return nil, err
	}
	return p.Bytes()
}

// AssembleProgram parses the assembly text into a Program without resolving it,
// so a caller can turn it into a relocatable object (via Program.Link) that keeps
// external bl/b branches as relocations and exported labels as symbols. It honors
// the `.globl`/`.global name` directive; other `.`-directives are ignored.
func AssembleProgram(src string) (*Program, error) {
	p := NewProgram()
	for lineno, raw := range strings.Split(src, "\n") {
		for _, line := range splitStatements(stripComment(raw)) {
			if strings.HasPrefix(line, ".") {
				if err := directive(p, line); err != nil {
					return nil, fmt.Errorf("line %d: %q: %w", lineno+1, line, err)
				}
				continue
			}
			if strings.HasSuffix(line, ":") {
				p.Label(strings.TrimSpace(line[:len(line)-1]))
				continue
			}
			if err := asmLine(p, line); err != nil {
				return nil, fmt.Errorf("line %d: %q: %w", lineno+1, line, err)
			}
		}
	}
	return p, nil
}

// directive handles the assembler directives the object path needs; `.globl`
// (or `.global`) marks a symbol exported. Unknown directives are ignored so
// common section/type noise in hand-written asm does not fail the parse.
func directive(p *Program, line string) error {
	name, rest := splitMnemonic(line)
	switch name {
	case ".globl", ".global":
		if strings.TrimSpace(rest) == "" {
			return fmt.Errorf("%s needs a symbol name", name)
		}
		p.Globl(strings.TrimSpace(rest))
	}
	return nil
}

func stripComment(s string) string {
	if i := strings.Index(s, "//"); i >= 0 {
		s = s[:i]
	}
	return s
}

// splitStatements breaks a line into its statements. GAS separates statements
// with ';' on both x86-64 and AArch64 -- it is not a comment character, and
// treating it as one silently drops everything after the first instruction of
// an `asm("cli; sti")`.
func splitStatements(line string) []string {
	var out []string
	for _, s := range strings.Split(line, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// asmLine assembles one instruction.
func asmLine(p *Program, line string) error {
	mn, rest := splitMnemonic(line)
	ops := splitOperands(rest)
	a := &asmCtx{p: p, mn: mn, ops: ops}
	switch mn {
	case "nop":
		p.Emit(0xd503201f)
		return nil
	case "ret":
		rn := Reg(30) // x30 (the link register) by default
		if len(ops) == 1 {
			r, _, _, err := a.reg(0)
			if err != nil {
				return err
			}
			rn = r
		}
		p.Emit(Ret(rn))
		return nil
	case "brk":
		p.Emit(Brk(0))
		return nil
	case "mov":
		return a.mov()
	case "add", "sub":
		return a.addSub(mn == "sub")
	case "and", "orr", "eor", "bic":
		return a.logical(mn)
	case "mul":
		return a.arith3(func(w bool, d, n, m Reg) uint32 { return Mul(w, d, n, m) })
	case "sdiv":
		return a.arith3(Sdiv)
	case "udiv":
		return a.arith3(Udiv)
	case "lsl", "lsr", "asr":
		return a.shift(mn)
	case "neg":
		return a.arith2(NegReg)
	case "mvn":
		return a.arith2(MvnReg)
	case "cmp":
		return a.cmp()
	case "ldr", "ldrb", "ldrh", "ldrsw", "str", "strb", "strh":
		return a.loadStore(mn)
	case "b":
		return a.branchLabel(func(l string) { p.B(l) })
	case "bl":
		return a.branchLabel(func(l string) { p.Bl(l) })
	case "cbz":
		return a.cbr(func(w bool, r Reg, l string) { p.Cbz(w, r, l) })
	case "cbnz":
		return a.cbr(func(w bool, r Reg, l string) { p.Cbnz(w, r, l) })
	case "br":
		return a.reg1(Br)
	case "blr":
		return a.reg1(Blr)
	case "clz":
		return a.arith2(Clz)
	case "sxtb", "sxth", "sxtw", "uxtb", "uxth":
		return a.extend(mn)
	case "madd":
		return a.arith4(Madd)
	case "msub":
		return a.arith4(Msub)
	case "extr":
		return a.extr()
	case "ror":
		return a.ror()
	case "csel":
		return a.csel()
	case "cset":
		return a.cset()
	case "mrs":
		return a.mrs()
	case "ldp", "stp":
		return a.pair(mn == "ldp")
	// Floating point. The encoders have been here all along; the parser could not
	// reach them, so `mov d0, d1` was refused rather than encoded.
	case "fadd":
		return a.fp3(Fadd)
	case "fsub":
		return a.fp3(Fsub)
	case "fmul":
		return a.fp3(Fmul)
	case "fdiv":
		return a.fp3(Fdiv)
	case "fneg":
		return a.fp2(Fneg)
	case "fcmp":
		return a.fcmp()
	case "fcvt":
		return a.fcvt()
	case "fmov":
		return a.fmov()
	case "scvtf", "ucvtf", "fcvtzs", "fcvtzu":
		return a.fpIntConv(mn)
	case "svc":
		v, ok := a.imm(0)
		if !ok || len(ops) != 1 {
			return fmt.Errorf("svc takes one immediate")
		}
		if v < 0 || v > 0xffff {
			return fmt.Errorf("svc: #%d out of range", v)
		}
		p.Emit(Svc(uint16(v)))
		return nil
	case "dmb", "dsb":
		return a.barrier(mn)
	case "isb":
		// ISB takes an optional domain; only the full-system form is encoded, and
		// it is the only one anything writes, so the operand (if any) must be sy.
		if len(ops) == 1 && ops[0] != "sy" {
			return fmt.Errorf("isb: only the sy domain is encoded, not %q", ops[0])
		}
		if len(ops) > 1 {
			return fmt.Errorf("isb takes at most one operand")
		}
		p.Emit(Isb())
		return nil
	case "ldxr", "ldaxr":
		return a.loadExclusive(mn == "ldaxr")
	case "stxr", "stlxr":
		return a.storeExclusive(mn == "stlxr")
	}
	if c, ok := condSuffix(mn, "b."); ok {
		if len(ops) != 1 {
			return fmt.Errorf("b.%s takes one label", c)
		}
		p.Bcond(condByName(c), ops[0])
		return nil
	}
	return fmt.Errorf("unsupported mnemonic %q", mn)
}

// asmCtx carries the parsed instruction while its operands are decoded.
type asmCtx struct {
	p   *Program
	mn  string
	ops []string
}

// reg parses operand i as a general register, returning it with its width.
//
// A d/s register here is an error rather than an integer register of the same
// number: the encoders reached from this parser are all integer ones, so
// accepting `mov d0, d1` would quietly encode `mov x0, x1` -- a bit-copy between
// entirely different registers. The FP encoders exist (Fadd, FmovReg, LdrFP and
// the rest); the parser does not reach them yet, and saying so is the honest
// answer until it does.
func (a *asmCtx) reg(i int) (Reg, bool, bool, error) {
	if i >= len(a.ops) {
		return 0, false, false, fmt.Errorf("%s: missing operand %d", a.mn, i+1)
	}
	r, w64, flt, ok := parseReg(a.ops[i])
	if !ok {
		return 0, false, false, fmt.Errorf("%s: %q is not a register", a.mn, a.ops[i])
	}
	if flt {
		return 0, false, false, fmt.Errorf("%s: %q is a floating-point register, which this assembler cannot encode here", a.mn, a.ops[i])
	}
	return r, w64, flt, nil
}

// imm parses operand i as an immediate (#N or a bare number).
func (a *asmCtx) imm(i int) (int64, bool) {
	if i >= len(a.ops) {
		return 0, false
	}
	return parseImm(a.ops[i])
}

func (a *asmCtx) arith3(enc func(w64 bool, rd, rn, rm Reg) uint32) error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	rm, _, _, err := a.reg(2)
	if err != nil {
		return err
	}
	a.p.Emit(enc(w, rd, rn, rm))
	return nil
}

func (a *asmCtx) arith2(enc func(w64 bool, rd, rn Reg) uint32) error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	a.p.Emit(enc(w, rd, rn))
	return nil
}

func (a *asmCtx) mov() error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	if v, ok := a.imm(1); ok {
		if v >= 0 && v <= 0xffff {
			a.p.Emit(Movz(w, rd, uint16(v), 0))
			return nil
		}
		return fmt.Errorf("mov: immediate %d needs a movz/movk sequence", v)
	}
	rm, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	a.p.Emit(MovReg(w, rd, rm))
	return nil
}

func (a *asmCtx) addSub(sub bool) error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	if v, ok := a.imm(2); ok {
		if v < 0 || v > 0xfff {
			return fmt.Errorf("%s: immediate %d out of range", a.mn, v)
		}
		if sub {
			a.p.Emit(SubImm(w, rd, rn, uint32(v)))
		} else {
			a.p.Emit(AddImm(w, rd, rn, uint32(v)))
		}
		return nil
	}
	rm, _, _, err := a.reg(2)
	if err != nil {
		return err
	}
	if sub {
		a.p.Emit(SubReg(w, rd, rn, rm))
	} else {
		a.p.Emit(AddReg(w, rd, rn, rm))
	}
	return nil
}

func (a *asmCtx) logical(mn string) error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	rm, _, _, err := a.reg(2)
	if err != nil {
		return err
	}
	var enc func(bool, Reg, Reg, Reg) uint32
	switch mn {
	case "and":
		enc = AndReg
	case "orr":
		enc = OrrReg
	case "eor":
		enc = EorReg
	case "bic":
		enc = BicReg
	}
	a.p.Emit(enc(w, rd, rn, rm))
	return nil
}

func (a *asmCtx) shift(mn string) error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	if v, ok := a.imm(2); ok {
		switch mn {
		case "lsl":
			a.p.Emit(LslImm(w, rd, rn, uint32(v)))
		case "lsr":
			a.p.Emit(LsrImm(w, rd, rn, uint32(v)))
		case "asr":
			a.p.Emit(AsrImm(w, rd, rn, uint32(v)))
		}
		return nil
	}
	rm, _, _, err := a.reg(2)
	if err != nil {
		return err
	}
	switch mn {
	case "lsl":
		a.p.Emit(Lslv(w, rd, rn, rm))
	case "lsr":
		a.p.Emit(Lsrv(w, rd, rn, rm))
	case "asr":
		a.p.Emit(Asrv(w, rd, rn, rm))
	}
	return nil
}

func (a *asmCtx) cmp() error {
	rn, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	if v, ok := a.imm(1); ok {
		if v < 0 || v > 0xfff {
			return fmt.Errorf("cmp: immediate %d out of range", v)
		}
		a.p.Emit(CmpImm(w, rn, uint32(v)))
		return nil
	}
	rm, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	a.p.Emit(CmpReg(w, rn, rm))
	return nil
}

func (a *asmCtx) loadStore(mn string) error {
	rt, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	base, off, err := a.mem(1)
	if err != nil {
		return err
	}
	if off < 0 {
		return fmt.Errorf("%s: negative offset unsupported", mn)
	}
	switch mn {
	case "ldr":
		a.p.Emit(LdrImm(w, rt, base, uint32(off)))
	case "ldrb":
		a.p.Emit(LdrbImm(rt, base, uint32(off)))
	case "ldrh":
		a.p.Emit(LdrhImm(rt, base, uint32(off)))
	case "ldrsw":
		a.p.Emit(LdrswImm(rt, base, uint32(off)))
	case "str":
		a.p.Emit(StrImm(w, rt, base, uint32(off)))
	case "strb":
		a.p.Emit(StrbImm(rt, base, uint32(off)))
	case "strh":
		a.p.Emit(StrhImm(rt, base, uint32(off)))
	}
	return nil
}

// mem parses a memory operand [base] or [base, #off] spanning operands i.. (the
// bracketed form is split across commas, so it may occupy one or two ops).
func (a *asmCtx) mem(i int) (Reg, int64, error) {
	if i >= len(a.ops) {
		return 0, 0, fmt.Errorf("%s: missing memory operand", a.mn)
	}
	joined := strings.Join(a.ops[i:], ", ")
	inner := strings.TrimSpace(joined)
	if !strings.HasPrefix(inner, "[") || !strings.HasSuffix(inner, "]") {
		return 0, 0, fmt.Errorf("%s: %q is not a memory operand", a.mn, joined)
	}
	inner = strings.TrimSpace(inner[1 : len(inner)-1])
	parts := strings.SplitN(inner, ",", 2)
	base, _, baseFlt, ok := parseReg(strings.TrimSpace(parts[0]))
	if !ok {
		return 0, 0, fmt.Errorf("%s: %q is not a base register", a.mn, parts[0])
	}
	if baseFlt {
		return 0, 0, fmt.Errorf("%s: %q is not a base register", a.mn, parts[0])
	}
	var off int64
	if len(parts) == 2 {
		v, ok := parseImm(strings.TrimSpace(parts[1]))
		if !ok {
			return 0, 0, fmt.Errorf("%s: %q is not an offset", a.mn, parts[1])
		}
		off = v
	}
	return base, off, nil
}

func (a *asmCtx) branchLabel(emit func(string)) error {
	if len(a.ops) != 1 {
		return fmt.Errorf("%s takes one target", a.mn)
	}
	emit(a.ops[0])
	return nil
}

func (a *asmCtx) cbr(emit func(bool, Reg, string)) error {
	r, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	if len(a.ops) != 2 {
		return fmt.Errorf("%s takes a register and a label", a.mn)
	}
	emit(w, r, a.ops[1])
	return nil
}

// --- lexing ---------------------------------------------------------------

func splitMnemonic(line string) (string, string) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], line[i+1:]
}

// splitOperands splits on commas but keeps a bracketed [..] group together only
// insofar as the caller (mem) rejoins; here a plain comma split suffices because
// mem rejoins its trailing operands.
func splitOperands(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseReg(s string) (Reg, bool, bool, bool) {
	switch s {
	case "sp", "wsp":
		return SP, s == "sp", false, true
	case "xzr":
		return ZR, true, false, true
	case "wzr":
		return ZR, false, false, true
	}
	if len(s) < 2 {
		return 0, false, false, false
	}
	n, err := strconv.Atoi(s[1:])
	if err != nil || n < 0 || n > 31 {
		return 0, false, false, false
	}
	switch s[0] {
	case 'x':
		return Reg(n), true, false, true
	case 'w':
		return Reg(n), false, false, true
	case 'd':
		return Reg(n), true, true, true
	case 's':
		return Reg(n), false, true, true
	}
	return 0, false, false, false
}

func parseImm(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// condSuffix returns the condition part of a "prefix<cond>" mnemonic.
func condSuffix(mn, prefix string) (string, bool) {
	if strings.HasPrefix(mn, prefix) {
		return mn[len(prefix):], true
	}
	return "", false
}

func condByName(c string) Cond {
	switch c {
	case "eq":
		return EQ
	case "ne":
		return NE
	case "lt":
		return LT
	case "le":
		return LE
	case "gt":
		return GT
	case "ge":
		return GE
	case "lo", "cc":
		return CC
	case "ls":
		return LS
	case "hi":
		return HI
	case "hs", "cs":
		return CS
	}
	return EQ
}

// --- floating point --------------------------------------------------------
//
// The FP encoders (Fadd, FmovReg, Fcvt and the rest) have been in this package
// all along; nothing here could reach them, so `mov d0, d1` was refused. These
// are the parser's side of them.

// fpr parses operand i as a floating-point register, returning it and whether it
// is a double. An x/w name here is an error for the same reason a d name is an
// error in an integer instruction: the register files share an encoding, so
// taking one for the other assembles cleanly onto the wrong register.
func (a *asmCtx) fpr(i int) (Reg, bool, error) {
	if i >= len(a.ops) {
		return 0, false, fmt.Errorf("%s: missing operand %d", a.mn, i+1)
	}
	r, dbl, flt, ok := parseReg(a.ops[i])
	if !ok {
		return 0, false, fmt.Errorf("%s: %q is not a register", a.mn, a.ops[i])
	}
	if !flt {
		return 0, false, fmt.Errorf("%s: %q is a general register, and this instruction takes a floating-point one", a.mn, a.ops[i])
	}
	return r, dbl, nil
}

// fpSame parses n FP operands and requires them all to be the same width: `fadd
// s0, d1, s2` names no instruction.
func (a *asmCtx) fpSame(n int) ([]Reg, bool, error) {
	regs := make([]Reg, n)
	var dbl bool
	for i := 0; i < n; i++ {
		r, d, err := a.fpr(i)
		if err != nil {
			return nil, false, err
		}
		if i == 0 {
			dbl = d
		} else if d != dbl {
			return nil, false, fmt.Errorf("%s: operands are not all the same width", a.mn)
		}
		regs[i] = r
	}
	return regs, dbl, nil
}

func (a *asmCtx) fp3(enc func(dbl bool, rd, rn, rm Reg) uint32) error {
	r, dbl, err := a.fpSame(3)
	if err != nil {
		return err
	}
	a.p.Emit(enc(dbl, r[0], r[1], r[2]))
	return nil
}

func (a *asmCtx) fp2(enc func(dbl bool, rd, rn Reg) uint32) error {
	r, dbl, err := a.fpSame(2)
	if err != nil {
		return err
	}
	a.p.Emit(enc(dbl, r[0], r[1]))
	return nil
}

func (a *asmCtx) fcmp() error {
	r, dbl, err := a.fpSame(2)
	if err != nil {
		return err
	}
	a.p.Emit(Fcmp(dbl, r[0], r[1]))
	return nil
}

// fcvt converts between the FP widths, so its operands deliberately differ.
func (a *asmCtx) fcvt() error {
	rd, dstDbl, err := a.fpr(0)
	if err != nil {
		return err
	}
	rn, srcDbl, err := a.fpr(1)
	if err != nil {
		return err
	}
	if dstDbl == srcDbl {
		return fmt.Errorf("fcvt: %q and %q are the same width, so there is nothing to convert", a.ops[0], a.ops[1])
	}
	if dstDbl {
		a.p.Emit(FcvtStoD(rd, rn))
	} else {
		a.p.Emit(FcvtDtoS(rd, rn))
	}
	return nil
}

// fmov is three instructions wearing one mnemonic: an FP copy, and a bit-for-bit
// move each way between the register files. The operands say which.
func (a *asmCtx) fmov() error {
	if len(a.ops) != 2 {
		return fmt.Errorf("fmov takes two operands")
	}
	_, dstDbl, dstFlt, dstOK := parseReg(a.ops[0])
	_, srcDbl, srcFlt, srcOK := parseReg(a.ops[1])
	if !dstOK || !srcOK {
		return fmt.Errorf("fmov: %q, %q: not registers", a.ops[0], a.ops[1])
	}
	rd, rn := mustReg(a.ops[0]), mustReg(a.ops[1])
	switch {
	case dstFlt && srcFlt:
		if dstDbl != srcDbl {
			return fmt.Errorf("fmov: %q and %q are different widths", a.ops[0], a.ops[1])
		}
		a.p.Emit(FmovReg(dstDbl, rd, rn))
	case dstFlt: // fmov d0, x1: an integer register's bits into an FP one
		if dstDbl != srcDbl {
			return fmt.Errorf("fmov: %q and %q are different widths", a.ops[0], a.ops[1])
		}
		a.p.Emit(FmovFromGP(dstDbl, rd, rn))
	case srcFlt: // fmov x0, d1: the other way
		if dstDbl != srcDbl {
			return fmt.Errorf("fmov: %q and %q are different widths", a.ops[0], a.ops[1])
		}
		a.p.Emit(FmovToGP(srcDbl, rd, rn))
	default:
		return fmt.Errorf("fmov: %q, %q: at least one operand must be a floating-point register", a.ops[0], a.ops[1])
	}
	return nil
}

// fpIntConv is the four conversions between an integer register and an FP one.
// Each names two widths independently -- `scvtf d0, w1` is an int to a double --
// so neither operand's width implies the other's.
func (a *asmCtx) fpIntConv(mn string) error {
	if len(a.ops) != 2 {
		return fmt.Errorf("%s takes two operands", mn)
	}
	toFloat := mn == "scvtf" || mn == "ucvtf"
	fpIdx, gpIdx := 0, 1
	if !toFloat {
		fpIdx, gpIdx = 1, 0
	}
	fp, dbl, err := a.fpr(fpIdx)
	if err != nil {
		return err
	}
	gp, w64, _, err := a.reg(gpIdx)
	if err != nil {
		return err
	}
	switch mn {
	case "scvtf":
		a.p.Emit(Scvtf(dbl, w64, fp, gp))
	case "ucvtf":
		a.p.Emit(Ucvtf(dbl, w64, fp, gp))
	case "fcvtzs":
		a.p.Emit(Fcvtzs(w64, dbl, gp, fp))
	case "fcvtzu":
		a.p.Emit(Fcvtzu(w64, dbl, gp, fp))
	}
	return nil
}

// mustReg is parseReg for a name already known to parse.
func mustReg(s string) Reg { r, _, _, _ := parseReg(s); return r }

// --- the rest of the integer set -------------------------------------------
//
// Encoders that were also unreachable: the register-indirect branches, the
// extends, the multiply-accumulates, the bitfield extract and its rotate alias,
// the conditional select, the system register read, and the load/store pair.

func (a *asmCtx) reg1(enc func(Reg) uint32) error {
	rn, _, _, err := a.reg(0)
	if err != nil {
		return err
	}
	a.p.Emit(enc(rn))
	return nil
}

func (a *asmCtx) arith4(enc func(w64 bool, rd, rn, rm, ra Reg) uint32) error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	rm, _, _, err := a.reg(2)
	if err != nil {
		return err
	}
	ra, _, _, err := a.reg(3)
	if err != nil {
		return err
	}
	a.p.Emit(enc(w, rd, rn, rm, ra))
	return nil
}

// extend sign- or zero-extends a narrow value. The source is always a w
// register: the bits above the extended width are what the instruction supplies.
func (a *asmCtx) extend(mn string) error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, srcW64, _, err := a.reg(1)
	if err != nil {
		return err
	}
	if srcW64 {
		return fmt.Errorf("%s: the source %q is an x register; an extend reads a w one", mn, a.ops[1])
	}
	switch mn {
	case "sxtb":
		a.p.Emit(Sxtb(w, rd, rn))
	case "sxth":
		a.p.Emit(Sxth(w, rd, rn))
	case "sxtw":
		if !w {
			return fmt.Errorf("sxtw: the destination %q is a w register; sxtw widens to an x", a.ops[0])
		}
		a.p.Emit(Sxtw(rd, rn))
	case "uxtb":
		a.p.Emit(Uxtb(rd, rn))
	case "uxth":
		a.p.Emit(Uxth(rd, rn))
	}
	return nil
}

func (a *asmCtx) extr() error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	rm, _, _, err := a.reg(2)
	if err != nil {
		return err
	}
	lsb, ok := a.imm(3)
	if !ok {
		return fmt.Errorf("extr: the shift must be an immediate")
	}
	a.p.Emit(Extr(w, rd, rn, rm, uint32(lsb)))
	return nil
}

// ror is EXTR with one source, so the assembler spells it separately.
func (a *asmCtx) ror() error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	sh, ok := a.imm(2)
	if !ok {
		return fmt.Errorf("ror: the shift must be an immediate (a register rotate is not encoded here)")
	}
	a.p.Emit(RorImm(w, rd, rn, uint32(sh)))
	return nil
}

func (a *asmCtx) csel() error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, _, _, err := a.reg(1)
	if err != nil {
		return err
	}
	rm, _, _, err := a.reg(2)
	if err != nil {
		return err
	}
	if len(a.ops) != 4 {
		return fmt.Errorf("csel takes three registers and a condition")
	}
	c, ok := condNamed(a.ops[3])
	if !ok {
		return fmt.Errorf("csel: %q is not a condition", a.ops[3])
	}
	a.p.Emit(Csel(w, rd, rn, rm, c))
	return nil
}

func (a *asmCtx) cset() error {
	rd, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	if len(a.ops) != 2 {
		return fmt.Errorf("cset takes a register and a condition")
	}
	c, ok := condNamed(a.ops[1])
	if !ok {
		return fmt.Errorf("cset: %q is not a condition", a.ops[1])
	}
	a.p.Emit(Cset(w, rd, c))
	return nil
}

// mrs reads a system register. Only the thread pointer is encoded: it is the one
// the compiler itself emits, and a wrong system register number is not something
// to guess at.
func (a *asmCtx) mrs() error {
	rt, w64, _, err := a.reg(0)
	if err != nil {
		return err
	}
	if len(a.ops) != 2 {
		return fmt.Errorf("mrs takes a register and a system register")
	}
	if !w64 {
		return fmt.Errorf("mrs: %q is a w register; a system register read gives an x", a.ops[0])
	}
	if strings.ToLower(a.ops[1]) != "tpidr_el0" {
		return fmt.Errorf("mrs: %q is not a system register this assembler encodes (only tpidr_el0)", a.ops[1])
	}
	a.p.Emit(MrsTPIDR(rt))
	return nil
}

// pair is ldp/stp in the signed-offset form: `ldp x0, x1, [x2, #16]`. The
// writeback forms the prologue uses are emitted by the compiler directly.
func (a *asmCtx) pair(load bool) error {
	rt, w, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rt2, w2, _, err := a.reg(1)
	if err != nil {
		return err
	}
	if w != w2 {
		return fmt.Errorf("%s: %q and %q are different widths", a.mn, a.ops[0], a.ops[1])
	}
	base, off, err := a.mem(2)
	if err != nil {
		return err
	}
	if load {
		a.p.Emit(Ldp(w, rt, rt2, base, int(off), SignedOffset))
	} else {
		a.p.Emit(Stp(w, rt, rt2, base, int(off), SignedOffset))
	}
	return nil
}

// condNamed resolves a condition name, reporting false for one that is not a
// condition at all -- condByName alone cannot say, since it has to return
// something.
func condNamed(s string) (Cond, bool) {
	switch strings.ToLower(s) {
	case "eq", "ne", "cs", "hs", "cc", "lo", "mi", "pl", "vs", "vc",
		"hi", "ls", "ge", "lt", "gt", "le":
		return condByName(strings.ToLower(s)), true
	}
	return 0, false
}

// barrier assembles DMB/DSB <option>, whose sole operand is a domain name rather
// than a register or immediate.
func (a *asmCtx) barrier(mn string) error {
	if len(a.ops) != 1 {
		return fmt.Errorf("%s takes one barrier option", mn)
	}
	o, ok := barrierByName(a.ops[0])
	if !ok {
		return fmt.Errorf("%s: %q is not a barrier option this assembler encodes", mn, a.ops[0])
	}
	if mn == "dmb" {
		a.p.Emit(Dmb(o))
	} else {
		a.p.Emit(Dsb(o))
	}
	return nil
}

// barrierByName maps a barrier domain name to its option. Only the domains an
// atomic sequence actually uses are encoded; a name outside the set is refused
// rather than encoded as the wrong domain.
func barrierByName(s string) (BarrierOption, bool) {
	switch s {
	case "sy":
		return BarrierSY, true
	case "ish":
		return BarrierISH, true
	case "ishst":
		return BarrierISHST, true
	case "ld":
		return BarrierLD, true
	case "st":
		return BarrierST, true
	}
	return 0, false
}

// loadExclusive assembles LDXR/LDAXR <Wt|Xt>, [<Xn>].
func (a *asmCtx) loadExclusive(acquire bool) error {
	rt, w64, _, err := a.reg(0)
	if err != nil {
		return err
	}
	rn, off, err := a.mem(1)
	if err != nil {
		return err
	}
	if off != 0 {
		return fmt.Errorf("%s: an exclusive access takes no offset", a.mn)
	}
	if acquire {
		a.p.Emit(Ldaxr(w64, rt, rn))
	} else {
		a.p.Emit(Ldxr(w64, rt, rn))
	}
	return nil
}

// storeExclusive assembles STXR/STLXR <Ws>, <Wt|Xt>, [<Xn>]. The status register
// Ws is always a 32-bit register; the width of the stored value comes from Wt/Xt.
func (a *asmCtx) storeExclusive(release bool) error {
	rs, sw, _, err := a.reg(0)
	if err != nil {
		return err
	}
	if sw {
		return fmt.Errorf("%s: the status register must be 32-bit (w%d, not x%d)", a.mn, rs, rs)
	}
	rt, w64, _, err := a.reg(1)
	if err != nil {
		return err
	}
	rn, off, err := a.mem(2)
	if err != nil {
		return err
	}
	if off != 0 {
		return fmt.Errorf("%s: an exclusive access takes no offset", a.mn)
	}
	if release {
		a.p.Emit(Stlxr(w64, rs, rt, rn))
	} else {
		a.p.Emit(Stxr(w64, rs, rt, rn))
	}
	return nil
}
