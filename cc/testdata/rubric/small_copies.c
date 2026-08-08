/* memcpy, memmove and memset of a small constant size are expanded inline by
   opt.ExpandSmallCopies rather than being handed to libc. This checks that the
   expansion moves the same bytes the library call would have.

   The sizes are chosen to land on and around every chunk boundary the expansion
   uses (8, 4, 2, 1 bytes), because the bugs live in the leftovers: a size of 15
   is one eight, one four, one two and one one, and getting the tail offsets
   wrong is the obvious way to break it. 65 is over the limit and stays a call,
   so the two paths are compared against each other as well.

   Every buffer is read back byte by byte and folded into a checksum, and each
   copy is bracketed by guard bytes that must not move -- writing one byte too
   many is otherwise invisible.

   memmove is the case that matters most. It has to work when source and
   destination overlap, which the expansion only gets right by loading every
   chunk before storing any, so both directions of overlap are here. */
#include <stdio.h>
#include <string.h>

static unsigned char buf[256];

/* fill puts a known, position-dependent pattern down so a misplaced byte shows. */
static void fill(void) {
  for (int i = 0; i < (int)sizeof buf; i++) buf[i] = (unsigned char)(i * 7 + 3);
}

static unsigned long sum(void) {
  unsigned long s = 0;
  for (int i = 0; i < (int)sizeof buf; i++) s = s * 31 + buf[i];
  return s;
}

struct small { int a; char b; };
struct mid { long a, b; int c; char d[7]; };
struct big { char pad[72]; };

int main(void) {
  static const int sizes[] = {0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 63, 64, 65};

  /* copy, non-overlapping, at both aligned and deliberately odd offsets */
  for (unsigned u = 0; u < sizeof sizes / sizeof sizes[0]; u++) {
    for (int off = 0; off < 3; off++) {
      fill();
      memcpy(buf + 128 + off, buf + off, (size_t)sizes[u]);
      printf("cpy %2d/%d %lu\n", sizes[u], off, sum());
    }
  }

  /* set, zero and non-zero fill */
  for (unsigned u = 0; u < sizeof sizes / sizeof sizes[0]; u++) {
    fill();
    memset(buf + 64, 0, (size_t)sizes[u]);
    printf("set0 %2d %lu\n", sizes[u], sum());
    fill();
    memset(buf + 65, 0xA5, (size_t)sizes[u]);
    printf("setA %2d %lu\n", sizes[u], sum());
  }

  /* move, overlapping forwards and backwards -- the case the expansion has to
     load entirely before it stores anything */
  for (unsigned u = 0; u < sizeof sizes / sizeof sizes[0]; u++) {
    fill();
    memmove(buf + 100, buf + 96, (size_t)sizes[u]); /* dst above src */
    printf("mvf %2d %lu\n", sizes[u], sum());
    fill();
    memmove(buf + 96, buf + 100, (size_t)sizes[u]); /* dst below src */
    printf("mvb %2d %lu\n", sizes[u], sum());
    fill();
    memmove(buf + 96, buf + 96, (size_t)sizes[u]); /* exactly on top */
    printf("mvs %2d %lu\n", sizes[u], sum());
  }

  /* struct assignment, which the front end lowers to a copy of a constant size */
  {
    struct small s1 = {0x11223344, 0x55}, s2;
    struct mid m1 = {0x1122334455667788L, -3, 0x99aabbcc, {1, 2, 3, 4, 5, 6, 7}}, m2;
    struct big b1, b2;
    for (unsigned i = 0; i < sizeof b1.pad; i++) b1.pad[i] = (char)(i ^ 0x5a);
    s2 = s1;
    m2 = m1;
    b2 = b1;
    printf("struct %d %d %ld %ld %d %d %d %d\n", s2.a, s2.b, m2.a, m2.b, m2.c,
           m2.d[6], b2.pad[0], b2.pad[71]);
  }

  /* a guard check: a copy must not touch the byte past its end */
  {
    fill();
    unsigned char before = buf[128 + 16];
    memcpy(buf + 128, buf, 16);
    printf("guard %d %d\n", buf[128 + 16] == before, buf[128] == buf[0]);
  }
  return 0;
}
