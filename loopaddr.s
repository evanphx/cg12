main:
	stp x29, x30, [sp, #-288]!
	add x29, sp, #0
	stp x19, x20, [x29, #16]
	stp x21, x22, [x29, #32]
	stp x23, x24, [x29, #48]
	add x16, x29, #128
	str x16, [x29, #64]
	add x1, x29, #136
	add x16, x29, #160
	str x16, [x29, #72]
	add x16, x29, #184
	str x16, [x29, #80]
	add x16, x29, #192
	str x16, [x29, #96]
	add x23, x29, #200
	add x24, x29, #208
	add x16, x29, #216
	str x16, [x29, #112]
	add x16, x29, #240
	str x16, [x29, #120]
	add x16, x29, #264
	str x16, [x29, #104]
	str xzr, [x1]
	str xzr, [x1, #8]
	str xzr, [x1, #16]
	ldr x0, [x29, #72]
	movz x2, #0x18
	bl goc_memcpy
	ldr x16, [x29, #72]
	ldr x17, [x29, #64]
	str x16, [x17]
	ldr x17, [x29, #80]
	str xzr, [x17]
	ldr x17, [x29, #80]
	ldr x9, [x17]
	cmp x9, #4
	b.lt .+108
	ldr x17, [x29, #64]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x0
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x12, [x9]
	ldr x17, [x29, #64]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x1
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x9, [x9]
	cmp x12, x9
	b.eq .+348
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_DISTINCT_703fd7755ce08e59
	add x1, x1, #:lo12:_goc_cstring_DISTINCT_703fd7755ce08e59
	bl printf
	b .+344
	ldr x17, [x29, #64]
	ldr x9, [x17]
	ldr x16, [x9]
	str x16, [x29, #88]
	ldr x20, [x9, #8]
	ldr x10, [x9, #16]
	add x19, x20, #1
	cmp x19, x10
	b.hi .+148
	ldr x16, [x29, #88]
	ldr x17, [x29, #96]
	str x16, [x17]
	add x17, x29, #200
	str x19, [x17]
	add x17, x29, #208
	str x10, [x17]
	ldr x17, [x29, #96]
	ldr x12, [x17]
	add x17, x29, #200
	ldr x11, [x17]
	add x17, x29, #208
	ldr x10, [x17]
	movz x17, #0x8
	madd x9, x20, x17, x12
	ldr x16, [x29, #80]
	str x16, [x9]
	ldr x17, [x29, #112]
	str x12, [x17]
	ldr x17, [x29, #112]
	str x11, [x17, #8]
	ldr x17, [x29, #112]
	str x10, [x17, #16]
	ldr x0, [x29, #120]
	ldr x1, [x29, #112]
	movz x2, #0x18
	bl goc_memcpy
	ldr x16, [x29, #120]
	ldr x17, [x29, #64]
	str x16, [x17]
	ldr x17, [x29, #80]
	ldr x9, [x17]
	add x9, x9, #1
	ldr x17, [x29, #80]
	str x9, [x17]
	b .-296
	movz x17, #0x2
	mul x21, x10, x17
	cmp x19, x21
	b.hi .+8
	b .+8
	mov x21, x19
	movz x17, #0x8
	mul x1, x21, x17
	movz x0, #0x1
	bl calloc
	mov x22, x0
	movz x17, #0x8
	mul x2, x20, x17
	mov x0, x22
	ldr x1, [x29, #88]
	bl goc_memcpy
	ldr x17, [x29, #104]
	str x22, [x17]
	ldr x17, [x29, #104]
	str x19, [x17, #8]
	ldr x17, [x29, #104]
	str x21, [x17, #16]
	ldr x17, [x29, #104]
	ldr x11, [x17]
	ldr x17, [x29, #104]
	ldr x10, [x17, #8]
	ldr x17, [x29, #104]
	ldr x9, [x17, #16]
	ldr x17, [x29, #96]
	str x11, [x17]
	add x17, x29, #200
	str x10, [x17]
	add x17, x29, #208
	str x9, [x17]
	b .-252
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_SHARED_4a65469ebec0ec2b
	add x1, x1, #:lo12:_goc_cstring_SHARED_4a65469ebec0ec2b
	bl printf
	ldr x17, [x29, #64]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x0
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x9, [x9]
	ldr x1, [x9]
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_869f1dfb999a452f
	add x1, x1, #:lo12:_goc_cstring_869f1dfb999a452f
	bl printf
	ldr x17, [x29, #64]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x1
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x9, [x9]
	ldr x1, [x9]
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_869f1dfb999a452f
	add x1, x1, #:lo12:_goc_cstring_869f1dfb999a452f
	bl printf
	ldr x17, [x29, #64]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x2
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x9, [x9]
	ldr x1, [x9]
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_869f1dfb999a452f
	add x1, x1, #:lo12:_goc_cstring_869f1dfb999a452f
	bl printf
	ldr x17, [x29, #64]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x3
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x9, [x9]
	ldr x1, [x9]
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_102b51b9765a56a3
	add x1, x1, #:lo12:_goc_cstring_102b51b9765a56a3
	bl printf
	movz w0, #0x0
	ldp x19, x20, [x29, #16]
	ldp x21, x22, [x29, #32]
	ldp x23, x24, [x29, #48]
	ldp x29, x30, [sp], #288
	ret
goc_memcpy:
	movz x10, #0x0
	cmp x10, x2
	b.cc .+8
	ret
	ldrb w9, [x1, x10]
	strb w9, [x0, x10]
	add x10, x10, #1
	b .-24
goc_memmove:
	cmp x0, x1
	b.cc .+32
	mov x10, x2
	b .+16
	sub x10, x10, #1
	ldrb w9, [x1, x10]
	strb w9, [x0, x10]
	cbnz x10, .-12
	ret
	movz x10, #0x0
	cmp x10, x2
	b.cs .-12
	ldrb w9, [x1, x10]
	strb w9, [x0, x10]
	add x10, x10, #1
	b .-20
goc_memcmp:
	movz x12, #0x0
	cmp x12, x2
	b.cc .+12
	movz w0, #0x0
	ret
	ldrb w11, [x0, x12]
	ldrb w10, [x1, x12]
	cmp w11, w10
	b.ne .+12
	add x12, x12, #1
	b .-36
	sub w0, w11, w10
	ret
goc_memset:
	movz x10, #0x0
	cmp x10, x2
	b.cc .+8
	ret
	strb w1, [x0, x10]
	add x10, x10, #1
	b .-20
