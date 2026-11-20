#include "textflag.h"

// func call(fn, a, b, c, sp uintptr) uint64
//
// Compiled code follows the System V ABI and runs on a stack the jit
// package owns, so it never touches the goroutine's stack. R12 is
// callee-saved in System V, so it survives the call and keeps the Go
// stack pointer. Go's ABI wrapper around this ABI0 function restores the
// registers Go's internal ABI pins, such as R14 and X15.
TEXT ·call(SB), NOSPLIT, $0-48
	MOVQ fn+0(FP), AX
	MOVQ a+8(FP), DI
	MOVQ b+16(FP), SI
	MOVQ c+24(FP), DX
	MOVQ sp+32(FP), BX
	MOVQ SP, R12
	ANDQ $~15, BX
	MOVQ BX, SP
	CALL AX
	MOVQ R12, SP
	MOVQ AX, ret+40(FP)
	RET
