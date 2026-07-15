.text

// These helpers deliberately use the write system call instead of libc. The
// runtime creates threads with clone before it has any cgo/libc thread state,
// so calling printf from those threads corrupts libc's internal state.
.global runtime_gocPrintString
.type runtime_gocPrintString, %function
runtime_gocPrintString:
	mov x2, x1
	mov x1, x0
	mov x0, 2
	mov x8, 64
	svc 0
	ret

// runtime_gocPrintInteger formats x0 into a stack buffer. x1 selects signed
// decimal (0), unsigned decimal (1), or hexadecimal with a 0x prefix (2).
.global runtime_gocPrintInteger
.type runtime_gocPrintInteger, %function
runtime_gocPrintInteger:
	// Preserve the Go frame-pointer link at -8 of the caller's SP.
	sub sp, sp, 80
	add x3, sp, 64
	mov x4, x0
	mov x5, x1
	cmp x5, 2
	b.eq 4f
	cmp x5, 0
	b.ne 1f
	tbz x4, 63, 1f
	neg x4, x4
	mov w6, '-'
	b 2f
1:	mov w6, 0
2:	mov x7, 10
3:	udiv x9, x4, x7
	msub x10, x9, x7, x4
	add w10, w10, '0'
	strb w10, [x3, -1]!
	mov x4, x9
	cbnz x4, 3b
	cbz w6, 7f
	strb w6, [x3, -1]!
	b 7f
4:	mov x7, 16
5:	and w10, w4, 15
	cmp w10, 10
	b.lo 6f
	add w10, w10, 'a' - 10
	b 8f
6:	add w10, w10, '0'
8:	strb w10, [x3, -1]!
	lsr x4, x4, 4
	cbnz x4, 5b
	mov w10, 'x'
	strb w10, [x3, -1]!
	mov w10, '0'
	strb w10, [x3, -1]!
7:	add x2, sp, 64
	sub x2, x2, x3
	mov x1, x3
	mov x0, 2
	mov x8, 64
	svc 0
	add sp, sp, 80
	ret

.macro load32 name
.global \name
.type \name, %function
\name:
	ldar w0, [x0]
	ret
.endm

.macro load64 name
.global \name
.type \name, %function
\name:
	ldar x0, [x0]
	ret
.endm

.macro store32 name
.global \name
.type \name, %function
\name:
	stlr w1, [x0]
	ret
.endm

.macro store64 name
.global \name
.type \name, %function
\name:
	stlr x1, [x0]
	ret
.endm

.macro cas32 name
.global \name
.type \name, %function
\name:
1:	ldaxr w3, [x0]
	cmp w3, w1
	b.ne 2f
	stlxr w4, w2, [x0]
	cbnz w4, 1b
	mov w0, 1
	ret
2:	clrex
	mov w0, 0
	ret
.endm

.macro cas64 name
.global \name
.type \name, %function
\name:
1:	ldaxr x3, [x0]
	cmp x3, x1
	b.ne 2f
	stlxr w4, x2, [x0]
	cbnz w4, 1b
	mov w0, 1
	ret
2:	clrex
	mov w0, 0
	ret
.endm

.macro xadd32 name
.global \name
.type \name, %function
\name:
1:	ldaxr w2, [x0]
	add w3, w2, w1
	stlxr w4, w3, [x0]
	cbnz w4, 1b
	mov w0, w3
	ret
.endm

.macro xadd64 name
.global \name
.type \name, %function
\name:
1:	ldaxr x2, [x0]
	add x3, x2, x1
	stlxr w4, x3, [x0]
	cbnz w4, 1b
	mov x0, x3
	ret
.endm

.macro xchg32 name
.global \name
.type \name, %function
\name:
1:	ldaxr w2, [x0]
	stlxr w3, w1, [x0]
	cbnz w3, 1b
	mov w0, w2
	ret
.endm

.macro xchg64 name
.global \name
.type \name, %function
\name:
1:	ldaxr x2, [x0]
	stlxr w3, x1, [x0]
	cbnz w3, 1b
	mov x0, x2
	ret
.endm

.macro bitop32 name, op
.global \name
.type \name, %function
\name:
1:	ldaxr w2, [x0]
	\op w3, w2, w1
	stlxr w4, w3, [x0]
	cbnz w4, 1b
	mov w0, w2
	ret
.endm

.macro bitop64 name, op
.global \name
.type \name, %function
\name:
1:	ldaxr x2, [x0]
	\op x3, x2, x1
	stlxr w4, x3, [x0]
	cbnz w4, 1b
	mov x0, x2
	ret
.endm

load32 internal_runtime_atomic_Load
load32 internal_runtime_atomic_LoadAcq
load32 internal_runtime_atomic_Loadint32
load64 internal_runtime_atomic_Load64
load64 internal_runtime_atomic_LoadAcq64
load64 internal_runtime_atomic_LoadAcquintptr
load64 internal_runtime_atomic_Loadint64
load64 internal_runtime_atomic_Loaduintptr
load64 internal_runtime_atomic_Loaduint
load64 internal_runtime_atomic_Loadp

.global internal_runtime_atomic_Load8
.type internal_runtime_atomic_Load8, %function
internal_runtime_atomic_Load8:
	ldarb w0, [x0]
	ret

store32 internal_runtime_atomic_Store
store32 internal_runtime_atomic_StoreRel
store32 internal_runtime_atomic_Storeint32
store64 internal_runtime_atomic_Store64
store64 internal_runtime_atomic_StoreRel64
store64 internal_runtime_atomic_StoreReluintptr
store64 internal_runtime_atomic_Storeint64
store64 internal_runtime_atomic_Storeuintptr
store64 internal_runtime_atomic_StorepNoWB

.global internal_runtime_atomic_Store8
.type internal_runtime_atomic_Store8, %function
internal_runtime_atomic_Store8:
	stlrb w1, [x0]
	ret

cas32 internal_runtime_atomic_Cas
cas32 internal_runtime_atomic_CasRel
cas32 internal_runtime_atomic_Casint32
cas64 internal_runtime_atomic_Cas64
cas64 internal_runtime_atomic_Casint64
cas64 internal_runtime_atomic_Casuintptr
cas64 internal_runtime_atomic_Casp1

xadd32 internal_runtime_atomic_Xadd
xadd32 internal_runtime_atomic_Xaddint32
xadd64 internal_runtime_atomic_Xadd64
xadd64 internal_runtime_atomic_Xaddint64
xadd64 internal_runtime_atomic_Xadduintptr

xchg32 internal_runtime_atomic_Xchg
xchg32 internal_runtime_atomic_Xchgint32
xchg64 internal_runtime_atomic_Xchg64
xchg64 internal_runtime_atomic_Xchguintptr
xchg64 internal_runtime_atomic_Xchgint64

.global internal_runtime_atomic_Xchg8
.type internal_runtime_atomic_Xchg8, %function
internal_runtime_atomic_Xchg8:
1:	ldaxrb w2, [x0]
	stlxrb w3, w1, [x0]
	cbnz w3, 1b
	mov w0, w2
	ret

bitop32 internal_runtime_atomic_And, and
bitop32 internal_runtime_atomic_And32, and
bitop32 internal_runtime_atomic_Or, orr
bitop32 internal_runtime_atomic_Or32, orr
bitop64 internal_runtime_atomic_And64, and
bitop64 internal_runtime_atomic_Anduintptr, and
bitop64 internal_runtime_atomic_Or64, orr
bitop64 internal_runtime_atomic_Oruintptr, orr

.global internal_runtime_atomic_And8
.type internal_runtime_atomic_And8, %function
internal_runtime_atomic_And8:
1:	ldaxrb w2, [x0]
	and w3, w2, w1
	stlxrb w4, w3, [x0]
	cbnz w4, 1b
	ret

.global internal_runtime_atomic_Or8
.type internal_runtime_atomic_Or8, %function
internal_runtime_atomic_Or8:
1:	ldaxrb w2, [x0]
	orr w3, w2, w1
	stlxrb w4, w3, [x0]
	cbnz w4, 1b
	ret

.macro branch_alias name, target
.global \name
.type \name, %function
\name:
	b \target
.endm

branch_alias internal_runtime_atomic_casPointer, internal_runtime_atomic_Cas64
branch_alias internal_runtime_atomic_storePointer, internal_runtime_atomic_Store64
branch_alias internal_runtime_atomic_Pointer_g__CompareAndSwapNoWB, internal_runtime_atomic_Cas64
branch_alias internal_runtime_atomic_Pointer_g__Load, internal_runtime_atomic_Load64
branch_alias internal_runtime_atomic_Pointer_m__Load, internal_runtime_atomic_Load64
branch_alias internal_runtime_atomic_Pointer_m__Store, internal_runtime_atomic_Store64
branch_alias internal_runtime_atomic_Pointer_profBuf__Load, internal_runtime_atomic_Load64
branch_alias internal_runtime_atomic_Pointer_spanSetBlock__Load, internal_runtime_atomic_Load64
branch_alias internal_runtime_atomic_Pointer_spanSetBlock__StoreNoWB, internal_runtime_atomic_Store64

// sync/atomic's public ARM64 entry points have the same calling convention and
// memory-ordering requirements as the internal/runtime/atomic primitives above.
branch_alias sync_atomic_LoadInt32, internal_runtime_atomic_Load
branch_alias sync_atomic_LoadInt64, internal_runtime_atomic_Load64
branch_alias sync_atomic_LoadUint32, internal_runtime_atomic_Load
branch_alias sync_atomic_LoadUint64, internal_runtime_atomic_Load64
branch_alias sync_atomic_LoadUintptr, internal_runtime_atomic_Loaduintptr
branch_alias sync_atomic_LoadPointer, internal_runtime_atomic_Loadp
branch_alias sync_atomic_StoreInt32, internal_runtime_atomic_Store
branch_alias sync_atomic_StoreInt64, internal_runtime_atomic_Store64
branch_alias sync_atomic_StoreUint32, internal_runtime_atomic_Store
branch_alias sync_atomic_StoreUint64, internal_runtime_atomic_Store64
branch_alias sync_atomic_StoreUintptr, internal_runtime_atomic_Storeuintptr
branch_alias sync_atomic_SwapInt32, internal_runtime_atomic_Xchg
branch_alias sync_atomic_SwapInt64, internal_runtime_atomic_Xchg64
branch_alias sync_atomic_SwapUint32, internal_runtime_atomic_Xchg
branch_alias sync_atomic_SwapUint64, internal_runtime_atomic_Xchg64
branch_alias sync_atomic_SwapUintptr, internal_runtime_atomic_Xchguintptr
branch_alias sync_atomic_AddInt32, internal_runtime_atomic_Xadd
branch_alias sync_atomic_AddInt64, internal_runtime_atomic_Xadd64
branch_alias sync_atomic_AddUint32, internal_runtime_atomic_Xadd
branch_alias sync_atomic_AddUint64, internal_runtime_atomic_Xadd64
branch_alias sync_atomic_AddUintptr, internal_runtime_atomic_Xadduintptr
branch_alias sync_atomic_CompareAndSwapInt32, internal_runtime_atomic_Cas
branch_alias sync_atomic_CompareAndSwapInt64, internal_runtime_atomic_Cas64
branch_alias sync_atomic_CompareAndSwapUint32, internal_runtime_atomic_Cas
branch_alias sync_atomic_CompareAndSwapUint64, internal_runtime_atomic_Cas64
branch_alias sync_atomic_CompareAndSwapUintptr, internal_runtime_atomic_Casuintptr

.global internal_runtime_sys_GetCallerSP
.type internal_runtime_sys_GetCallerSP, %function
internal_runtime_sys_GetCallerSP:
	mov x0, sp
	ret

.global internal_runtime_sys_GetCallerPC
.type internal_runtime_sys_GetCallerPC, %function
internal_runtime_sys_GetCallerPC:
	mov x0, x30
	ret

.global runtime_getfp
.type runtime_getfp, %function
runtime_getfp:
	mov x0, x29
	ret

.global runtime_procyieldAsm
.type runtime_procyieldAsm, %function
runtime_procyieldAsm:
	cbz w0, 2f
1:	yield
	subs w0, w0, 1
	b.ne 1b
2:	ret

.global runtime_osyield
.type runtime_osyield, %function
runtime_osyield:
	mov x8, 124
	svc 0
	ret

.global runtime_getpid
.type runtime_getpid, %function
runtime_getpid:
	mov x8, 172
	svc 0
	ret

.global runtime_exit
.type runtime_exit, %function
runtime_exit:
	mov x8, 94
	svc 0
	brk 0

.text
.global internal_runtime_sys_DITEnabled
.type internal_runtime_sys_DITEnabled, %function
internal_runtime_sys_DITEnabled:
	mov w0, 0
	ret

.global internal_runtime_sys_DisableDIT
.type internal_runtime_sys_DisableDIT, %function
internal_runtime_sys_DisableDIT:
	ret
.global internal_runtime_sys_EnableDIT
.type internal_runtime_sys_EnableDIT, %function
internal_runtime_sys_EnableDIT:
	ret

// reflect.methodValueCall is only entered by reflection-created method
// closures. Keep a hard failure for that unsupported assembly trampoline while
// allowing ordinary reflect users to link; silently returning would corrupt
// the caller's register and stack result areas.
.global reflect_methodValueCall
.type reflect_methodValueCall, %function
reflect_methodValueCall:
	brk 0

// setg installs the goroutine pointer used by cg12-generated Go code.
.global runtime_setg
.type runtime_setg, %function
runtime_setg:
	mov x28, x0
	ret

// reflectcall requires the full runtime dynamic-call frame dispatcher. Trap
// explicitly until that assembly path is available rather than returning with
// an uninitialized result frame.
.global runtime_reflectcall
.type runtime_reflectcall, %function
runtime_reflectcall:
	brk 0

// func rawVforkSyscall(trap, a1, a2, a3 uintptr) (r1, err uintptr)
// The second result uses cg12's hidden-result pointer in x4.
.global syscall_rawVforkSyscall
.type syscall_rawVforkSyscall, %function
syscall_rawVforkSyscall:
	mov x8, x0
	mov x0, x1
	mov x1, x2
	mov x2, x3
	mov x3, 0
	mov x5, 0
	mov x6, 0
	svc 0
	cmn x0, 4095
	b.cc 1f
	mov x1, -1
	neg x0, x0
	str x0, [x4]
	mov x0, x1
	ret
1:	str xzr, [x4]
	ret

// func rawSyscallNoError(trap, a1, a2, a3 uintptr) (r1, r2 uintptr)
// The second result uses cg12's hidden-result pointer in x4.
.global syscall_rawSyscallNoError
.type syscall_rawSyscallNoError, %function
syscall_rawSyscallNoError:
	mov x8, x0
	mov x0, x1
	mov x1, x2
	mov x2, x3
	mov x3, 0
	mov x5, 0
	mov x6, 0
	svc 0
	str x1, [x4]
	ret

.macro linux_syscall name, number
.global \name
.type \name, %function
\name:
	mov x8, \number
	svc 0
	ret
.endm

linux_syscall runtime_read, 63
linux_syscall runtime_write1, 64
linux_syscall runtime_futex, 98
linux_syscall runtime_timer_create, 107
linux_syscall runtime_timer_settime, 110
linux_syscall runtime_timer_delete, 111
linux_syscall runtime_raiseproc, 129
linux_syscall runtime_tgkill, 131
linux_syscall runtime_rt_sigaction, 134
linux_syscall runtime_rtsigprocmask, 135
linux_syscall runtime_madvise, 233

.global runtime_raise
.type runtime_raise, %function
runtime_raise:
	mov w9, w0
	mov x8, 172
	svc 0
	mov x10, x0
	mov x8, 178
	svc 0
	mov x1, x0
	mov x0, x10
	mov w2, w9
	mov x8, 131
	svc 0
	ret

.global runtime_usleep
.type runtime_usleep, %function
runtime_usleep:
	sub sp, sp, 32
	uxtw x3, w0
	ldr x4, =1000000
	udiv x1, x3, x4
	msub x2, x1, x4, x3
	mov x4, 1000
	mul x2, x2, x4
	stp x1, x2, [sp]
	mov x0, sp
	mov x1, 0
	mov x8, 101
	svc 0
	add sp, sp, 32
	ret

.global runtime_sysMmap
.type runtime_sysMmap, %function
runtime_sysMmap:
	mov x8, 222
	svc 0
	cmn x0, 4095
	b.lo 1f
	neg x9, x0
	str x9, [x6]
	mov x0, 0
	ret
1:	str xzr, [x6]
	ret

linux_syscall runtime_sysMunmap, 215

.global runtime_nanotime1
.type runtime_nanotime1, %function
runtime_nanotime1:
	sub sp, sp, 32
	mov x0, 1
	mov x1, sp
	mov x8, 113
	svc 0
	ldp x0, x1, [sp]
	ldr x2, =1000000000
	madd x0, x0, x2, x1
	add sp, sp, 32
	ret

.global runtime_walltime
.type runtime_walltime, %function
runtime_walltime:
	mov x9, x0
	sub sp, sp, 32
	mov x0, 0
	mov x1, sp
	mov x8, 113
	svc 0
	ldp x0, x1, [sp]
	str w1, [x9]
	add sp, sp, 32
	ret

.global runtime_secretEraseRegisters
.type runtime_secretEraseRegisters, %function
runtime_secretEraseRegisters:
	ret

.global internal_abi_FuncPCABI0
.type internal_abi_FuncPCABI0, %function
internal_abi_FuncPCABI0:
	ldr x0, [x0, 8]
	ldr x0, [x0]
	ret

branch_alias internal_abi_FuncPCABIInternal, internal_abi_FuncPCABI0

.global runtime_memhash
.type runtime_memhash, %function
runtime_memhash:
	ldr x3, =1099511628211
	ldr x4, =1469598103934665603
	eor x1, x1, x4
1:	cbz x2, 2f
	ldrb w4, [x0], 1
	eor x1, x1, x4
	mul x1, x1, x3
	sub x2, x2, 1
	b 1b
2:	mov x0, x1
	ret

.weak internal_chacha8rand_block_generic
branch_alias internal_chacha8rand_block, internal_chacha8rand_block_generic

.global internal_runtime_syscall_linux_Syscall6
.type internal_runtime_syscall_linux_Syscall6, %function
internal_runtime_syscall_linux_Syscall6:
	mov x9, x8
	mov x10, x7
	mov x8, x0
	mov x0, x1
	mov x1, x2
	mov x2, x3
	mov x3, x4
	mov x4, x5
	mov x5, x6
	svc 0
	cmn x0, 4095
	b.lo 1f
	neg x11, x0
	mov x0, -1
	str xzr, [x10]
	str x11, [x9]
	ret
1:	str x1, [x10]
	str xzr, [x9]
	ret

.global runtime_getg
.type runtime_getg, %function
runtime_getg:
	mov x0, x28
	ret

.global runtime_gogo
.type runtime_gogo, %function
runtime_gogo:
	ldr x28, [x0, 16]
	ldr x10, [x28, 48]
	str x28, [x10, 184]
	ldr x9, [x0]
	mov sp, x9
	ldr x29, [x0, 40]
	ldr x30, [x0, 32]
	ldr x26, [x0, 24]
	ldr x9, [x0, 8]
	str xzr, [x0]
	str xzr, [x0, 24]
	str xzr, [x0, 32]
	str xzr, [x0, 40]
	br x9

.global runtime_mcall
.type runtime_mcall, %function
runtime_mcall:
	mov x26, x0
	mov x9, sp
	str x9, [x28, 56]
	str x29, [x28, 96]
	str x30, [x28, 64]
	str xzr, [x28, 88]
	mov x0, x28
	ldr x10, [x28, 48]
	ldr x28, [x10]
	ldr x9, [x28, 56]
	mov sp, x9
	mov x29, 0
	ldr x9, [x26]
	blr x9
	brk 0

.weak runtime_newstack
.global runtime_morestack
.type runtime_morestack, %function
runtime_morestack:
	// Preserve register arguments in a frame that newstack will copy. x27 holds
	// the interrupted function's caller LR; x30 is the retry PC after BL. The
	// stack unwinder sees x27 at the frame's LR slot and therefore skips the
	// interrupted function, whose stack check ran before it allocated a frame.
	// Execution uses the retry PC kept separately at offset 136.
	sub sp, sp, 560
	str x27, [sp, 0]
	stp x0, x1, [sp, 8]
	stp x2, x3, [sp, 24]
	stp x4, x5, [sp, 40]
	stp x6, x7, [sp, 56]
	stp x8, x9, [sp, 72]
	stp x10, x11, [sp, 88]
	stp x12, x13, [sp, 104]
	stp x14, x15, [sp, 120]
	str x30, [sp, 136]
	str q0, [sp, 144]
	str q1, [sp, 160]
	str q2, [sp, 176]
	str q3, [sp, 192]
	str q4, [sp, 208]
	str q5, [sp, 224]
	str q6, [sp, 240]
	str q7, [sp, 256]
	str q8, [sp, 272]
	str q9, [sp, 288]
	str q10, [sp, 304]
	str q11, [sp, 320]
	str q12, [sp, 336]
	str q13, [sp, 352]
	str q14, [sp, 368]
	str q15, [sp, 384]
	cbz x29, 2f
	ldr x9, [x29]
	b 3f
2:	mov x9, 0
3:	str x9, [sp, 400]
	str x26, [sp, 408]
	str x25, [sp, 544]

	// The original argument values above are outside the pointer bitmap. Copy
	// only pointer-bearing registers into the scanned mirror area. x25 is a
	// per-function bitmap supplied by the generated stack-check slow path.
	tbz x25, 0, 10f
	str x0, [sp, 416]
	b 11f
10:	str xzr, [sp, 416]
11:	tbz x25, 1, 12f
	str x1, [sp, 424]
	b 13f
12:	str xzr, [sp, 424]
13:	tbz x25, 2, 14f
	str x2, [sp, 432]
	b 15f
14:	str xzr, [sp, 432]
15:	tbz x25, 3, 16f
	str x3, [sp, 440]
	b 17f
16:	str xzr, [sp, 440]
17:	tbz x25, 4, 18f
	str x4, [sp, 448]
	b 19f
18:	str xzr, [sp, 448]
19:	tbz x25, 5, 20f
	str x5, [sp, 456]
	b 21f
20:	str xzr, [sp, 456]
21:	tbz x25, 6, 22f
	str x6, [sp, 464]
	b 23f
22:	str xzr, [sp, 464]
23:	tbz x25, 7, 24f
	str x7, [sp, 472]
	b 25f
24:	str xzr, [sp, 472]
25:	tbz x25, 8, 26f
	str x8, [sp, 480]
	b 27f
26:	str xzr, [sp, 480]
27:	tbz x25, 9, 28f
	str x9, [sp, 488]
	b 29f
28:	str xzr, [sp, 488]
29:	tbz x25, 10, 30f
	str x10, [sp, 496]
	b 31f
30:	str xzr, [sp, 496]
31:	tbz x25, 11, 32f
	str x11, [sp, 504]
	b 33f
32:	str xzr, [sp, 504]
33:	tbz x25, 12, 34f
	str x12, [sp, 512]
	b 35f
34:	str xzr, [sp, 512]
35:	tbz x25, 13, 36f
	str x13, [sp, 520]
	b 37f
36:	str xzr, [sp, 520]
37:	tbz x25, 14, 38f
	str x14, [sp, 528]
	b 39f
38:	str xzr, [sp, 528]
39:	tbz x25, 15, 40f
	str x15, [sp, 536]
	b 41f
40:	str xzr, [sp, 536]
41:

	ldr x8, [x28, 48]
	mov x9, sp
	str x9, [x28, 56]
	adr x10, runtime_morestack_restore
	str x10, [x28, 64]
	str x28, [x28, 72]
	str x26, [x28, 80]
	str x27, [x28, 88]
	str x29, [x28, 96]

	ldr x4, [x8]
	cmp x28, x4
	b.eq 1f
	ldr x4, [x8, 72]
	cmp x28, x4
	b.eq 1f

	// m.morebuf is the gobuf immediately following m.g0.
	str x27, [x8, 16]
	add x10, x9, 560
	str x10, [x8, 8]
	str x28, [x8, 24]

	ldr x28, [x8]
	ldr x9, [x28, 56]
	mov sp, x9
	mov x29, 0
	sub sp, sp, 16
	str xzr, [sp]
	bl runtime_newstack
	brk 0
1:	brk 0

.global runtime_morestack_restore
.type runtime_morestack_restore, %function
runtime_morestack_restore:
	ldr x16, [sp, 136]
	ldr x26, [sp, 408]
	ldr x25, [sp, 544]
	ldr x9, [sp, 400]
	cbz x29, 2f
	str x9, [x29]
2:
	ldr q0, [sp, 144]
	ldr q1, [sp, 160]
	ldr q2, [sp, 176]
	ldr q3, [sp, 192]
	ldr q4, [sp, 208]
	ldr q5, [sp, 224]
	ldr q6, [sp, 240]
	ldr q7, [sp, 256]
	ldr q8, [sp, 272]
	ldr q9, [sp, 288]
	ldr q10, [sp, 304]
	ldr q11, [sp, 320]
	ldr q12, [sp, 336]
	ldr q13, [sp, 352]
	ldr q14, [sp, 368]
	ldr q15, [sp, 384]
	ldp x0, x1, [sp, 8]
	ldp x2, x3, [sp, 24]
	ldp x4, x5, [sp, 40]
	ldp x6, x7, [sp, 56]
	ldp x8, x9, [sp, 72]
	ldp x10, x11, [sp, 88]
	ldp x12, x13, [sp, 104]
	ldp x14, x15, [sp, 120]
	tbz x25, 0, 50f
	ldr x0, [sp, 416]
50:	tbz x25, 1, 51f
	ldr x1, [sp, 424]
51:	tbz x25, 2, 52f
	ldr x2, [sp, 432]
52:	tbz x25, 3, 53f
	ldr x3, [sp, 440]
53:	tbz x25, 4, 54f
	ldr x4, [sp, 448]
54:	tbz x25, 5, 55f
	ldr x5, [sp, 456]
55:	tbz x25, 6, 56f
	ldr x6, [sp, 464]
56:	tbz x25, 7, 57f
	ldr x7, [sp, 472]
57:	tbz x25, 8, 58f
	ldr x8, [sp, 480]
58:	tbz x25, 9, 59f
	ldr x9, [sp, 488]
59:	tbz x25, 10, 60f
	ldr x10, [sp, 496]
60:	tbz x25, 11, 61f
	ldr x11, [sp, 504]
61:	tbz x25, 12, 62f
	ldr x12, [sp, 512]
62:	tbz x25, 13, 63f
	ldr x13, [sp, 520]
63:	tbz x25, 14, 64f
	ldr x14, [sp, 528]
64:	tbz x25, 15, 65f
	ldr x15, [sp, 536]
65:	add sp, sp, 560
	br x16
.global runtime_morestack_restore_end
runtime_morestack_restore_end:

.global runtime_morestack_noctxt
.type runtime_morestack_noctxt, %function
runtime_morestack_noctxt:
	mov x26, 0
	b runtime_morestack

.global runtime_systemstack
.type runtime_systemstack, %function
runtime_systemstack:
	mov x26, x0
	ldr x9, [x28, 48]
	ldr x10, [x9, 72]
	cmp x28, x10
	b.eq 2f
	ldr x10, [x9]
	cmp x28, x10
	b.eq 2f
	ldr x10, [x9, 184]
	cmp x28, x10
	b.ne 3f
	mov x11, sp
	str x11, [x28, 56]
	str x29, [x28, 96]
	adr x11, runtime_systemstack_switch
	str x11, [x28, 64]
	str x28, [x28, 72]
	str xzr, [x28, 80]
	str x30, [x28, 88]
	mov x11, x28
	ldr x28, [x9]
	ldr x12, [x28, 56]
	mov sp, x12
	ldr x12, [x26]
	blr x12
	ldr x9, [x28, 48]
	ldr x28, [x9, 184]
	ldr x12, [x28, 56]
	ldr x30, [x28, 88]
	ldr x29, [x28, 96]
	mov sp, x12
	str xzr, [x28, 56]
	str xzr, [x28, 96]
	ret
2:	ldr x9, [x26]
	br x9
3:	brk 0

.global runtime_systemstack_switch
.type runtime_systemstack_switch, %function
runtime_systemstack_switch:
	ret

.global runtime_mstart
.type runtime_mstart, %function
runtime_mstart:
	mov x29, 0
	bl runtime_mstart0
	ret

.weak runtime_mstart0
.type runtime_mstart0, %function
runtime_mstart0:
	brk 0

.global runtime_asminit
.type runtime_asminit, %function
runtime_asminit:
	ret

.global runtime_gettid
.type runtime_gettid, %function
runtime_gettid:
	mov x8, 178
	svc 0
	ret

.global runtime_sigaltstack
.type runtime_sigaltstack, %function
runtime_sigaltstack:
	mov x8, 132
	svc 0
	cmn x0, 4095
	b.cc 1f
	brk 0
1:	ret

.global runtime_exitThread
.type runtime_exitThread, %function
runtime_exitThread:
	stlr wzr, [x0]
	mov x0, 0
	mov x8, 93
	svc 0
	brk 0

.global runtime_goexit
.type runtime_goexit, %function
runtime_goexit:
	brk 0

.global runtime_asmcgocall
.type runtime_asmcgocall, %function
runtime_asmcgocall:
	mov x9, x0
	mov x0, x1
	stp x29, x30, [sp, -16]!
	mov x29, sp
	blr x9
	ldp x29, x30, [sp], 16
	ret

.macro indirect_cgo name, pointer
.global \name
.type \name, %function
\name:
	adrp x9, \pointer
	ldr x9, [x9, #:lo12:\pointer]
	cbz x9, 1f
	stp x29, x30, [sp, -16]!
	mov x29, sp
	blr x9
	ldp x29, x30, [sp], 16
	ret
1:	brk 0
.endm

indirect_cgo runtime_callCgoMmap, _cgo_mmap
indirect_cgo runtime_callCgoMunmap, _cgo_munmap
indirect_cgo runtime_callCgoSigaction, _cgo_sigaction

.macro unsupported_async name
.global \name
.type \name, %function
\name:
	brk 0
.endm

unsupported_async runtime_asyncPreempt
unsupported_async runtime_cgoSigtramp
unsupported_async runtime_sigtramp

.global runtime_clone
.type runtime_clone, %function
runtime_clone:
	// Keep the new M, G, and entry point on the child stack. The clone
	// syscall returns with SP set to x1 in the child, where argument
	// registers are otherwise not useful as persistent storage.
	str x2, [x1, -8]
	str x3, [x1, -16]
	str x4, [x1, -24]
	mov x5, 1234
	str x5, [x1, -32]
	mov x8, 220
	svc 0
	cbz x0, 1f
	ret
1:	ldr x5, [sp, -32]
	mov x6, 1234
	cmp x5, x6
	b.ne 3f
	ldr x12, [sp, -24]
	ldr x28, [sp, -16]
	ldr x10, [sp, -8]
	cbz x10, 2f
	cbz x28, 2f
	str x10, [x28, 48]
2:	blr x12
	mov x0, 111
	mov x8, 93
	svc 0
3:	mov x0, 0
	str xzr, [x0]

.data
.balign 8
.weak _cgo_mmap
_cgo_mmap:
	.xword 0
.weak _cgo_munmap
_cgo_munmap:
	.xword 0
.weak _cgo_sigaction
_cgo_sigaction:
	.xword 0

.text
.weak runtime_throw
branch_alias internal_runtime_cgroup_throw, runtime_throw

.global internal_cpu_getMIDR
.type internal_cpu_getMIDR, %function
internal_cpu_getMIDR:
	mrs x0, midr_el1
	ret

.global runtime_checkASM
.type runtime_checkASM, %function
runtime_checkASM:
	mov w0, 1
	ret

.global runtime_open
.type runtime_open, %function
runtime_open:
	mov w3, w2
	mov w2, w1
	mov x1, x0
	mov x0, -100
	mov x8, 56
	svc 0
	cmn x0, 4095
	b.lo 1f
	mov w0, -1
1:	ret

linux_syscall runtime_closefd, 57
linux_syscall runtime_mincore, 232
linux_syscall runtime_sched_getaffinity, 123
linux_syscall runtime_vgetrandom1, 278
