package amd64_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestComplex exercises _Complex float/double on amd64 (run under qemu). It
// checks that the {re,im} aggregate representation is passed and returned per
// the System V x86-64 ABI (a homogeneous SSE pair), and that arithmetic, the
// a+b*I idiom, compound assignment, mixed real/complex, comparison, and
// __real__/__imag__ all compute correctly. Component access avoids libm.
func TestComplex(t *testing.T) {
	src := `
#define I (__extension__ 1.0iF)
double _Complex sq(double _Complex z){ return z*z; }
float _Complex fscale(float _Complex z, float k){ return z*k + 1.0f*I; }

int runtest(void){
	double _Complex z = 2.0 + 3.0*I;   /* idiom */
	double _Complex w = 1.0 - 1.0i;    /* literal imaginary */

	double _Complex m = z*w;           /* 5 + 1i */
	if((int)__real__ m != 5) return 1;
	if((int)__imag__ m != 1) return 2;

	double _Complex s = z + w;         /* 3 + 2i */
	if((int)__real__ s != 3) return 3;
	if((int)__imag__ s != 2) return 4;

	double _Complex d = z / w;         /* -0.5 + 2.5i */
	if(__real__ d != -0.5) return 5;
	if(__imag__ d != 2.5) return 6;

	if((int)__real__ sq(z) != -5) return 7;   /* returned by value */
	if((int)__imag__ sq(z) != 12) return 8;

	double _Complex a = z;             /* compound assignment */
	a += w;                            /* 3 + 2i */
	a *= 2.0;                          /* 6 + 4i */
	a -= 1.0*I;                        /* 6 + 3i */
	if((int)__real__ a != 6) return 9;
	if((int)__imag__ a != 3) return 10;

	if(!(z==z)) return 11;
	if(z==w) return 12;
	if(!(z!=w)) return 13;

	double _Complex rc = 5.0;          /* real -> complex */
	if((int)__real__ rc != 5 || __imag__ rc != 0) return 14;
	if((int)(double)(z*w) != 5) return 15; /* complex -> real keeps real part */

	float _Complex fz = fscale(2.0f + 1.0f*I, 3.0f); /* 6 + 4i */
	if((int)__real__ fz != 6) return 16;
	if((int)__imag__ fz != 4) return 17;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}
