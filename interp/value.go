// Package interp is a tree-walking interpreter for the cg12 SSA IR: it executes
// an *ir.Module directly, without a backend, as a target-independent oracle for
// differential testing (interp vs the arm64/amd64/wasm backends), for validating
// optimizer passes (run before and after a pass; results must agree), and for
// running a frontend's IR without a toolchain.
//
// It executes the portable, pre-lowering IR the backends consume. Its arithmetic
// is a transliteration of opt/fold.go (the semantics the backends are already
// continuously checked against); its floating-point and conversion behaviour
// mirrors the arm64 instructions those ops select (arm64/select.go). Constructs
// that have no target-independent meaning — inline asm, register variables,
// thread-locals, quads, and anything a backend produced during lowering — are
// refused at load time rather than approximated.
package interp

import (
	"math"

	"github.com/evanphx/cg12/ir"
)

// Value is a runtime value: raw bits plus the class that interprets them. A
// single bits payload (rather than separate int/float fields) makes OCast a
// retag and makes memory a uniform bits<->bytes copy.
//
// Canonical form, established at every write by normalize:
//   - ClsW: Bits holds the sign-extended-into-64 value (uint64(int64(int32(x)))),
//     matching opt/fold.go's truncCls convention, so a later signed compare of a
//     negative word is correct.
//   - ClsS: the float32 bits sit in the low 32, the high 32 are zero.
//   - ClsL/ClsD: the full 64 bits.
//
// ClsP is never stored: pointers are ClsL (the interpreter is a 64-bit target,
// the resolution arm64 makes). ClsQ is refused before any Value is built.
type Value struct {
	Cls  ir.Cls
	Bits uint64
}

// normCls collapses the abstract pointer class to the concrete 64-bit class the
// interpreter uses for it; every other class is unchanged.
func normCls(c ir.Cls) ir.Cls {
	if c == ir.ClsP {
		return ir.ClsL
	}
	return c
}

// intVal builds a canonical integer Value of class cls (W or L) from a raw int64.
func intVal(cls ir.Cls, v int64) Value {
	cls = normCls(cls)
	if cls == ir.ClsW {
		v = int64(int32(v)) // sign-extend the word into 64 bits
	}
	return Value{Cls: cls, Bits: uint64(v)}
}

// floatVal builds a Value of float class cls (S or D) from a float64; for ClsS
// the value is rounded through float32.
func floatVal(cls ir.Cls, v float64) Value {
	if cls == ir.ClsS {
		return Value{Cls: ir.ClsS, Bits: uint64(math.Float32bits(float32(v)))}
	}
	return Value{Cls: ir.ClsD, Bits: math.Float64bits(v)}
}

// ptrVal builds a pointer Value (a ClsL integer holding an address).
func ptrVal(addr uint64) Value { return Value{Cls: ir.ClsL, Bits: addr} }

// W / L / S / D construct scalar values from Go types (used by tests and drivers).
func W(x int32) Value    { return Value{Cls: ir.ClsW, Bits: uint64(int64(x))} }
func L(x int64) Value    { return Value{Cls: ir.ClsL, Bits: uint64(x)} }
func S(x float32) Value  { return Value{Cls: ir.ClsS, Bits: uint64(math.Float32bits(x))} }
func D(x float64) Value  { return Value{Cls: ir.ClsD, Bits: math.Float64bits(x)} }
func Ptr(x uint64) Value { return Value{Cls: ir.ClsL, Bits: x} }

// i64 reads the value as a signed integer at its class width. Because ClsW is
// stored sign-extended, int64(Bits) already yields the signed word.
func (v Value) i64() int64 { return int64(v.Bits) }

// u64 reads the value as an unsigned integer at its class width.
func (v Value) u64() uint64 {
	if v.Cls == ir.ClsW {
		return uint64(uint32(v.Bits))
	}
	return v.Bits
}

// f32 / f64 read the value as a float; f64 widens a single.
func (v Value) f32() float32 { return math.Float32frombits(uint32(v.Bits)) }
func (v Value) f64() float64 {
	if v.Cls == ir.ClsS {
		return float64(v.f32())
	}
	return math.Float64frombits(v.Bits)
}

// I64 / U64 / F32 / F64 read the value for callers outside the package (tests,
// harnesses).
func (v Value) I64() int64   { return v.i64() }
func (v Value) U64() uint64  { return v.u64() }
func (v Value) F32() float32 { return v.f32() }
func (v Value) F64() float64 { return v.f64() }

// asClass reinterprets/normalizes a value into class cls without changing its
// meaning — used to bind a value to a temp/param whose class differs only in
// pointer-vs-long or word canonicalization. It never converts int<->float; the
// conversion ops (OStosi, OSltof, ...) do that explicitly.
func (v Value) asClass(cls ir.Cls) Value {
	cls = normCls(cls)
	if cls == v.Cls {
		return v
	}
	switch {
	case cls.IsInt() && v.Cls.IsInt():
		return intVal(cls, v.i64())
	case cls.IsFloat() && v.Cls.IsFloat():
		// S<->D here would silently reround; the IR uses OExts/OTruncd for that,
		// so an implicit float reclass is a bug — keep the bits, retag only when
		// the widths already agree.
		if cls.Size() == v.Cls.Size() {
			return Value{Cls: cls, Bits: v.Bits}
		}
	}
	// Equal-size int<->float retag (what OCopy across banks would be) keeps bits.
	return Value{Cls: cls, Bits: v.Bits}
}
