package cc_test

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/stretchr/testify/require"
)

// cg12 builds an aggregate's type from its members' types and then assumes its
// own layout rules land where C did. For a bitfield they do not: a bitfield's
// type is its whole storage type, so `unsigned a:1, b:1, c:1, d:1, e:1` becomes
// five four-byte fields -- twenty bytes for a struct C makes four.
//
// The backends classify a by-value argument from that type, so the struct
// arrives at the callee with its fields somewhere else entirely. It looks fine
// within one translation unit, because both sides agree on the same wrong
// layout; it corrupts the moment it meets code someone else compiled. An error
// is the honest answer until ir.AggType can express a bitfield.
func TestBitfieldByValueIsRefused(t *testing.T) {
	for _, src := range []string{
		`struct W { unsigned a:1, b:1, c:1, d:1, e:1; };
		 int use(struct W w){ return w.a; }`, // by-value parameter
		`struct W { unsigned a:1, b:1; };
		 extern int peer(struct W);
		 int use(void){ struct W w = {1,0}; return peer(w); }`, // by-value argument
		`struct W { unsigned a:1, b:1; };
		 struct W make(void){ struct W w = {1,0}; return w; }`, // by-value return
	} {
		_, err := cc.Compile("w.c", src)
		require.Error(t, err, "a bitfield struct crossing the ABI must not compile silently")
		require.Contains(t, err.Error(), "by value")
		require.Contains(t, err.Error(), "bitfield")
	}
}

// Bitfields themselves are fine: member access uses the type checker's offsets,
// and the storage its size. Only crossing the ABI by value consults the
// aggregate type, so only that is refused.
func TestBitfieldsWorkWhenTheyDoNotCrossTheABI(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct W { unsigned a:1, b:3, c:4; };
static struct W g;
int main(void){
  struct W w;
  w.a = 1; w.b = 5; w.c = 9;
  g = w;                        /* whole-struct copy: not an ABI crossing */
  struct W *p = &g;             /* by pointer: likewise */
  printf("%u %u %u %zu\n", p->a, p->b, p->c, sizeof(struct W));
  return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "1 5 9 4\n", out, "bitfields pack as C says, and sizeof agrees")
}

// A struct whose layout cg12 does reproduce passes by value as before.
func TestOrdinaryStructsStillPassByValue(t *testing.T) {
	out, code := compileAndRun(t, `
#include <stdio.h>
struct P { char c; int i; };
struct D { double x, y; };
static int usep(struct P p){ return p.c + p.i; }
static double used(struct D d){ return d.x + d.y; }
int main(void){
  struct P p = {2, 40};
  struct D d = {1.5, 2.5};
  printf("%d %g\n", usep(p), used(d));
  return 0;
}`)
	require.Equal(t, 0, code)
	require.Equal(t, "42 4\n", out)
}
