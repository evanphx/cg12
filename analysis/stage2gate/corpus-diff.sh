#!/usr/bin/env bash
# Compare two corpus-run.sh manifests and report per-program differences.
# usage: corpus-diff.sh a.sha256 b.sha256 "label"
set -u
a="$1"; b="$2"; label="${3:-}"
same=0; different=0; failed=0
while read -r ha na; do
	hb=$(awk -v n="$na" '$2==n {print $1}' "$b")
	if [ -z "$hb" ]; then
		echo "MISSING-IN-B $na"; different=$((different + 1)); continue
	fi
	if [ "$ha" = "COMPILE-FAILED" ] || [ "$hb" = "COMPILE-FAILED" ]; then
		if [ "$ha" = "$hb" ]; then
			failed=$((failed + 1))
		else
			echo "FAIL-MISMATCH $na a=$ha b=$hb"; different=$((different + 1))
		fi
		continue
	fi
	if [ "$ha" = "$hb" ]; then
		same=$((same + 1))
	else
		echo "DIFFERENT $na a=${ha:0:16} b=${hb:0:16}"; different=$((different + 1))
	fi
done <"$a"
echo "corpus-diff${label:+ [$label]}: identical=$same different=$different both-failed=$failed"
[ "$different" -eq 0 ]
