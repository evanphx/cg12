package goc_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A closure that assigned to a captured string variable used to leave the
// enclosing frame's variable addressing the closure's dead frame
// (RUNTIME_PLAN.md 5.10).
//
// These tests pin the generated code rather than the answer, because the answer
// is not reliably wrong: the value survives whenever nothing has reused the
// abandoned frame yet, so a program that reads the variable straight after the
// call passes on the broken compiler.

// capturedHeaderTypes are the three types cg12 keeps behind a pointer in a
// frame slot, which is what made assigning to them from a closure wrong. A
// struct or an array is excluded because it already has stable backing storage,
// and a slice because a slice local is stored inline.
//
// observation reads the variable as an int, so the enclosing function returns
// without boxing anything and the allocation assertion below means what it
// says.
var capturedHeaderTypes = map[string]struct {
	declaration string
	assignment  string
	observation string
}{
	"string": {
		declaration: `text := "a"`,
		assignment:  `text = text + suffix`,
		observation: `len(text)`,
	},
	"interface": {
		declaration: `var text any = 1`,
		assignment:  `text = len(suffix)`,
		observation: `text.(int)`,
	},
	"complex128": {
		declaration: `text := complex(1.0, 2.0)`,
		assignment:  `text = complex(float64(len(suffix)), 4.0)`,
		observation: `int(real(text))`,
	},
}

// The closure writes the value into the variable's storage. Before the fix it
// copied the value into an allocation of its own frame and stored that address
// through the captured pointer, so the assignment published a dead frame to its
// caller.
func TestClosureWritesIntoACapturedVariablesStorage(t *testing.T) {
	for name, shape := range capturedHeaderTypes {
		t.Run(name, func(t *testing.T) {
			module := compileCapturedVariableProgram(t, shape.declaration, shape.assignment, shape.observation)

			closure := functionContaining(t, module, "install.func")
			assert.False(t, storesAnOwnAllocation(closure),
				"the closure stored one of its own frame allocations "+
					"into the captured variable's storage")
		})
	}
}

// The enclosing function keeps the value in the slot the closure environment
// carries, rather than a pointer to it, so the closure's write lands in this
// frame. Sixteen bytes is the width of all three of these values.
func TestCapturedVariableStorageHoldsTheValue(t *testing.T) {
	for name, shape := range capturedHeaderTypes {
		t.Run(name, func(t *testing.T) {
			module := compileCapturedVariableProgram(t, shape.declaration, shape.assignment, shape.observation)

			install := functionContaining(t, module, "main.install")
			assert.True(t, allocatesWidth(install, 16),
				"the captured variable was not given storage of its own value's width")
		})
	}
}

// A variable only a non-escaping closure captures stays in the frame. The fix
// changes its representation, not where it lives, so it must not start
// allocating: RUNTIME_PLAN.md 5.9's cost model depends on an ordinary closure
// being allocation-free.
func TestNonEscapingCaptureStaysOffTheHeap(t *testing.T) {
	for name, shape := range capturedHeaderTypes {
		t.Run(name, func(t *testing.T) {
			module := compileCapturedVariableProgram(t, shape.declaration, shape.assignment, shape.observation)

			install := functionContaining(t, module, "main.install")
			assert.False(t, callsSymbol(install, "runtime.newobject"),
				"a non-escaping closure's capture was heap-lifted")
		})
	}
}

// A range-over-function body is lowered into a yield function, which captures
// the enclosing frame's variables the same way a function literal does. It has
// no function literal in the source at all, which is why it needs its own case.
func TestRangeOverFunctionBodyWritesIntoACapturedVariablesStorage(t *testing.T) {
	module, err := goc.CompileExecutable("closurecapture.go", []byte(`
package main

func counter(yield func(int) bool) {
	for value := 0; value < 3; value++ {
		if !yield(value) {
			return
		}
	}
}

func install() int {
	text := ""
	for value := range counter {
		text = text + string(rune('0'+value))
	}
	return len(text)
}

func main() {
	println(install())
}
`))
	require.NoError(t, err)

	yield := functionContaining(t, module, "rangefunc")
	assert.False(t, storesAnOwnAllocation(yield),
		"the yield function stored one of its own frame allocations "+
			"into the captured variable's storage")
}

func compileCapturedVariableProgram(t *testing.T, declaration, assignment, observation string) *ir.Module {
	t.Helper()
	source := fmt.Sprintf(`
package main

func install(suffix string) int {
	%s
	write := func() {
		%s
	}
	write()
	return %s
}

func main() {
	println(install("z"))
}
`, declaration, assignment, observation)
	module, err := goc.CompileExecutable("closurecapture.go", []byte(source))
	require.NoError(t, err)
	return module
}

// storesAnOwnAllocation reports whether function stores the address of one of
// its own stack allocations through a pointer it did not allocate -- which, in
// a closure whose only such pointer is the captured variable's storage, is
// exactly the defect: the callee's frame is published to its caller.
func storesAnOwnAllocation(function *ir.Func) bool {
	allocations := make(map[uint32]bool)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op.IsAlloc() && instruction.To.Kind == ir.RefTemp {
				allocations[instruction.To.ID] = true
			}
		}
	}
	return containsInstruction(function, func(instruction ir.Instr) bool {
		if !instruction.Op.IsStore() || len(instruction.Args) < 2 {
			return false
		}
		value, destination := instruction.Args[0], instruction.Args[1]
		if value.Kind != ir.RefTemp || !allocations[value.ID] {
			return false
		}
		return destination.Kind == ir.RefTemp && !allocations[destination.ID]
	})
}

func allocatesWidth(function *ir.Func, width int64) bool {
	return containsInstruction(function, func(instruction ir.Instr) bool {
		if !instruction.Op.IsAlloc() || len(instruction.Args) != 1 {
			return false
		}
		size := instruction.Args[0]
		if size.Kind != ir.RefConst {
			return false
		}
		constant := function.Consts[size.ID]
		return constant.Kind == ir.ConstInt && constant.Int == width
	})
}

func functionContaining(t *testing.T, module *ir.Module, fragment string) *ir.Func {
	t.Helper()
	for _, function := range module.Funcs {
		if strings.Contains(function.Name, fragment) {
			return function
		}
	}
	t.Fatalf("function whose name contains %q not found", fragment)
	return nil
}
