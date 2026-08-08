/* The legacy __sync_* atomic builtins, and the __atomic_* ones they share a
   lowering with. Everything here is single-threaded on purpose: what is under
   test is the value each builtin computes and leaves behind, which is where the
   bugs were, not the ordering, which no single-threaded program can observe.

   The widths and signednesses are all enumerated because the sub-word cases are
   the ones that go wrong. The machine's byte and halfword atomics zero-extend
   what they load, so a signed char or short atomic hands back raw bits rather
   than the negative number they stand for; and a value argument narrower than
   the object reaches the operation unextended unless it is converted first,
   which turned `fetch_and_add(&somelong, -1)` into an addition of 0xffffffff. */
#include <stdio.h>

static signed char sc;
static unsigned char uc;
static short sh;
static unsigned short uh;
static int si;
static unsigned int ui;
static long sl;
static unsigned long ul;

int main(void) {
  /* fetch-and-op: yields the value from before, at every width */
  sc = -3;  printf("sc  fa  %d %d\n", (int)__sync_fetch_and_add(&sc, 2), (int)sc);
  uc = 200; printf("uc  fa  %d %d\n", (int)__sync_fetch_and_add(&uc, 100), (int)uc);
  sh = -300; printf("sh  fa  %d %d\n", (int)__sync_fetch_and_add(&sh, 1), (int)sh);
  uh = 65000; printf("uh  fa  %d %d\n", (int)__sync_fetch_and_add(&uh, 1000), (int)uh);
  si = -7;  printf("si  fa  %d %d\n", (int)__sync_fetch_and_add(&si, 3), si);
  ui = 4294967290u; printf("ui  fa  %u %u\n", (unsigned)__sync_fetch_and_add(&ui, 10), ui);
  sl = 1000; printf("sl  fa  %ld %ld\n", (long)__sync_fetch_and_add(&sl, -1), sl);
  ul = 1; printf("ul  fa  %lu %lu\n", (unsigned long)__sync_fetch_and_add(&ul, 0x100000000UL), ul);

  /* op-and-fetch: yields the value after, narrowed to the object's own width */
  sc = 126; printf("sc  af  %d %d\n", (int)__sync_add_and_fetch(&sc, 2), (int)sc);
  uc = 250; printf("uc  af  %d %d\n", (int)__sync_add_and_fetch(&uc, 10), (int)uc);
  sh = 32766; printf("sh  af  %d %d\n", (int)__sync_add_and_fetch(&sh, 3), (int)sh);
  sl = 1000; printf("sl  af  %ld %ld\n", (long)__sync_sub_and_fetch(&sl, 1), sl);

  /* the rest of the operations */
  si = 0x0f0f; printf("and     %d %d\n", (int)__sync_fetch_and_and(&si, 0xff), si);
  si = 0x0f0f; printf("or      %d %d\n", (int)__sync_fetch_and_or(&si, 0xf0), si);
  si = 0x0f0f; printf("xor     %d %d\n", (int)__sync_fetch_and_xor(&si, 0xff), si);
  si = 100;    printf("sub     %d %d\n", (int)__sync_sub_and_fetch(&si, 1), si);
  si = 0x0f0f; printf("andaf   %d %d\n", (int)__sync_and_and_fetch(&si, 0xff), si);
  si = 0x0f0f; printf("oraf    %d %d\n", (int)__sync_or_and_fetch(&si, 0xf0), si);
  si = 0x0f0f; printf("xoraf   %d %d\n", (int)__sync_xor_and_fetch(&si, 0xff), si);

  /* compare-and-swap, both the value form and the boolean one, and the sub-word
     cases where the expected value is negative */
  si = 5;
  printf("vcas hit  %d %d\n", (int)__sync_val_compare_and_swap(&si, 5, 9), si);
  printf("vcas miss %d %d\n", (int)__sync_val_compare_and_swap(&si, 5, 11), si);
  si = 5;
  printf("bcas hit  %d %d\n", __sync_bool_compare_and_swap(&si, 5, 9), si);
  printf("bcas miss %d %d\n", __sync_bool_compare_and_swap(&si, 5, 11), si);
  sc = -3;
  printf("scas hit  %d %d\n", (int)__sync_val_compare_and_swap(&sc, -3, -8), (int)sc);
  printf("scas miss %d %d\n", (int)__sync_val_compare_and_swap(&sc, -3, -9), (int)sc);
  sc = -3;
  printf("sbcas hit %d %d\n", __sync_bool_compare_and_swap(&sc, -3, 7), (int)sc);
  sh = -300;
  printf("hcas hit  %d %d\n", (int)__sync_val_compare_and_swap(&sh, -300, -400), (int)sh);
  ul = 0x100000000UL;
  printf("lcas hit  %lu %lu\n",
         (unsigned long)__sync_val_compare_and_swap(&ul, 0x100000000UL, 7), ul);

  /* the lock pair: an exchange that yields what was there, and a release to 0 */
  si = 3;
  printf("tas     %d %d\n", (int)__sync_lock_test_and_set(&si, 1), si);
  __sync_lock_release(&si);
  printf("release %d\n", si);
  sc = -5;
  printf("tas sc  %d %d\n", (int)__sync_lock_test_and_set(&sc, 1), (int)sc);
  __sync_lock_release(&sc);
  printf("rel sc  %d\n", (int)sc);
  ul = 99;
  printf("tas ul  %lu %lu\n", (unsigned long)__sync_lock_test_and_set(&ul, 1), ul);
  __sync_lock_release(&ul);
  printf("rel ul  %lu\n", ul);

  __sync_synchronize();

  /* the same shapes through the __atomic_* family, which shares the lowering */
  sl = 1000; printf("at fa   %ld %ld\n",
                    (long)__atomic_fetch_add(&sl, -1, __ATOMIC_SEQ_CST), sl);
  sc = -3;   printf("at ld   %d\n", (int)__atomic_load_n(&sc, __ATOMIC_SEQ_CST));
  sh = -300; printf("at ld h %d\n", (int)__atomic_load_n(&sh, __ATOMIC_SEQ_CST));
  sc = 0;    __atomic_store_n(&sc, -9, __ATOMIC_SEQ_CST);
  printf("at st   %d\n", (int)sc);
  sc = -3;   printf("at xchg %d %d\n",
                    (int)__atomic_exchange_n(&sc, 4, __ATOMIC_SEQ_CST), (int)sc);
  {
    signed char exp = -3;
    sc = -3;
    int ok = __atomic_compare_exchange_n(&sc, &exp, 6, 0,
                                         __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST);
    printf("at cas  %d %d %d\n", ok, (int)exp, (int)sc);
    ok = __atomic_compare_exchange_n(&sc, &exp, 8, 0,
                                     __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST);
    printf("at cas2 %d %d %d\n", ok, (int)exp, (int)sc);
  }
  return 0;
}
