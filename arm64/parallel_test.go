package arm64_test

import (
	"fmt"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// buildParallelBackendModule builds a module wide enough that the back end's
// workers genuinely overlap, and shaped so that the parts of an object whose
// layout depends on emission order are all exercised: text addresses and sizes,
// intra-module call relocations, DWARF line rows and subprograms, and -- through
// deliberately high register pressure -- spill slots, which is where the
// allocator's slot colouring runs.
func buildParallelBackendModule(functionCount, pressure int) *ir.Module {
	m := ir.NewModule()
	m.Files = []string{"parallel_test.go"}

	leaf := m.NewFunc("pbleaf", ir.ClsW)
	leafArgument := leaf.Param("x", ir.ClsW)
	leafEntry := leaf.Entry()
	leafEntry.Ret(leafEntry.Add(ir.ClsW, leafArgument, leaf.Word(1)))

	for index := 0; index < functionCount; index++ {
		f := m.NewFunc(fmt.Sprintf("pbfunc%d", index), ir.ClsW).Export()
		n := f.Param("n", ir.ClsW)
		entry := f.Entry()
		loop := f.NewBlock(fmt.Sprintf("loop%d", index))
		body := f.NewBlock(fmt.Sprintf("body%d", index))
		exit := f.NewBlock(fmt.Sprintf("exit%d", index))

		// A wide set of values defined before the loop and used after it: every one
		// is live across the loop's calls, so the allocator has to spill most of them
		// and then colour their slots.
		carried := make([]ir.Ref, 0, pressure+index%7)
		for value := 0; value < pressure+index%7; value++ {
			carried = append(carried, entry.Mul(ir.ClsW, n, f.Word(int64(value+1))))
		}
		entry.Goto(loop)

		counter := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
		accumulator := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
		done := loop.Cmp(ir.CmpSge, ir.ClsW, counter, n)
		loop.Jnz(done, exit, body)

		stepped := body.Call(ir.ClsW, f.Sym("pbleaf", 0), counter)
		nextAccumulator := body.Add(ir.ClsW, accumulator, stepped)
		nextCounter := body.Add(ir.ClsW, counter, f.Word(1))
		body.Goto(loop)
		loop.Phis[0].Add(body, nextCounter)
		loop.Phis[1].Add(body, nextAccumulator)

		total := accumulator
		for _, value := range carried {
			total = exit.Add(ir.ClsW, total, value)
		}
		exit.Ret(total)
	}
	return m
}

// TestParallelBackendIsByteIdenticalToSerial is the property the concurrent back
// end rests on: the worker count is a throughput knob and nothing else. If any
// symbol, address or relocation were derived from the order the workers finish
// in, these objects would differ.
func TestParallelBackendIsByteIdenticalToSerial(t *testing.T) {
	module := buildParallelBackendModule(64, 24)

	t.Setenv("GOC_BACKEND_WORKERS", "1")
	serial, err := arm64.CompileToObject(module)
	require.NoError(t, err)
	serialELF, err := serial.MarshalELF()
	require.NoError(t, err)

	for _, workers := range []string{"1", "2", "3", "8", "64", "256"} {
		t.Run("workers="+workers, func(t *testing.T) {
			t.Setenv("GOC_BACKEND_WORKERS", workers)
			// A fresh module: compiling lowers the IR in place, so the same module
			// cannot be compiled twice.
			parallel, err := arm64.CompileToObject(buildParallelBackendModule(64, 24))
			require.NoError(t, err)
			parallelELF, err := parallel.MarshalELF()
			require.NoError(t, err)
			require.Equal(t, len(serialELF), len(parallelELF), "object size differs at %s workers", workers)
			require.True(t, string(serialELF) == string(parallelELF), "object differs at %s workers", workers)
		})
	}
}

// TestParallelBackendReportsTheFirstFailureInFunctionOrder pins the error a
// concurrent compile reports. Workers finish in an arbitrary order, so without an
// explicit rule the reported failure would vary between runs of the same input.
func TestParallelBackendReportsTheFirstFailureInFunctionOrder(t *testing.T) {
	build := func() *ir.Module {
		m := ir.NewModule()
		for index := 0; index < 32; index++ {
			f := m.NewFunc(fmt.Sprintf("pbok%d", index), ir.ClsW).Export()
			entry := f.Entry()
			entry.Ret(f.Word(int64(index)))
		}
		// Two functions the back end refuses, so there is a choice of which failure
		// to report: lowering pins a target's registers into a function, and these
		// two are already lowered for another one.
		for _, name := range []string{"pbbad_first", "pbbad_second"} {
			f := m.NewFunc(name, ir.ClsW).Export()
			entry := f.Entry()
			entry.Ret(f.Word(0))
			require.NoError(t, f.MarkLowered("amd64"))
		}
		return m
	}

	t.Setenv("GOC_BACKEND_WORKERS", "1")
	_, serialErr := arm64.CompileToObject(build())
	require.Error(t, serialErr)

	for _, workers := range []string{"2", "8", "64"} {
		t.Run("workers="+workers, func(t *testing.T) {
			t.Setenv("GOC_BACKEND_WORKERS", workers)
			_, parallelErr := arm64.CompileToObject(build())
			require.Error(t, parallelErr)
			require.Equal(t, serialErr.Error(), parallelErr.Error())
		})
	}
}

// TestParallelBackendCompilesHighPressureFunctionCorrectly runs code from the
// concurrent back end rather than only comparing it. The function spills heavily,
// which is what drives the allocator's slot colouring.
func TestParallelBackendCompilesHighPressureFunctionCorrectly(t *testing.T) {
	t.Setenv("GOC_BACKEND_WORKERS", "8")
	module := buildParallelBackendModule(4, 40)
	// sum(i for i in 0..n-1 of i+1) + n*(1+2+...+40) for pbfunc0 (index%7 == 0).
	const n = 10
	expected := 0
	for i := 0; i < n; i++ {
		expected += i + 1
	}
	for value := 1; value <= 40; value++ {
		expected += n * value
	}
	output, code := buildAndRun(t, module, fmt.Sprintf(`
#include <stdio.h>
extern int pbfunc0(int);
int main(void){ printf("%%d\n", pbfunc0(%d)); return 0; }`, n))
	require.Equal(t, 0, code)
	require.Equal(t, fmt.Sprintf("%d\n", expected), output)
}
