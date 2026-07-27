package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// This backend has no Go ABI support at all: no ABIInternal argument assignment,
// no morestack prologue, no g-relative stack-guard check, no Go stack maps. It
// nevertheless used to accept a function annotated for those features and quietly
// emit an ordinary System V frame -- a managed-frame function came out as
// `push %rbp; mov %rsp,%rbp; mov $0x2a,%eax; leave; ret`, with err == nil and no
// stack-growth check anywhere. These tests pin the managed-frame annotation down
// as a hard error so the missing feature cannot be mistaken for a working one,
// and pin down the annotations that are deliberately still accepted.

// goABIModule builds a module with a single exported `runtest` returning 42, with
// annotate applied to it before compilation. The body is as trivial as a function
// can be on purpose: what is under test is the annotation, not the code.
func goABIModule(annotate func(*ir.Func)) *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("runtest", ir.ClsW).Export()
	annotate(f)
	entry := f.Entry()
	entry.Ret(f.Word(42))
	return m
}

// goABIEntryPoint is one way into amd64 codegen, reduced to just its error.
type goABIEntryPoint struct {
	name    string
	compile func(*ir.Module) error
}

// goABIEntryPoints lists every entry point that can reach codegen. They all
// funnel through CompileToObjectWith today, but a guard installed on one path is
// worthless if another path still reaches the emitter, so each is exercised
// separately rather than trusting that the delegation stays that way.
func goABIEntryPoints() []goABIEntryPoint {
	return []goABIEntryPoint{
		{"CompileObject", func(m *ir.Module) error {
			_, err := amd64.CompileObject(m)
			return err
		}},
		{"CompileObjectWith", func(m *ir.Module) error {
			_, err := amd64.CompileObjectWith(m, amd64.Options{})
			return err
		}},
		{"CompileToObject", func(m *ir.Module) error {
			_, err := amd64.CompileToObject(m)
			return err
		}},
		{"CompileToObjectWith", func(m *ir.Module) error {
			_, err := amd64.CompileToObjectWith(m, amd64.Options{})
			return err
		}},
		{"Backend.CompileModule", func(m *ir.Module) error {
			_, err := amd64.Backend{}.CompileModule(m)
			return err
		}},
	}
}

// A managed frame means the Go runtime owns stack growth and frame metadata. With
// no morestack prologue emitted, such a function overruns a goroutine's stack
// instead of growing it, so accepting the annotation is worse than refusing it.
func TestManagedFrameIsRejected(t *testing.T) {
	for _, entry := range goABIEntryPoints() {
		t.Run(entry.name, func(t *testing.T) {
			m := goABIModule(func(f *ir.Func) {
				f.ManagedFrame = true
			})
			err := entry.compile(m)
			require.Error(t, err)
			require.Contains(t, err.Error(), "unsupported managed frame")
			require.Contains(t, err.Error(), "runtest", "the error must name the offending function")
		})
	}
}

// The guard has to reject exactly the annotated functions and nothing else: a
// guard that rejected every module would satisfy the tests above while breaking
// the C path this backend exists to serve.
func TestPlatformCallConventionStillCompiles(t *testing.T) {
	for _, entry := range goABIEntryPoints() {
		t.Run(entry.name, func(t *testing.T) {
			m := goABIModule(func(f *ir.Func) {
				f.CallConv = ir.CallConvPlatform
			})
			require.NoError(t, entry.compile(m))
		})
	}

	// And it really is compiled, not merely accepted: `runtest` must come out as
	// a .text symbol with a body.
	m := goABIModule(func(f *ir.Func) {
		f.CallConv = ir.CallConvPlatform
	})
	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)
	var found bool
	for _, s := range o.Syms {
		if s.Name == "runtest" {
			found = true
			require.Equal(t, obj.SecText, s.Section)
			require.NotZero(t, s.Size, "runtest must have a body")
		}
	}
	require.True(t, found, "expected a runtest symbol")
}

// CallConvGoInternal on its own is intentionally accepted, and this test exists to
// stop a later "tightening" from being made by accident. amd64 does not implement
// ABIInternal's register assignment, so rejecting the annotation looks obviously
// right -- but CallConv is not currently a trustworthy signal that a function needs
// Go's ABI. goc stamps it on every closure-shaped function (function literals at
// goc/compile.go:10119, method-value wrappers at :9781, funcvalue adapters at
// :10638) while passing the environment through an ordinary fixed-register
// temporary instead of through the convention, so those bodies are self-consistent
// System V code that runs correctly today. Rejecting them catches no miscompile and
// costs 14 goc corpus subtests that build natively on amd64 (measured). What is
// genuinely dangerous is a managed frame, which is guarded above. If you want this
// error, first check whether those frontend sites still over-apply the annotation.
func TestGoInternalCallConventionWithoutManagedFrameStillCompiles(t *testing.T) {
	for _, entry := range goABIEntryPoints() {
		t.Run(entry.name, func(t *testing.T) {
			m := goABIModule(func(f *ir.Func) {
				f.CallConv = ir.CallConvGoInternal
			})
			require.NoError(t, entry.compile(m))
		})
	}
}

// The pairing is what the guard keys on, so the convention plus a managed frame --
// how a real Go function is annotated -- must still be refused, for the frame.
func TestGoInternalCallConventionWithManagedFrameIsRejected(t *testing.T) {
	m := goABIModule(func(f *ir.Func) {
		f.CallConv = ir.CallConvGoInternal
		f.ManagedFrame = true
	})
	_, err := amd64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported managed frame")
}

// NoSplit and SystemStack are intentionally outside the guard. Both only tune a
// managed frame's stack-growth check -- NoSplit omits it, SystemStack points it at
// g.stackguard1 and runtime_morestackc -- and an unmanaged platform-ABI function
// emits no such check for them to change. The C path already sets NoSplit on
// plain platform-ABI functions (goc's runtime coverage dump, semantic assembly's
// NOSPLIT flag), and that code compiles correctly today, so rejecting the flags
// would break working callers while catching no wrong code.
func TestNoSplitAndSystemStackWithoutManagedFrameStillCompile(t *testing.T) {
	m := goABIModule(func(f *ir.Func) {
		f.NoSplit = true
		f.SystemStack = true
	})
	_, err := amd64.CompileObject(m)
	require.NoError(t, err)
}

// A module is rejected because of any function in it, not just the first one the
// emitter happens to reach -- and the rejection lands before lowering has started
// mutating the module in place.
func TestGoABIGuardCoversLaterFunctionsInTheModule(t *testing.T) {
	m := ir.NewModule()
	plain := m.NewFunc("plain", ir.ClsW).Export()
	plain.Entry().Ret(plain.Word(1))
	managed := m.NewFunc("managed", ir.ClsW).Export()
	managed.ManagedFrame = true
	managed.Entry().Ret(managed.Word(2))

	_, err := amd64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "managed")
	require.Contains(t, err.Error(), "unsupported managed frame")
}
