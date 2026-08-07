#!/usr/bin/env bash
# GATE ITEM 3: the three degradation arms the branch did not run.
#
#   A  the cache directory becomes unwritable MID-COMPILE
#   B  a unit is truncated WHILE IT IS BEING READ
#   C  two compilers of different vintage sharing one directory
#
# Every arm has to end in the image an uncached compile produces.
#
# Usage: item3-degradation.sh <workdir>
set -u
work="$1"
mkdir -p "$work/out"
goc="$work/goc"        # the branch's compiler
mainGoc="$work/goc.main" # main's compiler, built in item3 setup

big=goc/testdata/stdlib_http_tls_client_server.go
mid=goc/testdata/fmt_sprintf.go

sha() { sha256sum <"$1" | cut -d' ' -f1; }
pass=0; fail=0
say() { if [ "$1" = OK ]; then pass=$((pass+1)); else fail=$((fail+1)); fi; echo "item3 $*"; }

echo "== controls =="
for p in "$big" "$mid"; do
	n=$(basename "$p" .go)
	env CG12_NOCACHE=1 "$goc" -o "$work/out/ctl.$n" "$p" >/dev/null 2>&1 || { echo "control build failed $n"; exit 1; }
	sha "$work/out/ctl.$n" >"$work/out/ctl.$n.sha"
	env CG12_NOCACHE=1 "$goc" -O -o "$work/out/ctl.$n.O" "$p" >/dev/null 2>&1 || { echo "control -O build failed $n"; exit 1; }
	sha "$work/out/ctl.$n.O" >"$work/out/ctl.$n.O.sha"
done
echo "controls recorded"

# ---------------------------------------------------------------- arm A
# Unwritable mid-compile, twice: once against a cold directory (so the writes at
# flush are the ones that fail) and once against a filled one (so reads have
# already happened and the re-write of carried-forward units fails).
armA() {
	local label="$1" prepare="$2" delay="$3"
	local dir="$work/A.$label"
	rm -rf "$dir"; mkdir -p "$dir"
	if [ "$prepare" = filled ]; then
		CG12_FUNC_CACHE="$dir" "$goc" -o /dev/null "$big" >/dev/null 2>&1
	fi
	CG12_FUNC_CACHE="$dir" "$goc" -o "$work/out/A.$label" "$big" >"$work/out/A.$label.log" 2>&1 &
	local pid=$!
	sleep "$delay"
	# Everything under it, so neither a create nor a rename can succeed.
	find "$dir" -type d -exec chmod 0555 {} + 2>/dev/null
	local rc=0
	wait $pid || rc=$?
	find "$dir" -type d -exec chmod 0755 {} + 2>/dev/null
	if [ $rc -ne 0 ]; then say "A/$label FAIL compile exited $rc"; sed -n 1,5p "$work/out/A.$label.log"; return; fi
	if [ -s "$work/out/A.$label.log" ]; then say "A/$label FAIL wrote to stderr/stdout"; sed -n 1,5p "$work/out/A.$label.log"; return; fi
	if [ "$(sha "$work/out/A.$label")" != "$(cat "$work/out/ctl.$(basename "$big" .go).sha")" ]; then
		say "A/$label FAIL image differs from the uncached control"; return
	fi
	say "OK A/$label image identical to the uncached control"
}

# ---------------------------------------------------------------- arm B
# Truncated while being read. A filled directory, and a mutator that keeps
# truncating live units for the whole first stretch of the compile, so a read
# lands on a file that changed size between the stat and the read.
armB() {
	local label="$1" seconds="$2"
	local dir="$work/B.$label"
	rm -rf "$dir"; mkdir -p "$dir"
	CG12_FUNC_CACHE="$dir" "$goc" -o /dev/null "$big" >/dev/null 2>&1
	local before; before=$(find "$dir" -name '*.gocfn' | wc -l)
	(
		endAt=$((SECONDS + seconds))
		while [ $SECONDS -lt $endAt ]; do
			for f in $(find "$dir" -name '*.gocfn' | shuf | head -30); do
				s=$(stat -c %s "$f" 2>/dev/null) || continue
				[ "${s:-0}" -gt 64 ] || continue
				truncate -s $((s / 2)) "$f" 2>/dev/null
			done
		done
	) &
	local mutator=$!
	CG12_FUNC_CACHE="$dir" "$goc" -o "$work/out/B.$label" "$big" >"$work/out/B.$label.log" 2>&1
	local rc=$?
	wait $mutator 2>/dev/null
	if [ $rc -ne 0 ]; then say "B/$label FAIL compile exited $rc"; sed -n 1,5p "$work/out/B.$label.log"; return; fi
	if [ -s "$work/out/B.$label.log" ]; then say "B/$label FAIL wrote to stderr/stdout"; sed -n 1,5p "$work/out/B.$label.log"; return; fi
	if [ "$(sha "$work/out/B.$label")" != "$(cat "$work/out/ctl.$(basename "$big" .go).sha")" ]; then
		say "B/$label FAIL image differs from the uncached control"; return
	fi
	say "OK B/$label image identical to the uncached control ($before units were being truncated under it)"
}

# ---------------------------------------------------------------- arm C
# Two vintages, one directory. main's goc has no default, so it is pointed at the
# directory explicitly; the branch's goc is too, so that both are certainly using
# the same one. Interleaved, both orders, both arms, and each image compared to
# ITS OWN compiler's uncached control.
armC() {
	local dir="$work/C"
	rm -rf "$dir"; mkdir -p "$dir"
	# main's own controls
	for p in "$big" "$mid"; do
		n=$(basename "$p" .go)
		env CG12_NOCACHE=1 "$mainGoc" -o "$work/out/ctlmain.$n" "$p" >/dev/null 2>&1 || { say "C FAIL main control build $n"; return; }
		sha "$work/out/ctlmain.$n" >"$work/out/ctlmain.$n.sha"
	done
	local round who p n binary control got
	for round in 1 2 3; do
		for who in main branch; do
			for p in "$mid" "$big"; do
				n=$(basename "$p" .go)
				if [ "$who" = main ]; then binary="$mainGoc"; control="$work/out/ctlmain.$n.sha"; else binary="$goc"; control="$work/out/ctl.$n.sha"; fi
				if ! CG12_FUNC_CACHE="$dir" "$binary" -o "$work/out/C.$who.$n" "$p" >"$work/out/C.$who.$n.log" 2>&1; then
					say "C FAIL round$round $who $n did not build"; sed -n 1,5p "$work/out/C.$who.$n.log"; continue
				fi
				got=$(sha "$work/out/C.$who.$n")
				if [ "$got" != "$(cat "$control")" ]; then
					say "C FAIL round$round $who $n image differs from its own uncached control"
				else
					say "OK C round$round $who $n identical to its own uncached control"
				fi
			done
		done
	done
	echo "item3 C: the shared directory holds $(find "$dir" -name '*.gocfn' | wc -l) units, $(du -sb "$dir" | cut -f1) bytes"
}

armA cold cold 4
armA filled filled 4
armA cold2 cold 9
armB steady 25
armC

echo "== item3 summary: $pass ok, $fail bad =="
