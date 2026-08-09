# workloads.sh — the three real C programs cg12 is judged against.
#
# Sourced, not run. It exists so bench-c.sh and inline-report.sh cannot drift
# apart about what "the lua workload" means: a timing number and a decision
# census that were taken against different include paths or different -D flags
# are not measurements of the same thing, and nothing about either output would
# say so.
#
# Why these three, and not one: they disagree. Jump threading measured as a 3.9%
# regression on SQLite and is worth 1.4% of QuickJS's text; lifting the inliner's
# caller ceiling measured as a 3.7% win on SQLite and a 4.7% loss on Lua. SQLite's
# VDBE and Lua's interpreter both dispatch on a switch; QuickJS's JS_CallInternal
# uses a computed goto, which is the shape several of the optimizer's heuristics
# were written against and the only one of the three that tests them.
#
# Usage:
#   . scripts/workloads.sh
#   workload_config quickjs      # sets SRC DRIVER INCLUDE LINK SUPPORT DEFINES
#   for w in $(workload_names); do ...; done

WORKLOAD_NAMES="sqlite lua quickjs"

workload_names() { echo "$WORKLOAD_NAMES"; }

# workload_config sets the variables describing one workload, and returns 2 for
# a name it does not know. BENCH and ROOT must already be set by the caller.
workload_config() {
	SUPPORT=()
	DEFINES=()
	case "$1" in
	sqlite)
		SRC="$BENCH/sqlite3.c"
		DRIVER="$ROOT/cc/testdata/perf/sqlite_bench.c"
		INCLUDE="$BENCH"
		LINK=(-lm -lpthread -ldl)
		;;
	lua)
		SRC="$BENCH/lua/onelua.c"
		DRIVER="$ROOT/cc/testdata/perf/lua_bench.c"
		INCLUDE="$BENCH/lua"
		LINK=(-lm)
		;;
	quickjs)
		# quickjs.c and libregexp.c collide on two static names, so they cannot
		# share a translation unit and the support files are built separately. The
		# interpreter is all in quickjs.c, which is the part being measured.
		SRC="$BENCH/quickjs/quickjs.c"
		DRIVER="$ROOT/cc/testdata/perf/quickjs_bench.c"
		INCLUDE="$BENCH/quickjs"
		LINK=(-lm -lpthread)
		SUPPORT=(libregexp.c libunicode.c cutils.c libbf.c)
		DEFINES=(-D_GNU_SOURCE -DCONFIG_BIGNUM '-DCONFIG_VERSION="cg12"')
		;;
	*)
		return 2
		;;
	esac
	return 0
}
