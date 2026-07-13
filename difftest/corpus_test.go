package difftest

// corpus is a set of QBE-compatible IL programs with C drivers that print a
// result. Each is compiled and run by cg12 at both optimization levels (and by
// QBE when available); every path must produce Want.
var corpus = []Case{
	{
		Name: "add",
		IL: `
export function w $add(w %a, w %b) {
@start
	%r =w add %a, %b
	ret %r
}
`,
		Main: `#include <stdio.h>
extern int add(int,int);
int main(void){ printf("%d\n", add(17,25)); return 0; }`,
		Want: "42\n",
	},
	{
		Name: "factorial",
		IL: `
export function l $fact(w %n) {
@start
	%nl =l extsw %n
	jmp @loop
@loop
	%i =l phi @start 1, @body %i1
	%acc =l phi @start 1, @body %acc1
	%done =w csgtl %i, %nl
	jnz %done, @exit, @body
@body
	%acc1 =l mul %acc, %i
	%i1 =l add %i, 1
	jmp @loop
@exit
	ret %acc
}
`,
		Main: `#include <stdio.h>
extern long long fact(int);
int main(void){ printf("%lld\n", fact(10)); return 0; }`,
		Want: "3628800\n",
	},
	{
		Name: "fib",
		IL: `
export function w $fib(w %n) {
@start
	%c =w csltw %n, 2
	jnz %c, @base, @rec
@base
	ret %n
@rec
	%n1 =w sub %n, 1
	%a =w call $fib(w %n1)
	%n2 =w sub %n, 2
	%b =w call $fib(w %n2)
	%r =w add %a, %b
	ret %r
}
`,
		Main: `#include <stdio.h>
extern int fib(int);
int main(void){ printf("%d\n", fib(15)); return 0; }`,
		Want: "610\n",
	},
	{
		Name: "gcd",
		IL: `
export function w $gcd(w %a, w %b) {
@start
	jmp @loop
@loop
	%x =w phi @start %a, @body %y
	%y =w phi @start %b, @body %r
	%z =w ceqw %y, 0
	jnz %z, @exit, @body
@body
	%r =w rem %x, %y
	jmp @loop
@exit
	ret %x
}
`,
		Main: `#include <stdio.h>
extern int gcd(int,int);
int main(void){ printf("%d\n", gcd(1071,462)); return 0; }`,
		Want: "21\n",
	},
	{
		Name: "dsum",
		IL: `
export function d $dsum(l %p, w %n) {
@start
	jmp @loop
@loop
	%i =w phi @start 0, @body %i1
	%s =d phi @start d_0, @body %s1
	%done =w csgew %i, %n
	jnz %done, @exit, @body
@body
	%off =l extsw %i
	%off8 =l mul %off, 8
	%addr =l add %p, %off8
	%v =d loadd %addr
	%s1 =d add %s, %v
	%i1 =w add %i, 1
	jmp @loop
@exit
	ret %s
}
`,
		Main: `#include <stdio.h>
extern double dsum(double*, int);
int main(void){ double a[4]={1.5,2.5,3.0,4.0}; printf("%g\n", dsum(a,4)); return 0; }`,
		Want: "11\n",
	},
	{
		Name: "darith",
		IL: `
export function d $darith(d %a, d %b, d %c) {
@start
	%t =d sub %a, %b
	%r =d div %t, %c
	ret %r
}
`,
		Main: `#include <stdio.h>
extern double darith(double,double,double);
int main(void){ printf("%g\n", darith(10.0,4.0,3.0)); return 0; }`,
		Want: "2\n",
	},
	{
		Name: "conversions",
		IL: `
export function d $conv(w %n, s %f) {
@start
	%nl =l extsw %n
	%nd =d sltof %nl
	%fi =w stosi %f
	%fil =l extsw %fi
	%fd =d sltof %fil
	%r =d add %nd, %fd
	ret %r
}
`,
		Main: `#include <stdio.h>
extern double conv(int, float);
int main(void){ printf("%g\n", conv(3, 2.9f)); return 0; }`,
		Want: "5\n",
	},
	{
		Name: "dtosi-alias",
		IL: `
export function l $toint(d %x) {
@start
	%r =l dtosi %x
	ret %r
}
`,
		Main: `#include <stdio.h>
extern long long toint(double);
int main(void){ printf("%lld\n", toint(3.9)); return 0; }`,
		Want: "3\n",
	},
	{
		Name: "memory",
		IL: `
export function w $mem(w %x) {
@start
	%p =l alloc4 4
	%x3 =w mul %x, 3
	storew %x3, %p
	%r =w loaduw %p
	ret %r
}
`,
		Main: `#include <stdio.h>
extern int mem(int);
int main(void){ printf("%d\n", mem(14)); return 0; }`,
		Want: "42\n",
	},
	{
		Name: "compares",
		IL: `
export function w $cmps(w %a, w %b) {
@start
	%lt =w csltw %a, %b
	%le =w cslew %a, %b
	%gt =w csgtw %a, %b
	%ge =w csgew %a, %b
	%eq =w ceqw %a, %b
	%ne =w cnew %a, %b
	%s1 =w add %lt, %le
	%s2 =w add %gt, %ge
	%s3 =w add %eq, %ne
	%s4 =w add %s1, %s2
	%r =w add %s4, %s3
	ret %r
}
`,
		Main: `#include <stdio.h>
extern int cmps(int,int);
int main(void){ printf("%d\n", cmps(3,5)); return 0; }`,
		Want: "3\n",
	},
	{
		Name: "bitops",
		IL: `
export function w $bits(w %a, w %b) {
@start
	%an =w and %a, %b
	%orr =w or %a, %b
	%xr =w xor %a, %b
	%sl =w shl %a, 2
	%sr =w shr %a, 1
	%sa =w sar %b, 1
	%s1 =w add %an, %orr
	%s2 =w add %xr, %sl
	%s3 =w add %sr, %sa
	%s4 =w add %s1, %s2
	%r =w add %s4, %s3
	ret %r
}
`,
		Main: `#include <stdio.h>
extern int bits(int,int);
int main(void){ printf("%d\n", bits(12,10)); return 0; }`,
		Want: "87\n",
	},
	{
		Name: "unsigned-div",
		IL: `
export function w $u(w %a, w %b) {
@start
	%q =w udiv %a, %b
	ret %q
}
`,
		Main: `#include <stdio.h>
extern unsigned u(unsigned,unsigned);
int main(void){ printf("%u\n", u(0xfffffff0u, 16)); return 0; }`,
		Want: "268435455\n",
	},
}
