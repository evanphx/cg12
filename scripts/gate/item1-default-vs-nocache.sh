#!/usr/bin/env bash
# GATE ITEM 1, written independently of scripts/function-cache-default-check.sh.
#
# Every corpus program, both -O arms, three separate processes each:
#   control  CG12_NOCACHE=1
#   fill     the real default (no CG12_FUNC_CACHE at all), shared cache dir
#   warm     the same, second time
# and, unlike the branch's script, the warm pass runs in REVERSE name order and
# the two arms share one cache directory, so the order in which units were
# written is different from the order they are read back in.
#
# Usage: item1-default-vs-nocache.sh <workdir> <jobs>
set -u
work="$1"; jobs="${2:-24}"
mkdir -p "$work/home" "$work/out"
export XDG_CACHE_HOME="$work/home"
goc="$work/goc"

one() {
	local source="$1" work="$2" arm="$3" phase="$4" name tag
	name=$(basename "$source" .go); tag="$name$arm"
	local opt=""; [ "$arm" = "-O" ] && opt="-O"
	export XDG_CACHE_HOME="$work/home"
	if [ "$phase" = fill ]; then
		if ! env CG12_NOCACHE=1 "$work/goc" $opt -o "$work/out/control.$tag" "$source" >/dev/null 2>&1; then
			echo "SKIP $tag"; rm -f "$work/out/control.$tag"; return
		fi
		sha256sum <"$work/out/control.$tag" | cut -d' ' -f1 >"$work/out/control.$tag.sha"
		rm -f "$work/out/control.$tag"
	fi
	[ -f "$work/out/control.$tag.sha" ] || { echo "SKIP $tag"; return; }
	if ! "$work/goc" $opt -o "$work/out/s.$tag.$phase" "$source" >/dev/null 2>&1; then
		echo "FAILBUILD $tag $phase"; return
	fi
	local got want
	got=$(sha256sum <"$work/out/s.$tag.$phase" | cut -d' ' -f1)
	want=$(cat "$work/out/control.$tag.sha")
	rm -f "$work/out/s.$tag.$phase"
	if [ "$got" != "$want" ]; then echo "DIFFER $tag $phase"; else echo "OK $tag $phase"; fi
}
export -f one

progs_fwd=$(ls goc/testdata/*.go | sort)
progs_rev=$(ls goc/testdata/*.go | sort -r)

: >"$work/results"
for arm in "" "-O"; do
	printf '%s\n' $progs_fwd | xargs -P "$jobs" -I{} bash -c 'one "$@"' _ {} "$work" "$arm" fill >>"$work/results" 2>&1
	echo "fill arm '${arm:-none}' done: $(grep -c "^OK .*fill$" "$work/results") ok so far" >&2
done
for arm in "" "-O"; do
	printf '%s\n' $progs_rev | xargs -P "$jobs" -I{} bash -c 'one "$@"' _ {} "$work" "$arm" warm >>"$work/results" 2>&1
	echo "warm arm '${arm:-none}' done" >&2
done

echo "== item1 results =="
for phase in fill warm; do
	echo "$phase: OK=$(grep -c "^OK .* $phase\$" "$work/results") DIFFER=$(grep -c "^DIFFER .* $phase\$" "$work/results") FAILBUILD=$(grep -c "^FAILBUILD .* $phase\$" "$work/results")"
done
echo "SKIP(uncompilable, counted once per arm)=$(grep -c '^SKIP ' "$work/results")"
grep -E '^(DIFFER|FAILBUILD) ' "$work/results" | sort | head -60
echo "cache units: $(find "$work/home/cg12/function-cache" -name '*.gocfn' 2>/dev/null | wc -l)"
echo "cache bytes: $(du -sb "$work/home/cg12/function-cache" 2>/dev/null | cut -f1)"
bad=$(grep -cE '^(DIFFER|FAILBUILD) ' "$work/results")
echo "item1 verdict: $([ "$bad" -eq 0 ] && echo PASS || echo FAIL)"
