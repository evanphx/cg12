package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
	lowerpass "github.com/evanphx/cg12/lower"
)

// lower rewrites f from SSA into a form ready for register allocation and
// emission: critical edges are split, phis are replaced by copies, and the
// AAPCS64 calling convention is made explicit with copies to and from
// pre-coloured physical-register temporaries.
//
// Only the integer subset (classes w and l) is handled; anything outside it
// returns an explicit error rather than emitting silently wrong code.
func lower(f *ir.Func, tlsModel TLSModel) error {
	if err := f.MarkLowered("arm64"); err != nil {
		return err
	}
	if err := validateComparisonPredicates(f); err != nil {
		return fmt.Errorf("before ARM64 lowering: %w", err)
	}
	lowerpass.JumpTables(f) // dense switches -> indexed branch (JmpTable)
	lowerpass.Switches(f)   // remaining multiway branches -> conditional branches
	// Before the folds: they rewrite addressing, and a thread-local is not an
	// address they can reason about.
	lowerTLS(f, tlsModel)
	lowerpass.HoistAllocas(f)
	canonCompares(f)
	foldIdioms(f)
	foldSharedOffset(f)
	foldAddressing(f)
	fuseArithShift(f) // after foldAddressing: it gets first claim on address shifts
	fuseBitfield(f)
	fuseCmpSel(f) // after fuseBitfield: it gets first claim on ANDs (ubfx)
	fuseCcmp(f)   // conjoined compares feeding a branch -> ccmp chain
	lowerpass.SplitCriticalEdges(f)
	lowerpass.CoalescePhis(f)
	lowerpass.DestructSSA(f)
	lowerpass.ThreadJumps(f) // collapse the empty forwarding blocks edge splitting left
	if err := validateComparisonPredicates(f); err != nil {
		return fmt.Errorf("after SSA destruction: %w", err)
	}
	if err := lowerABI(f); err != nil {
		return err
	}
	stabilizeClosureContext(f)
	if err := validateComparisonPredicates(f); err != nil {
		return fmt.Errorf("after ABI lowering: %w", err)
	}
	return nil
}

// stabilizeClosureContext copies an incoming ABIInternal closure environment
// out of X26 after ABI lowering. X26 is fixed for entry but volatile across
// calls; the ordinary temporary can therefore be spilled for precise GC when
// the closure remains live at a safepoint.
func stabilizeClosureContext(function *ir.Func) {
	if !function.HasClosureContext {
		return
	}

	var incoming ir.Ref
	for _, temporary := range function.Temps {
		if temporary != nil && temporary.ClosureContext {
			incoming = temporary.Ref()
			break
		}
	}
	if incoming.IsNone() {
		return
	}

	incomingTemporary := function.Temp(incoming)
	saved := function.NewTemp("closure.saved", incomingTemporary.Cls)
	savedTemporary := function.Temp(saved)
	savedTemporary.GCRef = incomingTemporary.GCRef
	savedTemporary.GCType = incomingTemporary.GCType

	for _, block := range function.Blocks {
		for _, phi := range block.Phis {
			for index, argument := range phi.Args {
				if argument == incoming {
					phi.Args[index] = saved
				}
			}
		}
		for index := range block.Instrs {
			for argumentIndex, argument := range block.Instrs[index].Args {
				if argument == incoming {
					block.Instrs[index].Args[argumentIndex] = saved
				}
			}
			if block.Instrs[index].ClosureContext == incoming {
				block.Instrs[index].ClosureContext = saved
			}
		}
		if block.Jmp.Arg == incoming {
			block.Jmp.Arg = saved
		}
		for index, argument := range block.Jmp.Args {
			if argument == incoming {
				block.Jmp.Args[index] = saved
			}
		}
	}

	copyContext := ir.Instr{
		Op:   ir.OCopy,
		Cls:  incomingTemporary.Cls,
		To:   saved,
		Args: []ir.Ref{incoming},
	}
	insertAt := 0
	for insertAt < len(function.Start.Instrs) && function.Start.Instrs[insertAt].Op == ir.OPar {
		insertAt++
	}
	function.Start.Instrs = append(function.Start.Instrs, ir.Instr{})
	copy(function.Start.Instrs[insertAt+1:], function.Start.Instrs[insertAt:])
	function.Start.Instrs[insertAt] = copyContext
}

func validateComparisonPredicates(function *ir.Func) error {
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op != ir.OCmp || len(instruction.Args) == 0 {
				continue
			}
			if instruction.Args[0].Kind == ir.RefAggregate {
				return fmt.Errorf("aggregate value reached comparison in %s.%s at %+v", function.Name, block.Name, instruction.Pos)
			}
			operandClass := function.ClassOf(instruction.Args[0])
			if operandClass.IsFloat() != instruction.Cmp.IsFloat() {
				return fmt.Errorf("comparison predicate %v does not match %s operands at %+v", instruction.Cmp, operandClass, instruction.Pos)
			}
		}
	}
	return nil
}

// foldSharedOffset folds a constant-offset address computation `a = add(base, #c)`
// into the immediate offset of EVERY load/store that uses a, dropping the now-dead
// add -- the multi-use case foldAddressing declines. Its motivating shape is a
// read-modify-write of a struct field (`cfp->sp += n`, pervasive in the interpreter's
// stack pushes): the address feeds both a load and a store, so foldAddressing leaves
// the add and reuses the address register, one instruction more than gcc's
// `ldr [base,#c] ; ... ; str [base,#c]`. Offset folding is free per access and folding
// every use removes the address register entirely, so it is never worse. It runs in
// SSA, where base dominates every use of a, so referencing base at each use is sound.
func foldSharedOffset(f *ir.Func) {
	// Collect every use site of every temp, and the temps a non-foldable context
	// (a phi arg or a block terminator) touches -- those block the fold.
	type useSite struct {
		in   *ir.Instr
		argi int
	}
	sites := map[uint32][]useSite{}
	poisoned := map[uint32]bool{}
	for _, b := range f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				if a.Kind == ir.RefTemp {
					poisoned[a.ID] = true
				}
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			for ai, a := range in.Args {
				if a.Kind == ir.RefTemp {
					sites[a.ID] = append(sites[a.ID], useSite{in, ai})
				}
			}
		}
		if b.Jmp.Arg.Kind == ir.RefTemp {
			poisoned[b.Jmp.Arg.ID] = true
		}
		for _, a := range b.Jmp.Args {
			if a.Kind == ir.RefTemp {
				poisoned[a.ID] = true
			}
		}
	}

	for _, b := range f.Blocks {
		for i := range b.Instrs {
			add := &b.Instrs[i]
			if add.Op != ir.OAdd || add.Aux != 0 || add.To.Kind != ir.RefTemp {
				continue
			}
			d := add.To.ID
			if poisoned[d] {
				continue
			}
			c, base, ok := constAddOffset(f, add)
			if !ok || base.Kind != ir.RefTemp {
				continue
			}
			us := sites[d]
			if len(us) < 2 { // single-use: foldAddressing handles it (and index forms)
				continue
			}
			foldable := true
			for _, s := range us {
				if !memBaseFits(s.in, s.argi, c) {
					foldable = false
					break
				}
			}
			if !foldable {
				continue
			}
			for _, s := range us {
				s.in.Aux = c
				s.in.Args[s.argi] = base
			}
			nop(add)
		}
	}
}

// constAddOffset reports whether add has exactly one integer-constant operand,
// returning that offset and the other (base) operand.
func constAddOffset(f *ir.Func, add *ir.Instr) (int64, ir.Ref, bool) {
	if len(add.Args) != 2 {
		return 0, ir.Ref{}, false
	}
	if c, ok := intConst(f, add.Args[1]); ok {
		if _, ok := intConst(f, add.Args[0]); !ok {
			return c, add.Args[0], true
		}
	}
	if c, ok := intConst(f, add.Args[0]); ok {
		if _, ok := intConst(f, add.Args[1]); !ok {
			return c, add.Args[1], true
		}
	}
	return 0, ir.Ref{}, false
}

// memBaseFits reports whether in is a simple (non-indexed, integer) load or store
// using its argi operand as the base address, with no existing offset, and whose
// access width admits c as a scaled unsigned immediate. A use as the stored value
// or an index (any other position) fails, forcing the caller to keep the add.
func memBaseFits(in *ir.Instr, argi int, c int64) bool {
	if in.Aux != 0 || in.Cls.IsFloat() {
		return false
	}
	access := accessBytes(in.Op)
	if access == 0 {
		return false
	}
	switch {
	case in.Op.IsLoad() && len(in.Args) == 1 && argi == 0:
	case in.Op.IsStore() && len(in.Args) == 2 && argi == 1:
	default:
		return false
	}
	return c >= 0 && c%int64(access) == 0 && c/int64(access) <= 4095
}

// foldAddressing folds a load/store's address computation into an AArch64
// register-offset addressing mode. It matches
//
//	off = shl(sext(i), k) ; a = add(base, off) ; load/store [a]
//
// (with the sext and shift optional, and k matching the access width for a
// scaled index) and rewrites the memory op to carry (base, index) plus the
// extend/scale in Amode, dropping the now-dead address arithmetic. A load then
// carries [base, index]; a store carries [value, base, index].
func foldAddressing(f *ir.Func) {
	uses, defOf := defUse(f)
	single := func(r ir.Ref) bool { return r.Kind == ir.RefTemp && uses[r.ID] == 1 }

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
			access := accessBytes(in.Op)
			if access == 0 || in.Cls.IsFloat() { // skip FP and 128-bit
				continue
			}
			addr := in.Args[ai]
			if !single(addr) {
				continue
			}
			add := defOf(addr)
			if add == nil || add.Op != ir.OAdd {
				continue
			}
			// A constant offset that fits the scaled unsigned immediate becomes
			// [base, #imm] with no index register at all.
			if c, cbase, ok := immOffset(f, add, access); ok {
				nop(add)
				in.Aux = c
				if in.Op.IsLoad() {
					in.Args = []ir.Ref{cbase}
				} else {
					in.Args = []ir.Ref{in.Args[0], cbase}
				}
				continue
			}
			base, off := add.Args[0], add.Args[1]
			// The index side is whichever operand is a shift or sign-extension; a
			// plain add folds too (base + index, no scale).
			if isIndexExpr(defOf(add.Args[0])) && !isIndexExpr(defOf(add.Args[1])) {
				base, off = add.Args[1], add.Args[0]
			}

			option, s := a64.ExtLSL, uint32(0)
			index := off
			// Peel a scale shift when it matches the access width.
			if sd := defOf(index); sd != nil && sd.Op == ir.OShl && single(index) {
				if sh, ok := intConst(f, sd.Args[1]); ok && sh == log2Bytes(access) && access > 1 {
					s = 1
					index = sd.Args[0]
					nop(sd)
				}
			}
			// Peel a 32-bit sign extension into an SXTW index.
			if ed := defOf(index); ed != nil && ed.Op == ir.OExtsw && single(index) {
				option = a64.ExtSXTW
				index = ed.Args[0]
				nop(ed)
			}

			nop(add)
			in.Amode = int32(option)<<1 | int32(s)
			if in.Op.IsLoad() {
				in.Args = []ir.Ref{base, index}
			} else {
				in.Args = []ir.Ref{in.Args[0], base, index}
			}
		}
	}
}

// immOffset reports whether one operand of add is a constant that fits an
// AArch64 scaled unsigned load/store offset for an access of the given width: a
// non-negative multiple of the width, at most 4095 of them. It returns the byte
// offset and the other operand (the base).
func immOffset(f *ir.Func, add *ir.Instr, access int) (int64, ir.Ref, bool) {
	for i := 0; i < 2; i++ {
		if c, ok := intConst(f, add.Args[i]); ok {
			if c >= 0 && c%int64(access) == 0 && c/int64(access) <= 4095 {
				return c, add.Args[1-i], true
			}
		}
	}
	return 0, ir.Ref{}, false
}

// canonCompares puts a constant comparison operand second, so instruction
// selection can fold it into a compare-immediate. A compare with the constant first
// (`CONST == x`, or one an earlier rewrite produced) otherwise materializes the
// constant into a scratch register and does a register compare -- `mov w16, #28 ;
// cmp w16, w11` instead of `cmp w11, #28`. Swapping the operands flips an ordered
// predicate (< becomes >, <= becomes >=); equality and inequality are unchanged.
func canonCompares(f *ir.Func) {
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OCmp || in.Cmp.IsFloat() || len(in.Args) != 2 {
				continue
			}
			_, c0 := intConst(f, in.Args[0])
			_, c1 := intConst(f, in.Args[1])
			if c0 && !c1 {
				in.Args[0], in.Args[1] = in.Args[1], in.Args[0]
				in.Cmp = swapCmp(in.Cmp)
			}
		}
	}
}

// fuseBitfield rewrites `and(shr(x, lsb), mask)` -- a value shifted down and masked
// to its low bits -- into a single unsigned bitfield extract (UBFX), the way gcc and
// LLVM lower a `(v >> lsb) & mask` flag test. It requires the shift to be single-use
// (so dropping it is free) and the mask to be a contiguous low run of bits, 2^width-1.
// The lsb and width ride in the AND's Aux (otherwise unused on an AND); the selector
// reads them back and emits ubfx. The shift stays logical (OShr) so the extracted
// bits are exactly the source's.
func fuseBitfield(f *ir.Func) {
	uses, defOf := defUse(f)
	single := func(r ir.Ref) bool { return r.Kind == ir.RefTemp && uses[r.ID] == 1 }
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OAnd || in.Cls.IsFloat() {
				continue
			}
			var maskRef, valRef ir.Ref
			if _, ok := intConst(f, in.Args[1]); ok {
				maskRef, valRef = in.Args[1], in.Args[0]
			} else if _, ok := intConst(f, in.Args[0]); ok {
				maskRef, valRef = in.Args[0], in.Args[1]
			} else {
				continue
			}
			mask, _ := intConst(f, maskRef)
			width, ok := lowMaskWidth(uint64(mask), in.Cls.Size())
			if !ok || !single(valRef) {
				continue
			}
			sd := defOf(valRef)
			if sd == nil || sd.Op != ir.OShr {
				continue
			}
			lsb, ok := intConst(f, sd.Args[1])
			if !ok || lsb <= 0 || int(lsb)+width > in.Cls.Size()*8 {
				continue
			}
			src := sd.Args[0]
			nop(sd)
			in.Args = []ir.Ref{src, maskRef} // selection uses Args[0] + Aux; the mask is inert
			in.Aux = lsb<<8 | int64(width)
		}
	}
}

// lowMaskWidth returns width when mask is the contiguous low mask 2^width-1, with
// 1 <= width < the register's bit count (a full-width mask is a no-op, not extract).
func lowMaskWidth(mask uint64, size int) (int, bool) {
	if size == 4 {
		mask &= 0xffffffff
	}
	if mask == 0 || mask&(mask+1) != 0 { // not of the form 2^width - 1
		return 0, false
	}
	width := 0
	for m := mask; m != 0; m >>= 1 {
		width++
	}
	if width >= size*8 {
		return 0, false
	}
	return width, true
}

// fuseCmpSel folds a single-use comparison feeding a conditional select into the
// select, so the emitter produces `cmp; csel <cond>` rather than materializing
// the boolean and re-testing it (`cmp; cset; cmp #0; csel`). It mirrors the
// compare-branch fusion in mc.go but is simpler: the comparison is pure and
// re-emitted at the select site, so there is no flags-lifetime constraint -- the
// operands dominate the select (they dominated the compare, which dominated its
// sole use), and register allocation, running after lowering, extends their live
// ranges to the select automatically.
//
// The condition may be an ordinary compare (OCmp) or a mask test (a tstable
// OAnd, `(x & mask) ? a : b`). The fused OSel carries the compare's two operands
// in Args[0:2] and the arms in Args[2:4], the predicate in Cmp, and the mode in
// Aux; the fused OSel is the private 4-operand form sel() reads back. A compare
// with any other use, or an AND already claimed by fuseBitfield (Aux set, a
// ubfx), is left alone -- the select then keeps the unfused cmp #0 path.
func fuseCmpSel(f *ir.Func) {
	uses, defOf := defUse(f)
	single := func(r ir.Ref) bool { return r.Kind == ir.RefTemp && uses[r.ID] == 1 }
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OSel || len(in.Args) != 3 || !single(in.Args[0]) {
				continue
			}
			d := defOf(in.Args[0])
			if d == nil {
				continue
			}
			armT, armF := in.Args[1], in.Args[2]
			switch {
			case d.Op == ir.OCmp:
				if _, ok := condOf(d.Cmp, d.Cmp.IsFloat()); !ok {
					continue
				}
				in.Cmp = d.Cmp
				in.Aux = selFuseCmp
			case d.Op == ir.OAnd && d.Aux == 0 && tstableAnd(f, d):
				in.Cmp = ir.CmpNe
				in.Aux = selFuseTst
			default:
				continue
			}
			in.Args = []ir.Ref{d.Args[0], d.Args[1], armT, armF}
			nop(d)
		}
	}
}

// fuseCcmp folds a conjoined branch condition -- `if (cmp1 && cmp2)` or
// `if (cmp1 || cmp2)` reassociated to a single AND/OR of two compares feeding the
// block's jnz -- into one OCCmp, which the emitter lowers to cmp; ccmp; b.cond
// (no branch between the two compares, no materialized booleans). It fires only
// when the AND/OR is the branch condition and both operands are single-use
// integer compares, so the private OCCmp only ever reaches the branch path.
//
// OCCmp carries the two compares' operands in Args [a1,b1,a2,b2], the second
// compare's predicate in Cmp (the branch tests it), and Aux = pred1<<8 | orBit.
// Floating compares are left alone (no FCCMP support yet).
func fuseCcmp(f *ir.Func) {
	uses, defOf := defUse(f)
	single := func(r ir.Ref) bool { return r.Kind == ir.RefTemp && uses[r.ID] == 1 }
	intCmp := func(r ir.Ref) *ir.Instr {
		if !single(r) {
			return nil
		}
		d := defOf(r)
		if d == nil || d.Op != ir.OCmp || d.Cmp.IsFloat() {
			return nil
		}
		if _, ok := condOf(d.Cmp, false); !ok {
			return nil
		}
		return d
	}
	for _, b := range f.Blocks {
		if b.Jmp.Kind != ir.JmpJnz || b.Jmp.Arg.Kind != ir.RefTemp || uses[b.Jmp.Arg.ID] != 1 {
			continue
		}
		d := defOf(b.Jmp.Arg)
		if d == nil || (d.Op != ir.OAnd && d.Op != ir.OOr) || d.Aux != 0 || d.Cls.IsFloat() {
			continue
		}
		c1, c2 := intCmp(d.Args[0]), intCmp(d.Args[1])
		if c1 == nil || c2 == nil {
			continue
		}
		var orBit int64
		if d.Op == ir.OOr {
			orBit = 1
		}
		pred1 := c1.Cmp
		a1, b1, a2, b2 := c1.Args[0], c1.Args[1], c2.Args[0], c2.Args[1]
		d.Op = ir.OCCmp
		d.Cmp = c2.Cmp // the branch tests the second compare's predicate
		d.Args = []ir.Ref{a1, b1, a2, b2}
		d.Aux = int64(pred1)<<8 | orBit
		nop(c1)
		nop(c2)
	}
}

// swapCmp is the predicate that holds when a comparison's operands are exchanged.
func swapCmp(c ir.Cmp) ir.Cmp {
	switch c {
	case ir.CmpSle:
		return ir.CmpSge
	case ir.CmpSlt:
		return ir.CmpSgt
	case ir.CmpSge:
		return ir.CmpSle
	case ir.CmpSgt:
		return ir.CmpSlt
	case ir.CmpUle:
		return ir.CmpUge
	case ir.CmpUlt:
		return ir.CmpUgt
	case ir.CmpUge:
		return ir.CmpUle
	case ir.CmpUgt:
		return ir.CmpUlt
	}
	return c // CmpEq, CmpNe: commutative
}

// isIndexExpr reports whether an instruction looks like the index side of an
// address (a scale shift or a sign extension).
func isIndexExpr(d *ir.Instr) bool {
	return d != nil && (d.Op == ir.OShl || d.Op == ir.OExtsw)
}

// accessBytes returns the memory-access width in bytes of a load/store op, or 0
// for the 128-bit ops this fold skips.
func accessBytes(op ir.Op) int {
	switch op {
	case ir.OLoadub, ir.OLoadsb, ir.OStoreb:
		return 1
	case ir.OLoaduh, ir.OLoadsh, ir.OStoreh:
		return 2
	case ir.OLoaduw, ir.OLoadsw, ir.OStorew, ir.OLoads:
		return 4
	case ir.OLoadl, ir.OStorel, ir.OLoadd:
		return 8
	}
	return 0
}

func log2Bytes(n int) int64 {
	switch n {
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	}
	return 0
}

// defUse returns, for f, a use count per temporary and a lookup from a ref to
// its defining instruction — enough to recognize and rewrite small multi-
// instruction idioms while still in SSA form.
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

// foldIdioms rewrites the shift-or rotate and and-not (bic) idioms into the
// single ORotr / OBic ops the emitters lower to one AArch64 instruction.
func foldIdioms(f *ir.Func) {
	uses, defOf := defUse(f)
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			switch in.Op {
			case ir.OOr:
				foldRotate(f, in, uses, defOf)
			case ir.OAnd:
				foldBic(f, in, uses, defOf)
			case ir.OAdd, ir.OSub:
				foldMulAdd(f, in, uses, defOf)
			}
		}
	}
}

// foldMulAdd fuses an integer multiply feeding an add or subtract into a single
// OMAdd/OMSub (AArch64 madd/msub: rd = ra + rn*rm and rd = ra - rn*rm), when the
// multiply feeds only this op so dropping it changes nothing else. A subtract
// fuses only when the multiply is the subtrahend, since msub negates the product.
func foldMulAdd(f *ir.Func, in *ir.Instr, uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	if in.Cls.IsFloat() {
		return
	}
	mulOf := func(r ir.Ref) *ir.Instr {
		if r.Kind != ir.RefTemp || uses[r.ID] != 1 {
			return nil
		}
		if d := defOf(r); d != nil && d.Op == ir.OMul && d.Cls == in.Cls {
			return d
		}
		return nil
	}
	fuse := func(op ir.Op, m *ir.Instr, addend ir.Ref) {
		in.Op = op
		in.Args = []ir.Ref{m.Args[0], m.Args[1], addend}
		nop(m)
	}
	if in.Op == ir.OAdd {
		if m := mulOf(in.Args[0]); m != nil {
			fuse(ir.OMAdd, m, in.Args[1])
		} else if m := mulOf(in.Args[1]); m != nil {
			fuse(ir.OMAdd, m, in.Args[0])
		}
		return
	}
	if m := mulOf(in.Args[1]); m != nil { // sub(z, x*y) -> msub x,y,z
		fuse(ir.OMSub, m, in.Args[0])
	}
}

// foldRotate rewrites the shift-or rotate idiom into a single ORotr, which the
// emitters turn into an AArch64 ROR. It matches
//
//	t1 = shr x, c1 ; t2 = shl x, c2 ; t3 = or t1, t2   (c1+c2 = width)
//
// (either shift order) and, when both shifts feed only the or, replaces the or
// with `rotr x, c1` and drops the now-dead shifts.
func foldRotate(f *ir.Func, in *ir.Instr, uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	x, ror, ok := rotateMatch(f, in, defOf)
	if !ok {
		return
	}
	// Only fold when each shift result is consumed solely by this or, so dropping
	// the shifts changes nothing else.
	if uses[in.Args[0].ID] != 1 || uses[in.Args[1].ID] != 1 {
		return
	}
	s1, s2 := defOf(in.Args[0]), defOf(in.Args[1])
	in.Op = ir.ORotr
	in.Args = []ir.Ref{x, f.ConstInt(in.Cls, int64(ror))}
	nop(s1)
	nop(s2)
}

// foldBic rewrites and(a, ~b) into a single OBic (a AND NOT b), when the NOT —
// a xor with an all-ones constant — feeds only this and.
func foldBic(f *ir.Func, in *ir.Instr, uses map[uint32]int, defOf func(ir.Ref) *ir.Instr) {
	notOf := func(r ir.Ref) (ir.Ref, bool) {
		if uses[r.ID] != 1 {
			return ir.Ref{}, false
		}
		d := defOf(r)
		if d == nil || d.Op != ir.OXor {
			return ir.Ref{}, false
		}
		if allOnes(f, d.Cls, d.Args[1]) {
			return d.Args[0], true
		}
		if allOnes(f, d.Cls, d.Args[0]) {
			return d.Args[1], true
		}
		return ir.Ref{}, false
	}
	a, b := in.Args[0], in.Args[1]
	if x, ok := notOf(b); ok {
		in.Op = ir.OBic
		in.Args = []ir.Ref{a, x}
		nop(defOf(b))
	} else if x, ok := notOf(a); ok {
		in.Op = ir.OBic
		in.Args = []ir.Ref{b, x}
		nop(defOf(a))
	}
}

// allOnes reports whether ref is the all-ones constant for cls's width.
func allOnes(f *ir.Func, cls ir.Cls, ref ir.Ref) bool {
	v, ok := intConst(f, ref)
	if !ok {
		return false
	}
	if cls.Size() == 4 {
		return uint32(v) == 0xffffffff
	}
	return uint64(v) == ^uint64(0)
}

// nop turns an instruction into a no-op that defines nothing.
func nop(in *ir.Instr) {
	in.Op = ir.ONop
	in.To = ir.Ref{}
	in.Args = nil
}

// fuseArithShift folds a single-use constant shift feeding an add or subtract into
// the shifted-register form of the instruction: add(a, shl(b,k)) becomes
// `add a, b, lsl #k`, dropping the standalone shift. It runs after foldAddressing,
// which gets first claim on shifts feeding a load/store address -- a scaled index
// belongs in the addressing mode, not a shifted add. The shift kind and amount ride
// in Aux (which OAdd/OSub do not otherwise use) with the shifted operand pinned to
// Args[1]; addSub reads it back at emit time. A subtract only fuses its subtrahend
// (a - (b<<k)); an add, being commutative, fuses either operand.
func fuseArithShift(f *ir.Func) {
	uses, defOf := defUse(f)
	single := func(r ir.Ref) bool { return r.Kind == ir.RefTemp && uses[r.ID] == 1 }
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OAdd && in.Op != ir.OSub {
				continue
			}
			if in.Cls.IsFloat() || in.Aux != 0 {
				continue
			}
			cands := []int{1} // sub: only the subtrahend
			if in.Op == ir.OAdd {
				cands = []int{1, 0} // add: either operand
			}
			for _, ai := range cands {
				operand := in.Args[ai]
				if !single(operand) {
					continue
				}
				sd := defOf(operand)
				sh, ok := shiftKind(sd)
				if !ok {
					continue
				}
				amt, ok := intConst(f, sd.Args[1])
				if !ok || amt < 1 || int(amt) >= in.Cls.Size()*8 {
					continue
				}
				shiftInput := sd.Args[0]
				nop(sd)                                       // wipes sd.Args, so read its input first
				in.Args = []ir.Ref{in.Args[1-ai], shiftInput} // shifted operand -> Args[1]
				in.Aux = int64(sh)<<6 | amt
				break
			}
		}
	}
}

// shiftKind maps a shift instruction to the emitter's shiftOp, reporting false for
// anything that is not a constant-amount shift.
func shiftKind(d *ir.Instr) (shiftOp, bool) {
	if d == nil {
		return 0, false
	}
	switch d.Op {
	case ir.OShl:
		return shLsl, true
	case ir.OShr:
		return shLsr, true
	case ir.OSar:
		return shAsr, true
	}
	return 0, false
}

// argLoc is where one AAPCS64 argument is passed: either a physical register or
// a stack slot at the given byte offset in the outgoing/incoming argument area.
type argLoc struct {
	reg     Reg
	onStack bool
	stacky  int // byte offset when onStack
}

// argAssigner walks a sequence of argument classes and assigns each to a
// register or, once a bank is exhausted, to the stack. Integer and
// floating-point arguments consume independent register banks (x0..x7, v0..v7).
type argAssigner struct {
	ngrn, nsrn int
	nsaa       int // next stacked-argument byte offset
	intRegs    int
	floatRegs  int
	goABI      bool
}

func newArgAssigner(goABI bool) argAssigner {
	c := conventionABI(goInternalConvention(goABI))
	return argAssigner{intRegs: c.intArgRegs, floatRegs: c.floatArgRegs, goABI: c.packsStackArgs}
}

func (a *argAssigner) assign(cls ir.Cls) argLoc {
	if cls.IsFloat() {
		if a.nsrn < a.floatRegs {
			r := vReg(a.nsrn)
			a.nsrn++
			return argLoc{reg: r}
		}
	} else if a.ngrn < a.intRegs {
		r := Reg(int(X0) + a.ngrn)
		a.ngrn++
		return argLoc{reg: r}
	}
	if a.goABI {
		off := a.assignStack(cls.Size(), cls.Size())
		return argLoc{onStack: true, stacky: off}
	}
	off := a.nsaa
	a.nsaa += 8 // AAPCS64 scalars occupy one 8-byte stack slot
	return argLoc{onStack: true, stacky: off}
}

// stackBytes returns the 16-aligned size of the stacked-argument area.
func (a *argAssigner) stackBytes() int { return roundUp(a.nsaa, 16) }

// retReg returns the register a value of the given class is returned in.
func retReg(cls ir.Cls) Reg {
	if cls.IsFloat() {
		return V0
	}
	return X0
}

// newPinned creates a fresh temporary hard-bound to physical register r.
func newPinned(f *ir.Func, r Reg, cls ir.Cls) ir.Ref {
	ref := f.NewTemp(fmt.Sprintf("R%s", r.xName()), cls)
	t := f.Temp(ref)
	t.Fixed = true
	t.Reg = int(r)
	return ref
}

// lowerABI makes the calling convention explicit: parameters are copied out of
// their incoming registers, return values into x0, and call arguments/results
// through the argument/result registers.
func lowerABI(f *ir.Func) error {
	if err := lowerTailCalls(f); err != nil {
		return err
	}
	// A function returning a > 16-byte aggregate is given the result buffer's
	// address in x8; capture it at entry so the returns can write there.
	var retBuf ir.Ref
	if f.RetAgg != nil {
		if f.UsesGoInternalCallConvention() {
			resultAssigner := newArgAssigner(true)
			_, onStack, _ := assignGoAggregate(&resultAssigner, f.RetAgg)
			if onStack {
				retBuf = f.NewTemp("retbuf", ir.ClsL)
				f.Temp(retBuf).Agg = f.RetAgg
			}
		} else if classifyAgg(f.RetAgg).kind == aggMemory {
			retBuf = f.NewTemp("retbuf", ir.ClsL)
		}
	}
	if !retBuf.IsNone() && f.UsesManagedFrame() {
		// Under a managed (growable, copied) stack the hidden result buffer points
		// into the caller's stack and must be rewritten if a nested call grows and
		// copies it, so the collector has to track it. A fixed C/Ruby stack never
		// moves, so the pointer is an ordinary address needing no GC identity.
		f.MarkGCRef(retBuf)
	}
	if err := lowerParams(f, retBuf); err != nil {
		return err
	}
	if err := lowerCalls(f); err != nil {
		return err
	}
	return lowerReturns(f, retBuf)
}

// lowerTailCalls prepares tail-call blocks: the callee returns the function's
// result directly, so the block's own return move is dropped and the call's
// result is left uncaptured. The actual tail branch (frame teardown + b) is
// emitted later. A tail-marked call anywhere but in tail position is rejected.
func lowerTailCalls(f *ir.Func) error {
	for _, b := range f.Blocks {
		call, ok := ir.TailCall(b)
		if !ok {
			if ir.HasTailCall(b) {
				return fmt.Errorf("arm64: tail call must be the last instruction and have its result returned")
			}
			continue
		}
		if call.RetAgg != nil {
			return fmt.Errorf("arm64: aggregate-returning tail call is not supported")
		}
		b.Jmp = ir.Jmp{Kind: ir.JmpRet} // the callee provides the return value
		call.To = ir.R                  // do not capture the result
	}
	return nil
}

func lowerParams(f *ir.Func, retBuf ir.Ref) error {
	a := newArgAssigner(f.UsesGoInternalCallConvention())
	// Register/stack parameter moves (OPar) must precede any aggregate
	// reconstruction, so the emitter can treat the OPar run as one parallel move.
	var pars, recon []ir.Instr
	if !retBuf.IsNone() && !f.UsesGoInternalCallConvention() {
		// The indirect-result pointer arrives in x8, alongside the arguments.
		pin := newPinned(f, X8, ir.ClsL)
		pars = append(pars, ir.Instr{Op: ir.OPar, Cls: ir.ClsL, To: retBuf, Args: []ir.Ref{pin}})
	}
	for parameterIndex := 0; parameterIndex < len(f.Params); {
		if group, ok := valueGroupAt(f.ParamGroups, parameterIndex); f.UsesGoInternalCallConvention() && ok {
			end := parameterIndex + group.Count
			if end > len(f.Params) {
				return fmt.Errorf("arm64: aggregate parameter group extends beyond parameter list")
			}
			groupParameters, err := lowerGoValueParams(f, f.Params[parameterIndex:end], group.Type, &a)
			if err != nil {
				return err
			}
			pars = append(pars, groupParameters...)
			parameterIndex = end
			continue
		}
		if group, ok := valueGroupAt(f.ParamGroups, parameterIndex); !f.UsesGoInternalCallConvention() && ok {
			classification := classifyAgg(group.Type)
			if classification.kind != aggMemory {
				end := parameterIndex + group.Count
				if end > len(f.Params) {
					return fmt.Errorf("arm64: aggregate parameter group extends beyond parameter list")
				}
				groupParameters, err := lowerGoValueParams(f, f.Params[parameterIndex:end], group.Type, &a)
				if err != nil {
					return err
				}
				pars = append(pars, groupParameters...)
				parameterIndex = end
				continue
			}
		}

		p := f.Params[parameterIndex]
		if p.Agg != nil {
			ps, rs, err := lowerAggParam(f, p, &a)
			if err != nil {
				return err
			}
			pars = append(pars, ps...)
			recon = append(recon, rs...)
			parameterIndex++
			continue
		}
		loc := a.assign(p.Cls)
		if loc.onStack {
			pars = append(pars, ir.Instr{Op: ir.OPar, Cls: p.Cls, To: p.Ref(), Aux: int64(loc.stacky)})
			parameterIndex++
			continue
		}
		pin := newPinned(f, loc.reg, p.Cls)
		pars = append(pars, ir.Instr{Op: ir.OPar, Cls: p.Cls, To: p.Ref(), Args: []ir.Ref{pin}})
		parameterIndex++
	}
	if !retBuf.IsNone() && f.UsesGoInternalCallConvention() {
		resultAssigner := newArgAssigner(true)
		resultAssigner.nsaa = roundUp(a.nsaa, 8)
		_, onStack, resultOffset := assignGoAggregate(&resultAssigner, f.RetAgg)
		if !onStack {
			return fmt.Errorf("arm64: internal error: Go ABI result buffer assigned to registers")
		}
		pars = append(pars, ir.Instr{
			Op:     ir.OPar,
			Cls:    ir.ClsL,
			To:     retBuf,
			Aux:    int64(resultOffset),
			RetAgg: f.RetAgg,
		})
	}
	prefix := append(pars, recon...)
	f.Start.Instrs = append(prefix, f.Start.Instrs...)
	return nil
}

// lowerAggParam handles one by-value aggregate parameter, returning the OPar
// instructions (for a Memory-class pointer) and the reconstruction instructions
// (for register-class aggregates, which are rebuilt into a stack slot).
func lowerAggParam(f *ir.Func, p *ir.Temp, a *argAssigner) (pars, recon []ir.Instr, err error) {
	if f.UsesGoInternalCallConvention() {
		return lowerGoAggregateParam(f, p, a)
	}
	// Aggregate parameters are represented by an address. Depending on their
	// classification, it points into this frame, the incoming argument area, or
	// caller-owned storage. Only a managed (copying) stack must relocate it and so
	// track it; a fixed C/Ruby stack leaves it an ordinary address.
	if f.UsesManagedFrame() {
		f.MarkGCRef(p.Ref())
	}
	cls := classifyAgg(p.Agg)
	if cls.kind == aggMemory {
		// Passed by reference: the incoming value is already a pointer.
		loc := a.assign(ir.ClsL)
		if loc.onStack {
			return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Aux: int64(loc.stacky)}}, nil, nil
		}
		pin := newPinned(f, loc.reg, ir.ClsL)
		return []ir.Instr{{Op: ir.OPar, Cls: ir.ClsL, To: p.Ref(), Args: []ir.Ref{pin}}}, nil, nil
	}

	var regs []Reg
	var elemCls ir.Cls
	var elemSize int
	var onStack bool
	var off int
	if cls.kind == aggGP {
		regs, onStack, off = a.assignGP(cls.nregs, cls.size)
		elemCls, elemSize = ir.ClsL, 8
	} else {
		regs, onStack, off = a.assignHFA(cls.nregs, cls.size)
		elemCls, elemSize = cls.elem, cls.elem.Size()
	}
	if onStack {
		// The aggregate sits in the incoming argument area; the parameter is its
		// address (emitStackParam takes the address for aggregate temps).
		return []ir.Instr{{
			Op:     ir.OPar,
			Cls:    ir.ClsL,
			To:     p.Ref(),
			Aux:    int64(off),
			RetAgg: p.Agg,
		}}, nil, nil
	}

	// Reconstruct: allocate a slot, store each incoming register into it, and
	// make the parameter point at the slot.
	slot := aggregateAlloc(f, p.Agg, &recon)
	pointerAt := make(map[int]bool)
	for _, offset := range goAggregatePointerOffsets(p.Agg) {
		pointerAt[offset] = true
	}
	for i, r := range regs {
		pin := newPinned(f, r, elemCls)
		if pointerAt[i*elemSize] && f.UsesManagedFrame() {
			f.MarkGCRef(pin)
		}
		addr := slot
		if i > 0 {
			tmp := f.NewTemp("", ir.ClsL)
			recon = append(recon, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: tmp, Args: []ir.Ref{slot, f.Long(int64(i * elemSize))}})
			addr = tmp
		}
		recon = append(recon, ir.Instr{Op: storeOpFor(elemCls), Cls: elemCls, Args: []ir.Ref{pin, addr}})
	}
	recon = append(recon, ir.Instr{Op: ir.OCopy, Cls: ir.ClsL, To: p.Ref(), Args: []ir.Ref{slot}})
	return nil, recon, nil
}

func lowerReturns(f *ir.Func, retBuf ir.Ref) error {
	for _, b := range f.Blocks {
		if b.Jmp.Kind != ir.JmpRet || b.Jmp.Arg.IsNone() {
			continue
		}
		if f.RetAgg != nil {
			if f.UsesGoInternalCallConvention() {
				var err error
				if f.RetValues {
					err = lowerGoValueReturn(f, b, retBuf)
				} else {
					err = lowerGoAggregateReturn(f, b, retBuf)
				}
				if err != nil {
					return err
				}
			} else if f.RetValues {
				if err := lowerAAPCSValueReturn(f, b, retBuf); err != nil {
					return err
				}
			} else {
				lowerAggReturn(f, b, retBuf)
			}
			continue
		}
		v := b.Jmp.Arg
		cls := f.ClassOf(v)
		pin := newPinned(f, retReg(cls), cls)
		b.Instrs = append(b.Instrs, ir.Instr{Op: ir.OCopy, Cls: cls, To: pin, Args: []ir.Ref{v}})
		b.Jmp.Arg = pin
	}
	return nil
}

// lowerAggReturn returns an aggregate: a Memory-class result is copied into the
// caller's buffer (x8) and the function returns void; a register-class result is
// loaded from the pointer into the return registers.
func lowerAggReturn(f *ir.Func, b *ir.Block, retBuf ir.Ref) {
	ptr := b.Jmp.Arg
	cls := classifyAgg(f.RetAgg)
	if cls.kind == aggMemory {
		emitMemcpy(f, retBuf, ptr, cls.size, &b.Instrs)
		b.Jmp = ir.Jmp{Kind: ir.JmpRet}
		return
	}

	var base Reg
	var elemCls ir.Cls
	var elemSize int
	if cls.kind == aggGP {
		base, elemCls, elemSize = X0, ir.ClsL, 8
	} else {
		base, elemCls, elemSize = V0, cls.elem, cls.elem.Size()
	}
	var pins []ir.Ref
	for i := 0; i < cls.nregs; i++ {
		addr := returnFieldAddress(f, ptr, i*elemSize, &b.Instrs)
		pin := newPinned(f, base+Reg(i), elemCls)
		b.Instrs = append(b.Instrs, ir.Instr{Op: loadOpFor(elemCls), Cls: elemCls, To: pin, Args: []ir.Ref{addr}})
		pins = append(pins, pin)
	}
	b.Jmp.Arg = pins[0]
	b.Jmp.Args = pins[1:]
}

func lowerCalls(f *ir.Func) error {
	for _, b := range f.Blocks {
		var out []ir.Instr
		for i := range b.Instrs {
			in := b.Instrs[i]
			if in.Op != ir.OCall {
				out = append(out, in)
				continue
			}
			callee := in.Args[0]
			callArgs := in.Args[1:]
			goInternal := callUsesGoInternal(f, &in)
			pins := make([]ir.Ref, 0, len(callArgs))
			var argSetup []ir.Instr // the OArg run, emitted after value computation
			a := newArgAssigner(goInternal)
			for argumentIndex := 0; argumentIndex < len(callArgs); {
				if group, ok := valueGroupAt(in.ArgGroups, argumentIndex); goInternal && ok {
					end := argumentIndex + group.Count
					if end > len(callArgs) {
						return fmt.Errorf("arm64: aggregate argument group extends beyond call argument list")
					}
					var err error
					argSetup, pins, err = lowerGoValueArg(f, callArgs[argumentIndex:end], group.Type, &a, argSetup, pins)
					if err != nil {
						return err
					}
					argumentIndex = end
					continue
				}

				arg := callArgs[argumentIndex]
				if agg := aggArgAt(&in, argumentIndex); agg != nil {
					var err error
					argSetup, pins, err = lowerAggArg(f, arg, agg, goInternal, &a, &out, argSetup, pins)
					if err != nil {
						return err
					}
					argumentIndex++
					continue
				}
				if arg.Kind == ir.RefAggregate {
					calleeName := "indirect call"
					if callee.Kind == ir.RefConst {
						constant := f.Consts[callee.ID]
						if constant.Kind == ir.ConstSym {
							calleeName = constant.Sym
						}
					}
					return fmt.Errorf("arm64: ungrouped aggregate call argument %d to %s in %s", argumentIndex, calleeName, f.Name)
				}
				cls := f.ClassOf(arg)
				loc := a.assign(cls)
				if loc.onStack {
					argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: cls, To: ir.R, Aux: int64(loc.stacky), Args: []ir.Ref{arg}})
					argumentIndex++
					continue
				}
				pin := newPinned(f, loc.reg, cls)
				argSetup = append(argSetup, ir.Instr{Op: ir.OArg, Cls: cls, To: pin, Args: []ir.Ref{arg}})
				pins = append(pins, pin)
				argumentIndex++
			}
			// Result handling: build the call's To/Defs and the post-call
			// instructions, possibly adding an x8 setup to the argument run.
			var callTo ir.Ref
			var callCls ir.Cls
			var callDefs []ir.Ref
			var post []ir.Instr
			var stackResult ir.Ref
			var stackResultOffset int
			resultEnd := roundUp(a.nsaa, 8)
			switch {
			case in.To.IsNone():
				// no result
			case in.RetAgg != nil:
				if goInternal {
					var err error
					if in.RetValues {
						destinations := append([]ir.Ref{in.To}, in.Defs...)
						callTo, callCls, callDefs, argSetup, pins, post, stackResult, stackResultOffset, resultEnd, err =
							lowerGoValueResult(f, destinations, in.RetAgg, resultEnd, &out, argSetup, pins)
					} else {
						callTo, callCls, callDefs, argSetup, pins, post, stackResult, stackResultOffset, resultEnd, err =
							lowerGoAggregateResult(f, in.To, in.RetAgg, resultEnd, &out, argSetup, pins)
					}
					if err != nil {
						return err
					}
				} else if in.RetValues {
					var err error
					destinations := append([]ir.Ref{in.To}, in.Defs...)
					callTo, callCls, callDefs, argSetup, pins, post, err =
						lowerAAPCSValueResult(f, destinations, in.RetAgg, &a, &out, argSetup, pins)
					if err != nil {
						return err
					}
				} else {
					callTo, callCls, callDefs, argSetup, pins, post =
						lowerAggResult(f, in.To, in.RetAgg, &a, &out, argSetup, pins)
				}
			default:
				pres := newPinned(f, retReg(in.Cls), in.Cls)
				callTo, callCls = pres, in.Cls
				post = []ir.Instr{{Op: ir.OCopy, Cls: in.Cls, To: in.To, Args: []ir.Ref{pres}}}
			}

			if in.Tail {
				if goInternal != f.UsesGoInternalCallConvention() {
					return fmt.Errorf("arm64: cannot tail-call across calling conventions")
				}
				// A tail call writes its stack arguments into this function's own
				// incoming-argument area (reused after the frame is torn down), so
				// the callee's stack args must fit in the space our caller gave us.
				_, _, inStack := computeNamedCounts(f)
				if incoming := roundUp(inStack, 16); a.stackBytes() > incoming {
					return fmt.Errorf("arm64: tail call needs %d bytes of stack arguments but the caller provides only %d; cannot tail-call", a.stackBytes(), incoming)
				}
			}
			out = append(out, argSetup...)
			stackBytes := a.stackBytes()
			if goInternal {
				stackBytes = goCallStackBytes(f, &in, resultEnd)
			} else if f.UsesManagedFrame() {
				stackBytes = aapcsCallStackBytes(f, &in)
			}
			loweredCall := ir.Instr{
				Op: ir.OCall, Args: append([]ir.Ref{callee}, pins...),
				Aux: int64(stackBytes), To: callTo, Cls: callCls, Defs: callDefs,
				Tail: in.Tail, ClosureCall: in.ClosureCall, ClosureContext: in.ClosureContext,
				Pos: in.Pos, Inl: in.Inl,
				CallConv: ir.CallConvPlatform, CallConvSet: true,
			}
			if goInternal {
				loweredCall.CallConv = ir.CallConvGoInternal
			}
			if !stackResult.IsNone() {
				loweredCall.RetAgg = in.RetAgg
				loweredCall.StackResult = stackResult
				loweredCall.StackResultOffset = int64(stackResultOffset)
			}
			out = append(out, loweredCall)
			out = append(out, post...)
		}
		b.Instrs = out
	}
	return nil
}

func lowerAAPCSValueResult(
	function *ir.Func,
	destinations []ir.Ref,
	aggregate *ir.AggType,
	assigner *argAssigner,
	out *[]ir.Instr,
	setup []ir.Instr,
	pins []ir.Ref,
) (ir.Ref, ir.Cls, []ir.Ref, []ir.Instr, []ir.Ref, []ir.Instr, error) {
	parts, flattenable := flattenGoAggregate(aggregate)
	if !flattenable || len(parts) != len(destinations) {
		return ir.R, 0, nil, nil, nil, nil,
			fmt.Errorf("arm64: AAPCS aggregate result has %d SSA parts, want %d", len(destinations), len(parts))
	}
	for index, destination := range destinations {
		class := function.ClassOf(destination)
		if class != parts[index].sub.Cls() {
			return ir.R, 0, nil, nil, nil, nil,
				fmt.Errorf("arm64: AAPCS aggregate result part %d has class %s, want %s", index, class, parts[index].sub.Cls())
		}
	}

	resultPointer := function.NewTemp("", ir.ClsL)
	callTo, callClass, definitions, setup, pins, post :=
		lowerAggResult(function, resultPointer, aggregate, assigner, out, setup, pins)
	for index, part := range parts {
		address := offsetAddr(function, resultPointer, part.offset, &post)
		post = append(post, ir.Instr{
			Op:   loadOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			To:   destinations[index],
			Args: []ir.Ref{address},
		})
	}
	return callTo, callClass, definitions, setup, pins, post, nil
}

func lowerAAPCSValueReturn(function *ir.Func, block *ir.Block, resultBuffer ir.Ref) error {
	values := append([]ir.Ref{block.Jmp.Arg}, block.Jmp.Args...)
	parts, flattenable := flattenGoAggregate(function.RetAgg)
	if !flattenable || len(parts) != len(values) {
		return fmt.Errorf("arm64: AAPCS aggregate return has %d SSA parts, want %d", len(values), len(parts))
	}

	resultPointer := aggregateAlloc(function, function.RetAgg, &block.Instrs)
	for index, part := range parts {
		value := values[index]
		class := function.ClassOf(value)
		if class != part.sub.Cls() {
			return fmt.Errorf("arm64: AAPCS aggregate return part %d has class %s, want %s", index, class, part.sub.Cls())
		}
		address := offsetAddr(function, resultPointer, part.offset, &block.Instrs)
		block.Instrs = append(block.Instrs, ir.Instr{
			Op:   storeOpForSub(part.sub),
			Cls:  part.sub.Cls(),
			Args: []ir.Ref{value, address},
		})
	}
	block.Jmp.Arg = resultPointer
	block.Jmp.Args = nil
	lowerAggReturn(function, block, resultBuffer)
	return nil
}

func callUsesGoInternal(function *ir.Func, call *ir.Instr) bool {
	if call.CallConvSet {
		return call.CallConv == ir.CallConvGoInternal
	}
	return function.UsesGoInternalCallConvention()
}

// lowerTLS rewrites references to thread-local variables into instructions the
// register allocator can see.
//
// A thread-local has no address of its own, and how one is reached depends on the
// model. Local-exec is a link-time constant that fits in a single register, so the
// emitter can improvise it. The others cannot be improvised: initial-exec adds the
// thread pointer to an offset -- two registers at once -- and general-dynamic ends
// in a call, which would clobber whatever the allocator happened to be keeping in
// caller-saved registers. Turning the access into ordinary instructions here lets
// the allocator supply the registers and (for the call) spill across it, instead
// of an addressing helper doing either behind its back.
func lowerTLS(f *ir.Func, model TLSModel) {
	if model == TLSLocalExec {
		return // a link-time constant, and one register is enough to build it
	}
	for _, b := range f.Blocks {
		out := make([]ir.Instr, 0, len(b.Instrs))
		for _, in := range b.Instrs {
			for i, a := range in.Args {
				c, ok := threadConst(f, a)
				if !ok {
					continue
				}
				var addr ir.Ref
				if model == TLSGeneralDynamic {
					// The variable's storage may not exist in this thread yet, so ask
					// __tls_get_addr for the address, handing it the descriptor. The call
					// is an ordinary one: the allocator sees it and spills across it.
					idx := f.NewTemp("", ir.ClsL)
					addr = f.NewTemp("", ir.ClsL)
					out = append(out,
						ir.Instr{Op: ir.OTLSIndexAddr, Cls: ir.ClsL, To: idx, Args: []ir.Ref{a}, Pos: in.Pos},
						ir.Instr{Op: ir.OCall, Cls: ir.ClsL, To: addr, Pos: in.Pos,
							Args: []ir.Ref{f.Sym("__tls_get_addr", 0), idx}},
					)
				} else {
					// addr = thread_pointer + offset(sym), an ordinary add of two temps.
					off := f.NewTemp("", ir.ClsL)
					tp := f.NewTemp("", ir.ClsL)
					addr = f.NewTemp("", ir.ClsL)
					out = append(out,
						ir.Instr{Op: ir.OTLSOffset, Cls: ir.ClsL, To: off, Args: []ir.Ref{a}, Pos: in.Pos},
						ir.Instr{Op: ir.OThreadPtr, Cls: ir.ClsL, To: tp, Pos: in.Pos},
						ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: addr, Args: []ir.Ref{tp, off}, Pos: in.Pos},
					)
				}
				if c.Int != 0 {
					sum := f.NewTemp("", ir.ClsL)
					out = append(out, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: sum,
						Args: []ir.Ref{addr, f.Long(c.Int)}, Pos: in.Pos})
					addr = sum
				}
				in.Args[i] = addr
			}
			out = append(out, in)
		}
		b.Instrs = out
	}
}

// threadConst reports whether ref names a thread-local symbol, and returns it.
func threadConst(f *ir.Func, ref ir.Ref) (ir.Const, bool) {
	if ref.Kind != ir.RefConst {
		return ir.Const{}, false
	}
	c := f.Consts[ref.ID]
	if c.Kind != ir.ConstSym || !c.Thread {
		return ir.Const{}, false
	}
	return c, true
}
