package x64

import (
	"fmt"
	"strconv"
	"strings"
)

// Assemble parses AT&T-syntax x86-64 assembly text -- one instruction or
// "label:" per line, with `#`, `//`, or `;` comments -- and returns the encoded
// machine code, resolving jumps to labels. It drives the same instruction
// encoders the compiler's machine-code emitter uses, so a hand-written or
// inline-asm sequence can be turned into object code without an external
// assembler.
//
// It covers the common integer instructions (mov, add/sub/and/or/xor/cmp, imul,
// neg/not, the shifts, lea), the base and base+displacement loads and stores,
// and the branches (jmp, jCC, call, ret, nop). Unsupported mnemonics or operand
// shapes are a reported error, never silently skipped.
func Assemble(src string) ([]byte, error) {
	p, err := AssembleProgram(src)
	if err != nil {
		return nil, err
	}
	return p.Bytes()
}

// AssembleProgram parses the assembly text into a Program without resolving it,
// so a caller can turn it into a relocatable object (via Program.Link) that
// keeps external calls as relocations and exported labels as symbols. It honors
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
	name, rest := splitFields(line)
	switch name {
	case ".globl", ".global":
		if rest == "" {
			return fmt.Errorf("%s needs a symbol name", name)
		}
		p.Globl(strings.TrimSpace(rest))
	}
	return nil
}

func stripComment(s string) string {
	for _, c := range []string{"#", "//"} {
		if i := strings.Index(s, c); i >= 0 {
			s = s[:i]
		}
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

func asmLine(p *Program, line string) error {
	mn, rest := splitFields(line)
	ops := splitOperands(rest)

	// No size suffix: the operand size is the instruction's own.
	switch mn {
	case "nop":
		return emit0(p, ops, []byte{0x90})
	case "syscall":
		return emit0(p, ops, Syscall())
	case "cdq": // sign-extend eax into edx:eax, for a 32-bit idiv
		return emit0(p, ops, Cdq())
	case "cqo": // and rax into rdx:rax, for a 64-bit one
		return emit0(p, ops, Cqo())
	case "ret":
		return emit0(p, ops, Ret())
	case "jmp":
		return branch(p, ops, func(l string) { p.Jmp(l) })
	case "call":
		return branch(p, ops, func(l string) { p.Call(l) })
	}
	if strings.HasPrefix(mn, "j") {
		if c, ok := condByName(mn[1:]); ok {
			return branch(p, ops, func(l string) { p.Jcc(c, l) })
		}
	}
	// The SSE set, before the suffix split below: these mnemonics carry their
	// width in their own name (ss is four bytes, sd is eight), so the trailing
	// letter is part of the instruction rather than a size suffix. Splitting
	// "addsd" into "adds" + d would invent an instruction that does not exist.
	if err, ok := sse(p, mn, ops); ok {
		return err
	}

	// Size-suffixed instructions: the last character is q/l/w/b.
	base, w, sz, ok := splitSuffix(mn)
	if !ok {
		return fmt.Errorf("unsupported mnemonic %q", mn)
	}
	a := &asmCtx{p: p, mn: mn, base: base, w: w, sz: sz, ops: ops}
	switch base {
	case "mov":
		return a.mov()
	case "add", "sub", "and", "or", "xor", "cmp":
		return a.binary()
	case "imul":
		return a.imul()
	case "neg", "not":
		return a.unary()
	case "test":
		return a.test()
	case "push":
		return a.pushPop(true)
	case "pop":
		return a.pushPop(false)
	case "div", "idiv":
		return a.divide(base == "idiv")
	case "shl", "shr", "sar":
		return a.shift()
	case "lea":
		return a.lea()
	}
	return fmt.Errorf("unsupported mnemonic %q", mn)
}

type asmCtx struct {
	p    *Program
	mn   string
	base string
	w    bool
	sz   int
	ops  []string
}

func (a *asmCtx) mov() error {
	if len(a.ops) != 2 {
		return fmt.Errorf("mov takes two operands")
	}
	src, dst := a.ops[0], a.ops[1]
	// register or immediate into a register or memory.
	if imm, ok := parseImm(src); ok {
		if isReg(dst) {
			dr, err := a.reg(dst)
			if err != nil {
				return err
			}
			if a.w {
				a.p.Emit(MovImm64(dr, imm))
			} else {
				a.p.Emit(MovImm32(a.w, dr, int32(imm)))
			}
			return nil
		}
		if m, ok := parseMem(dst); ok {
			a.p.Emit(StoreImm32(a.sz*8, m, int32(imm)))
			return nil
		}
		return fmt.Errorf("mov: bad destination %q", dst)
	}
	srcReg, dstReg := isReg(src), isReg(dst)
	switch {
	case srcReg && dstReg:
		sr, err := a.reg(src)
		if err != nil {
			return err
		}
		dr, err := a.reg(dst)
		if err != nil {
			return err
		}
		a.p.Emit(MovReg(a.w, dr, sr))
	case srcReg: // store: reg -> mem
		sr, err := a.reg(src)
		if err != nil {
			return err
		}
		m, ok := parseMem(dst)
		if !ok {
			return fmt.Errorf("mov: bad destination %q", dst)
		}
		a.p.Emit(Store(a.sz*8, sr, m))
	case dstReg: // load: mem -> reg
		dr, err := a.reg(dst)
		if err != nil {
			return err
		}
		m, ok := parseMem(src)
		if !ok {
			return fmt.Errorf("mov: bad source %q", src)
		}
		a.p.Emit(Load(a.w, dr, m))
	default:
		return fmt.Errorf("mov: unsupported operands")
	}
	return nil
}

// reg parses an operand as a register of this instruction's operand size. The
// mnemonic's suffix and the register's name must agree: `movl %al` names a byte
// register in a 32-bit instruction, and picking either width over the other
// would be a guess.
//
// A size this assembler has no encoders for is refused outright. The register
// encoding is the same 4 bits at every width, so a byte instruction reaching a
// 32-bit encoder assembles cleanly and writes four times what was asked --
// `movb %al, %bl` and `movl %eax, %ebx` are the same bytes.
func (a *asmCtx) reg(s string) (Reg, error) {
	r, sz, ok := parseReg(s)
	if !ok {
		return 0, fmt.Errorf("%s: %q is not a register", a.mn, s)
	}
	if sz != a.sz {
		return 0, fmt.Errorf("%s: %q is a %d-byte register in a %d-byte instruction", a.mn, s, sz, a.sz)
	}
	if a.sz != 4 && a.sz != 8 {
		return 0, fmt.Errorf("%s: %d-byte register operands are not encoded by this assembler", a.mn, a.sz)
	}
	return r, nil
}

func (a *asmCtx) binary() error {
	if len(a.ops) != 2 {
		return fmt.Errorf("%s takes two operands", a.base)
	}
	src, dst := a.ops[0], a.ops[1]
	dr, err := a.reg(dst)
	if err != nil {
		return err
	}
	if imm, ok := parseImm(src); ok {
		enc := map[string]func(bool, Reg, int32) []byte{
			"add": AddImm, "sub": SubImm, "and": AndImm, "or": OrImm, "xor": XorImm, "cmp": CmpImm,
		}[a.base]
		a.p.Emit(enc(a.w, dr, int32(imm)))
		return nil
	}
	sr, err := a.reg(src)
	if err != nil {
		return err
	}
	enc := map[string]func(bool, Reg, Reg) []byte{
		"add": AddReg, "sub": SubReg, "and": AndReg, "or": OrReg, "xor": XorReg, "cmp": CmpReg,
	}[a.base]
	a.p.Emit(enc(a.w, dr, sr))
	return nil
}

func (a *asmCtx) imul() error {
	if len(a.ops) != 2 {
		return fmt.Errorf("imul takes two operands")
	}
	sr, err := a.reg(a.ops[0])
	if err != nil {
		return err
	}
	dr, err := a.reg(a.ops[1])
	if err != nil {
		return err
	}
	a.p.Emit(Imul(a.w, dr, sr))
	return nil
}

func (a *asmCtx) unary() error {
	if len(a.ops) != 1 {
		return fmt.Errorf("%s takes one operand", a.base)
	}
	dr, err := a.reg(a.ops[0])
	if err != nil {
		return err
	}
	if a.base == "neg" {
		a.p.Emit(Neg(a.w, dr))
	} else {
		a.p.Emit(Not(a.w, dr))
	}
	return nil
}

func (a *asmCtx) shift() error {
	if len(a.ops) != 2 {
		return fmt.Errorf("%s takes two operands", a.base)
	}
	dr, err := a.reg(a.ops[1])
	if err != nil {
		return err
	}
	if a.ops[0] == "%cl" {
		switch a.base {
		case "shl":
			a.p.Emit(ShlCL(a.w, dr))
		case "shr":
			a.p.Emit(ShrCL(a.w, dr))
		case "sar":
			a.p.Emit(SarCL(a.w, dr))
		}
		return nil
	}
	v, ok := parseImm(a.ops[0])
	if !ok {
		return fmt.Errorf("%s: count must be $imm or %%cl", a.base)
	}
	switch a.base {
	case "shl":
		a.p.Emit(ShlImm(a.w, dr, byte(v)))
	case "shr":
		a.p.Emit(ShrImm(a.w, dr, byte(v)))
	case "sar":
		a.p.Emit(SarImm(a.w, dr, byte(v)))
	}
	return nil
}

func (a *asmCtx) lea() error {
	if len(a.ops) < 2 {
		return fmt.Errorf("lea takes a memory operand and a register")
	}
	dr, err := a.reg(a.ops[len(a.ops)-1])
	if err != nil {
		return err
	}
	m, ok := parseMem(strings.Join(a.ops[:len(a.ops)-1], ", "))
	if !ok {
		return fmt.Errorf("lea: bad memory operand")
	}
	a.p.Emit(Lea(a.w, dr, m))
	return nil
}

func emit0(p *Program, ops []string, code []byte) error {
	if len(ops) != 0 {
		return fmt.Errorf("instruction takes no operands")
	}
	p.Emit(code)
	return nil
}

func branch(p *Program, ops []string, emit func(string)) error {
	if len(ops) != 1 {
		return fmt.Errorf("branch takes one target")
	}
	emit(ops[0])
	return nil
}

// --- lexing ---------------------------------------------------------------

func splitFields(line string) (string, string) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}

// splitOperands splits on commas that are not inside parentheses (a memory
// operand like 8(%rbp) contains none, but a scaled index would).
func splitOperands(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

// splitSuffix strips an AT&T size suffix (q/l/w/b) from a mnemonic.
func splitSuffix(mn string) (base string, w bool, size int, ok bool) {
	if len(mn) < 2 {
		return "", false, 0, false
	}
	switch mn[len(mn)-1] {
	case 'q':
		return mn[:len(mn)-1], true, 8, true
	case 'l':
		return mn[:len(mn)-1], false, 4, true
	case 'w':
		return mn[:len(mn)-1], false, 2, true
	case 'b':
		return mn[:len(mn)-1], false, 1, true
	}
	return "", false, 0, false
}

func parseImm(s string) (int64, bool) {
	if !strings.HasPrefix(s, "$") {
		return 0, false
	}
	v, err := strconv.ParseInt(s[1:], 0, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseMem parses disp(%base) or (%base).
func parseMem(s string) (Mem, bool) {
	s = strings.TrimSpace(s)
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return Mem{}, false
	}
	var disp int64
	if d := strings.TrimSpace(s[:open]); d != "" {
		v, err := strconv.ParseInt(d, 0, 64)
		if err != nil {
			return Mem{}, false
		}
		disp = v
	}
	base, bsz, ok := parseReg(strings.TrimSpace(s[open+1 : len(s)-1]))
	if !ok || bsz != 8 {
		return Mem{}, false // an address is held in a 64-bit register
	}
	return At(base, int32(disp)), true
}

// attReg is a register named in AT&T syntax: which register, and at what width.
// The name says both -- %al and %eax are the same register through different
// windows, and an instruction that takes one will not do for the other.
type attReg struct {
	reg  Reg
	size int // 1, 2, 4 or 8 bytes
}

// attRegs maps every AT&T register name to its encoding and its width.
var attRegs = func() map[string]attReg {
	m := map[string]attReg{}
	names := [16][4]string{
		{"al", "ax", "eax", "rax"}, {"cl", "cx", "ecx", "rcx"},
		{"dl", "dx", "edx", "rdx"}, {"bl", "bx", "ebx", "rbx"},
		{"spl", "sp", "esp", "rsp"}, {"bpl", "bp", "ebp", "rbp"},
		{"sil", "si", "esi", "rsi"}, {"dil", "di", "edi", "rdi"},
	}
	sizes := [4]int{1, 2, 4, 8}
	for i, row := range names {
		for j, n := range row {
			m[n] = attReg{Reg(i), sizes[j]}
		}
	}
	for i := 8; i <= 15; i++ {
		d := strconv.Itoa(i)
		for suf, sz := range map[string]int{"b": 1, "w": 2, "d": 4, "": 8} {
			m["r"+d+suf] = attReg{Reg(i), sz}
		}
	}
	// The SSE registers. Their size is recorded as 0: an XMM name selects no width
	// -- movss and movsd differ in the mnemonic, not the register -- so there is
	// nothing for asmCtx.reg's suffix check to compare against, and an XMM operand
	// goes through xmm() instead.
	for i := 0; i <= 15; i++ {
		m["xmm"+strconv.Itoa(i)] = attReg{XMM0 + Reg(i), 0}
	}
	return m
}()

// isReg reports whether an operand names a register, at any width. The width
// check belongs to asmCtx.reg, which knows the instruction's size.
func isReg(s string) bool { _, _, ok := parseReg(s); return ok }

// parseReg returns the register an AT&T name refers to, and the width the name
// selects.
func parseReg(s string) (Reg, int, bool) {
	if !strings.HasPrefix(s, "%") {
		return 0, 0, false
	}
	r, ok := attRegs[s[1:]]
	return r.reg, r.size, ok
}

func condByName(c string) (Cond, bool) {
	m := map[string]Cond{
		"o": O, "no": NO, "b": B, "c": B, "ae": AE, "nc": AE,
		"e": E, "z": E, "ne": NE, "nz": NE, "be": BE, "a": A,
		"s": S, "ns": NS, "p": P, "np": NP,
		"l": L, "ge": GE, "le": LE, "g": G,
	}
	v, ok := m[c]
	return v, ok
}

// --- forms the parser could not reach --------------------------------------
//
// The encoders for these were already here and already checked against llvm-mc.
// syscall in particular is why most inline asm on this target exists at all.

func (a *asmCtx) test() error {
	if len(a.ops) != 2 {
		return fmt.Errorf("test takes two operands")
	}
	// AT&T names the source first, and the encoders take (dst, src) -- the same
	// order AddReg and the rest are called with.
	src, err := a.reg(a.ops[0])
	if err != nil {
		return err
	}
	dst, err := a.reg(a.ops[1])
	if err != nil {
		return err
	}
	a.p.Emit(TestReg(a.w, dst, src))
	return nil
}

// pushPop moves a register to or from the stack. There is one operand size: the
// 64-bit push, which needs no suffix and takes no other width.
func (a *asmCtx) pushPop(push bool) error {
	if len(a.ops) != 1 {
		return fmt.Errorf("%s takes one operand", a.mn)
	}
	r, sz, ok := parseReg(a.ops[0])
	if !ok {
		return fmt.Errorf("%s: %q is not a register", a.mn, a.ops[0])
	}
	if sz != 8 {
		return fmt.Errorf("%s: %q is a %d-byte register; push and pop move 8 bytes on this target", a.mn, a.ops[0], sz)
	}
	if push {
		a.p.Emit(Push(r))
	} else {
		a.p.Emit(Pop(r))
	}
	return nil
}

// divide is the one-operand div/idiv: the dividend is in rdx:rax and the
// quotient and remainder come back there, so only the divisor is named.
func (a *asmCtx) divide(signed bool) error {
	if len(a.ops) != 1 {
		return fmt.Errorf("%s takes one operand (the dividend is rdx:rax)", a.mn)
	}
	r, err := a.reg(a.ops[0])
	if err != nil {
		return err
	}
	if signed {
		a.p.Emit(Idiv(a.w, r))
	} else {
		a.p.Emit(Div(a.w, r))
	}
	return nil
}

// --- the SSE set -----------------------------------------------------------
//
// Every encoder here already existed and was already checked against llvm-mc;
// none of them could be reached by name. That is the same gap the a64 side had,
// and it has the same consequence: floating-point inline asm did not compile,
// because the assembler this backend hands templates to had no word for any of
// these instructions.

// xmmOperand resolves an operand that must be an SSE register.
//
// The check is on the width the name selects, not on the register number: in
// this package an XMM register and a general one share the numbers 0..15 (they
// are separate encoding spaces), so %xmm3 and %rbx are the same Reg. What tells
// them apart is that an XMM name selects no width -- see attRegs.
func xmmOperand(mn, s string) (Reg, error) {
	r, sz, ok := parseReg(s)
	if !ok || sz != 0 {
		return 0, fmt.Errorf("%s: %q is not an SSE register", mn, s)
	}
	return r, nil
}

// gpOperand resolves an operand that must be a general register of width sz.
func gpOperand(mn, s string, sz int) (Reg, error) {
	r, got, ok := parseReg(s)
	if !ok || got == 0 {
		return 0, fmt.Errorf("%s: %q is not a general register", mn, s)
	}
	if got != sz {
		return 0, fmt.Errorf("%s: %q is a %d-byte register where %d is required", mn, s, got, sz)
	}
	return r, nil
}

// sse assembles the SSE instructions, reporting whether the mnemonic was one of
// them. AT&T order names the source first, so the destination is the last
// operand -- the same convention the integer forms above follow.
func sse(p *Program, mn string, ops []string) (error, bool) {
	rr := map[string]func(dst, src Reg) []byte{
		"addsd": Addsd, "subsd": Subsd, "mulsd": Mulsd, "divsd": Divsd,
		"addss": Addss, "subss": Subss, "mulss": Mulss, "divss": Divss,
		"ucomisd": Ucomisd, "ucomiss": Ucomiss,
		"xorps": Xorps, "xorpd": Xorpd,
		"cvtsd2ss": Cvtsd2ss, "cvtss2sd": Cvtss2sd,
	}
	if enc, ok := rr[mn]; ok {
		if len(ops) != 2 {
			return fmt.Errorf("%s takes two operands", mn), true
		}
		src, err := xmmOperand(mn, ops[0])
		if err != nil {
			return err, true
		}
		dst, err := xmmOperand(mn, ops[1])
		if err != nil {
			return err, true
		}
		p.Emit(enc(dst, src))
		return nil, true
	}

	// movss/movsd move between registers or between a register and memory, and
	// which one it is depends on where the memory operand is.
	switch mn {
	case "movsd", "movss":
		if len(ops) != 2 {
			return fmt.Errorf("%s takes two operands", mn), true
		}
		wide := mn == "movsd"
		src, dst := ops[0], ops[1]
		switch {
		case isReg(src) && isReg(dst):
			sr, err := xmmOperand(mn, src)
			if err != nil {
				return err, true
			}
			dr, err := xmmOperand(mn, dst)
			if err != nil {
				return err, true
			}
			if wide {
				p.Emit(MovsdReg(dr, sr))
			} else {
				p.Emit(MovssReg(dr, sr))
			}
		case isReg(src): // register -> memory
			sr, err := xmmOperand(mn, src)
			if err != nil {
				return err, true
			}
			m, ok := parseMem(dst)
			if !ok {
				return fmt.Errorf("%s: %q is not a memory operand", mn, dst), true
			}
			if wide {
				p.Emit(MovsdStore(sr, m))
			} else {
				p.Emit(MovssStore(sr, m))
			}
		case isReg(dst): // memory -> register
			dr, err := xmmOperand(mn, dst)
			if err != nil {
				return err, true
			}
			m, ok := parseMem(src)
			if !ok {
				return fmt.Errorf("%s: %q is not a memory operand", mn, src), true
			}
			if wide {
				p.Emit(MovsdLoad(dr, m))
			} else {
				p.Emit(MovssLoad(dr, m))
			}
		default:
			return fmt.Errorf("%s: one operand must be a register", mn), true
		}
		return nil, true
	}
	return nil, false
}
