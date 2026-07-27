#!/usr/bin/env bash
#
# cruby-diff.sh — build real CRuby with cg12 and check correctness and
# performance against a reference (gcc) build.
#
# cg12's C compiler (cmd/cg12cc) stands in as CRuby's CC. CRuby is the
# correctness-and-performance target for the compiler: it is a large, real C
# program whose own test suite and benchmarks exercise the backend far beyond
# the in-tree corpus.
#
# STATUS: STUB. The steps below are placeholders. Fill them in, set
# CG12_CRUBY_CONFIGURED=1, and remove the guard; the CRuby CI workflow runs this.
set -euo pipefail

# --- configuration (fill in) ------------------------------------------------
CRUBY_REPO="${CRUBY_REPO:-https://github.com/ruby/ruby.git}"
CRUBY_REF="${CRUBY_REF:-}"                 # pin a Ruby commit cg12 is known to build
WORKDIR="${WORKDIR:-/tmp/cg12-cruby}"
JOBS="${JOBS:-$(nproc)}"

# --- stub guard -------------------------------------------------------------
if [ -z "${CRUBY_REF}" ] || [ "${CG12_CRUBY_CONFIGURED:-}" != "1" ]; then
  cat >&2 <<'EOF'
cruby-diff.sh is a stub. To enable the CRuby correctness + performance target:
  1. Pin CRUBY_REF to a Ruby commit cg12 builds.
  2. Build cg12's C compiler:  go build -o "$WORKDIR/cg12cc" ./cmd/cg12cc
  3. Build CRuby with it:      ./configure CC="$WORKDIR/cg12cc" ... && make miniruby
     (and a reference build with CC=gcc for the differential/benchmark baseline).
  4. Correctness:             run bootstraptest / `make test`, or diff the
                              cg12-built miniruby's output against the gcc build.
  5. Performance:             run the benchmark set on both builds and compare;
                              record a baseline and fail on regression past a
                              threshold.
  6. Set CG12_CRUBY_CONFIGURED=1 and delete this guard.
EOF
  exit 2
fi

mkdir -p "$WORKDIR"

# --- 1. cg12's C compiler ---------------------------------------------------
# go build -o "$WORKDIR/cg12cc" ./cmd/cg12cc

# --- 2. fetch + build CRuby with cg12 (and a gcc reference) ------------------
# git clone --depth 1 --branch "$CRUBY_REF" "$CRUBY_REPO" "$WORKDIR/ruby"
# ( cd "$WORKDIR/ruby" && ./autogen.sh && ./configure CC="$WORKDIR/cg12cc" && make -j"$JOBS" miniruby )

# --- 3. correctness ---------------------------------------------------------
# ( cd "$WORKDIR/ruby" && make btest )      # or a differential vs the gcc build

# --- 4. performance ---------------------------------------------------------
# run benchmarks on the cg12 and gcc builds; compare against a recorded baseline

echo "cruby-diff: TODO — build, correctness, and performance steps not yet filled in" >&2
exit 1
