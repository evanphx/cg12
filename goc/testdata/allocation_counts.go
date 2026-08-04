// Allocation counts for the calls a Go program makes most often, measured by
// running them. TestAllocationCounts holds every line here to a committed
// number and fails in both directions; see goc/alloccount_test.go for why.
//
// The counts are printed as allocations per call times 100, so a change of one
// allocation in a hundred calls is visible rather than rounded away. Each
// operation lives in its own //go:noinline function and the measuring loop
// calls that, so the allocation sites are in ordinary straight-line code --
// which is where they are in real programs. The two `_in_loop` rows are the
// deliberate exception: their allocation is written inside the loop body, which
// is what opt.promotionsBlockedByALoop refuses to promote, and they are here so
// the price of that rule is a number somebody can see.
package main

import (
	"fmt"
	"runtime"
	"sync"
)

var (
	sinkString string
	sinkAny    any
	sinkInt    int

	theInt    = 42
	theString = "answer"
	theStruct = pair{1, 2}
	thePtr    = &theInt

	// theLargeInt is past the last entry of runtime.staticuint64s, and
	// theFloat's bit pattern is far past it, so boxing either has to allocate
	// where boxing theInt does not. They are here to hold the fast path to
	// being about the value and not about the type.
	theLargeInt = 1 << 20
	theBool     = true

	// theFloat is assigned in init rather than in its declaration. goc compiles
	// a package-level float64 initialized to a constant to zero -- a defect that
	// predates this file's float row and has nothing to do with boxing -- and
	// zero's bit pattern does fit staticuint64s, so declaring it here would make
	// this row measure any(0.0) and quietly report a fast path that is not
	// running.
	theFloat float64
)

func init() { theFloat = 3.5 }

type pair struct{ a, b int }

//go:noinline
func sprintfInt() { sinkString = fmt.Sprintf("value=%d", theInt) }

//go:noinline
func sprintfString() { sinkString = fmt.Sprintf("value=%s", theString) }

//go:noinline
func sprintfStruct() { sinkString = fmt.Sprintf("value=%v", theStruct) }

//go:noinline
func sprintfNoArgs() { sinkString = fmt.Sprintf("a constant format") }

//go:noinline
func variadicInts(values ...int) int { return len(values) }

//go:noinline
func callVariadicInts() { sinkInt = variadicInts(theInt, theInt) }

//go:noinline
func variadicAny(values ...any) int { return len(values) }

//go:noinline
func callVariadicAny() { sinkInt = variadicAny(theInt, theString) }

// The variadic rows that are about *retention*, which is the question the
// backing array and the payload it points at have to be able to answer
// separately. keepElement stores one element into a package-level variable, so
// the boxed payload outlives the call and the `[N]any` array does not; a
// compiler that cannot tell those apart has to heap both, and one that gets it
// backwards leaves sinkAny holding a pointer into a returned frame.
//
// theRetained is past runtime.staticuint64s deliberately: a value inside the
// table would be boxed to a pointer into read-only memory whatever the escape
// analysis decided, and the row would pass without measuring anything.
var theRetained = 0x5eed

//go:noinline
func keepElement(args ...any) { sinkAny = args[0] }

//go:noinline
func retainVariadicElement() { keepElement(theRetained) }

//go:noinline
func keepStructElement(args ...any) { sinkAny = args[0] }

//go:noinline
func retainVariadicStructElement() { keepStructElement(theStruct) }

// Two convertible payloads in one call, which is where splitting them out of
// the combined object can cost rather than save: gc allocates one box each and
// goc used to allocate one object for both. With values inside
// runtime.staticuint64s both boxes are free and the split wins; with values
// outside it the split matches gc instead of beating it. Both rows are here so
// the trade is a number rather than an argument.
//
//go:noinline
func sprintfTwoSmallInts() { sinkString = fmt.Sprintf("%d/%d", theInt, theInt) }

//go:noinline
func sprintfTwoLargeInts() { sinkString = fmt.Sprintf("%d/%d", theLargeInt, theLargeInt) }

//go:noinline
func takeAny(value any) int {
	if value == nil {
		return 0
	}
	return 1
}

//go:noinline
func boxSmallInt() { sinkInt = takeAny(theInt) }

//go:noinline
func boxLargeInt() { sinkInt = takeAny(theLargeInt) }

//go:noinline
func boxBool() { sinkInt = takeAny(theBool) }

//go:noinline
func boxFloat64() { sinkInt = takeAny(theFloat) }

//go:noinline
func boxString() { sinkInt = takeAny(theString) }

//go:noinline
func boxPointer() { sinkInt = takeAny(thePtr) }

//go:noinline
func returnAnyFromInt(value int) any { return value }

//go:noinline
func returnAnyFromLargeInt(value int) any { return value }

//go:noinline
func returnAnyFromPointer(value *int) any { return value }

//go:noinline
func callReturnAnyFromInt() { sinkAny = returnAnyFromInt(theInt) }

//go:noinline
func callReturnAnyFromLargeInt() { sinkAny = returnAnyFromLargeInt(theLargeInt) }

//go:noinline
func callReturnAnyFromPointer() { sinkAny = returnAnyFromPointer(thePtr) }

// The constant conversions. A compile-time constant boxed into an interface has
// no storage to allocate: the payload's contents are known while compiling and
// nothing may write to the value inside an interface, so one read-only object
// serves every conversion of that constant in the program. gc has always done
// this, which is why every host number below is zero and why the string row is
// the one that used to differ.
//
// The large integer is here as well as the string because the two reach it
// differently. The string has no runtime conversion helper and used to be an
// allocation outright; the integer has one and was free at run time already,
// but only by calling convT64 and reading runtime.staticuint64s -- a call, and
// a payload that still had to be given somewhere to live.

//go:noinline
func boxStringConstant() { sinkInt = takeAny("a string constant") }

//go:noinline
func boxLargeIntConstant() { sinkInt = takeAny(1 << 20) }

// The `...any` shape log/slog is built out of, reduced to what makes it work.
//
// packAll walks its arguments two at a time through packNext, which returns
// both the converted value and the remainder of the slice -- the shape of
// log/slog.argsToAttr, and on the IR an aggregate return plus a result-area
// out-parameter. packOne keeps the *value* rather than where it lives for the
// kinds it knows, which is log/slog.Value's packed representation, and boxes
// only what it does not know. Nothing retains the array, so gc keeps it in a
// frame; goc could not, because its summary for packAll said the pointer
// escaped where gc said only its contents did.
//
// One argument, not two: a call with two escaping payloads costs two
// allocations split against one combined, and opt.foldSplitPayloadsBackIn
// correctly sends the array back to the heap to take them. That rule is
// measured by sprintf_two_small_ints; this row is measuring the placement.
type packed struct {
	key string
	num uint64
	box any
}

var sinkPacked packed

//go:noinline
func packAll(args ...any) {
	for len(args) > 0 {
		var one packed
		one, args = packNext(args)
		sinkPacked = one
	}
}

//go:noinline
func packNext(args []any) (packed, []any) {
	if key, isKey := args[0].(string); isKey && len(args) > 1 {
		return packOne(key, args[1]), args[2:]
	}
	return packOne("", args[0]), args[1:]
}

//go:noinline
func packOne(key string, value any) packed {
	switch typed := value.(type) {
	case int:
		return packed{key: key, num: uint64(typed)}
	case string:
		return packed{key: key, num: uint64(len(typed))}
	default:
		return packed{key: key, box: value}
	}
}

//go:noinline
func packVariadicElement() { packAll(theInt) }

// The same call reached through a forwarder that also passes an interface,
// which is the shape log/slog.Logger.Info is in: it calls Logger.log with a
// context.Context alongside the arguments it was handed. goc's parameter
// builder flattens an interface parameter into two words while its call emitter
// hands the interface over as one descriptor value, so the two lists have
// different lengths and every summary about the call used to be discarded --
// including the one about the backing array, which is not the argument that
// failed to line up.

//go:noinline
func packThrough(reason fmt.Stringer, args ...any) {
	sinkInt = len(reason.String())
	packAll(args...)
}

//go:noinline
func forwardVariadicPastAnInterface() { packThrough(theStringer, theInt) }

type reason struct{ text string }

func (r reason) String() string { return r.text }

var theStringer fmt.Stringer = reason{"because"}

var pool = sync.Pool{New: func() any { return new(pair) }}

//go:noinline
func poolRoundTrip() {
	value := pool.Get().(*pair)
	value.a = theInt
	pool.Put(value)
}

//go:noinline
func sprintfInLoop(times int) {
	for i := 0; i < times; i++ {
		sinkString = fmt.Sprintf("value=%d", theInt)
	}
}

//go:noinline
func variadicIntsInLoop(times int) {
	for i := 0; i < times; i++ {
		sinkInt = variadicInts(theInt, theInt)
	}
}

// The three rows below are about what a *local* object's uses say about where
// it has to live, rather than about boxing or the `...` array. Each one is a
// shape whose backing storage the reference implementation keeps in the frame,
// and each was an allocation under goc until the escape walk learned the rule
// named in its comment.

// consumeInt takes its argument and hands it straight back, which is the least
// a callee can do with a parameter. It is //go:noinline so the call survives to
// the escape walk, and its parameter is an int so there is nothing about the
// caller's storage for it to retain.
//
//go:noinline
func consumeInt(value int) int { return value }

// rangeSliceLiteral ranges over a slice literal and passes each element to a
// call. The element is a copy, so where the copy goes says nothing about where
// the backing array lives.
//
//go:noinline
func rangeSliceLiteral() {
	values := []int{1, 2, 3, 4}
	total := 0
	for _, value := range values {
		total += consumeInt(value)
	}
	sinkInt = total
}

// compareByteSliceAsString converts a byte slice to a string and compares it.
// The conversion builds the string out of storage of its own -- or, where the
// compiler is allowed to alias, out of a string that cannot outlive the
// comparison -- so the slice's backing array is not carried anywhere.
//
//go:noinline
func compareByteSliceAsString() {
	buffer := []byte{'a', 'b', 'c'}
	if string(buffer) == theString {
		sinkInt++
	}
}

// declareInterfaceFromPointer puts a pointer in an interface declared with var.
// Nothing is boxed -- a pointer is its own interface payload -- so the question
// is where the interface goes, which is the question the assignment form of the
// same statement has always been answered by.
//
//go:noinline
func declareInterfaceFromPointer() {
	box := &pair{theInt, 2}
	var value any = box
	if value == sinkAny {
		sinkInt++
	}
}

// link is a two-node chain written as one nested literal, which is the shape
// the row below is about.
type link struct {
	value int
	next  *link
}

// nestedCompositeLiteralAddress writes an address into a struct literal that is
// itself behind an address. The inner object lives wherever the outer one does,
// so both are frame storage or neither is.
//
//go:noinline
func nestedCompositeLiteralAddress() {
	root := &link{value: theInt, next: &link{value: 2}}
	sinkInt = root.value + root.next.value
}

// appendFromSpreadSource appends a slice to itself. The elements are copied out
// of the spread operand, so the operand's backing array is not retained by the
// call and does not have to outlive the frame.
//
//go:noinline
func appendFromSpreadSource() {
	values := []int{1, 2, 3, 4}
	values = append(values[:1], values[2:]...)
	sinkInt = len(values)
}

// scoreBox and its interface are the shape a value takes on the way into a
// local: an address converted to an interface type and then only called
// through. Nothing is boxed -- a pointer is its own interface payload -- so the
// only question is where the object goes, and it goes wherever the local does.
type scoreBox struct{ value int }

func (box *scoreBox) score() int { return box.value }

type scorer interface{ score() int }

// interfaceLocalMethodCall converts an address to an interface on the way into
// a local and then calls a method on it. The emitter's nonEscapingAddress climbs
// the conversion and used to meet the assignment as its default case, which put
// every such literal on the heap; and the method call is answered by asking
// every implementation the program can dispatch to, which is one -- and it does
// not retain its receiver.
//
//go:noinline
func interfaceLocalMethodCall() {
	value := scorer(&scoreBox{value: theInt})
	sinkInt = value.score()
}

// retainedReceiver's method stores the receiver in a package-level variable, so
// the object has to be on the heap. This row is the direction that got *more*
// expensive: an immediately called method used to be free whatever the method
// did, which left the object in the frame with a live pointer into it.
type retainedReceiver struct{ value int }

var retainedSink *retainedReceiver

func (box *retainedReceiver) keep() int {
	retainedSink = box
	return box.value
}

//go:noinline
func methodRetainsReceiver() {
	value := &retainedReceiver{value: theInt}
	sinkInt = value.keep()
}

// The four rows below are `defer`, one row per shape the statement can take.
//
// goc turns `defer x` into runtime.deferproc(fn), and builds fn three different
// ways: a directly deferred function literal is its own closure, a deferred
// method call is a *method value* holding the receiver, and a deferred call with
// arguments is a `deferwrap` closure holding them. Only the first was ever placed
// in a frame, so the other two cost an allocation per call -- and the method
// value's is worse than a wasted allocation, because a heap descriptor holding
// the address of a frame-local receiver is a frame address published into a heap
// object. testdata/runtime_defer_receiver_gc.go is the program that dies from it.
// See goc.gen.deferredFunctionValueStaysInFrame.

var deferMutex sync.Mutex

//go:noinline
func deferMutexUnlock() {
	deferMutex.Lock()
	defer deferMutex.Unlock()
	sinkInt++
}

// deferCounter's method is deferred on a frame-local receiver, which is the
// shape that was a published frame address and not merely an allocation.
type deferCounter struct{ n int }

func (counter *deferCounter) add() { sinkInt += counter.n }

//go:noinline
func deferMethodOnALocal() {
	counter := deferCounter{n: 1}
	defer counter.add()
	sinkInt += counter.n
}

//go:noinline
func addToSink(value int) { sinkInt += value }

//go:noinline
func deferCallWithArguments() {
	defer addToSink(theInt)
	sinkInt++
}

// The control: a directly deferred function literal, which has had the frame
// placement since it was written. It is here so that a change which took the
// rule away from all four rather than giving it to three is visible as a
// regression rather than as three rows going back to where they were.
//
//go:noinline
func deferFunctionLiteral() {
	defer func() { sinkInt++ }()
	sinkInt++
}

// methodValueReceiver takes a method value rather than calling the method. The
// value is a closure over the receiver, so the receiver is in the closure and
// nowhere else; the closure is a frame object here, so the receiver can be one
// too. This used to be answered "escapes" because the walk accepted only an
// immediately called method selector.
//
//go:noinline
func methodValueReceiver() {
	value := &scoreBox{value: theInt}
	scoreOnce := value.score
	sinkInt = scoreOnce()
}

// consumeBox is the callee the row below reaches through a function-typed local.
//
//go:noinline
func consumeBox(box *scoreBox) int { return box.value }

// callThroughAFunctionVariable calls a function through a local of function
// type. The local is assigned once from a named function, never assigned again
// and never addressed, so the call reaches that function and nothing else --
// which is what lets the argument's summary be asked at all. Before the walk
// resolved this, `f(box)` had no *types.Func to ask about and took the
// conservative answer.
//
//go:noinline
func callThroughAFunctionVariable() {
	var consume func(*scoreBox) int = consumeBox
	box := &scoreBox{value: theInt}
	sinkInt = consume(box)
}

// retainNothingVariadic is the callee the row below hands an address to. Its
// only use of the `...` parameter is len, so it cannot reach an element and the
// address does not escape through the call.
//
//go:noinline
func retainNothingVariadic(args ...any) int { return len(args) }

// addressIntoANonRetainingVariadic hands a local's address to a variadic callee
// that keeps nothing. The escape summary used to refuse to describe a variadic
// parameter at all -- an argument there is an element of a slice the callee
// builds, and the parameter's own summary answers a different question -- so
// every such address went to the heap. It now answers the question that is
// actually being asked: can the callee reach an element.
//
//go:noinline
func addressIntoANonRetainingVariadic() {
	value := theInt
	sinkInt = retainNothingVariadic(&value)
}

const iterations = 1000

// measure runs the operation `iterations` times after a warm-up of the same
// length, so a first-call cost -- a sync.Pool that has never been filled, a
// buffer grown once and kept -- is paid before the counting starts and is not
// mistaken for a per-call allocation.
func measure(name string, operation func(int)) {
	var before, after runtime.MemStats
	operation(iterations)
	runtime.ReadMemStats(&before)
	operation(iterations)
	runtime.ReadMemStats(&after)
	print(name, " ")
	println(int64(after.Mallocs-before.Mallocs) * 100 / iterations)
}

func repeat(operation func()) func(int) {
	return func(times int) {
		for i := 0; i < times; i++ {
			operation()
		}
	}
}

func main() {
	measure("sprintf_int", repeat(sprintfInt))
	measure("sprintf_string", repeat(sprintfString))
	measure("sprintf_struct", repeat(sprintfStruct))
	measure("sprintf_no_args", repeat(sprintfNoArgs))
	measure("variadic_ints", repeat(callVariadicInts))
	measure("variadic_any", repeat(callVariadicAny))
	measure("variadic_retained_element", repeat(retainVariadicElement))
	measure("variadic_retained_struct_element", repeat(retainVariadicStructElement))
	measure("sprintf_two_small_ints", repeat(sprintfTwoSmallInts))
	measure("sprintf_two_large_ints", repeat(sprintfTwoLargeInts))
	measure("box_small_int", repeat(boxSmallInt))
	measure("box_large_int", repeat(boxLargeInt))
	measure("box_bool", repeat(boxBool))
	measure("box_float64", repeat(boxFloat64))
	measure("box_string", repeat(boxString))
	measure("box_pointer", repeat(boxPointer))
	measure("box_string_constant", repeat(boxStringConstant))
	measure("box_large_int_constant", repeat(boxLargeIntConstant))
	measure("pack_variadic_element", repeat(packVariadicElement))
	measure("forward_variadic_past_an_interface", repeat(forwardVariadicPastAnInterface))
	measure("return_any_from_int", repeat(callReturnAnyFromInt))
	measure("return_any_from_large_int", repeat(callReturnAnyFromLargeInt))
	measure("return_any_from_pointer", repeat(callReturnAnyFromPointer))
	measure("sync_pool_round_trip", repeat(poolRoundTrip))
	measure("range_slice_literal", repeat(rangeSliceLiteral))
	measure("compare_byte_slice_as_string", repeat(compareByteSliceAsString))
	measure("declare_interface_from_pointer", repeat(declareInterfaceFromPointer))
	measure("nested_composite_literal_address", repeat(nestedCompositeLiteralAddress))
	measure("append_from_spread_source", repeat(appendFromSpreadSource))
	measure("interface_local_method_call", repeat(interfaceLocalMethodCall))
	measure("method_value_receiver", repeat(methodValueReceiver))
	measure("call_through_a_function_variable", repeat(callThroughAFunctionVariable))
	measure("address_into_a_non_retaining_variadic", repeat(addressIntoANonRetainingVariadic))
	measure("method_retains_receiver", repeat(methodRetainsReceiver))
	measure("defer_mutex_unlock", repeat(deferMutexUnlock))
	measure("defer_method_on_a_local", repeat(deferMethodOnALocal))
	measure("defer_call_with_arguments", repeat(deferCallWithArguments))
	measure("defer_function_literal", repeat(deferFunctionLiteral))
	measure("sprintf_in_loop", sprintfInLoop)
	measure("variadic_ints_in_loop", variadicIntsInLoop)
}
