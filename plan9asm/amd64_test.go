package plan9asm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireAMD64Assembler returns a C compiler driver that assembles x86-64
// GNU syntax, skipping the test when there is none.
func requireAMD64Assembler(t *testing.T) string {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("an x86-64 GNU assembler is required")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc is unavailable")
	}
	return cc
}

func assembleAMD64(t *testing.T, cc, name, source string) string {
	t.Helper()
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, name+".S")
	objectPath := filepath.Join(directory, name+".o")
	require.NoError(t, os.WriteFile(sourcePath, []byte(source), 0o644))
	output, err := exec.Command(cc, "-c", "-o", objectPath, sourcePath).CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, numberLines(source))
	return objectPath
}

func numberLines(source string) string {
	var output strings.Builder
	for index, line := range strings.Split(source, "\n") {
		fmt.Fprintf(&output, "%5d  %s\n", index+1, line)
	}
	return output.String()
}

func TestSupportsAMD64FileMatchesSelectedAssembly(t *testing.T) {
	assert.True(t, SupportsAMD64File("runtime", "asm_amd64.s"))
	assert.True(t, SupportsAMD64File("runtime", "rt0_linux_amd64.s"))
	assert.True(t, SupportsAMD64File("runtime", "sys_linux_amd64.s"))
	assert.True(t, SupportsAMD64File("internal/bytealg", "indexbyte_amd64.s"))
	assert.True(t, SupportsAMD64Package("internal/runtime/atomic"))
	assert.False(t, SupportsAMD64File("runtime", "asm_arm64.s"))
	assert.False(t, SupportsAMD64Package("other"))
	assert.False(t, SupportsARM64File("runtime", "asm_amd64.s"))
}

// TestTranslateAMD64AllowlistAssembles translates every allowlisted amd64 file
// (and the selected files kept off the list, amd64UnlistedFiles) from the real
// Go 1.26.1 sources and assembles each result with the system C compiler.
// PLAN9ASM_AMD64_DUMP=dir keeps the GNU output.
func TestTranslateAMD64AllowlistAssembles(t *testing.T) {
	cc := requireAMD64Assembler(t)
	for _, file := range amd64VerifiedFiles() {
		t.Run(file.packagePath+"/"+file.name, func(t *testing.T) {
			translation := translateAMD64SourceFile(t, file)
			if directory := os.Getenv("PLAN9ASM_AMD64_DUMP"); directory != "" {
				// Keep the GNU output for inspection.
				name := strings.ReplaceAll(file.packagePath, "/", "_") + "_" + strings.TrimSuffix(file.name, ".s") + ".S"
				require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(translation.Assembly), 0o644))
			}
			assembleAMD64(t, cc, strings.TrimSuffix(file.name, ".s"), translation.Assembly)
		})
	}
}
