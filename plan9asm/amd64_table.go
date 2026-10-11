package plan9asm

import (
	"strings"
)

// amd64Order says how Plan 9 operand order maps to AT&T operand order.
type amd64Order uint8

const (
	// amd64SameOrder: Plan 9 lists operands source first, destination last,
	// which is already AT&T order. This holds for nearly every instruction,
	// including the VEX/EVEX three- and four-operand forms (Plan 9 lists them
	// in reverse Intel order) and the immediate-first SSE forms (PSHUFD,
	// PALIGNR, PINSR*, PEXTR*, SHUFP*, PCMPESTRI, IMUL3, SHLD/SHRD).
	amd64SameOrder amd64Order = iota
	// amd64SwapOrder: CMP family. Plan 9 CMPQ a, b is Intel cmp a, b, which is
	// AT&T cmpq b, a.
	amd64SwapOrder
	// amd64PredicateLast: CMPPS/CMPPD/CMPSS/CMPSD put the comparison predicate
	// last (CMPPS X1, X0, $4); AT&T puts it first (cmpps $4, %xmm1, %xmm0).
	amd64PredicateLast
)

// amd64Spec describes how one Plan 9 mnemonic becomes GNU AT&T syntax.
type amd64Spec struct {
	mnemonic string
	// width is the size, in bits, of general-purpose register operands; zero
	// means the instruction takes none (a GPR operand is then an error).
	width int
	// widths overrides width per Plan 9 operand index (0 keeps width).
	widths []int
	order  amd64Order
}

func (spec amd64Spec) operandWidth(index int) int {
	if index < len(spec.widths) && spec.widths[index] != 0 {
		return spec.widths[index]
	}
	return spec.width
}

// amd64Conditions maps Go's condition-code spellings to GNU's.
var amd64Conditions = map[string]string{
	"CC": "ae",
	"CS": "b",
	"EQ": "e",
	"GE": "ge",
	"GT": "g",
	"HI": "a",
	"LE": "le",
	"LS": "be",
	"LT": "l",
	"MI": "s",
	"NE": "ne",
	"OC": "no",
	"OS": "o",
	"PC": "np",
	"PL": "ns",
	"PS": "p",
}

// amd64JumpMnemonics covers every conditional-jump spelling cmd/asm accepts
// for amd64 (arch.archX86's aliases included).
var amd64JumpMnemonics = map[string]string{
	"JCC": "jae", "JCS": "jb", "JEQ": "je", "JGE": "jge", "JGT": "jg",
	"JHI": "ja", "JLE": "jle", "JLS": "jbe", "JLT": "jl", "JMI": "js",
	"JNE": "jne", "JOC": "jno", "JOS": "jo", "JPC": "jnp", "JPL": "jns",
	"JPS": "jp",
	"JA":  "ja", "JAE": "jae", "JB": "jb", "JBE": "jbe", "JC": "jb",
	"JE": "je", "JG": "jg", "JHS": "jae", "JL": "jl", "JLO": "jb",
	"JNA": "jbe", "JNAE": "jb", "JNB": "jae", "JNBE": "ja", "JNC": "jae",
	"JNG": "jle", "JNGE": "jl", "JNL": "jge", "JNLE": "jg", "JNO": "jno",
	"JNP": "jnp", "JNS": "jns", "JNZ": "jne", "JO": "jo", "JP": "jp",
	"JPE": "jp", "JPO": "jnp", "JS": "js", "JZ": "je",
	"JCXZL": "jecxz", "JCXZQ": "jrcxz",
}

var amd64SizeWidths = map[byte]int{'B': 8, 'W': 16, 'L': 32, 'Q': 64}

// amd64SizedFamilies are the integer mnemonics spelled base+B/W/L/Q whose GNU
// spelling is the same base and suffix in lower case.
var amd64SizedFamilies = map[string]bool{
	"ADC": true, "ADD": true, "AND": true, "DEC": true, "DIV": true,
	"IDIV": true, "IMUL": true, "INC": true, "MOV": true, "MUL": true,
	"NEG": true, "NOT": true, "OR": true, "SAL": true, "SAR": true,
	"SBB": true, "SHL": true, "SHR": true, "ROL": true, "ROR": true,
	"RCL": true, "RCR": true, "SUB": true, "TEST": true, "XCHG": true,
	"XADD": true, "XOR": true, "CMPXCHG": true, "BT": true, "BTC": true,
	"BTR": true, "BTS": true, "BSF": true, "BSR": true, "LZCNT": true,
	"TZCNT": true, "POPCNT": true, "BSWAP": true, "LEA": true,
	"PUSH": true, "POP": true, "ANDN": true, "BEXTR": true, "BLSI": true,
	"BLSMSK": true, "BLSR": true, "BZHI": true, "MULX": true, "PDEP": true,
	"PEXT": true, "RORX": true, "SARX": true, "SHLX": true, "SHRX": true,
	"ADCX": true, "ADOX": true, "MOVBE": true, "CMP": true,
	"MOVNTI": true,
}

// amd64Fixed are mnemonics that need an explicit translation.
var amd64Fixed = map[string]amd64Spec{
	// Zero- and sign-extending moves.
	"MOVBLZX": {mnemonic: "movzbl", width: 32, widths: []int{8}},
	"MOVBQZX": {mnemonic: "movzbq", width: 64, widths: []int{8}},
	"MOVBWZX": {mnemonic: "movzbw", width: 16, widths: []int{8}},
	"MOVWLZX": {mnemonic: "movzwl", width: 32, widths: []int{16}},
	"MOVWQZX": {mnemonic: "movzwq", width: 64, widths: []int{16}},
	"MOVLQZX": {mnemonic: "movl", width: 32},
	"MOVBLSX": {mnemonic: "movsbl", width: 32, widths: []int{8}},
	"MOVBQSX": {mnemonic: "movsbq", width: 64, widths: []int{8}},
	"MOVBWSX": {mnemonic: "movsbw", width: 16, widths: []int{8}},
	"MOVWLSX": {mnemonic: "movswl", width: 32, widths: []int{16}},
	"MOVWQSX": {mnemonic: "movswq", width: 64, widths: []int{16}},
	"MOVLQSX": {mnemonic: "movslq", width: 64, widths: []int{32}},

	// Three-operand multiply.
	"IMUL3W": {mnemonic: "imulw", width: 16},
	"IMUL3L": {mnemonic: "imull", width: 32},
	"IMUL3Q": {mnemonic: "imulq", width: 64},

	// CRC32: the source has the suffix size, the accumulator is 32 bits
	// except for CRC32Q.
	"CRC32B": {mnemonic: "crc32b", width: 32, widths: []int{8}},
	"CRC32W": {mnemonic: "crc32w", width: 32, widths: []int{16}},
	"CRC32L": {mnemonic: "crc32l", width: 32},
	"CRC32Q": {mnemonic: "crc32q", width: 64},

	// No operands.
	"CLD": {mnemonic: "cld"}, "STD": {mnemonic: "std"}, "CLC": {mnemonic: "clc"},
	"STC": {mnemonic: "stc"}, "CMC": {mnemonic: "cmc"}, "CPUID": {mnemonic: "cpuid"},
	"RDTSC": {mnemonic: "rdtsc"}, "RDTSCP": {mnemonic: "rdtscp"},
	"SYSCALL": {mnemonic: "syscall"}, "PAUSE": {mnemonic: "pause"},
	"MFENCE": {mnemonic: "mfence"}, "LFENCE": {mnemonic: "lfence"},
	"SFENCE": {mnemonic: "sfence"}, "HLT": {mnemonic: "hlt"},
	"UD2": {mnemonic: "ud2"}, "UNDEF": {mnemonic: "ud2"},
	"CQO": {mnemonic: "cqto"}, "CDQ": {mnemonic: "cltd"}, "CWD": {mnemonic: "cwtd"},
	"CDQE": {mnemonic: "cltq"}, "CBW": {mnemonic: "cbtw"}, "CWDE": {mnemonic: "cwtl"},
	"PUSHFQ": {mnemonic: "pushfq"}, "POPFQ": {mnemonic: "popfq"},
	"PUSHFW": {mnemonic: "pushfw"}, "POPFW": {mnemonic: "popfw"},
	"LAHF": {mnemonic: "lahf"}, "SAHF": {mnemonic: "sahf"},
	"VZEROUPPER": {mnemonic: "vzeroupper"}, "VZEROALL": {mnemonic: "vzeroall"},
	"XGETBV": {mnemonic: "xgetbv"}, "XSETBV": {mnemonic: "xsetbv"},
	"EMMS": {mnemonic: "emms"}, "LEAVEQ": {mnemonic: "leaveq"},
	"ENDBR64": {mnemonic: "endbr64"}, "XTEST": {mnemonic: "xtest"},
	"XEND": {mnemonic: "xend"},

	// GNU spells these without a size suffix; the register sets the size.
	"RDRANDW": {mnemonic: "rdrand", width: 16}, "RDRANDL": {mnemonic: "rdrand", width: 32},
	"RDRANDQ": {mnemonic: "rdrand", width: 64}, "RDSEEDW": {mnemonic: "rdseed", width: 16},
	"RDSEEDL": {mnemonic: "rdseed", width: 32}, "RDSEEDQ": {mnemonic: "rdseed", width: 64},

	// String instructions.
	"MOVSB": {mnemonic: "movsb"}, "MOVSW": {mnemonic: "movsw"},
	"MOVSL": {mnemonic: "movsl"}, "MOVSQ": {mnemonic: "movsq"},
	"STOSB": {mnemonic: "stosb"}, "STOSW": {mnemonic: "stosw"},
	"STOSL": {mnemonic: "stosl"}, "STOSQ": {mnemonic: "stosq"},
	"LODSB": {mnemonic: "lodsb"}, "LODSW": {mnemonic: "lodsw"},
	"LODSL": {mnemonic: "lodsl"}, "LODSQ": {mnemonic: "lodsq"},
	"SCASB": {mnemonic: "scasb"}, "SCASW": {mnemonic: "scasw"},
	"SCASL": {mnemonic: "scasl"}, "SCASQ": {mnemonic: "scasq"},
	"CMPSB": {mnemonic: "cmpsb"}, "CMPSW": {mnemonic: "cmpsw"},
	"CMPSL": {mnemonic: "cmpsl"}, "CMPSQ": {mnemonic: "cmpsq"},

	// Prefixes written as separate statements (REP; MOVSB).
	"REP": {mnemonic: "rep"}, "REPN": {mnemonic: "repne"}, "LOCK": {mnemonic: "lock"},

	// Memory hints.
	"PREFETCHT0": {mnemonic: "prefetcht0"}, "PREFETCHT1": {mnemonic: "prefetcht1"},
	"PREFETCHT2": {mnemonic: "prefetcht2"}, "PREFETCHNTA": {mnemonic: "prefetchnta"},
	"CLFLUSH": {mnemonic: "clflush"}, "CLFLUSHOPT": {mnemonic: "clflushopt"},
	"NOPL": {mnemonic: "nopl", width: 32}, "NOPW": {mnemonic: "nopw", width: 16},

	// SSE compares with the predicate last in Plan 9.
	"CMPPS": {mnemonic: "cmpps", order: amd64PredicateLast},
	"CMPPD": {mnemonic: "cmppd", order: amd64PredicateLast},
	"CMPSS": {mnemonic: "cmpss", order: amd64PredicateLast},
	"CMPSD": {mnemonic: "cmpsd", order: amd64PredicateLast},

	// Go spellings of SSE integer instructions.
	"PADDL":     {mnemonic: "paddd"},
	"PSUBL":     {mnemonic: "psubd"},
	"PSLLL":     {mnemonic: "pslld"},
	"PSRLL":     {mnemonic: "psrld"},
	"PSRAL":     {mnemonic: "psrad"},
	"PSLLO":     {mnemonic: "pslldq"},
	"PSRLO":     {mnemonic: "psrldq"},
	"PSLLDQ":    {mnemonic: "pslldq"},
	"PSRLDQ":    {mnemonic: "psrldq"},
	"PSHUFL":    {mnemonic: "pshufd"},
	"PCMPEQL":   {mnemonic: "pcmpeqd"},
	"PCMPGTL":   {mnemonic: "pcmpgtd"},
	"PUNPCKLLQ": {mnemonic: "punpckldq"},
	"PUNPCKHLQ": {mnemonic: "punpckhdq"},
	"PUNPCKLWL": {mnemonic: "punpcklwd"},
	"PUNPCKHWL": {mnemonic: "punpckhwd"},
	"PMULULQ":   {mnemonic: "pmuludq"},
	"PMADDWL":   {mnemonic: "pmaddwd"},
	"PACKSSLW":  {mnemonic: "packssdw"},
	"MOVOU":     {mnemonic: "movdqu"},
	"MOVO":      {mnemonic: "movdqa"},
	"MOVOA":     {mnemonic: "movdqa"},
	"MOVNTO":    {mnemonic: "movntdq"},
	"MOVNTDQ":   {mnemonic: "movntdq"},
	"MOVQOZX":   {mnemonic: "movq"},
	"MASKMOVOU": {mnemonic: "maskmovdqu"},
	"CVTPL2PD":  {mnemonic: "cvtdq2pd"},
	"CVTPL2PS":  {mnemonic: "cvtdq2ps"},
	"CVTPD2PL":  {mnemonic: "cvtpd2dq"},
	"CVTPS2PL":  {mnemonic: "cvtps2dq"},
	"CVTTPD2PL": {mnemonic: "cvttpd2dq"},
	"CVTTPS2PL": {mnemonic: "cvttps2dq"},

	// Conversions and moves between general-purpose and vector registers.
	"CVTSL2SD":  {mnemonic: "cvtsi2sdl", width: 32},
	"CVTSL2SS":  {mnemonic: "cvtsi2ssl", width: 32},
	"CVTSQ2SD":  {mnemonic: "cvtsi2sdq", width: 64},
	"CVTSQ2SS":  {mnemonic: "cvtsi2ssq", width: 64},
	"CVTSD2SL":  {mnemonic: "cvtsd2si", width: 32},
	"CVTSD2SQ":  {mnemonic: "cvtsd2si", width: 64},
	"CVTSS2SL":  {mnemonic: "cvtss2si", width: 32},
	"CVTSS2SQ":  {mnemonic: "cvtss2si", width: 64},
	"CVTTSD2SL": {mnemonic: "cvttsd2si", width: 32},
	"CVTTSD2SQ": {mnemonic: "cvttsd2si", width: 64},
	"CVTTSS2SL": {mnemonic: "cvttss2si", width: 32},
	"CVTTSS2SQ": {mnemonic: "cvttss2si", width: 64},
	"PEXTRB":    {mnemonic: "pextrb", width: 32},
	"PEXTRW":    {mnemonic: "pextrw", width: 32},
	"PEXTRD":    {mnemonic: "pextrd", width: 32},
	"PEXTRQ":    {mnemonic: "pextrq", width: 64},
	"PINSRB":    {mnemonic: "pinsrb", width: 32},
	"PINSRW":    {mnemonic: "pinsrw", width: 32},
	"PINSRD":    {mnemonic: "pinsrd", width: 32},
	"PINSRQ":    {mnemonic: "pinsrq", width: 64},
	"VPEXTRB":   {mnemonic: "vpextrb", width: 32},
	"VPEXTRW":   {mnemonic: "vpextrw", width: 32},
	"VPEXTRD":   {mnemonic: "vpextrd", width: 32},
	"VPEXTRQ":   {mnemonic: "vpextrq", width: 64},
	"VPINSRB":   {mnemonic: "vpinsrb", width: 32},
	"VPINSRW":   {mnemonic: "vpinsrw", width: 32},
	"VPINSRD":   {mnemonic: "vpinsrd", width: 32},
	"VPINSRQ":   {mnemonic: "vpinsrq", width: 64},
	"PMOVMSKB":  {mnemonic: "pmovmskb", width: 32},
	"VPMOVMSKB": {mnemonic: "vpmovmskb", width: 32},
	"MOVMSKPS":  {mnemonic: "movmskps", width: 32},
	"MOVMSKPD":  {mnemonic: "movmskpd", width: 32},
	"VMOVMSKPS": {mnemonic: "vmovmskps", width: 32},
	"VMOVMSKPD": {mnemonic: "vmovmskpd", width: 32},
	"VMOVD":     {mnemonic: "vmovd", width: 32},
	"VMOVQ":     {mnemonic: "vmovq", width: 64},
	"KMOVB":     {mnemonic: "kmovb", width: 32},
	"KMOVW":     {mnemonic: "kmovw", width: 32},
	"KMOVD":     {mnemonic: "kmovd", width: 32},
	"KMOVQ":     {mnemonic: "kmovq", width: 64},

	"VPBROADCASTB": {mnemonic: "vpbroadcastb", width: 32},
	"VPBROADCASTW": {mnemonic: "vpbroadcastw", width: 32},
	"VPBROADCASTD": {mnemonic: "vpbroadcastd", width: 32},
	"VPBROADCASTQ": {mnemonic: "vpbroadcastq", width: 64},
}

// amd64PlainPrefixes are the instruction-set families whose Go spelling is
// the Intel mnemonic, so lower-casing it gives the GNU mnemonic. They take no
// general-purpose register operands.
var amd64PlainPrefixes = []string{
	"V", "P", "K", "AES", "SHA", "ADDS", "ADDP", "SUBS", "SUBP", "MULS", "MULP",
	"DIVS", "DIVP", "MINS", "MINP", "MAXS", "MAXP", "SQRTS", "SQRTP", "ANDP",
	"ANDNP", "ORP", "XORP", "UCOMIS", "COMIS", "MOVUP", "MOVAP", "MOVSD",
	"MOVSS", "MOVHLPS", "MOVLHPS", "MOVHP", "MOVLP", "MOVDDUP", "MOVSHDUP",
	"MOVSLDUP", "MOVNTP", "MOVNTDQA", "LDDQU", "ROUNDS", "ROUNDP", "SHUFP",
	"UNPCK", "BLENDP", "BLENDVP", "DPP", "CVTSD2SS", "CVTSS2SD", "CVTPD2PS",
	"CVTPS2PD", "HADDP", "HSUBP", "ADDSUBP", "RCPP", "RCPS", "RSQRTP",
	"RSQRTS", "INSERTPS", "EXTRACTPS", "MPSADBW", "LDMXCSR", "STMXCSR",
}

// lookupAMD64 finds the translation for a Plan 9 amd64 mnemonic.
func lookupAMD64(opcode string) (amd64Spec, bool) {
	if spec, ok := amd64Fixed[opcode]; ok {
		return spec, true
	}
	if strings.HasPrefix(opcode, "SET") {
		if condition, ok := amd64Conditions[opcode[3:]]; ok {
			return amd64Spec{mnemonic: "set" + condition, width: 8}, true
		}
	}
	if strings.HasPrefix(opcode, "CMOV") && len(opcode) > 5 {
		width, ok := amd64SizeWidths[opcode[4]]
		condition, conditionOK := amd64Conditions[opcode[5:]]
		if ok && conditionOK && width != 8 {
			return amd64Spec{mnemonic: "cmov" + condition, width: width}, true
		}
	}
	if len(opcode) > 1 {
		base := opcode[:len(opcode)-1]
		width, ok := amd64SizeWidths[opcode[len(opcode)-1]]
		if ok && amd64SizedFamilies[base] {
			spec := amd64Spec{mnemonic: strings.ToLower(opcode), width: width}
			if base == "CMP" {
				spec.order = amd64SwapOrder
			}
			return spec, true
		}
	}
	if strings.HasPrefix(opcode, "VGATHERPF") || strings.HasPrefix(opcode, "VSCATTERPF") {
		// AVX-512PF prefetches put the mask first; nothing in Go's runtime or
		// library assembly uses them.
		return amd64Spec{}, false
	}
	for _, prefix := range amd64PlainPrefixes {
		if strings.HasPrefix(opcode, prefix) {
			return amd64Spec{mnemonic: strings.ToLower(opcode)}, true
		}
	}
	return amd64Spec{}, false
}
