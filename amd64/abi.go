package amd64

import (
	"github.com/evanphx/cg12/ir"
)

// ebClass is the System V class of one eightbyte (8-byte chunk) of an aggregate.
type ebClass int

const (
	ebNone    ebClass = iota // NO_CLASS: only padding so far
	ebInteger                // INTEGER: passed in a general-purpose register
	ebSSE                    // SSE: passed in an XMM register
)

// merge combines two eightbyte classes per the System V rules: NO_CLASS yields to
// anything, and INTEGER dominates SSE.
func merge(a, b ebClass) ebClass {
	switch {
	case a == b:
		return a
	case a == ebNone:
		return b
	case b == ebNone:
		return a
	default:
		return ebInteger
	}
}

// aggClass classifies a by-value aggregate for the System V AMD64 ABI.
type aggClass struct {
	memory bool      // > 16 bytes: passed in memory (by value on the stack / sret)
	parts  []ebClass // per-eightbyte class when register-passed (len 1 or 2)
	size   int
}

// nGP / nSSE count the integer and SSE eightbytes of a register-passed aggregate.
func (c aggClass) nGP() (n int) {
	for _, p := range c.parts {
		if p == ebInteger {
			n++
		}
	}
	return n
}
func (c aggClass) nSSE() (n int) {
	for _, p := range c.parts {
		if p == ebSSE {
			n++
		}
	}
	return n
}

// classifyAgg classifies an aggregate type per the System V AMD64 rules: bigger
// than two eightbytes is MEMORY; otherwise each eightbyte is INTEGER or SSE.
func classifyAgg(t *ir.AggType) aggClass {
	size, _ := t.Layout()
	if size == 0 || size > 16 {
		return aggClass{memory: true, size: size}
	}
	parts := make([]ebClass, (size+7)/8)
	classifyAggInto(t, 0, parts)
	for i := range parts {
		if parts[i] == ebNone {
			parts[i] = ebSSE // an all-padding eightbyte is treated as SSE
		}
	}
	return aggClass{parts: parts, size: size}
}

// classifyAggInto merges every scalar leaf of t (offset by base) into the
// eightbyte(s) it occupies.
// classifyAggInto classifies every scalar leaf of t into the eightbytes it
// covers, at base within the aggregate being classified.
//
// Where each leaf is comes from ir.AggType.Leaves rather than from walking the
// fields and applying the placement rule again here. A union's cases overlap, so
// several leaves land in one eightbyte and merge, which is exactly SysV's rule
// for a union.
func classifyAggInto(t *ir.AggType, base int, parts []ebClass) {
	leaves, _ := t.Leaves()
	for _, l := range leaves {
		cls := ebInteger
		if l.Sub == ir.SubS || l.Sub == ir.SubD {
			cls = ebSSE
		}
		off := base + l.Off
		lo, hi := off/8, (off+l.Sub.Size()-1)/8
		for eb := lo; eb <= hi && eb < len(parts); eb++ {
			parts[eb] = merge(parts[eb], cls)
		}
	}
}

// aggReg is one eightbyte's destination register plus whether it is an SSE
// (XMM) register.
type aggReg struct {
	reg   Reg
	float bool
}

// assignAgg assigns each eightbyte of a register-class aggregate to a GP or SSE
// argument register, or places the whole aggregate on the stack when the needed
// registers are unavailable.
func (a *argAssigner) assignAgg(cls aggClass) (regs []aggReg, onStack bool, off int) {
	if cls.memory || a.ngrn+cls.nGP() > len(argGP) || a.nsrn+cls.nSSE() > len(argFP) {
		off = a.nsaa
		a.nsaa += roundUp(cls.size, 8)
		return nil, true, off
	}
	for _, p := range cls.parts {
		if p == ebInteger {
			regs = append(regs, aggReg{reg: argGP[a.ngrn]})
			a.ngrn++
		} else {
			regs = append(regs, aggReg{reg: argFP[a.nsrn], float: true})
			a.nsrn++
		}
	}
	return regs, false, 0
}

// ebOps returns the load/store ops and register class to move one eightbyte of
// the given class and byte width (the tail eightbyte may be narrower than 8).
func ebOps(float bool, bytes int) (load, store ir.Op, cls ir.Cls) {
	if float {
		if bytes >= 8 {
			return ir.OLoadd, ir.OStored, ir.ClsD
		}
		return ir.OLoads, ir.OStores, ir.ClsS
	}
	switch {
	case bytes >= 8:
		return ir.OLoadl, ir.OStorel, ir.ClsL
	case bytes >= 4:
		return ir.OLoaduw, ir.OStorew, ir.ClsW
	case bytes >= 2:
		return ir.OLoaduh, ir.OStoreh, ir.ClsW
	default:
		return ir.OLoadub, ir.OStoreb, ir.ClsW
	}
}

// offsetAddr returns a reference to base + off, appending an add when off != 0.
func offsetAddr(f *ir.Func, base ir.Ref, off int, out *[]ir.Instr) ir.Ref {
	if off == 0 {
		return base
	}
	t := f.NewTemp("", ir.ClsL)
	*out = append(*out, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: t, Args: []ir.Ref{base, f.Long(int64(off))}})
	return t
}

// emitMemcpy copies size bytes from src to dst using the largest power-of-two
// chunk at each step.
func emitMemcpy(f *ir.Func, dst, src ir.Ref, size int, out *[]ir.Instr) {
	chunk := func(off int, loadOp, storeOp ir.Op, cls ir.Cls) {
		s := offsetAddr(f, src, off, out)
		d := offsetAddr(f, dst, off, out)
		v := f.NewTemp("", cls)
		*out = append(*out, ir.Instr{Op: loadOp, Cls: cls, To: v, Args: []ir.Ref{s}})
		*out = append(*out, ir.Instr{Op: storeOp, Cls: cls, Args: []ir.Ref{v, d}})
	}
	off := 0
	for size-off >= 8 {
		chunk(off, ir.OLoadl, ir.OStorel, ir.ClsL)
		off += 8
	}
	for size-off >= 4 {
		chunk(off, ir.OLoaduw, ir.OStorew, ir.ClsW)
		off += 4
	}
	for size-off >= 2 {
		chunk(off, ir.OLoaduh, ir.OStoreh, ir.ClsW)
		off += 2
	}
	for size-off >= 1 {
		chunk(off, ir.OLoadub, ir.OStoreb, ir.ClsW)
		off++
	}
}

// ebBytes returns the meaningful byte count of eightbyte i of a size-byte
// aggregate (8 for a full eightbyte, less for the tail).
func ebBytes(size, i int) int {
	if r := size - i*8; r < 8 {
		return r
	}
	return 8
}

// loadAggInt assembles the low `bytes` (1..8) of an integer eightbyte at addr
// into a full 64-bit value, loading no more than `bytes` so it never reads past
// an odd-sized aggregate -- a packed struct, or any struct whose eightbyte has a
// 3/5/6/7-byte tail. Chunks are zero-extended and OR'd at their byte positions; a
// whole eightbyte is a single 8-byte load. The value is ClsL because an integer
// eightbyte travels in a whole general register.
func loadAggInt(f *ir.Func, addr ir.Ref, bytes int, out *[]ir.Instr) ir.Ref {
	if bytes >= 8 {
		v := f.NewTemp("", ir.ClsL)
		*out = append(*out, ir.Instr{Op: ir.OLoadl, Cls: ir.ClsL, To: v, Args: []ir.Ref{addr}})
		return v
	}
	var acc ir.Ref
	first := true
	off, shift := 0, 0
	for _, ch := range []struct {
		w  int
		op ir.Op
	}{{4, ir.OLoaduw}, {2, ir.OLoaduh}, {1, ir.OLoadub}} {
		if bytes-off < ch.w {
			continue
		}
		v := f.NewTemp("", ir.ClsL)
		*out = append(*out, ir.Instr{Op: ch.op, Cls: ir.ClsL, To: v, Args: []ir.Ref{offsetAddr(f, addr, off, out)}})
		if shift > 0 {
			s := f.NewTemp("", ir.ClsL)
			*out = append(*out, ir.Instr{Op: ir.OShl, Cls: ir.ClsL, To: s, Args: []ir.Ref{v, f.Long(int64(shift))}})
			v = s
		}
		if first {
			acc, first = v, false
		} else {
			o := f.NewTemp("", ir.ClsL)
			*out = append(*out, ir.Instr{Op: ir.OOr, Cls: ir.ClsL, To: o, Args: []ir.Ref{acc, v}})
			acc = o
		}
		off += ch.w
		shift += ch.w * 8
	}
	return acc
}
