/* A timing workload for a cg12-compiled SQLite.
 *
 * This is the performance counterpart of testdata/behaviour/sqlite_driver.c and
 * it exists because that one cannot be used for timing: it runs in about ten
 * milliseconds, which is the same order as process startup, so any number taken
 * from it is mostly noise. What is measured here instead runs for seconds, in
 * phases, each timed from inside the process so that fork, exec, dynamic linking
 * and teardown fall outside every measurement.
 *
 * Phases rather than one total, because a single number cannot say WHAT got
 * slower. They are chosen to lean on different parts of the engine, and so on
 * different parts of the compiler:
 *
 *   insert     b-tree descent, page splits, text binding -- the write path
 *   index      the sorter and the comparison callbacks
 *   integrity  a full structural walk of every page
 *   point      short indexed lookups: call overhead and prologue cost dominate
 *   range      cursor movement and the ORDER BY sorter over many rows
 *   group      aggregation, with the grouping key forcing a sort
 *   string     LIKE, replace and substr: byte loops over text
 *   expr       arithmetic in the VDBE interpreter, whose one enormous switch is
 *              the shape that stresses block layout and jump threading
 *
 * Everything accumulates into one checksum, printed at the end. A benchmark
 * whose arms computed different things is worthless, so the harness compares
 * checksums across compilers before it compares times.
 *
 * The first argument scales the work (default 1). Timings are printed as
 * "phase <name> <milliseconds>" so a harness can parse them without a parser.
 *
 * Every index expression here is long long on purpose. The obvious int version
 * of `(i * 31337) % 100000` overflows at i = 68529, and signed overflow is
 * undefined -- which entitles the compiler to assume the loop never gets that
 * far and drop the exit test. The first draft of this file did exactly that, and
 * an unbounded insert loop into an in-memory database took the machine down.
 * gcc says so out loud (-Waggressive-loop-optimizations); the harness builds
 * with -Werror so nobody has to notice it by hand.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "sqlite3.h"

static sqlite3 *db;
static long long checksum;

static double now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return ts.tv_sec * 1000.0 + ts.tv_nsec / 1e6;
}

static double phase_start;
static void begin(void) { phase_start = now_ms(); }
static void end(const char *name) {
  printf("phase %s %.1f\n", name, now_ms() - phase_start);
  fflush(stdout); /* so a harness, or a human watching, sees progress */
}

static void must(int rc, const char *what) {
  if (rc != SQLITE_OK && rc != SQLITE_DONE && rc != SQLITE_ROW) {
    printf("FAIL %s: %s\n", what, sqlite3_errmsg(db));
    fflush(stdout);
    _Exit(1);
  }
}

/* run_scalar executes sql and folds every integer column of every row into the
   checksum. Most phases are one of these. */
static void run_scalar(const char *sql, const char *what) {
  sqlite3_stmt *st;
  must(sqlite3_prepare_v2(db, sql, -1, &st, 0), what);
  while (sqlite3_step(st) == SQLITE_ROW) {
    for (int c = 0, n = sqlite3_column_count(st); c < n; c++) {
      checksum += sqlite3_column_int64(st, c);
    }
  }
  sqlite3_finalize(st);
}

int main(int argc, char **argv) {
  int scale = argc > 1 ? atoi(argv[1]) : 1;
  if (scale < 1) scale = 1;

  /* Sized so that no phase is short enough for timer and scheduler noise to
     matter -- the shortest is over a hundred milliseconds -- while the whole run
     stays a few seconds. The row count is kept modest on purpose: a table large
     enough to fall out of cache would measure the memory system rather than the
     generated code, which is not what this is for. */
  const long long rows = 500000LL * scale;
  const long long points = 800000LL * scale;
  const long long ranges = 500LL * scale;
  const int groupPasses = 5 * scale;
  const int stringPasses = 16 * scale;
  const int exprPasses = 12 * scale;

  sqlite3_stmt *st;
  char buf[64];

  must(sqlite3_open(":memory:", &db), "open");
  must(sqlite3_exec(db, "PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF;", 0, 0, 0), "pragma");
  must(sqlite3_exec(db, "PRAGMA cache_size=-65536;", 0, 0, 0), "cache");
  must(sqlite3_exec(db, "CREATE TABLE t(a INTEGER, b INTEGER, c TEXT)", 0, 0, 0), "create");

  begin();
  must(sqlite3_exec(db, "BEGIN", 0, 0, 0), "begin");
  must(sqlite3_prepare_v2(db, "INSERT INTO t VALUES(?,?,?)", -1, &st, 0), "prepare insert");
  for (long long i = 0; i < rows; i++) {
    snprintf(buf, sizeof buf, "row-%lld-%lld", i, (i * 7919) % 1000);
    sqlite3_bind_int64(st, 1, i);
    sqlite3_bind_int64(st, 2, (i * 31337) % 100000);
    sqlite3_bind_text(st, 3, buf, -1, SQLITE_TRANSIENT);
    must(sqlite3_step(st), "insert");
    sqlite3_reset(st);
  }
  sqlite3_finalize(st);
  must(sqlite3_exec(db, "COMMIT", 0, 0, 0), "commit");
  end("insert");

  begin();
  must(sqlite3_exec(db, "CREATE INDEX i1 ON t(b)", 0, 0, 0), "create index");
  must(sqlite3_exec(db, "CREATE INDEX i2 ON t(c)", 0, 0, 0), "create index c");
  end("index");

  begin();
  run_scalar("PRAGMA integrity_check", "integrity_check");
  end("integrity");

  /* Point lookups through the index. Each does very little work, so what this
     measures is the fixed cost of getting in and out of it -- prologues, call
     sequences, the register allocator's spill traffic on hot short paths. */
  begin();
  must(sqlite3_prepare_v2(db, "SELECT a FROM t WHERE b=?", -1, &st, 0), "prepare point");
  for (long long i = 0; i < points; i++) {
    sqlite3_bind_int64(st, 1, (i * 31337) % 100000);
    while (sqlite3_step(st) == SQLITE_ROW) checksum += sqlite3_column_int(st, 0);
    sqlite3_reset(st);
  }
  sqlite3_finalize(st);
  end("point");

  begin();
  must(sqlite3_prepare_v2(db,
      "SELECT a,b,c FROM t WHERE b BETWEEN ? AND ? ORDER BY b,a", -1, &st, 0), "prepare range");
  for (long long q = 0; q < ranges; q++) {
    sqlite3_bind_int64(st, 1, (q * 977) % 90000);
    sqlite3_bind_int64(st, 2, (q * 977) % 90000 + 900);
    while (sqlite3_step(st) == SQLITE_ROW) {
      checksum += sqlite3_column_int(st, 0) + sqlite3_column_int(st, 1);
      checksum += (long long)strlen((const char *)sqlite3_column_text(st, 2));
    }
    sqlite3_reset(st);
  }
  sqlite3_finalize(st);
  end("range");

  begin();
  for (int p = 0; p < groupPasses; p++) {
    run_scalar("SELECT b%997 g, count(*), sum(a), max(length(c)) "
               "FROM t GROUP BY g ORDER BY 2 DESC, 1", "group");
  }
  end("group");

  begin();
  for (int p = 0; p < stringPasses; p++) {
    run_scalar("SELECT count(*), sum(length(replace(c,'-','_'))) FROM t "
               "WHERE c LIKE '%-77-%' OR substr(c,5,2)='12'", "string");
  }
  end("string");

  /* Arithmetic with no I/O and no allocation: as close to a pure measurement of
     the interpreter loop as SQL gets. */
  begin();
  for (int p = 0; p < exprPasses; p++) {
    run_scalar("SELECT sum((a*7+b/3)%1009 + ((a|b)-(a&b))%577 - (a|b)%131 + (a&b)%67) FROM t", "expr");
  }
  end("expr");

  printf("checksum %lld\n", checksum);
  sqlite3_close(db);
  return 0;
}
