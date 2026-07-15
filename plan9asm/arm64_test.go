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
	assert.Contains(t, assembly, "\ttbz x2, #3, .Lcopy_arm64_0_done")
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

	assert.Contains(t, assembly, ".global .Lcompare_arm64_helper")
	assert.Contains(t, assembly, ".hidden .Lcompare_arm64_helper")
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

func TestSupportsARM64FileKeepsTargetPolicyWithTranslator(t *testing.T) {
	assert.True(t, SupportsARM64File("runtime", "memmove_arm64.s"))
	assert.True(t, SupportsARM64File("internal/bytealg", "compare_arm64.s"))
	assert.True(t, SupportsARM64File("internal/bytealg", "index_arm64.s"))
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
	}{
		{packagePath: "runtime", name: "atomic_arm64.s"},
		{packagePath: "runtime", name: "memclr_arm64.s"},
		{packagePath: "runtime", name: "memmove_arm64.s"},
		{packagePath: "internal/bytealg", name: "compare_arm64.s"},
		{packagePath: "internal/bytealg", name: "count_arm64.s"},
		{packagePath: "internal/bytealg", name: "equal_arm64.s"},
		{packagePath: "internal/bytealg", name: "index_arm64.s"},
		{packagePath: "internal/bytealg", name: "indexbyte_arm64.s"},
	}
	for _, sourceFile := range files {
		path := filepath.Join("..", "stdlib", "src", filepath.FromSlash(sourceFile.packagePath), sourceFile.name)
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := Parse(bytes.NewReader(source))
		require.NoError(t, err, sourceFile.name)
		translated, err := TranslateARM64(file, ARM64Options{PackagePath: sourceFile.packagePath, Filename: sourceFile.name})
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
