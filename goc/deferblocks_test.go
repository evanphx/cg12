package goc_test

import (
	"fmt"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// A function's control-flow graph must stay inside that function. The recovery
// edges a deferred call needs are added from every block that registered a defer
// to the function's deferreturn block, and the list of those blocks used to
// survive into the next function compiled by the same generator -- so the
// previous function gained a synthetic edge into the next one's blocks, and the
// dominance, liveness and frequency analyses built from that graph spanned two
// functions at once.
//
// It stayed invisible while the back end compiled one function at a time: the
// predecessor lists those analyses rebuild are stored on the blocks themselves,
// so each function overwrote the damage the previous one did on its way past.
// Compiling functions concurrently turned it into a data race, which is how it
// was found.
func TestEachFunctionsControlFlowStaysInsideThatFunction(t *testing.T) {
	source := []byte(`package main

func returnBeforeDefer() int {
	result := 0
	defer func() { result++ }()
	return result
}

func updateFloatResult() float64 {
	value := 1.0
	defer func() { value *= 2 }()
	return value
}

func updateForwardedResults() (first int, second int) {
	defer func() { first, second = second, first }()
	return 1, 2
}

func main() {
	first, second := updateForwardedResults()
	println(returnBeforeDefer(), updateFloatResult() > 0, first, second)
}
`)

	module, err := goc.CompileExecutable("defer_blocks.go", source)
	require.NoError(t, err)

	owner := map[*ir.Block]*ir.Func{}
	for _, function := range module.Funcs {
		for _, block := range function.Blocks {
			require.Nilf(t, owner[block],
				"block %s belongs to both %s and %s", block.Name, describeOwner(owner, block), function.Name)
			owner[block] = function
		}
	}

	for _, function := range module.Funcs {
		own := map[*ir.Block]bool{}
		for _, block := range function.Blocks {
			own[block] = true
		}
		for _, block := range function.Blocks {
			successors := append(append([]*ir.Block(nil), block.Succs()...), block.SyntheticSuccs...)
			for _, successor := range successors {
				if successor == nil || own[successor] {
					continue
				}
				require.Failf(t, "control flow leaves the function",
					"%s: block %s has a successor %s owned by %s",
					function.Name, block.Name, successor.Name, describeOwner(owner, successor))
			}
		}
	}
}

func describeOwner(owner map[*ir.Block]*ir.Func, block *ir.Block) string {
	if function, ok := owner[block]; ok {
		return function.Name
	}
	return fmt.Sprintf("no function in the module (%p)", block)
}
