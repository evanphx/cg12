.text
.global runtime_gocTextEnd
runtime_gocTextEnd:

.global main
.type main, %function
main:
	mov x29, 0
	adrp x28, runtime_g0
	add x28, x28, #:lo12:runtime_g0
	mov x9, sp
	mov x10, 65536
	sub x10, x9, x10
	str x10, [x28]
	str x9, [x28, 8]
	str x10, [x28, 16]
	str x10, [x28, 24]
	str x9, [x28, 56]
	str x28, [x28, 72]
	adrp x9, runtime_m0
	add x9, x9, #:lo12:runtime_m0
	str x28, [x9]
	str x9, [x28, 48]
	sub sp, sp, 16
	str w0, [sp]
	str x1, [sp, 8]
	bl runtime_check
	ldr w0, [sp]
	ldr x1, [sp, 8]
	add sp, sp, 16
	bl runtime_args
	bl runtime_osinit
	bl runtime_schedinit
	adrp x0, runtime_main_funcval
	add x0, x0, #:lo12:runtime_main_funcval
	bl runtime_newproc
	bl runtime_mstart0
	mov w0, 0
	mov x8, 94
	svc 0
	brk 0

.data
.balign 8
runtime_main_funcval:
	.xword runtime_main
