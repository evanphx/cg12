#!/usr/bin/env bash
# Compile the whole testdata corpus against a cache filled by ONE other program,
# and report how many link and how many match their own cold image.
#
# This is the cross-program property at corpus scale, and it is the one the
# per-declaration cold/warm pair in scripts/function-cache-check.sh cannot see. A
# program compiled warm against its OWN cache contains, by construction, whatever
# minted every interned artifact it references; a program compiled against a cache
# some OTHER program filled does not, and before goc/functionstore.go's artifact
# journal 357 of 408 corpus programs failed to link because of it.
#
# Usage: scripts/function-cache-corpus-check.sh [-fill prog.go] [-jobs N] [-limit N]
#
# Each program gets its own COPY of the filled directory, so what is measured is
# "a cache another program filled" and not "a cache many programs are writing to
# at once" -- that is the concurrency arm, and it is separate on purpose.
set -u

fill=fmt_sprintf.go
jobs=24
limit=0
while [ $# -gt 0 ]; do
	case "$1" in
	-fill) fill="$2"; shift 2 ;;
	-jobs) jobs="$2"; shift 2 ;;
	-limit) limit="$2"; shift 2 ;;
	*) echo "function-cache-corpus-check: unknown flag $1" >&2; exit 2 ;;
	esac
done

work="${TMPDIR:-/tmp}/function-cache-corpus-$$"
mkdir -p "$work"
trap 'rm -rf "$work"' EXIT

go build -o "$work/goc" ./cmd/goc || exit 1

# The filled directory, from one program and nothing else.
mkdir -p "$work/filled"
CG12_FUNC_CACHE="$work/filled" "$work/goc" -o "$work/fill.bin" "goc/testdata/$fill" >/dev/null 2>&1 || {
	echo "function-cache-corpus-check: the filling build failed"
	exit 1
}

programs=(goc/testdata/*.go)
if [ "$limit" -gt 0 ]; then
	programs=("${programs[@]:0:$limit}")
fi
echo "function-cache-corpus-check: ${#programs[@]} programs against a cache filled by $fill"

check() {
	local source="$1" name work="$2"
	name=$(basename "$source" .go)
	local cache="$work/c.$name"
	cp -r "$work/filled" "$cache" || { echo "SETUP $name"; return; }
	if ! CG12_NOCACHE=1 "$work/goc" -o "$work/cold.$name" "$source" >/dev/null 2>&1; then
		# Not a program this tree compiles at all; it is not the cache's business.
		echo "SKIP $name"
		rm -rf "$cache" "$work/cold.$name"
		return
	fi
	if ! CG12_FUNC_CACHE="$cache" "$work/goc" -o "$work/warm.$name" "$source" >/dev/null 2>&1; then
		echo "FAIL-BUILD $name"
		rm -rf "$cache" "$work/cold.$name" "$work/warm.$name"
		return
	fi
	if cmp -s "$work/cold.$name" "$work/warm.$name"; then
		echo "OK $name"
	else
		echo "DIFFER $name"
	fi
	rm -rf "$cache" "$work/cold.$name" "$work/warm.$name"
}
export -f check

printf '%s\n' "${programs[@]}" |
	xargs -P "$jobs" -I{} bash -c 'check "$@"' _ {} "$work" >"$work/results" 2>&1

ok=$(grep -c '^OK ' "$work/results" || true)
differ=$(grep -c '^DIFFER ' "$work/results" || true)
failed=$(grep -c '^FAIL-BUILD ' "$work/results" || true)
skipped=$(grep -c '^SKIP ' "$work/results" || true)
setup=$(grep -c '^SETUP ' "$work/results" || true)
echo "function-cache-corpus-check: $ok identical, $differ different, $failed failed to build, $skipped not compilable cold, $setup setup errors"
grep -E '^(DIFFER|FAIL-BUILD|SETUP) ' "$work/results" | sort | head -40
if [ "$differ" -gt 0 ] || [ "$failed" -gt 0 ] || [ "$setup" -gt 0 ]; then
	exit 1
fi
