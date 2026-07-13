package ir_test

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// Build a small function with the library API and print it as textual IL.
func ExampleFunc() {
	m := ir.NewModule()
	f := m.NewFunc("add", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))

	fmt.Print(f)
	// Output:
	// export function w $add(w %a, w %b) {
	// @start
	// 	%t1 =w add %a, %b
	// 	ret %t1
	// }
}

// Build a function with control flow: max(a, b).
func ExampleBlock_Jnz() {
	m := ir.NewModule()
	f := m.NewFunc("max", ir.ClsW)
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)

	start := f.Entry()
	retA := f.NewBlock("reta")
	retB := f.NewBlock("retb")

	cond := start.Cmp(ir.CmpSgt, ir.ClsW, a, b) // a > b ?
	start.Jnz(cond, retA, retB)
	retA.Ret(a)
	retB.Ret(b)

	fmt.Print(f)
	// Output:
	// function w $max(w %a, w %b) {
	// @start
	// 	%t1 =w csgtw %a, %b
	// 	jnz %t1, @reta, @retb
	// @reta
	// 	ret %a
	// @retb
	// 	ret %b
	// }
}
