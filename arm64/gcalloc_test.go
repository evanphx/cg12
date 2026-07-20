package arm64_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpillUnderHighPressure builds a function with far more simultaneously-live
// values than there are integer registers, forcing the allocator to spill, and runs
// it to prove the spilled values are stored and reloaded correctly. Each of the 40
// loaded values is used twice with a whole summation pass in between, so all 40 stay
// live across that pass at once (well past the ~25 allocatable integer registers).
func TestSpillUnderHighPressure(t *testing.T) {
	const n = 40
	var src strings.Builder
	src.WriteString("long f(long *p){\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "  long a%d=p[%d];\n", i, i)
	}
	src.WriteString("  long s1=0,s2=0;\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "  s1+=a%d;\n", i) // first use of every value
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "  s2+=a%d*%d;\n", i, i+1) // second use -> all stayed live to here
	}
	src.WriteString("  return s1*1000003 + s2;\n}\n")

	m, err := cc.Compile("c.c", src.String())
	require.NoError(t, err)

	// A reference computation in the driver, so the check does not depend on us
	// getting the arithmetic right by hand.
	var main strings.Builder
	main.WriteString("extern long f(long*);\nint runtest(void){\n  long p[40];\n")
	main.WriteString("  for(int i=0;i<40;i++) p[i]=i*7-3;\n")
	main.WriteString("  long s1=0,s2=0;\n  for(int i=0;i<40;i++) s1+=p[i];\n")
	main.WriteString("  for(int i=0;i<40;i++) s2+=p[i]*(i+1);\n")
	main.WriteString("  long want=s1*1000003+s2;\n  return f(p)==want?0:1;\n}\n")
	main.WriteString("int main(void){ return runtest(); }\n")

	_, code := buildAndRun(t, m, main.String())
	require.Equal(t, 0, code, "spilled values must round-trip through the stack")

	// Confirm the pressure really did force spills (frame-relative traffic).
	dis := disasmModule(t, m)
	assert.Contains(t, dis, "[x29", "40 live values over ~25 registers must spill to the frame")
}

// TestCoalesceEliminatesMoves checks that coalescing removes the copies SSA
// destruction and the ABI introduce. In an accumulating loop the induction variable
// and accumulator are loop-carried phis (a copy on the back edge) and the result is
// returned in x0 (a copy to the return register). With coalescing every one of those
// lands each copy's source and destination in one register, so no phi or return move
// survives -- the loop updates its values in place and the result is already in the
// return register. The one move that must remain is the entry boundary transfer: the
// parameter arrives in w0, which the accumulator then occupies, so it vacates w0.
func TestCoalesceEliminatesMoves(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("sum", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start, loop, body, exit := f.Entry(), f.NewBlock("loop"), f.NewBlock("body"), f.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	c := loop.Cmp(ir.CmpSge, ir.ClsW, i, n)
	loop.Jnz(c, exit, body)
	s2 := body.Add(ir.ClsW, s, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)

	dis := disasmModule(t, m)

	// A register-to-register move (mov wD, wS) is a copy; a move of an immediate
	// (mov wD, #imm) is not. Moves touching a scratch register (w15/w16/w17) are the
	// emitter routing a materialised constant, not a temp copy, so they are ignored.
	regMove := regexp.MustCompile(`\bmov\s+([wx]\d+),\s*([wx]\d+)\b`)
	var copies []string
	for _, mm := range regMove.FindAllStringSubmatch(dis, -1) {
		if isScratchName(mm[1]) || isScratchName(mm[2]) {
			continue
		}
		copies = append(copies, mm[0])
	}
	assert.LessOrEqualf(t, len(copies), 1,
		"loop-carried phis and the return must coalesce, leaving only the entry boundary move; got %v in:\n%s", copies, dis)
}

func isScratchName(reg string) bool {
	switch reg {
	case "w15", "w16", "w17", "x15", "x16", "x17":
		return true
	}
	return false
}
