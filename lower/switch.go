// Package lower holds target-independent IR-to-IR lowerings that every backend
// applies before code generation, turning high-level terminators into the
// ordinary branches a backend knows how to emit.
package lower

import (
	"sort"

	"github.com/evanphx/cg12/ir"
)

// linearMax is the largest case count still dispatched by a straight-line chain
// of equality tests. Above it, an ordered binary search is emitted instead.
const linearMax = 4

// Switches rewrites every JmpSwitch terminator in f into ordinary conditional
// branches. A small switch becomes a linear chain of equality tests; a larger
// one becomes an ordered binary search over the (sorted) case values, so
// dispatch is O(log n) comparisons rather than O(n). It reports whether any
// switch was lowered.
//
// A jump table (or a native multiway branch such as wasm's br_table) is a denser
// option a backend may layer on top; this pass provides the portable fallback.
func Switches(f *ir.Func) bool {
	// Snapshot the block list: lowering appends comparison blocks as it runs.
	blocks := append([]*ir.Block(nil), f.Blocks...)
	changed := false
	for _, b := range blocks {
		if b.Jmp.Kind == ir.JmpSwitch {
			lowerSwitch(f, b)
			changed = true
		}
	}
	return changed
}

func lowerSwitch(f *ir.Func, b *ir.Block) {
	val := b.Jmp.Arg
	deflt := b.Jmp.To
	signed := b.Jmp.Signed
	cases := append([]ir.SwitchCase(nil), b.Jmp.Cases...)
	cls := f.ClassOf(val)

	// Sort by value in the same order the ordered comparisons will use, so the
	// binary search partitions correctly.
	sort.Slice(cases, func(i, j int) bool {
		if signed {
			return cases[i].Val < cases[j].Val
		}
		return uint64(cases[i].Val) < uint64(cases[j].Val)
	})

	// Drop the switch terminator; dispatch code is emitted into b and, for a
	// search, into freshly created blocks.
	b.Jmp = ir.Jmp{}
	if len(cases) <= linearMax {
		emitLinear(f, b, val, cls, cases, deflt)
		return
	}
	emitSearch(f, b, val, cls, signed, cases, deflt)
}

// emitLinear dispatches with a straight chain of equality tests ending in the
// default block.
func emitLinear(f *ir.Func, cur *ir.Block, val ir.Ref, cls ir.Cls, cases []ir.SwitchCase, deflt *ir.Block) {
	for i, c := range cases {
		eq := cur.Cmp(ir.CmpEq, ir.ClsW, val, f.ConstInt(cls, c.Val))
		if i == len(cases)-1 {
			cur.Jnz(eq, c.Blk, deflt)
			return
		}
		next := f.NewBlock("")
		cur.Jnz(eq, c.Blk, next)
		cur = next
	}
	cur.Goto(deflt) // no cases: straight to default
}

// emitSearch dispatches with an ordered binary search over the sorted cases.
// Each internal node is a three-way test against the median pivot: == pivot hits
// that case, < pivot descends the lower half, > pivot descends the upper half.
// The equality test and the ordering test share operands, so the backend fuses
// them into one comparison feeding two conditional branches (the AArch64
// cmp; b.eq; b.lt idiom), reusing the flags -- exactly what GCC emits. A hit
// short-circuits at any level, so a path costs about log2(n) comparisons; small
// subranges finish as a linear chain of equality tests against the default.
func emitSearch(f *ir.Func, root *ir.Block, val ir.Ref, cls ir.Cls, signed bool, cases []ir.SwitchCase, deflt *ir.Block) {
	ltPred := ir.CmpUlt
	if signed {
		ltPred = ir.CmpSlt
	}
	var rec func(cur *ir.Block, lo, hi int)
	rec = func(cur *ir.Block, lo, hi int) {
		if hi < lo {
			cur.Goto(deflt)
			return
		}
		if hi-lo+1 <= linearMax {
			emitLinear(f, cur, val, cls, cases[lo:hi+1], deflt)
			return
		}
		mid := (lo + hi) / 2
		// One pivot constant, shared by both tests so the backend sees identical
		// operands and reuses the comparison's flags for the second branch.
		pivot := f.ConstInt(cls, cases[mid].Val)
		eq := cur.Cmp(ir.CmpEq, ir.ClsW, val, pivot)
		order := f.NewBlock("")
		cur.Jnz(eq, cases[mid].Blk, order)
		lt := order.Cmp(ltPred, ir.ClsW, val, pivot)
		left := f.NewBlock("")
		right := f.NewBlock("")
		order.Jnz(lt, left, right)
		rec(left, lo, mid-1)
		rec(right, mid+1, hi)
	}
	rec(root, 0, len(cases)-1)
}
