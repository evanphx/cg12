/* Exercises a cg12-compiled SQLite hard enough to catch a miscompile.
 *
 * The workload is chosen for what it touches, not for coverage of the API: rows
 * enough to push the b-tree past one page per node (the shape that hid a wrong
 * cell count for as long as nothing ran the result), an index build, range
 * queries through that index, grouping and ordering, and the string functions.
 * Everything it prints is a total or a count, so a single wrong byte anywhere in
 * the engine changes the output.
 *
 * It is compiled by the host compiler in every arm of the test. Only the SQLite
 * object varies, so a difference in output is a difference in how the compiler
 * under test built SQLite, and nothing else.
 */
#include <stdio.h>
#include <string.h>
#include "sqlite3.h"

static sqlite3 *db;

static void must(int rc, const char *what) {
  if (rc != SQLITE_OK && rc != SQLITE_DONE && rc != SQLITE_ROW) {
    printf("FAIL %s: %s\n", what, sqlite3_errmsg(db));
    fflush(stdout);
    _Exit(1);
  }
}

int main(void) {
  enum { N = 20000 };
  sqlite3_stmt *st;
  char buf[64];
  long long sum = 0;
  int rows = 0, groups = 0;

  must(sqlite3_open(":memory:", &db), "open");
  must(sqlite3_exec(db, "PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF;", 0, 0, 0), "pragma");
  must(sqlite3_exec(db, "CREATE TABLE t(a INTEGER, b INTEGER, c TEXT)", 0, 0, 0), "create");

  must(sqlite3_exec(db, "BEGIN", 0, 0, 0), "begin");
  must(sqlite3_prepare_v2(db, "INSERT INTO t VALUES(?,?,?)", -1, &st, 0), "prepare insert");
  for (int i = 0; i < N; i++) {
    snprintf(buf, sizeof buf, "row-%d-%d", i, (i * 7919) % 1000);
    sqlite3_bind_int(st, 1, i);
    sqlite3_bind_int(st, 2, (i * 31337) % 100000);
    sqlite3_bind_text(st, 3, buf, -1, SQLITE_TRANSIENT);
    must(sqlite3_step(st), "insert");
    sqlite3_reset(st);
  }
  sqlite3_finalize(st);
  must(sqlite3_exec(db, "COMMIT", 0, 0, 0), "commit");

  /* An index over more than one page: the b-tree balance this used to corrupt. */
  must(sqlite3_exec(db, "CREATE INDEX i1 ON t(b)", 0, 0, 0), "create index");
  must(sqlite3_exec(db, "PRAGMA integrity_check", 0, 0, 0), "integrity_check");

  must(sqlite3_prepare_v2(db, "SELECT a,b,c FROM t WHERE b BETWEEN ? AND ? ORDER BY b,a", -1, &st, 0), "prepare range");
  for (int q = 0; q < 40; q++) {
    sqlite3_bind_int(st, 1, (q * 977) % 90000);
    sqlite3_bind_int(st, 2, (q * 977) % 90000 + 900);
    while (sqlite3_step(st) == SQLITE_ROW) {
      sum += sqlite3_column_int(st, 0) + sqlite3_column_int(st, 1);
      sum += (long long)strlen((const char *)sqlite3_column_text(st, 2));
      rows++;
    }
    sqlite3_reset(st);
  }
  sqlite3_finalize(st);

  must(sqlite3_prepare_v2(db,
      "SELECT b%997 g, count(*), sum(a), max(length(c)) FROM t GROUP BY g ORDER BY 2 DESC, 1", -1, &st, 0), "prepare group");
  while (sqlite3_step(st) == SQLITE_ROW) {
    sum += sqlite3_column_int64(st, 1) * 31 + sqlite3_column_int64(st, 3);
    groups++;
  }
  sqlite3_finalize(st);

  must(sqlite3_prepare_v2(db,
      "SELECT count(*), sum(length(replace(c,'-','_'))) FROM t WHERE c LIKE '%-77-%' OR substr(c,5,2)='12'", -1, &st, 0), "prepare string");
  while (sqlite3_step(st) == SQLITE_ROW) sum += sqlite3_column_int64(st, 0) + sqlite3_column_int64(st, 1);
  sqlite3_finalize(st);

  printf("sqlite rows=%d groups=%d checksum=%lld\n", rows, groups, sum);
  sqlite3_close(db);
  return 0;
}
