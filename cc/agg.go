package cc

import (
	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
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
func isAggType(t cc.Type) bool {
	switch t.(type) {
	case *cc.StructType, *cc.UnionType:
		return true
	}
	return false
}

// subOfType maps a scalar C type to the cg12 sub-class that records its width
// and kind for aggregate field layout and ABI classification.
func subOfType(t cc.Type) ir.SubCls {
	switch t.Kind() {
	case cc.Float:
		return ir.SubS
	case cc.Double:
		return ir.SubD
	case cc.LongDouble:
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
func (g *gen) aggOf(t cc.Type) *ir.AggType {
	if a, ok := g.aggs[t]; ok {
		return a
	}
	agg := &ir.AggType{Name: aggName(t)}
	g.aggs[t] = agg // register before recursing so a self-referential pointer resolves
	switch st := t.(type) {
	case *cc.StructType:
		for i := 0; i < st.NumFields(); i++ {
			agg.Fields = append(agg.Fields, g.fieldOf(st.FieldByIndex(i).Type()))
		}
	case *cc.UnionType:
		agg.Union = true
		for i := 0; i < st.NumFields(); i++ {
			agg.Cases = append(agg.Cases, []ir.Field{g.fieldOf(st.FieldByIndex(i).Type())})
		}
	}
	g.mod.AddType(agg)
	g.checkPacked(t)
	g.checkAggLayout(t, agg)
	return agg
}

// checkAggLayout requires cg12's layout of an aggregate to be the one C gave it.
//
// A bitfield is the case that breaks it: its Type() is the whole storage type,
// so `unsigned a:1, b:1` builds two four-byte fields where C packs both into
// one, and the type comes out five times too big. Same-unit calls still work,
// because both sides share the same wrong layout -- it only shows when the
// struct meets code someone else compiled, which no test did.
//
// Note what this cannot catch, and why checkPacked exists beside it: this
// compares cg12's layout against the C type checker's, and for a packed struct
// the type checker's is wrong in exactly the same way cg12's is. The two agree,
// and both are wrong. A consistency check has nothing to say when its two
// witnesses share the mistake.
func (g *gen) checkAggLayout(t cc.Type, agg *ir.AggType) {
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
func (g *gen) fieldOf(t cc.Type) ir.Field {
	if isAggType(t) {
		return ir.Field{Type: g.aggOf(t)}
	}
	if at, ok := t.(*cc.ArrayType); ok {
		elem := at.Elem()
		if isAggType(elem) {
			return ir.Field{Type: g.aggOf(elem), Count: int(at.Len())}
		}
		return ir.Field{Sub: subOfType(elem), Count: int(at.Len())}
	}
	return ir.Field{Sub: subOfType(t)}
}

// aggName returns a readable name for an aggregate type (for the printed IL).
func aggName(t cc.Type) string {
	if st, ok := t.(*cc.StructType); ok {
		if tok := st.Tag(); tok.SrcStr() != "" {
			return "struct." + tok.SrcStr()
		}
	}
	if ut, ok := t.(*cc.UnionType); ok {
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

// checkPacked refuses a struct that carries __attribute__((packed)), whose
// layout cg12 would get wrong.
//
// modernc.org/cc/v4 (v4.29.1, the latest) records the attribute but does not
// apply it: for `struct { char c; int i; }` it reports the fields at 0 and 4 and
// the whole at 8, where C says 0 and 1 and 5. cg12 asks it for every offset, so
// it reads and writes the wrong bytes -- silently, because cg12's own layout of
// the same struct agrees with the wrong answer, which is why checkAggLayout
// cannot see it either.
//
// So the attribute itself is the only evidence, and it is only half there:
//
//	struct S { ... } __attribute__((packed));    // recorded: refused here
//	struct __attribute__((packed)) S { ... };    // discarded in the parser: invisible
//
// The second spelling is not detectable by any means this parser offers -- the
// attribute reaches neither the type, nor DeclarationSpecifiers, nor either of
// StructOrUnionSpecifier's two attribute lists. A program using it still
// miscompiles silently. This catches what can be caught and does not pretend to
// be a guarantee; the real fix is upstream.
func (g *gen) checkPacked(t cc.Type) {
	a := t.Attributes()
	if a == nil || !a.IsAttrSet("packed") {
		return
	}
	g.fail("cc: %s is __attribute__((packed)), which cg12 cannot lay out: "+
		"the C type checker reports the unpacked field offsets, so every access "+
		"would read the wrong bytes", aggName(t))
}
