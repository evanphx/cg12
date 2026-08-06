#!/usr/bin/env bash
# The targeted reducers: the handful of programs whose failures this tree has
# actually seen, run in a loop.
#
# Usage: scripts/reducers.sh [fast|full]
#
# These are not tests of new behaviour. Each one is a program that once failed
# intermittently -- a GC placement bug, a stack-growth crash, a wrong signature
# -- reduced to the smallest thing that reproduced it. Every gate since has run
# them in a loop because one execution proves almost nothing about a fault that
# showed up three times in fifty runs. RUNTIME_PLAN.md section 5.10 is the case
# for loops rather than single runs.
#
# Both the flate and the p256 program panic on a wrong answer -- a decompression
# mismatch and "signature did not verify" respectively -- so these are
# correctness loops, not merely crash loops.
#
#   fast   enough repetitions to catch a gross regression, inside the fast
#          tier's ten-minute budget. It will NOT reproduce a 1-in-50 fault; see
#          the coverage table in docs/verification.md.
#   full   the repetition counts the wave gates have used.
set -uo pipefail
cd "$(dirname "$0")/.."

mode=${1:-fast}
work=$(mktemp -d "${TMPDIR:-/tmp}/reducers-XXXXXX")
trap 'rm -rf "$work"' EXIT

goc="$work/goc"
go build -o "$goc" ./cmd/goc || exit 1

# program : GOGC : extra env : fast reps : full reps
cases=(
	"goc/testdata/runtime_gc_type_mask_padding.go:100:GOMAXPROCS=3:5:20"
	"goc/testdata/runtime_gc_type_mask_padding.go:10:GOMAXPROCS=3:5:20"
	"goc/testdata/placement_bench/flate/main.go:100::30:250"
	"goc/testdata/placement_bench/flate/main.go:10::30:250"
	"goc/testdata/placement_bench/p256/main.go:10::20:100"
	"goc/testdata/runtime_lock_osthread.go:100::40:400"
)

failures=0
for entry in "${cases[@]}"; do
	IFS=: read -r program gogc extra fast_reps full_reps <<<"$entry"
	reps=$fast_reps
	[ "$mode" = full ] && reps=$full_reps

	name=$(basename "$(dirname "$program")")/$(basename "$program" .go)
	exe="$work/$(echo "$name-$gogc" | tr / -)"
	if ! "$goc" -O -o "$exe" "$program" >"$work/compile.log" 2>&1; then
		echo "REDUCER FAIL  $name: compile with -O failed"
		sed 's/^/    /' "$work/compile.log"
		failures=$((failures + 1))
		continue
	fi

	# The repetitions are independent, so they run concurrently; the point is
	# the number of executions, not the elapsed time of a serial loop.
	environment="GOGC=$gogc ${extra:-}"
	bad=$(seq "$reps" | xargs -P "$(nproc)" -I{} sh -c \
		'env '"$environment"' "$0" >/dev/null 2>&1 || printf x' "$exe" | wc -c)
	bad=${bad// /}

	if [ "$bad" != 0 ]; then
		echo "REDUCER FAIL  $name GOGC=$gogc: $bad of $reps runs failed"
		failures=$((failures + 1))
	else
		echo "reducer ok    $name GOGC=$gogc: 0 of $reps runs failed"
	fi
done

if [ "$failures" != 0 ]; then
	echo "reducers: $failures case(s) failed"
	exit 1
fi
echo "reducers ($mode): all cases clean"
