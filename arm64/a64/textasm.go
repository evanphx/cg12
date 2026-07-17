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
