package amd64

import "github.com/evanphx/cg12/ir"

// foldAddressing folds a load/store's address computation into an x86
// base+index*scale+disp memory operand, dropping the now-dead address arithmetic.
// x86 addresses [base + index*scale + disp] in one instruction, so an array access
// need not compute its address in a register first.
//
// Two shapes are folded, chosen so the emitter never needs more scratch registers
// than exist:
//
//   - [base + disp]: base + a constant, for any base. One address register at most.
//   - [alloca + index*scale (+ disp)]: an alloca base (which resolves to rbp+off,
//     needing no register) plus a scaled index. A general (non-alloca) base with an
//     index is left alone, since base and index together could exhaust the scratch
//     registers a spilled value also needs.
//
// The result rides on the load/store: Args carry base and (when scaled) index after
// the stored value, Aux is the displacement, and Amode is the scale (0 = no index).
func foldAddressing(f *ir.Func) {
	uses, defOf := defUse(f)
	single := func(r ir.Ref) bool { return r.Kind == ir.RefTemp && uses[r.ID] == 1 }
	isAlloca := func(r ir.Ref) bool {
		d := defOf(r)
		return d != nil && (d.Op == ir.OAlloc4 || d.Op == ir.OAlloc8 || d.Op == ir.OAlloc16)
	}

	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			var ai int
			switch {
			case in.Op.IsLoad():
				ai = 0
			case in.Op.IsStore():
				ai = 1
			default:
				continue
			}
			if in.Op == ir.OLoads || in.Op == ir.OLoadd ||
				in.Op == ir.OStores || in.Op == ir.OStored ||
				in.Op == ir.OLoadq || in.Op == ir.OStoreq {
				continue // FP and 128-bit go through a different value path
			}
			addr := in.Args[ai]
			if in.Amode != 0 || !single(addr) {
				continue
			}
			add := defOf(addr)
			if add == nil || add.Op != ir.OAdd {
				continue
			}
			x, y := add.Args[0], add.Args[1]

			// [base + disp]: one operand a 32-bit constant.
			if c := intConstAMD(f, y); c != nil && fitsInt32(c.Int) {
				setFolded(in, ai, x, ir.Ref{}, 0, c.Int)
				nop(add)
				continue
			}
			if c := intConstAMD(f, x); c != nil && fitsInt32(c.Int) {
				setFolded(in, ai, y, ir.Ref{}, 0, c.Int)
				nop(add)
				continue
			}

			// [alloca + index*scale]: identify the alloca base and the index side.
			base, idx := x, y
			if isAlloca(y) && !isAlloca(x) {
				base, idx = y, x
			}
			if !isAlloca(base) {
				continue
			}
			index, scale := idx, byte(1)
			if sh := defOf(idx); sh != nil && sh.Op == ir.OShl && single(idx) {
				if k, ok := shiftConst(f, sh.Args[1]); ok {
					if sc := byte(1) << k; sc == 2 || sc == 4 || sc == 8 {
						index, scale = sh.Args[0], sc
						nop(sh)
					}
				}
			}
			setFolded(in, ai, base, index, scale, 0)
			nop(add)
		}
	}
}

// setFolded rewrites in's address operands to the folded form.
func setFolded(in *ir.Instr, ai int, base, index ir.Ref, scale byte, disp int64) {
	in.Aux = disp
	in.Amode = int32(scale)
	var args []ir.Ref
	if ai == 1 {
		args = append(args, in.Args[0]) // the stored value
	}
	args = append(args, base)
	if scale != 0 {
		args = append(args, index)
	}
	in.Args = args
}

func fitsInt32(v int64) bool { return v >= -0x80000000 && v <= 0x7fffffff }

// shiftConst returns the shift amount if ref is a small constant shift count.
func shiftConst(f *ir.Func, ref ir.Ref) (uint, bool) {
	if c := intConstAMD(f, ref); c != nil && c.Int >= 0 && c.Int <= 3 {
		return uint(c.Int), true
	}
	return 0, false
}

// defUse returns a use count per temp and a lookup from a ref to its defining
// instruction -- enough to recognize and rewrite small address idioms in SSA form.
func defUse(f *ir.Func) (uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	uses = map[uint32]int{}
	def := map[uint32]*ir.Instr{}
	mark := func(r ir.Ref) {
		if r.Kind == ir.RefTemp {
			uses[r.ID]++
		}
	}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				mark(a)
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			for _, a := range in.Uses() {
				mark(a)
			}
			if in.To.Kind == ir.RefTemp {
				def[in.To.ID] = in
			}
		}
		mark(b.Jmp.Arg)
		for _, a := range b.Jmp.Args {
			mark(a)
		}
	}
	return uses, func(r ir.Ref) *ir.Instr {
		if r.Kind != ir.RefTemp {
			return nil
		}
		return def[r.ID]
	}
}

func nop(in *ir.Instr) {
	in.Op = ir.ONop
	in.To = ir.Ref{}
	in.Args = nil
}
