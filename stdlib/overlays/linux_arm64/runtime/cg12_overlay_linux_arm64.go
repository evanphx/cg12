//go:build linux && arm64

package runtime

const cg12RuntimeOverlayVersion = 2

// gocInterfaceDispatchFailure reports the dynamic type that did not match any
// compiler-generated interface method candidate. Keeping this check in the
// generated failure path turns otherwise opaque aborts into actionable runtime
// diagnostics without affecting successful interface calls.
func gocInterfaceDispatchFailure(dynamicType uintptr) {
	print("cg12: interface dispatch failed for dynamic type ")
	printhex(uint64(dynamicType))
	printnl()
	throw("cg12: interface dispatch failure")
}
