#!/usr/bin/env bash
# Compare a cold (CG12_NOCACHE=1) build against a warm one, twice, for a sample
# of programs, and report whether the linked images are byte-identical.
#
# Usage: scripts/determinism-check.sh [-runtime <pack>]
#
# runtime_defer_capture_allocs.go is a known backend residue (RUNTIME_PLAN.md
# section 5.10): it is expected to differ. Everything else must match.
set -u

extra=("$@")
work="${TMPDIR:-/tmp}/determinism-$$"
mkdir -p "$work"
trap 'rm -rf "$work"' EXIT

go build -o "$work/goc" ./cmd/goc || exit 1

programs=(
	hello.go
	fmt_sprintf.go
	gc_struct.go
	runtime_cleanup_frame_retention.go
	runtime_defer_capture_allocs.go
)

for program in "${programs[@]}"; do
	source="goc/testdata/$program"
	if [ ! -f "$source" ]; then
		echo "$program: MISSING SOURCE"
		continue
	fi
	verdict=""
	for round in 1 2; do
		cold="$work/$program.cold.$round"
		warm="$work/$program.warm.$round"
		CG12_NOCACHE=1 "$work/goc" "${extra[@]}" -o "$cold" "$source" >/dev/null 2>&1 || { verdict="$verdict round$round:COLD-BUILD-FAILED "; continue; }
		"$work/goc" "${extra[@]}" -o "$warm" "$source" >/dev/null 2>&1 || { verdict="$verdict round$round:WARM-BUILD-FAILED "; continue; }
		coldHash=$(sha256sum "$cold" | cut -c1-16)
		warmHash=$(sha256sum "$warm" | cut -c1-16)
		if [ "$coldHash" = "$warmHash" ]; then
			verdict="$verdict round$round:identical($coldHash) "
		else
			verdict="$verdict round$round:DIFFERENT($coldHash/$warmHash) "
		fi
	done
	printf '%-40s %s\n' "$program" "$verdict"
done
