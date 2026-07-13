package x64

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A forward branch's displacement is the distance from the instruction end to
// the label; a backward branch's is negative.
func TestProgramBranchFixups(t *testing.T) {
	p := NewProgram()
	p.Label("top")
	p.Emit(AddImm(true, RAX, 1)) // 4 bytes
	p.Jcc(NE, "top")             // 6 bytes: jumps back to offset 0
	p.Jmp("end")                 // 5 bytes: forward
	p.Emit(Ret())                // 1 byte
	p.Label("end")
	p.Emit(Ret())

	code, err := p.Bytes()
	require.NoError(t, err)

	// Jcc "top": at offset 4, end at 10, target 0 => rel = 0 - 10 = -10.
	require.Equal(t, disasm(t, code[4:10]), "jne -10")
	// Jmp "end": at offset 10, end at 15, target 16 => rel = 16 - 15 = 1.
	require.Equal(t, disasm(t, code[10:15]), "jmp 1")
}

func TestProgramUndefinedLabel(t *testing.T) {
	p := NewProgram()
	p.Jmp("nowhere")
	_, err := p.Bytes()
	require.Error(t, err)
}
