# cg12 test setup. Local development and CI share these targets.
#
# The suite splits into parallelizable categories:
#   test-unit         — the Go unit tests: IR, optimizer, backends, analysis, ...
#                       (fast; no external toolchain required).
#   test-goc-corpus   — the goc corpus: compiles, links, and runs real Go
#                       programs (needs a system `cc`; arm64 Linux).
#   test-goc-cmd      — the goc driver end-to-end tests (cmd/goc), minus the
#                       long runtime-capability matrix (needs `cc`; arm64).
#   test-goc-status   — the runtime-capability matrix: ~322 Go programs compiled
#                       and executed to chart runtime coverage. Long-running;
#                       shardable across parallel jobs via STATUS_SHARDS/STATUS_SHARD
#                       (needs `cc`).
#   test-goc-coverage — the same matrix compiled with runtime coverage
#                       instrumentation, producing a diffable JSON report.
#                       Unsharded by construction; see COVERAGE_REPORT below.
#   runtime-cover-diff— compare a generated report against the accepted
#                       baseline and fail on a regression.
#   test-ruby         — the Ruby/C compiler test: cg12's C frontend checked
#                       differentially against gcc (Ruby is written in C), plus
#                       regressions captured from compiling miniruby (needs gcc).
# and a heavier, out-of-tree target:
#   test-cruby        — build real CRuby with cg12 and check correctness and
#                       performance against a reference build (scripts/cruby-diff.sh).

GO ?= go

# The heavy end-to-end suites are called out explicitly; everything else is a
# plain unit test. Kept in sync by exclusion so a new package lands in test-unit
# automatically. Note cmd/goc is a heavy driver/execution suite, so it is
# excluded from the unit bucket alongside goc/difftest/cc.
GOC_CORPUS_PKGS := ./goc/...
GOC_CMD_PKGS    := ./cmd/goc/...
RUBY_PKGS       := ./difftest/... ./cc/...
UNIT_PKGS       := $(shell $(GO) list ./... | grep -vE '^github.com/evanphx/cg12/(goc|cmd/goc|difftest|cc)(/|$$)')

# The runtime-capability matrix. Runs as one test with a subtest per capability.
# STATUS_SHARDS/STATUS_SHARD partition the matrix by capability index so CI can
# run it across parallel jobs; the defaults (1 shard, index 0) run everything.
STATUS_TEST     := TestARM64RuntimeCapabilityStatus
STATUS_SHARDS   ?= 1
STATUS_SHARD    ?= 0
# Generous default; a full unsharded matrix takes many minutes.
STATUS_TIMEOUT  ?= 30m
GOC_CMD_TIMEOUT ?= 15m
# The corpus compiles and runs real Go programs and legitimately needs well over
# go test's 10-minute default, which it was silently dying on.
GOC_CORPUS_TIMEOUT ?= 40m

# The runtime coverage collection. The report describes the whole corpus, so it
# is deliberately unsharded: the test refuses -runtime-coverprofile together
# with STATUS_SHARDS, because there is no way to merge partial reports and a
# shard would publish a fraction of the corpus as though it were complete.
# COVERAGE_RUNS repeats each compiled program and merges its hits, which damps
# scheduling noise at a proportional cost in wall-clock time.
COVERAGE_REPORT   ?= runtime_coverage_linux_arm64.json
COVERAGE_RUNS     ?= 1
COVERAGE_BASELINE := cmd/goc/testdata/runtime_coverage_linux_arm64.json
# A full instrumented corpus is dominated by compilation: roughly 40 minutes of
# compiler time per run at the current matrix size.
COVERAGE_TIMEOUT  ?= 180m

.PHONY: all build test test-unit test-goc-corpus test-goc-cmd test-goc-status \
        test-goc-coverage runtime-cover-diff test-ruby test-cruby fmt vet clean

# The default local check: build, then the whole suite.
all: build test

build:
	$(GO) build ./...

# The whole suite in one command.
test:
	$(GO) test ./...

# The Go unit tests (fast; no external toolchain required).
test-unit:
	$(GO) test $(UNIT_PKGS)

# The goc corpus (arm64 Linux; needs a system `cc`).
test-goc-corpus:
	$(GO) test -timeout $(GOC_CORPUS_TIMEOUT) $(GOC_CORPUS_PKGS)

# The goc driver end-to-end tests, excluding the long capability matrix.
test-goc-cmd:
	$(GO) test -timeout $(GOC_CMD_TIMEOUT) -skip '$(STATUS_TEST)' $(GOC_CMD_PKGS)

# The runtime-capability matrix (optionally sharded via STATUS_SHARDS/STATUS_SHARD).
# The shard flags are defined by the test binary, so they go after -args (and the
# package) where `go test` passes them through unaltered.
test-goc-status:
	$(GO) test -timeout $(STATUS_TIMEOUT) -run '^$(STATUS_TEST)$$' $(GOC_CMD_PKGS) \
		-args -runtime-status-shards=$(STATUS_SHARDS) -runtime-status-shard=$(STATUS_SHARD)

# The complete runtime coverage run: every capability compiled with runtime
# instrumentation, one explicit compile/run/coverage outcome per capability,
# written to COVERAGE_REPORT as a diffable JSON report.
test-goc-coverage:
	$(GO) test -timeout $(COVERAGE_TIMEOUT) -run '^$(STATUS_TEST)$$' $(GOC_CMD_PKGS) \
		-args -runtime-coverprofile=$(abspath $(COVERAGE_REPORT)) -runtime-coverruns=$(COVERAGE_RUNS)

# Compare a generated report against the accepted baseline. Exits non-zero on a
# coverage or program regression, so it is usable as a review gate.
runtime-cover-diff:
	$(GO) run ./cmd/goc runtime-cover-diff -fail-on-regression $(COVERAGE_BASELINE) $(COVERAGE_REPORT)

# The Ruby/C compiler test: the cg12-vs-gcc differential and miniruby
# regressions (arm64 Linux; needs gcc).
test-ruby:
	$(GO) test $(RUBY_PKGS)

# The full CRuby target: build real CRuby with cg12 and check correctness and
# performance against a reference build. Out of tree — see the script.
test-cruby:
	./scripts/cruby-diff.sh

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	$(GO) clean ./...
