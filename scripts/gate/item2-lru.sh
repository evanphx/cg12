#!/usr/bin/env bash
# GATE ITEM 2, part three: WHICH units the bound evicts.
#
# BUILD_CACHE.md says "a read refreshes an entry's mtime (hourly granularity), so
# a unit a build is using is the last thing to go". Three measurements of that:
#
#   A  what "hourly granularity" means for a unit being read right now
#   B  a working set half an hour old, against units a compiler generation minted
#      a minute ago
#   C  a working set written by the compiler doing the compiling, which is the
#      case the bound was designed for
#
# Elapsed time is simulated with touch, because mtime is the only input the
# policy has and waiting half an hour inside a gate is not a measurement.
#
# Usage: item2-lru.sh <workdir>
set -u
work="$1"
goc="$work/goc"
cache="$work/home/cg12/function-cache"
big=goc/testdata/stdlib_http_tls_client_server.go
export XDG_CACHE_HOME="$work/home"
mkdir -p "$work/out"
size() { du -sb "$cache" 2>/dev/null | cut -f1; }
units() { find "$cache" -name '*.gocfn' 2>/dev/null | wc -l; }
backdate() { echo $(( $(date +%s) - 90000 )) >"$cache/trim.txt"; }
freshen() { echo "$(date +%s)" >"$cache/trim.txt"; }

# The working set: the unit names a cold fill of $big writes. Same key digests
# wherever the directory is, so these are the names to look for in $cache.
rm -rf "$work/wsdir"; mkdir -p "$work/wsdir"
CG12_FUNC_CACHE="$work/wsdir" "$goc" -o /dev/null "$big" >/dev/null 2>&1
find "$work/wsdir" -name '*.gocfn' -printf '%f\n' | sort >"$work/out/ws.names"
WSN=$(wc -l <"$work/out/ws.names")
echo "working set: $WSN units"

wspaths() { # the paths in $cache for the working-set names that are present
	local n
	: >"$work/out/ws.paths"
	while read -r n; do
		local p="$cache/${n:0:2}/$n"
		[ -f "$p" ] && echo "$p" >>"$work/out/ws.paths"
	done <"$work/out/ws.names"
}
wsfresh() { # remove the working set, then compile so it is rewritten with mtime=now
	local n
	while read -r n; do rm -f "$cache/${n:0:2}/$n"; done <"$work/out/ws.names"
	freshen
	"$goc" -o /dev/null "$big" >/dev/null 2>&1
	wspaths
}
topup() { # mint fresh compiler generations until the directory is over budget
	local tag="$1" g b
	freshen
	while [ "$(size)" -lt 1288490188 ]; do
		for g in 1 2 3 4; do
			b="$work/goc.$tag.$g.$RANDOM"
			cp "$goc" "$b"; printf 'gate-topup-%s-%s-%s' "$tag" "$g" "$RANDOM" >>"$b"; chmod +x "$b"
			( "$b" -o /dev/null "$big" >/dev/null 2>&1
			  "$b" -o /dev/null goc/testdata/fmt_sprintf.go >/dev/null 2>&1
			  rm -f "$b" ) &
		done
		wait
		freshen
	done
}
survivors() { local p c=0; while read -r p; do [ -f "$p" ] && c=$((c+1)); done <"$work/out/ws.paths"; echo "$c"; }

echo "== A. does a read refresh the mtime of a unit being used? =="
wsfresh
present=$(wc -l <"$work/out/ws.paths")
echo "A: $present of $WSN working-set units are in the directory"
sample=$(head -1 "$work/out/ws.paths")
for age in "30 minutes ago" "2 hours ago"; do
	touch -d "$age" "$sample"
	before=$(stat -c %Y "$sample")
	"$goc" -o /dev/null "$big" >/dev/null 2>&1
	after=$(stat -c %Y "$sample")
	if [ "$before" = "$after" ]; then
		echo "A: a unit stamped '$age' and then read kept its old mtime -- NO refresh"
	else
		echo "A: a unit stamped '$age' and then read was refreshed by $((after-before))s"
	fi
done

echo "== B. a half-hour-old working set against a minute-old generation =="
wsfresh
n=$(wc -l <"$work/out/ws.paths")
xargs -a "$work/out/ws.paths" touch -d '30 minutes ago'
topup B
before=$(size)
backdate
"$goc" -o /dev/null goc/testdata/hello.go >/dev/null 2>&1
after=$(size)
s=$(survivors)
echo "B: $before -> $after bytes; $s of $n working-set units survived"
[ "$s" -eq "$n" ] && echo "B: the working set was protected" \
	|| echo "B: the prune took the working set FIRST, in favour of units minted a minute earlier by generations nothing will read again"
"$goc" -o "$work/out/lruB.bin" "$big" >/dev/null 2>&1
env CG12_NOCACHE=1 "$goc" -o "$work/out/lruB.ctl" "$big" >/dev/null 2>&1
cmp -s "$work/out/lruB.bin" "$work/out/lruB.ctl" && echo "B: OK the program still compiles to the uncached image" || echo "B: FAIL image differs"

echo "== C. a working set written by the compiler doing the compiling =="
topup C
find "$cache" -name '*.gocfn' -exec touch -d '2 days ago' {} + 2>/dev/null
wsfresh
n=$(wc -l <"$work/out/ws.paths")
before=$(size)
backdate
"$goc" -o /dev/null goc/testdata/hello.go >/dev/null 2>&1
after=$(size)
s=$(survivors)
echo "C: $before -> $after bytes; $s of $n units of the current generation survived"
[ "$s" -eq "$n" ] && echo "C: OK the current generation is the last thing to go" || echo "C: the current generation was partly evicted"
echo "item2 lru: done, cache at $(size) bytes / $(units) units"
