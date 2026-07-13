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
// union, registering it with the module. The layout follows the natural C rules,
// which cg12's own layout reproduces, so field offsets agree with the type
// checker's.
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
	return agg
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
