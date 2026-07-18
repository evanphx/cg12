package interp

import "github.com/evanphx/cg12/ir"

// A second interpreter: a register bytecode VM. Where the tree-walker executes
// the SSA directly (interp.go/exec.go/ops.go), this compiles each function to a
// linear instruction stream over a Lua-5-style rolling register window and runs
// that. It shares the whole runtime -- memory, Value, externs, and every pure
// arithmetic/conversion/memory helper -- so the two engines are independent code
// paths that must agree, strengthening the differential oracle: the compiler
// performs SSA destruction (phis to moves) and register allocation, the two
// transforms every real backend performs, which the tree-walker does neither of.
//
// An instruction is one uint64 in one of two Lua-like formats, decoded by the
// helpers below (no bit-twiddling at the call sites):
//
//	iABC: op(8) | cls(8) | aux(8) | a(12) | b(12) | c(12)   -- up to three registers
//	iABx: op(8) | cls(8) | aux(8) | a(12) | bx(28)          -- a register + a 28-bit index
//
// cls is the result (or, for a compare, need not be the operand) class; aux
// carries an ir.Op or ir.Cmp for the generic families so the executor can call
// the same intBinop/intCmp/... the tree-walker uses. Registers are 12-bit window
// slots (<= maxReg); anything wider than a word -- constants, call descriptors,
// switch tables -- lives in a per-function side table indexed by bx.
type inst uint64

// maxReg is the largest addressable window slot (12-bit register fields). No real
// frame approaches it; the compiler errors if a function needs more.
const maxReg = 1<<12 - 1

// bcOp is a bytecode opcode. The value families (bcBinop/bcCmp/bcUnop/bcLoad/
// bcStore/bcAlloc) carry the concrete ir.Op or ir.Cmp in the aux byte and reuse
// the tree-walker's pure semantic helpers, so the two engines cannot drift.
type bcOp uint8

const (
	bcNop       bcOp = iota
	bcMove           // R[a] = R[b]                       (cls = class)
	bcLoadK          // R[a] = const[bx]                  (iABx)
	bcBinop          // R[a] = binop(aux, cls, R[b], R[c])
	bcCmp            // R[a] = cmp(aux, R[b], R[c])       (aux = ir.Cmp)
	bcSel            // R[a] = select via sels[bx]        (iABx; needs 3 source regs)
	bcUnop           // R[a] = unop(aux, cls, R[b])       (neg/clz/ext/conv/cast/copy)
	bcLoad           // R[a] = load(aux) from R[b]        (aux = ir.OLoad*)
	bcStore          // store(aux) R[a] to R[b]           (aux = ir.OStore*)
	bcAlloc          // R[a] = alloc(aux) of R[b] bytes   (aux = ir.OAlloc*)
	bcBlit           // memmove(R[a] <- R[b], c bytes)
	bcCall           // call site calls[bx]               (iABx)
	bcVaStart        // va_start at R[a]
	bcVaArg          // R[a] = va_arg from *R[b]          (cls = class)
	bcBlockAddr      // R[a] = code address of block bx   (iABx; bx into blockPCs)
	bcJmp            // pc = bx                           (iABx)
	bcJnz            // if R[a] != 0 pc = bx              (iABx; else falls through)
	bcRet            // return R[a] (aux != 0) or void    (cls = Retty)
	bcSwitch         // switch R[a] via switches[bx]      (iABx)
	bcTable          // jump table R[a] via switches[bx]  (iABx)
	bcBr             // computed goto to address in R[a]
	bcHlt            // trap: unreachable
	bcIntrin         // named intrinsic via intrins[bx]         (iABx)
)

// Bit layout (from the MSB): op[56:64) cls[48:56) aux[40:48) a[28:40) b[16:28)
// c[4:16); the low 4 bits are spare. iABx overlays bx on [0:28).
func mkABC(op bcOp, cls ir.Cls, aux uint8, a, b, c uint16) inst {
	return inst(op)<<56 | inst(cls)<<48 | inst(aux)<<40 |
		inst(a&maxReg)<<28 | inst(b&maxReg)<<16 | inst(c&maxReg)<<4
}

func mkABx(op bcOp, cls ir.Cls, aux uint8, a uint16, bx uint32) inst {
	return inst(op)<<56 | inst(cls)<<48 | inst(aux)<<40 |
		inst(a&maxReg)<<28 | inst(bx&0x0fff_ffff)
}

func (w inst) op() bcOp    { return bcOp(w >> 56) }
func (w inst) cls() ir.Cls { return ir.Cls(w >> 48) }
func (w inst) aux() uint8  { return uint8(w >> 40) }
func (w inst) ra() uint16  { return uint16(w>>28) & maxReg }
func (w inst) rb() uint16  { return uint16(w>>16) & maxReg }
func (w inst) rc() uint16  { return uint16(w>>4) & maxReg }
func (w inst) bx() uint32  { return uint32(w) & 0x0fff_ffff }

// callSite is everything a bcCall needs that does not fit a word: who to call,
// where its arguments are staged in the window, where the result lands, and the
// aggregate shape of each. Indexed by a bcCall's bx.
type callSite struct {
	name      string        // direct callee (IR func or extern), or "" for indirect
	calleeReg uint16        // indirect callee address register, when name == ""
	argBase   uint16        // first staged-argument register (window-relative)
	nArgs     int           // number of arguments staged at argBase..
	resultReg uint16        // where the result is bound
	hasResult bool          // whether the call yields a value
	resultCls ir.Cls        // class to bind the result as
	aggArgs   []*ir.AggType // parallel to args: by-value aggregate types, nil = scalar
	retAgg    *ir.AggType   // non-nil when the result is a by-value aggregate
}

// switchInfo backs a bcSwitch/bcTable: the case table or target list plus the
// default, resolved to bytecode pcs. Indexed by bx.
type switchInfo struct {
	signed  bool
	cases   []switchCasePC // bcSwitch: value -> pc
	targets []uint32       // bcTable: index -> pc
	dflt    uint32         // default pc (bcSwitch)
	argCls  ir.Cls         // dispatched value's class, for signed/unsigned match
}

type switchCasePC struct {
	val int64
	pc  uint32
}

// selInfo backs a bcSel: the three source registers of a select, since one word
// cannot hold a destination plus three sources. Indexed by bx.
type selInfo struct {
	cond, a, b uint16
}

// intrinInfo backs a bcIntrin: the intrinsic name to dispatch on, the result
// register (dst, unused when the intrinsic yields no value), and the operand
// registers -- more than one word can carry. Indexed by bx.
type intrinInfo struct {
	name string
	dst  uint16
	args []uint16
}

// bcFunc is a compiled function: its instruction stream, the side tables its
// instructions index, and the window size to reserve for a call.
type bcFunc struct {
	fn        *ir.Func
	code      []inst
	consts    []Value              // const pool, indexed by bcLoadK's bx
	calls     []callSite           // indexed by bcCall's bx
	switches  []switchInfo         // indexed by bcSwitch/bcTable's bx
	sels      []selInfo            // indexed by bcSel's bx
	intrins   []intrinInfo         // indexed by bcIntrin's bx
	blockPC   map[*ir.Block]uint32 // address-taken block -> its pc, for computed goto (bcBr)
	frameSize uint16               // window slots to reserve
	nParams   int
	paramCls  []ir.Cls // params, for normalizing incoming values
}
