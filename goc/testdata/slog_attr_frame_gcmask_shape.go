// The defect without log/slog: a zero-length array field ahead of a scalar.
//
// slog_attr_frame_gcmask_control.go has this exact shape and passes on main,
// which was read as evidence that the shape is not the trigger. It is not
// evidence. Both programs are given the same frame map -- the word holding 200
// is claimed by both -- and the control survives only because nothing in it
// copies main's stack while the value is live. The mark phase tolerates a
// claimed word holding 200 (runtime.findObject on an address that was never
// heap returns silently); runtime.adjustpointers does not.
//
// This program is the control with its collection replaced by a recursion deep
// enough to copy the stack, so the walker that does reject it is the one that
// runs. It fails on main with
//
//	runtime: bad pointer in frame main_main at 0x...: 0xc8
//
// and it imports nothing. The trigger is the shape: [0]func() is one field
// whose length ir.Field.Count cannot express, so it becomes a phantom
// pointer-shaped element at the offset of the field after it.
package main

type value struct {
	_   [0]func()
	num uint64
	any any
}

type attr struct {
	Key   string
	Value value
}

var kind any = 4
var sink int

//go:noinline
func makeAttr(key string, n uint64) attr {
	return attr{Key: key, Value: value{num: n, any: kind}}
}

//go:noinline
func grow(depth int) {
	var pad [512]int
	pad[0] = depth
	if depth > 0 {
		grow(depth - 1)
	}
	sink += pad[0]
}

//go:noinline
func hold(a attr) uint64 {
	grow(400)
	return a.Value.num
}

func main() { println(hold(makeAttr("k", 200))) }
