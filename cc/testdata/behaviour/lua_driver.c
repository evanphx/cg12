/* Exercises a cg12-compiled Lua by running a program through it.
 *
 * Lua earns its place next to SQLite by being the input whose output can be
 * checked by running it: an interpreter that miscompiles almost anywhere gives
 * a wrong answer rather than a wrong byte. The script leans on the parts that
 * are hardest to get right -- closures over loop variables, metatables with
 * operator overloading, coroutines, string pattern matching, table sorting with
 * a comparator, integer and float arithmetic, and error handling through pcall
 * -- and reduces all of it to one line of totals.
 *
 * The script is embedded rather than loaded from a file so the test has nothing
 * to find at run time, and the driver is compiled by the host compiler in every
 * arm, so only the Lua object varies.
 */
#include <stdio.h>
#include "lua.h"
#include "lauxlib.h"
#include "lualib.h"

static const char *program =
  "local acc = 0\n"
  "local function fib(n) return n < 2 and n or fib(n-1) + fib(n-2) end\n"
  "acc = acc + fib(20)\n"
  /* closures capturing the loop variable */
  "local fns = {}\n"
  "for i = 1, 50 do fns[i] = function(k) return i * k end end\n"
  "for i = 1, 50 do acc = acc + fns[i](i) end\n"
  /* metatables and operator overloading */
  "local V = {}; V.__index = V\n"
  "function V.new(x,y) return setmetatable({x=x,y=y}, V) end\n"
  "V.__add = function(a,b) return V.new(a.x+b.x, a.y+b.y) end\n"
  "V.__eq  = function(a,b) return a.x==b.x and a.y==b.y end\n"
  "local v = V.new(0,0)\n"
  "for i = 1, 100 do v = v + V.new(i, -i) end\n"
  "acc = acc + v.x - v.y + (v == V.new(5050,-5050) and 1 or 0)\n"
  /* coroutines */
  "local co = coroutine.create(function(a) for _ = 1, 20 do a = coroutine.yield(a + 3) end return a end)\n"
  "local _, r = coroutine.resume(co, 1)\n"
  "for _ = 1, 19 do _, r = coroutine.resume(co, r) end\n"
  "acc = acc + r\n"
  /* strings, patterns, sorting with a comparator */
  "local words = {}\n"
  "for i = 1, 400 do words[i] = string.format('w%03d-%s', (i*37)%400, string.rep('x', i%7)) end\n"
  "table.sort(words, function(a,b) return a > b end)\n"
  "acc = acc + #words[1] + #words[#words]\n"
  "local hits = 0\n"
  "for _, w in ipairs(words) do if w:match('^w1%d%d') then hits = hits + 1 end end\n"
  "acc = acc + hits\n"
  "local s = table.concat(words, ','):gsub('x', 'y')\n"
  "acc = acc + #s\n"
  /* integer/float split, and error handling */
  "acc = acc + math.floor(2^31 % 1000) + math.tointeger(4.0)\n"
  "local ok, err = pcall(function() error({code=99}) end)\n"
  "acc = acc + (ok and 0 or err.code)\n"
  "collectgarbage()\n"
  "acc = acc + math.floor(collectgarbage('count')) % 2\n"
  "print(string.format('lua checksum=%d', acc))\n";

int main(void) {
  lua_State *L = luaL_newstate();
  if (L == NULL) { printf("FAIL newstate\n"); return 1; }
  luaL_openlibs(L);
  if (luaL_dostring(L, program) != LUA_OK) {
    printf("FAIL %s\n", lua_tostring(L, -1));
    lua_close(L);
    return 1;
  }
  lua_close(L);
  return 0;
}
