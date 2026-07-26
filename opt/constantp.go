package opt

import "github.com/evanphx/cg12/ir"

// ResolveConstantP settles the __builtin_constant_p markers the front end left
// (the "constant_p" intrinsic): each becomes 1 when its operand is a constant at
// this point, else 0. It runs after inlining, so a constant passed to an inlined
// helper -- Ruby's RB_TYPE_P(obj, T_ICLASS) is the pervasive case -- has reached
// the marker, letting the subsequent fold pick the helper's foldable fast path
// and drop the runtime one. Reporting 0 is always sound; the win is reporting 1
// where a later pass could not have recovered the constant.
//
// An address constant reports 0, matching GCC: __builtin_constant_p is about
// integer/float constant values, not linkage-time addresses.
func ResolveConstantP(f *ir.Func) bool {
	changed := false
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OIntrinsic || in.Intrin == nil || in.Intrin.Name != "constant_p" {
				continue
			}
			v := int64(0)
			if isValueConst(f, in.Args[0]) {
				v = 1
			}
			// Replace the marker with a copy of the answer; copy-propagation and the
			// following fold carry it into the branch that consumed it.
			in.Op = ir.OCopy
			in.Intrin = nil
			in.Args = []ir.Ref{f.ConstInt(in.Cls, v)}
			changed = true
		}
	}
	return changed
}

// isValueConst reports whether ref is an integer or floating constant (an address
// constant does not count -- see ResolveConstantP).
func isValueConst(f *ir.Func, ref ir.Ref) bool {
	if ref.Kind != ir.RefConst {
		return false
	}
	switch f.Consts[ref.ID].Kind {
	case ir.ConstInt, ir.ConstFloat:
		return true
	}
	return false
}
