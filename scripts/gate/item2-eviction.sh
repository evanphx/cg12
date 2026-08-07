#!/usr/bin/env bash
# GATE ITEM 2, part one: fill a real default-location cache past the 1 GiB bound
# with successive compiler generations.
#
# A "compiler generation" is a distinct compiler binary: clause 9 of the key is a
# digest of the bytes of os.Executable(), so appending distinct trailing bytes to
# a copy of the binary mints a new generation exactly as a rebuild does. The
# genuine-rebuild control is in item2-rebuild-control.sh.
#
# Usage: item2-eviction.sh <workdir> <generations> <gens-at-once>
set -u
work="$1"; gens="${2:-20}"; batch="${3:-3}"
mkdir -p "$work/home" "$work/out"
export XDG_CACHE_HOME="$work/home"
cache="$work/home/cg12/function-cache"

progs=(
	goc/testdata/stdlib_http_tls_client_server.go
	goc/testdata/fmt_sprintf.go
	goc/testdata/stdlib_crypto_ecdsa.go
	goc/testdata/gc_struct.go
)
size() { du -sb "$cache" 2>/dev/null | cut -f1; }
units() { find "$cache" -name '*.gocfn' 2>/dev/null | wc -l; }

generation() {
	local g="$1" work="$2"; shift 2
	export XDG_CACHE_HOME="$work/home"
	cp "$work/goc" "$work/goc.g$g"
	printf 'cg12-gate-generation-%06d' "$g" >>"$work/goc.g$g"
	chmod +x "$work/goc.g$g"
	local p n
	for p in "$@"; do
		n=$(basename "$p" .go)
		"$work/goc.g$g" -o /dev/null "$p" >/dev/null 2>&1 || echo "  BUILDFAIL gen$g $n"
		"$work/goc.g$g" -O -o /dev/null "$p" >/dev/null 2>&1 || echo "  BUILDFAIL-O gen$g $n"
	done
	rm -f "$work/goc.g$g"
}
export -f generation

echo "item2 fill: starting from $(size) bytes, $(units) units (that is one compiler generation over the whole 406-program corpus, both arms)"
g=1
while [ "$g" -le "$gens" ]; do
	last=$((g + batch - 1)); [ "$last" -gt "$gens" ] && last=$gens
	seq "$g" "$last" | xargs -P "$batch" -I{} bash -c 'generation "$@"' _ {} "$work" "${progs[@]}"
	echo "item2 fill: after generations $g-$last: $(size) bytes, $(units) units, trim.txt=$( [ -f "$cache/trim.txt" ] && cat "$cache/trim.txt" || echo none )"
	g=$((last + 1))
done
echo "item2 fill: final $(size) bytes, $(units) units"
