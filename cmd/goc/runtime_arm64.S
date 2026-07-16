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

.global internal_abi_FuncPCABI0
.type internal_abi_FuncPCABI0, %function
internal_abi_FuncPCABI0:
	ldr x0, [x0, 8]
	ldr x0, [x0]
	ret

branch_alias internal_abi_FuncPCABIInternal, internal_abi_FuncPCABI0

.global runtime_getg
.type runtime_getg, %function
runtime_getg:
	mov x0, x28
	ret

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

.global runtime_gocAssemblySupportEnd
.type runtime_gocAssemblySupportEnd, %function
runtime_gocAssemblySupportEnd:
	nop
	ret
