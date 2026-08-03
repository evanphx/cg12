package goc_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// An allocation in a loop body needs one object per iteration. Neither of
// cg12's escape analyses asks that question: both ask whether a pointer
// outlives the *frame*, and an allocation can be entirely frame-local by that
// test and still need to be distinct on every trip round the loop. Giving it
// one frame slot makes two objects the source says are different into the same
// object, and the program prints a wrong answer with nothing failing anywhere.
//
// That is an aliasing defect, not an escape one, and it is invisible to every
// other instrument in the tree:
//
//   - opt.FrameEscapes (TestFrameEscapeAudit) is a may-analysis over
//     publications. Nothing is published here -- both pointers stay inside the
//     frame and are simply the same pointer -- so a clean audit says nothing
//     about it.
//
//   - The allocation census (TestAllocationCensus) records where each object
//     went, and one frame slot per loop is exactly what it expects to see. It
//     cannot tell a correct promotion from this one.
//
// So the only thing that catches it is running the program and reading what it
// prints. These are the three reductions from the ccwork/escape-ir-design
// spike, which found the defect and could only fix half of it; they are corpus
// programs here so the compiler's answer is compared against the language's on
// every run.
//
// The expectations are literal rather than computed so this test asserts even
// where no host toolchain is installed.
// TestLoopAliasExpectationsMatchTheHostToolchain proves the literals are the
// host toolchain's own output rather than something written down by hand.
var loopAliasPrograms = []struct {
	source string
	want   string
}{
	// new(int), make([]int, 0, 4) and var a [2]int; &a, each allocated in a
	// loop body with the previous iteration's pointer kept in a local. Every
	// line is "previous current", so a shared allocation prints "2 2".
	{"loop_alias_forms.go", "new:   1 2\nmake:  1 2\narray: 1 2\n"},
	// &cell{v: i}, the same shape through a composite literal, which the front
	// end places by a different route.
	{"loop_alias_composite.go", "alternate: 1 2\ndistinct\n"},
	// A variadic call whose backing array does not outlive the callee. Not a
	// loop; it rides along because it is the third reduction the spike built
	// and the same placement code decides it.
	{"variadic_backing.go", "1\n"},
	// The other half of it: a callee that keeps an *element*. The array belongs
	// in the caller's frame and the boxed payload does not, and the two are
	// separate allocations so that both can be said. If the payload is left in
	// the frame the package-level variable holding it points into a frame that
	// has returned, been written over by a deep recursion and then relocated by
	// a stack copy -- so a wrong answer here is a wrong number rather than a
	// crash, which is why it is a printed value and not an audit.
	{"variadic_element_retention.go", "first  48879\nsecond 51966\nlength true\n"},
	// The same question asked of the front end's AST walk instead of opt's fact
	// table, and it used to get it wrong: parameterDoesNotEscape was consulted
	// at the variadic *parameter* for an argument that is an *element* of it, so
	// a local whose address the callee keeps was left in a frame. The collector
	// finds the resulting pointer and throws; before the walk was taught to
	// refuse the question, this program did not print at all.
	{"variadic_element_address_retention.go", "true true true true\nchurned true\n"},
	// The other direction. Three allocations in loop bodies that are finished
	// with before the iteration ends and so need no more than the one frame
	// slot they already have. A rule that heaped loop-body allocations without
	// asking whether anything retains them would move all three and nothing
	// here would fail -- which is why the program is also in the corpus, where
	// TestAllocationCensus would gain a runtime.newobject line for each.
	{"loop_alias_frame_local.go", "framed:  18\nwithin:  12\nliteral: 18\n"},
}

// TestLoopBodyAllocationsAreDistinctPerIteration runs each reduction and
// compares what goc's program printed against what the language says it
// prints, unoptimized and optimized. Both configurations are run because the
// placement is decided in two different places -- the AST front end commits
// some allocations itself and opt.LowerHeapAllocations decides the rest -- and
// a fix in either one leaves the other's answer unchanged.
func TestLoopBodyAllocationsAreDistinctPerIteration(t *testing.T) {
	for _, program := range loopAliasPrograms {
		for _, optimized := range []bool{false, true} {
			name := program.source
			if optimized {
				name += " -O"
			}
			t.Run(name, func(t *testing.T) {
				got := runCorpusProgramOutput(t, filepath.Join("testdata", program.source), optimized)
				require.Equal(t, program.want, got,
					"goc's answer differs from Go's: an allocation in the loop body is being\n"+
						"shared between iterations, so two objects the source says are distinct are\n"+
						"the same object")
			})
		}
	}
}

// TestLoopAliasExpectationsMatchTheHostToolchain checks the expectations above
// against `go run`. Without it the table would be a record of what someone
// believed Go prints, and a wrong entry would make the corpus test enforce the
// wrong answer forever.
func TestLoopAliasExpectationsMatchTheHostToolchain(t *testing.T) {
	toolchain, err := exec.LookPath("go")
	if err != nil {
		t.Skip("host Go toolchain unavailable")
	}
	for _, program := range loopAliasPrograms {
		t.Run(program.source, func(t *testing.T) {
			command := exec.Command(toolchain, "run", filepath.Join("testdata", program.source))
			output, err := command.CombinedOutput()
			require.NoError(t, err, "go run: %s", output)
			require.Equal(t, program.want, string(output))
		})
	}
}

// runCorpusProgramOutput compiles one corpus program, links it against the
// cg12-compiled Go runtime and returns everything it wrote to stdout and
// stderr. println writes to stderr, so the two are read together.
func runCorpusProgramOutput(t *testing.T, source string, optimized bool) string {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("Go runtime execution test requires Linux ARM64")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("system C linker unavailable")
	}

	program, err := os.ReadFile(source)
	require.NoError(t, err)
	module, err := goc.CompileExecutable(source, program)
	require.NoError(t, err, "compile executable")
	if optimized {
		opt.OptimizeModule(module)
	}

	directory := t.TempDir()
	objectPath := filepath.Join(directory, "program.o")
	objectFile, err := os.Create(objectPath)
	require.NoError(t, err)
	assemblySource, err := arm64.WriteObjectAndAssembly(objectFile, module)
	closeErr := objectFile.Close()
	require.NoError(t, err, "machine code")
	require.NoError(t, closeErr)

	assemblyPath := filepath.Join(directory, "runtime.S")
	require.NoError(t, os.WriteFile(assemblyPath, []byte(assemblySource), 0o644))
	assemblyObjectPath := filepath.Join(directory, "runtime.o")
	assemble := exec.Command(cc, "-c", "-o", assemblyObjectPath, assemblyPath)
	output, err := assemble.CombinedOutput()
	require.NoError(t, err, "assemble runtime: %s", output)

	executable := filepath.Join(directory, "program")
	link := exec.Command(cc, "-no-pie", "-o", executable, objectPath, assemblyObjectPath)
	output, err = link.CombinedOutput()
	require.NoError(t, err, "link executable: %s", output)

	run := exec.Command(executable)
	run.Env = append(os.Environ(), "GOMAXPROCS=1")
	output, err = run.CombinedOutput()
	require.NoError(t, err, "run: %s", output)
	return string(output)
}
