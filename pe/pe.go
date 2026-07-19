// Package pe is an experimental partial evaluator: the first Futamura projection.
// Given an interpreter (as cg12 IR) and a fixed program for it (a byte slice), it
// specializes the interpreter with respect to that program, producing a residual
// IR function that computes the same result for any runtime input -- the compiled
// form of the program.
//
// The engine symbolically executes the interpreter IR over a small abstract
// domain: every value is green (static -- a known constant or an address into a
// known region) or red (dynamic -- a value known only at run time, emitted as an
// instruction in the residual). Green branches are followed; the interpreter's
// dispatch resolves because the opcode it reads from the program is green. The
// operand stack lives in green-addressed memory, so pushes and pops cancel and the
// stack disappears; only genuinely runtime data survives into the residual.
//
// This is a proof of concept: it handles the straight-line integer subset a small
// stack machine needs (see the toy interpreter in the test). Data-dependent
// control flow in the program -- residualized branches and loop-closing merge
// points -- is future work.
package pe

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// MergePoint is the marker the interpreter calls at its dispatch head to name the
// green loop variables; the engine treats the call as a no-op.
const MergePoint = "__cg12_merge_point"

// vkind classifies an abstract value.
type vkind int

const (
	vConst vkind = iota // a known integer (green)
	vAddr               // a known address: a region and a static byte offset (green)
	vResid              // a runtime value: a reference in the residual function (red)
)

// value is the abstract value the engine tracks for an IR ref.
type value struct {
	kind vkind
	k    int64   // vConst: the integer
	reg  *region // vAddr: the base region
	off  int64   // vAddr: the byte offset within reg
	ref  ir.Ref  // vResid: the residual reference
	cls  ir.Cls  // width/class, for materializing a residual operand
}

// rkind classifies a memory region.
type rkind int

const (
	rMem   rkind = iota // an interpreter alloca: a green read/write store
	rCode               // the program bytes: green, read-only
	rInput              // a runtime input array: a red load becomes a scalar param
)

type region struct {
	kind rkind
	id   int
}

type engine struct {
	src  *ir.Func       // the interpreter
	prog []byte         // the program to specialize against
	out  *ir.Func       // the residual being built
	blk  *ir.Block      // the residual's (single) block
	env  map[uint32]value    // interpreter temp id -> abstract value
	mem  map[[2]int64]value  // (region id, offset) -> stored value
	ins  map[int64]ir.Ref    // input byte offset -> synthesized residual param
	inOrder []int64          // input offsets, in the order params were created
	nreg int
	err  error
}

// Specialize specializes the named interpreter in m against prog, returning a new
// module holding the residual function (same name, suffixed) and its name. The
// residual takes one scalar parameter per distinct runtime input the program
// reads, in first-seen order (see Inputs on the returned function's params).
func Specialize(m *ir.Module, interp string, prog []byte) (*ir.Module, string, []int64, error) {
	var src *ir.Func
	for _, f := range m.Funcs {
		if f.Name == interp {
			src = f
			break
		}
	}
	if src == nil {
		return nil, "", nil, fmt.Errorf("pe: no function %q", interp)
	}
	out := ir.NewModule()
	name := interp + "$spec"
	e := &engine{
		src:  src,
		prog: prog,
		out:  out.NewFunc(name, src.Retty),
		env:  map[uint32]value{},
		mem:  map[[2]int64]value{},
		ins:  map[int64]ir.Ref{},
	}
	e.out.Export()
	e.blk = e.out.Entry()
	e.run()
	if e.err != nil {
		return nil, "", nil, e.err
	}
	return out, name, e.inOrder, nil
}

func (e *engine) fail(format string, a ...any) {
	if e.err == nil {
		e.err = fmt.Errorf("pe: "+format, a...)
	}
}

// run symbolically executes the interpreter from its entry, following green
// control flow, until it returns.
func (e *engine) run() {
	// The interpreter's pointer parameters: the one named "code" is the green
	// program; any other is a runtime input array (red).
	for i, t := range e.src.Params {
		r := ir.Ref{Kind: ir.RefTemp, ID: uint32(t.ID)}
		if t.Name == "code" {
			e.env[uint32(t.ID)] = value{kind: vAddr, reg: e.newRegion(rCode)}
		} else {
			e.env[uint32(t.ID)] = value{kind: vAddr, reg: e.newRegion(rInput)}
		}
		_ = i
		_ = r
	}

	blk := e.src.Blocks[0]
	const maxSteps = 1_000_000
	for step := 0; step < maxSteps; step++ {
		for k := range blk.Instrs {
			e.exec(&blk.Instrs[k])
			if e.err != nil {
				return
			}
		}
		next := e.terminator(blk)
		if next == nil {
			return // returned, or errored
		}
		blk = next
	}
	e.fail("did not terminate within %d steps (a program loop needs merge points)", maxSteps)
}

func (e *engine) newRegion(k rkind) *region {
	e.nreg++
	return &region{kind: k, id: e.nreg}
}

// terminator resolves a block's terminator and returns the next block, or nil
// when the interpreter returns (emitting the residual's return).
func (e *engine) terminator(b *ir.Block) *ir.Block {
	switch b.Jmp.Kind {
	case ir.JmpJmp:
		return b.Jmp.To
	case ir.JmpSwitch:
		v := e.valueOf(b.Jmp.Arg)
		if v.kind != vConst {
			e.fail("switch on a non-green value")
			return nil
		}
		for _, c := range b.Jmp.Cases {
			if c.Val == v.k {
				return c.Blk
			}
		}
		return b.Jmp.To // default
	case ir.JmpJnz:
		v := e.valueOf(b.Jmp.Arg)
		if v.kind != vConst {
			e.fail("data-dependent branch is not yet supported (needs residual control flow)")
			return nil
		}
		if v.k != 0 {
			return b.Jmp.To
		}
		return b.Jmp.To2
	case ir.JmpRet:
		e.blk.Ret(e.materialize(e.valueOf(b.Jmp.Arg)))
		return nil
	default:
		e.fail("unsupported terminator %v", b.Jmp.Kind)
		return nil
	}
}

func (e *engine) valueOf(r ir.Ref) value {
	switch r.Kind {
	case ir.RefConst:
		c := e.src.Consts[r.ID]
		return value{kind: vConst, k: c.Int, cls: c.Cls}
	case ir.RefTemp:
		v, ok := e.env[r.ID]
		if !ok {
			e.fail("use of an undefined temporary %%%d", r.ID)
		}
		return v
	}
	return value{}
}

func (e *engine) set(r ir.Ref, v value) {
	if r.Kind == ir.RefTemp {
		e.env[r.ID] = v
	}
}

// exec symbolically executes one instruction.
func (e *engine) exec(in *ir.Instr) {
	switch {
	case in.Op.IsAlloc():
		e.set(in.To, value{kind: vAddr, reg: e.newRegion(rMem)})
	case in.Op.IsStore():
		e.store(e.valueOf(in.Args[1]), e.valueOf(in.Args[0]), storeBytes(in.Op))
	case in.Op.IsLoad():
		e.set(in.To, e.load(in.Op, e.valueOf(in.Args[0])))
	default:
		e.execData(in)
	}
}

func (e *engine) execData(in *ir.Instr) {
	switch in.Op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar:
		e.set(in.To, e.binop(in))
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw, ir.OCopy:
		e.set(in.To, e.extend(in))
	case ir.OCall:
		if e.calleeName(in) == MergePoint {
			return // the merge-point marker is a no-op here
		}
		e.fail("call to %q is not supported by the specializer", e.calleeName(in))
	default:
		e.fail("unsupported op %v", in.Op)
	}
}

// binop evaluates a binary op: constant-folded when both operands are green,
// address arithmetic when one is a known address, else residualized.
func (e *engine) binop(in *ir.Instr) value {
	a, b := e.valueOf(in.Args[0]), e.valueOf(in.Args[1])
	if a.kind == vConst && b.kind == vConst {
		return value{kind: vConst, k: fold(in.Op, a.k, b.k), cls: in.Cls}
	}
	if in.Op == ir.OAdd {
		if a.kind == vAddr && b.kind == vConst {
			return value{kind: vAddr, reg: a.reg, off: a.off + b.k}
		}
		if a.kind == vConst && b.kind == vAddr {
			return value{kind: vAddr, reg: b.reg, off: b.off + a.k}
		}
	}
	res := e.emitBin(in.Op, in.Cls, e.materialize(a), e.materialize(b))
	return value{kind: vResid, ref: res, cls: in.Cls}
}

func (e *engine) extend(in *ir.Instr) value {
	a := e.valueOf(in.Args[0])
	switch a.kind {
	case vConst:
		return value{kind: vConst, k: applyExt(in.Op, a.k), cls: in.Cls}
	case vAddr:
		return a // extending/copying an address leaves the region and offset
	default:
		if in.Op == ir.OCopy {
			return value{kind: vResid, ref: a.ref, cls: in.Cls}
		}
		res := e.emitExt(in.Op, in.Cls, e.materialize(a))
		return value{kind: vResid, ref: res, cls: in.Cls}
	}
}

// store writes val at a green address, or emits a residual store for a red one.
func (e *engine) store(addr, val value, _ int) {
	if addr.kind != vAddr {
		e.fail("store to a runtime address is not yet supported")
		return
	}
	switch addr.reg.kind {
	case rMem:
		e.mem[[2]int64{int64(addr.reg.id), addr.off}] = val
	case rCode:
		e.fail("the program is read-only")
	case rInput:
		e.fail("storing into a runtime input is not supported")
	}
}

// load reads from a green address: the stored value for an alloca, a program byte
// for the code, or a synthesized parameter for a runtime input.
func (e *engine) load(op ir.Op, addr value) value {
	if addr.kind != vAddr {
		e.fail("load from a runtime address is not yet supported")
		return value{}
	}
	switch addr.reg.kind {
	case rMem:
		v, ok := e.mem[[2]int64{int64(addr.reg.id), addr.off}]
		if !ok {
			return value{kind: vConst, k: 0, cls: loadCls(op)} // uninitialized reads as zero
		}
		return v
	case rCode:
		k, ok := e.readProg(op, addr.off)
		if !ok {
			e.fail("program read at offset %d is out of range", addr.off)
			return value{}
		}
		return value{kind: vConst, k: k, cls: loadCls(op)}
	case rInput:
		return value{kind: vResid, ref: e.inputParam(addr.off, loadCls(op)), cls: loadCls(op)}
	}
	return value{}
}

// inputParam returns the residual scalar parameter standing for the runtime input
// at byte offset off, creating it on first use.
func (e *engine) inputParam(off int64, cls ir.Cls) ir.Ref {
	if r, ok := e.ins[off]; ok {
		return r
	}
	r := e.out.Param(fmt.Sprintf("in%d", off/8), cls)
	e.ins[off] = r
	e.inOrder = append(e.inOrder, off)
	return r
}

// materialize turns an abstract value into a residual reference: a constant for a
// green integer, the reference itself for a red value.
func (e *engine) materialize(v value) ir.Ref {
	switch v.kind {
	case vConst:
		cls := v.cls
		if cls == 0 {
			cls = ir.ClsL
		}
		return e.out.ConstInt(cls, v.k)
	case vResid:
		return v.ref
	default:
		e.fail("a green address escaped into a runtime value")
		return ir.R
	}
}

func (e *engine) emitBin(op ir.Op, cls ir.Cls, a, b ir.Ref) ir.Ref {
	switch op {
	case ir.OAdd:
		return e.blk.Add(cls, a, b)
	case ir.OSub:
		return e.blk.Sub(cls, a, b)
	case ir.OMul:
		return e.blk.Mul(cls, a, b)
	case ir.OAnd:
		return e.blk.And(cls, a, b)
	case ir.OOr:
		return e.blk.Or(cls, a, b)
	case ir.OXor:
		return e.blk.Xor(cls, a, b)
	case ir.OShl:
		return e.blk.Shl(cls, a, b)
	case ir.OShr:
		return e.blk.Shr(cls, a, b)
	case ir.OSar:
		return e.blk.Sar(cls, a, b)
	}
	e.fail("cannot residualize op %v", op)
	return ir.R
}

func (e *engine) emitExt(op ir.Op, cls ir.Cls, a ir.Ref) ir.Ref {
	switch op {
	case ir.OExtsb:
		return e.blk.Extsb(cls, a)
	case ir.OExtub:
		return e.blk.Extub(cls, a)
	case ir.OExtsh:
		return e.blk.Extsh(cls, a)
	case ir.OExtuh:
		return e.blk.Extuh(cls, a)
	case ir.OExtsw:
		return e.blk.Extsw(cls, a)
	case ir.OExtuw:
		return e.blk.Extuw(cls, a)
	}
	e.fail("cannot residualize extension %v", op)
	return ir.R
}

func (e *engine) calleeName(in *ir.Instr) string {
	if len(in.Args) == 0 || in.Args[0].Kind != ir.RefConst {
		return ""
	}
	c := e.src.Consts[in.Args[0].ID]
	if c.Kind != ir.ConstSym {
		return ""
	}
	return c.Sym
}

// readProg reads a load's worth of program bytes at off (little-endian), applying
// the load's width and signedness.
func (e *engine) readProg(op ir.Op, off int64) (int64, bool) {
	n := loadBytes(op)
	if off < 0 || int(off)+n > len(e.prog) {
		return 0, false
	}
	var u uint64
	for i := 0; i < n; i++ {
		u |= uint64(e.prog[int(off)+i]) << (8 * i)
	}
	if loadSigned(op) {
		shift := uint(64 - 8*n)
		return int64(u<<shift) >> shift, true
	}
	return int64(u), true
}

// fold evaluates a green binary op.
func fold(op ir.Op, a, b int64) int64 {
	switch op {
	case ir.OAdd:
		return a + b
	case ir.OSub:
		return a - b
	case ir.OMul:
		return a * b
	case ir.OAnd:
		return a & b
	case ir.OOr:
		return a | b
	case ir.OXor:
		return a ^ b
	case ir.OShl:
		return a << uint64(b)
	case ir.OShr:
		return int64(uint64(a) >> uint64(b))
	case ir.OSar:
		return a >> uint64(b)
	}
	return 0
}

// applyExt evaluates a green extension/copy.
func applyExt(op ir.Op, k int64) int64 {
	switch op {
	case ir.OExtsb:
		return int64(int8(k))
	case ir.OExtub:
		return int64(uint8(k))
	case ir.OExtsh:
		return int64(int16(k))
	case ir.OExtuh:
		return int64(uint16(k))
	case ir.OExtsw:
		return int64(int32(k))
	case ir.OExtuw:
		return int64(uint32(k))
	}
	return k // OCopy
}

func loadBytes(op ir.Op) int {
	switch op {
	case ir.OLoadsb, ir.OLoadub:
		return 1
	case ir.OLoadsh, ir.OLoaduh:
		return 2
	case ir.OLoadsw, ir.OLoaduw, ir.OLoads:
		return 4
	default:
		return 8
	}
}

func loadSigned(op ir.Op) bool {
	switch op {
	case ir.OLoadsb, ir.OLoadsh, ir.OLoadsw:
		return true
	}
	return false
}

func loadCls(op ir.Op) ir.Cls {
	switch op {
	case ir.OLoads:
		return ir.ClsS
	case ir.OLoadd:
		return ir.ClsD
	case ir.OLoadl:
		return ir.ClsL
	default:
		return ir.ClsW
	}
}

func storeBytes(op ir.Op) int {
	switch op {
	case ir.OStoreb:
		return 1
	case ir.OStoreh:
		return 2
	case ir.OStorew, ir.OStores:
		return 4
	default:
		return 8
	}
}
