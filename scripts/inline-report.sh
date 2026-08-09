#!/usr/bin/env bash
#
# inline-report.sh — what the inliner decided, and what came out.
#
# WHY THIS EXISTS
#
# bench-c.sh answers "did this get faster", which is the question that matters
# and the slowest one to ask: seven rounds over three arms, and a difference
# under about a percent is below the instrument. That is a bad loop to iterate a
# heuristic in, and worse, a timing number does not say WHY it moved. A change
# that inlines three hundred more call sites and a change that inlines two both
# read as "no change" if neither is worth a percent.
#
# So this reports the decisions themselves. It is fast, exact, and diffable: run
# it before a change and after, diff the two, and the output names which rule
# started or stopped firing and on which callees. Timing still decides whether a
# change is kept; this decides whether it did what it was supposed to.
#
# WHAT THE NUMBERS MEAN
#
# The decision census counts EVALUATIONS, not call sites. The optimizer runs
# inlining in a fixpoint, so a call site that survives four rounds is counted
# four times, and a callee that gets inlined stops being counted at all. That
# makes the absolute numbers larger than the program and not worth quoting on
# their own -- but it is the same distortion before and after a change, which is
# all a diff needs. The object statistics underneath (text bytes, functions left,
# call instructions emitted) are exact, and are what a claim should quote.
#
# Functions left is the one to watch. gcc leaves 974 functions in QuickJS and
# cg12 leaves 1458: the ones gcc deleted are the ones it inlined at every site.
#
# USAGE
#
#   scripts/inline-report.sh                     # all three, to stdout
#   scripts/inline-report.sh -workload quickjs
#   scripts/inline-report.sh -o before.txt       # then diff after.txt
#
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
BENCH="$ROOT/cc/testdata/bench"
. "$ROOT/scripts/workloads.sh"

WORKLOAD=all
OUT=""
TOPN=6

while [ $# -gt 0 ]; do
	case "$1" in
	-workload) WORKLOAD=$2; shift 2 ;;
	-o) OUT=$2; shift 2 ;;
	-top) TOPN=$2; shift 2 ;;
	-h | -help | --help) sed -n '2,40p' "$0"; exit 0 ;;
	*) echo "unknown flag $1" >&2; exit 2 ;;
	esac
done

note() { echo "[inline-report] $*" >&2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

note "building cg12cc from the working tree"
if ! go build -o "$WORK/cc" ./cmd/cg12cc; then
	note "FAIL: the working tree does not build"
	exit 1
fi

targets=$(workload_names)
if [ "$WORKLOAD" != all ]; then
	targets=$WORKLOAD
fi

report() {
	echo "# inline-report $(git rev-parse --short HEAD 2>/dev/null || echo '?')$(git diff --quiet 2>/dev/null || echo '+dirty')"
	for name in $targets; do
		if ! workload_config "$name"; then
			note "unknown workload $name"
			continue
		fi
		if [ ! -f "$SRC" ]; then
			note "skipping $name: $SRC is absent (run scripts/fetch-bench-c.sh)"
			continue
		fi
		local obj="$WORK/$name.o" why="$WORK/$name.why" start elapsed
		start=$(date +%s.%N)
		if ! (cd "$INCLUDE" && CG12_INLINE_WHY='*' "$WORK/cc" -O2 -fwrapv "${DEFINES[@]}" \
			-c -o "$obj" "$SRC") 2>"$why" >"$WORK/$name.build"; then
			note "FAIL: $name did not compile (see $WORK/$name.build)"
			continue
		fi
		elapsed=$(echo "$(date +%s.%N) - $start" | bc)

		echo
		echo "=== $name"
		printf 'compile      %.1fs\n' "$elapsed"
		printf 'text         %s bytes\n' "$(size -A "$obj" | awk '/^\.text/{print $2}')"
		printf 'functions    %s\n' "$(nm "$obj" | grep -cE ' [Tt] ')"
		printf 'calls        %s\n' "$(objdump -d "$obj" | grep -cE '\s(bl|blr)\s')"

		# A verdict is either INLINE or every rule that refused, joined by " + ".
		# Two columns, because they answer different questions: "blocks" counts a
		# rule wherever it fires, and "alone" counts only where it is the single
		# thing standing in the way. Lifting a rule frees the second number, never
		# the first -- lifting the SCC gate on a census that only knew the first
		# looked like 23,758 sites and moved nothing.
		grep -- '->' "$why" | sed 's/.*sites=[0-9]* *//' >"$why.verdicts"
		echo 'decisions (evaluations, not call sites):'
		printf '  %8d  INLINE\n' "$(grep -cx 'INLINE' "$why.verdicts")"
		awk -F ' \\+ ' '
			$0 == "INLINE" { next }
			{ for (i = 1; i <= NF; i++) blocks[$i]++; if (NF == 1) alone[$1]++ }
			END { for (r in blocks) printf "%8d %8d %s\n", blocks[r], alone[r] + 0, r }
		' "$why.verdicts" | sort -rn |
			awk '{n=$1; a=$2; $1=""; $2=""; sub(/^  /,""); printf "  %8d  (%6d alone)  %s\n", n, a, $0}'

		echo "top callees each rule is the ONLY thing blocking (site count at the time):"
		awk -F ' \\+ ' 'NF == 1 && $0 != "INLINE" { print }' "$why.verdicts" | sort -u |
			while read -r reason; do
				printf '  %s\n' "$reason"
				grep -- '->' "$why" | awk -v r="$reason" -F ' \\+ ' 'NF == 1' |
					grep -F -- "$reason" |
					awk '{print $4, $5, $6}' | sed 's/size=//;s/sites=//' | sort -u |
					sort -k3 -rn | head -"$TOPN" |
					awk '{printf "      %-36s size=%-6s sites=%s\n", $1, $2, $3}'
			done
	done
}

if [ -n "$OUT" ]; then
	report >"$OUT"
	note "wrote $OUT"
else
	report
fi
