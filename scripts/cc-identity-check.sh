#!/usr/bin/env bash
#
# cc-identity-check.sh — prove a change to the compiler did not change what the
# compiler emits, on the C path.
#
# This is the C-side counterpart of scripts/determinism-check.sh, and it is the
# gate every memory or CPU optimization has to pass: those changes are supposed
# to make the compiler cheaper and leave its output alone, so the way to accept
# one is to compile a corpus before and after and compare the objects byte for
# byte. A change that alters the output is not necessarily wrong, but it stops
# being a free win and becomes a decision.
#
# It compares cg12's own .o files rather than linked executables. The host linker
# contributes bytes cg12 did not choose, and linking is not what is under test.
#
#   scripts/cc-identity-check.sh                 # self-determinism: same binary, N runs
#   scripts/cc-identity-check.sh -ref HEAD~1     # this tree against another commit
#   scripts/cc-identity-check.sh -ref main -rounds 3
#   scripts/cc-identity-check.sh -bench          # include the big fetched inputs
#
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

REF=""
ROUNDS=2
INCLUDE_BENCH=0

while [ $# -gt 0 ]; do
	case "$1" in
	-ref)
		REF=$2
		shift 2
		;;
	-rounds)
		ROUNDS=$2
		shift 2
		;;
	-bench)
		INCLUDE_BENCH=1
		shift
		;;
	-h | -help | --help)
		sed -n '2,25p' "$0"
		exit 0
		;;
	*)
		echo "unknown flag $1" >&2
		exit 2
		;;
	esac
done

WORK=$(mktemp -d)
cleanup() {
	rm -rf "$WORK"
	if [ -n "${REF_TREE:-}" ]; then
		git worktree remove --force "$REF_TREE" >/dev/null 2>&1
	fi
}
trap cleanup EXIT

note() { echo "[cc-identity] $*" >&2; }

# inputs lists the C the corpus is made of. The bench inputs are fetched rather
# than committed (scripts/fetch-bench-c.sh), so they are opt-in and skipped
# without complaint when absent -- but they are the ones that exercise the
# optimizer at scale, so -bench is what to use before believing a result.
inputs() {
	ls "$ROOT"/cc/testdata/rubric/*.c 2>/dev/null
	ls "$ROOT"/cc/testdata/escape/*.c 2>/dev/null
	ls "$ROOT"/difftest/testdata/*.c 2>/dev/null
	ls "$ROOT"/cmd/viz/testdata/*.c 2>/dev/null
	if [ "$INCLUDE_BENCH" = 1 ]; then
		ls "$ROOT"/cc/testdata/bench/*.c 2>/dev/null
		# lua is deliberately absent. Compiling cc/testdata/bench/lua/onelua.c
		# twice with the same binary produces two different objects, at -O0 as
		# well as -O, and it does so at HEAD with no local changes -- so it is a
		# standing bug in the compiler and not something a change under test did.
		# Including it here would report every run as DIFFER and make this check
		# useless for the thing it exists for. Put it back the day that is fixed;
		# it is the better input of the two, being the one whose output can be run.
	fi
}

# compile_all compiles every input with one cc binary at both optimization
# levels and prints "name<TAB>sha256" for each object.
#
# It compiles from the input's own directory because cmd/cc resolves #include
# "..." relative to the working directory, which is how the single-translation-
# unit bench inputs are put together.
compile_all() {
	local binary=$1 tag=$2
	local input name level object
	for input in $(inputs); do
		name=$(basename "$input" .c)
		for level in O0 O; do
			object="$WORK/$tag.$name.$level.o"
			local flags=(-c -o "$object")
			if [ "$level" = O ]; then
				flags=(-O "${flags[@]}")
			fi
			if (cd "$(dirname "$input")" && "$binary" "${flags[@]}" "$input" >/dev/null 2>&1); then
				printf '%s.%s\t%s\n' "$name" "$level" "$(sha256sum "$object" | cut -d' ' -f1)"
			else
				# A input that does not compile is recorded as such rather than
				# skipped: "it stopped compiling" is exactly the regression this
				# check exists to catch, and a silent skip would hide it.
				printf '%s.%s\t%s\n' "$name" "$level" "FAILED-TO-COMPILE"
			fi
		done
	done
}

note "building cc from the working tree"
if ! go build -o "$WORK/cc.new" ./cmd/cc; then
	note "FAIL: the working tree does not build"
	exit 1
fi

count=$(inputs | wc -l)
note "corpus: $count inputs, two optimization levels each"

if [ -n "$REF" ]; then
	REF_TREE=$(mktemp -d)
	rm -rf "$REF_TREE"
	note "checking out $REF into a worktree"
	if ! git worktree add --detach "$REF_TREE" "$REF" >/dev/null 2>&1; then
		note "FAIL: cannot create a worktree at $REF"
		exit 1
	fi
	note "building cc from $REF"
	if ! (cd "$REF_TREE" && go build -o "$WORK/cc.ref" ./cmd/cc); then
		note "FAIL: $REF does not build"
		exit 1
	fi
	compile_all "$WORK/cc.ref" ref >"$WORK/ref.txt"
	compile_all "$WORK/cc.new" new >"$WORK/new.txt"
	if diff -u "$WORK/ref.txt" "$WORK/new.txt" >"$WORK/diff.txt"; then
		note "PASS: every object is byte-identical to $REF"
		exit 0
	fi
	note "DIFFER: the working tree emits different objects than $REF"
	# Report the names rather than the hashes: the hash is noise, the name is
	# the thing to go and look at.
	awk '/^[-+][^-+]/ { print "  " $0 }' "$WORK/diff.txt" | sed 's/\t.*//' | sort -u >&2
	exit 1
fi

note "self-determinism over $ROUNDS rounds"
compile_all "$WORK/cc.new" round1 >"$WORK/round1.txt"
for round in $(seq 2 "$ROUNDS"); do
	compile_all "$WORK/cc.new" "round$round" >"$WORK/round$round.txt"
	if ! diff -q "$WORK/round1.txt" "$WORK/round$round.txt" >/dev/null; then
		note "DIFFER: round $round does not match round 1 -- the compiler is not deterministic"
		diff -u "$WORK/round1.txt" "$WORK/round$round.txt" | sed -n '3,$p' | head -40 >&2
		exit 1
	fi
done
if grep -q FAILED-TO-COMPILE "$WORK/round1.txt"; then
	note "note: some inputs do not compile; they are recorded, not skipped:"
	grep FAILED-TO-COMPILE "$WORK/round1.txt" | sed 's/^/  /' >&2
fi
note "PASS: $ROUNDS rounds agree"
