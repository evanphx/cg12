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
#
# Then a fourth build per ORDERED PAIR of programs: fill a directory with A, and
# compile B against it. That is the arm the same-program pair cannot stand in for.
# A program compiled warm against its own cache contains, by construction,
# whichever declaration minted every interned artifact it references; a program
# compiled against a cache another program filled does not. Before the artifact
# journal in goc/functionstore.go the reduced form of that was two commands:
#
#   CG12_FUNC_CACHE=/tmp/c goc -o a goc/testdata/fmt_sprintf.go   # fills
#   CG12_FUNC_CACHE=/tmp/c goc -o b goc/testdata/hello.go         # undefined _goc_type_time_Time_...
#
# Both orders are run, because the two fail differently: one way the reference
# dangles and the program does not link, and the other way it links and produces a
# different image, since Module.Data order is the order data is laid out in.
#
# scripts/function-cache-corpus-check.sh is the same property at corpus scale.
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

# The cross-program arm: every ordered pair, filled by one and used by the other.
printf '\n%-24s %-24s %-9s %s\n' filled-by compiled verdict note
for filler in "${programs[@]}"; do
	fillerSource="goc/testdata/$filler"
	[ -f "$fillerSource" ] || continue
	for subject in "${programs[@]}"; do
		[ "$subject" = "$filler" ] && continue
		subjectSource="goc/testdata/$subject"
		[ -f "$subjectSource" ] || continue

		cache="$work/cross.$filler.$subject"
		rm -rf "$cache"
		mkdir -p "$cache"
		if ! CG12_FUNC_CACHE="$cache" "$work/goc" -o "$work/filler" "$fillerSource" >/dev/null 2>&1; then
			printf '%-24s %-24s %s\n' "$filler" "$subject" "FILL-BUILD-FAILED"
			failures=$((failures + 1))
			continue
		fi
		if ! CG12_NOCACHE=1 "$work/goc" -o "$work/crosscold" "$subjectSource" >/dev/null 2>&1; then
			printf '%-24s %-24s %s\n' "$filler" "$subject" "COLD-BUILD-FAILED"
			failures=$((failures + 1))
			continue
		fi
		if ! CG12_FUNC_CACHE="$cache" "$work/goc" -o "$work/crosswarm" "$subjectSource" >/dev/null 2>&1; then
			printf '%-24s %-24s %-9s %s\n' "$filler" "$subject" "FAILED" "did not link against another program's cache"
			failures=$((failures + 1))
			continue
		fi
		c=$(sha256sum "$work/crosscold" | cut -c1-16)
		w=$(sha256sum "$work/crosswarm" | cut -c1-16)
		if [ "$c" = "$w" ]; then
			printf '%-24s %-24s %-9s %s\n' "$filler" "$subject" identical "$c"
		else
			printf '%-24s %-24s %-9s %s\n' "$filler" "$subject" DIFFERENT "cold=$c warm=$w"
			failures=$((failures + 1))
		fi
		rm -rf "$cache"
	done
done

if [ "$failures" -gt 0 ]; then
	echo "function-cache-check: $failures failure(s)"
	exit 1
fi
echo "function-cache-check: all builds identical"
