#!/usr/bin/env bash
# Compile the same source repeatedly and report whether the linked images are
# byte-identical.
#
# Usage: scripts/determinism-check.sh [-corpus] [-rounds N] [-j N] [goc flags...]
#
# Default: the five sample programs, each built cold and warm, twice, so a
# disagreement between the two caching paths shows up as well as one between two
# runs of the same path. All five must match; the four hashes printed for a
# program must all be equal, across rounds as well as within one.
#
# Cold is an empty pack cache and warm is a hit in the same cache, both with the
# pack cache doing what it does by default. It used to be `CG12_NOCACHE=1`
# against a plain compile, and that stopped meaning "cold against warm" when the
# pack cache became the default: `CG12_NOCACHE=1` now takes the whole-program
# path, so the two arms were two different compiles and the script reported all
# five programs DIFFERENT on a tree where nothing was wrong. Each round gets its
# own cache directory, so cold is cold in round 2 as well as round 1.
#
# Deliberately not compared here: the split image against the whole-program one
# (`GOC_AUTOPACK=0`). Those two differ by construction and always will -- they
# are different modules -- so a difference between them is not a determinism
# result. What matters, and is checked, is that each of them is reproducible.
#
# -corpus: every goc/testdata/*.go program, -rounds compiles each (default 4),
# driven through `goc compile-batch` workers so the whole corpus fits in minutes.
# Reports per program how many distinct images came out, and separates a layout
# residue (same content digest, different image) from a real difference in
# generated code. Exits non-zero if any program varies.
#
# Five programs is a weak sample on its own: RUNTIME_PLAN.md section 5.10 records
# a program that took its minority branch 3 times in 53 compiles, so a handful of
# agreeing compiles is not evidence. -corpus is the measurement that carries
# weight. runtime_defer_capture_allocs.go used to be the standing exception -- 25
# distinct executables in 30 compiles -- and section 5.10 records what it turned
# out to be.
set -u

corpus=0
rounds=4
jobs=$(nproc)
while [ $# -gt 0 ]; do
	case "$1" in
	-corpus)
		corpus=1
		shift
		;;
	-rounds)
		rounds="$2"
		shift 2
		;;
	-j)
		jobs="$2"
		shift 2
		;;
	*)
		break
		;;
	esac
done

extra=("$@")
work="${TMPDIR:-/tmp}/determinism-$$"
mkdir -p "$work"
trap 'rm -rf "$work"' EXIT

go build -o "$work/goc" ./cmd/goc || exit 1

if [ "$corpus" = 1 ]; then
	go build -o "$work/determinism" ./analysis/determinism || exit 1
	sweep=("$work/determinism" -goc "$work/goc" -work "$work" -j "$jobs" -rounds "$rounds")
	for flag in "${extra[@]+"${extra[@]}"}"; do
		case "$flag" in
		-O) sweep+=(-O) ;;
		*)
			echo "determinism-check: -corpus does not pass through $flag" >&2
			exit 2
			;;
		esac
	done
	exec "${sweep[@]}" goc/testdata/*.go
fi

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
		cache="$work/cache.$program.$round"
		mkdir -p "$cache"
		CG12_PACK_CACHE="$cache" "$work/goc" "${extra[@]+"${extra[@]}"}" -o "$cold" "$source" >/dev/null 2>&1 || { verdict="$verdict round$round:COLD-BUILD-FAILED "; continue; }
		CG12_PACK_CACHE="$cache" "$work/goc" "${extra[@]+"${extra[@]}"}" -o "$warm" "$source" >/dev/null 2>&1 || { verdict="$verdict round$round:WARM-BUILD-FAILED "; continue; }
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
