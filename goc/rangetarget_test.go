package goc_test

import (
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A `range` clause that assigns with `=` accepts any assignable operand, and
// the lowering used to resolve each side to a variable object and discard
// everything it could not name. These tests pin the shape of the generated
// code rather than only the answer, because the defect was invisible in the
// control flow: the loop iterated the right number of times and simply never
// stored.

// A package-level target is written through its symbol. Before the fix the
// clause handed the global to variableStorage, which gave it a fresh frame
// slot, so every iteration's assignment landed somewhere no other function
// could see.
func TestRangeClauseWritesAPackageLevelTarget(t *testing.T) {
	t.Parallel()
	loops := map[string]string{
		"slice":    "for target = range values {\n\t_ = target\n}",
		"array":    "for target = range [3]int{1, 2, 3} {\n\t_ = target\n}",
		"string":   "for target = range \"abc\" {\n\t_ = target\n}",
		"integer":  "for target = range 3 {\n\t_ = target\n}",
		"map":      "for target = range weights {\n\t_ = target\n}",
		"channel":  "for target = range stream {\n\t_ = target\n}",
		"iterator": "for target = range doubles {\n\t_ = target\n}",
	}

	for name, loop := range loops {
		t.Run(name, func(t *testing.T) {
			module, err := goc.Compile("rangetarget.go", []byte(rangeTargetProgram(loop)))
			require.NoError(t, err)

			// A range-over-function body is lowered into a yield function of its
			// own, so the store does not always land in install itself.
			assert.True(t, moduleStoresToSymbol(module, "main.target"),
				"the clause never wrote the package-level variable's symbol")
		})
	}
}

// The operands a target's address depends on are evaluated once per iteration,
// so a call in an index expression runs as many times as the loop body does.
func TestRangeTargetOperandsAreEvaluatedEveryIteration(t *testing.T) {
	t.Parallel()
	module, err := goc.Compile("rangetarget.go", []byte(rangeTargetProgram(`
	destination := make([]int, 1)
	for destination[position()] = range values {
		_ = destination
	}
`)))
	require.NoError(t, err)

	install := functionWithSuffix(t, module, "install")
	calls := loopAllocationDepths(install, "main.position")
	require.NotEmpty(t, calls, "the target's index operand was never evaluated")
	assert.Greater(t, maximum(calls), 0,
		"the target's index operand was hoisted out of the loop")
}

// Every destination that names a map element is written through the map
// runtime. Taking its address the way an index into a slice is taken writes
// through a value the map never handed out, which corrupts unrelated memory.
//
// The three statements share one compilation because they need the allocating
// runtime configuration goc itself uses, where the map operations are runtime
// calls rather than open-coded probing.
func TestMapElementDestinationsUseTheMapRuntime(t *testing.T) {
	t.Parallel()
	module, err := goc.CompileExecutable("maptarget.go", []byte(`
package main

var target int

func rangeIntoMapElement(values []int, counts map[string]int) {
	for counts["k"] = range values {
		_ = counts
	}
}

func tupleIntoMapElement(counts map[string]int) {
	target, counts["k"] = 4, 5
}

func bumpMapElement(counts map[string]int) {
	counts["k"] += 5
}

func main() {
	rangeIntoMapElement(nil, nil)
	tupleIntoMapElement(nil)
	bumpMapElement(nil)
}
`))
	require.NoError(t, err)

	rangeIntoMapElement := functionWithSuffix(t, module, "rangeIntoMapElement")
	assignments := loopAllocationDepths(rangeIntoMapElement, "runtime.mapassign")
	require.NotEmpty(t, assignments, "a map element range target was not assigned at all")
	assert.Greater(t, maximum(assignments), 0,
		"the map element was assigned outside the loop rather than once per iteration")

	tupleIntoMapElement := functionWithSuffix(t, module, "tupleIntoMapElement")
	assert.Equal(t, 1, countCallsSymbol(tupleIntoMapElement, "runtime.mapassign"),
		"a map element on the left of a tuple assignment did not go through the map runtime")

	// An assignment operator has to read the element before it stores the
	// combined value; the single-assignment path used to drop the operator and
	// store only the right operand.
	bumpMapElement := functionWithSuffix(t, module, "bumpMapElement")
	assert.Equal(t, 1, countCallsSymbol(bumpMapElement, "runtime.mapaccess2"),
		"an assignment operator on a map element never read the element")
	assert.Equal(t, 1, countCallsSymbol(bumpMapElement, "runtime.mapassign"),
		"an assignment operator on a map element never stored the result")
}

// moduleStoresToSymbol reports whether any function in the module stores to the
// named symbol itself, which is how a package-level variable is written.
func moduleStoresToSymbol(module *ir.Module, symbol string) bool {
	for _, function := range module.Funcs {
		if storesToSymbol(function, symbol) {
			return true
		}
	}
	return false
}

func storesToSymbol(function *ir.Func, symbol string) bool {
	return containsInstruction(function, func(instruction ir.Instr) bool {
		if !instruction.Op.IsStore() || len(instruction.Args) < 2 {
			return false
		}
		destination := instruction.Args[1]
		if destination.Kind != ir.RefConst {
			return false
		}
		constant := function.Consts[destination.ID]
		return constant.Kind == ir.ConstSym && constant.Sym == symbol
	})
}

func rangeTargetProgram(body string) string {
	return `
package main

var target int

func position() int {
	return 0
}

func doubles(yield func(int) bool) {
	for index := 0; index < 3; index++ {
		if !yield(index * 2) {
			return
		}
	}
}

func install(values []int, counts map[string]int, weights map[int]int, stream chan int) {
` + body + `}

func Test() {
	install(nil, nil, nil, nil)
}
`
}
