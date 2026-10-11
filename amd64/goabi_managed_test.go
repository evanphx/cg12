package amd64_test

import (
	"bytes"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// These tests pin down what a managed frame compiles to on amd64. A managed
// frame means the Go runtime owns stack growth: the function must check its
// stack against g.stackguard0 (g lives in R14) before building its frame, and
// call runtime.morestack when it does not fit.

// managedModule builds a module with a single exported `runtest` returning 42,
// with annotate applied to it before compilation.
func managedModule(annotate func(*ir.Func)) *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("runtest", ir.ClsW).Export()
	annotate(f)
	entry := f.Entry()
	entry.Ret(f.Word(42))
	return m
}

// relocationsTo lists the text relocations against a symbol.
func relocationsTo(o *obj.Object, symbol string) []obj.Reloc {
	var found []obj.Reloc
	for _, relocation := range o.Relocs {
		if relocation.Sym == symbol {
			found = append(found, relocation)
		}
	}
	return found
}

// The stack check compares the would-be SP with g.stackguard0, read from R14.
// `cmp 0x10(%r14), %r12` is 4d 3b 66 10.
var stackGuard0Compare = []byte{0x4d, 0x3b, 0x66, 0x10}

func TestManagedFrameChecksTheStackAndCallsMorestack(t *testing.T) {
	m := managedModule(func(f *ir.Func) {
		f.ManagedFrame = true
	})
	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)
	require.True(t, bytes.Contains(o.Text, stackGuard0Compare), "the prologue must compare against g.stackguard0 in R14")
	require.Len(t, relocationsTo(o, "runtime_morestack_noctxt"), 1)
}

// A nosplit managed function promises not to grow the stack, so it carries no
// check at all.
func TestNoSplitManagedFrameHasNoStackCheck(t *testing.T) {
	m := managedModule(func(f *ir.Func) {
		f.ManagedFrame = true
		f.NoSplit = true
	})
	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)
	require.False(t, bytes.Contains(o.Text, stackGuard0Compare))
	require.Empty(t, relocationsTo(o, "runtime_morestack_noctxt"))
}

// A system-stack function checks g.stackguard1 and grows through morestackc,
// which reports the overflow instead of growing a stack that cannot grow.
func TestSystemStackManagedFrameUsesStackGuard1(t *testing.T) {
	m := managedModule(func(f *ir.Func) {
		f.ManagedFrame = true
		f.SystemStack = true
	})
	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)
	require.True(t, bytes.Contains(o.Text, []byte{0x4d, 0x3b, 0x66, 0x18}), "the prologue must compare against g.stackguard1")
	require.Len(t, relocationsTo(o, "runtime_morestackc"), 1)
}

// A plain System V function is untouched by any of this.
func TestPlatformFunctionHasNoStackCheck(t *testing.T) {
	m := managedModule(func(f *ir.Func) {
		f.CallConv = ir.CallConvPlatform
	})
	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)
	require.False(t, bytes.Contains(o.Text, stackGuard0Compare))
	require.Empty(t, relocationsTo(o, "runtime_morestack_noctxt"))
}

// CallConvGoInternal without a managed frame compiles: ABIInternal register
// assignment no longer needs the runtime's stack growth.
func TestGoInternalCallConventionWithoutManagedFrameCompiles(t *testing.T) {
	m := managedModule(func(f *ir.Func) {
		f.CallConv = ir.CallConvGoInternal
	})
	_, err := amd64.CompileObject(m)
	require.NoError(t, err)
}
