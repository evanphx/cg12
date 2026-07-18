package interp

import (
	"fmt"
	"math"

	"github.com/evanphx/cg12/ir"
)

// layoutGlobals places every ir.Data into the globals segment and writes its
// initializer, in two linker-like passes so a datum holding the address of a
// later global (or a function) resolves. It mirrors arm64/mc.go addData
// item-for-item.
func (mc *Machine) layoutGlobals() error {
	type placed struct {
		d    *ir.Data
		addr uint64
	}
	var order []placed
	cursor := uint64(segGlobal)

	// Pass 1: assign an address to each datum and reserve its bytes.
	for _, d := range mc.mod.Data {
		align := uint64(d.Align)
		if align == 0 {
			align = 1
		}
		cursor = roundUpU(cursor, align)
		if _, dup := mc.syms[d.Name]; dup {
			return fmt.Errorf("interp: duplicate symbol %q", d.Name)
		}
		mc.syms[d.Name] = cursor
		sz := dataSize(d)
		mc.mem.mapRange(cursor, sz)
		order = append(order, placed{d, cursor})
		cursor += sz
	}

	// Pass 2: write initializers.
	for _, pl := range order {
		if err := mc.writeData(pl.d, pl.addr); err != nil {
			return err
		}
	}
	return nil
}

// dataSize is the byte length of a data definition's initializer.
func dataSize(d *ir.Data) uint64 {
	var n uint64
	for _, it := range d.Items {
		n += itemSize(it)
	}
	return n
}

func itemSize(it ir.DataItem) uint64 {
	switch {
	case it.Zero > 0:
		return uint64(it.Zero)
	case it.Str != "":
		return uint64(len(it.Str))
	case it.Sym != "":
		return 8
	case len(it.Flts) > 0:
		return uint64(len(it.Flts)) * uint64(it.Sub.Size())
	case len(it.Ints) > 0:
		return uint64(len(it.Ints)) * uint64(it.Sub.Size())
	}
	return 0
}

// writeData writes a definition's items into memory starting at addr.
func (mc *Machine) writeData(d *ir.Data, addr uint64) error {
	for _, it := range d.Items {
		switch {
		case it.Zero > 0:
			// Reserved pages already read as zero.
			addr += uint64(it.Zero)
		case it.Str != "":
			mc.mem.writeBytes(addr, []byte(it.Str))
			addr += uint64(len(it.Str))
		case it.Sym != "":
			target, ok := mc.symAddr(it.Sym)
			if !ok {
				return fmt.Errorf("interp: %s: initializer references undefined symbol %q", d.Name, it.Sym)
			}
			mc.mem.store(addr, 8, target+uint64(it.Off))
			addr += 8
		case len(it.Flts) > 0:
			sz := it.Sub.Size()
			for _, fv := range it.Flts {
				mc.mem.store(addr, sz, floatBits(it.Sub, fv))
				addr += uint64(sz)
			}
		case len(it.Ints) > 0:
			sz := it.Sub.Size()
			for _, iv := range it.Ints {
				mc.mem.store(addr, sz, uint64(iv))
				addr += uint64(sz)
			}
		}
	}
	return nil
}

// floatBits encodes a float value at the element's width (single or double).
func floatBits(sub ir.SubCls, f float64) uint64 {
	if sub.Size() == 4 {
		return uint64(math.Float32bits(float32(f)))
	}
	return math.Float64bits(f)
}

func roundUpU(n, a uint64) uint64 {
	if a <= 1 {
		return n
	}
	return (n + a - 1) &^ (a - 1)
}
