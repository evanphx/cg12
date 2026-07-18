// Package lift rebuilds cg12 SSA IR from the arm64 machine code cg12 emits: it
// decodes each word (a64.Decode), models the machine registers as stack slots,
// lowers each instruction to load-operate-store on those slots, and lets
// opt.Mem2Reg promote them back to SSA. The recovered IR runs on the interpreter,
// so the whole round trip — IR → arm64 → IR′ — is checked by
// interp(IR) == interp(IR′) with no host or qemu.
//
// It lifts only what the backend emits (the subset a64.Decode recognizes). A word
// it cannot decode, or an instruction it cannot yet lift, is an error, not a
// guess.
package lift

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// numGPR is the count of general registers modeled (x0..x30); x31 is zr/sp and is
// handled without a slot.
const numGPR = 31

// Lift rebuilds an ir.Module with one function `name` from its machine code. The
// AArch64 ABI puts the arguments in x0.., so params are stored into those slots on
// entry and the result is read from x0 by the function's `ret`. retty is the
// return class (use ir.ClsV-style void via hasRet=false through a nil retty is not
// possible, so pass the concrete class and voidRet to choose).
func Lift(name string, code []uint32, params []ir.Cls, retty ir.Cls, voidRet bool) (*ir.Module, error) {
	m := ir.NewModule()
	sig := sigInfo{params: params, retty: retty, void: voidRet}
	if err := liftOne(m, name, code, sig, map[string]sigInfo{name: sig}, nil); err != nil {
		return nil, err
	}
	if err := ir.VerifyModule(m); err != nil {
		return nil, fmt.Errorf("lift %s: %w", name, err)
	}
	return m, nil
}

// FuncCode is one function to lift: its machine code, signature, and the callee
// each bl resolves to (by word index, from relocations). It is the input the
// foreign-code path (e.g. gcc output read from an ELF) supplies directly.
type FuncCode struct {
	Name   string
	Code   []uint32
	Params []ir.Cls
	Retty  ir.Cls
	Void   bool
	Calls  map[int]string // bl word index -> callee name
}

// LiftFuncs lifts a set of functions into one self-contained IR module. Calls
// resolve among the set by name, so recursion and mutual calls lift whole.
func LiftFuncs(funcs []FuncCode) (*ir.Module, error) {
	sigs := map[string]sigInfo{}
	for _, fc := range funcs {
		sigs[fc.Name] = sigInfo{params: fc.Params, retty: fc.Retty, void: fc.Void}
	}
	m := ir.NewModule()
	for _, fc := range funcs {
		if err := liftOne(m, fc.Name, fc.Code, sigs[fc.Name], sigs, fc.Calls); err != nil {
			return nil, err
		}
	}
	if err := ir.VerifyModule(m); err != nil {
		return nil, fmt.Errorf("lift module: %w", err)
	}
	return m, nil
}

// LiftModule lifts every function of a cg12-compiled object back into one IR
// module, taking signatures from the original module and resolving bl calls
// through the object's .text relocations.
func LiftModule(orig *ir.Module, o *obj.Object) (*ir.Module, error) {
	sig := map[string]*ir.Func{}
	for _, f := range orig.Funcs {
		sig[f.Name] = f
	}
	relocAt := map[uint64]string{}
	for _, r := range o.Relocs {
		relocAt[r.Offset] = r.Sym
	}

	var funcs []FuncCode
	for _, s := range o.Syms {
		f := sig[s.Name]
		if s.Section != obj.SecText || !s.Func || f == nil || f.Start == nil {
			continue
		}
		var ps []ir.Cls
		for _, p := range f.Params {
			ps = append(ps, p.Cls)
		}
		code := textWords(o.Text, s.Value, s.Size)
		calls := map[int]string{}
		for wi := range code {
			if name, ok := relocAt[s.Value+uint64(wi*4)]; ok {
				calls[wi] = name
			}
		}
		funcs = append(funcs, FuncCode{Name: s.Name, Code: code, Params: ps,
			Retty: f.Retty, Void: !f.HasRet, Calls: calls})
	}
	return LiftFuncs(funcs)
}

type sigInfo struct {
	params []ir.Cls
	retty  ir.Cls
	void   bool
}

// liftOne builds one function into an existing module.
func liftOne(out *ir.Module, name string, code []uint32, sig sigInfo, sigs map[string]sigInfo, callName map[int]string) error {
	insts := make([]a64.Inst, len(code))
	for i, w := range code {
		in, ok := a64.Decode(w)
		if !ok {
			return fmt.Errorf("lift %s: cannot decode word %d (%#08x)", name, i, w)
		}
		insts[i] = in
	}

	var f *ir.Func
	if sig.void {
		f = out.NewFuncVoid(name)
	} else {
		f = out.NewFunc(name, sig.retty)
	}
	f.Export()

	l := &lifter{f: f, insts: insts, voidRet: sig.void, paramCls: sig.params, sigs: sigs, callName: callName}
	for i, cls := range sig.params {
		l.params = append(l.params, f.Param(fmt.Sprintf("a%d", i), cls))
	}
	l.buildBlocks()
	return l.emit()
}

func textWords(text []byte, off, size uint64) []uint32 {
	b := text[off : off+size]
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
	}
	return out
}

type lifter struct {
	f        *ir.Func
	insts    []a64.Inst
	params   []ir.Ref
	paramCls []ir.Cls
	voidRet  bool

	reg      [numGPR]ir.Ref     // x0..x30 -> alloca slot
	vreg     [numGPR]ir.Ref     // v0..v30 -> alloca slot (floating point)
	spSlot   ir.Ref             // sp value (a pointer into the stack buffer)
	blockAt  map[int]*ir.Block  // word index -> block starting there
	leaders  []int              // sorted block-leader word indices
	pending  *pendingCmp        // last flag-setting compare, for cset/b.cond
	sigs     map[string]sigInfo // callee signatures, for bl
	callName map[int]string     // word index of a bl -> callee name (from relocs)
}

// pendingCmp holds the operands of the most recent compare, already narrowed to
// the compare width, so a following cset or b.cond can turn a condition into an
// ir.OCmp — cg12 never models NZCV, it recomputes the boolean.
type pendingCmp struct {
	a, b  ir.Ref
	float bool
}

// buildBlocks finds the block leaders (word 0, every branch target, and the word
// after any branch or return) and creates a block per leader.
func (l *lifter) buildBlocks() {
	leader := map[int]bool{0: true}
	for i := range l.insts {
		in := &l.insts[i]
		if tgt, ok := branchTarget(i, in); ok {
			leader[tgt] = true
		}
		if isBlockEnd(in.Op) && i+1 < len(l.insts) {
			leader[i+1] = true
		}
	}
	for idx := range leader {
		if idx >= 0 && idx < len(l.insts) {
			l.leaders = append(l.leaders, idx)
		}
	}
	sort.Ints(l.leaders)

	l.blockAt = make(map[int]*ir.Block, len(l.leaders))
	for _, idx := range l.leaders {
		l.blockAt[idx] = l.f.NewBlock(fmt.Sprintf("b%d", idx))
	}
}

// branchTarget returns the word index a branch instruction reaches, if it is a
// direct pc-relative branch.
func branchTarget(idx int, in *a64.Inst) (int, bool) {
	switch in.Op {
	case a64.OpB, a64.OpBcond, a64.OpCbz, a64.OpCbnz:
		return idx + int(in.Target)/4, true
	}
	return 0, false
}

func isBlockEnd(op a64.Mnemonic) bool {
	switch op {
	case a64.OpB, a64.OpBcond, a64.OpCbz, a64.OpCbnz, a64.OpRet, a64.OpBr:
		return true
	}
	return false
}

// blockRange returns the [start, end) word span of the block that starts at the
// given leader index.
func (l *lifter) blockRange(start int) (int, int) {
	for i, ld := range l.leaders {
		if ld == start {
			if i+1 < len(l.leaders) {
				return start, l.leaders[i+1]
			}
			return start, len(l.insts)
		}
	}
	return start, len(l.insts)
}
