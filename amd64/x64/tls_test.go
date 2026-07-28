package x64

import "testing"

// The initial-exec addition must disassemble as a RIP-relative memory add with
// REX.W set, which is both what the instruction has to compute and the only shape
// a linker will relax to local-exec. The displacement is zero here because the
// relocation supplies it; llvm-mc renders a zero RIP displacement as "(%rip)".
func TestAddGotTPOff(t *testing.T) {
	check(t, "addq (%rip), %rax", AddGotTPOff(RAX))
	check(t, "addq (%rip), %r11", AddGotTPOff(R11))
	check(t, "addq (%rip), %rdi", AddGotTPOff(RDI))
}

// The relocation patches the last four bytes, so the encoding must end in the
// displacement field and be exactly seven bytes long -- REX, opcode, ModRM, then
// disp32. A test for a length is normally not worth writing; this one is, because
// the emitter locates the relocation by counting back four bytes from the end of
// the program.
func TestAddGotTPOffLayout(t *testing.T) {
	code := AddGotTPOff(RAX)
	if len(code) != 7 {
		t.Fatalf("AddGotTPOff: got %d bytes (% x), want 7", len(code), code)
	}
	for i, b := range code[3:] {
		if b != 0 {
			t.Fatalf("AddGotTPOff: displacement byte %d is %#x, want a zero field for the relocation", i, b)
		}
	}
}
