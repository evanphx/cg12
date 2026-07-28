package goc_test

import (
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func findFunctionNamed(module *ir.Module, name string) *ir.Func {
	for _, function := range module.Funcs {
		if function.Name == name {
			return function
		}
	}
	return nil
}

func functionNamed(t *testing.T, module *ir.Module, name string) *ir.Func {
	t.Helper()

	function := findFunctionNamed(module, name)
	require.NotNil(t, function, "function %q not found", name)
	return function
}

// genericDispatchSource mirrors the crypto/internal/fips140/ecdsa shape: a
// constraint whose type set is a union of pointer types and whose methods are
// declared in terms of the type parameter, used from one generic body that is
// instantiated more than once.
const genericDispatchSource = `package main

type alpha struct{ n int }

type beta struct{ n int }

func (a *alpha) Scale(k int) (*alpha, error) { return &alpha{n: a.n * k}, nil }

func (a *alpha) Read() int { return a.n }

func (b *beta) Scale(k int) (*beta, error) { return &beta{n: b.n*k + 1}, nil }

func (b *beta) Read() int { return b.n }

func (b beta) Plain() int { return b.n }

type scalable[P any] interface {
	*alpha | *beta

	Scale(int) (P, error)
	Read() int
}

type plain interface {
	*beta

	Plain() int
}

type carrier[P scalable[P]] struct {
	construct func() P
}

func derive[P scalable[P]](c *carrier[P], k int) int {
	scaled, err := c.construct().Scale(k)
	if err != nil {
		return 0
	}
	return scaled.Read()
}

func readPlain[P plain](value P) int { return value.Plain() }

type cell[T any] struct{ n int }

func (c *cell[T]) Count() int { return c.n }

type countable interface {
	*cell[int] | *cell[string]

	Count() int
}

func count[P countable](c P) int { return c.Count() }

type embeddedBase struct{ n int }

func (b *embeddedBase) Read() int { return b.n }

type narrowWrapper struct {
	lead int
	embeddedBase
}

type wideWrapper struct {
	lead [5]int
	embeddedBase
}

type promoted interface {
	*narrowWrapper | *wideWrapper

	Read() int
}

func readPromoted[P promoted](value P) int { return value.Read() }

func Test() int {
	alphaCarrier := &carrier[*alpha]{construct: func() *alpha { return &alpha{n: 2} }}
	betaCarrier := &carrier[*beta]{construct: func() *beta { return &beta{n: 2} }}
	return derive(alphaCarrier, 3) +
		derive(betaCarrier, 3) +
		readPlain(&beta{n: 1}) +
		count(&cell[int]{n: 1}) +
		count(&cell[string]{n: 1}) +
		readPromoted(&narrowWrapper{}) +
		readPromoted(&wideWrapper{})
}
`

// TestTypeParameterMethodCallsLowerToTheConcreteMethod checks the lowering
// directly rather than only through the runtime capability matrix: each
// instantiated body must call the type argument's own method, and none of them
// may route through the constraint interface's method symbol, which has no
// body and would be reached with a receiver that is not an interface value.
func TestTypeParameterMethodCallsLowerToTheConcreteMethod(t *testing.T) {
	module, err := goc.Compile("dispatch.go", []byte(genericDispatchSource))
	require.NoError(t, err)

	tests := []struct {
		name        string
		function    string
		wantCalls   []string
		rejectCalls []string
	}{
		{
			name:        "first instantiation calls its own methods",
			function:    "main.derive[*main.alpha]",
			wantCalls:   []string{"main.alpha.Scale", "main.alpha.Read"},
			rejectCalls: []string{"main.scalable.Scale", "main.scalable.Read", "main.beta.Scale", "main.beta.Read"},
		},
		{
			name:        "second instantiation calls its own methods",
			function:    "main.derive[*main.beta]",
			wantCalls:   []string{"main.beta.Scale", "main.beta.Read"},
			rejectCalls: []string{"main.scalable.Scale", "main.scalable.Read", "main.alpha.Scale", "main.alpha.Read"},
		},
		{
			name:        "value receiver reached through a pointer type argument",
			function:    "main.readPlain[*main.beta]",
			wantCalls:   []string{"main.beta.Plain"},
			rejectCalls: []string{"main.plain.Plain"},
		},
		{
			name:        "generic type argument keeps its own instantiated method",
			function:    "main.count[*main.cell[int]]",
			wantCalls:   []string{"main.cell.Count[int]"},
			rejectCalls: []string{"main.countable.Count", "main.cell.Count[string]"},
		},
		{
			name:        "second generic type argument keeps its own instantiated method",
			function:    "main.count[*main.cell[string]]",
			wantCalls:   []string{"main.cell.Count[string]"},
			rejectCalls: []string{"main.countable.Count", "main.cell.Count[int]"},
		},
		{
			name:        "method promoted from an embedded field",
			function:    "main.readPromoted[*main.narrowWrapper]",
			wantCalls:   []string{"main.embeddedBase.Read"},
			rejectCalls: []string{"main.promoted.Read"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			function := functionNamed(t, module, test.function)
			for _, symbol := range test.wantCalls {
				assert.True(t, callsSymbol(function, symbol), "%s does not call %s", test.function, symbol)
			}
			for _, symbol := range test.rejectCalls {
				assert.False(t, callsSymbol(function, symbol), "%s must not call %s", test.function, symbol)
			}
		})
	}

	// Resolving the call is only half the fix: the concrete method's body has
	// to be reachable too, which for a method on a generic type means the
	// right instantiation of it.
	for _, symbol := range []string{
		"main.alpha.Scale",
		"main.alpha.Read",
		"main.beta.Scale",
		"main.beta.Read",
		"main.beta.Plain",
		"main.cell.Count[int]",
		"main.cell.Count[string]",
	} {
		assert.NotNil(t, findFunctionNamed(module, symbol), "no body was compiled for %s", symbol)
	}
}

// TestOrdinaryInterfaceDispatchIsUnchanged pins the behavior the fix must not
// disturb: a real interface value still dispatches through the synthesized
// interface method wrapper.
func TestOrdinaryInterfaceDispatchIsUnchanged(t *testing.T) {
	module, err := goc.Compile("iface.go", []byte(`package main

type alpha struct{ n int }

type beta struct{ n int }

func (a *alpha) Read() int { return a.n }

func (b *beta) Read() int { return b.n * 2 }

type reader interface {
	Read() int
}

func readAll(values []reader) int {
	total := 0
	for _, value := range values {
		total += value.Read()
	}
	return total
}

func Test() int {
	return readAll([]reader{&alpha{n: 1}, &beta{n: 2}})
}
`))
	require.NoError(t, err)

	caller := functionNamed(t, module, "main.readAll")
	assert.True(t, callsSymbol(caller, "main.reader.Read"), "interface call did not go through the interface wrapper")

	wrapper := findFunctionNamed(module, "main.reader.Read")
	require.NotNil(t, wrapper, "no interface dispatch wrapper was emitted")
	assert.True(t, callsSymbol(wrapper, "main.alpha.Read"), "wrapper does not dispatch to *alpha")
	assert.True(t, callsSymbol(wrapper, "main.beta.Read"), "wrapper does not dispatch to *beta")
}
