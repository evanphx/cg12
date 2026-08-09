/* Exercises a cg12-compiled QuickJS and prints what it computed.
 *
 * The sibling drivers run a database and a Lua interpreter; this one runs a
 * JavaScript engine, which is a different shape of program: JS_CallInternal
 * dispatches on a computed goto, values are 16-byte structs passed and returned
 * by value everywhere, and the collector walks the heap. Both of the miscompiles
 * this driver was written after were invisible to the other two workloads -- a
 * return-block phi built at the wrong width, which segfaulted 71 functions here,
 * and a by-value struct argument passed without its copy.
 *
 * Sizes are small on purpose: this is a correctness check that runs three times
 * per test, not the timing workload in testdata/perf/quickjs_bench.c.
 *
 * Every phase prints a number, so a build that computes the wrong thing is a
 * diff rather than a crash -- and a build that crashes says which phase it got
 * to.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "quickjs.h"

static JSRuntime *rt;
static JSContext *ctx;

/* run evaluates one program and prints the number it produced. A JS exception
   is fatal: every program here is expected to work. */
static void run(const char *name, const char *program) {
  JSValue v = JS_Eval(ctx, program, strlen(program), name, JS_EVAL_TYPE_GLOBAL);
  if (JS_IsException(v)) {
    JSValue e = JS_GetException(ctx);
    const char *msg = JS_ToCString(ctx, e);
    printf("%s: EXCEPTION %s\n", name, msg ? msg : "(no message)");
    fflush(stdout);
    exit(1);
  }
  double d;
  if (JS_ToFloat64(ctx, &d, v) < 0) {
    printf("%s: not a number\n", name);
    fflush(stdout);
    exit(1);
  }
  JS_FreeValue(ctx, v);
  printf("%s %.0f\n", name, d);
}

int main(void) {
  rt = JS_NewRuntime();
  ctx = JS_NewContext(rt);

  run("arith",
      "var a = 0, b = 0.0;"
      "for (var i = 1; i <= 20000; i++) {"
      "  a = (a + i * 7) % 1000003;"
      "  b = b + (i % 17) * 0.5;"
      "}"
      "a + Math.floor(b);");

  run("call",
      "function fib(n) { return n < 2 ? n : fib(n-1) + fib(n-2); }"
      "fib(18);");

  run("object",
      "var s = 0;"
      "for (var i = 0; i < 20000; i++) {"
      "  var o = { x: i, y: i + 1, name: 'n' };"
      "  o.z = o.x + o.y;"
      "  s += o.z + o.name.length;"
      "}"
      "s % 1000000007;");

  run("array",
      "var t = [];"
      "for (var i = 0; i < 20000; i++) t.push(i * 3);"
      "var s = 0;"
      "for (var i = 0; i < t.length; i++) s += t[i];"
      "s + t.filter(function(v) { return (v % 7) === 0; }).length;");

  run("string",
      "var parts = [];"
      "for (var i = 0; i < 5000; i++) parts.push(i + '-' + (i % 9));"
      "var joined = parts.join(',');"
      "var n = 0;"
      "for (var i = 0; i < parts.length; i += 3) n += parts[i].length;"
      "n + joined.length;");

  run("closure",
      "var acc = 0;"
      "for (var i = 0; i < 20000; i++) {"
      "  var x = i;"
      "  var f = function(y) { return x + y; };"
      "  acc += f(i % 13);"
      "}"
      "acc % 1000000007;");

  run("sort",
      "var t = [];"
      "for (var i = 0; i < 5000; i++) t.push((i * 7919) % 1000003);"
      "t.sort(function(x, y) { return y - x; });"
      "t[0] + t[t.length - 1];");

  /* Object churn, to make the collector run. */
  run("gc",
      "var keep = [], acc = 0;"
      "for (var i = 0; i < 20000; i++) {"
      "  var o = { a: i, b: i + 1, c: 'n' + (i % 64) };"
      "  acc += o.a + o.c.length;"
      "  if (i % 32 === 0) keep.push(o);"
      "  if (keep.length > 100) keep = [];"
      "}"
      "acc % 1000000007;");

  /* Exceptions, regexps and JSON: three subsystems the loops above never reach. */
  run("misc",
      "var n = 0;"
      "try { null.x; } catch (e) { n += 1; }"
      "n += ('abc123def'.match(/[0-9]+/) || ['']) [0].length;"
      "n += JSON.parse(JSON.stringify({a: [1,2,3]})).a.length;"
      "n;");

  JS_FreeContext(ctx);
  JS_FreeRuntime(rt);
  return 0;
}
