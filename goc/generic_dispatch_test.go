package goc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// typeParameterMethodSelectionSource declares one method name on several
// receivers so that name-based resolution cannot accidentally succeed: the
// resolution under test has to come from the type argument bound to the type
// parameter.
const typeParameterMethodSelectionSource = `package p

type alpha struct{ n int }

type beta struct{ n int }

func (a *alpha) Rank() int { return a.n }

func (b *beta) Rank() int { return b.n * 2 }

func (b beta) Plain() int { return b.n }

type ranker interface {
	*alpha | *beta

	Rank() int
}

type plainRanker interface {
	*beta

	Plain() int
}

type ordinary interface {
	Rank() int
}

type wrapper struct {
	lead int
	alpha
}

type embeddedRanker interface {
	*wrapper

	Rank() int
}

func viaTypeParameter[P ranker](value P) int { return value.Rank() }

func viaPlainTypeParameter[P plainRanker](value P) int { return value.Plain() }

func viaEmbedding[P embeddedRanker](value P) int { return value.Rank() }

func viaInterface(value ordinary) int { return value.Rank() }

func viaConcrete(value *alpha) int { return value.Rank() }
`

// checkTypeParameterMethodSelectionSource type-checks the shared snippet and
// returns the method selector in each named function.
func checkTypeParameterMethodSelectionSource(t *testing.T) (*types.Info, *types.Package, map[string]*ast.SelectorExpr) {
	t.Helper()

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "dispatch.go", typeParameterMethodSelectionSource, 0)
	require.NoError(t, err)

	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Instances:  make(map[*ast.Ident]types.Instance),
	}
	var config types.Config
	pkg, err := config.Check("p", fileSet, []*ast.File{file}, info)
	require.NoError(t, err)

	selectors := make(map[string]*ast.SelectorExpr)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				selectors[function.Name.Name] = selector
			}
			return true
		})
	}
	return info, pkg, selectors
}

// typeParameterOf returns the single type parameter of the named function.
func typeParameterOf(t *testing.T, pkg *types.Package, functionName string) *types.TypeParam {
	t.Helper()

	object := pkg.Scope().Lookup(functionName)
	require.NotNil(t, object, "function %s not found", functionName)
	signature := object.Type().(*types.Signature)
	require.Equal(t, 1, signature.TypeParams().Len())
	return signature.TypeParams().At(0)
}

func namedType(t *testing.T, pkg *types.Package, name string) types.Type {
	t.Helper()

	object := pkg.Scope().Lookup(name)
	require.NotNil(t, object, "type %s not found", name)
	return object.Type()
}

func TestTypeParameterMethodSelection(t *testing.T) {
	t.Parallel()
	info, pkg, selectors := checkTypeParameterMethodSelectionSource(t)

	pointerToAlpha := types.NewPointer(namedType(t, pkg, "alpha"))
	pointerToBeta := types.NewPointer(namedType(t, pkg, "beta"))

	tests := []struct {
		name           string
		function       string
		bind           types.Type
		wantResolved   bool
		wantSymbol     string
		wantReceiverIs string
		wantIndexLen   int
	}{
		{
			name:           "type parameter bound to the first union term",
			function:       "viaTypeParameter",
			bind:           pointerToAlpha,
			wantResolved:   true,
			wantSymbol:     "p.alpha.Rank",
			wantReceiverIs: "*p.alpha",
			wantIndexLen:   1,
		},
		{
			name:           "type parameter bound to a later union term",
			function:       "viaTypeParameter",
			bind:           pointerToBeta,
			wantResolved:   true,
			wantSymbol:     "p.beta.Rank",
			wantReceiverIs: "*p.beta",
			wantIndexLen:   1,
		},
		{
			name:           "value receiver reached through a pointer type argument",
			function:       "viaPlainTypeParameter",
			bind:           pointerToBeta,
			wantResolved:   true,
			wantSymbol:     "p.beta.Plain",
			wantReceiverIs: "p.beta",
			wantIndexLen:   1,
		},
		{
			name:           "method promoted from an embedded field",
			function:       "viaEmbedding",
			bind:           types.NewPointer(namedType(t, pkg, "wrapper")),
			wantResolved:   true,
			wantSymbol:     "p.alpha.Rank",
			wantReceiverIs: "*p.alpha",
			// The receiver has to be advanced to the embedded field, so the
			// selection must keep the field index ahead of the method index.
			wantIndexLen: 2,
		},
		{
			name:         "unbound type parameter keeps the shared generic lowering",
			function:     "viaTypeParameter",
			bind:         nil,
			wantResolved: false,
		},
		{
			name:         "ordinary interface receiver is untouched",
			function:     "viaInterface",
			bind:         nil,
			wantResolved: false,
		},
		{
			name:         "concrete receiver is untouched",
			function:     "viaConcrete",
			bind:         nil,
			wantResolved: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := selectors[test.function]
			require.NotNil(t, selector, "no method selector found in %s", test.function)
			method, ok := info.Uses[selector.Sel].(*types.Func)
			require.True(t, ok, "selector target is not a function")

			var substitutions map[*types.TypeParam]types.Type
			if test.bind != nil {
				substitutions = map[*types.TypeParam]types.Type{
					typeParameterOf(t, pkg, test.function): test.bind,
				}
			}

			concreteSelection, resolved := typeParameterMethodSelection(info.Selections[selector], method, substitutions)
			require.Equal(t, test.wantResolved, resolved)
			if !test.wantResolved {
				assert.Nil(t, concreteSelection)
				return
			}
			concreteMethod := concreteSelection.Obj().(*types.Func)
			assert.Equal(t, test.wantSymbol, functionSymbol(concreteMethod))
			assert.Len(t, concreteSelection.Index(), test.wantIndexLen)
			assert.Equal(t, test.bind, concreteSelection.Recv())
			receiverType := concreteMethod.Type().(*types.Signature).Recv().Type()
			assert.Equal(t, test.wantReceiverIs, types.TypeString(receiverType, nil))
			_, receiverIsInterface := receiverType.Underlying().(*types.Interface)
			assert.False(t, receiverIsInterface, "resolved method still has an interface receiver")
		})
	}
}

func TestTypeParameterMethodSelectionIgnoresNonMethodSelections(t *testing.T) {
	t.Parallel()
	info, pkg, selectors := checkTypeParameterMethodSelectionSource(t)
	selector := selectors["viaTypeParameter"]
	require.NotNil(t, selector)
	method := info.Uses[selector.Sel].(*types.Func)
	substitutions := map[*types.TypeParam]types.Type{
		typeParameterOf(t, pkg, "viaTypeParameter"): types.NewPointer(namedType(t, pkg, "alpha")),
	}

	concreteSelection, resolved := typeParameterMethodSelection(nil, method, substitutions)
	assert.False(t, resolved, "a missing selection must not resolve")
	assert.Nil(t, concreteSelection)

	concreteSelection, resolved = typeParameterMethodSelection(info.Selections[selector], nil, substitutions)
	assert.False(t, resolved, "a missing method must not resolve")
	assert.Nil(t, concreteSelection)
}
