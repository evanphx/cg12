// Program permodule_probe is the user half of the two-module image test. It is
// compiled by goc exactly as any capability program is, and nothing in it knows
// that a second Go module exists.
//
// The package-level words below are patched at link time with addresses of
// symbols in a *separately compiled* object whose data lands megabytes from this
// program's and whose 32-bit NameOff/TypeOff fields were resolved against its own
// module base. Every line printed here is read back through
// runtime.resolveNameOff, runtime.resolveTypeOff, runtime.resolveTextOff or
// runtime.findfunc, each of which picks the module that contains the *referring*
// pointer -- which is the mechanism under test.
package main

import (
	"reflect"
	"runtime"
	"unsafe"
)

// Slots the link step patches. Each is zero as compiled.
var (
	// probeIntSlot is the second module's descriptor for Go's `int` -- a type
	// this program also describes. Two descriptions of one Go type is what
	// per-module regions create and what moduledata.typelinks resolves.
	probeIntSlot uintptr

	// probeCodeSlot is the entry address of the second module's first function,
	// and probeFuncSlot a word holding it (which is what a Go func value points
	// at).
	probeCodeSlot uintptr
	probeFuncSlot uintptr

	// probeHoldSlot is a word holding the second module's non-leaf function: it
	// keeps a managed pointer live across a call back into this module, so a
	// traceback and a GC stack scan both have to read the second module's pcsp
	// table and locals stack map.
	probeHoldSlot uintptr
)

// emptyInterface is the layout of an interface value with no methods: a type
// descriptor and a data word.
type emptyInterface struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

type payload struct {
	words [4]uintptr
}

const payloadMagic = 0x5ea1ed

var holder emptyInterface
var storage int = 42
var callbackRan bool
var frameNames []string
var survivedMagic uintptr

// probeCallback is what the second module's non-leaf function calls. Everything
// it does happens while a frame that only the second module's pclntab describes
// is on the stack.
func probeCallback() {
	callbackRan = true
	programCounters := make([]uintptr, 32)
	count := runtime.Callers(1, programCounters)
	frames := runtime.CallersFrames(programCounters[:count])
	for {
		frame, more := frames.Next()
		frameNames = append(frameNames, frame.Function)
		if !more {
			break
		}
	}
	// Collect twice with the second module's frame live. Its stack map is the
	// only thing that can keep the payload reachable.
	runtime.GC()
	runtime.GC()
}

func main() {
	if probeIntSlot == 0 || probeHoldSlot == 0 {
		println("probe: slots were never patched")
		return
	}

	// One Go type, two descriptors. reflect.TypeOf reads the eface word directly,
	// so it returns the second module's descriptor. reflect.PointerTo goes through
	// PtrToThis -- a TypeOff -- which runtime.resolveTypeOff answers out of the
	// typemap runtime.typelinksinit built, so it must come back as *this*
	// module's *int.
	holder.typ = unsafe.Pointer(probeIntSlot)
	holder.data = unsafe.Pointer(&storage)
	foreign := reflect.TypeOf(*(*any)(unsafe.Pointer(&holder)))
	println("foreign-int:" + foreign.String())
	println("foreign-int-kind:", int(foreign.Kind()))

	var local int
	localPointerType := reflect.TypeOf(&local)
	foreignPointerType := reflect.PointerTo(foreign)
	println("foreign-ptr:" + foreignPointerType.String())
	if foreignPointerType == localPointerType {
		println("ptr-identity: same")
	} else {
		println("ptr-identity: different")
	}

	// The second module's own pclntab. runtime.FuncForPC walks the module chain to
	// find the owning module and reads that module's name table, so a name here
	// means the second module's generated metadata is in use. This function is
	// the one at text offset 0 of its module, the slot whose name the runtime used
	// to read as empty.
	if probeCodeSlot != 0 {
		function := runtime.FuncForPC(probeCodeSlot)
		if function == nil {
			println("first-func: unresolved")
		} else {
			println("first-func:" + function.Name())
		}
	}
	if probeFuncSlot != 0 {
		value := probeFuncSlot
		call := *(*func() int32)(unsafe.Pointer(&value))
		println("first-call:", call())
	}

	// The non-leaf half: hand the second module a freshly allocated object, let it
	// hold the only live reference across a call back into this module, and
	// collect from inside that call.
	held := probeHoldSlot
	hold := *(*func(*payload) *payload)(unsafe.Pointer(&held))
	object := new(payload)
	object.words[0] = payloadMagic
	returned := hold(object)
	if returned == nil {
		println("hold: returned nil")
		return
	}
	survivedMagic = returned.words[0]

	if !callbackRan {
		println("callback: never ran")
		return
	}
	println("frames:", len(frameNames))
	for _, name := range frameNames {
		println("frame:" + name)
	}
	if survivedMagic == payloadMagic {
		println("payload: intact")
	} else {
		println("payload: clobbered")
	}
	println("probe: done")
}
