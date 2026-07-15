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

func TestTranslateExactRuntimeARM64FilesAssemble(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("GNU AArch64 assembler is required")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc is unavailable")
	}

	var assembly bytes.Buffer
	for _, name := range []string{"atomic_arm64.s", "memclr_arm64.s", "memmove_arm64.s"} {
		path := filepath.Join("..", "stdlib", "src", "runtime", name)
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := Parse(bytes.NewReader(source))
		require.NoError(t, err, name)
		translated, err := TranslateARM64(file, ARM64Options{PackagePath: "runtime", Filename: name})
		require.NoError(t, err, name)
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
