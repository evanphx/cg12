package a64

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tools locates an AArch64 assembler and objcopy, or skips.
func tools(t *testing.T) (as, objcopy string) {
	t.Helper()
	return testenv.Tool(t, "aarch64-linux-gnu-as", "aarch64-linux-gnu-gcc"),
		testenv.Tool(t, "aarch64-linux-gnu-objcopy")
}

// assemble assembles the instruction lines and returns the .text bytes.
func assemble(t *testing.T, as, objcopy string, lines []string) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "in.s")
	obj := filepath.Join(dir, "in.o")
	bin := filepath.Join(dir, "in.bin")
	require.NoError(t, os.WriteFile(src, []byte(".text\n"+strings.Join(lines, "\n")+"\n"), 0o644))

	var out []byte
	var err error
	if strings.HasSuffix(as, "gcc") {
		out, err = exec.Command(as, "-c", "-o", obj, src).CombinedOutput()
	} else {
		out, err = exec.Command(as, "-o", obj, src).CombinedOutput()
	}
	require.NoErrorf(t, err, "assemble failed: %s", out)

	out, err = exec.Command(objcopy, "-O", "binary", "--only-section=.text", obj, bin).CombinedOutput()
	require.NoErrorf(t, err, "objcopy failed: %s", out)
	b, err := os.ReadFile(bin)
	require.NoError(t, err)
	return b
}

// TestEncodingsMatchAssembler is the correctness proof: every encoding function
// produces exactly the bytes the reference assembler emits for the same
// instruction.
// andBits/orrBits/eorBits encode a logical-immediate instruction from a raw
// value via EncodeBitmask, for the reference-assembler comparison.
func andBits(w64 bool, rd, rn Reg, v uint64) uint32 {
	n, immr, imms, ok := EncodeBitmask(v, sizeOf(w64))
	if !ok {
		panic("not a bitmask immediate")
	}
	return AndImm(w64, rd, rn, n, immr, imms)
}
func orrBits(w64 bool, rd, rn Reg, v uint64) uint32 {
	n, immr, imms, _ := EncodeBitmask(v, sizeOf(w64))
	return OrrImm(w64, rd, rn, n, immr, imms)
}
func eorBits(w64 bool, rd, rn Reg, v uint64) uint32 {
	n, immr, imms, _ := EncodeBitmask(v, sizeOf(w64))
	return EorImm(w64, rd, rn, n, immr, imms)
}
func sizeOf(w64 bool) int {
	if w64 {
		return 8
	}
	return 4
}

func TestEncodingsMatchAssembler(t *testing.T) {
	as, objcopy := tools(t)

	cases := encodingCases

	lines := make([]string, len(cases))
	for i, c := range cases {
		lines[i] = c.asm
	}
	text := assemble(t, as, objcopy, lines)
	require.Len(t, text, 4*len(cases), "one word per instruction")

	for i, c := range cases {
		got := binary.LittleEndian.Uint32(text[4*i:])
		assert.Equalf(t, got, c.word, "%s: assembler=%#08x ours=%#08x", c.asm, got, c.word)
	}
}

// TestBranchOffsets checks the PC-relative offset encoding, which the
// assembler-comparison covers only at offset 0.
func TestBranchOffsets(t *testing.T) {
	assert.Equal(t, uint32(0x14000000), B(0))
	assert.Equal(t, uint32(0x14000002), B(8))  // forward two instructions
	assert.Equal(t, uint32(0x17ffffff), B(-4)) // back one instruction
	assert.Equal(t, uint32(0x94000001), Bl(4)) // BL forward one
	assert.Equal(t, uint32(0x54000020), Bcond(EQ, 4))
	assert.Equal(t, uint32(0xb5000040), Cbnz(true, 0, 8))
}

// TestProgramAssemblesLoop assembles a small function with a forward and a
// backward branch through the Program (label-resolving) path and checks it
// matches the reference assembler's bytes for the same source.
func TestProgramAssemblesLoop(t *testing.T) {
	as, objcopy := tools(t)

	// sum(n) in w0: acc=0; while n!=0 { acc+=n; n-- }; return acc.
	p := NewProgram()
	p.Emit(Movz(false, 1, 0, 0)) // movz w1, #0
	p.Label("loop")
	p.Cbz(false, 0, "done")        // cbz w0, done  (forward)
	p.Emit(AddReg(false, 1, 1, 0)) // add w1, w1, w0
	p.Emit(SubImm(false, 0, 0, 1)) // sub w0, w0, #1
	p.B("loop")                    // b loop        (backward)
	p.Label("done")
	p.Emit(MovReg(false, 0, 1)) // mov w0, w1
	p.Emit(Ret(30))             // ret
	got, err := p.Bytes()
	require.NoError(t, err)

	want := assemble(t, as, objcopy, []string{
		"movz w1, #0",
		"loop:",
		"cbz w0, done",
		"add w1, w1, w0",
		"sub w0, w0, #1",
		"b loop",
		"done:",
		"mov w0, w1",
		"ret",
	})
	assert.Equal(t, want, got)
}

func TestProgramErrors(t *testing.T) {
	p := NewProgram()
	p.B("missing")
	_, err := p.Bytes()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined label")

	dup := NewProgram()
	dup.Label("x")
	dup.Label("x")
	_, err = dup.Bytes()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate label")
}

// TestMoreEncodingsMatchAssembler validates the float, pair, extend, address,
// and sign-load encodings against the reference assembler.
func TestMoreEncodingsMatchAssembler(t *testing.T) {
	as, objcopy := tools(t)

	cases := moreEncodingCases

	lines := make([]string, len(cases))
	for i, c := range cases {
		lines[i] = c.asm
	}
	text := assemble(t, as, objcopy, lines)
	require.Len(t, text, 4*len(cases))
	for i, c := range cases {
		got := binary.LittleEndian.Uint32(text[4*i:])
		assert.Equalf(t, got, c.word, "%s: assembler=%#08x ours=%#08x", c.asm, got, c.word)
	}
}

// TestImmediateGuards checks that out-of-range immediates panic rather than
// silently truncating into a wrong-but-valid-looking instruction.
func TestImmediateGuards(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"add imm > 4095", func() { AddImm(true, 0, 1, 4096) }},
		{"str offset > range", func() { StrImm(true, 0, 1, 8*4096) }},
		{"str misaligned", func() { StrImm(true, 0, 1, 4) }},
		{"ldr misaligned", func() { LdrImm(true, 0, 1, 12) }},
		{"branch too far", func() { B(1 << 28) }},
		{"branch misaligned", func() { B(2) }},
		{"cbz too far", func() { Cbz(true, 0, 1<<21) }},
		{"bad register", func() { AddReg(true, 40, 1, 2) }},
		{"movz bad shift", func() { Movz(true, 0, 1, 8) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: expected a panic, got none", c.name)
				}
			}()
			c.fn()
		})
	}
}

// TestProgramDataWord checks that a DataWord entry resolves to the signed byte
// distance from its base label to its target label -- the jump-table offset.
func TestProgramDataWord(t *testing.T) {
	p := NewProgram()
	p.Label("back")             // word 0
	p.Emit(Ret(30))             // word 0
	p.Emit(Ret(30))             // word 1
	p.Label("table")            // word 2
	p.DataWord("back", "table") // word 2: back(0) - table(2) = -8
	p.DataWord("fwd", "table")  // word 3: fwd(5) - table(2) = +12
	p.Emit(Ret(30))             // word 4
	p.Label("fwd")              // word 5
	p.Emit(Ret(30))             // word 5

	got, err := p.Bytes()
	require.NoError(t, err)
	assert.Equal(t, int32(-8), int32(binary.LittleEndian.Uint32(got[8:12])))
	assert.Equal(t, int32(12), int32(binary.LittleEndian.Uint32(got[12:16])))
}

// encodingCase pairs a line of assembly with the word our encoder produces for
// it. The assembler validates the encoder against these; the disassembler is
// checked against the same pairs read the other way, so one set of facts holds
// both directions honest.
type encodingCase struct {
	asm  string
	word uint32
}

var encodingCases = []encodingCase{
	{"add w1, w2, w3", AddReg(false, 1, 2, 3)},
	{"add x1, x2, x3", AddReg(true, 1, 2, 3)},
	{"sub x5, x6, x7", SubReg(true, 5, 6, 7)},
	{"subs x1, x2, x3", SubsReg(true, 1, 2, 3)},
	{"cmp x9, x10", CmpReg(true, 9, 10)},
	{"add x1, x2, #5", AddImm(true, 1, 2, 5)},
	{"add x11, x12, #4095", AddImm(true, 11, 12, 4095)},
	{"sub w3, w4, #10", SubImm(false, 3, 4, 10)},
	{"and x1, x2, x3", AndReg(true, 1, 2, 3)},
	{"orr w4, w5, w6", OrrReg(false, 4, 5, 6)},
	{"eor x7, x8, x9", EorReg(true, 7, 8, 9)},
	{"mov x1, x2", MovReg(true, 1, 2)},
	{"movz x1, #0x1234", Movz(true, 1, 0x1234, 0)},
	{"movz x1, #0x1234, lsl #16", Movz(true, 1, 0x1234, 16)},
	{"movk w2, #0xabcd", Movk(false, 2, 0xabcd, 0)},
	{"movn x3, #0xff, lsl #32", Movn(true, 3, 0xff, 32)},
	{"udiv x1, x2, x3", Udiv(true, 1, 2, 3)},
	{"sdiv w4, w5, w6", Sdiv(false, 4, 5, 6)},
	{"lsl x1, x2, x3", Lslv(true, 1, 2, 3)},
	{"lsr w4, w5, w6", Lsrv(false, 4, 5, 6)},
	{"asr x7, x8, x9", Asrv(true, 7, 8, 9)},
	{"madd x1, x2, x3, x4", Madd(true, 1, 2, 3, 4)},
	{"msub w5, w6, w7, w8", Msub(false, 5, 6, 7, 8)},
	{"mul x1, x2, x3", Mul(true, 1, 2, 3)},
	{"csel x1, x2, x3, eq", Csel(true, 1, 2, 3, EQ)},
	{"cset w1, ne", Cset(false, 1, NE)},
	{"cset x2, ge", Cset(true, 2, GE)},
	{"b .", B(0)},
	{"bl .", Bl(0)},
	{"b.eq .", Bcond(EQ, 0)},
	{"cbz x1, .", Cbz(true, 1, 0)},
	{"cbnz w2, .", Cbnz(false, 2, 0)},
	{"ret", Ret(30)},
	{"br x1", Br(1)},
	{"blr x2", Blr(2)},
	{"brk #0", Brk(0)},
	{"brk #7", Brk(7)},
	{"str x0, [x1, #8]", StrImm(true, 0, 1, 8)},
	{"str w2, [x3, #12]", StrImm(false, 2, 3, 12)},
	{"ldr x4, [x5, #16]", LdrImm(true, 4, 5, 16)},
	{"ldr w6, [x7, #4]", LdrImm(false, 6, 7, 4)},
	{"strb w0, [x1]", StrbImm(0, 1, 0)},
	{"ldrb w2, [x3, #1]", LdrbImm(2, 3, 1)},
	{"strh w4, [x5, #2]", StrhImm(4, 5, 2)},
	{"ldrh w6, [x7, #4]", LdrhImm(6, 7, 4)},
	{"ldrsw x1, [x2, #8]", LdrswImm(1, 2, 8)},
	{"cmp w1, #16", CmpImm(false, 1, 16)},
	{"cmp x2, #4095", CmpImm(true, 2, 4095)},
	{"lsl w1, w2, #7", LslImm(false, 1, 2, 7)},
	{"lsl x3, x4, #40", LslImm(true, 3, 4, 40)},
	{"lsr w5, w6, #25", LsrImm(false, 5, 6, 25)},
	{"lsr x7, x8, #1", LsrImm(true, 7, 8, 1)},
	{"asr w1, w2, #3", AsrImm(false, 1, 2, 3)},
	{"asr x3, x4, #10", AsrImm(true, 3, 4, 10)},
	{"ror w9, w10, #7", RorImm(false, 9, 10, 7)},
	{"ror x11, x12, #40", RorImm(true, 11, 12, 40)},
	{"extr w1, w2, w3, #5", Extr(false, 1, 2, 3, 5)},
	{"and w1, w2, #0xff", andBits(false, 1, 2, 0xff)},
	{"and x3, x4, #0xffff", andBits(true, 3, 4, 0xffff)},
	{"orr w5, w6, #0xf0f0f0f0", orrBits(false, 5, 6, 0xf0f0f0f0)},
	{"eor x7, x8, #0x1", eorBits(true, 7, 8, 0x1)},
	{"and w9, w10, #0xfffffff8", andBits(false, 9, 10, 0xfffffff8)},
	{"bic w1, w2, w3", BicReg(false, 1, 2, 3)},
	{"bic x4, x5, x6", BicReg(true, 4, 5, 6)},
	{"orn w7, w8, w9", OrnReg(false, 7, 8, 9)},
	{"mvn w10, w11", MvnReg(false, 10, 11)},
	{"mvn x12, x13", MvnReg(true, 12, 13)},
	{"ldr w1, [x2, w3, sxtw #2]", LdrReg(false, 1, 2, 3, ExtSXTW, 1)},
	{"ldr w4, [x5, x6, lsl #2]", LdrReg(false, 4, 5, 6, ExtLSL, 1)},
	{"ldr x7, [x8, x9, lsl #3]", LdrReg(true, 7, 8, 9, ExtLSL, 1)},
	{"str w1, [x2, w3, sxtw #2]", StrReg(false, 1, 2, 3, ExtSXTW, 1)},
	{"str w4, [x5, x6, lsl #2]", StrReg(false, 4, 5, 6, ExtLSL, 1)},
	{"ldrb w1, [x2, w3, sxtw]", LdrbReg(1, 2, 3, ExtSXTW, 0)},
	{"ldrb w4, [x5, x6]", LdrbReg(4, 5, 6, ExtLSL, 0)},
	{"strb w1, [x2, w3, sxtw]", StrbReg(1, 2, 3, ExtSXTW, 0)},
	{"ldrsw x1, [x2, w3, sxtw #2]", LdrswReg(1, 2, 3, ExtSXTW, 1)},
	{"ldrh w4, [x5, w6, uxtw #1]", LdrhReg(4, 5, 6, ExtUXTW, 1)},
	{"clz w1, w2", Clz(false, 1, 2)},
	{"clz x3, x4", Clz(true, 3, 4)},

	// System and synchronization.
	{"svc #0", Svc(0)},
	{"svc #128", Svc(128)},
	{"dmb sy", Dmb(BarrierSY)},
	{"dmb ish", Dmb(BarrierISH)},
	{"dmb ishst", Dmb(BarrierISHST)},
	{"dmb ld", Dmb(BarrierLD)},
	{"dmb st", Dmb(BarrierST)},
	{"dsb sy", Dsb(BarrierSY)},
	{"dsb ish", Dsb(BarrierISH)},
	{"isb", Isb()},

	// Exclusive access: the atomic primitives.
	{"ldxr w0, [x1]", Ldxr(4, 0, 1)},
	{"ldxr x2, [x3]", Ldxr(8, 2, 3)},
	{"ldaxr w4, [x5]", Ldaxr(4, 4, 5)},
	{"ldaxr x6, [x7]", Ldaxr(8, 6, 7)},
	{"stxr w0, w1, [x2]", Stxr(4, 0, 1, 2)},
	{"stxr w3, x4, [x5]", Stxr(8, 3, 4, 5)},
	{"stlxr w6, w7, [x8]", Stlxr(4, 6, 7, 8)},
	{"stlxr w9, x10, [x11]", Stlxr(8, 9, 10, 11)},
	{"ldar w0, [x1]", Ldar(4, 0, 1)},
	{"ldar x2, [x3]", Ldar(8, 2, 3)},
	{"stlr w4, [x5]", Stlr(4, 4, 5)},
	{"stlr x6, [x7]", Stlr(8, 6, 7)},
	// Byte and halfword exclusives/ordered, for sub-word atomics.
	{"ldaxrb w0, [x1]", Ldaxr(1, 0, 1)},
	{"ldaxrh w2, [x3]", Ldaxr(2, 2, 3)},
	{"stlxrb w4, w5, [x6]", Stlxr(1, 4, 5, 6)},
	{"stlxrh w7, w8, [x9]", Stlxr(2, 7, 8, 9)},
	{"ldarb w0, [x1]", Ldar(1, 0, 1)},
	{"ldarh w2, [x3]", Ldar(2, 2, 3)},
	{"stlrb w4, [x5]", Stlr(1, 4, 5)},
	{"stlrh w6, [x7]", Stlr(2, 6, 7)},

	// System-register transfer (mrs already had tpidr_el0; the rest are new).
	{"mrs x0, fpcr", Mrs(0, SysRegs["fpcr"])},
	{"mrs x1, fpsr", Mrs(1, SysRegs["fpsr"])},
	{"mrs x2, nzcv", Mrs(2, SysRegs["nzcv"])},
	{"mrs x3, cntvct_el0", Mrs(3, SysRegs["cntvct_el0"])},
	{"mrs x4, midr_el1", Mrs(4, SysRegs["midr_el1"])},
	{"msr fpcr, x5", Msr(SysRegs["fpcr"], 5)},
	{"msr nzcv, x6", Msr(SysRegs["nzcv"], 6)},
	{"msr tpidr_el0, x7", Msr(SysRegs["tpidr_el0"], 7)},
}

var moreEncodingCases = []encodingCase{
	{"fadd s0, s1, s2", Fadd(false, 0, 1, 2)},
	{"fsub d3, d4, d5", Fsub(true, 3, 4, 5)},
	{"fmul s6, s7, s8", Fmul(false, 6, 7, 8)},
	{"fdiv d0, d1, d2", Fdiv(true, 0, 1, 2)},
	{"fneg s0, s1", Fneg(false, 0, 1)},
	{"fneg d2, d3", Fneg(true, 2, 3)},
	{"fmov s0, s1", FmovReg(false, 0, 1)},
	{"fmov d4, d5", FmovReg(true, 4, 5)},
	{"fcvt d0, s1", FcvtStoD(0, 1)},
	{"fcvt s2, d3", FcvtDtoS(2, 3)},
	{"fcmp s0, s1", Fcmp(false, 0, 1)},
	{"fcmp d2, d3", Fcmp(true, 2, 3)},
	{"fcvtzs w0, s1", Fcvtzs(false, false, 0, 1)},
	{"fcvtzs x2, d3", Fcvtzs(true, true, 2, 3)},
	{"fcvtzu w4, s5", Fcvtzu(false, false, 4, 5)},
	{"scvtf s0, w1", Scvtf(false, false, 0, 1)},
	{"scvtf d2, x3", Scvtf(true, true, 2, 3)},
	{"ucvtf s4, w5", Ucvtf(false, false, 4, 5)},
	{"fmov s0, w1", FmovFromGP(false, 0, 1)},
	{"fmov d2, x3", FmovFromGP(true, 2, 3)},
	{"fmov w0, s1", FmovToGP(false, 0, 1)},
	{"fmov x2, d3", FmovToGP(true, 2, 3)},
	{"str s0, [x1, #4]", StrFP(false, 0, 1, 4)},
	{"ldr d2, [x3, #8]", LdrFP(true, 2, 3, 8)},
	{"str q0, [x1]", StrQ(0, 1, 0)},
	{"ldr q2, [x3, #16]", LdrQ(2, 3, 16)},
	{"mov v0.16b, v1.16b", MovVec16b(0, 1)},
	{"stp x29, x30, [sp, #-16]!", Stp(true, 29, 30, SP, -16, PreIndex)},
	{"ldp x29, x30, [sp], #16", Ldp(true, 29, 30, SP, 16, PostIndex)},
	{"stp x0, x1, [x2, #16]", Stp(true, 0, 1, 2, 16, SignedOffset)},
	{"stp w3, w4, [x5, #8]", Stp(false, 3, 4, 5, 8, SignedOffset)},
	{"ldrsb w0, [x1]", LdrsbImm(false, 0, 1, 0)},
	{"ldrsb x2, [x3, #1]", LdrsbImm(true, 2, 3, 1)},
	{"ldrsh w4, [x5, #2]", LdrshImm(false, 4, 5, 2)},
	{"adrp x0, .", Adrp(0, 0)},
	{"adr x0, .", Adr(0, 0)},
	{"add sp, sp, #1, lsl #12", AddImmLSL12(true, SP, SP, 1)},
	{"sub x0, x1, #2, lsl #12", SubImmLSL12(true, 0, 1, 2)},
	{"sxtb w0, w1", Sxtb(false, 0, 1)},
	{"sxtb x2, w3", Sxtb(true, 2, 3)},
	{"sxth w4, w5", Sxth(false, 4, 5)},
	{"sxtw x6, w7", Sxtw(6, 7)},
	{"uxtb w0, w1", Uxtb(0, 1)},
	{"uxth w2, w3", Uxth(2, 3)},
	{"neg x0, x1", NegReg(true, 0, 1)},
	{"add x0, x1, w2, sxtw", AddExtSxtw(0, 1, 2)},
	{"add x5, x9, w10, sxtw", AddExtSxtw(5, 9, 10)},
	{"mrs x0, tpidr_el0", MrsTPIDR(0)},
	{"mrs x5, tpidr_el0", MrsTPIDR(5)},
	// Pre/post-indexed single loads and stores (write-back).
	{"ldr x0, [x1], #8", LdrWB(true, 0, 1, 8, Post)},
	{"ldr x0, [x1, #8]!", LdrWB(true, 0, 1, 8, Pre)},
	{"ldr w2, [x3], #-4", LdrWB(false, 2, 3, -4, Post)},
	{"str x4, [x5], #8", StrWB(true, 4, 5, 8, Post)},
	{"str x6, [x7, #-8]!", StrWB(true, 6, 7, -8, Pre)},
	{"ldrb w4, [x1], #1", LdrbWB(4, 1, 1, Post)},
	{"strb w8, [x9], #1", StrbWB(8, 9, 1, Post)},
	{"ldrsb x9, [x10], #1", LdrsbWB(true, 9, 10, 1, Post)},
	{"ldrsb w9, [x10], #1", LdrsbWB(false, 9, 10, 1, Post)},
	{"ldrh w0, [x1], #2", LdrhWB(0, 1, 2, Post)},
	{"strh w2, [x3, #-2]!", StrhWB(2, 3, -2, Pre)},
	{"ldrsh x4, [x5], #2", LdrshWB(true, 4, 5, 2, Post)},
	{"ldrsw x6, [x7], #4", LdrswWB(6, 7, 4, Post)},
}
