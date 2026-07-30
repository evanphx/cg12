#!/usr/bin/env bash
# Run the runtime capability matrix once and report the three terms that bound
# its wall clock: the slowest single compile, the total compile CPU, and the
# run phase.
#
# Usage: scripts/matrix-timing.sh <label> [extra test-binary flags...]
#
# The verbose log is kept so a measurement can be re-derived without re-running.
set -u

label="$1"
shift

out="${MATRIX_TIMING_DIR:-${TMPDIR:-/tmp}}/matrix-$label.log"

start=$(date +%s.%N)
go test -count=1 -v -timeout 60m -run '^TestARM64RuntimeCapabilityStatus$' ./cmd/goc \
	-args -runtime-status-progress "$@" >"$out" 2>&1
status=$?
end=$(date +%s.%N)

wall=$(echo "$end - $start" | bc)

subtests=$(grep -cE '^=== RUN   TestARM64RuntimeCapabilityStatus/' "$out")
passes=$(grep -cE '^ *--- PASS: TestARM64RuntimeCapabilityStatus/' "$out")
fails=$(grep -cE '^ *--- FAIL: TestARM64RuntimeCapabilityStatus/' "$out")
skips=$(grep -cE '^ *--- SKIP: TestARM64RuntimeCapabilityStatus/' "$out")
declaredPass=$(grep -cE '^ +[a-z_]*\.go:[0-9]+: PASS ' "$out")
expectedFail=$(grep -cE 'EXPECTED FAILURE ' "$out")
knownGap=$(grep -cE 'KNOWN GAP ' "$out")

awk -v label="$label" -v wall="$wall" -v status="$status" \
	-v subtests="$subtests" -v passes="$passes" -v fails="$fails" -v skips="$skips" \
	-v declaredPass="$declaredPass" -v expectedFail="$expectedFail" -v knownGap="$knownGap" '
	function duration(text,   parts) {
		if (text ~ /ms$/) { sub(/ms$/, "", text); return text / 1000 }
		if (text ~ /µs$/) { sub(/µs$/, "", text); return text / 1000000 }
		if (text ~ /^[0-9]+m/) {
			split(text, parts, "m")
			sub(/s$/, "", parts[2])
			return parts[1] * 60 + parts[2]
		}
		sub(/s$/, "", text)
		return text + 0
	}
	/runtime-status: (pass|fail) / {
		# runtime-status: <status> <cat>/<name> compile=<outcome> <dur> peak=..
		#                 run=<outcome> <dur> peak=.. coverage=..
		for (i = 1; i <= NF; i++) {
			if ($i ~ /^runtime-status:$/) { base = i; break }
		}
		name = $(base + 2)
		compileSeconds = duration($(base + 4))
		runSeconds = duration($(base + 7))
		programs++
		compileTotal += compileSeconds
		runTotal += runSeconds
		if (compileSeconds > slowestCompile) {
			slowestCompile = compileSeconds
			slowestCompileName = name
		}
		if (runSeconds > slowestRun) {
			slowestRun = runSeconds
			slowestRunName = name
		}
	}
	END {
		printf "label=%s wall=%.1f exit=%d subtests=%d pass=%d fail=%d skip=%d declaredPASS=%d expectedFAILURE=%d knownGAP=%d\n",
			label, wall, status, subtests, passes, fails, skips, declaredPass, expectedFail, knownGap
		printf "  programs=%d compile_cpu=%.1f slowest_compile=%.1f (%s) run_total=%.1f slowest_run=%.1f (%s)\n",
			programs, compileTotal, slowestCompile, slowestCompileName, runTotal, slowestRun, slowestRunName
	}
' "$out"

echo "  log: $out"
exit $status
