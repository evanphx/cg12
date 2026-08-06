package goc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/ir"
)

// TestGeneratedPackageLiteralsGetDistinctSymbols compiles the one place in the
// vendored standard library where two function literals of the same package sit
// at the same line and column.
//
// crypto/internal/fips140/nistec's p224.go, p384.go and p521.go are generated
// from one template. Naming a literal by its package and position gave all three
// sync.Once.Do closures the symbol crypto/internal/fips140/nistec.func.114.16,
// and obj.prepareELF keeps the last definition of a name -- so p224B would have
// called p521B's initializer. Compiling this program is the regression test:
// compile refuses a module whose functions do not have distinct linker symbols.
func TestGeneratedPackageLiteralsGetDistinctSymbols(t *testing.T) {
	t.Parallel()
	source := `package main

import "crypto/internal/fips140/nistec"

func main() {
	nistec.NewP224Point().ScalarBaseMult(make([]byte, 28))
	nistec.NewP384Point().ScalarBaseMult(make([]byte, 48))
	nistec.NewP521Point().ScalarBaseMult(make([]byte, 66))
}
`
	module, err := compile("nistec_literals.go", []byte(source), compileOptions{
		target:     TargetARM64,
		executable: true,
	})
	require.NoError(t, err)

	literals := map[string]int{}
	for _, function := range module.Funcs {
		literals[function.Name]++
	}
	assert.Equal(t, 1, literals["crypto/internal/fips140/nistec.p224B.func.114.16"],
		"p224B's Once.Do closure should have a symbol of its own")
	assert.Equal(t, 1, literals["crypto/internal/fips140/nistec.p384B.func.114.16"],
		"p384B's Once.Do closure should have a symbol of its own")
	assert.Equal(t, 1, literals["crypto/internal/fips140/nistec.p521B.func.114.16"],
		"p521B's Once.Do closure should have a symbol of its own")
}

// TestCheckUniqueFunctionSymbolsNamesBothFunctions asserts the collision is
// reported rather than silently resolved, and names what collided with what.
func TestCheckUniqueFunctionSymbolsNamesBothFunctions(t *testing.T) {
	t.Parallel()
	module := &ir.Module{}
	module.NewFuncVoid("pkg.one.func.3.4")
	module.NewFuncVoid("pkg/one.func.3.4")

	err := checkUniqueFunctionSymbols(module)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pkg.one.func.3.4")
	assert.Contains(t, err.Error(), "pkg/one.func.3.4")
	assert.Contains(t, err.Error(), "pkg_one_func_3_4")
}
