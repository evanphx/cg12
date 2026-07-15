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
