package arm64

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareAssemblyCollectsCodeReferencesAndFunctions(t *testing.T) {
	module := ir.NewModule()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/example_arm64.s",
		Source: `TEXT ·example<ABIInternal>(SB),NOSPLIT|NOFRAME,$0-8
	MOVBU runtime·flag(SB), R0
	RET
`,
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	assert.Contains(t, bundle.source, ".global runtime_example")
	assert.True(t, bundle.references["runtime_flag"])
	require.Len(t, bundle.functions, 1)
	assert.Equal(t, "runtime_example", bundle.functions[0].name)
	assert.Equal(t, byte(goFuncFlagAsm), bundle.functions[0].funcFlag)
}

func TestPrepareAssemblyMarksAsyncPreemptForRuntimeStackScanning(t *testing.T) {
	module := ir.NewModule()
	module.Assembly = append(module.Assembly, ir.AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/preempt_arm64.s",
		Source: `TEXT ·asyncPreempt(SB),NOSPLIT|NOFRAME,$0-0
	MOVD R30, -240(RSP)
	SUB $240, RSP
	RET
`,
	})

	bundle, err := prepareAssembly(module)
	require.NoError(t, err)
	require.Len(t, bundle.functions, 1)
	assert.Equal(t, "runtime_asyncPreempt", bundle.functions[0].name)
	assert.Equal(t, 240, bundle.functions[0].frameSize)
	assert.Equal(t, 8, bundle.functions[0].frameStart)
	assert.Equal(t, byte(3), bundle.functions[0].funcID)
	assert.Equal(t, byte(goFuncFlagAsm), bundle.functions[0].funcFlag)
}
