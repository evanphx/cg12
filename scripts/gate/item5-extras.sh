#!/usr/bin/env bash
# GATE, the three "also confirm" items.
#
#   5a  CG12_NOCACHE=1 bypasses at the default location, an explicit directory
#       and `auto` -- read side as well as write side, watched with strace.
#   5b  concurrency: 24 processes, 3 programs each, one shared cache directory.
#   5c  the first-compile penalty.
#
# Usage: item5-extras.sh <workdir> <which>
set -u
work="$1"; which="${2:-all}"
goc="$work/goc"
mkdir -p "$work/out"
sha() { sha256sum <"$1" | cut -d' ' -f1; }

prog=goc/testdata/fmt_sprintf.go
name=$(basename "$prog" .go)

if [ "$which" = all ] || [ "$which" = 5a ]; then
echo "===== 5a: CG12_NOCACHE=1 at three locations ====="
env CG12_NOCACHE=1 "$goc" -o "$work/out/nc.control" "$prog" >/dev/null 2>&1 || { echo "5a control failed"; exit 1; }
control=$(sha "$work/out/nc.control")

bypass() {
	local label="$1"; shift
	local home="$work/nc.$label.home" dir="$work/nc.$label.dir"
	rm -rf "$home" "$dir"; mkdir -p "$home" "$dir"
	# Pre-fill BOTH candidate locations with a real cache, so that a compiler that
	# reads has something to read and a hit would be visible.
	XDG_CACHE_HOME="$home" "$goc" -o /dev/null "$prog" >/dev/null 2>&1
	CG12_FUNC_CACHE="$dir" "$goc" -o /dev/null "$prog" >/dev/null 2>&1
	local filledDefault filledExplicit
	filledDefault=$(find "$home/cg12/function-cache" -name '*.gocfn' 2>/dev/null | wc -l)
	filledExplicit=$(find "$dir" -name '*.gocfn' 2>/dev/null | wc -l)
	# Snapshot both trees, then run with CG12_NOCACHE=1 under strace and require
	# that not one path under either was opened and not one byte changed.
	find "$home" "$dir" -type f -printf '%p %s\n' 2>/dev/null | sort >"$work/out/nc.$label.before"
	env CG12_NOCACHE=1 XDG_CACHE_HOME="$home" "$@" strace -f -qq -e trace=openat,mkdirat,unlinkat,renameat,renameat2,newfstatat \
		-o "$work/out/nc.$label.strace" "$goc" -o "$work/out/nc.$label.bin" "$prog" >/dev/null 2>&1
	local rc=$?
	find "$home" "$dir" -type f -printf '%p %s\n' 2>/dev/null | sort >"$work/out/nc.$label.after"
	local touched changed got
	touched=$(grep -cE "$(printf '%s|%s' "$home/cg12" "$dir")" "$work/out/nc.$label.strace" || true)
	changed=$(diff "$work/out/nc.$label.before" "$work/out/nc.$label.after" | grep -c '^[<>]' || true)
	got=$(sha "$work/out/nc.$label.bin")
	echo "5a $label: exit=$rc prefilled default=$filledDefault explicit=$filledExplicit"
	echo "5a $label: syscalls naming either cache location: $touched; files added/removed/resized: $changed"
	if [ "$got" = "$control" ] && [ "$touched" -eq 0 ] && [ "$changed" -eq 0 ] && [ $rc -eq 0 ]; then
		echo "5a $label: OK"
	else
		echo "5a $label: FAIL"
		grep -E "$(printf '%s|%s' "$home/cg12" "$dir")" "$work/out/nc.$label.strace" | head -10
	fi
}

bypass default env CG12_FUNC_CACHE=
bypass explicit env CG12_FUNC_CACHE="$work/nc.explicit.dir"
bypass auto env CG12_FUNC_CACHE=auto
fi

if [ "$which" = all ] || [ "$which" = 5b ]; then
echo "===== 5b: 24 processes, 3 programs each, one shared default-location cache ====="
progs=(goc/testdata/hello.go goc/testdata/fmt_sprintf.go goc/testdata/gc_struct.go)
home="$work/conc.home"; rm -rf "$home"; mkdir -p "$home"
: >"$work/out/conc.results"
for p in "${progs[@]}"; do
	n=$(basename "$p" .go)
	env CG12_NOCACHE=1 "$goc" -o "$work/out/conc.ctl.$n" "$p" >/dev/null 2>&1 || echo "control failed $n"
	sha "$work/out/conc.ctl.$n" >"$work/out/conc.ctl.$n.sha"
done
worker() {
	local id="$1" work="$2"; shift 2
	export XDG_CACHE_HOME="$work/conc.home"
	local p n
	for p in "$@"; do
		n=$(basename "$p" .go)
		if ! "$work/goc" -o "$work/out/conc.$id.$n" "$p" >/dev/null 2>&1; then
			echo "FAILBUILD $id $n"; continue
		fi
		if [ "$(sha256sum <"$work/out/conc.$id.$n" | cut -d' ' -f1)" = "$(cat "$work/out/conc.ctl.$n.sha")" ]; then
			echo "OK $id $n"
		else
			echo "DIFFER $id $n"
		fi
		rm -f "$work/out/conc.$id.$n"
	done
}
export -f worker
seq 1 24 | xargs -P 24 -I{} bash -c 'worker "$@"' _ {} "$work" "${progs[@]}" >>"$work/out/conc.results" 2>&1
echo "5b: OK=$(grep -c '^OK ' "$work/out/conc.results") DIFFER=$(grep -c '^DIFFER ' "$work/out/conc.results") FAILBUILD=$(grep -c '^FAILBUILD ' "$work/out/conc.results") of 72"
grep -E '^(DIFFER|FAILBUILD)' "$work/out/conc.results" | head -20
fi
