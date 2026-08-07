#!/usr/bin/env bash
# Compile every corpus program with one compiler under one environment, in N
# concurrent `goc compile-batch` workers, and write the images to $out/$tag.
#
# usage: corpus-run.sh -goc PATH -root REPO -out DIR -tag NAME [-O] [-j N]
# Environment (CG12_FUNC_CACHE, CG12_NOCACHE, ...) is inherited by the workers.
set -u

goc=""; root=""; out=""; tag=""; opt=""; jobs=24
while [ $# -gt 0 ]; do
	case "$1" in
	-goc) goc="$2"; shift 2 ;;
	-root) root="$2"; shift 2 ;;
	-out) out="$2"; shift 2 ;;
	-tag) tag="$2"; shift 2 ;;
	-O) opt="-O"; shift ;;
	-j) jobs="$2"; shift 2 ;;
	*) echo "corpus-run: unknown flag $1" >&2; exit 2 ;;
	esac
done
[ -n "$goc" ] && [ -n "$root" ] && [ -n "$out" ] && [ -n "$tag" ] || { echo "corpus-run: missing flag" >&2; exit 2; }

cd "$root" || exit 1
mapfile -t programs < <(ls goc/testdata/*.go | sort)
mkdir -p "$out/$tag"
rm -rf "$out/req.$tag"; mkdir -p "$out/req.$tag"

i=0
for ((w = 0; w < jobs; w++)); do : >"$out/req.$tag/$w"; done
for program in "${programs[@]}"; do
	base=$(basename "$program" .go)
	printf '{"source":"%s","output":"%s"}\n' "$program" "$out/$tag/$base" >>"$out/req.$tag/$((i % jobs))"
	i=$((i + 1))
done

started=$(date +%s)
for ((w = 0; w < jobs; w++)); do
	"$goc" compile-batch ${opt:+$opt} <"$out/req.$tag/$w" >"$out/req.$tag/$w.res" 2>"$out/req.$tag/$w.err" &
done
wait
elapsed=$(($(date +%s) - started))

built=0; failed=0
: >"$out/$tag.sha256"
for program in "${programs[@]}"; do
	base=$(basename "$program" .go)
	if [ -f "$out/$tag/$base" ]; then
		printf '%s  %s\n' "$(sha256sum "$out/$tag/$base" | cut -d' ' -f1)" "$base" >>"$out/$tag.sha256"
		built=$((built + 1))
	else
		printf '%s  %s\n' "COMPILE-FAILED" "$base" >>"$out/$tag.sha256"
		failed=$((failed + 1))
	fi
done
sort -k2 -o "$out/$tag.sha256" "$out/$tag.sha256"
echo "corpus-run[$tag]: ${#programs[@]} programs, built=$built failed=$failed, ${elapsed}s, j=$jobs opt='${opt:-none}'"
cat "$out/req.$tag"/*.err | grep -v '^$' | head -5
