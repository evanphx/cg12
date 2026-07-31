package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// The Go specification's table for the two print built-ins says println is
// "like print but prints spaces between arguments and a newline at the end", so
// the separators and the trailing newline are required rather than
// implementation-defined; only the rendering of an individual operand is left
// open. cg12 emitted one runtime print call per operand and a single printnl,
// so println("a", 1, true) wrote "a1true" where every other Go toolchain writes
// "a 1 true".
//
// This reducer pins the whole dispatch, not just the separators, because the
// operand-type table is where the rest of the differences were: a slice printed
// its header address as a decimal integer instead of [len/cap]0xaddr, an
// interface printed one decimal number instead of (0xtype,0xdata), and a
// complex printed a decimal integer. Every expectation below is the byte-exact
// output of the host Go toolchain for the same program.
//
// println writes to standard error, so each case runs with file descriptor 2
// pointed at a temporary file. The redirect is undone before anything is
// asserted, so a failure still reports through the real standard error.
func main() {
	// The reported defect, and the spec rule it violates.
	expect("a 1 true\n", func() { println("a", 1, true) })

	// print adds neither separators nor a newline; only println does.
	expect("a1true", func() { print("a", 1, true) })

	// Degenerate operand counts.
	expect("\n", func() { println() })
	expect("solo\n", func() { println("solo") })
	expect("", func() { print() })
	expect(" 1\n", func() { println("", 1) })

	// Every operand is separated, including two adjacent constant strings,
	// which the host toolchain collapses into one runtime call.
	expect("x y z\n", func() { println("x", "y", "z") })
	expect("1 2 3\n", func() { println(1, 2, 3) })

	// Integers, signed and unsigned, at their extremes.
	var minimum int64 = -9223372036854775808
	var maximum uint64 = 18446744073709551615
	var small int8 = -5
	var byteValue uint8 = 200
	expect("-9223372036854775808 18446744073709551615 -5 200\n", func() {
		println(minimum, maximum, small, byteValue)
	})

	// A rune constant is an integer operand, printed as its value.
	expect("120\n", func() { println('x') })

	// Floating point, both widths, formatted by the runtime rather than by fmt.
	var single float32 = 1.5
	var double float64 = 2.25
	expect("1.5 2.25 -0.5\n", func() { println(single, double, -0.5) })

	// Complex, both widths.
	var narrow complex64 = complex(3.5, 4.5)
	var wide complex128 = complex(-1.25, 0)
	expect("(3.5+4.5i) (-1.25+0i)\n", func() { println(narrow, wide) })

	// Booleans and strings held in variables rather than constants.
	yes, no := true, false
	text := "held"
	expect("true false held\n", func() { println(yes, no, text) })

	// A nil interface prints both words, not a single zero. This is the case
	// that segfaulted while the operand was being passed as a bare pointer.
	var empty any
	var failure error
	expect("(0x0,0x0) (0x0,0x0)\n", func() { println(empty, failure) })

	// The address-shaped operands: only their length and capacity are
	// predictable, so their shape is asserted instead of their text.
	assertShape("slice", capture(func() { println(make([]byte, 3, 5)) }), "[3/5]0x", "\n")
	assertShape("empty slice", capture(func() { println([]int{}) }), "[0/0]0x", "\n")

	pointer := new(int)
	assertShape("pointer", capture(func() { println(pointer) }), "0x", "\n")
	assertShape("unsafe pointer", capture(func() { println(unsafe.Pointer(pointer)) }), "0x", "\n")
	assertShape("channel", capture(func() { println(make(chan int, 1)) }), "0x", "\n")
	assertShape("map", capture(func() { println(map[int]int{}) }), "0x", "\n")
	assertShape("func", capture(func() { println(main) }), "0x", "\n")

	// A uintptr is an unsigned integer operand, so it prints in decimal even
	// though the pointer it came from prints in hexadecimal.
	assertDigits("uintptr", capture(func() { println(uintptr(unsafe.Pointer(pointer))) }))

	// A non-nil interface prints the descriptor and the payload as one pair.
	empty = 42
	assertShape("interface", capture(func() { println(empty) }), "(0x", ")\n")

	// Operands are evaluated before any of them is printed, so an operand whose
	// evaluation prints does not interleave with the statement printing it.
	expect("inner\nouter 7 8\n", func() { println("outer", noisy(7), 8) })

	println("print-operand-separation ok")
}

func noisy(value int) int {
	println("inner")
	return value
}

func expect(want string, print func()) {
	got := capture(print)
	if got != want {
		panic("println wrote " + quote(got) + ", want " + quote(want))
	}
}

func assertShape(label, got, prefix, suffix string) {
	if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) {
		panic("println of a " + label + " wrote " + quote(got) + ", want " + quote(prefix) + "...." + quote(suffix))
	}
	if len(got) <= len(prefix)+len(suffix) {
		panic("println of a " + label + " wrote no value: " + quote(got))
	}
}

func assertDigits(label, got string) {
	body := strings.TrimSuffix(got, "\n")
	if body == got || body == "" {
		panic("println of a " + label + " wrote " + quote(got) + ", want decimal digits and a newline")
	}
	for _, character := range body {
		if character < '0' || character > '9' {
			panic("println of a " + label + " wrote " + quote(got) + ", want decimal digits")
		}
	}
}

// capture runs print with file descriptor 2 pointed at a temporary file and
// returns everything written to it. A temporary file rather than a pipe, so
// that no reader is needed and no output can be lost to a full pipe buffer.
func capture(print func()) string {
	file, err := os.CreateTemp("", "goc-println-capture")
	if err != nil {
		panic(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()

	saved, err := syscall.Dup(2)
	if err != nil {
		panic(err)
	}
	if err := syscall.Dup3(int(file.Fd()), 2, 0); err != nil {
		panic(err)
	}
	print()
	if err := syscall.Dup3(saved, 2, 0); err != nil {
		panic(err)
	}
	if err := syscall.Close(saved); err != nil {
		panic(err)
	}

	if _, err := file.Seek(0, 0); err != nil {
		panic(err)
	}
	buffer := make([]byte, 8192)
	read, err := file.Read(buffer)
	if err != nil && read == 0 {
		return ""
	}
	return string(buffer[:read])
}

// quote renders a captured string readably in a panic message without pulling
// in fmt, whose own output would go through a different path than the one under
// test.
func quote(text string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for index := 0; index < len(text); index++ {
		switch character := text[index]; character {
		case '\n':
			builder.WriteString("\\n")
		case '"':
			builder.WriteString("\\\"")
		default:
			builder.WriteByte(character)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
