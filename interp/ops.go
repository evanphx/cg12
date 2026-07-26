package interp

import (
	"math"
	"math/bits"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// execInstr evaluates one non-terminator instruction and binds its result.
func (mc *Machine) execInstr(fr *frame, in *ir.Instr) error {
	switch {
	case in.Op == ir.OCall:
		return mc.execCall(fr, in)
	case in.Op == ir.OSafepoint, in.Op == ir.ONop:
		return nil
	case in.Op.IsLoad():
		return mc.execLoad(fr, in)
	case in.Op.IsStore():
		return mc.execStore(fr, in)
	case in.Op.IsAlloc() || in.Op == ir.OAllocN:
		return mc.execAlloc(fr, in)
	case in.Op == ir.OBlit:
		return mc.execBlit(fr, in)
	case in.Op == ir.OIntrinsic:
		return mc.execIntrinsic(fr, in)
	case in.Op == ir.OVaStart:
		return mc.execVaStart(fr, in)
	case in.Op == ir.OVaArg:
		return mc.execVaArg(fr, in)
	case in.Op == ir.OBlockAddr:
		addr, ok := mc.blockAddr[in.Blk]
		if !ok {
			return mc.trapf("block address of a block whose address was not taken at load")
		}
		if !in.To.IsNone() {
			fr.vals[in.To.ID] = ptrVal(addr)
		}
		return nil
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

// execIntrinsic runs a named intrinsic. The names it knows are the low-level
// primitives the interpreter can model; an unknown one is a trap rather than a
// silent no-op.
func (mc *Machine) execIntrinsic(fr *frame, in *ir.Instr) error {
	switch in.Intrin.Name {
	case "stacksave":
		fr.vals[in.To.ID] = ptrVal(mc.sp)
		return nil
	case "stackrestore":
		v, err := mc.evalRef(fr, in.Arg(0))
		if err != nil {
			return err
		}
		mc.sp = v.u64()
		return nil
	case "constant_p":
		// An unresolved __builtin_constant_p marker (opt.ResolveConstantP settles it
		// in a normal build): reporting "not constant" is always sound.
		if !in.To.IsNone() {
			fr.vals[in.To.ID] = W(0)
		}
		return nil
	}
	if strings.HasPrefix(in.Intrin.Name, "atomic.") {
		args := make([]Value, len(in.Args))
		for i := range in.Args {
			v, err := mc.evalRef(fr, in.Arg(i))
			if err != nil {
				return err
			}
			args[i] = v
		}
		res, hasResult, err := mc.atomicOp(in.Intrin.Name, args)
		if err != nil {
			return err
		}
		if hasResult && !in.To.IsNone() {
			fr.vals[in.To.ID] = res
		}
		return nil
	}
	return mc.trapf("interp: unknown intrinsic %q", in.Intrin.Name)
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

	case ir.OSel:
		cond, err := arg(0)
		if err != nil {
			return Value{}, err
		}
		pick := 2
		if cond.u64() != 0 {
			pick = 1
		}
		v, err := mc.evalRef(fr, in.Arg(pick))
		if err != nil {
			return Value{}, err
		}
		return v.asClass(in.Cls), nil

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

// --- memory operations -------------------------------------------------------

func (mc *Machine) memLoad(addr uint64, size int) (uint64, error) {
	v, ok := mc.mem.load(addr, size)
	if !ok {
		return 0, mc.trapf("load %d bytes at %#x: unmapped", size, addr)
	}
	return v, nil
}

// execLoad reads memory at Args[0], extending per the op's width and signedness
// into the result class.
func (mc *Machine) execLoad(fr *frame, in *ir.Instr) error {
	av, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return err
	}
	addr := av.u64()
	var out Value
	switch in.Op {
	case ir.OLoadsb:
		v, err := mc.memLoad(addr, 1)
		if err != nil {
			return err
		}
		out = intVal(in.Cls, int64(int8(v)))
	case ir.OLoadub:
		v, err := mc.memLoad(addr, 1)
		if err != nil {
			return err
		}
		out = intVal(in.Cls, int64(uint8(v)))
	case ir.OLoadsh:
		v, err := mc.memLoad(addr, 2)
		if err != nil {
			return err
		}
		out = intVal(in.Cls, int64(int16(v)))
	case ir.OLoaduh:
		v, err := mc.memLoad(addr, 2)
		if err != nil {
			return err
		}
		out = intVal(in.Cls, int64(uint16(v)))
	case ir.OLoadsw:
		v, err := mc.memLoad(addr, 4)
		if err != nil {
			return err
		}
		out = intVal(in.Cls, int64(int32(v)))
	case ir.OLoaduw:
		v, err := mc.memLoad(addr, 4)
		if err != nil {
			return err
		}
		out = intVal(in.Cls, int64(uint32(v)))
	case ir.OLoadl:
		v, err := mc.memLoad(addr, 8)
		if err != nil {
			return err
		}
		out = intVal(in.Cls, int64(v))
	case ir.OLoads:
		v, err := mc.memLoad(addr, 4)
		if err != nil {
			return err
		}
		out = Value{Cls: ir.ClsS, Bits: v & 0xffffffff}
	case ir.OLoadd:
		v, err := mc.memLoad(addr, 8)
		if err != nil {
			return err
		}
		out = Value{Cls: ir.ClsD, Bits: v}
	default:
		return mc.trapf("unhandled load %s", in.Op)
	}
	if !in.To.IsNone() {
		fr.vals[in.To.ID] = out.asClass(in.Cls)
	}
	return nil
}

// execStore writes the value (Args[0]) to the address (Args[1]) at the op's width.
func (mc *Machine) execStore(fr *frame, in *ir.Instr) error {
	vv, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return err
	}
	av, err := mc.evalRef(fr, in.Arg(1))
	if err != nil {
		return err
	}
	size := storeSize(in.Op)
	if size == 0 {
		return mc.trapf("unhandled store %s", in.Op)
	}
	if !mc.mem.store(av.u64(), size, vv.Bits) {
		return mc.trapf("store %d bytes at %#x: unmapped", size, av.u64())
	}
	return nil
}

func storeSize(op ir.Op) int {
	switch op {
	case ir.OStoreb:
		return 1
	case ir.OStoreh:
		return 2
	case ir.OStorew, ir.OStores:
		return 4
	case ir.OStorel, ir.OStored:
		return 8
	}
	return 0
}

// execAlloc reserves stack space and yields its address.
func (mc *Machine) execAlloc(fr *frame, in *ir.Instr) error {
	sv, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return err
	}
	size := sv.u64()
	var align uint64
	switch in.Op {
	case ir.OAlloc4:
		align = 4
	case ir.OAlloc8:
		align = 8
	default: // OAlloc16, OAllocN
		align = 16
		if in.Op == ir.OAllocN && size%16 != 0 {
			return mc.trapf("allocn size %d is not a multiple of 16", size)
		}
	}
	addr := mc.stackAlloc(size, align)
	if !in.To.IsNone() {
		fr.vals[in.To.ID] = ptrVal(addr)
	}
	return nil
}

// stackAlloc reserves size bytes of stack (descending, aligned) and returns the
// address. It backs both alloc ops and by-value aggregate argument/return copies.
func (mc *Machine) stackAlloc(size, align uint64) uint64 {
	if align == 0 {
		align = 1
	}
	mc.sp = (mc.sp - size) &^ (align - 1)
	mc.mem.mapRange(mc.sp, maxU(size, 1))
	return mc.sp
}

// copyMem copies n bytes from src to dst, trapping if either range is unmapped.
func (mc *Machine) copyMem(dst, src uint64, n int) error {
	buf, ok := mc.mem.readBytes(src, n)
	if !ok {
		return mc.trapf("aggregate copy source [%#x,+%d) unmapped", src, n)
	}
	if !mc.mem.writeBytes(dst, buf) {
		return mc.trapf("aggregate copy dest [%#x,+%d) unmapped", dst, n)
	}
	return nil
}

// execBlit copies Aux bytes from Args[1] to Args[0], overlap-safe.
func (mc *Machine) execBlit(fr *frame, in *ir.Instr) error {
	dv, err := mc.evalRef(fr, in.Arg(0))
	if err != nil {
		return err
	}
	sv, err := mc.evalRef(fr, in.Arg(1))
	if err != nil {
		return err
	}
	n := int(in.Aux)
	buf, ok := mc.mem.readBytes(sv.u64(), n)
	if !ok {
		return mc.trapf("blit source [%#x,+%d) unmapped", sv.u64(), n)
	}
	if !mc.mem.writeBytes(dv.u64(), buf) {
		return mc.trapf("blit dest [%#x,+%d) unmapped", dv.u64(), n)
	}
	return nil
}
