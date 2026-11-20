//go:build linux && (amd64 || arm64)

package jit

import (
	"sync"
	"syscall"
	"unsafe"
)

// Supported reports whether this platform can run compiled code.
const Supported = true

func addrOf(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }

// StackSize is the size of each stack compiled code runs on. Pages are
// committed only as the code touches them, and a page below each stack
// is unmapped, so overflowing it faults rather than corrupting memory.
const StackSize = 8 << 20

// stacks are the stacks not in use. Each is mapped once and kept: they
// aren't Go memory, so a sync.Pool, which drops what it holds at a
// collection, would leak them.
var stacks struct {
	sync.Mutex
	free [][]byte
}

func getStack() []byte {
	stacks.Lock()
	if n := len(stacks.free); n > 0 {
		s := stacks.free[n-1]
		stacks.free = stacks.free[:n-1]
		stacks.Unlock()
		return s
	}
	stacks.Unlock()
	guard := syscall.Getpagesize()
	m, err := syscall.Mmap(-1, 0, StackSize+guard, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		panic("jit: mapping a stack: " + err.Error())
	}
	if err := syscall.Mprotect(m[:guard], syscall.PROT_NONE); err != nil {
		panic("jit: protecting a stack's guard page: " + err.Error())
	}
	return m[guard:]
}

// Call runs fn(a, b, c), a compiled function of three 64-bit integer
// arguments returning one, on a stack of StackSize bytes, and returns its
// result. Nothing a stack holds survives the call.
func Call(fn, a, b, c uintptr) uint64 {
	s := getStack()
	r := call(fn, a, b, c, addrOf(s)+uintptr(len(s)))
	stacks.Lock()
	stacks.free = append(stacks.free, s)
	stacks.Unlock()
	return r
}

// call is Call on the stack whose top is sp. See call_linux_*.s.
//
//go:noescape
func call(fn, a, b, c, sp uintptr) uint64
