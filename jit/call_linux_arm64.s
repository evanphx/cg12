#include "textflag.h"

// func call(fn, a, b, c, sp uintptr) uint64
//
// Compiled code follows AAPCS64 and runs on a stack the jit package owns.
// R19 and R20 are callee-saved in AAPCS64, so they survive the call and
// keep the Go stack pointer and the link register; Go's ABI doesn't need
// them preserved. g, in R28, is callee-saved there too.
TEXT ·call(SB), NOSPLIT|NOFRAME, $0-48
	MOVD fn+0(FP), R9
	MOVD a+8(FP), R0
	MOVD b+16(FP), R1
	MOVD c+24(FP), R2
	MOVD sp+32(FP), R10
	MOVD RSP, R19
	MOVD R30, R20
	AND  $~15, R10
	MOVD R10, RSP
	CALL (R9)
	MOVD R19, RSP
	MOVD R20, R30
	MOVD R0, ret+40(FP)
	RET

// func flushICache(addr, n uintptr)
//
// Cleans the data cache to the point of unification, then invalidates the
// instruction cache, line by line, with the line sizes CTR_EL0 gives.
TEXT ·flushICache(SB), NOSPLIT|NOFRAME, $0-16
	MOVD addr+0(FP), R0
	MOVD n+8(FP), R1
	ADD  R0, R1, R1           // end
	WORD $0xd53b0022          // MRS X2, CTR_EL0
	MOVD $4, R4
	UBFX $16, R2, $4, R3
	LSL  R3, R4, R5           // data cache line size
	AND  $15, R2, R3
	LSL  R3, R4, R6           // instruction cache line size
	SUB  $1, R5, R8
	BIC  R8, R0, R7
dloop:
	WORD $0xd50b7b27          // DC CVAU, X7
	ADD  R5, R7, R7
	CMP  R1, R7
	BLO  dloop
	WORD $0xd5033b9f          // DSB ISH
	SUB  $1, R6, R8
	BIC  R8, R0, R7
iloop:
	WORD $0xd50b7527          // IC IVAU, X7
	ADD  R6, R7, R7
	CMP  R1, R7
	BLO  iloop
	WORD $0xd5033b9f          // DSB ISH
	WORD $0xd5033fdf          // ISB
	RET
