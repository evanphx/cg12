package interp

import (
	"fmt"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// compile lowers an ir.Func to bytecode. It reads the pristine SSA without
// mutating it (the tree-walker and backends consume the same *ir.Func), doing
// phi destruction itself: phis become register moves emitted on each outgoing
// edge in the linear stream, so every edge gets its own landing pad and critical
// edges need no block splitting.
func (mc *Machine) compile(f *ir.Func) (*bcFunc, error) {
	c := &compiler{
		mc:         mc,
		f:          f,
		cfg:        analysis.BuildCFG(f),
		reg:        make([]uint16, len(f.Temps)),
		isParam:    make([]bool, len(f.Temps)),
		constSlot:  map[uint32]uint16{},
		constPool:  map[uint32]uint32{},
		blockStart: map[*ir.Block]uint32{},
	}
	if err := c.assignRegisters(); err != nil {
		return nil, err
	}
	c.emitPrologue()
	if err := c.emitBlocks(); err != nil {
		return nil, err
	}
	c.patchJumps()
	if c.err != nil {
		return nil, c.err
	}
	if int(c.frameTop) > maxReg {
		return nil, fmt.Errorf("interp: bytecode: %s needs %d registers, over the %d limit", f.Name, c.frameTop, maxReg)
	}
	paramCls := make([]ir.Cls, len(f.Params))
	for i, p := range f.Params {
		paramCls[i] = p.Cls
	}
	blockPC := make(map[*ir.Block]uint32, len(c.blockStart))
	for b, pc := range c.blockStart {
		blockPC[b] = pc
	}
	return &bcFunc{
		fn:        f,
		code:      c.code,
		consts:    c.consts,
		calls:     c.calls,
		switches:  c.switches,
		sels:      c.sels,
		intrins:   c.intrins,
		blockPC:   blockPC,
		frameSize: c.frameTop,
		nParams:   len(f.Params),
		paramCls:  paramCls,
	}, nil
}

type compiler struct {
	mc  *Machine
	f   *ir.Func
	cfg *analysis.CFG

	reg     []uint16 // temp.ID -> window slot
	isParam []bool   // temp.ID -> is a parameter

	consts    []Value           // const pool
	constPool map[uint32]uint32 // f.Consts index -> pool index
	constSlot map[uint32]uint16 // f.Consts index -> permanent window slot

	code       []inst
	calls      []callSite
	switches   []switchInfo
	sels       []selInfo
	intrins    []intrinInfo
	blockStart map[*ir.Block]uint32
	fixups     []fixup

	localTop uint16 // first outgoing-argument staging slot
	argBase  uint16 // == localTop
	maxArgs  int
	scratch  uint16 // next cycle-break scratch slot
	frameTop uint16 // high-water register count
	err      error
}

type fixup struct {
	pos int
	blk *ir.Block
}

type regMove struct {
	dst, src uint16
	cls      ir.Cls
}

func (c *compiler) constValue(id uint32) (Value, error) {
	con := &c.f.Consts[id]
	switch con.Kind {
	case ir.ConstInt:
		return intVal(con.Cls, con.Int), nil
	case ir.ConstFloat:
		return floatVal(con.Cls, con.Flt), nil
	case ir.ConstSym:
		addr, ok := c.mc.symAddr(con.Sym)
		if !ok {
			return Value{}, fmt.Errorf("interp: bytecode: %s: undefined symbol %q", c.f.Name, con.Sym)
		}
		return ptrVal(addr + uint64(con.Int)), nil
	}
	return Value{}, fmt.Errorf("interp: bytecode: %s: unknown constant kind %d", c.f.Name, con.Kind)
}

func (c *compiler) countMaxArgs() int {
	max := 0
	for _, b := range c.f.Blocks {
		for i := range b.Instrs {
			if in := &b.Instrs[i]; in.Op == ir.OCall {
				if n := len(in.Args) - 1; n > max {
					max = n
				}
			}
		}
	}
	return max
}

// emitPrologue loads every constant into its permanent slot once, before the
// entry block's code.
func (c *compiler) emitPrologue() {
	for id, slot := range c.constSlot {
		c.emit(mkABx(bcLoadK, c.consts[c.constPool[id]].Cls, 0, slot, c.constPool[id]))
	}
}

func (c *compiler) emitBlocks() error {
	if len(c.f.Start.Phis) != 0 {
		return fmt.Errorf("interp: bytecode: %s entry block has phis", c.f.Name)
	}
	for _, b := range c.cfg.RPO {
		c.blockStart[b] = uint32(len(c.code))
		for i := range b.Instrs {
			c.emitInstr(&b.Instrs[i])
		}
		c.emitTerminator(b)
		if c.err != nil {
			return c.err
		}
	}
	return nil
}

func (c *compiler) emit(w inst) int {
	c.code = append(c.code, w)
	return len(c.code) - 1
}

func (c *compiler) fail(format string, a ...any) {
	if c.err == nil {
		c.err = fmt.Errorf("interp: bytecode: %s: "+format, append([]any{c.f.Name}, a...)...)
	}
}

// refReg resolves an operand to its window slot (temp or constant).
func (c *compiler) refReg(r ir.Ref) uint16 {
	switch r.Kind {
	case ir.RefTemp:
		return c.reg[r.ID]
	case ir.RefConst:
		return c.constSlot[r.ID]
	default:
		c.fail("operand ref kind %d has no register", r.Kind)
		return 0
	}
}

func (c *compiler) newScratch() uint16 {
	s := c.scratch
	c.scratch++
	if c.scratch > c.frameTop {
		c.frameTop = c.scratch
	}
	return s
}
