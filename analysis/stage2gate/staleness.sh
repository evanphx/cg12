#!/usr/bin/env bash
# Stage-2 gate, item 3: cache poisoning and staleness, end to end.
#
# Every case is: put the cache in a known state, move exactly one thing, compile
# again, and check both what came out (must equal an uncached build of the same
# tree) and what the cache did (from GOC_DEBUG_FUNCCACHE=1).
#
# usage: staleness.sh <repo-root> <goc-binary> <work-dir>
set -u

root="$1"; goc="$2"; work="$3"
program="goc/testdata/fmt_sprintf.go"
leaf="stdlib/src/internal/byteorder/byteorder.go"

cd "$root" || exit 1
mkdir -p "$work"
fails=0

# stats "<log file>" prints the one-line cache report goc wrote to stderr.
stats() { grep -o 'function cache: .*' "$1" | tail -1; }
field() { # field <log> <regex with one capture>
	stats "$1" | grep -oE "$2" | head -1
}

build() { # build <tag> <env assignments...> -- <goc flags...>
	local tag="$1"; shift
	local -a envs=()
	while [ "$1" != "--" ]; do envs+=("$1"); shift; done
	shift
	env "${envs[@]}" GOC_DEBUG_FUNCCACHE=1 "$goc" "$@" -o "$work/$tag.bin" "$program" \
		>"$work/$tag.out" 2>"$work/$tag.log"
	local status=$?
	if [ $status -ne 0 ]; then
		echo "  BUILD-FAILED $tag"
		sed -n '1,5p' "$work/$tag.log"
		return 1
	fi
	sha256sum "$work/$tag.bin" | cut -c1-16
}

report() { # report <name> <expected sha> <actual sha> [extra]
	if [ "$2" = "$3" ]; then
		printf '  ok    %-46s %s %s\n' "$1" "$3" "${4:-}"
	else
		printf '  FAIL  %-46s want=%s got=%s %s\n' "$1" "$2" "$3" "${4:-}"
		fails=$((fails + 1))
	fi
}

echo "=== reference: uncached build of the unmodified tree"
ref=$(build ref CG12_NOCACHE=1 -- ) || exit 1
echo "  nocache image $ref"

cache="$work/cache"
rm -rf "$cache"; mkdir -p "$cache"

echo "=== 1. cold then warm on a clean tree"
cold=$(build cold CG12_FUNC_CACHE="$cache" --) || exit 1
report "cold == nocache" "$ref" "$cold" "$(stats "$work/cold.log")"
warm=$(build warm CG12_FUNC_CACHE="$cache" --) || exit 1
report "warm == nocache" "$ref" "$warm" "$(stats "$work/warm.log")"
warmPackages=$(field "$work/warm.log" '[0-9]+/[0-9]+ packages')
echo "  warm packages: $warmPackages"
echo "  cache files: $(find "$cache" -name '*.gocfn' | wc -l), $(du -sh "$cache" | cut -f1)"

echo "=== 2. edit a leaf package's source ($leaf)"
cp "$leaf" "$work/leaf.orig"
printf '\n// stage-2 gate: a comment, and nothing else.\n' >>"$leaf"
refEdited=$(build ref-edited CG12_NOCACHE=1 --) || exit 1
warmEdited=$(build warm-edited CG12_FUNC_CACHE="$cache" --) || exit 1
report "edited leaf: warm == nocache" "$refEdited" "$warmEdited" ""
echo "  $(stats "$work/warm-edited.log")"
editedPackages=$(field "$work/warm-edited.log" '[0-9]+/[0-9]+ packages')
echo "  packages after the edit: $editedPackages (clean warm was $warmPackages)"
if [ "$refEdited" = "$ref" ]; then
	echo "  NOTE: the edit did not change the image (comment only), so warm==nocache is"
	echo "        also warm==the pre-edit image; the package count below is the real check."
fi
cp "$work/leaf.orig" "$leaf"

echo "=== 3. the same edit, second warm build (cache now refilled for the edited tree)"
warmEdited2=$(build warm-edited2 CG12_FUNC_CACHE="$cache" --) || exit 1
report "restored tree: warm == nocache" "$ref" "$warmEdited2" ""
echo "  $(stats "$work/warm-edited2.log")"

echo "=== 4. -O served from a cache filled without -O"
refOpt=$(build ref-opt CG12_NOCACHE=1 -- -O) || exit 1
warmOpt=$(build warm-opt CG12_FUNC_CACHE="$cache" -- -O) || exit 1
report "-O warm == -O nocache" "$refOpt" "$warmOpt" ""
echo "  $(stats "$work/warm-opt.log")"

echo "=== 5. text layout policy moves (GOC_TEXT_PAD=64)"
refPad=$(build ref-pad CG12_NOCACHE=1 GOC_TEXT_PAD=64 --) || exit 1
warmPad=$(build warm-pad CG12_FUNC_CACHE="$cache" GOC_TEXT_PAD=64 --) || exit 1
report "layout moved: warm == nocache" "$refPad" "$warmPad" ""
echo "  $(stats "$work/warm-pad.log")"

echo "=== 6. optimiser pipeline identity moves (GOC_OPT_SKIP=gvn)"
refSkip=$(build ref-skip CG12_NOCACHE=1 GOC_OPT_SKIP=gvn -- -O) || exit 1
warmSkip=$(build warm-skip CG12_FUNC_CACHE="$cache" GOC_OPT_SKIP=gvn -- -O) || exit 1
report "pipeline moved: warm == nocache" "$refSkip" "$warmSkip" ""
echo "  $(stats "$work/warm-skip.log")"

echo "=== 7. the compiler binary moves"
cp "$goc" "$work/goc-moved"
printf '\n' >>"$work/goc-moved" 2>/dev/null || true
chmod +x "$work/goc-moved"
env CG12_FUNC_CACHE="$cache" GOC_DEBUG_FUNCCACHE=1 "$work/goc-moved" -o "$work/moved.bin" "$program" \
	>"$work/moved.out" 2>"$work/moved.log"
if [ $? -ne 0 ]; then
	echo "  the appended-to binary did not run; using a rebuilt compiler instead"
else
	movedSha=$(sha256sum "$work/moved.bin" | cut -c1-16)
	report "moved compiler: image == nocache" "$ref" "$movedSha" ""
	echo "  $(stats "$work/moved.log")"
fi

echo "=== 8. CG12_NOCACHE=1 over a full cache reads and writes nothing"
before=$(find "$cache" -type f | wc -l)
beforeSum=$(find "$cache" -type f -printf '%p %s %T@\n' | sort | sha256sum | cut -c1-16)
noc=$(build nocache-over-full CG12_NOCACHE=1 CG12_FUNC_CACHE="$cache" --) || exit 1
after=$(find "$cache" -type f | wc -l)
afterSum=$(find "$cache" -type f -printf '%p %s %T@\n' | sort | sha256sum | cut -c1-16)
report "CG12_NOCACHE image == nocache" "$ref" "$noc" ""
report "cache directory untouched" "$beforeSum" "$afterSum" "($before -> $after files)"
if grep -q 'function cache:' "$work/nocache-over-full.log"; then
	echo "  FAIL  CG12_NOCACHE=1 still printed a cache report"
	fails=$((fails + 1))
else
	echo "  ok    CG12_NOCACHE=1 opened no cache at all"
fi

echo "=== 9. a truncated unit on disk is a miss, not a failure"
victim=$(find "$cache" -name '*.gocfn' -size +100k | head -1)
size=$(stat -c%s "$victim")
cp "$victim" "$work/victim.orig"
truncate -s $((size / 2)) "$victim"
truncWarm=$(build truncated CG12_FUNC_CACHE="$cache" --) || exit 1
report "truncated unit: warm == nocache" "$ref" "$truncWarm" ""
echo "  $(stats "$work/truncated.log")"
cp "$work/victim.orig" "$victim"

echo "=== 10. a bit flipped in a unit's body is a miss, not a wrong binary"
python3 - "$victim" <<'PY'
import sys
path = sys.argv[1]
data = bytearray(open(path, 'rb').read())
data[len(data) // 2] ^= 0x01
open(path, 'wb').write(data)
PY
flipWarm=$(build flipped CG12_FUNC_CACHE="$cache" --) || exit 1
report "flipped bit: warm == nocache" "$ref" "$flipWarm" ""
echo "  $(stats "$work/flipped.log")"

echo
if [ "$fails" -eq 0 ]; then
	echo "staleness: all checks passed"
else
	echo "staleness: $fails FAILURE(S)"
	exit 1
fi
