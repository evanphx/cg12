#!/usr/bin/env bash
#
# bench-sqlite.sh — time a cg12-compiled SQLite against the system compilers.
#
# This answers "is cg12's generated code getting faster or slower", which is a
# different question from the ones the other scripts ask. cc-identity-check.sh
# asks whether the emitted bytes moved, and cc/behaviour_test.go asks whether the
# program still computes the right answer. Neither notices a change that makes
# correct code slow.
#
# Method, and why each part is there:
#
#   Only the SQLite object varies. The driver is built by the host compiler in
#   every arm, so a difference in time is a difference in how SQLite was
#   compiled, and not in anything else.
#
#   Timing happens inside the process. cc/testdata/perf/sqlite_bench.c times each
#   phase with a monotonic clock, so process startup, dynamic linking and
#   teardown are outside every measurement.
#
#   Rounds are interleaved -- every arm runs once, then every arm runs again --
#   so a machine that warms up or throttles partway through spreads that drift
#   across all the arms instead of penalising whichever one ran last.
#
#   Every run is pinned to one core. Letting the scheduler migrate a run between
#   cores costs it its warm caches partway through, and measurably widens the
#   spread (6.6% unpinned against 4.1% pinned, over six runs here).
#
#   The reported number is a MINIMUM, per phase. Noise on a timing run is
#   one-sided -- interference can only make a run slower, never faster -- so the
#   fastest observation of a phase is the closest to what that code actually
#   costs. The total is the sum of those per-phase minima, so the total is
#   consistent with the column above it and does not depend on any single round
#   having been clean throughout.
#
#   Spread is reported separately, and means something different: it is how much
#   the slowest WHOLE round exceeded the fastest, which is a statement about how
#   settled the machine was rather than about the code. Read it as the resolution
#   of the instrument, and distrust any difference between arms smaller than it.
#
#   What it resolves. The round-to-round spread here is 5-11%, but the reported
#   minimum is far steadier than any single round: two independent invocations of
#   this script agreed to 1.1% (gcc), 0.6% (clang) and 1.7% (cg12).
#
#   Better still is a comparison made INSIDE one invocation, which is what -ref
#   gives, because both arms then meet the same machine in the same state. Run
#   `-ref HEAD` and the instrument measures itself: that builds the same compiler
#   twice, so whatever gap appears between the two cg12 arms is pure measurement
#   error. It came to 0.8%. So a regression of a couple of percent is visible
#   through -ref, and anything under about one percent is not.
#
#   Checksums are compared before times are. Arms that computed different things
#   cannot be compared, and a checksum mismatch means a miscompile, which is a
#   much more interesting result than a timing one.
#
# Everything runs under prlimit. A benchmark should never be able to take the
# machine down, and an early draft of the driver -- which had signed-overflow
# undefined behaviour that cost it its loop exit -- did exactly that.
#
# Usage:
#   scripts/bench-sqlite.sh                  # gcc -O2, clang -O2, cg12 -O
#   scripts/bench-sqlite.sh -rounds 9        # more rounds, tighter spread
#   scripts/bench-sqlite.sh -scale 2         # twice the work per round
#   scripts/bench-sqlite.sh -all             # add the -O0 arms
#   scripts/bench-sqlite.sh -ref HEAD~1      # add a cg12 built from another commit
#
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

ROUNDS=7
SCALE=1
ALL=0
REF=""

while [ $# -gt 0 ]; do
	case "$1" in
	-rounds) ROUNDS=$2; shift 2 ;;
	-scale) SCALE=$2; shift 2 ;;
	-all) ALL=1; shift ;;
	-ref) REF=$2; shift 2 ;;
	-h | -help | --help) sed -n '2,45p' "$0"; exit 0 ;;
	*) echo "unknown flag $1" >&2; exit 2 ;;
	esac
done

BENCH="$ROOT/cc/testdata/bench"
DRIVER="$ROOT/cc/testdata/perf/sqlite_bench.c"
SQLITE="$BENCH/sqlite3.c"

if [ ! -f "$SQLITE" ]; then
	echo "[bench-sqlite] $SQLITE is not present; run scripts/fetch-bench-c.sh" >&2
	exit 1
fi

WORK=$(mktemp -d)
cleanup() {
	rm -rf "$WORK"
	if [ -n "${REF_TREE:-}" ]; then
		git worktree remove --force "$REF_TREE" >/dev/null 2>&1
	fi
}
trap cleanup EXIT

note() { echo "[bench-sqlite] $*" >&2; }

# guard runs one command with a ceiling on address space and CPU time, so a
# runaway cannot reach the rest of the machine. 4 GiB is many times what the
# workload needs (about 75 MiB), and the CPU cap is generous for a run that
# should take a few seconds.
PIN=""
if command -v taskset >/dev/null && [ "$(nproc 2>/dev/null || echo 1)" -gt 2 ]; then
	# Any single core will do; 2 avoids core 0, which tends to take more interrupt
	# work than the rest.
	PIN="taskset -c 2"
fi
guard() { $PIN prlimit --as=4000000000 --cpu=300 -- "$@"; }

# The driver, built once by the host compiler and linked into every arm. -Werror
# is not decoration: the bug that made an early version of this driver dangerous
# announced itself as -Waggressive-loop-optimizations, and was read past.
note "building the driver with the host compiler"
if ! gcc -O2 -Werror -I "$BENCH" -c -o "$WORK/driver.o" "$DRIVER"; then
	note "FAIL: the driver does not build"
	exit 1
fi

ARMS=()

# build_arm compiles SQLite with one toolchain and links it against the shared
# driver. It records the arm only if every step succeeded, so a missing compiler
# is skipped with a note rather than reported as infinitely slow.
build_arm() {
	local name=$1; shift
	local object="$WORK/$name.o"
	note "building $name"
	if ! (cd "$BENCH" && "$@" -c -o "$object" "$SQLITE") >"$WORK/$name.build" 2>&1; then
		note "skipping $name: it did not compile (see $WORK/$name.build)"
		return
	fi
	if ! gcc -o "$WORK/$name" "$object" "$WORK/driver.o" -lm -lpthread -ldl >>"$WORK/$name.build" 2>&1; then
		note "skipping $name: it did not link (see $WORK/$name.build)"
		return
	fi
	ARMS+=("$name")
}

if command -v gcc >/dev/null; then
	build_arm gcc-O2 gcc -O2
	[ "$ALL" = 1 ] && build_arm gcc-O0 gcc -O0
fi
if command -v clang >/dev/null; then
	build_arm clang-O2 clang -O2
	[ "$ALL" = 1 ] && build_arm clang-O0 clang -O0
fi

note "building cc from the working tree"
if ! go build -o "$WORK/cc" ./cmd/cc; then
	note "FAIL: the working tree does not build"
	exit 1
fi
build_arm cg12-O "$WORK/cc" -O
[ "$ALL" = 1 ] && build_arm cg12-O0 "$WORK/cc"

# -ref adds a cg12 built from another commit, which is how a change to the
# optimizer is judged: the same workload, the same machine, the same rounds.
if [ -n "$REF" ]; then
	REF_TREE=$(mktemp -d)
	rm -rf "$REF_TREE"
	if git worktree add --detach "$REF_TREE" "$REF" >/dev/null 2>&1 &&
		(cd "$REF_TREE" && go build -o "$WORK/cc.ref" ./cmd/cc); then
		build_arm "cg12-O@$REF" "$WORK/cc.ref" -O
	else
		note "skipping the $REF arm: it does not build"
	fi
fi

if [ ${#ARMS[@]} -eq 0 ]; then
	note "FAIL: no arm built"
	exit 1
fi

# One discarded warm-up round per arm: the first run of a program pays for page
# faults and for filling caches that every later run finds already warm.
note "warm-up"
for arm in "${ARMS[@]}"; do
	guard "$WORK/$arm" "$SCALE" >/dev/null 2>&1
done

note "$ROUNDS rounds over ${#ARMS[@]} arms, scale $SCALE"
: >"$WORK/samples.txt"
for round in $(seq 1 "$ROUNDS"); do
	for arm in "${ARMS[@]}"; do
		if ! guard "$WORK/$arm" "$SCALE" >"$WORK/out" 2>&1; then
			note "FAIL: $arm did not complete (round $round)"
			cat "$WORK/out" >&2
			exit 1
		fi
		sed "s/^/$arm /" "$WORK/out" >>"$WORK/samples.txt"
	done
	printf '.' >&2
done
echo >&2

cp "$WORK/samples.txt" "${BENCH_SQLITE_LOG:-${TMPDIR:-/tmp}}/bench-sqlite-samples.txt" 2>/dev/null || true

# The report. Phases keep the order the driver printed them in, which is the
# order they ran, rather than being sorted into something less meaningful.
awk -v arms="${ARMS[*]}" '
# Each run prints its phases and then its checksum, so the checksum line is the
# end-of-run marker: that is where a round total is banked and the accumulator
# reset. Rounds have to be counted this way rather than assumed, because the
# spread below is a statement about whole runs.
$2 == "phase" {
	arm = $1; ph = $3; ms = $4
	key = arm SUBSEP ph
	if (!(key in lo) || ms < lo[key]) lo[key] = ms
	running[arm] += ms
	if (!(ph in seen)) { order[++nph] = ph; seen[ph] = 1 }
	next
}
$2 == "checksum" {
	arm = $1
	if (arm in ck && ck[arm] != $3) mixed[arm] = 1
	ck[arm] = $3
	t = running[arm]; running[arm] = 0
	if (!(arm in runLo) || t < runLo[arm]) runLo[arm] = t
	if (!(arm in runHi) || t > runHi[arm]) runHi[arm] = t
	next
}
END {
	n = split(arms, a, " ")

	# An arm that disagreed with ITSELF between rounds is not a timing problem
	# either: the same binary on the same input computed two different answers.
	for (i = 1; i <= n; i++) {
		if (a[i] in mixed) {
			printf "NONDETERMINISM: %s produced different checksums across rounds\\n", a[i] > "/dev/stderr"
			exit 1
		}
	}

	# A phase that measures as nothing is not fast, it is broken. The expr phase
	# once used ^ for xor, which SQLite does not have, so the statement failed to
	# prepare and the phase timed a loop that did nothing at all. Catching it costs
	# three lines; not catching it cost a plausible-looking benchmark result.
	for (i = 1; i <= n; i++) {
		for (p = 1; p <= nph; p++) {
			if (lo[a[i], order[p]] < 1.0) {
				printf "PHASE DID NO WORK: %s measured %.1f ms in %s\\n", order[p], lo[a[i], order[p]], a[i] > "/dev/stderr"
				printf "  a phase this fast is not doing what it claims; check the statement it runs\\n" > "/dev/stderr"
				exit 1
			}
		}
	}

	# Checksums across arms: a timing comparison between arms that computed
	# different things is meaningless, so this is a hard stop rather than a warning.
	base = ck[a[1]]
	for (i = 2; i <= n; i++) {
		if (ck[a[i]] != base) {
			printf "CHECKSUM MISMATCH: %s=%s but %s=%s\n", a[1], base, a[i], ck[a[i]] > "/dev/stderr"
			printf "  the arms did not compute the same thing; that is a miscompile, not a timing result\n" > "/dev/stderr"
			exit 1
		}
	}
	printf "\nchecksum %s (identical across all %d arms)\n\n", base, n

	# The total is the sum of the per-phase minima printed above it.
	for (i = 1; i <= n; i++) {
		t = 0
		for (p = 1; p <= nph; p++) t += lo[a[i], order[p]]
		sum[a[i]] = t
		if (t > 0 && (best == 0 || t < best)) best = t
	}

	printf "%-12s", "phase"
	for (i = 1; i <= n; i++) printf " %14s", a[i]
	printf "\n"
	for (p = 1; p <= nph; p++) {
		printf "%-12s", order[p]
		for (i = 1; i <= n; i++) printf " %14.1f", lo[a[i], order[p]]
		printf "\n"
	}
	printf "%-12s", "TOTAL ms"
	for (i = 1; i <= n; i++) printf " %14.1f", sum[a[i]]
	printf "\n"
	printf "%-12s", "vs fastest"
	for (i = 1; i <= n; i++) printf " %13.2fx", sum[a[i]] / best
	printf "\n"
	printf "%-12s", "spread"
	for (i = 1; i <= n; i++) printf " %13.1f%%", (runHi[a[i]] - runLo[a[i]]) / runLo[a[i]] * 100
	printf "\n\n"
	printf "Milliseconds, each the fastest round observed for that phase; TOTAL is\n"
	printf "their sum. Spread is a separate measure -- how much the slowest whole\n"
	printf "round exceeded the fastest -- and stands for how settled the machine was.\n"
	printf "Treat a gap between arms narrower than the spread as no gap at all.\n"
}
' "$WORK/samples.txt"
