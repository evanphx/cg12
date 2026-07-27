# cg12 test setup. Local development and CI share these targets.
#
# The suite splits into three tracked categories:
#   test-unit  — the Go unit tests: IR, optimizer, backends, analysis, ...
#   test-go    — the Go compiler work: the goc corpus compiles, links, and runs
#                real Go programs (needs a system `cc`; arm64 Linux).
#   test-ruby  — the Ruby/C compiler test: cg12's C frontend checked
#                differentially against gcc (Ruby is written in C), plus
#                regressions captured from compiling miniruby (needs gcc; arm64).
# and a heavier, out-of-tree target:
#   test-cruby — build real CRuby with cg12 and check correctness and
#                performance against a reference build (see scripts/cruby-diff.sh).

GO ?= go

# The Go compiler corpus and the C/Ruby differential are called out explicitly;
# everything else is a plain unit test. Kept in sync by exclusion so a new
# package lands in test-unit automatically.
GOC_PKGS  := ./goc/...
RUBY_PKGS := ./difftest/... ./cc/...
UNIT_PKGS := $(shell $(GO) list ./... | grep -vE '^github.com/evanphx/cg12/(goc|difftest|cc)(/|$$)')

.PHONY: all build test test-unit test-go test-ruby test-cruby fmt vet clean

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

# The Go compiler work: the goc corpus (arm64 Linux; needs a system `cc`).
test-go:
	$(GO) test $(GOC_PKGS)

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
