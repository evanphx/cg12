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

func runtimeARM64ParseOptions(t *testing.T) ParseOptions {
	t.Helper()
	runtimeDirectory := filepath.Join("..", "stdlib", "src", "runtime")
	includes := make(map[string]string)
	for _, name := range []string{"textflag.h", "go_tls.h", "funcdata.h", "tls_arm64.h", "cgo/abi_arm64.h"} {
		source, err := os.ReadFile(filepath.Join(runtimeDirectory, filepath.FromSlash(name)))
		require.NoError(t, err)
		includes[name] = string(source)
	}
	return ParseOptions{
		Defines:  map[string]string{"GOARCH_arm64": "1", "GOOS_linux": "1"},
		Includes: includes,
	}
}

func runtimeAssemblyDefines() map[string]int64 {
	return map[string]int64{
		"g_m":             48,
		"g_sched":         56,
		"g_secret":        0,
		"g_stack":         0,
		"g_syscallsp":     112,
		"gobuf_sp":        16,
		"m_cgoCallers":    0,
		"m_cgoCallersUse": 0,
		"m_curg":          0,
		"m_g0":            0,
		"m_gsignal":       0,
		"m_ncgo":          0,
		"m_procid":        0,
		"m_vdsoPC":        0,
		"m_vdsoSP":        0,
		"stack_lo":        0,
	}
}

func TestTranslateARM64OperandOrderAndAddressing(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·copy<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-24
	ADD R1, R2, R4
	SUB $16, R4, R4
	MOVD $(-64*1024)(R7), R0
	MOVD $-48(R4), R1
	MOVD $runtime·table+16(SB), R3
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
	assert.Contains(t, assembly, "\tsub x0, x7, #16, lsl #12")
	assert.Contains(t, assembly, "\tsub x1, x4, #48")
	assert.Contains(t, assembly, "\tadd x3, x3, :lo12:runtime_table+16")
	assert.Contains(t, assembly, "\tcmp x4, #8")
	assert.Contains(t, assembly, "\tldp x6, x7, [x1, #16]!")
	assert.Contains(t, assembly, "\tstp x6, x7, [x0], #-16")
	assert.Contains(t, assembly, "\ttbz x2, #3, .Lruntime_copy_arm64_0_done")
}

func TestTranslateARM64LargeWordImmediateUsesWordScratch(t *testing.T) {
	file, err := Parse(strings.NewReader("TEXT ·add<ABIInternal>(SB),NOSPLIT,$0\n\tADDW $0xd76aa478, R4\n\tRET\n"))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "crypto/md5", Filename: "md5block_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tmovz w27, #0xa478")
	assert.Contains(t, assembly, "\tadd w4, w4, w27")
	assert.NotContains(t, assembly, "add w4, w4, x27")
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

func TestTranslateARM64NamedVirtualStackSlots(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT runtime·callback(SB),NOSPLIT,$24-0
	MOVD R8, savedm-8(SP)
	MOVD savedsp-16(SP), R4
	RET
`))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{
		PackagePath: "runtime",
		Filename:    "asm_arm64.s",
	})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tstr x8, [sp, #24]")
	assert.Contains(t, assembly, "\tldr x4, [sp, #16]")
}

func TestTranslateARM64LeafFrameKeepsLiveLinkRegister(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·block<ABIInternal>(SB),NOSPLIT,$16
	MOVW R2, 0(RSP)
	RET
`))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "example"})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tstr w2, [sp]")
	assert.NotContains(t, assembly, "\tldr x30, [sp]")
}

func TestTranslateARM64VectorLoadCompareAndMove(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT runtime·memequal<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-25
	VLD1.P (R0), [V0.D2, V1.D2, V2.D2, V3.D2]
	VLD1.P 4(R0), V2.S[2]
	VLD1.P (R0)(R10), [V4.B16, V5.B16]
	VCMEQ V0.D2, V4.D2, V8.D2
	VAND V8.B16, V9.B16, V8.B16
	VMOV V8.D[1], R5
	VMOV R1, V0.S[0]
	AESE V2.B16, V0.B16
	AESMC V0.B16, V0.B16
	PRFM (R0), PLDL1KEEP
	RET
`))
	require.NoError(t, err)
	assembly, err := TranslateARM64(file, ARM64Options{PackagePath: "internal/bytealg", Filename: "equal_arm64.s"})
	require.NoError(t, err)

	assert.Contains(t, assembly, "\tld1 {v0.2d, v1.2d, v2.2d, v3.2d}, [x0], #64")
	assert.Contains(t, assembly, "\tld1 {v2.s}[2], [x0], #4")
	assert.Contains(t, assembly, "\tld1 {v4.16b, v5.16b}, [x0], x10")
	assert.Contains(t, assembly, "\tcmeq v8.2d, v4.2d, v0.2d")
	assert.Contains(t, assembly, "\tand v8.16b, v9.16b, v8.16b")
	assert.Contains(t, assembly, "\tmov x5, v8.d[1]")
	assert.Contains(t, assembly, "\tmov v0.s[0], w1")
	assert.Contains(t, assembly, "\taese v0.16b, v2.16b")
	assert.Contains(t, assembly, "\taesmc v0.16b, v0.16b")
	assert.Equal(t, 1, strings.Count(assembly, ".arch_extension crypto"))
	assert.Contains(t, assembly, "\tprfm pldl1keep, [x0]")
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
	assert.Contains(t, translation.Assembly, "\tstr x29, [sp, #16]")
	assert.Contains(t, translation.Assembly, "\tbl internal_cpu_readRegister_abi0")
	assert.Contains(t, translation.Assembly, "\tldr x0, [sp, #8]")
	assert.Contains(t, translation.Assembly, "\tldr x29, [sp, #16]")
	assert.Contains(t, translation.Assembly, ".global internal_cpu_readRegister_abi0")
	assert.Contains(t, translation.Assembly, "\tstr x0, [sp, #8]")
	require.Len(t, translation.Functions, 2)
	assert.Equal(t, ARM64Function{Name: "internal_cpu_readRegister", Frame: 32, FrameStart: 4, Args: 8, Flags: []string{"NOSPLIT"}}, translation.Functions[0])
	assert.Equal(t, ARM64Function{Name: "internal_cpu_readRegister_abi0", Args: 8, Flags: []string{"NOSPLIT"}}, translation.Functions[1])
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
	assert.Contains(t, translation.Assembly, "\tsub sp, sp, #128")
	assert.Contains(t, translation.Assembly, "\tstr x29, [sp, #104]")
	assert.Contains(t, translation.Assembly, "\tbl internal_runtime_syscall_linux_Syscall6_abi0")
	assert.Contains(t, translation.Assembly, "\tldr x0, [sp, #64]")
	assert.Contains(t, translation.Assembly, "\tldr x16, [sp, #72]")
	assert.Contains(t, translation.Assembly, "\tldr x16, [sp, #80]")
	assert.Contains(t, translation.Assembly, "\tldr x29, [sp, #104]")
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

	assert.Contains(t, translation.Assembly, ".hidden internal_runtime_atomic_Load_abi0")
	assert.NotContains(t, translation.Assembly, "sub sp")
	assert.Contains(t, translation.Assembly, "\tldar w0, [x0]")
	require.Len(t, translation.Functions, 1)
	assert.Equal(t, "internal_runtime_atomic_Load", translation.Functions[0].Name)
}

func TestTranslateARM64ABI0TailBranchUsesDirectTarget(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·caller(SB),NOSPLIT,$0-8
	MOVD value+0(FP), R0
	B ·abort(SB)
TEXT ·abort(SB),NOSPLIT,$0-0
	RET
`))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "runtime",
		Filename:         "asm_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	assert.Contains(t, translation.Assembly, "\tb runtime_abort\n")
	assert.Contains(t, translation.Assembly, ".hidden runtime_abort_abi0")
}

func TestTranslateARM64AssemblyFunctionAddressUsesABI0Body(t *testing.T) {
	file, err := Parse(strings.NewReader(`
TEXT ·target(SB),NOSPLIT,$0-0
	BL ·helper(SB)
	RET
TEXT ·helper(SB),NOSPLIT,$0-0
	RET
TEXT ·address<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-0
	MOVD $·target+8(SB), R0
	RET
`))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "runtime",
		Filename:         "asm_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	assert.Contains(t, translation.Assembly, "\tadrp x0, runtime_target_abi0")
	assert.Contains(t, translation.Assembly, "\tadd x0, x0, :lo12:runtime_target_abi0+8")
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
	assert.Contains(t, translation.Assembly, ".hidden runtime_secretEraseRegisters_abi0")
	assert.Contains(t, translation.Assembly, ".hidden runtime_secretEraseRegistersMcall_abi0")
	assert.Contains(t, translation.Assembly, "\tb runtime_secretEraseRegistersMcall")
	assert.Contains(t, translation.Assembly, "\tfmov d0, xzr")
	assert.Contains(t, translation.Assembly, "\tfmov d31, xzr")
}

func TestTranslateExactRuntimeAsyncPreempt(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "runtime", "preempt_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "runtime",
		Filename:    "preempt_arm64.s",
		Defines: map[string]int64{
			"g_m":              48,
			"m_p":              208,
			"p_xRegs":          13800,
			"xRegPerP_scratch": 0,
			"xRegPerP_cache":   512,
		},
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 1)
	assert.Equal(t, ARM64Function{
		Name:       "runtime_asyncPreempt",
		Frame:      240,
		FrameStart: 8,
		Flags:      []string{"NOSPLIT", "NOFRAME"},
	}, translation.Functions[0])
	assert.Contains(t, translation.Assembly, ".hidden runtime_asyncPreempt_abi0")
	assert.Contains(t, translation.Assembly, "\tmrs x0, nzcv")
	assert.Contains(t, translation.Assembly, "\tmsr fpsr, x0")
	assert.Contains(t, translation.Assembly, "\tldr x0, [x28, #48]")
	assert.Contains(t, translation.Assembly, "\tmov x27, #13800")
	assert.Contains(t, translation.Assembly, "\tadd x0, x0, x27")
	assert.Contains(t, translation.Assembly, "\tbl runtime_asyncPreempt2_abi0")
	assert.Contains(t, translation.Assembly, "\tret x27")
}

func TestTranslateExactRuntimeTLS(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "runtime", "tls_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := ParseWithOptions(bytes.NewReader(source), runtimeARM64ParseOptions(t))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "runtime",
		Filename:         "tls_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 2)
	assert.Equal(t, "runtime_load_g", translation.Functions[0].Name)
	assert.Equal(t, "runtime_save_g", translation.Functions[1].Name)
	assert.Contains(t, translation.Assembly, ".hidden runtime_load_g_abi0")
	assert.Contains(t, translation.Assembly, ".hidden runtime_save_g_abi0")
	assert.Contains(t, translation.Assembly, "\t.inst 0xd53bd040")
	assert.Contains(t, translation.Assembly, "\tldr x28, [x0, x27]")
	assert.Contains(t, translation.Assembly, "\tstr x28, [x0, x27]")
	assert.Contains(t, translation.Assembly, ".global runtime_tls_g\n")
	assert.NotContains(t, translation.Assembly, "runtime_tls_g_0")
}

func TestTranslateExactRuntimeLinuxSystemAssembly(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "runtime", "sys_linux_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := ParseWithOptions(bytes.NewReader(source), runtimeARM64ParseOptions(t))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "runtime",
		Filename:         "sys_linux_arm64.s",
		Defines:          runtimeAssemblyDefines(),
		PreferDirectABI0: true,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, translation.Assembly)
}

func TestTranslateDataRelocationAndString(t *testing.T) {
	file, err := Parse(strings.NewReader(`
DATA ·entry+0(SB)/8, $·target<ABIInternal>(SB)
GLOBL ·entry(SB), RODATA, $8
DATA message<>+0(SB)/6, $"hello"
GLOBL message<>(SB), RODATA, $6
`))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "runtime",
		Filename:    "data_arm64.s",
	})
	require.NoError(t, err)

	assert.Contains(t, translation.Assembly, "\t.quad runtime_target\n")
	assert.Contains(t, translation.Assembly, "\t.byte 0x68, 0x65, 0x6c, 0x6c, 0x6f\n")
	assert.Contains(t, translation.Assembly, "\t.zero 1\n")
	assert.Equal(t, []string{"runtime_target"}, translation.ExternalReferences)
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

func TestTranslateExactReflectAssembly(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "reflect", "asm_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := ParseWithOptions(bytes.NewReader(source), runtimeARM64ParseOptions(t))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "reflect",
		Filename:         "asm_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 4)
	assert.Equal(t, ARM64Function{
		Name:       "reflect_makeFuncStub",
		Frame:      32,
		FrameStart: 4,
		Flags:      []string{"NOSPLIT", "WRAPPER"},
	}, translation.Functions[0])
	assert.Equal(t, ARM64Function{
		Name:       "reflect_makeFuncStub_abi0",
		Frame:      448,
		FrameStart: 4,
		Flags:      []string{"NOSPLIT", "WRAPPER"},
	}, translation.Functions[1])
	assert.Equal(t, "reflect_methodValueCall", translation.Functions[2].Name)
	assert.Equal(t, "reflect_methodValueCall_abi0", translation.Functions[3].Name)
	assert.Contains(t, translation.Assembly, "\tbl runtime_spillArgs_abi0")
	assert.Contains(t, translation.Assembly, "\tbl reflect_moveMakeFuncArgPtrs")
	assert.Contains(t, translation.Assembly, "\tbl reflect_callReflect_abi0")
	assert.Contains(t, translation.Assembly, "\tbl runtime_unspillArgs_abi0")
}

func TestTranslateExactSHA256Assembly(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "crypto", "internal", "fips140", "sha256", "sha256block_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "crypto/internal/fips140/sha256",
		Filename:    "sha256block_arm64.s",
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 2)
	assert.Equal(t, "crypto_internal_fips140_sha256_blockSHA2", translation.Functions[0].Name)
	assert.Equal(t, "crypto_internal_fips140_sha256_blockSHA2_abi0", translation.Functions[1].Name)
	assert.Contains(t, translation.Assembly, "\tsha256h q2, q3, v9.4s")
	assert.Contains(t, translation.Assembly, "\tsha256su1 v4.4s, v6.4s, v7.4s")
}

func TestTranslateExactSHA512Assembly(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "crypto", "internal", "fips140", "sha512", "sha512block_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "crypto/internal/fips140/sha512",
		Filename:    "sha512block_arm64.s",
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 2)
	assert.Equal(t, "crypto_internal_fips140_sha512_blockSHA512", translation.Functions[0].Name)
	assert.Equal(t, "crypto_internal_fips140_sha512_blockSHA512_abi0", translation.Functions[1].Name)
	assert.Contains(t, translation.Assembly, "\tsha512h")
	assert.Contains(t, translation.Assembly, "\tsha512su1")
}

func TestTranslateExactMD5Assembly(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "crypto", "md5", "md5block_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "crypto/md5",
		Filename:    "md5block_arm64.s",
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 2)
	assert.Equal(t, "crypto_md5_block", translation.Functions[0].Name)
	assert.Equal(t, "crypto_md5_block_abi0", translation.Functions[1].Name)
	assert.Contains(t, translation.Assembly, "\tror w4, w4, #25")
}

func TestTranslateExactSHA1Assembly(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "crypto", "sha1", "sha1block_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "crypto/sha1",
		Filename:    "sha1block_arm64.s",
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 2)
	assert.Equal(t, "crypto_sha1_sha1block", translation.Functions[0].Name)
	assert.Equal(t, "crypto_sha1_sha1block_abi0", translation.Functions[1].Name)
	assert.Contains(t, translation.Assembly, "\tstr x3, [sp, #32]")
	assert.Contains(t, translation.Assembly, "\tstr x6, [sp, #56]")
	assert.Contains(t, translation.Assembly, "\tsha1c")
	assert.Contains(t, translation.Assembly, "\tsha1su1")
}

func TestTranslateExactCRC32Assembly(t *testing.T) {
	path := filepath.Join("..", "stdlib", "src", "hash", "crc32", "crc32_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "hash/crc32",
		Filename:    "crc32_arm64.s",
	})
	require.NoError(t, err)

	require.Len(t, translation.Functions, 4)
	assert.Equal(t, "hash_crc32_castagnoliUpdate", translation.Functions[0].Name)
	assert.Equal(t, "hash_crc32_castagnoliUpdate_abi0", translation.Functions[1].Name)
	assert.Equal(t, "hash_crc32_ieeeUpdate", translation.Functions[2].Name)
	assert.Equal(t, "hash_crc32_ieeeUpdate_abi0", translation.Functions[3].Name)
	assert.Contains(t, translation.Assembly, "\tcrc32cx w9, w9, x8")
	assert.Contains(t, translation.Assembly, "\tcrc32x w9, w9, x8")
}

func TestSupportsARM64FileKeepsTargetPolicyWithTranslator(t *testing.T) {
	assert.True(t, SupportsARM64File("runtime", "asm_arm64.s"))
	assert.True(t, SupportsARM64File("runtime", "asm.s"))
	assert.True(t, SupportsARM64File("runtime", "ints.s"))
	assert.True(t, SupportsARM64File("runtime", "memmove_arm64.s"))
	assert.True(t, SupportsARM64File("runtime", "secret_arm64.s"))
	assert.True(t, SupportsARM64File("runtime", "preempt_arm64.s"))
	assert.True(t, SupportsARM64File("runtime", "rt0_linux_arm64.s"))
	assert.True(t, SupportsARM64File("runtime", "tls_arm64.s"))
	assert.True(t, SupportsARM64File("runtime", "sys_linux_arm64.s"))
	assert.True(t, SupportsARM64File("internal/abi", "abi_test.s"))
	assert.True(t, SupportsARM64File("internal/abi", "stub.s"))
	assert.True(t, SupportsARM64File("internal/bytealg", "compare_arm64.s"))
	assert.True(t, SupportsARM64File("internal/bytealg", "index_arm64.s"))
	assert.True(t, SupportsARM64File("internal/cpu", "cpu.s"))
	assert.True(t, SupportsARM64File("internal/cpu", "cpu_arm64.s"))
	assert.True(t, SupportsARM64File("internal/chacha8rand", "chacha8_arm64.s"))
	assert.True(t, SupportsARM64File("internal/runtime/sys", "dit_arm64.s"))
	assert.True(t, SupportsARM64File("internal/runtime/sys", "empty.s"))
	assert.True(t, SupportsARM64File("internal/runtime/syscall/linux", "asm_linux_arm64.s"))
	assert.True(t, SupportsARM64File("internal/runtime/atomic", "atomic_arm64.s"))
	assert.True(t, SupportsARM64File("internal/reflectlite", "asm.s"))
	assert.True(t, SupportsARM64File("reflect", "asm_arm64.s"))
	assert.True(t, SupportsARM64File("crypto/internal/fips140/sha256", "sha256block_arm64.s"))
	assert.True(t, SupportsARM64File("crypto/internal/fips140/sha512", "sha512block_arm64.s"))
	assert.True(t, SupportsARM64File("crypto/md5", "md5block_arm64.s"))
	assert.True(t, SupportsARM64File("crypto/sha1", "sha1block_arm64.s"))
	assert.True(t, SupportsARM64File("hash/crc32", "crc32_arm64.s"))
	assert.True(t, SupportsARM64File("syscall", "asm_linux_arm64.s"))
	assert.True(t, SupportsARM64File("sync/atomic", "asm.s"))
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
		{packagePath: "runtime", name: "asm.s"},
		{packagePath: "runtime", name: "atomic_arm64.s"},
		{packagePath: "runtime", name: "ints.s"},
		{packagePath: "runtime", name: "memclr_arm64.s"},
		{packagePath: "runtime", name: "memmove_arm64.s"},
		{packagePath: "runtime", name: "preempt_arm64.s", defines: map[string]int64{
			"g_m": 48, "m_p": 208, "p_xRegs": 13800, "xRegPerP_scratch": 0, "xRegPerP_cache": 512,
		}},
		{packagePath: "runtime", name: "rt0_linux_arm64.s"},
		{packagePath: "runtime", name: "secret_arm64.s"},
		{packagePath: "runtime", name: "sys_linux_arm64.s", defines: runtimeAssemblyDefines()},
		{packagePath: "runtime", name: "tls_arm64.s"},
		{packagePath: "internal/abi", name: "abi_test.s"},
		{packagePath: "internal/abi", name: "stub.s"},
		{packagePath: "internal/bytealg", name: "compare_arm64.s"},
		{packagePath: "internal/bytealg", name: "count_arm64.s"},
		{packagePath: "internal/bytealg", name: "equal_arm64.s"},
		{packagePath: "internal/bytealg", name: "index_arm64.s"},
		{packagePath: "internal/bytealg", name: "indexbyte_arm64.s"},
		{packagePath: "internal/cpu", name: "cpu.s"},
		{packagePath: "internal/cpu", name: "cpu_arm64.s"},
		{packagePath: "internal/runtime/sys", name: "dit_arm64.s"},
		{packagePath: "internal/runtime/sys", name: "empty.s"},
		{packagePath: "internal/runtime/syscall/linux", name: "asm_linux_arm64.s"},
		{packagePath: "internal/chacha8rand", name: "chacha8_arm64.s"},
		{packagePath: "internal/reflectlite", name: "asm.s"},
		{packagePath: "internal/runtime/atomic", name: "atomic_arm64.s", defines: map[string]int64{"const_offsetARM64HasATOMICS": 135}},
		{packagePath: "sync/atomic", name: "asm.s"},
		{packagePath: "syscall", name: "asm_linux_arm64.s"},
	}
	for _, sourceFile := range files {
		path := filepath.Join("..", "stdlib", "src", filepath.FromSlash(sourceFile.packagePath), sourceFile.name)
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		var file *File
		if sourceFile.packagePath == "runtime" {
			file, err = ParseWithOptions(bytes.NewReader(source), runtimeARM64ParseOptions(t))
		} else {
			file, err = Parse(bytes.NewReader(source))
		}
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
	file, err := ParseWithOptions(bytes.NewReader(source), runtimeARM64ParseOptions(t))
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

func TestExecuteExactRuntimeAsyncPreemptRegisterSave(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 execution is required")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc is unavailable")
	}

	path := filepath.Join("..", "stdlib", "src", "runtime", "preempt_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := Parse(bytes.NewReader(source))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath: "runtime",
		Filename:    "preempt_arm64.s",
		Defines: map[string]int64{
			"g_m":              48,
			"m_p":              208,
			"p_xRegs":          13800,
			"xRegPerP_scratch": 0,
			"xRegPerP_cache":   512,
		},
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	// The adapter builds the 16-byte call record normally injected by the
	// runtime's signal handler. asyncPreempt2 points the cache at the exact
	// vector state that asyncPreempt saved, allowing the unchanged routine to
	// restore it before the adapter verifies representative registers.
	harness := translation.Assembly + `
.text
.global runtime_asyncPreempt2_abi0
.type runtime_asyncPreempt2_abi0, %function
runtime_asyncPreempt2_abi0:
	sub x9, x0, #512
	str x9, [x0]
	ret
.size runtime_asyncPreempt2_abi0, .-runtime_asyncPreempt2_abi0

.global test_preempt
.type test_preempt, %function
test_preempt:
	sub sp, sp, #176
	stp x29, x30, [sp]
	stp x19, x20, [sp, #16]
	stp x21, x22, [sp, #32]
	stp x23, x24, [sp, #48]
	stp x25, x26, [sp, #64]
	stp x27, x28, [sp, #80]
	stp d8, d9, [sp, #96]
	stp d10, d11, [sp, #112]
	stp d12, d13, [sp, #128]
	stp d14, d15, [sp, #144]
	mrs x9, fpsr
	str x9, [sp, #160]

	adrp x28, .Lpreempt_g
	add x28, x28, :lo12:.Lpreempt_g
	adrp x9, .Lpreempt_m
	add x9, x9, :lo12:.Lpreempt_m
	str x9, [x28, #48]
	adrp x10, .Lpreempt_p
	add x10, x10, :lo12:.Lpreempt_p
	str x10, [x9, #208]

	mov x0, #101
	mov x1, #102
	mov x17, #118
	mov x19, #120
	mov x26, #127
	mov x9, #201
	dup v0.2d, x9
	mov x9, #232
	dup v31.2d, x9
	mov x9, #1
	msr fpsr, x9
	cmp x0, x0

	str x30, [sp, #-16]!
	adr x30, .Lpreempt_resume
	b runtime_asyncPreempt

.Lpreempt_resume:
	mrs x9, nzcv
	mov x10, #0x60000000
	cmp x9, x10
	b.ne .Lpreempt_fail_1
	mrs x9, fpsr
	cmp x9, #1
	b.ne .Lpreempt_fail_2
	cmp x0, #101
	b.ne .Lpreempt_fail_3
	cmp x1, #102
	b.ne .Lpreempt_fail_4
	cmp x17, #118
	b.ne .Lpreempt_fail_5
	cmp x19, #120
	b.ne .Lpreempt_fail_6
	cmp x26, #127
	b.ne .Lpreempt_fail_7
	umov x9, v0.d[0]
	cmp x9, #201
	b.ne .Lpreempt_fail_8
	umov x9, v31.d[0]
	cmp x9, #232
	b.ne .Lpreempt_fail_9
	mov x0, #0
	b .Lpreempt_done
.Lpreempt_fail_1:
	mov x0, #1
	b .Lpreempt_done
.Lpreempt_fail_2:
	mov x0, #2
	b .Lpreempt_done
.Lpreempt_fail_3:
	mov x0, #3
	b .Lpreempt_done
.Lpreempt_fail_4:
	mov x0, #4
	b .Lpreempt_done
.Lpreempt_fail_5:
	mov x0, #5
	b .Lpreempt_done
.Lpreempt_fail_6:
	mov x0, #6
	b .Lpreempt_done
.Lpreempt_fail_7:
	mov x0, #7
	b .Lpreempt_done
.Lpreempt_fail_8:
	mov x0, #8
	b .Lpreempt_done
.Lpreempt_fail_9:
	mov x0, #9
.Lpreempt_done:
	ldr x9, [sp, #160]
	msr fpsr, x9
	ldp d14, d15, [sp, #144]
	ldp d12, d13, [sp, #128]
	ldp d10, d11, [sp, #112]
	ldp d8, d9, [sp, #96]
	ldp x27, x28, [sp, #80]
	ldp x25, x26, [sp, #64]
	ldp x23, x24, [sp, #48]
	ldp x21, x22, [sp, #32]
	ldp x19, x20, [sp, #16]
	ldp x29, x30, [sp]
	add sp, sp, #176
	ret
.size test_preempt, .-test_preempt

.global main
.type main, %function
main:
	b test_preempt
.size main, .-main

.bss
.balign 16
.Lpreempt_g:
	.zero 64
.balign 16
.Lpreempt_m:
	.zero 216
.balign 16
.Lpreempt_p:
	.zero 14320
`
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "preempt.S")
	executable := filepath.Join(directory, "preempt")
	require.NoError(t, os.WriteFile(sourcePath, []byte(harness), 0o644))
	command := exec.Command(cc, "-no-pie", "-o", executable, sourcePath)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	output, err = exec.Command(executable).CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestExecuteExactRuntimeTLSWithoutCgo(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("AArch64 execution is required")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc is unavailable")
	}

	path := filepath.Join("..", "stdlib", "src", "runtime", "tls_arm64.s")
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := ParseWithOptions(bytes.NewReader(source), runtimeARM64ParseOptions(t))
	require.NoError(t, err)
	translation, err := CompileARM64(file, ARM64Options{
		PackagePath:      "runtime",
		Filename:         "tls_arm64.s",
		PreferDirectABI0: true,
	})
	require.NoError(t, err)

	// With cgo disabled, load_g and save_g deliberately leave the dedicated g
	// register alone. This is the path used by cg12go's current runtime build.
	harness := translation.Assembly + `
.text
.global test_tls
.type test_tls, %function
test_tls:
	stp x28, x30, [sp, #-16]!
	mov x28, #1234
	bl runtime_load_g
	mov x9, #1234
	cmp x28, x9
	b.ne .Ltls_fail_load
	bl runtime_save_g
	mov x9, #1234
	cmp x28, x9
	b.ne .Ltls_fail_save
	mov w0, #0
	b .Ltls_done
.Ltls_fail_load:
	mov w0, #1
	b .Ltls_done
.Ltls_fail_save:
	mov w0, #2
.Ltls_done:
	ldp x28, x30, [sp], #16
	ret
.size test_tls, .-test_tls

.global main
.type main, %function
main:
	b test_tls
.size main, .-main

.data
.global runtime_iscgo
.type runtime_iscgo, %object
.size runtime_iscgo, 1
runtime_iscgo:
	.byte 0
`
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "tls.S")
	executable := filepath.Join(directory, "tls")
	require.NoError(t, os.WriteFile(sourcePath, []byte(harness), 0o644))
	command := exec.Command(cc, "-no-pie", "-o", executable, sourcePath)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	output, err = exec.Command(executable).CombinedOutput()
	require.NoError(t, err, "%s", output)
}
