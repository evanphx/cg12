package ir

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntrinsicRegistry(t *testing.T) {
	save := LookupIntrinsic("stacksave")
	require.NotNil(t, save)
	assert.True(t, save.HasResult)
	assert.False(t, save.SideEffect, "an unused stacksave is dead")
	assert.False(t, save.Pure, "the live stack pointer is not a function of operands")

	restore := LookupIntrinsic("stackrestore")
	require.NotNil(t, restore)
	assert.True(t, restore.SideEffect, "stackrestore moves the stack pointer")
	assert.False(t, restore.HasResult)

	for _, name := range []string{"getcallerpc", "getcallersp"} {
		callerFrame := LookupIntrinsic(name)
		require.NotNil(t, callerFrame)
		assert.True(t, callerFrame.HasResult)
		assert.False(t, callerFrame.SideEffect)
		assert.False(t, callerFrame.Pure, "the caller frame depends on the current invocation")
	}

	assert.Nil(t, LookupIntrinsic("no-such-intrinsic"))
}

func TestAtomicsRegistered(t *testing.T) {
	// Every atomic is side-effecting (so the optimizer never removes, moves, or
	// shares it) and touches memory as its kind implies.
	load := LookupIntrinsic("atomic.load.l")
	require.NotNil(t, load)
	assert.True(t, load.SideEffect)
	assert.True(t, load.ReadsMemory)
	assert.False(t, load.WritesMemory)
	assert.False(t, load.Pure, "an atomic is never a pure, shareable value")

	store := LookupIntrinsic("atomic.store.w")
	require.NotNil(t, store)
	assert.True(t, store.SideEffect)
	assert.True(t, store.WritesMemory)
	assert.False(t, store.HasResult)

	for _, op := range []string{"xchg", "cas", "add", "sub", "and", "or", "xor"} {
		rmw := LookupIntrinsic("atomic." + op + ".l")
		require.NotNilf(t, rmw, "atomic.%s.l", op)
		assert.Truef(t, rmw.HasResult && rmw.ReadsMemory && rmw.WritesMemory,
			"atomic.%s.l reads, writes, and returns the previous value", op)
	}

	assert.NotNil(t, LookupIntrinsic("atomic.fence"))
}

func TestAtomicBuilder(t *testing.T) {
	f := NewModule().NewFuncVoid("f")
	e := f.Entry()
	p := e.Intrinsic("stacksave", ClsP) // any pointer will do for the shape test
	e.AtomicStore(ClsL, p, f.Long(1))
	got := e.AtomicRMW("add", ClsL, p, f.Long(2))
	e.RetVoid()

	// The width and operation live in the name.
	assert.Equal(t, "atomic.store.l", e.Instrs[1].Intrin.Name)
	assert.True(t, e.Instrs[1].To.IsNone(), "a store yields nothing")
	assert.Equal(t, "atomic.add.l", e.Instrs[2].Intrin.Name)
	assert.Equal(t, got, e.Instrs[2].To, "an RMW returns the previous value")
}

func TestRegisterIntrinsicRejectsDuplicate(t *testing.T) {
	assert.Panics(t, func() {
		RegisterIntrinsic("stacksave", IntrinsicEffects{})
	}, "re-registering a name would let two descriptions disagree")
}

func TestIntrinsicBuilder(t *testing.T) {
	f := NewModule().NewFuncVoid("f")
	e := f.Entry()
	sp := e.StackSave()
	e.StackRestore(sp)
	e.RetVoid()

	require.Len(t, e.Instrs, 2)
	save := e.Instrs[0]
	assert.Equal(t, OIntrinsic, save.Op)
	require.NotNil(t, save.Intrin)
	assert.Equal(t, "stacksave", save.Intrin.Name)
	assert.Equal(t, RefTemp, save.To.Kind, "stacksave yields a value")

	restore := e.Instrs[1]
	assert.Equal(t, "stackrestore", restore.Intrin.Name)
	assert.True(t, restore.To.IsNone(), "stackrestore yields nothing")
	assert.Equal(t, sp, restore.Arg(0))
}

func TestIntrinsicPrints(t *testing.T) {
	f := NewModule().NewFuncVoid("f")
	e := f.Entry()
	sp := e.StackSave()
	e.StackRestore(sp)
	e.RetVoid()

	text := f.String()
	assert.Contains(t, text, "intrinsic stacksave")
	assert.Contains(t, text, "intrinsic stackrestore")
	// The result-producing one names its temporary; the void one does not.
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "stacksave") {
			assert.Contains(t, line, "=p intrinsic stacksave")
		}
	}
}

func TestIntrinsicBinaryRoundTrip(t *testing.T) {
	m := NewModule()
	f := m.NewFuncVoid("f").Export()
	e := f.Entry()
	sp := e.StackSave()
	e.StackRestore(sp)
	e.RetVoid()

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	got, err := DecodeModule(data)
	require.NoError(t, err)

	in := got.Funcs[0].Start.Instrs
	require.Len(t, in, 2)
	require.NotNil(t, in[0].Intrin)
	assert.Equal(t, "stacksave", in[0].Intrin.Name)
	require.NotNil(t, in[1].Intrin)
	assert.Equal(t, "stackrestore", in[1].Intrin.Name)
}

func TestVerifyIntrinsicPayload(t *testing.T) {
	// An OIntrinsic with no name is a construction the builder never makes but a
	// decoder or fuzzer can; verify names it rather than letting a backend panic.
	f := NewModule().NewFuncVoid("f")
	e := f.Entry()
	e.Instrs = append(e.Instrs, Instr{Op: OIntrinsic})
	e.RetVoid()
	err := Verify(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no name")

	// A name no one registered is likewise rejected: nothing knows how to run or
	// lower it.
	g := NewModule().NewFuncVoid("g")
	ge := g.Entry()
	ge.IntrinsicVoid("frobnicate")
	ge.RetVoid()
	err = Verify(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown intrinsic")
}
