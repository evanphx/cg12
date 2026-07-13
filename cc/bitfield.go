package cc

import (
	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// Bit fields are stored packed inside an "access unit" of AccessBytes bytes at
// Offset within the struct. A field occupies ValueBits bits starting OffsetBits
// into that unit; Mask marks those bits. Reading and writing therefore load the
// whole unit and extract or splice the bits, rather than addressing the field
// directly (a bit field has no byte address).

// asBitfield reports whether e is a bit-field member access, returning the field
// and the address of its access unit. It evaluates the base once (so callers
// must not also evaluate it), which is what a read-modify-write needs.
func (g *gen) asBitfield(e cc.ExpressionNode) (*cc.Field, ir.Ref, bool) {
	pe, ok := e.(*cc.PostfixExpression)
	if !ok {
		return nil, ir.R, false
	}
	f := pe.Field()
	if f == nil || !f.IsBitfield() {
		return nil, ir.R, false
	}
	var base ir.Ref
	switch pe.Case {
	case cc.PostfixExpressionSelect: // s.f
		base, _ = g.genAddr(pe.PostfixExpression)
	case cc.PostfixExpressionPSelect: // p->f
		base = g.genExpr(pe.PostfixExpression)
	default:
		return nil, ir.R, false
	}
	return f, g.offset(base, int(f.Offset())), true
}

// bitCls picks the register class used to compute on an access unit of the given
// size, and the number of bits in that register (the width the extraction shifts
// against).
func bitCls(accessBytes int64) (ir.Cls, int64) {
	if accessBytes <= 4 {
		return ir.ClsW, 32
	}
	return ir.ClsL, 64
}

// iconst builds an integer constant of a specific class (for shift counts and
// masks that must match the unit's computation class).
func (g *gen) iconst(cls ir.Cls, v int64) ir.Ref {
	if cls == ir.ClsL {
		return g.fn.Long(v)
	}
	return g.fn.Word(v)
}

func (g *gen) loadUnit(addr ir.Ref, bytes int64, cls ir.Cls) ir.Ref {
	switch bytes {
	case 1:
		return g.cur.LoadSub(cls, ir.SubUB, addr)
	case 2:
		return g.cur.LoadSub(cls, ir.SubUH, addr)
	case 4:
		return g.cur.Load(ir.ClsW, addr)
	default:
		return g.cur.Load(ir.ClsL, addr)
	}
}

func (g *gen) storeUnit(addr, val ir.Ref, bytes int64) {
	switch bytes {
	case 1:
		g.cur.StoreSub(ir.SubB, val, addr)
	case 2:
		g.cur.StoreSub(ir.SubH, val, addr)
	default:
		g.cur.Store(val, addr) // val's class (W or L) fixes the 4- or 8-byte width
	}
}

// readBitfield loads the access unit and extracts the field: shift it up so its
// top bit sits at the register's sign bit, then shift back down — arithmetically
// for a signed field so the value sign-extends, logically otherwise.
func (g *gen) readBitfield(unit ir.Ref, f *cc.Field) ir.Ref {
	cls, regBits := bitCls(f.AccessBytes())
	v := g.loadUnit(unit, f.AccessBytes(), cls)
	off := int64(f.OffsetBits())
	vb := f.ValueBits()
	if left := regBits - off - vb; left > 0 {
		v = g.cur.Shl(cls, v, g.iconst(cls, left))
	}
	if signed(f.Type()) {
		return g.cur.Sar(cls, v, g.iconst(cls, regBits-vb))
	}
	return g.cur.Shr(cls, v, g.iconst(cls, regBits-vb))
}

// writeBitfield splices val into the field's bits, preserving the rest of the
// access unit: clear the masked bits, OR in the shifted-and-masked value.
func (g *gen) writeBitfield(unit, val ir.Ref, f *cc.Field) {
	cls, _ := bitCls(f.AccessBytes())
	val = g.toCls(val, cls)
	old := g.loadUnit(unit, f.AccessBytes(), cls)
	mask := f.Mask()
	placed := g.cur.Shl(cls, val, g.iconst(cls, int64(f.OffsetBits())))
	placed = g.cur.And(cls, placed, g.iconst(cls, int64(mask)))
	cleared := g.cur.And(cls, old, g.iconst(cls, int64(^mask)))
	g.storeUnit(unit, g.cur.Or(cls, cleared, placed), f.AccessBytes())
}

// toCls coerces an integer value to a computation class, widening or truncating
// as needed (used when a bit field's storage is wider than its declared type's
// natural class).
func (g *gen) toCls(v ir.Ref, cls ir.Cls) ir.Ref {
	if g.fn.ClassOf(v) == cls {
		return v
	}
	if cls == ir.ClsL {
		return g.cur.Extuw(ir.ClsL, v)
	}
	return g.cur.Copy(ir.ClsW, v)
}
