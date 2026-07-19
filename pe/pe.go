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
// operand stack and local variables live in green-addressed memory, so their
// accesses fold away and only genuinely runtime data survives.
//
// Control flow in the program is handled by memoization. The interpreter marks its
// dispatch head with __cg12_merge_point(pc, sp), naming the green loop variables.
// The engine keeps one residual block per green state (pc, sp); a program branch on
// a runtime value residualizes to a real branch whose arms specialize both targets,
// and a back-edge that returns to a seen green state jumps to its block -- so a
// program loop becomes a residual loop, with the red state (stack cells, locals)
// carried as phis.
//
// This is a proof of concept over the integer subset a small stack machine needs.
// Interpreter memory is classified by the name cc gives each alloca: "stack" is the
// operand stack (sp deep), "pc"/"sp" are the green scalars, "code"/"in" hold the
// program and input base pointers, and anything else is a local-variable array
// whose cells are carried across merge points.
package pe

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/ir"
)

// MergePoint is the marker the interpreter calls at its dispatch head.
const MergePoint = "__cg12_merge_point"

type vkind int

const (
	vConst vkind = iota // a known integer (green)
	vAddr               // a known address: a region and a static byte offset (green)
	vResid              // a runtime value: a reference in the residual (red)
)

type value struct {
	kind vkind
	k    int64
	reg  *region
	off  int64
	ref  ir.Ref
	cls  ir.Cls
}

// role classifies what an interpreter memory region holds.
type role int

const (
	roleOther  role = iota // an alloca not otherwise recognized
	roleScalar             // the green pc or sp cell
	roleBase               // a held base pointer (code, in): a fixed address
	roleStack              // the operand stack (sp cells deep)
	roleVar                // a local-variable array (all cells carried across merges)
	roleCode               // the program bytes
	roleInput              // a runtime input array
)

type region struct {
	id   int
	role role
	size int64
}

// greenState is the memoization key: everything static that shapes the residual.
type greenState struct{ pc, sp int64 }

// state is the residual block for a green state and the phis carrying its red state.
type state struct {
	blk  *ir.Block
	phis []*ir.Phi
}

type engine struct {
	src, out *ir.Func
	prog     []byte

	cur     *ir.Block // residual block currently being emitted into
	env     map[uint32]value
	mem     map[[2]int64]value
	baseEnv map[uint32]value // the green state established by the interpreter's setup
	baseMem map[[2]int64]value
	atEntry bool // skip the merge point that heads the state being specialized

	ins     map[int64]ir.Ref // input byte offset -> synthesized residual param
	inOrder []int64

	nreg     int
	dispatch *ir.Block // the interpreter block holding the merge-point marker
	pcReg    *region
	spReg    *region
	stackReg *region
	varRegs  []*region

	states   map[greenState]*state
	work     []greenState
	regionOf map[uint32]*region // alloca temp id -> its region (stable across re-execution)
	nphi     int                // for distinct residual phi names
	err      error
}

// Specialize specializes the named interpreter in m against prog, returning a new
// module holding the residual function (same name, suffixed), its name, and the
// byte offsets of the runtime inputs it reads (in parameter order).
func Specialize(m *ir.Module, interp string, prog []byte) (*ir.Module, string, []int64, error) {
	var src *ir.Func
	for _, f := range m.Funcs {
		if f.Name == interp {
			src = f
		}
	}
	if src == nil {
		return nil, "", nil, fmt.Errorf("pe: no function %q", interp)
	}
	out := ir.NewModule()
	name := interp + "$spec"
	e := &engine{
		src:      src,
		prog:     prog,
		out:      out.NewFunc(name, src.Retty),
		env:      map[uint32]value{},
		mem:      map[[2]int64]value{},
		ins:      map[int64]ir.Ref{},
		states:   map[greenState]*state{},
		regionOf: map[uint32]*region{},
	}
	e.out.Export()
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

func (e *engine) run() {
	// The pointer parameters: "code" is the green program, others are runtime inputs.
	for _, t := range e.src.Params {
		r := roleInput
		if t.Name == "code" {
			r = roleCode
		}
		e.env[uint32(t.ID)] = value{kind: vAddr, reg: e.newRegion(r, 0)}
	}

	// The interpreter's setup runs once, into the entry block; it reaches the first
	// merge point, which becomes the residual's start state.
	e.cur = e.out.Entry()
	e.emit(e.src.Blocks[0])
	for len(e.work) > 0 && e.err == nil {
		gs := e.work[0]
		e.work = e.work[1:]
		e.specialize(gs)
	}
}

func (e *engine) newRegion(r role, size int64) *region {
	e.nreg++
	return &region{id: e.nreg, role: r, size: size}
}

// specialize emits the residual for one green state: reset the interpreter's
// memory to that state (green pc/sp, red cells bound to the block's phis), then
// symbolically execute the dispatch and its handler.
func (e *engine) specialize(gs greenState) {
	st := e.states[gs]
	e.env = copyEnv(e.baseEnv)
	e.mem = copyMem(e.baseMem)
	e.mem[cell(e.pcReg, 0)] = value{kind: vConst, k: gs.pc, cls: ir.ClsW}
	e.mem[cell(e.spReg, 0)] = value{kind: vConst, k: gs.sp, cls: ir.ClsW}
	e.bindRedState(gs.sp, st.phis)
	e.cur = st.blk
	e.atEntry = true
	e.emit(e.dispatch)
}

// emit symbolically executes interpreter block b (and its successors) into the
// current residual block, until the path returns or reaches a merge point.
func (e *engine) emit(b *ir.Block) {
	if e.err != nil {
		return
	}
	for k := range b.Instrs {
		in := &b.Instrs[k]
		if e.isMerge(in) {
			if e.atEntry {
				e.atEntry = false
				continue // this is the head of the state we are specializing
			}
			e.dispatch = b
			e.transition()
			return
		}
		e.exec(in)
		if e.err != nil {
			return
		}
	}
	e.control(b)
}

func (e *engine) isMerge(in *ir.Instr) bool {
	return in.Op == ir.OCall && e.calleeName(in) == MergePoint
}

// control follows a block's terminator, residualizing a runtime branch.
func (e *engine) control(b *ir.Block) {
	switch b.Jmp.Kind {
	case ir.JmpJmp:
		e.emit(b.Jmp.To)
	case ir.JmpSwitch:
		v := e.valueOf(b.Jmp.Arg)
		if v.kind != vConst {
			e.fail("switch on a runtime value")
			return
		}
		e.emit(e.caseOf(b, v.k))
	case ir.JmpJnz:
		c := e.valueOf(b.Jmp.Arg)
		if c.kind == vConst {
			if c.k != 0 {
				e.emit(b.Jmp.To)
			} else {
				e.emit(b.Jmp.To2)
			}
			return
		}
		// A branch on a runtime value: residualize, and specialize both targets.
		cond := e.materialize(c)
		bt, bf := e.out.NewBlock("bt"), e.out.NewBlock("bf")
		e.cur.Jnz(cond, bt, bf)
		env, mem := e.env, e.mem
		e.env, e.mem = copyEnv(env), copyMem(mem)
		e.cur = bt
		e.emit(b.Jmp.To)
		e.env, e.mem = env, mem
		e.cur = bf
		e.emit(b.Jmp.To2)
	case ir.JmpRet:
		e.cur.Ret(e.materialize(e.valueOf(b.Jmp.Arg)))
	default:
		e.fail("unsupported terminator %v", b.Jmp.Kind)
	}
}

func (e *engine) caseOf(b *ir.Block, v int64) *ir.Block {
	for _, c := range b.Jmp.Cases {
		if c.Val == v {
			return c.Blk
		}
	}
	return b.Jmp.To // default
}

// transition memoizes the merge point: jump the current block to the residual
// block for this green state (creating it, with phis for the red state, on first
// arrival), feeding the phis the red values carried on this edge.
func (e *engine) transition() {
	if e.baseMem == nil {
		e.baseEnv, e.baseMem = copyEnv(e.env), copyMem(e.mem)
	}
	sp := e.mem[cell(e.spReg, 0)].k
	gs := greenState{pc: e.mem[cell(e.pcReg, 0)].k, sp: sp}
	red := e.redState(sp)

	st, seen := e.states[gs]
	if !seen {
		st = &state{blk: e.out.NewBlock(fmt.Sprintf("s_pc%d_sp%d", gs.pc, sp))}
		for _, rv := range red {
			cls := rv.cls
			if cls == 0 {
				cls = ir.ClsL
			}
			p := &ir.Phi{Cls: cls, To: e.out.NewTemp(fmt.Sprintf("r%d", e.nphi), cls)}
			e.nphi++
			st.blk.Phis = append(st.blk.Phis, p)
			st.phis = append(st.phis, p)
		}
		e.states[gs] = st
		e.work = append(e.work, gs)
	}
	if len(st.phis) != len(red) {
		e.fail("merge point %v: red state changed size (%d vs %d)", gs, len(st.phis), len(red))
		return
	}
	for i, p := range st.phis {
		p.Args = append(p.Args, e.materialize(red[i]))
		p.Blocks = append(p.Blocks, e.cur)
	}
	e.cur.Goto(st.blk)
}

// redState is the vector of runtime values carried across a merge point: the live
// operand-stack cells (0..sp-1) then every local-variable cell, in a fixed order.
func (e *engine) redState(sp int64) []value {
	var red []value
	for i := int64(0); i < sp; i++ {
		red = append(red, e.load(ir.OLoadl, value{kind: vAddr, reg: e.stackReg, off: i * 8}))
	}
	for _, vr := range e.sortedVars() {
		for off := int64(0); off < vr.size; off += 8 {
			red = append(red, e.load(ir.OLoadl, value{kind: vAddr, reg: vr, off: off}))
		}
	}
	return red
}

// bindRedState installs the state block's phis as the current value of each red
// cell, in the same order redState produced them.
func (e *engine) bindRedState(sp int64, phis []*ir.Phi) {
	i := 0
	put := func(reg *region, off int64) {
		if i < len(phis) {
			e.mem[cell(reg, off)] = value{kind: vResid, ref: phis[i].To, cls: phis[i].Cls}
			i++
		}
	}
	for j := int64(0); j < sp; j++ {
		put(e.stackReg, j*8)
	}
	for _, vr := range e.sortedVars() {
		for off := int64(0); off < vr.size; off += 8 {
			put(vr, off)
		}
	}
}

func (e *engine) sortedVars() []*region {
	sort.Slice(e.varRegs, func(i, j int) bool { return e.varRegs[i].id < e.varRegs[j].id })
	return e.varRegs
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

func (e *engine) exec(in *ir.Instr) {
	switch {
	case in.Op.IsAlloc():
		e.set(in.To, value{kind: vAddr, reg: e.allocRegion(in)})
	case in.Op.IsStore():
		e.store(e.valueOf(in.Args[1]), e.valueOf(in.Args[0]))
	case in.Op.IsLoad():
		e.set(in.To, e.load(in.Op, e.valueOf(in.Args[0])))
	default:
		e.execData(in)
	}
}

// allocRegion classifies an interpreter alloca and returns its region, stable
// across re-execution (a handler's alloca runs once per specialized copy of that
// handler, but names the same storage). The name cc gives the result identifies
// the interpreter's structural regions; among the rest, a local array is
// persistent VM state carried across merge points, while a scalar is a handler
// temporary, plain memory that never crosses a merge.
func (e *engine) allocRegion(in *ir.Instr) *region {
	if in.To.Kind == ir.RefTemp {
		if r, ok := e.regionOf[in.To.ID]; ok {
			return r
		}
	}
	base := ""
	if t := e.src.Temp(in.To); t != nil {
		base = t.Name
	}
	if n := len(base); n > 5 && base[n-5:] == ".addr" {
		base = base[:n-5]
	}
	size := int64(0)
	if len(in.Args) > 0 && in.Args[0].Kind == ir.RefConst {
		size = e.src.Consts[in.Args[0].ID].Int
	}
	r := e.newRegion(roleOther, size)
	switch base {
	case "pc":
		r.role = roleScalar
		e.pcReg = r
	case "sp":
		r.role = roleScalar
		e.spReg = r
	case "stack":
		r.role = roleStack
		e.stackReg = r
	case "code", "in":
		r.role = roleBase
	default:
		if size > 8 { // a local array: persistent VM state carried across merges
			r.role = roleVar
			e.varRegs = append(e.varRegs, r)
		}
		// otherwise roleOther: a handler-local scalar, not carried across merges
	}
	if in.To.Kind == ir.RefTemp {
		e.regionOf[in.To.ID] = r
	}
	return r
}

func (e *engine) execData(in *ir.Instr) {
	switch in.Op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar:
		e.set(in.To, e.binop(in))
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw, ir.OCopy:
		e.set(in.To, e.extend(in))
	case ir.OCmp:
		e.set(in.To, e.compare(in))
	case ir.OCall:
		name := e.calleeName(in)
		if name == MergePoint {
			return
		}
		e.residualCall(in, name)
	default:
		e.fail("unsupported op %v", in.Op)
	}
}

// residualCall emits a call to a runtime helper the interpreter's handler makes.
// The helper is a black box -- its arguments are materialized (green ones become
// constants, so a green opcode selector folds into the call) and its result is a
// fresh runtime value. The dispatch that reached the handler is gone; the call the
// program actually performs remains, in order.
func (e *engine) residualCall(in *ir.Instr, name string) {
	if name == "" {
		e.fail("indirect calls are not supported")
		return
	}
	if len(in.AggArgs) != 0 || in.RetAgg != nil {
		e.fail("call to %q: aggregate arguments/results are not supported", name)
		return
	}
	args := make([]ir.Ref, 0, len(in.Args)-1)
	for _, a := range in.Args[1:] {
		args = append(args, e.materialize(e.valueOf(a)))
	}
	callee := e.out.Sym(name, 0)
	if in.To.Kind == ir.RefTemp {
		e.set(in.To, value{kind: vResid, ref: e.cur.Call(in.Cls, callee, args...), cls: in.Cls})
	} else {
		e.cur.CallVoid(callee, args...)
	}
}

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

func (e *engine) compare(in *ir.Instr) value {
	a, b := e.valueOf(in.Args[0]), e.valueOf(in.Args[1])
	if a.kind == vConst && b.kind == vConst {
		return value{kind: vConst, k: foldCmp(in.Cmp, a.k, b.k), cls: ir.ClsW}
	}
	res := e.cur.Cmp(in.Cmp, in.Cls, e.materialize(a), e.materialize(b))
	return value{kind: vResid, ref: res, cls: in.Cls}
}

func (e *engine) extend(in *ir.Instr) value {
	a := e.valueOf(in.Args[0])
	switch a.kind {
	case vConst:
		return value{kind: vConst, k: applyExt(in.Op, a.k), cls: in.Cls}
	case vAddr:
		return a
	default:
		if in.Op == ir.OCopy {
			return value{kind: vResid, ref: a.ref, cls: in.Cls}
		}
		return value{kind: vResid, ref: e.emitExt(in.Op, in.Cls, e.materialize(a)), cls: in.Cls}
	}
}

func (e *engine) store(addr, val value) {
	if addr.kind != vAddr {
		e.fail("store to a runtime address is not supported")
		return
	}
	switch addr.reg.role {
	case roleCode, roleInput:
		e.fail("the program and inputs are read-only")
	default:
		e.mem[cell(addr.reg, addr.off)] = val
	}
}

func (e *engine) load(op ir.Op, addr value) value {
	if addr.kind != vAddr {
		e.fail("load from a runtime address is not supported")
		return value{}
	}
	switch addr.reg.role {
	case roleCode:
		k, ok := e.readProg(op, addr.off)
		if !ok {
			e.fail("program read at offset %d is out of range", addr.off)
			return value{}
		}
		return value{kind: vConst, k: k, cls: loadCls(op)}
	case roleInput:
		return value{kind: vResid, ref: e.inputParam(addr.off, loadCls(op)), cls: loadCls(op)}
	default:
		v, ok := e.mem[cell(addr.reg, addr.off)]
		if !ok {
			return value{kind: vConst, k: 0, cls: loadCls(op)}
		}
		return v
	}
}

func (e *engine) inputParam(off int64, cls ir.Cls) ir.Ref {
	if r, ok := e.ins[off]; ok {
		return r
	}
	r := e.out.Param(fmt.Sprintf("in%d", off/8), cls)
	e.ins[off] = r
	e.inOrder = append(e.inOrder, off)
	return r
}

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
		return e.cur.Add(cls, a, b)
	case ir.OSub:
		return e.cur.Sub(cls, a, b)
	case ir.OMul:
		return e.cur.Mul(cls, a, b)
	case ir.OAnd:
		return e.cur.And(cls, a, b)
	case ir.OOr:
		return e.cur.Or(cls, a, b)
	case ir.OXor:
		return e.cur.Xor(cls, a, b)
	case ir.OShl:
		return e.cur.Shl(cls, a, b)
	case ir.OShr:
		return e.cur.Shr(cls, a, b)
	case ir.OSar:
		return e.cur.Sar(cls, a, b)
	}
	e.fail("cannot residualize op %v", op)
	return ir.R
}

func (e *engine) emitExt(op ir.Op, cls ir.Cls, a ir.Ref) ir.Ref {
	switch op {
	case ir.OExtsb:
		return e.cur.Extsb(cls, a)
	case ir.OExtub:
		return e.cur.Extub(cls, a)
	case ir.OExtsh:
		return e.cur.Extsh(cls, a)
	case ir.OExtuh:
		return e.cur.Extuh(cls, a)
	case ir.OExtsw:
		return e.cur.Extsw(cls, a)
	case ir.OExtuw:
		return e.cur.Extuw(cls, a)
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
		s := uint(64 - 8*n)
		return int64(u<<s) >> s, true
	}
	return int64(u), true
}

func cell(r *region, off int64) [2]int64 { return [2]int64{int64(r.id), off} }

func copyEnv(m map[uint32]value) map[uint32]value {
	c := make(map[uint32]value, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func copyMem(m map[[2]int64]value) map[[2]int64]value {
	c := make(map[[2]int64]value, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

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

func foldCmp(p ir.Cmp, a, b int64) int64 {
	var r bool
	switch p {
	case ir.CmpEq:
		r = a == b
	case ir.CmpNe:
		r = a != b
	case ir.CmpSle:
		r = a <= b
	case ir.CmpSlt:
		r = a < b
	case ir.CmpSge:
		r = a >= b
	case ir.CmpSgt:
		r = a > b
	case ir.CmpUle:
		r = uint64(a) <= uint64(b)
	case ir.CmpUlt:
		r = uint64(a) < uint64(b)
	case ir.CmpUge:
		r = uint64(a) >= uint64(b)
	case ir.CmpUgt:
		r = uint64(a) > uint64(b)
	}
	if r {
		return 1
	}
	return 0
}

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
	return k
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
