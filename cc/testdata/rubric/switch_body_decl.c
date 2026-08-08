/* A declaration inside a switch body but before the first case label reserves
   storage in a block nothing can jump to: control enters a switch at a case
   label, so the block holding the body's leading declarations has no
   predecessor. The reachable cases still use that storage.

   Emitting the reservation there is invalid -- the definition does not dominate
   its uses -- and it stays invisible for exactly as long as nothing removes the
   block. Jump threading's unreachable-block prune removes it, correctly, and
   then every use reads a value nothing defines. That was a SIGSEGV compiling
   SQLite's parser (yy_reduce, which is this shape), and it only appeared under
   the optimizer, which is why this program has to run both ways.

   The union and the loop are not incidental: the union is what SQLite declares
   there, and reaching the switch repeatedly is what makes a wrong answer show
   up as a wrong total rather than a single wrong line. */
#include <stdio.h>

union Value {
  int i;
  long l;
  char *s;
};

static long dispatch(int rule, int arg) {
  switch (rule) {
    union Value scratch;      /* the unreachable declaration */
    int adjust;

    case 0:
      scratch.i = arg * 3;
      adjust = 1;
      return scratch.i + adjust;
    case 1:
      scratch.l = (long)arg * 100;
      adjust = 2;
      return scratch.l - adjust;
    case 2:
      scratch.i = arg;
      adjust = scratch.i > 10 ? 10 : 3;
      return scratch.i * adjust;
    default:
      scratch.i = -arg;
      return scratch.i;
  }
}

int main(void) {
  long total = 0;
  for (int i = 0; i < 40; i++) total += dispatch(i % 4, i);
  printf("total %ld\n", total);
  for (int r = 0; r < 4; r++) printf("rule %d -> %ld\n", r, dispatch(r, 7));
  return 0;
}
