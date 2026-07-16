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
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
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
	for _, c := range []string{"#", "//", ";"} {
		if i := strings.Index(s, c); i >= 0 {
			s = s[:i]
		}
	}
	return s
}

func asmLine(p *Program, line string) error {
	mn, rest := splitFields(line)
	ops := splitOperands(rest)

	// Control flow (no size suffix).
	switch mn {
	case "nop":
		return emit0(p, ops, []byte{0x90})
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
		if dr, ok := parseReg(dst); ok {
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
	sr, srcReg := parseReg(src)
	dr, dstReg := parseReg(dst)
	switch {
	case srcReg && dstReg:
		a.p.Emit(MovReg(a.w, dr, sr))
	case srcReg: // store: reg -> mem
		m, ok := parseMem(dst)
		if !ok {
			return fmt.Errorf("mov: bad destination %q", dst)
		}
		a.p.Emit(Store(a.sz*8, sr, m))
	case dstReg: // load: mem -> reg
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

func (a *asmCtx) binary() error {
	if len(a.ops) != 2 {
		return fmt.Errorf("%s takes two operands", a.base)
	}
	src, dst := a.ops[0], a.ops[1]
	dr, ok := parseReg(dst)
	if !ok {
		return fmt.Errorf("%s: destination %q must be a register", a.base, dst)
	}
	if imm, ok := parseImm(src); ok {
		enc := map[string]func(bool, Reg, int32) []byte{
			"add": AddImm, "sub": SubImm, "and": AndImm, "or": OrImm, "xor": XorImm, "cmp": CmpImm,
		}[a.base]
		a.p.Emit(enc(a.w, dr, int32(imm)))
		return nil
	}
	sr, ok := parseReg(src)
	if !ok {
		return fmt.Errorf("%s: source %q must be a register or immediate", a.base, src)
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
	sr, ok := parseReg(a.ops[0])
	if !ok {
		return fmt.Errorf("imul: source must be a register")
	}
	dr, ok := parseReg(a.ops[1])
	if !ok {
		return fmt.Errorf("imul: destination must be a register")
	}
	a.p.Emit(Imul(a.w, dr, sr))
	return nil
}

func (a *asmCtx) unary() error {
	if len(a.ops) != 1 {
		return fmt.Errorf("%s takes one operand", a.base)
	}
	dr, ok := parseReg(a.ops[0])
	if !ok {
		return fmt.Errorf("%s: operand must be a register", a.base)
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
	dr, ok := parseReg(a.ops[1])
	if !ok {
		return fmt.Errorf("%s: destination must be a register", a.base)
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
	dr, ok := parseReg(a.ops[len(a.ops)-1])
	if !ok {
		return fmt.Errorf("lea: destination must be a register")
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
	base, ok := parseReg(strings.TrimSpace(s[open+1 : len(s)-1]))
	if !ok {
		return Mem{}, false
	}
	return At(base, int32(disp)), true
}

// attRegs maps every AT&T register name to its 4-bit encoding.
var attRegs = func() map[string]Reg {
	m := map[string]Reg{}
	names := [16][4]string{
		{"al", "ax", "eax", "rax"}, {"cl", "cx", "ecx", "rcx"},
		{"dl", "dx", "edx", "rdx"}, {"bl", "bx", "ebx", "rbx"},
		{"spl", "sp", "esp", "rsp"}, {"bpl", "bp", "ebp", "rbp"},
		{"sil", "si", "esi", "rsi"}, {"dil", "di", "edi", "rdi"},
	}
	for i, row := range names {
		for _, n := range row {
			m[n] = Reg(i)
		}
	}
	for i := 8; i <= 15; i++ {
		d := strconv.Itoa(i)
		for _, suf := range []string{"", "b", "w", "d"} {
			m["r"+d+suf] = Reg(i)
		}
	}
	return m
}()

func parseReg(s string) (Reg, bool) {
	if !strings.HasPrefix(s, "%") {
		return 0, false
	}
	r, ok := attRegs[s[1:]]
	return r, ok
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
