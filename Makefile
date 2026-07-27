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

.PHONY: all build test test-unit test-goc-corpus test-goc-cmd test-goc-status \
        test-ruby test-cruby fmt vet clean

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
	$(GO) test $(GOC_CORPUS_PKGS)

# The goc driver end-to-end tests, excluding the long capability matrix.
test-goc-cmd:
	$(GO) test -timeout $(GOC_CMD_TIMEOUT) -skip '$(STATUS_TEST)' $(GOC_CMD_PKGS)

# The runtime-capability matrix (optionally sharded via STATUS_SHARDS/STATUS_SHARD).
# The shard flags are defined by the test binary, so they go after -args (and the
# package) where `go test` passes them through unaltered.
test-goc-status:
	$(GO) test -timeout $(STATUS_TIMEOUT) -run '^$(STATUS_TEST)$$' $(GOC_CMD_PKGS) \
		-args -runtime-status-shards=$(STATUS_SHARDS) -runtime-status-shard=$(STATUS_SHARD)

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
