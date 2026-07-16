package plan9asm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslateARM64OperandOrderAndAddressing(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·copy<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-24
	ADD R1, R2, R4
	SUB $16, R4, R4
	CMP $8, R4
	LDP.W 16(R1), (R6, R7)
	STP.P (R6, R7), -16(R0)
	TBZ $3, R2, done
done:
	RET
`))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "runtime", Filename: "copy_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, ".global runtime_copy")
	assert.Contains(t, assembly, "\tadd x4, x2, x1")
	assert.Contains(t, assembly, "\tsub x4, x4, #16")
	assert.Contains(t, assembly, "\tcmp x4, #8")
	assert.Contains(t, assembly, "\tldp x6, x7, [x1, #16]!")
	assert.Contains(t, assembly, "\tstp x6, x7, [x0], #-16")
	assert.Contains(t, assembly, "\ttbz x2, #3, .Lruntime_copy_arm64_0_done")
}

func TestTranslateARM64ConditionalAndIndexedInstructions(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·compare<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-0
	MOVD (R0)(R6), R4
	CSEL LT, R3, R1, R6
	REV R4, R5
	CSET NE, R0
	CNEG LO, R0, R0
	RET
`))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "internal/bytealg", Filename: "compare_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tldr x4, [x0, x6]")
	assert.Contains(t, assembly, "\tcsel x6, x3, x1, lt")
	assert.Contains(t, assembly, "\trev x5, x4")
	assert.Contains(t, assembly, "\tcset x0, ne")
	assert.Contains(t, assembly, "\tcneg x0, x0, lo")
}

func TestTranslateARM64MakesStaticTextVisibleToRuntimeMetadata(t *testing.T) {
	file, err := Parse(strings.NewReader("TEXT helper<>(SB),NOSPLIT,$0-0\n\tRET\n"))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "internal/bytealg", Filename: "compare_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, ".global .Linternal_bytealg_compare_arm64_helper")
	assert.Contains(t, assembly, ".hidden .Linternal_bytealg_compare_arm64_helper")
}

func TestTranslateARM64VectorLoadCompareAndMove(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT runtime·memequal<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-25
	VLD1.P (R0), [V0.D2, V1.D2, V2.D2, V3.D2]
	VCMEQ V0.D2, V4.D2, V8.D2
	VAND V8.B16, V9.B16, V8.B16
	VMOV V8.D[1], R5
	RET
`))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "internal/bytealg", Filename: "equal_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tld1 {v0.2d, v1.2d, v2.2d, v3.2d}, [x0], #64")
	assert.Contains(t, assembly, "\tcmeq v8.2d, v4.2d, v0.2d")
	assert.Contains(t, assembly, "\tand v8.16b, v9.16b, v8.16b")
	assert.Contains(t, assembly, "\tmov x5, v8.d[1]")
}

func TestTranslateARM64CountInstructions(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·count<ABIInternal>(SB),NOSPLIT,$0-0
	CMP R2.UXTB, R5
	VMOV R2, V0.B16
	VUADDLV V6.B16, V7
	VADD V7, V8
	NEG R4<<1, R4
	RBIT R6, R6
	CLZ R6, R6
	RET
`))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "internal/bytealg", Filename: "count_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tcmp x5, w2, uxtb")
	assert.Contains(t, assembly, "\tdup v0.16b, w2")
	assert.Contains(t, assembly, "\tuaddlv h7, v6.16b")
	assert.Contains(t, assembly, "\tadd d8, d8, d7")
	assert.Contains(t, assembly, "\tneg x4, x4, lsl #1")
	assert.Contains(t, assembly, "\trbit x6, x6")
	assert.Contains(t, assembly, "\tclz x6, x6")
}

func TestTranslateARM64MaterializesWideImmediate(t *testing.T) {
	file, err := Parse(strings.NewReader("TEXT ·constant(SB),$0-0\nMOVD $0x40100401, R5\nRET\n"))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "internal/bytealg", Filename: "indexbyte_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tmovz x5, #0x401, lsl #0")
	assert.Contains(t, assembly, "\tmovk x5, #0x4010, lsl #16")
}

func TestTranslateARM64BuildsABI0RegisterWrapper(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·readRegister(SB),NOSPLIT,$0-8
	MRS MIDR_EL1, R0
	MOVD R0, ret+0(FP)
	RET
`))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{PackagePath: "internal/cpu", Filename: "cpu_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, translation.Assembly, ".global internal_cpu_readRegister\n")
	assert.Contains(t, translation.Assembly, "\tsub sp, sp, #32")
	assert.Contains(t, translation.Assembly, "\tstr x30, [sp]")
	assert.Contains(t, translation.Assembly, "\tbl internal_cpu_readRegister_abi0")
	assert.Contains(t, translation.Assembly, "\tldr x0, [sp, #8]")
	assert.Contains(t, translation.Assembly, ".global internal_cpu_readRegister_abi0")
	assert.Contains(t, translation.Assembly, "\tstr x0, [sp, #8]")
	require.Len(t, translation.Functions, 2)
	assert.Equal(t, ARM64Function{Name: "internal_cpu_readRegister", Frame: 32, FrameStart: 4, Flags: []string{"NOSPLIT"}}, translation.Functions[0])
	assert.Equal(t, "internal_cpu_readRegister_abi0", translation.Functions[1].Name)
}

func TestTranslateARM64BuildsABI0MultiResultWrapper(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "internal", "runtime", "syscall", "linux", "asm_linux_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "internal/runtime/syscall/linux",
		Filename:    "asm_linux_arm64.s",
	})
	require.NoError(t, err)

	assert.Contains(t, translation.Assembly, "\tstr x0, [sp, #8]")
	assert.Contains(t, translation.Assembly, "\tstr x6, [sp, #56]")
	assert.Contains(t, translation.Assembly, "\tstr x7, [sp, #88]")
	assert.Contains(t, translation.Assembly, "\tstr x8, [sp, #96]")
	assert.Contains(t, translation.Assembly, "\tbl internal_runtime_syscall_linux_Syscall6_abi0")
	assert.Contains(t, translation.Assembly, "\tldr x0, [sp, #64]")
	assert.Contains(t, translation.Assembly, "\tldr x16, [sp, #72]")
	assert.Contains(t, translation.Assembly, "\tldr x16, [sp, #80]")
}

func TestTranslateARM64DirectABI0Leaf(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·Load(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	LDARW (R0), R0
	MOVW R0, ret+8(FP)
	RET
`))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "internal/runtime/atomic",
		Filename:         "atomic_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	assert.NotContains(t, translation.Assembly, "_abi0")
	assert.NotContains(t, translation.Assembly, "sub sp")
	assert.Contains(t, translation.Assembly, "\tldar w0, [x0]")
	require.Len(t, translation.Functions, 1)
	assert.Equal(t, "internal_runtime_atomic_Load", translation.Functions[0].Name)
}

func TestTranslateExactRuntimeSecretEraseRegisters(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "runtime", "secret_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "runtime",
		Filename:         "secret_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 2)
	assert.NotContains(t, translation.Assembly, "_abi0")
	assert.Contains(t, translation.Assembly, "\tb runtime_secretEraseRegistersMcall")
	assert.Contains(t, translation.Assembly, "\tfmov d0, xzr")
	assert.Contains(t, translation.Assembly, "\tfmov d31, xzr")
}

func TestTranslateExactChacha8FrameAndReadOnlyData(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "internal", "chacha8rand", "chacha8_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "internal/chacha8rand",
		Filename:    "chacha8_arm64.s",
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 1)
	assert.Equal(t, ARM64Function{
		Name:       "internal_chacha8rand_block",
		Frame:      32,
		FrameStart: 4,
		Flags:      []string{"NOSPLIT"},
	}, translation.Functions[0])
	assert.Contains(t, translation.Assembly, "\tsub sp, sp, #32")
	assert.Contains(t, translation.Assembly, "\tstr w2, [sp]")
	assert.Contains(t, translation.Assembly, "\tld1r {v12.4s}, [sp]")
	assert.Contains(t, translation.Assembly, ".section .rodata")
	assert.Contains(t, translation.Assembly, "internal_chacha8rand_chachaConst:")
	assert.Contains(t, translation.Assembly, "\t.word 0x61707865")
	assert.Contains(t, translation.Assembly, "\t.word 0xe0d0c0f")
}

func TestSupportsARM64FileKeepsTargetPolicyWithTranslator(t *testing.T) {
	assert.True(t, SupportsARM64File("runtime", "memmove_arm64.s"))
	assert.True(t, SupportsARM64File("runtime", "secret_arm64.s"))
	assert.True(t, SupportsARM64File("internal/bytealg", "compare_arm64.s"))
	assert.True(t, SupportsARM64File("internal/bytealg", "index_arm64.s"))
	assert.True(t, SupportsARM64File("internal/cpu", "cpu_arm64.s"))
	assert.True(t, SupportsARM64File("internal/chacha8rand", "chacha8_arm64.s"))
	assert.True(t, SupportsARM64File("internal/runtime/sys", "dit_arm64.s"))
	assert.True(t, SupportsARM64File("internal/runtime/syscall/linux", "asm_linux_arm64.s"))
	assert.True(t, SupportsARM64File("internal/runtime/atomic", "atomic_arm64.s"))
	assert.True(t, SupportsARM64File("syscall", "asm_linux_arm64.s"))
	assert.False(t, SupportsARM64File("runtime", "asm_arm64.s"))
	assert.False(t, SupportsARM64File("other", "memmove_arm64.s"))
}

func TestTranslateExactRuntimeARM64FilesAssemble(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("GNU AArch64 assembler is required")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc is unavailable")
	}

	var assembly bytes.Buffer
	files := []struct {
		packagePath string
		name        string
		defines     map[string]int64
	}{
		{packagePath: "runtime", name: "atomic_arm64.s"},
		{packagePath: "runtime", name: "memclr_arm64.s"},
		{packagePath: "runtime", name: "memmove_arm64.s"},
		{packagePath: "runtime", name: "secret_arm64.s"},
		{packagePath: "internal/bytealg", name: "compare_arm64.s"},
		{packagePath: "internal/bytealg", name: "count_arm64.s"},
		{packagePath: "internal/bytealg", name: "equal_arm64.s"},
		{packagePath: "internal/bytealg", name: "index_arm64.s"},
		{packagePath: "internal/bytealg", name: "indexbyte_arm64.s"},
		{packagePath: "internal/cpu", name: "cpu_arm64.s"},
		{packagePath: "internal/runtime/sys", name: "dit_arm64.s"},
		{packagePath: "internal/runtime/syscall/linux", name: "asm_linux_arm64.s"},
		{packagePath: "internal/chacha8rand", name: "chacha8_arm64.s"},
		{packagePath: "internal/runtime/atomic", name: "atomic_arm64.s", defines: map[string]int64{"const_offsetARM64HasATOMICS": 135}},
		{packagePath: "syscall", name: "asm_linux_arm64.s"},
	}
	for _, sourceFile := range files {
		path := filepath.Join("..", "stdlib", "src", filepath.FromSlash(sourceFile.packagePath), sourceFile.name)
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := Parse(bytes.NewReader(source))
		require.NoError(t, err, sourceFile.name)
		translated, err := TranslateARM64(file, ARM64Options{PackagePath: sourceFile.packagePath, Filename: sourceFile.name, Defines: sourceFile.defines})
		require.NoError(t, err, sourceFile.name)
		assembly.WriteString(translated)
	}

	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "runtime.S")
	objectPath := filepath.Join(directory, "runtime.o")
	require.NoError(t, os.WriteFile(sourcePath, assembly.Bytes(), 0o644))
	command := exec.Command(cc, "-c", "-o", objectPath, sourcePath)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestExecuteExactRuntimeSecretEraseRegisters(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 execution is required")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc is unavailable")
	}

	path := filepath.Join("..", "stdlib", "src", "runtime", "secret_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "runtime",
		Filename:         "secret_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	// secretEraseRegisters intentionally clears AAPCS callee-saved registers.
	// Preserve them around the call so this test adapter can safely return to C,
	// while checking the generated function before restoring their old values.
	harness := translation.Assembly + `
.text
.global test_secret
.type test_secret, %function
test_secret:
	stp x29, x30, [sp, #-160]!
	stp x19, x20, [sp, #16]
	stp x21, x22, [sp, #32]
	stp x23, x24, [sp, #48]
	stp x25, x26, [sp, #64]
	stp x27, x28, [sp, #80]
	stp d8, d9, [sp, #96]
	stp d10, d11, [sp, #112]
	stp d12, d13, [sp, #128]
	stp d14, d15, [sp, #144]
	mov x0, #1
	mov x1, #1
	mov x19, #1
	mov x25, #1
	mov x26, #1
	mov x27, #1
	fmov d0, x19
	fmov d31, x19
	bl runtime_secretEraseRegisters
	orr x9, x0, x1
	orr x9, x9, x19
	orr x9, x9, x25
	orr x9, x9, x26
	orr x9, x9, x27
	fmov x10, d0
	orr x9, x9, x10
	fmov x10, d31
	orr x9, x9, x10
	cmp x9, #0
	cset w0, ne
	ldp d14, d15, [sp, #144]
	ldp d12, d13, [sp, #128]
	ldp d10, d11, [sp, #112]
	ldp d8, d9, [sp, #96]
	ldp x27, x28, [sp, #80]
	ldp x25, x26, [sp, #64]
	ldp x23, x24, [sp, #48]
	ldp x21, x22, [sp, #32]
	ldp x19, x20, [sp, #16]
	ldp x29, x30, [sp], #160
	ret
.size test_secret, .-test_secret

.global main
.type main, %function
main:
	b test_secret
.size main, .-main
`
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "secret.S")
	executable := filepath.Join(directory, "secret")
	require.NoError(t, os.WriteFile(sourcePath, []byte(harness), 0o644))
	command := exec.Command(cc, "-no-pie", "-o", executable, sourcePath)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	output, err = exec.Command(executable).CombinedOutput()
	require.NoError(t, err, "%s", output)
}
