package main

import "unsafe"

// A complex64 is two float32 halves packed into one 64-bit value, so reading a
// half out of it is a bitwise reinterpretation between a general-purpose and a
// floating-point register. cg12 used a plain register copy, which re-types only
// within one register file, so real() and imag() both returned whatever the
// integer register aliased -- the same wrong value for both halves -- and every
// complex64 arithmetic operation was wrong with them. Separately, gen.convert
// had no complex case at all, so complex128(c64) reinterpreted the packed pair
// as an address and the program took SIGSEGV at 0x4090000040600000, which is
// the packed bit pattern itself.
//
// Both were found by routing a complex64 operand of println to
// runtime.printcomplex64, which converts to complex128 internally.
func main() {
	var narrow complex64 = complex(3.5, 4.5)

	// The stored representation was never in doubt: the two halves are packed
	// little-endian, low word first. This anchors the rest of the checks to the
	// same bits the host toolchain lays down.
	bits := *(*uint64)(unsafe.Pointer(&narrow))
	if bits != 0x40900000_40600000 {
		panic("complex64 is not stored as two packed float32 halves")
	}

	if real(narrow) != 3.5 || imag(narrow) != 4.5 {
		panic("real/imag of a complex64 did not read its halves")
	}
	if real(narrow) == imag(narrow) {
		panic("real and imag of a complex64 returned the same half")
	}

	sum := narrow + complex64(complex(1, 1))
	if real(sum) != 4.5 || imag(sum) != 5.5 {
		panic("complex64 addition used the wrong halves")
	}

	difference := sum - narrow
	if real(difference) != 1 || imag(difference) != 1 {
		panic("complex64 subtraction used the wrong halves")
	}

	product := narrow * narrow
	if real(product) != -8 || imag(product) != 31.5 {
		panic("complex64 multiplication used the wrong halves")
	}

	if narrow != complex64(complex(3.5, 4.5)) || narrow == sum {
		panic("complex64 comparison used the wrong halves")
	}

	// The conversion that segfaulted, in both directions.
	widened := complex128(narrow)
	if real(widened) != 3.5 || imag(widened) != 4.5 {
		panic("complex64 to complex128 lost its halves")
	}
	narrowed := complex64(complex128(complex(6.5, -7.25)))
	if real(narrowed) != 6.5 || imag(narrowed) != -7.25 {
		panic("complex128 to complex64 lost its halves")
	}

	// complex128 keeps working: it is addressed rather than packed, so it
	// travels a different path and is the control for the two above.
	var wide complex128 = complex(-1.25, 8)
	if real(wide) != -1.25 || imag(wide) != 8 {
		panic("real/imag of a complex128 did not read its halves")
	}
	if real(wide*wide) != -62.4375 || imag(wide*wide) != -20 {
		panic("complex128 multiplication used the wrong halves")
	}

	// The halves survive a call boundary in both representations.
	if collapse64(narrow) != 8 || collapse128(wide) != 6.75 {
		panic("a complex argument lost its halves across a call")
	}

	println("complex64-parts ok")
}

//go:noinline
func collapse64(value complex64) float32 {
	return real(value) + imag(value)
}

//go:noinline
func collapse128(value complex128) float64 {
	return real(value) + imag(value)
}
