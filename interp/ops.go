package interp

import (
	"math"
	"math/bits"

	"github.com/evanphx/cg12/ir"
)

// execInstr evaluates one non-terminator instruction and binds its result.
func (mc *Machine) execInstr(fr *frame, in *ir.Instr) error {
	switch {
	case in.Op == ir.OCall:
		return mc.execCall(fr, in)
	case in.Op == ir.OSafepoint, in.Op == ir.ONop:
		return nil
	case in.Op.IsLoad(), in.Op.IsStore(), in.Op.IsAlloc(),
		in.Op == ir.OBlit, in.Op == ir.OStackSave, in.Op == ir.OStackRestore:
		return mc.trapf("memory op %s not yet supported (Phase B)", in.Op)
	case in.Op == ir.OVaStart, in.Op == ir.OVaArg:
		return mc.trapf("variadic op %s not yet supported (Phase C)", in.Op)
	case in.Op == ir.OBlockAddr:
		return mc.trapf("block address not yet supported (Phase C)")
	}

	v, err := mc.evalValueOp(fr, in)
	if err != nil {
		return err
	}
	if !in.To.IsNone() {
		fr.vals[in.To.ID] = v.asClass(in.Cls)
	}
	return nil
}

// evalValueOp evaluates a pure (result-producing, side-effect-free) op.
func (mc *Machine) evalValueOp(fr *frame, in *ir.Instr) (Value, error) {
	arg := func(i int) (Value, error) { return mc.evalRef(fr, in.Arg(i)) }

	switch in.Op {
	// Integer/float binary arithmetic and bitwise/shift.
	case ir.OAdd, ir.OSub, ir.OMul, ir.ODiv, ir.OUDiv, ir.ORem, ir.OURem,
		ir.OAnd, ir.OOr, ir.OXor, ir.OShl, ir.OShr, ir.OSar:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		b, err := arg(1)
		if err != nil {
			return Value{}, err
		}
		if in.Cls.IsFloat() {
			return mc.floatBinop(in.Op, in.Cls, a, b)
		}
		return mc.intBinop(in.Op, in.Cls, a, b)

	case ir.ONeg:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		if in.Cls.IsFloat() {
			return negFloat(in.Cls, a), nil
		}
		return intVal(in.Cls, truncInt(in.Cls, -a.i64())), nil

	case ir.OClz:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		if in.Cls == ir.ClsW {
			return intVal(ir.ClsW, int64(bits.LeadingZeros32(uint32(a.u64())))), nil
		}
		return intVal(ir.ClsL, int64(bits.LeadingZeros64(a.u64()))), nil

	case ir.OCmp:
		return mc.evalCmp(fr, in)

	// Integer sub-word extends (transliterate opt/fold.go:107-120).
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return intVal(in.Cls, extendInt(in.Op, a.i64())), nil

	// Float width conversions.
	case ir.OExts: // single -> double
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return floatVal(ir.ClsD, a.f64()), nil
	case ir.OTruncd: // double -> single
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return floatVal(ir.ClsS, a.f64()), nil

	// Float <-> int conversions (saturating float->int, matching fcvtz*).
	case ir.OStosi:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return intVal(in.Cls, f2iSigned(a.f64(), intBits(in.Cls))), nil
	case ir.OStoui:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return intVal(in.Cls, int64(f2iUnsigned(a.f64(), intBits(in.Cls)))), nil
	case ir.OSltof:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return floatVal(in.Cls, float64(asSigned(fr.fn.ClassOf(in.Arg(0)), a.i64()))), nil
	case ir.OUltof:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return floatVal(in.Cls, uint64ToFloat(asUnsigned(fr.fn.ClassOf(in.Arg(0)), a.i64()))), nil

	// Bit reinterpretation and plain copy.
	case ir.OCast:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return castValue(in.Cls, a), nil
	case ir.OCopy:
		a, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		return a.asClass(in.Cls), nil
	}

	return Value{}, mc.trapf("unhandled op %s", in.Op)
}

// intBinop mirrors opt/fold.go evalBinop; div/rem by zero traps (or yields zero
// under WithDivZeroYieldsZero).
func (mc *Machine) intBinop(op ir.Op, cls ir.Cls, av, bv Value) (Value, error) {
	a, b := av.i64(), bv.i64()
	sa, sb := asSigned(cls, a), asSigned(cls, b)
	ua, ub := asUnsigned(cls, a), asUnsigned(cls, b)
	shift := uint(b) & shiftMask(cls)

	divZero := func() (Value, error) {
		if mc.divZeroZero {
			return intVal(cls, 0), nil
		}
		return Value{}, mc.trapf("integer divide by zero")
	}

	switch op {
	case ir.OAdd:
		return intVal(cls, truncInt(cls, a+b)), nil
	case ir.OSub:
		return intVal(cls, truncInt(cls, a-b)), nil
	case ir.OMul:
		return intVal(cls, truncInt(cls, a*b)), nil
	case ir.OAnd:
		return intVal(cls, truncInt(cls, a&b)), nil
	case ir.OOr:
		return intVal(cls, truncInt(cls, a|b)), nil
	case ir.OXor:
		return intVal(cls, truncInt(cls, a^b)), nil
	case ir.OShl:
		return intVal(cls, truncInt(cls, a<<shift)), nil
	case ir.OShr:
		return intVal(cls, truncInt(cls, int64(ua>>shift))), nil
	case ir.OSar:
		return intVal(cls, truncInt(cls, sa>>shift)), nil
	case ir.ODiv:
		if sb == 0 {
			return divZero()
		}
		if sa == math.MinInt64 && sb == -1 {
			return intVal(cls, sa), nil
		}
		return intVal(cls, truncInt(cls, sa/sb)), nil
	case ir.ORem:
		if sb == 0 {
			return divZero()
		}
		if sa == math.MinInt64 && sb == -1 {
			return intVal(cls, 0), nil
		}
		return intVal(cls, truncInt(cls, sa%sb)), nil
	case ir.OUDiv:
		if ub == 0 {
			return divZero()
		}
		return intVal(cls, truncInt(cls, int64(ua/ub))), nil
	case ir.OURem:
		if ub == 0 {
			return divZero()
		}
		return intVal(cls, truncInt(cls, int64(ua%ub))), nil
	}
	return Value{}, mc.trapf("unhandled integer op %s", op)
}

// floatBinop evaluates a floating-point binary op at the operation's width,
// rounding each single-precision result through float32.
func (mc *Machine) floatBinop(op ir.Op, cls ir.Cls, av, bv Value) (Value, error) {
	if cls == ir.ClsS {
		x, y := av.f32(), bv.f32()
		var r float32
		switch op {
		case ir.OAdd:
			r = x + y
		case ir.OSub:
			r = x - y
		case ir.OMul:
			r = x * y
		case ir.ODiv:
			r = x / y
		default:
			return Value{}, mc.trapf("op %s is not defined on floats", op)
		}
		return Value{Cls: ir.ClsS, Bits: uint64(math.Float32bits(r))}, nil
	}
	x, y := av.f64(), bv.f64()
	var r float64
	switch op {
	case ir.OAdd:
		r = x + y
	case ir.OSub:
		r = x - y
	case ir.OMul:
		r = x * y
	case ir.ODiv:
		r = x / y
	default:
		return Value{}, mc.trapf("op %s is not defined on floats", op)
	}
	return floatVal(ir.ClsD, r), nil
}

// evalCmp evaluates an OCmp, producing 0/1 at the result class.
func (mc *Machine) evalCmp(fr *frame, in *ir.Instr) (Value, error) {
	a, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return Value{}, err
	}
	b, err := mc.evalRef(fr, in.Arg(1))
	if err != nil {
		return Value{}, err
	}
	argCls := fr.fn.ClassOf(in.Arg(0))
	var r bool
	if in.Cmp.IsFloat() {
		r = floatCmp(in.Cmp, a.f64(), b.f64())
	} else {
		r = intCmp(in.Cmp, argCls, a.i64(), b.i64())
	}
	return intVal(in.Cls, b2i(r)), nil
}

// intCmp mirrors opt/fold.go evalCmp.
func intCmp(pred ir.Cmp, cls ir.Cls, a, b int64) bool {
	sa, sb := asSigned(cls, a), asSigned(cls, b)
	ua, ub := asUnsigned(cls, a), asUnsigned(cls, b)
	switch pred {
	case ir.CmpEq:
		return sa == sb
	case ir.CmpNe:
		return sa != sb
	case ir.CmpSlt:
		return sa < sb
	case ir.CmpSle:
		return sa <= sb
	case ir.CmpSgt:
		return sa > sb
	case ir.CmpSge:
		return sa >= sb
	case ir.CmpUlt:
		return ua < ub
	case ir.CmpUle:
		return ua <= ub
	case ir.CmpUgt:
		return ua > ub
	case ir.CmpUge:
		return ua >= ub
	}
	return false
}

// floatCmp evaluates a float predicate with IEEE ordered/unordered semantics.
func floatCmp(pred ir.Cmp, x, y float64) bool {
	switch pred {
	case ir.CmpFeq:
		return x == y // ordered: false if either NaN
	case ir.CmpFne:
		return x != y // unordered: true if either NaN, else real !=
	case ir.CmpFle:
		return x <= y
	case ir.CmpFlt:
		return x < y
	case ir.CmpFge:
		return x >= y
	case ir.CmpFgt:
		return x > y
	case ir.CmpFo:
		return !math.IsNaN(x) && !math.IsNaN(y)
	case ir.CmpFuo:
		return math.IsNaN(x) || math.IsNaN(y)
	}
	return false
}

// --- pure helpers, mirroring opt/fold.go and arm64/select.go -----------------

func shiftMask(cls ir.Cls) uint {
	if cls == ir.ClsW {
		return 31
	}
	return 63
}

func asSigned(cls ir.Cls, v int64) int64 {
	if cls == ir.ClsW {
		return int64(int32(v))
	}
	return v
}

func asUnsigned(cls ir.Cls, v int64) uint64 {
	if cls == ir.ClsW {
		return uint64(uint32(v))
	}
	return uint64(v)
}

func truncInt(cls ir.Cls, v int64) int64 {
	if cls == ir.ClsW {
		return int64(int32(v))
	}
	return v
}

func intBits(cls ir.Cls) int {
	if cls == ir.ClsW {
		return 32
	}
	return 64
}

func b2i(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// extendInt reinterprets the low bits of v per an integer-extend op.
func extendInt(op ir.Op, v int64) int64 {
	switch op {
	case ir.OExtsw:
		return int64(int32(v))
	case ir.OExtuw:
		return int64(uint32(v))
	case ir.OExtsb:
		return int64(int8(v))
	case ir.OExtub:
		return int64(uint8(v))
	case ir.OExtsh:
		return int64(int16(v))
	case ir.OExtuh:
		return int64(uint16(v))
	}
	return v
}

// negFloat flips only the sign bit — the fneg semantics, exact for NaN and ±0.
func negFloat(cls ir.Cls, a Value) Value {
	if cls == ir.ClsS {
		return Value{Cls: ir.ClsS, Bits: uint64(uint32(a.Bits) ^ (1 << 31))}
	}
	return Value{Cls: ir.ClsD, Bits: a.Bits ^ (1 << 63)}
}

// castValue reinterprets the bits of a into class cls (equal-size int<->float).
func castValue(cls ir.Cls, a Value) Value {
	cls = normCls(cls)
	if cls.Size() == 4 {
		low := uint32(a.Bits)
		if cls == ir.ClsW {
			return intVal(ir.ClsW, int64(int32(low)))
		}
		return Value{Cls: ir.ClsS, Bits: uint64(low)}
	}
	return Value{Cls: cls, Bits: a.Bits}
}

// f2iSigned converts a float to a signed integer of the given bit width, rounding
// toward zero and saturating out-of-range (including NaN->0) as AArch64 fcvtzs.
func f2iSigned(x float64, bitWidth int) int64 {
	if math.IsNaN(x) {
		return 0
	}
	t := math.Trunc(x)
	if bitWidth == 32 {
		if t >= 2147483648.0 { // 2^31
			return math.MaxInt32
		}
		if t < -2147483648.0 {
			return math.MinInt32
		}
		return int64(int32(t))
	}
	if t >= 9223372036854775808.0 { // 2^63
		return math.MaxInt64
	}
	if t < -9223372036854775808.0 {
		return math.MinInt64
	}
	return int64(t)
}

// f2iUnsigned is f2iSigned's unsigned counterpart (fcvtzu): negatives and NaN
// saturate to 0, over-range to the max.
func f2iUnsigned(x float64, bitWidth int) uint64 {
	if math.IsNaN(x) {
		return 0
	}
	t := math.Trunc(x)
	if t <= 0 {
		return 0
	}
	if bitWidth == 32 {
		if t >= 4294967296.0 { // 2^32
			return math.MaxUint32
		}
		return uint64(uint32(t))
	}
	if t >= 18446744073709551616.0 { // 2^64
		return math.MaxUint64
	}
	return uint64(t)
}

// uint64ToFloat converts an unsigned integer to float64 with round-to-nearest,
// matching ucvtf.
func uint64ToFloat(u uint64) float64 { return float64(u) }
