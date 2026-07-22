package arm64

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/ir"
)

func deferredFunction(keepRecoveryEntry bool) *ir.Module {
	module := ir.NewModule()
	function := module.NewFuncVoid("deferred")
	function.ManagedFrame = true
	function.Entry().CallVoid(function.Sym("runtime.deferproc", 0), function.Sym("deferred.func1", 0))
	function.Entry().Hlt()

	recovery := function.NewBlock("deferreturn")
	recovery.SecondaryEntry = keepRecoveryEntry
	recovery.CallVoid(function.Sym("runtime.deferreturn", 0))
	recovery.RetVoid()
	return module
}

func TestCompileKeepsDeferReturnSecondaryEntry(t *testing.T) {
	object, err := CompileToObject(deferredFunction(true))
	require.NoError(t, err)
	assert.NotEmpty(t, object.Text)
}

func TestCompileRejectsMissingDeferReturnContinuation(t *testing.T) {
	_, err := CompileToObject(deferredFunction(false))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime defer registration has no defer-return continuation")
}
