#!/usr/bin/env bash
# Regenerate arm64/nosplit_debt.go -- the register of nosplit chains that were
# already over the reserve when the frame budget was introduced.
#
# Usage: scripts/nosplit-debt-regen.sh [-j N] [-arms pack,whole,split] [-update]
#
# Without -update this reports what the sweep found against the committed
# register and changes nothing. With -update it rewrites the map in place, and
# leaves the file's documentation alone; `git diff` is then the answer to what
# moved.
#
# The register is a floor -- a chain it names may be as deep as it records and
# no deeper, a chain it does not name may not exceed the reserve at all -- so
# the set of configurations it is generated from is part of its meaning. A chain
# the generation never saw is a chain the budget rejects the first time somebody
# compiles the program that contains it. That is not hypothetical: the recipe
# this replaced drove seven runtime pack roots and four whole programs, and
# goc/testdata/stdlib_os_exec_echo.go stopped building because
# syscall.runtime_AfterForkInChild's 976-byte chain was reachable from none of
# them.
#
# So the sweep is the product of the two properties that decide whether a chain
# is visible at all -- which functions are in the module, and where the module
# boundary falls -- rather than a sample of either:
#
#   pack   `goc build-runtime` per capability-matrix pack root, with and without -O
#   whole  every goc/testdata program with no prebuilt runtime, so the runtime,
#          the standard library and the program are one module and every frame of
#          every chain is visible at once
#   split  every program against the prebuilt packs, which is the boundary the
#          capability matrix, `goc compile-batch` and the pack cache use
#
# It is roughly 1600 compiles and takes about ten minutes on 24 workers.
set -u

jobs=$(nproc)
arms="pack,whole,split"
update=""
verbose=""
while [ $# -gt 0 ]; do
	case "$1" in
	-j)
		jobs="$2"
		shift 2
		;;
	-arms)
		arms="$2"
		shift 2
		;;
	-update)
		update="-update"
		shift
		;;
	-v)
		verbose="-v"
		shift
		;;
	*)
		echo "usage: scripts/nosplit-debt-regen.sh [-j N] [-arms pack,whole,split] [-update] [-v]" >&2
		exit 2
		;;
	esac
done

work="${TMPDIR:-/tmp}/nosplit-debt-$$"
mkdir -p "$work"
trap 'rm -rf "$work"' EXIT

go build -o "$work/goc" ./cmd/goc || exit 1
go build -o "$work/nosplitdebt" ./analysis/nosplitdebt || exit 1

exec "$work/nosplitdebt" \
	-goc "$work/goc" \
	-work "$work" \
	-j "$jobs" \
	-arms "$arms" \
	-register arm64/nosplit_debt.go \
	${update:+$update} \
	${verbose:+$verbose} \
	goc/testdata/*.go
