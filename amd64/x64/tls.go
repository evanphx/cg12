package x64

// Thread-local storage addressing.
//
// x86-64 uses TLS variant II: %fs holds the thread pointer, and the thread's
// block sits *below* it, so a variable's offset from the pointer is negative.
// Reaching one is therefore always "get the thread pointer, add an offset"; only
// where the offset comes from differs between the models. MovFSZero (in x64.go)
// is the first half and is shared by every model; this file holds the second half
// for the models whose offset is not a link-time constant.

// AddGotTPOff encodes ADD rd, [rip + disp32] (REX.W 03 /r with ModRM mod=00
// rm=101), the second instruction of an initial-exec thread-local access: rd
// already holds the thread pointer, and the GOT slot named by the displacement
// holds the variable's offset from it, put there by the loader once it has placed
// the owning module's TLS block.
//
// This is AddMem with a RIP-relative operand and could be written that way at the
// call site. It has a name of its own because the exact encoding is load-bearing
// beyond what the instruction computes. When the image being linked turns out not
// to need a GOT slot at all -- a static link, or a definition the linker can see
// is in the executable itself -- the linker rewrites this instruction in place
// into `ADD rd, $imm32` and drops the slot, relaxing initial-exec to local-exec.
// It recognizes the instruction to rewrite by its bytes: a REX prefix with W set,
// opcode 0x03, ModRM 0x05. Any other encoding of the same addition -- the 0x01
// direction, or a separate load into a scratch register followed by a register
// add -- computes the right value but is not a shape the linker knows how to
// relax, and a static link then keeps a GOT slot that no loader will ever fill.
//
// The displacement is the last four bytes, so a relocation patches the tail of
// what this returns.
func AddGotTPOff(dst Reg) []byte {
	return AddMem(true, dst, RIPRel(0))
}
