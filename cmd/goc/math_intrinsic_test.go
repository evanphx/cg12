package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The math functions AArch64 computes in one instruction must reach that
// instruction rather than a call. Two things have to hold and neither implies
// the other:
//
//   - the call is gone (TestARM64MathCallsLowerToInstructions), which is the
//     whole point of the change and is what makes math.Sqrt cost 2 ns instead
//     of 400;
//   - the answer is still the specified answer at every edge Go names
//     (TestARM64MathIntrinsicEdgeCasesExecute), because a fast wrong square
//     root is worse than a slow right one.
//
// The edge cases are compared on the bits. A comparison on values would accept
// +0 where -0 is specified, and would accept anything at all for a NaN.

// mathIntrinsicProgram exercises every documented special case of the lowered
// functions and reports the first that is wrong. It is written to be run by the
// goc-built binary, so it prints rather than using the testing package.
const mathIntrinsicProgram = `package main

import (
	"fmt"
	"math"
	"os"
)

const (
	positiveZero     = 0x0000000000000000
	negativeZero     = 0x8000000000000000
	positiveInfinity = 0x7FF0000000000000
	negativeInfinity = 0xFFF0000000000000
	// A quiet NaN. Which quiet NaN a function returns is not specified, so the
	// checks below ask only whether the result is a NaN.
	quietNaN = 0x7FF8000000000001
)

// isNaN reports NaN-ness from the bits, so it says the same thing about every
// payload and cannot be folded away by a comparison the way x != x can.
func isNaN(bits uint64) bool {
	return bits&0x7FF0000000000000 == 0x7FF0000000000000 && bits&0x000FFFFFFFFFFFFF != 0
}

var failures int

// exact checks the result bit for bit, which is how -0 is told from +0.
func exact(name string, got, want uint64) {
	if got != want {
		fmt.Printf("%s = %#016x, want %#016x\n", name, got, want)
		failures++
	}
}

// nan checks only that the result is a NaN, which is all Go specifies.
func nan(name string, got uint64) {
	if !isNaN(got) {
		fmt.Printf("%s = %#016x, want a NaN\n", name, got)
		failures++
	}
}

func bits(x float64) uint64      { return math.Float64bits(x) }
func value(b uint64) float64     { return math.Float64frombits(b) }

func main() {
	// Sqrt(+Inf) = +Inf, Sqrt(+-0) = +-0, Sqrt(x < 0) = NaN, Sqrt(NaN) = NaN.
	exact("Sqrt(4)", bits(math.Sqrt(4)), bits(2))
	exact("Sqrt(+0)", bits(math.Sqrt(value(positiveZero))), positiveZero)
	exact("Sqrt(-0)", bits(math.Sqrt(value(negativeZero))), negativeZero)
	exact("Sqrt(+Inf)", bits(math.Sqrt(value(positiveInfinity))), positiveInfinity)
	nan("Sqrt(-1)", bits(math.Sqrt(-1)))
	nan("Sqrt(-Inf)", bits(math.Sqrt(value(negativeInfinity))))
	nan("Sqrt(-MaxFloat64)", bits(math.Sqrt(-math.MaxFloat64)))
	nan("Sqrt(-SmallestNonzeroFloat64)", bits(math.Sqrt(-math.SmallestNonzeroFloat64)))
	nan("Sqrt(NaN)", bits(math.Sqrt(value(quietNaN))))
	exact("Sqrt(MaxFloat64)", bits(math.Sqrt(math.MaxFloat64)), bits(1.3407807929942596e154))
	exact("Sqrt(SmallestNonzeroFloat64)", bits(math.Sqrt(math.SmallestNonzeroFloat64)), bits(2.2227587494850775e-162))

	// Abs(+-Inf) = +Inf, Abs(NaN) = NaN, and -0 becomes +0.
	exact("Abs(-3)", bits(math.Abs(-3)), bits(3))
	exact("Abs(-0)", bits(math.Abs(value(negativeZero))), positiveZero)
	exact("Abs(+0)", bits(math.Abs(value(positiveZero))), positiveZero)
	exact("Abs(-Inf)", bits(math.Abs(value(negativeInfinity))), positiveInfinity)
	exact("Abs(+Inf)", bits(math.Abs(value(positiveInfinity))), positiveInfinity)
	exact("Abs(-MaxFloat64)", bits(math.Abs(-math.MaxFloat64)), bits(math.MaxFloat64))
	nan("Abs(NaN)", bits(math.Abs(value(quietNaN))))

	// Floor: toward -Inf. A fraction just above zero floors to +0; one just
	// below floors to -1.
	exact("Floor(1.5)", bits(math.Floor(1.5)), bits(1))
	exact("Floor(-1.5)", bits(math.Floor(-1.5)), bits(-2))
	exact("Floor(0.5)", bits(math.Floor(0.5)), positiveZero)
	exact("Floor(-0.5)", bits(math.Floor(-0.5)), bits(-1))
	exact("Floor(+0)", bits(math.Floor(value(positiveZero))), positiveZero)
	exact("Floor(-0)", bits(math.Floor(value(negativeZero))), negativeZero)
	exact("Floor(+Inf)", bits(math.Floor(value(positiveInfinity))), positiveInfinity)
	exact("Floor(-Inf)", bits(math.Floor(value(negativeInfinity))), negativeInfinity)
	nan("Floor(NaN)", bits(math.Floor(value(quietNaN))))

	// Ceil: toward +Inf. A fraction just below zero ceils to -0, which is the
	// case a comparison on values could not see.
	exact("Ceil(1.5)", bits(math.Ceil(1.5)), bits(2))
	exact("Ceil(-1.5)", bits(math.Ceil(-1.5)), bits(-1))
	exact("Ceil(0.5)", bits(math.Ceil(0.5)), bits(1))
	exact("Ceil(-0.5)", bits(math.Ceil(-0.5)), negativeZero)
	exact("Ceil(+0)", bits(math.Ceil(value(positiveZero))), positiveZero)
	exact("Ceil(-0)", bits(math.Ceil(value(negativeZero))), negativeZero)
	exact("Ceil(+Inf)", bits(math.Ceil(value(positiveInfinity))), positiveInfinity)
	exact("Ceil(-Inf)", bits(math.Ceil(value(negativeInfinity))), negativeInfinity)
	nan("Ceil(NaN)", bits(math.Ceil(value(quietNaN))))

	// Trunc: toward zero, so a fraction keeps its sign in the zero.
	exact("Trunc(1.5)", bits(math.Trunc(1.5)), bits(1))
	exact("Trunc(-1.5)", bits(math.Trunc(-1.5)), bits(-1))
	exact("Trunc(0.5)", bits(math.Trunc(0.5)), positiveZero)
	exact("Trunc(-0.5)", bits(math.Trunc(-0.5)), negativeZero)
	exact("Trunc(+0)", bits(math.Trunc(value(positiveZero))), positiveZero)
	exact("Trunc(-0)", bits(math.Trunc(value(negativeZero))), negativeZero)
	exact("Trunc(+Inf)", bits(math.Trunc(value(positiveInfinity))), positiveInfinity)
	exact("Trunc(-Inf)", bits(math.Trunc(value(negativeInfinity))), negativeInfinity)
	nan("Trunc(NaN)", bits(math.Trunc(value(quietNaN))))

	// RoundToEven: ties to even.
	exact("RoundToEven(0.5)", bits(math.RoundToEven(0.5)), positiveZero)
	exact("RoundToEven(-0.5)", bits(math.RoundToEven(-0.5)), negativeZero)
	exact("RoundToEven(1.5)", bits(math.RoundToEven(1.5)), bits(2))
	exact("RoundToEven(2.5)", bits(math.RoundToEven(2.5)), bits(2))
	exact("RoundToEven(-2.5)", bits(math.RoundToEven(-2.5)), bits(-2))
	exact("RoundToEven(3.5)", bits(math.RoundToEven(3.5)), bits(4))
	exact("RoundToEven(0.49999999999999994)", bits(math.RoundToEven(0.49999999999999994)), positiveZero)
	exact("RoundToEven(4503599627370495.5)", bits(math.RoundToEven(4503599627370495.5)), bits(4503599627370496))
	exact("RoundToEven(+0)", bits(math.RoundToEven(value(positiveZero))), positiveZero)
	exact("RoundToEven(-0)", bits(math.RoundToEven(value(negativeZero))), negativeZero)
	exact("RoundToEven(+Inf)", bits(math.RoundToEven(value(positiveInfinity))), positiveInfinity)
	exact("RoundToEven(-Inf)", bits(math.RoundToEven(value(negativeInfinity))), negativeInfinity)
	nan("RoundToEven(NaN)", bits(math.RoundToEven(value(quietNaN))))

	// Round: ties away from zero, which is the only place it parts company
	// with RoundToEven.
	exact("Round(0.5)", bits(math.Round(0.5)), bits(1))
	exact("Round(-0.5)", bits(math.Round(-0.5)), bits(-1))
	exact("Round(1.5)", bits(math.Round(1.5)), bits(2))
	exact("Round(2.5)", bits(math.Round(2.5)), bits(3))
	exact("Round(-2.5)", bits(math.Round(-2.5)), bits(-3))
	exact("Round(0.49999999999999994)", bits(math.Round(0.49999999999999994)), positiveZero)
	exact("Round(-0.49999999999999994)", bits(math.Round(-0.49999999999999994)), negativeZero)
	exact("Round(+0)", bits(math.Round(value(positiveZero))), positiveZero)
	exact("Round(-0)", bits(math.Round(value(negativeZero))), negativeZero)
	exact("Round(+Inf)", bits(math.Round(value(positiveInfinity))), positiveInfinity)
	exact("Round(-Inf)", bits(math.Round(value(negativeInfinity))), negativeInfinity)
	nan("Round(NaN)", bits(math.Round(value(quietNaN))))

	// The same functions reached indirectly, through a function value. These go
	// through the compiled body of math.Sqrt rather than through the callsite,
	// so they are what says the body itself was lowered.
	for _, indirect := range []struct {
		name string
		fn   func(float64) float64
		in   uint64
		want uint64
	}{
		{"indirect Sqrt(4)", math.Sqrt, bits(4), bits(2)},
		{"indirect Sqrt(-0)", math.Sqrt, negativeZero, negativeZero},
		{"indirect Abs(-0)", math.Abs, negativeZero, positiveZero},
		{"indirect Floor(-0.5)", math.Floor, bits(-0.5), bits(-1)},
		{"indirect Ceil(-0.5)", math.Ceil, bits(-0.5), negativeZero},
		{"indirect Trunc(-0.5)", math.Trunc, bits(-0.5), negativeZero},
		{"indirect Round(2.5)", math.Round, bits(2.5), bits(3)},
		{"indirect RoundToEven(2.5)", math.RoundToEven, bits(2.5), bits(2)},
	} {
		exact(indirect.name, bits(indirect.fn(value(indirect.in))), indirect.want)
	}

	if failures != 0 {
		fmt.Printf("%d edge cases wrong\n", failures)
		os.Exit(1)
	}
	fmt.Println("ok")
}
`

// TestARM64MathIntrinsicEdgeCasesExecute compiles the program above with goc
// and runs it.
func TestARM64MathIntrinsicEdgeCasesExecute(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("the math intrinsics are lowered on Linux ARM64")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc unavailable")
	}

	directory := t.TempDir()
	source := filepath.Join(directory, "main.go")
	if err := os.WriteFile(source, []byte(mathIntrinsicProgram), 0o644); err != nil {
		t.Fatal(err)
	}

	executable := filepath.Join(directory, "mathedge")
	compile := exec.Command(sharedGOCBinary(t), "-o", executable, source)
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile: %v\n%s", err, output)
	}

	output, err := exec.Command(executable).CombinedOutput()
	if err != nil {
		t.Fatalf("the lowered math functions disagree with the specification: %v\n%s", err, output)
	}
	if string(output) != "ok\n" {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

// TestARM64MathCallsLowerToInstructions reads the IR for a program that calls
// each lowered function and checks two things: that the intrinsic is there, and
// that the software implementation it replaced is no longer called. Only the
// first would still hold if, say, math.Sqrt were lowered at its callsites but
// its own body left calling the portable algorithm.
func TestARM64MathCallsLowerToInstructions(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("the math intrinsics are lowered on Linux ARM64")
	}

	directory := t.TempDir()
	source := filepath.Join(directory, "main.go")
	program := `package main

import "math"

var sink float64

func main() {
	sink = math.Sqrt(2) + math.Abs(-1) + math.Floor(1.5) + math.Ceil(1.5) +
		math.Trunc(1.5) + math.Round(2.5) + math.RoundToEven(2.5)
}
`
	if err := os.WriteFile(source, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}

	emit := exec.Command(sharedGOCBinary(t), "-emit-ir", source)
	output, err := emit.Output()
	if err != nil {
		t.Fatalf("emit IR: %v", err)
	}

	// The intrinsics that must appear, and for each the call that must not.
	// math.Sqrt delegates to the portable math.sqrt; Floor, Ceil and Trunc
	// delegate to the assembly stubs archFloor, archCeil and archTrunc.
	wantIntrinsic := []string{
		"float.sqrt.d",
		"float.abs.d",
		"float.floor.d",
		"float.ceil.d",
		"float.trunc.d",
		"float.roundaway.d",
		"float.roundeven.d",
	}
	forbiddenCalls := []string{
		"call $math.sqrt(",
		"call $math.archFloor(",
		"call $math.archCeil(",
		"call $math.archTrunc(",
	}

	found := make(map[string]bool, len(wantIntrinsic))
	var offenders []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		for _, name := range wantIntrinsic {
			if strings.Contains(line, "intrinsic "+name+" ") {
				found[name] = true
			}
		}
		for _, call := range forbiddenCalls {
			if strings.Contains(line, call) {
				offenders = append(offenders, strings.TrimSpace(line))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	for _, name := range wantIntrinsic {
		if !found[name] {
			t.Errorf("no %s in the emitted IR: the call was not lowered to the instruction", name)
		}
	}
	if len(offenders) != 0 {
		t.Errorf("the software implementation is still called, so the lowering did not reach the function's own body:\n\t%s",
			strings.Join(offenders, "\n\t"))
	}
}
