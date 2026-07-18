package interp

import "github.com/evanphx/cg12/ir"

// vmLoad reads memory at addr with the op's width and signedness, extending into
// the result class -- the register-based twin of execLoad (ops.go).
func (mc *Machine) vmLoad(op ir.Op, cls ir.Cls, addr uint64) (Value, error) {
	switch op {
	case ir.OLoadsb:
		v, err := mc.memLoad(addr, 1)
		return intVal(cls, int64(int8(v))), err
	case ir.OLoadub:
		v, err := mc.memLoad(addr, 1)
		return intVal(cls, int64(uint8(v))), err
	case ir.OLoadsh:
		v, err := mc.memLoad(addr, 2)
		return intVal(cls, int64(int16(v))), err
	case ir.OLoaduh:
		v, err := mc.memLoad(addr, 2)
		return intVal(cls, int64(uint16(v))), err
	case ir.OLoadsw:
		v, err := mc.memLoad(addr, 4)
		return intVal(cls, int64(int32(v))), err
	case ir.OLoaduw:
		v, err := mc.memLoad(addr, 4)
		return intVal(cls, int64(uint32(v))), err
	case ir.OLoadl:
		v, err := mc.memLoad(addr, 8)
		return intVal(cls, int64(v)), err
	case ir.OLoads:
		v, err := mc.memLoad(addr, 4)
		return Value{Cls: ir.ClsS, Bits: v & 0xffffffff}, err
	case ir.OLoadd:
		v, err := mc.memLoad(addr, 8)
		return Value{Cls: ir.ClsD, Bits: v}, err
	}
	return Value{}, mc.trapf("bytecode: unhandled load %s", op)
}

// vmStore writes val to addr at the op's width -- the twin of execStore.
func (mc *Machine) vmStore(op ir.Op, val Value, addr uint64) error {
	size := storeSize(op)
	if size == 0 {
		return mc.trapf("bytecode: unhandled store %s", op)
	}
	if !mc.mem.store(addr, size, val.Bits) {
		return mc.trapf("store %d bytes at %#x: unmapped", size, addr)
	}
	return nil
}

// vmAlloc reserves stack space for an alloc op -- the twin of execAlloc.
func (mc *Machine) vmAlloc(op ir.Op, size uint64) (uint64, error) {
	var align uint64
	switch op {
	case ir.OAlloc4:
		align = 4
	case ir.OAlloc8:
		align = 8
	default: // OAlloc16, OAllocN
		align = 16
		if op == ir.OAllocN && size%16 != 0 {
			return 0, mc.trapf("allocn size %d is not a multiple of 16", size)
		}
	}
	return mc.stackAlloc(size, align), nil
}
