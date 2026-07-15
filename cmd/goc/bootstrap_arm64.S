.text
.global main
.type main, %function
main:
	mov x19, x0
	mov x20, x1
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
	bl runtime_check
	mov w0, w19
	mov x1, x20
	bl runtime_args
	bl runtime_osinit
	bl runtime_schedinit
	bl main_main
	mov w0, 0
	mov x8, 94
	svc 0
	brk 0
