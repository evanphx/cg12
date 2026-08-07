#!/usr/bin/env bash
# GATE ITEM 2, part two: does the bound recover, is the working set protected,
# and is a compile safe while the prune runs?
#
# Trim rate-limits itself to once per 24 hours per directory, so a directory that
# reaches the bound in one afternoon cannot be made to prune by compiling more.
# Backdating the stamp is how the prune is reached inside a gate; that it has to
# be backdated at all is the finding, not a workaround.
#
# Usage: item2-prune.sh <workdir>
#
# CAVEAT, read before believing this script's output: its section 3 ("the working
# set is the last thing to go") identifies the working set by name and then looks
# for those names in the pressured directory. By the time it runs, section 2's
# prune has already evicted them and nothing has rewritten them, so it reports
# them as "evicted" whatever the policy did. That arm is WRONG and is superseded
# by item2-lru.sh, which deletes and rewrites the working set before each
# measurement. Sections 1, 2 and 4 -- the rate limit, the prune, and the compile
# racing a prune -- are sound and are what the report quotes from here.
set -u
work="$1"
goc="$work/goc"
cache="$work/home/cg12/function-cache"
big=goc/testdata/stdlib_http_tls_client_server.go
mkdir -p "$work/out"
export XDG_CACHE_HOME="$work/home"
sha() { sha256sum <"$1" | cut -d' ' -f1; }
size() { du -sb "$cache" 2>/dev/null | cut -f1; }
units() { find "$cache" -name '*.gocfn' 2>/dev/null | wc -l; }
backdate() { echo $(( $(date +%s) - 90000 )) >"$cache/trim.txt"; }
BUDGET=1073741824

env CG12_NOCACHE=1 "$goc" -o "$work/out/prune.ctl" "$big" >/dev/null 2>&1 || { echo "control failed"; exit 1; }
control=$(sha "$work/out/prune.ctl")
echo "control image $control"

# ---- 0. which units is a warm compile of $big actually reading? -----------
# The same key digests appear in the pressured directory, so this is the working
# set that the LRU ordering is supposed to protect.
rm -rf "$work/wsdir"; mkdir -p "$work/wsdir"
CG12_FUNC_CACHE="$work/wsdir" "$goc" -o /dev/null "$big" >/dev/null 2>&1
find "$work/wsdir" -name '*.gocfn' -printf '%f\n' | sort >"$work/out/workingset.txt"
echo "WORKING SET: $(wc -l <"$work/out/workingset.txt") units are what a warm compile of $(basename $big) reads"

# ---- 1. the rate limit ----------------------------------------------------
before=$(size)
"$goc" -o /dev/null "$big" >/dev/null 2>&1
after=$(size)
echo "RATE-LIMIT: with the stamp fresh, a compile took the directory from $before to $after bytes against a $BUDGET budget; stamp still $(cat "$cache/trim.txt")"

# ---- 2. the prune ---------------------------------------------------------
backdate
before=$(size); beforeUnits=$(units); start=$(date +%s)
"$goc" -o "$work/out/prune.run" "$big" >"$work/out/prune.run.log" 2>&1; rc=$?
end=$(date +%s); after=$(size); afterUnits=$(units)
echo "PRUNE: $before bytes / $beforeUnits units -> $after bytes / $afterUnits units in $((end-start))s, compile exit $rc"
[ -s "$work/out/prune.run.log" ] && { echo "PRUNE: the compile wrote output:"; head -5 "$work/out/prune.run.log"; }
[ "$(sha "$work/out/prune.run")" = "$control" ] && echo "PRUNE: OK the pruning compile produced the control image" || echo "PRUNE: FAIL image differs"
[ "$after" -le "$BUDGET" ] && echo "PRUNE: OK inside the 1 GiB budget" || echo "PRUNE: FAIL still over budget"
find "$cache" -name '*.gocfn' -printf '%f\n' | sort >"$work/out/after-prune.txt"
missing=$(comm -23 "$work/out/workingset.txt" "$work/out/after-prune.txt" | wc -l)
echo "PRUNE: $missing of $(wc -l <"$work/out/workingset.txt") working-set units were evicted (they were rewritten by the compile that pruned, so they are the youngest)"

# ---- 3. the working set is the last thing to go ---------------------------
# Refill over budget, then age everything EXCEPT the working set, which is what a
# box that has just been compiling this program looks like.
refill() {
	local tag="$1" count="$2" g
	for g in $(seq 1 "$count"); do
		( cp "$goc" "$work/goc.$tag$g"; printf 'gate-%s-%s' "$tag" "$g" >>"$work/goc.$tag$g"; chmod +x "$work/goc.$tag$g"
		  "$work/goc.$tag$g" -o /dev/null "$big" >/dev/null 2>&1
		  "$work/goc.$tag$g" -o /dev/null goc/testdata/fmt_sprintf.go >/dev/null 2>&1
		  rm -f "$work/goc.$tag$g" ) &
	done
	wait
}
refill lru 8
echo "LRU: refilled to $(size) bytes / $(units) units"
# everything old...
find "$cache" -name '*.gocfn' -exec touch -d '2026-08-05 12:00:00' {} + 2>/dev/null
# ...except the working set, touched now, as a read inside the last hour leaves it
while read -r name; do
	f=$(find "$cache" -name "$name" -print -quit); [ -n "$f" ] && touch "$f"
done <"$work/out/workingset.txt"
backdate
before=$(size)
"$goc" -o "$work/out/lru.run" goc/testdata/hello.go >/dev/null 2>&1
after=$(size)
find "$cache" -name '*.gocfn' -printf '%f\n' | sort >"$work/out/after-lru.txt"
survived=$(comm -12 "$work/out/workingset.txt" "$work/out/after-lru.txt" | wc -l)
total=$(wc -l <"$work/out/workingset.txt")
echo "LRU: $before -> $after bytes; $survived of $total working-set units survived the prune"
[ "$survived" -eq "$total" ] && echo "LRU: OK the working set is the last thing to go" || echo "LRU: FAIL the prune took part of the working set"
# and the program that owns that working set still compiles to the control image
"$goc" -o "$work/out/lru.big" "$big" >/dev/null 2>&1
[ "$(sha "$work/out/lru.big")" = "$control" ] && echo "LRU: OK the owning program still compiles to the control image" || echo "LRU: FAIL image differs after the prune"

# ---- 4. a compile reading units a concurrent prune is deleting ------------
# The adversarial form: every unit is given the SAME old mtime, so the LRU
# ordering gives the running compile's units no protection at all and the prune
# unlinks them while it reads them.
for round in 1 2 3; do
	refill race$round 8
	find "$cache" -name '*.gocfn' -exec touch -d '2026-08-05 12:00:00' {} + 2>/dev/null
	over=$(size)
	backdate
	"$goc" -o "$work/out/race.$round" "$big" >"$work/out/race.$round.log" 2>&1 &
	compile=$!
	sleep 1
	"$goc" -o /dev/null goc/testdata/hello.go >/dev/null 2>&1
	wait $compile; rc=$?
	got=$(sha "$work/out/race.$round" 2>/dev/null || echo none)
	echo "RACE round$round: over budget at $over bytes, ended at $(size) bytes / $(units) units, compile exit $rc, image $( [ "$got" = "$control" ] && echo MATCHES || echo DIFFERS )"
	[ -s "$work/out/race.$round.log" ] && { echo "RACE round$round: the compile wrote output:"; head -5 "$work/out/race.$round.log"; }
done
echo "item2 prune: done"
