// Command slogalloc measures what log/slog's allocation-avoidance designs
// actually cost, one case per line, and is meant to be compiled and run by two
// compilers so the two answers can be put side by side:
//
//	go run ./cmd/goc -run goc/testdata/slog_allocations/main.go
//	go run goc/testdata/slog_allocations/main.go
//
// goc/slogalloc_test.go does exactly that and joins the output; see it for how
// the pair is turned into a baseline.
//
// # Why this program exists
//
// log/slog is the stdlib package most deliberately built around what the
// reference compiler's escape analysis and interface conversion can prove. Three
// of its data-structure choices are bets on the compiler rather than on the
// algorithm:
//
//   - Record.front is a [5]Attr inline array (record.go's nAttrsInline), so a
//     record with five or fewer attributes needs no heap-allocated slice.
//   - Value packs int64, bool, time.Duration and float64 into a uint64 field and
//     puts only a Kind constant in its any field, so those four types are stored
//     without being boxed.
//   - Logger.log returns before doing any work when the level is disabled, so a
//     call that logs nothing is supposed to cost nothing at all.
//
// None of that is free by construction. Each one is free only if the compiler
// keeps the variadic []any backing array off the heap, resolves a constant
// interface conversion to a static symbol, and does not allocate for a value
// that never leaves the frame. A compiler that is more pessimistic than the
// reference implementation pays for every one of these designs and gets nothing
// back for them, which is why they make a good measurement.
//
// # Method
//
// Every case is a closure called through a func value, so neither compiler can
// see through the call and delete the work. measure runs the closure
// measureIterations times to warm up, then measureRounds more times, reading
// runtime.MemStats before and after each round and keeping the round with the
// fewest mallocs and the fewest bytes. The minimum is the right estimator here:
// background work in the runtime can only ever add allocations to a round, never
// remove them, so the smallest round is the one least contaminated by anything
// that is not the case under test.
//
// runtime.ReadMemStats stops the world and flushes every mcache before it
// answers, so Mallocs and TotalAlloc are exact counts and not samples. Both
// compilers run this identical source, so whatever the method gets wrong, it
// gets wrong equally on both sides. control/empty-body and
// control/new-64-byte-object are in the table to say so out loud: if the first
// is not 0.00 allocations the instrument is picking up noise, and if the second
// is not exactly one 64-byte allocation the instrument is not measuring what it
// claims to.
//
// # Reading the numbers
//
// The integer values passed to boxing cases are chosen deliberately.
// runtime.staticuint64s (runtime/iface.go) lets the reference compiler convert
// any integer value below 256 to an interface without allocating, so
// control/any-int-small and control/any-int-large differ only in whether that
// table applies. Reading them as a pair separates "this compiler has no static
// table" from "this compiler allocates for every conversion".
//
// The same reasoning runs through the rest of the table: attr/slog.String boxes
// a pointer (a stringptr) where attr/slog.Int boxes a Kind constant, so the two
// separate pointer-shaped conversions from integer-shaped ones;
// control/variadic-6-preboxed passes six already-boxed values so the only thing
// left to pay for is the []any backing array; control/return-interface returns
// an interface that was already an interface, so the only thing left to pay for
// is the result; and logattrs/* reaches the same Record through ...Attr instead
// of ...any, so it costs the record without costing the boxing.
//
// The controls are sized so the slog rows can be decomposed rather than merely
// compared, and every row has been decomposed that way at least once. When this
// program was written, info/5-attr's nine allocations under goc were
// context.Background's result, Logger.Handler's result, Handle's error result,
// one combined object holding the variadic array and its five boxed payloads,
// and one boxed Kind per attribute. All nine are gone; the row is 0.00, which
// is gc's number. What the remaining non-zero rows are made of is written down
// in the branch report rather than here, because it changes as they are closed.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"time"
)

// The measurement's two parameters. They are printed in the output so a
// baseline records the method that produced it and not just the result.
const (
	measureIterations = 2000
	measureRounds     = 5
)

// Sinks. Every case stores its result in one of these, so that a compiler that
// can see the value is unused cannot delete the work that produced it. They are
// package-level on purpose: a local would be provably dead.
var (
	sinkAny        any
	sinkAttr       slog.Attr
	sinkObject     *[64]byte
	sinkBool       bool
	sinkInt        int
	sinkContext    context.Context
	handledRecords int
	handledAttrs   int
	variadicLength int
)

// Inputs. None of these are constants, so a case measures a conversion of a
// value rather than a conversion the front end can fold away. Which value is
// which matters: smallInt is inside runtime.staticuint64s's range and largeInt
// is outside it.
var (
	smallInt    = 7
	largeInt    = 1 << 20
	boolValue   = true
	stringValue = "gc12"
	floatValue  = 1.5
	duration    = 1500 * time.Microsecond
	pointer     = new(int)
	statusCode  = 200
	method      = "GET"
	cached      = true
)

// Pre-boxed arguments for the variadic control. Boxing them once, here, is what
// makes control/variadic-6-preboxed a measurement of the backing array alone.
var (
	box0 any = "alpha"
	box1 any = 1
	box2 any = "beta"
	box3 any = 2
	box4 any = "gamma"
	box5 any = 3
)

// noRetain is the callee a variadic call site has to reason about: it reads its
// arguments and keeps none of them. A caller can leave the backing array in its
// frame if, and only if, it can learn that.
//
// It is marked noinline because the interesting case is the one slog is in --
// Logger.log is far too big to inline into its caller -- and an inlined callee
// would make the question trivial for both compilers instead of measuring it.
//
//go:noinline
func noRetain(args ...any) {
	variadicLength += len(args)
}

// identityAny returns an interface value it was handed. Nothing is converted
// here -- the argument arrives boxed and leaves boxed -- so the only thing a
// case calling it can be measuring is what it costs to return an interface.
//
//go:noinline
func identityAny(value any) any { return value }

// identityInt is the same call shape with a word-sized result, as the control
// for identityAny: it says whether the cost is in returning a value at all or
// in returning an interface specifically.
//
//go:noinline
func identityInt(value int) int { return value }

// nopHandler is a handler that does the least a handler can do while still
// forcing the Record to be built: it reports itself enabled at or above its
// level and counts what it is handed. Nothing in it formats or copies an
// attribute, so a case using it measures slog's record construction and nothing
// downstream of it.
type nopHandler struct {
	level slog.Level
}

func (h nopHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h nopHandler) Handle(_ context.Context, record slog.Record) error {
	handledRecords++
	handledAttrs += record.NumAttrs()
	return nil
}

func (h nopHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }

func (h nopHandler) WithGroup(name string) slog.Handler { return h }

// The loggers, built by buildLoggers at the top of main rather than in a
// package-level initializer.
//
// That is not a style choice. goc miscompiles
//
//	var jsonLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
//
// at package level: the program builds, and the first call through the
// slog.Handler interface dies with "cg12: interface dispatch failed for dynamic
// type". Assigning the identical expression inside a function is fine. See
// section 4 of this branch's report; the reproducer is nine lines and is not
// about slog's allocations at all, so this program routes around it rather than
// being unable to measure the two JSON rows.
var (
	// nopLogger is enabled at Info, so its Debug calls take the disabled path
	// and its Info calls take the enabled one. One logger for both directions
	// keeps the comparison between them honest.
	nopLogger *slog.Logger
	// jsonLogger is the realistic configuration: the stdlib JSON handler
	// formatting into a writer that throws the bytes away, so the case measures
	// the handler rather than the destination.
	jsonLogger *slog.Logger
	background context.Context
)

func buildLoggers() {
	nopLogger = slog.New(nopHandler{level: slog.LevelInfo})
	jsonLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	background = context.Background()
}

// measurement is one row of the table.
type measurement struct {
	name string
	body func()
}

// cases is the whole table, in the order it is printed. The order is fixed
// rather than sorted so that related rows stay adjacent: a row means much less
// on its own than it does beside the row it is meant to be compared with.
var cases = []measurement{
	// The instrument, measured before anything it is used to measure.
	{"control/empty-body", func() {}},
	{"control/new-64-byte-object", func() { sinkObject = new([64]byte) }},

	// Interface conversion on its own, with no slog in the picture. This is the
	// control for every other row: if these allocate, so will everything built
	// on top of them, and the cause is not slog's.
	{"control/any-int-small", func() { sinkAny = smallInt }},
	{"control/any-int-large", func() { sinkAny = largeInt }},
	{"control/any-bool", func() { sinkAny = boolValue }},
	{"control/any-string-constant", func() { sinkAny = "literal" }},
	{"control/any-string-variable", func() { sinkAny = stringValue }},
	{"control/any-pointer", func() { sinkAny = pointer }},

	// The variadic backing array, separated from what goes into it.
	{"control/variadic-0-args", func() { noRetain() }},
	{"control/variadic-6-preboxed", func() { noRetain(box0, box1, box2, box3, box4, box5) }},
	{"control/variadic-6-literal", func() { noRetain("alpha", 1, "beta", 2, "gamma", 3) }},

	// Returning an interface, with nothing converted on the way in or out.
	{"control/return-interface", func() { sinkAny = identityAny(box1) }},
	{"control/return-int", func() { sinkInt = identityInt(smallInt) }},

	// The two things every Logger method does before it looks at its arguments.
	// Together they account for the fixed part of the rows below -- the part a
	// call pays whether it carries attributes or not, and whether it is enabled
	// or not -- so that the per-attribute cost can be read off the difference
	// rather than guessed at.
	{"control/context-background", func() { sinkContext = context.Background() }},
	{"control/handler-enabled", func() { sinkBool = nopLogger.Enabled(background, slog.LevelDebug) }},

	// The packed Value: five constructors that are supposed to store their
	// value in Value.num and a Kind in Value.any, boxing nothing.
	{"attr/slog.Int", func() { sinkAttr = slog.Int("count", largeInt) }},
	{"attr/slog.String", func() { sinkAttr = slog.String("name", stringValue) }},
	{"attr/slog.Bool", func() { sinkAttr = slog.Bool("ok", boolValue) }},
	{"attr/slog.Duration", func() { sinkAttr = slog.Duration("elapsed", duration) }},
	{"attr/slog.Float64", func() { sinkAttr = slog.Float64("ratio", floatValue) }},

	// The inline array. Five attributes fit in Record.front; the sixth is the
	// first that has to spill into Record.back, so the step from five to six is
	// the cost of the design working, and any step before it is the cost of it
	// not working.
	{"info/1-attr", func() { nopLogger.Info("msg", "a", 1) }},
	{"info/3-attr", func() { nopLogger.Info("msg", "a", 1, "b", 2, "c", 3) }},
	{"info/5-attr", func() { nopLogger.Info("msg", "a", 1, "b", 2, "c", 3, "d", 4, "e", 5) }},
	{"info/6-attr", func() { nopLogger.Info("msg", "a", 1, "b", 2, "c", 3, "d", 4, "e", 5, "f", 6) }},
	// The same three attributes with values too large for runtime.staticuint64s,
	// which is what a real caller logging an ID or a byte count passes. The
	// difference from info/3-attr is the static table and nothing else.
	{"info/3-attr-large-ints", func() { nopLogger.Info("msg", "a", largeInt, "b", largeInt, "c", largeInt) }},

	// The same records reached through ...Attr instead of ...any: the same
	// Record built from values that are already typed, so no argument needs
	// boxing at the call site.
	{"logattrs/3-attr", func() {
		nopLogger.LogAttrs(background, slog.LevelInfo, "msg",
			slog.Int("a", 1), slog.Int("b", 2), slog.Int("c", 3))
	}},
	{"logattrs/6-attr", func() {
		nopLogger.LogAttrs(background, slog.LevelInfo, "msg",
			slog.Int("a", 1), slog.Int("b", 2), slog.Int("c", 3),
			slog.Int("d", 4), slog.Int("e", 5), slog.Int("f", 6))
	}},

	// The disabled level. Logger.log returns before it builds a Record, so
	// everything these rows cost was paid at the call site, before slog was
	// entered, on a call that did nothing.
	{"disabled/no-attrs", func() { nopLogger.Debug("msg") }},
	{"disabled/3-attr", func() { nopLogger.Debug("msg", "a", 1, "b", 2, "c", 3) }},
	{"disabled/logattrs-3-attr", func() {
		nopLogger.LogAttrs(background, slog.LevelDebug, "msg",
			slog.Int("a", 1), slog.Int("b", 2), slog.Int("c", 3))
	}},

	// A realistic record through the real JSON handler, both ways of writing
	// it: key-value pairs, which is what most code writes, and typed attributes,
	// which is what the documentation recommends for hot paths.
	{"json/kv-4-pairs", func() {
		jsonLogger.Info("request handled",
			"method", method, "status", statusCode, "elapsed", duration, "cached", cached)
	}},
	{"json/logattrs-4-attrs", func() {
		jsonLogger.LogAttrs(background, slog.LevelInfo, "request handled",
			slog.String("method", method), slog.Int("status", statusCode),
			slog.Duration("elapsed", duration), slog.Bool("cached", cached))
	}},
}

// measure returns allocations and bytes per operation for one case.
//
// GOMAXPROCS is pinned to 1 for the duration, as testing.AllocsPerRun does, so
// that nothing else in the program is running while the counters are read.
func measure(body func()) (allocations, bytes float64) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)

	// Warm up. First-call costs -- a sync.Pool that has never been filled, a
	// lazily built handler state -- are real, but they are not per-operation
	// costs and this table is per-operation.
	for iteration := 0; iteration < measureIterations; iteration++ {
		body()
	}
	runtime.GC()

	fewestMallocs, fewestBytes := ^uint64(0), ^uint64(0)
	for round := 0; round < measureRounds; round++ {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for iteration := 0; iteration < measureIterations; iteration++ {
			body()
		}
		runtime.ReadMemStats(&after)

		if mallocs := after.Mallocs - before.Mallocs; mallocs < fewestMallocs {
			fewestMallocs = mallocs
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated < fewestBytes {
			fewestBytes = allocated
		}
	}
	return float64(fewestMallocs) / float64(measureIterations),
		float64(fewestBytes) / float64(measureIterations)
}

// main runs every case, or only the ones named on the command line.
//
//	slogalloc              every case, in table order
//	slogalloc -list        every case's name, one per line, and nothing else
//	slogalloc info/5-attr  one case
//
// Running one case per process is how goc/slogalloc_test.go drives this, and it
// is not a convenience: goc currently dies part-way through the table with a
// fatal runtime error (see the buildLoggers comment and the report), and a
// process per case means one case that kills the runtime costs one row instead
// of every row after it.
func main() {
	if len(os.Args) > 1 && os.Args[1] == "-list" {
		for _, testCase := range cases {
			fmt.Println(testCase.name)
		}
		return
	}

	selected := cases
	if len(os.Args) > 1 {
		selected = nil
		for _, name := range os.Args[1:] {
			found := false
			for _, testCase := range cases {
				if testCase.name == name {
					selected = append(selected, testCase)
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "slogalloc: no case named %q\n", name)
				os.Exit(2)
			}
		}
	}

	buildLoggers()
	fmt.Printf("# iterations=%d rounds=%d\n", measureIterations, measureRounds)
	for _, testCase := range selected {
		allocations, bytes := measure(testCase.body)
		fmt.Printf("%s\t%.2f\t%.1f\n", testCase.name, allocations, bytes)
	}
	// Printed so that the work the sinks exist to preserve is observably
	// consumed, and so that a run where a case silently did nothing -- a handler
	// that was never called, a variadic that was never passed -- can be told
	// apart from one where it did.
	fmt.Fprintf(os.Stderr, "consumed records=%d attrs=%d variadic=%d any=%v attr=%v object=%p bool=%v ctx=%v int=%d\n",
		handledRecords, handledAttrs, variadicLength, sinkAny != nil, sinkAttr.Key != "", sinkObject,
		sinkBool, sinkContext != nil, sinkInt)
}
