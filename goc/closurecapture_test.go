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
			assert.False(t, publishesAnOwnAllocation(closure),
				"the closure published one of its own frame allocations "+
					"through the captured variable's storage")
		})
	}
}

// The storage the enclosing function hands to the closure environment is the
// value, not a pointer to it. Sixteen bytes is the width of all three of these
// values; eight is the pointer slot the defect handed over instead.
func TestCapturedVariableStorageHoldsTheValue(t *testing.T) {
	for name, shape := range capturedHeaderTypes {
		t.Run(name, func(t *testing.T) {
			module := compileCapturedVariableProgram(t, shape.declaration, shape.assignment, shape.observation)

			install := functionContaining(t, module, "main.install")
			widths := capturedStorageWidths(install)
			require.NotEmpty(t, widths, "nothing was written into a closure environment")
			for _, width := range widths {
				assert.Equal(t, int64(16), width,
					"the closure environment was handed a pointer to the value "+
						"rather than the value's own storage")
			}
		})
	}
}

// A variable only a non-escaping closure captures stays in the frame. The fix
// changes its representation, not where it lives, so it must not start
// allocating: RUNTIME_PLAN.md 5.9's cost model depends on an ordinary closure
// being allocation-free.
//
// The interface shape is not here because assigning a concrete value to an
// interface boxes it, which allocates for a reason that has nothing to do with
// capture, and the assertion could not tell the two apart.
func TestNonEscapingCaptureStaysOffTheHeap(t *testing.T) {
	for _, name := range []string{"string", "complex128"} {
		shape := capturedHeaderTypes[name]
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
	assert.False(t, publishesAnOwnAllocation(yield),
		"the yield function published one of its own frame allocations "+
			"through the captured variable's storage")
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

// publishesAnOwnAllocation reports whether function writes the address of one
// of its own stack allocations through a pointer it did not allocate. In a
// closure, the only such pointer is the captured variable's storage, so this is
// exactly the defect: the callee hands its own frame to its caller.
//
// Both forms of write count. A pointer store lowers to a goc_storep call
// whenever the destination is not a known stack address, and the captured
// pointer never is -- checking only ir.Instr.Op.IsStore() made this test pass
// against the compiler it was written to catch.
func publishesAnOwnAllocation(function *ir.Func) bool {
	allocations := ownAllocationSizes(function)
	published := func(value, destination ir.Ref) bool {
		if value.Kind != ir.RefTemp {
			return false
		}
		if _, allocated := allocations[value.ID]; !allocated {
			return false
		}
		if destination.Kind != ir.RefTemp {
			return true
		}
		_, destinationAllocated := allocations[destination.ID]
		return !destinationAllocated
	}
	return containsInstruction(function, func(instruction ir.Instr) bool {
		if instruction.Op.IsStore() && len(instruction.Args) >= 2 {
			return published(instruction.Args[0], instruction.Args[1])
		}
		if instruction.Op == ir.OCall && len(instruction.Args) == 3 &&
			symbolName(function, instruction.Args[0]) == "goc_storep" {
			return published(instruction.Args[2], instruction.Args[1])
		}
		return false
	})
}

// capturedStorageWidths reports the sizes of the allocations whose addresses
// the function writes into a closure environment -- the stores into a
// descriptor's capture words, which are offsets from the descriptor's own
// allocation.
func capturedStorageWidths(function *ir.Func) []int64 {
	allocations := ownAllocationSizes(function)
	offsets := make(map[uint32]bool)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op != ir.OAdd || len(instruction.Args) != 2 {
				continue
			}
			base := instruction.Args[0]
			if base.Kind != ir.RefTemp {
				continue
			}
			if _, allocated := allocations[base.ID]; allocated {
				offsets[instruction.To.ID] = true
			}
		}
	}
	var widths []int64
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if !instruction.Op.IsStore() || len(instruction.Args) < 2 {
				continue
			}
			value, destination := instruction.Args[0], instruction.Args[1]
			if value.Kind != ir.RefTemp || destination.Kind != ir.RefTemp {
				continue
			}
			if !offsets[destination.ID] {
				continue
			}
			if size, allocated := allocations[value.ID]; allocated {
				widths = append(widths, size)
			}
		}
	}
	return widths
}

func ownAllocationSizes(function *ir.Func) map[uint32]int64 {
	allocations := make(map[uint32]int64)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if !instruction.Op.IsAlloc() || instruction.To.Kind != ir.RefTemp {
				continue
			}
			if len(instruction.Args) != 1 || instruction.Args[0].Kind != ir.RefConst {
				continue
			}
			constant := function.Consts[instruction.Args[0].ID]
			if constant.Kind != ir.ConstInt {
				continue
			}
			allocations[instruction.To.ID] = constant.Int
		}
	}
	return allocations
}

func symbolName(function *ir.Func, reference ir.Ref) string {
	if reference.Kind != ir.RefConst {
		return ""
	}
	constant := function.Consts[reference.ID]
	if constant.Kind != ir.ConstSym {
		return ""
	}
	return constant.Sym
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
