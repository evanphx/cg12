package x64

// Bit-manipulation encoders: the bit scan that count-leading-zeros is built from,
// and the one-operand widening multiplies that produce a full double-width
// product.
//
// Everything here is baseline x86-64, deliberately. The tempting alternatives --
// LZCNT for the leading-zero count, TZCNT for the trailing one, POPCNT for a
// population count -- are all CPU-feature-gated, and cg12 has no way to say which
// features a target has (amd64.Options carries none, and there is no -march
// anywhere in the tree). For LZCNT and TZCNT that gap is not a missed
// optimization but a correctness trap: their encodings are BSR and BSF with an F3
// prefix, and a CPU without the feature *ignores* the prefix and executes the bit
// scan instead. The program does not fault, it silently computes a different
// number. So they stay out until a feature model exists to gate them on.

// Bsr encodes BSR reg, r/m (0F BD /r): dst receives the index of the most
// significant set bit of src, and ZF is set when src is zero.
//
// When src is zero the destination is architecturally *undefined* -- not
// unchanged, which is merely what current parts happen to do -- so a caller
// computing a leading-zero count must take the answer for zero from ZF and never
// from dst. See mcXasm.clzGP for the sequence that does.
func Bsr(w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x0f, 0xbd}, dst, src, false)
}

// MulWide encodes MUL r/m (F7 /4): the unsigned widening multiply, RDX:RAX =
// RAX * src (or EDX:EAX = EAX * src at 32 bits). ImulWide is the signed form,
// IMUL r/m (F7 /5).
//
// These are the one-operand accumulator forms, and they are the only way to get
// at the high half of the product. Imul/ImulMem/ImulImm in x64.go are the
// two-operand IMUL, which keeps just the low half and so cannot serve OUMulh or
// OSMulh -- hence the separate names rather than an overload of Imul. Div and
// Idiv are the same shape (F7 /6 and /7) reading RDX:RAX instead of writing it.
func MulWide(w bool, src Reg) []byte { return op_rr(nil, nil, w, []byte{0xf7}, 4, src, false) }

// ImulWide encodes IMUL r/m (F7 /5): the signed widening multiply into RDX:RAX.
func ImulWide(w bool, src Reg) []byte { return op_rr(nil, nil, w, []byte{0xf7}, 5, src, false) }
