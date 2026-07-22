package cc

import (
	moderncc "github.com/evanphx/cg12/internal/cc"
	"github.com/evanphx/cg12/ir"
)

// This file handles by-value aggregates (structs and unions passed, returned,
// assigned, and returned by value). cg12 represents an aggregate value as a
// pointer to its storage: a parameter arrives as a pointer to a reconstructed
// slot, a return passes a pointer the backend copies out, a call argument is a
// pointer the backend loads into registers, and a call result comes back as a
// pointer to a buffer. So the front end only ever moves addresses around and
// copies bytes; the backend's ABI lowering does the register work.

// isAggType reports whether t is a struct or union (an aggregate passed by value
// rather than in a single register).
func isAggType(t moderncc.Type) bool {
	switch t.(type) {
	case *moderncc.StructType, *moderncc.UnionType:
		return true
	}
	return false
}

// subOfType maps a scalar C type to the cg12 sub-class that records its width
// and kind for aggregate field layout and ABI classification.
func subOfType(t moderncc.Type) ir.SubCls {
	switch t.Kind() {
	case moderncc.Float:
		return ir.SubS
	case moderncc.Double:
		return ir.SubD
	case moderncc.LongDouble:
		return ir.SubQ
	default:
		return subFor(int(t.Size()))
	}
}

// aggOf builds (and memoizes) the cg12 aggregate type mirroring a C struct or
// union, registering it with the module. It is reached only where an aggregate
// crosses the ABI by value -- a parameter, an argument, a return -- which is
// exactly where its layout has to be right.
//
// The type is built from the members' types, and cg12's layout rules then
// reproduce the natural C ones. That is an assumption, not a derivation: this
// never consults the type checker's own offsets. Where the two disagree the
// backends classify from this type and place the fields somewhere C did not, so
// the check below is what stands between a disagreement and a struct that
// arrives at a gcc-compiled callee with its fields moved.
func (g *gen) aggOf(t moderncc.Type) *ir.AggType {
	if a, ok := g.aggs[t]; ok {
		return a
	}
	agg := &ir.AggType{Name: aggName(t)}
	// A packed aggregate places its members with no padding; the ir type carries a
	// Packed flag so its Layout and by-value ABI classification see the packed
	// offsets. The explicit Align keeps a packed struct's alignment (1, or a raised
	// aligned(N)) even though no member sets it.
	if t.Attributes().IsPacked() {
		agg.Packed = true
		agg.Align = t.Align()
	}
	g.aggs[t] = agg // register before recursing so a self-referential pointer resolves

	// A bitfield defeats the field-by-field model: its Type() is the whole storage
	// unit, so cg12 would lay `unsigned a:1, b:1` out as two words where C packs
	// both into one. cg12's aggregate types cannot express a bitfield, but they do
	// not need to for a struct that is copied by value or passed to a call: only its
	// size and alignment matter there (an individual bitfield is reached through a
	// pointer at its own offset, see bitfield.go, never through this type). So model
	// the whole aggregate as an opaque blob of C's size.
	if hasBitfield(t) {
		agg.Opaque = true
		agg.Size = int(t.Size())
		agg.Align = t.Align()
		g.mod.AddType(agg)
		return agg
	}

	switch st := t.(type) {
	case *moderncc.StructType:
		for i := 0; i < st.NumFields(); i++ {
			agg.Fields = append(agg.Fields, g.fieldOf(st.FieldByIndex(i).Type()))
		}
	case *moderncc.UnionType:
		agg.Union = true
		for i := 0; i < st.NumFields(); i++ {
			agg.Cases = append(agg.Cases, []ir.Field{g.fieldOf(st.FieldByIndex(i).Type())})
		}
	}
	g.mod.AddType(agg)
	g.checkAggLayout(t, agg)
	return agg
}

// hasBitfield reports whether a struct or union has a bitfield member.
func hasBitfield(t moderncc.Type) bool {
	switch st := t.(type) {
	case *moderncc.StructType:
		for i := 0; i < st.NumFields(); i++ {
			if st.FieldByIndex(i).IsBitfield() {
				return true
			}
		}
	case *moderncc.UnionType:
		for i := 0; i < st.NumFields(); i++ {
			if st.FieldByIndex(i).IsBitfield() {
				return true
			}
		}
	}
	return false
}

// checkAggLayout requires cg12's layout of an aggregate to be the one C gave it,
// a safety net for the by-value paths that reason from the ir aggregate.
//
// A bitfield is the case it guards: its Type() is the whole storage type, so
// `unsigned a:1, b:1` builds two four-byte fields where C packs both into one,
// and the type comes out too big. (Bitfield aggregates take the opaque path
// before reaching here; this catches any that slip through.) A packed struct now
// carries the Packed flag, so its layout matches the checker's and passes.
func (g *gen) checkAggLayout(t moderncc.Type, agg *ir.AggType) {
	size, align := agg.Layout()
	if size == int(t.Size()) && align == t.Align() {
		return
	}
	g.fail("cc: cannot pass %s by value: cg12 lays it out as %d bytes (align %d) "+
		"but C says %d (align %d) -- a bitfield, which cg12's aggregate types "+
		"cannot yet express", aggName(t), size, align, t.Size(), t.Align())
}

// fieldOf maps one C member type to an aggregate field: a nested aggregate keeps
// its type, an array becomes a repeated element, and a scalar becomes its sub-class.
func (g *gen) fieldOf(t moderncc.Type) ir.Field {
	if isAggType(t) {
		return ir.Field{Type: g.aggOf(t)}
	}
	if at, ok := t.(*moderncc.ArrayType); ok {
		elem := at.Elem()
		if isAggType(elem) {
			return ir.Field{Type: g.aggOf(elem), Count: int(at.Len())}
		}
		return ir.Field{Sub: subOfType(elem), Count: int(at.Len())}
	}
	return ir.Field{Sub: subOfType(t)}
}

// aggName returns a readable name for an aggregate type (for the printed IL).
func aggName(t moderncc.Type) string {
	if st, ok := t.(*moderncc.StructType); ok {
		if tok := st.Tag(); tok.SrcStr() != "" {
			return "struct." + tok.SrcStr()
		}
	}
	if ut, ok := t.(*moderncc.UnionType); ok {
		if tok := ut.Tag(); tok.SrcStr() != "" {
			return "union." + tok.SrcStr()
		}
	}
	return "anon"
}

// copyAgg copies size bytes from src to dst, the value-copy that struct
// assignment, initialization, and by-value passing all reduce to.
func (g *gen) copyAgg(dst, src ir.Ref, size int) {
	off := 0
	for size-off >= 8 {
		g.cur.Store(g.cur.Load(ir.ClsL, g.offset(src, off)), g.offset(dst, off))
		off += 8
	}
	if size-off >= 4 {
		g.cur.Store(g.cur.Load(ir.ClsW, g.offset(src, off)), g.offset(dst, off))
		off += 4
	}
	if size-off >= 2 {
		g.cur.StoreSub(ir.SubH, g.cur.LoadSub(ir.ClsW, ir.SubUH, g.offset(src, off)), g.offset(dst, off))
		off += 2
	}
	if size-off >= 1 {
		g.cur.StoreSub(ir.SubB, g.cur.LoadSub(ir.ClsW, ir.SubUB, g.offset(src, off)), g.offset(dst, off))
	}
}

// aggParam adds a by-value aggregate parameter and returns its pointer temp: the
// backend reconstructs the incoming struct into a slot and hands us its address.
func (g *gen) aggParam(name string, agg *ir.AggType) ir.Ref {
	r := g.fn.NewTemp(name, ir.ClsL)
	t := g.fn.Temp(r)
	t.Agg = agg
	g.fn.Params = append(g.fn.Params, t)
	return r
}

// checkPackedBitfield refuses the packed-bitfield cases cg12 cannot access safely.
//
// The type checker lays a packed struct's bit fields out as a bit stream and
// gives each one an access unit (Offset/AccessBytes) that cg12 reads and writes
// as a whole. A read-modify-write of that unit is only safe if it stays within
// the struct's own storage, and cg12 loads only 1/2/4/8-byte units. A bit field
// wide enough to need a unit that overruns the struct is refused rather than read
// or written out of bounds. Packed unions with bit fields are not modeled at all.
// A non-packed bitfield, or a packed struct whose bit fields all fit, is fine.
func (g *gen) checkPackedBitfield(t moderncc.Type) {
	if !t.Attributes().IsPacked() || !hasBitfield(t) {
		return
	}
	st, ok := t.(*moderncc.StructType)
	if !ok {
		g.fail("cc: %s is a packed union with a bitfield, which cg12 does not model", aggName(t))
		return
	}
	size := st.Size()
	for i := 0; i < st.NumFields(); i++ {
		f := st.FieldByIndex(i)
		if !f.IsBitfield() {
			continue
		}
		ab := f.AccessBytes()
		if ab != 1 && ab != 2 && ab != 4 && ab != 8 || f.Offset()+ab > size {
			g.fail("cc: %s has a packed bitfield too wide for cg12 to access "+
				"within the struct's %d bytes (it would need an out-of-bounds load)",
				aggName(t), size)
			return
		}
	}
}
