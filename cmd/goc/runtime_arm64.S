.text

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

.global runtime_memmove
.type runtime_memmove, %function
runtime_memmove:
	b memmove

.global runtime_memclrNoHeapPointers
.type runtime_memclrNoHeapPointers, %function
runtime_memclrNoHeapPointers:
	mov x2, x1
	mov w1, wzr
	b memset

.global runtime_memequal
.type runtime_memequal, %function
runtime_memequal:
	stp x29, x30, [sp, -16]!
	mov x29, sp
	bl memcmp
	cmp w0, 0
	cset w0, eq
	ldp x29, x30, [sp], 16
	ret

.global internal_bytealg_IndexByteString
.type internal_bytealg_IndexByteString, %function
internal_bytealg_IndexByteString:
	ldr x2, [x0]
	ldr x3, [x0, 8]
	mov x0, -1
	mov x4, 0
1:	cmp x4, x3
	b.hs 3f
	ldrb w5, [x2, x4]
	cmp w5, w1
	b.eq 2f
	add x4, x4, 1
	b 1b
2:	mov x0, x4
3:	ret

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

.global runtime_publicationBarrier
.type runtime_publicationBarrier, %function
runtime_publicationBarrier:
	dmb ish
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
	mov x8, 93
	svc 0
	brk 0

.data
.balign 1
.global internal_runtime_sys_DITSupported
internal_runtime_sys_DITSupported:
	.byte 0
.global internal_runtime_sys_DITEnabled
internal_runtime_sys_DITEnabled:
	.byte 0

.text
.global internal_runtime_sys_DisableDIT
.type internal_runtime_sys_DisableDIT, %function
internal_runtime_sys_DisableDIT:
	ret
.global internal_runtime_sys_EnableDIT
.type internal_runtime_sys_EnableDIT, %function
internal_runtime_sys_EnableDIT:
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
	sub sp, sp, 16
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
	add sp, sp, 16
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
	sub sp, sp, 16
	mov x0, 1
	mov x1, sp
	mov x8, 113
	svc 0
	ldp x0, x1, [sp]
	ldr x2, =1000000000
	madd x0, x0, x2, x1
	add sp, sp, 16
	ret

.global runtime_walltime
.type runtime_walltime, %function
runtime_walltime:
	mov x9, x0
	sub sp, sp, 16
	mov x0, 0
	mov x1, sp
	mov x8, 113
	svc 0
	ldp x0, x1, [sp]
	str w1, [x9]
	add sp, sp, 16
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
	ldr x9, [sp]
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
	ldr x9, [x0]
	mov sp, x9
	ldr x29, [x0, 40]
	ldr x30, [x0, 32]
	ldr x26, [x0, 24]
	ldr x9, [x0, 8]
	stp xzr, xzr, [x0]
	stp xzr, xzr, [x0, 16]
	stp xzr, xzr, [x0, 32]
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
	str x30, [x28, 64]
	str xzr, [x28, 88]
	mov x11, x28
	ldr x28, [x9]
	ldr x12, [x28, 56]
	mov sp, x12
	ldr x12, [x26]
	blr x12
	ldr x9, [x28, 48]
	ldr x28, [x9, 184]
	ldr x12, [x28, 56]
	mov sp, x12
	ldr x29, [x28, 96]
	str xzr, [x28, 56]
	str xzr, [x28, 96]
	ret
2:	ldr x9, [x26]
	br x9
3:	brk 0

.global runtime_mstart
.type runtime_mstart, %function
runtime_mstart:
	mov x29, 0
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
unsupported_async runtime_clone
unsupported_async runtime_sigtramp

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
.global internal_bytealg_Count
.type internal_bytealg_Count, %function
internal_bytealg_Count:
	ldr x2, [x0]
	ldr x3, [x0, 8]
	mov x0, 0
	mov x4, 0
1:	cmp x4, x3
	b.hs 2f
	ldrb w5, [x2, x4]
	cmp w5, w1
	cinc x0, x0, eq
	add x4, x4, 1
	b 1b
2:	ret

.global internal_bytealg_IndexByte
.type internal_bytealg_IndexByte, %function
internal_bytealg_IndexByte:
	ldr x2, [x0]
	ldr x3, [x0, 8]
	mov x0, -1
	mov x4, 0
1:	cmp x4, x3
	b.hs 3f
	ldrb w5, [x2, x4]
	cmp w5, w1
	b.eq 2f
	add x4, x4, 1
	b 1b
2:	mov x0, x4
3:	ret

.global internal_bytealg_Compare
.type internal_bytealg_Compare, %function
internal_bytealg_Compare:
	ldr x2, [x0]
	ldr x3, [x0, 8]
	ldr x4, [x1]
	ldr x5, [x1, 8]
	cmp x3, x5
	csel x6, x3, x5, lo
	mov x7, 0
1:	cmp x7, x6
	b.hs 4f
	ldrb w8, [x2, x7]
	ldrb w9, [x4, x7]
	cmp w8, w9
	b.lo 2f
	b.hi 3f
	add x7, x7, 1
	b 1b
2:	mov x0, -1
	ret
3:	mov x0, 1
	ret
4:	cmp x3, x5
	cset x0, hi
	cset x1, lo
	sub x0, x0, x1
	ret

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
