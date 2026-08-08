/* The value of ++x, x++ and x op= y on a narrow lvalue is what was stored, not
   what the arithmetic produced. C computes in int and stores back narrow, so
   for `unsigned char x = 255`, ++x is 0 -- not the 256 the addition made.

   This is worth a program of its own because it hides: assigning the result to
   another narrow object narrows it again, so only a consumer that takes the
   value as it stands can see the difference. SQLite has one. Its b-tree page
   header carries a two-byte cell count incremented by hand, low byte first,
   with the carry conditional on the wrap:

       if( (++data[hdr+4])==0 ) data[hdr+3]++;

   With the un-narrowed 256 that comparison never holds, no page with more than
   255 cells records its count correctly, and the database is quietly corrupt. */
#include <stdio.h>

static unsigned char page[2];

int main(void) {
  /* the SQLite shape: a hand-rolled two-byte counter */
  page[0] = 0;
  page[1] = 254;
  for (int i = 0; i < 4; i++) {
    if ((++page[1]) == 0) page[0]++;
  }
  printf("carry hi=%d lo=%d\n", page[0], page[1]);

  unsigned char u = 255;
  printf("preinc  %d %d\n", (++u) == 0, u);
  unsigned char v = 255;
  printf("postinc %d %d\n", (v++) == 255, v);
  unsigned char w = 255;
  printf("pluseq  %d %d\n", (w += 1) == 0, w);
  unsigned char d = 0;
  printf("predec  %d %d\n", (--d) == 255, d);

  unsigned short s = 65535;
  printf("ushort  %d %d\n", (++s) == 0, s);

  signed char c = 127;
  printf("schar   %d %d\n", (++c) == -128, c);

  /* the narrowed value must survive being used, not just compared */
  unsigned char n = 200;
  int wide = (n += 100);
  printf("towide  %d\n", wide);
  return 0;
}
