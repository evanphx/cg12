package goc

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

// determinismTestProgram exercises the front-end paths that were reached from a
// map, so that a regression is caught by the module differing rather than by a
// corpus program's bytes moving weeks later.
//
// The variadic calls are the point. A call passing several values that need
// boxing to a `...any` parameter allocates one combined object and computes one
// address per boxed payload, and those address computations were emitted in map
// order; each `add` takes the next temporary number, so a swap renumbers the
// whole rest of the function. Two arguments are enough to see it and three make
// a collision unlikely, so the program calls with several, in more than one
// order, from more than one function.
const determinismTestProgram = `package main

import "runtime"

type point struct{ x, y int }

type pair struct {
	name string
	n    int
}

func describe(values ...any) int {
	total := 0
	for range values {
		total++
	}
	return total
}

func mixed(a point, b pair, c [3]int, d string) int {
	return describe(a, b, c, d) + describe(d, c, b, a) + describe(c, a, d, b)
}

func nested(a point, b pair) int {
	return describe(a, b) + describe(describe(b, a), a, b)
}

func main() {
	runtime.GC()
	println(mixed(point{1, 2}, pair{"b", 3}, [3]int{7, 8, 9}, "plain"))
	println(nested(point{4, 5}, pair{"c", 6}))
}
`

// TestCompilingTheSameSourceTwiceGivesTheSameModule is the front-end half of the
// reproducibility property that TestParallelBackendIsByteIdenticalToSerial
// checks for the back end.
//
// It works because Go randomizes each `range` over a map independently, so two
// compiles in one process draw different traversal orders exactly as two
// processes would. RUNTIME_PLAN.md section 5.10 records what this caught: one
// `...any` call site made `runtime_defer_capture_allocs.go` produce 25 distinct
// executables in 30 compiles.
//
// The comparison is the serialized module, not the printed one: the text form
// omits a datum's relocation base, pointer words and typelink flag, and those
// reach the image too.
func TestCompilingTheSameSourceTwiceGivesTheSameModule(t *testing.T) {
	t.Parallel()
	first, err := CompileExecutableFor(TargetARM64, "determinism.go", []byte(determinismTestProgram))
	require.NoError(t, err)
	second, err := CompileExecutableFor(TargetARM64, "determinism.go", []byte(determinismTestProgram))
	require.NoError(t, err)

	// Named comparisons first, so a failure says which function or datum moved
	// or changed rather than printing two multi-megabyte encodings at each other.
	require.Equal(t, len(first.Funcs), len(second.Funcs), "the two compiles emitted different numbers of functions")
	require.Equal(t, len(first.Data), len(second.Data), "the two compiles emitted different numbers of data symbols")
	for index := range first.Funcs {
		require.Equal(t, first.Funcs[index].Name, second.Funcs[index].Name,
			"function %d is a different function in the two compiles", index)
		require.Equal(t, first.Funcs[index].String(), second.Funcs[index].String(),
			"%s was compiled differently by two compiles of the same source", first.Funcs[index].Name)
	}
	for index := range first.Data {
		require.Equal(t, first.Data[index].Name, second.Data[index].Name,
			"datum %d is a different datum in the two compiles", index)
	}

	firstBytes, err := first.MarshalBinary()
	require.NoError(t, err)
	secondBytes, err := second.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(firstBytes), sha256.Sum256(secondBytes),
		"two compiles of the same source produced different modules, and it is not a function body or a symbol order")
}
