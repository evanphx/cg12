#!/usr/bin/env bash
# Compile programs cold and then warm, in separate processes, and report whether
# the linked images are byte-identical and what the cache cost or saved.
#
# Usage: scripts/function-cache-check.sh [-programs "a.go b.go"] [-rounds N]
#
# This is the correctness property of the per-function cache and the only form of
# it that is worth much. A same-process cold/warm pair (goc/functioncachestore_test.go)
# proves the store round-trips; it cannot see a key that accidentally depends on
# something the process already had in memory -- a parsed file, a shared source
# world, a sync.Once. Two processes can.
#
# Three builds per program per -O arm:
#
#   nocache  CG12_NOCACHE=1, the control: no function cache and no pack cache.
#   cold     an empty CG12_FUNC_CACHE directory. Fills it. Must equal nocache.
#   warm     the same directory, now full. Must equal cold.
#
# The nocache arm is what makes this a test of the cache rather than a test of
# determinism: if all three agree, a cached compile produced the image an
# uncached compile would have.
set -u

rounds=1
programs=(
	hello.go
	fmt_sprintf.go
	gc_struct.go
	stdlib_http_tls_client_server.go
)
while [ $# -gt 0 ]; do
	case "$1" in
	-programs)
		read -r -a programs <<<"$2"
		shift 2
		;;
	-rounds)
		rounds="$2"
		shift 2
		;;
	*)
		echo "function-cache-check: unknown flag $1" >&2
		exit 2
		;;
	esac
done

work="${TMPDIR:-/tmp}/function-cache-$$"
mkdir -p "$work"
trap 'rm -rf "$work"' EXIT

go build -o "$work/goc" ./cmd/goc || exit 1

failures=0
printf '%-44s %-4s %-9s %-9s %-9s %s\n' program -O nocache cold warm verdict

elapsed() { # command...; prints seconds with two decimals
	local start end
	start=$(date +%s.%N)
	"$@" >/dev/null 2>&1 || return 1
	end=$(date +%s.%N)
	awk -v a="$start" -v b="$end" 'BEGIN{printf "%.2f", b-a}'
}

for program in "${programs[@]}"; do
	source="goc/testdata/$program"
	if [ ! -f "$source" ]; then
		printf '%-44s MISSING SOURCE\n' "$program"
		failures=$((failures + 1))
		continue
	fi
	for arm in "" "-O"; do
		for round in $(seq 1 "$rounds"); do
			cache="$work/cache.$program.$arm.$round"
			rm -rf "$cache"
			mkdir -p "$cache"

			nocacheTime=$(CG12_NOCACHE=1 CG12_FUNC_CACHE="$cache" elapsed "$work/goc" ${arm:+$arm} -o "$work/nocache" "$source") ||
				{ printf '%-44s %-4s NOCACHE-BUILD-FAILED\n' "$program" "${arm:--}"; failures=$((failures + 1)); continue; }
			coldTime=$(CG12_FUNC_CACHE="$cache" elapsed "$work/goc" ${arm:+$arm} -o "$work/cold" "$source") ||
				{ printf '%-44s %-4s COLD-BUILD-FAILED\n' "$program" "${arm:--}"; failures=$((failures + 1)); continue; }
			warmTime=$(CG12_FUNC_CACHE="$cache" elapsed "$work/goc" ${arm:+$arm} -o "$work/warm" "$source") ||
				{ printf '%-44s %-4s WARM-BUILD-FAILED\n' "$program" "${arm:--}"; failures=$((failures + 1)); continue; }

			n=$(sha256sum "$work/nocache" | cut -c1-16)
			c=$(sha256sum "$work/cold" | cut -c1-16)
			w=$(sha256sum "$work/warm" | cut -c1-16)
			if [ "$n" = "$c" ] && [ "$c" = "$w" ]; then
				verdict="identical($n)"
			else
				verdict="DIFFERENT nocache=$n cold=$c warm=$w"
				failures=$((failures + 1))
			fi
			printf '%-44s %-4s %-9s %-9s %-9s %s\n' \
				"$program" "${arm:--}" "$nocacheTime" "$coldTime" "$warmTime" "$verdict"
		done
	done
done

if [ "$failures" -gt 0 ]; then
	echo "function-cache-check: $failures failure(s)"
	exit 1
fi
echo "function-cache-check: all builds identical"
