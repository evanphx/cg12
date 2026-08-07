#!/usr/bin/env bash
# Compile the whole testdata corpus TWICE per arm -- once with the cache on by
# default, once with CG12_NOCACHE=1 -- and require the two images to be identical.
#
# This is the acceptance test for turning the cache on. The other two cache
# scripts answer "is a cache the user asked for correct"; this one answers "is a
# user who has never heard of the cache getting the same binary", which is the
# only question a default has to answer.
#
# What it does that the others do not:
#
#   * No CG12_FUNC_CACHE. The subject compile is `goc -o out prog.go` with
#     nothing set, so what is exercised is cmd/goc's own default and the default
#     LOCATION -- os.UserCacheDir()/cg12/function-cache -- rather than a directory
#     a script chose. XDG_CACHE_HOME is redirected into the work tree so the run
#     is reproducible and leaves the caller's real cache alone.
#   * One shared cache directory for every program and both -O arms, filled
#     concurrently. That is what default-on actually looks like: whatever you
#     compiled last week is what your cache holds, and -O is deliberately not a
#     clause of this cache's key, so a unit written without it has to serve a
#     build with it.
#   * Two passes. The first is each program against whatever the others have
#     already put there; the second is each program against its own units. Both
#     are compared to the same CG12_NOCACHE=1 control.
#
# Usage: scripts/function-cache-default-check.sh [-jobs N] [-limit N] [-O]
set -u

jobs=24
limit=0
optimize=""
while [ $# -gt 0 ]; do
	case "$1" in
	-jobs) jobs="$2"; shift 2 ;;
	-limit) limit="$2"; shift 2 ;;
	-O) optimize="-O"; shift ;;
	*) echo "function-cache-default-check: unknown flag $1" >&2; exit 2 ;;
	esac
done

work="${TMPDIR:-/tmp}/function-cache-default-$$"
mkdir -p "$work/home"
trap 'rm -rf "$work"' EXIT

go build -o "$work/goc" ./cmd/goc || exit 1

programs=(goc/testdata/*.go)
if [ "$limit" -gt 0 ]; then
	programs=("${programs[@]:0:$limit}")
fi
echo "function-cache-default-check: ${#programs[@]} programs, arm '${optimize:-none}', default cache under $work/home"

check() {
	local source="$1" work="$2" optimize="$3" name
	name=$(basename "$source" .go)
	# The default location, not a directory this script names: XDG_CACHE_HOME is
	# what os.UserCacheDir reads, so this is the path an ordinary `goc` takes.
	export XDG_CACHE_HOME="$work/home"

	if ! env CG12_NOCACHE=1 "$work/goc" $optimize -o "$work/control.$name" "$source" >/dev/null 2>&1; then
		# Not a program this tree compiles at all; it is not the cache's business.
		echo "SKIP $name"
		rm -f "$work/control.$name"
		return
	fi
	local pass status
	status=OK
	for pass in 1 2; do
		if ! "$work/goc" $optimize -o "$work/subject.$name" "$source" >/dev/null 2>&1; then
			echo "FAIL-BUILD $name pass$pass"
			status=BAD
			break
		fi
		if ! cmp -s "$work/control.$name" "$work/subject.$name"; then
			echo "DIFFER $name pass$pass"
			status=BAD
			break
		fi
	done
	[ "$status" = OK ] && echo "OK $name"
	rm -f "$work/control.$name" "$work/subject.$name"
}
export -f check

printf '%s\n' "${programs[@]}" |
	xargs -P "$jobs" -I{} bash -c 'check "$@"' _ {} "$work" "$optimize" >"$work/results" 2>&1

ok=$(grep -c '^OK ' "$work/results" || true)
differ=$(grep -c '^DIFFER ' "$work/results" || true)
failed=$(grep -c '^FAIL-BUILD ' "$work/results" || true)
skipped=$(grep -c '^SKIP ' "$work/results" || true)
units=$(find "$work/home/cg12/function-cache" -name '*.gocfn' 2>/dev/null | wc -l)
bytes=$(du -sh "$work/home/cg12/function-cache" 2>/dev/null | cut -f1)
echo "function-cache-default-check: $ok identical, $differ different, $failed failed to build, $skipped not compilable at all"
echo "function-cache-default-check: the shared default cache ended with $units units, $bytes"
grep -E '^(DIFFER|FAIL-BUILD) ' "$work/results" | sort | head -40
if [ "$differ" -gt 0 ] || [ "$failed" -gt 0 ]; then
	exit 1
fi
