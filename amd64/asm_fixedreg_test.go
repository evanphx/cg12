package amd64_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInlineAsmFixedReg exercises x86 fixed-register operand constraints on
// amd64 (run under qemu). "=a"/"a" pin the operand to a specific register; the
// output and a matching input can share it (the syscall idiom), and "S"/"D" pin
// distinct registers. Checked both raw and optimized (where the optimizer folds
// the call arguments to constants, exercising the fixed-input materialization).
func TestInlineAsmFixedReg(t *testing.T) {
	src := `
int addfixed(int a, int b){ /* =a output shares rax with the a input; b in rdx */
	int r; __asm__("addl %2, %k0" : "=a"(r) : "a"(a), "d"(b)); return r;
}
int sub_sd(int a, int b){ /* rsi and rdi inputs, any-register output */
	int r; __asm__("movl %1, %k0\n\tsubl %2, %k0" : "=r"(r) : "S"(a), "D"(b)); return r;
}
int rw_c(int x){ /* "+c": read-write pinned to rcx */
	__asm__("addl $5, %k0" : "+c"(x)); return x;
}
int runtest(void){
	if(addfixed(20, 22) != 42) return 1;
	if(addfixed(100, -58) != 42) return 2;
	if(sub_sd(50, 8) != 42) return 3;
	if(rw_c(37) != 42) return 4;
	return 0;
}`
	require.Equal(t, 0, runAsmSrc(t, src, false))
	require.Equal(t, 0, runAsmSrc(t, src, true))
}
