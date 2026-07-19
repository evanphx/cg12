package arm64

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExpandAsmQModifier(t *testing.T) {
	text, err := expandAsm("mov %q0, %q1", []asmVal{
		{reg: vReg(3), width: 16},
		{reg: vReg(7), width: 16},
	})
	require.NoError(t, err)
	require.Equal(t, "mov q3, q7", text)
}
