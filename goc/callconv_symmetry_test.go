package goc_test

import (
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
)

// AMD64_PARITY_PLAN's B0 note flags that goc marks function literals,
// method-value wrappers, and funcvalue adapters CallConvGoInternal
// unconditionally, and warns that a backend which starts honouring the flag
// could mis-lower them. This test establishes the shape of the IR that warning
// is about, because the shape decides what a correct backend rule looks like.
//
// Two properties hold, and they pull in opposite directions:
//
//   - A CallConvGoInternal function is never the target of a *direct* call. It
//     is reached only through a func value, and that indirect call site carries
//     ClosureCall with an explicit CallConvSet, so caller and callee agree.
//
//   - A CallConvGoInternal function does itself make direct calls to ordinary
//     CallConvPlatform functions, and those call instructions carry no explicit
//     convention at all.
//
// The second property is what made arm64's original resolution rule -- take the
// call's convention when set, otherwise inherit the *enclosing function's* --
// unsafe. Under that rule the unmarked call inside a wrapper inherited
// ABIInternal and was lowered against a callee that is plain platform ABI.
//
// arm64 hid it for a long time: both of its conventions assign integer arguments
// starting at X0, so for a small argument count the two lowerings pick the same
// registers and the mismatch is invisible. It was reproduced at nine integer
// arguments, where AAPCS64 puts the ninth on the stack and ABIInternal puts it in
// x8. amd64 has no overlap at all -- System V begins at RDI, ABIInternal at RAX
// -- so the same rule would have miscompiled the first method value it met.
//
// Both backends now resolve from the *callee*: see calleeConventions in
// amd64/convention.go and arm64/convention.go, which carry the identical rule.
func TestGoInternalFunctionsMakeUnmarkedPlatformCalls(t *testing.T) {
	t.Parallel()
	const src = `package main

type counter struct{ n int }

func (c *counter) add(v int) int { return c.n + v }

func apply(f func(int) int, v int) int { return f(v) }

func Test() int {
	c := &counter{n: 5}
	f := c.add
	scale := 3
	double := func(v int) int { return v * scale }
	return apply(double, 7) + f(4)
}
`

	mod, err := goc.Compile("case.go", []byte(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	conv := make(map[string]ir.CallConvention, len(mod.Funcs))
	goInternal := 0
	for _, f := range mod.Funcs {
		conv[f.Name] = f.CallConv
		if f.CallConv == ir.CallConvGoInternal {
			goInternal++
		}
	}
	if goInternal == 0 {
		t.Fatal("no function was marked CallConvGoInternal; the case no longer exercises the property")
	}

	var directToGoInternal, unmarkedCrossConvention, closureCalls int
	for _, f := range mod.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				in := &b.Instrs[i]
				if in.Op != ir.OCall || len(in.Args) == 0 {
					continue
				}
				callee, direct := calleeSymbol(f, in.Args[0])
				if !direct {
					if in.ClosureCall {
						closureCalls++
						if !in.CallConvSet || in.CallConv != ir.CallConvGoInternal {
							t.Errorf("closure call in %s does not carry an explicit ABIInternal convention "+
								"(set=%v conv=%d); the indirect callee would be lowered as ABIInternal and "+
								"the call as platform ABI", f.Name, in.CallConvSet, in.CallConv)
						}
					}
					continue
				}
				if conv[callee] == ir.CallConvGoInternal {
					directToGoInternal++
				}
				if f.CallConv == ir.CallConvGoInternal && !in.CallConvSet &&
					conv[callee] == ir.CallConvPlatform {
					unmarkedCrossConvention++
				}
			}
		}
	}

	if closureCalls == 0 {
		t.Error("no closure call was produced; the case no longer covers the indirect path")
	}
	if directToGoInternal != 0 {
		t.Errorf("found %d direct calls to an ABIInternal function; B0 assumed there are none, "+
			"and amd64's calleeConventions.forCall must be re-checked against them", directToGoInternal)
	}
	if unmarkedCrossConvention == 0 {
		t.Error("no unmarked platform-ABI call was found inside an ABIInternal function; " +
			"if goc's marking has been narrowed, amd64's rejection of arm64's enclosing-function " +
			"fallback should be revisited")
	}
}

// calleeSymbol returns the symbol a direct call targets. Indirect calls (through
// a func value) resolve to a temporary and are reported as not-a-symbol.
func calleeSymbol(f *ir.Func, ref ir.Ref) (string, bool) {
	if ref.Kind != ir.RefConst || int(ref.ID) >= len(f.Consts) {
		return "", false
	}
	c := f.Consts[ref.ID]
	if c.Kind != ir.ConstSym {
		return "", false
	}
	return c.Sym, true
}
