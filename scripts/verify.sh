#!/usr/bin/env bash
# Tiered verification. One entry point, two answers.
#
#   scripts/verify.sh fast    a trustworthy answer in under ten minutes
#   scripts/verify.sh full    the exhaustive run
#
# Normally reached through `make verify-fast` / `make verify-full`, which is
# where the defaults live.
#
# WHY THIS EXISTS
#
# Every gate in this tree took forty to a hundred and twenty minutes, which is
# too slow to steer by: by the time a run says no, the thing it says no about is
# three changes old. Two causes, in order of size.
#
# 1. The goc suite ran serially. 349 top-level tests, none calling t.Parallel,
#    on a 64-core box -- measured at 4.0 of 64 cores for nineteen minutes. Every
#    gate was told to pass `-parallel 10`, which bounds only tests that opt in,
#    so it bounded nothing. See goc/parallelpolicy_test.go and
#    goc/sequential_tests.txt, which is also where the 83 tests that must NOT be
#    parallel are listed and the measurement that put them there is written down.
# 2. A gate ran four long things one after another -- corpus, default
#    capability arm, -O arm, and a `main` control of each -- on a box with
#    sixty-four cores and one of them busy.
#
# This script fixes (2). It runs the independent items concurrently under an
# explicit core budget, shards the capability matrix, and reuses a recorded
# `main` control when the machine, the toolchain and `main` itself are all
# unchanged.
#
# WHAT IT WILL NOT DO
#
# It never runs `make bench-perf` or `make bench-crypto`. Those measure elapsed
# time, they pin to a core, and they have a pre-flight that refuses a busy box
# precisely because a contended timing run is worse than no timing run. They
# stay separate targets, to be run on an idle machine, alone. Nothing here
# schedules them and nothing here should.

set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

GO=${GO:-go}
tier=${1:-fast}
case "$tier" in
fast | full) ;;
*)
	echo "usage: $0 [fast|full]" >&2
	exit 2
	;;
esac

# ---------------------------------------------------------------- core budget

# The box's real width. Every job below declares what share of it it will use,
# and the scheduler admits jobs while the declared total fits. The declarations
# are approximations of a job's steady-state width, not guarantees -- their
# purpose is to stop three jobs that each want the whole machine from being
# started at once, not to partition it exactly.
CORES=${VERIFY_CORES:-$(nproc)}

# The memory ceiling matters more than the core ceiling here, because a goc
# compile of a large program peaks at 3.17 GiB (default GOMAXPROCS) and 4.23 GiB
# (GOMAXPROCS=1), and the wide fan-outs below multiply that. GOMEMLIMIT is a soft
# limit: Go collects harder as it approaches, rather than being killed at it. Set
# to 70% of MemAvailable so a burst has somewhere to go.
if [ -z "${VERIFY_GOMEMLIMIT:-}" ]; then
	available_kb=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo 2>/dev/null)
	if [ -n "${available_kb:-}" ]; then
		VERIFY_GOMEMLIMIT="$((available_kb * 7 / 10))KiB"
	fi
fi
export GOMEMLIMIT=${VERIFY_GOMEMLIMIT:-}
[ -n "$GOMEMLIMIT" ] || unset GOMEMLIMIT

# How many goc tests run at once inside one `go test ./goc/...` process. The
# Makefile owns the default; this is only the fallback for a direct invocation.
GOC_PARALLEL=${GOC_PARALLEL:-$((CORES / 2))}
# How many capability programs one matrix arm compiles at once. Two arms run
# concurrently, so each gets half the box.
STATUS_WORKERS=${STATUS_WORKERS:-$((CORES / 4))}
# How many shards the matrix is cut into. The fast tier runs one of them.
VERIFY_STATUS_SHARDS=${VERIFY_STATUS_SHARDS:-4}
VERIFY_STATUS_SHARD=${VERIFY_STATUS_SHARD:-0}

LOGS=${VERIFY_LOGS:-${TMPDIR:-/tmp}/verify-$$}
mkdir -p "$LOGS"

started=$(date +%s)
declare -a JOB_NAME JOB_PID JOB_WEIGHT
declare -A JOB_RESULT JOB_SECONDS
admitted=0

note() { printf '[verify] %s\n' "$*"; }

# spawn NAME WEIGHT -- COMMAND...
#
# Waits until WEIGHT cores are free, then starts COMMAND in the background with
# its output in $LOGS/NAME.log. Nothing is ever silently dropped: every spawned
# job is waited for and reported.
spawn() {
	local name=$1 weight=$2
	shift 3 # name, weight, the literal --
	while [ $((admitted + weight)) -gt "$CORES" ] && [ ${#JOB_PID[@]} -gt 0 ]; do
		reap_one
	done
	local index=${#JOB_NAME[@]}
	note "start  $name (weight $weight, $((CORES - admitted)) cores free)"
	(
		start=$(date +%s)
		"$@" >"$LOGS/$name.log" 2>&1
		status=$?
		echo "$status $(($(date +%s) - start))" >"$LOGS/$name.exit"
		exit $status
	) &
	JOB_NAME[index]=$name
	JOB_PID[index]=$!
	JOB_WEIGHT[index]=$weight
	admitted=$((admitted + weight))
}

reap_one() {
	local index
	while true; do
		for index in "${!JOB_PID[@]}"; do
			if ! kill -0 "${JOB_PID[index]}" 2>/dev/null; then
				collect "$index"
				return
			fi
		done
		sleep 1
	done
}

collect() {
	local index=$1 name=${JOB_NAME[$1]}
	wait "${JOB_PID[$index]}"
	local status=$? seconds=0
	if [ -f "$LOGS/$name.exit" ]; then
		read -r status seconds <"$LOGS/$name.exit"
	fi
	JOB_RESULT[$name]=$status
	JOB_SECONDS[$name]=$seconds
	admitted=$((admitted - JOB_WEIGHT[index]))
	if [ "$status" = 0 ]; then
		note "PASS   $name (${seconds}s)"
	else
		note "FAIL   $name (${seconds}s, exit $status) -- $LOGS/$name.log"
	fi
	unset 'JOB_PID[index]'
}

drain() {
	while [ ${#JOB_PID[@]} -gt 0 ]; do reap_one; done
}

# --------------------------------------------------------- the control cache

# A `main` control only changes when `main` changes -- so re-measuring it every
# gate is pure waste, and it was the single largest avoidable cost in the old
# recipe (it doubled everything). A recorded control is reused when, and only
# when, all of the following match:
#
#   the `main` commit           the thing being controlled for
#   the machine                 CPU model, core count, total RAM, arch, kernel
#   the toolchain               `go version`, the system `cc` version, GOOS/GOARCH
#   the recipe                  a hash of this script
#
# The machine and toolchain are in the key because a control is a *measurement*,
# and a measurement taken on a different box or a different compiler is not a
# control for this one -- it is a different experiment. Kernel release is
# included because it moves scheduling and page-fault behaviour and costs
# nothing to include; the price of including it is an occasional needless
# re-measure, which is the right side to err on. This script's own hash is in
# the key so that changing what a control *measures* invalidates every recorded
# control automatically, rather than depending on someone remembering to bump a
# version constant.
#
# Deliberately NOT in the key: the branch under test, the working tree, and the
# time. A control describes `main`, so anything that describes the branch would
# defeat the reuse this exists for.
#
# The cache lives outside the working tree (jobs get their own worktree, so a
# tree-local cache would never hit across jobs). VERIFY_CACHE overrides it;
# VERIFY_NO_CONTROL_CACHE=1 forces a fresh measurement.
VERIFY_CACHE=${VERIFY_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/cg12/verify-controls}

control_key() {
	local main_sha
	main_sha=$(git rev-parse "${VERIFY_CONTROL_REF:-main}" 2>/dev/null) || return 1
	{
		echo "main=$main_sha"
		echo "cpu=$(awk -F': ' '/^model name|^Model name|^CPU part/ {print $2; exit}' /proc/cpuinfo 2>/dev/null)"
		echo "cores=$(nproc)"
		echo "memtotal_gib=$(awk '/^MemTotal:/ {printf "%d", $2/1048576}' /proc/meminfo 2>/dev/null)"
		echo "arch=$(uname -m)"
		echo "kernel=$(uname -sr)"
		echo "go=$($GO version)"
		echo "goenv=$($GO env GOOS GOARCH | tr '\n' '/')"
		echo "cc=$( (cc --version 2>/dev/null || echo none) | head -1)"
		echo "recipe=$(sha256sum "$ROOT/scripts/verify.sh" | cut -d' ' -f1)"
	} | sha256sum | cut -d' ' -f1
}

# control ITEM -- COMMAND...
#
# Reuse the recorded result for ITEM if one exists under the current key;
# otherwise run COMMAND against a checkout of `main` and record it.
control() {
	local item=$1
	shift 2 # item, the literal --
	local key entry
	key=$(control_key) || {
		note "control $item: cannot resolve ${VERIFY_CONTROL_REF:-main}; SKIPPED"
		JOB_RESULT["control-$item"]=skipped
		return 0
	}
	entry="$VERIFY_CACHE/$key/$item"

	if [ -z "${VERIFY_NO_CONTROL_CACHE:-}" ] && [ -f "$entry.ok" ]; then
		note "REUSE  control-$item (recorded $(cat "$entry.when" 2>/dev/null), key ${key:0:12})"
		cp "$entry.log" "$LOGS/control-$item.log" 2>/dev/null
		JOB_RESULT["control-$item"]=0
		JOB_SECONDS["control-$item"]=0
		return 0
	fi

	note "MEASURE control-$item (no record for key ${key:0:12})"
	mkdir -p "$VERIFY_CACHE/$key"
	local worktree="${TMPDIR:-/tmp}/verify-control-$key"
	if [ ! -d "$worktree" ]; then
		git worktree add --detach "$worktree" "${VERIFY_CONTROL_REF:-main}" >/dev/null 2>&1 || {
			note "control $item: could not create a worktree for ${VERIFY_CONTROL_REF:-main}; SKIPPED"
			JOB_RESULT["control-$item"]=skipped
			return 0
		}
		CONTROL_WORKTREES="${CONTROL_WORKTREES:-} $worktree"
	fi

	local start status
	start=$(date +%s)
	(cd "$worktree" && "$@") >"$LOGS/control-$item.log" 2>&1
	status=$?
	JOB_RESULT["control-$item"]=$status
	JOB_SECONDS["control-$item"]=$(($(date +%s) - start))
	if [ "$status" = 0 ]; then
		cp "$LOGS/control-$item.log" "$entry.log"
		date -Is >"$entry.when"
		: >"$entry.ok"
		note "PASS   control-$item (${JOB_SECONDS[control-$item]}s, recorded)"
	else
		note "FAIL   control-$item (${JOB_SECONDS[control-$item]}s, exit $status) -- not recorded"
	fi
}

cleanup_controls() {
	local worktree
	for worktree in ${CONTROL_WORKTREES:-}; do
		git worktree remove --force "$worktree" >/dev/null 2>&1
	done
}
trap cleanup_controls EXIT

# --------------------------------------------------------------- the recipes

matrix_arm() {
	local shards=$1 shard=$2
	shift 2
	$GO test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/... -v \
		-args -runtime-status-shards="$shards" -runtime-status-shard="$shard" \
		-runtime-status-compile-workers="$STATUS_WORKERS" "$@"
}

audits() {
	$GO test -timeout 30m -count=1 -parallel "$GOC_PARALLEL" ./goc/ \
		-run '^(TestAllocationCensus|TestFrameEscapeAudit|TestLoopAliasAudit|TestEscapeShadowPlacement)$' -v
}

corpus() {
	$GO test -timeout 60m -count=1 -parallel "$GOC_PARALLEL" ./goc/...
}

# The corpus suite, split into a parallel half and a sequential half that run as
# two concurrent PROCESSES.
#
# Why processes and not one `go test`: goc/sequential_tests.txt lists 83 tests
# that must not run while another compile is in flight *in the same process*
# (see the file for the measurement). Inside one `go test`, Go gives them that by
# running every sequential test to completion before resuming any parallel one —
# correct, but strictly additive: the sequential half's several minutes and the
# parallel half's seconds are paid one after the other. Two processes share no
# compiler state at all, so the halves are independent and the suite costs the
# larger of the two rather than their sum.
#
# The sequential half is itself split across SEQ_SHARDS processes, each running
# `-parallel 1`, which is the same guarantee again: one compile at a time per
# process. The four corpus audits are pinned to shard 0 because they share one
# sync.Once corpus compile — splitting them would pay for it several times over.
SEQ_SHARDS=${SEQ_SHARDS:-3}
SEQUENTIAL_LIST=$ROOT/goc/sequential_tests.txt

sequential_names() { awk -F'\t' '/^Test/ {print $1}' "$SEQUENTIAL_LIST"; }

corpus_parallel_half() {
	local skip
	skip=$(sequential_names | paste -sd'|')
	$GO test -timeout 60m -count=1 -parallel "$GOC_PARALLEL" ./goc/ -skip "^($skip)\$"
}

corpus_sequential_shard() {
	local shard=$1 run
	# The audits go to shard 0; everything else round-robins by position. Cost
	# balancing beyond that is not worth the machinery — after the audit pool
	# was unbounded, no other listed test is close to dominating.
	run=$(sequential_names | awk -v s="$shard" -v n="$SEQ_SHARDS" '
		/^Test(AllocationCensus|FrameEscapeAudit|LoopAliasAudit|EscapeShadowPlacement)$/ {
			if (s == 0) print; next
		}
		{ if (i++ % n == s) print }' | paste -sd'|')
	if [ -z "$run" ]; then
		echo "shard $shard: no tests"
		return 0
	fi
	$GO test -timeout 60m -count=1 -parallel 1 ./goc/ -run "^($run)\$"
}

spawn_corpus() {
	spawn corpus-parallel "$GOC_PARALLEL" -- corpus_parallel_half
	local shard=0
	while [ "$shard" -lt "$SEQ_SHARDS" ]; do
		# A sequential shard is one compile at a time in its own process, but a
		# single goc compile is itself internally parallel, so it is not worth
		# one core.
		spawn "corpus-sequential-$shard" 6 -- corpus_sequential_shard "$shard"
		shard=$((shard + 1))
	done
}

note "tier=$tier cores=$CORES goc-parallel=$GOC_PARALLEL status-workers=$STATUS_WORKERS"
note "logs in $LOGS"
[ -n "${GOMEMLIMIT:-}" ] && note "GOMEMLIMIT=$GOMEMLIMIT"

# Build first and alone. Everything else needs the packages compiled, and a
# concurrent build is where `go build`'s own cache contention shows up.
note "start  build"
if ! $GO build ./... >"$LOGS/build.log" 2>&1; then
	JOB_RESULT[build]=1
	note "FAIL   build -- $LOGS/build.log"
	cat "$LOGS/build.log"
	exit 1
fi
JOB_RESULT[build]=0
note "PASS   build"

case "$tier" in
fast)
	# Under ten minutes. What it covers and what it cannot see is in the
	# Makefile's verify-fast comment and in docs/verification.md -- read it
	# before trusting a green run for anything a merge depends on.
	spawn vet 4 -- $GO vet ./...
	spawn gofmt 2 -- "$ROOT/scripts/gofmt-check.sh"
	spawn unit 8 -- $GO test -parallel "$GOC_PARALLEL" $($GO list ./... | grep -vE '/(goc|cmd/goc|difftest|cc)(/|$)')
	spawn_corpus
	spawn matrix-default "$STATUS_WORKERS" -- matrix_arm "$VERIFY_STATUS_SHARDS" "$VERIFY_STATUS_SHARD"
	spawn matrix-opt "$STATUS_WORKERS" -- matrix_arm "$VERIFY_STATUS_SHARDS" "$VERIFY_STATUS_SHARD" -runtime-opt
	spawn reducers 6 -- "$ROOT/scripts/reducers.sh" fast
	drain
	;;
full)
	spawn vet 4 -- $GO vet ./...
	spawn gofmt 2 -- "$ROOT/scripts/gofmt-check.sh"
	spawn unit 8 -- $GO test -count=1 -parallel "$GOC_PARALLEL" $($GO list ./... | grep -vE '/(goc|cmd/goc|difftest|cc)(/|$)')
	spawn_corpus
	spawn goc-cmd 8 -- $GO test -timeout 15m -parallel "$GOC_PARALLEL" -skip '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/...
	spawn ruby 8 -- $GO test -timeout 30m -parallel "$GOC_PARALLEL" ./difftest/... ./cc/...
	drain

	# The matrix arms and the determinism sweep each want a large share, so
	# they get the box to themselves rather than contending with the corpus.
	# Both arms together, because one arm cannot fill 64 cores: its wall clock
	# is floored by a single long compile (see
	# compileRuntimeCapabilityWorkers in cmd/goc/runtime_status_test.go).
	shard=0
	while [ "$shard" -lt "$VERIFY_STATUS_SHARDS" ]; do
		spawn "matrix-default-$shard" "$STATUS_WORKERS" -- matrix_arm "$VERIFY_STATUS_SHARDS" "$shard"
		spawn "matrix-opt-$shard" "$STATUS_WORKERS" -- matrix_arm "$VERIFY_STATUS_SHARDS" "$shard" -runtime-opt
		shard=$((shard + 1))
	done
	drain

	spawn determinism "$CORES" -- "$ROOT/scripts/determinism-check.sh" -corpus
	drain
	spawn determinism-opt "$CORES" -- "$ROOT/scripts/determinism-check.sh" -corpus -O
	drain

	spawn reducers 16 -- "$ROOT/scripts/reducers.sh" full
	drain

	# The `main` control, reused when nothing that could change it has changed.
	if [ -z "${VERIFY_NO_CONTROL:-}" ]; then
		control corpus -- $GO test -timeout 60m -parallel "$GOC_PARALLEL" ./goc/...
		control matrix-default -- $GO test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/... -v \
			-args -runtime-status-compile-workers="$STATUS_WORKERS"
		control matrix-opt -- $GO test -timeout 30m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc/... -v \
			-args -runtime-opt -runtime-status-compile-workers="$STATUS_WORKERS"
	fi
	;;
esac

# -------------------------------------------------------------------- verdict

elapsed=$(($(date +%s) - started))
failed=0
echo
printf '%-24s %8s  %s\n' "ITEM" "SECONDS" "RESULT"
for name in "${!JOB_RESULT[@]}"; do
	printf '%-24s %8s  %s\n' "$name" "${JOB_SECONDS[$name]:-0}" "${JOB_RESULT[$name]}"
done | sort
for name in "${!JOB_RESULT[@]}"; do
	case "${JOB_RESULT[$name]}" in
	0 | skipped) ;;
	*) failed=$((failed + 1)) ;;
	esac
done
echo
if [ "$failed" = 0 ]; then
	note "verify-$tier PASS in ${elapsed}s ($((elapsed / 60))m$((elapsed % 60))s)"
else
	note "verify-$tier FAIL in ${elapsed}s -- $failed item(s) failed; logs in $LOGS"
fi
exit $((failed > 0))
