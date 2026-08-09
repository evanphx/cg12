/* A timing workload for a cg12-compiled QuickJS.
 *
 * The third of the three interpreters cg12's optimiser heuristics are judged
 * against, and the one several of them were originally tuned for. SQLite's VDBE
 * dispatches on a switch and Lua's on a switch too; QuickJS's JS_CallInternal
 * uses computed goto when the compiler supports it, which is the shape the
 * cost-model inliner (selectCostInline) actually tests for and the shape the
 * jump threader's budget was written against. Measuring only the other two would
 * leave the passes' own target unmeasured.
 *
 * That has already mattered twice. Jump threading looked like a 3.9% regression
 * on SQLite and is worth 1.4% of QuickJS's text; lifting the inliner's caller
 * ceiling looked like a 3.7% win on SQLite and is a 4.7% loss on Lua. Neither
 * would have been decided correctly from one program.
 *
 * Every phase is a JavaScript program leaning on a different part of the engine,
 * timed from inside the process so runtime and context setup fall outside the
 * measurement:
 *
 *   arith     integer and float fast paths in the interpreter loop
 *   call      function call and return, including a recursive chain
 *   object    property lookup and creation, hidden-class transitions
 *   array     indexed access, growth, and the builtin iteration helpers
 *   string    concatenation, slicing and the property-access paths on strings
 *   closure   closure creation and captured-variable access
 *   sort      Array.sort with a JS comparator: interpreter re-entry per compare
 *   gc        object churn that forces collection
 *
 * The sizes are calibrated so no phase is short enough for scheduler noise to
 * matter; the first draft ran in 1.8s with one phase at 9.9ms.
 *
 * Each phase's result folds into a checksum, so an arm that miscompiled anything
 * the phases touch is caught before any timing is compared -- the same contract
 * sqlite_bench.c and lua_bench.c carry, and the same "phase <name> <ms>" output,
 * so one harness reads all three.
 *
 * Only quickjs.c is compiled by the compiler under test. libregexp.c,
 * libunicode.c and cutils.c are built by the host: quickjs.c cannot be joined
 * with them into one translation unit (compute_stack_size is static in both it
 * and libregexp.c, with different signatures, which gcc rejects too), and the
 * interpreter -- the thing being measured -- is entirely in quickjs.c.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "quickjs.h"

static JSRuntime *rt;
static JSContext *ctx;
static long long checksum;

static double now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return ts.tv_sec * 1000.0 + ts.tv_nsec / 1e6;
}

/* run evaluates one phase and folds the number it produced into the checksum. A
   JS exception is fatal: a phase that threw is not a fast phase. */
static void run(const char *name, const char *program) {
  double start = now_ms();
  JSValue v = JS_Eval(ctx, program, strlen(program), name, JS_EVAL_TYPE_GLOBAL);
  double elapsed = now_ms() - start;
  if (JS_IsException(v)) {
    JSValue e = JS_GetException(ctx);
    const char *msg = JS_ToCString(ctx, e);
    printf("FAIL %s: %s\n", name, msg ? msg : "(no message)");
    fflush(stdout);
    _Exit(1);
  }
  double d;
  if (JS_ToFloat64(ctx, &d, v) < 0) {
    printf("FAIL %s: result is not a number\n", name);
    fflush(stdout);
    _Exit(1);
  }
  JS_FreeValue(ctx, v);
  checksum = checksum * 31 + (long long)d;
  printf("phase %s %.1f\n", name, elapsed);
  fflush(stdout);
}

int main(int argc, char **argv) {
  int scale = argc > 1 ? atoi(argv[1]) : 1;
  if (scale < 1) scale = 1;
  char program[4096];

  rt = JS_NewRuntime();
  ctx = JS_NewContext(rt);

  snprintf(program, sizeof program,
    "var a = 0, b = 0.0;\n"
    "for (var i = 1; i <= %d; i++) {\n"
    "  a = (a + i * 7) %% 1000003;\n"
    "  b = b + (i %% 17) * 0.5;\n"
    "  if (a > 500000) a -= 250000;\n"
    "}\n"
    "a + Math.floor(b);\n", 6400000 * scale);
  run("arith", program);

  snprintf(program, sizeof program,
    "function fib(n) { return n < 2 ? n : fib(n-1) + fib(n-2); }\n"
    "var acc = 0;\n"
    "for (var k = 0; k < %d; k++) acc += fib(20);\n"
    "acc;\n", 1000 * scale);
  run("call", program);

  snprintf(program, sizeof program,
    "var s = 0;\n"
    "for (var i = 0; i < %d; i++) {\n"
    "  var o = { x: i, y: i + 1, name: 'n' };\n"
    "  o.z = o.x + o.y;\n"
    "  s += o.z + o.name.length;\n"
    "}\n"
    "s %% 1000000007;\n", 2000000 * scale);
  run("object", program);

  snprintf(program, sizeof program,
    "var t = [];\n"
    "for (var i = 0; i < %d; i++) t.push(i * 3);\n"
    "var s = 0;\n"
    "for (var i = 0; i < t.length; i++) s += t[i];\n"
    "s = s + t.filter(function(v) { return (v %% 7) === 0; }).length;\n"
    "s %% 1000000007;\n", 3400000 * scale);
  run("array", program);

  snprintf(program, sizeof program,
    "var n = 0, parts = [];\n"
    "for (var i = 0; i < %d; i++) parts.push(i + '-' + (i %% 9));\n"
    "var joined = parts.join(',');\n"
    "for (var i = 0; i < parts.length; i += 3) n += parts[i].length;\n"
    "n + joined.length;\n", 2000000 * scale);
  run("string", program);

  snprintf(program, sizeof program,
    "var acc = 0;\n"
    "for (var i = 0; i < %d; i++) {\n"
    "  var x = i;\n"
    "  var f = function(y) { return x + y; };\n"
    "  acc += f(i %% 13);\n"
    "}\n"
    "acc %% 1000000007;\n", 2700000 * scale);
  run("closure", program);

  /* A JS comparator re-enters the interpreter once per comparison, which is a
     different cost from sorting in C. */
  snprintf(program, sizeof program,
    "var t = [];\n"
    "for (var i = 0; i < %d; i++) t.push((i * 7919) %% 1000003);\n"
    "t.sort(function(x, y) { return y - x; });\n"
    "t[0] + t[t.length - 1];\n", 900000 * scale);
  run("sort", program);

  snprintf(program, sizeof program,
    "var keep = [], acc = 0;\n"
    "for (var i = 0; i < %d; i++) {\n"
    "  var o = { a: i, b: i + 1, c: 'n' + (i %% 64) };\n"
    "  acc += o.a + o.c.length;\n"
    "  if (i %% 32 === 0) keep.push(o);\n"
    "  if (keep.length > 500) keep = [];\n"
    "}\n"
    "acc %% 1000000007;\n", 1800000 * scale);
  run("gc", program);

  printf("checksum %lld\n", checksum);
  JS_FreeContext(ctx);
  JS_FreeRuntime(rt);
  return 0;
}
