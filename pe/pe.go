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
	vConst     vkind = iota // a known integer (green)
	vAddr                   // a known address: a region and a static byte offset (green)
	vResid                  // a runtime value: a reference in the residual (red)
	vSym                    // a global data/function symbol + offset (green)
	vBlockAddr              // the address of a code block, e.g. a dispatch-table entry (green)
)

type value struct {
	kind vkind
	k    int64
	reg  *region
	off  int64
	ref  ir.Ref
	cls  ir.Cls
	sym  string    // vSym: symbol name
	blk  *ir.Block // vBlockAddr: the target block
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

	srcMod   *ir.Module           // the module the interpreter lives in (for global data)
	blockSym map[string]*ir.Block // block-address symbol -> block (dispatch tables)

	cur     *ir.Block // residual block currently being emitted into
	env     map[uint32]value
	mem     map[[2]int64]value
	baseEnv map[uint32]value // the green state established by the interpreter's setup
	baseMem map[[2]int64]value

	ins     map[int64]ir.Ref // input byte offset -> synthesized residual param
	inOrder []int64

	nreg        int
	stackStride int64     // bytes between operand-stack elements (8 for a long, 16 for a struct)
	dispatch    *ir.Block // the interpreter block a specialized state resumes in
	dispatchIdx int       // the instruction in dispatch to resume at (just past the merge point)
	pcReg       *region
	spReg       *region
	stackReg    *region
	varRegs     []*region

	states   map[greenState]*state
	work     []greenState
	regionOf map[uint32]*region      // alloca temp id -> its region (stable across re-execution)
	ipdom    map[*ir.Block]*ir.Block // source block -> its immediate post-dominator
	stopAt   *ir.Block               // emit stops before this block (a diamond's join)
	nphi     int                     // for distinct residual phi names
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
		srcMod:   m,
		prog:     prog,
		out:      out.NewFunc(name, src.Retty),
		env:      map[uint32]value{},
		mem:      map[[2]int64]value{},
		ins:      map[int64]ir.Ref{},
		states:   map[greenState]*state{},
		regionOf: map[uint32]*region{},
		blockSym: map[string]*ir.Block{},
	}
	for _, b := range src.Blocks {
		if b.Sym != "" {
			e.blockSym[b.Sym] = b
		}
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
	e.ipdom = computePostIdom(e.src)
	e.stackStride = detectStackStride(e.src)

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
	e.emitFrom(e.dispatch, e.dispatchIdx)
}

// emit symbolically executes interpreter block b (and its successors) into the
// current residual block, until the path returns or reaches a merge point.
func (e *engine) emit(b *ir.Block) { e.emitFrom(b, 0) }

// emitFrom is emit resuming at instruction start -- used to continue just past the
// merge point that heads a specialized state, so the handler that produced it is
// not re-executed.
func (e *engine) emitFrom(b *ir.Block, start int) {
	if e.err != nil {
		return
	}
	if start == 0 && b == e.stopAt {
		return // reached a diamond's join; the caller resumes here after merging
	}
	for k := start; k < len(b.Instrs); k++ {
		in := &b.Instrs[k]
		if e.isMerge(in) {
			// A merge point: this path reaches a green state. Record where to resume
			// (just past the marker) and hand off to the memoizing transition.
			e.dispatch = b
			e.dispatchIdx = k + 1
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
		// A branch on a runtime value. When the two arms re-converge (the branch has
		// a post-dominator before the next merge point), specialize each only up to
		// that join and merge there, so the shared tail is emitted once. Otherwise
		// (the arms genuinely diverge -- one returns, one loops) specialize both to
		// completion.
		if j := e.ipdom[b]; j != nil && j != e.stopAt {
			e.diamond(b, e.materialize(c), j)
			return
		}
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
	case ir.JmpBr:
		// A computed goto (an interpreter's dispatch): the target address is green --
		// a block address read from the dispatch table at the green opcode -- so we
		// follow it, and the dispatch resolves to the one handler.
		a := e.valueOf(b.Jmp.Arg)
		if a.kind != vBlockAddr {
			e.fail("computed goto on a non-green target")
			return
		}
		e.emit(a.blk)
	case ir.JmpRet:
		e.cur.Ret(e.materialize(e.valueOf(b.Jmp.Arg)))
	default:
		e.fail("unsupported terminator %v", b.Jmp.Kind)
	}
}

// diamond specializes a runtime branch whose arms re-converge at join. It emits
// the residual Jnz, then specializes each arm only up to join. If the arms arrive
// with the same green state -- the branch decided only runtime data, like an
// inline-cache hit-or-miss -- they merge: a phi for each memory cell they leave
// differing, and the tail after join is emitted once. If instead they arrive with
// different green state -- the branch chose the program's next position, like a
// conditional jump -- there is nothing to share, and each arm continues from join
// on its own.
func (e *engine) diamond(b *ir.Block, cond ir.Ref, join *ir.Block) {
	bt, bf := e.out.NewBlock("t"), e.out.NewBlock("f")
	e.cur.Jnz(cond, bt, bf)

	saveStop := e.stopAt
	e.stopAt = join
	env0, mem0 := e.env, e.mem

	e.env, e.mem = copyEnv(env0), copyMem(mem0)
	e.cur = bt
	e.emit(b.Jmp.To)
	tEnd, tEnv, tMem := e.cur, e.env, e.mem

	e.env, e.mem = copyEnv(env0), copyMem(mem0)
	e.cur = bf
	e.emit(b.Jmp.To2)
	fEnd, fEnv, fMem := e.cur, e.env, e.mem

	e.stopAt = saveStop

	if !greenMatch(tMem, fMem) {
		// The arms diverge into different green states; continue each on its own.
		e.env, e.mem, e.cur = tEnv, tMem, tEnd
		e.emit(join)
		e.env, e.mem, e.cur = fEnv, fMem, fEnd
		e.emit(join)
		return
	}

	jr := e.out.NewBlock("join")
	merged := copyMem(fMem)
	for c, tv := range tMem {
		fv := fMem[c]
		if valueEqual(tv, fv) {
			continue
		}
		cls := tv.cls
		if cls == 0 {
			cls = ir.ClsL
		}
		p := &ir.Phi{Cls: cls, To: e.out.NewTemp(fmt.Sprintf("r%d", e.nphi), cls)}
		e.nphi++
		p.Args = []ir.Ref{e.materialize(tv), e.materialize(fv)}
		p.Blocks = []*ir.Block{tEnd, fEnd}
		jr.Phis = append(jr.Phis, p)
		merged[c] = value{kind: vResid, ref: p.To, cls: cls}
	}
	tEnd.Goto(jr)
	fEnd.Goto(jr)

	e.cur = jr
	e.env = fEnv // the join reads its inputs from memory; either arm's env serves
	e.mem = merged
	e.emit(join)
}

// greenMatch reports whether two memory states agree on every green cell (constant
// or address) they share -- the condition for merging a runtime branch's arms. A
// differing green cell (a program counter set two ways) or a green/red mismatch
// means the arms belong to different specializations.
func greenMatch(a, b map[[2]int64]value) bool {
	for c, av := range a {
		bv, ok := b[c]
		if !ok {
			continue
		}
		if av.kind == vResid && bv.kind == vResid {
			continue // both runtime: a phi will reconcile them
		}
		if !valueEqual(av, bv) {
			return false
		}
	}
	return true
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
	if e.stackReg != nil {
		// Each live operand-stack element spans one stride; its cells sit on the
		// 8-byte grain within it (a struct value's two halves, say).
		for i := int64(0); i < sp; i++ {
			for off := int64(0); off < e.stackStride; off += 8 {
				red = append(red, e.load(ir.OLoadl, value{kind: vAddr, reg: e.stackReg, off: i*e.stackStride + off}))
			}
		}
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
	if e.stackReg != nil {
		for j := int64(0); j < sp; j++ {
			for off := int64(0); off < e.stackStride; off += 8 {
				put(e.stackReg, j*e.stackStride+off)
			}
		}
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
		if c.Kind == ir.ConstSym {
			if blk := e.blockSym[c.Sym]; blk != nil {
				return value{kind: vBlockAddr, blk: blk}
			}
			return value{kind: vSym, sym: c.Sym, off: c.Int}
		}
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
	case ir.OBlockAddr:
		e.set(in.To, value{kind: vBlockAddr, blk: in.Blk})
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
	args := make([]ir.Ref, 0, len(in.Args)-1)
	for k, a := range in.Args[1:] {
		var aggT *ir.AggType
		if k < len(in.AggArgs) {
			aggT = in.AggArgs[k]
		}
		if aggT == nil {
			args = append(args, e.materialize(e.valueOf(a)))
			continue
		}
		// A by-value aggregate argument (a JSValue, say) is a pointer to struct
		// storage. Copy its tracked cells into fresh residual storage and pass a
		// pointer, matching the ABI.
		av := e.valueOf(a)
		if av.kind != vAddr {
			e.fail("call to %q: aggregate argument is not a known struct", name)
			return
		}
		size, align := aggT.Layout()
		slot := e.cur.Alloc(allocAlign(align), size)
		for off := int64(0); off < int64(size); off += 8 {
			cv := e.load(ir.OLoadl, value{kind: vAddr, reg: av.reg, off: av.off + off})
			dst := slot
			if off != 0 {
				dst = e.cur.Add(ir.ClsP, slot, e.out.ConstInt(ir.ClsL, off))
			}
			e.cur.Store(e.materialize(cv), dst)
		}
		args = append(args, slot)
	}
	callee := e.out.Sym(name, 0)
	if in.To.Kind != ir.RefTemp {
		e.cur.CallVoid(callee, args...)
		e.tagCall(in)
		return
	}
	// An aggregate result is a pointer to the returned struct; the handler reads its
	// fields with ordinary loads, which residualize against that pointer.
	res := e.cur.Call(in.Cls, callee, args...)
	e.tagCall(in)
	e.set(in.To, value{kind: vResid, ref: res, cls: in.Cls})
}

// tagCall copies the just-emitted residual call's aggregate metadata from the
// interpreter's call, so its by-value struct arguments and result use the same ABI.
func (e *engine) tagCall(in *ir.Instr) {
	last := &e.cur.Instrs[len(e.cur.Instrs)-1]
	last.RetAgg = in.RetAgg
	last.AggArgs = in.AggArgs
}

func allocAlign(a int) int {
	switch {
	case a >= 16:
		return 16
	case a >= 8:
		return 8
	default:
		return 4
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
		if a.kind == vSym && b.kind == vConst {
			return value{kind: vSym, sym: a.sym, off: a.off + b.k}
		}
		if a.kind == vConst && b.kind == vSym {
			return value{kind: vSym, sym: b.sym, off: b.off + a.k}
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
	if addr.kind == vResid {
		// A store to a runtime address (e.g. a per-position inline-cache slot,
		// reached through a runtime base at a green offset): residualize it.
		e.cur.Store(e.materialize(val), addr.ref)
		return
	}
	if addr.kind != vAddr {
		e.fail("store to an unknown address")
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
	if addr.kind == vResid {
		// A load from a runtime address at a green offset -- an inline-cache slot
		// keyed by the (green) program position, say: residualize it.
		return value{kind: vResid, ref: e.cur.Load(loadCls(op), addr.ref), cls: loadCls(op)}
	}
	if addr.kind == vSym {
		// A load from global data at a green offset -- a dispatch-table entry: read
		// it from the module.
		if v, ok := e.readData(addr.sym, addr.off); ok {
			return v
		}
		e.fail("cannot read global %q at offset %d", addr.sym, addr.off)
		return value{}
	}
	if addr.kind != vAddr {
		e.fail("load from an unknown address")
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

// readData reads the value at byte offset off of the named global -- used to read
// a dispatch table's entries (block addresses) at the green opcode.
func (e *engine) readData(name string, off int64) (value, bool) {
	var d *ir.Data
	for _, dd := range e.srcMod.Data {
		if dd.Name == name {
			d = dd
			break
		}
	}
	if d == nil {
		return value{}, false
	}
	pos := int64(0)
	for i := range d.Items {
		it := &d.Items[i]
		sz := itemBytes(it)
		if off >= pos && off < pos+sz {
			return e.itemValue(it, off-pos)
		}
		pos += sz
	}
	return value{}, false
}

func (e *engine) itemValue(it *ir.DataItem, rel int64) (value, bool) {
	switch {
	case it.Sym != "":
		if blk := e.blockSym[it.Sym]; blk != nil {
			return value{kind: vBlockAddr, blk: blk}, true
		}
		return value{kind: vSym, sym: it.Sym, off: it.Off + rel}, true
	case len(it.Ints) > 0:
		esz := int64(it.Sub.Size())
		if idx := rel / esz; int(idx) < len(it.Ints) {
			return value{kind: vConst, k: it.Ints[idx], cls: it.Sub.Cls()}, true
		}
	case it.Zero > 0:
		return value{kind: vConst, k: 0, cls: ir.ClsL}, true
	case it.Str != "":
		if int(rel) < len(it.Str) {
			return value{kind: vConst, k: int64(it.Str[rel]), cls: ir.ClsW}, true
		}
	}
	return value{}, false
}

func itemBytes(it *ir.DataItem) int64 {
	switch {
	case it.Sym != "":
		return int64(it.Sub.Size())
	case len(it.Ints) > 0:
		return int64(len(it.Ints)) * int64(it.Sub.Size())
	case len(it.Flts) > 0:
		return int64(len(it.Flts)) * int64(it.Sub.Size())
	case it.Zero > 0:
		return int64(it.Zero)
	case it.Str != "":
		return int64(len(it.Str))
	}
	return 0
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

func valueEqual(a, b value) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case vConst:
		return a.k == b.k
	case vAddr:
		return a.reg == b.reg && a.off == b.off
	case vResid:
		return a.ref == b.ref
	}
	return true
}

// computePostIdom returns each source block's immediate post-dominator (the
// nearest block all its outgoing paths must pass through), or absent when a block
// has none -- its paths diverge for good, one returning while another loops. Used
// to find where a runtime branch's arms re-converge. Exit blocks (no successors)
// are the roots; the iterative intersection is fine for the small interpreter CFGs
// this handles.
func computePostIdom(f *ir.Func) map[*ir.Block]*ir.Block {
	blocks := f.Blocks
	n := len(blocks)
	idx := make(map[*ir.Block]int, n)
	for i, b := range blocks {
		idx[b] = i
	}
	pdom := make([][]bool, n)
	exit := make([]bool, n)
	for i, b := range blocks {
		set := make([]bool, n)
		if len(b.Succs()) == 0 {
			exit[i] = true
			set[i] = true
		} else {
			for k := range set {
				set[k] = true
			}
		}
		pdom[i] = set
	}
	for changed := true; changed; {
		changed = false
		for i := n - 1; i >= 0; i-- {
			if exit[i] {
				continue
			}
			var nw []bool
			for _, s := range blocks[i].Succs() {
				if s == nil {
					continue
				}
				si := idx[s]
				if nw == nil {
					nw = append([]bool(nil), pdom[si]...)
				} else {
					for k := range nw {
						nw[k] = nw[k] && pdom[si][k]
					}
				}
			}
			if nw == nil {
				nw = make([]bool, n)
			}
			nw[i] = true
			if !boolEq(nw, pdom[i]) {
				pdom[i] = nw
				changed = true
			}
		}
	}
	ipdom := make(map[*ir.Block]*ir.Block)
	for i, b := range blocks {
		best, bestCount := -1, -1
		for k := 0; k < n; k++ {
			if k == i || !pdom[i][k] {
				continue
			}
			if c := boolCount(pdom[k]); c > bestCount { // closest strict post-dom has the most
				best, bestCount = k, c
			}
		}
		if best >= 0 {
			ipdom[b] = blocks[best]
		}
	}
	return ipdom
}

func boolEq(a, b []bool) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func boolCount(a []bool) int {
	n := 0
	for _, v := range a {
		if v {
			n++
		}
	}
	return n
}

// detectStackStride finds the byte stride between operand-stack elements by the
// scaling of the stack index in the interpreter: an add of the "stack" alloca and a
// multiply-by-constant. It is 8 for a stack of longs, 16 for a stack of two-word
// structs (a tagged value like a JSValue). Defaults to 8.
func detectStackStride(f *ir.Func) int64 {
	def := map[uint32]*ir.Instr{}
	var stackID uint32
	found := false
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.To.Kind == ir.RefTemp {
				def[in.To.ID] = in
			}
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp {
				if t := f.Temp(in.To); t != nil && allocBase(t.Name) == "stack" {
					stackID, found = in.To.ID, true
				}
			}
		}
	}
	if !found {
		return 8
	}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op != ir.OAdd || len(in.Args) != 2 {
				continue
			}
			for oi := 0; oi < 2; oi++ {
				if in.Args[oi].Kind != ir.RefTemp || in.Args[oi].ID != stackID {
					continue
				}
				if d := def[in.Args[1-oi].ID]; d != nil && d.Op == ir.OMul {
					for _, a := range d.Args {
						if a.Kind == ir.RefConst {
							if c := f.Consts[a.ID]; c.Kind == ir.ConstInt && c.Int > 0 {
								return c.Int
							}
						}
					}
				}
			}
		}
	}
	return 8
}

// allocBase strips the ".addr" suffix cc gives a variable's storage slot.
func allocBase(name string) string {
	if n := len(name); n > 5 && name[n-5:] == ".addr" {
		return name[:n-5]
	}
	return name
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
