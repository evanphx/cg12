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
#   test-goc-status-opt— the same matrix with goc optimization on. A separate
#                       target because it is a separate configuration, not a
#                       faster one; see below.
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

# Two tiered entry points sit on top of all of this:
#   verify-fast — build, vet, gofmt, unit, the corpus, one capability shard of
#                 each arm, and the targeted reducers. Under ten minutes.
#   verify-full — the exhaustive run, including a `main` control.
# docs/verification.md says what the fast tier can and cannot catch. Read it
# before treating a green verify-fast as a merge signal.

GO ?= go

# ---------------------------------------------------------------- parallelism
#
# How many tests run at once. This is a DEFAULT, not something a caller has to
# remember, because for most of this tree's life every caller remembered the
# wrong thing: gates were briefed to pass `-parallel 10`, a number that came
# from an eight-core laptop, to a sixty-four-core worker. It did not even do
# that -- `-parallel` bounds tests that call t.Parallel, and until now no test in
# the goc suite did, so the suite ran at 2.9 of 64 cores for twenty-one minutes.
#
# The number is bounded by memory, not by cores. A goc compile of the corpus's
# worst program peaks at 3.17 GiB of RSS at the default GOMAXPROCS (4.23 GiB at
# GOMAXPROCS=1), and every concurrent test can be compiling one. So the default
# is the smaller of the core count and what MemAvailable will hold at
# TEST_MEMORY_PER_JOB GiB apiece -- which on the 64-core / 243 GiB worker is
# 32, and on an eight-core laptop is 8, without either caller knowing the other
# exists.
#
# Measured on the 64-core worker, whole goc suite: see docs/verification.md.
# Above 32 the curve is flat and memory is the only thing still moving, so 32 is
# where this stops rather than where it runs out.
NPROC              := $(shell nproc 2>/dev/null || echo 8)
MEM_AVAILABLE_GIB  := $(shell awk '/^MemAvailable:/ {printf "%d", $$2/1048576}' /proc/meminfo 2>/dev/null || echo 16)
TEST_MEMORY_PER_JOB ?= 4
TEST_PARALLEL_CAP  ?= 32
GO_TEST_PARALLEL   ?= $(shell m=$$(( $(MEM_AVAILABLE_GIB) / $(TEST_MEMORY_PER_JOB) )); \
                        n=$(NPROC); [ $$m -lt $$n ] && n=$$m; \
                        [ $$n -gt $(TEST_PARALLEL_CAP) ] && n=$(TEST_PARALLEL_CAP); \
                        [ $$n -lt 1 ] && n=1; echo $$n)

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
# How the verify tiers shard it. Four, because the fast tier wants a shard it
# can afford and a quarter of the matrix is a meaningful sample; the full tier
# runs all four and so loses nothing. These are separate from STATUS_SHARDS so
# that a caller sharding by hand is unaffected.
VERIFY_STATUS_SHARDS ?= 4
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

# The crypto signing benchmark compiles one program with each compiler and then
# runs real P-256 and RSA arithmetic under both; the goc-built half dominates.
CRYPTO_BENCH_TIMEOUT ?= 20m

# Both timing suites refuse to start on a box that cannot support a measurement.
# The pre-flight costs about a second and a half: it times a calibration burst on
# the very core the run is about to pin to, launched through the same taskset
# prefix, and reads that core's /proc/stat counters around it. It exists because
# the ceilings that catch a contaminated run catch it at the END, which cost this
# tree eleven minutes an attempt to be told the machine was busy.
#
#   GOC_BENCH_PREFLIGHT=off   skip it and measure anyway. For when a number from a
#                             busy box is wanted deliberately; the run then does
#                             what it did before -- measures for its full duration
#                             and refuses at the noise ceiling if the
#                             contamination was large enough to show.
#
# goc/benchpreflight_test.go has what it measures and what it deliberately does
# not.

# The runtime performance suite builds eleven programs with each compiler and
# times three arms of each, nine times over. About eleven minutes on an idle
# box; the timeout is generous because a loaded one is slower and a run that
# dies half way through says nothing at all.
PERF_BENCH_TIMEOUT ?= 45m

.PHONY: all build test test-unit test-goc-corpus test-goc-cmd test-goc-status \
        test-goc-status-opt test-goc-coverage runtime-cover-diff test-ruby \
        test-cruby bench-crypto bench-crypto-update bench-perf bench-perf-update \
        verify-fast verify-full verify-audits verify-reducers \
        fmt vet clean

# The default local check: build, then the whole suite.
all: build test

build:
	$(GO) build ./...

# The whole suite in one command.
test:
	$(GO) test -parallel $(GO_TEST_PARALLEL) ./...

# The Go unit tests (fast; no external toolchain required).
test-unit:
	$(GO) test -parallel $(GO_TEST_PARALLEL) $(UNIT_PKGS)

# The goc corpus (arm64 Linux; needs a system `cc`).
test-goc-corpus:
	$(GO) test -timeout $(GOC_CORPUS_TIMEOUT) -parallel $(GO_TEST_PARALLEL) $(GOC_CORPUS_PKGS)

# The goc driver end-to-end tests, excluding the long capability matrix.
test-goc-cmd:
	$(GO) test -timeout $(GOC_CMD_TIMEOUT) -parallel $(GO_TEST_PARALLEL) -skip '$(STATUS_TEST)' $(GOC_CMD_PKGS)

# The runtime-capability matrix (optionally sharded via STATUS_SHARDS/STATUS_SHARD).
# The shard flags are defined by the test binary, so they go after -args (and the
# package) where `go test` passes them through unaltered.
test-goc-status:
	$(GO) test -timeout $(STATUS_TIMEOUT) -run '^$(STATUS_TEST)$$' $(GOC_CMD_PKGS) \
		-args -runtime-status-shards=$(STATUS_SHARDS) -runtime-status-shard=$(STATUS_SHARD)

# The same matrix with `goc -O`, which is a different configuration and not
# merely a faster one: the pack every program links against is built with -O
# too, and both halves of the split are optimized after the split has run.
#
# It exists because nothing ran it. `-O` plus a prebuilt pack failed to link
# sixteen capabilities for as long as the split has existed, and no job or CI
# run noticed, because every one of them exercised the default arm. A
# configuration that ships and is never run is a configuration whose state
# nobody knows.
test-goc-status-opt:
	$(GO) test -timeout $(STATUS_TIMEOUT) -run '^$(STATUS_TEST)$$' $(GOC_CMD_PKGS) \
		-args -runtime-opt -runtime-status-shards=$(STATUS_SHARDS) -runtime-status-shard=$(STATUS_SHARD)

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
	$(GO) test -parallel $(GO_TEST_PARALLEL) $(RUBY_PKGS)

# The full CRuby target: build real CRuby with cg12 and check correctness and
# performance against a reference build. Out of tree — see the script.
test-cruby:
	./scripts/cruby-diff.sh

# The crypto signing path's elapsed cost, against its committed baseline.
#
# This is the tree's only instrument that measures time rather than counting
# something, which is why it is a target of its own and not part of `test`: it
# needs an idle machine to mean anything, and it takes a few minutes. It exists
# because the one performance regression this tree has a record of -- the escape
# publication fix's effect on bigmod.Nat.Mul -- went unwatched and its number
# went stale. It fails in both directions; see goc/cryptobench_test.go for why
# faster is also news.
bench-crypto:
	$(GO) test -timeout $(CRYPTO_BENCH_TIMEOUT) -run '^TestCryptoSigningBench$$' ./goc -crypto-bench -v

# Rewrite the baseline from this run. Deliberately separate: the value of the
# file is that a movement gets looked at by a person.
bench-crypto-update:
	$(GO) test -timeout $(CRYPTO_BENCH_TIMEOUT) -run '^TestCryptoSigningBench$$' ./goc \
		-crypto-bench -update-crypto-bench -v

# The runtime performance suite: goc against the host Go toolchain on eleven
# workloads, against its committed baseline.
#
# `make bench-crypto` measures one path -- the ECDSA signing path -- because that
# is where the tree's one known performance regression was. This measures the
# rest: an interpreter dispatch loop, memory latency, goroutines and channels,
# allocation and collection, floating point, string formatting, maps and sorting,
# reflection, and two real library workloads (regexp and flate). It exists
# because until it did, a change that cost 5% everywhere outside crypto and slog
# would have landed green.
#
# Opt-in, like the placement comparison and the slog benchmark, because it needs
# a host Go toolchain; and unlike them it measures time, so it needs a quiet
# machine to mean anything. It fails in both directions and every row carries the
# smallest movement it can see; see goc/perfbench_test.go and the baseline's
# header for what a green run does and does not prove.
#
# It pins to the second-highest core it is allowed, leaving the top one to
# bench-crypto, so the two can run at once. Set GOC_PERF_CORE for a third.
bench-perf:
	$(GO) test -timeout $(PERF_BENCH_TIMEOUT) -run '^TestPerformanceSuite$$' ./goc -perf-bench -v

# Rewrite the baseline from this run. Deliberately separate: the value of the
# file is that a movement gets looked at by a person.
bench-perf-update:
	$(GO) test -timeout $(PERF_BENCH_TIMEOUT) -run '^TestPerformanceSuite$$' ./goc \
		-perf-bench -update-perf-bench -v

# ------------------------------------------------------------------- tiers
#
# `make verify-fast` — a trustworthy answer in under ten minutes.
#
# Runs, concurrently under a core budget: build, vet, gofmt, the unit tests, the
# WHOLE goc corpus suite (which carries the four audits in check mode), one shard
# of the capability matrix on each arm, and the targeted reducers at reduced
# repetition counts.
#
# WHAT IT CANNOT SEE. A fast tier that quietly skips something is worse than no
# fast tier, so:
#   * Three of every four capabilities, on both arms (VERIFY_STATUS_SHARDS=4,
#     shard 0). A capability that regresses outside shard 0 is invisible here.
#   * Determinism. `scripts/determinism-check.sh -corpus` is the only thing in
#     the tree that drives all 406 programs to a written object and compares
#     bytes; it is a full-tier item and nothing in the fast tier substitutes.
#   * The Ruby/C differential and the miniruby regressions (test-ruby).
#   * The goc driver end-to-end tests (test-goc-cmd).
#   * The runtime coverage report and its baseline diff.
#   * Any comparison against `main`. verify-fast reports whether the tree is
#     self-consistent, not whether it moved relative to a control.
#   * Anything timing. bench-perf and bench-crypto are never scheduled by either
#     tier — they pin a core, they refuse a busy box in their pre-flight, and a
#     tier that ran them concurrently with a corpus would be measuring the
#     corpus. Run them alone.
#   * A rare intermittent fault. The reducers run at fast counts (5–40 rather
#     than 20–400); RUNTIME_PLAN.md 5.10 records a fault that appeared 3 times in
#     53 runs, which no fast count reaches.
# docs/verification.md has this table with the reasoning.
verify-fast:
	GO=$(GO) GOC_PARALLEL=$(GO_TEST_PARALLEL) \
	VERIFY_STATUS_SHARDS=$(VERIFY_STATUS_SHARDS) ./scripts/verify.sh fast

# `make verify-full` — the exhaustive run. Covers everything verify-fast covers
# plus every item listed above as missing, and compares against a recorded
# `main` control (reused when `main`, the machine and the toolchain are all
# unchanged — see the control-cache comment in scripts/verify.sh).
verify-full:
	GO=$(GO) GOC_PARALLEL=$(GO_TEST_PARALLEL) \
	VERIFY_STATUS_SHARDS=$(VERIFY_STATUS_SHARDS) ./scripts/verify.sh full

# The four corpus audits alone, in check mode against the committed baselines.
# Called out because they are the cheapest high-value item in the tree: one
# corpus compile answers all four.
verify-audits:
	$(GO) test -timeout $(GOC_CORPUS_TIMEOUT) -count=1 -parallel $(GO_TEST_PARALLEL) ./goc/ \
		-run '^(TestAllocationCensus|TestFrameEscapeAudit|TestLoopAliasAudit|TestEscapeShadowPlacement)$$' -v

# The targeted reducer loops. REDUCER_MODE=full for the wave-gate counts.
REDUCER_MODE ?= fast
verify-reducers:
	./scripts/reducers.sh $(REDUCER_MODE)

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	$(GO) clean ./...
