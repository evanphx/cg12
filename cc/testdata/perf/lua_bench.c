/* A timing workload for a cg12-compiled Lua.
 *
 * The counterpart of sqlite_bench.c, and here for the reason SQLite alone was
 * not enough: cg12's optimiser has several knobs whose right setting depends on
 * what an interpreter's dispatch loop costs, and SQLite's VDBE is only one shape
 * of that. Lua's luaV_execute is a second, and quickjs_bench.c a third. A change
 * measured against one of them is a change measured against a sample of one.
 *
 * Every phase runs a Lua program written to lean on a different part of the
 * interpreter, timed from inside the process with a monotonic clock so process
 * startup and library loading fall outside the measurement:
 *
 *   arith    the integer/float fast paths in the dispatch loop, almost nothing else
 *   call     function call and return: the frame push/pop path
 *   table    hashed and array-part access, rehashing as tables grow
 *   string   interning, concatenation, and the pattern matcher
 *   closure  upvalue capture and the garbage collector's work on closures
 *   sort     table.sort with a Lua comparator: interpreter re-entry per compare
 *   gc       allocation churn that forces collection cycles
 *
 * Each phase folds its result into a checksum, so a compiler that miscompiles
 * anything the phases touch is caught before its timings are compared. The
 * harness refuses to report times when the checksums disagree.
 *
 * The sizes are calibrated so no phase is short enough for timer and scheduler
 * noise to matter -- the first draft ran the whole thing in 0.57s with one phase
 * at 2.7ms, which measures nothing -- while the total stays a few seconds.
 *
 * The first argument scales the work (default 1). Timings print as
 * "phase <name> <milliseconds>" -- the same format sqlite_bench.c uses, so one
 * harness parses both.
 */
#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include "lua.h"
#include "lauxlib.h"
#include "lualib.h"

static lua_State *L;
static long long checksum;

static double now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return ts.tv_sec * 1000.0 + ts.tv_nsec / 1e6;
}

/* run executes one phase's program, times it, and folds the number it leaves in
   the global `result` into the checksum. A Lua error is fatal: a phase that did
   not run is not a fast phase, it is a broken measurement. */
static void run(const char *name, const char *program) {
  double start = now_ms();
  if (luaL_dostring(L, program) != LUA_OK) {
    printf("FAIL %s: %s\n", name, lua_tostring(L, -1));
    fflush(stdout);
    _Exit(1);
  }
  double elapsed = now_ms() - start;
  lua_getglobal(L, "result");
  if (!lua_isnumber(L, -1)) {
    printf("FAIL %s: no numeric result\n", name);
    fflush(stdout);
    _Exit(1);
  }
  checksum = checksum * 31 + (long long)lua_tonumber(L, -1);
  lua_pop(L, 1);
  printf("phase %s %.1f\n", name, elapsed);
  fflush(stdout);
}

int main(int argc, char **argv) {
  int scale = argc > 1 ? atoi(argv[1]) : 1;
  if (scale < 1) scale = 1;
  char program[4096];

  L = luaL_newstate();
  luaL_openlibs(L);

  /* Integer and float arithmetic and comparison, and nothing else: the closest
     this gets to timing the dispatch loop by itself. */
  snprintf(program, sizeof program,
    "local a, b = 0, 0.0\n"
    "for i = 1, %d do\n"
    "  a = (a + i * 7) %% 1000003\n"
    "  b = b + (i %% 17) * 0.5\n"
    "  if a > 500000 then a = a - 250000 end\n"
    "end\n"
    "result = a + math.floor(b)\n", 26000000 * scale);
  run("arith", program);

  /* Calls and returns. Recursion keeps the frame path hot and defeats any
     attempt to keep the whole thing in registers. */
  snprintf(program, sizeof program,
    "local function fib(n) if n < 2 then return n end return fib(n-1) + fib(n-2) end\n"
    "local acc = 0\n"
    "for _ = 1, %d do acc = acc + fib(21) end\n"
    "result = acc\n", 700 * scale);
  run("call", program);

  snprintf(program, sizeof program,
    "local t, h = {}, {}\n"
    "for i = 1, %d do t[i] = i * 3; h['k' .. (i %% 5000)] = i end\n"
    "local s = 0\n"
    "for i = 1, #t do s = s + t[i] end\n"
    "for k, v in pairs(h) do s = s + v + #k end\n"
    "result = s %% 1000000007\n", 4800000 * scale);
  run("table", program);

  snprintf(program, sizeof program,
    "local parts, n = {}, 0\n"
    "for i = 1, %d do parts[#parts+1] = string.format('%%d-%%s', i, string.rep('ab', i %% 9)) end\n"
    "local joined = table.concat(parts, ',')\n"
    "for word in joined:gmatch('%%d+%%-a*b*') do n = n + #word end\n"
    "result = n + #joined\n", 840000 * scale);
  run("string", program);

  snprintf(program, sizeof program,
    "local acc = 0\n"
    "for i = 1, %d do\n"
    "  local x = i\n"
    "  local f = function(y) return x + y end\n"
    "  acc = acc + f(i %% 13)\n"
    "end\n"
    "result = acc %% 1000000007\n", 6400000 * scale);
  run("closure", program);

  /* A Lua comparator means the interpreter is re-entered once per comparison,
     which is a different cost from sorting in C. */
  snprintf(program, sizeof program,
    "local t = {}\n"
    "for i = 1, %d do t[i] = (i * 7919) %% 1000003 end\n"
    "table.sort(t, function(a, b) return a > b end)\n"
    "result = t[1] + t[#t]\n", 750000 * scale);
  run("sort", program);

  /* Churn: tables that die immediately, so the collector runs rather than the
     allocator merely growing. */
  snprintf(program, sizeof program,
    "local keep, acc = {}, 0\n"
    "for i = 1, %d do\n"
    "  local t = {i, i+1, i+2, name = 'n' .. (i %% 64)}\n"
    "  acc = acc + t[1] + #t.name\n"
    "  if i %% 32 == 0 then keep[#keep+1] = t end\n"
    "  if #keep > 500 then keep = {} end\n"
    "end\n"
    "result = acc %% 1000000007\n", 2600000 * scale);
  run("gc", program);

  printf("checksum %lld\n", checksum);
  lua_close(L);
  return 0;
}
