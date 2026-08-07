#!/usr/bin/env bash
# Item 4 with the cross-program confound removed: N concurrent goc processes
# compiling the SAME program into ONE shared cache directory.
set -u
G="$TMPDIR/gate"; R="$G/cmp"; W="$G/conc"
unset CG12_PACK_CACHE
cd "$R" || exit 1
rm -rf "$W"; mkdir -p "$W"
program="${1:-goc/testdata/fmt_sprintf.go}"
n="${2:-24}"
CG12_NOCACHE=1 "$G/goc-branch" -o "$W/ref" "$program" >/dev/null 2>&1 || { echo "reference build failed"; exit 1; }
ref=$(sha256sum "$W/ref" | cut -c1-16)
echo "reference (no cache): $ref"
for round in cold warm third; do
  [ "$round" = cold ] && { rm -rf "$W/cache"; mkdir -p "$W/cache"; }
  for ((i = 0; i < n; i++)); do
    CG12_FUNC_CACHE="$W/cache" "$G/goc-branch" -o "$W/$round.$i" "$program" >"$W/$round.$i.log" 2>&1 &
  done
  wait
  ok=0; bad=0; failed=0
  for ((i = 0; i < n; i++)); do
    if [ ! -f "$W/$round.$i" ]; then failed=$((failed+1)); continue; fi
    h=$(sha256sum "$W/$round.$i" | cut -c1-16)
    [ "$h" = "$ref" ] && ok=$((ok+1)) || { bad=$((bad+1)); echo "  $round.$i DIFFERENT $h"; }
  done
  strays=$(find "$W/cache" -type f ! -name '*.gocfn' ! -name 'trim.txt' | wc -l)
  echo "$round: $n concurrent compiles -> identical=$ok different=$bad failed=$failed; units=$(find "$W/cache" -name '*.gocfn'|wc -l) size=$(du -sh "$W/cache"|cut -f1) strays=$strays"
  rm -f "$W/$round".*
done
