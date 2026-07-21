package lower

import "github.com/evanphx/cg12/ir"

// Jump-table lowering: a sufficiently dense JmpSwitch becomes an O(1) indexed
// branch. The controlling value is biased to a 0-based index (val - min), range
// checked once, and used to index a table of block addresses. Sparser switches
// are left as JmpSwitch for the portable binary-search lowering that follows.
const (
	jtMinCases = 8    // below this a binary search is cheap enough
	jtMaxSpan  = 2048 // cap the table's size (4 bytes per entry)
	jtMaxRatio = 4    // span may be at most this multiple of the case count
)

// JumpTables rewrites eligible JmpSwitch terminators into a bounds check plus a
// JmpTable. It runs before the shared switch lowering, which handles whatever is
// left.
func JumpTables(f *ir.Func) {
	for _, b := range append([]*ir.Block(nil), f.Blocks...) {
		if b.Jmp.Kind == ir.JmpSwitch {
			tryJumpTable(f, b)
		}
	}
}

func tryJumpTable(f *ir.Func, b *ir.Block) {
	cases := b.Jmp.Cases
	deflt := b.Jmp.To
	signed := b.Jmp.Signed
	if len(cases) < jtMinCases {
		return
	}

	// Range of case values, in the switch's own signedness.
	lo, hi := cases[0].Val, cases[0].Val
	less := func(a, b int64) bool { return a < b }
	if !signed {
		less = func(a, b int64) bool { return uint64(a) < uint64(b) }
	}
	for _, c := range cases[1:] {
		if less(c.Val, lo) {
			lo = c.Val
		}
		if less(hi, c.Val) {
			hi = c.Val
		}
	}
	// spread is the span minus one; testing it before the +1 avoids the overflow
	// that a case set covering the full integer range (min and max both present)
	// would hit, which wrapped span to 0, slipped past the cap, and built a
	// zero-length table.
	spread := uint64(hi) - uint64(lo)
	if spread >= jtMaxSpan || spread >= uint64(jtMaxRatio*len(cases)) {
		return
	}
	span := spread + 1

	// A table entry branches straight to its target, so a target carrying a phi
	// (its value would depend on the edge) can't be handled here; fall back to the
	// ordered lowering for those. The default block is a table target only when
	// the value range has gaps -- uncovered indices route to it. With no gaps
	// (case values are distinct in C, so this is len(cases) == span) the default
	// is reached only by the range check's out-of-range edge, straight from b, and
	// its phis are unaffected; a promoted interpreter, whose dispatch's merge block
	// (the switch's implicit default) gathers phis for every loop-carried value,
	// relies on exactly this to still get an indexed dispatch.
	hasGap := uint64(len(cases)) < span
	if hasGap && hasPhi(deflt) {
		return
	}
	for _, c := range cases {
		if hasPhi(c.Blk) {
			return
		}
	}

	// Build the dispatch table: entry i is the block for value lo+i, gaps go to
	// the default.
	table := make([]*ir.Block, span)
	for i := range table {
		table[i] = deflt
	}
	for _, c := range cases {
		table[uint64(c.Val)-uint64(lo)] = c.Blk
	}

	val := b.Jmp.Arg
	cls := f.ClassOf(val)

	// b: index = val - lo; if (unsigned) index > span-1 goto default, else table.
	idx := val
	if lo != 0 {
		idx = b.Sub(cls, val, f.ConstInt(cls, lo))
	}
	above := b.Cmp(ir.CmpUgt, ir.ClsW, idx, f.ConstInt(cls, int64(span-1)))
	tbl := f.NewBlock("")
	b.Jnz(above, deflt, tbl)
	tbl.Jmp = ir.Jmp{Kind: ir.JmpTable, Arg: idx, Targets: table}
}

func hasPhi(b *ir.Block) bool { return len(b.Phis) > 0 }
