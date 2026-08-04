main:
	stp x29, x30, [sp, #-16]!
	add x29, sp, #0
	bl main_makeSlice
	mov x1, x0
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_869f1dfb999a452f
	add x1, x1, #:lo12:_goc_cstring_869f1dfb999a452f
	bl printf
	bl main_newBig
	mov x1, x0
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_869f1dfb999a452f
	add x1, x1, #:lo12:_goc_cstring_869f1dfb999a452f
	bl printf
	bl main_literalBig
	mov x1, x0
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_869f1dfb999a452f
	add x1, x1, #:lo12:_goc_cstring_869f1dfb999a452f
	bl printf
	bl main_varBig
	mov x1, x0
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_869f1dfb999a452f
	add x1, x1, #:lo12:_goc_cstring_869f1dfb999a452f
	bl printf
	bl main_makeSliceSmall
	mov x1, x0
	adrp x0, _goc_cstring_lld_2a0d4350beff9c9c
	add x0, x0, #:lo12:_goc_cstring_lld_2a0d4350beff9c9c
	bl printf
	adrp x0, _goc_cstring_s_2f3a3f9b55f5df84
	add x0, x0, #:lo12:_goc_cstring_s_2f3a3f9b55f5df84
	adrp x1, _goc_cstring_102b51b9765a56a3
	add x1, x1, #:lo12:_goc_cstring_102b51b9765a56a3
	bl printf
	movz w0, #0x0
	ldp x29, x30, [sp], #16
	ret
main_makeSliceSmall:
	stp x29, x30, [sp, #-352]!
	add x29, sp, #0
	stp x19, x20, [x29, #16]
	stp x21, x22, [x29, #32]
	str x23, [x29, #48]
	add x16, x29, #128
	str x16, [x29, #56]
	add x19, x29, #136
	add x16, x29, #200
	str x16, [x29, #64]
	add x16, x29, #224
	str x16, [x29, #72]
	add x16, x29, #248
	str x16, [x29, #88]
	add x22, x29, #256
	add x23, x29, #264
	add x16, x29, #272
	str x16, [x29, #104]
	add x16, x29, #296
	str x16, [x29, #112]
	add x16, x29, #320
	str x16, [x29, #96]
	movz w1, #0x0
	movz x2, #0x40
	add x0, x29, #136
	bl goc_memset
	add x16, x29, #136
	ldr x17, [x29, #64]
	str x16, [x17]
	ldr x17, [x29, #64]
	str xzr, [x17, #8]
	movz x16, #0x8
	ldr x17, [x29, #64]
	str x16, [x17, #16]
	ldr x0, [x29, #72]
	ldr x1, [x29, #64]
	movz x2, #0x18
	bl goc_memcpy
	ldr x16, [x29, #72]
	ldr x17, [x29, #56]
	str x16, [x17]
	ldr x17, [x29, #56]
	ldr x9, [x17]
	ldr x16, [x9]
	str x16, [x29, #80]
	ldr x20, [x9, #8]
	ldr x10, [x9, #16]
	add x19, x20, #1
	cmp x19, x10
	b.hi .+180
	ldr x16, [x29, #80]
	ldr x17, [x29, #88]
	str x16, [x17]
	add x17, x29, #256
	str x19, [x17]
	add x17, x29, #264
	str x10, [x17]
	ldr x17, [x29, #88]
	ldr x12, [x17]
	add x17, x29, #256
	ldr x11, [x17]
	add x17, x29, #264
	ldr x10, [x17]
	movz x17, #0x8
	madd x9, x20, x17, x12
	movz x16, #0x1
	str x16, [x9]
	ldr x17, [x29, #104]
	str x12, [x17]
	ldr x17, [x29, #104]
	str x11, [x17, #8]
	ldr x17, [x29, #104]
	str x10, [x17, #16]
	ldr x0, [x29, #112]
	ldr x1, [x29, #104]
	movz x2, #0x18
	bl goc_memcpy
	ldr x16, [x29, #112]
	ldr x17, [x29, #56]
	str x16, [x17]
	ldr x17, [x29, #56]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x0
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x0, [x9]
	ldp x19, x20, [x29, #16]
	ldp x21, x22, [x29, #32]
	ldr x23, [x29, #48]
	ldp x29, x30, [sp], #352
	ret
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
	mov x9, x0
	movz x17, #0x8
	mul x2, x20, x17
	str x9, [x29, #120]
	mov x0, x9
	ldr x1, [x29, #80]
	bl goc_memcpy
	ldr x9, [x29, #120]
	ldr x17, [x29, #96]
	str x9, [x17]
	ldr x17, [x29, #96]
	str x19, [x17, #8]
	ldr x17, [x29, #96]
	str x21, [x17, #16]
	ldr x17, [x29, #96]
	ldr x11, [x17]
	ldr x17, [x29, #96]
	ldr x10, [x17, #8]
	ldr x17, [x29, #96]
	ldr x9, [x17, #16]
	ldr x17, [x29, #88]
	str x11, [x17]
	add x17, x29, #256
	str x10, [x17]
	add x17, x29, #264
	str x9, [x17]
	b .-292
main_varBig:
	sub sp, sp, #390, lsl #12
	sub sp, sp, #2608
	stp x29, x30, [sp]
	add x29, sp, #0
	str x19, [x29, #16]
	add x16, x29, #32
	str x16, [x29, #24]
	add x19, x29, #40
	movz w1, #0x0
	movz x2, #0x6a00
	movk x2, #0x18, lsl #16
	add x0, x29, #40
	bl goc_memset
	add x16, x29, #40
	ldr x17, [x29, #24]
	str x16, [x17]
	ldr x17, [x29, #24]
	ldr x9, [x17]
	movz x16, #0x2
	movz x17, #0x8
	madd x9, x16, x17, x9
	movz x16, #0x7
	str x16, [x9]
	ldr x17, [x29, #24]
	ldr x9, [x17]
	movz x16, #0x2
	movz x17, #0x8
	madd x9, x16, x17, x9
	ldr x0, [x9]
	ldr x19, [x29, #16]
	ldp x29, x30, [sp]
	add sp, sp, #390, lsl #12
	add sp, sp, #2608
	ret
main_literalBig:
	sub sp, sp, #390, lsl #12
	sub sp, sp, #2608
	stp x29, x30, [sp]
	add x29, sp, #0
	add x16, x29, #32
	str x16, [x29, #16]
	add x16, x29, #40
	str x16, [x29, #24]
	ldr x0, [x29, #24]
	movz w1, #0x0
	movz x2, #0x6a00
	movk x2, #0x18, lsl #16
	bl goc_memset
	ldr x16, [x29, #24]
	ldr x17, [x29, #16]
	str x16, [x17]
	ldr x17, [x29, #16]
	ldr x9, [x17]
	movz x16, #0x1
	movz x17, #0x8
	madd x9, x16, x17, x9
	movz x16, #0x5
	str x16, [x9]
	ldr x17, [x29, #16]
	ldr x9, [x17]
	movz x16, #0x1
	movz x17, #0x8
	madd x9, x16, x17, x9
	ldr x0, [x9]
	ldp x29, x30, [sp]
	add sp, sp, #390, lsl #12
	add sp, sp, #2608
	ret
main_newBig:
	stp x29, x30, [sp, #-32]!
	add x29, sp, #0
	add x16, x29, #24
	str x16, [x29, #16]
	movz x0, #0x1
	movz x1, #0x6a00
	movk x1, #0x18, lsl #16
	bl calloc
	ldr x17, [x29, #16]
	str x0, [x17]
	ldr x17, [x29, #16]
	ldr x9, [x17]
	movz x16, #0x0
	movz x17, #0x8
	madd x9, x16, x17, x9
	movz x16, #0x3
	str x16, [x9]
	ldr x17, [x29, #16]
	ldr x9, [x17]
	movz x16, #0x0
	movz x17, #0x8
	madd x9, x16, x17, x9
	ldr x0, [x9]
	ldp x29, x30, [sp], #32
	ret
main_makeSlice:
	stp x29, x30, [sp, #-288]!
	add x29, sp, #0
	stp x19, x20, [x29, #16]
	stp x21, x22, [x29, #32]
	str x23, [x29, #48]
	add x16, x29, #128
	str x16, [x29, #56]
	add x16, x29, #136
	str x16, [x29, #64]
	add x16, x29, #160
	str x16, [x29, #72]
	add x16, x29, #184
	str x16, [x29, #88]
	add x22, x29, #192
	add x23, x29, #200
	add x16, x29, #208
	str x16, [x29, #104]
	add x16, x29, #232
	str x16, [x29, #112]
	add x16, x29, #256
	str x16, [x29, #96]
	movz x16, #0xd40
	movk x16, #0x3, lsl #16
	movz x17, #0x8
	mul x1, x16, x17
	movz x0, #0x1
	bl calloc
	ldr x17, [x29, #64]
	str x0, [x17]
	ldr x17, [x29, #64]
	str xzr, [x17, #8]
	movz x16, #0xd40
	movk x16, #0x3, lsl #16
	ldr x17, [x29, #64]
	str x16, [x17, #16]
	ldr x0, [x29, #72]
	ldr x1, [x29, #64]
	movz x2, #0x18
	bl goc_memcpy
	ldr x16, [x29, #72]
	ldr x17, [x29, #56]
	str x16, [x17]
	ldr x17, [x29, #56]
	ldr x9, [x17]
	ldr x16, [x9]
	str x16, [x29, #80]
	ldr x20, [x9, #8]
	ldr x10, [x9, #16]
	add x19, x20, #1
	cmp x19, x10
	b.hi .+180
	ldr x16, [x29, #80]
	ldr x17, [x29, #88]
	str x16, [x17]
	add x17, x29, #192
	str x19, [x17]
	add x17, x29, #200
	str x10, [x17]
	ldr x17, [x29, #88]
	ldr x12, [x17]
	add x17, x29, #192
	ldr x11, [x17]
	add x17, x29, #200
	ldr x10, [x17]
	movz x17, #0x8
	madd x9, x20, x17, x12
	movz x16, #0x1
	str x16, [x9]
	ldr x17, [x29, #104]
	str x12, [x17]
	ldr x17, [x29, #104]
	str x11, [x17, #8]
	ldr x17, [x29, #104]
	str x10, [x17, #16]
	ldr x0, [x29, #112]
	ldr x1, [x29, #104]
	movz x2, #0x18
	bl goc_memcpy
	ldr x16, [x29, #112]
	ldr x17, [x29, #56]
	str x16, [x17]
	ldr x17, [x29, #56]
	ldr x11, [x17]
	ldr x10, [x11]
	ldr x9, [x11, #8]
	ldr x9, [x11, #16]
	movz x16, #0x0
	movz x17, #0x8
	madd x9, x16, x17, x10
	ldr x0, [x9]
	ldp x19, x20, [x29, #16]
	ldp x21, x22, [x29, #32]
	ldr x23, [x29, #48]
	ldp x29, x30, [sp], #288
	ret
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
	mov x9, x0
	movz x17, #0x8
	mul x2, x20, x17
	str x9, [x29, #120]
	mov x0, x9
	ldr x1, [x29, #80]
	bl goc_memcpy
	ldr x9, [x29, #120]
	ldr x17, [x29, #96]
	str x9, [x17]
	ldr x17, [x29, #96]
	str x19, [x17, #8]
	ldr x17, [x29, #96]
	str x21, [x17, #16]
	ldr x17, [x29, #96]
	ldr x11, [x17]
	ldr x17, [x29, #96]
	ldr x10, [x17, #8]
	ldr x17, [x29, #96]
	ldr x9, [x17, #16]
	ldr x17, [x29, #88]
	str x11, [x17]
	add x17, x29, #192
	str x10, [x17]
	add x17, x29, #200
	str x9, [x17]
	b .-292
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
