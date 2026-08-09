/* A by-value aggregate parameter is the callee's own copy.
 *
 * AAPCS64 passes a composite larger than 16 bytes indirectly (rule C.12): the
 * caller copies it somewhere private and hands the callee a pointer to that
 * copy. cg12 handed over a pointer to the caller's own object, so a callee that
 * assigned to its parameter -- an ordinary local variable, as far as C is
 * concerned -- wrote through to the caller's variable.
 *
 * Composites of 16 bytes and under travel in registers, where the callee
 * necessarily gets a copy, and every aggregate the rubric passed by value before
 * this was that small. The bug needed a struct over 16 bytes, a callee that
 * writes to its parameter, and a caller that looks at the original afterwards.
 */
#include <stdio.h>

typedef struct { long a, b, c, d; } Big;   /* 32 bytes: passed indirectly */
typedef struct { long a, b, c; } Mid;      /* 24 bytes: passed indirectly */
typedef struct { long a, b; } Small;       /* 16 bytes: passed in registers */

void bump_big(Big x) { x.a += 100; }
void bump_mid(Mid x) { x.a += 100; }
void bump_small(Small x) { x.a += 100; }

/* A callee that both writes to its parameter and computes from it: the writes
   stay private, and the value it returns still reflects them. */
long sum_big(Big x) {
  x.a *= 10;
  return x.a + x.b + x.c + x.d;
}

/* The copy is per call, not per argument object: passing one struct twice in one
   expression must not let the two callees share storage. */
long twice(Big x, Big y) {
  x.a = 7;
  return x.a * 100 + y.a;
}

int main(void) {
  Big b = {1, 2, 3, 4};
  Mid m = {1, 2, 3};
  Small s = {1, 2};

  bump_big(b);
  bump_mid(m);
  bump_small(s);
  printf("%ld %ld %ld\n", b.a, m.a, s.a);

  printf("%ld %ld\n", sum_big(b), b.a);
  printf("%ld %ld\n", twice(b, b), b.a);

  /* And in a loop, where the caller's object is read again every iteration. */
  long acc = 0;
  for (int i = 0; i < 3; i++) {
    bump_big(b);
    acc += b.a;
  }
  printf("%ld\n", acc);
  return 0;
}
