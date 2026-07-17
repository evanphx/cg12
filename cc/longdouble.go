package cc

import (
	"fmt"
	"math/big"

	"github.com/evanphx/cg12/ir"
	"modernc.org/cc/v4"
)

// long double is a 128-bit IEEE quad on AArch64. There is no hardware quad, so
// every operation lowers to a libgcc soft-float call (__addtf3, __lttf2,
// __extenddftf2, ...). A quad value is modelled like an aggregate: it lives in a
// 16-byte slot and its "value" is that slot's address, which the AAPCS passes
// and returns in a single SIMD register (the ir.ClsQ class and the quad
// aggregate type).
//
// That is one machine's answer, and all of this assumes it. x86-64's long double
// is an 80-bit x87 extended value in a 16-byte slot: different bits, different
// operations (hardware x87, not soft-float), and different names for the libgcc
// helpers if it had any. Compiling this C for amd64 emitted calls to __addtf3
// and __eqtf2 -- the quad helpers -- which x86-64 does not provide, so the image
// failed to load with an undefined symbol. Worse would be finding them: they
// implement IEEE binary128, so the program would compute quietly in the wrong
// format and disagree with libc the moment a long double crossed to printf.
//
// So amd64 is refused rather than mis-served. See checkLongDouble.

// isLongDouble reports whether t is the long double type.
func isLongDouble(t cc.Type) bool { return t.Kind() == cc.LongDouble }

// checkLongDouble refuses a long double on a target whose long double is not the
// 128-bit quad this file implements.
func (g *gen) checkLongDouble() {
	if g.target != TargetARM64 {
		g.fail("cc: long double is not supported for %s: cg12 implements AArch64's "+
			"128-bit IEEE quad (soft-float via __addtf3 and friends), and %s's long "+
			"double is 80-bit x87 extended -- a different format, which those helpers "+
			"do not compute", g.target, g.target)
	}
}

// quadAgg returns the canonical aggregate type that models a 128-bit quad, so a
// long double travels through the SIMD-register path of the aggregate ABI.
//
// Every long double reaches this: the type is how one is stored, passed and
// returned, so refusing here catches them all.
func (g *gen) quadAgg() *ir.AggType {
	g.checkLongDouble()
	if g.quad == nil {
		g.quad = &ir.AggType{Name: "longdouble", Fields: []ir.Field{{Sub: ir.SubQ}}}
		g.mod.AddType(g.quad)
	}
	return g.quad
}

// aggTypeOf returns the aggregate type used to pass t by value: the quad type for
// a long double, otherwise the struct/union's own type.
func (g *gen) aggTypeOf(t cc.Type) *ir.AggType {
	if isLongDouble(t) {
		return g.quadAgg()
	}
	if cc.IsComplexType(t) {
		return g.complexAgg(t)
	}
	if isInt128(t) {
		return g.int128Agg()
	}
	return g.aggOf(t)
}

// quadArg is one argument to a soft-float helper: quad arguments pass by value as
// a quad aggregate (a pointer the ABI loads into a SIMD register); scalar
// arguments pass their value directly.
type quadArg struct {
	ref  ir.Ref
	quad bool
}

func qa(ref ir.Ref) quadArg { return quadArg{ref, true} }
func sa(ref ir.Ref) quadArg { return quadArg{ref, false} }

// softcall emits a call to a libgcc soft-float helper. A quad result is returned
// into a fresh 16-byte slot whose address is the result; otherwise the scalar
// result of class retCls is returned.
func (g *gen) softcall(fn string, retQuad bool, retCls ir.Cls, args ...quadArg) ir.Ref {
	callee := g.fn.Sym(fn, 0)
	refs := make([]ir.Ref, len(args))
	var aggArgs []*ir.AggType
	for i, a := range args {
		refs[i] = a.ref
		if a.quad {
			for len(aggArgs) <= i {
				aggArgs = append(aggArgs, nil)
			}
			aggArgs[i] = g.quadAgg()
		}
	}
	if retQuad {
		r := g.cur.Call(ir.ClsL, callee, refs...)
		call := g.lastInstr()
		call.RetAgg = g.quadAgg()
		call.AggArgs = aggArgs
		return r
	}
	r := g.cur.Call(retCls, callee, refs...)
	g.lastInstr().AggArgs = aggArgs
	return r
}

// quadArith emits a op b for +, -, *, / on quad operands (addresses), returning
// the result's address.
func (g *gen) quadArith(op string, a, b ir.Ref) ir.Ref {
	fn := map[string]string{"+": "__addtf3", "-": "__subtf3", "*": "__multf3", "/": "__divtf3"}[op]
	if fn == "" {
		return g.fail("cc: %q not defined for long double", op)
	}
	return g.softcall(fn, true, 0, qa(a), qa(b))
}

// quadCompare emits a relational/equality comparison of quad operands, producing
// a 0/1 int. The soft-float compare helpers return an int ordered like memcmp,
// which is then tested against zero per the operator.
func (g *gen) quadCompare(op string, a, b ir.Ref) ir.Ref {
	var fn string
	var pred ir.Cmp
	switch op {
	case "==":
		fn, pred = "__eqtf2", ir.CmpEq
	case "!=":
		fn, pred = "__netf2", ir.CmpNe
	case "<":
		fn, pred = "__lttf2", ir.CmpSlt
	case "<=":
		fn, pred = "__letf2", ir.CmpSle
	case ">":
		fn, pred = "__gttf2", ir.CmpSgt
	default: // ">="
		fn, pred = "__getf2", ir.CmpSge
	}
	c := g.softcall(fn, false, ir.ClsW, qa(a), qa(b))
	return g.cur.Cmp(pred, ir.ClsW, c, g.fn.Word(0))
}

// toQuad converts a scalar value to a quad, returning the result's address.
func (g *gen) toQuad(v ir.Ref, from cc.Type) ir.Ref {
	switch {
	case from.Kind() == cc.Double:
		return g.softcall("__extenddftf2", true, 0, sa(v))
	case from.Kind() == cc.Float:
		return g.softcall("__extendsftf2", true, 0, sa(v))
	case wide(clsOf(from)): // 64-bit integer
		if signed(from) {
			return g.softcall("__floatditf", true, 0, sa(v))
		}
		return g.softcall("__floatunditf", true, 0, sa(v))
	default: // 32-bit integer
		if signed(from) {
			return g.softcall("__floatsitf", true, 0, sa(v))
		}
		return g.softcall("__floatunsitf", true, 0, sa(v))
	}
}

// fromQuad converts a quad (address a) to a scalar of type to.
func (g *gen) fromQuad(a ir.Ref, to cc.Type) ir.Ref {
	switch {
	case to.Kind() == cc.Double:
		return g.softcall("__trunctfdf2", false, ir.ClsD, qa(a))
	case to.Kind() == cc.Float:
		return g.softcall("__trunctfsf2", false, ir.ClsS, qa(a))
	case to.Kind() == cc.Bool:
		c := g.softcall("__netf2", false, ir.ClsW, qa(a), qa(g.quadZero()))
		return g.cur.Cmp(ir.CmpNe, ir.ClsW, c, g.fn.Word(0))
	case wide(clsOf(to)): // 64-bit integer
		if signed(to) {
			return g.softcall("__fixtfdi", false, ir.ClsL, qa(a))
		}
		return g.softcall("__fixunstfdi", false, ir.ClsL, qa(a))
	default: // 32-bit integer
		if signed(to) {
			return g.softcall("__fixtfsi", false, ir.ClsW, qa(a))
		}
		return g.softcall("__fixunstfsi", false, ir.ClsW, qa(a))
	}
}

// quadZero returns the address of a constant +0.0 quad.
func (g *gen) quadZero() ir.Ref { return g.quadConst(new(big.Float)) }

// quadConst interns a quad constant as read-only data and returns its address.
func (g *gen) quadConst(f *big.Float) ir.Ref {
	bytes := quadBytes(f)
	key := string(bytes)
	name, ok := g.ldconsts[key]
	if !ok {
		name = fmt.Sprintf("ld%d", len(g.ldconsts))
		g.ldconsts[key] = name
		g.mod.Data = append(g.mod.Data, &ir.Data{
			Name:  name,
			Align: 16,
			Items: []ir.DataItem{{Str: key}},
		})
	}
	return g.fn.Sym(name, 0)
}

// quadBytes encodes a big.Float as the 16 little-endian bytes of an IEEE
// binary128 (quad): 1 sign bit, a 15-bit biased exponent, and a 112-bit
// fraction with an implicit leading one. Values outside the normal exponent
// range are flushed to zero or saturated to infinity — enough for source
// constants.
func quadBytes(f *big.Float) []byte {
	out := make([]byte, 16)
	neg := f.Signbit()
	if neg {
		out[15] |= 0x80
	}
	if f.Sign() == 0 {
		return out // signed zero
	}

	m := new(big.Float).SetPrec(113).Abs(f)
	var mant big.Float
	exp := m.MantExp(&mant) // m = mant * 2^exp, mant in [0.5, 1)
	e := int64(exp) - 1 + 16383
	switch {
	case e <= 0:
		return out // underflow to zero (subnormals not modelled)
	case e >= 0x7fff:
		// Infinity: exponent all ones (bits 112..126), fraction zero.
		out[14] |= 0xff
		out[15] |= 0x7f
		return out
	}

	// The 113-bit integer significand; bit 112 is the implicit one. The low 112
	// bits are the fraction, occupying bytes 0..13.
	mant.SetMantExp(&mant, 113)
	sig, _ := mant.Int(nil)
	frac := new(big.Int).And(sig, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1)))
	fb := frac.Bytes() // big-endian, up to 14 bytes
	for i, b := range fb {
		out[len(fb)-1-i] = b // little-endian
	}
	// The 15-bit exponent is bits 112..126: its low 8 bits fill byte 14, its high
	// 7 bits share byte 15 with the sign (bit 127).
	out[14] |= byte(e & 0xff)
	out[15] |= byte((e >> 8) & 0x7f)
	return out
}
