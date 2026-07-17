package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func TestDeadAllocPreservesStoresObservedByMemoryHelper(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("copy_local")
	entry := function.Entry()
	source := entry.Alloc(8, 8)
	destination := entry.Alloc(8, 8)
	entry.Store(function.Long(42), source)
	entry.CallVoid(function.Sym("goc_memcpy", 0), destination, source, function.Long(8))
	entry.RetVoid()

	assert.False(t, DeadAlloc(function))
	assert.Equal(t, ir.OStorel, entry.Instrs[2].Op)
}
