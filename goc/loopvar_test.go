package goc_test

import (
	"testing"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Go 1.22 gives a loop-declared variable one instance per iteration. cg12
// reproduces that by allocating the variable's cell inside the loop, so the
// allocation site sits in a block with a non-zero loop depth. A loop whose
// variable never leaves the iteration keeps a single frame slot and allocates
// nothing, which is what keeps ordinary loops -- including every loop in the
// runtime -- free of per-iteration allocation.
func TestCapturedLoopVariableIsAllocatedInsideTheLoop(t *testing.T) {
	sources := map[string]string{
		"three-clause for": `
	for index := 0; index < 3; index++ {
		callbacks = append(callbacks, func() int { return index })
	}
`,
		"range over int": `
	for index := range 3 {
		callbacks = append(callbacks, func() int { return index })
	}
`,
		"range over slice": `
	for index, value := range values {
		callbacks = append(callbacks, func() int { return index + value })
	}
`,
		"range over map": `
	for key, value := range counts {
		callbacks = append(callbacks, func() int { return len(key) + value })
	}
`,
		"range over channel": `
	for value := range stream {
		callbacks = append(callbacks, func() int { return value })
	}
`,
		"address of the loop variable": `
	for index := 0; index < 3; index++ {
		pointers = append(pointers, &index)
	}
`,
	}

	for name, loop := range sources {
		t.Run(name, func(t *testing.T) {
			module, err := goc.Compile("loopvar.go", []byte(loopVariableProgram(loop)))
			require.NoError(t, err)

			install := functionWithSuffix(t, module, "install")
			allocations := loopAllocationDepths(install, "runtime.newobject")
			require.NotEmpty(t, allocations, "the captured loop variable was never heap-lifted")
			assert.Greater(t, maximum(allocations), 0,
				"every heap allocation sits outside the loop, so all iterations share one cell")
		})
	}
}

func TestUncapturedLoopVariableAllocatesNothing(t *testing.T) {
	sources := map[string]string{
		"three-clause for": `
	for index := 0; index < 3; index++ {
		total += index
	}
`,
		"range over slice": `
	for index, value := range values {
		total += index + value
	}
`,
		"range over map": `
	for key, value := range counts {
		total += len(key) + value
	}
`,
		"range over channel": `
	for value := range stream {
		total += value
	}
`,
		"synchronously called literal": `
	for index := 0; index < 3; index++ {
		func() { total += index }()
	}
`,
		"address that does not escape": `
	for index := 0; index < 3; index++ {
		pointer := &index
		total += *pointer
	}
`,
	}

	for name, loop := range sources {
		t.Run(name, func(t *testing.T) {
			module, err := goc.Compile("loopvar.go", []byte(loopVariableProgram(loop)))
			require.NoError(t, err)

			install := functionWithSuffix(t, module, "install")
			assert.Equal(t, 0, countCallsSymbol(install, "runtime.newobject"),
				"a loop variable that cannot outlive its iteration was still allocated per iteration")
		})
	}
}

// `for k, v = range x` assigns to variables that already exist rather than
// declaring new ones, so those variables keep one instance for the whole loop
// even when a closure captures them.
func TestAssigningRangeClauseKeepsOneInstance(t *testing.T) {
	module, err := goc.Compile("loopvar.go", []byte(loopVariableProgram(`
	var index, value int
	for index, value = range values {
		callbacks = append(callbacks, func() int { return index + value })
	}
`)))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	inside, outside := countLoopAllocations(install, "runtime.newobject")
	// The two captured variables are heap-lifted once, ahead of the loop. The
	// only allocation left inside the loop is the escaping closure descriptor,
	// which the language does require to be fresh per iteration.
	assert.GreaterOrEqual(t, outside, 2, "the captured variables were not heap-lifted ahead of the loop")
	assert.Equal(t, 1, inside, "an assigning range clause was given per-iteration storage")
}

// A per-iteration cell must not be a heap-allocation candidate that
// opt.LowerHeapAllocations can promote to a frame slot. That pass decides
// whether a pointer outlives the frame, not whether it outlives one iteration,
// so a promoted cell would put every iteration back on one slot.
func TestPerIterationCellSurvivesHeapAllocationLowering(t *testing.T) {
	module, err := goc.Compile("loopvar.go", []byte(loopVariableProgram(`
	var local [3]func() int
	for index := 0; index < 3; index++ {
		local[index] = func() int { return index }
	}
	callbacks = append(callbacks, local[0], local[1], local[2])
`)))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	allocations := loopAllocationDepths(install, "runtime.newobject")
	require.NotEmpty(t, allocations, "the per-iteration cell was promoted to a frame slot")
	assert.Greater(t, maximum(allocations), 0,
		"the per-iteration cell was hoisted out of the loop")
}

// One captured variable needs one cell per iteration, not several. This pins
// the cost of the transformation: a loop that captures a single scalar pays a
// single allocation per iteration.
func TestOneCapturedScalarCostsOneAllocationPerIteration(t *testing.T) {
	module, err := goc.Compile("loopvar.go", []byte(loopVariableProgram(`
	for index := 0; index < 3; index++ {
		callbacks = append(callbacks, func() int { return index })
	}
`)))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	inside, _ := countLoopAllocations(install, "runtime.newobject")
	// The loop variable's cell plus the escaping closure descriptor, which the
	// language requires to be fresh per iteration as well.
	assert.Equal(t, 2, inside,
		"per-iteration lowering emitted more allocation sites than the variable and its closure")
}

// countLoopAllocations splits the calls to symbol into those inside a loop and
// those that run once per call to the function.
func countLoopAllocations(function *ir.Func, symbol string) (inside, outside int) {
	for _, depth := range loopAllocationDepths(function, symbol) {
		if depth > 0 {
			inside++
			continue
		}
		outside++
	}
	return inside, outside
}

func loopVariableProgram(loop string) string {
	return `
package main

import "runtime"

var callbacks []func() int
var pointers []*int
var total int

func install(values []int, counts map[string]int, stream chan int) {
` + loop + `}

func Test() {
	runtime.GC()
	install(nil, nil, nil)
}
`
}

// loopAllocationDepths returns the loop nesting depth of every block that calls
// symbol, so a caller can tell an allocation inside a loop from one that runs
// once per call.
func loopAllocationDepths(function *ir.Func, symbol string) []int {
	cfg := analysis.BuildCFG(function)
	depths := cfg.LoopDepth(cfg.Dominators())
	var found []int
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
				continue
			}
			callee := instruction.Args[0]
			if callee.Kind != ir.RefConst {
				continue
			}
			constant := function.Consts[callee.ID]
			if constant.Kind == ir.ConstSym && constant.Sym == symbol {
				found = append(found, depths[block])
			}
		}
	}
	return found
}

func maximum(values []int) int {
	highest := 0
	for _, value := range values {
		if value > highest {
			highest = value
		}
	}
	return highest
}
