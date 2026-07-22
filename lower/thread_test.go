package lower

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/ir"
)

// ThreadJumps retargets a branch through a chain of empty jump-only blocks and
// drops the blocks left unreachable.
func TestThreadJumpsCollapsesChain(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	e := f.Entry()
	t1 := f.NewBlock("t1") // empty forwarder
	t2 := f.NewBlock("t2") // empty forwarder
	end := f.NewBlock("end")

	e.Goto(t1)
	t1.Goto(t2)
	t2.Goto(end)
	end.Ret(f.Word(0))

	ThreadJumps(f)

	assert.Equal(t, ir.JmpJmp, e.Jmp.Kind)
	assert.Same(t, end, e.Jmp.To, "entry should jump straight to end")
	for _, b := range f.Blocks {
		assert.NotSame(t, t1, b, "empty forwarder t1 should be dropped")
		assert.NotSame(t, t2, b, "empty forwarder t2 should be dropped")
	}
}

// A block whose address is taken (a computed-goto landing pad) is never bypassed
// or removed, even when it is an empty forwarder: an indirect branch can reach it
// without any explicit edge.
func TestThreadJumpsKeepsAddressTakenBlock(t *testing.T) {
	f := ir.NewModule().NewFunc("f", ir.ClsW)
	e := f.Entry()
	pad := f.NewBlock("pad") // empty, but address-taken
	end := f.NewBlock("end")

	addr := e.BlockAddr(pad) // takes pad's address
	e.Store(addr, e.Alloc(8, 8))
	e.Goto(pad)
	pad.Goto(end)
	end.Ret(f.Word(0))

	ThreadJumps(f)

	require.Same(t, pad, e.Jmp.To, "the branch to an address-taken block must not be threaded past it")
	found := false
	for _, b := range f.Blocks {
		if b == pad {
			found = true
		}
	}
	assert.True(t, found, "address-taken block must be kept")
}

func TestThreadJumpsKeepsSecondaryEntry(t *testing.T) {
	function := ir.NewModule().NewFuncVoid("f")
	function.Entry().Hlt()
	recovery := function.NewBlock("recovery")
	recovery.SecondaryEntry = true
	recovery.RetVoid()

	ThreadJumps(function)

	assert.Contains(t, function.Blocks, recovery)
}
