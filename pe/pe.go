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

// The markers an interpreter uses to tell the specializer what is green. MergePoint
// heads the dispatch, naming the green loop variables. CodeMarker yields the green
// program pointer, so the bytecode need not be traced out of a runtime object.
// GreenMarker yields a green integer the specializer supplies -- the frame-layout
// metadata (argument, local, and stack counts) that must be static for the operand
// stack pointer to stay green.
const (
	MergePoint  = "__cg12_merge_point"
	CodeMarker  = "__cg12_code"
	GreenMarker = "__cg12_green"
)

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
	id        int
	role      role
	size      int64
	escapes   bool   // rInput: the pointer value is used by value, not just dereferenced
	name      string // rInput: the parameter name (for the residual pointer parameter)
	base      ir.Ref // rInput escaping, or a setup local's residualized buffer
	haveBase  bool
	setupTime bool // created during the one-time setup (before the first merge point)
}

// greenState is the memoization key: everything static that shapes the residual.
type greenState struct{ pc, sp int64 }

// state is the residual block for a green state and the phis carrying its red state.
// dispatch/dispatchIdx are where to resume interpreting for this state -- captured
// when it is created, since with inline dispatch each opcode's merge point sits in
// its own block.
type state struct {
	blk         *ir.Block
	phis        []*ir.Phi
	dispatch    *ir.Block
	dispatchIdx int
}

type engine struct {
	src, out *ir.Func
	prog     []byte

	srcMod   *ir.Module           // the module the interpreter lives in (for global data)
	blockSym map[string]*ir.Block // block-address symbol -> block (dispatch tables)
	meta     []int64              // green frame-layout metadata, indexed by __cg12_green id

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
	codeReg     *region // the program-bytes region (pc may be a pointer into it)
	varRegs     []*region
	pcIsPtr     bool // pc is a pointer walking the code (QuickJS style), not an int index
	spIsPtr     bool // sp is a pointer walking the stack, not an int depth

	states    map[greenState]*state
	work      []greenState
	regionOf  map[uint32]*region      // alloca temp id -> its region (stable across re-execution)
	ipdom     map[*ir.Block]*ir.Block // source block -> its immediate post-dominator
	stopAt    *ir.Block               // emit stops before this block (a diamond's join)
	nphi      int                     // for distinct residual phi names
	depth     int                     // emit recursion depth (guards runaway inlining)
	loops     map[*ir.Block]*loopInfo // merge-free loop headers to residualize
	active    map[*ir.Block]*loopState
	localBuf  map[*region]ir.Ref   // handler-local escape buffers (reset per specialization)
	escLocal  map[uint32]bool      // alloca temps whose address escapes -> stable storage
	codeIsPtr bool                 // pc walks the code region (set by the __cg12_code marker)
	allocDefs map[uint32]*ir.Instr // temp id -> its alloca instruction (for jump-skipped allocas)
	nblk      int                  // for unique residual block names (labels must not collide)
	err       error
}

// block creates a residual block with a name unique across the residual, so the
// backend's labels do not collide (many arms share a base name like "bt" or "join").
func (e *engine) block(prefix string) *ir.Block {
	e.nblk++
	return e.out.NewBlock(fmt.Sprintf("%s%d", prefix, e.nblk))
}

// loopState tracks a residual loop currently being emitted: its header block, the
// memory cells it carries, and their phis (so a back-edge can feed and close them).
type loopState struct {
	hdr   *ir.Block
	cells [][2]int64
	phis  []*ir.Phi
}

// maxEmitDepth bounds emit's recursive descent so an unmarked, non-converging loop
// fails cleanly rather than overflowing the Go stack. Legitimate straight-line
// setup stays far below this; a true cycle blows past it immediately.

const maxEmitDepth = 20000

// Specialize specializes the named interpreter in m against prog, returning a new
// module holding the residual function (same name, suffixed), its name, and the
// byte offsets of the runtime inputs it reads (in parameter order).
func Specialize(m *ir.Module, interp string, prog []byte) (*ir.Module, string, []int64, error) {
	return SpecializeWithMeta(m, interp, prog, nil)
}

// SpecializeWithMeta is Specialize with green frame-layout metadata: the integers a
// __cg12_green(id) marker resolves to, indexed by id. An interpreter whose program
// and frame counts live in a runtime object (QuickJS's JSFunctionBytecode) uses the
// markers to declare them green, so its operand stack and dispatch stay static.
func SpecializeWithMeta(m *ir.Module, interp string, prog []byte, meta []int64) (*ir.Module, string, []int64, error) {
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
		meta:     meta,
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
	e.out.RetAgg = src.RetAgg // the residual returns the same aggregate the interpreter does
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
	e.loops = analyzeLoops(e.src, e.isMerge)
	e.active = map[*ir.Block]*loopState{}
	e.localBuf = map[*region]ir.Ref{}
	e.escLocal = escapingLocals(e.src)
	e.allocDefs = map[uint32]*ir.Instr{}
	for _, b := range e.src.Blocks {
		for k := range b.Instrs {
			if in := &b.Instrs[k]; (in.Op.IsAlloc() || in.Op == ir.OAllocN) && in.To.Kind == ir.RefTemp {
				e.allocDefs[in.To.ID] = in
			}
		}
	}

	// Classify the parameters. "code" is the green program. A pointer parameter is a
	// runtime array: read-only ones synthesize a scalar per element, but one whose
	// pointer value escapes (a JSContext handed to runtime routines) becomes a
	// residual pointer parameter. A scalar parameter is a plain runtime value.
	esc := escapingParams(e.src)
	for _, t := range e.src.Params {
		id := uint32(t.ID)
		ref := ir.Ref{Kind: ir.RefTemp, ID: id}
		if t.Name == "code" {
			e.codeReg = e.newRegion(roleCode, 0)
			e.env[id] = value{kind: vAddr, reg: e.codeReg}
			continue
		}
		if t.Agg != nil {
			// A by-value aggregate parameter (a JSValue passed by value): a residual
			// aggregate parameter, so the backend uses the right ABI. Its value is the
			// address of the struct storage; field accesses residualize against it.
			pr := e.out.Param(t.Name, ir.ClsL)
			e.out.Temp(pr).Agg = t.Agg
			e.env[id] = value{kind: vResid, ref: pr, cls: ir.ClsL}
			continue
		}
		if e.src.ClassOf(ref) == ir.ClsP {
			reg := e.newRegion(roleInput, 0)
			reg.name, reg.escapes = t.Name, esc[t.Name]
			e.env[id] = value{kind: vAddr, reg: reg}
			continue
		}
		cls := e.src.ClassOf(ref)
		e.env[id] = value{kind: vResid, ref: e.out.Param(t.Name, cls), cls: cls}
	}
	if e.codeReg == nil {
		// No "code" parameter: the program arrives through the __cg12_code() marker.
		e.codeReg = e.newRegion(roleCode, 0)
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
	return &region{id: e.nreg, role: r, size: size, setupTime: e.baseMem == nil}
}

// specialize emits the residual for one green state: reset the interpreter's
// memory to that state (green pc/sp, red cells bound to the block's phis), then
// symbolically execute the dispatch and its handler.
func (e *engine) specialize(gs greenState) {
	st := e.states[gs]
	e.env = copyEnv(e.baseEnv)
	e.mem = copyMem(e.baseMem)
	e.localBuf = map[*region]ir.Ref{} // buffers are per handler copy: this state's blocks
	e.setPC(gs.pc)
	e.setSP(gs.sp)
	e.bindRedState(gs.sp, st.phis)
	e.cur = st.blk
	e.emitFrom(st.dispatch, st.dispatchIdx)
}

// The green loop variables pc and sp are read and written two ways: as integers (an
// index into the program, a stack depth) or as pointers walking the program and the
// stack (the QuickJS style). Either way the green key holds pc's byte offset into
// the program and sp's byte offset into the stack.

func (e *engine) greenPC() int64 {
	v := e.mem[cell(e.pcReg, 0)]
	if v.kind == vAddr {
		return v.off
	}
	return v.k
}

func (e *engine) spBytes() int64 {
	v := e.mem[cell(e.spReg, 0)]
	if v.kind == vAddr {
		return v.off
	}
	return v.k * e.stackStride
}

func (e *engine) setPC(pc int64) {
	if e.pcIsPtr {
		e.mem[cell(e.pcReg, 0)] = value{kind: vAddr, reg: e.codeReg, off: pc}
	} else {
		e.mem[cell(e.pcReg, 0)] = value{kind: vConst, k: pc, cls: ir.ClsW}
	}
}

func (e *engine) setSP(spBytes int64) {
	if e.spIsPtr {
		e.mem[cell(e.spReg, 0)] = value{kind: vAddr, reg: e.stackReg, off: spBytes}
	} else {
		e.mem[cell(e.spReg, 0)] = value{kind: vConst, k: spBytes / e.stackStride, cls: ir.ClsW}
	}
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
	// emit is a recursive descent that terminates at a merge point or a return. An
	// unmarked loop (a red-trip-count setup loop that ought to be residualized, not
	// unrolled) has no merge point, so the descent would recurse without bound; cap
	// it and fail cleanly instead of overflowing the stack.
	e.depth++
	defer func() { e.depth-- }()
	if e.depth > maxEmitDepth {
		e.fail("control flow did not converge")
		return
	}
	if start == 0 && b == e.stopAt {
		return // reached a diamond's join; the caller resumes here after merging
	}
	if start == 0 {
		if li, ok := e.loops[b]; ok {
			if ls := e.active[b]; ls != nil {
				e.closeLoop(ls) // the back-edge of a loop we are residualizing
				return
			}
			// A merge-free loop residualizes only if its trip count is a runtime value.
			// A static one (a green-bounded init loop) unrolls through the ordinary emit.
			if !e.headerGreen(b) {
				e.beginLoop(b, li)
				return
			}
		}
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
		bt, bf := e.block("bt"), e.block("bf")
		e.cur.Jnz(cond, bt, bf)
		env, mem := e.env, e.mem
		e.env, e.mem = copyEnv(env), copyMem(mem)
		e.cur = bt
		e.emit(b.Jmp.To)
		tEnd, tMem := e.cur, e.mem
		e.env, e.mem = copyEnv(env), copyMem(mem)
		e.cur = bf
		e.emit(b.Jmp.To2)
		fEnd, fEnv, fMem := e.cur, e.env, e.mem
		// Both arms reconverged at the enclosing stop (a nested branch inside a diamond
		// arm, both reaching the outer join): neither self-terminated, so merge them so
		// no block is left dangling and the caller continues from a single join.
		if tEnd != fEnd && tEnd.Jmp.Kind == ir.JmpNone && fEnd.Jmp.Kind == ir.JmpNone && e.sameState(tMem, fMem) {
			e.mergeArms(tEnd, fEnd, tMem, fMem, fEnv)
		}
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
		if e.src.RetAgg != nil && b.Jmp.Arg != ir.R {
			// An aggregate return (a JSValue): the value is a pointer to struct storage.
			// Copy its cells into a fresh residual slot and return that, matching the ABI
			// -- the same shape as a by-value aggregate call argument.
			e.retAggregate(e.valueOf(b.Jmp.Arg))
			return
		}
		e.cur.Ret(e.materialize(e.valueOf(b.Jmp.Arg)))
	default:
		e.fail("unsupported terminator %v", b.Jmp.Kind)
	}
}

// retAggregate emits an aggregate return: copy the returned struct's cells into fresh
// residual storage and return a pointer to it, the mirror of a by-value aggregate
// call argument.
func (e *engine) retAggregate(av value) {
	if av.kind != vAddr && av.kind != vResid {
		e.fail("aggregate return value is not a known struct")
		return
	}
	size, align := e.src.RetAgg.Layout()
	slot := e.cur.Alloc(allocAlign(align), size)
	for off := int64(0); off < int64(size); off += 8 {
		cv := e.aggCell(av, off)
		dst := slot
		if off != 0 {
			dst = e.cur.Add(ir.ClsP, slot, e.out.ConstInt(ir.ClsL, off))
		}
		e.cur.Store(e.materialize(cv), dst)
	}
	e.cur.Ret(slot)
}

// headerGreen reports whether a loop header's branch condition is static right now,
// tracing the condition back through the header's own instructions without emitting
// anything. A green-bounded loop (var_count from __cg12_green, say) is unrolled; a
// loop whose condition reads a runtime value is residualized. Anything the trace
// cannot resolve is treated as runtime -- the safe default is to residualize.
func (e *engine) headerGreen(h *ir.Block) bool {
	if h.Jmp.Kind != ir.JmpJnz {
		return false
	}
	val := map[uint32]value{}
	green := func(v value) bool {
		return v.kind == vConst || v.kind == vAddr || v.kind == vBlockAddr || v.kind == vSym
	}
	lookup := func(r ir.Ref) value {
		if r.Kind == ir.RefConst {
			return e.valueOf(r)
		}
		if v, ok := val[r.ID]; ok {
			return v
		}
		if v, ok := e.env[r.ID]; ok {
			return v
		}
		return value{kind: vResid} // an unknown temp: treat as runtime
	}
	for k := range h.Instrs {
		in := &h.Instrs[k]
		switch {
		case in.Op.IsAlloc() || in.Op.IsStore():
			// A condition header does not allocate or store; ignore if one appears.
		case in.Op.IsLoad():
			a := lookup(in.Args[0])
			if a.kind == vAddr {
				val[in.To.ID] = e.mem[cell(a.reg, a.off)] // an unset cell is the zero value: green
			} else {
				val[in.To.ID] = value{kind: vResid}
			}
		default:
			allGreen := true
			for _, arg := range in.Args {
				if !green(lookup(arg)) {
					allGreen = false
				}
			}
			if allGreen {
				val[in.To.ID] = value{kind: vConst}
			} else {
				val[in.To.ID] = value{kind: vResid}
			}
		}
	}
	return green(lookup(h.Jmp.Arg))
}

// beginLoop residualizes a merge-free loop as a real loop. The loop-carried scalar
// cells become phis at a fresh residual header: their pre-loop values feed the phis
// on entry, and the body's back-edge feeds them their next-iteration values. Once
// the cells are red phis the loop condition and body residualize like any runtime
// code, so the residual is a genuine loop rather than an unrolling.
func (e *engine) beginLoop(h *ir.Block, li *loopInfo) {
	if h.Jmp.Kind != ir.JmpJnz {
		e.fail("unsupported loop shape: header does not end in a conditional branch")
		return
	}
	inLoopTo := li.blocks[h.Jmp.To]
	if inLoopTo == li.blocks[h.Jmp.To2] {
		e.fail("unsupported loop shape: neither or both arms stay in the loop")
		return
	}

	// Every block dominating the header has already run, so its allocas have regions.
	// A carried alloca with no region yet is allocated inside the loop -- a loop-local
	// whose value does not cross the back-edge, so it needs no phi.
	cells := make([][2]int64, 0, len(li.carried))
	for _, aid := range li.carried {
		if reg := e.regionOf[aid]; reg != nil {
			cells = append(cells, cell(reg, 0))
		}
	}

	// The residual header, with a phi per carried cell fed the pre-loop value.
	hb := e.block("loop")
	phis := make([]*ir.Phi, len(cells))
	for i, c := range cells {
		cur := e.mem[c]
		cls := cur.cls
		if cls == 0 {
			cls = ir.ClsL
		}
		p := &ir.Phi{Cls: cls, To: e.out.NewTemp(fmt.Sprintf("lv%d", e.nphi), cls)}
		e.nphi++
		p.Args = append(p.Args, e.materialize(cur))
		p.Blocks = append(p.Blocks, e.cur)
		hb.Phis = append(hb.Phis, p)
		phis[i] = p
	}
	e.cur.Goto(hb)
	if e.err != nil {
		return
	}

	ls := &loopState{hdr: hb, cells: cells, phis: phis}
	e.active[h] = ls
	defer delete(e.active, h)

	// Enter the header: the carried cells now read as their phis (runtime), and the
	// header's own instructions (the loop condition) run there.
	e.cur = hb
	for i, c := range cells {
		e.mem[c] = value{kind: vResid, ref: phis[i].To, cls: phis[i].Cls}
	}
	for k := range h.Instrs {
		e.exec(&h.Instrs[k])
		if e.err != nil {
			return
		}
	}

	// The condition splits into the loop body (which re-enters the header via its
	// back-edge, closing the loop) and the exit (which continues after the loop).
	cond := e.materialize(e.valueOf(h.Jmp.Arg))
	body, exit := h.Jmp.To, h.Jmp.To2
	if !inLoopTo {
		body, exit = h.Jmp.To2, h.Jmp.To
	}
	rb, rx := e.block("lbody"), e.block("lexit")
	if inLoopTo {
		e.cur.Jnz(cond, rb, rx)
	} else {
		e.cur.Jnz(cond, rx, rb)
	}

	env0, mem0 := e.env, e.mem
	e.env, e.mem = copyEnv(env0), copyMem(mem0)
	e.cur = rb
	e.emit(body)

	e.env, e.mem = copyEnv(env0), copyMem(mem0)
	e.cur = rx
	e.emit(exit)
}

// closeLoop is reached at a loop's back-edge: feed each carried cell's current value
// to its header phi and jump back to the header.
func (e *engine) closeLoop(ls *loopState) {
	for i, c := range ls.cells {
		ls.phis[i].Args = append(ls.phis[i].Args, e.materialize(e.mem[c]))
		ls.phis[i].Blocks = append(ls.phis[i].Blocks, e.cur)
	}
	e.cur.Goto(ls.hdr)
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
	bt, bf := e.block("t"), e.block("f")
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

	// Only an arm still open (no terminator) reached the join; one that terminated
	// early -- a merge point that jumped to a memoized state, or a return inside the
	// arm -- is already done and must not be revived (overwriting its terminator
	// would resurrect it and loop).
	tOpen := tEnd.Jmp.Kind == ir.JmpNone
	fOpen := fEnd.Jmp.Kind == ir.JmpNone
	switch {
	case tOpen && fOpen && e.sameState(tMem, fMem):
		// Both reached the join at the same program point: merge, emit the tail once.
		e.mergeArms(tEnd, fEnd, tMem, fMem, fEnv)
		e.emit(join)
	case tOpen && fOpen:
		// Both reached the join but at different program points: continue each alone.
		e.env, e.mem, e.cur = tEnv, tMem, tEnd
		e.emit(join)
		e.env, e.mem, e.cur = fEnv, fMem, fEnd
		e.emit(join)
	case tOpen:
		e.env, e.mem, e.cur = tEnv, tMem, tEnd
		e.emit(join)
	case fOpen:
		e.env, e.mem, e.cur = fEnv, fMem, fEnd
		e.emit(join)
	}
}

// mergeArms joins two runtime-branch arms that arrive with the same green state: a
// phi for each memory cell they leave differing, both ends jumping to a fresh join
// block, which becomes the current block. It leaves the join unterminated for the
// caller to continue from.
func (e *engine) mergeArms(tEnd, fEnd *ir.Block, tMem, fMem map[[2]int64]value, fEnv map[uint32]value) {
	jr := e.block("join")
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
}

// sameState reports whether two memory states are at the same program point -- the
// green pc and sp agree. That, not agreement on every cell, is the condition for
// merging a runtime branch's arms: arms at one program point reconcile any differing
// data cell with a phi, while arms that reached different green pc/sp (a conditional
// jump chose the program's next position) belong to different specializations and
// each continue on their own.
func (e *engine) sameState(a, b map[[2]int64]value) bool {
	if e.pcReg != nil && !valueEqual(a[cell(e.pcReg, 0)], b[cell(e.pcReg, 0)]) {
		return false
	}
	if e.spReg != nil && !valueEqual(a[cell(e.spReg, 0)], b[cell(e.spReg, 0)]) {
		return false
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
		e.pcIsPtr = e.codeIsPtr || e.mem[cell(e.pcReg, 0)].kind == vAddr
		spv := e.mem[cell(e.spReg, 0)]
		e.spIsPtr = spv.kind == vAddr
		if e.spIsPtr && e.stackReg == nil && spv.reg != nil {
			// The operand stack has no alloca named "stack" (QuickJS keeps it inside
			// local_buf); learn the region sp actually points into.
			e.stackReg = spv.reg
		}
	}
	sp := e.spBytes()
	gs := greenState{pc: e.greenPC(), sp: sp}
	red := e.redState(sp)

	st, seen := e.states[gs]
	if !seen {
		st = &state{blk: e.out.NewBlock(fmt.Sprintf("s_pc%d_sp%d", gs.pc, sp)), dispatch: e.dispatch, dispatchIdx: e.dispatchIdx}
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
func (e *engine) redState(spBytes int64) []value {
	var red []value
	if e.stackReg != nil && !e.stackReg.haveBase {
		// The live operand stack is the bytes below the top, on the 8-byte grain (a
		// struct value's two halves, say). A stack that lives in residualized storage
		// (real memory) is not carried -- its cells persist in that buffer.
		for off := int64(0); off < spBytes; off += 8 {
			red = append(red, e.load(ir.OLoadl, value{kind: vAddr, reg: e.stackReg, off: off}))
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
func (e *engine) bindRedState(spBytes int64, phis []*ir.Phi) {
	i := 0
	put := func(reg *region, off int64) {
		if i < len(phis) {
			e.mem[cell(reg, off)] = value{kind: vResid, ref: phis[i].To, cls: phis[i].Cls}
			i++
		}
	}
	if e.stackReg != nil && !e.stackReg.haveBase {
		for off := int64(0); off < spBytes; off += 8 {
			put(e.stackReg, off)
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
			// An alloca whose defining instruction a jump skipped: cc places an alloca
			// at its C declaration, but a goto into the block (a shared slow-path label,
			// say) can bypass it. The storage exists regardless of the path, so create
			// its region on demand.
			if in := e.allocDefs[r.ID]; in != nil {
				av := value{kind: vAddr, reg: e.allocRegion(in)}
				e.set(r, av)
				return av
			}
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
	case in.Op.IsAlloc() || in.Op == ir.OAllocN:
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
	// A fixed alloca carries its size as a constant; a dynamic alloca (alloca of a
	// green-computed frame size) carries it as a value that has folded to a constant.
	size := int64(0)
	if len(in.Args) > 0 {
		if a := in.Args[0]; a.Kind == ir.RefConst {
			size = e.src.Consts[a.ID].Int
		} else if v := e.valueOf(a); v.kind == vConst {
			size = v.k
		}
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
		if size > 8 {
			r.role = roleVar
			switch {
			case in.To.Kind == ir.RefTemp && e.escLocal[in.To.ID] && r.setupTime:
				// A persistent frame structure whose address escapes to runtime code
				// (QuickJS's JSStackFrame, or local_buf holding by-value JSValues). It
				// cannot be phi-carried; back it with stable setup storage now, so its
				// cells are real memory and pointers into it can be passed. Allocated
				// during setup, so it dominates every handler and needs no phi.
				r.base = e.cur.Alloc(allocAlign(int(size)), int(size))
				r.haveBase = true
			case r.setupTime:
				// A local array declared before the interpreter loop is persistent VM
				// state, carried across merges as phis.
				e.varRegs = append(e.varRegs, r)
			}
			// A size>8 local allocated inside a handler (a JSValue built on the stack)
			// is a handler temporary, handled per-copy on escape.
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
	case ir.OAdd, ir.OSub, ir.OMul, ir.ODiv, ir.OUDiv, ir.ORem, ir.OURem,
		ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar:
		e.set(in.To, e.binop(in))
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw, ir.OCopy:
		e.set(in.To, e.extend(in))
	case ir.OExts, ir.OTruncd, ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof, ir.OCast:
		// Numeric conversions (JS arithmetic works in doubles): residualize against the
		// runtime operand.
		a := e.materialize(e.valueOf(in.Args[0]))
		e.set(in.To, value{kind: vResid, ref: e.emitConv(in.Op, in.Cls, a), cls: in.Cls})
	case ir.ONeg:
		a := e.valueOf(in.Args[0])
		if a.kind == vConst {
			e.set(in.To, value{kind: vConst, k: -a.k, cls: in.Cls})
		} else {
			e.set(in.To, value{kind: vResid, ref: e.cur.Sub(in.Cls, e.out.ConstInt(in.Cls, 0), e.materialize(a)), cls: in.Cls})
		}
	case ir.OCmp:
		e.set(in.To, e.compare(in))
	case ir.OBlockAddr:
		e.set(in.To, value{kind: vBlockAddr, blk: in.Blk})
	case ir.OCall:
		name := e.calleeName(in)
		switch name {
		case MergePoint:
			return
		case CodeMarker:
			// The green program pointer -- pc walks this instead of bytecode reached
			// through a runtime object. Because the marker defines pc as a pointer into
			// the code region, the walk is pointer-style regardless of what a runtime
			// setup branch (a generator or type-check preamble) leaves pc holding.
			e.codeIsPtr = true
			e.set(in.To, value{kind: vAddr, reg: e.codeReg, off: 0})
			return
		case GreenMarker:
			// A green metadata integer the specializer supplies, selected by a static id.
			id := e.valueOf(in.Args[1])
			if id.kind != vConst {
				e.fail("__cg12_green: id is not static")
				return
			}
			if id.k < 0 || int(id.k) >= len(e.meta) {
				e.fail("__cg12_green(%d): no metadata supplied", id.k)
				return
			}
			e.set(in.To, value{kind: vConst, k: e.meta[id.k], cls: in.Cls})
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
		// storage. Copy its cells into fresh residual storage and pass a pointer,
		// matching the ABI. The source is either a struct we track cell-by-cell
		// (vAddr) or an opaque struct another call returned (vResid pointer).
		av := e.valueOf(a)
		if av.kind != vAddr && av.kind != vResid {
			e.fail("call to %q: aggregate argument is not a known struct", name)
			return
		}
		size, align := aggT.Layout()
		slot := e.cur.Alloc(allocAlign(align), size)
		for off := int64(0); off < int64(size); off += 8 {
			cv := e.aggCell(av, off)
			dst := slot
			if off != 0 {
				dst = e.cur.Add(ir.ClsP, slot, e.out.ConstInt(ir.ClsL, off))
			}
			e.cur.Store(e.materialize(cv), dst)
		}
		args = append(args, slot)
	}
	// A named callee is a direct call; otherwise the callee is a runtime function
	// pointer (a dispatch through a class or method table), materialized and called
	// indirectly.
	var callee ir.Ref
	if name != "" {
		callee = e.out.Sym(name, 0)
	} else {
		callee = e.materialize(e.valueOf(in.Args[0]))
		if e.err != nil {
			return
		}
	}
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

// aggCell reads one 8-byte word at offset off of an aggregate value, whether it is
// a struct whose cells we track (vAddr) or an opaque struct pointer another call
// returned (vResid), which we read with a residual load.
func (e *engine) aggCell(av value, off int64) value {
	switch av.kind {
	case vAddr:
		return e.load(ir.OLoadl, value{kind: vAddr, reg: av.reg, off: av.off + off})
	case vResid:
		addr := av.ref
		if off != 0 {
			addr = e.cur.Add(ir.ClsP, av.ref, e.out.ConstInt(ir.ClsL, off))
		}
		return value{kind: vResid, ref: e.cur.Load(ir.ClsL, addr), cls: ir.ClsL}
	default:
		e.fail("aggregate argument is not a known struct")
		return value{}
	}
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
	if a.kind == vConst && b.kind == vConst && !(isDivRem(in.Op) && b.k == 0) {
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
	if in.Op == ir.OSub {
		// Pointer walks the other way: sp -= 1 (a pop), sp[-1].
		if a.kind == vAddr && b.kind == vConst {
			return value{kind: vAddr, reg: a.reg, off: a.off - b.k}
		}
		// Pointer difference within one region -- the byte distance, which the
		// interpreter then scales to a depth (sp - stack).
		if a.kind == vAddr && b.kind == vAddr && a.reg == b.reg {
			return value{kind: vConst, k: a.off - b.off, cls: in.Cls}
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
	// Two green addresses in the same region compare by their offsets -- a stack
	// pointer against the stack base, say (`pval < sp` walking the frame). Without
	// this the comparison would residualize and force the green addresses into the
	// runtime, escaping the region.
	if a.kind == vAddr && b.kind == vAddr && a.reg == b.reg {
		return value{kind: vConst, k: foldCmp(in.Cmp, a.off, b.off), cls: ir.ClsW}
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
	case roleCode:
		e.fail("the program is read-only")
	case roleInput:
		if addr.reg.escapes {
			e.cur.Store(e.materialize(val), e.materialize(addr))
			return
		}
		e.fail("a read-only input array was written")
	default:
		if _, ok := e.bufferOf(addr.reg); ok {
			// The region was residualized (its address escaped); write to its buffer.
			e.cur.Store(e.materialize(val), e.materialize(addr))
			return
		}
		e.mem[cell(addr.reg, addr.off)] = val
	}
}

// inputBase returns the residual pointer parameter standing for an escaping runtime
// pointer, creating it on first use.
func (e *engine) inputBase(reg *region) ir.Ref {
	if !reg.haveBase {
		reg.base = e.out.Param(reg.name, ir.ClsP)
		reg.haveBase = true
	}
	return reg.base
}

// bufferOf returns a local region's residual buffer, if it has been given one. A
// setup local's buffer lives in the entry block and is cached on the region; a
// handler local's buffer lives in this specialization's blocks and is tracked in a
// per-copy map that resets each specialization.
func (e *engine) bufferOf(reg *region) (ir.Ref, bool) {
	if reg.setupTime {
		return reg.base, reg.haveBase
	}
	b, ok := e.localBuf[reg]
	return b, ok
}

// ensureBuffer backs a local region with residual storage on first escape: it
// allocates a buffer, flushes the region's currently-tracked cells into it, and
// thereafter the region is real residual memory (loads and stores route through the
// buffer). The flush reads the abstract store, so it must run before the buffer
// redirects loads.
func (e *engine) ensureBuffer(reg *region) ir.Ref {
	if b, ok := e.bufferOf(reg); ok {
		return b
	}
	// A setup local's buffer sits in the entry block, valid everywhere, so a later
	// escape (even inside a handler) reuses it. But a setup local escaping for the
	// FIRST time inside a handler would get a buffer in one copy's block -- invalid in
	// the others -- and de-greening a carried region mid-run desyncs the merge states.
	if reg.setupTime && e.baseMem != nil {
		e.fail("a persistent local's address escaped after setup")
		return ir.R
	}
	size := reg.size
	if size < 8 {
		size = 8
	}
	buf := e.cur.Alloc(allocAlign(int(size)), int(size))
	for off := int64(0); off < reg.size; off += 8 {
		cv := e.load(ir.OLoadl, value{kind: vAddr, reg: reg, off: off})
		dst := buf
		if off != 0 {
			dst = e.cur.Add(ir.ClsP, buf, e.out.ConstInt(ir.ClsL, off))
		}
		e.cur.Store(e.materialize(cv), dst)
	}
	if reg.setupTime {
		reg.base, reg.haveBase = buf, true
		e.removeVar(reg) // de-green persistent state (safe only during setup)
	} else {
		e.localBuf[reg] = buf // a handler local is not carried, so nothing to remove
	}
	return buf
}

func (e *engine) removeVar(reg *region) {
	for i, r := range e.varRegs {
		if r == reg {
			e.varRegs = append(e.varRegs[:i], e.varRegs[i+1:]...)
			return
		}
	}
}

// escapingParams reports which pointer parameters have their pointer value used by
// value -- passed to a call, stored, or returned -- rather than only dereferenced.
// Such a parameter cannot be modeled as a read-only array of synthesized scalars; it
// becomes a residual pointer parameter.
func escapingParams(f *ir.Func) map[string]bool {
	out := map[string]bool{}
	for _, t := range f.Params {
		if f.ClassOf(ir.Ref{Kind: ir.RefTemp, ID: uint32(t.ID)}) != ir.ClsP || t.Name == "code" {
			continue
		}
		if paramEscapes(f, t.Name+".addr") {
			out[t.Name] = true
		}
	}
	return out
}

// escapingLocals returns the alloca temps whose own address (or a pointer computed
// from it) is handed to a runtime call, stored, or returned -- a frame structure
// like QuickJS's JSStackFrame, or the local_buf holding by-value JSValues. Such a
// region cannot be modeled as phi-carried cells; it becomes stable residual storage.
func escapingLocals(f *ir.Func) map[uint32]bool {
	out := map[uint32]bool{}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp && structAddrEscapes(f, in.To.ID) {
				out[in.To.ID] = true
			}
		}
	}
	return out
}

// structAddrEscapes reports whether the address of alloca allocID (or a pointer
// derived from it by pointer arithmetic) is used as a value -- a call argument, a
// stored value, or a return -- rather than only as the address of a load or store.
func structAddrEscapes(f *ir.Func, allocID uint32) bool {
	ptr := map[uint32]bool{allocID: true} // the alloca's address is itself a pointer
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			for k := range b.Instrs {
				in := &b.Instrs[k]
				if in.To.Kind != ir.RefTemp || ptr[in.To.ID] {
					continue
				}
				if in.Op == ir.OAdd || in.Op == ir.OCopy {
					for _, a := range in.Args {
						if a.Kind == ir.RefTemp && ptr[a.ID] {
							ptr[in.To.ID], changed = true, true
						}
					}
				}
			}
		}
	}
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op == ir.OCall {
				for _, a := range in.Args[1:] { // Args[0] is the callee, not an escape
					if a.Kind == ir.RefTemp && ptr[a.ID] {
						return true
					}
				}
			}
			if in.Op.IsStore() && in.Args[0].Kind == ir.RefTemp && ptr[in.Args[0].ID] {
				return true // a pointer into the region stored as a value (Args[1] is the address)
			}
		}
		if b.Jmp.Kind == ir.JmpRet && b.Jmp.Arg.Kind == ir.RefTemp && ptr[b.Jmp.Arg.ID] {
			return true
		}
	}
	return false
}

func paramEscapes(f *ir.Func, addrName string) bool {
	var addrID uint32
	found := false
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op.IsAlloc() && in.To.Kind == ir.RefTemp {
				if tt := f.Temp(in.To); tt != nil && tt.Name == addrName {
					addrID, found = in.To.ID, true
				}
			}
		}
	}
	if !found {
		return false
	}
	// A "pointer temp" holds the parameter's pointer: a load of its slot, or an add
	// or copy of one. Grow the set to a fixpoint.
	ptr := map[uint32]bool{}
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			for k := range b.Instrs {
				in := &b.Instrs[k]
				if in.To.Kind != ir.RefTemp || ptr[in.To.ID] {
					continue
				}
				is := false
				switch {
				case in.Op.IsLoad() && len(in.Args) == 1 && in.Args[0].Kind == ir.RefTemp && in.Args[0].ID == addrID:
					is = true
				case in.Op == ir.OAdd || in.Op == ir.OCopy:
					for _, a := range in.Args {
						if a.Kind == ir.RefTemp && ptr[a.ID] {
							is = true
						}
					}
				}
				if is {
					ptr[in.To.ID], changed = true, true
				}
			}
		}
	}
	// It escapes if a pointer temp is a call argument, a stored value, or returned --
	// as opposed to a load/store address, which is a dereference.
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if in.Op == ir.OCall {
				for _, a := range in.Args[1:] {
					if a.Kind == ir.RefTemp && ptr[a.ID] {
						return true
					}
				}
			}
			if in.Op.IsStore() && in.Args[0].Kind == ir.RefTemp && ptr[in.Args[0].ID] {
				return true
			}
		}
		if b.Jmp.Kind == ir.JmpRet && b.Jmp.Arg.Kind == ir.RefTemp && ptr[b.Jmp.Arg.ID] {
			return true
		}
	}
	return false
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
		if addr.reg.escapes {
			// A dereference of an escaping runtime pointer: load through the residual
			// pointer parameter.
			return value{kind: vResid, ref: e.cur.Load(loadCls(op), e.materialize(addr)), cls: loadCls(op)}
		}
		return value{kind: vResid, ref: e.inputParam(addr.off, loadCls(op)), cls: loadCls(op)}
	default:
		if _, ok := e.bufferOf(addr.reg); ok {
			// The region was residualized (its address escaped); read from its buffer.
			return value{kind: vResid, ref: e.cur.Load(loadCls(op), e.materialize(addr)), cls: loadCls(op)}
		}
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
	case vSym:
		// A green pointer to global data or a function (a string literal, a table):
		// materialize it as a reference to that symbol.
		return e.out.Sym(v.sym, v.off)
	case vAddr:
		if v.reg == nil {
			e.fail("a green address escaped into a runtime value")
			return ir.R
		}
		var base ir.Ref
		switch v.reg.role {
		case roleInput:
			if !v.reg.escapes {
				e.fail("a read-only input array escaped into a runtime value")
				return ir.R
			}
			// An escaping runtime pointer (ctx, argv): its base is a residual pointer
			// parameter, and this is that pointer offset by a static amount.
			base = e.inputBase(v.reg)
		case roleCode:
			// A green program pointer handed to runtime code (stored as cur_pc for a
			// backtrace, say): its bytes are static, but the pointer itself needs a
			// runtime base -- a residual "code" parameter the caller sets to the real
			// bytecode buffer. Reads still fold; only the escaping pointer uses this.
			if !v.reg.haveBase {
				v.reg.base = e.out.Param("code", ir.ClsP)
				v.reg.haveBase = true
			}
			base = v.reg.base
		case roleVar, roleOther:
			// A local (a JSValue built on the stack, say) whose address is handed to a
			// runtime routine: back it with residual storage holding its current cells.
			base = e.ensureBuffer(v.reg)
		default:
			e.fail("a green address escaped into a runtime value")
			return ir.R
		}
		if e.err != nil {
			return ir.R
		}
		if v.off == 0 {
			return base
		}
		return e.cur.Add(ir.ClsP, base, e.out.ConstInt(ir.ClsL, v.off))
	default:
		e.fail("a green address escaped into a runtime value")
		return ir.R
	}
}

func (e *engine) emitConv(op ir.Op, cls ir.Cls, x ir.Ref) ir.Ref {
	switch op {
	case ir.OExts:
		return e.cur.Exts(x)
	case ir.OTruncd:
		return e.cur.Truncd(x)
	case ir.OStosi:
		return e.cur.Stosi(cls, x)
	case ir.OStoui:
		return e.cur.Stoui(cls, x)
	case ir.OSltof:
		return e.cur.Sltof(cls, x)
	case ir.OUltof:
		return e.cur.Ultof(cls, x)
	case ir.OCast:
		return e.cur.Cast(cls, x)
	}
	e.fail("cannot residualize conversion %v", op)
	return ir.R
}

func (e *engine) emitBin(op ir.Op, cls ir.Cls, a, b ir.Ref) ir.Ref {
	switch op {
	case ir.OAdd:
		return e.cur.Add(cls, a, b)
	case ir.OSub:
		return e.cur.Sub(cls, a, b)
	case ir.OMul:
		return e.cur.Mul(cls, a, b)
	case ir.ODiv:
		return e.cur.Div(cls, a, b)
	case ir.OUDiv:
		return e.cur.UDiv(cls, a, b)
	case ir.ORem:
		return e.cur.Rem(cls, a, b)
	case ir.OURem:
		return e.cur.URem(cls, a, b)
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

func isDivRem(op ir.Op) bool {
	return op == ir.ODiv || op == ir.OUDiv || op == ir.ORem || op == ir.OURem
}

func fold(op ir.Op, a, b int64) int64 {
	switch op {
	case ir.OAdd:
		return a + b
	case ir.OSub:
		return a - b
	case ir.OMul:
		return a * b
	case ir.ODiv:
		return a / b
	case ir.OUDiv:
		return int64(uint64(a) / uint64(b))
	case ir.ORem:
		return a % b
	case ir.OURem:
		return int64(uint64(a) % uint64(b))
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
